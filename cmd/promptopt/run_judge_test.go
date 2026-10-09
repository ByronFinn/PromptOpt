package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
)

// --- judge isolation CLI surface (V7 roadmap §2.1) --------------------------

// judgeTaskYAML writes a one-metric llm_judge task and its dataset.
func judgeTaskYAML(t *testing.T) (task, cand, ds string) {
	t.Helper()
	task = writeYAML(t, "judge_task.yaml", `name: judged_tcm
prompt_template: |
  extract entities from {input}
metrics: [llm_judge]
`)
	cand = writeYAML(t, "judge_candidate.yaml", `id: baseline
prompt: |
  extract from {input}
`)
	ds = writeYAML(t, "judge_dataset.yaml", `name: judged_ds
samples:
  - id: j-001
    input: 中医文本
    expected: {ok: true}
    split: test
`)
	return task, cand, ds
}

// startRecordingLLM records request bodies while answering with a fixed
// completion; the bodies slice is guarded because eval workers dial
// concurrently.
func startRecordingLLM(t *testing.T, bodies *[]string, mu *sync.Mutex, content, usage string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":%s}`, content, usage)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runCliStderr captures stderr (the human-readable channel) instead of
// stdout, for asserting the judge-fallback notice and its absence from
// headless stdout.
func runCliStderr(t *testing.T, args ...string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = devNull, w
	code := runCommand(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// TestRunJudgeIsolationManual drives the full CLI seam with two
// httptest servers: the dedicated judge provider receives the grading
// call (its own model name on the wire), the executor call stays on the
// main server, UsageByRole splits judge out, and the manifest snapshots
// the judge surface (no api key, ever).
func TestRunJudgeIsolationManual(t *testing.T) {
	var mu sync.Mutex
	var mainBodies, judgeBodies []string
	mainSrv := startRecordingLLM(t, &mainBodies, &mu, `{"ok": true}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)
	judgeSrv := startRecordingLLM(t, &judgeBodies, &mu, `{"score": 1, "diagnosis": "完全正确"}`,
		`{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`)
	task, cand, ds := judgeTaskYAML(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", mainSrv.URL, "--model", "fake-model", "--api-key", "1",
		"--out", outDir, "--headless",
		"--judge-provider", "openai", "--judge-base-url", judgeSrv.URL,
		"--judge-model", "judge-model", "--judge-api-key", "jk", "--judge-max-tokens", "32")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}

	// Routing: one call per server, the grading prompt only on the
	// judge backend with its own model name.
	if len(judgeBodies) != 1 || !strings.Contains(judgeBodies[0], `"model":"judge-model"`) {
		t.Errorf("judge calls = %v, want exactly one carrying model judge-model", judgeBodies)
	}
	if len(mainBodies) != 1 || strings.Contains(mainBodies[0], "评估裁判") {
		t.Errorf("main calls = %v, want exactly one executor call without the judge prompt", mainBodies)
	}

	// Headless stdout stays pure JSON; the summary splits the roles.
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not JSON: %v\n%s", err, out)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 7 || u.CompletionTokens != 3 {
		t.Errorf("judge usage = %+v, want 7/3 under RoleJudge", u)
	}

	// Manifest snapshot: exactly the configured judge surface, no key.
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(outDir, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mf runManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.JudgeProvider != "openai" || mf.JudgeModel != "judge-model" ||
		mf.JudgeBaseURL != judgeSrv.URL || mf.JudgeMaxTokens != 32 {
		t.Errorf("manifest judge snapshot = %s/%s/%s/%d", mf.JudgeProvider, mf.JudgeBaseURL, mf.JudgeModel, mf.JudgeMaxTokens)
	}
	if strings.Contains(string(b), "jk") {
		t.Error("manifest.json leaks the judge api key")
	}

	// The replayable events carry the judge role too (run_done usage).
	events, err := os.ReadFile(filepath.Join(outDir, entries[0].Name(), "events.jsonl"))
	if err != nil || !strings.Contains(string(events), `"judge"`) {
		t.Errorf("events.jsonl missing the judge role key (%v)", err)
	}
}

// TestRunJudgeUnsetKeepsLegacyShape: without any --judge-* configuration
// the manifest omits the judge_* keys entirely (legacy manifests read
// unchanged) and the stderr fallback notice fires for llm_judge tasks —
// while a configured judge surface silences it again. headless stdout
// staying pure JSON is asserted by TestRunJudgeIsolationManual's
// summary decode.
func TestRunJudgeUnsetKeepsLegacyShape(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := judgeTaskYAML(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, stderr := runCliStderr(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "fake-model", "--api-key", "1",
		"--out", outDir, "--headless")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "llm_judge 但未配置裁判") {
		t.Errorf("stderr = %q, want the judge-fallback notice", stderr)
	}

	// The notice is a fallback signal only: one configured judge knob
	// silences it.
	code, stderr = runCliStderr(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "fake-model", "--api-key", "1",
		"--judge-model", "judge-model",
		"--out", filepath.Join(t.TempDir(), "runs2"), "--headless")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(stderr, "llm_judge 但未配置裁判") {
		t.Errorf("stderr = %q, want no fallback notice with --judge-model set", stderr)
	}

	entries, _ := os.ReadDir(outDir)
	b, err := os.ReadFile(filepath.Join(outDir, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"judge_provider", "judge_base_url", "judge_model", "judge_max_tokens"} {
		if _, ok := raw[key]; ok {
			t.Errorf("manifest.json carries %q without any judge configuration", key)
		}
	}
}

// TestParseRunFlagsJudgeSurface pins the whitelist and env resolution
// of the judge flag surface.
func TestParseRunFlagsJudgeSurface(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}

	if _, err := parseRunFlags(append(slices.Clone(base), "--judge-provider", "bogus")); err == nil ||
		!strings.Contains(err.Error(), "openai or anthropic") {
		t.Errorf("bogus judge provider err = %v, want the whitelist refusal", err)
	}
	if _, err := parseRunFlags(append(slices.Clone(base), "--judge-max-tokens", "-1")); err == nil {
		t.Error("--judge-max-tokens -1 accepted")
	}

	o, err := parseRunFlags(base)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if o.judgeProvider != "" || o.judgeModel != "" || o.judgeMaxTokens != 0 {
		t.Errorf("judge defaults = %s/%s/%d, want all unset", o.judgeProvider, o.judgeModel, o.judgeMaxTokens)
	}

	t.Setenv(config.EnvJudgeModel, "env-judge-model")
	t.Setenv(config.EnvJudgeMaxTokens, "77")
	o, err = parseRunFlags(append(slices.Clone(base), "--judge-model", "flag-judge-model"))
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.judgeModel != "flag-judge-model" || o.judgeMaxTokens != 77 {
		t.Errorf("judge precedence = %s/%d, want flag model over env model and env max tokens", o.judgeModel, o.judgeMaxTokens)
	}
}

// TestResolveVerifyConnJudgeFallbacks pins the verify-side judge chain:
// flag > PROMPTOPT_JUDGE_* env > manifest, an unknown provider name is
// a usage error, and an unset surface stays empty (the engine falls
// back to the executor side).
func TestResolveVerifyConnJudgeFallbacks(t *testing.T) {
	t.Setenv(config.EnvJudgeProvider, "")
	t.Setenv(config.EnvJudgeModel, "")
	t.Setenv(config.EnvJudgeBaseURL, "")
	t.Setenv(config.EnvJudgeAPIKey, "")
	t.Setenv(config.EnvJudgeMaxTokens, "")
	mf := runManifest{
		BaseURL: "http://manifest", Model: "m-model",
		JudgeProvider: "anthropic", JudgeBaseURL: "http://judge-manifest",
		JudgeModel: "jm-model", JudgeMaxTokens: 32,
	}
	o, err := resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("manifest tier: %v", err)
	}
	if o.judgeProvider != "anthropic" || o.judgeModel != "jm-model" ||
		o.judgeBaseURL != "http://judge-manifest" || o.judgeMaxTokens != 32 {
		t.Errorf("manifest judge tier = %+v", o)
	}

	t.Setenv(config.EnvJudgeModel, "env-judge-model")
	o, err = resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.judgeModel != "env-judge-model" {
		t.Errorf("judge model = %q, want the env value beating the manifest", o.judgeModel)
	}

	o, err = resolveVerifyConn(verifyOptions{judgeModel: "flag-judge-model"}, mf)
	if err != nil {
		t.Fatalf("flag tier: %v", err)
	}
	if o.judgeModel != "flag-judge-model" {
		t.Errorf("judge model = %q, want the flag value beating env and manifest", o.judgeModel)
	}

	if _, err := resolveVerifyConn(verifyOptions{judgeProvider: "bogus"}, mf); err == nil ||
		!strings.Contains(err.Error(), "openai or anthropic") {
		t.Errorf("bogus judge provider err = %v, want the whitelist refusal", err)
	}

	// An unset surface stays empty: verify then reproduces the legacy
	// behavior (judge on the executor side, resolved inside eval.Engine).
	t.Setenv(config.EnvJudgeModel, "")
	o, err = resolveVerifyConn(verifyOptions{}, runManifest{BaseURL: "b", Model: "m"})
	if err != nil {
		t.Fatalf("unset tier: %v", err)
	}
	if o.judgeProvider != "" || o.judgeModel != "" || o.judgeBaseURL != "" || o.judgeMaxTokens != 0 {
		t.Errorf("unset judge tier = %+v, want all empty", o)
	}
}

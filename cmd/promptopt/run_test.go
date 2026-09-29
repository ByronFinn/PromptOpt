package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// writeYAML creates a temp YAML file from s and returns its path.
func writeYAML(t *testing.T, name, s string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// fixtureYAMLs writes a 3-sample task/candidate/dataset trio.
func fixtureYAMLs(t *testing.T) (task, cand, ds string) {
	t.Helper()
	task = writeYAML(t, "task.yaml", `name: tcm_test
prompt_template: |
  extract entities from {input}
metrics: [json_validator, exact_match]
`)
	cand = writeYAML(t, "candidate.yaml", `id: baseline
prompt: |
  extract from {input}
`)
	var sb strings.Builder
	sb.WriteString("name: ds\nsamples:\n")
	for i, split := range []string{"train", "dev", "test"} {
		fmt.Fprintf(&sb, "  - id: %s-001\n    input: 中医文本%d\n    expected: {ok: true}\n    split: %s\n", split, i, split)
	}
	ds = writeYAML(t, "dataset.yaml", sb.String())
	return task, cand, ds
}

// startFakeLLM answers with a fixed completion and usage.
func startFakeLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"ok\": true}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runCli invokes runCommand with stdout captured and stderr silenced.
func runCli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = w, devNull
	code := runCommand(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

func baseFlags(t *testing.T, srv *httptest.Server, task, cand, ds string) []string {
	t.Helper()
	return []string{
		"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "fake-model", "--api-key", "1",
	}
}

func TestRunHeadlessSuccess(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", outDir, "--headless")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not the run summary JSON: %v\n%s", err, out)
	}
	if res.Status != core.StatusCompleted || res.ExitCode != 0 ||
		res.Evaluated != 3 || res.Undispatched != 0 || len(res.FailedSamples) != 0 {
		t.Errorf("summary = %+v", res)
	}
	if res.MetricMeans["json_validator"] != 1 || res.MetricMeans["exact_match"] != 1 {
		t.Errorf("means = %+v", res.MetricMeans)
	}
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 30 || u.CompletionTokens != 15 {
		t.Errorf("usage = %+v", u)
	}

	// Artifacts on disk under runs/<run_id>/.
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	runDir := filepath.Join(outDir, entries[0].Name())
	for _, name := range []string{"manifest.json", "events.jsonl", "summary.json", "run.json"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
	calls, _ := os.ReadDir(filepath.Join(runDir, "calls"))
	samples, _ := os.ReadDir(filepath.Join(runDir, "samples"))
	if len(calls) != 3 || len(samples) != 3 {
		t.Errorf("calls = %d, samples = %d, want 3/3", len(calls), len(samples))
	}
	events, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil || !strings.Contains(string(events), EventRunDoneForTest) {
		t.Errorf("events.jsonl = %q (%v)", string(events), err)
	}
}

// EventRunDoneForTest decouples the test from eval internals via the
// wire string.
const EventRunDoneForTest = `"run_done"`

func TestRunBudgetEvalsOneOfThreeExitsTwo(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", outDir, "--headless", "--budget-evals", "1")...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("summary JSON: %v\n%s", err, out)
	}
	if res.Status != core.StatusBudgetExhausted || res.Undispatched != 2 || res.Evaluated != 1 {
		t.Errorf("summary = %+v", res)
	}

	// summary.json on disk records the same verdict.
	entries, _ := os.ReadDir(outDir)
	runDir := filepath.Join(outDir, entries[0].Name())
	b, err := os.ReadFile(filepath.Join(runDir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary.json: %v", err)
	}
	var onDisk core.RunResult
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatalf("decode summary.json: %v", err)
	}
	if onDisk.Undispatched != 2 || onDisk.ExitCode != 2 {
		t.Errorf("summary.json = %+v", onDisk)
	}
	calls, _ := os.ReadDir(filepath.Join(runDir, "calls"))
	if len(calls) != 1 {
		t.Errorf("calls = %d, want 1", len(calls))
	}
}

func TestRunEnvFallback(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	t.Setenv("PROMPTOPT_BASE_URL", srv.URL)
	t.Setenv("PROMPTOPT_MODEL", "env-model")
	t.Setenv("PROMPTOPT_API_KEY", "env-key")

	code, out := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--out", filepath.Join(t.TempDir(), "runs"), "--headless")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
}

func TestRunSplitFiltersSamples(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", filepath.Join(t.TempDir(), "runs"), "--headless", "--split", "dev")...)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("summary JSON: %v", err)
	}
	if res.TotalSamples != 1 || res.Split != "dev" {
		t.Errorf("summary = %+v", res)
	}
}

func TestRunUsageErrors(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := filepath.Join(t.TempDir(), "runs")

	cases := []struct {
		name string
		args []string
	}{
		{"missing --task", []string{"--candidate", cand, "--dataset", ds, "--base-url", srv.URL, "--model", "m", "--out", out}},
		{"web and headless conflict", append(baseFlags(t, srv, task, cand, ds), "--out", out, "--web", "--headless")},
		{"addr without web", append(baseFlags(t, srv, task, cand, ds), "--out", out, "--addr", "127.0.0.1:17701")},
		{"no base-url", []string{"--task", task, "--candidate", cand, "--dataset", ds, "--model", "m", "--out", out}},
		{"no samples match split", append(baseFlags(t, srv, task, cand, ds), "--out", out, "--headless", "--split", "nosuch")},
	}
	for _, tc := range cases {
		if code, _ := runCli(t, tc.args...); code != 1 {
			t.Errorf("%s: exit = %d, want 1", tc.name, code)
		}
	}
}

func TestRunProviderFailuresExitOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()
	task, cand, ds := fixtureYAMLs(t)

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", filepath.Join(t.TempDir(), "runs"), "--headless")...)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("summary JSON: %v", err)
	}
	if res.Status != core.StatusFailed || len(res.FailedSamples) != 3 {
		t.Errorf("summary = %+v", res)
	}
}

// --- zero-config mode (positional prompt) -----------------------------------

const zcSpec = `{"name":"tcm_zhenghou_zc","description":"从中医医案文本判断证候","prompt_template":"你是中医辨证助手。阅读下面的文本，判断证候名称，只输出证候名称本身。文本：{input}","metrics":["exact_match"],"primary_metric":"exact_match"}`

const zcSamples = `{"samples":[
 {"id":"a1","input":"恶寒发热，无汗，脉浮紧。","expected":"风寒束表","split":"train"},
 {"id":"a2","input":"心烦不寐，腰膝酸软，脉细数。","expected":"心肾不交","split":"dev"},
 {"id":"a3","input":"发热微恶风寒，咽痛，脉浮数。","expected":"风热犯表","split":"train"}
]}`

const zcProbes = `{"probes":["变体甲：输出证候名：{input}","变体乙：仅输出证候：{input}"]}`

// zcScript pins the probe evidence per sample: a1 is dead easy, a3 is
// dead hard, a2 discriminates and is the only kept sample.
var zcScript = []struct {
	input, expected string
	p1, p2          int
}{
	{"恶寒发热，无汗，脉浮紧。", "风寒束表", 1, 1},
	{"心烦不寐，腰膝酸软，脉细数。", "心肾不交", 1, 0},
	{"发热微恶风寒，咽痛，脉浮数。", "风热犯表", 0, 0},
}

// startZeroConfigLLM answers synthesis calls by their stage markers
// and evaluation calls from the scripted score table.
func startZeroConfigLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		var content string
		switch {
		case strings.Contains(body, harness.MarkerSpec):
			content = zcSpec
		case strings.Contains(body, harness.MarkerSamples):
			content = zcSamples
		case strings.Contains(body, harness.MarkerProbes):
			content = zcProbes
		case strings.Contains(body, harness.MarkerRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			content = "{}"
		default:
			content = "无法辨证"
			for _, sc := range zcScript {
				if !strings.Contains(body, sc.input) {
					continue
				}
				score := 1 // baseline always answers correctly
				switch {
				case strings.Contains(body, "变体甲"):
					score = sc.p1
				case strings.Contains(body, "变体乙"):
					score = sc.p2
				}
				if score == 1 {
					content = sc.expected
				}
				break
			}
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func loadJSONFile(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

// TestParseRunFlagsFlagsAfterPositional pins the zero-config argument
// order contract: the stdlib flag package stops at the first
// positional, but `promptopt run "<prompt>" --samples 3` is the
// natural invocation, so flags must parse before AND after the
// positional prompt.
func TestParseRunFlagsFlagsAfterPositional(t *testing.T) {
	o, err := parseRunFlags([]string{
		"从中医文本判断证候", "--samples", "3", "--probe-variants", "2",
		"--interactive", "--base-url", "http://127.0.0.1:9", "--model", "m",
	})
	if err != nil {
		t.Fatalf("flags after positional: %v", err)
	}
	if o.prompt != "从中医文本判断证候" || o.samples != 3 || o.probeVariants != 2 || !o.interactive {
		t.Errorf("options = %+v", o)
	}

	// Interleaved order works too, and flag-first keeps working.
	o, err = parseRunFlags([]string{
		"--samples", "4", "某提示词", "--interactive", "--model", "m",
		"--base-url", "http://127.0.0.1:9",
	})
	if err != nil {
		t.Fatalf("interleaved: %v", err)
	}
	if o.prompt != "某提示词" || o.samples != 4 || !o.interactive {
		t.Errorf("options = %+v", o)
	}

	// A non-boolean flag consumes the next argument even when it looks
	// like a flag; mutual exclusion still fires after reordering.
	o, err = parseRunFlags([]string{
		"--task", "task.yaml", "--candidate", "c.yaml", "--dataset", "d.yaml",
		"--split", "-x", "--model", "m", "--base-url", "http://127.0.0.1:9",
	})
	if err != nil {
		t.Fatalf("manual flags: %v", err)
	}
	if o.split != "-x" {
		t.Errorf("split = %q, want the consumed value -x", o.split)
	}
	if _, err := parseRunFlags([]string{"某提示词", "--task", "task.yaml", "--model", "m", "--base-url", "http://127.0.0.1:9"}); err == nil {
		t.Error("positional + --task must still conflict after reordering")
	}
	// The -h help path survives the reorder.
	if _, err := parseRunFlags([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h after reorder = %v, want flag.ErrHelp", err)
	}
}

func TestRunZeroConfigEndToEnd(t *testing.T) {
	srv := startZeroConfigLLM(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	// The real zero-config invocation shape: positional prompt first,
	// flags after it (the stdlib flag package would stop parsing here).
	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not the run summary JSON: %v\n%s", err, out)
	}
	if res.Status != core.StatusCompleted || res.TotalSamples != 1 || res.Evaluated != 1 {
		t.Errorf("summary = %+v, want 1 kept sample evaluated", res)
	}
	if res.MetricMeans["exact_match"] != 1 {
		t.Errorf("means = %+v", res.MetricMeans)
	}

	// runs/<id>/: manifest carries the zero-config fields, task fields
	// stay omitted.
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	runID := entries[0].Name()
	var mf map[string]any
	loadJSONFile(t, filepath.Join(outDir, runID, "manifest.json"), &mf)
	if mf["mode"] != string(harness.ModeAutopilot) || mf["synth_samples"] != float64(3) || mf["probe_variants"] != float64(2) {
		t.Errorf("manifest zero-config fields = %v", mf)
	}
	if _, ok := mf["task"]; ok {
		t.Errorf("manual-only manifest field task = %v, want omitted", mf["task"])
	}
	for _, name := range []string{"summary.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(outDir, runID, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}

	// synth/<id>/: full artifact tree with the anchors slot present.
	synthDir := filepath.Join(filepath.Join(outDir, "..", "synth"), runID)
	var report harness.FilterReport
	loadJSONFile(t, filepath.Join(synthDir, "filter.json"), &report)
	if report.Variants != 2 || report.Kept != 1 || len(report.PerSample) != 3 {
		t.Errorf("filter report = kept %d variants %d samples %d", report.Kept, report.Variants, len(report.PerSample))
	}
	var cp harness.CheckpointState
	loadJSONFile(t, filepath.Join(synthDir, "checkpoint.json"), &cp)
	if cp.Status != harness.CheckpointApproved || cp.Mode != harness.ModeAutopilot {
		t.Errorf("checkpoint = %+v, want approved/autopilot", cp)
	}
	raw, err := os.ReadFile(filepath.Join(synthDir, "samples.json"))
	if err != nil || !strings.Contains(string(raw), `"anchors": []`) {
		t.Errorf("samples.json anchors slot missing (%v)", err)
	}
	calls, _ := os.ReadDir(filepath.Join(synthDir, "calls"))
	if len(calls) != 3 {
		t.Errorf("synth call traces = %d, want 3 (spec/samples/probes)", len(calls))
	}
}

func TestRunZeroConfigBudgetEatenByProbesExitsTwo(t *testing.T) {
	srv := startZeroConfigLLM(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	// 3 samples × 2 probes = 6 probe evaluations; --budget-evals 3
	// covers probe-1 only (probes run in order): probe-2 collects no
	// evidence, so every sample has incomplete probe evidence, stays
	// unmeasured and kept (budget jitter never deletes data), and the
	// baseline is fully undispatched → exit 2.
	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
		"--budget-evals", "3")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("summary JSON: %v\n%s", err, out)
	}
	if res.Status != core.StatusBudgetExhausted || res.Undispatched != 3 || res.Evaluated != 0 {
		t.Errorf("summary = %+v, want budget_exhausted with all 3 samples undispatched", res)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var report harness.FilterReport
	loadJSONFile(t, filepath.Join(filepath.Join(outDir, "..", "synth"), entries[0].Name(), "filter.json"), &report)
	if report.Kept != 3 {
		t.Errorf("filter kept = %d, want 3 (unmeasured samples are never dropped)", report.Kept)
	}
	for _, pv := range report.PerSample {
		if pv.Verdict != harness.VerdictUnmeasured {
			t.Errorf("sample %s verdict = %s, want unmeasured", pv.ID, pv.Verdict)
		}
	}
}

func TestRunZeroConfigInteractiveResumesAfterApproval(t *testing.T) {
	srv := startZeroConfigLLM(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	// runCommand blocks on the interactive gate, so it runs in a
	// goroutine. No global stdio swap here (it would race with the
	// goroutine): progress lines go to the test's stderr, which is
	// fine — this test reads its verdicts from artifacts.
	done := make(chan int, 1)
	go func() {
		done <- runCommand([]string{
			"从中医医案文本判断证候",
			"--base-url", srv.URL, "--model", "jiuwei-tcm",
			"--out", outDir, "--samples", "3", "--probe-variants", "2",
			"--interactive",
		})
	}()

	// Wait for the pending checkpoint artifact, then approve by
	// editing it — the same convergence the web approve button and a
	// manual file edit share.
	synthBase := filepath.Join(outDir, "..", "synth")
	deadline := time.Now().Add(5 * time.Second)
	synthDir := ""
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(synthBase)
		if err == nil && len(entries) == 1 {
			candidate := filepath.Join(synthBase, entries[0].Name())
			if _, err := os.Stat(filepath.Join(candidate, "checkpoint.json")); err == nil {
				synthDir = candidate
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if synthDir == "" {
		t.Fatal("checkpoint.json never appeared")
	}
	if err := harness.SaveCheckpoint(synthDir, harness.CheckpointState{
		Status: harness.CheckpointApproved, Mode: harness.ModeInteractive, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0 after artifact approval", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run never resumed after the approval")
	}
	var cp harness.CheckpointState
	loadJSONFile(t, filepath.Join(synthDir, "checkpoint.json"), &cp)
	if cp.Status != harness.CheckpointApproved {
		t.Errorf("checkpoint = %+v, want approved", cp)
	}
}

func TestRunZeroConfigUsageErrors(t *testing.T) {
	srv := startZeroConfigLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := filepath.Join(t.TempDir(), "runs")

	cases := []struct {
		name string
		args []string
	}{
		{"positional with --task", []string{"--task", task, "--base-url", srv.URL, "--model", "m", "--out", out, "某提示词"}},
		{"positional with --candidate", []string{"--candidate", cand, "--base-url", srv.URL, "--model", "m", "--out", out, "某提示词"}},
		{"positional with --dataset", []string{"--dataset", ds, "--base-url", srv.URL, "--model", "m", "--out", out, "某提示词"}},
		{"positional with --split", []string{"--split", "dev", "--base-url", srv.URL, "--model", "m", "--out", out, "某提示词"}},
		{"interactive and headless", []string{"--base-url", srv.URL, "--model", "m", "--out", out, "--interactive", "--headless", "某提示词"}},
		{"two positionals", []string{"--base-url", srv.URL, "--model", "m", "--out", out, "提示词一", "提示词二"}},
		{"zero samples", []string{"--base-url", srv.URL, "--model", "m", "--out", out, "--samples", "0", "某提示词"}},
		{"zero probe variants", []string{"--base-url", srv.URL, "--model", "m", "--out", out, "--probe-variants", "0", "某提示词"}},
	}
	for _, tc := range cases {
		if code, _ := runCli(t, tc.args...); code != 1 {
			t.Errorf("%s: exit = %d, want 1", tc.name, code)
		}
	}
}

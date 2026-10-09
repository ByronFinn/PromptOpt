package main

import (
	"encoding/json"
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

// --- decision-model judge surface (P7 级联) ----------------------------------

func TestParseRunFlagsJudgeDecisionSurface(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}

	// Unknown backend refused.
	if _, err := parseRunFlags(append(slices.Clone(base), "--judge-backend", "bogus")); err == nil ||
		!strings.Contains(err.Error(), "--judge-backend") {
		t.Errorf("bogus backend err = %v, want the whitelist refusal", err)
	}
	// decision without its connection surface refused (both halves).
	if _, err := parseRunFlags(append(slices.Clone(base), "--judge-backend", "decision")); err == nil ||
		!strings.Contains(err.Error(), "--judge-decision-url") {
		t.Errorf("decision without url err = %v, want the completeness refusal", err)
	}
	if _, err := parseRunFlags(append(slices.Clone(base),
		"--judge-backend", "decision", "--judge-decision-url", "http://d")); err == nil ||
		!strings.Contains(err.Error(), "--judge-decision-model") {
		t.Errorf("decision without model err = %v, want the completeness refusal", err)
	}
	// Thresholds must stay within [0, 1].
	for flag, val := range map[string]string{
		"--judge-decision-confidence": "1.5",
		"--judge-decision-diag-below": "-0.1",
	} {
		if _, err := parseRunFlags(append(slices.Clone(base), flag, val)); err == nil {
			t.Errorf("%s %s accepted", flag, val)
		}
	}

	// Defaults: everything unset.
	o, err := parseRunFlags(base)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if o.judgeBackend != "" || o.judgeDecisionURL != "" || o.judgeDecisionModel != "" ||
		o.judgeDecisionConfidence != 0 || o.judgeDecisionDiagBelow != 0 {
		t.Errorf("decision defaults = %+v, want all unset", o)
	}

	// env backfill for the connection fields, flag wins over env; an
	// env-provided URL satisfies the completeness check.
	t.Setenv(config.EnvJudgeDecisionURL, "http://env-decision")
	t.Setenv(config.EnvJudgeDecisionModel, "env-tev1")
	o, err = parseRunFlags(append(slices.Clone(base), "--judge-backend", "decision",
		"--judge-decision-model", "flag-tev1"))
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.judgeDecisionURL != "http://env-decision" || o.judgeDecisionModel != "flag-tev1" {
		t.Errorf("decision surface = %s/%s, want env url under flag model", o.judgeDecisionURL, o.judgeDecisionModel)
	}
}

// decisionAnswerJSON is the trusted 4-level score-3.0 answer (→ 1.0).
const decisionAnswerJSON = `{"model":"tev1:0.8b","answers":{"q1":{"type":"score",
"probabilities":{"0":0,"1":0,"2":0,"3":1},"confidence":0.9,"score":3.0,
"legend":{"0":"完全错误","1":"部分正确","2":"基本正确","3":"与参考答案完全等价"},"selected":3}},
"usage":{"input_tokens":123,"output_tokens":1}}`

// startRecordingDecision records /v1/systemone bodies and answers with
// the trusted answer.
func startRecordingDecision(t *testing.T, bodies *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(decisionAnswerJSON))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunJudgeDecisionEndToEnd drives the full CLI seam with two stub
// servers: the decision service receives the single grading call (the
// {model, state, questions} body), the executor server never sees a
// judge prompt, the manifest snapshots the five decision fields, and
// the headless summary meters the decision call under RoleJudge.
func TestRunJudgeDecisionEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var mainBodies, decBodies []string
	mainSrv := startRecordingLLM(t, &mainBodies, &mu, `{"ok": true}`,
		`{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`)
	decSrv := startRecordingDecision(t, &decBodies, &mu)
	task, cand, ds := judgeTaskYAML(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", mainSrv.URL, "--model", "fake-model", "--api-key", "1",
		"--out", outDir, "--headless",
		"--judge-backend", "decision",
		"--judge-decision-url", decSrv.URL, "--judge-decision-model", "tev1:0.8b",
		"--judge-decision-confidence", "0.3", "--judge-decision-diag-below", "0.5")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}

	// Routing: exactly one decision call, no judge prompt on the main
	// server (single judge per run).
	mu.Lock()
	if len(decBodies) != 1 {
		t.Fatalf("decision calls = %d, want 1", len(decBodies))
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(decBodies[0]), &req); err != nil {
		t.Fatalf("decision body: %v\n%s", err, decBodies[0])
	}
	if req["model"] != "tev1:0.8b" || req["state"] == "" {
		t.Errorf("decision body model/state = %v / %v", req["model"], req["state"])
	}
	if _, ok := req["questions"].(map[string]any); !ok {
		t.Errorf("decision questions not name-indexed: %T", req["questions"])
	}
	for _, b := range mainBodies {
		if strings.Contains(b, "评估裁判") {
			t.Errorf("judge prompt leaked to the executor server: %.200s", b)
		}
	}
	mu.Unlock()

	// Headless stdout stays pure JSON with the decision call metered
	// under RoleJudge.
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not JSON: %v\n%s", err, out)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 123 || u.CompletionTokens != 1 {
		t.Errorf("judge usage = %+v, want 123/1 (decision call)", u)
	}

	// Manifest snapshot: the five decision fields.
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	runDir := filepath.Join(outDir, entries[0].Name())
	mb, err := os.ReadFile(filepath.Join(runDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mf runManifest
	if err := json.Unmarshal(mb, &mf); err != nil {
		t.Fatal(err)
	}
	if mf.JudgeBackend != "decision" || mf.JudgeDecisionURL != decSrv.URL ||
		mf.JudgeDecisionModel != "tev1:0.8b" ||
		mf.JudgeDecisionConfidence != 0.3 || mf.JudgeDecisionDiagBelow != 0.5 {
		t.Errorf("manifest decision snapshot = %v/%v/%v/%v/%v",
			mf.JudgeBackend, mf.JudgeDecisionURL, mf.JudgeDecisionModel,
			mf.JudgeDecisionConfidence, mf.JudgeDecisionDiagBelow)
	}

	// The audit trace carries the decision stage with the answer JSON
	// (traces write through MarshalIndent — decode, don't byte-match).
	callEntries, err := os.ReadDir(filepath.Join(runDir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	sawDecision := false
	for _, en := range callEntries {
		raw, err := os.ReadFile(filepath.Join(runDir, "calls", en.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var ct struct {
			Stage    string `json:"stage"`
			Response struct {
				Content string `json:"content"`
			} `json:"response"`
		}
		if err := json.Unmarshal(raw, &ct); err != nil {
			t.Fatal(err)
		}
		if ct.Stage == "judge-decision" && strings.Contains(ct.Response.Content, `"confidence":0.9`) {
			sawDecision = true
		}
	}
	if !sawDecision {
		t.Error("no judge-decision call trace with the answer JSON")
	}
}

func TestResolveVerifyConnJudgeDecisionChain(t *testing.T) {
	t.Setenv(config.EnvJudgeDecisionURL, "")
	t.Setenv(config.EnvJudgeDecisionModel, "")
	mf := runManifest{
		BaseURL: "http://manifest", Model: "m-model",
		JudgeBackend:            "decision",
		JudgeDecisionURL:        "http://decision-manifest",
		JudgeDecisionModel:      "tev1:0.8b",
		JudgeDecisionConfidence: 0.3,
		JudgeDecisionDiagBelow:  0.7,
	}

	// Manifest tier: the run's decision surface replays untouched.
	o, err := resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("manifest tier: %v", err)
	}
	if o.judgeBackend != "decision" || o.judgeDecisionURL != "http://decision-manifest" ||
		o.judgeDecisionModel != "tev1:0.8b" ||
		o.judgeDecisionConfidence != 0.3 || o.judgeDecisionDiagBelow != 0.7 {
		t.Errorf("manifest decision tier = %+v", o)
	}

	// env beats manifest for the connection fields.
	t.Setenv(config.EnvJudgeDecisionURL, "http://decision-env")
	o, err = resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.judgeDecisionURL != "http://decision-env" {
		t.Errorf("decision url = %q, want the env value", o.judgeDecisionURL)
	}

	// flag beats both; thresholds ride flag > manifest with 0 = default.
	o, err = resolveVerifyConn(verifyOptions{
		judgeBackend: "decision", judgeDecisionURL: "http://decision-flag",
		judgeDecisionConfidence: 0.6,
	}, mf)
	if err != nil {
		t.Fatalf("flag tier: %v", err)
	}
	if o.judgeDecisionURL != "http://decision-flag" || o.judgeDecisionModel != "tev1:0.8b" ||
		o.judgeDecisionConfidence != 0.6 || o.judgeDecisionDiagBelow != 0.7 {
		t.Errorf("flag decision tier = %+v", o)
	}

	// An unknown backend in the chain is refused.
	if _, err := resolveVerifyConn(verifyOptions{judgeBackend: "oracle"}, mf); err == nil ||
		!strings.Contains(err.Error(), "llm or decision") {
		t.Errorf("bogus backend err = %v, want the refusal", err)
	}

	// decision without any connection surface is refused.
	if _, err := resolveVerifyConn(verifyOptions{judgeBackend: "decision"},
		runManifest{BaseURL: "b", Model: "m"}); err == nil ||
		!strings.Contains(err.Error(), "judge-decision-url") {
		t.Errorf("decision without surface err = %v, want the completeness refusal", err)
	}
}

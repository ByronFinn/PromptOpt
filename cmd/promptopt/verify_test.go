package main

import (
	"encoding/json"
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
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// verifyBadMarker keys the prompt-keyed fake LLM's wrong answers.
const verifyBadMarker = "坏候选提示词标记"

// captureCli runs cmd with stdout captured and stderr silenced — the
// mirror of runCli for the artifact subcommands.
func captureCli(t *testing.T, cmd func([]string) int, args ...string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = w, devNull
	code := cmd(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

// captureCliStderr runs cmd with stderr captured and stdout silenced.
func captureCliStderr(t *testing.T, cmd func([]string) int, args ...string) (int, string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	devNull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = devNull, w
	code := cmd(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return code, string(out)
}

func verifyCli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCli(t, verifyCommand, args...)
}

func verifyCliStderr(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCliStderr(t, verifyCommand, args...)
}

// startPromptKeyedLLM answers holdout synthesis (harness.MarkerSamples)
// with the fixture sample set and keys evaluation answers off the
// rendered candidate prompt: a prompt containing badMarker gets a wrong
// answer, anything else answers every sample from the script table's
// expected column.
func startPromptKeyedLLM(t *testing.T, badMarker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		content := "无法辨证"
		switch {
		case strings.Contains(body, harness.MarkerSamples):
			content = zcSamples
		case strings.Contains(body, badMarker):
			content = "坏输出不是证候"
		default:
			for _, sc := range zcScript {
				if strings.Contains(body, sc.input) {
					content = sc.expected
					break
				}
			}
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// zeroConfigRunTree drives one scripted zero-config run and returns its
// runs dir and run id.
func zeroConfigRunTree(t *testing.T) (runsDir, runID string) {
	t.Helper()
	srv := startScriptedZeroConfigLLM(t, zcSamples, zcScript)
	outDir := filepath.Join(t.TempDir(), "runs")
	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2")
	if code != 0 {
		t.Fatalf("zero-config run exit = %d\n%s", code, out)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	return outDir, entries[0].Name()
}

// adoptForVerify hand-writes adopted.json — the same artifact the
// frontier dashboard's adopt button produces.
func adoptForVerify(t *testing.T, runsDir, runID, id, prompt string) {
	t.Helper()
	b, err := json.Marshal(adoptedRecord{
		CandidateID: id, Prompt: prompt,
		AdoptedAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "adopted.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// onlyVerifyDir returns the run's single verify/<ts> directory.
func onlyVerifyDir(t *testing.T, runDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(runDir, "verify", "*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("verify dirs = %v (%v)", matches, err)
	}
	return matches[0]
}

// --- verdict: pure table ---------------------------------------------------

// TestVerifyCollectorMetering pins the per-sample evidence口径: tokens
// come from the event Usage (executor + judge), latency adds JudgeMS on
// top of the executor LatencyMS, and failed/non-sample events are
// ignored.
func TestVerifyCollectorMetering(t *testing.T) {
	c := &verifyCollector{}
	c.observe(eval.Event{Type: eval.EventSampleStart, SampleID: "s1"})
	c.observe(eval.Event{Type: eval.EventSampleDone, SampleID: "s1", Error: "boom", LatencyMS: 50})
	c.observe(eval.Event{
		Type: eval.EventSampleDone, SampleID: "s1",
		Usage:     &core.Usage{PromptTokens: 10, CompletionTokens: 5},
		LatencyMS: 100, JudgeMS: 50,
	})
	if got := c.avgTokens(); got != 15 {
		t.Errorf("avgTokens = %v, want 15 (usage totals executor+judge)", got)
	}
	if got := c.avgLatencyMS(); got != 150 {
		t.Errorf("avgLatencyMS = %v, want 150 (executor 100 + judge 50)", got)
	}
}

func TestVerifyVerdictTable(t *testing.T) {
	mean := func(exact, jsonv float64) map[string]float64 {
		return map[string]float64{"exact_match": exact, "json_validator": jsonv}
	}
	cases := []struct {
		name                    string
		base, deliv             verifySide
		o                       verifyOptions
		metrics                 []string
		wantCode                int
		wantReg, wantJSON       bool
		wantTokens, wantLatency bool
	}{
		{
			name: "equal means pass", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 1)}, o: verifyOptions{maxRegression: 0.05},
			metrics: []string{"exact_match"}, wantCode: exitOK,
		},
		{
			// 0.875 and 0.125 are exactly representable, so delta ==
			// threshold holds without float drift.
			name: "delta at threshold passes", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(0.875, 1)}, o: verifyOptions{maxRegression: 0.125},
			metrics: []string{"exact_match"}, wantCode: exitOK,
		},
		{
			name: "delta over threshold regresses", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(0.9, 1)}, o: verifyOptions{maxRegression: 0.05},
			metrics: []string{"exact_match"}, wantCode: exitRegression, wantReg: true,
		},
		{
			name: "json rate under one violates", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 0.5)}, o: verifyOptions{maxRegression: 0.05},
			metrics: []string{"json_validator", "exact_match"}, wantCode: exitRegression, wantJSON: true,
		},
		{
			name: "json rate at one passes", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 1)}, o: verifyOptions{maxRegression: 0},
			metrics: []string{"json_validator", "exact_match"}, wantCode: exitOK,
		},
		{
			name: "avg tokens over cap violates", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 1), AvgTokens: 150}, o: verifyOptions{maxRegression: 0.05, maxAvgTokens: 100},
			metrics: []string{"exact_match"}, wantCode: exitRegression, wantTokens: true,
		},
		{
			name: "avg tokens at cap passes", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 1), AvgTokens: 100}, o: verifyOptions{maxRegression: 0.05, maxAvgTokens: 100},
			metrics: []string{"exact_match"}, wantCode: exitOK,
		},
		{
			name: "avg latency over cap violates", base: verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(1, 1), AvgLatencyMS: 250}, o: verifyOptions{maxRegression: 0.05, maxAvgLatencyMS: 200},
			metrics: []string{"exact_match"}, wantCode: exitRegression, wantLatency: true,
		},
		{
			name:  "undispatched beats failure and regression",
			base:  verifySide{Means: mean(1, 1), Undispatched: 1},
			deliv: verifySide{Means: mean(0, 0.5), Failed: []string{"s1"}},
			o:     verifyOptions{maxRegression: 0.05}, metrics: []string{"json_validator", "exact_match"},
			wantCode: exitBudgetExhausted, wantReg: true, wantJSON: true,
		},
		{
			name:  "failure beats regression",
			base:  verifySide{Means: mean(1, 1)},
			deliv: verifySide{Means: mean(0.5, 1), Failed: []string{"s2"}},
			o:     verifyOptions{maxRegression: 0.05}, metrics: []string{"exact_match"},
			wantCode: exitFailure, wantReg: true,
		},
	}
	for _, tc := range cases {
		reg, con, code := verdict("exact_match", tc.base, tc.deliv, tc.o, tc.metrics)
		if code != tc.wantCode {
			t.Errorf("%s: code = %d, want %d", tc.name, code, tc.wantCode)
		}
		if reg.Regressed != tc.wantReg {
			t.Errorf("%s: regressed = %v (delta %.4f, threshold %v)", tc.name, reg.Regressed, reg.Delta, reg.Threshold)
		}
		if con.JSONViolated != tc.wantJSON {
			t.Errorf("%s: json violated = %v", tc.name, con.JSONViolated)
		}
		if con.TokensViolated != tc.wantTokens {
			t.Errorf("%s: tokens violated = %v", tc.name, con.TokensViolated)
		}
		if con.LatencyViolated != tc.wantLatency {
			t.Errorf("%s: latency violated = %v", tc.name, con.LatencyViolated)
		}
	}
}

// --- candidates: hand-made artifacts, no LLM -------------------------------

const candidatesFrontier = `{"primary":"exact_match",
 "best":{"id":"g01","means":{"exact_match":1},"prompt":"优化提示词 {input}"},
 "members":[
   {"id":"baseline","round":0,"operator":"baseline","primary_mean":0.5,"prompt":"基线提示词 {input}"},
   {"id":"g01","round":1,"operator":"rewrite","primary_mean":1,"prompt":"优化提示词 {input}"}
 ]}`

// legacyFrontier predates member/best prompts.
const legacyFrontier = `{"primary":"exact_match",
 "best":{"id":"baseline","means":{},"prompt":""},
 "members":[{"id":"baseline","round":0,"operator":"baseline","primary_mean":0.5}]}`

const candidatesSpec = `{"task":{"name":"t","prompt_template":"规格模板 {input}","metrics":["exact_match"]},"probes":[]}`

func decodeFrontier(t *testing.T, s string) engine.FrontierFile {
	t.Helper()
	var f engine.FrontierFile
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		t.Fatalf("decode frontier: %v", err)
	}
	return f
}

func TestVerifyCandidates(t *testing.T) {
	root := t.TempDir()
	synthDir := filepath.Join(root, "synth", "r1")
	if err := os.MkdirAll(synthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(synthDir, "spec.json"), []byte(candidatesSpec), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("adopted wins over best", func(t *testing.T) {
		r := verifyRun{
			Frontier: decodeFrontier(t, candidatesFrontier),
			Adopted:  &adoptedRecord{CandidateID: "hand", Prompt: "手工提示词 {input}"},
		}
		base, deliv, err := r.candidates(synthDir)
		if err != nil {
			t.Fatal(err)
		}
		if deliv.ID != "hand" || deliv.Prompt != "手工提示词 {input}" {
			t.Errorf("delivered = %+v, want the adoption", deliv)
		}
		if base.ID != "baseline" || base.Prompt != "基线提示词 {input}" {
			t.Errorf("baseline = %+v, want the frontier member", base)
		}
	})
	t.Run("best fallback without adoption", func(t *testing.T) {
		r := verifyRun{Frontier: decodeFrontier(t, candidatesFrontier)}
		_, deliv, err := r.candidates(synthDir)
		if err != nil {
			t.Fatal(err)
		}
		if deliv.ID != "g01" || deliv.Prompt != "优化提示词 {input}" {
			t.Errorf("delivered = %+v, want the best pick", deliv)
		}
	})
	t.Run("legacy frontier falls back to spec template", func(t *testing.T) {
		// The legacy best carries no prompt, so the adoption supplies the
		// delivered side while the baseline resolves through the spec.
		r := verifyRun{
			Frontier: decodeFrontier(t, legacyFrontier),
			Adopted:  &adoptedRecord{CandidateID: "hand", Prompt: "手工提示词 {input}"},
		}
		base, _, err := r.candidates(synthDir)
		if err != nil {
			t.Fatal(err)
		}
		if base.ID != "baseline" || base.Prompt != "规格模板 {input}" {
			t.Errorf("baseline = %+v, want the spec prompt_template", base)
		}
	})
	t.Run("legacy frontier without spec errors", func(t *testing.T) {
		r := verifyRun{
			Frontier: decodeFrontier(t, legacyFrontier),
			Adopted:  &adoptedRecord{CandidateID: "hand", Prompt: "手工提示词 {input}"},
		}
		if _, _, err := r.candidates(filepath.Join(t.TempDir(), "no-synth")); err == nil {
			t.Error("expected an error when no baseline prompt resolvable")
		}
	})
	t.Run("empty delivered prompt errors", func(t *testing.T) {
		r := verifyRun{
			Frontier: decodeFrontier(t, candidatesFrontier),
			Adopted:  &adoptedRecord{CandidateID: "hand", Prompt: "  "},
		}
		if _, _, err := r.candidates(synthDir); err == nil {
			t.Error("expected an error for an empty delivered prompt")
		}
	})
}

// --- connection resolution: flag > env > manifest ---------------------------

func TestResolveVerifyConnFallbacks(t *testing.T) {
	t.Setenv("PROMPTOPT_BASE_URL", "")
	t.Setenv("PROMPTOPT_MODEL", "")
	t.Setenv("PROMPTOPT_API_KEY", "")
	mf := runManifest{Model: "m-model", BaseURL: "http://manifest"}

	o, err := resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("manifest tier: %v", err)
	}
	if o.baseURL != "http://manifest" || o.model != "m-model" || o.providerName != "openai" || o.apiKey != "1" {
		t.Errorf("manifest tier = %+v", o)
	}

	t.Setenv("PROMPTOPT_BASE_URL", "http://env")
	t.Setenv("PROMPTOPT_MODEL", "env-model")
	t.Setenv("PROMPTOPT_API_KEY", "env-key")
	o, err = resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.baseURL != "http://env" || o.model != "env-model" || o.apiKey != "env-key" {
		t.Errorf("env tier = %+v", o)
	}

	o, err = resolveVerifyConn(verifyOptions{baseURL: "http://flag", model: "flag-model", providerName: "openai"}, mf)
	if err != nil {
		t.Fatalf("flag tier: %v", err)
	}
	if o.baseURL != "http://flag" || o.model != "flag-model" {
		t.Errorf("flag tier = %+v", o)
	}

	// With every source empty the resolution is a usage error.
	t.Setenv("PROMPTOPT_BASE_URL", "")
	t.Setenv("PROMPTOPT_MODEL", "")
	if _, err := resolveVerifyConn(verifyOptions{}, runManifest{}); err == nil {
		t.Error("expected a usage error with no connection source at all")
	}

	o, err = resolveVerifyConn(verifyOptions{}, runManifest{BaseURL: "b", Model: "m", Provider: "openai"})
	if err != nil || o.providerName != "openai" {
		t.Errorf("provider manifest tier: %v %+v", err, o)
	}
}

// --- end-to-end over a scripted zero-config run -----------------------------

// TestVerifyZeroConfigSkippedExitsZero: with no adoption the tree's
// best equals the baseline (every child was a clone), so verify skips
// evaluation entirely — no connection parameters required.
func TestVerifyZeroConfigSkippedExitsZero(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("headless stdout is not the verify report JSON: %v\n%s", err, out)
	}
	if rep.Mode != verifyModeSkipped || rep.ExitCode != exitOK || rep.Note == "" {
		t.Errorf("report = %+v", rep)
	}
	verifyDir := onlyVerifyDir(t, filepath.Join(runsDir, runID))
	for _, name := range []string{"verify.json", "verify.md"} {
		if _, err := os.Stat(filepath.Join(verifyDir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
}

// TestVerifyHoldoutRegressionExitsThree: an adopted bad candidate (its
// prompt carries the marker) regresses every holdout sample — the same
// spec holdout set is synthesized through the fake LLM, the baseline
// answers everything, the delivered candidate nothing.
func TestVerifyHoldoutRegressionExitsThree(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-bad", verifyBadMarker+" 文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model", "--headless")
	if code != exitRegression {
		t.Fatalf("exit = %d, want 3\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("headless stdout is not the verify report JSON: %v\n%s", err, out)
	}
	if rep.Mode != verifyModeHoldout || !rep.Caveat || rep.ExitCode != exitRegression {
		t.Errorf("report mode/caveat/exit = %s/%v/%d", rep.Mode, rep.Caveat, rep.ExitCode)
	}
	if rep.Baseline.Means["exact_match"] != 1 || rep.Delivered.Means["exact_match"] != 0 {
		t.Errorf("means = base %+v deliv %+v", rep.Baseline.Means, rep.Delivered.Means)
	}
	if !rep.Regression.Regressed || rep.Regression.Delta != 1 {
		t.Errorf("regression = %+v", rep.Regression)
	}
	if rep.HoldoutUsage == nil || rep.HoldoutUsage.Total() != 15 {
		t.Errorf("holdout usage = %+v, want 15 tokens accounted separately", rep.HoldoutUsage)
	}
	if rep.Baseline.Evaluated != 3 || rep.Delivered.Evaluated != 3 {
		t.Errorf("evaluated = %d/%d, want 3/3", rep.Baseline.Evaluated, rep.Delivered.Evaluated)
	}

	runDir := filepath.Join(runsDir, runID)
	verifyDir := onlyVerifyDir(t, runDir)
	var onDisk verifyReport
	loadJSONFile(t, filepath.Join(verifyDir, "verify.json"), &onDisk)
	if onDisk.ExitCode != exitRegression || onDisk.Mode != verifyModeHoldout {
		t.Errorf("verify.json = %+v", onDisk)
	}
	md, err := os.ReadFile(filepath.Join(verifyDir, "verify.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"结论未经真实数据验证", "每侧", "先 baseline 后 delivered", "回归"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("verify.md lacks %q", want)
		}
	}
	for _, side := range []string{"baseline", "delivered"} {
		calls, _ := os.ReadDir(filepath.Join(verifyDir, side, "calls"))
		if len(calls) != 3 {
			t.Errorf("%s calls = %d, want 3", side, len(calls))
		}
	}
	synthCalls, _ := os.ReadDir(filepath.Join(verifyDir, "holdout", "calls"))
	if len(synthCalls) != 1 {
		t.Errorf("holdout synth calls = %d, want 1", len(synthCalls))
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "holdout", "samples.json")); err != nil {
		t.Errorf("missing holdout/samples.json: %v", err)
	}
}

// TestVerifyAnchorModeExitsZero: a 3-sample real anchor dataset drives
// both sides to full marks; anchor mode is uncaveated and persists the
// anchor copy.
func TestVerifyAnchorModeExitsZero(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-good", "优化后的辨证提示词。文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	var sb strings.Builder
	sb.WriteString("name: anchor\nsamples:\n")
	for i, sc := range zcScript {
		fmt.Fprintf(&sb, "  - id: an-%d\n    input: %s\n    expected: %s\n    split: test\n", i+1, sc.input, sc.expected)
	}
	anchor := writeYAML(t, "anchor.yaml", sb.String())

	code, out := verifyCli(t, runID, "--runs-dir", runsDir, "--anchor", anchor,
		"--base-url", srv.URL, "--model", "fake-model", "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify report JSON: %v\n%s", err, out)
	}
	if rep.Mode != verifyModeAnchor || rep.Caveat || rep.AnchorPath != anchor {
		t.Errorf("report mode/caveat/anchor = %s/%v/%s", rep.Mode, rep.Caveat, rep.AnchorPath)
	}
	if rep.Delivered.Means["exact_match"] != 1 || rep.Baseline.Means["exact_match"] != 1 {
		t.Errorf("means = %+v / %+v", rep.Baseline.Means, rep.Delivered.Means)
	}
	verifyDir := onlyVerifyDir(t, filepath.Join(runsDir, runID))
	if _, err := os.Stat(filepath.Join(verifyDir, "anchor-dataset.json")); err != nil {
		t.Errorf("missing anchor-dataset.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, "holdout")); !os.IsNotExist(err) {
		t.Errorf("anchor mode wrote a holdout dir (err %v)", err)
	}

	// An anchor under 3 samples is a usage error (ADR 0001).
	anchor2 := writeYAML(t, "anchor2.yaml", `name: anchor2
samples:
  - id: an-1
    input: 恶寒发热，无汗，脉浮紧。
    expected: 风寒束表
    split: test
  - id: an-2
    input: 心烦不寐，腰膝酸软，脉细数。
    expected: 心肾不交
    split: test
`)
	if code, _ := verifyCli(t, runID, "--runs-dir", runsDir, "--anchor", anchor2,
		"--base-url", srv.URL, "--model", "fake-model", "--headless"); code != exitFailure {
		t.Errorf("2-sample anchor exit = %d, want 1", code)
	}

	// Over 5 samples warns on stderr but still verifies.
	var sb6 strings.Builder
	sb6.WriteString("name: anchor6\nsamples:\n")
	for i := range 6 {
		sc := zcScript[i%len(zcScript)]
		fmt.Fprintf(&sb6, "  - id: big-%d\n    input: %s\n    expected: %s\n    split: test\n", i, sc.input, sc.expected)
	}
	anchor6 := writeYAML(t, "anchor6.yaml", sb6.String())
	code, stderr := verifyCliStderr(t, runID, "--runs-dir", runsDir, "--anchor", anchor6,
		"--base-url", srv.URL, "--model", "fake-model", "--headless")
	if code != exitOK {
		t.Fatalf("6-sample anchor exit = %d, want 0\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "3~5") {
		t.Errorf("stderr lacks the >5 warning: %q", stderr)
	}
}

// TestVerifyBudgetPerSideExitsTwo: --budget-evals caps each side
// independently — with 2 of 3 holdout samples dispatched per side, the
// evidence is incomplete and no regression verdict is possible.
func TestVerifyBudgetPerSideExitsTwo(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-good", "优化后的辨证提示词。文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model",
		"--budget-evals", "2", "--headless")
	if code != exitBudgetExhausted {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify report JSON: %v\n%s", err, out)
	}
	for _, side := range []struct {
		name string
		s    verifySide
	}{{"baseline", rep.Baseline}, {"delivered", rep.Delivered}} {
		if side.s.Evaluated != 2 || side.s.Undispatched != 1 {
			t.Errorf("%s side = %d evaluated / %d undispatched, want 2/1 (per-side budget)",
				side.name, side.s.Evaluated, side.s.Undispatched)
		}
	}
	if rep.ExitCode != exitBudgetExhausted {
		t.Errorf("report exit = %d, want 2", rep.ExitCode)
	}
	md, err := os.ReadFile(filepath.Join(onlyVerifyDir(t, filepath.Join(runsDir, runID)), "verify.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "证据不完整") {
		t.Error("verify.md lacks the incomplete-evidence note")
	}
}

// TestVerifyAvgTokensConstraint: equal metric means but a per-sample
// token cap below the fake usage (15) trips the cost constraint alone.
func TestVerifyAvgTokensConstraint(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-good", "优化后的辨证提示词。文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model",
		"--max-avg-tokens", "14", "--headless")
	if code != exitRegression {
		t.Fatalf("exit = %d, want 3\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify report JSON: %v\n%s", err, out)
	}
	if rep.Regression.Regressed {
		t.Errorf("regression fired: %+v", rep.Regression)
	}
	if !rep.Constraints.TokensViolated || rep.Constraints.AvgTokens != 15 {
		t.Errorf("constraints = %+v, want 15 tokens over the 14 cap", rep.Constraints)
	}
}

// TestVerifyUsageErrors drives the flag surface through the CLI.
func TestVerifyUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no run id", []string{"--runs-dir", t.TempDir()}},
		{"two positionals", []string{"a", "b", "--runs-dir", t.TempDir()}},
		{"unknown run", []string{"nope", "--runs-dir", t.TempDir()}},
		{"negative regression", []string{"r1", "--runs-dir", t.TempDir(), "--max-regression", "-0.1"}},
		{"negative token cap", []string{"r1", "--runs-dir", t.TempDir(), "--max-avg-tokens", "-1"}},
		{"zero workers", []string{"r1", "--runs-dir", t.TempDir(), "--workers", "0"}},
	}
	for _, tc := range cases {
		if code, _ := verifyCli(t, tc.args...); code != exitFailure {
			t.Errorf("%s: exit = %d, want 1", tc.name, code)
		}
	}
}

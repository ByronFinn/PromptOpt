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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/builtin"
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
var zcScript = []zcEval{
	{"恶寒发热，无汗，脉浮紧。", "风寒束表", 1, 1},
	{"心烦不寐，腰膝酸软，脉细数。", "心肾不交", 1, 0},
	{"发热微恶风寒，咽痛，脉浮数。", "风热犯表", 0, 0},
}

// Engine marker scripts for the zero-config mock: reflection returns a
// fixed hypothesis pool, every mutation returns a valid prompt (the
// kept set answers every candidate identically → clone rejections and
// a VISTA restart on the stagnation ladder).
const (
	zcHypotheses = `{"hypotheses":[{"id":"h1","text":"明确要求只输出证候名称本身","confidence":0.8},{"id":"h2","text":"补充常见证候的辨别要点","confidence":0.6}]}`
	zcMutation   = `{"id":"g","name":"优化版","prompt":"优化后的辨证提示词。{input}"}`
)

// zcEval is one scripted probe row: which probe variants answer the
// sample correctly.
type zcEval struct {
	input, expected string
	p1, p2          int
}

// startZeroConfigLLM answers synthesis and optimization calls by their
// stage markers and evaluation calls from the scripted score table.
func startZeroConfigLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return startScriptedZeroConfigLLM(t, zcSamples, zcScript)
}

// startScriptedZeroConfigLLM is the parameterized zero-config mock:
// synthesis answers with samplesJSON, evaluation calls score from the
// script table (non-probe candidates answer every kept sample
// correctly).
func startScriptedZeroConfigLLM(t *testing.T, samplesJSON string, script []zcEval) *httptest.Server {
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
			content = samplesJSON
		case strings.Contains(body, harness.MarkerProbes):
			content = zcProbes
		case strings.Contains(body, harness.MarkerRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			content = "{}"
		case strings.Contains(body, engine.MarkerReflect):
			content = zcHypotheses
		case strings.Contains(body, engine.MarkerRewrite),
			strings.Contains(body, engine.MarkerMerge),
			strings.Contains(body, engine.MarkerFresh):
			content = zcMutation
		case strings.Contains(body, engine.MarkerHypRepair),
			strings.Contains(body, engine.MarkerCandFix):
			t.Errorf("unexpected engine repair call: %.200s", body)
			content = "{}"
		default:
			content = "无法辨证"
			for _, sc := range script {
				if !strings.Contains(body, sc.input) {
					continue
				}
				score := 1 // every candidate answers the kept samples correctly
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
	// Terminal three-way consistency: stdout status/exit_code match
	// the process exit code (rounds_done keeps completed/0).
	if res.Status != core.StatusCompleted || res.ExitCode != code {
		t.Errorf("summary status/exit = %s/%d, process exit = %d", res.Status, res.ExitCode, code)
	}
	if res.TotalSamples != 1 || res.Evaluated != 1 {
		t.Errorf("summary = %+v, want 1 kept sample evaluated", res)
	}
	if res.MetricMeans["exact_match"] != 1 {
		t.Errorf("means = %+v", res.MetricMeans)
	}

	// runs/<id>/: manifest carries the zero-config fields (including
	// the derived seed), task fields stay omitted.
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
	if seed, _ := mf["seed"].(float64); seed <= 0 {
		t.Errorf("manifest seed = %v, want a derived positive value", mf["seed"])
	}
	if _, ok := mf["task"]; ok {
		t.Errorf("manual-only manifest field task = %v, want omitted", mf["task"])
	}
	for _, name := range []string{"summary.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(outDir, runID, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}

	// Optimization artifacts: lineage, frontier, report, opt calls and
	// evaluation units all under runs/<id>/.
	runDir := filepath.Join(outDir, runID)
	var lineage []engine.LineageRecord
	loadJSONFile(t, filepath.Join(runDir, "lineage.json"), &lineage)
	if len(lineage) != 6 { // baseline + 5 default rounds
		t.Errorf("lineage records = %d, want 6 (baseline + 5 rounds)", len(lineage))
	}
	if lineage[0].ID != "baseline" || lineage[0].Operator != engine.OpBaseline {
		t.Errorf("lineage[0] = %+v, want the baseline seed row", lineage[0])
	}
	var frontier struct {
		Best struct {
			ID string `json:"id"`
		} `json:"best"`
	}
	loadJSONFile(t, filepath.Join(runDir, "frontier.json"), &frontier)
	if frontier.Best.ID != "baseline" {
		t.Errorf("frontier best = %s, want baseline (all children are clones)", frontier.Best.ID)
	}
	reportMD, err := os.ReadFile(filepath.Join(runDir, "report.md"))
	if err != nil || !strings.Contains(string(reportMD), "最优提示词") {
		t.Errorf("report.md missing Top-1 section (err %v)", err)
	}
	if calls, _ := os.ReadDir(filepath.Join(runDir, "opt-calls")); len(calls) == 0 {
		t.Error("opt-calls/ is empty, want reflection/mutation traces")
	}
	if units, _ := os.ReadDir(filepath.Join(runDir, "evals")); len(units) == 0 {
		t.Error("evals/ is empty, want optimization evaluation units")
	}

	// events.jsonl: the whole run carries exactly one terminal run_done
	// (cmd-owned); the engine emitted none and the baseline's inner
	// one was dropped.
	eventsB, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	runDones := 0
	optimizationRounds := 0
	vistaRestarts := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(eventsB)), "\n") {
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decode event line: %v", err)
		}
		switch ev.Type {
		case "run_done": // wire string; EventRunDoneForTest is for raw-text checks
			runDones++
		case engine.EventRoundStart:
			optimizationRounds++
		case engine.EventVistaRestart:
			vistaRestarts++
		}
	}
	if runDones != 1 {
		t.Errorf("run_done count = %d, want exactly 1 (cmd terminal)", runDones)
	}
	if optimizationRounds != 5 {
		t.Errorf("round_start count = %d, want 5 (default --max-rounds)", optimizationRounds)
	}
	if vistaRestarts != 1 {
		t.Errorf("vista_restart count = %d, want 1 (stagnation ladder fired)", vistaRestarts)
	}
	if !strings.Contains(string(eventsB), `"hypotheses_validated"`) {
		t.Error("events.jsonl lacks hypotheses_validated (ε-greedy audit trail)")
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
	// The skipped-loop path still emits exactly one terminal run_done.
	eventsB, err := os.ReadFile(filepath.Join(outDir, entries[0].Name(), "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if n := strings.Count(string(eventsB), EventRunDoneForTest); n != 1 {
		t.Errorf("run_done count = %d, want exactly 1 (terminal, baseline-exhausted path)", n)
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

// TestParseRunFlagsEngineFlags pins the GEPA flag validation surface.
func TestParseRunFlagsEngineFlags(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := func(extra ...string) []string {
		return append([]string{
			"--task", task, "--candidate", cand, "--dataset", ds,
			"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir(),
		}, extra...)
	}
	bad := []struct {
		name string
		args []string
	}{
		{"zero max rounds", base("--max-rounds", "0")},
		{"zero minibatch", base("--minibatch", "0")},
		{"zero stagnation limit", base("--stagnation-limit", "0")},
		{"negative epsilon", base("--epsilon", "-0.1")},
		{"epsilon above one", base("--epsilon", "1.5")},
		{"negative seed", base("--seed", "-1")},
		{"negative opt budget", base("--budget-opt-tokens", "-1")},
	}
	for _, tc := range bad {
		if _, err := parseRunFlags(tc.args); err == nil {
			t.Errorf("%s: parsed without error", tc.name)
		}
	}
	good := []struct {
		name string
		args []string
	}{
		{"epsilon bounds", base("--epsilon", "0", "--max-rounds", "1")},
		{"epsilon one", base("--epsilon", "1")},
		{"seed and opt budget", base("--seed", "42", "--budget-opt-tokens", "1000")},
	}
	for _, tc := range good {
		o, err := parseRunFlags(tc.args)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if o.maxRounds < 1 || o.minibatch < 1 || o.stagnationLimit < 1 || o.epsilon < 0 || o.epsilon > 1 || o.seed < 0 || o.budgetOptTokens < 0 {
			t.Errorf("%s: options = %+v", tc.name, o)
		}
	}
	// Defaults come from config.
	o, err := parseRunFlags(base())
	if err != nil {
		t.Fatal(err)
	}
	if o.maxRounds != config.DefaultMaxRounds || o.minibatch != config.DefaultMinibatch ||
		o.epsilon != config.DefaultEpsilon || o.stagnationLimit != config.DefaultStagnationLimit ||
		o.budgetOptTokens != 0 || o.seed != 0 {
		t.Errorf("engine flag defaults = %+v", o)
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

// --- optimizer/provider surface (V5 skeleton) --------------------------------

// zc3Samples keeps two train splits — the fixture the auto/miprov2 e2e
// cases route on (JointFewShot derivable from the retained set).
const zc3Samples = `{"samples":[
 {"id":"b1","input":"恶寒发热，无汗，脉浮紧。","expected":"风寒束表","split":"train"},
 {"id":"b2","input":"心烦不寐，腰膝酸软，脉细数。","expected":"心肾不交","split":"train"},
 {"id":"b3","input":"发热微恶风寒，咽痛，脉浮数。","expected":"风热犯表","split":"dev"}
]}`

// zc3Script keeps all three samples discriminative (probe 1 passes,
// probe 2 fails), so the retained set is the full 3 — unlike zcScript
// whose kept=1 anchors the GEPA clone-rejection assertions.
var zc3Script = []zcEval{
	{"恶寒发热，无汗，脉浮紧。", "风寒束表", 1, 0},
	{"心烦不寐，腰膝酸软，脉细数。", "心肾不交", 1, 0},
	{"发热微恶风寒，咽痛，脉浮数。", "风热犯表", 1, 0},
}

// runCliStderr invokes runCommand with stderr captured and stdout
// silenced — the mirror of runCli for error-message assertions.
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

// TestRouteFeatures pins the auto-routing derivation rules (V5 处置①):
// JointFewShot from train count + verifiable primary, TightBudget from
// the budget flag vs the retained set, MultiConstraint from the metric
// count, Pipeline without a V5 declaration source.
func TestRouteFeatures(t *testing.T) {
	task := core.Task{Name: "t", Metrics: []string{"exact_match"}}
	kept := []core.Sample{
		{ID: "a", Split: "train"}, {ID: "b", Split: "train"}, {ID: "c", Split: "dev"},
	}

	f := routeFeatures(task, kept, 0)
	if !f.JointFewShot || f.TightBudget || f.MultiConstraint || f.Pipeline {
		t.Errorf("features = %+v, want JointFewShot only", f)
	}
	// One train sample is not demo material.
	if f := routeFeatures(task, kept[1:], 0); f.JointFewShot {
		t.Errorf("features = %+v, want JointFewShot false with train=1", f)
	}
	// A custom metric has no per-sample automatic verdict.
	if f := routeFeatures(core.Task{Metrics: []string{"rouge"}}, kept, 0); f.JointFewShot {
		t.Errorf("features = %+v, want JointFewShot false for rouge", f)
	}
	// Setting a budget alone is not tight: below 2×kept is.
	if f := routeFeatures(task, kept, 6); f.TightBudget {
		t.Errorf("features = %+v, want TightBudget false at 2×kept", f)
	}
	if f := routeFeatures(task, kept, 5); !f.TightBudget {
		t.Errorf("features = %+v, want TightBudget true below 2×kept", f)
	}
	if f := routeFeatures(task, kept, 0); f.TightBudget {
		t.Errorf("features = %+v, want TightBudget false without a budget", f)
	}
	if f := routeFeatures(core.Task{Metrics: []string{"exact_match", "f1"}}, kept, 0); !f.MultiConstraint {
		t.Errorf("features = %+v, want MultiConstraint true", f)
	}
}

// TestParseRunFlagsOptimizerProvider pins the new flag validation:
// up-front --optimizer checks against the builtin registry (both
// modes), the manual-mode exclusion of paradigm flags, the evoprompt
// binding of --evo-variant and the provider name surface.
func TestParseRunFlagsOptimizerProvider(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	manual := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}
	zc := []string{"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir(), "某提示词"}

	bad := []struct {
		name string
		args []string
		want string
	}{
		{"bogus optimizer manual", append(slices.Clone(manual), "--optimizer", "bogus"), "available"},
		{"bogus optimizer zero-config", append(slices.Clone(zc), "--optimizer", "bogus"), "bogus"},
		{"explicit optimizer in manual mode", append(slices.Clone(manual), "--optimizer", "gepa"), "zero-config"},
		{"evo-variant in manual mode", append(slices.Clone(manual), "--evo-variant", "ga"), "zero-config"},
		{"evo-variant without evoprompt", append(slices.Clone(zc), "--optimizer", "gepa", "--evo-variant", "ga"), "evoprompt"},
		{"bad evo-variant value", append(slices.Clone(zc), "--evo-variant", "x"), "ga or de"},
		{"bad provider", append(slices.Clone(zc), "--provider", "bogus"), "openai or anthropic"},
	}
	for _, tc := range bad {
		_, err := parseRunFlags(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
	// The bogus list names the registered options (auto + registry);
	// derive the expectation from the same registry so it stays true
	// as paradigms join instead of pinning a stale name set.
	wantOpts := append(builtin.Registry().Names(), "auto")
	slices.Sort(wantOpts)
	_, err := parseRunFlags(append(slices.Clone(manual), "--optimizer", "nope"))
	if err == nil || !strings.Contains(err.Error(), strings.Join(wantOpts, ", ")) {
		t.Errorf("bogus optimizer error = %v, want the option list %s", err, strings.Join(wantOpts, ", "))
	}

	// Defaults come from config; auto is accepted in zero-config mode.
	o, err := parseRunFlags(zc)
	if err != nil {
		t.Fatalf("zero-config defaults: %v", err)
	}
	if o.optimizer != config.DefaultOptimizer || o.providerName != config.DefaultProvider || o.evoVariant != config.DefaultEvoVariant {
		t.Errorf("flag defaults = %s/%s/%s", o.optimizer, o.providerName, o.evoVariant)
	}
	if _, err := parseRunFlags(append(slices.Clone(zc), "--optimizer", "auto")); err != nil {
		t.Errorf("--optimizer auto rejected: %v", err)
	}
}

// TestRunOptimizerProviderUsageErrors drives the error paths through
// the CLI: every case exits 1 before any LLM dial.
func TestRunOptimizerProviderUsageErrors(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := filepath.Join(t.TempDir(), "runs")
	cases := []struct {
		name string
		args []string
	}{
		{"bogus optimizer", []string{"--task", task, "--candidate", cand, "--dataset", ds,
			"--base-url", srv.URL, "--model", "m", "--out", out, "--optimizer", "nope"}},
		{"optimizer in manual mode", []string{"--task", task, "--candidate", cand, "--dataset", ds,
			"--base-url", srv.URL, "--model", "m", "--out", out, "--optimizer", "gepa"}},
		{"evo-variant with gepa", []string{"某提示词", "--base-url", srv.URL, "--model", "m",
			"--out", out, "--optimizer", "gepa", "--evo-variant", "ga"}},
		{"bogus provider", []string{"某提示词", "--base-url", srv.URL, "--model", "m",
			"--out", out, "--provider", "nope"}},
	}
	for _, tc := range cases {
		if code, _ := runCli(t, tc.args...); code != 1 {
			t.Errorf("%s: exit = %d, want 1", tc.name, code)
		}
	}
}

// TestRunProviderAnthropicNotMerged pins the 处置⑦ error path: until
// the Anthropic implementation merges, --provider anthropic refuses to
// start with a clear message and writes no run artifacts.
func TestRunProviderAnthropicNotMerged(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "runs")
	code, stderr := runCliStderr(t, "某提示词",
		"--base-url", "http://127.0.0.1:9", "--model", "m",
		"--out", outDir, "--provider", "anthropic")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "anthropic") || !strings.Contains(stderr, "openai") {
		t.Errorf("stderr = %q, want the refusal naming anthropic and openai", stderr)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("run dirs = %v, want none on an early refusal", entries)
	}
}

// TestRunZeroConfigExplicitOptimizerGepa: the explicit --optimizer
// gepa run completes and the manifest records the paradigm, the
// request and the explicit-route reason.
func TestRunZeroConfigExplicitOptimizerGepa(t *testing.T) {
	srv := startZeroConfigLLM(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
		"--optimizer", "gepa")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var mf map[string]any
	loadJSONFile(t, filepath.Join(outDir, entries[0].Name(), "manifest.json"), &mf)
	if mf["optimizer"] != "gepa" || mf["optimizer_requested"] != "gepa" {
		t.Errorf("manifest optimizer fields = %v/%v, want gepa/gepa", mf["optimizer"], mf["optimizer_requested"])
	}
	if reason, _ := mf["optimizer_route_reason"].(string); !strings.Contains(reason, "显式指定") {
		t.Errorf("manifest route reason = %q, want the explicit-route note", reason)
	}
	if mf["provider"] != "openai" || mf["evo_variant"] != "ga" {
		t.Errorf("manifest provider/evo_variant = %v/%v, want openai/ga", mf["provider"], mf["evo_variant"])
	}
}

// TestRunZeroConfigAutoTightBudgetRoutesP1ToGepa pins the anchored
// auto-routing acceptance: a small --budget-evals flags TightBudget
// (below 2× the retained 3), auto requests p1 and degrades onto gepa.
// The probes starve the budget (exit 2, baseline undispatched), but
// routing resolves before the skip and the manifest records
// requested/paradigm/reason — with train=2 in the fixture this also
// pins that budget affordability outranks the JointFewShot rule.
func TestRunZeroConfigAutoTightBudgetRoutesP1ToGepa(t *testing.T) {
	srv := startScriptedZeroConfigLLM(t, zc3Samples, zc3Script)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
		"--optimizer", "auto", "--budget-evals", "3")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (baseline budget-starved)\n%s", code, out)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var mf map[string]any
	loadJSONFile(t, filepath.Join(outDir, entries[0].Name(), "manifest.json"), &mf)
	if mf["optimizer_requested"] != "p1" {
		t.Errorf("manifest optimizer_requested = %v, want p1 (tight budget)", mf["optimizer_requested"])
	}
	if mf["optimizer"] != "gepa" {
		t.Errorf("manifest optimizer = %v, want gepa (p1 degraded)", mf["optimizer"])
	}
	if reason, _ := mf["optimizer_route_reason"].(string); reason == "" {
		t.Error("manifest optimizer_route_reason is empty")
	} else if !strings.Contains(reason, "p1") || !strings.Contains(reason, "gepa") {
		t.Errorf("manifest route reason = %q, want it to name p1 and gepa", reason)
	}
}

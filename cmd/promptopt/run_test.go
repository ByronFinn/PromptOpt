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

	"github.com/ByronFinn/PromptOpt/internal/core"
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

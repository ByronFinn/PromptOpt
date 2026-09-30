package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

func rollbackCli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCli(t, rollbackCommand, args...)
}

// rollbackTree hand-writes a run tree for rollback: frontier, current
// adoption, optional history and optional synth spec. No LLM involved.
func rollbackTree(t *testing.T, frontier, adopted, history, spec string) (runsDir, runID string) {
	t.Helper()
	root := t.TempDir()
	runsDir = filepath.Join(root, "runs")
	runID = "r1"
	runDir := filepath.Join(runsDir, runID)
	for _, dir := range []string{runDir, filepath.Join(root, "synth", runID)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct {
		name, content string
	}{
		{"frontier.json", frontier},
		{"adopted.json", adopted},
		{"adopted-history.jsonl", history},
	} {
		if f.content != "" {
			if err := os.WriteFile(filepath.Join(runDir, f.name), []byte(f.content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if spec != "" {
		if err := os.WriteFile(filepath.Join(root, "synth", runID, "spec.json"), []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return runsDir, runID
}

// adoptLine renders one adopted-history.jsonl line.
func adoptLine(t *testing.T, id, prompt string, at time.Time) string {
	t.Helper()
	b, err := json.Marshal(adoptedRecord{CandidateID: id, Prompt: prompt, AdoptedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// rollbackFrontier carries baseline + g01 + g02 members with prompts.
const rollbackFrontier = `{"primary":"exact_match",
 "best":{"id":"g02","means":{},"prompt":"g02 提示词 {input}"},
 "members":[
   {"id":"baseline","round":0,"operator":"baseline","primary_mean":0.5,"prompt":"基线提示词 {input}"},
   {"id":"g01","round":1,"operator":"rewrite","primary_mean":0.8,"prompt":"g01 提示词 {input}"},
   {"id":"g02","round":2,"operator":"merge","primary_mean":0.9,"prompt":"g02 提示词 {input}"}
 ]}`

// rollbackLegacyFrontier predates member prompts (baseline has none).
const rollbackLegacyFrontier = `{"primary":"exact_match",
 "best":{"id":"g01","means":{},"prompt":"g01 提示词 {input}"},
 "members":[
   {"id":"baseline","round":0,"operator":"baseline","primary_mean":0.5},
   {"id":"g01","round":1,"operator":"rewrite","primary_mean":0.8,"prompt":"g01 提示词 {input}"}
 ]}`

// readAdoptedOrFatal decodes the tree's adopted.json.
func readAdoptedOrFatal(t *testing.T, runsDir, runID string) adoptedRecord {
	t.Helper()
	rec, ok := readAdopted(filepath.Join(runsDir, runID))
	if !ok {
		t.Fatalf("adopted.json unreadable in %s", filepath.Join(runsDir, runID))
	}
	return rec
}

func TestRollbackHistoryDefault(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	history := adoptLine(t, "g01", "g01 提示词 {input}", at) +
		adoptLine(t, "g02", "g02 提示词 {input}", at.Add(time.Minute))
	runsDir, runID := rollbackTree(t, rollbackFrontier,
		`{"candidate_id":"g02","prompt":"g02 提示词 {input}","adopted_at":"2026-09-30T00:01:00Z"}`,
		history, "")

	code, out := rollbackCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var res rollbackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not the rollback result JSON: %v\n%s", err, out)
	}
	if res.From != "g02" || res.To != "g01" || res.Prompt != "g01 提示词 {input}" {
		t.Errorf("result = %+v, want g02 → g01", res)
	}

	// adopted.json now carries the g01 verdict; the journal keeps the
	// rolled-back g02 line appended.
	if got := readAdoptedOrFatal(t, runsDir, runID); got.CandidateID != "g01" || got.Prompt != "g01 提示词 {input}" {
		t.Errorf("adopted.json = %+v, want g01", got)
	}
	b, err := os.ReadFile(filepath.Join(runsDir, runID, "adopted-history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], `"candidate_id":"g02"`) {
		t.Errorf("history lines = %q, want 3 with g02 appended last", lines)
	}
}

func TestRollbackBaselineFallback(t *testing.T) {
	runsDir, runID := rollbackTree(t, rollbackFrontier,
		`{"candidate_id":"g01","prompt":"g01 提示词 {input}","adopted_at":"2026-09-30T00:00:00Z"}`,
		"", "")

	code, out := rollbackCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var res rollbackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("rollback result JSON: %v\n%s", err, out)
	}
	if res.To != "baseline" || res.Prompt != "基线提示词 {input}" {
		t.Errorf("result = %+v, want the baseline member", res)
	}
	// The journal is created with the rolled-back adoption.
	if got := readAdoptedOrFatal(t, runsDir, runID); got.CandidateID != "baseline" {
		t.Errorf("adopted.json = %+v, want baseline", got)
	}
}

// TestRollbackLegacyBaselineFallsBackToSpec: a legacy frontier whose
// baseline member predates member prompts rolls back to the synth
// spec's prompt_template.
func TestRollbackLegacyBaselineFallsBackToSpec(t *testing.T) {
	runsDir, runID := rollbackTree(t, rollbackLegacyFrontier,
		`{"candidate_id":"g01","prompt":"g01 提示词 {input}","adopted_at":"2026-09-30T00:00:00Z"}`,
		"", `{"task":{"name":"t","prompt_template":"规格模板 {input}","metrics":["exact_match"]},"probes":[]}`)

	code, out := rollbackCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var res rollbackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("rollback result JSON: %v\n%s", err, out)
	}
	if res.To != "baseline" || res.Prompt != "规格模板 {input}" {
		t.Errorf("result = %+v, want the spec prompt_template under id baseline", res)
	}
}

func TestRollbackExplicitTo(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	history := adoptLine(t, "g01", "g01 提示词 {input}", at)
	runsDir, runID := rollbackTree(t, rollbackFrontier,
		`{"candidate_id":"baseline","prompt":"基线提示词 {input}","adopted_at":"2026-09-30T00:00:00Z"}`,
		history, "")

	// --to resolves frontier members (and would fall back to Best for
	// artifacts predating member prompts).
	code, out := rollbackCli(t, runID, "--runs-dir", runsDir, "--to", "g02", "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var res rollbackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("rollback result JSON: %v\n%s", err, out)
	}
	if res.From != "baseline" || res.To != "g02" || res.Prompt != "g02 提示词 {input}" {
		t.Errorf("result = %+v, want baseline → g02", res)
	}

	// The human rendering names the switch.
	code, stderr := captureCliStderr(t, rollbackCommand, runID, "--runs-dir", runsDir, "--to", "g01")
	if code != exitOK {
		t.Fatalf("second rollback exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "g02 → g01") {
		t.Errorf("stderr = %q, want the from → to line", stderr)
	}
	if got := readAdoptedOrFatal(t, runsDir, runID); got.CandidateID != "g01" {
		t.Errorf("adopted.json = %+v, want g01", got)
	}
}

func TestRollbackUnknownToExitsOne(t *testing.T) {
	runsDir, runID := rollbackTree(t, rollbackFrontier,
		`{"candidate_id":"g02","prompt":"g02 提示词 {input}","adopted_at":"2026-09-30T00:00:00Z"}`,
		"", "")
	before := readAdoptedOrFatal(t, runsDir, runID)

	if code, _ := rollbackCli(t, runID, "--runs-dir", runsDir, "--to", "nope"); code != exitFailure {
		t.Errorf("unknown --to exit = %d, want 1", code)
	}
	// Rolling back onto the current adoption is a no-op refusal.
	if code, _ := rollbackCli(t, runID, "--runs-dir", runsDir, "--to", "g02"); code != exitFailure {
		t.Errorf("same-target exit = %d, want 1", code)
	}
	if got := readAdoptedOrFatal(t, runsDir, runID); got.CandidateID != before.CandidateID {
		t.Errorf("adopted.json changed to %q on refusals", got.CandidateID)
	}

	// A legacy member without a prompt is refused with guidance.
	legacyRuns, legacyRun := rollbackTree(t, rollbackLegacyFrontier,
		`{"candidate_id":"g01","prompt":"g01 提示词 {input}","adopted_at":"2026-09-30T00:00:00Z"}`, "", "")
	if code, stderr := captureCliStderr(t, rollbackCommand, legacyRun, "--runs-dir", legacyRuns, "--to", "baseline"); code != exitFailure {
		t.Errorf("missing-prompt --to exit = %d, want 1", code)
	} else if !strings.Contains(stderr, "旧版产物") {
		t.Errorf("stderr = %q, want the legacy-artifact guidance", stderr)
	}
}

func TestRollbackMissingAdoptedExitsOne(t *testing.T) {
	runsDir, runID := rollbackTree(t, rollbackFrontier, "", "", "")
	if code, _ := rollbackCli(t, runID, "--runs-dir", runsDir); code != exitFailure {
		t.Errorf("exit = %d, want 1 without adopted.json", code)
	}
	if code, _ := rollbackCli(t, "nope", "--runs-dir", runsDir); code != exitFailure {
		t.Errorf("exit = %d, want 1 for an unknown run", code)
	}
}

func TestRollbackEmitExportsCandidateYAML(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	history := adoptLine(t, "g01", "g01 提示词 {input}", at)
	runsDir, runID := rollbackTree(t, rollbackFrontier,
		`{"candidate_id":"g02","prompt":"g02 提示词 {input}","adopted_at":"2026-09-30T00:01:00Z"}`,
		history, "")
	emit := filepath.Join(t.TempDir(), "sub", "candidate.yaml")

	code, out := rollbackCli(t, runID, "--runs-dir", runsDir, "--emit", emit, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var res rollbackResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("rollback result JSON: %v\n%s", err, out)
	}
	if res.Emitted != emit {
		t.Errorf("emitted = %q, want %q", res.Emitted, emit)
	}
	// The export round-trips through core.LoadCandidate.
	cand, err := core.LoadCandidate(emit)
	if err != nil {
		t.Fatalf("load emitted candidate: %v", err)
	}
	if cand.ID != "g01" || cand.Prompt != "g01 提示词 {input}" {
		t.Errorf("emitted candidate = %+v, want g01", cand)
	}
}

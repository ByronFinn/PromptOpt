package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// Zero-config fixtures for the tight-budget auto route: four samples,
// every probe-1 answer correct and every probe-2 answer wrong — each
// sample discriminates (variance 0.25, verdict keep), so the retained
// set is all four.
const p1ZCSamples = `{"samples":[
 {"id":"b1","input":"b1恶寒发热无汗","expected":"风寒束表","split":"train"},
 {"id":"b2","input":"b2心烦不寐","expected":"心肾不交","split":"dev"},
 {"id":"b3","input":"b3潮热盗汗","expected":"阴虚火旺","split":"train"},
 {"id":"b4","input":"b4脘腹胀满","expected":"气滞食积","split":"dev"}
]}`

var p1ZCScript = []zcEval{
	{"b1恶寒发热无汗", "风寒束表", 1, 0},
	{"b2心烦不寐", "心肾不交", 1, 0},
	{"b3潮热盗汗", "阴虚火旺", 1, 0},
	{"b4脘腹胀满", "气滞食积", 1, 0},
}

// TestRunZeroConfigAutoRoutesP1UnderTightBudget drives the full
// zero-config CLI with --optimizer auto and a budget too small for
// GEPA's minimal effective run (提案 §3.1 ②): 2 probes × 4 samples +
// 4 baseline evaluations = 12 of 14 slots before the optimizer; the
// corrected TightBudget rule (budgetEvals < 5×kept = 20) must route
// p1 — not the old dead rule's degradation — and the manifest must
// record the decision.
func TestRunZeroConfigAutoRoutesP1UnderTightBudget(t *testing.T) {
	srv := startScriptedZeroConfigLLM(t, p1ZCSamples, p1ZCScript)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "4", "--probe-variants", "2",
		"--budget-evals", "14", "--optimizer", "auto")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (budget exhausted)\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not the run summary JSON: %v\n%s", err, out)
	}
	if res.Status != core.StatusBudgetExhausted || res.ExitCode != code {
		t.Errorf("summary status/exit = %s/%d, want budget_exhausted/2", res.Status, res.ExitCode)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	runID := entries[0].Name()
	runDir := filepath.Join(outDir, runID)

	// The routing decision lands in the manifest: auto resolved to the
	// registered p1 paradigm with its reason recorded.
	var mf map[string]any
	loadJSONFile(t, filepath.Join(runDir, "manifest.json"), &mf)
	if mf["optimizer"] != "p1" || mf["optimizer_requested"] != "p1" {
		t.Errorf("manifest optimizer/requested = %v/%v, want p1/p1", mf["optimizer"], mf["optimizer_requested"])
	}
	if reason, _ := mf["optimizer_route_reason"].(string); !strings.Contains(reason, "p1") {
		t.Errorf("manifest route reason = %q, want it to name p1", reason)
	}

	// The filter kept all four samples — the tight budget is relative
	// to that set.
	var report harness.FilterReport
	loadJSONFile(t, filepath.Join(filepath.Join(outDir, "..", "synth"), runID, "filter.json"), &report)
	if report.Kept != 4 {
		t.Errorf("filter kept = %d, want 4", report.Kept)
	}

	// The p1 run's event trail: S* selection, then the truncated final
	// evaluation (2 slots remain after the pipeline, the search's
	// hypothesis probes eat them, the final's single sample stays
	// undispatched) — no silent fallback to a plain GEPA run.
	eventsB, err := os.ReadFile(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	events := string(eventsB)
	if !strings.Contains(events, `"select_done"`) {
		t.Error("events.jsonl missing the p1 select_done event")
	}
	if !strings.Contains(events, `"final_done"`) || !strings.Contains(events, `"undispatched":1`) {
		t.Error("events.jsonl missing the truncated p1 final_done event")
	}
	if strings.Contains(events, `"paradigm":"protegi"`) {
		t.Error("events.jsonl shows a protegi run — p1 must not silently degrade")
	}

	// The search's audit trail parks under p1-search/ beside the run
	// root's own lineage.
	if _, err := os.Stat(filepath.Join(runDir, "p1-search", "lineage.json")); err != nil {
		t.Errorf("missing p1-search lineage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runDir, "lineage.json")); err != nil {
		t.Errorf("missing run-root lineage: %v", err)
	}
}

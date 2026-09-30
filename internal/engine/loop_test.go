package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// TestNewLoopValidation surfaces request violations up front, before
// any artifact is written.
func TestNewLoopValidation(t *testing.T) {
	req := goldenRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Params = Params{MaxRounds: 0, Minibatch: 4, StagnationLimit: 2, Epsilon: 0, Seed: 7}
	if _, err := NewLoop(req); err == nil || !strings.Contains(err.Error(), "max rounds") {
		t.Fatalf("params violation = %v, want max rounds error", err)
	}

	req = goldenRequest(t.TempDir(), eval.NewBudget(0, 0), nil)
	req.Samples = nil
	if _, err := NewLoop(req); err == nil || !strings.Contains(err.Error(), "samples must not be empty") {
		t.Fatalf("request violation = %v, want samples error", err)
	}
}

// TestLoopFinishDeliversBaseline pins the scaffold's core contract:
// a loop that never ran a round still finishes with the baseline as
// Best, the seeded frontier and the full artifact set — every
// paradigm's "Best is never empty" guarantee comes from here.
func TestLoopFinishDeliversBaseline(t *testing.T) {
	dir := t.TempDir()
	req := goldenRequest(dir, eval.NewBudget(0, 0), nil)
	req.Provider = provider.NewOpenAI("http://127.0.0.1:1", "1", provider.OpenAIConfig{}) // never dialed

	loop, err := NewLoop(req)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}
	if loop.Primary() != "exact_match" || loop.Constraint() != "" {
		t.Errorf("primary/constraint = %q/%q", loop.Primary(), loop.Constraint())
	}
	if loop.Frontier().Size() != 1 || loop.Frontier().Members()[0].ID() != "baseline" {
		t.Fatalf("seeded frontier = %v, want the baseline row", loop.Frontier().Members())
	}
	res, err := loop.Finish(ReasonBudgetStopped, 0)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if res.Best.ID != "baseline" || res.Best.Prompt == "" {
		t.Fatalf("best = %+v, want the non-empty baseline delivery", res.Best)
	}
	if res.Reason != ReasonBudgetStopped || res.Rounds != 0 {
		t.Errorf("reason/rounds = %s/%d", res.Reason, res.Rounds)
	}
	for _, name := range []string{"lineage.json", "frontier.json", "report.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
}

// TestRequestOpt pins the Opts accessor contract paradigms rely on.
func TestRequestOpt(t *testing.T) {
	var req Request
	if got := req.Opt("evoprompt.variant"); got != "" {
		t.Errorf("Opt on nil map = %q, want empty", got)
	}
	req.Opts = map[string]string{"evoprompt.variant": "de"}
	if got := req.Opt("evoprompt.variant"); got != "de" {
		t.Errorf("Opt = %q, want de", got)
	}
	if got := req.Opt("evoprompt.missing"); got != "" {
		t.Errorf("Opt on missing key = %q, want empty", got)
	}
}

// Package engine implements the GEPA reflective-evolution optimizer
// core: minibatch reflection, hypothesis validation (VISTA decoupling),
// three mutation operators, a Pareto frontier over per-sample primary
// scores, candidate lineage and the run report.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Optimizer is the minimal optimization interface. V3 ships one
// implementation (Gepa); V5 splits further paradigms into
// internal/optimizers.
type Optimizer interface {
	Optimize(ctx context.Context, req Request) (Result, error)
}

// Stop reasons carried by Result.Reason.
const (
	// ReasonRoundsDone: all rounds completed.
	ReasonRoundsDone = "rounds_done"
	// ReasonBudgetStopped: the executor budget soft-stopped, the
	// optimizer-side token valve tripped or samples stayed
	// undispatched; the current best is still delivered.
	ReasonBudgetStopped = "budget_stopped"
	// ReasonAborted: the context was canceled.
	ReasonAborted = "aborted"
)

// Params tunes the GEPA loop.
type Params struct {
	MaxRounds       int     // optimization rounds, >= 1
	Minibatch       int     // samples drawn per round, >= 1
	StagnationLimit int     // stagnant rounds before a Fresh restart, >= 1
	Epsilon         float64 // exploration rate of hypothesis selection, [0, 1]
	Seed            int64   // PCG seed of the single rng stream, >= 0
}

// DefaultParams mirrors the config defaults (5 rounds, 4 samples, 0.2
// exploration, restart after 3 stagnant rounds).
func DefaultParams() Params {
	return Params{
		MaxRounds:       config.DefaultMaxRounds,
		Minibatch:       config.DefaultMinibatch,
		StagnationLimit: config.DefaultStagnationLimit,
		Epsilon:         config.DefaultEpsilon,
	}
}

// Validate reports every constraint violation at once.
func (p Params) Validate() error {
	var errs []error
	if p.MaxRounds < 1 {
		errs = append(errs, fmt.Errorf("max rounds must be >= 1, got %d", p.MaxRounds))
	}
	if p.Minibatch < 1 {
		errs = append(errs, fmt.Errorf("minibatch must be >= 1, got %d", p.Minibatch))
	}
	if p.StagnationLimit < 1 {
		errs = append(errs, fmt.Errorf("stagnation limit must be >= 1, got %d", p.StagnationLimit))
	}
	if p.Epsilon < 0 || p.Epsilon > 1 {
		errs = append(errs, fmt.Errorf("epsilon must be within [0, 1], got %v", p.Epsilon))
	}
	if p.Seed < 0 {
		errs = append(errs, fmt.Errorf("seed must be >= 0, got %d", p.Seed))
	}
	return errors.Join(errs...)
}

// SampleRecord is one sample's evidence from a candidate evaluation:
// response, per-metric scores and the ASI diagnosis that feeds the
// reflection context.
type SampleRecord struct {
	Sample    core.Sample        `json:"sample"`
	Response  string             `json:"response"`
	Scores    map[string]float64 `json:"scores,omitempty"`
	Diagnosis map[string]string  `json:"diagnosis,omitempty"`
}

// Request drives one optimization run. Samples is the retained
// evaluation set in a fixed order; every frontier score vector follows
// it. Baseline carries the initial candidate's per-sample records
// (missing score cells are recorded as 0 and flagged in the report).
type Request struct {
	Task     core.Task
	Params   Params
	Initial  core.Candidate
	Samples  []core.Sample
	Baseline []SampleRecord

	Provider provider.Provider
	Model    string
	// MaxTokens is the executor completion budget per evaluation call.
	MaxTokens int
	// OptMaxTokens is the optimizer-side completion floor; the advisor
	// never dials below config.DefaultOptMaxTokens.
	OptMaxTokens int
	Workers      int

	// Budget is the shared dispatch budget: executor-role usage arms
	// the soft stop, optimizer-role usage only feeds the valve below.
	Budget *eval.Budget
	// OptBudgetTokens bounds the optimizer role's cumulative tokens
	// (synthesis included); 0 = unlimited.
	OptBudgetTokens int64

	RunID   string
	RunDir  string // runs/<id>/; artifacts land beside the baseline run
	OnEvent func(eval.Event)
}

// validate reports Request-level precondition violations.
func (r Request) validate() error {
	var errs []error
	if len(r.Samples) == 0 {
		errs = append(errs, errors.New("samples must not be empty"))
	}
	if r.Provider == nil {
		errs = append(errs, errors.New("provider is required"))
	}
	if r.RunID == "" || r.RunDir == "" {
		errs = append(errs, errors.New("run id and run dir are required"))
	}
	if r.Budget == nil {
		errs = append(errs, errors.New("budget is required"))
	}
	return errors.Join(errs...)
}

// Result is the optimizer's terminal state. Best is never empty: the
// frontier is seeded with the baseline row, so even a budget-stopped
// run delivers the current best.
type Result struct {
	Best                core.Candidate
	BestMeans           map[string]float64
	ConstraintSatisfied bool
	Frontier            []Member
	Rounds              int
	Reason              string // rounds_done | budget_stopped | aborted
	Usage               map[core.Role]core.Usage
}

// RecordCollector harvests sample_done events into SampleRecords for
// the baseline context. The cmd layer subscribes it to the event
// fanout for the baseline evaluation window (probe events stay
// internal to the harness filter and never arrive here); the engine
// emits from worker goroutines, so state is guarded — the same
// pattern as harness.scoreCollector (filter.go).
type RecordCollector struct {
	mu      sync.Mutex
	harvest map[string]SampleRecord // sample id → response/scores/diagnosis
}

// NewRecordCollector returns an empty collector.
func NewRecordCollector() *RecordCollector {
	return &RecordCollector{harvest: make(map[string]SampleRecord)}
}

// Collect ingests one event; only sample_done is considered. Failed
// samples keep their error as a diagnosis so reflection still sees
// them.
func (c *RecordCollector) Collect(ev eval.Event) {
	if ev.Type != eval.EventSampleDone {
		return
	}
	rec := SampleRecord{Response: ev.Response, Scores: ev.Scores}
	if ev.Error != "" {
		rec.Diagnosis = map[string]string{"error": ev.Error}
	} else {
		rec.Diagnosis = ev.Diagnosis
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.harvest[ev.SampleID] = rec
}

// Records joins the harvest with samples in their fixed order and
// fills the Sample field. Samples without a harvested event are
// skipped (they surface as 0 cells in the baseline row).
func (c *RecordCollector) Records(samples []core.Sample) []SampleRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SampleRecord, 0, len(samples))
	for _, s := range samples {
		if rec, ok := c.harvest[s.ID]; ok {
			rec.Sample = s
			out = append(out, rec)
		}
	}
	return out
}

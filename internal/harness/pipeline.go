package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Harness lifecycle events share the eval event envelope and land in
// the same events.jsonl + SSE stream as the evaluation events.
const (
	EventSynthDone  = "synth_done"
	EventFilterDone = "filter_done"
	EventCheckpoint = "checkpoint"
)

// Pipeline runs the zero-config harness flow for one run id:
//
//	synth/<run_id>/ manifest → synthesize (spec, samples, probes)
//	→ save spec/samples → p¹ filter → save filter.json
//	→ checkpoint gate → re-read artifacts → baseline evaluation
//	  under runs/<run_id>/
//
// The returned RunResult is the baseline evaluation's summary; the
// caller persists summary.json. Any error aborts the run with exit
// code 1 at the caller.
type Pipeline struct {
	RunID          string
	RunsDir        string // baseline run dir root (runs/)
	SynthDir       string // synth/<run_id>
	Prompt         string // the user's natural-language prompt
	Provider       provider.Provider
	Model          string
	MaxTokens      int // evaluation completion budget
	SynthMaxTokens int
	SamplesN       int
	ProbeVariants  int
	Workers        int
	Budget         *eval.Budget // shared by probes and baseline
	Mode           GateMode     // defaults to autopilot
	OnEvent        func(eval.Event)
}

// Run executes the flow. Synthesis usage is recorded under the
// optimizer role (snapshot only — it never arms the executor soft
// stop, matching the V1 budget semantics); probes and the baseline
// share the pipeline budget, so probes starving it leave the baseline
// fully undispatched (exit 2).
func (p *Pipeline) Run(ctx context.Context) (core.RunResult, error) {
	if err := os.MkdirAll(p.SynthDir, 0o755); err != nil {
		return core.RunResult{}, fmt.Errorf("create synth dir: %w", err)
	}
	if err := SaveManifest(p.SynthDir, Manifest{
		Prompt:        p.Prompt,
		Model:         p.Model,
		SynthSamples:  p.SamplesN,
		ProbeVariants: p.ProbeVariants,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		return core.RunResult{}, fmt.Errorf("write synth manifest: %w", err)
	}

	synth := &Synthesizer{
		Provider: p.Provider, Model: p.Model, MaxTokens: p.SynthMaxTokens, Dir: p.SynthDir,
	}
	spec, err := synth.SynthesizeSpec(ctx, p.Prompt)
	if err != nil {
		return core.RunResult{}, err
	}
	samples, warnings, err := synth.SynthesizeSamples(ctx, spec, p.SamplesN)
	if err != nil {
		return core.RunResult{}, err
	}
	probes, err := synth.SynthesizeProbes(ctx, spec, p.ProbeVariants)
	if err != nil {
		return core.RunResult{}, err
	}
	p.emit(EventSynthDone, map[string]any{
		"samples": len(samples), "probes": len(probes), "warnings": warnings,
	})

	if err := SaveSpec(p.SynthDir, SpecFile{Task: spec, Probes: probes}); err != nil {
		return core.RunResult{}, fmt.Errorf("save spec.json: %w", err)
	}
	if err := SaveSamples(p.SynthDir, SampleFile{Samples: samples}); err != nil {
		return core.RunResult{}, fmt.Errorf("save samples.json: %w", err)
	}
	p.Budget.RecordUsage(core.RoleOptimizer, synth.Usage())

	filter := &Filter{
		RunID: p.RunID, SynthDir: p.SynthDir, Model: p.Model,
		MaxTokens: p.MaxTokens, Workers: p.Workers,
		Provider: p.Provider, Budget: p.Budget,
	}
	report, err := filter.Apply(ctx, SpecFile{Task: spec, Probes: probes}, samples, DefaultThresholds())
	if err != nil {
		return core.RunResult{}, err
	}
	p.emit(EventFilterDone, map[string]any{
		"kept": report.Kept, "dropped": len(samples) - report.Kept,
	})
	if err := SaveFilterReport(p.SynthDir, report); err != nil {
		return core.RunResult{}, fmt.Errorf("save filter.json: %w", err)
	}

	mode := p.Mode
	if mode == "" {
		mode = ModeAutopilot
	}
	gate := &Checkpoint{Dir: p.SynthDir, RunID: p.RunID, OnEvent: p.OnEvent}
	if err := gate.Gate(ctx, mode); err != nil {
		return core.RunResult{}, err
	}

	// Re-read the artifacts so edits made while the checkpoint was open
	// (web review or manual file edits) take effect for the baseline.
	// The reloaded spec is re-validated: a hand edit that injects an
	// unknown metric or drops the {input} placeholder must fail the run
	// loudly instead of driving a silent all-zero baseline.
	specAfter, err := LoadSpec(p.SynthDir)
	if err != nil {
		return core.RunResult{}, fmt.Errorf("reload spec.json: %w", err)
	}
	if err := specAfter.Task.Validate(); err != nil {
		return core.RunResult{}, fmt.Errorf("reload spec.json: %w", err)
	}
	samplesAfter, err := LoadSamples(p.SynthDir)
	if err != nil {
		return core.RunResult{}, fmt.Errorf("reload samples.json: %w", err)
	}
	reportAfter, err := LoadFilterReport(p.SynthDir)
	if err != nil {
		return core.RunResult{}, fmt.Errorf("reload filter.json: %w", err)
	}
	kept := SelectKept(samplesAfter.Samples, &reportAfter)
	if len(kept) == 0 {
		return core.RunResult{}, errors.New("过滤后保留样本集为空，无法进行 baseline 评估")
	}

	engine := &eval.Engine{
		RunID:       p.RunID,
		RunDir:      filepath.Join(p.RunsDir, p.RunID),
		Model:       p.Model,
		MaxTokens:   p.MaxTokens,
		Workers:     max(p.Workers, 1),
		Metrics:     specAfter.Task.Metrics,
		Budget:      p.Budget,
		Provider:    p.Provider,
		TaskName:    specAfter.Task.Name,
		CandidateID: "baseline",
		DatasetName: "synth",
		OnEvent:     p.OnEvent,
	}
	return engine.Run(ctx, core.Candidate{ID: "baseline", Prompt: specAfter.Task.PromptTemplate}, kept)
}

func (p *Pipeline) emit(typ string, detail map[string]any) {
	if p.OnEvent == nil {
		return
	}
	p.OnEvent(eval.Event{Type: typ, Time: time.Now(), RunID: p.RunID, Detail: detail})
}

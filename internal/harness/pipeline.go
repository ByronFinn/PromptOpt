package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Harness lifecycle events share the eval event envelope and land in
// the same events.jsonl + SSE stream as the evaluation events.
const (
	EventSynthDone      = "synth_done"
	EventFilterDone     = "filter_done"
	EventFilterFallback = "filter_fallback"
	EventCheckpoint     = "checkpoint"
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
//
// Statistical knobs: the probe filter only forwards Temperature (the
// K probe variants already provide the variance signal — reps would
// multiply probe cost for no extra discrimination), while the baseline
// evaluation runs with the same Temperature AND Reps as the later
// optimization units, so baseline and children rows share the
// k-rep-mean protocol and their SDs are symmetric (noise-aware
// admission compares like with like). Cost boundary of Reps > 1 on
// this side: (k−1) × kept extra evaluations, metered into the shared
// executor budget and eval slots; the default Reps 1 changes nothing.
type Pipeline struct {
	RunID          string
	RunsDir        string // baseline run dir root (runs/)
	SynthDir       string // synth/<run_id>
	Prompt         string // the user's natural-language prompt
	Provider       provider.Provider
	Model          string
	MaxTokens      int     // evaluation completion budget
	Temperature    float64 // executor sampling temperature (0 = gateway default)
	Reps           int     // sampling repetitions per baseline sample (<= 1 = single)
	SynthMaxTokens int
	// Judge coverage: forwarded to the filter's probe engines and the
	// baseline engine so a synthesized llm_judge spec grades through
	// the run's judge surface. Zero values fall back per field to
	// Provider/Model/MaxTokens inside eval.Engine. JudgeBackend/
	// DecisionClient forward the decision-model cascade — the whole
	// pipeline keeps one judge surface per run (the decision口径
	// follows --judge-backend, so synthesis-side probes and the
	// baseline never mix judges with the optimization loop).
	JudgeProvider           provider.Provider
	JudgeModel              string
	JudgeMaxTokens          int
	JudgeBackend            string
	DecisionClient          *provider.SystemOneClient
	JudgeDecisionConfidence float64
	JudgeDecisionDiagBelow  float64
	// ExtraBody carries gateway-private JSON fields forwarded to the
	// synthesizer calls, the probe engines and the baseline engine —
	// the whole pipeline hits the same gateway, where a thinking mode
	// burning the completion budget would stall synthesis first.
	ExtraBody map[string]any
	// SpecMetrics pins the synthesized spec's metrics (--spec-metrics /
	// 文件键 spec_metrics，PRD-0001 D7)：applied right after SynthesizeSpec
	// and BEFORE sample synthesis, so buildSamplesPrompt marshals the
	// overridden spec and the samples' expected answers are authored for
	// the pinned metrics. A pinned list that drops the spec's original
	// primary clears it (Task.Primary() falls back to Metrics[0], the
	// first pinned item). Empty = the LLM chooses (the default menu).
	SpecMetrics   []string
	SamplesN      int
	ProbeVariants int
	Workers       int
	Budget        *eval.Budget // shared by probes and baseline
	Mode          GateMode     // defaults to autopilot
	OnEvent       func(eval.Event)
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
		Provider: p.Provider, Model: p.Model, MaxTokens: p.SynthMaxTokens,
		ExtraBody: p.ExtraBody, Dir: p.SynthDir,
	}
	spec, err := synth.SynthesizeSpec(ctx, p.Prompt)
	if err != nil {
		return core.RunResult{}, err
	}
	// D7 覆盖点（PRD-0001 切分 3，钉死顺序）：SynthesizeSpec 之后、
	// SynthesizeSamples/SaveSpec 之前——buildSamplesPrompt 整体 marshal
	// spec，覆盖必须先于样本合成，expected 才按钉死指标出题；覆盖后立即
	// Validate() 快速失败，非法指标绝不烧样本/探针合成预算。原 primary
	// 不在钉死列表时置空交回落（Task.Primary() 取 Metrics[0]，即钉死
	// 列表首项——不新增 --spec-primary 的设计裁决）。
	if len(p.SpecMetrics) > 0 {
		spec.Metrics = slices.Clone(p.SpecMetrics)
		if !slices.Contains(spec.Metrics, spec.PrimaryMetric) {
			spec.PrimaryMetric = ""
		}
		if err := spec.Validate(); err != nil {
			return core.RunResult{}, fmt.Errorf("spec metrics override: %w", err)
		}
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
		MaxTokens: p.MaxTokens, Temperature: p.Temperature, Workers: p.Workers,
		Provider: p.Provider, Budget: p.Budget,
		JudgeProvider:           p.JudgeProvider,
		JudgeModel:              p.JudgeModel,
		JudgeMaxTokens:          p.JudgeMaxTokens,
		JudgeBackend:            p.JudgeBackend,
		DecisionClient:          p.DecisionClient,
		JudgeDecisionConfidence: p.JudgeDecisionConfidence,
		JudgeDecisionDiagBelow:  p.JudgeDecisionDiagBelow,
		ExtraBody:               p.ExtraBody,
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
		// The p¹ filter found no discriminative sample — every sample
		// classified dead or noisy. Hard-stopping would strand
		// autopilot runs on a data-quality issue the checkpoint could
		// have fixed, so fall back to the full synthesized set and
		// record the fallback loudly in filter.json and the event
		// stream: the scores will be near-constant and the
		// optimization report must be read with that in mind.
		kept = samplesAfter.Samples
		reportAfter.FallbackAll = true
		if err := SaveFilterReport(p.SynthDir, reportAfter); err != nil {
			return core.RunResult{}, fmt.Errorf("save fallback filter.json: %w", err)
		}
		p.emit(EventFilterFallback, map[string]any{"samples": len(kept)})
	}

	engine := &eval.Engine{
		RunID:                   p.RunID,
		RunDir:                  filepath.Join(p.RunsDir, p.RunID),
		Model:                   p.Model,
		MaxTokens:               p.MaxTokens,
		Temperature:             p.Temperature,
		Reps:                    p.Reps,
		Workers:                 max(p.Workers, 1),
		Metrics:                 specAfter.Task.Metrics,
		Budget:                  p.Budget,
		Provider:                p.Provider,
		JudgeProvider:           p.JudgeProvider,
		JudgeModel:              p.JudgeModel,
		JudgeMaxTokens:          p.JudgeMaxTokens,
		JudgeBackend:            p.JudgeBackend,
		DecisionClient:          p.DecisionClient,
		JudgeDecisionConfidence: p.JudgeDecisionConfidence,
		JudgeDecisionDiagBelow:  p.JudgeDecisionDiagBelow,
		ExtraBody:               p.ExtraBody,
		TaskName:                specAfter.Task.Name,
		CandidateID:             "baseline",
		DatasetName:             "synth",
		OnEvent:                 p.OnEvent,
	}
	return engine.Run(ctx, core.Candidate{ID: "baseline", Prompt: specAfter.Task.PromptTemplate}, kept)
}

func (p *Pipeline) emit(typ string, detail map[string]any) {
	if p.OnEvent == nil {
		return
	}
	p.OnEvent(eval.Event{Type: typ, Time: time.Now(), RunID: p.RunID, Detail: detail})
}

package harness

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Filter purifies a synthesized sample set with the p¹ variance
// method: every probe prompt variant from the spec is replayed over
// every sample, and each sample is classified from its K probe
// scores by Classify.
//
// Probe variants run sequentially (the worker pool inside one variant
// is bounded by Workers) so the shared budget is consumed in a
// deterministic priority order: probe-1 before probe-2 before the
// baseline evaluation.
type Filter struct {
	RunID     string
	SynthDir  string // probe traces land in SynthDir/probes/vN/
	Model     string
	MaxTokens int
	Workers   int
	Provider  provider.Provider
	Budget    *eval.Budget
}

// Apply replays every probe variant over samples and returns the
// filter report. Probe calls that fail after provider retries leave
// no score: a sample with incomplete probe evidence is classified
// unmeasured and kept, so provider jitter never deletes data. Its
// events stay internal — they never reach the user event stream.
func (f *Filter) Apply(ctx context.Context, spec SpecFile, samples []core.Sample, th Thresholds) (FilterReport, error) {
	if err := th.Validate(); err != nil {
		return FilterReport{}, err
	}
	k := len(spec.Probes)
	primary := spec.Task.Primary()
	scoresBySample := make(map[string][]float64, len(samples))

	for i, probe := range spec.Probes {
		coll := &scoreCollector{primary: primary, scores: map[string]float64{}}
		engine := &eval.Engine{
			RunID:       f.RunID,
			RunDir:      filepath.Join(f.SynthDir, "probes", fmt.Sprintf("v%d", i+1)),
			Model:       f.Model,
			MaxTokens:   f.MaxTokens,
			Workers:     max(f.Workers, 1),
			Metrics:     spec.Task.Metrics,
			Budget:      f.Budget,
			Provider:    f.Provider,
			TaskName:    spec.Task.Name,
			CandidateID: fmt.Sprintf("probe-%d", i+1),
			DatasetName: "synth",
			OnEvent:     coll.collect,
		}
		if _, err := engine.Run(ctx, core.Candidate{ID: fmt.Sprintf("probe-%d", i+1), Prompt: probe}, samples); err != nil {
			return FilterReport{}, fmt.Errorf("probe variant %d: %w", i+1, err)
		}
		for _, s := range samples {
			if score, ok := coll.scores[s.ID]; ok {
				scoresBySample[s.ID] = append(scoresBySample[s.ID], score)
			}
		}
	}

	report := FilterReport{Variants: k, Primary: primary, Thresholds: th}
	for _, s := range samples {
		scores := scoresBySample[s.ID]
		pv := SampleVerdict{ID: s.ID, Scores: scores, Variance: Variance(scores)}
		// Only complete probe evidence may decide a verdict; partial
		// evidence stays unmeasured and is kept conservatively.
		if len(scores) == k {
			pv.Verdict = Classify(scores, th)
		} else {
			pv.Verdict = VerdictUnmeasured
		}
		if pv.Verdict == VerdictKeep || pv.Verdict == VerdictUnmeasured {
			report.Kept++
		} else {
			if report.DroppedByVerdict == nil {
				report.DroppedByVerdict = make(map[Verdict]int)
			}
			report.DroppedByVerdict[pv.Verdict]++
		}
		report.PerSample = append(report.PerSample, pv)
	}
	return report, nil
}

// SelectKept computes the evaluation set for the baseline run from the
// current samples.json content and the filter report: verdicts keep
// and unmeasured survive, and samples added while the checkpoint was
// open (absent from the report) are kept too. Samples deleted during
// the pause are naturally absent from samples.
func SelectKept(samples []core.Sample, report *FilterReport) []core.Sample {
	verdicts := make(map[string]Verdict, len(report.PerSample))
	for _, pv := range report.PerSample {
		verdicts[pv.ID] = pv.Verdict
	}
	kept := make([]core.Sample, 0, len(samples))
	for _, s := range samples {
		v, measured := verdicts[s.ID]
		if !measured || v == VerdictKeep || v == VerdictUnmeasured {
			kept = append(kept, s)
		}
	}
	return kept
}

// scoreCollector harvests primary-metric scores from one probe run's
// sample_done events. The engine emits from worker goroutines, so the
// maps are guarded.
type scoreCollector struct {
	mu      sync.Mutex
	primary string
	scores  map[string]float64
}

func (c *scoreCollector) collect(ev eval.Event) {
	if ev.Type != eval.EventSampleDone {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Failed calls record nothing: the sample keeps whatever other
	// probes measured, and incomplete evidence reads as unmeasured.
	if score, ok := ev.Scores[c.primary]; ok {
		c.scores[ev.SampleID] = score
	}
}

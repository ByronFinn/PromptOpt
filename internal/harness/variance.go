// Package harness implements the Harness Builder: it synthesizes a
// task spec, an evaluation set and probe prompt variants from a
// natural-language prompt, purifies the set with the p¹ variance
// filter, and gates the pipeline on a human-review checkpoint.
package harness

import "fmt"

// Verdict is the p¹ classification of one sample across the K probe
// prompt variants.
type Verdict string

const (
	VerdictKeep       Verdict = "keep"       // probes disagree: discriminative
	VerdictDeadEasy   Verdict = "dead_easy"  // every probe solves it: no signal
	VerdictDeadHard   Verdict = "dead_hard"  // every probe fails it: no signal
	VerdictNoisy      Verdict = "noisy"      // never decisive under any probe
	VerdictUnmeasured Verdict = "unmeasured" // probe evidence incomplete
)

// Thresholds bounds the decisive band of a metric score. Scores <= Low
// count as "all wrong", >= High as "all right"; only scores strictly
// inside (Low, High) form the mid band.
type Thresholds struct {
	Low  float64
	High float64
}

// DefaultThresholds matches p¹ practice for [0,1] metrics: 0/1 metrics
// have no mid band, f1 keeps a narrow decisive band at both ends.
func DefaultThresholds() Thresholds { return Thresholds{Low: 0.01, High: 0.99} }

// Validate reports thresholds outside 0 <= Low < High <= 1.
func (t Thresholds) Validate() error {
	if t.Low < 0 || t.High > 1 || t.Low >= t.High {
		return fmt.Errorf("invalid thresholds low=%v high=%v: want 0 <= low < high <= 1", t.Low, t.High)
	}
	return nil
}

// Classify judges one sample from its probe scores.
//
//   - every score >= High: dead easy (all probes solve it)
//   - every score <= Low: dead hard (all probes fail it)
//   - every score strictly inside (Low, High): noisy — the sample is
//     never decisive under any probe, so prompt changes cannot move it
//   - otherwise: keep (some probes decide differently — discriminative)
//
// An empty score list is unmeasured. With K=1 no cross-prompt
// information exists, so the verdict reduces to dead/noisy — the keep
// branch needs at least two probes.
func Classify(scores []float64, th Thresholds) Verdict {
	if len(scores) == 0 {
		return VerdictUnmeasured
	}
	allLow, allHigh, allMid := true, true, true
	for _, s := range scores {
		switch {
		case s <= th.Low:
			allHigh, allMid = false, false
		case s >= th.High:
			allLow, allMid = false, false
		default:
			allLow, allHigh = false, false
		}
	}
	switch {
	case allHigh:
		return VerdictDeadEasy
	case allLow:
		return VerdictDeadHard
	case allMid:
		return VerdictNoisy
	default:
		return VerdictKeep
	}
}

// Variance returns the population variance of xs (0 for fewer than two
// values). It is the report's discrimination ordering value: higher
// variance across probes means the sample separates prompts better.
func Variance(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return ss / float64(len(xs))
}

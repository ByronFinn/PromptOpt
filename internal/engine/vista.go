package engine

import "math/rand/v2"

// Selection modes reported in the hypotheses_validated event (the
// ε-greedy audit trail).
const (
	ModeExplore = "explore"
	ModeExploit = "exploit"
)

// ValidatedHypothesis is one hypothesis plus its minibatch evidence:
// Mean is the temporary candidate's primary-metric mean over the
// batch; Lift is the difference against the parent on the same batch.
type ValidatedHypothesis struct {
	Hypothesis Hypothesis `json:"hypothesis"`
	Mean       float64    `json:"mean"`
	Lift       float64    `json:"lift"`
}

// VistaGuard implements the VISTA decoupling of hypothesis generation
// from selection: Select draws once per round from the validated pool
// (uniform exploration with probability Epsilon, argmax-lift
// exploitation otherwise), Record only counts stagnation. The restart
// decision lives in the main loop, which consumes Stagnant(), emits
// the event and calls Reset — Record never resets on its own so the
// trigger stays observable.
type VistaGuard struct {
	Epsilon         float64
	StagnationLimit int

	rng      *rand.Rand
	stagnant int
}

// NewVistaGuard returns a guard drawing from rng. Epsilon 0 always
// exploits, 1 always explores; the same rng stream replays the same
// decisions.
func NewVistaGuard(epsilon float64, stagnationLimit int, rng *rand.Rand) *VistaGuard {
	return &VistaGuard{Epsilon: epsilon, StagnationLimit: stagnationLimit, rng: rng}
}

// Select draws one hypothesis from the pool: a single rng sample
// routes between uniform exploration and argmax exploitation. Ties
// break by lift (desc), confidence (desc), id (asc). An empty pool
// returns the zero value with an empty mode.
func (v *VistaGuard) Select(pool []ValidatedHypothesis) (ValidatedHypothesis, string) {
	if len(pool) == 0 {
		return ValidatedHypothesis{}, ""
	}
	if v.rng.Float64() < v.Epsilon {
		return pool[v.rng.IntN(len(pool))], ModeExplore
	}
	return argmaxLift(pool), ModeExploit
}

// Record counts one round of progress. Progress resets the counter;
// no progress increments it. Crossing the limit does not reset — that
// is the main loop's job via Reset.
func (v *VistaGuard) Record(progress bool) {
	if progress {
		v.stagnant = 0
		return
	}
	v.stagnant++
}

// Stagnant returns the consecutive rounds without progress.
func (v *VistaGuard) Stagnant() int { return v.stagnant }

// Reset explicitly clears the stagnation counter (Fresh restart).
func (v *VistaGuard) Reset() { v.stagnant = 0 }

// argmaxLift picks the best hypothesis by lift (desc), confidence
// (desc), id (asc) — a deterministic total order.
func argmaxLift(pool []ValidatedHypothesis) ValidatedHypothesis {
	best := pool[0]
	for _, h := range pool[1:] {
		switch {
		case h.Lift != best.Lift:
			if h.Lift > best.Lift {
				best = h
			}
		case h.Hypothesis.Confidence != best.Hypothesis.Confidence:
			if h.Hypothesis.Confidence > best.Hypothesis.Confidence {
				best = h
			}
		case h.Hypothesis.ID < best.Hypothesis.ID:
			best = h
		}
	}
	return best
}

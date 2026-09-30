package protegi

import (
	"math"
	"math/rand/v2"
	"slices"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// bandit is a UCB1 multi-armed bandit over the fold arms — the
// ProTeGi bandit allocation simplified (the paper leaves the rule
// unspecified). Select plays every unvisited arm first in index
// order, then the arm maximizing mean + √(2·lnT/n). Ties break
// toward the lower index, so selection is deterministic given the
// update history and the same seed replays identically.
type bandit struct {
	pulls  []int
	reward []float64 // cumulative reward per arm
	total  int       // pulls across arms, == sum(pulls)
}

// newBandit returns a fresh bandit over arms (>= 1) with every arm
// unvisited.
func newBandit(arms int) *bandit {
	return &bandit{pulls: make([]int, arms), reward: make([]float64, arms)}
}

// Select returns the next arm to play.
func (b *bandit) Select() int {
	for i, n := range b.pulls {
		if n == 0 {
			return i
		}
	}
	best, bestScore := 0, math.Inf(-1)
	for i, n := range b.pulls {
		score := b.reward[i]/float64(n) + math.Sqrt(2*math.Log(float64(b.total))/float64(n))
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// Update records reward for arm (lift may be negative — the mean term
// handles it; only the count term must stay positive).
func (b *bandit) Update(arm int, reward float64) {
	b.pulls[arm]++
	b.reward[arm] += reward
	b.total++
}

// makeFolds shuffles samples once and cuts the shuffled copy into k
// contiguous folds: the same rng seed therefore yields the same fold
// order. k is clamped to [1, len(samples)], so a single-sample
// retained set degenerates to one fold and a k above the set size
// yields singleton folds. The folds alias one shuffled copy — callers
// treat them as read-only evidence batches.
func makeFolds(samples []core.Sample, k int, rng *rand.Rand) [][]core.Sample {
	if len(samples) == 0 {
		return nil
	}
	k = min(max(k, 1), len(samples))
	shuffled := slices.Clone(samples)
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	folds := make([][]core.Sample, k)
	for i := range folds {
		folds[i] = shuffled[i*len(shuffled)/k : (i+1)*len(shuffled)/k]
	}
	return folds
}

// batchMean is cand's primary mean over batch — missing evidence
// cells count as 0, matching Loop.BatchRecords' zero-fill projection
// so parent and child means stay comparable on the fold.
func batchMean(loop *engine.Loop, candID string, batch []core.Sample) float64 {
	recs := loop.BatchRecords(candID, batch)
	if len(recs) == 0 {
		return 0
	}
	sum := 0.0
	for _, rec := range recs {
		sum += rec.Scores[loop.Primary()]
	}
	return sum / float64(len(recs))
}

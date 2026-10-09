package engine

import (
	"math"
	"math/rand/v2"
	"slices"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Operator names recorded in lineage and frontier members.
const (
	OpBaseline = "baseline"
	OpRewrite  = "rewrite"
	OpMerge    = "merge"
	OpRestart  = "restart"
)

// Member is one candidate on the frontier. Scores is the per-sample
// primary-metric row over the retained set (fixed sample order); Means
// carries the per-metric means of the producing evaluation. With
// multi-rep sampling Scores are k-rep means and SD is the per-sample
// in-sample standard deviation of the primary metric over the same
// fixed order (nil when the row was measured single-shot, i.e. the
// historical shape); Reps records the sampling depth (0 reads as 1).
type Member struct {
	Candidate core.Candidate     `json:"candidate"`
	Scores    []float64          `json:"scores"`
	Means     map[string]float64 `json:"means"`
	SD        []float64          `json:"sd,omitempty"`
	Reps      int                `json:"reps,omitempty"`
	Round     int                `json:"round"`
	Operator  string             `json:"operator"`
}

// ID returns the member's candidate id.
func (m Member) ID() string { return m.Candidate.ID }

// Dominates reports whether score vector a Pareto-dominates b: a is at
// least as good on every sample and strictly better on at least one.
// Constraint metrics deliberately stay out of dominance (scale mixing)
// and only feed the Best hard filter. Vectors of unequal length never
// dominate.
func Dominates(a, b []float64) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	strictlyBetter := false
	for i := range a {
		if a[i] < b[i] {
			return false
		}
		if a[i] > b[i] {
			strictlyBetter = true
		}
	}
	return strictlyBetter
}

// DominatesWithMargin reports whether a dominates b under per-sample
// noise margins eps: a_i ≥ b_i+eps_i on every sample and strictly
// greater after the margin on at least one. The margin turns
// dominance into a claim of advantage beyond sampling noise: an edge
// smaller than the reps-derived ε must not decide admission or
// eviction. eps entries are expected non-negative; unequal vector
// lengths never dominate (same contract as Dominates).
func DominatesWithMargin(a, b, eps []float64) bool {
	if len(a) == 0 || len(a) != len(b) || len(eps) != len(a) {
		return false
	}
	strictlyBetter := false
	for i := range a {
		if a[i] < b[i]+eps[i] {
			return false
		}
		if a[i] > b[i]+eps[i] {
			strictlyBetter = true
		}
	}
	return strictlyBetter
}

// isEpsClone reports whether a and b are indistinguishable beyond the
// noise margin: the largest per-sample absolute difference stays
// within the largest ε. A candidate within the noise band of a current
// member carries no decision-relevant evidence, so it must not enter
// (or evict) based on that residue.
func isEpsClone(a, b, eps []float64) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	maxEps := 0.0
	for _, e := range eps {
		maxEps = max(maxEps, e)
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > maxEps {
			return false
		}
	}
	return true
}

// Frontier is the Pareto frontier over per-sample primary rows.
// Monotonicity: admission rejects any candidate dominated by (or a
// score-clone of) a current member, eviction only removes members the
// newcomer dominates — by transitivity the members stay pairwise
// non-dominating.
type Frontier struct {
	members []Member
}

// Add admits m unless a current member dominates it or shares its
// exact score vector (clone); admitted members dominated by m are
// evicted. It returns whether m was admitted and the evicted ids.
func (f *Frontier) Add(m Member) (admitted bool, evictedIDs []string) {
	for _, cur := range f.members {
		if Dominates(cur.Scores, m.Scores) || slices.Equal(cur.Scores, m.Scores) {
			return false, nil
		}
	}
	kept := make([]Member, 0, len(f.members)+1)
	for _, cur := range f.members {
		if Dominates(m.Scores, cur.Scores) {
			evictedIDs = append(evictedIDs, cur.ID())
			continue
		}
		kept = append(kept, cur)
	}
	f.members = append(kept, m)
	return true, evictedIDs
}

// AddNoiseAware is the reps-aware admission path: for every comparison
// against a current member, marginOf supplies that pair's per-sample
// ε (the pooled noise margin, caller-owned). Where marginOf returns a
// usable ε row, admission is denied when the current member dominates
// m beyond the margin or the pair is an ε-clone (max |Δ_i| ≤ max ε_i),
// and eviction requires m to dominate the incumbent beyond the margin
// — a sub-ε edge never decides. Where marginOf returns nil (no
// per-sample SD on either side, the default single-shot path) the
// comparison falls back to the exact Add semantics, so the default
// path is byte-for-byte unchanged. A mixed pair (one side carries SD,
// the other does not) degrades to Add too: no margin protection, but
// no false eviction either.
func (f *Frontier) AddNoiseAware(m Member, marginOf func(cur Member) []float64) (admitted bool, evictedIDs []string) {
	epsOf := func(cur Member) []float64 {
		if marginOf == nil {
			return nil
		}
		return marginOf(cur)
	}
	for _, cur := range f.members {
		if eps := epsOf(cur); len(eps) > 0 {
			if DominatesWithMargin(cur.Scores, m.Scores, eps) || isEpsClone(cur.Scores, m.Scores, eps) {
				return false, nil
			}
			continue
		}
		if Dominates(cur.Scores, m.Scores) || slices.Equal(cur.Scores, m.Scores) {
			return false, nil
		}
	}
	kept := make([]Member, 0, len(f.members)+1)
	for _, cur := range f.members {
		dominated := false
		if eps := epsOf(cur); len(eps) > 0 {
			dominated = DominatesWithMargin(m.Scores, cur.Scores, eps)
		} else {
			dominated = Dominates(m.Scores, cur.Scores)
		}
		if dominated {
			evictedIDs = append(evictedIDs, cur.ID())
			continue
		}
		kept = append(kept, cur)
	}
	f.members = append(kept, m)
	return true, evictedIDs
}

// Size returns the current member count.
func (f *Frontier) Size() int { return len(f.members) }

// Members returns a copy of the current members in admission order.
func (f *Frontier) Members() []Member { return slices.Clone(f.members) }

// UniformPick draws one member uniformly at random.
func (f *Frontier) UniformPick(rng *rand.Rand) Member {
	return f.members[rng.IntN(len(f.members))]
}

// Complement returns the merge partner for parent: the member scoring
// highest on the samples where someone strictly beats parent. With no
// such samples it falls back to the best primary mean. Ties break by
// primary mean (desc) then id (asc). Caller guarantees Size >= 2.
func (f *Frontier) Complement(parent Member, primary string) Member {
	failing := make([]int, 0, len(parent.Scores))
	for i := range parent.Scores {
		for _, m := range f.members {
			if m.ID() != parent.ID() && m.Scores[i] > parent.Scores[i] {
				failing = append(failing, i)
				break
			}
		}
	}
	scoreOf := func(m Member) float64 {
		if len(failing) == 0 {
			return m.Means[primary]
		}
		sum := 0.0
		for _, i := range failing {
			sum += m.Scores[i]
		}
		return sum / float64(len(failing))
	}
	var best Member
	for _, m := range f.members {
		if m.ID() == parent.ID() {
			continue
		}
		if best.Candidate.ID == "" || betterMember(m, best, scoreOf, primary) {
			best = m
		}
	}
	return best
}

// betterMember orders two candidates by score (desc), primary mean
// (desc) and id (asc) — a deterministic total order.
func betterMember(a, b Member, score func(Member) float64, primary string) bool {
	sa, sb := score(a), score(b)
	switch {
	case sa != sb:
		return sa > sb
	case a.Means[primary] != b.Means[primary]:
		return a.Means[primary] > b.Means[primary]
	default:
		return a.ID() < b.ID()
	}
}

// Wins counts the samples where member id exclusively wins: strictly
// better than every other member and above zero.
func (f *Frontier) Wins(id string) int {
	var m Member
	found := false
	for _, cur := range f.members {
		if cur.ID() == id {
			m, found = cur, true
			break
		}
	}
	if !found {
		return 0
	}
	wins := 0
	for i, v := range m.Scores {
		if v <= 0 {
			continue
		}
		exclusive := true
		for _, o := range f.members {
			if o.ID() != id && o.Scores[i] >= v {
				exclusive = false
				break
			}
		}
		if exclusive {
			wins++
		}
	}
	return wins
}

// Best picks the deliverable member. When constraint is declared, it
// hard-filters members whose constraint mean is 1, then orders by
// primary mean (desc), constraint mean (desc) and id (asc). If every
// member fails the constraint, it falls back to (constraint mean,
// primary mean) over all members and reports satisfied=false with a
// note.
func (f *Frontier) Best(primary, constraint string) (Member, bool, string) {
	pool := f.members
	if constraint != "" {
		filtered := make([]Member, 0, len(pool))
		for _, m := range pool {
			if m.Means[constraint] >= 1 {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) == 0 {
			best := f.maxBy(pool, func(m Member) float64 { return m.Means[constraint] }, primary)
			return best, false, "前沿没有成员满足约束 " + constraint + " 均值=1，已按（约束均值, 主指标）兜底交付"
		}
		pool = filtered
	}
	return f.maxBy(pool, func(m Member) float64 { return m.Means[primary] }, constraint), true, ""
}

// maxBy returns the member maximizing key; ties break by constraint
// mean (desc, when declared) then id (asc).
func (f *Frontier) maxBy(pool []Member, key func(Member) float64, constraint string) Member {
	order := func(a, b Member) bool {
		ka, kb := key(a), key(b)
		if ka != kb {
			return ka > kb
		}
		if ca, cb := a.Means[constraint], b.Means[constraint]; ca != cb {
			return ca > cb
		}
		return a.ID() < b.ID()
	}
	best := pool[0]
	for _, m := range pool[1:] {
		if order(m, best) {
			best = m
		}
	}
	return best
}

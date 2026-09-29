package engine

import (
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
// carries the per-metric means of the producing evaluation.
type Member struct {
	Candidate core.Candidate     `json:"candidate"`
	Scores    []float64          `json:"scores"`
	Means     map[string]float64 `json:"means"`
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

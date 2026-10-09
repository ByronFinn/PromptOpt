package eval

import (
	"maps"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Budget gates sample dispatch on two limits, accounting atomically
// per role. TokenLimit bounds the evaluation roles' prompt+completion
// tokens — executor and judge share one valve so the total evaluation
// cost keeps its pre-split semantics (judge spend was previously
// merged into the executor key); usage is only known after a response,
// so crossing it arms a soft stop — no new samples are dispatched
// while in-flight ones complete. EvalLimit is checked before dispatch
// and is never exceeded. Zero means unlimited.
type Budget struct {
	mu         sync.Mutex
	tokenLimit int64
	evalLimit  int64
	evals      int64
	usage      map[core.Role]core.Usage
	softStop   bool
}

// NewBudget returns a budget; zero limits are unlimited.
func NewBudget(tokenLimit, evalLimit int64) *Budget {
	return &Budget{
		tokenLimit: tokenLimit,
		evalLimit:  evalLimit,
		usage:      make(map[core.Role]core.Usage),
	}
}

// TryAcquireEval reserves one executor evaluation slot atomically. It
// fails when a soft stop is in effect or the eval limit would be
// exceeded.
func (b *Budget) TryAcquireEval() bool {
	return b.TryAcquireEvals(1)
}

// TryAcquireEvals reserves k executor evaluation slots atomically —
// all or nothing: on failure nothing is occupied, on success exactly k
// slots are. It is the dispatch-side unit for multi-rep sampling: a
// sample costing k provider passes either fits whole into the eval
// limit or stays undispatched, so --budget-evals never undercounts a
// partially evaluated sample. It fails when a soft stop is in effect
// or the eval limit would be exceeded.
func (b *Budget) TryAcquireEvals(k int64) bool {
	if k < 1 {
		k = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.softStop {
		return false
	}
	if b.evalLimit > 0 && b.evals+k > b.evalLimit {
		return false
	}
	b.evals += k
	return true
}

// EvalLimit returns the configured executor evaluation cap (0 =
// unlimited). Reporting and budget planning read it; the accounting
// state lives in Snapshot.
func (b *Budget) EvalLimit() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.evalLimit
}

// RecordUsage adds one call's usage to its role. The token limit stays
// one valve over the total evaluation cost: spending from both
// evaluation roles — executor and judge — is summed against it, so
// judge usage keeps arming the executor valve it historically shared
// (used == limit keeps dispatching) even though the per-role buckets
// are reported separately. Per-role buckets compared against the limit
// individually would silently delay the arm (a sample's 60+60 judge
// and executor spend must trip a 100-token valve as 120, not as two
// 60s) and turn which sample gets refused into a dispatch race.
func (b *Budget) RecordUsage(role core.Role, u core.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.usage[role]
	cur.PromptTokens += u.PromptTokens
	cur.CompletionTokens += u.CompletionTokens
	b.usage[role] = cur
	if (role == core.RoleExecutor || role == core.RoleJudge) && b.tokenLimit > 0 {
		total := b.usage[core.RoleExecutor].Total() + b.usage[core.RoleJudge].Total()
		if total > b.tokenLimit {
			b.softStop = true
		}
	}
}

// SoftStopped reports whether an evaluation role's token budget was
// crossed.
func (b *Budget) SoftStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.softStop
}

// Snapshot returns evaluations started so far and a copy of the
// per-role usage totals.
func (b *Budget) Snapshot() (evals int64, usage map[core.Role]core.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[core.Role]core.Usage, len(b.usage))
	maps.Copy(out, b.usage)
	return b.evals, out
}

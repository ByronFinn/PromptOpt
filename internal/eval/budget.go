package eval

import (
	"maps"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Budget gates sample dispatch on two limits, accounting atomically
// per role. TokenLimit bounds the executor role's prompt+completion
// tokens; usage is only known after a response, so crossing it arms a
// soft stop — no new samples are dispatched while in-flight ones
// complete. EvalLimit is checked before dispatch and is never
// exceeded. Zero means unlimited.
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
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.softStop {
		return false
	}
	if b.evalLimit > 0 && b.evals+1 > b.evalLimit {
		return false
	}
	b.evals++
	return true
}

// RecordUsage adds one call's usage to its role. Strictly crossing the
// executor token limit arms the soft stop: used == limit keeps
// dispatching.
func (b *Budget) RecordUsage(role core.Role, u core.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cur := b.usage[role]
	cur.PromptTokens += u.PromptTokens
	cur.CompletionTokens += u.CompletionTokens
	b.usage[role] = cur
	if role == core.RoleExecutor && b.tokenLimit > 0 && cur.Total() > b.tokenLimit {
		b.softStop = true
	}
}

// SoftStopped reports whether the executor token budget was crossed.
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

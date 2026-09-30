// Package optimizers hosts the optimizer paradigm registry and the
// auto-routing decision table shared by every paradigm implementation.
// The package root deliberately imports no paradigm subpackage —
// registration is centralized in internal/optimizers/builtin, the
// single paradigm manifest and the extension point for in-repo
// paradigms (docs/plugins.md) — so the dependency edge builtin →
// paradigms → engine cannot cycle.
package optimizers

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Factory constructs one optimizer instance. Implementations are
// stateless (per-run state lives inside Optimize), so factories may be
// called once per run.
type Factory func() engine.Optimizer

// Capabilities declares a paradigm's identity plus the routing
// metadata auto mode consults. The Suits* fields document which
// TaskFeatures the paradigm naturally serves (docs/plugins.md); the
// Route decision table is feature-driven and ordered, not
// capability-driven — capabilities are the catalog, Route is the
// choice.
type Capabilities struct {
	// Name is the --optimizer value ("gepa", "protegi", ...).
	Name string
	// Label is the human-facing Chinese display name rendered on the
	// dashboard's compare page.
	Label string
	// Summary is a one-line description of the search mechanism.
	Summary string
	// Paper cites the paradigm's origin.
	Paper string
	// SuitsPipeline marks fit for staged pipeline tasks (V6 task-level
	// declaration).
	SuitsPipeline bool
	// SuitsDirectional marks fit for text-gradient style directional
	// feedback loops.
	SuitsDirectional bool
	// SuitsJointFewShot marks fit for joint instruction + few-shot
	// demo search.
	SuitsJointFewShot bool
	// SuitsMultiConstraint marks fit for Pareto trade-offs across
	// several metrics.
	SuitsMultiConstraint bool
	// SuitsTightBudget marks fit for cheap search under a small
	// evaluation budget.
	SuitsTightBudget bool
}

// Descriptor couples one paradigm's capabilities with its factory.
type Descriptor struct {
	Capabilities
	Factory Factory
}

// Registry is the name→descriptor table behind --optimizer.
type Registry struct {
	mu     sync.Mutex
	byName map[string]Descriptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Descriptor)}
}

// Register adds d. It panics on an empty name, a duplicate name or a
// nil factory: registration happens at process startup where failing
// fast beats a deferred runtime error. The cmd layer validates user
// input against Names() before any run, so a panic here is a
// programming error, not user input.
func (r *Registry) Register(d Descriptor) {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		panic("optimizers: Register with empty name")
	}
	if d.Factory == nil {
		panic(fmt.Sprintf("optimizers: Register %q with nil factory", name))
	}
	d.Name = name
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		panic(fmt.Sprintf("optimizers: duplicate registration %q", name))
	}
	r.byName[name] = d
}

// Get returns the descriptor registered under name.
func (r *Registry) Get(name string) (Descriptor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byName[name]
	return d, ok
}

// Names returns every registered paradigm name, sorted.
func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Build constructs the optimizer registered under name. An unknown
// name errors listing the registered options — the second line of
// defense; the cmd layer validates names during flag parsing, before
// any synthesis budget is spent.
func (r *Registry) Build(name string) (engine.Optimizer, error) {
	d, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("unknown optimizer %q (registered: %s)", name, strings.Join(r.Names(), ", "))
	}
	return d.Factory(), nil
}

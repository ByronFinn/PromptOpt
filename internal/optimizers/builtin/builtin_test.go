package builtin

import (
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/optimizers"
)

// TestRegistryP1Usable pins the real builtin manifest: the p1 paradigm
// is registered, claims the tight-budget capability and constructs —
// the auto router's tight-budget rule must resolve onto it without
// degradation (提案 §3.1 ①).
func TestRegistryP1Usable(t *testing.T) {
	reg := Registry()
	d, ok := reg.Get("p1")
	if !ok {
		t.Fatalf("builtin registry lacks p1; registered: %v", reg.Names())
	}
	if !d.SuitsTightBudget {
		t.Error("p1 descriptor does not claim SuitsTightBudget")
	}
	opt, err := reg.Build("p1")
	if err != nil || opt == nil {
		t.Fatalf("Build(p1) = %v, %v; want a constructed optimizer", opt, err)
	}

	decision := optimizers.Route(reg, optimizers.TaskFeatures{TightBudget: true})
	if decision.Paradigm != "p1" || decision.Requested != "p1" || decision.Degraded {
		t.Errorf("Route(TightBudget) = %+v, want an undegraded p1", decision)
	}
}

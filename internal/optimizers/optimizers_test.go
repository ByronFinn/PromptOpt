package optimizers

import (
	"context"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// stubOptimizer stands in for real paradigms; Build only needs a
// non-nil engine.Optimizer.
type stubOptimizer struct{}

func (stubOptimizer) Optimize(context.Context, engine.Request) (engine.Result, error) {
	return engine.Result{}, nil
}

// fullRegistry registers the four V5 paradigm names with stub
// factories — the routing table's complete target set.
func fullRegistry() *Registry {
	reg := NewRegistry()
	for _, name := range []string{"gepa", "protegi", "miprov2", "evoprompt"} {
		reg.Register(Descriptor{
			Capabilities: Capabilities{Name: name, Label: name + "-标签"},
			Factory:      func() engine.Optimizer { return stubOptimizer{} },
		})
	}
	return reg
}

func TestRegistryRegisterGetNamesBuild(t *testing.T) {
	reg := fullRegistry()
	if names := reg.Names(); strings.Join(names, ",") != "evoprompt,gepa,miprov2,protegi" {
		t.Errorf("Names = %v, want sorted", names)
	}
	d, ok := reg.Get("gepa")
	if !ok || d.Name != "gepa" || d.Label != "gepa-标签" {
		t.Errorf("Get(gepa) = %+v ok=%v", d, ok)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get(nope) reported present")
	}
	opt, err := reg.Build("miprov2")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if opt == nil {
		t.Fatal("Build returned a nil optimizer")
	}
}

func TestRegistryBuildUnknownListsOptions(t *testing.T) {
	reg := fullRegistry()
	_, err := reg.Build("bogus")
	if err == nil {
		t.Fatal("Build(bogus) succeeded")
	}
	for _, want := range []string{"bogus", "gepa", "protegi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Build error %q missing %q", err, want)
		}
	}
}

func TestRegistryRegisterPanics(t *testing.T) {
	cases := []struct {
		name string
		reg  func() *Registry
		want string
	}{
		{"duplicate name", func() *Registry {
			reg := fullRegistry()
			reg.Register(Descriptor{Capabilities: Capabilities{Name: "gepa"}, Factory: func() engine.Optimizer { return stubOptimizer{} }})
			return reg
		}, "duplicate registration \"gepa\""},
		{"nil factory", func() *Registry {
			reg := NewRegistry()
			reg.Register(Descriptor{Capabilities: Capabilities{Name: "x"}})
			return reg
		}, "nil factory"},
		{"empty name", func() *Registry {
			reg := NewRegistry()
			reg.Register(Descriptor{Factory: func() engine.Optimizer { return stubOptimizer{} }})
			return reg
		}, "empty name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("Register did not panic, want %q", tc.want)
				}
				if msg, ok := r.(string); !ok || !strings.Contains(msg, tc.want) {
					t.Errorf("panic = %v, want it to contain %q", r, tc.want)
				}
			}()
			tc.reg()
		})
	}
}

func TestRouteFiveRules(t *testing.T) {
	cases := []struct {
		name      string
		features  TaskFeatures
		paradigm  string
		requested string
		degraded  bool
	}{
		{"default", TaskFeatures{}, "protegi", "protegi", false},
		{"pipeline degrades textgrad", TaskFeatures{Pipeline: true}, "protegi", "textgrad", true},
		{"tight budget degrades p1", TaskFeatures{TightBudget: true}, "gepa", "p1", true},
		{"joint few-shot", TaskFeatures{JointFewShot: true}, "miprov2", "miprov2", false},
		{"multi constraint", TaskFeatures{MultiConstraint: true}, "gepa", "gepa", false},
		// Priority: budget affordability outranks data shape, which
		// outranks multi-metric trade-offs; pipeline declaration wins
		// over everything.
		{"tight budget beats joint few-shot", TaskFeatures{TightBudget: true, JointFewShot: true}, "gepa", "p1", true},
		{"joint few-shot beats multi constraint", TaskFeatures{JointFewShot: true, MultiConstraint: true}, "miprov2", "miprov2", false},
		{"pipeline beats tight budget", TaskFeatures{Pipeline: true, TightBudget: true}, "protegi", "textgrad", true},
	}
	reg := fullRegistry()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Route(reg, tc.features)
			if d.Paradigm != tc.paradigm || d.Requested != tc.requested || d.Degraded != tc.degraded {
				t.Errorf("decision = %+v, want paradigm=%s requested=%s degraded=%v", d, tc.paradigm, tc.requested, tc.degraded)
			}
			if strings.TrimSpace(d.Reason) == "" {
				t.Error("decision reason is empty")
			}
		})
	}
}

func TestRouteDegradeReasonNamesParadigm(t *testing.T) {
	d := Route(fullRegistry(), TaskFeatures{TightBudget: true})
	if !strings.Contains(d.Reason, "gepa") || !strings.Contains(d.Reason, "p1") {
		t.Errorf("reason = %q, want it to name p1 and gepa", d.Reason)
	}
}

func TestRouteAvailabilityFallback(t *testing.T) {
	// Skeleton phase: only gepa registered — every non-gepa route
	// degrades onto it instead of failing.
	reg := NewRegistry()
	reg.Register(Descriptor{
		Capabilities: Capabilities{Name: "gepa", Label: "GEPA 反射进化"},
		Factory:      func() engine.Optimizer { return stubOptimizer{} },
	})
	d := Route(reg, TaskFeatures{})
	if d.Paradigm != "gepa" || d.Requested != "protegi" || !d.Degraded {
		t.Errorf("fallback decision = %+v, want gepa/protegi/degraded", d)
	}
	if !strings.Contains(d.Reason, "回退 gepa") {
		t.Errorf("reason = %q, want the fallback trail", d.Reason)
	}
	// gepa itself routes unchanged.
	if d := Route(reg, TaskFeatures{MultiConstraint: true}); d.Paradigm != "gepa" || d.Degraded {
		t.Errorf("gepa route = %+v, want undegraded gepa", d)
	}

	// Empty registry: pass through, Build reports the options.
	if d := Route(NewRegistry(), TaskFeatures{}); d.Paradigm != "protegi" || d.Degraded {
		t.Errorf("empty-registry decision = %+v, want passthrough protegi", d)
	}
}

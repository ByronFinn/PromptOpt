package engine

import (
	"math/rand/v2"
	"slices"
	"testing"
)

func hyp(id string, lift, confidence float64) ValidatedHypothesis {
	return ValidatedHypothesis{
		Hypothesis: Hypothesis{ID: id, Text: "text " + id, Confidence: confidence},
		Mean:       lift, Lift: lift,
	}
}

// TestVistaEpsilonZeroAlwaysArgmax: with ε=0 the guard must exploit
// the argmax-lift hypothesis across many random pools.
func TestVistaEpsilonZeroAlwaysArgmax(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	v := NewVistaGuard(0, 3, rng)
	for round := range 50 {
		n := 1 + rng.IntN(4)
		pool := make([]ValidatedHypothesis, n)
		for i := range pool {
			pool[i] = hyp(string(rune('a'+i)), rng.Float64()*2-1, rng.Float64())
		}
		got, mode := v.Select(pool)
		if mode != ModeExploit {
			t.Fatalf("round %d: mode = %q, want exploit", round, mode)
		}
		want := argmaxLift(pool)
		if got.Hypothesis.ID != want.Hypothesis.ID {
			t.Fatalf("round %d: selected %s, want argmax %s", round, got.Hypothesis.ID, want.Hypothesis.ID)
		}
	}
}

// TestVistaEpsilonOneReplaysAndExplores: with ε=1 the guard explores
// uniformly — the same seed replays the same selection sequence, and
// a constructed seed demonstrably picks a non-argmax hypothesis.
func TestVistaEpsilonOneReplaysAndExplores(t *testing.T) {
	pool := []ValidatedHypothesis{hyp("hi", 0.9, 0.5), hyp("lo", 0.1, 0.9)}
	draw := func() []string {
		v := NewVistaGuard(1, 3, rand.New(rand.NewPCG(11, 11)))
		out := make([]string, 20)
		for i := range out {
			sel, mode := v.Select(pool)
			if mode != ModeExplore {
				out[i] = mode + ":" + sel.Hypothesis.ID
				continue
			}
			out[i] = sel.Hypothesis.ID
		}
		return out
	}
	first, second := draw(), draw()
	if !slices.Equal(first, second) {
		t.Errorf("same-seed exploration diverged: %v vs %v", first, second)
	}
	sawNonArgmax := slices.Contains(first, "lo")
	if !sawNonArgmax {
		t.Errorf("ε=1 over 20 draws never left the argmax: %v", first)
	}
	if !slices.Contains(first, "hi") {
		t.Errorf("ε=1 never sampled the argmax either: %v", first)
	}
}

// TestVistaRecordSemantics: no progress increments, progress resets,
// and crossing the limit does NOT auto-reset — only Reset does. That
// keeps the restart trigger observable until the loop consumes it.
func TestVistaRecordSemantics(t *testing.T) {
	v := NewVistaGuard(0.2, 3, rand.New(rand.NewPCG(1, 1)))
	if got := v.Stagnant(); got != 0 {
		t.Fatalf("fresh guard stagnant = %d, want 0", got)
	}
	v.Record(false)
	v.Record(false)
	if got := v.Stagnant(); got != 2 {
		t.Fatalf("stagnant after two failures = %d, want 2", got)
	}
	// Cross the limit: Record must not clear it (Fresh reachability).
	v.Record(false)
	if got := v.Stagnant(); got != 3 {
		t.Fatalf("stagnant at the limit = %d, want 3", got)
	}
	v.Record(false)
	if got := v.Stagnant(); got != 4 {
		t.Fatalf("stagnant past the limit = %d, want 4 (no auto reset)", got)
	}
	// Progress resets.
	v.Record(true)
	if got := v.Stagnant(); got != 0 {
		t.Fatalf("stagnant after progress = %d, want 0", got)
	}
	// Explicit reset from a tripped state.
	v.Record(false)
	v.Record(false)
	v.Record(false)
	if got := v.Stagnant(); got < 3 {
		t.Fatalf("stagnant = %d, want >= 3 before Reset", got)
	}
	v.Reset()
	if got := v.Stagnant(); got != 0 {
		t.Fatalf("stagnant after Reset = %d, want 0", got)
	}
}

// TestVistaSelectEmptyPoolAndTies: an empty pool returns a zero value
// with an empty mode; exploit-path ties break by confidence then id
// (ε=0 forces the argmax path, keeping the case seed-independent).
func TestVistaSelectEmptyPoolAndTies(t *testing.T) {
	v := NewVistaGuard(0, 3, rand.New(rand.NewPCG(5, 5)))
	if _, mode := v.Select(nil); mode != "" {
		t.Errorf("empty pool mode = %q, want empty", mode)
	}

	// Same lift, different confidence.
	pool := []ValidatedHypothesis{hyp("a", 0.5, 0.9), hyp("b", 0.5, 0.2)}
	if sel, _ := v.Select(pool); sel.Hypothesis.ID != "a" {
		t.Errorf("confidence tie-break selected %s, want a", sel.Hypothesis.ID)
	}
	// Same lift and confidence, id asc.
	pool = []ValidatedHypothesis{hyp("b", 0.5, 0.5), hyp("a", 0.5, 0.5)}
	if sel, _ := v.Select(pool); sel.Hypothesis.ID != "a" {
		t.Errorf("id tie-break selected %s, want a", sel.Hypothesis.ID)
	}
}

package engine

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

func member(id string, scores []float64, means map[string]float64) Member {
	return Member{Candidate: core.Candidate{ID: id, Prompt: "prompt of " + id + " {input}"}, Scores: scores, Means: means}
}

func TestDominatesTruthTable(t *testing.T) {
	cases := []struct {
		name string
		a, b []float64
		want bool
	}{
		{"strictly better everywhere", []float64{1, 1}, []float64{0, 0}, true},
		{"better with one strict", []float64{1, 0.5}, []float64{1, 0}, true},
		{"equal vectors never dominate", []float64{1, 0}, []float64{1, 0}, false},
		{"worse on one sample", []float64{1, 0, 0, 0}, []float64{0, 1, 0, 1}, false},
		{"mirror of previous", []float64{0, 1, 0, 1}, []float64{1, 0, 0, 0}, false},
		{"strictly worse everywhere", []float64{0, 0}, []float64{1, 1}, false},
		{"unequal lengths", []float64{1, 1, 1}, []float64{1, 1}, false},
		{"empty vectors", nil, nil, false},
		{"fractional scores", []float64{0.5, 0.5}, []float64{0.5, 0.2}, true},
	}
	for _, tc := range cases {
		if got := Dominates(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: Dominates(%v, %v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// TestFrontierScriptedSequence drives the scripted 4-sample sequence
// from the design: A and B coexist, D evicts A, E joins D, clones are
// rejected. After every step the members must stay pairwise
// non-dominating and no evicted member may dominate a later admission.
func TestFrontierScriptedSequence(t *testing.T) {
	f := &Frontier{}
	var evicted []Member

	assertInvariant := func(step string) {
		t.Helper()
		members := f.Members()
		for i := range members {
			for j := range members {
				if i != j && Dominates(members[i].Scores, members[j].Scores) {
					t.Fatalf("%s: members %s dominates %s — monotonicity broken", step, members[i].ID(), members[j].ID())
				}
			}
		}
		for _, later := range members {
			for _, gone := range evicted {
				if Dominates(gone.Scores, later.Scores) {
					t.Fatalf("%s: evicted %s dominates later member %s", step, gone.ID(), later.ID())
				}
			}
		}
	}

	// A = [1,0,0,0] admitted into the empty frontier.
	if ok, _ := f.Add(member("A", []float64{1, 0, 0, 0}, nil)); !ok {
		t.Fatal("A must be admitted into an empty frontier")
	}
	assertInvariant("after A")

	// B = [0,1,0,1] — mutual non-domination with A, coexists.
	if ok, ev := f.Add(member("B", []float64{0, 1, 0, 1}, nil)); !ok || len(ev) != 0 {
		t.Fatalf("B must coexist with A, admitted=%v evicted=%v", ok, ev)
	}
	assertInvariant("after B")

	// D = [1,1,0,0] dominates A (≥ everywhere, strictly on sample 2)
	// but not B → A evicted, B survives.
	ok, ev := f.Add(member("D", []float64{1, 1, 0, 0}, nil))
	if !ok || !slices.Equal(ev, []string{"A"}) {
		t.Fatalf("D must evict exactly A, admitted=%v evicted=%v", ok, ev)
	}
	evicted = append(evicted, member("A", []float64{1, 0, 0, 0}, nil))
	assertInvariant("after D")

	// E = [0.8,0.8,1,1] — neither D nor the evicted A dominates it →
	// admitted alongside D.
	if ok, ev := f.Add(member("E", []float64{0.8, 0.8, 1, 1}, nil)); !ok || len(ev) != 0 {
		t.Fatalf("E must coexist with D, admitted=%v evicted=%v", ok, ev)
	}
	assertInvariant("after E")

	if got := f.Size(); got != 3 {
		t.Fatalf("frontier size = %d, want 3 (B, D, E)", got)
	}
	for _, want := range []string{"B", "D", "E"} {
		if !slices.ContainsFunc(f.Members(), func(m Member) bool { return m.ID() == want }) {
			t.Fatalf("frontier misses %s after E", want)
		}
	}

	// Clone of D's vector is rejected.
	if ok, _ := f.Add(member("D2", []float64{1, 1, 0, 0}, nil)); ok {
		t.Fatal("a vector clone of D must be rejected")
	}
	// A vector dominated by D is rejected too.
	if ok, _ := f.Add(member("A2", []float64{0.9, 0.9, 0, 0}, nil)); ok {
		t.Fatal("a vector dominated by D must be rejected")
	}
	assertInvariant("after rejections")
}

// TestFrontierRandomAdds replays 500 random vectors through one PCG
// stream and re-asserts pairwise non-domination after every add.
func TestFrontierRandomAdds(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 42))
	f := &Frontier{}
	for i := range 500 {
		scores := make([]float64, 4)
		for j := range scores {
			scores[j] = float64(rng.IntN(3)) / 2 // 0, 0.5, 1
		}
		f.Add(member(fmt.Sprintf("m%03d", i), scores, nil))
		members := f.Members()
		for a := range members {
			for b := range members {
				if a != b && Dominates(members[a].Scores, members[b].Scores) {
					t.Fatalf("step %d: %s dominates %s", i, members[a].ID(), members[b].ID())
				}
			}
		}
	}
}

// TestFrontierBestHardFilter covers the json_validator hard filter,
// the primary ordering with tie-breaks and the all-fail fallback.
func TestFrontierBestHardFilter(t *testing.T) {
	f := &Frontier{}
	// primary 0.9 with json mean 0.5, primary 0.7 with json 1.0,
	// primary 0.95 with json 0.2 — the filter must pick the 0.7 one.
	f.Add(member("x1", []float64{1, 1, 0, 0}, map[string]float64{"exact_match": 0.9, "json_validator": 0.5}))
	f.Add(member("x2", []float64{1, 0, 1, 1}, map[string]float64{"exact_match": 0.7, "json_validator": 1}))
	f.Add(member("x3", []float64{0, 1, 1, 1}, map[string]float64{"exact_match": 0.95, "json_validator": 0.2}))

	best, satisfied, note := f.Best("exact_match", "json_validator")
	if best.ID() != "x2" || !satisfied || note != "" {
		t.Fatalf("best = %s satisfied=%v note=%q, want x2/true/empty", best.ID(), satisfied, note)
	}

	// No constraint: pure primary ordering.
	best, satisfied, note = f.Best("exact_match", "")
	if best.ID() != "x3" || !satisfied || note != "" {
		t.Fatalf("unconstrained best = %s, want x3", best.ID())
	}

	// All-fail fallback: nobody reaches json mean 1.
	f2 := &Frontier{}
	f2.Add(member("y1", []float64{1, 0}, map[string]float64{"exact_match": 0.9, "json_validator": 0.25}))
	f2.Add(member("y2", []float64{0, 1}, map[string]float64{"exact_match": 0.8, "json_validator": 0.5}))
	best, satisfied, note = f2.Best("exact_match", "json_validator")
	if best.ID() != "y2" || satisfied || note == "" {
		t.Fatalf("fallback best = %s satisfied=%v note=%q, want y2/false/noted", best.ID(), satisfied, note)
	}

	// Tie on primary → constraint mean breaks it; tie on both → id asc.
	f3 := &Frontier{}
	f3.Add(member("bb", []float64{1, 0}, map[string]float64{"exact_match": 0.8, "json_validator": 1}))
	f3.Add(member("aa", []float64{0, 1}, map[string]float64{"exact_match": 0.8, "json_validator": 1}))
	if best, _, _ := f3.Best("exact_match", "json_validator"); best.ID() != "aa" {
		t.Fatalf("id tie-break best = %s, want aa", best.ID())
	}
	f4 := &Frontier{}
	f4.Add(member("lo", []float64{1, 0}, map[string]float64{"exact_match": 0.8, "json_validator": 0.3}))
	f4.Add(member("hi", []float64{0, 1}, map[string]float64{"exact_match": 0.8, "json_validator": 0.9}))
	if best, _, _ := f4.Best("exact_match", "json_validator"); best.ID() != "hi" {
		t.Fatalf("constraint tie-break best = %s, want hi", best.ID())
	}
}

// TestFrontierWinsAndComplement covers exclusive wins and the merge
// partner choice.
func TestFrontierWinsAndComplement(t *testing.T) {
	f := &Frontier{}
	parent := member("p", []float64{1, 0, 0, 0}, map[string]float64{"exact_match": 0.25})
	comp := member("c", []float64{0, 1, 1, 0}, map[string]float64{"exact_match": 0.5})
	other := member("o", []float64{0, 1, 0, 1}, map[string]float64{"exact_match": 0.5})
	f.Add(parent)
	f.Add(comp)
	f.Add(other)

	if got := f.Wins("p"); got != 1 {
		t.Errorf("Wins(p) = %d, want 1 (sample 1 exclusively)", got)
	}
	if got := f.Wins("c"); got != 1 {
		t.Errorf("Wins(c) = %d, want 1 (sample 3)", got)
	}
	if got := f.Wins("o"); got != 1 {
		t.Errorf("Wins(o) = %d, want 1 (sample 4)", got)
	}
	if got := f.Wins("missing"); got != 0 {
		t.Errorf("Wins(missing) = %d, want 0", got)
	}

	// Parent fails samples 2,3,4. c scores 2/3 there, o scores 1/3 →
	// c is the complement.
	if got := f.Complement(parent, "exact_match"); got.ID() != "c" {
		t.Errorf("Complement = %s, want c", got.ID())
	}
	// A parent failing nothing falls back to the best primary mean.
	strong := member("s", []float64{1, 1, 1, 0}, map[string]float64{"exact_match": 0.75})
	f.Add(strong) // evicts nobody (nobody dominates it, it dominates nobody)
	if got := f.Complement(strong, "exact_match"); got.ID() != "c" && got.ID() != "o" {
		t.Errorf("fallback complement = %s, want c or o (best primary mean 0.5)", got.ID())
	}
}

// TestFrontierUniformPickDeterminism: the same seed draws the same
// member sequence.
func TestFrontierUniformPickDeterminism(t *testing.T) {
	f := &Frontier{}
	for _, id := range []string{"a", "b", "c"} {
		f.Add(member(id, []float64{float64(id[0] - 'a'), 0}, nil))
	}
	draw := func() []string {
		rng := rand.New(rand.NewPCG(7, 7))
		out := make([]string, 10)
		for i := range out {
			out[i] = f.UniformPick(rng).ID()
		}
		return out
	}
	first, second := draw(), draw()
	if !slices.Equal(first, second) {
		t.Errorf("uniform picks diverged across same-seed runs: %v vs %v", first, second)
	}
}

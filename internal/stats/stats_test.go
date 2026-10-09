package stats

import (
	"testing"
)

// TestBootstrapVerdictConstants pins the wire strings both consumers
// record (verify.json's regression.ci.verdict and the pool's warning
// prefix) — renaming them would break artifact readers.
func TestBootstrapVerdictConstants(t *testing.T) {
	if CIRegressed != "regressed" || CIConfidentPass != "confident_pass" || CIInconclusive != "inconclusive" {
		t.Errorf("verdict constants = %s/%s/%s, want the historical wire strings",
			CIRegressed, CIConfidentPass, CIInconclusive)
	}
	if got := BootstrapVerdict(0.06, 0.4, 0.05); got != CIRegressed {
		t.Errorf("verdict(lo>thr) = %s", got)
	}
	if got := BootstrapVerdict(-0.4, 0, 0.05); got != CIConfidentPass {
		t.Errorf("verdict(hi==0) = %s", got)
	}
	if got := BootstrapVerdict(-0.1, 0.02, 0.05); got != CIInconclusive {
		t.Errorf("verdict(straddling) = %s", got)
	}
}

// TestPercentileEdges pins the interpolation: empty reads as 0, a
// single point reads as itself, and interior quantiles interpolate
// linearly between the neighboring order statistics.
func TestPercentileEdges(t *testing.T) {
	if got := Percentile(nil, 0.5); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}
	if got := Percentile([]float64{7}, 0.99); got != 7 {
		t.Errorf("percentile(single) = %v, want 7", got)
	}
	// q=0.25 of [0,10] interpolates to 2.5.
	if got := Percentile([]float64{0, 10}, 0.25); got != 2.5 {
		t.Errorf("percentile([0,10], 0.25) = %v, want 2.5", got)
	}
}

// TestNewBootstrapRNGReproducible pins the fixed-seed stream: two RNGs
// from the same constructor produce identical resampling sequences —
// the reproducibility stance both consumers rely on.
func TestNewBootstrapRNGReproducible(t *testing.T) {
	a, b := NewBootstrapRNG(), NewBootstrapRNG()
	for range 8 {
		if a.IntN(1000) != b.IntN(1000) {
			t.Fatal("same-seed streams diverged")
		}
	}
}

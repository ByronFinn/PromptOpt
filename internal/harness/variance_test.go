package harness

import "testing"

func TestClassify(t *testing.T) {
	th := DefaultThresholds()
	cases := []struct {
		name   string
		scores []float64
		th     Thresholds
		want   Verdict
	}{
		{"all right is dead easy", []float64{1, 1, 1}, th, VerdictDeadEasy},
		{"all wrong is dead hard", []float64{0, 0, 0}, th, VerdictDeadHard},
		{"mid band only is noisy", []float64{0.5, 0.4, 0.6}, th, VerdictNoisy},
		{"disagreement is kept", []float64{0, 1, 0}, th, VerdictKeep},
		{"one decisive high keeps", []float64{0.5, 1}, th, VerdictKeep},
		{"one decisive low keeps", []float64{0.5, 0}, th, VerdictKeep},
		{"empty is unmeasured", nil, th, VerdictUnmeasured},
		{"boundary low counts as dead", []float64{0.01, 0.005}, th, VerdictDeadHard},
		{"boundary high counts as dead", []float64{0.99, 1}, th, VerdictDeadEasy},
		{"K=1 all right", []float64{1}, th, VerdictDeadEasy},
		{"K=1 all wrong", []float64{0}, th, VerdictDeadHard},
		{"K=1 mid band is noisy", []float64{0.5}, th, VerdictNoisy},
		{"custom thresholds mid band", []float64{0.3, 0.4}, Thresholds{Low: 0.2, High: 0.8}, VerdictNoisy},
		{"custom thresholds keep", []float64{0.1, 0.4}, Thresholds{Low: 0.2, High: 0.8}, VerdictKeep},
	}
	for _, tc := range cases {
		if got := Classify(tc.scores, tc.th); got != tc.want {
			t.Errorf("%s: Classify(%v) = %q, want %q", tc.name, tc.scores, got, tc.want)
		}
	}
}

func TestClassifyZeroOneMetricsHaveNoMidBand(t *testing.T) {
	// exact_match / json_validator only ever produce 0 or 1, which sit
	// outside the default (0.01, 0.99) mid band — noisy is unreachable.
	th := DefaultThresholds()
	for _, scores := range [][]float64{{0}, {1}, {0, 1}, {1, 1, 0}} {
		if got := Classify(scores, th); got == VerdictNoisy {
			t.Errorf("Classify(%v) = noisy for a 0/1 metric", scores)
		}
	}
}

func TestVariance(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{0.7}, 0},
		{"uniform", []float64{0.5, 0.5, 0.5}, 0},
		{"zero one", []float64{0, 1}, 0.25},
		{"zero two", []float64{0, 2}, 1},
		{"known", []float64{1, 2, 3, 4}, 1.25},
	}
	for _, tc := range cases {
		if got := Variance(tc.xs); got != tc.want {
			t.Errorf("%s: Variance(%v) = %v, want %v", tc.name, tc.xs, got, tc.want)
		}
	}
}

func TestThresholdsValidate(t *testing.T) {
	if err := (Thresholds{Low: 0.01, High: 0.99}).Validate(); err != nil {
		t.Errorf("default thresholds rejected: %v", err)
	}
	for _, th := range []Thresholds{{Low: 0.9, High: 0.1}, {Low: -0.1, High: 0.9}, {Low: 0.1, High: 1.1}, {Low: 0.5, High: 0.5}} {
		if err := th.Validate(); err == nil {
			t.Errorf("thresholds %+v accepted", th)
		}
	}
}

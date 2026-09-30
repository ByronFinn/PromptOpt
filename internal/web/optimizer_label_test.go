package web

import "testing"

// TestOptimizerLabel pins the three resolution branches: registered
// name → registry label, unknown name → raw fallback, empty → empty.
func TestOptimizerLabel(t *testing.T) {
	if got := optimizerLabel("gepa"); got != "GEPA 反射进化" {
		t.Errorf("optimizerLabel(gepa) = %q, want the registry label", got)
	}
	if got := optimizerLabel("no-such-paradigm"); got != "no-such-paradigm" {
		t.Errorf("optimizerLabel(unknown) = %q, want the raw name", got)
	}
	if got := optimizerLabel(""); got != "" {
		t.Errorf("optimizerLabel(empty) = %q, want empty", got)
	}
}

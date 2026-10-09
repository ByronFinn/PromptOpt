package eval

import (
	"strings"
	"testing"
)

func TestRegisterMetricReservedAndDuplicates(t *testing.T) {
	// llm_judge is reserved: the engine dispatches it outside the
	// registry (judge provider call), so it must not be registerable.
	err := RegisterMetric(MetricLLMJudge, func(string, any) MetricResult { return MetricResult{} })
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("RegisterMetric(llm_judge) error = %v, want reserved-name rejection", err)
	}
	// Duplicate registration is rejected...
	err = RegisterMetric("f1", func(string, any) MetricResult { return MetricResult{} })
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("RegisterMetric(f1) error = %v, want duplicate rejection", err)
	}
	// ...and the builtin keeps scoring untouched after the rejection.
	if got := Evaluate("f1", "同 一 段", "同 一 段"); got.Score != 1 {
		t.Errorf("builtin f1 after rejected registration = %+v, want score 1", got)
	}
	// Degenerate arguments are rejected too.
	if err := RegisterMetric("", func(string, any) MetricResult { return MetricResult{} }); err == nil {
		t.Error(`RegisterMetric("") error = nil, want error`)
	}
	if err := RegisterMetric("nil_fn_probe", nil); err == nil {
		t.Error("RegisterMetric(nil fn) error = nil, want error")
	}
}

func TestRegisterMetricRoundTrip(t *testing.T) {
	const name = "test_probe_metric"
	if err := RegisterMetric(name, func(string, any) MetricResult {
		return MetricResult{Score: 0.42, Diagnosis: "probe"}
	}); err != nil {
		t.Fatalf("RegisterMetric(%s): %v", name, err)
	}
	// A second registration of the now-taken name must fail...
	if err := RegisterMetric(name, func(string, any) MetricResult { return MetricResult{} }); err == nil {
		t.Error("second RegisterMetric error = nil, want duplicate rejection")
	}
	// ...and Evaluate must dispatch to the registered function.
	if got := Evaluate(name, "output", "expected"); got.Score != 0.42 || got.Diagnosis != "probe" {
		t.Errorf("Evaluate(%s) = %+v, want the registered function's result", name, got)
	}
}

func TestEvaluateUnknownMetricDiagnosisUnchanged(t *testing.T) {
	// The historical diagnosis string, byte for byte.
	got := Evaluate("no_such_metric", "out", nil)
	if got.Score != 0 || got.Diagnosis != `unknown metric "no_such_metric"` {
		t.Errorf("Evaluate(unknown) = %+v, want zero score with historical diagnosis", got)
	}
	// llm_judge never enters the registry, so a direct Evaluate call
	// reads exactly like the pre-registry switch (the engine
	// special-cases the metric before reaching here).
	got = Evaluate(MetricLLMJudge, "out", nil)
	if got.Score != 0 || got.Diagnosis != `unknown metric "llm_judge"` {
		t.Errorf("Evaluate(llm_judge) = %+v, want the historical unknown-metric shape", got)
	}
}

func TestBuiltinMetricsRegisteredAndDispatched(t *testing.T) {
	// Wiring only — the golden behavior of the three builtins stays
	// pinned by TestJSONValidatorGoldenCases and friends in eval_test.go.
	cases := []struct {
		metric, output string
		expected       any
		want           float64
	}{
		{"json_validator", `{"a": 1}`, "ignored", 1},
		{"exact_match", " 风寒束表 ", "风寒束表", 1},
		{"f1", "风寒束表", "风寒束表", 1},
	}
	for _, tc := range cases {
		if got := Evaluate(tc.metric, tc.output, tc.expected); got.Score != tc.want {
			t.Errorf("Evaluate(%s) = %+v, want %v (diagnosis %q)", tc.metric, got, tc.want, got.Diagnosis)
		}
	}
}

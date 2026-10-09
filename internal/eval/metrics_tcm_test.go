package eval

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// tcmWant builds the reference side in the real examples/json_extraction
// schema shape (YAML decodes it into exactly this structure).
func tcmWant(pairs ...[2]string) map[string]any {
	entities := make([]any, 0, len(pairs))
	for _, p := range pairs {
		entities = append(entities, map[string]any{"type": p[0], "text": p[1]})
	}
	return map[string]any{"entities": entities}
}

// tcmOut renders the candidate output JSON in the same schema.
func tcmOut(pairs ...[2]string) string {
	var b strings.Builder
	b.WriteString(`{"entities": [`)
	for i, p := range pairs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"type": "` + p[0] + `", "text": "` + p[1] + `"}`)
	}
	b.WriteString("]}")
	return b.String()
}

func TestTCMDimensionF1RealSchemaFullMatch(t *testing.T) {
	// dataset.yaml train-002 shape: all four dimensions present, output
	// identical to the reference → every dimension 1, macro 1.
	want := tcmWant(
		[2]string{"证候", "脾胃虚寒"},
		[2]string{"治法", "温中健脾"},
		[2]string{"方剂", "理中汤"},
		[2]string{"中药", "人参"}, [2]string{"中药", "白术"},
		[2]string{"中药", "干姜"}, [2]string{"中药", "炙甘草"},
	)
	for _, metric := range []string{
		"tcm_f1_syndrome", "tcm_f1_treatment", "tcm_f1_formula", "tcm_f1_herb", "tcm_f1_entity",
	} {
		if got := Evaluate(metric, tcmOut(
			[2]string{"证候", "脾胃虚寒"},
			[2]string{"治法", "温中健脾"},
			[2]string{"方剂", "理中汤"},
			[2]string{"中药", "人参"}, [2]string{"中药", "白术"},
			[2]string{"中药", "干姜"}, [2]string{"中药", "炙甘草"},
		), want); got.Score != 1 {
			t.Errorf("%s = %+v, want 1", metric, got)
		}
	}
}

func TestTCMEntityMacroPartialAndOutOfScopeType(t *testing.T) {
	// One syndrome missing (F1 2/3), one out-of-scope type (症状) on
	// both sides — it must leak into no dimension — and both 中药
	// multisets empty (the two-empty convention scores 1).
	want := tcmWant(
		[2]string{"证候", "风寒束表"}, [2]string{"证候", "风热犯表"},
		[2]string{"治法", "辛温解表"},
		[2]string{"方剂", "银翘散"},
		[2]string{"症状", "汗出"},
	)
	output := tcmOut(
		[2]string{"证候", "风寒束表"},
		[2]string{"治法", "辛温解表"},
		[2]string{"方剂", "银翘散"},
		[2]string{"症状", "头痛"},
	)

	if got := Evaluate("tcm_f1_syndrome", output, want); math.Abs(got.Score-2.0/3.0) > 1e-9 {
		t.Errorf("tcm_f1_syndrome = %v, want 2/3 (diagnosis %q)", got.Score, got.Diagnosis)
	}
	for _, metric := range []string{"tcm_f1_treatment", "tcm_f1_formula", "tcm_f1_herb"} {
		if got := Evaluate(metric, output, want); got.Score != 1 {
			t.Errorf("%s = %+v, want 1 (empty dimension or full match)", metric, got)
		}
	}
	got := Evaluate("tcm_f1_entity", output, want)
	if math.Abs(got.Score-11.0/12.0) > 1e-9 {
		t.Errorf("tcm_f1_entity macro = %v, want 11/12 (diagnosis %q)", got.Score, got.Diagnosis)
	}
	// Macro diagnosis carries the per-dimension detail.
	for _, part := range []string{"证候=0.67", "治法=1.00", "方剂=1.00", "中药=1.00"} {
		if !strings.Contains(got.Diagnosis, part) {
			t.Errorf("macro diagnosis %q missing %q", got.Diagnosis, part)
		}
	}
}

func TestTCMHerbMultisetDuplicates(t *testing.T) {
	// Multiset semantics: two reference 人参 vs one predicted →
	// precision 1, recall 1/2, F1 2/3.
	want := tcmWant([2]string{"中药", "人参"}, [2]string{"中药", "人参"})
	got := Evaluate("tcm_f1_herb", tcmOut([2]string{"中药", "人参"}), want)
	if math.Abs(got.Score-2.0/3.0) > 1e-9 {
		t.Errorf("tcm_f1_herb = %v, want 2/3 (diagnosis %q)", got.Score, got.Diagnosis)
	}
}

func TestTCMFenceStrippedAndEmptyDimension(t *testing.T) {
	// Fenced output parses; a dimension absent on both sides scores 1.
	want := tcmWant([2]string{"证候", "风寒束表"})
	output := "```json\n" + tcmOut([2]string{"证候", "风寒束表"}) + "\n```"
	if got := Evaluate("tcm_f1_syndrome", output, want); got.Score != 1 {
		t.Errorf("fenced tcm_f1_syndrome = %+v, want 1", got)
	}
	if got := Evaluate("tcm_f1_herb", output, want); got.Score != 1 {
		t.Errorf("empty-on-both-sides tcm_f1_herb = %+v, want 1", got)
	}
	// Missing prediction drops the dimension to 0 (no overlap).
	if got := Evaluate("tcm_f1_syndrome", tcmOut(), want); got.Score != 0 || got.Diagnosis == "" {
		t.Errorf("empty output tcm_f1_syndrome = %+v, want 0 with diagnosis", got)
	}
}

func TestTCMMalformedSidesReportNotScore(t *testing.T) {
	want := tcmWant([2]string{"证候", "风寒束表"})
	cases := []struct {
		name, output string
		expected     any
		wantDiag     string
	}{
		{"non-JSON output", "无法抽取实体", want, "output is not valid JSON:"},
		{"JSON without entities", `{}`, want, "output does not carry an entities array"},
		{"entities not an array", `{"entities": "风寒束表"}`, want, "output does not carry an entities array"},
		{"expected without entities", tcmOut([2]string{"证候", "风寒束表"}), "风寒束表", "expected value does not carry an entities array"},
		{"expected map without entities", tcmOut([2]string{"证候", "风寒束表"}), map[string]any{"answer": "x"}, "expected value does not carry an entities array"},
	}
	for _, tc := range cases {
		for _, metric := range []string{"tcm_f1_syndrome", "tcm_f1_entity"} {
			got := Evaluate(metric, tc.output, tc.expected)
			if got.Score != 0 || !strings.HasPrefix(got.Diagnosis, tc.wantDiag) {
				t.Errorf("%s/%s = %+v, want 0 with diagnosis prefix %q", tc.name, metric, got, tc.wantDiag)
			}
		}
	}
	// Entities missing type or text cannot join a typed multiset: they
	// are skipped, so the herb dimension reads empty (score 1 against
	// an empty reference side) instead of inventing elements.
	got := Evaluate("tcm_f1_herb", `{"entities": [{"type": "中药"}, {"text": "石膏"}]}`, tcmWant())
	if got.Score != 1 {
		t.Errorf("malformed-entity tcm_f1_herb = %+v, want 1 (entries skipped)", got)
	}
}

func TestEngineTCMMetricScoresIntoMeans(t *testing.T) {
	// Engine integration: a task declaring tcm_f1_syndrome + the macro
	// runs through the httptest stub and lands per-dimension scores in
	// the run means, the sample trace and the sample_done event.
	samples := []core.Sample{{
		ID:    "tcm-001",
		Input: "恶寒发热，无汗。证属风寒束表，治以辛温解表。",
		Expected: tcmWant(
			[2]string{"证候", "风寒束表"},
			[2]string{"治法", "辛温解表"},
		),
		Split: "test",
	}}
	res, bodies, events, dir := runEngine(t, engineOpts{
		metrics: []string{"tcm_f1_syndrome", "tcm_f1_entity"},
		respond: func(int, string) (int, string) {
			return http.StatusOK, completionJSON(tcmOut([2]string{"证候", "风寒束表"}),
				core.Usage{PromptTokens: 5, CompletionTokens: 3})
		},
	}, samples)

	if res.ExitCode != 0 || res.Evaluated != 1 {
		t.Fatalf("result = %+v, want completed run", res)
	}
	if len(bodies) != 1 {
		t.Fatalf("llm calls = %d, want 1", len(bodies))
	}
	// Syndrome fully right; macro = (1 + 0 + 1 + 1) / 4 (treatment has
	// no overlap: predicted none vs required 辛温解表).
	if got := res.MetricMeans["tcm_f1_syndrome"]; got != 1 {
		t.Errorf("mean tcm_f1_syndrome = %v, want 1", got)
	}
	if got := res.MetricMeans["tcm_f1_entity"]; math.Abs(got-0.75) > 1e-9 {
		t.Errorf("mean tcm_f1_entity = %v, want 0.75", got)
	}

	b, err := os.ReadFile(filepath.Join(dir, "samples", "001-tcm-001.json"))
	if err != nil {
		t.Fatalf("read sample trace: %v", err)
	}
	var st core.SampleTrace
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.Scores["tcm_f1_syndrome"] != 1 || math.Abs(st.Scores["tcm_f1_entity"]-0.75) > 1e-9 {
		t.Errorf("sample trace scores = %+v", st.Scores)
	}
	if !strings.Contains(st.Diagnosis["tcm_f1_entity"], "治法=0.00") {
		t.Errorf("sample trace macro diagnosis = %q, want per-dimension detail", st.Diagnosis["tcm_f1_entity"])
	}

	sawScoredDone := false
	for _, ev := range events {
		if ev.Type == EventSampleDone && ev.Error == "" {
			sawScoredDone = true
			if ev.Scores["tcm_f1_syndrome"] != 1 || math.Abs(ev.Scores["tcm_f1_entity"]-0.75) > 1e-9 {
				t.Errorf("sample_done scores = %+v", ev.Scores)
			}
		}
	}
	if !sawScoredDone {
		t.Fatal("no scored sample_done event")
	}
}

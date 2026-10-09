// TCM dimension metrics: the pilot second consumer of the metric
// registry (roadmap V7 §2.3). The taxonomy below is the A5 check
// against the real schema of examples/json_extraction — expected and
// output both carry {"entities": [{"type": 证候|治法|方剂|中药, "text":
// 原文实体}]} — so every metric here parses both sides into entity
// pairs, filters them by type and scores a per-dimension multiset F1
// over the text values (multisetF1 reuse; a dimension absent on both
// sides scores 1, the two-empty-multisets convention).
package eval

import (
	"encoding/json"
	"fmt"
	"strings"
)

// tcmEntity is one {type, text} pair after both sides are normalized
// through JSON. Entities that are not objects with string type and
// text are skipped: they cannot participate in a typed multiset, and
// the json_validator metric (declared alongside in the pilot task) is
// the surface that flags contract-breaking output.
type tcmEntity struct {
	typ  string
	text string
}

// tcmDimensions is the four-way entity taxonomy of the pilot task:
// typ is the type value as written in the data, suffix names the
// metric (tcm_f1_<suffix>) and label appears in the macro diagnosis.
var tcmDimensions = []struct {
	typ    string
	suffix string
	label  string
}{
	{"证候", "syndrome", "证候"},
	{"治法", "treatment", "治法"},
	{"方剂", "formula", "方剂"},
	{"中药", "herb", "中药"},
}

// Builtin TCM metrics: one dimension each plus the macro average.
func init() {
	for _, d := range tcmDimensions {
		mustRegisterMetric("tcm_f1_"+d.suffix, tcmDimensionFunc(d.typ))
	}
	mustRegisterMetric("tcm_f1_entity", evalTCMEntity)
}

// tcmDimensionFunc scores one entity-type dimension: multiset F1 over
// the texts of that type on both sides. Out-of-scope types (anything
// outside the four-way taxonomy) take part in no dimension.
func tcmDimensionFunc(typ string) MetricFunc {
	return func(output string, expected any) MetricResult {
		wantEnts, diag := tcmExpectedEntities(expected)
		if diag != "" {
			return MetricResult{Diagnosis: diag}
		}
		gotEnts, diag := tcmOutputEntities(output)
		if diag != "" {
			return MetricResult{Diagnosis: diag}
		}
		return multisetF1(tcmTextsOf(gotEnts, typ), tcmTextsOf(wantEnts, typ))
	}
}

// evalTCMEntity is the macro average of the four dimension F1s. The
// diagnosis carries the per-dimension detail so an imperfect macro is
// attributable ("证候=1.00 治法=0.67 …") without re-reading traces.
func evalTCMEntity(output string, expected any) MetricResult {
	wantEnts, diag := tcmExpectedEntities(expected)
	if diag != "" {
		return MetricResult{Diagnosis: diag}
	}
	gotEnts, diag := tcmOutputEntities(output)
	if diag != "" {
		return MetricResult{Diagnosis: diag}
	}
	sum := 0.0
	parts := make([]string, 0, len(tcmDimensions))
	for _, d := range tcmDimensions {
		s := multisetF1(tcmTextsOf(gotEnts, d.typ), tcmTextsOf(wantEnts, d.typ)).Score
		sum += s
		parts = append(parts, fmt.Sprintf("%s=%.2f", d.label, s))
	}
	return MetricResult{
		Score:     sum / float64(len(tcmDimensions)),
		Diagnosis: "分维度 F1：" + strings.Join(parts, " "),
	}
}

// tcmExpectedEntities canonicalizes the YAML-decoded reference through
// JSON (the canonicalize reuse: key order and number types normalized
// exactly like the structured exact_match path) and parses it. A
// malformed reference must be reported, never scored.
func tcmExpectedEntities(expected any) ([]tcmEntity, string) {
	cv, err := canonicalize(expected)
	if err != nil {
		return nil, "expected value is not canonicalizable: " + err.Error()
	}
	ents, ok := tcmParseEntities(cv)
	if !ok {
		return nil, "expected value does not carry an entities array"
	}
	return ents, ""
}

// tcmOutputEntities parses the candidate output: fence-stripped first,
// then decoded — the same output contract as the builtin JSON metrics.
func tcmOutputEntities(output string) ([]tcmEntity, string) {
	var got any
	if err := json.Unmarshal([]byte(stripFence(output)), &got); err != nil {
		return nil, "output is not valid JSON: " + truncate(err.Error(), 120)
	}
	ents, ok := tcmParseEntities(got)
	if !ok {
		return nil, "output does not carry an entities array"
	}
	return ents, ""
}

// tcmParseEntities extracts the {type,text} pairs; ok is false unless
// v is an object carrying an entities array.
func tcmParseEntities(v any) (ents []tcmEntity, ok bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := obj["entities"].([]any)
	if !ok {
		return nil, false
	}
	ents = make([]tcmEntity, 0, len(raw))
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		text, _ := m["text"].(string)
		if typ == "" || text == "" {
			continue
		}
		ents = append(ents, tcmEntity{typ: typ, text: text})
	}
	return ents, true
}

// tcmTextsOf collects the texts of one entity type, preserving
// multiplicity (the multiset).
func tcmTextsOf(ents []tcmEntity, typ string) []string {
	var texts []string
	for _, e := range ents {
		if e.typ == typ {
			texts = append(texts, e.text)
		}
	}
	return texts
}

package core

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeFixture writes content to a temp YAML file and returns its path.
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

const validTaskYAML = `
name: tcm_ner
description: 中医医疗 NER 抽取
prompt_template: |
  抽取实体，输出 JSON：
  {"entities": [{"type": "...", "text": "..."}]}

  {input}
metrics: [json_validator, f1]
`

func TestLoadTaskValid(t *testing.T) {
	got, err := LoadTask(writeFixture(t, validTaskYAML))
	if err != nil {
		t.Fatalf("LoadTask() error = %v", err)
	}
	if got.Name != "tcm_ner" {
		t.Errorf("Name = %q, want %q", got.Name, "tcm_ner")
	}
	if want := []string{"json_validator", "f1"}; !slices.Equal(got.Metrics, want) {
		t.Errorf("Metrics = %v, want %v", got.Metrics, want)
	}
	// primary_metric defaults to the first declared metric.
	if got.Primary() != "json_validator" {
		t.Errorf("Primary() = %q, want %q", got.Primary(), "json_validator")
	}
	if !strings.Contains(got.PromptTemplate, InputPlaceholder) {
		t.Errorf("PromptTemplate lost the %s placeholder", InputPlaceholder)
	}
}

func TestLoadTaskPrimaryExplicit(t *testing.T) {
	path := writeFixture(t, validTaskYAML+"primary_metric: f1\n")
	got, err := LoadTask(path)
	if err != nil {
		t.Fatalf("LoadTask() error = %v", err)
	}
	if got.Primary() != "f1" {
		t.Errorf("Primary() = %q, want %q", got.Primary(), "f1")
	}
}

func TestLoadTaskRoundTrip(t *testing.T) {
	want := Task{
		Name:           "roundtrip",
		Description:    "desc",
		PromptTemplate: "answer: {input}",
		Metrics:        []string{"exact_match"},
	}
	data, err := yaml.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := LoadTask(writeFixture(t, string(data)))
	if err != nil {
		t.Fatalf("LoadTask() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch:\ngot  %#v\nwant %#v", got, want)
	}
}

func TestLoadTaskErrors(t *testing.T) {
	tests := map[string]string{
		"missing name": `
prompt_template: "x {input}"
metrics: [f1]
`,
		"missing prompt_template": `
name: x
metrics: [f1]
`,
		"missing input placeholder": `
name: x
prompt_template: "no placeholder"
metrics: [f1]
`,
		"no metrics": `
name: x
prompt_template: "{input}"
`,
		"unknown metric": `
name: x
prompt_template: "{input}"
metrics: [bleu]
`,
		"duplicate metrics": `
name: x
prompt_template: "{input}"
metrics: [f1, f1]
`,
		"primary not in metrics": `
name: x
prompt_template: "{input}"
metrics: [f1]
primary_metric: exact_match
`,
		"unknown field": `
name: x
prompt_template: "{input}"
metrics: [f1]
bogus: 1
`,
		"empty document":     "\n",
		"syntax error":       "name: [unclosed\n",
		"multiple documents": validTaskYAML + "---\nname: other\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadTask(writeFixture(t, content))
			if err == nil {
				t.Fatal("LoadTask() error = nil, want error")
			}
		})
	}
}

func TestLoadTaskValidationMarked(t *testing.T) {
	// Field violations carry ErrValidation; parse errors do not have to.
	_, err := LoadTask(writeFixture(t, "name: x\nprompt_template: nope\nmetrics: [f1]\n"))
	if !errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want it to match ErrValidation", err)
	}
}

func TestValidMetricsCoverRegistryTCMPilot(t *testing.T) {
	// The eval metric registry's shipped TCM set must pass task.yaml
	// validation (examples/json_extraction declares tcm_f1_entity); the
	// whitelist and the registry are maintained in sync (docs/plugins.md §8).
	for _, m := range []string{
		"tcm_f1_entity", "tcm_f1_syndrome", "tcm_f1_treatment", "tcm_f1_formula", "tcm_f1_herb",
	} {
		if !slices.Contains(ValidMetrics, m) {
			t.Errorf("ValidMetrics missing %q", m)
		}
	}
	if _, err := LoadTask(writeFixture(t,
		"name: x\nprompt_template: \"{input}\"\nmetrics: [json_validator, tcm_f1_entity]\nprimary_metric: tcm_f1_entity\n")); err != nil {
		t.Errorf("LoadTask with tcm_f1_entity: %v", err)
	}
}

func TestLoadTaskFileMissing(t *testing.T) {
	_, err := LoadTask(filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want it to match fs.ErrNotExist", err)
	}
}

func TestLoadCandidateValid(t *testing.T) {
	path := writeFixture(t, `
id: baseline
name: baseline
description: 初始候选
prompt: |
  抽取并输出 JSON：
  {"entities": []}

  {input}
`)
	got, err := LoadCandidate(path)
	if err != nil {
		t.Fatalf("LoadCandidate() error = %v", err)
	}
	if got.ID != "baseline" {
		t.Errorf("ID = %q, want %q", got.ID, "baseline")
	}
}

func TestLoadCandidateErrors(t *testing.T) {
	tests := map[string]string{
		"missing id":          "prompt: \"{input}\"\n",
		"missing prompt":      "id: x\n",
		"missing placeholder": "id: x\nprompt: nope\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadCandidate(writeFixture(t, content))
			if err == nil {
				t.Fatal("LoadCandidate() error = nil, want error")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error = %v, want it to match ErrValidation", err)
			}
		})
	}
}

func TestLoadCandidateEmptyDocument(t *testing.T) {
	// An empty document is a parse error, not a field validation error.
	_, err := LoadCandidate(writeFixture(t, "\n"))
	if err == nil {
		t.Fatal("LoadCandidate() error = nil, want error")
	}
	if errors.Is(err, ErrValidation) {
		t.Errorf("error = %v, want a parse error not ErrValidation", err)
	}
}

// validDatasetWith returns a one-sample dataset whose expected has the
// given YAML representation.
func validDatasetWith(expectedYAML string) string {
	return `
name: tcm
samples:
  - id: s1
    input: 患者发热恶寒
    expected: ` + expectedYAML + `
    split: train
`
}

func TestLoadDatasetExpectedForms(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want any
	}{
		"string": {"\"风寒束表\"", "风寒束表"},
		"array":  {"[风寒束表, 辛温解表]", []any{"风寒束表", "辛温解表"}},
		"object": {
			"{entities: [{type: 证候, text: 风寒束表}]}",
			map[string]any{
				"entities": []any{map[string]any{"type": "证候", "text": "风寒束表"}},
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := LoadDataset(writeFixture(t, validDatasetWith(tc.yaml)))
			if err != nil {
				t.Fatalf("LoadDataset() error = %v", err)
			}
			if len(got.Samples) != 1 {
				t.Fatalf("len(Samples) = %d, want 1", len(got.Samples))
			}
			if !reflect.DeepEqual(got.Samples[0].Expected, tc.want) {
				t.Errorf("Expected = %#v, want %#v", got.Samples[0].Expected, tc.want)
			}
		})
	}
}

func TestLoadDatasetErrors(t *testing.T) {
	tests := map[string]string{
		"missing name": `
samples:
  - {id: s1, input: x, expected: y, split: train}
`,
		"no samples": "name: tcm\n",
		"duplicate ids": `
name: tcm
samples:
  - {id: s1, input: x, expected: y, split: train}
  - {id: s1, input: z, expected: w, split: test}
`,
		"bad split": validDatasetWith("y") + `
  - {id: s2, input: x, expected: y, split: val}
`,
		"missing expected": `
name: tcm
samples:
  - {id: s1, input: x, split: train}
`,
		"scalar expected": validDatasetWith("42"),
		"missing input": `
name: tcm
samples:
  - {id: s1, expected: y, split: train}
`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadDataset(writeFixture(t, content))
			if err == nil {
				t.Fatal("LoadDataset() error = nil, want error")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error = %v, want it to match ErrValidation", err)
			}
		})
	}
}

func TestRenderPrompt(t *testing.T) {
	tests := []struct {
		name  string
		tmpl  string
		input string
		want  string
	}{
		{
			name:  "json braces survive",
			tmpl:  `输出 {"entities": []} 然后 {input} 完事`,
			input: "患者发热",
			want:  `输出 {"entities": []} 然后 患者发热 完事`,
		},
		{
			name:  "replaces every occurrence",
			tmpl:  "{input} 与 {input}",
			input: "文本",
			want:  "文本 与 文本",
		},
		{
			name:  "empty input",
			tmpl:  "前 {input} 后",
			input: "",
			want:  "前  后",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderPrompt(tc.tmpl, tc.input); got != tc.want {
				t.Errorf("RenderPrompt() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilterSplit(t *testing.T) {
	samples := []Sample{
		{ID: "a", Split: "train"},
		{ID: "b", Split: "dev"},
		{ID: "c", Split: "test"},
	}
	tests := []struct {
		name  string
		split string
		want  []string
	}{
		{name: "empty keeps all", split: "", want: []string{"a", "b", "c"}},
		{name: "train", split: "train", want: []string{"a"}},
		{name: "test", split: "test", want: []string{"c"}},
		{name: "no match", split: "val", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FilterSplit(samples, tc.split)
			ids := make([]string, len(got))
			for i, s := range got {
				ids[i] = s.ID
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("FilterSplit() ids = %v, want %v", ids, tc.want)
			}
		})
	}
	// The empty split must return the input slice unchanged.
	if got := FilterSplit(samples, ""); !slices.Equal(got, samples) {
		t.Errorf("FilterSplit(\"\") = %v, want the original slice", got)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{PromptTokens: 7, CompletionTokens: 5}
	if u.Total() != 12 {
		t.Errorf("Total() = %d, want 12", u.Total())
	}
}

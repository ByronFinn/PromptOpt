package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// InputPlaceholder marks where sample input is injected into prompts.
const InputPlaceholder = "{input}"

// ErrValidation marks task/candidate/dataset constraint violations;
// field errors are wrapped so errors.Is can match.
var ErrValidation = errors.New("invalid document")

// ValidMetrics lists the supported evaluation metrics.
var ValidMetrics = []string{"exact_match", "json_validator", "f1"}

// ValidSplits lists the supported dataset splits.
var ValidSplits = []string{"train", "dev", "test"}

// LoadTask reads and validates a task.yaml file.
func LoadTask(path string) (Task, error) {
	var t Task
	if err := decodeFile(path, &t); err != nil {
		return Task{}, err
	}
	if err := t.Validate(); err != nil {
		return Task{}, fmt.Errorf("task %s: %w", path, err)
	}
	return t, nil
}

// LoadCandidate reads and validates a candidate.yaml file.
func LoadCandidate(path string) (Candidate, error) {
	var c Candidate
	if err := decodeFile(path, &c); err != nil {
		return Candidate{}, err
	}
	if err := c.Validate(); err != nil {
		return Candidate{}, fmt.Errorf("candidate %s: %w", path, err)
	}
	return c, nil
}

// LoadDataset reads and validates a dataset.yaml file.
func LoadDataset(path string) (Dataset, error) {
	var d Dataset
	if err := decodeFile(path, &d); err != nil {
		return Dataset{}, err
	}
	if err := d.Validate(); err != nil {
		return Dataset{}, fmt.Errorf("dataset %s: %w", path, err)
	}
	return d, nil
}

// Validate reports every constraint violation of the task at once.
func (t Task) Validate() error {
	var errs []error
	if t.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	switch {
	case t.PromptTemplate == "":
		errs = append(errs, errors.New("prompt_template is required"))
	case !strings.Contains(t.PromptTemplate, InputPlaceholder):
		errs = append(errs, fmt.Errorf("prompt_template must contain %s", InputPlaceholder))
	}
	if len(t.Metrics) == 0 {
		errs = append(errs, errors.New("at least one metric is required"))
	} else {
		for _, m := range t.Metrics {
			if !slices.Contains(ValidMetrics, m) {
				errs = append(errs, fmt.Errorf("unknown metric %q (want one of %s)", m, strings.Join(ValidMetrics, ", ")))
			}
		}
		if hasDuplicates(t.Metrics) {
			errs = append(errs, errors.New("metrics must not contain duplicates"))
		}
		if t.PrimaryMetric != "" && !slices.Contains(t.Metrics, t.PrimaryMetric) {
			errs = append(errs, fmt.Errorf("primary_metric %q must be one of metrics", t.PrimaryMetric))
		}
	}
	return joinValidation(errs)
}

// Validate reports every constraint violation of the candidate at once.
func (c Candidate) Validate() error {
	var errs []error
	if c.ID == "" {
		errs = append(errs, errors.New("id is required"))
	}
	switch {
	case c.Prompt == "":
		errs = append(errs, errors.New("prompt is required"))
	case !strings.Contains(c.Prompt, InputPlaceholder):
		errs = append(errs, fmt.Errorf("prompt must contain %s", InputPlaceholder))
	}
	return joinValidation(errs)
}

// Validate reports every constraint violation of the dataset at once.
func (d Dataset) Validate() error {
	var errs []error
	if d.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if len(d.Samples) == 0 {
		errs = append(errs, errors.New("at least one sample is required"))
	}
	seen := make(map[string]struct{}, len(d.Samples))
	for _, s := range d.Samples {
		if err := s.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("sample: %w", err))
		}
		if _, dup := seen[s.ID]; dup {
			errs = append(errs, fmt.Errorf("duplicate sample id %q", s.ID))
		}
		seen[s.ID] = struct{}{}
	}
	return joinValidation(errs)
}

// Validate reports constraint violations of a single sample.
func (s Sample) Validate() error {
	var errs []error
	if s.ID == "" {
		errs = append(errs, errors.New("id is required"))
	}
	if s.Input == "" {
		errs = append(errs, errors.New("input is required"))
	}
	switch s.Expected.(type) {
	case string, []any, map[string]any:
	default:
		errs = append(errs, errors.New("expected is required and must be a string, array or object"))
	}
	if !slices.Contains(ValidSplits, s.Split) {
		errs = append(errs, fmt.Errorf("split %q must be one of %s", s.Split, strings.Join(ValidSplits, ", ")))
	}
	return joinValidation(errs)
}

// RenderPrompt injects sample input via plain string replacement.
// text/template is deliberately avoided: JSON prompts contain literal
// braces that the template engine would consume.
func RenderPrompt(tmpl, input string) string {
	return strings.ReplaceAll(tmpl, InputPlaceholder, input)
}

// FilterSplit returns the samples belonging to split. An empty split
// returns all samples unchanged.
func FilterSplit(samples []Sample, split string) []Sample {
	if split == "" {
		return samples
	}
	return slices.DeleteFunc(slices.Clone(samples), func(s Sample) bool {
		return s.Split != split
	})
}

// decodeFile reads a single YAML document into dst, rejecting unknown
// fields and multi-document files.
func decodeFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("parse %s: document is empty", path)
		}
		return fmt.Errorf("parse %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse %s: expected a single YAML document", path)
	}
	return nil
}

// joinValidation wraps the accumulated field errors with ErrValidation,
// or returns nil when errs is empty.
func joinValidation(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%w: %w", ErrValidation, errs[0])
	default:
		return fmt.Errorf("%w: %w", ErrValidation, errors.Join(errs...))
	}
}

// hasDuplicates reports whether xs contains a repeated element.
func hasDuplicates(xs []string) bool {
	seen := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			return true
		}
		seen[x] = struct{}{}
	}
	return false
}

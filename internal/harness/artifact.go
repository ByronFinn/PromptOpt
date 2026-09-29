package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Artifact file names inside synth/<run_id>/.
const (
	fileManifest   = "manifest.json"
	fileSpec       = "spec.json"
	fileSamples    = "samples.json"
	fileFilter     = "filter.json"
	fileCheckpoint = "checkpoint.json"
)

// Manifest snapshots the zero-config synthesis inputs of one run.
type Manifest struct {
	Prompt        string    `json:"prompt"`
	Model         string    `json:"model"`
	SynthSamples  int       `json:"synth_samples"`
	ProbeVariants int       `json:"probe_variants"`
	CreatedAt     time.Time `json:"created_at"`
}

// SpecFile is spec.json: the synthesized task plus the probe prompt
// variants the p¹ filter replays over every sample.
type SpecFile struct {
	Task   core.Task `json:"task"`
	Probes []string  `json:"probes"`
}

// SampleFile is samples.json. Anchors is deliberately always
// serialized (no omitempty): the slot is reserved for the ADR 0001
// real-anchor validation set. It stays empty in this milestone and
// never enters the optimization loop.
type SampleFile struct {
	Samples []core.Sample `json:"samples"`
	Anchors []core.Sample `json:"anchors"`
}

// FilterReport is filter.json: the p¹ verdict of every synthesized
// sample plus the aggregate outcome of the purification pass.
type FilterReport struct {
	Variants         int             `json:"variants"`
	Primary          string          `json:"primary"`
	Thresholds       Thresholds      `json:"thresholds"`
	PerSample        []SampleVerdict `json:"per_sample"`
	Kept             int             `json:"kept"`
	DroppedByVerdict map[Verdict]int `json:"dropped_by_verdict"`
}

// SampleVerdict is one sample's probe evidence and p¹ verdict.
type SampleVerdict struct {
	ID       string    `json:"id"`
	Scores   []float64 `json:"scores"`
	Variance float64   `json:"variance"`
	Verdict  Verdict   `json:"verdict"`
}

// CheckpointStatus is the state of the human gate.
type CheckpointStatus string

const (
	CheckpointPending  CheckpointStatus = "pending"
	CheckpointApproved CheckpointStatus = "approved"
)

// GateMode is how the checkpoint resolves: autopilot flips itself to
// approved immediately, interactive waits for the artifact to change.
type GateMode string

const (
	ModeAutopilot   GateMode = "autopilot"
	ModeInteractive GateMode = "interactive"
)

// CheckpointState is checkpoint.json — the single source of truth for
// the gate. The web approve button and a manual file edit converge on
// the same artifact.
type CheckpointState struct {
	Status    CheckpointStatus `json:"status"`
	Mode      GateMode         `json:"mode"`
	UpdatedAt time.Time        `json:"updated_at"`
}

// SaveJSON serializes v as indented JSON through a unique temp file
// and a rename, so concurrent readers never observe a partial
// artifact and concurrent writers never collide on one temp name. The
// parent directory is created on demand.
func SaveJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// LoadJSON reads one JSON artifact.
func LoadJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// SaveManifest / LoadManifest manage synth/<id>/manifest.json.
func SaveManifest(dir string, m Manifest) error { return SaveJSON(filepath.Join(dir, fileManifest), m) }

func LoadManifest(dir string) (Manifest, error) {
	var m Manifest
	err := LoadJSON(filepath.Join(dir, fileManifest), &m)
	return m, err
}

// SaveSpec / LoadSpec manage synth/<id>/spec.json.
func SaveSpec(dir string, s SpecFile) error { return SaveJSON(filepath.Join(dir, fileSpec), s) }

func LoadSpec(dir string) (SpecFile, error) {
	var s SpecFile
	err := LoadJSON(filepath.Join(dir, fileSpec), &s)
	return s, err
}

// SaveSamples / LoadSamples manage synth/<id>/samples.json. Both ends
// validate the sample set through the core dataset constraints (ids,
// inputs, expected kinds, splits, duplicates), so a hand edit during
// the checkpoint pause cannot smuggle in a malformed set. A nil
// Anchors slot is stored as an empty array — the artifact always
// carries the ADR 0001 slot explicitly.
func SaveSamples(dir string, f SampleFile) error {
	if f.Anchors == nil {
		f.Anchors = []core.Sample{}
	}
	if err := validateSamples(f.Samples); err != nil {
		return fmt.Errorf("samples: %w", err)
	}
	if err := validateSamples(f.Anchors); err != nil {
		return fmt.Errorf("anchors: %w", err)
	}
	return SaveJSON(filepath.Join(dir, fileSamples), f)
}

func LoadSamples(dir string) (SampleFile, error) {
	var f SampleFile
	if err := LoadJSON(filepath.Join(dir, fileSamples), &f); err != nil {
		return SampleFile{}, err
	}
	if err := validateSamples(f.Samples); err != nil {
		return SampleFile{}, fmt.Errorf("samples.json: %w", err)
	}
	if err := validateSamples(f.Anchors); err != nil {
		return SampleFile{}, fmt.Errorf("samples.json anchors: %w", err)
	}
	return f, nil
}

// validateSamples reuses core.Dataset.Validate (which requires a
// non-empty name) with a placeholder name; empty sets are valid here —
// the pipeline reports an empty kept set at gate time instead.
func validateSamples(samples []core.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	return core.Dataset{Name: "synth", Samples: samples}.Validate()
}

// SaveFilterReport / LoadFilterReport manage synth/<id>/filter.json.
func SaveFilterReport(dir string, r FilterReport) error {
	return SaveJSON(filepath.Join(dir, fileFilter), r)
}

func LoadFilterReport(dir string) (FilterReport, error) {
	var r FilterReport
	err := LoadJSON(filepath.Join(dir, fileFilter), &r)
	return r, err
}

// SaveCheckpoint / LoadCheckpoint manage synth/<id>/checkpoint.json.
func SaveCheckpoint(dir string, st CheckpointState) error {
	return SaveJSON(filepath.Join(dir, fileCheckpoint), st)
}

func LoadCheckpoint(dir string) (CheckpointState, error) {
	var st CheckpointState
	err := LoadJSON(filepath.Join(dir, fileCheckpoint), &st)
	return st, err
}

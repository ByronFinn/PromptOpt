// Package anchors implements the anchor library (V7 提案 §1.2, ADR 0001
// 治理机制化): a git-versionable file library of real samples per task
// key. Confirmed entries are the trusted gate material verify consumes
// through --anchor-lib; staging holds candidates (verify --promote
// reflow, manual imports without --confirmed) until a human promotes
// them — synthesized look-alike samples never gain "real" endorsement
// automatically. The library is write-only from the optimization loop's
// perspective: the only read side is verify's verification set, never
// the Loop.
//
// Layout (artifact files, not SQLite — PRD-0000 决议修订 2026-09-30):
//
//	<dir>/<task-key>/samples.yaml         confirmed library
//	<dir>/<task-key>/staging/samples.yaml candidate area
package anchors

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"gopkg.in/yaml.v3"
)

// DefaultDir is the library root, relative to the working directory.
const DefaultDir = "anchors"

// Source names the channel an entry entered the library through.
// verify-failure is the verify --promote reflow channel's name (提案
// §1.2 入口②: the samples that drove the regression verdict carry the
// most information and are deposited first) and covers the whole
// promoted verification set — the per-sample outcome rides the Verdict
// metadata, not the channel.
type Source string

const (
	SourceManual          Source = "manual"
	SourceVerifyFailure   Source = "verify-failure"
	SourceProductionTrace Source = "production-trace"
)

// Valid reports whether s is one of the three known channels.
func (s Source) Valid() bool {
	switch s {
	case SourceManual, SourceVerifyFailure, SourceProductionTrace:
		return true
	}
	return false
}

// Verdict labels a promoted sample's per-sample verification outcome:
// regressed (the delivered candidate scored below the baseline on that
// sample's primary metric) or pass. Manual imports carry no verdict.
const (
	VerdictRegressed = "regressed"
	VerdictPass      = "pass"
)

// Entry is one library record: the sample plus its provenance
// metadata. InputHash is the dedup key (core.HashInput of the sample
// input — the single shared normalization+hash, P5/P8 共用); Verdict is
// set only on the verify --promote reflow.
type Entry struct {
	Sample    core.Sample `json:"sample" yaml:"sample"`
	Source    Source      `json:"source" yaml:"source"`
	AddedAt   time.Time   `json:"added_at" yaml:"added_at"`
	OriginRun string      `json:"origin_run,omitempty" yaml:"origin_run,omitempty"`
	InputHash string      `json:"input_hash" yaml:"input_hash"`
	Verdict   string      `json:"verdict,omitempty" yaml:"verdict,omitempty"`
}

// libFile is one samples.yaml document.
type libFile struct {
	Entries []Entry `json:"entries" yaml:"entries"`
}

// ValidateTaskKey rejects task keys that cannot serve as a library
// directory name: empty, path separators, control characters, the dot
// names, or anything over 128 runes. CJK keys are allowed (the library
// is user-facing); ASCII keys stay the shell-friendly recommendation.
func ValidateTaskKey(key string) error {
	switch {
	case key == "":
		return errors.New("task key 不能为空（--task-key）")
	case key == "." || key == "..":
		return fmt.Errorf("task key %q 不能是相对路径点名", key)
	case len([]rune(key)) > 128:
		return fmt.Errorf("task key 超过 128 字符：%d", len([]rune(key)))
	}
	for _, r := range key {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return fmt.Errorf("task key %q 含非法字符（路径分隔符或控制字符）", key)
		}
	}
	return nil
}

// Store is the file library rooted at dir.
type Store struct {
	dir string
}

// NewStore returns the library rooted at dir (cwd-relative paths are
// resolved by the filesystem as usual).
func NewStore(dir string) Store { return Store{dir: dir} }

// ConfirmedPath returns the confirmed library file of one task key.
func (s Store) ConfirmedPath(taskKey string) string {
	return filepath.Join(s.dir, taskKey, "samples.yaml")
}

// StagingPath returns the staging candidates file of one task key.
func (s Store) StagingPath(taskKey string) string {
	return filepath.Join(s.dir, taskKey, "staging", "samples.yaml")
}

// HasConfirmed reports whether the task key has a confirmed library.
func (s Store) HasConfirmed(taskKey string) bool {
	_, err := os.Stat(s.ConfirmedPath(taskKey))
	return err == nil
}

// Add inserts entries into the target layer (confirmed or staging) and
// reports how many landed and how many were skipped as duplicates. An
// input_hash already present in EITHER layer of the task's library
// skips the entry — one input occupies one slot per task, so re-running
// verify --promote over an already-promoted anchor set is a no-op.
// Validation of every entry happens before anything is written, so a
// bad batch leaves the library untouched.
func (s Store) Add(taskKey string, confirmed bool, entries []Entry) (added, skipped int, err error) {
	if err := ValidateTaskKey(taskKey); err != nil {
		return 0, 0, err
	}
	existing, err := s.Load(taskKey, false)
	if err != nil {
		return 0, 0, err
	}
	seen := make(map[string]struct{}, len(existing)+len(entries))
	for _, e := range existing {
		seen[e.InputHash] = struct{}{}
	}
	fresh := make([]Entry, 0, len(entries))
	for i := range entries {
		e := entries[i]
		if err := e.validate(); err != nil {
			return 0, 0, err
		}
		if _, dup := seen[e.InputHash]; dup {
			skipped++
			continue
		}
		seen[e.InputHash] = struct{}{}
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return 0, skipped, nil
	}
	path := s.ConfirmedPath(taskKey)
	if !confirmed {
		path = s.StagingPath(taskKey)
	}
	layer, err := readLayer(path)
	if err != nil {
		return 0, 0, err
	}
	if err := saveLib(path, append(layer, fresh...)); err != nil {
		return 0, 0, err
	}
	return len(fresh), skipped, nil
}

// validate checks one entry: a structurally valid sample, a known
// source channel, a truthful input hash and a known verdict. Filling
// the zero AddedAt/InputHash in place is the constructor's convenience:
// callers may leave both unset.
func (e *Entry) validate() error {
	if err := e.Sample.Validate(); err != nil {
		return fmt.Errorf("anchor 样本 %q: %w", e.Sample.ID, err)
	}
	if !e.Source.Valid() {
		return fmt.Errorf("anchor 样本 %q: 未知来源 %q（manual|verify-failure|production-trace）", e.Sample.ID, e.Source)
	}
	if e.AddedAt.IsZero() {
		e.AddedAt = time.Now().UTC()
	}
	if h := core.HashInput(e.Sample.Input); e.InputHash == "" {
		e.InputHash = h
	} else if e.InputHash != h {
		return fmt.Errorf("anchor 样本 %q: input_hash 与样本内容不一致", e.Sample.ID)
	}
	switch e.Verdict {
	case "", VerdictRegressed, VerdictPass:
	default:
		return fmt.Errorf("anchor 样本 %q: 未知 verdict %q（regressed|pass）", e.Sample.ID, e.Verdict)
	}
	return nil
}

// Load reads a task's library. confirmedOnly restricts to the
// confirmed layer — the only layer verify --anchor-lib consumes; the
// staging area stays a candidate zone until anchor promote moves an
// entry over. A missing layer reads as empty (fresh libraries start
// nowhere); entries are re-validated on load so a broken hand edit
// fails loudly instead of poisoning the gate.
func (s Store) Load(taskKey string, confirmedOnly bool) ([]Entry, error) {
	if err := ValidateTaskKey(taskKey); err != nil {
		return nil, err
	}
	entries, err := readLayer(s.ConfirmedPath(taskKey))
	if err != nil {
		return nil, err
	}
	if !confirmedOnly {
		staging, err := readLayer(s.StagingPath(taskKey))
		if err != nil {
			return nil, err
		}
		entries = append(entries, staging...)
	}
	for i := range entries {
		e := entries[i]
		if err := e.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", taskKey, err)
		}
	}
	return entries, nil
}

// Promote moves staging entries into the confirmed library: all of
// them when ids is empty, otherwise only the named sample ids (a name
// that matches no staging entry is an error — a silent partial promote
// would read as success while the review intent failed). Provenance
// metadata rides along unchanged; copies already confirmed under the
// same input hash are dropped from staging without a second insert.
func (s Store) Promote(taskKey string, ids []string) (moved int, err error) {
	if err := ValidateTaskKey(taskKey); err != nil {
		return 0, err
	}
	staging, err := readLayer(s.StagingPath(taskKey))
	if err != nil {
		return 0, err
	}
	confirmed, err := readLayer(s.ConfirmedPath(taskKey))
	if err != nil {
		return 0, err
	}
	picked := make([]Entry, 0, len(staging))
	for _, e := range staging {
		if len(ids) == 0 || slices.Contains(ids, e.Sample.ID) {
			picked = append(picked, e)
		}
	}
	if len(ids) > 0 {
		var missing []string
		for _, id := range ids {
			if !slices.ContainsFunc(picked, func(e Entry) bool { return e.Sample.ID == id }) {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			return 0, fmt.Errorf("staging 中不存在待提升样本：%s", strings.Join(missing, ", "))
		}
	}
	if len(picked) == 0 {
		return 0, nil
	}
	seen := make(map[string]struct{}, len(confirmed)+len(picked))
	for _, e := range confirmed {
		seen[e.InputHash] = struct{}{}
	}
	next := slices.Clone(confirmed)
	for _, e := range picked {
		if _, dup := seen[e.InputHash]; dup {
			continue
		}
		seen[e.InputHash] = struct{}{}
		next = append(next, e)
		moved++
	}
	remaining := slices.DeleteFunc(slices.Clone(staging), func(e Entry) bool {
		return slices.ContainsFunc(picked, func(p Entry) bool { return p.InputHash == e.InputHash })
	})
	if err := saveLib(s.ConfirmedPath(taskKey), next); err != nil {
		return 0, err
	}
	if err := saveLib(s.StagingPath(taskKey), remaining); err != nil {
		return 0, err
	}
	return moved, nil
}

// readLayer decodes one library file: a missing file is an empty
// layer, a corrupt one an error (reading it as empty would silently
// bypass dedup and validation).
func readLayer(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f libFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return f.Entries, nil
}

// saveLib writes one library file atomically (temp file + rename, the
// harness.SaveJSON discipline) so concurrent readers never see a torn
// YAML document.
func saveLib(path string, entries []Entry) error {
	if entries == nil {
		entries = []Entry{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(libFile{Entries: entries}); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

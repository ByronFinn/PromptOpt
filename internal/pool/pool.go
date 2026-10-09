// Package pool implements the cross-run candidate pool (V7 提案 §3.3,
// V8 本体的最小版提入本期): a git-versionable, append-only artifact
// store of per-run best candidates keyed by the same task key as the
// anchor library, carrying the per-sample primary rows and input hashes
// that make cross-run pairing possible.
//
// 只做存储 + 对照预警（本期最小边界，写死在此）：
//
//   - 不做池准入去重——每个完成的 run 追加一条（Best 对照时同分取
//     更早的 run，语义确定）；重复内容不折叠。
//   - 不喂回优化循环——池的唯一读面是新 run 的对照预警，与锚点库
//     同一独立性边界：加载点永不进 Loop。
//   - 不做池 UI——无子命令、无看板页；pool.jsonl 可直接 git diff。
//
// Layout (artifact files, not SQLite — PRD-0000 决议修订 2026-09-30，
// 与锚点库同一口径):
//
//	<root>/<task-key>/pool.jsonl   one JSON object per line, append-only
//
// 退化预警直接复用 verify 的配对自助法（internal/stats，§1.1 C 层）：
// 新 run 交付候选 vs 池内历史最优，同集（样本 ID 序列一致【且】逐样本
// input_hash 一致）才跑配对 CI——双一致缺一不可，防合成集「ID 为确定
// 性序列 001..N 而内容不同」的伪配对；不同集降级为仅均值对照提示。
package pool

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/stats"
)

// DefaultMaxRegression is the tolerated primary-metric mean drop before
// a new run counts as regressed against the pool's historical best —
// the same default as verify --max-regression, so both consumers share
// one "退化超过噪声区间" caliber.
const DefaultMaxRegression = 0.05

// BootstrapResamples is the paired-bootstrap resample count of the pool
// comparison CI — the same default as verify --bootstrap (fixed-seed
// PCG stream from internal/stats, byte-reproducible from the same rows).
const BootstrapResamples = 1000

// Entry is one run's pool record. Rows is the per-sample primary row in
// the run's fixed sample order and InputHashes the per-sample
// core.HashInput of the normalized input — Means alone cannot pair, the
// rows make the paired bootstrap possible and the hashes make it honest.
// SampleIDs pins the ID sequence of the same-set criterion (same
// content under different keys still fails the conjunction — 宁缺勿错,
// the same stance as the anchor library's task keys). Adopted is
// reserved for the dashboard adoption flow (adopted.json) and stays
// false in this milestone — no writer exists yet.
type Entry struct {
	CandidateID string             `json:"candidate_id"`
	Prompt      string             `json:"prompt"`
	Means       map[string]float64 `json:"means"`
	Rows        []float64          `json:"rows"`
	InputHashes []string           `json:"input_hashes"`
	SampleIDs   []string           `json:"sample_ids"`
	RunID       string             `json:"run_id"`
	AddedAt     time.Time          `json:"added_at"`
	Adopted     bool               `json:"adopted,omitempty"`
}

// PoolStore is the append-only pool rooted at Root.
type PoolStore struct {
	Root string
}

// NewPoolStore returns the pool rooted at root (cwd-relative paths are
// resolved by the filesystem as usual).
func NewPoolStore(root string) PoolStore { return PoolStore{Root: root} }

// Path returns one task key's pool file.
func (s PoolStore) Path(taskKey string) string {
	return filepath.Join(s.Root, taskKey, "pool.jsonl")
}

// Append validates entry and appends it as one JSON line. The pool is
// append-only: no dedup, no rewrite — every completed run lands one
// record. A zero AddedAt is filled with the wall clock.
func (s PoolStore) Append(taskKey string, entry Entry) error {
	if err := anchors.ValidateTaskKey(taskKey); err != nil {
		return err
	}
	if entry.CandidateID == "" {
		return errors.New("候选池条目缺 candidate_id")
	}
	if entry.RunID == "" {
		return errors.New("候选池条目缺 run_id")
	}
	if len(entry.Rows) == 0 || len(entry.Rows) != len(entry.InputHashes) || len(entry.Rows) != len(entry.SampleIDs) {
		return fmt.Errorf("候选池条目 %q 的逐样本行不等长（rows=%d input_hashes=%d sample_ids=%d）",
			entry.CandidateID, len(entry.Rows), len(entry.InputHashes), len(entry.SampleIDs))
	}
	if entry.AddedAt.IsZero() {
		entry.AddedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(filepath.Dir(s.Path(taskKey)), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path(taskKey), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Entries reads a task's pool in append order. A missing file reads as
// an empty pool (fresh keys start nowhere); a corrupt line is an error
// — reading past it would silently drop runs from the comparison.
func (s PoolStore) Entries(taskKey string) ([]Entry, error) {
	if err := anchors.ValidateTaskKey(taskKey); err != nil {
		return nil, err
	}
	f, err := os.Open(s.Path(taskKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var e Entry
		// A JSONL file is a stream of concatenated JSON objects — the
		// decoder walks it without line-length limits; io.EOF is the
		// clean end, anything else a corrupt record.
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("解析 %s: %w", s.Path(taskKey), err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Best returns the entry with the highest primary mean — the historical
// best a new run compares against. Entries without the primary metric
// are skipped; equal means keep the earlier run (deterministic scan,
// strict >). A missing or metric-less pool reads as no best.
func (s PoolStore) Best(taskKey, primary string) (Entry, bool, error) {
	entries, err := s.Entries(taskKey)
	if err != nil {
		return Entry{}, false, err
	}
	best := Entry{}
	found := false
	for _, e := range entries {
		mean, ok := e.Means[primary]
		if !ok {
			continue
		}
		if !found || mean > best.Means[primary] {
			best, found = e, true
		}
	}
	return best, found, nil
}

// Comparison is the outcome of comparing a new run against a pool
// entry. SameSet gates the paired bootstrap: only a double-consistent
// sample set (ID sequence AND per-sample input hash) runs the CI;
// Verdict carries a stats.CI* value only then. NewMean/OldMean are the
// primary means either way — the mean-only hint's material.
type Comparison struct {
	SameSet bool
	Verdict string  // stats.CIRegressed | stats.CIConfidentPass | stats.CIInconclusive
	Lo, Hi  float64 // CI bounds of the paired mean difference
	NewMean float64
	OldMean float64
}

// Compare pairs newEntry against oldEntry (the pool's historical best)
// on the primary metric. Same set → paired-bootstrap CI over the
// per-sample rows with the fixed-seed stream (verify 同一统计口径);
// otherwise the comparison degrades to means only.
func Compare(newEntry, oldEntry Entry, primary string) Comparison {
	cmp := Comparison{
		NewMean: newEntry.Means[primary],
		OldMean: oldEntry.Means[primary],
	}
	if !sameSet(newEntry, oldEntry) {
		return cmp
	}
	cmp.SameSet = true
	cmp.Lo, cmp.Hi = stats.PairedBootstrapCI(oldEntry.Rows, newEntry.Rows, BootstrapResamples, stats.NewBootstrapRNG())
	cmp.Verdict = stats.BootstrapVerdict(cmp.Lo, cmp.Hi, DefaultMaxRegression)
	return cmp
}

// sameSet is the pairing precondition: equal-length sample ID sequences
// element-wise equal AND equal-length input-hash sequences element-wise
// equal. The conjunction is the pseudo-pairing defense — a synthetic
// set regenerating deterministic IDs (001..N) over different content
// fails on the hash half; a re-keyed dataset fails on the ID half.
func sameSet(a, b Entry) bool {
	if len(a.SampleIDs) == 0 || len(a.SampleIDs) != len(b.SampleIDs) {
		return false
	}
	for i, id := range a.SampleIDs {
		if id != b.SampleIDs[i] {
			return false
		}
	}
	if len(a.InputHashes) != len(b.InputHashes) {
		return false
	}
	for i, h := range a.InputHashes {
		if h != b.InputHashes[i] {
			return false
		}
	}
	return len(a.Rows) == len(b.Rows)
}

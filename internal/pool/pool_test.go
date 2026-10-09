package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/stats"
)

// poolEntry builds an Entry with the required length-consistent
// per-sample columns.
func poolEntry(candID string, rows []float64, hashes, ids []string, mean float64) Entry {
	return Entry{
		CandidateID: candID, Prompt: "prompt of " + candID,
		Means: map[string]float64{"exact_match": mean},
		Rows:  rows, InputHashes: hashes, SampleIDs: ids,
		RunID: "run-" + candID,
	}
}

// TestPoolAppendEntriesRoundtripAndBest（用例 ①）pins the JSONL store:
// append → read roundtrip preserves every column (Rows/InputHashes/
// SampleIDs/Means/AddedAt included), Best picks the highest primary
// mean (ties keep the earlier run), and task keys are isolated.
func TestPoolAppendEntriesRoundtripAndBest(t *testing.T) {
	store := NewPoolStore(t.TempDir())
	first := poolEntry("c1", []float64{1, 0.5}, []string{"h1", "h2"}, []string{"s1", "s2"}, 0.75)
	second := poolEntry("c2", []float64{0.9, 0.9}, []string{"h1", "h2"}, []string{"s1", "s2"}, 0.9)
	for _, e := range []Entry{first, second} {
		if err := store.Append("task-a", e); err != nil {
			t.Fatalf("append %s: %v", e.CandidateID, err)
		}
	}

	got, err := store.Entries("task-a")
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2", len(got))
	}
	for i, want := range []Entry{first, second} {
		have := got[i]
		if have.CandidateID != want.CandidateID || have.Prompt != want.Prompt || have.RunID != want.RunID {
			t.Errorf("entry %d identity = %+v", i, have)
		}
		if have.Means["exact_match"] != want.Means["exact_match"] {
			t.Errorf("entry %d means = %v, want %v", i, have.Means, want.Means)
		}
		if len(have.Rows) != len(want.Rows) || len(have.InputHashes) != len(want.InputHashes) ||
			len(have.SampleIDs) != len(want.SampleIDs) {
			t.Fatalf("entry %d column lengths = %d/%d/%d", i, len(have.Rows), len(have.InputHashes), len(have.SampleIDs))
		}
		for j := range want.Rows {
			if have.Rows[j] != want.Rows[j] || have.InputHashes[j] != want.InputHashes[j] || have.SampleIDs[j] != want.SampleIDs[j] {
				t.Errorf("entry %d column %d = %v/%s/%s", i, j, have.Rows[j], have.InputHashes[j], have.SampleIDs[j])
			}
		}
		if have.AddedAt.IsZero() || !have.AddedAt.After(time.Now().Add(-time.Hour)) {
			t.Errorf("entry %d added_at = %v, want a filled wall-clock stamp", i, have.AddedAt)
		}
		if have.Adopted {
			t.Errorf("entry %d adopted = true, want the zero default (no adoption writer exists)", i)
		}
	}

	// Best: 0.9 beats 0.75; an equal-mean later entry keeps the earlier.
	if err := store.Append("task-a", poolEntry("c3", []float64{1}, []string{"h3"}, []string{"s3"}, 0.9)); err != nil {
		t.Fatalf("append tie: %v", err)
	}
	best, ok, err := store.Best("task-a", "exact_match")
	if err != nil || !ok {
		t.Fatalf("best = %v/%v, want c2", ok, err)
	}
	if best.CandidateID != "c2" {
		t.Errorf("best = %s, want c2 (0.9; ties keep the earlier run)", best.CandidateID)
	}

	// Cross task-key isolation: task-b shares nothing with task-a.
	if entries, err := store.Entries("task-b"); err != nil || len(entries) != 0 {
		t.Errorf("entries(task-b) = %d/%v, want an empty pool", len(entries), err)
	}
	if _, ok, err := store.Best("task-b", "exact_match"); ok || err != nil {
		t.Errorf("best(task-b) = %v/%v, want no best", ok, err)
	}
	if _, err := os.Stat(store.Path("task-b")); !os.IsNotExist(err) {
		t.Errorf("task-b pool file = %v, want not created by reads", err)
	}

	// Entries lacking the primary metric are skipped by Best.
	if err := store.Append("task-a", Entry{
		CandidateID: "c4", RunID: "run-c4", Means: map[string]float64{"f1": 1},
		Rows: []float64{1}, InputHashes: []string{"h4"}, SampleIDs: []string{"s4"},
	}); err != nil {
		t.Fatalf("append c4: %v", err)
	}
	if best, _, _ := store.Best("task-a", "exact_match"); best.CandidateID != "c2" {
		t.Errorf("best after metric-less entry = %s, want c2", best.CandidateID)
	}
}

// TestPoolCompareSameSetCriterion（用例 ②）pins the pseudo-pairing
// defense: same ID sequence over different content fails on the input
// hashes, a re-keyed same-content dataset fails on the IDs — only the
// double-consistent set reaches the paired bootstrap.
func TestPoolCompareSameSetCriterion(t *testing.T) {
	old := poolEntry("old", []float64{1, 1}, []string{"h1", "h2"}, []string{"s1", "s2"}, 1)

	// 合成集伪配对防御：ID 序列一致（001..N 式），内容（hash）不同 →
	// 不跑 CI，仅均值对照。
	fake := poolEntry("new", []float64{0, 0}, []string{"h1", "h3"}, []string{"s1", "s2"}, 0)
	cmp := Compare(fake, old, "exact_match")
	if cmp.SameSet || cmp.Verdict != "" {
		t.Errorf("same IDs over different content: %+v, want no CI", cmp)
	}
	if cmp.NewMean != 0 || cmp.OldMean != 1 {
		t.Errorf("mean-only comparison = %v/%v, want 0/1", cmp.NewMean, cmp.OldMean)
	}

	// 同内容换 ID（重新编号的数据集）→ ID 半边失败，同样降级。
	rekeyed := poolEntry("new", []float64{0, 0}, []string{"h1", "h2"}, []string{"x1", "x2"}, 0)
	if cmp := Compare(rekeyed, old, "exact_match"); cmp.SameSet {
		t.Errorf("re-keyed IDs: %+v, want the mean-only degradation", cmp)
	}

	// 空 ID 列不配对。
	idless := old
	idless.SampleIDs = nil
	if cmp := Compare(idless, old, "exact_match"); cmp.SameSet {
		t.Error("empty ID column must not pair")
	}

	// ID+hash 双一致 → CI 分支触发（此处行全同 → CI [0,0] 置信通过）。
	twin := poolEntry("new", []float64{1, 1}, []string{"h1", "h2"}, []string{"s1", "s2"}, 1)
	cmp = Compare(twin, old, "exact_match")
	if !cmp.SameSet {
		t.Fatalf("double-consistent set did not pair: %+v", cmp)
	}
	if cmp.Verdict != stats.CIConfidentPass || cmp.Lo != 0 || cmp.Hi != 0 {
		t.Errorf("identical rows: verdict=%s ci=[%v,%v], want confident_pass [0,0]", cmp.Verdict, cmp.Lo, cmp.Hi)
	}
}

// TestPoolCompareRegressionWarning（用例 ④）constructs synthetic rows
// for each CI branch: uniform degradation → regressed (the warning
// trigger), uniform improvement → confident pass, mixed rows →
// inconclusive — all on the fixed-seed stream, so the branching is
// deterministic.
func TestPoolCompareRegressionWarning(t *testing.T) {
	old := poolEntry("old", []float64{1, 1, 1}, []string{"h1", "h2", "h3"}, []string{"s1", "s2", "s3"}, 1)

	degraded := poolEntry("new", []float64{0, 0, 0}, []string{"h1", "h2", "h3"}, []string{"s1", "s2", "s3"}, 0)
	if cmp := Compare(degraded, old, "exact_match"); cmp.Verdict != stats.CIRegressed || cmp.Lo <= DefaultMaxRegression {
		t.Errorf("uniform degradation: %+v, want CIRegressed with lo > %v", cmp, DefaultMaxRegression)
	}

	improved := poolEntry("new", []float64{1, 1, 1}, []string{"h1", "h2", "h3"}, []string{"s1", "s2", "s3"}, 1)
	if cmp := Compare(improved, old, "exact_match"); cmp.Verdict != stats.CIConfidentPass {
		t.Errorf("equal rows after same-mean append: %+v, want confident pass", cmp)
	}

	mixed := poolEntry("new", []float64{0, 1, 0}, []string{"h1", "h2", "h3"}, []string{"s1", "s2", "s3"}, 1.0/3.0)
	if cmp := Compare(mixed, old, "exact_match"); cmp.Verdict != stats.CIInconclusive {
		t.Errorf("mixed rows: %+v, want inconclusive", cmp)
	}

	// Deterministic: the same rows produce byte-identical bounds.
	a := Compare(degraded, old, "exact_match")
	b := Compare(degraded, old, "exact_match")
	if a.Lo != b.Lo || a.Hi != b.Hi {
		t.Errorf("CI diverged across same-seed calls: [%v,%v] vs [%v,%v]", a.Lo, a.Hi, b.Lo, b.Hi)
	}
}

// TestPoolAppendValidationAndCorruptFile pins the store's hygiene:
// length-consistent columns are mandatory, task keys are validated,
// a corrupt pool file errors instead of reading as empty, and a
// missing file reads as an empty pool.
func TestPoolAppendValidationAndCorruptFile(t *testing.T) {
	store := NewPoolStore(t.TempDir())

	if err := store.Append("k", poolEntry("c", []float64{1}, []string{"h1", "h2"}, []string{"s1"}, 1)); err == nil {
		t.Error("unequal column lengths must be rejected")
	}
	bad := poolEntry("c", []float64{1}, []string{"h1"}, []string{"s1"}, 1)
	bad.CandidateID = ""
	if err := store.Append("k", bad); err == nil {
		t.Error("missing candidate id must be rejected")
	}
	bad = poolEntry("c", []float64{1}, []string{"h1"}, []string{"s1"}, 1)
	bad.RunID = ""
	if err := store.Append("k", bad); err == nil {
		t.Error("missing run id must be rejected")
	}
	if err := store.Append("../evil", poolEntry("c", []float64{1}, []string{"h1"}, []string{"s1"}, 1)); err == nil {
		t.Error("path-traversal task key must be rejected")
	}
	// Nothing landed despite the errors.
	if entries, err := store.Entries("k"); err != nil || len(entries) != 0 {
		t.Errorf("entries after rejected appends = %d/%v, want 0/nil", len(entries), err)
	}

	// Zero AddedAt is filled by Append.
	filled := poolEntry("c", []float64{1}, []string{"h1"}, []string{"s1"}, 1)
	filled.AddedAt = time.Time{}
	if err := store.Append("k", filled); err != nil {
		t.Fatalf("append: %v", err)
	}
	entries, _ := store.Entries("k")
	if entries[0].AddedAt.IsZero() {
		t.Error("Append must fill a zero AddedAt")
	}

	// A corrupt pool file is an error on read (both Entries and Best) —
	// reading past it would silently drop runs from the comparison.
	corrupt := NewPoolStore(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(corrupt.Path("k")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt.Path("k"), []byte("{\"candidate_id\":\"c\"\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.Entries("k"); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Errorf("entries on corrupt file = %v, want a parse error", err)
	}
	if _, _, err := corrupt.Best("k", "exact_match"); err == nil {
		t.Error("best on corrupt file must error, not silently skip")
	}
}

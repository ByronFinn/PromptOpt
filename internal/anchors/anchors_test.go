package anchors

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"gopkg.in/yaml.v3"
)

// entry builds a valid library entry; inputHash left empty lets
// validate fill it, mirroring the CLI callers.
func entry(id, input string, source Source) Entry {
	return Entry{
		Sample: core.Sample{ID: id, Input: input, Expected: "风寒束表", Split: "test"},
		Source: source,
	}
}

// TestInputHashNormalization pins the dedup key: whitespace layout
// differences collapse to one hash, different text never does. The
// function lives in core (P5/P8 共用唯一实现) — these assertions pin the
// anchors-side contract.
func TestInputHashNormalization(t *testing.T) {
	same := []string{
		"恶寒发热， 无汗，脉浮紧。",
		"  恶寒发热， 无汗，脉浮紧。 ",
		"恶寒发热，\t 无汗，脉浮紧。\n",
		"恶寒发热，\n无汗，脉浮紧。",
	}
	want := core.HashInput(same[0])
	if len(want) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(want))
	}
	for _, s := range same[1:] {
		if got := core.HashInput(s); got != want {
			t.Errorf("HashInput(%q) = %s, want %s", s, got, want)
		}
	}
	if got := core.HashInput("心烦不寐，腰膝酸软"); got == want {
		t.Error("different inputs must not collide")
	}
}

// TestStoredInputHashMatchesCoreHashInput is the cross-package
// consistency baseline (复审补，与 P8 联合): every input_hash persisted by
// this library must be byte-for-byte core.HashInput of the same input —
// a drifted parallel hash implementation would systematically break
// cross-run library comparison.
func TestStoredInputHashMatchesCoreHashInput(t *testing.T) {
	store := NewStore(t.TempDir())
	inputs := []string{
		"恶寒发热，无汗，脉浮紧。",
		"  心烦不寐，\n腰膝酸软，脉细数。 ",
	}
	entries := make([]Entry, 0, len(inputs))
	for i, in := range inputs {
		entries = append(entries, Entry{
			Sample: core.Sample{ID: fmt.Sprintf("h-%d", i), Input: in, Expected: "x", Split: "test"},
			Source: SourceManual,
		})
	}
	if _, _, err := store.Add("k", true, entries); err != nil {
		t.Fatal(err)
	}
	// Read the persisted YAML back as raw bytes and decode it: the
	// comparison target is what the file actually contains.
	b, err := os.ReadFile(store.ConfirmedPath("k"))
	if err != nil {
		t.Fatal(err)
	}
	var f libFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != len(inputs) {
		t.Fatalf("stored entries = %d, want %d", len(f.Entries), len(inputs))
	}
	for i, e := range f.Entries {
		if want := core.HashInput(inputs[i]); e.InputHash != want {
			t.Errorf("stored input_hash = %q, want core.HashInput = %q (byte-for-byte)", e.InputHash, want)
		}
		if got := core.HashInput(e.Sample.Input); e.InputHash != got {
			t.Errorf("stored input_hash %q != hash of stored input %q", e.InputHash, got)
		}
	}
}

// TestStoreAddDedupAndRoundtrip covers the core流转: add → dedup by
// input hash (union across layers) → YAML roundtrip preserving sample
// and provenance.
func TestStoreAddDedupAndRoundtrip(t *testing.T) {
	store := NewStore(t.TempDir())
	addedAt := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	first := Entry{
		Sample: core.Sample{
			ID: "an-1", Input: "恶寒发热，无汗，脉浮紧。",
			Expected: map[string]any{"syndrome": "风寒束表"}, Split: "test",
		},
		Source: SourceManual, AddedAt: addedAt, OriginRun: "20261001-ab12",
	}
	added, skipped, err := store.Add("tcm", true, []Entry{first})
	if err != nil || added != 1 || skipped != 0 {
		t.Fatalf("first add = %d/%d, %v", added, skipped, err)
	}
	if !store.HasConfirmed("tcm") {
		t.Error("confirmed library file missing after --confirmed add")
	}

	// Same input, different id → skipped (hash dedup, not id dedup).
	added, skipped, err = store.Add("tcm", true, []Entry{entry("an-1-dup", "恶寒发热，无汗，脉浮紧。", SourceManual)})
	if err != nil || added != 0 || skipped != 1 {
		t.Fatalf("duplicate add = %d/%d, %v", added, skipped, err)
	}
	// Same input into staging is also a duplicate: one input, one slot.
	added, skipped, err = store.Add("tcm", false, []Entry{entry("an-1-staging", "恶寒发热，无汗，脉浮紧。", SourceVerifyFailure)})
	if err != nil || added != 0 || skipped != 1 {
		t.Fatalf("cross-layer duplicate add = %d/%d, %v", added, skipped, err)
	}

	// A genuinely new input lands in staging.
	second := Entry{
		Sample: core.Sample{ID: "an-2", Input: "心烦不寐，腰膝酸软，脉细数。", Expected: "心肾不交", Split: "test"},
		Source: SourceVerifyFailure, AddedAt: addedAt,
		OriginRun: "20261002-cd34", Verdict: VerdictRegressed,
	}
	if added, skipped, err = store.Add("tcm", false, []Entry{second}); err != nil || added != 1 {
		t.Fatalf("staging add = %d/%d, %v", added, skipped, err)
	}

	// confirmedOnly keeps staging out of the verification set.
	confirmed, err := store.Load("tcm", true)
	if err != nil || len(confirmed) != 1 {
		t.Fatalf("confirmed-only load = %d entries, %v", len(confirmed), err)
	}
	all, err := store.Load("tcm", false)
	if err != nil || len(all) != 2 {
		t.Fatalf("full load = %d entries, %v", len(all), err)
	}

	// YAML roundtrip: sample fields (incl. object expected) and
	// provenance survive the file.
	got := all[0]
	if got.Sample.ID != "an-1" || got.Sample.Input != first.Sample.Input ||
		got.Sample.Split != "test" ||
		!reflect.DeepEqual(got.Sample.Expected, first.Sample.Expected) {
		t.Errorf("roundtrip sample = %+v, want %+v", got.Sample, first.Sample)
	}
	if got.Source != SourceManual || !got.AddedAt.Equal(addedAt) || got.OriginRun != "20261001-ab12" {
		t.Errorf("roundtrip metadata = %+v", got)
	}
	if got.InputHash == "" || got.InputHash != core.HashInput(first.Sample.Input) {
		t.Errorf("roundtrip input_hash = %q", got.InputHash)
	}
	if staging := all[1]; staging.Verdict != VerdictRegressed || staging.Source != SourceVerifyFailure {
		t.Errorf("staging entry = %+v", staging)
	}
}

// TestStorePromote covers staging→confirmed: full promote, subset by
// ids, unknown ids erroring, and provenance riding along.
func TestStorePromote(t *testing.T) {
	store := NewStore(t.TempDir())
	a := entry("s-a", "样本甲", SourceVerifyFailure)
	a.Verdict = VerdictRegressed
	b := entry("s-b", "样本乙", SourceVerifyFailure)
	b.Verdict = VerdictPass
	if _, _, err := store.Add("k", false, []Entry{a, b}); err != nil {
		t.Fatal(err)
	}

	// Subset promote by id.
	moved, err := store.Promote("k", []string{"s-a"})
	if err != nil || moved != 1 {
		t.Fatalf("subset promote = %d, %v", moved, err)
	}
	confirmed, _ := store.Load("k", true)
	if len(confirmed) != 1 || confirmed[0].Sample.ID != "s-a" || confirmed[0].Verdict != VerdictRegressed {
		t.Fatalf("confirmed after subset promote = %+v", confirmed)
	}
	staging, _ := store.Load("k", true)
	_ = staging
	all, _ := store.Load("k", false)
	if n := len(all) - len(confirmed); n != 1 {
		t.Fatalf("staging after subset promote = %d entries, want 1", n)
	}

	// Unknown id is an error, not a silent no-op.
	if _, err := store.Promote("k", []string{"nope"}); err == nil {
		t.Error("unknown promote id must error")
	}

	// Promote the rest; a second run is an idempotent no-op.
	if moved, err = store.Promote("k", nil); err != nil || moved != 1 {
		t.Fatalf("full promote = %d, %v", moved, err)
	}
	if moved, err = store.Promote("k", nil); err != nil || moved != 0 {
		t.Fatalf("empty-staging promote = %d, %v", moved, err)
	}
	confirmed, _ = store.Load("k", true)
	if len(confirmed) != 2 || !slices.ContainsFunc(confirmed, func(e Entry) bool { return e.Sample.ID == "s-b" }) {
		t.Fatalf("confirmed after full promote = %+v", confirmed)
	}
	all, _ = store.Load("k", false)
	if len(all) != 2 {
		t.Errorf("library after full promote = %d entries, want 2", len(all))
	}
}

// TestStoreRejectsCorruptAndInvalid: a corrupt library file is an
// error (never a silent empty layer) and an invalid entry is rejected
// before anything is written.
func TestStoreRejectsCorruptAndInvalid(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if _, _, err := store.Add("t", true, []Entry{entry("ok", "正常样本", SourceManual)}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the confirmed file; Add (which loads both layers) fails.
	if err := writeFile(store.ConfirmedPath("t"), []byte("entries: [not: valid: yaml\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add("t", false, []Entry{entry("x", "新样本", SourceManual)}); err == nil {
		t.Error("corrupt library must fail Add, not read as empty")
	}
	if _, err := store.Load("t", false); err == nil {
		t.Error("corrupt library must fail Load")
	}

	// Invalid entries: unknown source, wrong hash, bad sample.
	store2 := NewStore(t.TempDir())
	bad := entry("bad", "样本", Source("nonsense"))
	if _, _, err := store2.Add("t", true, []Entry{bad}); err == nil {
		t.Error("unknown source must be rejected")
	}
	stale := entry("stale", "样本", SourceManual)
	stale.InputHash = "0000"
	if _, _, err := store2.Add("t", true, []Entry{stale}); err == nil {
		t.Error("stale input_hash must be rejected")
	}
	invalid := Entry{Sample: core.Sample{ID: "no-input", Expected: "x", Split: "test"}, Source: SourceManual}
	if _, _, err := store2.Add("t", true, []Entry{invalid}); err == nil {
		t.Error("sample without input must be rejected")
	}
	// Nothing was written by the failed batches.
	if _, err := store2.Load("t", false); err != nil {
		t.Errorf("failed batches must leave the library untouched: %v", err)
	}
}

// TestValidateTaskKey pins the directory-name guard.
func TestValidateTaskKey(t *testing.T) {
	for _, ok := range []string{"tcm", "tcm-30", "task_v2", "a.b", "中医辨证"} {
		if err := ValidateTaskKey(ok); err != nil {
			t.Errorf("ValidateTaskKey(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../evil", "a\nb", string(rune(0x1f))} {
		if err := ValidateTaskKey(bad); err == nil {
			t.Errorf("ValidateTaskKey(%q) = nil, want error", bad)
		}
	}
}

// TestCustomRootRoundtrip: the same key in two roots stays two
// independent libraries — the --anchors-dir contract.
func TestCustomRootRoundtrip(t *testing.T) {
	rootA, rootB := NewStore(t.TempDir()), NewStore(t.TempDir())
	if _, _, err := rootA.Add("k", true, []Entry{entry("a", "甲库样本", SourceManual)}); err != nil {
		t.Fatal(err)
	}
	gotA, err := rootA.Load("k", true)
	if err != nil || len(gotA) != 1 {
		t.Fatalf("root A = %d entries, %v", len(gotA), err)
	}
	gotB, err := rootB.Load("k", true)
	if err != nil || len(gotB) != 0 {
		t.Fatalf("root B = %d entries, %v (roots must be independent)", len(gotB), err)
	}
	if rootB.HasConfirmed("k") {
		t.Error("root B must not see root A's confirmed file")
	}
}

// writeFile writes b to path, creating parent directories.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

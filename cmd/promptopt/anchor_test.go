package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/core"
)

func anchorCli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCli(t, anchorCommand, args...)
}

func anchorCliStderr(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCliStderr(t, anchorCommand, args...)
}

// anchorDatasetYAML renders an import dataset; each sample is
// {input, expected, id}.
func anchorDatasetYAML(name string, samples [][3]string) string {
	var sb strings.Builder
	sb.WriteString("name: " + name + "\nsamples:\n")
	for _, s := range samples {
		input, expected, id := s[0], s[1], s[2]
		if expected == "" {
			expected = "预期"
		}
		fmt.Fprintf(&sb, "  - id: %s\n    input: %s\n    expected: %s\n    split: test\n", id, input, expected)
	}
	return sb.String()
}

// TestAnchorAddListPromoteFlow drives the沉淀 front door end to end:
// staging by default, dedup on re-import, --confirmed for the real
// library, list reporting both layers, promote moving entries over
// with provenance intact.
func TestAnchorAddListPromoteFlow(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), "lib")
	ds := writeYAML(t, "import.yaml", anchorDatasetYAML("import", [][3]string{
		{"恶寒发热，无汗，脉浮紧。", "风寒束表", "imp-a-0"},
		{"心烦不寐，腰膝酸软，脉细数。", "心肾不交", "imp-a-1"},
		{"发热微恶风寒，咽痛，脉浮数。", "风热犯表", "imp-a-2"},
	}))

	// Default lands in staging; the confirmed file must not exist.
	if code, out := anchorCli(t, "add", ds, "--anchors-dir", libDir, "--task-key", "tcm",
		"--source", "manual", "--origin-run", "20261001-ab"); code != exitOK {
		t.Fatalf("anchor add exit = %d\n%s", code, out)
	}
	store := anchors.NewStore(libDir)
	if _, err := os.Stat(store.StagingPath("tcm")); err != nil {
		t.Fatalf("staging file missing: %v", err)
	}
	if store.HasConfirmed("tcm") {
		t.Error("default add must not write the confirmed library")
	}
	all, err := store.Load("tcm", false)
	if err != nil || len(all) != 3 {
		t.Fatalf("staging load = %d entries, %v", len(all), err)
	}
	if all[0].Source != anchors.SourceManual || all[0].OriginRun != "20261001-ab" {
		t.Errorf("entry metadata = %+v", all[0])
	}
	if all[0].InputHash != core.HashInput(all[0].Sample.Input) {
		t.Errorf("input_hash = %q, want the shared core.HashInput", all[0].InputHash)
	}

	// Re-import is a full dedup: nothing added, everything skipped.
	code, stderr := anchorCliStderr(t, "add", ds, "--anchors-dir", libDir, "--task-key", "tcm")
	if code != exitOK {
		t.Fatalf("duplicate add exit = %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "跳过重复 3 条") {
		t.Errorf("duplicate add stderr = %q", stderr)
	}
	all, _ = store.Load("tcm", false)
	if len(all) != 3 {
		t.Errorf("library grew on duplicate import: %d entries", len(all))
	}

	// A confirmed import on a different dataset joins the real library.
	ds2 := writeYAML(t, "import2.yaml", anchorDatasetYAML("import2", [][3]string{
		{"新样本输入，与前者不同。", "全新证候", "imp-b-0"},
	}))
	if code, _ := anchorCli(t, "add", ds2, "--anchors-dir", libDir, "--task-key", "tcm", "--confirmed"); code != exitOK {
		t.Fatalf("confirmed add exit = %d", code)
	}
	confirmed, _ := store.Load("tcm", true)
	if len(confirmed) != 1 || confirmed[0].Sample.Input != "新样本输入，与前者不同。" {
		t.Fatalf("confirmed = %+v", confirmed)
	}

	// list reports both layers.
	code, stderr = anchorCliStderr(t, "list", "--anchors-dir", libDir, "--task-key", "tcm")
	if code != exitOK {
		t.Fatalf("list exit = %d\n%s", code, stderr)
	}
	for _, want := range []string{"confirmed 1 条", "staging 3 条", "source=manual", "hash="} {
		if !strings.Contains(stderr, want) {
			t.Errorf("list stderr lacks %q: %s", want, stderr)
		}
	}

	// promote --ids moves a subset; unknown ids are a hard error.
	if code, _ = anchorCli(t, "promote", "--anchors-dir", libDir, "--task-key", "tcm", "--ids", "ghost"); code != exitFailure {
		t.Errorf("promote of unknown id exit = %d, want 1", code)
	}
	if code, _ = anchorCli(t, "promote", "--anchors-dir", libDir, "--task-key", "tcm", "--ids", "imp-a-0"); code != exitOK {
		t.Errorf("subset promote exit = %d", code)
	}
	confirmed, _ = store.Load("tcm", true)
	if len(confirmed) != 2 || confirmed[1].Sample.Input != "恶寒发热，无汗，脉浮紧。" {
		t.Fatalf("confirmed after subset promote = %+v", confirmed)
	}
	if confirmed[1].InputHash != core.HashInput(confirmed[1].Sample.Input) {
		t.Errorf("promoted input_hash mismatch: %q", confirmed[1].InputHash)
	}

	// Full promote drains staging; repeat is an idempotent no-op.
	if code, _ = anchorCli(t, "promote", "--anchors-dir", libDir, "--task-key", "tcm"); code != exitOK {
		t.Errorf("full promote exit = %d", code)
	}
	if code, stderr = anchorCliStderr(t, "promote", "--anchors-dir", libDir, "--task-key", "tcm"); code != exitOK {
		t.Errorf("idempotent promote exit = %d\n%s", code, stderr)
	}
	confirmed, _ = store.Load("tcm", true)
	all, _ = store.Load("tcm", false)
	if len(confirmed) != 4 || len(all) != 4 {
		t.Fatalf("final library = confirmed %d / all %d, want 4/4", len(confirmed), len(all))
	}
}

// TestAnchorUsageErrors pins the guard rails: task key required,
// source whitelisted, traversal rejected, dataset must parse.
func TestAnchorUsageErrors(t *testing.T) {
	libDir := t.TempDir()
	ds := writeYAML(t, "import.yaml", anchorDatasetYAML("import", [][3]string{
		{"恶寒发热，无汗，脉浮紧。", "风寒束表", "imp-a-0"},
	}))
	cases := []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"drop", "--task-key", "t"}},
		{"add without task key", []string{"add", ds, "--anchors-dir", libDir}},
		{"add with bad source", []string{"add", ds, "--anchors-dir", libDir, "--task-key", "tcm", "--source", "gossip"}},
		{"add with traversal key", []string{"add", ds, "--anchors-dir", libDir, "--task-key", "../evil"}},
		{"add without dataset", []string{"add", "--anchors-dir", libDir, "--task-key", "tcm"}},
		{"add with missing file", []string{"add", filepath.Join(libDir, "nope.yaml"), "--anchors-dir", libDir, "--task-key", "tcm"}},
		{"list without task key", []string{"list", "--anchors-dir", libDir}},
		{"promote without task key", []string{"promote", "--anchors-dir", libDir}},
	}
	for _, tc := range cases {
		if code, _ := anchorCli(t, tc.args...); code != exitFailure {
			t.Errorf("%s: exit = %d, want 1", tc.name, code)
		}
	}
	// The traversal attempt must not have written outside the root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(libDir), "evil")); !os.IsNotExist(err) {
		t.Errorf("traversal key escaped the library root (stat err %v)", err)
	}
}

// TestAnchorAddedAtDefaultsToNow: entries the store validates carry a
// real UTC timestamp (the CLI leaves AddedAt zero).
func TestAnchorAddedAtDefaultsToNow(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), "lib")
	ds := writeYAML(t, "import.yaml", anchorDatasetYAML("import", [][3]string{
		{"时间戳样本输入。", "证候", "imp-a-0"},
	}))
	if code, _ := anchorCli(t, "add", ds, "--anchors-dir", libDir, "--task-key", "tcm", "--confirmed"); code != exitOK {
		t.Fatal("add failed")
	}
	store := anchors.NewStore(libDir)
	entries, err := store.Load("tcm", true)
	if err != nil || len(entries) != 1 {
		t.Fatalf("load = %d entries, %v", len(entries), err)
	}
	if entries[0].AddedAt.IsZero() || time.Since(entries[0].AddedAt) > time.Minute {
		t.Errorf("AddedAt = %v, want a fresh UTC stamp", entries[0].AddedAt)
	}
}

// TestAnchorAddDedupsWhitespaceVariant: dedup keys on the shared
// core.HashInput, so a re-import whose input differs only in whitespace
// layout collapses onto the stored entry and is skipped, not appended.
func TestAnchorAddDedupsWhitespaceVariant(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), "lib")
	store := anchors.NewStore(libDir)
	// The stored baseline carries one space after the comma; any
	// whitespace layout of the same token sequence must collapse onto it
	// (core.NormalizeInput: whitespace runs → one space, trimmed).
	first := writeYAML(t, "a.yaml", `name: a
samples:
  - id: v-1
    input: 恶寒发热， 无汗，脉浮紧。
    expected: 风寒束表
    split: test
`)
	if code, _ := anchorCli(t, "add", first, "--anchors-dir", libDir, "--task-key", "tcm", "--confirmed"); code != exitOK {
		t.Fatal("first add failed")
	}
	// Same token text, different whitespace layout + different id (YAML
	// double quotes carry the \n escapes).
	variant := writeYAML(t, "b.yaml", `name: b
samples:
  - id: v-2
    input: "  恶寒发热，\t\n  无汗，脉浮紧。  "
    expected: 风寒束表
    split: test
`)
	code, stderr := anchorCliStderr(t, "add", variant, "--anchors-dir", libDir, "--task-key", "tcm", "--confirmed")
	if code != exitOK || !strings.Contains(stderr, "跳过重复 1 条") {
		t.Fatalf("whitespace-variant add = %d, stderr %q", code, stderr)
	}
	confirmed, err := store.Load("tcm", true)
	if err != nil || len(confirmed) != 1 || confirmed[0].Sample.ID != "v-1" {
		t.Fatalf("library after variant add = %+v (%v)", confirmed, err)
	}
}

// TestTaskKeyDefault pins both fallback branches (提案 §1.2 关键前提):
// the zero-config prompt hashes through the shared core.HashInput (the
// synthesized task name is LLM-generated per run and unstable), the
// configured three-file mode uses the task file's stem.
func TestTaskKeyDefault(t *testing.T) {
	prompt := "从中医医案文本判断证候"
	if got := taskKeyDefault(prompt, ""); got != core.HashInput(prompt)[:12] {
		t.Errorf("zero-config key = %q, want core.HashInput(prompt)[:12] = %q", got, core.HashInput(prompt)[:12])
	}
	if got := taskKeyDefault("", "/tmp/examples/json_extraction/task.yaml"); got != "task" {
		t.Errorf("configured key = %q, want the task stem %q", got, "task")
	}
	if got := taskKeyDefault("", ""); got != "" {
		t.Errorf("empty inputs key = %q, want empty", got)
	}
}

// TestRunManifestTaskKeySnapshot: both run modes snapshot the resolved
// task key into manifest.task_key so verify --promote/--anchor-lib can
// reproduce the library key without the flag.
func TestRunManifestTaskKeySnapshot(t *testing.T) {
	t.Run("zero-config prompt hash", func(t *testing.T) {
		runsDir, runID := zeroConfigRunTree(t)
		var mf runManifest
		loadJSONFile(t, filepath.Join(runsDir, runID, "manifest.json"), &mf)
		if want := core.HashInput("从中医医案文本判断证候")[:12]; mf.TaskKey != want {
			t.Errorf("manifest.task_key = %q, want %q", mf.TaskKey, want)
		}
	})
	t.Run("configured task stem", func(t *testing.T) {
		srv := startFakeLLM(t)
		task, cand, ds := fixtureYAMLs(t)
		outDir := filepath.Join(t.TempDir(), "runs")
		code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
			"--out", outDir, "--headless")...)
		if code != exitOK {
			t.Fatalf("manual run exit = %d\n%s", code, out)
		}
		entries, err := os.ReadDir(outDir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("run dirs = %v (%v)", entries, err)
		}
		var mf runManifest
		loadJSONFile(t, filepath.Join(outDir, entries[0].Name(), "manifest.json"), &mf)
		if mf.TaskKey != "task" {
			t.Errorf("manifest.task_key = %q, want the task stem %q", mf.TaskKey, "task")
		}
	})
}

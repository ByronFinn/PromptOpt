package main

import (
	"os"
	"path/filepath"
	"testing"
)

// onlyRunDir returns the single run directory under outDir.
func onlyRunDir(t *testing.T, outDir string) string {
	t.Helper()
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	return entries[0].Name()
}

// TestRunTaskKeyExplicitFlagAndValidation covers what the shared
// snapshot test in anchor_test.go leaves out: an explicit --task-key
// wins over the derived default and lands in manifest.task_key
// verbatim, while a traversal key is a parse-time usage error that
// never reaches evaluation.
func TestRunTaskKeyExplicitFlagAndValidation(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)

	outDir := filepath.Join(t.TempDir(), "runs")
	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", outDir, "--task-key", "中医辨证库", "--headless")...)
	if code != exitOK {
		t.Fatalf("manual run with key exit = %d\n%s", code, out)
	}
	var mf runManifest
	loadJSONFile(t, filepath.Join(outDir, onlyRunDir(t, outDir), "manifest.json"), &mf)
	if mf.TaskKey != "中医辨证库" {
		t.Errorf("manifest.task_key = %q, want the explicit flag value", mf.TaskKey)
	}

	if code, _ = runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", filepath.Join(t.TempDir(), "runs"), "--task-key", "../evil", "--headless")...); code != exitFailure {
		t.Errorf("traversal task key exit = %d, want 1", code)
	}
}

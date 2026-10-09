package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/core"
)

// seedAnchorLibViaCLI fills a custom library root through the anchor
// CLI itself (add --confirmed + default staging — the store-API twin
// lives in verify_anchor_test.go): the confirmed layer gets the three
// zcScript samples (the keyed fake LLM answers them all), staging gets
// one alien input the model would answer wrong — its leaking into the
// verification set is exactly what this file's tests detect. Returns
// the staging input.
func seedAnchorLibViaCLI(t *testing.T, libDir, key string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("name: anchor-lib\nsamples:\n")
	for i, sc := range zcScript {
		fmt.Fprintf(&sb, "  - id: lib-%d\n    input: %s\n    expected: %s\n    split: test\n", i+1, sc.input, sc.expected)
	}
	if code, out := anchorCli(t, "add", writeYAML(t, "libseed.yaml", sb.String()),
		"--anchors-dir", libDir, "--task-key", key, "--confirmed"); code != exitOK {
		t.Fatalf("confirmed seed exit = %d\n%s", code, out)
	}
	stagingInput := "无关输入：键控模型不认识这段话。"
	staging := "name: staging\nsamples:\n  - id: lib-staging\n    input: " + stagingInput + "\n    expected: 无法辨证\n    split: test\n"
	if code, out := anchorCli(t, "add", writeYAML(t, "libstaging.yaml", staging),
		"--anchors-dir", libDir, "--task-key", key); code != exitOK {
		t.Fatalf("staging seed exit = %d\n%s", code, out)
	}
	return stagingInput
}

// callPrompts reads every calls/*.json under dir and returns the raw
// bytes (prompt substring checks stay schema-free).
func callPrompts(t *testing.T, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatalf("read calls dir: %v", err)
	}
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "calls", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// TestVerifyAnchorLibLoadsOnlyConfirmed: the verified set is exactly
// the confirmed library — staging candidates stay out of the gate
// (ADR 0001 真实性分级), and the custom --anchors-dir roundtrips.
func TestVerifyAnchorLibLoadsOnlyConfirmed(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), "anchors")
	stagingInput := seedAnchorLibViaCLI(t, libDir, "tcm")
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-good", "优化后的辨证提示词。文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir,
		"--anchor-lib", "tcm", "--anchors-dir", libDir,
		"--base-url", srv.URL, "--model", "fake-model", "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify report JSON: %v\n%s", err, out)
	}
	if rep.Mode != verifyModeAnchor || rep.AnchorLib != "tcm" || rep.AnchorPath != "" || rep.Caveat {
		t.Errorf("report anchor fields = mode %s lib %q path %q caveat %v", rep.Mode, rep.AnchorLib, rep.AnchorPath, rep.Caveat)
	}
	if rep.Baseline.Means["exact_match"] != 1 || rep.Delivered.Means["exact_match"] != 1 {
		t.Errorf("means = base %+v deliv %+v (a staging leak would push below 1)", rep.Baseline.Means, rep.Delivered.Means)
	}

	verifyDir := onlyVerifyDir(t, filepath.Join(runsDir, runID))
	for _, side := range []string{"baseline", "delivered"} {
		prompts := callPrompts(t, filepath.Join(verifyDir, side))
		if len(prompts) != 3 {
			t.Errorf("%s calls = %d, want 3 (confirmed library size)", side, len(prompts))
		}
		for i, b := range prompts {
			if strings.Contains(string(b), stagingInput) {
				t.Errorf("%s call %d contains the staging-only input", side, i)
			}
		}
	}
	// The verification set copy names the library it came from.
	var ds core.Dataset
	loadJSONFile(t, filepath.Join(verifyDir, "anchor-dataset.json"), &ds)
	if len(ds.Samples) != 3 {
		t.Errorf("anchor-dataset.json = %d samples, want 3", len(ds.Samples))
	}
}

// TestVerifyAnchorAndAnchorLibMutuallyExclusive: one verification set
// source per verify — giving both is a usage error, caught at flag
// parse time and again at the command boundary (测试③).
func TestVerifyAnchorAndAnchorLibMutuallyExclusive(t *testing.T) {
	if _, _, err := parseVerifyFlags([]string{"r1", "--anchor", "a.yaml", "--anchor-lib", "tcm"}); err == nil {
		t.Error("--anchor + --anchor-lib must be a usage error at parse time")
	}
	if code, _ := verifyCli(t, "r1", "--runs-dir", t.TempDir(), "--anchor", "a.yaml", "--anchor-lib", "tcm"); code != exitFailure {
		t.Errorf("CLI exit = %d, want 1", code)
	}
}

// TestVerifyPromoteLandsStagingWithVerdict: --promote reflows the whole
// verification set into the task's staging area with per-sample
// verdict labels and run provenance — never the confirmed library
// (同族偏差样本不得自动获得真实背书，ADR 0001).
func TestVerifyPromoteLandsStagingWithVerdict(t *testing.T) {
	libDir := filepath.Join(t.TempDir(), "anchors")
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-bad", verifyBadMarker+" 文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)

	code, out := verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model",
		"--promote", "--task-key", "tcm", "--anchors-dir", libDir, "--headless")
	if code != exitRegression {
		t.Fatalf("exit = %d, want 3\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("verify report JSON: %v\n%s", err, out)
	}
	if rep.Promoted != 3 {
		t.Errorf("report promoted = %d, want 3", rep.Promoted)
	}

	store := anchors.NewStore(libDir)
	if store.HasConfirmed("tcm") {
		t.Error("--promote must not write the confirmed library")
	}
	staging, err := store.Load("tcm", false)
	if err != nil || len(staging) != 3 {
		t.Fatalf("staging = %d entries, %v", len(staging), err)
	}
	for _, e := range staging {
		if e.Source != anchors.SourceVerifyFailure {
			t.Errorf("entry %s source = %s, want verify-failure", e.Sample.ID, e.Source)
		}
		if e.Verdict != anchors.VerdictRegressed {
			t.Errorf("entry %s verdict = %q, want regressed (delivered 0 vs baseline 1)", e.Sample.ID, e.Verdict)
		}
		if e.OriginRun != runID {
			t.Errorf("entry %s origin_run = %q, want %q", e.Sample.ID, e.OriginRun, runID)
		}
		if e.InputHash != core.HashInput(e.Sample.Input) {
			t.Errorf("entry %s input_hash mismatch", e.Sample.ID)
		}
	}
	// verify.md records the reflow.
	verifyDir := onlyVerifyDir(t, filepath.Join(runsDir, runID))
	md, err := os.ReadFile(filepath.Join(verifyDir, "verify.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "锚点回流") || !strings.Contains(string(md), "staging") {
		t.Errorf("verify.md lacks the reflow note:\n%s", md)
	}

	// The promote subcommand completes the loop: staging → confirmed.
	if code, _ := anchorCli(t, "promote", "--anchors-dir", libDir, "--task-key", "tcm"); code != exitOK {
		t.Fatalf("anchor promote exit = %d", code)
	}
	confirmed, _ := store.Load("tcm", true)
	if len(confirmed) != 3 || confirmed[0].Verdict != anchors.VerdictRegressed {
		t.Fatalf("confirmed after promote = %+v", confirmed)
	}
	// A second verify --promote over the same set dedups to a no-op.
	code, out = verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model",
		"--promote", "--task-key", "tcm", "--anchors-dir", libDir, "--headless")
	if code != exitRegression {
		t.Fatalf("re-verify exit = %d, want 3\n%s", code, out)
	}
	confirmed, _ = store.Load("tcm", true)
	all, _ := store.Load("tcm", false)
	if len(confirmed) != 3 || len(all) != 3 {
		t.Errorf("re-promote duplicated entries: confirmed %d / all %d", len(confirmed), len(all))
	}
}

// TestVerifyPromoteTaskKeyFallsBackToManifest: --task-key resolves
// flag > manifest.task_key — a run launched with --task-key lets
// verify --promote omit the flag.
func TestVerifyPromoteTaskKeyFallsBackToManifest(t *testing.T) {
	srv := startScriptedZeroConfigLLM(t, zcSamples, zcScript)
	outDir := filepath.Join(t.TempDir(), "runs")
	code, out := runCli(t, "从中医医案文本判断证候",
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
		"--task-key", "manifest-key")
	if code != 0 {
		t.Fatalf("zero-config run exit = %d\n%s", code, out)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	runID := entries[0].Name()
	var mf runManifest
	loadJSONFile(t, filepath.Join(outDir, runID, "manifest.json"), &mf)
	if mf.TaskKey != "manifest-key" {
		t.Fatalf("manifest task_key = %q, want the explicit flag", mf.TaskKey)
	}

	libDir := filepath.Join(t.TempDir(), "anchors")
	adoptForVerify(t, outDir, runID, "hand-bad", verifyBadMarker+" 文本：{input}")
	keyed := startPromptKeyedLLM(t, verifyBadMarker)
	code, out = verifyCli(t, runID, "--runs-dir", outDir,
		"--base-url", keyed.URL, "--model", "fake-model",
		"--promote", "--anchors-dir", libDir, "--headless")
	if code != exitRegression {
		t.Fatalf("verify exit = %d, want 3\n%s", code, out)
	}
	store := anchors.NewStore(libDir)
	staging, err := store.Load("manifest-key", false)
	if err != nil || len(staging) != 3 {
		t.Fatalf("staging under manifest key = %d entries, %v", len(staging), err)
	}
}

// TestVerifyPromoteWithoutAnyTaskKeyIsUsageError: with the flag unset
// AND the manifest snapshot stripped, the reflow is a usage error —
// samples must never land under an empty key.
func TestVerifyPromoteWithoutAnyTaskKeyIsUsageError(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	adoptForVerify(t, runsDir, runID, "hand-bad", verifyBadMarker+" 文本：{input}")
	srv := startPromptKeyedLLM(t, verifyBadMarker)
	mfPath := filepath.Join(runsDir, runID, "manifest.json")
	b, err := os.ReadFile(mfPath)
	if err != nil {
		t.Fatal(err)
	}
	var mfAny map[string]any
	if err := json.Unmarshal(b, &mfAny); err != nil {
		t.Fatal(err)
	}
	delete(mfAny, "task_key")
	nb, _ := json.Marshal(mfAny)
	if err := os.WriteFile(mfPath, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	libDir := filepath.Join(t.TempDir(), "anchors")
	if code, _ := verifyCli(t, runID, "--runs-dir", runsDir,
		"--base-url", srv.URL, "--model", "fake-model",
		"--promote", "--anchors-dir", libDir, "--headless"); code != exitFailure {
		t.Errorf("--promote without any task key exit = %d, want 1", code)
	}
}

// TestPromoteVerificationSetVerdictLabels pins the label rule at the
// function level: delivered below baseline on the primary → regressed,
// at or above (a tie included) → pass.
func TestPromoteVerificationSetVerdictLabels(t *testing.T) {
	libDir := t.TempDir()
	o := verifyOptions{anchorsDir: libDir, taskKey: "k"}
	samples := []core.Sample{
		{ID: "s-reg", Input: "回归样本输入。", Expected: "x", Split: "test"},
		{ID: "s-pass", Input: "通过样本输入。", Expected: "y", Split: "test"},
	}
	base := verifySide{Rows: []float64{1, 0.5}}
	deliv := verifySide{Rows: []float64{0.4, 0.5}}
	added, skipped, err := promoteVerificationSet(o, "run-xyz", samples, base, deliv)
	if err != nil || added != 2 || skipped != 0 {
		t.Fatalf("promote = %d/%d, %v", added, skipped, err)
	}
	store := anchors.NewStore(libDir)
	// The reflow lands in staging — the confirmed layer stays empty
	// until a human runs anchor promote.
	if entries, err := store.Load("k", true); err != nil || len(entries) != 0 {
		t.Fatalf("confirmedOnly load = %d entries (%v), want 0", len(entries), err)
	}
	all, err := store.Load("k", false)
	if err != nil || len(all) != 2 {
		t.Fatalf("staging load = %d entries, %v", len(all), err)
	}
	verdicts := map[string]string{}
	for _, e := range all {
		verdicts[e.Sample.ID] = e.Verdict
		if e.OriginRun != "run-xyz" {
			t.Errorf("entry %s origin_run = %q, want run-xyz", e.Sample.ID, e.OriginRun)
		}
	}
	if verdicts["s-reg"] != anchors.VerdictRegressed {
		t.Errorf("s-reg verdict = %q, want regressed", verdicts["s-reg"])
	}
	if verdicts["s-pass"] != anchors.VerdictPass {
		t.Errorf("s-pass verdict = %q, want pass (a tie is not a regression)", verdicts["s-pass"])
	}
}

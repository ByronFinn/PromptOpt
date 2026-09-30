package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

func replayCli(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return captureCli(t, replayCommand, args...)
}

// callTraceJSON renders one CallTrace artifact body.
func callTraceJSON(seq int, sampleID, role, prompt, ts string) string {
	return fmt.Sprintf(`{"seq":%d,"sample_id":%q,"role":%q,`+
		`"request":{"model":"m","messages":[{"role":"user","content":%q}],"max_tokens":100},`+
		`"response":{"content":"resp","usage":{"prompt_tokens":10,"completion_tokens":5}},`+
		`"latency_ms":120,"time":%q}`, seq, sampleID, role, prompt, ts)
}

// writeReplayFixture hand-writes a zero-config-shaped run tree with
// interleaved event and call timestamps:
//
//	09:59:59 synth spec call      10:00:03   round_start
//	10:00:00 run_start            10:00:03.5 optimizer reflect call
//	10:00:01 baseline eval call    10:00:04   optimization unit call
//	10:00:02 sample_done           10:00:05   run_done
func writeReplayFixture(t *testing.T) (runsDir, runID, synthDir string) {
	t.Helper()
	root := t.TempDir()
	runsDir = filepath.Join(root, "runs")
	runID = "r1"
	runDir := filepath.Join(runsDir, runID)
	synthDir = filepath.Join(root, "synth", runID)
	for _, dir := range []string{
		filepath.Join(runDir, "calls"),
		filepath.Join(runDir, "evals", "01-g01", "calls"),
		filepath.Join(runDir, "opt-calls"),
		filepath.Join(synthDir, "calls"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(runDir, "manifest.json"),
		`{"run_id":"r1","model":"m","mode":"autopilot","optimizer":"gepa","synth_samples":3}`)
	write(filepath.Join(runDir, "run.json"),
		`{"run_id":"r1","status":"completed","exit_code":0,`+
			`"usage_by_role":{"executor":{"prompt_tokens":30,"completion_tokens":15}}}`)
	write(filepath.Join(runDir, "frontier.json"), candidatesFrontier)
	write(filepath.Join(runDir, "adopted.json"),
		`{"candidate_id":"g01","prompt":"优化提示词 {input}","adopted_at":"2026-09-30T10:00:06Z"}`)
	events := strings.Join([]string{
		`{"type":"run_start","time":"2026-09-30T10:00:00Z","run_id":"r1"}`,
		`{"type":"sample_done","time":"2026-09-30T10:00:02Z","run_id":"r1","sample_id":"s1","scores":{"exact_match":1}}`,
		`{"type":"round_start","time":"2026-09-30T10:00:03Z","run_id":"r1","detail":{"round":1}}`,
		`{"type":"run_done","time":"2026-09-30T10:00:05Z","run_id":"r1","status":"completed","exit_code":0}`,
	}, "\n") + "\n"
	write(filepath.Join(runDir, "events.jsonl"), events)

	write(filepath.Join(synthDir, "calls", "001-spec.json"),
		callTraceJSON(1, "spec", "optimizer", "合成任务规格的完整提示词", "2026-09-30T09:59:59Z"))
	write(filepath.Join(runDir, "calls", "001-s1.json"),
		callTraceJSON(1, "s1", "executor", "基线评估提示词", "2026-09-30T10:00:01Z"))
	write(filepath.Join(runDir, "opt-calls", "001-reflect.json"),
		callTraceJSON(1, "reflect", "optimizer", "反思提示词", "2026-09-30T10:00:03.500Z"))
	write(filepath.Join(runDir, "evals", "01-g01", "calls", "001-s1.json"),
		callTraceJSON(1, "s1", "executor", "优化评估提示词", "2026-09-30T10:00:04Z"))
	return runsDir, runID, synthDir
}

func TestReplayTimelineSortedAndSummary(t *testing.T) {
	runsDir, runID, synthDir := writeReplayFixture(t)
	runDir := filepath.Join(runsDir, runID)

	entries, summary, err := collectAudit(runDir, synthDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 8 {
		t.Fatalf("entries = %d, want 8 (4 events + 4 calls)", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Time.After(entries[i].Time) {
			t.Errorf("entries[%d].Time %v > entries[%d].Time %v", i-1, entries[i-1].Time, i, entries[i].Time)
		}
	}
	// The earliest entry is the synthesis call; the latest is run_done.
	if entries[0].Kind != "call" || !strings.Contains(entries[0].Label, "合成调用") {
		t.Errorf("first entry = %+v, want the synth call", entries[0])
	}
	if entries[len(entries)-1].Kind != "event" || !strings.Contains(entries[len(entries)-1].Label, "运行结束") {
		t.Errorf("last entry = %+v, want run_done", entries[len(entries)-1])
	}

	if summary.Events != 4 || summary.TotalCalls != 4 {
		t.Errorf("summary counts = %d events / %d calls, want 4/4", summary.Events, summary.TotalCalls)
	}
	for _, src := range []string{replaySourceBaseline, replaySourceOptEval, replaySourceOptimizer, replaySourceSynth} {
		if summary.CallsBySource[src] != 1 {
			t.Errorf("calls[%s] = %d, want 1", src, summary.CallsBySource[src])
		}
	}
	if summary.Best != "g01" || summary.Adopted != "g01" {
		t.Errorf("best/adopted = %s/%s, want g01/g01", summary.Best, summary.Adopted)
	}
	if u := summary.UsageByRole[core.RoleExecutor]; u.PromptTokens != 30 || u.CompletionTokens != 15 {
		t.Errorf("usage = %+v, want run.json's executor row", u)
	}

	// Every call carries tokens and latency from its trace.
	for _, e := range entries {
		if e.Kind == "call" && (e.Tokens != 15 || e.LatencyMS != 120 || e.Model != "m") {
			t.Errorf("call entry = %+v, want 15 tok / 120 ms / model m", e)
		}
	}
}

func TestReplayHeadlessJSONLDecodes(t *testing.T) {
	runsDir, runID, _ := writeReplayFixture(t)

	code, out := replayCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var entries []auditEntry
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("JSONL line is not an audit entry: %v\n%s", err, line)
		}
		entries = append(entries, e)
	}
	if len(entries) != 8 {
		t.Fatalf("entries = %d, want 8", len(entries))
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Time.After(entries[i].Time) {
			t.Errorf("JSONL order broken at %d", i)
		}
	}
}

func TestReplayHumanOutput(t *testing.T) {
	runsDir, runID, _ := writeReplayFixture(t)

	code, stderr := captureCliStderr(t, replayCommand, runID, "--runs-dir", runsDir)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stderr)
	}
	for _, want := range []string{"run r1", "合成调用", "基线评估调用", "优化调用", "优化评估调用", "覆盖率：events 4 · calls 4", "best: g01"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

func TestReplayFullExpandsBodies(t *testing.T) {
	runsDir, runID, synthDir := writeReplayFixture(t)

	long := strings.Repeat("长", 200)
	callPath := filepath.Join(runsDir, runID, "evals", "01-g01", "calls", "002-s2.json")
	if err := os.WriteFile(callPath, []byte(callTraceJSON(2, "s2", "executor", long, "2026-09-30T10:00:04.500Z")), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, _, err := collectAudit(filepath.Join(runsDir, runID), synthDir, false)
	if err != nil {
		t.Fatal(err)
	}
	var excerpt, full string
	for _, e := range entries {
		if e.SampleID != "s2" || e.Kind != "call" {
			continue
		}
		excerpt, _ = e.Detail["prompt"].(string)
	}
	entries, _, err = collectAudit(filepath.Join(runsDir, runID), synthDir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.SampleID != "s2" || e.Kind != "call" {
			continue
		}
		full, _ = e.Detail["prompt"].(string)
	}
	if excerpt == "" || full == "" {
		t.Fatalf("s2 entries not found (excerpt %q full %q)", excerpt, full)
	}
	if len([]rune(excerpt)) > 123 || !strings.HasSuffix(excerpt, "...") {
		t.Errorf("excerpt length = %d, want <= 123 with ellipsis", len([]rune(excerpt)))
	}
	if full != long {
		t.Errorf("full body length = %d, want 200 runes verbatim", len([]rune(full)))
	}
}

// TestReplayJudgeStageSurfaces: a call trace carrying the additive
// Stage field (llm_judge) surfaces through the label/stage even though
// the filename alone cannot distinguish it.
func TestReplayJudgeStageSurfaces(t *testing.T) {
	runsDir, runID, _ := writeReplayFixture(t)
	trace := strings.Replace(callTraceJSON(2, "s1", "executor", "裁判提示词", "2026-09-30T10:00:02.500Z"),
		`"time":"2026-09-30T10:00:02.500Z"`, `"time":"2026-09-30T10:00:02.500Z","stage":"judge"`, 1)
	if err := os.WriteFile(filepath.Join(runsDir, runID, "calls", "002-s1.json"), []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, _, err := collectAudit(filepath.Join(runsDir, runID), filepath.Join(runsDir, "..", "synth", runID), false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Stage == "judge" {
			found = true
			if !strings.Contains(e.Label, "judge") {
				t.Errorf("label = %q, want the judge stage surfaced", e.Label)
			}
		}
	}
	if !found {
		t.Error("no entry carries stage=judge")
	}
}

// TestReplayManualRunWithoutFrontier: a manual-mode run tree (no
// frontier, no synth) replays its baseline timeline.
func TestReplayManualRunWithoutFrontier(t *testing.T) {
	runsDir := filepath.Join(t.TempDir(), "runs")
	runID := "manual1"
	runDir := filepath.Join(runsDir, runID)
	if err := os.MkdirAll(filepath.Join(runDir, "calls"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "run.json"),
		[]byte(`{"run_id":"manual1","status":"completed","exit_code":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"),
		[]byte(`{"type":"run_done","time":"2026-09-30T10:00:00Z","run_id":"manual1","status":"completed","exit_code":0}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "calls", "001-s1.json"),
		[]byte(callTraceJSON(1, "s1", "executor", "提示词", "2026-09-30T09:59:59Z")), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := replayCli(t, runID, "--runs-dir", runsDir, "--headless")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	var entries []auditEntry
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("JSONL decode: %v (%s)", err, line)
		}
		entries = append(entries, e)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (call + run_done)", len(entries))
	}
	// The call precedes the terminal event.
	if entries[0].Kind != "call" || entries[1].Kind != "event" {
		t.Errorf("order = %s then %s, want call before run_done", entries[0].Kind, entries[1].Kind)
	}
}

func TestReplayMissingRunDirExitsOne(t *testing.T) {
	if code, _ := replayCli(t, "nope", "--runs-dir", t.TempDir()); code != exitFailure {
		t.Errorf("exit = %d, want 1", code)
	}
	if code, _ := replayCli(t, "--runs-dir", t.TempDir()); code != exitFailure {
		t.Errorf("exit = %d, want 1 without a run id", code)
	}
}

// TestReplayEntryTimesDecode pins RFC3339 (with fractional seconds)
// round-trip through the JSONL output.
func TestReplayEntryTimesDecode(t *testing.T) {
	runsDir, runID, _ := writeReplayFixture(t)
	_, out := replayCli(t, runID, "--runs-dir", runsDir, "--headless")
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		if e.Time.IsZero() {
			t.Fatalf("zero time in entry %s", line)
		}
	}
	// The fractional-second optimizer call survives the round trip.
	if !strings.Contains(out, "10:00:03.5") {
		t.Errorf("output lacks the fractional timestamp:\n%s", out)
	}
}

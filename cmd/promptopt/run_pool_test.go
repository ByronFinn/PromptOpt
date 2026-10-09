package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/pool"
)

// runCliVerbose invokes runCommand capturing stdout AND stderr — the
// pool warnings live on stderr only, so the headless stdout JSON
// contract and the warning lines are asserted independently.
func runCliVerbose(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	code = runCommand(args)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = wOut.Close()
	_ = wErr.Close()
	outB, _ := io.ReadAll(rOut)
	errB, _ := io.ReadAll(rErr)
	return code, string(outB), string(errB)
}

// poolManualTrio writes a single-metric trio so the pool rows project
// the exact_match primary.
func poolManualTrio(t *testing.T) (task, cand, ds string) {
	t.Helper()
	task = writeYAML(t, "task.yaml", `name: pool_manual
prompt_template: |
  answer for {input}
metrics: [exact_match]
primary_metric: exact_match
`)
	cand = writeYAML(t, "candidate.yaml", `id: baseline
prompt: |
  baseline prompt {input}
`)
	ds = writeYAML(t, "dataset.yaml", `name: ds
samples:
  - id: s1
    input: 甲
    expected: {ok: true}
    split: train
  - id: s2
    input: 乙
    expected: {ok: true}
    split: dev
  - id: s3
    input: 丙
    expected: {ok: true}
    split: test
`)
	return task, cand, ds
}

// TestRunPoolManualTwoRunsSameDatasetWarnRegression（用例 ⑤ 手动腿）is
// the reachable same-set scenario: the manual trio keeps its dataset
// fixed, so the second run pairs against the first and the stubbed
// quality collapse trips the paired-bootstrap degradation warning.
func TestRunPoolManualTwoRunsSameDatasetWarnRegression(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		content := "not json"
		if calls.Add(1) <= 3 { // run 1's three samples answer correctly
			content = `{"ok": true}`
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	task, cand, ds := poolManualTrio(t)
	outDir := filepath.Join(t.TempDir(), "runs")
	base := append(baseFlags(t, srv, task, cand, ds), "--out", outDir, "--headless")

	// Run 1: the pool is empty — no comparison line, the entry lands.
	code, out, errOut := runCliVerbose(t, base...)
	if code != 0 {
		t.Fatalf("run 1 exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if strings.Contains(errOut, "候选池退化预警") || strings.Contains(errOut, "跨 run 样本集不同") {
		t.Errorf("run 1 must not warn (empty pool), stderr:\n%s", errOut)
	}
	store := pool.NewPoolStore(poolRoot(outDir))
	// The manual task key defaults to the task file's stem (task.yaml
	// → "task") — the same derivation the CLI used.
	taskKey := taskKeyDefault("", task)
	entries, err := store.Entries(taskKey)
	if err != nil || len(entries) != 1 {
		t.Fatalf("pool after run 1 = %d entries (%v)", len(entries), err)
	}
	first := entries[0]
	if first.CandidateID != "baseline" || len(first.Rows) != 3 || first.Rows[0] != 1 || first.Rows[2] != 1 {
		t.Errorf("run 1 entry = %+v, want baseline with rows [1 1 1]", first)
	}
	wantIDs := []string{"s1", "s2", "s3"}
	if !slices.Equal(first.SampleIDs, wantIDs) {
		t.Errorf("run 1 sample ids = %v, want %v", first.SampleIDs, wantIDs)
	}
	for i, input := range []string{"甲", "乙", "丙"} {
		if first.InputHashes[i] != core.HashInput(input) {
			t.Errorf("run 1 input hash %d mismatch", i)
		}
	}
	// The manifest stays clean when no warning fired (omitempty).
	var mf map[string]any
	loadJSONFile(t, filepath.Join(outDir, first.RunID, "manifest.json"), &mf)
	if _, ok := mf["pool_warning"]; ok {
		t.Errorf("run 1 manifest carries pool_warning = %v, want omitted", mf["pool_warning"])
	}

	// Run 2: same dataset (double-consistent set) over degraded answers
	// → the paired CI regresses, the warning hits stderr and the
	// manifest, and the entry appends.
	code, out, errOut = runCliVerbose(t, base...)
	if code != 0 {
		t.Fatalf("run 2 exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "候选池退化预警") || !strings.Contains(errOut, "退化超过噪声区间") {
		t.Errorf("run 2 stderr misses the degradation warning:\n%s", errOut)
	}
	entries, err = store.Entries(taskKey)
	if err != nil || len(entries) != 2 {
		t.Fatalf("pool after run 2 = %d entries (%v)", len(entries), err)
	}
	second := entries[1]
	if len(second.Rows) != 3 || second.Rows[0] != 0 || second.Rows[2] != 0 {
		t.Errorf("run 2 rows = %v, want [0 0 0]", second.Rows)
	}
	if !slices.Equal(second.SampleIDs, first.SampleIDs) || !slices.Equal(second.InputHashes, first.InputHashes) {
		t.Error("run 2 must share the run 1 sample set (IDs and hashes)")
	}
	// The second run's manifest records the warning; the exit contract
	// is untouched (预警不进门禁). Manifests are matched to runs by the
	// pool entries' RunID — same-second run ids sort by random hex, so
	// directory order is not creation order.
	manifestOf := func(runID string) map[string]any {
		t.Helper()
		var m map[string]any
		loadJSONFile(t, filepath.Join(outDir, runID, "manifest.json"), &m)
		return m
	}
	m1 := manifestOf(entries[0].RunID)
	m2 := manifestOf(entries[1].RunID)
	if _, ok := m1["pool_warning"]; ok {
		t.Errorf("run 1 manifest grew a pool_warning: %v", m1["pool_warning"])
	}
	warning, _ := m2["pool_warning"].(string)
	if !strings.HasPrefix(warning, "regressed:") || !strings.Contains(warning, "CI=") {
		t.Errorf("run 2 pool_warning = %q, want a regressed CI record", warning)
	}
	if !strings.Contains(warning, entries[0].RunID) {
		t.Errorf("warning %q does not cite the compared run %s", warning, entries[0].RunID)
	}
}

// poolZCSamplesB is the second synthesis answer: SAME deterministic ids
// (a1..a3) over different content — the exact pseudo-pairing shape the
// input-hash half of the same-set criterion exists to reject.
const poolZCSamplesB = `{"samples":[
 {"id":"a1","input":"恶寒轻重，咯痰色白，量多易咳。","expected":"痰湿蕴肺","split":"train"},
 {"id":"a2","input":"脘腹胀满，嗳腐吞酸，苔厚腻。","expected":"饮食积滞","split":"dev"},
 {"id":"a3","input":"喘咳气涌，痰黄质稠。","expected":"痰热壅肺","split":"train"}
]}`

// poolZCScript extends the probe table over both synthesis sets: a1
// dead easy, a2 discriminating (p1 correct / p2 wrong), a3 dead hard —
// so each run keeps exactly its own a2 with different content.
var poolZCScript = []zcEval{
	{"恶寒发热，无汗，脉浮紧。", "风寒束表", 1, 1},
	{"心烦不寐，腰膝酸软，脉细数。", "心肾不交", 1, 0},
	{"发热微恶风寒，咽痛，脉浮数。", "风热犯表", 0, 0},
	{"恶寒轻重，咯痰色白，量多易咳。", "痰湿蕴肺", 1, 1},
	{"脘腹胀满，嗳腐吞酸，苔厚腻。", "饮食积滞", 1, 0},
	{"喘咳气涌，痰黄质稠。", "痰热壅肺", 0, 0},
}

// TestRunPoolZeroConfigTwoRunsMeanOnlyHint（用例 ⑤ 零配置腿）: two
// zero-config runs over the same prompt (same task key) synthesize
// different sample sets — the comparison degrades to the mean-only
// hint instead of faking a paired CI over the shared a1..a3 ids.
func TestRunPoolZeroConfigTwoRunsMeanOnlyHint(t *testing.T) {
	var synthCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		var content string
		switch {
		case strings.Contains(body, harness.MarkerSpec):
			content = zcSpec
		case strings.Contains(body, harness.MarkerSamples):
			if synthCalls.Add(1) == 1 {
				content = zcSamples
			} else {
				content = poolZCSamplesB
			}
		case strings.Contains(body, harness.MarkerProbes):
			content = zcProbes
		case strings.Contains(body, harness.MarkerRepair):
			t.Errorf("unexpected repair call: %.200s", body)
			content = "{}"
		case strings.Contains(body, engine.MarkerReflect):
			content = zcHypotheses
		case strings.Contains(body, engine.MarkerRewrite),
			strings.Contains(body, engine.MarkerMerge),
			strings.Contains(body, engine.MarkerFresh):
			content = zcMutation
		case strings.Contains(body, engine.MarkerHypRepair),
			strings.Contains(body, engine.MarkerCandFix):
			t.Errorf("unexpected engine repair call: %.200s", body)
			content = "{}"
		default:
			content = "无法辨证"
			for _, sc := range poolZCScript {
				if !strings.Contains(body, sc.input) {
					continue
				}
				score := 1
				switch {
				case strings.Contains(body, "变体甲"):
					score = sc.p1
				case strings.Contains(body, "变体乙"):
					score = sc.p2
				}
				if score == 1 {
					content = sc.expected
				}
				break
			}
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	outDir := filepath.Join(t.TempDir(), "runs")
	prompt := "从中医医案文本判断证候"
	args := []string{prompt,
		"--base-url", srv.URL, "--model", "jiuwei-tcm",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2"}

	// Run 1: empty pool, no comparison line.
	code, out, errOut := runCliVerbose(t, args...)
	if code != 0 {
		t.Fatalf("run 1 exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if strings.Contains(errOut, "跨 run 样本集不同") || strings.Contains(errOut, "候选池退化预警") {
		t.Errorf("run 1 must not compare (empty pool), stderr:\n%s", errOut)
	}

	// Run 2: different synth content under the same deterministic ids →
	// the mean-only hint, never a paired warning.
	code, out, errOut = runCliVerbose(t, args...)
	if code != 0 {
		t.Fatalf("run 2 exit = %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(errOut, "跨 run 样本集不同，仅均值参考") {
		t.Errorf("run 2 stderr misses the mean-only hint:\n%s", errOut)
	}
	if strings.Contains(errOut, "候选池退化预警") {
		t.Errorf("different sets must not run the paired CI, stderr:\n%s", errOut)
	}

	store := pool.NewPoolStore(poolRoot(outDir))
	taskKey := taskKeyDefault(prompt, "")
	entries, err := store.Entries(taskKey)
	if err != nil || len(entries) != 2 {
		t.Fatalf("pool %s = %d entries (%v), want 2 appended runs", taskKey, len(entries), err)
	}
	if !slices.Equal(entries[0].SampleIDs, entries[1].SampleIDs) {
		t.Errorf("both runs kept the deterministic id sequence: %v vs %v",
			entries[0].SampleIDs, entries[1].SampleIDs)
	}
	if entries[0].InputHashes[0] == entries[1].InputHashes[0] {
		t.Error("the two synth sets must differ in content (input hashes)")
	}
}

// TestPoolReflectWarnsAndRecordsManifest（用例 ④，池反射单元级）drives
// poolReflect directly with synthetic rows: a same-set degradation
// returns the warning, patches the manifest and appends the entry; a
// different set returns no warning (the mean-only hint goes to stderr)
// and leaves the manifest untouched.
func TestPoolReflectWarnsAndRecordsManifest(t *testing.T) {
	dir := t.TempDir()
	outDir := filepath.Join(dir, "runs")
	runDir := filepath.Join(outDir, "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(runDir, "manifest.json"), runManifest{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	store := pool.NewPoolStore(poolRoot(outDir))
	samples := []core.Sample{{ID: "s1", Input: "甲"}, {ID: "s2", Input: "乙"}}
	old := pool.Entry{
		CandidateID: "old", RunID: "run-old",
		Means:       map[string]float64{"exact_match": 1},
		Rows:        []float64{1, 1},
		InputHashes: []string{core.HashInput("甲"), core.HashInput("乙")},
		SampleIDs:   []string{"s1", "s2"},
	}
	if err := store.Append("k", old); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	o := runOptions{outDir: outDir, taskKey: "k"}

	warning := poolReflect(o, "run-1", runDir, "exact_match", samples,
		[]float64{0, 0}, map[string]float64{"exact_match": 0},
		core.Candidate{ID: "baseline", Prompt: "p"})
	if !strings.HasPrefix(warning, "regressed:") || !strings.Contains(warning, "CI=") {
		t.Fatalf("warning = %q, want a regressed CI record", warning)
	}
	var mf runManifest
	loadJSONFile(t, filepath.Join(runDir, "manifest.json"), &mf)
	if mf.PoolWarning != warning {
		t.Errorf("manifest pool_warning = %q, want %q", mf.PoolWarning, warning)
	}
	entries, err := store.Entries("k")
	if err != nil || len(entries) != 2 {
		t.Fatalf("pool = %d entries (%v), want the appended run 1", len(entries), err)
	}
	if entries[1].CandidateID != "baseline" || entries[1].Prompt != "p" ||
		!slices.Equal(entries[1].Rows, []float64{0, 0}) {
		t.Errorf("appended entry = %+v", entries[1])
	}

	// A different set (same ids, different content) degrades: no
	// warning, nothing patched, the entry still lands.
	if warning2 := poolReflect(o, "run-2", runDir, "exact_match",
		[]core.Sample{{ID: "s1", Input: "丙"}},
		[]float64{0}, map[string]float64{"exact_match": 0},
		core.Candidate{ID: "baseline", Prompt: "p2"}); warning2 != "" {
		t.Errorf("different set returned warning %q, want none", warning2)
	}
	entries, err = store.Entries("k")
	if err != nil || len(entries) != 3 {
		t.Fatalf("pool = %d entries (%v), want run 2 appended", len(entries), err)
	}
	var mf2 runManifest
	loadJSONFile(t, filepath.Join(runDir, "manifest.json"), &mf2)
	if mf2.PoolWarning != mf.PoolWarning {
		t.Errorf("manifest pool_warning changed on the mean-only path: %q → %q",
			mf.PoolWarning, mf2.PoolWarning)
	}
}

package web

// V4 endpoint tests: frontier dashboard, adopt, trace browser, diff,
// compare, report views, the usage snapshot event and the budget
// gauge.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- fixtures ---------------------------------------------------------------

// writeFrontierFixture builds a finished optimization run directory:
// frontier.json + lineage.json + report.md via engine.WriteOutputs
// (SampleIDs included), plus dataset.json and summary.json for the
// trace/compare pages.
func writeFrontierFixture(t *testing.T, runsDir, id string) string {
	t.Helper()
	runDir := filepath.Join(runsDir, id)
	lin, err := engine.LoadOrInitLineage(filepath.Join(runDir, "lineage.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []engine.LineageRecord{
		{ID: "baseline", Operator: engine.OpBaseline, Round: 0, Scores: []float64{0.5, 0.5}, PrimaryMean: 0.5, Admitted: true, CreatedAt: time.Now()},
		{ID: "g01", Parents: []string{"baseline"}, Operator: engine.OpRewrite, Round: 1, Scores: []float64{1, 0}, PrimaryMean: 0.5, Admitted: true, CreatedAt: time.Now()},
		{ID: "g02", Parents: []string{"baseline", "g01"}, Operator: engine.OpMerge, Round: 2, Scores: []float64{0.5, 0}, PrimaryMean: 0.25, Admitted: false, CreatedAt: time.Now()},
		{ID: "g03", Parents: []string{"baseline"}, Operator: engine.OpRestart, Round: 3, Scores: []float64{0.9}, PrimaryMean: 0.9, Admitted: false, Incomplete: true, CreatedAt: time.Now()},
	} {
		if err := lin.Append(rec); err != nil {
			t.Fatal(err)
		}
	}
	f := &engine.Frontier{}
	for _, m := range []engine.Member{
		{Candidate: core.Candidate{ID: "baseline", Prompt: "基线提示词\n固定第二行 {input}"}, Scores: []float64{0.5, 0.5}, Means: map[string]float64{"exact_match": 0.5}, Round: 0, Operator: engine.OpBaseline},
		{Candidate: core.Candidate{ID: "g01", Prompt: "重写版提示词\n固定第二行 {input}"}, Scores: []float64{1, 0}, Means: map[string]float64{"exact_match": 0.5}, Round: 1, Operator: engine.OpRewrite},
		{Candidate: core.Candidate{ID: "g05", Prompt: "重启版提示词\n固定第二行 {input}"}, Scores: []float64{0, 1}, Means: map[string]float64{"exact_match": 0.5}, Round: 5, Operator: engine.OpRestart},
	} {
		f.Add(m)
	}
	if err := engine.WriteOutputs(runDir, engine.ReportInputs{
		Task:    core.Task{Name: "tcm", PromptTemplate: "判断 {input}", Metrics: []string{"exact_match"}},
		Lineage: lin,
		Result: engine.Result{
			Best:   core.Candidate{ID: "baseline"},
			Rounds: 5, Reason: engine.ReasonRoundsDone,
			Usage: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 100, CompletionTokens: 60}},
		},
		SampleIDs: []string{"s1", "s2"},
	}, f); err != nil {
		t.Fatal(err)
	}
	writeJSONT(t, filepath.Join(runDir, "dataset.json"), core.Dataset{Name: "synth", Samples: []core.Sample{
		{ID: "s1", Input: "恶寒发热", Expected: "风寒束表", Split: "train"},
		{ID: "s2", Input: "心烦不寐", Expected: map[string]any{"证候": "心肾不交"}, Split: "dev"},
	}})
	writeJSONT(t, filepath.Join(runDir, "summary.json"), core.RunResult{
		RunID: id, Status: core.StatusCompleted, ExitCode: 0,
		TaskName: "tcm", CandidateID: "baseline", DatasetName: "synth",
		TotalSamples: 2, Evaluated: 2,
		MetricMeans: map[string]float64{"exact_match": 0.5},
		UsageByRole: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 100, CompletionTokens: 60}},
		StartedAt:   time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 9, 29, 10, 5, 0, 0, time.UTC),
	})
	return runDir
}

// writeTraceArtifacts adds top-level samples/calls, one evals/* unit
// and one optimizer dial to a run directory.
func writeTraceArtifacts(t *testing.T, runDir string) {
	t.Helper()
	writeJSONT(t, filepath.Join(runDir, "samples", "001-s1.json"), core.SampleTrace{
		SampleID: "s1", Role: core.RoleExecutor, Prompt: "判断 恶寒发热", Response: "风寒束表",
		Scores: map[string]float64{"exact_match": 1}, Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5}, DurationMS: 800,
	})
	writeJSONT(t, filepath.Join(runDir, "calls", "001-s1.json"), eval.CallTrace{
		Seq: 1, SampleID: "s1", Role: core.RoleExecutor,
		Request:   provider.ChatRequest{Model: "fake-model", MaxTokens: 64, Messages: []provider.Message{{Role: "user", Content: "判断 恶寒发热"}}},
		Response:  provider.ChatResponse{Content: "风寒束表", FinishReason: "stop", Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5}},
		LatencyMS: 800,
	})
	writeJSONT(t, filepath.Join(runDir, "evals", "01-g01", "samples", "001-s2.json"), core.SampleTrace{
		SampleID: "s2", Role: core.RoleExecutor, Prompt: "判断 心烦不寐", Response: "心肾不交",
		Scores: map[string]float64{"exact_match": 1}, Usage: core.Usage{PromptTokens: 11, CompletionTokens: 6}, DurationMS: 900,
	})
	writeJSONT(t, filepath.Join(runDir, "evals", "01-g01", "calls", "001-s2.json"), eval.CallTrace{
		Seq: 1, SampleID: "s2", Role: core.RoleExecutor,
		Request:   provider.ChatRequest{Model: "fake-model", MaxTokens: 64, Messages: []provider.Message{{Role: "user", Content: "判断 心烦不寐"}}},
		Response:  provider.ChatResponse{Content: "心肾不交", FinishReason: "stop", Usage: core.Usage{PromptTokens: 11, CompletionTokens: 6}},
		LatencyMS: 900,
	})
	writeJSONT(t, filepath.Join(runDir, "opt-calls", "001-reflect.json"), eval.CallTrace{
		Seq: 1, SampleID: "reflect", Role: core.RoleOptimizer,
		Request:   provider.ChatRequest{Model: "fake-model", MaxTokens: 8192, Messages: []provider.Message{{Role: "user", Content: "反思提示"}}},
		Response:  provider.ChatResponse{Content: `{"hypotheses":[]}`, FinishReason: "stop", Usage: core.Usage{PromptTokens: 50, CompletionTokens: 20}},
		LatencyMS: 1200,
	})
}

// --- ① frontier page + artifact alignment -----------------------------------

func TestFrontierArtifactAlignment(t *testing.T) {
	runDir := writeFrontierFixture(t, t.TempDir(), "20260929-130000-f1")
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		t.Fatalf("LoadFrontier: %v", err)
	}
	if got := f.SampleIDs; len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Errorf("sample_ids = %v, want [s1 s2]", got)
	}
	for _, m := range f.Members {
		if len(m.Scores) != len(f.SampleIDs) {
			t.Errorf("member %s scores len = %d, want %d (aligned with sample_ids)", m.ID, len(m.Scores), len(f.SampleIDs))
		}
		if m.Prompt == "" {
			t.Errorf("member %s has no prompt on disk", m.ID)
		}
	}
	if f.Rounds != 5 || f.Reason != engine.ReasonRoundsDone {
		t.Errorf("rounds/reason = %d/%q", f.Rounds, f.Reason)
	}
	if u := f.UsageByRole[core.RoleExecutor]; u.Total() != 160 {
		t.Errorf("usage_by_role executor total = %d, want 160", u.Total())
	}
}

func TestFrontierPageRenders(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-f2"
	writeFrontierFixture(t, runsDir, id)
	h := NewServer(runsDir, "", nil).Handler()

	res := get(t, mustURL(t, h, "/runs/"+id+"/frontier"))
	if res.StatusCode != http.StatusOK || !strings.Contains(res.ContentType, "text/html") {
		t.Fatalf("frontier page = %d %s", res.StatusCode, res.ContentType)
	}
	for _, want := range []string{
		"s1", "s2", // heat table column headers from sample_ids
		"rgba(26,127,55,", // inline server-side heat color
		"互不支配",            // pairwise non-domination annotation
		"Top-1",           // best card badge
		"未准入",             // g02 lineage verdict
		"被 baseline 支配",   // g02 dominance mark via engine.Dominates
		"预算中断·评估不完整",      // g03 incomplete mark
		"重写",              // operator label
		"独占占优",            // wins column
		"尚未采纳",            // adopt panel initial state
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("frontier page missing %q", want)
		}
	}
	if res := get(t, mustURL(t, h, "/runs/no-such/frontier")); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run frontier = %d, want 404", res.StatusCode)
	}
}

func TestFrontierLegacyArtifactDegrades(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-f3"
	runDir := filepath.Join(runsDir, id)
	// Pre-extension frontier.json: no sample_ids, no member scores or
	// prompts, no rounds/reason.
	legacy := `{"primary":"exact_match","best":{"id":"baseline","constraint_satisfied":true,"means":{"exact_match":0.5},"prompt":"旧版提示词"},"members":[{"id":"baseline","round":0,"operator":"baseline","primary_mean":0.5,"wins":0}],"generated_at":"2026-09-29T10:00:00Z"}`
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "frontier.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	res := get(t, mustURL(t, NewServer(runsDir, "", nil).Handler(), "/runs/"+id+"/frontier"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("legacy frontier page = %d, want 200 (degrade, not 500): %s", res.StatusCode, res.Body)
	}
	for _, want := range []string{"旧版产物", "热力表不可用"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("legacy page missing degrade note %q", want)
		}
	}
}

// --- ② adopt ----------------------------------------------------------------

func TestAdoptRoundTripIdempotentAndSwitch(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-a1"
	runDir := writeFrontierFixture(t, runsDir, id)
	h := NewServer(runsDir, "", nil).Handler()

	adopt := func(candidate string) bodyResponse {
		return postForm(t, h, "/runs/"+id+"/adopt", url.Values{"candidate": {candidate}})
	}
	res := adopt("g01")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "已采纳") || !strings.Contains(res.Body, "g01") {
		t.Fatalf("adopt g01 = %d: %.300s", res.StatusCode, res.Body)
	}
	// Idempotent: same candidate again stays 200 and keeps the verdict.
	if res := adopt("g01"); res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "已采纳") {
		t.Fatalf("re-adopt g01 = %d: %.300s", res.StatusCode, res.Body)
	}
	assertAdopted(t, runDir, "g01")

	// Switching overwrites the artifact.
	if res := adopt("g05"); res.StatusCode != http.StatusOK {
		t.Fatalf("switch to g05 = %d: %s", res.StatusCode, res.Body)
	}
	assertAdopted(t, runDir, "g05")

	// Unknown candidate: 404, artifact untouched.
	if res := adopt("nope"); res.StatusCode != http.StatusNotFound {
		t.Errorf("adopt unknown = %d, want 404", res.StatusCode)
	}
	assertAdopted(t, runDir, "g05")
}

func assertAdopted(t *testing.T, runDir, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(runDir, "adopted.json"))
	if err != nil {
		t.Fatalf("read adopted.json: %v", err)
	}
	var a adoptedFile
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatalf("decode adopted.json: %v", err)
	}
	if a.CandidateID != want {
		t.Errorf("adopted candidate = %q, want %q", a.CandidateID, want)
	}
	if a.Prompt == "" {
		t.Error("adopted.json carries no prompt")
	}
}

// TestAdoptConcurrentDoubleClick pins the adopt write lock: htmx double
// clicks arrive concurrently and must serialize (all 200, artifact
// intact) instead of colliding on the atomic rename.
func TestAdoptConcurrentDoubleClick(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-a2"
	runDir := writeFrontierFixture(t, runsDir, id)
	ts := httptest.NewServer(NewServer(runsDir, "", nil).Handler())
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := range 16 {
		wg.Go(func() {
			form := url.Values{"candidate": {"g01"}}
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/runs/"+id+"/adopt", strings.NewReader(form.Encode()))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			res.Body.Close()
			codes[i] = res.StatusCode
		})
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("adopt %d status = %d, want 200 (writes must serialize)", i, code)
		}
	}
	assertAdopted(t, runDir, "g01")
}

func TestAdoptLiveBroadcastsSSE(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-a3"
	writeFrontierFixture(t, runsDir, id)
	bus := NewBus()
	h := NewServer(runsDir, "", bus).Handler()

	ch, cancel := bus.Subscribe()
	defer cancel()
	if res := postForm(t, h, "/runs/"+id+"/adopt", url.Values{"candidate": {"g01"}}); res.StatusCode != http.StatusOK {
		t.Fatalf("live adopt = %d: %s", res.StatusCode, res.Body)
	}
	ev := recv(t, ch)
	if ev.Type != EventCandidateAdopted || ev.RunID != id {
		t.Fatalf("bus event = %+v, want candidate_adopted for %s", ev, id)
	}
	if ev.Detail["candidate"] != "g01" {
		t.Errorf("event candidate = %v, want g01", ev.Detail["candidate"])
	}
}

// --- ③ trace browser ----------------------------------------------------------

func TestTraceBrowser(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-t1"
	runDir := writeFrontierFixture(t, runsDir, id)
	writeTraceArtifacts(t, runDir)
	h := NewServer(runsDir, "", nil).Handler()

	// Top level: units enumerated, baseline samples listed.
	res := get(t, mustURL(t, h, "/runs/"+id+"/trace"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("trace page = %d", res.StatusCode)
	}
	for _, want := range []string{"顶层（baseline/手动模式）", "01-g01", "opt-calls（优化侧调用）", "s1", "001-s1.json"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("trace top level missing %q", want)
		}
	}

	// evals/* unit: sample table + dataset.json join in the detail.
	res = get(t, mustURL(t, h, "/runs/"+id+"/trace?unit=01-g01"))
	if !strings.Contains(res.Body, "s2") {
		t.Errorf("eval unit page missing sample s2: %.300s", res.Body)
	}
	res = get(t, mustURL(t, h, "/runs/"+id+"/trace?unit=01-g01&kind=samples&file=001-s2.json"))
	for _, want := range []string{"心烦不寐", "心肾不交", "输入（dataset.json）", "期望（dataset.json）", "判断 心烦不寐"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("sample detail missing %q", want)
		}
	}

	// Top-level call detail: request messages and finish_reason.
	res = get(t, mustURL(t, h, "/runs/"+id+"/trace?unit=&kind=calls&file=001-s1.json"))
	for _, want := range []string{"请求消息", "[user]", "判断 恶寒发热", "finish_reason", "stop"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("call detail missing %q", want)
		}
	}

	// opt-calls unit.
	res = get(t, mustURL(t, h, "/runs/"+id+"/trace?unit=opt-calls&file=001-reflect.json"))
	for _, want := range []string{"reflect", "反思提示", "optimizer"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("opt call detail missing %q", want)
		}
	}

	// Path traversal rejected before any filesystem access.
	for _, bad := range []string{
		"/trace?unit=..%2F..%2Fetc",
		"/trace?file=..%2F..%2Fsummary.json",
		"/trace?unit=01-g01&kind=samples&file=..%2F..%2F..%2Ffrontier.json",
	} {
		if res := get(t, mustURL(t, h, "/runs/"+id+bad)); res.StatusCode != http.StatusNotFound {
			t.Errorf("traversal %q = %d, want 404", bad, res.StatusCode)
		}
	}
}

// --- ④ diff -------------------------------------------------------------------

func TestDiffPage(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-d1"
	writeFrontierFixture(t, runsDir, id)
	h := NewServer(runsDir, "", nil).Handler()

	res := get(t, mustURL(t, h, "/runs/"+id+"/diff?a=baseline&b=g01"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("diff = %d: %s", res.StatusCode, res.Body)
	}
	for _, want := range []string{
		"基线提示词", "重写版提示词", // deleted / added lines
		"固定第二行 {input}", // common line
		"+1 新增", "-1 删除", "1 相同",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("diff page missing %q", want)
		}
	}

	// a == b: everything same, no add/del.
	res = get(t, mustURL(t, h, "/runs/"+id+"/diff?a=g01&b=g01"))
	if !strings.Contains(res.Body, "+0 新增") || !strings.Contains(res.Body, "-0 删除") {
		t.Errorf("self diff shows changes: %s", truncateBody(res.Body))
	}
	if strings.Contains(res.Body, `class="add"`) || strings.Contains(res.Body, `class="del"`) {
		t.Errorf("self diff has add/del rows: %s", truncateBody(res.Body))
	}

	// Missing candidate: 404.
	if res := get(t, mustURL(t, h, "/runs/"+id+"/diff?a=nope&b=g01")); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown candidate diff = %d, want 404", res.StatusCode)
	}
}

func truncateBody(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// --- ⑤ compare ------------------------------------------------------------------

func TestComparePage(t *testing.T) {
	runsDir := t.TempDir()
	idA := "20260929-130000-c1"
	writeFrontierFixture(t, runsDir, idA)
	idB := "20260929-140000-c2"
	writeJSONT(t, filepath.Join(runsDir, idB, "summary.json"), core.RunResult{
		RunID: idB, Status: core.StatusCompleted, ExitCode: 0,
		TaskName: "tcm", CandidateID: "baseline", DatasetName: "synth",
		TotalSamples: 2, Evaluated: 2,
		MetricMeans: map[string]float64{"exact_match": 0.75, "f1": 0.6},
		UsageByRole: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 50, CompletionTokens: 50}},
		StartedAt:   time.Date(2026, 9, 29, 14, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 9, 29, 14, 3, 0, 0, time.UTC),
	})

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/compare?a="+idA+"&b="+idB))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("compare = %d: %s", res.StatusCode, res.Body)
	}
	for _, want := range []string{
		"0.5000", "0.7500", "0.2500", // metric cells and Δ (sign form asserted separately below)
		"160", "100", // per-role token totals
		idA, idB, // selects carry both runs
		"—", // B has no frontier: Top-1 dash
	} {
		if !strings.Contains(res.Body, want) {
			start := max(strings.Index(res.Body, "指标均值"), 0)
			t.Errorf("compare page missing %q; metrics section: %s", want, res.Body[start:min(start+700, len(res.Body))])
		}
	}
	if strings.Contains(res.Body, "两侧均无 frontier") {
		t.Error("compare claims both sides lack frontier though A has one")
	}

	// Empty runs dir: empty state, not a 500.
	res = get(t, mustURL(t, NewServer(t.TempDir(), "", nil).Handler(), "/compare"))
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Body, "暂无历史 run") {
		t.Errorf("empty compare = %d: %s", res.StatusCode, truncateBody(res.Body))
	}
}

// TestDeltaOrDash pins the Δ formatter independently of the template
// pipeline (html/template encodes a leading plus sign as &#43;).
func TestDeltaOrDash(t *testing.T) {
	if got := deltaOrDash(true, true, 0.25); got != "+0.2500" {
		t.Errorf("deltaOrDash(true,true,0.25) = %q", got)
	}
	if got := deltaOrDash(true, false, 0.25); got != "—" {
		t.Errorf("deltaOrDash(true,false,·) = %q, want dash", got)
	}
}

// --- ⑥ report views ------------------------------------------------------------

// reportFixture mirrors the real generator's shape, including the
// early-closing fence hazard: the Top-1 prompt contains a ``` line, so
// the generator's outer fence closes early, the prompt tail spills as
// plain lines, and the intended closing fence opens an unterminated
// block that swallows the rest of the report.
const reportFixture = `# 提示词优化报告

## 概览

- 优化轮数：5（终止原因：rounds_done）
- 预算消耗（分角色）：
  - executor：prompt=100，completion=60，合计=160

| 候选 | 轮次 | exact_match 均值 | 独占占优样本 |
|---|---|---|---|
| baseline | 0 | 0.5000 | 0 |

## 最优提示词（Top-1）

提示词全文：

` + "```" + `
基线提示词
` + "```" + `
溢出行 <script>alert(1)</script>
` + "```" + `

## 前沿成员
`

func TestReportEndpoints(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-r1"
	runDir := filepath.Join(runsDir, id)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "report.md"), []byte(reportFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewServer(runsDir, "", nil).Handler()

	// In-site view.
	res := get(t, mustURL(t, h, "/runs/"+id+"/report"))
	if res.StatusCode != http.StatusOK || !strings.Contains(res.ContentType, "text/html") {
		t.Fatalf("report view = %d %s", res.StatusCode, res.ContentType)
	}
	assertRenderedReport(t, res.Body)

	// Markdown download.
	res = get(t, mustURL(t, h, "/runs/"+id+"/report.md"))
	if !strings.Contains(res.ContentType, "text/markdown") {
		t.Errorf("report.md content type = %q", res.ContentType)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("report.md disposition = %q", cd)
	}
	if res.Body != reportFixture {
		t.Errorf("report.md body drifted from the on-disk artifact")
	}

	// Standalone HTML download.
	res = get(t, mustURL(t, h, "/runs/"+id+"/report.html"))
	if !strings.Contains(res.ContentType, "text/html") {
		t.Errorf("report.html content type = %q", res.ContentType)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("report.html disposition = %q", cd)
	}
	if !strings.Contains(res.Body, "<!doctype html>") || !strings.Contains(res.Body, "<style>") {
		t.Errorf("report.html is not a self-contained document")
	}
	assertRenderedReport(t, res.Body)

	if res := get(t, mustURL(t, h, "/runs/no-such/report")); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing report = %d, want 404", res.StatusCode)
	}
}

// assertRenderedReport pins the tolerant markdown rendering: escaped
// spill from an early-closed fence, flattened nested list, pipe table,
// and content preservation inside the unterminated trailing fence.
func assertRenderedReport(t *testing.T, body string) {
	t.Helper()
	for _, want := range []string{
		"<h1>提示词优化报告</h1>",
		"<h2>概览</h2>",
		"<li>executor：prompt=100，completion=60，合计=160</li>", // nested item flattened
		"<th>候选</th>", "<td>0.5000</td>", // pipe table
		"溢出行",                                   // spilled prompt tail kept
		"&lt;script&gt;alert(1)&lt;/script&gt;", // escaped, no injection
		"前沿成员",                                  // swallowed heading still rendered
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered report missing %q", want)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("rendered report contains a raw <script> tag")
	}
}

func TestRenderMarkdownTolerant(t *testing.T) {
	// Early fence close + unterminated fence at EOF: both keep content.
	src := "```\n外层围栏\n```\n溢出段落\n\n```\n未闭合围栏内容\n"
	html := renderMarkdownHTML(src)
	for _, want := range []string{"外层围栏", "溢出段落", "未闭合围栏内容"} {
		if !strings.Contains(html, want) {
			t.Errorf("tolerant render dropped %q: %s", want, html)
		}
	}
	if got := parseMarkdown("- a\n  - b\n"); len(got) != 1 || got[0].Kind != "list" || len(got[0].Lines) != 2 {
		t.Errorf("nested list not flattened: %+v", got)
	}
}

// --- ⑦ usage snapshot event ------------------------------------------------------

func TestNewUsageEventPayload(t *testing.T) {
	b := eval.NewBudget(1000, 10)
	b.TryAcquireEval()
	b.TryAcquireEval()
	b.RecordUsage(core.RoleExecutor, core.Usage{PromptTokens: 10, CompletionTokens: 5})
	b.RecordUsage(core.RoleOptimizer, core.Usage{PromptTokens: 7, CompletionTokens: 3})

	ev := engine.NewUsageEvent(b, "run-1", "probe")
	if ev.Type != engine.EventUsage || ev.RunID != "run-1" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Detail["stage"] != "probe" || ev.Detail["evals"] != int64(2) {
		t.Errorf("detail = %+v", ev.Detail)
	}
	usage, ok := ev.Detail["usage_by_role"].(map[core.Role]core.Usage)
	if !ok {
		t.Fatalf("usage_by_role type = %T", ev.Detail["usage_by_role"])
	}
	if usage[core.RoleExecutor].Total() != 15 || usage[core.RoleOptimizer].Total() != 10 {
		t.Errorf("usage payload = %+v, want executor 15 / optimizer 10 totals", usage)
	}

	// Wire shape: after the events.jsonl roundtrip both roles survive.
	wire, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back eval.Event
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatal(err)
	}
	roles, _ := back.Detail["usage_by_role"].(map[string]any)
	exec, _ := roles["executor"].(map[string]any)
	if exec == nil || exec["prompt_tokens"] != float64(10) {
		t.Errorf("wire usage_by_role executor = %#v", roles["executor"])
	}
	if back.Detail["evals"] != float64(2) {
		t.Errorf("wire evals = %#v", back.Detail["evals"])
	}
}

// --- ⑧ terminal budget gauge -------------------------------------------------------

func TestRunDetailBudgetGauge(t *testing.T) {
	runsDir := t.TempDir()

	// Snapshot-backed gauge: evals come from the last usage event.
	id := "20260929-130000-g1"
	runDir := filepath.Join(runsDir, id)
	writeJSONT(t, filepath.Join(runDir, "summary.json"), core.RunResult{
		RunID: id, Status: core.StatusCompleted, ExitCode: 0,
		TotalSamples: 3, Evaluated: 2, Undispatched: 1,
		UsageByRole: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 20, CompletionTokens: 10}},
		StartedAt:   time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC), FinishedAt: time.Now(),
	})
	writeJSONT(t, filepath.Join(runDir, "manifest.json"), map[string]any{
		"budget_tokens": 100, "budget_evals": 10,
	})
	events := `{"type":"sample_done","sample_id":"s1","response":"x"}
{"type":"usage","time":"2026-09-29T13:00:05Z","run_id":"` + id + `","detail":{"stage":"terminal","evals":7,"usage_by_role":{"executor":{"prompt_tokens":20,"completion_tokens":10}}}}
`
	if err := os.WriteFile(filepath.Join(runDir, "events.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/runs/"+id))
	for _, want := range []string{
		"executor：prompt=20 completion=10 total=30", // usage from run.json
		"已消耗 7",                    // evals from the last usage snapshot
		`<td class="mono">70</td>`, // token headroom 100-30
		`<td class="mono">3</td>`,  // eval headroom 10-7
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("gauge missing %q", want)
		}
	}
	if strings.Contains(res.Body, "近似") {
		t.Error("snapshot-backed gauge shows the approximation note")
	}

	// Legacy run without usage events: Total−Undispatched approximation.
	id2 := "20260929-130000-g2"
	writeJSONT(t, filepath.Join(runsDir, id2, "summary.json"), core.RunResult{
		RunID: id2, Status: core.StatusCompleted, ExitCode: 0,
		TotalSamples: 3, Evaluated: 2, Undispatched: 1,
		UsageByRole: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 8, CompletionTokens: 4}},
		StartedAt:   time.Date(2026, 9, 29, 13, 0, 0, 0, time.UTC), FinishedAt: time.Now(),
	})
	res = get(t, mustURL(t, h, "/runs/"+id2))
	if !strings.Contains(res.Body, "已消耗 2") || !strings.Contains(res.Body, "近似") {
		t.Errorf("legacy gauge missing approximation: %s", truncateBody(res.Body))
	}
}

// --- ⑨ live stream survives run_done ---------------------------------------------

func TestLiveEventsSurviveRunDone(t *testing.T) {
	bus := NewBus()
	ts := httptest.NewServer(NewServer(t.TempDir(), "", bus).Handler())
	defer ts.Close()

	payloads := make(chan []string, 1)
	go func() {
		res, err := http.Get(ts.URL + "/events")
		if err != nil {
			payloads <- nil
			return
		}
		b, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			payloads <- nil
			return
		}
		var got []string
		for line := range strings.SplitSeq(string(b), "\n") {
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				got = append(got, d)
			}
		}
		payloads <- got
	}()
	waitFor(t, func() bool { return bus.subscribers() == 1 })

	bus.Publish(eval.Event{Type: eval.EventRunDone, RunID: "r1", Status: "completed", ExitCode: 0})
	bus.Publish(eval.Event{Type: EventCandidateAdopted, RunID: "r1", Detail: map[string]any{"candidate": "g01"}})
	bus.Close()

	got := <-payloads
	if got == nil {
		t.Fatal("SSE request failed")
	}
	if len(got) != 2 {
		t.Fatalf("streamed %d events after run_done, want 2: %v", len(got), got)
	}
	var second eval.Event
	if err := json.Unmarshal([]byte(got[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.Type != EventCandidateAdopted || second.Detail["candidate"] != "g01" {
		t.Errorf("post-run_done event = %+v", second)
	}
}

// --- ⑩ serve mode keeps the new GET routes, /events stays 404 ----------------------

func TestServeModeRoutesAndNoLiveEndpoint(t *testing.T) {
	runsDir := t.TempDir()
	id := "20260929-130000-s1"
	writeFrontierFixture(t, runsDir, id)
	writeTraceArtifacts(t, filepath.Join(runsDir, id))
	h := NewServer(runsDir, "", nil).Handler()

	for _, path := range []string{
		"/runs/" + id + "/frontier",
		"/runs/" + id + "/trace",
		"/runs/" + id + "/diff?a=baseline&b=g01",
		"/runs/" + id + "/report",
		"/runs/" + id + "/report.md",
		"/runs/" + id + "/report.html",
		"/compare",
	} {
		if res := get(t, mustURL(t, h, path)); res.StatusCode != http.StatusOK {
			t.Errorf("serve mode GET %s = %d, want 200", path, res.StatusCode)
		}
	}
	if res := get(t, mustURL(t, h, "/events")); res.StatusCode != http.StatusNotFound {
		t.Errorf("/events in serve mode = %d, want 404", res.StatusCode)
	}
	// The intervention endpoint stays mounted in serve mode (POST, not
	// a live endpoint).
	if res := postForm(t, h, "/runs/"+id+"/adopt", url.Values{"candidate": {"g01"}}); res.StatusCode != http.StatusOK {
		t.Errorf("serve mode adopt = %d, want 200", res.StatusCode)
	}
}

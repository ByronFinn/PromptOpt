package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- event bus -----------------------------------------------------------

func TestBusBroadcastAndCancel(t *testing.T) {
	b := NewBus()
	ch1, cancel1 := b.Subscribe()
	ch2, _ := b.Subscribe()

	for i := range 3 {
		b.Publish(eval.Event{Type: "tick", RunID: "r", SampleID: fmt.Sprint(i)})
	}
	for i := range 3 {
		for name, ch := range map[string]<-chan eval.Event{"ch1": ch1, "ch2": ch2} {
			ev := recv(t, ch)
			if ev.SampleID != fmt.Sprint(i) {
				t.Fatalf("%s: event %d = %q", name, i, ev.SampleID)
			}
		}
	}

	cancel1()
	if n := b.subscribers(); n != 1 {
		t.Fatalf("subscribers after cancel = %d, want 1", n)
	}
	b.Publish(eval.Event{Type: "after-cancel"})
	if ev := recv(t, ch2); ev.Type != "after-cancel" {
		t.Fatalf("ch2 after cancel = %+v", ev)
	}
	select {
	case ev, ok := <-ch1:
		if ok {
			t.Fatalf("cancelled channel delivered %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("cancelled channel not closed")
	}

	b.Close()
	select {
	case _, ok := <-ch2:
		if ok {
			t.Fatal("closed bus still delivered events")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed by Close")
	}
	late, lateCancel := b.Subscribe()
	lateCancel()
	select {
	case _, ok := <-late:
		if ok {
			t.Fatal("subscribe after Close returned an open channel")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscribe after Close returned a blocking channel")
	}
}

// TestBusNeverBlocksOnSlowConsumer pins the drop-on-full contract:
// a full subscriber buffer must never stall the publisher.
func TestBusNeverBlocksOnSlowConsumer(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range subscriberBuffer + 50 {
			b.Publish(eval.Event{Type: "tick"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked on a full subscriber buffer")
	}
	cancel()
	// The buffered prefix survives; the overflow was dropped.
	n := 0
	for range ch {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("delivered %d events, want the %d buffered ones", n, subscriberBuffer)
	}
}

func recv(t *testing.T, ch <-chan eval.Event) eval.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
		return eval.Event{}
	}
}

// --- index page ----------------------------------------------------------

func TestIndexListsRunCards(t *testing.T) {
	runsDir := t.TempDir()
	writeJSONT(t, filepath.Join(runsDir, "20260928-110000-aaaa", "summary.json"), core.RunResult{
		RunID: "20260928-110000-aaaa", Status: core.StatusCompleted, ExitCode: 0,
		TaskName: "tcm_ner", CandidateID: "baseline", DatasetName: "ds", Split: "test",
		TotalSamples: 3, Evaluated: 3,
		MetricMeans: map[string]float64{"json_validator": 1, "f1": 0.6667},
		StartedAt:   time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
	})
	writeJSONT(t, filepath.Join(runsDir, "20260928-120000-bbbb", "summary.json"), core.RunResult{
		RunID: "20260928-120000-bbbb", Status: core.StatusBudgetExhausted, ExitCode: 2,
		TaskName: "tcm_ner", CandidateID: "baseline", DatasetName: "ds",
		TotalSamples: 3, Evaluated: 1, Undispatched: 2,
		StartedAt: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
	})
	if err := os.MkdirAll(filepath.Join(runsDir, "20260928-130000-cccc"), 0o755); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(NewServer(runsDir, "", nil).Handler())
	defer ts.Close()

	res := get(t, ts.URL+"/")
	if !strings.Contains(res.ContentType, "text/html") {
		t.Fatalf("content type = %q", res.ContentType)
	}
	body := res.Body
	for _, want := range []string{
		"20260928-110000-aaaa", "已完成", "json_validator=1.0000", "f1=0.6667",
		"20260928-120000-bbbb", "预算耗尽", "未派发 2",
		"20260928-130000-cccc", "未完成", "尚未产出 summary.json",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
	if strings.Contains(body, "实时事件流") {
		t.Error("serve mode rendered the live section")
	}
	// Newest run first.
	if strings.Index(body, "20260928-130000-cccc") > strings.Index(body, "20260928-110000-aaaa") {
		t.Error("runs not sorted newest first")
	}
}

func TestIndexEmptyAndLiveModes(t *testing.T) {
	runsDir := t.TempDir()

	plain := get(t, mustURL(t, NewServer(runsDir, "", nil).Handler(), "/"))
	if !strings.Contains(plain.Body, "暂无历史 run") {
		t.Errorf("empty index missing empty state: %q", plain.Body)
	}

	live := get(t, mustURL(t, NewServer(runsDir, "", NewBus()).Handler(), "/"))
	for _, want := range []string{"实时事件流", "EventSource('/events')"} {
		if !strings.Contains(live.Body, want) {
			t.Errorf("live index missing %q", want)
		}
	}
}

// --- run detail page -----------------------------------------------------

func TestRunDetailPage(t *testing.T) {
	runsDir := t.TempDir()
	runID := "20260928-140000-dddd"
	writeJSONT(t, filepath.Join(runsDir, runID, "summary.json"), core.RunResult{
		RunID: runID, Status: core.StatusFailed, ExitCode: 1,
		TaskName: "tcm_ner", CandidateID: "baseline", DatasetName: "ds",
		TotalSamples: 2, Evaluated: 2, FailedSamples: []string{"s2"},
		MetricMeans: map[string]float64{"json_validator": 0.5},
		UsageByRole: map[core.Role]core.Usage{core.RoleExecutor: {PromptTokens: 20, CompletionTokens: 10}},
		StartedAt:   time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC),
		FinishedAt:  time.Date(2026, 9, 28, 14, 0, 5, 0, time.UTC),
	})
	writeJSONT(t, filepath.Join(runsDir, runID, "samples", "s1.json"), core.SampleTrace{
		SampleID: "s1", Role: core.RoleExecutor,
		Prompt: "extract from 患者恶寒发热", Response: `{"ok": true}`,
		Scores: map[string]float64{"json_validator": 1},
		Usage:  core.Usage{PromptTokens: 10, CompletionTokens: 5}, DurationMS: 1200,
	})
	writeJSONT(t, filepath.Join(runsDir, runID, "samples", "s2.json"), core.SampleTrace{
		SampleID: "s2", Role: core.RoleExecutor,
		Prompt: "extract from 无汗而喘", Response: "",
		Scores:    map[string]float64{"json_validator": 0},
		Diagnosis: map[string]string{"json_validator": "output is not valid JSON"},
		Error:     "provider 500", Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5},
	})

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/runs/"+runID))
	for _, want := range []string{
		"（exit 1）", "s1", "s2", "json_validator=1.0000",
		"provider 500", "extract from 患者恶寒发热", "失败样本",
		"executor：prompt=20 completion=10 total=30", "事件流回放",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}

	if res := get(t, mustURL(t, h, "/runs/no-such-run")); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run status = %d, want 404", res.StatusCode)
	}
}

func TestSafeRunID(t *testing.T) {
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "a b", "a:b", "a;rm"} {
		if safeRunID(id) {
			t.Errorf("safeRunID(%q) = true", id)
		}
	}
	for _, id := range []string{"20260928-150000-ffff", "run.1", "A_b-9"} {
		if !safeRunID(id) {
			t.Errorf("safeRunID(%q) = false", id)
		}
	}
}

// --- live SSE ------------------------------------------------------------

// fakeProvider backs the engine in the wiring test.
type fakeProvider struct{}

func (fakeProvider) Chat(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{
		Content:      `{"ok": true}`,
		FinishReason: "stop",
		Usage:        core.Usage{PromptTokens: 10, CompletionTokens: 5},
	}, nil
}

// TestLiveSSEWiredToEngine drives a real eval.Engine through the bus
// into the SSE endpoint and asserts the streamed event sequence.
func TestLiveSSEWiredToEngine(t *testing.T) {
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
		var got []string
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				got = append(got, strings.TrimPrefix(line, "data: "))
			}
		}
		_ = res.Body.Close()
		payloads <- got
	}()

	// The bus has no replay: wait until the SSE client is attached.
	waitFor(t, func() bool { return bus.subscribers() == 1 })

	runDir := t.TempDir()
	engine := &eval.Engine{
		RunID: "20260928-160000-eeee", RunDir: runDir,
		Model: "fake-model", MaxTokens: 64, Workers: 1,
		Metrics:  []string{"json_validator"},
		Budget:   eval.NewBudget(0, 0),
		Provider: fakeProvider{},
		OnEvent:  bus.Publish,
	}
	samples := []core.Sample{
		{ID: "s1", Input: "患者恶寒发热", Expected: map[string]any{"ok": true}, Split: "test"},
		{ID: "s2", Input: "无汗而喘", Expected: map[string]any{"ok": true}, Split: "test"},
	}
	res, err := engine.Run(context.Background(), core.Candidate{ID: "c1", Prompt: "extract {input}"}, samples)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("engine.Run = %+v, %v", res, err)
	}
	bus.Close()

	got := <-payloads
	if got == nil {
		t.Fatal("SSE request failed")
	}
	wantTypes := []string{
		eval.EventRunStart,
		eval.EventSampleStart, eval.EventSampleDone,
		eval.EventSampleStart, eval.EventSampleDone,
		eval.EventRunDone,
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("streamed %d events, want %d: %v", len(got), len(wantTypes), got)
	}
	for i, want := range wantTypes {
		var ev eval.Event
		if err := json.Unmarshal([]byte(got[i]), &ev); err != nil {
			t.Fatalf("event %d is not JSON: %v (%q)", i, err, got[i])
		}
		if ev.Type != want {
			t.Errorf("event %d type = %q, want %q", i, ev.Type, want)
		}
		if ev.RunID != engine.RunID {
			t.Errorf("event %d run id = %q", i, ev.RunID)
		}
	}
	// sample_done carries the scoring payload.
	var done eval.Event
	if err := json.Unmarshal([]byte(got[2]), &done); err != nil {
		t.Fatal(err)
	}
	if done.SampleID != "s1" || done.Scores["json_validator"] != 1 || done.Usage == nil {
		t.Errorf("first sample_done = %+v", done)
	}
}

// TestSSEClientDisconnectCleansUp cancels the request context and
// asserts the subscriber is removed from the bus.
func TestSSEClientDisconnectCleansUp(t *testing.T) {
	bus := NewBus()
	ts := httptest.NewServer(NewServer(t.TempDir(), "", bus).Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
	waitFor(t, func() bool { return bus.subscribers() == 1 })

	cancel()
	waitFor(t, func() bool { return bus.subscribers() == 0 })
}

// TestServeModeHasNoLiveEndpoint pins the read-only contract of serve.
func TestServeModeHasNoLiveEndpoint(t *testing.T) {
	h := NewServer(t.TempDir(), "", nil).Handler()
	if res := get(t, mustURL(t, h, "/events")); res.StatusCode != http.StatusNotFound {
		t.Errorf("/events in serve mode = %d, want 404", res.StatusCode)
	}
}

// --- events.jsonl replay -------------------------------------------------

func TestEventsReplay(t *testing.T) {
	runsDir := t.TempDir()
	runID := "20260928-170000-gggg"
	events := []eval.Event{
		{Type: eval.EventRunStart, RunID: runID, Time: time.Now()},
		{Type: eval.EventSampleDone, RunID: runID, SampleID: "s1", Response: `{"ok": true}`,
			Scores: map[string]float64{"json_validator": 1}, LatencyMS: 900},
		{Type: eval.EventRunDone, RunID: runID, Status: "completed", ExitCode: 0},
	}
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sb, "%s\n", b)
	}
	if err := os.MkdirAll(filepath.Join(runsDir, runID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "events.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/runs/"+runID+"/events"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !strings.Contains(res.ContentType, "text/event-stream") {
		t.Fatalf("content type = %q", res.ContentType)
	}
	lines := strings.Split(strings.TrimSpace(res.Body), "\n")
	var data []string
	for _, l := range lines {
		if d, ok := strings.CutPrefix(l, "data: "); ok {
			data = append(data, d)
		}
	}
	if len(data) != len(events) {
		t.Fatalf("replayed %d events, want %d: %q", len(data), len(events), res.Body)
	}
	for i, want := range events {
		var got, raw eval.Event
		if err := json.Unmarshal([]byte(data[i]), &got); err != nil {
			t.Fatalf("event %d not JSON: %v", i, err)
		}
		// The replay forwards the logged bytes verbatim.
		b, _ := json.Marshal(want)
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		if got.Type != raw.Type || got.SampleID != raw.SampleID {
			t.Errorf("event %d = %+v, want %+v", i, got, raw)
		}
	}

	// Runs without events.jsonl answer 404.
	if res := get(t, mustURL(t, h, "/runs/no-such-run/events")); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing replay status = %d, want 404", res.StatusCode)
	}
}

// --- helpers -------------------------------------------------------------

type bodyResponse struct {
	StatusCode  int
	ContentType string
	Header      http.Header
	Body        string
}

// mustURL serves h on a throwaway httptest server and returns the URL
// for path.
func mustURL(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts.URL + path
}

func get(t *testing.T, url string) bodyResponse {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return bodyResponse{
		StatusCode:  res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Header:      res.Header.Clone(),
		Body:        string(b),
	}
}

func writeJSONT(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 2s")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- replay truncation regressions ----------------------------------------

// dataLines extracts the data: payloads from an SSE response body.
func dataLines(body string) []string {
	var out []string
	for l := range strings.SplitSeq(body, "\n") {
		if d, ok := strings.CutPrefix(l, "data: "); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestEventsReplayLargeLines(t *testing.T) {
	// sample_done embeds full prompt+response, so lines exceed
	// bufio's 64KB default; the replay must deliver them whole and
	// still reach run_done.
	runsDir := t.TempDir()
	runID := "20260928-180000-large"
	big := strings.Repeat("A", 70*1024)
	writeReplayRaw(t, runsDir, runID,
		`{"type":"run_start","run_id":"`+runID+`"}`,
		`{"type":"sample_done","response":"`+big+`"}`,
		`{"type":"run_done","status":"completed","exit_code":0}`)

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/runs/"+runID+"/events"))
	data := dataLines(res.Body)
	if len(data) != 3 {
		t.Fatalf("replayed %d events, want 3 (run_done included)", len(data))
	}
	if len(data[1]) <= len(big) {
		t.Errorf("large line truncated: got %d bytes, want > %d", len(data[1]), len(big))
	}
	if !strings.Contains(data[2], "run_done") {
		t.Errorf("last event = %q, want run_done", data[2])
	}
}

func TestEventsReplayReportsTruncation(t *testing.T) {
	// A line beyond the 16MB cap must end the replay with an explicit
	// replay_error event instead of a silent stop.
	runsDir := t.TempDir()
	runID := "20260928-180000-huge"
	huge := strings.Repeat("B", replayMaxLine+1024)
	writeReplayRaw(t, runsDir, runID,
		`{"type":"run_start","run_id":"`+runID+`"}`,
		`{"type":"sample_done","response":"`+huge+`"}`,
		`{"type":"run_done","status":"completed"}`)

	h := NewServer(runsDir, "", nil).Handler()
	res := get(t, mustURL(t, h, "/runs/"+runID+"/events"))
	data := dataLines(res.Body)
	if len(data) != 2 {
		t.Fatalf("replayed %d events, want run_start + replay_error", len(data))
	}
	var last eval.Event
	if err := json.Unmarshal([]byte(data[1]), &last); err != nil {
		t.Fatalf("last event not JSON: %v", err)
	}
	if last.Type != EventReplayError || !strings.Contains(last.Error, "truncated") {
		t.Errorf("last event = %+v, want replay_error with truncation note", last)
	}
}

// writeReplayRaw writes raw events.jsonl lines for one run dir.
func writeReplayRaw(t *testing.T, runsDir, runID string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runsDir, runID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runsDir, runID, "events.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- synthesis review ------------------------------------------------------

// writeSynthFixture writes a complete synth/<id>/ artifact tree: a
// two-sample jiuwei-tcm 证候判断 task with a one-variant filter report
// and a pending interactive checkpoint.
func writeSynthFixture(t *testing.T, synthRoot, id string) {
	t.Helper()
	dir := filepath.Join(synthRoot, id)
	if err := harness.SaveManifest(dir, harness.Manifest{
		Prompt: "从中医医案文本判断证候", Model: "jiuwei-tcm",
		SynthSamples: 2, ProbeVariants: 1,
		CreatedAt: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if err := harness.SaveSpec(dir, harness.SpecFile{
		Task: core.Task{
			Name: "tcm_zhenghou", Description: "判断证候",
			PromptTemplate: "判断证候：{input}", Metrics: []string{"exact_match"},
		},
		Probes: []string{"变体甲：{input}"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := harness.SaveSamples(dir, harness.SampleFile{Samples: []core.Sample{
		{ID: "s1", Input: "恶寒发热，无汗。", Expected: "风寒束表", Split: "train"},
		{ID: "s2", Input: "心烦不寐。", Expected: map[string]any{"证候": "心肾不交"}, Split: "dev"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := harness.SaveFilterReport(dir, harness.FilterReport{
		Variants: 1, Primary: "exact_match", Thresholds: harness.DefaultThresholds(),
		PerSample: []harness.SampleVerdict{
			{ID: "s1", Scores: []float64{1}, Variance: 0, Verdict: harness.VerdictDeadEasy},
			{ID: "s2", Scores: []float64{0.5}, Variance: 0, Verdict: harness.VerdictKeep},
		},
		Kept: 1, DroppedByVerdict: map[harness.Verdict]int{harness.VerdictDeadEasy: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := harness.SaveCheckpoint(dir, harness.CheckpointState{
		Status: harness.CheckpointPending, Mode: harness.ModeInteractive, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSynthReviewPage(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-a")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	res := get(t, mustURL(t, h, "/synth/20260929-100000-a"))
	for _, want := range []string{
		"合成集审核", "tcm_zhenghou", "判断证候：{input}", "变体甲：{input}",
		"s1", "s2", "死样本·全对", "保留", "风寒束表", "0.5000",
		"等待审核", "批准并继续", "every 2s", "htmx.min.js",
	} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("review page missing %q", want)
		}
	}
	if res := get(t, mustURL(t, h, "/synth/no-such-run")); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown synth id status = %d, want 404", res.StatusCode)
	}
}

func TestSynthEditSampleRoundTrip(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-b")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	form := url.Values{
		"input":    {"五心烦热，潮热盗汗。"},
		"expected": {`{"证候": "阴虚火旺"}`},
		"split":    {"train"},
	}
	res := postForm(t, h, "/synth/20260929-100000-b/samples/s1", form)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("edit status = %d: %s", res.StatusCode, res.Body)
	}
	if !strings.Contains(res.Body, "潮热盗汗") {
		t.Errorf("rows partial missing the edited input: %.300s", res.Body)
	}
	sf, err := harness.LoadSamples(filepath.Join(synthRoot, "20260929-100000-b"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sf.Samples[0]; got.Input != "五心烦热，潮热盗汗。" || got.Split != "train" {
		t.Errorf("edited sample = %+v", got)
	}
	if m, ok := sf.Samples[0].Expected.(map[string]any); !ok || m["证候"] != "阴虚火旺" {
		t.Errorf("edited expected = %#v", sf.Samples[0].Expected)
	}
}

func TestSynthEditSampleRejectsInvalidExpected(t *testing.T) {
	synthRoot := t.TempDir()
	id := "20260929-100000-c"
	writeSynthFixture(t, synthRoot, id)
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	before, err := os.ReadFile(filepath.Join(synthRoot, id, "samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]string{"not json": "不是 JSON", "bare number": "42", "empty": ""} {
		form := url.Values{"input": {"x"}, "expected": {expected}, "split": {"train"}}
		if res := postForm(t, h, "/synth/"+id+"/samples/s1", form); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", name, res.StatusCode)
		}
	}
	after, err := os.ReadFile(filepath.Join(synthRoot, id, "samples.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("samples.json changed despite the 422s")
	}
}

func TestSynthEditSampleRejectsInvalidSplit(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-d")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	form := url.Values{"input": {"x"}, "expected": {`"y"`}, "split": {"moon"}}
	if res := postForm(t, h, "/synth/20260929-100000-d/samples/s1", form); res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid split status = %d, want 422", res.StatusCode)
	}
}

func TestSynthDeleteSample(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-e")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	res := doMethod(t, http.MethodDelete, mustURL(t, h, "/synth/20260929-100000-e/samples/s2"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %s", res.StatusCode, res.Body)
	}
	if strings.Contains(res.Body, "心烦不寐") {
		t.Errorf("rows partial still shows the deleted sample: %.300s", res.Body)
	}
	sf, err := harness.LoadSamples(filepath.Join(synthRoot, "20260929-100000-e"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sf.Samples) != 1 || sf.Samples[0].ID != "s1" {
		t.Errorf("samples.json after delete = %+v", sf.Samples)
	}
}

func TestSynthApproveIsIdempotent(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-f")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	for i := range 2 {
		res := postForm(t, h, "/synth/20260929-100000-f/approve", url.Values{})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("approve #%d status = %d: %s", i+1, res.StatusCode, res.Body)
		}
		if !strings.Contains(res.Body, "已批准") {
			t.Errorf("approve #%d partial missing the approved badge: %.300s", i+1, res.Body)
		}
		if strings.Contains(res.Body, "every 2s") {
			t.Errorf("approve #%d partial still polls", i+1)
		}
	}
	st, err := harness.LoadCheckpoint(filepath.Join(synthRoot, "20260929-100000-f"))
	if err != nil || st.Status != harness.CheckpointApproved {
		t.Errorf("checkpoint = %+v (%v), want approved", st, err)
	}
}

// TestSynthConcurrentEditsSerialize pins the review write lock: htmx
// double clicks and multi-tab edits arrive as concurrent read-modify-
// write requests, which must serialize (all 200, artifact intact)
// instead of losing updates or failing on a shared temp name.
func TestSynthConcurrentEditsSerialize(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-h")
	ts := httptest.NewServer(NewServer(t.TempDir(), synthRoot, nil).Handler())
	t.Cleanup(ts.Close)

	var wg sync.WaitGroup
	codes := make([]int, 16)
	for i := range 16 {
		wg.Go(func() {
			form := url.Values{
				"input":    {fmt.Sprintf("并发编辑 %d", i)},
				"expected": {`"风寒束表"`},
				"split":    {"train"},
			}
			req, err := http.NewRequest(http.MethodPost,
				ts.URL+"/synth/20260929-100000-h/samples/s1", strings.NewReader(form.Encode()))
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
			t.Errorf("edit %d status = %d, want 200 (writes must serialize, not fail)", i, code)
		}
	}
	sf, err := harness.LoadSamples(filepath.Join(synthRoot, "20260929-100000-h"))
	if err != nil {
		t.Fatalf("samples.json after concurrent edits: %v", err)
	}
	if !strings.HasPrefix(sf.Samples[0].Input, "并发编辑") {
		t.Errorf("final input = %q, want one of the concurrent edits", sf.Samples[0].Input)
	}
}

func TestSynthStatusPartialReflectsGate(t *testing.T) {
	synthRoot := t.TempDir()
	writeSynthFixture(t, synthRoot, "20260929-100000-g")
	h := NewServer(t.TempDir(), synthRoot, nil).Handler()

	pending := get(t, mustURL(t, h, "/synth/20260929-100000-g/status"))
	if !strings.Contains(pending.Body, "等待审核") || !strings.Contains(pending.Body, "every 2s") {
		t.Errorf("pending status partial = %.300s", pending.Body)
	}
	if err := harness.SaveCheckpoint(filepath.Join(synthRoot, "20260929-100000-g"),
		harness.CheckpointState{Status: harness.CheckpointApproved, Mode: harness.ModeInteractive, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	approved := get(t, mustURL(t, h, "/synth/20260929-100000-g/status"))
	if !strings.Contains(approved.Body, "已批准") || strings.Contains(approved.Body, "every 2s") {
		t.Errorf("approved status partial = %.300s", approved.Body)
	}
}

func TestSynthRoutesAbsentWithoutSynthDir(t *testing.T) {
	// serve mode and manual runs pass an empty synthDir: the review
	// tree must 404, keeping serve read-only.
	h := NewServer(t.TempDir(), "", nil).Handler()
	if res := get(t, mustURL(t, h, "/synth/20260929-100000-a")); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /synth/{id} without synthDir = %d, want 404", res.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, mustURL(t, h, "/synth/x/approve"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := doReq(t, req); res.StatusCode != http.StatusNotFound {
		t.Errorf("POST /synth/{id}/approve without synthDir = %d, want 404", res.StatusCode)
	}
}

func TestRunDetailLinksSynthReview(t *testing.T) {
	// A zero-config run owns both a runs/<id> and a synth/<id> tree:
	// the run detail page must surface the review page link, and drop
	// it when no synth tree (or no synthDir) exists.
	runsDir, synthRoot := t.TempDir(), t.TempDir()
	runID := "20260929-110000-h"
	writeJSONT(t, filepath.Join(runsDir, runID, "summary.json"), core.RunResult{
		RunID: runID, Status: core.StatusCompleted, ExitCode: 0,
		TaskName: "tcm_zhenghou", CandidateID: "baseline", DatasetName: "synth",
		StartedAt: time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC),
	})
	writeSynthFixture(t, synthRoot, runID)

	withSynth := NewServer(runsDir, synthRoot, nil).Handler()
	res := get(t, mustURL(t, withSynth, "/runs/"+runID))
	if !strings.Contains(res.Body, `href="/synth/`+runID+`"`) {
		t.Errorf("run detail missing the synth review link:\n%.300s", res.Body)
	}

	// No synth tree for this id: no link even with a mounted synthDir.
	other := "20260929-110000-i"
	writeJSONT(t, filepath.Join(runsDir, other, "summary.json"), core.RunResult{
		RunID: other, Status: core.StatusCompleted, ExitCode: 0, StartedAt: time.Now(),
	})
	res = get(t, mustURL(t, withSynth, "/runs/"+other))
	if strings.Contains(res.Body, "合成集审核") {
		t.Errorf("run without a synth tree rendered the review link:\n%.300s", res.Body)
	}

	// serve mode (empty synthDir): never a link.
	plain := NewServer(runsDir, "", nil).Handler()
	res = get(t, mustURL(t, plain, "/runs/"+runID))
	if strings.Contains(res.Body, "合成集审核") {
		t.Errorf("serve mode rendered the review link:\n%.300s", res.Body)
	}
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values) bodyResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, mustURL(t, h, path), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doReq(t, req)
}

func doMethod(t *testing.T, method, rawURL string) bodyResponse {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return doReq(t, req)
}

func doReq(t *testing.T, req *http.Request) bodyResponse {
	t.Helper()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read %s: %v", req.URL, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", req.URL, err)
	}
	return bodyResponse{
		StatusCode:  res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Header:      res.Header.Clone(),
		Body:        string(b),
	}
}

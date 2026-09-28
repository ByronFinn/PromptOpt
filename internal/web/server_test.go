package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
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

	ts := httptest.NewServer(NewServer(runsDir, nil).Handler())
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

	plain := get(t, mustURL(t, NewServer(runsDir, nil).Handler(), "/"))
	if !strings.Contains(plain.Body, "暂无历史 run") {
		t.Errorf("empty index missing empty state: %q", plain.Body)
	}

	live := get(t, mustURL(t, NewServer(runsDir, NewBus()).Handler(), "/"))
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

	h := NewServer(runsDir, nil).Handler()
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
	ts := httptest.NewServer(NewServer(t.TempDir(), bus).Handler())
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
	ts := httptest.NewServer(NewServer(t.TempDir(), bus).Handler())
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
	h := NewServer(t.TempDir(), nil).Handler()
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

	h := NewServer(runsDir, nil).Handler()
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

	h := NewServer(runsDir, nil).Handler()
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

	h := NewServer(runsDir, nil).Handler()
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

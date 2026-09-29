// Package web serves the PromptOpt dashboard: a live SSE view of the
// running evaluation for `run --web`, and a read-only browser over
// past run artifacts for `serve`. Pages are embedded templates
// rendered server-side; the live page is a no-framework EventSource
// consumer.
package web

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// templates are parsed once; they are read-only afterwards.
var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

// subscriberBuffer bounds the per-client event backlog. Publishers
// never block on slow clients: excess events are dropped and the
// events.jsonl replay stays the source of truth.
const subscriberBuffer = 256

// replayMaxLine caps one events.jsonl line during replay; longer lines
// abort the replay with an explicit replay_error event.
const replayMaxLine = 16 << 20

// EventReplayError is a server-synthesized event marking a truncated
// events.jsonl replay (line over replayMaxLine or read failure).
const EventReplayError = "replay_error"

// Bus is the in-process event bus between the eval engine and SSE
// clients. The engine (or its fan-out adapter) calls Publish; every
// HTTP client owns one subscriber channel.
type Bus struct {
	mu     sync.Mutex
	subs   map[chan eval.Event]struct{}
	closed bool
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[chan eval.Event]struct{})}
}

// Subscribe returns a channel of future events and a cancel func that
// must be called once the consumer goes away (client disconnect).
// Subscribing after Close returns an already-closed channel.
func (b *Bus) Subscribe() (<-chan eval.Event, func()) {
	ch := make(chan eval.Event, subscriberBuffer)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch, func() {}
	}
	b.subs[ch] = struct{}{}
	return ch, func() { b.remove(ch) }
}

// remove closes and unregisters ch. Mutually exclusive with Publish,
// so a send can never race a close.
func (b *Bus) remove(ch chan eval.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; !ok {
		return
	}
	delete(b.subs, ch)
	close(ch)
}

// Publish broadcasts ev to every subscriber without blocking.
func (b *Bus) Publish(ev eval.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // drop rather than stall the engine
		}
	}
}

// Close terminates every subscriber and rejects later events.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

// subscribers reports the live subscriber count (used by tests).
func (b *Bus) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Server renders the dashboard pages and endpoints. A nil bus turns
// the live endpoints off (serve mode: history and replay only); an
// empty synthDir unmounts the synthesis review tree (runs without a
// Harness Builder phase).
type Server struct {
	runsDir  string
	synthDir string
	bus      *Bus
	// synthMu serializes synthesis artifact mutations: review handlers
	// do unlocked read-modify-write cycles, and concurrent htmx
	// requests (double click, two tabs) must not lose updates.
	synthMu sync.Mutex
	// adoptMu serializes adopted.json rewrites for the same reason as
	// synthMu (double-click adopt / two tabs switching candidates).
	adoptMu sync.Mutex
}

// NewServer returns a server rooted at runsDir, with the synthesis
// review endpoints mounted when synthDir is non-empty; bus may be nil.
func NewServer(runsDir, synthDir string, bus *Bus) *Server {
	return &Server{runsDir: runsDir, synthDir: synthDir, bus: bus}
}

// originGuard is the source check wrapped around every route: the Host
// header must be loopback (answers DNS rebinding, which targets reads),
// and non-GET requests must not be cross-site (answers CSRF on the
// write endpoints — adopt and the synth review mutations are browser
// "simple requests" that skip any CORS preflight). Cross-site shape is
// detected via Sec-Fetch-Site when present and via a mismatching Origin
// header otherwise; requests without either (curl, tests, htmx on the
// same origin) pass.
func (s *Server) originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether the Host header's host part is a
// loopback address or localhost. The port is deliberately unchecked:
// any request arriving here by definition targeted a port this server
// listens on (the Server never learns the configured one, and the
// harness runs it on random test ports).
func isLoopbackHost(hostPort string) bool {
	h := hostPort
	if hp, _, err := net.SplitHostPort(hostPort); err == nil {
		h = hp
	}
	h = strings.Trim(h, "[]")
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(h, "localhost")
}

// sameOrigin reports whether an Origin header points back at the
// request's own host: the dashboard serves plain HTTP, so comparing
// host:port is sufficient. "null" and unparsable origins are treated
// as foreign.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host != "" && strings.EqualFold(u.Host, host)
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /runs/{id}", s.handleRunDetail)
	mux.HandleFunc("GET /runs/{id}/events", s.handleRunReplay)
	mux.HandleFunc("GET /runs/{id}/frontier", s.handleFrontierPage)
	mux.HandleFunc("POST /runs/{id}/adopt", s.handleAdopt)
	mux.HandleFunc("GET /runs/{id}/trace", s.handleTracePage)
	mux.HandleFunc("GET /runs/{id}/diff", s.handleDiffPage)
	mux.HandleFunc("GET /runs/{id}/report", s.handleReportPage)
	mux.HandleFunc("GET /runs/{id}/report.md", s.handleReportMarkdown)
	mux.HandleFunc("GET /runs/{id}/report.html", s.handleReportStandalone)
	mux.HandleFunc("GET /compare", s.handleComparePage)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	s.registerSynthRoutes(mux)
	if s.bus != nil {
		mux.HandleFunc("GET /events", s.handleLiveEvents)
	}
	return s.originGuard(mux)
}

// Listen starts serving in the background; the caller owns shutdown.
func (s *Server) Listen(addr string) (*http.Server, error) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}

// ListenAndServe blocks serving until ctx is cancelled, then shuts
// down gracefully.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv, err := s.Listen(addr)
	if err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// --- index ------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	cards, err := listRunCards(s.runsDir)
	if err != nil {
		http.Error(w, "read runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "index.html", struct {
		Live bool
		Runs []runCard
	}{Live: s.bus != nil, Runs: cards})
}

// metricCell is one rendered metric value.
type metricCell struct{ Name, Value string }

// runCard is the index-page summary of one run directory.
type runCard struct {
	ID, Status, StatusClass                string
	HasSummary                             bool
	Task, Candidate, Dataset, Split        string
	Started                                string
	Evaluated, Total, Failed, Undispatched int
	ExitCode                               int
	Metrics                                []metricCell
}

// listRunCards reads every run directory, newest first. Run ids are
// timestamp-prefixed, so reverse lexicographic order is chronological.
func listRunCards(runsDir string) ([]runCard, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cards []runCard
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cards = append(cards, newRunCard(filepath.Join(runsDir, e.Name()), e.Name()))
	}
	slices.SortFunc(cards, func(a, b runCard) int { return strings.Compare(b.ID, a.ID) })
	return cards, nil
}

// newRunCard builds one card, degrading to an "unfinished" stub when
// no summary artifact decodes.
func newRunCard(runDir, id string) runCard {
	c := runCard{ID: id, Status: "未完成", StatusClass: "dim"}
	res, ok := readSummary(runDir)
	if !ok {
		return c
	}
	c.HasSummary = true
	c.Status, c.StatusClass = statusView(res.Status)
	c.Task, c.Candidate, c.Dataset, c.Split = res.TaskName, res.CandidateID, res.DatasetName, res.Split
	c.Started = res.StartedAt.Format("2006-01-02 15:04:05")
	c.Evaluated, c.Total = res.Evaluated, res.TotalSamples
	c.Failed, c.Undispatched = len(res.FailedSamples), res.Undispatched
	c.ExitCode = res.ExitCode
	c.Metrics = metricCells(res.MetricMeans)
	return c
}

// readSummary decodes summary.json, falling back to the identical
// run.json contract name.
func readSummary(runDir string) (core.RunResult, bool) {
	var res core.RunResult
	for _, name := range []string{"summary.json", "run.json"} {
		b, err := os.ReadFile(filepath.Join(runDir, name))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(b, &res); err != nil {
			continue
		}
		return res, true
	}
	return core.RunResult{}, false
}

// statusView maps a run status to a Chinese label and CSS class.
func statusView(s core.RunStatus) (string, string) {
	switch s {
	case core.StatusCompleted:
		return "已完成", "ok"
	case core.StatusFailed:
		return "失败", "bad"
	case core.StatusBudgetExhausted:
		return "预算耗尽", "warn"
	case core.StatusAborted:
		return "已中断", "warn"
	default:
		return "未完成", "dim"
	}
}

// metricCells renders metric values sorted by name.
func metricCells(values map[string]float64) []metricCell {
	names := make([]string, 0, len(values))
	for m := range values {
		names = append(names, m)
	}
	slices.Sort(names)
	cells := make([]metricCell, 0, len(names))
	for _, m := range names {
		cells = append(cells, metricCell{Name: m, Value: fmt.Sprintf("%.4f", values[m])})
	}
	return cells
}

// --- run detail ---------------------------------------------------------

// sampleView is one rendered sample trace.
type sampleView struct {
	ID, Status, StatusClass            string
	Scores, Diagnosis                  []metricCell
	Tokens, Latency                    string
	Prompt, Reasoning, Response, Error string
}

// runDetail is the run page view; it embeds the card fields.
type runDetail struct {
	runCard
	Finished      string
	FailedSamples []string
	Samples       []sampleView
	// Live marks a running run (run --web): the page subscribes /events
	// for the live budget gauge instead of relying on artifacts only.
	Live bool
	// LiveLimits feeds the live gauge's denominators (manifest limits).
	LiveLimits runManifestLimits
	// Budget is the terminal budget gauge (usage vs manifest limits);
	// nil when the run has neither summary nor manifest yet.
	Budget *budgetGauge
	// HasFrontier/HasReport gate the optimization-artifact nav links.
	HasFrontier, HasReport bool
	// SynthURL links the run's synthesis review page when a synth
	// tree exists for this id; empty in manual/serve mode.
	SynthURL string
}

// budgetRow is one gauge line: a usage figure against its limit.
type budgetRow struct {
	Label     string
	Detail    string // usage detail, e.g. "executor：prompt=20 …"
	Limit     string // "不限" or the numeric limit
	Remaining string // headroom under the limit, or "—"
	Note      string // approximation caveat, "" when exact
}

// budgetGauge is the run page budget dashboard.
type budgetGauge struct {
	Rows []budgetRow
}

// runManifestLimits is the budget-relevant subset of manifest.json.
type runManifestLimits struct {
	BudgetTokens    int64 `json:"budget_tokens"`
	BudgetEvals     int64 `json:"budget_evals"`
	BudgetOptTokens int64 `json:"budget_opt_tokens,omitempty"`
}

// readManifestLimits decodes manifest.json; a missing or malformed
// artifact means unlimited (zero limits).
func readManifestLimits(runDir string) runManifestLimits {
	var m runManifestLimits
	b, err := os.ReadFile(filepath.Join(runDir, "manifest.json"))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

// usageSnapshot is the minimal decode of one usage event line: the
// Detail payload of engine.NewUsageEvent.
type usageSnapshot struct {
	Detail struct {
		Evals       int64                    `json:"evals"`
		UsageByRole map[core.Role]core.Usage `json:"usage_by_role"`
	} `json:"detail"`
}

// lastUsageSnapshot scans events.jsonl and returns the final usage
// event's snapshot. The prefix test skips decoding the (potentially
// huge) sample_done lines.
func lastUsageSnapshot(runDir string) (usageSnapshot, bool) {
	f, err := os.Open(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		return usageSnapshot{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), replayMaxLine)
	var last usageSnapshot
	found := false
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte(`{"type":"usage"`)) {
			continue
		}
		var snap usageSnapshot
		if err := json.Unmarshal(line, &snap); err != nil {
			continue
		}
		last, found = snap, true
	}
	return last, found && sc.Err() == nil
}

// buildBudgetGauge assembles the terminal gauge: run.json usage by
// role against the manifest token limits, and consumed evaluations
// from the last usage snapshot in events.jsonl. Runs predating usage
// events fall back to the Total−Undispatched approximation (probe
// consumption of zero-config runs is invisible there — noted).
func buildBudgetGauge(runDir string, res core.RunResult, hasSummary bool) *budgetGauge {
	limits := readManifestLimits(runDir)
	g := &budgetGauge{}
	if hasSummary {
		for _, role := range []core.Role{core.RoleExecutor, core.RoleOptimizer} {
			u, ok := res.UsageByRole[role]
			if !ok {
				continue
			}
			limit := limits.BudgetTokens
			if role == core.RoleOptimizer {
				limit = limits.BudgetOptTokens
			}
			g.Rows = append(g.Rows, tokenRow(role, u, limit))
		}
	}
	evals := int64(0)
	note := ""
	snap, ok := lastUsageSnapshot(runDir)
	switch {
	case ok:
		evals = snap.Detail.Evals
	case hasSummary:
		evals = int64(res.TotalSamples - res.Undispatched)
		note = "旧 run 无 usage 快照，按 Total−Undispatched 近似（零配置 run 不含探针消耗，偏低）"
	default:
		return g
	}
	g.Rows = append(g.Rows, evalsRow(evals, limits.BudgetEvals, note))
	return g
}

// tokenRow renders one role's usage against its token limit.
func tokenRow(role core.Role, u core.Usage, limit int64) budgetRow {
	row := budgetRow{
		Label:     string(role),
		Detail:    fmt.Sprintf("%s：prompt=%d completion=%d total=%d", role, u.PromptTokens, u.CompletionTokens, u.Total()),
		Limit:     "不限",
		Remaining: "—",
	}
	if limit > 0 {
		row.Limit = fmt.Sprintf("%d", limit)
		row.Remaining = fmt.Sprintf("%d", max(limit-u.Total(), 0))
	}
	return row
}

// evalsRow renders consumed evaluations against the eval limit.
func evalsRow(used, limit int64, note string) budgetRow {
	return budgetRow{
		Label:     "评估次数",
		Detail:    fmt.Sprintf("已消耗 %d", used),
		Limit:     limitText(limit),
		Remaining: remainingText(limit, used),
		Note:      note,
	}
}

func limitText(limit int64) string {
	if limit > 0 {
		return fmt.Sprintf("%d", limit)
	}
	return "不限"
}

func remainingText(limit, used int64) string {
	if limit > 0 {
		return fmt.Sprintf("%d", max(limit-used, 0))
	}
	return "—"
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	runDir := filepath.Join(s.runsDir, id)
	if _, err := os.Stat(runDir); err != nil {
		http.NotFound(w, r)
		return
	}
	d := runDetail{runCard: newRunCard(runDir, id), Live: s.bus != nil}
	res, hasSummary := readSummary(runDir)
	if hasSummary {
		d.Finished = res.FinishedAt.Format("2006-01-02 15:04:05")
		d.FailedSamples = res.FailedSamples
	}
	if d.Live {
		d.LiveLimits = readManifestLimits(runDir)
	}
	d.Samples = readSampleViews(runDir)
	if g := buildBudgetGauge(runDir, res, hasSummary); len(g.Rows) > 0 {
		d.Budget = g
	}
	if _, err := os.Stat(filepath.Join(runDir, "frontier.json")); err == nil {
		d.HasFrontier = true
	}
	if _, err := os.Stat(filepath.Join(runDir, "report.md")); err == nil {
		d.HasReport = true
	}
	// Zero-config runs own a synth tree: surface the review page next
	// to the evaluation artifacts.
	if s.synthDir != "" {
		if _, err := os.Stat(filepath.Join(s.synthDir, id, "manifest.json")); err == nil {
			d.SynthURL = "/synth/" + id
		}
	}
	render(w, "run.html", d)
}

// readSampleViews loads samples/<id>.json traces sorted by sample id.
func readSampleViews(runDir string) []sampleView {
	dir := filepath.Join(runDir, "samples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var views []sampleView
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var t core.SampleTrace
		if err := json.Unmarshal(b, &t); err != nil {
			continue
		}
		views = append(views, newSampleView(t))
	}
	slices.SortFunc(views, func(a, b sampleView) int { return strings.Compare(a.ID, b.ID) })
	return views
}

func newSampleView(t core.SampleTrace) sampleView {
	v := sampleView{
		ID: t.SampleID, Prompt: t.Prompt, Reasoning: t.Reasoning,
		Response: t.Response, Error: t.Error, Scores: metricCells(t.Scores),
	}
	if t.Error != "" {
		v.Status, v.StatusClass = "失败", "bad"
	} else {
		v.Status, v.StatusClass = "完成", "ok"
	}
	v.Tokens = fmt.Sprintf("%d+%d", t.Usage.PromptTokens, t.Usage.CompletionTokens)
	if t.DurationMS > 0 {
		v.Latency = fmt.Sprintf("%.1fs", float64(t.DurationMS)/1000)
	}
	v.Diagnosis = diagnosisCells(t.Diagnosis)
	return v
}

// diagnosisCells renders metric diagnoses sorted by metric name.
func diagnosisCells(d map[string]string) []metricCell {
	names := make([]string, 0, len(d))
	for m := range d {
		names = append(names, m)
	}
	slices.Sort(names)
	cells := make([]metricCell, 0, len(names))
	for _, m := range names {
		cells = append(cells, metricCell{Name: m, Value: d[m]})
	}
	return cells
}

// render buffers the template so failures answer 500 cleanly.
func render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, "render "+name+": "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// --- SSE ----------------------------------------------------------------

// handleLiveEvents streams the bus as SSE until the bus closes or the
// client disconnects. run_done no longer ends the stream: with --web
// the dashboard stays up past the terminal event (adopt flow, terminal
// usage snapshots), and every page's JS closes its own EventSource.
func (s *Server) handleLiveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	ch, cancel := s.bus.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSE(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleRunReplay streams a finished run's events.jsonl as SSE.
func (s *Server) handleRunReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.runsDir, id, "events.jsonl"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	// sample_done events embed full prompt+response, so lines can far
	// exceed bufio's 64KB default; allow up to 16MB and surface read
	// errors explicitly instead of ending the stream silently.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), replayMaxLine)
	for sc.Scan() {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		flusher.Flush()
	}
	if err := sc.Err(); err != nil {
		// Replay is the reproducibility contract: tell the client the
		// log was truncated rather than dropping run_done quietly.
		_ = writeSSE(w, eval.Event{
			Type:  EventReplayError,
			Time:  time.Now(),
			Error: "events.jsonl replay truncated: " + err.Error(),
		})
		flusher.Flush()
	}
}

// writeSSE frames one event as a server-sent event.
func writeSSE(w io.Writer, ev eval.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// safeRunID accepts only run ids usable as a single path segment,
// rejecting empty, dot and separator-containing ids.
func safeRunID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return false
		}
		return true
	}) < 0
}

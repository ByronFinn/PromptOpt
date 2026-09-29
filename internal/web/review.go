package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// registerSynthRoutes mounts the synthesis review endpoints. An empty
// synthDir (serve mode, manual runs) keeps the tree unmounted.
func (s *Server) registerSynthRoutes(mux *http.ServeMux) {
	if s.synthDir == "" {
		return
	}
	mux.HandleFunc("GET /synth/{id}", s.handleSynthPage)
	mux.HandleFunc("GET /synth/{id}/rows", s.handleSynthRows)
	mux.HandleFunc("GET /synth/{id}/status", s.handleSynthStatus)
	mux.HandleFunc("POST /synth/{id}/samples/{sid}", s.handleSynthEditSample)
	mux.HandleFunc("DELETE /synth/{id}/samples/{sid}", s.handleSynthDeleteSample)
	mux.HandleFunc("POST /synth/{id}/approve", s.handleSynthApprove)
}

// synthRowView is one editable sample row of the review page.
type synthRowView struct {
	ID, Split                           string
	Verdict, VerdictLabel, VerdictClass string
	Variance, Scores, Expected, Input   string
	Kept                                bool
}

// synthReviewView backs the review page and its htmx partials.
type synthReviewView struct {
	ID           string
	Prompt       string
	Model        string
	CreatedAt    string
	SynthSamples int
	Task         core.Task
	Probes       []string
	Filtered     bool
	Thresholds   string
	Kept, Total  int
	Dropped      int
	Status       string // "", pending, approved
	StatusClass  string
	StatusLabel  string
	Rows         []synthRowView
}

func (s *Server) handleSynthPage(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review.html", v)
	}
}

// handleSynthRows re-renders the sample table after an edit or delete.
func (s *Server) handleSynthRows(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review_rows", v)
	}
}

// handleSynthStatus is polled by the page while the gate is pending.
func (s *Server) handleSynthStatus(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review_status", v)
	}
}

// handleSynthEditSample applies one row edit. The expected field must
// be strict JSON; anything else answers 422 and leaves the artifact
// untouched.
func (s *Server) handleSynthEditSample(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.synthSampleTarget(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	var expected any
	if err := json.Unmarshal([]byte(r.PostFormValue("expected")), &expected); err != nil {
		http.Error(w, "expected 不是合法 JSON: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	split := r.PostFormValue("split")
	if !slices.Contains(core.ValidSplits, split) {
		http.Error(w, fmt.Sprintf("split %q 非法（仅允许 train/dev/test）", split), http.StatusUnprocessableEntity)
		return
	}
	// The read-modify-write cycle runs under the synth write lock so
	// concurrent htmx requests serialize instead of losing updates.
	s.synthMu.Lock()
	defer s.synthMu.Unlock()
	sf, err := harness.LoadSamples(dir)
	if err != nil {
		http.Error(w, "读取 samples.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sid := r.PathValue("sid")
	idx := slices.IndexFunc(sf.Samples, func(x core.Sample) bool { return x.ID == sid })
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	sf.Samples[idx].Input = r.PostFormValue("input")
	sf.Samples[idx].Expected = expected
	sf.Samples[idx].Split = split
	if err := harness.SaveSamples(dir, sf); err != nil {
		// Whole-set validation failed: nothing was written.
		http.Error(w, "样本校验失败: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review_rows", v)
	}
}

// handleSynthDeleteSample removes one row from samples.json.
func (s *Server) handleSynthDeleteSample(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.synthSampleTarget(w, r)
	if !ok {
		return
	}
	s.synthMu.Lock()
	defer s.synthMu.Unlock()
	sf, err := harness.LoadSamples(dir)
	if err != nil {
		http.Error(w, "读取 samples.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	sid := r.PathValue("sid")
	idx := slices.IndexFunc(sf.Samples, func(x core.Sample) bool { return x.ID == sid })
	if idx < 0 {
		http.NotFound(w, r)
		return
	}
	sf.Samples = slices.Delete(sf.Samples, idx, idx+1)
	if err := harness.SaveSamples(dir, sf); err != nil {
		http.Error(w, "样本校验失败: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review_rows", v)
	}
}

// handleSynthApprove flips the gate pending→approved. It is idempotent:
// approving twice leaves an approved artifact and 200s both times.
func (s *Server) handleSynthApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.synthDir, id)
	s.synthMu.Lock()
	defer s.synthMu.Unlock()
	cp, err := harness.LoadCheckpoint(dir)
	if err != nil {
		http.Error(w, "checkpoint.json 尚未创建（合成流程未到达检查点）", http.StatusNotFound)
		return
	}
	if cp.Status == harness.CheckpointPending {
		cp.Status = harness.CheckpointApproved
		cp.UpdatedAt = time.Now()
		if err := harness.SaveCheckpoint(dir, cp); err != nil {
			http.Error(w, "写 checkpoint.json: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if v, ok := s.loadReview(w, r); ok {
		render(w, "review_status", v)
	}
}

// synthSampleTarget validates the path pair and returns the synth dir.
func (s *Server) synthSampleTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, sid := r.PathValue("id"), r.PathValue("sid")
	if !safeRunID(id) || !safeRunID(sid) {
		http.NotFound(w, r)
		return "", false
	}
	return filepath.Join(s.synthDir, id), true
}

// loadReview assembles the review view, answering 404/500 itself.
func (s *Server) loadReview(w http.ResponseWriter, r *http.Request) (*synthReviewView, bool) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return nil, false
	}
	dir := filepath.Join(s.synthDir, id)
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	manifest, err := harness.LoadManifest(dir)
	if err != nil {
		http.Error(w, "读取 manifest.json: "+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	v := &synthReviewView{
		ID:           id,
		Prompt:       manifest.Prompt,
		Model:        manifest.Model,
		SynthSamples: manifest.SynthSamples,
		CreatedAt:    manifest.CreatedAt.Format("2006-01-02 15:04:05"),
	}
	if spec, err := harness.LoadSpec(dir); err == nil {
		v.Task, v.Probes = spec.Task, spec.Probes
	}
	var report *harness.FilterReport
	if rp, err := harness.LoadFilterReport(dir); err == nil {
		report = &rp
		v.Filtered = true
		v.Kept = rp.Kept
		v.Thresholds = fmt.Sprintf("[%.2f, %.2f]", rp.Thresholds.Low, rp.Thresholds.High)
	}
	if sf, err := harness.LoadSamples(dir); err == nil {
		v.Total = len(sf.Samples)
		v.Rows = buildRows(sf.Samples, report)
	}
	if v.Filtered {
		v.Dropped = v.Total - v.Kept
	}
	if cp, err := harness.LoadCheckpoint(dir); err == nil {
		v.Status = string(cp.Status)
	}
	v.StatusClass, v.StatusLabel = "dim", "未到检查点"
	switch harness.CheckpointStatus(v.Status) {
	case harness.CheckpointPending:
		v.StatusClass, v.StatusLabel = "warn", "等待审核"
	case harness.CheckpointApproved:
		v.StatusClass, v.StatusLabel = "ok", "已批准"
	}
	return v, true
}

func buildRows(samples []core.Sample, report *harness.FilterReport) []synthRowView {
	var verdicts map[string]harness.SampleVerdict
	if report != nil {
		verdicts = make(map[string]harness.SampleVerdict, len(report.PerSample))
		for _, pv := range report.PerSample {
			verdicts[pv.ID] = pv
		}
	}
	rows := make([]synthRowView, 0, len(samples))
	for _, s := range samples {
		row := synthRowView{
			ID: s.ID, Split: s.Split, Input: s.Input, Expected: expectedJSON(s.Expected),
			Verdict: "-", VerdictLabel: "待过滤", VerdictClass: "dim",
		}
		if pv, ok := verdicts[s.ID]; ok {
			row.Verdict = string(pv.Verdict)
			row.VerdictLabel, row.VerdictClass = verdictView(pv.Verdict)
			row.Variance = fmt.Sprintf("%.4f", pv.Variance)
			parts := make([]string, len(pv.Scores))
			for i, sc := range pv.Scores {
				parts[i] = fmt.Sprintf("%.4f", sc)
			}
			row.Scores = strings.Join(parts, " / ")
			row.Kept = pv.Verdict == harness.VerdictKeep || pv.Verdict == harness.VerdictUnmeasured
		} else if report != nil {
			// A sample added while the checkpoint was open: no probe
			// evidence, kept by the pipeline as a new sample.
			row.Verdict, row.VerdictLabel, row.VerdictClass = "new", "新增（未测）", "ok"
			row.Kept = true
		}
		rows = append(rows, row)
	}
	return rows
}

func verdictView(v harness.Verdict) (string, string) {
	switch v {
	case harness.VerdictKeep:
		return "保留", "ok"
	case harness.VerdictDeadEasy:
		return "死样本·全对", "bad"
	case harness.VerdictDeadHard:
		return "死样本·全错", "bad"
	case harness.VerdictNoisy:
		return "高噪声", "warn"
	case harness.VerdictUnmeasured:
		return "未测得", "dim"
	default:
		return string(v), "dim"
	}
}

// expectedJSON renders the expected value as strict JSON for the edit
// textarea; the POST handler requires strict JSON back.
func expectedJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

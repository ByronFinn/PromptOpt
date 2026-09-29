package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// EventCandidateAdopted is the SSE event published when the frontier
// page adopts (or switches) the delivered candidate.
const EventCandidateAdopted = "candidate_adopted"

// adoptedFile is runs/<id>/adopted.json — the intervention artifact
// behind the frontier page's adopt button.
type adoptedFile struct {
	CandidateID string    `json:"candidate_id"`
	Prompt      string    `json:"prompt"`
	AdoptedAt   time.Time `json:"adopted_at"`
}

// readAdopted decodes adopted.json; ok is false when absent.
func readAdopted(runDir string) (adoptedFile, bool) {
	var a adoptedFile
	b, err := os.ReadFile(filepath.Join(runDir, "adopted.json"))
	if err != nil {
		return a, false
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, false
	}
	return a, true
}

// heatCell is one per-sample score cell with its inline color.
type heatCell struct {
	Value string
	// Color is the rgba background as trusted CSS: the value is built
	// server-side from a clamped float, but html/template's style
	// attribute filter rejects any parenthesis, which rgba() needs.
	Color template.CSS
}

// frontierMemberView is one candidate card plus its heat-table row.
type frontierMemberView struct {
	ID, Operator          string
	Round                 int
	PrimaryMean, JSONRate string
	HasConstraint         bool
	ConstraintOK          bool
	Wins                  int
	IsBest, IsBaseline    bool
	DominatesBaseline     bool
	DominatedByBaseline   bool
	NoDomMarks            bool // scores missing: no annotation
	Prompt                string
	Cells                 []heatCell
}

// lineageRowView is one provenance entry of the lineage section.
type lineageRowView struct {
	ID, Origin           string
	Round                int
	PrimaryMean          string
	Admitted, Incomplete bool
	Label, LabelClass    string
	// DomLabel annotates the record's score row against the baseline
	// row via engine.Dominates — meaningful for rejected children,
	// since frontier members are pairwise non-dominating by admission.
	DomLabel string
}

// adoptPanelView backs the adopt partial (page and POST response).
type adoptPanelView struct {
	ID        string
	MemberIDs []string
	BestID    string
	Adopted   string
	AdoptedAt string
}

// frontierPageView backs frontier.html.
type frontierPageView struct {
	ID                  string
	Primary, Constraint string
	HasConstraint       bool
	Rounds              int
	Reason, ReasonLabel string
	GeneratedAt         string
	Best                engine.FrontierBest
	BestPrimaryMean     string
	BestConstraintLabel string
	BestConstraintClass string
	Members             []frontierMemberView
	SampleIDs           []string
	HasSampleIDs        bool
	Lineage             []lineageRowView
	Notes               []string
	Adopt               adoptPanelView
}

func (s *Server) handleFrontierPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	runDir := filepath.Join(s.runsDir, id)
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, "frontier.html", buildFrontierView(id, runDir, f))
}

// buildFrontierView assembles the page view, degrading over artifacts
// that predate sample_ids / member scores / prompts.
func buildFrontierView(id, runDir string, f engine.FrontierFile) frontierPageView {
	v := frontierPageView{
		ID: id, Primary: f.Primary, Constraint: f.Constraint,
		HasConstraint: f.Constraint != "",
		Rounds:        f.Rounds, Reason: f.Reason,
		ReasonLabel:     reasonLabel(f.Reason),
		GeneratedAt:     f.GeneratedAt.Format("2006-01-02 15:04:05"),
		Best:            f.Best,
		BestPrimaryMean: fmt.Sprintf("%.4f", f.Best.Means[f.Primary]),
		SampleIDs:       f.SampleIDs,
		HasSampleIDs:    len(f.SampleIDs) > 0,
	}
	v.BestConstraintLabel, v.BestConstraintClass = "满足", "ok"
	if !f.Best.ConstraintSatisfied {
		v.BestConstraintLabel, v.BestConstraintClass = "不满足（兜底交付）", "bad"
	}

	// Baseline row for dominance annotations; it may have been evicted.
	var baseline []float64
	hasBaseline := false
	for _, m := range f.Members {
		if m.Operator == engine.OpBaseline {
			baseline, hasBaseline = m.Scores, true
			break
		}
	}
	for _, m := range f.Members {
		view := frontierMemberView{
			ID: m.ID, Operator: m.Operator, Round: m.Round,
			PrimaryMean:   fmt.Sprintf("%.4f", m.PrimaryMean),
			HasConstraint: v.HasConstraint, Prompt: m.Prompt,
			Wins:       m.Wins,
			IsBest:     m.ID == f.Best.ID,
			IsBaseline: m.Operator == engine.OpBaseline,
		}
		if v.HasConstraint {
			view.JSONRate = fmt.Sprintf("%.4f", m.JSONRate)
			view.ConstraintOK = m.JSONRate >= 1
		}
		if len(m.Scores) > 0 && len(m.Scores) == len(v.SampleIDs) {
			for _, sc := range m.Scores {
				view.Cells = append(view.Cells, heatCell{
					Value: fmt.Sprintf("%.2f", sc),
					Color: heatColor(sc),
				})
			}
			if hasBaseline && len(baseline) == len(m.Scores) && !view.IsBaseline {
				view.DominatesBaseline = engine.Dominates(m.Scores, baseline)
				view.DominatedByBaseline = engine.Dominates(baseline, m.Scores)
			}
		} else {
			view.NoDomMarks = true
		}
		v.Members = append(v.Members, view)
	}

	if !v.HasSampleIDs {
		v.Notes = append(v.Notes, "frontier.json 无 sample_ids（旧版产物）：热力表列头退化为序号，逐样本分数不可用")
	}
	if !hasBaseline || len(baseline) == 0 {
		v.Notes = append(v.Notes, "baseline 已被淘汰出前沿（或旧产物无 scores）：无支配标注")
	}
	if f.Rounds == 0 && f.Reason == "" {
		v.Notes = append(v.Notes, "frontier.json 无 rounds/reason（旧版产物）")
	}

	v.Adopt = buildAdoptPanel(id, runDir, f)
	v.Lineage = buildLineageRows(runDir, f)
	return v
}

// buildAdoptPanel loads the current adopted.json (if any).
func buildAdoptPanel(id, runDir string, f engine.FrontierFile) adoptPanelView {
	p := adoptPanelView{ID: id, BestID: f.Best.ID}
	for _, m := range f.Members {
		p.MemberIDs = append(p.MemberIDs, m.ID)
	}
	if a, ok := readAdopted(runDir); ok {
		p.Adopted = a.CandidateID
		p.AdoptedAt = a.AdoptedAt.Format("2006-01-02 15:04:05")
	}
	return p
}

// buildLineageRows renders lineage.json with admission verdicts and
// per-record dominance marks against the baseline row.
func buildLineageRows(runDir string, f engine.FrontierFile) []lineageRowView {
	lin, err := engine.LoadOrInitLineage(filepath.Join(runDir, "lineage.json"))
	if err != nil {
		return nil
	}
	var baseline []float64
	for _, m := range f.Members {
		if m.Operator == engine.OpBaseline {
			baseline = m.Scores
			break
		}
	}
	recs := lin.Records()
	rows := make([]lineageRowView, 0, len(recs))
	for _, rec := range recs {
		origin := rec.Operator
		if len(rec.Parents) > 0 {
			origin = fmt.Sprintf("%s ← %s", origin, strings.Join(rec.Parents, " + "))
		}
		row := lineageRowView{
			ID: rec.ID, Origin: origin, Round: rec.Round,
			PrimaryMean: fmt.Sprintf("%.4f", rec.PrimaryMean),
			Admitted:    rec.Admitted, Incomplete: rec.Incomplete,
		}
		switch {
		case rec.Incomplete:
			row.Label, row.LabelClass = "预算中断·评估不完整", "warn"
		case rec.Admitted:
			row.Label, row.LabelClass = "准入前沿", "ok"
		default:
			row.Label, row.LabelClass = "未准入", "dim"
		}
		switch {
		case rec.Operator == engine.OpBaseline || len(baseline) == 0 || len(rec.Scores) != len(baseline):
			// no comparable baseline row
		case engine.Dominates(rec.Scores, baseline):
			row.DomLabel = "支配 baseline"
		case engine.Dominates(baseline, rec.Scores):
			row.DomLabel = "被 baseline 支配"
		default:
			row.DomLabel = "互不支配"
		}
		rows = append(rows, row)
	}
	return rows
}

// reasonLabel maps a stop reason to its Chinese label.
func reasonLabel(reason string) string {
	switch reason {
	case engine.ReasonRoundsDone:
		return "轮数跑完"
	case engine.ReasonBudgetStopped:
		return "预算停止"
	case engine.ReasonAborted:
		return "中断"
	case "":
		return "—"
	default:
		return reason
	}
}

// heatColor maps a per-sample score to an inline rgba background,
// clamped to [0, 1] and capped for text contrast.
func heatColor(v float64) template.CSS {
	if math.IsNaN(v) {
		return ""
	}
	a := 0.05 + 0.5*min(max(v, 0), 1)
	return template.CSS(fmt.Sprintf("rgba(26,127,55,%.2f)", a))
}

// handleAdopt writes (or switches) adopted.json. Idempotent: adopting
// the current candidate rewrites the same verdict; adopting an unknown
// candidate answers 404 without touching the artifact.
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return
	}
	runDir := filepath.Join(s.runsDir, id)
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	candidate := r.PostFormValue("candidate")
	prompt, ok := frontierPrompt(f, candidate)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The write runs under the adopt lock so double clicks and two
	// tabs serialize instead of racing the atomic rename.
	s.adoptMu.Lock()
	err = harness.SaveJSON(filepath.Join(runDir, "adopted.json"), adoptedFile{
		CandidateID: candidate, Prompt: prompt, AdoptedAt: time.Now().UTC(),
	})
	s.adoptMu.Unlock()
	if err != nil {
		http.Error(w, "写 adopted.json: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.bus != nil {
		s.bus.Publish(eval.Event{
			Type: EventCandidateAdopted, Time: time.Now(), RunID: id,
			Detail: map[string]any{"candidate": candidate},
		})
	}
	render(w, "adopt_panel", buildAdoptPanel(id, runDir, f))
}

// frontierPrompt resolves a candidate id to its prompt: member prompts
// first, the best pick as fallback for artifacts predating member
// prompts.
func frontierPrompt(f engine.FrontierFile, id string) (string, bool) {
	for _, m := range f.Members {
		if m.ID == id {
			if m.Prompt != "" {
				return m.Prompt, true
			}
			break
		}
	}
	if f.Best.ID == id && f.Best.Prompt != "" {
		return f.Best.Prompt, true
	}
	return "", false
}

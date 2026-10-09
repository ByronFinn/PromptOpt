// Dashboard surface (P10): the redesigned four-view dashboard page
// (总览监控 / 前沿与采纳 / 样本证据 / 交付门禁, 1:1 from
// docs/prototype/dashboard-redesign.html) plus its five read-only JSON
// data endpoints. The page keeps the prototype's static demo tables —
// pixel-level visual acceptance is human; what the server fills in is
// the run identity, the verify-gate verdict banner and the adopt
// wiring. The endpoints are the structured truth the demo tables
// stand in for (overview / trend / heatmap / lineage / verify).
package web

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// registerDashboardRoutes mounts the dashboard page and its data
// endpoints (called from Handler).
func (s *Server) registerDashboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /runs/{id}/dashboard", s.handleDashboardPage)
	mux.HandleFunc("GET /runs/{id}/api/overview", s.handleAPIOverview)
	mux.HandleFunc("GET /runs/{id}/api/trend", s.handleAPITrend)
	mux.HandleFunc("GET /runs/{id}/api/heatmap", s.handleAPIHeatmap)
	mux.HandleFunc("GET /runs/{id}/api/lineage", s.handleAPILineage)
	mux.HandleFunc("GET /runs/{id}/api/verify", s.handleAPIVerify)
}

// dashboardView backs dashboard.html. Verdict is never nil: without
// verify artifacts it renders the 「未过门禁」 state — the same
// conclusion the CLI prints (finishRun 结论块), read per request so a
// later `promptopt verify <run_id>` shows up on reload.
type dashboardView struct {
	ID      string
	Task    string
	Verdict *gateBanner
	// Adopt wires the frontier page's adopt endpoint to the dashboard
	// button (candidate id = frontier Best); nil when no frontier
	// artifact exists (manual runs) — the button then degrades.
	Adopt *dashboardAdopt
}

// dashboardAdopt is the adopt button's server-side wiring.
type dashboardAdopt struct {
	CandidateID string
	// Adopted is the currently adopted candidate id ("" = none yet).
	Adopted string
}

// gateBanner is the gate banner rendered in both the overview top and
// the 交付门禁 view (template "verdict_banner"). Class maps onto the
// prototype's verdict color language: "" green (pass), "regress" red,
// "inconclusive" gray.
type gateBanner struct {
	Class  string
	Title  string
	Detail string
}

func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
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
	d := dashboardView{ID: id, Verdict: pendingVerdict()}
	if res, ok := readSummary(runDir); ok {
		d.Task = res.TaskName
	}
	if v, ok := readLatestVerify(runDir); ok {
		d.Verdict = buildVerdictView(v)
	}
	if f, err := engine.LoadFrontier(runDir); err == nil {
		d.Adopt = &dashboardAdopt{CandidateID: f.Best.ID}
		if a, ok := readAdopted(runDir); ok {
			d.Adopt.Adopted = a.CandidateID
		}
	}
	render(w, "dashboard.html", d)
}

// pendingVerdict is the no-verify-artifact banner state (未过门禁).
func pendingVerdict() *gateBanner {
	return &gateBanner{
		Class: "inconclusive",
		Title: "未过门禁 —— 尚未运行 verify",
		Detail: "运行 promptopt verify <run_id> 出具三态结论（0 通过 / 3 回归）；" +
			"verify.json 的三态判定会在刷新后显示在此横幅。",
	}
}

// buildVerdictView maps the verify report's gate verdict onto the
// banner's three-state color language (原型 .verdict 类).
func buildVerdictView(v verifyFile) *gateBanner {
	switch {
	case v.ExitCode == 2:
		return &gateBanner{
			Class: "inconclusive",
			Title: "证据不完整 —— 回归结论不可得",
			Detail: "两侧存在未派发样本（预算耗尽，退出码 2）：没有完整证据，" +
				"门禁不给出回归结论——提高预算后重跑 verify。",
		}
	case v.ExitCode == 1:
		return &gateBanner{
			Class:  "regress",
			Title:  "验证评估失败 —— 无有效结论",
			Detail: "verify 评估侧失败（退出码 1）：先解决执行器/连接问题再重跑 verify。",
		}
	}
	reg := v.Regression
	ci := reg.CI
	switch {
	case ci != nil && ci.Verdict == "confident_pass":
		return &gateBanner{
			Class:  "",
			Title:  "置信通过 —— 提升超出噪声区间，证据充分",
			Detail: verdictDetail(v, "CI 上界 ≤ 0（提升可信）"),
		}
	case ci != nil && ci.Verdict == "regressed":
		return &gateBanner{
			Class:  "regress",
			Title:  "回归 —— CI 下界超过阈值",
			Detail: verdictDetail(v, "退化超过噪声区间；发现线上退化时 promptopt rollback <run_id> 回退"),
		}
	case ci != nil:
		return &gateBanner{
			Class:  "inconclusive",
			Title:  "不可判定 —— 差异未超噪声区间，样本量不足",
			Detail: verdictDetail(v, "退出码 0，但该结论不构成回归背书；锚点库沉淀可收窄区间"),
		}
	case reg.Regressed:
		return &gateBanner{
			Class:  "regress",
			Title:  "回归 —— 均值差超过阈值",
			Detail: verdictDetail(v, "均值差判定（未跑配对自助法 CI）；promptopt rollback <run_id> 可回退"),
		}
	default:
		return &gateBanner{
			Class:  "",
			Title:  "通过 —— 均值差在阈值内",
			Detail: verdictDetail(v, "均值差判定（未跑配对自助法 CI，无三态置信结论）"),
		}
	}
}

// verdictDetail renders the banner's supporting line: primary delta,
// the CI bounds when present, and the caller's tail note.
func verdictDetail(v verifyFile, tail string) string {
	reg := v.Regression
	var b strings.Builder
	if ci := reg.CI; ci != nil {
		fmt.Fprintf(&b, "%s Δ %.4f（基线−交付） · 配对自助法 CI [%.4f, %.4f] B=%d · ",
			reg.Primary, reg.Delta, ci.Lo, ci.Hi, ci.B)
	} else {
		fmt.Fprintf(&b, "%s Δ %.4f（基线−交付） · 阈值 %.2f · ", reg.Primary, reg.Delta, reg.Threshold)
	}
	fmt.Fprintf(&b, "%s。退出码 %d", tail, v.ExitCode)
	return b.String()
}

// --- verify.json minimal decode (cmd's verifyReport is unreachable
// from internal/, so the dashboard carries the field subset it
// renders; additive verify.json fields decode-and-drop safely). ------

type verifyFileCI struct {
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
	B       int     `json:"b"`
	Verdict string  `json:"verdict"`
}

type verifyFileRegression struct {
	Primary       string        `json:"primary"`
	BaselineMean  float64       `json:"baseline_mean"`
	DeliveredMean float64       `json:"delivered_mean"`
	Delta         float64       `json:"delta"`
	Threshold     float64       `json:"threshold"`
	Regressed     bool          `json:"regressed"`
	CI            *verifyFileCI `json:"ci,omitempty"`
}

type verifyFileSide struct {
	CandidateID string             `json:"candidate_id"`
	Means       map[string]float64 `json:"metric_means"`
	Rows        []float64          `json:"rows,omitempty"`
	Reps        int                `json:"reps,omitempty"`
}

// verifyFile is the verify.json subset the dashboard renders.
type verifyFile struct {
	RunID      string               `json:"run_id"`
	VerifiedAt time.Time            `json:"verified_at"`
	Mode       string               `json:"mode"`
	Note       string               `json:"note,omitempty"`
	Primary    string               `json:"primary"`
	Baseline   verifyFileSide       `json:"baseline"`
	Delivered  verifyFileSide       `json:"delivered"`
	Regression verifyFileRegression `json:"regression"`
	ExitCode   int                  `json:"exit_code"`
}

// readLatestVerify loads the newest runs/<id>/verify/<ts>/verify.json
// (timestamp-named directories sort lexicographically newest-first).
func readLatestVerify(runDir string) (verifyFile, bool) {
	entries, err := os.ReadDir(filepath.Join(runDir, "verify"))
	if err != nil {
		return verifyFile{}, false
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(b.Name(), a.Name()) })
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(runDir, "verify", e.Name(), "verify.json"))
		if err != nil {
			continue
		}
		var v verifyFile
		if err := json.Unmarshal(b, &v); err != nil {
			continue
		}
		return v, true
	}
	return verifyFile{}, false
}

// latestVerifyDir returns the newest verify/<ts> directory path (""
// when none), shared by the id resolvers.
func latestVerifyDir(runDir string) string {
	entries, err := os.ReadDir(filepath.Join(runDir, "verify"))
	if err != nil {
		return ""
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(b.Name(), a.Name()) })
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(runDir, "verify", e.Name())
		}
	}
	return ""
}

// --- data endpoints ---------------------------------------------------

// writeJSON answers one JSON payload with the canonical content type.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// runIDOr404 resolves the {id} path value against the runs tree.
func (s *Server) runIDOr404(w http.ResponseWriter, r *http.Request) (id, runDir string, ok bool) {
	id = r.PathValue("id")
	if !safeRunID(id) {
		http.NotFound(w, r)
		return "", "", false
	}
	runDir = filepath.Join(s.runsDir, id)
	if _, err := os.Stat(runDir); err != nil {
		http.NotFound(w, r)
		return "", "", false
	}
	return id, runDir, true
}

// sampleRepRow is one sample's evidence row in the overview payload:
// per-metric means ± in-sample sd and the per-rep scores (the rep
// strip's bar data).
type sampleRepRow struct {
	SampleID  string               `json:"sample_id"`
	Scores    map[string]float64   `json:"scores,omitempty"`
	ScoresSD  map[string]float64   `json:"scores_sd,omitempty"`
	RepScores []map[string]float64 `json:"rep_scores,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// judgeRow is the overview's RoleJudge usage line (single-listed usage
// that still arms the executor soft stop — one evaluation budget).
type judgeRow struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Total            int64 `json:"total"`
}

// overviewPayload is GET /runs/{id}/api/overview: the conclusion
// payload (verify three-state banner data), primary mean ± sd
// aggregated from SampleTrace ScoresSD, the budget gauge and the
// RoleJudge row.
type overviewPayload struct {
	RunID     string `json:"run_id"`
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`
	Status    string `json:"status,omitempty"`
	ExitCode  int    `json:"exit_code,omitempty"`
	Task      string `json:"task,omitempty"`
	Candidate string `json:"candidate,omitempty"`
	Primary   string `json:"primary,omitempty"`
	// PrimaryMean/PrimarySD: the run mean and the mean of per-sample
	// in-sample sd (reps>1 artifacts only; 0 otherwise).
	PrimaryMean float64            `json:"primary_mean,omitempty"`
	PrimarySD   float64            `json:"primary_sd,omitempty"`
	MetricMeans map[string]float64 `json:"metric_means,omitempty"`
	Budget      []budgetRow        `json:"budget,omitempty"`
	Judge       *judgeRow          `json:"judge,omitempty"`
	Verdict     *verdictPayload    `json:"verdict"`
	Samples     []sampleRepRow     `json:"samples,omitempty"`
}

func (s *Server) handleAPIOverview(w http.ResponseWriter, r *http.Request) {
	id, runDir, ok := s.runIDOr404(w, r)
	if !ok {
		return
	}
	p := overviewPayload{RunID: id, Verdict: verifyPayloadFor(runDir)}
	res, hasSummary := readSummary(runDir)
	if !hasSummary {
		p.Note = "无 summary.json（run 未完成或工件缺失）"
		writeJSON(w, p)
		return
	}
	p.Available = true
	p.Status = string(res.Status)
	p.ExitCode = res.ExitCode
	p.Task = res.TaskName
	p.Candidate = res.CandidateID
	p.MetricMeans = res.MetricMeans
	if j, ok := res.UsageByRole[core.RoleJudge]; ok {
		p.Judge = &judgeRow{
			PromptTokens:     j.PromptTokens,
			CompletionTokens: j.CompletionTokens,
			Total:            j.Total(),
		}
	}
	if g := buildBudgetGauge(runDir, res, hasSummary); len(g.Rows) > 0 {
		p.Budget = g.Rows
	}
	// Primary resolution: frontier.json carries it; manual runs without
	// optimization artifacts degrade to means only.
	if f, err := engine.LoadFrontier(runDir); err == nil {
		p.Primary = f.Primary
		p.PrimaryMean = res.MetricMeans[f.Primary]
		p.PrimarySD = sampleSDMean(runDir, f.Primary)
	}
	p.Samples = sampleRepRows(runDir)
	writeJSON(w, p)
}

// sampleSDMean aggregates the per-sample in-sample sd of one metric
// over the run's sample traces (mean of ScoresSD[m]; the ±sd behind
// the overview stat card).
func sampleSDMean(runDir, metric string) float64 {
	sum, n := 0.0, 0
	for _, t := range readSampleTraces(runDir) {
		if sd, ok := t.ScoresSD[metric]; ok && !math.IsNaN(sd) {
			sum += sd
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// readSampleTraces decodes every samples/*.json trace sorted by id.
func readSampleTraces(runDir string) []core.SampleTrace {
	dir := filepath.Join(runDir, "samples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var traces []core.SampleTrace
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
		traces = append(traces, t)
	}
	slices.SortFunc(traces, func(a, b core.SampleTrace) int { return strings.Compare(a.SampleID, b.SampleID) })
	return traces
}

// sampleRepRows projects the sample traces onto the overview's
// per-sample evidence rows (rep-strip data source).
func sampleRepRows(runDir string) []sampleRepRow {
	traces := readSampleTraces(runDir)
	rows := make([]sampleRepRow, 0, len(traces))
	for _, t := range traces {
		rows = append(rows, sampleRepRow{
			SampleID:  t.SampleID,
			Scores:    t.Scores,
			ScoresSD:  t.ScoresSD,
			RepScores: t.RepScores,
			Error:     t.Error,
		})
	}
	return rows
}

// trendPoint is one round's best candidate on the trend line.
type trendPoint struct {
	Round       int     `json:"round"`
	BestID      string  `json:"best_id"`
	Operator    string  `json:"operator"`
	PrimaryMean float64 `json:"primary_mean"`
	// SD is the member's mean per-sample in-sample sd (the error bar);
	// omitted for single-shot artifacts.
	SD float64 `json:"sd,omitempty"`
}

// trendPayload is GET /runs/{id}/api/trend: the per-round best
// sequence aggregated from frontier.json members (round → best primary
// mean, error bar from the member's SD row).
type trendPayload struct {
	Available bool         `json:"available"`
	Note      string       `json:"note,omitempty"`
	Primary   string       `json:"primary,omitempty"`
	Rounds    int          `json:"rounds,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Points    []trendPoint `json:"points,omitempty"`
}

func (s *Server) handleAPITrend(w http.ResponseWriter, r *http.Request) {
	_, runDir, ok := s.runIDOr404(w, r)
	if !ok {
		return
	}
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		writeJSON(w, trendPayload{
			Note: "无 frontier.json（手动模式不产生优化工件；预算饿死 baseline 的零配置 run 同样跳过循环）",
		})
		return
	}
	p := trendPayload{
		Available: true, Primary: f.Primary, Rounds: f.Rounds, Reason: f.Reason,
	}
	byRound := map[int]engine.FrontierMember{}
	for _, m := range f.Members {
		cur, seen := byRound[m.Round]
		if !seen || m.PrimaryMean > cur.PrimaryMean ||
			(m.PrimaryMean == cur.PrimaryMean && m.ID < cur.ID) {
			byRound[m.Round] = m
		}
	}
	rounds := make([]int, 0, len(byRound))
	for rd := range byRound {
		rounds = append(rounds, rd)
	}
	slices.Sort(rounds)
	for _, rd := range rounds {
		m := byRound[rd]
		p.Points = append(p.Points, trendPoint{
			Round: rd, BestID: m.ID, Operator: m.Operator,
			PrimaryMean: m.PrimaryMean, SD: sdRowMean(m.SD),
		})
	}
	writeJSON(w, p)
}

// sdRowMean averages a per-sample sd row (the error-bar height); empty
// rows (single-shot artifacts) read 0.
func sdRowMean(sd []float64) float64 {
	if len(sd) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range sd {
		sum += v
	}
	return sum / float64(len(sd))
}

// heatMember is one heatmap column (a frontier member).
type heatMember struct {
	ID          string  `json:"id"`
	Operator    string  `json:"operator"`
	Round       int     `json:"round"`
	PrimaryMean float64 `json:"primary_mean"`
	Reps        int     `json:"reps,omitempty"`
	IsBest      bool    `json:"is_best,omitempty"`
	IsBaseline  bool    `json:"is_baseline,omitempty"`
}

// apiHeatCell is one heatmap cell: the member's score on the sample with
// its sd and the noise-band marks.
type apiHeatCell struct {
	Score float64 `json:"score"`
	SD    float64 `json:"sd,omitempty"`
	// Win marks the sample's best cell (the prototype's green frame).
	Win bool `json:"win,omitempty"`
	// NearNoise marks a non-winning cell whose gap to the row's best
	// stays within the ε noise band (presentation-level pooling: the
	// per-sample max sd across members, mirroring the loop's
	// max(childSD, curSD) pairing). Only computed when at least one
	// member carries an SD row — single-shot evidence has no ε.
	NearNoise bool `json:"near_noise,omitempty"`
}

type heatSampleRow struct {
	SampleID string        `json:"sample_id"`
	Cells    []apiHeatCell `json:"cells"`
}

// heatmapPayload is GET /runs/{id}/api/heatmap: the frontier heat
// table (scores ± sd + reps, near-noise marks) with the constraint row
// for the tfoot.
type heatmapPayload struct {
	Available  bool            `json:"available"`
	Note       string          `json:"note,omitempty"`
	Primary    string          `json:"primary,omitempty"`
	Constraint string          `json:"constraint,omitempty"`
	SampleIDs  []string        `json:"sample_ids,omitempty"`
	Members    []heatMember    `json:"members,omitempty"`
	Rows       []heatSampleRow `json:"rows,omitempty"`
	// ConstraintRow is the per-member constraint mean (tfoot 约束行),
	// present only when the task declares one.
	ConstraintRow []float64 `json:"constraint_row,omitempty"`
}

func (s *Server) handleAPIHeatmap(w http.ResponseWriter, r *http.Request) {
	_, runDir, ok := s.runIDOr404(w, r)
	if !ok {
		return
	}
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		writeJSON(w, heatmapPayload{
			Note: "无 frontier.json（手动模式不产生优化工件；预算饿死 baseline 的零配置 run 同样跳过循环）",
		})
		return
	}
	p := heatmapPayload{
		Available: true, Primary: f.Primary, Constraint: f.Constraint,
		SampleIDs: f.SampleIDs,
	}
	for _, m := range f.Members {
		p.Members = append(p.Members, heatMember{
			ID: m.ID, Operator: m.Operator, Round: m.Round,
			PrimaryMean: m.PrimaryMean, Reps: m.Reps,
			IsBest: m.ID == f.Best.ID, IsBaseline: m.Operator == engine.OpBaseline,
		})
		if f.Constraint != "" {
			p.ConstraintRow = append(p.ConstraintRow, m.JSONRate)
		}
	}
	// ε band per sample: the max sd across members (conservative
	// presentation-level pooling of the loop's pairwise max rule).
	eps := make([]float64, len(f.SampleIDs))
	hasSD := false
	for _, m := range f.Members {
		if len(m.SD) == 0 {
			continue
		}
		hasSD = true
		for i, sd := range m.SD {
			if i < len(eps) {
				eps[i] = max(eps[i], sd)
			}
		}
	}
	for i, sid := range f.SampleIDs {
		best := math.Inf(-1)
		for _, m := range f.Members {
			if i < len(m.Scores) {
				best = max(best, m.Scores[i])
			}
		}
		row := heatSampleRow{SampleID: sid, Cells: make([]apiHeatCell, 0, len(f.Members))}
		for _, m := range f.Members {
			cell := apiHeatCell{}
			if i < len(m.Scores) {
				cell.Score = m.Scores[i]
				cell.Win = m.Scores[i] == best
				if i < len(m.SD) {
					cell.SD = m.SD[i]
				}
				if hasSD && !cell.Win && math.Abs(best-m.Scores[i]) <= eps[min(i, len(eps)-1)] {
					cell.NearNoise = true
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		p.Rows = append(p.Rows, row)
	}
	writeJSON(w, p)
}

// lineagePayloadRow is one lineage record projected for the API
// (reusing the frontier page's lineage rendering semantics).
type lineagePayloadRow struct {
	ID          string  `json:"id"`
	Origin      string  `json:"origin"`
	Round       int     `json:"round"`
	PrimaryMean float64 `json:"primary_mean"`
	Admitted    bool    `json:"admitted"`
	Incomplete  bool    `json:"incomplete,omitempty"`
	Label       string  `json:"label"`
	DomLabel    string  `json:"dom_label,omitempty"`
}

// lineagePayload is GET /runs/{id}/api/lineage: the candidate flow,
// projected from the same builder the frontier page renders.
type lineagePayload struct {
	Available bool                `json:"available"`
	Note      string              `json:"note,omitempty"`
	Rows      []lineagePayloadRow `json:"rows,omitempty"`
}

func (s *Server) handleAPILineage(w http.ResponseWriter, r *http.Request) {
	_, runDir, ok := s.runIDOr404(w, r)
	if !ok {
		return
	}
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		writeJSON(w, lineagePayload{
			Note: "无 frontier.json（lineage 渲染按前沿成员行做支配标注，手动模式无优化工件）",
		})
		return
	}
	p := lineagePayload{Available: true}
	for _, v := range buildLineageRows(runDir, f) {
		// The frontier view renders the mean as "%.4f"; the API serves
		// it back as a number.
		mean, _ := strconv.ParseFloat(v.PrimaryMean, 64)
		p.Rows = append(p.Rows, lineagePayloadRow{
			ID: v.ID, Origin: v.Origin, Round: v.Round,
			PrimaryMean: mean, Admitted: v.Admitted,
			Incomplete: v.Incomplete, Label: v.Label, DomLabel: v.DomLabel,
		})
	}
	writeJSON(w, p)
}

// verifyDiffRow is one row of the paired-difference table: D = 基线−交付
// (the paired bootstrap's per-sample input).
type verifyDiffRow struct {
	SampleID  string  `json:"sample_id"`
	Baseline  float64 `json:"baseline"`
	Delivered float64 `json:"delivered"`
	D         float64 `json:"d"`
}

// exitCodeRow is the static exit-code contract table row.
type exitCodeRow struct {
	Code  int    `json:"code"`
	Label string `json:"label"`
}

// exitCodeContract is the AGENTS.md 0/1/2/3 contract as served data.
func exitCodeContract() []exitCodeRow {
	return []exitCodeRow{
		{0, "成功（含「不可判定」——证据不足不是失败）"},
		{1, "评估失败或用法错误"},
		{2, "预算耗尽（优先于 1）"},
		{3, "verify 回归或约束违反（优先级 2 > 1 > 3）"},
	}
}

// verdictPayload is the three-state gate conclusion embedded in the
// overview (and mirrored by the dashboard banner): the verify.json
// gate verdict plus its CI numbers.
type verdictPayload struct {
	Available bool    `json:"available"`
	State     string  `json:"state"` // confident_pass|regressed|inconclusive|pass|failed|incomplete|pending
	Title     string  `json:"title"`
	Detail    string  `json:"detail"`
	Primary   string  `json:"primary,omitempty"`
	Delta     float64 `json:"delta,omitempty"`
	Lo        float64 `json:"lo,omitempty"`
	Hi        float64 `json:"hi,omitempty"`
	B         int     `json:"b,omitempty"`
	ExitCode  int     `json:"exit_code,omitempty"`
}

// verifyPayloadFor builds the overview's embedded verdict.
func verifyPayloadFor(runDir string) *verdictPayload {
	v, ok := readLatestVerify(runDir)
	if !ok {
		pv := pendingVerdict()
		return &verdictPayload{State: "pending", Title: pv.Title, Detail: pv.Detail}
	}
	vv := buildVerdictView(v)
	return &verdictPayload{
		Available: true,
		State:     mapVerdictState(v),
		Title:     vv.Title,
		Detail:    vv.Detail,
		Primary:   v.Regression.Primary,
		Delta:     v.Regression.Delta,
		ExitCode:  v.ExitCode,
		Lo:        ciBound(v.Regression.CI, true),
		Hi:        ciBound(v.Regression.CI, false),
		B:         ciB(v.Regression.CI),
	}
}

// mapVerdictState derives the payload state from the report shape.
func mapVerdictState(v verifyFile) string {
	switch {
	case v.ExitCode == 2:
		return "incomplete"
	case v.ExitCode == 1:
		return "failed"
	case v.Regression.CI != nil:
		return v.Regression.CI.Verdict // confident_pass | regressed | inconclusive
	case v.Regression.Regressed:
		return "regressed"
	default:
		return "pass"
	}
}

func ciBound(ci *verifyFileCI, lo bool) float64 {
	if ci == nil {
		return 0
	}
	if lo {
		return ci.Lo
	}
	return ci.Hi
}

func ciB(ci *verifyFileCI) int {
	if ci == nil {
		return 0
	}
	return ci.B
}

// verifyPayload is GET /runs/{id}/api/verify: the latest verify.json
// payload with the paired-difference rows table and the exit-code
// contract table. Empty state (available=false) when no verify
// artifact exists yet — the endpoint depends on `promptopt verify`
// having run.
type verifyPayload struct {
	Available bool            `json:"available"`
	Note      string          `json:"note,omitempty"`
	Report    *verifyFile     `json:"report,omitempty"`
	DiffRows  []verifyDiffRow `json:"diff_rows,omitempty"`
	ExitCodes []exitCodeRow   `json:"exit_codes"`
}

func (s *Server) handleAPIVerify(w http.ResponseWriter, r *http.Request) {
	_, runDir, ok := s.runIDOr404(w, r)
	if !ok {
		return
	}
	p := verifyPayload{ExitCodes: exitCodeContract()}
	v, found := readLatestVerify(runDir)
	if !found {
		p.Note = "无 verify 工件：运行 promptopt verify <run_id> 后此处给出三态门禁结论（CI lo/hi/B、配对差分表、退出码契约）"
		writeJSON(w, p)
		return
	}
	p.Available = true
	p.Report = &v
	p.DiffRows = pairedDiffRows(latestVerifyDir(runDir), v)
	writeJSON(w, p)
}

// pairedDiffRows joins the two sides' fixed-order rows into the
// D = 基线−交付 table. Sample ids resolve best-effort from the verify
// dir's anchor dataset (anchor mode) or holdout samples (fallback);
// unresolved ids fall back to ordinal names.
func pairedDiffRows(verifyDir string, v verifyFile) []verifyDiffRow {
	base, deliv := v.Baseline.Rows, v.Delivered.Rows
	if len(base) == 0 || len(base) != len(deliv) {
		return nil
	}
	ids := verifySampleIDs(verifyDir, len(base))
	rows := make([]verifyDiffRow, 0, len(base))
	for i := range base {
		rows = append(rows, verifyDiffRow{
			SampleID:  ids[i],
			Baseline:  base[i],
			Delivered: deliv[i],
			D:         base[i] - deliv[i],
		})
	}
	return rows
}

// verifySampleIDs resolves the verification set's sample ids: the
// anchor-dataset.json copy first, then the holdout samples file, then
// ordinal fallbacks.
func verifySampleIDs(verifyDir string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("sample-%02d", i+1)
	}
	if verifyDir == "" {
		return ids
	}
	for _, name := range []string{"anchor-dataset.json", filepath.Join("holdout", "samples.json")} {
		b, err := os.ReadFile(filepath.Join(verifyDir, name))
		if err != nil {
			continue
		}
		var ds struct {
			Samples []core.Sample `json:"samples"`
		}
		if json.Unmarshal(b, &ds) != nil || len(ds.Samples) == 0 {
			continue
		}
		for i := 0; i < min(n, len(ds.Samples)); i++ {
			ids[i] = ds.Samples[i].ID
		}
		return ids
	}
	return ids
}

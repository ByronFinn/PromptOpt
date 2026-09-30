package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// diffLine is one row of the side-by-side rendering.
type diffLine struct {
	Kind string // same | add | del
	A, B string
}

// diffPageView backs diff.html.
type diffPageView struct {
	ID, A, B             string
	ASummary, BSummary   string // operator @ round
	Lines                []diffLine
	Added, Deleted, Same int
	Members              []string // for the in-page selects
}

// handleDiffPage serves GET /runs/{id}/diff?a=&b=: a line-level LCS
// diff of two frontier members' prompts.
func (s *Server) handleDiffPage(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	a, b := q.Get("a"), q.Get("b")
	if a == "" && len(f.Members) > 0 {
		a = f.Members[0].ID
	}
	if b == "" && f.Best.ID != "" {
		b = f.Best.ID
	}
	pa, oka := frontierPrompt(f, a)
	pb, okb := frontierPrompt(f, b)
	if !oka || !okb {
		http.NotFound(w, r)
		return
	}
	v := diffPageView{ID: id, A: a, B: b}
	for _, m := range f.Members {
		v.Members = append(v.Members, m.ID)
	}
	v.ASummary, v.BSummary = memberSummary(f, a), memberSummary(f, b)
	v.Lines = lcsDiff(splitLines(pa), splitLines(pb))
	for _, l := range v.Lines {
		switch l.Kind {
		case "add":
			v.Added++
		case "del":
			v.Deleted++
		default:
			v.Same++
		}
	}
	render(w, "diff.html", v)
}

// memberSummary renders "operator @ round" for the selects and
// headers.
func memberSummary(f engine.FrontierFile, id string) string {
	for _, m := range f.Members {
		if m.ID == id {
			return fmt.Sprintf("%s · 第 %d 轮", operatorLabel(m.Operator), m.Round)
		}
	}
	if f.Best.ID == id {
		return "Top-1"
	}
	return id
}

// operatorLabel maps an operator to its Chinese label.
func operatorLabel(op string) string {
	switch op {
	case engine.OpBaseline:
		return "baseline"
	case engine.OpRewrite:
		return "重写"
	case engine.OpMerge:
		return "合并"
	case engine.OpRestart:
		return "重启"
	default:
		return op
	}
}

// splitLines splits s into lines (strings.SplitSeq keeps the iterator
// idiom; a trailing newline yields no phantom empty line).
func splitLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSuffix(s, "\n"), "\n") {
		out = append(out, line)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// lcsDiff computes a line-level diff by longest-common-subsequence
// dynamic programming (O(n·m)); prompts are tens to hundreds of lines,
// so the quadratic table is fine.
func lcsDiff(a, b []string) []diffLine {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	out := make([]diffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{Kind: "same", A: a[i], B: b[j]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			out = append(out, diffLine{Kind: "del", A: a[i]})
			i++
		default:
			out = append(out, diffLine{Kind: "add", B: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{Kind: "del", A: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{Kind: "add", B: b[j]})
	}
	return out
}

// --- run compare ----------------------------------------------------------

// compareSideView is one run column of the compare page.
type compareSideView struct {
	ID                  string
	Status, StatusClass string
	ExitCode            int
	// Optimizer is the paradigm name from manifest.json (empty for
	// pre-V5 runs); OptimizerLabel is its registry-resolved display
	// name, falling back to the raw name for unregistered paradigms.
	Optimizer           string
	OptimizerLabel      string
	HasSummary          bool
	Metrics             map[string]float64
	Usage               map[core.Role]core.Usage
	HasFrontier         bool
	Top1ID              string
	Top1Primary         string
	Top1Constraint      string
	ConstraintLabel     string
	ConstraintClass     string
	Rounds              int
	Reason, ReasonLabel string
}

// compareMetricRow is one metric compared across the two runs.
type compareMetricRow struct {
	Name        string
	A, B, Delta string
}

// compareUsageRow is one role's token usage compared.
type compareUsageRow struct {
	Role  string
	A, B  string
	Delta string
}

// comparePageView backs compare.html.
type comparePageView struct {
	Runs         []string // select options
	A, B         string
	SideA, SideB compareSideView
	Metrics      []compareMetricRow
	Usage        []compareUsageRow
}

// handleComparePage serves GET /compare?a=&b=: summary.json (status,
// exit, metric means, per-role usage) and frontier.json (Top-1) of two
// runs side by side with Δ columns.
func (s *Server) handleComparePage(w http.ResponseWriter, r *http.Request) {
	cards, err := listRunCards(s.runsDir)
	if err != nil {
		http.Error(w, "read runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ids := make([]string, 0, len(cards))
	for _, c := range cards {
		ids = append(ids, c.ID)
	}
	if len(ids) == 0 {
		render(w, "compare.html", comparePageView{})
		return
	}
	q := r.URL.Query()
	a, b := q.Get("a"), q.Get("b")
	if !slices.Contains(ids, a) {
		a = ids[min(0, len(ids)-1)]
	}
	if !slices.Contains(ids, b) {
		b = ids[min(1, len(ids)-1)]
	}
	v := comparePageView{Runs: ids, A: a, B: b}
	v.SideA = s.compareSide(a)
	v.SideB = s.compareSide(b)
	v.buildMetricRows()
	v.buildUsageRows()
	render(w, "compare.html", v)
}

// compareSide loads one run's summary + frontier view.
func (s *Server) compareSide(id string) compareSideView {
	runDir := filepath.Join(s.runsDir, id)
	side := compareSideView{ID: id}
	if res, ok := readSummary(runDir); ok {
		side.HasSummary = true
		side.Status, side.StatusClass = statusView(res.Status)
		side.ExitCode = res.ExitCode
		side.Metrics = res.MetricMeans
		side.Usage = res.UsageByRole
	} else {
		side.Status, side.StatusClass = "未完成", "dim"
	}
	side.Optimizer = readManifestLimits(runDir).Optimizer
	side.OptimizerLabel = optimizerLabel(side.Optimizer)
	f, err := engine.LoadFrontier(runDir)
	if err != nil {
		return side
	}
	side.HasFrontier = true
	side.Top1ID = f.Best.ID
	side.Top1Primary = fmt.Sprintf("%.4f", f.Best.Means[f.Primary])
	if f.Constraint != "" {
		side.Top1Constraint = fmt.Sprintf("%.4f", f.Best.Means[f.Constraint])
		side.ConstraintLabel, side.ConstraintClass = "满足", "ok"
		if !f.Best.ConstraintSatisfied {
			side.ConstraintLabel, side.ConstraintClass = "不满足", "bad"
		}
	}
	side.Rounds, side.Reason, side.ReasonLabel = f.Rounds, f.Reason, reasonLabel(f.Reason)
	return side
}

// buildMetricRows unions both runs' metric names, sorted.
func (v *comparePageView) buildMetricRows() {
	names := make([]string, 0, len(v.SideA.Metrics)+len(v.SideB.Metrics))
	for _, side := range []compareSideView{v.SideA, v.SideB} {
		for m := range side.Metrics {
			if !slices.Contains(names, m) {
				names = append(names, m)
			}
		}
	}
	slices.Sort(names)
	for _, m := range names {
		va, okA := v.SideA.Metrics[m]
		vb, okB := v.SideB.Metrics[m]
		v.Metrics = append(v.Metrics, compareMetricRow{
			Name:  m,
			A:     metricOrDash(okA, va),
			B:     metricOrDash(okB, vb),
			Delta: deltaOrDash(okA, okB, va-vb),
		})
	}
}

// buildUsageRows compares per-role token totals.
func (v *comparePageView) buildUsageRows() {
	for _, role := range []core.Role{core.RoleExecutor, core.RoleOptimizer} {
		ua, okA := v.SideA.Usage[role]
		ub, okB := v.SideB.Usage[role]
		if !okA && !okB {
			continue
		}
		v.Usage = append(v.Usage, compareUsageRow{
			Role:  string(role),
			A:     usageOrDash(okA, ua),
			B:     usageOrDash(okB, ub),
			Delta: fmt.Sprintf("%+d", ub.Total()-ua.Total()),
		})
	}
}

func metricOrDash(ok bool, v float64) string {
	if ok {
		return fmt.Sprintf("%.4f", v)
	}
	return "—"
}

func deltaOrDash(okA, okB bool, d float64) string {
	if okA && okB {
		return fmt.Sprintf("%+.4f", d)
	}
	return "—"
}

func usageOrDash(ok bool, u core.Usage) string {
	if ok {
		return fmt.Sprintf("%d", u.Total())
	}
	return "—"
}

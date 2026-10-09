package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Artifact file names under runs/<id>/.
const (
	fileLineage  = "lineage.json"
	fileFrontier = "frontier.json"
	fileReport   = "report.md"
)

// ReportInputs carries everything WriteOutputs needs beyond the
// frontier itself.
type ReportInputs struct {
	Task    core.Task
	Lineage *Lineage
	Result  Result
	// SampleIDs is the retained evaluation set in its fixed order; it
	// is the sole data source of frontier.json's sample_ids column
	// header, keeping Member.Scores alignment explicit on disk.
	SampleIDs []string
	// Constraint is "json_validator" when declared, else "".
	Constraint string
	// OptBudgetTokens is the optimizer-side valve setting (0 =
	// unlimited); OptValveTripped reports whether it fired.
	OptBudgetTokens int64
	OptValveTripped bool
	// BaselineGaps counts baseline score cells recorded as 0.
	BaselineGaps int
}

// FrontierMember is one frontier member as rendered in frontier.json.
type FrontierMember struct {
	ID          string  `json:"id"`
	Round       int     `json:"round"`
	Operator    string  `json:"operator"`
	PrimaryMean float64 `json:"primary_mean"`
	JSONRate    float64 `json:"json_valid_rate,omitempty"`
	Wins        int     `json:"wins"`
	// Scores is the per-sample primary row over the retained set in
	// sample_ids order; Prompt mirrors the candidate for self-contained
	// downstream tooling (diff, adopt). SD is the matching per-sample
	// in-sample standard deviation row and Reps the sampling depth —
	// both omitted for single-shot artifacts (the historical shape), so
	// legacy frontier.json files read unchanged.
	Scores []float64 `json:"scores,omitempty"`
	SD     []float64 `json:"sd,omitempty"`
	Reps   int       `json:"reps,omitempty"`
	Prompt string    `json:"prompt,omitempty"`
}

// FrontierBest is the deliverable candidate as rendered in
// frontier.json.
type FrontierBest struct {
	ID                  string             `json:"id"`
	ConstraintSatisfied bool               `json:"constraint_satisfied"`
	Note                string             `json:"note,omitempty"`
	Means               map[string]float64 `json:"means"`
	Prompt              string             `json:"prompt"`
}

// FrontierFile is frontier.json: the terminal frontier plus the best
// pick and per-member views. SampleIDs/Rounds/Reason/UsageByRole and
// the member Scores/Prompt extensions are additive; artifacts written
// before they existed decode with zero values and the dashboard
// degrades instead of failing.
type FrontierFile struct {
	Primary     string                   `json:"primary"`
	Constraint  string                   `json:"constraint,omitempty"`
	Best        FrontierBest             `json:"best"`
	Members     []FrontierMember         `json:"members"`
	SampleIDs   []string                 `json:"sample_ids,omitempty"`
	Rounds      int                      `json:"rounds,omitempty"`
	Reason      string                   `json:"reason,omitempty"`
	UsageByRole map[core.Role]core.Usage `json:"usage_by_role,omitempty"`
	GeneratedAt time.Time                `json:"generated_at"`
}

// LoadFrontier decodes runs/<id>/frontier.json.
func LoadFrontier(runDir string) (FrontierFile, error) {
	b, err := os.ReadFile(filepath.Join(runDir, fileFrontier))
	if err != nil {
		return FrontierFile{}, err
	}
	var f FrontierFile
	if err := json.Unmarshal(b, &f); err != nil {
		return FrontierFile{}, fmt.Errorf("decode %s: %w", fileFrontier, err)
	}
	return f, nil
}

// WriteOutputs renders frontier.json and report.md under runDir. It is
// called on every terminal state (rounds done, budget stopped,
// aborted) — an optimization run always leaves its artifacts behind.
func WriteOutputs(runDir string, in ReportInputs, f *Frontier) error {
	primary := in.Task.Primary()
	constraint := in.Constraint
	best, satisfied, note := f.Best(primary, constraint)

	members := f.Members()
	views := make([]FrontierMember, 0, len(members))
	for _, m := range members {
		view := FrontierMember{
			ID: m.ID(), Round: m.Round, Operator: m.Operator,
			PrimaryMean: m.Means[primary], Wins: f.Wins(m.ID()),
			Scores: m.Scores, SD: m.SD, Reps: m.Reps, Prompt: m.Candidate.Prompt,
		}
		if constraint == "json_validator" {
			view.JSONRate = m.Means[constraint]
		}
		views = append(views, view)
	}
	if err := saveJSON(filepath.Join(runDir, fileFrontier), FrontierFile{
		Primary:    primary,
		Constraint: constraint,
		Best: FrontierBest{
			ID: best.ID(), ConstraintSatisfied: satisfied, Note: note,
			Means: best.Means, Prompt: best.Candidate.Prompt,
		},
		Members:     views,
		SampleIDs:   in.SampleIDs,
		Rounds:      in.Result.Rounds,
		Reason:      in.Result.Reason,
		UsageByRole: in.Result.Usage,
		GeneratedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("write frontier.json: %w", err)
	}
	if err := writeFile(filepath.Join(runDir, fileReport), renderReport(in, f, views, best, satisfied, note)); err != nil {
		return fmt.Errorf("write report.md: %w", err)
	}
	return nil
}

// renderReport produces the Chinese run report: overview (rounds,
// stop reason, per-role budget, optimizer valve), the Top-1 prompt,
// the frontier table, the trade-off notes and the lineage summary.
func renderReport(in ReportInputs, f *Frontier, views []FrontierMember, best Member, satisfied bool, note string) string {
	primary := in.Task.Primary()
	res := in.Result
	var b strings.Builder

	b.WriteString("# 提示词优化报告\n\n")
	b.WriteString("## 概览\n\n")
	fmt.Fprintf(&b, "- 优化轮数：%d（终止原因：%s）\n", res.Rounds, res.Reason)
	b.WriteString("- 预算消耗（分角色）：\n")
	for _, role := range []core.Role{core.RoleExecutor, core.RoleJudge, core.RoleOptimizer} {
		u := res.Usage[role]
		fmt.Fprintf(&b, "  - %s：prompt=%d，completion=%d，合计=%d\n", role, u.PromptTokens, u.CompletionTokens, u.Total())
	}
	switch {
	case in.OptBudgetTokens <= 0:
		b.WriteString("- 优化侧 token 阀门：未启用（--budget-opt-tokens=0，不限）\n")
	case in.OptValveTripped:
		fmt.Fprintf(&b, "- 优化侧 token 阀门：已触发（上限 %d tokens，软停转 budget_stopped）\n", in.OptBudgetTokens)
	default:
		fmt.Fprintf(&b, "- 优化侧 token 阀门：未触发（上限 %d tokens）\n", in.OptBudgetTokens)
	}
	if res.Reason == ReasonBudgetStopped {
		b.WriteString("- 预算终止时已交付当前最优候选，未空手而归\n")
	}
	b.WriteString("\n")

	b.WriteString("## 最优提示词（Top-1）\n\n")
	fmt.Fprintf(&b, "- 候选 ID：%s（算子 %s，第 %d 轮）\n", best.ID(), best.Operator, best.Round)
	fmt.Fprintf(&b, "- 主指标 %s 均值：%.4f\n", primary, best.Means[primary])
	if in.Constraint != "" {
		state := "满足"
		if !satisfied {
			state = "不满足（兜底交付）"
		}
		fmt.Fprintf(&b, "- 约束 %s：均值 %.4f，%s\n", in.Constraint, best.Means[in.Constraint], state)
	}
	if in.BaselineGaps > 0 {
		fmt.Fprintf(&b, "- 注：baseline 有 %d 个缺分样本格，按 0 计入支配与均值\n", in.BaselineGaps)
	}
	b.WriteString("\n提示词全文：\n\n```\n")
	b.WriteString(best.Candidate.Prompt)
	b.WriteString("\n```\n\n")

	b.WriteString("## 前沿成员\n\n")
	fmt.Fprintf(&b, "| 候选 | 轮次 | 算子 | %s 均值 |", primary)
	if in.Constraint == "json_validator" {
		b.WriteString(" JSON 合法率 |")
	}
	b.WriteString(" 独占占优样本 |\n|---|---|---|---|")
	if in.Constraint == "json_validator" {
		b.WriteString("---|")
	}
	b.WriteString("---|\n")
	for _, v := range views {
		fmt.Fprintf(&b, "| %s | %d | %s | %.4f |", v.ID, v.Round, v.Operator, v.PrimaryMean)
		if in.Constraint == "json_validator" {
			fmt.Fprintf(&b, " %.4f |", v.JSONRate)
		}
		fmt.Fprintf(&b, " %d |\n", v.Wins)
	}
	b.WriteString("\n")

	b.WriteString("## 取舍说明\n\n")
	b.WriteString("- 支配关系按保留集逐样本主指标分计算（非支配准入、被支配淘汰、向量克隆拒绝），单调性由传递性保证。\n")
	b.WriteString("- 约束指标（json_validator）不参与支配（量纲不混入），仅作 Top-1 硬过滤。\n")
	b.WriteString("- Top-1 选取：约束均值=1 硬过滤 → 主指标降序 → 约束均值 → ID 序。\n")
	b.WriteString("- 预算中断的候选（假设探针或子代）评估不完整，缺格不参与支配与排序：探针直接跳过，子代保留 lineage 标记但不入前沿。\n")
	if note != "" {
		fmt.Fprintf(&b, "- %s。\n", note)
	}
	b.WriteString("\n")

	b.WriteString("## 谱系\n\n")
	for _, rec := range in.Lineage.Records() {
		origin := rec.Operator
		if len(rec.Parents) > 0 {
			origin = fmt.Sprintf("%s ← %s", origin, strings.Join(rec.Parents, " + "))
		}
		hypo := ""
		if len(rec.Hypotheses) > 0 {
			ids := make([]string, 0, len(rec.Hypotheses))
			for _, h := range rec.Hypotheses {
				ids = append(ids, h.ID)
			}
			hypo = fmt.Sprintf("（假设 %s）", strings.Join(ids, ","))
		}
		fmt.Fprintf(&b, "- %s：第 %d 轮，%s%s，主指标均值 %.4f，%s\n",
			rec.ID, rec.Round, origin, hypo, rec.PrimaryMean, admittedLabel(rec))
	}
	return b.String()
}

// admittedLabel renders the frontier verdict, flagging budget-truncated
// children whose zero-filled score cells are not real zeros.
func admittedLabel(rec LineageRecord) string {
	if rec.Incomplete {
		return "预算中断评估不完整（缺格记 0，未参与前沿）"
	}
	if rec.Admitted {
		return "准入前沿"
	}
	return "未准入"
}

// writeFile writes content through the same temp+rename contract as
// saveJSON.
func writeFile(path, content string) error {
	return writeAtomic(path, []byte(content))
}

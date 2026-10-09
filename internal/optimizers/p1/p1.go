// Package p1 implements the p¹-style budget-allocation optimizer
// (提案 §3.1). It is deliberately NOT a new search engine: the round
// loop is the GEPA reflective engine run over S*, a minimal
// discriminative subset carved from the retained set by the probe
// variance the synthesis pipeline already measured (zero extra
// probe spend), and the delivered Best gets one full retained-set
// final evaluation before Finish. The mechanism is a
// budget-allocation heuristic inspired by the p¹ filtering method
// (arXiv:2604.08801); 本实现不宣称复现论文数字，文档措辞为
// 「p¹ 式预算分配」。
package p1

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// Paradigm identity: artifact/event names. select_done/final_done are
// paradigm-specific, so they carry Detail{paradigm, stage} for the
// live dashboard's default branch (docs/plugins.md event contract).
const (
	// paradigmName tags this paradigm's events; the registry name is
	// "p1" (internal/optimizers/builtin).
	paradigmName = "p1"
	// searchDirName parks the inner GEPA search's artifacts (lineage,
	// evals, opt-calls, its own report) in a subtree of the run dir,
	// keeping the run root's artifacts the authoritative
	// full-retained-set picture (baseline row + final evaluation).
	searchDirName = "p1-search"
	// finalID and OpFinal identify the delivered final evaluation in
	// the lineage and on the frontier.
	finalID = "p1-final"
	OpFinal = "final"
	// EventSelectDone fires once after S* is chosen; EventFinalDone
	// fires after the final evaluation (or its skip/truncation).
	EventSelectDone = "select_done"
	EventFinalDone  = "final_done"
)

// Stage names carried by paradigm events (Detail.stage).
const (
	stageSelect = "select"
	stageFinal  = "final"
)

// P1 is the p¹-style budget-allocation engine.Optimizer. It is
// stateless and safe for concurrent use; per-run state lives inside
// Optimize.
type P1 struct{}

var _ engine.Optimizer = (*P1)(nil)

// New returns the instance the registry factory hands out.
func New() *P1 { return &P1{} }

// Optimize allocates the remaining evaluation budget onto a minimal
// discriminative set and runs the GEPA search over it:
//
//  1. read the probe variance report the synthesis pipeline already
//     produced (req.SynthDir/filter.json) — a missing report is an
//     explicit error, never a silent fallback;
//  2. pick S* = top-m kept samples by probe variance, m backed out of
//     the remaining executor budget (planStar);
//  3. phase A — engine.Gepa over a sub-request narrowed to S*, its
//     artifacts parked under runs/<id>/p1-search/;
//  4. phase B — one full retained-set final evaluation of the search's
//     Best through a fresh Loop over the original request, so the run
//     root's frontier/report compare the final candidate against the
//     fully measured baseline.
//
// The baseline row is always delivered, so Best is never empty.
func (p *P1) Optimize(ctx context.Context, req engine.Request) (engine.Result, error) {
	// ① probe variance report. The CLI cannot reach a missing report:
	// p1 only routes/runs in the zero-config mode, whose pipeline
	// always writes filter.json, and the configured mode rejects
	// --optimizer outright — this branch guards direct engine.Request
	// constructions (tests, future callers) and refuses to guess a
	// synthesis root it was not handed.
	if req.SynthDir == "" {
		return engine.Result{}, errors.New("p1: Request.SynthDir 为空：p¹ 式预算分配需要零配置合成的探针方差报告（synth/<id>/filter.json），无报告即无分配依据，拒绝静默降级")
	}
	report, err := harness.LoadFilterReport(req.SynthDir)
	if err != nil {
		return engine.Result{}, fmt.Errorf("p1: 读取探针方差报告 %s 失败: %w", filepath.Join(req.SynthDir, "filter.json"), err)
	}
	kept := req.Samples
	if len(kept) == 0 {
		return engine.Result{}, errors.New("p1: 保留样本集为空")
	}

	// ② S* = kept 中方差 Top-m，m 由剩余预算反推。
	used, _ := req.Budget.Snapshot()
	m, remaining := planStar(len(kept), max(req.Params.MaxRounds, 1), max(req.Params.Minibatch, 1), req.Budget.EvalLimit(), used)
	star := selectStar(kept, report, m)
	emit(req, EventSelectDone, map[string]any{
		"paradigm": paradigmName, "stage": stageSelect,
		"kept": len(kept), "s_star": len(star),
		"remaining_evals": remaining, "samples": sampleIDs(star),
	})

	// ③ phase A — the GEPA reflective search over S* only: Loop.
	// Evaluate's samples parameter is the paradigm contract, so a
	// narrowed sub-request is the whole integration. The shared
	// Budget pointer keeps probe/baseline/round spend in one account.
	search := req
	search.Samples = star
	search.RunDir = filepath.Join(req.RunDir, searchDirName)
	search.OnEvent = stampParadigm(req.OnEvent)
	inner, err := (&engine.Gepa{}).Optimize(ctx, search)
	if err != nil {
		return engine.Result{}, fmt.Errorf("p1: S* 搜索轮失败: %w", err)
	}
	reason, rounds := inner.Reason, inner.Rounds

	// ④ phase B — the full retained-set final evaluation. A fresh Loop
	// over the ORIGINAL request seeds the run-root frontier with the
	// fully measured baseline row; the final candidate joins it with a
	// real full-coverage row, so Best ordering compares like with
	// like. (The inner search's optimizer-side valve state lives in
	// p1-search/report.json; the run-root report's optimizer-role
	// token totals still come from the shared budget.)
	outer, err := engine.NewLoop(req)
	if err != nil {
		return engine.Result{}, err
	}
	if ctx.Err() != nil {
		emit(req, EventFinalDone, map[string]any{
			"paradigm": paradigmName, "stage": stageFinal, "skipped": true, "aborted": true,
		})
		return outer.Finish(engine.ReasonAborted, rounds)
	}
	if inner.Best.ID == req.Initial.ID {
		// No child beat the baseline on S*: nothing earned the final
		// evaluation — the baseline row is already fully measured in
		// req.Baseline, and Finish delivers it with the search's
		// terminal reason.
		emit(req, EventFinalDone, map[string]any{
			"paradigm": paradigmName, "stage": stageFinal, "skipped": true,
		})
		return outer.Finish(reason, rounds)
	}
	final := core.Candidate{
		ID:          finalID,
		Name:        "p1-final",
		Description: "S* 搜索最优候选在全保留集上的终评",
		Prompt:      inner.Best.Prompt,
	}
	res, records, uerr := outer.Evaluate(ctx, final, kept, rounds+1)
	if uerr != nil {
		return engine.Result{}, uerr
	}
	detail := map[string]any{"paradigm": paradigmName, "stage": stageFinal, "candidate": final.ID}
	if res.Undispatched > 0 {
		// The budget ran dry before the final evaluation could
		// complete: undispatched cells would read as fake zeros in
		// dominance and Best ordering, so the partial row never joins
		// the frontier — the trail is flagged incomplete and the
		// current best (the baseline) is delivered.
		if err := outer.MarkIncomplete(final, OpFinal, []string{inner.Best.ID}, nil, rounds+1, res.Undispatched, records); err != nil {
			return engine.Result{}, err
		}
		detail["undispatched"] = res.Undispatched
		emit(req, EventFinalDone, detail)
		return outer.Finish(engine.ReasonBudgetStopped, rounds)
	}
	admitted, aerr := outer.Admit(final, OpFinal, []string{inner.Best.ID}, nil, rounds+1, res, records)
	if aerr != nil {
		return engine.Result{}, aerr
	}
	detail["admitted"] = admitted
	emit(req, EventFinalDone, detail)
	return outer.Finish(reason, rounds)
}

// planStar derives |S*| from the remaining executor budget. 提案 §3.1:
// 总预算 ≈ 探针已花 + baseline 已花 + rounds×|S*| + kept(终评) — the
// shared budget has already absorbed the probe and baseline spend by
// the time the optimizer runs, so the search rounds get
// remaining − kept, split evenly over the rounds. One correction to
// the raw formula: a progressing GEPA round also validates hypotheses
// over minibatch probes before its |S*| child pass, so each round
// reserves min(Minibatch, m0) evaluations for one probe — without the
// reserve the final evaluation is starved by exactly the probe cost
// and p1 would never deliver a full-set verdict. An unlimited budget
// (limit 0) skips the allocation entirely: S* = kept.
func planStar(kept int, rounds, minibatch int, evalLimit, used int64) (m int, remaining int64) {
	if evalLimit <= 0 {
		return kept, 0
	}
	remaining = evalLimit - used
	budget := remaining - int64(kept) // what the search rounds may spend
	m0 := int64(0)
	if budget > 0 {
		m0 = budget / int64(rounds)
	}
	reserve := min(int64(minibatch), m0)
	m = int(max(1, min(m0-reserve, int64(kept))))
	return m, remaining
}

// selectStar ranks the retained set by probe variance (descending;
// ties keep the retained order) and returns the top m in retained
// order. Samples the report does not mention (e.g. added during a
// checkpoint pause) carry no probe evidence and rank last with
// variance 0 — the conservative end of the ordering.
func selectStar(kept []core.Sample, report harness.FilterReport, m int) []core.Sample {
	variance := make(map[string]float64, len(report.PerSample))
	for _, pv := range report.PerSample {
		variance[pv.ID] = pv.Variance
	}
	order := make([]int, len(kept))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(variance[kept[b].ID], variance[kept[a].ID])
	})
	picked := order[:min(m, len(order))]
	slices.Sort(picked)
	out := make([]core.Sample, 0, len(picked))
	for _, i := range picked {
		out = append(out, kept[i])
	}
	return out
}

// stampParadigm tags the inner GEPA search's events with the p1
// paradigm so the event stream reads as one run; events already
// carrying a paradigm tag pass through untouched.
func stampParadigm(next func(eval.Event)) func(eval.Event) {
	if next == nil {
		return nil
	}
	return func(ev eval.Event) {
		if ev.Detail == nil {
			ev.Detail = map[string]any{}
		}
		if _, ok := ev.Detail["paradigm"]; !ok {
			ev.Detail["paradigm"] = paradigmName
		}
		next(ev)
	}
}

// emit publishes one p1 event on the run's event stream — the same
// shape Loop.Emit produces; p1 emits outside any loop for the select
// stage.
func emit(req engine.Request, typ string, detail map[string]any) {
	if req.OnEvent == nil {
		return
	}
	req.OnEvent(eval.Event{Type: typ, Time: time.Now(), RunID: req.RunID, Detail: detail})
}

func sampleIDs(samples []core.Sample) []string {
	out := make([]string, len(samples))
	for i, s := range samples {
		out[i] = s.ID
	}
	return out
}

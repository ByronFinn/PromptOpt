package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
)

// Loop is the per-run scaffold shared by every optimizer paradigm: it
// validates the request, seeds the frontier and lineage with the
// baseline row (so Best is never empty), evaluates candidates through
// per-unit eval engines rooted at evals/<seq>-<id>/, admits children
// onto the Pareto frontier and writes the terminal artifacts. The
// paradigm owns its search decisions and drives the loop from outside;
// GEPA (gepa.go) is the reference consumer and the V5 paradigm
// subpackages follow the same shape (docs/plugins.md).
//
// Concurrency: Loop methods run on the paradigm's main goroutine and
// the struct is not safe for concurrent use. Evaluate is the one seam
// where events arrive from eval worker goroutines; they only reach the
// guarded unitCollector, never Loop state directly.
type Loop struct {
	req          Request
	primary      string
	constraint   string
	frontier     *Frontier
	lineage      *Lineage
	adv          *Advisor
	baseline     Member
	baselineGaps int
	seq          int                                // evaluation unit sequence
	records      map[string]map[string]SampleRecord // candidate id → sample id → record
}

// NewLoop validates req (Params and Request constraints) and seeds the
// scaffold: the baseline row joins the frontier and the lineage, so
// Finish delivers a non-empty Best even when every round fails or the
// budget stops the run before round 1.
func NewLoop(req Request) (*Loop, error) {
	var errs []error
	if err := req.Params.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := req.validate(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("engine request: %w", err)
	}
	constraint := ""
	if slices.Contains(req.Task.Metrics, "json_validator") {
		constraint = "json_validator"
	}
	primary := req.Task.Primary()
	row, means, sdRow, gaps := baselineRow(req, primary)
	l := &Loop{
		req: req, primary: primary, constraint: constraint,
		frontier: &Frontier{}, records: map[string]map[string]SampleRecord{},
		adv: NewAdvisor(req),
		baseline: Member{
			Candidate: req.Initial, Scores: row, Means: means,
			SD: sdRow, Reps: req.Reps, Round: 0, Operator: OpBaseline,
		},
		baselineGaps: gaps,
	}
	l.frontier.Add(l.baseline)
	l.Record(req.Initial.ID, req.Baseline)
	lin, err := LoadOrInitLineage(filepath.Join(req.RunDir, fileLineage))
	if err != nil {
		return nil, err
	}
	if err := lin.Append(LineageRecord{
		ID: req.Initial.ID, Operator: OpBaseline, Round: 0,
		Scores: row, PrimaryMean: mean(row), Admitted: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	l.lineage = lin
	return l, nil
}

// Request returns the driving request.
func (l *Loop) Request() Request { return l.req }

// Advisor returns the optimizer-side LLM dialer.
func (l *Loop) Advisor() *Advisor { return l.adv }

// Primary returns the task's primary metric name.
func (l *Loop) Primary() string { return l.primary }

// Constraint returns "json_validator" when the task declares it, else
// the empty string (the Best hard filter stays off).
func (l *Loop) Constraint() string { return l.constraint }

// Frontier returns the Pareto frontier store.
func (l *Loop) Frontier() *Frontier { return l.frontier }

// Lineage returns the persisted candidate lineage.
func (l *Loop) Lineage() *Lineage { return l.lineage }

// Stopped reports whether the run must stop before the next unit: the
// executor soft stop armed or the optimizer token valve tripped.
func (l *Loop) Stopped() bool {
	return l.req.Budget.SoftStopped() || l.adv.ValveTripped()
}

// NotifyValve emits the optimizer-role budget_stop event once.
func (l *Loop) NotifyValve() { l.adv.notifyValve() }

// Emit publishes one engine event on the run's event stream (SSE +
// events.jsonl). Paradigm-specific events are allowed; the live
// dashboard renders unknown types through its default branch, so
// carrying Detail{paradigm, stage} keeps them readable.
func (l *Loop) Emit(typ string, detail map[string]any) {
	if l.req.OnEvent != nil {
		l.req.OnEvent(eval.Event{Type: typ, Time: time.Now(), RunID: l.req.RunID, Detail: detail})
	}
}

// Evaluate evaluates one candidate over samples through a fresh
// eval.Engine rooted at runs/<id>/evals/<seq>-<cand>/, forwards the
// whitelisted inner events stamped with Detail{candidate, round},
// harvests the per-sample records into the loop's evidence store and
// returns them alongside the unit summary. Callers check
// res.Undispatched: a partially dispatched unit's records still land
// in the store, but the row must not join the frontier
// (MarkIncomplete).
func (l *Loop) Evaluate(ctx context.Context, cand core.Candidate, samples []core.Sample, round int) (core.RunResult, []SampleRecord, error) {
	l.seq++
	unitDir := filepath.Join(l.req.RunDir, evalsDir, fmt.Sprintf("%02d-%s", l.seq, sanitizeID(cand.ID)))
	coll := newUnitCollector()
	unit := &eval.Engine{
		RunID:                   l.req.RunID,
		RunDir:                  unitDir,
		Model:                   l.req.Model,
		MaxTokens:               l.req.MaxTokens,
		Workers:                 max(l.req.Workers, 1),
		Metrics:                 l.req.Task.Metrics,
		Budget:                  l.req.Budget,
		Provider:                l.req.Provider,
		JudgeProvider:           l.req.JudgeProvider,
		JudgeModel:              l.req.JudgeModel,
		JudgeMaxTokens:          l.req.JudgeMaxTokens,
		JudgeBackend:            l.req.JudgeBackend,
		DecisionClient:          l.req.DecisionClient,
		JudgeDecisionConfidence: l.req.JudgeDecisionConfidence,
		JudgeDecisionDiagBelow:  l.req.JudgeDecisionDiagBelow,
		ExtraBody:               l.req.ExtraBody,
		Temperature:             l.req.Temperature,
		Reps:                    l.req.Reps,
		TaskName:                l.req.Task.Name,
		CandidateID:             cand.ID,
		DatasetName:             "synth",
		OnEvent: func(ev eval.Event) {
			coll.observe(ev)
			l.forward(ev, cand.ID, round)
		},
	}
	res, err := unit.Run(ctx, cand, samples)
	if err != nil {
		return res, nil, fmt.Errorf("evaluate unit %s: %w", cand.ID, err)
	}
	records := coll.records(samples)
	l.Record(cand.ID, records)
	return res, records, nil
}

// Record stores one candidate's per-sample records for later
// reflection contexts; BatchRecords reads them back.
func (l *Loop) Record(candID string, records []SampleRecord) {
	m := make(map[string]SampleRecord, len(records))
	for _, rec := range records {
		m[rec.Sample.ID] = rec
	}
	l.records[candID] = m
}

// BatchRecords returns the candidate's records over batch, in batch
// order. Samples without evidence surface as zero-score records so the
// reflection context still shows them.
func (l *Loop) BatchRecords(candID string, batch []core.Sample) []SampleRecord {
	recs := l.records[candID]
	out := make([]SampleRecord, 0, len(batch))
	for _, s := range batch {
		if rec, ok := recs[s.ID]; ok {
			out = append(out, rec)
			continue
		}
		out = append(out, SampleRecord{Sample: s})
	}
	return out
}

// Admit projects records onto the fixed sample order (missing cells
// count as 0, keeping the primary mean consistent with dominance
// input), adds the child to the frontier — dominated, cloned or
// ε-indistinguishable children are rejected but still recorded in the
// lineage — emits frontier_updated and appends the lineage record. It
// returns whether the child was admitted.
//
// Noise gate: when both the child's row and a current member's row
// carry per-sample SD (both sides measured with Reps > 1), admission
// goes through AddNoiseAware with the pooled per-dimension margin
// max(child.SD[i], cur.SD[i]) — conservative pooling, an advantage
// smaller than the noise band must not decide. A mixed pair (SD on
// one side only) cannot arise on the wired path (pipeline baseline and
// unit evaluations share the same Reps); it can only come from a
// hand-edited artifact, and then the comparison degrades to plain Add
// semantics — no margin protection, but no false eviction either.
func (l *Loop) Admit(child core.Candidate, op string, parents []string, hyps []Hypothesis, round int, res core.RunResult, records []SampleRecord) (bool, error) {
	childRow := recordsRow(records, l.req.Samples, l.primary)
	childSD := recordsSDRow(records, l.req.Samples, l.primary)
	childMeans := maps.Clone(res.MetricMeans)
	if childMeans == nil {
		childMeans = make(map[string]float64)
	}
	childMeans[l.primary] = mean(childRow)
	marginOf := func(cur Member) []float64 {
		if len(childSD) == 0 || len(cur.SD) == 0 {
			// 混合态（单侧有 SD，异常工件）：退化为 Add 语义。
			return nil
		}
		eps := make([]float64, len(childSD))
		for i := range eps {
			eps[i] = max(childSD[i], cur.SD[i])
		}
		return eps
	}
	admitted, evicted := l.frontier.AddNoiseAware(Member{
		Candidate: child, Scores: childRow, Means: childMeans,
		SD: childSD, Reps: l.req.Reps, Round: round, Operator: op,
	}, marginOf)
	l.Emit(EventFrontierUpdated, map[string]any{
		"round": round, "candidate": child.ID, "admitted": admitted,
		"evicted": evicted, "frontier_size": l.frontier.Size(),
	})
	if err := l.lineage.Append(LineageRecord{
		ID: child.ID, Parents: parents, Operator: op, Hypotheses: hyps, Round: round,
		Scores: childRow, PrimaryMean: mean(childRow), Admitted: admitted,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return admitted, err
	}
	return admitted, nil
}

// MarkIncomplete records a budget-interrupted child: its partial row's
// undispatched cells would read as real zeros in dominance and Best
// ordering (a half-evaluated child could evict a fully evaluated
// member scoring 0 on exactly those cells), so it never joins the
// frontier — the lineage trail keeps the partial evidence flagged
// Incomplete instead.
func (l *Loop) MarkIncomplete(child core.Candidate, op string, parents []string, hyps []Hypothesis, round int, undispatched int, records []SampleRecord) error {
	partialRow := recordsRow(records, l.req.Samples, l.primary)
	l.Emit(EventFrontierUpdated, map[string]any{
		"round": round, "candidate": child.ID, "admitted": false,
		"incomplete": true, "undispatched": undispatched,
		"frontier_size": l.frontier.Size(),
	})
	return l.lineage.Append(LineageRecord{
		ID: child.ID, Parents: parents, Operator: op, Hypotheses: hyps, Round: round,
		Scores: partialRow, PrimaryMean: mean(partialRow), Admitted: false, Incomplete: true,
		CreatedAt: time.Now().UTC(),
	})
}

// Finish assembles the terminal Result — Best over the frontier under
// the constraint filter — and writes the report artifacts
// (lineage.json is already persisted incrementally by Append).
func (l *Loop) Finish(reason string, rounds int) (Result, error) {
	_, usage := l.req.Budget.Snapshot()
	best, satisfied, _ := l.frontier.Best(l.primary, l.constraint)
	res := Result{
		Best:                best.Candidate,
		BestMeans:           maps.Clone(best.Means),
		ConstraintSatisfied: satisfied,
		Frontier:            l.frontier.Members(),
		Rounds:              rounds,
		Reason:              reason,
		Usage:               usage,
	}
	// SampleIDs fixes the column order of Member.Scores on disk (the
	// row projection follows req.Samples; recordsRow).
	sampleIDs := make([]string, len(l.req.Samples))
	for i, s := range l.req.Samples {
		sampleIDs[i] = s.ID
	}
	if err := WriteOutputs(l.req.RunDir, ReportInputs{
		Task: l.req.Task, Lineage: l.lineage, Result: res, Constraint: l.constraint,
		SampleIDs:       sampleIDs,
		OptBudgetTokens: l.adv.optBudgetTokens, OptValveTripped: l.adv.ValveFired(),
		BaselineGaps: l.baselineGaps,
	}, l.frontier); err != nil {
		return Result{}, err
	}
	return res, nil
}

// forward relays whitelisted inner events with the candidate/round
// stamp; the inner run lifecycle stays internal (an inner run_done
// would cut the SSE stream and the replay short).
func (l *Loop) forward(ev eval.Event, candID string, round int) {
	if l.req.OnEvent == nil {
		return
	}
	switch ev.Type {
	case eval.EventRunStart, eval.EventRunDone:
		return
	case eval.EventSampleStart, eval.EventSampleDone, eval.EventBudgetStop:
	default:
		return
	}
	detail := make(map[string]any, len(ev.Detail)+2)
	maps.Copy(detail, ev.Detail)
	detail["candidate"] = candID
	detail["round"] = round
	ev.Detail = detail
	l.req.OnEvent(ev)
}

// unitCollector harvests one unit's sample_done events into records;
// engine events arrive from worker goroutines, so state is guarded
// (harness.scoreCollector precedent).
type unitCollector struct {
	mu      sync.Mutex
	harvest map[string]SampleRecord
}

func newUnitCollector() *unitCollector {
	return &unitCollector{harvest: make(map[string]SampleRecord)}
}

func (c *unitCollector) observe(ev eval.Event) {
	if ev.Type != eval.EventSampleDone {
		return
	}
	rec := SampleRecord{Response: ev.Response, Scores: ev.Scores, ScoresSD: ev.ScoresSD, Diagnosis: ev.Diagnosis}
	if ev.Error != "" {
		rec.Diagnosis = map[string]string{"error": ev.Error}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.harvest[ev.SampleID] = rec
}

// records joins the harvest with samples in order, filling the Sample
// field; failed samples carry an error diagnosis.
func (c *unitCollector) records(samples []core.Sample) []SampleRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SampleRecord, 0, len(samples))
	for _, s := range samples {
		if rec, ok := c.harvest[s.ID]; ok {
			rec.Sample = s
			out = append(out, rec)
		}
	}
	return out
}

// --- shared row projection helpers ------------------------------------------

// baselineRow builds the baseline score row and means from the request
// records; missing primary cells count as 0 and are counted as gaps.
// The returned sd row projects the records' per-sample in-sample
// standard deviation (nil when no record carries one — the
// single-shot shape).
func baselineRow(req Request, primary string) (row []float64, means map[string]float64, sd []float64, gaps int) {
	byID := make(map[string]SampleRecord, len(req.Baseline))
	for _, rec := range req.Baseline {
		byID[rec.Sample.ID] = rec
	}
	sums := make(map[string]float64, len(req.Task.Metrics))
	for _, m := range req.Task.Metrics {
		sums[m] = 0
	}
	row = make([]float64, len(req.Samples))
	hasSD := false
	sdRow := make([]float64, len(req.Samples))
	for i, s := range req.Samples {
		rec, ok := byID[s.ID]
		if !ok {
			gaps++
			continue
		}
		if rec.ScoresSD != nil {
			hasSD = true
			sdRow[i] = rec.ScoresSD[primary]
		}
		seen := false
		for m, v := range rec.Scores {
			if _, declared := sums[m]; declared {
				sums[m] += v
			}
			if m == primary {
				row[i] = v
				seen = true
			}
		}
		if !seen {
			gaps++
		}
	}
	if !hasSD {
		sdRow = nil
	}
	n := float64(len(req.Samples))
	means = make(map[string]float64, len(sums))
	for m, sum := range sums {
		means[m] = sum / n
	}
	return row, means, sdRow, gaps
}

// recordsRow projects harvested records onto the fixed sample order;
// missing cells count as 0.
func recordsRow(records []SampleRecord, samples []core.Sample, primary string) []float64 {
	byID := make(map[string]SampleRecord, len(records))
	for _, rec := range records {
		byID[rec.Sample.ID] = rec
	}
	row := make([]float64, len(samples))
	for i, s := range samples {
		row[i] = byID[s.ID].Scores[primary]
	}
	return row
}

// recordsSDRow projects the records' per-sample in-sample sd onto the
// fixed sample order; nil when no record carries one, so the SD
// pipeline stays off for single-shot evidence.
func recordsSDRow(records []SampleRecord, samples []core.Sample, primary string) []float64 {
	byID := make(map[string]SampleRecord, len(records))
	any := false
	for _, rec := range records {
		byID[rec.Sample.ID] = rec
		if rec.ScoresSD != nil {
			any = true
		}
	}
	if !any {
		return nil
	}
	row := make([]float64, len(samples))
	for i, s := range samples {
		row[i] = byID[s.ID].ScoresSD[primary]
	}
	return row
}

// recordsMean is the primary mean over harvested records, missing
// cells counting as 0 — consistent with the row projection.
func recordsMean(records []SampleRecord, primary string) float64 {
	if len(records) == 0 {
		return 0
	}
	sum := 0.0
	for _, rec := range records {
		sum += rec.Scores[primary]
	}
	return sum / float64(len(records))
}

func mean(row []float64) float64 {
	if len(row) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range row {
		sum += v
	}
	return sum / float64(len(row))
}

// sanitizeID keeps candidate ids filesystem-safe (private copy of the
// eval helper).
func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, id)
}

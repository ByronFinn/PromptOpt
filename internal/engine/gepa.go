package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
)

// Engine lifecycle events (SSE + events.jsonl). The engine never
// emits run_done — the terminal run_done belongs to the cmd layer;
// inner evaluation units' run_start/run_done stay internal too
// (dropped by evaluateUnit) so live streams and replays are not cut
// short mid-optimization.
const (
	EventRoundStart      = "round_start"
	EventReflectDone     = "reflect_done"
	EventHypoValidated   = "hypotheses_validated"
	EventMutateDone      = "mutate_done"
	EventFrontierUpdated = "frontier_updated"
	EventVistaRestart    = "vista_restart"
	EventRoundDone       = "round_done"
)

// Loop-wide tuning constants.
const (
	maxHypotheses = 3 // reflection pool size
	maxLessons    = 6 // ancestor lessons fed to reflection/mutation
	childIDFormat = "g%02d"
	evalsDir      = "evals"
	minibatchHint = "\n\n补充指导："
)

// Gepa is the GEPA reflective-evolution Optimizer: per-round it
// reflects a parent's minibatch failures into hypotheses, validates
// them as temporary candidates, selects one ε-greedily (VISTA
// decoupling), mutates via the operator ladder (Rewrite → Merge →
// Fresh restart) and admits the child onto the Pareto frontier. It is
// stateless and safe for concurrent use.
type Gepa struct{}

var _ Optimizer = (*Gepa)(nil)

// Optimize runs the loop for req and returns the terminal state with
// the frontier/report artifacts written under req.RunDir. The
// baseline row is always delivered, so Best is never empty.
func (g *Gepa) Optimize(ctx context.Context, req Request) (Result, error) {
	var errs []error
	if err := req.Params.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := req.validate(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Result{}, fmt.Errorf("engine request: %w", err)
	}

	constraint := ""
	if slices.Contains(req.Task.Metrics, "json_validator") {
		constraint = "json_validator"
	}
	r := &gepaRun{
		req:        req,
		rng:        rand.New(rand.NewPCG(uint64(req.Params.Seed), uint64(req.Params.Seed))),
		frontier:   &Frontier{},
		records:    map[string]map[string]SampleRecord{},
		primary:    req.Task.Primary(),
		constraint: constraint,
	}
	r.adv = newAdvisor(req)
	r.reflector = NewReflector(r.adv)
	r.mutator = NewMutator(r.adv)
	r.vista = NewVistaGuard(req.Params.Epsilon, req.Params.StagnationLimit, r.rng)
	return r.run(ctx)
}

// gepaRun holds one Optimize invocation's mutable state.
type gepaRun struct {
	req        Request
	rng        *rand.Rand
	adv        *advisor
	reflector  *Reflector
	mutator    *Mutator
	vista      *VistaGuard
	frontier   *Frontier
	records    map[string]map[string]SampleRecord // candidate id → sample id → record
	seq        int                                // evaluation unit sequence
	primary    string
	constraint string
}

// run executes the round loop (§1 of the design).
func (g *gepaRun) run(ctx context.Context) (Result, error) {
	req := g.req

	// Seed the frontier with the baseline row (missing score cells
	// record as 0; the report flags them).
	row, means, baselineGaps := baselineRow(req, g.primary)
	baseline := Member{
		Candidate: req.Initial, Scores: row, Means: means,
		Round: 0, Operator: OpBaseline,
	}
	g.frontier.Add(baseline)
	g.harvest(req.Initial.ID, req.Baseline)

	lin, err := LoadOrInitLineage(filepath.Join(req.RunDir, "lineage.json"))
	if err != nil {
		return Result{}, err
	}
	if err := lin.Append(LineageRecord{
		ID: req.Initial.ID, Operator: OpBaseline, Round: 0,
		Scores: row, PrimaryMean: mean(row), Admitted: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return Result{}, err
	}

	reason := ReasonRoundsDone
	rounds := 0
	bestMean := mean(row)

	for r := 1; r <= req.Params.MaxRounds; r++ {
		// ① gates: cancellation beats budget exhaustion.
		if ctx.Err() != nil {
			reason = ReasonAborted
			break
		}
		if req.Budget.SoftStopped() || g.adv.ValveTripped() {
			g.adv.notifyValve()
			reason = ReasonBudgetStopped
			break
		}
		g.emit(EventRoundStart, map[string]any{"round": r})

		// ② parent: baseline on round 1, uniform frontier draw after.
		parent := baseline
		if r > 1 {
			parent = g.frontier.UniformPick(g.rng)
		}
		// ③ minibatch: rng draw of K, all when fewer.
		batch := pickMinibatch(g.rng, req.Samples, req.Params.Minibatch)

		// ④ reflection (optimizer role).
		lessons := lin.Lessons(parent.ID(), maxLessons)
		hyps, rerr := g.reflector.Reflect(ctx, req.Task, parent.Candidate, lessons, g.batchRecords(parent.ID(), batch), maxHypotheses)
		if rerr != nil {
			if errors.Is(rerr, errOptBudget) {
				reason = ReasonBudgetStopped
				break
			}
			// Failure resilience: stagnate and skip the round.
			g.vista.Record(false)
			g.emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": rerr.Error()})
			rounds = r
			continue
		}
		g.emit(EventReflectDone, map[string]any{"round": r, "hypotheses": len(hyps)})

		// ⑤ hypothesis validation: hypotheses serial (deterministic
		// budget order, filter.go precedent), workers parallel inside
		// one unit. Temporary candidate = parent + guidance.
		parentBatchMean := g.batchMean(parent, batch)
		validated := make([]ValidatedHypothesis, 0, len(hyps))
		unitUndispatched := false
		for _, h := range hyps {
			if ctx.Err() != nil {
				break
			}
			temp := core.Candidate{
				ID:     fmt.Sprintf("%s-probe-%s", parent.ID(), h.ID),
				Prompt: parent.Candidate.Prompt + minibatchHint + cleanInputLiteral(h.Text),
			}
			res, records, uerr := g.evaluateUnit(ctx, temp, batch, r)
			if uerr != nil {
				return Result{}, uerr
			}
			g.harvest(temp.ID, records)
			if res.Undispatched > 0 {
				// The budget ran dry mid-probe: the missing cells would
				// inflate the mean's denominator or read as fake zeros,
				// so the lift is not comparable with the parent's. Skip
				// the hypothesis; the round closes budget-stopped below.
				unitUndispatched = true
				continue
			}
			m := recordsMean(records, g.primary)
			validated = append(validated, ValidatedHypothesis{Hypothesis: h, Mean: m, Lift: m - parentBatchMean})
		}
		if ctx.Err() != nil {
			reason = ReasonAborted
			break
		}
		if len(validated) == 0 {
			g.vista.Record(false)
			rounds = r
			if unitUndispatched {
				// Every probe was budget-truncated: no selection
				// evidence remains and later dispatches can only fail —
				// stop here instead of burning optimizer calls on
				// doomed rounds.
				reason = ReasonBudgetStopped
				break
			}
			g.emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": "没有可验证的假设"})
			continue
		}

		// ⑥ ε-greedy selection — the audit trail lands in the event.
		sel, mode := g.vista.Select(validated)
		g.emit(EventHypoValidated, map[string]any{
			"round": r, "selected_hypothesis": sel.Hypothesis.ID, "mode": mode,
			"lift": sel.Lift, "validated": len(validated),
		})

		// ⑦ operator ladder (deterministic): stagnation restart,
		// stale merge, else rewrite.
		var child core.Candidate
		var op string
		var parents []string
		switch {
		case g.vista.Stagnant() >= req.Params.StagnationLimit:
			g.emit(EventVistaRestart, map[string]any{
				"round": r, "stagnant": g.vista.Stagnant(), "limit": req.Params.StagnationLimit,
			})
			child, err = g.mutator.Fresh(ctx, req.Task)
			g.vista.Reset()
			op, parents = OpRestart, []string{parent.ID()}
		case g.vista.Stagnant() >= 1 && g.frontier.Size() >= 2:
			comp := g.frontier.Complement(parent, g.primary)
			child, err = g.mutator.Merge(ctx, req.Task, parent.Candidate, comp.Candidate, sel.Hypothesis, lessons)
			op, parents = OpMerge, []string{parent.ID(), comp.ID()}
		default:
			child, err = g.mutator.Rewrite(ctx, req.Task, parent.Candidate, sel.Hypothesis, lessons)
			op, parents = OpRewrite, []string{parent.ID()}
		}
		if err != nil {
			if errors.Is(err, errOptBudget) {
				reason = ReasonBudgetStopped
				break
			}
			g.vista.Record(false)
			g.emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": err.Error()})
			rounds = r
			continue
		}
		child.ID = fmt.Sprintf(childIDFormat, r)
		g.emit(EventMutateDone, map[string]any{"round": r, "operator": op, "candidate": child.ID, "parent": parent.ID()})

		// ⑧ full retained-set evaluation of the child.
		res, records, uerr := g.evaluateUnit(ctx, child, req.Samples, r)
		if uerr != nil {
			return Result{}, uerr
		}
		g.harvest(child.ID, records)
		if res.Undispatched > 0 {
			// The budget ran out mid-evaluation: the row's undispatched
			// cells would read as real zeros in dominance and Best
			// ordering, so a half-evaluated child must never join the
			// frontier (it could evict a fully evaluated member scoring
			// 0 on exactly those cells). Keep the lineage trail marked
			// incomplete and deliver the current best.
			partialRow := recordsRow(records, req.Samples, g.primary)
			g.emit(EventFrontierUpdated, map[string]any{
				"round": r, "candidate": child.ID, "admitted": false,
				"incomplete": true, "undispatched": res.Undispatched,
				"frontier_size": g.frontier.Size(),
			})
			if err := lin.Append(LineageRecord{
				ID: child.ID, Parents: parents, Operator: op,
				Hypotheses: []Hypothesis{sel.Hypothesis}, Round: r,
				Scores: partialRow, PrimaryMean: mean(partialRow),
				Admitted: false, Incomplete: true,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				return Result{}, err
			}
			g.vista.Record(false)
			rounds = r
			reason = ReasonBudgetStopped
			break
		}
		childRow := recordsRow(records, req.Samples, g.primary)
		childMeans := maps.Clone(res.MetricMeans)
		if childMeans == nil {
			childMeans = make(map[string]float64)
		}
		// Keep the primary mean consistent with the row (missing
		// cells count as 0) so Best ordering matches dominance input.
		childMeans[g.primary] = mean(childRow)

		// ⑨ frontier admission + progress bookkeeping.
		member := Member{Candidate: child, Scores: childRow, Means: childMeans, Round: r, Operator: op}
		admitted, evicted := g.frontier.Add(member)
		g.emit(EventFrontierUpdated, map[string]any{
			"round": r, "candidate": child.ID, "admitted": admitted,
			"evicted": evicted, "frontier_size": g.frontier.Size(),
		})
		progress := mean(childRow) > bestMean
		if progress {
			bestMean = mean(childRow)
		}
		g.vista.Record(progress)

		// ⑩ lineage + round close.
		if err := lin.Append(LineageRecord{
			ID: child.ID, Parents: parents, Operator: op,
			Hypotheses: []Hypothesis{sel.Hypothesis}, Round: r,
			Scores: childRow, PrimaryMean: mean(childRow), Admitted: admitted,
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return Result{}, err
		}
		rounds = r
		g.emit(EventRoundDone, map[string]any{
			"round": r, "candidate": child.ID, "admitted": admitted,
			"progress": progress, "primary_mean": mean(childRow),
		})

		// ⑪ budget stop: undispatched samples or an armed valve/soft
		// stop end the loop with the current best.
		if unitUndispatched || res.Undispatched > 0 || req.Budget.SoftStopped() || g.adv.ValveTripped() {
			g.adv.notifyValve()
			reason = ReasonBudgetStopped
			break
		}
	}

	_, usage := req.Budget.Snapshot()
	best, satisfied, _ := g.frontier.Best(g.primary, g.constraint)
	res := Result{
		Best:                best.Candidate,
		BestMeans:           maps.Clone(best.Means),
		ConstraintSatisfied: satisfied,
		Frontier:            g.frontier.Members(),
		Rounds:              rounds,
		Reason:              reason,
		Usage:               usage,
	}
	if err := WriteOutputs(req.RunDir, ReportInputs{
		Task: req.Task, Lineage: lin, Result: res, Constraint: g.constraint,
		OptBudgetTokens: g.adv.optBudgetTokens, OptValveTripped: g.adv.ValveFired(),
		BaselineGaps: baselineGaps,
	}, g.frontier); err != nil {
		return Result{}, err
	}
	return res, nil
}

// emit publishes one engine event on the request stream.
func (g *gepaRun) emit(typ string, detail map[string]any) {
	if g.req.OnEvent != nil {
		g.req.OnEvent(eval.Event{Type: typ, Time: time.Now(), RunID: g.req.RunID, Detail: detail})
	}
}

// harvest stores one unit's per-sample records for later reflection
// contexts.
func (g *gepaRun) harvest(candID string, records []SampleRecord) {
	m := make(map[string]SampleRecord, len(records))
	for _, rec := range records {
		m[rec.Sample.ID] = rec
	}
	g.records[candID] = m
}

// batchRecords returns the parent's records over the minibatch, in
// batch order. Samples without evidence surface as zero-score records
// so the reflection context still shows them.
func (g *gepaRun) batchRecords(parentID string, batch []core.Sample) []SampleRecord {
	recs := g.records[parentID]
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

// batchMean is the parent's primary mean over the batch samples
// (missing cells count as 0).
func (g *gepaRun) batchMean(parent Member, batch []core.Sample) float64 {
	recs := g.records[parent.ID()]
	sum := 0.0
	for _, s := range batch {
		if rec, ok := recs[s.ID]; ok {
			sum += rec.Scores[g.primary]
		}
	}
	return sum / float64(len(batch))
}

// evaluateUnit evaluates one candidate over samples through a fresh
// eval.Engine rooted at runs/<id>/evals/<seq>-<unit>/. The OnEvent
// wrapper drops the inner run_start/run_done (they would cut the SSE
// stream and replay short), forwards sample_start/sample_done and
// budget_stop stamped with Detail{candidate, round}, and harvests the
// per-sample records.
func (g *gepaRun) evaluateUnit(ctx context.Context, cand core.Candidate, samples []core.Sample, round int) (core.RunResult, []SampleRecord, error) {
	g.seq++
	unitDir := filepath.Join(g.req.RunDir, evalsDir, fmt.Sprintf("%02d-%s", g.seq, sanitizeID(cand.ID)))
	coll := newUnitCollector()
	engine := &eval.Engine{
		RunID:       g.req.RunID,
		RunDir:      unitDir,
		Model:       g.req.Model,
		MaxTokens:   g.req.MaxTokens,
		Workers:     max(g.req.Workers, 1),
		Metrics:     g.req.Task.Metrics,
		Budget:      g.req.Budget,
		Provider:    g.req.Provider,
		TaskName:    g.req.Task.Name,
		CandidateID: cand.ID,
		DatasetName: "synth",
		OnEvent: func(ev eval.Event) {
			coll.observe(ev)
			g.forward(ev, cand.ID, round)
		},
	}
	res, err := engine.Run(ctx, cand, samples)
	if err != nil {
		return res, nil, fmt.Errorf("evaluate unit %s: %w", cand.ID, err)
	}
	return res, coll.records(samples), nil
}

// forward relays whitelisted inner events with the candidate/round
// stamp; the inner run lifecycle stays internal.
func (g *gepaRun) forward(ev eval.Event, candID string, round int) {
	if g.req.OnEvent == nil {
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
	g.req.OnEvent(ev)
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
	rec := SampleRecord{Response: ev.Response, Scores: ev.Scores, Diagnosis: ev.Diagnosis}
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

// baselineRow builds the baseline score row and means from the request
// records; missing primary cells count as 0 and are counted as gaps.
func baselineRow(req Request, primary string) (row []float64, means map[string]float64, gaps int) {
	byID := make(map[string]SampleRecord, len(req.Baseline))
	for _, rec := range req.Baseline {
		byID[rec.Sample.ID] = rec
	}
	sums := make(map[string]float64, len(req.Task.Metrics))
	for _, m := range req.Task.Metrics {
		sums[m] = 0
	}
	row = make([]float64, len(req.Samples))
	for i, s := range req.Samples {
		rec, ok := byID[s.ID]
		if !ok {
			gaps++
			continue
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
	n := float64(len(req.Samples))
	means = make(map[string]float64, len(sums))
	for m, sum := range sums {
		means[m] = sum / n
	}
	return row, means, gaps
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

// pickMinibatch draws k samples at random; a set of k or fewer
// returns unchanged, in order.
func pickMinibatch(rng *rand.Rand, samples []core.Sample, k int) []core.Sample {
	if k >= len(samples) {
		return samples
	}
	perm := rng.Perm(len(samples))
	out := make([]core.Sample, k)
	for i := range k {
		out[i] = samples[perm[i]]
	}
	return out
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

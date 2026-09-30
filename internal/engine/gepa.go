package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Engine lifecycle events (SSE + events.jsonl). The engine never
// emits run_done — the terminal run_done belongs to the cmd layer;
// inner evaluation units' run_start/run_done stay internal too
// (dropped by Loop.forward) so live streams and replays are not cut
// short mid-optimization.
const (
	EventRoundStart      = "round_start"
	EventReflectDone     = "reflect_done"
	EventHypoValidated   = "hypotheses_validated"
	EventMutateDone      = "mutate_done"
	EventFrontierUpdated = "frontier_updated"
	EventVistaRestart    = "vista_restart"
	EventRoundDone       = "round_done"
	EventUsage           = "usage"
)

// Loop-wide tuning constants.
const (
	maxHypotheses = 3 // reflection pool size
	maxLessons    = 6 // ancestor lessons fed to reflection/mutation
	childIDFormat = "g%02d"
	evalsDir      = "evals"
	minibatchHint = "\n\n补充指导："
)

// Gepa is the GEPA reflective-evolution Optimizer: per round it
// reflects a parent's minibatch failures into hypotheses, validates
// them as temporary candidates, selects one ε-greedily (VISTA
// decoupling), mutates via the operator ladder (Rewrite → Merge →
// Fresh restart) and admits the child onto the Pareto frontier. The
// shared per-run plumbing (baseline seeding, unit evaluation,
// admission, artifacts) lives in Loop; Gepa owns only the search
// decisions. It is stateless and safe for concurrent use.
type Gepa struct{}

var _ Optimizer = (*Gepa)(nil)

// Optimize runs the GEPA loop for req and returns the terminal state
// with the frontier/report artifacts written under req.RunDir. The
// baseline row is always delivered, so Best is never empty.
func (g *Gepa) Optimize(ctx context.Context, req Request) (Result, error) {
	loop, err := NewLoop(req)
	if err != nil {
		return Result{}, err
	}
	rng := rand.New(rand.NewPCG(uint64(req.Params.Seed), uint64(req.Params.Seed)))
	r := &gepaRun{
		loop:      loop,
		rng:       rng,
		reflector: NewReflector(loop.Advisor()),
		mutator:   NewMutator(loop.Advisor()),
		vista:     NewVistaGuard(req.Params.Epsilon, req.Params.StagnationLimit, rng),
	}
	return r.run(ctx)
}

// gepaRun holds the GEPA-specific decisions of one Optimize
// invocation: the rng draws (parent pick, minibatch, ε-greedy
// selection) and the VISTA stagnation ladder. Everything shared across
// paradigms lives in the Loop.
type gepaRun struct {
	loop      *Loop
	rng       *rand.Rand
	reflector *Reflector
	mutator   *Mutator
	vista     *VistaGuard
}

// run executes the round loop (§1 of the design).
func (g *gepaRun) run(ctx context.Context) (Result, error) {
	req := g.loop.Request()

	reason := ReasonRoundsDone
	rounds := 0
	bestMean := mean(g.loop.baseline.Scores)

	for r := 1; r <= req.Params.MaxRounds; r++ {
		// ① gates: cancellation beats budget exhaustion.
		if ctx.Err() != nil {
			reason = ReasonAborted
			break
		}
		if g.loop.Stopped() {
			g.loop.NotifyValve()
			reason = ReasonBudgetStopped
			break
		}
		g.loop.Emit(EventRoundStart, map[string]any{"round": r})

		// ② parent: baseline on round 1, uniform frontier draw after.
		parent := g.loop.baseline
		if r > 1 {
			parent = g.loop.Frontier().UniformPick(g.rng)
		}
		// ③ minibatch: rng draw of K, all when fewer.
		batch := pickMinibatch(g.rng, req.Samples, req.Params.Minibatch)

		// ④ reflection (optimizer role).
		lessons := g.loop.Lineage().Lessons(parent.ID(), maxLessons)
		hyps, rerr := g.reflector.Reflect(ctx, req.Task, parent.Candidate, lessons, g.loop.BatchRecords(parent.ID(), batch), maxHypotheses)
		if rerr != nil {
			if errors.Is(rerr, ErrOptBudget) {
				reason = ReasonBudgetStopped
				break
			}
			// Failure resilience: stagnate and skip the round.
			g.vista.Record(false)
			g.loop.Emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": rerr.Error()})
			rounds = r
			continue
		}
		g.loop.Emit(EventReflectDone, map[string]any{"round": r, "hypotheses": len(hyps)})

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
				Prompt: parent.Candidate.Prompt + minibatchHint + CleanInputLiteral(h.Text),
			}
			res, records, uerr := g.loop.Evaluate(ctx, temp, batch, r)
			if uerr != nil {
				return Result{}, uerr
			}
			if res.Undispatched > 0 {
				// The budget ran dry mid-probe: the missing cells would
				// inflate the mean's denominator or read as fake zeros,
				// so the lift is not comparable with the parent's. Skip
				// the hypothesis; the round closes budget-stopped below.
				unitUndispatched = true
				continue
			}
			m := recordsMean(records, g.loop.Primary())
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
			g.loop.Emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": "没有可验证的假设"})
			continue
		}

		// ⑥ ε-greedy selection — the audit trail lands in the event.
		sel, mode := g.vista.Select(validated)
		g.loop.Emit(EventHypoValidated, map[string]any{
			"round": r, "selected_hypothesis": sel.Hypothesis.ID, "mode": mode,
			"lift": sel.Lift, "validated": len(validated),
		})

		// ⑦ operator ladder (deterministic): stagnation restart,
		// stale merge, else rewrite.
		var child core.Candidate
		var op string
		var parents []string
		var err error
		switch {
		case g.vista.Stagnant() >= req.Params.StagnationLimit:
			g.loop.Emit(EventVistaRestart, map[string]any{
				"round": r, "stagnant": g.vista.Stagnant(), "limit": req.Params.StagnationLimit,
			})
			child, err = g.mutator.Fresh(ctx, req.Task)
			g.vista.Reset()
			op, parents = OpRestart, []string{parent.ID()}
		case g.vista.Stagnant() >= 1 && g.loop.Frontier().Size() >= 2:
			comp := g.loop.Frontier().Complement(parent, g.loop.Primary())
			child, err = g.mutator.Merge(ctx, req.Task, parent.Candidate, comp.Candidate, sel.Hypothesis, lessons)
			op, parents = OpMerge, []string{parent.ID(), comp.ID()}
		default:
			child, err = g.mutator.Rewrite(ctx, req.Task, parent.Candidate, sel.Hypothesis, lessons)
			op, parents = OpRewrite, []string{parent.ID()}
		}
		if err != nil {
			if errors.Is(err, ErrOptBudget) {
				reason = ReasonBudgetStopped
				break
			}
			g.vista.Record(false)
			g.loop.Emit(EventRoundDone, map[string]any{"round": r, "skipped": true, "error": err.Error()})
			rounds = r
			continue
		}
		child.ID = fmt.Sprintf(childIDFormat, r)
		g.loop.Emit(EventMutateDone, map[string]any{"round": r, "operator": op, "candidate": child.ID, "parent": parent.ID()})

		// ⑧ full retained-set evaluation of the child.
		res, records, uerr := g.loop.Evaluate(ctx, child, req.Samples, r)
		if uerr != nil {
			return Result{}, uerr
		}
		if res.Undispatched > 0 {
			// The budget ran out mid-evaluation: the row's undispatched
			// cells would read as real zeros in dominance and Best
			// ordering, so a half-evaluated child must never join the
			// frontier (it could evict a fully evaluated member scoring
			// 0 on exactly those cells). Keep the lineage trail marked
			// incomplete and deliver the current best.
			if err := g.loop.MarkIncomplete(child, op, parents, []Hypothesis{sel.Hypothesis}, r, res.Undispatched, records); err != nil {
				return Result{}, err
			}
			g.vista.Record(false)
			rounds = r
			reason = ReasonBudgetStopped
			break
		}

		// ⑨ frontier admission + progress bookkeeping.
		admitted, aerr := g.loop.Admit(child, op, parents, []Hypothesis{sel.Hypothesis}, r, res, records)
		if aerr != nil {
			return Result{}, aerr
		}
		childRowMean := mean(recordsRow(records, req.Samples, g.loop.Primary()))
		progress := childRowMean > bestMean
		if progress {
			bestMean = childRowMean
		}
		g.vista.Record(progress)

		// ⑩ round close.
		rounds = r
		g.loop.Emit(EventRoundDone, map[string]any{
			"round": r, "candidate": child.ID, "admitted": admitted,
			"progress": progress, "primary_mean": childRowMean,
		})

		// ⑪ budget stop: undispatched samples or an armed valve/soft
		// stop end the loop with the current best.
		if unitUndispatched || res.Undispatched > 0 || g.loop.Stopped() {
			g.loop.NotifyValve()
			reason = ReasonBudgetStopped
			break
		}
	}

	return g.loop.Finish(reason, rounds)
}

// batchMean is the parent's primary mean over the batch samples
// (missing cells count as 0).
func (g *gepaRun) batchMean(parent Member, batch []core.Sample) float64 {
	recs := g.loop.records[parent.ID()]
	sum := 0.0
	for _, s := range batch {
		if rec, ok := recs[s.ID]; ok {
			sum += rec.Scores[g.loop.Primary()]
		}
	}
	return sum / float64(len(batch))
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

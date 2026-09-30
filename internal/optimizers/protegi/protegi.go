// Package protegi implements the ProTeGi-style text-gradient
// optimizer (Yang et al., 2024, "Automatic Prompt Optimization with
// Gradient Descent and Beam Search"). Per round a UCB1 bandit over
// retained-set folds picks the minibatch to criticize, one
// optimizer-role call turns the fold's failure evidence into a
// natural-language "textual gradient" (criticism + fix direction), a
// second rewrites the current best prompt along that gradient, and
// the child is evaluated over the full retained set and admitted onto
// the Pareto frontier through the shared engine.Loop. Mechanism-
// equivalent to the paper with the documented simplifications: beam
// width 1 (the frontier keeps the non-dominated set instead of a
// top-B list), UCB1 as the bandit rule (the paper leaves it
// unspecified) and folds as the batch arms.
package protegi

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// OpGradient is the lineage/frontier operator of every ProTeGi child.
const OpGradient = "gradient"

// EventGradientDone reports the textual-gradient stage of one round.
// It is a paradigm-specific event: Detail carries paradigm and stage
// per the docs/plugins.md event contract, which the live dashboard's
// default branch renders as "[protegi] gradient_done：gradient".
const EventGradientDone = "gradient_done"

// maxFoldArms caps the fold (bandit arm) count: every fold keeps at
// least two samples whenever the retained set affords it, and a
// single-sample set degenerates to one arm.
const maxFoldArms = 4

// ProTeGi is the text-gradient Optimizer. It is stateless and safe
// for concurrent use; per-run state lives inside Optimize.
type ProTeGi struct{}

var _ engine.Optimizer = (*ProTeGi)(nil)

// New returns the instance the registry factory hands out.
func New() *ProTeGi { return &ProTeGi{} }

// foldArms derives the fold count from the retained-set size.
func foldArms(n int) int { return min(maxFoldArms, max(1, n/2)) }

// Optimize runs the text-gradient loop for req and returns the
// terminal state with the frontier/report artifacts written under
// req.RunDir. The baseline row is always delivered, so Best is never
// empty.
func (p *ProTeGi) Optimize(ctx context.Context, req engine.Request) (engine.Result, error) {
	loop, err := engine.NewLoop(req)
	if err != nil {
		return engine.Result{}, err
	}
	rng := rand.New(rand.NewPCG(uint64(req.Params.Seed), uint64(req.Params.Seed)))
	folds := makeFolds(req.Samples, foldArms(len(req.Samples)), rng)
	return (&protegiRun{loop: loop, folds: folds, bandit: newBandit(len(folds))}).run(ctx)
}

// protegiRun holds the ProTeGi-specific decisions of one Optimize
// invocation: the fold split and its bandit. Everything shared across
// paradigms lives in the Loop.
type protegiRun struct {
	loop   *engine.Loop
	folds  [][]core.Sample
	bandit *bandit
}

// run executes the gradient-descent round loop.
func (p *protegiRun) run(ctx context.Context) (engine.Result, error) {
	req := p.loop.Request()

	reason := engine.ReasonRoundsDone
	rounds := 0

	for r := 1; r <= req.Params.MaxRounds; r++ {
		// ① gates: cancellation beats budget exhaustion.
		if ctx.Err() != nil {
			reason = engine.ReasonAborted
			break
		}
		if p.loop.Stopped() {
			p.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
		p.loop.Emit(engine.EventRoundStart, map[string]any{"round": r, "paradigm": "protegi"})

		// ② arm = one retained-set fold; the beam-1 parent is the
		// frontier's current best.
		arm := p.bandit.Select()
		fold := p.folds[arm]
		parent, _, _ := p.loop.Frontier().Best(p.loop.Primary(), p.loop.Constraint())

		// ③ failure evidence: the fold's samples the parent still
		// fails — the error minibatch ProTeGi criticizes.
		failures := foldFailures(p.loop, parent.ID(), fold)
		if len(failures) == 0 {
			// The parent already aces this fold: no error to criticize.
			// Charge the arm zero reward (UCB1 then leans toward folds
			// that still produce lift) and close the round without
			// burning optimizer calls.
			p.bandit.Update(arm, 0)
			rounds = r
			p.loop.Emit(engine.EventRoundDone, map[string]any{
				"round": r, "skipped": true, "arm": arm,
				"error": "该折样本全部通过，无失败证据", "paradigm": "protegi",
			})
			continue
		}

		// ④ textual gradient (optimizer role).
		grad, gerr := gradient(ctx, p.loop.Advisor(), req.Task, parent.Candidate, failures)
		if gerr != nil {
			if errors.Is(gerr, engine.ErrOptBudget) {
				reason = engine.ReasonBudgetStopped
				break
			}
			// Failure resilience: skip the round, keep the best.
			rounds = r
			p.loop.Emit(engine.EventRoundDone, map[string]any{
				"round": r, "skipped": true, "error": gerr.Error(), "paradigm": "protegi",
			})
			continue
		}
		p.loop.Emit(EventGradientDone, map[string]any{
			"round": r, "arm": arm, "failures": len(failures), "chars": len([]rune(grad)),
			"paradigm": "protegi", "stage": "gradient",
		})

		// ⑤ apply the gradient to the parent.
		child, aerr := apply(ctx, p.loop.Advisor(), req.Task, parent.Candidate, grad)
		if aerr != nil {
			if errors.Is(aerr, engine.ErrOptBudget) {
				reason = engine.ReasonBudgetStopped
				break
			}
			rounds = r
			p.loop.Emit(engine.EventRoundDone, map[string]any{
				"round": r, "skipped": true, "error": aerr.Error(), "paradigm": "protegi",
			})
			continue
		}
		child.ID = fmt.Sprintf("p%02d", r)
		p.loop.Emit(engine.EventMutateDone, map[string]any{
			"round": r, "operator": OpGradient, "candidate": child.ID, "parent": parent.ID(),
			"paradigm": "protegi", "stage": "apply",
		})

		// ⑥ full retained-set evaluation of the child.
		res, records, uerr := p.loop.Evaluate(ctx, child, req.Samples, r)
		if uerr != nil {
			return engine.Result{}, uerr
		}
		if res.Undispatched > 0 {
			// The budget ran out mid-evaluation: the partial row's
			// undispatched cells would read as real zeros in dominance
			// and Best ordering, so the child never joins the frontier —
			// the lineage trail keeps the partial evidence flagged
			// incomplete and the current best is delivered.
			if err := p.loop.MarkIncomplete(child, OpGradient, []string{parent.ID()}, nil, r, res.Undispatched, records); err != nil {
				return engine.Result{}, err
			}
			rounds = r
			reason = engine.ReasonBudgetStopped
			break
		}

		// ⑦ frontier admission + bandit reward (the fold lift).
		admitted, err := p.loop.Admit(child, OpGradient, []string{parent.ID()}, nil, r, res, records)
		if err != nil {
			return engine.Result{}, err
		}
		lift := batchMean(p.loop, child.ID, fold) - batchMean(p.loop, parent.ID(), fold)
		p.bandit.Update(arm, lift)

		// ⑧ round close.
		rounds = r
		p.loop.Emit(engine.EventRoundDone, map[string]any{
			"round": r, "candidate": child.ID, "admitted": admitted,
			"arm": arm, "lift": lift, "paradigm": "protegi",
		})

		// ⑨ budget stop: an armed valve or executor soft stop ends the
		// loop with the current best.
		if p.loop.Stopped() {
			p.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
	}

	return p.loop.Finish(reason, rounds)
}

// foldFailures filters the fold down to the samples the candidate
// still fails (primary score below 1). BatchRecords zero-fills
// missing cells, so unevaluated samples surface as failures rather
// than silently vanishing.
func foldFailures(loop *engine.Loop, candID string, fold []core.Sample) []engine.SampleRecord {
	recs := loop.BatchRecords(candID, fold)
	out := recs[:0]
	for _, rec := range recs {
		if rec.Scores[loop.Primary()] < 1 {
			out = append(out, rec)
		}
	}
	return out
}

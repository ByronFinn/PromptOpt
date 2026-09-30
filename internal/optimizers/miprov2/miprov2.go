// Package miprov2 implements a MIPROv2-style optimizer: a joint search
// over instruction variants and few-shot demo subsets. One proposal
// call yields several instruction variants; every round then samples a
// handful of (instruction, demo-subset) pairs, scores each pair on a
// minibatch that excludes the pair's own demos (leak mitigation), and
// promotes the winner to a full retained-set evaluation — successive
// halving collapsed into one cheap-rung → full-rung promotion, the
// documented mechanism-equivalent stand-in for MIPRO's TPE surrogate
// (docs/plugins.md).
package miprov2

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Paradigm identity: operator and event names. propose_done/joint_done
// are paradigm-specific, so they carry Detail{paradigm, stage} for the
// live dashboard's default branch (docs/plugins.md event contract).
const (
	// paradigmName tags this paradigm's events; the registry name is
	// "miprov2" (internal/optimizers/builtin).
	paradigmName = "miprov2"
	// OpJoint is the lineage operator of an admitted joint pair.
	OpJoint = "joint"
	// EventProposeDone fires once after the instruction proposal.
	EventProposeDone = "propose_done"
	// EventJointDone fires after each round's winner admission.
	EventJointDone = "joint_done"
)

// Stage names carried by paradigm events (Detail.stage) and opt-call
// trace files.
const (
	stagePropose = "propose"
	stageJoint   = "joint"
	stageRound   = "round"
)

// Search-shape constants (sub-package knobs; the flag surface stays
// additive through engine.Request.Opts if any needs exposing later).
const (
	numInstructions = 4 // instruction variants per proposal (I)
	combosPerRound  = 3 // joint pairs scored per round (S = min(3, I))
	childIDFormat   = "m%02d"
)

// Optimizer is the MIPROv2-style joint-search engine.Optimizer. It is
// stateless and safe for concurrent use; per-run state lives inside
// Optimize.
type Optimizer struct{}

var _ engine.Optimizer = (*Optimizer)(nil)

// New returns one optimizer instance.
func New() *Optimizer { return &Optimizer{} }

// Optimize runs the joint search for req: propose instruction variants
// once, then per round sample S (instruction, demo-subset) pairs, score
// them on demo-free minibatches and admit the round winner after a
// full retained-set evaluation. The shared per-run plumbing (baseline
// seeding, unit evaluation, admission, artifacts) lives in
// engine.Loop; the baseline row is always delivered, so Best is never
// empty.
func (o *Optimizer) Optimize(ctx context.Context, req engine.Request) (engine.Result, error) {
	loop, err := engine.NewLoop(req)
	if err != nil {
		return engine.Result{}, err
	}
	rng := rand.New(rand.NewPCG(uint64(req.Params.Seed), uint64(req.Params.Seed)))
	r := &run{loop: loop, rng: rng}
	return r.run(ctx)
}

// run holds the joint search's per-run decisions: the seeded rng stream
// behind the instruction/demo draws. Everything shared across
// paradigms lives in the Loop.
type run struct {
	loop *engine.Loop
	rng  *rand.Rand
}

func (r *run) run(ctx context.Context) (engine.Result, error) {
	req := r.loop.Request()

	// The proposal precedes the round loop, so cancellation needs its
	// own gate here — no doomed provider call may leave the opt-calls
	// trail on an aborted run.
	if ctx.Err() != nil {
		return r.loop.Finish(engine.ReasonAborted, 0)
	}

	// ① instruction proposal (optimizer role). A budget stop here still
	// delivers the baseline; any other failure degrades to the baseline
	// prompt as the single instruction so demo search can proceed.
	instructions, perr := r.propose(ctx, req)
	if perr != nil {
		if errors.Is(perr, engine.ErrOptBudget) {
			r.loop.NotifyValve()
			return r.loop.Finish(engine.ReasonBudgetStopped, 0)
		}
		instructions = []string{req.Initial.Prompt}
	}
	detail := map[string]any{
		"paradigm": paradigmName, "stage": stagePropose,
		"instructions": len(instructions), "fallback": perr != nil,
	}
	if perr != nil {
		detail["error"] = perr.Error()
	}
	r.loop.Emit(EventProposeDone, detail)

	pool := demoPool(req.Samples, demoPoolCap)
	reason := engine.ReasonRoundsDone
	rounds := 0

	for round := 1; round <= req.Params.MaxRounds; round++ {
		// ② gates: cancellation beats budget exhaustion.
		if ctx.Err() != nil {
			reason = engine.ReasonAborted
			break
		}
		if r.loop.Stopped() {
			r.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
		r.loop.Emit(engine.EventRoundStart, map[string]any{
			"round": round, "paradigm": paradigmName, "stage": stageRound,
		})

		// ③ cheap rung: score S joint pairs on demo-free minibatches.
		scored, truncated, serr := r.scoreCombos(ctx, instructions, pool, round)
		if serr != nil {
			return engine.Result{}, serr
		}
		if ctx.Err() != nil {
			reason = engine.ReasonAborted
			break
		}
		if len(scored) == 0 {
			// Every pair was budget-truncated: no selection evidence
			// remains and later dispatches can only fail — stop instead
			// of burning optimizer calls on doomed rounds.
			rounds = round
			reason = engine.ReasonBudgetStopped
			break
		}

		// ④ full rung: successive halving's promotion — the winner is
		// evaluated over the whole retained set and admitted.
		winner := pickWinner(scored)
		child := core.Candidate{
			ID:          fmt.Sprintf(childIDFormat, round),
			Name:        fmt.Sprintf("joint-%02d", round),
			Description: fmt.Sprintf("指令变体#%d 与 %d 条 few-shot 示例的联合", winner.pair.instr+1, len(winner.pair.demos)),
			Prompt:      composePrompt(winner.pair.instruction, winner.pair.demos),
		}
		hyps := []engine.Hypothesis{{
			ID:    fmt.Sprintf("j%d-%d", round, winner.pair.instr),
			Text: fmt.Sprintf("指令变体#%d 配 %d 条示例的组合在去泄漏 minibatch 上均分 %.4f，胜过其余 %d 个组合",
				winner.pair.instr+1, len(winner.pair.demos), winner.mean, len(scored)-1),
			SampleIDs:  sampleIDs(winner.pair.demos),
			Confidence: clamp01(winner.mean),
		}}
		res, records, uerr := r.loop.Evaluate(ctx, child, req.Samples, round)
		if uerr != nil {
			return engine.Result{}, uerr
		}
		rounds = round
		if res.Undispatched > 0 {
			// The budget ran out mid-evaluation: the row's undispatched
			// cells would read as real zeros in dominance and Best
			// ordering, so the half-evaluated child never joins the
			// frontier — the lineage trail keeps the partial evidence
			// flagged Incomplete instead (Loop.MarkIncomplete).
			if err := r.loop.MarkIncomplete(child, OpJoint, []string{req.Initial.ID}, hyps, round, res.Undispatched, records); err != nil {
				return engine.Result{}, err
			}
			reason = engine.ReasonBudgetStopped
			break
		}
		admitted, aerr := r.loop.Admit(child, OpJoint, []string{req.Initial.ID}, hyps, round, res, records)
		if aerr != nil {
			return engine.Result{}, aerr
		}
		r.loop.Emit(EventJointDone, map[string]any{
			"round": round, "candidate": child.ID, "admitted": admitted,
			"combos": len(scored) + truncated, "demos": len(winner.pair.demos),
			"minibatch_mean": winner.mean,
			"paradigm":       paradigmName, "stage": stageJoint,
		})
		r.loop.Emit(engine.EventRoundDone, map[string]any{
			"round": round, "candidate": child.ID, "admitted": admitted,
			"primary_mean": recordsMean(records, r.loop.Primary()),
			"paradigm":     paradigmName, "stage": stageRound,
		})

		// ⑤ budget stop: truncated pairs or an armed valve/soft stop end
		// the loop with the current best.
		if truncated > 0 || r.loop.Stopped() {
			r.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
	}

	return r.loop.Finish(reason, rounds)
}

// combo is one joint-search candidate: an instruction variant (by pool
// index) paired with a demo subset.
type combo struct {
	instr       int
	instruction string
	demos       []core.Sample
}

// comboScore couples a pair with its cheap-rung evidence: the primary
// mean over its leak-free minibatch.
type comboScore struct {
	pair combo
	mean float64
}

// scoreCombos evaluates the round's joint pairs on minibatches that
// exclude each pair's own demos. Budget-truncated pairs are skipped —
// their means are not comparable with fully dispatched ones — and
// counted in the truncated return.
func (r *run) scoreCombos(ctx context.Context, instructions []string, pool []core.Sample, round int) ([]comboScore, int, error) {
	req := r.loop.Request()
	var scored []comboScore
	truncated := 0
	for i, pair := range drawCombos(r.rng, instructions, pool) {
		if ctx.Err() != nil {
			break
		}
		probe := core.Candidate{
			ID:     fmt.Sprintf("%s-combo%d", fmt.Sprintf(childIDFormat, round), i),
			Prompt: composePrompt(pair.instruction, pair.demos),
		}
		batch := minibatchFrom(r.rng, req.Samples, pair.demos, req.Params.Minibatch)
		res, records, err := r.loop.Evaluate(ctx, probe, batch, round)
		if err != nil {
			return nil, 0, err
		}
		if res.Undispatched > 0 {
			truncated++
			continue
		}
		scored = append(scored, comboScore{pair: pair, mean: recordsMean(records, r.loop.Primary())})
	}
	return scored, truncated, nil
}

// drawCombos samples the round's joint pairs: S distinct instructions
// (shuffled indices) each paired with a fresh demo draw.
func drawCombos(rng *rand.Rand, instructions []string, pool []core.Sample) []combo {
	n := min(combosPerRound, len(instructions))
	pairs := make([]combo, 0, n)
	for _, idx := range rng.Perm(len(instructions))[:n] {
		pairs = append(pairs, combo{
			instr:       idx,
			instruction: instructions[idx],
			demos:       drawDemos(rng, pool, demosPerCombo),
		})
	}
	return pairs
}

// pickWinner returns the top cheap-rung score; ties break toward the
// earlier draw, keeping the promotion deterministic.
func pickWinner(scored []comboScore) comboScore {
	best := scored[0]
	for _, sc := range scored[1:] {
		if sc.mean > best.mean {
			best = sc
		}
	}
	return best
}

// propose runs the single instruction-proposal call (plus at most one
// repair) and normalizes the pool.
func (r *run) propose(ctx context.Context, req engine.Request) ([]string, error) {
	raw, err := r.loop.Advisor().Call(ctx, stagePropose,
		buildProposePrompt(req.Task, req.Initial.Prompt, demoPool(req.Samples, summarySamples), numInstructions))
	if err != nil {
		return nil, err
	}
	payload, err := engine.Defend(ctx, r.loop.Advisor(), stagePropose, MarkerProposeFix, raw, checkInstructions)
	if err != nil {
		return nil, err
	}
	return normalizeInstructions(payload.Instructions, numInstructions), nil
}

// recordsMean is the primary mean over harvested records, missing
// cells counting as 0 — consistent with the loop's row projection.
func recordsMean(records []engine.SampleRecord, primary string) float64 {
	if len(records) == 0 {
		return 0
	}
	sum := 0.0
	for _, rec := range records {
		sum += rec.Scores[primary]
	}
	return sum / float64(len(records))
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

func sampleIDs(samples []core.Sample) []string {
	out := make([]string, len(samples))
	for i, s := range samples {
		out[i] = s.ID
	}
	return out
}

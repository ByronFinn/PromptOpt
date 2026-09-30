// Package evoprompt implements the EvoPrompt paradigm: the LLM itself
// is the evolutionary operator. A population of prompts is seeded from
// the baseline (MarkerInitPop variants), then every generation selects
// parents by fitness tournament (primary-metric mean over the retained
// set) and breeds children through LLM crossover and mutation — or,
// under the differential-evolution variant, through LLM differential
// mutation (parent moving along a−b) — evaluates each child over the
// full retained set and truncates the population back by fitness
// (elitism). The shared per-run plumbing (baseline seeding, unit
// evaluation, Pareto frontier admission, artifacts) lives in
// engine.Loop; this package owns only the evolutionary decisions.
//
// Mechanism deviations from the paper (docs/plugins.md): population
// size, children per generation and the tournament size are fixed
// subpackage constants; --evo-variant (ga|de) is the paradigm's only
// flag.
package evoprompt

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Paradigm stamps every evoprompt event Detail; the live dashboard's
// default branch renders paradigm-specific events as
// "[paradigm] type：stage".
const Paradigm = "evoprompt"

// EventPopulationUpdated reports one population snapshot — after
// seeding and after every generation's truncation.
const EventPopulationUpdated = "population_updated"

// Variant names (Opts key evoprompt.variant) selecting the breeding
// scheme: genetic algorithm (crossover + mutation) or differential
// evolution (differential mutation).
const (
	VariantGA = "ga"
	VariantDE = "de"

	// OptVariant is the Opts key carrying the variant; unset means ga.
	OptVariant = "evoprompt.variant"
)

// Loop tuning constants (fixed by design; see the package doc).
const (
	populationSize        = 6
	childrenPerGeneration = 2
	childIDFormat         = "e%02d"
)

// EvoPrompt is the EvoPrompt Optimizer. It is stateless and safe for
// concurrent use; per-run state lives inside Optimize.
type EvoPrompt struct{}

var _ engine.Optimizer = (*EvoPrompt)(nil)

// New returns one EvoPrompt optimizer.
func New() *EvoPrompt { return &EvoPrompt{} }

// Optimize runs the evolutionary loop for req and returns the terminal
// state with the frontier/report artifacts written under req.RunDir.
// The baseline row is always delivered, so Best is never empty.
func (o *EvoPrompt) Optimize(ctx context.Context, req engine.Request) (engine.Result, error) {
	variant := cmp.Or(req.Opt(OptVariant), VariantGA)
	if variant != VariantGA && variant != VariantDE {
		return engine.Result{}, fmt.Errorf("evoprompt: unknown variant %q (want %s or %s)",
			variant, VariantGA, VariantDE)
	}
	loop, err := engine.NewLoop(req)
	if err != nil {
		return engine.Result{}, err
	}
	r := &evoRun{
		loop:    loop,
		rng:     rand.New(rand.NewPCG(uint64(req.Params.Seed), uint64(req.Params.Seed))),
		variant: variant,
	}
	return r.run(ctx)
}

// evoRun carries one Optimize invocation's evolutionary state: the rng
// stream behind every tournament draw, the chosen variant and the
// living population. Everything shared across paradigms lives in the
// engine.Loop.
type evoRun struct {
	loop    *engine.Loop
	rng     *rand.Rand
	variant string
	pop     []individual
	seq     int // LLM-born candidate counter; seed variants and children share it
}

// individual is one population member: the candidate and its latest
// full-set primary mean — the fitness the tournament selects on. The
// baseline row's fitness comes from the request's baseline records.
type individual struct {
	cand    core.Candidate
	fitness float64
}

// run executes the evolutionary loop: population seeding, then
// generations of breeding, full-set evaluation and truncation.
func (r *evoRun) run(ctx context.Context) (engine.Result, error) {
	req := r.loop.Request()
	reason := engine.ReasonRoundsDone
	rounds := 0

	// ① seed the population (round 0): the baseline row plus LLM-born
	// variants, each quality-gated (ProduceCandidate) and fully
	// evaluated over the retained set.
	r.pop = []individual{{cand: req.Initial, fitness: primaryMean(req.Baseline, req.Samples, r.loop.Primary())}}
	stop, err := r.seedPopulation(ctx)
	if err != nil {
		return engine.Result{}, err
	}
	if stop != "" {
		return r.loop.Finish(stop, rounds)
	}
	r.pop = truncate(r.pop, populationSize)
	r.emitPopulation(0, "init")

	// ② generations: tournament parents → LLM offspring → full-set
	// evaluation → fitness truncation (elitism).
gen:
	for g := 1; g <= req.Params.MaxRounds; g++ {
		// gates: cancellation beats budget exhaustion.
		if ctx.Err() != nil {
			reason = engine.ReasonAborted
			break
		}
		if r.loop.Stopped() {
			r.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
		rounds = g
		r.loop.Emit(engine.EventRoundStart, map[string]any{
			"round": g, "paradigm": Paradigm, "stage": "generation", "variant": r.variant,
		})

		// ③ breed the generation's children: one production failure
		// skips that child; a budget stop ends the run with the
		// current best.
		born, admitted, failed := 0, 0, 0
		for _, b := range r.planBirths() {
			child, perr := r.produceBirth(ctx, b)
			if perr != nil {
				if errors.Is(perr, engine.ErrOptBudget) {
					reason = engine.ReasonBudgetStopped
					break gen
				}
				failed++
				continue
			}
			child.ID = r.nextID()
			stop, ok, rerr := r.raiseChild(ctx, child, b.op, r.parentIDs(b), g)
			if rerr != nil {
				return engine.Result{}, rerr
			}
			if stop != "" {
				reason = stop
				break gen
			}
			born++
			if ok {
				admitted++
			}
		}

		// ④ truncation (elitism) + bookkeeping.
		r.pop = truncate(r.pop, populationSize)
		r.emitPopulation(g, "generation")
		r.loop.Emit(engine.EventRoundDone, map[string]any{
			"round": g, "paradigm": Paradigm, "stage": "generation",
			"born": born, "admitted": admitted, "failed": failed,
			"population": len(r.pop), "best": r.pop[0].cand.ID, "fitness": r.pop[0].fitness,
		})

		// ⑤ budget stop: an armed valve or soft stop ends the loop
		// with the current best.
		if r.loop.Stopped() {
			r.loop.NotifyValve()
			reason = engine.ReasonBudgetStopped
			break
		}
	}

	return r.loop.Finish(reason, rounds)
}

// seedPopulation breeds the populationSize-1 LLM-born seed variants
// (round 0). A production failure skips one variant — the population
// may seed smaller, down to the baseline alone (later generations then
// degenerate to single-parent mutation); a budget stop ends seeding
// with its reason. It returns "" while the run continues.
func (r *evoRun) seedPopulation(ctx context.Context) (string, error) {
	req := r.loop.Request()
	for range populationSize - 1 {
		if ctx.Err() != nil {
			return engine.ReasonAborted, nil
		}
		if r.loop.Stopped() {
			r.loop.NotifyValve()
			return engine.ReasonBudgetStopped, nil
		}
		cand, err := engine.ProduceCandidate(ctx, r.loop.Advisor(), "init",
			buildInitPopPrompt(req.Task, req.Initial, len(r.pop), populationSize-1))
		if err != nil {
			if errors.Is(err, engine.ErrOptBudget) {
				return engine.ReasonBudgetStopped, nil
			}
			continue
		}
		cand.ID = r.nextID()
		stop, _, rerr := r.raiseChild(ctx, cand, OpInit, []string{req.Initial.ID}, 0)
		if rerr != nil {
			return "", rerr
		}
		if stop != "" {
			return stop, nil
		}
	}
	return "", nil
}

// birth plans one offspring ahead of its LLM call: the operator and
// the population indices of its parents (primary parent first).
type birth struct {
	op     string
	parent int
	mates  []int
}

// planBirths lays out the generation's children:
//
//   - ga: a crossover of two distinct tournament winners plus one
//     tournament parent's mutation;
//   - de: two differential births (parent, a, b) with a drawn away
//     from the parent and b away from a — b may equal the parent,
//     which degenerates the direction to "toward a" and keeps the
//     operator usable on a two-member population;
//   - a single-member population degenerates to single-parent
//     mutation under both variants.
func (r *evoRun) planBirths() []birth {
	if len(r.pop) == 1 {
		return []birth{{op: OpMutation}, {op: OpMutation}}
	}
	if r.variant == VariantDE {
		births := make([]birth, 0, childrenPerGeneration)
		for range childrenPerGeneration {
			parent := r.tournament(-1)
			a := r.tournament(parent)
			births = append(births, birth{op: OpDE, parent: parent, mates: []int{a, r.tournament(a)}})
		}
		return births
	}
	p1 := r.tournament(-1)
	return []birth{
		{op: OpCrossover, parent: p1, mates: []int{r.tournament(p1)}},
		{op: OpMutation, parent: r.tournament(-1)},
	}
}

// parentIDs lists the birth's lineage parent ids (primary parent
// first); duplicate ids — a DE mate redrawing the parent — collapse.
func (r *evoRun) parentIDs(b birth) []string {
	ids := []string{r.pop[b.parent].cand.ID}
	for _, m := range b.mates {
		if id := r.pop[m].cand.ID; !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// produceBirth runs the birth's LLM operator and returns the raw child
// (its id is still unset; the caller stamps the run sequence).
func (r *evoRun) produceBirth(ctx context.Context, b birth) (core.Candidate, error) {
	req := r.loop.Request()
	parent := r.pop[b.parent].cand
	switch b.op {
	case OpCrossover:
		return engine.ProduceCandidate(ctx, r.loop.Advisor(), "crossover",
			buildCrossoverPrompt(req.Task, parent, r.pop[b.mates[0]].cand))
	case OpDE:
		return engine.ProduceCandidate(ctx, r.loop.Advisor(), "de",
			buildDEPrompt(req.Task, parent, r.pop[b.mates[0]].cand, r.pop[b.mates[1]].cand))
	default:
		return engine.ProduceCandidate(ctx, r.loop.Advisor(), "mutation",
			buildMutationPrompt(req.Task, parent))
	}
}

// raiseChild evaluates one child over the full retained set, admits it
// onto the frontier and registers it in the population. It returns the
// engine stop reason when the run must not continue ("" = continue)
// alongside whether the frontier admitted the child.
func (r *evoRun) raiseChild(ctx context.Context, child core.Candidate, op string, parents []string, round int) (string, bool, error) {
	req := r.loop.Request()
	res, records, err := r.loop.Evaluate(ctx, child, req.Samples, round)
	if err != nil {
		return "", false, err
	}
	if res.Undispatched > 0 {
		// The budget ran dry mid-evaluation: the row's undispatched
		// cells would read as real zeros in dominance, so the child
		// joins the lineage flagged incomplete and never joins the
		// frontier (Loop.MarkIncomplete).
		if err := r.loop.MarkIncomplete(child, op, parents, nil, round, res.Undispatched, records); err != nil {
			return "", false, err
		}
		return engine.ReasonBudgetStopped, false, nil
	}
	admitted, err := r.loop.Admit(child, op, parents, nil, round, res, records)
	if err != nil {
		return "", false, err
	}
	r.pop = append(r.pop, individual{cand: child, fitness: primaryMean(records, req.Samples, r.loop.Primary())})
	return "", admitted, nil
}

// emitPopulation publishes one population snapshot (after seeding and
// after every truncation); the population stays sorted by fitness, so
// the head is the current elite.
func (r *evoRun) emitPopulation(round int, stage string) {
	ids := make([]string, len(r.pop))
	for i, ind := range r.pop {
		ids[i] = ind.cand.ID
	}
	r.loop.Emit(EventPopulationUpdated, map[string]any{
		"round": round, "paradigm": Paradigm, "stage": stage,
		"size": len(r.pop), "members": ids,
		"best": r.pop[0].cand.ID, "fitness": r.pop[0].fitness,
	})
}

// nextID stamps the next LLM-born candidate id (e01, e02, …); seed
// variants and generation children share the one sequence.
func (r *evoRun) nextID() string {
	r.seq++
	return fmt.Sprintf(childIDFormat, r.seq)
}

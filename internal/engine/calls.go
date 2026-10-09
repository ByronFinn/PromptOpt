package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// ErrOptBudget is the sentinel for a tripped optimizer-side token
// valve; a paradigm's main loop converts it to reason=budget_stopped.
var ErrOptBudget = errors.New("optimizer token budget exhausted")

// maxOptAttempts bounds the empty-content escalation ladder: a
// reasoning model can burn the whole completion cap on reasoning, so
// the cap doubles per attempt (floor → 2× → 4×) before failing.
const maxOptAttempts = 3

// Advisor performs optimizer-side LLM calls (reflection, mutation,
// repairs): every attempt is traced under RunDir/opt-calls/NNN-<stage>
// .json, successful usage is metered into the shared budget under the
// optimizer role (which never arms the executor soft stop), and every
// dial is gated by the optimizer token valve (OptBudgetTokens,
// cumulative optimizer-role usage, synthesis included; 0 = unlimited).
// The escalation ladder mirrors harness.Synthesizer
// (synthesize.go:147-217). V5 makes it the shared dial tone of every
// paradigm: construct one through NewLoop (or NewAdvisor) and pass it
// to Reflector/Mutator/Defend/ProduceCandidate.
type Advisor struct {
	provider        provider.Provider
	model           string
	optMaxTokens    int
	extraBody       map[string]any
	dir             string
	budget          *eval.Budget
	optBudgetTokens int64
	runID           string
	onEvent         func(eval.Event)

	floor      int // completion cap learned from a successful escalation
	seq        int
	valveFired bool
}

// NewAdvisor builds the optimizer-side dialer for one run request.
func NewAdvisor(req Request) *Advisor {
	return &Advisor{
		provider:        req.Provider,
		model:           req.Model,
		optMaxTokens:    req.OptMaxTokens,
		extraBody:       req.ExtraBody,
		dir:             filepath.Join(req.RunDir, "opt-calls"),
		budget:          req.Budget,
		optBudgetTokens: req.OptBudgetTokens,
		runID:           req.RunID,
		onEvent:         req.OnEvent,
	}
}

// ValveTripped reports whether the optimizer-side valve is armed.
func (a *Advisor) ValveTripped() bool {
	if a.optBudgetTokens <= 0 {
		return false
	}
	_, usage := a.budget.Snapshot()
	return usage[core.RoleOptimizer].Total() >= a.optBudgetTokens
}

// ValveFired reports whether the valve event was ever emitted.
func (a *Advisor) ValveFired() bool { return a.valveFired }

// notifyValve emits the optimizer-role budget_stop event once.
func (a *Advisor) notifyValve() {
	if a.valveFired || a.onEvent == nil {
		a.valveFired = true
		return
	}
	a.valveFired = true
	a.onEvent(eval.Event{
		Type: eval.EventBudgetStop, Time: time.Now(), RunID: a.runID,
		Detail: map[string]any{"role": string(core.RoleOptimizer)},
	})
}

// Call dials one optimizer-side request through the escalation
// ladder. The valve is checked before every dial.
func (a *Advisor) Call(ctx context.Context, stage, prompt string) (string, error) {
	if a.ValveTripped() {
		a.notifyValve()
		return "", ErrOptBudget
	}
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return "", fmt.Errorf("create opt-calls dir: %w", err)
	}
	floor := max(a.optMaxTokens, config.DefaultOptMaxTokens, a.floor)
	var last provider.ChatResponse
	for attempt := range maxOptAttempts {
		resp, err := a.chat(ctx, stage, prompt, floor<<attempt)
		if err != nil {
			return "", err
		}
		last = resp
		if resp.Content != "" {
			if attempt > 0 {
				// Remember the working cap so later stages skip the
				// reasoning burn instead of re-escalating.
				a.floor = floor << attempt
			}
			return resp.Content, nil
		}
		if resp.FinishReason != "length" && resp.ReasoningContent == "" {
			return "", fmt.Errorf("opt call %s: 模型返回空内容（finish_reason=%s）",
				stage, cmp.Or(resp.FinishReason, "unknown"))
		}
		// Reasoning consumed the completion cap: escalate.
	}
	return "", fmt.Errorf("opt call %s: 连续 %d 次空内容（finish_reason=%s，completion=%d tokens）— 推理模型耗尽了全部 completion 预算，请提高 --max-tokens",
		stage, maxOptAttempts, cmp.Or(last.FinishReason, "unknown"), last.Usage.CompletionTokens)
}

// chat performs one traced round trip. Every attempt — including
// escalations — writes its own trace file with its own sequence
// number. Usage counts even when the content is empty: the tokens
// were spent either way.
func (a *Advisor) chat(ctx context.Context, stage, prompt string, tokenCap int) (provider.ChatResponse, error) {
	a.seq++
	req := provider.ChatRequest{
		Model:     a.model,
		MaxTokens: tokenCap,
		Role:      core.RoleOptimizer,
		ExtraBody: a.extraBody,
		Messages:  []provider.Message{{Role: "user", Content: prompt}},
	}
	start := time.Now()
	resp, err := a.provider.Chat(ctx, req)
	trace := eval.CallTrace{
		Seq:       a.seq,
		SampleID:  stage,
		Role:      core.RoleOptimizer,
		Request:   req,
		ExtraBody: a.extraBody,
		Response:  resp,
		LatencyMS: time.Since(start).Milliseconds(),
		Time:      start,
	}
	if err != nil {
		trace.Error = err.Error()
	}
	path := filepath.Join(a.dir, fmt.Sprintf("%03d-%s.json", a.seq, stage))
	if werr := saveJSON(path, trace); werr != nil && err == nil {
		err = fmt.Errorf("write opt call trace: %w", werr)
	}
	if err != nil {
		return provider.ChatResponse{}, fmt.Errorf("opt call %s: %w", stage, err)
	}
	a.budget.RecordUsage(core.RoleOptimizer, resp.Usage)
	// Every successful optimizer dial publishes a usage snapshot so the
	// live budget gauge tracks optimizer-role consumption too.
	if a.onEvent != nil {
		a.onEvent(NewUsageEvent(a.budget, a.runID, stage))
	}
	return resp, nil
}

// NewUsageEvent builds one usage snapshot event carrying the full
// Budget.Snapshot() — evaluations started plus per-role usage for both
// roles — under Detail. The advisor and the cmd layer's terminal
// emitters share this constructor so every usage event on the wire has
// one payload shape.
func NewUsageEvent(b *eval.Budget, runID, stage string) eval.Event {
	evals, usage := b.Snapshot()
	return eval.Event{
		Type: EventUsage, Time: time.Now(), RunID: runID,
		Detail: map[string]any{
			"stage":         stage,
			"evals":         evals,
			"usage_by_role": usage,
		},
	}
}

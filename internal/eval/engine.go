// Package eval implements the metric suite, the dispatch budget and
// the parallel evaluation engine of the v2 pipeline.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cmp"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Event types emitted over the run lifecycle.
const (
	EventRunStart    = "run_start"
	EventSampleStart = "sample_start"
	EventSampleDone  = "sample_done"
	EventBudgetStop  = "budget_stop"
	EventRunDone     = "run_done"
)

// Event is one observable pipeline step. It feeds the live SSE stream
// and the events.jsonl replay log; sample_done carries the request and
// response summaries plus usage so a finished run can be replayed
// offline (reasoning text lives in the per-call traces only). Usage on
// sample_done is the sample's total evaluation spend — the executor
// call plus the judge call when the llm_judge metric is declared;
// LatencyMS stays the executor call alone and JudgeMS, when set, is
// the judge call's latency. Scores are per-metric means over the
// sample's reps (single call when --reps 1); ScoresSD carries the
// per-metric in-sample standard deviation over those reps and is only
// set when Reps > 1.
type Event struct {
	Type          string                   `json:"type"`
	Time          time.Time                `json:"time"`
	RunID         string                   `json:"run_id"`
	SampleID      string                   `json:"sample_id,omitempty"`
	Prompt        string                   `json:"prompt,omitempty"`
	Response      string                   `json:"response,omitempty"`
	Scores        map[string]float64       `json:"scores,omitempty"`
	ScoresSD      map[string]float64       `json:"scores_sd,omitempty"`
	Diagnosis     map[string]string        `json:"diagnosis,omitempty"`
	Usage         *core.Usage              `json:"usage,omitempty"`
	LatencyMS     int64                    `json:"latency_ms,omitempty"`
	JudgeMS       int64                    `json:"judge_ms,omitempty"`
	Error         string                   `json:"error,omitempty"`
	Status        string                   `json:"status,omitempty"`
	ExitCode      int                      `json:"exit_code,omitempty"`
	Undispatched  int                      `json:"undispatched,omitempty"`
	MetricMeans   map[string]float64       `json:"metric_means,omitempty"`
	UsageByRole   map[core.Role]core.Usage `json:"usage_by_role,omitempty"`
	FailedSamples []string                 `json:"failed_samples,omitempty"`
	// Detail carries stage-specific payloads for non-eval lifecycle
	// events (harness synthesis/filter/checkpoint counters) so the
	// events.jsonl + SSE envelope stays single-shaped.
	Detail map[string]any `json:"detail,omitempty"`
}

// CallTrace records one LLM call of one sample; it is serialized to
// runs/<run_id>/calls/<seq>-<sample_id>.json. Stage names the calling
// pipeline phase when it is not the default executor answer ("judge"
// for the llm_judge metric; optimizer-side calls reuse SampleID for
// their stage). ExtraBody is the audit trail of the --extra-body
// surface: ChatRequest.ExtraBody is json:"-" (the backends merge it
// onto the wire payload's top level after marshaling), so the call
// trace carries the map itself — with the documented merge contract
// (user keys override base fields, merged top level) the exact wire
// payload is reconstructible from the trace.
type CallTrace struct {
	Seq       int                   `json:"seq"`
	SampleID  string                `json:"sample_id"`
	Role      core.Role             `json:"role"`
	Stage     string                `json:"stage,omitempty"`
	Request   provider.ChatRequest  `json:"request"`
	ExtraBody map[string]any        `json:"extra_body,omitempty"`
	Response  provider.ChatResponse `json:"response"`
	LatencyMS int64                 `json:"latency_ms"`
	Time      time.Time             `json:"time"`
	Error     string                `json:"error,omitempty"`
}

// Engine evaluates a candidate over dataset samples with a worker
// pool. One Engine instance runs once. Temperature (default 0 = field
// omitted from the wire, the gateway default takes over) and Reps
// (default 1 = single sampling) are the statistical knobs of the
// evidence pipeline: Reps > 1 turns every sample score into a k-rep
// mean and feeds the per-metric in-sample sd downstream.
type Engine struct {
	RunID       string
	RunDir      string
	Model       string
	MaxTokens   int
	Workers     int
	Metrics     []string
	Budget      *Budget
	Provider    provider.Provider
	Role        core.Role // defaults to executor
	Temperature float64   // executor sampling temperature; 0 = omit from request
	Reps        int       // sampling repetitions per sample; <= 1 = single pass
	// Judge coverage: an optional second LLM dedicated to the llm_judge
	// metric (independent provider instance, model name and completion
	// budget). Zero values fall back to the executor's values per field
	// — nil provider → Provider, empty model → Model, 0 tokens →
	// MaxTokens (the cmp.Or precedent of Role below) — so an unset
	// judge surface reproduces the historical same-LLM behavior.
	JudgeProvider  provider.Provider
	JudgeModel     string
	JudgeMaxTokens int
	// JudgeBackend selects the llm_judge implementation: "" / "llm"
	// (the zero value) keeps the generative judge — every engine that
	// predates the decision backend behaves byte-identically;
	// "decision" routes grading through the SystemOne decision-model
	// cascade (judge_decision.go). A decision-configured engine without
	// a DecisionClient fails samples loudly instead of silently mixing
	// judges within one run.
	JudgeBackend   string
	DecisionClient *provider.SystemOneClient
	// Decision cascade thresholds: 0 = the eval-package defaults
	// (DefaultDecisionConfidence 0.5, DefaultDecisionDiagBelow 0.6).
	// DecisionLevels overrides the default 4-level score rubric (nil =
	// default); the normalization divisor is always the actual level
	// count minus one.
	JudgeDecisionConfidence float64
	JudgeDecisionDiagBelow  float64
	DecisionLevels          []string
	// ExtraBody carries gateway-private JSON fields (e.g.
	// chat_template_kwargs to disable thinking) merged onto every
	// executor request's wire payload top level. Deliberately NOT
	// forwarded to judge calls: the judge is an independent surface
	// (provider/model may differ) and blind-forwarding executor-side
	// gateway parameters to a different judge gateway could break it.
	ExtraBody   map[string]any
	TaskName    string
	CandidateID string
	DatasetName string
	Split       string
	OnEvent     func(Event)
}

// sampleOutcome is the per-sample result aggregated into RunResult.
// usage carries the sample's total evaluation spend (executor plus
// judge when llm_judge is declared, accumulated over all reps);
// latency is the executor time and judgeMS the judge time, kept
// separate so dashboards keep their executor-only DurationMS
// semantics. scores are per-metric means over the reps and scoresSD
// the per-metric in-sample sd (only computed for Reps > 1).
type sampleOutcome struct {
	undispatched bool
	failed       bool
	scores       map[string]float64
	scoresSD     map[string]float64
	diagnosis    map[string]string
	response     string
	reasoning    string
	usage        core.Usage
	latency      time.Duration
	judgeMS      int64
	err          string
}

// Run evaluates every sample and returns the aggregated result. The
// exit code is 2 when samples remain undispatched due to the budget
// (this takes precedence over failures), 1 when any dispatched sample
// failed after provider retries or the run was interrupted, else 0.
// An interrupted run (ctx canceled) reports status "aborted" so
// cancellation is never mistaken for budget exhaustion or a bad
// candidate.
func (e *Engine) Run(ctx context.Context, cand core.Candidate, samples []core.Sample) (core.RunResult, error) {
	role := cmp.Or(e.Role, core.RoleExecutor)
	if err := os.MkdirAll(filepath.Join(e.RunDir, "calls"), 0o755); err != nil {
		return core.RunResult{}, fmt.Errorf("create calls dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(e.RunDir, "samples"), 0o755); err != nil {
		return core.RunResult{}, fmt.Errorf("create samples dir: %w", err)
	}

	start := time.Now()
	res := core.RunResult{
		RunID:        e.RunID,
		TaskName:     e.TaskName,
		CandidateID:  e.CandidateID,
		DatasetName:  e.DatasetName,
		Split:        e.Split,
		TotalSamples: len(samples),
		StartedAt:    start,
	}
	e.emit(Event{Type: EventRunStart, Time: start, RunID: e.RunID})

	outcomes := make([]sampleOutcome, len(samples))
	var callSeq atomic.Int64
	notifyStop := sync.OnceFunc(func() {
		e.emit(Event{Type: EventBudgetStop, Time: time.Now(), RunID: e.RunID})
	})

	jobs := make(chan int)
	var wg sync.WaitGroup
	for range max(e.Workers, 1) {
		wg.Go(func() {
			for idx := range jobs {
				outcomes[idx] = e.evaluate(ctx, role, cand, samples[idx], &callSeq, notifyStop)
			}
		})
	}

	// Dispatch: the budget precheck runs before a sample enters the
	// pool, so --budget-evals is never exceeded. A multi-rep sample
	// reserves one slot per rep up front (TryAcquireEvals, all or
	// nothing) — "每 rep 计一次评估次数" is a dispatch-time fact, not
	// just a runtime accumulation inside evaluate. Denied samples stay
	// undispatched; a token soft stop only halts future dispatches.
	// Cancellation halts dispatch too: the rest stays undispatched
	// instead of burning budget on doomed calls and writing junk
	// traces.
	for i := range samples {
		if ctx.Err() != nil {
			for j := i; j < len(samples); j++ {
				outcomes[j].undispatched = true
			}
			break
		}
		if !e.Budget.TryAcquireEvals(int64(max(e.Reps, 1))) {
			outcomes[i].undispatched = true
			continue
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	sums := make(map[string]float64)
	counts := make(map[string]int)
	for i, oc := range outcomes {
		switch {
		case oc.undispatched:
			res.Undispatched++
		case oc.failed:
			res.Evaluated++
			res.FailedSamples = append(res.FailedSamples, samples[i].ID)
		default:
			res.Evaluated++
			for m, v := range oc.scores {
				sums[m] += v
				counts[m]++
			}
		}
	}
	res.MetricMeans = make(map[string]float64, len(sums))
	for m, sum := range sums {
		res.MetricMeans[m] = sum / float64(counts[m])
	}
	_, usage := e.Budget.Snapshot()
	res.UsageByRole = usage
	res.FinishedAt = time.Now()

	switch {
	case ctx.Err() != nil:
		res.Status = core.StatusAborted
		res.ExitCode = 1
	case res.Undispatched > 0:
		res.Status = core.StatusBudgetExhausted
		res.ExitCode = 2
	case len(res.FailedSamples) > 0:
		res.Status = core.StatusFailed
		res.ExitCode = 1
	default:
		res.Status = core.StatusCompleted
		res.ExitCode = 0
	}

	e.emit(Event{
		Type: EventRunDone, Time: res.FinishedAt, RunID: e.RunID,
		Status: string(res.Status), ExitCode: res.ExitCode,
		Undispatched: res.Undispatched, MetricMeans: res.MetricMeans,
		UsageByRole: res.UsageByRole, FailedSamples: res.FailedSamples,
	})
	return res, nil
}

// evaluate renders, calls, scores and traces one sample. With Reps > 1
// the provider pass runs k = max(Reps, 1) times: per-sample scores are
// the k-rep means, scoresSD the per-metric in-sample sd, any failing
// rep fails the whole sample and usage accumulates over every rep (the
// dispatch loop reserved one eval slot per rep up front). Metrics named
// llm_judge are graded by a dedicated judge call — generative judge or
// decision-model cascade per JudgeBackend — whose usage and latency
// fold into the sample's total evidence (see Engine.judgeSample).
func (e *Engine) evaluate(ctx context.Context, role core.Role, cand core.Candidate, s core.Sample, callSeq *atomic.Int64, notifyStop func()) sampleOutcome {
	prompt := core.RenderPrompt(cand.Prompt, s.Input)
	e.emit(Event{Type: EventSampleStart, Time: time.Now(), RunID: e.RunID, SampleID: s.ID})

	req := provider.ChatRequest{
		Model:       e.Model,
		MaxTokens:   e.MaxTokens,
		Role:        role,
		Temperature: e.Temperature, // 0 keeps the field off the wire: gateway default takes over
		ExtraBody:   e.ExtraBody,
		Messages:    []provider.Message{{Role: "user", Content: prompt}},
	}
	reps := max(e.Reps, 1)
	oc := sampleOutcome{}
	perRep := make(map[string][]float64, len(e.Metrics))
	var repScores []map[string]float64 // 逐 rep 逐指标分数（reps>1 落 trace，rep-strip 数据源）
	traceSeq := 0                      // first rep's call seq: unique per sample, names the sample trace
	for rep := range reps {
		out := e.executeRep(ctx, role, s, prompt, req, callSeq, notifyStop)
		if rep == 0 {
			traceSeq = out.seq
		}
		// Every rep's tokens were spent whether or not it scored.
		oc.usage.PromptTokens += out.usage.PromptTokens
		oc.usage.CompletionTokens += out.usage.CompletionTokens
		oc.judgeMS += out.judgeMS
		// Reasoning folds unconditionally, like usage above: a failed
		// rep's reasoning must still reach the sample trace.
		oc.reasoning = out.reasoning
		if out.err != "" {
			oc.failed = true
			oc.err = out.err
			break // 任一 rep 失败即样本失败
		}
		oc.latency += out.latency
		oc.response = out.content
		if reps > 1 {
			repScores = append(repScores, out.scores)
		}
		for m, v := range out.scores {
			perRep[m] = append(perRep[m], v)
		}
			if len(out.diagnosis) > 0 {
				if oc.diagnosis == nil {
					oc.diagnosis = make(map[string]string, len(out.diagnosis))
				}
				maps.Copy(oc.diagnosis, out.diagnosis)
			}
	}
	if !oc.failed {
		oc.scores = make(map[string]float64, len(perRep))
		for m, vals := range perRep {
			oc.scores[m] = repMean(vals)
		}
		if reps > 1 {
			oc.scoresSD = make(map[string]float64, len(perRep))
			for m, vals := range perRep {
				oc.scoresSD[m] = repSD(vals)
			}
		}
	}
	// 失败样本与 Scores/ScoresSD 同口径：不落逐 rep 分数，避免半截
	// 证据被读作完整评估。
	if oc.failed {
		repScores = nil
	}

	seq := traceSeq
	sampleTrace := core.SampleTrace{
		SampleID:   s.ID,
		Role:       role,
		Prompt:     prompt,
		Response:   oc.response,
		Reasoning:  oc.reasoning,
		Scores:     oc.scores,
		ScoresSD:   oc.scoresSD,
		RepScores:  repScores,
		Reps:       max(e.Reps, 1),
		Diagnosis:  oc.diagnosis,
		Error:      oc.err,
		Usage:      oc.usage,
		DurationMS: oc.latency.Milliseconds(),
		JudgeMS:    oc.judgeMS,
	}
	// The seq prefix keeps sample traces collision-free: distinct ids
	// may sanitize to the same filename ("train/001" vs "train:001")
	// and would otherwise overwrite each other under parallel workers.
	if werr := e.writeJSON(filepath.Join("samples", traceFile(seq, s.ID)), sampleTrace); werr != nil {
		oc.failed = true
		oc.err = strings.TrimSpace(oc.err + " " + werr.Error())
		sampleTrace.Error = oc.err
	}

	ev := Event{
		Type: EventSampleDone, Time: time.Now(), RunID: e.RunID, SampleID: s.ID,
		Prompt: prompt, LatencyMS: oc.latency.Milliseconds(), JudgeMS: oc.judgeMS,
	}
	if oc.failed {
		ev.Error = oc.err
	} else {
		ev.Response = oc.response
		ev.Scores = oc.scores
		ev.ScoresSD = oc.scoresSD
		ev.Diagnosis = oc.diagnosis
		ev.Usage = &oc.usage
	}
	e.emit(ev)
	return oc
}

// repOutcome is one provider pass's evidence for one sample, folded
// into the sampleOutcome by evaluate.
type repOutcome struct {
	seq       int
	usage     core.Usage
	latency   time.Duration
	judgeMS   int64
	content   string
	reasoning string
	scores    map[string]float64
	diagnosis map[string]string
	err       string
}

// executeRep runs one provider pass over a sample: the executor call,
// its budget metering, the metric loop (judge included) and the
// per-call trace. A non-empty err marks the rep — and therefore the
// sample — failed.
func (e *Engine) executeRep(ctx context.Context, role core.Role, s core.Sample, prompt string, req provider.ChatRequest, callSeq *atomic.Int64, notifyStop func()) repOutcome {
	start := time.Now()
	resp, err := e.Provider.Chat(ctx, req)
	latency := time.Since(start)
	seq := int(callSeq.Add(1))

	out := repOutcome{seq: seq, latency: latency}
	switch {
	case err != nil:
		out.err = err.Error()
	default:
		out.usage = resp.Usage
		// Reasoning is captured unconditionally: even a rep that
		// failed for empty content carries trace evidence (the
		// reasoning that consumed the completion budget).
		out.reasoning = resp.ReasoningContent
		// Usage counts against the budget even when the answer is
		// unusable: the tokens were spent either way.
		e.Budget.RecordUsage(role, resp.Usage)
		if e.Budget.SoftStopped() {
			notifyStop()
		}
		if resp.Content == "" && (resp.FinishReason == "length" || resp.ReasoningContent != "") {
			// A reasoning model can spend the whole completion budget
			// on reasoning and return no content. That is a max_tokens
			// configuration problem, not a wrong answer: fail with an
			// explicit attribution instead of scoring it 0.
			out.err = fmt.Sprintf("empty content, finish_reason=%s", cmp.Or(resp.FinishReason, "unknown"))
		} else {
			out.content = resp.Content
			out.scores = make(map[string]float64, len(e.Metrics))
			out.diagnosis = make(map[string]string)
			for _, m := range e.Metrics {
				if m != MetricLLMJudge {
					mr := Evaluate(m, resp.Content, s.Expected)
					out.scores[m] = mr.Score
					if mr.Diagnosis != "" {
						out.diagnosis[m] = mr.Diagnosis
					}
					continue
				}
				mr, judgeUsage, judgeLatency, jerr := e.judgeSample(ctx, s, resp.Content, callSeq)
				out.usage.PromptTokens += judgeUsage.PromptTokens
				out.usage.CompletionTokens += judgeUsage.CompletionTokens
				out.judgeMS = judgeLatency.Milliseconds()
				if e.Budget.SoftStopped() {
					notifyStop()
				}
				if jerr != nil {
					// The judge being unavailable means incomplete
					// evidence: fail the sample loudly instead of
					// scoring it silently.
					out.err = "llm_judge: " + jerr.Error()
					continue
				}
				out.scores[m] = mr.Score
				if mr.Diagnosis != "" {
					out.diagnosis[m] = mr.Diagnosis
				}
			}
		}
	}

	trace := CallTrace{
		Seq:       seq,
		SampleID:  s.ID,
		Role:      role,
		Request:   req,
		ExtraBody: req.ExtraBody,
		Response:  resp,
		LatencyMS: latency.Milliseconds(),
		Time:      start,
		Error:     out.err,
	}
	// Traces are contractual artifacts: failing to write one fails the
	// sample loudly instead of silently losing evidence.
	if werr := e.writeJSON(filepath.Join("calls", traceFile(seq, s.ID)), trace); werr != nil {
		out.err = strings.TrimSpace(out.err + " " + werr.Error())
		trace.Error = out.err
	}
	return out
}

// repMean is the mean over one sample's rep scores.
func repMean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// repSD is the in-sample standard deviation over one sample's rep
// scores (population form, ÷k, so a single rep reads 0): the
// per-dimension noise floor the frontier's ε margins pool from.
func repSD(vals []float64) float64 {
	if len(vals) < 2 {
		return 0
	}
	m := repMean(vals)
	var ss float64
	for _, v := range vals {
		ss += (v - m) * (v - m)
	}
	return math.Sqrt(ss / float64(len(vals)))
}

func (e *Engine) emit(ev Event) {
	if e.OnEvent != nil {
		e.OnEvent(ev)
	}
}

// writeJSON serializes v under RunDir with stable formatting.
func (e *Engine) writeJSON(rel string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.RunDir, rel), append(b, '\n'), 0o644)
}

func traceFile(seq int, sampleID string) string {
	return fmt.Sprintf("%03d-%s.json", seq, sanitizeID(sampleID))
}

// sanitizeID keeps sample ids filesystem-safe.
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

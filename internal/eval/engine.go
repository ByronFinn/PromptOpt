// Package eval implements the metric suite, the dispatch budget and
// the parallel evaluation engine of the v2 pipeline.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
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
// offline (reasoning text lives in the per-call traces only).
type Event struct {
	Type          string                   `json:"type"`
	Time          time.Time                `json:"time"`
	RunID         string                   `json:"run_id"`
	SampleID      string                   `json:"sample_id,omitempty"`
	Prompt        string                   `json:"prompt,omitempty"`
	Response      string                   `json:"response,omitempty"`
	Scores        map[string]float64       `json:"scores,omitempty"`
	Diagnosis     map[string]string        `json:"diagnosis,omitempty"`
	Usage         *core.Usage              `json:"usage,omitempty"`
	LatencyMS     int64                    `json:"latency_ms,omitempty"`
	Error         string                   `json:"error,omitempty"`
	Status        string                   `json:"status,omitempty"`
	ExitCode      int                      `json:"exit_code,omitempty"`
	Undispatched  int                      `json:"undispatched,omitempty"`
	MetricMeans   map[string]float64       `json:"metric_means,omitempty"`
	UsageByRole   map[core.Role]core.Usage `json:"usage_by_role,omitempty"`
	FailedSamples []string                 `json:"failed_samples,omitempty"`
}

// CallTrace records one LLM call of one sample; it is serialized to
// runs/<run_id>/calls/<seq>-<sample_id>.json.
type CallTrace struct {
	Seq       int                   `json:"seq"`
	SampleID  string                `json:"sample_id"`
	Role      core.Role             `json:"role"`
	Request   provider.ChatRequest  `json:"request"`
	Response  provider.ChatResponse `json:"response"`
	LatencyMS int64                 `json:"latency_ms"`
	Time      time.Time             `json:"time"`
	Error     string                `json:"error,omitempty"`
}

// Engine evaluates a candidate over dataset samples with a worker
// pool. One Engine instance runs once.
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
	TaskName    string
	CandidateID string
	DatasetName string
	Split       string
	OnEvent     func(Event)
}

// sampleOutcome is the per-sample result aggregated into RunResult.
type sampleOutcome struct {
	undispatched bool
	failed       bool
	scores       map[string]float64
	diagnosis    map[string]string
	usage        core.Usage
	latency      time.Duration
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				outcomes[idx] = e.evaluate(ctx, role, cand, samples[idx], &callSeq, notifyStop)
			}
		}()
	}

	// Dispatch: the budget precheck runs before a sample enters the
	// pool, so --budget-evals is never exceeded. Denied samples stay
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
		if !e.Budget.TryAcquireEval() {
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

// evaluate renders, calls, scores and traces one sample.
func (e *Engine) evaluate(ctx context.Context, role core.Role, cand core.Candidate, s core.Sample, callSeq *atomic.Int64, notifyStop func()) sampleOutcome {
	prompt := core.RenderPrompt(cand.Prompt, s.Input)
	e.emit(Event{Type: EventSampleStart, Time: time.Now(), RunID: e.RunID, SampleID: s.ID})

	req := provider.ChatRequest{
		Model:     e.Model,
		MaxTokens: e.MaxTokens,
		Role:      role,
		Messages:  []provider.Message{{Role: "user", Content: prompt}},
	}
	start := time.Now()
	resp, err := e.Provider.Chat(ctx, req)
	latency := time.Since(start)
	seq := int(callSeq.Add(1))

	oc := sampleOutcome{latency: latency}
	switch {
	case err != nil:
		oc.failed = true
		oc.err = err.Error()
	default:
		oc.usage = resp.Usage
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
			oc.failed = true
			oc.err = fmt.Sprintf("empty content, finish_reason=%s", cmp.Or(resp.FinishReason, "unknown"))
		} else {
			oc.scores = make(map[string]float64, len(e.Metrics))
			oc.diagnosis = make(map[string]string)
			for _, m := range e.Metrics {
				mr := Evaluate(m, resp.Content, s.Expected)
				oc.scores[m] = mr.Score
				if mr.Diagnosis != "" {
					oc.diagnosis[m] = mr.Diagnosis
				}
			}
		}
	}

	trace := CallTrace{
		Seq:       seq,
		SampleID:  s.ID,
		Role:      role,
		Request:   req,
		Response:  resp,
		LatencyMS: latency.Milliseconds(),
		Time:      start,
		Error:     oc.err,
	}
	// Traces are contractual artifacts: failing to write one fails the
	// sample loudly instead of silently losing evidence.
	if werr := e.writeJSON(filepath.Join("calls", traceFile(seq, s.ID)), trace); werr != nil {
		oc.failed = true
		oc.err = strings.TrimSpace(oc.err + " " + werr.Error())
		trace.Error = oc.err
	}
	sampleTrace := core.SampleTrace{
		SampleID:   s.ID,
		Role:       role,
		Prompt:     prompt,
		Response:   resp.Content,
		Reasoning:  resp.ReasoningContent,
		Scores:     oc.scores,
		Diagnosis:  oc.diagnosis,
		Error:      oc.err,
		Usage:      oc.usage,
		DurationMS: latency.Milliseconds(),
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
		Prompt: prompt, LatencyMS: latency.Milliseconds(),
	}
	if oc.failed {
		ev.Error = oc.err
	} else {
		ev.Response = resp.Content
		ev.Scores = oc.scores
		ev.Diagnosis = oc.diagnosis
		ev.Usage = &oc.usage
	}
	e.emit(ev)
	return oc
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

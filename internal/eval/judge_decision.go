package eval

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// llm_judge backend selectors (Engine.JudgeBackend). The zero value
// keeps the generative judge — every run that predates the decision
// backend behaves byte-identically.
const (
	// JudgeBackendLLM grades through the generative judge LLM (judge.go).
	JudgeBackendLLM = "llm"
	// JudgeBackendDecision grades through the SystemOne decision-model
	// cascade (judge_decision.go): cheap decision scoring first, the
	// generative judge only where the cascade declines the answer.
	JudgeBackendDecision = "decision"
)

// Cascade defaults (flag help cites the measured basis: tcmsp-30 实测
// tev1:0.8b confidence 可低至 0.21/0.111 — a 0.5 default rejects those
// to the generative judge instead of trusting them blindly).
const (
	// DefaultDecisionConfidence: below this decisiveness the decision
	// answer falls back to the generative judge entirely.
	DefaultDecisionConfidence = 0.5
	// DefaultDecisionDiagBelow: a normalized score below this line buys
	// the Chinese diagnosis from the generative judge — GEPA's
	// reflection loop reads diagnosis, and the decision model covers
	// only the score half of the judge contract.
	DefaultDecisionDiagBelow = 0.6
)

// decisionState is the systemone "state" payload: the judge role and
// scoring principle of the built-in llm_judge rubric, condensed to one
// sentence (the decision model has no system message — the state is the
// persona slot).
const decisionState = "你是提示词优化管线的评估裁判，只依据内容正确性对照参考答案评分，忽略格式差异"

// decisionQuestion is the fixed name of the single score question per
// sample (the questions object is name-indexed; one name keeps traces
// and answer lookup unambiguous).
const decisionQuestion = "q1"

// decisionLevels is the default score rubric: 4 ascending levels, so
// the decision score normalizes by 3. Overridable per Engine
// (DecisionLevels) for golden-fixture reproduction against a service's
// own level count — the divisor is always the question's actual level
// count minus one, never a constant.
var decisionLevels = []string{"完全错误", "部分正确", "基本正确", "与参考答案完全等价"}

// judgeSample dispatches one llm_judge grading to the configured
// backend. The zero-value/unknown backend keeps the generative judge.
// A decision-configured engine without a decision client fails the
// sample loudly instead of silently grading through the generative
// judge: within one run there is exactly one judge (research 0001 §3.2
// — the verify differential gate requires both sides on the same
// surface), and a silent fallback would mix them.
func (e *Engine) judgeSample(ctx context.Context, s core.Sample, output string, callSeq *atomic.Int64) (MetricResult, core.Usage, time.Duration, error) {
	if e.JudgeBackend == JudgeBackendDecision {
		if e.DecisionClient == nil {
			return MetricResult{}, core.Usage{}, 0, errors.New("judge backend decision requires DecisionClient")
		}
		return e.judgeDecision(ctx, s, output, callSeq)
	}
	return e.judge(ctx, s, output, callSeq)
}

// judgeDecision grades one sample through the SystemOne decision model
// with a two-way cascade fallback to the generative judge (judge()):
//
//   - confidence < fallback threshold → the decision answer is too
//     indecisive to trust; the generative verdict replaces it entirely
//     (score + diagnosis from the same call, so reflection reads a
//     coherent pair).
//   - normalized score < diagnosis line → the score half is plausible
//     but the sample needs the Chinese diagnosis the decision model
//     cannot produce; the generative judge supplies it (and its own
//     score with it, for the same coherence).
//
// A transport/protocol error never cascades: an unreachable decision
// service is incomplete evidence and fails the sample loudly, exactly
// like an unreachable judge LLM — a systemic fallback would silently
// re-label the whole run as generatively judged.
//
// Normalization: the decision score is a probability-weighted level
// index in [0, len(levels)-1]; dividing by the question's actual level
// count minus one lands it on the generative judge's 0~1 scale, so both
// backends feed the same metric means and CIs.
//
// Metering: the decision call's usage records under RoleJudge (which
// arms the same token soft stop as the executor — total evaluation cost
// stays one valve); on fallback the generative call meters its own
// inside judge(), and both calls' usage/latency fold into the sample.
// The decision call writes its own CallTrace (Stage "judge-decision",
// Role RoleJudge): the ChatRequest/Response projection carries the
// model + evidence and the full systemone answer JSON respectively —
// the wire body is derivable from (model, state, levels, evidence),
// with state/levels being run constants.
func (e *Engine) judgeDecision(ctx context.Context, s core.Sample, output string, callSeq *atomic.Int64) (MetricResult, core.Usage, time.Duration, error) {
	levels := e.DecisionLevels
	if len(levels) == 0 {
		levels = decisionLevels
	}
	if len(levels) < 2 {
		return MetricResult{}, core.Usage{}, 0, fmt.Errorf("decision levels need >= 2 entries for normalization, got %d", len(levels))
	}
	evidence := judgeEvidence(s, output)
	req := provider.SystemOneRequest{
		Model: e.DecisionClient.Model(),
		State: decisionState,
		Questions: map[string]provider.SystemOneQuestion{
			decisionQuestion: {Type: "score", Instructions: evidence, Criteria: levels},
		},
	}

	start := time.Now()
	resp, err := e.DecisionClient.Do(ctx, req)
	latency := time.Since(start)
	seq := int(callSeq.Add(1))
	usage := resp.Usage.Usage()

	// Tokens were spent either way: meter before anything else.
	e.Budget.RecordUsage(core.RoleJudge, usage)

	trace := CallTrace{
		Seq:      seq,
		SampleID: s.ID,
		Role:     core.RoleJudge,
		Stage:    "judge-decision",
		Request: provider.ChatRequest{
			Model:    req.Model,
			Role:     core.RoleJudge,
			Messages: []provider.Message{{Role: "user", Content: evidence}},
		},
		Response:  provider.ChatResponse{FinishReason: "systemone", Usage: usage},
		LatencyMS: latency.Milliseconds(),
		Time:      start,
	}
	if err == nil {
		if b, merr := json.Marshal(resp); merr == nil {
			trace.Response.Content = string(b)
		} else {
			err = fmt.Errorf("encode systemone answer: %w", merr)
		}
	} else {
		trace.Error = err.Error()
	}
	if werr := e.writeJSON(filepath.Join("calls", traceFile(seq, s.ID)), trace); werr != nil && err == nil {
		err = fmt.Errorf("write judge-decision trace: %w", werr)
	}
	if err != nil {
		return MetricResult{}, usage, latency, err
	}

	ans, ok := resp.Answers[decisionQuestion]
	if !ok {
		return MetricResult{}, usage, latency, fmt.Errorf("systemone response missing answer %q", decisionQuestion)
	}
	confidence := cmp.Or(e.JudgeDecisionConfidence, DefaultDecisionConfidence)
	if ans.Confidence < confidence {
		return e.decisionFallback(ctx, s, output, usage, latency, callSeq)
	}
	norm := min(max(ans.Score/float64(len(levels)-1), 0), 1)
	diagBelow := cmp.Or(e.JudgeDecisionDiagBelow, DefaultDecisionDiagBelow)
	if norm < diagBelow {
		return e.decisionFallback(ctx, s, output, usage, latency, callSeq)
	}
	// Trusted decision answer: the diagnosis slot takes a deterministic
	// auditable placeholder (no timestamps, no randomness) — the score
	// half of the judge contract is covered, the diagnosis half is
	// explicitly marked absent rather than synthesized. Which trigger
	// would have fired is reconstructible from the trace's answer JSON
	// (confidence, score) against the run's thresholds.
	diag := fmt.Sprintf("决策裁判采信（confidence=%.4f ≥ 阈值 %.2f）：未触发生成式诊断", ans.Confidence, confidence)
	return MetricResult{Score: norm, Diagnosis: diag}, usage, latency, nil
}

// decisionFallback rides the P2-isolated generative judge chain for the
// verdict the decision model declined. The decision call's usage and
// latency are already metered and traced; judge() meters and traces the
// generative call itself, so the sums here never double-count. The
// paired judge-decision + judge traces are the fallback's audit trail.
func (e *Engine) decisionFallback(ctx context.Context, s core.Sample, output string, usage core.Usage, latency time.Duration, callSeq *atomic.Int64) (MetricResult, core.Usage, time.Duration, error) {
	mr, judgeUsage, judgeLatency, err := e.judge(ctx, s, output, callSeq)
	return mr,
		core.Usage{
			PromptTokens:     usage.PromptTokens + judgeUsage.PromptTokens,
			CompletionTokens: usage.CompletionTokens + judgeUsage.CompletionTokens,
		},
		latency + judgeLatency,
		err
}

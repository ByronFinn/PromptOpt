package eval

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// MetricLLMJudge declares an LLM-as-judge score: the candidate output is
// graded 0~1 by the run's provider against the sample's reference
// answer, with a short Chinese diagnosis that rides the ASI seam into
// reflection. Unlike the deterministic metrics it cannot run inside
// Evaluate (it needs the provider), so the engine dispatches it
// specially in its metric loop.
const MetricLLMJudge = "llm_judge"

// judgeRubric is the built-in Chinese scoring contract of every judge
// call: content correctness against the reference answer only, plus a
// diagnosis capped at 80 characters so traces stay reviewable.
const judgeRubric = `你是提示词优化管线的评估裁判，对照参考答案为候选输出打分。

评分规则：
1. 只依据内容正确性评分，忽略空白、标点、大小写与 JSON 键序等格式差异；
2. score 为 0~1 的小数：1=与参考答案完全等价，0=完全错误或答非所问，部分正确按正确比例给分；
3. diagnosis 用不超过 80 字的中文说明给分依据，重点解释扣分原因。

只输出一个 JSON 对象，禁止输出任何其他文字：
{"score": 0.0, "diagnosis": "中文诊断"}`

// judgePrompt renders the rubric plus one sample's evidence: the input,
// the reference answer (JSON form for structured expectations) and the
// candidate output under judgment.
func judgePrompt(s core.Sample, output string) string {
	return judgeRubric + "\n\n" + judgeEvidence(s, output)
}

// judgeEvidence assembles one sample's judging evidence — the input,
// the reference answer (JSON form for structured expectations) and the
// candidate output under judgment. Shared verbatim by the generative
// judge prompt (judgePrompt) and the decision-model question
// instructions (judge_decision.go), so both judge surfaces read the
// same evidence for the same sample.
func judgeEvidence(s core.Sample, output string) string {
	expected, err := json.Marshal(s.Expected)
	if err != nil {
		expected = []byte(fmt.Sprintf("%v", s.Expected))
	}
	return fmt.Sprintf(`【样本输入】
%s

【参考答案】
%s

【候选输出】
%s`, s.Input, expected, output)
}

// judgeVerdict is the wire shape the judge model must return.
type judgeVerdict struct {
	Score     float64 `json:"score"`
	Diagnosis string  `json:"diagnosis"`
}

// parseJudge decodes one judge response: fence-stripped first, then
// unmarshaled, then clamped into [0,1] so an out-of-range score degrades
// to the nearest bound instead of poisoning metric means. Anything that
// is not a JSON object with a numeric score is an error — a judge that
// cannot be understood is missing evidence, not a zero.
func parseJudge(raw string) (MetricResult, error) {
	var v judgeVerdict
	if err := json.Unmarshal([]byte(stripFence(raw)), &v); err != nil {
		return MetricResult{}, fmt.Errorf("parse judge verdict: %s", truncate(err.Error(), 120))
	}
	return MetricResult{Score: min(max(v.Score, 0), 1), Diagnosis: v.Diagnosis}, nil
}

// judge grades one candidate output through the judge LLM. It writes
// its own CallTrace (Stage "judge", Role RoleJudge, sharing the run's
// call sequence so replays stay ordered), meters its usage into the
// budget under the judge role — which arms the same token soft stop as
// the executor, so total evaluation cost keeps one valve — and returns
// the verdict together with its usage and latency so evaluate can fold
// them into the sample's evidence. The returned usage is non-zero even
// when the verdict is unusable: the tokens were spent either way.
//
// The judge surface falls back per field to the executor's values
// (JudgeProvider → Provider, JudgeModel → Model, JudgeMaxTokens →
// MaxTokens), so an unset judge surface sends the exact request the
// pre-split code sent. Temperature stays pinned at 0: deterministic
// scoring is not a knob. ExtraBody is deliberately not forwarded even
// on fallback: the executor's extra body carries gateway-private
// parameters (e.g. chat_template_kwargs) tuned for the executor's
// gateway, and blind-forwarding them to a differently-configured
// judge endpoint could break the call. If a judge gateway ever needs
// its own extras that is a separate judge-surface flag, not a silent
// inheritance.
func (e *Engine) judge(ctx context.Context, s core.Sample, output string, callSeq *atomic.Int64) (MetricResult, core.Usage, time.Duration, error) {
	prov := e.Provider
	if e.JudgeProvider != nil {
		prov = e.JudgeProvider
	}
	req := provider.ChatRequest{
		Model:       cmp.Or(e.JudgeModel, e.Model),
		MaxTokens:   cmp.Or(e.JudgeMaxTokens, e.MaxTokens),
		Role:        core.RoleJudge,
		Temperature: 0, // deterministic scoring: the judge must not sample
		Messages:    []provider.Message{{Role: "user", Content: judgePrompt(s, output)}},
	}
	start := time.Now()
	resp, err := prov.Chat(ctx, req)
	latency := time.Since(start)
	seq := int(callSeq.Add(1))

	trace := CallTrace{
		Seq:       seq,
		SampleID:  s.ID,
		Role:      core.RoleJudge,
		Stage:     "judge",
		Request:   req,
		Response:  resp,
		LatencyMS: latency.Milliseconds(),
		Time:      start,
	}
	if err != nil {
		trace.Error = err.Error()
	}
	// Traces are contractual artifacts: failing to write one fails the
	// judge call loudly instead of silently losing evidence.
	if werr := e.writeJSON(filepath.Join("calls", traceFile(seq, s.ID)), trace); werr != nil && err == nil {
		err = fmt.Errorf("write judge trace: %w", werr)
	}
	if err != nil {
		return MetricResult{}, resp.Usage, latency, err
	}
	e.Budget.RecordUsage(core.RoleJudge, resp.Usage)
	mr, perr := parseJudge(resp.Content)
	if perr != nil {
		return MetricResult{}, resp.Usage, latency, perr
	}
	return mr, resp.Usage, latency, nil
}

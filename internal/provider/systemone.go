package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cmp"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// SystemOne decision-model protocol (P7 决策模型裁判级联).
//
// The service answers POST {base}/v1/systemone with a batch of scored
// questions instead of a chat completion:
//
//	request  {"model": "...", "state": "...", "questions": {"q1": {...}}}
//	question {"type": "score"|"choice"|"noul", "instructions": "...", "criteria": ...}
//	response {"model": "...", "answers": {"q1": {"type", "probabilities",
//	          "confidence", "score", "legend", "selected"}}, "usage":
//	          {"input_tokens", "output_tokens"}}
//
// Wire contract, pinned by re-review (the protocol shape was measured
// live on tcmsp-30 with two byte-identical read-only curls against
// tev1:0.8b / ollama 0.35.0; smoke-decision.sh re-proves it on the real
// service at deployment time):
//   - questions is a NAME-INDEXED object (not an array); answers uses
//     the same names as keys.
//   - criteria is polymorphic: for score questions an ASCENDING array
//     of level descriptions (index = level), for choice questions an
//     option→description mapping. PromptOpt only emits score questions.
//   - score is the probability-weighted level index — a continuous
//     value in [0, len(criteria)-1] (measured: 3 levels, score
//     1.386988025801118 ≈ 1·p1 + 2·p2). Callers normalize with
//     score/(levels-1); the divisor is the question's own level count
//     minus one, never a constant.
//   - confidence in [0,1] grades the answer's decisiveness; low values
//     drive the eval-package cascade fallback to the generative judge.
//
// state is the "persona" half of the request: PromptOpt fills it with
// the role + scoring-principle sentence of the built-in llm_judge
// rubric (see eval.decisionState), instructions carry one sample's
// evidence assembly.
const systemOnePath = "/v1/systemone"

// SystemOneRequest is the /v1/systemone request body. Questions must be
// name-indexed; the response echoes the names back in Answers.
type SystemOneRequest struct {
	Model     string                       `json:"model"`
	State     string                       `json:"state"`
	Questions map[string]SystemOneQuestion `json:"questions"`
}

// SystemOneQuestion is one scored question. Criteria follows the wire
// polymorphism: []string for score (ascending level descriptions),
// map[string]string for choice (option→description).
type SystemOneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

// SystemOneAnswer is one answer cell. Score is the expected level index
// (continuous); Confidence grades decisiveness in [0,1]; Legend echoes
// the level index→description map.
type SystemOneAnswer struct {
	Type          string             `json:"type"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Score         float64            `json:"score"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Selected      any                `json:"selected,omitempty"`
}

// SystemOneUsage mirrors the service's token accounting (input/output,
// not the chat-completions prompt/completion naming).
type SystemOneUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Usage converts into the pipeline's metering shape so the decision
// call lands in the same per-role usage maps as every other call.
func (u SystemOneUsage) Usage() core.Usage {
	return core.Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens}
}

// SystemOneResponse is the /v1/systemone response body.
type SystemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]SystemOneAnswer `json:"answers"`
	Usage   SystemOneUsage             `json:"usage"`
}

// SystemOneConfig tunes the decision-model client; zero values become
// defaults. No retry policy in P7: a failed decision call fails the
// sample loudly (incomplete evidence), matching judge() semantics —
// silently retrying/falling back would blur which judge graded what.
type SystemOneConfig struct {
	HTTPClient *http.Client
	Timeout    time.Duration // per-attempt deadline; default 180s; CLI: --timeout / PROMPTOPT_TIMEOUT
}

// SystemOneClient posts scored questions to one decision-model service.
// The model binds at construction (URL+model pair = the decision
// surface, the same way a judgeConn binds URL+key inside a Provider):
// one client serves one decision model for the whole run, so a run
// cannot silently mix two decision judges.
type SystemOneClient struct {
	baseURL string
	model   string
	http    *http.Client
	timeout time.Duration
}

// NewSystemOne returns a client for the decision service at baseURL
// (host root, e.g. http://127.0.0.1:11434 — the client appends
// /v1/systemone) scoring with model.
func NewSystemOne(baseURL, model string, cfg SystemOneConfig) *SystemOneClient {
	c := &SystemOneClient{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		model:   model,
		http:    cfg.HTTPClient,
		timeout: cmp.Or(cfg.Timeout, 180*time.Second),
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	return c
}

// Model returns the bound decision model name (the audit trail reads it
// for the CallTrace projection).
func (c *SystemOneClient) Model() string { return c.model }

// Do performs one scored-questions round trip. Transport errors and
// non-2xx answers surface as errors; decoding tolerates unknown fields
// so the service can evolve additively.
func (c *SystemOneClient) Do(ctx context.Context, req SystemOneRequest) (SystemOneResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return SystemOneResponse{}, fmt.Errorf("encode systemone request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		c.baseURL+systemOnePath, bytes.NewReader(payload))
	if err != nil {
		return SystemOneResponse{}, fmt.Errorf("build systemone request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// No retry policy here by design (P7: a failed decision call
		// fails the sample loudly) — the timeout message still points at
		// the knob so the operator can widen the deadline instead of
		// guessing.
		if errors.Is(err, context.DeadlineExceeded) {
			return SystemOneResponse{}, fmt.Errorf("systemone request: %w（单次尝试超过 %s 超时上限：调大 --timeout 或设置 PROMPTOPT_TIMEOUT）", err, c.timeout)
		}
		return SystemOneResponse{}, fmt.Errorf("systemone request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return SystemOneResponse{}, fmt.Errorf("systemone service returned %d: %s", resp.StatusCode, truncateStatusBody(body))
	}
	var out SystemOneResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SystemOneResponse{}, fmt.Errorf("decode systemone response: %w", err)
	}
	return out, nil
}

// truncateStatusBody keeps error messages bounded like StatusError.
func truncateStatusBody(body []byte) string {
	const max = 512
	s := string(body)
	if len(s) > max {
		return s[:max]
	}
	return s
}

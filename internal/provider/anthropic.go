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

// AnthropicVersion pins the Messages API schema this client speaks;
// sent as the required anthropic-version header on every request.
const AnthropicVersion = "2023-06-01"

// AnthropicConfig tunes a native Anthropic Messages client; zero
// values become defaults.
type AnthropicConfig struct {
	HTTPClient  *http.Client
	MaxAttempts int           // total attempts including the first; default 4 (initial + 3 retries)
	Backoff     time.Duration // exponential backoff base; default 500ms
	Timeout     time.Duration // per-attempt deadline; default 180s (reasoning models are slow)
}

// Anthropic talks to the native Anthropic /v1/messages endpoint. It
// retries 429/5xx and transport errors with exponential backoff,
// honors Retry-After, separates thinking blocks from the scored text
// content and meters usage per role.
type Anthropic struct {
	baseURL     string
	apiKey      string
	meter       *Meter
	http        *http.Client
	maxAttempts int
	backoff     time.Duration
	timeout     time.Duration
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewAnthropic returns a client for baseURL, the API root (e.g.
// https://api.anthropic.com) — unlike the OpenAI client the /v1
// prefix is not part of baseURL; /v1/messages is appended here.
func NewAnthropic(baseURL, apiKey string, cfg AnthropicConfig) *Anthropic {
	a := &Anthropic{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		meter:   NewMeter(),
		http:    cfg.HTTPClient,
		// Default 4 attempts = initial request + 3 retries.
		maxAttempts: cmp.Or(cfg.MaxAttempts, 4),
		backoff:     cmp.Or(cfg.Backoff, 500*time.Millisecond),
		timeout:     cmp.Or(cfg.Timeout, 180*time.Second),
		sleep:       sleepContext,
	}
	if a.http == nil {
		a.http = &http.Client{}
	}
	return a
}

// UsageSnapshot returns the per-role token totals recorded so far.
func (a *Anthropic) UsageSnapshot() map[core.Role]core.Usage {
	return a.meter.Snapshot()
}

// Chat performs one Messages completion, retrying retryable failures.
// On success the response usage is metered under the request's role.
func (a *Anthropic) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	p := retryPolicy{maxAttempts: a.maxAttempts, backoff: a.backoff, sleep: a.sleep}
	return p.run(ctx, a.meter, req.Role, func(ctx context.Context) (ChatResponse, error) {
		return a.attempt(ctx, req)
	})
}

// attempt performs one HTTP round trip against /v1/messages.
func (a *Anthropic) attempt(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	payload, err := buildMessagesRequest(req)
	if err != nil {
		return ChatResponse{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		a.baseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", AnthropicVersion)

	resp, err := a.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// Caller canceled: not worth retrying.
			return ChatResponse{}, fmt.Errorf("request aborted: %w", err)
		}
		// Per-attempt timeouts and other transport errors are retryable.
		return ChatResponse{}, &transportError{cause: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatResponse{}, &StatusError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Body:       string(body),
		}
	}

	var msg messagesResponse
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return msg.chatResponse()
}

// messagesRequest is the /v1/messages wire payload. system is
// top-level (the Messages API has no system role inside messages) and
// is omitted when the conversation carries no system message.
type messagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature,omitempty"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
}

// buildMessagesRequest validates req and converts it to the Messages
// wire format: system messages are split out into the top-level
// system field and the remaining conversation is merged into
// alternating user/assistant turns.
func buildMessagesRequest(req ChatRequest) ([]byte, error) {
	if req.MaxTokens <= 0 {
		return nil, errors.New("anthropic request requires max_tokens > 0")
	}
	system, convo, err := splitMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	return json.Marshal(messagesRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		System:      system,
		Messages:    convo,
	})
}

// splitMessages separates system messages (concatenated into one
// preamble) from the conversation and merges consecutive messages of
// the same role so the result alternates user/assistant.
func splitMessages(msgs []Message) (system string, convo []Message, err error) {
	var systems []string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			systems = append(systems, m.Content)
		case "user", "assistant":
			if n := len(convo); n > 0 && convo[n-1].Role == m.Role {
				convo[n-1].Content += "\n\n" + m.Content
				continue
			}
			convo = append(convo, m)
		default:
			return "", nil, fmt.Errorf("anthropic messages: unsupported role %q", m.Role)
		}
	}
	if len(convo) == 0 {
		return "", nil, errors.New("anthropic messages: need at least one non-system message")
	}
	return strings.Join(systems, "\n\n"), convo, nil
}

// messagesResponse mirrors the subset of the Anthropic Messages
// response the pipeline consumes.
type messagesResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// contentBlock is one block of the response content array: text
// blocks carry the answer, thinking blocks the extended-thinking
// trace. Other block types are ignored.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

// chatResponse distills the wire response into the pipeline shape;
// thinking stays separate from the scored content, mirroring how the
// OpenAI client treats reasoning_content.
func (m messagesResponse) chatResponse() (ChatResponse, error) {
	var texts, thinking []string
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			texts = append(texts, b.Text)
		case "thinking":
			thinking = append(thinking, b.Thinking)
		}
	}
	if len(texts) == 0 {
		return ChatResponse{}, errors.New("response contains no text content blocks")
	}
	out := ChatResponse{
		Content:          strings.Join(texts, "\n\n"),
		ReasoningContent: strings.Join(thinking, "\n\n"),
		FinishReason:     m.StopReason,
	}
	if m.Usage != nil {
		out.Usage = core.Usage{
			PromptTokens:     m.Usage.InputTokens,
			CompletionTokens: m.Usage.OutputTokens,
		}
	}
	return out, nil
}

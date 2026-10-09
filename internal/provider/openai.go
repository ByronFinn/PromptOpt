package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"cmp"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// OpenAIConfig tunes an OpenAI-compatible client; zero values become
// defaults.
type OpenAIConfig struct {
	HTTPClient  *http.Client
	MaxAttempts int           // total attempts including the first; default 4 (initial + 3 retries)
	Backoff     time.Duration // exponential backoff base; default 500ms
	Timeout     time.Duration // per-attempt deadline; default 180s (reasoning models are slow); CLI: --timeout / PROMPTOPT_TIMEOUT
	MaxRPS      float64       // client-side pacing, requests per second; 0 = off
}

// Client talks to an OpenAI-compatible /chat/completions endpoint.
// It retries 429/5xx and transport errors with exponential backoff,
// honors Retry-After, tolerates reasoning_content (kept separate from
// the scored content) and meters usage per role.
type Client struct {
	baseURL     string
	apiKey      string
	meter       *Meter
	http        *http.Client
	maxAttempts int
	backoff     time.Duration
	timeout     time.Duration
	sleep       func(ctx context.Context, d time.Duration) error
	limiter     *rateLimiter // nil when MaxRPS is off
}

// NewOpenAI returns a client for baseURL (e.g. http://host:port/v1).
func NewOpenAI(baseURL, apiKey string, cfg OpenAIConfig) *Client {
	c := &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		meter:   NewMeter(),
		http:    cfg.HTTPClient,
		// Default 4 attempts = initial request + 3 retries.
		maxAttempts: cmp.Or(cfg.MaxAttempts, 4),
		backoff:     cmp.Or(cfg.Backoff, 500*time.Millisecond),
		timeout:     cmp.Or(cfg.Timeout, 180*time.Second),
		sleep:       sleepContext,
		limiter:     newRateLimiter(cfg.MaxRPS, time.Now),
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	return c
}

// UsageSnapshot returns the per-role token totals recorded so far.
func (c *Client) UsageSnapshot() map[core.Role]core.Usage {
	return c.meter.Snapshot()
}

// Chat performs one completion, retrying retryable failures. On success
// the response usage is metered under the request's role.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	p := retryPolicy{maxAttempts: c.maxAttempts, backoff: c.backoff, sleep: c.sleep}
	return p.run(ctx, c.meter, req.Role, func(ctx context.Context) (ChatResponse, error) {
		return c.attempt(ctx, req)
	})
}

// attempt performs one HTTP round trip.
func (c *Client) attempt(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	// Pacing gate before every attempt (retries included): the limiter
	// enforces the configured request rate while the retry policy's
	// backoff and Retry-After waits stack on top of it.
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return ChatResponse{}, err
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("encode request: %w", err)
	}
	// ExtraBody merges onto the payload's top level (user keys win);
	// with no extras the payload bytes pass through untouched.
	if payload, err = mergeExtraBody(payload, req.ExtraBody); err != nil {
		return ChatResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(callCtx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// Caller canceled: not worth retrying.
			return ChatResponse{}, fmt.Errorf("request aborted: %w", err)
		}
		// A per-attempt deadline expiration would hit the same wall on
		// every retry — classified so the retry policy gives up and the
		// message points at the knob. Other transport errors (connection
		// refused, reset, EOF) stay retryable.
		if errors.Is(err, context.DeadlineExceeded) {
			return ChatResponse{}, &transportError{cause: err, timeout: c.timeout}
		}
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

	var completion chatCompletion
	if err := json.NewDecoder(resp.Body).Decode(&completion); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return ChatResponse{}, errors.New("response contains no choices")
	}
	msg := completion.Choices[0].Message
	out := ChatResponse{
		Content:          msg.Content,
		ReasoningContent: msg.ReasoningContent,
		FinishReason:     completion.Choices[0].FinishReason,
	}
	if completion.Usage != nil {
		out.Usage = core.Usage{
			PromptTokens:     completion.Usage.PromptTokens,
			CompletionTokens: completion.Usage.CompletionTokens,
		}
	}
	return out, nil
}

// mergeExtraBody merges the user-supplied extra body onto the
// marshaled request payload's top level. Contract: ExtraBody keys
// override the struct's own fields — the explicit --extra-body value
// is the user's stated intent (e.g. a gateway-private parameter
// colliding with a generic field name must win). With an empty extra
// map the payload bytes are returned untouched (no key reordering).
func mergeExtraBody(payload []byte, extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return payload, nil
	}
	var top map[string]any
	if err := json.Unmarshal(payload, &top); err != nil {
		return nil, fmt.Errorf("decode payload for extra_body merge: %w", err)
	}
	maps.Copy(top, extra)
	return json.Marshal(top)
}

// chatCompletion mirrors the subset of the OpenAI chat-completions
// response the pipeline consumes. reasoning_content is the field
// reasoning-capable gateways emit before the final content.
type chatCompletion struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

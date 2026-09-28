package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	Timeout     time.Duration // per-attempt deadline; default 180s (reasoning models are slow)
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
}

// maxRetryAfter caps how long a server-supplied Retry-After may stall
// the pipeline.
const maxRetryAfter = time.Minute

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
	for attempt := 0; ; attempt++ {
		resp, err := c.attempt(ctx, req)
		if err == nil {
			c.meter.Record(req.Role, resp.Usage)
			return resp, nil
		}
		if attempt+1 >= c.maxAttempts || ctx.Err() != nil {
			return ChatResponse{}, err
		}
		if wait, retry := retryDelay(err, attempt, c.backoff); retry {
			if werr := c.sleep(ctx, wait); werr != nil {
				return ChatResponse{}, fmt.Errorf("aborted while backing off: %w", werr)
			}
		} else {
			return ChatResponse{}, err
		}
	}
}

// retryDelay classifies err and returns the wait before the next
// attempt; retry reports whether another attempt helps at all.
func retryDelay(err error, attempt int, backoff time.Duration) (time.Duration, bool) {
	if se, ok := errors.AsType[*StatusError](err); ok {
		if !se.retryable() {
			return 0, false
		}
		if se.RetryAfter > 0 {
			return min(se.RetryAfter, maxRetryAfter), true
		}
		return backoff << min(attempt, 16), true
	}
	if _, ok := errors.AsType[*transportError](err); ok {
		return backoff << min(attempt, 16), true
	}
	return 0, false
}

// attempt performs one HTTP round trip.
func (c *Client) attempt(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("encode request: %w", err)
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

// StatusError is a non-2xx API response.
type StatusError struct {
	StatusCode int
	RetryAfter time.Duration // parsed from the Retry-After header, 0 when absent
	Body       string
}

func (e *StatusError) Error() string {
	snippet := e.Body
	if len(snippet) > 200 {
		snippet = snippet[:200] + "..."
	}
	return fmt.Sprintf("llm api status %d: %s", e.StatusCode, snippet)
}

// retryable: only 429 and 5xx responses get another attempt.
func (e *StatusError) retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// transportError is a failed HTTP round trip (connection refused,
// reset, per-attempt timeout, ...).
type transportError struct{ cause error }

func (e *transportError) Error() string { return "llm request failed: " + e.cause.Error() }

func (e *transportError) Unwrap() error { return e.cause }

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

// parseRetryAfter accepts delay-seconds and HTTP-date forms.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(time.Now()), 0)
	}
	return 0
}

// sleepContext waits for d or until ctx is done; injectable in tests.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// maxRetryAfter caps how long a server-supplied Retry-After may stall
// the pipeline.
const maxRetryAfter = time.Minute

// retryPolicy drives the retry loop shared by every backend client:
// attempts run until success, retry-budget exhaustion or a
// non-retryable failure; the winning attempt's usage is metered under
// the request role.
type retryPolicy struct {
	maxAttempts int                                              // total attempts including the first
	backoff     time.Duration                                    // exponential backoff base
	sleep       func(ctx context.Context, d time.Duration) error // injectable in tests
}

// run executes one attempt under the policy, retrying retryable
// failures with backoff.
func (p retryPolicy) run(ctx context.Context, m *Meter, role core.Role, attempt func(context.Context) (ChatResponse, error)) (ChatResponse, error) {
	for i := 0; ; i++ {
		resp, err := attempt(ctx)
		if err == nil {
			m.Record(role, resp.Usage)
			return resp, nil
		}
		if i+1 >= p.maxAttempts || ctx.Err() != nil {
			return ChatResponse{}, err
		}
		if wait, retry := retryDelay(err, i, p.backoff); retry {
			if werr := p.sleep(ctx, wait); werr != nil {
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
	if te, ok := errors.AsType[*transportError](err); ok {
		// A per-attempt timeout hits the same deadline on every retry
		// (tcmsp-30 实测：4×180s=723.9s 全废) — retrying cannot help,
		// the operator must raise --timeout / PROMPTOPT_TIMEOUT.
		if te.timeout > 0 {
			return 0, false
		}
		return backoff << min(attempt, 16), true
	}
	return 0, false
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
// reset, per-attempt timeout, ...). A positive timeout marks the
// per-attempt deadline expiration: retrying cannot help, and the
// error points at the configurable knob.
type transportError struct {
	cause   error
	timeout time.Duration // the deadline that expired; 0 = other transport failure
}

func (e *transportError) Error() string {
	msg := "llm request failed: " + e.cause.Error()
	if e.timeout > 0 {
		msg += fmt.Sprintf("（单次尝试超过 %s 超时上限，已放弃重试：调大 --timeout 或设置 PROMPTOPT_TIMEOUT）", e.timeout)
	}
	return msg
}

func (e *transportError) Unwrap() error { return e.cause }

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
		return max(time.Until(t), 0)
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

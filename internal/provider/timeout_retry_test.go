package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// TestChatDoesNotRetryPerAttemptTimeout pins the tcmsp-30 大基数实测
// fix: a per-attempt deadline expiration used to burn the full retry
// budget (4×180s=723.9s all wasted). It must surface after ONE attempt,
// with no backoff waits, and the error must point at the knob.
func TestChatDoesNotRetryPerAttemptTimeout(t *testing.T) {
	var trips atomic.Int32
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		trips.Add(1)
		return nil, fmt.Errorf("Post %q: %w", r.URL, context.DeadlineExceeded)
	})
	c, waits := newTestClient(t, "http://unused.local", OpenAIConfig{
		HTTPClient: &http.Client{Transport: rt},
	})
	_, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err == nil {
		t.Fatal("want the timeout error")
	}
	if trips.Load() != 1 {
		t.Errorf("trips = %d, want 1 (a timeout hits the same deadline on every retry)", trips.Load())
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none (no retry after a timeout)", *waits)
	}
	for _, hint := range []string{"--timeout", "PROMPTOPT_TIMEOUT"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("error %q does not mention %s", err, hint)
		}
	}
}

// TestChatPerAttemptTimeoutEndToEnd proves the classification on a real
// HTTP round trip: a slow server against a short per-attempt deadline
// fails once and fast, instead of retrying into the same wall.
func TestChatPerAttemptTimeoutEndToEnd(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, completionBody("", "ok", 1, 1))
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv.URL, OpenAIConfig{Timeout: 50 * time.Millisecond})
	start := time.Now()
	_, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want the timeout error")
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1", hits.Load())
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none", *waits)
	}
	if elapsed > time.Second {
		t.Errorf("timeout path took %s — the retry loop is re-hitting the deadline", elapsed)
	}
}

// TestAnthropicDoesNotRetryPerAttemptTimeout: the native Messages
// client classifies identically.
func TestAnthropicDoesNotRetryPerAttemptTimeout(t *testing.T) {
	var trips atomic.Int32
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		trips.Add(1)
		return nil, fmt.Errorf("Post %q: %w", r.URL, context.DeadlineExceeded)
	})
	a := NewAnthropic("http://unused.local", "k", AnthropicConfig{
		HTTPClient: &http.Client{Transport: rt},
	})
	waits := &[]time.Duration{}
	a.sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	_, err := a.Chat(context.Background(), ChatRequest{
		Role: core.RoleExecutor, MaxTokens: 16,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want the timeout error")
	}
	if trips.Load() != 1 {
		t.Errorf("trips = %d, want 1", trips.Load())
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none", *waits)
	}
	if !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("error %q does not point at --timeout", err)
	}
}

// TestRetryDelayClassifiesTimeout pins the policy seam directly: a
// timeout-carrying transportError is terminal, a plain one retries with
// exponential backoff.
func TestRetryDelayClassifiesTimeout(t *testing.T) {
	if wait, retry := retryDelay(&transportError{cause: io.ErrUnexpectedEOF, timeout: 180 * time.Second}, 0, time.Second); retry {
		t.Errorf("timeout transportError: wait %v retry %v, want no retry", wait, retry)
	}
	wait, retry := retryDelay(&transportError{cause: io.ErrUnexpectedEOF}, 0, time.Second)
	if !retry || wait != time.Second {
		t.Errorf("plain transportError = (%v, %v), want (1s, true)", wait, retry)
	}
}

// TestSystemOneTimeoutHintsTheKnob: the decision client has no retry
// policy by design, but its timeout error still tells the operator
// which knob to turn.
func TestSystemOneTimeoutHintsTheKnob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c := NewSystemOne(srv.URL, "tev1", SystemOneConfig{Timeout: 50 * time.Millisecond})
	_, err := c.Do(context.Background(), SystemOneRequest{Model: "tev1"})
	if err == nil {
		t.Fatal("want the timeout error")
	}
	for _, hint := range []string{"--timeout", "PROMPTOPT_TIMEOUT"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("error %q does not mention %s", err, hint)
		}
	}
}

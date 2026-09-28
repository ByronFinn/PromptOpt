package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// newTestClient returns a client whose backoff sleeps are recorded
// instead of really slept.
func newTestClient(t *testing.T, baseURL string, cfg OpenAIConfig) (*Client, *[]time.Duration) {
	t.Helper()
	c := NewOpenAI(baseURL, "test-key", cfg)
	waits := &[]time.Duration{}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return c, waits
}

// completionBody builds an OpenAI-style response payload.
func completionBody(reasoning, content string, promptTok, completionTok int64) string {
	return fmt.Sprintf(`{
		"choices": [{
			"finish_reason": "stop",
			"message": {"role": "assistant", "reasoning_content": %q, "content": %q}
		}],
		"usage": {"prompt_tokens": %d, "completion_tokens": %d, "total_tokens": %d}
	}`, reasoning, content, promptTok, completionTok, promptTok+completionTok)
}

func TestChatParsesReasoningAndMetersUsage(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completionBody("think...", "{\"ok\":1}", 11, 7))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, OpenAIConfig{})
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:     "jiuwei-tcm",
		Messages:  []Message{{Role: "user", Content: "hello"}},
		MaxTokens: 2048,
		Role:      core.RoleExecutor,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "{\"ok\":1}" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.ReasoningContent != "think..." {
		t.Errorf("reasoning_content = %q", resp.ReasoningContent)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q", gotAuth)
	}
	var sent ChatRequest
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if sent.Model != "jiuwei-tcm" || sent.MaxTokens != 2048 || len(sent.Messages) != 1 {
		t.Errorf("request = %+v", sent)
	}
	// Role drives metering only and must not leak into the wire payload.
	if strings.Contains(gotBody, "executor") {
		t.Errorf("role leaked into request body: %s", gotBody)
	}
	u := c.UsageSnapshot()[core.RoleExecutor]
	if u.PromptTokens != 11 || u.CompletionTokens != 7 {
		t.Errorf("meter snapshot = %+v", u)
	}
}

func TestChatRetries429HonoringRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, completionBody("", "ok", 1, 1))
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv.URL, OpenAIConfig{})
	resp, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
	if len(*waits) != 1 || (*waits)[0] != 7*time.Second {
		t.Errorf("waits = %v, want [7s]", *waits)
	}
}

func TestChatRetries5xxWithExponentialBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, completionBody("", "ok", 1, 1))
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv.URL, OpenAIConfig{Backoff: 10 * time.Millisecond})
	if _, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	if len(*waits) != 2 || (*waits)[0] != want[0] || (*waits)[1] != want[1] {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
}

func TestChatGivesUpAfterMaxAttempts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, OpenAIConfig{MaxAttempts: 3})
	_, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err == nil {
		t.Fatal("want error after exhausted retries")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should mention status: %v", err)
	}
	if hits.Load() != 3 {
		t.Errorf("hits = %d, want 3", hits.Load())
	}
}

func TestChatDoesNotRetry4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"error": "invalid_request_error: bad model"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	c, waits := newTestClient(t, srv.URL, OpenAIConfig{})
	_, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err == nil {
		t.Fatal("want error on 400")
	}
	if !strings.Contains(err.Error(), "invalid_request_error") {
		t.Errorf("error should contain body snippet: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d, want 1 (no retry)", hits.Load())
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none", *waits)
	}
}

func TestChatRetriesTransportErrors(t *testing.T) {
	var trips atomic.Int32
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if trips.Add(1) <= 2 {
			return nil, io.ErrUnexpectedEOF
		}
		body := io.NopCloser(strings.NewReader(completionBody("", "ok", 1, 1)))
		return &http.Response{StatusCode: 200, Body: body, Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	})
	c, waits := newTestClient(t, "http://unused.local", OpenAIConfig{
		HTTPClient: &http.Client{Transport: rt}, Backoff: time.Millisecond,
	})
	resp, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	if trips.Load() != 3 || len(*waits) != 2 {
		t.Errorf("trips = %d, waits = %v", trips.Load(), *waits)
	}
}

func TestChatAbortsOnCanceledContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := newTestClient(t, srv.URL, OpenAIConfig{})
	_, err := c.Chat(ctx, ChatRequest{Role: core.RoleExecutor})
	if err == nil {
		t.Fatal("want error on canceled context")
	}
	// The canceled context aborts locally: the server is never reached
	// and no retry is attempted.
	if hits.Load() != 0 {
		t.Errorf("hits = %d, want 0 (canceled before send)", hits.Load())
	}
}

func TestChatRejectsEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices": [], "usage": {"prompt_tokens": 1}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, OpenAIConfig{})
	if _, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor}); err == nil {
		t.Fatal("want error on empty choices")
	}
}

func TestMeterIsConcurrencySafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, completionBody("", "ok", 10, 5))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, OpenAIConfig{})
	var wg sync.WaitGroup
	for range 25 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if _, err := c.Chat(context.Background(), ChatRequest{Role: core.RoleExecutor}); err != nil {
					t.Errorf("Chat: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	u := c.UsageSnapshot()[core.RoleExecutor]
	if u.PromptTokens != 1000 || u.CompletionTokens != 500 {
		t.Errorf("meter = %+v, want 1000/500", u)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"3", 3 * time.Second},
		{"0", 0},
		{"", 0},
		{"junk", 0},
		{"-5", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// HTTP-date form: a future date yields a positive delay.
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 90*time.Second {
		t.Errorf("parseRetryAfter(future date) = %v, want in (0, 90s]", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

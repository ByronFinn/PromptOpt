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

// newAnthropicTestClient returns a client whose backoff sleeps are
// recorded instead of really slept.
func newAnthropicTestClient(t *testing.T, baseURL string, cfg AnthropicConfig) (*Anthropic, *[]time.Duration) {
	t.Helper()
	a := NewAnthropic(baseURL, "test-key", cfg)
	waits := &[]time.Duration{}
	a.sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return a, waits
}

// anthropicBody builds a Messages-style response payload.
func anthropicBody(text string, inputTok, outputTok int64) string {
	return fmt.Sprintf(`{
		"content": [{"type": "text", "text": %q}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": %d, "output_tokens": %d}
	}`, text, inputTok, outputTok)
}

func TestAnthropicChatSendsNativeHeadersAndWireFormat(t *testing.T) {
	var gotPath, gotAPIKey, gotVersion, gotAuth, gotCT, gotRaw string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotRaw = string(b)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, anthropicBody("{\"ok\":1}", 11, 7))
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	resp, err := a.Chat(context.Background(), ChatRequest{
		Model:     "claude-sonnet-4",
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
	if resp.ReasoningContent != "" {
		t.Errorf("reasoning_content = %q, want empty", resp.ReasoningContent)
	}
	if resp.FinishReason != "end_turn" {
		t.Errorf("finish_reason = %q", resp.FinishReason)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", gotPath)
	}
	if gotAPIKey != "test-key" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotVersion != AnthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, AnthropicVersion)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want none (native auth is x-api-key)", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotBody["model"] != "claude-sonnet-4" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(2048) {
		t.Errorf("max_tokens = %v", gotBody["max_tokens"])
	}
	if _, ok := gotBody["temperature"]; ok {
		t.Errorf("temperature leaked for zero value: %v", gotBody["temperature"])
	}
	if _, ok := gotBody["system"]; ok {
		t.Errorf("system leaked for empty preamble: %v", gotBody["system"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %v", gotBody["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello" {
		t.Errorf("messages[0] = %v", first)
	}
	// Role drives metering only and must not leak into the wire payload.
	if strings.Contains(gotRaw, "executor") {
		t.Errorf("role leaked into request body: %s", gotRaw)
	}
	u := a.UsageSnapshot()[core.RoleExecutor]
	if u.PromptTokens != 11 || u.CompletionTokens != 7 {
		t.Errorf("meter snapshot = %+v", u)
	}
}

func TestAnthropicChatSplitsSystemFromConversation(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Errorf("request body not JSON: %v", err)
		}
		fmt.Fprint(w, anthropicBody("ok", 1, 1))
	}))
	defer srv.Close()

	// Trailing slash in baseURL must not corrupt the endpoint path.
	a, _ := newAnthropicTestClient(t, srv.URL+"/", AnthropicConfig{})
	_, err := a.Chat(context.Background(), ChatRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 64,
		Messages: []Message{
			{Role: "system", Content: "You are terse."},
			{Role: "system", Content: "Answer in JSON."},
			{Role: "user", Content: "hi"},
			{Role: "user", Content: "again"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "bye"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotBody["system"] != "You are terse.\n\nAnswer in JSON." {
		t.Errorf("system = %v", gotBody["system"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want 3 merged turns", gotBody["messages"])
	}
	want := []struct{ role, content string }{
		{"user", "hi\n\nagain"},
		{"assistant", "hello"},
		{"user", "bye"},
	}
	for i, w := range want {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != w.role || m["content"] != w.content {
			t.Errorf("messages[%d] = %v, want %s/%s", i, m, w.role, w.content)
		}
	}
}

func TestAnthropicChatSeparatesThinkingFromContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"content": [
				{"type": "thinking", "thinking": "ponder..."},
				{"type": "text", "text": "answer"}
			],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 5, "output_tokens": 9}
		}`)
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	resp, err := a.Chat(context.Background(), ChatRequest{
		Model:     "claude-sonnet-4",
		Messages:  []Message{{Role: "user", Content: "hello"}},
		MaxTokens: 2048,
		Role:      core.RoleExecutor,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "answer" {
		t.Errorf("content = %q, want scored text only", resp.Content)
	}
	if resp.ReasoningContent != "ponder..." {
		t.Errorf("reasoning_content = %q", resp.ReasoningContent)
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 9 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAnthropicChatRejectsInvalidRequests(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, anthropicBody("ok", 1, 1))
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	cases := []struct {
		name string
		req  ChatRequest
		want string
	}{
		{
			"zero max tokens",
			ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}},
			"max_tokens",
		},
		{
			"only system messages",
			ChatRequest{MaxTokens: 64, Messages: []Message{{Role: "system", Content: "x"}}},
			"non-system",
		},
		{
			"unsupported role",
			ChatRequest{MaxTokens: 64, Messages: []Message{{Role: "tool", Content: "x"}}},
			"unsupported role",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Chat(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
	// Validation fails before any HTTP traffic and is not retried.
	if hits.Load() != 0 {
		t.Errorf("hits = %d, want 0 (rejected client-side)", hits.Load())
	}
}

func TestAnthropicChatRetries429HonoringRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, anthropicBody("ok", 1, 1))
	}))
	defer srv.Close()

	a, waits := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	resp, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
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

func TestAnthropicChatRetries5xxWithExponentialBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, anthropicBody("ok", 1, 1))
	}))
	defer srv.Close()

	a, waits := newAnthropicTestClient(t, srv.URL, AnthropicConfig{Backoff: 10 * time.Millisecond})
	_, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
	if err != nil {
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

func TestAnthropicChatGivesUpAfterMaxAttempts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{MaxAttempts: 3})
	_, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
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

func TestAnthropicChatDoesNotRetry4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: required"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	a, waits := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	_, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
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

func TestAnthropicChatRetriesTransportErrors(t *testing.T) {
	var trips atomic.Int32
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if trips.Add(1) <= 2 {
			return nil, io.ErrUnexpectedEOF
		}
		body := io.NopCloser(strings.NewReader(anthropicBody("ok", 1, 1)))
		return &http.Response{StatusCode: 200, Body: body, Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	})
	a, waits := newAnthropicTestClient(t, "http://unused.local", AnthropicConfig{
		HTTPClient: &http.Client{Transport: rt}, Backoff: time.Millisecond,
	})
	resp, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
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

func TestAnthropicChatAbortsOnCanceledContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	_, err := a.Chat(ctx, ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
	if err == nil {
		t.Fatal("want error on canceled context")
	}
	// The canceled context aborts locally: the server is never reached
	// and no retry is attempted.
	if hits.Load() != 0 {
		t.Errorf("hits = %d, want 0 (canceled before send)", hits.Load())
	}
}

func TestAnthropicChatRejectsEmptyTextContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content": [{"type": "thinking", "thinking": "only thoughts"}], "stop_reason": "end_turn", "usage": {"input_tokens": 1, "output_tokens": 1}}`)
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	_, err := a.Chat(context.Background(), ChatRequest{
		Messages:  []Message{{Role: "user", Content: "x"}},
		MaxTokens: 64,
		Role:      core.RoleExecutor,
	})
	if err == nil || !strings.Contains(err.Error(), "no text content") {
		t.Fatalf("err = %v, want no-text-content error", err)
	}
}

func TestAnthropicMeterIsConcurrencySafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, anthropicBody("ok", 10, 5))
	}))
	defer srv.Close()

	a, _ := newAnthropicTestClient(t, srv.URL, AnthropicConfig{})
	var wg sync.WaitGroup
	for range 25 {
		wg.Go(func() {
			for range 4 {
				if _, err := a.Chat(context.Background(), ChatRequest{
					Messages:  []Message{{Role: "user", Content: "x"}},
					MaxTokens: 64,
					Role:      core.RoleExecutor,
				}); err != nil {
					t.Errorf("Chat: %v", err)
				}
			}
		})
	}
	wg.Wait()
	u := a.UsageSnapshot()[core.RoleExecutor]
	if u.PromptTokens != 1000 || u.CompletionTokens != 500 {
		t.Errorf("meter = %+v, want 1000/500", u)
	}
}

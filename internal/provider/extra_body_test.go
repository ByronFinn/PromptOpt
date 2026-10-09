package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// TestMergeExtraBody pins the merge contract of the --extra-body
// surface: user keys win over the struct fields, non-colliding keys
// pass through alongside them, and an empty map leaves the payload
// bytes untouched (no re-marshal, no key reordering).
func TestMergeExtraBody(t *testing.T) {
	base := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	// No collision: extras land at the top level next to the fields.
	got, err := mergeExtraBody(base, map[string]any{
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
		"top_k":                1,
	})
	if err != nil {
		t.Fatalf("mergeExtraBody: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(got, &top); err != nil {
		t.Fatalf("merged payload not JSON: %v", err)
	}
	if top["model"] != "m" || top["max_tokens"] != float64(64) {
		t.Errorf("base fields lost: %s", got)
	}
	ktw, ok := top["chat_template_kwargs"].(map[string]any)
	if !ok || ktw["enable_thinking"] != false {
		t.Errorf("chat_template_kwargs = %v, want {enable_thinking: false}", top["chat_template_kwargs"])
	}
	if top["top_k"] != float64(1) {
		t.Errorf("top_k = %v, want 1", top["top_k"])
	}

	// Collision: the user's explicit value overrides the struct field —
	// --extra-body is the user's stated intent.
	got, err = mergeExtraBody(base, map[string]any{"max_tokens": 16})
	if err != nil {
		t.Fatalf("mergeExtraBody: %v", err)
	}
	top = nil
	if err := json.Unmarshal(got, &top); err != nil {
		t.Fatalf("merged payload not JSON: %v", err)
	}
	if top["max_tokens"] != float64(16) {
		t.Errorf("max_tokens = %v, want the extra body's 16 (user keys win)", top["max_tokens"])
	}

	// Empty map: payload passes through byte-identical.
	if got, err = mergeExtraBody(base, nil); err != nil || string(got) != string(base) {
		t.Errorf("empty extra = %q, %v; want the payload untouched", got, err)
	}
	if got, err = mergeExtraBody(base, map[string]any{}); err != nil || string(got) != string(base) {
		t.Errorf("nil-value empty map = %q, %v; want the payload untouched", got, err)
	}
}

// TestChatExtraBodyReachesTheWire drives the OpenAI client end to end
// and asserts the merged extras appear at the wire payload's top level
// (the disable-thinking shape from the docs example).
func TestChatExtraBodyReachesTheWire(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, completionBody("", "ok", 1, 1))
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv.URL, OpenAIConfig{})
	req := ChatRequest{
		Model:     "jiuwei-tcm",
		MaxTokens: 64,
		Role:      core.RoleExecutor,
		Messages:  []Message{{Role: "user", Content: "hello"}},
		ExtraBody: map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
		},
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(gotBody), &top); err != nil {
		t.Fatalf("wire body not JSON: %v", gotBody)
	}
	ktw, ok := top["chat_template_kwargs"].(map[string]any)
	if !ok || ktw["enable_thinking"] != false {
		t.Errorf("wire top level lacks chat_template_kwargs.enable_thinking: %s", gotBody)
	}
	if top["model"] != "jiuwei-tcm" || top["max_tokens"] != float64(64) {
		t.Errorf("base fields lost on the wire: %s", gotBody)
	}
	// The struct's own serialization never carries the extras (they are
	// json:"-"): only the attempt-time merge puts them on the wire.
	naked, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if strings.Contains(string(naked), "extra_body") || strings.Contains(string(naked), "chat_template_kwargs") {
		t.Errorf("ChatRequest serialization leaked extras: %s", naked)
	}
}

// TestAnthropicExtraBodyReachesTheWire pins the /v1/messages side: the
// extras merge onto the payload top level too, passed through without
// gateway-private field-name validation (A3).
func TestAnthropicExtraBodyReachesTheWire(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	a := NewAnthropic(srv.URL, "k", AnthropicConfig{})
	req := ChatRequest{
		Model:     "claude",
		MaxTokens: 64,
		Role:      core.RoleExecutor,
		Messages:  []Message{{Role: "user", Content: "hello"}},
		ExtraBody: map[string]any{
			"chat_template_kwargs": map[string]any{"enable_thinking": false},
			"gateway_private_flag": true,
		},
	}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(gotBody), &top); err != nil {
		t.Fatalf("wire body not JSON: %v", gotBody)
	}
	if top["model"] != "claude" || top["max_tokens"] != float64(64) {
		t.Errorf("messages fields lost: %s", gotBody)
	}
	ktw, ok := top["chat_template_kwargs"].(map[string]any)
	if !ok || ktw["enable_thinking"] != false {
		t.Errorf("wire top level lacks chat_template_kwargs: %s", gotBody)
	}
	if top["gateway_private_flag"] != true {
		t.Errorf("gateway_private_flag = %v, want passthrough", top["gateway_private_flag"])
	}
}

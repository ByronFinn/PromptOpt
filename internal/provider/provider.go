// Package provider abstracts LLM chat-completions backends and meters
// their token usage per pipeline role.
package provider

import (
	"context"
	"maps"
	"sync"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// Message is one chat message exchanged with the model.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest asks a model to complete a conversation. Role drives
// usage metering only and is never serialized to the wire format.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature,omitempty"`
	Role        core.Role `json:"-"`
}

// ChatResponse is the distilled completion of one request.
type ChatResponse struct {
	Content          string     `json:"content"`                     // final answer text
	ReasoningContent string     `json:"reasoning_content,omitempty"` // reasoning models emit this before content
	FinishReason     string     `json:"finish_reason,omitempty"`
	Usage            core.Usage `json:"usage"`
}

// Provider abstracts a chat-completions backend.
type Provider interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Meter accumulates token usage per role; safe for concurrent use.
type Meter struct {
	mu     sync.Mutex
	byRole map[core.Role]core.Usage
}

// NewMeter returns an empty meter.
func NewMeter() *Meter {
	return &Meter{byRole: make(map[core.Role]core.Usage)}
}

// Record adds one call's usage to its role.
func (m *Meter) Record(role core.Role, u core.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.byRole[role]
	cur.PromptTokens += u.PromptTokens
	cur.CompletionTokens += u.CompletionTokens
	m.byRole[role] = cur
}

// Snapshot returns a copy of the per-role totals.
func (m *Meter) Snapshot() map[core.Role]core.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[core.Role]core.Usage, len(m.byRole))
	maps.Copy(out, m.byRole)
	return out
}

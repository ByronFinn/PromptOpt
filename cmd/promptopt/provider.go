package main

import (
	"cmp"
	"fmt"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// newProvider builds the LLM backend named by --provider. The two
// clients keep separate wire formats: OpenAI-compatible chat
// completions and the native Anthropic /v1/messages endpoint. rps is
// the optional client-side pacing (--rps; <= 0 disables it) — each
// built client carries its own limiter, so the executor and a
// dedicated judge instance pace independently against the same
// gateway. timeout is the per-attempt deadline (--timeout; 0 = the
// constructors' 180s default) and threads through both backends the
// same way.
func newProvider(name, baseURL, apiKey string, rps float64, timeout time.Duration) (provider.Provider, error) {
	switch name {
	case "openai", "":
		return provider.NewOpenAI(baseURL, apiKey, provider.OpenAIConfig{MaxRPS: rps, Timeout: timeout}), nil
	case "anthropic":
		return provider.NewAnthropic(baseURL, apiKey, provider.AnthropicConfig{MaxRPS: rps, Timeout: timeout}), nil
	default:
		return nil, fmt.Errorf("unknown --provider %q (available: openai, anthropic)", name)
	}
}

// judgeConn carries the executor-side connection (already resolved
// through its own flag > env chain) and the judge-side fields into the
// single second-instance factory shared by run and verify.
type judgeConn struct {
	providerName, baseURL, apiKey string // executor side, resolved
	rps                           float64
	timeout                       time.Duration
	// judge side, resolved ("" = reuse the executor's value)
	judgeProvider, judgeBaseURL, judgeAPIKey string
}

// newJudgeProvider builds the optional second provider backing the
// llm_judge metric through the same factory as the executor provider.
// Construction rule (review-widened): any judge connection field
// constructs the instance — base URL and API key bind inside the
// Provider (newProvider's parameters; the interface never exposes the
// address), so keying off --judge-provider alone would silently drop a
// lone --judge-base-url / --judge-api-key. Fields fall back
// individually to the executor's; with no judge connection field set
// the executor client is shared (nil, nil) and the engine-level
// per-field fallbacks decide. The executor's rps and timeout carry
// over so the dedicated judge instance paces and times out the same
// way.
func newJudgeProvider(c judgeConn) (provider.Provider, error) {
	if c.judgeProvider == "" && c.judgeBaseURL == "" && c.judgeAPIKey == "" {
		return nil, nil
	}
	return newProvider(
		cmp.Or(c.judgeProvider, c.providerName),
		cmp.Or(c.judgeBaseURL, c.baseURL),
		cmp.Or(c.judgeAPIKey, c.apiKey),
		c.rps,
		c.timeout,
	)
}

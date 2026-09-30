package main

import (
	"fmt"

	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// newProvider builds the LLM backend named by --provider. The two
// clients keep separate wire formats: OpenAI-compatible chat
// completions and the native Anthropic /v1/messages endpoint.
func newProvider(name, baseURL, apiKey string) (provider.Provider, error) {
	switch name {
	case "openai", "":
		return provider.NewOpenAI(baseURL, apiKey, provider.OpenAIConfig{}), nil
	case "anthropic":
		return provider.NewAnthropic(baseURL, apiKey, provider.AnthropicConfig{}), nil
	default:
		return nil, fmt.Errorf("unknown --provider %q (available: openai, anthropic)", name)
	}
}

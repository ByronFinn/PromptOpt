package main

import (
	"errors"
	"fmt"

	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// newProvider builds the LLM backend named by --provider. The
// anthropic branch stays an explicit refusal until the Anthropic
// implementation merges: silently falling back to the OpenAI client
// would dial the wrong wire format.
func newProvider(name, baseURL, apiKey string) (provider.Provider, error) {
	switch name {
	case "openai", "":
		return provider.NewOpenAI(baseURL, apiKey, provider.OpenAIConfig{}), nil
	case "anthropic":
		// TODO(V5 anthropic track): replace with
		// provider.NewAnthropic(baseURL, apiKey, provider.AnthropicConfig{})
		// once internal/provider/anthropic.go lands.
		return nil, errors.New("--provider anthropic 尚未合入：Anthropic 后端落地前不可用（当前可用：openai）")
	default:
		return nil, fmt.Errorf("unknown --provider %q (available: openai)", name)
	}
}

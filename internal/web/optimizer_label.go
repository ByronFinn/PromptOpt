package web

import (
	"cmp"

	"github.com/ByronFinn/PromptOpt/internal/optimizers/builtin"
)

// optimizerLabel resolves a paradigm name to its registry label: the
// builtin descriptors carry the Chinese display names. An unregistered
// name — a run produced before the paradigm merged, or by a registry
// this binary did not compile in — falls back to the raw name, so the
// compare page renders any paradigm with zero web-side changes
// (docs/plugins.md).
func optimizerLabel(name string) string {
	if d, ok := builtin.Registry().Get(name); ok {
		return cmp.Or(d.Label, name)
	}
	return name
}

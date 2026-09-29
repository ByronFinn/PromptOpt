// Package config resolves CLI flag values with environment fallbacks
// and centralizes flag defaults.
package config

import (
	"cmp"
	"os"
)

// Environment variables consulted when the matching flag is unset.
// Workers and addr are flag-only by design and have no env fallback.
const (
	EnvBaseURL = "PROMPTOPT_BASE_URL"
	EnvModel   = "PROMPTOPT_MODEL"
	EnvAPIKey  = "PROMPTOPT_API_KEY"
	EnvOutDir  = "PROMPTOPT_OUT"
)

// Flag defaults shared across commands.
const (
	DefaultAPIKey  = "1"               // local gateway convention
	DefaultOutDir  = "runs"            // run artifacts land here
	DefaultWorkers = 4                 // parallel evaluation workers
	DefaultAddr    = "127.0.0.1:17700" // web dashboard listen address
	// DefaultMaxTokens: reasoning models (jiuwei-tcm) spend thousands
	// of completion tokens on reasoning before writing content — a
	// single extraction call was observed consuming 7.5k tokens
	// (tcmsp-30 e2e, 2026-09-29). 2048 flakily truncates them.
	DefaultMaxTokens = 8192

	DefaultSamples        = 6    // synthesized samples in zero-config mode
	DefaultProbeVariants  = 2    // probe prompt variants for the p¹ filter
	// DefaultSynthMaxTokens: one synthesis call returns a task spec plus
	// N samples as JSON — reasoning models spend most of the budget
	// thinking first. jiuwei-tcm serves n_ctx=20736, so 8192 completion
	// tokens leaves ample headroom and stops content-empty responses.
	DefaultSynthMaxTokens = 8192 // completion floor for synthesis calls
)

// resolve returns the first non-empty of flag value, environment
// variable and fallback.
func resolve(flagVal, envKey, fallback string) string {
	return cmp.Or(flagVal, os.Getenv(envKey), fallback)
}

// BaseURL resolves the OpenAI-compatible API base URL. It has no
// default: the run command must reject an empty value.
func BaseURL(flagVal string) string {
	return resolve(flagVal, EnvBaseURL, "")
}

// Model resolves the model name. It has no default.
func Model(flagVal string) string {
	return resolve(flagVal, EnvModel, "")
}

// APIKey resolves the API key, defaulting to "1" for local gateways.
func APIKey(flagVal string) string {
	return resolve(flagVal, EnvAPIKey, DefaultAPIKey)
}

// OutDir resolves the run output directory.
func OutDir(flagVal string) string {
	return resolve(flagVal, EnvOutDir, DefaultOutDir)
}

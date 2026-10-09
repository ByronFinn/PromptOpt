// Package config resolves CLI flag values with environment fallbacks
// and centralizes flag defaults.
package config

import (
	"cmp"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables consulted when the matching flag is unset.
// Workers and addr are flag-only by design and have no env fallback.
// The judge variables mirror the executor ones for the optional second
// judge LLM (V7 裁判隔离): same precedence, per-field fallback to the
// executor values when unset.
const (
	EnvBaseURL = "PROMPTOPT_BASE_URL"
	EnvModel   = "PROMPTOPT_MODEL"
	EnvAPIKey  = "PROMPTOPT_API_KEY"
	EnvOutDir  = "PROMPTOPT_OUT"
	EnvTimeout = "PROMPTOPT_TIMEOUT"

	EnvJudgeProvider  = "PROMPTOPT_JUDGE_PROVIDER"
	EnvJudgeModel     = "PROMPTOPT_JUDGE_MODEL"
	EnvJudgeBaseURL   = "PROMPTOPT_JUDGE_BASE_URL"
	EnvJudgeAPIKey    = "PROMPTOPT_JUDGE_API_KEY"
	EnvJudgeMaxTokens = "PROMPTOPT_JUDGE_MAX_TOKENS"

	// Decision-model judge surface (P7): only the connection fields
	// carry an env fallback — the backend selector and the cascade
	// thresholds are flag-only (thresholds default inside eval, the
	// backend is an explicit opt-in).
	EnvJudgeDecisionURL   = "PROMPTOPT_JUDGE_DECISION_URL"
	EnvJudgeDecisionModel = "PROMPTOPT_JUDGE_DECISION_MODEL"
)

// Flag defaults shared across commands.
const (
	DefaultAPIKey  = "1"    // local gateway convention
	DefaultOutDir  = "runs" // run artifacts land here
	DefaultWorkers = 4      // parallel evaluation workers
	// DefaultPort is the dashboard port half of DefaultAddr (P10 分立
	// 参数 --port): a host-only --addr combines with it and --port
	// alone binds loopback at it.
	DefaultPort = 17700
	DefaultAddr = "127.0.0.1:17700" // web dashboard listen address (loopback:DefaultPort — keep in sync)
	// DefaultMaxTokens: reasoning models (jiuwei-tcm) spend thousands
	// of completion tokens on reasoning before writing content — a
	// single extraction call was observed consuming 7.5k tokens
	// (tcmsp-30 e2e, 2026-09-29). 2048 flakily truncates them.
	DefaultMaxTokens = 8192

	DefaultSamples       = 6 // synthesized samples in zero-config mode
	DefaultProbeVariants = 2 // probe prompt variants for the p¹ filter
	// DefaultSynthMaxTokens: one synthesis call returns a task spec plus
	// N samples as JSON — reasoning models spend most of the budget
	// thinking first. jiuwei-tcm serves n_ctx=20736, so 8192 completion
	// tokens leaves ample headroom and stops content-empty responses.
	DefaultSynthMaxTokens = 8192 // completion floor for synthesis calls

	// GEPA engine defaults (V3).
	DefaultMaxRounds       = 5   // optimization rounds
	DefaultMinibatch       = 4   // samples drawn per round
	DefaultEpsilon         = 0.2 // exploration rate of hypothesis selection
	DefaultStagnationLimit = 3   // stagnant rounds before a Fresh restart

	// Multi-paradigm surface defaults (V5).
	DefaultOptimizer  = "gepa"   // --optimizer default (see internal/optimizers/builtin)
	DefaultProvider   = "openai" // --provider default
	DefaultEvoVariant = "ga"     // --evo-variant default (evoprompt paradigm)
	// DefaultOptMaxTokens: optimizer-side calls (reflection, mutation)
	// return long JSON after heavy reasoning — give them the same
	// generous completion floor as synthesis; no 4096-level clamping.
	DefaultOptMaxTokens = 8192

	// DefaultTimeout is the per-attempt provider deadline (every
	// backend client: OpenAI-compatible, Anthropic and the SystemOne
	// decision judge). Reasoning models are slow; a lower value must
	// come from --timeout / PROMPTOPT_TIMEOUT. The provider
	// constructors carry the same fallback so a zero config still
	// resolves to this.
	DefaultTimeout = 180 * time.Second
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

// JudgeProvider resolves the judge provider backend name; empty means
// the judge reuses the executor provider (the engine-level fallback).
func JudgeProvider(flagVal string) string {
	return resolve(flagVal, EnvJudgeProvider, "")
}

// JudgeModel resolves the judge model name; empty falls back to the
// executor model inside eval.Engine.
func JudgeModel(flagVal string) string {
	return resolve(flagVal, EnvJudgeModel, "")
}

// JudgeBaseURL resolves the judge API base URL; the call sites fall
// back to the executor base URL when it stays empty (no default: the
// judge shares whatever endpoint the executor resolves).
func JudgeBaseURL(flagVal string) string {
	return resolve(flagVal, EnvJudgeBaseURL, "")
}

// JudgeAPIKey resolves the judge API key without a default: the call
// sites fall back to the executor key chain (flag > PROMPTOPT_API_KEY
// > "1"), so the judge never outlives the executor's credentials.
func JudgeAPIKey(flagVal string) string {
	return resolve(flagVal, EnvJudgeAPIKey, "")
}

// JudgeMaxTokens resolves the judge completion budget; 0 = unset (the
// engine falls back to the executor budget). A malformed or
// non-positive env value reads as unset: the judge surface is optional
// by construction and must not fail runs it was never asked for.
func JudgeMaxTokens(flagVal int) int {
	if flagVal > 0 {
		return flagVal
	}
	if n, err := strconv.Atoi(os.Getenv(EnvJudgeMaxTokens)); err == nil && n > 0 {
		return n
	}
	return 0
}

// JudgeDecisionURL resolves the decision-model service base URL (P7);
// empty means no decision surface — --judge-backend decision then
// fails as a usage error instead of guessing an endpoint.
func JudgeDecisionURL(flagVal string) string {
	return resolve(flagVal, EnvJudgeDecisionURL, "")
}

// JudgeDecisionModel resolves the decision model name (P7); the
// /v1/systemone request body carries it, so the decision backend
// requires it (empty = usage error under --judge-backend decision).
func JudgeDecisionModel(flagVal string) string {
	return resolve(flagVal, EnvJudgeDecisionModel, "")
}

// ParseTimeout reads one per-attempt timeout value: Go duration syntax
// ("90s", "2m30s") or a bare number in seconds ("300"). Empty reads as
// 0 (unset); a malformed or non-positive value is a usage error — the
// flag has a real default (DefaultTimeout) and must fail loudly
// instead of silently second-guessing the operator.
func ParseTimeout(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if secs, err := strconv.Atoi(s); err == nil {
		if secs <= 0 {
			return 0, fmt.Errorf("timeout must be positive, got %q seconds", s)
		}
		return time.Duration(secs) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q (use Go duration syntax like 90s / 2m30s, or bare seconds like 300)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout must be positive, got %q", s)
	}
	return d, nil
}

// Timeout resolves the per-attempt provider deadline: flag > env
// (PROMPTOPT_TIMEOUT) > 0 (the caller's "unset", resolved to
// DefaultTimeout inside the provider constructors). A malformed env
// value reads as unset, mirroring JudgeMaxTokens: the surface is
// optional and must not fail runs that never asked for it — the flag
// side (ParseTimeout) is the strict one.
func Timeout(flagVal time.Duration) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	if d, err := ParseTimeout(os.Getenv(EnvTimeout)); err == nil {
		return d
	}
	return 0
}

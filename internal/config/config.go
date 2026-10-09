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

// resolve is the merge kernel for the string keys whose flag default is
// the empty string: flag > env > 文件 > 默认 (PRD-0001 D2 — the config
// package owns the chain order; cmd layers never splice their own). A
// zero fileVal degrades to the old three-tier semantics exactly
// (TestResolverFileParity pins the per-field equality).
func resolve(flagVal, envKey, fileVal, fallback string) string {
	return cmp.Or(flagVal, os.Getenv(envKey), fileVal, fallback)
}

// BaseURL resolves the OpenAI-compatible API base URL. It has no
// default: the run command must reject an empty value.
func BaseURL(flagVal, fileVal string) string {
	return resolve(flagVal, EnvBaseURL, fileVal, "")
}

// Model resolves the model name. It has no default.
func Model(flagVal, fileVal string) string {
	return resolve(flagVal, EnvModel, fileVal, "")
}

// APIKey resolves the API key, defaulting to "1" for local gateways.
// The chain is flag > env > file > "1" — the key never persists in the
// manifest, so the file tier is its only persistence.
func APIKey(flagVal, fileVal string) string {
	return resolve(flagVal, EnvAPIKey, fileVal, DefaultAPIKey)
}

// OutDir resolves the run output directory.
func OutDir(flagVal, fileVal string) string {
	return resolve(flagVal, EnvOutDir, fileVal, DefaultOutDir)
}

// JudgeProvider resolves the judge provider backend name; empty means
// the judge reuses the executor provider (the engine-level fallback).
func JudgeProvider(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeProvider, fileVal, "")
}

// JudgeModel resolves the judge model name; empty falls back to the
// executor model inside eval.Engine.
func JudgeModel(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeModel, fileVal, "")
}

// JudgeBaseURL resolves the judge API base URL; the call sites fall
// back to the executor base URL when it stays empty (no default: the
// judge shares whatever endpoint the executor resolves).
func JudgeBaseURL(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeBaseURL, fileVal, "")
}

// JudgeAPIKey resolves the judge API key without a default: the call
// sites fall back to the executor key chain (flag > env > file >
// executor chain), so the judge never outlives the executor's
// credentials.
func JudgeAPIKey(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeAPIKey, fileVal, "")
}

// JudgeDecisionURL resolves the decision-model service base URL (P7);
// empty means no decision surface — --judge-backend decision then
// fails as a usage error instead of guessing an endpoint.
func JudgeDecisionURL(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeDecisionURL, fileVal, "")
}

// JudgeDecisionModel resolves the decision model name (P7); the
// /v1/systemone request body carries it, so the decision backend
// requires it (empty = usage error under --judge-backend decision).
func JudgeDecisionModel(flagVal, fileVal string) string {
	return resolve(flagVal, EnvJudgeDecisionModel, fileVal, "")
}

// JudgeBackend resolves the llm_judge backend selector. It stays
// flag/file only by design (no env — the backend is an explicit
// opt-in, config.go:32-35 semantics); a non-empty flag value wins by
// value, the file tier fills an unset flag.
func JudgeBackend(flagVal, fileVal string) string {
	return cmp.Or(flagVal, fileVal)
}

// Provider resolves the executor provider backend. It is an
// explicitness key (D2 十键): an explicitly given flag — including its
// default value — is terminal, so the file tier only fills an unset
// flag; with no file this is byte-identical to the old flag-default
// behavior.
func Provider(flagVal string, flagSet bool, fileVal string) string {
	if flagSet {
		return flagVal
	}
	return cmp.Or(fileVal, DefaultProvider)
}

// Optimizer resolves the paradigm selection (zero-config mode). Like
// Provider it is an explicitness key: the file tier only fills an
// unset flag.
func Optimizer(flagVal string, flagSet bool, fileVal string) string {
	if flagSet {
		return flagVal
	}
	return cmp.Or(fileVal, DefaultOptimizer)
}

// EvoVariant resolves the evoprompt variant; explicitness key, same
// rule as Provider/Optimizer.
func EvoVariant(flagVal string, flagSet bool, fileVal string) string {
	if flagSet {
		return flagVal
	}
	return cmp.Or(fileVal, DefaultEvoVariant)
}

// SpecMetrics resolves the zero-config synthesis metrics pin (D7，
// PRD-0001 切分 3)：链 flag > env（无——键面设计如此）> 文件 > 默认
// （空 = 现状 LLM 自选）。显式性键：显式给定的 flag——包括显式空列表
// （「显式 LLM 自选」）——终判压过文件层。
func SpecMetrics(flagVal []string, flagSet bool, fileVal []string) []string {
	if flagSet {
		return flagVal
	}
	return fileVal
}

// MaxTokens resolves the per-request completion budget. Explicitness
// key: an explicit flag wins as-is, the file tier (a positive value)
// fills an unset flag, and the default applies last.
func MaxTokens(flagVal int, flagSet bool, fileVal int) int {
	if flagSet {
		return flagVal
	}
	if fileVal > 0 {
		return fileVal
	}
	return DefaultMaxTokens
}

// RPS resolves the outbound pacing valve. Explicitness key: an
// explicit --rps 0 ("off") is terminal and must not be overridden by
// the file; the file tier fills an unset flag.
func RPS(flagVal float64, flagSet bool, fileVal float64) float64 {
	if flagSet {
		return flagVal
	}
	return fileVal
}

// ExtraBody resolves the gateway-private extras. Explicitness key: an
// explicit --extra-body "" ("explicitly off") is terminal; the file
// tier fills an unset flag. Both sides are already-decoded maps.
func ExtraBody(flagVal map[string]any, flagSet bool, fileVal map[string]any) map[string]any {
	if flagSet {
		return flagVal
	}
	return fileVal
}

// JudgeMaxTokens resolves the judge completion budget; 0 = unset (the
// engine falls back to the executor budget). A malformed or
// non-positive env value reads as unset: the judge surface is optional
// by construction and must not fail runs it was never asked for.
// Explicitness key (R1 #2): an explicitly given flag — including an
// explicit 0 ("reuse --max-tokens") — is terminal and beats both env
// and file.
func JudgeMaxTokens(flagVal int, flagSet bool, fileVal int) int {
	if flagSet {
		if flagVal > 0 {
			return flagVal
		}
		return 0
	}
	if n, err := strconv.Atoi(os.Getenv(EnvJudgeMaxTokens)); err == nil && n > 0 {
		return n
	}
	if fileVal > 0 {
		return fileVal
	}
	return 0
}

// JudgeDecisionConfidence resolves the decision cascade confidence
// threshold; 0 = the eval-package default (0.5). Explicitness key
// (R1 #2): an explicit flag 0 means "use the eval default" and must
// not be overridden by the file tier.
func JudgeDecisionConfidence(flagVal float64, flagSet bool, fileVal float64) float64 {
	if flagSet {
		return flagVal
	}
	return fileVal
}

// JudgeDecisionDiagBelow resolves the decision cascade diagnosis line;
// 0 = the eval-package default (0.6). Explicitness key, same rule as
// JudgeDecisionConfidence.
func JudgeDecisionDiagBelow(flagVal float64, flagSet bool, fileVal float64) float64 {
	if flagSet {
		return flagVal
	}
	return fileVal
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
// (PROMPTOPT_TIMEOUT) > 文件 > 0 (the caller's "unset", resolved to
// DefaultTimeout inside the provider constructors). A malformed env
// value reads as unset, mirroring JudgeMaxTokens: the surface is
// optional and must not fail runs that never asked for it — the flag
// side (ParseTimeout) and the file side (validated at the merge site)
// are the strict ones. fileVal is the merge site's already-parsed file
// timeout (0 when the key is absent).
func Timeout(flagVal, fileVal time.Duration) time.Duration {
	if flagVal > 0 {
		return flagVal
	}
	if d, err := ParseTimeout(os.Getenv(EnvTimeout)); err == nil && d > 0 {
		return d
	}
	return fileVal
}

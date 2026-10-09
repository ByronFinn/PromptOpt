// 配置快照与设置页供料（PRD-0001 切分 4 / D6）：
//
//   - PublishSnapshot 包级原子快照（atomic.Pointer，仿 cmd 侧 sink 注入
//     法——startSink 先例）：run --web 起看板前发布合并后 runOptions 的
//     逐键值与来源，web 设置页 handler 快照优先；serve 无快照回落
//     Discover+env 全局链（「当前环境」语义）。NewServer 签名零变更。
//   - 逐键来源标注复用 P1 的单一 per-key 收集器（KeySources /
//     DeriveSources，R2 #5）——本文件不设第二套 bool/map：10 键显式性
//     判定与快照来源标注派生自同一张表。
//   - 行构建（EffectiveRows / FormatValue / DefaultValue / File.Value）
//     与 `config list` 同一实现：cmd 的 list 与 web 的 /settings 共用
//     同源标注口径，展示语义不可能漂移（R1 #10：命中路径与被遮蔽文件
//     必须可见——Web 设置页同）。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
)

// MaskText is the unconditional secret mask (D8①): the config list, get
// and the web settings page always display it for api_key/judge_api_key —
// no partial echo, ever.
const MaskText = "******"

// SecretKey reports whether the key is a credential whose display value
// is always MaskText.
func SecretKey(key string) bool {
	return key == "api_key" || key == "judge_api_key"
}

// keyOrder is the canonical display/write order of the 22 file keys,
// derived once by reflection from the File declaration order — a new
// File field lands here automatically. The cmd-side registry-equality
// test (TestConfigKeyOrderMatchesRegistry) keeps this table honest
// against keyRegistry.
var keyOrder = func() []string {
	t := reflect.TypeFor[File]()
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" {
			out = append(out, name)
		}
	}
	return out
}()

// KeyOrder returns the 22 file keys in File field order (a defensive
// clone — callers must not be able to reorder the table for everyone).
func KeyOrder() []string {
	return slices.Clone(keyOrder)
}

// FlagName returns a file key's CLI flag spelling ("--base-url").
func FlagName(key string) string {
	return "--" + strings.ReplaceAll(key, "_", "-")
}

// Value reads one key's typed value from a decoded File; set=false means
// the key is absent (zero value) and the chain falls through. Nil-safe:
// a no-hit discovery carries a nil File.
func (f *File) Value(key string) (any, bool) {
	if f == nil {
		return nil, false
	}
	switch key {
	case "provider":
		return f.Provider, f.Provider != ""
	case "base_url":
		return f.BaseURL, f.BaseURL != ""
	case "model":
		return f.Model, f.Model != ""
	case "api_key":
		return f.APIKey, f.APIKey != ""
	case "max_tokens":
		return f.MaxTokens, f.MaxTokens != 0
	case "timeout":
		return string(f.Timeout), f.Timeout != ""
	case "rps":
		return f.RPS, f.RPS != 0
	case "extra_body":
		return f.ExtraBody, f.ExtraBody != nil
	case "out":
		return f.Out, f.Out != ""
	case "judge_provider":
		return f.JudgeProvider, f.JudgeProvider != ""
	case "judge_base_url":
		return f.JudgeBaseURL, f.JudgeBaseURL != ""
	case "judge_model":
		return f.JudgeModel, f.JudgeModel != ""
	case "judge_api_key":
		return f.JudgeAPIKey, f.JudgeAPIKey != ""
	case "judge_max_tokens":
		return f.JudgeMaxTokens, f.JudgeMaxTokens != 0
	case "judge_backend":
		return f.JudgeBackend, f.JudgeBackend != ""
	case "judge_decision_url":
		return f.JudgeDecisionURL, f.JudgeDecisionURL != ""
	case "judge_decision_model":
		return f.JudgeDecisionModel, f.JudgeDecisionModel != ""
	case "judge_decision_confidence":
		return f.JudgeDecisionConfidence, f.JudgeDecisionConfidence != 0
	case "judge_decision_diag_below":
		return f.JudgeDecisionDiagBelow, f.JudgeDecisionDiagBelow != 0
	case "optimizer":
		return f.Optimizer, f.Optimizer != ""
	case "evo_variant":
		return f.EvoVariant, f.EvoVariant != ""
	case "spec_metrics":
		return f.SpecMetrics, len(f.SpecMetrics) > 0
	}
	return nil, false
}

// DefaultValue is the display value a key resolves to when neither env
// nor file carries it (same semantics as the resolution chain's default
// tier; judge_* 空 = 回落执行器，spec_metrics 空 = LLM 自选，
// judge_backend 空 = llm). The "180s" literal keeps the same
// comment-sync convention as the provider constructors' 180s fallback.
func DefaultValue(key string) any {
	switch key {
	case "provider":
		return DefaultProvider
	case "api_key":
		return DefaultAPIKey
	case "max_tokens":
		return DefaultMaxTokens
	case "timeout":
		return "180s"
	case "rps":
		return 0.0
	case "out":
		return DefaultOutDir
	case "optimizer":
		return DefaultOptimizer
	case "evo_variant":
		return DefaultEvoVariant
	default:
		return ""
	}
}

// FormatValue renders one key's value for display: secret keys are
// always masked (D8①), extra_body becomes JSON, spec_metrics a comma
// list. 空串原样返回（展示层再落成 -）。
func FormatValue(key string, v any) any {
	if SecretKey(key) {
		return MaskText
	}
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return ""
		}
		b, err := json.Marshal(t)
		if err != nil {
			return strings.TrimSpace(fmt.Sprint(t))
		}
		return string(b)
	case []string:
		return strings.Join(t, ",")
	default:
		return v
	}
}

// SnapshotKey is one row of the settings page / config list: the key,
// its effective display value (already formatted — masked for secrets),
// the per-key source and the flag spelling.
type SnapshotKey struct {
	Key    string
	Value  any
	Source Source
	Flag   string
}

// NewSnapshotKey builds one row from a merged effective value: the
// run-side snapshot path passes the merged runOptions field; a
// default-sourced row displays the resolved default (e.g. the timeout
// zero value means the constructors' 180s). An explicit flag zero keeps
// its literal display ("0") — that is the honest value the run uses.
func NewSnapshotKey(key string, val any, src Source) SnapshotKey {
	if src == SourceDefault {
		val = DefaultValue(key)
	}
	return SnapshotKey{Key: key, Value: FormatValue(key, val), Source: src, Flag: FlagName(key)}
}

// Snapshot is the package-level settings snapshot published before a
// run --web dashboard starts: the merged per-key values with their
// sources (the SAME KeySources collector the merge consulted — R2 #5)
// plus the discovery hit-site provenance (R1 #10).
type Snapshot struct {
	HitPath  string
	Shadowed []string
	Keys     []SnapshotKey
}

// liveSnapshot is the atomic slot; zero value = "no snapshot" (serve
// mode falls back to the global discovery chain).
var liveSnapshot atomic.Pointer[Snapshot]

// PublishSnapshot atomically replaces the settings snapshot. Called by
// run --web's startSink right before the dashboard listens (仿 sink 注入
// 法：包级原子位，零 NewServer 签名变更); nil resets it.
func PublishSnapshot(s *Snapshot) { liveSnapshot.Store(s) }

// CurrentSnapshot returns the published snapshot, or nil when the
// process never published one (serve mode — the handler then walks the
// global Discover+env chain with the「当前环境」semantics).
func CurrentSnapshot() *Snapshot { return liveSnapshot.Load() }

// EffectiveRows resolves the per-key effective rows for the "no run
// flags" context (config list and the serve-mode settings page): env >
// file > default, with sources derived from the SAME registry the merge
// itself consults — the display cannot disagree with the chain.
func EffectiveRows(disc Discovery) []SnapshotKey {
	sources := DeriveSources(map[string]bool{}, disc.File)
	rows := make([]SnapshotKey, 0, len(keyOrder))
	for _, key := range keyOrder {
		var raw any
		switch sources[key] {
		case SourceEnv:
			raw = os.Getenv(keyRegistry[key].EnvKey)
		case SourceFile:
			v, _ := disc.File.Value(key)
			raw = v
		default:
			raw = DefaultValue(key)
		}
		rows = append(rows, SnapshotKey{
			Key: key, Value: FormatValue(key, raw), Source: sources[key], Flag: FlagName(key),
		})
	}
	return rows
}

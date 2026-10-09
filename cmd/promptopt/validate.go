package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/builtin"
)

// 单键校验 helper（PRD-0001 切分 1 前置重构，R1 #6）：校验原本全部内联
// 在 parseRunFlags 尾部、无可复用 seam。「config set/init 与 flag 同一套
// 校验器、不出现第二条校验语义」的红线要求先抽取——parseRunFlags 改调
// helper（行为保持：错误文案与 errors.Join 顺序由
// TestParseRunFlagsValidationOrderSnapshot 钉死），切分 2 的 config
// set/init 复用同一批 helper。helper 只做单键校验：跨键完整性（如
// decision 后端缺 url/model）留 run/verify 汇聚点，set 永不复制第二条
// 完整性语义。

// validateProvider checks the executor provider backend whitelist.
func validateProvider(name string) error {
	if name != "openai" && name != "anthropic" {
		return fmt.Errorf("--provider must be openai or anthropic, got %q", name)
	}
	return nil
}

// validateOptimizer checks the paradigm name against the builtin
// registry plus "auto".
func validateOptimizer(name string) error {
	validOptimizers := builtin.Registry().Names()
	validOptimizers = append(validOptimizers, "auto")
	slices.Sort(validOptimizers)
	if !slices.Contains(validOptimizers, name) {
		return fmt.Errorf("--optimizer %q is not a registered paradigm (available: %s)",
			name, strings.Join(validOptimizers, ", "))
	}
	return nil
}

// validateJudgeProvider checks the dedicated judge provider whitelist
// (empty = reuse the executor provider, always legal).
func validateJudgeProvider(name string) error {
	if name != "" && name != "openai" && name != "anthropic" {
		return fmt.Errorf("--judge-provider must be openai or anthropic, got %q", name)
	}
	return nil
}

// validateJudgeBackend checks the llm_judge backend selector.
func validateJudgeBackend(backend string) error {
	switch backend {
	case "", eval.JudgeBackendLLM, eval.JudgeBackendDecision:
		return nil
	default:
		return fmt.Errorf("--judge-backend must be %s or %s, got %q",
			eval.JudgeBackendLLM, eval.JudgeBackendDecision, backend)
	}
}

// validateTimeout parses one --timeout flag value: strict grammar on
// the flag side (ParseTimeout), wrapped with the flag name for the
// usage error.
func validateTimeout(raw string) (time.Duration, error) {
	d, err := config.ParseTimeout(raw)
	if err != nil {
		return 0, fmt.Errorf("--timeout: %w", err)
	}
	return d, nil
}

// validateMetricsList validates a spec-metrics list (the --spec-metrics
// flag and the spec_metrics file key share it): every item must be in
// core.ValidMetrics, no duplicates, no empty items. llm_judge is legal
// here — the engine special-cases it ahead of the eval registry.
func validateMetricsList(metrics []string) error {
	if len(metrics) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(metrics))
	for _, m := range metrics {
		switch {
		case strings.TrimSpace(m) == "":
			return fmt.Errorf("--spec-metrics 含空项（合法指标：%s）", strings.Join(core.ValidMetrics, ", "))
		case !slices.Contains(core.ValidMetrics, m):
			return fmt.Errorf("--spec-metrics %q is not a valid metric (available: %s)",
				m, strings.Join(core.ValidMetrics, ", "))
		case seen[m]:
			return fmt.Errorf("--spec-metrics %q 重复出现", m)
		}
		seen[m] = true
	}
	return nil
}

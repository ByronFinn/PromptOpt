package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// --- --spec-metrics flag 面（PRD-0001 切分 3 / D7）---------------------------
//
// 本文件钉 D7 的 CLI 侧：flag 校验（白名单/去重/非空）、显式性链
//（显式 flag 含显式空压过文件）、configured 模式互斥与文件键提示扩展、
// decision 键的文件层全链，以及 --spec-metrics llm_judge 的零配置
// e2e（httptest 桩：llm_judge 走引擎特判 + decision 裁判路径）。

// TestParseRunFlagsSpecMetrics pins the flag validation: 逐项 ∈
// core.ValidMetrics、无重复、非空（经 validateMetricsList，与 config set
// 的 spec_metrics 值同一准入）；三件套模式显式给 → 互斥报错。
func TestParseRunFlagsSpecMetrics(t *testing.T) {
	isolateRunConfigEnv(t)
	zc := []string{"从中医医案判断证候", "--base-url", "http://127.0.0.1:9", "--model", "m"}

	// 合法列表：逐项裁剪空格后过白名单；llm_judge 合法（引擎特判分发）。
	o, err := parseRunFlags(append(slices.Clone(zc), "--spec-metrics", " llm_judge , f1 "))
	if err != nil {
		t.Fatalf("valid list: %v", err)
	}
	if !slices.Equal(o.specMetrics, []string{"llm_judge", "f1"}) {
		t.Errorf("specMetrics = %v, want [llm_judge f1] (trimmed)", o.specMetrics)
	}
	if o.sources["spec_metrics"] != config.SourceFlag {
		t.Errorf("sources[spec_metrics] = %s, want flag (explicit)", o.sources["spec_metrics"])
	}

	for _, tc := range []struct {
		name, raw, wantErr string
	}{
		{"whitelist refusal", "exact_match,sentiment", "is not a valid metric"},
		{"duplicate refusal", "f1,exact_match,f1", "重复出现"},
		{"empty item refusal", "f1,,exact_match", "含空项"},
	} {
		if _, err := parseRunFlags(append(slices.Clone(zc), "--spec-metrics", tc.raw)); err == nil ||
			!strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}

	// 三件套模式显式给 → 互斥报错（--optimizer/--evo-variant 显式性先例）。
	task, cand, ds := fixtureYAMLs(t)
	_, err = parseRunFlags([]string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--spec-metrics", "f1"})
	if err == nil || !strings.Contains(err.Error(), "--spec-metrics only takes effect in the zero-config mode") {
		t.Errorf("configured-mode explicit --spec-metrics err = %v, want the mutual-exclusion refusal", err)
	}
}

// TestSpecMetricsFileTier pins the spec_metrics 文件键 in the single
// chain: 文件层供链（零配置模式生效）、显式 flag（含合法列表与显式空
// 「LLM 自选」）终判压过文件、文件值非法走同一 validateMetricsList 拒绝。
func TestSpecMetricsFileTier(t *testing.T) {
	zc := []string{"从中医医案判断证候", "--base-url", "http://127.0.0.1:9", "--model", "m"}

	t.Run("file tier supplies the zero-config pin", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [llm_judge, f1]\n", 0o600)
		o, err := parseRunFlags(zc)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !slices.Equal(o.specMetrics, []string{"llm_judge", "f1"}) {
			t.Errorf("specMetrics = %v, want the file list", o.specMetrics)
		}
		if o.sources["spec_metrics"] != config.SourceFile {
			t.Errorf("sources[spec_metrics] = %s, want file", o.sources["spec_metrics"])
		}
	})

	t.Run("explicit flag list beats the file", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [llm_judge, f1]\n", 0o600)
		o, err := parseRunFlags(append(slices.Clone(zc), "--spec-metrics", "exact_match"))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !slices.Equal(o.specMetrics, []string{"exact_match"}) {
			t.Errorf("specMetrics = %v, want the flag list (file must not win)", o.specMetrics)
		}
		if o.sources["spec_metrics"] != config.SourceFlag {
			t.Errorf("sources[spec_metrics] = %s, want flag", o.sources["spec_metrics"])
		}
	})

	t.Run("explicit empty flag = 显式 LLM 自选，压过文件", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [llm_judge]\n", 0o600)
		o, err := parseRunFlags(append(slices.Clone(zc), "--spec-metrics", ""))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(o.specMetrics) != 0 {
			t.Errorf("specMetrics = %v, want empty (explicit LLM 自选 must beat the file)", o.specMetrics)
		}
	})

	t.Run("invalid file value refuses with the shared validator", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [bogus_metric]\n", 0o600)
		_, err := parseRunFlags(zc)
		if err == nil || !strings.Contains(err.Error(), "is not a valid metric") {
			t.Errorf("invalid file spec_metrics err = %v, want the shared whitelist refusal", err)
		}
	})
}

// TestConfiguredModeSpecMetricsHint 钉 D3/R1 #5 提示扩展：文件含
// spec_metrics 且三件套模式 → 与 optimizer/evo_variant 同一句 stderr
// 中文提示（键列表并排）；零配置模式照常生效不提示。
func TestConfiguredModeSpecMetricsHint(t *testing.T) {
	t.Run("configured mode joins spec_metrics into the hint", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "optimizer: p1\nspec_metrics: [f1]\n", 0o600)
		// 缺 base_url 让 run 在提示之后以用法错退出——提示先于校验输出。
		code, stderr := captureCliStderr(t, runCommand, "--task", "t", "--candidate", "c", "--dataset", "d")
		if code != 1 {
			t.Errorf("exit = %d, want 1 (missing required params)", code)
		}
		if !strings.Contains(stderr, "配置模式（task/candidate/dataset 三件套）下不生效") {
			t.Errorf("stderr lacks the paradigm hint:\n%s", stderr)
		}
		if !strings.Contains(stderr, "optimizer/spec_metrics") {
			t.Errorf("stderr hint lacks the joined key list:\n%s", stderr)
		}
	})
	t.Run("spec_metrics alone still hints", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [f1]\n", 0o600)
		_, stderr := captureCliStderr(t, runCommand, "--task", "t", "--candidate", "c", "--dataset", "d")
		if !strings.Contains(stderr, "配置文件 promptopt.yaml 中的 spec_metrics 在配置模式") {
			t.Errorf("stderr lacks the spec_metrics-only hint:\n%s", stderr)
		}
	})
	t.Run("zero-config mode takes the file value without a hint", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "spec_metrics: [f1]\n", 0o600)
		_, stderr := captureCliStderr(t, runCommand, "零配置提示词")
		if strings.Contains(stderr, "不生效") {
			t.Errorf("zero-config mode must not hint (the value takes effect):\n%s", stderr)
		}
	})
}

// TestDecisionSurfaceFromConfigFile 是 D7 验收的 decision 键全链切口：
// config 文件给齐 judge_backend=decision + judge_decision_url/model →
// 经 P1 文件层合并后走 run 侧既有完整性检查（不报缺参）；文件缺
// url/model → D5 新文案（errors.Is 哨兵，文案含 config set 键指引）。
func TestDecisionSurfaceFromConfigFile(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	trio := []string{"--task", task, "--candidate", cand, "--dataset", ds}

	t.Run("file-supplied decision keys pass the completeness check", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, `base_url: http://x
model: m
judge_backend: decision
judge_decision_url: http://decision.example
judge_decision_model: tev1:0.8b
`, 0o600)
		o, err := parseRunFlags(trio)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if o.judgeBackend != "decision" || o.judgeDecisionURL != "http://decision.example" ||
			o.judgeDecisionModel != "tev1:0.8b" {
			t.Errorf("decision surface from file = %s/%s/%s", o.judgeBackend, o.judgeDecisionURL, o.judgeDecisionModel)
		}
	})

	t.Run("file decision backend without url/model hits the D5 guidance", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, `base_url: http://x
model: m
judge_backend: decision
`, 0o600)
		_, err := parseRunFlags(trio)
		if !errors.Is(err, errRunDecisionRequired) {
			t.Fatalf("err = %v, want errRunDecisionRequired", err)
		}
		if !strings.Contains(err.Error(), "config set judge_decision_url") {
			t.Errorf("D5 text lacks the config-key guidance:\n%s", err.Error())
		}
	})
}

// --- --spec-metrics llm_judge 零配置 e2e（httptest 桩，绝不触网）--------------

// startRecordingZeroConfigLLM 是带请求体记录的零配置 mock：spec 应答
// exact_match 规格（LLM 自选）——--spec-metrics llm_judge 的覆盖效果由
// 盘上 spec.json 证明；评测调用按 zcScript 打分；裁判面走 decision 桩，
// 主服务永不见裁判提示词。
func startRecordingZeroConfigLLM(t *testing.T, bodies *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var content string
		switch {
		case strings.Contains(body, harness.MarkerSpec):
			content = zcSpec
		case strings.Contains(body, harness.MarkerSamples):
			content = zcSamples
		case strings.Contains(body, harness.MarkerProbes):
			content = zcProbes
		case strings.Contains(body, harness.MarkerRepair),
			strings.Contains(body, engine.MarkerHypRepair),
			strings.Contains(body, engine.MarkerCandFix):
			t.Errorf("unexpected repair call: %.200s", body)
			content = "{}"
		case strings.Contains(body, engine.MarkerReflect):
			content = zcHypotheses
		case strings.Contains(body, engine.MarkerRewrite),
			strings.Contains(body, engine.MarkerMerge),
			strings.Contains(body, engine.MarkerFresh):
			content = zcMutation
		default:
			content = "无法辨证"
			for _, sc := range zcScript {
				if !strings.Contains(body, sc.input) {
					continue
				}
				if sc.p1 == 1 || sc.p2 == 1 {
					content = sc.expected
				}
				break
			}
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` +
			jsonQuote(content) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonQuote marshals one string as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestRunSpecMetricsLLMJudgeEndToEnd：--spec-metrics llm_judge 的零配置
// run 走通探针/基线（Filter judge 面透传），llm_judge 由引擎特判分发
// 不经注册表，decision 桩接住全部裁判调用（judge-decision 路径），
// manifest 快照 spec_metrics。LLM 自选 exact_match 而盘上规格是
// llm_judge——覆盖点在端到端链路里生效的直接证据。
func TestRunSpecMetricsLLMJudgeEndToEnd(t *testing.T) {
	isolateRunConfigEnv(t)
	base := t.TempDir()
	outDir := filepath.Join(base, "runs")

	var mu sync.Mutex
	var mainBodies, decBodies []string
	mainSrv := startRecordingZeroConfigLLM(t, &mainBodies, &mu)
	decSrv := startRecordingDecision(t, &decBodies, &mu)

	code, stdout, stderr := runCliVerbose(t, "从中医医案文本判断证候",
		"--base-url", mainSrv.URL, "--model", "jiuwei-tcm", "--api-key", "1",
		"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
		"--max-rounds", "1",
		"--spec-metrics", "llm_judge",
		"--judge-backend", "decision",
		"--judge-decision-url", decSrv.URL, "--judge-decision-model", "tev1:0.8b")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "裁判回退执行器配置") {
		t.Errorf("decision backend must not trip the judge-fallback warning:\n%s", stderr)
	}

	// Headless stdout：llm_judge 均值 1.0（决策桩可信答案 score 3/(4−1)），
	// 裁判用量单列 RoleJudge。
	var res core.RunResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("headless stdout is not JSON: %v\n%s", err, stdout)
	}
	if res.MetricMeans["llm_judge"] != 1 {
		t.Errorf("llm_judge mean = %v, want 1 (decision cascade trusted answers)", res.MetricMeans)
	}
	if u, ok := res.UsageByRole[core.RoleJudge]; !ok || u.PromptTokens == 0 {
		t.Errorf("judge usage = %+v (present=%v), want decision calls metered under RoleJudge", u, ok)
	}

	// 盘上 spec.json：LLM 应答的是 exact_match 规格，落盘必须是钉死的
	// llm_judge（覆盖点端到端生效）。
	runDirs, err := os.ReadDir(outDir)
	if err != nil || len(runDirs) != 1 {
		t.Fatalf("run dirs = %v (%v)", runDirs, err)
	}
	runID := runDirs[0].Name()
	synthDir := filepath.Join(filepath.Join(outDir, "..", "synth"), runID)
	specFile, err := harness.LoadSpec(synthDir)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if !slices.Equal(specFile.Task.Metrics, []string{"llm_judge"}) {
		t.Errorf("spec metrics = %v, want the pinned [llm_judge] (the stub answered exact_match)", specFile.Task.Metrics)
	}

	// manifest 快照 spec_metrics。
	var mf runManifest
	loadJSONFile(t, filepath.Join(outDir, runID, "manifest.json"), &mf)
	if !slices.Equal(mf.SpecMetrics, []string{"llm_judge"}) {
		t.Errorf("manifest spec_metrics = %v, want [llm_judge]", mf.SpecMetrics)
	}

	// decision 桩接住裁判调用：body 是 {model, state, questions} 名字
	// 索引形；主服务全程不见裁判提示词（单一裁判口径）。
	mu.Lock()
	defer mu.Unlock()
	if len(decBodies) == 0 {
		t.Fatal("decision service received no calls")
	}
	var req map[string]any
	if err := json.Unmarshal([]byte(decBodies[0]), &req); err != nil {
		t.Fatalf("decision body: %v\n%s", err, decBodies[0])
	}
	if req["model"] != "tev1:0.8b" || req["state"] == "" {
		t.Errorf("decision body model/state = %v / %v", req["model"], req["state"])
	}
	if _, ok := req["questions"].(map[string]any); !ok {
		t.Errorf("decision questions not name-indexed: %T", req["questions"])
	}
	for _, b := range mainBodies {
		if strings.Contains(b, "评估裁判") {
			t.Errorf("judge prompt leaked to the executor server: %.200s", b)
		}
	}

	// 审计锚点：run 树 + synth 树里存在 stage=judge-decision 的调用迹。
	sawDecision := false
	for _, root := range []string{filepath.Join(outDir, runID), synthDir} {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.Contains(filepath.ToSlash(path), "/calls/") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			var ct struct {
				Stage string `json:"stage"`
			}
			if json.Unmarshal(raw, &ct) == nil && ct.Stage == "judge-decision" {
				sawDecision = true
			}
			return nil
		})
	}
	if !sawDecision {
		t.Error("no call trace carries stage judge-decision")
	}
}

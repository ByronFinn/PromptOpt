package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/config"
)

// --- 配置文件层测试（PRD-0001 切分 1）----------------------------------------
//
// 本文件钉三层字节级不变（D2）+ 显式 0 切口（R1 #2）+ 发现序与损坏
// 语义（D1/R1 #1）+ 来源收集器唯一性（R2 #5）+ configured 模式提示
//（D3/R1 #5）。verify 侧的 11 键白名单补位切口在 verify_test.go。

// isolateRunConfigEnv 把环境收敛到「无任何配置文件」的确定性状态（与
// internal/config 的测试助手同构）：临时工作目录、临时 HOME、空
// XDG_CONFIG_HOME、清空 PROMPTOPT_CONFIG 与全部 PROMPTOPT_* env。
// t.Chdir 不得与 t.Parallel 同用（漏隔离炸测试而非静默污染）。
func isolateRunConfigEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv(config.EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		config.EnvBaseURL, config.EnvModel, config.EnvAPIKey, config.EnvOutDir,
		config.EnvTimeout, config.EnvJudgeProvider, config.EnvJudgeModel,
		config.EnvJudgeBaseURL, config.EnvJudgeAPIKey, config.EnvJudgeMaxTokens,
		config.EnvJudgeDecisionURL, config.EnvJudgeDecisionModel,
	} {
		t.Setenv(k, "")
	}
}

// userRunConfigPath 按 GOOS 推导用户级配置路径（R2 #6②：切口不写死
// 单一路径）。
func userRunConfigPath(t *testing.T) string {
	t.Helper()
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	return filepath.Join(dir, "promptopt", "config.yaml")
}

func writeRunFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

// TestParseRunFlagsNoFileInvariant is the D2 byte-level layer 2: with
// every config layer absent, a fixed argv must resolve to exactly the
// pre-file whole-struct value set — the whole struct, not spot fields.
func TestParseRunFlagsNoFileInvariant(t *testing.T) {
	isolateRunConfigEnv(t)
	task, cand, ds := fixtureYAMLs(t)
	out := filepath.Join(t.TempDir(), "runs")
	args := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", out}
	o, err := parseRunFlags(args)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := runOptions{
		taskPath: task, candidatePath: cand, datasetPath: ds,
		baseURL: "http://127.0.0.1:9", model: "m", apiKey: "1", outDir: out,
		addr:         config.DefaultAddr,
		optimizer:    config.DefaultOptimizer,
		providerName: config.DefaultProvider,
		evoVariant:   config.DefaultEvoVariant,
		maxTokens:    config.DefaultMaxTokens, workers: config.DefaultWorkers,
		samples: config.DefaultSamples, probeVariants: config.DefaultProbeVariants,
		maxRounds: config.DefaultMaxRounds, minibatch: config.DefaultMinibatch,
		stagnationLimit: config.DefaultStagnationLimit, epsilon: config.DefaultEpsilon,
		reps: 1,
		// 单一收集器：显式 flag 层（task/candidate/dataset 不在文件键
		// 集），其余在无文件无 env 时全部落 default。
		sources: config.KeySources{
			"provider": config.SourceDefault, "base_url": config.SourceFlag,
			"model": config.SourceFlag, "api_key": config.SourceDefault,
			"max_tokens": config.SourceDefault, "timeout": config.SourceDefault,
			"rps": config.SourceDefault, "extra_body": config.SourceDefault,
			"out": config.SourceFlag, "judge_provider": config.SourceDefault,
			"judge_base_url": config.SourceDefault, "judge_model": config.SourceDefault,
			"judge_api_key": config.SourceDefault, "judge_max_tokens": config.SourceDefault,
			"judge_backend": config.SourceDefault, "judge_decision_url": config.SourceDefault,
			"judge_decision_model":      config.SourceDefault,
			"judge_decision_confidence": config.SourceDefault,
			"judge_decision_diag_below": config.SourceDefault,
			"optimizer":                 config.SourceDefault, "evo_variant": config.SourceDefault,
			"spec_metrics": config.SourceDefault,
		},
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("no-file parse drifted from the pre-file invariant:\n got %+v\nwant %+v", o, want)
	}
}

// TestParseVerifyFlagsNoFileInvariant mirrors the run-side layer-2 pin
// for the verify flag surface.
func TestParseVerifyFlagsNoFileInvariant(t *testing.T) {
	isolateRunConfigEnv(t)
	runID := "20261009-000000-abcd"
	o, gotID, err := parseVerifyFlags([]string{runID, "--runs-dir", "rd",
		"--base-url", "http://127.0.0.1:9", "--model", "m"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gotID != runID {
		t.Errorf("run id = %q, want %q", gotID, runID)
	}
	want := verifyOptions{
		runsDir: "rd", anchorsDir: anchors.DefaultDir,
		maxRegression: defaultMaxRegression, reps: 1, bootstrap: defaultBootstrap,
		baseURL: "http://127.0.0.1:9", model: "m",
		maxTokens: config.DefaultMaxTokens, workers: config.DefaultWorkers,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("no-file verify parse drifted:\n got %+v\nwant %+v", o, want)
	}
}

// TestParseRunFlagsConfigFileMerge pins the file tier inside the single
// chain: file-only values resolve, env slots above file, explicit flags
// stay terminal — and the sources collector agrees with the merge
// (same-origin, R2 #5).
func TestParseRunFlagsConfigFileMerge(t *testing.T) {
	isolateRunConfigEnv(t)
	task, cand, ds := fixtureYAMLs(t)
	writeRunFile(t, config.ProjectFileName, `base_url: http://file.example/v1
model: file-model
api_key: file-key
max_tokens: 512
provider: anthropic
rps: 2.5
out: file-runs
judge_provider: openai
judge_base_url: http://judge.example/v1
judge_model: file-judge-m
judge_api_key: file-judge-key
judge_max_tokens: 256
judge_backend: decision
judge_decision_url: http://decision.example
judge_decision_model: tev1
judge_decision_confidence: 0.7
judge_decision_diag_below: 0.55
timeout: 45
extra_body:
  chat_template_kwargs:
    enable_thinking: false
`, 0o600)
	t.Setenv(config.EnvJudgeMaxTokens, "111") // env 层压过文件层

	o, err := parseRunFlags([]string{"--task", task, "--candidate", cand, "--dataset", ds})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// 文件层补齐了 run 的必填连接参数——没有 --base-url/--model flag。
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"base_url from file", o.baseURL, "http://file.example/v1"},
		{"model from file", o.model, "file-model"},
		{"api_key from file", o.apiKey, "file-key"},
		{"max_tokens from file", o.maxTokens, 512},
		{"provider from file", o.providerName, "anthropic"},
		{"rps from file", o.rps, 2.5},
		{"out from file", o.outDir, "file-runs"},
		{"judge_provider from file", o.judgeProvider, "openai"},
		{"judge_base_url from file", o.judgeBaseURL, "http://judge.example/v1"},
		{"judge_model from file", o.judgeModel, "file-judge-m"},
		{"judge_api_key from file", o.judgeAPIKey, "file-judge-key"},
		{"judge_backend from file", o.judgeBackend, "decision"},
		{"judge_decision_url from file", o.judgeDecisionURL, "http://decision.example"},
		{"judge_decision_model from file", o.judgeDecisionModel, "tev1"},
		{"judge_decision_confidence from file", o.judgeDecisionConfidence, 0.7},
		{"judge_decision_diag_below from file", o.judgeDecisionDiagBelow, 0.55},
		{"timeout from file", o.timeout, 45 * time.Second},
		{"env beats file for judge_max_tokens", o.judgeMaxTokens, 111},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	wantBody := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	if !reflect.DeepEqual(o.extraBody, wantBody) {
		t.Errorf("extra_body = %v, want %v", o.extraBody, wantBody)
	}
	// 来源收集器与合并决策同源（R2 #5）：文件在场的键落 file，env 压过
	// 文件的键落 env，文件缺席的键落 default。
	wantSource := map[string]config.Source{}
	for key := range o.sources {
		wantSource[key] = config.SourceFile
	}
	wantSource["judge_max_tokens"] = config.SourceEnv // env 压过文件
	wantSource["optimizer"] = config.SourceDefault    // 文件未写
	wantSource["evo_variant"] = config.SourceDefault
	wantSource["spec_metrics"] = config.SourceDefault
	for key, want := range wantSource {
		if o.sources[key] != want {
			t.Errorf("sources[%s] = %s, want %s", key, o.sources[key], want)
		}
	}
}

// TestConfigFileExplicitZeroWins is the R1 #2 切口: a file value must
// never override an explicitly given flag — including the meaningful
// zeros (显式 0 = 回落/关闭).
func TestConfigFileExplicitZeroWins(t *testing.T) {
	isolateRunConfigEnv(t)
	task, cand, ds := fixtureYAMLs(t)
	writeRunFile(t, config.ProjectFileName, `base_url: http://file/v1
model: file-m
judge_decision_confidence: 0.7
judge_decision_diag_below: 0.8
judge_max_tokens: 512
rps: 2.5
extra_body:
  chat_template_kwargs:
    enable_thinking: false
`, 0o600)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds}

	// 对照组：不给 flag → 文件值生效。
	o, err := parseRunFlags(base)
	if err != nil {
		t.Fatalf("control parse: %v", err)
	}
	if o.judgeDecisionConfidence != 0.7 || o.judgeDecisionDiagBelow != 0.8 ||
		o.judgeMaxTokens != 512 || o.rps != 2.5 || o.extraBody == nil {
		t.Errorf("control: file values must apply, got %+v", o)
	}

	// 切口：显式 0 终判压过文件。
	o, err = parseRunFlags(append(append([]string{}, base...),
		"--judge-decision-confidence", "0", "--judge-decision-diag-below", "0",
		"--judge-max-tokens", "0", "--rps", "0", "--extra-body", ""))
	if err != nil {
		t.Fatalf("explicit-zero parse: %v", err)
	}
	if o.judgeDecisionConfidence != 0 || o.judgeDecisionDiagBelow != 0 {
		t.Errorf("explicit zero confidence/diag = %v/%v, want 0/0 (file 0.7/0.8 must not win)",
			o.judgeDecisionConfidence, o.judgeDecisionDiagBelow)
	}
	if o.judgeMaxTokens != 0 {
		t.Errorf("explicit zero judge_max_tokens = %d, want 0 (file 512 must not win)", o.judgeMaxTokens)
	}
	if o.rps != 0 {
		t.Errorf("explicit zero rps = %v, want 0 (file 2.5 must not win)", o.rps)
	}
	if o.extraBody != nil {
		t.Errorf("explicit empty extra_body = %v, want nil (file map must not win)", o.extraBody)
	}
	// 显式性判定派生自 Source==flag（R2 #5）。
	for _, key := range []string{"judge_decision_confidence", "judge_decision_diag_below", "judge_max_tokens", "rps", "extra_body"} {
		if o.sources[key] != config.SourceFlag {
			t.Errorf("sources[%s] = %s, want flag (explicit)", key, o.sources[key])
		}
	}
}

// TestConfigFileDiscoveryErrors pins the D1/R1 #1 hard-error semantics
// through parseRunFlags: a corrupt hit never falls through to a good
// lower layer; an explicit missing path errors; unknown keys carry the
// path and the YAML line.
func TestConfigFileDiscoveryErrors(t *testing.T) {
	t.Run("corrupt project file shadows a good user file", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "base_url: [unclosed\n", 0o600)
		writeRunFile(t, userRunConfigPath(t), "base_url: http://user/v1\nmodel: user-m\n", 0o600)
		_, err := parseRunFlags([]string{"--task", "t", "--candidate", "c", "--dataset", "d"})
		if err == nil {
			t.Fatal("corrupt hit must hard-error, not fall through to the user file")
		}
		if !strings.Contains(err.Error(), config.ProjectFileName) {
			t.Errorf("error %q lacks the corrupt path", err)
		}
	})
	t.Run("explicit missing path errors", func(t *testing.T) {
		isolateRunConfigEnv(t)
		_, err := parseRunFlags([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml"),
			"--task", "t", "--candidate", "c", "--dataset", "d",
			"--base-url", "http://x", "--model", "m"})
		if err == nil {
			t.Fatal("explicit --config pointing at a missing file must hard-error")
		}
		if !strings.Contains(err.Error(), "nope.yaml") {
			t.Errorf("error %q lacks the path", err)
		}
	})
	t.Run("unknown key carries path and line", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "baseurl: http://typo/v1\n", 0o600)
		_, err := parseRunFlags([]string{"--task", "t", "--candidate", "c", "--dataset", "d"})
		if err == nil {
			t.Fatal("unknown key must hard-error")
		}
		for _, frag := range []string{config.ProjectFileName, "line 1", "baseurl"} {
			if !strings.Contains(err.Error(), frag) {
				t.Errorf("error %q lacks %q", err, frag)
			}
		}
	})
	t.Run("malformed file timeout errors", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "timeout: junk\n", 0o600)
		_, err := parseRunFlags([]string{"--task", "t", "--candidate", "c", "--dataset", "d"})
		if err == nil || !strings.Contains(err.Error(), "配置文件") {
			t.Errorf("malformed file timeout = %v, want a 配置文件 usage error", err)
		}
	})
	t.Run("PROMPTOPT_CONFIG beats the project file", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "model: project-m\n", 0o600)
		envPath := filepath.Join(t.TempDir(), "env.yaml")
		writeRunFile(t, envPath, "model: env-m\n", 0o600)
		t.Setenv(config.EnvConfig, envPath)
		o, err := parseRunFlags([]string{"--task", "t", "--candidate", "c", "--dataset", "d",
			"--base-url", "http://x"})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if o.model != "env-m" {
			t.Errorf("model = %q, want the env-layer file value", o.model)
		}
		if o.sources["model"] != config.SourceFile {
			t.Errorf("sources[model] = %s, want file (the value came from a file)", o.sources["model"])
		}
	})
}

// TestConfiguredModeParadigmHint pins the D3/R1 #5 strategy: a file
// carrying optimizer/evo_variant in the configured (trio) mode prints
// one stderr Chinese hint and does NOT hard-error; the zero-config mode
// stays hint-free (the values take effect there).
func TestConfiguredModeParadigmHint(t *testing.T) {
	t.Run("configured mode hints on stderr", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "optimizer: p1\nevo_variant: de\n", 0o600)
		// 缺 base_url 让 run 在提示之后以用法错退出——提示先于校验输出。
		code, stderr := captureCliStderr(t, runCommand, "--task", "t", "--candidate", "c", "--dataset", "d")
		if code != 1 {
			t.Errorf("exit = %d, want 1 (missing required params)", code)
		}
		if !strings.Contains(stderr, "配置模式（task/candidate/dataset 三件套）下不生效") {
			t.Errorf("stderr lacks the paradigm hint:\n%s", stderr)
		}
		if !strings.Contains(stderr, "optimizer/evo_variant") {
			t.Errorf("stderr hint lacks the key list:\n%s", stderr)
		}
	})
	t.Run("zero-config mode stays hint-free", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "optimizer: p1\nevo_variant: de\n", 0o600)
		_, stderr := captureCliStderr(t, runCommand, "零配置提示词")
		if strings.Contains(stderr, "不生效") {
			t.Errorf("zero-config mode must not hint (the values take effect):\n%s", stderr)
		}
	})
}

// TestParseRunFlagsValidationOrderSnapshot pins the helper-extraction
// behavior contract (R1 #6): the exact error texts and the
// errors.Join order survive the refactor byte-for-byte. The missing-
// param wording is the D5 upgraded text (切分 2)：三途径指路快照同时由
// TestD5MissingParamGuidance 逐字钉死，这里引用同一常量保证 Join 顺序。
func TestParseRunFlagsValidationOrderSnapshot(t *testing.T) {
	isolateRunConfigEnv(t)
	_, err := parseRunFlags([]string{"--provider", "bogus", "--optimizer", "nope"})
	if err == nil {
		t.Fatal("expected joined usage errors")
	}
	want := strings.Join([]string{
		`--optimizer "nope" is not a registered paradigm (available: auto, evoprompt, gepa, miprov2, p1, protegi)`,
		`--provider must be openai or anthropic, got "bogus"`,
		`--optimizer only takes effect in the zero-config mode (the configured mode runs no optimization loop)`,
		`--task is required`,
		`--candidate is required`,
		`--dataset is required`,
		errRunBaseURLRequired.Error(),
		errRunModelRequired.Error(),
	}, "\n")
	if err.Error() != want {
		t.Errorf("joined errors drifted:\n got: %q\nwant: %q", err.Error(), want)
	}
}

// TestRunManifestNoFileInvariant is the D2 byte-level layer 3: an e2e
// run with no config file produces a manifest identical field-by-field
// to the pre-file golden (run_timeout_test.go pattern).
func TestRunManifestNoFileInvariant(t *testing.T) {
	isolateRunConfigEnv(t)
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := t.TempDir()
	code, stdout, stderr := runCliVerbose(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "fake-model", "--out", out, "--headless")
	if code != 0 {
		t.Fatalf("run exit = %d; stderr:\n%s", code, stderr)
	}
	// --headless stdout 保持纯 JSON。
	var summary map[string]any
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("headless stdout is not JSON: %v\n%s", err, stdout)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var mf runManifest
	loadJSONFile(t, filepath.Join(out, entries[0].Name(), "manifest.json"), &mf)
	// 易变字段（run id 与时间戳）置零后整结构体比对。
	mf.RunID, mf.CreatedAt = "", time.Time{}
	want := runManifest{
		Version: version,
		Task:    "tcm_test", Candidate: "baseline", Dataset: "ds",
		TaskPath: task, CandidatePath: cand, DatasetPath: ds,
		TaskKey: "task",
		Model:   "fake-model", BaseURL: srv.URL,
		MaxTokens: config.DefaultMaxTokens, Workers: config.DefaultWorkers,
		Reps:    1,
		Samples: 3, Provider: config.DefaultProvider,
	}
	if !reflect.DeepEqual(mf, want) {
		t.Errorf("manifest drifted from the no-file invariant:\n got %+v\nwant %+v", mf, want)
	}
}

// TestRunConfigFileMergesIntoManifest closes the loop end-to-end: the
// file tier alone supplies the connection identity (no --base-url/
// --model flags) and the merged values land in the manifest snapshot
// byte-identically to flag-provided ones (manifest carries no
// provenance by design; the key never persists).
func TestRunConfigFileMergesIntoManifest(t *testing.T) {
	isolateRunConfigEnv(t)
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := t.TempDir()
	writeRunFile(t, config.ProjectFileName, "base_url: "+srv.URL+`
model: file-model
api_key: file-key
max_tokens: 4096
rps: 2.5
judge_max_tokens: 256
timeout: 90
`, 0o600)
	code, _, stderr := runCliVerbose(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--out", out, "--headless")
	if code != 0 {
		t.Fatalf("run exit = %d; stderr:\n%s", code, stderr)
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var mf runManifest
	loadJSONFile(t, filepath.Join(out, entries[0].Name(), "manifest.json"), &mf)
	if mf.BaseURL != srv.URL || mf.Model != "file-model" || mf.MaxTokens != 4096 ||
		mf.RPS != 2.5 || mf.JudgeMaxTokens != 256 || mf.TimeoutSeconds != 90 {
		t.Errorf("manifest did not take the file tier: %+v", mf)
	}
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.Contains(line, "file-key") {
			t.Errorf("stderr leaked the file api_key: %s", line)
		}
	}
}

// TestValidationHelpers 单键校验 helper 自身的表驱动钉子（parseRunFlags
// 与切分 2 的 config set 共用同一语义）。
func TestValidationHelpers(t *testing.T) {
	if err := validateProvider("openai"); err != nil {
		t.Errorf("validateProvider(openai) = %v", err)
	}
	if err := validateProvider("bogus"); err == nil {
		t.Error("validateProvider(bogus) must error")
	}
	if err := validateOptimizer("auto"); err != nil {
		t.Errorf("validateOptimizer(auto) = %v", err)
	}
	if err := validateOptimizer("gepa"); err != nil {
		t.Errorf("validateOptimizer(gepa) = %v", err)
	}
	if err := validateJudgeProvider(""); err != nil {
		t.Errorf("empty judge provider must pass, got %v", err)
	}
	if err := validateJudgeProvider("anthropic"); err != nil {
		t.Errorf("validateJudgeProvider(anthropic) = %v", err)
	}
	if err := validateJudgeBackend("decision"); err != nil {
		t.Errorf("validateJudgeBackend(decision) = %v", err)
	}
	if err := validateJudgeBackend("nope"); err == nil {
		t.Error("validateJudgeBackend(nope) must error")
	}
	if d, err := validateTimeout("90"); err != nil || d != 90*time.Second {
		t.Errorf("validateTimeout(90) = %v, %v", d, err)
	}
	if _, err := validateTimeout("junk"); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("validateTimeout(junk) = %v, want a --timeout usage error", err)
	}
	if err := validateMetricsList([]string{"llm_judge", "f1"}); err != nil {
		t.Errorf("validateMetricsList(llm_judge,f1) = %v", err)
	}
	for _, bad := range [][]string{
		{"llm_judge", "llm_judge"},
		{"not_a_metric"},
		{"f1", ""},
	} {
		if err := validateMetricsList(bad); err == nil {
			t.Errorf("validateMetricsList(%v) must error", bad)
		}
	}
	if err := validateMetricsList(nil); err != nil {
		t.Errorf("empty list is legal (LLM 自选), got %v", err)
	}
}

// TestConfigFilePermWarningSurfaces pins the D8④ plumbing end to end:
// a discovered file with a loose permission carrying api_key surfaces
// one stderr warning line through parseRunFlags (0600 stays silent).
func TestConfigFilePermWarningSurfaces(t *testing.T) {
	isolateRunConfigEnv(t)
	task, cand, ds := fixtureYAMLs(t)
	args := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://x", "--model", "m"}
	t.Run("0644 warns on stderr", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "api_key: sk-x\n", 0o644)
		_, stderr := captureCliStderr(t, runCommand, args...)
		if !strings.Contains(stderr, "权限为 0644") || !strings.Contains(stderr, "api_key") {
			t.Errorf("stderr lacks the permission warning:\n%s", stderr)
		}
	})
	t.Run("0600 stays silent", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "api_key: sk-x\n", 0o600)
		_, stderr := captureCliStderr(t, runCommand, args...)
		if strings.Contains(stderr, "权限为") {
			t.Errorf("0600 must not warn:\n%s", stderr)
		}
	})
}

package main

// config 子命令测试（PRD-0001 切分 2 验收清单）：九问向导非 tty httptest
// 式覆盖（io.Reader 注入逐行喂答案）、`-` 前置读取顺序切口（R2 #2）、
// init 覆盖保护（R2 #4）、set/unset 写回语义（R2 #4）、list 输出双通道
// 与恒掩码（R1 #4/D8①）、单键校验复用 P1 helper（跨键红线）、权限与
// gitignore 闸（D8②③）、D5 新文案快照（R2 #3）。全部只碰磁盘与内存
// 管道，绝不触网。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
)

// withConfigStdin swaps the config subcommand's stdin injection point
// for one test（仿 mcp 的 In 注入；测试串行执行，Cleanup 恢复）。
func withConfigStdin(t *testing.T, s string) {
	t.Helper()
	old := configStdin
	configStdin = strings.NewReader(s)
	t.Cleanup(func() { configStdin = old })
}

// initCli runs configInit with the given stdin feed and stderr captured.
func initCli(t *testing.T, feed string, args ...string) (int, string) {
	t.Helper()
	return captureCliStderr(t, func(a []string) int {
		return configInit(a, strings.NewReader(feed))
	}, args...)
}

// setCli runs configSet with the given stdin feed and stderr captured.
func setCli(t *testing.T, feed string, args ...string) (int, string) {
	t.Helper()
	return captureCliStderr(t, func(a []string) int {
		return configSet(a, strings.NewReader(feed))
	}, args...)
}

// emptyFeed 是 19 个空行答案（全问回车 = 全默认）。
const emptyFeed = "\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n"

// wizardFullFeed 是九问（含 7/8 两问的全部子问）的完整作答序。
const wizardFullFeed = "http://x/v1\nm1\nsk-wiz\nanthropic\np1\nllm_judge,f1\n" +
	"yes\nopenai\nhttp://j/v1\njm\njsk\n512\n" +
	"decision\nhttp://d:9000\ndm\n0.7\n0.55\n" +
	"45\n2.5\n"

func TestConfigCommandDispatch(t *testing.T) {
	isolateRunConfigEnv(t)
	if code, _ := captureCliStderr(t, configCommand); code != exitFailure {
		t.Errorf("no args: exit = %d, want 1", code)
	}
	if code, stderr := captureCliStderr(t, configCommand, "bogus"); code != exitFailure ||
		!strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("unknown sub: exit = %d stderr = %q", code, stderr)
	}
	if code, _ := captureCli(t, configCommand, "help"); code != exitOK {
		t.Errorf("help: exit = %d, want 0", code)
	}
	if code, _ := captureCli(t, configCommand, "-h"); code != exitOK {
		t.Errorf("-h: exit = %d, want 0", code)
	}
}

// TestConfigInitWizardFullLineProtocol 是验收切口「九问序各键落位」：
// io.Reader 注入逐行喂答案，问题序（base_url → model → api_key →
// provider → optimizer → spec_metrics → 裁判 yes+五子项 → decision+四
// 子项 → timeout/rps）各键写到位。
func TestConfigInitWizardFullLineProtocol(t *testing.T) {
	isolateRunConfigEnv(t)
	target := filepath.Join(t.TempDir(), "cfg.yaml")
	code, stderr := initCli(t, wizardFullFeed, "--config", target)
	if code != exitOK {
		t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
	}
	f, err := config.Load(target)
	if err != nil {
		t.Fatalf("written file does not strict-load: %v", err)
	}
	want := config.File{
		BaseURL: "http://x/v1", Model: "m1", APIKey: "sk-wiz",
		Provider: "anthropic", Optimizer: "p1",
		SpecMetrics: []string{"llm_judge", "f1"},
		Timeout:     "45", RPS: 2.5,
		JudgeProvider: "openai", JudgeBaseURL: "http://j/v1", JudgeModel: "jm",
		JudgeAPIKey: "jsk", JudgeMaxTokens: 512,
		JudgeBackend: "decision", JudgeDecisionURL: "http://d:9000",
		JudgeDecisionModel: "dm", JudgeDecisionConfidence: 0.7, JudgeDecisionDiagBelow: 0.55,
	}
	if !reflect.DeepEqual(*f, want) {
		t.Errorf("wizard file = %+v\nwant          %+v", *f, want)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("written file perm = %v (%v), want 0600", info.Mode(), err)
	}
}

// TestConfigInitEmptyAndEOFDefaults 钉两态：空行取默认（19 个空行 → 零
// 键文件）；EOF 余下全默认（喂两行后断流 → 只落 base_url/model）。
func TestConfigInitEmptyAndEOFDefaults(t *testing.T) {
	t.Run("empty lines take defaults", func(t *testing.T) {
		isolateRunConfigEnv(t)
		target := filepath.Join(t.TempDir(), "cfg.yaml")
		code, stderr := initCli(t, emptyFeed, "--config", target)
		if code != exitOK {
			t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
		}
		f, err := config.Load(target)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !reflect.DeepEqual(*f, config.File{}) {
			t.Errorf("all-defaults file = %+v, want zero", *f)
		}
	})
	t.Run("EOF fills the rest with defaults", func(t *testing.T) {
		isolateRunConfigEnv(t)
		target := filepath.Join(t.TempDir(), "cfg.yaml")
		code, stderr := initCli(t, "http://x\nm1", "--config", target) // 无尾部换行也取到
		if code != exitOK {
			t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
		}
		f, err := config.Load(target)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		want := config.File{BaseURL: "http://x", Model: "m1"}
		if !reflect.DeepEqual(*f, want) {
			t.Errorf("partial feed file = %+v, want %+v", *f, want)
		}
	})
}

// TestConfigInitStdinDashPreRead 是 R2 #2 钉死的读取顺序切口：`-` 通道
// 值在向导开始前从 stdin 前置读取（读至首个换行），九问行协议从剩余
// stdin 继续——`init --api-key -` 喂 "key\nhttp://x\nm1\n" 必须得到
// api_key=key、base_url=http://x、model=m1（前置读取不吞后续行）。
func TestConfigInitStdinDashPreRead(t *testing.T) {
	isolateRunConfigEnv(t)
	target := filepath.Join(t.TempDir(), "cfg.yaml")
	code, stderr := initCli(t, "key\nhttp://x\nm1\n", "--config", target, "--api-key", "-")
	if code != exitOK {
		t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
	}
	f, err := config.Load(target)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.APIKey != "key" || f.BaseURL != "http://x" || f.Model != "m1" {
		t.Errorf("pre-read misplaced the answers: api_key=%q base_url=%q model=%q, want key/http://x/m1",
			f.APIKey, f.BaseURL, f.Model)
	}

	// 两个 `-` 按问题序前置读取（api_key → judge_api_key），行协议随后。
	t.Run("both dash keys pre-read in question order", func(t *testing.T) {
		isolateRunConfigEnv(t)
		target := filepath.Join(t.TempDir(), "cfg.yaml")
		code, stderr := initCli(t, "k1\njkey\nhttp://x\nm1\n",
			"--config", target, "--api-key", "-", "--judge-api-key", "-")
		if code != exitOK {
			t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
		}
		f, err := config.Load(target)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if f.APIKey != "k1" || f.JudgeAPIKey != "jkey" || f.BaseURL != "http://x" || f.Model != "m1" {
			t.Errorf("double pre-read drifted: %+v", f)
		}
	})
}

// TestConfigInitOverwriteProtection 是 R2 #4 覆盖保护切口：目标已存在 →
// stderr 列出将被覆盖的键，非 tty 无 --force 拒绝退出 1 且文件逐字节不
// 变；--force 放行；损坏目标同样要求 --force。
func TestConfigInitOverwriteProtection(t *testing.T) {
	existing := "model: old-m\njudge_api_key: old-j\n"
	t.Run("non-tty without --force refuses and keeps bytes", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, existing, 0o600)
		code, stderr := initCli(t, emptyFeed, "--scope", "project")
		if code != exitFailure {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		for _, frag := range []string{"将覆盖以下已有键", "model", "judge_api_key", "非 tty"} {
			if !strings.Contains(stderr, frag) {
				t.Errorf("stderr lacks %q:\n%s", frag, stderr)
			}
		}
		if b, _ := os.ReadFile(config.ProjectFileName); string(b) != existing {
			t.Errorf("file changed after refusal: %q", string(b))
		}
	})
	t.Run("--force overwrites", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, existing, 0o600)
		feed := "http://new/v1\nnew-m\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n\n"
		code, stderr := initCli(t, feed, "--scope", "project", "--force")
		if code != exitOK {
			t.Fatalf("exit = %d; stderr:\n%s", code, stderr)
		}
		f, err := config.Load(config.ProjectFileName)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if f.BaseURL != "http://new/v1" || f.Model != "new-m" || f.JudgeAPIKey != "" {
			t.Errorf("forced overwrite did not replace the keys: %+v", f)
		}
	})
	t.Run("corrupt target demands --force", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "base_url: [unclosed\n", 0o600)
		code, stderr := initCli(t, emptyFeed, "--scope", "project")
		if code != exitFailure || !strings.Contains(stderr, "--force") {
			t.Errorf("corrupt target: exit = %d stderr = %q, want refusal naming --force", code, stderr)
		}
	})
}

// TestConfigInitPermsAndGitignore 钉 D8②③：user 级写入目录 0700、文件
// 0600（stat 断言）；project 作用域含密钥 → .gitignore 幂等追加（重复
// 执行不重复追加）；.gitignore 不存在 → 只打印指引不代建。
func TestConfigInitPermsAndGitignore(t *testing.T) {
	t.Run("user scope dir 0700 file 0600", func(t *testing.T) {
		isolateRunConfigEnv(t)
		feed := "http://x\nm1\nsk-1\n" + strings.Repeat("\n", 16)
		code, stderr := initCli(t, feed)
		if code != exitOK {
			t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
		}
		target := userRunConfigPath(t)
		if info, err := os.Stat(filepath.Dir(target)); err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("user dir perm = %v (%v), want 0700", info.Mode(), err)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("user file perm = %v (%v), want 0600", info.Mode(), err)
		}
	})
	t.Run("project scope with key appends gitignore idempotently", func(t *testing.T) {
		isolateRunConfigEnv(t)
		if err := os.WriteFile(".gitignore", []byte("runs/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		feed := "http://x\nm1\nsk-1\n" + strings.Repeat("\n", 16)
		if code, stderr := initCli(t, feed, "--scope", "project"); code != exitOK {
			t.Fatalf("init exit = %d; stderr:\n%s", code, stderr)
		}
		want := "runs/\npromptopt.yaml\n"
		if b, _ := os.ReadFile(".gitignore"); string(b) != want {
			t.Fatalf(".gitignore = %q, want %q", string(b), want)
		}
		// 重复执行（config set 再次写密钥）不重复追加。
		if code, stderr := setCli(t, "", "api_key", "sk-2", "--scope", "project"); code != exitOK {
			t.Fatalf("set exit = %d; stderr:\n%s", code, stderr)
		}
		if b, _ := os.ReadFile(".gitignore"); string(b) != want {
			t.Errorf(".gitignore after repeat = %q, want unchanged %q", string(b), want)
		}
	})
	t.Run("missing gitignore prints guidance only", func(t *testing.T) {
		isolateRunConfigEnv(t)
		code, stderr := setCli(t, "", "api_key", "sk-1", "--scope", "project")
		if code != exitOK {
			t.Fatalf("set exit = %d; stderr:\n%s", code, stderr)
		}
		if _, err := os.Stat(".gitignore"); !os.IsNotExist(err) {
			t.Errorf(".gitignore must not be created by proxy (stat err = %v)", err)
		}
		if !strings.Contains(stderr, ".gitignore") {
			t.Errorf("stderr lacks the guidance:\n%s", stderr)
		}
	})
}

// TestConfigSetUnsetWriteBack 是 R2 #4 写回语义切口：损坏文件 + set →
// 退出 1、逐字节不变；set 保留其它键与注释；unset 幂等成功；`-` 整读
// stdin。
func TestConfigSetUnsetWriteBack(t *testing.T) {
	t.Run("corrupt file refuses and keeps bytes", func(t *testing.T) {
		isolateRunConfigEnv(t)
		corrupt := "base_url: [unclosed\n"
		writeRunFile(t, config.ProjectFileName, corrupt, 0o600)
		code, stderr := setCli(t, "", "model", "x", "--scope", "project")
		if code != exitFailure {
			t.Fatalf("exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		if b, _ := os.ReadFile(config.ProjectFileName); string(b) != corrupt {
			t.Errorf("file changed after refusal: %q", string(b))
		}
		if !strings.Contains(stderr, config.ProjectFileName) {
			t.Errorf("error lacks the path:\n%s", stderr)
		}
	})
	t.Run("set preserves other keys and comments", func(t *testing.T) {
		isolateRunConfigEnv(t)
		good := "# 我的手工注释\nbase_url: http://keep/v1\nmodel: old\n"
		writeRunFile(t, config.ProjectFileName, good, 0o600)
		if code, stderr := setCli(t, "", "model", "new", "--scope", "project"); code != exitOK {
			t.Fatalf("set exit = %d; stderr:\n%s", code, stderr)
		}
		b, _ := os.ReadFile(config.ProjectFileName)
		for _, frag := range []string{"# 我的手工注释", "http://keep/v1", "model: new"} {
			if !strings.Contains(string(b), frag) {
				t.Errorf("written file lacks %q:\n%s", frag, string(b))
			}
		}
		if _, err := config.Load(config.ProjectFileName); err != nil {
			t.Errorf("written file does not strict-load: %v", err)
		}
	})
	t.Run("unset is idempotent then removes", func(t *testing.T) {
		isolateRunConfigEnv(t)
		writeRunFile(t, config.ProjectFileName, "model: m\nbase_url: http://x/v1\n", 0o600)
		if code, _ := captureCliStderr(t, configUnset, "optimizer", "--scope", "project"); code != exitOK {
			t.Errorf("unset of an absent key must succeed idempotently")
		}
		if b, _ := os.ReadFile(config.ProjectFileName); !strings.Contains(string(b), "model: m") {
			t.Errorf("idempotent unset touched the file: %q", string(b))
		}
		if code, _ := captureCliStderr(t, configUnset, "model", "--scope", "project"); code != exitOK {
			t.Errorf("unset of an existing key failed")
		}
		f, err := config.Load(config.ProjectFileName)
		if err != nil || f.Model != "" || f.BaseURL != "http://x/v1" {
			t.Errorf("after unset: %+v (%v)", f, err)
		}
	})
	t.Run("dash value reads all of stdin", func(t *testing.T) {
		isolateRunConfigEnv(t)
		code, stderr := setCli(t, "sk-from-stdin\n", "api_key", "-", "--scope", "project")
		if code != exitOK {
			t.Fatalf("set exit = %d; stderr:\n%s", code, stderr)
		}
		f, err := config.Load(config.ProjectFileName)
		if err != nil || f.APIKey != "sk-from-stdin" {
			t.Fatalf("api_key = %q (%v), want the stdin value", f.APIKey, err)
		}
		if !strings.Contains(stderr, maskText) || strings.Contains(stderr, "sk-from-stdin") {
			t.Errorf("set confirmation leaked the key:\n%s", stderr)
		}
	})
	t.Run("empty value is refused", func(t *testing.T) {
		isolateRunConfigEnv(t)
		if code, _ := setCli(t, "", "model", "", "--scope", "project"); code != exitFailure {
			t.Errorf("empty value must be refused")
		}
	})
}

// TestConfigSetSingleKeyValidation 钉「仅单键校验、复用 P1 helper」：
// provider 白名单 / judge_backend 枚举 / timeout 文法 / metrics 白名单
// 等各一正一反。
func TestConfigSetSingleKeyValidation(t *testing.T) {
	cases := []struct {
		key, value string
		wantOK     bool
	}{
		{"provider", "anthropic", true},
		{"provider", "bogus", false},
		{"judge_backend", "decision", true},
		{"judge_backend", "oracle", false},
		{"timeout", "90", true},
		{"timeout", "junk", false},
		{"spec_metrics", "f1", true},
		{"spec_metrics", "f1,bogus", false},
		{"evo_variant", "de", true},
		{"evo_variant", "dx", false},
		{"rps", "2.5", true},
		{"rps", "-1", false},
		{"judge_decision_confidence", "0.7", true},
		{"judge_decision_confidence", "1.5", false},
		{"max_tokens", "512", true},
		{"max_tokens", "0", false},
		{"nope", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			isolateRunConfigEnv(t)
			code, stderr := setCli(t, "", tc.key, tc.value, "--scope", "project")
			if tc.wantOK && code != exitOK {
				t.Fatalf("exit = %d; stderr:\n%s", code, stderr)
			}
			if !tc.wantOK && code != exitFailure {
				t.Errorf("exit = %d, want 1", code)
			}
		})
	}
}

// TestConfigSetCrossKeyDefersToRun 是跨键红线切口：judge_backend=decision
// 缺 url/model 在 set 成功（config set 永不拦跨键完整性），在 run 汇聚
// 点报 D5 新文案。
func TestConfigSetCrossKeyDefersToRun(t *testing.T) {
	isolateRunConfigEnv(t)
	code, stderr := setCli(t, "", "judge_backend", "decision", "--scope", "project")
	if code != exitOK {
		t.Fatalf("set must not check cross-key completeness: exit = %d; stderr:\n%s", code, stderr)
	}
	task, cand, ds := fixtureYAMLs(t)
	_, err := parseRunFlags([]string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://x", "--model", "m"})
	if err == nil {
		t.Fatal("run must refuse decision without url/model")
	}
	if !errors.Is(err, errRunDecisionRequired) {
		t.Errorf("run error lacks the D5 decision guidance: %v", err)
	}
	if !strings.Contains(err.Error(), "config set judge_decision_url") {
		t.Errorf("D5 decision text lacks the config-key pointer: %v", err)
	}
}

// TestConfigListChannels 钉 list 双通道（R1 #4）：人类可读走 stderr、
// 首行 # config: 命中路径 + # shadowed 行、api_key/judge_api_key 恒掩
// 码；--headless JSON 走 stdout（同掩码、无密钥泄漏）。
func TestConfigListChannels(t *testing.T) {
	isolateRunConfigEnv(t)
	writeRunFile(t, config.ProjectFileName,
		"base_url: http://file/v1\napi_key: sk-file-key\njudge_api_key: sk-judge-key\n", 0o600)
	writeRunFile(t, userRunConfigPath(t), "model: user-m\n", 0o600)
	t.Setenv(config.EnvModel, "env-m") // env 层压过两层文件

	t.Run("human readable on stderr", func(t *testing.T) {
		code, stderr := captureCliStderr(t, configList)
		if code != exitOK {
			t.Fatalf("list exit = %d", code)
		}
		for _, frag := range []string{
			"# config: " + config.ProjectFileName,
			"# shadowed: ",
			userRunConfigPath(t),
			"env-m",
			maskText,
			"来源", // 来源列头（tabwriter 展开后的表格）
		} {
			if !strings.Contains(stderr, frag) {
				t.Errorf("stderr lacks %q:\n%s", frag, stderr)
			}
		}
		if strings.Contains(stderr, "sk-file-key") || strings.Contains(stderr, "sk-judge-key") {
			t.Errorf("stderr leaked a secret:\n%s", stderr)
		}
	})
	t.Run("headless JSON on stdout", func(t *testing.T) {
		code, stdout := captureCli(t, configList, "--headless")
		if code != exitOK {
			t.Fatalf("list exit = %d", code)
		}
		var out struct {
			ConfigPath string   `json:"config_path"`
			Shadowed   []string `json:"shadowed"`
			Keys       map[string]struct {
				Value  any    `json:"value"`
				Source string `json:"source"`
				Flag   string `json:"flag"`
			} `json:"keys"`
		}
		if err := json.Unmarshal([]byte(stdout), &out); err != nil {
			t.Fatalf("headless stdout is not JSON: %v\n%s", err, stdout)
		}
		if out.ConfigPath != config.ProjectFileName || len(out.Shadowed) != 1 {
			t.Errorf("provenance drifted: path=%q shadowed=%v", out.ConfigPath, out.Shadowed)
		}
		if k := out.Keys["api_key"]; k.Value != maskText || k.Source != "file" || k.Flag != "--api-key" {
			t.Errorf("api_key row = %+v", k)
		}
		if k := out.Keys["model"]; k.Value != "env-m" || k.Source != "env" {
			t.Errorf("model row = %+v, want env-m/env", k)
		}
		if k := out.Keys["provider"]; k.Value != "openai" || k.Source != "default" {
			t.Errorf("provider row = %+v, want openai/default", k)
		}
		if strings.Contains(stdout, "sk-file-key") || strings.Contains(stdout, "sk-judge-key") {
			t.Errorf("stdout leaked a secret:\n%s", stdout)
		}
	})
}

// TestConfigGet 钉 get：单键生效值 + 来源 + file 命中路径；掩码；未知
// 键报错列全键集。
func TestConfigGet(t *testing.T) {
	isolateRunConfigEnv(t)
	writeRunFile(t, config.ProjectFileName, "model: file-m\napi_key: sk-secret\n", 0o600)
	code, stderr := captureCliStderr(t, configGet, "model")
	if code != exitOK ||
		!strings.Contains(stderr, "model = file-m") ||
		!strings.Contains(stderr, "来源=file") ||
		!strings.Contains(stderr, config.ProjectFileName) ||
		!strings.Contains(stderr, "--model") {
		t.Errorf("get model: exit = %d stderr = %q", code, stderr)
	}
	code, stderr = captureCliStderr(t, configGet, "api_key")
	if code != exitOK || !strings.Contains(stderr, maskText) || strings.Contains(stderr, "sk-secret") {
		t.Errorf("get api_key must mask: exit = %d stderr = %q", code, stderr)
	}
	code, stderr = captureCliStderr(t, configGet, "nope")
	if code != exitFailure || !strings.Contains(stderr, "未知键") || !strings.Contains(stderr, "base_url") {
		t.Errorf("unknown key: exit = %d stderr = %q", code, stderr)
	}
	if code, _ := captureCliStderr(t, configGet); code != exitFailure {
		t.Errorf("no key: exit = %d, want 1", code)
	}
}

// TestD5MissingParamGuidance 是 R2 #3 文案快照：六条缺参指路文案逐字
// 钉死（独立于源码常量的字面量比对），并断言 run/verify/decision 三处
// 退出码均为 1。
func TestD5MissingParamGuidance(t *testing.T) {
	t.Run("run base_url three routes", func(t *testing.T) {
		want := "--base-url is required. 可通过三种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt run --base-url http://localhost:8080/v1 ...\n" +
			"  2. 环境变量：export PROMPTOPT_BASE_URL=http://localhost:8080/v1\n" +
			"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）"
		if got := errRunBaseURLRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("run model three routes", func(t *testing.T) {
		want := "--model is required. 可通过三种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt run --model <model-name> ...\n" +
			"  2. 环境变量：export PROMPTOPT_MODEL=<model-name>\n" +
			"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）"
		if got := errRunModelRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("run decision config-key pointer", func(t *testing.T) {
		want := "--judge-backend decision requires --judge-decision-url and --judge-decision-model. 可通过三种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt run --judge-decision-url http://decision-svc:9000 --judge-decision-model <model>\n" +
			"  2. 环境变量：export PROMPTOPT_JUDGE_DECISION_URL=... PROMPTOPT_JUDGE_DECISION_MODEL=...\n" +
			"  3. 配置文件：promptopt config set judge_decision_url <url> 与 judge_decision_model <model>"
		if got := errRunDecisionRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("verify base_url four routes", func(t *testing.T) {
		want := "--base-url is required. 可通过四种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt verify <run_id> --base-url http://localhost:8080/v1\n" +
			"  2. 环境变量：export PROMPTOPT_BASE_URL=http://localhost:8080/v1\n" +
			"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）\n" +
			"  4. run manifest 快照：manifest.json 含 base_url 时自动复现现场"
		if got := errVerifyBaseURLRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("verify model four routes", func(t *testing.T) {
		want := "--model is required. 可通过四种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt verify <run_id> --model <model-name>\n" +
			"  2. 环境变量：export PROMPTOPT_MODEL=<model-name>\n" +
			"  3. 配置文件：promptopt config init（或手写 promptopt.yaml）\n" +
			"  4. run manifest 快照：manifest.json 含 model 时自动复现现场"
		if got := errVerifyModelRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})
	t.Run("verify decision config-key pointer", func(t *testing.T) {
		want := "--judge-backend decision 需要 --judge-decision-url 与 --judge-decision-model. 可通过四种途径配置（任选其一）：\n" +
			"  1. 命令行 flag：promptopt verify <run_id> --judge-decision-url http://decision-svc:9000 --judge-decision-model <model>\n" +
			"  2. 环境变量：export PROMPTOPT_JUDGE_DECISION_URL=... PROMPTOPT_JUDGE_DECISION_MODEL=...\n" +
			"  3. 配置文件：promptopt config set judge_decision_url <url> 与 judge_decision_model <model>\n" +
			"  4. run manifest 快照：manifest.json 含 judge_decision_url/judge_decision_model 时自动复现现场"
		if got := errVerifyDecisionRequired.Error(); got != want {
			t.Errorf("text drifted:\n got: %q\nwant: %q", got, want)
		}
	})

	// run e2e：缺 base_url/model → 退出码 1，stderr 带三途径文案。
	t.Run("run exits 1 with the guidance", func(t *testing.T) {
		isolateRunConfigEnv(t)
		code, stderr := captureCliStderr(t, runCommand, "--task", "t", "--candidate", "c", "--dataset", "d")
		if code != exitFailure {
			t.Fatalf("run exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		for _, frag := range []string{"三种途径", "promptopt config init", "export PROMPTOPT_BASE_URL"} {
			if !strings.Contains(stderr, frag) {
				t.Errorf("stderr lacks %q:\n%s", frag, stderr)
			}
		}
	})
	// run decision 缺参 → 退出码 1（config set 已成功的跨键场景在 run 汇聚点报）。
	t.Run("run decision exits 1", func(t *testing.T) {
		isolateRunConfigEnv(t)
		code, stderr := captureCliStderr(t, runCommand, "--task", "t", "--candidate", "c",
			"--dataset", "d", "--base-url", "http://x", "--model", "m", "--judge-backend", "decision")
		if code != exitFailure || !strings.Contains(stderr, "config set judge_decision_url") {
			t.Errorf("decision exit = %d stderr:\n%s", code, stderr)
		}
	})

	// verify 侧：resolveVerifyConn 返回的正是四途径文案常量（verify.go
	// 的错误打印点以 exitFailure 落地）；decision 缺参带 config 键指引。
	t.Run("verify four routes", func(t *testing.T) {
		clearVerifyConnEnv(t)
		_, err := resolveVerifyConn(verifyOptions{}, runManifest{})
		if err == nil || err.Error() != errVerifyBaseURLRequired.Error() {
			t.Errorf("verify missing conn err = %v", err)
		}
		if _, err := resolveVerifyConn(verifyOptions{judgeBackend: "decision"},
			runManifest{BaseURL: "b", Model: "m"}); err != errVerifyDecisionRequired {
			t.Errorf("verify decision err = %v, want the exact sentinel", err)
		}
	})
	// verify e2e：手工搭一棵 manifest 无连接参数的 run 树，verify 命令
	// 退出码 1 且 stderr 带四途径文案（verify.go 的 resolveVerifyConn
	// 错误打印点）。
	t.Run("verify exits 1 with the guidance e2e", func(t *testing.T) {
		isolateRunConfigEnv(t)
		base := t.TempDir()
		runsDir := filepath.Join(base, "runs")
		runID := "20261009-000000-d5tst"
		runDir := filepath.Join(runsDir, runID)
		for _, d := range []string{runDir, filepath.Join(base, "synth", runID)} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		files := map[string]string{
			filepath.Join(runDir, "run.json"):                `{"status":"completed","exit_code":0}`,
			filepath.Join(runDir, "manifest.json"):           `{"version":"test"}`,
			filepath.Join(base, "synth", runID, "spec.json"): `{"task":{"name":"t","prompt_template":"baseline 提示词 {input}","metrics":["exact_match"]},"probes":[]}`,
		}
		frontier := `{"primary":"f1","best":{"id":"cand","means":{"f1":1},"prompt":"优化后提示词 {input}"},` +
			`"members":[{"candidate":{"id":"cand","prompt":"优化后提示词 {input}"},"scores":[1],"means":{"f1":1},"round":1,"operator":"baseline"}],` +
			`"generated_at":"2026-10-09T00:00:00Z"}`
		files[filepath.Join(runDir, "frontier.json")] = frontier
		for path, content := range files {
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		code, stderr := verifyCliStderr(t, runID, "--runs-dir", runsDir)
		if code != exitFailure {
			t.Fatalf("verify exit = %d, want 1; stderr:\n%s", code, stderr)
		}
		for _, frag := range []string{"四种途径", "run manifest 快照", "promptopt config init"} {
			if !strings.Contains(stderr, frag) {
				t.Errorf("stderr lacks %q:\n%s", frag, stderr)
			}
		}
	})
}

// TestConfigKeyOrderMatchesRegistry 钉 configKeyOrder 与 File 字段序的
// 顺序一致——展示/写出序漂移即测试红（登记表完整性由 config 包的
// TestKeyRegistryReflection 另行把守）。
func TestConfigKeyOrderMatchesRegistry(t *testing.T) {
	typ := reflect.TypeFor[config.File]()
	tags := make([]string, 0, typ.NumField())
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		tags = append(tags, name)
	}
	if !slices.Equal(tags, configKeyOrder) {
		t.Errorf("configKeyOrder drifted from the File field order:\n got %v\nwant %v", configKeyOrder, tags)
	}
	if duplicates := len(tags) - len(slices.Compact(slices.Sorted(slices.Values(tags)))); duplicates != 0 {
		t.Errorf("configKeyOrder carries %d duplicates", duplicates)
	}
}

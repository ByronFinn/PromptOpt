package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// isolateConfigEnv 把环境收敛到「无任何配置文件」的确定性状态：临时
// 工作目录（层 3 永不命中）、临时 HOME 与空 XDG_CONFIG_HOME（层 4 永不
// 命中，路径本身由 os.UserConfigDir() 按 GOOS 推导——切口不写死单一
// 路径，R2 #6②）、清空 PROMPTOPT_CONFIG 与全部 PROMPTOPT_* env。
// t.Chdir 不得与 t.Parallel 同用（Go 1.24+ 直接 panic，漏隔离炸测试而
// 非静默污染——按设计保持）。
func isolateConfigEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv(EnvConfig, "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		EnvBaseURL, EnvModel, EnvAPIKey, EnvOutDir, EnvTimeout,
		EnvJudgeProvider, EnvJudgeModel, EnvJudgeBaseURL, EnvJudgeAPIKey,
		EnvJudgeMaxTokens, EnvJudgeDecisionURL, EnvJudgeDecisionModel,
	} {
		t.Setenv(k, "")
	}
}

// userConfigPath returns the GOOS-correct user-level config file path
// for the isolated HOME.
func userConfigPath(t *testing.T) string {
	t.Helper()
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	return filepath.Join(dir, "promptopt", "config.yaml")
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

const fullFileYAML = `# 完整键面冒烟
provider: anthropic
base_url: http://file.example/v1
model: file-model
api_key: sk-file
max_tokens: 512
timeout: 300
rps: 2.5
extra_body:
  chat_template_kwargs:
    enable_thinking: false
out: file-runs
judge_provider: openai
judge_base_url: http://judge.example/v1
judge_model: judge-model
judge_api_key: sk-judge
judge_max_tokens: 256
judge_backend: decision
judge_decision_url: http://decision.example
judge_decision_model: tev1
judge_decision_confidence: 0.7
judge_decision_diag_below: 0.55
optimizer: p1
evo_variant: de
spec_metrics: [llm_judge, f1]
`

// TestLoad pins the strict decode contract: unknown keys, type errors
// and syntax errors all fail with the path and the YAML line position;
// the documented full key set decodes; bare-second timeouts decode via
// the scalarTimeout; an empty file is an empty config, not corruption.
func TestLoad(t *testing.T) {
	isolateConfigEnv(t)

	t.Run("full key set decodes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, fullFileYAML, 0o600)
		f, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		want := File{
			Provider: "anthropic", BaseURL: "http://file.example/v1", Model: "file-model",
			APIKey: "sk-file", MaxTokens: 512, Timeout: "300", RPS: 2.5,
			ExtraBody:     map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
			Out:           "file-runs",
			JudgeProvider: "openai", JudgeBaseURL: "http://judge.example/v1",
			JudgeModel: "judge-model", JudgeAPIKey: "sk-judge", JudgeMaxTokens: 256,
			JudgeBackend: "decision", JudgeDecisionURL: "http://decision.example",
			JudgeDecisionModel: "tev1", JudgeDecisionConfidence: 0.7, JudgeDecisionDiagBelow: 0.55,
			Optimizer: "p1", EvoVariant: "de", SpecMetrics: []string{"llm_judge", "f1"},
		}
		if !reflect.DeepEqual(*f, want) {
			t.Errorf("Load = %+v\nwant %+v", *f, want)
		}
		if d, err := ParseTimeout(string(f.Timeout)); err != nil || d != 300_000_000_000 {
			t.Fatalf("file timeout 300 must parse as bare seconds: %v %v", d, err)
		}
	})

	t.Run("unknown key fails with path and line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, "baseurl: http://typo.example/v1\n", 0o600)
		_, err := Load(path)
		if err == nil {
			t.Fatal("unknown key must hard-error (KnownFields(true))")
		}
		for _, frag := range []string{path, "line 1", "baseurl"} {
			if !strings.Contains(err.Error(), frag) {
				t.Errorf("error %q lacks %q", err, frag)
			}
		}
	})

	t.Run("type error fails with line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, "max_tokens: \"abc\"\n", 0o600)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("type error = %v, want a line-positioned failure", err)
		}
	})

	t.Run("syntax error fails with line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, "base_url: [unclosed\n", 0o600)
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("syntax error = %v, want the file path", err)
		}
	})

	t.Run("timeout must be a scalar", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, "timeout: [90]\n", 0o600)
		if _, err := Load(path); err == nil {
			t.Error("non-scalar timeout must fail")
		}
	})

	t.Run("empty file is an empty config", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeFile(t, path, "", 0o600)
		f, err := Load(path)
		if err != nil {
			t.Fatalf("empty file: %v", err)
		}
		if !reflect.DeepEqual(*f, File{}) {
			t.Errorf("empty file decoded to %+v, want zero", *f)
		}
	})

	t.Run("missing file errors with path", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
		if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
			t.Errorf("missing file = %v", err)
		}
	})
}

// TestDiscoverNone is the D2 byte-level precondition: with every layer
// absent, Discover hits nothing and reports no shadowed files.
func TestDiscoverNone(t *testing.T) {
	isolateConfigEnv(t)
	d, err := Discover("")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Path != "" || d.File != nil || len(d.Shadowed) != 0 || d.Warning != "" {
		t.Errorf("discovery = %+v, want an empty hit", d)
	}
}

// TestDiscoverLayers drives the four-layer discovery order table-driven
// (PRD-0001 D1): the first EXISTING file wins, lower existing layers
// become the shadowed list (existence-only), explicit layers are strict
// about existence, implicit layers are silent.
func TestDiscoverLayers(t *testing.T) {
	cases := []struct {
		name string
		// 每层放置器：返回 YAML 内容（"" = 不放置该层）。
		flagCfg, envCfg, projectCfg, userCfg string
		flagPath                             func(t *testing.T) string
		wantPath                             func(t *testing.T) string
		wantShadowed                         int
	}{
		{
			name:     "no layers hits nothing",
			wantPath: func(*testing.T) string { return "" },
		},
		{
			name:         "project beats user",
			projectCfg:   "model: project-m\n",
			userCfg:      "model: user-m\n",
			wantPath:     func(*testing.T) string { return ProjectFileName },
			wantShadowed: 1,
		},
		{
			name:         "env beats project and user",
			envCfg:       "model: env-m\n",
			projectCfg:   "model: project-m\n",
			userCfg:      "model: user-m\n",
			wantPath:     func(*testing.T) string { return os.Getenv(EnvConfig) },
			wantShadowed: 2,
		},
		{
			name:         "flag beats everything",
			flagCfg:      "model: flag-m\n",
			envCfg:       "model: env-m\n",
			projectCfg:   "model: project-m\n",
			userCfg:      "model: user-m\n",
			wantPath:     func(*testing.T) string { return os.Getenv("cfgPath") },
			wantShadowed: 3,
		},
		{
			name:         "user alone hits",
			userCfg:      "model: user-m\n",
			wantPath:     userConfigPath,
			wantShadowed: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnv(t)
			if tc.flagCfg != "" {
				p := filepath.Join(t.TempDir(), "flag.yaml")
				writeFile(t, p, tc.flagCfg, 0o600)
				t.Setenv("cfgPath", p) // 经由 t.Flag 传给 wantPath
			}
			if tc.envCfg != "" {
				p := filepath.Join(t.TempDir(), "env.yaml")
				writeFile(t, p, tc.envCfg, 0o600)
				t.Setenv(EnvConfig, p)
			}
			if tc.projectCfg != "" {
				writeFile(t, ProjectFileName, tc.projectCfg, 0o600)
			}
			if tc.userCfg != "" {
				writeFile(t, userConfigPath(t), tc.userCfg, 0o600)
			}
			explicit := ""
			if tc.flagCfg != "" {
				explicit = os.Getenv("cfgPath")
			}
			d, err := Discover(explicit)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if want := tc.wantPath(t); d.Path != want {
				t.Errorf("hit path = %q, want %q", d.Path, want)
			}
			if len(d.Shadowed) != tc.wantShadowed {
				t.Errorf("shadowed = %v, want %d entries", d.Shadowed, tc.wantShadowed)
			}
		})
	}
}

// TestDiscoverCorruptHitNeverFallsThrough is the R1 #1 遮蔽切口: a
// syntactically broken ./promptopt.yaml with a GOOD user-level file
// must hard-error — 存在性决定命中，命中后的可解析性决定生死，项目级
// 损坏文件不得被用户级好文件静默遮蔽. The user-level path is placed per
// runtime.GOOS via os.UserConfigDir() (R2 #6②).
func TestDiscoverCorruptHitNeverFallsThrough(t *testing.T) {
	isolateConfigEnv(t)
	writeFile(t, ProjectFileName, "base_url: [unclosed\n", 0o600)
	writeFile(t, userConfigPath(t), "model: user-m\nbase_url: http://user/v1\n", 0o600)
	_, err := Discover("")
	if err == nil {
		t.Fatal("corrupt hit must hard-error, not fall through to the user file")
	}
	if !strings.Contains(err.Error(), ProjectFileName) {
		t.Errorf("error = %v, want the corrupt file path", err)
	}
}

// TestDiscoverShadowedIsExistenceOnly pins the R2 #6① semantics: a
// CORRUPT shadowed file never affects Discover — only the hit file
// carries the hard-error duty. The shadowed list still names it.
func TestDiscoverShadowedIsExistenceOnly(t *testing.T) {
	isolateConfigEnv(t)
	// env 层命中（好文件），项目级与用户级都损坏——只登记存在性。
	envPath := filepath.Join(t.TempDir(), "env.yaml")
	writeFile(t, envPath, "model: env-m\n", 0o600)
	t.Setenv(EnvConfig, envPath)
	writeFile(t, ProjectFileName, "base_url: [unclosed\n", 0o600)
	writeFile(t, userConfigPath(t), "完全不合法的 {{ yaml", 0o600)

	d, err := Discover("")
	if err != nil {
		t.Fatalf("corrupt shadowed files must not fail Discover: %v", err)
	}
	if d.Path != envPath {
		t.Errorf("hit = %q, want the env layer", d.Path)
	}
	if !slices.Contains(d.Shadowed, ProjectFileName) {
		t.Errorf("shadowed = %v, want the corrupt project file listed", d.Shadowed)
	}
}

// TestDiscoverExplicitStrict pins the explicit-tier asymmetry: --config
// pointing at a missing file is a hard error (a failed claim must
// answer), while an absent implicit layer is silently skipped.
func TestDiscoverExplicitStrict(t *testing.T) {
	isolateConfigEnv(t)
	if _, err := Discover(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("explicit missing file must hard-error")
	}
	t.Setenv(EnvConfig, filepath.Join(t.TempDir(), "env-missing.yaml"))
	if _, err := Discover(""); err == nil {
		t.Error("PROMPTOPT_CONFIG pointing at a missing file must hard-error")
	}
	t.Setenv(EnvConfig, "")
	if d, err := Discover(""); err != nil {
		t.Fatalf("absent implicit layers must stay silent: %v", err)
	} else if d.Path != "" {
		t.Errorf("hit = %q, want none", d.Path)
	}
}

// TestDiscoverTildeExpansion pins the ~ expansion of the explicit path.
func TestDiscoverTildeExpansion(t *testing.T) {
	isolateConfigEnv(t)
	home := os.Getenv("HOME")
	writeFile(t, filepath.Join(home, "cfg.yaml"), "model: home-m\n", 0o600)
	d, err := Discover("~/cfg.yaml")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Path != filepath.Join(home, "cfg.yaml") || d.File.Model != "home-m" {
		t.Errorf("discovery = %s/%v, want the expanded home path", d.Path, d.File)
	}
}

// TestPermissionWarning pins the D8④ bitwise gate: only
// perm & 0o077 != 0 warns (0600 stays silent — an integer comparison
// would flag our own output on every read), and only files carrying a
// key warn.
func TestPermissionWarning(t *testing.T) {
	isolateConfigEnv(t)
	cases := []struct {
		name    string
		yaml    string
		perm    os.FileMode
		wantHit bool
	}{
		{name: "0600 with key stays silent", yaml: "api_key: sk-x\n", perm: 0o600},
		{name: "0644 with key warns", yaml: "api_key: sk-x\n", perm: 0o644, wantHit: true},
		{name: "0644 judge key warns", yaml: "judge_api_key: sk-j\n", perm: 0o644, wantHit: true},
		{name: "0644 without key stays silent", yaml: "model: m\n", perm: 0o644},
		{name: "0700 file perm (exec bit only) stays silent", yaml: "api_key: sk-x\n", perm: 0o700},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnv(t)
			writeFile(t, ProjectFileName, tc.yaml, tc.perm)
			d, err := Discover("")
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if got := d.Warning != ""; got != tc.wantHit {
				t.Errorf("warning = %q, wantHit = %v", d.Warning, tc.wantHit)
			}
			if d.Warning != "" && !strings.Contains(d.Warning, ProjectFileName) {
				t.Errorf("warning %q lacks the file path", d.Warning)
			}
		})
	}
}

// TestDeriveSources pins the per-key source collector: flag (from the
// fs.Visit-derived explicit set) > env (only when the value actually
// parses) > file (non-zero field) > default. This map is the single
// collector the merge decision and the explicitness marks derive from.
func TestDeriveSources(t *testing.T) {
	file := &File{
		BaseURL: "http://file/v1", Model: "file-m", RPS: 2.5,
		JudgeMaxTokens: 512, Timeout: "300", Optimizer: "p1",
	}
	t.Run("file tier", func(t *testing.T) {
		isolateConfigEnv(t)
		ks := DeriveSources(map[string]bool{}, file)
		for _, key := range []string{"base_url", "model", "rps", "judge_max_tokens", "timeout", "optimizer"} {
			if ks[key] != SourceFile {
				t.Errorf("sources[%s] = %q, want file", key, ks[key])
			}
		}
		if ks["provider"] != SourceDefault || ks["api_key"] != SourceDefault {
			t.Errorf("absent keys must read default, got provider=%s api_key=%s", ks["provider"], ks["api_key"])
		}
	})
	t.Run("flag tier wins", func(t *testing.T) {
		isolateConfigEnv(t)
		ks := DeriveSources(map[string]bool{"base-url": true, "rps": true, "optimizer": true}, file)
		if ks["base_url"] != SourceFlag || ks["rps"] != SourceFlag || ks["optimizer"] != SourceFlag {
			t.Errorf("explicit flags must read flag, got %+v", ks)
		}
	})
	t.Run("env tier", func(t *testing.T) {
		isolateConfigEnv(t)
		t.Setenv(EnvModel, "env-m")
		t.Setenv(EnvJudgeMaxTokens, "111")
		ks := DeriveSources(map[string]bool{}, file)
		if ks["model"] != SourceEnv || ks["judge_max_tokens"] != SourceEnv {
			t.Errorf("env keys must read env, got %+v", ks)
		}
	})
	t.Run("invalid env does not claim the env tier", func(t *testing.T) {
		isolateConfigEnv(t)
		t.Setenv(EnvJudgeMaxTokens, "abc")
		t.Setenv(EnvTimeout, "junk")
		ks := DeriveSources(map[string]bool{}, file)
		if ks["judge_max_tokens"] != SourceFile || ks["timeout"] != SourceFile {
			t.Errorf("malformed env must fall through to file, got judge_max_tokens=%s timeout=%s",
				ks["judge_max_tokens"], ks["timeout"])
		}
	})
	t.Run("no env layer keys never read env", func(t *testing.T) {
		isolateConfigEnv(t)
		t.Setenv(EnvOutDir, "env-runs") // out 有 env；provider 没有
		ks := DeriveSources(map[string]bool{}, &File{Out: "file-runs"})
		if ks["out"] != SourceEnv {
			t.Errorf("out with env set must read env, got %s", ks["out"])
		}
		if ks["provider"] != SourceDefault {
			t.Errorf("provider has no env layer, got %s", ks["provider"])
		}
	})
	t.Run("nil file reads default", func(t *testing.T) {
		isolateConfigEnv(t)
		ks := DeriveSources(map[string]bool{}, nil)
		for key := range keyRegistry {
			if ks[key] != SourceDefault {
				t.Errorf("sources[%s] = %s, want default (nil file)", key, ks[key])
			}
		}
	})
}

// TestKeyRegistryReflection is the R2 #6③ completeness gate: every
// File field's yaml tag must be registered in keyRegistry, and the two
// contract columns must carry exactly the documented sets — the 10
// explicitness keys (D2) and the 11 verify connection-identity
// whitelist keys (R2 #1). A new file key without a registry row (or a
// registry row without a field) turns this test red.
func TestKeyRegistryReflection(t *testing.T) {
	tagKeys := map[string]bool{}
	typ := reflect.TypeFor[File]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
		if name == "" {
			t.Fatalf("field %s lacks a yaml tag", typ.Field(i).Name)
		}
		tagKeys[name] = true
		if _, ok := keyRegistry[name]; !ok {
			t.Errorf("File field %q (yaml %s) is missing from keyRegistry — 新增文件键忘登记（R2 #6③）", typ.Field(i).Name, name)
		}
	}
	for key := range keyRegistry {
		if !tagKeys[key] {
			t.Errorf("keyRegistry key %q has no File field", key)
		}
	}

	explicitWant := []string{
		"provider", "max_tokens", "rps", "extra_body", "optimizer",
		"evo_variant", "spec_metrics", "judge_max_tokens",
		"judge_decision_confidence", "judge_decision_diag_below",
	}
	whitelistWant := []string{
		"provider", "base_url", "model", "api_key", "timeout",
		"judge_provider", "judge_base_url", "judge_model", "judge_api_key",
		"judge_decision_url", "judge_decision_model",
	}
	var explicitGot, whitelistGot []string
	for key, spec := range keyRegistry {
		if spec.ExplicitFlag {
			explicitGot = append(explicitGot, key)
		}
		if spec.VerifyWhitelist {
			whitelistGot = append(whitelistGot, key)
		}
	}
	slices.Sort(explicitGot)
	slices.Sort(whitelistGot)
	slices.Sort(explicitWant)
	slices.Sort(whitelistWant)
	if !slices.Equal(explicitGot, explicitWant) {
		t.Errorf("explicitness keys = %v, want the D2 ten-key set %v", explicitGot, explicitWant)
	}
	if !slices.Equal(whitelistGot, whitelistWant) {
		t.Errorf("verify whitelist = %v, want the R2 #1 eleven-key set %v", whitelistGot, whitelistWant)
	}
}

// TestExpandHome pins the ~ expansion rules: bare ~, ~/path, and the
// unsupported ~user form passing through unchanged.
func TestExpandHome(t *testing.T) {
	isolateConfigEnv(t)
	home := os.Getenv("HOME")
	cases := []struct{ in, want string }{
		{"", ""},
		{"~/x.yaml", filepath.Join(home, "x.yaml")},
		{"~", home},
		{"~user/x.yaml", "~user/x.yaml"},
		{"/abs/path", "/abs/path"},
	}
	for _, tc := range cases {
		if got := expandHome(tc.in); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

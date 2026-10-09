// 配置文件层（PRD-0001 切分 1）：YAML 发现、严格解码与逐键来源收集。
//
// 设计要点（D1/D2/D3/D8）：
//   - 发现序 --config > PROMPTOPT_CONFIG > ./promptopt.yaml >
//     os.UserConfigDir()/promptopt/config.yaml，首个**存在**的文件命中
//     即止、不跨文件合并；存在性决定命中，命中后的可解析性决定生死
//     （损坏硬错，绝不跳发现链——防项目级损坏文件被用户级好文件静默遮蔽）。
//   - 命中即严格：语法错 / 类型错 / 未知键（KnownFields(true)）一律报错，
//     错误信息带文件路径与行位置。文件是书写的持久意图，静默忽略 =
//     程序以用户不认可的语义烧预算。
//   - shadowed（被遮蔽文件）仅标注存在性：os.Stat 命中即入列，不读内容、
//     不校验、不解析——被遮蔽文件损坏不影响 Discover/list，命中文件才负
//     硬错义务。
//   - 文件只承载环境身份（这台机器跟哪个 LLM 说话、用什么范式），不承载
//     单次实验意图（预算/轮数/温度等成本行为旋钮绝不入文件，D3）。
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvConfig is the config-file discovery layer right below --config
// (precedence: --config > PROMPTOPT_CONFIG > ./promptopt.yaml > the
// user-level file). An empty value reads as unset.
const EnvConfig = "PROMPTOPT_CONFIG"

// ProjectFileName is the project-level config file discovered in the
// working directory (a plain, non-dot file).
const ProjectFileName = "promptopt.yaml"

// UserLevelPath returns the user-level config file path: discovery
// layer 4 and the config subcommand's default user-scope write target.
// os.UserConfigDir() respects XDG_CONFIG_HOME on Linux and lands in
// ~/Library/Application Support on macOS.
func UserLevelPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "promptopt", "config.yaml"), nil
}

// ExpandHome exposes the ~ expansion for the config subcommand's
// explicit write targets — the same rule discovery layer 1 applies.
func ExpandHome(p string) string {
	return expandHome(p)
}

// EnvKeyFor returns a file key's PROMPTOPT_* env binding ("" when the
// key has no env layer) — the read-side companion of the registry for
// the config subcommand's list/get provenance display.
func EnvKeyFor(key string) string {
	return keyRegistry[key].EnvKey
}

// TimeoutValue carries the file-side per-attempt timeout. The documented
// grammar is ParseTimeout's — Go durations ("90s") AND bare seconds
// ("300") — so the field must accept YAML scalars of either shape; a
// plain string field would hard-reject the documented `timeout: 300`.
// Grammar validation stays with ParseTimeout at the merge site (a
// malformed value is a usage error, not a silent unset).
type TimeoutValue string

// UnmarshalYAML accepts any scalar (int seconds or duration string) and
// stores it verbatim; non-scalars stay a decode error (strict layer).
func (t *TimeoutValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: timeout 必须是标量（Go 时长如 90s 或裸秒数如 300），got %s", node.Line, node.ShortTag())
	}
	*t = TimeoutValue(node.Value)
	return nil
}

// File is the strict decode target of one config file: the 22
// environment-identity keys of D3. Zero values read as "key absent" —
// the merge tier falls through to the next layer (default).
type File struct {
	Provider                string         `yaml:"provider"`
	BaseURL                 string         `yaml:"base_url"`
	Model                   string         `yaml:"model"`
	APIKey                  string         `yaml:"api_key"`
	MaxTokens               int            `yaml:"max_tokens"`
	Timeout                 TimeoutValue   `yaml:"timeout"`
	RPS                     float64        `yaml:"rps"`
	ExtraBody               map[string]any `yaml:"extra_body"`
	Out                     string         `yaml:"out"`
	JudgeProvider           string         `yaml:"judge_provider"`
	JudgeBaseURL            string         `yaml:"judge_base_url"`
	JudgeModel              string         `yaml:"judge_model"`
	JudgeAPIKey             string         `yaml:"judge_api_key"`
	JudgeMaxTokens          int            `yaml:"judge_max_tokens"`
	JudgeBackend            string         `yaml:"judge_backend"`
	JudgeDecisionURL        string         `yaml:"judge_decision_url"`
	JudgeDecisionModel      string         `yaml:"judge_decision_model"`
	JudgeDecisionConfidence float64        `yaml:"judge_decision_confidence"`
	JudgeDecisionDiagBelow  float64        `yaml:"judge_decision_diag_below"`
	Optimizer               string         `yaml:"optimizer"`
	EvoVariant              string         `yaml:"evo_variant"`
	SpecMetrics             []string       `yaml:"spec_metrics"`
}

// Load strictly decodes one config file. Syntax errors, type errors and
// unknown keys (KnownFields(true)) are all errors carrying the file
// path and the YAML line position. An empty (or comment-only) file
// decodes to an empty config rather than erroring — "no keys" is a
// valid expression of "nothing configured".
func Load(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("配置文件 %s: %w", path, err)
	}
	defer f.Close()
	var file File
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	err = dec.Decode(&file)
	switch {
	case errors.Is(err, io.EOF):
		// 空文件 = 无键配置，不算损坏。
	case err != nil:
		return nil, fmt.Errorf("配置文件 %s: %w", path, err)
	}
	return &file, nil
}

// Discovery aggregates one Discover pass: the hit path ("" when no
// layer exists), the lower-priority files that exist but were shadowed
// (existence-only, see package comment), the strictly decoded File
// (nil when no hit) and a human-readable permission warning (D8④).
type Discovery struct {
	Path     string
	Shadowed []string
	File     *File
	Warning  string
}

// Discover runs the four-layer discovery (D1): --config > PROMPTOPT_CONFIG
// > ./promptopt.yaml > os.UserConfigDir()/promptopt/config.yaml. The
// first EXISTING file wins; its decodability decides life or death —
// a corrupt hit is a hard error, never a fall-through to a lower layer.
// Explicit layers (--config, PROMPTOPT_CONFIG) are strict about
// existence too: a path that cannot be stat-ed is an error, because the
// operator explicitly claimed "the config is here".
func Discover(explicit string) (Discovery, error) {
	type layer struct {
		path     string
		explicit bool
	}
	var layers []layer
	if p := expandHome(explicit); p != "" {
		layers = append(layers, layer{p, true})
	}
	if p := expandHome(os.Getenv(EnvConfig)); p != "" {
		layers = append(layers, layer{p, true})
	}
	layers = append(layers, layer{ProjectFileName, false})
	if p, err := UserLevelPath(); err == nil {
		layers = append(layers, layer{p, false})
	}

	for i, l := range layers {
		info, err := os.Stat(l.path)
		if err != nil {
			if l.explicit {
				return Discovery{}, fmt.Errorf("配置文件 %s: %w", l.path, err)
			}
			continue // 隐含层缺失是常态：静默跳过。
		}
		file, err := Load(l.path)
		if err != nil {
			return Discovery{}, err
		}
		// 命中即止；其后仍然存在的层仅登记存在性（不读内容、不校验、
		// 不解析——被遮蔽文件损坏不影响发现，见包注释）。
		var shadowed []string
		for _, low := range layers[i+1:] {
			if _, err := os.Stat(low.path); err == nil {
				shadowed = append(shadowed, low.path)
			}
		}
		return Discovery{
			Path:     l.path,
			Shadowed: shadowed,
			File:     file,
			Warning:  permWarning(l.path, info, file),
		}, nil
	}
	return Discovery{}, nil
}

// permWarning is the D8④ read-permission warning: when the discovered
// file carries a key and its group/other permission bits are non-zero
// (perm & 0o077 != 0 — bitwise, NOT an integer comparison, which would
// flag our own 0600 output every read), return one human-readable
// warning line. A warning never fails the read: CI umasks vary widely.
func permWarning(path string, info os.FileInfo, file *File) string {
	if file.APIKey == "" && file.JudgeAPIKey == "" {
		return ""
	}
	if perm := info.Mode().Perm(); perm&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("配置文件 %s 含 api_key 且权限为 %04o（group/other 位非零，同机他用户可读），建议 chmod 600 收紧",
		path, info.Mode().Perm())
}

// expandHome expands a leading ~ with os.UserHomeDir(). A bare "~"
// becomes the home directory itself; "~user" forms are unsupported and
// pass through unchanged (an explicit layer then fails its stat with
// the raw path, which is the honest error).
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	if len(p) > 1 && p[1] != '/' && p[1] != filepath.Separator {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if len(p) == 1 {
		return home
	}
	return filepath.Join(home, p[2:])
}

// KeySpec is one row of the merge registry (D2/D3): a file key's env
// binding and its two contract flags. The registry is the single table
// tests reflect against (R2 #6③: a new File field without a registry
// row turns the reflection test red), and DeriveSources reads it.
type KeySpec struct {
	// EnvKey is the PROMPTOPT_* fallback for this key; empty = no env
	// layer (mirrors the D3 table's env column).
	EnvKey string
	// ExplicitFlag marks the 10 explicitness keys: an explicitly given
	// flag (including an explicit 0 / empty value) is terminal and must
	// not be overridden by the file tier.
	ExplicitFlag bool
	// VerifyWhitelist marks the 11 connection-identity keys verify may
	// backfill from the file when the run manifest omits them (R2 #1).
	// Behavior keys (extra_body/rps/judge_max_tokens/judge_backend/the
	// two thresholds) are never backfilled — backfilling them would
	// certify a scenario the run never executed.
	VerifyWhitelist bool
}

// keyRegistry is the merge registry: YAML key → merge semantics. YAML
// keys are flag names with '-' replaced by '_'.
var keyRegistry = map[string]KeySpec{
	"provider":                  {ExplicitFlag: true, VerifyWhitelist: true},
	"base_url":                  {EnvKey: EnvBaseURL, VerifyWhitelist: true},
	"model":                     {EnvKey: EnvModel, VerifyWhitelist: true},
	"api_key":                   {EnvKey: EnvAPIKey, VerifyWhitelist: true},
	"max_tokens":                {ExplicitFlag: true},
	"timeout":                   {EnvKey: EnvTimeout, VerifyWhitelist: true},
	"rps":                       {ExplicitFlag: true},
	"extra_body":                {ExplicitFlag: true},
	"out":                       {EnvKey: EnvOutDir},
	"judge_provider":            {EnvKey: EnvJudgeProvider, VerifyWhitelist: true},
	"judge_base_url":            {EnvKey: EnvJudgeBaseURL, VerifyWhitelist: true},
	"judge_model":               {EnvKey: EnvJudgeModel, VerifyWhitelist: true},
	"judge_api_key":             {EnvKey: EnvJudgeAPIKey, VerifyWhitelist: true},
	"judge_max_tokens":          {EnvKey: EnvJudgeMaxTokens, ExplicitFlag: true},
	"judge_backend":             {},
	"judge_decision_url":        {EnvKey: EnvJudgeDecisionURL, VerifyWhitelist: true},
	"judge_decision_model":      {EnvKey: EnvJudgeDecisionModel, VerifyWhitelist: true},
	"judge_decision_confidence": {ExplicitFlag: true},
	"judge_decision_diag_below": {ExplicitFlag: true},
	"optimizer":                 {ExplicitFlag: true},
	"evo_variant":               {ExplicitFlag: true},
	"spec_metrics":              {ExplicitFlag: true},
}

// fileFieldIndex maps YAML keys onto File field indexes, built once by
// reflection; fileValueSet reads through it.
var fileFieldIndex = func() map[string]int {
	m := make(map[string]int, reflect.TypeFor[File]().NumField())
	t := reflect.TypeFor[File]()
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" {
			m[name] = i
		}
	}
	return m
}()

// fileValueSet reports whether the File carries a non-zero value for
// the key ("key present" in merge terms).
func fileValueSet(fv reflect.Value, key string) bool {
	i, ok := fileFieldIndex[key]
	return ok && !fv.Field(i).IsZero()
}

// Source marks where one key's effective value came from.
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	SourceDefault Source = "default"
)

// KeySources records the per-key source of the effective value after a
// merge, one entry per registry key. It is the SINGLE collector: the 10
// explicitness marks derive from Source==flag, and the settings-page
// provenance display (切分 4) reuses the same map — parseRunFlags must
// not grow a second, parallel bookkeeping (R2 #5: two collectors drift,
// and the page starts lying).
type KeySources map[string]Source

// envTakes reports whether the key's env value actually participates in
// the chain — matching the resolver read semantics: a malformed or
// non-positive judge_max_tokens / timeout env reads as unset, so the
// source must not claim the env tier.
func envTakes(key, envKey string) bool {
	if envKey == "" {
		return false
	}
	v := os.Getenv(envKey)
	if v == "" {
		return false
	}
	switch key {
	case "judge_max_tokens":
		n, err := strconv.Atoi(v)
		return err == nil && n > 0
	case "timeout":
		d, err := ParseTimeout(v)
		return err == nil && d > 0
	}
	return true
}

// DeriveSources fills the per-key source map for one merge: flag (the
// fs.Visit-derived explicit set) > env (when the value actually parses)
// > file (non-zero field) > default. explicitFlags is the SAME map the
// merge itself consults — collector and merge decision stay same-origin.
func DeriveSources(explicitFlags map[string]bool, file *File) KeySources {
	ks := make(KeySources, len(keyRegistry))
	var fv reflect.Value
	if file != nil {
		fv = reflect.ValueOf(file).Elem()
	}
	for key, spec := range keyRegistry {
		switch {
		case explicitFlags[strings.ReplaceAll(key, "_", "-")]:
			ks[key] = SourceFlag
		case envTakes(key, spec.EnvKey):
			ks[key] = SourceEnv
		case file != nil && fileValueSet(fv, key):
			ks[key] = SourceFile
		default:
			ks[key] = SourceDefault
		}
	}
	return ks
}

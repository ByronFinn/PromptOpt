package config

import (
	"slices"
	"testing"
)

// TestResolvePrecedence pins the single resolution chain (PRD-0001 D2):
// flag > env > 文件 > 默认. The file tier slots between env and the
// default; an empty fileVal degrades to the old three-tier semantics.
func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		flagVal  string
		envVal   string // "" leaves the variable unset
		fileVal  string
		fallback string
		want     string
	}{
		{name: "flag wins", flagVal: "flag", envVal: "env", fileVal: "file", fallback: "def", want: "flag"},
		{name: "env beats file", flagVal: "", envVal: "env", fileVal: "file", fallback: "def", want: "env"},
		{name: "file beats default", flagVal: "", envVal: "", fileVal: "file", fallback: "def", want: "file"},
		{name: "fallback when all unset", flagVal: "", envVal: "", fileVal: "", fallback: "def", want: "def"},
		{name: "all empty", flagVal: "", envVal: "", fileVal: "", fallback: "", want: ""},
		{name: "empty file tier equals old three-tier", flagVal: "", envVal: "env", fileVal: "", fallback: "def", want: "env"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv("PROMPTOPT_TEST", tc.envVal)
			}
			if got := resolve(tc.flagVal, "PROMPTOPT_TEST", tc.fileVal, tc.fallback); got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolvers pins each string resolver's chain and default. The
// fileVal=="" rows double as the D2 byte-level parity layer: they assert
// the resolvers behave exactly like the pre-file three-tier chain.
func TestResolvers(t *testing.T) {
	t.Run("base url has no default", func(t *testing.T) {
		if got := BaseURL("", ""); got != "" {
			t.Errorf("BaseURL(\"\") = %q, want empty", got)
		}
	})

	t.Run("model has no default", func(t *testing.T) {
		if got := Model("", ""); got != "" {
			t.Errorf("Model(\"\") = %q, want empty", got)
		}
	})

	t.Run("api key defaults to 1", func(t *testing.T) {
		if got := APIKey("", ""); got != "1" {
			t.Errorf("APIKey(\"\") = %q, want %q", got, "1")
		}
	})

	t.Run("out dir defaults to runs", func(t *testing.T) {
		if got := OutDir("", ""); got != "runs" {
			t.Errorf("OutDir(\"\") = %q, want %q", got, "runs")
		}
	})

	t.Run("env fills base url", func(t *testing.T) {
		t.Setenv(EnvBaseURL, "http://env.example/v1")
		if got := BaseURL("", ""); got != "http://env.example/v1" {
			t.Errorf("BaseURL(\"\") = %q, want env value", got)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv(EnvModel, "env-model")
		if got := Model("flag-model", ""); got != "flag-model" {
			t.Errorf("Model() = %q, want flag value", got)
		}
	})

	t.Run("file tier table", func(t *testing.T) {
		t.Setenv(EnvBaseURL, "")
		t.Setenv(EnvAPIKey, "")
		t.Setenv(EnvOutDir, "")
		cases := []struct {
			name string
			call func() string
			want string
		}{
			{name: "base url file tier", call: func() string { return BaseURL("", "http://file/v1") }, want: "http://file/v1"},
			{name: "base url env beats file", call: func() string {
				t.Setenv(EnvBaseURL, "http://env/v1")
				defer t.Setenv(EnvBaseURL, "")
				return BaseURL("", "http://file/v1")
			}, want: "http://env/v1"},
			{name: "base url flag beats file", call: func() string { return BaseURL("http://flag/v1", "http://file/v1") }, want: "http://flag/v1"},
			{name: "api key file before default", call: func() string { return APIKey("", "file-key") }, want: "file-key"},
			{name: "out dir file tier", call: func() string { return OutDir("", "file-runs") }, want: "file-runs"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.call(); got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("explicitness keys are flagSet-terminal", func(t *testing.T) {
		cases := []struct {
			name string
			call func() string
			want string
		}{
			{name: "provider unset takes file", call: func() string { return Provider("", false, "anthropic") }, want: "anthropic"},
			{name: "provider unset no file takes default", call: func() string { return Provider("", false, "") }, want: DefaultProvider},
			{name: "provider explicit default beats file", call: func() string { return Provider(DefaultProvider, true, "anthropic") }, want: DefaultProvider},
			{name: "optimizer unset takes file", call: func() string { return Optimizer("", false, "p1") }, want: "p1"},
			{name: "optimizer explicit beats file", call: func() string { return Optimizer("gepa", true, "p1") }, want: "gepa"},
			{name: "evo variant unset takes file", call: func() string { return EvoVariant("", false, "de") }, want: "de"},
			{name: "evo variant explicit beats file", call: func() string { return EvoVariant("ga", true, "de") }, want: "ga"},
			{name: "judge backend file fills unset flag", call: func() string { return JudgeBackend("", "decision") }, want: "decision"},
			{name: "judge backend flag wins by value", call: func() string { return JudgeBackend("llm", "decision") }, want: "llm"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.call(); got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// TestJudgeResolvers pins the judge surface's flag > env > 文件 chain
// per resolver; the judge resolvers carry no default — an empty result
// means "fall back to the executor side" downstream.
func TestJudgeResolvers(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		tests := []struct {
			name    string
			envKey  string
			envVal  string
			flagVal string
			fileVal string
			want    string
		}{
			{name: "flag wins over env", envKey: EnvJudgeProvider, envVal: "anthropic", flagVal: "openai", want: "openai"},
			{name: "env fills unset flag", envKey: EnvJudgeModel, envVal: "judge-model", flagVal: "", want: "judge-model"},
			{name: "file fills unset flag and env", envKey: EnvJudgeModel, envVal: "", flagVal: "", fileVal: "file-judge-model", want: "file-judge-model"},
			{name: "env beats file", envKey: EnvJudgeBaseURL, envVal: "http://env", flagVal: "", fileVal: "http://file", want: "http://env"},
			{name: "empty when all unset (base url)", envKey: EnvJudgeBaseURL, envVal: "", flagVal: "", fileVal: "", want: ""},
			{name: "api key has no default", envKey: EnvJudgeAPIKey, envVal: "", flagVal: "", fileVal: "", want: ""},
			{name: "api key flag wins", envKey: EnvJudgeAPIKey, envVal: "env-key", flagVal: "flag-key", fileVal: "file-key", want: "flag-key"},
			{name: "api key file tier", envKey: EnvJudgeAPIKey, envVal: "", flagVal: "", fileVal: "file-key", want: "file-key"},
			{name: "decision url file tier", envKey: EnvJudgeDecisionURL, envVal: "", flagVal: "", fileVal: "http://file", want: "http://file"},
			{name: "decision model env beats file", envKey: EnvJudgeDecisionModel, envVal: "env-m", flagVal: "", fileVal: "file-m", want: "env-m"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.envVal != "" {
					t.Setenv(tc.envKey, tc.envVal)
				}
				var got string
				switch tc.envKey {
				case EnvJudgeProvider:
					got = JudgeProvider(tc.flagVal, tc.fileVal)
				case EnvJudgeModel:
					got = JudgeModel(tc.flagVal, tc.fileVal)
				case EnvJudgeBaseURL:
					got = JudgeBaseURL(tc.flagVal, tc.fileVal)
				case EnvJudgeAPIKey:
					got = JudgeAPIKey(tc.flagVal, tc.fileVal)
				case EnvJudgeDecisionURL:
					got = JudgeDecisionURL(tc.flagVal, tc.fileVal)
				case EnvJudgeDecisionModel:
					got = JudgeDecisionModel(tc.flagVal, tc.fileVal)
				default:
					t.Fatalf("unmapped env key %q", tc.envKey)
				}
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("max tokens table", func(t *testing.T) {
		tests := []struct {
			name    string
			envVal  string
			flagVal int
			flagSet bool
			fileVal int
			want    int
		}{
			// fileVal==0 + flagSet=false rows are the D2 byte-level
			// parity layer: they match the old three-tier semantics.
			{name: "flag wins", envVal: "111", flagVal: 32, flagSet: true, fileVal: 0, want: 32},
			{name: "env fills unset flag", envVal: "111", flagVal: 0, flagSet: false, fileVal: 0, want: 111},
			{name: "file fills unset flag and env", envVal: "", flagVal: 0, flagSet: false, fileVal: 512, want: 512},
			{name: "env beats file", envVal: "111", flagVal: 0, flagSet: false, fileVal: 512, want: 111},
			{name: "explicit flag beats env and file", envVal: "111", flagVal: 32, flagSet: true, fileVal: 512, want: 32},
			// R1 #2 切口：显式 0 = 回落 --max-tokens，终判压过文件。
			{name: "explicit zero beats file (R1 #2)", envVal: "", flagVal: 0, flagSet: true, fileVal: 512, want: 0},
			{name: "explicit zero beats env too", envVal: "111", flagVal: 0, flagSet: true, fileVal: 512, want: 0},
			{name: "unset reads zero", envVal: "", flagVal: 0, flagSet: false, fileVal: 0, want: 0},
			{name: "malformed env reads unset", envVal: "abc", flagVal: 0, flagSet: false, fileVal: 512, want: 512},
			{name: "non-positive env reads unset", envVal: "-5", flagVal: 0, flagSet: false, fileVal: 512, want: 512},
			{name: "non-positive file reads unset", envVal: "", flagVal: 0, flagSet: false, fileVal: -5, want: 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.envVal != "" {
					t.Setenv(EnvJudgeMaxTokens, tc.envVal)
				}
				if got := JudgeMaxTokens(tc.flagVal, tc.flagSet, tc.fileVal); got != tc.want {
					t.Errorf("JudgeMaxTokens(%d, %v, %d) = %d, want %d", tc.flagVal, tc.flagSet, tc.fileVal, got, tc.want)
				}
			})
		}
	})

	t.Run("decision thresholds are flagSet-terminal (R1 #2)", func(t *testing.T) {
		cases := []struct {
			name    string
			flagVal float64
			flagSet bool
			fileVal float64
			want    float64
		}{
			{name: "file fills unset flag", flagVal: 0, flagSet: false, fileVal: 0.7, want: 0.7},
			{name: "explicit zero beats file", flagVal: 0, flagSet: true, fileVal: 0.7, want: 0},
			{name: "explicit value wins", flagVal: 0.9, flagSet: true, fileVal: 0.7, want: 0.9},
			{name: "all unset", flagVal: 0, flagSet: false, fileVal: 0, want: 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := JudgeDecisionConfidence(tc.flagVal, tc.flagSet, tc.fileVal); got != tc.want {
					t.Errorf("JudgeDecisionConfidence = %v, want %v", got, tc.want)
				}
				if got := JudgeDecisionDiagBelow(tc.flagVal, tc.flagSet, tc.fileVal); got != tc.want {
					t.Errorf("JudgeDecisionDiagBelow = %v, want %v", got, tc.want)
				}
			})
		}
	})
}

// TestTypedResolvers pins the remaining explicitness-key resolvers
// (max tokens, rps, extra body) and the file parity of the timeout
// resolver.
func TestTypedResolvers(t *testing.T) {
	t.Run("max tokens", func(t *testing.T) {
		if got := MaxTokens(0, false, 0); got != DefaultMaxTokens {
			t.Errorf("MaxTokens(0,false,0) = %d, want default", got)
		}
		if got := MaxTokens(0, false, 512); got != 512 {
			t.Errorf("MaxTokens(0,false,512) = %d, want 512", got)
		}
		if got := MaxTokens(DefaultMaxTokens, true, 512); got != DefaultMaxTokens {
			t.Errorf("explicit default flag must be terminal, got %d", got)
		}
		if got := MaxTokens(0, false, -5); got != DefaultMaxTokens {
			t.Errorf("non-positive file reads unset, got %d", got)
		}
	})
	t.Run("rps explicit zero is terminal", func(t *testing.T) {
		if got := RPS(0, false, 2.5); got != 2.5 {
			t.Errorf("RPS unset flag + file = %v, want 2.5", got)
		}
		if got := RPS(0, true, 2.5); got != 0 {
			t.Errorf("explicit --rps 0 must beat the file, got %v", got)
		}
		if got := RPS(3, true, 2.5); got != 3 {
			t.Errorf("explicit flag wins, got %v", got)
		}
	})
	t.Run("extra body tiers", func(t *testing.T) {
		fileBody := map[string]any{"k": "file"}
		flagBody := map[string]any{"k": "flag"}
		if got := ExtraBody(nil, false, fileBody); got == nil {
			t.Error("unset flag + file must take the file map")
		}
		if got := ExtraBody(nil, true, fileBody); got != nil {
			t.Errorf("explicit empty flag must be terminal (nil), got %v", got)
		}
		if got := ExtraBody(flagBody, true, fileBody); got["k"] != "flag" {
			t.Errorf("explicit flag wins, got %v", got)
		}
	})
}

// TestSpecMetricsResolver pins the D7 synthesis-metrics pin resolver
// (PRD-0001 切分 3): flag > env（无）> 文件 > 默认（空 = LLM 自选），
// 显式性键语义与 Provider/Optimizer 同规——显式空列表（显式 LLM 自选）
// 同样终判压过文件层。
func TestSpecMetricsResolver(t *testing.T) {
	fileList := []string{"llm_judge", "f1"}
	t.Run("explicit flag list is terminal", func(t *testing.T) {
		got := SpecMetrics([]string{"exact_match"}, true, fileList)
		if !slices.Equal(got, []string{"exact_match"}) {
			t.Errorf("SpecMetrics(explicit) = %v, want the flag list", got)
		}
	})
	t.Run("explicit empty list beats the file (显式 LLM 自选)", func(t *testing.T) {
		got := SpecMetrics(nil, true, fileList)
		if len(got) != 0 {
			t.Errorf("SpecMetrics(explicit empty) = %v, want no override", got)
		}
	})
	t.Run("file tier fills an unset flag", func(t *testing.T) {
		got := SpecMetrics(nil, false, fileList)
		if !slices.Equal(got, fileList) {
			t.Errorf("SpecMetrics(unset) = %v, want the file list", got)
		}
	})
	t.Run("all unset reads LLM 自选 (nil)", func(t *testing.T) {
		if got := SpecMetrics(nil, false, nil); got != nil {
			t.Errorf("SpecMetrics(all unset) = %v, want nil", got)
		}
	})
}

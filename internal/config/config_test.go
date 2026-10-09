package config

import "testing"

func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name     string
		flagVal  string
		envVal   string // "" leaves the variable unset
		fallback string
		want     string
	}{
		{name: "flag wins", flagVal: "flag", envVal: "env", fallback: "def", want: "flag"},
		{name: "env beats default", flagVal: "", envVal: "env", fallback: "def", want: "env"},
		{name: "fallback when both unset", flagVal: "", envVal: "", fallback: "def", want: "def"},
		{name: "all empty", flagVal: "", envVal: "", fallback: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv("PROMPTOPT_TEST", tc.envVal)
			}
			if got := resolve(tc.flagVal, "PROMPTOPT_TEST", tc.fallback); got != tc.want {
				t.Errorf("resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolvers(t *testing.T) {
	t.Run("base url has no default", func(t *testing.T) {
		if got := BaseURL(""); got != "" {
			t.Errorf("BaseURL(\"\") = %q, want empty", got)
		}
	})

	t.Run("model has no default", func(t *testing.T) {
		if got := Model(""); got != "" {
			t.Errorf("Model(\"\") = %q, want empty", got)
		}
	})

	t.Run("api key defaults to 1", func(t *testing.T) {
		if got := APIKey(""); got != "1" {
			t.Errorf("APIKey(\"\") = %q, want %q", got, "1")
		}
	})

	t.Run("out dir defaults to runs", func(t *testing.T) {
		if got := OutDir(""); got != "runs" {
			t.Errorf("OutDir(\"\") = %q, want %q", got, "runs")
		}
	})

	t.Run("env fills base url", func(t *testing.T) {
		t.Setenv(EnvBaseURL, "http://env.example/v1")
		if got := BaseURL(""); got != "http://env.example/v1" {
			t.Errorf("BaseURL(\"\") = %q, want env value", got)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv(EnvModel, "env-model")
		if got := Model("flag-model"); got != "flag-model" {
			t.Errorf("Model() = %q, want flag value", got)
		}
	})
}

// TestJudgeResolvers pins the judge surface's flag > env > fallback
// chain per resolver; the judge resolvers carry no default — an empty
// result means "fall back to the executor side" downstream.
func TestJudgeResolvers(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		tests := []struct {
			name    string
			envKey  string
			envVal  string
			flagVal string
			want    string
		}{
			{name: "flag wins over env", envKey: EnvJudgeProvider, envVal: "anthropic", flagVal: "openai", want: "openai"},
			{name: "env fills unset flag", envKey: EnvJudgeModel, envVal: "judge-model", flagVal: "", want: "judge-model"},
			{name: "empty when both unset (base url)", envKey: EnvJudgeBaseURL, envVal: "", flagVal: "", want: ""},
			{name: "api key has no default", envKey: EnvJudgeAPIKey, envVal: "", flagVal: "", want: ""},
			{name: "api key flag wins", envKey: EnvJudgeAPIKey, envVal: "env-key", flagVal: "flag-key", want: "flag-key"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.envVal != "" {
					t.Setenv(tc.envKey, tc.envVal)
				}
				switch tc.envKey {
				case EnvJudgeProvider:
					if got := JudgeProvider(tc.flagVal); got != tc.want {
						t.Errorf("JudgeProvider() = %q, want %q", got, tc.want)
					}
				case EnvJudgeModel:
					if got := JudgeModel(tc.flagVal); got != tc.want {
						t.Errorf("JudgeModel() = %q, want %q", got, tc.want)
					}
				case EnvJudgeBaseURL:
					if got := JudgeBaseURL(tc.flagVal); got != tc.want {
						t.Errorf("JudgeBaseURL() = %q, want %q", got, tc.want)
					}
				case EnvJudgeAPIKey:
					if got := JudgeAPIKey(tc.flagVal); got != tc.want {
						t.Errorf("JudgeAPIKey() = %q, want %q", got, tc.want)
					}
				}
			})
		}
	})

	t.Run("max tokens table", func(t *testing.T) {
		tests := []struct {
			name    string
			envVal  string
			flagVal int
			want    int
		}{
			{name: "flag wins", envVal: "111", flagVal: 32, want: 32},
			{name: "env fills unset flag", envVal: "111", flagVal: 0, want: 111},
			{name: "unset reads zero", envVal: "", flagVal: 0, want: 0},
			{name: "malformed env reads unset", envVal: "abc", flagVal: 0, want: 0},
			{name: "non-positive env reads unset", envVal: "-5", flagVal: 0, want: 0},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if tc.envVal != "" {
					t.Setenv(EnvJudgeMaxTokens, tc.envVal)
				}
				if got := JudgeMaxTokens(tc.flagVal); got != tc.want {
					t.Errorf("JudgeMaxTokens(%d) = %d, want %d", tc.flagVal, got, tc.want)
				}
			})
		}
	})
}

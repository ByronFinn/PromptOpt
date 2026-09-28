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

package config

import (
	"testing"
	"time"
)

// TestParseTimeout pins the --timeout / PROMPTOPT_TIMEOUT grammar: Go
// duration syntax, bare seconds, empty = unset; malformed or
// non-positive values are usage errors (the flag has a real default
// and must fail loudly).
func TestParseTimeout(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"300", 300 * time.Second, false},
		{"90s", 90 * time.Second, false},
		{"2m30s", 150 * time.Second, false},
		{"1h", time.Hour, false},
		{"0", 0, true},
		{"0s", 0, true},
		{"-5", 0, true},
		{"-90s", 0, true},
		{"junk", 0, true},
		{"300 sec", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseTimeout(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTimeout(%q) = %v, nil; want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeout(%q) err = %v, want nil", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestTimeoutPrecedence pins the resolution chain: flag > env > 0 (the
// providers' DefaultTimeout). A malformed env value reads as unset —
// the optional surface must not fail runs that never asked for it.
func TestTimeoutPrecedence(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv(EnvTimeout, "45")
		if got := Timeout(30 * time.Second); got != 30*time.Second {
			t.Errorf("Timeout(30s) with env = %v, want 30s", got)
		}
	})
	t.Run("env fills bare seconds", func(t *testing.T) {
		t.Setenv(EnvTimeout, "45")
		if got := Timeout(0); got != 45*time.Second {
			t.Errorf("Timeout(0) with env 45 = %v, want 45s", got)
		}
	})
	t.Run("env fills duration syntax", func(t *testing.T) {
		t.Setenv(EnvTimeout, "2m")
		if got := Timeout(0); got != 2*time.Minute {
			t.Errorf("Timeout(0) with env 2m = %v, want 2m", got)
		}
	})
	t.Run("malformed env reads unset", func(t *testing.T) {
		t.Setenv(EnvTimeout, "junk")
		if got := Timeout(0); got != 0 {
			t.Errorf("Timeout(0) with junk env = %v, want 0", got)
		}
	})
	t.Run("all unset", func(t *testing.T) {
		t.Setenv(EnvTimeout, "")
		if got := Timeout(0); got != 0 {
			t.Errorf("Timeout(0) unset = %v, want 0 (constructor default applies)", got)
		}
	})
}

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
)

// --- --timeout / PROMPTOPT_TIMEOUT CLI surface (大基数实测修复) -------------

// TestParseRunFlagsTimeout pins the run-side grammar and precedence:
// Go durations and bare seconds, malformed values rejected up front,
// flag > PROMPTOPT_TIMEOUT env > unset (the providers' 180s default).
func TestParseRunFlagsTimeout(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}
	t.Setenv(config.EnvTimeout, "") // isolate from operator env

	for _, bad := range []string{"junk", "0", "-5", "300 sec"} {
		_, err := parseRunFlags(append(slices.Clone(base), "--timeout", bad))
		if err == nil || !strings.Contains(err.Error(), "--timeout") {
			t.Errorf("--timeout %q err = %v, want a usage error naming the flag", bad, err)
		}
	}

	o, err := parseRunFlags(base)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if o.timeout != 0 {
		t.Errorf("default timeout = %v, want 0 (constructor default 180s applies)", o.timeout)
	}

	o, err = parseRunFlags(append(slices.Clone(base), "--timeout", "90"))
	if err != nil {
		t.Fatalf("bare seconds: %v", err)
	}
	if o.timeout != 90*time.Second {
		t.Errorf("timeout 90 = %v, want 90s", o.timeout)
	}

	o, err = parseRunFlags(append(slices.Clone(base), "--timeout", "2m30s"))
	if err != nil {
		t.Fatalf("duration syntax: %v", err)
	}
	if o.timeout != 150*time.Second {
		t.Errorf("timeout 2m30s = %v, want 150s", o.timeout)
	}

	t.Setenv(config.EnvTimeout, "45")
	o, err = parseRunFlags(base)
	if err != nil {
		t.Fatalf("env tier: %v", err)
	}
	if o.timeout != 45*time.Second {
		t.Errorf("env timeout = %v, want 45s", o.timeout)
	}
	o, err = parseRunFlags(append(slices.Clone(base), "--timeout", "90"))
	if err != nil {
		t.Fatalf("flag beats env: %v", err)
	}
	if o.timeout != 90*time.Second {
		t.Errorf("flag over env = %v, want 90s", o.timeout)
	}
}

// TestResolveVerifyConnTimeout pins the verify-side layering:
// flag > PROMPTOPT_TIMEOUT env > manifest.timeout_seconds > unset.
func TestResolveVerifyConnTimeout(t *testing.T) {
	mf := runManifest{BaseURL: "http://manifest", Model: "m-model", TimeoutSeconds: 300}

	t.Run("manifest tier", func(t *testing.T) {
		t.Setenv(config.EnvTimeout, "")
		o, err := resolveVerifyConn(verifyOptions{}, mf)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if o.timeout != 300*time.Second {
			t.Errorf("timeout = %v, want the manifest 300s", o.timeout)
		}
	})
	t.Run("env over manifest", func(t *testing.T) {
		t.Setenv(config.EnvTimeout, "120")
		o, err := resolveVerifyConn(verifyOptions{}, mf)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if o.timeout != 120*time.Second {
			t.Errorf("timeout = %v, want the env 120s", o.timeout)
		}
	})
	t.Run("flag over env", func(t *testing.T) {
		t.Setenv(config.EnvTimeout, "120")
		o, err := resolveVerifyConn(verifyOptions{timeout: 60 * time.Second}, mf)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if o.timeout != 60*time.Second {
			t.Errorf("timeout = %v, want the flag 60s", o.timeout)
		}
	})
	t.Run("unset falls to constructor default", func(t *testing.T) {
		t.Setenv(config.EnvTimeout, "")
		o, err := resolveVerifyConn(verifyOptions{}, runManifest{BaseURL: "http://manifest", Model: "m-model"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if o.timeout != 0 {
			t.Errorf("timeout = %v, want 0 (the providers' 180s default applies)", o.timeout)
		}
	})
}

// TestRunTimeoutEndToEnd drives the full CLI seam: a slow gateway
// against --timeout 50ms fails the sample once per attempt (no 4×
// retry burn), the human-readable diagnosis points at the knob, and
// the manifest snapshots the configured deadline for verify.
func TestRunTimeoutEndToEnd(t *testing.T) {
	task := writeYAML(t, "to_task.yaml", `name: to_task
prompt_template: |
  answer {input}
metrics: [exact_match]
`)
	cand := writeYAML(t, "to_candidate.yaml", `id: baseline
prompt: |
  answer {input}
`)
	ds := writeYAML(t, "to_dataset.yaml", `name: to_ds
samples:
  - id: to-001
    input: 你好
    expected: "ok"
    split: test
`)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	t.Cleanup(srv.Close)

	out := t.TempDir()
	code, _, stderr := runCliVerbose(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "to-model", "--out", out, "--headless",
		"--timeout", "50ms")
	if code != 1 {
		t.Fatalf("run exit = %d, want 1 (evaluation failure); stderr %s", code, stderr)
	}
	// One attempt, one wall: no retry re-hits the deadline.
	if hits.Load() != 1 {
		t.Errorf("gateway hits = %d, want 1 (timeout must not retry)", hits.Load())
	}
	// The knob hint rides the sample error (events.jsonl error field),
	// not the stderr conclusion block.
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read run dir: %v (%v)", entries, err)
	}
	evb, err := os.ReadFile(filepath.Join(out, entries[0].Name(), "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if !strings.Contains(string(evb), "--timeout") || !strings.Contains(string(evb), "PROMPTOPT_TIMEOUT") {
		t.Errorf("events.jsonl diagnosis lacks the knob hint: %s", evb)
	}

	var mf runManifest
	mb, err := os.ReadFile(filepath.Join(out, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(mb, &mf); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if mf.TimeoutSeconds != 0 { // 50ms < 1s: seconds granularity rounds to 0
		t.Errorf("manifest timeout_seconds = %d, want 0 (sub-second omits)", mf.TimeoutSeconds)
	}

	// The seconds-granular snapshot: an explicit whole-second timeout
	// (long enough to let the slow gateway answer) lands in the
	// manifest for verify to replay, and the run completes normally.
	out2 := t.TempDir()
	code2, _, stderr2 := runCliVerbose(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "to-model", "--out", out2, "--headless",
		"--timeout", "90")
	if code2 != 0 {
		t.Fatalf("second run exit = %d, want 0 (90s deadline clears the 200ms gateway); stderr %s", code2, stderr2)
	}
	entries, err = os.ReadDir(out2)
	if err != nil || len(entries) != 1 {
		t.Fatalf("read run dir: %v (%v)", entries, err)
	}
	mb, err = os.ReadFile(filepath.Join(out2, entries[0].Name(), "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(mb, &mf); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if mf.TimeoutSeconds != 90 {
		t.Errorf("manifest timeout_seconds = %d, want 90", mf.TimeoutSeconds)
	}
}

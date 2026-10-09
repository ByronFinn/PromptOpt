package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
)

// --- --extra-body / --rps CLI surface (V7 roadmap §2.2) ---------------------

// TestParseRunFlagsExtraBodyAndRPS pins the usage-error surface: a
// malformed or non-object --extra-body is rejected up front, a negative
// --rps is rejected, defaults stay off, and a valid object parses into
// the map that rides engine requests.
func TestParseRunFlagsExtraBodyAndRPS(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}

	for _, bad := range []string{`{bad`, `[1,2]`, `"scalar"`, `42`} {
		_, err := parseRunFlags(append(slices.Clone(base), "--extra-body", bad))
		if err == nil || !strings.Contains(err.Error(), "JSON object") {
			t.Errorf("--extra-body %q err = %v, want a JSON-object usage error", bad, err)
		}
	}
	if _, err := parseRunFlags(append(slices.Clone(base), "--rps", "-1")); err == nil ||
		!strings.Contains(err.Error(), "--rps") {
		t.Errorf("--rps -1 err = %v, want a refusal", err)
	}

	o, err := parseRunFlags(base)
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if o.rps != 0 || o.extraBody != nil {
		t.Errorf("defaults = rps %v extraBody %v, want off/unset", o.rps, o.extraBody)
	}

	o, err = parseRunFlags(append(slices.Clone(base),
		"--rps", "5.5", "--extra-body", `{"chat_template_kwargs":{"enable_thinking":false}}`))
	if err != nil {
		t.Fatalf("valid surface: %v", err)
	}
	if o.rps != 5.5 {
		t.Errorf("rps = %v, want 5.5", o.rps)
	}
	ktw, ok := o.extraBody["chat_template_kwargs"].(map[string]any)
	if !ok || ktw["enable_thinking"] != false {
		t.Errorf("extraBody = %v, want the parsed chat_template_kwargs", o.extraBody)
	}
}

// TestResolveVerifyConnRPSAndExtraBody pins the verify-side layering:
// without an explicit flag the manifest snapshot replays the run's own
// pacing and gateway extras; --rps 0 / --extra-body ” set explicitly
// override it off.
func TestResolveVerifyConnRPSAndExtraBody(t *testing.T) {
	mf := runManifest{
		BaseURL: "http://manifest", Model: "m-model",
		RPS:       5,
		ExtraBody: map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}},
	}

	o, err := resolveVerifyConn(verifyOptions{}, mf)
	if err != nil {
		t.Fatalf("manifest tier: %v", err)
	}
	if o.rps != 5 || o.extraBody["chat_template_kwargs"] == nil {
		t.Errorf("manifest tier = rps %v extraBody %v, want the snapshot", o.rps, o.extraBody)
	}

	// Explicit flag overrides (the set-markers come from parseVerifyFlags).
	o, err = resolveVerifyConn(verifyOptions{rpsSet: true, extraBodySet: true}, mf)
	if err != nil {
		t.Fatalf("explicit-off tier: %v", err)
	}
	if o.rps != 0 || o.extraBody != nil {
		t.Errorf("explicit off = rps %v extraBody %v, want zeroed", o.rps, o.extraBody)
	}
}

// TestRunExtraBodyEndToEnd drives the full CLI seam: --extra-body must
// reach the wire payload's top level (the disable-thinking shape), the
// per-call trace must record it (the audit trail for the json:"-"
// request field), and the manifest must snapshot it for verify/replay.
func TestRunExtraBodyEndToEnd(t *testing.T) {
	task := writeYAML(t, "eb_task.yaml", `name: eb_task
prompt_template: |
  answer {input}
metrics: [exact_match]
`)
	cand := writeYAML(t, "eb_candidate.yaml", `id: baseline
prompt: |
  answer {input}
`)
	ds := writeYAML(t, "eb_dataset.yaml", `name: eb_ds
samples:
  - id: e-001
    input: 你好
    expected: "ok"
    split: test
`)

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	t.Cleanup(srv.Close)

	out := t.TempDir()
	code, summary := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "eb-model", "--out", out, "--headless",
		"--rps", "500",
		"--extra-body", `{"chat_template_kwargs":{"enable_thinking":false},"max_tokens":77}`,
	)
	if code != 0 {
		t.Fatalf("run exit = %d, stderr summary %s", code, summary)
	}
	if len(bodies) == 0 {
		t.Fatal("no request reached the stub LLM")
	}
	// Wire: the merged extras sit at the payload's top level, and the
	// user's max_tokens override wins over the flag value.
	var top map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &top); err != nil {
		t.Fatalf("wire body not JSON: %v", bodies[0])
	}
	ktw, ok := top["chat_template_kwargs"].(map[string]any)
	if !ok || ktw["enable_thinking"] != false {
		t.Errorf("wire top level lacks chat_template_kwargs.enable_thinking: %s", bodies[0])
	}
	if top["max_tokens"] != float64(77) {
		t.Errorf("wire max_tokens = %v, want the extra body's 77 (user keys win)", top["max_tokens"])
	}

	var res core.RunResult
	if err := json.Unmarshal([]byte(summary), &res); err != nil {
		t.Fatalf("headless summary not JSON: %v", summary)
	}
	// Manifest: the flag snapshots for verify/replay.
	var mf runManifest
	mb, err := os.ReadFile(filepath.Join(out, res.RunID, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if err := json.Unmarshal(mb, &mf); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if mf.RPS != 500 {
		t.Errorf("manifest rps = %v, want 500", mf.RPS)
	}
	if mf.ExtraBody["chat_template_kwargs"] == nil {
		t.Errorf("manifest extra_body = %v, want the snapshot", mf.ExtraBody)
	}
	// Trace: the audit trail records the same map (CallTrace.extra_body).
	traceFiles, err := filepath.Glob(filepath.Join(out, res.RunID, "calls", "*.json"))
	if err != nil || len(traceFiles) == 0 {
		t.Fatalf("no call traces under %s: %v", out, err)
	}
	tb, err := os.ReadFile(traceFiles[0])
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var tc eval.CallTrace
	if err := json.Unmarshal(tb, &tc); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if tc.ExtraBody["chat_template_kwargs"] == nil {
		t.Errorf("trace extra_body = %v, want the audit record", tc.ExtraBody)
	}
	// Replay: the audit timeline surfaces extra_body per call instead
	// of going blind on the json:"-" request field (the wire body is
	// reconstructible from the documented merge contract).
	entries, _, err := collectAudit(filepath.Join(out, res.RunID),
		filepath.Join(synthRoot(out), res.RunID), false)
	if err != nil {
		t.Fatalf("collectAudit: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == "call" {
			if _, ok := e.Detail["extra_body"]; ok {
				found = true
			}
		}
	}
	if !found {
		t.Error("replay timeline carries extra_body on no call entry")
	}
}

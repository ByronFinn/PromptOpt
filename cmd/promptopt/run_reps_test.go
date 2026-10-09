package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// decodeSummary decodes a headless stdout run summary.
func decodeSummary(t *testing.T, out string) core.RunResult {
	t.Helper()
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless stdout is not the run summary JSON: %v\n%s", err, out)
	}
	return res
}

// --- 手动模式 --temperature/--reps 接线（用例 ⑧） ---------------------------

// startCountingLLM answers every call with a fixed valid completion and
// records the raw request bodies.
func startCountingLLM(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"ok\": true}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

// TestRunManualTemperatureReps pins the manual-mode wiring: --temperature
// reaches every executor request body, --reps doubles the per-sample
// calls, and the manifest snapshot matches what actually executed
// （工件不撒谎——不接线则 flag 静默无效且与 manifest 不一致）.
func TestRunManualTemperatureReps(t *testing.T) {
	srv, bodies := startCountingLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds),
		"--out", outDir, "--headless", "--temperature", "0.5", "--reps", "2")...)
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}

	if got := len(*bodies); got != 6 {
		t.Fatalf("llm calls = %d, want 6 (3 samples × 2 reps)", got)
	}
	for _, b := range *bodies {
		if !strings.Contains(b, `"temperature":0.5`) {
			t.Errorf("executor request missing temperature: %.200s", b)
		}
	}

	// The stdout summary reflects the k-rep usage accumulation
	// (3 samples × 2 reps × 15 tokens = 90).
	res := decodeSummary(t, out)
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 60 || u.CompletionTokens != 30 {
		t.Errorf("usage = %+v, want 60/30 (3 samples × 2 reps × 10/5)", u)
	}

	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run dirs = %v (%v)", entries, err)
	}
	var mf runManifest
	loadJSONFile(t, filepath.Join(outDir, entries[0].Name(), "manifest.json"), &mf)
	if mf.Temperature != 0.5 || mf.Reps != 2 {
		t.Errorf("manifest temperature/reps = %v/%v, want 0.5/2", mf.Temperature, mf.Reps)
	}
}

// TestRunStatsFlagUsageErrors pins the flag validation surface.
func TestRunStatsFlagUsageErrors(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", "http://127.0.0.1:9", "--model", "m", "--out", t.TempDir()}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"temperature over 2", append(mustClone(base), "--temperature", "2.5"), "within [0, 2]"},
		{"negative temperature", append(mustClone(base), "--temperature", "-0.1"), "within [0, 2]"},
		{"reps below 1", append(mustClone(base), "--reps", "0"), "--reps must be >= 1"},
	} {
		if _, err := parseRunFlags(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func mustClone(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// --- 零配置 pipeline baseline 同口径（用例 ② 第二部分） ---------------------

// TestRunZeroConfigRepsBaselineBudget pins the pipeline-side accounting:
// kept=3、--reps 2 时 baseline 每样本整占 2 个派发名额。探针窗口先消耗
// 2 变体 × 3 样本 = 6 个名额：
//   - budget-evals 7 只剩 1 个名额 < 2 → baseline 第一个样本即被拒 →
//     全部 undispatched、退出码 2；
//   - budget-evals 8 恰好放下 1 个样本（名额 7–8），其余 undispatched。
//
// reps 计入共享 executor 预算与派发名额的承诺由此在零配置侧成立。
func TestRunZeroConfigRepsBaselineBudget(t *testing.T) {
	t.Run("budget 7 starves the whole baseline", func(t *testing.T) {
		srv := startScriptedZeroConfigLLM(t, zc3Samples, zc3Script)
		outDir := filepath.Join(t.TempDir(), "runs")
		code, out := runCli(t, "从中医医案文本判断证候",
			"--base-url", srv.URL, "--model", "jiuwei-tcm",
			"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
			"--reps", "2", "--budget-evals", "7")
		if code != 2 {
			t.Fatalf("exit = %d, want 2\n%s", code, out)
		}
		res := decodeSummary(t, out)
		if res.Undispatched != 3 || res.Evaluated != 0 {
			t.Errorf("undispatched/evaluated = %d/%d, want 3/0 (每样本整占 2 名额，1 个余名额放不下)", res.Undispatched, res.Evaluated)
		}
		if res.Status != core.StatusBudgetExhausted {
			t.Errorf("status = %s, want budget_exhausted", res.Status)
		}
	})

	t.Run("budget 8 fits exactly one baseline sample", func(t *testing.T) {
		srv := startScriptedZeroConfigLLM(t, zc3Samples, zc3Script)
		outDir := filepath.Join(t.TempDir(), "runs")
		code, out := runCli(t, "从中医医案文本判断证候",
			"--base-url", srv.URL, "--model", "jiuwei-tcm",
			"--out", outDir, "--headless", "--samples", "3", "--probe-variants", "2",
			"--reps", "2", "--budget-evals", "8")
		if code != 2 {
			t.Fatalf("exit = %d, want 2\n%s", code, out)
		}
		res := decodeSummary(t, out)
		if res.Evaluated != 1 || res.Undispatched != 2 {
			t.Errorf("evaluated/undispatched = %d/%d, want 1/2 (名额 7–8 恰好整占一个样本)", res.Evaluated, res.Undispatched)
		}
	})
}

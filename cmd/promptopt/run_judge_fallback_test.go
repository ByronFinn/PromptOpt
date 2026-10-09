package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
)

// --- 复审拓宽的第二实例构造条件 ---------------------------------------------
//
// 构造条件是「--judge-provider/--judge-base-url/--judge-api-key 任一非空」，
// 不是「仅 --judge-provider」：baseURL/apiKey 绑在 Provider 实例内
// （newProvider 工厂参数，provider.Provider 接口不暴露地址），单设
// --judge-base-url 或 --judge-api-key 而不构造第二实例会让配置静默失效。

const (
	isoExecCompletion  = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"ok\": true}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	isoJudgeVerdict    = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"score\": 0.75, \"diagnosis\": \"部分正确\"}"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	isoJudgePromptMark = "评估裁判"
)

// startAuthRecordingLLM records each request's Authorization header next
// to its raw body and answers with a body-keyed fake: requests carrying
// the judge rubric get the judge verdict, everything else the executor
// completion.
func startAuthRecordingLLM(t *testing.T) (*httptest.Server, *[]string, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies, auths := &[]string{}, &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		mu.Lock()
		*bodies = append(*bodies, string(b))
		*auths = append(*auths, auth)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), isoJudgePromptMark) {
			fmt.Fprint(w, isoJudgeVerdict)
			return
		}
		fmt.Fprint(w, isoExecCompletion)
	}))
	t.Cleanup(srv.Close)
	return srv, bodies, auths
}

// TestRunJudgeBaseURLAloneRoutesJudgeCalls pins the widened rule for
// --judge-base-url: with the provider name left at the executor's
// default, the second instance must still be constructed and the judge
// call must reach the second address while the executor call stays on
// the main one.
func TestRunJudgeBaseURLAloneRoutesJudgeCalls(t *testing.T) {
	execSrv, execBodies, _ := startAuthRecordingLLM(t)
	judgeSrv, judgeBodies, _ := startAuthRecordingLLM(t)
	task, cand, ds := judgeTaskYAML(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", execSrv.URL, "--model", "fake-model", "--api-key", "1",
		"--judge-base-url", judgeSrv.URL, // 仅设 base-url：provider 名同主
		"--out", outDir, "--headless")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}

	// The judge dialed its own address; the executor never saw the
	// grading prompt.
	if got := len(*judgeBodies); got != 1 || !strings.Contains((*judgeBodies)[0], isoJudgePromptMark) {
		t.Errorf("judge server calls = %v, want exactly the one grading call", *judgeBodies)
	}
	if got := len(*execBodies); got != 1 || strings.Contains((*execBodies)[0], isoJudgePromptMark) {
		t.Errorf("executor server calls = %v, want exactly one non-judge call", *execBodies)
	}
	// Model name falls back to the executor's (only the address differs).
	if !strings.Contains((*judgeBodies)[0], `"model":"fake-model"`) {
		t.Errorf("judge wire body = %.200s, want the executor's model name as fallback", (*judgeBodies)[0])
	}

	// UsageByRole: judge spend sits under its own key (7/3), executor
	// keeps 10/5.
	res := decodeSummary(t, out)
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 7 || u.CompletionTokens != 3 {
		t.Errorf("judge usage = %+v, want 7/3 under RoleJudge", u)
	}
}

// TestRunJudgeAPIKeyAloneReachesSecondInstance pins the widened rule for
// --judge-api-key: the second instance must be constructed and its
// requests must carry that key's Authorization header while the
// executor requests keep the main key.
func TestRunJudgeAPIKeyAloneReachesSecondInstance(t *testing.T) {
	srv, bodies, auths := startAuthRecordingLLM(t)
	task, cand, ds := judgeTaskYAML(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, "--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "fake-model", "--api-key", "1",
		"--judge-api-key", "judge-secret", // 仅设 api-key：地址回落主配置
		"--out", outDir, "--headless")
	if code != 0 {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}

	if got := len(*bodies); got != 2 {
		t.Fatalf("llm calls = %d, want 2 (executor + judge on the one address)", got)
	}
	for i, b := range *bodies {
		if strings.Contains(b, isoJudgePromptMark) {
			if got := (*auths)[i]; got != "Bearer judge-secret" {
				t.Errorf("judge Authorization = %q, want Bearer judge-secret", got)
			}
			continue
		}
		if got := (*auths)[i]; got != "Bearer 1" {
			t.Errorf("executor Authorization = %q, want Bearer 1", got)
		}
	}
}

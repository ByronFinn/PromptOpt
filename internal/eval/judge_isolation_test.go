package eval

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- judge isolation (V7 roadmap §2.1): the optional second judge LLM -----

// TestEngineIndependentJudgeProvider drives the judge isolation seam
// with two httptest servers: the dedicated judge provider receives the
// grading call carrying the judge's own model name and completion
// budget, the executor call keeps flowing to the main server, and the
// two spends land under separate UsageByRole keys while the
// sample-level totals stay merged (executor+judge, one evaluation
// cost).
func TestEngineIndependentJudgeProvider(t *testing.T) {
	execSrv, execBodies := startLLM(t, func(_ int, _ string) (int, string) {
		return http.StatusOK, completionJSON(`{"ok": true}`, execSpend)
	})
	judgeSrv, judgeBodies := startLLM(t, func(_ int, _ string) (int, string) {
		return http.StatusOK, completionJSON(`{"score": 0.75, "diagnosis": "部分正确：少抽一个实体"}`, judgeSpend)
	})

	dir := t.TempDir()
	var done *Event
	e := &Engine{
		RunID: "judge-iso", RunDir: dir, Model: "fake-model", MaxTokens: 64,
		Workers: 1, Metrics: []string{MetricLLMJudge},
		Budget:        NewBudget(0, 0),
		Provider:      provider.NewOpenAI(execSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		JudgeProvider: provider.NewOpenAI(judgeSrv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		JudgeModel:    "judge-model",
		JudgeMaxTokens: 32,
		TaskName:       "task", CandidateID: "cand", DatasetName: "ds",
		OnEvent: func(ev Event) {
			if ev.Type == EventSampleDone {
				done = &ev
			}
		},
	}
	res, err := e.Run(context.Background(), core.Candidate{ID: "cand", Prompt: "抽取实体：{input}"}, testSamples(1))
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	if res.ExitCode != 0 || res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Fatalf("result = %s/exit %d means %v, want completed/0 with llm_judge 0.75", res.Status, res.ExitCode, res.MetricMeans)
	}

	// Routing: exactly one call per server — the judge dialed its own
	// backend, the executor never saw the grading prompt.
	if got := len(*judgeBodies); got != 1 {
		t.Fatalf("judge server calls = %d, want 1", got)
	}
	if got := len(*execBodies); got != 1 {
		t.Fatalf("executor server calls = %d, want 1", got)
	}
	if body := (*judgeBodies)[0]; !strings.Contains(body, `"model":"judge-model"`) || !strings.Contains(body, `"max_tokens":32`) {
		t.Errorf("judge wire body = %.200s, want model judge-model and max_tokens 32", body)
	}
	if body := (*execBodies)[0]; !strings.Contains(body, `"model":"fake-model"`) || !strings.Contains(body, `"max_tokens":64`) {
		t.Errorf("executor wire body = %.200s, want model fake-model and max_tokens 64", body)
	}

	// Per-role metering: executor 10/5, judge 7/3 — single-key each.
	if u := res.UsageByRole[core.RoleExecutor]; u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("executor usage = %+v, want 10/5", u)
	}
	if u := res.UsageByRole[core.RoleJudge]; u.PromptTokens != 7 || u.CompletionTokens != 3 {
		t.Errorf("judge usage = %+v, want 7/3 under RoleJudge", u)
	}
	// The sample-level total keeps merging both calls (17/8) — the
	// one-evaluation-cost semantics the verify caps and the soft stop
	// share.
	if done == nil {
		t.Fatal("sample_done event missing")
	}
	if done.Usage == nil || done.Usage.PromptTokens != 17 || done.Usage.CompletionTokens != 8 {
		t.Errorf("sample_done usage = %+v, want 17/8 (executor+judge)", done.Usage)
	}

	// Call traces: the judge trace carries the judge role and the
	// judge's own request meta; the executor trace stays executor.
	judgeTrace, execTrace := splitJudgeTraces(t, dir)
	if judgeTrace.Role != core.RoleJudge || judgeTrace.Request.Model != "judge-model" || judgeTrace.Request.MaxTokens != 32 {
		t.Errorf("judge trace = role %s model %s max_tokens %d, want judge/judge-model/32",
			judgeTrace.Role, judgeTrace.Request.Model, judgeTrace.Request.MaxTokens)
	}
	if execTrace.Role != core.RoleExecutor || execTrace.Request.Model != "fake-model" {
		t.Errorf("executor trace = role %s model %s, want executor/fake-model",
			execTrace.Role, execTrace.Request.Model)
	}
}

// TestEngineJudgePartialFallback pins the per-field fallback: only the
// judge model is set, so the grading call still flows through the
// executor's provider but carries the independent model name while
// reusing the executor's completion budget.
func TestEngineJudgePartialFallback(t *testing.T) {
	srv, bodies := startLLM(t, func(_ int, body string) (int, string) {
		if strings.Contains(body, "评估裁判") {
			return http.StatusOK, completionJSON(`{"score": 0.75, "diagnosis": "部分正确"}`, judgeSpend)
		}
		return http.StatusOK, completionJSON(`{"ok": true}`, execSpend)
	})

	e := &Engine{
		RunID: "judge-partial", RunDir: t.TempDir(), Model: "fake-model", MaxTokens: 64,
		Workers: 1, Metrics: []string{MetricLLMJudge},
		Budget:     NewBudget(0, 0),
		Provider:   provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		JudgeModel: "judge-model", // provider and tokens unset: fall back
		TaskName:   "task", CandidateID: "cand", DatasetName: "ds",
	}
	res, err := e.Run(context.Background(), core.Candidate{ID: "cand", Prompt: "抽取实体：{input}"}, testSamples(1))
	if err != nil {
		t.Fatalf("Engine.Run: %v", err)
	}
	if res.ExitCode != 0 || res.MetricMeans[MetricLLMJudge] != 0.75 {
		t.Fatalf("result = %s/exit %d, want completed/0 with llm_judge 0.75", res.Status, res.ExitCode)
	}
	if got := len(*bodies); got != 2 {
		t.Fatalf("calls = %d, want 2 (both through the one server)", got)
	}
	var judgeBody string
	for _, b := range *bodies {
		if strings.Contains(b, "评估裁判") {
			judgeBody = b
		}
	}
	if !strings.Contains(judgeBody, `"model":"judge-model"`) || !strings.Contains(judgeBody, `"max_tokens":64`) {
		t.Errorf("judge wire body = %.200s, want judge-model with the executor's 64-token budget", judgeBody)
	}
}

// splitJudgeTraces decodes the calls/ dir into its judge and executor
// traces.
func splitJudgeTraces(t *testing.T, dir string) (judge, exec *CallTrace) {
	t.Helper()
	traces := callTraces(t, dir)
	for i := range traces {
		if traces[i].Stage == "judge" {
			judge = &traces[i]
		} else {
			exec = &traces[i]
		}
	}
	if judge == nil || exec == nil {
		t.Fatalf("traces incomplete: %+v", traces)
	}
	return judge, exec
}

package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// startOptLLM routes fake completions by the engine markers, recording
// every raw request body (the harness startLLM precedent).
func startOptLLM(t *testing.T, route func(body string) string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, route(string(b)))
	}))
	t.Cleanup(srv.Close)
	return srv, bodies
}

func testAdvisor(t *testing.T, srv *httptest.Server) *advisor {
	t.Helper()
	return &advisor{
		provider:     provider.NewOpenAI(srv.URL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		model:        "fake-model",
		optMaxTokens: 1024,
		dir:          t.TempDir(),
		budget:       eval.NewBudget(0, 0),
		runID:        "test-run",
	}
}

func reflectTask() core.Task {
	return core.Task{
		Name: "tcm_test", Description: "判断证候",
		PromptTemplate: "你是中医助手。文本：{input}", Metrics: []string{"exact_match"},
	}
}

func reflectBatch() []SampleRecord {
	return []SampleRecord{
		{
			Sample:    core.Sample{ID: "s1", Input: "恶寒发热，无汗，脉浮紧。", Expected: "风寒束表", Split: "train"},
			Response:  "风热犯表",
			Scores:    map[string]float64{"exact_match": 0},
			Diagnosis: map[string]string{"exact_match": "与参考答案不一致"},
		},
	}
}

const hypJSON = `{"hypotheses":[
 {"id":"h1","text":"明确要求只输出证候名称本身","sample_ids":["s1"],"confidence":0.9},
 {"id":"h2","text":"给出常见证候列表供选择","confidence":0.5}
]}`

// TestReflectorParsesHypotheses covers the pass-through, fence and
// clip/normalize paths of the hypothesis JSON.
func TestReflectorParsesHypotheses(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"bare JSON passes through", hypJSON},
		{"fenced JSON is extracted", "```json\n" + hypJSON + "\n```"},
		{"prose around fenced JSON", "好的，假设如下：\n```\n" + hypJSON + "\n```\n以上。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := startOptLLM(t, func(body string) string {
				if !strings.Contains(body, MarkerReflect) {
					t.Errorf("reflection prompt missing marker: %.200s", body)
				}
				return tc.content
			})
			r := NewReflector(testAdvisor(t, srv))
			hyps, err := r.Reflect(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, nil, reflectBatch(), 3)
			if err != nil {
				t.Fatalf("Reflect: %v", err)
			}
			if len(hyps) != 2 || hyps[0].ID != "h1" || hyps[1].ID != "h2" {
				t.Fatalf("hypotheses = %+v", hyps)
			}
			if hyps[0].Confidence != 0.9 {
				t.Errorf("confidence = %v, want preserved 0.9", hyps[0].Confidence)
			}
		})
	}
}

// TestReflectorClipsAndCleans verifies the pool clip to n, assigned
// ids, {input} literal removal, text truncation and confidence
// clamping.
func TestReflectorClipsAndCleans(t *testing.T) {
	var long strings.Builder
	for range 2000 {
		long.WriteRune('长')
	}
	raw := fmt.Sprintf(`{"hypotheses":[
	 {"text":"假设一包含{input}字面量","confidence":1.7},
	 {"text":"%s"},
	 {"text":"假设三"},
	 {"text":"假设四应被裁剪"}
	]}`, long.String())
	srv, _ := startOptLLM(t, func(string) string { return raw })
	r := NewReflector(testAdvisor(t, srv))
	hyps, err := r.Reflect(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, nil, reflectBatch(), 3)
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(hyps) != 3 {
		t.Fatalf("hypotheses = %d, want clipped to 3", len(hyps))
	}
	if hyps[0].ID != "h1" || hyps[1].ID != "h2" || hyps[2].ID != "h3" {
		t.Fatalf("assigned ids = %s/%s/%s, want h1/h2/h3", hyps[0].ID, hyps[1].ID, hyps[2].ID)
	}
	if strings.Contains(hyps[0].Text, "{input}") {
		t.Errorf("hypothesis text kept a literal {input}: %q", hyps[0].Text)
	}
	if got := len([]rune(hyps[1].Text)); got > maxHypoTextRunes+1 {
		t.Errorf("long hypothesis text = %d runes, want capped at %d", got, maxHypoTextRunes)
	}
	if hyps[0].Confidence != 1 {
		t.Errorf("confidence = %v, want clamped to 1", hyps[0].Confidence)
	}
}

// TestReflectorRepairsInvalidJSON: a non-JSON response triggers
// exactly one repair call that fixes it.
func TestReflectorRepairsInvalidJSON(t *testing.T) {
	srv, bodies := startOptLLM(t, func(body string) string {
		if strings.Contains(body, MarkerHypRepair) {
			return hypJSON
		}
		return "这不是 JSON，抱歉。"
	})
	r := NewReflector(testAdvisor(t, srv))
	hyps, err := r.Reflect(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, nil, reflectBatch(), 3)
	if err != nil {
		t.Fatalf("Reflect after repair: %v", err)
	}
	if len(hyps) != 2 {
		t.Fatalf("hypotheses = %+v", hyps)
	}
	if len(*bodies) != 2 {
		t.Errorf("calls = %d, want 2 (reflect + repair)", len(*bodies))
	}
	// The repair prompt embeds the raw draft.
	if !strings.Contains((*bodies)[1], "这不是 JSON") {
		t.Errorf("repair prompt lost the raw draft")
	}
	// Traces exist for both attempts under opt-calls/.
	entries, _ := os.ReadDir(r.adv.dir)
	if len(entries) != 2 {
		t.Errorf("opt-calls traces = %d, want 2", len(entries))
	}
}

// TestReflectorEmptyResponseShortCircuits: an empty raw response
// errors without firing a repair call.
func TestReflectorEmptyResponseShortCircuits(t *testing.T) {
	srv, bodies := startOptLLM(t, func(string) string { return "" })
	adv := testAdvisor(t, srv)
	type payload struct {
		Hypotheses []Hypothesis `json:"hypotheses"`
	}
	_, err := defend[payload](t.Context(), adv, "reflect", MarkerHypRepair, "", func(payload) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "空") {
		t.Fatalf("defend on empty raw = %v, want 空-content error", err)
	}
	if len(*bodies) != 0 {
		t.Errorf("repair fired %d times on empty raw, want 0", len(*bodies))
	}
}

const candJSON = `{"id":"llm-id","name":"改写版","description":"更明确的输出要求","prompt":"你是资深中医辨证助手。仔细阅读：{input}。只输出证候名称。"}`

func TestMutatorRewriteProducesCandidate(t *testing.T) {
	srv, bodies := startOptLLM(t, func(body string) string {
		if !strings.Contains(body, MarkerRewrite) {
			t.Errorf("rewrite prompt missing marker: %.200s", body)
		}
		// The selected hypothesis text must feed the prompt.
		if !strings.Contains(body, "text h1") {
			t.Errorf("rewrite prompt lost the selected hypothesis text")
		}
		return candJSON
	})
	m := NewMutator(testAdvisor(t, srv))
	cand, err := m.Rewrite(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, hyp("h1", 0.5, 0.8).Hypothesis, nil)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if cand.ID != "llm-id" || !strings.Contains(cand.Prompt, core.InputPlaceholder) {
		t.Fatalf("candidate = %+v", cand)
	}
	if len(*bodies) != 1 {
		t.Errorf("calls = %d, want 1", len(*bodies))
	}
}

// TestMutatorRepairsMissingPlaceholder: a product missing {input}
// goes through exactly one repair call.
func TestMutatorRepairsMissingPlaceholder(t *testing.T) {
	bad := strings.Replace(candJSON, "{input}", "输入", 1)
	srv, bodies := startOptLLM(t, func(body string) string {
		if strings.Contains(body, MarkerCandFix) {
			return candJSON
		}
		return bad
	})
	m := NewMutator(testAdvisor(t, srv))
	cand, err := m.Rewrite(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, hyp("h1", 0.5, 0.8).Hypothesis, nil)
	if err != nil {
		t.Fatalf("Rewrite after repair: %v", err)
	}
	if !strings.Contains(cand.Prompt, core.InputPlaceholder) {
		t.Fatalf("repaired prompt = %q, want {input} present", cand.Prompt)
	}
	if len(*bodies) != 2 {
		t.Errorf("calls = %d, want 2 (rewrite + repair)", len(*bodies))
	}
	if !strings.Contains((*bodies)[1], "缺少 {input}") {
		t.Errorf("repair prompt lost the problem list: %.200s", (*bodies)[1])
	}
}

// TestMutatorFailsWithExcerpt: a repair that still fails surfaces the
// raw excerpt.
func TestMutatorFailsWithExcerpt(t *testing.T) {
	bad := strings.Replace(candJSON, "{input}", "输入", 1)
	srv, bodies := startOptLLM(t, func(string) string { return bad })
	m := NewMutator(testAdvisor(t, srv))
	_, err := m.Rewrite(t.Context(), reflectTask(), core.Candidate{ID: "baseline", Prompt: "p {input}"}, hyp("h1", 0.5, 0.8).Hypothesis, nil)
	if err == nil {
		t.Fatal("expected failure after a bad repair")
	}
	if !strings.Contains(err.Error(), "原文摘录") {
		t.Errorf("error lacks the raw excerpt: %v", err)
	}
	if len(*bodies) != 2 {
		t.Errorf("calls = %d, want 2", len(*bodies))
	}
}

// TestMutatorMergeCarriesBothParents: the merge prompt must embed both
// parent prompts and the hypothesis as directive.
func TestMutatorMergeCarriesBothParents(t *testing.T) {
	srv, bodies := startOptLLM(t, func(body string) string {
		if !strings.Contains(body, MarkerMerge) {
			t.Errorf("merge prompt missing marker: %.200s", body)
		}
		return candJSON
	})
	m := NewMutator(testAdvisor(t, srv))
	parentA := core.Candidate{ID: "a", Prompt: "父提示词甲：阅读 {input} 并判断"}
	parentB := core.Candidate{ID: "b", Prompt: "父提示词乙：审阅 {input} 后作答"}
	cand, err := m.Merge(t.Context(), reflectTask(), parentA, parentB, hyp("h2", 0.3, 0.7).Hypothesis, nil)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if !strings.Contains(cand.Prompt, core.InputPlaceholder) {
		t.Fatalf("merged prompt = %q", cand.Prompt)
	}
	if len(*bodies) != 1 {
		t.Fatalf("calls = %d, want 1", len(*bodies))
	}
	sent := (*bodies)[0]
	for _, frag := range []string{"父提示词甲", "父提示词乙", "合并指导"} {
		if !strings.Contains(sent, frag) {
			t.Errorf("merge prompt missing %q", frag)
		}
	}
}

// TestMutatorFreshIgnoresParents: the fresh restart prompt carries the
// task spec but not the parent prompt.
func TestMutatorFreshIgnoresParents(t *testing.T) {
	srv, bodies := startOptLLM(t, func(body string) string {
		if !strings.Contains(body, MarkerFresh) {
			t.Errorf("fresh prompt missing marker: %.200s", body)
		}
		return candJSON
	})
	m := NewMutator(testAdvisor(t, srv))
	if _, err := m.Fresh(t.Context(), reflectTask()); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	sent := (*bodies)[0]
	if !strings.Contains(sent, "判断证候") {
		t.Errorf("fresh prompt lost the task description")
	}
	if strings.Contains(sent, "旧父提示词") {
		t.Errorf("fresh prompt unexpectedly carries parent material")
	}
}

// TestAdvisorEscalationLadderAndUsage: empty content with
// finish_reason=length doubles the cap (floored at the config
// default); usage of every attempt is metered under the optimizer role
// without arming the executor soft stop.
func TestAdvisorEscalationLadderAndUsage(t *testing.T) {
	var mu sync.Mutex
	var caps []int
	ladder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(b, &req)
		mu.Lock()
		caps = append(caps, req.MaxTokens)
		first := len(caps) == 1
		mu.Unlock()
		finish, content := "stop", "好的"
		if first {
			finish, content = "length", ""
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":%q,"message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`, finish, content)
	}))
	defer ladder.Close()
	adv := testAdvisor(t, ladder)
	adv.optMaxTokens = 4096
	got, err := adv.call(t.Context(), "stage", "prompt")
	if err != nil || got != "好的" {
		t.Fatalf("call = %q err = %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(caps) != 2 || caps[0] != 8192 || caps[1] != 16384 {
		t.Errorf("caps = %v, want [8192 16384] (floor is max(opt, default))", caps)
	}
	// Usage of both attempts recorded under the optimizer role.
	_, usage := adv.budget.Snapshot()
	if u := usage[core.RoleOptimizer]; u.PromptTokens != 200 || u.CompletionTokens != 100 {
		t.Errorf("optimizer usage = %+v, want 200/100", u)
	}
	if adv.budget.SoftStopped() {
		t.Error("optimizer usage must not arm the executor soft stop")
	}
	if adv.floor != 16384 {
		t.Errorf("learned floor = %d, want 16384", adv.floor)
	}
}

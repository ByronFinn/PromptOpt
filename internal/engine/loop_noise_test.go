package engine

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// --- SD 数据通路集成（用例 ④⑤，夹具形态对齐方案①真实路径——双侧有 SD） -----

// noiseSamples is the single-sample retained set; rows are therefore
// one-dimensional and easy to reason about.
var noiseSamples = []core.Sample{{ID: "s1", Input: "甲恶寒发热", Expected: "风寒", Split: "train"}}

// noiseTask is the one-metric task of the noise fixtures.
var noiseTask = core.Task{Name: "noise", PromptTemplate: "基础助手。{input}", Metrics: []string{"exact_match"}}

// startAlternatingLLM answers correct on odd calls and wrong on even
// calls: with reps=2 a sample's exact_match row is the mean 0.5 with
// in-sample sd 0.5; with reps=1 the single call answers correct → 1.
func startAlternatingLLM(t *testing.T) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call++
		w.Header().Set("Content-Type", "application/json")
		content := "答错"
		if call%2 == 1 {
			content = "风寒"
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"` + content + `"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
}

func noiseRequest(dir string, srvURL string, reps int, baseline []SampleRecord) Request {
	return Request{
		Task: noiseTask,
		Params: Params{MaxRounds: 2, Minibatch: 1, StagnationLimit: 2, Epsilon: 0, Seed: 7},
		Initial:  core.Candidate{ID: "baseline", Prompt: noiseTask.PromptTemplate},
		Samples:  noiseSamples,
		Baseline: baseline,
		Provider: provider.NewOpenAI(srvURL, "1", provider.OpenAIConfig{MaxAttempts: 1}),
		Model:    "fake-model", MaxTokens: 64, OptMaxTokens: 64, Workers: 1,
		Temperature: 0, Reps: reps,
		Budget: eval.NewBudget(0, 0),
		RunID:  "noise", RunDir: dir,
	}
}

// sdRecord builds one baseline record shaped like a reps>1 pipeline
// row: k-rep mean score plus the per-sample in-sample sd.
func sdRecord(score, sd float64) SampleRecord {
	return SampleRecord{
		Sample:   noiseSamples[0],
		Response: "应答",
		Scores:   map[string]float64{"exact_match": score},
		ScoresSD: map[string]float64{"exact_match": sd},
	}
}

// TestLoopNoiseAwareAdmission（用例 ④）: baseline [0.4]（sd 0.5）seed 进前沿，
// child 经 reps=2 交替分评出 [0.5]（sd 0.5）——池化 ε=0.5 下 |Δ|=0.1 落入
// 噪声带 → ε-clone 拒绝、前沿保留旧成员；同一形状 reps=1（双侧无 SD）时
// child [1.0] 严格支配 [0.4] → 准入并淘汰——证明走了 AddNoiseAware 的
// 两条分支而非恒退化。
func TestLoopNoiseAwareAdmission(t *testing.T) {
	t.Run("reps=2 both sides SD: sub-ε edge is an ε-clone rejection", func(t *testing.T) {
		srv := startAlternatingLLM(t)
		dir := t.TempDir()
		req := noiseRequest(dir, srv.URL, 2, []SampleRecord{sdRecord(0.4, 0.5)})
		l, err := NewLoop(req)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		if sd := l.Frontier().Members()[0].SD; !slices.Equal(sd, []float64{0.5}) {
			t.Fatalf("baseline member SD = %v, want [0.5] (NewLoop seeds the sd row)", sd)
		}

		child := core.Candidate{ID: "child", Prompt: "改进助手。{input}"}
		res, records, err := l.Evaluate(t.Context(), child, noiseSamples, 1)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("unit eval exit = %d", res.ExitCode)
		}
		// SD 通路第 3 环：unit 采集透传 ev.ScoresSD。
		if got := records[0].Scores["exact_match"]; got != 0.5 {
			t.Errorf("child row = %v, want the 0.5 rep-mean", got)
		}
		if got := records[0].ScoresSD["exact_match"]; got != 0.5 {
			t.Errorf("child row sd = %v, want 0.5", got)
		}

		admitted, err := l.Admit(child, OpRewrite, nil, nil, 1, res, records)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if admitted {
			t.Fatal("child must be rejected as an ε-clone (|0.5−0.4| = 0.1 ≤ ε 0.5)")
		}
		members := l.Frontier().Members()
		if len(members) != 1 || members[0].ID() != "baseline" {
			t.Errorf("frontier = %v, want the incumbent baseline kept", members)
		}
		if members[0].SD == nil {
			t.Error("incumbent member must keep its SD row")
		}
	})

	t.Run("reps=1 both sides SD-free: strict dominance still admits and evicts", func(t *testing.T) {
		srv := startAlternatingLLM(t)
		dir := t.TempDir()
		plain := sdRecord(0.4, 0.5)
		plain.ScoresSD = nil // 模拟 reps=1 工件：无 SD
		req := noiseRequest(dir, srv.URL, 1, []SampleRecord{plain})
		l, err := NewLoop(req)
		if err != nil {
			t.Fatalf("NewLoop: %v", err)
		}
		if sd := l.Frontier().Members()[0].SD; sd != nil {
			t.Fatalf("baseline member SD = %v, want nil on the single-shot path", sd)
		}

		child := core.Candidate{ID: "child", Prompt: "改进助手。{input}"}
		res, records, err := l.Evaluate(t.Context(), child, noiseSamples, 1)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if records[0].ScoresSD != nil {
			t.Errorf("child sd = %v, want nil at reps=1", records[0].ScoresSD)
		}
		admitted, err := l.Admit(child, OpRewrite, nil, nil, 1, res, records)
		if err != nil {
			t.Fatalf("Admit: %v", err)
		}
		if !admitted {
			t.Fatal("child [1.0] strictly dominates [0.4] — the nil-margin path must behave like Add")
		}
		members := l.Frontier().Members()
		if len(members) != 1 || members[0].ID() != "child" {
			t.Errorf("frontier = %v, want the child having evicted baseline", members)
		}
	})
}

// TestLoopMixedSDDegradesToAdd（用例 ⑤）: baseline 无 SD（异常工件形态）vs
// child 有 SD → 门控退化为 Add 语义：优势在噪声带内也准入并淘汰——不 panic、
// 不误杀，作为混合态的既定语义锚定。
func TestLoopMixedSDDegradesToAdd(t *testing.T) {
	srv := startAlternatingLLM(t)
	dir := filepath.Join(t.TempDir(), "unused")
	plain := sdRecord(0.4, 0.5)
	plain.ScoresSD = nil
	req := noiseRequest(dir, srv.URL, 2, []SampleRecord{plain})
	l, err := NewLoop(req)
	if err != nil {
		t.Fatalf("NewLoop: %v", err)
	}

	child := core.Candidate{ID: "child", Prompt: "改进助手。{input}"}
	res, records, err := l.Evaluate(t.Context(), child, noiseSamples, 1)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if records[0].ScoresSD["exact_match"] != 0.5 {
		t.Fatalf("child sd = %v, want 0.5 (reps=2 unit)", records[0].ScoresSD)
	}
	admitted, err := l.Admit(child, OpRewrite, nil, nil, 1, res, records)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	// 单侧有 SD：margin 返回 nil → Add 语义 → child [0.5] 支配 [0.4] 准入。
	if !admitted {
		t.Fatal("mixed pair must degrade to Add (admit), not misfire the ε gate")
	}
	if members := l.Frontier().Members(); len(members) != 1 || members[0].ID() != "child" {
		t.Errorf("frontier = %v, want child after the Add-semantics eviction", members)
	}
}

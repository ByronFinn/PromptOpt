package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestSystemOneWireShape pins the re-reviewed /v1/systemone request
// contract on the wire: POST to the service root + /v1/systemone, body
// {model, state, questions} with a name-indexed questions object and a
// score question whose criteria is the ascending level array.
func TestSystemOneWireShape(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "tev1:0.8b",
			"answers": {"q1": {"type": "score",
				"probabilities": {"0": 0.2, "1": 0.3, "2": 0.5},
				"confidence": 0.81, "score": 2.3,
				"legend": {"0": "a", "1": "b", "2": "c"}, "selected": 2}},
			"usage": {"input_tokens": 9, "output_tokens": 1}
		}`))
	}))
	t.Cleanup(srv.Close)

	levels := []string{"完全错误", "部分正确", "基本正确", "与参考答案完全等价"}
	c := NewSystemOne(srv.URL, "tev1:0.8b", SystemOneConfig{})
	resp, err := c.Do(context.Background(), SystemOneRequest{
		Model: c.Model(),
		State: "你是提示词优化管线的评估裁判",
		Questions: map[string]SystemOneQuestion{
			"q1": {Type: "score", Instructions: "证据拼装", Criteria: levels},
		},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/systemone" {
		t.Errorf("wire = %s %s, want POST /v1/systemone", gotMethod, gotPath)
	}
	var req map[string]any
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, gotBody)
	}
	if req["model"] != "tev1:0.8b" {
		t.Errorf("wire model = %v", req["model"])
	}
	if req["state"] == "" {
		t.Error("wire state is empty")
	}
	questions, ok := req["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions not name-indexed: %T", req["questions"])
	}
	q, ok := questions["q1"].(map[string]any)
	if !ok {
		t.Fatalf("questions[q1] missing: %v", questions)
	}
	if q["type"] != "score" {
		t.Errorf("wire question type = %v, want score", q["type"])
	}
	criteria, ok := q["criteria"].([]any)
	if !ok || len(criteria) != 4 {
		t.Fatalf("wire criteria not a 4-array: %v", q["criteria"])
	}

	// Response decode + usage conversion into the metering shape.
	ans := resp.Answers["q1"]
	if ans.Score != 2.3 || ans.Confidence != 0.81 || ans.Type != "score" {
		t.Errorf("answer = %+v, want score 2.3 confidence 0.81", ans)
	}
	if len(ans.Probabilities) != 3 || ans.Probabilities["2"] != 0.5 {
		t.Errorf("probabilities = %+v", ans.Probabilities)
	}
	if u := resp.Usage.Usage(); u.PromptTokens != 9 || u.CompletionTokens != 1 {
		t.Errorf("usage conversion = %+v, want 9/1", u)
	}
}

// TestSystemOneGoldenFixture replays the measured tcmsp-30 response:
// the fixture pins the exact score (1.386988025801118) and usage
// (123/1) of the 3-level criteria call; probabilities and confidence
// are the transcript's truncated prefixes (see the fixture README —
// no digits were fabricated).
func TestSystemOneGoldenFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/systemone-score.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var resp SystemOneResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	ans := resp.Answers["q1"]
	if ans.Score != 1.386988025801118 {
		t.Errorf("fixture score = %.18f, want 1.386988025801118", ans.Score)
	}
	if ans.Type != "score" || len(ans.Legend) != 3 {
		t.Errorf("fixture answer shape = %+v, want score type with a 3-level legend", ans)
	}
	if resp.Usage.InputTokens != 123 || resp.Usage.OutputTokens != 1 {
		t.Errorf("fixture usage = %+v, want 123/1", resp.Usage)
	}
}

// TestSystemOneErrorsSurface: non-2xx answers and unknown paths fail
// loudly — the eval cascade never falls back on transport errors, so
// the client must not swallow them.
func TestSystemOneErrorsSurface(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c := NewSystemOne(srv.URL, "tev1:0.8b", SystemOneConfig{})
	_, err := c.Do(context.Background(), SystemOneRequest{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Do error = %v, want a 503 attribution", err)
	}
}

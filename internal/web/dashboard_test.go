package web

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// dashboardRunDir builds one complete run artifact tree for the
// dashboard tests: summary + frontier (with SD/Reps rows) + sample
// traces (rep_scores) — the P10 endpoint fixture.
func dashboardRunDir(t *testing.T) (string, string) {
	t.Helper()
	runsDir := t.TempDir()
	runID := "20261008-100000-aaaa"
	dir := filepath.Join(runsDir, runID)
	writeJSONT(t, filepath.Join(dir, "summary.json"), core.RunResult{
		RunID: runID, Status: core.StatusCompleted, ExitCode: 0,
		TaskName: "tcm_ner", CandidateID: "c-3", DatasetName: "ds",
		TotalSamples: 2, Evaluated: 2,
		MetricMeans: map[string]float64{"f1": 0.789, "json_validator": 1},
		UsageByRole: map[core.Role]core.Usage{
			core.RoleExecutor: {PromptTokens: 100, CompletionTokens: 50},
			core.RoleJudge:    {PromptTokens: 12, CompletionTokens: 4},
		},
		StartedAt: time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
	})
	writeJSONT(t, filepath.Join(dir, "manifest.json"), map[string]any{
		"run_id": runID, "budget_tokens": 60000, "budget_evals": 120,
	})
	// 2 samples × 2 members: s1 baseline wins with c-3 inside the ε
	// band (near-noise), s2 c-3 wins far outside it.
	writeJSONT(t, filepath.Join(dir, "frontier.json"), engine.FrontierFile{
		Primary: "f1", Constraint: "json_validator",
		Best: engine.FrontierBest{ID: "c-3", Means: map[string]float64{"f1": 0.789, "json_validator": 1}},
		Members: []engine.FrontierMember{
			{ID: "baseline", Operator: engine.OpBaseline, Round: 0, PrimaryMean: 0.62,
				Scores: []float64{0.83, 0.38}, JSONRate: 0.83},
			{ID: "c-3", Operator: "rewrite", Round: 5, PrimaryMean: 0.789,
				Scores: []float64{0.79, 0.90}, SD: []float64{0.04, 0.03}, Reps: 3, JSONRate: 1},
		},
		SampleIDs: []string{"dev-001", "dev-002"},
		Rounds:    5, Reason: engine.ReasonRoundsDone,
	})
	writeJSONT(t, filepath.Join(dir, "lineage.json"), []engine.LineageRecord{
		{ID: "baseline", Operator: engine.OpBaseline, Round: 0,
			Scores: []float64{0.83, 0.38}, PrimaryMean: 0.62, Admitted: true},
		{ID: "c-3", Operator: "rewrite", Round: 5, Parents: []string{"baseline"},
			Scores: []float64{0.79, 0.90}, PrimaryMean: 0.789, Admitted: true},
	})
	writeJSONT(t, filepath.Join(dir, "samples", "dev-001.json"), core.SampleTrace{
		SampleID: "dev-001", Role: core.RoleExecutor, Prompt: "p1", Response: "r1",
		Scores:   map[string]float64{"f1": 0.85, "json_validator": 1},
		ScoresSD: map[string]float64{"f1": 0.04},
		RepScores: []map[string]float64{
			{"f1": 0.83, "json_validator": 1},
			{"f1": 0.79, "json_validator": 1},
			{"f1": 0.85, "json_validator": 1},
		},
		Reps: 3, Usage: core.Usage{PromptTokens: 50, CompletionTokens: 25},
	})
	writeJSONT(t, filepath.Join(dir, "samples", "dev-002.json"), core.SampleTrace{
		SampleID: "dev-002", Role: core.RoleExecutor, Prompt: "p2", Response: "r2",
		Scores: map[string]float64{"f1": 0.76, "json_validator": 1},
		Reps:   3, Usage: core.Usage{PromptTokens: 50, CompletionTokens: 25},
	})
	return runsDir, runID
}

// prototypeStyleBlock extracts the raw <style>…</style> block from the
// redesign prototype (the CSS same-origin source).
func prototypeStyleBlock(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "prototype", "dashboard-redesign.html"))
	if err != nil {
		t.Fatalf("read prototype: %v", err)
	}
	return styleBlock(t, string(b))
}

// styleBlock extracts the first <style>…</style> block of an HTML file.
func styleBlock(t *testing.T, html string) string {
	t.Helper()
	start := strings.Index(html, "<style>")
	end := strings.Index(html, "</style>")
	if start < 0 || end < 0 || end < start {
		t.Fatal("style block not found")
	}
	return html[start+len("<style>") : end]
}

// TestDashboardPageStructure pins the 1:1 structural acceptance: the
// four view ids, the embedded design-token <style> (byte-identical to
// the prototype's block), the roving tablist JS, the run identity and
// the adopt wiring.
func TestDashboardPageStructure(t *testing.T) {
	runsDir, runID := dashboardRunDir(t)
	h := NewServer(runsDir, "", nil).Handler()

	res := get(t, mustURL(t, h, "/runs/"+runID+"/dashboard"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d", res.StatusCode)
	}
	body := res.Body
	for _, want := range []string{
		`id="view-overview"`, `id="view-frontier"`, `id="view-trace"`, `id="view-verify"`,
		`role="tablist"`, `aria-selected`, "ArrowRight", // roving tablist JS 原样
		"<style>", "--bg:#F8FAFC", ".heat .near-noise", ".rep-strip", // 设计令牌同源锚点
		"EventSource('/runs/'+document.body.dataset.run+'/events')", // SSE 接既有回放端点
		`data-run="` + runID + `"`,
		runID, "tcm_ner",
		`data-candidate="c-3"`, // adopt 接线：frontier Best
		"fetch('/runs/'+document.body.dataset.run+'/adopt'",
		"promptopt rollback", // 「回退」卡片维持 CLI 指引形态
		"promptopt verify",   // 未过门禁指引
		"未过门禁",               // 无 verify 工件时的横幅状态
		"/api/",              // footer 真实说明指向数据端点
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	// Demo feed rows are gone; the JS fills the feed from the replay.
	if strings.Contains(body, "演示：每 2.5s 模拟一条") || strings.Contains(body, "14:52:08") {
		t.Error("dashboard still carries the prototype demo feed rows/hint")
	}

	// CSS same-origin: the template's <style> block must stay
	// byte-identical to the prototype's design tokens.
	got := styleBlock(t, mustRead(t, filepath.Join("templates", "dashboard.html")))
	if want := prototypeStyleBlock(t); got != want {
		t.Errorf("dashboard <style> diverged from the prototype (%d vs %d bytes)", len(got), len(want))
	}

	if res := get(t, mustURL(t, h, "/runs/no-such-run/dashboard")); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run dashboard status = %d, want 404", res.StatusCode)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDashboardVerdictBanner drives the gate banner's three states from
// fixture verify.json files (the CLI 结论块的看板侧同一结论).
func TestDashboardVerdictBanner(t *testing.T) {
	cases := []struct {
		name      string
		exitCode  int
		ci        *verifyFileCI
		regressed bool
		want      string
		class     string
	}{
		{"confident_pass", 0, &verifyFileCI{Lo: -0.24, Hi: -0.06, B: 1000, Verdict: "confident_pass"}, false, "置信通过", `class="verdict "`},
		{"regressed", 3, &verifyFileCI{Lo: 0.12, Hi: 0.30, B: 1000, Verdict: "regressed"}, true, "回归", "verdict regress"},
		{"inconclusive", 0, &verifyFileCI{Lo: -0.03, Hi: 0.05, B: 1000, Verdict: "inconclusive"}, false, "不可判定", "verdict inconclusive"},
		{"no-ci pass", 0, nil, false, "通过", `class="verdict "`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runsDir, runID := dashboardRunDir(t)
			dir := filepath.Join(runsDir, runID)
			writeJSONT(t, filepath.Join(dir, "verify", "20261008-110000", "verify.json"), verifyFile{
				RunID: runID, Mode: "anchor", Primary: "f1",
				Baseline:  verifyFileSide{CandidateID: "baseline", Rows: []float64{0.83, 0.38}, Means: map[string]float64{"f1": 0.62}},
				Delivered: verifyFileSide{CandidateID: "c-3", Rows: []float64{0.79, 0.90}, Means: map[string]float64{"f1": 0.789}},
				Regression: verifyFileRegression{
					Primary: "f1", Delta: -0.169, Threshold: 0.05, Regressed: tc.regressed, CI: tc.ci,
				},
				ExitCode: tc.exitCode,
			})
			h := NewServer(runsDir, "", nil).Handler()
			body := get(t, mustURL(t, h, "/runs/"+runID+"/dashboard")).Body
			if !strings.Contains(body, tc.want) {
				t.Errorf("banner missing %q", tc.want)
			}
			if !strings.Contains(body, tc.class) {
				t.Errorf("banner missing class %q", tc.class)
			}
			// 总览与交付门禁两视图渲染同一横幅（两次 include）。
			if n := strings.Count(body, `class="verdict`); n < 2 {
				t.Errorf("banner rendered %d times, want ≥2 (overview top + 交付门禁)", n)
			}
		})
	}
}

// TestDashboardAPIEndpoints covers the five data endpoints: 200 + key
// JSON keys with the full fixture, then the verify endpoint's populated
// shape (ci 三态/lo/hi/B、配对差分 rows、退出码契约表).
func TestDashboardAPIEndpoints(t *testing.T) {
	runsDir, runID := dashboardRunDir(t)
	h := NewServer(runsDir, "", nil).Handler()
	base := mustURL(t, h, "/runs/"+runID)

	// overview: conclusion payload + primary ±sd + budget + judge row.
	ov := decodeJSONT(t, base+"/api/overview")
	for _, key := range []string{"run_id", "available", "status", "primary", "primary_mean", "primary_sd", "metric_means", "budget", "judge", "verdict", "samples"} {
		if _, ok := ov[key]; !ok {
			t.Errorf("overview missing key %q (payload %v)", key, ov)
		}
	}
	if ov["primary_mean"] != 0.789 || ov["primary_sd"] != 0.04 {
		t.Errorf("overview primary mean/sd = %v/%v, want 0.789/0.04", ov["primary_mean"], ov["primary_sd"])
	}
	judge, _ := ov["judge"].(map[string]any)
	if judge == nil || judge["total"] != float64(16) {
		t.Errorf("overview judge row = %v, want total 16 (RoleJudge 单列)", ov["judge"])
	}
	samples, _ := ov["samples"].([]any)
	if len(samples) != 2 {
		t.Fatalf("overview samples = %d, want 2", len(samples))
	}
	first, _ := samples[0].(map[string]any)
	reps, _ := first["rep_scores"].([]any)
	if len(reps) != 3 {
		t.Errorf("dev-001 rep_scores = %v, want 3 reps (rep-strip 数据源)", first["rep_scores"])
	}
	if v, ok := ov["verdict"].(map[string]any); !ok || v["state"] != "pending" {
		t.Errorf("overview verdict without verify artifacts = %v, want state pending (未过门禁)", ov["verdict"])
	}

	// trend: per-round best sequence with the SD error bar.
	tr := decodeJSONT(t, base+"/api/trend")
	if tr["available"] != true || tr["primary"] != "f1" || tr["rounds"] != float64(5) {
		t.Errorf("trend header = %v", tr)
	}
	points, _ := tr["points"].([]any)
	if len(points) != 2 {
		t.Fatalf("trend points = %d, want 2 (round 0 baseline + round 5)", len(points))
	}
	p5, _ := points[1].(map[string]any)
	if p5["best_id"] != "c-3" || p5["primary_mean"] != 0.789 || p5["sd"] != 0.035 {
		t.Errorf("trend round-5 point = %v, want c-3/0.789/sd 0.035", p5)
	}

	// heatmap: scores ± sd + near-noise/win marks + constraint row.
	hm := decodeJSONT(t, base+"/api/heatmap")
	if hm["available"] != true || hm["constraint"] != "json_validator" {
		t.Errorf("heatmap header = %v", hm)
	}
	members, _ := hm["members"].([]any)
	m3, _ := members[1].(map[string]any)
	if m3["reps"] != float64(3) || m3["is_best"] != true {
		t.Errorf("heatmap member c-3 = %v, want reps 3 + is_best", m3)
	}
	rows, _ := hm["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("heatmap rows = %d, want 2", len(rows))
	}
	row0, _ := rows[0].(map[string]any)
	cells0, _ := row0["cells"].([]any)
	c0base, _ := cells0[0].(map[string]any)
	c0c3, _ := cells0[1].(map[string]any)
	if c0base["win"] != true || c0c3["near_noise"] != true || c0c3["sd"] != 0.04 {
		t.Errorf("s1 cells = %v / %v, want baseline win + c-3 near-noise(sd 0.04)", c0base, c0c3)
	}
	row1, _ := rows[1].(map[string]any)
	cells1, _ := row1["cells"].([]any)
	c1c3, _ := cells1[1].(map[string]any)
	if c1c3["win"] != true || c1c3["near_noise"] == true {
		t.Errorf("s2 c-3 cell = %v, want win and outside the ε band", c1c3)
	}
	crow, _ := hm["constraint_row"].([]any)
	if len(crow) != 2 || crow[0] != 0.83 || crow[1] != float64(1) {
		t.Errorf("constraint tfoot row = %v, want [0.83, 1]", crow)
	}

	// lineage: the same builder the frontier page renders.
	ln := decodeJSONT(t, base+"/api/lineage")
	lnRows, _ := ln["rows"].([]any)
	if ln["available"] != true || len(lnRows) != 2 {
		t.Errorf("lineage = %v (%d rows), want available with 2 records", ln, len(lnRows))
	}

	// verify: empty state before any verify run.
	vp := decodeJSONT(t, base+"/api/verify")
	if vp["available"] != false || vp["note"] == "" {
		t.Errorf("verify empty state = %v, want available=false with note", vp)
	}
	if _, ok := vp["exit_codes"]; !ok {
		t.Error("verify payload missing the exit-code contract table")
	}

	// verify: populated shape after a fixture verify run (配对差分表).
	dir := filepath.Join(runsDir, runID)
	writeJSONT(t, filepath.Join(dir, "verify", "20261008-110000", "verify.json"), verifyFile{
		RunID: runID, Mode: "anchor", Primary: "f1",
		Baseline:  verifyFileSide{CandidateID: "baseline", Rows: []float64{0.61, 0.55}, Means: map[string]float64{"f1": 0.62}},
		Delivered: verifyFileSide{CandidateID: "c-3", Rows: []float64{0.92, 0.73}, Means: map[string]float64{"f1": 0.789}},
		Regression: verifyFileRegression{
			Primary: "f1", Delta: -0.169, Threshold: 0.05,
			CI: &verifyFileCI{Lo: -0.24, Hi: -0.06, B: 1000, Verdict: "confident_pass"},
		},
		ExitCode: 0,
	})
	writeJSONT(t, filepath.Join(dir, "verify", "20261008-110000", "anchor-dataset.json"), core.Dataset{
		Name: "anchor",
		Samples: []core.Sample{
			{ID: "anchor-01"}, {ID: "anchor-02"},
		},
	})
	vp = decodeJSONT(t, base+"/api/verify")
	if vp["available"] != true {
		t.Fatalf("verify after fixture = %v", vp)
	}
	rep, _ := vp["report"].(map[string]any)
	reg, _ := rep["regression"].(map[string]any)
	ci, _ := reg["ci"].(map[string]any)
	if ci["lo"] != -0.24 || ci["hi"] != -0.06 || ci["b"] != float64(1000) || ci["verdict"] != "confident_pass" {
		t.Errorf("verify ci = %v, want lo -0.24 / hi -0.06 / B 1000 / confident_pass", ci)
	}
	diffRows, _ := vp["diff_rows"].([]any)
	d0, _ := diffRows[0].(map[string]any)
	d, _ := d0["d"].(float64)
	if d0["sample_id"] != "anchor-01" || math.Abs(d-(-0.31)) > 1e-9 {
		t.Errorf("diff row 0 = %v, want anchor-01 with D=-0.31", d0)
	}

	// index cards link the dashboard (新入口页的可达性).
	body := get(t, mustURL(t, NewServer(runsDir, "", nil).Handler(), "/")).Body
	if !strings.Contains(body, `/runs/`+runID+`/dashboard`) {
		t.Error("index card missing the dashboard link")
	}
}

// TestDashboardAPIEmptyRun pins the degraded shapes: a run directory
// with neither summary nor frontier answers 200 with available=false
// notes instead of failing (live runs land here before artifacts).
func TestDashboardAPIEmptyRun(t *testing.T) {
	runsDir := t.TempDir()
	runID := "20261008-120000-bbbb"
	if err := os.MkdirAll(filepath.Join(runsDir, runID), 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewServer(runsDir, "", nil).Handler()
	for _, ep := range []string{"overview", "trend", "heatmap", "lineage", "verify"} {
		p := decodeJSONT(t, mustURL(t, h, "/runs/"+runID+"/api/"+ep))
		if avail, _ := p["available"].(bool); avail {
			t.Errorf("%s: available=true on an artifact-free run: %v", ep, p)
		}
	}
	// The page itself still renders with the pending banner.
	body := get(t, mustURL(t, h, "/runs/"+runID+"/dashboard")).Body
	if !strings.Contains(body, "未过门禁") {
		t.Error("empty run dashboard missing the pending banner")
	}
}

func decodeJSONT(t *testing.T, url string) map[string]any {
	t.Helper()
	res := get(t, url)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, res.StatusCode)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Body), &m); err != nil {
		t.Fatalf("GET %s: not JSON: %v\n%s", url, err, res.Body)
	}
	return m
}

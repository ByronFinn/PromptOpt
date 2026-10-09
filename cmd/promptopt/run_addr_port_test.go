package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// TestParseRunFlagsAddrPortMatrix pins the P10 方案 (a) address matrix
// at the parse layer: explicit --addr/--port implies the dashboard, the
// resolution rules and the mutual exclusions.
func TestParseRunFlagsAddrPortMatrix(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	out := filepath.Join(t.TempDir(), "runs")
	base := []string{"--task", task, "--candidate", cand, "--dataset", ds,
		"--base-url", srv.URL, "--model", "m", "--out", out}

	cases := []struct {
		name       string
		extra      []string
		wantWeb    bool
		wantListen string
		wantErr    bool
	}{
		{"neither flag stays byte-identical", nil, false, config.DefaultAddr, false},
		{"addr host only implies dashboard", []string{"--addr", "127.0.0.1"}, true, "127.0.0.1:17700", false},
		{"addr host only default port", []string{"--addr", "0.0.0.0"}, true, "0.0.0.0:17700", false},
		{"full address wins as-is", []string{"--web", "--addr", "0.0.0.0:18080"}, true, "0.0.0.0:18080", false},
		{"port only binds loopback", []string{"--port", "17000"}, true, "127.0.0.1:17000", false},
		{"host only addr combines with port", []string{"--addr", "0.0.0.0", "--port", "17000"}, true, "0.0.0.0:17000", false},
		{"full address rejects port", []string{"--addr", "127.0.0.1:17701", "--port", "17000"}, false, "", true},
		{"addr with headless", []string{"--addr", "127.0.0.1", "--headless"}, false, "", true},
		{"port with headless", []string{"--port", "17000", "--headless"}, false, "", true},
		{"web with headless keeps original contract", []string{"--web", "--headless"}, false, "", true},
		{"plain web unchanged", []string{"--web"}, true, config.DefaultAddr, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseRunFlags(append(append([]string{}, base...), tc.extra...))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse = %+v, want error", o)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if o.web != tc.wantWeb {
				t.Errorf("web = %v, want %v", o.web, tc.wantWeb)
			}
			if got := o.listenAddr(); got != tc.wantListen {
				t.Errorf("listenAddr = %q, want %q", got, tc.wantListen)
			}
		})
	}
}

// freePort returns an OS-allocated free loopback port (released before
// the caller rebinds it — the usual test race, acceptable here).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestStartSinkResolvesAddrPortPair proves the resolution reaches a
// real listener: host-only --addr + --port and a full host:port
// --addr both bind exactly the resolved address and serve the
// dashboard (which is the 新入口页 over a run tree).
func TestStartSinkResolvesAddrPortPair(t *testing.T) {
	cases := []struct {
		name string
		o    func(port int) runOptions
	}{
		{"host-only addr + port", func(port int) runOptions {
			return runOptions{addr: "127.0.0.1", addrSet: true, port: port, portSet: true}
		}},
		{"full address as-is", func(port int) runOptions {
			return runOptions{addr: fmt.Sprintf("127.0.0.1:%d", port), addrSet: true}
		}},
		{"port only", func(port int) runOptions {
			return runOptions{port: port, portSet: true}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port := freePort(t)
			outDir := t.TempDir()
			o := tc.o(port)
			o.outDir = outDir
			o.web = true // parseRunFlags 已按矩阵置位；此处直接构造
			sink, err := startSink(o, filepath.Join(outDir, "r"), "")
			if err != nil {
				t.Fatalf("startSink: %v", err)
			}
			defer sink.close()
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
			if err != nil {
				t.Fatalf("GET dashboard: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("index status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// TestStartSinkRejectsBusyAddr keeps the failure path honest: a taken
// address surfaces as a startSink error (exit 1 upstream), not silence.
func TestStartSinkRejectsBusyAddr(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	outDir := t.TempDir()
	o := runOptions{outDir: outDir, web: true,
		addr: fmt.Sprintf("127.0.0.1:%d", port), addrSet: true}
	if _, err := startSink(o, filepath.Join(outDir, "r"), ""); err == nil {
		t.Fatal("startSink on a busy address must fail")
	}
}

// TestPrintRunConclusionKeywords pins the conclusion block's content
// contract at the unit level: 最优候选/Δ/置信 keywords, the 未过门禁
// verify guidance and the dashboard address.
func TestPrintRunConclusionKeywords(t *testing.T) {
	runDir := t.TempDir()
	opt := &engine.Result{
		Best:      core.Candidate{ID: "c-r5-reflect"},
		BestMeans: map[string]float64{"f1": 0.92},
		Frontier: []engine.Member{
			{Candidate: core.Candidate{ID: "baseline"}, Operator: engine.OpBaseline,
				Scores: []float64{0.42}, Means: map[string]float64{"f1": 0.42}},
			{Candidate: core.Candidate{ID: "c-r5-reflect"}, Operator: "rewrite",
				Scores: []float64{0.92}, Means: map[string]float64{"f1": 0.92}},
		},
	}
	res := core.RunResult{RunID: "r1", CandidateID: "c-r5-reflect",
		MetricMeans: map[string]float64{"f1": 0.92}}
	o := runOptions{outDir: "runs", web: true, addr: "127.0.0.1", addrSet: true, port: 17700}

	var buf strings.Builder
	printRunConclusion(&buf, o, res, runDir, "f1", opt)
	out := buf.String()
	for _, want := range []string{
		"--- run 结论 ---",
		"最优候选: c-r5-reflect · f1 0.9200",
		"Δ +0.5000", // 0.92 − 0.42，提升为正
		"配对自助法 CI",
		"置信通过", // D=[−0.5] → CI 上界 ≤ 0
		"未过门禁——运行 promptopt verify r1",
		"http://127.0.0.1:17700/runs/r1/dashboard",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("conclusion missing %q:\n%s", want, out)
		}
	}

	// Manual mode (no optimizer result) degrades honestly instead of
	// inventing a Δ.
	buf.Reset()
	printRunConclusion(&buf, runOptions{outDir: "runs"}, core.RunResult{
		RunID: "r2", CandidateID: "baseline", MetricMeans: map[string]float64{"f1": 0.7},
	}, runDir, "f1", nil)
	manual := buf.String()
	for _, want := range []string{"候选: baseline", "手动模式无优化交付", "verify 门禁不适用"} {
		if !strings.Contains(manual, want) {
			t.Errorf("manual conclusion missing %q:\n%s", want, manual)
		}
	}
}

// TestReadLatestVerifyReportPicksNewest pins the newest-first read of
// runs/<id>/verify/<ts>/verify.json feeding the conclusion's 门禁 line.
func TestReadLatestVerifyReportPicksNewest(t *testing.T) {
	runDir := t.TempDir()
	for _, ts := range []string{"20261008-100000", "20261008-110000"} {
		if err := os.MkdirAll(filepath.Join(runDir, "verify", ts), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFileT(t, filepath.Join(runDir, "verify", "20261008-100000", "verify.json"),
		`{"run_id":"r","primary":"f1","exit_code":3,"regression":{"primary":"f1","regressed":true}}`)
	writeFileT(t, filepath.Join(runDir, "verify", "20261008-110000", "verify.json"),
		`{"run_id":"r","primary":"f1","exit_code":0,"regression":{"primary":"f1","regressed":false}}`)

	rep, ok := readLatestVerifyReport(runDir)
	if !ok || rep.ExitCode != 0 {
		t.Fatalf("latest verify = %+v ok=%v, want the 110000 report", rep, ok)
	}
	line := verifyGateLine(rep)
	if !strings.Contains(line, "均值差判定") || !strings.Contains(line, "通过") {
		t.Errorf("gate line = %q, want the no-CI pass shape", line)
	}
	if _, ok := readLatestVerifyReport(t.TempDir()); ok {
		t.Error("verify report found in an unverified run dir")
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// startConclusionLLM drives a zero-config run whose delivered candidate
// strictly beats the baseline on the retained set (mutations answer the
// kept sample correctly, the baseline misses) — the delivery verdict
// then reads 置信通过 in the human conclusion block.
func startConclusionLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		w.Header().Set("Content-Type", "application/json")
		var content string
		switch {
		case strings.Contains(body, harness.MarkerSpec):
			content = zcSpec
		case strings.Contains(body, harness.MarkerSamples):
			content = zcSamples
		case strings.Contains(body, harness.MarkerProbes):
			content = zcProbes
		case strings.Contains(body, engine.MarkerReflect):
			content = zcHypotheses
		case strings.Contains(body, engine.MarkerRewrite),
			strings.Contains(body, engine.MarkerMerge),
			strings.Contains(body, engine.MarkerFresh):
			content = zcMutation
		default:
			// 评估调用：优化候选命中保留样本 a2 的期望，基线与探针落空
			// （探针全空 → fallback-all 保留全集；基线 [0,0,0]，候选
			// [0,1,0] 支配基线 → Δ>0，配对自助法 CI 上界 ≤ 0 → 置信通过）。
			if strings.Contains(body, "优化后的辨证提示词") {
				content = "心肾不交"
			} else {
				content = "无法辨证"
			}
		}
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunHumanConclusionBlockEndToEnd runs the zero-config CLI in
// human mode and asserts the conclusion keywords on stderr; the
// headless rerun pins the stdout JSON contract against pollution.
func TestRunHumanConclusionBlockEndToEnd(t *testing.T) {
	srv := startConclusionLLM(t)
	prompt := "从中医医案文本判断证候"
	common := func(outDir string) []string {
		return []string{prompt, "--base-url", srv.URL, "--model", "jiuwei-tcm",
			"--out", outDir, "--samples", "3", "--probe-variants", "2"}
	}

	// Human mode: the conclusion block lives on stderr only.
	code, stdout, stderr := runCliVerbose(t, common(filepath.Join(t.TempDir(), "runs"))...)
	if code != 0 {
		t.Fatalf("human run exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"--- run 结论 ---", "最优候选", "Δ", "置信", "promptopt verify"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("human stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "run 结论") {
		t.Errorf("human stdout polluted by the conclusion block:\n%s", stdout)
	}

	// Headless mode: stdout stays the single-line JSON summary.
	headless := append(common(filepath.Join(t.TempDir(), "runs")), "--headless")
	code, stdout, stderr = runCliVerbose(t, headless...)
	if code != 0 {
		t.Fatalf("headless run exit = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--- run 结论 ---") || !strings.Contains(stderr, "最优候选") {
		t.Errorf("headless stderr missing the conclusion block:\n%s", stderr)
	}
	line := strings.TrimRight(stdout, "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("headless stdout is not single-line:\n%s", stdout)
	}
	if strings.Contains(stdout, "最优候选") || strings.Contains(stdout, "run 结论") {
		t.Errorf("headless stdout polluted:\n%s", stdout)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("headless stdout not JSON: %v\n%s", err, stdout)
	}
}

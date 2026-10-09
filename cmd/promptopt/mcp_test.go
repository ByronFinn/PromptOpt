package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/mcp"
)

// serveMCP drives the production handler wiring over pipes: in carries
// JSON-RPC request lines, the returned slices are the response frames.
func serveMCP(t *testing.T, handlers mcp.Handlers, in string) []json.RawMessage {
	t.Helper()
	var out bytes.Buffer
	srv := &mcp.Server{
		In: strings.NewReader(in), Out: &out,
		Err:  os.Stderr,
		Name: "promptopt", Version: "test",
		Handlers: handlers,
	}
	if err := srv.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return decodeRPCFrames(t, out.String())
}

// decodeRPCFrames asserts stdout purity at the transport level: every
// byte the server wrote parses as a JSON-RPC 2.0 frame — non-JSON-RPC
// bytes are zero (依赖 out 注入生效：run/verify 侧 headless JSON 写入
// io.Discard，不落此通道).
func decodeRPCFrames(t *testing.T, raw string) []json.RawMessage {
	t.Helper()
	var frames []json.RawMessage
	for i, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		var f struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("JSON-RPC 通道被污染（第 %d 行非 JSON-RPC）：%v\n原始字节：%q", i+1, err, line)
		}
		if f.JSONRPC != "2.0" || (f.Result == nil) == (f.Error == nil) {
			t.Fatalf("JSON-RPC 通道被污染（第 %d 行非响应帧）：%q", i+1, line)
		}
		frames = append(frames, json.RawMessage(line))
	}
	return frames
}

// mcpResult unpacks one tools/call in-band result.
type mcpResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// rpcErrorFrame unpacks one protocol-level error frame.
func rpcErrorFrame(t *testing.T, raw json.RawMessage) (code int, message string) {
	t.Helper()
	var f struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if f.Error == nil {
		t.Fatalf("预期协议错误帧，得到：%s", raw)
	}
	return f.Error.Code, f.Error.Message
}

func decodeMCPResult(t *testing.T, raw json.RawMessage) mcpResult {
	t.Helper()
	var f struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if f.Error != nil {
		t.Fatalf("意外协议错误 %d: %s", f.Error.Code, f.Error.Message)
	}
	var res mcpResult
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return res
}

func optimizeCall(t *testing.T, id int, task, cand, ds, baseURL, model, out string) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{
		"task": task, "candidate": cand, "dataset": ds,
		"base_url": baseURL, "model": model, "out": out,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"optimize","arguments":%s}}`+"\n", id, args)
}

// assertSameJSON pins the disk source-of-truth contract: the tool
// payload decodes from the on-disk artifact (transit re-encodes maps,
// so the comparison is semantic).
func assertSameJSON(t *testing.T, payload json.RawMessage, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got, want any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if !bytes.Equal(gb, wb) {
		t.Errorf("载荷与磁盘工件不一致:\n%s\n%s", gb, wb)
	}
}

// TestMCPOptimizeE2EManualTrio covers ③ (optimize leg): a real headless
// run through the production seams against an httptest stub — the
// response payload equals runDir/summary.json on disk.
func TestMCPOptimizeE2EManualTrio(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")
	handlers := newMCPHandlers(outDir, execRun, execVerify)

	frames := serveMCP(t, handlers, optimizeCall(t, 1, task, cand, ds, srv.URL, "fake-model", outDir))
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	res := decodeMCPResult(t, frames[0])
	if res.IsError {
		t.Fatalf("optimize 意外失败：%s", res.Content[0].Text)
	}

	// The pinned run id makes discovery deterministic: one run dir.
	runID := onlyRunDir(t, outDir)
	runDir := filepath.Join(outDir, runID)
	assertSameJSON(t, res.StructuredContent, filepath.Join(runDir, "summary.json"))
	// content[0].text is the disk bytes verbatim.
	b, _ := os.ReadFile(filepath.Join(runDir, "summary.json"))
	if res.Content[0].Text != string(b) {
		t.Error("content text 应逐字等于磁盘 summary.json")
	}
	var sum core.RunResult
	if err := json.Unmarshal(res.StructuredContent, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.ExitCode != 0 || sum.Status != core.StatusCompleted || sum.MetricMeans["exact_match"] != 1 {
		t.Errorf("run 摘要异常: %+v", sum)
	}
	if sum.RunID != runID {
		t.Errorf("payload run_id=%q 与目录名 %q 不一致", sum.RunID, runID)
	}
}

// TestMCPVerifyE2E covers ③ (verify leg): a real zero-config run, then
// tools/call verify — the response payload equals verifyDir/verify.json
// on disk and carries the three-state verdict with its CI payload.
func TestMCPVerifyE2E(t *testing.T) {
	runsDir, runID := zeroConfigRunTree(t)
	// The MCP verify surface carries no connection flags on purpose:
	// resolveVerifyConn reproduces the run's endpoint from the manifest
	// snapshot (flag > env > manifest), and zeroConfigRunTree's stub is
	// still alive to answer the holdout synthesis and both evaluation
	// sides — this pins the manifest-reproduction chain end to end.
	// The scripted run delivers its baseline (every mutation got
	// rejected as an ε-clone), so verify would skip; adopting a
	// different prompt (the dashboard's adopt flow artifact) forces a
	// real evaluation, and the stub answers it correctly from the
	// script table → both sides score 1.
	adoptForVerify(t, runsDir, runID, "hand-good", "优化后的辨证提示词。文本：{input}")
	handlers := newMCPHandlers(runsDir, execRun, execVerify)

	args, _ := json.Marshal(map[string]any{"run_id": runID, "runs_dir": runsDir})
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"verify","arguments":%s}}`+"\n", args)
	frames := serveMCP(t, handlers, req)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	res := decodeMCPResult(t, frames[0])
	if res.IsError {
		t.Fatalf("verify 意外失败：%s", res.Content[0].Text)
	}

	verifyRoot := filepath.Join(runsDir, runID, "verify")
	entries, err := os.ReadDir(verifyRoot)
	if err != nil || len(entries) == 0 {
		t.Fatalf("verify dirs = %v (%v)", entries, err)
	}
	verifyDir := filepath.Join(verifyRoot, entries[len(entries)-1].Name())
	assertSameJSON(t, res.StructuredContent, filepath.Join(verifyDir, "verify.json"))

	var rep verifyReport
	if err := json.Unmarshal(res.StructuredContent, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.RunID != runID || rep.Mode != verifyModeHoldout {
		t.Errorf("verify 报告头异常: run=%s mode=%s", rep.RunID, rep.Mode)
	}
	if rep.Regression.CI == nil || rep.Regression.CI.Verdict == "" {
		t.Errorf("CI 载荷缺失: %+v", rep.Regression)
	}
	if rep.ExitCode != exitOK {
		t.Errorf("预期通过（两端同分），exit = %d", rep.ExitCode)
	}
}

// TestMCPSignalNoBacklog covers ⑦: N consecutive tools/call optimize in
// one stdio session — after every call the sink registry reads zero
// (每次 NotifyContext 均被 stop), and each call leaves its own run dir.
func TestMCPSignalNoBacklog(t *testing.T) {
	task, cand, ds := fixtureYAMLs(t)
	srv := startFakeLLM(t)
	outDir := filepath.Join(t.TempDir(), "runs")
	handlers := newMCPHandlers(outDir, execRun, execVerify)

	var in strings.Builder
	const n = 3
	for i := 1; i <= n; i++ {
		in.WriteString(optimizeCall(t, i, task, cand, ds, srv.URL, "fake-model", outDir))
	}
	frames := serveMCP(t, handlers, in.String())
	if len(frames) != n {
		t.Fatalf("frames = %d, want %d", len(frames), n)
	}
	for i, f := range frames {
		res := decodeMCPResult(t, f)
		if res.IsError {
			t.Fatalf("第 %d 次 optimize 失败：%s", i+1, res.Content[0].Text)
		}
		if got := countLiveSinks(); got != 0 {
			t.Errorf("第 %d 次调用后信号上下文积压：%d 个 runSink 未关闭", i+1, got)
		}
	}
	entries, err := os.ReadDir(outDir)
	if err != nil || len(entries) != n {
		t.Fatalf("run dirs = %d (%v), want %d", len(entries), err, n)
	}
}

// TestMCPPanicReclaimsSink pins the panic leg of the「每次调用一个
// sink、调用必关闭」contract: a run that opens a real sink and then
// panics gets reclaimed during unwind (closeOrphanedSinks, before the
// server's recover turns the panic into -32603) — no backlog, session
// alive.
func TestMCPPanicReclaimsSink(t *testing.T) {
	outDir := filepath.Join(t.TempDir(), "runs")
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	panicRun := func(o runOptions, _ io.Writer) int {
		sink, err := startSink(o, filepath.Join(o.outDir, o.runID), "")
		if err != nil {
			t.Errorf("startSink: %v", err)
			return exitFailure
		}
		_ = sink // 故意不 close：模拟 run 路径中途 panic
		panic("模拟 run 中途 panic")
	}
	handlers := newMCPHandlers(outDir, panicRun, execVerify)

	frames := serveMCP(t, handlers, optimizeCall(t, 1, task, cand, ds, srv.URL, "fake-model", outDir))
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1（panic 后服务器必须存活）", len(frames))
	}
	if code, msg := rpcErrorFrame(t, frames[0]); code != -32603 || !strings.Contains(msg, "模拟 run 中途 panic") {
		t.Errorf("panic 应转 -32603 并携带 panic 值，得到 %d: %s", code, msg)
	}
	if got := countLiveSinks(); got != 0 {
		t.Errorf("panic 路径遗留 %d 个 runSink 未回收", got)
	}
}

// TestMCPRunSideStdoutSilence covers ④'s mechanism: with os.Stdout
// swapped into a capture buffer, the whole optimize tool call (the run
// path with io.Discard injected) leaves it untouched — the
// pre-injection code would have written the headless summary there.
func TestMCPRunSideStdoutSilence(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	_, exit, toolErr := optimizeTool(outDir, mcp.OptimizeArgs{
		Task: task, Candidate: cand, Dataset: ds,
		BaseURL: srv.URL, Model: "fake-model", Out: outDir,
	}, execRun)
	os.Stdout = old
	_ = w.Close()
	leaked, _ := io.ReadAll(r)

	if toolErr != nil {
		t.Fatalf("optimize tool: %v", toolErr)
	}
	if exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	if len(leaked) != 0 {
		t.Errorf("run 侧泄漏 %d 字节到 stdout: %q", len(leaked), leaked)
	}
	if got := countLiveSinks(); got != 0 {
		t.Errorf("sink 积压：%d", got)
	}
}

// TestCLIHeadlessByteContract supports ⑤ inside the suite: the CLI
// headless run summary and verify report stay one-line compact JSON +
// trailing newline — json.Encoder.Encode's exact shape on os.Stdout —
// so the injected writer is byte-identical to the pre-refactor bytes.
func TestCLIHeadlessByteContract(t *testing.T) {
	srv := startFakeLLM(t)
	task, cand, ds := fixtureYAMLs(t)
	outDir := filepath.Join(t.TempDir(), "runs")

	code, out := runCli(t, append(baseFlags(t, srv, task, cand, ds), "--out", outDir, "--headless")...)
	if code != exitOK {
		t.Fatalf("exit = %d, stdout:\n%s", code, out)
	}
	var res core.RunResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("headless 输出不是 JSON: %v", err)
	}
	compact, _ := json.Marshal(res)
	if want := string(compact) + "\n"; out != want {
		t.Errorf("headless 摘要非「紧凑单行 + 换行」契约:\n got %q\nwant %q", out, want)
	}

	// verify leg: finishVerify's headless branch obeys the same shape.
	rep := verifyReport{
		RunID: "r-pin", VerifiedAt: time.Now().UTC(), Mode: verifyModeSkipped,
		Primary: "exact_match", ExitCode: exitOK,
		Note: "交付候选与 baseline 提示词相同（该 run 无优化交付），跳过评估",
	}
	verifyDir := t.TempDir()
	codeV, outV := captureCli(t, func([]string) int {
		return finishVerify(verifyOptions{headless: true}, rep, verifyDir, os.Stdout)
	})
	if codeV != exitOK {
		t.Fatalf("finishVerify exit = %d", codeV)
	}
	var rep2 verifyReport
	if err := json.Unmarshal([]byte(outV), &rep2); err != nil {
		t.Fatalf("headless 报告不是 JSON: %v", err)
	}
	compactV, _ := json.Marshal(rep2)
	if want := string(compactV) + "\n"; outV != want {
		t.Errorf("headless 报告非「紧凑单行 + 换行」契约:\n got %q\nwant %q", outV, want)
	}
}

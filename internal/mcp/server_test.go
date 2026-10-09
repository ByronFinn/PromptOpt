package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve drives one server session over pipes and returns every frame
// it wrote (one JSON object per line).
func serve(t *testing.T, in string, h Handlers) []json.RawMessage {
	t.Helper()
	var out bytes.Buffer
	s := &Server{In: strings.NewReader(in), Out: &out, Err: os.Stderr, Handlers: h, Version: "test"}
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return decodeFrames(t, out.String())
}

// decodeFrames asserts stdout purity at the protocol level: every byte
// the server wrote parses as a JSON-RPC 2.0 frame (result xor error).
func decodeFrames(t *testing.T, raw string) []json.RawMessage {
	t.Helper()
	var frames []json.RawMessage
	for i, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		var f struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("stdout 非纯净 JSON-RPC（第 %d 行）：%v\n原始字节：%q", i+1, err, line)
		}
		if f.JSONRPC != "2.0" {
			t.Fatalf("stdout 非纯净 JSON-RPC（第 %d 行）：jsonrpc=%q", i+1, f.JSONRPC)
		}
		if (f.Result == nil) == (f.Error == nil) {
			t.Fatalf("stdout 非纯净 JSON-RPC（第 %d 行）：result 与 error 必须恰有其一", i+1)
		}
		frames = append(frames, json.RawMessage(line))
	}
	return frames
}

// rpcCall builds one JSON-RPC request line.
func rpcCall(id int, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + "}\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mustFrame(t *testing.T, raw json.RawMessage) frame {
	t.Helper()
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return f
}

// TestInitializeHandshakeAndToolsList covers ①: the handshake names the
// server and its pinned protocol version; tools/list returns exactly
// the three frozen tools.
func TestInitializeHandshakeAndToolsList(t *testing.T) {
	frames := serve(t,
		rpcCall(1, "initialize", `{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}`)+
			rpcCall(2, "tools/list", `{}`), Handlers{})
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(mustFrame(t, frames[0]).Result, &init); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if init.ProtocolVersion != protocolVersion {
		t.Errorf("protocolVersion = %q, want pinned %q", init.ProtocolVersion, protocolVersion)
	}
	if init.ServerInfo.Name != "promptopt" || init.ServerInfo.Version != "test" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}
	if _, ok := init.Capabilities["tools"]; !ok {
		t.Errorf("capabilities 缺少 tools: %+v", init.Capabilities)
	}

	var list struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(mustFrame(t, frames[1]).Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(list.Tools) != 3 {
		t.Fatalf("tools = %d, want exactly 3", len(list.Tools))
	}
	want := map[string]bool{"optimize": false, "verify": false, "runs": false}
	for _, tool := range list.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Fatalf("意外工具 %q", tool.Name)
		}
		want[tool.Name] = true
		if tool.InputSchema["type"] != "object" {
			t.Errorf("工具 %s 的 inputSchema.type != object", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("工具 %s 缺少描述", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("缺少工具 %s", name)
		}
	}
}

// TestUnknownMethodReturnsError covers ⑥: an unknown method answers
// -32601 and the server keeps serving the next request.
func TestUnknownMethodReturnsError(t *testing.T) {
	frames := serve(t,
		rpcCall(1, "resources/list", `{}`)+
			rpcCall(2, "tools/list", `{}`), Handlers{})
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2（服务器未随未知方法崩溃）", len(frames))
	}
	f := mustFrame(t, frames[0])
	if f.Error == nil || f.Error.Code != codeMethod {
		t.Fatalf("error = %+v, want -32601", f.Error)
	}
	if f.Error.Message == "" {
		t.Error("error.message 为空")
	}
	if f2 := mustFrame(t, frames[1]); f2.Error != nil {
		t.Fatalf("后续请求失败：%+v", f2.Error)
	}
}

// TestParseErrorAndInvalidRequest pins the two protocol-level failure
// frames (garbage line, request missing method).
func TestParseErrorAndInvalidRequest(t *testing.T) {
	frames := serve(t, "这不是 JSON\n"+`{"jsonrpc":"2.0","id":7}`+"\n", Handlers{})
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if f := mustFrame(t, frames[0]); f.Error == nil || f.Error.Code != codeParse {
		t.Errorf("parse error = %+v, want -32700", f.Error)
	}
	if f := mustFrame(t, frames[1]); f.Error == nil || f.Error.Code != codeInvalidReq {
		t.Errorf("invalid request = %+v, want -32600", f.Error)
	}
}

// TestNotificationGetsNoResponse pins the notification discipline: no
// id, no frame — the next response must be the only line.
func TestNotificationGetsNoResponse(t *testing.T) {
	frames := serve(t,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"+
			rpcCall(1, "tools/list", `{}`), Handlers{})
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1（通知不得产生响应）", len(frames))
	}
}

// TestToolCallPanicBecomesRPCError pins the recover contract: a
// panicking handler surfaces as -32603 and the session survives.
func TestToolCallPanicBecomesRPCError(t *testing.T) {
	frames := serve(t, rpcCall(1, "tools/call", `{"name":"optimize","arguments":{"prompt":"p","base_url":"u","model":"m"}}`)+
		rpcCall(2, "tools/list", `{}`), Handlers{
		Optimize: func(OptimizeArgs) (string, int, error) { panic("boom") },
	})
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2（panic 后服务器必须存活）", len(frames))
	}
	f := mustFrame(t, frames[0])
	if f.Error == nil || f.Error.Code != codeInternal {
		t.Fatalf("error = %+v, want -32603", f.Error)
	}
	if !strings.Contains(f.Error.Message, "boom") {
		t.Errorf("error.message 应携带 panic 值: %q", f.Error.Message)
	}
}

// TestUnknownToolAndBadParams pins the -32602 boundary: unknown tool
// name and schema-violating arguments are protocol errors.
func TestUnknownToolAndBadParams(t *testing.T) {
	frames := serve(t,
		rpcCall(1, "tools/call", `{"name":"banana"}`)+
			rpcCall(2, "tools/call", `{"name":"optimize","arguments":{"task":"a"}}`)+
			rpcCall(3, "tools/call", `{"name":"optimize"}`), Handlers{})
	for i, want := range []int{codeParams, codeParams, codeParams} {
		f := mustFrame(t, frames[i])
		if f.Error == nil || f.Error.Code != want {
			t.Errorf("frame %d error = %+v, want %d", i+1, f.Error, want)
		}
	}
}

// seedRun writes one run dir with manifest + summary artifacts.
func seedRun(t *testing.T, root, id, task string, exitCode int) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	created := "2026-01-02T03:04:05Z"
	write(t, filepath.Join(dir, "manifest.json"),
		`{"run_id":"`+id+`","created_at":"`+created+`","task":"`+task+`","candidate":"c1","model":"m"}`)
	write(t, filepath.Join(dir, runArtifactName),
		`{"run_id":"`+id+`","status":"completed","exit_code":`+itoa(exitCode)+`,"metric_means":{"exact_match":1}}`)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestToolsCallRunsCards covers ②: a temp runs dir yields card list —
// sorted newest first, skipping non-run directories.
func TestToolsCallRunsCards(t *testing.T) {
	root := t.TempDir()
	seedRun(t, root, "20260102-030405-aaaa", "旧任务", 0)
	seedRun(t, root, "20260103-030405-bbbb", "新任务", 0)
	if err := os.MkdirAll(filepath.Join(root, "not-a-run"), 0o755); err != nil {
		t.Fatal(err)
	}

	frames := serve(t, rpcCall(1, "tools/call", `{"name":"runs"}`), Handlers{RunsDir: root})
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	f := mustFrame(t, frames[0])
	if f.Error != nil {
		t.Fatalf("runs 失败：%+v", f.Error)
	}
	var res struct {
		StructuredContent struct {
			Runs []RunCard `json:"runs"`
		} `json:"structuredContent"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatalf("decode runs result: %v", err)
	}
	cards := res.StructuredContent.Runs
	if len(cards) != 2 {
		t.Fatalf("cards = %d, want 2（非 run 目录跳过）", len(cards))
	}
	if cards[0].RunID != "20260103-030405-bbbb" || cards[1].RunID != "20260102-030405-aaaa" {
		t.Errorf("卡片未按新到旧排序: %s, %s", cards[0].RunID, cards[1].RunID)
	}
	if cards[0].Task != "新任务" || cards[0].Status != "completed" || cards[0].Means["exact_match"] != 1 {
		t.Errorf("卡片字段不完整: %+v", cards[0])
	}
}

// TestOptimizePayloadIsDiskSummary pins the disk source-of-truth path:
// the structured payload decodes from runDir/summary.json bytes, not
// from any in-memory re-encode; exit 1 turns into isError while the
// payload still rides along.
func TestOptimizePayloadIsDiskSummary(t *testing.T) {
	runDir := t.TempDir()
	want := `{"run_id":"rX","status":"completed","exit_code":0,"metric_means":{"f1":0.5}}`
	write(t, filepath.Join(runDir, runArtifactName), want)

	frames := serve(t, rpcCall(1, "tools/call", `{"name":"optimize","arguments":{"prompt":"p","base_url":"http://x","model":"m"}}`),
		Handlers{Optimize: func(OptimizeArgs) (string, int, error) { return runDir, 0, nil }})
	f := mustFrame(t, frames[0])
	if f.Error != nil {
		t.Fatalf("optimize 失败：%+v", f.Error)
	}
	var res struct {
		Content           []Content       `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Error("exit 0 不应 isError")
	}
	// content[0].text 是磁盘字节逐字真源；structuredContent 经传输层
	// 重编码（键序按 map 序），按语义等价对照。
	if res.Content[0].Text != want {
		t.Errorf("content 文本应逐字等于磁盘 summary.json:\n%q\n%q", res.Content[0].Text, want)
	}
	var got, wantPayload any
	if err := json.Unmarshal(res.StructuredContent, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantPayload); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(t, got, wantPayload) {
		t.Errorf("载荷与磁盘 summary.json 语义不一致:\n%v\n%v", got, wantPayload)
	}

	// Exit 1 with an intact payload: in-band tool error, payload rides.
	frames = serve(t, rpcCall(1, "tools/call", `{"name":"optimize","arguments":{"prompt":"p","base_url":"http://x","model":"m"}}`),
		Handlers{Optimize: func(OptimizeArgs) (string, int, error) { return runDir, 1, nil }})
	var res2 struct {
		IsError bool `json:"isError"`
	}
	_ = json.Unmarshal(mustFrame(t, frames[0]).Result, &res2)
	if !res2.IsError {
		t.Error("exit 1 应 isError")
	}

	// No artifacts at all: in-band error result, not a protocol error.
	frames = serve(t, rpcCall(1, "tools/call", `{"name":"optimize","arguments":{"prompt":"p","base_url":"http://x","model":"m"}}`),
		Handlers{Optimize: func(OptimizeArgs) (string, int, error) { return "", 1, os.ErrNotExist }})
	var res3 struct {
		Error  *struct{ Code int } `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(frames[0], &res3); err != nil {
		t.Fatal(err)
	}
	if res3.Error != nil || !res3.Result.IsError || len(res3.Result.Content) == 0 {
		t.Errorf("无工件失败应为 in-band isError result: %+v", res3)
	}
}

// TestVerifyPayloadIsDiskReport mirrors the disk-source pin for verify.
func TestVerifyPayloadIsDiskReport(t *testing.T) {
	verifyDir := t.TempDir()
	want := `{"run_id":"r1","mode":"holdout","exit_code":3,"regression":{"regressed":true}}`
	write(t, filepath.Join(verifyDir, verifyArtifactName), want)

	frames := serve(t, rpcCall(1, "tools/call", `{"name":"verify","arguments":{"run_id":"r1"}}`),
		Handlers{Verify: func(VerifyArgs) (string, int, error) { return verifyDir, 3, nil }})
	f := mustFrame(t, frames[0])
	if f.Error != nil {
		t.Fatalf("verify 失败：%+v", f.Error)
	}
	var res struct {
		StructuredContent struct {
			Mode       string `json:"mode"`
			Regression struct {
				Regressed bool `json:"regressed"`
			} `json:"regression"`
		} `json:"structuredContent"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(f.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Error("退出码 3（回归）是有效结论，不应 isError")
	}
	if res.StructuredContent.Mode != "holdout" || !res.StructuredContent.Regression.Regressed {
		t.Errorf("载荷与磁盘 verify.json 不一致: %+v", res.StructuredContent)
	}
}

// TestValidateOptimizeArgs pins the schema edge cases at the unit
// level (trio XOR prompt, env fallback for connection fields).
func TestValidateOptimizeArgs(t *testing.T) {
	t.Setenv("PROMPTOPT_BASE_URL", "")
	t.Setenv("PROMPTOPT_MODEL", "")
	if err := validateOptimizeArgs(OptimizeArgs{Task: "t", Candidate: "c", Dataset: "d"}); err == nil {
		t.Error("三件套缺 base_url/model 应拒绝")
	}
	if err := validateOptimizeArgs(OptimizeArgs{Task: "t", Candidate: "c", Dataset: "d", BaseURL: "u", Model: "m"}); err != nil {
		t.Errorf("合法三件套被拒: %v", err)
	}
	if err := validateOptimizeArgs(OptimizeArgs{Task: "t", Candidate: "c", Dataset: "d", Prompt: "p", BaseURL: "u", Model: "m"}); err == nil {
		t.Error("三件套与 prompt 并用应拒绝")
	}
	if err := validateOptimizeArgs(OptimizeArgs{BaseURL: "u", Model: "m"}); err == nil {
		t.Error("既无三件套也无 prompt 应拒绝")
	}
	if err := validateOptimizeArgs(OptimizeArgs{Prompt: "p", BaseURL: "u", Model: "m", BudgetEvals: -1}); err == nil {
		t.Error("负预算应拒绝")
	}
	t.Setenv("PROMPTOPT_BASE_URL", "http://env")
	t.Setenv("PROMPTOPT_MODEL", "env-model")
	if err := validateOptimizeArgs(OptimizeArgs{Prompt: "p"}); err != nil {
		t.Errorf("env 回落应放行: %v", err)
	}
}

// equalJSON compares two decoded JSON values semantically.
func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(ab, bb)
}

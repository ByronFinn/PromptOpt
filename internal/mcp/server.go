// Package mcp is PromptOpt's minimal single-machine API surface: a
// stdio JSON-RPC 2.0 server (newline-delimited messages) exposing
// exactly three tools — optimize, verify, runs (提案 §2.4 V8 候选最小版
// 提前落地).
//
// 取舍（提案 §2.4 随附）：MCP spec 仍在演化，本包用标准库自实现一个
// 固化最小面（initialize / tools/list / tools/call，协议版本钉
// protocolVersion），不引官方 SDK——spec 跟随成本由此记录在案，V8
// 立项时再评估官方 Go SDK 依赖（标准库优先原则要求依赖给出明确理由）。
//
// 单机边界（PRD-0000 Out of Scope 不动摇）：一个 stdio 会话、无鉴权、
// 无多用户——本地面板只服务本机的一个 agent。stdout 是 JSON-RPC 通道：
// 除响应帧外任何字节都不得写入（run/verify 子路径经 out 注入
// io.Discard 静默，人类可读输出一律走 stderr）。
//
// 信号上下文契约（tools/call optimize 的 run 路径每次调用一个
// runSink）：正常与早退路径由 run 路径自身 close；panic 路径由本包把
// panic 转成 JSON-RPC error（-32603），cmd 侧 handler 的
// closeOrphanedSinks 兜底回收 sink（其 stop() 释放 NotifyContext），
// 反复调用不积压信号处理器——测试以计数探针断言无积压。
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// protocolVersion pins the minimal face this server speaks. The MCP
// spec keeps evolving; the server declares one version instead of
// negotiating a family it does not implement.
const protocolVersion = "2024-11-05"

// JSON-RPC 2.0 error codes used by this server.
const (
	codeParse      = -32700
	codeInvalidReq = -32600
	codeMethod     = -32601
	codeParams     = -32602
	codeInternal   = -32603
)

// maxMessageBytes bounds one inbound JSON-RPC line. Tool arguments are
// paths, prompts and small numbers; 8 MiB leaves orders of magnitude
// of headroom while a runaway peer cannot exhaust memory.
const maxMessageBytes = 8 << 20

// Server serves one stdio JSON-RPC session. In/Out/Err are required;
// Out carries nothing but JSON-RPC frames.
type Server struct {
	In       io.Reader
	Out      io.Writer
	Err      io.Writer // human-readable diagnostics; never protocol frames
	Handlers Handlers
	// Name and Version fill serverInfo in the initialize result.
	Name    string
	Version string

	tools []Tool
}

// request is one inbound JSON-RPC 2.0 message. A missing (or null) id
// marks a notification: it never earns a response.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is one outbound frame; result and error are mutually
// exclusive per JSON-RPC 2.0.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError carries the protocol-level failure.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads newline-delimited requests until EOF (stdin closed ends
// the session cleanly). Malformed lines answer with a parse error and
// the session continues; a panicking tool handler is converted into an
// internal-error frame so one bad call cannot kill the server.
func (s *Server) Serve() error {
	if s.Out == nil || s.In == nil {
		return fmt.Errorf("mcp: In 与 Out 必须注入")
	}
	if s.tools == nil {
		s.tools = DefaultTools()
	}
	enc := json.NewEncoder(s.Out)
	sc := bufio.NewScanner(s.In)
	sc.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		req, rerr := decodeRequest(line)
		if rerr != nil {
			if err := enc.Encode(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: rerr}); err != nil {
				return err
			}
			continue
		}
		res, rerr := s.dispatch(req)
		// Notifications never earn a response — not even errors.
		if isNotification(req) {
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			resp.Result = res
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	end := len(b)
	for end > start && (b[end-1] == ' ' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}

func decodeRequest(line []byte) (*request, *rpcError) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil, &rpcError{codeParse, "parse error: 请求不是合法 JSON"}
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return nil, &rpcError{codeInvalidReq, "invalid request: 缺少 jsonrpc:\"2.0\" 或 method"}
	}
	return &req, nil
}

// isNotification reports an absent or null id — such messages get no
// response frame by spec.
func isNotification(req *request) bool {
	return len(req.ID) == 0 || string(req.ID) == "null"
}

// dispatch routes one request; it never panics into the caller (tool
// panics are recovered inside callTool).
func (s *Server) dispatch(req *request) (res any, rerr *rpcError) {
	switch req.Method {
	case "initialize":
		return initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      map[string]any{"name": s.serverName(), "version": s.Version},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		type toolsListResult struct {
			Tools []Tool `json:"tools"`
		}
		return toolsListResult{Tools: s.tools}, nil
	case "tools/call":
		return s.callTool(req.Params)
	default:
		return nil, &rpcError{codeMethod, fmt.Sprintf("method not found: %q", req.Method)}
	}
}

func (s *Server) serverName() string {
	if s.Name != "" {
		return s.Name
	}
	return "promptopt"
}

// initializeResult is the initialize handshake payload.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo"`
}

// callTool resolves tools/call params to one tool implementation.
// Protocol-level problems (unknown tool, malformed arguments) answer
// with JSON-RPC errors; execution problems (the run failed, artifacts
// missing) answer in-band as ToolResult{IsError:true} per the MCP
// convention. A panicking handler recovers into -32603 so the server
// survives.
func (s *Server) callTool(params json.RawMessage) (res any, rerr *rpcError) {
	defer func() {
		if r := recover(); r != nil {
			res, rerr = nil, &rpcError{codeInternal, fmt.Sprintf("internal error: 工具调用 panic: %v", r)}
		}
	}()
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{codeParams, fmt.Sprintf("invalid params: %v", err)}
	}
	switch p.Name {
	case "optimize":
		return s.toolOptimize(p.Arguments)
	case "verify":
		return s.toolVerify(p.Arguments)
	case "runs":
		return s.toolRuns(p.Arguments)
	default:
		return nil, &rpcError{codeParams, fmt.Sprintf("unknown tool: %q", p.Name)}
	}
}

// Command promptopt is the CLI entry point of the v2 evaluation
// pipeline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/web"
)

// Exit codes per spec: 0 success, 1 evaluation failure or usage error,
// 2 budget exhausted (takes precedence over 1), 3 verify regression or
// constraint violation (lowest precedence: 2 > 1 > 3).
const (
	exitOK              = 0
	exitFailure         = 1
	exitBudgetExhausted = 2
	exitRegression      = 3
)

// Overridable at build time via
// -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = ""
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// dispatch routes a subcommand and maps it to a process exit code.
func dispatch(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitFailure
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return runCommand(rest)
	case "serve":
		return serveCommand(rest)
	case "verify":
		return verifyCommand(rest)
	case "rollback":
		return rollbackCommand(rest)
	case "anchor":
		return anchorCommand(rest)
	case "config":
		return configCommand(rest)
	case "mcp":
		return mcpCommand(rest)
	case "replay":
		return replayCommand(rest)
	case "version":
		fmt.Printf("promptopt %s (commit %s)\n", version, vcsCommit())
		return exitOK
	case "help":
		usage(os.Stdout)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "promptopt: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		return exitFailure
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage: promptopt <command> [flags]

commands:
  run      evaluate a candidate against a task and dataset
  serve    browse past runs and replay their events
  verify   验证交付候选：锚点/锚点库/合成保留集回归门禁（退出码 3=回归或约束违反）
  rollback 回退采纳：回滚到上一不同采纳或 baseline，可 --emit 导出 candidate.yaml
  replay   回放 run 的完整调用与决策审计时间线（--headless 输出 JSONL）
  anchor   锚点库：add/list/promote 沉淀真实样本（anchors/<task-key>/，ADR 0001 治理）
  config   配置文件层：init/list/get/set/unset（发现序 --config > PROMPTOPT_CONFIG > ./promptopt.yaml > 用户级；缺参报错指路）
  mcp      MCP stdio JSON-RPC server：optimize/verify/runs 三工具（单机单会话、无鉴权；stdout 只走 JSON-RPC）
  version  print version and commit
  help     show this help`)
}

// vcsCommit returns the -ldflags commit when set, otherwise the VCS
// revision embedded by the Go toolchain.
func vcsCommit() string {
	if commit != "" {
		return commit
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}
	return "unknown"
}

// resolveListenAddr combines a --addr/--port pair into the listen
// address (P10 分立参数, 同一规则服务 run 与 serve): a full host:port
// address wins as-is, a host-only --addr combines with --port
// (default config.DefaultPort), the port flag alone binds loopback,
// and neither flag leaves the historical default byte-identical.
// Callers reject the conflicting full-address+--port combination
// before calling (run: parseRunFlags; serve: below).
func resolveListenAddr(addr string, addrSet, portSet bool, port int) string {
	switch {
	case portSet && !addrSet:
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	case addrSet && portSet:
		return net.JoinHostPort(addr, strconv.Itoa(port))
	case addrSet:
		if _, _, err := net.SplitHostPort(addr); err == nil {
			return addr
		}
		return net.JoinHostPort(addr, strconv.Itoa(config.DefaultPort))
	default:
		return config.DefaultAddr
	}
}

// serveCommand browses past runs read-only: run summary cards, run
// detail pages with sample traces, and events.jsonl replay. It exposes
// no live endpoint — that belongs to `run --web`.
func serveCommand(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runsDir := fs.String("runs-dir", config.DefaultOutDir, "directory containing run artifacts")
	addr := fs.String("addr", config.DefaultAddr, "listen address (host or host:port; combine host-only with --port)")
	port := fs.Int("port", 0, "listen port when --addr is host-only (default 17700; a full host:port --addr rejects --port)")
	if err := fs.Parse(args); err != nil {
		return parseExitCode(err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "promptopt serve: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}
	addrSet, portSet := false, false
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			addrSet = true
		case "port":
			portSet = true
		}
	})
	if addrSet && portSet {
		if _, _, err := net.SplitHostPort(*addr); err == nil {
			fmt.Fprintf(os.Stderr, "promptopt serve: --addr %q 已含端口，不能与 --port 同给（纯主机地址才与 --port 组合）\n", *addr)
			return exitFailure
		}
	}
	listen := resolveListenAddr(*addr, addrSet, portSet, *port)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "promptopt serve: http://%s (runs: %s)\n", listen, *runsDir)
	// serve stays read-only: an empty synth dir keeps the synthesis
	// review tree unmounted.
	if err := web.NewServer(*runsDir, "", nil).ListenAndServe(ctx, listen); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt serve: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// parseExitCode maps a flag parse error: -h/--help is help, not failure.
func parseExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	return exitFailure
}

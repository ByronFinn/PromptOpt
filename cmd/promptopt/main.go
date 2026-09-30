// Command promptopt is the CLI entry point of the v2 evaluation
// pipeline.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
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
  verify   验证交付候选：锚点/合成保留集回归门禁（退出码 3=回归或约束违反）
  rollback 回退采纳：回滚到上一不同采纳或 baseline，可 --emit 导出 candidate.yaml
  replay   回放 run 的完整调用与决策审计时间线（--headless 输出 JSONL）
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

// serveCommand browses past runs read-only: run summary cards, run
// detail pages with sample traces, and events.jsonl replay. It exposes
// no live endpoint — that belongs to `run --web`.
func serveCommand(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runsDir := fs.String("runs-dir", config.DefaultOutDir, "directory containing run artifacts")
	addr := fs.String("addr", config.DefaultAddr, "listen address")
	if err := fs.Parse(args); err != nil {
		return parseExitCode(err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "promptopt serve: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "promptopt serve: http://%s (runs: %s)\n", *addr, *runsDir)
	// serve stays read-only: an empty synth dir keeps the synthesis
	// review tree unmounted.
	if err := web.NewServer(*runsDir, "", nil).ListenAndServe(ctx, *addr); err != nil {
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

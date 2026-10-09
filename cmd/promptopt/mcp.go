package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/mcp"
)

// mcpCommand serves the single-machine MCP API surface over stdio
// (提案 §2.4 最小版): three tools — optimize / verify / runs — over
// newline-delimited JSON-RPC 2.0. Output discipline: stdout carries
// nothing but JSON-RPC frames (the transport owns it); every
// human-readable line goes to stderr, and the run/verify sub-paths
// write their headless JSON into io.Discard (the tool payload is read
// back from the on-disk summary.json / verify.json instead).
// Boundary: one stdio session, no authentication, no multi-user
// (PRD-0000 Out of Scope 不动摇); the process ends when stdin closes
// or a signal terminates it.
func mcpCommand(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	runsDir := fs.String("runs-dir", config.DefaultOutDir, "default runs directory for the three tools (optimize 的 --out 与 verify/runs 的缺省 runs 根)")
	if err := fs.Parse(args); err != nil {
		return parseExitCode(err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "promptopt mcp: unexpected argument %q\n", fs.Arg(0))
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "promptopt mcp: stdio JSON-RPC 已就绪（runs: %s；单机单会话、无鉴权；stdin EOF 或 Ctrl-C 退出）\n", *runsDir)
	srv := &mcp.Server{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
		Name: "promptopt", Version: version,
		Handlers: newMCPHandlers(*runsDir, execRun, execVerify),
	}
	if err := srv.Serve(); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt mcp: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// newMCPHandlers wires the three tools onto the run/verify seams.
// runFn/verifyFn are the same seams the CLI subcommands use
// (execRun/execVerify); they are parameters so tests can drive the
// tool handlers with fakes and panic injectors.
func newMCPHandlers(runsDir string,
	runFn func(runOptions, io.Writer) int,
	verifyFn func(verifyOptions, string, io.Writer) int) mcp.Handlers {
	return mcp.Handlers{
		RunsDir: runsDir,
		Optimize: func(args mcp.OptimizeArgs) (string, int, error) {
			return optimizeTool(runsDir, args, runFn)
		},
		Verify: func(args mcp.VerifyArgs) (string, int, error) {
			return verifyTool(runsDir, args, verifyFn)
		},
	}
}

// optimizeTool runs one synchronous headless run and returns the run
// dir whose summary.json is the payload's source of truth.
//
// 信号上下文契约「每次调用一个 sink、调用必关闭」: the run path closes
// its own sink on every normal and early-return path (finishRun 之内
// 含早退分支）；a panicked run unwinds without reaching those closes,
// so the deferred closeOrphanedSinks here reclaims the orphan (its
// stop() releases the signal.NotifyContext) before the server's
// recover turns the panic into a JSON-RPC error — repeated calls never
// accumulate signal handlers (countLiveSinks==0 after every call).
//
// The run id is pinned before the run so the tool can locate
// <out>/<run_id>/summary.json deterministically; the run-side stdout
// is io.Discard (the JSON-RPC stream owns stdout).
func optimizeTool(runsDir string, args mcp.OptimizeArgs, runFn func(runOptions, io.Writer) int) (string, int, error) {
	defer closeOrphanedSinks()
	o, err := parseRunFlags(buildOptimizeArgs(runsDir, args))
	if err != nil {
		return "", exitFailure, fmt.Errorf("参数无效: %w", err)
	}
	o.runID = newRunID()
	exit := runFn(o, io.Discard)
	runDir := filepath.Join(o.outDir, o.runID)
	if _, statErr := os.Stat(filepath.Join(runDir, "summary.json")); statErr != nil {
		return "", exit, fmt.Errorf("run 未产出 summary.json: %w", statErr)
	}
	return runDir, exit, nil
}

// buildOptimizeArgs maps the tool arguments onto the CLI flag surface
// (parseRunFlags applies the full validation and env fallback chain,
// so MCP and CLI agree on one configuration grammar). The positional
// prompt goes last: flagsFirst reorders either way.
func buildOptimizeArgs(runsDir string, args mcp.OptimizeArgs) []string {
	cli := []string{"--headless", "--out", cmp.Or(args.Out, runsDir)}
	if args.BaseURL != "" {
		cli = append(cli, "--base-url", args.BaseURL)
	}
	if args.Model != "" {
		cli = append(cli, "--model", args.Model)
	}
	if args.APIKey != "" {
		cli = append(cli, "--api-key", args.APIKey)
	}
	if args.BudgetEvals > 0 {
		cli = append(cli, "--budget-evals", fmt.Sprint(args.BudgetEvals))
	}
	if args.BudgetTokens > 0 {
		cli = append(cli, "--budget-tokens", fmt.Sprint(args.BudgetTokens))
	}
	if args.Prompt != "" {
		return append(cli, args.Prompt)
	}
	return append(cli, "--task", args.Task, "--candidate", args.Candidate, "--dataset", args.Dataset)
}

// verifyTool runs the regression gate for one run and returns the
// verify dir whose verify.json is the payload's source of truth. Exit
// codes 0/2/3 are valid verdicts (3 = regression — the gate did its
// job); only 1 is an execution failure. The verify dir is named by a
// second-granularity UTC timestamp; stdio sessions serve calls
// sequentially, so the newest entry under runs/<id>/verify/ is this
// call's.
func verifyTool(runsDir string, args mcp.VerifyArgs, verifyFn func(verifyOptions, string, io.Writer) int) (string, int, error) {
	dir := cmp.Or(args.RunsDir, runsDir)
	o, runID, err := parseVerifyFlags([]string{args.RunID, "--headless", "--runs-dir", dir})
	if err != nil {
		return "", exitFailure, fmt.Errorf("参数无效: %w", err)
	}
	exit := verifyFn(o, runID, io.Discard)
	verifyDir, err := newestVerifyDir(filepath.Join(dir, runID, "verify"))
	if err != nil {
		return "", exit, fmt.Errorf("verify 未产出报告: %w", err)
	}
	return verifyDir, exit, nil
}

// newestVerifyDir picks the most recent verify/<timestamp> directory.
func newestVerifyDir(base string) (string, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", errors.New("no verify directory")
	}
	return filepath.Join(base, slices.Max(dirs)), nil
}

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/provider"
	"github.com/ByronFinn/PromptOpt/internal/web"
)

// runOptions is the resolved run configuration.
type runOptions struct {
	taskPath, candidatePath, datasetPath  string
	split, baseURL, model, apiKey, outDir string
	addr, prompt                          string
	maxTokens, budgetTokens, budgetEvals  int
	workers, samples, probeVariants       int
	web, headless, interactive            bool
}

// runCommand implements the run subcommand. With a positional prompt
// it takes the zero-config mode (synthesize → p¹ filter → checkpoint
// → baseline evaluation); otherwise it loads the task/candidate/
// dataset YAML trio directly. Exit codes: 0 success, 1 synthesis or
// evaluation or usage failure, 2 budget exhausted (undispatched
// samples remain; takes precedence over 1).
func runCommand(args []string) int {
	o, err := parseRunFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	if o.prompt != "" {
		return runSynthesized(o)
	}
	return runManual(o)
}

// runManual is the configured mode: an explicit task/candidate/dataset
// trio is evaluated with the candidate prompt, no synthesis involved.
func runManual(o runOptions) int {
	task, err := core.LoadTask(o.taskPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	cand, err := core.LoadCandidate(o.candidatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	dataset, err := core.LoadDataset(o.datasetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	samples := core.FilterSplit(dataset.Samples, o.split)
	if len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "promptopt run: no samples match split %q\n", o.split)
		return exitFailure
	}

	runID := newRunID()
	runDir := filepath.Join(o.outDir, runID)
	sink, err := startSink(o, runDir, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

	// The manifest snapshots the run configuration for reproduction.
	if err := writeJSONFile(filepath.Join(runDir, "manifest.json"), runManifest{
		RunID: runID, CreatedAt: time.Now().UTC(), Version: version,
		Task: task.Name, Candidate: cand.ID, Dataset: dataset.Name, Split: o.split,
		TaskPath: o.taskPath, CandidatePath: o.candidatePath, DatasetPath: o.datasetPath,
		Model: o.model, BaseURL: o.baseURL, MaxTokens: o.maxTokens, Workers: o.workers,
		BudgetTokens: int64(o.budgetTokens), BudgetEvals: int64(o.budgetEvals),
		Samples: len(samples),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}

	engine := &eval.Engine{
		RunID: runID, RunDir: runDir, Model: o.model, MaxTokens: o.maxTokens,
		Workers: o.workers, Metrics: task.Metrics, Split: o.split,
		Budget:   eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Provider: provider.NewOpenAI(o.baseURL, o.apiKey, provider.OpenAIConfig{}),
		TaskName: task.Name, CandidateID: cand.ID, DatasetName: dataset.Name,
		OnEvent: sink.fanout.emit,
	}

	res, err := engine.Run(sink.ctx, cand, samples)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	return finishRun(o, sink, res, runDir, task.Primary())
}

// runSynthesized is the zero-config mode: the positional natural-
// language prompt drives the Harness Builder pipeline. Artifacts land
// twice — synthesis under synth/<run_id>/, the baseline evaluation
// under runs/<run_id>/ — sharing one event stream and budget.
func runSynthesized(o runOptions) int {
	runID := newRunID()
	runDir := filepath.Join(o.outDir, runID)
	synthBase := synthRoot(o.outDir)
	sink, err := startSink(o, runDir, synthBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

	mode := harness.ModeAutopilot
	if o.interactive {
		mode = harness.ModeInteractive
	}
	if err := writeJSONFile(filepath.Join(runDir, "manifest.json"), runManifest{
		RunID: runID, CreatedAt: time.Now().UTC(), Version: version,
		Model: o.model, BaseURL: o.baseURL, MaxTokens: o.maxTokens, Workers: o.workers,
		BudgetTokens: int64(o.budgetTokens), BudgetEvals: int64(o.budgetEvals),
		Mode: string(mode), Prompt: o.prompt,
		SynthSamples: o.samples, ProbeVariants: o.probeVariants,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}

	pipeline := &harness.Pipeline{
		RunID: runID, RunsDir: o.outDir,
		SynthDir: filepath.Join(synthBase, runID),
		Prompt:   o.prompt,
		Provider: provider.NewOpenAI(o.baseURL, o.apiKey, provider.OpenAIConfig{}),
		Model:    o.model, MaxTokens: o.maxTokens, SynthMaxTokens: o.maxTokens,
		SamplesN: o.samples, ProbeVariants: o.probeVariants, Workers: o.workers,
		Budget:  eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Mode:    mode,
		OnEvent: sink.fanout.emit,
	}
	res, err := pipeline.Run(sink.ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	primary := ""
	if spec, err := harness.LoadSpec(filepath.Join(synthBase, runID)); err == nil {
		primary = spec.Task.Primary()
	}
	return finishRun(o, sink, res, runDir, primary)
}

// startSink opens the run dir, the events.jsonl fanout and the
// optional dashboard. synthDir is the synthesis review root; an empty
// value keeps the review tree unmounted.
func startSink(o runOptions, runDir, synthDir string) (*runSink, error) {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	sink := &runSink{ctx: ctx, stop: stop}
	events, err := os.Create(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		stop()
		return nil, err
	}
	sink.events = events
	sink.fanout = newEventFanout(events)
	if !o.headless {
		sink.fanout.subscribe(progressPrinter(os.Stderr))
	}
	if o.web {
		sink.bus = web.NewBus()
		sink.unhookBus = sink.fanout.subscribe(sink.bus.Publish)
		dashboard, err := web.NewServer(o.outDir, synthDir, sink.bus).Listen(o.addr)
		if err != nil {
			sink.close()
			return nil, err
		}
		sink.dashboard = dashboard
		fmt.Fprintf(os.Stderr, "promptopt run: dashboard at http://%s\n", o.addr)
	}
	return sink, nil
}

// runSink bundles the shared per-run plumbing: signal context, the
// events.jsonl fanout and the optional dashboard.
type runSink struct {
	ctx       context.Context
	stop      context.CancelFunc
	events    *os.File
	fanout    *eventFanout
	unhookBus func()
	bus       *web.Bus
	dashboard *http.Server
}

// close tears the sink down: closing the bus ends attached SSE
// streams once run_done has been flushed; Shutdown then waits for the
// handlers.
func (s *runSink) close() {
	if s.dashboard != nil {
		s.unhookBus()
		s.bus.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.dashboard.Shutdown(shutdownCtx)
		cancel()
	}
	s.events.Close()
	s.stop()
}

// finishRun persists the run summary, tears down the sink and prints
// the result per the output convention (headless: JSON on stdout).
func finishRun(o runOptions, sink *runSink, res core.RunResult, runDir, primary string) int {
	// summary.json is the primary artifact (spec); run.json is written
	// alongside with identical content as the engine-slice contract
	// name for the run summary.
	for _, name := range []string{"summary.json", "run.json"} {
		if err := writeJSONFile(filepath.Join(runDir, name), res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
			sink.close()
			return exitFailure
		}
	}
	sink.close()
	if o.headless {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: encode summary: %v\n", err)
			return exitFailure
		}
		return res.ExitCode
	}
	printHumanSummary(os.Stderr, res, runDir, primary)
	return res.ExitCode
}

// parseRunFlags parses and validates the run flag surface. Environment
// fallbacks (PROMPTOPT_BASE_URL/MODEL/API_KEY/OUT) apply to unset
// flags; workers and addr are flag-only by design. A single positional
// argument switches to the zero-config mode, and flags may appear
// before or after it.
func parseRunFlags(args []string) (runOptions, error) {
	var o runOptions
	addrSet := false
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.taskPath, "task", "", "task YAML path (required without a positional prompt)")
	fs.StringVar(&o.candidatePath, "candidate", "", "candidate YAML path (required without a positional prompt)")
	fs.StringVar(&o.datasetPath, "dataset", "", "dataset YAML path (required without a positional prompt)")
	fs.StringVar(&o.split, "split", "", "dataset split to evaluate (default: all)")
	fs.StringVar(&o.baseURL, "base-url", "", "OpenAI-compatible base URL (env PROMPTOPT_BASE_URL)")
	fs.StringVar(&o.model, "model", "", "model name (env PROMPTOPT_MODEL)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (env PROMPTOPT_API_KEY, default \"1\")")
	fs.IntVar(&o.maxTokens, "max-tokens", config.DefaultMaxTokens, "max completion tokens per request")
	fs.IntVar(&o.budgetTokens, "budget-tokens", 0, "executor token budget, prompt+completion (0 = unlimited)")
	fs.IntVar(&o.budgetEvals, "budget-evals", 0, "max executor evaluations (0 = unlimited)")
	fs.IntVar(&o.workers, "workers", config.DefaultWorkers, "parallel evaluation workers")
	fs.IntVar(&o.samples, "samples", config.DefaultSamples, "synthesized sample count (zero-config mode)")
	fs.IntVar(&o.probeVariants, "probe-variants", config.DefaultProbeVariants, "probe prompt variants for variance filtering (zero-config mode)")
	fs.BoolVar(&o.interactive, "interactive", false, "pause at the synthesis checkpoint for manual review (zero-config mode)")
	fs.StringVar(&o.outDir, "out", "", "run output directory (env PROMPTOPT_OUT, default runs/)")
	fs.StringVar(&o.addr, "addr", config.DefaultAddr, "dashboard listen address (requires --web)")
	fs.BoolVar(&o.web, "web", false, "serve a live SSE dashboard during the run")
	fs.BoolVar(&o.headless, "headless", false, "print only the JSON run summary")
	// The stdlib flag package stops parsing at the first positional,
	// but the natural zero-config invocation puts flags after the
	// prompt (`promptopt run "<prompt>" --samples 3`): reorder so both
	// orders parse identically.
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return o, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrSet = true
		}
	})
	switch fs.NArg() {
	case 0:
	case 1:
		o.prompt = fs.Arg(0)
	default:
		return o, fmt.Errorf("unexpected argument %q (a single positional prompt enables the zero-config mode)", fs.Arg(1))
	}

	var errs []error
	if o.prompt != "" {
		// Zero-config mode: the positional prompt replaces the YAML
		// trio and the split selector.
		for _, c := range []struct{ flag, val string }{
			{"--task", o.taskPath}, {"--candidate", o.candidatePath},
			{"--dataset", o.datasetPath}, {"--split", o.split},
		} {
			if c.val != "" {
				errs = append(errs, fmt.Errorf("the positional prompt is mutually exclusive with %s", c.flag))
			}
		}
	} else {
		for _, req := range []struct{ flag, val string }{
			{"--task", o.taskPath}, {"--candidate", o.candidatePath}, {"--dataset", o.datasetPath},
		} {
			if req.val == "" {
				errs = append(errs, fmt.Errorf("%s is required", req.flag))
			}
		}
	}
	if o.web && o.headless {
		errs = append(errs, errors.New("--web and --headless are mutually exclusive"))
	}
	if o.interactive && o.headless {
		errs = append(errs, errors.New("--interactive and --headless are mutually exclusive"))
	}
	if addrSet && !o.web {
		errs = append(errs, errors.New("--addr only takes effect together with --web"))
	}
	if o.maxTokens <= 0 {
		errs = append(errs, fmt.Errorf("--max-tokens must be positive, got %d", o.maxTokens))
	}
	if o.budgetTokens < 0 || o.budgetEvals < 0 {
		errs = append(errs, errors.New("budgets must be zero (unlimited) or positive"))
	}
	if o.workers <= 0 {
		errs = append(errs, fmt.Errorf("--workers must be positive, got %d", o.workers))
	}
	if o.samples <= 0 {
		errs = append(errs, fmt.Errorf("--samples must be positive, got %d", o.samples))
	}
	if o.probeVariants <= 0 {
		errs = append(errs, fmt.Errorf("--probe-variants must be positive, got %d", o.probeVariants))
	}
	o.baseURL = config.BaseURL(o.baseURL)
	if o.baseURL == "" {
		errs = append(errs, errors.New("--base-url (or PROMPTOPT_BASE_URL) is required"))
	}
	o.model = config.Model(o.model)
	if o.model == "" {
		errs = append(errs, errors.New("--model (or PROMPTOPT_MODEL) is required"))
	}
	o.apiKey = config.APIKey(o.apiKey)
	o.outDir = config.OutDir(o.outDir)
	return o, errors.Join(errs...)
}

// flagsFirst reorders command-line arguments so all flags precede
// positional ones: `promptopt run "<prompt>" --samples 3` parses like
// the flag-first form. Flags are collected from anywhere in the list,
// positionals keep their relative order at the end. Non-boolean flags
// consume the next argument as their value, `-flag=value` is self-
// contained, and `--` ends flag parsing with everything after it
// positional. Unknown flags are reordered bare — the flag package
// then reports them itself.
func flagsFirst(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			rest = append(rest, args[i+1:]...)
			return append(flags, rest...)
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			rest = append(rest, arg)
		default:
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			if strings.IndexByte(name, '=') >= 0 {
				continue
			}
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f.Value) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return append(flags, rest...)
}

// isBoolFlag reports whether v is a boolean flag, which takes no
// separate value argument. This is the same probe the flag package
// itself uses.
func isBoolFlag(v flag.Value) bool {
	bv, ok := v.(interface{ IsBoolFlag() bool })
	return ok && bv.IsBoolFlag()
}

// runManifest snapshots the run configuration. The zero-config fields
// stay empty in the configured mode.
type runManifest struct {
	RunID         string    `json:"run_id"`
	CreatedAt     time.Time `json:"created_at"`
	Version       string    `json:"version"`
	Task          string    `json:"task,omitempty"`
	Candidate     string    `json:"candidate,omitempty"`
	Dataset       string    `json:"dataset,omitempty"`
	Split         string    `json:"split,omitempty"`
	TaskPath      string    `json:"task_path,omitempty"`
	CandidatePath string    `json:"candidate_path,omitempty"`
	DatasetPath   string    `json:"dataset_path,omitempty"`
	Model         string    `json:"model"`
	BaseURL       string    `json:"base_url"`
	MaxTokens     int       `json:"max_tokens"`
	Workers       int       `json:"workers"`
	BudgetTokens  int64     `json:"budget_tokens"`
	BudgetEvals   int64     `json:"budget_evals"`
	Samples       int       `json:"samples,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	Prompt        string    `json:"prompt,omitempty"`
	SynthSamples  int       `json:"synth_samples,omitempty"`
	ProbeVariants int       `json:"probe_variants,omitempty"`
}

// synthRoot places the synthesis artifact tree next to the runs tree:
// the default --out runs/ yields a sibling synth/ directory.
func synthRoot(runsDir string) string {
	return filepath.Join(runsDir, "..", "synth")
}

// newRunID mints a sortable, collision-unlikely run id.
func newRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102-150405")
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// eventFanout broadcasts engine events to subscribed handlers.
// Handlers run under the fanout lock, which guarantees a globally
// consistent event order; they must be quick and non-blocking.
type eventFanout struct {
	mu   sync.Mutex
	file io.Writer
	subs []*subBox
}

// subBox wraps a handler so subscriptions compare by identity.
type subBox struct{ handle func(eval.Event) }

func newEventFanout(w io.Writer) *eventFanout {
	return &eventFanout{file: w}
}

// subscribe registers a handler and returns its remover.
func (f *eventFanout) subscribe(h func(eval.Event)) func() {
	box := &subBox{handle: h}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, box)
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, s := range f.subs {
			if s == box {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				return
			}
		}
	}
}

func (f *eventFanout) emit(ev eval.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, err := json.Marshal(ev); err == nil {
		fmt.Fprintf(f.file, "%s\n", b)
	}
	for _, s := range f.subs {
		s.handle(ev)
	}
}

// progressPrinter renders events as plain terminal lines.
func progressPrinter(w io.Writer) func(eval.Event) {
	return func(ev eval.Event) {
		switch ev.Type {
		case eval.EventSampleDone:
			if ev.Error != "" {
				fmt.Fprintf(w, "  sample %-16s FAILED: %s\n", ev.SampleID, truncateRunes(ev.Error, 120))
				return
			}
			metrics := make([]string, 0, len(ev.Scores))
			for m := range ev.Scores {
				metrics = append(metrics, m)
			}
			sort.Strings(metrics)
			parts := make([]string, 0, len(metrics))
			for _, m := range metrics {
				parts = append(parts, fmt.Sprintf("%s=%.4f", m, ev.Scores[m]))
			}
			var tok string
			if ev.Usage != nil {
				tok = fmt.Sprintf(" (%d+%d tok)", ev.Usage.PromptTokens, ev.Usage.CompletionTokens)
			}
			fmt.Fprintf(w, "  sample %-16s %s%s %.1fs\n", ev.SampleID,
				strings.Join(parts, " "), tok, float64(ev.LatencyMS)/1000)
		case eval.EventBudgetStop:
			fmt.Fprintln(w, "  budget reached: stopping dispatch, in-flight samples continue")
		case harness.EventSynthDone:
			fmt.Fprintf(w, "  合成完成：%s 条样本、%s 条探针变体%s\n",
				detailCount(ev.Detail, "samples"), detailCount(ev.Detail, "probes"), detailWarnings(ev.Detail))
		case harness.EventFilterDone:
			fmt.Fprintf(w, "  方差过滤：保留 %s 条，剔除 %s 条\n",
				detailCount(ev.Detail, "kept"), detailCount(ev.Detail, "dropped"))
		case harness.EventCheckpoint:
			if status, _ := ev.Detail["status"].(string); status == "pending" {
				fmt.Fprintln(w, "  检查点：合成集等待审核（--interactive；可在 Web 审核页批准或直接编辑 checkpoint.json）…")
			} else {
				fmt.Fprintln(w, "  检查点：已放行，继续 baseline 评估")
			}
		}
	}
}

// detailCount renders an in-process event detail counter.
func detailCount(detail map[string]any, key string) string {
	if n, ok := detail[key].(int); ok {
		return strconv.Itoa(n)
	}
	return "?"
}

func detailWarnings(detail map[string]any) string {
	ws, ok := detail["warnings"].([]string)
	if !ok || len(ws) == 0 {
		return ""
	}
	return fmt.Sprintf("（%d 条规整警告）", len(ws))
}

// printHumanSummary prints the final run block for interactive use.
func printHumanSummary(w io.Writer, res core.RunResult, runDir, primary string) {
	fmt.Fprintf(w, "\nrun %s: %s (exit %d)\n", res.RunID, res.Status, res.ExitCode)
	fmt.Fprintf(w, "samples: %d evaluated, %d failed, %d undispatched\n",
		res.Evaluated, len(res.FailedSamples), res.Undispatched)
	if len(res.FailedSamples) > 0 {
		fmt.Fprintf(w, "failed: %s\n", strings.Join(res.FailedSamples, ", "))
	}
	metrics := make([]string, 0, len(res.MetricMeans))
	for m := range res.MetricMeans {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	parts := make([]string, 0, len(metrics))
	for _, m := range metrics {
		marker := ""
		if m == primary {
			marker = " *"
		}
		parts = append(parts, fmt.Sprintf("%s=%.4f%s", m, res.MetricMeans[m], marker))
	}
	if len(parts) > 0 {
		fmt.Fprintf(w, "metrics:  %s\n", strings.Join(parts, "  "))
	}
	for _, role := range []core.Role{core.RoleExecutor, core.RoleOptimizer} {
		if u, ok := res.UsageByRole[role]; ok {
			fmt.Fprintf(w, "usage[%s]: prompt=%d completion=%d total=%d\n",
				role, u.PromptTokens, u.CompletionTokens, u.Total())
		}
	}
	fmt.Fprintf(w, "artifacts: %s\n", runDir)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

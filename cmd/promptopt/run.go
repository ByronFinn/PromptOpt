package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
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
	maxRounds, minibatch, stagnationLimit int
	epsilon                               float64
	seed                                  int64
	budgetOptTokens                       int64
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

	eng := &eval.Engine{
		RunID: runID, RunDir: runDir, Model: o.model, MaxTokens: o.maxTokens,
		Workers: o.workers, Metrics: task.Metrics, Split: o.split,
		Budget:   eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Provider: provider.NewOpenAI(o.baseURL, o.apiKey, provider.OpenAIConfig{}),
		TaskName: task.Name, CandidateID: cand.ID, DatasetName: dataset.Name,
		OnEvent: sink.fanout.emit,
	}

	res, err := eng.Run(sink.ctx, cand, samples)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	return finishRun(o, sink, res, runDir, task.Primary(), nil)
}

// runSynthesized is the zero-config mode: the positional natural-
// language prompt drives the Harness Builder pipeline, then the GEPA
// engine optimizes the prompt over the retained sample set. Artifacts
// land twice — synthesis under synth/<run_id>/, the baseline
// evaluation and optimization under runs/<run_id>/ — sharing one
// event stream and budget.
//
// run_done ownership: the engine layer never emits run_done, and the
// OnEvent wrapper drops the baseline's inner run_done too, so the SSE
// stream and the events.jsonl replay survive the whole optimization.
// Exactly one terminal run_done is emitted here, after the exit code
// is finalized.
func runSynthesized(o runOptions) int {
	runID := newRunID()
	runDir := filepath.Join(o.outDir, runID)
	synthBase := synthRoot(o.outDir)
	sink, err := startSink(o, runDir, synthBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	// Drop every run_done before the fanout (the baseline evaluation
	// emits one); the single terminal run_done is emitted below.
	emit := func(ev eval.Event) {
		if ev.Type == eval.EventRunDone {
			return
		}
		sink.fanout.emit(ev)
	}

	if o.seed == 0 {
		o.seed = newSeed()
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
		MaxRounds: o.maxRounds, Minibatch: o.minibatch, Epsilon: o.epsilon,
		StagnationLimit: o.stagnationLimit, Seed: o.seed,
		BudgetOptTokens: o.budgetOptTokens,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}

	budget := eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals))
	prov := provider.NewOpenAI(o.baseURL, o.apiKey, provider.OpenAIConfig{})
	pipeline := &harness.Pipeline{
		RunID: runID, RunsDir: o.outDir,
		SynthDir: filepath.Join(synthBase, runID),
		Prompt:   o.prompt,
		Provider: prov,
		Model:    o.model, MaxTokens: o.maxTokens, SynthMaxTokens: o.maxTokens,
		SamplesN: o.samples, ProbeVariants: o.probeVariants, Workers: o.workers,
		Budget:  budget,
		Mode:    mode,
		OnEvent: emit,
	}
	// The collector rides the fanout for the baseline window only:
	// probe events stay internal to the harness filter, engine unit
	// events subscribe after Optimize has finished.
	collector := engine.NewRecordCollector()
	unhook := sink.fanout.subscribe(collector.Collect)
	res, err := pipeline.Run(sink.ctx)
	unhook()
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	primary := ""
	if spec, err := harness.LoadSpec(filepath.Join(synthBase, runID)); err == nil {
		primary = spec.Task.Primary()
	}

	// Baseline budget-exhausted (or aborted): skip the loop, emit the
	// terminal run_done and keep the exit contract (2 / 1).
	if res.Status != core.StatusCompleted {
		fanoutTerminalRunDone(sink.fanout.emit, res)
		return finishRun(o, sink, res, runDir, primary, nil)
	}

	optRes, err := runOptimization(sink.ctx, runID, runDir, filepath.Join(synthBase, runID), o, prov, budget, collector, emit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		res.Status = core.StatusFailed
		res.ExitCode = exitFailure
	} else {
		// Terminal sync: the engine's reason maps onto the run verdict
		// and the usage snapshot is refreshed past its baseline-time
		// copy (engine.go copies at run end).
		switch optRes.Reason {
		case engine.ReasonBudgetStopped:
			res.Status = core.StatusBudgetExhausted
			res.ExitCode = exitBudgetExhausted
		case engine.ReasonAborted:
			res.Status = core.StatusAborted
			res.ExitCode = exitFailure
		}
		_, usage := budget.Snapshot()
		res.UsageByRole = usage
	}
	fanoutTerminalRunDone(sink.fanout.emit, res)
	var best *engine.Result
	if err == nil {
		best = &optRes
	}
	return finishRun(o, sink, res, runDir, primary, best)
}

// runOptimization drives the GEPA loop over the retained sample set:
// it recomputes the kept samples (the checkpoint pause may have edited
// them), harvests the baseline records and wires the engine request.
func runOptimization(ctx context.Context, runID, runDir, synthDir string, o runOptions,
	prov *provider.Client, budget *eval.Budget, collector *engine.RecordCollector, emit func(eval.Event)) (engine.Result, error) {
	spec, err := harness.LoadSpec(synthDir)
	if err != nil {
		return engine.Result{}, fmt.Errorf("reload spec.json: %w", err)
	}
	samples, err := harness.LoadSamples(synthDir)
	if err != nil {
		return engine.Result{}, fmt.Errorf("reload samples.json: %w", err)
	}
	report, err := harness.LoadFilterReport(synthDir)
	if err != nil {
		return engine.Result{}, fmt.Errorf("reload filter.json: %w", err)
	}
	kept := harness.SelectKept(samples.Samples, &report)
	if len(kept) == 0 {
		// The pipeline already fell back to the full synthesized set
		// when the filter kept nothing (report.FallbackAll); stay
		// consistent with that decision here. A kept==0 report without
		// the fallback flag remains a hard error.
		if !report.FallbackAll {
			return engine.Result{}, errors.New("过滤后保留样本集为空，无法进行优化")
		}
		kept = samples.Samples
	}
	req := engine.Request{
		Task: spec.Task,
		Params: engine.Params{
			MaxRounds: o.maxRounds, Minibatch: o.minibatch,
			StagnationLimit: o.stagnationLimit, Epsilon: o.epsilon, Seed: o.seed,
		},
		Initial:  core.Candidate{ID: "baseline", Prompt: spec.Task.PromptTemplate},
		Samples:  kept,
		Baseline: collector.Records(kept),
		Provider: prov,
		Model:    o.model, MaxTokens: o.maxTokens, OptMaxTokens: o.maxTokens,
		Workers:         o.workers,
		Budget:          budget,
		OptBudgetTokens: o.budgetOptTokens,
		RunID:           runID, RunDir: runDir,
		OnEvent: emit,
	}
	gepa := &engine.Gepa{}
	return gepa.Optimize(ctx, req)
}

// fanoutTerminalRunDone emits the single terminal run_done carrying
// the final verdict; the cmd layer is its only owner.
func fanoutTerminalRunDone(emit func(eval.Event), res core.RunResult) {
	emit(eval.Event{
		Type: eval.EventRunDone, Time: time.Now(), RunID: res.RunID,
		Status: string(res.Status), ExitCode: res.ExitCode,
		Undispatched: res.Undispatched, MetricMeans: res.MetricMeans,
		UsageByRole: res.UsageByRole, FailedSamples: res.FailedSamples,
	})
}

// newSeed derives a random positive seed for --seed 0.
func newSeed() int64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().UnixNano()
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1)
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
// opt, when non-nil, adds the optimizer's best-prompt block to the
// human summary.
func finishRun(o runOptions, sink *runSink, res core.RunResult, runDir, primary string, opt *engine.Result) int {
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
	printHumanSummary(os.Stderr, res, runDir, primary, opt)
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
	fs.IntVar(&o.maxRounds, "max-rounds", config.DefaultMaxRounds, "GEPA optimization rounds (zero-config mode)")
	fs.IntVar(&o.minibatch, "minibatch", config.DefaultMinibatch, "samples drawn per reflection round (zero-config mode)")
	fs.Float64Var(&o.epsilon, "epsilon", config.DefaultEpsilon, "exploration rate of hypothesis selection, 0..1 (zero-config mode)")
	fs.IntVar(&o.stagnationLimit, "stagnation-limit", config.DefaultStagnationLimit, "stagnant rounds before a fresh restart (zero-config mode)")
	fs.Int64Var(&o.seed, "seed", 0, "optimization rng seed (0 = derive and record in the manifest)")
	fs.Int64Var(&o.budgetOptTokens, "budget-opt-tokens", 0, "optimizer token budget, cumulative (0 = unlimited)")
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
	if o.maxRounds < 1 {
		errs = append(errs, fmt.Errorf("--max-rounds must be >= 1, got %d", o.maxRounds))
	}
	if o.minibatch < 1 {
		errs = append(errs, fmt.Errorf("--minibatch must be >= 1, got %d", o.minibatch))
	}
	if o.epsilon < 0 || o.epsilon > 1 {
		errs = append(errs, fmt.Errorf("--epsilon must be within [0, 1], got %v", o.epsilon))
	}
	if o.stagnationLimit < 1 {
		errs = append(errs, fmt.Errorf("--stagnation-limit must be >= 1, got %d", o.stagnationLimit))
	}
	if o.seed < 0 {
		errs = append(errs, fmt.Errorf("--seed must be >= 0, got %d", o.seed))
	}
	if o.budgetOptTokens < 0 {
		errs = append(errs, fmt.Errorf("--budget-opt-tokens must be zero (unlimited) or positive, got %d", o.budgetOptTokens))
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
	// GEPA engine settings (zero-config mode only; seed always records
	// the derived actual value when --seed 0).
	MaxRounds       int     `json:"max_rounds,omitempty"`
	Minibatch       int     `json:"minibatch,omitempty"`
	Epsilon         float64 `json:"epsilon,omitempty"`
	StagnationLimit int     `json:"stagnation_limit,omitempty"`
	Seed            int64   `json:"seed,omitempty"`
	BudgetOptTokens int64   `json:"budget_opt_tokens,omitempty"`
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
		case harness.EventFilterFallback:
			fmt.Fprintf(w, "  方差过滤：无区分样本（全部 dead/noisy），回退使用全部 %s 条合成样本（见 filter.json fallback_all）\n",
				detailCount(ev.Detail, "samples"))
		case harness.EventCheckpoint:
			if status, _ := ev.Detail["status"].(string); status == "pending" {
				fmt.Fprintln(w, "  检查点：合成集等待审核（--interactive；可在 Web 审核页批准或直接编辑 checkpoint.json）…")
			} else {
				fmt.Fprintln(w, "  检查点：已放行，继续 baseline 评估")
			}
		case engine.EventRoundStart:
			fmt.Fprintf(w, "  优化第 %s 轮开始…\n", detailCount(ev.Detail, "round"))
		case engine.EventReflectDone:
			fmt.Fprintf(w, "  反思完成：%s 条假设\n", detailCount(ev.Detail, "hypotheses"))
		case engine.EventHypoValidated:
			mode, _ := ev.Detail["mode"].(string)
			sel, _ := ev.Detail["selected_hypothesis"].(string)
			fmt.Fprintf(w, "  假设验证：选中 %s（模式 %s）\n", sel, mode)
		case engine.EventMutateDone:
			operator, _ := ev.Detail["operator"].(string)
			cand, _ := ev.Detail["candidate"].(string)
			fmt.Fprintf(w, "  突变完成：%s → %s\n", operator, cand)
		case engine.EventFrontierUpdated:
			if admitted, _ := ev.Detail["admitted"].(bool); admitted {
				fmt.Fprintf(w, "  前沿更新：%s 准入（淘汰 %d 个）\n", detailAny(ev.Detail["candidate"]), lenDetail(ev.Detail, "evicted"))
			} else {
				fmt.Fprintf(w, "  前沿更新：%s 未准入\n", detailAny(ev.Detail["candidate"]))
			}
		case engine.EventVistaRestart:
			fmt.Fprintf(w, "  VISTA 重启：连续 %s 轮无改进，随机重启\n", detailCount(ev.Detail, "stagnant"))
		case engine.EventRoundDone:
			if skipped, _ := ev.Detail["skipped"].(bool); skipped {
				fmt.Fprintf(w, "  第 %s 轮跳过（优化侧调用失败，记停滞）\n", detailCount(ev.Detail, "round"))
				return
			}
			fmt.Fprintf(w, "  第 %s 轮结束：主指标均值 %s\n", detailCount(ev.Detail, "round"), detailFloat(ev.Detail, "primary_mean"))
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

// detailAny renders an in-process detail value as-is.
func detailAny(v any) string {
	if v == nil {
		return "?"
	}
	return fmt.Sprint(v)
}

// lenDetail renders the length of a string-slice detail value.
func lenDetail(detail map[string]any, key string) int {
	if xs, ok := detail[key].([]string); ok {
		return len(xs)
	}
	return 0
}

// detailFloat renders a float detail value with four decimals.
func detailFloat(detail map[string]any, key string) string {
	if v, ok := detail[key].(float64); ok {
		return strconv.FormatFloat(v, 'f', 4, 64)
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
// opt, when non-nil, adds the optimizer's best prompt block.
func printHumanSummary(w io.Writer, res core.RunResult, runDir, primary string, opt *engine.Result) {
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
	if opt != nil {
		fmt.Fprintf(w, "optimized: %d rounds (%s), best candidate %q\n",
			opt.Rounds, opt.Reason, opt.Best.ID)
		for _, m := range slices.Sorted(maps.Keys(opt.BestMeans)) {
			fmt.Fprintf(w, "  best %s=%.4f\n", m, opt.BestMeans[m])
		}
		fmt.Fprintf(w, "best prompt:\n---\n%s\n---\n", opt.Best.Prompt)
		fmt.Fprintf(w, "report: %s\n", filepath.Join(runDir, "report.md"))
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

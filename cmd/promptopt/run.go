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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/provider"
	"github.com/ByronFinn/PromptOpt/internal/web"
)

// runOptions is the resolved run configuration.
type runOptions struct {
	taskPath, candidatePath, datasetPath  string
	split, baseURL, model, apiKey, outDir string
	addr                                  string
	maxTokens, budgetTokens, budgetEvals  int
	workers                               int
	web, headless                         bool
}

// runCommand implements the run subcommand: load the YAML inputs,
// evaluate the candidate over the dataset with a worker pool, persist
// artifacts under <out>/<run_id>/ and print the summary. Exit codes:
// 0 success, 1 evaluation or usage failure, 2 budget exhausted
// (undispatched samples remain; takes precedence over 1).
func runCommand(args []string) int {
	o, err := parseRunFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

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
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	eventsFile, err := os.Create(filepath.Join(runDir, "events.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	defer eventsFile.Close()

	fanout := newEventFanout(eventsFile)
	if !o.headless {
		fanout.subscribe(progressPrinter(os.Stderr))
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
		return exitFailure
	}

	var (
		dashboard *http.Server
		bus       *web.Bus
		unhookBus func()
	)
	if o.web {
		bus = web.NewBus()
		unhookBus = fanout.subscribe(bus.Publish)
		dashboard, err = web.NewServer(o.outDir, bus).Listen(o.addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(os.Stderr, "promptopt run: dashboard at http://%s\n", o.addr)
	}

	engine := &eval.Engine{
		RunID: runID, RunDir: runDir, Model: o.model, MaxTokens: o.maxTokens,
		Workers: o.workers, Metrics: task.Metrics, Split: o.split,
		Budget:   eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Provider: provider.NewOpenAI(o.baseURL, o.apiKey, provider.OpenAIConfig{}),
		TaskName: task.Name, CandidateID: cand.ID, DatasetName: dataset.Name,
		OnEvent: fanout.emit,
	}

	res, err := engine.Run(ctx, cand, samples)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

	// summary.json is the primary artifact (spec); run.json is written
	// alongside with identical content as the engine-slice contract
	// name for the run summary.
	for _, name := range []string{"summary.json", "run.json"} {
		if err := writeJSONFile(filepath.Join(runDir, name), res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
			return exitFailure
		}
	}

	if dashboard != nil {
		// Closing the bus ends attached SSE streams once run_done has
		// been flushed; Shutdown then waits for the handlers.
		unhookBus()
		bus.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = dashboard.Shutdown(shutdownCtx)
		cancel()
	}

	if o.headless {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: encode summary: %v\n", err)
			return exitFailure
		}
		return res.ExitCode
	}
	printHumanSummary(os.Stderr, res, runDir, task.Primary())
	return res.ExitCode
}

// parseRunFlags parses and validates the run flag surface. Environment
// fallbacks (PROMPTOPT_BASE_URL/MODEL/API_KEY/OUT) apply to unset
// flags; workers and addr are flag-only by design.
func parseRunFlags(args []string) (runOptions, error) {
	var o runOptions
	addrSet := false
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.taskPath, "task", "", "task YAML path (required)")
	fs.StringVar(&o.candidatePath, "candidate", "", "candidate YAML path (required)")
	fs.StringVar(&o.datasetPath, "dataset", "", "dataset YAML path (required)")
	fs.StringVar(&o.split, "split", "", "dataset split to evaluate (default: all)")
	fs.StringVar(&o.baseURL, "base-url", "", "OpenAI-compatible base URL (env PROMPTOPT_BASE_URL)")
	fs.StringVar(&o.model, "model", "", "model name (env PROMPTOPT_MODEL)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (env PROMPTOPT_API_KEY, default \"1\")")
	fs.IntVar(&o.maxTokens, "max-tokens", config.DefaultMaxTokens, "max completion tokens per request")
	fs.IntVar(&o.budgetTokens, "budget-tokens", 0, "executor token budget, prompt+completion (0 = unlimited)")
	fs.IntVar(&o.budgetEvals, "budget-evals", 0, "max executor evaluations (0 = unlimited)")
	fs.IntVar(&o.workers, "workers", config.DefaultWorkers, "parallel evaluation workers")
	fs.StringVar(&o.outDir, "out", "", "run output directory (env PROMPTOPT_OUT, default runs/)")
	fs.StringVar(&o.addr, "addr", config.DefaultAddr, "dashboard listen address (requires --web)")
	fs.BoolVar(&o.web, "web", false, "serve a live SSE dashboard during the run")
	fs.BoolVar(&o.headless, "headless", false, "print only the JSON run summary")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrSet = true
		}
	})
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	var errs []error
	for _, req := range []struct{ flag, val string }{
		{"--task", o.taskPath}, {"--candidate", o.candidatePath}, {"--dataset", o.datasetPath},
	} {
		if req.val == "" {
			errs = append(errs, fmt.Errorf("%s is required", req.flag))
		}
	}
	if o.web && o.headless {
		errs = append(errs, errors.New("--web and --headless are mutually exclusive"))
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

// runManifest snapshots the run configuration.
type runManifest struct {
	RunID         string    `json:"run_id"`
	CreatedAt     time.Time `json:"created_at"`
	Version       string    `json:"version"`
	Task          string    `json:"task"`
	Candidate     string    `json:"candidate"`
	Dataset       string    `json:"dataset"`
	Split         string    `json:"split,omitempty"`
	TaskPath      string    `json:"task_path"`
	CandidatePath string    `json:"candidate_path"`
	DatasetPath   string    `json:"dataset_path"`
	Model         string    `json:"model"`
	BaseURL       string    `json:"base_url"`
	MaxTokens     int       `json:"max_tokens"`
	Workers       int       `json:"workers"`
	BudgetTokens  int64     `json:"budget_tokens"`
	BudgetEvals   int64     `json:"budget_evals"`
	Samples       int       `json:"samples"`
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
		}
	}
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

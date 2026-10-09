package main

import (
	"cmp"
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
	"net"
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

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/optimizers"
	"github.com/ByronFinn/PromptOpt/internal/optimizers/builtin"
	"github.com/ByronFinn/PromptOpt/internal/pool"
	"github.com/ByronFinn/PromptOpt/internal/provider"
	"github.com/ByronFinn/PromptOpt/internal/stats"
	"github.com/ByronFinn/PromptOpt/internal/web"
)

// runOptions is the resolved run configuration.
type runOptions struct {
	taskPath, candidatePath, datasetPath  string
	split, baseURL, model, apiKey, outDir string
	taskKey                               string // anchor-library key; default derivation in taskKeyDefault
	addr, prompt                          string
	// port pairs with a host-only --addr (P10 分立参数): a full
	// host:port --addr wins as-is and rejects --port; the set flags
	// mark explicit --addr/--port (the flags carry defaults), drive the
	// dashboard implication and the listen resolution in listenAddr.
	port             int
	addrSet, portSet bool
	// runID pins a pre-minted run id; empty (the CLI case) mints one at
	// run start. The MCP tools/call optimize surface pins one so the
	// tool can locate <out>/<run_id>/summary.json deterministically.
	runID                                 string
	optimizer, providerName, evoVariant   string
	maxTokens, budgetTokens, budgetEvals  int
	workers, samples, probeVariants       int
	maxRounds, minibatch, stagnationLimit int
	epsilon, temperature                  float64
	reps                                  int
	seed                                  int64
	budgetOptTokens                       int64
	web, headless, interactive            bool

	// Judge surface: an optional second LLM for the llm_judge metric.
	// Zero values keep the judge on the executor's configuration (the
	// engine-level per-field fallback); base URL and API key fall back
	// to the executor's only when building a dedicated provider.
	judgeProvider, judgeBaseURL, judgeModel, judgeAPIKey string
	judgeMaxTokens                                       int

	// Decision-model judge surface (P7 级联): --judge-backend decision
	// routes llm_judge grading through the SystemOne decision service
	// (URL + model bind the decision surface) with a per-sample
	// cascade fallback to the generative judge. Thresholds of 0 mean
	// the eval-package defaults (0.5 confidence, 0.6 diagnosis line).
	judgeBackend            string
	judgeDecisionURL        string
	judgeDecisionModel      string
	judgeDecisionConfidence float64
	judgeDecisionDiagBelow  float64

	// Outbound safety valve and gateway-private extras: rps paces every
	// built provider client (0 = off, the default); extraBody is the
	// parsed --extra-body JSON merged onto the wire payload top level
	// (gateway-private fields such as chat_template_kwargs to disable
	// thinking). timeout is the explicit per-attempt provider deadline
	// (0 = unset; the constructors' 180s default applies).
	rps       float64
	extraBody map[string]any
	timeout   time.Duration

	// 配置文件层（PRD-0001 切分 1）：configPath 是 --config 的原始值
	//（发现序第一层）；sources 是 22 个文件键的逐键来源表——单一收集
	// 器（R2 #5）：10 键显式性判定派生自 Source==flag，切分 4 的快照
	// 来源标注复用同一张表，parseRunFlags 不另立 per-key bool 并存。
	configPath string
	sources    config.KeySources

	// 配置文件层命中现场（parseRunFlags 的 Discover 结果）：设置页快照
	// 的来源可见性（R1 #10——Web 设置页与 config list 同样必须展示命中
	// 路径与被遮蔽文件）。无文件命中时两者皆零值。
	configHitPath  string
	configShadowed []string

	// specMetrics 钉死零配置合成规格的指标（PRD-0001 D7 切分 3，
	// --spec-metrics / 文件键 spec_metrics）：仅零配置模式合法，覆盖点
	// 在 harness.Pipeline（SynthesizeSpec 之后、样本合成之前）。空 =
	// 现状 LLM 自选；不新增 --spec-primary（primary 回落列表首项）。
	specMetrics []string
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
	return execRun(o, os.Stdout)
}

// execRun dispatches the two run modes after flag parsing. stdout is
// the headless summary channel, injected so callers own it: the CLI
// passes os.Stdout (byte-identical to the pre-injection behavior) and
// the MCP tools/call optimize surface passes io.Discard — there the
// JSON-RPC stream owns stdout and the payload is read back from the
// on-disk summary.json.
func execRun(o runOptions, stdout io.Writer) int {
	if o.prompt != "" {
		return runSynthesized(o, stdout)
	}
	return runManual(o, stdout)
}

// runManual is the configured mode: an explicit task/candidate/dataset
// trio is evaluated with the candidate prompt, no synthesis involved.
func runManual(o runOptions, stdout io.Writer) int {
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

	prov, err := newProvider(o.providerName, o.baseURL, o.apiKey, o.rps, o.timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	judgeProv, err := newJudgeProvider(judgeConn{
		providerName: o.providerName, baseURL: o.baseURL, apiKey: o.apiKey, rps: o.rps, timeout: o.timeout,
		judgeProvider: o.judgeProvider, judgeBaseURL: o.judgeBaseURL, judgeAPIKey: o.judgeAPIKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	// Judge fallback notice: stderr-only so the headless stdout JSON
	// summary stays unpolluted.
	warnJudgeFallback(os.Stderr, task.Metrics, o)

	// runID normally mints here; a caller that pinned one (MCP
	// tools/call optimize, so the tool can locate runDir) wins.
	runID := cmp.Or(o.runID, newRunID())
	runDir := filepath.Join(o.outDir, runID)
	sink, err := startSink(o, runDir, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}

	// The manifest snapshots the run configuration for reproduction.
	// task_key defaults to the task file's stem — verify --promote and
	// --anchor-lib reproduce the library key from this snapshot.
	o.taskKey = cmp.Or(o.taskKey, taskKeyDefault(o.prompt, o.taskPath))
	if err := writeJSONFile(filepath.Join(runDir, "manifest.json"), runManifest{
		RunID: runID, CreatedAt: time.Now().UTC(), Version: version,
		Task: task.Name, Candidate: cand.ID, Dataset: dataset.Name, Split: o.split,
		TaskPath: o.taskPath, CandidatePath: o.candidatePath, DatasetPath: o.datasetPath,
		TaskKey: o.taskKey,
		Model:   o.model, BaseURL: o.baseURL, MaxTokens: o.maxTokens, Workers: o.workers,
		BudgetTokens: int64(o.budgetTokens), BudgetEvals: int64(o.budgetEvals),
		Temperature: o.temperature, Reps: o.reps,
		Samples: len(samples), Provider: o.providerName,
		RPS: o.rps, ExtraBody: o.extraBody,
		TimeoutSeconds: int(o.timeout.Seconds()),
		JudgeProvider:  o.judgeProvider, JudgeBaseURL: o.judgeBaseURL,
		JudgeModel: o.judgeModel, JudgeMaxTokens: o.judgeMaxTokens,
		JudgeBackend: o.judgeBackend, JudgeDecisionURL: o.judgeDecisionURL,
		JudgeDecisionModel:      o.judgeDecisionModel,
		JudgeDecisionConfidence: o.judgeDecisionConfidence,
		JudgeDecisionDiagBelow:  o.judgeDecisionDiagBelow,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	// The split-filtered set makes the run self-contained: trace views
	// join sample ids against this dataset.json for input/expected.
	if err := writeJSONFile(filepath.Join(runDir, "dataset.json"), core.Dataset{
		Name: dataset.Name, Samples: samples,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}

	// The pool's fixed-order primary row comes from the same event
	// projection the verify gate uses (verifyCollector).
	poolRows := &verifyCollector{primary: task.Primary()}
	eng := &eval.Engine{
		RunID: runID, RunDir: runDir, Model: o.model, MaxTokens: o.maxTokens,
		Workers: o.workers, Metrics: task.Metrics, Split: o.split,
		Budget:   eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Provider: prov,
		// 手动模式与零配置统一解析 --temperature/--reps：flag 只在
		// 零配置生效而手动静默丢弃的话，manifest 快照会与执行不一致
		// （工件撒谎），且手动三件套正是 DSPy 预算对照跑 reps 的场景。
		Temperature: o.temperature, Reps: o.reps,
		JudgeProvider:           judgeProv,
		JudgeModel:              o.judgeModel,
		JudgeMaxTokens:          o.judgeMaxTokens,
		JudgeBackend:            o.judgeBackend,
		DecisionClient:          decisionClient(o),
		JudgeDecisionConfidence: o.judgeDecisionConfidence,
		JudgeDecisionDiagBelow:  o.judgeDecisionDiagBelow,
		ExtraBody:               o.extraBody,
		TaskName:                task.Name, CandidateID: cand.ID, DatasetName: dataset.Name,
		OnEvent: func(ev eval.Event) {
			// The pool's primary-row projection rides the same event
			// stream (verifyCollector, the verify gate's seam): scores
			// are harvested per sample id and projected onto the fixed
			// sample order after Run.
			poolRows.observe(ev)
			sink.fanout.emit(ev)
		},
	}

	res, err := eng.Run(sink.ctx, cand, samples)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}
	// Terminal calibration snapshot: it covers failed samples too (their
	// sample_done carries no usage payload), which the incremental gauge
	// on the dashboard would otherwise miss.
	sink.fanout.emit(engine.NewUsageEvent(eng.Budget, runID, "eval"))
	// Cross-run candidate pool (提案 §3.3 最小版): the manual trio's fixed
	// dataset is the one reachable same-set scenario — a completed run
	// compares against the pool's historical best and appends its own
	// record (rows projected from this evaluation's SampleTrace scores).
	if res.ExitCode == exitOK {
		poolReflect(o, runID, runDir, task.Primary(), samples, poolRows.rows(samples), res.MetricMeans, cand)
	}
	return finishRun(o, sink, res, runDir, task.Primary(), nil, stdout)
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
func runSynthesized(o runOptions, stdout io.Writer) int {
	prov, err := newProvider(o.providerName, o.baseURL, o.apiKey, o.rps, o.timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	judgeProv, err := newJudgeProvider(judgeConn{
		providerName: o.providerName, baseURL: o.baseURL, apiKey: o.apiKey, rps: o.rps, timeout: o.timeout,
		judgeProvider: o.judgeProvider, judgeBaseURL: o.judgeBaseURL, judgeAPIKey: o.judgeAPIKey,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		return exitFailure
	}
	runID := cmp.Or(o.runID, newRunID())
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
	// task_key defaults to the prompt hash prefix: the synthesized task
	// name is LLM-generated per run (internal/harness/synthesize.go) and
	// unstable across runs — it cannot key the anchor library.
	o.taskKey = cmp.Or(o.taskKey, taskKeyDefault(o.prompt, o.taskPath))
	mode := harness.ModeAutopilot
	if o.interactive {
		mode = harness.ModeInteractive
	}
	if err := writeJSONFile(filepath.Join(runDir, "manifest.json"), runManifest{
		RunID: runID, CreatedAt: time.Now().UTC(), Version: version,
		TaskKey: o.taskKey,
		Model:   o.model, BaseURL: o.baseURL, MaxTokens: o.maxTokens, Workers: o.workers,
		BudgetTokens: int64(o.budgetTokens), BudgetEvals: int64(o.budgetEvals),
		Temperature: o.temperature, Reps: o.reps,
		Mode: string(mode), Prompt: o.prompt,
		SynthSamples: o.samples, ProbeVariants: o.probeVariants,
		MaxRounds: o.maxRounds, Minibatch: o.minibatch, Epsilon: o.epsilon,
		StagnationLimit: o.stagnationLimit, Seed: o.seed,
		BudgetOptTokens: o.budgetOptTokens,
		Optimizer:       o.optimizer, Provider: o.providerName, EvoVariant: o.evoVariant,
		SpecMetrics: o.specMetrics,
		RPS:         o.rps, ExtraBody: o.extraBody,
		TimeoutSeconds: int(o.timeout.Seconds()),
		JudgeProvider:  o.judgeProvider, JudgeBaseURL: o.judgeBaseURL,
		JudgeModel: o.judgeModel, JudgeMaxTokens: o.judgeMaxTokens,
		JudgeBackend: o.judgeBackend, JudgeDecisionURL: o.judgeDecisionURL,
		JudgeDecisionModel:      o.judgeDecisionModel,
		JudgeDecisionConfidence: o.judgeDecisionConfidence,
		JudgeDecisionDiagBelow:  o.judgeDecisionDiagBelow,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", err)
		sink.close()
		return exitFailure
	}

	budget := eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals))
	pipeline := &harness.Pipeline{
		RunID: runID, RunsDir: o.outDir,
		SynthDir: filepath.Join(synthBase, runID),
		Prompt:   o.prompt,
		Provider: prov,
		Model:    o.model, MaxTokens: o.maxTokens, SynthMaxTokens: o.maxTokens,
		Temperature: o.temperature, Reps: o.reps,
		JudgeProvider:           judgeProv,
		JudgeModel:              o.judgeModel,
		JudgeMaxTokens:          o.judgeMaxTokens,
		JudgeBackend:            o.judgeBackend,
		DecisionClient:          decisionClient(o),
		JudgeDecisionConfidence: o.judgeDecisionConfidence,
		JudgeDecisionDiagBelow:  o.judgeDecisionDiagBelow,
		ExtraBody:               o.extraBody,
		SpecMetrics:             o.specMetrics,
		SamplesN:                o.samples, ProbeVariants: o.probeVariants, Workers: o.workers,
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
	// Calibration snapshot right after the pipeline: probe evaluations
	// are metered into the shared budget as executor-role usage but
	// their events stay inside the filter, so this is the first point
	// where the live gauge can see that consumption.
	emit(engine.NewUsageEvent(budget, runID, "pipeline"))
	primary := ""
	if spec, err := harness.LoadSpec(filepath.Join(synthBase, runID)); err == nil {
		primary = spec.Task.Primary()
		// The synthesized spec is the first place the zero-config mode
		// knows whether llm_judge is declared; warn here (stderr-only).
		warnJudgeFallback(os.Stderr, spec.Task.Metrics, o)
	}

	// Resolve the optimizer paradigm now that the retained set is
	// known and record the route in the manifest. This runs even when
	// the baseline exhausted the budget and the loop will be skipped:
	// the manifest then still documents which paradigm the run was
	// bound to (routing is a configuration fact, not a completion
	// fact).
	opt, routeErr := resolveOptimizer(runDir, filepath.Join(synthBase, runID), o)
	if routeErr != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", routeErr)
	}

	// Baseline budget-exhausted (or aborted): skip the loop, emit the
	// terminal run_done and keep the exit contract (2 / 1).
	if res.Status != core.StatusCompleted {
		fanoutTerminalRunDone(sink.fanout.emit, res)
		return finishRun(o, sink, res, runDir, primary, nil, stdout)
	}

	var optRes engine.Result
	var optErr error
	if opt == nil {
		optErr = routeErr
	} else {
		optRes, optErr = runOptimization(sink.ctx, runID, runDir, filepath.Join(synthBase, runID), o, prov, judgeProv, budget, collector, emit, opt)
	}
	if optErr != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: %v\n", optErr)
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
	// Terminal snapshot before run_done: the optimizer's last dial and
	// any failed-sample usage land in the gauge even though no further
	// sample_done events will carry them.
	emit(engine.NewUsageEvent(budget, runID, "terminal"))
	fanoutTerminalRunDone(sink.fanout.emit, res)
	var best *engine.Result
	if optErr == nil {
		best = &optRes
	}
	return finishRun(o, sink, res, runDir, primary, best, stdout)
}

// resolveOptimizer picks the run's paradigm and rewrites the
// manifest's routing fields. The retained set is only known after the
// synthesis pipeline, so the initial manifest snapshot carries the
// requested --optimizer value and this rewrite lands once routing is
// resolved. It runs even when the baseline exhausted the budget — the
// manifest then documents which paradigm the run was bound to.
func resolveOptimizer(runDir, synthDir string, o runOptions) (engine.Optimizer, error) {
	spec, err := harness.LoadSpec(synthDir)
	if err != nil {
		return nil, fmt.Errorf("reload spec.json: %w", err)
	}
	kept, err := retainedSamples(synthDir)
	if err != nil {
		return nil, err
	}
	reg := builtin.Registry()
	decision := optimizers.Decision{
		Paradigm: o.optimizer, Requested: o.optimizer,
		Reason: fmt.Sprintf("显式指定 %s（未走 auto 路由）", o.optimizer),
	}
	if o.optimizer == "auto" {
		decision = optimizers.Route(reg, routeFeatures(spec.Task, kept, int64(o.budgetEvals)))
	}
	if err := rewriteManifestRoute(runDir, decision, o); err != nil {
		return nil, err
	}
	return reg.Build(decision.Paradigm)
}

// rewriteManifestRoute patches manifest.json's optimizer routing
// fields (and the evo variant snapshot) once routing is resolved.
func rewriteManifestRoute(runDir string, decision optimizers.Decision, o runOptions) error {
	path := filepath.Join(runDir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read manifest.json: %w", err)
	}
	var mf runManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return fmt.Errorf("decode manifest.json: %w", err)
	}
	mf.Optimizer = decision.Paradigm
	mf.OptimizerRequested = decision.Requested
	mf.OptimizerRouteReason = decision.Reason
	mf.EvoVariant = o.evoVariant
	return writeJSONFile(path, mf)
}

// retainedSamples reloads the synthesis artifacts and applies the p¹
// filter's kept verdicts — the checkpoint pause may have edited them.
func retainedSamples(synthDir string) ([]core.Sample, error) {
	samples, err := harness.LoadSamples(synthDir)
	if err != nil {
		return nil, fmt.Errorf("reload samples.json: %w", err)
	}
	report, err := harness.LoadFilterReport(synthDir)
	if err != nil {
		return nil, fmt.Errorf("reload filter.json: %w", err)
	}
	kept := harness.SelectKept(samples.Samples, &report)
	if len(kept) == 0 {
		// The pipeline already fell back to the full synthesized set
		// when the filter kept nothing (report.FallbackAll); stay
		// consistent with that decision here. A kept==0 report without
		// the fallback flag remains a hard error.
		if !report.FallbackAll {
			return nil, errors.New("过滤后保留样本集为空，无法进行优化")
		}
		kept = samples.Samples
	}
	return kept, nil
}

// routeFeatures derives the auto-routing input from the task, the
// retained set and the eval budget flag. The derivation is pinned
// here (unit-testable, no hidden state); task-level structured
// declarations remain the upgrade path (docs/plugins.md):
//
//   - JointFewShot needs ≥2 train samples plus a verifiable primary
//     metric — joint instruction+demo search is pointless without
//     demo material or an automatic verdict;
//   - TightBudget marks a budget that cannot afford GEPA's minimal
//     effective run — routing GEPA there would send the loop to
//     starve, so it goes to p1 instead (提案 §3.1 ②);
//   - MultiConstraint is any multi-metric task.
func routeFeatures(task core.Task, kept []core.Sample, budgetEvals int64) optimizers.TaskFeatures {
	train := 0
	for _, s := range kept {
		if s.Split == "train" {
			train++
		}
	}
	return optimizers.TaskFeatures{
		JointFewShot:    train >= 2 && slices.Contains(verifiablePrimaries, task.Primary()),
		TightBudget:     budgetEvals > 0 && budgetEvals < tightBudgetEvals(len(kept)),
		MultiConstraint: len(task.Metrics) > 1,
	}
}

// tightBudgetEvals is the per-kept-sample evaluation cost below which
// a GEPA run cannot afford its minimal effective shape (提案 §3.1 ②):
// two probe variants + one baseline pass + minEffectiveRounds full
// passes. Setting a budget alone is not "tight"; a budget under this
// threshold would starve the baseline and the loop would be skipped —
// exactly the dead rule the old `budgetEvals < 2×kept` produced.
const minEffectiveRounds = 2

func tightBudgetEvals(kept int) int64 {
	return (2 + 1 + minEffectiveRounds) * int64(kept)
}

// verifiablePrimaries are the per-sample auto-judged metrics — the
// prerequisite for scoring demo-augmented candidates on a minibatch.
var verifiablePrimaries = []string{"exact_match", "f1", "json_validator", "llm_judge"}

// runOptimization drives the resolved optimizer over the retained
// sample set: it recomputes the kept samples (the checkpoint pause may
// have edited them), harvests the baseline records and wires the
// engine request with the paradigm options injected. judgeProv is the
// optional dedicated judge provider (nil = judge falls back to prov).
func runOptimization(ctx context.Context, runID, runDir, synthDir string, o runOptions,
	prov, judgeProv provider.Provider, budget *eval.Budget, collector *engine.RecordCollector, emit func(eval.Event), opt engine.Optimizer) (engine.Result, error) {
	spec, err := harness.LoadSpec(synthDir)
	if err != nil {
		return engine.Result{}, fmt.Errorf("reload spec.json: %w", err)
	}
	kept, err := retainedSamples(synthDir)
	if err != nil {
		return engine.Result{}, err
	}
	// The finalized kept set makes the run self-contained for trace
	// views (sample id → input/expected join).
	if err := writeJSONFile(filepath.Join(runDir, "dataset.json"), core.Dataset{
		Name: "synth", Samples: kept,
	}); err != nil {
		return engine.Result{}, fmt.Errorf("write dataset.json: %w", err)
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
		JudgeProvider:           judgeProv,
		JudgeModel:              o.judgeModel,
		JudgeMaxTokens:          o.judgeMaxTokens,
		JudgeBackend:            o.judgeBackend,
		DecisionClient:          decisionClient(o),
		JudgeDecisionConfidence: o.judgeDecisionConfidence,
		JudgeDecisionDiagBelow:  o.judgeDecisionDiagBelow,
		ExtraBody:               o.extraBody,
		Workers:                 o.workers,
		Temperature:             o.temperature,
		Reps:                    o.reps,
		Budget:                  budget,
		OptBudgetTokens:         o.budgetOptTokens,
		RunID:                   runID, RunDir: runDir,
		// SynthDir hands the paradigm the synthesis artifact root; p1
		// reads the probe variance report from it (提案 §3.1) instead
		// of re-deriving the synth/<id> path convention.
		SynthDir: synthDir,
		OnEvent:  emit,
		Opts:     map[string]string{"evoprompt.variant": o.evoVariant},
	}
	res, err := opt.Optimize(ctx, req)
	if err != nil {
		return res, err
	}
	// Cross-run candidate pool (提案 §3.3 最小版): the write lands after
	// the Loop's Finish/WriteOutputs — the frontier member carrying Best
	// holds the delivered candidate's fixed-order primary row. Zero-config
	// synth sets differ per run, so this usually records the entry and
	// stays below the same-set CI branch (the manual trio is the one
	// reachable scenario for it).
	if row, ok := frontierBestRow(res); ok {
		poolReflect(o, runID, runDir, spec.Task.Primary(), kept, row, res.BestMeans, res.Best)
	}
	return res, nil
}

// frontierBestRow projects the delivered best's per-sample primary row
// from the frontier: Loop.Finish always finds Best on the frontier, so
// the row rides the member record (fixed sample order, same projection
// as lineage/report). ok=false would mean an off-frontier best — the
// pool write is skipped (advisory), never the run.
func frontierBestRow(res engine.Result) ([]float64, bool) {
	for _, m := range res.Frontier {
		if m.ID() == res.Best.ID {
			return m.Scores, len(m.Scores) > 0
		}
	}
	return nil, false
}

// warnJudgeFallback prints one stderr line when the task declares
// llm_judge but no judge surface was configured: the judge then grades
// through the executor's provider/model/tokens, which is fine for
// self-evaluation but worth flagging. stderr-only — the headless
// stdout JSON summary stays unpolluted. The decision backend is its
// own configured judge (URL+model bind the decision surface), so the
// notice does not fire for it.
func warnJudgeFallback(w io.Writer, metrics []string, o runOptions) {
	if !slices.Contains(metrics, eval.MetricLLMJudge) {
		return
	}
	if o.judgeBackend == eval.JudgeBackendDecision {
		return
	}
	// "No judge configuration" means none of the five fields was set:
	// a lone --judge-base-url/--judge-api-key already separates the
	// judge instance, so the fallback notice would be lying then.
	if o.judgeProvider != "" || o.judgeBaseURL != "" || o.judgeAPIKey != "" ||
		o.judgeModel != "" || o.judgeMaxTokens != 0 {
		return
	}
	fmt.Fprintln(w, "promptopt run: 任务声明 llm_judge 但未配置裁判（--judge-provider/--judge-model/--judge-max-tokens），裁判回退执行器配置（同 Provider/Model/MaxTokens）")
}

// decisionClient builds the P7 decision-model judge surface when
// --judge-backend decision is on: the URL+model pair binds inside the
// client (one client serves one decision model for the whole run, the
// same binding rule as judgeConn). parseRunFlags already guaranteed
// both fields under the decision backend, so this never builds a
// half-configured surface; nil keeps the generative judge.
func decisionClient(o runOptions) *provider.SystemOneClient {
	if o.judgeBackend != eval.JudgeBackendDecision {
		return nil
	}
	return provider.NewSystemOne(o.judgeDecisionURL, o.judgeDecisionModel, provider.SystemOneConfig{Timeout: o.timeout})
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

// taskKeyDefault derives the anchor-library key when --task-key is
// unset (提案 §1.2): the zero-config prompt hashes to its first 12 hex
// characters — the synthesized task name is LLM-generated per run and
// unusable as a key — and the configured mode falls back to the task
// file's stem. Stability is a user convention: a renamed task file or
// an edited prompt starts a new library (宁缺勿错 — no fuzzy matching).
// The hash goes through core.HashInput, the single shared
// normalization+hash seam (P5/P8 共用).
func taskKeyDefault(prompt, taskPath string) string {
	if prompt != "" {
		return core.HashInput(prompt)[:12]
	}
	if taskPath != "" {
		return strings.TrimSuffix(filepath.Base(taskPath), filepath.Ext(taskPath))
	}
	return ""
}

// poolRoot places the cross-run candidate pool next to the synthesis
// tree (the same <out>/../ convention as synthRoot): the default
// --out runs/ yields a sibling pool/ directory, git-versionable like
// anchors/.
func poolRoot(runsDir string) string {
	return filepath.Join(runsDir, "..", "pool")
}

// poolReflect is a finished run's whole cross-run candidate pool
// interaction (提案 §3.3 最小版): compare the delivered candidate
// against the pool's historical best, emit the stderr warnings, then
// append this run's record. Only a double-consistent sample set (ID
// sequence AND per-sample input hash) runs the paired-bootstrap
// degradation warning; anything else degrades to a mean-only hint —
// deterministic IDs over different content cannot fake a pairing. The
// read happens here rather than at run start because the pairing needs
// this run's rows, which only exist after evaluation; Best is fetched
// before the append, so a run never compares against its own record.
// The pool is advisory: failures warn on stderr and never fail the
// run, and a regression is additionally recorded in the manifest's
// pool_warning (verify 门禁语义不受影响——预警不改变退出码).
func poolReflect(o runOptions, runID, runDir, primary string,
	samples []core.Sample, rows []float64, means map[string]float64, cand core.Candidate) string {
	if len(samples) == 0 || len(rows) != len(samples) {
		return ""
	}
	ids := make([]string, len(samples))
	hashes := make([]string, len(samples))
	for i, s := range samples {
		ids[i] = s.ID
		hashes[i] = core.HashInput(s.Input)
	}
	entry := pool.Entry{
		CandidateID: cand.ID, Prompt: cand.Prompt, Means: means,
		Rows: rows, InputHashes: hashes, SampleIDs: ids,
		RunID: runID, AddedAt: time.Now().UTC(),
	}
	store := pool.NewPoolStore(poolRoot(o.outDir))
	var warning string
	best, found, err := store.Best(o.taskKey, primary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: 读取候选池失败，跳过对照: %v\n", err)
	} else if found {
		cmp := pool.Compare(entry, best, primary)
		if cmp.SameSet {
			if cmp.Verdict == stats.CIRegressed {
				warning = fmt.Sprintf("regressed: CI=[%.4f,%.4f] 下界超阈值 %.2f（对照 run %s candidate %s，%s 均值 %.4f→%.4f）",
					cmp.Lo, cmp.Hi, pool.DefaultMaxRegression,
					best.RunID, best.CandidateID, primary, cmp.OldMean, cmp.NewMean)
				fmt.Fprintf(os.Stderr, "promptopt run: 候选池退化预警：本次 %s=%.4f 低于池内历史最优 %.4f（run %s），配对自助法 CI [%.4f, %.4f] 下界超过阈值 %.2f——退化超过噪声区间\n",
					primary, cmp.NewMean, cmp.OldMean, best.RunID, cmp.Lo, cmp.Hi, pool.DefaultMaxRegression)
			}
		} else {
			fmt.Fprintf(os.Stderr, "promptopt run: 跨 run 样本集不同，仅均值参考（本次 %s=%.4f vs 池内最优 %.4f，run %s）\n",
				primary, cmp.NewMean, cmp.OldMean, best.RunID)
		}
	}
	if err := store.Append(o.taskKey, entry); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: 写候选池失败: %v\n", err)
		return warning
	}
	if warning != "" {
		patchManifestPoolWarning(runDir, warning)
	}
	return warning
}

// patchManifestPoolWarning records the pool regression warning in the
// already-written manifest.json (read-modify-write, the same discipline
// as rewriteManifestRoute): the manifest snapshots the run before
// evaluation, but the warning only exists after it.
func patchManifestPoolWarning(runDir, warning string) {
	path := filepath.Join(runDir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: 读 manifest.json 失败，pool_warning 未记录: %v\n", err)
		return
	}
	var mf runManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: 解码 manifest.json 失败，pool_warning 未记录: %v\n", err)
		return
	}
	mf.PoolWarning = warning
	if err := writeJSONFile(path, mf); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt run: 写 manifest.json 失败，pool_warning 未记录: %v\n", err)
	}
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
		// 设置页快照（PRD-0001 切分 4 / D6，仿 sink 注入法）：起看板前
		// 发布合并后 options 的逐键值与来源（含 flag 层）——run --web 下
		// 真相在手的进程不得显示错误值（R1 #7：「文件 0.7 + flag 显式 0」
		// 页面必须显示 0）。零 NewServer 签名变更。
		config.PublishSnapshot(runConfigSnapshot(o))
		listen := o.listenAddr()
		dashboard, err := web.NewServer(o.outDir, synthDir, sink.bus).Listen(listen)
		if err != nil {
			sink.close()
			return nil, err
		}
		sink.dashboard = dashboard
		fmt.Fprintf(os.Stderr, "promptopt run: dashboard at http://%s/runs/<run_id>/dashboard\n", listen)
	}
	liveSinks.Store(sink, struct{}{})
	return sink, nil
}

// runConfigSnapshot builds the settings-page snapshot for this run
// （PRD-0001 切分 4 / D6）：逐键值取合并后的 options（即该 run 实际生效
// 值），来源取 o.sources——P1 单一 per-key 收集器的同一张表（R2 #5，
// 禁止第二套 bool/map），命中现场（路径/被遮蔽文件）取 parseRunFlags 的
// Discover 结果（R1 #10）。default 来源的行显示解析链缺省（如 timeout
// 0 = 构造器 180s）；显式 flag 0 保留字面 0——那才是 run 的真相。
func runConfigSnapshot(o runOptions) *config.Snapshot {
	row := func(key string, val any) config.SnapshotKey {
		return config.NewSnapshotKey(key, val, o.sources[key])
	}
	return &config.Snapshot{
		HitPath:  o.configHitPath,
		Shadowed: o.configShadowed,
		Keys: []config.SnapshotKey{
			row("provider", o.providerName),
			row("base_url", o.baseURL),
			row("model", o.model),
			row("api_key", o.apiKey),
			row("max_tokens", o.maxTokens),
			row("timeout", o.timeout),
			row("rps", o.rps),
			row("extra_body", o.extraBody),
			row("out", o.outDir),
			row("judge_provider", o.judgeProvider),
			row("judge_base_url", o.judgeBaseURL),
			row("judge_model", o.judgeModel),
			row("judge_api_key", o.judgeAPIKey),
			row("judge_max_tokens", o.judgeMaxTokens),
			row("judge_backend", o.judgeBackend),
			row("judge_decision_url", o.judgeDecisionURL),
			row("judge_decision_model", o.judgeDecisionModel),
			row("judge_decision_confidence", o.judgeDecisionConfidence),
			row("judge_decision_diag_below", o.judgeDecisionDiagBelow),
			row("optimizer", o.optimizer),
			row("evo_variant", o.evoVariant),
			row("spec_metrics", o.specMetrics),
		},
	}
}

// liveSinks tracks every opened-but-unclosed runSink. The run path
// closes its own sink on every normal and early-return path; the
// registry exists for the MCP reuse contract「每次调用一个 sink、调用必
// 关闭」: a panicked run unwinds without reaching those closes, and the
// tools/call handler reclaims the orphan via closeOrphanedSinks (its
// stop() releases the signal.NotifyContext so repeated calls never
// accumulate signal handlers).
var liveSinks sync.Map

// countLiveSinks is the no-backlog probe: after every completed
// tools/call optimize (and every CLI run) it reads zero.
func countLiveSinks() int {
	n := 0
	liveSinks.Range(func(_, _ any) bool { n++; return true })
	return n
}

// closeOrphanedSinks closes every sink left open by an unwound
// (panicked) run and returns how many were reclaimed. Idempotent on
// the normal path where the registry is already empty.
func closeOrphanedSinks() int {
	n := 0
	liveSinks.Range(func(k, _ any) bool {
		if s, ok := k.(*runSink); ok {
			s.close()
			n++
		}
		return true
	})
	return n
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
// handlers. Registry deletion first keeps close idempotent (a sink
// reclaimed via closeOrphanedSinks after a normal close is a no-op).
func (s *runSink) close() {
	liveSinks.Delete(s)
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
// the result per the output convention (headless: JSON on stdout —
// injected as out so the CLI keeps os.Stdout while the MCP surface
// silences it; the payload's source of truth is runDir/summary.json on
// disk either way). opt, when non-nil, adds the optimizer's
// best-prompt block to the human summary. With --web the dashboard
// stays up after the summary is written until Ctrl-C: the frontier
// page's adopt flow needs the pages (and the SSE stream) alive past
// run_done.
func finishRun(o runOptions, sink *runSink, res core.RunResult, runDir, primary string, opt *engine.Result, out io.Writer) int {
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
	if o.web {
		printHumanSummary(os.Stderr, res, runDir, primary, opt)
		printRunConclusion(os.Stderr, o, res, runDir, primary, opt)
		fmt.Fprintf(os.Stderr, "promptopt run: 看板驻留中（http://%s/runs/%s/dashboard），可在前沿看板采纳候选，Ctrl-C 退出\n",
			o.listenAddr(), res.RunID)
		<-sink.ctx.Done()
		sink.close()
		return res.ExitCode
	}
	sink.close()
	if o.headless {
		if err := json.NewEncoder(out).Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt run: encode summary: %v\n", err)
			return exitFailure
		}
		// --headless 只约束 stdout（纯 JSON 摘要）；stderr 沿用
		// pool/warn 提示的先例继续承载人类可读结论（CI 日志可见）。
		printRunConclusion(os.Stderr, o, res, runDir, primary, opt)
		return res.ExitCode
	}
	printHumanSummary(os.Stderr, res, runDir, primary, opt)
	printRunConclusion(os.Stderr, o, res, runDir, primary, opt)
	return res.ExitCode
}

// printRunConclusion renders the command-line conclusion block (P10
// 命令行结论呈现): the best candidate, the primary Δ with its delivery
// confidence verdict (paired bootstrap over the frontier's fixed-order
// rows — 提案 §1.1 C 层交付标注), the verify gate's three-state result
// read from the latest verify.json (or the 未过门禁 guidance) and the
// verify/adopt next steps with the dashboard address. Human modes only;
// the headless stdout JSON summary stays byte-clean. The exit-code
// contract (0/1/2/3) is carried by the caller, never by this block.
func printRunConclusion(w io.Writer, o runOptions, res core.RunResult, runDir, primary string, opt *engine.Result) {
	fmt.Fprintln(w, "--- run 结论 ---")
	if opt != nil && primary != "" {
		line := fmt.Sprintf("最优候选: %s · %s %.4f", opt.Best.ID, primary, opt.BestMeans[primary])
		if v := deliveryVerdict(opt, runDir, primary); v != "" {
			line += "（" + v + "）"
		}
		fmt.Fprintln(w, line)
	} else {
		fmt.Fprintf(w, "候选: %s · %s %.4f（手动模式无优化交付，无 Δ/置信判定）\n",
			res.CandidateID, cmp.Or(primary, "primary"), res.MetricMeans[primary])
	}
	// Gate line: the latest verify.json three-state result, or the
	// 未过门禁 guidance (a manual run has no frontier artifacts, so
	// verify cannot gate it — say so instead of pointing at a command
	// that would fail).
	if rep, ok := readLatestVerifyReport(runDir); ok {
		fmt.Fprintf(w, "门禁: %s\n", verifyGateLine(rep))
	} else if opt == nil {
		fmt.Fprintln(w, "门禁: 未过门禁——手动模式无 frontier 工件，verify 门禁不适用")
	} else {
		fmt.Fprintf(w, "门禁: 未过门禁——运行 promptopt verify %s 出具三态结论（退出码 0 通过 / 3 回归）\n", res.RunID)
	}
	if o.web {
		fmt.Fprintf(w, "看板: http://%s/runs/%s/dashboard（总览顶部横幅渲染同一门禁结论）\n", o.listenAddr(), res.RunID)
	} else {
		fmt.Fprintf(w, "看板: promptopt serve --runs-dir %s 后访问 /runs/%s/dashboard\n", o.outDir, res.RunID)
	}
}

// deliveryVerdict computes the delivered best's confidence verdict from
// the fixed-order per-sample rows: Δ = 最优−基线, and the paired
// bootstrap CI runs over D = 基线−最优 (verify's regression
// convention) with the shared stats implementation and the pool's
// 0.05 threshold (提案 §1.1 C 层交付标注). The best row lives on the
// frontier; the baseline row falls back to lineage.json because strict
// dominance EVICTS the baseline member from the frontier exactly when
// the delivery story is best. Returns "" when the rows are missing or
// mismatched — the annotation is evidence, never invented.
func deliveryVerdict(opt *engine.Result, runDir, primary string) string {
	var base, best []float64
	for _, m := range opt.Frontier {
		switch {
		case m.Operator == engine.OpBaseline:
			base = m.Scores
		case m.ID() == opt.Best.ID:
			best = m.Scores
		}
	}
	if len(base) == 0 {
		if lin, err := engine.LoadOrInitLineage(filepath.Join(runDir, "lineage.json")); err == nil {
			for _, rec := range lin.Records() {
				if rec.Operator == engine.OpBaseline {
					base = rec.Scores
					break
				}
			}
		}
	}
	if len(base) == 0 || len(base) != len(best) {
		return ""
	}
	// Δ from the rows themselves (mean of 最优−基线): self-consistent
	// with the CI input and independent of the member Means, which are
	// unreachable once the baseline was evicted.
	delta := 0.0
	for i := range best {
		delta += best[i] - base[i]
	}
	delta /= float64(len(best))
	lo, hi := stats.PairedBootstrapCI(base, best, pool.BootstrapResamples, stats.NewBootstrapRNG())
	switch stats.BootstrapVerdict(lo, hi, pool.DefaultMaxRegression) {
	case stats.CIConfidentPass:
		return fmt.Sprintf("Δ %+.4f · 配对自助法 CI [%.4f, %.4f] B=%d → 置信通过：提升超出噪声区间",
			delta, lo, hi, pool.BootstrapResamples)
	case stats.CIRegressed:
		return fmt.Sprintf("Δ %+.4f · 配对自助法 CI [%.4f, %.4f] B=%d → 回归：退化超过噪声区间",
			delta, lo, hi, pool.BootstrapResamples)
	default:
		return fmt.Sprintf("Δ %+.4f · 配对自助法 CI [%.4f, %.4f] B=%d → 不可判定：差异未超噪声区间，样本量不足",
			delta, lo, hi, pool.BootstrapResamples)
	}
}

// verifyGateLine renders the latest verify.json's three-state result
// for the conclusion block (the same conclusion the dashboard banner
// renders).
func verifyGateLine(rep verifyReport) string {
	if ci := rep.Regression.CI; ci != nil {
		switch ci.Verdict {
		case ciConfidentPass:
			return fmt.Sprintf("置信通过（%s Δ %.4f，CI [%.4f, %.4f] B=%d 上界 ≤ 0，提升可信；退出码 %d）",
				rep.Primary, rep.Regression.Delta, ci.Lo, ci.Hi, ci.B, rep.ExitCode)
		case ciRegressed:
			return fmt.Sprintf("回归（%s Δ %.4f，CI [%.4f, %.4f] B=%d 下界超阈值 %.2f，退化超过噪声区间；退出码 %d）",
				rep.Primary, rep.Regression.Delta, ci.Lo, ci.Hi, ci.B, rep.Regression.Threshold, rep.ExitCode)
		default:
			return fmt.Sprintf("不可判定（%s Δ %.4f，CI [%.4f, %.4f] B=%d 跨越阈值——差异未超噪声区间，样本量不足；退出码 %d 但不构成回归背书）",
				rep.Primary, rep.Regression.Delta, ci.Lo, ci.Hi, ci.B, rep.ExitCode)
		}
	}
	switch {
	case rep.ExitCode == exitBudgetExhausted:
		return fmt.Sprintf("证据不完整（存在未派发样本，回归结论不可得；退出码 %d）", rep.ExitCode)
	case rep.ExitCode == exitFailure:
		return fmt.Sprintf("验证评估失败（无有效结论；退出码 %d）", rep.ExitCode)
	case rep.Regression.Regressed:
		return fmt.Sprintf("回归（%s Δ %.4f 超过阈值 %.2f，均值差判定；退出码 %d）",
			rep.Primary, rep.Regression.Delta, rep.Regression.Threshold, rep.ExitCode)
	default:
		return fmt.Sprintf("通过（%s Δ %.4f 在阈值 %.2f 内，均值差判定未跑 CI；退出码 %d）",
			rep.Primary, rep.Regression.Delta, rep.Regression.Threshold, rep.ExitCode)
	}
}

// readLatestVerifyReport loads the newest runs/<id>/verify/<ts>/
// verify.json (timestamp directories sort lexicographically
// newest-first); ok=false when the run has not been verified yet.
func readLatestVerifyReport(runDir string) (verifyReport, bool) {
	entries, err := os.ReadDir(filepath.Join(runDir, "verify"))
	if err != nil {
		return verifyReport{}, false
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(b.Name(), a.Name()) })
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(runDir, "verify", e.Name(), "verify.json"))
		if err != nil {
			continue
		}
		var rep verifyReport
		if json.Unmarshal(b, &rep) == nil {
			return rep, true
		}
	}
	return verifyReport{}, false
}

// parseRunFlags parses and validates the run flag surface. Environment
// fallbacks (PROMPTOPT_BASE_URL/MODEL/API_KEY/OUT) apply to unset
// flags, and the config file layer (PRD-0001 切分 1) slots between env
// and the defaults: flag > env > 文件 > 默认, the chain owned by the
// config package's resolvers. Workers and addr are flag-only by
// design. A single positional argument switches to the zero-config
// mode, and flags may appear before or after it.
func parseRunFlags(args []string) (runOptions, error) {
	var o runOptions
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.taskPath, "task", "", "task YAML path (required without a positional prompt)")
	fs.StringVar(&o.candidatePath, "candidate", "", "candidate YAML path (required without a positional prompt)")
	fs.StringVar(&o.datasetPath, "dataset", "", "dataset YAML path (required without a positional prompt)")
	fs.StringVar(&o.split, "split", "", "dataset split to evaluate (default: all)")
	fs.StringVar(&o.taskKey, "task-key", "", "stable anchor-library key (default: zero-config = first 12 hex of the prompt sha256; configured mode = task file stem; snapshot into manifest.task_key for verify --promote/--anchor-lib)")
	fs.StringVar(&o.baseURL, "base-url", "", "OpenAI-compatible base URL (env PROMPTOPT_BASE_URL)")
	fs.StringVar(&o.model, "model", "", "model name (env PROMPTOPT_MODEL)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (env PROMPTOPT_API_KEY, default \"1\")")
	fs.IntVar(&o.maxTokens, "max-tokens", config.DefaultMaxTokens, "max completion tokens per request")
	fs.Float64Var(&o.temperature, "temperature", 0, "executor sampling temperature 0..2 (0 = field omitted, gateway default takes over; judge stays deterministic; noise magnitude unmeasured)")
	fs.IntVar(&o.reps, "reps", 1, "sampling repetitions per sample (each rep costs one evaluation and its executor budget; applies to evaluation units, zero-config baseline and manual runs; soft-stop accounting unchanged)")
	fs.IntVar(&o.budgetTokens, "budget-tokens", 0, "executor token budget, prompt+completion (0 = unlimited)")
	fs.IntVar(&o.budgetEvals, "budget-evals", 0, "max executor evaluations (0 = unlimited)")
	fs.IntVar(&o.workers, "workers", config.DefaultWorkers, "parallel evaluation workers")
	fs.IntVar(&o.samples, "samples", config.DefaultSamples, "synthesized sample count (zero-config mode)")
	fs.IntVar(&o.probeVariants, "probe-variants", config.DefaultProbeVariants, "probe prompt variants for variance filtering (zero-config mode)")
	var rawSpecMetrics string
	fs.StringVar(&rawSpecMetrics, "spec-metrics", "", "comma list pinning the synthesized spec's metrics, e.g. \"llm_judge,f1\" (zero-config mode; empty = the LLM chooses; each item must be a valid metric, no --spec-primary: the primary falls back to the first item)")
	fs.IntVar(&o.maxRounds, "max-rounds", config.DefaultMaxRounds, "GEPA optimization rounds (zero-config mode)")
	fs.IntVar(&o.minibatch, "minibatch", config.DefaultMinibatch, "samples drawn per reflection round (zero-config mode)")
	fs.Float64Var(&o.epsilon, "epsilon", config.DefaultEpsilon, "exploration rate of hypothesis selection, 0..1 (zero-config mode)")
	fs.IntVar(&o.stagnationLimit, "stagnation-limit", config.DefaultStagnationLimit, "stagnant rounds before a fresh restart (zero-config mode)")
	fs.Int64Var(&o.seed, "seed", 0, "optimization rng seed (0 = derive and record in the manifest)")
	fs.Int64Var(&o.budgetOptTokens, "budget-opt-tokens", 0, "optimizer token budget, cumulative (0 = unlimited)")
	fs.StringVar(&o.optimizer, "optimizer", config.DefaultOptimizer, "optimizer paradigm: a registered name or auto (zero-config mode; see docs/plugins.md)")
	fs.StringVar(&o.providerName, "provider", config.DefaultProvider, "LLM provider backend (openai|anthropic)")
	fs.Float64Var(&o.rps, "rps", 0, "client-side request pacing, requests per second (0 = off; applies per provider client, executor and a dedicated judge instance each)")
	var rawTimeout string
	fs.StringVar(&rawTimeout, "timeout", "", "per-attempt provider timeout: Go duration like 90s / 2m30s, or bare seconds like 300 (env PROMPTOPT_TIMEOUT; default 180s; applies per provider client, executor, judge and decision judge alike)")
	var rawExtraBody string
	fs.StringVar(&rawExtraBody, "extra-body", "", "gateway-private JSON object merged onto the request body top level, e.g. '{\"chat_template_kwargs\":{\"enable_thinking\":false}}' (parameter shape varies per gateway/model)")
	fs.StringVar(&o.judgeProvider, "judge-provider", "", "dedicated judge provider backend for llm_judge (openai|anthropic; empty = reuse --provider)")
	fs.StringVar(&o.judgeBaseURL, "judge-base-url", "", "judge API base URL (env PROMPTOPT_JUDGE_BASE_URL; empty = reuse --base-url)")
	fs.StringVar(&o.judgeModel, "judge-model", "", "judge model name (env PROMPTOPT_JUDGE_MODEL; empty = reuse --model)")
	fs.StringVar(&o.judgeAPIKey, "judge-api-key", "", "judge API key (env PROMPTOPT_JUDGE_API_KEY; empty = reuse --api-key)")
	fs.IntVar(&o.judgeMaxTokens, "judge-max-tokens", 0, "judge completion budget (env PROMPTOPT_JUDGE_MAX_TOKENS; 0 = reuse --max-tokens; judge output is just the verdict, a small value works)")
	fs.StringVar(&o.judgeBackend, "judge-backend", "", "llm_judge backend: llm (generative judge, default) or decision (SystemOne decision-model cascade, per-sample fallback to the generative judge)")
	fs.StringVar(&o.judgeDecisionURL, "judge-decision-url", "", "decision-model service base URL (env PROMPTOPT_JUDGE_DECISION_URL; the client posts to <url>/v1/systemone; required with --judge-backend decision)")
	fs.StringVar(&o.judgeDecisionModel, "judge-decision-model", "", "decision model name (env PROMPTOPT_JUDGE_DECISION_MODEL; required with --judge-backend decision)")
	fs.Float64Var(&o.judgeDecisionConfidence, "judge-decision-confidence", 0, "decision cascade: fall back to the generative judge when confidence < threshold (0 = default 0.5; tcmsp-30 实测 tev1:0.8b confidence 低至 0.21/0.111)")
	fs.Float64Var(&o.judgeDecisionDiagBelow, "judge-decision-diag-below", 0, "decision cascade: fetch the Chinese diagnosis from the generative judge when the normalized score < line (0 = default 0.6; the decision model covers the score half of the judge contract only)")
	fs.StringVar(&o.evoVariant, "evo-variant", config.DefaultEvoVariant, "evoprompt variant: ga|de (requires --optimizer evoprompt)")
	fs.BoolVar(&o.interactive, "interactive", false, "pause at the synthesis checkpoint for manual review (zero-config mode)")
	fs.StringVar(&o.outDir, "out", "", "run output directory (env PROMPTOPT_OUT, default runs/)")
	fs.StringVar(&o.addr, "addr", config.DefaultAddr, "dashboard listen address (host or host:port; implies dashboard; combine host-only with --port)")
	fs.IntVar(&o.port, "port", 0, "dashboard listen port when --addr is host-only (default 17700; implies dashboard; a full host:port --addr rejects --port)")
	fs.BoolVar(&o.web, "web", false, "serve a live SSE dashboard during the run")
	fs.BoolVar(&o.headless, "headless", false, "print only the JSON run summary")
	fs.StringVar(&o.configPath, "config", "", "配置文件路径（发现序第一层：显式给定而缺失即硬错；缺省按 PROMPTOPT_CONFIG > ./promptopt.yaml > 用户级 promptopt/config.yaml 发现，命中即严格解码）")
	// The stdlib flag package stops parsing at the first positional,
	// but the natural zero-config invocation puts flags after the
	// prompt (`promptopt run "<prompt>" --samples 3`): reorder so both
	// orders parse identically.
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return o, err
	}
	// --extra-body must parse as a JSON object up front: failing before
	// the synthesis pipeline runs beats burning its budget on a typo.
	extraBody, err := parseExtraBody(rawExtraBody)
	if err != nil {
		return o, err
	}
	o.extraBody = extraBody
	// --timeout parses strictly (flag side); env/file resolution joins
	// in the merge below, without failing runs over a malformed
	// optional env.
	o.timeout, err = validateTimeout(rawTimeout)
	if err != nil {
		return o, err
	}
	// --spec-metrics（D7）：逗号列表先行拆分裁剪，与 config set 的
	// spec_metrics 值共用同一 splitSpecMetrics 通道；flag 侧的显式空值
	// = 显式「LLM 自选」（终判压过文件层，--extra-body "" 同规）在拆分
	// 前短路——splitSpecMetrics 对空串产出一项空串（config set 侧把
	// 真空项留给 validateMetricsList 报错，语义归 P2 所有，此处不改）。
	var specMetricsFlag []string
	if strings.TrimSpace(rawSpecMetrics) != "" {
		specMetricsFlag = splitSpecMetrics(rawSpecMetrics)
	}
	// 单一来源收集器（R2 #5）：fs.Visit 一次性标记全部显式 flag——文件
	// 层合并、10 键显式性判定与切分 4 的快照来源标注共用这一张表，
	// parseRunFlags 不另立 per-key bool 与快照 map 并存。
	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	addrSet, portSet := explicit["addr"], explicit["port"]
	optimizerSet, evoVariantSet := explicit["optimizer"], explicit["evo-variant"]
	switch fs.NArg() {
	case 0:
	case 1:
		o.prompt = fs.Arg(0)
	default:
		return o, fmt.Errorf("unexpected argument %q (a single positional prompt enables the zero-config mode)", fs.Arg(1))
	}

	// 配置文件层（PRD-0001 切分 1）：四层发现序，首个存在的文件命中即
	// 止，命中即严格解码，损坏硬错（显式层连「存在」都严格——宣称失败
	// 必须响，隐含层缺失是常态静默跳过）。权限告警只走 stderr 一行，
	// 不报错（CI umask 千差万别）。
	disc, err := config.Discover(o.configPath)
	if err != nil {
		return o, err
	}
	// 命中现场入 options：设置页快照的来源可见性（R1 #10，切分 4）。
	o.configHitPath, o.configShadowed = disc.Path, disc.Shadowed
	if disc.Warning != "" {
		fmt.Fprintln(os.Stderr, "promptopt run: "+disc.Warning)
	}
	cfg := disc.File
	if cfg == nil {
		cfg = &config.File{}
	}
	// 文件 timeout 是书写面：文法非法即用法错（与 flag 同一 ParseTimeout
	// 文法），绝不静默读作 unset。
	fileTimeout, err := config.ParseTimeout(string(cfg.Timeout))
	if err != nil {
		return o, fmt.Errorf("配置文件 %s: %w", disc.Path, err)
	}

	// 合并在校验之前：文件层提供的值与 flag/env 值走完全相同的校验路径
	//（rps<0、confidence 越界等照报，报错值不分来源）。无文件时各
	// resolver 与旧三层语义逐字段相等（D2 字节级不变第一层，parity 表
	// 钉死在 config 包测试）。
	o.maxTokens = config.MaxTokens(o.maxTokens, explicit["max-tokens"], cfg.MaxTokens)
	o.providerName = config.Provider(o.providerName, explicit["provider"], cfg.Provider)
	o.rps = config.RPS(o.rps, explicit["rps"], cfg.RPS)
	o.extraBody = config.ExtraBody(o.extraBody, explicit["extra-body"], cfg.ExtraBody)
	o.optimizer = config.Optimizer(o.optimizer, explicit["optimizer"], cfg.Optimizer)
	o.evoVariant = config.EvoVariant(o.evoVariant, explicit["evo-variant"], cfg.EvoVariant)
	o.specMetrics = config.SpecMetrics(specMetricsFlag, explicit["spec-metrics"], cfg.SpecMetrics)
	o.judgeBackend = config.JudgeBackend(o.judgeBackend, cfg.JudgeBackend)
	o.timeout = config.Timeout(o.timeout, fileTimeout)
	o.sources = config.DeriveSources(explicit, cfg)

	// configured（三件套）模式的范式键提示（D3/R1 #5）：文件承载的
	// optimizer/evo_variant/spec_metrics 在该模式下不生效——提示而非
	// 静默（范式 flag「不得静默通过」成文哲学延伸到文件层），也不硬错
	//（CI 复用机器文件跑三件套不该被炸）。
	if o.prompt == "" && (cfg.Optimizer != "" || cfg.EvoVariant != "" || len(cfg.SpecMetrics) > 0) {
		var keys []string
		if cfg.Optimizer != "" {
			keys = append(keys, "optimizer")
		}
		if cfg.EvoVariant != "" {
			keys = append(keys, "evo_variant")
		}
		if len(cfg.SpecMetrics) > 0 {
			keys = append(keys, "spec_metrics")
		}
		fmt.Fprintf(os.Stderr, "promptopt run: 配置文件 %s 中的 %s 在配置模式（task/candidate/dataset 三件套）下不生效（该模式无优化循环）\n",
			disc.Path, strings.Join(keys, "/"))
	}

	var errs []error
	// --optimizer is validated up front against the builtin registry
	// (plus "auto") in both modes: failing before the synthesis
	// pipeline runs beats burning its budget only to error at Build
	// time.
	if err := validateOptimizer(o.optimizer); err != nil {
		errs = append(errs, err)
	}
	if err := validateProvider(o.providerName); err != nil {
		errs = append(errs, err)
	}
	// --spec-metrics（D7）：合并后的列表不分来源走同一 validateMetricsList
	// 准入（flag/file 报错同一段语义）；llm_judge 合法——引擎在注册表
	// 之前特判分发。
	if err := validateMetricsList(o.specMetrics); err != nil {
		errs = append(errs, err)
	}
	// The task key names a library directory — reject path traversal
	// before anything is written under it.
	if o.taskKey != "" {
		if err := anchors.ValidateTaskKey(o.taskKey); err != nil {
			errs = append(errs, fmt.Errorf("--task-key: %w", err))
		}
	}
	// The judge provider mirrors the --provider whitelist up front so a
	// typo fails before the synthesis pipeline burns its budget.
	if err := validateJudgeProvider(o.judgeProvider); err != nil {
		errs = append(errs, err)
	}
	if o.judgeMaxTokens < 0 {
		errs = append(errs, fmt.Errorf("--judge-max-tokens must be zero (reuse --max-tokens) or positive, got %d", o.judgeMaxTokens))
	}
	if err := validateJudgeBackend(o.judgeBackend); err != nil {
		errs = append(errs, err)
	}
	if o.judgeDecisionConfidence < 0 || o.judgeDecisionConfidence > 1 {
		errs = append(errs, fmt.Errorf("--judge-decision-confidence must be within [0, 1] (0 = default), got %v", o.judgeDecisionConfidence))
	}
	if o.judgeDecisionDiagBelow < 0 || o.judgeDecisionDiagBelow > 1 {
		errs = append(errs, fmt.Errorf("--judge-decision-diag-below must be within [0, 1] (0 = default), got %v", o.judgeDecisionDiagBelow))
	}
	if o.rps < 0 {
		errs = append(errs, fmt.Errorf("--rps must be zero (off) or positive, got %v", o.rps))
	}
	if o.evoVariant != "ga" && o.evoVariant != "de" {
		errs = append(errs, fmt.Errorf("--evo-variant must be ga or de, got %q", o.evoVariant))
	}
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
		// --evo-variant only steers the evoprompt paradigm.
		if evoVariantSet && o.optimizer != "evoprompt" {
			errs = append(errs, fmt.Errorf("--evo-variant only takes effect with --optimizer evoprompt (got %q)", o.optimizer))
		}
	} else {
		// The configured mode runs no optimization loop: the paradigm
		// flags must not pass silently.
		if optimizerSet {
			errs = append(errs, errors.New("--optimizer only takes effect in the zero-config mode (the configured mode runs no optimization loop)"))
		}
		if evoVariantSet {
			errs = append(errs, errors.New("--evo-variant only takes effect in the zero-config mode"))
		}
		// --spec-metrics 与三件套互斥（D7：仅零配置模式合法；显式给定
		// 即报错——沿用上方 --optimizer/--evo-variant 的显式性先例，文件
		// 层值走下方成文提示不硬错）。
		if explicit["spec-metrics"] {
			errs = append(errs, errors.New("--spec-metrics only takes effect in the zero-config mode (the configured mode's metrics come from the task file)"))
		}
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
	// Dashboard address surface (P10 方案 a): a full host:port --addr
	// wins as-is and cannot combine with --port; a host-only --addr
	// pairs with --port; either flag given implies the dashboard, so
	// both are mutually exclusive with --headless (the old "addr
	// requires --web" guard is gone — -addr and --addr are the same
	// flag and the bare-host form must start the dashboard).
	if addrSet && portSet {
		if _, _, err := net.SplitHostPort(o.addr); err == nil {
			errs = append(errs, fmt.Errorf("--addr %q already carries a port and cannot combine with --port (use a host-only --addr with --port)", o.addr))
		}
	}
	if (addrSet || portSet) && o.headless {
		errs = append(errs, errors.New("--addr/--port 任一显式给定即隐含起看板，与 --headless 互斥（--headless 无看板）"))
	}
	if o.maxTokens <= 0 {
		errs = append(errs, fmt.Errorf("--max-tokens must be positive, got %d", o.maxTokens))
	}
	if o.temperature < 0 || o.temperature > 2 {
		errs = append(errs, fmt.Errorf("--temperature must be within [0, 2], got %v", o.temperature))
	}
	if o.reps < 1 {
		errs = append(errs, fmt.Errorf("--reps must be >= 1, got %d", o.reps))
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
	o.baseURL = config.BaseURL(o.baseURL, cfg.BaseURL)
	if o.baseURL == "" {
		// D5 缺参指路（PRD-0001）：报错职责是一次性给全全部合法出路，
		// 文案快照钉在 TestD5MissingParamGuidance。
		errs = append(errs, errRunBaseURLRequired)
	}
	o.model = config.Model(o.model, cfg.Model)
	if o.model == "" {
		errs = append(errs, errRunModelRequired)
	}
	o.apiKey = config.APIKey(o.apiKey, cfg.APIKey)
	o.outDir = config.OutDir(o.outDir, cfg.Out)
	// Judge surface resolves flag > PROMPTOPT_JUDGE_* env > 文件; per-field
	// fallback to the executor values happens later — inside
	// eval.Engine for provider/model/tokens, at the dedicated-provider
	// construction for base URL and API key. The parsed values stay
	// "as configured" here so the manifest snapshot records exactly
	// what the user set (nothing appears when nothing was set).
	o.judgeProvider = config.JudgeProvider(o.judgeProvider, cfg.JudgeProvider)
	o.judgeBaseURL = config.JudgeBaseURL(o.judgeBaseURL, cfg.JudgeBaseURL)
	o.judgeModel = config.JudgeModel(o.judgeModel, cfg.JudgeModel)
	o.judgeAPIKey = config.JudgeAPIKey(o.judgeAPIKey, cfg.JudgeAPIKey)
	// 显式性键（R1 #2）：显式 0 = 回落 --max-tokens / eval 默认，终判
	// 压过文件层。
	o.judgeMaxTokens = config.JudgeMaxTokens(o.judgeMaxTokens, explicit["judge-max-tokens"], cfg.JudgeMaxTokens)
	// Decision surface (P7): only the connection fields carry an env
	// fallback; the completeness check runs after the merge so an
	// env- or file-provided URL/model counts.
	o.judgeDecisionURL = config.JudgeDecisionURL(o.judgeDecisionURL, cfg.JudgeDecisionURL)
	o.judgeDecisionModel = config.JudgeDecisionModel(o.judgeDecisionModel, cfg.JudgeDecisionModel)
	o.judgeDecisionConfidence = config.JudgeDecisionConfidence(o.judgeDecisionConfidence, explicit["judge-decision-confidence"], cfg.JudgeDecisionConfidence)
	o.judgeDecisionDiagBelow = config.JudgeDecisionDiagBelow(o.judgeDecisionDiagBelow, explicit["judge-decision-diag-below"], cfg.JudgeDecisionDiagBelow)
	if o.judgeBackend == eval.JudgeBackendDecision && (o.judgeDecisionURL == "" || o.judgeDecisionModel == "") {
		// config set 不拦跨键完整性，这里就是完整性报错的落点（D5/R1 #6）。
		errs = append(errs, errRunDecisionRequired)
	}
	// Explicit --addr/--port implies the dashboard (equivalent to
	// --web; a plain --web run is unchanged). Recorded even when other
	// validation failed — the caller exits on error without using o.
	o.addrSet, o.portSet = addrSet, portSet
	o.web = o.web || addrSet || portSet
	return o, errors.Join(errs...)
}

// listenAddr resolves the dashboard listen address from the --addr/
// --port pair (P10 方案 a; parseRunFlags already rejected the
// conflicting full-address+--port combination): a full host:port
// address wins as-is (现状不变，端口不被拆丢), a bare host combines
// with --port (default 17700), the port flag alone binds loopback, and
// neither flag leaves the historical default byte-identical.
func (o runOptions) listenAddr() string {
	return resolveListenAddr(o.addr, o.addrSet, o.portSet, o.port)
}

// parseExtraBody decodes the --extra-body flag value into the map
// merged onto the wire payload top level. An empty value means unset;
// anything that is not a JSON object (including arrays and scalars) is
// a usage error — the merge contract operates on the object's keys.
func parseExtraBody(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("--extra-body must be a JSON object, got %q: %v", raw, err)
	}
	return m, nil
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
	// TaskKey is the anchor-library key (V7 §1.2): verify --promote and
	// --anchor-lib resolve it from this snapshot; omitted in legacy
	// artifacts where it was never recorded.
	TaskKey      string `json:"task_key,omitempty"`
	Model        string `json:"model"`
	BaseURL      string `json:"base_url"`
	MaxTokens    int    `json:"max_tokens"`
	Workers      int    `json:"workers"`
	BudgetTokens int64  `json:"budget_tokens"`
	BudgetEvals  int64  `json:"budget_evals"`
	// Executor statistical knobs: temperature 0 keeps the field off
	// the wire (gateway default), reps 1 is single sampling — both
	// omitted so legacy manifests read unchanged.
	Temperature   float64 `json:"temperature,omitempty"`
	Reps          int     `json:"reps,omitempty"`
	Samples       int     `json:"samples,omitempty"`
	Mode          string  `json:"mode,omitempty"`
	Prompt        string  `json:"prompt,omitempty"`
	SynthSamples  int     `json:"synth_samples,omitempty"`
	ProbeVariants int     `json:"probe_variants,omitempty"`
	// Optimizer routing: the initial snapshot carries the requested
	// --optimizer value; resolveOptimizer rewrites the final paradigm,
	// the request and the route reason once the retained set is known.
	Optimizer            string `json:"optimizer,omitempty"`
	OptimizerRequested   string `json:"optimizer_requested,omitempty"`
	OptimizerRouteReason string `json:"optimizer_route_reason,omitempty"`
	Provider             string `json:"provider,omitempty"`
	EvoVariant           string `json:"evo_variant,omitempty"`
	// SpecMetrics snapshots the --spec-metrics pin (D7, PRD-0001 切分 3):
	// the metrics the synthesized spec was overridden to; omitted when
	// the LLM chose them (empty pin). Zero-config mode only.
	SpecMetrics []string `json:"spec_metrics,omitempty"`
	// Outbound pacing and gateway-private extras (V7 §2.2): omitted
	// entirely when unset so legacy manifests read unchanged.
	// extra_body is also the run-level audit anchor for the flag (the
	// per-call trace records the same map under extra_body).
	// timeout_seconds snapshots an explicitly configured per-attempt
	// provider deadline (--timeout / PROMPTOPT_TIMEOUT, in seconds);
	// 0 (the 180s default) omits — verify replays the same deadline.
	RPS            float64        `json:"rps,omitempty"`
	ExtraBody      map[string]any `json:"extra_body,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	// Judge surface snapshot (V7 裁判隔离): exactly what the user set
	// for the optional second judge LLM — omitted entirely when nothing
	// was configured, so legacy manifests read unchanged. The judge API
	// key never persists here.
	JudgeProvider  string `json:"judge_provider,omitempty"`
	JudgeBaseURL   string `json:"judge_base_url,omitempty"`
	JudgeModel     string `json:"judge_model,omitempty"`
	JudgeMaxTokens int    `json:"judge_max_tokens,omitempty"`
	// Decision-model judge surface (P7 级联): the decision backend and
	// its connection/thresholds, so verify reproduces the run's judge
	// backend. Thresholds snapshot as configured (0 = the eval-package
	// defaults, omitted by omitempty — same "0 = default" semantics at
	// every layer).
	JudgeBackend            string  `json:"judge_backend,omitempty"`
	JudgeDecisionURL        string  `json:"judge_decision_url,omitempty"`
	JudgeDecisionModel      string  `json:"judge_decision_model,omitempty"`
	JudgeDecisionConfidence float64 `json:"judge_decision_confidence,omitempty"`
	JudgeDecisionDiagBelow  float64 `json:"judge_decision_diag_below,omitempty"`
	// PoolWarning is the cross-run candidate pool's degradation warning
	// (提案 §3.3 最小版): set only when the same-set paired bootstrap
	// regressed against the pool's historical best — first runs and
	// different-set mean-only comparisons stay silent on the manifest.
	PoolWarning string `json:"pool_warning,omitempty"`
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
	for _, role := range []core.Role{core.RoleExecutor, core.RoleJudge, core.RoleOptimizer} {
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

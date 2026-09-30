package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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
)

// Verification-set modes recorded in verify.json. The anchor mode is the
// ADR 0001 trusted gate (real user samples, never fed into the
// optimization loop); the holdout mode is its declared fallback — an
// independently resynthesized set over the original run spec, same task
// contract and output schema, no p¹ probes, conclusions explicitly
// caveated.
const (
	verifyModeAnchor  = "anchor"
	verifyModeHoldout = "holdout"
	verifyModeSkipped = "skipped"
)

// defaultMaxRegression is the tolerated primary-metric mean drop
// (baseline − delivered) before the delivered candidate counts as
// regressed.
const defaultMaxRegression = 0.05

// verifyOptions is the resolved verify configuration. Connection
// parameters resolve flag > env (PROMPTOPT_*) > run manifest.
type verifyOptions struct {
	runsDir, anchorPath string
	holdoutSamples      int     // 0 = inherit manifest.synth_samples
	maxRegression       float64 // tolerated primary mean drop, default 0.05
	maxAvgTokens        int64   // delivered avg tokens per sample cap (judge included), 0 = off
	maxAvgLatencyMS     int64   // delivered avg latency per sample cap (executor+judge), 0 = off

	baseURL, model, apiKey, providerName          string
	maxTokens, workers, budgetTokens, budgetEvals int

	headless bool
}

// verifyCommand re-validates a finished run's delivered candidate
// against its baseline on a verification set: the --anchor real-sample
// dataset when provided (ADR 0001), otherwise a resynthesized same-spec
// holdout set with an explicit caveat. Exit codes: 0 pass, 1 usage or
// evaluation failure, 2 incomplete evidence (undispatched samples, no
// regression verdict possible; takes precedence), 3 regression or
// constraint violation.
func verifyCommand(args []string) int {
	o, runID, err := parseVerifyFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r, err := loadVerifyRun(o.runsDir, runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	synthDir := filepath.Join(synthRoot(o.runsDir), runID)
	spec, err := harness.LoadSpec(synthDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: 读取任务契约失败：%v（verify 需要原 run 的 synth spec：指标/主指标与 baseline 兜底）\n", err)
		return exitFailure
	}
	task := spec.Task

	base, deliv, err := r.candidates(synthDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}

	verifyDir := filepath.Join(r.Dir, "verify", time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(verifyDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	rep := verifyReport{
		RunID: runID, VerifiedAt: time.Now().UTC(),
		Mode: verifyModeSkipped, Primary: task.Primary(),
	}

	// Delivered equals baseline: nothing was optimized away, so there is
	// nothing to verify — record the skip and exit 0 without any LLM
	// dial (connection parameters are not even required).
	if strings.EqualFold(strings.TrimSpace(base.Prompt), strings.TrimSpace(deliv.Prompt)) {
		rep.Note = "交付候选与 baseline 提示词相同（该 run 无优化交付），跳过评估"
		rep.ExitCode = exitOK
		return finishVerify(o, rep, verifyDir)
	}

	o, err = resolveVerifyConn(o, r.Manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	prov, err := newProvider(o.providerName, o.baseURL, o.apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}

	samples, mode, caveat, synthUsage, err := verificationSet(ctx, o, task, verifyDir, prov,
		cmp.Or(o.holdoutSamples, r.Manifest.SynthSamples))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	rep.Mode, rep.Caveat = mode, caveat
	if mode == verifyModeAnchor {
		rep.AnchorPath = o.anchorPath
		if err := writeJSONFile(filepath.Join(verifyDir, "anchor-dataset.json"), core.Dataset{
			Name: "anchor", Samples: samples,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
			return exitFailure
		}
	} else {
		rep.HoldoutUsage = &synthUsage
	}

	// Fixed order: baseline first, then delivered — per-side budgets are
	// independent, so the order cannot leak budget into the verdict.
	baseSide, err := evaluateCandidate(ctx, base, task, samples, o, prov, filepath.Join(verifyDir, "baseline"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	delivSide, err := evaluateCandidate(ctx, deliv, task, samples, o, prov, filepath.Join(verifyDir, "delivered"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	rep.Baseline, rep.Delivered = baseSide, delivSide
	rep.Regression, rep.Constraints, rep.ExitCode = verdict(task.Primary(), baseSide, delivSide, o, task.Metrics)
	return finishVerify(o, rep, verifyDir)
}

// finishVerify writes verify.json/verify.md, prints the verdict (JSON on
// stdout when headless, human text on stderr otherwise) and returns the
// process exit code.
func finishVerify(o verifyOptions, rep verifyReport, verifyDir string) int {
	if err := writeJSONFile(filepath.Join(verifyDir, "verify.json"), rep); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	if err := os.WriteFile(filepath.Join(verifyDir, "verify.md"),
		[]byte(renderVerifyMarkdown(rep, o)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt verify: %v\n", err)
		return exitFailure
	}
	if o.headless {
		if err := json.NewEncoder(os.Stdout).Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt verify: encode report: %v\n", err)
			return exitFailure
		}
		return rep.ExitCode
	}
	printVerifySummary(os.Stderr, rep, verifyDir)
	return rep.ExitCode
}

// parseVerifyFlags parses the verify flag surface. The run id is the
// single positional argument and may appear before or after the flags.
func parseVerifyFlags(args []string) (verifyOptions, string, error) {
	var o verifyOptions
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.runsDir, "runs-dir", config.DefaultOutDir, "directory containing run artifacts")
	fs.StringVar(&o.anchorPath, "anchor", "", "anchor dataset YAML with 3~5 real samples (ADR 0001); omit to fall back to a resynthesized holdout set")
	fs.IntVar(&o.holdoutSamples, "holdout-samples", 0, "holdout set size (0 = inherit the run's synth_samples)")
	fs.Float64Var(&o.maxRegression, "max-regression", defaultMaxRegression, "tolerated primary-metric mean drop (baseline - delivered)")
	fs.Int64Var(&o.maxAvgTokens, "max-avg-tokens", 0, "delivered avg tokens per sample cap, judge included (0 = off)")
	fs.Int64Var(&o.maxAvgLatencyMS, "max-avg-latency-ms", 0, "delivered avg latency per sample cap in ms, executor+judge (0 = off)")
	fs.StringVar(&o.providerName, "provider", "", "LLM provider backend (flag > env > manifest, default openai)")
	fs.StringVar(&o.baseURL, "base-url", "", "OpenAI-compatible base URL (flag > env PROMPTOPT_BASE_URL > manifest)")
	fs.StringVar(&o.model, "model", "", "model name (flag > env PROMPTOPT_MODEL > manifest)")
	fs.StringVar(&o.apiKey, "api-key", "", "API key (env PROMPTOPT_API_KEY, default \"1\")")
	fs.IntVar(&o.maxTokens, "max-tokens", config.DefaultMaxTokens, "max completion tokens per request")
	fs.IntVar(&o.workers, "workers", config.DefaultWorkers, "parallel evaluation workers")
	fs.IntVar(&o.budgetTokens, "budget-tokens", 0, "executor token budget PER SIDE, prompt+completion (0 = unlimited)")
	fs.IntVar(&o.budgetEvals, "budget-evals", 0, "max executor evaluations PER SIDE (0 = unlimited)")
	fs.BoolVar(&o.headless, "headless", false, "print only the JSON verify report")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return o, "", err
	}
	if fs.NArg() != 1 {
		return o, "", fmt.Errorf("expected exactly one run id, got %d arguments", fs.NArg())
	}
	var errs []error
	if o.maxRegression < 0 {
		errs = append(errs, fmt.Errorf("--max-regression must be >= 0, got %v", o.maxRegression))
	}
	if o.maxAvgTokens < 0 {
		errs = append(errs, fmt.Errorf("--max-avg-tokens must be zero (off) or positive, got %d", o.maxAvgTokens))
	}
	if o.maxAvgLatencyMS < 0 {
		errs = append(errs, fmt.Errorf("--max-avg-latency-ms must be zero (off) or positive, got %d", o.maxAvgLatencyMS))
	}
	if o.holdoutSamples < 0 {
		errs = append(errs, fmt.Errorf("--holdout-samples must be >= 0, got %d", o.holdoutSamples))
	}
	if o.maxTokens <= 0 {
		errs = append(errs, fmt.Errorf("--max-tokens must be positive, got %d", o.maxTokens))
	}
	if o.workers <= 0 {
		errs = append(errs, fmt.Errorf("--workers must be positive, got %d", o.workers))
	}
	if o.budgetTokens < 0 || o.budgetEvals < 0 {
		errs = append(errs, errors.New("budgets must be zero (unlimited) or positive"))
	}
	return o, fs.Arg(0), errors.Join(errs...)
}

// resolveVerifyConn applies the connection contract shared with CI (no
// flags needed when PROMPTOPT_* env or the manifest carry the values):
// provider = flag > manifest > openai; base URL and model = flag > env >
// manifest, all empty being a usage error. The API key never persists in
// the manifest and keeps the env default "1".
func resolveVerifyConn(o verifyOptions, mf runManifest) (verifyOptions, error) {
	o.providerName = cmp.Or(o.providerName, mf.Provider, "openai")
	o.baseURL = cmp.Or(o.baseURL, os.Getenv(config.EnvBaseURL), mf.BaseURL)
	if o.baseURL == "" {
		return o, errors.New("--base-url (或 PROMPTOPT_BASE_URL，或 manifest.base_url) is required")
	}
	o.model = cmp.Or(o.model, os.Getenv(config.EnvModel), mf.Model)
	if o.model == "" {
		return o, errors.New("--model (或 PROMPTOPT_MODEL，或 manifest.model) is required")
	}
	o.apiKey = config.APIKey(o.apiKey)
	return o, nil
}

// verifyRun aggregates one run's verification inputs: the persisted
// result, the manifest (connection fallback + holdout size), the
// frontier (baseline + delivered) and the optional human adoption.
type verifyRun struct {
	RunID, Dir string
	Res        core.RunResult
	Manifest   runManifest
	Frontier   engine.FrontierFile
	Adopted    *adoptedRecord
}

// loadVerifyRun reads the run artifacts verify depends on. A missing
// frontier.json names the two known causes: manual-mode runs produce no
// optimization artifacts at all, and a zero-config run whose budget
// starved the baseline skips the loop (run.go jumps straight to
// finishRun) so no frontier is written either.
func loadVerifyRun(runsDir, runID string) (verifyRun, error) {
	dir := filepath.Join(runsDir, runID)
	b, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if errors.Is(err, os.ErrNotExist) {
		if b, err = os.ReadFile(filepath.Join(dir, "summary.json")); err != nil {
			return verifyRun{}, fmt.Errorf("读取 run 摘要（run.json/summary.json）: %w", err)
		}
	}
	if err != nil {
		return verifyRun{}, fmt.Errorf("读取 run.json: %w", err)
	}
	var res core.RunResult
	if err := json.Unmarshal(b, &res); err != nil {
		return verifyRun{}, fmt.Errorf("decode run.json: %w", err)
	}
	var mf runManifest
	if err := harness.LoadJSON(filepath.Join(dir, "manifest.json"), &mf); err != nil {
		return verifyRun{}, fmt.Errorf("读取 manifest.json: %w", err)
	}
	f, err := engine.LoadFrontier(dir)
	if err != nil {
		return verifyRun{}, fmt.Errorf("读取 frontier.json: %w（手动模式 run 不产生优化工件；预算饿死 baseline 的零配置 run 也不写 frontier.json——请提高预算后重跑）", err)
	}
	r := verifyRun{RunID: runID, Dir: dir, Res: res, Manifest: mf, Frontier: f}
	if a, ok := readAdopted(dir); ok {
		r.Adopted = &a
	}
	return r, nil
}

// candidates resolves the two comparison subjects: the delivered
// candidate (the human adoption wins over the frontier's Best pick) and
// the baseline (the frontier's baseline member, falling back to the
// synth spec's prompt_template for artifacts predating member prompts).
func (r verifyRun) candidates(synthDir string) (baseline, delivered core.Candidate, err error) {
	if r.Adopted != nil {
		delivered = core.Candidate{ID: r.Adopted.CandidateID, Prompt: r.Adopted.Prompt}
	} else {
		delivered = core.Candidate{ID: r.Frontier.Best.ID, Prompt: r.Frontier.Best.Prompt}
	}
	if strings.TrimSpace(delivered.Prompt) == "" {
		return baseline, delivered, errors.New("交付候选提示词为空：旧版 frontier.json/adopted.json 缺少 prompt 字段，无法验证")
	}
	id, prompt, ok := resolveBaselinePrompt(synthDir, r.Frontier)
	if !ok {
		return baseline, delivered, errors.New("无法解析 baseline 提示词：frontier 无 baseline 成员提示词，且 synth spec 缺少 prompt_template")
	}
	return core.Candidate{ID: id, Prompt: prompt}, delivered, nil
}

// verificationSet builds the evaluation set: the anchor dataset when
// given (3~5 real samples per ADR 0001, structural separation from the
// optimization loop guaranteed by loading it only here), otherwise a
// freshly synthesized holdout set over the original run spec — same task
// contract and output schema, no p¹ probes, no filtering. Synthesis
// usage is accounted separately (never in either side's budget) and
// returned for the report. n is the holdout size (flag or manifest).
func verificationSet(ctx context.Context, o verifyOptions, spec core.Task, verifyDir string,
	prov provider.Provider, n int) (samples []core.Sample, mode string, caveat bool, synthUsage core.Usage, err error) {
	if o.anchorPath != "" {
		ds, err := core.LoadDataset(o.anchorPath)
		if err != nil {
			return nil, "", false, core.Usage{}, fmt.Errorf("读取锚点数据集: %w", err)
		}
		if len(ds.Samples) < 3 {
			return nil, "", false, core.Usage{}, fmt.Errorf("锚点验证集至少需要 3 条真实样本（ADR 0001），当前 %d 条", len(ds.Samples))
		}
		if len(ds.Samples) > 5 {
			fmt.Fprintf(os.Stderr, "promptopt verify: 锚点样本 %d 条，超过 ADR 0001 建议的 3~5 条\n", len(ds.Samples))
		}
		return ds.Samples, verifyModeAnchor, false, core.Usage{}, nil
	}
	if n <= 0 {
		return nil, "", false, core.Usage{}, errors.New("无法确定保留集规模：--holdout-samples 与 manifest.synth_samples 均为空")
	}
	synth := &harness.Synthesizer{
		Provider: prov, Model: o.model, MaxTokens: o.maxTokens,
		Dir: filepath.Join(verifyDir, "holdout"),
	}
	samples, _, err = synth.SynthesizeSamples(ctx, spec, n)
	if err != nil {
		return nil, "", false, core.Usage{}, fmt.Errorf("合成保留集失败: %w", err)
	}
	if err := harness.SaveSamples(filepath.Join(verifyDir, "holdout"), harness.SampleFile{Samples: samples}); err != nil {
		return nil, "", false, core.Usage{}, fmt.Errorf("写 holdout/samples.json: %w", err)
	}
	return samples, verifyModeHoldout, true, synth.Usage(), nil
}

// evaluateCandidate runs one side's evaluation with its own fresh
// budget: --budget-tokens/--budget-evals are per-side caps, so the two
// sides and the holdout synthesis cannot pollute each other. Tokens and
// latency are tallied from the per-sample events, never from the
// budget, and the usage snapshot comes from this side's engine result.
func evaluateCandidate(ctx context.Context, cand core.Candidate, task core.Task,
	samples []core.Sample, o verifyOptions, prov provider.Provider, dir string) (verifySide, error) {
	coll := &verifyCollector{}
	eng := &eval.Engine{
		RunID:       "verify-" + cand.ID,
		RunDir:      dir,
		Model:       o.model,
		MaxTokens:   o.maxTokens,
		Workers:     o.workers,
		Metrics:     task.Metrics,
		Budget:      eval.NewBudget(int64(o.budgetTokens), int64(o.budgetEvals)),
		Provider:    prov,
		TaskName:    task.Name,
		CandidateID: cand.ID,
		DatasetName: "verify",
		OnEvent:     coll.observe,
	}
	res, err := eng.Run(ctx, cand, samples)
	if err != nil {
		return verifySide{}, fmt.Errorf("evaluate %s: %w", cand.ID, err)
	}
	return verifySide{
		CandidateID:  cand.ID,
		Means:        res.MetricMeans,
		Evaluated:    res.Evaluated,
		Failed:       res.FailedSamples,
		Undispatched: res.Undispatched,
		AvgTokens:    coll.avgTokens(),
		AvgLatencyMS: coll.avgLatencyMS(),
		Usage:        res.UsageByRole[core.RoleExecutor],
	}, nil
}

// verdict is the pure regression/constraint gate. Priority follows the
// exit contract 2 > 1 > 3: undispatched samples (either side) mean
// incomplete evidence — no regression verdict is possible; failures come
// next; only a fully evaluated pair can regress (primary mean drop over
// the threshold, json_validator delivery rate under 1, or the avg
// tokens/latency caps on the delivered side).
func verdict(primary string, base, deliv verifySide, o verifyOptions, taskMetrics []string) (regressionInfo, constraintsInfo, int) {
	reg := regressionInfo{
		Primary:       primary,
		BaselineMean:  base.Means[primary],
		DeliveredMean: deliv.Means[primary],
		Delta:         base.Means[primary] - deliv.Means[primary],
		Threshold:     o.maxRegression,
	}
	reg.Regressed = reg.Delta > o.maxRegression

	con := constraintsInfo{
		AvgTokens:       deliv.AvgTokens,
		MaxAvgTokens:    o.maxAvgTokens,
		AvgLatencyMS:    deliv.AvgLatencyMS,
		MaxAvgLatencyMS: o.maxAvgLatencyMS,
	}
	if slices.Contains(taskMetrics, "json_validator") {
		rate := deliv.Means["json_validator"]
		con.JSONRate = &rate
		con.JSONViolated = rate < 1
	}
	con.TokensViolated = o.maxAvgTokens > 0 && deliv.AvgTokens > float64(o.maxAvgTokens)
	con.LatencyViolated = o.maxAvgLatencyMS > 0 && deliv.AvgLatencyMS > float64(o.maxAvgLatencyMS)
	con.Violated = con.JSONViolated || con.TokensViolated || con.LatencyViolated

	switch {
	case base.Undispatched > 0 || deliv.Undispatched > 0:
		return reg, con, exitBudgetExhausted
	case len(base.Failed) > 0 || len(deliv.Failed) > 0:
		return reg, con, exitFailure
	case reg.Regressed || con.Violated:
		return reg, con, exitRegression
	}
	return reg, con, exitOK
}

// verifySide is one candidate's verification outcome. Means/Evaluated/
// Failed/Undispatched mirror the engine result; AvgTokens and
// AvgLatencyMS are tallied from the per-sample events (usage semantics:
// total evaluation cost of the sample — executor plus judge once the
// llm_judge metric lands); Usage is this side's engine
// UsageByRole[executor] snapshot under its own budget.
type verifySide struct {
	CandidateID  string             `json:"candidate_id"`
	Means        map[string]float64 `json:"metric_means"`
	Evaluated    int                `json:"evaluated"`
	Failed       []string           `json:"failed_samples,omitempty"`
	Undispatched int                `json:"undispatched"`
	AvgTokens    float64            `json:"avg_tokens_per_sample"`
	AvgLatencyMS float64            `json:"avg_latency_ms_per_sample"`
	Usage        core.Usage         `json:"usage"`
}

// regressionInfo records the primary-metric regression verdict.
type regressionInfo struct {
	Primary       string  `json:"primary"`
	BaselineMean  float64 `json:"baseline_mean"`
	DeliveredMean float64 `json:"delivered_mean"`
	Delta         float64 `json:"delta"` // baseline − delivered; > Threshold regresses
	Threshold     float64 `json:"threshold"`
	Regressed     bool    `json:"regressed"`
}

// constraintsInfo records the hard-constraint verdicts on the delivered
// side. JSONRate is present only when the task declares json_validator
// (delivery-side legal-JSON rate must be exactly 1); the token/latency
// caps are off when zero.
type constraintsInfo struct {
	JSONRate        *float64 `json:"json_valid_rate,omitempty"`
	JSONViolated    bool     `json:"json_valid_violated"`
	AvgTokens       float64  `json:"avg_tokens_per_sample"`
	MaxAvgTokens    int64    `json:"max_avg_tokens,omitempty"`
	TokensViolated  bool     `json:"avg_tokens_violated"`
	AvgLatencyMS    float64  `json:"avg_latency_ms_per_sample"`
	MaxAvgLatencyMS int64    `json:"max_avg_latency_ms,omitempty"`
	LatencyViolated bool     `json:"avg_latency_violated"`
	Violated        bool     `json:"violated"`
}

// verifyReport is verify.json (and the --headless stdout payload).
type verifyReport struct {
	RunID        string          `json:"run_id"`
	VerifiedAt   time.Time       `json:"verified_at"`
	Mode         string          `json:"mode"`
	AnchorPath   string          `json:"anchor_path,omitempty"`
	Caveat       bool            `json:"caveat"`
	Note         string          `json:"note,omitempty"`
	Primary      string          `json:"primary"`
	Baseline     verifySide      `json:"baseline"`
	Delivered    verifySide      `json:"delivered"`
	Regression   regressionInfo  `json:"regression"`
	Constraints  constraintsInfo `json:"constraints"`
	HoldoutUsage *core.Usage     `json:"holdout_usage,omitempty"`
	ExitCode     int             `json:"exit_code"`
}

// verifyCollector tallies per-sample evidence from sample_done events.
// Counting from the events — never from the shared Budget — keeps the
// two sides' token/latency numbers structurally unpolluted.
type verifyCollector struct {
	mu        sync.Mutex
	samples   int64
	tokens    int64
	latencyMS int64
}

func (c *verifyCollector) observe(ev eval.Event) {
	if ev.Type != eval.EventSampleDone || ev.Error != "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples++
	if ev.Usage != nil {
		c.tokens += ev.Usage.Total()
	}
	c.latencyMS += eventLatencyMS(ev)
}

func (c *verifyCollector) avgTokens() float64 {
	if c.samples == 0 {
		return 0
	}
	return float64(c.tokens) / float64(c.samples)
}

func (c *verifyCollector) avgLatencyMS() float64 {
	if c.samples == 0 {
		return 0
	}
	return float64(c.latencyMS) / float64(c.samples)
}

// eventLatencyMS returns one sample's evaluation latency: the executor
// call plus the judge call — LatencyMS stays the executor call alone
// and JudgeMS carries the judge call when the llm_judge metric is
// declared (the same event's Usage already totals both calls' tokens).
func eventLatencyMS(ev eval.Event) int64 { return ev.LatencyMS + ev.JudgeMS }

// adoptedRecord mirrors internal/web's adoptedFile (runs/<id>/
// adopted.json): candidate id, full prompt and adoption time.
type adoptedRecord struct {
	CandidateID string    `json:"candidate_id"`
	Prompt      string    `json:"prompt"`
	AdoptedAt   time.Time `json:"adopted_at"`
}

// readAdopted decodes adopted.json; ok is false when absent or broken.
func readAdopted(runDir string) (adoptedRecord, bool) {
	var a adoptedRecord
	b, err := os.ReadFile(filepath.Join(runDir, "adopted.json"))
	if err != nil {
		return a, false
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, false
	}
	return a, true
}

// frontierPrompt resolves a candidate id to its prompt from a frontier
// file — member prompts first, the best pick as fallback for artifacts
// predating member prompts. It mirrors internal/web's parsing semantics
// without importing the package.
func frontierPrompt(f engine.FrontierFile, id string) (string, bool) {
	for _, m := range f.Members {
		if m.ID == id {
			if m.Prompt != "" {
				return m.Prompt, true
			}
			break
		}
	}
	if f.Best.ID == id && f.Best.Prompt != "" {
		return f.Best.Prompt, true
	}
	return "", false
}

// renderVerifyMarkdown produces the Chinese verification report. It
// always states the verification mode and its trust boundary, the
// per-side budget semantics, the fixed evaluation order and the exit
// code table (2 = incomplete evidence, no regression verdict).
func renderVerifyMarkdown(rep verifyReport, o verifyOptions) string {
	var b strings.Builder
	b.WriteString("# 验证报告（verify）\n\n")
	fmt.Fprintf(&b, "- run：%s\n- 验证时间：%s\n", rep.RunID, rep.VerifiedAt.Format(time.RFC3339))
	switch rep.Mode {
	case verifyModeAnchor:
		fmt.Fprintf(&b, "- 模式：锚点验证集（%s，真实样本，仅用于本次最终验证）\n", rep.AnchorPath)
	case verifyModeHoldout:
		b.WriteString("- 模式：合成保留集（按原 run spec 独立重合成，无 p¹ 探针、无过滤）\n")
		b.WriteString("- ⚠️ 结论未经真实数据验证：合成保留集是 ADR 0001 的降级路径，对同族偏差不设防，真实数据背书需本地 --anchor\n")
	case verifyModeSkipped:
		b.WriteString("- 模式：跳过（交付候选与 baseline 相同，无优化交付）\n")
	}
	if rep.Note != "" {
		fmt.Fprintf(&b, "- %s\n", rep.Note)
	}
	fmt.Fprintf(&b, "- 主指标：%s\n- 评估顺序：固定先 baseline 后 delivered\n", rep.Primary)
	fmt.Fprintf(&b, "- 预算语义：--budget-tokens/--budget-evals 为每侧独立上限（本次各 %d tokens / %d evals，0=不限）\n",
		o.budgetTokens, o.budgetEvals)
	b.WriteString("- 退出码：0 通过；1 评估失败；2 证据不完整（存在未派发样本，回归结论不可得，优先级最高）；3 回归或约束违反\n\n")

	if rep.Mode == verifyModeSkipped {
		return b.String()
	}

	b.WriteString("## 指标对照\n\n| 指标 | baseline | delivered |\n|---|---|---|\n")
	metrics := make([]string, 0, len(rep.Baseline.Means)+len(rep.Delivered.Means))
	for m := range rep.Baseline.Means {
		metrics = append(metrics, m)
	}
	for m := range rep.Delivered.Means {
		if !slices.Contains(metrics, m) {
			metrics = append(metrics, m)
		}
	}
	slices.Sort(metrics)
	for _, m := range metrics {
		marker := ""
		if m == rep.Primary {
			marker = " *"
		}
		fmt.Fprintf(&b, "| %s%s | %.4f | %.4f |\n", m, marker, rep.Baseline.Means[m], rep.Delivered.Means[m])
	}
	b.WriteString("\n## 回归判定\n\n")
	fmt.Fprintf(&b, "- 阈值 %.4f；Δ = baseline − delivered = %.4f\n", rep.Regression.Threshold, rep.Regression.Delta)
	if rep.Regression.Regressed {
		b.WriteString("- 判定：**回归**（Δ 超过阈值，退出码 3）\n")
	} else {
		b.WriteString("- 判定：通过\n")
	}

	b.WriteString("\n## 约束检查（交付侧）\n\n")
	if rep.Constraints.JSONRate != nil {
		state := "通过"
		if rep.Constraints.JSONViolated {
			state = "违反（交付侧 JSON 合法率须为 100%）"
		}
		fmt.Fprintf(&b, "- json_validator 合法率：%.4f → %s\n", *rep.Constraints.JSONRate, state)
	} else {
		b.WriteString("- json_validator：任务未声明，跳过\n")
	}
	if rep.Constraints.MaxAvgTokens > 0 {
		fmt.Fprintf(&b, "- 平均每样本 token（含 judge）：%.1f（上限 %d）→ %s\n",
			rep.Constraints.AvgTokens, rep.Constraints.MaxAvgTokens, passLabel(!rep.Constraints.TokensViolated))
	} else {
		b.WriteString("- 平均每样本 token：未设上限（--max-avg-tokens）\n")
	}
	if rep.Constraints.MaxAvgLatencyMS > 0 {
		fmt.Fprintf(&b, "- 平均每样本延迟（executor+judge）：%.1f ms（上限 %d）→ %s\n",
			rep.Constraints.AvgLatencyMS, rep.Constraints.MaxAvgLatencyMS, passLabel(!rep.Constraints.LatencyViolated))
	} else {
		b.WriteString("- 平均每样本延迟：未设上限（--max-avg-latency-ms）\n")
	}

	b.WriteString("\n## 用量\n\n")
	for _, side := range []struct {
		name  string
		usage core.Usage
	}{{"baseline 侧", rep.Baseline.Usage}, {"delivered 侧", rep.Delivered.Usage}} {
		fmt.Fprintf(&b, "- %s：prompt=%d，completion=%d，合计=%d\n",
			side.name, side.usage.PromptTokens, side.usage.CompletionTokens, side.usage.Total())
	}
	if rep.HoldoutUsage != nil {
		fmt.Fprintf(&b, "- 保留集合成（独立记账，不进任一侧预算）：prompt=%d，completion=%d\n",
			rep.HoldoutUsage.PromptTokens, rep.HoldoutUsage.CompletionTokens)
	}
	return b.String()
}

func passLabel(pass bool) string {
	if pass {
		return "通过"
	}
	return "违反"
}

// printVerifySummary renders the human verdict on stderr.
func printVerifySummary(w io.Writer, rep verifyReport, dir string) {
	fmt.Fprintf(w, "\nverify %s: exit %d\n", rep.RunID, rep.ExitCode)
	switch rep.Mode {
	case verifyModeAnchor:
		fmt.Fprintf(w, "模式: 锚点验证集（%s）\n", rep.AnchorPath)
	case verifyModeHoldout:
		fmt.Fprintln(w, "模式: 合成保留集（结论未经真实数据验证）")
	case verifyModeSkipped:
		fmt.Fprintln(w, "模式: 跳过（无优化交付）")
	}
	if rep.Note != "" {
		fmt.Fprintf(w, "%s\n", rep.Note)
	}
	if rep.Mode != verifyModeSkipped {
		fmt.Fprintf(w, "%s: baseline %.4f → delivered %.4f（Δ %.4f，阈值 %.4f）\n",
			rep.Primary, rep.Regression.BaselineMean, rep.Regression.DeliveredMean,
			rep.Regression.Delta, rep.Regression.Threshold)
		if rep.Constraints.JSONRate != nil {
			fmt.Fprintf(w, "json_validator 合法率: %.4f（要求 1.0）\n", *rep.Constraints.JSONRate)
		}
		fmt.Fprintf(w, "每样本: %.1f tok, %.1f ms（delivered，含 judge 口径）\n",
			rep.Constraints.AvgTokens, rep.Constraints.AvgLatencyMS)
		fmt.Fprintf(w, "baseline %d 评估/%d 未派发, delivered %d 评估/%d 未派发\n",
			rep.Baseline.Evaluated, rep.Baseline.Undispatched, rep.Delivered.Evaluated, rep.Delivered.Undispatched)
	}
	fmt.Fprintf(w, "artifacts: %s\n", dir)
}

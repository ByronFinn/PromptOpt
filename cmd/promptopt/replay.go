package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/eval"
	"github.com/ByronFinn/PromptOpt/internal/harness"
	"github.com/ByronFinn/PromptOpt/internal/provider"
)

// Call sources of the audit timeline.
const (
	replaySourceBaseline  = "baseline"  // runs/<id>/calls: baseline evaluation
	replaySourceOptEval   = "opt-eval"  // runs/<id>/evals/<unit>/calls: optimization units
	replaySourceOptimizer = "optimizer" // runs/<id>/opt-calls: reflection/mutation
	replaySourceSynth     = "synthesis" // synth/<id>/calls: task/sample/probe synthesis
)

// replayOptions is the resolved replay configuration.
type replayOptions struct {
	runsDir        string
	full, headless bool
}

// auditEntry is one step of the merged audit timeline — a decision
// event from events.jsonl or an LLM call traced under one of the call
// trees — sorted by time.
type auditEntry struct {
	Time      time.Time      `json:"time"`
	Kind      string         `json:"kind"` // event | call
	Label     string         `json:"label"`
	Role      core.Role      `json:"role,omitempty"`
	Stage     string         `json:"stage,omitempty"`
	SampleID  string         `json:"sample_id,omitempty"`
	Model     string         `json:"model,omitempty"`
	Tokens    int64          `json:"tokens,omitempty"`
	LatencyMS int64          `json:"latency_ms,omitempty"`
	Error     string         `json:"error,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// auditSummary is the coverage digest printed after the timeline.
type auditSummary struct {
	RunID         string                   `json:"run_id"`
	Events        int                      `json:"events"`
	TotalCalls    int                      `json:"total_calls"`
	CallsBySource map[string]int           `json:"calls_by_source"`
	FailedCalls   int                      `json:"failed_calls"`
	UsageByRole   map[core.Role]core.Usage `json:"usage_by_role,omitempty"`
	Best          string                   `json:"best,omitempty"`
	Adopted       string                   `json:"adopted,omitempty"`
}

// replayCallTrace decodes one CallTrace artifact. It is a local view:
// the additive Stage field (llm_judge) decodes as the empty string on
// artifacts predating it, so replay reads both generations. ExtraBody
// (V7 --extra-body) reads as nil on artifacts predating it; carrying
// it into the timeline is what lets the audit explain why the same
// model behaves differently across calls.
type replayCallTrace struct {
	Seq       int                   `json:"seq"`
	SampleID  string                `json:"sample_id"`
	Role      core.Role             `json:"role"`
	Request   provider.ChatRequest  `json:"request"`
	ExtraBody map[string]any        `json:"extra_body,omitempty"`
	Response  provider.ChatResponse `json:"response"`
	LatencyMS int64                 `json:"latency_ms"`
	Time      time.Time             `json:"time"`
	Error     string                `json:"error,omitempty"`
	Stage     string                `json:"stage,omitempty"`
}

// replayCommand replays one run's complete call and decision audit
// timeline: events.jsonl merged with every call tree (baseline
// evaluation, optimization units, optimizer dials, synthesis), sorted
// by time, plus a coverage summary. --headless emits the entries as
// JSONL on stdout; the human rendering goes to stderr.
func replayCommand(args []string) int {
	o, runID, err := parseReplayFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "promptopt replay: %v\n", err)
		return exitFailure
	}
	runDir := filepath.Join(o.runsDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt replay: run 目录不存在：%s\n", runDir)
		return exitFailure
	}
	entries, summary, err := collectAudit(runDir, filepath.Join(synthRoot(o.runsDir), runID), o.full)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt replay: %v\n", err)
		return exitFailure
	}
	if o.headless {
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(os.Stderr, "promptopt replay: encode entry: %v\n", err)
				return exitFailure
			}
		}
		return exitOK
	}
	printReplayHuman(os.Stderr, runDir, entries, summary)
	return exitOK
}

// parseReplayFlags parses the replay flag surface; the run id is the
// single positional argument.
func parseReplayFlags(args []string) (replayOptions, string, error) {
	var o replayOptions
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.runsDir, "runs-dir", config.DefaultOutDir, "directory containing run artifacts")
	fs.BoolVar(&o.full, "full", false, "expand full prompts and responses instead of excerpts")
	fs.BoolVar(&o.headless, "headless", false, "emit the audit timeline as JSONL")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return o, "", err
	}
	if fs.NArg() != 1 {
		return o, "", fmt.Errorf("expected exactly one run id, got %d arguments", fs.NArg())
	}
	return o, fs.Arg(0), nil
}

// collectAudit merges the run's decision events and every traced LLM
// call into one time-sorted timeline and derives the coverage summary.
// Ties keep insertion order (events first, then calls in trace-file
// order). full switches the call bodies from excerpts to complete
// texts.
func collectAudit(runDir, synthDir string, full bool) ([]auditEntry, auditSummary, error) {
	summary := auditSummary{RunID: filepath.Base(runDir), CallsBySource: map[string]int{}}
	var entries []auditEntry

	if b, err := os.ReadFile(filepath.Join(runDir, "events.jsonl")); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var ev eval.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				return nil, summary, fmt.Errorf("decode events.jsonl line: %w", err)
			}
			detail := ev.Detail
			if len(ev.Scores) > 0 {
				detail = maps.Clone(ev.Detail)
				if detail == nil {
					detail = make(map[string]any, 1)
				}
				detail["scores"] = ev.Scores
			}
			entries = append(entries, auditEntry{
				Time: ev.Time, Kind: "event", Label: replayEventLabel(ev),
				SampleID: ev.SampleID, Error: ev.Error, Detail: detail,
			})
			summary.Events++
		}
	}

	addCalls := func(dir, source, unit string) error {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return err
		}
		for _, path := range files {
			var tc replayCallTrace
			if err := harness.LoadJSON(path, &tc); err != nil {
				return fmt.Errorf("decode %s: %w", path, err)
			}
			stage := tc.Stage
			if stage == "" {
				stage = stageFromTraceFile(path)
			}
			// The rendered evaluation prompt is the last user message of
			// the traced request.
			prompt := ""
			if len(tc.Request.Messages) > 0 {
				prompt = tc.Request.Messages[len(tc.Request.Messages)-1].Content
			}
			detail := map[string]any{"prompt": clipBody(prompt, full)}
			if tc.Response.Content != "" {
				detail["response"] = clipBody(tc.Response.Content, full)
			}
			if len(tc.ExtraBody) > 0 {
				detail["extra_body"] = tc.ExtraBody
			}
			if unit != "" {
				detail["unit"] = unit
			}
			entries = append(entries, auditEntry{
				Time: tc.Time, Kind: "call", Label: replayCallLabel(source, stage, tc),
				Role: tc.Role, Stage: stage, SampleID: tc.SampleID,
				Model: tc.Request.Model, Tokens: tc.Response.Usage.Total(),
				LatencyMS: tc.LatencyMS, Error: tc.Error, Detail: detail,
			})
			summary.CallsBySource[source]++
			summary.TotalCalls++
			if tc.Error != "" {
				summary.FailedCalls++
			}
		}
		return nil
	}
	if err := addCalls(filepath.Join(runDir, "calls"), replaySourceBaseline, ""); err != nil {
		return nil, summary, err
	}
	if units, err := os.ReadDir(filepath.Join(runDir, "evals")); err == nil {
		for _, u := range units {
			if u.IsDir() {
				if err := addCalls(filepath.Join(runDir, "evals", u.Name(), "calls"), replaySourceOptEval, u.Name()); err != nil {
					return nil, summary, err
				}
			}
		}
	}
	if err := addCalls(filepath.Join(runDir, "opt-calls"), replaySourceOptimizer, ""); err != nil {
		return nil, summary, err
	}
	if err := addCalls(filepath.Join(synthDir, "calls"), replaySourceSynth, ""); err != nil {
		return nil, summary, err
	}

	slices.SortStableFunc(entries, func(a, b auditEntry) int { return a.Time.Compare(b.Time) })

	if b, err := os.ReadFile(filepath.Join(runDir, "run.json")); err == nil {
		var res core.RunResult
		if json.Unmarshal(b, &res) == nil {
			summary.UsageByRole = res.UsageByRole
		}
	}
	if f, err := engine.LoadFrontier(runDir); err == nil {
		summary.Best = f.Best.ID
	}
	if a, ok := readAdopted(runDir); ok {
		summary.Adopted = a.CandidateID
	}
	return entries, summary, nil
}

// clipBody returns the call body verbatim when full, else a 120-rune
// excerpt.
func clipBody(s string, full bool) string {
	if full {
		return s
	}
	return truncateRunes(s, 120)
}

// replayCallLabel renders one call's Chinese label from its source and
// stage (judge calls surface through their Stage once the metric
// lands).
func replayCallLabel(source, stage string, tc replayCallTrace) string {
	names := map[string]string{
		replaySourceBaseline:  "基线评估调用",
		replaySourceOptEval:   "优化评估调用",
		replaySourceOptimizer: "优化调用",
		replaySourceSynth:     "合成调用",
	}
	label := names[source]
	if stage != "" && stage != tc.SampleID {
		label += "·" + stage
	} else if tc.SampleID != "" {
		label += "·" + tc.SampleID
	}
	return label
}

// stageFromTraceFile recovers a stage label from the NNN-<stage>.json
// trace filename (legacy artifacts carry no Stage field).
func stageFromTraceFile(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// replayEventLabel renders one event's Chinese label, reusing the
// progressPrinter vocabulary.
func replayEventLabel(ev eval.Event) string {
	switch ev.Type {
	case eval.EventRunStart:
		return "运行开始"
	case eval.EventSampleStart:
		return "样本开始"
	case eval.EventSampleDone:
		if ev.Error != "" {
			return "样本失败"
		}
		return "样本完成"
	case eval.EventBudgetStop:
		if role, _ := ev.Detail["role"].(string); role != "" {
			return "预算停止（" + role + "）"
		}
		return "预算停止：停止派发，在途样本继续"
	case eval.EventRunDone:
		return "运行结束（" + cmp.Or(ev.Status, "?") + "，exit " + fmt.Sprint(ev.ExitCode) + "）"
	case harness.EventSynthDone:
		return "合成完成：" + detailCount(ev.Detail, "samples") + " 条样本、" +
			detailCount(ev.Detail, "probes") + " 条探针变体" + detailWarnings(ev.Detail)
	case harness.EventFilterDone:
		return "方差过滤：保留 " + detailCount(ev.Detail, "kept") + " 条，剔除 " + detailCount(ev.Detail, "dropped") + " 条"
	case harness.EventFilterFallback:
		return "方差过滤：无区分样本（全部 dead/noisy），回退使用全部 " +
			detailCount(ev.Detail, "samples") + " 条合成样本"
	case harness.EventCheckpoint:
		if status, _ := ev.Detail["status"].(string); status == "pending" {
			return "检查点：合成集等待审核"
		}
		return "检查点：已放行"
	case engine.EventRoundStart:
		return "优化第 " + detailCount(ev.Detail, "round") + " 轮开始"
	case engine.EventReflectDone:
		return "反思完成：" + detailCount(ev.Detail, "hypotheses") + " 条假设"
	case engine.EventHypoValidated:
		mode, _ := ev.Detail["mode"].(string)
		sel, _ := ev.Detail["selected_hypothesis"].(string)
		return "假设验证：选中 " + sel + "（模式 " + mode + "）"
	case engine.EventMutateDone:
		operator, _ := ev.Detail["operator"].(string)
		cand, _ := ev.Detail["candidate"].(string)
		return "突变完成：" + operator + " → " + cand
	case engine.EventFrontierUpdated:
		if admitted, _ := ev.Detail["admitted"].(bool); admitted {
			return "前沿更新：" + detailAny(ev.Detail["candidate"]) + " 准入"
		}
		return "前沿更新：" + detailAny(ev.Detail["candidate"]) + " 未准入"
	case engine.EventVistaRestart:
		return "VISTA 重启：连续 " + detailCount(ev.Detail, "stagnant") + " 轮无改进"
	case engine.EventRoundDone:
		if skipped, _ := ev.Detail["skipped"].(bool); skipped {
			return "第 " + detailCount(ev.Detail, "round") + " 轮跳过（优化侧调用失败，记停滞）"
		}
		return "第 " + detailCount(ev.Detail, "round") + " 轮结束：主指标均值 " + detailFloat(ev.Detail, "primary_mean")
	case engine.EventUsage:
		return "用量快照（" + detailAny(ev.Detail["stage"]) + "）"
	default:
		return ev.Type
	}
}

// printReplayHuman renders the manifest header, the merged timeline and
// the coverage summary on stderr.
func printReplayHuman(w io.Writer, runDir string, entries []auditEntry, summary auditSummary) {
	var mf runManifest
	if err := harness.LoadJSON(filepath.Join(runDir, "manifest.json"), &mf); err == nil {
		header := "run " + summary.RunID + "（" + cmp.Or(mf.Mode, "manual") + "，model " + mf.Model
		if mf.Optimizer != "" {
			header += "，optimizer " + mf.Optimizer
		}
		header += "）"
		fmt.Fprintln(w, header)
	}
	for _, e := range entries {
		line := fmt.Sprintf("[%s] %s", e.Time.Format("15:04:05.000"), e.Label)
		var parts []string
		if e.Role != "" {
			parts = append(parts, string(e.Role))
		}
		if e.Model != "" {
			parts = append(parts, e.Model)
		}
		if e.Tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tok", e.Tokens))
		}
		if e.LatencyMS > 0 {
			parts = append(parts, fmt.Sprintf("%d ms", e.LatencyMS))
		}
		if len(parts) > 0 {
			line += "（" + strings.Join(parts, " · ") + "）"
		}
		if e.Error != "" {
			line += " 错误: " + truncateRunes(e.Error, 120)
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "覆盖率：events %d · calls %d", summary.Events, summary.TotalCalls)
	if len(summary.CallsBySource) > 0 {
		keys := slices.Sorted(maps.Keys(summary.CallsBySource))
		kv := make([]string, 0, len(keys))
		for _, k := range keys {
			kv = append(kv, fmt.Sprintf("%s %d", k, summary.CallsBySource[k]))
		}
		fmt.Fprintf(w, "（%s）", strings.Join(kv, " / "))
	}
	if summary.FailedCalls > 0 {
		fmt.Fprintf(w, " · 失败调用 %d", summary.FailedCalls)
	}
	fmt.Fprintln(w)
	if summary.Best != "" || summary.Adopted != "" {
		fmt.Fprintf(w, "best: %s；采纳: %s\n", cmp.Or(summary.Best, "—"), cmp.Or(summary.Adopted, "—"))
	}
}

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ByronFinn/PromptOpt/internal/config"
	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
	"github.com/ByronFinn/PromptOpt/internal/harness"
)

// rollbackOptions is the resolved rollback configuration.
type rollbackOptions struct {
	runsDir, to, emit string
	headless          bool
}

// rollbackResult is the --headless stdout payload.
type rollbackResult struct {
	RunID   string `json:"run_id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Prompt  string `json:"prompt"`
	Emitted string `json:"emitted,omitempty"`
}

// rollbackCommand rolls a run's adoption back: the current adoption is
// appended to adopted-history.jsonl first, then adopted.json is rewritten
// to the target — the previous different adoption from the history, the
// frontier baseline, or the candidate named by --to. Exit codes: 0 done,
// 1 usage error or nothing to roll back to.
func rollbackCommand(args []string) int {
	o, runID, err := parseRollbackFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "promptopt rollback: %v\n", err)
		return exitFailure
	}
	runDir := filepath.Join(o.runsDir, runID)
	if _, err := os.Stat(runDir); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt rollback: run 目录不存在：%s\n", runDir)
		return exitFailure
	}
	current, ok := readAdopted(runDir)
	if !ok {
		fmt.Fprintf(os.Stderr, "promptopt rollback: 没有 adopted.json（%s）：先在前沿看板采纳候选或手写该文件\n", runDir)
		return exitFailure
	}
	frontier, err := engine.LoadFrontier(runDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt rollback: 读取 frontier.json: %v（手动模式 run 不产生优化工件，无回退目标）\n", err)
		return exitFailure
	}
	synthDir := filepath.Join(synthRoot(o.runsDir), runID)
	target, err := rollbackTarget(o.to, current, readAdoptedHistory(runDir), frontier, synthDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt rollback: %v\n", err)
		return exitFailure
	}
	if target.CandidateID == current.CandidateID {
		fmt.Fprintf(os.Stderr, "promptopt rollback: 当前采纳即 %q，无需回退\n", current.CandidateID)
		return exitFailure
	}
	// History first: once adopted.json is rewritten the previous verdict
	// would be lost, so the current adoption is journaled before the
	// switch. The append is a single small JSON line.
	if err := appendAdoptedHistory(runDir, current); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt rollback: %v\n", err)
		return exitFailure
	}
	next := adoptedRecord{CandidateID: target.CandidateID, Prompt: target.Prompt, AdoptedAt: time.Now().UTC()}
	if err := harness.SaveJSON(filepath.Join(runDir, "adopted.json"), next); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt rollback: 写 adopted.json: %v\n", err)
		return exitFailure
	}
	res := rollbackResult{
		RunID: runID, From: current.CandidateID, To: next.CandidateID, Prompt: next.Prompt,
	}
	if o.emit != "" {
		if err := writeCandidateYAML(o.emit, next.CandidateID, next.Prompt); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt rollback: %v\n", err)
			return exitFailure
		}
		res.Emitted = o.emit
	}
	if o.headless {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			fmt.Fprintf(os.Stderr, "promptopt rollback: encode result: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "promptopt rollback: %s 采纳 %s → %s\n", runID, res.From, res.To)
	if res.Emitted != "" {
		fmt.Fprintf(os.Stderr, "已导出: %s\n", res.Emitted)
	}
	return exitOK
}

// parseRollbackFlags parses the rollback flag surface; the run id is the
// single positional argument.
func parseRollbackFlags(args []string) (rollbackOptions, string, error) {
	var o rollbackOptions
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.runsDir, "runs-dir", config.DefaultOutDir, "directory containing run artifacts")
	fs.StringVar(&o.to, "to", "", "target candidate id (frontier member or best); default rolls back to the previous different adoption, else the baseline")
	fs.StringVar(&o.emit, "emit", "", "export the rolled-back candidate to this candidate.yaml path")
	fs.BoolVar(&o.headless, "headless", false, "print only the JSON rollback result")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return o, "", err
	}
	if fs.NArg() != 1 {
		return o, "", fmt.Errorf("expected exactly one run id, got %d arguments", fs.NArg())
	}
	return o, fs.Arg(0), nil
}

// rollbackTarget resolves the rollback destination: --to wins (frontier
// member or best pick), then the most recent different adoption from
// adopted-history.jsonl (history entries carry their prompt, so legacy
// frontier artifacts without member prompts cannot break them), then
// the baseline (frontier member prompt, falling back to the synth
// spec's prompt_template through resolveBaselinePrompt).
func rollbackTarget(to string, current adoptedRecord, history []adoptedRecord, f engine.FrontierFile, synthDir string) (adoptedRecord, error) {
	if to != "" {
		prompt, ok := frontierPrompt(f, to)
		if !ok {
			return adoptedRecord{}, fmt.Errorf("--to %q 在前沿中不存在或缺少提示词（旧版产物请用 history/基线回退）", to)
		}
		return adoptedRecord{CandidateID: to, Prompt: prompt}, nil
	}
	for _, rec := range slices.Backward(history) {
		if rec.CandidateID != current.CandidateID {
			return rec, nil
		}
	}
	if id, prompt, ok := resolveBaselinePrompt(synthDir, f); ok {
		return adoptedRecord{CandidateID: id, Prompt: prompt}, nil
	}
	return adoptedRecord{}, errors.New("没有可回退的目标：history 为空且 baseline 提示词不可解析（frontier 无 baseline 成员提示词，synth spec 缺少 prompt_template）")
}

// resolveBaselinePrompt resolves the baseline candidate: the frontier
// member recorded with operator "baseline" carries the prompt on
// current artifacts; artifacts predating member prompts fall back to
// the synth spec's task.prompt_template. It is shared by verify
// (baseline side) and rollback (baseline fallback).
func resolveBaselinePrompt(synthDir string, f engine.FrontierFile) (id, prompt string, ok bool) {
	for _, m := range f.Members {
		if m.Operator == engine.OpBaseline {
			if m.Prompt != "" {
				return m.ID, m.Prompt, true
			}
			break
		}
	}
	if spec, err := harness.LoadSpec(synthDir); err == nil && spec.Task.PromptTemplate != "" {
		return engine.OpBaseline, spec.Task.PromptTemplate, true
	}
	return "", "", false
}

// readAdoptedHistory decodes adopted-history.jsonl, skipping blank and
// malformed lines — the journal is append-only and a torn tail must not
// block recovery.
func readAdoptedHistory(runDir string) []adoptedRecord {
	b, err := os.ReadFile(filepath.Join(runDir, "adopted-history.jsonl"))
	if err != nil {
		return nil
	}
	var recs []adoptedRecord
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r adoptedRecord
		if json.Unmarshal([]byte(line), &r) == nil && r.CandidateID != "" {
			recs = append(recs, r)
		}
	}
	return recs
}

// appendAdoptedHistory journals one adoption as a single JSON line.
func appendAdoptedHistory(runDir string, rec adoptedRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(runDir, "adopted-history.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// writeCandidateYAML exports the rolled-back candidate as a minimal
// candidate.yaml (LoadCandidate-compatible).
func writeCandidateYAML(path, id, prompt string) error {
	b, err := yaml.Marshal(core.Candidate{ID: id, Prompt: prompt})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

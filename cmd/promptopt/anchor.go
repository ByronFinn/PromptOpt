package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ByronFinn/PromptOpt/internal/anchors"
	"github.com/ByronFinn/PromptOpt/internal/core"
)

// anchorCommand manages the anchor library (V7 提案 §1.2): add imports
// a dataset's samples, list inspects a task's library, promote moves
// staging candidates into the confirmed library after human review.
// Exit codes: 0 done, 1 usage or IO error.
func anchorCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, anchorUsage)
		return exitFailure
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return anchorAdd(rest)
	case "list":
		return anchorList(rest)
	case "promote":
		return anchorPromote(rest)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, anchorUsage)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "promptopt anchor: unknown subcommand %q\n\n", sub)
		fmt.Fprint(os.Stderr, anchorUsage)
		return exitFailure
	}
}

const anchorUsage = `usage: promptopt anchor <subcommand> [flags]

subcommands:
  add     导入数据集样本入库：anchor add <dataset.yaml> --task-key K [--confirmed]
  list    查看任务库：anchor list --task-key K
  promote 人工确认后提升 staging → confirmed：anchor promote --task-key K [--ids id1,id2]

库布局（--anchors-dir 相对 cwd，可 git 版本化）：
  <anchors-dir>/<task-key>/samples.yaml         confirmed 正式库
  <anchors-dir>/<task-key>/staging/samples.yaml staging 候选区
`

// anchorAdd imports a dataset's samples: --confirmed writes the
// confirmed library, the default lands in staging (ADR 0001 精神：
// 正式库只收显式确认的样本). Duplicates by input hash are skipped and
// reported, never re-inserted.
func anchorAdd(args []string) int {
	fs := flag.NewFlagSet("anchor add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var o struct {
		anchorsDir, taskKey, source, originRun string
		confirmed                              bool
	}
	fs.StringVar(&o.anchorsDir, "anchors-dir", anchors.DefaultDir, "anchor library root directory (cwd-relative)")
	fs.StringVar(&o.taskKey, "task-key", "", "task key naming the library (required)")
	fs.StringVar(&o.source, "source", string(anchors.SourceManual), "entry channel: manual|verify-failure|production-trace")
	fs.StringVar(&o.originRun, "origin-run", "", "origin run id for provenance")
	fs.BoolVar(&o.confirmed, "confirmed", false, "write the confirmed library (default: staging candidate area)")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return parseExitCode(err)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "promptopt anchor add: expected exactly one dataset.yaml path, got %d arguments\n", fs.NArg())
		return exitFailure
	}
	var errs []error
	if o.taskKey == "" {
		errs = append(errs, errors.New("--task-key is required"))
	} else if err := anchors.ValidateTaskKey(o.taskKey); err != nil {
		errs = append(errs, err)
	}
	src := anchors.Source(o.source)
	if !src.Valid() {
		errs = append(errs, fmt.Errorf("--source must be manual, verify-failure or production-trace, got %q", o.source))
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor add: %v\n", err)
		return exitFailure
	}

	ds, err := core.LoadDataset(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor add: %v\n", err)
		return exitFailure
	}
	now := time.Now().UTC()
	entries := make([]anchors.Entry, 0, len(ds.Samples))
	for _, s := range ds.Samples {
		entries = append(entries, anchors.Entry{
			Sample: core.Sample{ID: s.ID, Input: s.Input, Expected: s.Expected, Split: s.Split},
			Source: src, AddedAt: now, OriginRun: o.originRun,
		})
	}
	store := anchors.NewStore(o.anchorsDir)
	added, skipped, err := store.Add(o.taskKey, o.confirmed, entries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor add: %v\n", err)
		return exitFailure
	}
	layer, path := "staging", store.StagingPath(o.taskKey)
	if o.confirmed {
		layer, path = "confirmed", store.ConfirmedPath(o.taskKey)
	}
	fmt.Fprintf(os.Stderr, "promptopt anchor add: %s 库新增 %d 条，跳过重复 %d 条（%s）\n",
		layer, added, skipped, path)
	return exitOK
}

// anchorList prints one task's library: counts first, then each entry
// with its provenance. Human-readable output stays on stderr per the
// output convention.
func anchorList(args []string) int {
	fs := flag.NewFlagSet("anchor list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	anchorsDir := fs.String("anchors-dir", anchors.DefaultDir, "anchor library root directory (cwd-relative)")
	taskKey := fs.String("task-key", "", "task key naming the library (required)")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return parseExitCode(err)
	}
	if *taskKey == "" {
		fmt.Fprintln(os.Stderr, "promptopt anchor list: --task-key is required")
		return exitFailure
	}
	if err := anchors.ValidateTaskKey(*taskKey); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor list: %v\n", err)
		return exitFailure
	}
	store := anchors.NewStore(*anchorsDir)
	confirmed, err := store.Load(*taskKey, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor list: %v\n", err)
		return exitFailure
	}
	all, err := store.Load(*taskKey, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor list: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "task %s：confirmed %d 条，staging %d 条（库根 %s）\n",
		*taskKey, len(confirmed), len(all)-len(confirmed), *anchorsDir)
	printAnchorLayer(os.Stderr, "confirmed", confirmed)
	printAnchorLayer(os.Stderr, "staging", all[len(confirmed):])
	return exitOK
}

// printAnchorLayer renders one layer's entries; the hash is shown
// truncated — the full value lives in samples.yaml.
func printAnchorLayer(w io.Writer, layer string, entries []anchors.Entry) {
	if len(entries) == 0 {
		fmt.Fprintf(w, "%s:\n  （空）\n", layer)
		return
	}
	fmt.Fprintf(w, "%s:\n", layer)
	for _, e := range entries {
		hash := e.InputHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		line := fmt.Sprintf("  - %-16s source=%s hash=%s added=%s",
			e.Sample.ID, e.Source, hash, e.AddedAt.Format("2006-01-02"))
		if e.Verdict != "" {
			line += " verdict=" + e.Verdict
		}
		if e.OriginRun != "" {
			line += " origin=" + e.OriginRun
		}
		fmt.Fprintln(w, line)
	}
}

// anchorPromote moves staging entries into the confirmed library after
// human review: all of them by default, or only --ids (comma-separated
// sample ids; an unknown id is an error, not a silent partial move).
func anchorPromote(args []string) int {
	fs := flag.NewFlagSet("anchor promote", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var o struct {
		anchorsDir, taskKey, ids string
	}
	fs.StringVar(&o.anchorsDir, "anchors-dir", anchors.DefaultDir, "anchor library root directory (cwd-relative)")
	fs.StringVar(&o.taskKey, "task-key", "", "task key naming the library (required)")
	fs.StringVar(&o.ids, "ids", "", "comma-separated sample ids to promote (default: all staging entries)")
	if err := fs.Parse(flagsFirst(fs, args)); err != nil {
		return parseExitCode(err)
	}
	if o.taskKey == "" {
		fmt.Fprintln(os.Stderr, "promptopt anchor promote: --task-key is required")
		return exitFailure
	}
	if err := anchors.ValidateTaskKey(o.taskKey); err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor promote: %v\n", err)
		return exitFailure
	}
	var ids []string
	for id := range strings.SplitSeq(o.ids, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	store := anchors.NewStore(o.anchorsDir)
	moved, err := store.Promote(o.taskKey, ids)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor promote: %v\n", err)
		return exitFailure
	}
	confirmed, err := store.Load(o.taskKey, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor promote: %v\n", err)
		return exitFailure
	}
	all, err := store.Load(o.taskKey, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promptopt anchor promote: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(os.Stderr, "promptopt anchor promote: 已提升 %d 条到 confirmed（%s），staging 剩余 %d 条\n",
		moved, store.ConfirmedPath(o.taskKey), len(all)-len(confirmed))
	return exitOK
}

package miprov2

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"

	"github.com/ByronFinn/PromptOpt/internal/core"
	"github.com/ByronFinn/PromptOpt/internal/engine"
)

// Demo-shape constants: the few-shot pool bound, the per-pair draw
// target and the per-field truncation inside the demo block.
const (
	demoPoolCap       = 8   // pool bound (train split first)
	demosPerCombo     = 2   // per-pair draw target (k = min(2, len(pool)-1))
	maxDemoFieldRunes = 600 // mirrors engine.maxSampleInputRunes
)

// demoPool returns the few-shot pool over samples: train-split samples
// first (sample order preserved), then the rest, capped at cap. Demos
// carry the expected answer — that is what makes them teach.
func demoPool(samples []core.Sample, cap int) []core.Sample {
	out := make([]core.Sample, 0, min(len(samples), cap))
	add := func(s core.Sample) {
		if len(out) < cap {
			out = append(out, s)
		}
	}
	for _, s := range samples {
		if s.Split == "train" {
			add(s)
		}
	}
	for _, s := range samples {
		if s.Split != "train" {
			add(s)
		}
	}
	return out
}

// drawDemos draws k distinct demos from pool, k capped at len(pool)-1
// so at least one pool sample stays out of the prompt. A pool of ≤1
// leaves no unbiased demo, so none are embedded and the search
// degrades to pure instruction optimization. Selected demos keep pool
// order for a deterministic composition.
func drawDemos(rng *rand.Rand, pool []core.Sample, k int) []core.Sample {
	if len(pool) <= 1 {
		return nil
	}
	n := min(k, len(pool)-1)
	idx := rng.Perm(len(pool))[:n]
	slices.Sort(idx)
	out := make([]core.Sample, n)
	for i, j := range idx {
		out[i] = pool[j]
	}
	return out
}

// composePrompt embeds the demo block before the first {input} of
// instruction, so the demos read as solved examples preceding the live
// task. Rendering is plain string replacement (core.RenderPrompt): a
// literal {input} inside demo text is stripped — it would otherwise
// capture the injection point — while JSON braces are inert.
func composePrompt(instruction string, demos []core.Sample) string {
	if len(demos) == 0 {
		return instruction
	}
	at := strings.Index(instruction, core.InputPlaceholder)
	if at < 0 {
		return instruction
	}
	return instruction[:at] + demoBlock(demos) + instruction[at:]
}

// demoBlock formats the demos as solved examples.
func demoBlock(demos []core.Sample) string {
	var b strings.Builder
	b.WriteString("\n以下是若干已解决的参考示例：\n")
	for i, s := range demos {
		fmt.Fprintf(&b, "\n示例 %d\n输入：%s\n期望输出：%s\n", i+1,
			engine.TruncateRunes(engine.CleanInputLiteral(s.Input), maxDemoFieldRunes),
			engine.TruncateRunes(engine.CleanInputLiteral(expectedText(s.Expected)), maxDemoFieldRunes))
	}
	b.WriteString("\n请参考上述示例的处理方式：\n")
	return b.String()
}

// expectedText renders a sample's expected value as demo text: strings
// pass through, anything else is JSON-encoded.
func expectedText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// minibatchFrom draws the scoring minibatch over kept minus the pair's
// demos — a demo must not grade itself (leak mitigation; the demo-free
// remainder still shares the retained set's distribution). When the
// exclusion covers everything (a single retained sample, an all-demo
// retained set) the whole retained set is the batch — never empty, no
// division by zero; k wider than the remainder returns it unchanged,
// in order.
func minibatchFrom(rng *rand.Rand, kept, demos []core.Sample, k int) []core.Sample {
	rest := make([]core.Sample, 0, len(kept))
	for _, s := range kept {
		if !slices.ContainsFunc(demos, func(d core.Sample) bool { return d.ID == s.ID }) {
			rest = append(rest, s)
		}
	}
	if len(rest) == 0 {
		return kept
	}
	if k >= len(rest) {
		return rest
	}
	perm := rng.Perm(len(rest))
	out := make([]core.Sample, k)
	for i := range k {
		out[i] = rest[perm[i]]
	}
	return out
}

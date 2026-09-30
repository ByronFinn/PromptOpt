package optimizers

import "fmt"

// Routing intents that no in-repo optimizer carries one-to-one; they
// degrade onto the paradigm implementing the same mechanism.
const (
	// IntentTextGrad: textual gradients — served by ProTeGi's
	// natural-language gradient loop.
	IntentTextGrad = "textgrad"
	// IntentP1: variance-filter-first flow — served by GEPA, which
	// already optimizes over the p¹-filtered retained set.
	IntentP1 = "p1"
)

// degradeTo maps routing intents onto the paradigm implementing them.
// Resolution follows the chain until a name has no entry, so a future
// multi-hop chain needs no new code here.
var degradeTo = map[string]string{
	IntentTextGrad: "protegi",
	IntentP1:       "gepa",
}

// TaskFeatures is the routing input the cmd layer derives from the
// task, the retained sample set and the budget flags (cmd
// routeFeatures). V5 heuristic sources, to be upgraded to task-level
// structured declarations in V6 (docs/plugins.md):
//
//   - Pipeline: staged-pipeline tasks; no V5 declaration source
//     exists, so the derived value is always false.
//   - JointFewShot: ≥2 retained train samples plus a verifiable
//     primary metric (exact_match / f1 / json_validator) — joint
//     instruction+demo search needs demo material and an automatic
//     verdict.
//   - TightBudget: an eval budget set below twice the retained set
//     (a full reflection round costs ≈2×kept evaluations; setting a
//     budget at all is not "tight").
//   - MultiConstraint: more than one declared metric.
type TaskFeatures struct {
	Pipeline        bool
	JointFewShot    bool
	TightBudget     bool
	MultiConstraint bool
}

// Decision is one routing outcome: the paradigm that will drive the
// loop, what was requested (the explicit name or the routed intent),
// a human-readable reason recorded in the manifest, and whether the
// decision degraded off the preferred paradigm.
type Decision struct {
	Paradigm  string
	Requested string
	Reason    string
	Degraded  bool
}

// Route resolves the auto-mode paradigm for f against reg. Rules apply
// in priority order — a budget too small to afford a paradigm outranks
// the data shape, which outranks multi-metric trade-offs:
//
//	① Pipeline        → textgrad (degraded onto protegi)
//	② TightBudget     → p1 (degraded onto gepa)
//	③ JointFewShot    → miprov2
//	④ MultiConstraint → gepa
//	⑤ default         → protegi
//
// The resolved paradigm is then checked against reg: when it is not
// registered (an in-progress paradigm not yet wired into builtin), the
// decision degrades onto gepa — auto must degrade gracefully, never
// fail — and the reason records the fallback. With no gepa registered
// either, the decision passes through unchanged and the caller's
// Build reports the options.
func Route(reg *Registry, f TaskFeatures) Decision {
	requested, reason := routeIntent(f)
	return available(reg, resolve(requested, reason))
}

// routeIntent applies the five-rule table.
func routeIntent(f TaskFeatures) (requested, reason string) {
	switch {
	case f.Pipeline:
		return IntentTextGrad, "任务呈流水线特征：请求 textgrad（文本梯度）范式"
	case f.TightBudget:
		return IntentP1, "评估预算低于保留集两倍：请求 p1（方差过滤优先）范式"
	case f.JointFewShot:
		return "miprov2", "保留集含 ≥2 条 train 样本且主指标可判别：适合指令+示范联合搜索"
	case f.MultiConstraint:
		return "gepa", "多指标任务：GEPA 帕累托前沿原生权衡多目标"
	default:
		return "protegi", "默认路由：ProTeGi 文本梯度适合通用提示词改进"
	}
}

// resolve follows the degradation chain from the requested intent to
// the paradigm implementing it.
func resolve(requested, reason string) Decision {
	paradigm := requested
	for {
		next, ok := degradeTo[paradigm]
		if !ok {
			break
		}
		paradigm = next
	}
	d := Decision{Paradigm: paradigm, Requested: requested, Reason: reason}
	if d.Degraded = paradigm != requested; d.Degraded {
		d.Reason = fmt.Sprintf("%s，由 %s 等价实现（已降级）", reason, paradigm)
	}
	return d
}

// available degrades an unregistered paradigm onto gepa.
func available(reg *Registry, d Decision) Decision {
	if _, ok := reg.Get(d.Paradigm); ok {
		return d
	}
	if d.Paradigm == "gepa" {
		return d
	}
	if _, ok := reg.Get("gepa"); !ok {
		return d
	}
	d.Paradigm = "gepa"
	d.Degraded = true
	d.Reason += "；该范式尚未注册（builtin 清单见 docs/plugins.md），已回退 gepa"
	return d
}

package optimizers

// TaskFeatures is the routing input the cmd layer derives from the
// task, the retained sample set and the budget flags (cmd
// routeFeatures). V5 heuristic sources, to be upgraded to task-level
// structured declarations (docs/plugins.md):
//
//   - JointFewShot: ≥2 retained train samples plus a verifiable
//     primary metric (exact_match / f1 / json_validator) — joint
//     instruction+demo search needs demo material and an automatic
//     verdict.
//   - TightBudget: an eval budget that cannot afford GEPA's minimal
//     effective run (≈ (2+1+r_min)×kept evaluations, r_min = 2) —
//     routing GEPA there would send the loop to starve, so it goes to
//     p1 instead (提案 §3.1 ②).
//   - MultiConstraint: more than one declared metric.
type TaskFeatures struct {
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
//	① TightBudget     → p1
//	② JointFewShot    → miprov2
//	③ MultiConstraint → gepa
//	④ default         → protegi
//
// Every intent names a registered paradigm directly (the textgrad
// intent, its Pipeline feature and the degradeTo chain were removed
// in V7, 提案 §3.2). The resolved paradigm is then checked against
// reg: when it is not registered (an in-progress paradigm not yet
// wired into builtin), the decision degrades onto gepa — auto must
// degrade gracefully, never fail — and the reason records the
// fallback. With no gepa registered either, the decision passes
// through unchanged and the caller's Build reports the options.
func Route(reg *Registry, f TaskFeatures) Decision {
	requested, reason := routeIntent(f)
	d := Decision{Paradigm: requested, Requested: requested, Reason: reason}
	return available(reg, d)
}

// routeIntent applies the four-rule table.
func routeIntent(f TaskFeatures) (requested, reason string) {
	switch {
	case f.TightBudget:
		return "p1", "评估预算付不起 GEPA 最小有效轮次：路由 p1（p¹ 式预算分配，评估集中在最小辨识集）"
	case f.JointFewShot:
		return "miprov2", "保留集含 ≥2 条 train 样本且主指标可判别：适合指令+示范联合搜索"
	case f.MultiConstraint:
		return "gepa", "多指标任务：GEPA 帕累托前沿原生权衡多目标"
	default:
		return "protegi", "默认路由：ProTeGi 文本梯度适合通用提示词改进"
	}
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

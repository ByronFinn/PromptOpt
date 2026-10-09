package main

import (
	"math/rand/v2"
	"testing"
)

// rowsSide builds a verifySide carrying a fixed-order per-sample row;
// Means mirrors the row mean like the engine result does (the legacy
// rule reads Means, the CI rule reads Rows).
func rowsSide(rows ...float64) verifySide {
	mean := 0.0
	for _, v := range rows {
		mean += v
	}
	mean /= float64(len(rows))
	return verifySide{
		Rows:  rows,
		Reps:  1,
		Means: map[string]float64{"exact_match": mean},
	}
}

// TestVerdictBootstrapRules（用例 ⑥）pins the CI gate: 差分正负各半 →
// CI 跨 0 → inconclusive 且退出码 0；全量退化 → CI 下界超阈值 → 回归
// 退出码 3；全量改进 → CI 上界 ≤ 0 → 置信通过退出码 0；--bootstrap 0
// 回退旧均值差规则且不产 CI 字段。
func TestVerdictBootstrapRules(t *testing.T) {
	metrics := []string{"exact_match"}
	opts := verifyOptions{maxRegression: 0.05, bootstrap: 1000}

	t.Run("mixed diffs are inconclusive and exit 0", func(t *testing.T) {
		reg, _, code := verdict("exact_match", rowsSide(1, 0), rowsSide(0, 1), opts, metrics)
		if code != exitOK {
			t.Errorf("exit = %d, want 0 (inconclusive keeps the gate actionable)", code)
		}
		if reg.Regressed {
			t.Errorf("regressed = %v, want false", reg.Regressed)
		}
		if reg.CI == nil || reg.CI.Verdict != ciInconclusive {
			t.Errorf("ci = %+v, want verdict inconclusive", reg.CI)
		}
		if reg.CI != nil && (reg.CI.Lo > 0.05 || reg.CI.Hi <= 0) {
			t.Errorf("ci bounds = [%v, %v], want lo ≤ threshold and hi > 0", reg.CI.Lo, reg.CI.Hi)
		}
	})

	t.Run("uniform regression exits 3", func(t *testing.T) {
		reg, _, code := verdict("exact_match", rowsSide(1, 1), rowsSide(0, 0), opts, metrics)
		if code != exitRegression {
			t.Errorf("exit = %d, want 3", code)
		}
		if !reg.Regressed || reg.CI == nil || reg.CI.Verdict != ciRegressed {
			t.Errorf("regression = %+v, want regressed via CI lower bound", reg)
		}
	})

	t.Run("uniform improvement is a confident pass", func(t *testing.T) {
		reg, _, code := verdict("exact_match", rowsSide(0, 0), rowsSide(1, 1), opts, metrics)
		if code != exitOK {
			t.Errorf("exit = %d, want 0", code)
		}
		if reg.Regressed || reg.CI == nil || reg.CI.Verdict != ciConfidentPass {
			t.Errorf("regression = %+v, want confident_pass (CI upper bound ≤ 0)", reg)
		}
	})

	t.Run("bootstrap 0 falls back to the legacy mean-diff rule", func(t *testing.T) {
		reg, _, code := verdict("exact_match", rowsSide(1, 1), rowsSide(0.9, 0.9),
			verifyOptions{maxRegression: 0.05, bootstrap: 0}, metrics)
		if code != exitRegression {
			t.Errorf("exit = %d, want 3 (Δ=0.2 > 0.05)", code)
		}
		if reg.CI != nil {
			t.Errorf("ci = %+v, want unset on the legacy path", reg.CI)
		}
		if !reg.Regressed {
			t.Error("legacy rule must still flag the regression")
		}
	})

	t.Run("constraint checks stay deterministic beside the CI", func(t *testing.T) {
		// CI 说不回归，但 json 合法率 0.5 仍硬违反 → 退出码 3。
		base := rowsSide(1, 1)
		deliv := rowsSide(1, 1)
		deliv.Means = map[string]float64{"exact_match": 1, "json_validator": 0.5}
		_, con, code := verdict("exact_match", base, deliv, opts, []string{"json_validator", "exact_match"})
		if code != exitRegression || !con.JSONViolated {
			t.Errorf("exit = %d json violated = %v, want the hard constraint firing", code, con.JSONViolated)
		}
	})
}

// TestBootstrapReproducible pins the fixed-seed contract (now through
// the internal/stats delegation): the same rows produce byte-identical
// bounds across calls (verify.json 可复现), and the mixed-diff case
// bounds the true mean difference.
func TestBootstrapReproducible(t *testing.T) {
	base := []float64{1, 0}
	deliv := []float64{0, 1}
	lo1, hi1 := pairedBootstrapCI(base, deliv, 1000, newBootstrapRNG())
	lo2, hi2 := pairedBootstrapCI(base, deliv, 1000, newBootstrapRNG())
	if lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("CI diverged across same-seed runs: [%v,%v] vs [%v,%v]", lo1, hi1, lo2, hi2)
	}
	// D = [1,-1]：CI 必须跨 0（lo = -1、hi = 1 的百分位）。
	if lo1 != -1 || hi1 != 1 {
		t.Errorf("CI = [%v, %v], want the exact [-1, 1] percentile bounds", lo1, hi1)
	}

	// 常量差分：CI 收敛到该常量（0.75 可精确表示，差分无浮点噪声）。
	lo3, hi3 := pairedBootstrapCI([]float64{1, 1}, []float64{0.75, 0.75}, 500, rand.New(rand.NewPCG(1, 2)))
	if lo3 != 0.25 || hi3 != 0.25 {
		t.Errorf("constant-diff CI = [%v, %v], want [0.25, 0.25]", lo3, hi3)
	}

	if got := bootstrapVerdict(0.06, 0.4, 0.05); got != ciRegressed {
		t.Errorf("verdict(lo>thr) = %s", got)
	}
	if got := bootstrapVerdict(-0.4, 0, 0.05); got != ciConfidentPass {
		t.Errorf("verdict(hi==0) = %s", got)
	}
	if got := bootstrapVerdict(-0.1, 0.02, 0.05); got != ciInconclusive {
		t.Errorf("verdict(straddling) = %s", got)
	}
}

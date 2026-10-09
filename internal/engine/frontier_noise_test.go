package engine

import (
	"slices"
	"testing"
)

// --- DominatesWithMargin truth table（用例 ③） -------------------------------

func TestDominatesWithMarginTruthTable(t *testing.T) {
	cases := []struct {
		name         string
		a, b, eps    []float64
		want         bool
	}{
		{"advantage beyond margin", []float64{0.5}, []float64{0.3}, []float64{0.1}, true},
		{"advantage within margin", []float64{0.6}, []float64{0.55}, []float64{0.1}, false},
		{"exactly at margin is not strictly better", []float64{0.4}, []float64{0.3}, []float64{0.1}, false}, // a == b+eps：不劣但不严格优于余量后
		{"one losing dim vetoes", []float64{0.5, 0.9}, []float64{0.3, 0.95}, []float64{0.1, 0}, false},
		{"zero margins reduce to Dominates", []float64{0.5, 0.2}, []float64{0.5, 0.1}, []float64{0, 0}, true},
		{"unequal lengths never dominate", []float64{1, 1}, []float64{1}, []float64{0}, false},
		{"eps length mismatch never dominates", []float64{1, 1}, []float64{0, 0}, []float64{0}, false},
		{"empty vectors", nil, nil, nil, false},
	}
	for _, tc := range cases {
		if got := DominatesWithMargin(tc.a, tc.b, tc.eps); got != tc.want {
			t.Errorf("%s: DominatesWithMargin(%v, %v, %v) = %v, want %v", tc.name, tc.a, tc.b, tc.eps, got, tc.want)
		}
	}
}

// --- AddNoiseAware（用例 ③） -------------------------------------------------

func TestAddNoiseAwareEpsCloneRejection(t *testing.T) {
	// a=[0.6] vs b=[0.55]、ε=0.1：优势 0.05 落在噪声带内 → ε-clone 拒绝。
	f := &Frontier{}
	f.Add(member("cur", []float64{0.6}, nil))
	ok, evicted := f.AddNoiseAware(member("new", []float64{0.55}, nil), func(cur Member) []float64 {
		return []float64{0.1}
	})
	if ok || evicted != nil {
		t.Errorf("ε-clone admission = %v evicted = %v, want rejection", ok, evicted)
	}
	if f.Size() != 1 || f.Members()[0].ID() != "cur" {
		t.Errorf("frontier = %v, want the incumbent kept", f.Members())
	}
}

func TestAddNoiseAwareMarginDominationRejection(t *testing.T) {
	// 现有成员按余量支配新成员：cur 每维至少 new+ε（0.9 ≥ 0.7+0.1、
	// 0.9 ≥ 0.6+0.1），且差值超 ε 非克隆 → 余量支配拒绝。
	f := &Frontier{}
	f.Add(member("cur", []float64{0.9, 0.9}, nil))
	ok, _ := f.AddNoiseAware(member("new", []float64{0.7, 0.6}, nil), func(Member) []float64 {
		return []float64{0.1, 0.1}
	})
	if ok {
		t.Error("margin-domination must reject admission")
	}
}

func TestAddNoiseAwareEvictionBeyondMargin(t *testing.T) {
	// 淘汰=新成员按余量支配旧成员：new 全维超过 cur+ε → 准入并淘汰。
	f := &Frontier{}
	f.Add(member("cur", []float64{0.3, 0.3}, nil))
	ok, evicted := f.AddNoiseAware(member("new", []float64{0.6, 0.6}, nil), func(Member) []float64 {
		return []float64{0.1, 0.1}
	})
	if !ok || !slices.Equal(evicted, []string{"cur"}) {
		t.Errorf("admitted = %v evicted = %v, want true/[cur]", ok, evicted)
	}
	// 余量内的差异不分方向都是 ε-clone（§1.1：clone 判定 =
	// max|a_i−b_i| ≤ ε，方向对称）：cur 被 new 以 0.05/维 领先（ε=0.1）
	// 落在噪声带内 → 拒绝准入且不淘汰，噪声带内的残差不得决定准入。
	f2 := &Frontier{}
	f2.Add(member("cur", []float64{0.3, 0.3}, nil))
	ok, evicted = f2.AddNoiseAware(member("new", []float64{0.35, 0.35}, nil), func(Member) []float64 {
		return []float64{0.1, 0.1}
	})
	if ok || evicted != nil {
		t.Errorf("ε-clone newcomer must be rejected without eviction, got %v/%v", ok, evicted)
	}
}

// TestAddNoiseAwareNilMarginEqualsAdd pins the default-path contract:
// marginOf 返回 nil 时与 Add 逐语义等价（准入、克隆拒绝、淘汰），默认路径
// 行为不变。
func TestAddNoiseAwareNilMarginEqualsAdd(t *testing.T) {
	scenarios := []struct {
		name     string
		members  []Member
		newcomer Member
	}{
		{"clone rejected", []Member{member("a", []float64{1, 0}, nil)}, member("b", []float64{1, 0}, nil)},
		{"dominated rejected", []Member{member("a", []float64{1, 1}, nil)}, member("b", []float64{0.9, 0.9}, nil)},
		{"evicts dominated", []Member{member("a", []float64{0, 0}, nil)}, member("b", []float64{1, 1}, nil)},
		{"coexists", []Member{member("a", []float64{1, 0}, nil)}, member("b", []float64{0, 1}, nil)},
	}
	for _, sc := range scenarios {
		fAdd := &Frontier{}
		for _, m := range sc.members {
			fAdd.Add(m)
		}
		okAdd, evAdd := fAdd.Add(sc.newcomer)

		fNoise := &Frontier{}
		for _, m := range sc.members {
			fNoise.Add(m)
		}
		okNoise, evNoise := fNoise.AddNoiseAware(sc.newcomer, func(Member) []float64 { return nil })

		if okAdd != okNoise || !slices.Equal(evAdd, evNoise) {
			t.Errorf("%s: Add(%v/%v) vs AddNoiseAware-nil(%v/%v) diverge", sc.name, okAdd, evAdd, okNoise, evNoise)
		}
	}
}

// TestAddNoiseAwareMixedPairDegradesToAdd（用例 ⑤ 的 frontier 层锚定）：
// 单侧有 ε（异常工件形态）→ 按 Add 语义比较，不 panic、不误杀。
// 语义裁决（roadmap-v7-proposal §1.1 spec 优先）：「不误杀」指不引入余量
// 保护造成的误判，而非豁免支配拒绝——Add 语义下 [0.55] 本就被 [0.6]
// 严格支配，退化路径同样必须拒绝准入。
func TestAddNoiseAwareMixedPairDegradesToAdd(t *testing.T) {
	f := &Frontier{}
	// cur 自带 SD，但 marginOf 对该成员返回 nil（门控的"双侧才保护"约定）。
	cur := member("cur", []float64{0.6}, nil)
	cur.SD = []float64{0.5}
	f.Add(cur)
	ok, _ := f.AddNoiseAware(member("new", []float64{0.55}, nil), func(Member) []float64 {
		return nil
	})
	if ok {
		t.Error("mixed pair must degrade to Add semantics: the strictly dominated newcomer must be rejected")
	}
}

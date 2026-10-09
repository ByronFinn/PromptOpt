// Package stats is the shared statistical machinery of the two
// paired-bootstrap consumers: the verify regression gate (V7 提案
// §1.1 C 层) and the cross-run candidate pool's degradation warning
// (§3.3, P8 最小版). One implementation keeps one statistical caliber —
// verify 门禁与候选池预警的 CI 数字逐字节可比，两个消费方不会漂移出
// 各自的「噪声区间」定义。Pure functions only; the gate logic that
// maps a verdict onto an exit code lives with the consumers.
package stats

import (
	"math/rand/v2"
	"slices"
)

// CI verdicts recorded by both consumers (verify.json's
// regression.ci.verdict and the pool's degradation warning).
const (
	CIRegressed     = "regressed"      // CI lower bound beyond the threshold
	CIConfidentPass = "confident_pass" // CI upper bound ≤ 0
	CIInconclusive  = "inconclusive"   // between: signal not separable from noise
)

// Fixed PCG seeds for the bootstrap stream: the CI must be
// byte-reproducible from the same rows (same input → same verify.json /
// same pool warning), matching the run layer's --seed reproducibility
// stance. Randomness here is only a resampling device, not a source of
// information.
const (
	bootstrapPCGSeedA = 0x9E3779B97F4A7C15
	bootstrapPCGSeedB = 0xBF58476D1CE4E5B9
)

// NewBootstrapRNG returns the fixed-seed bootstrap stream.
func NewBootstrapRNG() *rand.Rand {
	return rand.New(rand.NewPCG(bootstrapPCGSeedA, bootstrapPCGSeedB))
}

// PairedBootstrapCI computes the percentile confidence interval of the
// paired mean difference over b resamples. Pairing comes free: both
// consumers evaluate their two sides over the same samples in the same
// fixed order, so D_i = base_i − deliv_i cancels per-sample difficulty
// and the CI captures the sampling noise of the mean difference
// itself. The two bounds are the 2.5% / 97.5% percentiles of the b
// resampled means.
func PairedBootstrapCI(base, deliv []float64, b int, rng *rand.Rand) (lo, hi float64) {
	n := len(base)
	diffs := make([]float64, n)
	for i := range diffs {
		diffs[i] = base[i] - deliv[i]
	}
	means := make([]float64, max(b, 1))
	for r := range means {
		sum := 0.0
		for range n {
			sum += diffs[rng.IntN(n)]
		}
		means[r] = sum / float64(n)
	}
	slices.Sort(means)
	return Percentile(means, 0.025), Percentile(means, 0.975)
}

// Percentile linearly interpolates the q-quantile of a sorted slice.
func Percentile(sorted []float64, q float64) float64 {
	switch n := len(sorted); {
	case n == 0:
		return 0
	case n == 1:
		return sorted[0]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(pos)
	hiIdx := min(lo+1, len(sorted)-1)
	frac := pos - float64(lo)
	return sorted[lo] + frac*(sorted[hiIdx]-sorted[lo])
}

// BootstrapVerdict maps the CI onto a gate: regression requires the
// whole interval beyond maxRegression ("退化超过噪声区间"), a
// confident pass requires the whole interval at or below zero;
// everything between is inconclusive — the honest "样本量不足" answer
// that keeps gates actionable (verify exits 0 with the reports
// flagging it; the pool stays quiet).
func BootstrapVerdict(lo, hi, maxRegression float64) string {
	switch {
	case lo > maxRegression:
		return CIRegressed
	case hi <= 0:
		return CIConfidentPass
	default:
		return CIInconclusive
	}
}

// Statistical gate helpers of verify: thin aliases of internal/stats —
// the paired-bootstrap implementation was sunk there (P8) so verify and
// the cross-run candidate pool share one statistical caliber. The gate
// logic lives in verdict(), the numbers come from the stats package.
package main

import (
	"math/rand/v2"

	"github.com/ByronFinn/PromptOpt/internal/stats"
)

// CI verdicts recorded in verify.json's regression.ci.verdict.
const (
	ciRegressed     = stats.CIRegressed
	ciConfidentPass = stats.CIConfidentPass
	ciInconclusive  = stats.CIInconclusive
)

// newBootstrapRNG returns the fixed-seed bootstrap stream.
func newBootstrapRNG() *rand.Rand { return stats.NewBootstrapRNG() }

// pairedBootstrapCI computes the percentile confidence interval of the
// paired mean difference over b resamples (see stats.PairedBootstrapCI).
func pairedBootstrapCI(base, deliv []float64, b int, rng *rand.Rand) (lo, hi float64) {
	return stats.PairedBootstrapCI(base, deliv, b, rng)
}

// bootstrapVerdict maps the CI onto the gate (see stats.BootstrapVerdict).
func bootstrapVerdict(lo, hi, maxRegression float64) string {
	return stats.BootstrapVerdict(lo, hi, maxRegression)
}

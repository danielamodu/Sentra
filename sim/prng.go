package sim

// prng.go provides the single deterministic randomness source used by M2
// experiment machinery (workload jitter, deadline sampling, future fault
// strategies). There is deliberately no use of package math/rand and no
// wall-clock seeding: every consumer passes an explicit (seed, stream) pair,
// so identical inputs reproduce identical traces by construction
// (docs/ENGINE_SEMANTICS.md — determinism guarantees).
//
// The generator is splitmix64. Correlation between nearby seeds is
// negligible for the sample rates used here, and modulo reduction (Intn)
// has the standard negligible low-order bias; both are constant across runs
// and platforms, which is the property experiments require.

import "sentra/domain"

// PRNG is a deterministic integer generator.
type PRNG struct {
	s uint64
}

// goldenRatio is an odd constant with near-maximal bit diffusion.
const goldenRatio = 0x9E3779B97F4A7C15

// NewPRNG derives a generator state from (seed, stream). Distinct streams
// give independent sequences under one seed (workload components do not
// share a random walk).
func NewPRNG(seed, stream uint64) *PRNG {
	z := seed ^ (stream * 0xBF58476D1CE4E5B9)
	if z == 0 {
		z = 1 // never all-zero: the increment-only state would then be dead
	}
	return &PRNG{s: z}
}

// Next returns the next 64-bit value and advances the state.
func (r *PRNG) Next() uint64 {
	r.s += goldenRatio
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Intn returns a value in [0, n). Panics on n <= 0.
func (r *PRNG) Intn(n int) int {
	if n <= 0 {
		panic("sim: PRNG.Intn with non-positive bound")
	}
	return int(r.Next() % uint64(n))
}

// Span returns a value in [0, span) as a SimDuration (sampled instant inside
// a window, or a deadline inside a deadline range).
func (r *PRNG) Span(span domain.SimDuration) domain.SimDuration {
	if span <= 0 {
		return 0
	}
	return domain.SimDuration(r.Next() % uint64(span))
}

// Choose returns true with probability p (clamped to [0,1]). Used only where
// an explicit Bernoulli is needed; spacing jitter otherwise.
func (r *PRNG) Choose(p float64) bool {
	if p >= 1 {
		return true
	}
	if p <= 0 {
		return false
	}
	return float64(r.Next()>>11)*(1.0/9007199254740992.0) < p // 53-bit uniform
}

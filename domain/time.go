// Package domain holds Sentra's foundational, strongly typed domain model:
// logical requests, physical attempts, tenants, upstream dependencies, workers,
// pools, queue entries, retry budgets and circuit breakers.
//
// The package is deliberately dependency-free and side-effect free. It owns the
// simulated-time primitives (SimTime, SimDuration) so that every other package
// (engine, sim, and any future independent simulator) shares one explicit
// representation of simulated time.
package domain

import (
	"errors"
	"fmt"
)

// ErrTimeOverflow is a sentinel error: checked simulated-time arithmetic
// refused to produce a time point outside the int64 range. Callers must
// distinguish it from other failures (see SimTime.AddChecked).
var ErrTimeOverflow = errors.New("simulated time overflow: result outside int64 nanosecond range")

// SimTime is a point in simulated time, measured in simulated nanoseconds.
//
// One tick equals one simulated nanosecond. SimTime is never derived from the
// wall clock; the deterministic engine is the only producer of simulated time.
type SimTime int64

// SimDuration is a span of simulated time, measured in simulated nanoseconds.
//
// The separate type makes it impossible to accidentally mix a time point and a
// duration, or to leak a wall-clock time.Duration into simulation arithmetic.
type SimDuration int64

const (
	Nanosecond  SimDuration = 1
	Microsecond SimDuration = 1_000
	Millisecond SimDuration = 1_000_000
	Second      SimDuration = 1_000_000_000
)

// Add returns t+d. Overflow behaviour follows int64 arithmetic; scenarios are
// expected to stay far from the int64 boundaries. For boundary-adjacent
// scenarios use AddChecked and decide explicitly.
func (t SimTime) Add(d SimDuration) SimTime { return t + SimTime(d) }

// AddChecked returns t+d and reports whether the addition stayed inside the
// int64 nanosecond range. On overflow it returns (0, false); it never wraps.
// This is the mechanism named in docs/ASSUMPTIONS.md for scheduling far-future
// events (timeouts, completions, backoffs, cooldowns) so that a boundary-heavy
// scenario fails with a distinct, testable error instead of silent wrap-around.
func (t SimTime) AddChecked(d SimDuration) (SimTime, bool) {
	// Assert d >= 0 — the sim schedules into the future only. A negative d is
	// treated like any addition: check via subtraction identity.
	if d >= 0 {
		sum := t + SimTime(d)
		if sum < t { // wrapped past MaxInt64
			return 0, false
		}
		return sum, true
	}
	sum := t + SimTime(d)
	if sum > t { // wrapped past MinInt64
		return 0, false
	}
	return sum, true
}

// SubtractChecked returns t-o and reports whether the subtraction stayed
// inside the int64 range (kept for symmetry with AddChecked).
func (t SimTime) SubtractChecked(o SimTime) (SimDuration, bool) {
	diff := t - o
	if (o >= 0 && diff > t) || (o < 0 && diff < t) {
		return 0, false
	}
	return SimDuration(diff), true
}

// Sub returns t-o as a duration.
func (t SimTime) Sub(o SimTime) SimDuration { return SimDuration(t - o) }

func (t SimTime) Before(o SimTime) bool     { return t < o }
func (t SimTime) After(o SimTime) bool      { return t > o }
func (t SimTime) OnOrBefore(o SimTime) bool { return t <= o }
func (t SimTime) OnOrAfter(o SimTime) bool  { return t >= o }

func (t SimTime) String() string     { return fmt.Sprintf("%dns", int64(t)) }
func (d SimDuration) String() string { return fmt.Sprintf("%dns", int64(d)) }

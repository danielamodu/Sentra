package domain

import "fmt"

// BreakerState is the classic three-state circuit breaker lifecycle.
type BreakerState int

const (
	// BreakerClosed: normal operation; failures are counted.
	BreakerClosed BreakerState = iota
	// BreakerOpen: new work against the upstream is shed until the cooldown
	// elapses; in-flight attempts are unaffected.
	BreakerOpen
	// BreakerHalfOpen: limited probe work is admitted to test recovery.
	BreakerHalfOpen
)

var breakerStateNames = map[BreakerState]string{
	BreakerClosed:   "closed",
	BreakerOpen:     "open",
	BreakerHalfOpen: "half-open",
}

func (s BreakerState) String() string {
	if n, ok := breakerStateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("BreakerState(%d)", int(s))
}

// CircuitBreaker is the per-upstream breaker state machine. Transitions are
// applied by the reference system (package sim) as typed
// BreakerTransitioned events; this type only carries state.
//
// INDEPENDENCE RISK — tier 3 (mechanism-level contract, see
// docs/ARCHITECTURE.md §3): the breaker lifecycle semantics here (which states
// interact, what failures mean) are shared deliberately so the two systems can
// be compared; the TRANSITION MATRIX itself (failure threshold, cooldown,
// probe policy) is decision-bearing policy. An independent validator must NOT
// call sim's transition implementation; it must re-derive transitions from
// this state carrier and its own matrix implementation.
type CircuitBreaker struct {
	Upstream            UpstreamID
	State               BreakerState
	ConsecutiveFailures int
	OpenedAt            SimTime
	// ProbesActive counts probe permits currently reserved by admitted
	// requests while the breaker is half-open.
	ProbesActive int
}

// NewCircuitBreaker creates a breaker in the closed state.
func NewCircuitBreaker(up UpstreamID) *CircuitBreaker {
	return &CircuitBreaker{Upstream: up, State: BreakerClosed}
}

// RetryBudget is a per-(tenant, upstream) integer token bucket regulating how
// often a tenant may retry against an upstream. It exists to bound the retry
// amplification feedback loop that participates in metastable failure.
//
// All arithmetic is integer-only over simulated nanoseconds, so behaviour is
// exactly reproducible.
//
// INDEPENDENCE RISK — tier 3 (see docs/ARCHITECTURE.md §3): refill /
// consumption is the retry-policy decision itself. sim's handlers must call
// this method (single source of truth for the reference system), but an
// independent validator that is *checking* retry policy must re-implement the
// bucket from the algorithmic description, not delegate to it.
type RetryBudget struct {
	Key            BudgetKey
	Capacity       int
	Tokens         int
	RefillInterval SimDuration
	LastRefill     SimTime
}

// NewRetryBudget creates a full bucket. A non-positive interval means the
// bucket never refills.
func NewRetryBudget(k BudgetKey, capacity int, interval SimDuration) *RetryBudget {
	return &RetryBudget{Key: k, Capacity: capacity, Tokens: capacity, RefillInterval: interval}
}

// refill lazily advances the bucket to simulated time t. Full intervals convert
// into tokens; the remainder is preserved by advancing LastRefill by exactly
// gain*interval, so fractional progress is kept without floating point.
func (b *RetryBudget) refill(t SimTime) {
	if b.RefillInterval <= 0 || t <= b.LastRefill {
		return
	}
	gain := int64(t.Sub(b.LastRefill)) / int64(b.RefillInterval)
	if gain <= 0 {
		return
	}
	b.Tokens += int(gain)
	if b.Tokens > b.Capacity {
		b.Tokens = b.Capacity
	}
	b.LastRefill += SimTime(SimDuration(gain) * b.RefillInterval)
}

// TryConsume spends one token at simulated time t if one is available.
// It reports false and leaves state unchanged when the bucket is empty.
func (b *RetryBudget) TryConsume(t SimTime) bool {
	b.refill(t)
	if b.Tokens <= 0 {
		return false
	}
	b.Tokens--
	return true
}

// Available reports the refillable token count at time t without consuming.
func (b *RetryBudget) Available(t SimTime) int {
	b.refill(t)
	return b.Tokens
}

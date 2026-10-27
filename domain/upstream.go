package domain

import "fmt"

// Criticality distinguishes the two reference dependency classes: U1 is
// critical (degradation is a severe condition) and U2 is standard. It is
// metadata for later prioritization milestones; M1 treats both classes
// identically at runtime.
type Criticality int

const (
	CriticalityStandard Criticality = iota
	CriticalityCritical
)

var criticalityNames = map[Criticality]string{
	CriticalityStandard: "standard",
	CriticalityCritical: "critical",
}

func (c Criticality) String() string {
	if n, ok := criticalityNames[c]; ok {
		return n
	}
	return fmt.Sprintf("Criticality(%d)", int(c))
}

// Upstream is an upstream dependency with a latency configuration and an
// availability flag.
//
// M1 failure model (deterministic stand-in for a stochastic one):
//
//   - an attempt's execution duration is fixed at actual execution start:
//     NormalLatency when the upstream is healthy at that instant,
//     DegradedLatency when it is degraded;
//   - an attempt's result is a failure iff the upstream is still degraded at
//     the completion instant (an upstream that recovered in between yields a
//     successful, slow completion).
//
// This is explicit, reproducible, and sufficient to exercise breaker, retry and
// admission behaviour. A seeded stochastic execution model is a later-milestone
// replacement that must preserve determinism.
type Upstream struct {
	ID              UpstreamID
	Criticality     Criticality
	NormalLatency   SimDuration
	DegradedLatency SimDuration
	Degraded        bool
}

// ExecutionLatency returns the latency applied to attempts starting now.
//
// INDEPENDENCE RISK — tier 3 (see docs/ARCHITECTURE.md §3): this selector IS
// the failure model decision. sim's execution start / connection grant call it
// as the single source of truth; independent validators must re-derive the
// latency from Upstream fields and their own selection logic.
func (u *Upstream) ExecutionLatency() SimDuration {
	if u.Degraded {
		return u.DegradedLatency
	}
	return u.NormalLatency
}

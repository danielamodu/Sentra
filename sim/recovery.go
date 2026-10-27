package sim

// recovery.go implements the M2 recovery analysis (workstream 8).
//
// The analysis is POST-HOC and NON-MUTATING. When an upstream recovers, the
// system keeps every piece of internal state it accumulated during the
// degradation window: queues hold backlog, pinned waiters stay pinned, occupied
// workers keep executing, retry budgets and breakers remain where the causally
// correct transitions left them, and open requests stay open. No code path in
// the reference system resets queues, workers, budgets, breakers or request
// state to manufacture a recovery; the analysis never needs to — every phase
// state here is computed from the final SystemState, the occupancy timelines
// (occupancy.go) and the in-order record ledger.
//
// Phase decomposition around one degradation episode [DegFrom, RecAt):
//
//	pre       [0, DegFrom)               state before degradation
//	during    [DegFrom, RecAt)           degradation window
//	immediate [RecAt, RecAt+DrainSpan)   system right after recovery
//	drain     [RecAt, DrainAt)           backlog drainage interval
//	post      [DrainAt, Horizon)         after recovery (or continued collapse)
//
// Classification (deterministic; thresholds are PARAMETERS recorded in the
// report, and all raw measured numbers are recorded alongside — workstream 10):
//
//   - immediate: recovered in the immediate window AND no backlog and no
//     in-flight work at RecAt (there was nothing to drain).
//   - delayed-but-eventual: backlog/in-flight work existed at RecAt, the
//     system fully does NOT drain instantly but drains within the horizon
//     and the post-drain goodput rate meets the threshold.
//   - sustained-collapse-within-horizon: the post-window goodput rate fails
//     the threshold up to the horizon. The class name carries the horizon
//     qualifier: sustained collapse is a claim about THIS horizon, never an
//     unbounded one.
//
// Goodput rate means DeadlineRespecting completions per unit of simulated
// time, quantized to milliseconds; the pre-degradation phase is the healthy
// baseline. An empty pre-phase baseline (degradation starting at time 0)
// classifies as no-baseline — scenarios must include a healthy prefix.

import (
	"sort"

	"sentra/domain"
)

// RecoveryClass is the behavioural class of one post-recovery observation.
type RecoveryClass string

const (
	RecoveryImmediate          RecoveryClass = "immediate"
	RecoveryDelayedButEventual RecoveryClass = "delayed-but-eventual"
	RecoverySustainedCollapse  RecoveryClass = "sustained-collapse-within-horizon"
	RecoveryNoBaseline         RecoveryClass = "no-baseline"
	RecoveryNoPostWindow       RecoveryClass = "no-post-window"
)

// RecoveryPolicy fixes the classifier thresholds. They are experiment inputs,
// recorded next to the measured results so the classification can never
// silently encode a desired outcome.
type RecoveryPolicy struct {
	// DrainSpan is the length of the immediate-after observation window.
	DrainSpan domain.SimDuration `json:"drain_span_ns"`
	// GoodputThreshold: the post-recovery goodput rate must reach at least
	// this fraction of the pre-degradation rate to count as recovered.
	GoodputThreshold float64 `json:"goodput_threshold"`
}

// DefaultRecoveryPolicy: recovery means regaining >= 50% of the healthy
// goodput rate, observed over a 10ms window after the upstream recovered.
func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{DrainSpan: 10 * domain.Millisecond, GoodputThreshold: 0.5}
}

// RecoveryReport is one degradation episode's complete recovery analysis.
type RecoveryReport struct {
	DegFrom domain.SimTime `json:"deg_from_ns"`
	RecAt   domain.SimTime `json:"rec_at_ns"`
	Horizon domain.SimTime `json:"horizon_ns"`
	Policy  RecoveryPolicy `json:"policy"`

	Pre       PhaseReport `json:"pre"`
	During    PhaseReport `json:"during"`
	Immediate PhaseReport `json:"immediate"`
	Drain     PhaseReport `json:"drain"`
	Post      PhaseReport `json:"post"`

	// DrainAt: the first instant after RecAt at which no attempt is queued,
	// no attempt is pinned waiting for a connection and no worker is busy —
	// the full backlog drained. Drained=false means the system never fully
	// drained inside the horizon.
	DrainAt domain.SimTime `json:"drain_at_ns"`
	Drained bool           `json:"drained"`
	// PostEmpty: the post phase has zero length (the horizon was reached
	// before any post-recovery observation was possible).
	PostEmpty bool          `json:"post_empty"`
	Class     RecoveryClass `json:"class"`

	// Goodput rates in completions per millisecond of simulated time.
	PreGoodputRate  float64 `json:"pre_goodput_rate_per_ms"`
	PostGoodputRate float64 `json:"post_goodput_rate_per_ms"`

	// BudgetDecisions: retry-budget consumptions per phase name, counted
	// from the in-order record ledger ("budget" note records). If the
	// recorder ran as a bounded ring and evicted records, this is the count
	// over retained records only (documented limitation).
	BudgetDecisions map[string]int `json:"budget_decisions"`
}

// --- occupancy timeline views --------------------------------------------------

// tl is a piecewise-constant timeline (ascending instants + counts), the
// internal analysis view over the occupancy samples of occupancy.go.
type tl struct {
	times  []domain.SimTime
	counts []int
}

// at reports the occupancy in force at instant t (0 before the first sample).
func (x tl) at(t domain.SimTime) int { return occAtUntil(x.times, x.counts, t) }

// instantsUnion returns the sorted union instants of a set of timelines.
func instantsUnion(tls ...tl) []domain.SimTime {
	set := make(map[domain.SimTime]struct{})
	for _, x := range tls {
		for _, t := range x.times {
			set[t] = struct{}{}
		}
	}
	out := make([]domain.SimTime, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// workerTl builds the shared worker-pool occupancy timeline.
func (st *SystemState) workerTl() tl {
	x := tl{times: make([]domain.SimTime, len(st.WorkerOccupancy)), counts: make([]int, len(st.WorkerOccupancy))}
	for i, w := range st.WorkerOccupancy {
		x.times[i], x.counts[i] = w.At, w.Busy
	}
	return x
}

// queueTotalTl merges the per-tenant queue timelines into one total-depth
// timeline sampled at the union of all sample instants.
func (st *SystemState) queueTotalTl() tl {
	byTenant := make(map[domain.TenantID]tl)
	for _, s := range st.QueueOccupancy {
		x := byTenant[s.Tenant]
		x.times = append(x.times, s.At)
		x.counts = append(x.counts, s.Length)
		byTenant[s.Tenant] = x
	}
	var parts []tl
	for _, x := range byTenant {
		parts = append(parts, x)
	}
	instants := instantsUnion(parts...)
	out := tl{times: instants, counts: make([]int, len(instants))}
	for i, t := range instants {
		total := 0
		for _, x := range byTenant {
			total += x.at(t)
		}
		out.counts[i] = total
	}
	return out
}

// waitingTl merges the per-upstream waitlist timelines into one total-waiting
// timeline sampled at the union of all sample instants.
func (st *SystemState) waitingTl() tl {
	byUp := make(map[domain.UpstreamID]tl)
	for _, s := range st.ConnOccupancy {
		x := byUp[s.Up]
		x.times = append(x.times, s.At)
		x.counts = append(x.counts, s.Waiting)
		byUp[s.Up] = x
	}
	var parts []tl
	for _, x := range byUp {
		parts = append(parts, x)
	}
	instants := instantsUnion(parts...)
	out := tl{times: instants, counts: make([]int, len(instants))}
	for i, t := range instants {
		total := 0
		for _, x := range byUp {
			total += x.at(t)
		}
		out.counts[i] = total
	}
	return out
}

// drainAfter scans for the first instant in (recAt, horizon] at which the
// total queue depth, the total waitlist size and the busy-worker count are
// all zero. Occupancy is piecewise-constant and every relevant transition
// lands a sample exactly at its transition instant, so only instants present
// in the timelines need checking — the scan is EXACT.
func drainAfter(st *SystemState, recAt, horizon domain.SimTime) (domain.SimTime, bool) {
	workers := st.workerTl()
	queues := st.queueTotalTl()
	waiting := st.waitingTl()
	insts := instantsUnion(workers, queues, waiting)
	for _, t := range insts {
		if t <= recAt || t > horizon {
			continue
		}
		if workers.at(t) == 0 && queues.at(t) == 0 && waiting.at(t) == 0 {
			return t, true
		}
	}
	return 0, false
}

// countBudgetDecisionsIn counts retry-budget consumption decisions inside a
// window from the in-order record ledger ("budget" note records).
func countBudgetDecisionsIn(recs []Record, from, until domain.SimTime) int {
	n := 0
	for _, r := range recs {
		t := domain.SimTime(r.SimTimeNS)
		if r.Category == CategoryNote && r.Kind == "budget" && t >= from && t < until {
			n++
		}
	}
	return n
}

// goodputRateOf returns DeadlineRespecting completions per millisecond of the
// phase window (0 for an empty window).
func goodputRateOf(pr PhaseReport) float64 {
	w := pr.Until.Sub(pr.From)
	if w <= 0 {
		return 0
	}
	return float64(pr.DeadlineRespecting) / float64(w) * float64(domain.Millisecond)
}

// AnalyzeRecovery computes the full RecoveryReport for one degradation
// episode [degFrom, recAt) against the given horizon. Pure over the final
// state: nothing is mutated, nothing is reset.
func AnalyzeRecovery(
	st *SystemState,
	recs []Record,
	degFrom, recAt, horizon domain.SimTime,
	policy RecoveryPolicy,
) RecoveryReport {
	rep := RecoveryReport{
		DegFrom:         degFrom,
		RecAt:           recAt,
		Horizon:         horizon,
		Policy:          policy,
		BudgetDecisions: map[string]int{},
	}

	rep.Pre = st.SummarizePhase(0, degFrom)
	rep.During = st.SummarizePhase(degFrom, recAt)

	immUntil := recAt.Add(policy.DrainSpan)
	if immUntil > horizon {
		immUntil = horizon
	}
	rep.Immediate = st.SummarizePhase(recAt, immUntil)

	drainAt, drained := drainAfter(st, recAt, horizon)
	rep.DrainAt = drainAt
	rep.Drained = drained
	drainUntil := recAt
	if drained {
		drainUntil = drainAt
	}
	rep.Drain = st.SummarizePhase(recAt, drainUntil)

	postFrom := drainUntil
	rep.PostEmpty = postFrom >= horizon
	if postFrom > horizon {
		postFrom = horizon
	}
	rep.Post = st.SummarizePhase(postFrom, horizon)

	rep.PreGoodputRate = goodputRateOf(rep.Pre)
	rep.PostGoodputRate = goodputRateOf(rep.Post)

	setStateAtRecAt := rep.During // phase Until == recAt: state-at-Until reads exactly recAt

	switch {
	case rep.PostEmpty:
		rep.Class = RecoveryNoPostWindow
	case rep.Pre.DeadlineRespecting == 0:
		// empty healthy baseline: the classifier must refuse, loudly
		rep.Class = RecoveryNoBaseline
	case rep.PostGoodputRate >= policy.GoodputThreshold*rep.PreGoodputRate:
		if setStateAtRecAt.QueueDepthAtUntil == 0 && setStateAtRecAt.AttemptsActiveAtUntil == 0 {
			rep.Class = RecoveryImmediate
		} else {
			rep.Class = RecoveryDelayedButEventual
		}
	default:
		rep.Class = RecoverySustainedCollapse
	}

	rep.BudgetDecisions["pre"] = countBudgetDecisionsIn(recs, 0, degFrom)
	rep.BudgetDecisions["during"] = countBudgetDecisionsIn(recs, degFrom, recAt)
	rep.BudgetDecisions["immediate"] = countBudgetDecisionsIn(recs, recAt, immUntil)
	rep.BudgetDecisions["drain"] = countBudgetDecisionsIn(recs, recAt, drainUntil)
	rep.BudgetDecisions["post"] = countBudgetDecisionsIn(recs, drainUntil, horizon)
	return rep
}

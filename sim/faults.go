package sim

// faults.go implements controllable upstream fault injection (M2 workstream
// 2). A FaultSchedule is plain, recorded data: per (upstream, episode) it
// says WHICH degradation mode applies and WHEN, and Events() materializes it
// into the typed Model-control event stream. Because schedules are data and
// the events are typed, fault input is fully reproducible and recorded in
// experiment evidence next to the workload.
//
// The three modes compose with the M1 A12 failure model WITHOUT changing it:
//
//   - M1 degraded episode (EventUpstreamDegraded): latency becomes
//     DegradedLatency and — unchanged — every completion during the episode
//     fails. This IS the "complete unavailability" mode of the milestone.
//   - EventUpstreamLatencySet: an elevated-latency-but-succeeding mode: every
//     attempt started during the effect runs with the override duration and
//     COMPLETES SUCCESSFULLY (the completion-time failure rule of A12 is
//     bypassed exactly here, documented in ENGINE_SEMANTICS and the
//     ASSUMPTIONS addition for M2, never silently).
//   - EventUpstreamFailureSet{EveryN}: an elevated-failure-rate mode at
//     normal latency: the k-th completion during the effect (k deterministic
//     per attempt-finalization order) fails when k mod EveryN == 0.
//     EveryN == 1 means every completion fails (temporarily "hard down" but
//     fast-failing).
//
// Latency is fixed at execution start (A12), failures decided at completion
// (A12): the same attempt rules apply under all modes, only the inputs to
// those rules are dictated by the schedule here.

import (
	"fmt"
	"sort"

	"sentra/domain"
	"sentra/engine"
)

// FaultKind selects the mode applied during an episode.
type FaultKind int

const (
	// FaultSlowLatency: increased latency; completions succeed. Override
	// duration must be positive; a non-positive It value in the episode is
	// a schedule bug.
	FaultSlowLatency FaultKind = iota
	// FaultFailRate: elevated failure rate at normal latency; EveryN
	// completions, exactly the ones at k mod EveryN == 0 fail.
	FaultFailRate
	// FaultUnavailable: complete unavailability — the M1 degraded episode
	// semantics (slow latency AND every completion fails).
	FaultUnavailable
)

// String renders the mode name deterministically (evidence + scenarios).
func (k FaultKind) String() string {
	switch k {
	case FaultSlowLatency:
		return "slow-latency"
	case FaultFailRate:
		return "fail-rate"
	case FaultUnavailable:
		return "unavailable"
	default:
		return "FaultKind(?)"
	}
}

// FaultEpisode is one interval [From, Until) with one mode active on one
// upstream. Until == 0 (or >= horizon) means "active until the horizon".
type FaultEpisode struct {
	Upstream domain.UpstreamID `json:"upstream"`
	Kind     FaultKind         `json:"kind" `
	// EveryN is used only by FaultFailRate.
	EveryN int                `json:"every_n,omitempty"`
	It     domain.SimDuration `json:"latency_ns,omitempty"`
	From   domain.SimTime     `json:"from_ns"`
	Until  domain.SimTime     `json:"until_ns"`
}

// FaultSchedule is the recorded fault input of an experiment.
type FaultSchedule []FaultEpisode

// Validate rejects malformed episodes.
func (fs FaultSchedule) Validate(horizon domain.SimTime) error {
	for i, e := range fs {
		if e.Upstream == "" {
			return fmt.Errorf("fault episode %d: upstream must be set", i)
		}
		if e.From < 0 || e.Until < 0 {
			return fmt.Errorf("fault episode %d: negative bound", i)
		}
		if e.Until != 0 && e.Until <= e.From {
			return fmt.Errorf("fault episode %d: until (%s) not after from (%s)", i, e.Until, e.From)
		}
		if e.Until > horizon {
			return fmt.Errorf("fault episode %d: until (%s) beyond horizon (%s)", i, e.Until, horizon)
		}
		switch e.Kind {
		case FaultSlowLatency:
			if e.It <= 0 {
				return fmt.Errorf("fault episode %d: slow-latency episode needs positive latency", i)
			}
		case FaultFailRate:
			if e.EveryN < 1 {
				return fmt.Errorf("fault episode %d: fail-rate episode needs EveryN >= 1", i)
			}
		case FaultUnavailable:
			// no extra parameters
		default:
			return fmt.Errorf("fault episode %d: unknown kind %d", i, e.Kind)
		}
	}
	// Overlapping two-configured episodes on one upstream would make the
	// applied mode encode order; schedules are flat data instead.
	for i := range fs {
		for j := range fs {
			if i == j || fs[i].Upstream != fs[j].Upstream {
				continue
			}
			a, b := fs[i], fs[j]
			bu := b.Until
			if bu == 0 {
				bu = horizon
			}
			au := a.Until
			if au == 0 {
				au = horizon
			}
			if a.From < bu && b.From < au {
				return fmt.Errorf("fault episodes %d and %d overlap on upstream %s", i, j, a.Upstream)
			}
		}
	}
	return nil
}

// Events materializes the schedule into the serialized Model-control event
// stream: a set at From and a restore of exactly the episode's own mode at
// Until (restores are skipped when the episode already runs to the horizon).
// Events are ordered by time; restore events precede set events at equal
// instants so adjacent episodes hand over cleanly (documented convention —
// the ordering does the handover, not extra state logic).
func (fs FaultSchedule) Events(horizon domain.SimTime) []engine.Event {
	if err := fs.Validate(horizon); err != nil {
		panic(err.Error())
	}
	type bound struct {
		t   domain.SimTime
		up  domain.UpstreamID
		set bool // true = apply, false = restore the episode's own mode
		fe  *FaultEpisode
	}
	var bounds []bound
	for i := range fs {
		e := fs[i]
		bounds = append(bounds, bound{t: domain.SimTime(e.From), up: e.Upstream, set: true, fe: &fs[i]})
		u := e.Until
		if u == 0 || u > horizon {
			continue // restore at horizon edge is a no-op: snapshot ends the experiment there
		}
		bounds = append(bounds, bound{t: u, up: e.Upstream, set: false, fe: &fs[i]})
	}
	sort.SliceStable(bounds, func(i, j int) bool {
		if bounds[i].t != bounds[j].t {
			return bounds[i].t < bounds[j].t
		}
		if bounds[i].set != bounds[j].set {
			return !bounds[i].set // restore before set at the same instant
		}
		return false
	})
	var out []engine.Event
	for _, b := range bounds {
		if b.set {
			out = append(out, faultSetEvent(b.t, *b.fe))
		} else {
			out = append(out, faultRestoreEvent(b.t, *b.fe))
		}
	}
	return out
}

// faultSetEvent applies the episode's mode.
// RecordJSON returns the canonical, replay-safe textual form of the fault
// schedule (recorded in experiment evidence next to the workload).
func (fs FaultSchedule) RecordJSON() string { return mustJSON(fs) }

func faultSetEvent(t domain.SimTime, e FaultEpisode) engine.Event {
	switch e.Kind {
	case FaultSlowLatency:
		return &EventUpstreamLatencySet{baseEvent: baseEvent{Time: t}, Upstream: e.Upstream, Latency: e.It}
	case FaultFailRate:
		return &EventUpstreamFailureSet{baseEvent: baseEvent{Time: t}, Upstream: e.Upstream, EveryN: e.EveryN}
	case FaultUnavailable:
		return NewUpstreamDegradedEvent(t, e.Upstream)
	default:
		panic("sim: faultSetEvent: unknown kind")
	}
}

// faultRestoreEvent clears exactly the mode the episode applied, restoring
// the M1-default controls on that axis (latency override → 0 = selection by
// Degraded; EveryN → 0 = failures follow A12 completion only).
func faultRestoreEvent(t domain.SimTime, e FaultEpisode) engine.Event {
	switch e.Kind {
	case FaultSlowLatency:
		return &EventUpstreamLatencySet{baseEvent: baseEvent{Time: t}, Upstream: e.Upstream, Latency: 0}
	case FaultFailRate:
		return &EventUpstreamFailureSet{baseEvent: baseEvent{Time: t}, Upstream: e.Upstream, EveryN: 0}
	case FaultUnavailable:
		return NewUpstreamRecoveredEvent(t, e.Upstream)
	default:
		panic("sim: faultRestoreEvent: unknown kind")
	}
}

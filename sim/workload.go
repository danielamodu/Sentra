package sim

// workload.go implements deterministic, replayable workload generation
// (M2 workstream 1). The WorkloadSpec is plain data: it is JSON-serializable
// (it IS the recorded workload input), and Build() materializes arrival
// events with only simulated-time arithmetic and the seeded PRNG of
// prng.go — never the wall clock, never package math/rand.
//
// Replay guarantee: calling Build twice on an identical spec yields two
// identical event lists (same times, same order); two engines fed with them
// run identical traces. The spec plus the fault schedule fully describe the
// recorded input of any experiment (see scenarios.go).

import (
	"encoding/json"
	"fmt"
	"sort"

	"sentra/domain"
	"sentra/engine"
)

// ArrivalComponent is one tenant's traffic stream feeding one upstream.
//
// Schedule model: the stream is sliced into fixed windows of length Window.
// Each window holds n = floor(rate + carry) arrivals; the fractional carry
// propagates across windows, so a stream of rate 0.4/window over 10 windows
// yields exactly 4 arrivals — never a sampling artifact. Within a window:
//
//   - Jitter == 0: arrivals are evenly spaced inside the window (no
//     randomness involved at all);
//   - 0 < Jitter <= 1: arrival positions are drawn uniformly inside the
//     window from the seeded PRNG (deterministic, explicitly seeded —
//     replay yields the identical trace).
//
// Deadlines are relative to the arrival instant and uniform in
// [DeadlineMin, DeadlineMax]; equal bounds give a fixed deadline. A
// DeadlineMin of 0 admits deadline == arrival requests (immediate expiry
// per A14 — an explicit scenario tool, e.g. for useful/wasted accounting).
type ArrivalComponent struct {
	Tenant   domain.TenantID   `json:"tenant"`
	Upstream domain.UpstreamID `json:"upstream"`

	// Rate is arrivals per Window. Sustained overload is expressed as plain
	// data: a rate whose per-window arrival count exceeds the system's
	// clearing capacity. There is no special "overload mode".
	Rate   float64            `json:"rate_per_window"`
	Window domain.SimDuration `json:"window_ns"`

	// From (inclusive) and Until (exclusive) bound the stream in simulated
	// time; Until == 0 means "until the horizon passed to Build".
	From  domain.SimTime `json:"from_ns"`
	Until domain.SimTime `json:"until_ns"`

	// Deadline bounds relative to the arrival instant.
	DeadlineMin domain.SimDuration `json:"deadline_min_ns"`
	DeadlineMax domain.SimDuration `json:"deadline_max_ns"`

	MaxRetries int `json:"max_retries"`

	// Jitter: 0 = strictly periodic spacing; (0,1] = seeded uniform jitter
	// inside each window. The PRNG stream index is the component's slice
	// index, so components never share a random walk.
	Jitter float64 `json:"jitter"`
}

// Burst is a controlled one-shot surge of a single (tenant, upstream) pair
// (workstream 1: controlled bursts). Arrivals are evenly spaced inside
// [At, At+Spread); Spread == 0 places all of them at exactly At (a
// same-instant burst, which exercises the pending-lane admission books
// directly).
type Burst struct {
	Tenant      domain.TenantID    `json:"tenant"`
	Upstream    domain.UpstreamID  `json:"upstream"`
	At          domain.SimTime     `json:"at_ns"`
	Count       int                `json:"count"`
	Spread      domain.SimDuration `json:"spread_ns"`
	DeadlineMin domain.SimDuration `json:"deadline_min_ns"`
	DeadlineMax domain.SimDuration `json:"deadline_max_ns"`
	MaxRetries  int                `json:"max_retries"`
}

// WorkloadSpec is the complete workload input of an experiment.
type WorkloadSpec struct {
	Seed       uint64             `json:"seed"`
	Components []ArrivalComponent `json:"components"`
	Bursts     []Burst            `json:"bursts"`
}

// Validate rejects malformed input early (before expensive builds).
func (w WorkloadSpec) Validate(horizon domain.SimTime) error {
	for i := range w.Components {
		c := w.Components[i]
		if c.Tenant == "" || c.Upstream == "" {
			return fmt.Errorf("workload component %d: tenant and upstream must be set", i)
		}
		if c.Rate < 0 {
			return fmt.Errorf("workload component %d: rate must be non-negative", i)
		}
		if c.Window <= 0 {
			return fmt.Errorf("workload component %d: window must be positive", i)
		}
		if c.Jitter < 0 || c.Jitter > 1 {
			return fmt.Errorf("workload component %d: jitter must be in [0,1]", i)
		}
		if c.DeadlineMin > c.DeadlineMax {
			return fmt.Errorf("workload component %d: deadline range inverted", i)
		}
		if c.Until > horizon {
			return fmt.Errorf("workload component %d: until (%s) beyond horizon (%s)", i, c.Until, horizon)
		}
	}
	for i := range w.Bursts {
		b := w.Bursts[i]
		if b.Tenant == "" || b.Upstream == "" {
			return fmt.Errorf("burst %d: tenant and upstream must be set", i)
		}
		if b.Count <= 0 {
			return fmt.Errorf("burst %d: count must be positive", i)
		}
		if b.At > horizon {
			return fmt.Errorf("burst %d: at (%s) beyond horizon (%s)", i, b.At, horizon)
		}
		if b.DeadlineMin > b.DeadlineMax {
			return fmt.Errorf("burst %d: deadline range inverted", i)
		}
	}
	return nil
}

// buildJob is the internal (time, event) pair used for the final
// deterministic ordering. source preserves the declaration order of the
// spec (component index, then burst index) as the stable secondary key.
type buildJob struct {
	t      domain.SimTime
	source int
	ev     engine.Event
}

// Build materializes the complete arrival event list, ordered by time and,
// for equal times, by the order in which the spec declared the sources.
func (w WorkloadSpec) Build(horizon domain.SimTime) []engine.Event {
	if err := w.Validate(horizon); err != nil {
		panic(err.Error()) // spec errors are programming errors, not runtime conditions
	}
	var out []buildJob
	for i := range w.Components {
		c := w.Components[i] // local copy: the loop must not mutate the spec
		until := c.Until
		if until == 0 {
			until = horizon
		}
		rng := NewPRNG(w.Seed, uint64(i)+1) // component i uses stream i+1 (stream 0 is reserved)
		carry := 0.0
		st := domain.SimTime(c.From)
		for c.Rate > 0 && st < until {
			n := nextWindowCount(c.Rate, &carry)
			for k := 0; k < n; k++ {
				t := windowArrivalPlace(c, st, k, n, rng)
				dl := deadlineSpan(c.DeadlineMin, c.DeadlineMax, rng)
				out = append(out, buildJob{
					t:      t,
					source: i,
					ev:     NewArrivalEvent(t, c.Tenant, c.Upstream, t.Add(dl), c.MaxRetries),
				})
			}
			st = st.Add(c.Window)
		}
	}
	for i := range w.Bursts {
		b := w.Bursts[i]
		dlRng := NewPRNG(w.Seed^0xA5A5A5, uint64(len(w.Components))+uint64(i)+1)
		for j := 0; j < b.Count; j++ {
			t := b.At
			if b.Spread > 0 {
				t = b.At + domain.SimTime(int64(b.Spread)*int64(j)/int64(b.Count))
			}
			dl := deadlineSpan(b.DeadlineMin, b.DeadlineMax, dlRng)
			out = append(out, buildJob{
				t:      t,
				source: len(w.Components) + i,
				ev:     NewArrivalEvent(t, b.Tenant, b.Upstream, t.Add(dl), b.MaxRetries),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].t != out[j].t {
			return out[i].t < out[j].t
		}
		return out[i].source < out[j].source
	})
	events := make([]engine.Event, 0, len(out))
	for _, o := range out {
		events = append(events, o.ev)
	}
	return events
}

// nextWindowCount returns the integer arrival count of the next window for a
// component with the given fractional rate, advancing the carry. Extracting
// the fraction exactly makes e.g. rate 0.4 over 10 windows yield precisely
// arrivals at windows 3, 8 (integer weights 1+... accumulating exactly 4).
func nextWindowCount(rate float64, carry *float64) int {
	x := rate + *carry
	n := int(x) // floor pass: n == floor(rate+carry) since x >= 0
	*carry = x - float64(n)
	return n
}

// windowArrivalPlace computes the instant of arrival k (0-based, of n) in the
// window starting at winStart: evenly spaced at (k+1)/(n+1) fractions when
// jitter is 0, otherwise uniformly drawn from the component's PRNG.
func windowArrivalPlace(c ArrivalComponent, winStart domain.SimTime, k int, n int, rng *PRNG) domain.SimTime {
	if k < 0 || k >= n || n == 0 {
		panic(fmt.Sprintf("sim: arrival placement k=%d of n=%d", k, n))
	}
	if c.Jitter == 0 {
		return winStart + domain.SimTime(int64(c.Window)*int64(k+1)/int64(n+1))
	}
	return winStart + domain.SimTime(rng.Span(c.Window))
}

// deadlineSpan returns a deterministic uniform value in [minD, maxD]. With
// minD == maxD it returns minD WITHOUT consuming the PRNG, so a fixed
// deadline does not shift the arrival-jitter sequence of the same stream.
func deadlineSpan(minD, maxD domain.SimDuration, rng *PRNG) domain.SimDuration {
	if maxD <= minD {
		return minD
	}
	return minD + rng.Span(maxD-minD)
}

// MarshalJSON implements json.Marshaler via the plain encoding/json path of
// the exported fields; kept as a tiny indirection for stability of the
// recorded representation.
func (w WorkloadSpec) MarshalJSON() ([]byte, error) {
	type plain WorkloadSpec
	return json.Marshal(plain(w))
}

// RecordJSON returns the canonical, replay-safe textual form of the workload
// (recorded in experiment evidence next to the fault schedule).
func (w WorkloadSpec) RecordJSON() string { return mustJSON(w) }

package sim

import (
	"testing"

	"sentra/domain"
	"sentra/engine"
)

// m2_workload_test.go covers the recorded-input machinery of M2:
//
//   - workstream 1: the seeded PRNG (determinism, stream separation, bounds)
//     and workload generation (replayability, exact fractional rates, fixed
//     deadlines never consuming the PRNG stream, burst placement);
//   - workstream 2: fault-schedule materialization (set/restore instants,
//     horizon rules, unavailable→degraded/recovered mapping, overlap
//     rejection) and the two M2 fault modes' actual runtime behaviour
//     through a real experiment run.

// --- workstream 1: seeded PRNG --------------------------------------------------

func TestPRNGIsDeterministicPerSeedAndStream(t *testing.T) {
	a := NewPRNG(12345, 7)
	b := NewPRNG(12345, 7)
	for i := 0; i < 100; i++ {
		if a.Next() != b.Next() {
			t.Fatalf("same (seed, stream) diverged at draw %d", i)
		}
	}
}

func TestPRNGStreamSeparation(t *testing.T) {
	// distinct streams under one seed must never advance in lockstep
	x1 := NewPRNG(1, 1)
	x2 := NewPRNG(1, 2)
	same := 0
	for i := 0; i < 64; i++ {
		if x1.Next() == x2.Next() {
			same++
		}
	}
	if same != 0 {
		t.Fatalf("%d of 64 draws identical across two streams", same)
	}
}

func TestPRNGBounds(t *testing.T) {
	r := NewPRNG(9, 3)
	for i := 0; i < 1000; i++ {
		if v := r.Intn(10); v < 0 || v >= 10 {
			t.Fatalf("Intn out of range: %d", v)
		}
		if d := r.Span(1000); d < 0 || d >= 1000 {
			t.Fatalf("Span out of range: %d", d)
		}
	}
	if got := NewPRNG(4, 1).Span(0); got != 0 {
		t.Fatalf("Span(0) = %d, want 0", got)
	}
}

// --- workstream 1: workload generation ------------------------------------------

func TestWorkloadBuildIsReplayable(t *testing.T) {
	spec := WorkloadSpec{
		Seed: 777,
		Components: []ArrivalComponent{
			compUntil(comp(t1, u1, 1.5, 40*ms, 2), 30*msT),
			compUntil(comp(t2, u2, 0.4, 30*ms, 1), 30*msT),
		},
		Bursts: []Burst{
			{Tenant: t1, Upstream: u1, At: 10 * msT, Count: 5, Spread: 3 * ms, DeadlineMin: 5 * ms, DeadlineMax: 15 * ms, MaxRetries: 1},
		},
	}
	hor := 50 * msT
	a := spec.Build(hor)
	b := spec.Build(hor)
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("replay lists differ in length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		ra, ok1 := a[i].(*EventRequestArrival)
		rb, ok2 := b[i].(*EventRequestArrival)
		if !ok1 || !ok2 {
			t.Fatalf("replay produced different event types at %d", i)
		}
		if ra.At() != rb.At() || ra.Deadline != rb.Deadline ||
			ra.Tenant != rb.Tenant || ra.Upstream != rb.Upstream ||
			ra.MaxRetries != rb.MaxRetries {
			t.Fatalf("replay diverges at event %d: (%s,dl=%s) vs (%s,dl=%s)", i, ra.At(), ra.Deadline, rb.At(), rb.Deadline)
		}
	}
	// ordering is strictly by time
	for i := 1; i < len(a); i++ {
		if a[i-1].At() > a[i].At() {
			t.Fatalf("event list not time-ordered at %d", i)
		}
	}
}

// TestWorkloadFractionalRateExactness: rate 0.4/window over 10 windows yields
// EXACTLY 4 arrivals — the fractional carry propagates, no sampling artifact.
func TestWorkloadFractionalRateExactness(t *testing.T) {
	spec := WorkloadSpec{Seed: 1, Components: []ArrivalComponent{
		compUntil(comp(t1, u1, 0.4, 100*ms, 1), 20*msT), // window = 2ms → 10 windows
	}}
	ev := spec.Build(20 * msT)
	if len(ev) != 4 {
		t.Fatalf("rate 0.4/window over 10 windows produced %d arrivals, want exactly 4", len(ev))
	}
	for _, e := range ev {
		ar, ok := e.(*EventRequestArrival)
		if !ok {
			t.Fatalf("expected an arrival, got %T", e)
		}
		if ar.At() < 0 || ar.At() >= 20*msT {
			t.Fatalf("arrival at %s outside the stream window", ar.At())
		}
	}
}

// TestWorkloadJitterIsSeeded: a jittered build is reproduced identically, and
// differs from the same stream without jitter (both are deterministic input).
func TestWorkloadJitterIsSeeded(t *testing.T) {
	base := comp(t1, u1, 1.0, 40*ms, 0)
	base.Window = 4 * ms
	jit := base
	jit.Jitter = 1

	hor := 20 * msT
	a := WorkloadSpec{Seed: 3, Components: []ArrivalComponent{jit}}.Build(hor)
	b := WorkloadSpec{Seed: 3, Components: []ArrivalComponent{jit}}.Build(hor)
	for i := range a {
		if a[i].At() != b[i].At() {
			t.Fatalf("jittered build not replayable at %d", i)
		}
	}
	c := WorkloadSpec{Seed: 3, Components: []ArrivalComponent{base}}.Build(hor)
	if len(a) != len(c) {
		t.Fatalf("count changed with jitter: %d vs %d", len(a), len(c))
	}
	diff := false
	for i := range a {
		if a[i].At() != c[i].At() {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatal("jittered stream identical to the even stream — jitter had no effect")
	}
	// jittered instants must still land inside their windows
	for _, e := range a {
		at := e.(*EventRequestArrival).At()
		if at < 0 || at >= 20*msT {
			t.Fatalf("jittered arrival at %s out of bounds", at)
		}
	}
}

// TestWorkloadFixedDeadlineDoesNotConsumePRNG: with equal deadline bounds the
// deadline sample consumes nothing, so changing ONLY the fixed-deadline value
// must leave the (jittered) arrival placement sequence untouched.
func TestWorkloadFixedDeadlineDoesNotConsumePRNG(t *testing.T) {
	fixedA := comp(t1, u1, 1.0, 30*ms, 0)
	fixedA.Window, fixedA.Jitter = 3*ms, 1
	fixedB := fixedA
	fixedB.DeadlineMin, fixedB.DeadlineMax = 60*ms, 60*ms

	ga := WorkloadSpec{Seed: 5, Components: []ArrivalComponent{fixedA}}.Build(20 * msT)
	gb := WorkloadSpec{Seed: 5, Components: []ArrivalComponent{fixedB}}.Build(20 * msT)
	if len(ga) != len(gb) {
		t.Fatalf("lengths differ: %d vs %d", len(ga), len(gb))
	}
	changedDeadline := false
	for i := range ga {
		ra, rb := ga[i].(*EventRequestArrival), gb[i].(*EventRequestArrival)
		if ra.At() != rb.At() {
			t.Fatalf("fixed deadline changed arrival placement at %d", i)
		}
		if ra.Deadline.Sub(ra.At()) != rb.Deadline.Sub(rb.At()) {
			changedDeadline = true
		}
	}
	if !changedDeadline {
		t.Fatal("deadline change did not take effect — test inconclusive")
	}
}

// TestWorkloadBurstSpreadZero: Spread == 0 places every burst arrival exactly
// at At (a same-instant burst exercising the admission books directly).
func TestWorkloadBurstSpreadZero(t *testing.T) {
	spec := WorkloadSpec{
		Seed: 11,
		Bursts: []Burst{
			{Tenant: t1, Upstream: u1, At: 5 * msT, Count: 4, DeadlineMin: 10 * ms, DeadlineMax: 10 * ms, MaxRetries: 0},
		},
	}
	ev := spec.Build(20 * msT)
	if len(ev) != 4 {
		t.Fatalf("burst count = %d, want 4", len(ev))
	}
	for _, e := range ev {
		ar := e.(*EventRequestArrival)
		if ar.At() != 5*msT {
			t.Fatalf("spread-0 burst arrival at %s, want exactly %s", ar.At(), 5*msT)
		}
	}
}

// --- workstream 2: fault-schedule materialization --------------------------------

func TestFaultScheduleEventsMaterializeSetAndRestore(t *testing.T) {
	hor := 100 * msT
	fs := FaultSchedule{
		{Upstream: u1, Kind: FaultSlowLatency, It: 12 * ms, From: 20 * msT, Until: 60 * msT},
	}
	// one set at From, one restore of exactly the episode's own mode at Until
	ev := fs.Events(hor)
	if len(ev) != 2 {
		t.Fatalf("episode within horizon must yield 2 events, got %d", len(ev))
	}
	set, ok := ev[0].(*EventUpstreamLatencySet)
	if !ok || set.Latency != 12*ms || set.Upstream != u1 {
		t.Fatalf("first event must SET the latency override, got %T %+v", ev[0], ev[0])
	}
	rest, ok := ev[1].(*EventUpstreamLatencySet)
	if !ok || rest.Latency != 0 {
		t.Fatalf("second event must CLEAR the latency (restore), got %T %+v", ev[1], ev[1])
	}
	if ev[0].At() != 20*msT || ev[1].At() != 60*msT {
		t.Fatalf("fault event instants wrong: %s then %s", ev[0].At(), ev[1].At())
	}

	// Until == 0 means active-to-horizon: only the set event is emitted.
	fsOpenEnded := FaultSchedule{{Upstream: u1, Kind: FaultFailRate, EveryN: 3, From: 10 * msT}}
	if ev := fsOpenEnded.Events(hor); len(ev) != 1 {
		t.Fatalf("open-ended episode must emit only the set event, got %d", len(ev))
	}
}

// TestFaultScheduleRestoreOrdering: adjacent episodes hand over through event
// ordering alone — restore-before-set at the shared instant.
func TestFaultScheduleRestoreOrdering(t *testing.T) {
	hor := 100 * msT
	fs := FaultSchedule{
		{Upstream: u1, Kind: FaultSlowLatency, It: 12 * ms, From: 10 * msT, Until: 30 * msT},
		{Upstream: u1, Kind: FaultSlowLatency, It: 18 * ms, From: 30 * msT, Until: 60 * msT},
	}
	ev := fs.Events(hor)
	if len(ev) != 4 {
		t.Fatalf("two adjacent episodes must yield 4 events, got %d", len(ev))
	}
	want := []struct {
		t       domain.SimTime
		latency domain.SimDuration
	}{
		{10 * msT, 12 * ms},
		{30 * msT, 0}, // restore first at the shared instant
		{30 * msT, 18 * ms},
		{60 * msT, 0},
	}
	for i, w := range want {
		e, ok := ev[i].(*EventUpstreamLatencySet)
		if !ok {
			t.Fatalf("event %d is %T, want latency set", i, ev[i])
		}
		if e.At() != w.t || e.Latency != w.latency {
			t.Fatalf("event %d: (%s, %s), want (%s, %s)", i, e.At(), e.Latency, w.t, w.latency)
		}
	}
}

// TestFaultScheduleHorizonSemantics documents the materialization boundary:
// an episode whose Until is strictly past the horizon emits no restore (the
// run ends first); an episode ending exactly AT the horizon still emits its
// restore at that final instant, where the engine processes it (harmless by
// construction — the run ends there).
func TestFaultScheduleHorizonSemantics(t *testing.T) {
	hor := 100 * msT
	past := FaultSchedule{{Upstream: u1, Kind: FaultSlowLatency, It: 9 * ms, From: 20 * msT, Until: 120 * msT}}
	if err := past.Validate(hor); err == nil {
		t.Fatal("Until beyond the horizon must be rejected by Validate")
	}

	exact := FaultSchedule{{Upstream: u1, Kind: FaultSlowLatency, It: 9 * ms, From: 20 * msT, Until: 100 * msT}}
	ev := exact.Events(hor)
	if len(ev) != 2 {
		t.Fatalf("episode ending exactly at the horizon: set + restore (processed at the final instant), got %d events", len(ev))
	}
}

func TestFaultScheduleUnavailableMapsToDegradedRecovered(t *testing.T) {
	hor := 100 * msT
	fs := FaultSchedule{{Upstream: u1, Kind: FaultUnavailable, From: 20 * msT, Until: 60 * msT}}
	ev := fs.Events(hor)
	if _, ok := ev[0].(*EventUpstreamDegraded); !ok {
		t.Fatalf("unavailable set must be the M1 degraded event, got %T", ev[0])
	}
	if _, ok := ev[1].(*EventUpstreamRecovered); !ok {
		t.Fatalf("unavailable restore must be the M1 recovered event, got %T", ev[1])
	}
}

// TestFaultScheduleRejectsSameUpstreamOverlap: overlapping episodes on ONE
// upstream would make the applied mode encode list order; schedules are flat
// data instead. Overlaps across DIFFERENT upstreams are legal (U1/U2 fault
// independence).
func TestFaultScheduleRejectsSameUpstreamOverlap(t *testing.T) {
	hor := 100 * msT
	bad := FaultSchedule{
		{Upstream: u1, Kind: FaultSlowLatency, It: 12 * ms, From: 20 * msT, Until: 60 * msT},
		{Upstream: u1, Kind: FaultFailRate, EveryN: 2, From: 40 * msT, Until: 90 * msT},
	}
	if err := bad.Validate(hor); err == nil {
		t.Fatal("overlapping episodes on one upstream must be rejected")
	}
	cross := FaultSchedule{
		{Upstream: u1, Kind: FaultSlowLatency, It: 12 * ms, From: 20 * msT, Until: 60 * msT},
		{Upstream: u2, Kind: FaultFailRate, EveryN: 2, From: 40 * msT, Until: 90 * msT},
	}
	if err := cross.Validate(hor); err != nil {
		t.Fatalf("overlaps across different upstreams are legal: %v", err)
	}
}

// TestFaultScheduleRejectsMalformedEpisodes: per-kind parameter rules.
func TestFaultScheduleRejectsMalformedEpisodes(t *testing.T) {
	hor := 100 * msT
	cases := []FaultSchedule{
		{{Upstream: u1, Kind: FaultSlowLatency, It: 0, From: 0, Until: 10 * msT}},                     // no latency
		{{Upstream: u1, Kind: FaultFailRate, EveryN: 0, From: 0, Until: 10 * msT}},                    // no rate
		{{Upstream: u1, Kind: FaultUnavailable, From: 30 * msT, Until: 10 * msT}},                     // inverted interval
		{{Upstream: domain.UpstreamID(""), Kind: FaultFailRate, EveryN: 2, From: 0, Until: 10 * msT}}, // no upstream
	}
	for i, fs := range cases {
		if err := fs.Validate(hor); err == nil {
			t.Fatalf("case %d must be rejected", i)
		}
	}
}

// --- workstream 2: runtime fault behaviour ---------------------------------------

func faultRunConfig() SystemConfig {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 4
	cfg.Tenants = map[domain.TenantID]TenantConfig{
		t1: {MaxArrivalQueueDepth: 16, MaxRetryQueueDepth: 16, MaxTotalQueueDepth: 16},
	}
	return cfg
}

// specEvents materializes one arrival component as a seeded workload.
func specEvents(c ArrivalComponent, hor domain.SimTime) []engine.Event {
	return WorkloadSpec{Seed: 42, Components: []ArrivalComponent{c}}.Build(hor)
}

// TestFaultLatencyAppliesAndRestores: during the latency window every
// attempt that starts executing takes the override duration; attempts that
// start at-or-after the restore instant run the M1 health-based latency again.
// Completions succeed throughout, so the breaker stays closed and no request
// retries or fails.
func TestFaultLatencyAppliesAndRestores(t *testing.T) {
	const (
		override    = domain.SimDuration(20 * domain.Millisecond)
		normal      = domain.SimDuration(5 * domain.Millisecond)
		windowStart = 10 * msT
		windowEnd   = 30 * msT
	)
	hor := 60 * msT
	fs := FaultSchedule{{Upstream: u1, Kind: FaultSlowLatency, It: override, From: windowStart, Until: windowEnd}}

	cfg := faultRunConfig()
	s := mustNew(t, cfg)
	initial := specEvents(comp(t1, u1, 1.0, 50*ms, 0), hor)
	run(t, s, append(initial, fs.Events(hor)...), hor)

	st := s.State()
	applied, restored := false, false
	for _, att := range st.Attempts {
		if att.Upstream != u1 {
			continue
		}
		switch att.BeganRunningAt >= windowStart && att.BeganRunningAt < windowEnd {
		case true:
			if att.ExecDuration != override {
				t.Fatalf("attempt %s started at %s ran %s, want the %s override", att.ID, att.BeganRunningAt, att.ExecDuration, override)
			}
			applied = true
		default:
			if att.BeganRunningAt > 0 {
				if att.ExecDuration != normal {
					t.Fatalf("attempt %s started at %s (outside window) ran %s, want the normal %s", att.ID, att.BeganRunningAt, att.ExecDuration, normal)
				}
				if att.BeganRunningAt >= windowEnd {
					restored = true
				}
			}
		}
	}
	if !applied {
		t.Fatal("no attempt ran inside the fault window — fault did not apply")
	}
	if !restored {
		t.Fatal("no attempt started after the restore instant — restore unverifiable")
	}
	if st.Breakers[u1].State != domain.BreakerClosed {
		t.Fatalf("breaker state %s, want closed (completions succeed under latency mode)", st.Breakers[u1].State)
	}
	for _, req := range st.Requests {
		if req.State == domain.RequestFailed {
			t.Fatalf("request %s failed under latency-only mode", req.ID)
		}
	}
	if st.Faults[u1].LatencyOverride != 0 {
		t.Fatalf("latency override %s still armed after restore", st.Faults[u1].LatencyOverride)
	}
}

// TestFaultFailRateDeterministicCounter: with EveryN == 2, exactly every
// second completion DECISION fails. Every running u1 attempt completes (5ms
// exec < timeout), so with K completions there are exactly floor(K/2) failures,
// independent of arrival interleaving — a deterministic counter, not randomness.
func TestFaultFailRateDeterministicCounter(t *testing.T) {
	hor := 40 * msT
	fs := FaultSchedule{{Upstream: u1, Kind: FaultFailRate, EveryN: 2, From: 0, Until: hor}}

	cfg := faultRunConfig()
	s := mustNew(t, cfg)
	initial := specEvents(comp(t1, u1, 1.0, 100*ms, 1), hor)
	run(t, s, append(initial, fs.Events(hor)...), hor)

	st := s.State()
	running, failed := 0, 0
	for _, att := range st.Attempts {
		if att.Upstream != u1 {
			continue
		}
		if att.BeganRunningAt > 0 && att.State.Terminal() {
			running++
			if att.State == domain.AttemptFailed {
				failed++
			}
		}
	}
	if running < 4 {
		t.Fatalf("too few completions to exercise the counter: K=%d", running)
	}
	if want := running / 2; failed != want {
		t.Fatalf("EveryN=2: %d failures of %d completions, want exactly %d", failed, running, want)
	}
	if st.Faults[u1].EveryN != 0 || st.Faults[u1].FailCounter != 0 {
		t.Fatalf("fail-rate mode not cleared after the episode: %+v", st.Faults[u1])
	}
}

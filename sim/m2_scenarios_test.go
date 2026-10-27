package sim

import (
	"math"
	"testing"

	"sentra/domain"
)

// m2_scenarios_test.go covers workstreams 9/10: the scenario library A–J
// (reproducibility + pinned qualitative behaviour) and experimental validity
// (a control scenario, recorded inputs, replay checks, observed classes).
//
// Every quantitative anchor here is a MEASURED value from the calibrated
// library (see docs/MILESTONE_2_REPORT.md for the full tables); tests pin
// observed behaviour so silent semantic changes fail loudly. RunExperiment
// itself panics if any state invariant is violated at a phase boundary, so
// each call below is also an invariant validation.

// TestReplayCheckAllScenarios: every scenario must be bit-for-bit
// reproducible — two fresh runs agree on summaries, phase reports, the whole
// record ledger and the recovery reports. This is the replay guarantee of the
// recorded-input machinery, executed over the entire library.
func TestReplayCheckAllScenarios(t *testing.T) {
	for _, exp := range AllScenarios() {
		if err := ReplayCheck(exp); err != nil {
			t.Fatalf("%s: %v", exp.Name, err)
		}
	}
}

// TestScenarioAHealthyControl pins the control scenario: zero fault input,
// zero amplification, zero waste, and queues that never build up. Every other
// scenario inherits A's config and is read against these numbers.
func TestScenarioAHealthyControl(t *testing.T) {
	exp := ScenarioA()
	res := RunExperiment(exp)
	s := res.Summary

	// control input hygiene: no fault events and no breaker movement anywhere
	for _, r := range res.Records {
		if r.Kind == "fault" || r.Kind == "breaker" {
			t.Fatalf("control scenario recorded %s note: %+v", r.Kind, r)
		}
	}
	if s.Offered != 29 || s.Admitted != 29 || s.Shed != 0 {
		t.Fatalf("A offered/admitted/shed = %d/%d/%d, want 29/29/0", s.Offered, s.Admitted, s.Shed)
	}
	if s.Completed != 28 || s.DeadlineRespecting != 28 {
		t.Fatalf("A completed/goodput = %d/%d, want 28/28 (one request still open at the horizon)",
			s.Completed, s.DeadlineRespecting)
	}
	if s.RetryPhysical != 0 || s.RetryTokensConsumed != 0 || s.BlockedRetries != 0 {
		t.Fatalf("A retry activity: attempts=%d tokens=%d blocked=%d, want all zero",
			s.RetryPhysical, s.RetryTokensConsumed, s.BlockedRetries)
	}
	if s.Amplification != 1.0 {
		t.Fatalf("A amplification = %v, want exactly 1 (no retries)", s.Amplification)
	}
	if s.WastedShare != 0.0 || s.AttemptsWasted != 0 {
		t.Fatalf("A wasted work: share=%v attempts=%d, want zero", s.WastedShare, s.AttemptsWasted)
	}
	if failed, expired := s.Failed, s.Expired; failed != 0 || expired != 0 {
		t.Fatalf("A failed=%d expired=%d, want 0/0", failed, expired)
	}
	// workers partially busy, queues empty
	if math.Abs(s.WorkerOcc.Util-0.54) > 0.02 {
		t.Fatalf("A worker utilization %v, want ≈0.54", s.WorkerOcc.Util)
	}
	if s.QueueOcc[string(u1)].Peak != 0 {
		t.Fatalf("A u1 queue occupancy peak = %d, want 0 (queues never build)", s.QueueOcc[string(u1)].Peak)
	}
}

// TestScenarioBOrdinaryOverload pins the no-fault overload reference: heavy
// shedding at admission, everything admitted completes, and the tail phase
// fully drains (ordinary overload without amplification does NOT metastabilize).
func TestScenarioBOrdinaryOverload(t *testing.T) {
	res := RunExperiment(ScenarioB())
	s := res.Summary
	if s.Offered != 128 || s.Admitted != 48 || s.Shed != 80 {
		t.Fatalf("B offered/admitted/shed = %d/%d/%d, want 128/48/80", s.Offered, s.Admitted, s.Shed)
	}
	if s.Completed != s.Admitted || s.DeadlineRespecting != s.Admitted {
		t.Fatalf("B admitted requests all complete in deadline: %d/%d of %d",
			s.Completed, s.DeadlineRespecting, s.Admitted)
	}
	if s.Failed != 0 || s.Expired != 0 || s.RetryPhysical != 0 {
		t.Fatalf("B failure/retry activity impossible without faults: failed=%d expired=%d retry=%d",
			s.Failed, s.Expired, s.RetryPhysical)
	}
	var overload, tail PhaseReport
	for _, ph := range res.Phases {
		switch ph.Name {
		case "overload":
			overload = ph.Report
		case "tail":
			tail = ph.Report
		default:
			t.Fatalf("B has unexpected phase %q", ph.Name)
		}
	}
	if overload.QueueDepthAtUntil == 0 || overload.AttemptsActiveAtUntil == 0 {
		t.Fatalf("B overload phase must end with a live backlog: %+v", overload)
	}
	if tail.QueueDepthAtUntil != 0 || tail.AttemptsActiveAtUntil != 0 {
		t.Fatalf("B tail must fully drain: queue=%d active=%d",
			tail.QueueDepthAtUntil, tail.AttemptsActiveAtUntil)
	}
	for _, r := range res.Records {
		if r.Kind == "fault" || r.Kind == "breaker" {
			t.Fatalf("B must run without fault or breaker input: %+v", r)
		}
	}
}

// TestScenarioCDegradedWithHealthyPeer pins scenario C: complete u1
// unavailability inside the window with u2 healthy; delayed-but-eventual
// recovery; the healthy peer keeps serving throughout.
func TestScenarioCDegraded(t *testing.T) {
	res := RunExperiment(ScenarioC())
	s := res.Summary
	rec := res.Recovery
	if rec == nil {
		t.Fatal("C must define a recovery anchor")
	}
	if s.Offered != 29 || s.Admitted != 22 || s.Shed != 7 {
		t.Fatalf("C offered/admitted/shed = %d/%d/%d, want 29/22/7", s.Offered, s.Admitted, s.Shed)
	}
	// u2 healthy throughout: no failures, completions all deadline-respecting
	t2 := s.Tenant[string(t2)]
	if t2.Failed != 0 || t2.Completed != t2.DeadlineRespect {
		t.Fatalf("C t2 must be untouched by the u1 outage: failed=%d completed=%d dr=%d",
			t2.Failed, t2.Completed, t2.DeadlineRespect)
	}
	if rec.Class != RecoveryDelayedButEventual {
		t.Fatalf("C class %s, want delayed-but-eventual (u1 recovers, FIFO drains the 3 backlog)",
			rec.Class)
	}
	if rec.During.QueueDepthAtUntil != 3 {
		t.Fatalf("C backlog at recovery = %d, want 3", rec.During.QueueDepthAtUntil)
	}
	if rec.Drained {
		t.Fatalf("C drained=%v — the backlog must still be inside the system at the horizon as measured", rec.Drained)
	}
}

// TestScenarioDSlowRecovery pins the slow-mode probe: completions never
// fail (breaker never fires, retries never spawn), the backlog carries
// through the recovery instant, drains FIFO above the resumed inflow, and
// goodput recovers — delayed but eventual.
func TestScenarioDSlowRecovery(t *testing.T) {
	res := RunExperiment(ScenarioD())
	s := res.Summary
	rec := res.Recovery
	if rec == nil {
		t.Fatal("D must define a recovery anchor")
	}
	if s.RetryPhysical != 0 || s.BlockedRetries != 0 {
		t.Fatalf("D must produce zero retries (slow mode never fails): retry=%d blocked=%d",
			s.RetryPhysical, s.BlockedRetries)
	}
	if s.Amplification != 1.0 {
		t.Fatalf("D amplification = %v, want exactly 1", s.Amplification)
	}
	for _, r := range res.Records {
		if r.Kind == "breaker" {
			t.Fatalf("D breaker must never fire under slow-but-succeeding mode: %+v", r)
		}
	}
	if got := rec.During.QueueDepthAtUntil; got != 11 {
		t.Fatalf("D backlog at recovery = %d, want 11", got)
	}
	if got := rec.Post.QueueDepthAtUntil; got != 1 {
		t.Fatalf("D post-recovery queue = %d, want 1 (draining above the resumed inflow, never reset)", got)
	}
	if rec.PostGoodputRate <= rec.PreGoodputRate {
		t.Fatalf("D post rate %.3f must exceed the pre rate %.3f (recovery while draining)",
			rec.PostGoodputRate, rec.PreGoodputRate)
	}
	if rec.Class != RecoveryDelayedButEventual {
		t.Fatalf("D class %s, want delayed-but-eventual", rec.Class)
	}
	if s.Expired != 0 {
		t.Fatalf("D expired = %d, want 0 (drain beats the queue-expiry deadlines as measured)", s.Expired)
	}
}

// TestScenarioERetryAmplification pins the amplification probe: aware of the
// every-2nd-completion failure rule, retries consume budget tokens, failures
// exhaust retry-limited requests, and the breaker STAYS CLOSED (no
// consecutive-failure threshold is ever met).
func TestScenarioERetryAmplification(t *testing.T) {
	res := RunExperiment(ScenarioE())
	s := res.Summary
	if s.Offered != 29 || s.Admitted != 29 || s.Shed != 0 {
		t.Fatalf("E offered/admitted/shed = %d/%d/%d, want 29/29/0", s.Offered, s.Admitted, s.Shed)
	}
	if s.ArrivalAttempts != 29 || s.RetryPhysical != 3 || s.PhysicalAttempts != 32 {
		t.Fatalf("E attempts arr/retry/total = %d/%d/%d, want 29/3/32",
			s.ArrivalAttempts, s.RetryPhysical, s.PhysicalAttempts)
	}
	if s.RetryTokensConsumed != 3 {
		t.Fatalf("E retry tokens consumed = %d, want 3 (one per started retry)", s.RetryTokensConsumed)
	}
	if s.Failed != 4 || s.Completed != 24 || s.DeadlineRespecting != 24 {
		t.Fatalf("E outcomes completed/failed/GoodputLevel = %d/%d/%d, want 24/4/24",
			s.Completed, s.Failed, s.DeadlineRespecting)
	}
	if st := res.FinalState.Breakers[u1].State; st != domain.BreakerClosed {
		t.Fatalf("E breaker must never open (no consecutive failures): state %s", st)
	}
	// failure notes are counted (Kind "breaker", no transition details); the
	// TRANSITION set must be empty — the two-failure threshold is never met
	for _, r := range res.Records {
		if r.Category == CategoryNote && r.Kind == "breaker" && r.Detail.To != "" {
			t.Fatalf("E must record zero breaker transitions: %+v", r)
		}
	}
}

// TestScenarioFContentionAndConnExhaustion pins the contention probe: the
// slow upstream saturates the shared worker pool (cross-upstream starvation),
// exhausts its 2-connection pool exactly to capacity, expires queued work,
// wastes work on requests that expired and leaves a backlog that cannot
// clear inside the horizon (observed collapse within THIS horizon).
func TestScenarioFContention(t *testing.T) {
	res := RunExperiment(ScenarioF())
	s := res.Summary
	rec := res.Recovery
	if rec == nil {
		t.Fatal("F must define a recovery anchor")
	}
	if s.Offered != 66 || s.Admitted != 41 || s.Shed != 25 {
		t.Fatalf("F offered/admitted/shed = %d/%d/%d, want 66/41/25", s.Offered, s.Admitted, s.Shed)
	}
	if s.Expired != 16 || s.AttemptsWasted != 8 {
		t.Fatalf("F expired=%d wasted-attempts=%d, want 16/8 (deadline expiry under saturation)", s.Expired, s.AttemptsWasted)
	}
	if math.Abs(s.WastedShare-0.26) > 0.005 {
		t.Fatalf("F wasted share %v, want ≈0.26", s.WastedShare)
	}
	// the u1 connection pool exhausts EXACTLY to capacity (2)
	if got := s.ConnOcc[string(u1)].Peak; got != 2 {
		t.Fatalf("F u1 connection occupancy peak = %d, want exactly 2 (pool exhausted)", got)
	}
	// the recovery phase (boundary is 90ms; the reported 'during' is [30,90))
	if got := rec.During.WorkerOcc.Util; got < 0.98 {
		t.Fatalf("F during-worker utilization %v, want ≥0.98 (pool saturated)", got)
	}
	if got := rec.Post.QueueDepthAtUntil; got != 7 {
		t.Fatalf("F post queue depth = %d, want 7 (backlog survives the recovery instant)", got)
	}
	if rec.Class != RecoverySustainedCollapse {
		t.Fatalf("F class %s, want sustained-collapse-within-horizon (post rate %.3f ≪ pre %.3f)",
			rec.Class, rec.PostGoodputRate, rec.PreGoodputRate)
	}
}

// TestScenarioGInterference pins the cross-tenant fairness observation on the
// FIFO baseline: the heavy tenant's offered volume exceeds the light one's by
// construction, and BOTH tenants lose deadline-respecting completions —
// interference reaches the victim through the shared worker pool.
func TestScenarioGInterference(t *testing.T) {
	res := RunExperiment(ScenarioG())
	s := res.Summary
	th, tl := s.Tenant[string(t1)], s.Tenant[string(t2)]
	if th.Offered < 4*tl.Offered {
		t.Fatalf("G heavy-to-light offered ratio inverted or too small: t1=%d t3=%d", th.Offered, tl.Offered)
	}
	if th.DeadlineRespect >= th.Admitted || tl.DeadlineRespect >= tl.Admitted {
		t.Fatalf("G FIFO interference must cost BOTH tenants deadline-respecting completions: "+
			"t1 %d/%d t2 %d/%d", th.DeadlineRespect, th.Admitted, tl.DeadlineRespect, tl.Admitted)
	}
	if s.Expired != 7 || s.AttemptsWasted != 4 {
		t.Fatalf("G expired=%d wasted=%d, want 7/4", s.Expired, s.AttemptsWasted)
	}
	if math.Abs(s.WastedShare-0.08) > 0.005 {
		t.Fatalf("G wasted share %v, want ≈0.08", s.WastedShare)
	}
	// fairness math is available on the final state and is finite here
	jain := res.FinalState.FairnessSnapshot().JainSnapshot()
	if math.IsNaN(jain) || jain <= 0 || jain > 1 {
		t.Fatalf("G Jain index %v out of (0,1]", jain)
	}
}

// TestScenarioHBreakerLifecycle pins the breaker lifecycle evidence chain:
// exactly one full closed→open→half-open→closed cycle on u1, recorded as
// ordered breaker notes, with the sheds and failures that cause it.
func TestScenarioHBreakerLifecycle(t *testing.T) {
	res := RunExperiment(ScenarioH())
	s := res.Summary
	if s.Shed != 8 || s.Failed != 2 {
		t.Fatalf("H sheds/failures = %d/%d, want 8/2", s.Shed, s.Failed)
	}
	if s.Offered != 29 || s.Admitted != 21 {
		t.Fatalf("H offered/admitted = %d/%d, want 29/21", s.Offered, s.Admitted)
	}
	var chain []string
	for _, r := range res.Records {
		if r.Category == CategoryNote && r.Kind == "breaker" && r.Detail.Upstream == string(u1) {
			if r.Detail.To != "" {
				chain = append(chain, r.Detail.To)
			}
		}
	}
	want := []string{"open", "half-open", "closed"}
	if len(chain) != len(want) {
		t.Fatalf("H breaker chain %v, want %v", chain, want)
	}
	for i, to := range want {
		if chain[i] != to {
			t.Fatalf("H breaker chain %v, want %v (full recovery cycle)", chain, want)
		}
	}
	if got := res.Recovery.During.QueueDepthAtUntil; got != 0 {
		t.Fatalf("H backlog at recovery = %d, want 0 (no queue amplification under the breaker)", got)
	}
	if class := res.Recovery.Class; class != RecoveryDelayedButEventual {
		t.Fatalf("H class %s, want delayed-but-eventual", class)
	}
	if got := res.FinalState.Breakers[u1].State; got != domain.BreakerClosed {
		t.Fatalf("H breaker must be closed at the horizon: %s", got)
	}
}

// TestScenarioIBurstIsAdmissionEffect pins the burst experiment: the 12-strong
// burst at 60ms adds EXACTLY +12 offered and +12 sheds against H, everything
// else byte-identical in the shared phases.
func TestScenarioIBurst(t *testing.T) {
	h := RunExperiment(ScenarioH()).Summary
	i := RunExperiment(ScenarioI()).Summary
	if i.Offered != h.Offered+12 || i.Shed != h.Shed+12 {
		t.Fatalf("I must be H plus exactly (+12 offered, +12 sheds): offered %d vs %d, sheds %d vs %d",
			i.Offered, h.Offered, i.Shed, h.Shed)
	}
	if i.Admitted != h.Admitted {
		t.Fatalf("I admitted %d != H admitted %d — the burst must be fully rejected by admission",
			i.Admitted, h.Admitted)
	}
	if i.Completed != h.Completed || i.Failed != h.Failed {
		t.Fatalf("I terminal outcomes drifted from H: completed %d/%d failed %d/%d",
			i.Completed, h.Completed, i.Failed, h.Failed)
	}
	if i.ArrivalAttempts != h.ArrivalAttempts || i.RetryPhysical != h.RetryPhysical {
		t.Fatalf("I attempt volumes drifted from H: %d/%d vs %d/%d",
			i.ArrivalAttempts, i.RetryPhysical, h.ArrivalAttempts, h.RetryPhysical)
	}
}

// TestScenarioJMultipleEpisodes pins the repeated-episode probe: three
// degradation episodes produce retries and failures but no cumulative
// collapse — recovery after the last episode is delayed but eventual.
func TestScenarioJMultipleEpisodes(t *testing.T) {
	res := RunExperiment(ScenarioJ())
	s := res.Summary
	// token accounting: every STARTED retry spends one token, and BLOCKED
	// retries keep their token spent (documented semantics of the budget
	// ledger — a blocked retry is rejected after consumption to keep the
	// rule independent of admission state).
	if s.RetryPhysical != 3 {
		t.Fatalf("J started retries = %d, want 3", s.RetryPhysical)
	}
	if s.RetryTokensConsumed != s.RetryPhysical+s.BlockedRetries {
		t.Fatalf("J tokens %d != started %d + blocked %d",
			s.RetryTokensConsumed, s.RetryPhysical, s.BlockedRetries)
	}
	if s.Offered != 42 || s.Shed != 8 || s.Failed != 4 {
		t.Fatalf("J offered/sheds/failures = %d/%d/%d, want 42/8/4", s.Offered, s.Shed, s.Failed)
	}
	if len(res.Phases) != 7 {
		t.Fatalf("J phases = %d, want 7 (pre + 3 episodes + 2 gaps + post-last)", len(res.Phases))
	}
	if class := res.Recovery.Class; class != RecoveryDelayedButEventual {
		t.Fatalf("J class %s, want delayed-but-eventual", class)
	}
	// episodes never left a cumulative backlog: the queue stays empty at every
	// phase boundary (measured)
	for _, ph := range res.Phases {
		if ph.Report.QueueDepthAtUntil != 0 {
			t.Fatalf("J queue non-empty at phase %q boundary: %d", ph.Name, ph.Report.QueueDepthAtUntil)
		}
	}
}

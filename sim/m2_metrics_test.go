package sim

import (
	"math"
	"testing"

	"sentra/domain"
)

// m2_metrics_test.go covers the metrics model of workstream 7:
//
//   - occupancy integrals are EXACT and reconcile against the independent
//     per-attempt WorkerTime bookkeeping (two measurements, one quantity);
//   - logical-request and physical-attempt volume are never conflated
//     (goodput per request; amplification = attempts/admitted);
//   - blocked retries are counted as distinct retry-admission rejections;
//   - WorkAfterExpiry counts executed work delivered after its request's
//     expiry instant (definition C3);
//   - phase reports of a full-horizon partition reconcile with the whole-run
//     summary (no double counting, no missing volume);
//   - the discrete-percentile and piecewise-constant-occupancy conventions
//     are pinned by unit tests on synthetic inputs.

// reconcileWorkerTime returns (∫ busy dt over [0,horizon), Σ recorded
// WorkerTime, Σ open holds at the horizon instant) from the final state.
// The timeline integration here is an independent reimplementation of
// OccSummary integration, deliberately not sharing code with it.
func reconcileWorkerTime(st *SystemState, horizon domain.SimTime) (int64, domain.SimDuration, int64) {
	var sumNS int64
	cur := 0
	var cursor domain.SimTime
	first := true
	for _, w := range st.WorkerOccupancy {
		if first {
			cursor, cur, first = w.At, w.Busy, false
			continue
		}
		sumNS += int64(cur) * int64(w.At.Sub(cursor))
		cursor, cur = w.At, w.Busy
	}
	sumNS += int64(cur) * int64(horizon.Sub(cursor))

	var recorded domain.SimDuration
	var openNS int64
	for _, att := range st.Attempts {
		if att.WorkerSet {
			openNS += int64(horizon.Sub(att.WorkerSince))
		}
	}
	for _, m := range st.Metrics {
		recorded += m.WorkerTime
	}
	return sumNS, recorded, openNS
}

// TestWorkerOccupancyIntegralReconcilesWithWorkerTime: the timeline (one
// sample per exact take/release instant) and the per-attempt WorkerTime
// ledger are independent measurements of the same quantity; they must agree
// exactly once the never-released holds (open intervals at the horizon) are
// accounted for.
func TestWorkerOccupancyIntegralReconcilesWithWorkerTime(t *testing.T) {
	for _, exp := range []Experiment{ScenarioA(), ScenarioD(), ScenarioF()} {
		res := RunExperiment(exp)
		integral, recorded, openNS := reconcileWorkerTime(res.FinalState, exp.Horizon)
		if want := int64(recorded) + openNS; integral != want {
			t.Fatalf("%s: worker occupancy integral %d ns != WorkerTime %d ns + open holds %d ns",
				exp.Name, integral, recorded, openNS)
		}
	}
}

// TestSummaryConservation: terminal states partition the request set, open
// requests are exactly the admitted-but-not-terminal ones, and attempts per
// lane sum to the physical volume.
func TestSummaryConservation(t *testing.T) {
	for _, exp := range AllScenarios() {
		s := RunExperiment(exp).Summary
		terminalResolving := s.Completed + s.Failed + s.Expired + s.Cancelled
		if terminalResolving+s.Shed != s.Terminal {
			t.Fatalf("%s: terminal states %d != %d+%d", exp.Name, s.Terminal, terminalResolving, s.Shed)
		}
		if s.Terminal+s.Open != s.Offered {
			t.Fatalf("%s: terminal %d + open %d != offered %d", exp.Name, s.Terminal, s.Open, s.Offered)
		}
		if terminalResolving+s.Open != s.Admitted {
			t.Fatalf("%s: admitted %d != resolving %d + open %d (shed requests are never admitted)",
				exp.Name, s.Admitted, terminalResolving, s.Open)
		}
		if s.PhysicalAttempts != s.ArrivalAttempts+s.RetryPhysical {
			t.Fatalf("%s: attempts %d != arrival %d + retry %d",
				exp.Name, s.PhysicalAttempts, s.ArrivalAttempts, s.RetryPhysical)
		}
	}
}

// TestGoodputCountsRequestsNotAttempts: scenario E elevates the failure rate
// so physical attempts exceed logical requests while the request ledger is
// counted once per request; the summary exposes both never-conflated.
func TestGoodputCountsRequestsNotAttempts(t *testing.T) {
	s := RunExperiment(ScenarioE()).Summary
	if s.Offered != 29 || s.Admitted != 29 || s.PhysicalAttempts != 32 {
		t.Fatalf("expected the calibrated E volumes: offered=%d admitted=%d attempts=%d",
			s.Offered, s.Admitted, s.PhysicalAttempts)
	}
	if s.PhysicalAttempts <= s.Admitted {
		t.Fatalf("amplification regime broken: attempts %d <= admitted %d", s.PhysicalAttempts, s.Admitted)
	}
	if got := s.Amplification; math.Abs(got-32.0/29.0) > 1e-9 {
		t.Fatalf("amplification = %v, want exactly 32/29", got)
	}
	if s.RetryPhysical != s.RetryTokensConsumed {
		t.Fatalf("retry attempts %d != consumed budget tokens %d — every retry spends exactly one token",
			s.RetryPhysical, s.RetryTokensConsumed)
	}
	if s.RetriesPerRequest != 3.0/29.0 {
		t.Fatalf("retries-per-request = %v, want 3/29", s.RetriesPerRequest)
	}
}

// TestBlockedRetriesCountedAsAdmissionRejections: with the breaker open and
// all completions failing, scheduled retries that reach the admission gate
// are rejected (never queued, never counted as attempts) and recorded in
// BlockedRetries — a distinct category from attempts and ordinary sheds.
func TestBlockedRetriesCountedAsAdmissionRejections(t *testing.T) {
	fs := FaultSchedule{{Upstream: u1, Kind: FaultFailRate, EveryN: 1, From: 0, Until: 40 * msT}}
	res := RunExperiment(Experiment{
		Name:     "blocked-retries",
		Config:   faultRunConfig(),
		Workload: WorkloadSpec{Seed: 42, Components: []ArrivalComponent{comp(t1, u1, 1.0, 60*ms, 1)}},
		Faults:   fs,
		Horizon:  60 * msT,
	})
	s := res.Summary
	if s.BlockedRetries <= 0 {
		t.Fatalf("blocked retries = %d, want > 0 (breaker-open blocks pending retries)", s.BlockedRetries)
	}
	found := false
	for _, r := range res.Records {
		if r.Kind == "retry-admission" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no retry-admission record written despite BlockedRetries > 0")
	}
	// blocked retries are NOT attempts: every started retry consumed exactly
	// one token, so retries that DID start can never exceed consumed tokens
	if s.RetryPhysical > s.RetryTokensConsumed {
		t.Fatalf("retry attempts %d exceed consumed tokens %d", s.RetryPhysical, s.RetryTokensConsumed)
	}
	if !containsShedReason(s.Tenant[string(t1)]) {
		t.Fatalf("t1 shed reasons lack the breaker-open block reason: %v",
			s.Tenant[string(t1)].ShedReasons)
	}
}

func containsShedReason(ts TenantSummary) bool {
	for r := range ts.ShedReasons {
		if r == "retry-blocked-breaker-open" {
			return true
		}
	}
	return false
}

// TestWorkAfterExpiryDefinitionC3: scenario F expires queued requests while
// their in-flight attempts keep executing; those attempts land AFTER the
// request's expiry instant and are counted as executed work after expiry —
// they must never appear as goodput (the request is Expired, not Completed).
func TestWorkAfterExpiryDefinitionC3(t *testing.T) {
	res := RunExperiment(ScenarioF())
	s := res.Summary
	if s.Expired <= 0 {
		t.Fatalf("expected expired requests in D, got %d", s.Expired)
	}
	if s.WorkAfterExpiry <= 0 {
		t.Fatalf("work-after-expiry = %d, want > 0 (orphaned executions land after expiry)", s.WorkAfterExpiry)
	}
	// reconcile the C3 count against an independent sweep of the final state
	st := res.FinalState
	recount := 0
	for _, att := range st.Attempts {
		if !att.State.Terminal() || att.State == domain.AttemptRetracted {
			continue
		}
		req := st.Requests[att.LogicalID]
		if req != nil && req.State == domain.RequestExpired && att.FinishedAt >= req.TerminalAt {
			recount++
		}
	}
	if recount != s.WorkAfterExpiry {
		t.Fatalf("work-after-expiry summary %d != state recount %d", s.WorkAfterExpiry, recount)
	}
	// the expired requests' terminal instants are their deadlines: no
	// post-expiry attempt is EVER goodput (expired != completed)
	for _, req := range st.Requests {
		if req.State == domain.RequestExpired && req.TerminalAt != req.Deadline {
			t.Fatalf("request %s expired at %s, deadline %s", req.ID, req.TerminalAt, req.Deadline)
		}
	}
}

// TestPhasesPartitionReconcilesWithSummary: J's phases partition the whole
// horizon; summing event-window counts over them must equal the whole-run
// summary exactly (state-at-Until measures may double count by design, event
// counts may not).
func TestPhasesPartitionReconcilesWithSummary(t *testing.T) {
	res := RunExperiment(ScenarioJ())
	s := res.Summary
	var offered, admitted, attempts, retries, completed, failed, expired, shed int
	for _, ph := range res.Phases {
		p := ph.Report
		offered += p.Offered
		admitted += p.Admitted
		attempts += p.AttemptsCreated
		retries += p.RetryCreated
		completed += p.Completed
		failed += p.Failed
		expired += p.Expired
		shed += p.Shed
	}
	if len(res.Phases) == 0 {
		t.Fatal("J must define phases")
	}
	if offered != s.Offered || admitted != s.Admitted {
		t.Fatalf("request volume leaks across phases: offered %d/%d admitted %d/%d",
			offered, s.Offered, admitted, s.Admitted)
	}
	if attempts != s.PhysicalAttempts || retries != s.RetryPhysical {
		t.Fatalf("attempt volume leaks across phases: created %d/%d retry %d/%d",
			attempts, s.PhysicalAttempts, retries, s.RetryPhysical)
	}
	if completed != s.Completed || failed != s.Failed || expired != s.Expired || shed != s.Shed {
		t.Fatalf("terminal states leak across phases: completed %d/%d failed %d/%d expired %d/%d shed %d/%d",
			completed, s.Completed, failed, s.Failed, expired, s.Expired, shed, s.Shed)
	}
}

// TestDiscretePercentileConvention: Pq is the sample at 1-based rank
// ceil(q*N) with no interpolation; the empty aggregate is the zero value.
func TestDiscretePercentileConvention(t *testing.T) {
	empty := summarizeLatencies(nil)
	if empty.N != 0 || empty.Mean != 0 || empty.P50 != 0 {
		t.Fatalf("empty aggregate must be zero-valued: %+v", empty)
	}
	agg := summarizeLatencies([]domain.SimDuration{10, 20, 30})
	// ranks: P50 → ceil(1.5)=2 → 20; P90 → ceil(2.7)=3 → 30; P99 → 30
	if agg.N != 3 || agg.Mean != 20 || agg.P50 != 20 || agg.P90 != 30 || agg.P99 != 30 || agg.Max != 30 {
		t.Fatalf("percentile aggregate wrong: %+v", agg)
	}
	agg2 := summarizeLatencies([]domain.SimDuration{10, 20, 30, 40})
	// P50 → ceil(2)=2 → 20; P90 → ceil(3.6)=4 → 40
	if agg2.P50 != 20 || agg2.P90 != 40 {
		t.Fatalf("percentile ranks wrong: %+v", agg2)
	}
}

// TestOccConventions: piecewise-constant integration with carry-in, the
// occupancy-at-instant reader, and the NaN capacity convention on a synthetic
// timeline: counts 1@[0,5), 2@[5,10), 2@[10,30), 0@{30,∞).
func TestOccConventions(t *testing.T) {
	times := []domain.SimTime{0, 5 * msT, 10 * msT, 30 * msT}
	counts := []int{1, 2, 2, 0}

	// [0,30): 1*5 + 2*5 + 2*20 = 55 → avg 55/30, peak 2
	occ := integrateOcc(times, counts, 0, 30*msT)
	wantAvg := 55.0 / 30.0
	if occ.Window != 30*ms || math.Abs(occ.Avg-wantAvg) > 1e-9 || occ.Peak != 2 {
		t.Fatalf("integral wrong: %+v, want avg=%v peak=2", occ, wantAvg)
	}
	if !math.IsNaN(occ.Util) {
		t.Fatalf("Util without capacity must be NaN, got %v", occ.Util)
	}
	// carry-in regime [12,30): occupancy 2 in force at 12 → 2*18 = 36 → avg 2
	occ2 := integrateOcc(times, counts, 12*msT, 30*msT)
	if occ2.Window != 18*ms || occ2.Avg != 2 || occ2.Peak != 2 {
		t.Fatalf("carry-in integral wrong: %+v", occ2)
	}
	// the empty window is the zero value
	occ3 := integrateOcc(times, counts, 20*msT, 20*msT)
	if occ3.Window != 0 || occ3.Avg != 0 {
		t.Fatalf("empty window must be zero-valued: %+v", occ3)
	}
	if v := occAtUntil(times, counts, 7*msT); v != 2 {
		t.Fatalf("occAtUntil(7ms) = %d, want 2", v)
	}
	if v := occAtUntil(times, counts, 30*msT); v != 0 {
		t.Fatalf("occAtUntil(30ms) = %d, want 0 (the 30ms sample is in force)", v)
	}
}

// TestWorkerBusyAtUntilMatchesOpenHolds: the worker timeline's state-at-Until
// reader and the attempts' open-hold bookkeeping must agree at the horizon
// instant (the timeline is appended at the same instants the hold opens).
func TestWorkerBusyAtUntilMatchesOpenHolds(t *testing.T) {
	res := RunExperiment(ScenarioJ())
	st := res.FinalState
	wTimes := make([]domain.SimTime, len(st.WorkerOccupancy))
	wCounts := make([]int, len(st.WorkerOccupancy))
	for i, w := range st.WorkerOccupancy {
		wTimes[i], wCounts[i] = w.At, w.Busy
	}
	timelineBusy := occAtUntil(wTimes, wCounts, res.Experiment.Horizon)
	open := 0
	for _, att := range st.Attempts {
		if att.WorkerSet {
			open++
		}
	}
	if timelineBusy != open {
		t.Fatalf("worker busy at horizon %d != open holds %d", timelineBusy, open)
	}
}

// TestRecordedInputsMatchTheWorkloadBuild: for the fault-free control A the
// recorded initial events ARE the workload build; the horizon caps them.
func TestRecordedInputsMatchTheWorkloadBuild(t *testing.T) {
	res := RunExperiment(ScenarioA())
	built := res.Experiment.Workload.Build(res.Experiment.Horizon)
	if len(res.InitEvents) != len(built) {
		t.Fatalf("recorded init events %d != workload build %d", len(res.InitEvents), len(built))
	}
	for i := range built {
		ra, okA := res.InitEvents[i].(*EventRequestArrival)
		rb, okB := built[i].(*EventRequestArrival)
		if !okA || !okB || ra.At() != rb.At() || ra.Deadline != rb.Deadline {
			t.Fatalf("recorded input diverges from the workload build at %d", i)
		}
	}
}

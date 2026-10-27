package sim

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"sentra/domain"
	"sentra/engine"
)

// remediation_test.go holds the Milestone-1.1 regression suite: every defect
// found by the adversarial review has exactly one demonstrating test here.
// Scenario comments state the pre-fix (defective) behaviour so future refactors
// cannot silently regress the semantics.

const long = 10000 * msT // effectively "no deadline" within run bounds

// --- F-priority-1.1: centralized retraction cleanup removes waitlist members
// AND their pinned workers (pre-fix: worker stranded, waitlist member leak). ---

func TestCancellationRetractsWaitingConnectionAttempt(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	cfg.ConnectionPools[u1] = 1 // force the second attempt to pin waiting
	s := mustNew(t, cfg)
	events := append(arrivals(0, 2, t1, u1, long, 0),
		NewCancellationEvent(1*msT, "req-2", "cancel while pinned for connection"))
	run(t, s, events, 10*msT)

	req2 := requestOf(t, s, 2)
	a2 := attemptOf(t, s, 2)
	if req2.State != domain.RequestCancelled {
		t.Fatalf("req2 = %s, want cancelled", req2.State)
	}
	if a2.State != domain.AttemptRetracted || a2.Outcome != domain.OutcomeDiscarded {
		t.Fatalf("a2 = %s/%s, want retracted/discarded", a2.State, a2.Outcome)
	}
	if a2.HasWorker || a2.Worker != "" {
		t.Fatalf("retraction must release the pinned worker (HasWorker=%v worker=%s)", a2.HasWorker, a2.Worker)
	}
	st := s.State()
	if st.Pools[cfg.WorkerPool.ID].IdleCount() != 2 {
		t.Fatalf("idle workers = %d after release, want 2", st.Pools[cfg.WorkerPool.ID].IdleCount())
	}
	if st.Connections[u1].Waiting() != 0 {
		t.Fatalf("waitlist holds %d entries after retraction, want 0", st.Connections[u1].Waiting())
	}
}

func TestDeadlineRetractsWaitingConnectionAttempt(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	cfg.ConnectionPools[u1] = 1
	s := mustNew(t, cfg)
	// Both deadlines at 1ms: the executing attempt is orphaned; the waiting
	// attempt is retracted before it ever runs.
	run(t, s, arrivals(0, 2, t1, u1, 1*msT, 0), 10*msT)

	req1, req2 := requestOf(t, s, 1), requestOf(t, s, 2)
	a1, a2 := attemptOf(t, s, 1), attemptOf(t, s, 2)
	if req1.State != domain.RequestExpired || req2.State != domain.RequestExpired {
		t.Fatalf("requests = %s/%s, want expired/expired", req1.State, req2.State)
	}
	if a1.State != domain.AttemptCompleted || a1.Outcome != domain.OutcomeWasted {
		t.Fatalf("a1 = %s/%s, want completed/wasted (orphan runs out)", a1.State, a1.Outcome)
	}
	if a2.State != domain.AttemptRetracted || a2.Outcome != domain.OutcomeDiscarded {
		t.Fatalf("a2 = %s/%s, want retracted/discarded", a2.State, a2.Outcome)
	}
	if a2.HasWorker {
		t.Fatal("deadline retraction must release the pinned worker")
	}
	st := s.State()
	if st.Connections[u1].InUse() != 0 || st.Connections[u1].Waiting() != 0 {
		t.Fatal("connection pool and waitlist must drain fully")
	}
}

// --- probe permit ledger: retraction of a queued probe restores the permit
// (pre-fix: permit leaked with the attempt, the next probe was shed). ---------

func TestHalfOpenProbeExpiryRecoversPermit(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 1, Cooldown: 10 * ms, MaxHalfOpenProbes: 1}
	up2 := cfg.Upstreams[u2]
	up2.NormalLatency = 15 * ms
	cfg.Upstreams[u2] = up2
	s := mustNew(t, cfg)
	initial := []engine.Event{
		NewUpstreamDegradedEvent(0, u1),
		NewArrivalEvent(0, t1, u1, long, 0),        // fails at 20ms -> open -> half-open at 30ms
		NewArrivalEvent(30*msT, t1, u2, long, 0),   // filler occupies the worker 30-45ms
		NewArrivalEvent(31*msT, t1, u1, 33*msT, 0), // P: probe reserved, queued
		NewArrivalEvent(35*msT, t1, u1, long, 0),   // Q: must NOT be shed
	}
	run(t, s, initial, 36*msT)

	br := s.State().Breakers[u1]
	if br.State != domain.BreakerHalfOpen {
		t.Fatalf("breaker = %s, want still half-open at 36ms", br.State)
	}
	if br.ProbesActive != 1 {
		t.Fatalf("ProbesActive = %d, want 1 (P's permit recovered at expiry, Q holds a fresh permit)", br.ProbesActive)
	}
	// P was expired while queued; its attempt must be retracted and its probe
	// flag cleared by the retraction path itself.
	reqP := requestOf(t, s, 3) // P is the third arrival
	p := s.State().Attempts[reqP.Attempts[0]]
	if p == nil || p.State != domain.AttemptRetracted || p.Outcome != domain.OutcomeDiscarded {
		t.Fatalf("P attempt = %+v, want retracted/discarded", p)
	}
	if p.IsProbe {
		t.Fatal("retracted probe attempt must not still hold IsProbe")
	}
	// Q must have been admitted (not shed) at 35ms.
	q := requestOf(t, s, 4)
	if q.State != domain.RequestQueued {
		t.Fatalf("Q state = %s, want admitted and queued", q.State)
	}
	if sheds := s.State().Metrics[t1].Sheds; len(sheds) != 0 {
		t.Fatalf("sheds = %v, want none (pre-fix: P's permit leaked and Q was shed)", sheds)
	}
}

// --- same-instant connection-grant race (pre-fix: both grants named the same
// waitlist head; the second waitlist member could never run). -----------------

func TestTwoSimultaneousConnectionReleasesGrantBothWaiters(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 4
	cfg.ConnectionPools[u1] = 2
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	cfg.Upstreams[u1] = up
	s := mustNew(t, cfg)
	run(t, s, arrivals(0, 4, t1, u1, long, 0), 20*msT)

	a3, a4 := attemptOf(t, s, 3), attemptOf(t, s, 4)
	if a3.BeganRunningAt != 5*msT || a4.BeganRunningAt != 5*msT {
		t.Fatalf("a3 ran at %s, a4 at %s — both waiters must be granted at the release instant 5ms", a3.BeganRunningAt, a4.BeganRunningAt)
	}
	checkAllCompleted(t, s, 4)
	st := s.State()
	if st.Connections[u1].InUse() != 0 || st.Connections[u1].Waiting() != 0 {
		t.Fatal("pool must drain after both grants complete")
	}
}

// outcome check helper (IsUseful kept separate so the list above stays short)
func checkAllCompleted(t *testing.T, s *Sim, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		a := attemptOf(t, s, i)
		if a.State != domain.AttemptCompleted || a.Outcome != domain.OutcomeUseful {
			t.Fatalf("att-%d = %s/%s, want completed/useful", i, a.State, a.Outcome)
		}
	}
}

// --- stale waitlist head cannot strand capacity (pre-fix: a wake event named
// the head at RELEASE time; if that waiter timed out first, the connection
// went unused while a *later* waiter stayed pinned forever). ------------------

func TestStaleWakeSelectsOldestEligibleWaiterAtProcessingTime(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 3
	cfg.ConnectionPools[u1] = 1
	cfg.AttemptTimeout = 5 * ms
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	cfg.Upstreams[u1] = up
	s := mustNew(t, cfg)
	events := append(arrivals(0, 2, t1, u1, long, 0), NewArrivalEvent(1*msT, t1, u1, long, 0))
	run(t, s, events, 15*msT)

	// a1 (holder) hits timeout and completion both at 5ms; timeout (prio 20)
	// releases the connection first. a2 (head of waitlist, timeout at 5ms)
	// retracts at 5ms before the wake event runs. The payload-free wake must
	// then hand the connection to a3 instead of skipping/stranding. a3's own
	// timeout (armed at 1ms) expires at 6ms — it starts running first.
	a2, a3 := attemptOf(t, s, 2), attemptOf(t, s, 3)
	if a2.State != domain.AttemptTimedOut {
		t.Fatalf("a2 = %s, want timed-out (its eligible window closed exactly at the wake)", a2.State)
	}
	if a3.BeganRunningAt != 5*msT {
		t.Fatalf("a3 began running at %s, want 5ms (wake re-selected the oldest eligible waiter)", a3.BeganRunningAt)
	}
	if a3.State != domain.AttemptTimedOut || a3.Outcome != domain.OutcomeUseful {
		t.Fatalf("a3 = %s/%s, want timed-out at 6ms/useful (handover happened at 5ms)", a3.State, a3.Outcome)
	}
	st := s.State()
	if st.Connections[u1].InUse() != 0 || st.Connections[u1].Waiting() != 0 {
		t.Fatal("no capacity may be stranded by the stale head")
	}
}

// --- deadline == arrival: immediate expiration, never a panic -----------------

func TestDeadlineEqualsArrivalExpiresImmediately(t *testing.T) {
	s := mustNew(t, baseCfg())
	run(t, s, []engine.Event{NewArrivalEvent(0, t1, u1, 0, 0)}, 10*msT)

	req := requestOf(t, s, 1)
	if req.State != domain.RequestExpired {
		t.Fatalf("req1 = %s, want expired at admission", req.State)
	}
	if len(req.Attempts) != 0 {
		t.Fatalf("expired-at-arrival request created attempts: %v", req.Attempts)
	}
	if s.State().TenantActive[t1] != 0 {
		t.Fatalf("tenant active = %d, want 0", s.State().TenantActive[t1])
	}
	var sawAttemptCreated, sawDeadlineExpired, sawAdmission bool
	var admReason string
	for _, rec := range s.Recorder().Records() {
		switch {
		case rec.Kind == "attempt-created":
			sawAttemptCreated = true
		case rec.Kind == "DeadlineExpired":
			sawDeadlineExpired = true
		case rec.Kind == "AdmissionDecided":
			sawAdmission = true
			admReason = rec.Detail.Reason
		}
	}
	if sawAttemptCreated || sawDeadlineExpired {
		t.Fatal("deadline==arrival must not create attempts nor schedule DeadlineExpired")
	}
	if !sawAdmission || admReason != "deadline-at-arrival" {
		t.Fatalf("admission record reason = %q, want deadline-at-arrival", admReason)
	}
}

// --- cancellation of unknown requests: deterministic recorded no-op ----------

func TestCancellationOfUnknownRequestIsRecordedNoOp(t *testing.T) {
	s := mustNew(t, baseCfg())
	run(t, s, []engine.Event{NewCancellationEvent(1*msT, "req-99", "ghost cancel")}, 10*msT)

	if n := len(s.State().Requests); n != 0 {
		t.Fatalf("state must not acquire the unknown request (requests=%d)", n)
	}
	found := false
	for _, rec := range s.Recorder().Records() {
		if rec.Kind == "cancellation" && strings.Contains(rec.Detail.Description, "deterministic no-op") {
			found = true
		}
	}
	if !found {
		t.Fatal("unknown-request cancellation must be recorded as a deterministic no-op")
	}
}

// --- retries cannot execute against an OPEN breaker --------------------------

func TestRetryBlockedWhenBreakerOpen(t *testing.T) {
	cfg := baseCfg()
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 1, Cooldown: 20 * ms, MaxHalfOpenProbes: 1}
	cfg.RetryBackoff = 2 * ms
	s := mustNew(t, cfg)
	initial := []engine.Event{NewUpstreamDegradedEvent(0, u1)}
	initial = append(initial, NewArrivalEvent(0, t1, u1, long, 3))
	run(t, s, initial, 30*msT)

	req := requestOf(t, s, 1)
	if req.State != domain.RequestFailed || req.RetriesUsed != 1 {
		t.Fatalf("req1 = %s with %d retries used, want failed after exactly 1 consumed retry", req.State, req.RetriesUsed)
	}
	if n := len(req.Attempts); n != 1 {
		t.Fatalf("attempts = %d, want 1 (the retry attempt was never created)", n)
	}
	if tok := s.State().Budgets[domain.BudgetKey{Tenant: t1, Upstream: u1}].Tokens; tok != 1 {
		t.Fatalf("budget tokens = %d, want 1 (the spent token is NOT refunded)", tok)
	}
	if sheds := s.State().Metrics[t1].Sheds["retry-blocked-breaker-open"]; sheds != 1 {
		t.Fatalf("recorded outcome sheds[retry-blocked-breaker-open] = %d, want 1", sheds)
	}
}

// --- retries cannot run as HALF_OPEN probes ----------------------------------

func TestRetryBlockedWhenBreakerHalfOpen(t *testing.T) {
	cfg := baseCfg()
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 1, Cooldown: 10 * ms, MaxHalfOpenProbes: 1}
	cfg.RetryBackoff = 12 * ms // retry would land at 32ms, mid half-open
	s := mustNew(t, cfg)
	initial := []engine.Event{NewUpstreamDegradedEvent(0, u1)}
	initial = append(initial, NewArrivalEvent(0, t1, u1, long, 3))
	run(t, s, initial, 40*msT)

	req := requestOf(t, s, 1)
	if req.State != domain.RequestFailed || req.RetriesUsed != 1 {
		t.Fatalf("req1 = %s with %d retries, want failed with the blocked retry recorded", req.State, req.RetriesUsed)
	}
	if n := len(req.Attempts); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
	br := s.State().Breakers[u1]
	if br.ProbesActive != 0 {
		t.Fatalf("ProbesActive = %d, want 0 (retries must not consume probe permits)", br.ProbesActive)
	}
	if sheds := s.State().Metrics[t1].Sheds["retry-blocked-breaker-halfopen"]; sheds != 1 {
		t.Fatalf("recorded sheds[retry-blocked-breaker-halfopen] = %d, want 1", sheds)
	}
}

// --- retry lane bound: the second same-instant retry admission is blocked
// (pre-fix both admissions ran while the queue was empty and the lane
// silently overflowed). --------------------------------------------------------

func sameInstantRetryPairRun(t *testing.T, maxArr, maxRetry, maxTotal int) *Sim {
	t.Helper()
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: maxArr, MaxRetryQueueDepth: maxRetry, MaxTotalQueueDepth: maxTotal}
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 5, Cooldown: 50 * ms, MaxHalfOpenProbes: 1}
	cfg.RetryBackoff = 2 * ms
	cfg.RetryBudget = RetryBudgetConfig{Capacity: 5, RefillInterval: 100 * ms}
	s := mustNew(t, cfg)
	events := []engine.Event{NewUpstreamDegradedEvent(0, u1)}
	events = append(events, arrivals(0, 2, t1, u1, long, 3)...)
	run(t, s, events, 30*msT)
	return s
}

func TestRetryLaneBoundBlocksSecondSameInstantRetry(t *testing.T) {
	s := sameInstantRetryPairRun(t, 8, 1, 8)
	req1, req2 := requestOf(t, s, 1), requestOf(t, s, 2)

	if req2.State != domain.RequestFailed || req2.RetriesUsed != 1 {
		t.Fatalf("req2 = %s with %d retries, want failed at the blocked retry", req2.State, req2.RetriesUsed)
	}
	if sheds := s.State().Metrics[t1].Sheds["retry-queue-full"]; sheds != 1 {
		t.Fatalf("recorded sheds[retry-queue-full] = %d, want 1", sheds)
	}
	a3 := s.State().Attempts[domain.AttemptID("att-3")]
	if a3 == nil || !a3.IsRetry {
		t.Fatalf("req1's retry attempt missing or not marked IsRetry: %+v", a3)
	}
	if req1.State != domain.RequestDispatched {
		t.Fatalf("req1 = %s, want its retry admitted (dispatched within the lane)", req1.State)
	}
	retryLane := 0
	for _, e := range s.State().Queues[t1].Entries {
		att := s.State().Attempts[e.Attempt]
		if att.IsRetry {
			retryLane++
		}
	}
	if retryLane > 1 {
		t.Fatalf("retry lane holds %d entries, max 1", retryLane)
	}
}

func TestTotalQueueBoundBlocksRetryEvenWithRetryLaneRoom(t *testing.T) {
	// t1 lanes (2,2,2): two same-instant arrivals fill and dispatch; the two
	// long u2 fillers re-occupy both workers at 20ms so the retries queue.
	// The third t1 request keeps the total book at 1 when retry req-1's
	// attempt enters the book; req-2's retry admission must then be refused
	// by the TOTAL bound even though the retry lane has room.
	cfg := baseCfg()
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	up2 := cfg.Upstreams[u2]
	up2.NormalLatency = 60 * ms
	cfg.Upstreams[u2] = up2
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 2, MaxRetryQueueDepth: 2, MaxTotalQueueDepth: 2}
	cfg.Tenants[t2] = TenantConfig{MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8}
	cfg.Breaker = BreakerConfig{FailureThreshold: 5, Cooldown: 50 * ms, MaxHalfOpenProbes: 1}
	cfg.RetryBackoff = 2 * ms
	cfg.RetryBudget = RetryBudgetConfig{Capacity: 5, RefillInterval: 100 * ms}
	s := mustNew(t, cfg)
	initial := []engine.Event{
		NewUpstreamDegradedEvent(0, u1),
		NewArrivalEvent(0, t1, u1, long, 3),     // req1
		NewArrivalEvent(0, t1, u1, long, 3),     // req2; fails at 20ms
		NewArrivalEvent(1*msT, t2, u2, long, 0), // filler req3
		NewArrivalEvent(2*msT, t2, u2, long, 0), // filler req4
		NewArrivalEvent(3*msT, t1, u1, long, 0), // req5: total book 1 while retries land at 22ms
	}
	run(t, s, initial, 30*msT)

	req2 := requestOf(t, s, 2)
	if req2.State != domain.RequestFailed {
		t.Fatalf("req2 = %s, want failed at the blocked retry", req2.State)
	}
	if sheds := s.State().Metrics[t1].Sheds["retry-queue-total-full"]; sheds != 1 {
		t.Fatalf("recorded sheds[retry-queue-total-full] = %d, want 1", sheds)
	}
	// req1's retry must be queued behind the occupied total book as expected.
	req1 := requestOf(t, s, 1)
	if req1.State != domain.RequestQueued {
		t.Fatalf("req1 = %s, want queued (its retry waits on the occupied total book)", req1.State)
	}
}

// --- zero backoff is a valid boundary; same-instant chains stay bounded and
// deterministic (pre-fix zero backoff produced degenerate behaviour). ---------

func zeroBackoffSystem(t *testing.T) *Sim {
	t.Helper()
	cfg := baseCfg()
	up := cfg.Upstreams[u1]
	up.NormalLatency = 1 * ms
	up.DegradedLatency = 0 // zero-duration degradation: entire chain collapses to t=0
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 10, Cooldown: 50 * ms, MaxHalfOpenProbes: 1}
	cfg.RetryBackoff = 0
	cfg.RetryBudget = RetryBudgetConfig{Capacity: 2, RefillInterval: 100 * ms}
	s := mustNew(t, cfg)
	events := []engine.Event{NewUpstreamDegradedEvent(0, u1)}
	events = append(events, NewArrivalEvent(0, t1, u1, long, 5))
	run(t, s, events, 30*msT)
	return s
}

func TestZeroBackoffSameInstantRetryChainIsBoundedAndDeterministic(t *testing.T) {
	s := zeroBackoffSystem(t)

	req := requestOf(t, s, 1)
	if req.State != domain.RequestFailed || req.RetriesUsed != 2 {
		t.Fatalf("req1 = %s with %d retries, want failed after budget capacity 2", req.State, req.RetriesUsed)
	}
	if n := len(req.Attempts); n != 3 {
		t.Fatalf("attempts = %d, want exactly 3 (initial + 2 budget-backed retries)", n)
	}
	for _, rec := range s.Recorder().Records() {
		if rec.Kind == "request-transition" && rec.Detail.To == "failed" &&
			strings.Contains(rec.Detail.Description, "retry budget exhausted") {
			return // bounded as expected
		}
	}
	t.Fatal("request must fail with the retry-budget-exhausted reason")

}

func TestZeroBackoffChainRunsAreIdentical(t *testing.T) {
	s1 := zeroBackoffSystem(t)
	s2 := zeroBackoffSystem(t)
	if !reflect.DeepEqual(s1.State(), s2.State()) {
		t.Fatal("zero-backoff chain must be deterministic in state")
	}
	if !reflect.DeepEqual(s1.Recorder().Records(), s2.Recorder().Records()) {
		t.Fatal("zero-backoff chain must be deterministic in its records")
	}
}

// --- checked simulated-time arithmetic with a distinct overflow error --------

func TestSimulatedTimeOverflowHasDistinctError(t *testing.T) {
	cfg := baseCfg()
	cfg.AttemptTimeout = domain.SimDuration(math.MaxInt64)
	s := mustNew(t, cfg)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("overflow scheduling must panic")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "simulated time overflow") {
			t.Fatalf("panic = %q, want the distinct overflow message", msg)
		}
		if strings.Contains(msg, "behind current time") || strings.Contains(msg, "ill") {
			t.Fatalf("overflow must not be reported by the generic past-scheduling message: %q", msg)
		}
	}()
	e := engine.New(s, arrivals(1*msT, 1, t1, u1, long, 0))
	e.Run(5 * msT)
}

// --- invariant ledger consistency is actually enforced ------------------------

func TestValidateDetectsProbeAndLedgerCorruption(t *testing.T) {
	cfg := baseCfg()
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	up.DegradedLatency = 20 * ms
	cfg.Upstreams[u1] = up
	cfg.Breaker = BreakerConfig{FailureThreshold: 1, Cooldown: 10 * ms, MaxHalfOpenProbes: 1}
	s := mustNew(t, cfg)
	initial := []engine.Event{
		NewUpstreamDegradedEvent(0, u1),
		NewArrivalEvent(0, t1, u1, long, 0),      // opens the breaker at 20ms
		NewArrivalEvent(31*msT, t1, u1, long, 0), // probe runs 31-51ms
	}
	run(t, s, initial, 40*msT)

	// snapshot the healthy probe-ledger state
	st := s.State()
	if got := st.Breakers[u1].ProbesActive; got != 1 {
		t.Fatalf("setup: ProbesActive = %d, want 1", got)
	}

	st.Breakers[u1].ProbesActive = 2
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "probe ledger mismatch") {
		t.Fatalf("validator must catch probe counter inflation: %v", err)
	}
	st.Breakers[u1].ProbesActive = 1

	st.Attempts[domain.AttemptID("att-2")].IsProbe = false
	if got := st.Breakers[u1].ProbesActive; got != 1 {
		t.Fatalf("setup: ProbesActive expected still 1, got %d", got)
	}
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "probe ledger mismatch") {
		t.Fatalf("validator must catch cleared IsProbe with permit reserved: %v", err)
	}
	st.Attempts[domain.AttemptID("att-2")].IsProbe = true

	c := st.OutcomeCounters[t1]
	c.Wasted++
	st.OutcomeCounters[t1] = c
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "outcome counters") {
		t.Fatalf("validator must catch outcome-counter drift: %v", err)
	}
	c.Wasted--
	st.OutcomeCounters[t1] = c
	if err := st.Validate(); err != nil {
		t.Fatalf("healthy state must validate: %v", err)
	}
}

func TestValidateDetectsWaitlistAndPendingLedgerCorruption(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	cfg.ConnectionPools[u1] = 1
	s := mustNew(t, cfg)
	events := arrivals(0, 2, t1, u1, long, 0)
	e := engine.New(s, events)
	e.Run(1 * msT) // a2 pinned waiting for the connection

	st := s.State()
	if st.Connections[u1].Waiting() != 1 {
		t.Fatalf("setup: waitlist = %d, want 1", st.Connections[u1].Waiting())
	}
	st.Connections[u1].Waitlist = append(st.Connections[u1].Waitlist, "att-ghost")
	if err := st.Validate(); err == nil || !strings.Contains(err.Error(), "unknown waitlisted attempt") {
		t.Fatalf("validator must catch ghost waitlist entries: %v", err)
	}
	st.Connections[u1].Waitlist = st.Connections[u1].Waitlist[:1]

	if err := st.Validate(); err != nil {
		t.Fatalf("healthy waitlist must validate: %v", err)
	}
}

// --- fairness metrics (P3.17): per-tenant queue-wait distribution, dispatch
// share, shed-reason mix, useful-work share and the Jain index. ---------------

func TestPerTenantQueueWaitDistributionAndDispatchShare(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	up := cfg.Upstreams[u1]
	up.NormalLatency = 50 * ms
	cfg.Upstreams[u1] = up
	cfg.AttemptTimeout = 200 * ms // the 25ms default would time the holder out early
	s := mustNew(t, cfg)
	events := []engine.Event{
		NewArrivalEvent(0, t1, u1, long, 0),     // runs 0-50ms
		NewArrivalEvent(1*msT, t2, u2, long, 0), // queues behind t1; dispatch share 0 until 50
		NewArrivalEvent(2*msT, t2, u2, long, 0), // still queued
	}
	e := engine.New(s, events)
	e.Run(20 * msT)
	if err := s.State().Validate(); err != nil {
		t.Fatalf("mid-run invariants: %v", err)
	}
	mm := s.State().Metrics
	if mm[t2].QueueWaitN != 0 || mm[t2].Dispatched != 0 || mm[t2].QueueWaitSum != 0 {
		t.Fatalf("starved tenant must show zero dispatch share mid-run: %+v", mm[t2])
	}
	if mm[t2].QueueWaitMax != 0 {
		t.Fatalf("no wait without dispatch: queue-wait max = %s", mm[t2].QueueWaitMax)
	}

	e.Run(55 * msT) // t1's worker frees at 50ms; the first t2 attempt dispatches
	mm = s.State().Metrics
	if mm[t2].Dispatched != 1 || mm[t2].QueueWaitN != 1 {
		t.Fatalf("t2 dispatch records: %+v, want wait N=1, dispatched=1", mm[t2])
	}
	if got, want := mm[t2].QueueWaitMax, 49*ms; got != want {
		t.Fatalf("t2 queue-wait max = %s, want %s", got, want)
	}
	if mm[t1].QueueWaitMax != 0 {
		t.Fatalf("t1 dispatched immediately: max wait = %s, want 0", mm[t1].QueueWaitMax)
	}
	if mm[t2].QueueWaitHist[26] != 1 {
		t.Fatalf("49ms wait must land in the log2 histogram: %v", mm[t2].QueueWaitHist)
	}
	if mm[t1].Dispatched != 1 || mm[t1].QueueWaitN != 1 {
		t.Fatalf("t1 wait records: %+v", mm[t1])
	}
}

func TestShedReasonsAreRecordedPerTenant(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1 // keeps the single queued attempt in its lane
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 1, MaxRetryQueueDepth: 1, MaxTotalQueueDepth: 1}
	s := mustNew(t, cfg)
	// At 0ms both workers take arrivals. req2 arrives at 1ms while the lane
	// is free (its attempt queues); at 2ms req3 arrives with the lane still
	// occupied by the queued attempt and must shed at admission.
	initial := []engine.Event{
		NewArrivalEvent(0, t1, u1, long, 0),     // dispatched immediately
		NewArrivalEvent(1*msT, t1, u1, long, 0), // queued (lane has room once)
		NewArrivalEvent(2*msT, t1, u1, long, 0), // lane occupied → shed
	}
	run(t, s, initial, 10*msT)

	sheds := s.State().Metrics[t1].Sheds
	if sheds["tenant-queue-full"] != 1 {
		t.Fatalf("sheds = %v, want exactly one tenant-queue-full shed", sheds)
	}
	if r3 := requestOf(t, s, 3); r3.State != domain.RequestShed {
		t.Fatalf("req3 = %s, want shed at admission", r3.State)
	}
	if r2 := requestOf(t, s, 2); r2.State != domain.RequestCompleted {
		t.Fatalf("req2 = %s, want completed (queued at 1ms, dispatched at 5ms, served)", r2.State)
	}
	// t2 must not inherit t1's sheds.
	if sheds2 := s.State().Metrics[t2].Sheds; len(sheds2) != 0 {
		t.Fatalf("t2 sheds = %v, want none", sheds2)
	}
}

func TestUsefulShareAndJainIndex(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	cfg.Upstreams[u1] = up
	s := mustNew(t, cfg)
	events := []engine.Event{
		NewArrivalEvent(0, t1, u1, long, 0),
		NewArrivalEvent(1*msT, t2, u2, long, 0),
	}
	run(t, s, events, 15*msT)

	snap := s.State().FairnessSnapshot()
	if len(snap.Tenants) != 2 || snap.Tenants[0].Tenant != t1 || snap.Tenants[1].Tenant != t2 {
		t.Fatalf("snapshot must list tenants sorted by ID: %+v", snap.Tenants)
	}
	useful := s.State().UsefulOutcomeCounts()
	if useful[0] < 1 {
		t.Fatalf("t1 useful = %d, want >=1", useful[0])
	}
	if JainIndex([]int{}) == JainIndex([]int{}) {
		t.Fatal("empty vector must be NaN")
	}
	if z := JainIndex([]int{0, 0}); !math.IsNaN(z) {
		t.Fatalf("all-zero vector must be NaN, got %f", z)
	}
	if got := JainIndex([]int{1, 0}); got != 0.5 {
		t.Fatalf("jain([1,0]) = %f, want 0.5", got)
	}
	if got := JainIndex([]int{1, 1}); got != 1 {
		t.Fatalf("jain([1,1]) = %f, want 1", got)
	}
}

// --- P2.11: exhaustive event-priority classification -------------------------

// allSampleEvents returns one instance of every sim event kind (20 total,
// 18 from M1 plus the 2 M2 fault-injection kinds), used to classify Priority()
// values exactly once per kind.
func allSampleEvents() []Event {
	return []Event{
		&EventRequestArrival{baseEvent: baseEvent{Time: 0}},
		&EventAdmissionDecided{baseEvent: baseEvent{Time: 0}},
		&EventQueueInsertion{baseEvent: baseEvent{Time: 0}},
		&EventQueueRemoval{baseEvent: baseEvent{Time: 0}},
		&EventWorkerAssignment{baseEvent: baseEvent{Time: 0}},
		&EventExecutionStart{baseEvent: baseEvent{Time: 0}},
		&EventExecutionCompletion{baseEvent: baseEvent{Time: 0}},
		&EventTimeout{baseEvent: baseEvent{Time: 0}},
		&EventCancellationRequested{baseEvent: baseEvent{Time: 0}},
		&EventCancellationAcknowledged{baseEvent: baseEvent{Time: 0}},
		&EventRetryScheduled{baseEvent: baseEvent{Time: 0}},
		&EventRetryExecution{baseEvent: baseEvent{Time: 0}},
		&EventDeadlineExpired{baseEvent: baseEvent{Time: 0}},
		&EventUpstreamDegraded{baseEvent: baseEvent{Time: 0}},
		&EventUpstreamRecovered{baseEvent: baseEvent{Time: 0}},
		&EventBreakerTransitioned{baseEvent: baseEvent{Time: 0}},
		&EventLoadShed{baseEvent: baseEvent{Time: 0}},
		&EventConnectionGranted{baseEvent: baseEvent{Time: 0}},
		// M2 fault-injection kinds (workstream 2):
		&EventUpstreamLatencySet{baseEvent: baseEvent{Time: 0}},
		&EventUpstreamFailureSet{baseEvent: baseEvent{Time: 0}},
	}
}

func TestEventPriorityClassesAreExhaustive(t *testing.T) {
	samples := allSampleEvents()
	seenKinds := make(map[Kind]bool, len(samples))
	counts := map[int]int{}
	for _, ev := range samples {
		k := ev.Kind()
		if seenKinds[k] {
			t.Fatalf("kind %s sampled twice", k)
		}
		seenKinds[k] = true
		counts[ev.Priority()]++
	}

	// every declared kind must be covered — no silent new kind
	if len(seenKinds) != len(kindNames) {
		t.Fatalf("kinds sampled = %d, kindNames = %d", len(seenKinds), len(kindNames))
	}
	if len(kindNames) != 20 {
		t.Fatalf("taxonomy size = %d, want 20", len(kindNames))
	}

	// classification: control=0, governance=10, timeout=20, work=30.
	// counts: 5 control (degraded/recovered/breaker + M2 latency/failure
	// set), 3 governance (cancel requested/acknowledged, deadline),
	// 1 timeout, 11 work.
	if counts[PriorityModelControl] != 5 || counts[PriorityGovernance] != 3 ||
		counts[PriorityTimeout] != 1 || counts[PriorityWork] != 11 {
		t.Fatalf("class distribution = %v, want control 5 / governance 3 / timeout 1 / work 11", counts)
	}
	for _, p := range []int{PriorityModelControl, PriorityGovernance, PriorityTimeout, PriorityWork} {
		if _, ok := counts[p]; !ok {
			t.Fatalf("priority class %d has no members", p)
		}
	}
	// constants are distinct and ordered
	if !(PriorityModelControl < PriorityGovernance && PriorityGovernance < PriorityTimeout && PriorityTimeout < PriorityWork) {
		t.Fatal("priority constants must be strictly ordered")
	}
}

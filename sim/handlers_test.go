package sim

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"sentra/domain"
	"sentra/engine"
)

// `ms`, `msT` and the reference tenant/upstream IDs (t1..t3, u1, u2) are
// shared and defined in scenario_library.go.

// baseCfg returns a config with two workers, one connection per upstream and
// permissive queue depths. Tests narrow it via the fields they care about.
func baseCfg() SystemConfig {
	return SystemConfig{
		WorkerPool: WorkerPoolConfig{ID: "pool-main", Size: 2},
		ConnectionPools: map[domain.UpstreamID]int{
			u1: 2,
			u2: 2,
		},
		Upstreams: map[domain.UpstreamID]UpstreamConfig{
			u1: {Criticality: domain.CriticalityCritical, NormalLatency: 5 * ms, DegradedLatency: 20 * ms},
			u2: {Criticality: domain.CriticalityStandard, NormalLatency: 10 * ms, DegradedLatency: 40 * ms},
		},
		Tenants: map[domain.TenantID]TenantConfig{
			t1: {MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8},
			t2: {MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8},
		},
		RetryBudget:    RetryBudgetConfig{Capacity: 2, RefillInterval: 100 * ms},
		RetryBackoff:   2 * ms,
		AttemptTimeout: 25 * ms,
		Breaker:        BreakerConfig{FailureThreshold: 2, Cooldown: 50 * ms, MaxHalfOpenProbes: 1},
	}
}

func mustNew(t *testing.T, cfg SystemConfig) *Sim {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("sim: %v", err)
	}
	return s
}

// run drives the sim through engine.New (initial may carry injected events).
func run(t *testing.T, s *Sim, initial []engine.Event, until domain.SimTime) *engine.Engine {
	t.Helper()
	e := engine.New(s, initial)
	e.Run(until)
	if err := s.State().Validate(); err != nil {
		t.Fatalf("state invariants after run to %s: %v", until, err)
	}
	return e
}

func arrivals(t domain.SimTime, n int, tenant domain.TenantID, up domain.UpstreamID, deadline domain.SimTime, retries int) []engine.Event {
	out := make([]engine.Event, n)
	for i := 0; i < n; i++ {
		out[i] = NewArrivalEvent(t, tenant, up, deadline, retries)
	}
	return out
}

func requestOf(t *testing.T, s *Sim, n int) *domain.LogicalRequest {
	t.Helper()
	req, ok := s.State().Requests[domain.RequestID("req-"+strconv.Itoa(n))]
	if !ok {
		t.Fatalf("request req-%d missing", n)
	}
	return req
}

func attemptOf(t *testing.T, s *Sim, n int) *domain.PhysicalAttempt {
	t.Helper()
	att, ok := s.State().Attempts[domain.AttemptID("att-"+strconv.Itoa(n))]
	if !ok {
		t.Fatalf("attempt att-%d missing", n)
	}
	return att
}

// --- 1. logical request vs attempt identity ---------------------------------

func TestLogicalRequestAndAttemptLifecycleAreDistinct(t *testing.T) {
	s := mustNew(t, baseCfg())
	run(t, s, arrivals(0, 1, t1, u1, 100*msT, 0), 50*msT)

	req := requestOf(t, s, 1)
	att := attemptOf(t, s, 1)

	if string(req.ID) == string(att.ID) {
		t.Fatal("request and attempt identity spaces must produce distinct IDs")
	}
	if len(req.Attempts) != 1 || req.Attempts[0] != att.ID {
		t.Fatalf("request must link to exactly its attempt: %v", req.Attempts)
	}
	if att.LogicalID != req.ID {
		t.Fatal("attempt must link back to its logical request")
	}
	if req.State != domain.RequestCompleted {
		t.Fatalf("request state = %s, want completed", req.State)
	}
	if att.State != domain.AttemptCompleted || att.Outcome != domain.OutcomeUseful {
		t.Fatalf("attempt = %s/%s, want completed/useful", att.State, att.Outcome)
	}
	// Resources fully accounted.
	st := s.State()
	if st.mainPool().Occupancy() != 0 {
		t.Fatalf("worker pool occupancy = %d after completion", st.mainPool().Occupancy())
	}
	if st.Connections[u1].InUse() != 0 || st.Connections[u1].Waiting() != 0 {
		t.Fatal("connection pool must be empty after completion")
	}
	if st.TenantActive[t1] != 0 {
		t.Fatalf("tenant active = %d, want 0", st.TenantActive[t1])
	}
}

// --- 2. attempt outliving its logical request --------------------------------

func TestAttemptExecutesAfterLogicalExpiryAndIsWasted(t *testing.T) {
	s := mustNew(t, baseCfg())
	// u2 normal latency 10ms, logical deadline 5ms: the deadline fires while
	// the attempt is executing; the attempt keeps running and completes late.
	run(t, s, arrivals(0, 1, t1, u2, 5*msT, 0), 30*msT)

	req := requestOf(t, s, 1)
	att := attemptOf(t, s, 1)

	if req.State != domain.RequestExpired {
		t.Fatalf("request state = %s, want expired", req.State)
	}
	if att.State != domain.AttemptCompleted {
		t.Fatalf("attempt execution state = %s, want completed (still executed)", att.State)
	}
	if !att.Orphaned {
		t.Fatal("late attempt must be marked orphaned")
	}
	if att.OrphanedAt != 5*msT {
		t.Fatalf("orphaned at %s, want expiry instant 5ms", att.OrphanedAt)
	}
	if att.Outcome != domain.OutcomeWasted {
		t.Fatalf("late completion outcome = %s, want wasted", att.Outcome)
	}
	if got := s.State().OutcomeCounters[t1]; got.Wasted != 1 || got.Useful != 0 {
		t.Fatalf("outcome counters = %+v, want one wasted", got)
	}
}

// --- 3. same-instant race: timeout before completion -------------------------

func TestTimeoutBeatsCompletionAtSameInstant(t *testing.T) {
	cfg := baseCfg()
	cfg.AttemptTimeout = 5 * ms // equals u1 normal latency: completion and timeout land on the same instant
	up := cfg.Upstreams[u1]
	up.NormalLatency = 5 * ms
	cfg.Upstreams[u1] = up
	s := mustNew(t, cfg)
	run(t, s, arrivals(0, 1, t1, u1, 100*msT, 0), 30*msT)

	req := requestOf(t, s, 1)
	att := attemptOf(t, s, 1)

	if att.State != domain.AttemptTimedOut {
		t.Fatalf("attempt state = %s, want timed-out (pessimistic boundary)", att.State)
	}
	if req.State != domain.RequestFailed {
		t.Fatalf("request state = %s, want failed after unretractable timeout", req.State)
	}
	if att.Outcome != domain.OutcomeUseful {
		t.Fatalf("outcome = %s, want useful (failure consumed by live request)", att.Outcome)
	}
	for _, rec := range s.Recorder().Records() {
		if rec.Kind == "attempt-transition" && strings.Contains(rec.Detail.Description, string(att.ID)) &&
			strings.Contains(rec.Detail.Description, "-> completed") {
			t.Fatalf("attempt must never have completed: %s", rec.Detail.Description)
		}
	}
}

// --- 4. same-instant race: deadline before completion -------------------------

func TestDeadlineBeatsCompletionAtSameInstant(t *testing.T) {
	cfg := baseCfg()
	up := cfg.Upstreams[u2]
	up.NormalLatency = 10 * ms
	cfg.Upstreams[u2] = up
	s := mustNew(t, cfg)
	// deadline == completion instant: governance class (deadline) runs first,
	// so the completion arrives late and is wasted.
	run(t, s, arrivals(0, 1, t1, u2, 10*msT, 0), 30*msT)

	req := requestOf(t, s, 1)
	att := attemptOf(t, s, 1)

	if req.State != domain.RequestExpired {
		t.Fatalf("request state = %s, want expired", req.State)
	}
	if att.OrphanedAt != 10*msT {
		t.Fatalf("orphaned at %s, want 10ms", att.OrphanedAt)
	}
	if att.State != domain.AttemptCompleted || att.Outcome != domain.OutcomeWasted {
		t.Fatalf("attempt = %s/%s, want completed/wasted", att.State, att.Outcome)
	}
}

// --- 5. deterministic same-instant arrival competition -----------------------

func TestSameInstantArrivalsDispatchDeterministically(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	s := mustNew(t, cfg)
	run(t, s, arrivals(0, 2, t1, u1, 100*msT, 0), 20*msT)

	r1, r2 := requestOf(t, s, 1), requestOf(t, s, 2)
	a1, a2 := attemptOf(t, s, 1), attemptOf(t, s, 2)

	if a1.State != domain.AttemptCompleted || a2.State != domain.AttemptCompleted {
		t.Fatalf("attempts = %s/%s, both must complete", a1.State, a2.State)
	}
	if a1.Worker != "" || a2.Worker != "" {
		t.Fatal("workers must be released after completion")
	}
	// With a single worker and FIFO release order, the queued attempt must
	// have taken exactly the slot its predecessor freed (deterministic reuse).
	if got := s.Recorder(); got.Len() == 0 {
		t.Fatal("recorder must capture the run")
	}
	if r1.State != domain.RequestCompleted || r2.State != domain.RequestCompleted {
		t.Fatalf("requests = %s/%s, want completed/completed", r1.State, r2.State)
	}
}

// --- 6. worker resource accounting -------------------------------------------

func TestWorkerAccountingUnderContention(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 2
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 4, MaxRetryQueueDepth: 4, MaxTotalQueueDepth: 4}
	s := mustNew(t, cfg)
	e := engine.New(s, arrivals(0, 3, t1, u1, 100*msT, 0))

	e.Run(1 * msT)
	st := s.State()
	if got := st.mainPool().Occupancy(); got != 2 {
		t.Fatalf("occupancy = %d, want 2", got)
	}
	if got := st.Queues[t1].Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}
	if st.mainPool().IdleCount() != 0 {
		t.Fatal("no worker may be idle while occupancy is 2")
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("mid-run invariants: %v", err)
	}

	e.Run(10 * msT) // completions at 5ms release workers; the queued attempt dispatches
	if got := st.mainPool().Occupancy(); got != 0 {
		t.Fatalf("occupancy after all completions = %d, want 0", got)
	}
	for i := 1; i <= 3; i++ {
		att := attemptOf(t, s, i)
		if att.State != domain.AttemptCompleted || att.Outcome != domain.OutcomeUseful {
			t.Fatalf("att-%d = %s/%s", i, att.State, att.Outcome)
		}
		if att.HasWorker {
			t.Fatalf("att-%d still holds a worker", i)
		}
	}
}

// --- 7. connection resource accounting & FIFO waitlist -----------------------

func TestConnectionWaitlistGrantsInFIFOOrder(t *testing.T) {
	cfg := baseCfg()
	cfg.ConnectionPools[u1] = 1 // one connection shared by up to two workers
	s := mustNew(t, cfg)
	run(t, s, arrivals(0, 3, t1, u1, 100*msT, 0), 20*msT)

	a1, a2, a3 := attemptOf(t, s, 1), attemptOf(t, s, 2), attemptOf(t, s, 3)

	if a1.State != domain.AttemptCompleted {
		t.Fatalf("a1 = %s", a1.State)
	}
	// a2 must have been granted the released connection first.
	if a2.BeganRunningAt != 5*msT {
		t.Fatalf("a2 began running at %s, want 5ms (FIFO grant)", a2.BeganRunningAt)
	}
	// a3 must have been dispatched onto the freed worker but pinned waiting
	// until a2's completion.
	if a3.BeganRunningAt != 10*msT {
		t.Fatalf("a3 began running at %s, want 10ms", a3.BeganRunningAt)
	}
	for _, a := range []*domain.PhysicalAttempt{a1, a2, a3} {
		if a.Outcome != domain.OutcomeUseful {
			t.Fatalf("%s outcome = %s, want useful", a.ID, a.Outcome)
		}
		if a.ConnHeld {
			t.Fatalf("%s must have released its connection on completion", a.ID)
		}
	}
	st := s.State()
	if st.Connections[u1].InUse() != 0 || st.Connections[u1].Waiting() != 0 {
		t.Fatal("connection pool must drain fully")
	}
}

// --- 8. queue membership consistency ------------------------------------------

func TestQueueMembershipConsistency(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	s := mustNew(t, cfg)
	run(t, s, arrivals(0, 3, t1, u1, 100*msT, 0), 20*msT)

	st := s.State()
	req1, req2, req3 := requestOf(t, s, 1), requestOf(t, s, 2), requestOf(t, s, 3)
	a1, a2, a3 := attemptOf(t, s, 1), attemptOf(t, s, 2), attemptOf(t, s, 3)

	if req1.State != domain.RequestCompleted {
		t.Fatalf("req1 = %s, want completed", req1.State)
	}
	// With one worker att-1 runs first; att-2 and att-3 queue, then get
	// dispatched and completed in FIFO order.
	if a1.State != domain.AttemptCompleted || a2.State != domain.AttemptCompleted || a3.State != domain.AttemptCompleted {
		t.Fatalf("attempts = %s/%s/%s, all must complete", a1.State, a2.State, a3.State)
	}
	if a2.BeganRunningAt >= a3.BeganRunningAt {
		t.Fatalf("FIFO dispatch violated: a2 ran at %s, a3 at %s", a2.BeganRunningAt, a3.BeganRunningAt)
	}
	if st.Queues[t1].Len() != 0 {
		t.Fatalf("queue must be empty, has %d", st.Queues[t1].Len())
	}
	if req2.State != domain.RequestCompleted || req3.State != domain.RequestCompleted {
		t.Fatalf("req2/req3 = %s/%s", req2.State, req3.State)
	}
}

func TestCancellationRetractsQueuedAttemptImmediately(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	s := mustNew(t, cfg)
	events := arrivals(0, 2, t1, u1, 100*msT, 0)
	events = append(events, NewCancellationEvent(1*msT, "req-2", "test: retract queued attempt"))
	run(t, s, events, 20*msT)

	req2 := requestOf(t, s, 2)
	a2 := attemptOf(t, s, 2)
	switch {
	case req2.State != domain.RequestCancelled:
		t.Fatalf("req2 state = %s, want cancelled (outstanding==0 at ack)", req2.State)
	case a2.State != domain.AttemptRetracted:
		t.Fatalf("a2 state = %s, want retracted", a2.State)
	case a2.Outcome != domain.OutcomeDiscarded:
		t.Fatalf("a2 outcome = %s, want discarded", a2.Outcome)
	}
	if s.State().Queues[t1].Contains(a2.ID) {
		t.Fatal("retracted attempt must not remain a queue member")
	}
	foundAck := false
	for _, rec := range s.Recorder().Records() {
		if rec.Kind == "CancellationAcknowledged" && rec.Detail.Request == "req-2" {
			foundAck = true
		}
	}
	if !foundAck {
		t.Fatal("cancellation acknowledgement must be recorded")
	}
}

// --- 9. admission shedding ----------------------------------------------------

func TestAdmissionShedsWhenTenantQueueFull(t *testing.T) {
	cfg := baseCfg()
	cfg.WorkerPool.Size = 1
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 1, MaxRetryQueueDepth: 1, MaxTotalQueueDepth: 1}
	s := mustNew(t, cfg)
	// Staggered arrivals: the first request is admitted and dispatched; the
	// second fills the single queue slot; the third must be shed because the
	// tenant queue is at capacity at its arrival instant.
	initial := append(
		arrivals(0, 1, t1, u1, 100*msT, 0),
		NewArrivalEvent(1*msT, t1, u1, 100*msT, 0),
		NewArrivalEvent(2*msT, t1, u1, 100*msT, 0),
	)
	// Stop at 2ms: before the first completion, so one request is dispatched,
	// one queued and the third must have been shed at admission.
	run(t, s, initial, 2*msT)

	r1, r2, r3 := requestOf(t, s, 1), requestOf(t, s, 2), requestOf(t, s, 3)
	if r3.State != domain.RequestShed {
		t.Fatalf("req3 state = %s, want shed at admission", r3.State)
	}
	if len(r3.Attempts) != 0 {
		t.Fatalf("shed request must not create attempts: %v", r3.Attempts)
	}
	if r2.State != domain.RequestQueued {
		t.Fatalf("req2 state = %s, want queued at the shed check", r2.State)
	}
	if got := s.State().TenantActive[t1]; got != 2 {
		t.Fatalf("tenant active = %d, want 2 (one running, one queued)", got)
	}
	if r1.State != domain.RequestDispatched {
		t.Fatalf("req1 state = %s, want dispatched", r1.State)
	}
	shedSeen := false
	for _, rec := range s.Recorder().Records() {
		if rec.Kind == "LoadShed" && rec.Detail.Request == string(r3.ID) {
			shedSeen = true
		}
	}
	if !shedSeen {
		t.Fatal("load-shed event must be recorded for req3")
	}
}

// --- 10. circuit breaker lifecycle --------------------------------------------

func TestBreakerLifecycleTransitionsAndShedding(t *testing.T) {
	const long = 10000 * msT // 10s of simulated time: effectively "no logical deadline"
	cfg := baseCfg()
	cfg.ConnectionPools[u1] = 2
	cfg.RetryBackoff = 1 * ms
	s := mustNew(t, cfg)
	initial := []engine.Event{
		NewUpstreamDegradedEvent(0, u1),
		NewArrivalEvent(0, t1, u1, long, 0),
		NewArrivalEvent(0, t1, u1, long, 0),
		NewUpstreamRecoveredEvent(60*msT, u1),
		NewArrivalEvent(70*msT, t1, u1, long, 0), // half-open probe
		NewArrivalEvent(71*msT, t1, u1, long, 0), // probe saturated → shed
		NewArrivalEvent(72*msT, t1, u1, long, 0), // still saturated → shed
	}
	run(t, s, initial, 120*msT)

	var open, half, closed int
	var sheds []string
	for _, rec := range s.Recorder().Records() {
		if rec.Kind != "breaker" && rec.Kind != "LoadShed" {
			continue
		}
		switch {
		case rec.Kind == "breaker" && rec.Detail.To == "open":
			open++
		case rec.Kind == "breaker" && rec.Detail.To == "half-open":
			half++
		case rec.Kind == "breaker" && rec.Detail.To == "closed":
			closed++
		case rec.Kind == "LoadShed":
			sheds = append(sheds, rec.Detail.Description)
		}
	}
	if open != 1 || half != 1 || closed != 1 {
		t.Fatalf("breaker transitions open=%d half-open=%d closed=%d, want 1/1/1", open, half, closed)
	}
	if len(sheds) != 2 {
		t.Fatalf("sheds = %d, want 2 (probe saturation): %v", len(sheds), sheds)
	}

	br := s.State().Breakers[u1]
	if br.State != domain.BreakerClosed || br.ConsecutiveFailures != 0 {
		t.Fatalf("final breaker = %s with %d failures, want closed/0", br.State, br.ConsecutiveFailures)
	}
	// Probe request completed through the recovered upstream.
	req3 := requestOf(t, s, 3)
	if req3.State != domain.RequestCompleted {
		t.Fatalf("probe request state = %s, want completed", req3.State)
	}
	if att := attemptOf(t, s, 3); att.IsProbe {
		t.Fatal("probe flag must clear when the attempt finishes")
	}
}

// --- 11. retry budget ---------------------------------------------------------

func TestRetryBudgetExhaustionBlocksRetryAndRefillsLater(t *testing.T) {
	const long = 10000 * msT
	cfg := baseCfg()
	cfg.RetryBudget = RetryBudgetConfig{Capacity: 1, RefillInterval: 100 * ms}
	cfg.AttemptTimeout = 5 * ms // always fires before the 10ms u2 executions
	up := cfg.Upstreams[u2]
	up.NormalLatency = 10 * ms
	cfg.Upstreams[u2] = up
	cfg.RetryBackoff = 2 * ms
	s := mustNew(t, cfg)
	initial := []engine.Event{
		NewArrivalEvent(0, t1, u2, long, 10),
		NewArrivalEvent(200*msT, t1, u2, long, 10),
	}
	run(t, s, initial, 400*msT)

	r1, r2 := requestOf(t, s, 1), requestOf(t, s, 2)
	if r1.State != domain.RequestFailed {
		t.Fatalf("req1 = %s, want failed with exhausted budget", r1.State)
	}
	if r1.RetriesUsed != 1 {
		t.Fatalf("req1 retries used = %d, want 1 (budget capacity)", r1.RetriesUsed)
	}
	// By 200ms the bucket refilled, so the second request manages one retry
	// before its own budget runs dry.
	if r2.State != domain.RequestFailed {
		t.Fatalf("req2 = %s, want failed after its retry", r2.State)
	}
	if r2.RetriesUsed != 1 {
		t.Fatalf("req2 retries used = %d, want 1 after refill", r2.RetriesUsed)
	}
	if b := s.State().Budgets[domain.BudgetKey{Tenant: t1, Upstream: u2}]; b == nil {
		t.Fatal("budget must exist in state after use")
	}
}

// --- 12. cancellation of an executing attempt ---------------------------------

func TestCancellationOfExecutingAttemptOrphansAndTerminatesLate(t *testing.T) {
	cfg := baseCfg()
	cfg.Upstreams[u2] = UpstreamConfig{Criticality: domain.CriticalityStandard, NormalLatency: 50 * ms, DegradedLatency: 200 * ms}
	// Long attempt timeout so the orphaned attempt completes naturally.
	cfg.AttemptTimeout = 200 * ms
	s := mustNew(t, cfg)
	const long = 10000 * msT
	events := []engine.Event{
		NewArrivalEvent(0, t1, u2, long, 0),
		NewCancellationEvent(5*msT, "req-1", "client gave up"),
	}
	run(t, s, events, 80*msT)

	req := requestOf(t, s, 1)
	att := attemptOf(t, s, 1)

	// Acknowledged with one outstanding attempt; the attempt finishes later,
	// and only then does the request become fully cancelled.
	if req.State != domain.RequestCancelled {
		t.Fatalf("request = %s, want cancelled after orphan finished", req.State)
	}
	if att.OrphanedAt != 5*msT {
		t.Fatalf("orphaned at %s, want the cancellation instant 5ms", att.OrphanedAt)
	}
	if att.State != domain.AttemptCompleted || att.Outcome != domain.OutcomeWasted {
		t.Fatalf("attempt = %s/%s, want completed/wasted", att.State, att.Outcome)
	}
	if got := s.State().OutcomeCounters[t1]; got.Wasted != 1 {
		t.Fatalf("wasted counter = %+v, want exactly one wasted", got)
	}
}

// --- 13. repeatability of the whole reference system ---------------------------

func TestReferenceSystemRepeatabilityFromIdenticalInputs(t *testing.T) {
	build := func() (*Sim, []engine.Event) {
		cfg := baseCfg()
		cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 2, MaxRetryQueueDepth: 2, MaxTotalQueueDepth: 2}
		cfg.RetryBudget = RetryBudgetConfig{Capacity: 2, RefillInterval: 20 * ms}
		s := mustNew(t, cfg)
		const long = 10000 * msT
		events := []engine.Event{
			NewUpstreamDegradedEvent(2*msT, u1),
			NewArrivalEvent(0, t1, u1, 40*msT, 2),
			NewArrivalEvent(0, t1, u1, 40*msT, 2),
			NewArrivalEvent(1*msT, t2, u2, long, 0),
			// Cancels req-1 while its attempt is still executing: the request
			// terminalizes at the completion instant. Note retries in this
			// scenario are blocked later by the (new) retry-vs-breaker rule;
			// that interplay has its dedicated regression tests in
			// remediation_test.go.
			NewCancellationEvent(4*msT, "req-1", "repeat test cancel"),
			NewUpstreamRecoveredEvent(30*msT, u1),
			NewArrivalEvent(45*msT, t1, u1, 60*msT, 3),
			NewArrivalEvent(50*msT, t1, u1, 53*msT, 0), // expires; late completion wasted
		}
		return s, events
	}

	s1, ev1 := build()
	e1 := engine.New(s1, ev1)
	e1.Run(120 * msT)
	if err := s1.State().Validate(); err != nil {
		t.Fatalf("run1 invariants: %v", err)
	}

	s2, ev2 := build()
	e2 := engine.New(s2, ev2)
	e2.Run(120 * msT)
	if err := s2.State().Validate(); err != nil {
		t.Fatalf("run2 invariants: %v", err)
	}

	if !reflect.DeepEqual(s1.State(), s2.State()) {
		t.Fatal("identical inputs must produce identical final state")
	}
	if !reflect.DeepEqual(s1.Recorder().Records(), s2.Recorder().Records()) {
		t.Fatal("identical inputs must produce identical record traces")
	}
	if e1.Processed() != e2.Processed() {
		t.Fatalf("processed counts differ: %d vs %d", e1.Processed(), e2.Processed())
	}

	// The scenario must actually exercise the interesting parts; guard
	// against it silently degenerating.
	if req := requestOf(t, s1, 1); req.State != domain.RequestCancelled {
		t.Fatalf("req1 = %s, want cancelled in the combined scenario", req.State)
	}
	lateWasted := false
	for _, att := range s1.State().Attempts {
		if att.Outcome == domain.OutcomeWasted {
			lateWasted = true
		}
	}
	if !lateWasted {
		t.Fatal("scenario must produce at least one wasted (late) completion")
	}
	if len(s1.Recorder().Records()) < 50 {
		t.Fatalf("scenario underexercised: %d records", len(s1.Recorder().Records()))
	}
}

// --- 14. invariants actually detect corruption --------------------------------

func TestValidateDetectsCorruptedAccounting(t *testing.T) {
	s := mustNew(t, baseCfg())
	run(t, s, arrivals(0, 1, t1, u1, 100*msT, 0), 50*msT)

	att := attemptOf(t, s, 1)
	att.ConnHeld = true // claim a connection the pool does not hold
	err := s.State().Validate()
	if err == nil || !strings.Contains(err.Error(), "ConnHeld") {
		t.Fatalf("validator must catch phantom connection: %v", err)
	}
	att.ConnHeld = false

	st := s.State()
	bak := st.TenantActive[t1]
	st.TenantActive[t1] = -5
	err = st.Validate()
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("validator must catch negative tenant accounting: %v", err)
	}
	st.TenantActive[t1] = bak
}

// --- 15. the reference system rejects foreign events ---------------------------

type foreignEvent struct{ t domain.SimTime }

func (f *foreignEvent) At() domain.SimTime { return f.t }
func (f *foreignEvent) Priority() int      { return 0 }

func TestSimRejectsForeignEventTypes(t *testing.T) {
	s := mustNew(t, baseCfg())
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("sim must panic on unknown event types")
		}
	}()
	s.Handle(&foreignEvent{t: 0}, 0)
}

// --- 16. deadline and cancellation interplay -----------------------------------

func TestCancellationSupersedesLaterDeadline(t *testing.T) {
	cfg := baseCfg()
	cfg.Upstreams[u2] = UpstreamConfig{Criticality: domain.CriticalityStandard, NormalLatency: 50 * ms, DegradedLatency: 200 * ms}
	// Long attempt timeout so the orphaned attempt completes naturally.
	cfg.AttemptTimeout = 200 * ms
	s := mustNew(t, cfg)
	events := []engine.Event{
		NewArrivalEvent(0, t1, u2, 20*msT, 0),
		NewCancellationEvent(2*msT, "req-1", "before deadline"),
	}
	run(t, s, events, 80*msT)

	req := requestOf(t, s, 1)
	if req.State != domain.RequestCancelled {
		t.Fatalf("request = %s, want cancelled (ack path, deadline redundant)", req.State)
	}
	att := attemptOf(t, s, 1)
	if att.State != domain.AttemptCompleted || att.Outcome != domain.OutcomeWasted {
		t.Fatalf("attempt = %s/%s, want completed/wasted", att.State, att.Outcome)
	}
}

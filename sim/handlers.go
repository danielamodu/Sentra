package sim

import (
	"fmt"
	"sort"

	"sentra/domain"
	"sentra/engine"
)

// Sim is the M1 reference implementation: it wires the domain model to the
// deterministic engine and owns ALL state-transition logic. Handlers are
// invoked strictly one at a time by the engine; the Sim is therefore not
// goroutine-safe by design and must not be shared across engines.
//
// Architectural contract (docs/ARCHITECTURE.md): the future independent
// discrete-event simulator must NOT reuse these handlers; it re-derives
// transitions from shared domain contracts so the two systems can be compared
// as independent validators.
type Sim struct {
	st  *SystemState
	rec *Recorder

	// dispatchScheduled marks attempts that already have a pending
	// QueueRemoval event, so dispatch probes never double-schedule the same
	// queue entry at the same instant. Entries are consumed when their
	// QueueRemoval event is processed (even if it turns out to be a no-op).
	//
	// pendingRemovals counts QueueRemoval events that are scheduled but not
	// yet processed. Workers are only claimed when WorkerAssignment runs,
	// which is after all removals of the same instant; the probe therefore
	// gates on (idle workers - pending removals) to never schedule more
	// dispatches than the pool can actually serve.
	dispatchScheduled map[domain.AttemptID]struct{}
	pendingRemovals   int
}

// New builds the reference system over a validated config. The recorder uses
// the default bounded in-memory ring sink; a streaming sink can be installed
// via NewRecorder+Recorder fields by tests or future tooling.
func New(cfg SystemConfig) (*Sim, error) {
	st, err := NewState(cfg)
	if err != nil {
		return nil, err
	}
	return &Sim{
		st:                st,
		rec:               NewRecorder(nil),
		dispatchScheduled: make(map[domain.AttemptID]struct{}),
	}, nil
}

// State exposes the system state (read-only by convention: mutate only via
// events through Handle).
func (s *Sim) State() *SystemState { return s.st }

// Recorder exposes the structured evidence ledger.
func (s *Sim) Recorder() *Recorder { return s.rec }

// Handle implements engine.Handler. The seq argument is the engine's
// scheduling sequence number of the consumed event; it anchors all records the
// handler writes while processing this event (correlation contract,
// docs/ENGINE_SEMANTICS.md §5). Every processed event is typed and recorded
// first, then dispatched through an exhaustive type switch; unknown kinds
// panic.
func (s *Sim) Handle(ev engine.Event, seq uint64) []engine.Event {
	e, ok := ev.(Event)
	if !ok {
		panic(fmt.Sprintf("sim: received event of foreign type %T — the reference system only processes its own typed events", ev))
	}
	s.rec.beginEvent(seq)
	s.rec.LogEvent(e)

	switch e := e.(type) {
	case *EventRequestArrival:
		return s.handleRequestArrival(e)
	case *EventAdmissionDecided:
		return nil // decision applied atomically by the arrival; observable by record only
	case *EventQueueInsertion:
		return s.handleQueueInsertion(e)
	case *EventQueueRemoval:
		return s.handleQueueRemoval(e)
	case *EventWorkerAssignment:
		return s.handleWorkerAssignment(e)
	case *EventExecutionStart:
		return s.handleExecutionStart(e)
	case *EventExecutionCompletion:
		return s.handleExecutionCompletion(e)
	case *EventTimeout:
		return s.handleTimeout(e)
	case *EventCancellationRequested:
		return s.handleCancellationRequested(e)
	case *EventCancellationAcknowledged:
		return s.handleCancellationAcknowledged(e)
	case *EventRetryScheduled:
		return s.handleRetryScheduled(e)
	case *EventRetryExecution:
		return s.handleRetryExecution(e)
	case *EventDeadlineExpired:
		return s.handleDeadlineExpired(e)
	case *EventUpstreamDegraded:
		return s.handleUpstreamDegraded(e)
	case *EventUpstreamRecovered:
		return s.handleUpstreamRecovered(e)
	case *EventBreakerTransitioned:
		return s.handleBreakerTransitioned(e)
	case *EventLoadShed:
		return nil // termination applied atomically by the arrival; observable by record only
	case *EventConnectionGranted:
		return s.handleConnectionGranted(e)
	case *EventUpstreamLatencySet:
		return s.handleUpstreamLatencySet(e)
	case *EventUpstreamFailureSet:
		return s.handleUpstreamFailureSet(e)
	default:
		panic(fmt.Sprintf("sim: unhandled event kind %s", e.Kind()))
	}
}

// --- checked time arithmetic -------------------------------------------------

// addT computes t+d with an explicit overflow boundary. The four scheduling
// sites in this file (attempt timeout, execution completion, retry backoff,
// breaker cooldown) all go through it, so a boundary-adjacent scenario fails
// with a distinct, testable panic — "simulated time overflow" — instead of
// silently wrapping int64 nanoseconds (docs/ASSUMPTIONS.md A17).
func (s *Sim) addT(t domain.SimTime, d domain.SimDuration, what string) domain.SimTime {
	nt, ok := t.AddChecked(d)
	if !ok {
		panic(fmt.Sprintf("sim: %s scheduling comes after simulated time overflow: %s + %s exceeds the int64 nanosecond range", what, t, d))
	}
	return nt
}

// --- identity & transition helpers -----------------------------------------

func (s *Sim) mustRequest(id domain.RequestID) *domain.LogicalRequest {
	req, ok := s.st.Requests[id]
	if !ok {
		panic(fmt.Sprintf("sim: unknown request %s", id))
	}
	return req
}

func (s *Sim) mustAttempt(id domain.AttemptID) *domain.PhysicalAttempt {
	att, ok := s.st.Attempts[id]
	if !ok {
		panic(fmt.Sprintf("sim: unknown attempt %s", id))
	}
	return att
}

func (s *Sim) reqTransition(req *domain.LogicalRequest, to domain.RequestState, t domain.SimTime, why string) {
	if !domain.CanTransitionRequest(req.State, to) {
		panic(fmt.Sprintf("sim: illegal request transition %s -> %s at %s (%s)", req.State, to, t, why))
	}
	from := req.State
	req.State = to
	if to.Terminal() {
		// M2 metric: exactly-once terminal instant for goodput/latency
		// aggregation (the transition table guarantees this is the only
		// terminal entry per request).
		req.TerminalAt = t
	}
	s.rec.Log(t, "request-transition", Detail{Request: string(req.ID), From: from.String(), To: to.String(), Description: fmt.Sprintf("%s: %s -> %s (%s)", req.ID, from, to, why)})
}

func (s *Sim) attTransition(att *domain.PhysicalAttempt, to domain.AttemptState, t domain.SimTime, why string) {
	if !domain.CanTransitionAttempt(att.State, to) {
		panic(fmt.Sprintf("sim: illegal attempt transition %s: %s -> %s at %s (%s)", att.ID, att.State, to, t, why))
	}
	from := att.State
	att.State = to
	if to.Terminal() {
		// M2 metric: exactly-once terminal instant for phase aggregation
		// (attempt terminal states have no outgoing transitions).
		att.FinishedAt = t
	}
	s.rec.Log(t, "attempt-transition", Detail{Attempt: string(att.ID), From: from.String(), To: to.String(), Description: fmt.Sprintf("%s: %s -> %s (%s)", att.ID, from, to, why)})
}

// terminalizeRequest moves a request to a terminal state and releases its
// tenant-level admission accounting. Called exactly once per request via the
// transition table (terminal states have no outgoing transitions).
func (s *Sim) terminalizeRequest(req *domain.LogicalRequest, to domain.RequestState, t domain.SimTime, why string) {
	s.reqTransition(req, to, t, why)
	n, ok := s.st.TenantActive[req.Tenant]
	if !ok || n <= 0 {
		panic(fmt.Sprintf("sim: tenant %s active-count underflow at %s", req.Tenant, t))
	}
	s.st.TenantActive[req.Tenant] = n - 1
}

// createAttempt registers a fresh physical attempt (state Created) and links
// it to its logical request. isRetry marks the admission lane the attempt
// occupies from this instant; retries are counted in the retry lane from this
// point on (pending bookkeeping) so same-instant retry executions cannot
// overshoot the configured retry bounds.
func (s *Sim) createAttempt(req *domain.LogicalRequest, t domain.SimTime, isRetry bool) *domain.PhysicalAttempt {
	id := s.st.NewAttemptID()
	att := &domain.PhysicalAttempt{
		ID:        id,
		LogicalID: req.ID,
		Tenant:    req.Tenant,
		Upstream:  req.Upstream,
		CreatedAt: t,
		State:     domain.AttemptCreated,
		Outcome:   domain.OutcomeUndetermined,
		Timeout:   s.st.Cfg.AttemptTimeout,
		IsRetry:   isRetry,
	}
	s.st.Attempts[id] = att
	s.st.recordAttemptCreate(att) // M2 metric: physical attempt volume per lane
	req.Attempts = append(req.Attempts, id)
	// D1 additive evidence (Milestone 3): the creation effect carries typed
	// request/attempt/tenant/upstream identity and the admission lane, so an
	// independent verifier can attribute an attempt to its logical request
	// without parsing prose. Behaviour is unchanged; the description is
	// identical to the previous Logf form.
	s.rec.Log(t, "attempt-created", Detail{
		Request:     string(req.ID),
		Attempt:     string(id),
		Tenant:      string(req.Tenant),
		Upstream:    string(req.Upstream),
		Lane:        attemptLane(isRetry),
		Description: fmt.Sprintf("%s created for request %s (timeout %s, lane %s)", id, req.ID, att.Timeout, attemptLane(isRetry)),
	})
	if isRetry {
		s.st.recordPendingRetry(att)
	} else {
		s.st.recordPendingArrival(att)
	}
	return att
}

func attemptLane(isRetry bool) string {
	if isRetry {
		return "retry"
	}
	return "arrival"
}

// --- arrival & admission -----------------------------------------------------

// countLane returns the number of queued attempts of a lane (arrival or retry)
// across all queue entries of the tenant. Lane membership is read from the
// attempts themselves (IsRetry), so no parallel lane structure can drift.
func (s *Sim) countLane(q *domain.Queue, isRetry bool) int {
	n := 0
	for _, e := range q.Entries {
		if att := s.st.Attempts[e.Attempt]; att != nil && att.IsRetry == isRetry {
			n++
		}
	}
	return n
}

// shedReason evaluates the M1 deterministic admission policy for an original
// arrival. Load shedding fires when: the target upstream's breaker is open;
// the breaker is half-open with all probe permits reserved; the tenant's
// arrival lane is at capacity (queued arrivals + pending arrivals, which
// closes the same-instant burst overshoot); or the total queue bound is
// exhausted. Retry attempts have their own admission checks at
// RetryExecution (see handleRetryExecution).
func (s *Sim) shedReason(req *domain.LogicalRequest) (string, bool) {
	st := s.st
	br := st.Breakers[req.Upstream]
	if br.State == domain.BreakerOpen {
		return "breaker-open", true
	}
	if br.State == domain.BreakerHalfOpen && br.ProbesActive >= st.Cfg.Breaker.MaxHalfOpenProbes {
		return "breaker-halfopen-saturated", true
	}
	cfg := st.Cfg.Tenants[req.Tenant]
	q := st.Queues[req.Tenant]
	arrivalUsed := s.countLane(q, false) + st.PendingArrivalCounts[req.Tenant]
	if arrivalUsed >= cfg.MaxArrivalQueueDepth {
		return "tenant-queue-full", true
	}
	totalUsed := q.Len() + st.PendingArrivalCounts[req.Tenant] + st.PendingRetryCounts[req.Tenant]
	if totalUsed >= cfg.MaxTotalQueueDepth {
		return "tenant-queue-total-full", true
	}
	return "", false
}

func (s *Sim) handleRequestArrival(e *EventRequestArrival) []engine.Event {
	t := e.At()
	st := s.st
	if _, ok := st.Cfg.Upstreams[e.Upstream]; !ok {
		panic(fmt.Sprintf("sim: arrival for unknown upstream %s at %s", e.Upstream, t))
	}
	if _, ok := st.Cfg.Tenants[e.Tenant]; !ok {
		panic(fmt.Sprintf("sim: arrival for unknown tenant %s at %s", e.Tenant, t))
	}

	id := st.NewRequestID()
	req := &domain.LogicalRequest{
		ID:         id,
		Tenant:     e.Tenant,
		Upstream:   e.Upstream,
		ArrivedAt:  t,
		Deadline:   e.Deadline,
		MaxRetries: e.MaxRetries,
		State:      domain.RequestAdmitted,
	}
	st.Requests[id] = req
	st.TenantActive[e.Tenant]++
	// M3 P3: typed arrival payload for workload reconstruction. Behaviour and
	// the description are unchanged; the deadline and retry limit are now
	// evidence rather than prose.
	s.rec.Log(t, "request-created", Detail{
		Request:     string(id),
		Tenant:      string(e.Tenant),
		Upstream:    string(e.Upstream),
		DeadlineNS:  int64(e.Deadline),
		MaxRetries:  retriesPtr(e.MaxRetries),
		Description: fmt.Sprintf("%s arrived (tenant %s, upstream %s, deadline %s)", id, e.Tenant, e.Upstream, e.Deadline),
	})

	// Deadline == arrival is defined as immediate expiration: the request
	// never enters admission, no attempt is created and no DeadlineExpired
	// event is scheduled (there is nothing left to expire). Deadline < arrival
	// remains a scenario programming error and panics (docs/ASSUMPTIONS.md).
	if e.Deadline == t {
		s.terminalizeRequest(req, domain.RequestExpired, t, "deadline equals arrival: expired at admission")
		s.rec.Log(t, "admission", Detail{Request: string(id), Tenant: string(e.Tenant), Upstream: string(e.Upstream), Decision: "expired", Reason: "deadline-at-arrival", Description: fmt.Sprintf("%s expired at admission (deadline == arrival)", id)})
		return []engine.Event{
			&EventAdmissionDecided{baseEvent: baseEvent{Time: t}, Request: id, Tenant: e.Tenant, Upstream: e.Upstream, Admitted: false, Reason: "deadline-at-arrival"},
		}
	}
	if e.Deadline < t {
		panic(fmt.Sprintf("sim: arrival %s deadline %s precedes arrival time %s", id, e.Deadline, t))
	}

	if reason, shed := s.shedReason(req); shed {
		s.terminalizeRequest(req, domain.RequestShed, t, "admission rejected: "+reason)
		s.st.recordShed(req.Tenant, reason)
		return []engine.Event{
			&EventAdmissionDecided{baseEvent: baseEvent{Time: t}, Request: id, Tenant: e.Tenant, Upstream: e.Upstream, Admitted: false, Reason: reason},
			&EventLoadShed{baseEvent: baseEvent{Time: t}, Request: id, Reason: reason},
		}
	}

	att := s.createAttempt(req, t, false)
	if br := st.Breakers[req.Upstream]; br.State == domain.BreakerHalfOpen {
		// Probe permit reserved at admission; released when this attempt
		// finishes executing or is retracted (see finishAttempt and
		// retractAttempt — the permit ledger is exact).
		br.ProbesActive++
		att.IsProbe = true
		s.rec.Log(t, "probe-reserved", Detail{Attempt: string(att.ID), Request: string(req.ID), Tenant: string(req.Tenant), Upstream: string(req.Upstream), Decision: "reserved", Description: fmt.Sprintf("%s reserved a half-open probe permit for %s", att.ID, req.Upstream)})
	}
	return []engine.Event{
		&EventAdmissionDecided{baseEvent: baseEvent{Time: t}, Request: id, Tenant: e.Tenant, Upstream: e.Upstream, Admitted: true},
		&EventQueueInsertion{baseEvent: baseEvent{Time: t}, Attempt: att.ID},
		// The logical deadline is armed once, at admission, and fires at the
		// deadline instant regardless of how many attempts exist by then.
		&EventDeadlineExpired{baseEvent: baseEvent{Time: e.Deadline}, Request: id},
	}
}

// --- queueing & dispatch -----------------------------------------------------

func (s *Sim) handleQueueInsertion(e *EventQueueInsertion) []engine.Event {
	t := e.At()
	att := s.mustAttempt(e.Attempt)
	switch att.State {
	case domain.AttemptRetracted:
		// A retraction exit ran between attempt creation and insertion (for
		// example a cancellation at the same instant, priority 10, ahead of
		// this insertion). The attempt is already finalized; insertion is an
		// observed no-op.
		s.rec.Logf(t, "skip", "%s already retracted; queue insertion ignored", att.ID)
		return nil
	case domain.AttemptCreated:
		// expected
	default:
		panic(fmt.Sprintf("sim: queue insertion for attempt %s in state %s at %s", att.ID, att.State, t))
	}
	req := s.mustRequest(att.LogicalID)
	if req.State.Terminal() {
		// Defensive: an attempt created for a request that terminated before
		// queueing is retracted through the single retraction exit path.
		s.retractAttempt(att, t, "logical request already terminal")
		return nil
	}
	s.st.settlePending(att)
	s.attTransition(att, domain.AttemptQueued, t, "entered queue")
	s.reqTransition(req, domain.RequestQueued, t, "attempt queued")
	q := s.st.Queues[att.Tenant]
	seq := s.st.NextQueueSeq()
	if err := q.Append(domain.QueueEntry{Attempt: att.ID, EnqueuedAt: t, Seq: seq}); err != nil {
		panic(err)
	}
	s.st.recordQueueOccupancy(t, att.Tenant)
	s.rec.Log(t, "queue", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Lane: attemptLane(att.IsRetry), Seq: seq, Count: q.Len(), Decision: "append", Description: fmt.Sprintf("%s appended to tenant %s queue (len %d, lane %s)", att.ID, att.Tenant, q.Len(), attemptLane(att.IsRetry))})
	return s.probeDispatch(t)
}

// probeDispatch schedules one QueueRemoval event per eligible queue entry,
// bounded by the number of idle workers not already claimed by pending
// removal events. Entries already carrying a pending removal are skipped
// (dedup); candidates are chosen oldest-inserted first across all tenant
// queues by the shared global sequence number.
func (s *Sim) probeDispatch(t domain.SimTime) []engine.Event {
	st := s.st
	pool := st.mainPool()
	var out []engine.Event
	for pool.IdleCount() > s.pendingRemovals {
		cand := s.oldestDispatchCandidate()
		if cand == "" {
			break
		}
		s.dispatchScheduled[cand] = struct{}{}
		s.pendingRemovals++
		out = append(out, &EventQueueRemoval{baseEvent: baseEvent{Time: t}, Attempt: cand})
	}
	return out
}

func (s *Sim) oldestDispatchCandidate() domain.AttemptID {
	st := s.st
	var best domain.AttemptID
	var bestSeq uint64
	found := false
	for _, q := range st.Queues {
		for _, en := range q.Entries {
			if _, pending := s.dispatchScheduled[en.Attempt]; pending {
				continue
			}
			att, ok := st.Attempts[en.Attempt]
			if !ok || att.State != domain.AttemptQueued {
				continue // stale entry; its removal event will clean up
			}
			if !found || en.Seq < bestSeq {
				found, best, bestSeq = true, en.Attempt, en.Seq
			}
		}
	}
	return best
}

func (s *Sim) handleQueueRemoval(e *EventQueueRemoval) []engine.Event {
	t := e.At()
	delete(s.dispatchScheduled, e.Attempt) // probe reservation consumed regardless of outcome
	s.pendingRemovals--
	att := s.mustAttempt(e.Attempt)
	if att.State != domain.AttemptQueued {
		s.rec.Logf(t, "skip", "%s no longer queued (state %s); removal ignored", att.ID, att.State)
		return nil
	}
	pool := s.st.mainPool()
	if !pool.HasIdle() {
		s.rec.Logf(t, "skip", "no idle worker at dispatch time; %s stays queued", att.ID)
		return nil
	}
	q := s.st.Queues[att.Tenant]
	entry, ok := q.EntryFor(att.ID)
	if !ok {
		panic(fmt.Sprintf("sim: queue invariant violated: %s marked queued but absent at %s", att.ID, t))
	}
	q.Remove(att.ID)
	s.st.recordQueueOccupancy(t, att.Tenant)
	// Fairness evidence: this attempt now pays its queue-wait (may be zero)
	// and counts toward the tenant's dispatch share.
	wait := t.Sub(entry.EnqueuedAt)
	s.st.recordQueueWait(att.Tenant, wait)
	s.rec.Log(t, "queue", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Lane: attemptLane(att.IsRetry), Seq: entry.Seq, Count: q.Len(), LatencyNS: int64(wait), Decision: "remove", Description: fmt.Sprintf("%s removed from tenant %s queue for dispatch (len %d, waited %s)", att.ID, att.Tenant, q.Len(), wait)})
	s.attTransition(att, domain.AttemptDispatching, t, "removed from queue for dispatch")
	return []engine.Event{&EventWorkerAssignment{baseEvent: baseEvent{Time: t}, Attempt: att.ID}}
}

func (s *Sim) handleWorkerAssignment(e *EventWorkerAssignment) []engine.Event {
	t := e.At()
	att := s.mustAttempt(e.Attempt)
	if att.State != domain.AttemptDispatching {
		s.rec.Logf(t, "skip", "%s assignment ignored (state %s)", att.ID, att.State)
		return nil
	}
	pool := s.st.mainPool()
	w := pool.FirstIdle()
	if w == "" {
		panic(fmt.Sprintf("sim: no idle worker at assignment of %s (dispatch accounting broken)", att.ID))
	}
	if err := pool.Take(w, att.ID); err != nil {
		panic(err)
	}
	att.Worker = w
	att.HasWorker = true
	att.WorkerSince = t // M2 metric: folded into WorkerTime at release
	att.WorkerSet = true
	s.rec.Log(t, "worker", Detail{Worker: string(w), Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Count: pool.IdleCount(), Decision: "acquired", Description: fmt.Sprintf("%s claimed by %s (idle %d)", w, att.ID, pool.IdleCount())})
	s.st.recordWorkerOccupancy(t)
	s.attTransition(att, domain.AttemptAssigned, t, "assigned to "+string(w))
	req := s.mustRequest(att.LogicalID)
	s.reqTransition(req, domain.RequestDispatched, t, "attempt on "+string(w))
	return []engine.Event{&EventExecutionStart{baseEvent: baseEvent{Time: t}, Attempt: att.ID}}
}

// --- execution ---------------------------------------------------------------

func (s *Sim) handleExecutionStart(e *EventExecutionStart) []engine.Event {
	t := e.At()
	att := s.mustAttempt(e.Attempt)
	if att.State != domain.AttemptAssigned {
		s.rec.Logf(t, "skip", "%s execution start ignored (state %s)", att.ID, att.State)
		return nil
	}
	// The execution duration is fixed here: M1 health-based selection
	// (A12) unless an M2 fault episode overrides latency on this upstream
	// (faults.go). The attempt timeout covers everything from now on,
	// including time spent waiting for a connection.
	att.ExecDuration = s.executionLatencyFor(att, t)
	out := []engine.Event{&EventTimeout{baseEvent: baseEvent{Time: s.addT(t, att.Timeout, "attempt timeout")}, Attempt: att.ID}}
	cp := s.st.Connections[att.Upstream]
	if cp.HasCapacity() {
		out = append(out, s.startRunning(att, t)...)
	} else {
		s.attTransition(att, domain.AttemptWaitingConnection, t, "connection pool exhausted")
		if err := cp.EnqueueWaiter(att.ID); err != nil {
			panic(err)
		}
		att.WaitingSince = t // M2 metric: closed when waiting ends (granted or abandoned)
		att.WaitingSet = true
		s.rec.Log(t, "connection", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Count: cp.Waiting(), Decision: "waiting", Description: fmt.Sprintf("%s pinned waiting for a connection (waitlist %d)", att.ID, cp.Waiting())})
		s.st.recordConnOccupancy(t, att.Upstream)
	}
	return out
}

// executionLatencyFor fixes an attempt's execution duration at its actual
// execution start. M2 extension of A12: a live latency override from the M2
// fault machinery wins (and is logged); otherwise the M1 selection applies.
func (s *Sim) executionLatencyFor(att *domain.PhysicalAttempt, t domain.SimTime) domain.SimDuration {
	if o := s.st.Faults[att.Upstream].LatencyOverride; o != 0 {
		s.rec.Logf(t, "fault", "%s starts with M2 latency override %s on %s", att.ID, o, att.Upstream)
		return o
	}
	return s.st.Upstreams[att.Upstream].ExecutionLatency()
}

// completionFails decides, for one completing attempt, whether the upstream
// fails it. M1 rule (A12): failure iff the upstream is degraded at the
// completion instant. M2 extension: an active elevated-failure mode force-
// fails completions independently of health; the mode's counter counts
// DECISIONS (completions) only, which keeps the k-accounting replay-safe.
// Both rules compose: a completion fails when either rule fires.
func (s *Sim) completionFails(att *domain.PhysicalAttempt, t domain.SimTime) bool {
	ups := s.st.Upstreams[att.Upstream]
	if ups.Degraded {
		return true
	}
	fs := s.st.Faults[att.Upstream]
	if fs.EveryN >= 1 {
		fs.FailCounter++
		if fs.FailCounter%fs.EveryN == 0 {
			s.rec.Logf(t, "fault", "%s completion fails: forced by fail-rate every-%d on %s (decision %d)",
				att.ID, fs.EveryN, att.Upstream, fs.FailCounter)
			s.st.Faults[att.Upstream] = fs
			return true
		}
		s.st.Faults[att.Upstream] = fs
	}
	return false
}

// startRunning moves an attempt (holding a worker) into running and schedules
// its completion. Called from ExecutionStart and ConnectionGranted.
func (s *Sim) startRunning(att *domain.PhysicalAttempt, t domain.SimTime) []engine.Event {
	cp := s.st.Connections[att.Upstream]
	if err := cp.Acquire(att.ID); err != nil {
		panic(err)
	}
	att.ConnHeld = true
	s.rec.Log(t, "connection", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Count: len(cp.Held), Decision: "acquired", Description: fmt.Sprintf("%s acquired a connection on %s (held %d/%d)", att.ID, att.Upstream, len(cp.Held), cp.Capacity)})
	s.attTransition(att, domain.AttemptRunning, t, "connection acquired")
	att.BeganRunningAt = t
	s.st.recordConnWaitEnds(att, t) // M2 metric: closing a granted pinned-wait, if open
	s.st.recordConnOccupancy(t, att.Upstream)
	return []engine.Event{&EventExecutionCompletion{baseEvent: baseEvent{Time: s.addT(t, att.ExecDuration, "execution completion")}, Attempt: att.ID}}
}

// handleConnectionGranted acquires one free connection for the oldest
// *state-valid* waiter on the upstream's waitlist. The event carries no
// attempt payload (docs/ASSUMPTIONS.md A15): eligibility is decided HERE, at
// processing time, never at release time, so stale or ineligible waiters can
// never strand capacity and same-instant releases hand over in waitlist order.
func (s *Sim) handleConnectionGranted(e *EventConnectionGranted) []engine.Event {
	t := e.At()
	cp := s.st.Connections[e.Upstream]
	for len(cp.Waitlist) > 0 {
		head, _ := cp.PopWaiterHead()
		// M2 metric: the record-purged waitlist sample (pop always mutates).
		s.st.recordConnOccupancy(t, e.Upstream)
		att := s.st.Attempts[head]
		if att == nil || att.State != domain.AttemptWaitingConnection {
			// Stale or ineligible waiter (defence in depth: timeout and
			// retraction exits already clean the waitlist at priority 10/20
			// before this work-class event runs).
			s.rec.Logf(t, "connection", "waitlist head %s of %s ineligible (state %s); connection stays free", head, e.Upstream, attStateOf(att))
			continue
		}
		att.ExecDuration = s.executionLatencyFor(att, t) // measured at actual execution start
		s.rec.Log(t, "connection", Detail{Attempt: string(head), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(e.Upstream), Decision: "granted", Description: fmt.Sprintf("%s granted a deferred connection on %s", head, e.Upstream)})
		return s.startRunning(att, t)
	}
	s.rec.Logf(t, "connection", "no eligible waiter on %s; connection unused", e.Upstream)
	return nil
}

func attStateOf(att *domain.PhysicalAttempt) string {
	if att == nil {
		return "<gone>"
	}
	return att.State.String()
}

func (s *Sim) handleExecutionCompletion(e *EventExecutionCompletion) []engine.Event {
	t := e.At()
	att := s.mustAttempt(e.Attempt)
	if att.State != domain.AttemptRunning {
		s.rec.Logf(t, "skip", "%s completion ignored (state %s)", att.ID, att.State)
		return nil
	}
	success := !s.completionFails(att, t) // M1 A12 rule + M2 fail-rate mode (faults.go)

	var out []engine.Event
	out = append(out, s.releaseConnectionWake(att, t)...)
	if success {
		s.finishAttempt(att, domain.AttemptCompleted, t, "execution finished")
	} else if s.st.Upstreams[att.Upstream].Degraded {
		s.finishAttempt(att, domain.AttemptFailed, t, "upstream degraded at completion")
	} else {
		s.finishAttempt(att, domain.AttemptFailed, t, "completion forced to fail (fail-rate mode)")
	}
	out = append(out, s.releaseWorker(att, t)...)
	out = append(out, s.observeBreaker(att.Upstream, success, t)...)

	req := s.mustRequest(att.LogicalID)
	if success {
		s.settleSuccess(req, att, t)
	} else {
		out = append(out, s.scheduleRetryOrConclude(req, att, t, "attempt failed")...)
	}
	s.reconcileCancellation(req, t)
	out = append(out, s.probeDispatch(t)...)
	return out
}

// releaseConnectionWake frees a held connection and, if anyone is pinned
// waiting, emits the payload-free wake event naming only the upstream. The
// grant handler re-derives the eligible waiter at its own processing time;
// duplicate grants at the same instant degrade to observed no-ops and can
// never strand a connection.
func (s *Sim) releaseConnectionWake(att *domain.PhysicalAttempt, t domain.SimTime) []engine.Event {
	if !att.ConnHeld {
		return nil
	}
	cp := s.st.Connections[att.Upstream]
	if err := cp.Release(att.ID); err != nil {
		panic(err)
	}
	att.ConnHeld = false
	s.st.recordConnOccupancy(t, att.Upstream)
	s.rec.Log(t, "connection", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Count: len(cp.Held), Decision: "released", Description: fmt.Sprintf("%s released its connection on %s (held %d/%d)", att.ID, att.Upstream, len(cp.Held), cp.Capacity)})
	if len(cp.Waitlist) == 0 {
		return nil
	}
	return []engine.Event{&EventConnectionGranted{baseEvent: baseEvent{Time: t}, Upstream: cp.Upstream}}
}

// finishAttempt applies a terminal execution state, assigns the attempt's
// outcome (usefulness is a property separate from execution state) and
// releases a reserved probe permit if any.
//
// Idempotency guard: a caller re-entering finishAttempt on an already finalized
// attempt is a no-op (never double-releases a probe permit or re-counts an
// outcome).
func (s *Sim) finishAttempt(att *domain.PhysicalAttempt, final domain.AttemptState, t domain.SimTime, why string) {
	if att.State == final && att.Outcome != domain.OutcomeUndetermined {
		s.rec.Logf(t, "skip", "%s finish ignored: already %s with outcome %s", att.ID, final, att.Outcome)
		return
	}
	s.attTransition(att, final, t, why)
	s.assignOutcome(att, t)
	s.releaseProbePermit(att, t)
}

// releaseProbePermit clears an attempt's probe reservation exactly once.
func (s *Sim) releaseProbePermit(att *domain.PhysicalAttempt, t domain.SimTime) {
	if !att.IsProbe {
		return
	}
	att.IsProbe = false
	br := s.st.Breakers[att.Upstream]
	if br.ProbesActive <= 0 {
		panic(fmt.Sprintf("sim: probe permit ledger underflow releasing %s at %s", att.ID, t))
	}
	br.ProbesActive--
	s.rec.Log(t, "probe-released", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Decision: "released", Description: fmt.Sprintf("%s released a half-open probe permit for %s", att.ID, att.Upstream)})
}

// retractAttempt is the SINGLE retraction exit path: it moves a non-executing
// attempt to AttemptRetracted and guarantees that every held resource is
// released exactly once. It covers
//
//   - queue membership (arrival and retry lanes both settle their pending
//     book here);
//   - connection-waitlist membership;
//   - pending lane books (Created state attempts waiting for insertion);
//   - a reserved circuit-breaker probe permit;
//   - a defensively held worker/connection (no retraction target should hold
//     either; asserting here keeps the ledger honest if one ever does).
//
// Retracting an executing (Assigned/Running) attempt is a programming error:
// those are governed by orphaning, never retraction, so this path panics.
// The path is idempotent: re-application on an already-terminal attempt is an
// observed no-op.
func (s *Sim) retractAttempt(att *domain.PhysicalAttempt, t domain.SimTime, why string) {
	if att.State.Terminal() {
		s.rec.Logf(t, "skip", "%s retraction ignored: already %s (%s)", att.ID, att.State, why)
		return
	}
	switch att.State {
	case domain.AttemptQueued:
		q := s.st.Queues[att.Tenant]
		entry, ok := q.EntryFor(att.ID)
		if !ok {
			panic(fmt.Sprintf("sim: queue invariant violated retracting %s at %s", att.ID, t))
		}
		if !q.Remove(att.ID) {
			panic(fmt.Sprintf("sim: queue invariant violated retracting %s at %s", att.ID, t))
		}
		// M2 metrics: queue occupancy after the removal, plus this attempt's
		// abandoned queue-residence contribution.
		s.st.recordQueueOccupancy(t, att.Tenant)
		s.st.recordQueueDwellAbandonedEnds(att, entry.EnqueuedAt, t)
		// M3 P3: a retraction leaves the queue exactly like a dispatch does, so
		// the typed interval stream records it. Without this effect a lane
		// replay would over-count occupancy forever.
		s.rec.Log(t, "queue", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Lane: attemptLane(att.IsRetry), Seq: entry.Seq, Count: q.Len(), LatencyNS: int64(t.Sub(entry.EnqueuedAt)), Decision: "retract", Description: fmt.Sprintf("%s removed from tenant %s queue by retraction (len %d, waited %s)", att.ID, att.Tenant, q.Len(), t.Sub(entry.EnqueuedAt))})
	case domain.AttemptWaitingConnection:
		if !s.st.Connections[att.Upstream].RemoveWaiter(att.ID) {
			panic(fmt.Sprintf("sim: waitlist invariant violated retracting %s at %s", att.ID, t))
		}
		// M2 metrics: waitlist transition and the abandoned pinned-wait.
		s.st.recordConnOccupancy(t, att.Upstream)
		s.st.recordConnWaitAbandonedEnds(att, t)
	case domain.AttemptCreated, domain.AttemptDispatching:
		// resource-free states; only the pending lane book may apply
	default:
		panic(fmt.Sprintf("sim: illegal retraction of attempt %s in state %s at %s (%s)", att.ID, att.State, t, why))
	}
	if att.ConnHeld {
		// Executing attempts are governed by orphaning (the switch above
		// rejected them); holding a connection here would be a ledger error.
		panic(fmt.Sprintf("sim: attempt %s holds a connection but was retracted at %s", att.ID, t))
	}
	if att.HasWorker {
		// A waiting-connection attempt pins its worker; releasing it here is
		// the worker-recovery half of centralized retraction cleanup.
		s.releaseWorker(att, t)
	}
	st := s.st
	st.settlePending(att)
	s.attTransition(att, domain.AttemptRetracted, t, why)
	s.assignOutcome(att, t)
	s.releaseProbePermit(att, t)
}

// assignOutcome derives usefulness. Attempts retracted before producing a
// result are Discarded; attempts finishing while their logical request can no
// longer use the result (request terminal or attempt orphaned) are Wasted;
// everything else delivered an observable result to a live request (including
// failures and timeouts that drove a retry/failure decision) and is Useful.
func (s *Sim) assignOutcome(att *domain.PhysicalAttempt, t domain.SimTime) {
	if att.Outcome != domain.OutcomeUndetermined {
		return // idempotent
	}
	req := s.st.Requests[att.LogicalID]
	var o domain.AttemptOutcome
	switch {
	case att.State == domain.AttemptRetracted:
		o = domain.OutcomeDiscarded
	case req != nil && req.State.Terminal() || att.Orphaned:
		o = domain.OutcomeWasted
	default:
		o = domain.OutcomeUseful
	}
	att.Outcome = o
	c := s.st.OutcomeCounters[att.Tenant]
	switch o {
	case domain.OutcomeUseful:
		c.Useful++
	case domain.OutcomeWasted:
		c.Wasted++
	case domain.OutcomeDiscarded:
		c.Discarded++
	}
	s.st.OutcomeCounters[att.Tenant] = c
	s.rec.Log(t, "outcome", Detail{Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Outcome: o.String(), Decision: "assigned", Description: fmt.Sprintf("%s outcome %s (%s)", att.ID, o, reasonOf(att))})
}

// reasonOf returns a compact provenance note for outcome logging.
func reasonOf(att *domain.PhysicalAttempt) string {
	switch {
	case att.Orphaned:
		return "orphaned: " + att.OrphanReason
	case att.State == domain.AttemptRetracted:
		return "retracted before result"
	default:
		return "served live logical request"
	}
}

func (s *Sim) releaseWorker(att *domain.PhysicalAttempt, t domain.SimTime) []engine.Event {
	if !att.HasWorker {
		return nil
	}
	pool := s.st.mainPool()
	if err := pool.Release(att.Worker); err != nil {
		panic(err)
	}
	w := att.Worker
	s.st.recordWorkerTimeEnds(att, t) // M2 metric: close the worker-occupancy interval
	s.st.recordWorkerOccupancy(t)
	att.HasWorker = false
	att.Worker = ""
	s.rec.Log(t, "worker", Detail{Worker: string(w), Attempt: string(att.ID), Request: string(att.LogicalID), Tenant: string(att.Tenant), Count: pool.IdleCount(), Decision: "released", Description: fmt.Sprintf("%s released by %s (idle %d)", w, att.ID, pool.IdleCount())})
	return nil
}

// settleSuccess delivers a successful result to the logical request: the
// first success wins and terminalizes the request; sibling attempts are
// governed (retracted if not yet executing, orphaned otherwise) by
// orphanSiblings. Late/orphan successes are recorded as unusable.
func (s *Sim) settleSuccess(req *domain.LogicalRequest, att *domain.PhysicalAttempt, t domain.SimTime) {
	if att.Orphaned || req.State.Terminal() {
		s.rec.Logf(t, "outcome", "%s result not usable: request %s is %s", att.ID, req.ID, req.State)
		return
	}
	s.terminalizeRequest(req, domain.RequestCompleted, t, "attempt "+string(att.ID)+" succeeded")
	s.orphanSiblings(req, att, t, "logical request completed")
}

// orphanSiblings governs the other attempts of a request that just reached a
// terminal state. Attempts that have not started executing are retracted
// through the single retraction path (freed from queue, waitlist, pending
// books and probe permits); attempts already executing are marked orphaned
// and continue to their own completion — their eventual results are accounted
// as wasted work.
func (s *Sim) orphanSiblings(req *domain.LogicalRequest, except *domain.PhysicalAttempt, t domain.SimTime, why string) {
	for _, aid := range req.Attempts {
		if except != nil && aid == except.ID {
			continue
		}
		att := s.st.Attempts[aid]
		if att == nil || att.State.Terminal() || att.Orphaned {
			continue
		}
		switch att.State {
		case domain.AttemptQueued, domain.AttemptWaitingConnection, domain.AttemptCreated, domain.AttemptDispatching:
			s.retractAttempt(att, t, why)
		case domain.AttemptAssigned, domain.AttemptRunning:
			att.Orphaned = true
			att.OrphanedAt = t
			att.OrphanReason = why
			s.rec.Log(t, "orphan", Detail{Attempt: string(att.ID), Request: string(req.ID), Tenant: string(att.Tenant), Upstream: string(att.Upstream), Reason: why, Decision: "orphaned", Description: fmt.Sprintf("%s orphaned: %s", att.ID, why)})
		}
	}
}

// scheduleRetryOrConclude decides what a failed or timed-out attempt means for
// its logical request: schedule a retry (verify limit, logical deadline and
// retry budget), or fail the request and govern siblings.
func (s *Sim) scheduleRetryOrConclude(req *domain.LogicalRequest, att *domain.PhysicalAttempt, t domain.SimTime, why string) []engine.Event {
	if att.Orphaned || req.State.Terminal() {
		return nil
	}
	if req.RetriesUsed >= req.MaxRetries {
		s.terminalizeRequest(req, domain.RequestFailed, t, why+" and retry limit reached")
		s.orphanSiblings(req, att, t, "logical request failed")
		return nil
	}
	if !t.Before(req.Deadline) {
		// Normally unreachable: DeadlineExpired (priority Governance) would
		// have terminalized the request already. Kept as explicit defence.
		s.terminalizeRequest(req, domain.RequestExpired, t, why+" and logical deadline passed")
		s.orphanSiblings(req, att, t, "logical request expired after failure")
		return nil
	}
	if !s.st.ConsumeRetryBudget(t, req.Tenant, req.Upstream) {
		s.terminalizeRequest(req, domain.RequestFailed, t, why+" and retry budget exhausted")
		s.orphanSiblings(req, att, t, "logical request failed")
		return nil
	}
	req.RetriesUsed++
	s.rec.Log(t, "budget", Detail{Request: string(req.ID), Tenant: string(req.Tenant), Upstream: string(req.Upstream), Count: 1, Decision: "consumed", Description: fmt.Sprintf("retry budget (%s,%s) consumed by retry of %s", req.Tenant, req.Upstream, req.ID)})
	s.reqTransition(req, domain.RequestRetrying, t, "retry scheduled after: "+why)
	return []engine.Event{&EventRetryScheduled{
		baseEvent:   baseEvent{Time: t},
		Request:     req.ID,
		Predecessor: att.ID,
		Reason:      why,
	}}
}

func (s *Sim) handleRetryScheduled(e *EventRetryScheduled) []engine.Event {
	t := e.At()
	backoff := s.st.Cfg.RetryBackoff
	return []engine.Event{&EventRetryExecution{baseEvent: baseEvent{Time: s.addT(t, backoff, "retry backoff")}, Request: e.Request}}
}

// handleRetryExecution admits a retry attempt into the retry lane at its true
// execution instant. Admission checks (docs/ENGINE_SEMANTICS.md §4):
//
//   - the breaker must not be OPEN ("retry-blocked-breaker-open");
//   - the breaker must not even be HALF_OPEN — retries can never be probes,
//     because probe results would then be retry-driven and could consume
//     permits without admission control ("retry-blocked-breaker-halfopen");
//   - the retry lane must have room, counting queued retry entries plus
//     pending retry attempts created at this instant ("retry-queue-full");
//   - the explicit total bound must have room ("retry-queue-total-full").
//
// A blocked retry fails its request explicitly and deterministically; the
// retry budget token stays spent (checked at scheduling time, conservative
// and non-refundable — docs/ASSUMPTIONS.md A16).
func (s *Sim) handleRetryExecution(e *EventRetryExecution) []engine.Event {
	t := e.At()
	req := s.mustRequest(e.Request)
	if req.State != domain.RequestRetrying {
		// The request completed, expired or was cancelled while the backoff
		// elapsed — the scheduled retry is obsolete.
		s.rec.Logf(t, "skip", "retry execution for %s ignored (state %s)", req.ID, req.State)
		return nil
	}
	st := s.st
	br := st.Breakers[req.Upstream]
	block := ""
	switch br.State {
	case domain.BreakerOpen:
		block = "retry-blocked-breaker-open"
	case domain.BreakerHalfOpen:
		block = "retry-blocked-breaker-halfopen"
	}
	if block == "" {
		cfg := st.Cfg.Tenants[req.Tenant]
		q := st.Queues[req.Tenant]
		retryUsed := s.countLane(q, true) + st.PendingRetryCounts[req.Tenant]
		totalUsed := q.Len() + st.PendingArrivalCounts[req.Tenant] + st.PendingRetryCounts[req.Tenant]
		if retryUsed >= cfg.MaxRetryQueueDepth {
			block = "retry-queue-full"
		} else if totalUsed >= cfg.MaxTotalQueueDepth {
			block = "retry-queue-total-full"
		}
	}
	if block != "" {
		s.rec.Log(t, "retry-admission", Detail{Request: string(req.ID), Tenant: string(req.Tenant), Upstream: string(req.Upstream), Decision: "blocked", Reason: block, Description: fmt.Sprintf("%s retry blocked: %s (budget token stays spent)", req.ID, block)})
		st.recordShed(req.Tenant, block)
		st.recordBlockedRetry(req.Tenant) // M2 metric: retry admission rejection
		s.terminalizeRequest(req, domain.RequestFailed, t, "retry blocked: "+block)
		s.orphanSiblings(req, nil, t, "logical request failed")
		return nil
	}
	att := s.createAttempt(req, t, true)
	return []engine.Event{&EventQueueInsertion{baseEvent: baseEvent{Time: t}, Attempt: att.ID}}
}

// --- deadline, timeout, cancellation ----------------------------------------

func (s *Sim) handleTimeout(e *EventTimeout) []engine.Event {
	t := e.At()
	att := s.mustAttempt(e.Attempt)
	if att.State != domain.AttemptRunning && att.State != domain.AttemptWaitingConnection {
		s.rec.Logf(t, "skip", "%s timeout ignored (state %s)", att.ID, att.State)
		return nil
	}
	req := s.mustRequest(att.LogicalID)
	var out []engine.Event
	if att.State == domain.AttemptWaitingConnection {
		// Never acquired a connection; leave the waitlist quietly.
		if !s.st.Connections[att.Upstream].RemoveWaiter(att.ID) {
			panic(fmt.Sprintf("sim: waitlist invariant violated timing out %s at %s", att.ID, t))
		}
		// M2 metrics: the abandoned pinned-wait ends here.
		s.st.recordConnOccupancy(t, att.Upstream)
		s.st.recordConnWaitAbandonedEnds(att, t)
	} else if att.ConnHeld {
		out = append(out, s.releaseConnectionWake(att, t)...)
	}
	out = append(out, s.observeBreaker(att.Upstream, false, t)...)
	s.finishAttempt(att, domain.AttemptTimedOut, t, "attempt timeout elapsed")
	out = append(out, s.releaseWorker(att, t)...)
	out = append(out, s.scheduleRetryOrConclude(req, att, t, "attempt timed out")...)
	s.reconcileCancellation(req, t)
	out = append(out, s.probeDispatch(t)...)
	return out
}

func (s *Sim) handleDeadlineExpired(e *EventDeadlineExpired) []engine.Event {
	t := e.At()
	req := s.mustRequest(e.Request)
	if req.State.Terminal() {
		s.rec.Logf(t, "skip", "deadline ignored for %s (state %s)", req.ID, req.State)
		return nil
	}
	if req.State == domain.RequestCancelling {
		// Documented decision (docs/ASSUMPTIONS.md): an accepted cancellation
		// supersedes a deadline hit at the same instant; expiry is redundant.
		s.rec.Logf(t, "skip", "deadline ignored for %s: cancellation already in progress", req.ID)
		return nil
	}
	s.terminalizeRequest(req, domain.RequestExpired, t, "logical deadline reached")
	s.orphanSiblings(req, nil, t, "logical request expired")
	return nil
}

func (s *Sim) handleCancellationRequested(e *EventCancellationRequested) []engine.Event {
	t := e.At()
	req, ok := s.st.Requests[e.Request]
	if !ok {
		// Documented semantics (docs/ASSUMPTIONS.md A14): cancelling an
		// unknown request is a deterministic, recorded no-op. It never panics
		// and never perturbs any other entity.
		s.rec.Log(t, "cancellation", Detail{Request: string(e.Request), Reason: e.Reason, Description: fmt.Sprintf("cancellation of unknown request %s: deterministic no-op", e.Request)})
		return nil
	}
	if req.State.Terminal() || req.State == domain.RequestCancelling {
		s.rec.Logf(t, "skip", "cancellation ignored for %s (state %s)", req.ID, req.State)
		return nil
	}
	s.reqTransition(req, domain.RequestCancelling, t, "cancellation: "+e.Reason)
	outstanding := 0
	for _, aid := range req.Attempts {
		att := s.st.Attempts[aid]
		switch att.State {
		case domain.AttemptQueued, domain.AttemptWaitingConnection, domain.AttemptCreated, domain.AttemptDispatching:
			s.retractAttempt(att, t, "cancelled")
		case domain.AttemptAssigned, domain.AttemptRunning:
			// In-flight execution is not interrupted: it continues to its
			// own completion and is accounted as wasted work.
			att.Orphaned = true
			att.OrphanedAt = t
			att.OrphanReason = "cancelled while executing"
			outstanding++
		default:
			// already terminal — nothing to govern
		}
	}
	return []engine.Event{&EventCancellationAcknowledged{
		baseEvent:   baseEvent{Time: t},
		Request:     req.ID,
		Outstanding: outstanding,
	}}
}

func (s *Sim) handleCancellationAcknowledged(e *EventCancellationAcknowledged) []engine.Event {
	t := e.At()
	req := s.mustRequest(e.Request)
	if e.Outstanding == 0 && req.State == domain.RequestCancelling {
		s.terminalizeRequest(req, domain.RequestCancelled, t, "cancellation fully applied; no in-flight work")
	}
	// Otherwise the request stays cancelling until the last orphaned attempt
	// finishes (see reconcileCancellation).
	return nil
}

// reconcileCancellation terminalizes a cancelling request once none of its
// attempts is executing anymore. Called after every attempt finalization.
func (s *Sim) reconcileCancellation(req *domain.LogicalRequest, t domain.SimTime) {
	if req.State != domain.RequestCancelling {
		return
	}
	for _, aid := range req.Attempts {
		if att := s.st.Attempts[aid]; att.ExecutionActive() {
			return
		}
	}
	s.terminalizeRequest(req, domain.RequestCancelled, t, "all in-flight attempts finished; cancellation applied")
}

// --- environment & breaker ---------------------------------------------------

func (s *Sim) handleUpstreamDegraded(e *EventUpstreamDegraded) []engine.Event {
	ups := s.st.Upstreams[e.Upstream]
	if ups == nil {
		panic(fmt.Sprintf("sim: degradation of unknown upstream %s", e.Upstream))
	}
	ups.Degraded = true
	s.rec.Log(e.At(), "upstream", Detail{Upstream: string(ups.ID), Decision: "degraded", Description: fmt.Sprintf("%s degraded (exec latency now %s)", ups.ID, ups.ExecutionLatency())})
	return nil
}

func (s *Sim) handleUpstreamRecovered(e *EventUpstreamRecovered) []engine.Event {
	ups := s.st.Upstreams[e.Upstream]
	if ups == nil {
		panic(fmt.Sprintf("sim: recovery of unknown upstream %s", e.Upstream))
	}
	ups.Degraded = false
	s.rec.Log(e.At(), "upstream", Detail{Upstream: string(ups.ID), Decision: "recovered", Description: fmt.Sprintf("%s recovered (exec latency now %s)", ups.ID, ups.ExecutionLatency())})
	return nil
}

// handleUpstreamLatencySet applies or clears an M2 latency override. The M1
// degraded flag is untouched; the override simply wins at execution start
// (executionLatencyFor) while it is non-zero.
func (s *Sim) handleUpstreamLatencySet(e *EventUpstreamLatencySet) []engine.Event {
	t := e.At()
	ups := s.st.Upstreams[e.Upstream]
	if ups == nil {
		panic(fmt.Sprintf("sim: latency set of unknown upstream %s", e.Upstream))
	}
	fs := s.st.Faults[e.Upstream]
	fs.LatencyOverride = e.Latency
	s.st.Faults[e.Upstream] = fs
	if e.Latency == 0 {
		s.rec.Log(t, "fault", Detail{Upstream: string(ups.ID), Decision: "cleared", Description: fmt.Sprintf("%s latency override cleared (exec latency back to %s)", ups.ID, ups.ExecutionLatency())})
	} else {
		s.rec.Log(t, "fault", Detail{Upstream: string(ups.ID), Decision: "set", LatencyNS: int64(e.Latency), Description: fmt.Sprintf("%s latency override set to %s for new execution starts", ups.ID, e.Latency)})
	}
	return nil
}

// handleUpstreamFailureSet applies or clears an M2 elevated-failure mode.
// EveryN >= 1 arms the deterministic completion counter; 0 disarms it and
// resets the counter, because an empty schedule position must reproduce the
// same decision sequence as a fresh state (keeps Restore semantics local to
// the mode that set it).
func (s *Sim) handleUpstreamFailureSet(e *EventUpstreamFailureSet) []engine.Event {
	t := e.At()
	ups := s.st.Upstreams[e.Upstream]
	if ups == nil {
		panic(fmt.Sprintf("sim: failure set of unknown upstream %s", e.Upstream))
	}
	fs := s.st.Faults[e.Upstream]
	fs.EveryN = e.EveryN
	if e.EveryN == 0 {
		fs.FailCounter = 0
	}
	s.st.Faults[e.Upstream] = fs
	if e.EveryN == 0 {
		s.rec.Log(t, "fault", Detail{Upstream: string(ups.ID), Decision: "cleared", Description: fmt.Sprintf("%s fail-rate mode cleared (completion-time failure rule unchanged)", ups.ID)})
	} else {
		s.rec.Log(t, "fault", Detail{Upstream: string(ups.ID), Decision: "set", Count: e.EveryN, Description: fmt.Sprintf("%s fail-rate mode set: every %dth completion fails", ups.ID, e.EveryN)})
	}
	return nil
}

func (s *Sim) handleBreakerTransitioned(e *EventBreakerTransitioned) []engine.Event {
	t := e.At()
	br := s.st.Breakers[e.Upstream]
	if br.State != e.From {
		// Stale transition (e.g. a scheduled half-open event arriving after
		// a re-open). Ignoring keeps the machine total without event
		// cancellation machinery.
		s.rec.Logf(t, "skip", "stale breaker transition %s→%s ignored (current %s)", e.From, e.To, br.State)
		return nil
	}
	br.State = e.To
	s.rec.Log(t, "breaker", Detail{Upstream: string(e.Upstream), From: e.From.String(), To: e.To.String(), Description: fmt.Sprintf("%s transitioned %s -> %s (%s)", e.Upstream, e.From, e.To, e.Reason)})
	var out []engine.Event
	switch e.To {
	case domain.BreakerOpen, domain.BreakerHalfOpen:
		// Both states begin a fresh permit ledger. Any attempt still holding
		// an IsProbe flag from an earlier epoch must have that flag cleared
		// each by name, deterministically ordered, so that the exact probe
		// ledger (Σ IsProbe == ProbesActive) holds everywhere. Re-open is the
		// historical stuck case; half-open normally starts from zero.
		if br.ProbesActive > 0 || s.st.probeAttemptsOf(e.Upstream) != 0 {
			s.clearProbeLedger(e.Upstream, t)
		}
		br.ProbesActive = 0
		if e.To == domain.BreakerOpen {
			br.OpenedAt = t
			out = append(out, &EventBreakerTransitioned{
				baseEvent: baseEvent{Time: s.addT(t, s.st.Cfg.Breaker.Cooldown, "breaker cooldown")},
				Upstream:  e.Upstream,
				From:      domain.BreakerOpen,
				To:        domain.BreakerHalfOpen,
				Reason:    "cooldown elapsed",
			})
		}
	case domain.BreakerClosed:
		br.ConsecutiveFailures = 0
	}
	return out
}

// probeAttemptsOf counts attempts holding a probe permit for the upstream
// (mirror of the Validate probe ledger without the error reporting).
func (st *SystemState) probeAttemptsOf(up domain.UpstreamID) int {
	n := 0
	for _, att := range st.Attempts {
		if att.IsProbe && att.Upstream == up {
			n++
		}
	}
	return n
}

// clearProbeLedger releases every outstanding probe permit of an upstream,
// in deterministic attempt-ID order. Used on re-open and fresh half-open.
func (s *Sim) clearProbeLedger(up domain.UpstreamID, t domain.SimTime) {
	ids := make([]domain.AttemptID, 0)
	for aid, att := range s.st.Attempts {
		if att.IsProbe && att.Upstream == up {
			ids = append(ids, aid)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, aid := range ids {
		att := s.st.Attempts[aid]
		att.IsProbe = false
		s.rec.Log(t, "probe-released", Detail{Attempt: string(aid), Request: string(att.LogicalID), Tenant: string(att.Tenant), Upstream: string(up), Decision: "reset", Description: fmt.Sprintf("%s probe flag cleared by breaker reset on %s", aid, up)})
	}
}

// observeBreaker feeds an attempt result into the breaker state machine and
// returns any transition events caused by it.
func (s *Sim) observeBreaker(up domain.UpstreamID, success bool, t domain.SimTime) []engine.Event {
	br := s.st.Breakers[up]
	switch br.State {
	case domain.BreakerClosed:
		if success {
			br.ConsecutiveFailures = 0
			return nil
		}
		br.ConsecutiveFailures++
		s.rec.Log(t, "breaker", Detail{Upstream: string(up), Count: br.ConsecutiveFailures, Decision: "failure-counted", Description: fmt.Sprintf("%s failure counted (%d consecutive)", up, br.ConsecutiveFailures)})
		if br.ConsecutiveFailures >= s.st.Cfg.Breaker.FailureThreshold {
			return []engine.Event{&EventBreakerTransitioned{
				baseEvent: baseEvent{Time: t},
				Upstream:  up,
				From:      domain.BreakerClosed,
				To:        domain.BreakerOpen,
				Reason:    "failure threshold reached",
			}}
		}
	case domain.BreakerHalfOpen:
		if success {
			return []engine.Event{&EventBreakerTransitioned{
				baseEvent: baseEvent{Time: t},
				Upstream:  up,
				From:      domain.BreakerHalfOpen,
				To:        domain.BreakerClosed,
				Reason:    "probe succeeded",
			}}
		}
		return []engine.Event{&EventBreakerTransitioned{
			baseEvent: baseEvent{Time: t},
			Upstream:  up,
			From:      domain.BreakerHalfOpen,
			To:        domain.BreakerOpen,
			Reason:    "probe failed",
		}}
	case domain.BreakerOpen:
		// In-flight results neither extend nor reopen the open state.
		return nil
	}
	return nil
}

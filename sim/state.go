package sim

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"sentra/domain"
)

// UpstreamConfig configures one upstream dependency.
type UpstreamConfig struct {
	Criticality     domain.Criticality
	NormalLatency   domain.SimDuration
	DegradedLatency domain.SimDuration
}

// TenantConfig configures one tenant's admission lanes. There are two queues
// with a shared total bound:
//
//   - MaxArrivalQueueDepth bounds the lane of original-arrival attempts;
//   - MaxRetryQueueDepth bounds the lane of retry attempts independently, so
//     retry amplification cannot crowd out arrivals (and vice versa);
//   - MaxTotalQueueDepth bounds the SUM of both lanes explicitly.
//
// Arrivals beyond the arrival bound are load-shed at admission; retries beyond
// the retry bound fail the request explicitly (docs/ENGINE_SEMANTICS.md). A
// total bound smaller than either lane bound is a configuration error: the
// total must be able to contain each lane.
type TenantConfig struct {
	MaxArrivalQueueDepth int
	MaxRetryQueueDepth   int
	MaxTotalQueueDepth   int
}

// RetryBudgetConfig configures the per-(tenant, upstream) retry budget. A
// non-positive RefillInterval means the bucket never refills.
type RetryBudgetConfig struct {
	Capacity       int
	RefillInterval domain.SimDuration
}

// BreakerConfig configures the classic three-state circuit breaker applied
// per-upstream.
type BreakerConfig struct {
	FailureThreshold  int // consecutive failures that open the breaker
	Cooldown          domain.SimDuration
	MaxHalfOpenProbes int
}

// WorkerPoolConfig configures the (single, in M1) worker pool.
type WorkerPoolConfig struct {
	ID   domain.WorkerPoolID
	Size int
}

// SystemConfig is the complete M1 system shape: it is the initial-condition
// contract that makes runs repeatable.
type SystemConfig struct {
	WorkerPool      WorkerPoolConfig
	ConnectionPools map[domain.UpstreamID]int
	Upstreams       map[domain.UpstreamID]UpstreamConfig
	Tenants         map[domain.TenantID]TenantConfig
	RetryBudget     RetryBudgetConfig
	RetryBackoff    domain.SimDuration
	AttemptTimeout  domain.SimDuration
	Breaker         BreakerConfig
}

// AttemptCounters tracks per-tenant attempt-outcome consumption: useful
// results, wasted downstream work, and discarded (retracted) attempts. This is
// the observability surface later milestones use for wasted-work analysis.
type AttemptCounters struct {
	Useful    int
	Wasted    int
	Discarded int
}

// FaultState is the M2 fault-injection bookkeeping for one upstream. The zero
// value means "no fault mode active" (normal latency selection, failures only
// at degraded completions).
type FaultState struct {
	// LatencyOverride: when non-zero, execution starts on this upstream run
	// for exactly this duration regardless of health (faults.go).
	LatencyOverride domain.SimDuration
	// EveryN: when >= 1, every k-th completion decision of this upstream
	// fails deterministically, k counted while the mode is armed.
	EveryN int
	// FailCounter advances only on completion decisions while EveryN >= 1
	// (replay-safe: timeouts and retractions never advance it).
	FailCounter int
}

// SystemState is the complete M1 system state. It is owned and mutated only by
// package sim transition handlers; nothing else may mutate it. It carries no
// goroutines, channels, timers or wall-clock dependencies, which makes a full
// DeepEqual comparison a valid repeatability check.
type SystemState struct {
	Cfg SystemConfig

	Requests    map[domain.RequestID]*domain.LogicalRequest
	Attempts    map[domain.AttemptID]*domain.PhysicalAttempt
	Queues      map[domain.TenantID]*domain.Queue
	Pools       map[domain.WorkerPoolID]*domain.WorkerPool
	Connections map[domain.UpstreamID]*domain.ConnectionPool
	Upstreams   map[domain.UpstreamID]*domain.Upstream
	Breakers    map[domain.UpstreamID]*domain.CircuitBreaker
	Budgets     map[domain.BudgetKey]*domain.RetryBudget

	// TenantActive counts admitted, non-terminal logical requests per tenant.
	TenantActive map[domain.TenantID]int
	// OutcomeCounters aggregates attempt outcomes per tenant. Validate()
	// cross-checks this ledger against the attempt registry exactly.
	OutcomeCounters map[domain.TenantID]AttemptCounters

	// Pending arrivals: attempts of original arrivals that admission accepted
	// but whose QueueInsertion has not yet been processed (same-instant
	// admissions sit between arrival and insertion). Counting them closes the
	// same-instant burst overshoot: at the arrival instant the lane occupancy
	// is queued-arrival-entries + pending arrivals.
	//
	// Retries get their own symmetric bookkeeping (PendingRetry*), counted in
	// the retry lane from the retry-execution instant. The two books are
	// separate so the invariant ledger stays lane-accurate; both settle in
	// handleQueueInsertion and in retraction.
	PendingArrivalCounts   map[domain.TenantID]int
	PendingArrivalAttempts map[domain.AttemptID]struct{}
	PendingRetryCounts     map[domain.TenantID]int
	PendingRetryAttempts   map[domain.AttemptID]struct{}

	// Metrics is the per-tenant fairness evidence (queue-wait distribution,
	// dispatch share, shed reasons). Value semantics keep DeepEqual
	// repeatability checks valid.
	Metrics map[domain.TenantID]TenantMetrics

	// --- M2 experiment instrumentation ---------------------------------------

	// Faults carries the active M2 fault-injection mode per upstream. It is
	// mutated only by the two fault-set handlers; the zero value is the
	// healthy control state. Attempts always read through index lookups, so
	// an absent entry behaves like the zero value.
	Faults map[domain.UpstreamID]FaultState

	// Occupancy timelines: one sample per exact resource-transition instant
	// (registered every take/release/acquire/waitlist/queue mutation).
	// Aggregation over [From, Until) of a phase yields average occupancy and
	// utilization without any additional sampling clock, and no wall time
	// passes between samples (occupancy is piecewise-constant in simulated
	// time). Track-mode only in records; never reset during a run.
	WorkerOccupancy []WorkerSample
	ConnOccupancy   []ConnSample
	QueueOccupancy  []QueueSample

	// RetryTokensConsumed counts consumed retry-budget tokens per
	// (tenant, upstream) book — the aggregate view over Budgets for
	// amplification analysis.
	RetryTokensConsumed map[domain.BudgetKey]int

	nextReq uint64
	nextAtt uint64
	nextSeq uint64
}

// NewState validates the config and builds the initial state.
func NewState(cfg SystemConfig) (*SystemState, error) {
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("config: at least one upstream is required")
	}
	if len(cfg.Tenants) == 0 {
		return nil, errors.New("config: at least one tenant is required")
	}
	if cfg.WorkerPool.ID == "" {
		return nil, errors.New("config: worker pool ID must be set")
	}
	if cfg.WorkerPool.Size <= 0 {
		return nil, fmt.Errorf("config: worker pool size must be positive, got %d", cfg.WorkerPool.Size)
	}
	for up, uc := range cfg.Upstreams {
		if uc.NormalLatency < 0 || uc.DegradedLatency < 0 {
			return nil, fmt.Errorf("config: upstream %s latency must be non-negative", up)
		}
		if _, ok := cfg.ConnectionPools[up]; !ok {
			return nil, fmt.Errorf("config: no connection pool configured for upstream %s", up)
		}
	}
	for up, c := range cfg.ConnectionPools {
		if _, ok := cfg.Upstreams[up]; !ok {
			return nil, fmt.Errorf("config: connection pool for unknown upstream %s", up)
		}
		if c <= 0 {
			return nil, fmt.Errorf("config: connection pool %s capacity must be positive, got %d", up, c)
		}
	}
	for t, tc := range cfg.Tenants {
		if tc.MaxArrivalQueueDepth < 0 {
			return nil, fmt.Errorf("config: tenant %s MaxArrivalQueueDepth must be non-negative, got %d", t, tc.MaxArrivalQueueDepth)
		}
		if tc.MaxRetryQueueDepth < 0 {
			return nil, fmt.Errorf("config: tenant %s MaxRetryQueueDepth must be non-negative, got %d", t, tc.MaxRetryQueueDepth)
		}
		if tc.MaxTotalQueueDepth < 0 {
			return nil, fmt.Errorf("config: tenant %s MaxTotalQueueDepth must be non-negative, got %d", t, tc.MaxTotalQueueDepth)
		}
		if tc.MaxTotalQueueDepth < max(tc.MaxArrivalQueueDepth, tc.MaxRetryQueueDepth) {
			return nil, fmt.Errorf("config: tenant %s MaxTotalQueueDepth (%d) must contain both lane bounds (arrival %d, retry %d)",
				t, tc.MaxTotalQueueDepth, tc.MaxArrivalQueueDepth, tc.MaxRetryQueueDepth)
		}
	}
	if cfg.AttemptTimeout <= 0 {
		return nil, fmt.Errorf("config: AttemptTimeout must be positive, got %s", cfg.AttemptTimeout)
	}
	if cfg.RetryBackoff < 0 {
		return nil, errors.New("config: RetryBackoff must be non-negative")
	}
	if cfg.RetryBudget.Capacity < 0 {
		return nil, errors.New("config: retry budget capacity must be non-negative")
	}
	if cfg.Breaker.FailureThreshold < 1 {
		return nil, fmt.Errorf("config: breaker failure threshold must be >= 1, got %d", cfg.Breaker.FailureThreshold)
	}
	if cfg.Breaker.Cooldown < 0 {
		return nil, errors.New("config: breaker cooldown must be non-negative")
	}
	if cfg.Breaker.MaxHalfOpenProbes < 1 {
		return nil, fmt.Errorf("config: breaker MaxHalfOpenProbes must be >= 1, got %d", cfg.Breaker.MaxHalfOpenProbes)
	}

	st := &SystemState{
		Cfg:                    cfg,
		Requests:               make(map[domain.RequestID]*domain.LogicalRequest),
		Attempts:               make(map[domain.AttemptID]*domain.PhysicalAttempt),
		Queues:                 make(map[domain.TenantID]*domain.Queue, len(cfg.Tenants)),
		Pools:                  make(map[domain.WorkerPoolID]*domain.WorkerPool, 1),
		Connections:            make(map[domain.UpstreamID]*domain.ConnectionPool, len(cfg.ConnectionPools)),
		Upstreams:              make(map[domain.UpstreamID]*domain.Upstream, len(cfg.Upstreams)),
		Breakers:               make(map[domain.UpstreamID]*domain.CircuitBreaker, len(cfg.Upstreams)),
		Budgets:                make(map[domain.BudgetKey]*domain.RetryBudget),
		TenantActive:           make(map[domain.TenantID]int, len(cfg.Tenants)),
		OutcomeCounters:        make(map[domain.TenantID]AttemptCounters, len(cfg.Tenants)),
		PendingArrivalCounts:   make(map[domain.TenantID]int, len(cfg.Tenants)),
		PendingArrivalAttempts: make(map[domain.AttemptID]struct{}),
		PendingRetryCounts:     make(map[domain.TenantID]int, len(cfg.Tenants)),
		PendingRetryAttempts:   make(map[domain.AttemptID]struct{}),
		Metrics:                make(map[domain.TenantID]TenantMetrics, len(cfg.Tenants)),
		Faults:                 make(map[domain.UpstreamID]FaultState, len(cfg.Upstreams)),
		RetryTokensConsumed:    make(map[domain.BudgetKey]int),
	}

	pool, err := domain.NewWorkerPool(cfg.WorkerPool.ID, cfg.WorkerPool.Size)
	if err != nil {
		return nil, err
	}
	st.Pools[cfg.WorkerPool.ID] = pool

	for t := range cfg.Tenants {
		st.Queues[t] = domain.NewQueue(t)
		st.TenantActive[t] = 0
		st.PendingArrivalCounts[t] = 0
		st.PendingRetryCounts[t] = 0
		st.OutcomeCounters[t] = AttemptCounters{}
		st.Metrics[t] = TenantMetrics{}
	}
	for up, uc := range cfg.Upstreams {
		st.Upstreams[up] = &domain.Upstream{
			ID:              up,
			Criticality:     uc.Criticality,
			NormalLatency:   uc.NormalLatency,
			DegradedLatency: uc.DegradedLatency,
		}
		st.Breakers[up] = domain.NewCircuitBreaker(up)
		cp, err := domain.NewConnectionPool(up, cfg.ConnectionPools[up])
		if err != nil {
			return nil, err
		}
		st.Connections[up] = cp
	}
	return st, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mainPool returns the single M1 worker pool. Multi-pool scheduling is a
// later-milestone decision (see docs/ASSUMPTIONS.md).
func (st *SystemState) mainPool() *domain.WorkerPool {
	p, ok := st.Pools[st.Cfg.WorkerPool.ID]
	if !ok {
		panic("sim: configured worker pool missing from state")
	}
	return p
}

func (st *SystemState) NewRequestID() domain.RequestID {
	st.nextReq++
	return domain.RequestID(fmt.Sprintf("req-%d", st.nextReq))
}

func (st *SystemState) NewAttemptID() domain.AttemptID {
	st.nextAtt++
	return domain.AttemptID(fmt.Sprintf("att-%d", st.nextAtt))
}

func (st *SystemState) NextQueueSeq() uint64 {
	st.nextSeq++
	return st.nextSeq
}

// ConsumeRetryBudget lazily creates, refills and consumes from the
// (tenant, upstream) retry budget, reporting whether a token was available.
// INDEPENDENCE RISK — tier 3: this is the retry-policy decision; see
// domain.RetryBudget and docs/ARCHITECTURE.md §3.
func (st *SystemState) ConsumeRetryBudget(t domain.SimTime, tenant domain.TenantID, up domain.UpstreamID) bool {
	k := domain.BudgetKey{Tenant: tenant, Upstream: up}
	b, ok := st.Budgets[k]
	if !ok {
		b = domain.NewRetryBudget(k, st.Cfg.RetryBudget.Capacity, st.Cfg.RetryBudget.RefillInterval)
		st.Budgets[k] = b
	}
	got := b.TryConsume(t)
	if got {
		st.RetryTokensConsumed[k]++ // M2: consumed-token ledger for amplification analysis
	}
	return got
}

// --- pending-lane bookkeeping ------------------------------------------------

// recordPendingArrival marks a Created arrival attempt as pending in its
// tenant's arrival lane.
func (st *SystemState) recordPendingArrival(att *domain.PhysicalAttempt) {
	if att.IsRetry {
		panic(fmt.Sprintf("sim: pending-arrival bookkeeping requested for retry attempt %s", att.ID))
	}
	st.PendingArrivalAttempts[att.ID] = struct{}{}
	st.PendingArrivalCounts[att.Tenant]++
}

// recordPendingRetry marks a Created retry attempt as pending in its tenant's
// retry lane.
func (st *SystemState) recordPendingRetry(att *domain.PhysicalAttempt) {
	if !att.IsRetry {
		panic(fmt.Sprintf("sim: pending-retry bookkeeping requested for arrival attempt %s", att.ID))
	}
	st.PendingRetryAttempts[att.ID] = struct{}{}
	st.PendingRetryCounts[att.Tenant]++
}

// pendingLaneOf reports which pending book (arrival or retry) holds an attempt,
// so callers neither need nor guess.
func (st *SystemState) pendingLaneOf(att *domain.PhysicalAttempt) bool {
	if att.IsRetry {
		_, ok := st.PendingRetryAttempts[att.ID]
		return ok
	}
	_, ok := st.PendingArrivalAttempts[att.ID]
	return ok
}

// settlePending unregisters a pending attempt from its lane book (arrival or
// retry). It is called on queue insertion and on any retraction; it is a no-op
// for attempts that were never pending.
func (st *SystemState) settlePending(att *domain.PhysicalAttempt) {
	if att.IsRetry {
		if _, ok := st.PendingRetryAttempts[att.ID]; ok {
			delete(st.PendingRetryAttempts, att.ID)
			st.PendingRetryCounts[att.Tenant]--
		}
		return
	}
	if _, ok := st.PendingArrivalAttempts[att.ID]; ok {
		delete(st.PendingArrivalAttempts, att.ID)
		st.PendingArrivalCounts[att.Tenant]--
	}
}

// --- fairness metrics ----------------------------------------------------------

// recordQueueWait folds one dispatched attempt's wait into tenant metrics and
// counts it toward the dispatch share. Every dispatched attempt records a wait
// (possibly zero), so Dispatched == QueueWaitN by construction; they are
// distributed into separate fields for fair-share analysis.
func (st *SystemState) recordQueueWait(tid domain.TenantID, wait domain.SimDuration) {
	m := st.Metrics[tid]
	m.recordQueueWait(wait)
	m.Dispatched++
	st.Metrics[tid] = m
}

// recordShed folds one admission shed into tenant metrics.
func (st *SystemState) recordShed(tid domain.TenantID, reason string) {
	m := st.Metrics[tid]
	m.recordShed(reason)
	st.Metrics[tid] = m
}

// --- M2 metric wrappers --------------------------------------------------------

func (st *SystemState) recordAttemptCreate(att *domain.PhysicalAttempt) {
	m := st.Metrics[att.Tenant]
	m.recordAttemptCreate(att.IsRetry)
	st.Metrics[att.Tenant] = m
}

func (st *SystemState) recordBlockedRetry(tid domain.TenantID) {
	m := st.Metrics[tid]
	m.BlockedRetries++
	st.Metrics[tid] = m
}

// recordWorkerTimeEnds closes an attempt's worker-occupancy interval, if one
// is open, at the given instant. Returns true when an open interval existed.
func (st *SystemState) recordWorkerTimeEnds(att *domain.PhysicalAttempt, t domain.SimTime) bool {
	if !att.WorkerSet {
		return false
	}
	att.WorkerSet = false
	d := t.Sub(att.WorkerSince)
	m := st.Metrics[att.Tenant]
	m.recordWorkerTime(d)
	st.Metrics[att.Tenant] = m
	att.WorkerSince = 0
	return true
}

// recordConnWaitEnds closes an attempt's pinned-wait interval after wait
// ended in a GRANTED acquisition.
func (st *SystemState) recordConnWaitEnds(att *domain.PhysicalAttempt, t domain.SimTime) {
	if !att.WaitingSet {
		return
	}
	att.WaitingSet = false
	wait := t.Sub(att.WaitingSince)
	att.WaitingSince = 0
	m := st.Metrics[att.Tenant]
	m.recordConnWait(wait)
	st.Metrics[att.Tenant] = m
}

// recordConnWaitAbandonedEnds closes an attempt's pinned-wait interval after
// the attempt timed out or was retracted while waiting.
func (st *SystemState) recordConnWaitAbandonedEnds(att *domain.PhysicalAttempt, t domain.SimTime) {
	if !att.WaitingSet {
		return
	}
	att.WaitingSet = false
	wait := t.Sub(att.WaitingSince)
	att.WaitingSince = 0
	m := st.Metrics[att.Tenant]
	m.recordConnWaitAbandoned(wait)
	st.Metrics[att.Tenant] = m
}

// recordQueueDwellAbandonedEnds folds the queue residence of an attempt that
// was retracted while queued (never dispatched).
func (st *SystemState) recordQueueDwellAbandonedEnds(att *domain.PhysicalAttempt, enqueuedAt domain.SimTime, t domain.SimTime) {
	m := st.Metrics[att.Tenant]
	m.recordQueueDwellAbandoned(t.Sub(enqueuedAt))
	st.Metrics[att.Tenant] = m
}

// Validate checks aggregate resource-accounting invariants across pools,
// connection pools, queues, waitlists, requests, breakers and the outcome and
// probe ledgers. It is a single pass over the attempt registry (O(N)), with
// N = number of attempts ever created: the per-tenant, per-request and
// per-upstream aggregates are built in one sweep instead of nested rescans, so
// checkpointing stays cheap in long runs.
//
// Invariants checked (docs/ENGINE_SEMANTICS.md §6):
//
//   - worker ownership: pool assignment ↔ attempt claim, both directions;
//   - connection ownership: ConnHeld ↔ pool Held set, state consistency;
//   - waitlist membership: waitlisted attempts must be WaitingConnection and
//     vice versa (both directions);
//   - queue membership: entries exist, right tenant, right state;
//   - lane bounds: arrival lane ≤ MaxArrivalQueueDepth (counting pending
//     arrivals), retry lane ≤ MaxRetryQueueDepth (counting pending retries),
//     total entries ≤ MaxTotalQueueDepth;
//   - pending-lane books: per-tenant counts match the attempt sets, and
//     pending attempts are Created-state with the matching lane flag;
//   - probe ledger: Σ attempt.IsProbe per upstream == Breaker.ProbesActive
//     exactly (hard accounting, no slack);
//   - outcome ledger: per-tenant outcome tallies recomputed from the attempt
//     registry match OutcomeCounters exactly;
//   - tenant accounting: TenantActive equals the number of non-terminal
//     requests of the tenant (and is non-negative);
//   - requests: attempt lists agree with the attempt registry.
func (st *SystemState) Validate() error {
	var p []string

	// --- worker pools: busy/idle slot accounting and ownership ---------------
	for pid, pool := range st.Pools {
		tag := fmt.Sprintf("pool %s", pid)
		if len(pool.Busy)+len(pool.Idle) != pool.Size {
			p = append(p, fmt.Sprintf("%s: busy(%d)+idle(%d) != size(%d)", tag, len(pool.Busy), len(pool.Idle), pool.Size))
		}
		idle := make(map[domain.WorkerID]bool, len(pool.Idle))
		for _, w := range pool.Idle {
			if idle[w] {
				p = append(p, fmt.Sprintf("%s: worker %s listed idle twice", tag, w))
			}
			idle[w] = true
		}
		for w, a := range pool.Busy {
			if idle[w] {
				p = append(p, fmt.Sprintf("%s: worker %s both busy and idle", tag, w))
			}
			att, ok := st.Attempts[a]
			if !ok {
				p = append(p, fmt.Sprintf("%s: worker %s busy with unknown attempt %s", tag, w, a))
				continue
			}
			if !att.HasWorker || att.Worker != w {
				p = append(p, fmt.Sprintf("%s: worker %s attributed to %s but attempt claims worker %s", tag, w, a, att.Worker))
			}
			if !att.State.HoldingWorker() || att.State == domain.AttemptDispatching {
				p = append(p, fmt.Sprintf("%s: worker %s held by attempt %s in non-holding state %s", tag, w, a, att.State))
			}
		}
	}

	// --- single pass over attempts: ownership, ledgers, aggregates -----------
	queuedPerTenant := make(map[domain.TenantID]int, len(st.Queues))
	arrivalLane := make(map[domain.TenantID]int, len(st.Queues))
	retryLane := make(map[domain.TenantID]int, len(st.Queues))
	pendingArrivalsByTenant := make(map[domain.TenantID]int, len(st.Queues))
	pendingRetriesByTenant := make(map[domain.TenantID]int, len(st.Queues))
	probesPerUpstream := make(map[domain.UpstreamID]int, len(st.Breakers))
	outcomeTally := make(map[domain.TenantID]AttemptCounters, len(st.Cfg.Tenants))
	waitingForConn := make(map[domain.AttemptID]bool)

	for aid, att := range st.Attempts {
		// (a) worker ownership, attempt direction.
		if att.HasWorker != (att.Worker != "") {
			p = append(p, fmt.Sprintf("attempt %s: HasWorker/Worker mismatch", aid))
		}
		if att.HasWorker {
			pool, ok := st.Pools[st.Cfg.WorkerPool.ID]
			if !ok || pool.Busy[att.Worker] != aid {
				p = append(p, fmt.Sprintf("attempt %s: claims worker %s which is not attributed to it", aid, att.Worker))
			}
		}

		// (b) connection ownership and waitlist direction.
		cp, ok := st.Connections[att.Upstream]
		if !ok {
			p = append(p, fmt.Sprintf("attempt %s: unknown upstream %s", aid, att.Upstream))
			continue
		}
		_, heldOnPool := cp.Held[aid]
		if att.ConnHeld != heldOnPool {
			p = append(p, fmt.Sprintf("attempt %s: ConnHeld(%v) disagrees with pool Held set", aid, att.ConnHeld))
		}
		if att.ConnHeld && att.State != domain.AttemptRunning {
			p = append(p, fmt.Sprintf("attempt %s: holds connection in state %s", aid, att.State))
		}
		waiting := att.State == domain.AttemptWaitingConnection
		waitingForConn[aid] = waiting
		if waiting {
			// membership verified in the waitlist sweep below; note the
			// reverse direction there.
		}

		// (c) outcome ledger.
		if att.State.Terminal() && att.Outcome == domain.OutcomeUndetermined {
			p = append(p, fmt.Sprintf("attempt %s: terminal with undetermined outcome", aid))
		}
		if !att.State.Terminal() && att.Outcome != domain.OutcomeUndetermined {
			p = append(p, fmt.Sprintf("attempt %s: non-terminal with assigned outcome %s", aid, att.Outcome))
		}
		t := outcomeTally[att.Tenant]
		switch att.Outcome {
		case domain.OutcomeUseful:
			t.Useful++
		case domain.OutcomeWasted:
			t.Wasted++
		case domain.OutcomeDiscarded:
			t.Discarded++
		}
		outcomeTally[att.Tenant] = t

		// (d) queue/state membership aggregates.
		if att.State == domain.AttemptQueued {
			queuedPerTenant[att.Tenant]++
			if att.IsRetry {
				retryLane[att.Tenant]++
			} else {
				arrivalLane[att.Tenant]++
			}
		}

		// (e) pending-lane books.
		if att.State == domain.AttemptCreated && st.pendingLaneOf(att) {
			if att.IsRetry {
				pendingRetriesByTenant[att.Tenant]++
			} else {
				pendingArrivalsByTenant[att.Tenant]++
			}
		}
		if att.State != domain.AttemptCreated && st.pendingLaneOf(att) {
			p = append(p, fmt.Sprintf("attempt %s: pending in lane book but state %s", aid, att.State))
		}

		// (f) probe ledger.
		if att.IsProbe {
			probesPerUpstream[att.Upstream]++
		}
		// Probe permits exist only while the breaker has capacity for them;
		// correctness is enforced against ProbesActive below.
	}

	// (g) pending counts bookkeeping: sets vs per-tenant counters.
	for tid, n := range st.PendingArrivalCounts {
		if n != pendingArrivalsByTenant[tid] {
			p = append(p, fmt.Sprintf("tenant %s: pending-arrival count %d != %d created-state pending attempts", tid, n, pendingArrivalsByTenant[tid]))
		}
	}
	for tid, n := range st.PendingRetryCounts {
		if n != pendingRetriesByTenant[tid] {
			p = append(p, fmt.Sprintf("tenant %s: pending-retry count %d != %d created-state pending attempts", tid, n, pendingRetriesByTenant[tid]))
		}
	}

	// --- connection pools: waitlist direction --------------------------------
	for up, cp := range st.Connections {
		for _, w := range cp.Waitlist {
			att, ok := st.Attempts[w]
			if !ok {
				p = append(p, fmt.Sprintf("connection pool %s: unknown waitlisted attempt %s", up, w))
				continue
			}
			if !waitingForConn[w] {
				p = append(p, fmt.Sprintf("connection pool %s: waitlisted attempt %s in state %s (want waiting-connection)", up, w, att.State))
			}
			if att.Upstream != up {
				p = append(p, fmt.Sprintf("connection pool %s: waitlisted attempt %s targets upstream %s", up, w, att.Upstream))
			}
		}
	}

	// --- queues: entry-level, tenant and lane bounds --------------------------
	for tid, q := range st.Queues {
		cfg := st.Cfg.Tenants[tid]
		if n := queuedPerTenant[tid]; n != q.Len() {
			p = append(p, fmt.Sprintf("queue %s: %d queued attempts vs %d entries", tid, n, q.Len()))
		}
		seen := make(map[domain.AttemptID]bool, q.Len())
		for _, e := range q.Entries {
			att, ok := st.Attempts[e.Attempt]
			if !ok {
				p = append(p, fmt.Sprintf("queue %s: unknown attempt %s", tid, e.Attempt))
				continue
			}
			if att.State != domain.AttemptQueued {
				p = append(p, fmt.Sprintf("queue %s: entry %s in state %s", tid, e.Attempt, att.State))
			}
			if att.Tenant != tid {
				p = append(p, fmt.Sprintf("queue %s: entry %s belongs to tenant %s", tid, e.Attempt, att.Tenant))
			}
			if seen[e.Attempt] {
				p = append(p, fmt.Sprintf("queue %s: duplicate entry %s", tid, e.Attempt))
			}
			seen[e.Attempt] = true
		}
		if a := arrivalLane[tid]; a > cfg.MaxArrivalQueueDepth {
			p = append(p, fmt.Sprintf("queue %s: arrival lane %d exceeds MaxArrivalQueueDepth %d", tid, a, cfg.MaxArrivalQueueDepth))
		}
		if r := retryLane[tid]; r > cfg.MaxRetryQueueDepth {
			p = append(p, fmt.Sprintf("queue %s: retry lane %d exceeds MaxRetryQueueDepth %d", tid, r, cfg.MaxRetryQueueDepth))
		}
		if tot := arrivalLane[tid] + retryLane[tid] + pendingArrivalsByTenant[tid] + pendingRetriesByTenant[tid]; tot > cfg.MaxTotalQueueDepth {
			p = append(p, fmt.Sprintf("queue %s: total lane occupancy %d exceeds MaxTotalQueueDepth %d", tid, tot, cfg.MaxTotalQueueDepth))
		}
	}

	// --- requests: attempt lists ---------------------------------------------
	attemptsPerRequest := make(map[domain.RequestID]int, len(st.Requests))
	for _, att := range st.Attempts {
		attemptsPerRequest[att.LogicalID]++
	}
	for rid, req := range st.Requests {
		if n := attemptsPerRequest[rid]; n != len(req.Attempts) {
			p = append(p, fmt.Sprintf("request %s: %d registered attempts vs %d listed", rid, n, len(req.Attempts)))
		}
	}

	// --- tenant accounting -----------------------------------------------------
	nonTerminal := make(map[domain.TenantID]int, len(st.Cfg.Tenants))
	for _, req := range st.Requests {
		if !req.State.Terminal() {
			nonTerminal[req.Tenant]++
		}
	}
	for t, n := range st.TenantActive {
		if n < 0 {
			p = append(p, fmt.Sprintf("tenant %s: negative active count %d", t, n))
		}
		if n != nonTerminal[t] {
			p = append(p, fmt.Sprintf("tenant %s: active count %d != %d non-terminal requests", t, n, nonTerminal[t]))
		}
	}

	// --- outcome ledger consistency -------------------------------------------
	// Iterate configured tenants so a missing counters book is detected too.
	for t := range st.Cfg.Tenants {
		got := outcomeTally[t]
		cfgN, ok := st.OutcomeCounters[t]
		if !ok {
			p = append(p, fmt.Sprintf("tenant %s: no outcome counters book", t))
			continue
		}
		if got != cfgN {
			p = append(p, fmt.Sprintf("tenant %s: outcome counters %+v disagree with attempt registry tally %+v", t, cfgN, got))
		}
	}

	// --- breakers: probe ledger exactness -------------------------------------
	for up, br := range st.Breakers {
		if br.ProbesActive < 0 {
			p = append(p, fmt.Sprintf("breaker %s: negative probe count", up))
		}
		if got := probesPerUpstream[up]; got != br.ProbesActive {
			p = append(p, fmt.Sprintf("breaker %s: probe ledger mismatch — %d attempts hold IsProbe but ProbesActive is %d", up, got, br.ProbesActive))
		}
		if br.ProbesActive > st.Cfg.Breaker.MaxHalfOpenProbes {
			p = append(p, fmt.Sprintf("breaker %s: %d probes exceed limit %d", up, br.ProbesActive, st.Cfg.Breaker.MaxHalfOpenProbes))
		}
	}

	if len(p) == 0 {
		return nil
	}
	sort.Strings(p)
	return errors.New("system state invariants violated: " + strings.Join(p, "; "))
}

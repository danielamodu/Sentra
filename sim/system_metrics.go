package sim

// system_metrics.go implements the M2 experiment metrics model (workstream 7).
//
// Every metric below has an exact definition in its doc comment; the
// overarching conventions are:
//
//   - Logical-request volume and physical-attempt volume are NEVER conflated.
//     A request is counted once per arrival; an attempt is counted once per
//     creation (a retry creates a new attempt).
//   - A successful attempt is NOT automatically a successful request: only a
//     request whose terminal state is RequestCompleted counts toward
//     goodput (the first success settles the request; sibling attempts are
//     governed by orphaning and land as Wasted outcomes).
//   - Raw executed-work throughput is never used as goodput: attempts include
//     retries, and completions after the logical deadline are reported
//     separately from deadline-respecting completions.
//   - Occupancy is piecewise-constant in simulated time: timelines are
//     appended at exact resource-transition instants (occupancy.go), so
//     integral aggregation over a window is EXACT, with no sampling clock.
//   - Every aggregate is a pure function of the final SystemState (plus, for
//     in-window budget consumption, the in-order record ledger — see
//     countBudgetDecisionsIn below): computing them never mutates state and
//     never resets queues, workers, budgets, breakers or request state
//     (workstream 8 requirement).
//
// Phase semantics: a phase is the half-open interval [From, Until) of
// simulated time. An event belongs to the phase containing its instant.
// "State at Until" measures (occupancy, active attempts) read the instant
// Until inclusively; event counts are exclusive of Until.

import (
	"math"
	"sort"

	"sentra/domain"
)

// ---------------------------------------------------------------------------
// latency aggregation
// ---------------------------------------------------------------------------

// LatencyAgg aggregates a duration distribution. The zero value is the defined
// empty aggregate — nothing is ever fabricated when no samples exist.
//
// Percentile convention (exact, discrete): samples sorted ascending, Pq is
// the sample at 1-based rank ceil(q*N), clamped into [1, N]. No interpolation.
type LatencyAgg struct {
	N    int                `json:"n"`
	Mean float64            `json:"mean_ns"`
	P50  domain.SimDuration `json:"p50_ns"`
	P90  domain.SimDuration `json:"p90_ns"`
	P99  domain.SimDuration `json:"p99_ns"`
	Max  domain.SimDuration `json:"max_ns"`
}

// summarizeLatencies computes the aggregate; it never modifies the input
// slice.
func summarizeLatencies(ds []domain.SimDuration) LatencyAgg {
	if len(ds) == 0 {
		return LatencyAgg{}
	}
	s := make([]domain.SimDuration, len(ds))
	copy(s, ds)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum domain.SimDuration
	for _, d := range s {
		sum += d
	}
	return LatencyAgg{
		N:    len(s),
		Mean: float64(sum) / float64(len(s)),
		P50:  percentileAt(s, 0.50),
		P90:  percentileAt(s, 0.90),
		P99:  percentileAt(s, 0.99),
		Max:  s[len(s)-1],
	}
}

func percentileAt(sorted []domain.SimDuration, q float64) domain.SimDuration {
	n := len(sorted)
	r := int(math.Ceil(q * float64(n)))
	if r < 1 {
		r = 1
	}
	if r > n {
		r = n
	}
	return sorted[r-1]
}

// ratio reports a/b as float64; a zero denominator yields NaN (never 0 or
// infinity), so callers must handle the undefined case explicitly.
func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// meanOf is sum/count guarded against the empty aggregate.
func meanOf(sum domain.SimDuration, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(sum) / float64(n)
}

// ---------------------------------------------------------------------------
// occupancy aggregation
// ---------------------------------------------------------------------------

// OccSummary aggregates the occupancy of one resource over a time window.
//
//   - Window: the window length.
//   - Avg = ∫ occupancy dt / Window — the exact time average.
//   - Peak = the maximum occupancy held at any instant inside the window
//     (the occupancy in force at `from`, carried from the last sample at or
//     before `from`, is eligible; before the first sample occupancy is 0).
//   - Util = Avg / capacity. It is NaN when the capacity is unknown or zero;
//     the sim never reports an undefined ratio as a number.
type OccSummary struct {
	Window domain.SimDuration `json:"window_ns"`
	Avg    float64            `json:"avg_occupancy"`
	Peak   int                `json:"peak_occupancy"`
	Util   float64            `json:"utilization"`
}

// occAtUntil reports the occupancy value in force at instant t from a
// timeline; before the first sample the occupancy is 0.
func occAtUntil(times []domain.SimTime, counts []int, t domain.SimTime) int {
	v := 0
	for i := range times {
		if times[i] > t {
			break
		}
		v = counts[i]
	}
	return v
}

// integrateOcc computes the OccSummary of a piecewise-constant timeline given
// in two parallel ascending arrays (sample instants, occupancy counts).
// Segments accumulate in raw nanoseconds, keeping the integral exact.
func integrateOcc(times []domain.SimTime, counts []int, from, until domain.SimTime) OccSummary {
	if until <= from {
		return OccSummary{Window: until.Sub(from)}
	}
	cur := 0 // occupancy in force at `cursor`
	var sumNS int64
	cursor := from
	for i := range times {
		t := times[i]
		if t <= from {
			cur = counts[i]
			continue
		}
		if t >= until {
			break
		}
		sumNS += int64(cur) * int64(t.Sub(cursor))
		cursor = t
		cur = counts[i]
	}
	sumNS += int64(cur) * int64(until.Sub(cursor))
	peak := cur
	for i := range times {
		t := times[i]
		if t < from {
			continue
		}
		if t >= until {
			break
		}
		if counts[i] > peak {
			peak = counts[i]
		}
	}
	w := until.Sub(from)
	return OccSummary{
		Window: w,
		Avg:    float64(sumNS) / float64(w),
		Peak:   peak,
		Util:   math.NaN(), // callers fill Util when the capacity is known
	}
}

// ---------------------------------------------------------------------------
// phase report (workstream 8's state-at-phase building block)
// ---------------------------------------------------------------------------

// PhaseReport is the aggregate system snapshot of one half-open phase
// [From, Until). Computed from the final SystemState; never mutates it.
//
// Definitions:
//
//   - Offered: logical arrivals whose arrival instant falls in the window.
//   - Admitted: arrivals in the window that produced at least one attempt.
//   - Completed: requests terminalized with state RequestCompleted inside
//     the window. DeadlineRespecting additionally requires
//     TerminalAt <= Deadline (implied by completion under the M1 deadline
//     semantics; stated independently so the definition never depends on
//     the transition graph).
//   - Failed / Expired / Cancelled / Shed: terminal states inside the window.
//   - AttemptsCreated: physical attempts created in the window (both lanes;
//     RetryCreated is the retry-lane slice). NO attempt is a request.
//   - AttemptsFinished (Useful/Wasted/Disc): attempts terminalized in the
//     window, broken out by outcome (usefulness per assignOutcome).
//   - AttemptsActiveAtUntil: attempts whose lifecycle spans instant Until
//     (created before Until and terminalized at-or-after Until, or still
//     open). This is the in-flight volume carried across the boundary.
//   - QueueDepthAtUntil: queued entries (both lanes) summed over tenants at
//     Until, read from the queue occupancy timeline.
//   - WorkerBusyAtUntil / ConnBusyAtUntil: held worker / per-upstream held
//     connection counts at Until, from the timelines.
//   - WorkerOcc / ConnOcc / QueueOcc: exact occupancy integrals over the
//     window (Util per capacity where known).
type PhaseReport struct {
	From  domain.SimTime `json:"from_ns"`
	Until domain.SimTime `json:"until_ns"`

	Offered            int `json:"offered"`
	Admitted           int `json:"admitted"`
	Completed          int `json:"completed"`
	DeadlineRespecting int `json:"deadline_respecting"`
	Failed             int `json:"failed"`
	Expired            int `json:"expired"`
	Cancelled          int `json:"cancelled"`
	Shed               int `json:"shed"`

	AttemptsCreated  int `json:"attempts_created"`
	RetryCreated     int `json:"retry_created"`
	AttemptsFinished int `json:"attempts_finished"`
	AttemptsUseful   int `json:"attempts_useful_finished"`
	AttemptsWasted   int `json:"attempts_wasted_finished"`
	AttemptsDisc     int `json:"attempts_discarded_finished"`

	AttemptsActiveAtUntil int                   `json:"attempts_active_at_until"`
	QueueDepthAtUntil     int                   `json:"queue_depth_at_until"`
	WorkerBusyAtUntil     int                   `json:"worker_busy_at_until"`
	ConnBusyAtUntil       map[string]int        `json:"conn_busy_at_until"`
	WorkerOcc             OccSummary            `json:"worker_occ"`
	ConnOcc               map[string]OccSummary `json:"conn_occ"`
	QueueOcc              map[string]OccSummary `json:"queue_occ"`
}

// SummarizePhase computes the PhaseReport of [from, until) from the FINAL
// state. It never mutates anything.
func (st *SystemState) SummarizePhase(from, until domain.SimTime) PhaseReport {
	rep := PhaseReport{
		From:            from,
		Until:           until,
		ConnBusyAtUntil: map[string]int{},
		ConnOcc:         map[string]OccSummary{},
		QueueOcc:        map[string]OccSummary{},
	}

	for _, req := range st.Requests {
		inWindow := req.ArrivedAt >= from && req.ArrivedAt < until
		if inWindow {
			rep.Offered++
			if len(req.Attempts) > 0 {
				rep.Admitted++
			}
		}
		if req.State.Terminal() && req.TerminalAt >= from && req.TerminalAt < until {
			switch req.State {
			case domain.RequestCompleted:
				rep.Completed++
				if req.TerminalAt <= req.Deadline {
					rep.DeadlineRespecting++
				}
			case domain.RequestFailed:
				rep.Failed++
			case domain.RequestExpired:
				rep.Expired++
			case domain.RequestCancelled:
				rep.Cancelled++
			case domain.RequestShed:
				rep.Shed++
			}
		}
	}
	for _, att := range st.Attempts {
		if att.CreatedAt >= from && att.CreatedAt < until {
			rep.AttemptsCreated++
			if att.IsRetry {
				rep.RetryCreated++
			}
		}
		if att.State.Terminal() && att.FinishedAt >= from && att.FinishedAt < until {
			rep.AttemptsFinished++
			switch att.Outcome {
			case domain.OutcomeUseful:
				rep.AttemptsUseful++
			case domain.OutcomeWasted:
				rep.AttemptsWasted++
			case domain.OutcomeDiscarded:
				rep.AttemptsDisc++
			}
		}
		if att.CreatedAt < until && (att.FinishedAt == 0 || att.FinishedAt >= until) {
			rep.AttemptsActiveAtUntil++
		}
	}

	// worker-pool timeline (single global series)
	wTimes := make([]domain.SimTime, len(st.WorkerOccupancy))
	wCounts := make([]int, len(st.WorkerOccupancy))
	for i, w := range st.WorkerOccupancy {
		wTimes[i], wCounts[i] = w.At, w.Busy
	}
	rep.WorkerOcc = integrateOcc(wTimes, wCounts, from, until)
	if pool := st.mainPool(); pool != nil && pool.Size > 0 {
		rep.WorkerOcc.Util = rep.WorkerOcc.Avg / float64(pool.Size)
	}
	rep.WorkerBusyAtUntil = occAtUntil(wTimes, wCounts, until)

	// connection pools: one series per upstream
	connTimes := make(map[domain.UpstreamID][]domain.SimTime)
	connCounts := make(map[domain.UpstreamID][]int)
	for _, c := range st.ConnOccupancy {
		connTimes[c.Up] = append(connTimes[c.Up], c.At)
		connCounts[c.Up] = append(connCounts[c.Up], c.Busy)
	}
	for up, cp := range st.Connections {
		occ := integrateOcc(connTimes[up], connCounts[up], from, until)
		if cp.Capacity > 0 {
			occ.Util = occ.Avg / float64(cp.Capacity)
		}
		rep.ConnOcc[string(up)] = occ
		rep.ConnBusyAtUntil[string(up)] = occAtUntil(connTimes[up], connCounts[up], until)
	}

	// queues: one series per tenant (total length)
	queueTimes := make(map[domain.TenantID][]domain.SimTime)
	queueCounts := make(map[domain.TenantID][]int)
	for _, q := range st.QueueOccupancy {
		queueTimes[q.Tenant] = append(queueTimes[q.Tenant], q.At)
		queueCounts[q.Tenant] = append(queueCounts[q.Tenant], q.Length)
	}
	for tid := range st.Queues {
		occ := integrateOcc(queueTimes[tid], queueCounts[tid], from, until)
		cfg := st.Cfg.Tenants[tid]
		if depth := cfg.MaxArrivalQueueDepth + cfg.MaxRetryQueueDepth; depth > 0 {
			occ.Util = occ.Avg / float64(depth)
		}
		rep.QueueOcc[string(tid)] = occ
		rep.QueueDepthAtUntil += occAtUntil(queueTimes[tid], queueCounts[tid], until)
	}
	return rep
}

// ---------------------------------------------------------------------------
// whole-experiment summary
// ---------------------------------------------------------------------------

// TenantSummary is one tenant's whole-run service profile (workstreams 3/6).
type TenantSummary struct {
	Offered         int `json:"offered"`
	Admitted        int `json:"admitted"`
	Completed       int `json:"completed"`
	DeadlineRespect int `json:"deadline_respecting"`
	Failed          int `json:"failed"`
	Expired         int `json:"expired"`
	Cancelled       int `json:"cancelled"`
	Shed            int `json:"shed"`

	AttemptsStarted int                `json:"attempts_started"`
	RetryAttempts   int                `json:"retry_attempts"`
	BlockedRetries  int                `json:"blocked_retries"`
	Dispatched      int                `json:"dispatched"`
	WorkerSeconds   domain.SimDuration `json:"worker_time_ns"`

	AttemptsUseful int `json:"attempts_useful"`
	AttemptsWasted int `json:"attempts_wasted"`
	AttemptsDisc   int `json:"attempts_discarded"`

	// Distribution summaries recorded via value aggregation with the exact
	// recording samples; percentiles for these delivered-wait measures are
	// covered by QueueWait in ExperimentSummary (histogram approximation).
	ConnWaitMean      float64            `json:"conn_wait_mean_ns"`
	ConnWaitMax       domain.SimDuration `json:"conn_wait_max_ns"`
	ConnWaitAbandoned int                `json:"conn_wait_abandoned"`
	QueueWaitMean     float64            `json:"queue_wait_mean_ns"`
	QueueWaitMax      domain.SimDuration `json:"queue_wait_max_ns"`
	QueueDwellMean    float64            `json:"queue_dwell_abandoned_mean_ns"`
	QueueDwellMax     domain.SimDuration `json:"queue_dwell_abandoned_max_ns"`

	ShedReasons map[string]int `json:"shed_reasons"`
}

// UpstreamSummary is one upstream's whole-run load profile (workstreams 2/5).
type UpstreamSummary struct {
	Offered         int        `json:"offered"`
	Admitted        int        `json:"admitted"`
	AttemptsCreated int        `json:"attempts_created"`
	AttemptsUseful  int        `json:"attempts_useful"`
	AttemptsWasted  int        `json:"attempts_wasted"`
	AttemptsDisc    int        `json:"attempts_discarded"`
	ConnOcc         OccSummary `json:"conn_occ"`
	ConnBusyAtUntil int        `json:"conn_busy_at_until"`
}

// ExperimentSummary is the whole-run metrics model, computed once at the end
// of an experiment from the final state (never during the run).
type ExperimentSummary struct {
	Horizon domain.SimTime `json:"horizon_ns"`

	Offered            int `json:"offered"`
	Admitted           int `json:"admitted"`
	Terminal           int `json:"terminal"`
	Open               int `json:"open"`
	Completed          int `json:"completed"`
	DeadlineRespecting int `json:"deadline_respecting"`
	Failed             int `json:"failed"`
	Expired            int `json:"expired"`
	Cancelled          int `json:"cancelled"`
	Shed               int `json:"shed"`

	// Physical attempt volume and retry metrics (workstream 3).
	PhysicalAttempts    int `json:"physical_attempts"`
	ArrivalAttempts     int `json:"arrival_attempts"`
	RetryPhysical       int `json:"retry_attempts"`
	RetryTokensConsumed int `json:"retry_tokens_consumed"`
	BlockedRetries      int `json:"blocked_retries"`

	// Amplification = PhysicalAttempts / Admitted (NaN when 0/0: undefined,
	// never faked). RetriesPerRequest = Σ RetriesUsed / Admitted.
	Amplification     float64 `json:"retry_amplification"`
	RetriesPerRequest float64 `json:"retries_per_request"`

	// Outcome split and wasted work. WastedShare = AttemptsWasted / attempts
	// terminalized with a determined outcome.
	AttemptsUseful int     `json:"attempts_useful"`
	AttemptsWasted int     `json:"attempts_wasted"`
	AttemptsDisc   int     `json:"attempts_discarded"`
	WastedShare    float64 `json:"wasted_share"`

	// Definition C3 (workstream 3): executed work delivered after its logical
	// request's expiry instant — terminal NON-retracted attempts whose
	// FinishedAt is at-or-after the terminal instant of their Expired logical
	// request. Completions and timeouts both count; retracted attempts are
	// excluded (they never produced a result).
	WorkAfterExpiry int `json:"work_after_expiry"`

	// Latency distributions. E2E*: TerminalAt - ArrivedAt over admitted and
	// terminal requests (Success slices Completed only) — exact per-request
	// arrays with exact discrete percentiles. QueueWait: per-dispatched-
	// attempt samples (exact N/Mean/Max from the tenant sums; P50/P90/P99
	// from the per-tenant log2 histograms, documented approximation: each
	// bucket's upper bound stands for its samples).
	E2EAll     LatencyAgg `json:"e2e_all"`
	E2ESuccess LatencyAgg `json:"e2e_success"`
	QueueWait  LatencyAgg `json:"queue_wait"`

	// Occupancy over the whole horizon.
	WorkerOcc OccSummary            `json:"worker_occ"`
	ConnOcc   map[string]OccSummary `json:"conn_occ"`
	QueueOcc  map[string]OccSummary `json:"queue_occ"`

	WorkerSecondsByTenant map[string]domain.SimDuration `json:"worker_seconds_by_tenant"`
	Tenant                map[string]TenantSummary      `json:"tenants"`
	Upstream              map[string]UpstreamSummary    `json:"upstreams"`
}

// Summarize computes the whole-horizon ExperimentSummary from the final
// state. Pure function; it never mutates anything and never resets a book.
func (st *SystemState) Summarize(horizon domain.SimTime) ExperimentSummary {
	s := ExperimentSummary{
		Horizon:               horizon,
		ConnOcc:               map[string]OccSummary{},
		QueueOcc:              map[string]OccSummary{},
		WorkerSecondsByTenant: map[string]domain.SimDuration{},
		Tenant:                map[string]TenantSummary{},
		Upstream:              map[string]UpstreamSummary{},
	}
	for tid := range st.Cfg.Tenants {
		s.Tenant[string(tid)] = TenantSummary{ShedReasons: map[string]int{}}
	}
	for up := range st.Upstreams {
		s.Upstream[string(up)] = UpstreamSummary{}
	}

	// --- request-side: single sweep fills system, tenant and upstream books --
	var e2eAll, e2eOK []domain.SimDuration
	var retriesUsed int
	for _, req := range st.Requests {
		s.Offered++
		ts := s.Tenant[string(req.Tenant)]
		ts.Offered++
		s.Tenant[string(req.Tenant)] = ts
		us := s.Upstream[string(req.Upstream)]
		us.Offered++
		s.Upstream[string(req.Upstream)] = us

		admitted := len(req.Attempts) > 0
		if admitted {
			s.Admitted++
			ts.Admitted++
			us.Admitted++
			retriesUsed += req.RetriesUsed
		}
		if req.State.Terminal() {
			s.Terminal++
		} else {
			s.Open++
		}
		switch req.State {
		case domain.RequestCompleted:
			s.Completed++
			ts.Completed++
			if req.TerminalAt <= req.Deadline {
				s.DeadlineRespecting++
				ts.DeadlineRespect++
			}
		case domain.RequestFailed:
			s.Failed++
			ts.Failed++
		case domain.RequestExpired:
			s.Expired++
			ts.Expired++
		case domain.RequestCancelled:
			s.Cancelled++
			ts.Cancelled++
		case domain.RequestShed:
			s.Shed++
			ts.Shed++
		}
		if admitted && req.TerminalAt > 0 {
			d := req.TerminalAt.Sub(req.ArrivedAt)
			e2eAll = append(e2eAll, d)
			if req.State == domain.RequestCompleted {
				e2eOK = append(e2eOK, d)
			}
		}
		s.Tenant[string(req.Tenant)] = ts
		s.Upstream[string(req.Upstream)] = us
	}
	s.E2EAll = summarizeLatencies(e2eAll)
	s.E2ESuccess = summarizeLatencies(e2eOK)
	s.RetriesPerRequest = ratio(retriesUsed, s.Admitted)

	// --- attempt-side -----------------------------------------------------------
	for _, att := range st.Attempts {
		s.PhysicalAttempts++
		if att.IsRetry {
			s.RetryPhysical++
		} else {
			s.ArrivalAttempts++
		}
		us := s.Upstream[string(att.Upstream)]
		us.AttemptsCreated++
		ts := s.Tenant[string(att.Tenant)]
		if att.State.Terminal() {
			switch att.Outcome {
			case domain.OutcomeUseful:
				s.AttemptsUseful++
				ts.AttemptsUseful++
				us.AttemptsUseful++
			case domain.OutcomeWasted:
				s.AttemptsWasted++
				ts.AttemptsWasted++
				us.AttemptsWasted++
			case domain.OutcomeDiscarded:
				s.AttemptsDisc++
				ts.AttemptsDisc++
				us.AttemptsDisc++
			}
		}
		if att.State.Terminal() && att.State != domain.AttemptRetracted {
			if req := st.Requests[att.LogicalID]; req != nil &&
				req.State == domain.RequestExpired && att.FinishedAt >= req.TerminalAt {
				s.WorkAfterExpiry++
			}
		}
		s.Tenant[string(att.Tenant)] = ts
		s.Upstream[string(att.Upstream)] = us
	}
	s.WastedShare = ratio(s.AttemptsWasted, s.AttemptsUseful+s.AttemptsWasted+s.AttemptsDisc)
	s.Amplification = ratio(s.PhysicalAttempts, s.Admitted)

	for _, n := range st.RetryTokensConsumed {
		s.RetryTokensConsumed += n
	}

	// --- per-tenant metric books ----------------------------------------------
	for tid, m := range st.Metrics {
		ts := s.Tenant[string(tid)]
		ts.AttemptsStarted = m.AttemptsStarted
		ts.RetryAttempts = m.RetryAttempts
		ts.BlockedRetries = m.BlockedRetries
		ts.Dispatched = m.Dispatched
		ts.WorkerSeconds = m.WorkerTime
		ts.ConnWaitMean = meanOf(m.ConnWaitSum, m.ConnWaitN)
		ts.ConnWaitMax = m.ConnWaitMax
		ts.ConnWaitAbandoned = m.ConnWaitAbandoned
		ts.QueueWaitMean = meanOf(m.QueueWaitSum, m.QueueWaitN)
		ts.QueueWaitMax = m.QueueWaitMax
		ts.QueueDwellMean = meanOf(m.QueueDwellSum, m.QueueDwellN)
		ts.ShedReasons = m.Sheds
		s.WorkerSecondsByTenant[string(tid)] = m.WorkerTime
		s.Tenant[string(tid)] = ts
	}
	s.BlockedRetries = 0
	for _, m := range st.Metrics {
		s.BlockedRetries += m.BlockedRetries
	}

	// --- occupancy timelines -----------------------------------------------------
	wTimes := make([]domain.SimTime, len(st.WorkerOccupancy))
	wCounts := make([]int, len(st.WorkerOccupancy))
	for i, w := range st.WorkerOccupancy {
		wTimes[i], wCounts[i] = w.At, w.Busy
	}
	s.WorkerOcc = integrateOcc(wTimes, wCounts, 0, horizon)
	if pool := st.mainPool(); pool != nil && pool.Size > 0 {
		s.WorkerOcc.Util = s.WorkerOcc.Avg / float64(pool.Size)
	}

	connTimes := make(map[domain.UpstreamID][]domain.SimTime)
	connCounts := make(map[domain.UpstreamID][]int)
	for _, c := range st.ConnOccupancy {
		connTimes[c.Up] = append(connTimes[c.Up], c.At)
		connCounts[c.Up] = append(connCounts[c.Up], c.Busy)
	}
	for up, cp := range st.Connections {
		occ := integrateOcc(connTimes[up], connCounts[up], 0, horizon)
		if cp.Capacity > 0 {
			occ.Util = occ.Avg / float64(cp.Capacity)
		}
		s.ConnOcc[string(up)] = occ
		us := s.Upstream[string(up)]
		us.ConnOcc = occ
		us.ConnBusyAtUntil = occAtUntil(connTimes[up], connCounts[up], horizon)
		s.Upstream[string(up)] = us
	}

	queueTimes := make(map[domain.TenantID][]domain.SimTime)
	queueCounts := make(map[domain.TenantID][]int)
	for _, q := range st.QueueOccupancy {
		queueTimes[q.Tenant] = append(queueTimes[q.Tenant], q.At)
		queueCounts[q.Tenant] = append(queueCounts[q.Tenant], q.Length)
	}
	for tid := range st.Queues {
		occ := integrateOcc(queueTimes[tid], queueCounts[tid], 0, horizon)
		cfg := st.Cfg.Tenants[tid]
		if depth := cfg.MaxArrivalQueueDepth + cfg.MaxRetryQueueDepth; depth > 0 {
			occ.Util = occ.Avg / float64(depth)
		}
		s.QueueOcc[string(tid)] = occ
	}

	s.QueueWait = combinedQueueWaitAgg(st)
	return s
}

// combinedQueueWaitAgg folds every tenant's queue-wait book into the
// system-level aggregate: exact N/Mean/Max from the direct samples, and
// histogram-approximate percentiles (bucket i of queueWaitBuckets covers
// [2^(i-1), 2^i) ns, i>0; bucket 0 covers waits of exactly 0; a bucket's
// upper bound stands for its samples — the documented approximation).
func combinedQueueWaitAgg(st *SystemState) LatencyAgg {
	var sum domain.SimDuration
	var n int
	hist := make([]int64, queueWaitBuckets)
	for _, m := range st.Metrics {
		sum += m.QueueWaitSum
		n += m.QueueWaitN
		mmax := m.QueueWaitMax
		_ = mmax
		for i := range m.QueueWaitHist {
			hist[i] += m.QueueWaitHist[i]
		}
	}
	if n == 0 {
		return LatencyAgg{}
	}
	out := LatencyAgg{N: n, Mean: float64(sum) / float64(n)}
	// per-tenant max is folded into Max directly
	for _, m := range st.Metrics {
		if m.QueueWaitMax > out.Max {
			out.Max = m.QueueWaitMax
		}
	}
	out.P50 = histPercentile(hist, n, 0.50)
	out.P90 = histPercentile(hist, n, 0.90)
	out.P99 = histPercentile(hist, n, 0.99)
	return out
}

// histPercentile resolves the discrete percentile of a log2 histogram: the
// bucket holding the 1-based rank ceil(q*n) stands for its upper bound.
func histPercentile(hist []int64, n int, q float64) domain.SimDuration {
	if n == 0 {
		return 0
	}
	r := int(math.Ceil(q * float64(n)))
	var cum int64
	for i := range hist {
		cum += hist[i]
		if int64(r) <= cum {
			if i == 0 {
				return 0
			}
			return domain.SimDuration(1) << uint(i)
		}
	}
	return domain.SimDuration(1) << uint(len(hist)-1) // clamped bucket upper bound
}

package domain

import "fmt"

// RequestState is the logical-request lifecycle. It advances when governance
// decisions about the request as a whole change, independent of how far any of
// its physical attempts have progressed.
type RequestState int

const (
	// RequestAdmitted: admission accepted the request; its first attempt
	// exists but is not yet queued.
	RequestAdmitted RequestState = iota
	// RequestQueued: at least one attempt is waiting in the tenant queue.
	RequestQueued
	// RequestDispatched: an attempt holds a worker (running or waiting for
	// a connection).
	RequestDispatched
	// RequestRetrying: a failure has been recorded and a retry decision is
	// pending (backoff elapsing).
	RequestRetrying
	// RequestCancelling: cancellation was accepted; queued work has been
	// retracted, in-flight attempts may still be executing.
	RequestCancelling

	// Terminal states.
	RequestCompleted // an attempt delivered a usable success
	RequestFailed    // no further retry is possible
	RequestExpired   // the logical deadline passed
	RequestCancelled // cancellation fully applied
	RequestShed      // rejected at admission (load shed)
)

var requestStateNames = map[RequestState]string{
	RequestAdmitted:   "admitted",
	RequestQueued:     "queued",
	RequestDispatched: "dispatched",
	RequestRetrying:   "retrying",
	RequestCancelling: "cancelling",
	RequestCompleted:  "completed",
	RequestFailed:     "failed",
	RequestExpired:    "expired",
	RequestCancelled:  "cancelled",
	RequestShed:       "shed",
}

func (s RequestState) String() string {
	if n, ok := requestStateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("RequestState(%d)", int(s))
}

// Terminal reports whether s is a terminal request state. Terminal states have
// no outgoing transitions.
func (s RequestState) Terminal() bool {
	switch s {
	case RequestCompleted, RequestFailed, RequestExpired, RequestCancelled, RequestShed:
		return true
	}
	return false
}

// requestTransitions is the explicit request state machine. Transitions not
// listed here are illegal and the reference system turns them into panics
// (programmer error, not runtime condition).
var requestTransitions = map[RequestState][]RequestState{
	// Admitted→Expired: a request whose deadline equals its arrival expires
	// immediately, without admission or attempts (docs/ASSUMPTIONS.md A14).
	RequestAdmitted: {RequestQueued, RequestShed, RequestCancelling, RequestExpired},
	RequestQueued:   {RequestDispatched, RequestCancelling, RequestExpired},
	// Dispatched→Expired covers logical expiry while an attempt executes.
	// Retrying→Failed: a buffered retry can be refused explicitly at its
	// execution instant (retry-blocked or deadline), failing the request.
	RequestDispatched: {RequestCompleted, RequestFailed, RequestRetrying, RequestCancelling, RequestExpired},
	RequestRetrying:   {RequestQueued, RequestCompleted, RequestFailed, RequestCancelling, RequestExpired},
	RequestCancelling: {RequestCancelled},
}

// CanTransitionRequest reports whether from→to is a defined request transition.
func CanTransitionRequest(from, to RequestState) bool {
	if from.Terminal() {
		return false
	}
	for _, t := range requestTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AttemptState is the physical-attempt execution lifecycle. It is deliberately
// separate from the logical request lifecycle and from AttemptOutcome: an
// attempt keeps executing (and holds resources) even after its logical request
// has expired, been cancelled or completed.
type AttemptState int

const (
	// AttemptCreated: the attempt exists but is not queued yet.
	AttemptCreated AttemptState = iota
	// AttemptQueued: the attempt is a queue entry in its tenant queue.
	AttemptQueued
	// AttemptDispatching: the attempt left the queue; a worker is being
	// claimed on the same simulated instant.
	AttemptDispatching
	// AttemptAssigned: the attempt holds a worker; it is about to start
	// executing (possibly waiting for a connection first).
	AttemptAssigned
	// AttemptRunning: the attempt holds a worker and a connection and is
	// executing against the upstream.
	AttemptRunning
	// AttemptWaitingConnection: the attempt holds its worker but could not
	// obtain a connection; it is pinned in the connection pool waitlist.
	AttemptWaitingConnection

	// Terminal execution states.
	AttemptCompleted // execution finished and produced a result
	AttemptFailed    // execution finished with a failure result
	AttemptTimedOut  // the attempt timeout elapsed while executing or waiting
	AttemptRetracted // removed before producing a result (cancelled / expired retract)
)

var attemptStateNames = map[AttemptState]string{
	AttemptCreated:           "created",
	AttemptQueued:            "queued",
	AttemptDispatching:       "dispatching",
	AttemptAssigned:          "assigned",
	AttemptRunning:           "running",
	AttemptWaitingConnection: "waiting-connection",
	AttemptCompleted:         "completed",
	AttemptFailed:            "failed",
	AttemptTimedOut:          "timed-out",
	AttemptRetracted:         "retracted",
}

func (s AttemptState) String() string {
	if n, ok := attemptStateNames[s]; ok {
		return n
	}
	return fmt.Sprintf("AttemptState(%d)", int(s))
}

// Terminal reports whether s is a terminal attempt execution state.
func (s AttemptState) Terminal() bool {
	switch s {
	case AttemptCompleted, AttemptFailed, AttemptTimedOut, AttemptRetracted:
		return true
	}
	return false
}

// HoldingWorker reports whether attempts in state s occupy a worker slot.
func (s AttemptState) HoldingWorker() bool {
	switch s {
	case AttemptDispatching, AttemptAssigned, AttemptRunning, AttemptWaitingConnection:
		return true
	}
	return false
}

var attemptTransitions = map[AttemptState][]AttemptState{
	AttemptCreated:           {AttemptQueued, AttemptRetracted},
	AttemptQueued:            {AttemptDispatching, AttemptRetracted},
	AttemptDispatching:       {AttemptAssigned, AttemptRetracted},
	AttemptAssigned:          {AttemptRunning, AttemptWaitingConnection},
	AttemptWaitingConnection: {AttemptRunning, AttemptTimedOut, AttemptRetracted},
	AttemptRunning:           {AttemptCompleted, AttemptFailed, AttemptTimedOut},
}

// CanTransitionAttempt reports whether from→to is a defined attempt transition.
func CanTransitionAttempt(from, to AttemptState) bool {
	if from.Terminal() {
		return false
	}
	for _, t := range attemptTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AttemptOutcome records whether an attempt's eventual result was usable by its
// logical request. It is intentionally separate from AttemptState:
//
//   - an attempt may finish executing with AttemptCompleted long after its
//     logical request expired; the execution state is "completed" but the
//     outcome is OutcomeWasted, because the result cannot be delivered;
//   - an attempt that never produced a result (retracted while queued or
//     waiting for a connection) is OutcomeDiscarded;
//   - an attempt whose failure or timeout drove an observable decision of a
//     live logical request (retry scheduling, request failure) is OutcomeUseful.
//
// OutcomeWasted attempts are the primary metastability signal: downstream work
// that consumes finite resources without any beneficiary.
type AttemptOutcome int

const (
	OutcomeUndetermined AttemptOutcome = iota
	OutcomeUseful
	OutcomeWasted
	OutcomeDiscarded
)

var attemptOutcomeNames = map[AttemptOutcome]string{
	OutcomeUndetermined: "undetermined",
	OutcomeUseful:       "useful",
	OutcomeWasted:       "wasted",
	OutcomeDiscarded:    "discarded",
}

func (o AttemptOutcome) String() string {
	if n, ok := attemptOutcomeNames[o]; ok {
		return n
	}
	return fmt.Sprintf("AttemptOutcome(%d)", int(o))
}

// LogicalRequest is the tenant-visible unit of work: one deadline, one retry
// policy at most this many retries, one terminal outcome, and a set of
// physical attempts that may execute concurrently (e.g. a retry scheduled
// while a timed-out attempt is still executing downstream).
type LogicalRequest struct {
	ID          RequestID
	Tenant      TenantID
	Upstream    UpstreamID
	ArrivedAt   SimTime
	Deadline    SimTime
	MaxRetries  int
	RetriesUsed int
	Attempts    []AttemptID
	State       RequestState
	// TerminalAt is set exactly once by the transition that moves the
	// request into a terminal state (M2: end-to-end latency and goodput
	// aggregation read it; zero means not terminal).
	TerminalAt SimTime
}

// Active reports whether the logical request has not reached a terminal state.
func (r *LogicalRequest) Active() bool { return !r.State.Terminal() }

// PhysicalAttempt is one concrete execution of (part of) a logical request's
// work: it occupies queue, worker and connection resources under its own
// lifecycle, regardless of the logical request's state.
type PhysicalAttempt struct {
	ID             AttemptID
	LogicalID      RequestID
	Tenant         TenantID
	Upstream       UpstreamID
	CreatedAt      SimTime
	State          AttemptState
	Outcome        AttemptOutcome
	Timeout        SimDuration
	ExecDuration   SimDuration // fixed at actual execution start
	BeganRunningAt SimTime     // instant the attempt entered AttemptRunning
	Worker         WorkerID
	HasWorker      bool
	ConnHeld       bool
	// M2 metric fields:
	//
	//   - FinishedAt: set exactly once when the attempt enters a terminal
	//     state (zero means not terminal).
	//   - WaitingSince/WaitingSet: pinned-waiting-for-a-connection interval
	//     bookkeeping (WaitingSet disambiguates a wait starting at time 0).
	//   - WorkerSince/WorkerSet: worker-occupancy interval bookkeeping.
	FinishedAt   SimTime
	WaitingSince SimTime
	WaitingSet   bool
	WorkerSince  SimTime
	WorkerSet    bool
	// Orphaned marks attempts still executing while their logical request
	// can no longer use their result (expired, cancelled or completed by
	// another attempt). Orphaned attempts run to their own completion and
	// their results are eventually accounted as OutcomeWasted.
	Orphaned     bool
	OrphanedAt   SimTime
	OrphanReason string
	// IsProbe marks an attempt that consumed a circuit-breaker probe permit
	// (admitted while the breaker was half-open).
	IsProbe bool
	// IsRetry marks an attempt created by a retry decision rather than by an
	// original arrival. It exists so retry lanes can be governed by the
	// separately configurable MaxRetryQueueDepth without inspecting anything
	// outside the attempt itself (docs/ENGINE_SEMANTICS.md, admission).
	IsRetry bool
}

// ExecutionActive reports whether the attempt still occupies queue, worker or
// waitlist resources.
func (a *PhysicalAttempt) ExecutionActive() bool {
	switch a.State {
	case AttemptQueued, AttemptDispatching, AttemptAssigned, AttemptRunning, AttemptWaitingConnection:
		return true
	}
	return false
}

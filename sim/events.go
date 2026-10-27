package sim

import (
	"fmt"

	"sentra/domain"
	"sentra/engine"
)

// Kind enumerates the M1 event taxonomy. Every kind is a distinct Go type
// below; the reference system's handler switches exhaustively over types, and
// unknown concrete types panic.
type Kind int

const (
	KindRequestArrival           Kind = iota // external: a logical request arrives
	KindAdmissionDecided                     // emitted by arrival admission policy
	KindQueueInsertion                       // attempt appended to its tenant queue
	KindQueueRemoval                         // attempt selected for dispatch
	KindWorkerAssignment                     // attempt claims a worker slot
	KindExecutionStart                       // attempt begins execution lifecycle
	KindExecutionCompletion                  // attempt execution finishes
	KindTimeout                              // attempt timeout elapses
	KindCancellationRequested                // external: cancel a logical request
	KindCancellationAcknowledged             // cancellation application acknowledged
	KindRetryScheduled                       // retry decision recorded
	KindRetryExecution                       // retry attempt creation
	KindDeadlineExpired                      // logical request deadline reached
	KindUpstreamDegraded                     // external: upstream enters degradation
	KindUpstreamRecovered                    // external: upstream leaves degradation
	KindBreakerTransitioned                  // circuit-breaker state transition
	KindLoadShed                             // admission rejected the request
	KindConnectionGranted                    // deferred connection acquisition (auxiliary)
	KindUpstreamLatencySet                   // M2 fault injection: latency override
	KindUpstreamFailureSet                   // M2 fault injection: failure-rate mode
)

var kindNames = map[Kind]string{
	KindRequestArrival:           "RequestArrival",
	KindAdmissionDecided:         "AdmissionDecided",
	KindQueueInsertion:           "QueueInsertion",
	KindQueueRemoval:             "QueueRemoval",
	KindWorkerAssignment:         "WorkerAssignment",
	KindExecutionStart:           "ExecutionStart",
	KindExecutionCompletion:      "ExecutionCompletion",
	KindTimeout:                  "Timeout",
	KindCancellationRequested:    "CancellationRequested",
	KindCancellationAcknowledged: "CancellationAcknowledged",
	KindRetryScheduled:           "RetryScheduled",
	KindRetryExecution:           "RetryExecution",
	KindDeadlineExpired:          "DeadlineExpired",
	KindUpstreamDegraded:         "UpstreamDegraded",
	KindUpstreamRecovered:        "UpstreamRecovered",
	KindBreakerTransitioned:      "BreakerTransitioned",
	KindLoadShed:                 "LoadShed",
	KindConnectionGranted:        "ConnectionGranted",
	KindUpstreamLatencySet:       "UpstreamLatencySet",
	KindUpstreamFailureSet:       "UpstreamFailureSet",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Priority classes. Events at the same simulated instant are ordered first by
// these classes (lower runs earlier), then by scheduling sequence number.
// See docs/ENGINE_SEMANTICS.md for the full precedence table and the races it
// resolves.
const (
	// PriorityModelControl: environment changes (upstream degradation and
	// recovery, breaker transitions). Applied first so that any work
	// progressing at the same instant observes the new environment.
	PriorityModelControl = 0
	// PriorityGovernance: deadline expiration, cancellation request and
	// acknowledgement. Applied before further work so that retraction and
	// termination decisions are made before more resources are consumed,
	// and before completions at the same instant can deliver results.
	PriorityGovernance = 10
	// PriorityTimeout: attempt timeouts. Resolved pessimistically before
	// execution completions at the same instant (a result that lands exactly
	// at the timeout boundary is not trusted).
	PriorityTimeout = 20
	// PriorityWork: everything else — queueing, dispatch, execution starts,
	// completions, admission, load shedding, retries, connection grants.
	PriorityWork = 30
)

// Event is the sim-level event interface: every concrete event implements it,
// and its objects are the only events a Sim accepts from an engine.
type Event interface {
	Kind() Kind
	At() domain.SimTime
	Priority() int
}

// baseEvent carries the simulated-time point shared by all events.
type baseEvent struct{ Time domain.SimTime }

func (b baseEvent) At() domain.SimTime { return b.Time }

// --- External / injected events -------------------------------------------

// EventRequestArrival is a logical request arriving at the gateway.
// When injected directly it represents external traffic; the admission policy
// runs inside it and emits AdmissionDecided plus QueueInsertion or LoadShed.
type EventRequestArrival struct {
	baseEvent
	Tenant     domain.TenantID
	Upstream   domain.UpstreamID
	Deadline   domain.SimTime
	MaxRetries int
}

func (e *EventRequestArrival) Kind() Kind    { return KindRequestArrival }
func (e *EventRequestArrival) Priority() int { return PriorityWork }

// EventCancellationRequested externally cancels one logical request.
type EventCancellationRequested struct {
	baseEvent
	Request domain.RequestID
	Reason  string
}

func (e *EventCancellationRequested) Kind() Kind    { return KindCancellationRequested }
func (e *EventCancellationRequested) Priority() int { return PriorityGovernance }

// EventUpstreamDegraded externally marks an upstream degraded.
type EventUpstreamDegraded struct {
	baseEvent
	Upstream domain.UpstreamID
}

func (e *EventUpstreamDegraded) Kind() Kind    { return KindUpstreamDegraded }
func (e *EventUpstreamDegraded) Priority() int { return PriorityModelControl }

// EventUpstreamRecovered externally clears an upstream's degradation.
type EventUpstreamRecovered struct {
	baseEvent
	Upstream domain.UpstreamID
}

func (e *EventUpstreamRecovered) Kind() Kind    { return KindUpstreamRecovered }
func (e *EventUpstreamRecovered) Priority() int { return PriorityModelControl }

// --- Internal events (scheduled by transition handlers) --------------------

// EventAdmissionDecided records the admission decision for a request. The
// decision itself is applied atomically by EventRequestArrival; this event
// makes the decision observable and ordered.
type EventAdmissionDecided struct {
	baseEvent
	Request  domain.RequestID
	Tenant   domain.TenantID
	Upstream domain.UpstreamID
	Admitted bool
	Reason   string
}

func (e *EventAdmissionDecided) Kind() Kind    { return KindAdmissionDecided }
func (e *EventAdmissionDecided) Priority() int { return PriorityWork }

// EventQueueInsertion appends an attempt to its tenant queue.
type EventQueueInsertion struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventQueueInsertion) Kind() Kind    { return KindQueueInsertion }
func (e *EventQueueInsertion) Priority() int { return PriorityWork }

// EventQueueRemoval selects one queued attempt for dispatch. The named attempt
// may have been retracted in the meantime; the handler guards against that.
type EventQueueRemoval struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventQueueRemoval) Kind() Kind    { return KindQueueRemoval }
func (e *EventQueueRemoval) Priority() int { return PriorityWork }

// EventWorkerAssignment claims a worker slot for an attempt. The slot is
// chosen deterministically at execution time (FIFO release order), not in the
// payload, so that the payload cannot go stale.
type EventWorkerAssignment struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventWorkerAssignment) Kind() Kind    { return KindWorkerAssignment }
func (e *EventWorkerAssignment) Priority() int { return PriorityWork }

// EventExecutionStart begins the attempt's execution lifecycle: a timeout is
// armed and a connection is acquired (or the attempt is pinned waiting).
type EventExecutionStart struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventExecutionStart) Kind() Kind    { return KindExecutionStart }
func (e *EventExecutionStart) Priority() int { return PriorityWork }

// EventExecutionCompletion finishes an executing attempt. The M1 result
// (success/failure) is derived at this instant from upstream state.
type EventExecutionCompletion struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventExecutionCompletion) Kind() Kind    { return KindExecutionCompletion }
func (e *EventExecutionCompletion) Priority() int { return PriorityWork }

// EventTimeout fires an attempt's timeout, armed at ExecutionStart. It covers
// both running and waiting-for-connection phases.
type EventTimeout struct {
	baseEvent
	Attempt domain.AttemptID
}

func (e *EventTimeout) Kind() Kind    { return KindTimeout }
func (e *EventTimeout) Priority() int { return PriorityTimeout }

// EventCancellationAcknowledged acknowledges that a cancellation request was
// applied: queued/waiting work was retracted, and Outstanding reports the
// number of attempts still executing and no longer cancellable.
type EventCancellationAcknowledged struct {
	baseEvent
	Request     domain.RequestID
	Outstanding int
}

func (e *EventCancellationAcknowledged) Kind() Kind    { return KindCancellationAcknowledged }
func (e *EventCancellationAcknowledged) Priority() int { return PriorityGovernance }

// EventRetryScheduled records a retry decision (budget consumed, limits
// checked) and schedules the delayed retry execution.
type EventRetryScheduled struct {
	baseEvent
	Request     domain.RequestID
	Predecessor domain.AttemptID
	Reason      string
}

func (e *EventRetryScheduled) Kind() Kind    { return KindRetryScheduled }
func (e *EventRetryScheduled) Priority() int { return PriorityWork }

// EventRetryExecution creates the retry attempt of a logical request. If the
// request is no longer retrying (cancelled, expired, completed meanwhile) the
// event is a observed no-op.
type EventRetryExecution struct {
	baseEvent
	Request domain.RequestID
}

func (e *EventRetryExecution) Kind() Kind    { return KindRetryExecution }
func (e *EventRetryExecution) Priority() int { return PriorityWork }

// EventDeadlineExpired terminates a logical request whose deadline passed and
// governs its attempts: queued/waiting attempts retracted, executing attempts
// orphaned (they continue and become wasted when they finish).
type EventDeadlineExpired struct {
	baseEvent
	Request domain.RequestID
}

func (e *EventDeadlineExpired) Kind() Kind    { return KindDeadlineExpired }
func (e *EventDeadlineExpired) Priority() int { return PriorityGovernance }

// EventBreakerTransitioned applies a circuit-breaker transition. A stale
// transition (source state does not match) is ignored, which keeps the state
// machine total and deterministic without cancelling scheduled events.
type EventBreakerTransitioned struct {
	baseEvent
	Upstream domain.UpstreamID
	From     domain.BreakerState
	To       domain.BreakerState
	Reason   string
}

func (e *EventBreakerTransitioned) Kind() Kind    { return KindBreakerTransitioned }
func (e *EventBreakerTransitioned) Priority() int { return PriorityModelControl }

// EventLoadShed records that admission rejected a request. The termination is
// applied atomically by EventRequestArrival; this event makes the shed
// observable and attributable.
type EventLoadShed struct {
	baseEvent
	Request domain.RequestID
	Reason  string
}

func (e *EventLoadShed) Kind() Kind    { return KindLoadShed }
func (e *EventLoadShed) Priority() int { return PriorityWork }

// EventConnectionGranted is a payload-free wake event: a connection became
// free on its upstream. There is deliberately NO attempt payload — the
// release instant cannot know which waiter is still eligible when the grant
// runs, and events never change position once scheduled (docs/ASSUMPTIONS.md).
// The handler selects the oldest *state-valid* waiter from the pool's
// waitlist at processing time; stale or ineligible waiters can never strand
// capacity. It is an auxiliary kind beyond the milestone's core taxonomy; it
// exists so connection handover is an explicit, ordered event rather than
// hidden inside completions.
type EventConnectionGranted struct {
	baseEvent
	Upstream domain.UpstreamID
}

func (e *EventConnectionGranted) Kind() Kind    { return KindConnectionGranted }
func (e *EventConnectionGranted) Priority() int { return PriorityWork }

// EventUpstreamLatencySet is an M2 fault-injection event: it applies (or
// clears) a latency override on an upstream without touching the M1
// completion-time failure rule. Latency != 0: every attempt STARTING while
// the override is active gets this execution duration regardless of health
// (duration is still fixed at execution start per A12). Latency == 0:
// restore normal/degraded selection. Attempts completions keep succeeding —
// this is the "increased latency" mode.
//
// A upstream currently DEGRADED (slow + failing) receiving a latency set
// continues to fail at completion; the latency selection itself is then
// overridden by the M2 effect, which composes: the override wins for the
// duration, the A12 failure rule stays.
type EventUpstreamLatencySet struct {
	baseEvent
	Upstream domain.UpstreamID
	// Latency: 0 = restore health-based selection; > 0 = override.
	Latency domain.SimDuration
}

func (e *EventUpstreamLatencySet) Kind() Kind    { return KindUpstreamLatencySet }
func (e *EventUpstreamLatencySet) Priority() int { return PriorityModelControl }

// EventUpstreamFailureSet is an M2 fault-injection event: it applies (or
// clears) a deterministic elevated failure rate on an upstream at normal
// latency. EveryN == 0: clear the mode (failures follow A12 again: only
// degraded completions fail). EveryN >= 1: every k-th completion of the
// upstream fails, where k is the upstream's completion counter (including
// earlier failures of any origin). EveryN == 1 means every completion fails
// while the mode is active (fast-failing hard fault, no degraded latency
// unless the upstream is also degraded).
//
// The failure is decided at the completion instant exactly as A12 decrees;
// this event only lists WHICH completions fail. Timeouts/retractions do not
// advance the counter (documented: the counter counts *decisions*, i.e.
// completions, which keeps the k-accounting replay-safe).
type EventUpstreamFailureSet struct {
	baseEvent
	Upstream domain.UpstreamID
	EveryN   int
}

func (e *EventUpstreamFailureSet) Kind() Kind    { return KindUpstreamFailureSet }
func (e *EventUpstreamFailureSet) Priority() int { return PriorityModelControl }

// --- Constructors for externally injectable events -------------------------

// NewArrivalEvent builds a request-arrival event for scenario/test injection.
func NewArrivalEvent(t domain.SimTime, tenant domain.TenantID, up domain.UpstreamID, deadline domain.SimTime, maxRetries int) engine.Event {
	return &EventRequestArrival{
		baseEvent:  baseEvent{Time: t},
		Tenant:     tenant,
		Upstream:   up,
		Deadline:   deadline,
		MaxRetries: maxRetries,
	}
}

// NewCancellationEvent builds a cancellation event for scenario injection.
func NewCancellationEvent(t domain.SimTime, id domain.RequestID, reason string) engine.Event {
	return &EventCancellationRequested{baseEvent: baseEvent{Time: t}, Request: id, Reason: reason}
}

// NewUpstreamDegradedEvent builds a degradation event for scenario injection.
func NewUpstreamDegradedEvent(t domain.SimTime, up domain.UpstreamID) engine.Event {
	return &EventUpstreamDegraded{baseEvent: baseEvent{Time: t}, Upstream: up}
}

// NewUpstreamRecoveredEvent builds a recovery event for scenario injection.
func NewUpstreamRecoveredEvent(t domain.SimTime, up domain.UpstreamID) engine.Event {
	return &EventUpstreamRecovered{baseEvent: baseEvent{Time: t}, Upstream: up}
}

// NewUpstreamLatencySetEvent builds an M2 latency-override event for
// scenario injection (workstream 2: "increased latency").
func NewUpstreamLatencySetEvent(t domain.SimTime, up domain.UpstreamID, latency domain.SimDuration) engine.Event {
	return &EventUpstreamLatencySet{baseEvent: baseEvent{Time: t}, Upstream: up, Latency: latency}
}

// NewUpstreamFailureSetEvent builds an M2 elevated-failure-rate event for
// scenario injection. EveryN == 0 restores the A12 default; 1 = always fail.
func NewUpstreamFailureSetEvent(t domain.SimTime, up domain.UpstreamID, everyN int) engine.Event {
	return &EventUpstreamFailureSet{baseEvent: baseEvent{Time: t}, Upstream: up, EveryN: everyN}
}

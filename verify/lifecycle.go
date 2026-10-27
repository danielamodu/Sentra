package verify

// The verifier's own lifecycle tables. They encode the contracts documented in
// docs/SYSTEM_MODEL.md and are deliberately re-declared here rather than read
// from the reference implementation: calling domain.CanTransitionRequest would
// make the verifier accept whatever the reference believes, including a future
// reference defect. The tables are contracts, not mechanisms.

// requestStates is the closed set of documented logical-request states.
var requestStates = map[string]bool{
	"admitted":   true,
	"queued":     true,
	"dispatched": true,
	"retrying":   true,
	"cancelling": true,
	"completed":  true,
	"failed":     true,
	"expired":    true,
	"cancelled":  true,
	"shed":       true,
}

// requestTransitions is the logical-request state machine.
var requestTransitions = map[string]map[string]bool{
	"admitted":   {"queued": true, "shed": true, "cancelling": true, "expired": true},
	"queued":     {"dispatched": true, "cancelling": true, "expired": true},
	"dispatched": {"completed": true, "failed": true, "retrying": true, "cancelling": true, "expired": true},
	"retrying":   {"queued": true, "completed": true, "failed": true, "cancelling": true, "expired": true},
	"cancelling": {"cancelled": true},
}

// requestTerminal is the set of terminal logical-request states.
var requestTerminal = map[string]bool{
	"completed": true,
	"failed":    true,
	"expired":   true,
	"cancelled": true,
	"shed":      true,
}

// attemptStates is the closed set of documented physical-attempt states.
var attemptStates = map[string]bool{
	"created":            true,
	"queued":             true,
	"dispatching":        true,
	"assigned":           true,
	"running":            true,
	"waiting-connection": true,
	"completed":          true,
	"failed":             true,
	"timed-out":          true,
	"retracted":          true,
}

// attemptTransitions is the physical-attempt state machine.
var attemptTransitions = map[string]map[string]bool{
	"created":            {"queued": true, "retracted": true},
	"queued":             {"dispatching": true, "retracted": true},
	"dispatching":        {"assigned": true, "retracted": true},
	"assigned":           {"running": true, "waiting-connection": true},
	"waiting-connection": {"running": true, "timed-out": true, "retracted": true},
	"running":            {"completed": true, "failed": true, "timed-out": true},
}

// attemptTerminal is the set of terminal physical-attempt states.
var attemptTerminal = map[string]bool{
	"completed": true,
	"failed":    true,
	"timed-out": true,
	"retracted": true,
}

var outcomes = map[string]bool{
	"undetermined": true,
	"useful":       true,
	"wasted":       true,
	"discarded":    true,
}

var breakerStateSet = map[string]bool{
	"closed":    true,
	"open":      true,
	"half-open": true,
}

var breakerTransitions = map[string]map[string]bool{
	"closed":    {"open": true},
	"open":      {"half-open": true},
	"half-open": {"closed": true, "open": true},
}

// attemptHoldsWorker reports whether attempts in that state occupy a worker.
func attemptHoldsWorker(state string) bool {
	switch state {
	case "dispatching", "assigned", "running", "waiting-connection":
		return true
	}
	return false
}

// attemptExecutionActive reports whether the attempt still occupies queue,
// worker or waitlist resources.
func attemptExecutionActive(state string) bool {
	switch state {
	case "queued", "dispatching", "assigned", "running", "waiting-connection":
		return true
	}
	return false
}

func validRequestTransition(from, to string) bool {
	return requestTransitions[from][to]
}

func validAttemptTransition(from, to string) bool {
	return attemptTransitions[from][to]
}

func validBreakerTransition(from, to string) bool {
	return breakerTransitions[from][to]
}

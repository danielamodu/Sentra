package verify

import (
	"fmt"
)

// The property registry is the single authoritative inventory of verifier
// properties. It is package-private, ordered, and never returned to callers:
// evaluation iterates this exact slice, so execution order and the reported
// property set cannot drift from the declared contracts.
//
// Every property declares what evidence supports its claim, which shortfalls
// cap or skip it, how large a counterexample window it needs, and which test
// cases exercise its behavior. The check function is the only executable part;
// the surrounding policy is enforced by evaluateProperty, not by convention.

// propertyCategory is the approved P3 taxonomy. Categories F and H are reserved
// for future families and are intentionally empty at this stage.
type propertyCategory string

const (
	categoryLifecycle   propertyCategory = "A" // L lifecycle and linkage
	categoryResources   propertyCategory = "B" // S resource and horizon contracts
	categoryQueueing    propertyCategory = "C" // S queue and lane contracts
	categoryRetryBudget propertyCategory = "D" // retry-budget contracts across paths
	categoryBreaker     propertyCategory = "E" // breaker and probe contracts
	categorySemantics   propertyCategory = "F" // reserved: semantic invariants
	categoryCrossPath   propertyCategory = "G" // X cross-path agreement
	categoryWorkload    propertyCategory = "H" // reserved: workload/fault reconstruction
)

// evidenceSource names a path a property reads. Config is always validated
// before evaluation; listing it documents that a claim is relative to declared
// bounds rather than to observed facts alone.
type evidenceSource string

const (
	evidenceLedger   evidenceSource = "ledger"
	evidenceSnapshot evidenceSource = "snapshot"
	evidenceInput    evidenceSource = "input"
)

// shortfallNeed names a precondition the evidence must satisfy for a property to
// make an unrestricted claim.
type shortfallNeed string

const (
	// needSnapshot is satisfied only when final-state books are present.
	needSnapshot shortfallNeed = "snapshot"
	// needHeadComplete is satisfied when the retained ledger begins at the
	// documented initial boundary. It says nothing about the tail.
	needHeadComplete shortfallNeed = "head-complete-ledger"
	// needTerminalComplete is satisfied only when the retained ledger is proven
	// to reach the run's terminal boundary. It says nothing about the head.
	needTerminalComplete shortfallNeed = "terminal-complete-ledger"
	// needFullHistory requires both: the initial boundary and the terminal
	// boundary. Contract v1 cannot establish the terminal boundary, so this
	// need cannot be satisfied in production; properties that require it are
	// INCONCLUSIVE rather than downgrading the claim to head completeness.
	needFullHistory shortfallNeed = "full-history"
)

// shortfallMode controls what a shortfall does to a property result.
type shortfallMode string

const (
	// shortfallSkip does not run the check: the evidence cannot support even a
	// partial claim, so the property is INCONCLUSIVE with the declared reason.
	shortfallSkip shortfallMode = "skip"
	// shortfallCap runs the check so established violations are still
	// reported. A PASS is capped to INCONCLUSIVE; a FAIL is preserved.
	shortfallCap shortfallMode = "cap"
)

type shortfallPolicy struct {
	Need   shortfallNeed
	Mode   shortfallMode
	Reason string
}

// counterexamplePolicy bounds the nearby-block context attached to each
// finding. Window is the number of blocks before and after the primary block;
// zero means the approved default of three. Causality is false unless a
// property actually establishes a causal relationship: without it, nearby
// blocks are labelled "context", never "cause" or "effect".
type counterexamplePolicy struct {
	Window    int
	Causality bool
}

func (c counterexamplePolicy) radius() int {
	if c.Window <= 0 {
		return 3
	}
	return c.Window
}

// testKind is the evidence situation a test case must exercise.
type testKind string

const (
	testValid     testKind = "valid"
	testViolation testKind = "violation"
	testMissing   testKind = "missing"
	testPartial   testKind = "partial"
	testTerminal  testKind = "terminal"
	testCorrupt   testKind = "corrupt"
)

// propertyTest declares one test case. Test names a Go test function and Case
// names the behavioral case inside it. The declaration is a claim about
// coverage, not proof: registry_behavior_test.go binds every declaration to an
// executed case and fails if the binding is missing.
type propertyTest struct {
	Kind testKind
	Test string
	Case string
}

// propertySpec is one registry entry.
type propertySpec struct {
	ID         string
	Name       string
	Category   propertyCategory
	Claim      string
	Sources    []evidenceSource
	Shortfalls []shortfallPolicy
	// Counterexample is the per-property window policy. The zero value selects
	// the default three-block radius.
	Counterexample counterexamplePolicy
	Tests          []propertyTest
	// ViolationNotApplicable documents a property that can only PASS or
	// INCONCLUSIVE (an evidence-completeness or attribution property). It is
	// never a reason to omit a test.
	ViolationNotApplicable bool
	Check                  func(*evalContext) PropertyResult
}

func (p propertySpec) hasSource(s evidenceSource) bool {
	for _, have := range p.Sources {
		if have == s {
			return true
		}
	}
	return false
}

func (p propertySpec) hasNeed(n shortfallNeed) bool {
	for _, sf := range p.Shortfalls {
		if sf.Need == n {
			return true
		}
	}
	return false
}

// behaviorTestFunction is the one Go test function that executes the behavioral
// table. Every declaration must name it: a declaration pointing at any other
// test cannot be bound to an executed case.
const behaviorTestFunction = "TestRegistryPropertyBehavior"

// behaviorCaseSource returns the behavioral case names the package's test suite
// executes. registry_behavior_test.go installs the real table; production
// builds leave it nil, so no declaration can be proven verified outside the
// test binary and coverage() reports declared-not-verified rather than assuming
// coverage.
var behaviorCaseSource func() map[string]bool

// propertyTests builds the declaration list for one property. Keeping the case
// name derived from the ID and kind makes the binding to the behavioral table
// unambiguous and resistant to copy-paste drift.
func propertyTests(id string, kinds ...testKind) []propertyTest {
	out := make([]propertyTest, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, propertyTest{
			Kind: k,
			Test: behaviorTestFunction,
			Case: id + "/" + string(k),
		})
	}
	return out
}

const (
	reasonNoSnapshotResource = "snapshot evidence is absent; resource and bookkeeping properties cannot be established from the ledger alone"
	reasonNoSnapshotCross    = "snapshot evidence is absent; this cross-path fact cannot be established from the ledger alone"
	reasonHeadTruncated      = "the ledger head is truncated, so history before the retained window is unavailable"
	reasonTerminalUnbound    = "the retained ledger is not proven to reach the run's terminal boundary, so a fact that depends on the absence of later records cannot be established"
	reasonFullHistory        = "the ledger is not proven complete from the initial boundary through the run's terminal boundary"
)

// propertyRegistry is the authoritative ordered inventory. Do not append to it
// at runtime and do not expose it outside this package.
var propertyRegistry = []propertySpec{
	// --- A: logical and physical lifecycle ---------------------------------
	{
		ID:       "L-request-lifecycle",
		Name:     "logical request transitions follow the documented lifecycle",
		Category: categoryLifecycle,
		Claim:    "every observed logical-request transition is documented and starts at the reconstructed request state",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("L-request-lifecycle", testValid, testViolation, testCorrupt, testPartial),
		Check:    checkRequestLifecycle,
	},
	{
		ID:       "L-attempt-lifecycle",
		Name:     "physical attempt transitions follow the documented lifecycle",
		Category: categoryLifecycle,
		Claim:    "every observed attempt transition is documented and starts at the reconstructed attempt state",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("L-attempt-lifecycle", testValid, testViolation, testCorrupt, testPartial),
		Check:    checkAttemptLifecycle,
	},
	{
		ID:                     "L-history-completeness",
		Name:                   "reconstruction starts at documented initial states",
		Category:               categoryLifecycle,
		Claim:                  "every reconstructed request and attempt starts at its documented initial state on evidence that proves no earlier transition was evicted",
		Sources:                []evidenceSource{evidenceLedger},
		Shortfalls:             []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: "the ledger head is truncated, so the beginning of every reconstructed history is unavailable"}},
		Tests:                  propertyTests("L-history-completeness", testValid, testPartial),
		ViolationNotApplicable: true,
		Check:                  checkHistoryCompleteness,
	},
	{
		ID:                     "L-attempt-linkage",
		Name:                   "attempts are attributable to a logical request",
		Category:               categoryLifecycle,
		Claim:                  "each attempt is attributable to exactly one logical request from typed identity, never from prose",
		Sources:                []evidenceSource{evidenceLedger},
		Tests:                  propertyTests("L-attempt-linkage", testValid, testMissing, testPartial),
		ViolationNotApplicable: true,
		Check:                  checkAttemptLinkage,
	},
	{
		ID:       "L-breaker-lifecycle",
		Name:     "breaker transitions are legal and contiguous",
		Category: categoryBreaker,
		Claim:    "every observed breaker transition is documented and starts at the reconstructed breaker state",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("L-breaker-lifecycle", testValid, testViolation, testPartial),
		Check:    checkBreakerLifecycle,
	},
	{
		ID:         "L-retry-budget-ledger",
		Name:       "each recorded retry decision consumed exactly one retry-budget token",
		Category:   categoryRetryBudget,
		Claim:      "on a head-complete ledger, the number of budget effects equals the number of scheduled retry decisions",
		Sources:    []evidenceSource{evidenceLedger},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallSkip, Reason: "the ledger is truncated, so budget effects and retry decisions may fall on opposite sides of the eviction boundary"}},
		Tests:      propertyTests("L-retry-budget-ledger", testValid, testViolation, testPartial),
		Check:      checkRetryBudgetLedger,
	},

	// --- B: resources and horizon ------------------------------------------
	{
		ID:         "S-worker-conservation",
		Name:       "worker pool size is conserved and partitions busy/idle",
		Category:   categoryResources,
		Claim:      "each worker pool's busy and idle entries partition its declared size exactly",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-worker-conservation", testValid, testViolation, testMissing),
		Check:      checkWorkerConservation,
	},
	{
		ID:         "S-worker-ownership",
		Name:       "worker holders and attempt worker fields agree",
		Category:   categoryResources,
		Claim:      "worker bindings and attempt worker fields agree in both directions, and only worker-holding states hold a worker",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-worker-ownership", testValid, testViolation, testMissing),
		Check:      checkWorkerOwnership,
	},
	{
		ID:         "S-connection-ownership",
		Name:       "connection holders and attempt connection fields agree",
		Category:   categoryResources,
		Claim:      "connection holders and conn_held agree, held never exceeds capacity, and holding implies a running attempt",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-connection-ownership", testValid, testViolation, testMissing),
		Check:      checkConnectionOwnership,
	},
	{
		ID:         "S-waitlist-membership",
		Name:       "connection waitlists contain exactly the waiting attempts",
		Category:   categoryResources,
		Claim:      "waitlists contain exactly the waiting-connection attempts, once each, on the right upstream, holding no connection",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-waitlist-membership", testValid, testViolation, testMissing),
		Check:      checkWaitlists,
	},
	{
		ID:         "S-connection-stranding",
		Name:       "a free connection never coexists with a waiting attempt",
		Category:   categoryResources,
		Claim:      "an upstream with a free connection has no waiting attempts",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-connection-stranding", testValid, testViolation, testMissing),
		Check:      checkConnectionStranding,
	},
	{
		ID:         "S-horizon-completeness",
		Name:       "snapshot facts lie inside the observed run and terminal stamps are consistent",
		Category:   categoryResources,
		Claim:      "request, attempt and queue timestamps lie inside the run and terminal stamps exist exactly for terminal states",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-horizon-completeness", testValid, testViolation, testMissing),
		Check:      checkSnapshotHorizon,
	},

	// --- C: queueing and admission -----------------------------------------
	{
		ID:         "S-queue-consistency",
		Name:       "queue entries reference the queued attempts, in sequence order",
		Category:   categoryQueueing,
		Claim:      "queues contain exactly the queued attempts of their tenant, no duplicates, in strictly increasing global sequence order",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-queue-consistency", testValid, testViolation, testMissing),
		Check:      checkQueues,
	},
	{
		ID:         "S-lane-bounds",
		Name:       "tenant lane and total queue bounds hold including pending admissions",
		Category:   categoryQueueing,
		Claim:      "arrival-lane, retry-lane and total queue usage, including pending admissions, stay within declared bounds",
		Sources:    []evidenceSource{evidenceSnapshot, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-lane-bounds", testValid, testViolation, testMissing),
		Check:      checkLaneBounds,
	},

	// --- D: retry budget ----------------------------------------------------
	{
		ID:         "S-budget-state",
		Name:       "retry budget books are within their declared bounds",
		Category:   categoryRetryBudget,
		Claim:      "token counts, refill instants and consumed-token rows are internally valid and name configured buckets",
		Sources:    []evidenceSource{evidenceSnapshot, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-budget-state", testValid, testViolation, testMissing),
		Check:      checkBudgets,
	},
	{
		ID:       "X-budget-consumption",
		Name:     "consumed retry budget agrees across paths",
		Category: categoryRetryBudget,
		Claim:    "budget effects across the whole run equal the snapshot's total consumed tokens",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			{Need: needTerminalComplete, Mode: shortfallSkip, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-budget-consumption", testValid, testViolation, testMissing, testPartial, testTerminal),
		Check: checkBudgetConsumption,
	},

	// --- E: breakers and probes --------------------------------------------
	{
		ID:         "S-breaker-state",
		Name:       "breaker counters are within their declared bounds",
		Category:   categoryBreaker,
		Claim:      "failure counters are non-negative, probe allowance is respected, and no probes are reserved outside half-open",
		Sources:    []evidenceSource{evidenceSnapshot, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-breaker-state", testValid, testViolation, testMissing),
		Check:      checkBreakers,
	},
	{
		ID:         "S-probe-ledger",
		Name:       "breaker probe counters agree with the attempts holding permits",
		Category:   categoryBreaker,
		Claim:      "per upstream, permit-holding attempts equal ProbesActive and no terminal attempt holds a permit",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-probe-ledger", testValid, testViolation, testMissing),
		Check:      checkProbeBooks,
	},
	{
		ID:       "X-breaker-final-state",
		Name:     "final breaker states agree across paths",
		Category: categoryBreaker,
		Claim:    "ledger reconstruction and the snapshot agree on each breaker's final state; a terminal-complete ledger with no transition pins closed",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			// Cap, not skip: a breaker transition observed inside the retained
			// window can still contradict the snapshot where the comparison is
			// independent of the missing tail. The check itself only compares
			// when finality is established (see checkBreakerFinalStates).
			{Need: needTerminalComplete, Mode: shortfallCap, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-breaker-final-state", testValid, testViolation, testMissing, testPartial, testTerminal, testCorrupt),
		Check: checkBreakerFinalStates,
	},
	{
		ID:       "X-probe-ledger",
		Name:     "probe permits reserved and released agree with the snapshot",
		Category: categoryBreaker,
		Claim:    "reserved minus released probe permits across the whole run equals the snapshot's active-probe total",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			{Need: needTerminalComplete, Mode: shortfallSkip, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-probe-ledger", testValid, testViolation, testMissing, testPartial, testTerminal),
		Check: checkProbeLedger,
	},

	// --- G: cross-path consistency -----------------------------------------
	{
		ID:       "X-entity-sets",
		Name:     "the ledger and the snapshot describe the same entity population",
		Category: categoryCrossPath,
		Claim:    "every ledger-observed entity appears in the snapshot, and on a full-history ledger every snapshot entity is established by the ledger",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			{Need: needHeadComplete, Mode: shortfallCap, Reason: "the ledger head is truncated, so snapshot-only entities may predate the retained window; population equality cannot be claimed"},
			{Need: needTerminalComplete, Mode: shortfallCap, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-entity-sets", testValid, testViolation, testMissing, testPartial, testTerminal),
		Check: checkEntitySets,
	},
	{
		ID:       "X-request-final-state",
		Name:     "final request states and terminal instants agree across paths",
		Category: categoryCrossPath,
		Claim:    "where the snapshot and a ledger that establishes finality agree on a request's final state and terminal instant, they agree",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			// Cap, not skip: a request the ledger reconstructs as terminal has
			// an absorbing final state, so a disagreement with the snapshot is a
			// positive contradiction even when the ledger tail is unproven.
			{Need: needTerminalComplete, Mode: shortfallCap, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-request-final-state", testValid, testViolation, testMissing, testPartial, testTerminal, testCorrupt),
		Check: checkRequestFinalStates,
	},
	{
		ID:       "X-attempt-final-state",
		Name:     "final attempt states and finish instants agree across paths",
		Category: categoryCrossPath,
		Claim:    "where the snapshot and a ledger that establishes finality agree on an attempt's final state and finish instant, they agree",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			// Cap, not skip: a terminal attempt has an absorbing final state, so
			// a disagreement is a positive contradiction without the tail.
			{Need: needTerminalComplete, Mode: shortfallCap, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("X-attempt-final-state", testValid, testViolation, testMissing, testPartial, testTerminal, testCorrupt),
		Check: checkAttemptFinalStates,
	},
	{
		ID:         "X-attempt-parentage",
		Name:       "attempt-to-request attribution agrees across paths",
		Category:   categoryCrossPath,
		Claim:      "where the ledger establishes an attempt's parent request, the snapshot names the same request",
		Sources:    []evidenceSource{evidenceLedger, evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross}},
		Tests:      propertyTests("X-attempt-parentage", testValid, testViolation, testMissing, testPartial, testCorrupt),
		Check:      checkAttemptParentage,
	},
	{
		ID:         "S-outcome-ledger",
		Name:       "attempt outcomes are determined exactly once and match tenant counters",
		Category:   categoryCrossPath,
		Claim:      "terminal attempts have determined outcomes, non-terminal attempts do not, and per-tenant counters equal the attempt books",
		Sources:    []evidenceSource{evidenceSnapshot},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-outcome-ledger", testValid, testViolation, testMissing),
		Check:      checkOutcomeLedger,
	},
	{
		ID:         "S-tenant-active",
		Name:       "tenant active-request counters match the request books",
		Category:   categoryCrossPath,
		Claim:      "each tenant's active counter equals its non-terminal request count in the snapshot books",
		Sources:    []evidenceSource{evidenceSnapshot, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotResource}},
		Tests:      propertyTests("S-tenant-active", testValid, testViolation, testMissing),
		Check:      checkTenantActive,
	},
	{
		ID:       "X-tenant-active",
		Name:     "active-request counts agree across paths",
		Category: categoryCrossPath,
		Claim:    "on a full-history ledger, non-terminal requests per tenant equal the snapshot's active counters",
		Sources:  []evidenceSource{evidenceLedger, evidenceSnapshot, evidenceInput},
		Shortfalls: []shortfallPolicy{
			{Need: needSnapshot, Mode: shortfallSkip, Reason: reasonNoSnapshotCross},
			{Need: needFullHistory, Mode: shortfallSkip, Reason: reasonFullHistory},
		},
		Tests: propertyTests("X-tenant-active", testValid, testViolation, testMissing, testPartial, testTerminal),
		Check: checkTenantActiveCross,
	},

	// --- F: event and effect semantics -------------------------------------
	{
		ID:       "F-effect-attribution",
		Name:     "effects are attributable to the event that caused them",
		Category: categorySemantics,
		Claim:    "effects written while handling an event belong to the entities that event acted on",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("F-effect-attribution", testValid, testViolation, testPartial),
		Check:    checkEffectAttribution,
	},
	{
		ID:       "F-attempt-state-effects",
		Name:     "attempt transitions match the state their triggering event may produce",
		Category: categorySemantics,
		Claim:    "every attempt transition applied inside a block is a state the triggering event may produce",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("F-attempt-state-effects", testValid, testViolation, testPartial),
		Check:    checkAttemptStateEffects,
	},
	{
		ID:       "F-creation-lane",
		Name:     "attempt creation effects carry the admission lane their event establishes",
		Category: categorySemantics,
		Claim:    "arrival-lane attempts are created by RequestArrival events and retry-lane attempts by RetryExecution events",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("F-creation-lane", testValid, testViolation, testPartial),
		Check:    checkCreationLane,
	},
	{
		ID:         "F-terminal-effect-closure",
		Name:       "a finalized attempt is never resurrected by a later effect",
		Category:   categorySemantics,
		Claim:      "after an attempt terminal transition, no later effect creates it or grants it runtime resources or probe permits",
		Sources:    []evidenceSource{evidenceLedger},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("F-terminal-effect-closure", testValid, testViolation, testPartial),
		Check:      checkTerminalEffectClosure,
	},
	{
		ID:       "F-retry-admission-context",
		Name:     "blocked retries are effects of the retry admission decision they report",
		Category: categorySemantics,
		Claim:    "blocked retries are emitted by RetryExecution, fail their request and carry documented reasons",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("F-retry-admission-context", testValid, testViolation, testPartial),
		Check:    checkRetryAdmissionContext,
	},

	// --- B: resource intervals (ledger-side) -------------------------------
	{
		ID:         "B-worker-interval-pairing",
		Name:       "worker acquisitions and releases are paired per worker",
		Category:   categoryResources,
		Claim:      "worker assignments and releases strictly alternate without double assignment",
		Sources:    []evidenceSource{evidenceLedger},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("B-worker-interval-pairing", testValid, testViolation, testPartial),
		Check:      checkWorkerIntervals,
	},
	{
		ID:         "B-worker-occupancy",
		Name:       "reported worker occupancy matches the declared pool size",
		Category:   categoryResources,
		Claim:      "worker effects report idle counts consistent with declared pool size and active holders",
		Sources:    []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("B-worker-occupancy", testValid, testViolation, testPartial),
		Check:      checkWorkerOccupancy,
	},
	{
		ID:         "B-connection-interval-pairing",
		Name:       "connection acquisitions and releases are paired per attempt and within capacity",
		Category:   categoryResources,
		Claim:      "connection allocations are paired per attempt and concurrent connections stay within capacity",
		Sources:    []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("B-connection-interval-pairing", testValid, testViolation, testPartial),
		Check:      checkConnectionIntervals,
	},

	// --- D: retry semantics (ledger-side) ----------------------------------
	{
		ID:       "D-retry-causality",
		Name:     "retry scheduling is causally linked to a finalized predecessor attempt",
		Category: categoryRetryBudget,
		Claim:    "every RetryScheduled event names a predecessor attempt of the same request in a terminal failure state",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("D-retry-causality", testValid, testViolation, testPartial),
		Check:    checkRetryCausality,
	},
	{
		ID:       "D-retry-backoff",
		Name:     "retry executions follow retry scheduling by exactly the declared backoff",
		Category: categoryRetryBudget,
		Claim:    "retry execution events are paired with preceding schedules after the configured backoff interval",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{
			{Need: needTerminalComplete, Mode: shortfallCap, Reason: reasonTerminalUnbound},
		},
		Tests: propertyTests("D-retry-backoff", testValid, testViolation, testPartial, testTerminal),
		Check: checkRetryBackoff,
	},
	{
		ID:       "D-retry-limit",
		Name:     "retry counts do not exceed declared request retry limits",
		Category: categoryRetryBudget,
		Claim:    "the number of retries scheduled for any request never exceeds its declared limit",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Tests:    propertyTests("D-retry-limit", testValid, testViolation, testPartial),
		Check:    checkRetryLimit,
	},
	{
		ID:       "D-retry-blocked",
		Name:     "blocked retries cleanly terminate with documented reasons",
		Category: categoryRetryBudget,
		Claim:    "blocked retry executions reject requests with documented reasons and fail them immediately",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Tests:    propertyTests("D-retry-blocked", testValid, testViolation, testPartial),
		Check:    checkRetryBlocked,
	},

	// --- C: queue and admission semantics (ledger-side) --------------------
	{
		ID:       "C-admission-outcome",
		Name:     "admission decisions agree with the request lifecycle they caused",
		Category: categoryQueueing,
		Claim:    "admitted requests create attempts and shed requests transition to shed with documented reasons",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("C-admission-outcome", testValid, testViolation, testPartial),
		Check:    checkAdmissionOutcome,
	},
	{
		ID:         "C-queue-interval",
		Name:       "queue residency is a single append/remove interval per attempt",
		Category:   categoryQueueing,
		Claim:      "an attempt is appended at most once and removed at most once with matching lane and sequence",
		Sources:    []evidenceSource{evidenceLedger},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("C-queue-interval", testValid, testViolation, testPartial),
		Check:      checkQueueIntervals,
	},
	{
		ID:       "C-fifo-dispatch",
		Name:     "dispatch removals follow the global enqueue order",
		Category: categoryQueueing,
		Claim:    "queue dispatches occur in non-decreasing global enqueue sequence order",
		Sources:  []evidenceSource{evidenceLedger},
		Tests:    propertyTests("C-fifo-dispatch", testValid, testViolation, testPartial),
		Check:    checkDispatchFIFO,
	},
	{
		ID:         "C-lane-bounds",
		Name:       "observed queue lanes stay within their declared bounds",
		Category:   categoryQueueing,
		Claim:      "replayed queue lane occupancies never exceed declared arrival, retry or total bounds",
		Sources:    []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("C-lane-bounds", testValid, testViolation, testPartial),
		Check:      checkObservedLaneBounds,
	},

	// --- E: breaker semantics (ledger-side) --------------------------------
	{
		ID:       "E-breaker-open-threshold",
		Name:     "circuit breaker opens when consecutive failures reach threshold",
		Category: categoryBreaker,
		Claim:    "a circuit breaker transitions to open when and only when consecutive failures hit the configured threshold",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Tests:    propertyTests("E-breaker-open-threshold", testValid, testViolation, testPartial),
		Check:    checkBreakerOpenThreshold,
	},
	{
		ID:       "E-breaker-cooldown",
		Name:     "circuit breaker cooldown elapses before half-open transition",
		Category: categoryBreaker,
		Claim:    "a circuit breaker transitions from open to half-open only after the declared cooldown interval",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Tests:    propertyTests("E-breaker-cooldown", testValid, testViolation, testPartial),
		Check:    checkBreakerCooldown,
	},
	{
		ID:         "E-probe-allowance",
		Name:       "half-open probe reservations stay within declared limits and lifecycle",
		Category:   categoryBreaker,
		Claim:      "half-open probe traffic is strictly bounded and probe permits are tracked and released",
		Sources:    []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("E-probe-allowance", testValid, testViolation, testPartial),
		Check:      checkProbeAllowance,
	},
	{
		ID:         "E-failure-observation",
		Name:       "consecutive upstream failures are counted accurately and reset on success",
		Category:   categoryBreaker,
		Claim:      "consecutive failure counts increment monotonically and reset to zero on successful completions",
		Sources:    []evidenceSource{evidenceLedger},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("E-failure-observation", testValid, testViolation, testPartial),
		Check:      checkFailureObservation,
	},

	// --- H: workload and fault reconstruction ------------------------------
	{
		ID:       "H-arrival-envelope",
		Name:     "observed request arrivals conform to declared workload envelopes",
		Category: categoryWorkload,
		Claim:    "every observed arrival falls within a declared component or burst time window, deadline and retry limit",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Tests:    propertyTests("H-arrival-envelope", testValid, testViolation, testPartial),
		Check:    checkArrivalEnvelope,
	},
	{
		ID:       "H-burst-materialization",
		Name:     "observed arrivals match declared workload volume",
		Category: categoryWorkload,
		Claim:    "on full history, total observed arrivals per tenant and upstream equal the declared workload volume",
		Sources:  []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{
			{Need: needFullHistory, Mode: shortfallSkip, Reason: reasonFullHistory},
		},
		Tests: propertyTests("H-burst-materialization", testValid, testViolation, testPartial, testTerminal),
		Check: checkBurstMaterialization,
	},
	{
		ID:         "H-fault-intervals",
		Name:       "fault activation and recovery follow declared fault schedules",
		Category:   categoryWorkload,
		Claim:      "observed fault injection and recovery events match the declared fault episode schedule",
		Sources:    []evidenceSource{evidenceLedger, evidenceInput},
		Shortfalls: []shortfallPolicy{{Need: needHeadComplete, Mode: shortfallCap, Reason: reasonHeadTruncated}},
		Tests:      propertyTests("H-fault-intervals", testValid, testViolation, testPartial),
		Check:      checkFaultIntervals,
	},
	{
		ID:                     "H-seeded-replay",
		Name:                   "arrival sequence matches seeded PRNG stream",
		Category:               categoryWorkload,
		Claim:                  "seeded PRNG arrival sequences are verified independently",
		Sources:                []evidenceSource{evidenceLedger, evidenceInput},
		Tests:                  propertyTests("H-seeded-replay", testValid, testPartial),
		ViolationNotApplicable: true,
		Check:                  checkSeededReplay,
	},
}

// evalContext carries exactly what property checks may read. It is built once
// per evaluation; checks never reach back into document or reference packages.
type evalContext struct {
	ledger   *Ledger
	world    *World
	snapshot *Snapshot
	input    *Input

	attemptByID map[string]*AttemptFact
	requestByID map[string]*RequestFact
	tenants     map[string]TenantSpec
}

func newEvalContext(led *Ledger, w *World, s *Snapshot, in *Input) *evalContext {
	if w == nil {
		w = &World{}
	}
	if led == nil {
		led = &Ledger{Partial: w.Partial, Coverage: w.Coverage, TerminalProven: w.TerminalProven, Records: w.Records}
	}
	if led.Coverage == "" {
		led.Coverage = classifyHistory(led.FirstEffectSeq)
	}
	ctx := &evalContext{ledger: led, world: w, snapshot: s, input: in}
	if s != nil {
		ctx.attemptByID = make(map[string]*AttemptFact, len(s.Attempts))
		for i := range s.Attempts {
			ctx.attemptByID[s.Attempts[i].ID] = &s.Attempts[i]
		}
		ctx.requestByID = make(map[string]*RequestFact, len(s.Requests))
		for i := range s.Requests {
			ctx.requestByID[s.Requests[i].ID] = &s.Requests[i]
		}
	}
	if in != nil {
		ctx.tenants = make(map[string]TenantSpec, len(in.Tenants))
		for _, t := range in.Tenants {
			ctx.tenants[t.ID] = t
		}
	}
	return ctx
}

func ledgerFromWorld(w *World) *Ledger {
	if w == nil {
		return &Ledger{Coverage: HistoryTerminalUnestablished}
	}
	cov := w.Coverage
	if cov == "" {
		if w.Partial {
			cov = HistoryHeadTruncated
		} else {
			cov = HistoryTerminalUnestablished
		}
	}
	return &Ledger{Partial: w.Partial, Coverage: cov, TerminalProven: w.TerminalProven, Records: w.Records}
}

// shortfallActive reports whether a declared shortfall applies and the concrete
// reason to report. The evidence contract, never contiguity alone, decides.
func (ctx *evalContext) shortfallActive(sf shortfallPolicy) (bool, string) {
	switch sf.Need {
	case needSnapshot:
		if ctx.snapshot == nil {
			return true, sf.Reason
		}
	case needHeadComplete:
		if !ctx.ledger.headComplete() {
			return true, sf.Reason
		}
	case needTerminalComplete:
		// Only proven terminal completeness satisfies this. "Head complete" and
		// "last observed record" are not evidence about the tail.
		if !ctx.ledger.terminalComplete() {
			return true, sf.Reason
		}
	case needFullHistory:
		// Full history needs both independent proofs. A proven tail does not
		// imply a complete head.
		if !ctx.ledger.headComplete() || !ctx.ledger.terminalComplete() {
			return true, sf.Reason
		}
	}
	return false, ""
}

type evaluation struct {
	Results []PropertyResult
	// Defects are registry contract violations, not evidence defects. They make
	// the verdict INVALID_INPUT rather than blaming a property on bad evidence.
	Defects []Finding
}

// evaluateAll runs every property in registry order. It never sorts, filters or
// deduplicates: the declared order is the reported order.
func evaluateAll(ctx *evalContext) evaluation {
	eval := evaluation{Results: make([]PropertyResult, 0, len(propertyRegistry))}
	for _, spec := range propertyRegistry {
		r, defect := evaluateProperty(spec, ctx)
		if defect != nil {
			eval.Defects = append(eval.Defects, *defect)
			continue
		}
		eval.Results = append(eval.Results, r)
	}
	if len(eval.Defects) == 0 {
		attachCounterexamples(ctx.ledger, propIndex(), eval.Results)
	}
	return eval
}

func propIndex() map[string]propertySpec {
	m := make(map[string]propertySpec, len(propertyRegistry))
	for _, p := range propertyRegistry {
		m[p.ID] = p
	}
	return m
}

// evaluateProperty enforces the declared evidence policy around one check.
func evaluateProperty(spec propertySpec, ctx *evalContext) (PropertyResult, *Finding) {
	if spec.ID == "" || spec.Name == "" || spec.Check == nil {
		return PropertyResult{}, contractDefect("registry.incomplete", "property %q is missing an id, name or check function", spec.ID)
	}

	var caps []string
	for _, sf := range spec.Shortfalls {
		active, reason := ctx.shortfallActive(sf)
		if !active {
			continue
		}
		switch sf.Mode {
		case shortfallSkip:
			r := newCheck(spec.ID, spec.Name)
			r.addGap(reason)
			return *r, nil
		case shortfallCap:
			caps = append(caps, reason)
		default:
			return PropertyResult{}, contractDefect("registry.shortfall_mode", "property %s declares unknown shortfall mode %q", spec.ID, sf.Mode)
		}
	}

	r := spec.Check(ctx)
	if r.ID != spec.ID {
		return PropertyResult{}, contractDefect("registry.property_id", "property %s returned result for %q", spec.ID, r.ID)
	}
	switch r.Status {
	case StatusPass, StatusFail, StatusInconclusive:
	default:
		return PropertyResult{}, contractDefect("registry.status", "property %s returned status %q", spec.ID, r.Status)
	}
	for i := range r.Gaps {
		if r.Gaps[i].Property == "" {
			r.Gaps[i].Property = spec.ID
		} else if r.Gaps[i].Property != spec.ID {
			return PropertyResult{}, contractDefect("registry.gap_property", "property %s returned a gap attributed to %q", spec.ID, r.Gaps[i].Property)
		}
	}

	// Shortfalls are re-checked after the check: a cap must preserve FAIL and
	// may only turn an otherwise-unestablished PASS/INCONCLUSIVE into an
	// explicit gap.
	for _, reason := range caps {
		if len(r.Findings) > 0 {
			break
		}
		r.addGap(reason)
	}
	return r, nil
}

func contractDefect(code, format string, args ...any) *Finding {
	return &Finding{
		Code:           code,
		Message:        fmt.Sprintf(format, args...),
		Classification: ClassVerifierDefectCandidate,
	}
}

// lPathChecks, snapshotChecks and crossPathChecks are focused projections of
// the registry for tests and internal diagnostics. They select entries by
// declared source, never by a second hand-maintained list.
func lPathChecks(w *World) []PropertyResult {
	ctx := newEvalContext(ledgerFromWorld(w), w, nil, nil)
	return evaluateMatching(ctx, func(p propertySpec) bool {
		return p.hasSource(evidenceLedger) && !p.hasSource(evidenceSnapshot)
	})
}

func lPathChecksWithInput(w *World, in *Input) []PropertyResult {
	ctx := newEvalContext(ledgerFromWorld(w), w, nil, in)
	return evaluateMatching(ctx, func(p propertySpec) bool {
		return p.hasSource(evidenceLedger) && !p.hasSource(evidenceSnapshot)
	})
}

func snapshotChecks(s *Snapshot, in *Input) []PropertyResult {
	ctx := newEvalContext(&Ledger{Coverage: HistoryTerminalUnestablished}, &World{Coverage: HistoryTerminalUnestablished}, s, in)
	return evaluateMatching(ctx, func(p propertySpec) bool {
		return p.hasSource(evidenceSnapshot) && !p.hasSource(evidenceLedger)
	})
}

func crossPathChecks(w *World, s *Snapshot, in *Input) []PropertyResult {
	ctx := newEvalContext(ledgerFromWorld(w), w, s, in)
	return evaluateMatching(ctx, func(p propertySpec) bool {
		return p.hasSource(evidenceLedger) && p.hasSource(evidenceSnapshot)
	})
}

func evaluateMatching(ctx *evalContext, match func(propertySpec) bool) []PropertyResult {
	out := make([]PropertyResult, 0, len(propertyRegistry))
	for _, spec := range propertyRegistry {
		if !match(spec) {
			continue
		}
		r, defect := evaluateProperty(spec, ctx)
		if defect != nil {
			// A registry defect is a programming error. The Verify path reports
			// it as INVALID_INPUT; focused helpers fail loudly instead of
			// fabricating a property status.
			panic(fmt.Sprintf("verify: registry contract defect: %s", defect.Message))
		}
		out = append(out, r)
	}
	return out
}

// --- counterexamples --------------------------------------------------------

// CounterexampleBlock is one evidence block attached to a finding.
type CounterexampleBlock struct {
	EffectSeq uint64 `json:"effect_seq"`
	EngineSeq uint64 `json:"engine_seq"`
	Role      string `json:"role"`
}

// Counterexample is the bounded context attached to a finding. The primary
// block is always present; Truncated reports that the requested window was
// clipped by the retained evidence boundary.
type Counterexample struct {
	Blocks    []CounterexampleBlock `json:"blocks"`
	Truncated bool                  `json:"truncated,omitempty"`
}

const (
	counterexamplePrimary = "primary"
	counterexampleContext = "context"
	counterexampleCause   = "cause"
	counterexampleEffect  = "effect"
)

// citesLedgerBlock reports whether a finding references a ledger block. Both
// sequence fields are meaningful at zero: `effect_seq` 0 is the first record of
// an untruncated ledger and `engine_seq` 0 is the first engine step, so treating
// a zero pair as "no reference" would silently drop the counterexample for a
// violation established by the run's first block. A finding that names only a
// snapshot object is snapshot-only; a zero pair without a snapshot reference
// cites block engine 0 only when the retained ledger still contains it.
func citesLedgerBlock(f *Finding, index map[uint64]int) bool {
	if f.EngineSeq != 0 || f.EffectSeq != 0 {
		return true
	}
	if f.SnapshotRef != "" {
		return false
	}
	_, retained := index[0]
	return retained
}

// attachCounterexamples adds a bounded window to every finding that cites a
// ledger block. Nearby blocks are context: no current property establishes that
// a neighbour caused or resulted from the violation, so labelling one a cause
// or effect would invent causality.
func attachCounterexamples(led *Ledger, specs map[string]propertySpec, results []PropertyResult) {
	if led == nil || len(led.Blocks) == 0 {
		return
	}
	index := make(map[uint64]int, len(led.Blocks))
	for i := range led.Blocks {
		index[led.Blocks[i].EngineSeq] = i
	}
	for ri := range results {
		spec, ok := specs[results[ri].ID]
		if !ok {
			continue
		}
		radius := spec.Counterexample.radius()
		for fi := range results[ri].Findings {
			f := &results[ri].Findings[fi]
			if !citesLedgerBlock(f, index) {
				continue
			}
			bi, ok := index[f.EngineSeq]
			if !ok {
				// The primary reference is still actionable even when its block
				// is no longer retained.
				f.Counterexample = &Counterexample{
					Blocks: []CounterexampleBlock{{
						EffectSeq: f.EffectSeq,
						EngineSeq: f.EngineSeq,
						Role:      counterexamplePrimary,
					}},
					Truncated: true,
				}
				continue
			}
			lo, hi := bi-radius, bi+radius
			truncated := false
			if lo < 0 {
				lo, truncated = 0, true
			}
			if hi >= len(led.Blocks) {
				hi, truncated = len(led.Blocks)-1, true
			}
			blocks := make([]CounterexampleBlock, 0, hi-lo+1)
			for j := lo; j <= hi; j++ {
				role := counterexampleContext
				effect := led.Blocks[j].First.EffectSeq
				if j == bi {
					role = counterexamplePrimary
					if f.EffectSeq != 0 {
						effect = f.EffectSeq
					}
				}
				blocks = append(blocks, CounterexampleBlock{
					EffectSeq: effect,
					EngineSeq: led.Blocks[j].EngineSeq,
					Role:      role,
				})
			}
			// The primary block is always included: lo <= bi <= hi holds by
			// construction even when both edges were clipped.
			f.Counterexample = &Counterexample{Blocks: blocks, Truncated: truncated}
		}
	}
}

// --- coverage ---------------------------------------------------------------

// coverageStatus classifies one property's implementation/test declaration.
// DeclaredNotVerified is assigned only while the package's test suite is not
// running (or when a declared case is not bound): runtime code cannot observe
// test execution, so an unbound declaration is reported honestly instead of
// being assumed covered.
type coverageStatus string

const (
	coverageNotImplemented      coverageStatus = "not-implemented"
	coverageImplementedUntested coverageStatus = "implemented-untested"
	coverageDeclaredNotVerified coverageStatus = "declared-not-verified"
	coverageImplementedTested   coverageStatus = "implemented-tested"
)

func (p propertySpec) requiredTestKinds() []testKind {
	kinds := []testKind{testValid}
	if !p.ViolationNotApplicable {
		kinds = append(kinds, testViolation)
	}
	if p.hasNeed(needSnapshot) {
		kinds = append(kinds, testMissing)
	}
	if p.hasSource(evidenceLedger) {
		kinds = append(kinds, testPartial)
	}
	if p.hasNeed(needTerminalComplete) || p.hasNeed(needFullHistory) {
		kinds = append(kinds, testTerminal)
	}
	return kinds
}

func (p propertySpec) declaredKinds() map[testKind]bool {
	m := make(map[testKind]bool, len(p.Tests))
	for _, tc := range p.Tests {
		m[tc.Kind] = true
	}
	return m
}

func (p propertySpec) hasRequiredTests() bool {
	declared := p.declaredKinds()
	for _, kind := range p.requiredTestKinds() {
		if !declared[kind] {
			return false
		}
	}
	return true
}

// declarationsBound reports whether every declared case names the behavioral
// test function and a case the executable table runs. A declaration is not
// proof of coverage: the binding is what turns it into one.
func (p propertySpec) declarationsBound(bound map[string]bool) bool {
	if len(p.Tests) == 0 {
		return false
	}
	for _, tc := range p.Tests {
		if tc.Test != behaviorTestFunction {
			return false
		}
		if tc.Case != p.ID+"/"+string(tc.Kind) {
			return false
		}
		if !bound[tc.Case] {
			return false
		}
	}
	return true
}

func (p propertySpec) coverage() coverageStatus {
	var bound map[string]bool
	if behaviorCaseSource != nil {
		bound = behaviorCaseSource()
	}
	return p.coverageWith(bound)
}

// coverageWith evaluates one property against an explicit binding set, so the
// test suite can pin each coverage state without mutating package state.
func (p propertySpec) coverageWith(bound map[string]bool) coverageStatus {
	if p.Check == nil {
		return coverageNotImplemented
	}
	if !p.hasRequiredTests() {
		return coverageImplementedUntested
	}
	if !p.declarationsBound(bound) {
		return coverageDeclaredNotVerified
	}
	return coverageImplementedTested
}

// requiredCategories is the agreed P3 scope. Completion is not a property
// count: every category must be represented by at least one implemented and
// tested property. Reserved categories are therefore part of the calculation,
// not silently excluded from it.
var requiredCategories = []propertyCategory{
	categoryLifecycle,
	categoryResources,
	categoryQueueing,
	categoryRetryBudget,
	categoryBreaker,
	categorySemantics,
	categoryCrossPath,
	categoryWorkload,
}

// implementedTestedCategoryCounts counts only properties that are implemented
// and whose declarations are bound to executed cases, per category.
func implementedTestedCategoryCounts(bound map[string]bool) map[propertyCategory]int {
	counts := make(map[propertyCategory]int, len(requiredCategories))
	for _, p := range propertyRegistry {
		if p.coverageWith(bound) == coverageImplementedTested {
			counts[p.Category]++
		}
	}
	return counts
}

// missingRequiredCategories lists required categories with no implemented and
// tested property, in the agreed category order.
func missingRequiredCategories(bound map[string]bool) []propertyCategory {
	return missingCategoriesFrom(implementedTestedCategoryCounts(bound))
}

// missingCategoriesFrom applies the category gate to an explicit count map, so
// the gate can be tested without mutating the registry.
func missingCategoriesFrom(counts map[propertyCategory]int) []propertyCategory {
	var missing []propertyCategory
	for _, c := range requiredCategories {
		if counts[c] == 0 {
			missing = append(missing, c)
		}
	}
	return missing
}

// propertiesComplete reports per-property completion only: every entry is
// implemented, declares its required test kinds, has unique identity and every
// declaration is bound to an executed case.
func propertiesComplete(bound map[string]bool) bool {
	if len(propertyRegistry) == 0 {
		return false
	}
	seen := make(map[string]bool, len(propertyRegistry))
	for _, p := range propertyRegistry {
		if p.coverageWith(bound) != coverageImplementedTested {
			return false
		}
		if seen[p.ID] {
			return false
		}
		seen[p.ID] = true
	}
	return true
}

// P3Complete reports whether every registered property is implemented and
// bound, and whether every required category is represented. The 27 migrated
// properties satisfy the per-property gate; F and H have no properties yet, so
// P3 is not complete. Outside the test binary nothing is bound, so this also
// reports false rather than assuming unverified coverage.
func P3Complete() bool {
	var bound map[string]bool
	if behaviorCaseSource != nil {
		bound = behaviorCaseSource()
	}
	return p3CompleteWith(bound)
}

// p3CompleteWith evaluates the completion gate against an explicit binding set.
func p3CompleteWith(bound map[string]bool) bool {
	if !propertiesComplete(bound) {
		return false
	}
	return len(missingRequiredCategories(bound)) == 0
}

// categoryCounts derives category coverage from the properties themselves; it
// is never maintained as an independent status.
func categoryCounts() map[propertyCategory]int {
	m := make(map[propertyCategory]int)
	for _, p := range propertyRegistry {
		m[p.Category]++
	}
	return m
}

// propertyIDs returns the registry IDs in execution order. Used by tests and
// reports; the slice is a copy and cannot mutate the registry.
func propertyIDs() []string {
	out := make([]string, len(propertyRegistry))
	for i := range propertyRegistry {
		out[i] = propertyRegistry[i].ID
	}
	return out
}

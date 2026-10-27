package verify

import (
	"fmt"
	"testing"
)

// This file binds every registry test declaration to an executed behavioral
// case. A declaration names a case (for example
// "L-request-lifecycle/violation"); TestRegistryPropertyBehavior runs exactly
// that case against a fixture built to exercise the described evidence
// situation, and asserts the property status plus the presence of the expected
// findings or gaps. TestRegistryDeclarationsAreVerified then binds the
// declaration set to this table in both directions, so a renamed, missing or
// invented case fails the suite.

// behaviorCase is one executable case named "<property>/<kind>".
type behaviorCase struct {
	name string
	run  func(*testing.T) PropertyResult
	want Status
}

// behaviorTable is the single source of truth for behavioral coverage. It is
// deliberately a slice, not a map: execution order is deterministic and the
// table can be walked by both the behavior test and the declaration binder.
func behaviorTable() []behaviorCase {
	var cases []behaviorCase

	// --- A: lifecycle and linkage ------------------------------------------
	cases = append(cases,
		ledgerPass("L-request-lifecycle", "valid", validLifecycleEvidence()),
		ledgerFail("L-request-lifecycle", "violation", illegalRequestTransitionEvidence()),
		ledgerFail("L-request-lifecycle", "corrupt", requestContinuityCorruptionEvidence()),
		ledgerResult("L-request-lifecycle", "partial", headTruncatedLifecycleEvidence(), StatusPass),

		ledgerPass("L-attempt-lifecycle", "valid", validLifecycleEvidence()),
		ledgerFail("L-attempt-lifecycle", "violation", illegalAttemptTransitionEvidence()),
		ledgerFail("L-attempt-lifecycle", "corrupt", attemptContinuityCorruptionEvidence()),
		ledgerResult("L-attempt-lifecycle", "partial", headTruncatedLifecycleEvidence(), StatusPass),

		ledgerPass("L-history-completeness", "valid", validLifecycleEvidence()),
		ledgerResult("L-history-completeness", "partial", headTruncatedLifecycleEvidence(), StatusInconclusive),

		ledgerPass("L-attempt-linkage", "valid", validLifecycleEvidence()),
		ledgerResult("L-attempt-linkage", "missing", unlinkedAttemptEvidence(), StatusInconclusive),
		ledgerResult("L-attempt-linkage", "partial", headTruncatedLifecycleEvidence(), StatusPass),

		ledgerPass("L-breaker-lifecycle", "valid", validLifecycleEvidence()),
		ledgerFail("L-breaker-lifecycle", "violation", illegalBreakerTransitionEvidence()),
		ledgerResult("L-breaker-lifecycle", "partial", truncatedBreakerEvidence(), StatusInconclusive),

		ledgerPass("L-retry-budget-ledger", "valid", balancedRetryEvidence()),
		ledgerFail("L-retry-budget-ledger", "violation", unbalancedRetryEvidence()),
		ledgerResult("L-retry-budget-ledger", "partial", truncatedRetryEvidence(), StatusInconclusive),
	)

	// --- B, C, D, E: snapshot books and cross-path books -------------------
	snapshotViolations := []struct {
		id   string
		edit func(*Snapshot)
	}{
		{"S-worker-conservation", func(s *Snapshot) {
			s.WorkerPools[0].Busy = []WorkerBusyFact{{Worker: "w1", Attempt: "att-1"}}
		}},
		{"S-worker-ownership", func(s *Snapshot) {
			s.WorkerPools[0].Busy = []WorkerBusyFact{{Worker: "w1", Attempt: "att-1"}}
			s.WorkerPools[0].Idle = []string{"w2"}
			s.Attempts[0].HasWorker = false
			s.Attempts[0].Worker = ""
		}},
		{"S-connection-ownership", func(s *Snapshot) {
			s.Attempts[0].ConnHeld = true
		}},
		{"S-waitlist-membership", func(s *Snapshot) {
			s.Connections[0].Waiting = []string{"att-1"}
		}},
		{"S-connection-stranding", func(s *Snapshot) {
			s.Connections[0].Waiting = []string{"att-1"}
		}},
		{"S-queue-consistency", func(s *Snapshot) {
			s.Attempts[0].State = "queued"
			s.Attempts[0].Outcome = "undetermined"
			s.Attempts[0].FinishedAtNS = 0
			s.Requests[0].State = "queued"
			s.Requests[0].TerminalAtNS = 0
			s.TenantActive[0].Active = 1
			s.Outcomes[0] = OutcomeFact{Tenant: "T1"}
		}},
		{"S-tenant-active", func(s *Snapshot) {
			s.TenantActive[0].Active = 1
		}},
		{"S-outcome-ledger", func(s *Snapshot) {
			s.Attempts[0].Outcome = "undetermined"
		}},
		{"S-budget-state", func(s *Snapshot) {
			s.Budgets = []BudgetFact{{Tenant: "T1", Upstream: "U1", Capacity: 3, Tokens: 4, RefillIntervalNS: 1_000_000_000}}
			s.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 4}}
		}},
		{"S-breaker-state", func(s *Snapshot) {
			s.Breakers[0].ProbesActive = 1
		}},
		{"S-probe-ledger", func(s *Snapshot) {
			s.Attempts[0].IsProbe = true
		}},
		{"S-horizon-completeness", func(s *Snapshot) {
			s.Requests[0].ArrivedAtNS = s.TakenAtNS + 1
		}},
	}
	for _, v := range snapshotViolations {
		id := v.id
		edit := v.edit
		cases = append(cases,
			fullPass(id, "valid"),
			fullFail(id, "violation", edit),
			missingInconclusive(id),
		)
	}

	// S-lane-bounds needs a queue that exceeds the declared lane bound rather
	// than an edit to the one-request reference fixture.
	cases = append(cases,
		fullPass("S-lane-bounds", "valid"),
		behaviorCase{name: "S-lane-bounds/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "S-lane-bounds", laneBoundsViolationContext(t))
		}},
		missingInconclusive("S-lane-bounds"),
	)

	// --- D and E: cross-path budget, breaker and probe books ---------------
	// These claims read totals or final states over the whole run. Contract v1
	// cannot prove the terminal boundary, so their normal evidence is
	// INCONCLUSIVE; the valid/violation cases run against a synthetic
	// terminal-complete ledger to exercise the check semantics themselves.
	cases = append(cases,
		completePass("X-budget-consumption", "valid"),
		completeFail("X-budget-consumption", "violation", func(s *Snapshot) {
			s.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 2}}
		}),
		missingInconclusive("X-budget-consumption"),
		partialResult("X-budget-consumption", StatusInconclusive),
		terminalResult("X-budget-consumption"),

		completePass("X-breaker-final-state", "valid"),
		completeFail("X-breaker-final-state", "violation", func(s *Snapshot) {
			s.Breakers[0].State = "open"
		}),
		missingInconclusive("X-breaker-final-state"),
		partialResult("X-breaker-final-state", StatusInconclusive),
		completeFail("X-breaker-final-state", "corrupt", func(s *Snapshot) {
			// A parseable but undocumented final state: the focused check can
			// still compare it, while stage 3b rejects it as a structural
			// evidence defect in the pipeline.
			s.Breakers[0].State = "warped"
		}),
		terminalResult("X-breaker-final-state"),

		completePass("X-probe-ledger", "valid"),
		completeFail("X-probe-ledger", "violation", func(s *Snapshot) {
			s.Breakers[0].State = "half-open"
			s.Breakers[0].ProbesActive = 1
		}),
		missingInconclusive("X-probe-ledger"),
		partialResult("X-probe-ledger", StatusInconclusive),
		terminalResult("X-probe-ledger"),
	)

	// --- G: cross-path population and final-state agreement ----------------
	cases = append(cases,
		completePass("X-entity-sets", "valid"),
		// The forward direction does not depend on the ledger tail: a snapshot
		// that is complete at the horizon must list every ledger entity. A cap
		// preserves that violation even though the reverse direction cannot be
		// claimed on terminal-unestablished evidence.
		fullFail("X-entity-sets", "violation", func(s *Snapshot) {
			s.Attempts = nil
		}),
		missingInconclusive("X-entity-sets"),
		partialResult("X-entity-sets", StatusInconclusive),
		terminalResult("X-entity-sets"),

		completePass("X-request-final-state", "valid"),
		completeFail("X-request-final-state", "violation", func(s *Snapshot) {
			s.Requests[0].TerminalAtNS = 600
		}),
		missingInconclusive("X-request-final-state"),
		partialResult("X-request-final-state", StatusInconclusive),
		completeFail("X-request-final-state", "corrupt", func(s *Snapshot) {
			// Terminal state without a terminal stamp: internally corrupt
			// bookkeeping that the comparison must still expose.
			s.Requests[0].TerminalAtNS = 0
		}),
		terminalResult("X-request-final-state"),

		completePass("X-attempt-final-state", "valid"),
		completeFail("X-attempt-final-state", "violation", func(s *Snapshot) {
			s.Attempts[0].State = "failed"
		}),
		missingInconclusive("X-attempt-final-state"),
		partialResult("X-attempt-final-state", StatusInconclusive),
		completeFail("X-attempt-final-state", "corrupt", func(s *Snapshot) {
			s.Attempts[0].FinishedAtNS = 0
		}),
		terminalResult("X-attempt-final-state"),

		fullPass("X-attempt-parentage", "valid"),
		fullFail("X-attempt-parentage", "violation", func(s *Snapshot) {
			s.Attempts[0].Request = "req-2"
		}),
		missingInconclusive("X-attempt-parentage"),
		partialResult("X-attempt-parentage", StatusPass),
		fullFail("X-attempt-parentage", "corrupt", func(s *Snapshot) {
			// A dangling parent reference: stage 3b treats it as an evidence
			// defect, while the focused comparison still sees a disagreement.
			s.Attempts[0].Request = "req-9"
		}),

		completePass("X-tenant-active", "valid"),
		completeFail("X-tenant-active", "violation", func(s *Snapshot) {
			s.TenantActive[0].Active = 1
		}),
		missingInconclusive("X-tenant-active"),
		partialResult("X-tenant-active", StatusInconclusive),
		terminalResult("X-tenant-active"),
	)

	// --- F: event semantics ------------------------------------------------
	cases = append(cases,
		fullPass("F-effect-attribution", "valid"),
		behaviorCase{name: "F-effect-attribution/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "F-effect-attribution", effectAttributionViolationContext(t))
		}},
		partialResult("F-effect-attribution", StatusPass),

		fullPass("F-attempt-state-effects", "valid"),
		behaviorCase{name: "F-attempt-state-effects/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "F-attempt-state-effects", attemptStateEffectsViolationContext(t))
		}},
		partialResult("F-attempt-state-effects", StatusPass),

		fullPass("F-creation-lane", "valid"),
		behaviorCase{name: "F-creation-lane/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "F-creation-lane", creationLaneViolationContext(t))
		}},
		partialResult("F-creation-lane", StatusPass),

		fullPass("F-terminal-effect-closure", "valid"),
		behaviorCase{name: "F-terminal-effect-closure/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "F-terminal-effect-closure", terminalClosureViolationContext(t))
		}},
		partialResult("F-terminal-effect-closure", StatusInconclusive),

		fullPass("F-retry-admission-context", "valid"),
		behaviorCase{name: "F-retry-admission-context/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "F-retry-admission-context", retryAdmissionContextViolation(t))
		}},
		partialResult("F-retry-admission-context", StatusPass),
	)

	// --- B: resource intervals ---------------------------------------------
	cases = append(cases,
		fullPass("B-worker-interval-pairing", "valid"),
		behaviorCase{name: "B-worker-interval-pairing/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "B-worker-interval-pairing", workerPairingViolationContext(t))
		}},
		partialResult("B-worker-interval-pairing", StatusInconclusive),

		fullPass("B-worker-occupancy", "valid"),
		behaviorCase{name: "B-worker-occupancy/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "B-worker-occupancy", workerOccupancyViolationContext(t))
		}},
		partialResult("B-worker-occupancy", StatusInconclusive),

		fullPass("B-connection-interval-pairing", "valid"),
		behaviorCase{name: "B-connection-interval-pairing/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "B-connection-interval-pairing", connectionIntervalViolationContext(t))
		}},
		partialResult("B-connection-interval-pairing", StatusInconclusive),
	)

	// --- D: retry semantics ------------------------------------------------
	cases = append(cases,
		fullPass("D-retry-causality", "valid"),
		behaviorCase{name: "D-retry-causality/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "D-retry-causality", retryCausalityViolationContext(t))
		}},
		partialResult("D-retry-causality", StatusPass),

		completePass("D-retry-backoff", "valid"),
		behaviorCase{name: "D-retry-backoff/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "D-retry-backoff", retryBackoffViolationContext(t))
		}},
		partialResult("D-retry-backoff", StatusInconclusive),
		terminalResult("D-retry-backoff"),

		fullPass("D-retry-limit", "valid"),
		behaviorCase{name: "D-retry-limit/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "D-retry-limit", retryLimitViolationContext(t))
		}},
		partialResult("D-retry-limit", StatusPass),

		fullPass("D-retry-blocked", "valid"),
		behaviorCase{name: "D-retry-blocked/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "D-retry-blocked", retryBlockedViolationContext(t))
		}},
		partialResult("D-retry-blocked", StatusPass),
	)

	// --- C: queue and admission semantics ----------------------------------
	cases = append(cases,
		fullPass("C-admission-outcome", "valid"),
		behaviorCase{name: "C-admission-outcome/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "C-admission-outcome", admissionOutcomeViolationContext(t))
		}},
		partialResult("C-admission-outcome", StatusPass),

		fullPass("C-queue-interval", "valid"),
		behaviorCase{name: "C-queue-interval/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "C-queue-interval", queueIntervalViolationContext(t))
		}},
		partialResult("C-queue-interval", StatusInconclusive),

		fullPass("C-fifo-dispatch", "valid"),
		behaviorCase{name: "C-fifo-dispatch/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "C-fifo-dispatch", fifoDispatchViolationContext(t))
		}},
		partialResult("C-fifo-dispatch", StatusPass),

		fullPass("C-lane-bounds", "valid"),
		behaviorCase{name: "C-lane-bounds/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "C-lane-bounds", observedLaneBoundsViolationContext(t))
		}},
		partialResult("C-lane-bounds", StatusInconclusive),
	)

	// --- E: breaker semantics ----------------------------------------------
	cases = append(cases,
		fullPass("E-breaker-open-threshold", "valid"),
		behaviorCase{name: "E-breaker-open-threshold/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "E-breaker-open-threshold", breakerOpenThresholdViolationContext(t))
		}},
		partialResult("E-breaker-open-threshold", StatusPass),

		fullPass("E-breaker-cooldown", "valid"),
		behaviorCase{name: "E-breaker-cooldown/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "E-breaker-cooldown", breakerCooldownViolationContext(t))
		}},
		partialResult("E-breaker-cooldown", StatusPass),

		fullPass("E-probe-allowance", "valid"),
		behaviorCase{name: "E-probe-allowance/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "E-probe-allowance", probeAllowanceViolationContext(t))
		}},
		partialResult("E-probe-allowance", StatusInconclusive),

		fullPass("E-failure-observation", "valid"),
		behaviorCase{name: "E-failure-observation/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "E-failure-observation", failureObservationViolationContext(t))
		}},
		partialResult("E-failure-observation", StatusInconclusive),
	)

	// --- H: workload and fault reconstruction ------------------------------
	cases = append(cases,
		fullPass("H-arrival-envelope", "valid"),
		behaviorCase{name: "H-arrival-envelope/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "H-arrival-envelope", arrivalEnvelopeViolationContext(t))
		}},
		partialResult("H-arrival-envelope", StatusPass),

		completePass("H-burst-materialization", "valid"),
		behaviorCase{name: "H-burst-materialization/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "H-burst-materialization", burstMaterializationViolationContext(t))
		}},
		partialResult("H-burst-materialization", StatusInconclusive),
		terminalResult("H-burst-materialization"),

		fullPass("H-fault-intervals", "valid"),
		behaviorCase{name: "H-fault-intervals/violation", want: StatusFail, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "H-fault-intervals", faultIntervalsViolationContext(t))
		}},
		partialResult("H-fault-intervals", StatusInconclusive),

		behaviorCase{name: "H-seeded-replay/valid", want: StatusInconclusive, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "H-seeded-replay", fullContext(t))
		}},
		behaviorCase{name: "H-seeded-replay/partial", want: StatusInconclusive, run: func(t *testing.T) PropertyResult {
			return evalProperty(t, "H-seeded-replay", partialEvalContext(t))
		}},
	)

	return cases
}

// behaviorCaseNames is the case-name set the declaration binder checks against.
// It is derived from the same table that TestRegistryPropertyBehavior executes,
// so the binder cannot be satisfied by cases that do not run.
func behaviorCaseNames() map[string]bool {
	cases := behaviorTable()
	names := make(map[string]bool, len(cases))
	for _, c := range cases {
		names[c.name] = true
	}
	return names
}

// init installs the executed behavioral table as the registry's coverage
// binding. coverage() and P3Complete() therefore observe real declarations
// while the package's tests run; nothing here executes a case.
func init() {
	behaviorCaseSource = behaviorCaseNames
}

// TestRegistryPropertyBehavior executes every declared behavioral case.
func TestRegistryPropertyBehavior(t *testing.T) {
	cases := behaviorTable()
	if len(cases) == 0 {
		t.Fatal("behavioral table is empty")
	}
	seen := make(map[string]bool, len(cases))
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if seen[c.name] {
				t.Fatalf("behavioral case %q appears twice", c.name)
			}
			seen[c.name] = true
			r := c.run(t)
			id := propertyIDFromCase(c.name)
			if r.ID != id {
				t.Fatalf("case %q returned property %q", c.name, r.ID)
			}
			if r.Status != c.want {
				t.Fatalf("status = %s, want %s (findings %+v, gaps %+v)", r.Status, c.want, r.Findings, r.Gaps)
			}
			switch c.want {
			case StatusPass:
				if len(r.Findings) != 0 || len(r.Gaps) != 0 {
					t.Fatalf("PASS carries findings %+v or gaps %+v", r.Findings, r.Gaps)
				}
			case StatusFail:
				if len(r.Findings) == 0 {
					t.Fatal("FAIL case produced no finding")
				}
				for _, f := range r.Findings {
					if f.Code == "" {
						t.Fatalf("finding on %s has no code: %+v", id, f)
					}
					if !samePropertyFamily(id, f.Code) {
						t.Fatalf("finding code %q does not belong to %s", f.Code, id)
					}
				}
			case StatusInconclusive:
				if len(r.Gaps) == 0 {
					t.Fatal("INCONCLUSIVE case produced no evidence gap")
				}
				if len(r.Findings) != 0 {
					t.Fatalf("INCONCLUSIVE case also blamed evidence: %+v", r.Findings)
				}
			default:
				t.Fatalf("unexpected expected status %s", c.want)
			}
		})
	}
}

func propertyIDFromCase(name string) string {
	for i := 0; i < len(name); i++ {
		if name[i] == '/' {
			return name[:i]
		}
	}
	return name
}

// samePropertyFamily accepts the property id itself and its documented
// sub-codes (for example S-worker-duplicate under S-worker-conservation) while
// rejecting a finding that leaked from a different path family.
func samePropertyFamily(id, code string) bool {
	family := func(s string) string {
		for i := 0; i < len(s); i++ {
			if s[i] == '-' {
				return s[:i+1]
			}
		}
		return s
	}
	if family(id) != family(code) {
		return false
	}
	// Cross-path and lifecycle findings reuse their own property prefix; any
	// code in the same family is acceptable, but it must be a suffix code of
	// the same path (L/S/X).
	return true
}

// --- behavioral case constructors -----------------------------------------

func ledgerPass(id, kind string, ev Evidence) behaviorCase {
	return ledgerResult(id, kind, ev, StatusPass)
}

func ledgerFail(id, kind string, ev Evidence) behaviorCase {
	return ledgerResult(id, kind, ev, StatusFail)
}

func ledgerResult(id, kind string, ev Evidence, want Status) behaviorCase {
	return behaviorCase{
		name: id + "/" + kind,
		want: want,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, ledgerOnlyContext(t, ev))
		},
	}
}

func fullPass(id, kind string) behaviorCase {
	return behaviorCase{
		name: id + "/" + kind,
		want: StatusPass,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, fullContext(t))
		},
	}
}

func fullFail(id, kind string, edit func(*Snapshot)) behaviorCase {
	return behaviorCase{
		name: id + "/" + kind,
		want: StatusFail,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, snapshotContext(t, edit))
		},
	}
}

func missingInconclusive(id string) behaviorCase {
	return behaviorCase{
		name: id + "/missing",
		want: StatusInconclusive,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, noSnapshotContext(t))
		},
	}
}

func partialResult(id string, want Status) behaviorCase {
	return behaviorCase{
		name: id + "/partial",
		want: want,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, partialEvalContext(t))
		},
	}
}

// completePass and completeFail exercise a check whose declared evidence
// requirement is satisfied. Contract v1 cannot produce terminal-complete
// evidence, so the coverage field is set explicitly: the check itself reads the
// data, not the coverage classification, and the fixture is marked synthetic.
func completePass(id, kind string) behaviorCase {
	return behaviorCase{
		name: id + "/" + kind,
		want: StatusPass,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, terminalCompleteContext(t, nil))
		},
	}
}

func completeFail(id, kind string, edit func(*Snapshot)) behaviorCase {
	return behaviorCase{
		name: id + "/" + kind,
		want: StatusFail,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, terminalCompleteContext(t, edit))
		},
	}
}

// terminalResult is the adversarial case the contract currently mandates: the
// head is complete, the snapshot is present, and the ledger is not proven to
// reach the run's terminal boundary. A claim that depends on the absence of a
// later record must be INCONCLUSIVE, never PASS.
func terminalResult(id string) behaviorCase {
	return behaviorCase{
		name: id + "/terminal",
		want: StatusInconclusive,
		run: func(t *testing.T) PropertyResult {
			return evalProperty(t, id, fullContext(t))
		},
	}
}

// --- contexts --------------------------------------------------------------

func evidenceContext(t *testing.T, ev Evidence, s *Snapshot, in *Input) *evalContext {
	t.Helper()
	led, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("fixture evidence did not validate: %+v", fs)
	}
	w := reconstruct(&led)
	if len(w.Fatal) != 0 {
		t.Fatalf("fixture reconstruction produced fatal findings: %+v", w.Fatal)
	}
	return newEvalContext(&led, w, s, in)
}

// validEvalContext is the full healthy corpus: one request and one attempt
// followed end to end, agreeing final books, and the declared input contract.
func validEvalContext(t *testing.T) *evalContext {
	t.Helper()
	return evidenceContext(t, validLifecycleEvidence(), validSnapshot(), validInput())
}

// partialEvalContext is a legal but head-truncated ledger: the first four
// records (arrival, admission and typed creation) were evicted. The snapshot
// and input still describe the complete run.
func partialEvalContext(t *testing.T) *evalContext {
	t.Helper()
	return evidenceContext(t, headTruncatedLifecycleEvidence(), validSnapshot(), validInput())
}

func fullContext(t *testing.T) *evalContext {
	t.Helper()
	return evidenceContext(t, validLifecycleEvidence(), validSnapshot(), validInput())
}

func noSnapshotContext(t *testing.T) *evalContext {
	t.Helper()
	return evidenceContext(t, validLifecycleEvidence(), nil, validInput())
}

func snapshotContext(t *testing.T, edit func(*Snapshot)) *evalContext {
	t.Helper()
	s := validSnapshot()
	if edit != nil {
		edit(s)
	}
	return evidenceContext(t, validLifecycleEvidence(), s, validInput())
}

// terminalCompleteContext marks the healthy fixture as terminal-complete. It is
// synthetic by necessity: evidence contract v1 has no terminal-boundary marker,
// so only a test can set the classification that a future schema will prove.
func terminalCompleteContext(t *testing.T, edit func(*Snapshot)) *evalContext {
	t.Helper()
	ctx := snapshotContext(t, edit)
	ctx.ledger.Coverage = HistoryComplete
	ctx.world.Coverage = HistoryComplete
	return ctx
}

func ledgerOnlyContext(t *testing.T, ev Evidence) *evalContext {
	t.Helper()
	return evidenceContext(t, ev, nil, validInput())
}

// laneBoundsViolationContext holds five arrival-lane entries against a declared
// bound of four. It is built directly because the reference fixture has an
// empty queue and lane accounting is the whole property under test.
func laneBoundsViolationContext(t *testing.T) *evalContext {
	t.Helper()
	s := &Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		TakenAtNS:     60_000_000_000,
		Queues: []QueueFact{{
			Tenant: "T1",
			Entries: []QueueEntryFact{
				{Attempt: "att-1", Seq: 1},
				{Attempt: "att-2", Seq: 2},
				{Attempt: "att-3", Seq: 3},
				{Attempt: "att-4", Seq: 4},
				{Attempt: "att-5", Seq: 5},
			},
		}},
	}
	return newEvalContext(nil, &World{}, s, validInput())
}

func evalProperty(t *testing.T, id string, ctx *evalContext) PropertyResult {
	t.Helper()
	r, defect := evaluateProperty(registrySpec(t, id), ctx)
	if defect != nil {
		t.Fatalf("registry contract defect for %s: %+v", id, defect)
	}
	return r
}

// --- ledger fixtures -------------------------------------------------------

// headTruncatedLifecycleEvidence drops the first four records of the healthy
// lifecycle (arrival, admission decision and typed creation effects) and
// declares the retained window to start at effect_seq 4.
func headTruncatedLifecycleEvidence() Evidence {
	full := validLifecycleEvidence()
	if len(full.Records) != 17 {
		panic("validLifecycleEvidence record count changed; update the truncation fixture")
	}
	return Evidence{
		SchemaVersion:  full.SchemaVersion,
		FirstEffectSeq: 4,
		Records:        append([]Record(nil), full.Records[4:]...),
	}
}

func illegalRequestTransitionEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "completed"})
	return b.done()
}

func requestContinuityCorruptionEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "queued", To: "dispatched"}).
		// Legal edge ("admitted" -> "shed") recorded from the wrong state.
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "shed"})
	return b.done()
}

func illegalAttemptTransitionEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(1, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "completed"})
	return b.done()
}

func attemptContinuityCorruptionEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(1, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "queued"}).
		note(1, 100, "attempt-transition", Detail{Attempt: "att-1", From: "queued", To: "dispatching"}).
		// Legal edge ("created" -> "queued") recorded from the wrong state.
		note(1, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "queued"})
	return b.done()
}

func unlinkedAttemptEvidence() Evidence {
	b := newEvidence()
	b.event(1, 1_000, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(1, 1_000, "attempt-transition", Detail{Attempt: "att-1", From: "assigned", To: "running"})
	return b.done()
}

func balancedRetryEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	b.event(2, 200, "RetryScheduled", Detail{Request: "req-1"}).
		note(2, 200, "budget", Detail{Description: "budget consumed"})
	return b.done()
}

func unbalancedRetryEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	// The retry decision has no matching budget effect.
	b.event(2, 200, "RetryScheduled", Detail{Request: "req-1"})
	return b.done()
}

func illegalBreakerTransitionEvidence() Evidence {
	b := newEvidence()
	b.event(1, 100, "BreakerTransitioned", Detail{Upstream: "U1", From: "closed", To: "half-open"})
	return b.done()
}

func truncatedBreakerEvidence() Evidence {
	b := newEvidenceAt(10)
	b.event(11, 100, "BreakerTransitioned", Detail{Upstream: "U1", From: "closed", To: "open"})
	return b.done()
}

func truncatedRetryEvidence() Evidence {
	b := newEvidenceAt(10)
	// If the truncation shortfall were ignored this would be a false FAIL:
	// the budget effect may simply have been evicted.
	b.event(11, 100, "RetryScheduled", Detail{Request: "req-1"})
	return b.done()
}

func retriesPtr(n int) *int {
	return &n
}

func effectAttributionViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(0)}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(2, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(2, 100, "request-created", Detail{Request: "req-2", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(0)}).
		note(2, 100, "attempt-created", Detail{Request: "req-2", Attempt: "att-2", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(3, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(3, 100, "attempt-transition", Detail{Attempt: "att-2", From: "created", To: "queued"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func attemptStateEffectsViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(0)}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(2, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(2, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "completed"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func creationLaneViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(0)}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "retry"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func terminalClosureViolationContext(t *testing.T) *evalContext {
	full := validLifecycleEvidence()
	b := newEvidence()
	for _, rec := range full.Records {
		b.append(rec.EngineSeq, rec.SimTimeNS, rec.Category, rec.Kind, rec.Detail)
	}
	b.event(8, 600, "WorkerAssignment", Detail{Attempt: "att-1"}).
		note(8, 600, "worker", Detail{Worker: "w1", Attempt: "att-1", Decision: "acquired"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func retryAdmissionContextViolation(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "retry-admission", Detail{Request: "req-1", Decision: "blocked", Reason: "retry-queue-full"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func workerPairingViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "WorkerAssignment", Detail{Attempt: "att-1"}).
		note(1, 100, "worker", Detail{Worker: "w1", Attempt: "att-1", Decision: "acquired"})
	b.event(2, 100, "WorkerAssignment", Detail{Attempt: "att-2"}).
		note(2, 100, "worker", Detail{Worker: "w1", Attempt: "att-2", Decision: "acquired"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func workerOccupancyViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "WorkerAssignment", Detail{Attempt: "att-1"}).
		note(1, 100, "worker", Detail{Worker: "w1", Attempt: "att-1", Decision: "acquired"})
	b.event(2, 100, "WorkerAssignment", Detail{Attempt: "att-2"}).
		note(2, 100, "worker", Detail{Worker: "w2", Attempt: "att-2", Decision: "acquired"})
	b.event(3, 100, "WorkerAssignment", Detail{Attempt: "att-3"}).
		note(3, 100, "worker", Detail{Worker: "w3", Attempt: "att-3", Decision: "acquired"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func connectionIntervalViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(1, 100, "connection", Detail{Attempt: "att-1", Upstream: "U1", Decision: "acquired"})
	b.event(2, 100, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(2, 100, "connection", Detail{Attempt: "att-1", Upstream: "U1", Decision: "acquired"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func retryCausalityViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(1)}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(2, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(2, 100, "request-created", Detail{Request: "req-2", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(1)}).
		note(2, 100, "attempt-created", Detail{Request: "req-2", Attempt: "att-2", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(3, 200, "ExecutionCompletion", Detail{Attempt: "att-2"}).
		note(3, 200, "attempt-transition", Detail{Attempt: "att-2", From: "running", To: "failed"})
	b.event(4, 200, "RetryScheduled", Detail{Request: "req-1", Predecessor: "att-2"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func retryBackoffViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RetryScheduled", Detail{Request: "req-1"})
	b.event(2, 150, "RetryScheduled", Detail{Request: "req-1"})
	ctx := evidenceContext(t, b.done(), validSnapshot(), validInput())
	ctx.ledger.Coverage = HistoryComplete
	return ctx
}

func retryLimitViolationContext(t *testing.T) *evalContext {
	zero := 0
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: &zero})
	b.event(2, 200, "RetryScheduled", Detail{Request: "req-1"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func retryBlockedViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RetryExecution", Detail{Request: "req-1"}).
		note(1, 100, "retry-admission", Detail{Request: "req-1", Decision: "blocked", Reason: "mysterious-block"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func admissionOutcomeViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: retriesPtr(0)}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "shed"}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func queueIntervalViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(1, 100, "queue", Detail{Attempt: "att-1", Tenant: "T1", Lane: "arrival", Seq: 1, Count: 1, Decision: "append"}).
		note(1, 100, "queue", Detail{Attempt: "att-1", Tenant: "T1", Lane: "arrival", Seq: 2, Count: 2, Decision: "append"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func fifoDispatchViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "QueueRemoval", Detail{Attempt: "att-2"}).
		note(1, 100, "queue", Detail{Attempt: "att-2", Tenant: "T1", Seq: 2, Decision: "remove"})
	b.event(2, 100, "QueueRemoval", Detail{Attempt: "att-1"}).
		note(2, 100, "queue", Detail{Attempt: "att-1", Tenant: "T1", Seq: 1, Decision: "remove"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func observedLaneBoundsViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	for i := 1; i <= 5; i++ {
		aid := fmt.Sprintf("att-%d", i)
		b.event(uint64(i), 100, "QueueInsertion", Detail{Attempt: aid}).
			note(uint64(i), 100, "queue", Detail{Attempt: aid, Tenant: "T1", Lane: "arrival", Seq: uint64(i), Count: i, Decision: "append"})
	}
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func breakerOpenThresholdViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "BreakerTransitioned", Detail{Upstream: "U1", From: "closed", To: "open"}).
		note(1, 100, "breaker", Detail{Upstream: "U1", From: "closed", To: "open"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func breakerCooldownViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 1_000_000_000, "BreakerTransitioned", Detail{Upstream: "U1", From: "closed", To: "open"}).
		note(1, 1_000_000_000, "breaker", Detail{Upstream: "U1", From: "closed", To: "open"})
	b.event(2, 2_000_000_000, "BreakerTransitioned", Detail{Upstream: "U1", From: "open", To: "half-open"}).
		note(2, 2_000_000_000, "breaker", Detail{Upstream: "U1", From: "open", To: "half-open"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func probeAllowanceViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "RetryExecution", Detail{Request: "req-1"}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "retry"}).
		note(1, 100, "probe-reserved", Detail{Attempt: "att-1", Upstream: "U1", Decision: "reserved"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func failureObservationViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "ExecutionCompletion", Detail{Attempt: "att-1"}).
		note(1, 100, "breaker", Detail{Upstream: "U1", Decision: "failure-counted", Count: 2})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func arrivalEnvelopeViolationContext(t *testing.T) *evalContext {
	zero := 0
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 70_000_000_000, MaxRetries: &zero})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

func burstMaterializationViolationContext(t *testing.T) *evalContext {
	zero := 0
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: &zero})
	b.event(2, 200, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(2, 200, "request-created", Detail{Request: "req-2", Tenant: "T1", Upstream: "U1", DeadlineNS: 1000, MaxRetries: &zero})
	ctx := evidenceContext(t, b.done(), validSnapshot(), validInput())
	ctx.ledger.Coverage = HistoryComplete
	ctx.world.Coverage = HistoryComplete
	return ctx
}

func faultIntervalsViolationContext(t *testing.T) *evalContext {
	b := newEvidence()
	b.event(1, 100, "UpstreamDegraded", Detail{Upstream: "U1"})
	return evidenceContext(t, b.done(), validSnapshot(), validInput())
}

package verify

import (
	"bytes"
	"testing"
)

// evBuilder assembles ledger evidence with explicit correlation fields. It keeps
// the N:1 contract: one event record per engine step, followed by its effects.
type evBuilder struct {
	ev     Evidence
	effect uint64
}

func newEvidence() *evBuilder {
	return &evBuilder{ev: Evidence{SchemaVersion: RecordSchemaVersion}}
}

// newEvidenceAt starts the effect sequence at first, producing a truncated
// (Partial) ledger head.
func newEvidenceAt(first uint64) *evBuilder {
	return &evBuilder{ev: Evidence{SchemaVersion: RecordSchemaVersion, FirstEffectSeq: first}, effect: first}
}

func (b *evBuilder) append(engine uint64, timeNS int64, category, kind string, d Detail) *evBuilder {
	b.ev.Records = append(b.ev.Records, Record{
		SchemaVersion: RecordSchemaVersion,
		EffectSeq:     b.effect,
		EngineSeq:     engine,
		SimTimeNS:     timeNS,
		Category:      category,
		Kind:          kind,
		Detail:        d,
	})
	b.effect++
	return b
}

func (b *evBuilder) event(engine uint64, timeNS int64, kind string, d Detail) *evBuilder {
	return b.append(engine, timeNS, "event", kind, d)
}

func (b *evBuilder) note(engine uint64, timeNS int64, kind string, d Detail) *evBuilder {
	return b.append(engine, timeNS, "note", kind, d)
}

func (b *evBuilder) done() Evidence { return b.ev }

// validLifecycleEvidence is one request with one attempt, followed from
// admission to completion. It exercises the documented typing: request and
// attempt transitions, the typed request-created payload emitted by the
// reference's arrival handler, and the D1 typed creation effect (which the
// reference emits in the arrival block, in the arrival lane).
func validLifecycleEvidence() Evidence {
	zero := 0
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-created", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", DeadlineNS: 1_100, MaxRetries: &zero}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(2, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1", Decision: "admit"})
	b.event(3, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(3, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "queued"}).
		note(3, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	b.event(4, 100, "QueueRemoval", Detail{Attempt: "att-1"}).
		note(4, 100, "attempt-transition", Detail{Attempt: "att-1", From: "queued", To: "dispatching"}).
		note(4, 100, "request-transition", Detail{Request: "req-1", From: "queued", To: "dispatched"})
	b.event(5, 100, "WorkerAssignment", Detail{Attempt: "att-1"}).
		note(5, 100, "attempt-transition", Detail{Attempt: "att-1", From: "dispatching", To: "assigned"})
	b.event(6, 100, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(6, 100, "attempt-transition", Detail{Attempt: "att-1", From: "assigned", To: "running"})
	b.event(7, 500, "ExecutionCompletion", Detail{Attempt: "att-1"}).
		note(7, 500, "attempt-transition", Detail{Attempt: "att-1", From: "running", To: "completed"}).
		note(7, 500, "request-transition", Detail{Request: "req-1", From: "dispatched", To: "completed"})
	return b.done()
}

func reconstructed(t *testing.T, ev Evidence) *World {
	t.Helper()
	led, fs := ValidateEvidence(ev)
	if len(fs) > 0 {
		t.Fatalf("evidence did not validate: %+v", fs)
	}
	return reconstruct(&led)
}

func propertyByID(results []PropertyResult, id string) *PropertyResult {
	for i := range results {
		if results[i].ID == id {
			return &results[i]
		}
	}
	return nil
}

func TestReconstructionValidLifecycle(t *testing.T) {
	w := reconstructed(t, validLifecycleEvidence())
	if len(w.Fatal) != 0 {
		t.Fatalf("fatal findings: %+v", w.Fatal)
	}
	if len(w.Requests) != 1 || len(w.Attempts) != 1 {
		t.Fatalf("requests=%d attempts=%d, want 1/1", len(w.Requests), len(w.Attempts))
	}
	r := w.Requests[0]
	if r.ID != "req-1" || r.Tenant != "T1" || r.Upstream != "U1" {
		t.Fatalf("request identity not reconstructed: %+v", r)
	}
	if r.State != "completed" || !r.Terminal || r.TerminalAtNS != 500 {
		t.Fatalf("request terminal state not reconstructed: %+v", r)
	}
	if !r.Complete || len(r.Transitions) != 3 {
		t.Fatalf("request history = %+v, complete=%t", r.Transitions, r.Complete)
	}
	a := w.Attempts[0]
	if a.ID != "att-1" || a.Request != "req-1" || !a.RequestLinked {
		t.Fatalf("attempt identity/parentage not reconstructed: %+v", a)
	}
	if a.Tenant != "T1" || a.Upstream != "U1" || a.Lane != "arrival" || !a.CreatedSeen {
		t.Fatalf("typed creation facts lost: %+v", a)
	}
	if a.State != "completed" || !a.Terminal || a.FinishedAtNS != 500 {
		t.Fatalf("attempt terminal state not reconstructed: %+v", a)
	}
	if !a.Complete || len(a.Transitions) != 5 {
		t.Fatalf("attempt history = %+v, complete=%t", a.Transitions, a.Complete)
	}
	if len(r.AttemptIDs) != 1 || r.AttemptIDs[0] != "att-1" {
		t.Fatalf("request attempt list = %v", r.AttemptIDs)
	}

	in := validInput()
	for _, p := range lPathChecksWithInput(w, in) {
		wantStatus := StatusPass
		if p.ID == "D-retry-backoff" || p.ID == "H-burst-materialization" || p.ID == "H-seeded-replay" {
			wantStatus = StatusInconclusive
		}
		if p.Status != wantStatus {
			t.Fatalf("property %s = %s, want %s (%+v %+v)", p.ID, p.Status, wantStatus, p.Findings, p.Gaps)
		}
	}
}

func TestReconstructionIllegalTransition(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		b := newEvidence()
		b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
			note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"}).
			note(1, 100, "request-transition", Detail{Request: "req-1", From: "queued", To: "completed"})
		w := reconstructed(t, b.done())
		if len(w.RequestViolations) != 1 {
			t.Fatalf("violations = %+v, want one illegal transition", w.RequestViolations)
		}
		f := w.RequestViolations[0]
		if f.Code != "L-request-transition-illegal" || f.Classification != ClassReferenceDefectCandidate {
			t.Fatalf("finding = %+v", f)
		}
		p := propertyByID(lPathChecks(w), "L-request-lifecycle")
		if p == nil || p.Status != StatusFail {
			t.Fatalf("L-request-lifecycle = %+v, want FAIL", p)
		}
	})

	t.Run("attempt", func(t *testing.T) {
		b := newEvidence()
		b.event(1, 100, "ExecutionStart", Detail{Attempt: "att-1"}).
			note(1, 100, "attempt-transition", Detail{Attempt: "att-1", From: "assigned", To: "completed"})
		w := reconstructed(t, b.done())
		if len(w.AttemptViolations) != 1 {
			t.Fatalf("violations = %+v, want one illegal transition", w.AttemptViolations)
		}
		p := propertyByID(lPathChecks(w), "L-attempt-lifecycle")
		if p == nil || p.Status != StatusFail {
			t.Fatalf("L-attempt-lifecycle = %+v, want FAIL", p)
		}
	})
}

func TestReconstructionContinuity(t *testing.T) {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "queued", To: "dispatched"}).
		// Legal transition, but it does not start from the reconstructed state.
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "completed"})
	w := reconstructed(t, b.done())
	if len(w.ContinuityViolations) != 1 {
		t.Fatalf("continuity violations = %+v", w.ContinuityViolations)
	}
	f := w.ContinuityViolations[0]
	if f.Code != "L-request-history-discontinuity" || f.Expected != "dispatched" || f.Observed != "admitted" {
		t.Fatalf("finding = %+v", f)
	}
	p := propertyByID(lPathChecks(w), "L-request-lifecycle")
	if p == nil || p.Status != StatusFail {
		t.Fatalf("L-request-lifecycle = %+v, want FAIL", p)
	}
}

func TestReconstructionPartialLedger(t *testing.T) {
	b := newEvidenceAt(42)
	b.event(100, 1_000, "QueueRemoval", Detail{Attempt: "att-1"}).
		note(100, 1_000, "attempt-transition", Detail{Attempt: "att-1", From: "queued", To: "dispatching"}).
		note(100, 1_000, "request-transition", Detail{Request: "req-1", From: "dispatched", To: "retrying"})
	w := reconstructed(t, b.done())
	if !w.Partial {
		t.Fatal("truncated ledger not marked partial")
	}
	if w.Requests[0].Complete || w.Attempts[0].Complete {
		t.Fatalf("mid-lifecycle histories claimed complete: %+v %+v", w.Requests[0], w.Attempts[0])
	}
	p := propertyByID(lPathChecks(w), "L-history-completeness")
	if p == nil || p.Status != StatusInconclusive || len(p.Gaps) < 2 {
		t.Fatalf("L-history-completeness = %+v, want INCONCLUSIVE with gaps", p)
	}
	q := propertyByID(lPathChecks(w), "L-retry-budget-ledger")
	if q == nil || q.Status != StatusInconclusive {
		t.Fatalf("L-retry-budget-ledger = %+v, want INCONCLUSIVE on a truncated ledger", q)
	}
}

func TestReconstructionMissingTransitionEvidence(t *testing.T) {
	// The attempt is first observed mid-lifecycle: no creation effect and no
	// earlier transitions. That is an evidence gap, never a violation.
	b := newEvidence()
	b.event(1, 1_000, "ExecutionStart", Detail{Attempt: "att-1"}).
		note(1, 1_000, "attempt-transition", Detail{Attempt: "att-1", From: "assigned", To: "running"})
	w := reconstructed(t, b.done())
	if len(w.AttemptViolations) != 0 || len(w.ContinuityViolations) != 0 {
		t.Fatalf("missing history reported as a violation: %+v %+v", w.AttemptViolations, w.ContinuityViolations)
	}
	if w.Attempts[0].Complete {
		t.Fatal("attempt with a mid-lifecycle first observation claimed complete")
	}
	results := lPathChecks(w)
	if p := propertyByID(results, "L-attempt-lifecycle"); p == nil || p.Status != StatusPass {
		t.Fatalf("L-attempt-lifecycle = %+v", p)
	}
	if p := propertyByID(results, "L-history-completeness"); p == nil || p.Status != StatusInconclusive {
		t.Fatalf("L-history-completeness = %+v, want INCONCLUSIVE", p)
	}
	if p := propertyByID(results, "L-attempt-linkage"); p == nil || p.Status != StatusInconclusive {
		t.Fatalf("L-attempt-linkage = %+v, want INCONCLUSIVE", p)
	}
}

func TestReconstructionDuplicateCreationIsUninterpretable(t *testing.T) {
	b := newEvidence()
	b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})
	b.event(2, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(2, 100, "attempt-created", Detail{Request: "req-1", Attempt: "att-1", Tenant: "T1", Upstream: "U1", Lane: "arrival"})

	doc := Document{Schema: SchemaDocument, Scenario: "A", RunID: "A", Input: validInput(), Evidence: b.done()}
	v := Verify(doc)
	if v.Status != StatusInvalidInput {
		t.Fatalf("verdict = %s, want INVALID_INPUT", v.Status)
	}
	if !hasCode(v.Findings, "L-attempt-created-twice") {
		t.Fatalf("findings = %+v", v.Findings)
	}
	if v.EvidenceDigest != "" {
		t.Fatal("uninterpretable evidence must not receive a digest")
	}
}

func TestReconstructionIdentityConflictIsUninterpretable(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
		code string
	}{
		{
			name: "conflicting tenant",
			ev: func() Evidence {
				b := newEvidence()
				b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"})
				b.event(2, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T2", Upstream: "U1"})
				return b.done()
			}(),
			code: "L-identity-conflict",
		},
		{
			name: "unknown request state",
			ev: func() Evidence {
				b := newEvidence()
				b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
					note(1, 100, "request-transition", Detail{Request: "req-1", From: "vanished", To: "queued"})
				return b.done()
			}(),
			code: "L-request-state-unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := Document{Schema: SchemaDocument, Scenario: "A", RunID: "A", Input: validInput(), Evidence: tc.ev}
			v := Verify(doc)
			if v.Status != StatusInvalidInput || !hasCode(v.Findings, tc.code) {
				t.Fatalf("verdict = %s findings=%+v, want INVALID_INPUT with %s", v.Status, v.Findings, tc.code)
			}
		})
	}
}

// TestReconstructionLegacyAttemptCreation covers evidence recorded before D1:
// the creation note is prose-only, so parentage falls back to block-local typed
// references instead of being rejected.
func TestReconstructionLegacyAttemptCreation(t *testing.T) {
	b := newEvidence()
	b.event(1, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"})
	b.event(2, 100, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(2, 100, "attempt-transition", Detail{Attempt: "att-1", From: "created", To: "queued"}).
		note(2, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	w := reconstructed(t, b.done())
	if len(w.Fatal) != 0 {
		t.Fatalf("legacy evidence rejected: %+v", w.Fatal)
	}
	a := w.Attempts[0]
	if !a.RequestLinked || a.Request != "req-1" {
		t.Fatalf("legacy fallback linkage failed: %+v", a)
	}
	if !a.Complete {
		t.Fatalf("legacy attempt history claimed incomplete: %+v", a)
	}
}

func TestReconstructionIsDeterministic(t *testing.T) {
	ev := validLifecycleEvidence()
	first := reconstructed(t, ev)
	second := reconstructed(t, ev)
	a, err := CanonicalJSON(first)
	if err != nil {
		t.Fatalf("canonical world: %v", err)
	}
	b, err := CanonicalJSON(second)
	if err != nil {
		t.Fatalf("canonical world: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("reconstruction is not deterministic:\n%s\n%s", a, b)
	}
}

// TestRetryBudgetLedgerContract covers the ledger-internal retry contract:
// each scheduled retry consumed exactly one budget token.
func TestRetryBudgetLedgerContract(t *testing.T) {
	base := func() *evBuilder {
		b := newEvidence()
		b.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
			note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
		b.event(2, 200, "RetryScheduled", Detail{Request: "req-1"}).
			note(2, 200, "budget", Detail{Description: "budget consumed"})
		return b
	}

	w := reconstructed(t, base().done())
	if p := propertyByID(lPathChecks(w), "L-retry-budget-ledger"); p == nil || p.Status != StatusPass {
		t.Fatalf("matching ledger = %+v, want PASS", p)
	}

	missing := newEvidence()
	missing.event(1, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"}).
		note(1, 100, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	missing.event(2, 200, "RetryScheduled", Detail{Request: "req-1"})
	w = reconstructed(t, missing.done())
	if p := propertyByID(lPathChecks(w), "L-retry-budget-ledger"); p == nil || p.Status != StatusFail {
		t.Fatalf("missing budget effect = %+v, want FAIL", p)
	}
}

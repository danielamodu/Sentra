package verify

import (
	"bytes"
	"strings"
	"testing"
)

// partialLifecycleEvidence is a valid but truncated ledger: the retained window
// begins after the attempt started running, so both lifecycles begin
// mid-history and entity history before the window is unknowable.
func partialLifecycleEvidence() Evidence {
	b := newEvidenceAt(42)
	b.event(100, 500, "ExecutionCompletion", Detail{Attempt: "att-1"})
	b.note(100, 500, "attempt-transition", Detail{Attempt: "att-1", From: "running", To: "completed"})
	b.note(100, 500, "request-transition", Detail{Request: "req-1", From: "dispatched", To: "completed"})
	return b.done()
}

func crossChecked(t *testing.T, ev Evidence, s *Snapshot) map[string]*PropertyResult {
	t.Helper()
	return crossCheckedWith(t, ev, s, HistoryTerminalUnestablished)
}

// crossCheckedComplete marks the reconstructed world terminal-complete. It is
// synthetic: evidence contract v1 has no terminal-boundary marker, so only a
// test can produce the classification that a future schema will prove. It is
// used to exercise the checks themselves, never to claim a real run was fully
// observed.
func crossCheckedComplete(t *testing.T, ev Evidence, s *Snapshot) map[string]*PropertyResult {
	t.Helper()
	return crossCheckedWith(t, ev, s, HistoryComplete)
}

func crossCheckedWith(t *testing.T, ev Evidence, s *Snapshot, coverage HistoryCoverage) map[string]*PropertyResult {
	t.Helper()
	w := reconstructed(t, ev)
	w.Coverage = coverage
	results := crossPathChecks(w, s, validInput())
	if len(results) != crossPathPropertyCount() {
		t.Fatalf("results = %d, want %d", len(results), crossPathPropertyCount())
	}
	return resultsByID(results)
}

// TestCrossPathAgreement covers the healthy case with real, head-complete but
// terminal-unestablished evidence. Only parentage, whose fact is recorded
// rather than inferred from the absence of a later record, may PASS. Every
// claim that reads a total or a final state must stay INCONCLUSIVE with an
// explicit gap.
func TestCrossPathAgreement(t *testing.T) {
	byID := crossChecked(t, validLifecycleEvidence(), validSnapshot())
	for id, p := range byID {
		if id == "X-attempt-parentage" {
			if p.Status != StatusPass {
				t.Errorf("property %s = %s (findings %+v, gaps %+v)", id, p.Status, p.Findings, p.Gaps)
			}
			continue
		}
		if p.Status != StatusInconclusive {
			t.Errorf("property %s = %s on terminal-unestablished evidence, want INCONCLUSIVE", id, p.Status)
		}
		if len(p.Gaps) == 0 {
			t.Errorf("property %s is INCONCLUSIVE without a gap", id)
		}
		if len(p.Findings) != 0 {
			t.Errorf("property %s blamed agreeing evidence: %+v", id, p.Findings)
		}
	}
}

// TestCrossPathAgreementOnCompleteEvidence shows that the checks themselves
// hold when the terminal guarantee is present, so the INCONCLUSIVE results
// above are an evidence limitation rather than a broken comparison.
func TestCrossPathAgreementOnCompleteEvidence(t *testing.T) {
	byID := crossCheckedComplete(t, validLifecycleEvidence(), validSnapshot())
	for id, p := range byID {
		if p.Status != StatusPass {
			t.Errorf("property %s = %s on terminal-complete evidence (findings %+v, gaps %+v)", id, p.Status, p.Findings, p.Gaps)
		}
	}
}

// TestCrossPathAgreementDocumentIsInconclusive is the end-to-end consequence of
// the head/terminal split: agreeing books are no longer enough for PASS, because
// contract v1 cannot prove the retained ledger reaches the run's terminal
// boundary. The evidence is still usable and still carries a digest.
func TestCrossPathAgreementDocumentIsInconclusive(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "A-run-1",
		Input:    validInput(),
		Evidence: validLifecycleEvidence(),
	}
	doc.Evidence.Snapshot = validSnapshot()

	v := Verify(doc)
	if v.Status != StatusInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE\nfindings: %+v\ngaps: %+v", v.Status, v.Findings, v.Gaps)
	}
	if v.EvidenceDigest == "" {
		t.Fatal("a usable verdict has no evidence digest")
	}
	if len(v.Gaps) == 0 {
		t.Fatal("INCONCLUSIVE verdict has no gaps")
	}
}

// TestCrossPathDisagreementIsStructured checks that a disagreement between the
// two independent paths is reported as a finding with the property, the entity,
// both evidence references and the expected/observed relationship, and that it
// is never automatically attributed to the reference implementation.
func TestCrossPathDisagreementIsStructured(t *testing.T) {
	cases := []struct {
		name     string
		property string
		edit     func(*Snapshot)
		entity   string
		ledgerAt bool
		// complete asks for the synthetic terminal-complete world: these
		// properties skip evaluation without the terminal guarantee, because a
		// disagreement could equally be explained by a later record that the
		// retained window does not contain.
		complete bool
	}{
		{
			name:     "attempt final state",
			property: "X-attempt-final-state",
			edit:     func(s *Snapshot) { s.Attempts[0].State = "failed" },
			entity:   "att-1",
			ledgerAt: true,
			complete: true,
		},
		{
			name:     "request terminal instant",
			property: "X-request-final-state",
			edit:     func(s *Snapshot) { s.Requests[0].TerminalAtNS = 600 },
			entity:   "req-1",
			ledgerAt: true,
			complete: true,
		},
		{
			name:     "attempt parentage",
			property: "X-attempt-parentage",
			edit:     func(s *Snapshot) { s.Attempts[0].Request = "req-2" },
			entity:   "att-1",
			ledgerAt: true,
		},
		{
			name:     "breaker final state",
			property: "X-breaker-final-state",
			edit:     func(s *Snapshot) { s.Breakers[0].State = "open" },
			entity:   "U1",
			ledgerAt: false,
			complete: true,
		},
		{
			name:     "budget consumption",
			property: "X-budget-consumption",
			edit: func(s *Snapshot) {
				s.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 2}}
			},
			entity:   "",
			ledgerAt: false,
			complete: true,
		},
		{
			name:     "probe permits",
			property: "X-probe-ledger",
			edit: func(s *Snapshot) {
				s.Breakers[0].State = "half-open"
				s.Breakers[0].ProbesActive = 1
			},
			// The probe ledger is compared in aggregate: reserve/release notes
			// carry no upstream identity, so the verifier cannot attribute the
			// imbalance to one breaker without inventing information.
			entity:   "",
			ledgerAt: false,
			complete: true,
		},
		{
			name:     "tenant active count",
			property: "X-tenant-active",
			edit:     func(s *Snapshot) { s.TenantActive[0].Active = 1 },
			entity:   "T1",
			ledgerAt: false,
			complete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.edit(s)
			check := crossChecked
			if tc.complete {
				check = crossCheckedComplete
			}
			byID := check(t, validLifecycleEvidence(), s)
			p := byID[tc.property]
			if p == nil || p.Status != StatusFail {
				t.Fatalf("%s = %+v, want FAIL", tc.property, p)
			}
			for _, f := range p.Findings {
				if f.Code != tc.property {
					t.Errorf("finding code %q does not match property %q", f.Code, tc.property)
				}
				if f.Classification != ClassUnclassified {
					t.Errorf("disagreement classified %q; a cross-path disagreement must not be auto-attributed", f.Classification)
				}
				if f.Expected == "" || f.Observed == "" {
					t.Errorf("finding does not state expected/observed: %+v", f)
				}
				if f.SnapshotRef == "" {
					t.Errorf("finding has no snapshot reference: %+v", f)
				}
				if tc.entity != "" && !findingNames(f, tc.entity) {
					t.Errorf("finding does not name entity %s: %+v", tc.entity, f)
				}
				if tc.ledgerAt && f.EffectSeq == 0 && f.EngineSeq == 0 {
					t.Errorf("finding has no ledger reference: %+v", f)
				}
				if strings.Contains(strings.ToLower(f.Message), "reference defect") {
					t.Errorf("finding blames the reference implementation in prose: %s", f.Message)
				}
			}
		})
	}
}

func findingNames(f Finding, id string) bool {
	for _, field := range []string{f.Request, f.Attempt, f.Tenant, f.Upstream, f.Worker, f.SnapshotRef, f.Message} {
		if strings.Contains(field, id) {
			return true
		}
	}
	return false
}

// TestCrossPathEntitySets covers both directions of population equality:
// ledger-observed entities missing from the snapshot always disagree; entities
// present only in the snapshot disagree only when the ledger is complete.
func TestCrossPathEntitySets(t *testing.T) {
	t.Run("ledger entity missing from snapshot", func(t *testing.T) {
		s := validSnapshot()
		s.Attempts = nil
		p := crossChecked(t, validLifecycleEvidence(), s)["X-entity-sets"]
		if p.Status != StatusFail || !propertyHasCode(p, "X-entity-sets") {
			t.Fatalf("X-entity-sets = %+v, want FAIL", p)
		}
	})

	t.Run("snapshot-only entity with complete ledger", func(t *testing.T) {
		s := validSnapshot()
		s.Requests = append(s.Requests, RequestFact{
			ID: "req-2", Tenant: "T1", Upstream: "U1", State: "completed",
			ArrivedAtNS: 100, DeadlineNS: 60_000_000_000, TerminalAtNS: 500,
		})
		p := crossCheckedComplete(t, validLifecycleEvidence(), s)["X-entity-sets"]
		if p.Status != StatusFail {
			t.Fatalf("X-entity-sets = %+v, want FAIL for a complete ledger", p)
		}
		if !propertyHasCode(p, "X-entity-sets") {
			t.Fatalf("findings = %+v", p.Findings)
		}
	})

	t.Run("snapshot-only entity with truncated ledger cannot claim equality", func(t *testing.T) {
		s := validSnapshot()
		s.Requests = append(s.Requests, RequestFact{
			ID: "req-2", Tenant: "T1", Upstream: "U1", State: "completed",
			ArrivedAtNS: 100, DeadlineNS: 60_000_000_000, TerminalAtNS: 500,
		})
		p := crossChecked(t, partialLifecycleEvidence(), s)["X-entity-sets"]
		if p.Status != StatusInconclusive {
			t.Fatalf("X-entity-sets = %+v; an evicted head makes population equality INCONCLUSIVE", p)
		}
		if len(p.Findings) != 0 {
			t.Fatalf("an evicted entity must not be blamed on the snapshot: %+v", p.Findings)
		}
		if len(p.Gaps) == 0 {
			t.Fatal("INCONCLUSIVE entity-set result has no gap")
		}
	})
}

// TestCrossPathInsufficientEvidence enforces the rule that a comparison must not
// claim agreement when one path does not establish the fact.
func TestCrossPathInsufficientEvidence(t *testing.T) {
	t.Run("no snapshot", func(t *testing.T) {
		w := reconstructed(t, validLifecycleEvidence())
		for _, p := range crossPathChecks(w, nil, validInput()) {
			if p.Status != StatusInconclusive {
				t.Errorf("%s = %s, want INCONCLUSIVE", p.ID, p.Status)
			}
			if len(p.Gaps) == 0 {
				t.Errorf("%s has no gap explaining what is missing", p.ID)
			}
			if len(p.Findings) != 0 {
				t.Errorf("%s blamed a finding on absent evidence: %+v", p.ID, p.Findings)
			}
		}
	})

	t.Run("truncated ledger", func(t *testing.T) {
		byID := crossChecked(t, partialLifecycleEvidence(), validSnapshot())
		for _, id := range []string{"X-budget-consumption", "X-probe-ledger", "X-tenant-active"} {
			p := byID[id]
			if p == nil || p.Status != StatusInconclusive {
				t.Errorf("%s = %+v, want INCONCLUSIVE on a truncated ledger", id, p)
			}
			if p != nil && len(p.Gaps) == 0 {
				t.Errorf("%s has no gap explaining the truncation", id)
			}
		}
	})
}

// TestCrossPathDisagreementIsFailNotInvalidInput keeps verdict semantics
// distinct: two consistent books that disagree are a FAIL, while unusable
// evidence is INVALID_INPUT. Parentage is used deliberately: it is the one
// cross-path claim whose fact is recorded rather than inferred, so it remains
// evaluable without a terminal-boundary proof.
func TestCrossPathDisagreementIsFailNotInvalidInput(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "A-run-1",
		Input:    validInput(),
		Evidence: validLifecycleEvidence(),
	}
	snap := validSnapshot()
	// Move att-1 under a second, otherwise identical request. The snapshot
	// stays internally consistent (each request lists exactly its attempts),
	// while the ledger still attributes att-1 to req-1.
	snap.Requests[0].Attempts = nil
	snap.Requests = append(snap.Requests, RequestFact{
		ID: "req-2", Tenant: "T1", Upstream: "U1", State: "completed",
		ArrivedAtNS: 100, DeadlineNS: 60_000_000_000, TerminalAtNS: 500,
		Attempts: []string{"att-1"},
	})
	snap.Attempts[0].Request = "req-2"
	doc.Evidence.Snapshot = snap

	v := Verify(doc)
	if v.Status != StatusFail {
		t.Fatalf("verdict = %s, want FAIL (findings %+v)", v.Status, v.Findings)
	}
	if v.EvidenceDigest == "" {
		t.Fatal("consistent evidence must keep its digest even when paths disagree")
	}
	found := false
	for _, f := range v.Findings {
		if f.Code == "X-attempt-parentage" {
			found = true
			if f.Classification != ClassUnclassified {
				t.Fatalf("disagreement classified %q", f.Classification)
			}
		}
	}
	if !found {
		t.Fatalf("verdict findings do not name the disagreement: %+v", v.Findings)
	}
}

// TestTerminalDependentDisagreementIsInconclusiveNotInvalidInput proves the
// other half of the distinction: a disagreement that genuinely depends on the
// unobserved tail leaves the evidence usable. It must be INCONCLUSIVE with a
// gap, never INVALID_INPUT and never FAIL, because the retained window cannot
// exclude a later record. The retry-budget total is such a fact: the snapshot's
// larger count may come from consumptions after the last retained record.
func TestTerminalDependentDisagreementIsInconclusiveNotInvalidInput(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "A-run-1",
		Input:    validInput(),
		Evidence: validLifecycleEvidence(),
	}
	doc.Evidence.Snapshot = validSnapshot()
	doc.Evidence.Snapshot.Budgets = []BudgetFact{{Tenant: "T1", Upstream: "U1", Capacity: 3, Tokens: 1, RefillIntervalNS: 1_000_000_000}}
	doc.Evidence.Snapshot.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 2}}

	v := Verify(doc)
	if v.Status != StatusInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE (findings %+v)", v.Status, v.Findings)
	}
	if v.EvidenceDigest == "" {
		t.Fatal("consistent evidence must keep its digest")
	}
	p := resultsByID(v.Properties)["X-budget-consumption"]
	if p == nil || p.Status != StatusInconclusive || len(p.Gaps) == 0 {
		t.Fatalf("X-budget-consumption = %+v, want INCONCLUSIVE with a gap", p)
	}
	if len(p.Findings) != 0 {
		t.Fatalf("terminal-dependent property reported a finding it cannot establish: %+v", p.Findings)
	}
}

// TestCorruptFinalStateStillDetectedUnderCapSemantics is the Section 2
// regression: cap semantics must not hide an observable contradiction. The
// ledger reconstructs req-1 as terminal ("completed"), and a terminal state is
// absorbing, so the snapshot's "failed" is a positive contradiction even though
// the ledger tail is unproven. The property must FAIL, not degrade to
// INCONCLUSIVE, and the cap must not add a gap to a failing result.
func TestCorruptFinalStateStillDetectedUnderCapSemantics(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "corrupt-final-state",
		Input:    validInput(),
		Evidence: validLifecycleEvidence(),
	}
	doc.Evidence.Snapshot = validSnapshot()
	doc.Evidence.Snapshot.Requests[0].State = "failed"

	v := Verify(doc)
	p := resultsByID(v.Properties)["X-request-final-state"]
	if p == nil || p.Status != StatusFail {
		t.Fatalf("X-request-final-state = %+v, want FAIL", p)
	}
	if len(p.Findings) == 0 {
		t.Fatal("FAIL carries no finding")
	}
	if len(p.Gaps) != 0 {
		t.Fatalf("FAIL must not also carry a shortfall gap: %+v", p.Gaps)
	}
	for _, f := range p.Findings {
		if f.Code != "X-request-final-state" {
			t.Fatalf("unexpected finding code %q", f.Code)
		}
		if f.Expected != "completed" || f.Observed != "failed" {
			t.Fatalf("finding does not name the contradiction: %+v", f)
		}
	}
}

// TestNonTerminalReconstructionIsNotCompared proves the negative side of the
// cap rule: on terminal-unestablished evidence a non-terminal reconstruction
// says nothing about the final state, so no finding may be fabricated.
func TestNonTerminalReconstructionIsNotCompared(t *testing.T) {
	// Drop the completion records: the retained ledger stops while the attempt
	// is still running, so the reconstruction is non-terminal.
	full := validLifecycleEvidence()
	ev := Evidence{
		SchemaVersion: RecordSchemaVersion,
		Records:       append([]Record(nil), full.Records[:14]...),
	}
	ctx := evidenceContext(t, ev, validSnapshot(), validInput())
	p := evalProperty(t, "X-attempt-final-state", ctx)
	if p.Status != StatusInconclusive {
		t.Fatalf("X-attempt-final-state = %s (findings %+v), want INCONCLUSIVE", p.Status, p.Findings)
	}
	if len(p.Findings) != 0 {
		t.Fatalf("a non-terminal reconstruction produced a final-state finding: %+v", p.Findings)
	}
	if len(p.Gaps) == 0 {
		t.Fatal("INCONCLUSIVE without a gap")
	}
}

// TestBreakerReverseDirectionNeedsFullHistory proves the repaired direction:
// "no transition observed" pins the initial breaker state only when the ledger
// covers the whole run. On a head-complete but terminal-unestablished ledger the
// transition may simply be after the last retained record.
func TestBreakerReverseDirectionNeedsFullHistory(t *testing.T) {
	snap := validSnapshot()
	snap.Breakers[0].State = "open"

	byID := crossChecked(t, validLifecycleEvidence(), snap)
	p := byID["X-breaker-final-state"]
	if p.Status != StatusInconclusive {
		t.Fatalf("X-breaker-final-state = %s (findings %+v), want INCONCLUSIVE on a tail-unproven ledger", p.Status, p.Findings)
	}
	if len(p.Findings) != 0 {
		t.Fatalf("tail-unproven ledger pinned a breaker state it cannot establish: %+v", p.Findings)
	}

	// With a proven tail the same evidence is a contradiction.
	complete := crossCheckedComplete(t, validLifecycleEvidence(), snap)
	p = complete["X-breaker-final-state"]
	if p.Status != StatusFail {
		t.Fatalf("X-breaker-final-state = %s, want FAIL on full history (findings %+v)", p.Status, p.Findings)
	}
	if len(p.Findings) != 1 || p.Findings[0].Expected != "closed" || p.Findings[0].Observed != "open" {
		t.Fatalf("unexpected findings: %+v", p.Findings)
	}
}

func TestCrossPathChecksAreDeterministic(t *testing.T) {
	a, err := CanonicalJSON(crossPathChecks(reconstructed(t, validLifecycleEvidence()), validSnapshot(), validInput()))
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	b, err := CanonicalJSON(crossPathChecks(reconstructed(t, validLifecycleEvidence()), validSnapshot(), validInput()))
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("cross-path checks are not deterministic:\n%s\n%s", a, b)
	}
}

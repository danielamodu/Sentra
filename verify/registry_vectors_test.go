package verify

import (
	"testing"
)

// TestRegistryFixtureVectors is the aggregate golden test for the registry
// migration. It pins the per-property status vector of the reference fixtures so
// that a future check change cannot silently move a status without the change
// being visible here.
//
// The vectors encode two rounds of approved change:
//
//   - the three partial-ledger repairs (L-history-completeness, X-entity-sets,
//     X-breaker-final-state become INCONCLUSIVE on a head-truncated ledger);
//   - the head/terminal split: contract v1 cannot prove the terminal boundary,
//     so every claim that depends on the absence of a later record is
//     INCONCLUSIVE even on a complete head.
//
// "full" is therefore NOT an all-PASS fixture: it is head-complete and
// terminal-unestablished, and the seven terminal-dependent X properties are
// INCONCLUSIVE. Only X-attempt-parentage, whose fact is recorded rather than
// inferred, remains evaluable on the X-path.
var newInconclusiveClaims = []string{
	"D-retry-backoff",
	"H-burst-materialization",
	"H-seeded-replay",
}

var headCappedClaims = []string{
	"F-terminal-effect-closure",
	"B-worker-interval-pairing",
	"B-worker-occupancy",
	"B-connection-interval-pairing",
	"C-queue-interval",
	"C-lane-bounds",
	"E-probe-allowance",
	"E-failure-observation",
	"H-fault-intervals",
}

func TestRegistryFixtureVectors(t *testing.T) {
	fixtures := fixtureDocuments()

	want := map[string]map[Status][]string{
		"full": {
			StatusInconclusive: append(terminalClaims(), newInconclusiveClaims...),
		},
		"no-snapshot": {
			StatusInconclusive: append(append(snapshotDependent(), allCrossPath()...), newInconclusiveClaims...),
		},
		"head-truncated": {
			StatusInconclusive: append(append(terminalClaims(),
				"L-history-completeness", "L-retry-budget-ledger"), append(newInconclusiveClaims, headCappedClaims...)...),
		},
		"corrupt-final-state": {
			// Cap semantics keep the observable contradiction: the ledger
			// reconstructs req-1 as terminal, so the snapshot's contradictory
			// state is still a positive FAIL even though the tail is unproven.
			StatusFail:         []string{"X-request-final-state"},
			StatusInconclusive: append(corruptTerminalClaims(), newInconclusiveClaims...),
		},
	}

	for name, doc := range fixtures {
		doc := doc
		t.Run(name, func(t *testing.T) {
			assertVectorListsOnce(t, name, want[name])

			v := Verify(doc)
			if len(v.Properties) != len(propertyRegistry) {
				t.Fatalf("verdict has %d properties, want %d", len(v.Properties), len(propertyRegistry))
			}
			for i, p := range v.Properties {
				if p.ID != propertyRegistry[i].ID {
					t.Fatalf("property[%d] = %q, want registry order %q", i, p.ID, propertyRegistry[i].ID)
				}
			}

			byID := resultsByID(v.Properties)
			for _, id := range propertyIDs() {
				p := byID[id]
				if p == nil {
					t.Fatalf("property %s missing from verdict", id)
				}
				wantStatus := wantStatusFor(want[name], id)
				if p.Status != wantStatus {
					t.Errorf("%s = %s, want %s (findings %+v, gaps %+v)", id, p.Status, wantStatus, p.Findings, p.Gaps)
				}
				switch p.Status {
				case StatusPass:
					if len(p.Gaps) != 0 || len(p.Findings) != 0 {
						t.Errorf("%s PASS carries findings %+v or gaps %+v", id, p.Findings, p.Gaps)
					}
				case StatusInconclusive:
					if len(p.Gaps) == 0 {
						t.Errorf("%s INCONCLUSIVE without a gap", id)
					}
					if len(p.Findings) != 0 {
						t.Errorf("%s INCONCLUSIVE also reports findings %+v", id, p.Findings)
					}
				case StatusFail:
					if len(p.Findings) == 0 {
						t.Errorf("%s FAIL without a finding", id)
					}
				}
			}

			wantVerdict := Aggregate(v.Properties)
			if v.Status != wantVerdict {
				t.Fatalf("verdict status = %s, aggregate = %s", v.Status, wantVerdict)
			}
		})
	}
}

// TestPipelineCounterexampleOnIllegalTransition checks that a ledger finding
// still receives its bounded window through Verify, including when the primary
// block is effect_seq 0 / engine_seq 1 (a zero effect sequence is a real
// reference, not "no reference").
func TestPipelineCounterexampleOnIllegalTransition(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "illegal-transition",
		Input:    validInput(),
		Evidence: Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records:       illegalRequestTransitionEvidence().Records,
		},
	}
	v := Verify(doc)
	if v.Status != StatusFail {
		t.Fatalf("verdict = %s, want FAIL (findings %+v, gaps %+v)", v.Status, v.Findings, v.Gaps)
	}
	p := resultsByID(v.Properties)["L-request-lifecycle"]
	if p == nil || p.Status != StatusFail {
		t.Fatalf("L-request-lifecycle = %+v, want FAIL", p)
	}
	for _, f := range p.Findings {
		assertPrimaryCounterexample(t, f)
	}
}

func fixtureDocuments() map[string]Document {
	base := validLifecycleEvidence()
	full := func() Document {
		return Document{
			Schema:   SchemaDocument,
			Scenario: "A",
			RunID:    "A",
			Input:    validInput(),
			Evidence: Evidence{
				SchemaVersion:  RecordSchemaVersion,
				FirstEffectSeq: 0,
				Records:        append([]Record(nil), base.Records...),
				Snapshot:       validSnapshot(),
			},
		}
	}

	noSnapshot := full()
	noSnapshot.Evidence.Snapshot = nil

	truncated := full()
	truncated.Evidence.FirstEffectSeq = 4
	truncated.Evidence.Records = append([]Record(nil), base.Records[4:]...)

	corrupt := full()
	bad := validSnapshot()
	bad.Requests[0].State = "failed"
	corrupt.Evidence.Snapshot = bad

	return map[string]Document{
		"full":                full(),
		"no-snapshot":         noSnapshot,
		"head-truncated":      truncated,
		"corrupt-final-state": corrupt,
	}
}

// terminalClaims are the cross-path properties whose fact depends on the
// absence of a later record. They cannot PASS without a proven terminal
// boundary.
func terminalClaims() []string {
	return []string{
		"X-budget-consumption",
		"X-breaker-final-state",
		"X-probe-ledger",
		"X-entity-sets",
		"X-request-final-state",
		"X-attempt-final-state",
		"X-tenant-active",
	}
}

// corruptTerminalClaims is terminalClaims without X-request-final-state, which
// cap semantics keep evaluable on this fixture.
func corruptTerminalClaims() []string {
	var out []string
	for _, id := range terminalClaims() {
		if id != "X-request-final-state" {
			out = append(out, id)
		}
	}
	return out
}

// snapshotDependent are the properties that cannot be evaluated without final
// books.
func snapshotDependent() []string {
	var out []string
	for _, p := range propertyRegistry {
		if p.hasNeed(needSnapshot) && !p.hasSource(evidenceLedger) {
			out = append(out, p.ID)
		}
	}
	return out
}

func allCrossPath() []string {
	var out []string
	for _, p := range propertyRegistry {
		if p.hasSource(evidenceLedger) && p.hasSource(evidenceSnapshot) {
			out = append(out, p.ID)
		}
	}
	return out
}

// assertVectorListsOnce rejects a vector that lists the same property under two
// statuses, which would make the golden expectation nondeterministic.
func assertVectorListsOnce(t *testing.T, fixture string, vector map[Status][]string) {
	t.Helper()
	seen := make(map[string]Status, len(propertyRegistry))
	for status, ids := range vector {
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				t.Fatalf("fixture %s lists %s as both %s and %s", fixture, id, prev, status)
			}
			seen[id] = status
		}
	}
}

func wantStatusFor(vector map[Status][]string, id string) Status {
	for status, ids := range vector {
		for _, have := range ids {
			if have == id {
				return status
			}
		}
	}
	// Any status not listed is expected PASS; listing PASS explicitly is
	// optional, but a typo in a non-PASS list must not silently fall through.
	return StatusPass
}

// assertPrimaryCounterexample enforces the approved window contract on a real
// pipeline finding: the primary block is present, neighbours are context (no
// property establishes causality), and the window is flagged when clipped.
func assertPrimaryCounterexample(t *testing.T, f Finding) {
	t.Helper()
	if f.Counterexample == nil {
		t.Fatalf("finding %q has no counterexample window", f.Code)
	}
	primary := 0
	for _, b := range f.Counterexample.Blocks {
		switch b.Role {
		case counterexamplePrimary:
			primary++
			if b.EngineSeq != f.EngineSeq {
				t.Fatalf("primary block engine_seq = %d, want %d", b.EngineSeq, f.EngineSeq)
			}
		case counterexampleContext:
		default:
			t.Fatalf("unexpected counterexample role %q", b.Role)
		}
	}
	if primary != 1 {
		t.Fatalf("counterexample has %d primary blocks, want exactly 1", primary)
	}
}

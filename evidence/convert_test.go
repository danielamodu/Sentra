package evidence_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"sentra/domain"
	"sentra/evidence"
	"sentra/sim"
	"sentra/verify"
)

// propertyOrder is the registry order frozen by the M3 P3 migration. The
// evidence bridge asserts it explicitly so a registry reordering is visible here
// as well as in the verify package.
var propertyOrder = []string{
	"L-request-lifecycle",
	"L-attempt-lifecycle",
	"L-history-completeness",
	"L-attempt-linkage",
	"L-breaker-lifecycle",
	"L-retry-budget-ledger",
	"S-worker-conservation",
	"S-worker-ownership",
	"S-connection-ownership",
	"S-waitlist-membership",
	"S-connection-stranding",
	"S-horizon-completeness",
	"S-queue-consistency",
	"S-lane-bounds",
	"S-budget-state",
	"X-budget-consumption",
	"S-breaker-state",
	"S-probe-ledger",
	"X-breaker-final-state",
	"X-probe-ledger",
	"X-entity-sets",
	"X-request-final-state",
	"X-attempt-final-state",
	"X-attempt-parentage",
	"S-outcome-ledger",
	"S-tenant-active",
	"X-tenant-active",
	"F-effect-attribution",
	"F-attempt-state-effects",
	"F-creation-lane",
	"F-terminal-effect-closure",
	"F-retry-admission-context",
	"B-worker-interval-pairing",
	"B-worker-occupancy",
	"B-connection-interval-pairing",
	"D-retry-causality",
	"D-retry-backoff",
	"D-retry-limit",
	"D-retry-blocked",
	"C-admission-outcome",
	"C-queue-interval",
	"C-fifo-dispatch",
	"C-lane-bounds",
	"E-breaker-open-threshold",
	"E-breaker-cooldown",
	"E-probe-allowance",
	"E-failure-observation",
	"H-arrival-envelope",
	"H-burst-materialization",
	"H-fault-intervals",
	"H-seeded-replay",
}

// terminalDependent are the cross-path properties whose fact depends on the
// absence of a later record. With the terminal attestation present they PASS on
// every recorded scenario; when the attestation is removed (v1 evidence) they
// must become INCONCLUSIVE. The list is deliberately literal: it is the fixture
// expectation, not a computed mirror of the registry.
var terminalDependent = map[string]bool{
	"X-budget-consumption":    true,
	"X-breaker-final-state":   true,
	"X-probe-ledger":          true,
	"X-entity-sets":           true,
	"X-request-final-state":   true,
	"X-attempt-final-state":   true,
	"X-tenant-active":         true,
	"D-retry-backoff":         true,
	"H-burst-materialization": true,
}

// assertScenarioVector freezes one scenario's per-property status vector. A
// completed run carries a validated terminal attestation, so every property must
// PASS with no gaps (exempting H-seeded-replay, which is permanently INCONCLUSIVE).
func assertScenarioVector(t *testing.T, v verify.Verdict) {
	t.Helper()
	if len(v.Properties) != len(propertyOrder) {
		t.Fatalf("verdict has %d properties, want %d", len(v.Properties), len(propertyOrder))
	}
	for i, p := range v.Properties {
		if p.ID != propertyOrder[i] {
			t.Fatalf("property[%d] = %q, want %q", i, p.ID, propertyOrder[i])
		}
		if p.ID == "H-seeded-replay" {
			if p.Status != verify.StatusInconclusive {
				t.Errorf("%s = %s, want INCONCLUSIVE", p.ID, p.Status)
			}
			continue
		}
		if p.Status != verify.StatusPass {
			t.Errorf("%s = %s, want PASS (findings %+v, gaps %+v)", p.ID, p.Status, p.Findings, p.Gaps)
		}
		if len(p.Findings) != 0 {
			t.Errorf("%s reported findings on a healthy reference run: %+v", p.ID, p.Findings)
		}
		if len(p.Gaps) != 0 {
			t.Errorf("%s reported gaps on terminal-complete evidence: %+v", p.ID, p.Gaps)
		}
	}
	var nonReplayGaps []verify.Gap
	for _, g := range v.Gaps {
		if g.Property != "H-seeded-replay" {
			nonReplayGaps = append(nonReplayGaps, g)
		}
	}
	if len(nonReplayGaps) != 0 {
		t.Errorf("aggregate verdict carries gaps on terminal-complete evidence: %+v", nonReplayGaps)
	}
}

// assertV1Vector freezes the v1 compatibility vector: removing the terminal
// attestation must leave the document readable and must move exactly the
// terminal-dependent properties to INCONCLUSIVE.
func assertV1Vector(t *testing.T, v verify.Verdict) {
	t.Helper()
	var gaps []string
	for _, p := range v.Properties {
		want := verify.StatusPass
		if terminalDependent[p.ID] || p.ID == "H-seeded-replay" {
			want = verify.StatusInconclusive
			gaps = append(gaps, p.ID)
		}
		if p.Status != want {
			t.Errorf("%s = %s, want %s (findings %+v, gaps %+v)", p.ID, p.Status, want, p.Findings, p.Gaps)
		}
		if want == verify.StatusInconclusive && len(p.Gaps) == 0 {
			t.Errorf("%s is INCONCLUSIVE without an evidence gap", p.ID)
		}
	}
	var wantGaps []string
	for _, id := range propertyOrder {
		if terminalDependent[id] || id == "H-seeded-replay" {
			wantGaps = append(wantGaps, id)
		}
	}
	if strings.Join(gaps, ",") != strings.Join(wantGaps, ",") {
		t.Errorf("gap vector = %v, want %v", gaps, wantGaps)
	}
}

// TestScenarioABridgeProducesVerifiableDocument is the end-to-end serializer
// test: a real reference run is converted without repair and the independent
// verifier evaluates it. The recorder attests the run's terminal boundary, so the
// complete document passes. Removing only that attestation must leave the
// document readable and move exactly the terminal-dependent claims to
// INCONCLUSIVE.
func TestScenarioABridgeProducesVerifiableDocument(t *testing.T) {
	res := sim.RunExperiment(sim.ScenarioA())
	doc, err := evidence.FromRun(res)
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	if doc.Schema != verify.SchemaDocument {
		t.Fatalf("schema = %q", doc.Schema)
	}
	if doc.Scenario != res.Experiment.Name || doc.RunID == "" {
		t.Fatalf("identity not preserved: scenario %q run %q", doc.Scenario, doc.RunID)
	}
	if doc.Input == nil || doc.Input.HorizonNS != int64(res.Experiment.Horizon) {
		t.Fatalf("input horizon not preserved: %+v", doc.Input)
	}
	if len(doc.Evidence.Records) != len(res.Records) {
		t.Fatalf("records = %d, want %d", len(doc.Evidence.Records), len(res.Records))
	}
	if doc.Evidence.FirstEffectSeq != res.Records[0].EffectSeq {
		t.Fatalf("first effect = %d, want %d", doc.Evidence.FirstEffectSeq, res.Records[0].EffectSeq)
	}
	if doc.Evidence.Snapshot == nil {
		t.Fatal("snapshot was dropped")
	}
	if got, want := len(doc.Evidence.Snapshot.Requests), len(res.FinalState.Requests); got != want {
		t.Fatalf("snapshot requests = %d, want %d", got, want)
	}
	if got, want := len(doc.Evidence.Snapshot.Attempts), len(res.FinalState.Attempts); got != want {
		t.Fatalf("snapshot attempts = %d, want %d", got, want)
	}

	term := doc.Evidence.Terminal
	if term == nil {
		t.Fatal("completed run carries no terminal attestation")
	}
	if term.EffectSeq != res.TerminalEffectSeq || term.RecordCount != res.RecordCount || term.SimTimeNS != res.TerminalSimTimeNS {
		t.Fatalf("terminal attestation not preserved: %+v (run: seq=%d count=%d time=%d)", term, res.TerminalEffectSeq, res.RecordCount, res.TerminalSimTimeNS)
	}
	// The recorder and verifier compute the digest independently. They must
	// agree, otherwise the attestation would not bind the full run.
	if got := verify.TerminalRecordsDigest(doc.Evidence.Records); got != term.RecordsDigest {
		t.Fatalf("verifier digest %q disagrees with recorder digest %q", got, term.RecordsDigest)
	}

	v := verify.Verify(doc)
	if v.Status != verify.StatusInconclusive {
		t.Fatalf("ScenarioA verdict = %s, want INCONCLUSIVE\nfindings: %+v\ngaps: %+v", v.Status, v.Findings, v.Gaps)
	}
	if v.EvidenceDigest == "" {
		t.Fatal("usable verdict has no evidence digest")
	}
	assertScenarioVector(t, v)

	// v1 compatibility: the attestation is additive, so removing it yields a
	// still-valid document whose terminal claims are explicitly unestablished.
	v1 := doc
	v1.Evidence.Terminal = nil
	v1v := verify.Verify(v1)
	if v1v.Status != verify.StatusInconclusive {
		t.Fatalf("v1 verdict = %s, want INCONCLUSIVE", v1v.Status)
	}
	assertV1Vector(t, v1v)
}

// TestAllM2ScenariosBridgeToPass cross-validates the checks against every
// recorded M2 scenario A-J. It reuses the recorded scenario inputs but never the
// reference's transition, validation or metric mechanics: the verifier only sees
// the serialized document.
func TestAllM2ScenariosBridgeToPass(t *testing.T) {
	scenarios := sim.AllScenarios()
	if len(scenarios) != 10 {
		t.Fatalf("recorded scenarios = %d, want 10 (A-J)", len(scenarios))
	}
	for _, exp := range scenarios {
		t.Run(exp.Name, func(t *testing.T) {
			res := sim.RunExperiment(exp)
			doc, err := evidence.FromRun(res)
			if err != nil {
				t.Fatalf("FromRun: %v", err)
			}
			if doc.Evidence.Terminal == nil {
				t.Fatal("completed run carries no terminal attestation")
			}
			if got := verify.TerminalRecordsDigest(doc.Evidence.Records); got != doc.Evidence.Terminal.RecordsDigest {
				t.Fatalf("digest disagreement: verifier %q recorder %q", got, doc.Evidence.Terminal.RecordsDigest)
			}
			v := verify.Verify(doc)
			if v.Status != verify.StatusInconclusive {
				t.Fatalf("verdict = %s, want INCONCLUSIVE\nfindings: %+v\ngaps: %+v", v.Status, v.Findings, v.Gaps)
			}
			assertScenarioVector(t, v)
		})
	}
}

// TestBridgeIsDeterministic checks that map-derived slices are materialized in a
// stable order: two conversions of the same run produce identical evidence bytes.
func TestBridgeIsDeterministic(t *testing.T) {
	res := sim.RunExperiment(sim.ScenarioA())
	a, err := evidence.FromRun(res)
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	b, err := evidence.FromRun(res)
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	ab, err := verify.CanonicalJSON(a.Evidence)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	bb, err := verify.CanonicalJSON(b.Evidence)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	if !bytes.Equal(ab, bb) {
		t.Fatal("two conversions of one run produced different evidence")
	}
	va := verify.Verify(a)
	vb := verify.Verify(b)
	if va.Status != vb.Status || va.EvidenceDigest != vb.EvidenceDigest {
		t.Fatalf("verdicts differ: %s/%s vs %s/%s", va.Status, va.EvidenceDigest, vb.Status, vb.EvidenceDigest)
	}
}

// TestBridgeRoundTripPreservesEvidence ensures serialization is lossless and the
// digest is stable across a JSON round trip.
func TestBridgeRoundTripPreservesEvidence(t *testing.T) {
	doc, err := evidence.FromExperiment(sim.ScenarioB())
	if err != nil {
		t.Fatalf("FromExperiment: %v", err)
	}
	raw, err := verify.CanonicalJSON(doc)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	var decoded verify.Document
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode document: %v", err)
	}

	first := verify.Verify(doc)
	second := verify.Verify(decoded)
	if first.Status != second.Status || first.EvidenceDigest != second.EvidenceDigest {
		t.Fatalf("round trip changed verdict: %s/%s vs %s/%s", first.Status, first.EvidenceDigest, second.Status, second.EvidenceDigest)
	}
	if len(decoded.Evidence.Records) != len(doc.Evidence.Records) {
		t.Fatalf("round trip lost records: %d vs %d", len(decoded.Evidence.Records), len(doc.Evidence.Records))
	}
	if decoded.Evidence.Snapshot == nil || len(decoded.Evidence.Snapshot.Attempts) != len(doc.Evidence.Snapshot.Attempts) {
		t.Fatal("round trip lost snapshot content")
	}
}

// TestBridgePreservesIdentifiersAndTimestamps spot-checks one entity end to end.
func TestBridgePreservesIdentifiersAndTimestamps(t *testing.T) {
	res := sim.RunExperiment(sim.ScenarioA())
	doc, err := evidence.FromRun(res)
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	var reqID string
	for id := range res.FinalState.Requests {
		reqID = string(id)
		break
	}
	r := res.FinalState.Requests[domain.RequestID(reqID)]
	var fact *verify.RequestFact
	for i := range doc.Evidence.Snapshot.Requests {
		if doc.Evidence.Snapshot.Requests[i].ID == reqID {
			fact = &doc.Evidence.Snapshot.Requests[i]
			break
		}
	}
	if fact == nil {
		t.Fatalf("request %s missing from snapshot", reqID)
	}
	if fact.Tenant != string(r.Tenant) || fact.Upstream != string(r.Upstream) ||
		fact.ArrivedAtNS != int64(r.ArrivedAt) || fact.DeadlineNS != int64(r.Deadline) ||
		fact.State != r.State.String() {
		t.Fatalf("request fact not preserved: %+v vs %+v", fact, r)
	}
}

// TestBridgeReportsAbsentLedger covers a malformed/incomplete reference result:
// an absent ledger is a serialization failure, never an empty ledger.
func TestBridgeReportsAbsentLedger(t *testing.T) {
	res := sim.RunExperiment(sim.ScenarioA())
	res.Records = nil
	if _, err := evidence.FromRun(res); err == nil {
		t.Fatal("expected a serialization failure for an absent ledger")
	} else if !strings.Contains(err.Error(), "absent") {
		t.Fatalf("error should name the absent ledger: %v", err)
	}
}

// TestBridgeDoesNotRepairIncompleteResult covers the missing-snapshot path: the
// bridge converts what exists and the verifier reports the gaps; nothing is
// invented and nothing is dropped silently.
func TestBridgeDoesNotRepairIncompleteResult(t *testing.T) {
	res := sim.RunExperiment(sim.ScenarioA())
	res.FinalState = nil
	doc, err := evidence.FromRun(res)
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	if doc.Evidence.Snapshot != nil {
		t.Fatal("bridge invented a snapshot")
	}
	v := verify.Verify(doc)
	if v.Status != verify.StatusInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE", v.Status)
	}
	if len(v.Gaps) == 0 {
		t.Fatal("missing snapshot produced no evidence gap")
	}
}

// TestBridgeZeroResultIsNotSilentlyValid shows that an entirely empty result is
// carried through unchanged and rejected by verification, not repaired.
func TestBridgeZeroResultIsNotSilentlyValid(t *testing.T) {
	doc, err := evidence.FromRun(sim.RunResult{})
	if err != nil {
		t.Fatalf("FromRun: %v", err)
	}
	if doc.Evidence.Snapshot != nil || len(doc.Evidence.Records) != 0 {
		t.Fatalf("zero result invented evidence: %+v", doc.Evidence)
	}
	v := verify.Verify(doc)
	if v.Status != verify.StatusInvalidInput {
		t.Fatalf("verdict = %s, want INVALID_INPUT", v.Status)
	}
}

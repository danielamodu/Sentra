package verify

import (
	"strings"
	"testing"
)

// attachTerminal adds a well-formed attestation for the full run represented by
// ev. It is the same shape the recorder emits: final effect_seq, the run
// horizon as the terminal instant, the full record count and the digest of the
// full sequence.
func attachTerminal(ev Evidence, terminalTimeNS int64) Evidence {
	last := ev.Records[len(ev.Records)-1]
	ev.Terminal = &TerminalAttestation{
		SchemaVersion: TerminalSchemaVersion,
		EffectSeq:     last.EffectSeq,
		SimTimeNS:     terminalTimeNS,
		RecordCount:   len(ev.Records),
		RecordsDigest: TerminalRecordsDigest(ev.Records),
	}
	return ev
}

// TestTerminalAttestationProvesTail proves the positive case: an attestation
// that matches the retained sequence establishes the terminal boundary, and a
// head-complete ledger becomes HistoryComplete.
func TestTerminalAttestationProvesTail(t *testing.T) {
	ev := attachTerminal(validLifecycleEvidence(), 60_000_000_000)
	led, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("attested evidence rejected: %+v", fs)
	}
	if !led.TerminalProven {
		t.Fatal("valid attestation did not prove the tail")
	}
	if led.Coverage != HistoryComplete {
		t.Fatalf("coverage = %q, want %q", led.Coverage, HistoryComplete)
	}
	if !led.headComplete() || !led.terminalComplete() {
		t.Fatalf("both boundaries must be proven: head=%t terminal=%t", led.headComplete(), led.terminalComplete())
	}
	if TerminalRecordsDigest(ev.Records) != ev.Terminal.RecordsDigest {
		t.Fatal("digest is not stable")
	}
}

// TestTerminalAbsentIsV1Unestablished proves the compatibility rule: evidence
// without an attestation is still valid, and its tail is never inferred.
func TestTerminalAbsentIsV1Unestablished(t *testing.T) {
	led, fs := ValidateEvidence(validLifecycleEvidence())
	if len(fs) != 0 {
		t.Fatalf("v1 evidence rejected: %+v", fs)
	}
	if led.TerminalProven {
		t.Fatal("v1 evidence must not prove a terminal boundary")
	}
	if led.Coverage != HistoryTerminalUnestablished {
		t.Fatalf("coverage = %q, want %q", led.Coverage, HistoryTerminalUnestablished)
	}
	if led.terminalComplete() {
		t.Fatal("v1 evidence satisfied terminal completeness")
	}
}

// TestTerminalHeadTruncationRemainsDetectable proves the central anti-masquerade
// rule: a valid tail attestation does not make a head-truncated ledger complete.
func TestTerminalHeadTruncationRemainsDetectable(t *testing.T) {
	full := validLifecycleEvidence()
	truncated := headTruncatedLifecycleEvidence()
	truncated.Terminal = &TerminalAttestation{
		SchemaVersion: TerminalSchemaVersion,
		EffectSeq:     full.Records[len(full.Records)-1].EffectSeq,
		SimTimeNS:     60_000_000_000,
		RecordCount:   len(full.Records),
		// The verifier cannot recompute a full-run digest from a truncated
		// window; a well-formed value is accepted and never used as head proof.
		RecordsDigest: TerminalRecordsDigest(full.Records),
	}
	led, fs := ValidateEvidence(truncated)
	if len(fs) != 0 {
		t.Fatalf("head-truncated attested evidence rejected: %+v", fs)
	}
	if !led.TerminalProven {
		t.Fatal("retained terminal record did not prove the tail")
	}
	if led.Coverage != HistoryHeadTruncated {
		t.Fatalf("coverage = %q, want head-truncated even with a proven tail", led.Coverage)
	}
	if led.headComplete() {
		t.Fatal("a terminal marker made a head-truncated ledger head-complete")
	}

	ctx := newEvalContext(&led, &World{Partial: led.Partial, Coverage: led.Coverage, TerminalProven: led.TerminalProven, Records: led.Records}, validSnapshot(), validInput())
	// A full-history claim still cannot be established.
	if r := evalProperty(t, "X-tenant-active", ctx); r.Status != StatusInconclusive {
		t.Fatalf("full-history property on head-truncated evidence = %s, want INCONCLUSIVE", r.Status)
	}
	// A terminal-only claim can be.
	if r := evalProperty(t, "X-budget-consumption", ctx); r.Status != StatusPass {
		t.Fatalf("terminal-only property with proven tail = %s (%+v %+v), want PASS", r.Status, r.Findings, r.Gaps)
	}
}

// TestTerminalContradictionsAreInvalid pins every way an attestation can
// contradict the retained evidence. None of these may be downgraded to a gap:
// the document cannot describe one run.
func TestTerminalContradictionsAreInvalid(t *testing.T) {
	base := attachTerminal(validLifecycleEvidence(), 60_000_000_000)
	base.Snapshot = validSnapshot()

	cases := []struct {
		name string
		edit func(*Evidence)
		code string
	}{
		{"schema version", func(ev *Evidence) { ev.Terminal.SchemaVersion = 2 }, CodeTerminalSchema},
		{"record count below retained", func(ev *Evidence) { ev.Terminal.RecordCount = len(ev.Records) - 1 }, CodeTerminalCount},
		{"effect seq before window", func(ev *Evidence) { ev.Terminal.EffectSeq = 0; ev.FirstEffectSeq = 4 }, CodeTerminalRange},
		{"records after boundary", func(ev *Evidence) { ev.Terminal.EffectSeq-- }, CodeTerminalRecordsAfter},
		{"terminal time before last record", func(ev *Evidence) { ev.Terminal.SimTimeNS = 499 }, CodeTerminalTime},
		{"digest mismatch", func(ev *Evidence) { ev.Terminal.RecordsDigest = TerminalRecordsDigest(ev.Records[:1]) }, CodeTerminalDigest},
		{"malformed digest", func(ev *Evidence) { ev.Terminal.RecordsDigest = "sha256:deadbeef" }, CodeTerminalDigest},
		{"snapshot instant mismatch", func(ev *Evidence) { ev.Terminal.SimTimeNS = 60_000_000_001 }, CodeTerminalSnapshotInstant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := base
			ev.Records = append([]Record(nil), base.Records...)
			t2 := *base.Terminal
			ev.Terminal = &t2
			tc.edit(&ev)
			_, fs := ValidateEvidence(ev)
			if !hasCode(fs, tc.code) {
				t.Fatalf("findings %+v do not contain %s", fs, tc.code)
			}
		})
	}
}

// TestTerminalShortfallIsNotAContradiction proves the asymmetry: fewer retained
// records than attested is legal evidence whose tail is simply unproven.
func TestTerminalShortfallIsNotAContradiction(t *testing.T) {
	ev := attachTerminal(validLifecycleEvidence(), 60_000_000_000)
	ev.Terminal.RecordCount++ // the run wrote one more record than retained
	led, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("shortfall treated as invalid: %+v", fs)
	}
	if led.TerminalProven {
		t.Fatal("a shortfall proved the tail")
	}
	if led.Coverage != HistoryTerminalUnestablished {
		t.Fatalf("coverage = %q, want terminal-unestablished", led.Coverage)
	}
}

// TestTerminalMetadataIsCoveredByEvidenceDigest proves the attestation is part
// of canonical evidence integrity, not an out-of-band hint.
func TestTerminalMetadataIsCoveredByEvidenceDigest(t *testing.T) {
	ev := attachTerminal(validLifecycleEvidence(), 60_000_000_000)
	before, err := EvidenceDigest(ev)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	ev.Terminal.SimTimeNS++
	after, err := EvidenceDigest(ev)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if before == after {
		t.Fatal("evidence digest does not cover terminal metadata")
	}
	if !strings.HasPrefix(before, "sha256:") {
		t.Fatalf("unexpected digest form %q", before)
	}
}

package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

// Terminal validation codes. They are stable, machine-readable identifiers.
const (
	CodeTerminalSchema          = "terminal.schema_version"
	CodeTerminalRange           = "terminal.range"
	CodeTerminalCount           = "terminal.record_count"
	CodeTerminalRecordsAfter    = "terminal.records_after_boundary"
	CodeTerminalTime            = "terminal.time_boundary"
	CodeTerminalDigest          = "terminal.records_digest"
	CodeTerminalSnapshotInstant = "terminal.snapshot_instant"
)

// TerminalRecordsDigest is the canonical fingerprint of a complete record
// sequence. The reference recorder computes the identical value over every
// record it writes (including records a bounded sink later evicts), so the
// verifier can bind the attested full run to the records it actually holds.
//
// The encoding is explicit and length-prefixed rather than JSON: the two
// packages have independent record types, and the digest must not depend on
// struct field order or optional-field omission rules. Each field is encoded as
// "<byte-length>:<bytes>" and each record is terminated by a 0x1e separator.
func TerminalRecordsDigest(records []Record) string {
	h := sha256.New()
	for i := range records {
		r := &records[i]
		fields := []string{
			strconv.Itoa(r.SchemaVersion),
			strconv.FormatUint(r.EffectSeq, 10),
			strconv.FormatUint(r.EngineSeq, 10),
			strconv.FormatInt(r.SimTimeNS, 10),
			r.Category,
			r.Kind,
			r.Subject,
			r.Detail.Request,
			r.Detail.Attempt,
			r.Detail.Tenant,
			r.Detail.Upstream,
			r.Detail.Worker,
			r.Detail.From,
			r.Detail.To,
			r.Detail.Reason,
			r.Detail.Lane,
			r.Detail.Description,
			strconv.FormatInt(r.Detail.DeadlineNS, 10),
			r.Detail.Decision,
			r.Detail.Predecessor,
			r.Detail.Outcome,
			strconv.Itoa(r.Detail.Count),
			strconv.FormatInt(r.Detail.LatencyNS, 10),
			strconv.FormatUint(r.Detail.Seq, 10),
			digestIntField(r.Detail.MaxRetries),
		}
		for _, f := range fields {
			fmt.Fprintf(h, "%d:%s", len(f), f)
		}
		h.Write([]byte{0x1e})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// digestIntField encodes an optional integer detail field. Absence has its own
// token so a missing declaration can never be hashed as a declared zero.
func digestIntField(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

// validDigestFormat reports whether s is a lowercase sha256 digest. The format
// is checked even when the digest cannot be recomputed (head-truncated
// evidence), so a placeholder can never pass as an attestation.
func validDigestFormat(s string) bool {
	const prefix = "sha256:"
	if len(s) != len(prefix)+64 || s[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validateTerminal validates the optional terminal attestation against the
// retained evidence and sets led.TerminalProven when the tail is proven.
//
// The rules are deliberately asymmetric between the head and the tail:
//
//   - A contradiction between the attestation and retained records is
//     INVALID_INPUT (the document cannot describe one run).
//   - Missing retained records are an evidence shortfall, not a contradiction:
//     the tail stays unproven and the property that needs it is INCONCLUSIVE.
//   - A valid terminal attestation never proves the head. FirstEffectSeq > 0
//     keeps the ledger head-truncated, so a terminal marker cannot make a
//     head-truncated ledger masquerade as full history.
func validateTerminal(ev Evidence, led *Ledger, add func(code, format string, args ...any)) {
	t := ev.Terminal
	if t == nil {
		return
	}
	if t.SchemaVersion != TerminalSchemaVersion {
		add(CodeTerminalSchema, "terminal attestation schema version %d is not %d", t.SchemaVersion, TerminalSchemaVersion)
		return
	}
	if t.RecordCount <= 0 {
		add(CodeTerminalCount, "terminal attestation declares %d records; a completed run writes at least one", t.RecordCount)
		return
	}
	if t.SimTimeNS < 0 {
		add(CodeTerminalTime, "terminal attestation declares negative simulated time %d", t.SimTimeNS)
		return
	}
	if t.EffectSeq < ev.FirstEffectSeq {
		add(CodeTerminalRange, "terminal effect_seq %d precedes the retained window's first effect_seq %d", t.EffectSeq, ev.FirstEffectSeq)
		return
	}
	if !validDigestFormat(t.RecordsDigest) {
		add(CodeTerminalDigest, "terminal records digest %q is not a sha256 digest", t.RecordsDigest)
		return
	}
	if len(ev.Records) > t.RecordCount {
		add(CodeTerminalCount, "evidence retains %d records but the attestation says the run wrote only %d", len(ev.Records), t.RecordCount)
		return
	}

	last := ev.Records[len(ev.Records)-1]
	if last.EffectSeq > t.EffectSeq {
		add(CodeTerminalRecordsAfter, "evidence retains effect_seq %d after the attested terminal boundary %d", last.EffectSeq, t.EffectSeq)
		return
	}
	if last.SimTimeNS > t.SimTimeNS {
		add(CodeTerminalTime, "retained effect_seq %d runs at %dns, after the attested terminal instant %dns", last.EffectSeq, last.SimTimeNS, t.SimTimeNS)
		return
	}
	if ev.Snapshot != nil && ev.Snapshot.TakenAtNS > 0 && ev.Snapshot.TakenAtNS != t.SimTimeNS {
		add(CodeTerminalSnapshotInstant, "terminal instant %dns disagrees with the snapshot's observation instant %dns", t.SimTimeNS, ev.Snapshot.TakenAtNS)
		return
	}

	if ev.FirstEffectSeq == 0 {
		// The head is complete, so the retained sequence must be exactly the
		// attested one. A shorter retained sequence means the tail was evicted:
		// legal evidence, but the terminal boundary is not proven.
		if len(ev.Records) != t.RecordCount {
			return
		}
		if last.EffectSeq != t.EffectSeq {
			add(CodeTerminalRange, "the retained tail ends at effect_seq %d but the attestation names %d", last.EffectSeq, t.EffectSeq)
			return
		}
		if got := TerminalRecordsDigest(ev.Records); got != t.RecordsDigest {
			add(CodeTerminalDigest, "records digest %q does not match the attested %q", got, t.RecordsDigest)
			return
		}
		led.TerminalProven = true
		return
	}

	// Head-truncated evidence cannot recompute the full-run digest. The tail is
	// proven only when the attested terminal record itself is retained; the
	// head remains truncated independently of this result.
	if last.EffectSeq == t.EffectSeq {
		led.TerminalProven = true
	}
}

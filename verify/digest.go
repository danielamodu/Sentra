package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// CanonicalJSON returns the canonical serialization of v.
//
// Canonical form (D2, docs/VERIFIER_DESIGN.md §3):
//
//   - UTF-8 JSON, no insignificant whitespace, no trailing newline;
//   - struct fields in declaration order, never map iteration order (the
//     evidence model contains no maps, so serialization is total);
//   - HTML escaping disabled (`<`, `>`, `&` stay literal), standard Go string
//     escaping otherwise;
//   - empty optional fields omitted exactly as declared by their json tags.
//
// Two evidence values with equal content therefore serialize to identical bytes
// regardless of how they were produced.
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("verify: canonical encode failed: %w", err)
	}
	b := buf.Bytes()
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	return b, nil
}

// EvidenceDigest is the stable identity of the evidence a verdict is based on.
//
// Digest fields (exactly the Evidence struct, canonical JSON):
// schema_version, first_effect_seq; for every record: schema_version,
// effect_seq, engine_seq, sim_time_ns, category, kind, subject, detail.{request,
// attempt, tenant, upstream, worker, from, to, reason, lane, description,
// deadline_ns, decision, predecessor, outcome, count, latency_ns, seq,
// max_retries}; when present,
// the terminal attestation with schema_version, effect_seq, sim_time_ns,
// record_count and records_digest; and, when present, snapshot with taken_at_ns
// plus every request, attempt, worker pool, connection pool, queue entry, pending
// admission, tenant counter, outcome counter, retry budget, consumed-token row
// and breaker fact.
//
// The digest does not cover the input contract: it is a declaration to check
// against observations, not an observation to fingerprint.
func EvidenceDigest(ev Evidence) (string, error) {
	b, err := CanonicalJSON(ev)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

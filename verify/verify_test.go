package verify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func validDocument() Document {
	ev := evidenceFrom(
		testBlock{engine: 1, timeNS: 100, event: "RequestArrival", notes: []string{"request-created", "queue"}},
		testBlock{engine: 2, timeNS: 200, event: "ExecutionCompletion", notes: []string{"outcome", "worker"}},
	)
	ev.Records[0].Detail = Detail{Request: "R-001", Tenant: "T1", Upstream: "U1"}
	return Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "A-run-1",
		Input:    validInput(),
		Evidence: ev,
	}
}

func TestVerifyPipelineStages(t *testing.T) {
	t.Run("valid document without observations is inconclusive", func(t *testing.T) {
		v := Verify(validDocument())
		if v.Status != StatusInconclusive {
			t.Fatalf("status = %s, want %s (no lifecycle was observed and no snapshot is present)", v.Status, StatusInconclusive)
		}
		if len(v.Gaps) == 0 {
			t.Fatal("inconclusive verdict must document its evidence gap")
		}
		byID := resultsByID(v.Properties)
		for _, id := range []string{"L-request-lifecycle", "L-attempt-lifecycle"} {
			if p := byID[id]; p == nil || p.Status != StatusInconclusive || len(p.Gaps) == 0 {
				t.Fatalf("%s = %+v, want INCONCLUSIVE with a gap", id, p)
			}
		}
		if !strings.HasPrefix(v.EvidenceDigest, "sha256:") || len(v.EvidenceDigest) != len("sha256:")+64 {
			t.Fatalf("evidence digest %q is not a sha256 digest", v.EvidenceDigest)
		}
	})

	t.Run("unknown document schema is invalid input", func(t *testing.T) {
		doc := validDocument()
		doc.Schema = "sentra-verify/v0"
		v := Verify(doc)
		if v.Status != StatusInvalidInput || !hasCode(v.Findings, CodeDocumentSchema) {
			t.Fatalf("verdict = %+v, want INVALID_INPUT with %s", v, CodeDocumentSchema)
		}
	})

	t.Run("missing input contract is invalid input", func(t *testing.T) {
		doc := validDocument()
		doc.Input = nil
		v := Verify(doc)
		if v.Status != StatusInvalidInput || !hasCode(v.Findings, CodeInputMissing) {
			t.Fatalf("verdict = %+v, want INVALID_INPUT with %s", v, CodeInputMissing)
		}
	})

	t.Run("corrupt evidence is invalid input", func(t *testing.T) {
		doc := validDocument()
		doc.Evidence.Records[1].EffectSeq = 99
		v := Verify(doc)
		if v.Status != StatusInvalidInput || !hasCode(v.Findings, CodeEffectSeqMismatch) {
			t.Fatalf("verdict = %+v, want INVALID_INPUT with %s", v, CodeEffectSeqMismatch)
		}
		if v.EvidenceDigest != "" {
			t.Fatal("invalid evidence must not receive an evidence digest")
		}
	})

	t.Run("malformed JSON is invalid input not an error", func(t *testing.T) {
		v := VerifyJSON([]byte("{not json"))
		if v.Status != StatusInvalidInput || !hasCode(v.Findings, "document.decode") {
			t.Fatalf("verdict = %+v, want INVALID_INPUT decode finding", v)
		}
	})
}

func TestVerifyIsDeterministicAndSerializable(t *testing.T) {
	doc := validDocument()
	first := Verify(doc)
	second := Verify(doc)

	a, err := CanonicalJSON(first)
	if err != nil {
		t.Fatalf("canonical verdict encode: %v", err)
	}
	b, err := CanonicalJSON(second)
	if err != nil {
		t.Fatalf("canonical verdict encode: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("identical documents produced different verdicts:\n%s\n%s", a, b)
	}

	var decoded Verdict
	if err := json.Unmarshal(a, &decoded); err != nil {
		t.Fatalf("verdict is not valid JSON: %v", err)
	}
	if decoded.Status != StatusInconclusive || decoded.EvidenceDigest != first.EvidenceDigest {
		t.Fatalf("round-tripped verdict lost content: %+v", decoded)
	}
}

func TestEvidenceDigestContract(t *testing.T) {
	doc := validDocument()
	base, err := EvidenceDigest(doc.Evidence)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	again, err := EvidenceDigest(doc.Evidence)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if base != again {
		t.Fatalf("digest is not deterministic: %s vs %s", base, again)
	}

	t.Run("round trip preserves digest", func(t *testing.T) {
		raw, err := CanonicalJSON(doc.Evidence)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		var ev Evidence
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		rt, err := EvidenceDigest(ev)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		if rt != base {
			t.Fatalf("digest changed across round trip: %s vs %s", rt, base)
		}
	})

	t.Run("diagnostic prose is covered", func(t *testing.T) {
		changed := doc.Evidence
		changed.Records = append([]Record(nil), doc.Evidence.Records...)
		changed.Records[0].Detail.Description = "different diagnostic text"
		d, err := EvidenceDigest(changed)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		if d == base {
			t.Fatal("digest ignored a record field it is defined to cover")
		}
	})

	t.Run("input and scenario are not evidence", func(t *testing.T) {
		other := validDocument()
		other.Scenario = "B"
		other.Input.WorkerPool.Size = 9
		d, err := EvidenceDigest(other.Evidence)
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		if d != base {
			t.Fatal("evidence digest must not cover the input contract")
		}
	})
}

func TestCanonicalJSONForm(t *testing.T) {
	ev := evidenceFrom(testBlock{engine: 1, timeNS: 100, event: "RequestArrival"})
	ev.Records[0].Detail = Detail{Description: "a < b & c"}

	b, err := CanonicalJSON(ev)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Fatal("canonical JSON must not carry a trailing newline")
	}
	if !bytes.Contains(b, []byte("a < b & c")) {
		t.Fatalf("canonical JSON must not HTML-escape: %s", b)
	}
	other, err := CanonicalJSON(ev)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	if !bytes.Equal(b, other) {
		t.Fatal("canonical encoding is not stable")
	}
}

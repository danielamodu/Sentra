package verify

import "encoding/json"

// Verify runs the verification pipeline on one document.
//
// Milestone 3 is built in phases. This entry point performs:
//
//	stages 1-3  document, input and evidence validation       -> INVALID_INPUT
//	stage 3b    snapshot structural validation (when present) -> INVALID_INPUT
//	stage 4     independent reconstruction (L-path)           -> INVALID_INPUT on
//	                                                             uninterpretable
//	                                                             contradictions
//	stage 5     invariant evaluation (L/S checks)             -> PASS/FAIL/INCONCLUSIVE
//	stage 6     cross-path consistency (X checks)             -> PASS/FAIL/INCONCLUSIVE
//	stage 7     aggregate verdict + evidence digest
//
// Workload and fault schedules carried in the input contract are validated for
// declaration consistency only (stage 2). They are not verified against observed
// behaviour; no property reads them yet.
func Verify(doc Document) Verdict {
	v := Verdict{
		Schema:   SchemaDocument,
		Scenario: doc.Scenario,
		RunID:    doc.RunID,
	}

	// Stage 1: document envelope.
	if fs := ValidateDocument(doc); len(fs) > 0 {
		v.Status = StatusInvalidInput
		v.Findings = fs
		return v
	}

	// Stage 2: declared configuration contract.
	if fs := ValidateInput(doc.Input); len(fs) > 0 {
		v.Status = StatusInvalidInput
		v.Findings = fs
		return v
	}

	// Stage 3: evidence structure, ordering and correlation.
	led, fs := ValidateEvidence(doc.Evidence)
	if len(fs) > 0 {
		v.Status = StatusInvalidInput
		v.Findings = fs
		return v
	}
	v.Partial = led.Partial

	// Stage 3b: snapshot structure. A snapshot that does not describe the run
	// the input declares is not usable evidence; it is not a failed property.
	if doc.Evidence.Snapshot != nil {
		if sfs := validateSnapshot(doc.Evidence.Snapshot, doc.Input); len(sfs) > 0 {
			v.Status = StatusInvalidInput
			v.Findings = sfs
			return v
		}
	}

	// Stage 4: independent reconstruction. Fatal findings mean the ledger
	// cannot be interpreted against the documented vocabulary at all.
	world := reconstruct(&led)
	if len(world.Fatal) > 0 {
		v.Status = StatusInvalidInput
		v.Findings = world.Fatal
		return v
	}

	// Evidence that passed every structural gate has a stable identity.
	digest, err := EvidenceDigest(doc.Evidence)
	if err != nil {
		v.Status = StatusInvalidInput
		v.Findings = []Finding{{Code: "evidence.digest", Message: err.Error()}}
		return v
	}
	v.EvidenceDigest = digest

	// Stages 5-6: property evaluation and cross-path consistency, driven by the
	// single ordered registry. The registry executor enforces each property's
	// declared evidence policy; the checks themselves never decide to skip.
	ctx := newEvalContext(&led, world, doc.Evidence.Snapshot, doc.Input)
	eval := evaluateAll(ctx)
	if len(eval.Defects) > 0 {
		// A registry contract defect is a verifier bug, not an evidence defect
		// and not a failed property. No property claim can be made.
		v.Status = StatusInvalidInput
		v.Findings = eval.Defects
		return v
	}
	v.Properties = eval.Results

	// Stage 7: aggregate verdict. Findings and gaps are mirrored at the top
	// level so a caller can act on them without walking every property; the
	// per-property attribution remains the authoritative record.
	v.Status = Aggregate(eval.Results)
	for _, r := range eval.Results {
		v.Findings = append(v.Findings, r.Findings...)
		v.Gaps = append(v.Gaps, r.Gaps...)
	}
	return v
}

// VerifyJSON decodes a serialized document and verifies it. A decode failure is
// INVALID_INPUT, never an error that could be mistaken for a failed property.
func VerifyJSON(data []byte) Verdict {
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return Verdict{
			Schema: SchemaDocument,
			Status: StatusInvalidInput,
			Findings: []Finding{{
				Code:    "document.decode",
				Message: "document is not valid JSON: " + err.Error(),
			}},
		}
	}
	return Verify(doc)
}

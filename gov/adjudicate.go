package gov

import (
	"fmt"
	"time"

	"sentra/ops"
	"sentra/verify"
)

// Adjudicate evaluates an operational proposal against the independent verifier's verdict,
// evidence document, and the governance policy. Fails closed on any inconsistency.
func Adjudicate(prop *ops.Proposal, verdict *verify.Verdict, doc *verify.Document, pol Policy) (*AdjudicationResult, error) {
	if prop == nil {
		return nil, fmt.Errorf("adjudication failed: proposal is nil")
	}
	if verdict == nil {
		return nil, fmt.Errorf("adjudication failed: verifier verdict is nil")
	}
	if doc == nil {
		return nil, fmt.Errorf("adjudication failed: evidence document is nil")
	}

	res := &AdjudicationResult{
		Eligible:           true,
		VerificationStatus: string(verdict.Status),
		AdjudicatedAt:      time.Now().UTC(),
	}

	// 1. Overall verifier status gate: FAIL and INVALID_INPUT are strictly non-promotable
	if verdict.Status == verify.StatusFail {
		res.Eligible = false
		res.RejectionReasons = append(res.RejectionReasons,
			fmt.Sprintf("verifier returned FAIL with %d findings", len(verdict.Findings)))
	}
	if verdict.Status == verify.StatusInvalidInput {
		res.Eligible = false
		res.RejectionReasons = append(res.RejectionReasons,
			fmt.Sprintf("verifier returned INVALID_INPUT with %d findings", len(verdict.Findings)))
	}

	// 2. Evidence completeness checks
	if pol.RequireHeadCompleteness && doc.Evidence.FirstEffectSeq > 0 {
		res.Eligible = false
		gap := fmt.Sprintf("head-truncated evidence: first effect sequence is %d (must be 0)", doc.Evidence.FirstEffectSeq)
		res.MissingEvidenceGaps = append(res.MissingEvidenceGaps, gap)
		res.RejectionReasons = append(res.RejectionReasons, gap)
	}

	if pol.RequireTerminalCompleteness && doc.Evidence.Terminal == nil {
		res.Eligible = false
		gap := "terminal boundary attestation is absent; run is terminal-unestablished"
		res.MissingEvidenceGaps = append(res.MissingEvidenceGaps, gap)
		res.RejectionReasons = append(res.RejectionReasons, gap)
	}

	// 3. Mandatory safety properties evaluation
	propMap := make(map[string]verify.PropertyResult)
	for _, p := range verdict.Properties {
		propMap[p.ID] = p
	}

	for _, reqProp := range pol.RequiredProperties {
		pr, exists := propMap[reqProp]
		if !exists {
			res.Eligible = false
			reason := fmt.Sprintf("required safety property %s was not evaluated in verifier run", reqProp)
			res.FailedSafetyProperties = append(res.FailedSafetyProperties, reqProp)
			res.RejectionReasons = append(res.RejectionReasons, reason)
			continue
		}

		if pr.Status != verify.StatusPass {
			res.Eligible = false
			reason := fmt.Sprintf("required safety property %s evaluated to %s (want PASS)", reqProp, pr.Status)
			res.FailedSafetyProperties = append(res.FailedSafetyProperties, reqProp)
			res.RejectionReasons = append(res.RejectionReasons, reason)
		} else {
			res.PassedSafetyProperties = append(res.PassedSafetyProperties, reqProp)
		}
	}

	// 4. Governance policy validation
	if err := pol.ValidateProposalPolicy(prop); err != nil {
		res.Eligible = false
		res.RejectionReasons = append(res.RejectionReasons, fmt.Sprintf("policy check failed: %v", err))
	}

	return res, nil
}

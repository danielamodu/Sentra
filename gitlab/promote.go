package gitlab

import (
	"fmt"
	"time"

	"sentra/evidence"
	"sentra/gov"
	"sentra/ops"
	"sentra/sim"
	"sentra/verify"
)

// ExecutePromotion executes a controlled, explicitly authorized configuration promotion.
// Validates all pre-conditions: approval record validity, cryptographic signatures,
// digest continuity, policy compatibility, applies changes, and executes post-promotion validation.
func ExecutePromotion(
	prop *ops.Proposal,
	apprRec *gov.ApprovalRecord,
	secretKey []byte,
	activeConfig *sim.SystemConfig,
	testExp sim.Experiment,
	pol gov.Policy,
	audit *gov.AuditLog,
) (*PromotionReport, error) {
	now := time.Now().UTC()
	promID := fmt.Sprintf("prom-%s-%d", prop.ID, now.UnixNano())

	rep := &PromotionReport{
		PromotionID: promID,
		ProposalID:  prop.ID,
		PromotedAt:  now,
		Status:      "FAILED",
	}

	if prop == nil {
		return nil, fmt.Errorf("promotion failed: proposal is nil")
	}
	if apprRec == nil {
		rep.Status = "REJECTED"
		rep.RejectionReason = "missing required human approval record"
		if audit != nil {
			audit.Append(prop.ID, gov.ActionPromotionAttempt, "promotion-controller", "", "rejected: missing approval record")
		}
		return rep, fmt.Errorf("promotion blocked: %s", rep.RejectionReason)
	}

	// 1. Identity match check
	if prop.ID != apprRec.ProposalID {
		rep.Status = "REJECTED"
		rep.RejectionReason = fmt.Sprintf("approval record proposal ID %q does not match proposal %q", apprRec.ProposalID, prop.ID)
		return rep, fmt.Errorf("promotion blocked: %s", rep.RejectionReason)
	}

	// 2. Compute current digests to detect configuration drift
	currentConfigDigest, err := prop.Digest()
	if err != nil {
		return nil, fmt.Errorf("compute proposal digest: %w", err)
	}

	// 3. Verify approval signature and digest match
	if err := gov.VerifyApproval(apprRec, currentConfigDigest, apprRec.VerificationDigest, secretKey); err != nil {
		rep.Status = "REJECTED"
		rep.RejectionReason = fmt.Sprintf("approval record validation failed: %v", err)
		if audit != nil {
			audit.Append(prop.ID, gov.ActionPromotionAttempt, "promotion-controller", currentConfigDigest, rep.RejectionReason)
		}
		return rep, fmt.Errorf("promotion blocked: %w", err)
	}

	// 4. Policy version compatibility check
	if apprRec.PolicyVersion != pol.Version {
		rep.Status = "REJECTED"
		rep.RejectionReason = fmt.Sprintf("approved under policy version %s, but active policy is %s", apprRec.PolicyVersion, pol.Version)
		return rep, fmt.Errorf("promotion blocked: %s", rep.RejectionReason)
	}

	// 5. Apply the approved changes to the active system configuration
	for _, c := range prop.Changes {
		if err := ops.ApplyChangeToConfig(activeConfig, c); err != nil {
			rep.Status = "FAILED"
			rep.RejectionReason = fmt.Sprintf("failed to apply parameter change: %v", err)
			return rep, fmt.Errorf("promotion application failed: %w", err)
		}
	}
	rep.AppliedConfig = *activeConfig
	rep.PromotedBy = apprRec.ApproverID

	// 6. Post-Promotion Validation Simulation
	// Run the newly promoted configuration against the test scenario
	postExp := testExp
	postExp.Name = fmt.Sprintf("%s-post-promotion-validation", testExp.Name)
	postExp.Config = *activeConfig

	postSimRes := sim.RunExperiment(postExp)
	postDoc, err := evidence.FromRun(postSimRes)
	if err != nil {
		rep.Status = "FAILED"
		rep.RejectionReason = fmt.Sprintf("post-promotion evidence serialization failed: %v", err)
		return rep, fmt.Errorf("post-promotion validation failed: %w", err)
	}

	postVerdict := verify.Verify(postDoc)
	rep.PostValidationVerdict = &postVerdict

	// 1. Ensure no invariant failures or structural invalid input
	if postVerdict.Status == verify.StatusFail || postVerdict.Status == verify.StatusInvalidInput {
		rep.Status = "FAILED"
		rep.RejectionReason = fmt.Sprintf("post-promotion verifier returned %s (%d findings)", postVerdict.Status, len(postVerdict.Findings))
		return rep, fmt.Errorf("post-promotion validation failed: %s", rep.RejectionReason)
	}

	// 2. Validate each mandatory safety property under governance policy
	propMap := make(map[string]verify.PropertyResult)
	for _, p := range postVerdict.Properties {
		propMap[p.ID] = p
	}

	for _, reqProp := range pol.RequiredProperties {
		pr, exists := propMap[reqProp]
		if !exists {
			rep.Status = "FAILED"
			rep.RejectionReason = fmt.Sprintf("post-promotion validation did not evaluate required safety property %s", reqProp)
			return rep, fmt.Errorf("post-promotion validation failed: %s", rep.RejectionReason)
		}
		if pr.Status != verify.StatusPass {
			rep.Status = "FAILED"
			rep.RejectionReason = fmt.Sprintf("post-promotion required safety property %s evaluated to %s (want PASS)", reqProp, pr.Status)
			return rep, fmt.Errorf("post-promotion validation failed: %s", rep.RejectionReason)
		}
	}

	// 3. Inspect verification status and account for evidence gaps honestly
	if postVerdict.Status == verify.StatusInconclusive {
		for _, g := range postVerdict.Gaps {
			if g.Property != "H-seeded-replay" {
				rep.Status = "FAILED"
				rep.RejectionReason = fmt.Sprintf("unexpected evidence gap in property %s: %s", g.Property, g.Reason)
				return rep, fmt.Errorf("post-promotion validation failed: %s", rep.RejectionReason)
			}
		}
		rep.PostValidationStatus = fmt.Sprintf("INCONCLUSIVE_EXPECTED_GAP (H-seeded-replay declared limitation; %d/%d required safety properties PASS; 0 findings)", len(pol.RequiredProperties), len(pol.RequiredProperties))
	} else {
		rep.PostValidationStatus = fmt.Sprintf("PASS (%d/%d required safety properties PASS; 0 findings)", len(pol.RequiredProperties), len(pol.RequiredProperties))
	}

	rep.Status = "SUCCESS"

	// 7. Audit Logging
	if audit != nil {
		evt, err := audit.Append(
			prop.ID,
			gov.ActionPromoted,
			apprRec.ApproverID,
			currentConfigDigest,
			fmt.Sprintf("successfully promoted by %s (approval: %s)", apprRec.ApproverID, apprRec.ApprovalID),
		)
		if err == nil {
			rep.AuditTrailHash = evt.EntryHash
		}
	}

	return rep, nil
}

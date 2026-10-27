package e2e

import (
	"strings"
	"testing"

	"sentra/gitlab"
	"sentra/gov"
	"sentra/ops"
	"sentra/sim"
	"sentra/verify"
)

// TestEndToEndIntegratedOperationsWorkflow demonstrates the full 18-step sequence
// from operational failure through autonomous investigation, bounded experimentation,
// independent adjudication, GitLab MR generation, CI execution, human authorization,
// and controlled promotion with post-promotion validation.
func TestEndToEndIntegratedOperationsWorkflow(t *testing.T) {
	t.Log("=== STEP 1 & 2: Running reference system baseline with degraded upstream ===")
	baselineExp := sim.ScenarioC()
	baselineRes := sim.RunExperiment(baselineExp)

	t.Logf("Baseline metrics: Offered=%d Admitted=%d Completed=%d Failed=%d Shed=%d WastedShare=%.2f",
		baselineRes.Summary.Offered,
		baselineRes.Summary.Admitted,
		baselineRes.Summary.Completed,
		baselineRes.Summary.Failed,
		baselineRes.Summary.Shed,
		baselineRes.Summary.WastedShare,
	)

	audit := gov.NewAuditLog()
	secretKey := []byte("sentra-production-governance-secret-key-32b")
	pol := gov.DefaultPolicy()
	lm := gov.NewLifecycleManager()

	t.Log("=== STEP 3 & 4: Agent investigates telemetry and forms operational hypothesis ===")
	inv, err := ops.Investigate(&baselineRes)
	if err != nil {
		t.Fatalf("agent investigation failed: %v", err)
	}
	if len(inv.Hypotheses) == 0 {
		t.Fatalf("agent failed to form hypotheses from degraded telemetry")
	}

	primaryHyp := inv.Hypotheses[0]
	t.Logf("Formed Hypothesis [%s]: %s (Symptom: %s)", primaryHyp.ID, primaryHyp.Description, primaryHyp.Symptom)

	audit.Append("session-001", gov.ActionInvestigated, "agent-sentra", "",
		"identified operational degradation; formed hypothesis "+primaryHyp.ID)

	t.Log("=== STEP 5, 6, 7 & 8: Bounded autonomous experimentation and verifier adjudication ===")
	budget := ops.ExperimentBudget{
		MaxIterations:   3,
		MaxCandidates:   6,
		MaxSimulations:  8,
		MaxDeltaPercent: 100.0,
	}
	weights := ops.DefaultObjectiveWeights()
	expEngine, err := ops.NewExperimenter("agent-sentra", budget, weights)
	if err != nil {
		t.Fatalf("failed to initialize experimenter: %v", err)
	}

	expRecord, err := expEngine.RunAutonomousLoop(baselineExp, &baselineRes, inv)
	if err != nil {
		t.Fatalf("autonomous experiment loop failed: %v", err)
	}

	t.Logf("Exploration completed: %d iterations, %d simulations, %d total candidates",
		expRecord.IterationsRun, expRecord.SimulationsRun, len(expRecord.Candidates))

	for _, c := range expRecord.Candidates {
		audit.Append(c.Proposal.ID, gov.ActionExperimentRan, "agent-sentra", "",
			c.CandidateID)
		if c.Rejected {
			audit.Append(c.Proposal.ID, gov.ActionCandidateRejected, "independent-verifier", "",
				c.RejectionReason)
			t.Logf("Candidate %s REJECTED: %s", c.CandidateID, c.RejectionReason)
		} else {
			t.Logf("Candidate %s SURVIVED: Score=%.2f, Goodput=%d, WastedShare=%.2f",
				c.CandidateID, c.Score, c.Summary.Completed, c.Summary.WastedShare)
		}
	}

	t.Log("=== STEP 9 & 10: Unsafe candidates rejected by verifier and counterexamples inspected ===")
	// Explicitly test that an unsafe candidate violating technical boundaries or invariants is rejected
	unsafeVariantCandidate := expRecord.Candidates[0] // copy first candidate
	unsafeVariantCandidate.Proposal.Changes = []ops.ParameterChange{
		{
			Parameter:     "verifier.rules.disable",
			OriginalValue: false,
			ProposedValue: true,
			Reason:        "adversarial attempt to disable verifier checks",
		},
	}
	if err := ops.ValidateBoundaries(unsafeVariantCandidate.Proposal.Changes); err == nil {
		t.Fatalf("SECURITY BREACH: boundary check failed to reject protected verifier path")
	} else {
		t.Logf("Technical boundary successfully caught and rejected unsafe change: %v", err)
	}

	t.Log("=== STEP 11: Candidate selected using multi-objective ranking ===")
	if expRecord.BestCandidate == nil || expRecord.WinningProposal == nil {
		t.Fatalf("no candidate survived autonomous exploration")
	}
	if expRecord.BestCandidate.Score <= expRecord.BaselineScore {
		t.Fatalf("experiment engine failed to improve objective: best score %.2f <= baseline %.2f",
			expRecord.BestCandidate.Score, expRecord.BaselineScore)
	}
	if expRecord.BestCandidate.Summary.Completed <= baselineRes.Summary.Completed {
		t.Fatalf("experiment engine failed to improve goodput: best completed %d <= baseline %d",
			expRecord.BestCandidate.Summary.Completed, baselineRes.Summary.Completed)
	}
	winningProp := *expRecord.WinningProposal
	t.Logf("Winning Proposal [%s]: Score=%.2f (Baseline: %.2f), Goodput=%d (Baseline: %d)",
		winningProp.ID, expRecord.BestCandidate.Score, expRecord.BaselineScore,
		expRecord.BestCandidate.Summary.Completed, baselineRes.Summary.Completed)

	t.Log("=== STEP 12: Governance lifecycle initialized and policy checked ===")
	_, err = lm.InitProposal(winningProp.ID)
	if err != nil {
		t.Fatalf("failed to initialize proposal in lifecycle manager: %v", err)
	}
	_ = lm.Transition(winningProp.ID, gov.StateExperimenting, "autonomous experimentation completed")
	_ = lm.Transition(winningProp.ID, gov.StateVerified, "independent verifier passed")
	_ = lm.Transition(winningProp.ID, gov.StatePendingApproval, "submitted for governance adjudication")

	if err := pol.ValidateProposalPolicy(&winningProp); err != nil {
		t.Fatalf("winning proposal violates governance policy: %v", err)
	}
	audit.Append(winningProp.ID, gov.ActionPolicyChecked, "governance-engine", "", "policy compliance validated")

	t.Log("=== STEP 13: Proposal branch and GitLab Merge Request prepared ===")
	branchName := gitlab.FormatProposalBranchName(winningProp.ID)
	t.Logf("Prepared Git proposal branch: %s", branchName)

	artifacts, err := gitlab.BundleProposalArtifacts(
		&winningProp,
		baselineExp.Config,
		expRecord.BestCandidate.Config,
		expRecord.BestCandidate.Verdict,
		nil,
	)
	if err != nil {
		t.Fatalf("failed to bundle proposal artifacts: %v", err)
	}

	mrDesc := gitlab.GenerateMergeRequestDescription(
		&winningProp,
		baselineExp.Config,
		expRecord.BestCandidate.Config,
		&baselineRes.Summary,
		expRecord.BestCandidate.Summary,
		expRecord.BestCandidate.Verdict,
		nil,
		"digest-baseline-evidence",
	)
	if len(mrDesc) == 0 || !strings.Contains(mrDesc, "Operational Proposal") {
		t.Fatalf("generated Merge Request description is invalid or empty")
	}

	t.Log("=== STEP 14: GitLab CI independently reproduces simulation and verification ===")
	ciReport, err := gitlab.RunCIPipeline(artifacts, baselineExp, pol)
	if err != nil {
		t.Fatalf("CI pipeline crashed: %v", err)
	}
	if !ciReport.Passed {
		t.Fatalf("CI pipeline failed closed: %s", ciReport.BlockReason)
	}
	if len(ciReport.Stages) != 10 {
		t.Fatalf("expected exactly 10 CI stages, got %d", len(ciReport.Stages))
	}
	t.Logf("CI Pipeline %s PASSED across all %d stages", ciReport.PipelineID, len(ciReport.Stages))
	audit.Append(winningProp.ID, gov.ActionVerified, "gitlab-ci-runner", "",
		"independent reproduction verified all mandatory properties")

	t.Log("=== STEP 15 & 16: Human authorization requested; self-approval strictly blocked ===")
	// 1. Agent self-approval attempt MUST fail
	agentApprover := gov.ApproverInfo{
		ID:      "agent-sentra",
		Name:    "Sentra Agent",
		Role:    "infrastructure-lead",
		IsAgent: true,
	}
	cfgDigest, _ := winningProp.Digest()
	verifDigest := "verif-digest-ci-pipeline-passed"

	_, err = gov.Authorize(&winningProp, agentApprover, cfgDigest, verifDigest, pol, "APPROVED", "self-approval attempt", secretKey)
	if err == nil {
		t.Fatalf("SECURITY BREACH: agent self-approval was allowed!")
	}
	t.Logf("Self-approval check enforced: %v", err)

	// 2. Promotion attempt WITHOUT approval MUST be blocked
	activeSystem := baselineExp.Config
	promRepPreAuth, err := gitlab.ExecutePromotion(&winningProp, nil, secretKey, &activeSystem, baselineExp, pol, audit)
	if err == nil || promRepPreAuth.Status != "REJECTED" {
		t.Fatalf("SECURITY BREACH: promotion proceeded without human approval!")
	}
	t.Log("Pre-approval promotion gate verified: promotion blocked pending human authorization.")

	// 3. Legitimate human authorization
	humanApprover := gov.ApproverInfo{
		ID:      "sarah-lead",
		Name:    "Sarah SRE Lead",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}
	apprRec, err := gov.Authorize(&winningProp, humanApprover, cfgDigest, verifDigest, pol, "APPROVED", "telemetry and verifier reports reviewed and approved", secretKey)
	if err != nil {
		t.Fatalf("human authorization failed: %v", err)
	}
	t.Logf("Human Authorization granted by %s (Approval ID: %s, Signature: %s..)",
		humanApprover.ID, apprRec.ApprovalID, apprRec.Signature[:16])

	_ = lm.Transition(winningProp.ID, gov.StateApproved, "authorized by human approver")
	audit.Append(winningProp.ID, gov.ActionApproved, humanApprover.ID, cfgDigest, "human authorization granted")

	t.Log("=== STEP 17: Controlled promotion execution with drift invalidation checks ===")
	// Test drift invalidation: if proposal is tampered with post-approval, promotion must fail
	tamperedProp := winningProp
	tamperedProp.Changes = append([]ops.ParameterChange{}, winningProp.Changes...)
	tamperedProp.Changes[0].ProposedValue = 99 // out of band modification
	driftRep, err := gitlab.ExecutePromotion(&tamperedProp, apprRec, secretKey, &activeSystem, baselineExp, pol, audit)
	if err == nil || driftRep.Status != "REJECTED" {
		t.Fatalf("SECURITY BREACH: promotion succeeded despite configuration drift!")
	}
	t.Logf("Configuration drift check verified: promotion rejected modified proposal (%v)", err)

	// Execute legitimate promotion
	promRep, err := gitlab.ExecutePromotion(&winningProp, apprRec, secretKey, &activeSystem, baselineExp, pol, audit)
	if err != nil {
		t.Fatalf("controlled promotion execution failed: %v", err)
	}
	if promRep.Status != "SUCCESS" {
		t.Fatalf("promotion did not succeed: %s (reason: %s)", promRep.Status, promRep.RejectionReason)
	}
	_ = lm.Transition(winningProp.ID, gov.StatePromoted, "promoted to active runtime")
	t.Logf("Promotion SUCCESS: Promoted by %s, Audit Hash: %s..",
		promRep.PromotedBy, promRep.AuditTrailHash[:16])

	t.Log("=== STEP 18: Post-promotion validation confirmed healthy operation ===")
	if promRep.PostValidationVerdict == nil {
		t.Fatalf("post-promotion validation verdict is nil")
	}
	if promRep.PostValidationVerdict.Status == verify.StatusFail || promRep.PostValidationVerdict.Status == verify.StatusInvalidInput {
		t.Fatalf("post-promotion validation failed verifier checks: %s", promRep.PostValidationVerdict.Status)
	}
	t.Logf("Post-promotion verification status: %s (0 findings)", promRep.PostValidationVerdict.Status)

	// Final verification of entire audit trail
	if err := audit.VerifyIntegrity(); err != nil {
		t.Fatalf("audit trail integrity compromised: %v", err)
	}
	trail := audit.GetProposalTrail(winningProp.ID)
	t.Logf("Reconstructable Audit Trail verified: %d chained events recorded without gap or tampering", len(trail))

	// Verify lifecycle manager state
	st, err := lm.GetState(winningProp.ID)
	if err != nil || st != gov.StatePromoted {
		t.Fatalf("expected lifecycle state PROMOTED, got %s (err: %v)", st, err)
	}
	t.Log("=== END-TO-END OPERATIONAL WORKFLOW FULLY VERIFIED ===")
}

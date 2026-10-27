package gitlab

import (
	"strings"
	"testing"

	"sentra/gov"
	"sentra/ops"
	"sentra/sim"
	"sentra/verify"
)

func mockScenarioAndProposal() (sim.Experiment, ops.Proposal, *sim.RunResult, *ops.InvestigationReport) {
	exp := sim.ScenarioC()
	res := sim.RunExperiment(exp)
	inv, _ := ops.Investigate(&res)

	pb := ops.NewProposalBuilder("prop-gl-001", "agent-sentra", inv, inv.Hypotheses[0])
	pb.AddChange("breaker.failure_threshold", 2, 4, "Tuning breaker threshold")
	prop, _ := pb.Build()

	return exp, prop, &res, inv
}

func TestMergeRequestArtifactAndDescriptionGeneration(t *testing.T) {
	exp, prop, res, _ := mockScenarioAndProposal()

	proposedCfg := exp.Config
	proposedCfg.Breaker.FailureThreshold = 4

	verdict := verify.Verdict{
		Status: verify.StatusPass,
	}
	adj := &gov.AdjudicationResult{
		Eligible:               true,
		PassedSafetyProperties: []string{"S-worker-conservation", "B-worker-ownership-intervals"},
	}

	// 1. Bundle artifacts
	artifacts, err := BundleProposalArtifacts(&prop, exp.Config, proposedCfg, &verdict, adj)
	if err != nil {
		t.Fatalf("BundleProposalArtifacts failed: %v", err)
	}

	if len(artifacts.ProposalJSON) == 0 {
		t.Errorf("ProposalJSON is empty")
	}
	if !strings.Contains(artifacts.ConfigDiff, "breaker.failure_threshold") {
		t.Errorf("ConfigDiff missing expected diff")
	}
	if !strings.Contains(artifacts.EvidenceSummaryMD, "Evidence & Verification Summary") {
		t.Errorf("EvidenceSummaryMD missing header")
	}

	// 2. Format Branch Name
	branch := FormatProposalBranchName(prop.ID)
	if branch != "ops/proposal-prop-gl-001" {
		t.Errorf("unexpected branch name: %s", branch)
	}

	// 3. Generate MR Description
	mrDesc := GenerateMergeRequestDescription(
		&prop,
		exp.Config,
		proposedCfg,
		&res.Summary,
		&res.Summary,
		&verdict,
		adj,
		"digest-sha256-abcdef",
	)

	for _, section := range []string{
		"1. Operational Problem Statement",
		"2. Agent Hypothesis",
		"3. Proposed Configuration Changes",
		"4. Telemetry Impact Comparison",
		"5. Independent Verifier Adjudication",
		"6. Governance Compliance",
		"7. Required Human Authorization",
	} {
		if !strings.Contains(mrDesc, section) {
			t.Errorf("MR description missing section %q", section)
		}
	}
}

func TestCIPipelineExecutionSuccessAndFailurePaths(t *testing.T) {
	exp, prop, _, _ := mockScenarioAndProposal()
	pol := gov.DefaultPolicy()

	proposedCfg := exp.Config
	proposedCfg.Breaker.FailureThreshold = 4

	artifacts, err := BundleProposalArtifacts(&prop, exp.Config, proposedCfg, nil, nil)
	if err != nil {
		t.Fatalf("BundleProposalArtifacts: %v", err)
	}

	// 1. CI Success Path
	report, err := RunCIPipeline(artifacts, exp, pol)
	if err != nil {
		t.Fatalf("RunCIPipeline error: %v", err)
	}
	if !report.Passed {
		t.Errorf("expected CI pipeline to pass, but failed: %s", report.BlockReason)
	}
	if len(report.Stages) != 10 {
		t.Errorf("expected 10 CI stages, got %d", len(report.Stages))
	}

	// 2. CI Failure Path: Malformed/Invalid Proposal JSON
	corruptArtifacts := *artifacts
	corruptArtifacts.ProposalJSON = []byte("{ invalid json")
	corruptReport, _ := RunCIPipeline(&corruptArtifacts, exp, pol)
	if corruptReport.Passed {
		t.Errorf("expected CI to fail on corrupt proposal JSON")
	} else if !strings.Contains(corruptReport.BlockReason, "validate-schema") {
		t.Errorf("expected failure at validate-schema stage, got: %s", corruptReport.BlockReason)
	}

	// 3. CI Failure Path: Boundary Violation in Proposal
	badProp := prop
	badProp.Changes = []ops.ParameterChange{
		{
			Parameter:     "verifier.property.bypass",
			OriginalValue: false,
			ProposedValue: true,
			Reason:        "hack",
		},
	}
	badArtifacts, _ := BundleProposalArtifacts(&badProp, exp.Config, proposedCfg, nil, nil)
	badReport, _ := RunCIPipeline(badArtifacts, exp, pol)
	if badReport.Passed {
		t.Errorf("expected CI to fail on protected parameter modification")
	} else if !strings.Contains(badReport.BlockReason, "protected path") && !strings.Contains(badReport.BlockReason, "validate-boundaries") {
		t.Errorf("expected failure citing protected path or boundary validation, got: %s", badReport.BlockReason)
	}
}

func TestPromotionExecution(t *testing.T) {
	exp, prop, _, _ := mockScenarioAndProposal()
	pol := gov.DefaultPolicy()
	secretKey := []byte("promotion-test-secret-key-32-byte")
	audit := gov.NewAuditLog()

	activeConfig := exp.Config

	// 1. Missing approval record MUST block promotion
	rep, err := ExecutePromotion(&prop, nil, secretKey, &activeConfig, exp, pol, audit)
	if err == nil || rep.Status != "REJECTED" {
		t.Errorf("expected missing approval to block promotion, got: %v, rep status: %s", err, rep.Status)
	}

	// 2. Authorize legally
	approver := gov.ApproverInfo{
		ID:      "sarah-lead",
		Name:    "Sarah SRE",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}
	cfgDigest, _ := prop.Digest()
	verifDigest := "verif-sha256-verified-evidence-hash"

	apprRec, err := gov.Authorize(&prop, approver, cfgDigest, verifDigest, pol, "APPROVED", "changes validated", secretKey)
	if err != nil {
		t.Fatalf("Authorize failed: %v", err)
	}

	// 3. Successful Promotion Execution
	promRep, err := ExecutePromotion(&prop, apprRec, secretKey, &activeConfig, exp, pol, audit)
	if err != nil {
		t.Fatalf("ExecutePromotion failed: %v", err)
	}
	if promRep.Status != "SUCCESS" {
		t.Errorf("expected promotion status SUCCESS, got: %s (reason: %s)", promRep.Status, promRep.RejectionReason)
	}
	if promRep.AppliedConfig.Breaker.FailureThreshold != 4 {
		t.Errorf("expected active config Breaker.FailureThreshold to be updated to 4, got: %d", promRep.AppliedConfig.Breaker.FailureThreshold)
	}
	if promRep.PostValidationVerdict == nil {
		t.Errorf("expected post-promotion validation verdict, got nil")
	}

	// 4. Verify Audit Log recorded the promotion
	if err := audit.VerifyIntegrity(); err != nil {
		t.Errorf("audit log integrity failed after promotion: %v", err)
	}
	trail := audit.GetProposalTrail(prop.ID)
	if len(trail) == 0 {
		t.Errorf("expected audit events for %s, got 0", prop.ID)
	}

	// 5. Stale/Drifted Proposal MUST be rejected by promotion
	tamperedProp := prop
	tamperedProp.Changes[0].ProposedValue = 10 // changed after approval!
	driftRep, err := ExecutePromotion(&tamperedProp, apprRec, secretKey, &activeConfig, exp, pol, audit)
	if err == nil || driftRep.Status != "REJECTED" {
		t.Errorf("expected configuration drift to block promotion, got: %v", err)
	}
}

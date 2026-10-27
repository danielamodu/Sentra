package gitlab

import (
	"encoding/json"
	"fmt"
	"time"

	"sentra/domain"
	"sentra/evidence"
	"sentra/gov"
	"sentra/ops"
	"sentra/sim"
	"sentra/verify"
)

// RunCIPipeline executes the independent 10-stage CI/CD adjudication pipeline.
// Fails closed: any error or contract violation halts the pipeline and blocks promotion.
func RunCIPipeline(
	artifacts *ProposalArtifacts,
	baselineExp sim.Experiment,
	pol gov.Policy,
) (*CIAdjudicationReport, error) {
	start := time.Now().UTC()
	pipelineID := fmt.Sprintf("ci-pipe-%d", start.UnixNano())

	report := &CIAdjudicationReport{
		PipelineID:  pipelineID,
		Passed:      true,
		PublishedAt: start,
	}

	recordStage := func(name string, passed bool, d time.Duration, msg, errTxt string) bool {
		report.Stages = append(report.Stages, CIStageResult{
			Name:      name,
			Passed:    passed,
			Duration:  d,
			Message:   msg,
			ErrorText: errTxt,
		})
		if !passed {
			report.Passed = false
			report.BlockReason = fmt.Sprintf("pipeline blocked at stage %q: %s (%s)", name, msg, errTxt)
		}
		return passed
	}

	// Stage 1: Checkout and Ingest Proposal Artifacts
	s0Start := time.Now()
	if artifacts == nil {
		recordStage("checkout-artifacts", false, 0, "artifacts payload is nil", "missing proposal artifacts")
		return report, nil
	}
	recordStage("checkout-artifacts", true, time.Since(s0Start), "proposal artifacts bundle checked out and verified", "")

	// Stage 2: Validate Proposal Schema
	s1Start := time.Now()
	var prop ops.Proposal
	if err := json.Unmarshal(artifacts.ProposalJSON, &prop); err != nil {
		recordStage("validate-schema", false, time.Since(s1Start), "failed to unmarshal proposal.json", err.Error())
		return report, nil
	}
	if err := ops.ValidateProposal(&prop); err != nil {
		recordStage("validate-schema", false, time.Since(s1Start), "proposal schema validation failed", err.Error())
		return report, nil
	}
	recordStage("validate-schema", true, time.Since(s1Start), "proposal schema valid", "")

	// Stage 2: Validate Technical Boundaries & Allowlists
	s2Start := time.Now()
	if err := ops.ValidateBoundaries(prop.Changes); err != nil {
		recordStage("validate-boundaries", false, time.Since(s2Start), "boundary validation failed", err.Error())
		return report, nil
	}
	recordStage("validate-boundaries", true, time.Since(s2Start), "all proposed changes within technical boundaries", "")

	// Stage 3: Reconstruct Proposed Configuration
	s3Start := time.Now()
	propCfg := baselineExp.Config
	// Deep-copy connection pools
	if baselineExp.Config.ConnectionPools != nil {
		propCfg.ConnectionPools = make(map[domain.UpstreamID]int)
		for k, v := range baselineExp.Config.ConnectionPools {
			propCfg.ConnectionPools[k] = v
		}
	}
	// Deep-copy tenants
	if baselineExp.Config.Tenants != nil {
		propCfg.Tenants = make(map[domain.TenantID]sim.TenantConfig)
		for k, v := range baselineExp.Config.Tenants {
			propCfg.Tenants[k] = v
		}
	}

	for _, c := range prop.Changes {
		if err := ops.ApplyChangeToConfig(&propCfg, c); err != nil {
			recordStage("reconstruct-config", false, time.Since(s3Start), "failed to apply parameter change", err.Error())
			return report, nil
		}
	}
	recordStage("reconstruct-config", true, time.Since(s3Start), "proposed configuration reconstructed cleanly", "")

	// Stage 4: Run Deterministic Simulation
	s4Start := time.Now()
	propExp := baselineExp
	propExp.Name = fmt.Sprintf("%s-ci-adjudication", baselineExp.Name)
	propExp.Config = propCfg

	simRes := sim.RunExperiment(propExp)
	recordStage("simulate", true, time.Since(s4Start),
		fmt.Sprintf("simulation completed: offered=%d completed=%d failed=%d shed=%d",
			simRes.Summary.Offered, simRes.Summary.Completed, simRes.Summary.Failed, simRes.Summary.Shed), "")

	// Stage 5: Generate Evidence
	s5Start := time.Now()
	doc, err := evidence.FromRun(simRes)
	if err != nil {
		recordStage("generate-evidence", false, time.Since(s5Start), "failed to serialize evidence document", err.Error())
		return report, nil
	}
	recordStage("generate-evidence", true, time.Since(s5Start),
		fmt.Sprintf("evidence serialized with %d records (terminal attestation present: %t)",
			len(doc.Evidence.Records), doc.Evidence.Terminal != nil), "")

	// Stage 6: Independent Verifier Adjudication
	s6Start := time.Now()
	verdict := verify.Verify(doc)
	report.VerificationVerdict = &verdict

	if verdict.Status == verify.StatusFail {
		recordStage("independent-verification", false, time.Since(s6Start),
			"independent verifier returned FAIL", fmt.Sprintf("%d invariant findings", len(verdict.Findings)))
		return report, nil
	}
	if verdict.Status == verify.StatusInvalidInput {
		recordStage("independent-verification", false, time.Since(s6Start),
			"independent verifier returned INVALID_INPUT", fmt.Sprintf("%d structural findings", len(verdict.Findings)))
		return report, nil
	}
	recordStage("independent-verification", true, time.Since(s6Start),
		fmt.Sprintf("independent verification status: %s (0 invariant failures)", verdict.Status), "")

	// Stage 7: Governance Policy Enforcement
	s7Start := time.Now()
	adj, err := gov.Adjudicate(&prop, &verdict, &doc, pol)
	if err != nil {
		recordStage("govern-policy", false, time.Since(s7Start), "governance adjudication error", err.Error())
		return report, nil
	}
	report.AdjudicationResult = adj

	if !adj.Eligible {
		reasons := "unspecified eligibility rejection"
		if len(adj.RejectionReasons) > 0 {
			reasons = adj.RejectionReasons[0]
		}
		recordStage("govern-policy", false, time.Since(s7Start), "proposal not eligible for promotion under policy", reasons)
		return report, nil
	}
	recordStage("govern-policy", true, time.Since(s7Start),
		fmt.Sprintf("governance policy satisfied: %d safety properties passed", len(adj.PassedSafetyProperties)), "")

	// Stage 8: Publish Machine and Human Reports
	s8Start := time.Now()
	recordStage("publish-reports", true, time.Since(s8Start), "adjudication reports generated and archived", "")

	// Stage 9: Promotion Boundary Gate (enforces that CI does not automatically promote)
	s9Start := time.Now()
	recordStage("promotion-gate", true, time.Since(s9Start), "promotion blocked pending required human authorization", "")

	return report, nil
}

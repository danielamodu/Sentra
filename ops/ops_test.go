package ops

import (
	"strings"
	"testing"

	"sentra/sim"
)

// Helper to create a baseline experiment with a degraded upstream (Scenario C)
func createDegradedExperiment() sim.Experiment {
	return sim.ScenarioC()
}

func TestInvestigationTelemetryExtraction(t *testing.T) {
	exp := createDegradedExperiment()
	res := sim.RunExperiment(exp)

	inv, err := Investigate(&res)
	if err != nil {
		t.Fatalf("Investigate failed: %v", err)
	}

	if len(inv.Observations) == 0 {
		t.Fatalf("expected non-empty observations, got 0")
	}

	// Verify required metrics were extracted
	metricsFound := make(map[string]bool)
	for _, obs := range inv.Observations {
		metricsFound[obs.Metric] = true
		if obs.EvidenceRef == "" {
			t.Errorf("observation %s missing evidence reference", obs.Metric)
		}
	}

	for _, req := range []string{"OfferedRequests", "GoodputRequests", "FailedRequests", "AttemptsWasted"} {
		if !metricsFound[req] {
			t.Errorf("expected observation for metric %s not found", req)
		}
	}

	// Verify hypotheses were formed
	if len(inv.Hypotheses) == 0 {
		t.Fatalf("expected at least one operational hypothesis, got 0")
	}

	for _, h := range inv.Hypotheses {
		if h.ID == "" || h.Symptom == "" || len(h.CandidateParameters) == 0 {
			t.Errorf("malformed hypothesis: %+v", h)
		}
		if len(h.SupportingObservations) == 0 {
			t.Errorf("hypothesis %s has no supporting observations", h.ID)
		}
	}
}

func TestTechnicalBoundariesAllowlistAndProtection(t *testing.T) {
	// 1. Allowed parameter should pass
	validChange := ParameterChange{
		Parameter:     "breaker.failure_threshold",
		OriginalValue: 3,
		ProposedValue: 5,
		Reason:        "Tuning breaker sensitivity",
	}
	if err := ValidateParameterChange(validChange); err != nil {
		t.Errorf("expected valid change to pass, got: %v", err)
	}

	// 2. Out of range parameter should fail
	outOfRangeChange := ParameterChange{
		Parameter:     "breaker.failure_threshold",
		OriginalValue: 3,
		ProposedValue: 500, // max is 50
		Reason:        "Excessive threshold",
	}
	if err := ValidateParameterChange(outOfRangeChange); err == nil {
		t.Errorf("expected out of range error, got nil")
	}

	// 3. Protected verifier paths must be rejected
	protectedVerifierChange := ParameterChange{
		Parameter:     "verifier.property.bypass",
		OriginalValue: false,
		ProposedValue: true,
		Reason:        "Attempting to disable verifier checks",
	}
	if err := ValidateParameterChange(protectedVerifierChange); err == nil {
		t.Errorf("expected protected verifier error, got nil")
	} else if !strings.Contains(err.Error(), "protected path") {
		t.Errorf("expected error citing protected path, got: %v", err)
	}

	// 4. Protected governance paths must be rejected
	protectedGovChange := ParameterChange{
		Parameter:     "governance.policy.skip_approval",
		OriginalValue: false,
		ProposedValue: true,
		Reason:        "Attempting to bypass governance",
	}
	if err := ValidateParameterChange(protectedGovChange); err == nil {
		t.Errorf("expected protected governance error, got nil")
	}

	// 5. Unknown parameter not in allowlist must be rejected
	unknownParamChange := ParameterChange{
		Parameter:     "arbitrary.runtime.hack",
		OriginalValue: 1,
		ProposedValue: 2,
		Reason:        "Unknown knob",
	}
	if err := ValidateParameterChange(unknownParamChange); err == nil {
		t.Errorf("expected unknown parameter error, got nil")
	}
}

func TestProposalValidationAndDiff(t *testing.T) {
	exp := createDegradedExperiment()
	res := sim.RunExperiment(exp)
	inv, _ := Investigate(&res)

	// Valid proposal
	pb := NewProposalBuilder("prop-101", "agent-sentra", inv, inv.Hypotheses[0])
	pb.AddChange("breaker.failure_threshold", 3, 5, "Prevent premature tripping")
	pb.AddExpectedEffect("Wasted work reduction")
	pb.AddPotentialRegression("Slightly delayed trip under sustained fault")

	prop, err := pb.Build()
	if err != nil {
		t.Fatalf("expected valid proposal to build, got: %v", err)
	}

	// Verify digest
	digest, err := prop.Digest()
	if err != nil || digest == "" {
		t.Errorf("failed to compute proposal digest: %v", err)
	}

	// Verify ConfigDiff
	proposedCfg := exp.Config
	origThresh := exp.Config.Breaker.FailureThreshold
	proposedCfg.Breaker.FailureThreshold = origThresh + 2
	diff := ConfigDiff(exp.Config, proposedCfg)
	if !strings.Contains(diff, "- breaker.failure_threshold =") || !strings.Contains(diff, "+ breaker.failure_threshold =") {
		t.Errorf("unexpected diff output:\n%s", diff)
	}

	// Invalid proposal: missing ID
	invalidProp := prop
	invalidProp.ID = ""
	if err := ValidateProposal(&invalidProp); err == nil {
		t.Errorf("expected validation error for empty proposal ID, got nil")
	}

	// Invalid proposal: missing author
	invalidProp = prop
	invalidProp.AuthorID = ""
	if err := ValidateProposal(&invalidProp); err == nil {
		t.Errorf("expected validation error for empty AuthorID, got nil")
	}
}

func TestAutonomousExperimenterExecutionAndRanking(t *testing.T) {
	exp := createDegradedExperiment()
	res := sim.RunExperiment(exp)
	inv, err := Investigate(&res)
	if err != nil {
		t.Fatalf("Investigate failed: %v", err)
	}

	budget := ExperimentBudget{
		MaxIterations:   3,
		MaxCandidates:   6,
		MaxSimulations:  8,
		MaxDeltaPercent: 100.0,
	}

	weights := DefaultObjectiveWeights()
	expEngine, err := NewExperimenter("agent-sentra", budget, weights)
	if err != nil {
		t.Fatalf("NewExperimenter failed: %v", err)
	}

	record, err := expEngine.RunAutonomousLoop(exp, &res, inv)
	if err != nil {
		t.Fatalf("RunAutonomousLoop failed: %v", err)
	}

	if len(record.Candidates) == 0 {
		t.Fatalf("expected candidates to be evaluated, got 0")
	}

	// Ensure execution budget was respected
	if record.SimulationsRun > budget.MaxSimulations {
		t.Errorf("simulations run %d exceeded budget %d", record.SimulationsRun, budget.MaxSimulations)
	}
	if len(record.Candidates) > budget.MaxCandidates {
		t.Errorf("candidates count %d exceeded budget %d", len(record.Candidates), budget.MaxCandidates)
	}

	// Verify that each candidate was evaluated by the independent verifier
	for _, c := range record.Candidates {
		if c.Verdict == nil {
			t.Errorf("candidate %s missing verifier verdict", c.CandidateID)
		}
		t.Logf("Candidate %s: Goodput=%d, Shed=%d, Score=%.2f, Rejected=%t",
			c.CandidateID, c.Summary.Completed, c.Summary.Shed, c.Score, c.Rejected)
	}

	// Verify that an actual improvement was discovered over the baseline
	if record.BestCandidate == nil {
		t.Fatalf("expected a surviving best candidate, got nil")
	}
	t.Logf("Baseline Score: %.2f (Goodput: %d) -> Best Candidate Score: %.2f (Goodput: %d)",
		record.BaselineScore, res.Summary.Completed, record.BestCandidate.Score, record.BestCandidate.Summary.Completed)
	if record.BestCandidate.Score <= record.BaselineScore {
		t.Errorf("expected best candidate score %.2f to exceed baseline %.2f", record.BestCandidate.Score, record.BaselineScore)
	}
	if record.BestCandidate.Summary.Completed <= res.Summary.Completed {
		t.Errorf("expected best candidate goodput %d to exceed baseline %d", record.BestCandidate.Summary.Completed, res.Summary.Completed)
	}
}

func TestVerifierRejectionOfUnsafeCandidate(t *testing.T) {
	exp := createDegradedExperiment()
	res := sim.RunExperiment(exp)
	inv, _ := Investigate(&res)

	// Construct an unsafe candidate that violates breaker invariants or parameters
	// If a candidate causes a FAIL or INVALID_INPUT, evaluateCandidate must mark it Rejected = true
	expEngine, _ := NewExperimenter("agent-sentra", DefaultBudget(), DefaultObjectiveWeights())

	unsafeVariant := candidateVariant{
		id:   "cand-unsafe-boundary-breach",
		desc: "Attempt out of bounds parameter",
		mutate: func(cfg *sim.SystemConfig) []ParameterChange {
			return []ParameterChange{{
				Parameter:     "breaker.failure_threshold",
				OriginalValue: 3,
				ProposedValue: 99999, // out of allowed range [1, 50]
				Reason:        "Unsafe value",
			}}
		},
	}

	cand := expEngine.evaluateCandidate(exp, unsafeVariant, inv.Hypotheses[0], inv)
	if !cand.Rejected {
		t.Errorf("expected unsafe candidate to be rejected, but it survived")
	}
	if !strings.Contains(cand.RejectionReason, "boundary violation") && !strings.Contains(cand.RejectionReason, "out of allowed range") {
		t.Errorf("unexpected rejection reason: %s", cand.RejectionReason)
	}
}

func TestBudgetValidation(t *testing.T) {
	invalidBudget := ExperimentBudget{
		MaxIterations: 0,
	}
	if err := invalidBudget.Validate(); err == nil {
		t.Errorf("expected error for 0 max_iterations, got nil")
	}

	invalidBudget2 := ExperimentBudget{
		MaxIterations:  5,
		MaxCandidates:  10,
		MaxSimulations: 500, // max 200
	}
	if err := invalidBudget2.Validate(); err == nil {
		t.Errorf("expected error for excessive max_simulations, got nil")
	}
}

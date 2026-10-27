package ops

import (
	"fmt"
	"time"

	"sentra/domain"
	"sentra/sim"
	"sentra/verify"
)

// Symptom represents an observed operational degradation pattern.
type Symptom string

const (
	SymptomWastedWorkElevated   Symptom = "WastedWorkElevated"
	SymptomConnectionStranding  Symptom = "ConnectionStranding"
	SymptomRetryAmplification   Symptom = "RetryAmplification"
	SymptomQueueCongestion      Symptom = "QueueCongestion"
	SymptomPrematureBreakerTrip Symptom = "PrematureBreakerTrip"
	SymptomTenantStarvation     Symptom = "TenantStarvation"
	SymptomExcessiveLatency     Symptom = "ExcessiveLatency"
)

// Observation represents a factual measurement extracted from telemetry or ledger.
// Measurements are never fabricated; every observation points to its metric and context.
type Observation struct {
	Metric      string  `json:"metric"`
	Value       float64 `json:"value"`
	Unit        string  `json:"unit"`
	Context     string  `json:"context,omitempty"`
	EvidenceRef string  `json:"evidence_ref,omitempty"`
}

// Hypothesis represents an explicit operational hypothesis formed by the agent,
// tying observed symptoms to suspected causes and tunable candidate parameters.
type Hypothesis struct {
	ID                     string        `json:"id"`
	Symptom                Symptom       `json:"symptom"`
	Description            string        `json:"description"`
	SupportingObservations []Observation `json:"supporting_observations"`
	CandidateParameters    []string      `json:"candidate_parameters"`
	Rationale              string        `json:"rationale"`
}

// ParameterChange represents a proposed modification to an operational parameter.
type ParameterChange struct {
	Parameter     string `json:"parameter"`
	OriginalValue any    `json:"original_value"`
	ProposedValue any    `json:"proposed_value"`
	Reason        string `json:"reason"`
}

// ExperimentConfigSnapshot captures the baseline conditions used for evaluation.
type ExperimentConfigSnapshot struct {
	ScenarioName string         `json:"scenario_name"`
	HorizonNS    domain.SimTime `json:"horizon_ns"`
	WorkloadName string         `json:"workload_name,omitempty"`
	FaultSummary []string       `json:"fault_summary,omitempty"`
}

// Proposal is a structured, machine-readable operational change proposal.
type Proposal struct {
	ID                   string                   `json:"id"`
	AuthorID             string                   `json:"author_id"`
	CreatedAt            time.Time                `json:"created_at"`
	TargetConfigVersion  string                   `json:"target_config_version"`
	Changes              []ParameterChange        `json:"changes"`
	Observations         []Observation            `json:"observations"`
	Hypothesis           Hypothesis               `json:"hypothesis"`
	ExpectedEffects      []string                 `json:"expected_effects"`
	PotentialRegressions []string                 `json:"potential_regressions"`
	RelevantInvariants   []string                 `json:"relevant_invariants"`
	ExperimentConfig     ExperimentConfigSnapshot `json:"experiment_config"`
	EvidenceRefs         []string                 `json:"evidence_refs"`
}

// Candidate represents an explored configuration variant during experimentation.
type Candidate struct {
	CandidateID     string                 `json:"candidate_id"`
	Proposal        Proposal               `json:"proposal"`
	Config          sim.SystemConfig       `json:"-"`
	RunResult       *sim.RunResult         `json:"-"`
	Verdict         *verify.Verdict        `json:"verdict,omitempty"`
	Summary         *sim.ExperimentSummary `json:"summary,omitempty"`
	Score           float64                `json:"score"`
	Rejected        bool                   `json:"rejected"`
	RejectionReason string                 `json:"rejection_reason,omitempty"`
	Findings        []verify.Finding       `json:"findings,omitempty"`
}

// ExperimentBudget limits the resources allocated to autonomous exploration.
type ExperimentBudget struct {
	MaxIterations   int     `json:"max_iterations"`
	MaxCandidates   int     `json:"max_candidates"`
	MaxSimulations  int     `json:"max_simulations"`
	MaxDeltaPercent float64 `json:"max_delta_percent"`
}

// DefaultBudget returns a conservative, bounded experimentation budget.
func DefaultBudget() ExperimentBudget {
	return ExperimentBudget{
		MaxIterations:   5,
		MaxCandidates:   10,
		MaxSimulations:  20,
		MaxDeltaPercent: 100.0, // max 100% change from baseline in one proposal
	}
}

// Validate validates that the budget parameters are positive and bounded.
func (b ExperimentBudget) Validate() error {
	if b.MaxIterations <= 0 || b.MaxIterations > 50 {
		return fmt.Errorf("invalid max_iterations %d: must be in 1..50", b.MaxIterations)
	}
	if b.MaxCandidates <= 0 || b.MaxCandidates > 100 {
		return fmt.Errorf("invalid max_candidates %d: must be in 1..100", b.MaxCandidates)
	}
	if b.MaxSimulations <= 0 || b.MaxSimulations > 200 {
		return fmt.Errorf("invalid max_simulations %d: must be in 1..200", b.MaxSimulations)
	}
	if b.MaxDeltaPercent <= 0 || b.MaxDeltaPercent > 500 {
		return fmt.Errorf("invalid max_delta_percent %.1f: must be in 1..500", b.MaxDeltaPercent)
	}
	return nil
}

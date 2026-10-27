package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"sentra/domain"
	"sentra/sim"
)

// ValidateProposal validates that a proposal conforms strictly to the proposal schema,
// technical configuration boundaries, and operational invariants.
func ValidateProposal(p *Proposal) error {
	if p == nil {
		return fmt.Errorf("proposal is nil")
	}
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("proposal ID is required")
	}
	if strings.TrimSpace(p.AuthorID) == "" {
		return fmt.Errorf("proposal AuthorID is required")
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("proposal CreatedAt timestamp is required")
	}
	if strings.TrimSpace(p.TargetConfigVersion) == "" {
		return fmt.Errorf("proposal TargetConfigVersion is required")
	}
	if len(p.Changes) == 0 {
		return fmt.Errorf("proposal must declare at least one parameter change")
	}
	if err := ValidateBoundaries(p.Changes); err != nil {
		return fmt.Errorf("proposal boundary validation failed: %w", err)
	}
	if strings.TrimSpace(p.Hypothesis.ID) == "" {
		return fmt.Errorf("proposal must declare a valid Hypothesis ID")
	}
	if len(p.Hypothesis.SupportingObservations) == 0 {
		return fmt.Errorf("hypothesis must contain supporting observations")
	}
	if len(p.RelevantInvariants) == 0 {
		return fmt.Errorf("proposal must specify relevant verification invariants")
	}
	if p.ExperimentConfig.ScenarioName == "" {
		return fmt.Errorf("proposal must specify experiment scenario name")
	}
	if len(p.EvidenceRefs) == 0 {
		return fmt.Errorf("proposal must cite baseline evidence references")
	}
	return nil
}

// ProposalDigest computes the deterministic SHA-256 digest of a Proposal.
func (p Proposal) Digest() (string, error) {
	// Canonicalize by serializing to sorted JSON
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal proposal for digest: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// ConfigDiff generates a human-readable text diff comparing original and proposed SystemConfig.
func ConfigDiff(original, proposed sim.SystemConfig) string {
	var lines []string
	lines = append(lines, "--- active_config")
	lines = append(lines, "+++ proposed_config")

	// WorkerPool
	if original.WorkerPool.Size != proposed.WorkerPool.Size {
		lines = append(lines, fmt.Sprintf("- worker_pool.size = %d", original.WorkerPool.Size))
		lines = append(lines, fmt.Sprintf("+ worker_pool.size = %d", proposed.WorkerPool.Size))
	}

	// Breaker
	if original.Breaker.FailureThreshold != proposed.Breaker.FailureThreshold {
		lines = append(lines, fmt.Sprintf("- breaker.failure_threshold = %d", original.Breaker.FailureThreshold))
		lines = append(lines, fmt.Sprintf("+ breaker.failure_threshold = %d", proposed.Breaker.FailureThreshold))
	}
	if original.Breaker.Cooldown != proposed.Breaker.Cooldown {
		lines = append(lines, fmt.Sprintf("- breaker.cooldown_ns = %d", original.Breaker.Cooldown))
		lines = append(lines, fmt.Sprintf("+ breaker.cooldown_ns = %d", proposed.Breaker.Cooldown))
	}
	if original.Breaker.MaxHalfOpenProbes != proposed.Breaker.MaxHalfOpenProbes {
		lines = append(lines, fmt.Sprintf("- breaker.max_half_open_probes = %d", original.Breaker.MaxHalfOpenProbes))
		lines = append(lines, fmt.Sprintf("+ breaker.max_half_open_probes = %d", proposed.Breaker.MaxHalfOpenProbes))
	}

	// Retry
	if original.RetryBudget.Capacity != proposed.RetryBudget.Capacity {
		lines = append(lines, fmt.Sprintf("- retry_budget.capacity = %d", original.RetryBudget.Capacity))
		lines = append(lines, fmt.Sprintf("+ retry_budget.capacity = %d", proposed.RetryBudget.Capacity))
	}
	if original.RetryBudget.RefillInterval != proposed.RetryBudget.RefillInterval {
		lines = append(lines, fmt.Sprintf("- retry_budget.refill_interval_ns = %d", original.RetryBudget.RefillInterval))
		lines = append(lines, fmt.Sprintf("+ retry_budget.refill_interval_ns = %d", proposed.RetryBudget.RefillInterval))
	}
	if original.RetryBackoff != proposed.RetryBackoff {
		lines = append(lines, fmt.Sprintf("- retry_backoff_ns = %d", original.RetryBackoff))
		lines = append(lines, fmt.Sprintf("+ retry_backoff_ns = %d", proposed.RetryBackoff))
	}
	if original.AttemptTimeout != proposed.AttemptTimeout {
		lines = append(lines, fmt.Sprintf("- attempt_timeout_ns = %d", original.AttemptTimeout))
		lines = append(lines, fmt.Sprintf("+ attempt_timeout_ns = %d", proposed.AttemptTimeout))
	}

	// ConnectionPools
	var upKeys []string
	for k := range original.ConnectionPools {
		upKeys = append(upKeys, string(k))
	}
	for k := range proposed.ConnectionPools {
		if _, ok := original.ConnectionPools[k]; !ok {
			upKeys = append(upKeys, string(k))
		}
	}
	sort.Strings(upKeys)
	for _, up := range upKeys {
		origCap := original.ConnectionPools[domain.UpstreamID(up)]
		propCap := proposed.ConnectionPools[domain.UpstreamID(up)]
		if origCap != propCap {
			lines = append(lines, fmt.Sprintf("- connection_pools.%s = %d", up, origCap))
			lines = append(lines, fmt.Sprintf("+ connection_pools.%s = %d", up, propCap))
		}
	}

	// Tenants
	var tKeys []string
	for k := range original.Tenants {
		tKeys = append(tKeys, string(k))
	}
	for k := range proposed.Tenants {
		if _, ok := original.Tenants[k]; !ok {
			tKeys = append(tKeys, string(k))
		}
	}
	sort.Strings(tKeys)
	for _, t := range tKeys {
		ot := original.Tenants[domain.TenantID(t)]
		pt := proposed.Tenants[domain.TenantID(t)]
		if ot.MaxArrivalQueueDepth != pt.MaxArrivalQueueDepth {
			lines = append(lines, fmt.Sprintf("- tenants.%s.max_arrival_queue_depth = %d", t, ot.MaxArrivalQueueDepth))
			lines = append(lines, fmt.Sprintf("+ tenants.%s.max_arrival_queue_depth = %d", t, pt.MaxArrivalQueueDepth))
		}
		if ot.MaxRetryQueueDepth != pt.MaxRetryQueueDepth {
			lines = append(lines, fmt.Sprintf("- tenants.%s.max_retry_queue_depth = %d", t, ot.MaxRetryQueueDepth))
			lines = append(lines, fmt.Sprintf("+ tenants.%s.max_retry_queue_depth = %d", t, pt.MaxRetryQueueDepth))
		}
		if ot.MaxTotalQueueDepth != pt.MaxTotalQueueDepth {
			lines = append(lines, fmt.Sprintf("- tenants.%s.max_total_queue_depth = %d", t, ot.MaxTotalQueueDepth))
			lines = append(lines, fmt.Sprintf("+ tenants.%s.max_total_queue_depth = %d", t, pt.MaxTotalQueueDepth))
		}
	}

	if len(lines) == 2 {
		return "# No configuration differences"
	}
	return strings.Join(lines, "\n")
}

// NewProposalBuilder creates a new proposal builder anchored to an investigation report.
func NewProposalBuilder(propID, authorID string, inv *InvestigationReport, hyp Hypothesis) *ProposalBuilder {
	return &ProposalBuilder{
		p: Proposal{
			ID:                  propID,
			AuthorID:            authorID,
			CreatedAt:           time.Now().UTC(),
			TargetConfigVersion: "v1.0",
			Hypothesis:          hyp,
			Observations:        hyp.SupportingObservations,
			RelevantInvariants: []string{
				"S-worker-conservation",
				"S-connection-stranding",
				"B-worker-interval-pairing",
				"B-connection-interval-pairing",
				"C-admission-outcome",
				"C-fifo-dispatch",
				"D-retry-limit",
				"E-breaker-open-threshold",
				"L-request-lifecycle",
			},
			ExperimentConfig: ExperimentConfigSnapshot{
				ScenarioName: inv.ScenarioName,
				HorizonNS:    inv.Summary.Horizon,
			},
			EvidenceRefs: []string{
				fmt.Sprintf("evidence://%s/baseline", inv.ScenarioName),
			},
		},
	}
}

// ProposalBuilder provides a fluent builder for generating valid Proposals.
type ProposalBuilder struct {
	p Proposal
}

// AddChange appends a parameter change.
func (b *ProposalBuilder) AddChange(param string, orig, prop any, reason string) *ProposalBuilder {
	b.p.Changes = append(b.p.Changes, ParameterChange{
		Parameter:     param,
		OriginalValue: orig,
		ProposedValue: prop,
		Reason:        reason,
	})
	return b
}

// AddExpectedEffect appends an expected effect claim.
func (b *ProposalBuilder) AddExpectedEffect(eff string) *ProposalBuilder {
	b.p.ExpectedEffects = append(b.p.ExpectedEffects, eff)
	return b
}

// AddPotentialRegression appends a potential regression risk.
func (b *ProposalBuilder) AddPotentialRegression(reg string) *ProposalBuilder {
	b.p.PotentialRegressions = append(b.p.PotentialRegressions, reg)
	return b
}

// Build validates and returns the final Proposal.
func (b *ProposalBuilder) Build() (Proposal, error) {
	if err := ValidateProposal(&b.p); err != nil {
		return Proposal{}, err
	}
	return b.p, nil
}

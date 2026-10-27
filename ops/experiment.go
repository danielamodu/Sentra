package ops

import (
	"fmt"
	"sort"
	"time"

	"sentra/domain"
	"sentra/evidence"
	"sentra/sim"
	"sentra/verify"
)

// ExperimentRecord records the full lineage and results of an autonomous experimentation run.
type ExperimentRecord struct {
	SessionID       string                `json:"session_id"`
	ScenarioName    string                `json:"scenario_name"`
	BaselineSummary sim.ExperimentSummary `json:"baseline_summary"`
	BaselineScore   float64               `json:"baseline_score"`
	Hypothesis      Hypothesis            `json:"hypothesis"`
	Candidates      []Candidate           `json:"candidates"`
	BestCandidate   *Candidate            `json:"best_candidate,omitempty"`
	WinningProposal *Proposal             `json:"winning_proposal,omitempty"`
	CompletedAt     time.Time             `json:"completed_at"`
	IterationsRun   int                   `json:"iterations_run"`
	SimulationsRun  int                   `json:"simulations_run"`
}

// AutonomousExperimenter coordinates bounded autonomous exploration of configuration proposals.
type AutonomousExperimenter struct {
	authorID string
	budget   ExperimentBudget
	weights  ObjectiveWeights
}

// NewExperimenter creates a new AutonomousExperimenter with explicit author identity and budget.
func NewExperimenter(authorID string, budget ExperimentBudget, weights ObjectiveWeights) (*AutonomousExperimenter, error) {
	if err := budget.Validate(); err != nil {
		return nil, fmt.Errorf("invalid experiment budget: %w", err)
	}
	return &AutonomousExperimenter{
		authorID: authorID,
		budget:   budget,
		weights:  weights,
	}, nil
}

// RunAutonomousLoop executes the full bounded exploration loop:
// 1. Generates candidates based on the investigation report hypotheses.
// 2. Evaluates each candidate against identical initial conditions, workload, and faults.
// 3. Submits evidence to the independent verifier.
// 4. Rejects invariant-violating candidates with counterexamples.
// 5. Inspects counterexamples to refine candidates across iterations.
// 6. Ranks surviving candidates by declared operational objectives.
// 7. Emits the final winning proposal and complete experiment record.
func (ae *AutonomousExperimenter) RunAutonomousLoop(baselineExp sim.Experiment, baselineRes *sim.RunResult, inv *InvestigationReport) (*ExperimentRecord, error) {
	if baselineRes == nil || inv == nil {
		return nil, fmt.Errorf("baseline result and investigation report must not be nil")
	}
	if len(inv.Hypotheses) == 0 {
		return nil, fmt.Errorf("investigation report contains no hypotheses to explore")
	}

	sessionID := fmt.Sprintf("exp-sess-%d", time.Now().UnixNano())
	baselineScore := ScoreCandidate(&baselineRes.Summary, ae.weights)

	rec := &ExperimentRecord{
		SessionID:       sessionID,
		ScenarioName:    baselineExp.Name,
		BaselineSummary: baselineRes.Summary,
		BaselineScore:   baselineScore,
		Hypothesis:      inv.Hypotheses[0], // primary hypothesis
		CompletedAt:     time.Now().UTC(),
	}

	primaryHyp := inv.Hypotheses[0]
	simCount := 0
	iteration := 0

	var allCandidates []Candidate

	for iteration < ae.budget.MaxIterations && simCount < ae.budget.MaxSimulations {
		iteration++

		// Generate candidate variants for this iteration
		variants := ae.generateCandidateVariants(iteration, baselineExp.Config, primaryHyp)

		for _, v := range variants {
			if simCount >= ae.budget.MaxSimulations || len(allCandidates) >= ae.budget.MaxCandidates {
				break
			}
			simCount++

			cand := ae.evaluateCandidate(baselineExp, v, primaryHyp, inv)
			allCandidates = append(allCandidates, cand)

			// If candidate was rejected by verifier, inspect counterexample to guide next iteration
			if cand.Rejected && len(cand.Findings) > 0 {
				primaryHyp.Rationale = fmt.Sprintf("%s (Refined by counterexample %s: %s)",
					primaryHyp.Rationale, cand.Findings[0].Code, cand.Findings[0].Message)
			}
		}
	}

	rec.IterationsRun = iteration
	rec.SimulationsRun = simCount
	rec.Candidates = allCandidates

	// Rank surviving candidates
	var surviving []*Candidate
	for i := range rec.Candidates {
		c := &rec.Candidates[i]
		if !c.Rejected && c.Score >= baselineScore {
			surviving = append(surviving, c)
		}
	}

	sort.Slice(surviving, func(i, j int) bool {
		return surviving[i].Score > surviving[j].Score
	})

	if len(surviving) > 0 {
		rec.BestCandidate = surviving[0]
		rec.WinningProposal = &surviving[0].Proposal
	}

	return rec, nil
}

// candidateVariant defines a proposed set of changes for one candidate.
type candidateVariant struct {
	id     string
	desc   string
	mutate func(cfg *sim.SystemConfig) []ParameterChange
}

// generateCandidateVariants generates focused operational variants for a given hypothesis.
func (ae *AutonomousExperimenter) generateCandidateVariants(iteration int, baseCfg sim.SystemConfig, hyp Hypothesis) []candidateVariant {
	var variants []candidateVariant

	switch hyp.Symptom {
	case SymptomWastedWorkElevated, SymptomRetryAmplification:
		// Variant 1: increase retry backoff to damp retry storm
		variants = append(variants, candidateVariant{
			id:   fmt.Sprintf("cand-backoff-damp-it%d", iteration),
			desc: "Increase retry backoff delay",
			mutate: func(cfg *sim.SystemConfig) []ParameterChange {
				orig := cfg.RetryBackoff
				proposed := orig + domain.SimDuration(2_000_000*int64(iteration)) // +2ms * iter
				if proposed > 10_000_000 {
					proposed = 10_000_000
				}
				cfg.RetryBackoff = proposed
				return []ParameterChange{{
					Parameter:     "retry_backoff_ns",
					OriginalValue: int64(orig),
					ProposedValue: int64(proposed),
					Reason:        "Damp retry storm by spacing out retries during degraded upstream intervals",
				}}
			},
		})

		// Variant 2: throttle retry budget capacity
		variants = append(variants, candidateVariant{
			id:   fmt.Sprintf("cand-budget-throttle-it%d", iteration),
			desc: "Throttle retry budget capacity",
			mutate: func(cfg *sim.SystemConfig) []ParameterChange {
				orig := cfg.RetryBudget.Capacity
				proposed := orig - iteration
				if proposed < 1 {
					proposed = 1
				}
				cfg.RetryBudget.Capacity = proposed
				return []ParameterChange{{
					Parameter:     "retry_budget.capacity",
					OriginalValue: orig,
					ProposedValue: proposed,
					Reason:        "Limit retry amplification by reducing retry budget bucket capacity",
				}}
			},
		})

		// Variant 3: combined tuned backoff and retry budget
		variants = append(variants, candidateVariant{
			id:   fmt.Sprintf("cand-combined-retry-it%d", iteration),
			desc: "Combined backoff increase and budget throttle",
			mutate: func(cfg *sim.SystemConfig) []ParameterChange {
				origBackoff := cfg.RetryBackoff
				propBackoff := origBackoff + domain.SimDuration(1_500_000*int64(iteration))
				cfg.RetryBackoff = propBackoff

				origCap := cfg.RetryBudget.Capacity
				propCap := origCap - 1
				if propCap < 1 {
					propCap = 1
				}
				cfg.RetryBudget.Capacity = propCap

				return []ParameterChange{
					{
						Parameter:     "retry_backoff_ns",
						OriginalValue: int64(origBackoff),
						ProposedValue: int64(propBackoff),
						Reason:        "Spacing retry attempts to avoid compounding downstream delays",
					},
					{
						Parameter:     "retry_budget.capacity",
						OriginalValue: origCap,
						ProposedValue: propCap,
						Reason:        "Constraining total retries per window",
					},
				}
			},
		})

	case SymptomConnectionStranding:
		// Find degraded upstream
		for up := range baseCfg.ConnectionPools {
			upID := up
			variants = append(variants, candidateVariant{
				id:   fmt.Sprintf("cand-conn-expand-%s-it%d", upID, iteration),
				desc: fmt.Sprintf("Expand connection pool for %s", upID),
				mutate: func(cfg *sim.SystemConfig) []ParameterChange {
					orig := cfg.ConnectionPools[upID]
					proposed := orig + (2 * iteration)
					if proposed > 20 {
						proposed = 20
					}
					if cfg.ConnectionPools == nil {
						cfg.ConnectionPools = make(map[domain.UpstreamID]int)
					}
					cfg.ConnectionPools[upID] = proposed
					return []ParameterChange{{
						Parameter:     fmt.Sprintf("connection_pools.%s", upID),
						OriginalValue: orig,
						ProposedValue: proposed,
						Reason:        fmt.Sprintf("Increase connection pool concurrency for %s to match worker capacity", upID),
					}}
				},
			})
		}

	case SymptomQueueCongestion:
		for tID, tc := range baseCfg.Tenants {
			tenantID := tID
			origArrival := tc.MaxArrivalQueueDepth
			variants = append(variants, candidateVariant{
				id:   fmt.Sprintf("cand-queue-expand-%s-it%d", tenantID, iteration),
				desc: fmt.Sprintf("Expand arrival queue capacity for tenant %s", tenantID),
				mutate: func(cfg *sim.SystemConfig) []ParameterChange {
					proposedArrival := origArrival + (2 * iteration)
					if proposedArrival > 50 {
						proposedArrival = 50
					}
					tCfg := cfg.Tenants[tenantID]
					tCfg.MaxArrivalQueueDepth = proposedArrival
					tCfg.MaxTotalQueueDepth = proposedArrival + tCfg.MaxRetryQueueDepth
					cfg.Tenants[tenantID] = tCfg
					return []ParameterChange{{
						Parameter:     fmt.Sprintf("tenants.%s.max_arrival_queue_depth", tenantID),
						OriginalValue: origArrival,
						ProposedValue: proposedArrival,
						Reason:        fmt.Sprintf("Expand arrival queue depth for tenant %s to accommodate degradation bursts", tenantID),
					}}
				},
			})
		}

	case SymptomPrematureBreakerTrip:
		variants = append(variants, candidateVariant{
			id:   fmt.Sprintf("cand-breaker-thresh-it%d", iteration),
			desc: "Raise breaker failure threshold",
			mutate: func(cfg *sim.SystemConfig) []ParameterChange {
				orig := cfg.Breaker.FailureThreshold
				proposed := orig + iteration
				if proposed > 15 {
					proposed = 15
				}
				cfg.Breaker.FailureThreshold = proposed
				return []ParameterChange{{
					Parameter:     "breaker.failure_threshold",
					OriginalValue: orig,
					ProposedValue: proposed,
					Reason:        "Prevent premature tripping on brief transient degradation spikes",
				}}
			},
		})

	default:
		// Default tuning: tune worker pool or timeout
		variants = append(variants, candidateVariant{
			id:   fmt.Sprintf("cand-timeout-tune-it%d", iteration),
			desc: "Tune attempt timeout",
			mutate: func(cfg *sim.SystemConfig) []ParameterChange {
				orig := cfg.AttemptTimeout
				proposed := orig + domain.SimDuration(2_000_000*int64(iteration))
				cfg.AttemptTimeout = proposed
				return []ParameterChange{{
					Parameter:     "attempt_timeout_ns",
					OriginalValue: int64(orig),
					ProposedValue: int64(proposed),
					Reason:        "Provide slack for execution during degradation",
				}}
			},
		})
	}

	return variants
}

// evaluateCandidate evaluates a single candidate configuration variant against the simulator and independent verifier.
func (ae *AutonomousExperimenter) evaluateCandidate(baseExp sim.Experiment, v candidateVariant, hyp Hypothesis, inv *InvestigationReport) Candidate {
	// 1. Clone baseline configuration and apply mutation
	candCfg := baseExp.Config
	// Deep copy maps
	if baseExp.Config.ConnectionPools != nil {
		candCfg.ConnectionPools = make(map[domain.UpstreamID]int)
		for k, val := range baseExp.Config.ConnectionPools {
			candCfg.ConnectionPools[k] = val
		}
	}
	if baseExp.Config.Tenants != nil {
		candCfg.Tenants = make(map[domain.TenantID]sim.TenantConfig)
		for k, val := range baseExp.Config.Tenants {
			candCfg.Tenants[k] = val
		}
	}

	changes := v.mutate(&candCfg)

	cand := Candidate{
		CandidateID: v.id,
		Config:      candCfg,
	}

	// 2. Validate technical boundaries
	if err := ValidateBoundaries(changes); err != nil {
		cand.Rejected = true
		cand.RejectionReason = fmt.Sprintf("boundary violation: %v", err)
		return cand
	}

	// 3. Build candidate proposal
	pb := NewProposalBuilder(fmt.Sprintf("prop-%s", v.id), ae.authorID, inv, hyp)
	for _, c := range changes {
		pb.AddChange(c.Parameter, c.OriginalValue, c.ProposedValue, c.Reason)
	}
	prop, err := pb.Build()
	if err != nil {
		cand.Rejected = true
		cand.RejectionReason = fmt.Sprintf("proposal schema validation failed: %v", err)
		return cand
	}
	cand.Proposal = prop

	// 4. Run deterministic simulation under identical conditions (same workload, faults, horizon)
	candExp := baseExp
	candExp.Name = fmt.Sprintf("%s-%s", baseExp.Name, v.id)
	candExp.Config = candCfg

	res := sim.RunExperiment(candExp)
	cand.RunResult = &res
	cand.Summary = &res.Summary

	// 5. Serialize to verifier document
	doc, err := evidence.FromRun(res)
	if err != nil {
		cand.Rejected = true
		cand.RejectionReason = fmt.Sprintf("evidence conversion failed: %v", err)
		return cand
	}

	// 6. Independent Verifier Adjudication
	verdict := verify.Verify(doc)
	cand.Verdict = &verdict

	// Invariant check: Fail or InvalidInput MUST be rejected immediately!
	if verdict.Status == verify.StatusFail {
		cand.Rejected = true
		cand.RejectionReason = fmt.Sprintf("independent verifier detected invariant failure (%d findings)", len(verdict.Findings))
		cand.Findings = verdict.Findings
		return cand
	}
	if verdict.Status == verify.StatusInvalidInput {
		cand.Rejected = true
		cand.RejectionReason = fmt.Sprintf("independent verifier rejected evidence as INVALID_INPUT (%d findings)", len(verdict.Findings))
		cand.Findings = verdict.Findings
		return cand
	}

	// For Inconclusive: check if any required safety invariant failed
	hasFailedSafetyProperty := false
	for _, p := range verdict.Properties {
		if p.Status == verify.StatusFail {
			hasFailedSafetyProperty = true
			break
		}
	}
	if hasFailedSafetyProperty {
		cand.Rejected = true
		cand.RejectionReason = "independent verifier property check failed"
		cand.Findings = verdict.Findings
		return cand
	}

	// 7. Surviving candidate: compute multi-objective score
	cand.Score = ScoreCandidate(&res.Summary, ae.weights)
	return cand
}

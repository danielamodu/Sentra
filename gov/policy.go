package gov

import (
	"fmt"
	"math"
	"strings"

	"sentra/ops"
)

// RangeSpec defines min, max, and max percentage delta allowed for a parameter.
type RangeSpec struct {
	Min             float64
	Max             float64
	MaxDeltaPercent float64
}

// Policy specifies machine-enforceable governance rules for configuration changes.
type Policy struct {
	Version                      string               `json:"version"`
	AllowedRanges                map[string]RangeSpec `json:"allowed_ranges"`
	RequiredProperties           []string             `json:"required_properties"`
	RequireHeadCompleteness      bool                 `json:"require_head_completeness"`
	RequireTerminalCompleteness  bool                 `json:"require_terminal_completeness"`
	RequiredApproverRole         string               `json:"required_approver_role"`
	MaxExperimentBudget          ops.ExperimentBudget `json:"max_experiment_budget"`
	ProtectedPrefixes            []string             `json:"protected_prefixes"`
	MinGoodputImprovementPercent float64              `json:"min_goodput_improvement_percent"`
}

// DefaultPolicy returns the production governance policy for Sentra.
func DefaultPolicy() Policy {
	return Policy{
		Version: "gov-v1.0",
		AllowedRanges: map[string]RangeSpec{
			"breaker.failure_threshold": {
				Min:             1,
				Max:             25,
				MaxDeltaPercent: 200.0,
			},
			"breaker.cooldown_ns": {
				Min:             1_000_000,     // 1ms
				Max:             5_000_000_000, // 5s
				MaxDeltaPercent: 200.0,
			},
			"breaker.max_half_open_probes": {
				Min:             1,
				Max:             10,
				MaxDeltaPercent: 200.0,
			},
			"retry_budget.capacity": {
				Min:             0,
				Max:             50,
				MaxDeltaPercent: 100.0,
			},
			"retry_backoff_ns": {
				Min:             0,
				Max:             3_000_000_000, // 3s
				MaxDeltaPercent: 300.0,
			},
			"attempt_timeout_ns": {
				Min:             5_000_000,      // 5ms
				Max:             45_000_000_000, // 45s
				MaxDeltaPercent: 100.0,
			},
			"worker_pool.size": {
				Min:             4,
				Max:             64,
				MaxDeltaPercent: 50.0,
			},
		},
		RequiredProperties: []string{
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
		RequireHeadCompleteness:     true,
		RequireTerminalCompleteness: true,
		RequiredApproverRole:        "infrastructure-lead",
		MaxExperimentBudget:         ops.DefaultBudget(),
		ProtectedPrefixes: []string{
			"verifier.",
			"verify.",
			"governance.",
			"policy.",
			"auth.",
			"scenarios.",
			"benchmark.",
			"evidence.",
			"deployment.",
		},
		MinGoodputImprovementPercent: 0.0, // proposal must not decrease baseline goodput
	}
}

// toFloat64 converts numeric values to float64 for comparison.
func toFloat64(v any) (float64, error) {
	switch val := v.(type) {
	case int:
		return float64(val), nil
	case int32:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case float64:
		return val, nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}

// ValidateProposalPolicy validates an operational proposal against the machine-enforceable governance policy.
func (p Policy) ValidateProposalPolicy(prop *ops.Proposal) error {
	if prop == nil {
		return fmt.Errorf("%w: proposal is nil", ErrPolicyViolation)
	}

	for _, c := range prop.Changes {
		param := strings.TrimSpace(c.Parameter)

		// 1. Protected path check
		for _, pref := range p.ProtectedPrefixes {
			if strings.HasPrefix(strings.ToLower(param), pref) {
				return fmt.Errorf("%w: parameter %q touches protected governance prefix %q",
					ErrPolicyViolation, param, pref)
			}
		}

		// 2. Allowed range and delta check
		spec, ok := p.AllowedRanges[param]
		if !ok {
			// Check parameterized connection_pools.<up>
			if strings.HasPrefix(param, "connection_pools.") {
				spec = RangeSpec{Min: 1, Max: 50, MaxDeltaPercent: 200.0}
				ok = true
			} else if strings.HasPrefix(param, "tenants.") {
				spec = RangeSpec{Min: 1, Max: 200, MaxDeltaPercent: 200.0}
				ok = true
			}
		}

		if !ok {
			return fmt.Errorf("%w: parameter %q is not permitted by policy version %s",
				ErrPolicyViolation, param, p.Version)
		}

		origVal, err := toFloat64(c.OriginalValue)
		if err != nil {
			return fmt.Errorf("%w: original value for %s: %v", ErrPolicyViolation, param, err)
		}
		propVal, err := toFloat64(c.ProposedValue)
		if err != nil {
			return fmt.Errorf("%w: proposed value for %s: %v", ErrPolicyViolation, param, err)
		}

		if propVal < spec.Min || propVal > spec.Max {
			return fmt.Errorf("%w: proposed value %.1f for %s out of policy range [%.1f, %.1f]",
				ErrPolicyViolation, propVal, param, spec.Min, spec.Max)
		}

		// 3. Max delta percent check
		if origVal > 0 && spec.MaxDeltaPercent > 0 {
			deltaPercent := (math.Abs(propVal-origVal) / origVal) * 100.0
			if deltaPercent > spec.MaxDeltaPercent {
				return fmt.Errorf("%w: proposed delta %.1f%% for %s exceeds max allowed delta %.1f%%",
					ErrPolicyViolation, deltaPercent, param, spec.MaxDeltaPercent)
			}
		}
	}

	return nil
}

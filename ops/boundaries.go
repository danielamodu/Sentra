package ops

import (
	"fmt"
	"strings"

	"sentra/domain"
	"sentra/sim"
)

// AllowedParamSpec defines the bounds for an editable operational parameter.
type AllowedParamSpec struct {
	Name        string
	Type        string // "int", "duration"
	MinInt      int64
	MaxInt      int64
	Description string
}

// allowedParameters defines the strict allowlist of operational knobs the agent is permitted to tune.
var allowedParameters = map[string]AllowedParamSpec{
	"breaker.failure_threshold": {
		Name:        "breaker.failure_threshold",
		Type:        "int",
		MinInt:      1,
		MaxInt:      50,
		Description: "Consecutive failures before circuit breaker trips open",
	},
	"breaker.cooldown_ns": {
		Name:        "breaker.cooldown_ns",
		Type:        "duration",
		MinInt:      1_000_000,      // 1ms
		MaxInt:      10_000_000_000, // 10s
		Description: "Duration breaker remains open before probing half-open",
	},
	"breaker.max_half_open_probes": {
		Name:        "breaker.max_half_open_probes",
		Type:        "int",
		MinInt:      1,
		MaxInt:      20,
		Description: "Maximum concurrent probe permits in half-open state",
	},
	"retry_budget.capacity": {
		Name:        "retry_budget.capacity",
		Type:        "int",
		MinInt:      0,
		MaxInt:      100,
		Description: "Maximum token capacity of the retry budget bucket",
	},
	"retry_budget.refill_interval_ns": {
		Name:        "retry_budget.refill_interval_ns",
		Type:        "duration",
		MinInt:      1_000_000,      // 1ms
		MaxInt:      10_000_000_000, // 10s
		Description: "Refill period for retry budget tokens",
	},
	"retry_backoff_ns": {
		Name:        "retry_backoff_ns",
		Type:        "duration",
		MinInt:      0,
		MaxInt:      5_000_000_000, // 5s
		Description: "Delay before dispatching a scheduled retry",
	},
	"attempt_timeout_ns": {
		Name:        "attempt_timeout_ns",
		Type:        "duration",
		MinInt:      1_000_000,      // 1ms
		MaxInt:      60_000_000_000, // 60s
		Description: "Per-attempt timeout deadline",
	},
	"worker_pool.size": {
		Name:        "worker_pool.size",
		Type:        "int",
		MinInt:      1,
		MaxInt:      200,
		Description: "Size of the concurrency worker pool",
	},
}

// protectedPrefixes defines strictly forbidden paths that the agent must never touch.
var protectedPrefixes = []string{
	"verifier.",
	"verify.",
	"governance.",
	"policy.",
	"auth.",
	"scenarios.",
	"benchmark.",
	"evidence.digest",
	"evidence.integrity",
	"deployment.",
	"production.",
	"cluster.",
	"root.",
	"admin.",
}

// IsParameterProtected checks if a parameter string attempts to modify protected domains.
func IsParameterProtected(param string) bool {
	lower := strings.ToLower(strings.TrimSpace(param))
	for _, p := range protectedPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// IsParameterAllowed checks if a parameter is on the approved allowlist.
// Handles parameterized templates like connection_pools.<upstream> and tenants.<tenant>.<queue>.
func IsParameterAllowed(param string) (AllowedParamSpec, bool) {
	p := strings.TrimSpace(param)
	if spec, ok := allowedParameters[p]; ok {
		return spec, true
	}

	// Dynamic check for connection_pools.<upstream>
	if strings.HasPrefix(p, "connection_pools.") {
		parts := strings.Split(p, ".")
		if len(parts) == 2 && parts[1] != "" {
			return AllowedParamSpec{
				Name:        p,
				Type:        "int",
				MinInt:      1,
				MaxInt:      100,
				Description: fmt.Sprintf("Connection pool capacity for upstream %s", parts[1]),
			}, true
		}
	}

	// Dynamic check for tenants.<tenant>.max_arrival_queue_depth, max_retry_queue_depth, max_total_queue_depth
	if strings.HasPrefix(p, "tenants.") {
		parts := strings.Split(p, ".")
		if len(parts) == 3 && parts[1] != "" {
			switch parts[2] {
			case "max_arrival_queue_depth":
				return AllowedParamSpec{
					Name:        p,
					Type:        "int",
					MinInt:      1,
					MaxInt:      500,
					Description: fmt.Sprintf("Max arrival queue depth for tenant %s", parts[1]),
				}, true
			case "max_retry_queue_depth":
				return AllowedParamSpec{
					Name:        p,
					Type:        "int",
					MinInt:      0,
					MaxInt:      500,
					Description: fmt.Sprintf("Max retry queue depth for tenant %s", parts[1]),
				}, true
			case "max_total_queue_depth":
				return AllowedParamSpec{
					Name:        p,
					Type:        "int",
					MinInt:      1,
					MaxInt:      1000,
					Description: fmt.Sprintf("Max total queue depth for tenant %s", parts[1]),
				}, true
			}
		}
	}

	return AllowedParamSpec{}, false
}

// toInt64 safely converts numeric types to int64.
func toInt64(v any) (int64, error) {
	switch val := v.(type) {
	case int:
		return int64(val), nil
	case int32:
		return int64(val), nil
	case int64:
		return val, nil
	case float64:
		return int64(val), nil
	case domain.SimTime:
		return int64(val), nil
	case domain.SimDuration:
		return int64(val), nil
	default:
		return 0, fmt.Errorf("unsupported numeric type %T for value %v", v, v)
	}
}

// ValidateParameterChange validates a single proposed parameter change against technical boundaries.
func ValidateParameterChange(c ParameterChange) error {
	if IsParameterProtected(c.Parameter) {
		return fmt.Errorf("parameter %q is in a protected path; agent modification is strictly prohibited", c.Parameter)
	}

	spec, ok := IsParameterAllowed(c.Parameter)
	if !ok {
		return fmt.Errorf("parameter %q is not in the agent operational allowlist", c.Parameter)
	}

	val, err := toInt64(c.ProposedValue)
	if err != nil {
		return fmt.Errorf("invalid proposed value for %s: %w", c.Parameter, err)
	}

	if val < spec.MinInt || val > spec.MaxInt {
		return fmt.Errorf("proposed value %d for %s is out of allowed range [%d, %d]", val, c.Parameter, spec.MinInt, spec.MaxInt)
	}

	return nil
}

// ValidateBoundaries validates an entire slice of parameter changes.
func ValidateBoundaries(changes []ParameterChange) error {
	if len(changes) == 0 {
		return fmt.Errorf("proposal must contain at least one parameter change")
	}
	seen := make(map[string]bool)
	for _, c := range changes {
		if seen[c.Parameter] {
			return fmt.Errorf("duplicate parameter change for %q", c.Parameter)
		}
		seen[c.Parameter] = true
		if err := ValidateParameterChange(c); err != nil {
			return err
		}
	}
	return nil
}

// ApplyChangeToConfig applies a validated parameter change to a SystemConfig copy.
func ApplyChangeToConfig(cfg *sim.SystemConfig, c ParameterChange) error {
	val, err := toInt64(c.ProposedValue)
	if err != nil {
		return err
	}

	p := strings.TrimSpace(c.Parameter)
	switch p {
	case "breaker.failure_threshold":
		cfg.Breaker.FailureThreshold = int(val)
	case "breaker.cooldown_ns":
		cfg.Breaker.Cooldown = domain.SimDuration(val)
	case "breaker.max_half_open_probes":
		cfg.Breaker.MaxHalfOpenProbes = int(val)
	case "retry_budget.capacity":
		cfg.RetryBudget.Capacity = int(val)
	case "retry_budget.refill_interval_ns":
		cfg.RetryBudget.RefillInterval = domain.SimDuration(val)
	case "retry_backoff_ns":
		cfg.RetryBackoff = domain.SimDuration(val)
	case "attempt_timeout_ns":
		cfg.AttemptTimeout = domain.SimDuration(val)
	case "worker_pool.size":
		cfg.WorkerPool.Size = int(val)
	default:
		if strings.HasPrefix(p, "connection_pools.") {
			parts := strings.Split(p, ".")
			upID := domain.UpstreamID(parts[1])
			if cfg.ConnectionPools == nil {
				cfg.ConnectionPools = make(map[domain.UpstreamID]int)
			}
			cfg.ConnectionPools[upID] = int(val)
			return nil
		}
		if strings.HasPrefix(p, "tenants.") {
			parts := strings.Split(p, ".")
			tID := domain.TenantID(parts[1])
			tc := cfg.Tenants[tID]
			switch parts[2] {
			case "max_arrival_queue_depth":
				tc.MaxArrivalQueueDepth = int(val)
			case "max_retry_queue_depth":
				tc.MaxRetryQueueDepth = int(val)
			case "max_total_queue_depth":
				tc.MaxTotalQueueDepth = int(val)
			default:
				return fmt.Errorf("unknown tenant field: %s", parts[2])
			}
			// Maintain tenant invariant: total >= arrival and total >= retry
			if tc.MaxTotalQueueDepth < tc.MaxArrivalQueueDepth {
				tc.MaxTotalQueueDepth = tc.MaxArrivalQueueDepth + tc.MaxRetryQueueDepth
			}
			cfg.Tenants[tID] = tc
			return nil
		}
		return fmt.Errorf("unhandled allowed parameter: %s", p)
	}
	return nil
}

package ops

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"sentra/domain"
	"sentra/sim"
)

// InvestigationReport holds the extracted observations and operational hypotheses
// derived from analyzing a simulation run result.
type InvestigationReport struct {
	ScenarioName string                `json:"scenario_name"`
	Observations []Observation         `json:"observations"`
	Hypotheses   []Hypothesis          `json:"hypotheses"`
	Summary      sim.ExperimentSummary `json:"summary"`
}

// Investigate analyzes a simulation RunResult, extracts factual observations,
// identifies degradation symptoms, and forms explicit, evidence-backed hypotheses.
func Investigate(res *sim.RunResult) (*InvestigationReport, error) {
	if res == nil {
		return nil, fmt.Errorf("cannot investigate nil run result")
	}

	sum := res.Summary
	var obs []Observation

	// 1. Request outcome observations
	obs = append(obs, Observation{
		Metric:      "OfferedRequests",
		Value:       float64(sum.Offered),
		Unit:        "requests",
		Context:     "total logical request arrivals",
		EvidenceRef: "summary.offered",
	})
	obs = append(obs, Observation{
		Metric:      "AdmittedRequests",
		Value:       float64(sum.Admitted),
		Unit:        "requests",
		Context:     "admitted requests",
		EvidenceRef: "summary.admitted",
	})
	obs = append(obs, Observation{
		Metric:      "GoodputRequests",
		Value:       float64(sum.Completed),
		Unit:        "requests",
		Context:     "completed requests meeting logical deadline",
		EvidenceRef: "summary.completed",
	})
	obs = append(obs, Observation{
		Metric:      "FailedRequests",
		Value:       float64(sum.Failed),
		Unit:        "requests",
		Context:     "requests failed",
		EvidenceRef: "summary.failed",
	})
	obs = append(obs, Observation{
		Metric:      "ExpiredRequests",
		Value:       float64(sum.Expired),
		Unit:        "requests",
		Context:     "requests expired before completion",
		EvidenceRef: "summary.expired",
	})
	obs = append(obs, Observation{
		Metric:      "ShedRequests",
		Value:       float64(sum.Shed),
		Unit:        "requests",
		Context:     "requests load-shed at admission",
		EvidenceRef: "summary.shed",
	})

	// Shed reason breakdown
	breakerShedCount := 0
	queueShedCount := 0
	for _, ts := range sum.Tenant {
		for reason, cnt := range ts.ShedReasons {
		if strings.Contains(reason, "breaker") {
			breakerShedCount += cnt
		} else if strings.Contains(reason, "queue") {
			queueShedCount += cnt
		}
	}
	}
	if breakerShedCount > 0 {
		obs = append(obs, Observation{
			Metric:      "BreakerShedRequests",
			Value:       float64(breakerShedCount),
			Unit:        "requests",
			Context:     "requests load-shed due to circuit breaker trip or probe saturation",
			EvidenceRef: "summary.tenant.shed_reasons.breaker",
		})
	}
	if queueShedCount > 0 {
		obs = append(obs, Observation{
			Metric:      "QueueShedRequests",
			Value:       float64(queueShedCount),
			Unit:        "requests",
			Context:     "requests load-shed due to tenant queue depth saturation",
			EvidenceRef: "summary.tenant.shed_reasons.queue",
		})
	}

	// 2. Retry and wasted work observations
	if !math.IsNaN(sum.WastedShare) {
		obs = append(obs, Observation{
			Metric:      "WastedShare",
			Value:       sum.WastedShare,
			Unit:        "ratio",
			Context:     "wasted attempts / determined attempts",
			EvidenceRef: "summary.wasted_share",
		})
	}
	obs = append(obs, Observation{
		Metric:      "AttemptsWasted",
		Value:       float64(sum.AttemptsWasted),
		Unit:        "attempts",
		Context:     "physical attempts finished as wasted work",
		EvidenceRef: "summary.attempts_wasted",
	})
	obs = append(obs, Observation{
		Metric:      "WorkAfterExpiry",
		Value:       float64(sum.WorkAfterExpiry),
		Unit:        "attempts",
		Context:     "attempts executed after logical request deadline expired",
		EvidenceRef: "summary.work_after_expiry",
	})
	if !math.IsNaN(sum.Amplification) {
		obs = append(obs, Observation{
			Metric:      "RetryAmplification",
			Value:       sum.Amplification,
			Unit:        "ratio",
			Context:     "physical attempts / logical admitted requests",
			EvidenceRef: "summary.retry_amplification",
		})
	}

	// 3. Resource occupancy observations
	if !math.IsNaN(sum.WorkerOcc.Util) {
		obs = append(obs, Observation{
			Metric:      "WorkerUtilization",
			Value:       sum.WorkerOcc.Util,
			Unit:        "ratio",
			Context:     "time-averaged worker utilization",
			EvidenceRef: "summary.worker_occ.util",
		})
	}
	obs = append(obs, Observation{
		Metric:      "WorkerPeakBusy",
		Value:       float64(sum.WorkerOcc.Peak),
		Unit:        "workers",
		Context:     "peak concurrent busy workers",
		EvidenceRef: "summary.worker_occ.peak",
	})

	for up, occ := range sum.ConnOcc {
		if !math.IsNaN(occ.Util) {
			obs = append(obs, Observation{
				Metric:      fmt.Sprintf("ConnectionUtilization_%s", up),
				Value:       occ.Util,
				Unit:        "ratio",
				Context:     fmt.Sprintf("time-averaged connection utilization for %s", up),
				EvidenceRef: fmt.Sprintf("summary.conn_occ[%s].util", up),
			})
		}
	}

	// 4. Latency observations
	if sum.QueueWait.N > 0 {
		obs = append(obs, Observation{
			Metric:      "QueueWaitP90NS",
			Value:       float64(sum.QueueWait.P90),
			Unit:        "nanoseconds",
			Context:     "90th percentile queue waiting time",
			EvidenceRef: "summary.queue_wait.p90",
		})
		obs = append(obs, Observation{
			Metric:      "QueueWaitP99NS",
			Value:       float64(sum.QueueWait.P99),
			Unit:        "nanoseconds",
			Context:     "99th percentile queue waiting time",
			EvidenceRef: "summary.queue_wait.p99",
		})
	}

	// Now synthesize explicit operational hypotheses from observed patterns
	var hypotheses []Hypothesis

	// Hypothesis 1: Circuit Breaker Sensitivity & Recovery
	// If breaker shedding or breaker opening occurred, identify the affected upstream deterministically
	var upstreams []string
	for up := range res.Experiment.Config.Upstreams {
		upstreams = append(upstreams, string(up))
	}
	sort.Strings(upstreams)

	var degradedUpstream domain.UpstreamID
	for _, upStr := range upstreams {
		up := domain.UpstreamID(upStr)
		if res.FinalState != nil && res.FinalState.Breakers != nil {
			if br, ok := res.FinalState.Breakers[up]; ok && br != nil {
				if br.State != domain.BreakerClosed || br.ConsecutiveFailures > 0 {
					degradedUpstream = up
					break
				}
			}
		}
	}
	if degradedUpstream == "" && len(upstreams) > 0 {
		degradedUpstream = domain.UpstreamID(upstreams[0])
	}

	breakerActive := false
	if degradedUpstream != "" && res.FinalState != nil && res.FinalState.Breakers != nil {
		if br, ok := res.FinalState.Breakers[degradedUpstream]; ok && br != nil {
			if br.State != domain.BreakerClosed || br.ConsecutiveFailures > 0 {
				breakerActive = true
			}
		}
	}

	if breakerActive || breakerShedCount > 0 || sum.Failed > 0 {
		var sup []Observation
		for _, o := range obs {
			if o.Metric == "FailedRequests" || o.Metric == "ShedRequests" || o.Metric == "BreakerShedRequests" {
				sup = append(sup, o)
			}
		}
		hypotheses = append(hypotheses, Hypothesis{
			ID:                     fmt.Sprintf("HYP-BREAKER-STABILITY-%s-%s", res.Experiment.Name, degradedUpstream),
			Symptom:                SymptomPrematureBreakerTrip,
			Description:            fmt.Sprintf("Upstream %s breaker behavior is shedding requests; tuning cooldown or failure threshold can accelerate goodput recovery.", degradedUpstream),
			SupportingObservations: sup,
			CandidateParameters: []string{
				"breaker.cooldown_ns",
				"breaker.failure_threshold",
				"breaker.max_half_open_probes",
			},
			Rationale: "Reducing breaker cooldown allows earlier recovery probing when degraded upstreams heal, while raising failure threshold prevents premature tripping on transient spikes.",
		})
	}

	// Hypothesis 2: Wasted Work & Retry Storm
	if sum.WastedShare > 0.10 || sum.Amplification > 1.25 || sum.WorkAfterExpiry > 0 {
		var sup []Observation
		for _, o := range obs {
			if o.Metric == "WastedShare" || o.Metric == "RetryAmplification" || o.Metric == "WorkAfterExpiry" {
				sup = append(sup, o)
			}
		}
		hypotheses = append(hypotheses, Hypothesis{
			ID:                     fmt.Sprintf("HYP-RETRY-STORM-%s", res.Experiment.Name),
			Symptom:                SymptomWastedWorkElevated,
			Description:            "Aggressive retry scheduling during degraded upstream intervals is amplifying attempt volume and generating wasted execution work.",
			SupportingObservations: sup,
			CandidateParameters: []string{
				"retry_budget.capacity",
				"retry_backoff_ns",
				"attempt_timeout_ns",
			},
			Rationale: "Increasing retry backoff or tightening the retry budget reduces attempt amplification during degradation, preventing workers from processing obsolete retries.",
		})
	}

	// Hypothesis 3: Connection Pool Bottleneck / Stranding
	for up, occ := range sum.ConnOcc {
		if occ.Util > 0.80 || (occ.Peak >= 6 && sum.WorkerOcc.Util > 0.70) {
			var sup []Observation
			for _, o := range obs {
				if o.Metric == fmt.Sprintf("ConnectionUtilization_%s", up) || o.Metric == "WorkerUtilization" {
					sup = append(sup, o)
				}
			}
			hypotheses = append(hypotheses, Hypothesis{
				ID:                     fmt.Sprintf("HYP-CONN-STRANDING-%s-%s", res.Experiment.Name, up),
				Symptom:                SymptomConnectionStranding,
				Description:            fmt.Sprintf("High connection pool utilization on upstream %s is causing assigned workers to wait for connections.", up),
				SupportingObservations: sup,
				CandidateParameters: []string{
					fmt.Sprintf("connection_pools.%s", up),
					"attempt_timeout_ns",
				},
				Rationale: fmt.Sprintf("Increasing connection pool capacity for %s or reducing attempt timeout frees workers and prevents thread starvation.", up),
			})
		}
	}

	// Hypothesis 4: Queue Congestion & Cascading Timeouts
	// Only form queue congestion hypothesis if actual queue drops or high queue wait occurred
	if queueShedCount > 0 || (sum.QueueWait.N > 0 && float64(sum.QueueWait.P99) > 10_000_000) { // 10ms
		var sup []Observation
		for _, o := range obs {
			if o.Metric == "QueueShedRequests" || o.Metric == "ShedRequests" || o.Metric == "QueueWaitP99NS" || o.Metric == "ExpiredRequests" {
				sup = append(sup, o)
			}
		}
		var tenantParams []string
		for tid := range res.Experiment.Config.Tenants {
			tenantParams = append(tenantParams,
				fmt.Sprintf("tenants.%s.max_arrival_queue_depth", tid),
				fmt.Sprintf("tenants.%s.max_total_queue_depth", tid),
			)
		}
		hypotheses = append(hypotheses, Hypothesis{
			ID:                     fmt.Sprintf("HYP-QUEUE-CONGESTION-%s", res.Experiment.Name),
			Symptom:                SymptomQueueCongestion,
			Description:            "Excessive queue wait times or queue arrival drops indicate queue admission limits and drain rates are misconfigured relative to arrival bursts.",
			SupportingObservations: sup,
			CandidateParameters:    tenantParams,
			Rationale:              "Rebalancing tenant arrival queue depths accommodates short bursts without allowing backlogs to age past logical deadlines.",
		})
	}



	return &InvestigationReport{
		ScenarioName: res.Experiment.Name,
		Observations: obs,
		Hypotheses:   hypotheses,
		Summary:      sum,
	}, nil
}

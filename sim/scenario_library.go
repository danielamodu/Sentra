package sim

// scenario_library.go defines the ten M2 scenarios A–J as recorded data
// (workstream 9). Every scenario derives from ONE shared config factory
// (experimentCfg) and composes workload components out of the same helpers,
// so control comparisons hold initial conditions identical (workstream 10).
//
// Design rule (workstream 10): a scenario DEFINES inputs and states its
// hypothesis as text; it does not hard-code parameter regions that
// guarantee a particular classification. The observed outputs live in
// RunResult; docs/MILESTONE_2_REPORT.md records the runs it actually
// executed.

import (
	"sentra/domain"
)

// ms is one simulated millisecond as a duration unit; msT is the same as a
// time-point unit. Shared between the scenario library and the tests.
const (
	ms  = domain.Millisecond
	msT = domain.SimTime(domain.Millisecond)
)

// Experiment tenants/upstreams — the reference topology of M1.
const (
	t1 = domain.TenantID("t1")
	t2 = domain.TenantID("t2")
	t3 = domain.TenantID("t3")
	u1 = domain.UpstreamID("u1") // critical upstream
	u2 = domain.UpstreamID("u2") // standard upstream
)

// experimentCfg is the shared initial-conditions shape of every scenario:
// 3 shared workers, 2 connections per upstream, 3 tenants with symmetric
// lane bounds, one retry budget of 3 tokens per (tenant, upstream) refilling
// every 100ms, 2ms retry backoff, 25ms attempt timeout, and a classic
// three-state breaker opening after 2 consecutive failures with a 50ms
// cooldown and one half-open probe.
func experimentCfg() SystemConfig {
	return SystemConfig{
		WorkerPool: WorkerPoolConfig{ID: "pool-main", Size: 3},
		ConnectionPools: map[domain.UpstreamID]int{
			u1: 2,
			u2: 2,
		},
		Upstreams: map[domain.UpstreamID]UpstreamConfig{
			u1: {Criticality: domain.CriticalityCritical, NormalLatency: 5 * ms, DegradedLatency: 20 * ms},
			u2: {Criticality: domain.CriticalityStandard, NormalLatency: 10 * ms, DegradedLatency: 40 * ms},
		},
		Tenants: map[domain.TenantID]TenantConfig{
			t1: {MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8},
			t2: {MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8},
			t3: {MaxArrivalQueueDepth: 8, MaxRetryQueueDepth: 8, MaxTotalQueueDepth: 8},
		},
		RetryBudget:    RetryBudgetConfig{Capacity: 3, RefillInterval: 100 * ms},
		RetryBackoff:   2 * ms,
		AttemptTimeout: 25 * ms,
		Breaker:        BreakerConfig{FailureThreshold: 2, Cooldown: 50 * ms, MaxHalfOpenProbes: 1},
	}
}

const scenarioSeed = 20261026

// comp builds one constant-rate, evenly-spaced arrival component with a fixed
// relative deadline (DeadlineMin == DeadlineMax) until the horizon.
func comp(tid domain.TenantID, up domain.UpstreamID, rate float64, deadline domain.SimDuration, retries int) ArrivalComponent {
	return ArrivalComponent{
		Tenant:      tid,
		Upstream:    up,
		Rate:        rate,
		Window:      2 * ms,
		DeadlineMin: deadline,
		DeadlineMax: deadline,
		MaxRetries:  retries,
		Jitter:      0,
	}
}

// compUntil bounds a component to end at `until` in simulated time
// ([0, until)).
func compUntil(c ArrivalComponent, until domain.SimTime) ArrivalComponent {
	c.Until = until
	return c
}

// compFrom starts a component at `from` in simulated time ([from, horizon)).
func compFrom(c ArrivalComponent, from domain.SimTime) ArrivalComponent {
	c.From = from
	return c
}

// ---------------------------------------------------------------------------
// A. Healthy baseline (control scenario)
// ---------------------------------------------------------------------------

// ScenarioA is the control: no faults, light traffic well under capacity.
// It anchors all comparisons (same config/workload shape, zero fault input).
// Hypothesis: full deadline-respecting goodput, near-empty queues, workers
// partially busy (~58%), no retries, no sheds.
func ScenarioA() Experiment {
	return Experiment{
		Name: "A-healthy-baseline",
		Hypothesis: "Control run: light rate (0.15/ms on u1, 0.10/ms on u2) well under the clearing " +
			"capacity of 3x(1/5ms)=0.6/ms for u1 and 3/(10ms)=0.3/ms for u2 (worker demand ≈ " +
			"0.75+1.00 = 1.75 of 3 pools ≈ 58%): every request completes within its 50ms " +
			"deadline; queues stay near-empty; no retries, no sheds.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			comp(t1, u1, 0.3, 50*ms, 1),
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Phases: []Phase{
			{Name: "steady", From: 0, Until: 120 * msT},
		},
		Horizon: 120 * msT,
	}
}

// ---------------------------------------------------------------------------
// B. Ordinary overload without upstream failure (control for collapse claims)
// ---------------------------------------------------------------------------

// ScenarioB runs 2.5x over the u1 goodput capacity for 80ms, then stops. It is
// the control for the ordinary-overload-without-metastability region: sustained
// shedding and backlog DRIVING AROUND, but the system recovers by simple
// drainage once the load stops — no internal feedback loop claim.
func ScenarioB() Experiment {
	return Experiment{
		Name: "B-ordinary-overload",
		Hypothesis: "Rate 1.5/ms on u1 (2.5x its 0.6/ms clearing capacity) for 80ms: admission " +
			"caps saturate, sustained shedding appears, queued requests drain FIFO. When the " +
			"overload stops at 80ms the tail phase [80,120) drains to zero without any fault: " +
			"ordinary overload WITHOUT metastability.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			compUntil(comp(t1, u1, 3.0, 50*ms, 1), 80*msT),
			compUntil(comp(t2, u2, 0.2, 50*ms, 1), 80*msT),
		}},
		Phases: []Phase{
			{Name: "overload", From: 0, Until: 80 * msT},
			{Name: "tail", From: 80 * msT, Until: 120 * msT},
		},
		Horizon: 120 * msT,
	}
}

// ---------------------------------------------------------------------------
// C. U1 degradation while U2 remains healthy
// ---------------------------------------------------------------------------

// ScenarioC makes u1 completely unavailable (M1 degraded semantics: slow AND
// every completion fails) while u2 keeps serving. Hypothesis: the breaker
// opens on u1 and admission sheds u1 arrivals early; u2 goodput continues
// mostly untouched — upstream isolation measured on shared workers (u1's
// stranded 20ms executions can still contend for the pool).
func ScenarioC() Experiment {
	return Experiment{
		Name: "C-u1-degraded-u2-healthy",
		Hypothesis: "u1 fully unavailable during [40,70): u1 completions fail, the breaker opens " +
			"and u1 arrivals shed at admission from opening onward; u2 keeps serving; shared " +
			"workers can transiently hold stranded u1 executions. Post-recovery goodput on u1 " +
			"resumes via breaker cooldown + half-open probes.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			comp(t1, u1, 0.3, 50*ms, 1),
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultUnavailable, From: 40 * msT, Until: 70 * msT},
		},
		Phases: []Phase{
			{Name: "pre", From: 0, Until: 40 * msT},
			{Name: "during", From: 40 * msT, Until: 70 * msT},
			{Name: "immediate", From: 70 * msT, Until: 80 * msT},
			{Name: "post", From: 70 * msT, Until: 120 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 40 * msT, RecAt: 70 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  120 * msT,
	}
}

// ---------------------------------------------------------------------------
// D. U1 degradation with recovery while backlog remains
// ---------------------------------------------------------------------------

// ScenarioD is the central slow-recovery probe: u1 runs SLOW-BUT-SUCCEEDING
// (15ms latency override — no failure, so the breaker never fires) while the
// offered rate exceeds the degraded clearing capacity, so queues accumulate
// with completions still succeeding. The baseline light rate resumes exactly
// when the override clears; the upstream recovers at 70ms with the backlog
// fully intact. Hypothesis: recovery is delayed by drainage but eventual
// within the horizon — no retry amplification feeds the queue and the breaker
// never trips, so the goodput rate recovers while the backlog drains FIFO.
func ScenarioD() Experiment {
	return Experiment{
		Name: "D-u1-slow-recovery-backlog-remains",
		Hypothesis: "u1 latency overridden to 15ms (completions still succeed — BREAKER NEVER " +
			"FIRES) while t1 pushes 1.0 arrivals/2ms (0.5/ms), above the degraded clearing " +
			"capacity of 3x(1/15ms)=0.2/ms, during [40,70): the queue saturates and queued " +
			"requests expire only at their 100ms deadline. Baseline rates resume at 70ms with " +
			"the backlog intact: recovery is delayed by drainage but the goodput rate recovers " +
			"and the backlog drains above the healthy inflow.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			compUntil(comp(t1, u1, 0.3, 100*ms, 1), 40*msT),                   // light pre-load
			compUntil(compFrom(comp(t1, u1, 1.0, 100*ms, 1), 40*msT), 70*msT), // heavy only inside the window
			compFrom(comp(t1, u1, 0.3, 100*ms, 1), 70*msT),                    // light again at recovery
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultSlowLatency, It: 15 * ms, From: 40 * msT, Until: 70 * msT},
		},
		Phases: []Phase{
			{Name: "pre", From: 0, Until: 40 * msT},
			{Name: "during", From: 40 * msT, Until: 70 * msT},
			{Name: "immediate", From: 70 * msT, Until: 80 * msT},
			{Name: "post", From: 70 * msT, Until: 120 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 40 * msT, RecAt: 70 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  120 * msT,
	}
}

// ---------------------------------------------------------------------------
// E. Retry amplification under sustained degradation
// ---------------------------------------------------------------------------

// ScenarioE raises the u1 FAILURE RATE to 50% (every 2nd completion fails) at
// normal latency. Hypothesis: physical attempts exceed logical requests
// (amplification > 1), retry tokens deplete, blocked retries fail requests,
// and the breaker swings to open (consecutive failures) partway through the
// window — the three internal amplifiers interacting.
func ScenarioE() Experiment {
	return Experiment{
		Name: "E-retry-amplification-sustained",
		Hypothesis: "u1 fails every 2nd completion during [20,100) at normal latency: physical " +
			"attempts exceed logical requests (amplification above 1), every retry spends " +
			"exactly one budget token, and requests fail when their retry limit or budget is " +
			"exhausted — retry amplification and budget throttling interacting. The breaker " +
			"stays closed: an every-2nd failure rule produces no consecutive completion " +
			"failures, so the two-failures threshold is never met.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			comp(t1, u1, 0.3, 50*ms, 1),
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultFailRate, EveryN: 2, From: 20 * msT, Until: 100 * msT},
		},
		Phases: []Phase{
			{Name: "pre", From: 0, Until: 20 * msT},
			{Name: "during", From: 20 * msT, Until: 100 * msT},
			{Name: "post", From: 100 * msT, Until: 120 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 20 * msT, RecAt: 100 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  120 * msT,
	}
}

// ---------------------------------------------------------------------------
// F. Worker-pool contention and connection-pool exhaustion
// ---------------------------------------------------------------------------

// ScenarioF injects worker-pool contention through a slow upstream: u1's
// latency is overridden to 20ms (succeeding — NO breaker) while its offered
// rate far exceeds the degraded clearing capacity, and a heavier u2 stream
// joins for the same window. The shared 3-worker pool saturates on long u1
// executions, the u1 connection pool (2) exhausts and pins further workers
// waiting (some abandoned by timeout), and u2 inherits the starvation purely
// via worker-pool contention. Pre and post phases run the healthy baseline
// A rates, so the recovery measurement contrasts with a clean regime.
func ScenarioF() Experiment {
	return Experiment{
		Name: "F-contention-and-conn-exhaustion",
		Hypothesis: "Baseline A rates outside [30,90). Inside the window: u1 latency overridden " +
			"to 20ms (succeeds — NO breaker) under 1.2 arrivals/2ms (6x the degraded clearing " +
			"capacity 0.15/ms) plus u2 raised to 0.6 arrivals/2ms (2x its own 0.3/ms capacity): " +
			"the shared 3-worker pool saturates, both queues fill, u1's 2-connection pool " +
			"exhausts and pins workers waiting (abandoned by timeout). At 90ms baseline rates " +
			"resume with the backlog intact: pools recover by drainage — contention amplifies " +
			"the outage beyond the recovery instant. Best-case arithmetic: the ~16-deep backlog " +
			"demands ~80ms of shared worker time at 5ms per u1 execution, so deadline-respecting " +
			"drainage may not complete inside the 30ms post window — the recovery speed is " +
			"bounded by the backlog, not by the fault duration.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			compUntil(comp(t1, u1, 0.3, 50*ms, 1), 30*msT),                   // healthy pre-load
			compUntil(compFrom(comp(t1, u1, 1.2, 50*ms, 1), 30*msT), 90*msT), // heavy u1 in-window
			compFrom(comp(t1, u1, 0.3, 50*ms, 1), 90*msT),                    // light again after recovery
			compUntil(comp(t2, u2, 0.2, 50*ms, 1), 30*msT),                   // healthy pre-load
			compUntil(compFrom(comp(t2, u2, 0.6, 50*ms, 1), 30*msT), 90*msT), // u2 surge in-window
			compFrom(comp(t2, u2, 0.2, 50*ms, 1), 90*msT),                    // light again after recovery
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultSlowLatency, It: 20 * ms, From: 30 * msT, Until: 90 * msT},
		},
		Phases: []Phase{
			{Name: "pre", From: 0, Until: 30 * msT},
			{Name: "during", From: 30 * msT, Until: 90 * msT},
			{Name: "post", From: 90 * msT, Until: 120 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 30 * msT, RecAt: 90 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  120 * msT,
	}
}

// ---------------------------------------------------------------------------
// G. Cross-tenant interference (FIFO baseline, no alternative scheduling)
// ---------------------------------------------------------------------------

// ScenarioG drives a heavy tenant and a light tenant into the SAME critical
// upstream, over capacity, with no faults. All dispatch is the FIFO baseline
// scheduler. Hypothesis: queue depth and deadline expiries concentrate on t1
// by volume, but t2's dispatch share and useful-work share degrade because its
// attempts age behind t1's; the useful-only Jain index drops below 1 despite
// equal per-tenant lane caps.
func ScenarioG() Experiment {
	return Experiment{
		Name: "G-cross-tenant-interference-fifo-baseline",
		Hypothesis: "t1 pushes 1.0/ms and t2 0.2/ms into u1 (capacity 0.6/ms), no faults, FIFO " +
			"dispatch: t1 dominates volume, but t2's attempts age behind t1's backlog and lose " +
			"deadline-respecting completions; jain(useful) below 1 while the per-tenant lane " +
			"caps are equal — the measurable FIFO baseline for later scheduling milestones.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			compUntil(comp(t1, u1, 2.0, 50*ms, 1), 100*msT),
			compUntil(comp(t2, u1, 0.4, 50*ms, 1), 100*msT),
			comp(t3, u2, 0.4, 50*ms, 1),
		}},
		Phases: []Phase{
			{Name: "steady", From: 0, Until: 60 * msT},
			{Name: "under-load", From: 60 * msT, Until: 100 * msT},
			{Name: "tail", From: 100 * msT, Until: 120 * msT},
		},
		Horizon: 120 * msT,
	}
}

// ---------------------------------------------------------------------------
// H. Circuit-breaker recovery and reopening (workstream 5)
// ---------------------------------------------------------------------------

// ScenarioH fails EVERY u1 completion for 25ms so the breaker opens on two
// consecutive failures; when the fault clears, the breaker must pass through
// its cooldown, allow exactly one half-open probe, see it succeed and close.
// Hypothesis: closed-while-healthy → open-while-failing → half-open probe →
// close; probe completions are Useful; new admissions resume only via the
// half-open gate.
func ScenarioH() Experiment {
	return Experiment{
		Name: "H-breaker-recovery",
		Hypothesis: "u1 fails EVERY completion during [30,55): the breaker accumulates 2 " +
			"consecutive failures and opens ~10ms into the fault; after recover at 55ms it " +
			"stays open until cooldown (50ms), admits exactly ONE half-open probe (a probe " +
			"permit is counted and released exactly once), sees it succeed, and closes — " +
			"workstream 5's full state machine under one fault window.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			comp(t1, u1, 0.3, 50*ms, 1),
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultFailRate, EveryN: 1, From: 30 * msT, Until: 55 * msT},
		},
		Phases: []Phase{
			{Name: "healthy", From: 0, Until: 30 * msT},
			{Name: "failing", From: 30 * msT, Until: 55 * msT},
			{Name: "cooldown", From: 55 * msT, Until: 90 * msT},
			{Name: "probing-and-recovered", From: 90 * msT, Until: 120 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 30 * msT, RecAt: 55 * msT, Policy: RecoveryPolicy{DrainSpan: 10 * ms, GoodputThreshold: 0.5}},
		Horizon:  120 * msT,
	}
}

// ---------------------------------------------------------------------------
// I. Adversarial burst during half-open recovery (workstream 5)
// ---------------------------------------------------------------------------

// ScenarioI repeats H's fault window and adds a 12-arrivals burst from t1 at
// the recovery instant. Hypothesis: the burst lands while the breaker is
// STILL OPEN (cooldown running): the burst sheds via breaker-open; the
// background stream keeps flowing; when the breaker finally probes, the probe
// competes only with a drained burst — burst + half-open gating is visible in
// the shed mix and probe timing.
func ScenarioI() Experiment {
	return Experiment{
		Name: "I-burst-during-halfopen-recovery",
		Hypothesis: "Same as H, plus a 12-arrival t1 burst exactly at 60ms (breaker still in its " +
			"open-cooldown): burst arrivals shed at admission (breaker-open), background t1 " +
			"traffic continues to shed until the breaker closes; the probe sequence is delayed " +
			"only by the inherent cooldown — burst and half-open gating are visible in the " +
			"shed mix, not in the breaker timing.",
		Config: experimentCfg(),
		Workload: WorkloadSpec{
			Seed: scenarioSeed,
			Components: []ArrivalComponent{
				comp(t1, u1, 0.3, 50*ms, 1),
				comp(t2, u2, 0.2, 50*ms, 1),
			},
			Bursts: []Burst{
				{Tenant: t1, Upstream: u1, At: 60 * msT, Count: 12, DeadlineMin: 40 * ms, DeadlineMax: 40 * ms, MaxRetries: 1},
			},
		},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultFailRate, EveryN: 1, From: 30 * msT, Until: 55 * msT},
		},
		Phases: []Phase{
			{Name: "healthy", From: 0, Until: 30 * msT},
			{Name: "failing", From: 30 * msT, Until: 55 * msT},
			{Name: "burst-window", From: 55 * msT, Until: 90 * msT},
			{Name: "recovered", From: 90 * msT, Until: 120 * msT},
		},
		Horizon: 120 * msT,
	}
}

// ---------------------------------------------------------------------------
// J. Multiple degradation and recovery episodes (workstream 2 "multiple")
// ---------------------------------------------------------------------------

// ScenarioJ sequences THREE independent u1 episodes — a hard failure window,
// a latency-only window, and a 50%-fail window — with healthy gaps between
// them. The recovery anchor is placed on the LAST episode's recovery;
// the per-episode phases show the intermediate clean gaps.
func ScenarioJ() Experiment {
	return Experiment{
		Name: "J-multiple-episodes",
		Hypothesis: "Three u1 episodes with healthy gaps: [30,50) complete-fail (breaker " +
			"engages), [70,90) latency-only 15ms (breaker idle, queues fill), [110,150) " +
			"50%-fail (amplification + breaker). Each gap [50,70) and [90,110) returns the " +
			"system to its steady state; the LAST recovery at 150ms anchors the recovery " +
			"analysis of the horizon [150,170).",
		Config: experimentCfg(),
		Workload: WorkloadSpec{Seed: scenarioSeed, Components: []ArrivalComponent{
			comp(t1, u1, 0.3, 50*ms, 1),
			comp(t2, u2, 0.2, 50*ms, 1),
		}},
		Faults: FaultSchedule{
			{Upstream: u1, Kind: FaultFailRate, EveryN: 1, From: 30 * msT, Until: 50 * msT},
			{Upstream: u1, Kind: FaultSlowLatency, It: 15 * ms, From: 70 * msT, Until: 90 * msT},
			{Upstream: u1, Kind: FaultFailRate, EveryN: 2, From: 110 * msT, Until: 150 * msT},
		},
		Phases: []Phase{
			{Name: "pre", From: 0, Until: 30 * msT},
			{Name: "episode-1", From: 30 * msT, Until: 50 * msT},
			{Name: "gap-1", From: 50 * msT, Until: 70 * msT},
			{Name: "episode-2", From: 70 * msT, Until: 90 * msT},
			{Name: "gap-2", From: 90 * msT, Until: 110 * msT},
			{Name: "episode-3", From: 110 * msT, Until: 150 * msT},
			{Name: "post-last", From: 150 * msT, Until: 170 * msT},
		},
		Recovery: &RecoveryAnchor{DegFrom: 110 * msT, RecAt: 150 * msT, Policy: DefaultRecoveryPolicy()},
		Horizon:  170 * msT,
	}
}

// AllScenarios returns scenarios A–J in order.
func AllScenarios() []Experiment {
	return []Experiment{
		ScenarioA(),
		ScenarioB(),
		ScenarioC(),
		ScenarioD(),
		ScenarioE(),
		ScenarioF(),
		ScenarioG(),
		ScenarioH(),
		ScenarioI(),
		ScenarioJ(),
	}
}

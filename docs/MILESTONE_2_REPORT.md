# Milestone 2 Report — Failure Injection & System Behaviour

This report documents the M2 deliverables built on top of the M1.1
reference system, and the **actual measured results** of running the
scenario library. Every number below comes from the code as it stands
(fresh `go test ./sim -run TestSmokeRunAllScenarios` capture and the pinned
assertions in `sim/m2_*_test.go`); nothing is aspirational, and nothing
here claims metastability is *demonstrated* — see §8 for exactly what is
and is not claimed.

## 0. Procedure and verification gate

- Incremental coherent workstreams; after each change: `gofmt`,
  `go vet ./...`, `go test ./... -count=1`.
- Current gate (primary evidence):

| Check | Result |
|---|---|
| `gofmt -l .` | clean (no output) |
| `go vet ./...` | clean, `ok sentra/domain`, `ok sentra/engine`, `ok sentra/sim` |
| `go test ./... -count=1` | **111 tests, 0 failures** across domain/engine/sim |
| Deterministic replay | `ReplayCheck` over all 10 scenarios: two fresh runs agree on summaries, phase reports, full record ledgers, recovery reports (`TestReplayCheckAllScenarios`) |
| State invariants | full `Validate()` re-asserted at every experiment phase boundary inside `RunExperiment` (panics on any violation) |

- 46 new M2 test functions: `m2_workload_test.go` (16), `m2_metrics_test.go`
  (10), `m2_recovery_test.go` (9), `m2_scenarios_test.go` (11).
- M1.1 remediation layer untouched: `mustJSON`, overflow-guarded
  arithmetic, exact ledgers and all M1 tests pass unmodified.

## 1. Recorded inputs (workstreams 1–2)

The full input of any experiment is **plain JSON-serializable data**,
recorded next to the results (`RunResult.Inputs` → `RecordJSON()` of
workload and faults):

- **Workload** (`sim/workload.go`): per tenant→upstream `ArrivalComponent`
  (integer-window rate with fractional carry, optional inst-window jitter,
  deadline bounds, retry limits, optional activation window) plus one-off
  `Burst`s. `Build(horizon)` materializes arrivals deterministically.
  - Rate 0.4/window over 10 windows yields **exactly 4** arrivals
    (`TestWorkloadFractionalRateExactness`) — counts are never sampled away.
  - A fixed deadline (`min == max`) consumes no PRNG stream, so changing a
    fixed deadline cannot move arrival placement
    (`TestWorkloadFixedDeadlineDoesNotConsumePRNG`).
- **Randomness** (`sim/prng.go`): splitmix64 keyed `(seed, stream)`, the
  only randomness in the codebase; no `math/rand`, no wall clock.
  (`TestPRNGIsDeterministicPerSeedAndStream`, `TestPRNGStreamSeparation`).
- **Fault injection** (`sim/faults.go`), three modes, U1/U2 fully
  independent:

| Mode | Event | Runtime behaviour |
|---|---|---|
| Slow-but-succeeding | `UpstreamLatencySet{Latency}` | every new execution start uses the override duration; completions SUCCEED; breaker never fires |
| Elevated failure rate | `UpstreamFailureSet{EveryN}` | every k-th completion fails (k = the upstream's own completion counter — deterministic, replay-safe); normal latency |
| Complete unavailability | `UpstreamDegraded`/`UpstreamRecovered` (M1 events) | degraded latency + every completion fails (A12 unchanged) |

Schedules are set/restore pairs of each episode's own mode; restore is
emitted at `Until` (including `Until == horizon`), restore-before-set at
shared instants, same-upstream overlaps rejected. Measured runtime
behaviour: latency override pins `ExecDuration` exactly for attempts
started inside the window and restores M1 latency after it
(`TestFaultLatencyAppliesAndRestores`); `EveryN=2` produces exactly
⌊K/2⌋ failures of K completions independent of interleaving
(`TestFaultFailRateDeterministicCounter`).

## 2. Metrics model (workstreams 3, 4, 7)

Definitions and conventions as implemented (`sim/system_metrics.go`,
`sim/state.go`), all pinned by tests:

- **Goodput** counts *logical requests*: terminal `Completed` with
  `TerminalAt ≤ Deadline`. **A successful attempt is never a successful
  request.** Under current ordering Completed == deadline-respecting, and
  the two are nevertheless reported separately so the definition never
  depends on the transition graph.
- **Amplification** = `PhysicalAttempts / Admitted` (NaN at 0/0 —
  undefined, never faked). Scenario E measures **1.103** (32 attempts /
  29 requests), retries-per-request 3/29 ≈ **0.103**.
- **Token ledger**: every started retry spends exactly one token; a retry
  **blocked at re-admission keeps its token spent** (M1 A16, unchanged) and
  is counted separately (`BlockedRetries`, `"retry-admission"` notes).
  Measured consistency across scenarios that exercise blocking:
  C, H, I: tokens 2 = 1 started + 1 blocked; J: tokens 4 = 3 + 1; E: 3 = 3 + 0.
- **Wasted work**: attempts terminalized `WASTED`/`DISCARDED` vs `USEFUL`,
  `WastedShare = Wasted/(Useful+Wasted+Disc)`. F measures 8 wasted of 41
  attempts (**WastedShare 0.26**); G measures 4 wasted over 74 (**0.075**).
- **Work after expiry (C3)**: terminal non-retracted attempts finishing at
  or after their logical request's expiry instant. F measures **8**
  (reconciled against an independent recount from the final state —
  `TestWorkAfterExpiryDefinitionC3`).
- **Occupancy timelines** (`sim/occupancy.go`): one sample at every exact
  resource-mutation instant; never reset, never read mid-run. The worker-
  occupancy integral equals Σ per-attempt `WorkerTime` + open holds
  EXACTLY — two independent measurements of the same quantity agree to the
  nanosecond on A, D, F (`TestWorkerOccupancyIntegralReconcilesWithWorkerTime`).
- **Percentiles**: discrete rank `ceil(q·N)`, no interpolation;
  QueueWait P50/P90/P99 are documented histogram approximations (each
  log2 bucket's bound stands for its samples — e.g. B's measured
  33 554 432 ns ≈ 33.5 ms is a bucket bound, not an exact wait).
- **Phases** partition the horizon; summing a phase-partition's counts
  equals the whole-run summary exactly for J's 7 phases
  (`TestPhasesPartitionReconcilesWithSummary`).

## 3. Scenario library (workstream 9)

All scenarios (`sim/scenario_library.go`) share one config shape
(3 workers, 2 conns per upstream, lanes 8/8/8, budget cap 2,
refill 100 ms, backoff 2 ms, attempt timeout 25 ms, breaker 2/50 ms/1) and
inherit the control scenario A's workload shape. Horizon 120 ms except A
(50 ms) and burst/two-upstream variants as noted. Config, workload and
faults are recorded in `RunResult.Inputs`.

Measured results (fresh run, `TestSmokeRunAllScenarios`):

| # | Scenario | Fault input | Offered | Admitted | Goodput | Attempts (arr+retry) | Shed | Failed | Expired | Wasted att. | WastedShare | Amp. |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A | healthy-baseline (control) | none | 29 | 29 | 28 | 29+0 | 0 | 0 | 0 | 0 | 0.00 | 1.000 |
| B | ordinary-overload | none | 128 | 48 | 48 | 48+0 | 80 | 0 | 0 | 0 | 0.00 | 1.000 |
| C | u1-degraded-u2-healthy | unavailable u1 [20,100) | 29 | 22 | 20 | 23+1 | 7 | 1 | 0 | 0 | 0.00 | 1.045 |
| D | slow-recovery-backlog | latency 15 ms u1 [40,70) | 39 | 37 | 33 | 37+0 | 2 | 0 | 0 | 0 | 0.00 | 1.000 |
| E | retry-amplification-sustained | EveryN=2 u1 [20,100) | 29 | 29 | 24 | 32+3 | 0 | 4 | 0 | 0 | 0.00 | 1.103 |
| F | contention-conn-exhaustion | latency 20 ms u1 [30,90), u1+u2 surge | 66 | 41 | 15 | 41+0 | 25 | 0 | 16 | 8 | 0.26 | 1.000 |
| G | cross-tenant-interference | none (t1+t2 → shared u1 baseline) | 144 | 74 | 49 | 74+0 | 70 | 0 | 7 | 4 | 0.075 | 1.000 |
| H | breaker-recovery | EveryN=1 u1 [30,55) | 29 | 21 | 18 | 22+1 | 8 | 2 | 0 | 0 | 0.00 | 1.048 |
| I | burst-during-halfopen | = H + 12-arrival burst at 60 ms | 41 | 21 | 18 | 22+1 | 20 | 2 | 0 | 0 | 0.00 | 1.048 |
| J | multiple-episodes | 3× latency/raise episodes u1 | 42 | 34 | 28 | 34+3 | 8 | 4 | 0 | 0 | 0.00 | 1.088 |

Phase/recovery context per scenario is in `scenario_run.txt` (repo root)
and reproduced for keyed scenarios in §4.

### Hypothesis vs observed

- **A** — hypothesis confirmed exactly: 0 sheds, 0 retries, 0 waste,
  workers 54 % busy, queue peak 0.
- **B** — hypothesis confirmed: overload sheds 80 at admission, all 48
  admitted complete in deadline, tail phase drains to zero queue and
  in-flight. Ordinary overload without amplification does NOT metastabilize
  — this is the reference against which amplification is read.
- **C** — hypothesis confirmed: during the u1 outage goodput collapses from
  0.200/ms (pre, [0,40)) to 0.067/ms (during, [40,70)), t2 keeps serving
  untouched (all t2 attempts complete in deadline), recovery is
  delayed-but-eventual (0.200/ms pre → 0.200/ms post with backlog 3 at
  recovery and the post phase never fully draining inside the horizon).
- **D** — hypothesis confirmed **after recalibration** (see §6.1): light
  0.3/2 ms pre and post, heavy 1.0/2 ms only inside [40,70): backlog of 11
  entries at recovery (9 carried past the immediate window), workers still
  100 % busy at the immediate boundary, post phase drains to queue 1 —
  recovery delayed by drainage but eventual, goodput rate 0.200 → 0.440/ms.
  No expiry, no retries, breaker never fires.
- **E** — hypothesis confirmed **after one expectation correction** (see
  §6.2): amplification 32 attempts / 29 requests, 3 retries consume exactly
  3 tokens, 4 requests fail on exhausted budget/retry limit, and the
  breaker **stays closed** — an every-2nd-completion rule never produces
  the two consecutive failures the threshold needs (the original text
  expected an open; the measured mechanism does not support it, so the
  hypothesis was corrected to match the mechanism, and the corrected claim
  is what the test pins).
- **F** — hypothesis confirmed in mechanism, honest about speed (see
  §6.1): during the window the 3-worker pool saturATES (util 0.989 in
  [30,90)), u1's 2-connection pool exhausts exactly to capacity 2
  (`ConnOcc[u1].Peak == 2`), 16 queued requests expire, 8 attempts are
  wasted after their requests expired, 25 requests shed. After the 90 ms
  recovery the backlog (16 at recovery) cannot beat the 50 ms deadlines in
  the 30 ms post window: post rate 0.033 vs pre 0.167/ms — observed class
  **sustained-collapse-within-horizon**, which is a bounded statement about
  THIS horizon, not a metastability claim (§8).
- **G** — hypothesis confirmed: t1 (2.0/ms) and t2 (0.4/ms) share u1 at
  capacity 0.6/ms; t2's light attempts age behind t1's and lose
  deadline-respecting completions (t2: 13 of 19 vs t1: 28 of 33); the
  equal-lane-cap FIFO baseline yields Jain(useful) = **0.787**. t3 (0.4/ms
  into healthy u2, capacity 0.3/ms) serves as the same-rate comparison —
  and itself expires 6 of 22, independently confirming that plain
  over-capacity also accumulates deadline losses WITHOUT interference.
- **H** — hypothesis confirmed: full breaker lifecycle recorded as an
  ordered evidence chain `closed → open → half-open → closed` (exactly 3
  transitions, `Detail.From/To` in `"breaker"` notes,
  `TestScenarioHBreakerLifecycle`); 8 sheds + 2 failed; exactly one
  half-open probe; delayed-but-eventual recovery (0.167 → 0.169/ms,
  backlog 0).
- **I** — hypothesis confirmed exactly: the 12-arrival burst at 60 ms
  (inside H's cooldown phase) adds **exactly** +12 offered and +12 sheds
  against H with byte-identical terminal outcomes and attempt volumes —
  the burst is entirely an admission phenomenon, absorbed by load shedding
  (`TestScenarioIBurst`).
- **J** — hypothesis confirmed: three episodes produce retries (3) and
  failures (4) but cumulative collapse does not set in inside the horizon;
  queue depth 0 at every phase boundary; recovery delayed-but-eventual
  (0.136 → 0.250/ms). The retry flow exercises all three accounting
  regimes: 3 started + 1 blocked, 4 tokens spent
  (`TestScenarioJMultipleEpisodes`).

## 4. Recovery analysis (workstream 8)

`sim/recovery.go` is post-hoc, pure and non-mutating:

- Windows are **pre** [0, DegFrom), **during** [DegFrom, RecAt),
  **immediate** [RecAt, RecAt + 10 ms), **drain** (scan to the first
  all-empty instant or the horizon), **post** (remainder to horizon).
- Drain scan: exact union-of-timeline-instants search for the first instant
  after RecAt with zero queue, zero waitlisted and zero busy; reports
  `Drained=false` honestly (D, F, C, H, J never fully certify within their
  horizons; B's tail variant is proven to find a real drain instant).
- Classification (threshold 0.5, pinned to each scenario's own numbers):

| Scenario | Pre/ms | Post/ms | Backlog@rec | In-flight@rec | Class |
|---|---|---|---|---|---|
| C | 0.200 | 0.200 | 3 | 6 | delayed-but-eventual |
| D | 0.200 | 0.440 | 11 | 14 | delayed-but-eventual |
| E | 0.150 | 0.250 | 0 | 1 | delayed-but-eventual |
| F | 0.167 | 0.033 | 16 | 19 | sustained-collapse-within-horizon |
| H | 0.167 | 0.169 | 0 | 1 | delayed-but-eventual |
| J | 0.136 | 0.250 | 0 | 2 | delayed-but-eventual |

Edge refuses are tested: `RecAt == horizon` ⇒ `no-post-window`;
degradation from t = 0 ⇒ `no-baseline`
(`TestRecoveryEdgeCasesNoBaselineNoPostWindow`). The classifier is proven
to follow its own measured rates
(`TestClassifierMatchesMeasuredRates`), the class map of the library is
pinned (`TestObservedClassesPinTheScenarios`), and **no state reset
hides at the recovery instant** — D's backlog of 11 is present at RecAt,
9 still there 10 ms later, workers busy throughout, all budgets and
breakers intact (`TestNoStateResetsAcrossRecovery`, E's budget ledger
reconciles to the 3 `"budget"` records in the during-window with 0
before/after).

## 5. Tenant interference / fairness (workstream 6)

Measured under the existing FIFO baseline only (no alternative scheduling
was implemented — the milestone requires the measurement, not a fix):

| Tenant (G) | Offered | Admitted | Goodput | Deadline-respect | Shed | Expired |
|---|---|---|---|---|---|---|
| t1 (2.0/ms → u1) | 100 | 33 | 28 | 28 | 67 | 0 |
| t2 (0.4/ms → u1) | 20 | 19 | 13 | 13 | 1 | 1 |
| t3 (0.4/ms → u2) | 24 | 22 | 8 | 8 | 2 | 6 |

Both shared-upstream tenants lose deadline-respecting completions (t1 by
volume and shedding, t2 by FIFO aging behind t1's backlog). Jain index
over useful outcomes: **0.787** — below 1 despite equal per-tenant lane
caps. t3's independent expiry (plain over-capacity, capacity 0.3/ms) is
the same-rate no-interference comparison: deadline losses occur without
interference too, in proportion to demand vs own capacity.

## 6. Experimental validity (workstream 10)

### 6.1 Calibration honesty (design changes, not result plumbing)

Two scenarios were re-drawn at calibration time because their ORIGINAL
input did not address the regime their hypothesis spoke about:

- **D**: the original overload ran to the horizon, so "recovery" could
  never be observed regardless of mechanism. Recalibrated: baseline rates
  pre and post, overload coincident with the fault window only. The
  recovery claim then became observable; what it observes is a delayed-
  but-eventual recovery driven by FIFO drainage.
- **F**: the original had no healthy pre-phase and no post-recovery
  baseline, so the recovery window had nothing to be measured against.
  Recalibrated with healthy A rates outside [30,90) and the surge inside
  the window. The measured post rate is lower still (0.033/ms) because the
  accumulated backlog's arithmetic drain time (~16 entries × 5 ms of
  shared worker time each, against 3 workers shared with u2 demand)
  exceeds the 30 ms post window — the hypothesis text now bounds its
  claim to the horizon ("recovery speed is bounded by the backlog, not by
  the fault duration").

No code special-cases any scenario: `TestClassifierMatchesMeasuredRates`
re-derives each class from the report's own recorded rates, and the
recalibration changed INPUTS only (rates and windows).

### 6.2 Hypothesis correction (E)

The original E hypothesis predicted the breaker would open. The measured
mechanism contradicts that: failing every 2nd completion never accumulates
the 2 consecutive failures the threshold requires, so the breaker stays
closed for the whole episode (0 recorded transitions). The hypothesis text
was corrected to its actual mechanism claim — amplification, budget
throttling and exhaustion — which is what the scenario exists to measure;
the corrected claim is pinned by `TestScenarioERetryAmplification`. No
result was changed to meet the old text.

### 6.3 Validity mechanisms

- **Control scenario**: A carries zero fault input (verified against the
  ledger: no `"fault"`/`"breaker"` records) and anchors every comparison.
- **Recorded inputs**: every run's `Inputs` embeds the exact workload and
  fault JSON; `TestRecordedInputsMatchTheWorkloadBuild` ties A's recorded
  initial stream to a fresh `Build`.
- **Replay checks**: two fresh runs per scenario are DeepEqual on all
  derived artifacts (`TestReplayCheckAllScenarios`); additionally
  scenario A's `InitEvents` equal its workload build
  (`TestSimEngineWiringIndependence` in metrics tests).
- **No result-guaranteeing designs**: the experiment machine has no
  between-boundary state fixes (invariants validated at each boundary;
  `RunExperiment` panics); recovery analysis mutates nothing (DeepEqual
  before/after); blocked retries, sheds, expiry and waste all flow through
  M1.1's recorded paths.

## 7. Config and taxonomy changes (compatibility)

- Two new Model-control event kinds: `UpstreamLatencySet`,
  `UpstreamFailureSet` — taxonomy now **20 kinds**, 5 control-class
  (exhaustive-switch test updated, all M2 runtime events exercised).
- New per-request/attempt fields (`TerminalAt`, `FinishedAt`,
  waiting/worker stamps) are observational exactly-once stamps; no
  lifecycle or ordering semantics changed.
- `RunResult` exposes `FinalState *SystemState` and `InitEvents`
  (read-only evidence access; mutation is outside the engine's ownership).
- Shared scenario constants (`ms`, `msT`, tenant/upstream IDs) live in
  `scenario_library.go`.
- `smoke_test.go` is retained as the report data source (it prints the
  terminal-state split per scenario).

## 8. What this milestone does and does not claim

DOES claim, measured:

- Retry amplification, budget-exhaustion failure, admission blocking and
  breaker cycling are reproducible, replayable, and fully accounted
  (attempts ≠ requests; blocked ≠ started; token ledger balanced).
- Resource contention cascades measurably across upstreams through the
  shared pool (F: pool saturation + connection exhaustion + u2 starvation;
  cross-tenant interference in G) under the FIFO baseline.
- Recovery from a single degradation episode (C, D, H, J) is measurable
  and classified against recorded numbers, with the backlog carried
  through recovery untouched by any reset.
- Scenario F **within its 120 ms horizon** a sustained-collapse state:
  post-recovery goodput 0.033/ms vs 0.167/ms baseline — contention
  amplifies the outage beyond the recovery instant at this horizon.

DOES NOT claim:

- **Metastability is not demonstrated.** A metastable claim requires a
  mechanism that reproduces amplification internally (self-sustaining
  load generation) — E's 1.103 amplification is triggered externally and
  dies with its trigger; F's collapse is bounded by its 30 ms observation
  window, not proven self-sustaining. Establishing or refuting
  metastability is the goal of the NEXT milestone, which now has the
  machinery (workload bursts, sustained-rate composition, amplification
  accounting, recovery classes) to run such probes.
- No multi-pool scheduling, no fairness policy, no remediation logic and
  no alternative scheduler were added (explicitly out of scope; A27).

## 9. Known limitations

See `docs/ASSUMPTIONS.md` §D (M2 additions). Headlines: the recorder's
8 192-record ring has not overflowed in the library (max 1 888, G), but
heavy scenarios could evict early ledger records (streaming sink exists
for such use cases); QueueWait percentiles are documented histogram
approximations; F's collapse statement is horizon-bounded by design
(A24); occupancy timelines are evidence-only and O(resource mutations) in
memory.

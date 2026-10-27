# Sentra System Model (Milestones 1 & 2)

This document defines the domain entities, their lifecycles, the state they
own, and the resource accounting rules of the M1 foundation. Nothing here is
aspirational: every entity and every transition listed exists in code and is
tested. Section 6 records the Milestone 2 additions: recorded experiment
inputs, the era-stamped occupancy timelines and the terminal-timestamp
accounting that the failure-injection experiments read.

## 1. Entities

| Entity | Type | Identity | Package |
|---|---|---|---|
| Logical request | `domain.LogicalRequest` | `RequestID` ("req-N") | domain |
| Physical attempt | `domain.PhysicalAttempt` | `AttemptID` ("att-N") | domain |
| Tenant | configuration entity + accounting | `TenantID` | domain/sim |
| Upstream dependency | `domain.Upstream` | `UpstreamID` | domain |
| Worker | slot identity | `WorkerID` ("pool-ID-w-NN") | domain |
| Worker pool | `domain.WorkerPool` | `WorkerPoolID` | domain |
| Connection pool | `domain.ConnectionPool` | keyed by `UpstreamID` | domain |
| Queue / entry | `domain.Queue`, `domain.QueueEntry` | keyed by `TenantID` | domain |
| Retry budget | `domain.RetryBudget` | `(TenantID, UpstreamID)` key | domain |
| Circuit breaker | `domain.CircuitBreaker` | keyed by `UpstreamID` | domain |

ID spaces (`RequestID`, `AttemptID`, …) are distinct named types over string.
They share representation but not type, so conflation of a logical request
with a physical attempt is a compile-time error, and the generated IDs are
deterministic per-run (monotonic counters inside `SystemState`).

## 2. Two lifecycles, deliberately separate

### Logical request (`RequestState`)

```
ADMITTED ──→ QUEUED ──→ DISPATCHED ──→ COMPLETED      (first usable success)
   │      │           │   └→ RETRYING ──→ QUEUED   (retry loop; retry is
   │      │           │                             re-admitted at its exec
   │      │           │                             instant, A16)
   │      │           └→ FAILED / EXPIRED
   │      └→ EXPIRED
   │      (also: deadline == arrival expires ADMITTED immediately, A14)
   ├──→ SHED (admission rejected; terminal at arrival)
   └──→ (any active state) ──→ CANCELLING ──→ CANCELLED
```

Terminal states: `COMPLETED`, `FAILED`, `EXPIRED`, `CANCELLED`, `SHED`.
Terminal states have no outgoing transitions; every transition is validated
against an explicit table (`CanTransitionRequest`), and illegal transitions
panic — they indicate programming errors, not runtime conditions.

Ownership: lifecycle state, deadline, retry policy (`MaxRetries`,
`RetriesUsed`), and the list of its attempts.

### Physical attempt (`AttemptState`)

```
CREATED ──→ QUEUED ──→ DISPATCHING ──→ ASSIGNED ──→ RUNNING ──→ COMPLETED
                                 │           │   └→ FAILED / TIMED-OUT
                                 │           └→ WAITING_CONNECTION ──→ RUNNING / TIMED-OUT
                                 └→ (CREATED / QUEUED / DISPATCHING / WAITING) ──→ RETRACTED
                                    (single centralized exit path `retractAttempt`; releases the
                                     queue entry, waitlist membership, pending-lane book, probe
                                     permit and — for WAITING, which holds a worker — the worker,
                                     exactly once; executing attempts are orphaned instead)
```

Key property: **an attempt's execution is independent of its logical
request's fate.** When a request expires or is cancelled:

- attempts not yet executing (queued / waiting for a connection) are
  *retracted* — cheap, frees resources immediately;
- attempts already executing are *orphaned* (`Orphaned`, `OrphanedAt`,
  `OrphanReason`) and **continue to their own completion** — they hold their
  worker and connection until they finish or time out.

This distinction is what later milestone experiments quantify: orphaned work
is downstream resource consumption with no beneficiary — the raw material of
metastable failure.

### Attempt outcome, separate from execution state

`AttemptOutcome` records whether an attempt's eventual result was usable:

| Outcome | Meaning |
|---|---|
| `USEFUL` | result delivered to a live logical request (including failures and timeouts that drove a retry/failure decision) |
| `WASTED` | attempt produced a result its logical request can no longer use — late completion after expiry/cancel, or superseded success |
| `DISCARDED` | attempt never produced a result (retracted before starting execution) |
| `UNDETERMINED` | attempt still in flight |

A late completion is therefore representable *without* being mistreated as
useful: execution state `COMPLETED` + outcome `WASTED`. Per-tenant counters
(`SystemState.OutcomeCounters`) aggregate Useful/Wasted/Discarded.

## 3. State ownership

`SystemState` (package `sim`) is the single owner of mutable system state.
Who may write what:

| State | Owned / mutated by | Trigger events |
|---|---|---|
| Request lifecycle | arrival, completion, timeout, deadline, cancellation handlers | Arrival, ExecutionCompletion, Timeout, DeadlineExpired, Cancellation* |
| Attempt lifecycle | queue insertion/removal, worker assignment, execution start/grant, completion, timeout handlers | QueueInsertion…Timeout |
| Queue membership | queue-insertion handler adds; removal/retraction paths remove | QueueInsertion, QueueRemoval, DeadlineExpired, CancellationRequested |
| Worker occupancy (`WorkerPool.Busy/Idle`) | worker-assignment (take) and attempt-finalization paths (release) | WorkerAssignment, ExecutionCompletion, Timeout |
| Connection occupancy & waitlist | execution start/grant (acquire, enqueue), releases (release + wake) | ExecutionStart, ConnectionGranted, completions/timeouts |
| Retry budgets | retry decisions, via `ConsumeRetryBudget` (lazy create + refill + consume) | completions/timeouts |
| Breaker state | `BreakerTransitioned` handler only | BreakerTransitioned |
| Upstream `Degraded` flag | environment handlers | UpstreamDegraded, UpstreamRecovered |
| Per-tenant counters | arrival (increment) and terminalization (decrement); outcomes finalized at terminal | all terminal paths |

Mutation happens exclusively inside `sim.Handle` (single-threaded, from the
engine, one event at a time). The state carries no locks, goroutines or wall
clocks, which makes whole-state `DeepEqual` a valid repeatability check.

## 4. Resource accounting invariants

`SystemState.Validate()` enforces, in a single O(N) pass over the attempt
registry, and the tests exercise each family:

1. Worker pools: `len(Busy) + len(Idle) == Size`; no worker both busy and
   idle; every busy worker is claimed by an existing attempt that names it
   and is in a worker-holding state (both directions).
2. Every attempt claiming a worker really occupies that worker in the pool.
3. `ConnHeld == (attempt is in the pool's Held set)`; holding implies
   `RUNNING`; a `WAITING_CONNECTION` attempt is in the waitlist of the
   right upstream and holds no connection (both directions).
4. Terminal attempts always carry a determined outcome; active attempts never
   do.
5. Queue membership is exactly the set of `QUEUED` attempts of that tenant
   (bidirectional check, no duplicates, right tenant).
6. Lane bounds **including pending books**: arrival lane ≤
   `MaxArrivalQueueDepth`, retry lane ≤ `MaxRetryQueueDepth`, and the
   sum of both lanes plus both pending books ≤ `MaxTotalQueueDepth`.
7. Pending books: per-tenant pending counts match the created-state
   pending attempt sets, which carry the matching lane flag.
8. Probe ledger **exact**: `Σ attempt.IsProbe == ProbesActive` per
   upstream, within `[0, MaxHalfOpenProbes]` — no slack.
9. Outcome ledger **exact in both directions**: per-tenant tallies
   recomputed from the attempt registry equal `OutcomeCounters`, and every
   configured tenant has a counters book.
10. Request shape: `len(req.Attempts)` equals the number of registered
    attempts back-linked to the request.
11. Tenant active counts are non-negative and equal the non-terminal
    request count of that tenant.

The validator is used by tests at run end and at mid-run checkpoints, and is
proven to detect corruption per family:
`TestValidateDetectsProbeAndLedgerCorruption`,
`TestValidateDetectsWaitlistAndPendingLedgerCorruption`.

## 5. Resource accounting rules

- **Workers**: acquired at `ASSIGNED` (worker assignment), held through
  `WAITING_CONNECTION` (a worker pinned waiting for a connection is the
  resource-cascade case this model must expose), released at attempt
  termination (`COMPLETED`, `FAILED`, `TIMED-OUT`) or on retraction from
  pre-execution states. Idle list is FIFO: workers are taken in release
  order.
- **Connections**: acquired at actual runnable execution start (immediately
  at `ExecutionStart` if capacity remains, otherwise via
  `ConnectionGranted` after oldest-first release wakeups). A connection is
  held until the attempt terminates.
- **Queue slots**: bounded per tenant by **two lanes plus a total bound**
  (`MaxArrivalQueueDepth`, `MaxRetryQueueDepth`, `MaxTotalQueueDepth`
  in `TenantConfig`; `NewState` rejects a total smaller than either lane).
  Membership is a FIFO list with a global sequence number for cross-queue
  ordering. Same-instant admissions and retry executions are closed
  against overshoot via the pending-arrival/pending-retry books (A16) —
  there is no silent lane overshoot; blocked retries fail explicitly with
  a recorded reason.
- **Retry budget**: integer token bucket per `(tenant, upstream)`; one token
  per scheduled retry (checked at scheduling time, A5); refused when
  exhausted; **not refunded when the retry's admission is later blocked**
  (A16). Refill by whole intervals with the fractional remainder preserved
  (`LastRefill += gain*interval`); exhausted budget means the request fails
  rather than retrying.
- **Probe permits**: a request admitted while the breaker is HALF_OPEN
  reserves a permit at admission and releases it when its attempt
  terminates **or is retracted** — the ledger is exact (Σ `IsProbe` ==
  `ProbesActive`), enforced by `Validate()`.

## 6. Milestone 2 additions (experimental capabilities)

M2 adds no new lifecycle states and no new transition rules. It adds:
recorded *inputs* (workload + fault schedules), *evidence-only timelines*
stamped at exact instants, and *derived analytics* computed post-hoc.

### 6.1 Recorded experiment inputs (plain data)

| Entity | Type | Role |
|---|---|---|
| Arrival component | `sim.ArrivalComponent` | one tenant→upstream stream: integer-window rate (fractional carry propagates, never sampled away), optional inst-window jitter, per-arrival deadline bounds and retry limits, optional `[From, Until)` activation |
| Burst | `sim.Burst` | one-off arrival surge: `Count` arrivals near `At` with a seeded spread over `[At, At+Spread)` |
| Workload spec | `sim.WorkloadSpec` | seed + components + bursts; **is** the recorded workload; `Build(horizon)` materializes arrival events deterministically |
| Fault schedule | `sim.FaultSchedule` | per `(upstream, episode)`: mode (`FaultSlowLatency{Latency}`, `FaultFailRate{EveryN}`, `FaultUnavailable`), `From`, `Until` (until==0 ⇒ to-horizon); `Events(horizon)` materializes set/restore pairs |
| Generator | `sim.PRNG` | splitmix64 keyed by `(seed, stream)`; the only randomness; distinct streams are independent (idea-level separation of jitter samples and other strategies) |

Both specs are JSON-serializable (`RecordJSON()`) and are carried in every
run's recorded inputs — the experiment evidence names its own input exactly.

Materialization rules (tested): a within-horizon episode emits one set event
at `From` and one restore event of exactly the episode's own mode at `Until`;
`Until == horizon` still emits its restore (processed at the final instant);
`Until > horizon` is rejected by `Validate`. At equal instants restore
precedes set, so adjacent episodes hand over by event ordering alone —
the applied mode never encodes list order (overlapping episodes on ONE
upstream are rejected; overlaps across different upstreams are legal).

### 6.2 Fault modes vs the M1 failure model (A12)

- `FaultUnavailable` maps onto the M1 events `UpstreamDegraded`/
  `UpstreamRecovered` — the complete-unavailability mode IS the M1 degraded
  episode unchanged.
- `EventUpstreamLatencySet` overrides each *new execution start's* latency
  while armed (completions succeed — the completion-fails-while-degraded
  piece of A12 is bypassed exactly here and nowhere else, documented, never
  silent). Restore clears the override; M1's health-based latency applies
  again.
- `EventUpstreamFailureSet{EveryN}` arms a deterministic fail-rate mode at
  normal latency: every k-th *completion* of that upstream fails, k counted
  by that upstream's own counter (replay-safe: timeouts/retractions do not
  advance it). EveryN==0 disarms. Failure reasons record the mode, e.g.
  "completion forced to fail (fail-rate mode)".

A fault schedule is model-control input like any scenario: it never resets
state wholesale. Restore events clear exactly the mode that set them.

### 6.3 Era-stamped occupancy timelines (evidence-only)

`sim/occupancy.go` appends a sample at EVERY pool/queue/take/release/
waitlist mutation site (M2):

| Timeline | Records |
|---|---|
| worker occupancy | busy-count samples per exact take/release instant |
| connection occupancy | per-upstream held-connection samples (incl. waitlist growth/shrink) |
| queue occupancy | per-tenant per-lane plus total depth samples |

Chronology rules: during-simulated-time samples are appended in event order
(non-decreasing instants); the final drain-time samples are emitted at the
post-simulated horizon timestamp and sort after all run-time samples.
Timelines are never reset and never read mid-run by handlers — they are
post-hoc analysis input only.

### 6.4 Terminal-timestamp accounting (exactly once)

- `LogicalRequest.TerminalAt`: set exactly once at the terminal transition
  (also for SHED at arrival), enabling per-phase and delay/deadline-respect
  arithmetic without re-deriving instants from the ledger.
- `PhysicalAttempt`: `FinishedAt` at terminality; `WaitingSince/Set` and
  `WorkerSince/Set` stamped exactly-once at the waitlist join and worker
  hold, so connection-wait and worker-occupancy integrals reconcile against
  per-attempt interval bookkeeping (two independent measurements of the
  same quantity; the tests prove their equality exactly).

### 6.5 Per-tenant M2 books (additive to M1 counters)

`TenantMetrics` gains: `AttemptsStarted`, `RetryAttempts`, `BlockedRetries`
(retries rejected at re-admission under breaker/lane gates — a distinct
category from attempts and sheds), `WorkerTime` (Σ worker-held intervals),
`ConnWaitSum/Max/N/Abandoned`, and `QueueDwellSum/N` (abandoned-in-queue
dwell). Blocked retries keep their budget token spent (M1 A16, unchanged);
the metric simply makes the rejection visible.

## 7. Verifier-side reconstruction semantics (M3 P2)

The authoritative model above is what `sim` implements. The independent verifier
re-derives a *model of the observed run* from serialized evidence without
calling any of it:

| Verifier artifact | Re-derived from | Never read from reference code |
|---|---|---|
| request lifecycle | `request-transition` notes (`request`, `from`, `to`) | `requestTransitions`, `CanTransitionRequest` |
| attempt lifecycle | `attempt-created` and `attempt-transition` notes | `attemptTransitions`, `CanTransitionAttempt` |
| attempt parentage | typed creation note (D1), else block-local typed request reference | `LogicalRequest.Attempts` back-links |
| breaker lifecycle | `BreakerTransitioned` event payload (`upstream`, `from`, `to`) | breaker transition application |
| final books | snapshot facts copied by the serializer | `SystemState`, its maps or `Validate()` |
| retry decisions | `RetryScheduled` event kind and `budget` note count | `ConsumeRetryBudget` |

Semantics that differ deliberately from the reference implementation:

1. **Continuity is a contract.** A transition whose `from` does not equal the
   reconstructed state is a failed property, even if the transition itself is
   legal. The reference would panic on such a state; the verifier treats it as
   evidence the two books disagree and refuses to assign blame.
2. **Missing history is a gap.** A history that begins at a non-initial state is
   incomplete evidence (ring truncation or a dropped term), not a violation.
3. **Empty observation sets are gaps.** A ledger that never observed a request
   or attempt cannot establish that a lifecycle was followed.
4. **The retry-budget equality is complete-ledger-only.** Truncation can split a
   scheduled retry from its budget note.
5. **Resource contracts are checked on the final books.** The snapshot must
   satisfy the documented invariants of §4 by independent arithmetic, including
   pending admissions in lane bounds; it is not validated by calling
   `SystemState.Validate()`.
6. **Cross-path disagreement is not a defect verdict.** The ledger and the
   snapshot are independent observations. When they disagree, the verifier
   reports the disputed fact, both references and expected/observed values, and
   classifies the finding `unclassified`.

Evidence typing used by the verifier: `request-transition`,
`attempt-transition`, `attempt-created` (D1), `cancellation`, and the
`BreakerTransitioned` event payload. Worker, connection, budget and probe notes
remain diagnostic prose; resource identity comes from the snapshot.

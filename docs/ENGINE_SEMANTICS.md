# Engine Semantics (Milestones 1 & 2)

## 1. Time

- One tick = one simulated nanosecond. `domain.SimTime` is a point;
  `domain.SimDuration` is a span; both are int64-backed and never derived
  from the wall clock. Converting between them requires an explicit cast.
- Simulated time advances only when an event executes (`engine.Step`).
  `engine.Run(until)` executes events with `At() <= until` (boundary
  inclusive) and returns the count processed.
- Scheduling in the past (handler outputs or injections with
  `At() < now`) is an invariant violation and panics.
- All in-engine scheduling of far-future instants goes through checked
  arithmetic (`Sim.addT` → `SimTime.AddChecked`): an int64 overflow panics
  with the distinct message "simulated time overflow" — never silent
  wrap-around, never the generic past-scheduling error
  (`docs/ASSUMPTIONS.md` A17;
  `TestSimulatedTimeOverflowHasDistinctError`).

## 2. Event ordering — the total order

Events are ordered by the tuple:

```
( At() , Priority() , scheduling sequence number )
```

- `At()`: simulated instant.
- `Priority()`: coarse class resolving same-instant races (§3).
- scheduling seq: assigned when the event is enqueued (initial events in
  list order, handler outputs in return order). Same-instant, same-priority
  events therefore run FIFO by scheduling order — never by a hidden rule.

**The tuple applies to injected and generated events identically.** There
is no injected-precedence rule of any kind. In particular:

- a *generated* event with a smaller priority outranks an *injected* event
  with a larger priority at the same instant, even though the generated
  event was enqueued later;
- a generated event competes with the remaining injected events of the
  same instant purely by its own `(priority, seq)`: generated events tie-
  break among themselves by generation order, and against still-unprocessed
  injected events by their later seq.

This refutes the earlier "A13" reading that injected events as a group run
before generated events at an instant. Demonstrated exhaustively by:

- `TestGeneratedEventsObeyPriorityNotGenerationOrder` — injected
  governance+work at t, generation order [gwork, ggov]; executed order:
  igov → ggov → iw → gwork (generated governance outranks injected work;
  injected work outranks the later-scheduled generated work at the same
  primary class);
- `TestEventPriorityClassesAreExhaustive` — all 18 kinds sample at exactly
  one of the four class constants; the distribution is control 3 /
  governance 3 / timeout 1 / work 11.

Scenarios that need instance-level interleaving guarantees stronger than the
tuple must use distinct instants — there is no additional injected/generated
guarantee to lean on.

## 3. Same-instant precedence classes

Lower priority number runs earlier at the same instant:

| Class | Value | Members | Resolves |
|---|---|---|---|
| Model control | 0 | `UpstreamDegraded`, `UpstreamRecovered`, `BreakerTransitioned` | environment applies before any work observes it at that instant |
| Governance | 10 | `DeadlineExpired`, `CancellationRequested`, `CancellationAcknowledged` | termination/retraction decided before further resource consumption |
| Timeout | 20 | `Timeout` | the timeout/completion boundary is resolved pessimistically |
| Work | 30 | arrival, admission, queueing, dispatch, assignment, execution start/completion, retry scheduling/execution, load shedding, connection grant | everything else |

### Explicit race resolutions

1. **Deadline expiration vs execution completion at the same instant**
   → the deadline wins. The request is expired first, the attempt is
   orphaned, and the same-instant completion arrives late: execution state
   `COMPLETE`, outcome `WASTED`, the request stays expired.
   (`TestDeadlineBeatsCompletionAtSameInstant`)
2. **Attempt timeout vs execution completion at the same instant**
   → the timeout wins. The completion event is ignored (guarded) — a result
   landing exactly on the timeout boundary is not trusted.
   (`TestTimeoutBeatsCompletionAtSameInstant`)
3. **Cancellation vs deadline at the same instant** — both are Governance
   class, so scheduling seq decides; both handlers guard against terminal
   states, so either order converges to a consistent outcome. The
   documented sub-decision: a deadline arriving after an accepted
   cancellation is ignored (cancellation has already superseded expiry;
   in-flight attempts are governed by the cancellation lifecycle).
   (`TestCancellationSupersedesLaterDeadline`)
4. **Attempt timeout vs connection release at the same instant** — the
   timeout (priority 20) retracts the waiting attempt from the waitlist
   first; the payload-free `ConnectionGranted` (priority 30) then selects
   the oldest *still-eligible* waiter, which may be an attempt that enqueued
   later (`TestStaleWakeSelectsOldestEligibleWaiterAtProcessingTime`).
5. **Duplicate same-instant connection grants / stale breaker transitions**
   → guarded no-ops; the event taxonomy needs no cancellation machinery
   because every handler re-validates its preconditions.

## 4. Admission, queueing and retry admission

### Lane model

Each tenant has **two bounded lanes plus one explicit total bound**
(`TenantConfig`):

- `MaxArrivalQueueDepth` — bounds original-arrival attempts (queued
  entries **plus pending attempts created at the same instant**, which
  closes the same-instant burst overshoot);
- `MaxRetryQueueDepth` — bounds retry attempts independently, so retry
  amplification cannot crowd out arrivals (and vice versa); counted the
  same way via its pending book;
- `MaxTotalQueueDepth` — bounds the sum; configuration requires it to
  contain both lane bounds (`NewState` rejects smaller values).

### Original-arrival admission (`handleRequestArrival`)

Shed reasons, evaluated in order at the instant of arrival:

| Reason | Fires when |
|---|---|
| `breaker-open` | target breaker is OPEN |
| `breaker-halfopen-saturated` | breaker HALF_OPEN with all probe permits reserved |
| `tenant-queue-full` | arrival lane at capacity (`countLane(q,false) + PendingArrivalCounts >= MaxArrivalQueueDepth`) |
| `tenant-queue-total-full` | total book at capacity (queued entries of both lanes + both pending books) |
| `deadline-at-arrival` | deadline equals arrival: the request expires immediately at admission, creating no attempt and arming no `DeadlineExpired` (A14) |

A shed emits `AdmissionDecided{Admitted: false}` + `LoadShed` with the
reason, and is recorded in `Metrics[t].Sheds`.

### Retry admission (`handleRetryExecution`)

Retries are re-admitted **at their true execution instant** (after
backoff), against the following gates — in order:

| Reason | Fires when |
|---|---|
| `retry-blocked-breaker-open` | breaker OPEN |
| `retry-blocked-breaker-halfopen` | breaker HALF_OPEN — retries can never consume probe permits, because probe results would then be retry-driven and could escape admission control |
| `retry-queue-full` | retry lane (queued retry entries + pending retry attempts) at `MaxRetryQueueDepth` |
| `retry-queue-total-full` | total book at capacity |

A blocked retry **fails its request explicitly** (`RequestRetrying →
RequestFailed`) and is recorded in `Metrics[t].Sheds` as well. The retry
budget token stays spent — non-refundable by design (A16) — so the request
fails having consumed exactly the budget of the retry it attempted.

### Repeatability note on same-instant retries

Two same-instant `RetryExecution` events serialize: the first creates its
pending retry attempt (visible to the second's admission check), the second
is blocked when the lane/total bound permits only one. This is intended and
tested (`TestRetryLaneBoundBlocksSecondSameInstantRetry`).

## 5. Evidence ledger and correlation

Every processed engine event and every explicit effect produces one
**record** — the versioned schema in `sim/recorder.go`:

```go
Record{SchemaVersion, EffectSeq, EngineSeq, SimTimeNS, Category, Kind, Subject, Detail}
```

- `EffectSeq` is gapless and unique per run (0-based);
  `EngineSeq` is the scheduling sequence number of the event being handled —
  the engine hands it to the handler (`Handle(ev, seq)`) and every record
  written while that event is processed carries it.
- `Category` is `event` (the processing trigger itself, one record per
  engine event) or `note` (one explicit effect: transition, skip, shed,
  resource release, outcome assignment, …).
- `Detail` is typed and explicitly serialized (request/attempt/tenant/
  upstream/worker ids, from/to, reason, deterministic description built
  from ID fragments) — never a `%v` structural dump of an event struct.
  Typed identity fields are populated by exactly four note kinds:
  `request-transition` (`Request`, `From`, `To`), `attempt-transition`
  (`Attempt`, `From`, `To`), `breaker` (`Upstream`, `From`, `To`) and
  `cancellation` (`Request`, `Reason`). All other note kinds
  (`worker`, `connection`, `budget`, `probe-*`, `queue`, `outcome`,
  `orphan`, `admission`, `skip`, `fault`, `*created`, `retry-admission`)
  carry their facts only in `Description` prose; consumers must not parse
  it (docs/VERIFIER_DESIGN.md §7).
- **The event→effect mapping is N:1, not 1:1.** Several effect_seq values
  share one engine_seq; the event line itself is one of them. Consumers of
  the ledger must join on `EngineSeq` and expect multiplicity. The
  independent verifier validates this shape as contiguous same-engine_seq
  blocks (one event record first, then its notes, all at the event's
  simulated time) rather than assuming one record per step
  (`verify/ledger.go`). It deliberately does not require `EngineSeq` to
  increase across blocks: priority class outranks scheduling sequence at
  equal simulated time, so processed engine sequences are not globally
  monotone.
- Sinks: `RingMemorySink` (default capacity 8192, oldest-eviction;
  eviction never perturbs effect_seq numbering), and streaming
  `JSONLSink` (one JSON object per line, flush on `Close`). Sink write
  failures are **sticky and panic** at the recording site: a partially
  flushed trace must never masquerade as a complete one.
- Hallucination resistance: two identical runs produce identical record
  lists; the JSON encoding is the versioned interchange contract
  (`schema_version:1`).
- `FormatRecords` renders the ledger for debugging with the engine_seq
  printed on every line.

Tests: `TestEffectsCorrelateWithOriginatingEngineEvent`,
`TestRecordJSONRoundTripThroughJSONLSink`,
`TestRingSinkRetentionIsBoundedAndOrderPreserving`,
`TestEvictionDoesNotPerturbEffectSeq`,
`TestRecorderPanicsOnSinkWriteFailure`.

## 6. `SystemState.Validate()` invariants

Single-pass (O(N) over the attempt registry) validator; the test suite
pins each family:

1. Worker pools: `len(Busy) + len(Idle) == Size`, no double-idle or dual
   busy+idle worker, assignment attributes match both directions.
2. Worker/attempt back-references agree in both directions; a worker is
   held only by an attempt in a worker-holding state.
3. Connections: `ConnHeld == (attempt in pool Held set)`; a held connection
   implies `RUNNING`; waitlist membership requires
   `WAITING_CONNECTION` and the right upstream.
4. Queues: entries reference known, `QUEUED`, right-tenant attempts; no
   duplicates; queue length equals the per-tenant queued aggregate.
5. Lane bounds counting pending books:
   arrival lane ≤ `MaxArrivalQueueDepth`,
   retry lane ≤ `MaxRetryQueueDepth`,
   total (all lanes + both pending books) ≤ `MaxTotalQueueDepth`.
6. Pending books: per-tenant counts equal the created-state pending sets;
   pending attempts are `Created` and carry the matching lane flag.
7. Probe ledger **exact**: `Σ attempt.IsProbe == Breaker.ProbesActive`
   per upstream — no slack, no underflow guards.
8. Outcome ledger exact: per-tenant tallies recomputed from the attempt
   registry equal `OutcomeCounters` exactly (both directions).
9. Request validity: attempt lists match the back-linked registry;
   terminal-state/outcome coherence.
10. Tenant accounting: `TenantActive == non-terminal request count`,
    non-negative.

Corruption detection is deliberately tested per family:
`TestValidateDetectsProbeAndLedgerCorruption`,
`TestValidateDetectsWaitlistAndPendingLedgerCorruption`.

## 7. Event taxonomy (M1 baseline + M2 additions)

Every kind is a distinct Go type (`sim/events.go`); handlers switch
exhaustively over types and panic on unknown kinds.

| Event | Class | Scheduled by | Effect |
|---|---|---|---|
| `RequestArrival` | Work | external/scenario | create request+first attempt; run admission; emit AdmissionDecided + QueueInsertion or LoadShed; arm DeadlineExpired (or expire immediately when deadline==arrival) |
| `AdmissionDecided` | Work | arrival | observation of the decision (it is applied atomically in the arrival) |
| `QueueInsertion` | Work | arrival / retry execution | queue the attempt; make request QUEUED; probe dispatch |
| `QueueRemoval` | Work | dispatch probes | pop the named attempt, claim dispatch, emit WorkerAssignment |
| `WorkerAssignment` | Work | queue removal | claim worker slot (FIFO), request DISPATCHED, emit ExecutionStart |
| `ExecutionStart` | Work | worker assignment | fix exec duration; arm Timeout (covers future waiting time too); acquire connection or pin waiting |
| `ExecutionCompletion` | Work | execution start / connection grant | result per upstream state; release conn+worker; breaker bookkeeping; success settles the request, failure schedules retry |
| `Timeout` | Timeout | execution start | terminate running/waiting attempt; release resources; breaker counts a failure; retry decision |
| `CancellationRequested` | Governance | external/scenario | retract queued/waiting attempts via the single retraction path; orphan executing attempts; acknowledge; **unknown request → recorded deterministic no-op (A14)** |
| `CancellationAcknowledged` | Governance | cancellation request | if nothing is in flight, terminalize CANCELLED; otherwise remain CANCELLING until the last orphan finishes |
| `RetryScheduled` | Work | completion/timeout handlers | record retry decision; schedule delayed RetryExecution |
| `RetryExecution` | Work | retry scheduled | re-admit the retry against lane/breaker gates (§4); blocked → explicit failure |
| `DeadlineExpired` | Governance | arrival (armed once) | expire request; retract queued/waiting attempts; orphan executing ones |
| `UpstreamDegraded` / `UpstreamRecovered` | Model control | external/scenario | toggle availability; affects exec latency at next start and results at next completion |
| `BreakerTransitioned` | Model control | completion/timeout observation, cooldown | apply transition; OPEN additionally schedules half-open at cooldown end; both re-open and fresh half-open reset the probe permit ledger exactly; stale transitions ignored |
| `LoadShed` | Work | arrival | observation of the admission rejection |
| `ConnectionGranted` | Work (auxiliary) | connection releases | **payload-free wake**: names only the upstream; the handler selects the oldest state-valid waiter from the waitlist at processing time (A15) |
| `UpstreamLatencySet` | Model control (M2) | fault-schedule materialization | latency ≥ 1: every *new execution start* on the upstream uses the override duration (completions succeed); 0: restore the M1 health-based selection |
| `UpstreamFailureSet` | Model control (M2) | fault-schedule materialization | EveryN ≥ 1: every k-th *completion* of that upstream fails (k = the upstream's completion counter; failures of any origin advance it); 0: disarm and clear the counter |

The full taxonomy is 20 kinds; 5 of them are Model-control. Same-instant
ordering rules are unchanged: a fault *restore* and the next episode's *set*
order by the standard classes at their shared instant, and the materializer
emits restore-before-set so state sequence carries the handover
(`docs/SYSTEM_MODEL.md` §6.1).

### Handler contract

`engine.Handler.Handle(ev Event, seq uint64) []Event`: `seq` is the
scheduling sequence number of the consumed event and anchors the evidence
ledger (§5). Handlers must never observe wall-clock time, must not mutate
already-scheduled events, and may only schedule `At() >= now`.

### Transitions deferred to later milestones

The taxonomy is the *existing* set. Events that later milestones are expected
to add here (without changing the core ordering contract) include: schedule
change / gateway configuration events, admission policy revision events,
tenant priority re-weighting, acceptance/proposal/adjudication events of the
governance layer, experiment-controller events for metastability studies.
None of these exist in code today.

## 8. Repeatability

Identical engine inputs ⇒ identical event order ⇒ identical handler
invocations ⇒ identical state and record trace. This is asserted at both
levels: engine-only (`TestRepeatabilityOfIdenticalEngineInputs`) and for the
whole reference system including a degradation, retries, cancellation,
shedding and a late-completion sandbox
(`TestReferenceSystemRepeatabilityFromIdenticalInputs`). The zero-backoff
degenerate chain is separately proven bounded and deterministic
(`TestZeroBackoffSameInstantRetryChainIsBoundedAndDeterministic`,
`TestZeroBackoffChainRunsAreIdentical`).

## 9. Milestone 2: deterministic experiment inputs (M2)

The engine core is untouched. M2 wraps it:

- **Workload build.** `WorkloadSpec.Build(horizon)` produces the initial
  event list from simulated-time arithmetic plus the seeded PRNG only.
  Window count is exact integer/carry arithmetic (fractional rates are never
  sampled away); jitter samples are the only PRNG consumers before deadline
  drawing, and a fixed deadline (`min == max`) consumes nothing — changing a
  fixed deadline cannot move arrivals.
- **Fault schedules.** `FaultSchedule.Events(horizon)` materializes typed
  Model-control events (see §7 and `docs/SYSTEM_MODEL.md` §6.1–6.2).
  Schedules are flat data; validation rejects malformed episodes, overlaps
  on one upstream, and `Until` beyond the horizon.
- **Experiment runs.** `RunExperiment` resumes ONE engine run across the
  sorted phase/recovery/horizon boundaries and validates the full state
  invariants at every boundary (it panics on any violation — result
  computation never runs on an invalid state). All summaries, phase reports,
  recovery analyses and occupancy integrals are computed POST-HOC from the
  final state and the record ledger; they never mutate the state (pinned by
  `TestRecoveryAnalysisDoesNotMutateState`).
- **Replay.** `ReplayCheck` runs the experiment twice on fresh state and
  DeepEqual-compares summaries, phase reports, the full record ledger and
  recovery reports; every library scenario passes it
  (`TestReplayCheckAllScenarios`).
- **Metrics conventions.** Goodput counts *logical requests* (terminal
  completed with `TerminalAt ≤ Deadline`); amplification is
  `PhysicalAttempts/Admitted` (NaN on 0/0 — undefined, never faked); a
  successful *attempt* is never conflated with a successful *request*.
  Occupancy integrals are exact piecewise-constant integrals over the
  timelines; percentiles are the discrete rank-`ceil(q·N)` sample without
  interpolation; the P50/P90/P99 of QueueWait come from the per-tenant
  log2 histograms and are documented approximations (each bucket's upper
  bound stands for its samples). Blocked retries are recorded as
  `"retry-admission"` notes and counted separately from retries and sheds.
  Breaker transitions carry `From`/`To` in their `Kind: "breaker"` note
  detail; ordinary failure counting also lands there as observations only.

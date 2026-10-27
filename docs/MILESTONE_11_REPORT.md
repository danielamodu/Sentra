# Milestone 1.1 — Correctness and Evidence Integrity: Remediation Report

Status: **complete**. Every defect found by the Milestone 1 adversarial
review has been fixed, demonstrated by a named regression test, and the
whole suite passes. Two externally observable semantics changed as
intended (same-instant ordering semantics A13; retry re-admission, A16)
and are documented in `docs/ASSUMPTIONS.md` and
`docs/ENGINE_SEMANTICS.md`.

## Verification results

| Command | Result |
|---|---|
| `gofmt -w .` / `gofmt -l .` | formatted; `gofmt -l` prints nothing |
| `go vet ./...` | clean, no diagnostics |
| `go test ./... -count=1` | `ok sentra/domain`, `ok sentra/engine`, `ok sentra/sim` |

Sim suite has 46 test functions (18 baseline scenario tests updated to the
new config/recorder APIs, 17 dedicated remediation regressions, and the
11 evidence-ledger/ordering tests), plus 10 domain and 11 engine tests.
The repository is not a git worktree, so no commit hashes are cited; the
implementation lives in the tree as listed below.

## Finding → implementation → regression test → verification

Findings are grouped by area as in the review (F1 correctness/resource
cleanup, F2 semantics & docs, F3 domain independence tiers, F4 evidence
schema, F5 validation, F6 fairness metrics). Every implemented fix has its
demonstrating test; no fix is claimed anywhere without one.

### F1 — Correctness blockers (lifecycle and resource integrity)

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F1.1 Retraction of a waitlisted attempt stranded its pinned worker and leaked the waitlist member | `retractAttempt` is the single, idempotent retraction exit path (queue entry, waitlist membership, pending-lane book, probe permit, and — for `WAITING_CONNECTION`, which holds a worker — the worker); Created/Queued/WaitingConnection/Dispatching retract, Assigned/Running are orphaned instead | `TestCancellationRetractsWaitingConnectionAttempt`, `TestDeadlineRetractsWaitingConnectionAttempt` | PASS |
| F1.2 Retracting a queued half-open probe leaked its breaker permit, shedding the next legitimate probe | Permit release moved into `retractAttempt`/`finishAttempt` via `releaseProbePermit`; re-open and fresh half-open clear outstanding probe flags deterministically by attempt ID | `TestHalfOpenProbeExpiryRecoversPermit` | PASS |
| F1.3 Same-instant double connection grant named the same waitlist head; the second waiter could never run | `EventConnectionGranted` is payload-free (upstream only); the handler pops the oldest *state-valid* waiter at processing time | `TestTwoSimultaneousConnectionReleasesGrantBothWaiters` | PASS |
| F1.4 Stale wake event named a waiter whose eligibility window had closed, stranding the connection while a later waiter stayed pinned | Payload-free wake (F1.3) + stale-entry skipping in `handleConnectionGranted`; logs `no eligible waiter; connection unused` when nothing qualifies | `TestStaleWakeSelectsOldestEligibleWaiterAtProcessingTime` | PASS |
| F1.5 `deadline == arrival` panicked at admission | Inline immediate expiry (request `ADMITTED → EXPIRED`, reason `deadline-at-arrival`); no attempt, no armed `DeadlineExpired`; `deadline < arrival` remains a scenario bug and still panics | `TestDeadlineEqualsArrivalExpiresImmediately` | PASS |
| F1.6 Cancellation of an unknown request panicked (map lookup) | Deterministic, recorded no-op; cancellation addresses requests by belief, not registry fact | `TestCancellationOfUnknownRequestIsRecordedNoOp` | PASS |
| F1.7 Single shared `MaxQueueDepth` let same-instant retry executions overshoot and silently overflow the lane | 3-lane `TenantConfig` (`MaxArrivalQueueDepth`, `MaxRetryQueueDepth`, `MaxTotalQueueDepth`; NewState requires total ≥ each lane) with per-lane *pending books* so the second same-instant retry admission is seen and blocked | `TestRetryLaneBoundBlocksSecondSameInstantRetry`, `TestTotalQueueBoundBlocksRetryEvenWithRetryLaneRoom` | PASS |
| F1.8 Retries executed against OPEN/HALF_OPEN breakers and consumed probe permits implicitly | `handleRetryExecution` re-admits the retry at its execution instant; OPEN → `retry-blocked-breaker-open`, HALF_OPEN → `retry-blocked-breaker-halfopen` (retries are never probes), with recorded outcomes (`Metrics.Sheds`) and explicit request failure | `TestRetryBlockedWhenBreakerOpen`, `TestRetryBlockedWhenBreakerHalfOpen` | PASS |
| F1.9 Zero retry backoff was not bounded/deterministic; a same-instant retry chain could behave degenerately | Zero backoff chain fully bounded by budget + lane gates; proven deterministic at both state and record level | `TestZeroBackoffSameInstantRetryChainIsBoundedAndDeterministic`, `TestZeroBackoffChainRunsAreIdentical` | PASS |
| F1.10 Far-future scheduling overflowed int64 nanoseconds silently (wrap-around into the past) | Checked arithmetic helper `addT` at all four scheduling sites (timeout, completion, backoff, cooldown) over `SimTime.AddChecked`; overflow panics with the distinct "simulated time overflow" message, never the generic past-scheduling error | `TestSimulatedTimeOverflowHasDistinctError` | PASS |

### F2 — Semantics and documentation integrity

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F2.1 `ENGINE_SEMANTICS.md` §2 and `ASSUMPTIONS.md` A13 described a spurious "injected events run before generated events at the same instant" rule | Doc rewrite: ordering is purely `(time, priority, seq)` for injected **and** generated events; A13 replaced with the tuple-only semantics and the rejected alternative recorded | `TestGeneratedEventsObeyPriorityNotGenerationOrder` (engine-level proof of documented order) | PASS |
| F2.2 Priority classes were asserted only by spot-stamping specific kinds | Exhaustive coverage: all 18 kinds sampled exactly once, classified into the four constants; distribution pinned at control 3 / governance 3 / timeout 1 / work 11; distinct constants required strictly ordered | `TestEventPriorityClassesAreExhaustive` | PASS |
| F2.3 No same-timestamp generated-ordering scenario existed | Dedicated scenario: injected gov+work at one instant, generation order `[gwork, ggov]`, executed order `igov → ggov → iw → gwork` | `TestGeneratedEventsObeyPriorityNotGenerationOrder` | PASS |
| F2.4 Docs claimed handler contract `Handle(Event)`; engine now hands the scheduling seq for evidence anchoring | `engine.Handler.Handle(ev, seq)`; contract documented in `engine/event.go` and `ENGINE_SEMANTICS.md` §7 | `TestReferenceSystemRepeatabilityFromIdenticalInputs` (signature change exercised end-to-end), all engine tests | PASS |
| F2.5 Assumption docs claimed "retries do not re-run admission" and "probe underflow guarded" both of which are now false | `ASSUMPTIONS.md` section B rewritten: explicit retry re-admission (A16) and exact probe ledger (A9 hardening) | — (doc-accuracy item; enforced behaviorally by F1.7/F1.8 tests and the validator family F5) | done |

### F3 — Domain export independence tiers

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F3.1 No tier marking distinguished safe-to-share contracts from decision-bearing policy | Tier-3 INDEPENDENCE RISK markers with rationale placed in-source on the three decision-bearing artifacts: `CircuitBreaker` (transition matrix), `RetryBudget.TryConsume` (retry bound), `Upstream.ExecutionLatency` (failure model) | — (marker review item; ARCHITECTURE.md §3 matrix cross-references them) | done |
| F3.2 Independence contract did not name what an independent simulator must NOT reuse | `docs/ARCHITECTURE.md` §3 independence matrix: tier 1 (data carriers), tier 2 (FIFO mechanics + transition *tables*, shared by design), tier 3 (must re-derive); `sim/handlers.go` excluded outright | `TestReferenceSystemRepeatabilityFromIdenticalInputs` keeps the compare surface deterministic, the prerequisite of the cross-check | PASS |

### F4 — Evidence schema and serialization

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F4.1 Recorder entries were unversioned free-format `{Index, Time, Kind, Message}` strings | `Record{SchemaVersion=1, EffectSeq, EngineSeq, SimTimeNS, Category(event|note), Kind, Subject, Detail}` with typed `Detail` fields (request/attempt/tenant/upstream/worker/from/to/reason) | `TestRecordSchemaIsVersionedAndGaplessOrdering` | PASS |
| F4.2 Detail fields were `fmt` strings (`%v` of event structs) — unparseable | Explicit typed serialization switch (`marshalEvent`/`marshalSubject`); unknown kinds panic, no `%v` structural dumps | `TestEventRecordsAreTypedNotStructDumps`, `TestLoadShedRecordCarriesRequestAndReason` | PASS |
| F4.3 No streaming sink; everything accumulated in memory unboundedly | `Sink` interface with `RingMemorySink` (default 8192, oldest eviction) and `JSONLSink` (bufio, flush on Close); sticky-error policy on write failure | `TestRingSinkRetentionIsBoundedAndOrderPreserving`, `TestEvictionDoesNotPerturbEffectSeq`, `TestRecorderPanicsOnSinkWriteFailure` | PASS |
| F4.4 `engine_seq` ↔ `effect_seq` treated as 1:1 | Correlation contract N:1: engine hands seq to handler (`Handle(ev, seq)`), every record of the processing step carries it; tested that ≥2 effects share one engine_seq | `TestEffectsCorrelateWithOriginatingEngineEvent` | PASS |
| F4.5 Record JSON was not round-trippable against the in-memory ledger (or not equivalent) | Canonical one-line JSON; buffered JSONL run compared `DeepEqual` against ring run; key set pinned | `TestRecordJSONRoundTripThroughJSONLSink` | PASS |

### F5 — Validation coverage

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F5.1 `Validate()` was nested-rescan (O(N²)-ish) and missed ledger families | Single-pass O(N) rewrite over the attempt registry building all aggregates once | all tests that call `Validate()` (mid-run and end-of-run) | PASS |
| F5.2 Missing probe-permit checks | Exact ledger: Σ `IsProbe` == `ProbesActive` per upstream (both directions), range-checked | `TestValidateDetectsProbeAndLedgerCorruption` | PASS |
| F5.3 Missing outcome-ledger checks | Exact two-direction check per configured tenant; missing counters book detected (books pre-seeded in `NewState`) | `TestValidateDetectsProbeAndLedgerCorruption` | PASS |
| F5.4 Missing waitlist/pending-book/queue membership checks | Empty waitlist entries rejected; pending books (arrival + retry) checked both per-tenant counts and per-attempt lane/state consistency; queue entries checked against `QUEUED` state and tenant | `TestValidateDetectsWaitlistAndPendingLedgerCorruption`, `TestQueueMembershipConsistency` | PASS |
| F5.5 Missing lane-bound checks | Arrival/retry lane and total bounds (with pending books) validated against config | `TestRetryLaneBoundBlocksSecondSameInstantRetry` (bound closure), `TestWorkerAccountingUnderContention` (mid-run validation) | PASS |
| F5.6 Tenant-active accounting unchecked | `TenantActive == non-terminal request count`, non-negative | `TestCancellationRetractsQueuedAttemptImmediately`, `TestCancellationRetractsWaitingConnectionAttempt` (active count 0 after cancel) | PASS |

### F6 — Fairness / per-tenant evidence

| Finding | Implementation | Regression test | Result |
|---|---|---|---|
| F6.1 No per-tenant queue-wait distribution | `TenantMetrics.QueueWait{Sum,Max,N,Hist}` (log2 buckets); recorded at dispatch (zero wait included) | `TestPerTenantQueueWaitDistributionAndDispatchShare` | PASS |
| F6.2 No dispatch-share observability | Dispatch share folded into wait accounting, `Dispatched == QueueWaitN` by construction | `TestPerTenantQueueWaitDistributionAndDispatchShare` | PASS |
| F6.3 Shed reasons not attributed per tenant | `Metrics[t].Sheds` reason mix for both admission and retry-admission outcomes | `TestShedReasonsAreRecordedPerTenant`, `TestRetryBlockedWhenBreakerOpen` | PASS |
| F6.4 No useful-work share per tenant | `OutcomeCounters[t].Useful` surfaced in `FairnessSnapshot` (deterministically tenant-sorted) | `TestUsefulShareAndJainIndex` | PASS |
| F6.5 No fairness metric across tenants | `JainIndex` over the useful vector, with the explicit NaN contract (empty/all-zero vector are NaN, never fake 0) | `TestUsefulShareAndJainIndex` | PASS |
| F6.6 Starvation was observable only via end-state | Mid-run and post-dispatch assertion that a starved tenant's dispatch share stays zero while queued, then records the exact wait | `TestPerTenantQueueWaitDistributionAndDispatchShare` | PASS |

## Public API / config changes (compatibility notes)

- `TenantConfig` changed from a single `MaxQueueDepth` to the three-lane
  shape — **no compat shim was kept**; all scenarios updated.
- `engine.Handler` signature changed to `Handle(ev, seq)` for the
  correlation contract.
- Recorder API replaced (`Entry`/`Log(format)` → `Record`/`Log(detail)`,
  `FormatEntries` → `FormatRecords`); the sim's default sink is the
  bounded ring so existing callers see the same "records exist" behaviour.
- Transition tables gained `ADMITTED → EXPIRED`, `RETRYING → FAILED`,
  `CREATED → RETRACTED`, `DISPATCHING → RETRACTED` (each directly
  exercised above).

## Unresolved decisions (explicitly deferred, not silently dropped)

1. **Refund policy for blocked-retry budget tokens** — implemented
   non-refundable (A16) for bound integrity. Governance may later want a
   distinct accounting line ("attempted but refused") instead of pure loss;
   requires a semantics decision before implementation.
2. **Jain index target set** — currently the useful-outcome vector.
   Dispatch-share or queue-wait-based fairness indices are plausible
   alternates; awaiting the governance milestone's definition of "fair".
3. **Same-instant arrival shed accounting** — the second of two same-instant
   arrivals sheds when the arrival lane holds exactly 1 slot (the first is
   already pending in the book). Outcome-correct for the bound, but the
   first-come-wins tie at an identical instant is scheduling-seq quantum,
   not policy; deserves a revisit when tenant priority (later milestone)
   arrives.
4. **Recorder sink rotation/compaction for very long runs** — bounded ring +
   JSONL exist; no rotation story yet (noted limitation).
5. **Ring capacity as config vs constant** — currently a package constant
   (8 192) and test-choosable; making it `SystemConfig` requires the
   experiment-runner design.

## Prohibited-scope confirmation

No independent simulator, no metastability detection, no AI agent, no
GitLab governance, and no optimization engine were added. The scope of this
milestone was strictly the remediation above plus its evidence, docs and
report.

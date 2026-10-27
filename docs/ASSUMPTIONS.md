# Assumptions, Decisions and Open Questions (Milestones 1 & 2)

Per the engineering standard, nothing here was silently chosen: each
ambiguity that materially affects behaviour is listed with the decision made
in M1, its rationale, and the alternative that was rejected (or the note that
it must be resolved later).

## A. Resolved decisions (with rationale)

### A1. Deadline vs completion at the same instant
Decision: the deadline (Governance class) wins; the same-instant completion
is late — the attempt completes (`COMPLETED`) but the outcome is `WASTED`.
Rationale: a client-visible deadline is the contract; results at or beyond
it have no user. Alternative rejected: trusting a result that lands exactly
on the deadline (optimistic boundary) would make SLO accounting ambiguous.

### A2. Timeout vs completion at the same instant
Decision: the timeout wins; the completion event is ignored.
Rationale: pessimistic boundary — consistent with A1, avoids double-
accounting a result that raced the deadline-driven retry decision.

### A3. Cancellation vs deadline at the same instant
Both are Governance class; scheduling order decides. Sub-decision: a
deadline arriving after an accepted cancellation is ignored (the request is
already CANCELLING; expiry would be redundant). Alternative rejected:
expiry upgrades CANCELLING to EXPIRED — this would erase cancellation
provenance for no operational benefit. If governance later needs to
distinguish "would have expired anyway", it can be derived from the
recorder.

### A4. Timeout semantics for waiting attempts
The attempt timeout covers the whole post-assignment phase, including time
pinned waiting for a connection (it is armed at `ExecutionStart` and covers
WAITING_CONNECTION). Rationale: the client is already paying the latency,
and pinned-worker resource cascades are exactly what the model must expose.
Alternative rejected: a separate connection-wait timeout — deferred until a
milestone motivates a distinct knob, with its own tests.

### A5. Retry decision checks, in order
Retry limit → logical deadline (`t < deadline`, strict) → retry budget.
The budget token is consumed only when a retry will actually be scheduled.
Alternative rejected: consuming on attempt (would waste tokens on
policy-blocked retries and skew the amplification bound).

### A6. Retry ordering / late sibling completions
The first success to reach a live request completes it; remaining active
attempts are orphaned and accounted WASTED when they finish, and pending
retry executions of the now-terminal request are ignored (observable no-op
events). Clarifies that "first success wins" is deterministic.

### A7. In-flight execution is not interrupted
Cancellation and expiry retract queued/waiting work but never kill executing
attempts; those finish and are accounted WASTED. Rationale: models real
systems where in-flight downstream calls cannot be (reliably) aborted, and
gives the wasted-work accounting its raw material. Alternative rejected:
instant cancellation of in-flight attempts — hides the cascade the project
exists to study.

### A8. Acknowledgement semantics
`CancellationAcknowledged` is emitted at the instant the cancellation is
applied; `Outstanding` reports executing-attempt count. Zero → the request
terminalizes CANCELLED at the ack; non-zero → it stays CANCELLING until the
last orphan finishes (reconciled after every attempt finalization).
Alternative rejected: deferring the ack until full settle (clients cannot
distinguish "lost" from "settling").

### A9. Probe permits at half-open permits are reserved at admission
A request admitted while the breaker is HALF_OPEN reserves its permit at
admission time (not at execution start) and releases it when its attempt
terminates (completed, failed or timed out). Reopening the breaker consumes
outstanding permits; permit decrements are guarded against underflow if
in-flight probes outlive an OPEN episode. First successful completion while
HALF_OPEN closes the breaker; any failure reopens it.
Alternatives (explicitly deferred): per-probe success quotas, requiring N
probe successes, half-open admission by permit at execution start.

### A10. Queue re-insertion on dispatch contention
Dispatch probes schedule at most `idle − pendingClaims` removals; a removal
whose worker vanished by processing time degrades to an observed no-op and
the attempt is reconsidered by the next probe (post-completion, post-
insertion). Rationale: avoids stale worker reservations in event payloads.
Alternative rejected: provisional worker reservation at removal scheduling —
adds a claim bookkeeping state to attempts for no observed M1 benefit.

### A11. M1 admission policy
Deterministic, configurable: shed when (a) the target breaker is OPEN, (b)
the breaker is HALF_OPEN with all probe permits reserved, or (c) the
tenant's queue is at `MaxQueueDepth` at the arrival instant. Fairness across
tenants at dispatch: globally oldest-inserted entry first (shared sequence
number) — deliberately *not* tenant-fair; see C3.

### A12. Deterministic M1 failure model
An attempt's duration is fixed at execution start (`NormalLatency` vs
`DegradedLatency` per upstream state at that instant); its result is a
failure iff the upstream is still degraded at the completion instant. This
is an explicit, reproducible stand-in for a stochastic model. Rationale:
reproducibility and experiment legibility. Replacement (later milestone,
with determinism preserved via seeded RNG): latency distributions, partial
failure, jitter.

### A13. Same-instant ordering is purely the tuple (injected and generated alike)
Events at the same simulated instant are ordered **only** by
`(priority, scheduling seq)`. There is no injected-precedence rule: a
handler-*generated* event competes with still-unprocessed *injected* events
of the same instant by its own priority class first and its later seq
second. Consequence: at an instant where an injected governance event and
an injected work event both sit in the queue, and the governance handler
generates a fresh work event and a fresh governance event, the executed
order is igov → ggov → iw → gwork — the generated governance outranks the
injected work, and the injected work outranks the later-scheduled generated
work at the same primary class.
Rationale: one total order is simpler to reason about, easier to verify
independently, and matches the engine implementation; special-casing
injected events would add an ordering rule to the shared contract without
any requirement driving it.
Alternative rejected: "all injected events of an instant run before all
generated ones" — this was the superseded M1 doc text (now corrected); it
would also have made generated governance events (e.g. a same-instant
cancellation acknowledgement) run after unrelated injected work, which is
unacceptable for governance semantics.
Demonstrated by `TestGeneratedEventsObeyPriorityNotGenerationOrder` and
`TestEventPriorityClassesAreExhaustive`.

### A14. Lifecycle boundary decisions (expiry, cancellation, retraction)
Three corner decisions, all recorded/reproducible:
1. **Deadline equals arrival** → the request expires immediately at
   admission: no attempt is created, no `DeadlineExpired` event is armed,
   and the `AdmissionDecided` record carries reason `deadline-at-arrival`.
   Deadline < arrival remains a scenario programming error (panic).
   Rationale: expiry is definitional; an attempt could never start within
   a zero-length window, so creating one would manufacture wasted work.
   Alternative rejected: arming an immediate expiry event — the outcome is
   identical but the ledger, the request (it would briefly own an attempt)
   and the metrics would all carry fictitious entries.
   (`TestDeadlineEqualsArrivalExpiresImmediately`)
2. **Cancellation of an unknown request** → deterministic, recorded no-op:
   the request map is not polluted, the record kind is `cancellation` with
   description "deterministic no-op".
   Rationale: cancellation addresses a request the client believes exists;
   silently succeeding (with a record) is the least-surprise behaviour.
   Alternative rejected: panic — would make scenarios order-fragile.
   (`TestCancellationOfUnknownRequestIsRecordedNoOp`)
3. **Retraction is a single centralized exit path** (`retractAttempt`,
   `sim/handlers.go`): every retraction of an attempt that is not executing
   (Created/Queued/WaitingConnection/Dispatching) goes through it, and it
   releases queue entry, waitlist membership, pending-lane book, and probe
   permit exactly once (idempotent). Executing attempts (Assigned/Running)
   are never retracted — they are orphaned by design (A7). A
   `WaitingConnection` attempt holds its worker, so the retraction path
   releases it (the M1 defect where these workers were stranded is the exact
   case fixed here).
   Rationale: exits duplicated release logic before; centralizing it made
   the stranded-worker/waitlist-leak defects structurally impossible.
   (`TestCancellationRetractsWaitingConnectionAttempt`,
   `TestDeadlineRetractsWaitingConnectionAttempt`,
   `TestHalfOpenProbeExpiryRecoversPermit`)

### A15. Connection wakes are payload-free
`ConnectionGranted` carries only the upstream ID: the release instant
cannot know which waiter is still eligible when the grant event runs
(events never change position once scheduled), so the handler selects the
oldest *state-valid* waiter from the pool's waitlist at its own processing
time, skipping stale entries and logging "no eligible waiter; connection
unused" when there is none.
Rationale: fixes the same-instant release race (two releases at one instant
used to name the same head at release time; the second waiter could never
run) and the stale-wake stranding (a waitlist head that timed out before
the wake ran used to strand the connection).
Alternative rejected: naming the attempt in the event — stale payloads are
exactly what caused both defects.
(`TestTwoSimultaneousConnectionReleasesGrantBothWaiters`,
`TestStaleWakeSelectsOldestEligibleWaiterAtProcessingTime`)

### A16. Retry admission is explicit; the budget token stays spent
A retry attempt is admitted **at its execution instant** (§4 of engine
semantics). Gates in order: breaker OPEN → `retry-blocked-breaker-open`;
breaker HALF_OPEN → `retry-blocked-breaker-halfopen` (retries can never be
probes); retry lane full → `retry-queue-full`; total book full →
`retry-queue-total-full`. A blocked retry fails its request explicitly
(`RequestRetrying → RequestFailed`) and is counted in `Metrics[t].Sheds`.
The budget token consumed when the retry was scheduled is **NOT
refunded** — the check "will a retry actually be scheduled" (A5) passed
conservatively before the admission conditions were known.
Rationale: refunding on block would couple the scheduling-time budget
decision (A5) to execution-time conditions, re-introducing the same-instant
overshoot the lane books exist to close; the conservative semantics keeps
the bound airtight at the documented cost of not re-crediting blocked
retries. This *replaces* the former M1 simplification "retries do not
re-run admission" (section B) — the re-admission required explicit
recorded outcomes instead of silent overshoot.
(`TestRetryBlockedWhenBreakerOpen`, `TestRetryBlockedWhenBreakerHalfOpen`,
`TestRetryLaneBoundBlocksSecondSameInstantRetry`,
`TestTotalQueueBoundBlocksRetryEvenWithRetryLaneRoom`)

### A17. Checked simulated-time arithmetic with a distinct error
All four far-future scheduling sites (attempt timeout, execution
completion, retry backoff, breaker cooldown) go through `Sim.addT` →
`SimTime.AddChecked`: an int64 nanosecond overflow panics with a message
containing "simulated time overflow", distinct from the generic
past-scheduling invariant error, so a boundary-heavy scenario fails with a
diagnosable panic instead of silently wrapping.
Rationale: `Add` alone would wrap and produce a maximally-negative (past!)
event, converting an overflow into a confusing panic about scheduling in
the past.
Alternative rejected: saturating to MaxInt64 — silently schedules an
"effective never" timeout, laundering a real overflow into a quiet
behaviour change.
(`TestSimulatedTimeOverflowHasDistinctError`; `domain/time.go`,
`ErrTimeOverflow`)

### A18. M2 fault modes extend A12 without changing it
Three modes: slow-but-succeeding is a NEW mode (`UpstreamLatencySet`,
completions SUCCEED); the M1 degraded episode remains the complete-
unavailability mode; and `UpstreamFailureSet{EveryN}` fails every k-th
completion at normal latency via a deterministic per-upstream counter.
Rationale: the milestone requires slow and elevated-failure probes; reusing
"degraded" for slow would have silently changed A12's semantics. The
slow-mode bypass of the completion-fails-while-degraded rule is implemented
at exactly one decision point and documented.
Alternative rejected: a generic "fault factor" flag — it would have folded
three different mechanisms into one flag and lost per-mode restore
semantics.
Resting point: any future change must keep A12's degraded semantics
byte-identical, and per-mode restore must stay local.
(`TestFaultLatencyAppliesAndRestores`, `TestFaultFailRateDeterministicCounter`)

### A19. Fault schedules are set/restore pairs of each episode's own mode
Restore-at-`Until` (including `Until == horizon`) keeps each episode's
footprint local; no global reset event exists. Adjacent episodes hand over
by ordering alone (restore before set at the shared instant), and same-
upstream overlaps are REJECTED so the applied mode cannot encode list order.
Rationale: schedules are flat, order-independent recorded data; any
"later episode wins" rule would make inputs order-sensitive for no benefit.
(`TestFaultScheduleRestoreOrdering`,
`TestFaultScheduleRejectsSameUpstreamOverlap`)

### A20. Fractional arrival rates are exact, not sampled
Window counts propagate through an integer carry (rate 0.4/window over 10
windows ⇒ exactly 4 arrivals). Rationale: counts, not statistical
behaviour, are the experiment regression surface; rounding would make every
count seed-dependent for no experimental value.
(`TestWorkloadFractionalRateExactness`)

### A21. The seeded PRNG is splitmix64 with explicit (seed, stream) keys
No `math/rand`, no wall-clock seeding: the "no uncontrolled randomness"
boundary is preserved and replays are bit-identical. Distinct consumers use
distinct streams so drawing jitter never shifts deadline sampling.
Rationale: reproducibility is an M2 deliverable, not an implementation
accident. (`TestPRNGIsDeterministicPerSeedAndStream`,
`TestPRNGStreamSeparation`, `TestWorkloadFixedDeadlineDoesNotConsumePRNG`)

### A22. Recovery analysis is post-hoc and never resets state
The analyzer reads the final state and the record ledger and is a pure
function (DeepEqual before/after, pinned); "recovery" reports measure what
actually happened. There is no between-phases cleanup in the experiment
machine — the backlog, budgets, breakers and occupied resources a fault
left behind are exactly what the post phase must overcome.
Rationale: a state reset between phases would manufacture recovery, which
is the one thing the experiments must not do.
(`TestRecoveryAnalysisDoesNotMutateState`, `TestNoStateResetsAcrossRecovery`)

### A23. Scenario calibration is input calibration, not result plumbing
Designs were re-drawn (rates, windows, pre/post phases) where a hypothesis
was blind to the mechanism — NOT to make results match, but to make the
probe address the claimed regime (D's overload continued past recovery; F
had no healthy baseline against which to read recovery). No code path
special-cases a scenario; every scenario is measured by the same machinery.
Resting point: hypotheses quote measured rates at calibration time only;
the report is the arbiter, never a test helper.
(`TestObservedClassesPinTheScenarios`, `TestClassifierMatchesMeasuredRates`)

### A24. Recovery classes are thresholded measurements, horizon-relative
immediate / delayed-but-eventual / sustained-collapse-within-horizon, plus
the honest refuses no-baseline and no-post-window; threshold: post
goodput-rate ≥ 0.5 × pre rate. "Within-horizon" is part of the class name
because the horizon is the observation window, not a claim about infinity.
Rationale: classification must follow recorded numbers, never ahead of
them; the alternative ("eventually recovers" claims) is unobservable in a
finite run. Threshold changes must re-run the library and update the
report tables. (`TestClassifierMatchesMeasuredRates`)

### A25. Blocked retries keep their budget token spent (A16) and are counted
The measurement adds `BlockedRetries` and `"retry-admission"` notes without
changing A16. Rationale: the amplification experiments must distinguish
"retry attempted", "retry started" and "retry rejected at admission"; a
refund would let rejected retries repeat forever against a breaker — an
unbounded retry loop.
(`TestBlockedRetriesCountedAsAdmissionRejections`)

### A26. Percentiles are discrete (rank ceil(q·N), no interpolation)
E2E percentiles are exact over per-request samples; QueueWait P50/P90/P99
are computed from per-tenant log2 histograms (each bucket's bound stands
for its samples) and are documented approximations. Rationale: exact
discrete ranks are defensible without distributional claims; the histogram
cost/precision tradeoff is stated rather than hidden.
(`TestDiscretePercentileConvention`)

### A27. FIFO dispatch is retained as the fairness baseline
M2 MEASURES cross-tenant interference and waste under the existing FIFO
baseline (Jain index over useful outcomes, per-tenant profiles); alternative
scheduling/priority policies are explicitly NOT implemented in M2.
Rationale: the milestone asks for interference measurement under the
baseline, not for remediation. (`TestScenarioGInterference`)

### A28. The verifier re-declares lifecycles; it does not call domain decisions
`verify/lifecycle.go` declares its own state sets and transition matrices from
`docs/SYSTEM_MODEL.md`. The verifier never calls `domain.CanTransitionRequest`,
`domain.CanTransitionAttempt` or `SystemState.Validate`. Rationale: a verifier
that delegates to the reference's decision code accepts whatever that code
believes, including a future defect; independence would become cosmetic.
(`TestPackageIndependence`, `TestReconstructionIllegalTransition`)

### A29. D1: `attempt-created` carries typed identity (additive evidence)
The creation note is written with typed `Request`, `Attempt`, `Tenant`,
`Upstream` and `Lane` fields; the rendered description is byte-identical to the
old `Logf` form and no behaviour, ordering or state changes. Rationale: parentage
must be established from typed data, never prose. Pre-D1 evidence remains
readable through the block-local fallback.
(`TestReconstructionLegacyAttemptCreation`, `TestScenarioABridgeProducesVerifiableDocument`)

### A30. Lifecycle finding classification separates illegality from disagreement
An undocumented transition is `FAIL` / `reference-defect-candidate` (a contract
is violated on one path). A legal transition whose `from` does not match the
reconstructed state is `FAIL` / `unclassified` (the ledger and reconstruction
disagree; the evidence does not say which is wrong). Unknown states, missing
identities, contradictory typed identities and duplicate creation are
`INVALID_INPUT` / `evidence-defect`. Missing history is a gap, never a failure.
(`TestReconstructionContinuity`, `TestReconstructionIdentityConflictIsUninterpretable`)

### A31. A missing snapshot never passes a resource property
Every S-path property and every X-path comparison becomes `INCONCLUSIVE` with an
explicit gap when the snapshot is absent. Structural snapshot defects are
`INVALID_INPUT`; semantic violations are `FAIL`. Rationale: absence of evidence
must not read as satisfaction, and a structurally unusable snapshot cannot
support claims about another run.
(`TestSnapshotAbsenceIsInconclusive`, `TestSnapshotStructuralCorruptionIsInvalidInput`)

### A32. Cross-path disagreement is structured and blame-free
X-path findings name the property, entities, ledger reference, snapshot
reference and expected/observed values, and are classified `unclassified`. The
verifier never attributes a disagreement to the reference automatically.
Comparisons that need complete totals (budget consumption, probe balance, active
counts, reverse entity equality) are `INCONCLUSIVE` on a truncated ledger.
(`TestCrossPathDisagreementIsStructured`, `TestCrossPathInsufficientEvidence`)

### A33. Empty observation sets are evidence gaps
A lifecycle property whose universe is empty (no request or attempt observed) is
`INCONCLUSIVE`, not a vacuous `PASS`. Breaker lifecycle is the deliberate
exception: on a head-complete ledger, "no transition" establishes that the
breaker never left `closed`, which the snapshot corroborates.
(`TestVerifyPipelineStages`, `TestCrossPathAgreementDocumentIsInconclusive`)

### A34. History completeness is proven, not inferred
A retained ledger is `HistoryHeadTruncated` when it starts after `effect_seq` 0
and `HistoryTerminalUnestablished` when the head is complete but no terminal
marker exists. `HistoryComplete` requires both proofs and is unreachable under
evidence schema v1. A history beginning at `admitted`/`created` on a truncated
ledger is complete only when the typed creation evidence survived; state alone
and record contiguity are not proof. Head completeness, terminal completeness and
full history are three separate needs evaluated centrally by the executor. A
head-complete ledger never satisfies a terminal or full-history claim, so the
seven terminal-dependent X properties are `INCONCLUSIVE` in production.
A terminal boundary is never fabricated from the last record, contiguous
sequences or the snapshot timestamp.
(`TestHistoryCoverageTriState`, `TestHistoryCompletenessRequiresCreationEvidence`,
`TestRegistryShortfallEnforcement`)

### A35. The registry is the only property inventory
Property IDs, order, evidence sources, shortfall policy, counterexample radius
and test declarations live in one package-private registry. Evaluation iterates
that exact slice; coverage and category counts are derived from it. Coverage is
four-valued: `not-implemented`, `implemented-untested`, `declared-not-verified`
and `implemented-tested`. A declaration is bound only when it names the
behavioral test function and its case is executed by the suite; the binding is
installed by the test binary, so outside tests `P3Complete()` reports false
rather than assuming coverage. `P3Complete()` also requires every category A–H
to contain at least one implemented-and-tested property. The 27 current
properties satisfy the per-property gate, but F and H are empty, so P3 is not
complete.
(`TestRegistryStableInventory`, `TestRegistryCoverageStates`,
`TestRegistryDeclarationsAreVerified`, `TestP3RequiredCategoryGate`,
`TestRegistryFixtureVectors`, `TestPreMigrationFixtureFreeze`)

### A36. The bridge has zero authority and is deterministic
`sentra/evidence` copies observations verbatim, never repairs or suppresses, and
makes no property decision. Map-derived slices are sorted by identity; order-
bearing slices (queues, waitlists, idle lists, attempt lists) are preserved.
`RunResult.Records == nil` with processed engine events is a serialization
failure, not an empty ledger.
(`TestBridgeIsDeterministic`, `TestBridgeReportsAbsentLedger`, `TestBridgeDoesNotRepairIncompleteResult`)


## B. Deliberate simplifications (documented, unmotivated to fix in M1)

- **Single worker pool.** State keeps a pool map and a `mainPool()` path;
  multi-pool placement policy is unwritten (C2).
- **One shared breaker + retry-budget config** applied to all upstreams/tenant
  pairs; per-upstream/per-tenant overrides are config-shape work.
- **Timeout is uniform** (`AttemptTimeout`) for all attempts; per-upstream or
  per-request timeout policies are later decisions with tests.
- **Retry admission is explicit** (A16): retries re-run admission at their
  execution instant against breaker state, a dedicated retry lane and the
  total bound, with recorded reasons — this is no longer the old
  "no re-admission, transient overshoot" simplification.
- **Connection waitlist wakeups are payload-free** (`ConnectionGranted`
  names only the upstream, A15), so duplicate same-instant grants cannot
  both name the same waiter; they degrade to recorded no-ops without
  risking a waitlisted attempt being skipped silently.
- **Probe-permit accounting is exact** (A9, hardened): Σ `IsProbe` ==
  `ProbesActive` is enforced by `Validate()`, re-open/fresh half-open
  clear outstanding flags deterministically by attempt ID, and decrements
  panic on underflow instead of being guarded silently.

## C. Unresolved — must be decided before the affected milestone

1. **Cancellation sources and authorization.** Who cancels: client, gateway,
   governance verdict? M1 models cancellation as an external event with a
   reason string only.
2. **Multi-pool scheduling** (worker placement, tenant pinning, priority
   classes) — nothing implemented; single FIFO pool only.
3. **Cross-tenant fairness at dispatch** — currently global FIFO by arrival
   sequence. If tenant fairness/quotas are required, dispatch eligibility
   rules must be specified and tested.
4. **Routing.** Arrival events carry the target upstream directly; M1 has no
   routing, fan-out, hedging, or multi-dependency request shapes.
5. **Half-open probe success criteria** — single successful completion
   closes the breaker (A9). Governance may later require stricter probes.
6. **Retry backoff policy** — constant config value; jitter, exponential
   backoff and per-request retry strategies are unwritten.
7. **Deadline semantics relative to attempts** — request-level only. Attempt
   deadlines are the uniform attempt timeout (B); per-attempt slack or
   split deadlines are open.
8. **Wasted-work remediation** — behavior that would *react* to waste
   (e.g. pausing retries, deprioritizing orphan-heavy tenants) does not
   exist; M1 only accounts it.
9. **Load-shed request semantics afterwards** — shed requests are terminal;
   re-arrival, backpressure signaling and tenant accounting for shed bursts
   are open questions for the governance milestone.
10. **Degradation granularity** — an upstream is binary degraded/healthy;
    partial degradation (error rates, saturation) is a later model decision.

## D. Known limitations

- The engine validates "no scheduling in the past" but cannot detect a
  handler that behaves differently for identical inputs; independence is a
  design consequence (no I/O, no randomness), not a runtime check.
- `OutcomeCounters` are per-tenant only; per-upstream waste attribution is a
  trivial extension that awaits a use case.
- The default recorder sink is a bounded in-memory ring (8 192 records);
  streaming JSONL exists, but no rotate/snapshot/compaction story for very
  long runs is built — a later-scaling decision.
- No persistence, transport, deployment, or observability integration.
- Degradation affects results only at completion instants; an attempt that
  starts healthy and runs into a degradation completes successfully if the
  upstream recovers in between (A12). This is intentional and must be
  retained by any stochastic replacement.

Milestone 2 additions:

- The 8 192-record ring has not overflowed in the library (heaviest is G at
  1 888 records), but heavier scenarios could evict early records. Analyses
  that read the ledger (recovery budget accounting) are therefore counts
  over RETAINED records. Any experiment needing guaranteed full-ledger
  coverage must install a streaming sink — a use-case-driven decision, not
  a silent size bump.
- Percentile conventions are exact-discrete (A26); QueueWait Pxx remain
  documented approximations — never a rounding error for the request
  counts they accompany.
- F's observed collapse class is explicitly "within-horizon": the backlog's
  arithmetic drain time exceeds the 30 ms post window, so the horizon
  cannot certify eventual recovery — the class refuses to claim more than
  the window shows (A24).
- Worker/queue occupancy timelines are evidence-only and are not
  garbage-collected per phase; long-running experiments price memory O(#
  resource mutation events), bounded and deterministic.

Milestone 3 additions:

- **Terminal completeness boundary contract** — Completed runs carry an explicit,
  versioned `TerminalAttestation` covering terminal effect sequence, terminal
  simulation time boundary, and an evidence digest. Head truncation remains
  independently detectable (`FirstEffectSeq > 0`); a terminal marker cannot allow
  a head-truncated ledger to masquerade as full history. Evidence without an
  attestation remains readable and terminal-unestablished.
- **Violation preservation via cap semantics** — Properties whose unconditional
  PASS requires complete history (such as cross-path consistency) use cap
  semantics: if retained evidence demonstrates a factual contradiction, the
  verdict remains FAIL. Incomplete history caps an otherwise-clean result to
  INCONCLUSIVE rather than suppressing observable defects.
- **Independent verifier scope & category completion** — All 51 properties across
  categories A–H are evaluated by the independent verifier (`verify`), enforcing
  zero reference imports via static AST checks.
- **Workload reconstruction & seeded PRNG streams** — Arrival envelopes, burst
  volumes, and fault schedules are independently cross-checked against declared
  inputs. Deterministic stream replay from raw pseudo-random seeds is left as an
  explicit gap (`H-seeded-replay`), ensuring the verifier never fabricates
  passing verdicts without independent proof.

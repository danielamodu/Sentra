# Independent Verifier Design (Milestone 3)

The verifier is a second authority, not a second simulator. The reference system
(`sim`) produces behaviour; the verifier (`verify`) evaluates whether the produced
behaviour and its resulting books satisfy the documented contracts. It never
schedules events, never predicts behaviour and never calls reference transition,
state-mutation or invariant-validation code.

This document is the contract for that authority. Sections state what is
implemented in which phase; claims about properties are only made once the phase
that evaluates them is green.

| Area | Phase | Status |
|---|---|---|
| Package boundary + independence enforcement | P1 | implemented |
| Document, input and evidence contracts; ledger/N:1 validation | P1 | implemented |
| Canonical serialization and evidence digest | P1 | implemented |
| Verdict model and precedence | P1 | implemented |
| Evidence → world reconstruction (L-path) | P2 | implemented |
| Snapshot contract and resource/bookkeeping checks (S-path) | P2 | implemented |
| Cross-path consistency (X-path) | P2 | implemented |
| Zero-authority reference→document bridge | P2 | implemented |
| Property groups A–H complete registry (51 properties) | P3 | implemented — 51 properties, full category coverage, P3Complete = true |
| Additive terminal-boundary contract & cap semantics | P3 | implemented — versioned TerminalAttestation, anti-masquerade invariants |
| Cross-check against reported reference analytics | P4 | planned |
| M2 A–J cross-validation, defect injection, corruption, replay suites | P5 | planned |
| Final report with measured results | P6 | planned |

P2 evaluates the L-, S- and X-path property families described in §8–§10.
P3 established the single authoritative property registry (§2.1) covering all
required categories A–H (51 properties). Every property declares its evidence
sources, shortfall policy (`skip` or `cap`), counterexample window and test
cases. Categories F (event semantics), B (resource intervals), D (retry
semantics), C (queue/admission semantics), E (breaker semantics), and H
(workload and fault reconstruction) are fully implemented and verified.
`P3Complete()` evaluates to `true`.

Terminal completeness is established via an additive, versioned
`TerminalAttestation` on the evidence container. A completed run carries an
explicit terminal attestation specifying `TerminalEffectSeq`, `TerminalSimTimeNS`,
and a SHA-256 evidence digest. Head truncation remains independently detectable
(`FirstEffectSeq > 0`), ensuring a terminal attestation cannot let a
head-truncated ledger masquerade as full history. Furthermore, terminal-dependent
cross-path properties use violation-preserving `cap` semantics: observed
contradictions in retained evidence remain `FAIL`, while missing history prevents
an unverified `PASS` by capping the verdict to `INCONCLUSIVE`. Existing v1
evidence without terminal attestation remains readable and terminal-unestablished.

## 1. Independence boundary

Dependency direction is strictly downward:

```
sim       -> engine -> domain            reference implementation
evidence  -> sim, verify, domain         serializer and test harness
verify    -> domain, standard library    independent authority
```

Concretely:

| Package | May import | Must never import |
|---|---|---|
| `verify` | `sentra/domain`, standard library | `sentra/sim`, `sentra/engine`, `sentra/evidence` |
| `evidence` | `sentra/sim`, `sentra/verify`, `sentra/domain` | nothing upward from `verify` |

`evidence` may see both sides because it is a serializer and test harness; the
authority itself consumes serialized data only. This makes reference code
unreachable from `verify` at compile time, not by convention.

Enforcement: `verify/independence_test.go` parses every Go file in the package
(including tests) with `go/parser` and fails on any import outside the
whitelist. Tests are scanned deliberately: a verifier test that imported `sim`
could rebuild reference mechanics and turn agreement into an assumption.
`TestReferencePackagesAreForbidden` also pins the forbidden set so a future
whitelist edit cannot quietly re-admit a reference package.

## 2. Verification pipeline

Seven explicit stages (stage 3b is the structural sub-stage of evidence
validation), each observable in the returned `Verdict`:

| Stage | Implemented by | Failure effect |
|---|---|---|
| 1. Document schema validation | `ValidateDocument` | `INVALID_INPUT` |
| 2. Input contract validation | `ValidateInput` | `INVALID_INPUT` |
| 3. Ledger validation | `ValidateEvidence` | `INVALID_INPUT` |
| 3b. Snapshot structural validation | `validateSnapshot` | `INVALID_INPUT` |
| 4. Independent reconstruction (L-path) | `reconstruct` → `World` | uninterpretable contradictions → `INVALID_INPUT` |
| 5. Invariant evaluation (L + S) | `evaluateAll` (property registry) | per-property `PASS`/`FAIL`/`INCONCLUSIVE` |
| 6. Cross-path consistency (X) | `evaluateAll` (same registry; X entries) | per-property `PASS`/`FAIL`/`INCONCLUSIVE` |
| 7. Verdict + evidence digest | `Aggregate`, `EvidenceDigest` | aggregate status and machine-readable evidence |

Rules that hold at every stage:

- A missing or insufficient input never becomes `PASS`. Affected properties are
  `INCONCLUSIVE` with a documented gap.
- `INVALID_INPUT` means no property claim can be made at all; it is not a
  failed property. Structural evidence defects are `INVALID_INPUT`; semantic
  contract violations and cross-path disagreements are `FAIL`.
- Stage order is fixed: evidence is validated structurally before any property
  reads it, and the verdict is derived only from recorded per-property results,
  never from a short-circuit boolean.
- No stage repairs, normalizes or suppresses evidence. An unreadable record set
  stops the pipeline; it is not trimmed until it validates.
- An empty observation set is a gap, not a vacuous pass: a ledger that observed
  no request or attempt cannot establish that its lifecycle was followed.

### 2.1 Property registry (P3)

`verify/registry.go` is the single authoritative inventory of properties. It is
package-private, ordered and evaluated exactly as declared; no runtime code sorts,
filters or appends to it.

Each `propertySpec` declares:

| Field | Meaning |
|---|---|
| `ID`, `Name`, `Claim` | stable identity and the fact the property establishes |
| `Category` | approved taxonomy A–H (A lifecycle, B resources, C queueing, D retry budget, E breaker, F event semantics, G cross-path, H workload/fault reconstruction) |
| `Sources` | evidence paths the claim reads: `ledger`, `snapshot`, `input` |
| `Shortfalls` | prerequisite need plus policy: `skip` (do not run; INCONCLUSIVE) or `cap` (run, preserve FAIL, cap an otherwise-unconditional PASS to INCONCLUSIVE) |
| `Counterexample` | per-property window radius; zero selects the approved default of ±3 blocks |
| `Tests` | declared behavioral cases (`valid`, `violation`, `missing`, `partial`, `terminal`, `corrupt`) |
| `ViolationNotApplicable` | documents properties that can only PASS or INCONCLUSIVE |
| `Check` | the executable check over exactly the declared evidence |

Evidence prerequisites are three distinct needs, all evaluated centrally by the
executor (`shortfallActive`), never by a check:

| Need | Satisfied when | Means |
|---|---|---|
| `needHeadComplete` | coverage is `terminal-unestablished` or `complete` | the retained ledger begins at the documented initial boundary |
| `needTerminalComplete` | coverage is `complete` | the retained ledger is proven to reach the run's terminal boundary |
| `needFullHistory` | coverage is `complete` | both boundaries are proven |
| `needSnapshot` | a snapshot is present | final-state books are available |

A head-complete ledger never satisfies `needTerminalComplete` or
`needFullHistory` unless an explicit `TerminalAttestation` is present and valid.
Under evidence contract v1, evidence without a terminal attestation remains
readable and terminal-unestablished, with terminal-dependent properties reporting
explicit gaps. When a valid `TerminalAttestation` is present, `terminalComplete`
is proven against retained evidence (validating terminal effect sequence and
simulation time boundary). Head completeness is independently proven by
`FirstEffectSeq == 0`. Full history requires both head and terminal completeness.

The executor (`evaluateProperty`) enforces the declaration: a shortfall cannot be
ignored by a check, a check must return its own property ID, findings must cite
the property family, and gaps must be attributed to the property that reported
them. A registry contract violation is a verifier bug and produces
`INVALID_INPUT`, never a fabricated property status.

Coverage is derived from the registry, not maintained separately:

- per property, `coverage()` reports `not-implemented`, `implemented-untested`,
  `declared-not-verified` or `implemented-tested` based on the check, the
  required test kinds for its evidence policy, and whether every declared case
  names the behavioral test function and is bound to a case the suite executes;
- required test kinds are derived from the declaration: `valid`, plus
  `violation` unless inapplicable, plus `missing` when a snapshot is needed,
  plus `partial` for ledger sources, plus `terminal` when terminal or full
  history is needed;
- category counts are computed by walking the properties (A:4, B:9, C:6, D:7, E:9, F:5, G:7, H:4; total 51 properties);
- `P3Complete()` is false if any property lacks a check, lacks a required test
  kind, declares a case the test suite does not bind, or if an ID is duplicated;
- `P3Complete()` requires every required category A–H to contain at least one
  implemented-and-tested property. With all 51 properties across categories A–H
  fully implemented, verified, and bound to behavioral test cases, `P3Complete()`
  evaluates to `true`. Outside the test binary nothing is bound, so the gate
  also reports false rather than assuming unverified coverage.

`TestRegistryDeclarationsAreVerified` binds each declaration to an executed case
in `registry_behavior_test.go` in both directions. An AST existence check alone
is not treated as semantic coverage; the case must run and assert a status.

### 2.2 Counterexamples

A finding that cites a ledger block receives a bounded window of nearby blocks.
The primary block is always present, including when clipping at the retention
boundary. A reference to `engine_seq` 0 / `effect_seq` 0 is the run's first
block, not "no reference": the window is attached whenever the retained ledger
still contains that block, and no window is fabricated when the head was
evicted. Snapshot-only findings carry no ledger window. Neighbouring blocks are
labelled `context` unless the property establishes causality; no current
property does, so `cause` and `effect` are not emitted. `Truncated` reports that
the requested window was clipped.

## 3. Document contract (`sentra-verify/v1`)

```go
Document{
  Schema   string    // must equal "sentra-verify/v1"
  Scenario string
  RunID    string
  Input    *Input
  Evidence Evidence
}
```

`RunID` and `Scenario` are required: a counterexample without run identity is
not actionable. `Input` is required: without the configuration contract the
verifier cannot distinguish a legitimate bound from a violation.

### Input

`Input` mirrors documented configuration semantics only; it does not import
reference types.

| Field | Meaning | Validation |
|---|---|---|
| `HorizonNS` | simulated horizon | > 0 |
| `WorkerPool{ID,Size}` | execution pool | non-empty id, size > 0 |
| `ConnectionPools[{Upstream,Capacity}]` | per-upstream connections | non-empty, unique, capacity > 0, at least one pool |
| `Tenants[{ID,QueueLane,QueueRetry,QueueTotal}]` | admission lanes | unique non-empty ids, bounds ≥ 0, `QueueTotal` ≥ each lane |
| `RetryBudget{Capacity,RefillIntervalNS}` | per-(tenant, upstream) bucket | capacity > 0 |
| `RetryBackoffNS` | retry delay | ≥ 0 |
| `AttemptTimeoutNS` | attempt timeout | > 0 |
| `Breaker{FailureThreshold,CooldownNS,MaxHalfOpenProbes}` | breaker policy | all > 0 |
| `Workload{Seed,Components,Bursts}` | declared arrival schedule (copied data) | seed > 0; component/burst tenant and upstream must exist; non-negative rate; positive window; jitter in [0,1]; deadline range ordered; activation window inside the horizon; positive burst count |
| `Faults[]FaultEpisode` | declared fault schedule (copied data) | upstream must exist; known kind; kind-specific parameters; episode inside the horizon; no two episodes overlap on the same upstream |

`Workload` and `Faults` are **declared inputs, not verified behaviour**. Their
validation is declaration-shape checking only: the verifier does not replay the
seed, re-derive arrivals, apply bursts, or confirm that a fault episode was
active or that its effects were observed. No property reads either field: the
reserved category H is where workload/fault reconstruction belongs, and until
those properties exist a `PASS` says nothing about workload or fault semantics.
The consequence for empty runs is documented in §13: the verifier cannot
distinguish "the run was empty" from "the ledger observed nothing".

### Evidence

```go
Evidence{SchemaVersion, FirstEffectSeq, Records []Record, Snapshot *Snapshot}
Record{SchemaVersion, EffectSeq, EngineSeq, SimTimeNS, Category, Kind, Subject, Detail}
Detail{Request, Attempt, Tenant, Upstream, Worker, From, To, Reason, Lane, Description}
```

`Lane` was added by P2 (D1) to carry the admission lane on the typed
`attempt-created` note. The description text is unchanged; only the payload
gained a typed field.

`Snapshot` is the independent final-state observation. It is optional in the
wire format only because a producer may be unable to observe final books; its
absence makes every S- and X-path property `INCONCLUSIVE` with a gap (§9). It is
never an empty placeholder that could read as "nothing to check".

#### Snapshot structure

```go
Snapshot{
  SchemaVersion, TakenAtNS,
  Requests []RequestFact, Attempts []AttemptFact,
  WorkerPools []WorkerPoolFact, Connections []ConnectionPoolFact,
  Queues []QueueFact, Pending []PendingFact,
  TenantActive []TenantActiveFact, Outcomes []OutcomeFact,
  Budgets []BudgetFact, TokensConsumed []TokenConsumedFact,
  Breakers []BreakerFact,
}
```

Snapshot facts are verbatim copies of final books: identity, lifecycle state,
timestamps, retry counters, attempt parentage, worker/connection ownership,
FIFO queue order with global sequence numbers, pending admissions per lane,
tenant active counts, outcome counters, budget tokens and breaker counters.
Ordering that is itself evidence (queue entries, waitlist order, idle list,
request attempt order) is preserved exactly by the bridge.

Structural validation (stage 3b) rejects, as `INVALID_INPUT`:

| Code | Meaning |
|---|---|
| `snapshot.schema_version` | unknown snapshot schema version |
| `snapshot.horizon_mismatch` | `TakenAtNS` is not the declared input horizon |
| `snapshot.empty_id` | a fact has no identity key |
| `snapshot.duplicate_id` | two facts share an identity |
| `snapshot.unknown_tenant` | a fact names a tenant the input does not configure |
| `snapshot.unknown_upstream` | a fact names an upstream the input does not configure |
| `snapshot.unknown_state` | request/attempt/breaker state outside the documented vocabulary |
| `snapshot.unknown_outcome` | attempt outcome outside the documented vocabulary |
| `snapshot.invalid_time` | negative time or deadline before arrival |
| `snapshot.invalid_retries` | negative retries or `RetriesUsed > MaxRetries` |
| `snapshot.invalid_capacity` | non-positive pool size |
| `snapshot.invalid_pending` | pending admission lane is neither `arrival` nor `retry` |
| `snapshot.dangling_reference` | a request/attempt/pending fact names an unknown entity |
| `snapshot.duplicate_reference` | a request lists the same attempt twice |
| `snapshot.reference_mismatch` | parent/child fields disagree (tenant, upstream, attempt list, pending lane/state) |
| `snapshot.pool_missing` | a configured worker or connection pool is absent |
| `snapshot.queue_missing` | a configured tenant queue is absent |
| `snapshot.breaker_missing` | a configured upstream breaker is absent |
| `snapshot.config_mismatch` | pool size, connection capacity, budget capacity or refill interval disagrees with the input |

Structural findings are evidence defects, never property failures: a snapshot
that does not describe the declared run cannot support any claim about it.

### Validation codes

Codes are stable identifiers, machine-readable, and documented here because
verdicts and counterexamples cite them.

| Code | Meaning |
|---|---|
| `document.schema` | unknown document schema version |
| `document.decode` | document is not valid JSON |
| `input.missing` | input contract absent |
| `input.identity` | scenario or run identity empty |
| `input.horizon` | horizon not positive |
| `input.worker_pool` | pool id/size invalid |
| `input.upstream` | connection pool declarations invalid |
| `input.tenant` | tenant lane declarations invalid |
| `input.retry_budget` | budget or backoff invalid |
| `input.timeout` | attempt timeout invalid |
| `input.breaker` | breaker policy invalid |
| `evidence.schema_version` | evidence schema version unknown |
| `evidence.empty` | no records; nothing can be verified |
| `record.schema_version` | record schema version mismatch |
| `evidence.effect_seq_mismatch` | gap, duplicate or reorder in `effect_seq` |
| `record.category` | category is neither `event` nor `note` |
| `record.event_kind` | event kind outside the documented taxonomy |
| `record.note_kind` | note kind outside the documented taxonomy |
| `evidence.time_negative` | negative simulated time |
| `evidence.time_order` | simulated time regresses between records |
| `evidence.block_time_mismatch` | a record's time disagrees with its engine step |
| `evidence.block_missing_event` | a retained step has notes but no event |
| `evidence.block_multiple_events` | a step carries more than one event |
| `evidence.block_event_not_first` | effect recorded before its trigger |
| `evidence.engine_seq_interleaved` | records of one step are not contiguous |

Property and finding codes are namespaced by path and listed with their
contracts in §8 (L), §9 (S) and §10 (X).

## 4. Canonical serialization and evidence digest (D2)

`CanonicalJSON` defines canonical form:

- UTF-8 JSON with no insignificant whitespace and no trailing newline;
- struct fields in declaration order (the evidence model contains no maps, so
  serialization is total and independent of iteration order);
- HTML escaping disabled; standard Go string escaping otherwise;
- empty optional fields omitted exactly as their `json` tags declare.

`EvidenceDigest` = `sha256:` + hex(SHA-256(canonical JSON of `Evidence`)).

Fields included in the digest, exhaustively:

- `schema_version`, `first_effect_seq`;
- per record: `schema_version`, `effect_seq`, `engine_seq`, `sim_time_ns`,
  `category`, `kind`, `subject`, `detail.request`, `detail.attempt`,
  `detail.tenant`, `detail.upstream`, `detail.worker`, `detail.from`,
  `detail.to`, `detail.reason`, `detail.lane`, `detail.description`;
- when present, `snapshot`: `schema_version`, `taken_at_ns`, every request fact,
  attempt fact, worker pool (busy bindings and idle order), connection pool
  (held set and waitlist order), queue entry (attempt, enqueue time, global
  sequence), pending admission, tenant active counter, outcome counter, retry
  budget, consumed-token row and breaker fact.

Excluded by design: the document schema string, scenario/run identity, and the
input contract including the declared workload and fault schedules. Those are
declarations to check against observations, not observations to fingerprint.

A missing snapshot is omitted (`omitempty`) and therefore does not change the
digest of P1 evidence: nil-snapshot digests are stable across the P2 change.
When a snapshot is present, the digest covers it, so two verdicts citing the same
digest were computed over byte-identical books.

An `INVALID_INPUT` verdict carries no digest: evidence that failed structural
validation has no stable identity worth citing.

## 5. Ledger validation and the N:1 correlation contract

The engine-to-effect mapping is **N:1**: one processed event (one `engine_seq`)
produces one event record plus zero or more effect notes, and all of them share
that `engine_seq` and the event's simulated time. Validation therefore uses
contiguous blocks, never 1:1 assumptions:

1. `effect_seq` starts at `FirstEffectSeq`, is gapless and strictly increasing.
2. All records of one `engine_seq` are contiguous. A `engine_seq` that
   reappears later is interleaved corruption.
3. Each retained block carries exactly one event record, and it is the first
   record of the block: the reference records the trigger before applying any
   effect.
4. A head block may lack its event record only when `FirstEffectSeq > 0`; that
   is ring truncation, marked `Partial`, not a defect.
5. All records in a block share one simulated time; simulated time is
   non-decreasing across blocks. Negative time is invalid.
6. `category` is `event` or `note`; kinds are drawn from the closed documented
   taxonomy (20 event kinds, 18 note kinds). Unknown kinds are unverifiable and
   rejected rather than ignored.
7. `effect_seq` is validated numerically, including overflow at the top of the
   sequence space.

`engine_seq` is deliberately **not** required to increase across blocks: the
engine orders by `(time, priority class, scheduling seq)`, so a lower-priority
event scheduled earlier can be processed after a higher-priority event
scheduled later at the same instant. Requiring monotone `engine_seq` would
reject valid evidence; the block containment rules are the correct invariant.

`effect_seq` ordering plus block contiguity yields the properties later stages
need: causal order within a step, step order across the run, and exact effect
multiplicity.

## 6. Verdict semantics

| Status | Meaning |
|---|---|
| `PASS` | every evaluated property held on sufficient evidence |
| `FAIL` | at least one property violated on consistent evidence |
| `INCONCLUSIVE` | no violation found, but a required property could not be established |
| `INVALID_INPUT` | document, input or evidence unusable; no property claim possible |

Aggregation precedence: `INVALID_INPUT > FAIL > INCONCLUSIVE > PASS`. An empty
property set aggregates to `INCONCLUSIVE`.

`Verdict` reports the aggregate status, per-property results, findings, gaps and
the evidence digest. Every finding already carries run identity via the verdict,
the property result it belongs to, an entity reference where one exists, the
expected/observed relationship and the evidence reference (ledger
`effect_seq`/`engine_seq` and/or `snapshot_ref`). Findings that cite a ledger
block also carry the bounded counterexample window: the primary block plus up to
three blocks before and after it, with neighbours labelled `context` because no
current property establishes causality (§2.2).

## 7. Evidence gaps and limitation policy

- **History completeness is three-valued.** `HistoryHeadTruncated` means the
  retained window starts after `effect_seq` 0, so earlier records are
  unrecoverable. `HistoryTerminalUnestablished` means the head is complete but
  the evidence carries no marker proving the retained tail reaches the run's
  terminal boundary. `HistoryComplete` requires both proofs and is unreachable
  under evidence schema v1, because contract v1 has no terminal-boundary marker.
  Completeness is never inferred from `FirstEffectSeq == 1` alone or from record
  contiguity.
- **Truncation (D5).** A retained window that starts after `effect_seq` 0 is
  `Partial`. Properties that need pre-window history are `INCONCLUSIVE`, never
  assumed. Properties that require head completeness but can still preserve an
  established violation (`L-history-completeness`, `X-entity-sets`) run under a
  `cap`: a PASS is capped to `INCONCLUSIVE` on a truncated ledger. Totals that
  can straddle the eviction boundary (`L-retry-budget-ledger`,
  `X-budget-consumption`, `X-probe-ledger`, `X-tenant-active`) are skipped
  entirely on a truncated ledger.
- **The terminal boundary is a separate guarantee.** Head completeness says
  nothing about the tail. Any property whose claim depends on the absence of a
  later record declares `needTerminalComplete` (or `needFullHistory` for both
  boundaries). Because contract v1 carries no terminal-boundary marker,
  `HistoryComplete` is unreachable and those properties are `INCONCLUSIVE` with
  an explicit gap. A terminal boundary is never fabricated from the last
  observed record, contiguous effect sequences or the snapshot timestamp. This
  currently affects seven X properties: `X-budget-consumption`,
  `X-breaker-final-state`, `X-probe-ledger`, `X-entity-sets`,
  `X-request-final-state`, `X-attempt-final-state` and `X-tenant-active`.
  `X-attempt-parentage` and the L/S families do not require the tail.
- **Horizon truncation (D5).** Occupancy that is still open at the horizon is
  legitimate. It is distinguished from a leak; "eventual release" claims that
  the evidence horizon cannot cover are `INCONCLUSIVE`.
- **Prose is not evidence.** Typed identity is carried by `request-transition`,
  `attempt-transition`, `attempt-created` (D1), `cancellation` and the
  `breaker` event payload. `worker`, `connection`, `budget`, `probe-reserved`
  and `probe-released` notes remain prose-only, so probe reservations cannot be
  attributed to an upstream and are cross-checked in aggregate (§10).
  Resource identity is established from the snapshot (D3), never by string
  parsing.
- **Independence, not agreement.** Two implementations agreeing is weak
  evidence. The verifier establishes properties from contracts; agreement with
  reported analytics is cross-checked separately (P4), and an unexplained
  disagreement blocks milestone acceptance pending investigation (D6) without
  being attributed to either implementation.

## 8. L-path: independent reconstruction

`reconstruct` turns validated ledger blocks into a verifier-side `World`:
request, attempt and breaker lifecycles; typed identities; parentage; and the
ledger-level retry/probe counts. It is a pure function of the evidence, ordered
by evidence order, with first-observation order preserved.

The lifecycles are **re-derived**, not delegated:

- `verify/lifecycle.go` declares the closed state sets and transition matrices
  from `docs/SYSTEM_MODEL.md`.
- It never calls `domain.CanTransitionRequest`, `domain.CanTransitionAttempt` or
  any reference validator. Calling them would make the verifier accept whatever
  the reference believes, including a future reference defect.

| Property ID | Contract |
|---|---|
| `L-request-lifecycle` | every observed request transition is documented and contiguous |
| `L-attempt-lifecycle` | every observed attempt transition is documented and contiguous |
| `L-breaker-lifecycle` | breaker transitions are documented and contiguous |
| `L-history-completeness` | each reconstructed history begins at its documented initial state, on evidence proving no earlier transition was evicted |
| `L-attempt-linkage` | each attempt is attributable to exactly one logical request |
| `L-retry-budget-ledger` | on a head-complete ledger, one budget effect per scheduled retry |

Classification rules:

| Situation | Result |
|---|---|
| undocumented transition (`admitted -> completed`) | `FAIL`, `reference-defect-candidate` |
| legal transition that does not start at the reconstructed state | `FAIL`, `unclassified`: the ledger and the reconstruction disagree; the evidence does not say which is wrong |
| history starts mid-lifecycle (truncation or missing terms) | gap on `L-history-completeness`, never `FAIL` |
| truncated window begins at `admitted`/`created` but the typed creation evidence was evicted | gap on `L-history-completeness`: state alone is not proof of the head |
| unknown state name, missing identity, contradictory typed identity, duplicate creation | `INVALID_INPUT`, `evidence-defect` |
| no request/attempt observed at all | gap on the affected property, never vacuous `PASS` |

Attempt parentage: the D1 `attempt-created` note carries typed
request/attempt/tenant/upstream/lane fields. Pre-D1 evidence (prose-only
creation note) is reconstructed from the first `attempt-transition` and the
block's single typed request reference, exactly as before. An attempt first seen
in a block that names zero or several requests stays unlinked; that is a gap on
`L-attempt-linkage`, never a guess. A creation note that carries typed facts but
no attempt identity is uninterpretable.

History completeness uses typed creation evidence, not the first observed state:
a request transition beginning at `admitted` is complete on a head-complete
ledger, and on a truncated ledger only when a retained `AdmissionDecided` event
establishes its admission decision. An attempt is complete when its typed
creation effect is retained (or the head is complete and it begins at `created`).
Breaker completeness needs a head-complete ledger, because an earlier open/close
cycle cannot be excluded from a truncated window. `L-history-completeness`
claims head completeness only; it does not claim the tail, and a `PASS` therefore
says the beginning of each retained history is established, not that the run is
fully observed (§13).

The retry-budget ledger equality is asserted only on a head-complete ledger:
ring truncation can evict a `RetryScheduled` event and its `budget` note on
opposite sides of the window boundary. On a truncated ledger the property is
`INCONCLUSIVE`, not `FAIL`.

## 9. S-path: snapshot checks

The registry's S-path entries evaluate the final books against the documented
resource contracts. Every check reads the snapshot and the declared input only;
none executes a transition. `snapshotChecks` remains as a focused projection of
the same registry entries for unit tests; it is not a second inventory.

| Property ID | Contract |
|---|---|
| `S-worker-conservation` | `busy + idle == size`, each worker exactly one of busy/idle |
| `S-worker-ownership` | pool bindings and attempt `has_worker`/`worker` agree in both directions; only worker-holding states hold workers |
| `S-connection-ownership` | pool holders and `conn_held` agree; held ≤ capacity; holding implies `running` |
| `S-waitlist-membership` | waitlists contain exactly the `waiting-connection` attempts, once each, on the right upstream, holding no connection |
| `S-connection-stranding` | no free connection coexists with a waiting attempt |
| `S-queue-consistency` | queues reference exactly the `queued` attempts of their tenant, no duplicates, strictly increasing global sequence |
| `S-lane-bounds` | arrival/retry lane usage and their sum, **including pending admissions**, stay within the declared bounds |
| `S-tenant-active` | counters equal the non-terminal request count per tenant |
| `S-outcome-ledger` | terminal attempts have determined outcomes, non-terminal attempts do not, and per-tenant counters equal the attempt books |
| `S-budget-state` | `0 ≤ tokens ≤ capacity`, refills inside the run, consumed rows name real and configured buckets |
| `S-breaker-state` | non-negative failure counters, probe allowance respected, no probes outside half-open |
| `S-probe-ledger` | per upstream, permit-holding attempts equal `ProbesActive`, and no terminal attempt holds a permit |
| `S-horizon-completeness` | facts lie inside `[0, TakenAtNS]`; terminal stamps exist exactly for terminal states |

Semantic violations are `FAIL` with classification `unclassified`: the books
are internally readable, so the snapshot itself (or the run that produced it) is
wrong, but the evidence does not establish which. Finding codes are the property
ID plus the diagnostic sub-codes `S-worker-duplicate`, `S-connection-capacity`,
`S-queue-sequence` and `S-terminal-resource-retention`.

If `Snapshot` is absent, all thirteen properties are `INCONCLUSIVE` with the
same concrete gap. None is skipped and none is treated as satisfied.

## 10. X-path: cross-path consistency

The registry's X-path entries compare a fact only where both paths establish it.
This is the deliberate asymmetry that keeps agreement meaningful.
`crossPathChecks` remains as a focused projection for unit tests.

| Property ID | Compared fact | Evidence requirement |
|---|---|---|
| `X-entity-sets` | every ledger entity exists in the snapshot; every snapshot entity exists in the ledger | head for the forward direction, terminal for the reverse |
| `X-request-final-state` | final request state and terminal instant | terminal |
| `X-attempt-final-state` | final attempt state and finish instant | terminal |
| `X-attempt-parentage` | attempt → request attribution | neither |
| `X-breaker-final-state` | final breaker state (a terminal-complete ledger with no transition pins `closed`) | terminal |
| `X-budget-consumption` | ledger budget effects versus snapshot consumed-token total | terminal |
| `X-probe-ledger` | ledger reserved−released versus snapshot active probe sum | terminal |
| `X-tenant-active` | non-terminal ledger requests per tenant versus snapshot counters | full history (head and terminal) |

Head completeness and terminal completeness are distinct. Contract v1 cannot
prove the terminal boundary, so the seven terminal-dependent claims above are
`INCONCLUSIVE` with an explicit gap even on a head-complete ledger; a final state
or a total is never inferred from the last observed record. `X-entity-sets` keeps
its forward direction: a ledger entity missing from the snapshot is a violation
independent of the tail, so a `cap` preserves that `FAIL` while capping an
otherwise-unconditional `PASS` to `INCONCLUSIVE`. `X-attempt-parentage` records a
parent link rather than inferring one from absence, so it remains evaluable. A
snapshot-only entity in a head-truncated ledger is an eviction, not a
disagreement; population equality is therefore capped rather than claimed.

A disagreement produces a finding carrying:

- the property code (the finding code is the property ID);
- the entity ID(s) that disagree;
- the ledger reference (`effect_seq`, `engine_seq`) where the ledger established
  the fact;
- the snapshot reference (`request:<id>`, `attempt:<id>`, `breaker:<id>`,
  `snapshot.breakers`, …);
- `Expected` and `Observed`.

Disagreements are always classified `unclassified`. The evidence admits "at
least one of the two books is wrong"; it does not establish which one, and the
verifier never attributes a disagreement to the reference implementation
automatically (D6). `reference-defect-candidate` appears only where an
independent contract is violated on a single path (an illegal transition), not
where two paths disagree.

Deliberate limitations of this path:

- Facts are not compared where only one path establishes them. A ledger entity
  missing from the snapshot is a disagreement; a snapshot entity missing from a
  **truncated** ledger is an eviction and is not blamed on either path.
- Probe reservations and releases are typed only by attempt ID in prose notes,
  so their balance is compared in aggregate; the verifier does not invent an
  upstream attribution.
- Final-state agreement is a point-in-time comparison at the horizon, and only
  where the retained ledger is proven to reach that horizon. Contract v1 cannot
  prove it, so final-state and total comparisons are `INCONCLUSIVE` today. The
  property does not prove that the paths agreed at earlier instants; the L-path
  covers earlier lifecycle steps and the S-path covers the final books.

## 11. The reference→document bridge

`evidence.FromRun(sim.RunResult) → verify.Document` (and the
`FromExperiment` convenience) is a serializer with zero authority:

- no verification logic, no property evaluation, no sufficiency decision;
- no repair, normalization, suppression or dropping of invalid references;
  every value is copied verbatim and validation belongs to `verify`;
- the three kinds of input stay distinct: **observations** (records and
  snapshot), **declared inputs** (the configuration contract) and, later,
  **reported claims** (P4). They are never merged or reconciled;
- conversion is total for well-formed results. The one reported failure is a
  result whose sink retained no records although the engine processed events:
  an absent ledger must not serialize as an empty (and therefore differently
  meaningful) ledger;
- determinism: map-derived slices are sorted by identity. Slices whose order is
  itself evidence (queue entries, waitlist, idle list, request attempt order)
  are preserved exactly. Two conversions of one result are byte-identical.

Identity mapping: `RunID = Scenario = exp.Name`; `TakenAtNS = exp.Horizon`;
`Evidence.SchemaVersion = sim.SchemaVersion`; record `Detail` is copied
field-for-field including `Lane`.

## 12. Coverage and tests

P3 is covered by:

| Test file | Coverage |
|---|---|
| `verify/registry_test.go` | ordered 51-property inventory, category counts (A:4, B:9, C:6, D:7, E:9, F:5, G:7, H:4), four-state coverage and declaration binding, category-aware `P3Complete` gate (all A–H present and tested, evaluates to true), shortfall skip/cap enforcement, head-complete/terminal-unestablished adversarial cases, counterexample window roles and clipping (including the first block), deterministic aggregation, tri-state history coverage |
| `verify/registry_behavior_test.go` | executed behavioral cases for all 51 properties covering every declared case kind (`valid`, `violation`, `missing`, `partial`, `terminal`, `corrupt`) over real fixtures; `terminal` cases assert INCONCLUSIVE on terminal-unestablished evidence, and violations are preserved under cap semantics |
| `verify/registry_vectors_test.go` | aggregate golden status vectors for reference fixtures and pipeline-level counterexample attachment |
| `verify/fixture_freeze_test.go` | per-property status and gap vectors compared against pre-migration vectors, verifying that all pre-migration properties are preserved and new P3 properties expand the inventory cleanly |
| `verify/world_test.go` | reconstruction identity/state/timestamps, illegal transitions, continuity breaks, truncation, missing history, duplicate creation, identity conflict, legacy pre-D1 creation, determinism, retry-budget ledger |
| `verify/snapshot_test.go` | structural corruption matrix, semantic corruption matrix, lane accounting with pending admissions, absent-snapshot gaps, invalid-input classification, determinism, finding structure |
| `verify/crosscheck_test.go` | agreement on terminal-unestablished evidence, terminal-complete agreement with cap semantics preserving observable violations, disagreement structure and blame-freedom, both entity-set directions, truncation tolerance, insufficient evidence, FAIL-vs-INCONCLUSIVE-vs-INVALID_INPUT, determinism |
| `verify/independence_test.go` | import whitelist across all verifier Go files including tests (zero reference imports) |
| `evidence/convert_test.go` | bridge preservation, determinism, round trip, absent ledger, missing snapshot, zero result, terminal attestation generation and digest verification, A–J per-property status across all 51 properties (50 PASS on terminal-complete evidence, with H-seeded-replay reporting an explicit gap) |

The A–J bridge test runs every recorded M2 scenario through the real reference
run, serializes it with explicit terminal attestation, and verifies the document
independently. It reuses scenario inputs and final books, never reference
transition, validation or metric code. Across all 10 scenarios, 50 properties
consistently pass and `H-seeded-replay` reports an explicit gap.

## 13. Remaining limitations and gaps (honest status)

1. **Independent seeded PRNG stream reconstruction (`H-seeded-replay`).**
   The verifier does not duplicate the reference PRNG implementation or its
   stochastic stream generation to reconstruct individual request arrival sequences
   from raw seeds. It reports an explicit gap (`ViolationNotApplicable: true`),
   preserving verifier independence rather than faking PRNG state machines or
   passing vacuously.
2. **Reported reference analytics are not serialized (P4).** M2 summary, phase,
   amplification and recovery values are not cross-checked. A `PASS` today says
   nothing about those numbers.
3. **Probe permitting is aggregate on the X-path.** Reserve/release notes are
   prose-only, so an imbalance is reported without upstream attribution.
4. **Point-in-time cross-check.** X-path agreement is at the horizon, not
   proven continuously.
5. **Counterexample windows are bounded and causal-free.** Findings citing a
   ledger block carry the primary block plus up to three context blocks on each
   side (§2.2). Neighbours are labelled `context`; properties do not establish
   arbitrary cross-block causality graphs beyond declared parent-attempt links.
6. **Snapshot completeness assumption.** Reference maps never delete entries, so
   final-state books are complete at the horizon; ring truncation affects only
   the ledger. This is a documented property of the reference at this milestone,
   not a general assumption about arbitrary evidence.
7. **Fixture-freeze baseline is partial.** The captured pre-migration corpus
   contains four reference fixtures with per-property status vectors. Post-migration
   expansions cover all 51 properties, but historical baseline records exist only
   for the initial subset.
8. **`effect_seq` 0 references are omitted from serialized findings** by the P1
   `omitempty` contract. The `engine_seq` and entity/message fields still
   identify the establishing step.

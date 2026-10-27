# Sentra Architecture (Milestones 1 & 2)

## 1. Package structure and dependency rules

```
domain/     ← pure types; depends on nothing
engine/     ← deterministic event core; depends only on domain (time types)
sim/        ← reference system: state, events, transitions, recorder;
              depends on domain + engine
verify/     ← independent verification authority (M3);
              depends on domain + standard library only
evidence/   ← serializer + cross-validation harness (M3);
              depends on sim + verify + domain
```

Dependency direction is strictly downward: `sim → engine → domain`, and
`domain → (nothing)`. No package imports `net/http`, `os`, `time`, crypto,
or any infrastructure library. In particular, nothing in the codebase reads
the wall clock — `SimTime`/`SimDuration` are the only time types in use.

## 2. The three architectural roles

### domain — contracts

Pure, side-effect-free types and small invariant helpers: the liveness of
`CanTransitionRequest` / `CanTransitionAttempt`, the FIFO pool/queue/waitlist
mechanics, and the integer token-bucket arithmetic. This package is the
stable contract surface.

### engine — deterministic ordering

A discrete-event core that knows nothing about Sentra beyond simulated time:
events are `(time point, caller-defined priority class)`; the engine orders
them by `(simulated time, priority, scheduling sequence)`, executes them one
at a time against a `Handler`, and panics on handler-scheduled events in the
past. It provides `Step` (single-event execution) and `Run(until)` for
testable scheduling. No goroutines, channels, timers or randomness.

### sim — the M1 reference system

All **transition logic** lives in `sim/handlers.go`, implemented as an
exhaustive type switch over the sim event taxonomy. `sim/state.go` owns the
entire mutable system state, `sim/recorder.go` the structured log from which
a run's state trajectory can be reconstructed.

## 3. The reference system vs the independent simulator (contract)

Sentra's eventual verification strategy requires *independent* validation:
the future discrete-event simulator must not simply replay the same
transition implementation as the system it validates. M1 sets the
architectural contract for that:

1. **Shared contracts** — `domain` (types, lifecycle tables, resource
   mechanics) and `engine` (ordering core) are the allowed shared surface.
2. **Scenario definitions** — configs (`SystemConfig`) and event scripts
   (the injectable `New*Event` constructors) are scenario-level contracts
   the independent simulator may reuse.
3. **Independent transitions** — `sim/handlers.go` is *excluded* from the
   shared surface. The future simulator will re-derive transitions from the
   same documented semantics (`docs/SYSTEM_MODEL.md`,
   `docs/ENGINE_SEMANTICS.md`) with its own implementation, so that identical
   inputs going through both systems can be compared event-for-event as an
   experimental cross-check.

### Independence matrix (per contract artifact)

Shared domain types are graded by how decision-bearing their code is. The
grading uses INDEPENDENCE RISK markers placed in the type docs themselves:

- **Tier 1 — safe to share outright.** Pure data carriers and identity
  types: `SimTime`/`SimDuration`, identity IDs, `QueueEntry`, record types.
  No decisions encoded; an independent implementation should reuse or
  mirror them verbatim.
- **Tier 2 — structural mechanics, share only the mechanism.**
  `WorkerPool`/`Queue`/`ConnectionPool` FIFO arithmetic and the lifecycle
  *tables* (`requestTransitions`, `attemptTransitions`). The ordering
  *rules* (`CanTransition*`) are exempt from independence by design: both
  systems must agree on the transition matrix, otherwise the comparison
  would flag structural disagreement as behavioural disagreement.
- **Tier 3 — decision-bearing; an independent validator must NOT delegate
  to these implementations.** Marked in-source. A tier-3 reimplementation
  re-derives the decision from the type's fields and its own algorithm:

  | Artifact | Tier-3 decision it encodes | Marker |
  |---|---|---|
  | `CircuitBreaker` transition application | failure threshold / cooldown / probe policy = the resilience policy itself | `domain/resilience.go` |
  | `RetryBudget.TryConsume` | the retry-amplification bound | `domain/resilience.go` |
  | `Upstream.ExecutionLatency` | the failure model selection (normal vs degraded latency) | `domain/upstream.go` |
  | `sim/handlers.go` (whole file) | every transition order and admission/retry gate | excluded from the shared surface entirely |

Concrete implication for later milestones: any behaviour change must land in
the documented semantics and in `domain` contracts *before* it lands in
`sim` handlers, so an independent implementation remains possible.

## 4. Determinism guarantees (end to end)

- IDs: assigned by monotonic counters during handler execution; identical
  event sequences yield identical IDs (`req-1`, `att-1`, …).
- Ordering: total order on `(simTime, priority, seq)` — see
  `docs/ENGINE_SEMANTICS.md`.
- Resource choice: FIFO idle lists, FIFO waitlists, global queue sequence
  numbers, oldest-inserted-first dispatch candidates.
- No randomness, no map-iteration-order-dependent decisions (scans that need
  an order sort by Seq or select by min), no global mutable state.
- Two runs from identical inputs are bit-identical: same recorder trace, same
  final state (`TestReferenceSystemRepeatabilityFromIdenticalInputs`).

## 5. Injection and extensibility points

- External events enter only as initial engine inputs or via
  `engine.Schedule` (guarded to never schedule in the past); the transition
  surface itself remains closed.
- One worker pool and a `Pools map` exist; multi-pool scheduling is a
  documented unresolved decision for later milestones, not a speculative
  abstraction implemented now.
- The deterministic failure model (degradation ⇒ slow + failed at completion)
  lives behind `Upstream.ExecutionLatency()` and the completion handler; a
  seeded stochastic model is a later replacement that must preserve
  determinism.

## 6. What is intentionally absent

- No AI agent, GitLab integration, optimization engine, governance UI.
- No metastability detection or wasted-work remediation logic — only the
  accounting surface (`OutcomeCounters`, orphan flags) such logic will need.
- No persistence, HTTP transport, concurrency, or deployment artifacts.

Nothing in this milestone silently manufactures those capabilities.

## 7. Milestone 2: the experimental harness (new sim files)

M2 extends the same architecture — no new package, no new dependency edge.
New files inside `sim/`, each with one responsibility:

| File | Responsibility |
|---|---|
| `prng.go` | splitmix64 keyed `(seed, stream)`; the single deterministic randomness source (no `math/rand`, no wall clock anywhere) |
| `workload.go` | `WorkloadSpec`/`ArrivalComponent`/`Burst`: recorded, replayable workload generation (`Build(horizon)`) |
| `faults.go` | `FaultSchedule`: recorded fault-injection input; `Events(horizon)` → typed Model-control events; `Validate` |
| `mustjson.go` | JSON helpers for the recorded-input evidence strings |
| `occupancy.go` | evidence-only era-stamped timelines (workers/conns/queues), appended at every resource mutation site |
| `system_metrics.go` | the metrics model: summary/phase reports, exact occupancy integrals, amplification/waste ratios, latency percentiles |
| `recovery.go` | post-hoc recovery analysis: pre/during/immediate/drain/post decomposition, drain scan, classification |
| `scenarios.go` | `Experiment` (recorded data: workload+faults+phases+recovery anchor) and `RunExperiment`/`ReplayCheck` |
| `scenario_library.go` | scenarios A–J with their calibrated constants |

### Experiment machine contract

- An `Experiment` is plain data (JSON-serializable): config, workload spec,
  fault schedule, phase windows, recovery anchor, horizon. `RunExperiment`
  is the only place engine runs are created.
- One engine run through all boundaries; full-state `Validate()` at each;
  panic (not silent degradation) on violation; all observations computed
  post-hoc from final state + ledger.
- `ReplayCheck`: two fresh runs must agree bit-for-bit on every derived
  artifact (see `docs/ENGINE_SEMANTICS.md` §9).
- No experiment reads or mutates system state outside the engine; no
  scenario can "guarantee" its result by resetting or advancing state — the
  library only calibrates *inputs* (rates, fault windows) to probe regimes.

### Determinism extension

M2 introduces the only randomness in the codebase, and it is fully
deterministic: splitmix64 keyed by `(seed, stream)`. Distinct consumers use
distinct streams (workload jitter, deadline sampling), so pulling a sample
for one purpose never shifts another's sequence. Two runs of identical
specs (and hence identical schedules) produce identical event streams,
ledger traces and final states — asserted by `ReplayCheck` across the whole
scenario library.

### M1.1 preservation

The remediation layer (must-JSON, overflow guards, ledger exactness) is
untouched in M2; the new event kinds extend the taxonomy without touching
ordering classes or the same-instant precedence rules, so M1.1's
guarantees hold verbatim in experiment runs (invariants are re-validated
at every experiment boundary).

## 8. Milestone 3: the independent verifier (new packages)

M3 adds two packages. P1 introduced the contracts; P2 adds reconstruction and
checks, plus one additive reference change (D1).

| Package | Responsibility | May import |
|---|---|---|
| `verify/` | the verification authority: document/input/evidence contracts, snapshot contract, independent reconstruction, L/S/X property evaluation, verdicts | `sentra/domain`, standard library |
| `evidence/` | serializer from reference runs to verifier documents, plus cross-validation and defect-injection harnesses; contains no verification logic | `sentra/sim`, `sentra/verify`, `sentra/domain` |

P2 files inside `verify/`:

| File | Responsibility |
|---|---|
| `lifecycle.go` | the verifier's own state sets and transition matrices (never `domain.CanTransition*`) |
| `world.go` | L-path reconstruction from typed ledger records; lifecycle, linkage and retry-budget properties |
| `snapshot.go` | snapshot types, structural validation and the S-path resource/bookkeeping checks |
| `crosscheck.go` | X-path comparisons; structured, blame-free disagreements |
| `verify.go` | explicit pipeline wiring (stages 1–7) and verdict aggregation |

P2 files inside `evidence/`:

| File | Responsibility |
|---|---|
| `convert.go` | `FromRun`/`FromExperiment`: verbatim conversion of records, input contract and final books; no repair and no verification |
| `sort.go` | deterministic materialization of map-derived slices; order-bearing slices are left untouched |

### The one additive reference change (D1)

The `attempt-created` note now carries typed `Request`, `Attempt`, `Tenant`,
`Upstream` and `Lane` fields (`sim/recorder.go`, `sim/handlers.go`). The rendered
description is byte-identical to the previous `Logf` form and no decision,
ordering or state changes. This is evidence plumbing, not behaviour: it lets the
verifier attribute an attempt to its request without parsing prose. Pre-D1
evidence remains readable through the block-local fallback (§8 of
`docs/VERIFIER_DESIGN.md`).

### Separation of authority

The reference system produces behaviour; the verifier evaluates it. The
verifier has no handle on a `Sim`, `SystemState`, engine or recorder: it consumes
a serialized document. Reference transition handlers, state mutation, lifecycle
code, queue dispatch, retries, breaker transitions, resource accounting and
`SystemState.Validate` are all unavailable to it by construction, not by
convention.

`verify` is an authority; `evidence` is a serializer. `evidence` may import both
sides, which is precisely why the authority must not import it: the dependency
edge `evidence → verify` can never become `verify → evidence`.

### Enforcement

`verify/independence_test.go` parses every file in the package (tests included)
and rejects any import outside `{sentra/domain, standard library}`. The
forbidden set is also pinned explicitly (`sim`, `engine`, `evidence`). Boundary
violations fail the suite, so an accidental reference import cannot pass review.

### Evidence contract

The verifier's inputs are the recorded input contract, the reference ledger
(`effect_seq`/`engine_seq` correlation, N:1) and a snapshot of final books. Prose
note descriptions are never parsed. The document schema is versioned
(`sentra-verify/v1`), canonically serialized, and fingerprinted with a SHA-256
evidence digest that covers the ledger and, when present, the snapshot; see
`docs/VERIFIER_DESIGN.md` and `docs/INDEPENDENCE_MATRIX.md`.

### P2 evaluation semantics (summary)

- L-path re-derives request, attempt and breaker lifecycles from typed records
  using its own tables; illegal transitions are `FAIL`, continuity breaks are
  `FAIL`, missing history is a gap, uninterpretable identity is `INVALID_INPUT`.
- S-path validates the snapshot structurally (`INVALID_INPUT` on defects) and
  then checks conservation, ownership, queue discipline, lane bounds, counters,
  budgets, breakers, probe permits and horizon consistency (`FAIL` on
  violations). An absent snapshot makes every S property `INCONCLUSIVE`.
- X-path compares facts only where both paths establish them. Disagreements
  carry both evidence references and are classified `unclassified`; the verifier
  never blames the reference automatically. Complete-ledger-only checks state
  their gap when the ledger is truncated.
- The A–J scenarios are reused as inputs. `sentra/evidence` bridges a real
  reference run into a document and `verify` evaluates it; all ten currently
  reach `PASS`, which means the ledger, books and their agreement satisfy the P2
  contracts — not that M2's reported metrics have been cross-checked (P4).

## 5. Integrated Operational Workflow (Milestones 5–7)

Milestones 5–7 integrate the reference simulator and independent verifier into an
autonomous, governed, GitLab-native operational engineering pipeline.

```
+-----------------------------------------------------------------------------+
|                      M5: Autonomous Operations Agent (ops)                  |
|  - Telemetry Investigation: extracts factual measurements & degradation     |
|  - Hypothesis Generation: maps observed symptoms to tunable parameters     |
|  - Bounded Autonomous Experimentation: evaluates candidate configurations   |
|  - Technical Boundaries: strictly enforces editable allowlists & protections|
|  - Multi-Objective Ranking: scores candidates (goodput, waste, latency)     |
+-----------------------------------------------------------------------------+
                                      |
                                      v
+-----------------------------------------------------------------------------+
|                  Independent Verifier (verify) & Evidence (evidence)        |
|  - Adjudicates all candidates against 51 formal safety/liveness properties  |
|  - Fails closed: rejects unsafe candidates with deterministic findings      |
+-----------------------------------------------------------------------------+
                                      |
                                      v
+-----------------------------------------------------------------------------+
|                    M6: Governance & Authorization (gov)                     |
|  - Explicit Lifecycle State Machine: DRAFT -> EXPERIMENTING -> VERIFIED ->  |
|    PENDING_APPROVAL -> APPROVED -> PROMOTED                                 |
|  - Machine-Enforceable Policy: bounds parameter ranges, deltas, invariants  |
|  - Human Authorization Boundary: strict self-approval prevention            |
|  - Cryptographic Binding: HMAC-SHA256 over config & verification digests   |
|  - Append-Only Audit Trail: tamper-evident hash-chained event ledger        |
+-----------------------------------------------------------------------------+
                                      |
                                      v
+-----------------------------------------------------------------------------+
|                     M7: GitLab-Native Delivery (gitlab)                     |
|  - Proposal Branches & Artifact Bundling: proposal.json, config.diff, MRs   |
|  - 10-Stage GitLab CI Pipeline: independently reproduces sim & adjudication |
|  - Controlled Promotion Controller: enforces pre-flight checks, deploys,    |
|    and executes post-promotion validation simulation                        |
+-----------------------------------------------------------------------------+
```

### Trust and Authority Separation
- **Agent (`ops`):** Untrusted generator of operational hypotheses, candidate configurations, and proposals within strictly enforced technical allowlists.
- **Independent Verifier (`verify`):** Sole authority for adjudicating evidence compliance against mathematical invariants without reference simulator imports.
- **Governance (`gov`):** Enforces policy compliance, lifecycle integrity, approver role eligibility, and tamper-evident audit logging.
- **Human Approver:** Holds exclusive authority to authorize promotion; agent self-approval is cryptographically and procedurally forbidden.
- **GitLab CI (`gitlab`):** Authoritative execution platform independently reproducing simulation, verification, and policy gates.
- **Promotion Controller:** Executes controlled deployment only upon valid, un-drifted human authorization, followed by mandatory post-promotion validation.

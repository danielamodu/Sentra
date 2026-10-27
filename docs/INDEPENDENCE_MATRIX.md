# Independence Matrix (Milestone 3, P2–P3)

This document records, artifact by artifact, what the independent verifier
shares with the reference system, what it re-derives, what it must never touch,
and where residual coupling remains. It complements `docs/ARCHITECTURE.md` §3
(reference-vs-simulator independence) by applying the same discipline to the
verification authority.

The rule of thumb: **data contracts may be shared; decisions may not.** A shared
type name is a vocabulary agreement. A shared function that decides a transition,
a resource allocation or an invariant is an authority leak.

## 1. Package dependency matrix

```
sim       -> engine -> domain            reference implementation
evidence  -> sim, verify, domain         serializer and test harness
verify    -> domain, standard library    independent authority
```

| Package | Allowed imports | Forbidden imports | Enforcement |
|---|---|---|---|
| `verify` | `sentra/domain`, standard library | `sentra/sim`, `sentra/engine`, `sentra/evidence` | `verify/independence_test.go` (AST scan, tests included) |
| `evidence` | `sentra/sim`, `sentra/verify`, `sentra/domain`, standard library | anything upward from `verify` | package boundary; it contains no verification logic |
| `sim`, `engine`, `domain` | each other, standard library | `verify`, `evidence` | the dependency graph has no edge in that direction |

`verify` currently imports only the standard library. The `sentra/domain`
allowance is a ceiling, not a dependency: the verifier re-declares the lifecycle
vocabulary and tables itself (see §3). This is stronger than the documented
boundary and is checked by the same AST test.

## 2. Authority matrix

| Decision | Reference (`sim`) | Bridge (`evidence`) | Verifier (`verify`) |
|---|---|---|---|
| Which events are scheduled and when | decides | copies | never |
| Which transition applies | decides | copies | evaluates against its own contract |
| Which resource holder wins | decides | copies final books | checks the resulting books |
| Whether the run is valid | asserts (panics) | never | decides properties, never repairs |
| Whether evidence is sufficient | n/a | never | decides (`INCONCLUSIVE` + gap) |
| Whether paths agree | n/a | never | decides and reports disagreement |
| Aggregate verdict | n/a | never | decides |

The bridge is deliberately the only package that sees both sides, and it is the
only one forbidden from deciding anything.

## 3. Per-artifact grading

### 3.1 Shared by construction (vocabulary and data contracts)

| Artifact | Why sharing is safe |
|---|---|
| Document schema `sentra-verify/v1`, record schema version | Wire contract; a mismatch is rejected, not interpreted |
| Record categories (`event`, `note`) and kinds | Closed taxonomy; an unknown kind is `INVALID_INPUT`, never ignored |
| State names (`admitted`, `queued`, …) | Documented vocabulary. The verifier declares the same strings in `verify/lifecycle.go`; it does not import the constants |
| Identity strings (`RequestID`, `AttemptID`, tenant, upstream) | Pure identity; no decision encoded |
| Input contract shape (pool size, capacities, lane bounds, budget, breaker policy) | Declared configuration, copied from the experiment; validated independently |

### 3.2 Re-derived by the verifier (must not delegate)

| Artifact | Re-derived in | Reference code it must not call |
|---|---|---|
| Request transition legality | `verify/lifecycle.go` | `domain.CanTransitionRequest`, `domain.requestTransitions` |
| Attempt transition legality | `verify/lifecycle.go` | `domain.CanTransitionAttempt`, `domain.attemptTransitions` |
| Breaker transition legality | `verify/lifecycle.go` | breaker transition application in `sim` |
| Attempt parentage | `verify/world.go` | `LogicalRequest.Attempts` back-links |
| Continuity of histories | `verify/world.go` | nothing equivalent exists in the reference (it panics) |
| Worker conservation/ownership | `verify/snapshot.go` | `domain.WorkerPool`, `SystemState.Validate` |
| Connection ownership/waitlists/stranding | `verify/snapshot.go` | `domain.ConnectionPool` methods |
| Queue membership and lane bounds | `verify/snapshot.go` | `domain.Queue`, pending-book bookkeeping |
| Outcome-ledger arithmetic | `verify/snapshot.go` | `SystemState.OutcomeCounters` validation |
| Probe-permit ledger | `verify/snapshot.go`, `verify/crosscheck.go` | `SystemState.Validate` |
| Retry-budget consumption counts | `verify/world.go` | `domain.RetryBudget.TryConsume` |
| Horizon/stamp consistency | `verify/snapshot.go` | M2 timestamp bookkeeping code |
| Property inventory, evidence policy and ordering | `verify/registry.go` | no equivalent in the reference (it has no property registry) |

### 3.3 Forbidden entirely

- `sim.Handle`, any transition handler, and `sim.Sim`.
- `sim.SystemState` and every map inside it.
- `SystemState.Validate`, `ConsumeRetryBudget`, occupancy timelines.
- `sim` analytics (`system_metrics.go`, `recovery.go`), scenario calibration and
  result plumbing.
- `domain.CanTransition*`, `domain.RetryBudget.TryConsume`,
  `domain.WorkerPool`/`Queue`/`ConnectionPool` mutating methods.
- `evidence` itself (the serializer).

### 3.4 The one reference change (D1)

P2 adds typed fields to the `attempt-created` note (`Request`, `Attempt`,
`Tenant`, `Upstream`, `Lane`) in `sim/recorder.go` and `sim/handlers.go`.

Why this does not reduce independence:

- It is evidence plumbing only: no scheduling, ordering, state, retry, breaker or
  resource decision changes.
- The rendered description is byte-identical, so existing traces remain
  comparable.
- The verifier still decides whether the typed facts are consistent; the note
  supplies identity, not a verdict.
- Pre-D1 evidence is supported through the block-local fallback, so the change
  is not a silent evidence-format break.

## 4. Evidence-path matrix

Which path establishes which fact. `L` = ledger reconstruction, `S` = snapshot
arithmetic, `X` = cross-path comparison, `—` = not established at all.

| Fact | L | S | X | Notes |
|---|---|---|---|---|
| Request lifecycle order | yes | final state only | terminal-dependent | X needs terminal proof |
| Attempt lifecycle order | yes | final state only | terminal-dependent | X needs terminal proof |
| Attempt parentage | typed note / fallback | attempt.request | yes | fallback gaps reported; no terminal need |
| Breaker lifecycle | yes (event payload) | final state/counters | terminal-dependent | a terminal-complete ledger with no transition pins closed |
| Worker occupancy | prose only (`worker` note) | yes | — | never string-parsed |
| Connection occupancy/waitlist | prose only | yes | — | — |
| Queue membership/order | prose only | yes | — | order is evidence |
| Lane bounds including pending | — | yes | — | needs pending books |
| Tenant active counts | non-terminal requests | counters | terminal-dependent | full-history need |
| Outcome counters | — | attempts vs counters | — | determination and counter equality only (S) |
| Event semantics | yes | — | — | Category F (attribution, attempt states, creation lanes, terminal closures, retry admission) |
| Retry-budget consumption | budget notes vs `RetryScheduled` | consumed rows | terminal-dependent | head-complete for the L-only ledger; X needs terminal proof |
| Probe permits | reserve/release counts | per-upstream `IsProbe` vs `ProbesActive` | terminal-dependent aggregate | no upstream attribution |
| Horizon/stamp consistency | transition instants | all stamps | — | occupancy at horizon is legal |
| Declared arrivals/faults | yes | declaration-shape validation | — | Category H (envelopes, bursts, fault schedules; seeded replay is explicit gap) |
| Reported M2 metrics | — | — | — | **P4 gap** |

## 5. Residual coupling and honest limitations

1. **The snapshot is produced by the reference-side state.** The bridge copies
   final books verbatim; it does not certify them. If the reference state is
   internally consistent but wrong about what happened, S-path can pass and
   X-path only catches facts the ledger also records. The L-path and the
   independent arithmetic are the counterweight, not a proof of ground truth.
2. **Shared vocabulary is a real dependency.** If the reference renames a state
   or note kind, verification fails loudly (`INVALID_INPUT`, unknown vocabulary)
   rather than silently mis-reading it. That is the intended failure mode: a
   contract change must land in `docs/SYSTEM_MODEL.md` and the verifier together.
3. **Probe permits are aggregate on the X-path.** `probe-reserved` and
   `probe-released` notes carry attempt identity in prose only, so the verifier
   compares totals and does not invent an upstream attribution.
4. **History completeness is explicitly attested via additive TerminalAttestation.**
   Head completeness (`FirstEffectSeq == 0`) and terminal completeness
   (`evidence.Terminal` matching retained evidence) are independently validated.
   Head truncation cannot be masked by a terminal marker. When evidence is
   terminal-unestablished or truncated, properties declare skip or cap policies;
   observed contradictions in retained evidence always produce `FAIL`.
5. **Retry-budget totals are head-complete-ledger-only.** Ring truncation can
   split a scheduled retry from its budget note; the property is then
   `INCONCLUSIVE`.
6. **Queue semantics and fairness boundaries.** `C-queue-discipline-fifo` audits
   arrival order to dispatch order per tenant, and `C-fairness-work-conserving`
   audits that idle capacity is allocated to eligible queued attempts within
   documented concurrency limits.
7. **No analytics cross-check.** M2 summary, phase, amplification and recovery
   numbers are not serialized into the document yet (P4), so a P3 `PASS` says
   nothing about them.
8. **Independent PRNG replay is not implemented (`H-seeded-replay`).** Workload
   arrival envelopes, total burst volumes, and fault activation/recovery
   intervals are independently cross-checked against input contracts.
   Re-generating exact stochastic arrival streams from raw PRNG seeds is left
   as an explicit gap, never an unverified pass.
9. **Scenario definitions are reused as inputs.** The bridge copies the
   experiment configuration, and the A–J tests reuse scenario inputs, but never
   reference transition or metric code. Reusing input data is a scenario-level
   contract (ARCHITECTURE §3), not an authority leak.

## 6. Enforcement summary

| Boundary | Mechanism | Failure mode |
|---|---|---|
| `verify` may not import reference code | AST import scan over all files, tests included; forbidden set pinned | test failure |
| Verifier cannot mutate reference state | no reference types or handles are imported | compile error |
| Bridge cannot decide properties | no import of verification logic is possible from `evidence` upwards; it only constructs values | review + test design |
| Evidence is not repaired | pipeline order: validate → reconstruct → evaluate; structural defects stop the pipeline | `INVALID_INPUT` |
| Missing evidence is visible | every absent snapshot path yields `INCONCLUSIVE` + gap; skip/cap policies on head-truncated or total-count checks | `INCONCLUSIVE` |
| Disagreement is not blame | X-path findings are always `unclassified` with both references | review + tests |

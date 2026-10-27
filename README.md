# Sentra — Milestone 1: Core Foundation

Sentra investigates, proposes and independently adjudicates operational
configuration changes for distributed infrastructure. Its central failure
class is **metastable failure**: temporary external degradation that triggers
internal feedback loops which sustain degraded behaviour even after the
external disturbance disappears.

This repository currently contains **Milestone 1 only**: the foundational
domain model and the deterministic event engine. There is no AI agent, no
GitLab integration, no optimization engine, no governance UI and no production
deployment path — and nothing in the code claims otherwise.

## Layout

```
domain/     Pure, strongly typed domain model (no dependencies, no I/O):
            requests, attempts, tenants, upstreams, workers, pools, queues,
            retry budgets, circuit breakers, simulated-time primitives.
engine/     Deterministic discrete-event core: ordering by
            (simulated time, priority class, scheduling sequence), no
            goroutines, no timers, no wall-clock reads.
sim/        The M1 reference system: ALL state-transition logic lives here,
            plus the system state and the structured recorder.
docs/       System model, architecture, engine semantics, assumptions.
```

## What is implemented

- Explicit logical-request lifecycle **separate** from physical-attempt
  lifecycle, including attempts that keep executing after their logical
  request has expired, been cancelled, or been completed by a sibling
  attempt. Usefulness of a result (`AttemptOutcome`) is tracked separately
  from execution state. Retraction of pre-execution attempts is a single
  centralized, idempotent exit path that frees every held resource exactly
  once.
- A deterministic execution engine with total event ordering
  `(simulated time, priority class, scheduling sequence)` — applied
  identically to injected and generated events — with documented race
  resolutions, injectable events and repeatable runs from identical initial
  conditions. The handler receives the event's scheduling seq for evidence
  correlation.
- A typed 18-event taxonomy (arrival, admission, queueing, dispatch, worker
  assignment, execution start/completion, timeout, cancellation request and
  acknowledgement, retry scheduling/execution with explicit re-admission,
  deadline expiration, upstream degradation/recovery, circuit-breaker
  transitions, load shedding, plus one auxiliary **payload-free**
  connection-grant wake event).
- Full M1 state tracking with a single-pass O(N) invariant validator that
  provably detects corruption: requests and attempts, two bounded queue
  lanes plus an explicit total bound per tenant (with pending-admission
  books closing same-instant overshoot), worker occupancy, connection
  occupancy and waitlists, retry budgets, circuit breakers with an **exact**
  probe-permit ledger, upstream availability/latency, and per-tenant outcome
  counters with an exactly-checked ledger.
- Deterministic M1 admission policy with recorded shed reasons
  (`breaker-open`, `tenant-queue-full`, …); retries re-run admission at
  their execution instant (never against an OPEN/HALF_OPEN breaker, never
  consuming probes) and blocked retries fail explicitly with recorded
  outcomes and a non-refunded budget token.
- Checked simulated-time arithmetic: scheduling overflows fail fast with a
  distinct "simulated time overflow" error instead of silent wrap-around.
- A versioned, typed evidence ledger: every processed event and every
  applied effect is one record carrying `engine_seq` (N:1 event→effect
  correlation) and a gapless `effect_seq`; explicit typed serialization
  (no `%v` dumps), bounded in-memory ring sink and streaming JSONL sink
  with sticky failure semantics; round-trippable JSON.
- Per-tenant fairness metrics: queue-wait distributions (log2 histogram),
  dispatch share, shed-reason mix, useful-work share and the Jain fairness
  index.
- Behavioural tests for identity separation, event ordering incl. exhaustive
  priority-class coverage and generated-vs-injected ordering, tie-breaking,
  deadline/cancellation races, worker/connection accounting, queue lane
  bounds, retry admission gates, breaker lifecycle, run repeatability
  (bit-identical records) and validator corruption detection.

## Build & test

```
go build ./...
go test ./... -count=1
go vet ./...
gofmt -l .   # must print nothing
```

Requires Go 1.24+ (developed with Go 1.26). No third-party dependencies.

## Documentation

- [`docs/SYSTEM_MODEL.md`](docs/SYSTEM_MODEL.md) — entities, state ownership,
  lifecycles, resource accounting.
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — package structure,
  dependency rules, the reference-system-vs-independent-simulator contract.
- [`docs/ENGINE_SEMANTICS.md`](docs/ENGINE_SEMANTICS.md) — event ordering,
  time, tie-breaking, race resolutions, event taxonomy.
- [`docs/ASSUMPTIONS.md`](docs/ASSUMPTIONS.md) — explicit assumptions,
  documented decisions, unresolved semantics, known limitations.

## Not implemented (later milestones)

Metastability *detection*, proposal/optimization, governance adjudication,
experiment comparison between independent simulators, GitLab integration and
the agent layer do **not** exist yet and are deliberately not stubbed.

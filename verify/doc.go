// Package verify is Sentra's independent verification authority (Milestone 3).
//
// The reference system (package sim) produces behaviour; this package evaluates
// that behaviour against contracts. It never executes, schedules or predicts
// system behaviour, and it never imports the reference implementation:
//
//	sentra/domain   shared vocabulary and documented lifecycle contracts
//	standard library only
//
// P2 code currently needs only the standard library: the verifier re-declares
// the documented lifecycles itself (verify/lifecycle.go) instead of calling
// domain transition functions, so the allowance is a ceiling, not a dependency.
//
// Packages sentra/sim, sentra/engine and sentra/evidence are forbidden imports.
// They are enforced by TestPackageIndependence, which parses every Go file in
// this package (including tests) and rejects any import outside the whitelist.
// sentra/evidence may import this package and sim because it is a serializer and
// test harness, never an authority: the dependency direction is strictly
// sim -> evidence -> verify, and verify -> nothing.
//
// # Pipeline
//
// Verification is an explicit pipeline (docs/VERIFIER_DESIGN.md):
//
//  1. document schema validation  -> INVALID_INPUT
//  2. input validation            -> INVALID_INPUT
//  3. evidence validation         -> INVALID_INPUT
//     snapshot structure (3b)     -> INVALID_INPUT
//  4. independent reconstruction  -> verifier-side world (L-path)
//  5. invariant evaluation        -> L/S properties: PASS/FAIL/INCONCLUSIVE
//  6. cross-path consistency      -> X properties: disagreement, never blame
//  7. verdict and evidence digest -> aggregate + machine-readable evidence
//
// Each stage is observable in the returned Verdict; no stage collapses into a
// single opaque boolean. A missing or insufficient input never becomes PASS:
// the affected properties are INCONCLUSIVE with a documented evidence gap, or
// the whole verdict is INVALID_INPUT when the document itself is unusable. An
// empty observation set is a gap, not a vacuous pass.
//
// # Evidence model
//
// The verifier consumes serialized evidence, not live reference objects:
//
//   - the ledger (records with effect_seq/engine_seq correlation) establishes
//     causal order, lifecycle transitions and effect multiplicity;
//   - a snapshot of final books establishes resource identity and bookkeeping;
//   - the input contract establishes the configuration the run declared.
//
// Cross-path checks compare only facts both paths establish; a disagreement is
// reported with both references and classified unclassified. Prose note
// descriptions are never string-parsed: they are diagnostic text, not a
// verification surface.
package verify

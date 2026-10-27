package verify

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// snapshotPropertyCount and crossPathPropertyCount derive the focused-helper
// sizes from the registry, so tests cannot drift from a second inventory.
func snapshotPropertyCount() int {
	n := 0
	for _, p := range propertyRegistry {
		if p.hasSource(evidenceSnapshot) && !p.hasSource(evidenceLedger) {
			n++
		}
	}
	return n
}

func crossPathPropertyCount() int {
	n := 0
	for _, p := range propertyRegistry {
		if p.hasSource(evidenceSnapshot) && p.hasSource(evidenceLedger) {
			n++
		}
	}
	return n
}

// TestRegistryStableInventory pins the authoritative ordered inventory and the
// approved category mapping. Category coverage is counted from the properties,
// never from a separate status.
func TestRegistryStableInventory(t *testing.T) {
	want := []string{
		"L-request-lifecycle",
		"L-attempt-lifecycle",
		"L-history-completeness",
		"L-attempt-linkage",
		"L-breaker-lifecycle",
		"L-retry-budget-ledger",
		"S-worker-conservation",
		"S-worker-ownership",
		"S-connection-ownership",
		"S-waitlist-membership",
		"S-connection-stranding",
		"S-horizon-completeness",
		"S-queue-consistency",
		"S-lane-bounds",
		"S-budget-state",
		"X-budget-consumption",
		"S-breaker-state",
		"S-probe-ledger",
		"X-breaker-final-state",
		"X-probe-ledger",
		"X-entity-sets",
		"X-request-final-state",
		"X-attempt-final-state",
		"X-attempt-parentage",
		"S-outcome-ledger",
		"S-tenant-active",
		"X-tenant-active",
		"F-effect-attribution",
		"F-attempt-state-effects",
		"F-creation-lane",
		"F-terminal-effect-closure",
		"F-retry-admission-context",
		"B-worker-interval-pairing",
		"B-worker-occupancy",
		"B-connection-interval-pairing",
		"D-retry-causality",
		"D-retry-backoff",
		"D-retry-limit",
		"D-retry-blocked",
		"C-admission-outcome",
		"C-queue-interval",
		"C-fifo-dispatch",
		"C-lane-bounds",
		"E-breaker-open-threshold",
		"E-breaker-cooldown",
		"E-probe-allowance",
		"E-failure-observation",
		"H-arrival-envelope",
		"H-burst-materialization",
		"H-fault-intervals",
		"H-seeded-replay",
	}
	got := propertyIDs()
	if len(got) != len(want) {
		t.Fatalf("registry has %d properties, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	seen := map[string]bool{}
	for _, p := range propertyRegistry {
		if p.ID == "" || p.Name == "" || p.Claim == "" {
			t.Errorf("property %q has an incomplete identity or claim: %+v", p.ID, p)
		}
		if seen[p.ID] {
			t.Errorf("property id %q is duplicated", p.ID)
		}
		seen[p.ID] = true
		if len(p.Sources) == 0 {
			t.Errorf("property %s declares no evidence sources", p.ID)
		}
		if p.Counterexample.radius() != 3 && p.Counterexample.Window <= 0 {
			t.Errorf("property %s has an invalid counterexample window", p.ID)
		}
		for _, sf := range p.Shortfalls {
			if sf.Reason == "" {
				t.Errorf("property %s declares a shortfall without a reason", p.ID)
			}
			switch sf.Mode {
			case shortfallSkip, shortfallCap:
			default:
				t.Errorf("property %s declares unknown shortfall mode %q", p.ID, sf.Mode)
			}
		}
	}

	counts := categoryCounts()
	wantCounts := map[propertyCategory]int{
		categoryLifecycle:   4,
		categoryResources:   9,
		categoryQueueing:    6,
		categoryRetryBudget: 7,
		categoryBreaker:     9,
		categorySemantics:   5,
		categoryCrossPath:   7,
		categoryWorkload:    4,
	}
	for cat, wantN := range wantCounts {
		if counts[cat] != wantN {
			t.Errorf("category %s has %d properties, want %d", cat, counts[cat], wantN)
		}
	}
	if snapshotPropertyCount() != 13 {
		t.Errorf("snapshot-only properties = %d, want 13", snapshotPropertyCount())
	}
	if crossPathPropertyCount() != 8 {
		t.Errorf("cross-path properties = %d, want 8", crossPathPropertyCount())
	}
	if !propertiesComplete(behaviorCaseNames()) {
		t.Fatal("the registered properties are not all implemented and bound")
	}
	missing := missingRequiredCategories(behaviorCaseNames())
	if len(missing) != 0 {
		t.Fatalf("missing required categories = %v, want []", missing)
	}
	if !P3Complete() {
		t.Fatal("P3Complete() = false when all required categories are implemented and tested")
	}
}

// TestP3RequiredCategoryGate proves that the completion gate is category-aware:
// an empty F or H category prevents completion even when every registered
// property is implemented and tested.
func TestP3RequiredCategoryGate(t *testing.T) {
	bound := behaviorCaseNames()
	counts := implementedTestedCategoryCounts(bound)

	full := make(map[propertyCategory]int, len(counts))
	for c, n := range counts {
		full[c] = n
	}
	for _, c := range requiredCategories {
		if full[c] == 0 {
			full[c] = 1
		}
	}
	if missing := missingCategoriesFrom(full); len(missing) != 0 {
		t.Fatalf("complete count map reports missing categories %v", missing)
	}

	for _, empty := range []propertyCategory{categorySemantics, categoryWorkload} {
		broken := make(map[propertyCategory]int, len(full))
		for c, n := range full {
			broken[c] = n
		}
		broken[empty] = 0
		missing := missingCategoriesFrom(broken)
		if len(missing) != 1 || missing[0] != empty {
			t.Fatalf("empty category %s reports missing %v, want [%s]", empty, missing, empty)
		}
	}
}

// TestRegistryCoverageStates pins the four coverage states and proves that a
// declaration which is not bound to an executed case is reported as
// declared-not-verified, never as tested. AST existence alone cannot satisfy
// the binding.
func TestRegistryCoverageStates(t *testing.T) {
	bound := behaviorCaseNames()
	for _, p := range propertyRegistry {
		if got := p.coverageWith(bound); got != coverageImplementedTested {
			t.Errorf("%s coverage = %s, want implemented-tested", p.ID, got)
		}
		// The live view must agree while the test binary is running.
		if got := p.coverage(); got != coverageImplementedTested {
			t.Errorf("%s live coverage = %s, want implemented-tested", p.ID, got)
		}
	}

	spec := registrySpec(t, "L-request-lifecycle")

	unimplemented := spec
	unimplemented.Check = nil
	if got := unimplemented.coverageWith(bound); got != coverageNotImplemented {
		t.Errorf("nil check coverage = %s, want not-implemented", got)
	}

	untested := spec
	untested.Tests = nil
	if got := untested.coverageWith(bound); got != coverageImplementedUntested {
		t.Errorf("no test declarations coverage = %s, want implemented-untested", got)
	}

	unbound := spec
	unbound.Tests = append([]propertyTest(nil), spec.Tests...)
	unbound.Tests[0].Case = spec.ID + "/not-an-executed-case"
	if got := unbound.coverageWith(bound); got != coverageDeclaredNotVerified {
		t.Errorf("unbound case coverage = %s, want declared-not-verified", got)
	}
	if got := unbound.coverageWith(nil); got != coverageDeclaredNotVerified {
		t.Errorf("nil binding coverage = %s, want declared-not-verified", got)
	}

	wrongTest := spec
	wrongTest.Tests = append([]propertyTest(nil), spec.Tests...)
	wrongTest.Tests[0].Test = "TestRegistryStableInventory"
	if got := wrongTest.coverageWith(bound); got != coverageDeclaredNotVerified {
		t.Errorf("wrong test function coverage = %s, want declared-not-verified", got)
	}

	if p3CompleteWith(nil) {
		t.Fatal("P3Complete treats declared-not-verified properties as complete")
	}
	if !propertiesComplete(bound) {
		t.Fatal("per-property completion = false with every declaration bound")
	}
	if !p3CompleteWith(bound) {
		t.Fatal("p3CompleteWith(bound) = false when all properties are bound and all categories implemented")
	}
}

// TestRegistryChecksReturnOwnIdentity runs the whole registry and asserts the
// executor contract: stable order, one result per property, own ID, valid status
// and no executor defects.
func TestRegistryChecksReturnOwnIdentity(t *testing.T) {
	ctx := validEvalContext(t)
	eval := evaluateAll(ctx)
	if len(eval.Defects) != 0 {
		t.Fatalf("registry defects: %+v", eval.Defects)
	}
	if len(eval.Results) != len(propertyRegistry) {
		t.Fatalf("results = %d, want %d", len(eval.Results), len(propertyRegistry))
	}
	for i, r := range eval.Results {
		if r.ID != propertyRegistry[i].ID {
			t.Fatalf("result[%d] = %q, want %q", i, r.ID, propertyRegistry[i].ID)
		}
		switch r.Status {
		case StatusPass, StatusFail, StatusInconclusive:
		default:
			t.Fatalf("property %s returned invalid status %q", r.ID, r.Status)
		}
		for _, g := range r.Gaps {
			if g.Property != r.ID {
				t.Fatalf("property %s has a gap attributed to %q", r.ID, g.Property)
			}
		}
	}
}

// TestRegistryShortfallEnforcement exercises the two declared policies: skip
// (no check, explicit gap) and cap (check runs, FAIL preserved, PASS capped).
func TestRegistryShortfallEnforcement(t *testing.T) {
	t.Run("missing snapshot skips every snapshot property", func(t *testing.T) {
		ctx := validEvalContext(t)
		ctx.snapshot = nil
		ctx.attemptByID = nil
		ctx.requestByID = nil
		eval := evaluateAll(ctx)
		if len(eval.Defects) != 0 {
			t.Fatalf("registry defects: %+v", eval.Defects)
		}
		byID := resultsByID(eval.Results)
		for _, p := range propertyRegistry {
			if !p.hasNeed(needSnapshot) {
				continue
			}
			got := byID[p.ID]
			if got == nil || got.Status != StatusInconclusive || len(got.Gaps) == 0 {
				t.Errorf("%s = %+v, want INCONCLUSIVE with a gap", p.ID, got)
			}
			if got != nil && len(got.Findings) != 0 {
				t.Errorf("%s blamed absent evidence: %+v", p.ID, got.Findings)
			}
		}
	})

	t.Run("head truncation caps the three approved properties", func(t *testing.T) {
		ctx := partialEvalContext(t)
		eval := evaluateAll(ctx)
		if len(eval.Defects) != 0 {
			t.Fatalf("registry defects: %+v", eval.Defects)
		}
		byID := resultsByID(eval.Results)
		for _, id := range []string{"L-history-completeness", "X-entity-sets", "X-breaker-final-state"} {
			p := byID[id]
			if p == nil || p.Status != StatusInconclusive {
				t.Errorf("%s = %+v, want INCONCLUSIVE on a head-truncated ledger", id, p)
			}
		}
	})

	t.Run("head-complete but terminal-unestablished caps terminal claims", func(t *testing.T) {
		ctx := validEvalContext(t)
		if ctx.ledger.Coverage != HistoryTerminalUnestablished {
			t.Fatalf("fixture coverage = %q, want %q", ctx.ledger.Coverage, HistoryTerminalUnestablished)
		}
		if !ctx.ledger.Coverage.headComplete() {
			t.Fatal("fixture must have a complete head for this adversarial case")
		}
		eval := evaluateAll(ctx)
		if len(eval.Defects) != 0 {
			t.Fatalf("registry defects: %+v", eval.Defects)
		}
		byID := resultsByID(eval.Results)
		for _, p := range propertyRegistry {
			if !p.hasNeed(needTerminalComplete) && !p.hasNeed(needFullHistory) {
				continue
			}
			got := byID[p.ID]
			if got == nil {
				t.Fatalf("%s missing from results", p.ID)
			}
			if got.Status != StatusInconclusive {
				t.Errorf("%s = %s on head-complete/terminal-unestablished evidence, want INCONCLUSIVE", p.ID, got.Status)
			}
			if len(got.Gaps) == 0 {
				t.Errorf("%s is INCONCLUSIVE without an evidence gap", p.ID)
			}
		}
	})

	t.Run("evidence needs are evaluated centrally and distinctly", func(t *testing.T) {
		ctx := validEvalContext(t)
		cases := []struct {
			need shortfallNeed
			want bool
		}{
			{needHeadComplete, false},
			{needTerminalComplete, true},
			{needFullHistory, true},
		}
		for _, c := range cases {
			active, reason := ctx.shortfallActive(shortfallPolicy{Need: c.need, Mode: shortfallSkip, Reason: "test"})
			if active != c.want {
				t.Errorf("need %s active = %t, want %t (coverage %q)", c.need, active, c.want, ctx.ledger.Coverage)
			}
			if active && reason != "test" {
				t.Errorf("need %s returned reason %q", c.need, reason)
			}
		}

		truncated := partialEvalContext(t)
		if active, _ := truncated.shortfallActive(shortfallPolicy{Need: needHeadComplete}); !active {
			t.Error("head-complete need is satisfied by a head-truncated ledger")
		}
	})

	t.Run("cap preserves an established violation", func(t *testing.T) {
		ctx := partialEvalContext(t)
		// The ledger saw req-1, but the snapshot no longer lists it. That
		// direction is established even with an evicted head and must FAIL.
		ctx.snapshot = validSnapshot()
		ctx.snapshot.Requests = nil
		ctx.snapshot.Attempts = nil
		// Structural validation is not part of this focused test; the check
		// reads the books directly.
		r, defect := evaluateProperty(registrySpec(t, "X-entity-sets"), ctx)
		if defect != nil {
			t.Fatalf("defect: %+v", defect)
		}
		if r.Status != StatusFail {
			t.Fatalf("X-entity-sets = %s, want FAIL despite the partial shortfall", r.Status)
		}
	})
}

func registrySpec(t *testing.T, id string) propertySpec {
	t.Helper()
	for _, p := range propertyRegistry {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no registry property %q", id)
	return propertySpec{}
}

// TestRegistryCounterexampleWindow checks the approved default radius, role
// labelling, truncation flags and the guarantee that the primary block survives
// clipping at both edges.
func TestRegistryCounterexampleWindow(t *testing.T) {
	// Eight blocks give a finding in the middle a full ±3 window and findings
	// at the edges a clipped one.
	b := newEvidence()
	for i := uint64(1); i <= 8; i++ {
		b.event(i, int64(i)*100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"})
	}
	led, fs := ValidateEvidence(b.done())
	if len(fs) != 0 {
		t.Fatalf("evidence did not validate: %+v", fs)
	}
	makeResult := func(effect, engine uint64) []PropertyResult {
		r := newCheck("X-entity-sets", "test")
		r.fail(Finding{Code: "X-entity-sets", Message: "test", EffectSeq: effect, EngineSeq: engine})
		return []PropertyResult{*r}
	}
	specs := map[string]propertySpec{"X-entity-sets": registrySpec(t, "X-entity-sets")}

	results := makeResult(5, 5)
	attachCounterexamples(&led, specs, results)
	cx := results[0].Findings[0].Counterexample
	if cx == nil {
		t.Fatal("no counterexample attached")
	}
	if len(cx.Blocks) != 7 || cx.Truncated {
		t.Fatalf("middle window = %d blocks, truncated=%t; want 7,false", len(cx.Blocks), cx.Truncated)
	}
	primary := 0
	for _, blk := range cx.Blocks {
		if blk.Role == counterexamplePrimary {
			primary++
			if blk.EngineSeq != 5 {
				t.Fatalf("primary block = %d, want 5", blk.EngineSeq)
			}
		} else if blk.Role != counterexampleContext {
			t.Fatalf("unexpected role %q: a property without causality must label neighbours context", blk.Role)
		}
	}
	if primary != 1 {
		t.Fatalf("primary blocks = %d, want 1", primary)
	}

	results = makeResult(1, 1)
	attachCounterexamples(&led, specs, results)
	cx = results[0].Findings[0].Counterexample
	if cx == nil || !cx.Truncated {
		t.Fatalf("head-clipped window = %+v, want truncated", cx)
	}
	if cx.Blocks[0].Role != counterexamplePrimary || cx.Blocks[0].EngineSeq != 1 {
		t.Fatalf("head clipping removed the primary block: %+v", cx.Blocks)
	}

	results = makeResult(8, 8)
	attachCounterexamples(&led, specs, results)
	cx = results[0].Findings[0].Counterexample
	if cx == nil || !cx.Truncated {
		t.Fatalf("tail-clipped window = %+v, want truncated", cx)
	}
	last := cx.Blocks[len(cx.Blocks)-1]
	if last.Role != counterexamplePrimary || last.EngineSeq != 8 {
		t.Fatalf("tail clipping removed the primary block: %+v", cx.Blocks)
	}

	// Per-property override: radius 1 yields three blocks around the primary.
	override := specs["X-entity-sets"]
	override.Counterexample.Window = 1
	specs["X-entity-sets"] = override
	results = makeResult(4, 4)
	attachCounterexamples(&led, specs, results)
	cx = results[0].Findings[0].Counterexample
	if cx == nil || len(cx.Blocks) != 3 {
		t.Fatalf("override window = %+v, want 3 blocks", cx)
	}

	// engine_seq 0 / effect_seq 0 is the run's first block, not "no reference".
	// A violation established there must still receive its primary block.
	first := newEvidence()
	first.event(0, 100, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		event(1, 200, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"})
	firstLed, fs := ValidateEvidence(first.done())
	if len(fs) != 0 {
		t.Fatalf("first-block evidence did not validate: %+v", fs)
	}
	results = makeResult(0, 0)
	attachCounterexamples(&firstLed, specs, results)
	cx = results[0].Findings[0].Counterexample
	if cx == nil || len(cx.Blocks) != 2 || !cx.Truncated {
		t.Fatalf("first-block window = %+v, want 2 blocks truncated", cx)
	}
	if cx.Blocks[0].Role != counterexamplePrimary || cx.Blocks[0].EngineSeq != 0 {
		t.Fatalf("first block is not the primary: %+v", cx.Blocks)
	}

	// A snapshot-only finding names no ledger block, even though the ledger
	// retains engine 0.
	snapshotOnly := []PropertyResult{*newCheck("S-worker-conservation", "test")}
	snapshotOnly[0].fail(Finding{Code: "S-worker-conservation", Message: "test", SnapshotRef: "pool:pool"})
	attachCounterexamples(&firstLed, specs, snapshotOnly)
	if snapshotOnly[0].Findings[0].Counterexample != nil {
		t.Fatalf("snapshot-only finding received a ledger window: %+v", snapshotOnly[0].Findings[0].Counterexample)
	}

	// A zero pair without a snapshot reference cites engine 0 only when the
	// retained window still contains it; an evicted head must not fabricate a
	// primary block.
	evicted := newEvidenceAt(10)
	evicted.event(5, 500, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"}).
		note(5, 500, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	evictedLed, fs := ValidateEvidence(evicted.done())
	if len(fs) != 0 {
		t.Fatalf("truncated evidence did not validate: %+v", fs)
	}
	results = makeResult(0, 0)
	attachCounterexamples(&evictedLed, specs, results)
	if results[0].Findings[0].Counterexample != nil {
		t.Fatalf("evicted-head finding received a fabricated window: %+v", results[0].Findings[0].Counterexample)
	}
}

// TestRegistryDeterministicOrderingAndAggregation protects the verdict contract:
// identical evidence yields byte-identical results, and aggregation follows the
// documented precedence.
func TestRegistryDeterministicOrderingAndAggregation(t *testing.T) {
	first := evaluateAll(validEvalContext(t))
	second := evaluateAll(validEvalContext(t))
	a, err := CanonicalJSON(first.Results)
	if err != nil {
		t.Fatalf("canonical first: %v", err)
	}
	b, err := CanonicalJSON(second.Results)
	if err != nil {
		t.Fatalf("canonical second: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("registry evaluation is not deterministic:\n%s\n%s", a, b)
	}
	// The default fixture is head-complete but terminal-unestablished, so
	// aggregation is INCONCLUSIVE even though no property fails.
	if got := Aggregate(first.Results); got != StatusInconclusive {
		t.Fatalf("head-complete fixture aggregates to %s, want INCONCLUSIVE", got)
	}
	if got := Aggregate(nil); got != StatusInconclusive {
		t.Fatalf("empty result set aggregates to %s, want INCONCLUSIVE", got)
	}

	// With a (synthetic) terminal-complete ledger the same evidence passes,
	// which shows the INCONCLUSIVE aggregate is an evidence limit rather than
	// a property failure.
	complete := evaluateAll(terminalCompleteContext(t, nil))
	if len(complete.Defects) != 0 {
		t.Fatalf("registry defects on terminal-complete context: %+v", complete.Defects)
	}
	var nonReplay []PropertyResult
	for _, r := range complete.Results {
		if r.ID != "H-seeded-replay" {
			nonReplay = append(nonReplay, r)
		}
	}
	if got := Aggregate(nonReplay); got != StatusPass {
		t.Fatalf("terminal-complete fixture aggregates to %s, want PASS (gaps %+v)", got, nonReplay)
	}
}

// TestHistoryCoverageTriState pins what the evidence contract can prove. Under
// schema v1, completeness is never proven because there is no terminal marker.
func TestHistoryCoverageTriState(t *testing.T) {
	led, fs := ValidateEvidence(validLifecycleEvidence())
	if len(fs) != 0 {
		t.Fatalf("valid evidence rejected: %+v", fs)
	}
	if led.Coverage != HistoryTerminalUnestablished {
		t.Fatalf("complete-head ledger coverage = %q, want %q", led.Coverage, HistoryTerminalUnestablished)
	}
	if led.Coverage == HistoryComplete {
		t.Fatal("contract v1 cannot prove a terminal boundary")
	}

	b := newEvidenceAt(4)
	b.event(9, 900, "RequestArrival", Detail{Tenant: "T1", Upstream: "U1"})
	led, fs = ValidateEvidence(b.done())
	if len(fs) != 0 {
		t.Fatalf("truncated evidence rejected: %+v", fs)
	}
	if led.Coverage != HistoryHeadTruncated {
		t.Fatalf("truncated ledger coverage = %q, want %q", led.Coverage, HistoryHeadTruncated)
	}
}

// TestHistoryCompletenessRequiresCreationEvidence covers the approved repair: a
// truncated window that happens to begin at "admitted" is not proof that the
// request started there.
func TestHistoryCompletenessRequiresCreationEvidence(t *testing.T) {
	b := newEvidenceAt(10)
	b.event(20, 200, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(20, 200, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	w := reconstructed(t, b.done())
	r := w.RequestByID["req-1"]
	if r == nil {
		t.Fatal("request not reconstructed")
	}
	if r.Complete {
		t.Fatal("truncated history beginning at admitted claimed complete without creation evidence")
	}
	p := propertyByID(lPathChecks(w), "L-history-completeness")
	if p == nil || p.Status != StatusInconclusive {
		t.Fatalf("L-history-completeness = %+v, want INCONCLUSIVE", p)
	}

	// Retaining the typed admission decision is creation evidence even in a
	// head-truncated window: no earlier request transition can precede it.
	c := newEvidenceAt(10)
	c.event(19, 100, "AdmissionDecided", Detail{Request: "req-1", Tenant: "T1", Upstream: "U1"})
	c.event(20, 200, "QueueInsertion", Detail{Attempt: "att-1"}).
		note(20, 200, "request-transition", Detail{Request: "req-1", From: "admitted", To: "queued"})
	w = reconstructed(t, c.done())
	if r := w.RequestByID["req-1"]; r == nil || !r.Complete {
		t.Fatalf("request with retained admission evidence = %+v, want complete", r)
	}
}

// TestRegistryDeclarationsAreVerified binds every declared test case to an
// executed behavioral case. The AST scan proves the Go test function and case
// literal exist; the behavioral table proves the case runs. Neither alone is
// treated as semantic coverage.
func TestRegistryDeclarationsAreVerified(t *testing.T) {
	cases := behaviorCaseNames()
	for _, p := range propertyRegistry {
		declared := map[string]bool{}
		for _, tc := range p.Tests {
			if tc.Test == "" || tc.Case == "" {
				t.Errorf("property %s has an incomplete test declaration: %+v", p.ID, tc)
				continue
			}
			declared[tc.Case] = true
			if !functionExistsInTests(t, tc.Test) {
				t.Errorf("property %s declares missing test function %q", p.ID, tc.Test)
			}
			if !cases[tc.Case] {
				t.Errorf("property %s declares case %q that no behavioral case exercises", p.ID, tc.Case)
			}
		}
		for _, kind := range p.requiredTestKinds() {
			want := p.ID + "/" + string(kind)
			if !declared[want] {
				t.Errorf("property %s is missing its required %s test declaration %q", p.ID, kind, want)
			}
		}
	}
	for name := range cases {
		id := strings.SplitN(name, "/", 2)[0]
		spec := findSpec(id)
		if spec == nil {
			t.Errorf("behavioral case %q names no registry property", name)
			continue
		}
		found := false
		for _, tc := range spec.Tests {
			if tc.Case == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("behavioral case %q is not declared by property %s", name, id)
		}
	}
}

func findSpec(id string) *propertySpec {
	for i := range propertyRegistry {
		if propertyRegistry[i].ID == id {
			return &propertyRegistry[i]
		}
	}
	return nil
}

// functionExistsInTests parses the package's test files and reports whether a
// function with the given name is declared. It is an existence check only; the
// behavioral table is what establishes that the case runs.
func functionExistsInTests(t *testing.T, name string) bool {
	t.Helper()
	entries, err := parser.ParseDir(token.NewFileSet(), ".", func(fi fs.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse test package: %v", err)
	}
	for _, pkg := range entries {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name.Name == name {
					return true
				}
			}
		}
	}
	return false
}

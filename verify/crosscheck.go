package verify

import "fmt"

// Cross-path checks compare facts that the ledger (L-path) and the final-state
// snapshot (S-path) establish *independently*. They are only meaningful where
// both paths actually establish the same fact:
//
//   - entity identity: the ledger names every request/attempt it observed, and
//     the snapshot lists the final books;
//   - final lifecycle state and terminal instant: the ledger's retained tail is
//     authoritative up to the horizon, the snapshot is taken at the horizon;
//   - attempt parentage: both paths carry a typed parent request;
//   - breaker final state, consumed retry tokens and probe permits: ledger
//     counters versus snapshot books, only when the ledger is complete.
//
// A disagreement is reported as a finding with both evidence references and is
// classified `unclassified`: the evidence admits "one of the two books is
// wrong", but not which one, and the verifier never attributes a disagreement
// to the reference implementation automatically (D6, docs/INDEPENDENCE_MATRIX.md).
//
// When either path does not establish the fact (no snapshot, truncated ledger),
// the affected property is INCONCLUSIVE with an explicit gap. It is never
// silently skipped and never assumed to agree.

func checkEntitySets(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-entity-sets", "the ledger and the snapshot describe the same entity population")

	snapReq := make(map[string]*RequestFact, len(s.Requests))
	for i := range s.Requests {
		snapReq[s.Requests[i].ID] = &s.Requests[i]
	}
	snapAtt := make(map[string]*AttemptFact, len(s.Attempts))
	for i := range s.Attempts {
		snapAtt[s.Attempts[i].ID] = &s.Attempts[i]
	}

	for _, rw := range w.Requests {
		if _, ok := snapReq[rw.ID]; !ok {
			r.fail(Finding{
				Code:           "X-entity-sets",
				Message:        fmt.Sprintf("request %s was observed in the ledger but is absent from the snapshot books", rw.ID),
				EffectSeq:      rw.FirstSeenSeq,
				EngineSeq:      rw.FirstSeenEngine,
				Request:        rw.ID,
				Expected:       "request present in snapshot",
				Observed:       "absent",
				SnapshotRef:    "snapshot.requests",
				Classification: ClassUnclassified,
			})
		}
	}
	for _, aw := range w.Attempts {
		if _, ok := snapAtt[aw.ID]; !ok {
			r.fail(Finding{
				Code:           "X-entity-sets",
				Message:        fmt.Sprintf("attempt %s was observed in the ledger but is absent from the snapshot books", aw.ID),
				EffectSeq:      aw.FirstSeenSeq,
				EngineSeq:      aw.FirstSeenEngine,
				Attempt:        aw.ID,
				Request:        aw.Request,
				Expected:       "attempt present in snapshot",
				Observed:       "absent",
				SnapshotRef:    "snapshot.attempts",
				Classification: ClassUnclassified,
			})
		}
	}

	// Equality in the other direction is only established on full history: with
	// an evicted head, older entities legitimately predate the retained window,
	// and with an unproven tail an entity may be created after the last retained
	// record. Adding an entity the ledger never saw to a full-history ledger is
	// a disagreement, not an eviction.
	if !ctx.ledger.headComplete() || !ctx.ledger.terminalComplete() {
		return *r
	}
	for i := range s.Requests {
		f := &s.Requests[i]
		if _, ok := w.RequestByID[f.ID]; !ok {
			r.fail(Finding{
				Code:           "X-entity-sets",
				Message:        fmt.Sprintf("request %s is in the snapshot books but no ledger record establishes it, although the ledger covers the full run", f.ID),
				Request:        f.ID,
				Expected:       "request established by the ledger",
				Observed:       "snapshot-only entity",
				SnapshotRef:    "request:" + f.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	for i := range s.Attempts {
		f := &s.Attempts[i]
		if _, ok := w.AttemptByID[f.ID]; !ok {
			r.fail(Finding{
				Code:           "X-entity-sets",
				Message:        fmt.Sprintf("attempt %s is in the snapshot books but no ledger record establishes it, although the ledger covers the full run", f.ID),
				Attempt:        f.ID,
				Request:        f.Request,
				Expected:       "attempt established by the ledger",
				Observed:       "snapshot-only entity",
				SnapshotRef:    "attempt:" + f.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkRequestFinalStates(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-request-final-state", "final request states and terminal instants agree across paths")
	snap := make(map[string]*RequestFact, len(s.Requests))
	for i := range s.Requests {
		snap[s.Requests[i].ID] = &s.Requests[i]
	}

	for _, rw := range w.Requests {
		f, ok := snap[rw.ID]
		if !ok || rw.State == "" {
			// Entity-set disagreement or no lifecycle observation; this check
			// makes no claim it cannot establish.
			continue
		}
		// A terminal request state is absorbing: no later record can change it,
		// so a disagreement with the snapshot is a positive contradiction even
		// when the ledger tail is unproven. A non-terminal reconstruction needs
		// the proven terminal boundary before its state may be called final.
		if !ctx.finalityEstablished(rw.Terminal) {
			continue
		}
		eff, eng := lastTransitionRef(rw.Transitions)
		if rw.State != f.State {
			r.fail(Finding{
				Code:           "X-request-final-state",
				Message:        fmt.Sprintf("ledger reconstructs request %s as %q but the snapshot books it as %q", rw.ID, rw.State, f.State),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Request:        rw.ID,
				Expected:       rw.State,
				Observed:       f.State,
				SnapshotRef:    "request:" + rw.ID,
				Classification: ClassUnclassified,
			})
			continue
		}
		if rw.Terminal && f.TerminalAtNS != rw.TerminalAtNS {
			r.fail(Finding{
				Code:           "X-request-final-state",
				Message:        fmt.Sprintf("request %s terminalized at %dns in the ledger but at %dns in the snapshot", rw.ID, rw.TerminalAtNS, f.TerminalAtNS),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Request:        rw.ID,
				Expected:       fmt.Sprintf("terminal_at_ns=%d", rw.TerminalAtNS),
				Observed:       fmt.Sprintf("terminal_at_ns=%d", f.TerminalAtNS),
				SnapshotRef:    "request:" + rw.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkAttemptFinalStates(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-attempt-final-state", "final attempt states and finish instants agree across paths")
	snap := make(map[string]*AttemptFact, len(s.Attempts))
	for i := range s.Attempts {
		snap[s.Attempts[i].ID] = &s.Attempts[i]
	}

	for _, aw := range w.Attempts {
		f, ok := snap[aw.ID]
		if !ok || aw.State == "" {
			continue
		}
		// Same asymmetry as the request check: a terminal attempt state is
		// absorbing, a non-terminal one is only final on a proven tail.
		if !ctx.finalityEstablished(aw.Terminal) {
			continue
		}
		eff, eng := lastTransitionRef(aw.Transitions)
		if aw.State != f.State {
			r.fail(Finding{
				Code:           "X-attempt-final-state",
				Message:        fmt.Sprintf("ledger reconstructs attempt %s as %q but the snapshot books it as %q", aw.ID, aw.State, f.State),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Attempt:        aw.ID,
				Request:        aw.Request,
				Expected:       aw.State,
				Observed:       f.State,
				SnapshotRef:    "attempt:" + aw.ID,
				Classification: ClassUnclassified,
			})
			continue
		}
		if aw.Terminal && f.FinishedAtNS != aw.FinishedAtNS {
			r.fail(Finding{
				Code:           "X-attempt-final-state",
				Message:        fmt.Sprintf("attempt %s finished at %dns in the ledger but at %dns in the snapshot", aw.ID, aw.FinishedAtNS, f.FinishedAtNS),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Attempt:        aw.ID,
				Request:        aw.Request,
				Expected:       fmt.Sprintf("finished_at_ns=%d", aw.FinishedAtNS),
				Observed:       fmt.Sprintf("finished_at_ns=%d", f.FinishedAtNS),
				SnapshotRef:    "attempt:" + aw.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkAttemptParentage(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-attempt-parentage", "attempt-to-request attribution agrees across paths")
	snap := make(map[string]*AttemptFact, len(s.Attempts))
	for i := range s.Attempts {
		snap[s.Attempts[i].ID] = &s.Attempts[i]
	}

	unlinked := 0
	for _, aw := range w.Attempts {
		f, ok := snap[aw.ID]
		if !ok {
			continue
		}
		if !aw.RequestLinked {
			unlinked++
			continue
		}
		if f.Request != aw.Request {
			eff, eng := lastTransitionRef(aw.Transitions)
			r.fail(Finding{
				Code:           "X-attempt-parentage",
				Message:        fmt.Sprintf("attempt %s is attributed to request %s by the ledger but to %q by the snapshot", aw.ID, aw.Request, f.Request),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Attempt:        aw.ID,
				Request:        aw.Request,
				Expected:       aw.Request,
				Observed:       f.Request,
				SnapshotRef:    "attempt:" + aw.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	if unlinked > 0 {
		r.addGap(fmt.Sprintf("%d attempt(s) have no establishable parent request in the retained ledger; parentage cannot be cross-checked for them", unlinked))
	}
	return *r
}

func checkBreakerFinalStates(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-breaker-final-state", "final breaker states agree across paths")
	snap := make(map[string]*BreakerFact, len(s.Breakers))
	for i := range s.Breakers {
		snap[s.Breakers[i].Upstream] = &s.Breakers[i]
	}

	for _, bw := range w.Breakers {
		f, ok := snap[bw.Upstream]
		if !ok || bw.State == "" {
			continue
		}
		// Breaker states are not absorbing (closed -> open -> half-open ->
		// closed), so the last retained state is only final when the ledger is
		// proven to reach the terminal boundary. Without that proof this check
		// establishes nothing and the declared cap reports the gap.
		if !ctx.ledger.terminalComplete() {
			continue
		}
		eff, eng := lastTransitionRef(bw.Transitions)
		if bw.State != f.State {
			r.fail(Finding{
				Code:           "X-breaker-final-state",
				Message:        fmt.Sprintf("ledger reconstructs breaker %s as %q but the snapshot books it as %q", bw.Upstream, bw.State, f.State),
				EffectSeq:      eff,
				EngineSeq:      eng,
				Upstream:       bw.Upstream,
				Expected:       bw.State,
				Observed:       f.State,
				SnapshotRef:    "breaker:" + bw.Upstream,
				Classification: ClassUnclassified,
			})
		}
	}
	// The reverse direction needs full history, not just an intact head: a
	// breaker may have transitioned after the retained tail. Only when both
	// boundaries are proven does "no transition observed" pin the documented
	// initial state.
	if ctx.ledger.headComplete() && ctx.ledger.terminalComplete() {
		// A full-history ledger with no BreakerTransitioned record establishes
		// that the breaker never left its documented initial state.
		for i := range s.Breakers {
			f := &s.Breakers[i]
			if _, ok := w.BreakerByUpstream[f.Upstream]; !ok && f.State != "closed" {
				r.fail(Finding{
					Code:           "X-breaker-final-state",
					Message:        fmt.Sprintf("the full-history ledger records no transition for breaker %s, but the snapshot books it as %q", f.Upstream, f.State),
					Upstream:       f.Upstream,
					Expected:       "closed",
					Observed:       f.State,
					SnapshotRef:    "breaker:" + f.Upstream,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	return *r
}

func checkBudgetConsumption(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-budget-consumption", "consumed retry budget agrees across paths")
	total := 0
	for _, c := range s.TokensConsumed {
		total += c.Consumed
	}
	if total != w.BudgetConsumptions {
		r.fail(Finding{
			Code:           "X-budget-consumption",
			Message:        fmt.Sprintf("the ledger records %d retry-budget consumptions but the snapshot reports %d consumed tokens", w.BudgetConsumptions, total),
			Expected:       fmt.Sprintf("consumed=%d", w.BudgetConsumptions),
			Observed:       fmt.Sprintf("consumed=%d", total),
			SnapshotRef:    "snapshot.retry_tokens_consumed",
			Classification: ClassUnclassified,
		})
	}
	return *r
}

func checkProbeLedger(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	r := newCheck("X-probe-ledger", "probe permits reserved and released agree with the snapshot")
	active := 0
	for _, b := range s.Breakers {
		active += b.ProbesActive
	}
	net := w.ProbeReserved - w.ProbeReleased
	if net < 0 {
		r.fail(Finding{
			Code:           "X-probe-ledger",
			Message:        fmt.Sprintf("the ledger records %d probe releases but only %d reservations", w.ProbeReleased, w.ProbeReserved),
			Expected:       "releases<=reservations",
			Observed:       fmt.Sprintf("released=%d reserved=%d", w.ProbeReleased, w.ProbeReserved),
			SnapshotRef:    "snapshot.breakers",
			Classification: ClassUnclassified,
		})
		return *r
	}
	if net != active {
		r.fail(Finding{
			Code:           "X-probe-ledger",
			Message:        fmt.Sprintf("the ledger leaves %d probe permit(s) open (%d reserved, %d released) but the snapshot books %d active probes", net, w.ProbeReserved, w.ProbeReleased, active),
			Expected:       fmt.Sprintf("active_probes=%d", net),
			Observed:       fmt.Sprintf("active_probes=%d", active),
			SnapshotRef:    "snapshot.breakers",
			Classification: ClassUnclassified,
		})
	}
	return *r
}

func checkTenantActiveCross(ctx *evalContext) PropertyResult {
	w := ctx.world
	s := ctx.snapshot
	in := ctx.input
	r := newCheck("X-tenant-active", "active-request counts agree across paths")
	computed := make(map[string]int, len(in.Tenants))
	for _, t := range in.Tenants {
		computed[t.ID] = 0
	}
	unknown := 0
	for _, rw := range w.Requests {
		if rw.Terminal {
			continue
		}
		if rw.Tenant == "" {
			unknown++
			continue
		}
		computed[rw.Tenant]++
	}
	if unknown > 0 {
		r.addGap(fmt.Sprintf("%d non-terminal request(s) have no tenant identity in the ledger; active counts cannot be compared", unknown))
		return *r
	}

	facts := make(map[string]int, len(s.TenantActive))
	for _, f := range s.TenantActive {
		facts[f.Tenant] = f.Active
	}
	for _, t := range in.Tenants {
		if got, want := facts[t.ID], computed[t.ID]; got != want {
			r.fail(Finding{
				Code:           "X-tenant-active",
				Message:        fmt.Sprintf("the ledger reconstructs %d active request(s) for tenant %s but the snapshot reports %d", want, t.ID, got),
				Tenant:         t.ID,
				Expected:       fmt.Sprintf("active=%d", want),
				Observed:       fmt.Sprintf("active=%d", got),
				SnapshotRef:    "tenant:" + t.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

// finalityEstablished reports whether a reconstructed entity's state may be
// called final. A terminal entity state is absorbing, so it is final on its own;
// a non-terminal state is final only when the retained ledger is proven to reach
// the run's terminal boundary. This is the cap-semantics rule for the cross-path
// final-state checks: the comparison runs, but only where the fact is
// establishable, so a positive contradiction still surfaces as FAIL while an
// unprovable one becomes an explicit gap.
func (ctx *evalContext) finalityEstablished(entityTerminal bool) bool {
	return entityTerminal || ctx.ledger.terminalComplete()
}

// lastTransitionRef returns the evidence reference of the last recorded
// transition, or zero when a history was established by identity only.
func lastTransitionRef(ts []Transition) (effectSeq, engineSeq uint64) {
	if len(ts) == 0 {
		return 0, 0
	}
	last := ts[len(ts)-1]
	return last.EffectSeq, last.EngineSeq
}

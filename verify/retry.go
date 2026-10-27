package verify

import "fmt"

// This file implements category D: retry semantics. Retries are causally linked
// to a failed or timed-out predecessor attempt, spaced by the configured backoff
// duration, bounded by each request's declared retry limit, and cleanly blocked
// when upstream breakers or queues cannot admit them.

// checkRetryCausality verifies that every RetryScheduled event names a predecessor
// attempt that belongs to the same request, that the predecessor was in a terminal
// state (failed or timed_out), and that the request was placed into retrying with
// a consumed budget token.
func checkRetryCausality(ctx *evalContext) PropertyResult {
	r := newCheck("D-retry-causality", "retry scheduling is causally linked to a finalized predecessor attempt")
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil || b.Event.Kind != "RetryScheduled" {
			continue
		}
		ev := b.Event
		reqID := ev.Detail.Request
		predID := ev.Detail.Predecessor
		if reqID == "" {
			r.addGap(fmt.Sprintf("RetryScheduled event at effect_seq %d names no request", ev.EffectSeq))
			continue
		}
		if predID == "" {
			r.addGap(fmt.Sprintf("RetryScheduled event at effect_seq %d names no predecessor", ev.EffectSeq))
			continue
		}
		pw := ctx.world.AttemptByID[predID]
		if pw == nil || !pw.RequestLinked {
			r.addGap(fmt.Sprintf("predecessor attempt %s of retry at effect_seq %d is not linked to a request", predID, ev.EffectSeq))
			continue
		}
		if pw.Request != reqID {
			f := findingFrom(*ev, "D-retry-causality", "RetryScheduled for request %s names predecessor %s belonging to request %s", reqID, predID, pw.Request)
			f.Expected = reqID
			f.Observed = pw.Request
			r.fail(f)
		}
		if !pw.Terminal {
			f := findingFrom(*ev, "D-retry-causality", "predecessor attempt %s for retry of %s was not in a terminal state", predID, reqID)
			f.Expected = "predecessor terminal"
			f.Observed = pw.State
			r.fail(f)
		}
		if pw.State != "failed" && pw.State != "timed_out" && pw.State != "timed-out" {
			f := findingFrom(*ev, "D-retry-causality", "predecessor attempt %s for retry of %s terminated with unexpected state %s", predID, reqID, pw.State)
			f.Expected = "failed or timed_out"
			f.Observed = pw.State
			r.fail(f)
		}

		// At the same instant, there must be a request-transition to retrying and a budget consumed effect
		hasRetrying := false
		hasBudgetConsumed := false
		for i := range ctx.ledger.Blocks {
			otherB := &ctx.ledger.Blocks[i]
			if otherB.TimeNS == b.TimeNS {
				for _, n := range otherB.Notes {
					if n.Kind == "request-transition" && n.Detail.Request == reqID && n.Detail.To == "retrying" {
						hasRetrying = true
					}
					if n.Kind == "budget" && n.Detail.Decision == "consumed" && (n.Detail.Request == "" || n.Detail.Request == reqID) {
						hasBudgetConsumed = true
					}
				}
			}
		}
		if ctx.ledger.headComplete() {
			if !hasRetrying {
				f := findingFrom(*ev, "D-retry-causality", "RetryScheduled for %s at %d has no corresponding request-transition to retrying", reqID, b.TimeNS)
				r.fail(f)
			}
			if !hasBudgetConsumed {
				f := findingFrom(*ev, "D-retry-causality", "RetryScheduled for %s at %d has no corresponding budget consumed effect", reqID, b.TimeNS)
				r.fail(f)
			}
		}
	}
	return *r
}

// checkRetryBackoff verifies that every RetryScheduled event is paired with a
// RetryExecution event after exactly RetryBackoffNS, that no two schedules occur
// consecutively without an execution, and that executions have preceding schedules.
func checkRetryBackoff(ctx *evalContext) PropertyResult {
	r := newCheck("D-retry-backoff", "retry executions follow retry scheduling by exactly the declared backoff")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; retry backoff cannot be established")
		return *r
	}
	backoff := ctx.input.RetryBackoffNS
	terminalComplete := ctx.ledger.terminalComplete()
	headComplete := ctx.ledger.headComplete()

	type eventRec struct {
		kind      string
		simTimeNS int64
		record    Record
	}
	byRequest := make(map[string][]eventRec)

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil {
			continue
		}
		if b.Event.Kind == "RetryScheduled" || b.Event.Kind == "RetryExecution" {
			reqID := b.Event.Detail.Request
			if reqID == "" {
				r.addGap(fmt.Sprintf("%s event at effect_seq %d names no request", b.Event.Kind, b.Event.EffectSeq))
				continue
			}
			byRequest[reqID] = append(byRequest[reqID], eventRec{
				kind:      b.Event.Kind,
				simTimeNS: b.TimeNS,
				record:    *b.Event,
			})
		}
	}

	for reqID, evs := range byRequest {
		var lastSched *eventRec
		for i := range evs {
			ev := &evs[i]
			if ev.kind == "RetryScheduled" {
				if lastSched != nil {
					f := findingFrom(ev.record, "D-retry-backoff", "consecutive RetryScheduled events for request %s at %d and %d without an intervening RetryExecution", reqID, lastSched.simTimeNS, ev.simTimeNS)
					r.fail(f)
				}
				lastSched = ev
			} else if ev.kind == "RetryExecution" {
				if lastSched == nil {
					if headComplete {
						f := findingFrom(ev.record, "D-retry-backoff", "RetryExecution for request %s at %d has no preceding RetryScheduled event", reqID, ev.simTimeNS)
						r.fail(f)
					}
				} else {
					wantTime := lastSched.simTimeNS + backoff
					if ev.simTimeNS != wantTime {
						f := findingFrom(ev.record, "D-retry-backoff", "RetryExecution for request %s at %d does not match scheduled time %d + backoff %d = %d", reqID, ev.simTimeNS, lastSched.simTimeNS, backoff, wantTime)
						f.Expected = fmt.Sprintf("time=%d", wantTime)
						f.Observed = fmt.Sprintf("time=%d", ev.simTimeNS)
						r.fail(f)
					}
					lastSched = nil
				}
			}
		}
		if lastSched != nil && terminalComplete {
			wantTime := lastSched.simTimeNS + backoff
			if wantTime <= ctx.input.HorizonNS {
				f := findingFrom(lastSched.record, "D-retry-backoff", "RetryScheduled for request %s at %d had no RetryExecution at %d before horizon %d", reqID, lastSched.simTimeNS, wantTime, ctx.input.HorizonNS)
				r.fail(f)
			}
		}
	}
	return *r
}

// checkRetryLimit verifies that no request exceeds its declared retry limit.
func checkRetryLimit(ctx *evalContext) PropertyResult {
	r := newCheck("D-retry-limit", "retry counts do not exceed declared request retry limits")
	reqRetries := make(map[string]int)
	maxRetriesByReq := make(map[string]*int)

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event != nil && b.Event.Kind == "RetryScheduled" {
			reqID := b.Event.Detail.Request
			if reqID != "" {
				reqRetries[reqID]++
			}
		}
		for _, n := range blockNotes(b, "request-created") {
			if n.Detail.Request != "" && n.Detail.MaxRetries != nil {
				maxRetriesByReq[n.Detail.Request] = n.Detail.MaxRetries
			}
		}
	}

	for _, rw := range ctx.world.Requests {
		limit := -1
		if p, ok := maxRetriesByReq[rw.ID]; ok && p != nil {
			limit = *p
		} else {
			// Check if declared in input workload
			if ctx.input != nil && ctx.input.Workload != nil {
				for _, c := range ctx.input.Workload.Components {
					if c.Tenant == rw.Tenant && c.Upstream == rw.Upstream {
						limit = c.MaxRetries
						break
					}
				}
			}
		}
		count := reqRetries[rw.ID]
		if limit < 0 {
			if !ctx.ledger.headComplete() && count == 0 {
				continue
			}
			r.addGap(fmt.Sprintf("request %s has no declared max_retries", rw.ID))
			continue
		}
		if count > limit {
			f := Finding{
				Code:     "D-retry-limit",
				Message:  fmt.Sprintf("request %s scheduled %d retries against a declared limit of %d", rw.ID, count, limit),
				Request:  rw.ID,
				Tenant:   rw.Tenant,
				Upstream: rw.Upstream,
				Expected: fmt.Sprintf("retries<=%d", limit),
				Observed: fmt.Sprintf("retries=%d", count),
			}
			r.fail(f)
		}
	}
	return *r
}

// checkRetryBlocked verifies that blocked retries are cleanly rejected with a
// documented reason, fail their request, and pair with a preceding schedule.
func checkRetryBlocked(ctx *evalContext) PropertyResult {
	r := newCheck("D-retry-blocked", "blocked retries cleanly terminate with documented reasons")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; retry backoff cannot be established")
		return *r
	}
	backoff := ctx.input.RetryBackoffNS
	headComplete := ctx.ledger.headComplete()

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "retry-admission") {
			if n.Detail.Decision != "blocked" {
				continue
			}
			reqID := n.Detail.Request
			if reqID == "" {
				r.addGap(fmt.Sprintf("blocked retry effect at effect_seq %d names no request", n.EffectSeq))
				continue
			}
			if !documentedRetryBlocks[n.Detail.Reason] {
				f := findingFrom(n, "D-retry-blocked", "blocked retry for %s uses undocumented reason %q", reqID, n.Detail.Reason)
				r.fail(f)
			}
			if b.Event == nil || b.Event.Kind != "RetryExecution" {
				f := findingFrom(n, "D-retry-blocked", "blocked retry for %s was emitted under %s, not RetryExecution", reqID, b.Event.Kind)
				r.fail(f)
			}
			// Verify pairing with preceding schedule at b.TimeNS - backoff
			if headComplete {
				wantTime := b.TimeNS - backoff
				foundSched := false
				for i := range ctx.ledger.Blocks {
					prevB := &ctx.ledger.Blocks[i]
					if prevB.TimeNS == wantTime && prevB.Event != nil && prevB.Event.Kind == "RetryScheduled" && prevB.Event.Detail.Request == reqID {
						foundSched = true
						break
					}
				}
				if !foundSched && wantTime >= 0 {
					f := findingFrom(n, "D-retry-blocked", "blocked retry for %s at %d has no preceding RetryScheduled at %d", reqID, b.TimeNS, wantTime)
					r.fail(f)
				}
			}
			// Request failed in same block
			failed := false
			for _, tn := range blockNotes(b, "request-transition") {
				if tn.Detail.Request == reqID && tn.Detail.To == "failed" {
					failed = true
				}
			}
			if !failed {
				f := findingFrom(n, "D-retry-blocked", "blocked retry for %s did not fail the request in the same block", reqID)
				r.fail(f)
			}
			// No attempt created
			if created := blockNotes(b, "attempt-created"); len(created) > 0 {
				f := findingFrom(created[0], "D-retry-blocked", "blocked retry for %s created attempt %s", reqID, created[0].Detail.Attempt)
				r.fail(f)
			}
		}
	}
	return *r
}

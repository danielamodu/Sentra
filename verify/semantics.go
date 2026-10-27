package verify

import "fmt"

// This file implements category F: event/effect semantics. A ledger block is one
// engine step (the event record plus the effects the handler applied while
// processing it). These properties check that the effects are attributable to
// the event that caused them, that the state a handler applies is one the
// documented semantics allow for that event, that creation effects carry the
// admission lane they claim, and that a finalized attempt is never resurrected.
//
// Everything here reads typed record fields. Prose descriptions are never
// parsed: a note without the typed fact simply cannot support the claim, and the
// affected property reports a gap instead of guessing.

// attemptScopedEvents are the event kinds that name exactly one attempt. Their
// blocks may still touch sibling attempts of the same logical request (a
// completion that exhausts the retry limit retracts them), so attribution is
// checked per request, not per attempt.
var attemptScopedEvents = map[string]bool{
	"QueueInsertion":      true,
	"QueueRemoval":        true,
	"WorkerAssignment":    true,
	"ExecutionStart":      true,
	"ExecutionCompletion": true,
	"Timeout":             true,
}

// eventAttemptStateEffects is the documented state each single-attempt event may
// apply. "retracted" appears where the handler may govern sibling attempts of
// the same request through the single retraction exit path, or where an attempt
// is retracted on entry to queue insertion because its request terminated.
var eventAttemptStateEffects = map[string]map[string]bool{
	"QueueInsertion":        {"queued": true, "retracted": true},
	"QueueRemoval":          {"dispatching": true},
	"WorkerAssignment":      {"assigned": true},
	"ExecutionStart":        {"running": true, "waiting_connection": true, "waiting-connection": true},
	"ExecutionCompletion":   {"completed": true, "failed": true, "retracted": true},
	"Timeout":               {"timed_out": true, "timed-out": true, "retracted": true},
	"DeadlineExpired":       {"retracted": true},
	"CancellationRequested": {"retracted": true},
	"ConnectionGranted":     {"running": true},
}

// blockNotes returns the typed effects of kind in evidence order.
func blockNotes(b *Block, kind string) []Record {
	var out []Record
	for _, n := range b.Notes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// findingFrom builds a finding anchored at one record.
func findingFrom(rec Record, code, format string, args ...any) Finding {
	return Finding{
		Code:           code,
		Message:        fmt.Sprintf(format, args...),
		Classification: ClassUnclassified,
		EffectSeq:      rec.EffectSeq,
		EngineSeq:      rec.EngineSeq,
		Request:        rec.Detail.Request,
		Attempt:        rec.Detail.Attempt,
		Tenant:         rec.Detail.Tenant,
		Upstream:       rec.Detail.Upstream,
		Worker:         rec.Detail.Worker,
	}
}

// checkEffectAttribution verifies that an effect written while handling an
// attempt-scoped event belongs to that attempt or to a sibling attempt of the
// same logical request. An effect attributed to an unrelated request means the
// handler reached outside the scope of the event it was processing.
func checkEffectAttribution(ctx *evalContext) PropertyResult {
	r := newCheck("F-effect-attribution", "effects are attributable to the event that caused them")
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil {
			continue
		}
		if attemptScopedEvents[b.Event.Kind] {
			primary := b.Event.Detail.Attempt
			if primary == "" {
				r.addGap(fmt.Sprintf("%s event at effect_seq %d names no attempt; effects cannot be attributed", b.Event.Kind, b.Event.EffectSeq))
				continue
			}
			pw := ctx.world.AttemptByID[primary]
			var primaryRequest string
			if pw != nil {
				primaryRequest = pw.Request
			}
			for _, n := range b.Notes {
				if n.Detail.Attempt == "" || n.Detail.Attempt == primary {
					continue
				}
				aw := ctx.world.AttemptByID[n.Detail.Attempt]
				if aw == nil || !aw.RequestLinked || primaryRequest == "" {
					r.addGap(fmt.Sprintf("effect at effect_seq %d names attempt %s whose parent request is not establishable; attribution cannot be checked", n.EffectSeq, n.Detail.Attempt))
					continue
				}
				if aw.Request != primaryRequest {
					f := findingFrom(n, "F-effect-attribution", "effect %s names attempt %s of request %s, but the event acted on attempt %s of request %s", n.Kind, n.Detail.Attempt, aw.Request, primary, primaryRequest)
					f.Expected = "effect within the event's request"
					f.Observed = n.Detail.Attempt
					r.fail(f)
				}
			}
		}

		// Request identity agreement between request-scoped events and their notes
		if b.Event.Detail.Request != "" {
			reqID := b.Event.Detail.Request
			for _, n := range b.Notes {
				if n.Detail.Request != "" && n.Detail.Request != reqID {
					f := findingFrom(n, "F-effect-attribution", "effect %s names request %s, but event %s acted on request %s", n.Kind, n.Detail.Request, b.Event.Kind, reqID)
					f.Expected = reqID
					f.Observed = n.Detail.Request
					r.fail(f)
				}
			}
		}
	}
	return *r
}

// checkAttemptStateEffects verifies that every attempt transition applied inside
// a block is a state the documented semantics allow the triggering event to
// produce. It complements L-attempt-lifecycle (contiguity) with event causality.
func checkAttemptStateEffects(ctx *evalContext) PropertyResult {
	r := newCheck("F-attempt-state-effects", "attempt transitions match the state their triggering event may produce")
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil {
			continue
		}
		allowed, known := eventAttemptStateEffects[b.Event.Kind]
		if !known {
			allowed = map[string]bool{}
		}
		for _, n := range blockNotes(b, "attempt-transition") {
			if n.Detail.To == "" {
				r.addGap(fmt.Sprintf("attempt transition at effect_seq %d carries no target state; the effect cannot be interpreted", n.EffectSeq))
				continue
			}
			if !allowed[n.Detail.To] {
				f := findingFrom(n, "F-attempt-state-effects", "%s applied attempt state %q to %s, which its documented semantics do not allow", b.Event.Kind, n.Detail.To, n.Detail.Attempt)
				f.Expected = "a documented target state"
				f.Observed = n.Detail.To
				r.fail(f)
			}
		}
	}
	return *r
}

// checkCreationLane verifies the admission lane of every creation effect: an
// arrival-lane attempt is created in a RequestArrival event, a retry-lane attempt
// in a RetryExecution event, and each such block creates at most one attempt.
func checkCreationLane(ctx *evalContext) PropertyResult {
	r := newCheck("F-creation-lane", "attempt creation effects carry the admission lane their event establishes")
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		created := blockNotes(b, "attempt-created")
		if len(created) == 0 {
			continue
		}
		if b.Event == nil {
			r.addGap(fmt.Sprintf("creation effect at effect_seq %d has no retained triggering event; its lane cannot be attributed", created[0].EffectSeq))
			continue
		}
		if len(created) > 1 {
			r.fail(findingFrom(created[1], "F-creation-lane", "%s block creates %d attempts; one admission decision creates at most one", b.Event.Kind, len(created)))
		}
		for _, n := range created {
			wantLane := ""
			switch b.Event.Kind {
			case "RequestArrival":
				wantLane = "arrival"
			case "RetryExecution":
				wantLane = "retry"
			default:
				f := findingFrom(n, "F-creation-lane", "attempt %s was created while handling %s, which creates no attempt", n.Detail.Attempt, b.Event.Kind)
				f.Expected = "RequestArrival or RetryExecution"
				f.Observed = b.Event.Kind
				r.fail(f)
				continue
			}
			if n.Detail.Lane == "" {
				r.addGap(fmt.Sprintf("attempt %s creation carries no typed lane", n.Detail.Attempt))
				continue
			}
			if n.Detail.Lane != wantLane {
				f := findingFrom(n, "F-creation-lane", "attempt %s claims lane %q but %s establishes lane %q", n.Detail.Attempt, n.Detail.Lane, b.Event.Kind, wantLane)
				f.Expected = "lane=" + wantLane
				f.Observed = "lane=" + n.Detail.Lane
				r.fail(f)
			}
			// Identity agreement between the creation effect and its event.
			for _, pair := range [][2]string{
				{n.Detail.Tenant, b.Event.Detail.Tenant},
				{n.Detail.Upstream, b.Event.Detail.Upstream},
				{n.Detail.Request, b.Event.Detail.Request},
			} {
				if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
					f := findingFrom(n, "F-creation-lane", "attempt %s creation says %q but its event says %q", n.Detail.Attempt, pair[0], pair[1])
					f.Expected = pair[1]
					f.Observed = pair[0]
					r.fail(f)
				}
			}
		}
	}
	return *r
}

// checkTerminalEffectClosure verifies that a finalized attempt is never
// resurrected: after its terminal transition no later effect may create it
// again or grant it a queue, worker or connection, or reserve a probe permit.
func checkTerminalEffectClosure(ctx *evalContext) PropertyResult {
	r := newCheck("F-terminal-effect-closure", "a finalized attempt is never resurrected by a later effect")
	terminalAt := make(map[string]Record, len(ctx.world.Attempts))
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range b.Notes {
			if n.Detail.Attempt == "" {
				continue
			}
			if n.Kind == "attempt-created" {
				if prev, ok := terminalAt[n.Detail.Attempt]; ok {
					f := findingFrom(n, "F-terminal-effect-closure", "attempt %s is created again after it finalized at effect_seq %d", n.Detail.Attempt, prev.EffectSeq)
					f.Expected = "no creation after finalization"
					f.Observed = "second creation"
					r.fail(f)
				}
				continue
			}
			if prev, ok := terminalAt[n.Detail.Attempt]; ok && (grantsResource(n) || n.Kind == "probe-reserved") {
				f := findingFrom(n, "F-terminal-effect-closure", "attempt %s is granted a resource or probe permit by effect %s after it finalized at effect_seq %d", n.Detail.Attempt, n.Kind, prev.EffectSeq)
				f.Expected = "no resource grant or probe reservation after finalization"
				f.Observed = n.Kind
				r.fail(f)
			}
			if n.Kind == "attempt-transition" && n.Detail.To != "" && isTerminalAttemptState(n.Detail.To) {
				if _, seen := terminalAt[n.Detail.Attempt]; !seen {
					terminalAt[n.Detail.Attempt] = n
				}
			}
		}
	}
	return *r
}

// grantsResource reports whether a typed effect grants a runtime resource.
func grantsResource(n Record) bool {
	switch n.Kind {
	case "queue":
		return n.Detail.Decision == "append"
	case "worker":
		return n.Detail.Decision == "acquired"
	case "connection":
		return n.Detail.Decision == "acquired" || n.Detail.Decision == "granted"
	}
	return false
}

func isTerminalAttemptState(s string) bool {
	switch s {
	case "completed", "failed", "timed_out", "timed-out", "retracted":
		return true
	}
	return false
}

// documentedRetryBlocks is the closed set of retry admission rejection reasons.
var documentedRetryBlocks = map[string]bool{
	"retry-blocked-breaker-open":     true,
	"retry-blocked-breaker-halfopen": true,
	"retry-queue-full":               true,
	"retry-queue-total-full":         true,
}

// checkRetryAdmissionContext verifies that a blocked retry is an effect of a
// RetryExecution event for the same request, uses a documented reason, fails the
// request in the same block and creates no attempt. It also validates in reverse
// that a RetryExecution that fails the request emitted a blocked retry note.
func checkRetryAdmissionContext(ctx *evalContext) PropertyResult {
	r := newCheck("F-retry-admission-context", "blocked retries are effects of the retry admission decision they report")
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "retry-admission") {
			if n.Detail.Decision != "blocked" {
				r.addGap(fmt.Sprintf("retry-admission effect at effect_seq %d carries decision %q; only a block is interpretable", n.EffectSeq, n.Detail.Decision))
				continue
			}
			if b.Event == nil || b.Event.Kind != "RetryExecution" {
				kind := "<none>"
				if b.Event != nil {
					kind = b.Event.Kind
				}
				r.fail(findingFrom(n, "F-retry-admission-context", "retry rejection for %s was written while handling %s, not a retry execution", n.Detail.Request, kind))
				continue
			}
			if b.Event.Detail.Request != "" && n.Detail.Request != b.Event.Detail.Request {
				f := findingFrom(n, "F-retry-admission-context", "retry rejection names request %s but its event names %s", n.Detail.Request, b.Event.Detail.Request)
				f.Expected = b.Event.Detail.Request
				f.Observed = n.Detail.Request
				r.fail(f)
			}
			if !documentedRetryBlocks[n.Detail.Reason] {
				f := findingFrom(n, "F-retry-admission-context", "retry rejection for %s uses undocumented reason %q", n.Detail.Request, n.Detail.Reason)
				f.Expected = "a documented retry block reason"
				f.Observed = n.Detail.Reason
				r.fail(f)
			}
			failed := false
			for _, tn := range blockNotes(b, "request-transition") {
				if tn.Detail.Request == n.Detail.Request && tn.Detail.To == "failed" {
					failed = true
				}
			}
			if !failed {
				r.fail(findingFrom(n, "F-retry-admission-context", "retry rejection for %s did not fail the request in the same block", n.Detail.Request))
			}
			if created := blockNotes(b, "attempt-created"); len(created) > 0 {
				r.fail(findingFrom(created[0], "F-retry-admission-context", "retry rejection for %s also created attempt %s", n.Detail.Request, created[0].Detail.Attempt))
			}
		}

		// Reverse direction: RetryExecution that fails the request must emit a blocked retry note
		if b.Event != nil && b.Event.Kind == "RetryExecution" && b.Event.Detail.Request != "" {
			reqID := b.Event.Detail.Request
			created := blockNotes(b, "attempt-created")
			retryNotes := blockNotes(b, "retry-admission")
			if len(created) > 0 && len(retryNotes) > 0 {
				r.fail(findingFrom(retryNotes[0], "F-retry-admission-context", "RetryExecution for %s created attempt %s and also rejected retry", reqID, created[0].Detail.Attempt))
			}
			failed := false
			for _, tn := range blockNotes(b, "request-transition") {
				if tn.Detail.Request == reqID && tn.Detail.To == "failed" {
					failed = true
				}
			}
			if failed && len(retryNotes) == 0 {
				r.fail(findingFrom(*b.Event, "F-retry-admission-context", "RetryExecution failed request %s without emitting a retry-admission blocked note", reqID))
			}
		}
	}
	return *r
}

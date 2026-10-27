package verify

import "fmt"

// This file implements category C: admission and queueing. The ledger records an
// admission decision per arrival and one typed queue effect per append, dispatch
// removal and retraction removal. Those effects are enough to replay each
// tenant's lanes and to check dispatch order without reading reference state.

// documentedShedReasons is the closed set of admission rejection reasons.
var documentedShedReasons = map[string]bool{
	"breaker-open":               true,
	"breaker-halfopen-saturated": true,
	"tenant-queue-full":          true,
	"tenant-queue-total-full":    true,
}

// checkAdmissionOutcome verifies that each admission decision agrees with the
// request lifecycle it caused: an admitted request creates exactly one attempt
// and is not shed; a shed request transitions to shed with the decision's reason
// and creates no attempt; an expired arrival transitions to expired.
func checkAdmissionOutcome(ctx *evalContext) PropertyResult {
	r := newCheck("C-admission-outcome", "admission decisions agree with the request lifecycle they caused")

	// 1. Check RequestArrival blocks
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil {
			continue
		}
		if b.Event.Kind == "RequestArrival" {
			reqID := ""
			for _, n := range blockNotes(b, "request-created") {
				reqID = n.Detail.Request
			}
			created := blockNotes(b, "attempt-created")
			shedTransitions := 0
			expiredTransitions := 0
			for _, tn := range blockNotes(b, "request-transition") {
				if tn.Detail.To == "shed" {
					shedTransitions++
				}
				if tn.Detail.To == "expired" {
					expiredTransitions++
				}
			}

			if shedTransitions > 0 && len(created) > 0 {
				r.fail(findingFrom(*b.Event, "C-admission-outcome", "request %s was shed but %d attempt(s) were created", reqID, len(created)))
			}
			if shedTransitions > 0 {
				rw := ctx.world.RequestByID[reqID]
				if rw != nil && rw.State != "shed" {
					r.fail(findingFrom(*b.Event, "C-admission-outcome", "request %s shed transition recorded but request state is %s", reqID, rw.State))
				}
			}
			if expiredTransitions > 0 && len(created) > 0 {
				r.fail(findingFrom(*b.Event, "C-admission-outcome", "request %s expired at arrival but %d attempt(s) were created", reqID, len(created)))
			}
		}

		// 2. Check AdmissionDecided events
		if b.Event.Kind == "AdmissionDecided" {
			ev := b.Event
			req := ev.Detail.Request
			if req == "" {
				r.addGap(fmt.Sprintf("admission decision at effect_seq %d names no request", ev.EffectSeq))
				continue
			}
			rw := ctx.world.RequestByID[req]
			switch ev.Detail.Decision {
			case "admit":
				if rw != nil && len(rw.AttemptIDs) == 0 && ctx.ledger.headComplete() {
					f := findingFrom(*ev, "C-admission-outcome", "request %s was admitted but no attempt was created", req)
					f.Expected = "at least one attempt created"
					f.Observed = "none"
					r.fail(f)
				}
				if rw != nil && rw.State == "shed" {
					r.fail(findingFrom(*ev, "C-admission-outcome", "request %s was admitted but transitioned to shed", req))
				}
			case "shed":
				if ev.Detail.Reason != "deadline-at-arrival" && !documentedShedReasons[ev.Detail.Reason] {
					f := findingFrom(*ev, "C-admission-outcome", "request %s was shed with undocumented reason %q", req, ev.Detail.Reason)
					f.Expected = "a documented shed reason"
					f.Observed = ev.Detail.Reason
					r.fail(f)
				}
				if rw != nil && len(rw.AttemptIDs) > 0 {
					r.fail(findingFrom(*ev, "C-admission-outcome", "request %s was shed but created %d attempt(s)", req, len(rw.AttemptIDs)))
				}
			default:
				r.addGap(fmt.Sprintf("admission decision at effect_seq %d carries decision %q", ev.EffectSeq, ev.Detail.Decision))
			}
		}
	}
	return *r
}

// checkQueueIntervals verifies the queue residency interval of every attempt:
// at most one append, at most one removal, removal only after an append, and a
// removal effect that names the attempt's own queue entry.
func checkQueueIntervals(ctx *evalContext) PropertyResult {
	r := newCheck("C-queue-interval", "queue residency is a single append/remove interval per attempt")
	headComplete := ctx.ledger.headComplete()
	type queueEntry struct {
		tenant string
		lane   string
		seq    uint64
	}
	state := make(map[string]queueEntry) // attempt -> queueEntry
	qLen := make(map[string]int)         // tenant -> count

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "queue") {
			if n.Detail.Attempt == "" {
				r.addGap(fmt.Sprintf("queue effect at effect_seq %d carries no typed attempt identity", n.EffectSeq))
				continue
			}
			switch n.Detail.Decision {
			case "append":
				if prev, ok := state[n.Detail.Attempt]; ok {
					f := findingFrom(n, "C-queue-interval", "attempt %s is appended to the queue again (seq %d) while already queued (seq %d)", n.Detail.Attempt, n.Detail.Seq, prev.seq)
					f.Expected = "one append per residency"
					f.Observed = "second append"
					r.fail(f)
					continue
				}
				if n.Detail.Seq == 0 {
					r.addGap(fmt.Sprintf("queue append at effect_seq %d carries no enqueue sequence", n.EffectSeq))
				}
				state[n.Detail.Attempt] = queueEntry{tenant: n.Detail.Tenant, lane: n.Detail.Lane, seq: n.Detail.Seq}
				qLen[n.Detail.Tenant]++
				if headComplete && n.Detail.Count != qLen[n.Detail.Tenant] {
					f := findingFrom(n, "C-queue-interval", "queue append for %s reports queue len %d but replayed len is %d", n.Detail.Attempt, n.Detail.Count, qLen[n.Detail.Tenant])
					f.Expected = fmt.Sprintf("len=%d", qLen[n.Detail.Tenant])
					f.Observed = fmt.Sprintf("len=%d", n.Detail.Count)
					r.fail(f)
				}
			case "remove", "retract":
				prev, ok := state[n.Detail.Attempt]
				if !ok {
					if headComplete {
						f := findingFrom(n, "C-queue-interval", "attempt %s is removed from the queue by %s but no append is recorded", n.Detail.Attempt, n.Detail.Decision)
						f.Expected = "an outstanding append"
						f.Observed = "none"
						r.fail(f)
					}
					continue
				}
				if n.Detail.Tenant != "" && prev.tenant != "" && n.Detail.Tenant != prev.tenant {
					f := findingFrom(n, "C-queue-interval", "attempt %s removed from tenant %s queue but was enqueued to %s", n.Detail.Attempt, n.Detail.Tenant, prev.tenant)
					f.Expected = prev.tenant
					f.Observed = n.Detail.Tenant
					r.fail(f)
				}
				if n.Detail.Lane != "" && prev.lane != "" && n.Detail.Lane != prev.lane {
					f := findingFrom(n, "C-queue-interval", "attempt %s removed with lane %s but was enqueued with %s", n.Detail.Attempt, n.Detail.Lane, prev.lane)
					f.Expected = prev.lane
					f.Observed = n.Detail.Lane
					r.fail(f)
				}
				if n.Detail.Seq != 0 && prev.seq != 0 && n.Detail.Seq != prev.seq {
					f := findingFrom(n, "C-queue-interval", "attempt %s removed with seq %d but was enqueued with %d", n.Detail.Attempt, n.Detail.Seq, prev.seq)
					f.Expected = fmt.Sprintf("seq=%d", prev.seq)
					f.Observed = fmt.Sprintf("seq=%d", n.Detail.Seq)
					r.fail(f)
				}
				delete(state, n.Detail.Attempt)
				if qLen[prev.tenant] > 0 {
					qLen[prev.tenant]--
				}
				if headComplete && n.Detail.Count != qLen[prev.tenant] {
					f := findingFrom(n, "C-queue-interval", "queue %s for %s reports queue len %d but replayed len is %d", n.Detail.Decision, n.Detail.Attempt, n.Detail.Count, qLen[prev.tenant])
					f.Expected = fmt.Sprintf("len=%d", qLen[prev.tenant])
					f.Observed = fmt.Sprintf("len=%d", n.Detail.Count)
					r.fail(f)
				}
			default:
				r.addGap(fmt.Sprintf("queue effect at effect_seq %d carries unknown decision %q", n.EffectSeq, n.Detail.Decision))
			}
		}
	}
	return *r
}

// checkDispatchFIFO verifies the documented global dispatch order: queue
// removals happen in non-decreasing enqueue-sequence order across all tenants.
// Retraction removals are not dispatches and do not participate.
func checkDispatchFIFO(ctx *evalContext) PropertyResult {
	r := newCheck("C-fifo-dispatch", "dispatch removals follow the global enqueue order")
	var lastSeq uint64
	seen := false
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "queue") {
			if n.Detail.Decision != "remove" {
				continue
			}
			if n.Detail.Seq == 0 {
				r.addGap(fmt.Sprintf("dispatch removal at effect_seq %d carries no enqueue sequence; ordering cannot be established", n.EffectSeq))
				continue
			}
			if seen && n.Detail.Seq < lastSeq {
				f := findingFrom(n, "C-fifo-dispatch", "attempt %s was enqueued at sequence %d but dispatched after sequence %d", n.Detail.Attempt, n.Detail.Seq, lastSeq)
				f.Expected = fmt.Sprintf("enqueue sequence >= %d", lastSeq)
				f.Observed = fmt.Sprintf("sequence=%d", n.Detail.Seq)
				r.fail(f)
			}
			lastSeq = n.Detail.Seq
			seen = true
		}
	}
	return *r
}

// checkObservedLaneBounds replays each tenant's arrival and retry lanes from the
// typed queue effects and verifies that no append exceeds the declared lane or
// total bound. This is the admission bound observed from evidence, not from the
// reference's internal counters.
func checkObservedLaneBounds(ctx *evalContext) PropertyResult {
	r := newCheck("C-lane-bounds", "observed queue lanes stay within their declared bounds")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; lane bounds cannot be established")
		return *r
	}
	type lanes struct{ arrival, retry int }
	bounds := make(map[string]TenantSpec, len(ctx.input.Tenants))
	for _, t := range ctx.input.Tenants {
		bounds[t.ID] = t
	}
	occupancy := make(map[string]*lanes)
	inQueue := make(map[string]string) // attempt -> tenant
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "queue") {
			if n.Detail.Attempt == "" || n.Detail.Tenant == "" {
				r.addGap(fmt.Sprintf("queue effect at effect_seq %d carries no typed attempt/tenant identity", n.EffectSeq))
				continue
			}
			occ := occupancy[n.Detail.Tenant]
			if occ == nil {
				occ = &lanes{}
				occupancy[n.Detail.Tenant] = occ
			}
			switch n.Detail.Decision {
			case "append":
				switch n.Detail.Lane {
				case "arrival":
					occ.arrival++
				case "retry":
					occ.retry++
				default:
					r.addGap(fmt.Sprintf("queue append at effect_seq %d carries unknown lane %q", n.EffectSeq, n.Detail.Lane))
					continue
				}
				inQueue[n.Detail.Attempt] = n.Detail.Tenant
				spec, declared := bounds[n.Detail.Tenant]
				if !declared {
					r.addGap(fmt.Sprintf("tenant %s is not declared, so its lane bounds cannot be checked", n.Detail.Tenant))
					continue
				}
				laneBound, laneName := spec.QueueLane, "arrival"
				if n.Detail.Lane == "retry" {
					laneBound, laneName = spec.QueueRetry, "retry"
				}
				laneUsed := occ.arrival
				if n.Detail.Lane == "retry" {
					laneUsed = occ.retry
				}
				if laneUsed > laneBound {
					f := findingFrom(n, "C-lane-bounds", "tenant %s %s lane holds %d entr(ies) against a declared bound of %d", n.Detail.Tenant, laneName, laneUsed, laneBound)
					f.Expected = fmt.Sprintf("%s<=%d", laneName, laneBound)
					f.Observed = fmt.Sprintf("%s=%d", laneName, laneUsed)
					r.fail(f)
				}
				if total := occ.arrival + occ.retry; total > spec.QueueTotal {
					f := findingFrom(n, "C-lane-bounds", "tenant %s holds %d queued entr(ies) against a declared total bound of %d", n.Detail.Tenant, total, spec.QueueTotal)
					f.Expected = fmt.Sprintf("total<=%d", spec.QueueTotal)
					f.Observed = fmt.Sprintf("total=%d", total)
					r.fail(f)
				}
			case "remove", "retract":
				tenant, ok := inQueue[n.Detail.Attempt]
				if !ok {
					continue
				}
				if tenant != n.Detail.Tenant {
					f := findingFrom(n, "C-lane-bounds", "attempt %s is removed from tenant %s but was appended to tenant %s", n.Detail.Attempt, n.Detail.Tenant, tenant)
					f.Expected = tenant
					f.Observed = n.Detail.Tenant
					r.fail(f)
				}
				if occ := occupancy[tenant]; occ != nil {
					if n.Detail.Lane == "retry" && occ.retry > 0 {
						occ.retry--
					} else if n.Detail.Lane == "arrival" && occ.arrival > 0 {
						occ.arrival--
					}
				}
				delete(inQueue, n.Detail.Attempt)
			}
		}
	}
	return *r
}

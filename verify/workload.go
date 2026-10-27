package verify

import "fmt"

// This file implements category H: workload and fault reconstruction. Observed
// arrival envelopes are compared against declared workload components and bursts,
// total volume is audited against fractional rate/carry schedules, and fault
// activation and recovery intervals are cross-checked with the declared schedule.

// checkArrivalEnvelope verifies that every observed request arrival falls within
// a declared component or burst envelope for its tenant and upstream.
func checkArrivalEnvelope(ctx *evalContext) PropertyResult {
	r := newCheck("H-arrival-envelope", "observed request arrivals conform to declared workload envelopes")
	if ctx.input == nil || ctx.input.Workload == nil {
		r.addGap("workload declaration is absent; arrival envelope cannot be established")
		return *r
	}
	w := ctx.input.Workload

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "request-created") {
			reqID := n.Detail.Request
			tenant := n.Detail.Tenant
			upstream := n.Detail.Upstream
			arrTime := b.TimeNS

			if n.Detail.DeadlineNS == 0 {
				r.addGap(fmt.Sprintf("request %s carries no deadline", reqID))
				continue
			}
			if n.Detail.MaxRetries == nil {
				r.addGap(fmt.Sprintf("request %s carries no max_retries", reqID))
				continue
			}
			relDeadline := n.Detail.DeadlineNS - arrTime
			maxRetries := *n.Detail.MaxRetries

			matched := false
			for _, c := range w.Components {
				if c.Tenant != tenant || c.Upstream != upstream {
					continue
				}
				until := c.UntilNS
				if until == 0 {
					until = ctx.input.HorizonNS
				}
				if arrTime >= c.FromNS && arrTime < until {
					if relDeadline >= c.DeadlineMinNS && relDeadline <= c.DeadlineMaxNS && maxRetries == c.MaxRetries {
						matched = true
						break
					}
				}
			}
			if !matched {
				for _, bst := range w.Bursts {
					if bst.Tenant != tenant || bst.Upstream != upstream {
						continue
					}
					spread := bst.SpreadNS
					inBurstTime := false
					if spread == 0 {
						inBurstTime = (arrTime == bst.AtNS)
					} else {
						inBurstTime = (arrTime >= bst.AtNS && arrTime < bst.AtNS+spread)
					}
					if inBurstTime {
						if relDeadline >= bst.DeadlineMinNS && relDeadline <= bst.DeadlineMaxNS && maxRetries == bst.MaxRetries {
							matched = true
							break
						}
					}
				}
			}
			if !matched {
				f := findingFrom(n, "H-arrival-envelope", "request %s arrival at %d (deadline %d, retries %d) matches no declared workload envelope for (%s, %s)", reqID, arrTime, relDeadline, maxRetries, tenant, upstream)
				f.Expected = "matching declared envelope"
				f.Observed = fmt.Sprintf("arrTime=%d deadline=%d retries=%d", arrTime, relDeadline, maxRetries)
				r.fail(f)
			}
		}
	}
	return *r
}

// checkBurstMaterialization verifies that total observed arrivals match the
// volume declared across all workload components and bursts.
func checkBurstMaterialization(ctx *evalContext) PropertyResult {
	r := newCheck("H-burst-materialization", "observed arrivals match declared workload volume")
	if ctx.input == nil || ctx.input.Workload == nil {
		r.addGap("workload declaration is absent; workload volume cannot be established")
		return *r
	}
	w := ctx.input.Workload

	type tuKey struct{ tenant, upstream string }
	expectedCounts := make(map[tuKey]int)
	for _, c := range w.Components {
		key := tuKey{c.Tenant, c.Upstream}
		until := c.UntilNS
		if until == 0 {
			until = ctx.input.HorizonNS
		}
		carry := 0.0
		st := c.FromNS
		for c.Rate > 0 && st < until {
			x := c.Rate + carry
			n := int(x)
			carry = x - float64(n)
			expectedCounts[key] += n
			st += c.WindowNS
		}
	}
	for _, b := range w.Bursts {
		key := tuKey{b.Tenant, b.Upstream}
		expectedCounts[key] += b.Count
	}

	observedCounts := make(map[tuKey]int)
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "request-created") {
			key := tuKey{n.Detail.Tenant, n.Detail.Upstream}
			observedCounts[key]++
		}
	}

	for key, exp := range expectedCounts {
		got := observedCounts[key]
		if got != exp {
			f := Finding{
				Code:     "H-burst-materialization",
				Message:  fmt.Sprintf("tenant %s upstream %s observed %d arrivals, want %d from declared workload", key.tenant, key.upstream, got, exp),
				Tenant:   key.tenant,
				Upstream: key.upstream,
				Expected: fmt.Sprintf("arrivals=%d", exp),
				Observed: fmt.Sprintf("arrivals=%d", got),
			}
			r.fail(f)
		}
	}
	return *r
}

// checkFaultIntervals verifies that upstream fault events match declared fault
// episodes and that all declared fault events were enacted.
func checkFaultIntervals(ctx *evalContext) PropertyResult {
	r := newCheck("H-fault-intervals", "fault activation and recovery follow declared fault schedules")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; fault intervals cannot be established")
		return *r
	}
	var fs FaultSchedule
	if ctx.input.Faults != nil {
		fs = *ctx.input.Faults
	}
	horizon := ctx.input.HorizonNS
	headComplete := ctx.ledger.headComplete()

	type expectedFaultEvent struct {
		simTime  int64
		kind     string
		upstream string
	}
	var expected []expectedFaultEvent
	for _, e := range fs {
		switch e.Kind {
		case FaultUnavailable:
			expected = append(expected, expectedFaultEvent{simTime: e.FromNS, kind: "UpstreamDegraded", upstream: e.Upstream})
			if e.UntilNS > 0 && e.UntilNS <= horizon {
				expected = append(expected, expectedFaultEvent{simTime: e.UntilNS, kind: "UpstreamRecovered", upstream: e.Upstream})
			}
		case FaultSlowLatency:
			expected = append(expected, expectedFaultEvent{simTime: e.FromNS, kind: "UpstreamLatencySet", upstream: e.Upstream})
			if e.UntilNS > 0 && e.UntilNS <= horizon {
				expected = append(expected, expectedFaultEvent{simTime: e.UntilNS, kind: "UpstreamLatencySet", upstream: e.Upstream})
			}
		case FaultFailRate:
			expected = append(expected, expectedFaultEvent{simTime: e.FromNS, kind: "UpstreamFailureSet", upstream: e.Upstream})
			if e.UntilNS > 0 && e.UntilNS <= horizon {
				expected = append(expected, expectedFaultEvent{simTime: e.UntilNS, kind: "UpstreamFailureSet", upstream: e.Upstream})
			}
		}
	}

	observedFaultEvents := make(map[string]bool)
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		if b.Event == nil {
			continue
		}
		k := b.Event.Kind
		if k == "UpstreamDegraded" || k == "UpstreamRecovered" || k == "UpstreamLatencySet" || k == "UpstreamFailureSet" {
			up := b.Event.Detail.Upstream
			t := b.TimeNS
			matched := false
			for _, exp := range expected {
				if exp.kind == k && exp.upstream == up && exp.simTime == t {
					matched = true
					break
				}
			}
			if !matched {
				f := findingFrom(*b.Event, "H-fault-intervals", "observed undeclared fault event %s on %s at %d", k, up, t)
				r.fail(f)
			}
			observedFaultEvents[fmt.Sprintf("%s/%s/%d", k, up, t)] = true
		}
	}

	if headComplete {
		for _, exp := range expected {
			key := fmt.Sprintf("%s/%s/%d", exp.kind, exp.upstream, exp.simTime)
			if !observedFaultEvents[key] {
				f := Finding{
					Code:     "H-fault-intervals",
					Message:  fmt.Sprintf("declared fault event %s on %s at %d missing from ledger", exp.kind, exp.upstream, exp.simTime),
					Upstream: exp.upstream,
					Expected: fmt.Sprintf("%s at %d", exp.kind, exp.simTime),
					Observed: "none",
				}
				r.fail(f)
			}
		}
	}
	return *r
}

// checkSeededReplay reports that independent seeded PRNG reconstruction cannot
// be established, providing an explicit gap rather than an unverified PASS.
func checkSeededReplay(ctx *evalContext) PropertyResult {
	r := newCheck("H-seeded-replay", "arrival sequence matches seeded PRNG stream")
	r.addGap("independent seeded PRNG replay is not implemented; arrival sequence cannot be reconstructed")
	return *r
}

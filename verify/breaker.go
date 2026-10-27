package verify

import "fmt"

// This file implements category E: circuit breaker semantics. Upstream breakers
// open when consecutive failures reach the configured threshold, remain open
// throughout the cooldown duration, transition cleanly to half-open, and admit
// strictly bounded probe traffic until health is re-established or failure recurs.

// checkBreakerOpenThreshold verifies that a circuit breaker transitions from
// closed to open only when consecutive failures reach the configured threshold.
func checkBreakerOpenThreshold(ctx *evalContext) PropertyResult {
	r := newCheck("E-breaker-open-threshold", "circuit breaker opens when consecutive failures reach threshold")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; breaker threshold cannot be established")
		return *r
	}
	threshold := ctx.input.Breaker.FailureThreshold
	headComplete := ctx.ledger.headComplete()

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "breaker") {
			if n.Detail.From == "closed" && n.Detail.To == "open" {
				up := n.Detail.Upstream
				t := b.TimeNS

				// Must find a failure-counted effect for this upstream at the same instant with Count == threshold
				foundFailureCounted := false
				for i := range ctx.ledger.Blocks {
					otherB := &ctx.ledger.Blocks[i]
					if otherB.TimeNS == t {
						for _, bn := range otherB.Notes {
							if bn.Kind == "breaker" && bn.Detail.Decision == "failure-counted" && bn.Detail.Upstream == up && bn.Detail.Count == threshold {
								foundFailureCounted = true
							}
						}
					}
				}
				if !foundFailureCounted && headComplete {
					f := findingFrom(n, "E-breaker-open-threshold", "breaker for %s opened at %d without consecutive failures reaching threshold %d", up, t, threshold)
					r.fail(f)
				}
			}
		}
	}
	return *r
}

// checkBreakerCooldown verifies that a circuit breaker transitions from open to
// half-open only after the declared cooldown duration has elapsed.
func checkBreakerCooldown(ctx *evalContext) PropertyResult {
	r := newCheck("E-breaker-cooldown", "circuit breaker cooldown elapses before half-open transition")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; breaker cooldown cannot be established")
		return *r
	}
	cooldown := ctx.input.Breaker.CooldownNS
	headComplete := ctx.ledger.headComplete()

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "breaker") {
			if n.Detail.From == "open" && n.Detail.To == "half-open" {
				up := n.Detail.Upstream
				tHalf := b.TimeNS
				tOpen := tHalf - cooldown

				foundOpen := false
				for i := range ctx.ledger.Blocks {
					prevB := &ctx.ledger.Blocks[i]
					if prevB.TimeNS == tOpen {
						for _, bn := range prevB.Notes {
							if bn.Kind == "breaker" && bn.Detail.Upstream == up && bn.Detail.To == "open" {
								foundOpen = true
							}
						}
					}
				}
				if !foundOpen && headComplete && tOpen >= 0 {
					f := findingFrom(n, "E-breaker-cooldown", "breaker for %s transitioned to half-open at %d without an open transition at %d (cooldown %d)", up, tHalf, tOpen, cooldown)
					r.fail(f)
				}
			}
		}
	}
	return *r
}

// checkProbeAllowance verifies that half-open probe traffic stays within the
// declared limit, only reserves during half-open, never admits retries as probes,
// and pairs reservations with releases.
func checkProbeAllowance(ctx *evalContext) PropertyResult {
	r := newCheck("E-probe-allowance", "half-open probe reservations stay within declared limits and lifecycle")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; probe allowance cannot be established")
		return *r
	}
	maxProbes := ctx.input.Breaker.MaxHalfOpenProbes
	headComplete := ctx.ledger.headComplete()

	probesActive := make(map[string]int)
	isProbe := make(map[string]string) // attempt -> upstream

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		// Reset probe ledger on open or half-open transitions
		for _, n := range blockNotes(b, "breaker") {
			if n.Detail.To == "open" || n.Detail.To == "half-open" {
				probesActive[n.Detail.Upstream] = 0
			}
		}

		for _, n := range b.Notes {
			if n.Kind == "probe-reserved" {
				attID := n.Detail.Attempt
				up := n.Detail.Upstream
				if attID == "" || up == "" {
					r.addGap(fmt.Sprintf("probe reservation at effect_seq %d carries no attempt or upstream", n.EffectSeq))
					continue
				}
				if prevUp, exists := isProbe[attID]; exists {
					f := findingFrom(n, "E-probe-allowance", "attempt %s reserved a second probe permit on %s (already reserved on %s)", attID, up, prevUp)
					r.fail(f)
				}
				aw := ctx.world.AttemptByID[attID]
				if aw != nil && aw.Lane == "retry" {
					f := findingFrom(n, "E-probe-allowance", "attempt %s is a retry but reserved a probe permit on %s", attID, up)
					r.fail(f)
				}
				isProbe[attID] = up
				probesActive[up]++
				if probesActive[up] > maxProbes && headComplete {
					f := findingFrom(n, "E-probe-allowance", "upstream %s has %d active probes against a declared maximum of %d", up, probesActive[up], maxProbes)
					f.Expected = fmt.Sprintf("probes<=%d", maxProbes)
					f.Observed = fmt.Sprintf("probes=%d", probesActive[up])
					r.fail(f)
				}
			}
			if n.Kind == "probe-released" {
				attID := n.Detail.Attempt
				up := n.Detail.Upstream
				if n.Detail.Decision == "reset" {
					delete(isProbe, attID)
					continue
				}
				prevUp, exists := isProbe[attID]
				if !exists {
					if headComplete {
						f := findingFrom(n, "E-probe-allowance", "attempt %s released a probe permit on %s but no reservation is recorded", attID, up)
						r.fail(f)
					}
					continue
				}
				if up != "" && up != prevUp {
					f := findingFrom(n, "E-probe-allowance", "attempt %s released a probe permit on %s but reserved one on %s", attID, up, prevUp)
					r.fail(f)
				}
				delete(isProbe, attID)
				if probesActive[prevUp] > 0 {
					probesActive[prevUp]--
				}
			}
		}
	}
	return *r
}

// checkFailureObservation verifies that upstream failures are counted
// monotonically and reset upon success or breaker closure.
func checkFailureObservation(ctx *evalContext) PropertyResult {
	r := newCheck("E-failure-observation", "consecutive upstream failures are counted accurately and reset on success")
	headComplete := ctx.ledger.headComplete()
	failCount := make(map[string]int)

	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "breaker") {
			if n.Detail.Decision == "failure-counted" {
				up := n.Detail.Upstream
				last := failCount[up]
				if headComplete && n.Detail.Count != last+1 {
					f := findingFrom(n, "E-failure-observation", "consecutive failure count for %s jumped from %d to %d", up, last, n.Detail.Count)
					f.Expected = fmt.Sprintf("count=%d", last+1)
					f.Observed = fmt.Sprintf("count=%d", n.Detail.Count)
					r.fail(f)
				}
				failCount[up] = n.Detail.Count
			}
			if n.Detail.To == "open" || n.Detail.To == "closed" {
				failCount[n.Detail.Upstream] = 0
			}
		}

		// Success completion resets consecutive failures
		for _, n := range blockNotes(b, "attempt-transition") {
			if n.Detail.To == "completed" {
				aw := ctx.world.AttemptByID[n.Detail.Attempt]
				if aw != nil && aw.Upstream != "" {
					failCount[aw.Upstream] = 0
				}
			}
		}
	}
	return *r
}

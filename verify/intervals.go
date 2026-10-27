package verify

import "fmt"

// This file implements category B: resource intervals. Workers and connections
// are exclusive resources: an acquisition must be paired with exactly one
// release, a release must return the resource held by the releasing attempt, and
// the number of concurrent holders must stay within the declared capacity.
//
// The evidence for these properties is the typed worker/connection effect
// stream. An effect that lacks the typed decision or identity cannot support the
// claim; the property reports a gap rather than inferring the resource state from
// prose.

// checkWorkerIntervals verifies worker acquisition/release pairing. Per worker,
// acquisitions and releases must alternate, the releasing attempt must be the
// current holder, and no worker may be assigned twice without an intervening
// release.
func checkWorkerIntervals(ctx *evalContext) PropertyResult {
	r := newCheck("B-worker-interval-pairing", "worker acquisitions and releases are paired per worker")
	headComplete := ctx.ledger.headComplete()
	holder := make(map[string]string)
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "worker") {
			if n.Detail.Worker == "" || n.Detail.Attempt == "" {
				r.addGap(fmt.Sprintf("worker effect at effect_seq %d carries no typed worker/attempt identity", n.EffectSeq))
				continue
			}
			switch n.Detail.Decision {
			case "acquired":
				if prev, held := holder[n.Detail.Worker]; held {
					f := findingFrom(n, "B-worker-interval-pairing", "worker %s is assigned to %s while %s still holds it", n.Detail.Worker, n.Detail.Attempt, prev)
					f.Expected = "worker free before assignment"
					f.Observed = "held by " + prev
					r.fail(f)
					continue
				}
				holder[n.Detail.Worker] = n.Detail.Attempt
			case "released":
				prev, held := holder[n.Detail.Worker]
				if !held {
					if headComplete {
						f := findingFrom(n, "B-worker-interval-pairing", "worker %s is released by %s but no acquisition is recorded", n.Detail.Worker, n.Detail.Attempt)
						f.Expected = "an outstanding acquisition"
						f.Observed = "none"
						r.fail(f)
					}
					continue
				}
				if prev != n.Detail.Attempt {
					f := findingFrom(n, "B-worker-interval-pairing", "worker %s is released by %s but is held by %s", n.Detail.Worker, n.Detail.Attempt, prev)
					f.Expected = prev
					f.Observed = n.Detail.Attempt
					r.fail(f)
					continue
				}
				delete(holder, n.Detail.Worker)
			default:
				r.addGap(fmt.Sprintf("worker effect at effect_seq %d carries unknown decision %q", n.EffectSeq, n.Detail.Decision))
			}
		}
	}
	return *r
}

// checkWorkerOccupancy verifies that every worker effect reports an idle count
// consistent with the declared pool size and the holders the ledger itself
// establishes.
func checkWorkerOccupancy(ctx *evalContext) PropertyResult {
	r := newCheck("B-worker-occupancy", "reported worker occupancy matches the declared pool size")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; pool size cannot be established")
		return *r
	}
	headComplete := ctx.ledger.headComplete()
	size := ctx.input.WorkerPool.Size
	holder := make(map[string]string)
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "worker") {
			if n.Detail.Worker == "" || n.Detail.Attempt == "" {
				r.addGap(fmt.Sprintf("worker effect at effect_seq %d carries no typed worker/attempt identity", n.EffectSeq))
				continue
			}
			switch n.Detail.Decision {
			case "acquired":
				holder[n.Detail.Worker] = n.Detail.Attempt
			case "released":
				if holder[n.Detail.Worker] == n.Detail.Attempt {
					delete(holder, n.Detail.Worker)
				}
			default:
				continue
			}
			if len(holder) > size {
				f := findingFrom(n, "B-worker-occupancy", "%d workers are held but the declared pool size is %d", len(holder), size)
				f.Expected = fmt.Sprintf("holders<=%d", size)
				f.Observed = fmt.Sprintf("holders=%d", len(holder))
				r.fail(f)
			}
			if headComplete {
				if got, want := n.Detail.Count, size-len(holder); got != want {
					f := findingFrom(n, "B-worker-occupancy", "worker effect reports %d idle worker(s) but %d of %d are held by the ledger", got, len(holder), size)
					f.Expected = fmt.Sprintf("idle=%d", want)
					f.Observed = fmt.Sprintf("idle=%d", got)
					r.fail(f)
				}
			}
		}
	}
	return *r
}

// checkConnectionIntervals verifies connection acquisition/release pairing per
// attempt and the declared per-upstream capacity at every acquisition.
func checkConnectionIntervals(ctx *evalContext) PropertyResult {
	r := newCheck("B-connection-interval-pairing", "connection acquisitions and releases are paired per attempt and within capacity")
	if ctx.input == nil {
		r.addGap("the configuration contract is absent; connection capacity cannot be established")
		return *r
	}
	headComplete := ctx.ledger.headComplete()
	capacity := make(map[string]int, len(ctx.input.ConnectionPools))
	for _, p := range ctx.input.ConnectionPools {
		capacity[p.Upstream] = p.Capacity
	}
	held := make(map[string]string) // attempt -> upstream
	heldCount := make(map[string]int)
	for bi := range ctx.ledger.Blocks {
		b := &ctx.ledger.Blocks[bi]
		for _, n := range blockNotes(b, "connection") {
			if n.Detail.Attempt == "" {
				r.addGap(fmt.Sprintf("connection effect at effect_seq %d carries no typed attempt identity", n.EffectSeq))
				continue
			}
			switch n.Detail.Decision {
			case "acquired":
				if up, ok := held[n.Detail.Attempt]; ok {
					f := findingFrom(n, "B-connection-interval-pairing", "attempt %s acquires a second connection on %s while holding one on %s", n.Detail.Attempt, n.Detail.Upstream, up)
					f.Expected = "one held connection"
					f.Observed = "second acquisition"
					r.fail(f)
					continue
				}
				if n.Detail.Upstream == "" {
					r.addGap(fmt.Sprintf("connection acquisition at effect_seq %d names no upstream", n.EffectSeq))
					continue
				}
				held[n.Detail.Attempt] = n.Detail.Upstream
				heldCount[n.Detail.Upstream]++
				if cap, known := capacity[n.Detail.Upstream]; !known {
					r.addGap(fmt.Sprintf("upstream %s is not declared, so its connection capacity cannot be checked", n.Detail.Upstream))
				} else if heldCount[n.Detail.Upstream] > cap {
					f := findingFrom(n, "B-connection-interval-pairing", "%s holds %d connections but the declared capacity is %d", n.Detail.Upstream, heldCount[n.Detail.Upstream], cap)
					f.Expected = fmt.Sprintf("held<=%d", cap)
					f.Observed = fmt.Sprintf("held=%d", heldCount[n.Detail.Upstream])
					r.fail(f)
				}
			case "released":
				up, ok := held[n.Detail.Attempt]
				if !ok {
					if headComplete {
						f := findingFrom(n, "B-connection-interval-pairing", "attempt %s releases a connection but no acquisition is recorded", n.Detail.Attempt)
						f.Expected = "an outstanding acquisition"
						f.Observed = "none"
						r.fail(f)
					}
					continue
				}
				if n.Detail.Upstream != "" && n.Detail.Upstream != up {
					f := findingFrom(n, "B-connection-interval-pairing", "attempt %s releases a connection on %s but acquired one on %s", n.Detail.Attempt, n.Detail.Upstream, up)
					f.Expected = up
					f.Observed = n.Detail.Upstream
					r.fail(f)
				}
				delete(held, n.Detail.Attempt)
				if heldCount[up] > 0 {
					heldCount[up]--
				}
			default:
				// "waiting" and "granted" do not transfer the resource; the
				// acquisition effect is emitted by the running transition.
			}
		}
	}
	return *r
}

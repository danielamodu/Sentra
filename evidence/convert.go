// Package evidence is the serializer bridge between the reference system and
// the independent verifier (Milestone 3).
//
// It converts one recorded reference run into the verifier document contract:
//
//	sim.RunResult -> verify.Document
//
// Authority boundary (docs/INDEPENDENCE_MATRIX.md):
//
//   - This package contains NO verification logic. It never evaluates a
//     property, never decides whether evidence is sufficient and never changes a
//     verdict.
//   - It never repairs inconsistent records, normalizes contradictory evidence
//     into agreement, fills missing fields or drops invalid references. Every
//     value is copied verbatim; validation belongs to verify.
//   - It preserves the three distinct kinds of input: observations (records and
//     snapshot), declared inputs (the configuration contract) and, in a later
//     phase, reported claims. They are never merged.
//   - It does not rewrite historical M2 results. The conversion is a pure
//     function of the RunResult value it is given.
//
// Determinism: reference state is held in maps; every map-derived slice is
// sorted here so the same RunResult always serializes to the same document.
// Slices whose order is itself evidence (queue entries, connection waitlist,
// worker idle list, request attempt order) are preserved exactly.
package evidence

import (
	"fmt"
	"sort"

	"sentra/sim"
	"sentra/verify"
)

// FromRun converts one reference run into a verifier document.
//
// The conversion is total for well-formed RunResult values. The only reported
// serialization failure is an incomplete result whose sink retained no records
// although the engine processed events: an absent ledger must not be serialized
// as an empty (and therefore differently meaningful) one.
func FromRun(res sim.RunResult) (verify.Document, error) {
	if res.Records == nil && res.EngineEvents > 0 {
		return verify.Document{}, fmt.Errorf(
			"evidence: run %q processed %d engine events but its sink retained no records; the ledger is absent, not empty",
			res.Experiment.Name, res.EngineEvents)
	}

	doc := verify.Document{
		Schema:   verify.SchemaDocument,
		Scenario: res.Experiment.Name,
		RunID:    res.Experiment.Name,
		Input:    inputFromExperiment(res.Experiment),
		Evidence: verify.Evidence{
			SchemaVersion: sim.SchemaVersion,
			Records:       recordsFrom(res.Records),
			Snapshot:      snapshotFrom(res.FinalState, int64(res.Experiment.Horizon)),
		},
	}
	if len(doc.Evidence.Records) > 0 {
		doc.Evidence.FirstEffectSeq = doc.Evidence.Records[0].EffectSeq
	}
	if res.RecordCount > 0 {
		doc.Evidence.Terminal = &verify.TerminalAttestation{
			SchemaVersion: verify.TerminalSchemaVersion,
			EffectSeq:     res.TerminalEffectSeq,
			SimTimeNS:     res.TerminalSimTimeNS,
			RecordCount:   res.RecordCount,
			RecordsDigest: res.RecordsDigest,
		}
	}
	return doc, nil
}

// FromExperiment runs the reference experiment and converts its result. It is a
// harness convenience: it invokes reference execution, which the verifier itself
// must never do.
func FromExperiment(exp sim.Experiment) (verify.Document, error) {
	return FromRun(sim.RunExperiment(exp))
}

func recordsFrom(recs []sim.Record) []verify.Record {
	if len(recs) == 0 {
		return nil
	}
	out := make([]verify.Record, len(recs))
	for i := range recs {
		r := &recs[i]
		out[i] = verify.Record{
			SchemaVersion: r.SchemaVersion,
			EffectSeq:     r.EffectSeq,
			EngineSeq:     r.EngineSeq,
			SimTimeNS:     r.SimTimeNS,
			Category:      r.Category,
			Kind:          r.Kind,
			Subject:       r.Subject,
			Detail: verify.Detail{
				Request:     r.Detail.Request,
				Attempt:     r.Detail.Attempt,
				Tenant:      r.Detail.Tenant,
				Upstream:    r.Detail.Upstream,
				Worker:      r.Detail.Worker,
				From:        r.Detail.From,
				To:          r.Detail.To,
				Reason:      r.Detail.Reason,
				Lane:        r.Detail.Lane,
				Description: r.Detail.Description,
				DeadlineNS:  r.Detail.DeadlineNS,
				Decision:    r.Detail.Decision,
				Predecessor: r.Detail.Predecessor,
				Outcome:     r.Detail.Outcome,
				Count:       r.Detail.Count,
				LatencyNS:   r.Detail.LatencyNS,
				Seq:         r.Detail.Seq,
				MaxRetries:  r.Detail.MaxRetries,
			},
		}
	}
	return out
}

func inputFromExperiment(exp sim.Experiment) *verify.Input {
	cfg := exp.Config
	in := &verify.Input{
		Scenario:         exp.Name,
		RunID:            exp.Name,
		HorizonNS:        int64(exp.Horizon),
		WorkerPool:       verify.WorkerPoolSpec{ID: string(cfg.WorkerPool.ID), Size: cfg.WorkerPool.Size},
		RetryBudget:      verify.RetryBudgetSpec{Capacity: cfg.RetryBudget.Capacity, RefillIntervalNS: int64(cfg.RetryBudget.RefillInterval)},
		RetryBackoffNS:   int64(cfg.RetryBackoff),
		AttemptTimeoutNS: int64(cfg.AttemptTimeout),
		Breaker: verify.BreakerSpec{
			FailureThreshold:  cfg.Breaker.FailureThreshold,
			CooldownNS:        int64(cfg.Breaker.Cooldown),
			MaxHalfOpenProbes: cfg.Breaker.MaxHalfOpenProbes,
		},
	}

	for up, capacity := range cfg.ConnectionPools {
		in.ConnectionPools = append(in.ConnectionPools, verify.ConnectionPoolSpec{
			Upstream: string(up),
			Capacity: capacity,
		})
	}
	sort.Slice(in.ConnectionPools, func(i, j int) bool {
		return in.ConnectionPools[i].Upstream < in.ConnectionPools[j].Upstream
	})

	for tenant, tc := range cfg.Tenants {
		in.Tenants = append(in.Tenants, verify.TenantSpec{
			ID:         string(tenant),
			QueueLane:  tc.MaxArrivalQueueDepth,
			QueueRetry: tc.MaxRetryQueueDepth,
			QueueTotal: tc.MaxTotalQueueDepth,
		})
	}
	sort.Slice(in.Tenants, func(i, j int) bool { return in.Tenants[i].ID < in.Tenants[j].ID })

	if len(exp.Workload.Components) > 0 || len(exp.Workload.Bursts) > 0 {
		in.Workload = workloadSpecFrom(exp.Workload)
	}
	if len(exp.Faults) > 0 {
		in.Faults = faultScheduleFrom(exp.Faults)
	}
	return in
}

func workloadSpecFrom(w sim.WorkloadSpec) *verify.WorkloadSpec {
	out := &verify.WorkloadSpec{Seed: w.Seed}
	for i := range w.Components {
		c := w.Components[i]
		out.Components = append(out.Components, verify.ArrivalComponent{
			Tenant:        string(c.Tenant),
			Upstream:      string(c.Upstream),
			Rate:          c.Rate,
			WindowNS:      int64(c.Window),
			FromNS:        int64(c.From),
			UntilNS:       int64(c.Until),
			DeadlineMinNS: int64(c.DeadlineMin),
			DeadlineMaxNS: int64(c.DeadlineMax),
			MaxRetries:    c.MaxRetries,
			Jitter:        c.Jitter,
		})
	}
	for i := range w.Bursts {
		b := w.Bursts[i]
		out.Bursts = append(out.Bursts, verify.Burst{
			Tenant:        string(b.Tenant),
			Upstream:      string(b.Upstream),
			AtNS:          int64(b.At),
			Count:         b.Count,
			SpreadNS:      int64(b.Spread),
			DeadlineMinNS: int64(b.DeadlineMin),
			DeadlineMaxNS: int64(b.DeadlineMax),
			MaxRetries:    b.MaxRetries,
		})
	}
	return out
}

func faultScheduleFrom(fs sim.FaultSchedule) *verify.FaultSchedule {
	out := make(verify.FaultSchedule, len(fs))
	for i := range fs {
		e := fs[i]
		out[i] = verify.FaultEpisode{
			Upstream:  string(e.Upstream),
			Kind:      verify.FaultKind(e.Kind.String()),
			EveryN:    e.EveryN,
			LatencyNS: int64(e.It),
			FromNS:    int64(e.From),
			UntilNS:   int64(e.Until),
		}
	}
	return &out
}

func snapshotFrom(st *sim.SystemState, takenAtNS int64) *verify.Snapshot {
	if st == nil {
		return nil
	}
	s := &verify.Snapshot{
		SchemaVersion: verify.SnapshotSchemaVersion,
		TakenAtNS:     takenAtNS,
	}

	for _, id := range sortedRequestIDs(st) {
		r := st.Requests[id]
		attempts := make([]string, len(r.Attempts))
		for i, a := range r.Attempts {
			attempts[i] = string(a)
		}
		s.Requests = append(s.Requests, verify.RequestFact{
			ID:           string(r.ID),
			Tenant:       string(r.Tenant),
			Upstream:     string(r.Upstream),
			State:        r.State.String(),
			ArrivedAtNS:  int64(r.ArrivedAt),
			DeadlineNS:   int64(r.Deadline),
			TerminalAtNS: int64(r.TerminalAt),
			MaxRetries:   r.MaxRetries,
			RetriesUsed:  r.RetriesUsed,
			Attempts:     attempts,
		})
	}

	for _, id := range sortedAttemptIDs(st) {
		a := st.Attempts[id]
		s.Attempts = append(s.Attempts, verify.AttemptFact{
			ID:           string(a.ID),
			Request:      string(a.LogicalID),
			Tenant:       string(a.Tenant),
			Upstream:     string(a.Upstream),
			State:        a.State.String(),
			Outcome:      a.Outcome.String(),
			CreatedAtNS:  int64(a.CreatedAt),
			FinishedAtNS: int64(a.FinishedAt),
			TimeoutNS:    int64(a.Timeout),
			Worker:       string(a.Worker),
			HasWorker:    a.HasWorker,
			ConnHeld:     a.ConnHeld,
			IsProbe:      a.IsProbe,
			IsRetry:      a.IsRetry,
			Orphaned:     a.Orphaned,
			OrphanedAtNS: int64(a.OrphanedAt),
		})
	}

	for _, id := range sortedPoolIDs(st) {
		p := st.Pools[id]
		fact := verify.WorkerPoolFact{ID: string(p.ID), Size: p.Size}
		for _, w := range sortedBusyWorkers(p) {
			fact.Busy = append(fact.Busy, verify.WorkerBusyFact{
				Worker:  string(w),
				Attempt: string(p.Busy[w]),
			})
		}
		for _, w := range p.Idle {
			fact.Idle = append(fact.Idle, string(w))
		}
		s.WorkerPools = append(s.WorkerPools, fact)
	}

	for _, up := range sortedConnectionUpstreams(st) {
		p := st.Connections[up]
		fact := verify.ConnectionPoolFact{Upstream: string(p.Upstream), Capacity: p.Capacity}
		held := make([]string, 0, len(p.Held))
		for a := range p.Held {
			held = append(held, string(a))
		}
		sort.Strings(held)
		fact.Held = held
		for _, a := range p.Waitlist {
			fact.Waiting = append(fact.Waiting, string(a))
		}
		s.Connections = append(s.Connections, fact)
	}

	for _, tenant := range sortedTenantIDs(st) {
		q := st.Queues[tenant]
		fact := verify.QueueFact{Tenant: string(q.Tenant)}
		for _, e := range q.Entries {
			fact.Entries = append(fact.Entries, verify.QueueEntryFact{
				Attempt:      string(e.Attempt),
				EnqueuedAtNS: int64(e.EnqueuedAt),
				Seq:          e.Seq,
			})
		}
		s.Queues = append(s.Queues, fact)
	}

	s.Pending = pendingFacts(st)

	for _, tenant := range sortedTenantIDs(st) {
		s.TenantActive = append(s.TenantActive, verify.TenantActiveFact{
			Tenant: string(tenant),
			Active: st.TenantActive[tenant],
		})
		c := st.OutcomeCounters[tenant]
		s.Outcomes = append(s.Outcomes, verify.OutcomeFact{
			Tenant:    string(tenant),
			Useful:    c.Useful,
			Wasted:    c.Wasted,
			Discarded: c.Discarded,
		})
	}

	for _, key := range sortedBudgetKeys(st) {
		b := st.Budgets[key]
		s.Budgets = append(s.Budgets, verify.BudgetFact{
			Tenant:           string(key.Tenant),
			Upstream:         string(key.Upstream),
			Capacity:         b.Capacity,
			Tokens:           b.Tokens,
			RefillIntervalNS: int64(b.RefillInterval),
			LastRefillNS:     int64(b.LastRefill),
		})
	}
	for _, key := range sortedConsumedKeys(st) {
		s.TokensConsumed = append(s.TokensConsumed, verify.TokenConsumedFact{
			Tenant:   string(key.Tenant),
			Upstream: string(key.Upstream),
			Consumed: st.RetryTokensConsumed[key],
		})
	}

	for _, up := range sortedBreakerUpstreams(st) {
		b := st.Breakers[up]
		s.Breakers = append(s.Breakers, verify.BreakerFact{
			Upstream:            string(b.Upstream),
			State:               b.State.String(),
			ConsecutiveFailures: b.ConsecutiveFailures,
			ProbesActive:        b.ProbesActive,
			OpenedAtNS:          int64(b.OpenedAt),
		})
	}

	return s
}

// pendingFacts merges the two pending books into one sorted slice. The two
// books are separate in the reference state (arrival and retry lanes), so the
// lane kind is preserved per entry.
func pendingFacts(st *sim.SystemState) []verify.PendingFact {
	var out []verify.PendingFact
	for a := range st.PendingArrivalAttempts {
		att := st.Attempts[a]
		if att == nil {
			continue
		}
		out = append(out, verify.PendingFact{
			Tenant:  string(att.Tenant),
			Attempt: string(a),
			Kind:    "arrival",
		})
	}
	for a := range st.PendingRetryAttempts {
		att := st.Attempts[a]
		if att == nil {
			continue
		}
		out = append(out, verify.PendingFact{
			Tenant:  string(att.Tenant),
			Attempt: string(a),
			Kind:    "retry",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Attempt < out[j].Attempt
	})
	return out
}

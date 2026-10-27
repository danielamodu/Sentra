package evidence

import (
	"sort"

	"sentra/domain"
	"sentra/sim"
)

// Map iteration order in Go is deliberately unspecified. The reference state
// stores entities in maps, so the bridge materializes them in a deterministic
// order derived from the entity identity. These helpers are pure ordering: they
// add, remove and rewrite nothing.

func sortedRequestIDs(st *sim.SystemState) []domain.RequestID {
	out := make([]domain.RequestID, 0, len(st.Requests))
	for id := range st.Requests {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedAttemptIDs(st *sim.SystemState) []domain.AttemptID {
	out := make([]domain.AttemptID, 0, len(st.Attempts))
	for id := range st.Attempts {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedPoolIDs(st *sim.SystemState) []domain.WorkerPoolID {
	out := make([]domain.WorkerPoolID, 0, len(st.Pools))
	for id := range st.Pools {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedBusyWorkers(p *domain.WorkerPool) []domain.WorkerID {
	out := make([]domain.WorkerID, 0, len(p.Busy))
	for w := range p.Busy {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedConnectionUpstreams(st *sim.SystemState) []domain.UpstreamID {
	out := make([]domain.UpstreamID, 0, len(st.Connections))
	for up := range st.Connections {
		out = append(out, up)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedTenantIDs(st *sim.SystemState) []domain.TenantID {
	out := make([]domain.TenantID, 0, len(st.Queues))
	for id := range st.Queues {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedBudgetKeys(st *sim.SystemState) []domain.BudgetKey {
	out := make([]domain.BudgetKey, 0, len(st.Budgets))
	for key := range st.Budgets {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Upstream < out[j].Upstream
	})
	return out
}

func sortedConsumedKeys(st *sim.SystemState) []domain.BudgetKey {
	out := make([]domain.BudgetKey, 0, len(st.RetryTokensConsumed))
	for key := range st.RetryTokensConsumed {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tenant != out[j].Tenant {
			return out[i].Tenant < out[j].Tenant
		}
		return out[i].Upstream < out[j].Upstream
	})
	return out
}

func sortedBreakerUpstreams(st *sim.SystemState) []domain.UpstreamID {
	out := make([]domain.UpstreamID, 0, len(st.Breakers))
	for up := range st.Breakers {
		out = append(out, up)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

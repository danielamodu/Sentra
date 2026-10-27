package sim

// occupancy.go records the M2 resource-occupancy timelines (workstream 4).
// Every sample is appended at the exact simulated instant a resource's
// occupancy changes, so occupancy is piecewise-constant between samples and
// NO sampling clock exists anywhere. Phase aggregation (recovery.go) therefore
// captures instantaneous spikes at full fidelity with zero overhead between
// transitions.
//
// Tracking is evidence-only: no handler reads timelines back, no sample is
// ever removed or edited after appending, and the append order is the handler
// execution order — which is fully deterministic under the engine. A full
// DeepEqual of state therefore remains a valid repeatability check
// (docs/ENGINE_SEMANTICS.md §7).

import "sentra/domain"

// WorkerSample is one point on the worker-pool occupancy timeline.
type WorkerSample struct {
	At   domain.SimTime `json:"at_ns"`
	Busy int            `json:"busy"`
	Size int            `json:"size"`
}

// ConnSample is one point on a connection pool's occupancy timeline.
type ConnSample struct {
	At      domain.SimTime    `json:"at_ns"`
	Up      domain.UpstreamID `json:"upstream"`
	Busy    int               `json:"busy"`
	Cap     int               `json:"cap"`
	Waiting int               `json:"waiting"`
}

// QueueSample is one point on a tenant queue's occupancy timeline. Both lane
// depths and the pending books are sampled, so the timeline matches the lane
// accounting used by admission (see shedReason).
type QueueSample struct {
	At              domain.SimTime  `json:"at_ns"`
	Tenant          domain.TenantID `json:"tenant"`
	Length          int             `json:"length"`
	ArrivalLane     int             `json:"arrival_lane"`
	RetryLane       int             `json:"retry_lane"`
	PendingArrivals int             `json:"pending_arrivals"`
	PendingRetries  int             `json:"pending_retries"`
}

// recordWorkerOccupancy appends a worker-pool sample. Called after every
// busy-slot transition.
func (st *SystemState) recordWorkerOccupancy(t domain.SimTime) {
	pool := st.mainPool()
	st.WorkerOccupancy = append(st.WorkerOccupancy, WorkerSample{At: t, Busy: pool.Occupancy(), Size: pool.Size})
}

// recordConnOccupancy appends a connection-pool sample. Called after every
// held or waitlisted transition on that pool.
func (st *SystemState) recordConnOccupancy(t domain.SimTime, up domain.UpstreamID) {
	cp := st.Connections[up]
	st.ConnOccupancy = append(st.ConnOccupancy, ConnSample{
		At:      t,
		Up:      up,
		Busy:    cp.InUse(),
		Cap:     cp.Capacity,
		Waiting: cp.Waiting(),
	})
}

// recordQueueOccupancy appends a tenant-queue sample. Called after every
// append/remove/retract-from-queue transition.
func (st *SystemState) recordQueueOccupancy(t domain.SimTime, tid domain.TenantID) {
	q := st.Queues[tid]
	nA, nR := 0, 0
	for _, e := range q.Entries {
		if att := st.Attempts[e.Attempt]; att != nil {
			if att.IsRetry {
				nR++
			} else {
				nA++
			}
		}
	}
	st.QueueOccupancy = append(st.QueueOccupancy, QueueSample{
		At:              t,
		Tenant:          tid,
		Length:          q.Len(),
		ArrivalLane:     nA,
		RetryLane:       nR,
		PendingArrivals: st.PendingArrivalCounts[tid],
		PendingRetries:  st.PendingRetryCounts[tid],
	})
}

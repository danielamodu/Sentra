package domain

import "fmt"

// QueueEntry identifies one queued physical attempt. Seq is a globally
// monotonic insertion sequence number shared across all tenant queues, which
// makes cross-queue ordering (and therefore dispatch eligibility) fully
// deterministic.
type QueueEntry struct {
	Attempt    AttemptID
	EnqueuedAt SimTime
	Seq        uint64
}

// Queue is the per-tenant FIFO admission queue. One queue per tenant keeps
// shedding decisions tenant-scoped; global dispatch eligibility is resolved by
// the shared Seq counter.
type Queue struct {
	Tenant  TenantID
	Entries []QueueEntry
}

// NewQueue creates an empty queue for a tenant.
func NewQueue(t TenantID) *Queue { return &Queue{Tenant: t} }

func (q *Queue) Len() int { return len(q.Entries) }

func (q *Queue) Contains(a AttemptID) bool {
	for _, e := range q.Entries {
		if e.Attempt == a {
			return true
		}
	}
	return false
}

func (q *Queue) Head() (QueueEntry, bool) {
	if len(q.Entries) == 0 {
		return QueueEntry{}, false
	}
	return q.Entries[0], true
}

// Append adds an entry at the tail; duplicates are an invariant violation.
func (q *Queue) Append(e QueueEntry) error {
	if q.Contains(e.Attempt) {
		return fmt.Errorf("queue %s: attempt %s already queued", q.Tenant, e.Attempt)
	}
	q.Entries = append(q.Entries, e)
	return nil
}

// Remove removes the entry for attempt a; reports whether it was found.
func (q *Queue) Remove(a AttemptID) bool {
	for i, e := range q.Entries {
		if e.Attempt == a {
			q.Entries = append(q.Entries[:i], q.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// EntryFor returns the queue entry currently registered for attempt a.
// It backs the indexed validator path (docs/ARCHITECTURE.md §4): probes of
// dispatch eligibility and cross-checks of per-tenant queue-wait metrics need
// the full entry (enqueued time and global sequence), not just membership.
func (q *Queue) EntryFor(a AttemptID) (QueueEntry, bool) {
	for _, e := range q.Entries {
		if e.Attempt == a {
			return e, true
		}
	}
	return QueueEntry{}, false
}

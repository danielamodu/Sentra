package domain

import "fmt"

// WorkerPool is a finite set of execution slots. Idle is a FIFO free list:
// workers are taken in release order, so repeated runs with identical inputs
// produce identical assignments.
type WorkerPool struct {
	ID   WorkerPoolID
	Size int
	Busy map[WorkerID]AttemptID
	Idle []WorkerID
}

// NewWorkerPool creates a pool with deterministic worker IDs "<poolID>-w-NN".
func NewWorkerPool(id WorkerPoolID, size int) (*WorkerPool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("worker pool %s: size must be positive, got %d", id, size)
	}
	p := &WorkerPool{ID: id, Size: size, Busy: make(map[WorkerID]AttemptID, size)}
	for i := 0; i < size; i++ {
		p.Idle = append(p.Idle, WorkerID(fmt.Sprintf("%s-w-%02d", id, i)))
	}
	return p, nil
}

func (p *WorkerPool) HasIdle() bool  { return len(p.Idle) > 0 }
func (p *WorkerPool) IdleCount() int { return len(p.Idle) }
func (p *WorkerPool) Occupancy() int { return len(p.Busy) }

// FirstIdle returns the worker that would be assigned next (FIFO release
// order), or "" when the pool has no free worker.
func (p *WorkerPool) FirstIdle() WorkerID {
	if len(p.Idle) == 0 {
		return ""
	}
	return p.Idle[0]
}

// Take removes w from the idle list and attributes it to attempt `by`.
func (p *WorkerPool) Take(w WorkerID, by AttemptID) error {
	for i, id := range p.Idle {
		if id == w {
			p.Idle = append(p.Idle[:i], p.Idle[i+1:]...)
			p.Busy[w] = by
			return nil
		}
	}
	return fmt.Errorf("worker pool %s: worker %s is not idle", p.ID, w)
}

// Release returns w to the idle list. Release order defines assignment order.
func (p *WorkerPool) Release(w WorkerID) error {
	if _, ok := p.Busy[w]; !ok {
		return fmt.Errorf("worker pool %s: worker %s is not busy", p.ID, w)
	}
	delete(p.Busy, w)
	p.Idle = append(p.Idle, w)
	return nil
}

// ConnectionPool is a finite set of upstream connections. Held is the set of
// attempts currently owning a connection; Waitlist is a FIFO list of attempts
// holding a worker while pinned waiting for a connection.
type ConnectionPool struct {
	Upstream UpstreamID
	Capacity int
	Held     map[AttemptID]struct{}
	Waitlist []AttemptID
}

// NewConnectionPool creates a connection pool for one upstream.
func NewConnectionPool(up UpstreamID, capacity int) (*ConnectionPool, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("connection pool %s: capacity must be positive, got %d", up, capacity)
	}
	return &ConnectionPool{
		Upstream: up,
		Capacity: capacity,
		Held:     make(map[AttemptID]struct{}, capacity),
	}, nil
}

func (p *ConnectionPool) HasCapacity() bool { return len(p.Held) < p.Capacity }
func (p *ConnectionPool) InUse() int        { return len(p.Held) }
func (p *ConnectionPool) Waiting() int      { return len(p.Waitlist) }

// Acquire grants a connection to attempt a (must have capacity).
func (p *ConnectionPool) Acquire(a AttemptID) error {
	if !p.HasCapacity() {
		return fmt.Errorf("connection pool %s: no capacity for %s", p.Upstream, a)
	}
	if _, ok := p.Held[a]; ok {
		return fmt.Errorf("connection pool %s: %s already holds a connection", p.Upstream, a)
	}
	p.Held[a] = struct{}{}
	return nil
}

// Release returns the connection owned by attempt a.
func (p *ConnectionPool) Release(a AttemptID) error {
	if _, ok := p.Held[a]; !ok {
		return fmt.Errorf("connection pool %s: %s does not hold a connection", p.Upstream, a)
	}
	delete(p.Held, a)
	return nil
}

// EnqueueWaiter pins attempt a to the end of the FIFO waitlist.
func (p *ConnectionPool) EnqueueWaiter(a AttemptID) error {
	for _, w := range p.Waitlist {
		if w == a {
			return fmt.Errorf("connection pool %s: %s already waiting", p.Upstream, a)
		}
	}
	p.Waitlist = append(p.Waitlist, a)
	return nil
}

// PopWaiterHead removes and returns the oldest waiter, if any.
func (p *ConnectionPool) PopWaiterHead() (AttemptID, bool) {
	if len(p.Waitlist) == 0 {
		return "", false
	}
	head := p.Waitlist[0]
	p.Waitlist = p.Waitlist[1:]
	return head, true
}

// RemoveWaiter removes attempt a from the waitlist (used by timeout and
// retraction). It reports whether a was found.
func (p *ConnectionPool) RemoveWaiter(a AttemptID) bool {
	for i, w := range p.Waitlist {
		if w == a {
			p.Waitlist = append(p.Waitlist[:i], p.Waitlist[i+1:]...)
			return true
		}
	}
	return false
}

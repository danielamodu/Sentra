package engine

import (
	"container/heap"
	"fmt"

	"sentra/domain"
)

type scheduled struct {
	ev  Event
	seq uint64
}

// eventQueue is a min-heap ordered by (event time, priority, scheduling seq).
// The seq component gives same-instant, same-priority events a deterministic
// FIFO tie-break derived from scheduling order, which itself is deterministic
// for identical engine inputs.
type eventQueue []scheduled

func (q eventQueue) Len() int { return len(q) }
func (q eventQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if ta, tb := a.ev.At(), b.ev.At(); ta != tb {
		return ta < tb
	}
	if pa, pb := a.ev.Priority(), b.ev.Priority(); pa != pb {
		return pa < pb
	}
	return a.seq < b.seq
}
func (q eventQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *eventQueue) Push(x any)   { *q = append(*q, x.(scheduled)) }
func (q *eventQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}

// Engine is the deterministic discrete-event core.
type Engine struct {
	handler   Handler
	q         eventQueue
	nextSeq   uint64
	now       domain.SimTime
	processed int
}

// New creates an engine with the given handler and initial events. Initial
// events are assigned scheduling sequence numbers in list order; events at
// negative simulated times are rejected.
func New(h Handler, initial []Event) *Engine {
	if h == nil {
		panic("engine: nil handler")
	}
	e := &Engine{handler: h}
	for _, ev := range initial {
		if ev.At() < 0 {
			panic(fmt.Sprintf("engine: initial event at negative simulated time %s", ev.At()))
		}
		e.schedule(ev)
	}
	return e
}

func (e *Engine) schedule(ev Event) {
	heap.Push(&e.q, scheduled{ev: ev, seq: e.nextSeq})
	e.nextSeq++
}

// Schedule injects an event at or after the current simulated time. It exists
// for test harnesses and future orchestration layers that deterministically
// extend a run; production transition logic should only schedule through
// handler return values.
func (e *Engine) Schedule(ev Event) {
	if ev.At() < e.now {
		panic(fmt.Sprintf("engine: injected event at %s behind current time %s", ev.At(), e.now))
	}
	e.schedule(ev)
}

// Now returns the current simulated time — the time of the most recently
// executed event, or 0 before any event ran.
func (e *Engine) Now() domain.SimTime { return e.now }

// Pending reports how many events are scheduled and not yet executed.
func (e *Engine) Pending() int { return len(e.q) }

// Processed reports how many events have been executed.
func (e *Engine) Processed() int { return e.processed }

// Step pops exactly one event and executes it, returning the event and true,
// or (nil, false) when nothing is scheduled.
//
// Invariants enforced here:
//
//   - a popped event is never older than the current time (would indicate a
//     broken heap or a mutated event);
//   - handler-produced events never precede the current simulated time.
func (e *Engine) Step() (Event, bool) {
	if len(e.q) == 0 {
		return nil, false
	}
	s := heap.Pop(&e.q).(scheduled)
	t := s.ev.At()
	if t < e.now {
		panic(fmt.Sprintf("engine: internal ordering violated: event seq=%d at %s popped behind current time %s", s.seq, t, e.now))
	}
	e.now = t
	e.processed++
	for _, out := range e.handler.Handle(s.ev, s.seq) {
		if out.At() < e.now {
			panic(fmt.Sprintf("engine: handler scheduled event at %s behind current time %s", out.At(), e.now))
		}
		e.schedule(out)
	}
	return s.ev, true
}

// Run executes events with At() <= until, in engine order, until the queue is
// empty or the next event is beyond until. It returns the number of events
// processed. The boundary is inclusive: an event exactly at until is executed.
func (e *Engine) Run(until domain.SimTime) int {
	n := 0
	for len(e.q) > 0 {
		if t := e.q[0].ev.At(); t > until {
			break
		}
		e.Step()
		n++
	}
	return n
}

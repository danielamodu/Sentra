// Package engine implements Sentra's deterministic discrete-event core.
//
// The engine knows nothing about Sentra's domain beyond simulated time: it
// orders events by the triple (simulated time, caller-defined priority class,
// scheduling sequence number), pops them one at a time, and hands each to a
// Handler that may return further events to schedule.
//
// Guarantees, independent of the handler's domain semantics:
//
//   - simulated time advances only by executing events; the wall clock is
//     never read;
//   - events execute in strictly ascending (time, priority, seq) order;
//   - two engines with the same handler and the same initial event list
//     execute the identical event sequence and produce identical state;
//   - a handler may only schedule events at or after the current simulated
//     time; scheduling in the past is an invariant violation and panics;
//   - once scheduled, an event never changes position.
//
// The engine contains no goroutine scheduling, timers, channels or randomness.
// It is deliberately free of Sentra-specific transition logic: the future
// independent simulator is expected to plug its own handlers onto this same
// core, sharing domain contracts but not transition implementations.
//
// The handler receives the scheduling sequence number (seq) of the event it is
// consuming; it is the engine's one anchoring value that lets handler-side
// evidence (records) be correlated back to the exact event processing step
// regardless of how many effects that step produced.
package engine

import "sentra/domain"

// Event is anything the engine can order: a simulated-time point and a
// caller-defined priority class. Priority classes are a coarse, documented
// ordering dimension used to resolve same-instant races (for example deadline
// expiration versus execution completion).
type Event interface {
	At() domain.SimTime
	Priority() int
}

// Handler consumes one event and returns any events to schedule now or later.
// The seq argument is the engine's scheduling sequence number of the consumed
// event (its unique position in the (time, priority, seq) ordering); handlers
// use it to correlate their recorded effects with the originating event.
//
// Handlers must never observe wall-clock time and must not mutate events that
// the engine already scheduled. Returned events must satisfy
// At() >= current simulated time.
type Handler interface {
	Handle(ev Event, seq uint64) []Event
}

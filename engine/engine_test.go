package engine

import (
	"fmt"
	"reflect"
	"testing"

	"sentra/domain"
)

// msT is one simulated millisecond, as a time point unit.
const msT = domain.SimTime(domain.Millisecond)

// testEvent is a minimal engine-agnostic event; it proves the engine orders
// foreign event types purely by (time, priority, scheduling sequence).
type testEvent struct {
	t        domain.SimTime
	priority int
	id       string
}

func (e *testEvent) At() domain.SimTime { return e.t }
func (e *testEvent) Priority() int      { return e.priority }

// traceHandler records the processed event IDs and can return scripted
// follow-up events per consumed event.
type traceHandler struct {
	trace []string
	outs  map[string][]Event
}

func (h *traceHandler) Handle(ev Event, seq uint64) []Event {
	te := ev.(*testEvent)
	h.trace = append(h.trace, te.id)
	_ = seq
	return h.outs[te.id]
}

func ev(t domain.SimTime, id string, priorities ...int) *testEvent {
	p := 50
	if len(priorities) > 0 {
		p = priorities[0]
	}
	return &testEvent{t: t, priority: p, id: id}
}

func initial(evs ...*testEvent) []Event {
	out := make([]Event, len(evs))
	for i, e := range evs {
		out[i] = e
	}
	return out
}

func TestEventsExecuteInSimulatedTimeOrder(t *testing.T) {
	h := &traceHandler{}
	e := New(h, initial(
		ev(10*msT, "late"),
		ev(0, "first"),
		ev(3*msT, "third"),
	))
	e.Run(100 * msT)

	if !reflect.DeepEqual(h.trace, []string{"first", "third", "late"}) {
		t.Fatalf("unexpected execution order: %v", h.trace)
	}
	if e.Processed() != 3 {
		t.Fatalf("processed = %d, want 3", e.Processed())
	}
	if e.Now() != 10*msT {
		t.Fatalf("now = %s, want %s", e.Now(), 10*msT)
	}
	if e.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", e.Pending())
	}
}

func TestSameInstantPriorityPrecedesSchedulingOrder(t *testing.T) {
	h := &traceHandler{}
	// "work" is scheduled first but has the lower precedence (higher number).
	e := New(h, initial(
		ev(5*msT, "work", 30),
		ev(5*msT, "governance", 10),
	))
	e.Run(5 * msT)

	if !reflect.DeepEqual(h.trace, []string{"governance", "work"}) {
		t.Fatalf("priority class must run before later class at same instant: %v", h.trace)
	}
}

func TestSameInstantSamePriorityHonorsSchedulingOrder(t *testing.T) {
	h := &traceHandler{}
	e := New(h, initial(
		ev(7*msT, "b"),
		ev(7*msT, "a"),
	))
	e.Run(7 * msT)

	if !reflect.DeepEqual(h.trace, []string{"b", "a"}) {
		t.Fatalf("same-instant same-priority events must run in scheduling order: %v", h.trace)
	}
}

func TestRunUntilBoundaryIsInclusive(t *testing.T) {
	h := &traceHandler{}
	e := New(h, initial(
		ev(1*msT, "a"),
		ev(2*msT, "b"),
		ev(3*msT, "c"),
	))
	n := e.Run(2 * msT)
	if n != 2 {
		t.Fatalf("Run processed %d events, want 2", n)
	}
	if got, _ := e.Step(); got == nil {
		t.Fatal("event exactly beyond until must remain schedulable via Step")
	}
	if !reflect.DeepEqual(h.trace, []string{"a", "b", "c"}) {
		t.Fatalf("unexpected trace: %v", h.trace)
	}
}

func TestStepProcessesExactlyOneEvent(t *testing.T) {
	h := &traceHandler{}
	e := New(h, initial(
		ev(0, "a"),
		ev(0, "b"),
	))
	if _, ok := e.Step(); !ok {
		t.Fatal("expected first step to process an event")
	}
	if _, ok := e.Step(); !ok {
		t.Fatal("expected second step to process an event")
	}
	if _, ok := e.Step(); ok {
		t.Fatal("expected empty queue on third step")
	}
	if !reflect.DeepEqual(h.trace, []string{"a", "b"}) {
		t.Fatalf("unexpected trace: %v", h.trace)
	}
}

func TestHandlersCannotScheduleInThePast(t *testing.T) {
	h := &traceHandler{outs: map[string][]Event{
		"a": {ev(0, "past-event")},
	}}
	e := New(h, initial(ev(2*msT, "a")))
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic when handler schedules behind current time")
			}
			if fmt.Sprint(r) == "" {
				t.Fatal("panic message must be non-empty")
			}
		}()
		e.Run(5 * msT)
	}()
}

func TestScheduleInjectionRejectsPastTimes(t *testing.T) {
	h := &traceHandler{}
	e := New(h, initial(ev(10*msT, "a")))
	e.Run(10 * msT)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for injection behind current time")
		}
	}()
	e.Schedule(ev(9*msT, "too-late"))
}

func TestRepeatabilityOfIdenticalEngineInputs(t *testing.T) {
	build := func() (*Engine, *traceHandler) {
		h := &traceHandler{outs: map[string][]Event{
			"a":  {ev(2*msT, "a1")},
			"a1": {ev(2*msT, "a2", 10), ev(9*msT, "a3")},
		}}
		e := New(h, initial(
			ev(0, "a"),
			ev(2*msT, "side", 30),
		))
		return e, h
	}

	e1, h1 := build()
	e1.Run(50 * msT)
	e2, h2 := build()
	// Drive the second engine in exact single steps instead of Run.
	for {
		if _, more := e2.Step(); !more {
			break
		}
	}

	if !reflect.DeepEqual(h1.trace, h2.trace) {
		t.Fatalf("identical inputs must produce identical execution traces:\nRun:  %v\nStep: %v", h1.trace, h2.trace)
	}
	if e1.Now() != e2.Now() {
		t.Fatalf("final simulated times differ: %s vs %s", e1.Now(), e2.Now())
	}
}

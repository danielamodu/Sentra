package domain

import (
	"reflect"
	"testing"
)

func TestLogicalAndAttemptIdentityAreDistinctTypes(t *testing.T) {
	// The two ID spaces share representation but not type: converting between
	// them requires an explicit cast, which keeps request/attempt identity
	// separation enforceable by the compiler.
	var r RequestID = "1"
	var a AttemptID = "1"
	if reflect.TypeOf(r).String() == reflect.TypeOf(a).String() {
		t.Fatalf("RequestID and AttemptID must be distinct types, both are %s", reflect.TypeOf(r))
	}
	if string(r) != string(a) {
		t.Fatal("same textual identity should be representable in both spaces")
	}
}

func TestRequestTransitionTable(t *testing.T) {
	valid := map[RequestState][]RequestState{
		RequestAdmitted:   {RequestQueued, RequestShed, RequestCancelling, RequestExpired},
		RequestQueued:     {RequestDispatched, RequestCancelling, RequestExpired},
		RequestDispatched: {RequestCompleted, RequestFailed, RequestRetrying, RequestCancelling, RequestExpired},
		RequestRetrying:   {RequestQueued, RequestCompleted, RequestFailed, RequestCancelling, RequestExpired},
		RequestCancelling: {RequestCancelled},
	}
	for from, tos := range valid {
		for _, to := range tos {
			if !CanTransitionRequest(from, to) {
				t.Errorf("%s -> %s must be allowed", from, to)
			}
		}
	}
	illegal := [][2]RequestState{
		{RequestAdmitted, RequestCompleted},
		{RequestAdmitted, RequestFailed},
		{RequestQueued, RequestCompleted},
		{RequestQueued, RequestShed},
		{RequestQueued, RequestFailed},
		{RequestDispatched, RequestShed},
		{RequestRetrying, RequestDispatched},
		{RequestRetrying, RequestShed},
		{RequestCancelling, RequestQueued},
		{RequestCompleted, RequestFailed},
		{RequestExpired, RequestCancelling},
		{RequestCancelled, RequestCompleted},
		{RequestShed, RequestQueued},
	}
	for _, c := range illegal {
		if CanTransitionRequest(c[0], c[1]) {
			t.Errorf("%s -> %s must be rejected", c[0], c[1])
		}
	}
	for _, s := range []RequestState{RequestCompleted, RequestFailed, RequestExpired, RequestCancelled, RequestShed} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
		if CanTransitionRequest(s, s) {
			t.Errorf("terminal state %s must have no outgoing transitions", s)
		}
	}
}

func TestAttemptTransitionTable(t *testing.T) {
	valid := map[AttemptState][]AttemptState{
		AttemptCreated:           {AttemptQueued, AttemptRetracted},
		AttemptQueued:            {AttemptDispatching, AttemptRetracted},
		AttemptDispatching:       {AttemptAssigned, AttemptRetracted},
		AttemptAssigned:          {AttemptRunning, AttemptWaitingConnection},
		AttemptWaitingConnection: {AttemptRunning, AttemptTimedOut, AttemptRetracted},
		AttemptRunning:           {AttemptCompleted, AttemptFailed, AttemptTimedOut},
	}
	for from, tos := range valid {
		for _, to := range tos {
			if !CanTransitionAttempt(from, to) {
				t.Errorf("%s -> %s must be allowed", from, to)
			}
		}
	}
	illegal := [][2]AttemptState{
		{AttemptCreated, AttemptRunning},
		{AttemptQueued, AttemptRunning},
		{AttemptCreated, AttemptTimedOut},
		{AttemptDispatching, AttemptTimedOut},
		{AttemptAssigned, AttemptRetracted}, // executing attempts are not retracted; they are orphaned
		{AttemptWaitingConnection, AttemptFailed},
		{AttemptRunning, AttemptRetracted},
		{AttemptCompleted, AttemptFailed},
		{AttemptTimedOut, AttemptCompleted},
		{AttemptRetracted, AttemptQueued},
	}
	for _, c := range illegal {
		if CanTransitionAttempt(c[0], c[1]) {
			t.Errorf("%s -> %s must be rejected", c[0], c[1])
		}
	}
	for _, s := range []AttemptState{AttemptCompleted, AttemptFailed, AttemptTimedOut, AttemptRetracted} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	if !AttemptWaitingConnection.HoldingWorker() || !AttemptRunning.HoldingWorker() {
		t.Error("assigned-waiting and running attempts hold their worker while pinned for a connection")
	}
	if AttemptQueued.HoldingWorker() {
		t.Error("queued attempts must not hold workers")
	}
}

func TestRetryBudgetTokenBucket(t *testing.T) {
	b := NewRetryBudget(BudgetKey{Tenant: "t1", Upstream: "u1"}, 3, 10*Nanosecond)
	for i := 0; i < 3; i++ {
		if !b.TryConsume(SimTime(i)) {
			t.Fatalf("consume %d of full bucket failed", i)
		}
	}
	if b.TryConsume(SimTime(3)) {
		t.Fatal("empty bucket must refuse consumption")
	}
	if got := b.Available(SimTime(25)); got != 2 {
		t.Fatalf("after 25ns with 10ns refill, tokens = %d, want 2 (refill caps at capacity)", got)
	}
	if b.LastRefill != SimTime(20) {
		t.Fatalf("refill must advance by whole intervals only: LastRefill = %s, want 20ns", b.LastRefill)
	}
	if b.TryConsume(SimTime(25)) != true {
		t.Fatal("consume after refill must succeed")
	}
	if got := b.Available(SimTime(29)); got != 1 {
		t.Fatalf("between refill instants tokens must be stable: got %d, want 1", got)
	}
	if got := b.Available(SimTime(35)); got != 2 {
		t.Fatalf("partial refill: got %d, want 2", got)
	}
}

func TestRetryBudgetWithoutRefill(t *testing.T) {
	b := NewRetryBudget(BudgetKey{Tenant: "t1", Upstream: "u1"}, 1, 0)
	if !b.TryConsume(0) {
		t.Fatal("initial token must be consumable")
	}
	if b.Available(SimTime(1_000_000_000)) != 0 {
		t.Fatal("non-positive refill interval means the bucket never refills")
	}
}

func TestQueueFIFOOrderMembership(t *testing.T) {
	q := NewQueue("t1")
	if err := q.Append(QueueEntry{Attempt: "a", EnqueuedAt: 0, Seq: 1}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Append(QueueEntry{Attempt: "b", EnqueuedAt: 5, Seq: 2}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := q.Append(QueueEntry{Attempt: "a", EnqueuedAt: 6, Seq: 3}); err == nil {
		t.Fatal("duplicate append must be rejected")
	}
	if head, ok := q.Head(); !ok || head.Attempt != "a" {
		t.Fatalf("head = %+v, want attempt a", head)
	}
	if !q.Remove("a") {
		t.Fatal("remove of present entry must succeed")
	}
	if q.Remove("a") {
		t.Fatal("remove of absent entry must report false")
	}
	if q.Contains("a") {
		t.Fatal("a must no longer be a member")
	}
	if head, ok := q.Head(); !ok || head.Attempt != "b" {
		t.Fatalf("after removal head = %+v, want b", head)
	}
}

func TestWorkerPoolFIFOReleaseOrder(t *testing.T) {
	p, err := NewWorkerPool("pool-main", 2)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	w0 := p.FirstIdle()
	if err := p.Take(w0, "att-1"); err != nil {
		t.Fatalf("take: %v", err)
	}
	w1 := p.FirstIdle()
	if w0 == w1 {
		t.Fatal("distinct idle workers must be returned")
	}
	if err := p.Take(w1, "att-2"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if p.HasIdle() || p.FirstIdle() != "" {
		t.Fatal("pool of two with both workers taken must have no idle workers")
	}
	if err := p.Release(w1); err != nil {
		t.Fatalf("release: %v", err)
	}
	if got := p.FirstIdle(); got != w1 {
		t.Fatalf("released worker must head the idle list: got %s want %s", got, w1)
	}
	if err := p.Release(w1); err == nil {
		t.Fatal("double release must be rejected")
	}
	if err := p.Take("pool-main-w-99", "att-2"); err == nil {
		t.Fatal("taking an absent idle worker must be rejected")
	}
}

func TestConnectionPoolWaitlistFIFO(t *testing.T) {
	p, err := NewConnectionPool("u1", 1)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := p.Acquire("att-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if p.HasCapacity() {
		t.Fatal("exhausted pool must report no capacity")
	}
	if err := p.Acquire("att-2"); err == nil {
		t.Fatal("acquire over capacity must be rejected")
	}
	if err := p.EnqueueWaiter("att-2"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := p.EnqueueWaiter("att-2"); err == nil {
		t.Fatal("duplicate waiter must be rejected")
	}
	if err := p.EnqueueWaiter("att-3"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if head, ok := p.PopWaiterHead(); !ok || head != "att-2" {
		t.Fatalf("waitlist head = %s, want att-2", head)
	}
	if !p.RemoveWaiter("att-3") {
		t.Fatal("remove waiter must find att-3")
	}
	if p.RemoveWaiter("att-3") {
		t.Fatal("second remove must report false")
	}
	if err := p.Release("att-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := p.Release("att-1"); err == nil {
		t.Fatal("double release must be rejected")
	}
	if !p.HasCapacity() {
		t.Fatal("released pool must have capacity again")
	}
}

func TestUpstreamExecutionLatencySelection(t *testing.T) {
	u := &Upstream{ID: "u1", Criticality: CriticalityCritical, NormalLatency: 5 * Millisecond, DegradedLatency: 20 * Millisecond}
	if got := u.ExecutionLatency(); got != 5*Millisecond {
		t.Fatalf("healthy latency = %s, want 5ms", got)
	}
	u.Degraded = true
	if got := u.ExecutionLatency(); got != 20*Millisecond {
		t.Fatalf("degraded latency = %s, want 20ms", got)
	}
}

func TestSimulatedTimeArithmetic(t *testing.T) {
	t0 := SimTime(3) * SimTime(Millisecond)
	if t0 != SimTime(3_000_000) {
		t.Fatalf("SimTime construction from duration units failed: %s", t0)
	}
	if got := t0.Add(2 * Millisecond); got != SimTime(5_000_000) {
		t.Fatalf("Add = %s", got)
	}
	if got := t0.Sub(SimTime(1_000_000)); got != 2*Millisecond {
		t.Fatalf("Sub = %s", got)
	}
}

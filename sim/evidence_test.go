package sim

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"sentra/domain"
	"sentra/engine"
)

// evidence_test.go covers the Milestone-1.1 evidence-integrity work: versioned
// typed record serialization, correlation via engine_seq/effect_seq, bounded
// in-memory retention, streaming sinks and the injected/generated ordering
// regression (P2.12).

func evidenceRun(t *testing.T) *Sim {
	t.Helper()
	s := mustNew(t, baseCfg())
	events := arrivals(0, 2, t1, u1, long, 0)
	events = append(events, NewCancellationEvent(1*msT, "req-2", "evidence test"))
	events = append(events, NewUpstreamDegradedEvent(4*msT, u2))
	run(t, s, events, 12*msT)
	return s
}

// --- schema version, ordering and gaplessness --------------------------------

func TestRecordSchemaIsVersionedAndGaplessOrdering(t *testing.T) {
	s := evidenceRun(t)
	records := s.Recorder().Records()
	if len(records) == 0 {
		t.Fatal("scenario must record evidence")
	}
	for i, rec := range records {
		if rec.SchemaVersion != SchemaVersion {
			t.Fatalf("record %d schema = %d, want %d", i, rec.SchemaVersion, SchemaVersion)
		}
		if rec.EffectSeq != uint64(i) {
			t.Fatalf("record %d effect_seq = %d: sequence must be gapless and zero-based", i, rec.EffectSeq)
		}
		if rec.Category != CategoryEvent && rec.Category != CategoryNote {
			t.Fatalf("record %d category = %q, want event or note", i, rec.Category)
		}
		if rec.SimTimeNS < 0 {
			t.Fatalf("record %d at negative simulated time", i)
		}
	}
	// First record: the first injected arrival, engine_seq 0.
	if records[0].Category != CategoryEvent || records[0].Kind != "RequestArrival" || records[0].EngineSeq != 0 {
		t.Fatalf("first record = %+v, want the arrival event at engine_seq 0", records[0])
	}
}

// --- N:1 correlation of effects to the originating engine event --------------

func TestEffectsCorrelateWithOriginatingEngineEvent(t *testing.T) {
	s := evidenceRun(t)
	records := s.Recorder().Records()

	// Find the CancellationRequested engine event record.
	var cancelSeq uint64
	found := false
	for _, rec := range records {
		if rec.Category == CategoryEvent && rec.Kind == "CancellationRequested" {
			cancelSeq, found = rec.EngineSeq, true
			break
		}
	}
	if !found {
		t.Fatal("cancellation event must be recorded as an event record")
	}

	// All effects produced while handling that event share its engine_seq.
	effects := 0
	for _, rec := range records {
		if rec.EngineSeq == cancelSeq {
			if rec.Category != CategoryEvent && rec.Category != CategoryNote {
				t.Fatalf("record %+v has invalid category", rec)
			}
			effects++
		}
	}
	if effects < 2 {
		t.Fatalf("one engine event produced %d records; the event→effect mapping must be N:1 (≥2 here: the event line itself plus handler notes)", effects)
	}
	// The ack must be a distinct effect of a later engine event.
	for _, rec := range records {
		if rec.Kind == "CancellationAcknowledged" && rec.EngineSeq == cancelSeq {
			t.Fatal("the acknowledgement event has its own engine seq; it must not share the request's")
		}
	}
}

// --- explicit typed serialization --------------------------------------------

func TestEventRecordsAreTypedNotStructDumps(t *testing.T) {
	s := evidenceRun(t)
	for _, rec := range s.Recorder().Records() {
		if rec.Category != CategoryEvent {
			continue
		}
		desc := rec.Detail.Description
		switch rec.Kind {
		case "RequestArrival":
			if !strings.Contains(desc, "upstream=u1") || strings.HasPrefix(desc, "{") {
				t.Fatalf("arrival detail must be typed, not a %v dump: %q", desc, desc)
			}
		case "LoadShed":
			t.Fatal("scenario must not shed; assertion below pins typed load-shed separately")
		}
	}
}

func TestLoadShedRecordCarriesRequestAndReason(t *testing.T) {
	cfg := baseCfg()
	cfg.Tenants[t1] = TenantConfig{MaxArrivalQueueDepth: 0, MaxRetryQueueDepth: 0, MaxTotalQueueDepth: 0}
	s := mustNew(t, cfg)
	e := engine.New(s, arrivals(0, 1, t1, u1, long, 0))
	e.Run(5 * msT)
	var shedRecord *Record
	records := s.Recorder().Records()
	for i := range records {
		if records[i].Kind == "LoadShed" {
			shedRecord = &records[i]
		}
	}
	if shedRecord == nil {
		t.Fatal("queue depth 0 must shed the arrival")
	}
	if shedRecord.Detail.Request != "req-1" || shedRecord.Detail.Reason != "tenant-queue-full" {
		t.Fatalf("shed record = %+v, want request=req-1 reason=tenant-queue-full", shedRecord.Detail)
	}
}

// --- JSONL serialization round trip and reconstruction ------------------------

func TestRecordJSONRoundTripThroughJSONLSink(t *testing.T) {
	build := func() []engine.Event {
		events := arrivals(0, 2, t1, u1, long, 0)
		events = append(events, NewCancellationEvent(1*msT, "req-2", "roundtrip"))
		events = append(events, NewUpstreamDegradedEvent(4*msT, u2))
		return events
	}

	// Run A: default in-memory ring.
	a := mustNew(t, baseCfg())
	eA := engine.New(a, build())
	eA.Run(12 * msT)

	// Run B: streaming JSONL sink over a buffer.
	b := mustNew(t, baseCfg())
	var buf bytes.Buffer
	jsonl := NewJSONLSinkWriter(&buf)
	b.rec = NewRecorder(jsonl)
	eB := engine.New(b, build())
	eB.Run(12 * msT)
	if err := jsonl.Close(); err != nil {
		t.Fatalf("jsonl close: %v", err)
	}

	// Reconstruct records line by line and compare with run A.
	var got []Record
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("jsonl line decode: %v (%s)", err, line)
		}
		got = append(got, rec)
	}
	if !reflect.DeepEqual(got, a.Recorder().Records()) {
		t.Fatalf("JSONL round trip must reconstruct the identical record list: got %d records, run A has %d",
			len(got), a.Recorder().Len())
	}
	// JSON key set is the versioned contract: pinned to catch accidental shape changes.
	firstLine := strings.SplitN(buf.String(), "\n", 2)[0]
	for _, key := range []string{`"effect_seq":0`, `"schema_version":1`, `"engine_seq":`, `"sim_time_ns":`, `"category":`, `"kind":`} {
		if !strings.Contains(firstLine, key) {
			t.Fatalf("record JSON must contain %s: %s", key, firstLine)
		}
	}
}

// --- bounded in-memory test retention -----------------------------------------

func TestRingSinkRetentionIsBoundedAndOrderPreserving(t *testing.T) {
	s := mustNew(t, baseCfg())
	small, err := NewRingMemorySink(4)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	s.rec = NewRecorder(small)
	e := engine.New(s, arrivals(0, 3, t1, u1, long, 0))
	e.Run(15 * msT)

	records := s.Recorder().Records()
	if len(records) != 4 {
		t.Fatalf("retained records = %d, want the 4-window capacity", len(records))
	}
	for i := 1; i < len(records); i++ {
		if records[i].EffectSeq != records[i-1].EffectSeq+1 {
			t.Fatalf("eviction must preserve effect_seq contiguity: %d -> %d", records[i-1].EffectSeq, records[i].EffectSeq)
		}
	}
	if small.Start() != records[0].EffectSeq {
		t.Fatalf("ring Start() = %d, oldest retained = %d", small.Start(), records[0].EffectSeq)
	}
	// Eviction drops only the visible window: the total number of records
	// written by the run strictly exceeds the window (the scenario produces
	// far more than 4 records), proving retention is bounded without
	// perturbing numbering — covered further by TestEvictionDoesNotPerturbEffectSeq.
}

func TestEvictionDoesNotPerturbEffectSeq(t *testing.T) {
	small, err := NewRingMemorySink(2)
	if err != nil {
		t.Fatalf("ring: %v", err)
	}
	r := NewRecorder(small)
	r.beginEvent(7)
	r.Logf(0, "note-a", "first")
	r.Logf(0, "note-b", "second")
	r.Logf(0, "note-c", "third")
	recs := r.Records()
	if len(recs) != 2 {
		t.Fatalf("retained = %d, want 2", len(recs))
	}
	if recs[0].EffectSeq != 1 || recs[1].EffectSeq != 2 {
		t.Fatalf("retained seqs = %d,%d; want 1,2 (note-a evicted, numbering continues)", recs[0].EffectSeq, recs[1].EffectSeq)
	}
	if small.Start() != 1 {
		t.Fatalf("Start() = %d, want 1", small.Start())
	}
}

// --- sticky-error policy on sink failure --------------------------------------

type failingSink struct{ err error }

func (f *failingSink) Write(Record) error { return f.err }
func (f *failingSink) Close() error       { return f.err }

func TestRecorderPanicsOnSinkWriteFailure(t *testing.T) {
	r := NewRecorder(&failingSink{err: contextError()})
	r.beginEvent(0)
	defer func() {
		if rc := recover(); rc == nil {
			t.Fatal("sink write failure must panic (sticky-error policy)")
		}
	}()
	r.Logf(0, "note", "boom")
}

func contextError() error {
	return &errSink{msg: "disk full"}
}

type errSink struct{ msg string }

func (e *errSink) Error() string { return "sink: " + e.msg }

// --- P2.12: generated events obey priority, not injection order ---------------

type orderEvent struct {
	id       string
	t        domain.SimTime
	priority int
}

func (e *orderEvent) At() domain.SimTime { return e.t }
func (e *orderEvent) Priority() int      { return e.priority }

type orderHandler struct {
	trace []string
	outs  map[string][]engine.Event
}

func (h *orderHandler) Handle(ev engine.Event, seq uint64) []engine.Event {
	_ = seq
	oe := ev.(*orderEvent)
	h.trace = append(h.trace, oe.id)
	return h.outs[oe.id]
}

func TestGeneratedEventsObeyPriorityNotGenerationOrder(t *testing.T) {
	// Injected at t=5: governance (10, seq 0) and work (30, seq 1). The first
	// generated event's handler emits [gwork (30), ggov (10)] — gwork is
	// generated first and gets the lower seq of the two, but both are
	// ordered by (time, priority, seq) with the freshly generated pair:
	//	ggov outranks iw   (priority 10 < 30, despite generating later)
	//	iw   outranks gwork (same priority 30, earlier scheduling seq)
	// Expected: igov, ggov, iw, gwork.
	ih := &orderHandler{outs: map[string][]engine.Event{}}
	e := engine.New(ih, []engine.Event{
		&orderEvent{id: "igov", t: 5 * msT, priority: PriorityGovernance},
		&orderEvent{id: "iw", t: 5 * msT, priority: PriorityWork},
	})
	// Script the generated children of the first processed event.
	ih.outs["igov"] = []engine.Event{
		&orderEvent{id: "gwork", t: 5 * msT, priority: PriorityWork},
		&orderEvent{id: "ggov", t: 5 * msT, priority: PriorityGovernance},
	}
	e.Run(5 * msT)
	want := []string{"igov", "ggov", "iw", "gwork"}
	if !reflect.DeepEqual(ih.trace, want) {
		t.Fatalf("execution order = %v, want %v (generated events obey priority and seq, refuting the injected-precedence reading)", ih.trace, want)
	}
}

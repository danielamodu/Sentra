package sim

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"

	"sentra/domain"
)

// SchemaVersion is the version of the record schema written by this package.
// Consumers must treat the integer as part of the contract: field meaning is
// only defined for the version the reader was built against.
const SchemaVersion = 1

// defaultRingCapacity bounds the in-memory sink used by default. It keeps
// ordinary test runs fully in memory while guaranteeing that pathological
// scenarios cannot grow the process unboundedly; older records are evicted
// first. effect_seq numbering is unaffected by eviction (see Records).
const defaultRingCapacity = 8192

// Categories. Every record is exactly one of these.
const (
	// CategoryEvent records the processing-trigger itself: one record per
	// engine event as it arrives at the handler. Multiple effects (notes)
	// typically belong to one event — the correspondence is N:1, never 1:1.
	CategoryEvent = "event"
	// CategoryNote records one explicit effect the handler applied (a state
	// transition, a skip, a shed decision, ...).
	CategoryNote = "note"
)

// Detail is the typed, explicitly serialized payload of a record. Only known
// fields exist; descriptive prose lives in Description. This replaces the
// previous free-form printf message: serialization is typed, so records can be
// round-tripped and machine-processed without parsing human text.
type Detail struct {
	Request  string `json:"request,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	Tenant   string `json:"tenant,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Worker   string `json:"worker,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Description carries the human-readable summary. It is built from
	// deterministic, typed fragments — never a raw %v dump of an event struct.
	Description string `json:"description,omitempty"`
	// Lane names the admission lane of an attempt-created effect (D1 additive
	// evidence, Milestone 3): "arrival" or "retry". Empty for other notes.
	Lane string `json:"lane,omitempty"`
	// Typed effect payloads added for M3 P3. They are additive: older evidence
	// simply leaves them empty, and the verifier never parses prose to recover
	// them.
	DeadlineNS  int64  `json:"deadline_ns,omitempty"`
	Decision    string `json:"decision,omitempty"`
	Predecessor string `json:"predecessor,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	Count       int    `json:"count,omitempty"`
	LatencyNS   int64  `json:"latency_ns,omitempty"`
	// Seq is the queue entry's global enqueue sequence number (queue effects).
	Seq uint64 `json:"seq,omitempty"`
	// MaxRetries is the logical retry limit declared at arrival. It is a
	// pointer so an absent declaration (nil) is distinguishable from a real
	// declared limit of zero.
	MaxRetries *int `json:"max_retries,omitempty"`
}

// retriesPtr boxes a declared retry limit so the typed detail records the
// declaration as present even when the limit is zero.
func retriesPtr(n int) *int { return &n }

// Record is one entry of the evidence ledger.
//
// Correlation contract (docs/ENGINE_SEMANTICS.md §5): EngineSeq identifies the
// originating engine event (one processing step) — all records written while
// handling that event share it. EffectSeq is a run-global monotonic counter
// over records; several effect_seq values may share one engine_seq, and an
// event line itself is one of those effects. The mapping from engine events to
// effects is N:1 by design.
type Record struct {
	// SchemaVersion equals SchemaVersion at write time.
	SchemaVersion int `json:"schema_version"`
	// EffectSeq is unique and gapless across all records of a run.
	EffectSeq uint64 `json:"effect_seq"`
	// EngineSeq is the scheduling sequence number of the event being handled.
	EngineSeq uint64 `json:"engine_seq"`
	// SimTimeNS is the simulated instant of the event, in nanoseconds.
	SimTimeNS int64 `json:"sim_time_ns"`
	// Category is "event" or "note".
	Category string `json:"category"`
	// Kind is the event kind name for events, or a short note tag for notes.
	Kind string `json:"kind"`
	// Subject names the primary entity (best effort, see marshalSubject).
	Subject string `json:"subject,omitempty"`
	// Detail carries the typed payload.
	Detail Detail `json:"detail"`
}

// JSON returns the canonical one-line JSON encoding of the record.
func (rec Record) JSON() (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("sim: record marshal failed: %w", err)
	}
	return string(b), nil
}

// Sink is the streaming destination of records. Sinks must not observe the
// wall clock and must not reorder: Write is called in effect_seq order.
type Sink interface {
	Write(rec Record) error
	// Close flushes and releases the sink (no-op for pure-memory sinks).
	Close() error
}

// RingMemorySink keeps the most recent `capacity` records in memory and evicts
// the oldest ones beyond that. Eviction never perturbs effect_seq numbering;
// it only limits the visible window, which keeps bounded-memory test runs
// cheap without falsifying correlation.
type RingMemorySink struct {
	capacity int
	records  []Record
	start    uint64 // effect_seq of records[0]
}

// NewRingMemorySink creates a ring sink with the given capacity (>0). A
// capacity of 0 is an error; use NewDefaultRingSink for the default.
func NewRingMemorySink(capacity int) (*RingMemorySink, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("sim: ring sink capacity must be positive, got %d", capacity)
	}
	return &RingMemorySink{capacity: capacity}, nil
}

// NewDefaultRingSink returns the standard in-memory sink (capacity
// defaultRingCapacity).
func NewDefaultRingSink() *RingMemorySink {
	r, _ := NewRingMemorySink(defaultRingCapacity)
	return r
}

func (s *RingMemorySink) Write(rec Record) error {
	if len(s.records) == s.capacity {
		s.records = s.records[1:]
		s.start++
	}
	s.records = append(s.records, rec)
	return nil
}

// Close implements Sink; a memory sink has nothing to flush.
func (s *RingMemorySink) Close() error { return nil }

// Records returns a copy of the retained records in effect order. The first
// retained record has effect_seq == s.Start().
func (s *RingMemorySink) Records() []Record {
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

// Start reports the effect_seq of the oldest retained record.
func (s *RingMemorySink) Start() uint64 { return s.start }

// JSONLSink streams records as one JSON object per line. It honors the
// io.Closer contract: no record is durable until Close succeeds.
type JSONLSink struct {
	w    *bufio.Writer
	own  io.Closer
	err  error
	line int
}

// NewJSONLSinkFile writes to a file (created or truncated).
func NewJSONLSinkFile(path string) (*JSONLSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("sim: jsonl sink open %s: %w", path, err)
	}
	return &JSONLSink{w: bufio.NewWriter(f), own: f}, nil
}

// NewJSONLSinkWriter writes to an existing writer (useful for tests with a
// bytes.Buffer; the caller drives flushing via Close).
func NewJSONLSinkWriter(w io.Writer) *JSONLSink {
	return &JSONLSink{w: bufio.NewWriter(w)}
}

func (s *JSONLSink) Write(rec Record) error {
	if s.err != nil {
		return s.err
	}
	line, err := rec.JSON()
	if err != nil {
		s.err = err
		return err
	}
	if _, err := s.w.WriteString(line + "\n"); err != nil {
		s.err = err
		return err
	}
	s.line++
	return nil
}

// Close flushes buffered lines and releases an owned file. It does not fail on
// zero records.
func (s *JSONLSink) Close() error {
	if err := s.w.Flush(); err != nil && s.err == nil {
		s.err = err
	}
	if s.own != nil {
		if err := s.own.Close(); err != nil && s.err == nil {
			s.err = err
		}
	}
	return s.err
}

// Recorder is the append-only evidence ledger of the reference system. Every
// processed engine event is recorded once (Category "event", typed
// serialization via marshalEvent), and every explicit effect the handler
// applies is recorded as a note (Category "note").
//
// Records depend only on simulated time, event payloads and entity IDs — never
// on wall-clock time or map iteration order — so two identical runs produce
// identical record lists.
//
// Error policy: a sink write failure is sticky and panics at the recording
// site immediately. A partially flushed trace must never be observable as a
// complete one: recording failures are programming/stream failures, not
// runtime conditions the simulation may continue past.
type Recorder struct {
	sink      Sink
	next      uint64 // next effect_seq
	curEngine uint64 // scheduling seq of the event currently being handled
	total     int    // records written by this run, including evicted ones
	digest    hash.Hash
}

// NewRecorder builds a recorder over the given sink. A nil sink selects the
// default bounded in-memory ring (see defaultRingCapacity).
func NewRecorder(sink Sink) *Recorder {
	if sink == nil {
		sink = NewDefaultRingSink()
	}
	return &Recorder{sink: sink, digest: sha256.New()}
}

// beginEvent pins the engine sequence of the event being handled; all records
// until the next beginEvent share that engine_seq.
func (r *Recorder) beginEvent(seq uint64) { r.curEngine = seq }

// base stamps a record with the current run position.
func (r *Recorder) base() Record {
	return Record{
		SchemaVersion: SchemaVersion,
		EffectSeq:     r.next,
		EngineSeq:     r.curEngine,
	}
}

// write stamps, numbers and stores one record. Write failures panic
// (sticky-error policy, above).
func (r *Recorder) write(rec Record) {
	rec.SchemaVersion = SchemaVersion
	rec.EffectSeq = r.next
	rec.EngineSeq = r.curEngine
	r.next++
	if err := r.sink.Write(rec); err != nil {
		panic(fmt.Sprintf("sim: recorder sink write failed (sticky): %v", err))
	}
	// The terminal digest covers every record the run wrote, including records
	// the bounded sink later evicts. It is updated only after a successful
	// write, so a failed run can never attest a complete sequence.
	writeDigestRecord(r.digest, rec)
	r.total++
}

// TotalRecords reports how many records the run wrote, including records the
// sink no longer retains.
func (r *Recorder) TotalRecords() int { return r.total }

// LastEffectSeq reports the effect_seq of the last record written. It is only
// meaningful when TotalRecords() > 0.
func (r *Recorder) LastEffectSeq() uint64 {
	if r.total == 0 {
		return 0
	}
	return r.next - 1
}

// RecordsDigest is the canonical digest of the full record sequence written by
// this run. verify.TerminalRecordsDigest recomputes the same value.
func (r *Recorder) RecordsDigest() string {
	return "sha256:" + hex.EncodeToString(r.digest.Sum(nil))
}

// LogEvent records one event line: Category "event", Kind set to the event's
// kind name, Subject and Detail produced by the explicit typed switch
// (marshalSubject / marshalEvent — never a %v structural dump).
func (r *Recorder) LogEvent(ev Event) {
	kind := ev.Kind()
	rec := r.base()
	rec.Category = CategoryEvent
	rec.Kind = kind.String()
	rec.SimTimeNS = int64(ev.At())
	rec.Subject = marshalSubject(kind, ev)
	rec.Detail = marshalEvent(kind, ev)
	r.write(rec)
}

// Log records one explicit effect as a note. kind is a short tag (for example
// a transition category); detail carries the typed fragments.
func (r *Recorder) Log(t domain.SimTime, kind string, detail Detail) {
	rec := r.base()
	rec.Category = CategoryNote
	rec.Kind = kind
	rec.SimTimeNS = int64(t)
	rec.Detail = detail
	r.write(rec)
}

// Logf records one effect as a note with a formatted description (convenience
// for the free-text fragments; the printed values are always IDs or durations,
// never raw structs).
func (r *Recorder) Logf(t domain.SimTime, kind string, format string, args ...any) {
	rec := r.base()
	rec.Category = CategoryNote
	rec.Kind = kind
	rec.SimTimeNS = int64(t)
	rec.Detail = Detail{Description: fmt.Sprintf(format, args...)}
	r.write(rec)
}

// Records returns the records retained by the sink. For the default ring this
// is the bounded window; for a streaming sink there is nothing to return —
// read the stream instead. Records are in effect_seq order.
func (r *Recorder) Records() []Record {
	if ring, ok := r.sink.(*RingMemorySink); ok {
		return ring.Records()
	}
	return nil
}

// Len reports the number of records currently retained (see Records for the
// visibility caveat of streaming sinks).
func (r *Recorder) Len() int { return len(r.Records()) }

// Close flushes and closes the sink. After Close the recorder must not be
// used for further recording.
func (r *Recorder) Close() error { return r.sink.Close() }

// marshalSubject names the primary entity of an event, by kind. The event
// payload always decides; derived values (attempt id of the oldest waiter at
// grant time) are decided at processing time and become note subjects instead.
func marshalSubject(kind Kind, ev Event) string {
	switch e := ev.(type) {
	case *EventRequestArrival:
		return string(e.Tenant)
	case *EventCancellationRequested:
		return string(e.Request)
	case *EventUpstreamDegraded:
		return string(e.Upstream)
	case *EventUpstreamRecovered:
		return string(e.Upstream)
	case *EventAdmissionDecided:
		return string(e.Request)
	case *EventQueueInsertion:
		return string(e.Attempt)
	case *EventQueueRemoval:
		return string(e.Attempt)
	case *EventWorkerAssignment:
		return string(e.Attempt)
	case *EventExecutionStart:
		return string(e.Attempt)
	case *EventExecutionCompletion:
		return string(e.Attempt)
	case *EventTimeout:
		return string(e.Attempt)
	case *EventCancellationAcknowledged:
		return string(e.Request)
	case *EventRetryScheduled:
		return string(e.Request)
	case *EventRetryExecution:
		return string(e.Request)
	case *EventDeadlineExpired:
		return string(e.Request)
	case *EventBreakerTransitioned:
		return string(e.Upstream)
	case *EventLoadShed:
		return string(e.Request)
	case *EventConnectionGranted:
		return string(e.Upstream)
	case *EventUpstreamLatencySet:
		return string(e.Upstream)
	case *EventUpstreamFailureSet:
		return string(e.Upstream)
	default:
		return ""
	}
}

// marshalEvent serializes an event payload explicitly, kind by kind. Any new
// event kind must extend this switch (enforced by the exhaustive `Kind()`
// coverage in sim tests); the compiler enforces the concrete-type switch
// itself, because a default that panics keeps unknown kinds from silting.
func marshalEvent(kind Kind, ev Event) Detail {
	switch e := ev.(type) {
	case *EventRequestArrival:
		return Detail{
			Tenant:      string(e.Tenant),
			Upstream:    string(e.Upstream),
			DeadlineNS:  int64(e.Deadline),
			Description: fmt.Sprintf("tenant=%s upstream=%s deadline=%s maxRetries=%d", e.Tenant, e.Upstream, e.Deadline, e.MaxRetries),
		}
	case *EventCancellationRequested:
		return Detail{Request: string(e.Request), Reason: e.Reason, Description: fmt.Sprintf("request=%s reason=%s", e.Request, e.Reason)}
	case *EventUpstreamDegraded:
		return Detail{Upstream: string(e.Upstream), Description: fmt.Sprintf("upstream=%s", e.Upstream)}
	case *EventUpstreamRecovered:
		return Detail{Upstream: string(e.Upstream), Description: fmt.Sprintf("upstream=%s", e.Upstream)}
	case *EventAdmissionDecided:
		decision := "shed"
		if e.Admitted {
			decision = "admit"
		}
		return Detail{
			Request:     string(e.Request),
			Tenant:      string(e.Tenant),
			Upstream:    string(e.Upstream),
			Reason:      e.Reason,
			Decision:    decision,
			Description: fmt.Sprintf("request=%s admitted=%t reason=%s", e.Request, e.Admitted, e.Reason),
		}
	case *EventQueueInsertion:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventQueueRemoval:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventWorkerAssignment:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventExecutionStart:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventExecutionCompletion:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventTimeout:
		return Detail{Attempt: string(e.Attempt), Description: fmt.Sprintf("attempt=%s", e.Attempt)}
	case *EventCancellationAcknowledged:
		return Detail{Request: string(e.Request), Description: fmt.Sprintf("request=%s outstanding=%d", e.Request, e.Outstanding)}
	case *EventRetryScheduled:
		return Detail{
			Request:     string(e.Request),
			Predecessor: string(e.Predecessor),
			Reason:      e.Reason,
			Description: fmt.Sprintf("request=%s predecessor=%s reason=%s", e.Request, e.Predecessor, e.Reason),
		}
	case *EventRetryExecution:
		return Detail{Request: string(e.Request), Description: fmt.Sprintf("request=%s", e.Request)}
	case *EventDeadlineExpired:
		return Detail{Request: string(e.Request), Description: fmt.Sprintf("request=%s", e.Request)}
	case *EventBreakerTransitioned:
		return Detail{
			Upstream:    string(e.Upstream),
			From:        e.From.String(),
			To:          e.To.String(),
			Description: fmt.Sprintf("upstream=%s %s -> %s (%s)", e.Upstream, e.From, e.To, e.Reason),
		}
	case *EventLoadShed:
		return Detail{Request: string(e.Request), Reason: e.Reason, Description: fmt.Sprintf("request=%s reason=%s", e.Request, e.Reason)}
	case *EventConnectionGranted:
		return Detail{Upstream: string(e.Upstream), Description: fmt.Sprintf("upstream=%s (waiter selected at processing time)", e.Upstream)}
	case *EventUpstreamLatencySet:
		decision := "set"
		if e.Latency == 0 {
			decision = "clear"
		}
		if e.Latency == 0 {
			return Detail{Upstream: string(e.Upstream), Decision: decision, Description: fmt.Sprintf("upstream=%s latency override cleared", e.Upstream)}
		}
		return Detail{Upstream: string(e.Upstream), Decision: decision, LatencyNS: int64(e.Latency), Description: fmt.Sprintf("upstream=%s latency overridden to %s (completions unaffected)", e.Upstream, e.Latency)}
	case *EventUpstreamFailureSet:
		decision := "set"
		if e.EveryN == 0 {
			decision = "clear"
		}
		if e.EveryN == 0 {
			return Detail{Upstream: string(e.Upstream), Decision: decision, Description: fmt.Sprintf("upstream=%s failure mode restored to completion-time-only", e.Upstream)}
		}
		return Detail{Upstream: string(e.Upstream), Decision: decision, Count: e.EveryN, Description: fmt.Sprintf("upstream=%s every %dth completion forced to fail", e.Upstream, e.EveryN)}
	default:
		panic(fmt.Sprintf("sim: marshalEvent: unhandled event kind %s", kind))
	}
}

// writeDigestRecord appends one record to the terminal digest using the exact
// length-prefixed encoding implemented by verify.TerminalRecordsDigest. The two
// implementations are independent (sim never imports verify), and
// evidence/convert_test.go asserts they agree for a real run.
func writeDigestRecord(h hash.Hash, r Record) {
	fields := []string{
		strconv.Itoa(r.SchemaVersion),
		strconv.FormatUint(r.EffectSeq, 10),
		strconv.FormatUint(r.EngineSeq, 10),
		strconv.FormatInt(r.SimTimeNS, 10),
		r.Category,
		r.Kind,
		r.Subject,
		r.Detail.Request,
		r.Detail.Attempt,
		r.Detail.Tenant,
		r.Detail.Upstream,
		r.Detail.Worker,
		r.Detail.From,
		r.Detail.To,
		r.Detail.Reason,
		r.Detail.Lane,
		r.Detail.Description,
		strconv.FormatInt(r.Detail.DeadlineNS, 10),
		r.Detail.Decision,
		r.Detail.Predecessor,
		r.Detail.Outcome,
		strconv.Itoa(r.Detail.Count),
		strconv.FormatInt(r.Detail.LatencyNS, 10),
		strconv.FormatUint(r.Detail.Seq, 10),
		digestIntField(r.Detail.MaxRetries),
	}
	for _, f := range fields {
		fmt.Fprintf(h, "%d:%s", len(f), f)
	}
	h.Write([]byte{0x1e})
}

// digestIntField encodes an optional integer detail field. Absence has its own
// token so a missing declaration can never be hashed as a declared zero.
func digestIntField(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

// FormatRecords renders records as a compact line-oriented trace, useful for
// debugging and for diffing two runs. It replaces FormatEntries.
//
// engine_seq is rendered per record (not only on event lines) so effects can
// always be traced back to their originating engine event.
func FormatRecords(records []Record) string {
	var b strings.Builder
	for _, rec := range records {
		desc := rec.Detail.Description
		if desc == "" {
			if sub, err := json.Marshal(rec.Detail); err == nil {
				desc = string(sub)
			}
		}
		fmt.Fprintf(&b, "%04d engine=%d t=%-12s %-26s %s\n",
			rec.EffectSeq, rec.EngineSeq, domain.SimTime(rec.SimTimeNS), rec.Kind, desc)
	}
	return b.String()
}

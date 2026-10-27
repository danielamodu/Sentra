package verify

import "fmt"

// Evidence validation codes. They are stable, machine-readable identifiers used
// by verdicts and counterexamples; docs/VERIFIER_DESIGN.md lists their meaning.
const (
	CodeEvidenceSchemaVersion = "evidence.schema_version"
	CodeEvidenceEmpty         = "evidence.empty"
	CodeRecordSchemaVersion   = "record.schema_version"
	CodeEffectSeqMismatch     = "evidence.effect_seq_mismatch"
	CodeRecordCategory        = "record.category"
	CodeEventKind             = "record.event_kind"
	CodeNoteKind              = "record.note_kind"
	CodeTimeNegative          = "evidence.time_negative"
	CodeTimeOrder             = "evidence.time_order"
	CodeBlockTimeMismatch     = "evidence.block_time_mismatch"
	CodeBlockMissingEvent     = "evidence.block_missing_event"
	CodeBlockMultipleEvents   = "evidence.block_multiple_events"
	CodeBlockEventNotFirst    = "evidence.block_event_not_first"
	CodeBlockInterleaved      = "evidence.engine_seq_interleaved"
)

// EventKinds is the closed set of reference event kinds (docs/ENGINE_SEMANTICS.md
// §7). An evidence stream naming any other kind is not verifiable: the verifier
// cannot claim to understand an event it was never told about.
var EventKinds = map[string]bool{
	"RequestArrival":           true,
	"AdmissionDecided":         true,
	"QueueInsertion":           true,
	"QueueRemoval":             true,
	"WorkerAssignment":         true,
	"ExecutionStart":           true,
	"ExecutionCompletion":      true,
	"Timeout":                  true,
	"CancellationRequested":    true,
	"CancellationAcknowledged": true,
	"RetryScheduled":           true,
	"RetryExecution":           true,
	"DeadlineExpired":          true,
	"UpstreamDegraded":         true,
	"UpstreamRecovered":        true,
	"BreakerTransitioned":      true,
	"LoadShed":                 true,
	"ConnectionGranted":        true,
	"UpstreamLatencySet":       true,
	"UpstreamFailureSet":       true,
}

// NoteKinds is the closed set of reference note tags. Notes are effects within a
// handler invocation, not events.
var NoteKinds = map[string]bool{
	"admission":          true,
	"attempt-created":    true,
	"attempt-transition": true,
	"breaker":            true,
	"budget":             true,
	"cancellation":       true,
	"connection":         true,
	"fault":              true,
	"orphan":             true,
	"outcome":            true,
	"probe-released":     true,
	"probe-reserved":     true,
	"queue":              true,
	"request-created":    true,
	"request-transition": true,
	"retry-admission":    true,
	"skip":               true,
	"upstream":           true,
	"worker":             true,
}

// Block is one engine processing step: the event record (absent only when the
// ledger head was truncated) plus every note written while handling it. The
// engine-to-effect relationship is N:1 by contract.
type Block struct {
	EngineSeq uint64
	TimeNS    int64
	// First is the first retained record of the block, in evidence order.
	First Record
	// Event is the trigger record, nil when the head of a truncated ledger
	// evicted it.
	Event *Record
	// EventCount is the number of event-category records in the block.
	EventCount int
	Notes      []Record
	// Partial marks a block whose beginning was evicted from the retained
	// window: its event record is legitimately missing.
	Partial bool
}

// HistoryCoverage states what the evidence contract can prove about a ledger's
// completeness. It is deliberately three-valued: "not known to be truncated"
// is not the same as "proven complete".
type HistoryCoverage string

const (
	// HistoryHeadTruncated means the retained window starts after effect_seq 0:
	// records before the window were evicted and cannot be recovered.
	HistoryHeadTruncated HistoryCoverage = "head-truncated"
	// HistoryTerminalUnestablished means the head is complete but the evidence
	// carries no marker proving that the retained tail reaches the run's
	// terminal boundary. Contiguity of the retained records is not proof of
	// that boundary.
	HistoryTerminalUnestablished HistoryCoverage = "terminal-unestablished"
	// HistoryComplete means both the head and the terminal boundary are proven.
	// It is reachable only from evidence carrying a valid terminal attestation
	// (see TerminalAttestation); it is never inferred.
	HistoryComplete HistoryCoverage = "complete"
)

// classifyCoverage combines the two independent completeness proofs. The head
// proof comes from FirstEffectSeq; the terminal proof comes only from a
// validated attestation.
func classifyCoverage(partial, terminalProven bool) HistoryCoverage {
	if partial {
		return HistoryHeadTruncated
	}
	if terminalProven {
		return HistoryComplete
	}
	return HistoryTerminalUnestablished
}

// classifyHistory derives head coverage from the evidence contract alone. It
// deliberately knows nothing about the tail: a caller must not read it as
// terminal completeness.
func classifyHistory(firstEffectSeq uint64) HistoryCoverage {
	if firstEffectSeq > 0 {
		return HistoryHeadTruncated
	}
	return HistoryTerminalUnestablished
}

// headComplete reports whether the retained window begins at the initial
// boundary. It says nothing about the tail.
func (c HistoryCoverage) headComplete() bool {
	return c == HistoryTerminalUnestablished || c == HistoryComplete
}

// terminalComplete reports whether the retained window is proven to reach the
// run's terminal boundary. It says nothing about the head.
func (c HistoryCoverage) terminalComplete() bool {
	return c == HistoryComplete
}

// Ledger is structurally validated evidence. It preserves evidence order; it
// never reorders records to make them validate.
type Ledger struct {
	SchemaVersion  int
	FirstEffectSeq uint64
	// Partial is true when the retained window does not start at effect_seq 0.
	// Truncation is a legal evidence state, not a defect; properties that need
	// the missing history must be INCONCLUSIVE (D5).
	Partial bool
	// Coverage records exactly what completeness the evidence proves. The head
	// and tail are independent: a head-truncated ledger can have a proven tail
	// (TerminalProven true) and still never claims HistoryComplete.
	Coverage HistoryCoverage
	// TerminalProven is true only when a validated terminal attestation binds
	// the retained tail to the attested final effect.
	TerminalProven bool
	Blocks         []Block
	Records        int
}

// headComplete reports whether the retained window begins at the initial
// boundary. It says nothing about the tail.
func (l *Ledger) headComplete() bool {
	return l != nil && l.Coverage.headComplete()
}

// terminalComplete reports whether the retained window is proven to reach the
// run's terminal boundary. Coverage == HistoryComplete is accepted for
// synthetic test fixtures that carry no separate flag.
func (l *Ledger) terminalComplete() bool {
	return l != nil && (l.TerminalProven || l.Coverage == HistoryComplete)
}

// ValidateEvidence performs stage 3 of the pipeline: schema, sequence, ordering
// and correlation validation. It returns the validated ledger and every finding;
// a non-empty finding list means the evidence is unusable as a whole
// (INVALID_INPUT), not merely incomplete.
func ValidateEvidence(ev Evidence) (Ledger, []Finding) {
	led := Ledger{
		SchemaVersion:  ev.SchemaVersion,
		FirstEffectSeq: ev.FirstEffectSeq,
		Partial:        ev.FirstEffectSeq > 0,
		Coverage:       classifyHistory(ev.FirstEffectSeq),
		Records:        len(ev.Records),
	}

	var fs []Finding
	add := func(code, format string, args ...any) {
		fs = append(fs, Finding{Code: code, Message: fmt.Sprintf(format, args...)})
	}

	if ev.SchemaVersion != RecordSchemaVersion {
		add(CodeEvidenceSchemaVersion, "evidence schema version %d is not %d",
			ev.SchemaVersion, RecordSchemaVersion)
		return led, fs
	}
	if len(ev.Records) == 0 {
		add(CodeEvidenceEmpty, "evidence contains no records; nothing can be verified")
		return led, fs
	}

	prevTime := int64(0)
	prevSeq := uint64(0)
	var blockTime int64
	for i := range ev.Records {
		rec := &ev.Records[i]

		want := ev.FirstEffectSeq + uint64(i)
		if want < ev.FirstEffectSeq {
			add(CodeEffectSeqMismatch, "effect_seq overflows at index %d (first effect_seq %d)",
				i, ev.FirstEffectSeq)
		} else if rec.EffectSeq != want {
			add(CodeEffectSeqMismatch, "record %d has effect_seq %d, expected %d (evidence must be gapless and ordered)",
				i, rec.EffectSeq, want)
		}
		if rec.SchemaVersion != RecordSchemaVersion {
			add(CodeRecordSchemaVersion, "effect_seq %d has record schema version %d, expected %d",
				rec.EffectSeq, rec.SchemaVersion, RecordSchemaVersion)
		}
		if rec.SimTimeNS < 0 {
			add(CodeTimeNegative, "effect_seq %d has negative simulated time %d",
				rec.EffectSeq, rec.SimTimeNS)
		}
		if i > 0 && rec.SimTimeNS < prevTime {
			add(CodeTimeOrder, "effect_seq %d goes back in simulated time: %d after %d",
				rec.EffectSeq, rec.SimTimeNS, prevTime)
		}
		if i == 0 || rec.EngineSeq != prevSeq {
			blockTime = rec.SimTimeNS
		} else if rec.SimTimeNS != blockTime {
			add(CodeBlockTimeMismatch, "effect_seq %d is %dns but its engine_seq %d block runs at %dns",
				rec.EffectSeq, rec.SimTimeNS, rec.EngineSeq, blockTime)
		}
		prevTime = rec.SimTimeNS
		prevSeq = rec.EngineSeq

		switch rec.Category {
		case "event":
			if !EventKinds[rec.Kind] {
				add(CodeEventKind, "effect_seq %d is an event of unknown kind %q",
					rec.EffectSeq, rec.Kind)
			}
		case "note":
			if !NoteKinds[rec.Kind] {
				add(CodeNoteKind, "effect_seq %d is a note of unknown kind %q",
					rec.EffectSeq, rec.Kind)
			}
		default:
			add(CodeRecordCategory, "effect_seq %d has unknown category %q",
				rec.EffectSeq, rec.Category)
		}
	}

	led.Blocks = buildBlocks(ev.Records)
	seen := make(map[uint64]bool, len(led.Blocks))
	for bi := range led.Blocks {
		b := &led.Blocks[bi]
		if seen[b.EngineSeq] {
			add(CodeBlockInterleaved, "engine_seq %d appears in more than one block: records of one processing step must be contiguous",
				b.EngineSeq)
			continue
		}
		seen[b.EngineSeq] = true

		if b.Event == nil {
			// Only a truncated head block may lack its event record. Every
			// retained middle or tail block must show the event that caused it.
			if bi == 0 && led.Partial {
				b.Partial = true
				continue
			}
			add(CodeBlockMissingEvent, "engine_seq %d has notes but no event record", b.EngineSeq)
			continue
		}
		if b.EventCount > 1 {
			add(CodeBlockMultipleEvents, "engine_seq %d carries %d event records; one processing step has one trigger",
				b.EngineSeq, b.EventCount)
		}
		// The reference handler records the trigger before applying any
		// effect, so the event must be the first retained record of the block.
		if b.First.Category != "event" {
			add(CodeBlockEventNotFirst, "engine_seq %d records an effect before its event (effect_seq %d)",
				b.EngineSeq, b.First.EffectSeq)
		}
	}

	validateTerminal(ev, &led, add)
	led.Coverage = classifyCoverage(led.Partial, led.TerminalProven)
	return led, fs
}

// buildBlocks groups records into contiguous same-engine_seq blocks. It does not
// reorder, merge or discard anything: an engine_seq that reappears after another
// block forms a second block and is reported as interleaved.
func buildBlocks(records []Record) []Block {
	var blocks []Block
	for i := range records {
		rec := records[i]
		if len(blocks) == 0 || blocks[len(blocks)-1].EngineSeq != rec.EngineSeq {
			blocks = append(blocks, Block{
				EngineSeq: rec.EngineSeq,
				TimeNS:    rec.SimTimeNS,
				First:     rec,
			})
		}
		b := &blocks[len(blocks)-1]
		if rec.Category == "event" {
			b.EventCount++
			if b.Event == nil {
				b.Event = &records[i]
			}
			continue
		}
		b.Notes = append(b.Notes, rec)
	}
	return blocks
}

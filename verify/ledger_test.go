package verify

import (
	"strings"
	"testing"
)

// testRecord builds one ledger record with explicit correlation fields.
func testRecord(effect, engine uint64, timeNS int64, category, kind string) Record {
	return Record{
		SchemaVersion: RecordSchemaVersion,
		EffectSeq:     effect,
		EngineSeq:     engine,
		SimTimeNS:     timeNS,
		Category:      category,
		Kind:          kind,
	}
}

type testBlock struct {
	engine uint64
	timeNS int64
	event  string
	notes  []string
}

// evidenceFrom builds gapless evidence: one event per block followed by its
// notes, all sharing the block's engine_seq and simulated time (the N:1
// contract).
func evidenceFrom(blocks ...testBlock) Evidence {
	ev := Evidence{SchemaVersion: RecordSchemaVersion}
	var effect uint64
	for _, b := range blocks {
		ev.Records = append(ev.Records, testRecord(effect, b.engine, b.timeNS, "event", b.event))
		effect++
		for _, n := range b.notes {
			ev.Records = append(ev.Records, testRecord(effect, b.engine, b.timeNS, "note", n))
			effect++
		}
	}
	return ev
}

func hasCode(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestValidEvidencePreservesCorrelation(t *testing.T) {
	ev := evidenceFrom(
		testBlock{engine: 1, timeNS: 100, event: "RequestArrival", notes: []string{"request-created", "attempt-created", "admission", "queue"}},
		testBlock{engine: 2, timeNS: 100, event: "QueueRemoval", notes: []string{"attempt-transition"}},
		testBlock{engine: 7, timeNS: 250, event: "ExecutionCompletion", notes: []string{"outcome", "worker"}},
	)

	led, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("valid evidence produced findings: %+v", fs)
	}
	if led.Partial {
		t.Fatal("gapless evidence must not be marked partial")
	}
	if led.Records != 10 || len(led.Blocks) != 3 {
		t.Fatalf("records=%d blocks=%d, want 10 and 3", led.Records, len(led.Blocks))
	}
	if got := len(led.Blocks[0].Notes); got != 4 {
		t.Fatalf("block 0 notes=%d, want 4 (engine-to-effect mapping is N:1)", got)
	}
	if led.Blocks[0].Event == nil || led.Blocks[0].Event.Kind != "RequestArrival" {
		t.Fatalf("block 0 event not retained: %+v", led.Blocks[0].Event)
	}
	if led.Blocks[2].Event.EffectSeq != 7 {
		t.Fatalf("block 2 event effect_seq=%d, want 7", led.Blocks[2].Event.EffectSeq)
	}
}

func TestTruncatedHeadIsPartialNotInvalid(t *testing.T) {
	ev := Evidence{
		SchemaVersion:  RecordSchemaVersion,
		FirstEffectSeq: 5,
		Records: []Record{
			testRecord(5, 7, 100, "note", "worker"),
			testRecord(6, 7, 100, "note", "connection"),
			testRecord(7, 8, 120, "event", "ExecutionStart"),
		},
	}
	led, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("truncated head must be legal evidence, got findings %+v", fs)
	}
	if !led.Partial {
		t.Fatal("first_effect_seq > 0 must mark the ledger partial")
	}
	if !led.Blocks[0].Partial || led.Blocks[0].Event != nil {
		t.Fatalf("head block must be partial without an event: %+v", led.Blocks[0])
	}
	if led.Blocks[1].Event == nil {
		t.Fatal("complete tail block lost its event")
	}
}

func TestEvidenceViolations(t *testing.T) {
	valid := evidenceFrom(
		testBlock{engine: 1, timeNS: 100, event: "RequestArrival", notes: []string{"queue"}},
		testBlock{engine: 2, timeNS: 200, event: "QueueRemoval", notes: []string{"worker"}},
	)

	t.Run("missing head event without truncation", func(t *testing.T) {
		ev := Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records:       []Record{testRecord(0, 1, 100, "note", "worker")},
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeBlockMissingEvent) {
			t.Fatalf("findings %+v, want %s", fs, CodeBlockMissingEvent)
		}
	})

	t.Run("effect sequence gap", func(t *testing.T) {
		ev := valid
		ev.Records = append([]Record(nil), valid.Records...)
		ev.Records[2].EffectSeq += 5
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeEffectSeqMismatch) {
			t.Fatalf("findings %+v, want %s", fs, CodeEffectSeqMismatch)
		}
	})

	t.Run("duplicate effect sequence", func(t *testing.T) {
		ev := valid
		ev.Records = append([]Record(nil), valid.Records...)
		ev.Records[1].EffectSeq = ev.Records[0].EffectSeq
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeEffectSeqMismatch) {
			t.Fatalf("findings %+v, want %s", fs, CodeEffectSeqMismatch)
		}
	})

	t.Run("time regresses between blocks", func(t *testing.T) {
		ev := valid
		ev.Records = append([]Record(nil), valid.Records...)
		for i := range ev.Records {
			if ev.Records[i].EngineSeq == 2 {
				ev.Records[i].SimTimeNS = 50
			}
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeTimeOrder) {
			t.Fatalf("findings %+v, want %s", fs, CodeTimeOrder)
		}
	})

	t.Run("engine sequence interleaved", func(t *testing.T) {
		ev := evidenceFrom(
			testBlock{engine: 1, timeNS: 100, event: "RequestArrival"},
			testBlock{engine: 2, timeNS: 100, event: "QueueInsertion"},
			testBlock{engine: 1, timeNS: 100, event: "LoadShed"},
		)
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeBlockInterleaved) {
			t.Fatalf("findings %+v, want %s", fs, CodeBlockInterleaved)
		}
	})

	t.Run("effect recorded before its event", func(t *testing.T) {
		ev := Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records: []Record{
				testRecord(0, 1, 100, "note", "worker"),
				testRecord(1, 1, 100, "event", "QueueRemoval"),
			},
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeBlockEventNotFirst) {
			t.Fatalf("findings %+v, want %s", fs, CodeBlockEventNotFirst)
		}
	})

	t.Run("two event records in one step", func(t *testing.T) {
		ev := Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records: []Record{
				testRecord(0, 1, 100, "event", "QueueRemoval"),
				testRecord(1, 1, 100, "event", "WorkerAssignment"),
			},
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeBlockMultipleEvents) {
			t.Fatalf("findings %+v, want %s", fs, CodeBlockMultipleEvents)
		}
	})

	t.Run("note time disagrees with its block", func(t *testing.T) {
		ev := valid
		ev.Records = append([]Record(nil), valid.Records...)
		ev.Records[0].SimTimeNS = 101
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeBlockTimeMismatch) {
			t.Fatalf("findings %+v, want %s", fs, CodeBlockTimeMismatch)
		}
	})

	t.Run("unknown category", func(t *testing.T) {
		ev := Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records:       []Record{testRecord(0, 1, 100, "trace", "RequestArrival")},
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeRecordCategory) {
			t.Fatalf("findings %+v, want %s", fs, CodeRecordCategory)
		}
	})

	t.Run("unknown event kind", func(t *testing.T) {
		ev := Evidence{
			SchemaVersion: RecordSchemaVersion,
			Records:       []Record{testRecord(0, 1, 100, "event", "MysteryEvent")},
		}
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeEventKind) {
			t.Fatalf("findings %+v, want %s", fs, CodeEventKind)
		}
	})

	t.Run("unknown note kind", func(t *testing.T) {
		ev := evidenceFrom(testBlock{engine: 1, timeNS: 100, event: "RequestArrival", notes: []string{"mystery-note"}})
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeNoteKind) {
			t.Fatalf("findings %+v, want %s", fs, CodeNoteKind)
		}
	})

	t.Run("negative simulated time", func(t *testing.T) {
		ev := evidenceFrom(testBlock{engine: 1, timeNS: -1, event: "RequestArrival"})
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeTimeNegative) {
			t.Fatalf("findings %+v, want %s", fs, CodeTimeNegative)
		}
	})

	t.Run("empty evidence", func(t *testing.T) {
		_, fs := ValidateEvidence(Evidence{SchemaVersion: RecordSchemaVersion})
		if !hasCode(fs, CodeEvidenceEmpty) {
			t.Fatalf("findings %+v, want %s", fs, CodeEvidenceEmpty)
		}
	})

	t.Run("unknown evidence schema version", func(t *testing.T) {
		_, fs := ValidateEvidence(Evidence{SchemaVersion: 2, Records: valid.Records})
		if !hasCode(fs, CodeEvidenceSchemaVersion) {
			t.Fatalf("findings %+v, want %s", fs, CodeEvidenceSchemaVersion)
		}
	})

	t.Run("record schema version mismatch", func(t *testing.T) {
		ev := valid
		ev.Records = append([]Record(nil), valid.Records...)
		ev.Records[1].SchemaVersion = 99
		_, fs := ValidateEvidence(ev)
		if !hasCode(fs, CodeRecordSchemaVersion) {
			t.Fatalf("findings %+v, want %s", fs, CodeRecordSchemaVersion)
		}
	})
}

func TestFindingsCarryNoProseParsing(t *testing.T) {
	// Description is diagnostic text; changing it must not change validation.
	ev := evidenceFrom(testBlock{engine: 1, timeNS: 100, event: "RequestArrival", notes: []string{"queue"}})
	ev.Records[1].Detail.Description = "worker W has 3 idle slots, budget=unknown"
	_, fs := ValidateEvidence(ev)
	if len(fs) != 0 {
		t.Fatalf("prose description influenced validation: %+v", fs)
	}
	for _, f := range fs {
		if strings.Contains(f.Message, "idle slots") {
			t.Fatalf("finding parsed prose: %+v", f)
		}
	}
}

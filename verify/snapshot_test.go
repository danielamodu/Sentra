package verify

import (
	"bytes"
	"strings"
	"testing"
)

// validSnapshot is the final-state book set that agrees with validInput() and
// with validLifecycleEvidence(): one logical request with one physical attempt,
// both completed, all resources released. It is the S/X-path reference fixture.
//
// Tenant active is 0 because the only request is terminal; the outcome ledger
// counts the completed attempt as useful.
func validSnapshot() *Snapshot {
	return &Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		TakenAtNS:     60_000_000_000,
		Requests: []RequestFact{{
			ID:           "req-1",
			Tenant:       "T1",
			Upstream:     "U1",
			State:        "completed",
			ArrivedAtNS:  100,
			DeadlineNS:   60_000_000_000,
			TerminalAtNS: 500,
			MaxRetries:   0,
			RetriesUsed:  0,
			Attempts:     []string{"att-1"},
		}},
		Attempts: []AttemptFact{{
			ID:           "att-1",
			Request:      "req-1",
			Tenant:       "T1",
			Upstream:     "U1",
			State:        "completed",
			Outcome:      "useful",
			CreatedAtNS:  100,
			FinishedAtNS: 500,
			TimeoutNS:    1_000_000_000,
		}},
		WorkerPools:    []WorkerPoolFact{{ID: "pool", Size: 2, Idle: []string{"w1", "w2"}}},
		Connections:    []ConnectionPoolFact{{Upstream: "U1", Capacity: 2}},
		Queues:         []QueueFact{{Tenant: "T1"}},
		TenantActive:   []TenantActiveFact{{Tenant: "T1", Active: 0}},
		Outcomes:       []OutcomeFact{{Tenant: "T1", Useful: 1}},
		Breakers:       []BreakerFact{{Upstream: "U1", State: "closed"}},
		Budgets:        nil,
		TokensConsumed: nil,
	}
}

// resultsByID indexes property results so a test can assert on one family
// without caring about the others.
func resultsByID(results []PropertyResult) map[string]*PropertyResult {
	m := make(map[string]*PropertyResult, len(results))
	for i := range results {
		m[results[i].ID] = &results[i]
	}
	return m
}

func propertyHasCode(p *PropertyResult, code string) bool {
	for _, f := range p.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func assertAllPass(t *testing.T, results []PropertyResult) {
	t.Helper()
	for _, r := range results {
		if r.Status != StatusPass {
			t.Errorf("property %s = %s (findings %+v, gaps %+v)", r.ID, r.Status, r.Findings, r.Gaps)
		}
	}
}

func TestSnapshotStructuralValidationAcceptsCoherentBooks(t *testing.T) {
	if fs := validateSnapshot(validSnapshot(), validInput()); len(fs) != 0 {
		t.Fatalf("coherent snapshot produced structural findings: %+v", fs)
	}
}

// TestSnapshotStructuralValidationRejectsUnusableBooks covers every structural
// family: schema, horizon, configuration agreement, reference integrity and
// closed vocabularies. Structural findings are evidence defects (INVALID_INPUT
// at the pipeline level), never property failures.
func TestSnapshotStructuralValidationRejectsUnusableBooks(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Snapshot)
		code string
	}{
		{"schema version", func(s *Snapshot) { s.SchemaVersion = 0 }, "snapshot.schema_version"},
		{"horizon mismatch", func(s *Snapshot) { s.TakenAtNS-- }, "snapshot.horizon_mismatch"},
		{"unknown tenant", func(s *Snapshot) { s.Requests[0].Tenant = "T9" }, "snapshot.unknown_tenant"},
		{"unknown upstream", func(s *Snapshot) { s.Requests[0].Upstream = "U9" }, "snapshot.unknown_upstream"},
		{"unknown request state", func(s *Snapshot) { s.Requests[0].State = "warped" }, "snapshot.unknown_state"},
		{"unknown attempt state", func(s *Snapshot) { s.Attempts[0].State = "warped" }, "snapshot.unknown_state"},
		{"unknown outcome", func(s *Snapshot) { s.Attempts[0].Outcome = "maybe" }, "snapshot.unknown_outcome"},
		{"invalid retries", func(s *Snapshot) { s.Requests[0].RetriesUsed = 1 }, "snapshot.invalid_retries"},
		{"invalid request time", func(s *Snapshot) { s.Requests[0].DeadlineNS = 0 }, "snapshot.invalid_time"},
		{"dangling attempt reference", func(s *Snapshot) { s.Requests[0].Attempts = []string{"att-9"} }, "snapshot.dangling_reference"},
		{"dangling parent reference", func(s *Snapshot) { s.Attempts[0].Request = "req-9" }, "snapshot.dangling_reference"},
		{"attempt absent from request", func(s *Snapshot) { s.Requests[0].Attempts = nil }, "snapshot.reference_mismatch"},
		{"duplicate request id", func(s *Snapshot) {
			s.Requests = append(s.Requests, s.Requests[0])
		}, "snapshot.duplicate_id"},
		{"duplicate attempt reference", func(s *Snapshot) {
			s.Requests[0].Attempts = []string{"att-1", "att-1"}
		}, "snapshot.duplicate_reference"},
		{"empty identity", func(s *Snapshot) { s.Requests[0].ID = "" }, "snapshot.empty_id"},
		{"missing worker pool", func(s *Snapshot) { s.WorkerPools = nil }, "snapshot.pool_missing"},
		{"worker pool size mismatch", func(s *Snapshot) { s.WorkerPools[0].Size = 3 }, "snapshot.config_mismatch"},
		{"missing connection pool", func(s *Snapshot) { s.Connections = nil }, "snapshot.pool_missing"},
		{"connection capacity mismatch", func(s *Snapshot) { s.Connections[0].Capacity = 5 }, "snapshot.config_mismatch"},
		{"missing queue", func(s *Snapshot) { s.Queues = nil }, "snapshot.queue_missing"},
		{"missing breaker", func(s *Snapshot) { s.Breakers = nil }, "snapshot.breaker_missing"},
		{"budget capacity mismatch", func(s *Snapshot) {
			s.Budgets = []BudgetFact{{Tenant: "T1", Upstream: "U1", Capacity: 4, RefillIntervalNS: 1_000_000_000}}
			s.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 0}}
		}, "snapshot.config_mismatch"},
		{"invalid pending lane", func(s *Snapshot) {
			s.Pending = []PendingFact{{Tenant: "T1", Attempt: "att-1", Kind: "sideways"}}
		}, "snapshot.invalid_pending"},
		{"dangling pending attempt", func(s *Snapshot) {
			s.Pending = []PendingFact{{Tenant: "T1", Attempt: "att-9", Kind: "arrival"}}
		}, "snapshot.dangling_reference"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.edit(s)
			fs := validateSnapshot(s, validInput())
			if len(fs) == 0 {
				t.Fatalf("corrupted snapshot produced no findings")
			}
			if !hasCode(fs, tc.code) {
				t.Fatalf("findings %+v do not contain %s", fs, tc.code)
			}
			for _, f := range fs {
				if f.Classification != ClassEvidenceDefect {
					t.Fatalf("structural finding %+v is not classified as an evidence defect", f)
				}
			}
		})
	}
}

func TestSnapshotChecksPassOnCoherentBooks(t *testing.T) {
	results := snapshotChecks(validSnapshot(), validInput())
	if len(results) != snapshotPropertyCount() {
		t.Fatalf("results = %d, want %d", len(results), snapshotPropertyCount())
	}
	assertAllPass(t, results)
}

// TestSnapshotChecksRejectSemanticCorruption covers violations that the
// structure admits but the contracts forbid: resource conservation, ownership,
// queue discipline, counters and horizon. Each must be a FAIL, not a structural
// evidence defect.
func TestSnapshotChecksRejectSemanticCorruption(t *testing.T) {
	cases := []struct {
		name     string
		property string
		edit     func(*Snapshot)
	}{
		{
			name:     "worker conservation",
			property: "S-worker-conservation",
			edit: func(s *Snapshot) {
				s.WorkerPools[0].Busy = []WorkerBusyFact{{Worker: "w1", Attempt: "att-1"}}
			},
		},
		{
			name:     "worker binding disagrees with attempt",
			property: "S-worker-ownership",
			edit: func(s *Snapshot) {
				s.WorkerPools[0].Busy = []WorkerBusyFact{{Worker: "w1", Attempt: "att-1"}}
				s.WorkerPools[0].Idle = []string{"w2"}
				s.Attempts[0].HasWorker = false
				s.Attempts[0].Worker = ""
			},
		},
		{
			name:     "attempt claims a connection nobody holds",
			property: "S-connection-ownership",
			edit: func(s *Snapshot) {
				s.Attempts[0].ConnHeld = true
			},
		},
		{
			name:     "waitlisted attempt is not waiting",
			property: "S-waitlist-membership",
			edit: func(s *Snapshot) {
				s.Connections[0].Waiting = []string{"att-1"}
			},
		},
		{
			name:     "free connection with a waiting attempt",
			property: "S-connection-stranding",
			edit: func(s *Snapshot) {
				s.Connections[0].Waiting = []string{"att-1"}
			},
		},
		{
			name:     "queued attempt on no queue",
			property: "S-queue-consistency",
			edit: func(s *Snapshot) {
				s.Attempts[0].State = "queued"
				s.Attempts[0].Outcome = "undetermined"
				s.Attempts[0].FinishedAtNS = 0
				s.Requests[0].State = "queued"
				s.Requests[0].TerminalAtNS = 0
				s.TenantActive[0].Active = 1
				s.Outcomes[0] = OutcomeFact{Tenant: "T1"}
			},
		},
		{
			name:     "tenant active counter drift",
			property: "S-tenant-active",
			edit: func(s *Snapshot) {
				s.TenantActive[0].Active = 1
			},
		},
		{
			name:     "terminal attempt without outcome",
			property: "S-outcome-ledger",
			edit: func(s *Snapshot) {
				s.Attempts[0].Outcome = "undetermined"
			},
		},
		{
			name:     "outcome counters disagree with attempts",
			property: "S-outcome-ledger",
			edit: func(s *Snapshot) {
				s.Outcomes[0] = OutcomeFact{Tenant: "T1", Useful: 0}
			},
		},
		{
			name:     "budget over capacity",
			property: "S-budget-state",
			edit: func(s *Snapshot) {
				s.Budgets = []BudgetFact{{Tenant: "T1", Upstream: "U1", Capacity: 3, Tokens: 4, RefillIntervalNS: 1_000_000_000}}
				s.TokensConsumed = []TokenConsumedFact{{Tenant: "T1", Upstream: "U1", Consumed: 4}}
			},
		},
		{
			name:     "breaker holds probes while closed",
			property: "S-breaker-state",
			edit: func(s *Snapshot) {
				s.Breakers[0].ProbesActive = 1
			},
		},
		{
			name:     "terminal attempt holds a probe permit",
			property: "S-probe-ledger",
			edit: func(s *Snapshot) {
				s.Attempts[0].IsProbe = true
			},
		},
		{
			name:     "request arrives after the horizon",
			property: "S-horizon-completeness",
			edit: func(s *Snapshot) {
				s.Requests[0].ArrivedAtNS = s.TakenAtNS + 1
			},
		},
		{
			name:     "attempt finishes before it starts",
			property: "S-horizon-completeness",
			edit: func(s *Snapshot) {
				s.Attempts[0].FinishedAtNS = 50
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSnapshot()
			tc.edit(s)
			byID := resultsByID(snapshotChecks(s, validInput()))
			p := byID[tc.property]
			if p == nil {
				t.Fatalf("property %s was not evaluated", tc.property)
			}
			if p.Status != StatusFail {
				t.Fatalf("property %s = %s, want FAIL (findings %+v)", tc.property, p.Status, p.Findings)
			}
			for _, f := range p.Findings {
				if f.Classification != ClassUnclassified {
					t.Fatalf("semantic finding is classified %q; only structural defects may be evidence defects", f.Classification)
				}
			}
		})
	}
}

// TestLaneBoundsCountsQueuesAndPending checks the lane accounting directly: both
// queue entries and accepted-but-not-inserted admissions consume lane capacity.
func TestLaneBoundsCountsQueuesAndPending(t *testing.T) {
	att := map[string]*AttemptFact{
		"att-1": {ID: "att-1", Tenant: "T1", State: "queued"},
		"att-2": {ID: "att-2", Tenant: "T1", State: "created", IsRetry: true},
	}
	s := &Snapshot{
		Queues:  []QueueFact{{Tenant: "T1", Entries: []QueueEntryFact{{Attempt: "att-1", Seq: 1}}}},
		Pending: []PendingFact{{Tenant: "T1", Attempt: "att-2", Kind: "retry"}},
	}
	tenants := map[string]TenantSpec{"T1": {ID: "T1", QueueLane: 0, QueueRetry: 0, QueueTotal: 0}}

	ctx := newEvalContext(nil, &World{}, s, &Input{Tenants: []TenantSpec{tenants["T1"]}})
	ctx.attemptByID = att
	r := checkLaneBounds(ctx)
	if r.Status != StatusFail {
		t.Fatalf("lane bounds = %s, want FAIL", r.Status)
	}
	if len(r.Findings) != 3 {
		t.Fatalf("findings = %+v, want arrival, retry and total violations", r.Findings)
	}

	tenants["T1"] = TenantSpec{ID: "T1", QueueLane: 1, QueueRetry: 1, QueueTotal: 2}
	ctx = newEvalContext(nil, &World{}, s, &Input{Tenants: []TenantSpec{tenants["T1"]}})
	ctx.attemptByID = att
	if r := checkLaneBounds(ctx); r.Status != StatusPass {
		t.Fatalf("within-bounds lanes = %+v, want PASS", r)
	}
}

// TestSnapshotAbsenceIsInconclusive enforces D3: no snapshot means every
// resource/bookkeeping property is INCONCLUSIVE with an explicit gap. It must
// never be silently skipped and never PASS.
func TestSnapshotAbsenceIsInconclusive(t *testing.T) {
	results := snapshotChecks(nil, validInput())
	if len(results) != snapshotPropertyCount() {
		t.Fatalf("results = %d, want %d", len(results), snapshotPropertyCount())
	}
	for _, r := range results {
		if r.Status != StatusInconclusive {
			t.Errorf("property %s = %s, want INCONCLUSIVE", r.ID, r.Status)
		}
		if len(r.Gaps) == 0 {
			t.Errorf("property %s has no evidence gap", r.ID)
		}
		if len(r.Findings) != 0 {
			t.Errorf("property %s blamed a finding on absent evidence: %+v", r.ID, r.Findings)
		}
	}
}

// TestSnapshotStructuralCorruptionIsInvalidInput verifies the pipeline
// classification: a snapshot that does not describe the declared run cannot
// support any property claim, so it is INVALID_INPUT with no digest.
func TestSnapshotStructuralCorruptionIsInvalidInput(t *testing.T) {
	doc := validDocument()
	doc.Evidence.Snapshot = validSnapshot()
	doc.Evidence.Snapshot.TakenAtNS = 1

	v := Verify(doc)
	if v.Status != StatusInvalidInput {
		t.Fatalf("verdict = %s, want INVALID_INPUT", v.Status)
	}
	if !hasCode(v.Findings, "snapshot.horizon_mismatch") {
		t.Fatalf("findings = %+v", v.Findings)
	}
	if v.EvidenceDigest != "" {
		t.Fatal("invalid snapshot evidence must not receive a digest")
	}
}

// TestMissingSnapshotDocumentIsInconclusive covers the end-to-end pipeline with
// a valid ledger but no snapshot: L-path claims stand, S- and X-path claims are
// inconclusive, and the aggregate verdict is INCONCLUSIVE.
func TestMissingSnapshotDocumentIsInconclusive(t *testing.T) {
	doc := Document{
		Schema:   SchemaDocument,
		Scenario: "A",
		RunID:    "A-run-1",
		Input:    validInput(),
		Evidence: validLifecycleEvidence(),
	}

	v := Verify(doc)
	if v.Status != StatusInconclusive {
		t.Fatalf("verdict = %s, want INCONCLUSIVE (findings %+v, gaps %+v)", v.Status, v.Findings, v.Gaps)
	}
	byID := resultsByID(v.Properties)
	for _, id := range []string{"S-worker-conservation", "X-entity-sets"} {
		p := byID[id]
		if p == nil || p.Status != StatusInconclusive || len(p.Gaps) == 0 {
			t.Fatalf("%s = %+v, want INCONCLUSIVE with a gap", id, p)
		}
	}
	if len(v.Gaps) == 0 {
		t.Fatal("aggregate verdict lost the property gaps")
	}
	if v.EvidenceDigest == "" {
		t.Fatal("valid ledger evidence lost its digest")
	}
}

func TestSnapshotChecksAreDeterministic(t *testing.T) {
	first := snapshotChecks(validSnapshot(), validInput())
	second := snapshotChecks(validSnapshot(), validInput())
	a, err := CanonicalJSON(first)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	b, err := CanonicalJSON(second)
	if err != nil {
		t.Fatalf("canonical encode: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("snapshot checks are not deterministic:\n%s\n%s", a, b)
	}
}

// TestSnapshotMissingPoolDoesNotPanic exercises the guard paths: a structurally
// invalid snapshot never reaches the checks in the pipeline, but the checks must
// still be total if called directly.
func TestSnapshotMissingPoolDoesNotPanic(t *testing.T) {
	cases := []*Snapshot{
		{SchemaVersion: SnapshotSchemaVersion, TakenAtNS: 60_000_000_000},
		{SchemaVersion: SnapshotSchemaVersion, TakenAtNS: 60_000_000_000, Attempts: []AttemptFact{{ID: "att-1"}}},
	}
	for _, s := range cases {
		for _, r := range snapshotChecks(s, validInput()) {
			if r.Status == "" {
				t.Fatalf("property %s has no status", r.ID)
			}
		}
	}
}

// TestSnapshotSemanticFindingsCarrySnapshotReferences keeps counterexamples
// actionable: every S-path finding names the book it came from and states the
// expected/observed relationship.
func TestSnapshotSemanticFindingsCarrySnapshotReferences(t *testing.T) {
	s := validSnapshot()
	s.WorkerPools[0].Busy = []WorkerBusyFact{{Worker: "w1", Attempt: "att-1"}}
	s.WorkerPools[0].Idle = nil

	for _, r := range snapshotChecks(s, validInput()) {
		for _, f := range r.Findings {
			if f.SnapshotRef == "" {
				t.Errorf("finding %q on %s has no snapshot reference", f.Code, r.ID)
			}
			if f.Expected == "" || f.Observed == "" {
				t.Errorf("finding %q on %s does not state expected/observed: %+v", f.Code, r.ID, f)
			}
			if f.Classification == "" {
				t.Errorf("finding %q on %s has no classification", f.Code, r.ID)
			}
			if strings.Contains(f.Message, "reference system") {
				t.Errorf("finding %q attributes blame in prose: %s", f.Code, f.Message)
			}
		}
	}
}

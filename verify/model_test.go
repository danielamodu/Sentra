package verify

import (
	"strings"
	"testing"
)

func validInput() *Input {
	faults := FaultSchedule{}
	return &Input{
		Scenario:         "A",
		RunID:            "A-run-1",
		HorizonNS:        60_000_000_000,
		WorkerPool:       WorkerPoolSpec{ID: "pool", Size: 2},
		ConnectionPools:  []ConnectionPoolSpec{{Upstream: "U1", Capacity: 2}},
		Tenants:          []TenantSpec{{ID: "T1", QueueLane: 4, QueueRetry: 4, QueueTotal: 8}},
		RetryBudget:      RetryBudgetSpec{Capacity: 3, RefillIntervalNS: 1_000_000_000},
		RetryBackoffNS:   100_000_000,
		AttemptTimeoutNS: 1_000_000_000,
		Breaker:          BreakerSpec{FailureThreshold: 3, CooldownNS: 2_000_000_000, MaxHalfOpenProbes: 1},
		// The synthetic corpus admits exactly one arrival at t=100ns from a
		// single declared component. The envelope [From, From+K*Window) with
		// K=1 covers it; the declared volume is one arrival.
		Workload: &WorkloadSpec{
			Seed: 1,
			Components: []ArrivalComponent{{
				Tenant:        "T1",
				Upstream:      "U1",
				Rate:          1,
				WindowNS:      60_000_000_000,
				FromNS:        0,
				UntilNS:       0,
				DeadlineMinNS: 0,
				DeadlineMaxNS: 60_000_000_000,
				MaxRetries:    0,
				Jitter:        0,
			}},
		},
		Faults: &faults,
	}
}

func TestValidateInputAcceptsDeclaredContract(t *testing.T) {
	if fs := ValidateInput(validInput()); len(fs) != 0 {
		t.Fatalf("valid input produced findings: %+v", fs)
	}
}

func TestValidateInputRejectsBrokenContracts(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
		code string
	}{
		{"missing scenario", func(in *Input) { in.Scenario = "" }, CodeInputIdentity},
		{"missing run identity", func(in *Input) { in.RunID = "" }, CodeInputIdentity},
		{"non-positive horizon", func(in *Input) { in.HorizonNS = 0 }, CodeInputHorizon},
		{"empty pool id", func(in *Input) { in.WorkerPool.ID = "" }, CodeInputWorkerPool},
		{"zero pool size", func(in *Input) { in.WorkerPool.Size = 0 }, CodeInputWorkerPool},
		{"no connection pools", func(in *Input) { in.ConnectionPools = nil }, CodeInputUpstream},
		{"empty upstream id", func(in *Input) { in.ConnectionPools[0].Upstream = "" }, CodeInputUpstream},
		{"zero connection capacity", func(in *Input) { in.ConnectionPools[0].Capacity = 0 }, CodeInputUpstream},
		{"duplicate upstream", func(in *Input) {
			in.ConnectionPools = append(in.ConnectionPools, in.ConnectionPools[0])
		}, CodeInputUpstream},
		{"no tenants", func(in *Input) { in.Tenants = nil }, CodeInputTenant},
		{"empty tenant id", func(in *Input) { in.Tenants[0].ID = "" }, CodeInputTenant},
		{"negative lane bound", func(in *Input) { in.Tenants[0].QueueLane = -1 }, CodeInputTenant},
		{"total bound below lane", func(in *Input) { in.Tenants[0].QueueTotal = 1 }, CodeInputTenant},
		{"duplicate tenant", func(in *Input) { in.Tenants = append(in.Tenants, in.Tenants[0]) }, CodeInputTenant},
		{"zero retry budget", func(in *Input) { in.RetryBudget.Capacity = 0 }, CodeInputRetryBudget},
		{"negative backoff", func(in *Input) { in.RetryBackoffNS = -1 }, CodeInputRetryBudget},
		{"zero attempt timeout", func(in *Input) { in.AttemptTimeoutNS = 0 }, CodeInputTimeout},
		{"zero failure threshold", func(in *Input) { in.Breaker.FailureThreshold = 0 }, CodeInputBreaker},
		{"zero cooldown", func(in *Input) { in.Breaker.CooldownNS = 0 }, CodeInputBreaker},
		{"no half-open probes", func(in *Input) { in.Breaker.MaxHalfOpenProbes = 0 }, CodeInputBreaker},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.edit(in)
			fs := ValidateInput(in)
			if !hasCode(fs, tc.code) {
				t.Fatalf("findings %+v, want %s", fs, tc.code)
			}
		})
	}
}

func TestValidateInputRequiresInput(t *testing.T) {
	fs := ValidateInput(nil)
	if !hasCode(fs, CodeInputMissing) {
		t.Fatalf("findings %+v, want %s", fs, CodeInputMissing)
	}
}

// validWorkload returns a declared workload that is internally consistent with
// validInput().
func validWorkload() *WorkloadSpec {
	return &WorkloadSpec{
		Seed: 42,
		Components: []ArrivalComponent{{
			Tenant:        "T1",
			Upstream:      "U1",
			Rate:          1,
			WindowNS:      1_000_000_000,
			DeadlineMinNS: 1_000_000_000,
			DeadlineMaxNS: 2_000_000_000,
			MaxRetries:    1,
		}},
	}
}

// validFaults returns a declared fault schedule for the upstream in validInput().
func validFaults() *FaultSchedule {
	return &FaultSchedule{{
		Upstream:  "U1",
		Kind:      FaultSlowLatency,
		LatencyNS: 500_000_000,
		FromNS:    1_000_000_000,
		UntilNS:   5_000_000_000,
	}}
}

func TestValidateInputAcceptsDeclaredWorkloadAndFaults(t *testing.T) {
	in := validInput()
	in.Workload = validWorkload()
	in.Faults = validFaults()
	if fs := ValidateInput(in); len(fs) != 0 {
		t.Fatalf("valid declarations produced findings: %+v", fs)
	}
}

// TestValidateInputAccumulatesAllFindings is the regression test for the
// shadowed-slice defect: an invalid workload must not suppress identity,
// worker-pool or fault findings collected before it.
func TestValidateInputAccumulatesAllFindings(t *testing.T) {
	in := validInput()
	in.Scenario = ""
	in.RunID = ""
	in.WorkerPool.Size = 0
	w := validWorkload()
	w.Components[0].Tenant = "T9"
	in.Workload = w
	in.Faults = &FaultSchedule{{Upstream: "", Kind: FaultFailRate, EveryN: 2}}

	fs := ValidateInput(in)
	codes := []string{CodeInputIdentity, CodeInputIdentity, CodeInputWorkerPool, CodeInputWorkload, CodeInputFaults}
	if len(fs) != len(codes) {
		t.Fatalf("findings = %d (%+v), want %d: every independent defect is reported", len(fs), fs, len(codes))
	}
	counts := map[string]int{}
	for _, f := range fs {
		counts[f.Code]++
	}
	for _, code := range codes {
		if counts[code] == 0 {
			t.Fatalf("missing %s in findings %+v", code, fs)
		}
	}
	if counts[CodeInputIdentity] != 2 {
		t.Fatalf("identity findings = %d, want 2: %+v", counts[CodeInputIdentity], fs)
	}
}

// TestValidateInputReportsOverlappingFaultsOnce pins one finding per conflicting
// pair; the bidirectional loop used to report the same pair twice.
func TestValidateInputReportsOverlappingFaultsOnce(t *testing.T) {
	in := validInput()
	in.Faults = &FaultSchedule{
		{Upstream: "U1", Kind: FaultUnavailable, FromNS: 0, UntilNS: 10_000_000_000},
		{Upstream: "U1", Kind: FaultFailRate, EveryN: 2, FromNS: 5_000_000_000, UntilNS: 20_000_000_000},
	}
	fs := ValidateInput(in)
	if len(fs) != 1 {
		t.Fatalf("findings = %d (%+v), want exactly 1 overlap finding", len(fs), fs)
	}
	if fs[0].Code != CodeInputFaults || !strings.Contains(fs[0].Message, "0 and 1") {
		t.Fatalf("finding = %+v, want one %s finding naming episodes 0 and 1", fs[0], CodeInputFaults)
	}

	// Episodes on different upstreams never overlap, and adjacent half-open
	// intervals [0,5) and [5,10) do not overlap either.
	in = validInput()
	in.ConnectionPools = append(in.ConnectionPools, ConnectionPoolSpec{Upstream: "U2", Capacity: 1})
	in.Faults = &FaultSchedule{
		{Upstream: "U1", Kind: FaultUnavailable, FromNS: 0, UntilNS: 5_000_000_000},
		{Upstream: "U2", Kind: FaultUnavailable, FromNS: 0, UntilNS: 5_000_000_000},
		{Upstream: "U1", Kind: FaultUnavailable, FromNS: 5_000_000_000, UntilNS: 10_000_000_000},
	}
	if fs := ValidateInput(in); len(fs) != 0 {
		t.Fatalf("non-overlapping episodes produced findings: %+v", fs)
	}
}

func TestValidateInputRejectsInvalidWorkloadDeclarations(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"unknown tenant", func(in *Input) { in.Workload.Components[0].Tenant = "T9" }},
		{"empty tenant", func(in *Input) { in.Workload.Components[0].Tenant = "" }},
		{"unknown upstream", func(in *Input) { in.Workload.Components[0].Upstream = "U9" }},
		{"negative rate", func(in *Input) { in.Workload.Components[0].Rate = -1 }},
		{"zero window", func(in *Input) { in.Workload.Components[0].WindowNS = 0 }},
		{"jitter above one", func(in *Input) { in.Workload.Components[0].Jitter = 1.5 }},
		{"inverted deadline range", func(in *Input) {
			in.Workload.Components[0].DeadlineMinNS = 3_000_000_000
			in.Workload.Components[0].DeadlineMaxNS = 2_000_000_000
		}},
		{"from at horizon", func(in *Input) { in.Workload.Components[0].FromNS = in.HorizonNS }},
		{"until before from", func(in *Input) {
			in.Workload.Components[0].FromNS = 2_000_000_000
			in.Workload.Components[0].UntilNS = 1_000_000_000
		}},
		{"until beyond horizon", func(in *Input) {
			in.Workload.Components[0].UntilNS = in.HorizonNS + 1
		}},
		{"zero seed", func(in *Input) { in.Workload.Seed = 0 }},
		{"burst with zero count", func(in *Input) {
			in.Workload.Bursts = []Burst{{Tenant: "T1", Upstream: "U1", AtNS: 1_000_000_000, Count: 0}}
		}},
		{"burst at horizon", func(in *Input) {
			in.Workload.Bursts = []Burst{{Tenant: "T1", Upstream: "U1", AtNS: in.HorizonNS, Count: 3}}
		}},
		{"burst unknown upstream", func(in *Input) {
			in.Workload.Bursts = []Burst{{Tenant: "T1", Upstream: "U9", AtNS: 1_000_000_000, Count: 3}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			in.Workload = validWorkload()
			tc.edit(in)
			fs := ValidateInput(in)
			if !hasCode(fs, CodeInputWorkload) {
				t.Fatalf("findings %+v, want %s", fs, CodeInputWorkload)
			}
		})
	}
}

func TestValidateInputRejectsInvalidFaultDeclarations(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Input)
	}{
		{"unknown upstream", func(in *Input) { in.Faults = &FaultSchedule{{Upstream: "U9", Kind: FaultUnavailable}} }},
		{"empty upstream", func(in *Input) { in.Faults = &FaultSchedule{{Upstream: "", Kind: FaultUnavailable}} }},
		{"unknown kind", func(in *Input) { in.Faults = &FaultSchedule{{Upstream: "U1", Kind: "meteor"}} }},
		{"slow latency without latency", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultSlowLatency}}
		}},
		{"fail rate without period", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultFailRate, EveryN: 0}}
		}},
		{"from at horizon", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultUnavailable, FromNS: in.HorizonNS}}
		}},
		{"negative from", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultUnavailable, FromNS: -1}}
		}},
		{"until before from", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultUnavailable, FromNS: 5_000_000_000, UntilNS: 1_000_000_000}}
		}},
		{"until beyond horizon", func(in *Input) {
			in.Faults = &FaultSchedule{{Upstream: "U1", Kind: FaultUnavailable, UntilNS: in.HorizonNS + 1}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.edit(in)
			fs := ValidateInput(in)
			if !hasCode(fs, CodeInputFaults) {
				t.Fatalf("findings %+v, want %s", fs, CodeInputFaults)
			}
		})
	}
}

func TestValidateDocumentRequiresKnownSchema(t *testing.T) {
	if fs := ValidateDocument(Document{Schema: SchemaDocument}); len(fs) != 0 {
		t.Fatalf("known schema rejected: %+v", fs)
	}
	fs := ValidateDocument(Document{Schema: "sentra-verify/v0"})
	if !hasCode(fs, CodeDocumentSchema) {
		t.Fatalf("findings %+v, want %s", fs, CodeDocumentSchema)
	}
}

func TestAggregatePrecedence(t *testing.T) {
	cases := []struct {
		name    string
		results []PropertyResult
		want    Status
	}{
		{"nothing evaluated", nil, StatusInconclusive},
		{"all pass", []PropertyResult{{Status: StatusPass}, {Status: StatusPass}}, StatusPass},
		{"inconclusive outranks pass", []PropertyResult{{Status: StatusPass}, {Status: StatusInconclusive}}, StatusInconclusive},
		{"fail outranks inconclusive", []PropertyResult{{Status: StatusInconclusive}, {Status: StatusFail}}, StatusFail},
		{"invalid input outranks fail", []PropertyResult{{Status: StatusFail}, {Status: StatusInvalidInput}}, StatusInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Aggregate(tc.results); got != tc.want {
				t.Fatalf("Aggregate = %s, want %s", got, tc.want)
			}
		})
	}
}

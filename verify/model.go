package verify

import "fmt"

// SchemaDocument identifies the verifier document contract (D2).
const SchemaDocument = "sentra-verify/v1"

// RecordSchemaVersion is the reference ledger schema version this verifier
// reads. It is part of the evidence contract, not an implementation detail.
const RecordSchemaVersion = 1

// TerminalSchemaVersion versions the additive terminal-boundary attestation.
//
// The attestation is additive: evidence that omits it is still valid v1
// evidence, and it remains terminal-unestablished forever. Nothing in the
// verifier ever infers a terminal boundary from the last ordinary record,
// contiguous effect_seq values or a snapshot timestamp; only this explicit,
// validated attestation can prove the tail.
const TerminalSchemaVersion = 1

// TerminalAttestation is the producer's explicit statement about the run's
// terminal boundary.
//
// Validation invariants (enforced by ValidateEvidence, documented there):
//
//  1. EffectSeq is the final effect_seq the run wrote.
//  2. SimTimeNS is the run's terminal simulated instant (its horizon).
//  3. RecordCount is the total number of records the run wrote, including any
//     records no longer retained by a bounded sink.
//  4. RecordsDigest fingerprints that full record sequence.
//
// The attestation never proves the *head*. A head-truncated ledger remains
// head-truncated even when its tail is attested, so it can never masquerade as
// full history.
type TerminalAttestation struct {
	SchemaVersion int    `json:"schema_version"`
	EffectSeq     uint64 `json:"effect_seq"`
	SimTimeNS     int64  `json:"sim_time_ns"`
	RecordCount   int    `json:"record_count"`
	RecordsDigest string `json:"records_digest"`
}

// Document is the complete verification input: the declared configuration
// contract and the evidence produced by one reference run.
type Document struct {
	Schema   string   `json:"schema"`
	Scenario string   `json:"scenario,omitempty"`
	RunID    string   `json:"run_id,omitempty"`
	Input    *Input   `json:"input,omitempty"`
	Evidence Evidence `json:"evidence"`
}

// Input is the verifier-side configuration contract. It mirrors the documented
// M1/M2 configuration semantics without importing reference types.
//
// Workload components and fault schedules are carried as declared inputs, which
// the P3 property registry will eventually compare with observed behaviour. They
// are validated for internal consistency only: seed replay, arrival generation,
// burst application and fault activation are NOT verified, and no property yet
// reads them. The declarations must not be read as workload/fault verification.
type Input struct {
	Scenario         string               `json:"scenario"`
	RunID            string               `json:"run_id"`
	HorizonNS        int64                `json:"horizon_ns"`
	WorkerPool       WorkerPoolSpec       `json:"worker_pool"`
	ConnectionPools  []ConnectionPoolSpec `json:"connection_pools"`
	Tenants          []TenantSpec         `json:"tenants"`
	RetryBudget      RetryBudgetSpec      `json:"retry_budget"`
	RetryBackoffNS   int64                `json:"retry_backoff_ns"`
	AttemptTimeoutNS int64                `json:"attempt_timeout_ns"`
	Breaker          BreakerSpec          `json:"breaker"`
	// Workload is the declared arrival schedule, copied as plain data. Nothing
	// compares it with observed arrivals yet.
	Workload *WorkloadSpec `json:"workload,omitempty"`
	// Faults is the declared fault-injection schedule, copied as plain data.
	// Nothing compares it with observed upstream behaviour yet.
	Faults *FaultSchedule `json:"faults,omitempty"`
}

// WorkerPoolSpec declares the single execution pool shape.
type WorkerPoolSpec struct {
	ID   string `json:"id"`
	Size int    `json:"size"`
}

// ConnectionPoolSpec declares the connection capacity of one upstream.
type ConnectionPoolSpec struct {
	Upstream string `json:"upstream"`
	Capacity int    `json:"capacity"`
}

// TenantSpec declares one tenant's admission lanes. QueueLane and QueueRetry are
// the two independent lanes; QueueTotal bounds their sum and must be at least
// each lane.
type TenantSpec struct {
	ID         string `json:"id"`
	QueueLane  int    `json:"queue_lane"`
	QueueRetry int    `json:"queue_retry"`
	QueueTotal int    `json:"queue_total"`
}

// RetryBudgetSpec declares the per-(tenant, upstream) retry bucket. A
// non-positive RefillIntervalNS means the bucket never refills.
type RetryBudgetSpec struct {
	Capacity         int   `json:"capacity"`
	RefillIntervalNS int64 `json:"refill_interval_ns"`
}

// BreakerSpec declares the per-upstream circuit breaker policy.
type BreakerSpec struct {
	FailureThreshold  int   `json:"failure_threshold"`
	CooldownNS        int64 `json:"cooldown_ns"`
	MaxHalfOpenProbes int   `json:"max_half_open_probes"`
}

// Evidence is the serialized observation of one reference run.
//
// Records are the ledger (causal order and lifecycle). Terminal is the optional
// explicit terminal-boundary attestation; without it the retained tail is never
// assumed to reach the run's end. Snapshot is the independent final-state
// observation required by resource properties (D3). The evidence digest covers
// all three; the input contract is deliberately outside it
// (docs/VERIFIER_DESIGN.md §4).
type Evidence struct {
	SchemaVersion  int      `json:"schema_version"`
	FirstEffectSeq uint64   `json:"first_effect_seq"`
	Records        []Record `json:"records"`
	// Terminal is the optional, versioned terminal-boundary attestation. Its
	// absence means the tail is unproven, never that the run had no tail.
	Terminal *TerminalAttestation `json:"terminal,omitempty"`
	// Snapshot is absent when the producer could not observe final books. Its
	// absence yields INCONCLUSIVE resource properties, never a silent skip.
	Snapshot *Snapshot `json:"snapshot,omitempty"`
}

// Record is the verifier-side ledger schema. It mirrors the documented reference
// record contract field-for-field so that evidence can be validated and
// cross-checked, but it is an independent type: converting reference records is
// the serializer's job (package evidence), not the authority's.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	EffectSeq     uint64 `json:"effect_seq"`
	EngineSeq     uint64 `json:"engine_seq"`
	SimTimeNS     int64  `json:"sim_time_ns"`
	Category      string `json:"category"`
	Kind          string `json:"kind"`
	Subject       string `json:"subject,omitempty"`
	Detail        Detail `json:"detail"`
}

// Detail is the typed payload of a record. Fields are carried verbatim; prose in
// Description is diagnostic only and is never parsed as evidence.
type Detail struct {
	Request     string `json:"request,omitempty"`
	Attempt     string `json:"attempt,omitempty"`
	Tenant      string `json:"tenant,omitempty"`
	Upstream    string `json:"upstream,omitempty"`
	Worker      string `json:"worker,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Lane        string `json:"lane,omitempty"`
	Description string `json:"description,omitempty"`
	// Typed effect payloads added for M3 P3. They are additive and optional:
	// evidence produced before them stays readable and simply does not carry
	// the fact.
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

// ValidateDocument checks the document envelope (stage 1 of the pipeline).
func ValidateDocument(doc Document) []Finding {
	var fs []Finding
	if doc.Schema != SchemaDocument {
		fs = append(fs, Finding{
			Code:    CodeDocumentSchema,
			Message: fmt.Sprintf("document schema %q is not %q", doc.Schema, SchemaDocument),
		})
	}
	return fs
}

// ValidateInput checks the configuration contract (stage 2). It validates the
// declared shape only; it does not check the evidence against it.
func ValidateInput(in *Input) []Finding {
	if in == nil {
		return []Finding{{Code: CodeInputMissing, Message: "input contract is absent"}}
	}

	var fs []Finding
	add := func(code, format string, args ...any) {
		fs = append(fs, Finding{Code: code, Message: fmt.Sprintf(format, args...)})
	}

	if in.Scenario == "" {
		add(CodeInputIdentity, "scenario identity is empty")
	}
	if in.RunID == "" {
		add(CodeInputIdentity, "run identity is empty")
	}
	if in.HorizonNS <= 0 {
		add(CodeInputHorizon, "horizon must be positive, got %d", in.HorizonNS)
	}
	if in.WorkerPool.ID == "" {
		add(CodeInputWorkerPool, "worker pool id is empty")
	}
	if in.WorkerPool.Size <= 0 {
		add(CodeInputWorkerPool, "worker pool size must be positive, got %d", in.WorkerPool.Size)
	}
	if in.AttemptTimeoutNS <= 0 {
		add(CodeInputTimeout, "attempt timeout must be positive, got %d", in.AttemptTimeoutNS)
	}

	upstreams := make(map[string]bool, len(in.ConnectionPools))
	for i, p := range in.ConnectionPools {
		if p.Upstream == "" {
			add(CodeInputUpstream, "connection pool %d has an empty upstream id", i)
		}
		if p.Capacity <= 0 {
			add(CodeInputUpstream, "connection pool %q capacity must be positive, got %d", p.Upstream, p.Capacity)
		}
		if p.Upstream != "" && upstreams[p.Upstream] {
			add(CodeInputUpstream, "connection pool %q is declared twice", p.Upstream)
		}
		upstreams[p.Upstream] = true
	}
	if len(in.ConnectionPools) == 0 {
		add(CodeInputUpstream, "no connection pools are declared")
	}

	tenants := make(map[string]bool, len(in.Tenants))
	for i, t := range in.Tenants {
		if t.ID == "" {
			add(CodeInputTenant, "tenant %d has an empty id", i)
		}
		if t.ID != "" && tenants[t.ID] {
			add(CodeInputTenant, "tenant %q is declared twice", t.ID)
		}
		tenants[t.ID] = true
		if t.QueueLane < 0 {
			add(CodeInputTenant, "tenant %q arrival lane bound must be non-negative, got %d", t.ID, t.QueueLane)
		}
		if t.QueueRetry < 0 {
			add(CodeInputTenant, "tenant %q retry lane bound must be non-negative, got %d", t.ID, t.QueueRetry)
		}
		if t.QueueTotal < t.QueueLane || t.QueueTotal < t.QueueRetry {
			add(CodeInputTenant, "tenant %q total queue bound %d cannot contain its lanes (%d, %d)",
				t.ID, t.QueueTotal, t.QueueLane, t.QueueRetry)
		}
	}
	if len(in.Tenants) == 0 {
		add(CodeInputTenant, "no tenants are declared")
	}

	if in.RetryBudget.Capacity <= 0 {
		add(CodeInputRetryBudget, "retry budget capacity must be positive, got %d", in.RetryBudget.Capacity)
	}
	if in.RetryBackoffNS < 0 {
		add(CodeInputRetryBudget, "retry backoff must be non-negative, got %d", in.RetryBackoffNS)
	}
	if in.Breaker.FailureThreshold <= 0 {
		add(CodeInputBreaker, "breaker failure threshold must be positive, got %d", in.Breaker.FailureThreshold)
	}
	if in.Breaker.CooldownNS <= 0 {
		add(CodeInputBreaker, "breaker cooldown must be positive, got %d", in.Breaker.CooldownNS)
	}
	if in.Breaker.MaxHalfOpenProbes <= 0 {
		add(CodeInputBreaker, "breaker probe allowance must be positive, got %d", in.Breaker.MaxHalfOpenProbes)
	}

	// Workload and fault schedule validation (P3 prerequisite): the declared
	// schedules must be well-formed against the input. This is declaration
	// checking only; it never compares a declaration with observed behaviour.
	// An invalid declared schedule is not a property failure — it is an unusable
	// input, reported alongside every other input finding.
	if in.HorizonNS > 0 {
		fs = append(fs, validateWorkload(in.Workload, in)...)
		fs = append(fs, validateFaults(in.Faults, in)...)
	}
	return fs
}

// validateWorkload checks the declared workload components and bursts against the
// input contract (tenant/upstream identity, horizon bounds).
func validateWorkload(w *WorkloadSpec, in *Input) []Finding {
	if w == nil {
		return nil
	}
	var fs []Finding
	add := func(code, format string, args ...any) {
		fs = append(fs, Finding{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	knownUpstreams := make(map[string]bool)
	for _, p := range in.ConnectionPools {
		knownUpstreams[p.Upstream] = true
	}
	knownTenants := make(map[string]bool)
	for _, t := range in.Tenants {
		knownTenants[t.ID] = true
	}
	horizon := in.HorizonNS
	for i := range w.Components {
		c := &w.Components[i]
		if c.Tenant == "" {
			add(CodeInputWorkload, "workload component %d: tenant must be set", i)
		} else if !knownTenants[c.Tenant] {
			add(CodeInputWorkload, "workload component %d: tenant %q is not configured", i, c.Tenant)
		}
		if c.Upstream == "" {
			add(CodeInputWorkload, "workload component %d: upstream must be set", i)
		} else if !knownUpstreams[c.Upstream] {
			add(CodeInputWorkload, "workload component %d: upstream %q is not configured", i, c.Upstream)
		}
		if c.Rate < 0 {
			add(CodeInputWorkload, "workload component %d: rate must be non-negative, got %v", i, c.Rate)
		}
		if c.WindowNS <= 0 {
			add(CodeInputWorkload, "workload component %d: window must be positive, got %d", i, c.WindowNS)
		}
		if c.Jitter < 0 || c.Jitter > 1 {
			add(CodeInputWorkload, "workload component %d: jitter must be in [0,1], got %v", i, c.Jitter)
		}
		if c.DeadlineMinNS > c.DeadlineMaxNS {
			add(CodeInputWorkload, "workload component %d: deadline range inverted (min %d > max %d)", i, c.DeadlineMinNS, c.DeadlineMaxNS)
		}
		if c.FromNS < 0 || c.FromNS >= horizon {
			add(CodeInputWorkload, "workload component %d: from %dns outside [0,%d)", i, c.FromNS, horizon)
		}
		if c.UntilNS != 0 && c.UntilNS <= c.FromNS {
			add(CodeInputWorkload, "workload component %d: until %dns not after from %dns", i, c.UntilNS, c.FromNS)
		}
		if c.UntilNS != 0 && c.UntilNS > horizon {
			add(CodeInputWorkload, "workload component %d: until %dns beyond horizon %dns", i, c.UntilNS, horizon)
		}
	}
	if w.Seed == 0 {
		add(CodeInputWorkload, "workload seed must be non-zero")
	}
	for i := range w.Bursts {
		b := &w.Bursts[i]
		if b.Tenant == "" {
			add(CodeInputWorkload, "burst %d: tenant must be set", i)
		} else if !knownTenants[b.Tenant] {
			add(CodeInputWorkload, "burst %d: tenant %q is not configured", i, b.Tenant)
		}
		if b.Upstream == "" {
			add(CodeInputWorkload, "burst %d: upstream must be set", i)
		} else if !knownUpstreams[b.Upstream] {
			add(CodeInputWorkload, "burst %d: upstream %q is not configured", i, b.Upstream)
		}
		if b.Count <= 0 {
			add(CodeInputWorkload, "burst %d: count must be positive, got %d", i, b.Count)
		}
		if b.AtNS < 0 || b.AtNS >= horizon {
			add(CodeInputWorkload, "burst %d: at %dns outside [0,%d)", i, b.AtNS, horizon)
		}
		if b.DeadlineMinNS > b.DeadlineMaxNS {
			add(CodeInputWorkload, "burst %d: deadline range inverted", i)
		}
	}
	return fs
}

// validateFaults checks the declared fault schedule against the input contract.
func validateFaults(fs *FaultSchedule, in *Input) []Finding {
	if fs == nil {
		return nil
	}
	var findings []Finding
	add := func(code, format string, args ...any) {
		findings = append(findings, Finding{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	knownUpstreams := make(map[string]bool)
	for _, p := range in.ConnectionPools {
		knownUpstreams[p.Upstream] = true
	}
	horizon := in.HorizonNS
	for i := range *fs {
		e := &(*fs)[i]
		if e.Upstream == "" {
			add(CodeInputFaults, "fault episode %d: upstream must be set", i)
		} else if !knownUpstreams[e.Upstream] {
			add(CodeInputFaults, "fault episode %d: upstream %q is not configured", i, e.Upstream)
		}
		switch e.Kind {
		case FaultSlowLatency:
			if e.LatencyNS <= 0 {
				add(CodeInputFaults, "fault episode %d: slow-latency episode needs positive latency, got %d", i, e.LatencyNS)
			}
		case FaultFailRate:
			if e.EveryN < 1 {
				add(CodeInputFaults, "fault episode %d: fail-rate episode needs every_n >= 1, got %d", i, e.EveryN)
			}
		case FaultUnavailable:
			// no parameters
		default:
			add(CodeInputFaults, "fault episode %d: unknown kind %q", i, e.Kind)
		}
		if e.FromNS < 0 || e.FromNS >= horizon {
			add(CodeInputFaults, "fault episode %d: from %dns outside [0,%d)", i, e.FromNS, horizon)
		}
		if e.UntilNS != 0 && e.UntilNS <= e.FromNS {
			add(CodeInputFaults, "fault episode %d: until %dns not after from %dns", i, e.UntilNS, e.FromNS)
		}
		if e.UntilNS != 0 && e.UntilNS > horizon {
			add(CodeInputFaults, "fault episode %d: until %dns beyond horizon %dns", i, e.UntilNS, horizon)
		}
	}
	// Overlap check on the same upstream. Each conflicting pair is reported once
	// (i < j); reporting both orientations would double every finding.
	for i := range *fs {
		for j := i + 1; j < len(*fs); j++ {
			if (*fs)[i].Upstream != (*fs)[j].Upstream {
				continue
			}
			a, b := &(*fs)[i], &(*fs)[j]
			au := a.UntilNS
			if au == 0 {
				au = horizon
			}
			bu := b.UntilNS
			if bu == 0 {
				bu = horizon
			}
			if a.FromNS < bu && b.FromNS < au {
				add(CodeInputFaults, "fault episodes %d and %d overlap on upstream %q: [%d,%d) and [%d,%d)",
					i, j, a.Upstream, a.FromNS, au, b.FromNS, bu)
			}
		}
	}
	return findings
}

// WorkloadSpec is the verifier-side copy of the recorded workload input. It is
// plain data: the verifier never imports sim workload code, and it does not yet
// re-derive expected arrivals from it.
type WorkloadSpec struct {
	Seed       uint64             `json:"seed"`
	Components []ArrivalComponent `json:"components"`
	Bursts     []Burst            `json:"bursts"`
}

// ArrivalComponent mirrors the sim type: one tenant's traffic stream to one
// upstream, integer-window rate with fractional carry, optional jitter,
// deadline bounds, retry limits, optional activation window.
type ArrivalComponent struct {
	Tenant        string  `json:"tenant"`
	Upstream      string  `json:"upstream"`
	Rate          float64 `json:"rate_per_window"`
	WindowNS      int64   `json:"window_ns"`
	FromNS        int64   `json:"from_ns"`
	UntilNS       int64   `json:"until_ns"`
	DeadlineMinNS int64   `json:"deadline_min_ns"`
	DeadlineMaxNS int64   `json:"deadline_max_ns"`
	MaxRetries    int     `json:"max_retries"`
	Jitter        float64 `json:"jitter"`
}

// Burst mirrors the sim type: a one-off arrival surge.
type Burst struct {
	Tenant        string `json:"tenant"`
	Upstream      string `json:"upstream"`
	AtNS          int64  `json:"at_ns"`
	Count         int    `json:"count"`
	SpreadNS      int64  `json:"spread_ns"`
	DeadlineMinNS int64  `json:"deadline_min_ns"`
	DeadlineMaxNS int64  `json:"deadline_max_ns"`
	MaxRetries    int    `json:"max_retries"`
}

// FaultKind mirrors the sim FaultKind: the mode applied during a fault episode.
type FaultKind string

const (
	FaultSlowLatency FaultKind = "slow-latency"
	FaultFailRate    FaultKind = "fail-rate"
	FaultUnavailable FaultKind = "unavailable"
)

// FaultEpisode is one interval [FromNS, UntilNS) applying one mode to one
// upstream. UntilNS == 0 means "active until the horizon".
type FaultEpisode struct {
	Upstream  string    `json:"upstream"`
	Kind      FaultKind `json:"kind"`
	EveryN    int       `json:"every_n,omitempty"`
	LatencyNS int64     `json:"latency_ns,omitempty"`
	FromNS    int64     `json:"from_ns"`
	UntilNS   int64     `json:"until_ns"`
}

// FaultSchedule is the verifier-side copy of the recorded fault schedule.
type FaultSchedule []FaultEpisode

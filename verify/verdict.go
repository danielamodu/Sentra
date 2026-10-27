package verify

// Status is the outcome of verification. The four categories are semantically
// distinct and never collapse into a boolean:
//
//   - PASS: every evaluated property held on sufficient evidence.
//   - FAIL: at least one property was violated on consistent evidence.
//   - INCONCLUSIVE: no property was violated, but at least one required property
//     could not be established from the available evidence. Never treated as a
//     pass.
//   - INVALID_INPUT: the document, input contract or evidence is unusable, so no
//     property claim can be made at all.
type Status string

const (
	StatusPass         Status = "PASS"
	StatusFail         Status = "FAIL"
	StatusInconclusive Status = "INCONCLUSIVE"
	StatusInvalidInput Status = "INVALID_INPUT"
)

// rank orders statuses by severity for aggregation.
func (s Status) rank() int {
	switch s {
	case StatusPass:
		return 0
	case StatusInconclusive:
		return 1
	case StatusFail:
		return 2
	case StatusInvalidInput:
		return 3
	}
	return 3
}

// Document and input validation codes (stages 1-2 of the pipeline).
const (
	CodeDocumentSchema   = "document.schema"
	CodeInputMissing     = "input.missing"
	CodeInputIdentity    = "input.identity"
	CodeInputHorizon     = "input.horizon"
	CodeInputWorkerPool  = "input.worker_pool"
	CodeInputUpstream    = "input.upstream"
	CodeInputTenant      = "input.tenant"
	CodeInputRetryBudget = "input.retry_budget"
	CodeInputTimeout     = "input.timeout"
	CodeInputBreaker     = "input.breaker"
	CodeInputWorkload    = "input.workload"
	CodeInputFaults      = "input.faults"
)

// Discrepancy classifications. They are candidate labels, never verdicts: the
// verifier reports what kind of explanation the evidence admits and never
// attributes a disagreement to the reference implementation automatically
// (M3 plan §7, D6).
const (
	ClassReferenceDefectCandidate = "reference-defect-candidate"
	ClassVerifierDefectCandidate  = "verifier-defect-candidate"
	ClassEvidenceDefect           = "evidence-defect"
	ClassContractAmbiguity        = "contract-ambiguity"
	ClassLegitimateDifference     = "legitimate-difference"
	ClassUnclassified             = "unclassified"
)

// Finding is one machine-readable validation failure or violation. Entity
// fields, expected/observed relationships and evidence references make the
// finding actionable without reading prose.
type Finding struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Classification string `json:"classification,omitempty"`
	EffectSeq      uint64 `json:"effect_seq,omitempty"`
	EngineSeq      uint64 `json:"engine_seq,omitempty"`
	Request        string `json:"request,omitempty"`
	Attempt        string `json:"attempt,omitempty"`
	Tenant         string `json:"tenant,omitempty"`
	Upstream       string `json:"upstream,omitempty"`
	Worker         string `json:"worker,omitempty"`
	Expected       string `json:"expected,omitempty"`
	Observed       string `json:"observed,omitempty"`
	SnapshotRef    string `json:"snapshot_ref,omitempty"`
	// Counterexample is the bounded evidence context attached by the registry
	// executor. Primary is always present; neighbours are context unless the
	// property establishes a causal role.
	Counterexample *Counterexample `json:"counterexample,omitempty"`
}

// Gap documents an evidence gap that prevented a property from being
// established. A gap always yields INCONCLUSIVE for the affected property.
type Gap struct {
	Property string `json:"property"`
	Reason   string `json:"reason"`
}

// PropertyResult is the outcome of one named property.
type PropertyResult struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Status   Status    `json:"status"`
	Findings []Finding `json:"findings,omitempty"`
	Gaps     []Gap     `json:"gaps,omitempty"`
}

// Verdict is the complete verification output: what was checked, what passed,
// what failed, what could not be established, and which evidence supports the
// claim. It is deterministic and JSON-serializable.
type Verdict struct {
	Schema         string           `json:"schema"`
	Status         Status           `json:"status"`
	Scenario       string           `json:"scenario,omitempty"`
	RunID          string           `json:"run_id,omitempty"`
	EvidenceDigest string           `json:"evidence_digest,omitempty"`
	Partial        bool             `json:"partial"`
	Properties     []PropertyResult `json:"properties,omitempty"`
	Findings       []Finding        `json:"findings,omitempty"`
	Gaps           []Gap            `json:"gaps,omitempty"`
}

// Aggregate computes the verdict status from property results using the
// documented precedence INVALID_INPUT > FAIL > INCONCLUSIVE > PASS.
//
// An empty property list is INCONCLUSIVE, never PASS: a verifier that evaluated
// nothing has established nothing.
func Aggregate(results []PropertyResult) Status {
	if len(results) == 0 {
		return StatusInconclusive
	}
	worst := StatusPass
	for _, r := range results {
		if r.Status.rank() > worst.rank() {
			worst = r.Status
		}
	}
	return worst
}

// newCheck starts a property result in PASS; it becomes FAIL when a finding is
// added and INCONCLUSIVE when a gap is added (a finding always outranks a gap).
func newCheck(id, name string) *PropertyResult {
	return &PropertyResult{ID: id, Name: name, Status: StatusPass}
}

func (r *PropertyResult) fail(f Finding) {
	if f.Classification == "" {
		f.Classification = ClassUnclassified
	}
	r.Findings = append(r.Findings, f)
	r.Status = StatusFail
}

func (r *PropertyResult) addGap(reason string) {
	r.Gaps = append(r.Gaps, Gap{Property: r.ID, Reason: reason})
	if r.Status == StatusPass {
		r.Status = StatusInconclusive
	}
}

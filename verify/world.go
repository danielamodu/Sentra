package verify

import "fmt"

// Transition is one recorded lifecycle step, kept with the evidence reference
// that establishes it so counterexamples can point at the exact record.
type Transition struct {
	From      string
	To        string
	TimeNS    int64
	EffectSeq uint64
	EngineSeq uint64
}

// RequestWorld is the verifier-side reconstruction of one logical request.
type RequestWorld struct {
	ID              string
	Tenant          string
	Upstream        string
	State           string
	Transitions     []Transition
	Terminal        bool
	TerminalAtNS    int64
	FirstSeenSeq    uint64
	FirstSeenEngine uint64
	FirstSeenTimeNS int64
	// CreatedSeen is true when a typed AdmissionDecided event in the retained
	// window establishes the request's admission decision. It is the only
	// typed creation evidence a request carries: the reference's request-created
	// note is prose-only and is never parsed.
	CreatedSeen bool
	// Complete is true when the retained history begins at the documented
	// initial state and the evidence establishes that no earlier transition
	// was evicted. A truncated head without typed creation evidence makes it
	// false, and properties needing the full history must not claim a result.
	Complete   bool
	AttemptIDs []string
}

// AttemptWorld is the verifier-side reconstruction of one physical attempt.
type AttemptWorld struct {
	ID              string
	Request         string
	RequestLinked   bool
	Tenant          string
	Upstream        string
	Lane            string
	CreatedSeen     bool
	CreatedAtNS     int64
	State           string
	Transitions     []Transition
	Terminal        bool
	FinishedAtNS    int64
	FirstSeenSeq    uint64
	FirstSeenEngine uint64
	FirstSeenTimeNS int64
	Complete        bool
}

// BreakerWorld is the verifier-side reconstruction of one upstream breaker.
type BreakerWorld struct {
	Upstream    string
	State       string
	Transitions []Transition
	Complete    bool
}

// World is the independent reconstruction of behaviour from evidence.
//
// It contains no reference state: every field is derived from typed ledger
// records (state transitions, typed request/attempt references, typed breaker
// events). Prose descriptions are never consulted.
type World struct {
	Requests          []*RequestWorld
	Attempts          []*AttemptWorld
	Breakers          []*BreakerWorld
	RequestByID       map[string]*RequestWorld
	AttemptByID       map[string]*AttemptWorld
	BreakerByUpstream map[string]*BreakerWorld

	// Fatal findings mean the ledger cannot be interpreted against the
	// documented contracts at all (unknown state names, contradictory typed
	// identities). Callers must treat them as INVALID_INPUT.
	Fatal []Finding

	// Violations are reconstruction results: they are evaluated by the L-path
	// checks, not silently treated as evidence defects.
	RequestViolations    []Finding
	AttemptViolations    []Finding
	ContinuityViolations []Finding
	BreakerViolations    []Finding

	Gaps     []Gap
	Partial  bool
	Coverage HistoryCoverage
	// TerminalProven records whether the evidence carried a validated terminal
	// attestation binding the retained tail to the attested final effect.
	TerminalProven bool
	Records        int
	Unlinked       int
	Linked         int

	// Ledger-level retry/probe facts used by cross-path checks.
	BudgetConsumptions int
	RetryDecisions     int
	ProbeReserved      int
	ProbeReleased      int
}

// reconstruct builds the verifier-side world from validated evidence.
//
// Determinism: blocks are visited in evidence order and every output slice
// preserves first-observation order; maps are lookup indexes only.
func reconstruct(led *Ledger) *World {
	w := &World{
		RequestByID:       make(map[string]*RequestWorld),
		AttemptByID:       make(map[string]*AttemptWorld),
		BreakerByUpstream: make(map[string]*BreakerWorld),
		Partial:           led.Partial,
		Coverage:          led.Coverage,
		TerminalProven:    led.TerminalProven,
		Records:           led.Records,
	}

	for bi := range led.Blocks {
		b := &led.Blocks[bi]
		refs := distinctStrings(blockRequestRefs(b))

		for ni := range b.Notes {
			switch b.Notes[ni].Kind {
			case "budget":
				w.BudgetConsumptions++
			case "probe-reserved":
				w.ProbeReserved++
			case "probe-released":
				w.ProbeReleased++
			}
		}

		if b.Event != nil {
			if b.Event.Kind == "RetryScheduled" {
				w.RetryDecisions++
			}
			if id := requestRefOfEvent(b.Event); id != "" {
				r, _ := w.request(id, b.Event)
				w.attachEventIdentity(r, b.Event)
			}
			if b.Event.Kind == "BreakerTransitioned" {
				w.breakerTransition(b.Event)
			}
		}

		for ni := range b.Notes {
			n := b.Notes[ni]
			switch n.Kind {
			case "attempt-created":
				w.attemptCreated(n, refs)
			case "request-transition":
				w.requestTransition(n)
			case "attempt-transition":
				w.attemptTransition(n, refs)
			}
		}
	}

	// Creation effects are emitted in the arrival block, which precedes the
	// AdmissionDecided block that creates the request entity here. Reconcile the
	// reverse index once every entity is known so the link order of the
	// reference is preserved rather than guessed from block adjacency.
	for _, a := range w.Attempts {
		if !a.RequestLinked || a.Request == "" {
			continue
		}
		if r, ok := w.RequestByID[a.Request]; ok && !containsString(r.AttemptIDs, a.ID) {
			r.AttemptIDs = append(r.AttemptIDs, a.ID)
		}
	}

	// A request reconstructed from identity alone has no lifecycle history at
	// all. That is missing evidence, not an empty history.
	for _, r := range w.Requests {
		if len(r.Transitions) == 0 {
			w.completenessGap(fmt.Sprintf("request %s has no retained lifecycle transition; its history cannot be established", r.ID))
		}
	}

	return w
}

// completenessGap records an L-history-completeness gap. The property is about
// what the evidence can establish, so missing history is always a gap, never a
// violation.
func (w *World) completenessGap(reason string) {
	w.Gaps = append(w.Gaps, Gap{Property: "L-history-completeness", Reason: reason})
}

func requestCompletenessReason(r *RequestWorld, from string, partial bool) string {
	if from != "admitted" {
		return fmt.Sprintf("request %s history begins at %q instead of the initial state %q", r.ID, from, "admitted")
	}
	if partial && !r.CreatedSeen {
		return fmt.Sprintf("request %s history begins at %q but the ledger head is truncated and no retained typed admission record establishes its creation", r.ID, from)
	}
	return fmt.Sprintf("request %s history cannot be established from the retained records", r.ID)
}

func attemptCompletenessReason(a *AttemptWorld, from string, partial bool) string {
	if from != "created" {
		return fmt.Sprintf("attempt %s history begins at %q instead of the initial state %q", a.ID, from, "created")
	}
	if partial && !a.CreatedSeen {
		return fmt.Sprintf("attempt %s history begins at %q but the ledger head is truncated and no retained typed creation effect establishes its creation", a.ID, from)
	}
	return fmt.Sprintf("attempt %s history cannot be established from the retained records", a.ID)
}

func breakerCompletenessReason(b *BreakerWorld, from string, partial bool) string {
	if from != "closed" {
		return fmt.Sprintf("breaker %s history begins at %q instead of the initial state %q", b.Upstream, from, "closed")
	}
	if partial {
		return fmt.Sprintf("breaker %s history begins at %q but the ledger head is truncated, so an earlier open/close cycle cannot be excluded", b.Upstream, from)
	}
	return fmt.Sprintf("breaker %s history cannot be established from the retained records", b.Upstream)
}

// blockRequestRefs returns the request identities a block explicitly carries in
// typed fields. Prose descriptions are never inspected.
func blockRequestRefs(b *Block) []string {
	var refs []string
	if b.Event != nil {
		if id := requestRefOfEvent(b.Event); id != "" {
			refs = append(refs, id)
		}
	}
	for _, n := range b.Notes {
		switch n.Kind {
		case "request-transition", "attempt-created":
			if n.Detail.Request != "" {
				refs = append(refs, n.Detail.Request)
			}
		case "cancellation":
			// Typed, but a cancellation of an unknown request creates no
			// request entity; it is still an explicit request reference.
			if n.Detail.Request != "" {
				refs = append(refs, n.Detail.Request)
			}
		}
	}
	return refs
}

// requestRefOfEvent reports which event kinds carry a typed request identity.
func requestRefOfEvent(ev *Record) string {
	switch ev.Kind {
	case "AdmissionDecided", "LoadShed", "DeadlineExpired",
		"RetryScheduled", "RetryExecution", "CancellationRequested",
		"CancellationAcknowledged":
		return ev.Detail.Request
	}
	return ""
}

func (w *World) request(id string, rec *Record) (*RequestWorld, bool) {
	if r, ok := w.RequestByID[id]; ok {
		return r, false
	}
	r := &RequestWorld{
		ID:              id,
		FirstSeenSeq:    rec.EffectSeq,
		FirstSeenEngine: rec.EngineSeq,
		FirstSeenTimeNS: rec.SimTimeNS,
	}
	w.RequestByID[id] = r
	w.Requests = append(w.Requests, r)
	return r, true
}

func (w *World) attempt(id string, rec *Record) (*AttemptWorld, bool) {
	if a, ok := w.AttemptByID[id]; ok {
		return a, false
	}
	a := &AttemptWorld{
		ID:              id,
		FirstSeenSeq:    rec.EffectSeq,
		FirstSeenEngine: rec.EngineSeq,
		FirstSeenTimeNS: rec.SimTimeNS,
	}
	w.AttemptByID[id] = a
	w.Attempts = append(w.Attempts, a)
	return a, true
}

func (w *World) breaker(upstream string, rec *Record) (*BreakerWorld, bool) {
	if b, ok := w.BreakerByUpstream[upstream]; ok {
		return b, false
	}
	b := &BreakerWorld{Upstream: upstream}
	w.BreakerByUpstream[upstream] = b
	w.Breakers = append(w.Breakers, b)
	return b, true
}

// attachEventIdentity records tenant/upstream from the typed AdmissionDecided
// payload. Conflicting identities are uninterpretable evidence.
func (w *World) attachEventIdentity(r *RequestWorld, ev *Record) {
	if r == nil || ev.Kind != "AdmissionDecided" {
		return
	}
	// AdmissionDecided is the request's typed creation evidence: the reference
	// emits it exactly once per request, after the request exists. It is what
	// lets a truncated window distinguish "begins at admitted" from "begins at
	// admitted because everything earlier was evicted".
	r.CreatedSeen = true
	if t := ev.Detail.Tenant; t != "" {
		if r.Tenant != "" && r.Tenant != t {
			w.fatalAt(ev, "L-identity-conflict", "request %s is attributed to tenant %q and %q", r.ID, r.Tenant, t)
			return
		}
		r.Tenant = t
	}
	if u := ev.Detail.Upstream; u != "" {
		if r.Upstream != "" && r.Upstream != u {
			w.fatalAt(ev, "L-identity-conflict", "request %s is attributed to upstream %q and %q", r.ID, r.Upstream, u)
			return
		}
		r.Upstream = u
	}
}

func (w *World) fatal(code, format string, args ...any) {
	w.Fatal = append(w.Fatal, Finding{
		Code:           code,
		Message:        fmt.Sprintf(format, args...),
		Classification: ClassEvidenceDefect,
	})
}

func (w *World) fatalAt(rec *Record, code, format string, args ...any) {
	f := Finding{
		Code:           code,
		Message:        fmt.Sprintf(format, args...),
		Classification: ClassEvidenceDefect,
	}
	if rec != nil {
		f.EffectSeq = rec.EffectSeq
		f.EngineSeq = rec.EngineSeq
		if rec.Detail.Request != "" {
			f.Request = rec.Detail.Request
		}
		if rec.Detail.Attempt != "" {
			f.Attempt = rec.Detail.Attempt
		}
	}
	w.Fatal = append(w.Fatal, f)
}

// attemptCreated consumes the typed creation effect (D1 additive evidence):
// parent request, tenant, upstream and admission lane are carried as typed
// fields, so attribution never depends on prose.
//
// Compatibility: evidence recorded before the D1 note typing carries an
// attempt-created note with only a prose description. Such a note establishes
// nothing typed and is ignored here; the attempt is then reconstructed from its
// first attempt-transition, exactly as before. A note that carries some typed
// identity but no attempt identity is contradictory evidence.
func (w *World) attemptCreated(n Record, refs []string) {
	if n.Detail.Attempt == "" {
		if n.Detail.Request != "" || n.Detail.Tenant != "" || n.Detail.Upstream != "" || n.Detail.Lane != "" {
			w.fatalAt(&n, "L-attempt-identity-missing", "attempt creation carries typed facts but no attempt identity")
		}
		return
	}
	a, isNew := w.attempt(n.Detail.Attempt, &n)
	if a.CreatedSeen {
		w.fatalAt(&n, "L-attempt-created-twice", "attempt %s records more than one creation effect", a.ID)
		return
	}
	a.CreatedSeen = true
	a.CreatedAtNS = n.SimTimeNS
	if a.State == "" {
		a.State = "created"
		a.Complete = true
	}
	if n.Detail.Tenant != "" {
		a.Tenant = n.Detail.Tenant
	}
	if n.Detail.Upstream != "" {
		a.Upstream = n.Detail.Upstream
	}
	if n.Detail.Lane != "" {
		a.Lane = n.Detail.Lane
	}
	if req := n.Detail.Request; req != "" {
		if a.RequestLinked && a.Request != req {
			w.fatalAt(&n, "L-identity-conflict", "attempt %s is attributed to request %q and %q", a.ID, a.Request, req)
			return
		}
		if !a.RequestLinked {
			a.RequestLinked = true
			w.Linked++
		}
		a.Request = req
		if r, ok := w.RequestByID[req]; ok && !containsString(r.AttemptIDs, a.ID) {
			r.AttemptIDs = append(r.AttemptIDs, a.ID)
		}
	} else if isNew {
		w.linkAttempt(a, refs)
	}
}

func (w *World) requestTransition(n Record) {
	if n.Detail.Request == "" {
		w.fatalAt(&n, "L-request-identity-missing", "request transition carries no request identity")
		return
	}
	r, _ := w.request(n.Detail.Request, &n)
	from, to := n.Detail.From, n.Detail.To
	if !requestStates[from] || !requestStates[to] {
		w.fatalAt(&n, "L-request-state-unknown", "request %s transition names unknown states %q -> %q", r.ID, from, to)
		return
	}

	if r.State == "" {
		r.Complete = from == "admitted" && (r.CreatedSeen || !w.Partial)
		if !r.Complete {
			w.completenessGap(requestCompletenessReason(r, from, w.Partial))
		}
	} else if r.State != from {
		w.ContinuityViolations = append(w.ContinuityViolations, Finding{
			Code:           "L-request-history-discontinuity",
			Message:        fmt.Sprintf("request %s transition starts from %q but the reconstructed state is %q", r.ID, from, r.State),
			EffectSeq:      n.EffectSeq,
			EngineSeq:      n.EngineSeq,
			Request:        r.ID,
			Expected:       r.State,
			Observed:       from,
			Classification: ClassUnclassified,
		})
	}

	if !validRequestTransition(from, to) {
		w.RequestViolations = append(w.RequestViolations, Finding{
			Code:           "L-request-transition-illegal",
			Message:        fmt.Sprintf("request %s records an illegal transition %q -> %q", r.ID, from, to),
			EffectSeq:      n.EffectSeq,
			EngineSeq:      n.EngineSeq,
			Request:        r.ID,
			Expected:       "a documented transition",
			Observed:       from + " -> " + to,
			Classification: ClassReferenceDefectCandidate,
		})
	}

	r.Transitions = append(r.Transitions, Transition{
		From: from, To: to, TimeNS: n.SimTimeNS, EffectSeq: n.EffectSeq, EngineSeq: n.EngineSeq,
	})
	r.State = to
	if requestTerminal[to] {
		r.Terminal = true
		r.TerminalAtNS = n.SimTimeNS
	}
}

func (w *World) attemptTransition(n Record, refs []string) {
	if n.Detail.Attempt == "" {
		w.fatalAt(&n, "L-attempt-identity-missing", "attempt transition carries no attempt identity")
		return
	}
	a, isNew := w.attempt(n.Detail.Attempt, &n)
	from, to := n.Detail.From, n.Detail.To
	if !attemptStates[from] || !attemptStates[to] {
		w.fatalAt(&n, "L-attempt-state-unknown", "attempt %s transition names unknown states %q -> %q", a.ID, from, to)
		return
	}

	if isNew {
		a.Complete = from == "created" && (a.CreatedSeen || !w.Partial)
		if !a.Complete {
			w.completenessGap(attemptCompletenessReason(a, from, w.Partial))
		}
		w.linkAttempt(a, refs)
	} else if a.State != from {
		w.ContinuityViolations = append(w.ContinuityViolations, Finding{
			Code:           "L-attempt-history-discontinuity",
			Message:        fmt.Sprintf("attempt %s transition starts from %q but the reconstructed state is %q", a.ID, from, a.State),
			EffectSeq:      n.EffectSeq,
			EngineSeq:      n.EngineSeq,
			Attempt:        a.ID,
			Expected:       a.State,
			Observed:       from,
			Classification: ClassUnclassified,
		})
	}

	if !validAttemptTransition(from, to) {
		w.AttemptViolations = append(w.AttemptViolations, Finding{
			Code:           "L-attempt-transition-illegal",
			Message:        fmt.Sprintf("attempt %s records an illegal transition %q -> %q", a.ID, from, to),
			EffectSeq:      n.EffectSeq,
			EngineSeq:      n.EngineSeq,
			Attempt:        a.ID,
			Expected:       "a documented transition",
			Observed:       from + " -> " + to,
			Classification: ClassReferenceDefectCandidate,
		})
	}

	a.Transitions = append(a.Transitions, Transition{
		From: from, To: to, TimeNS: n.SimTimeNS, EffectSeq: n.EffectSeq, EngineSeq: n.EngineSeq,
	})
	a.State = to
	if attemptTerminal[to] {
		a.Terminal = true
		a.FinishedAtNS = n.SimTimeNS
	}
}

// linkAttempt attributes a first-seen attempt to the single request its block
// explicitly references. This is the fallback for evidence written before the
// typed creation effect: a block that creates one attempt and names one request
// admits exactly one interpretation. Ambiguous or absent references leave the
// link unknown (an evidence gap, never a guess).
func (w *World) linkAttempt(a *AttemptWorld, refs []string) {
	if a.RequestLinked {
		return
	}
	if len(refs) != 1 {
		w.Unlinked++
		return
	}
	a.Request = refs[0]
	a.RequestLinked = true
	w.Linked++
	if r, ok := w.RequestByID[refs[0]]; ok && !containsString(r.AttemptIDs, a.ID) {
		r.AttemptIDs = append(r.AttemptIDs, a.ID)
	}
}

func (w *World) breakerTransition(ev *Record) {
	up := ev.Detail.Upstream
	from, to := ev.Detail.From, ev.Detail.To
	if up == "" {
		w.fatalAt(ev, "L-breaker-identity-missing", "breaker transition carries no upstream identity")
		return
	}
	if !breakerStateSet[from] || !breakerStateSet[to] {
		w.fatalAt(ev, "L-breaker-state-unknown", "breaker %s names unknown states %q -> %q", up, from, to)
		return
	}
	b, isNew := w.breaker(up, ev)
	if isNew {
		b.Complete = from == "closed" && !w.Partial
		if !b.Complete {
			w.completenessGap(breakerCompletenessReason(b, from, w.Partial))
		}
	} else if b.State != from {
		w.ContinuityViolations = append(w.ContinuityViolations, Finding{
			Code:           "L-breaker-history-discontinuity",
			Message:        fmt.Sprintf("breaker %s transition starts from %q but the reconstructed state is %q", up, from, b.State),
			EffectSeq:      ev.EffectSeq,
			EngineSeq:      ev.EngineSeq,
			Upstream:       up,
			Expected:       b.State,
			Observed:       from,
			Classification: ClassUnclassified,
		})
	}
	if !validBreakerTransition(from, to) {
		w.BreakerViolations = append(w.BreakerViolations, Finding{
			Code:           "L-breaker-transition-illegal",
			Message:        fmt.Sprintf("breaker %s records an illegal transition %q -> %q", up, from, to),
			EffectSeq:      ev.EffectSeq,
			EngineSeq:      ev.EngineSeq,
			Upstream:       up,
			Expected:       "a documented breaker transition",
			Observed:       from + " -> " + to,
			Classification: ClassReferenceDefectCandidate,
		})
	}
	b.Transitions = append(b.Transitions, Transition{
		From: from, To: to, TimeNS: ev.SimTimeNS, EffectSeq: ev.EffectSeq, EngineSeq: ev.EngineSeq,
	})
	b.State = to
}

// L-path property checks. Each is a plain function over the reconstructed
// world; the registry owns ordering, evidence policy and counterexamples.
//
// An empty observation set is an evidence gap, not a vacuous pass: a ledger that
// never observed a request or an attempt cannot establish that its lifecycle was
// followed. Breaker lifecycle is different — a complete ledger with no breaker
// transition establishes that the breaker never left "closed", which is itself a
// lifecycle observation (the snapshot section corroborates the final state).
func checkRequestLifecycle(ctx *evalContext) PropertyResult {
	w := ctx.world
	r := newCheck("L-request-lifecycle", "logical request transitions follow the documented lifecycle")
	if len(w.Requests) == 0 {
		r.addGap("the ledger established no logical-request lifecycle; there is nothing for the lifecycle contract to apply to")
	}
	for _, f := range w.RequestViolations {
		r.fail(f)
	}
	for _, f := range w.ContinuityViolations {
		if f.Code == "L-request-history-discontinuity" {
			r.fail(f)
		}
	}
	return *r
}

func checkAttemptLifecycle(ctx *evalContext) PropertyResult {
	w := ctx.world
	r := newCheck("L-attempt-lifecycle", "physical attempt transitions follow the documented lifecycle")
	if len(w.Attempts) == 0 {
		r.addGap("the ledger established no physical-attempt lifecycle; there is nothing for the lifecycle contract to apply to")
	}
	for _, f := range w.AttemptViolations {
		r.fail(f)
	}
	for _, f := range w.ContinuityViolations {
		if f.Code == "L-attempt-history-discontinuity" || f.Code == "L-attempt-created-twice" {
			r.fail(f)
		}
	}
	return *r
}

func checkBreakerLifecycle(ctx *evalContext) PropertyResult {
	w := ctx.world
	r := newCheck("L-breaker-lifecycle", "breaker transitions are legal and contiguous")
	for _, f := range w.BreakerViolations {
		r.fail(f)
	}
	for _, f := range w.ContinuityViolations {
		if f.Code == "L-breaker-history-discontinuity" {
			r.fail(f)
		}
	}
	for _, b := range w.Breakers {
		if !b.Complete {
			r.addGap(fmt.Sprintf("breaker %s history is incomplete in the retained window", b.Upstream))
		}
	}
	return *r
}

func checkHistoryCompleteness(ctx *evalContext) PropertyResult {
	r := newCheck("L-history-completeness", "reconstruction starts at documented initial states")
	for _, g := range ctx.world.Gaps {
		if g.Property == "L-history-completeness" {
			r.addGap(g.Reason)
		}
	}
	return *r
}

func checkAttemptLinkage(ctx *evalContext) PropertyResult {
	w := ctx.world
	r := newCheck("L-attempt-linkage", "attempts are attributable to a logical request")
	if len(w.Attempts) == 0 {
		r.addGap("the ledger established no attempt parentage; there is nothing to attribute")
	}
	if w.Unlinked > 0 {
		r.addGap(fmt.Sprintf("%d attempt(s) first appear in blocks that do not name exactly one request; parentage is not establishable from the retained ledger", w.Unlinked))
	}
	return *r
}

func checkRetryBudgetLedger(ctx *evalContext) PropertyResult {
	w := ctx.world
	r := newCheck("L-retry-budget-ledger", "each recorded retry decision consumed exactly one retry-budget token")
	if w.BudgetConsumptions != w.RetryDecisions {
		r.fail(Finding{
			Code:           "L-retry-budget-ledger",
			Message:        fmt.Sprintf("the ledger records %d retry decisions but %d budget consumptions", w.RetryDecisions, w.BudgetConsumptions),
			Expected:       fmt.Sprintf("budget consumptions=%d", w.RetryDecisions),
			Observed:       fmt.Sprintf("budget consumptions=%d", w.BudgetConsumptions),
			Classification: ClassReferenceDefectCandidate,
		})
	}
	return *r
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func distinctStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

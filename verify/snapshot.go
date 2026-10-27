package verify

import "fmt"

// SnapshotSchemaVersion is the verifier snapshot schema version.
const SnapshotSchemaVersion = 1

// Snapshot is the independent verifier-side representation of the reference
// books at the end of a run. It is evidence, not authority: a structurally
// plausible snapshot may still be wrong, which is exactly what the S-path
// checks test.
type Snapshot struct {
	SchemaVersion int   `json:"schema_version"`
	TakenAtNS     int64 `json:"taken_at_ns"`

	Requests       []RequestFact        `json:"requests"`
	Attempts       []AttemptFact        `json:"attempts"`
	WorkerPools    []WorkerPoolFact     `json:"worker_pools"`
	Connections    []ConnectionPoolFact `json:"connections"`
	Queues         []QueueFact          `json:"queues"`
	Pending        []PendingFact        `json:"pending"`
	TenantActive   []TenantActiveFact   `json:"tenant_active"`
	Outcomes       []OutcomeFact        `json:"outcomes"`
	Budgets        []BudgetFact         `json:"budgets"`
	TokensConsumed []TokenConsumedFact  `json:"retry_tokens_consumed"`
	Breakers       []BreakerFact        `json:"breakers"`
}

// RequestFact is one logical request in the final books.
type RequestFact struct {
	ID           string   `json:"id"`
	Tenant       string   `json:"tenant"`
	Upstream     string   `json:"upstream"`
	State        string   `json:"state"`
	ArrivedAtNS  int64    `json:"arrived_at_ns"`
	DeadlineNS   int64    `json:"deadline_ns"`
	TerminalAtNS int64    `json:"terminal_at_ns"`
	MaxRetries   int      `json:"max_retries"`
	RetriesUsed  int      `json:"retries_used"`
	Attempts     []string `json:"attempts"`
}

// AttemptFact is one physical attempt in the final books.
type AttemptFact struct {
	ID           string `json:"id"`
	Request      string `json:"request"`
	Tenant       string `json:"tenant"`
	Upstream     string `json:"upstream"`
	State        string `json:"state"`
	Outcome      string `json:"outcome"`
	CreatedAtNS  int64  `json:"created_at_ns"`
	FinishedAtNS int64  `json:"finished_at_ns"`
	TimeoutNS    int64  `json:"timeout_ns"`
	Worker       string `json:"worker"`
	HasWorker    bool   `json:"has_worker"`
	ConnHeld     bool   `json:"conn_held"`
	IsProbe      bool   `json:"is_probe"`
	IsRetry      bool   `json:"is_retry"`
	Orphaned     bool   `json:"orphaned"`
	OrphanedAtNS int64  `json:"orphaned_at_ns"`
}

// WorkerPoolFact is one worker pool: the busy map and the idle free list, in
// the reference's own order.
type WorkerPoolFact struct {
	ID   string           `json:"id"`
	Size int              `json:"size"`
	Busy []WorkerBusyFact `json:"busy"`
	Idle []string         `json:"idle"`
}

// WorkerBusyFact is one worker-to-attempt binding.
type WorkerBusyFact struct {
	Worker  string `json:"worker"`
	Attempt string `json:"attempt"`
}

// ConnectionPoolFact is one upstream's connection pool. Held and Waiting are
// both explicit: identity of holders and waitlist order are evidence.
type ConnectionPoolFact struct {
	Upstream string   `json:"upstream"`
	Capacity int      `json:"capacity"`
	Held     []string `json:"held"`
	Waiting  []string `json:"waiting"`
}

// QueueFact is one tenant queue, in FIFO order.
type QueueFact struct {
	Tenant  string           `json:"tenant"`
	Entries []QueueEntryFact `json:"entries"`
}

// QueueEntryFact is one queue entry with its global insertion sequence.
type QueueEntryFact struct {
	Attempt      string `json:"attempt"`
	EnqueuedAtNS int64  `json:"enqueued_at_ns"`
	Seq          uint64 `json:"seq"`
}

// PendingFact is one admission accepted but not yet inserted into its queue.
type PendingFact struct {
	Tenant  string `json:"tenant"`
	Attempt string `json:"attempt"`
	// Kind is "arrival" or "retry", the lane whose pending book holds it.
	Kind string `json:"kind"`
}

// TenantActiveFact is the admitted, non-terminal request count per tenant.
type TenantActiveFact struct {
	Tenant string `json:"tenant"`
	Active int    `json:"active"`
}

// OutcomeFact is the per-tenant attempt outcome ledger.
type OutcomeFact struct {
	Tenant    string `json:"tenant"`
	Useful    int    `json:"useful"`
	Wasted    int    `json:"wasted"`
	Discarded int    `json:"discarded"`
}

// BudgetFact is one (tenant, upstream) retry bucket.
type BudgetFact struct {
	Tenant           string `json:"tenant"`
	Upstream         string `json:"upstream"`
	Capacity         int    `json:"capacity"`
	Tokens           int    `json:"tokens"`
	RefillIntervalNS int64  `json:"refill_interval_ns"`
	LastRefillNS     int64  `json:"last_refill_ns"`
}

// TokenConsumedFact is the aggregate consumed-token ledger for one bucket.
type TokenConsumedFact struct {
	Tenant   string `json:"tenant"`
	Upstream string `json:"upstream"`
	Consumed int    `json:"consumed"`
}

// BreakerFact is one upstream circuit breaker.
type BreakerFact struct {
	Upstream            string `json:"upstream"`
	State               string `json:"state"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	ProbesActive        int    `json:"probes_active"`
	OpenedAtNS          int64  `json:"opened_at_ns"`
}

// validateSnapshot performs structural validation: schema, identity integrity,
// configuration agreement and internal reference consistency. Any finding here
// means the snapshot cannot be used as evidence (INVALID_INPUT); it does not
// attempt to decide whether the reference books are correct.
func validateSnapshot(s *Snapshot, in *Input) []Finding {
	var fs []Finding
	add := func(code, format string, args ...any) {
		fs = append(fs, Finding{Code: code, Message: fmt.Sprintf(format, args...), Classification: ClassEvidenceDefect})
	}
	addRef := func(code, ref, format string, args ...any) {
		fs = append(fs, Finding{Code: code, Message: fmt.Sprintf(format, args...), Classification: ClassEvidenceDefect, SnapshotRef: ref})
	}

	if s.SchemaVersion != SnapshotSchemaVersion {
		add("snapshot.schema_version", "snapshot schema version %d is not %d", s.SchemaVersion, SnapshotSchemaVersion)
		return fs
	}
	if s.TakenAtNS != in.HorizonNS {
		add("snapshot.horizon_mismatch", "snapshot taken at %dns but the input horizon is %dns", s.TakenAtNS, in.HorizonNS)
	}

	reqByID, reqOK := indexRequests(s.Requests, add)
	attByID, attOK := indexAttempts(s.Attempts, add)
	poolByID, _ := indexPools(s.WorkerPools, add)
	connByUp, _ := indexConnections(s.Connections, add)
	queueByTenant, _ := indexQueues(s.Queues, add)
	budgetByKey, _ := indexBudgets(s.Budgets, add)
	breakerByUp, _ := indexBreakers(s.Breakers, add)

	if reqOK {
		// Config agreement: the snapshot must describe the run the input
		// declares, otherwise every conservation claim is about another run.
		for id := range reqByID {
			r := reqByID[id]
			if !hasTenant(in, r.Tenant) {
				addRef("snapshot.unknown_tenant", "request:"+id, "request %s names tenant %q which the input does not configure", id, r.Tenant)
			}
			if !hasUpstream(in, r.Upstream) {
				addRef("snapshot.unknown_upstream", "request:"+id, "request %s names upstream %q which the input does not configure", id, r.Upstream)
			}
			if !requestStates[r.State] {
				addRef("snapshot.unknown_state", "request:"+id, "request %s has unknown state %q", id, r.State)
			}
			if r.ArrivedAtNS < 0 || r.DeadlineNS < r.ArrivedAtNS {
				addRef("snapshot.invalid_time", "request:"+id, "request %s has arrival %dns and deadline %dns", id, r.ArrivedAtNS, r.DeadlineNS)
			}
			if r.MaxRetries < 0 || r.RetriesUsed < 0 || r.RetriesUsed > r.MaxRetries {
				addRef("snapshot.invalid_retries", "request:"+id, "request %s has retries used %d of max %d", id, r.RetriesUsed, r.MaxRetries)
			}
			seen := map[string]bool{}
			for _, aid := range r.Attempts {
				if seen[aid] {
					addRef("snapshot.duplicate_reference", "request:"+id, "request %s lists attempt %s twice", id, aid)
				}
				seen[aid] = true
				if attOK {
					if a, ok := attByID[aid]; !ok {
						addRef("snapshot.dangling_reference", "request:"+id, "request %s lists unknown attempt %s", id, aid)
					} else if a.Request != id {
						addRef("snapshot.reference_mismatch", "request:"+id, "request %s lists attempt %s which belongs to %q", id, aid, a.Request)
					}
				}
			}
		}
	}

	if attOK {
		for id := range attByID {
			a := attByID[id]
			if !attemptStates[a.State] {
				addRef("snapshot.unknown_state", "attempt:"+id, "attempt %s has unknown state %q", id, a.State)
			}
			if !outcomes[a.Outcome] {
				addRef("snapshot.unknown_outcome", "attempt:"+id, "attempt %s has unknown outcome %q", id, a.Outcome)
			}
			if a.CreatedAtNS < 0 {
				addRef("snapshot.invalid_time", "attempt:"+id, "attempt %s has negative creation time", id)
			}
			if reqOK {
				r, ok := reqByID[a.Request]
				if !ok {
					addRef("snapshot.dangling_reference", "attempt:"+id, "attempt %s names unknown request %q", id, a.Request)
				} else {
					if r.Tenant != a.Tenant || r.Upstream != a.Upstream {
						addRef("snapshot.reference_mismatch", "attempt:"+id, "attempt %s tenant/upstream (%s/%s) disagree with its request (%s/%s)", id, a.Tenant, a.Upstream, r.Tenant, r.Upstream)
					}
					if !containsString(r.Attempts, id) {
						addRef("snapshot.reference_mismatch", "attempt:"+id, "attempt %s is absent from request %s's attempt list", id, a.Request)
					}
				}
			}
			if !hasTenant(in, a.Tenant) {
				addRef("snapshot.unknown_tenant", "attempt:"+id, "attempt %s names tenant %q which the input does not configure", id, a.Tenant)
			}
			if !hasUpstream(in, a.Upstream) {
				addRef("snapshot.unknown_upstream", "attempt:"+id, "attempt %s names upstream %q which the input does not configure", id, a.Upstream)
			}
		}
	}

	if poolByID != nil {
		if p, ok := poolByID[in.WorkerPool.ID]; !ok {
			add("snapshot.pool_missing", "snapshot has no worker pool %q", in.WorkerPool.ID)
		} else if p.Size != in.WorkerPool.Size {
			addRef("snapshot.config_mismatch", "pool:"+p.ID, "worker pool %s has size %d but the input configures %d", p.ID, p.Size, in.WorkerPool.Size)
		}
		for id, p := range poolByID {
			if p.Size <= 0 {
				addRef("snapshot.invalid_capacity", "pool:"+id, "worker pool %s has non-positive size %d", id, p.Size)
			}
		}
	}
	if connByUp != nil {
		for _, c := range in.ConnectionPools {
			p, ok := connByUp[c.Upstream]
			if !ok {
				add("snapshot.pool_missing", "snapshot has no connection pool for upstream %q", c.Upstream)
				continue
			}
			if p.Capacity != c.Capacity {
				addRef("snapshot.config_mismatch", "conn:"+p.Upstream, "connection pool %s has capacity %d but the input configures %d", p.Upstream, p.Capacity, c.Capacity)
			}
		}
	}
	if queueByTenant != nil {
		for _, t := range in.Tenants {
			if _, ok := queueByTenant[t.ID]; !ok {
				add("snapshot.queue_missing", "snapshot has no queue for tenant %q", t.ID)
			}
		}
	}
	if budgetByKey != nil {
		for _, b := range s.Budgets {
			if b.Capacity != in.RetryBudget.Capacity {
				addRef("snapshot.config_mismatch", budgetRef(b.Tenant, b.Upstream), "budget (%s,%s) has capacity %d but the input configures %d", b.Tenant, b.Upstream, b.Capacity, in.RetryBudget.Capacity)
			}
			if b.RefillIntervalNS != in.RetryBudget.RefillIntervalNS {
				addRef("snapshot.config_mismatch", budgetRef(b.Tenant, b.Upstream), "budget (%s,%s) has refill interval %dns but the input configures %dns", b.Tenant, b.Upstream, b.RefillIntervalNS, in.RetryBudget.RefillIntervalNS)
			}
		}
	}
	if breakerByUp != nil {
		for _, c := range in.ConnectionPools {
			if _, ok := breakerByUp[c.Upstream]; !ok {
				add("snapshot.breaker_missing", "snapshot has no breaker for upstream %q", c.Upstream)
			}
		}
	}
	for _, p := range s.Pending {
		if !hasTenant(in, p.Tenant) {
			addRef("snapshot.unknown_tenant", "pending:"+p.Attempt, "pending attempt %s names tenant %q which the input does not configure", p.Attempt, p.Tenant)
		}
		if p.Kind != "arrival" && p.Kind != "retry" {
			addRef("snapshot.invalid_pending", "pending:"+p.Attempt, "pending attempt %s has unknown lane %q", p.Attempt, p.Kind)
		}
		if attOK {
			a, ok := attByID[p.Attempt]
			if !ok {
				addRef("snapshot.dangling_reference", "pending:"+p.Attempt, "pending book names unknown attempt %s", p.Attempt)
			} else {
				if a.State != "created" {
					addRef("snapshot.reference_mismatch", "pending:"+p.Attempt, "pending attempt %s is in state %q, not created", p.Attempt, a.State)
				}
				if (p.Kind == "retry") != a.IsRetry {
					addRef("snapshot.reference_mismatch", "pending:"+p.Attempt, "pending lane %q disagrees with attempt lane (is_retry=%t)", p.Kind, a.IsRetry)
				}
			}
		}
	}
	return fs
}

func indexRequests(in []RequestFact, add func(string, string, ...any)) (map[string]*RequestFact, bool) {
	m := make(map[string]*RequestFact, len(in))
	for i := range in {
		r := &in[i]
		if r.ID == "" {
			add("snapshot.empty_id", "request fact %d has no identity", i)
			return m, false
		}
		if _, dup := m[r.ID]; dup {
			add("snapshot.duplicate_id", "request %s is listed more than once", r.ID)
			return m, false
		}
		m[r.ID] = r
	}
	return m, true
}

func indexAttempts(in []AttemptFact, add func(string, string, ...any)) (map[string]*AttemptFact, bool) {
	m := make(map[string]*AttemptFact, len(in))
	for i := range in {
		a := &in[i]
		if a.ID == "" {
			add("snapshot.empty_id", "attempt fact %d has no identity", i)
			return m, false
		}
		if _, dup := m[a.ID]; dup {
			add("snapshot.duplicate_id", "attempt %s is listed more than once", a.ID)
			return m, false
		}
		m[a.ID] = a
	}
	return m, true
}

func indexPools(in []WorkerPoolFact, add func(string, string, ...any)) (map[string]*WorkerPoolFact, bool) {
	m := make(map[string]*WorkerPoolFact, len(in))
	for i := range in {
		p := &in[i]
		if p.ID == "" {
			add("snapshot.empty_id", "worker pool fact %d has no identity", i)
			return m, false
		}
		if _, dup := m[p.ID]; dup {
			add("snapshot.duplicate_id", "worker pool %s is listed more than once", p.ID)
			return m, false
		}
		m[p.ID] = p
	}
	return m, true
}

func indexConnections(in []ConnectionPoolFact, add func(string, string, ...any)) (map[string]*ConnectionPoolFact, bool) {
	m := make(map[string]*ConnectionPoolFact, len(in))
	for i := range in {
		p := &in[i]
		if p.Upstream == "" {
			add("snapshot.empty_id", "connection pool fact %d has no upstream", i)
			return m, false
		}
		if _, dup := m[p.Upstream]; dup {
			add("snapshot.duplicate_id", "connection pool %s is listed more than once", p.Upstream)
			return m, false
		}
		m[p.Upstream] = p
	}
	return m, true
}

func indexQueues(in []QueueFact, add func(string, string, ...any)) (map[string]*QueueFact, bool) {
	m := make(map[string]*QueueFact, len(in))
	for i := range in {
		q := &in[i]
		if q.Tenant == "" {
			add("snapshot.empty_id", "queue fact %d has no tenant", i)
			return m, false
		}
		if _, dup := m[q.Tenant]; dup {
			add("snapshot.duplicate_id", "queue for tenant %s is listed more than once", q.Tenant)
			return m, false
		}
		m[q.Tenant] = q
	}
	return m, true
}

func indexBudgets(in []BudgetFact, add func(string, string, ...any)) (map[string]*BudgetFact, bool) {
	m := make(map[string]*BudgetFact, len(in))
	for i := range in {
		b := &in[i]
		key := budgetRef(b.Tenant, b.Upstream)
		if b.Tenant == "" || b.Upstream == "" {
			add("snapshot.empty_id", "budget fact %d has no key", i)
			return m, false
		}
		if _, dup := m[key]; dup {
			add("snapshot.duplicate_id", "budget %s is listed more than once", key)
			return m, false
		}
		m[key] = b
	}
	return m, true
}

func indexBreakers(in []BreakerFact, add func(string, string, ...any)) (map[string]*BreakerFact, bool) {
	m := make(map[string]*BreakerFact, len(in))
	for i := range in {
		b := &in[i]
		if b.Upstream == "" {
			add("snapshot.empty_id", "breaker fact %d has no upstream", i)
			return m, false
		}
		if _, dup := m[b.Upstream]; dup {
			add("snapshot.duplicate_id", "breaker for %s is listed more than once", b.Upstream)
			return m, false
		}
		m[b.Upstream] = b
	}
	return m, true
}

func budgetRef(tenant, upstream string) string { return tenant + "/" + upstream }

func hasTenant(in *Input, id string) bool {
	for _, t := range in.Tenants {
		if t.ID == id {
			return true
		}
	}
	return false
}

func hasUpstream(in *Input, id string) bool {
	for _, u := range in.ConnectionPools {
		if u.Upstream == id {
			return true
		}
	}
	return false
}

func checkWorkerConservation(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	r := newCheck("S-worker-conservation", "worker pool size is conserved and partitions busy/idle")
	for pi := range s.WorkerPools {
		p := &s.WorkerPools[pi]
		if got := len(p.Busy) + len(p.Idle); got != p.Size {
			r.fail(Finding{
				Code:           "S-worker-conservation",
				Message:        fmt.Sprintf("worker pool %s has %d busy + %d idle entries but size %d", p.ID, len(p.Busy), len(p.Idle), p.Size),
				Worker:         "",
				Expected:       fmt.Sprintf("busy+idle=%d", p.Size),
				Observed:       fmt.Sprintf("busy+idle=%d", got),
				SnapshotRef:    "pool:" + p.ID,
				Classification: ClassUnclassified,
			})
		}
		seen := make(map[string]string, p.Size)
		for _, b := range p.Busy {
			if prev, dup := seen[b.Worker]; dup {
				r.fail(Finding{
					Code:           "S-worker-duplicate",
					Message:        fmt.Sprintf("worker pool %s lists worker %s in %s and busy on %s", p.ID, b.Worker, prev, b.Attempt),
					Worker:         b.Worker,
					Attempt:        b.Attempt,
					Expected:       "one binding per worker",
					Observed:       "duplicate worker entry",
					SnapshotRef:    "pool:" + p.ID,
					Classification: ClassUnclassified,
				})
				continue
			}
			seen[b.Worker] = "busy"
		}
		for _, w := range p.Idle {
			if prev, dup := seen[w]; dup {
				r.fail(Finding{
					Code:           "S-worker-duplicate",
					Message:        fmt.Sprintf("worker pool %s lists worker %s as %s and idle", p.ID, w, prev),
					Worker:         w,
					Expected:       "each worker is either busy or idle",
					Observed:       prev + " and idle",
					SnapshotRef:    "pool:" + p.ID,
					Classification: ClassUnclassified,
				})
				continue
			}
			seen[w] = "idle"
		}
	}
	return *r
}

func checkWorkerOwnership(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	attByID := ctx.attemptByID
	r := newCheck("S-worker-ownership", "worker holders and attempt worker fields agree")
	bound := make(map[string]int, len(s.Attempts))
	for pi := range s.WorkerPools {
		p := &s.WorkerPools[pi]
		for _, b := range p.Busy {
			bound[b.Attempt]++
			a, ok := attByID[b.Attempt]
			if !ok {
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("worker %s is busy on unknown attempt %s", b.Worker, b.Attempt),
					Worker:         b.Worker,
					Attempt:        b.Attempt,
					Expected:       "a registered attempt",
					Observed:       "unknown attempt",
					SnapshotRef:    "pool:" + p.ID,
					Classification: ClassUnclassified,
				})
				continue
			}
			if !a.HasWorker || a.Worker != b.Worker {
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("worker %s is bound to attempt %s but the attempt reports worker %q (has_worker=%t)", b.Worker, b.Attempt, a.Worker, a.HasWorker),
					Worker:         b.Worker,
					Attempt:        b.Attempt,
					Expected:       b.Worker,
					Observed:       fmt.Sprintf("worker=%q has_worker=%t", a.Worker, a.HasWorker),
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
			if !attemptHoldsWorker(a.State) {
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("attempt %s in state %q holds worker %s", a.ID, a.State, b.Worker),
					Worker:         b.Worker,
					Attempt:        a.ID,
					Expected:       "a worker-holding state",
					Observed:       a.State,
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.HasWorker {
			if a.Worker == "" {
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("attempt %s reports a worker but names none", a.ID),
					Attempt:        a.ID,
					Expected:       "a worker id",
					Observed:       "empty",
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
			switch bound[a.ID] {
			case 0:
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("attempt %s reports worker %s but no pool binds it", a.ID, a.Worker),
					Attempt:        a.ID,
					Worker:         a.Worker,
					Expected:       "exactly one pool binding",
					Observed:       "no binding",
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			case 1:
			default:
				r.fail(Finding{
					Code:           "S-worker-ownership",
					Message:        fmt.Sprintf("attempt %s is bound to %d workers", a.ID, bound[a.ID]),
					Attempt:        a.ID,
					Expected:       "exactly one pool binding",
					Observed:       fmt.Sprintf("%d bindings", bound[a.ID]),
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
			if !attemptHoldsWorker(a.State) {
				r.fail(Finding{
					Code:           "S-terminal-resource-retention",
					Message:        fmt.Sprintf("attempt %s in state %q still reports a worker", a.ID, a.State),
					Attempt:        a.ID,
					Worker:         a.Worker,
					Expected:       "no worker outside worker-holding states",
					Observed:       a.State,
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
		} else if a.Worker != "" {
			r.fail(Finding{
				Code:           "S-worker-ownership",
				Message:        fmt.Sprintf("attempt %s reports has_worker=false but names worker %q", a.ID, a.Worker),
				Attempt:        a.ID,
				Worker:         a.Worker,
				Expected:       "no worker id",
				Observed:       a.Worker,
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		} else if attemptHoldsWorker(a.State) {
			r.fail(Finding{
				Code:           "S-worker-ownership",
				Message:        fmt.Sprintf("attempt %s in state %q does not report a worker", a.ID, a.State),
				Attempt:        a.ID,
				Expected:       "a held worker",
				Observed:       "none",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkConnectionOwnership(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	attByID := ctx.attemptByID
	r := newCheck("S-connection-ownership", "connection holders and attempt connection fields agree")
	heldBy := make(map[string]int, len(s.Attempts))
	for ci := range s.Connections {
		p := &s.Connections[ci]
		if len(p.Held) > p.Capacity {
			r.fail(Finding{
				Code:           "S-connection-capacity",
				Message:        fmt.Sprintf("connection pool %s holds %d of %d connections", p.Upstream, len(p.Held), p.Capacity),
				Upstream:       p.Upstream,
				Expected:       fmt.Sprintf("held<=%d", p.Capacity),
				Observed:       fmt.Sprintf("held=%d", len(p.Held)),
				SnapshotRef:    "conn:" + p.Upstream,
				Classification: ClassUnclassified,
			})
		}
		for _, aid := range p.Held {
			heldBy[aid]++
			a, ok := attByID[aid]
			if !ok {
				r.fail(Finding{
					Code:           "S-connection-ownership",
					Message:        fmt.Sprintf("connection on %s is held by unknown attempt %s", p.Upstream, aid),
					Upstream:       p.Upstream,
					Attempt:        aid,
					Expected:       "a registered attempt",
					Observed:       "unknown attempt",
					SnapshotRef:    "conn:" + p.Upstream,
					Classification: ClassUnclassified,
				})
				continue
			}
			if !a.ConnHeld {
				r.fail(Finding{
					Code:           "S-connection-ownership",
					Message:        fmt.Sprintf("pool %s holds a connection for attempt %s which reports conn_held=false", p.Upstream, aid),
					Upstream:       p.Upstream,
					Attempt:        aid,
					Expected:       "conn_held=true",
					Observed:       "conn_held=false",
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
			if a.Upstream != p.Upstream {
				r.fail(Finding{
					Code:           "S-connection-ownership",
					Message:        fmt.Sprintf("attempt %s holds a connection from %s but targets %s", aid, p.Upstream, a.Upstream),
					Upstream:       p.Upstream,
					Attempt:        aid,
					Expected:       a.Upstream,
					Observed:       p.Upstream,
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
			if a.State != "running" {
				r.fail(Finding{
					Code:           "S-terminal-resource-retention",
					Message:        fmt.Sprintf("attempt %s holds a connection in state %q", aid, a.State),
					Attempt:        aid,
					Upstream:       p.Upstream,
					Expected:       "running",
					Observed:       a.State,
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.ConnHeld && heldBy[a.ID] == 0 {
			r.fail(Finding{
				Code:           "S-connection-ownership",
				Message:        fmt.Sprintf("attempt %s reports conn_held=true but no pool holds it", a.ID),
				Attempt:        a.ID,
				Expected:       "one holding pool",
				Observed:       "none",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
		if !a.ConnHeld && heldBy[a.ID] > 0 {
			r.fail(Finding{
				Code:           "S-connection-ownership",
				Message:        fmt.Sprintf("attempt %s is held by a pool but reports conn_held=false", a.ID),
				Attempt:        a.ID,
				Expected:       "conn_held=true",
				Observed:       "conn_held=false",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
		if a.State == "running" && !a.ConnHeld {
			r.fail(Finding{
				Code:           "S-connection-ownership",
				Message:        fmt.Sprintf("attempt %s is running without a connection", a.ID),
				Attempt:        a.ID,
				Expected:       "a held connection",
				Observed:       "none",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkWaitlists(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	attByID := ctx.attemptByID
	r := newCheck("S-waitlist-membership", "connection waitlists contain exactly the waiting attempts")
	waitingIn := make(map[string]int, len(s.Attempts))
	for ci := range s.Connections {
		p := &s.Connections[ci]
		seen := make(map[string]bool, len(p.Waiting))
		for _, aid := range p.Waiting {
			if seen[aid] {
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("waitlist on %s contains attempt %s twice", p.Upstream, aid),
					Upstream:       p.Upstream,
					Attempt:        aid,
					Expected:       "unique waitlist entries",
					Observed:       "duplicate entry",
					SnapshotRef:    "conn:" + p.Upstream,
					Classification: ClassUnclassified,
				})
				continue
			}
			seen[aid] = true
			waitingIn[aid]++
			a, ok := attByID[aid]
			if !ok {
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("waitlist on %s names unknown attempt %s", p.Upstream, aid),
					Upstream:       p.Upstream,
					Attempt:        aid,
					Expected:       "a registered attempt",
					Observed:       "unknown attempt",
					SnapshotRef:    "conn:" + p.Upstream,
					Classification: ClassUnclassified,
				})
				continue
			}
			if a.State != "waiting-connection" {
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("waitlisted attempt %s is in state %q", aid, a.State),
					Attempt:        aid,
					Upstream:       p.Upstream,
					Expected:       "waiting-connection",
					Observed:       a.State,
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
			if a.Upstream != p.Upstream {
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("attempt %s waits on %s but targets %s", aid, p.Upstream, a.Upstream),
					Attempt:        aid,
					Upstream:       p.Upstream,
					Expected:       a.Upstream,
					Observed:       p.Upstream,
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
			if a.ConnHeld {
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("waitlisted attempt %s also holds a connection", aid),
					Attempt:        aid,
					Upstream:       p.Upstream,
					Expected:       "no held connection",
					Observed:       "connection held",
					SnapshotRef:    "attempt:" + aid,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.State == "waiting-connection" {
			switch waitingIn[a.ID] {
			case 0:
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("attempt %s waits for a connection but appears on no waitlist", a.ID),
					Attempt:        a.ID,
					Expected:       "exactly one waitlist",
					Observed:       "none",
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			case 1:
			default:
				r.fail(Finding{
					Code:           "S-waitlist-membership",
					Message:        fmt.Sprintf("attempt %s appears on %d waitlists", a.ID, waitingIn[a.ID]),
					Attempt:        a.ID,
					Expected:       "exactly one waitlist",
					Observed:       fmt.Sprintf("%d waitlists", waitingIn[a.ID]),
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
		} else if waitingIn[a.ID] > 0 {
			r.fail(Finding{
				Code:           "S-waitlist-membership",
				Message:        fmt.Sprintf("attempt %s is waitlisted while in state %q", a.ID, a.State),
				Attempt:        a.ID,
				Expected:       "waiting-connection",
				Observed:       a.State,
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkConnectionStranding(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	r := newCheck("S-connection-stranding", "a free connection never coexists with a waiting attempt")
	for ci := range s.Connections {
		p := &s.Connections[ci]
		if len(p.Waiting) > 0 && len(p.Held) < p.Capacity {
			r.fail(Finding{
				Code:           "S-connection-stranding",
				Message:        fmt.Sprintf("upstream %s has %d waiting attempts while %d of %d connections are free", p.Upstream, len(p.Waiting), p.Capacity-len(p.Held), p.Capacity),
				Upstream:       p.Upstream,
				Expected:       "no free connection while attempts wait",
				Observed:       fmt.Sprintf("held=%d capacity=%d waiting=%d", len(p.Held), p.Capacity, len(p.Waiting)),
				SnapshotRef:    "conn:" + p.Upstream,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkQueues(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	attByID := ctx.attemptByID
	r := newCheck("S-queue-consistency", "queue entries reference the queued attempts, in sequence order")
	inQueue := make(map[string]int, len(s.Attempts))
	for qi := range s.Queues {
		q := &s.Queues[qi]
		var lastSeq uint64
		haveSeq := false
		seen := make(map[string]bool, len(q.Entries))
		for _, e := range q.Entries {
			if seen[e.Attempt] {
				r.fail(Finding{
					Code:           "S-queue-consistency",
					Message:        fmt.Sprintf("queue %s contains attempt %s twice", q.Tenant, e.Attempt),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       "unique queue entries",
					Observed:       "duplicate entry",
					SnapshotRef:    "queue:" + q.Tenant,
					Classification: ClassUnclassified,
				})
				continue
			}
			seen[e.Attempt] = true
			inQueue[e.Attempt]++
			if haveSeq && e.Seq <= lastSeq {
				r.fail(Finding{
					Code:           "S-queue-sequence",
					Message:        fmt.Sprintf("queue %s entry %s has sequence %d after %d", q.Tenant, e.Attempt, e.Seq, lastSeq),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       "strictly increasing sequence",
					Observed:       fmt.Sprintf("%d after %d", e.Seq, lastSeq),
					SnapshotRef:    "queue:" + q.Tenant,
					Classification: ClassUnclassified,
				})
			}
			lastSeq = e.Seq
			haveSeq = true
			a, ok := attByID[e.Attempt]
			if !ok {
				r.fail(Finding{
					Code:           "S-queue-consistency",
					Message:        fmt.Sprintf("queue %s names unknown attempt %s", q.Tenant, e.Attempt),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       "a registered attempt",
					Observed:       "unknown attempt",
					SnapshotRef:    "queue:" + q.Tenant,
					Classification: ClassUnclassified,
				})
				continue
			}
			if a.Tenant != q.Tenant {
				r.fail(Finding{
					Code:           "S-queue-consistency",
					Message:        fmt.Sprintf("attempt %s is queued on tenant %s but belongs to %s", e.Attempt, q.Tenant, a.Tenant),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       a.Tenant,
					Observed:       q.Tenant,
					SnapshotRef:    "attempt:" + e.Attempt,
					Classification: ClassUnclassified,
				})
			}
			if a.State != "queued" {
				r.fail(Finding{
					Code:           "S-queue-consistency",
					Message:        fmt.Sprintf("queued attempt %s is in state %q", e.Attempt, a.State),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       "queued",
					Observed:       a.State,
					SnapshotRef:    "attempt:" + e.Attempt,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if a.State == "queued" && inQueue[a.ID] == 0 {
			r.fail(Finding{
				Code:           "S-queue-consistency",
				Message:        fmt.Sprintf("attempt %s is queued but appears on no tenant queue", a.ID),
				Attempt:        a.ID,
				Tenant:         a.Tenant,
				Expected:       "exactly one queue entry",
				Observed:       "none",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
		if a.State == "queued" && inQueue[a.ID] > 1 {
			r.fail(Finding{
				Code:           "S-queue-consistency",
				Message:        fmt.Sprintf("attempt %s appears on %d tenant queues", a.ID, inQueue[a.ID]),
				Attempt:        a.ID,
				Expected:       "exactly one queue entry",
				Observed:       fmt.Sprintf("%d entries", inQueue[a.ID]),
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkLaneBounds(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	attByID := ctx.attemptByID
	tenants := ctx.tenants
	r := newCheck("S-lane-bounds", "tenant lane and total queue bounds hold including pending admissions")
	type usage struct {
		arrival int
		retry   int
		total   int
	}
	byTenant := make(map[string]*usage, len(tenants))
	for id := range tenants {
		byTenant[id] = &usage{}
	}
	for qi := range s.Queues {
		q := &s.Queues[qi]
		u := byTenant[q.Tenant]
		for _, e := range q.Entries {
			u.total++
			if a, ok := attByID[e.Attempt]; ok && a.IsRetry {
				u.retry++
			} else {
				u.arrival++
			}
		}
	}
	for _, p := range s.Pending {
		u := byTenant[p.Tenant]
		if u == nil {
			continue
		}
		u.total++
		if p.Kind == "retry" {
			u.retry++
		} else {
			u.arrival++
		}
	}
	for id, spec := range tenants {
		u := byTenant[id]
		if u.arrival > spec.QueueLane {
			r.fail(Finding{
				Code:           "S-lane-bounds",
				Message:        fmt.Sprintf("tenant %s uses %d arrival lane slots of %d", id, u.arrival, spec.QueueLane),
				Tenant:         id,
				Expected:       fmt.Sprintf("arrival<=%d", spec.QueueLane),
				Observed:       fmt.Sprintf("arrival=%d", u.arrival),
				SnapshotRef:    "queue:" + id,
				Classification: ClassUnclassified,
			})
		}
		if u.retry > spec.QueueRetry {
			r.fail(Finding{
				Code:           "S-lane-bounds",
				Message:        fmt.Sprintf("tenant %s uses %d retry lane slots of %d", id, u.retry, spec.QueueRetry),
				Tenant:         id,
				Expected:       fmt.Sprintf("retry<=%d", spec.QueueRetry),
				Observed:       fmt.Sprintf("retry=%d", u.retry),
				SnapshotRef:    "queue:" + id,
				Classification: ClassUnclassified,
			})
		}
		if u.total > spec.QueueTotal {
			r.fail(Finding{
				Code:           "S-lane-bounds",
				Message:        fmt.Sprintf("tenant %s uses %d total queue slots of %d", id, u.total, spec.QueueTotal),
				Tenant:         id,
				Expected:       fmt.Sprintf("total<=%d", spec.QueueTotal),
				Observed:       fmt.Sprintf("total=%d", u.total),
				SnapshotRef:    "queue:" + id,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkTenantActive(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	in := ctx.input
	r := newCheck("S-tenant-active", "tenant active-request counters match the request books")
	computed := make(map[string]int, len(in.Tenants))
	for i := range s.Requests {
		q := &s.Requests[i]
		if !requestTerminal[q.State] {
			computed[q.Tenant]++
		}
	}
	facts := make(map[string]int, len(s.TenantActive))
	for _, f := range s.TenantActive {
		facts[f.Tenant] = f.Active
	}
	for _, t := range in.Tenants {
		if got, want := facts[t.ID], computed[t.ID]; got != want {
			r.fail(Finding{
				Code:           "S-tenant-active",
				Message:        fmt.Sprintf("tenant %s reports %d active requests but the request books contain %d", t.ID, got, want),
				Tenant:         t.ID,
				Expected:       fmt.Sprintf("active=%d", want),
				Observed:       fmt.Sprintf("active=%d", got),
				SnapshotRef:    "tenant:" + t.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkOutcomeLedger(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	r := newCheck("S-outcome-ledger", "attempt outcomes are determined exactly once and match tenant counters")
	type counts struct{ useful, wasted, discarded int }
	computed := make(map[string]*counts, len(s.Outcomes))
	for _, f := range s.Outcomes {
		computed[f.Tenant] = &counts{}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		c := computed[a.Tenant]
		if c == nil {
			c = &counts{}
			computed[a.Tenant] = c
		}
		if attemptTerminal[a.State] {
			switch a.Outcome {
			case "useful":
				c.useful++
			case "wasted":
				c.wasted++
			case "discarded":
				c.discarded++
			case "undetermined":
				r.fail(Finding{
					Code:           "S-outcome-ledger",
					Message:        fmt.Sprintf("terminal attempt %s has no determined outcome", a.ID),
					Attempt:        a.ID,
					Tenant:         a.Tenant,
					Expected:       "determined outcome",
					Observed:       "undetermined",
					SnapshotRef:    "attempt:" + a.ID,
					Classification: ClassUnclassified,
				})
			}
			continue
		}
		if a.Outcome != "undetermined" {
			r.fail(Finding{
				Code:           "S-outcome-ledger",
				Message:        fmt.Sprintf("non-terminal attempt %s already has outcome %q", a.ID, a.Outcome),
				Attempt:        a.ID,
				Tenant:         a.Tenant,
				Expected:       "undetermined",
				Observed:       a.Outcome,
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	seenFacts := make(map[string]bool, len(s.Outcomes))
	for _, f := range s.Outcomes {
		seenFacts[f.Tenant] = true
		c := computed[f.Tenant]
		if c == nil {
			c = &counts{}
		}
		if f.Useful != c.useful || f.Wasted != c.wasted || f.Discarded != c.discarded {
			r.fail(Finding{
				Code:           "S-outcome-ledger",
				Message:        fmt.Sprintf("tenant %s outcome counters (%d/%d/%d) disagree with the attempt books (%d/%d/%d)", f.Tenant, f.Useful, f.Wasted, f.Discarded, c.useful, c.wasted, c.discarded),
				Tenant:         f.Tenant,
				Expected:       fmt.Sprintf("useful=%d wasted=%d discarded=%d", c.useful, c.wasted, c.discarded),
				Observed:       fmt.Sprintf("useful=%d wasted=%d discarded=%d", f.Useful, f.Wasted, f.Discarded),
				SnapshotRef:    "tenant:" + f.Tenant,
				Classification: ClassUnclassified,
			})
		}
	}
	// Tenants with attempts but no counter fact must not go unnoticed.
	for tenant, c := range computed {
		if seenFacts[tenant] {
			continue
		}
		if c.useful+c.wasted+c.discarded > 0 {
			r.fail(Finding{
				Code:           "S-outcome-ledger",
				Message:        fmt.Sprintf("tenant %s has terminal attempts but no outcome counters", tenant),
				Tenant:         tenant,
				Expected:       "counter facts",
				Observed:       "missing",
				SnapshotRef:    "tenant:" + tenant,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkBudgets(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	in := ctx.input
	r := newCheck("S-budget-state", "retry budget books are within their declared bounds")
	budgets := make(map[string]*BudgetFact, len(s.Budgets))
	for i := range s.Budgets {
		budgets[budgetRef(s.Budgets[i].Tenant, s.Budgets[i].Upstream)] = &s.Budgets[i]
	}
	for _, b := range s.Budgets {
		if b.Tokens < 0 || b.Tokens > b.Capacity {
			r.fail(Finding{
				Code:           "S-budget-state",
				Message:        fmt.Sprintf("budget (%s,%s) holds %d tokens of capacity %d", b.Tenant, b.Upstream, b.Tokens, b.Capacity),
				Tenant:         b.Tenant,
				Upstream:       b.Upstream,
				Expected:       fmt.Sprintf("0<=tokens<=%d", b.Capacity),
				Observed:       fmt.Sprintf("tokens=%d", b.Tokens),
				SnapshotRef:    budgetRef(b.Tenant, b.Upstream),
				Classification: ClassUnclassified,
			})
		}
		if b.LastRefillNS < 0 || b.LastRefillNS > s.TakenAtNS {
			r.fail(Finding{
				Code:           "S-budget-state",
				Message:        fmt.Sprintf("budget (%s,%s) last refilled at %dns outside [0,%d]", b.Tenant, b.Upstream, b.LastRefillNS, s.TakenAtNS),
				Tenant:         b.Tenant,
				Upstream:       b.Upstream,
				Expected:       "last refill inside the run",
				Observed:       fmt.Sprintf("%dns", b.LastRefillNS),
				SnapshotRef:    budgetRef(b.Tenant, b.Upstream),
				Classification: ClassUnclassified,
			})
		}
	}
	for _, c := range s.TokensConsumed {
		if c.Consumed < 0 {
			r.fail(Finding{
				Code:           "S-budget-state",
				Message:        fmt.Sprintf("budget (%s,%s) reports %d consumed tokens", c.Tenant, c.Upstream, c.Consumed),
				Tenant:         c.Tenant,
				Upstream:       c.Upstream,
				Expected:       "consumed>=0",
				Observed:       fmt.Sprintf("consumed=%d", c.Consumed),
				SnapshotRef:    budgetRef(c.Tenant, c.Upstream),
				Classification: ClassUnclassified,
			})
		}
		if _, ok := budgets[budgetRef(c.Tenant, c.Upstream)]; !ok {
			r.fail(Finding{
				Code:           "S-budget-state",
				Message:        fmt.Sprintf("consumed-token ledger names budget (%s,%s) that has no bucket", c.Tenant, c.Upstream),
				Tenant:         c.Tenant,
				Upstream:       c.Upstream,
				Expected:       "a budget bucket",
				Observed:       "none",
				SnapshotRef:    budgetRef(c.Tenant, c.Upstream),
				Classification: ClassUnclassified,
			})
		}
		if !hasTenant(in, c.Tenant) || !hasUpstream(in, c.Upstream) {
			r.fail(Finding{
				Code:           "S-budget-state",
				Message:        fmt.Sprintf("consumed-token ledger names unconfigured key (%s,%s)", c.Tenant, c.Upstream),
				Tenant:         c.Tenant,
				Upstream:       c.Upstream,
				Expected:       "a configured (tenant, upstream) pair",
				Observed:       "unknown pair",
				SnapshotRef:    budgetRef(c.Tenant, c.Upstream),
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkBreakers(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	in := ctx.input
	attByID := ctx.attemptByID
	r := newCheck("S-breaker-state", "breaker counters are within their declared bounds")
	_ = attByID
	for _, b := range s.Breakers {
		if !breakerStateSet[b.State] {
			continue // structural validation already rejected it
		}
		if b.ConsecutiveFailures < 0 {
			r.fail(Finding{
				Code:           "S-breaker-state",
				Message:        fmt.Sprintf("breaker %s reports %d consecutive failures", b.Upstream, b.ConsecutiveFailures),
				Upstream:       b.Upstream,
				Expected:       "consecutive failures >= 0",
				Observed:       fmt.Sprintf("%d", b.ConsecutiveFailures),
				SnapshotRef:    "breaker:" + b.Upstream,
				Classification: ClassUnclassified,
			})
		}
		if b.ProbesActive < 0 || b.ProbesActive > in.Breaker.MaxHalfOpenProbes {
			r.fail(Finding{
				Code:           "S-breaker-state",
				Message:        fmt.Sprintf("breaker %s reports %d active probes of allowance %d", b.Upstream, b.ProbesActive, in.Breaker.MaxHalfOpenProbes),
				Upstream:       b.Upstream,
				Expected:       fmt.Sprintf("0<=probes<=%d", in.Breaker.MaxHalfOpenProbes),
				Observed:       fmt.Sprintf("probes=%d", b.ProbesActive),
				SnapshotRef:    "breaker:" + b.Upstream,
				Classification: ClassUnclassified,
			})
		}
		if b.State != "half-open" && b.ProbesActive != 0 {
			r.fail(Finding{
				Code:           "S-breaker-state",
				Message:        fmt.Sprintf("breaker %s is %s with %d reserved probe permits", b.Upstream, b.State, b.ProbesActive),
				Upstream:       b.Upstream,
				Expected:       "0 probes outside half-open",
				Observed:       fmt.Sprintf("probes=%d", b.ProbesActive),
				SnapshotRef:    "breaker:" + b.Upstream,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

// checkProbeBooks verifies the probe permit ledger internally: only live
// attempts may hold a permit, and the number of permit-holding attempts on an
// upstream must equal the breaker's active-probe counter.
func checkProbeBooks(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	r := newCheck("S-probe-ledger", "breaker probe counters agree with the attempts holding permits")
	probesByUp := make(map[string]int, len(s.Breakers))
	for i := range s.Attempts {
		a := &s.Attempts[i]
		if !a.IsProbe {
			continue
		}
		probesByUp[a.Upstream]++
		if attemptTerminal[a.State] {
			r.fail(Finding{
				Code:           "S-probe-ledger",
				Message:        fmt.Sprintf("terminal attempt %s still holds a probe permit", a.ID),
				Attempt:        a.ID,
				Upstream:       a.Upstream,
				Expected:       "released permit",
				Observed:       "is_probe=true",
				SnapshotRef:    "attempt:" + a.ID,
				Classification: ClassUnclassified,
			})
		}
	}
	for _, b := range s.Breakers {
		if got := probesByUp[b.Upstream]; got != b.ProbesActive {
			r.fail(Finding{
				Code:           "S-probe-ledger",
				Message:        fmt.Sprintf("breaker %s reports %d active probes but %d attempts carry a probe permit", b.Upstream, b.ProbesActive, got),
				Upstream:       b.Upstream,
				Expected:       fmt.Sprintf("probes=%d", b.ProbesActive),
				Observed:       fmt.Sprintf("probe attempts=%d", got),
				SnapshotRef:    "breaker:" + b.Upstream,
				Classification: ClassUnclassified,
			})
		}
	}
	return *r
}

func checkSnapshotHorizon(ctx *evalContext) PropertyResult {
	s := ctx.snapshot
	in := ctx.input
	r := newCheck("S-horizon-completeness", "snapshot facts lie inside the observed run and terminal stamps are consistent")
	for i := range s.Requests {
		q := &s.Requests[i]
		ref := "request:" + q.ID
		if q.ArrivedAtNS > s.TakenAtNS {
			r.fail(Finding{
				Code:           "S-horizon-completeness",
				Message:        fmt.Sprintf("request %s arrived at %dns after the snapshot horizon %dns", q.ID, q.ArrivedAtNS, s.TakenAtNS),
				Request:        q.ID,
				Expected:       "arrival within the run",
				Observed:       fmt.Sprintf("%dns", q.ArrivedAtNS),
				SnapshotRef:    ref,
				Classification: ClassUnclassified,
			})
		}
		if requestTerminal[q.State] {
			if q.TerminalAtNS < q.ArrivedAtNS || q.TerminalAtNS > s.TakenAtNS {
				r.fail(Finding{
					Code:           "S-horizon-completeness",
					Message:        fmt.Sprintf("terminal request %s has terminal time %dns outside [%d,%d]", q.ID, q.TerminalAtNS, q.ArrivedAtNS, s.TakenAtNS),
					Request:        q.ID,
					Expected:       "terminal time inside the run",
					Observed:       fmt.Sprintf("%dns", q.TerminalAtNS),
					SnapshotRef:    ref,
					Classification: ClassUnclassified,
				})
			}
		} else if q.TerminalAtNS != 0 {
			r.fail(Finding{
				Code:           "S-horizon-completeness",
				Message:        fmt.Sprintf("non-terminal request %s carries terminal time %dns", q.ID, q.TerminalAtNS),
				Request:        q.ID,
				Expected:       "terminal_at_ns=0",
				Observed:       fmt.Sprintf("%dns", q.TerminalAtNS),
				SnapshotRef:    ref,
				Classification: ClassUnclassified,
			})
		}
	}
	for i := range s.Attempts {
		a := &s.Attempts[i]
		ref := "attempt:" + a.ID
		if a.CreatedAtNS > s.TakenAtNS || a.CreatedAtNS < 0 {
			r.fail(Finding{
				Code:           "S-horizon-completeness",
				Message:        fmt.Sprintf("attempt %s was created at %dns outside [0,%d]", a.ID, a.CreatedAtNS, s.TakenAtNS),
				Attempt:        a.ID,
				Expected:       "creation inside the run",
				Observed:       fmt.Sprintf("%dns", a.CreatedAtNS),
				SnapshotRef:    ref,
				Classification: ClassUnclassified,
			})
		}
		if a.TimeoutNS <= 0 {
			r.fail(Finding{
				Code:           "S-horizon-completeness",
				Message:        fmt.Sprintf("attempt %s has non-positive timeout %dns", a.ID, a.TimeoutNS),
				Attempt:        a.ID,
				Expected:       "timeout>0",
				Observed:       fmt.Sprintf("%dns", a.TimeoutNS),
				SnapshotRef:    ref,
				Classification: ClassUnclassified,
			})
		}
		if attemptTerminal[a.State] {
			if a.FinishedAtNS < a.CreatedAtNS || a.FinishedAtNS > s.TakenAtNS {
				r.fail(Finding{
					Code:           "S-horizon-completeness",
					Message:        fmt.Sprintf("terminal attempt %s has finish time %dns outside [%d,%d]", a.ID, a.FinishedAtNS, a.CreatedAtNS, s.TakenAtNS),
					Attempt:        a.ID,
					Expected:       "finish time inside the run",
					Observed:       fmt.Sprintf("%dns", a.FinishedAtNS),
					SnapshotRef:    ref,
					Classification: ClassUnclassified,
				})
			}
		} else if a.FinishedAtNS != 0 {
			r.fail(Finding{
				Code:           "S-horizon-completeness",
				Message:        fmt.Sprintf("non-terminal attempt %s carries finish time %dns", a.ID, a.FinishedAtNS),
				Attempt:        a.ID,
				Expected:       "finished_at_ns=0",
				Observed:       fmt.Sprintf("%dns", a.FinishedAtNS),
				SnapshotRef:    ref,
				Classification: ClassUnclassified,
			})
		}
	}
	for _, q := range s.Queues {
		for _, e := range q.Entries {
			if e.EnqueuedAtNS < 0 || e.EnqueuedAtNS > s.TakenAtNS {
				r.fail(Finding{
					Code:           "S-horizon-completeness",
					Message:        fmt.Sprintf("queue entry %s on %s has enqueue time %dns outside [0,%d]", e.Attempt, q.Tenant, e.EnqueuedAtNS, s.TakenAtNS),
					Tenant:         q.Tenant,
					Attempt:        e.Attempt,
					Expected:       "enqueue time inside the run",
					Observed:       fmt.Sprintf("%dns", e.EnqueuedAtNS),
					SnapshotRef:    "queue:" + q.Tenant,
					Classification: ClassUnclassified,
				})
			}
		}
	}
	_ = in
	return *r
}

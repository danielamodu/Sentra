package sim

import (
	"fmt"
	"math"
	"sort"

	"sentra/domain"
)

// queueWaitBuckets is the number of log2 buckets retained per tenant for the
// queue-wait histogram. Bucket i holds waits in [2^(i-1) ns, 2^i ns) (i==0:
// wait 0). 32 buckets cover waits up to ~4s of simulated time; longer waits
// clamp into the highest bucket (documented approximation).
const queueWaitBuckets = 32

// TenantMetrics aggregates per-tenant fairness evidence:
//
//   - QueueWait*: distribution of queue-wait time (enqueue → removal for
//     dispatch) of attempts that actually got dispatched;
//   - Dispatched: attempts dispatched onto a worker (dispatch share);
//   - Sheds: load-shed count per reason (shed-reason mix);
//   - UsefulShare: computed from OutcomeCounters useful/(useful+wasted+...).
//
// All fields update from handlers; the zero value is a valid, empty aggregate.
type TenantMetrics struct {
	QueueWaitSum  domain.SimDuration      `json:"queue_wait_sum_ns"`
	QueueWaitMax  domain.SimDuration      `json:"queue_wait_max_ns"`
	QueueWaitN    int                     `json:"queue_wait_n"`
	QueueWaitHist [queueWaitBuckets]int64 `json:"queue_wait_hist"`
	Dispatched    int                     `json:"dispatched"`
	Sheds         map[string]int          `json:"sheds,omitempty"`

	// --- M2 system-metrics fields (workstreams 3/4/6) -------------------------

	// AttemptsStarted counts created arrival-lane attempts; RetryAttempts
	// counts created retry-lane attempts. Their sum is the physical attempt
	// volume; the logical-request volume is tracked per request, never here,
	// so the two are never conflated.
	AttemptsStarted int `json:"attempts_started"`
	RetryAttempts   int `json:"retry_attempts"`

	// BlockedRetries counts retry-admission rejections (a retry was needed
	// but could not even enter the queue).
	BlockedRetries int `json:"blocked_retries"`

	// WorkerTime sums per-attempt worker-occupancy durations: Σ (release −
	// assignment) over attempts that held a worker, in simulated time.
	WorkerTime domain.SimDuration `json:"worker_time_ns"`

	// ConnWait* describes pinned-waiting-for-a-connection intervals:
	// ConnWaitSum/N over waits that ENDED in an acquisition (granted), and
	// ConnWaitAbandoned over waits that ended in timeout or retraction.
	ConnWaitSum       domain.SimDuration `json:"conn_wait_sum_ns"`
	ConnWaitMax       domain.SimDuration `json:"conn_wait_max_ns"`
	ConnWaitN         int                `json:"conn_wait_n"`
	ConnWaitAbandoned int                `json:"conn_wait_abandoned"`

	// QueueDwellSum/N sums the queue residence time of attempts retracted
	// while queued (dispatched attempts are covered by QueueWait*). Together
	// the two distributions cover the full queue-residence story.
	QueueDwellSum domain.SimDuration `json:"queue_dwell_abandoned_sum_ns"`
	QueueDwellN   int                `json:"queue_dwell_abandoned_n"`
}

// recordQueueWait folds one dispatched attempt's wait time into the aggregate.
func (m *TenantMetrics) recordQueueWait(wait domain.SimDuration) {
	m.QueueWaitSum += wait
	if wait > m.QueueWaitMax {
		m.QueueWaitMax = wait
	}
	m.QueueWaitN++
	m.QueueWaitHist[histBucket(wait)]++
}

// recordShed folds one admission shed into the per-reason mix.
func (m *TenantMetrics) recordShed(reason string) {
	if m.Sheds == nil {
		m.Sheds = make(map[string]int)
	}
	m.Sheds[reason]++
}

// --- M2 recording helpers (value receivers through state.go wrappers) --------

// recordConnWait folds one granted pinned-wait interval into the aggregate.
func (m *TenantMetrics) recordConnWait(wait domain.SimDuration) {
	m.ConnWaitSum += wait
	if wait > m.ConnWaitMax {
		m.ConnWaitMax = wait
	}
	m.ConnWaitN++
}

// recordConnWaitAbandoned folds one pinned-wait interval that ended in a
// timeout or retraction (the attempt never acquired its connection).
func (m *TenantMetrics) recordConnWaitAbandoned(wait domain.SimDuration) {
	m.ConnWaitSum += wait
	if wait > m.ConnWaitMax {
		m.ConnWaitMax = wait
	}
	m.ConnWaitAbandoned++
}

// recordQueueDwellAbandoned folds the queue residence time of an attempt
// retracted while queued (never dispatched).
func (m *TenantMetrics) recordQueueDwellAbandoned(dwell domain.SimDuration) {
	m.QueueDwellSum += dwell
	m.QueueDwellN++
}

// recordAttemptCreate folds attempt creation into the lane counts.
func (m *TenantMetrics) recordAttemptCreate(isRetry bool) {
	if isRetry {
		m.RetryAttempts++
	} else {
		m.AttemptsStarted++
	}
}

// recordWorkerTime folds one worker-occupancy interval into the aggregate.
func (m *TenantMetrics) recordWorkerTime(d domain.SimDuration) {
	m.WorkerTime += d
}

// histBucket maps a wait duration to its log2 bucket, clamped at the top.
func histBucket(wait domain.SimDuration) int {
	w := int64(wait)
	if w <= 0 {
		return 0
	}
	b := 0
	for w > 1 && b < queueWaitBuckets-1 {
		w = (w + 1) / 2 // ceil-halving: deterministic log2 without math/bits
		b++
	}
	if b > queueWaitBuckets-1 {
		b = queueWaitBuckets - 1
	}
	return b
}

// FairnessSnapshot is a copy of per-tenant metrics for one instant, ordered by
// tenant ID so downstream rendering and comparisons are deterministic.
type FairnessSnapshot struct {
	Tenants []TenantFair
}

// TenantFair is one tenant's slice of the fairness snapshot.
type TenantFair struct {
	Tenant domain.TenantID `json:"tenant"`
	// Useful is the tenant's useful-outcome count (from OutcomeCounters), so
	// the snapshot is self-contained for fairness math.
	Useful  int           `json:"useful"`
	Metrics TenantMetrics `json:"metrics"`
}

// FairnessSnapshot renders deterministically ordered per-tenant metrics.
// Tenants with no traffic also appear (zeroed), so the snapshot always covers
// every configured tenant.
func (st *SystemState) FairnessSnapshot() FairnessSnapshot {
	ids := make([]domain.TenantID, 0, len(st.Queues))
	for t := range st.Queues {
		ids = append(ids, t)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := FairnessSnapshot{Tenants: make([]TenantFair, 0, len(ids))}
	for _, t := range ids {
		out.Tenants = append(out.Tenants, TenantFair{Tenant: t, Useful: st.OutcomeCounters[t].Useful, Metrics: st.Metrics[t]})
	}
	return out
}

// JainIndex computes the Jain fairness index over per-tenant Useful outcome
// counts (documented in the milestone as the "useful-work share" dimension).
//
// Returns NaN when the index is mathematically undefined — zero tenants or an
// all-zero vector (0/0). Callers must handle NaN explicitly; the sim never
// pretends an undefined quantity is 0.
func JainIndex(useful []int) float64 {
	n := len(useful)
	if n == 0 {
		return math.NaN()
	}
	var sum, sumSq float64
	for _, x := range useful {
		f := float64(x)
		sum += f
		sumSq += f * f
	}
	if sumSq == 0 {
		return math.NaN()
	}
	return (sum * sum) / (float64(n) * sumSq)
}

// JainSnapshot computes the Jain index over the useful-outcome counts
// embedded in the snapshot. See JainIndex for the NaN contract.
func (fs FairnessSnapshot) JainSnapshot() float64 {
	counts := make([]int, len(fs.Tenants))
	for i, tf := range fs.Tenants {
		counts[i] = tf.Useful
	}
	return JainIndex(counts)
}

// UsefulOutcomeCounts returns the per-tenant useful-outcome vector in the same
// deterministic (sorted by tenant ID) order as FairnessSnapshot.
func (st *SystemState) UsefulOutcomeCounts() []int {
	counts := make([]int, 0, len(st.Cfg.Tenants))
	for _, tf := range st.FairnessSnapshot().Tenants {
		counts = append(counts, tf.Useful)
	}
	return counts
}

// String renders the snapshot compactly for logs and debugging.
func (fs FairnessSnapshot) String() string {
	return fmt.Sprintf("fairness: tenants=%d jain(useful)=%.3f", len(fs.Tenants), fs.JainSnapshot())
}

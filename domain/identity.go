package domain

// RequestID identifies a logical request: the tenant-visible unit of work with
// one deadline, one retry policy and one terminal outcome.
type RequestID string

// AttemptID identifies a physical attempt: one actual execution of work
// against an upstream dependency. A logical request may own several attempts,
// and attempts may outlive their logical request.
type AttemptID string

// TenantID identifies a multi-tenant gateway client.
type TenantID string

// UpstreamID identifies an upstream dependency (U1 critical, U2 standard).
type UpstreamID string

// WorkerID identifies one execution slot of a worker pool.
type WorkerID string

// WorkerPoolID identifies a worker pool.
type WorkerPoolID string

// BudgetKey scopes a retry budget to a (tenant, upstream) pair: retries of
// tenant A's traffic against U1 do not consume tenant B's retry budget, and
// retries against U2 do not consume the budget reserved for U1.
type BudgetKey struct {
	Tenant   TenantID
	Upstream UpstreamID
}

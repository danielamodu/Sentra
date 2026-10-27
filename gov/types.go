package gov

import (
	"errors"
	"time"
)

// ProposalState represents the explicit lifecycle phase of an operational change proposal.
type ProposalState string

const (
	StateDraft           ProposalState = "DRAFT"
	StateExperimenting   ProposalState = "EXPERIMENTING"
	StateVerified        ProposalState = "VERIFIED"
	StateRejected        ProposalState = "REJECTED"
	StatePendingApproval ProposalState = "PENDING_APPROVAL"
	StateApproved        ProposalState = "APPROVED"
	StatePromoted        ProposalState = "PROMOTED"
	StateExpired         ProposalState = "EXPIRED"
)

var (
	ErrSelfApprovalForbidden      = errors.New("governance: agent self-approval is strictly forbidden")
	ErrIllegalTransition          = errors.New("governance: illegal proposal lifecycle transition")
	ErrPolicyViolation            = errors.New("governance: proposal violates governance policy")
	ErrVerificationFailed         = errors.New("governance: verification failed or incomplete")
	ErrApprovalInvalidatedByDrift = errors.New("governance: approval invalidated due to configuration drift")
	ErrProposalExpired            = errors.New("governance: proposal has expired or been superseded")
	ErrUnauthorizedApprover       = errors.New("governance: approver lacks required authorization role")
	ErrTamperedAuditLog           = errors.New("governance: audit log integrity failure; hash chain broken")
)

// ApproverInfo identifies an entity requesting or granting authorization.
type ApproverInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	IsAgent bool   `json:"is_agent"`
}

// ApprovalRecord captures an immutable, cryptographically bound authorization record.
type ApprovalRecord struct {
	ApprovalID         string    `json:"approval_id"`
	ProposalID         string    `json:"proposal_id"`
	ApproverID         string    `json:"approver_id"`
	ApproverRole       string    `json:"approver_role"`
	ConfigDigest       string    `json:"config_digest"`
	VerificationDigest string    `json:"verification_digest"`
	PolicyVersion      string    `json:"policy_version"`
	Timestamp          time.Time `json:"timestamp"`
	Decision           string    `json:"decision"` // "APPROVED" or "REJECTED"
	Reason             string    `json:"reason"`
	Signature          string    `json:"signature"`
}

// AdjudicationResult records the outcome of independent verifier and policy adjudication.
type AdjudicationResult struct {
	Eligible               bool      `json:"eligible"`
	RejectionReasons       []string  `json:"rejection_reasons,omitempty"`
	PassedSafetyProperties []string  `json:"passed_safety_properties"`
	FailedSafetyProperties []string  `json:"failed_safety_properties,omitempty"`
	MissingEvidenceGaps    []string  `json:"missing_evidence_gaps,omitempty"`
	VerificationStatus     string    `json:"verification_status"`
	AdjudicatedAt          time.Time `json:"adjudicated_at"`
}

package gov

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// AuditAction defines the standardized taxonomy of auditable governance events.
type AuditAction string

const (
	ActionInvestigated      AuditAction = "INVESTIGATED"
	ActionProposed          AuditAction = "PROPOSED"
	ActionExperimentRan     AuditAction = "EXPERIMENT_RAN"
	ActionCandidateRejected AuditAction = "CANDIDATE_REJECTED"
	ActionVerified          AuditAction = "VERIFIED"
	ActionPolicyChecked     AuditAction = "POLICY_CHECKED"
	ActionApprovalRequested AuditAction = "APPROVAL_REQUESTED"
	ActionApproved          AuditAction = "APPROVED"
	ActionRejected          AuditAction = "REJECTED"
	ActionPromotionAttempt  AuditAction = "PROMOTION_ATTEMPT"
	ActionPromoted          AuditAction = "PROMOTED"
)

// AuditEvent represents one immutable, hash-chained entry in the audit trail.
type AuditEvent struct {
	Index         int64       `json:"index"`
	Timestamp     time.Time   `json:"timestamp"`
	ProposalID    string      `json:"proposal_id"`
	Action        AuditAction `json:"action"`
	Actor         string      `json:"actor"`
	PayloadDigest string      `json:"payload_digest"`
	Details       string      `json:"details"`
	PrevHash      string      `json:"prev_hash"`
	EntryHash     string      `json:"entry_hash"`
}

// AuditLog is a thread-safe, append-only, tamper-evident hash-chained audit ledger.
type AuditLog struct {
	mu     sync.RWMutex
	events []AuditEvent
}

// NewAuditLog initializes an empty AuditLog.
func NewAuditLog() *AuditLog {
	return &AuditLog{
		events: make([]AuditEvent, 0),
	}
}

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Append appends a new event to the hash chain.
func (al *AuditLog) Append(proposalID string, action AuditAction, actor, payloadDigest, details string) (*AuditEvent, error) {
	al.mu.Lock()
	defer al.mu.Unlock()

	idx := int64(len(al.events))
	prevHash := genesisHash
	if idx > 0 {
		prevHash = al.events[idx-1].EntryHash
	}

	now := time.Now().UTC()
	evt := AuditEvent{
		Index:         idx,
		Timestamp:     now,
		ProposalID:    proposalID,
		Action:        action,
		Actor:         actor,
		PayloadDigest: payloadDigest,
		Details:       details,
		PrevHash:      prevHash,
	}

	evt.EntryHash = computeEntryHash(evt)
	al.events = append(al.events, evt)
	return &evt, nil
}

// VerifyIntegrity verifies the cryptographic continuity of the entire hash chain.
func (al *AuditLog) VerifyIntegrity() error {
	al.mu.RLock()
	defer al.mu.RUnlock()

	for i, evt := range al.events {
		// Verify index continuity
		if evt.Index != int64(i) {
			return fmt.Errorf("%w: event at position %d has index %d", ErrTamperedAuditLog, i, evt.Index)
		}

		// Verify previous hash pointer
		expectedPrev := genesisHash
		if i > 0 {
			expectedPrev = al.events[i-1].EntryHash
		}
		if evt.PrevHash != expectedPrev {
			return fmt.Errorf("%w: event %d prev_hash mismatch: expected %s, got %s",
				ErrTamperedAuditLog, i, expectedPrev, evt.PrevHash)
		}

		// Verify entry hash recalculation
		expectedHash := computeEntryHash(evt)
		if evt.EntryHash != expectedHash {
			return fmt.Errorf("%w: event %d entry_hash mismatch: expected %s, got %s (content tampered)",
				ErrTamperedAuditLog, i, expectedHash, evt.EntryHash)
		}
	}

	return nil
}

// GetProposalTrail returns all audit events recorded for a given proposal ID.
func (al *AuditLog) GetProposalTrail(proposalID string) []AuditEvent {
	al.mu.RLock()
	defer al.mu.RUnlock()

	var out []AuditEvent
	for _, e := range al.events {
		if e.ProposalID == proposalID {
			out = append(out, e)
		}
	}
	return out
}

// Events returns a copy of all audit events.
func (al *AuditLog) Events() []AuditEvent {
	al.mu.RLock()
	defer al.mu.RUnlock()

	out := make([]AuditEvent, len(al.events))
	copy(out, al.events)
	return out
}

func computeEntryHash(e AuditEvent) string {
	payload := fmt.Sprintf("%d:%s:%s:%s:%s:%s:%s:%s",
		e.Index,
		e.Timestamp.Format(time.RFC3339Nano),
		e.ProposalID,
		e.Action,
		e.Actor,
		e.PayloadDigest,
		e.Details,
		e.PrevHash,
	)
	h := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(h[:])
}

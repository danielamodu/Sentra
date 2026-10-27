package gov

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"sentra/ops"
)

// Authorize records an explicit human authorization decision.
// Enforces that agents and proposal authors cannot self-approve proposals.
func Authorize(
	prop *ops.Proposal,
	approver ApproverInfo,
	configDigest string,
	verificationDigest string,
	pol Policy,
	decision string,
	reason string,
	secretKey []byte,
) (*ApprovalRecord, error) {
	if prop == nil {
		return nil, fmt.Errorf("authorization failed: proposal is nil")
	}

	// 1. Strict self-approval prevention
	if approver.IsAgent {
		return nil, fmt.Errorf("%w: approver %q is flagged as an autonomous agent",
			ErrSelfApprovalForbidden, approver.ID)
	}
	if strings.EqualFold(approver.ID, prop.AuthorID) {
		return nil, fmt.Errorf("%w: proposal author %q cannot approve their own proposal",
			ErrSelfApprovalForbidden, approver.ID)
	}

	// 2. Role authorization check
	if approver.Role != pol.RequiredApproverRole && pol.RequiredApproverRole != "" {
		return nil, fmt.Errorf("%w: approver role %q does not satisfy required policy role %q",
			ErrUnauthorizedApprover, approver.Role, pol.RequiredApproverRole)
	}

	// 3. Decision validation
	dec := strings.ToUpper(strings.TrimSpace(decision))
	if dec != "APPROVED" && dec != "REJECTED" {
		return nil, fmt.Errorf("invalid approval decision %q: must be APPROVED or REJECTED", decision)
	}

	if configDigest == "" || verificationDigest == "" {
		return nil, fmt.Errorf("authorization requires non-empty config and verification digests")
	}
	if len(secretKey) == 0 {
		return nil, fmt.Errorf("authorization HMAC secret key must not be empty")
	}

	now := time.Now().UTC()
	rec := &ApprovalRecord{
		ApprovalID:         fmt.Sprintf("appr-%s-%d", prop.ID, now.UnixNano()),
		ProposalID:         prop.ID,
		ApproverID:         approver.ID,
		ApproverRole:       approver.Role,
		ConfigDigest:       configDigest,
		VerificationDigest: verificationDigest,
		PolicyVersion:      pol.Version,
		Timestamp:          now,
		Decision:           dec,
		Reason:             reason,
	}

	// 4. Compute cryptographic HMAC signature binding all fields
	payload := formatSignaturePayload(rec)
	rec.Signature = computeHMAC(payload, secretKey)

	return rec, nil
}

// VerifyApproval verifies that an ApprovalRecord has a valid cryptographic signature
// and has not been invalidated by configuration drift or verification changes.
func VerifyApproval(rec *ApprovalRecord, currentConfigDigest, currentVerifDigest string, secretKey []byte) error {
	if rec == nil {
		return fmt.Errorf("approval record is nil")
	}
	if len(secretKey) == 0 {
		return fmt.Errorf("verification HMAC secret key must not be empty")
	}

	// 1. Verify signature integrity
	expectedSig := computeHMAC(formatSignaturePayload(rec), secretKey)
	if !hmac.Equal([]byte(rec.Signature), []byte(expectedSig)) {
		return fmt.Errorf("approval signature is invalid; record may have been tampered with")
	}

	// 2. Verify decision
	if rec.Decision != "APPROVED" {
		return fmt.Errorf("approval record is not APPROVED (decision: %s, reason: %s)", rec.Decision, rec.Reason)
	}

	// 3. Configuration drift check: approval is cryptographically bound to the exact configuration
	if rec.ConfigDigest != currentConfigDigest {
		return fmt.Errorf("%w: approved config digest %s != current config digest %s",
			ErrApprovalInvalidatedByDrift, rec.ConfigDigest, currentConfigDigest)
	}

	// 4. Verification drift check: approval is bound to the verified evidence digest
	if rec.VerificationDigest != currentVerifDigest {
		return fmt.Errorf("%w: approved verification digest %s != current verification digest %s",
			ErrApprovalInvalidatedByDrift, rec.VerificationDigest, currentVerifDigest)
	}

	return nil
}

func formatSignaturePayload(rec *ApprovalRecord) string {
	return fmt.Sprintf("%s:%s:%s:%s:%s:%s:%s:%d:%s",
		rec.ApprovalID,
		rec.ProposalID,
		rec.ApproverID,
		rec.ApproverRole,
		rec.ConfigDigest,
		rec.VerificationDigest,
		rec.PolicyVersion,
		rec.Timestamp.UnixNano(),
		rec.Decision,
	)
}

func computeHMAC(message string, key []byte) string {
	if len(key) == 0 {
		key = []byte("sentra-default-governance-secret-key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

package gov

import (
	"strings"
	"testing"
	"time"

	"sentra/ops"
	"sentra/verify"
)

func mockValidProposal() ops.Proposal {
	return ops.Proposal{
		ID:                  "prop-gov-001",
		AuthorID:            "agent-sentra",
		CreatedAt:           time.Now().UTC(),
		TargetConfigVersion: "v1.0",
		Changes: []ops.ParameterChange{
			{
				Parameter:     "breaker.failure_threshold",
				OriginalValue: 3,
				ProposedValue: 5,
				Reason:        "Tuning breaker sensitivity",
			},
		},
		RelevantInvariants: []string{"S-worker-conservation"},
		ExperimentConfig: ops.ExperimentConfigSnapshot{
			ScenarioName: "ScenarioC",
			HorizonNS:    100_000_000,
		},
		EvidenceRefs: []string{"evidence://test"},
	}
}

func TestLifecycleLegalAndIllegalTransitions(t *testing.T) {
	lm := NewLifecycleManager()
	propID := "prop-101"

	// Initialize
	lc, err := lm.InitProposal(propID)
	if err != nil || lc.Current != StateDraft {
		t.Fatalf("InitProposal failed: %v", err)
	}

	// 1. Legal transition: DRAFT -> EXPERIMENTING
	if err := lm.Transition(propID, StateExperimenting, "starting experiments"); err != nil {
		t.Errorf("expected legal transition DRAFT -> EXPERIMENTING to succeed, got: %v", err)
	}

	// 2. Illegal transition: EXPERIMENTING -> APPROVED (cannot jump directly to APPROVED)
	if err := lm.Transition(propID, StateApproved, "illegal jump"); err == nil {
		t.Errorf("expected illegal transition EXPERIMENTING -> APPROVED to fail, got nil")
	} else if !strings.Contains(err.Error(), "illegal proposal lifecycle transition") {
		t.Errorf("expected ErrIllegalTransition, got: %v", err)
	}

	// 3. Legal path: EXPERIMENTING -> VERIFIED -> PENDING_APPROVAL -> APPROVED -> PROMOTED
	if err := lm.Transition(propID, StateVerified, "simulation and verifier passed"); err != nil {
		t.Errorf("EXPERIMENTING -> VERIFIED failed: %v", err)
	}
	if err := lm.Transition(propID, StatePendingApproval, "submitting for human review"); err != nil {
		t.Errorf("VERIFIED -> PENDING_APPROVAL failed: %v", err)
	}
	if err := lm.Transition(propID, StateApproved, "human approver authorized"); err != nil {
		t.Errorf("PENDING_APPROVAL -> APPROVED failed: %v", err)
	}
	if err := lm.Transition(propID, StatePromoted, "applied to live system"); err != nil {
		t.Errorf("APPROVED -> PROMOTED failed: %v", err)
	}

	// Terminal state: PROMOTED cannot transition further
	if err := lm.Transition(propID, StateDraft, "cannot revive"); err == nil {
		t.Errorf("expected transition from PROMOTED to fail, got nil")
	}
}

func TestPolicyValidation(t *testing.T) {
	pol := DefaultPolicy()

	// 1. Valid proposal
	validProp := mockValidProposal()
	if err := pol.ValidateProposalPolicy(&validProp); err != nil {
		t.Errorf("expected valid proposal to comply with policy, got: %v", err)
	}

	// 2. Out of range parameter
	outOfRangeProp := mockValidProposal()
	outOfRangeProp.Changes[0].ProposedValue = 999 // max is 25
	if err := pol.ValidateProposalPolicy(&outOfRangeProp); err == nil {
		t.Errorf("expected policy violation for out of range parameter, got nil")
	}

	// 3. Excessive delta
	excessiveDeltaProp := mockValidProposal()
	excessiveDeltaProp.Changes[0].OriginalValue = 2
	excessiveDeltaProp.Changes[0].ProposedValue = 18 // 800% delta, max allowed is 200%
	if err := pol.ValidateProposalPolicy(&excessiveDeltaProp); err == nil {
		t.Errorf("expected policy violation for excessive delta, got nil")
	}

	// 4. Protected path violation
	protectedProp := mockValidProposal()
	protectedProp.Changes[0].Parameter = "verifier.checks.bypass"
	if err := pol.ValidateProposalPolicy(&protectedProp); err == nil {
		t.Errorf("expected policy violation for protected path, got nil")
	}
}

func TestSelfApprovalPreventionAndAuthorization(t *testing.T) {
	pol := DefaultPolicy()
	prop := mockValidProposal()
	secretKey := []byte("test-auth-secret-key-32-bytes!!")

	// 1. Agent self-approval attempt MUST fail
	agentApprover := ApproverInfo{
		ID:      "agent-sentra",
		Name:    "Sentra Autonomous Agent",
		Role:    "infrastructure-lead",
		IsAgent: true,
	}
	_, err := Authorize(&prop, agentApprover, "digest-cfg-1", "digest-verif-1", pol, "APPROVED", "self-ok", secretKey)
	if err == nil {
		t.Errorf("expected agent self-approval to be rejected, got nil")
	} else if !strings.Contains(err.Error(), "agent self-approval is strictly forbidden") {
		t.Errorf("expected ErrSelfApprovalForbidden, got: %v", err)
	}

	// 2. Author self-approval attempt (human or agent with same ID) MUST fail
	authorApprover := ApproverInfo{
		ID:      "agent-sentra",
		Name:    "Author",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}
	_, err = Authorize(&prop, authorApprover, "digest-cfg-1", "digest-verif-1", pol, "APPROVED", "self-ok", secretKey)
	if err == nil {
		t.Errorf("expected author self-approval to be rejected, got nil")
	}

	// 3. Unauthorized role MUST fail
	juniorApprover := ApproverInfo{
		ID:      "alice-human",
		Name:    "Alice Junior",
		Role:    "intern",
		IsAgent: false,
	}
	_, err = Authorize(&prop, juniorApprover, "digest-cfg-1", "digest-verif-1", pol, "APPROVED", "looks good", secretKey)
	if err == nil {
		t.Errorf("expected unauthorized role to fail, got nil")
	}

	// 4. Valid human authorization
	validApprover := ApproverInfo{
		ID:      "bob-lead",
		Name:    "Bob SRE Lead",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}
	apprRec, err := Authorize(&prop, validApprover, "digest-cfg-1", "digest-verif-1", pol, "APPROVED", "verified safe", secretKey)
	if err != nil {
		t.Fatalf("expected valid human authorization to succeed, got: %v", err)
	}

	// 5. Verification of valid approval
	if err := VerifyApproval(apprRec, "digest-cfg-1", "digest-verif-1", secretKey); err != nil {
		t.Errorf("expected approval verification to pass, got: %v", err)
	}

	// 6. Drift invalidation: configuration modified after approval
	if err := VerifyApproval(apprRec, "digest-cfg-MODIFIED", "digest-verif-1", secretKey); err == nil {
		t.Errorf("expected config drift to invalidate approval, got nil")
	} else if !strings.Contains(err.Error(), "approval invalidated due to configuration drift") {
		t.Errorf("expected ErrApprovalInvalidatedByDrift, got: %v", err)
	}

	// 7. Drift invalidation: verification modified after approval
	if err := VerifyApproval(apprRec, "digest-cfg-1", "digest-verif-MODIFIED", secretKey); err == nil {
		t.Errorf("expected verification drift to invalidate approval, got nil")
	}
}

func TestIndependentAdjudication(t *testing.T) {
	pol := DefaultPolicy()
	prop := mockValidProposal()

	// 1. PASS adjudication
	validDoc := verify.Document{
		Evidence: verify.Evidence{
			FirstEffectSeq: 0,
			Terminal: &verify.TerminalAttestation{
				RecordCount: 10,
			},
		},
	}
	validVerdict := verify.Verdict{
		Status: verify.StatusPass,
		Properties: []verify.PropertyResult{
			{ID: "S-worker-conservation", Status: verify.StatusPass},
			{ID: "S-connection-stranding", Status: verify.StatusPass},
			{ID: "B-worker-interval-pairing", Status: verify.StatusPass},
			{ID: "B-connection-interval-pairing", Status: verify.StatusPass},
			{ID: "C-admission-outcome", Status: verify.StatusPass},
			{ID: "C-fifo-dispatch", Status: verify.StatusPass},
			{ID: "D-retry-limit", Status: verify.StatusPass},
			{ID: "E-breaker-open-threshold", Status: verify.StatusPass},
			{ID: "L-request-lifecycle", Status: verify.StatusPass},
		},
	}

	res, err := Adjudicate(&prop, &validVerdict, &validDoc, pol)
	if err != nil || !res.Eligible {
		t.Fatalf("expected valid adjudication to be eligible, got: %v (reasons: %v)", err, res.RejectionReasons)
	}

	// 2. FAIL verdict MUST be ineligible
	failVerdict := validVerdict
	failVerdict.Status = verify.StatusFail
	failVerdict.Findings = []verify.Finding{{Code: "S-worker-conservation", Message: "violation"}}
	res, _ = Adjudicate(&prop, &failVerdict, &validDoc, pol)
	if res.Eligible {
		t.Errorf("expected FAIL verdict to be ineligible, but was marked eligible")
	}

	// 3. Head truncation MUST be ineligible
	truncatedDoc := validDoc
	truncatedDoc.Evidence.FirstEffectSeq = 42 // head evicted
	res, _ = Adjudicate(&prop, &validVerdict, &truncatedDoc, pol)
	if res.Eligible {
		t.Errorf("expected head-truncated evidence to be ineligible")
	}

	// 4. Missing terminal attestation MUST be ineligible
	noTerminalDoc := validDoc
	noTerminalDoc.Evidence.Terminal = nil
	res, _ = Adjudicate(&prop, &validVerdict, &noTerminalDoc, pol)
	if res.Eligible {
		t.Errorf("expected missing terminal attestation to be ineligible")
	}
}

func TestAuditLogTamperEvidence(t *testing.T) {
	log := NewAuditLog()

	// Append events
	e1, err := log.Append("prop-1", ActionInvestigated, "agent", "d1", "investigation done")
	if err != nil {
		t.Fatalf("Append e1: %v", err)
	}
	_, err = log.Append("prop-1", ActionProposed, "agent", "d2", "proposed change")
	if err != nil {
		t.Fatalf("Append e2: %v", err)
	}
	_, err = log.Append("prop-1", ActionApproved, "bob-lead", "d3", "authorized")
	if err != nil {
		t.Fatalf("Append e3: %v", err)
	}

	// Verify intact log
	if err := log.VerifyIntegrity(); err != nil {
		t.Errorf("expected intact audit log to pass verification, got: %v", err)
	}

	if trail := log.GetProposalTrail("prop-1"); len(trail) != 3 {
		t.Errorf("expected 3 trail events, got %d", len(trail))
	}

	// Tamper with e1 in the log
	log.events[0].Details = "tampered details"
	if err := log.VerifyIntegrity(); err == nil {
		t.Errorf("expected tampered audit log to fail verification, got nil")
	} else if !strings.Contains(err.Error(), "audit log integrity failure") {
		t.Errorf("expected ErrTamperedAuditLog, got: %v", err)
	}
	_ = e1
}

func TestAdversarialHMACAndSignatureValidation(t *testing.T) {
	pol := DefaultPolicy()
	prop := mockValidProposal()
	secretKey := []byte("production-grade-secret-key-32b!")
	wrongKey := []byte("adversary-forged-secret-key-32b!")

	approver := ApproverInfo{
		ID:      "sarah-lead",
		Name:    "Sarah SRE Lead",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}

	// 1. Authorize with empty secret key MUST fail closed
	if _, err := Authorize(&prop, approver, "cfg-digest-1", "verif-digest-1", pol, "APPROVED", "ok", nil); err == nil {
		t.Errorf("SECURITY DEFECT: Authorize allowed nil/empty HMAC key")
	}

	// 2. Authorize legally
	apprRec, err := Authorize(&prop, approver, "cfg-digest-1", "verif-digest-1", pol, "APPROVED", "valid authorization", secretKey)
	if err != nil {
		t.Fatalf("legitimate Authorize failed: %v", err)
	}

	// 3. VerifyApproval with empty secret key MUST fail closed
	if err := VerifyApproval(apprRec, "cfg-digest-1", "verif-digest-1", nil); err == nil {
		t.Errorf("SECURITY DEFECT: VerifyApproval allowed nil/empty HMAC key")
	}

	// 4. VerifyApproval with WRONG secret key MUST fail
	if err := VerifyApproval(apprRec, "cfg-digest-1", "verif-digest-1", wrongKey); err == nil {
		t.Errorf("SECURITY DEFECT: VerifyApproval passed with wrong HMAC key")
	}

	// 5. VerifyApproval with single-bit signature corruption MUST fail
	corruptSigRec := *apprRec
	corruptBytes := []byte(corruptSigRec.Signature)
	if corruptBytes[0] == 'a' {
		corruptBytes[0] = 'b'
	} else {
		corruptBytes[0] = 'a'
	}
	corruptSigRec.Signature = string(corruptBytes)
	if err := VerifyApproval(&corruptSigRec, "cfg-digest-1", "verif-digest-1", secretKey); err == nil {
		t.Errorf("SECURITY DEFECT: VerifyApproval accepted corrupted signature")
	}

	// 6. VerifyApproval with truncated signature MUST fail
	truncatedSigRec := *apprRec
	truncatedSigRec.Signature = truncatedSigRec.Signature[:16]
	if err := VerifyApproval(&truncatedSigRec, "cfg-digest-1", "verif-digest-1", secretKey); err == nil {
		t.Errorf("SECURITY DEFECT: VerifyApproval accepted truncated signature")
	}
}

func TestAdversarialReplayResistance(t *testing.T) {
	pol := DefaultPolicy()
	propA := mockValidProposal()
	propA.ID = "prop-A-001"
	propB := mockValidProposal()
	propB.ID = "prop-B-002"

	secretKey := []byte("sentra-security-secret-key-32b!")
	approver := ApproverInfo{
		ID:      "sarah-lead",
		Name:    "Sarah SRE Lead",
		Role:    "infrastructure-lead",
		IsAgent: false,
	}

	// Authorize Proposal A
	apprRecA, err := Authorize(&propA, approver, "cfg-digest-A", "verif-digest-A", pol, "APPROVED", "auth for A", secretKey)
	if err != nil {
		t.Fatalf("Authorize propA: %v", err)
	}

	// 1. Direct Replay: Attempting to verify Approval A against Proposal B's digests MUST fail
	if err := VerifyApproval(apprRecA, "cfg-digest-B", "verif-digest-A", secretKey); err == nil {
		t.Errorf("SECURITY DEFECT: replay attack succeeded across different config digests")
	}

	// 2. Identity Tampering Replay: Adversary alters ProposalID in record A to match Proposal B
	forgedRec := *apprRecA
	forgedRec.ProposalID = propB.ID
	if err := VerifyApproval(&forgedRec, "cfg-digest-A", "verif-digest-A", secretKey); err == nil {
		t.Errorf("SECURITY DEFECT: signature check failed to detect forged ProposalID")
	}
}

func TestAdversarialAuditChainIntegrity(t *testing.T) {
	log := NewAuditLog()

	_, err := log.Append("prop-1", ActionInvestigated, "agent", "d1", "event 1")
	if err != nil {
		t.Fatalf("Append e1: %v", err)
	}
	_, err = log.Append("prop-1", ActionProposed, "agent", "d2", "event 2")
	if err != nil {
		t.Fatalf("Append e2: %v", err)
	}
	_, err = log.Append("prop-1", ActionApproved, "sarah-lead", "d3", "event 3")
	if err != nil {
		t.Fatalf("Append e3: %v", err)
	}

	if err := log.VerifyIntegrity(); err != nil {
		t.Fatalf("baseline audit chain verification failed: %v", err)
	}

	// 1. Tamper with PrevHash of middle entry
	logTamperedPrev := NewAuditLog()
	logTamperedPrev.events = append([]AuditEvent{}, log.events...)
	logTamperedPrev.events[1].PrevHash = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := logTamperedPrev.VerifyIntegrity(); err == nil {
		t.Errorf("SECURITY DEFECT: audit log accepted corrupted PrevHash")
	}

	// 2. Tamper with EntryHash of middle entry
	logTamperedEntry := NewAuditLog()
	logTamperedEntry.events = append([]AuditEvent{}, log.events...)
	logTamperedEntry.events[1].EntryHash = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := logTamperedEntry.VerifyIntegrity(); err == nil {
		t.Errorf("SECURITY DEFECT: audit log accepted corrupted EntryHash")
	}

	// 3. Drop middle entry from chain (deletion attack)
	logDropped := NewAuditLog()
	logDropped.events = []AuditEvent{log.events[0], log.events[2]}
	if err := logDropped.VerifyIntegrity(); err == nil {
		t.Errorf("SECURITY DEFECT: audit log failed to detect deleted intermediate event")
	}
}

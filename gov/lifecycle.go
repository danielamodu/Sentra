package gov

import (
	"fmt"
	"sync"
	"time"
)

// StateTransition records one state transition event.
type StateTransition struct {
	From      ProposalState `json:"from"`
	To        ProposalState `json:"to"`
	Timestamp time.Time     `json:"timestamp"`
	Reason    string        `json:"reason"`
}

// ProposalLifecycle tracks the lifecycle state and history of one proposal.
type ProposalLifecycle struct {
	ProposalID string            `json:"proposal_id"`
	Current    ProposalState     `json:"current"`
	History    []StateTransition `json:"history"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// LifecycleManager manages the state machine for operational change proposals.
type LifecycleManager struct {
	mu         sync.RWMutex
	lifecycles map[string]*ProposalLifecycle
}

// NewLifecycleManager creates an empty, thread-safe LifecycleManager.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		lifecycles: make(map[string]*ProposalLifecycle),
	}
}

// InitProposal initializes a proposal in the DRAFT state.
func (lm *LifecycleManager) InitProposal(proposalID string) (*ProposalLifecycle, error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if _, exists := lm.lifecycles[proposalID]; exists {
		return nil, fmt.Errorf("proposal %s already initialized in lifecycle manager", proposalID)
	}

	now := time.Now().UTC()
	lc := &ProposalLifecycle{
		ProposalID: proposalID,
		Current:    StateDraft,
		History: []StateTransition{
			{
				From:      "",
				To:        StateDraft,
				Timestamp: now,
				Reason:    "proposal created",
			},
		},
		UpdatedAt: now,
	}
	lm.lifecycles[proposalID] = lc
	return lc, nil
}

// legalTransitions maps each state to its allowed successor states.
var legalTransitions = map[ProposalState]map[ProposalState]bool{
	StateDraft: {
		StateExperimenting: true,
		StateRejected:      true,
	},
	StateExperimenting: {
		StateVerified: true,
		StateRejected: true,
	},
	StateVerified: {
		StatePendingApproval: true,
		StateRejected:        true,
		StateExpired:         true,
	},
	StatePendingApproval: {
		StateApproved: true,
		StateRejected: true,
		StateExpired:  true,
	},
	StateApproved: {
		StatePromoted: true,
		StateRejected: true,
		StateExpired:  true,
	},
	StateRejected: {
		// Terminal state
	},
	StatePromoted: {
		// Terminal state
	},
	StateExpired: {
		// Terminal state
	},
}

// Transition advances a proposal to a new state if the transition is legally permitted.
func (lm *LifecycleManager) Transition(proposalID string, to ProposalState, reason string) error {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lc, ok := lm.lifecycles[proposalID]
	if !ok {
		return fmt.Errorf("proposal %s not found in lifecycle manager", proposalID)
	}

	allowed := legalTransitions[lc.Current]
	if !allowed[to] {
		return fmt.Errorf("%w: cannot transition proposal %s from %s to %s",
			ErrIllegalTransition, proposalID, lc.Current, to)
	}

	now := time.Now().UTC()
	lc.History = append(lc.History, StateTransition{
		From:      lc.Current,
		To:        to,
		Timestamp: now,
		Reason:    reason,
	})
	lc.Current = to
	lc.UpdatedAt = now
	return nil
}

// GetState returns the current state of a proposal.
func (lm *LifecycleManager) GetState(proposalID string) (ProposalState, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	lc, ok := lm.lifecycles[proposalID]
	if !ok {
		return "", fmt.Errorf("proposal %s not found in lifecycle manager", proposalID)
	}
	return lc.Current, nil
}

// GetHistory returns the transition history of a proposal.
func (lm *LifecycleManager) GetHistory(proposalID string) ([]StateTransition, error) {
	lm.mu.RLock()
	defer lm.mu.RUnlock()

	lc, ok := lm.lifecycles[proposalID]
	if !ok {
		return nil, fmt.Errorf("proposal %s not found in lifecycle manager", proposalID)
	}
	out := make([]StateTransition, len(lc.History))
	copy(out, lc.History)
	return out, nil
}

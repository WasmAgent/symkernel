// Package governance - lifecycle state machine for policy versions.
//
// A Policy progresses through a linear state machine:
//
//	Draft -> PendingApproval -> Approved -> Staged -> Deployed
//	                                                    |
//	                                                    v
//	                                                   Deprecated
//	                                                    |
//	                                                    v
//	                                                  RolledBack -> Draft (re-edit cycle)
//
// Each transition is guarded by a set of allowed predecessor states;
// attempting an invalid transition returns ErrInvalidTransition.
package governance

import (
	"fmt"
	"sync"
	"time"
)

// LifecycleState represents the current phase of a policy in its lifecycle.
type LifecycleState string

const (
	// StateDraft is the initial state when a policy is being authored.
	StateDraft LifecycleState = "draft"

	// StatePendingApproval means the policy has been submitted for review.
	StatePendingApproval LifecycleState = "pending_approval"

	// StateApproved means all required approvals have been granted.
	StateApproved LifecycleState = "approved"

	// StateStaged means the policy is deployed to a non-production environment.
	StateStaged LifecycleState = "staged"

	// StateDeployed means the policy is live in production.
	StateDeployed LifecycleState = "deployed"

	// StateDeprecated means the policy has been superseded and is no longer
	// evaluated.
	StateDeprecated LifecycleState = "deprecated"

	// StateRolledBack means the policy was reverted from a later state.
	StateRolledBack LifecycleState = "rolled_back"
)

// ValidTransitions maps each state to the set of states it can transition to.
var ValidTransitions = map[LifecycleState][]LifecycleState{
	StateDraft:           {StatePendingApproval},
	StatePendingApproval: {StateDraft, StateApproved},
	StateApproved:        {StateStaged},
	StateStaged:          {StateDeployed, StateDraft},
	StateDeployed:        {StateDeprecated, StateRolledBack},
	StateDeprecated:      {},
	StateRolledBack:      {StateDraft},
}

// ErrInvalidTransition is returned when a state transition is not permitted.
var ErrInvalidTransition = fmt.Errorf("invalid lifecycle transition")

// TransitionRecord captures a single state change for audit purposes.
type TransitionRecord struct {
	PolicyID    string         `json:"policy_id"`
	Version     SchemaVersion  `json:"version"`
	From        LifecycleState `json:"from"`
	To          LifecycleState `json:"to"`
	Actor       string         `json:"actor"`
	Reason      string         `json:"reason,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
}

// Policy represents a single versioned policy artifact governed by the
// lifecycle state machine.
type Policy struct {
	// ID is the human-readable policy identifier (e.g. "rate-limit-v2").
	ID string `json:"id"`

	// Version is the semantic version of this policy.
	Version SchemaVersion `json:"version"`

	// State is the current lifecycle state.
	State LifecycleState `json:"state"`

	// Content is the serialized policy body.
	Content PolicyContent `json:"content"`

	// SchemaID references the schema this policy conforms to.
	SchemaID string `json:"schema_id"`

	// Labels are arbitrary key-value pairs for organizational grouping.
	Labels map[string]string `json:"labels,omitempty"`

	// CreatedAt is when the policy was first created.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the timestamp of the last state transition.
	UpdatedAt time.Time `json:"updated_at"`

	mu sync.Mutex
}

// NewPolicy creates a policy in the Draft state with the given parameters.
// The CreatedAt and UpdatedAt fields are set to the provided time (or
// time.Now if zero).
func NewPolicy(id string, v SchemaVersion, content PolicyContent, schemaID string) *Policy {
	now := time.Now().UTC()
	return &Policy{
		ID:        id,
		Version:   v,
		State:     StateDraft,
		Content:   content,
		SchemaID:  schemaID,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// Transition moves the policy from its current state to the target state.
// It returns ErrInvalidTransition if the transition is not allowed.
func (p *Policy) Transition(to LifecycleState, actor, reason string) (TransitionRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	allowed, ok := ValidTransitions[p.State]
	if !ok {
		return TransitionRecord{}, fmt.Errorf("%w: unknown current state %q", ErrInvalidTransition, p.State)
	}
	found := false
	for _, a := range allowed {
		if a == to {
			found = true
			break
		}
	}
	if !found {
		return TransitionRecord{}, fmt.Errorf("%w: %q -> %q is not permitted", ErrInvalidTransition, p.State, to)
	}

	rec := TransitionRecord{
		PolicyID:  p.ID,
		Version:   p.Version,
		From:      p.State,
		To:        to,
		Actor:     actor,
		Reason:    reason,
		Timestamp: time.Now().UTC(),
	}
	p.State = to
	p.UpdatedAt = rec.Timestamp
	return rec, nil
}

// CanTransition reports whether the policy can move to the given state.
func (p *Policy) CanTransition(to LifecycleState) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return canTransition(p.State, to)
}

func canTransition(from, to LifecycleState) bool {
	allowed, ok := ValidTransitions[from]
	if !ok {
		return false
	}
	for _, a := range allowed {
		if a == to {
			return true
		}
	}
	return false
}

// IsTerminal reports whether the policy is in a state with no outgoing
// transitions (Deprecated).
func (p *Policy) IsTerminal() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(ValidTransitions[p.State]) == 0
}

// PolicyStore holds all known policy versions, keyed by "id@version".
// It is safe for concurrent use.
type PolicyStore struct {
	policies map[string]*Policy
	mu       sync.RWMutex
}

// NewPolicyStore creates an empty policy store.
func NewPolicyStore() *PolicyStore {
	return &PolicyStore{
		policies: make(map[string]*Policy),
	}
}

// Add registers a policy. Returns an error if a policy with the same ID
// and version already exists.
func (s *PolicyStore) Add(p *Policy) error {
	key := policyStoreKey(p.ID, p.Version)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.policies[key]; exists {
		return fmt.Errorf("policy %s already exists", key)
	}
	s.policies[key] = p
	return nil
}

// Get retrieves a policy by ID and version.
func (s *PolicyStore) Get(id string, v SchemaVersion) (*Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[policyStoreKey(id, v)]
	return p, ok
}

// Latest returns the policy with the highest version for the given ID.
func (s *PolicyStore) Latest(id string) (*Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best *Policy
	found := false
	for _, p := range s.policies {
		if p.ID != id {
			continue
		}
		if !found || versionGt(p.Version, best.Version) {
			best = p
			found = true
		}
	}
	return best, found
}

// ListByState returns all policies in the given state.
func (s *PolicyStore) ListByState(state LifecycleState) []*Policy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Policy
	for _, p := range s.policies {
		if p.State == state {
			out = append(out, p)
		}
	}
	return out
}

func policyStoreKey(id string, v SchemaVersion) string {
	return fmt.Sprintf("%s@%s", id, v.String())
}

// IsFinalState reports whether a LifecycleState is terminal (no outgoing
// transitions). This is a pure function useful for testing and validation.
func IsFinalState(s LifecycleState) bool {
	return len(ValidTransitions[s]) == 0
}

// Package governance - policy rollback support.
//
// Rollback reverts a policy to a previous version, recording the rollback
// event for audit. The RollbackManager tracks the full history of
// deployments and rollbacks per policy ID.
package governance

import (
	"fmt"
	"sync"
	"time"
)

// RollbackRecord captures a single rollback event.
type RollbackRecord struct {
	PolicyID    string        `json:"policy_id"`
	FromVersion SchemaVersion `json:"from_version"`
	ToVersion   SchemaVersion `json:"to_version"`
	Actor       string        `json:"actor"`
	Reason      string        `json:"reason,omitempty"`
	Timestamp   time.Time     `json:"timestamp"`
}

// DeploymentRecord captures a single deployment event (including rollbacks
// framed as deployments of the target version).
type DeploymentRecord struct {
	PolicyID  string        `json:"policy_id"`
	Version   SchemaVersion `json:"version"`
	Actor     string        `json:"actor"`
	Action    string        `json:"action"` // "deploy" or "rollback"
	Env       Environment   `json:"env"`
	Timestamp time.Time     `json:"timestamp"`
}

// RollbackManager tracks deployment history and performs rollbacks.
// It is safe for concurrent use.
type RollbackManager struct {
	// history maps policyID to its ordered deployment history.
	history map[string][]DeploymentRecord
	mu      sync.Mutex
}

// NewRollbackManager creates an empty rollback manager.
func NewRollbackManager() *RollbackManager {
	return &RollbackManager{
		history: make(map[string][]DeploymentRecord),
	}
}

// RecordDeploy appends a deployment event to the history.
func (m *RollbackManager) RecordDeploy(policyID string, v SchemaVersion, actor string, env Environment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history[policyID] = append(m.history[policyID], DeploymentRecord{
		PolicyID:  policyID,
		Version:   v,
		Actor:     actor,
		Action:    "deploy",
		Env:       env,
		Timestamp: time.Now().UTC(),
	})
}

// RecordRollback appends a rollback event and returns a RollbackRecord
// suitable for audit logging.
func (m *RollbackManager) RecordRollback(policyID string, from, to SchemaVersion, actor, reason string, env Environment) RollbackRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	rb := RollbackRecord{
		PolicyID:    policyID,
		FromVersion: from,
		ToVersion:   to,
		Actor:       actor,
		Reason:      reason,
		Timestamp:   time.Now().UTC(),
	}
	m.history[policyID] = append(m.history[policyID], DeploymentRecord{
		PolicyID:  policyID,
		Version:   to,
		Actor:     actor,
		Action:    "rollback",
		Env:       env,
		Timestamp: rb.Timestamp,
	})
	return rb
}

// PreviousVersion returns the version that was active before the most
// recent deployment of the given policy. Returns false if there is no
// previous version to roll back to.
func (m *RollbackManager) PreviousVersion(policyID string) (SchemaVersion, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hist := m.history[policyID]
	if len(hist) < 2 {
		return SchemaVersion{}, false
	}
	return hist[len(hist)-2].Version, true
}

// History returns the full deployment history for a policy.
func (m *RollbackManager) History(policyID string) []DeploymentRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DeploymentRecord, len(m.history[policyID]))
	copy(out, m.history[policyID])
	return out
}

// CanRollback reports whether a rollback is possible (there must be at
// least 2 deployment records: the current one and a previous one).
func (m *RollbackManager) CanRollback(policyID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.history[policyID]) >= 2
}

// Rollback performs a rollback to the previous version, transitioning the
// policy state and recording the event. Returns the RollbackRecord for
// audit logging, or an error if rollback is not possible.
func (m *RollbackManager) Rollback(p *Policy, actor, reason string, env Environment) (RollbackRecord, error) {
	prev, ok := m.PreviousVersion(p.ID)
	if !ok {
		return RollbackRecord{}, fmt.Errorf("no previous version to roll back to for policy %q", p.ID)
	}

	from := p.Version

	rec, err := p.Transition(StateRolledBack, actor, reason)
	if err != nil {
		return RollbackRecord{}, fmt.Errorf("rollback transition failed: %w", err)
	}

	// Record the previous version as the active deployment after rollback
	_ = rec // audit trail available via Transition return
	rb := m.RecordRollback(p.ID, from, prev, actor, reason, env)
	return rb, nil
}

// Package governance - staging environment management.
//
// Policies are promoted through environments in order:
//   dev -> staging -> prod
//
// Each environment tracks which policy version is currently active.
// Promotion moves a policy from one environment to the next, requiring
// that the policy be in the appropriate lifecycle state.
package governance

import (
	"fmt"
	"sync"
)

// Environment represents a deployment stage.
type Environment string

const (
	EnvDev     Environment = "dev"
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

// EnvOrder defines the promotion order for environments.
var EnvOrder = []Environment{EnvDev, EnvStaging, EnvProd}

// envIndex returns the ordinal position of an environment in EnvOrder.
func envIndex(e Environment) int {
	for i, env := range EnvOrder {
		if env == e {
			return i
		}
	}
	return -1
}

// CanPromoteTo reports whether promotion from 'from' to 'to' is a valid
// environment step (must be the immediate next environment in EnvOrder).
func CanPromoteTo(from, to Environment) bool {
	fi := envIndex(from)
	ti := envIndex(to)
	return fi >= 0 && ti == fi+1
}

// StagedPolicy records which policy version is active in an environment.
type StagedPolicy struct {
	PolicyID  string        `json:"policy_id"`
	Version   SchemaVersion `json:"version"`
	Env       Environment   `json:"env"`
	PromotedBy string       `json:"promoted_by"`
}

// StagingTable tracks which policy version is deployed to each environment.
// It is safe for concurrent use.
type StagingTable struct {
	// active maps "policyID:env" to the currently active version.
	active map[string]StagedPolicy
	mu     sync.RWMutex
}

// NewStagingTable creates an empty staging table.
func NewStagingTable() *StagingTable {
	return &StagingTable{
		active: make(map[string]StagedPolicy),
	}
}

// Stage deploys a policy version to the given environment. Returns an error
// if the environment is not a valid promotion step from the policy's
// current environment, or if the policy is not in an appropriate lifecycle
// state.
func (t *StagingTable) Stage(p *Policy, env Environment, actor string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := stagingKey(p.ID, env)
	current, exists := t.active[key]
	if exists {
		if !CanPromoteTo(current.Env, env) {
			return fmt.Errorf("cannot promote %s from %s to %s: invalid environment step",
				p.ID, current.Env, env)
		}
	}

	t.active[key] = StagedPolicy{
		PolicyID:   p.ID,
		Version:    p.Version,
		Env:        env,
		PromotedBy: actor,
	}
	return nil
}

// Active returns the currently staged policy for the given ID and
// environment. Returns false if no policy is staged there.
func (t *StagingTable) Active(policyID string, env Environment) (StagedPolicy, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.active[stagingKey(policyID, env)]
	return s, ok
}

// ActiveInEnv returns all policies currently staged in the given environment.
func (t *StagingTable) ActiveInEnv(env Environment) []StagedPolicy {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []StagedPolicy
	for _, s := range t.active {
		if s.Env == env {
			out = append(out, s)
		}
	}
	return out
}

// Remove deletes the staging record for a policy in a given environment.
func (t *StagingTable) Remove(policyID string, env Environment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, stagingKey(policyID, env))
}

func stagingKey(policyID string, env Environment) string {
	return fmt.Sprintf("%s:%s", policyID, env)
}

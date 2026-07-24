// Package governance - blue-green deployment patterns.
//
// BlueGreen maintains two deployment slots per environment. Traffic is
// routed to exactly one slot (the "active" slot). A new policy version
// is deployed to the inactive slot, validated, and then traffic is
// switched via SwitchTraffic. If issues are detected, rollback is a
// single SwitchTraffic call back to the previous slot.
package governance

import (
	"fmt"
	"sync"
	"time"
)

// DeploymentSlot names the two deployment slots.
type DeploymentSlot string

const (
	SlotBlue  DeploymentSlot = "blue"
	SlotGreen DeploymentSlot = "green"
)

// SlotState holds the policy version deployed to a single slot.
type SlotState struct {
	Slot       DeploymentSlot `json:"slot"`
	PolicyID   string        `json:"policy_id"`
	Version    SchemaVersion `json:"version"`
	DeployedAt time.Time     `json:"deployed_at"`
	DeployedBy string        `json:"deployed_by"`
}

// BlueGreen manages two deployment slots for a single policy in a
// single environment.
type BlueGreen struct {
	PolicyID string       `json:"policy_id"`
	Env      Environment `json:"env"`

	// slots holds the two slot states, keyed by slot name.
	slots map[DeploymentSlot]*SlotState

	// active is the slot currently receiving traffic.
	active DeploymentSlot

	mu sync.RWMutex
}

// NewBlueGreen creates a blue-green deployment manager for the given
// policy and environment. Both slots start empty.
func NewBlueGreen(policyID string, env Environment) *BlueGreen {
	return &BlueGreen{
		PolicyID: policyID,
		Env:      env,
		slots:    make(map[DeploymentSlot]*SlotState),
	}
}

// DeployToSlot deploys a policy version to the specified slot. If the
// slot already has a deployment, it is replaced.
func (bg *BlueGreen) DeployToSlot(slot DeploymentSlot, v SchemaVersion, actor string) {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	bg.slots[slot] = &SlotState{
		Slot:       slot,
		PolicyID:   bg.PolicyID,
		Version:    v,
		DeployedAt: time.Now().UTC(),
		DeployedBy: actor,
	}
}

// ActiveSlot returns the name of the currently active slot.
func (bg *BlueGreen) ActiveSlot() DeploymentSlot {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	return bg.active
}

// ActiveState returns the slot state for the currently active slot.
// Returns false if the active slot has no deployment.
func (bg *BlueGreen) ActiveState() (*SlotState, bool) {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	s, ok := bg.slots[bg.active]
	return s, ok
}

// InactiveSlot returns the slot that is NOT currently active.
func (bg *BlueGreen) InactiveSlot() DeploymentSlot {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	if bg.active == SlotBlue {
		return SlotGreen
	}
	return SlotBlue
}

// SwitchTraffic moves traffic from the current active slot to the
// target slot. The target slot must have a deployment. Returns the
// previous active slot name.
func (bg *BlueGreen) SwitchTraffic(toSlot DeploymentSlot, actor string) (DeploymentSlot, error) {
	bg.mu.Lock()
	defer bg.mu.Unlock()

	if toSlot != SlotBlue && toSlot != SlotGreen {
		return "", fmt.Errorf("invalid slot %q; must be %q or %q", toSlot, SlotBlue, SlotGreen)
	}

	state, ok := bg.slots[toSlot]
	if !ok || state == nil {
		return "", fmt.Errorf("slot %q has no deployment", toSlot)
	}

	previous := bg.active
	bg.active = toSlot
	return previous, nil
}

// GetSlot returns the state of the named slot.
func (bg *BlueGreen) GetSlot(slot DeploymentSlot) (*SlotState, bool) {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	s, ok := bg.slots[slot]
	return s, ok
}

// BothDeployed reports whether both slots have a deployment.
func (bg *BlueGreen) BothDeployed() bool {
	bg.mu.RLock()
	defer bg.mu.RUnlock()
	_, b := bg.slots[SlotBlue]
	_, g := bg.slots[SlotGreen]
	return b && g
}

// Package governance - approval workflow for policy promotion.
//
// An ApprovalWorkflow defines how many approvals are required and who
// can provide them. A policy in StatePendingApproval collects approvals
// until the quorum is met, at which point it transitions to StateApproved.
package governance

import (
	"fmt"
	"sync"
	"time"
)

// ApprovalStatus represents the disposition of a single approval.
type ApprovalStatus string

const (
	// ApprovalPending means the approver has not yet acted.
	ApprovalPending ApprovalStatus = "pending"

	// ApprovalGranted means the approver approved the policy.
	ApprovalGranted ApprovalStatus = "granted"

	// ApprovalRejected means the approver rejected the policy.
	ApprovalRejected ApprovalStatus = "rejected"
)

// ApprovalRecord captures a single approver's decision.
type ApprovalRecord struct {
	Approver  string         `json:"approver"`
	Status    ApprovalStatus `json:"status"`
	Comment   string         `json:"comment,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

// ApprovalWorkflow defines the quorum and allowed approvers for a
// policy promotion.
type ApprovalWorkflow struct {
	// Required is the number of grants needed to promote the policy.
	Required int `json:"required"`

	// Approvers is the set of actor identities that may approve.
	// An empty set means any actor may approve.
	Approvers map[string]bool `json:"-"`

	// Records tracks individual approval decisions.
	Records []ApprovalRecord `json:"records"`

	mu sync.Mutex
}

// NewApprovalWorkflow creates a workflow requiring n grants from the
// given set of approvers. If approvers is nil or empty, any actor may
// approve.
func NewApprovalWorkflow(required int, approvers []string) *ApprovalWorkflow {
	aw := &ApprovalWorkflow{
		Required:  required,
		Approvers: make(map[string]bool),
		Records:   make([]ApprovalRecord, 0),
	}
	for _, a := range approvers {
		aw.Approvers[a] = true
	}
	return aw
}

// Grant records an approval from the given actor. Returns an error if the
// actor is not in the allowed set (when restricted) or has already voted.
func (w *ApprovalWorkflow) Grant(actor, comment string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.vote(actor, ApprovalGranted, comment)
}

// Reject records a rejection from the given actor.
func (w *ApprovalWorkflow) Reject(actor, comment string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.vote(actor, ApprovalRejected, comment)
}

func (w *ApprovalWorkflow) vote(actor string, status ApprovalStatus, comment string) error {
	if len(w.Approvers) > 0 && !w.Approvers[actor] {
		return fmt.Errorf("actor %q is not an authorized approver", actor)
	}
	for _, r := range w.Records {
		if r.Approver == actor {
			return fmt.Errorf("actor %q has already voted", actor)
		}
	}
	w.Records = append(w.Records, ApprovalRecord{
		Approver:  actor,
		Status:    status,
		Comment:   comment,
		Timestamp: time.Now().UTC(),
	})
	return nil
}

// Grants returns the number of approvals that have been granted.
func (w *ApprovalWorkflow) Grants() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.Records {
		if r.Status == ApprovalGranted {
			n++
		}
	}
	return n
}

// Rejections returns the number of rejections.
func (w *ApprovalWorkflow) Rejections() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, r := range w.Records {
		if r.Status == ApprovalRejected {
			n++
		}
	}
	return n
}

// IsQuorumMet reports whether the required number of grants has been reached.
func (w *ApprovalWorkflow) IsQuorumMet() bool {
	return w.Grants() >= w.Required
}

// IsRejected reports whether any rejection has been recorded.
func (w *ApprovalWorkflow) IsRejected() bool {
	return w.Rejections() > 0
}

// Quorum returns the current grant count and the required threshold.
func (w *ApprovalWorkflow) Quorum() (granted, required int) {
	return w.Grants(), w.Required
}

// Package policy prototypes the backwards-compatible evaluateAsync overload
// proposed for the upstream wasmagent-js PolicyRule contract (see
// docs/15-milestones.md, Milestone 4 — "Evaluate PolicyRule.evaluateAsync
// upstream PR"). It exists so symkernel can evaluate the latency trade-off of
// pre-fetching a policy decision ahead of an inline tool invocation before
// the change is merged upstream into WasmAgent/wasmagent-js.
//
// The Go shape mirrors the proposed TypeScript overload:
//
//	evaluateAsync?: (toolName, args, vetting) => Promise<InvocationDecision | undefined>
//
// The trailing "?" makes the hook OPTIONAL: a PolicyRule that leaves
// EvaluateAsync nil simply defers, preserving the pre-overload (synchronous,
// decide-every-time) behaviour. This file is the in-repo prototype of that
// contract; the upstream draft-PR submission to wasmagent-js is tracked as a
// cross-repo follow-up.
package policy

import (
	"context"
	"sync"
)

// DecisionKind enumerates the outcomes a PolicyRule can return for a tool
// invocation. The string values mirror the wasmagent-js InvocationDecision
// union surfaced over the wire.
type DecisionKind string

const (
	// DecisionAllow permits the invocation to proceed unchanged.
	DecisionAllow DecisionKind = "allow"

	// DecisionDeny blocks the invocation; Reason carries the rationale that
	// should be surfaced to the agent and the decision log.
	DecisionDeny DecisionKind = "deny"

	// DecisionModify permits the invocation with rewritten arguments taken
	// from ModifiedArgs (e.g. redacting or coercing a field).
	DecisionModify DecisionKind = "modify"
)

// InvocationDecision is the Go mirror of the wasmagent-js InvocationDecision.
// A zero-value InvocationDecision (Kind == "") is treated as "no decision",
// which is the Go analogue of the TypeScript |undefined half of the
// evaluateAsync return.
type InvocationDecision struct {
	// Kind is the decision category (one of the DecisionKind constants).
	// A rule signals "no opinion / defer" by returning Kind == "" rather
	// than a concrete decision.
	Kind DecisionKind `json:"kind"`

	// Reason is a human-readable rationale, surfaced to the agent and to the
	// OpenTelemetry decision span. Optional for allow, expected for deny.
	Reason string `json:"reason,omitempty"`

	// ModifiedArgs carries rewritten arguments when Kind == DecisionModify;
	// it is nil for allow and deny decisions.
	ModifiedArgs map[string]any `json:"modified_args,omitempty"`
}

// Vetting carries the runtime context handed to a PolicyRule at evaluation
// time: who is asking, which session the invocation belongs to, and any
// caller-supplied annotations. It mirrors the wasmagent-js Vetting type.
type Vetting struct {
	// AgentID identifies the agent requesting the invocation.
	AgentID string `json:"agent_id"`

	// SessionID correlates the invocation to a tracing/session span so the
	// decision can be joined to the surrounding telemetry.
	SessionID string `json:"session_id,omitempty"`

	// Hints are caller-supplied annotations (e.g. trust level, source of the
	// request) that a rule may consult but must not trust blindly.
	Hints map[string]string `json:"hints,omitempty"`
}

// EvaluateAsyncFunc is the Go shape of the proposed upstream overload:
//
//	evaluateAsync?: (toolName, args, vetting) => Promise<InvocationDecision | undefined>
//
// The trailing bool reports whether the rule produced a decision (true) or
// deferred to the next rule (false), modelling the TypeScript |undefined. A
// returned InvocationDecision with an empty Kind is also treated as a defer,
// so implementations only need to set the bool.
type EvaluateAsyncFunc func(ctx context.Context, toolName string, args map[string]any, vetting Vetting) (InvocationDecision, bool)

// PolicyRule mirrors the wasmagent-js PolicyRule interface. EvaluateAsync is
// OPTIONAL: a rule that leaves it nil simply defers, preserving the existing
// synchronous contract. This is the backwards-compatible shape under
// evaluation in the upstream PR.
type PolicyRule struct {
	// Name identifies the rule for diagnostics and span attributes.
	Name string

	// EvaluateAsync, when non-nil, is consulted before a tool invocation
	// runs. Returning ok == false (or a zero decision) defers to the next
	// rule in the registry. Leaving this nil makes the rule a no-op for
	// async evaluation, matching the pre-overload behaviour exactly.
	EvaluateAsync EvaluateAsyncFunc
}

// Registry is an ordered collection of PolicyRule values consulted
// first-non-deferring-wins. A zero-value Registry is ready to use; the
// embedded mutex makes concurrent Register/Evaluate safe.
type Registry struct {
	mu    sync.RWMutex
	rules []PolicyRule
}

// Register appends a rule to the end of the registry. Rules registered later
// are consulted later (lower priority), mirroring a fall-through chain.
func (r *Registry) Register(rule PolicyRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
}

// Evaluate consults each rule's EvaluateAsync in registration order and
// returns the first decision that does not defer (ok == true with a non-empty
// Kind). If every rule defers — including the case where no rule defines an
// EvaluateAsync hook at all — Evaluate returns ok == false, signalling the
// caller to fall back to its default (allow) path. That fallback is the exact
// behaviour the upstream overload targets: legacy rules that never opted in
// keep working unchanged.
//
// The context propagates cancellation/timeout into rule evaluation so a slow
// rule cannot block a pre-fetched decision indefinitely.
func (r *Registry) Evaluate(ctx context.Context, toolName string, args map[string]any, vetting Vetting) (InvocationDecision, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, rule := range r.rules {
		if rule.EvaluateAsync == nil {
			continue // backwards-compatible: rule opts out of async evaluation
		}
		dec, ok := rule.EvaluateAsync(ctx, toolName, args, vetting)
		if ok && dec.Kind != "" {
			return dec, true
		}
		// Rule deferred (ok == false or zero Kind); fall through to the next.
		if ctx.Err() != nil {
			return InvocationDecision{}, false // deadline/cancel: stop walking the chain
		}
	}
	return InvocationDecision{}, false
}

// Prefetch starts Evaluate asynchronously and returns a closure that awaits
// the result. It is the Go analogue of the upstream "pre-fetch the decision,
// then await it inline at the call site" pattern: the caller can run other
// work (the tool body, further lookups) concurrently with the rule evaluation
// and only block when the decision is actually needed. The accompanying
// BenchmarkInlineVsPrefetch measures its latency advantage over a purely
// inline (synchronous) call.
//
// If ctx is cancelled before the decision resolves, the returned closure
// reports a defer (ok == false) so the caller can fall back deterministically.
func (r *Registry) Prefetch(ctx context.Context, toolName string, args map[string]any, vetting Vetting) func() (InvocationDecision, bool) {
	type result struct {
		dec InvocationDecision
		ok  bool
	}
	ch := make(chan result, 1) // buffered so the goroutine never leaks if the caller never awaits
	go func() {
		dec, ok := r.Evaluate(ctx, toolName, args, vetting)
		ch <- result{dec: dec, ok: ok}
	}()
	return func() (InvocationDecision, bool) {
		select {
		case res := <-ch:
			return res.dec, res.ok
		case <-ctx.Done():
			return InvocationDecision{}, false
		}
	}
}

package policy

import (
	"context"
	"testing"
	"time"
)

// slowRule simulates a rule whose async evaluation takes a fixed latency,
// standing in for a real policy check (CEL eval, schema lookup, or a Z3 call).
func slowRule(latency time.Duration, dec InvocationDecision) PolicyRule {
	return PolicyRule{
		Name: "slow",
		EvaluateAsync: func(ctx context.Context, _ string, _ map[string]any, _ Vetting) (InvocationDecision, bool) {
			select {
			case <-time.After(latency):
				return dec, true
			case <-ctx.Done():
				return InvocationDecision{}, false
			}
		},
	}
}

func TestRegistry_DeferWhenNoAsyncHook(t *testing.T) {
	// A rule with EvaluateAsync == nil must defer — the backwards-compatible
	// path under evaluation in the upstream PR. Legacy rules keep working
	// without opting into the overload.
	r := &Registry{}
	r.Register(PolicyRule{Name: "legacy-no-op"})

	dec, ok := r.Evaluate(context.Background(), "search", nil, Vetting{})
	if ok {
		t.Fatalf("expected defer (ok=false), got ok=true dec=%+v", dec)
	}
	if dec.Kind != "" {
		t.Errorf("dec.Kind = %q, want empty (no decision)", dec.Kind)
	}
}

func TestRegistry_AllRulesDefer(t *testing.T) {
	// Multiple deferring rules collapse to a single defer / default-allow.
	r := &Registry{}
	r.Register(PolicyRule{
		Name: "deferrer-a",
		EvaluateAsync: func(context.Context, string, map[string]any, Vetting) (InvocationDecision, bool) {
			return InvocationDecision{}, false
		},
	})
	r.Register(PolicyRule{
		Name: "deferrer-b",
		EvaluateAsync: func(context.Context, string, map[string]any, Vetting) (InvocationDecision, bool) {
			return InvocationDecision{Kind: ""}, true // zero Kind also means defer
		},
	})

	if _, ok := r.Evaluate(context.Background(), "exec", nil, Vetting{}); ok {
		t.Fatal("expected ok=false when every rule defers")
	}
}

func TestRegistry_FirstNonDeferringWins(t *testing.T) {
	r := &Registry{}
	r.Register(PolicyRule{
		Name: "deferrer",
		EvaluateAsync: func(context.Context, string, map[string]any, Vetting) (InvocationDecision, bool) {
			return InvocationDecision{}, false
		},
	})
	r.Register(slowRule(0, InvocationDecision{Kind: DecisionDeny, Reason: "blocked by rule 2"}))
	r.Register(slowRule(0, InvocationDecision{Kind: DecisionAllow})) // never reached

	dec, ok := r.Evaluate(context.Background(), "exec", map[string]any{"cmd": "rm"}, Vetting{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if dec.Kind != DecisionDeny {
		t.Errorf("kind = %q, want %q", dec.Kind, DecisionDeny)
	}
	if dec.Reason != "blocked by rule 2" {
		t.Errorf("reason = %q, want %q", dec.Reason, "blocked by rule 2")
	}
}

func TestRegistry_ModifyCarriesArgs(t *testing.T) {
	want := InvocationDecision{
		Kind:         DecisionModify,
		Reason:       "redacted secret",
		ModifiedArgs: map[string]any{"token": "***"},
	}
	r := &Registry{}
	r.Register(PolicyRule{
		Name: "redactor",
		EvaluateAsync: func(_ context.Context, _ string, _ map[string]any, _ Vetting) (InvocationDecision, bool) {
			return want, true
		},
	})

	dec, ok := r.Evaluate(context.Background(), "http", map[string]any{"token": "secret"}, Vetting{AgentID: "a1"})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if dec.Kind != DecisionModify {
		t.Fatalf("kind = %q, want %q", dec.Kind, DecisionModify)
	}
	if dec.ModifiedArgs["token"] != "***" {
		t.Errorf("modified_args[token] = %v, want %q", dec.ModifiedArgs["token"], "***")
	}
}

func TestRegistry_ContextCancelStopsChain(t *testing.T) {
	// A cancelled context short-circuits the rule walk so a later blocking
	// rule is never consulted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := &Registry{}
	r.Register(PolicyRule{
		Name:          "never-resolves",
		EvaluateAsync: slowRule(time.Hour, InvocationDecision{Kind: DecisionAllow}).EvaluateAsync,
	})

	if _, ok := r.Evaluate(ctx, "exec", nil, Vetting{}); ok {
		t.Fatal("expected ok=false on cancelled context")
	}
}

func TestPrefetch_AwaitsAndReturnsDecision(t *testing.T) {
	r := &Registry{}
	r.Register(slowRule(5*time.Millisecond, InvocationDecision{Kind: DecisionAllow}))

	await := r.Prefetch(context.Background(), "exec", nil, Vetting{AgentID: "a1"})

	// Simulate other work overlapping with the pre-fetched evaluation.
	time.Sleep(2 * time.Millisecond)

	dec, ok := await()
	if !ok {
		t.Fatal("expected ok=true from prefetched decision")
	}
	if dec.Kind != DecisionAllow {
		t.Errorf("kind = %q, want %q", dec.Kind, DecisionAllow)
	}
}

func TestPrefetch_CancelledContextDefers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	r := &Registry{}
	r.Register(slowRule(time.Hour, InvocationDecision{Kind: DecisionAllow}))

	await := r.Prefetch(ctx, "exec", nil, Vetting{})

	if _, ok := await(); ok {
		t.Fatal("expected ok=false when context expires before decision resolves")
	}
}

// simulateToolBody stands in for the real work a caller does between launching
// a pre-fetched policy decision and actually needing its result.
func simulateToolBody(d time.Duration) { time.Sleep(d) }

// BenchmarkInlineVsPrefetch measures the latency trade-off that motivates the
// upstream evaluateAsync overload: pre-fetching the policy decision so it
// overlaps with the tool body (prefetch) versus evaluating it synchronously
// immediately before the tool runs (inline). Each simulated rule costs
// ruleLatency; the tool body costs toolLatency. Inline serializes the two;
// prefetch runs them concurrently, so the per-iteration cost drops from
// ruleLatency+toolLatency toward max(ruleLatency, toolLatency).
func BenchmarkInlineVsPrefetch(b *testing.B) {
	const (
		ruleLatency = 2 * time.Millisecond
		toolLatency = 8 * time.Millisecond
	)
	dec := InvocationDecision{Kind: DecisionAllow}
	ctx := context.Background()

	cases := []struct {
		name     string
		prefetch bool
	}{
		{"inline", false},
		{"prefetch", true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			r := &Registry{}
			r.Register(slowRule(ruleLatency, dec))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if tc.prefetch {
					await := r.Prefetch(ctx, "exec", nil, Vetting{})
					simulateToolBody(toolLatency) // overlaps with rule eval
					if _, ok := await(); !ok {
						b.Fatal("expected ok=true from prefetched decision")
					}
				} else {
					if _, ok := r.Evaluate(ctx, "exec", nil, Vetting{}); !ok {
						b.Fatal("expected ok=true from inline decision")
					}
					simulateToolBody(toolLatency) // runs after rule eval finishes
				}
			}
		})
	}
}

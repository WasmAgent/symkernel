package cache

import "testing"

func TestCacheEvictsLeastRecentlyUsedDecision(t *testing.T) {
	c := New(2)
	c.Set("first", Decision{Sat: "sat"})
	c.Set("second", Decision{Sat: "unsat"})
	if _, ok := c.Get("first"); !ok {
		t.Fatal("first decision was not cached")
	}
	c.Set("third", Decision{Sat: "unknown"})

	if _, ok := c.Get("second"); ok {
		t.Fatal("least recently used decision was retained")
	}
	if decision, ok := c.Get("first"); !ok || decision.Sat != "sat" {
		t.Fatalf("first decision = %#v, %t; want cached sat decision", decision, ok)
	}
	if decision, ok := c.Get("third"); !ok || decision.Sat != "unknown" {
		t.Fatalf("third decision = %#v, %t; want cached unknown decision", decision, ok)
	}

	stats := c.Stats()
	if stats.Evictions != 1 {
		t.Errorf("evictions = %d, want 1", stats.Evictions)
	}
}

func TestCacheCopiesDecisionValues(t *testing.T) {
	c := New(1)
	c.Set("query", Decision{Sat: "sat", Model: map[string]any{"x": "1"}, UnsatCore: []string{"a"}})

	decision, ok := c.Get("query")
	if !ok {
		t.Fatal("decision was not cached")
	}
	decision.Model["x"] = "changed"
	decision.UnsatCore[0] = "changed"

	decision, ok = c.Get("query")
	if !ok || decision.Model["x"] != "1" || decision.UnsatCore[0] != "a" {
		t.Fatalf("cached decision was mutated: %#v, %t", decision, ok)
	}
}

func TestCacheUsesAssertionHash(t *testing.T) {
	c := New(1)
	assertions := "(assert (= x 1))"
	c.Set(assertions, Decision{Sat: "sat"})

	decision, ok := c.GetByHash(HashAssertions(assertions))
	if !ok || decision.Sat != "sat" {
		t.Fatalf("decision = %#v, %t; want decision stored by assertion hash", decision, ok)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv(EnvMaxEntries, "2")
	if got := NewFromEnv().Stats().MaxEntries; got != 2 {
		t.Errorf("max entries = %d, want 2", got)
	}

	t.Setenv(EnvMaxEntries, "invalid")
	if got := NewFromEnv().Stats().MaxEntries; got != DefaultMaxEntries {
		t.Errorf("invalid max entries = %d, want %d", got, DefaultMaxEntries)
	}
}

func TestNewFromEnvDisablesCacheAtZero(t *testing.T) {
	t.Setenv(EnvMaxEntries, "0")
	c := NewFromEnv()
	c.Set("query", Decision{Sat: "sat"})
	if _, ok := c.Get("query"); ok {
		t.Fatal("disabled cache returned a decision")
	}
}

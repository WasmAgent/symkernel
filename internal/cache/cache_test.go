package cache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mustHashContext is a test helper that calls HashContext and fails the test
// on error.
func mustHashContext(t *testing.T, ctx map[string]any) string {
	t.Helper()
	h, err := HashContext(ctx)
	if err != nil {
		t.Fatalf("HashContext: %v", err)
	}
	return h
}

func TestHashExpr_Deterministic(t *testing.T) {
	h1 := HashExpr("x + 1")
	h2 := HashExpr("x + 1")
	if h1 != h2 {
		t.Errorf("HashExpr not deterministic: %q != %q", h1, h2)
	}
	if h1 == "" {
		t.Error("HashExpr returned empty string")
	}
}

func TestHashExpr_DifferentExpressions(t *testing.T) {
	h1 := HashExpr("x + 1")
	h2 := HashExpr("x + 2")
	if h1 == h2 {
		t.Error("HashExpr produced same hash for different expressions")
	}
}

func TestHashContext_Deterministic(t *testing.T) {
	ctx := map[string]any{"x": 1, "y": "hello"}
	h1 := mustHashContext(t, ctx)
	h2 := mustHashContext(t, ctx)
	if h1 != h2 {
		t.Errorf("HashContext not deterministic: %q != %q", h1, h2)
	}
}

func TestHashContext_KeyOrderIndependent(t *testing.T) {
	// Two maps with the same entries in different insertion order should
	// produce the same hash because HashContext sorts keys before serialising.
	ctx1 := map[string]any{"a": 1, "b": 2}
	ctx2 := map[string]any{"b": 2, "a": 1}
	h1 := mustHashContext(t, ctx1)
	h2 := mustHashContext(t, ctx2)
	if h1 != h2 {
		t.Errorf("HashContext key-order dependent: %q != %q", h1, h2)
	}
}

func TestHashContext_Error(t *testing.T) {
	// Values that cannot be marshalled (e.g. channels) should produce an error.
	ctx := map[string]any{"bad": make(chan int)}
	_, err := HashContext(ctx)
	if err == nil {
		t.Error("HashContext did not return an error for unmarshalable context")
	}
}

func TestNew_Defaults(t *testing.T) {
	c := New()
	if c.capacity != defaultCapacity {
		t.Errorf("capacity = %d, want %d", c.capacity, defaultCapacity)
	}
	if c.ttl != defaultTTL {
		t.Errorf("ttl = %v, want %v", c.ttl, defaultTTL)
	}
}

func TestWithCapacity(t *testing.T) {
	c := New(WithCapacity(50))
	if c.capacity != 50 {
		t.Errorf("capacity = %d, want 50", c.capacity)
	}
}

func TestWithTTL(t *testing.T) {
	d := 10 * time.Second
	c := New(WithTTL(d))
	if c.ttl != d {
		t.Errorf("ttl = %v, want %v", c.ttl, d)
	}
}

// --- GetByKey tests ---

func TestSetAndGetByKey(t *testing.T) {
	c := New()
	exprHash := HashExpr("x > 0")
	ctxHash := mustHashContext(t, map[string]any{"x": 5})

	c.Set(exprHash, ctxHash, true, nil)

	val, ok := c.GetByKey(exprHash, ctxHash)
	if !ok {
		t.Fatal("GetByKey returned false (cache miss)")
	}
	if val != true {
		t.Errorf("value = %v, want true", val)
	}
}

func TestGetByKey_Miss(t *testing.T) {
	c := New()
	val, ok := c.GetByKey("nonexistent", "nonexistent")
	if ok {
		t.Error("GetByKey returned true for missing key")
	}
	if val != nil {
		t.Errorf("value = %v, want nil on miss", val)
	}
}

func TestSet_Overwrite(t *testing.T) {
	c := New()
	exprHash := HashExpr("x > 0")
	ctxHash := mustHashContext(t, map[string]any{"x": 5})

	c.Set(exprHash, ctxHash, true, nil)
	c.Set(exprHash, ctxHash, false, nil)

	val, ok := c.GetByKey(exprHash, ctxHash)
	if !ok {
		t.Fatal("GetByKey returned false after overwrite")
	}
	if val != false {
		t.Errorf("value = %v, want false after overwrite", val)
	}
}

// --- Get tests ---

func TestGet_Hit(t *testing.T) {
	c := New()
	expr := "x > 0"
	ctx := map[string]any{"x": 5}
	exprHash := HashExpr(expr)
	ctxHash := mustHashContext(t, ctx)

	c.Set(exprHash, ctxHash, true, []string{"tagA"})

	val, ok, err := c.Get(expr, ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("Get returned false (cache miss)")
	}
	if val != true {
		t.Errorf("value = %v, want true", val)
	}
}

func TestGet_Miss(t *testing.T) {
	c := New()
	ctx := map[string]any{"x": 5}

	val, ok, err := c.Get("nonexistent_expr", ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Error("Get returned true for missing key")
	}
	if val != nil {
		t.Errorf("value = %v, want nil on miss", val)
	}
}

func TestGet_ErrorOnBadContext(t *testing.T) {
	c := New()
	ctx := map[string]any{"bad": make(chan int)}

	_, _, err := c.Get("expr", ctx)
	if err == nil {
		t.Error("Get did not return error for unmarshalable context")
	}
}

func TestGet_RoundTrip(t *testing.T) {
	// Verify that storing via Set and retrieving via Get works when using
	// the same raw expression and context values.
	c := New()

	expr := "x + y > 10"
	ctx := map[string]any{"x": 3, "y": 8}
	exprHash := HashExpr(expr)
	ctxHash := mustHashContext(t, ctx)

	// Store via Set with pre-computed hashes.
	c.Set(exprHash, ctxHash, 11, []string{"test"})

	// Retrieve via Get with raw inputs.
	val, ok, err := c.Get(expr, ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("Get returned false (cache miss) after Set")
	}
	if val != 11 {
		t.Errorf("value = %v, want 11", val)
	}
}

func TestGet_PromotesLRU(t *testing.T) {
	c := New(WithCapacity(3))

	expr0 := "expr0"
	expr1 := "expr1"
	expr2 := "expr2"
	ctx0 := map[string]any{"v": 0}
	ctx1 := map[string]any{"v": 1}
	ctx2 := map[string]any{"v": 2}

	eh0 := HashExpr(expr0)
	ch0 := mustHashContext(t, ctx0)
	eh1 := HashExpr(expr1)
	ch1 := mustHashContext(t, ctx1)
	eh2 := HashExpr(expr2)
	ch2 := mustHashContext(t, ctx2)

	c.Set(eh0, ch0, 0, nil)
	c.Set(eh1, ch1, 1, nil)
	c.Set(eh2, ch2, 2, nil)

	// Access entry 0 via Get to promote it.
	val, ok, err := c.Get(expr0, ctx0)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok || val != 0 {
		t.Fatal("Get failed to retrieve entry 0")
	}

	// Adding a new entry should evict entry 1 (the LRU), not entry 0.
	expr3 := "expr3"
	ctx3 := map[string]any{"v": 3}
	eh3 := HashExpr(expr3)
	ch3 := mustHashContext(t, ctx3)
	c.Set(eh3, ch3, 3, nil)

	// Entry 0 should still be present (it was accessed via Get recently).
	val, ok, err = c.Get(expr0, ctx0)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Error("expected entry 0 to survive LRU eviction (was promoted via Get)")
	}
	if val != 0 {
		t.Errorf("value = %v, want 0", val)
	}

	// Entry 1 should have been evicted.
	_, ok, err = c.Get(expr1, ctx1)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Error("expected entry 1 to be evicted (was LRU)")
	}
}

// --- TTL tests ---

func TestTTL_Expiry(t *testing.T) {
	c := New(WithTTL(50*time.Millisecond))
	exprHash := HashExpr("x > 0")
	ctxHash := mustHashContext(t, map[string]any{"x": 5})

	c.Set(exprHash, ctxHash, true, nil)

	// Should be available immediately.
	val, ok := c.GetByKey(exprHash, ctxHash)
	if !ok {
		t.Fatal("expected cache hit before TTL expiry")
	}
	if val != true {
		t.Errorf("value = %v, want true", val)
	}

	// Wait for expiry.
	time.Sleep(80 * time.Millisecond)

	val, ok = c.GetByKey(exprHash, ctxHash)
	if ok {
		t.Errorf("expected cache miss after TTL expiry, got value = %v", val)
	}
}

func TestTTL_ExpiryViaGet(t *testing.T) {
	c := New(WithTTL(50*time.Millisecond))
	expr := "x > 0"
	ctx := map[string]any{"x": 5}
	exprHash := HashExpr(expr)
	ctxHash := mustHashContext(t, ctx)

	c.Set(exprHash, ctxHash, true, nil)

	// Should be available immediately via Get.
	val, ok, err := c.Get(expr, ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected cache hit before TTL expiry via Get")
	}
	if val != true {
		t.Errorf("value = %v, want true", val)
	}

	// Wait for expiry.
	time.Sleep(80 * time.Millisecond)

	_, ok, err = c.Get(expr, ctx)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if ok {
		t.Error("expected cache miss after TTL expiry via Get")
	}
}

func TestTTL_Override(t *testing.T) {
	c := New(WithTTL(5 * time.Minute))
	exprHash := HashExpr("x > 0")
	ctxHash := mustHashContext(t, map[string]any{"x": 5})

	// Set with a very short TTL override.
	c.Set(exprHash, ctxHash, true, nil, 30*time.Millisecond)

	time.Sleep(50 * time.Millisecond)

	_, ok := c.GetByKey(exprHash, ctxHash)
	if ok {
		t.Error("expected cache miss after TTL override expiry")
	}
}

// --- LRU tests ---

func TestLRU_Eviction(t *testing.T) {
	c := New(WithCapacity(3))

	// Fill the cache to capacity.
	for i := 0; i < 3; i++ {
		exprHash := HashExpr(strings.Repeat("a", i+1))
		ctxHash := mustHashContext(t, map[string]any{"i": i})
		c.Set(exprHash, ctxHash, i, []string{"test"})
	}

	// Fourth entry should evict the LRU (first) entry.
	exprHash := HashExpr("aaaa")
	ctxHash := mustHashContext(t, map[string]any{"i": 3})
	c.Set(exprHash, ctxHash, 3, []string{"test"})

	// Check by size — the first entry should have been evicted.
	_ = HashExpr("a") // verify it doesn't panic
	stats := c.Stats()
	if stats.Size != 3 {
		t.Errorf("size = %d, want 3 after LRU eviction", stats.Size)
	}
	if stats.Evictions == 0 {
		t.Error("expected at least 1 eviction")
	}
}

func TestLRU_AccessPromotes(t *testing.T) {
	c := New(WithCapacity(3))

	eh0 := HashExpr("expr0")
	eh1 := HashExpr("expr1")
	eh2 := HashExpr("expr2")
	ch0 := mustHashContext(t, map[string]any{"v": 0})
	ch1 := mustHashContext(t, map[string]any{"v": 1})
	ch2 := mustHashContext(t, map[string]any{"v": 2})

	c.Set(eh0, ch0, 0, nil)
	c.Set(eh1, ch1, 1, nil)
	c.Set(eh2, ch2, 2, nil)

	// Access entry 0 to promote it.
	c.GetByKey(eh0, ch0)

	// Adding a new entry should evict entry 1 (the LRU), not entry 0.
	eh3 := HashExpr("expr3")
	ch3 := mustHashContext(t, map[string]any{"v": 3})
	c.Set(eh3, ch3, 3, nil)

	// Entry 0 should still be present (it was accessed recently).
	val, ok := c.GetByKey(eh0, ch0)
	if !ok {
		t.Error("expected entry 0 to survive LRU eviction (was promoted)")
	}
	if val != 0 {
		t.Errorf("value = %v, want 0", val)
	}

	// Entry 1 should have been evicted.
	_, ok = c.GetByKey(eh1, ch1)
	if ok {
		t.Error("expected entry 1 to be evicted (was LRU)")
	}
}

// --- Invalidation tests ---

func TestInvalidateByTag(t *testing.T) {
	c := New()

	eh0 := HashExpr("expr0")
	eh1 := HashExpr("expr1")
	ch := mustHashContext(t, map[string]any{"v": 1})

	c.Set(eh0, ch, "result0", []string{"schema:v1"})
	c.Set(eh1, ch, "result1", []string{"schema:v1", "other"})

	// Both entries share the "schema:v1" tag.
	stats := c.Stats()
	if stats.Size != 2 {
		t.Fatalf("size = %d, want 2 before invalidation", stats.Size)
	}

	count := c.InvalidateByTag("schema:v1")
	if count != 2 {
		t.Errorf("InvalidateByTag removed %d entries, want 2", count)
	}

	stats = c.Stats()
	if stats.Size != 0 {
		t.Errorf("size = %d, want 0 after tag invalidation", stats.Size)
	}
}

func TestInvalidateByTag_Partial(t *testing.T) {
	c := New()

	eh0 := HashExpr("expr0")
	eh1 := HashExpr("expr1")
	ch := mustHashContext(t, map[string]any{"v": 1})

	c.Set(eh0, ch, "result0", []string{"tagA"})
	c.Set(eh1, ch, "result1", []string{"tagB"})

	count := c.InvalidateByTag("tagA")
	if count != 1 {
		t.Errorf("InvalidateByTag removed %d entries, want 1", count)
	}

	// Entry 1 should survive.
	val, ok := c.GetByKey(eh1, ch)
	if !ok {
		t.Error("expected entry with tagB to survive tagA invalidation")
	}
	if val != "result1" {
		t.Errorf("value = %v, want %q", val, "result1")
	}
}

func TestInvalidateByTag_Nonexistent(t *testing.T) {
	c := New()
	count := c.InvalidateByTag("nonexistent")
	if count != 0 {
		t.Errorf("InvalidateByTag removed %d entries, want 0", count)
	}
}

func TestInvalidateAll(t *testing.T) {
	c := New()

	for i := 0; i < 5; i++ {
		eh := HashExpr(strings.Repeat("e", i+1))
		ch := mustHashContext(t, map[string]any{"i": i})
		c.Set(eh, ch, i, nil)
	}

	count := c.InvalidateAll()
	if count != 5 {
		t.Errorf("InvalidateAll removed %d entries, want 5", count)
	}

	stats := c.Stats()
	if stats.Size != 0 {
		t.Errorf("size = %d, want 0 after InvalidateAll", stats.Size)
	}
}

// --- Stats tests ---

func TestStats_Initial(t *testing.T) {
	c := New()
	stats := c.Stats()
	if stats.Size != 0 {
		t.Errorf("Size = %d, want 0", stats.Size)
	}
	if stats.Hits != 0 {
		t.Errorf("Hits = %d, want 0", stats.Hits)
	}
	if stats.Misses != 0 {
		t.Errorf("Misses = %d, want 0", stats.Misses)
	}
}

func TestStats_HitsAndMisses(t *testing.T) {
	c := New()
	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})

	c.Set(eh, ch, true, nil)

	// Hit.
	c.GetByKey(eh, ch)

	// Miss.
	c.GetByKey(eh, "wrong_context_hash")

	stats := c.Stats()
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
}

func TestStats_TagCount(t *testing.T) {
	c := New()

	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})

	c.Set(eh, ch, true, []string{"schema:v1", "tier:cel"})

	stats := c.Stats()
	if stats.Tags != 2 {
		t.Errorf("Tags = %d, want 2", stats.Tags)
	}
	if stats.TagCount["schema:v1"] != 1 {
		t.Errorf("TagCount[schema:v1] = %d, want 1", stats.TagCount["schema:v1"])
	}
}

// --- HTTP handler tests ---

func TestInvalidateHandler_ByTag(t *testing.T) {
	c := New()

	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})
	c.Set(eh, ch, true, []string{"schema:v1"})

	body := `{"tag": "schema:v1"}`
	req := httptest.NewRequest("POST", "/v1/admin/cache/invalidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.invalidateHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp invalidateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Invalidated != 1 {
		t.Errorf("Invalidated = %d, want 1", resp.Invalidated)
	}
}

func TestInvalidateHandler_ByTags(t *testing.T) {
	c := New()

	eh0 := HashExpr("x > 0")
	eh1 := HashExpr("y > 0")
	ch := mustHashContext(t, map[string]any{"x": 5, "y": 10})
	c.Set(eh0, ch, true, []string{"tagA"})
	c.Set(eh1, ch, true, []string{"tagB"})

	body := `{"tags": ["tagA", "tagB"]}`
	req := httptest.NewRequest("POST", "/v1/admin/cache/invalidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.invalidateHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp invalidateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Invalidated != 2 {
		t.Errorf("Invalidated = %d, want 2", resp.Invalidated)
	}
}

func TestInvalidateHandler_All(t *testing.T) {
	c := New()

	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})
	c.Set(eh, ch, true, nil)

	body := `{"all": true}`
	req := httptest.NewRequest("POST", "/v1/admin/cache/invalidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.invalidateHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp invalidateResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Invalidated != 1 {
		t.Errorf("Invalidated = %d, want 1", resp.Invalidated)
	}
}

func TestInvalidateHandler_EmptyBody(t *testing.T) {
	c := New()

	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})
	c.Set(eh, ch, true, nil)

	// Empty body should be treated as "invalidate all" per the handler logic.
	req := httptest.NewRequest("POST", "/v1/admin/cache/invalidate", strings.NewReader(""))
	rec := httptest.NewRecorder()

	c.invalidateHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (empty body should invalidate all)", rec.Code, http.StatusOK)
	}
}

func TestInvalidateHandler_NoFields(t *testing.T) {
	c := New()

	// JSON body with no fields set — should return 400.
	body := `{}`
	req := httptest.NewRequest("POST", "/v1/admin/cache/invalidate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	c.invalidateHandler()(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestStatsHandler(t *testing.T) {
	c := New()

	eh := HashExpr("x > 0")
	ch := mustHashContext(t, map[string]any{"x": 5})
	c.Set(eh, ch, true, []string{"schema:v1"})
	c.GetByKey(eh, ch) // hit

	req := httptest.NewRequest("GET", "/v1/admin/cache/stats", nil)
	rec := httptest.NewRecorder()

	c.statsHandler()(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var stats CacheStats
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if stats.Size != 1 {
		t.Errorf("Size = %d, want 1", stats.Size)
	}
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
}

func TestRegisterRoutes(t *testing.T) {
	c := New()
	mux := http.NewServeMux()
	c.RegisterRoutes(mux)

	// Verify routes are mounted by checking the handler exists.
	req := httptest.NewRequest("GET", "/v1/admin/cache/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /v1/admin/cache/stats: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

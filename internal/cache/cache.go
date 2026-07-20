// Package cache provides a multi-tier caching layer for CEL expression
// evaluation results. It implements an in-process LRU cache keyed by
// (expr_hash, context_hash) with TTL-based expiry and tag-based
// invalidation. A POST /v1/admin/cache/invalidate endpoint allows
// operators to purge entries by tag (e.g. when upstream schemas drift).
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// defaultCapacity is the maximum number of entries the LRU cache holds
// before evicting the least-recently-used items.
const defaultCapacity = 10000

// defaultTTL is the default time-to-live for cached entries.
const defaultTTL = 5 * time.Minute

// cacheKey uniquely identifies a cached evaluation result.
type cacheKey struct {
	// ExprHash is the SHA-256 hex digest of the CEL expression string.
	ExprHash string `json:"expr_hash"`

	// ContextHash is the SHA-256 hex digest of the JSON-serialised context
	// map, ensuring cache keys are sensitive to variable bindings.
	ContextHash string `json:"context_hash"`
}

// cacheEntry stores a cached evaluation result along with its metadata.
type cacheEntry struct {
	// Value is the cached evaluation result (any JSON-compatible value).
	Value any `json:"value"`

	// Tags are opaque strings associated with this entry. When a tag is
	// invalidated all entries sharing that tag are evicted.
	Tags []string `json:"tags"`

	// ExpiresAt is the absolute time at which this entry expires.
	ExpiresAt time.Time `json:"expires_at"`

	// LastAccess tracks when this entry was last read or written, for LRU
	// eviction ordering.
	LastAccess time.Time `json:"last_access"`
}

// Cache is a concurrent-safe LRU cache with TTL-based expiry and
// tag-based invalidation. Zero-value is unusable; use New to construct.
type Cache struct {
	mu sync.RWMutex

	// entries stores cached results keyed by (expr_hash, context_hash).
	entries map[cacheKey]*cacheEntry

	// accessOrder tracks keys in LRU order (most recent at tail). A linked
	// list is simulated with an ordered slice for simplicity; for the
	// default capacity of 10k this is adequate.
	accessOrder []cacheKey

	// tagIndex maps tags to the set of keys that carry that tag, for fast
	// invalidation.
	tagIndex map[string]map[cacheKey]struct{}

	// capacity is the maximum number of live entries.
	capacity int

	// ttl is the default time-to-live for new entries.
	ttl time.Duration

	// metrics counters (atomic for lock-free reads).
	hits   uint64
	misses uint64
	evicts uint64
}

// Option configures a Cache during construction.
type Option func(*Cache)

// WithCapacity sets the maximum number of entries the cache retains before
// LRU eviction. Defaults to 10000.
func WithCapacity(n int) Option {
	return func(c *Cache) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// WithTTL sets the default time-to-live for cached entries. Defaults to
// 5 minutes.
func WithTTL(d time.Duration) Option {
	return func(c *Cache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// New creates a new Cache with the given options.
func New(opts ...Option) *Cache {
	c := &Cache{
		entries:     make(map[cacheKey]*cacheEntry),
		accessOrder: make([]cacheKey, 0, defaultCapacity),
		tagIndex:    make(map[string]map[cacheKey]struct{}),
		capacity:    defaultCapacity,
		ttl:         defaultTTL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// HashExpr computes a SHA-256 hex digest of the expression string.
func HashExpr(expr string) string {
	h := sha256.Sum256([]byte(expr))
	return hex.EncodeToString(h[:])
}

// HashContext computes a SHA-256 hex digest of the JSON-serialised context
// map. The map is sorted by key before encoding to ensure deterministic
// hashes for equivalent contexts. Returns an error if the context cannot be
// serialised to JSON.
func HashContext(ctx map[string]any) (string, error) {
	// Normalise by sorting keys for deterministic serialisation.
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	// Simple insertion sort — context maps are typically small.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	m := make(map[string]any, len(keys))
	for _, k := range keys {
		m[k] = ctx[k]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("cache: cannot marshal context to JSON: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Get retrieves a cached evaluation result for the given expression and
// context. It hashes the inputs identically to how Set expects them, so
// values stored via Set can be retrieved via Get. Returns (value, true) on
// a cache hit, or (nil, false) on a miss or expired entry. Returns an error
// if the context cannot be serialised for hashing.
func (c *Cache) Get(expr string, ctx map[string]any) (any, bool, error) {
	ctxHash, err := HashContext(ctx)
	if err != nil {
		return nil, false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey{
		ExprHash:    HashExpr(expr),
		ContextHash: ctxHash,
	}

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		if ok {
			// Entry expired — remove it.
			c.removeEntryLocked(key)
		}
		atomic.AddUint64(&c.misses, 1)
		return nil, false, nil
	}

	// Cache hit — promote to most-recently-used.
	entry.LastAccess = time.Now()
	c.promoteLocked(key)
	atomic.AddUint64(&c.hits, 1)
	return entry.Value, true, nil
}

// GetByKey retrieves a cached evaluation result using pre-computed hashes.
// This is useful when the caller has already computed expression and
// context hashes.
func (c *Cache) GetByKey(exprHash, contextHash string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cacheKey{
		ExprHash:    exprHash,
		ContextHash: contextHash,
	}

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.ExpiresAt) {
		if ok {
			c.removeEntryLocked(key)
		}
		atomic.AddUint64(&c.misses, 1)
		return nil, false
	}

	entry.LastAccess = time.Now()
	c.promoteLocked(key)
	atomic.AddUint64(&c.hits, 1)
	return entry.Value, true
}

// Set stores a cached evaluation result for the given expression and
// context hashes. Tags can be used for later bulk invalidation.
func (c *Cache) Set(exprHash, contextHash string, value any, tags []string, ttlOverride ...time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ttl := c.ttl
	if len(ttlOverride) > 0 && ttlOverride[0] > 0 {
		ttl = ttlOverride[0]
	}

	key := cacheKey{
		ExprHash:    exprHash,
		ContextHash: contextHash,
	}

	// If key already exists, remove old entry first to clean up tag index.
	if _, ok := c.entries[key]; ok {
		c.removeEntryLocked(key)
	}

	entry := &cacheEntry{
		Value:      value,
		Tags:       tags,
		ExpiresAt:  time.Now().Add(ttl),
		LastAccess: time.Now(),
	}
	c.entries[key] = entry
	c.accessOrder = append(c.accessOrder, key)

	// Update tag index.
	for _, tag := range tags {
		if c.tagIndex[tag] == nil {
			c.tagIndex[tag] = make(map[cacheKey]struct{})
		}
		c.tagIndex[tag][key] = struct{}{}
	}

	// Evict LRU entries if over capacity.
	c.evictLocked()
}

// InvalidateByTag removes all cache entries carrying the given tag. This
// is the primary mechanism for schema-drift invalidation: when upstream
// schemas change, operators call this with the corresponding tag.
func (c *Cache) InvalidateByTag(tag string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	keys, ok := c.tagIndex[tag]
	if !ok {
		return 0
	}

	count := 0
	for key := range keys {
		c.removeEntryLocked(key)
		count++
	}
	return count
}

// InvalidateAll removes all entries from the cache.
func (c *Cache) InvalidateAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := len(c.entries)
	c.entries = make(map[cacheKey]*cacheEntry)
	c.accessOrder = c.accessOrder[:0]
	c.tagIndex = make(map[string]map[cacheKey]struct{})
	return count
}

// Stats returns a point-in-time snapshot of cache metrics.
func (c *Cache) Stats() CacheStats {
	return CacheStats{
		Size:        len(c.entries),
		Capacity:    c.capacity,
		Hits:        atomic.LoadUint64(&c.hits),
		Misses:      atomic.LoadUint64(&c.misses),
		Evictions:   atomic.LoadUint64(&c.evicts),
		Tags:        len(c.tagIndex),
		DefaultTTL:  c.ttl.String(),
		TagCount:    c.tagCounts(),
	}
}

// CacheStats holds a point-in-time snapshot of the cache state.
type CacheStats struct {
	// Size is the current number of live entries.
	Size int `json:"size"`

	// Capacity is the maximum number of entries before LRU eviction.
	Capacity int `json:"capacity"`

	// Hits is the cumulative number of cache hits.
	Hits uint64 `json:"hits"`

	// Misses is the cumulative number of cache misses (including TTL expiries).
	Misses uint64 `json:"misses"`

	// Evictions is the cumulative number of LRU evictions.
	Evictions uint64 `json:"evictions"`

	// Tags is the number of distinct tags in the tag index.
	Tags int `json:"tags"`

	// DefaultTTL is the configured default time-to-live as a Go duration
	// string (e.g. "5m0s").
	DefaultTTL string `json:"default_ttl"`

	// TagCount breaks down the number of entries per tag.
	TagCount map[string]int `json:"tag_count"`
}

// tagCounts returns a map of tag → entry count for the current tag index.
func (c *Cache) tagCounts() map[string]int {
	m := make(map[string]int, len(c.tagIndex))
	for tag, keys := range c.tagIndex {
		m[tag] = len(keys)
	}
	return m
}

// --- Internal helpers (must be called with c.mu held) ---

// promoteLocked moves key to the tail of the access-order slice (most
// recently used position).
func (c *Cache) promoteLocked(key cacheKey) {
	for i, k := range c.accessOrder {
		if k == key {
			// Remove from current position.
			c.accessOrder = append(c.accessOrder[:i], c.accessOrder[i+1:]...)
			break
		}
	}
	c.accessOrder = append(c.accessOrder, key)
}

// removeEntryLocked removes a single entry and updates all indexes.
func (c *Cache) removeEntryLocked(key cacheKey) {
	entry, ok := c.entries[key]
	if !ok {
		return
	}
	// Remove from tag index.
	for _, tag := range entry.Tags {
		delete(c.tagIndex[tag], key)
		if len(c.tagIndex[tag]) == 0 {
			delete(c.tagIndex, tag)
		}
	}
	delete(c.entries, key)
	// Remove from access order.
	for i, k := range c.accessOrder {
		if k == key {
			c.accessOrder = append(c.accessOrder[:i], c.accessOrder[i+1:]...)
			break
		}
	}
}

// evictLocked removes expired and LRU entries until size <= capacity.
func (c *Cache) evictLocked() {
	now := time.Now()

	// First pass: remove all expired entries.
	var fresh []cacheKey
	for _, key := range c.accessOrder {
		entry := c.entries[key]
		if now.After(entry.ExpiresAt) {
			c.removeEntryLocked(key)
			atomic.AddUint64(&c.evicts, 1)
		} else {
			fresh = append(fresh, key)
		}
	}
	c.accessOrder = fresh

	// Second pass: evict LRU entries if still over capacity.
	for len(c.entries) > c.capacity && len(c.accessOrder) > 0 {
		key := c.accessOrder[0] // oldest (least recently used)
		c.removeEntryLocked(key)
		c.accessOrder = c.accessOrder[1:]
		atomic.AddUint64(&c.evicts, 1)
	}
}

// --- HTTP handlers ---

// RegisterRoutes registers the cache admin endpoints on the given ServeMux.
// It mounts:
//
//	POST /v1/admin/cache/invalidate — tag-based or full cache invalidation
//	GET  /v1/admin/cache/stats        — cache statistics
func (c *Cache) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/admin/cache/invalidate", c.invalidateHandler())
	mux.Handle("GET /v1/admin/cache/stats", c.statsHandler())
}

// invalidateRequest is the JSON request body for POST /v1/admin/cache/invalidate.
type invalidateRequest struct {
	// Tag specifies a single tag to invalidate. All entries carrying this
	// tag are evicted. If empty and All is true, the entire cache is purged.
	Tag string `json:"tag"`

	// Tags specifies multiple tags to invalidate in a single call.
	Tags []string `json:"tags"`

	// All, when true, invalidates all entries regardless of tags.
	All bool `json:"all"`
}

// invalidateResponse is the JSON response body for the invalidate endpoint.
type invalidateResponse struct {
	Invalidated int `json:"invalidated"`
	Message     string `json:"message"`
}

// invalidateHandler handles POST /v1/admin/cache/invalidate. It accepts a
// JSON body with optional tag/tags/all fields and evicts matching entries.
func (c *Cache) invalidateHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req invalidateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Allow empty body — treat as "invalidate all".
			req.All = true
		}

		count := 0
		if req.All {
			count = c.InvalidateAll()
		} else if len(req.Tags) > 0 {
			for _, tag := range req.Tags {
				count += c.InvalidateByTag(tag)
			}
		} else if req.Tag != "" {
			count = c.InvalidateByTag(req.Tag)
		} else {
			http.Error(w, `must specify "tag", "tags", or "all"`, http.StatusBadRequest)
			return
		}

		resp := invalidateResponse{
			Invalidated: count,
			Message:     fmt.Sprintf("invalidated %d cache entries", count),
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// statsHandler handles GET /v1/admin/cache/stats. It returns a JSON snapshot
// of the cache state.
func (c *Cache) statsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := c.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

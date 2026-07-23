package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WasmAgent/symkernel/internal/cache"
)

// defaultTTL is the time-to-live applied to new entries when no override
// is supplied. It matches the in-process cache default for consistency.
const defaultTTL = 5 * time.Minute

// defaultPrefix namespaces symkernel keys in a shared backend so they do
// not collide with other services pointing at the same Redis instance.
const defaultPrefix = "symkernel:v1:"

// Cache is a distributed, backend-backed verification-result cache. It
// derives keys identically to the in-process cache (expr hash + context
// hash) so the two tiers can share a key space, applies a TTL on writes,
// supports tag-based invalidation and cache warming, and exposes hit/miss
// telemetry. The zero value is unusable; use New.
type Cache struct {
	backend Backend
	ttl     time.Duration
	prefix  string

	// tagMu serializes the read-modify-write on the per-tag member index
	// stored in the backend, so concurrent Set calls for one tag cannot
	// lose entries within a single process.
	tagMu sync.Mutex

	// Telemetry counters (atomic for lock-free reads on the hot path).
	hits      uint64
	misses    uint64
	sets      uint64
	evictions uint64
	errors    uint64
}

// Option configures a Cache during construction.
type Option func(*Cache)

// WithBackend installs a specific Backend. Defaults to a MemoryBackend.
func WithBackend(b Backend) Option {
	return func(c *Cache) { c.backend = b }
}

// WithTTL sets the default time-to-live for new entries.
func WithTTL(d time.Duration) Option {
	return func(c *Cache) {
		if d > 0 {
			c.ttl = d
		}
	}
}

// WithPrefix sets the key prefix used to namespace entries.
func WithPrefix(p string) Option {
	return func(c *Cache) {
		if p != "" {
			c.prefix = p
		}
	}
}

// New creates a Cache with the given options, defaulting to an in-memory
// backend, a 5-minute TTL, and the symkernel key prefix.
func New(opts ...Option) *Cache {
	c := &Cache{
		ttl:    defaultTTL,
		prefix: defaultPrefix,
	}
	for _, o := range opts {
		o(c)
	}
	if c.backend == nil {
		c.backend = NewMemoryBackend()
	}
	return c
}

// WarmEntry is a single precomputed result used to warm the cache for a
// common policy pattern (e.g. the CEL expressions exercised most often by
// CI pipelines).
type WarmEntry struct {
	Expr    string         `json:"expr"`
	Context map[string]any `json:"context"`
	Value   any            `json:"value"`
	Tags    []string       `json:"tags,omitempty"`
}

// dataKey returns the backend key for a cached result given its hashes.
func (c *Cache) dataKey(exprHash, ctxHash string) string {
	return c.prefix + exprHash + ":" + ctxHash
}

// tagKey returns the backend key holding the JSON member list for a tag.
func (c *Cache) tagKey(tag string) string {
	return c.prefix + "::tag::" + tag
}

// Get retrieves a cached result for the expression and context. It hashes
// the inputs identically to Set, so a value written via Set is retrievable
// here. Returns (value, true, nil) on a hit and (nil, false, nil) on a
// miss. Returns an error only if hashing or the backend fails.
func (c *Cache) Get(expr string, ctx map[string]any) (any, bool, error) {
	ctxHash, err := cache.HashContext(ctx)
	if err != nil {
		atomic.AddUint64(&c.errors, 1)
		return nil, false, err
	}
	return c.GetByKey(cache.HashExpr(expr), ctxHash)
}

// GetByKey is Get for callers that have precomputed the hashes.
func (c *Cache) GetByKey(exprHash, ctxHash string) (any, bool, error) {
	key := c.dataKey(exprHash, ctxHash)
	b, err := c.backend.Get(context.Background(), key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			atomic.AddUint64(&c.misses, 1)
			return nil, false, nil
		}
		atomic.AddUint64(&c.errors, 1)
		return nil, false, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		atomic.AddUint64(&c.errors, 1)
		return nil, false, fmt.Errorf("distributed cache: decode value: %w", err)
	}
	atomic.AddUint64(&c.hits, 1)
	return v, true, nil
}

// Set stores value for the expression and context with the default TTL
// (or ttlOverride if supplied and positive). Tags associate the entry
// with one or more tags for later bulk invalidation.
func (c *Cache) Set(expr string, ctx map[string]any, value any, tags []string, ttlOverride ...time.Duration) error {
	ctxHash, err := cache.HashContext(ctx)
	if err != nil {
		atomic.AddUint64(&c.errors, 1)
		return err
	}
	return c.SetByKey(cache.HashExpr(expr), ctxHash, value, tags, ttlOverride...)
}

// SetByKey is Set for callers that have precomputed the hashes.
func (c *Cache) SetByKey(exprHash, ctxHash string, value any, tags []string, ttlOverride ...time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		atomic.AddUint64(&c.errors, 1)
		return fmt.Errorf("distributed cache: encode value: %w", err)
	}

	ttl := c.ttl
	if len(ttlOverride) > 0 && ttlOverride[0] > 0 {
		ttl = ttlOverride[0]
	}

	key := c.dataKey(exprHash, ctxHash)
	if err := c.backend.Set(context.Background(), key, data, ttl); err != nil {
		atomic.AddUint64(&c.errors, 1)
		return err
	}
	atomic.AddUint64(&c.sets, 1)

	for _, tag := range tags {
		if err := c.addTagKey(tag, key); err != nil {
			// Tag indexing is best-effort: the cached result itself is
			// stored, it just will not be reachable via this tag.
			atomic.AddUint64(&c.errors, 1)
		}
	}
	return nil
}

// Delete removes a single entry identified by its hashes.
func (c *Cache) Delete(exprHash, ctxHash string) error {
	n, err := c.backend.Del(context.Background(), c.dataKey(exprHash, ctxHash))
	if err != nil {
		atomic.AddUint64(&c.errors, 1)
		return err
	}
	if n > 0 {
		atomic.AddUint64(&c.evictions, uint64(n))
	}
	return nil
}

// InvalidateByTag removes every entry associated with tag and returns the
// number of entries invalidated. It reads the tag's member list from the
// backend, bulk-deletes the members plus the tag index key, and is safe
// to call with an unknown tag (returns 0).
func (c *Cache) InvalidateByTag(tag string) (int, error) {
	c.tagMu.Lock()
	defer c.tagMu.Unlock()

	tk := c.tagKey(tag)
	b, err := c.backend.Get(context.Background(), tk)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		atomic.AddUint64(&c.errors, 1)
		return 0, err
	}

	var members []string
	if err := json.Unmarshal(b, &members); err != nil {
		atomic.AddUint64(&c.errors, 1)
		return 0, fmt.Errorf("distributed cache: decode tag index: %w", err)
	}
	if len(members) == 0 {
		return 0, nil
	}

	keys := append(members, tk)
	if _, err := c.backend.Del(context.Background(), keys...); err != nil {
		atomic.AddUint64(&c.errors, 1)
		return 0, err
	}
	atomic.AddUint64(&c.evictions, uint64(len(members)))
	return len(members), nil
}

// addTagKey appends key to the member list of tag, creating the index
// entry if needed. The read-modify-write is serialized per process by
// tagMu so concurrent writers within this process do not lose entries.
func (c *Cache) addTagKey(tag, key string) error {
	c.tagMu.Lock()
	defer c.tagMu.Unlock()

	tk := c.tagKey(tag)
	var members []string
	if b, err := c.backend.Get(context.Background(), tk); err == nil {
		_ = json.Unmarshal(b, &members) // a corrupt index is treated as empty
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}

	for _, m := range members {
		if m == key {
			return nil // already indexed
		}
	}
	members = append(members, key)
	data, err := json.Marshal(members)
	if err != nil {
		return err
	}
	// The tag index is persistent (ttl=0); it outlives the entries it
	// references, which is fine because Del of a stale member is a no-op.
	return c.backend.Set(context.Background(), tk, data, 0)
}

// Warm pre-populates the cache with the given entries (e.g. the most
// common CEL/Criterion policy patterns), returning the number written.
// It stops and returns the first error encountered.
func (c *Cache) Warm(_ context.Context, entries []WarmEntry) (int, error) {
	n := 0
	for _, e := range entries {
		if err := c.Set(e.Expr, e.Context, e.Value, e.Tags); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Ping verifies the backend is reachable.
func (c *Cache) Ping() error {
	if err := c.backend.Ping(context.Background()); err != nil {
		atomic.AddUint64(&c.errors, 1)
		return err
	}
	return nil
}

// Close releases any backend resources (e.g. the Redis connection). For
// backends with nothing to release it is a no-op.
func (c *Cache) Close() error {
	if closer, ok := c.backend.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// Stats is a point-in-time snapshot of distributed-cache telemetry.
type Stats struct {
	Backend    string  `json:"backend"`
	Prefix     string  `json:"prefix"`
	DefaultTTL string  `json:"default_ttl"`
	Keys       int     `json:"keys"`
	Hits       uint64  `json:"hits"`
	Misses     uint64  `json:"misses"`
	Sets       uint64  `json:"sets"`
	Evictions  uint64  `json:"evictions"`
	Errors     uint64  `json:"errors"`
	HitRatio   float64 `json:"hit_ratio"`
}

// Stats returns a point-in-time snapshot of the cache state and telemetry.
// HitRatio is hits / (hits + misses), or zero before any lookups.
func (c *Cache) Stats() Stats {
	hits := atomic.LoadUint64(&c.hits)
	misses := atomic.LoadUint64(&c.misses)

	st := Stats{
		Prefix:     c.prefix,
		DefaultTTL: c.ttl.String(),
		Keys:       0,
		Hits:       hits,
		Misses:     misses,
		Sets:       atomic.LoadUint64(&c.sets),
		Evictions:  atomic.LoadUint64(&c.evictions),
		Errors:     atomic.LoadUint64(&c.errors),
	}
	if total := hits + misses; total > 0 {
		st.HitRatio = float64(hits) / float64(total)
	}
	if n, ok := c.backend.(namedBackend); ok {
		st.Backend = n.Name()
	}
	if s, ok := c.backend.(sizer); ok {
		st.Keys = s.Len()
	}
	return st
}

// RegisterRoutes mounts the distributed-cache admin endpoint on mux:
//
//	GET /v1/admin/cache/distributed/stats — cache telemetry
func (c *Cache) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/admin/cache/distributed/stats", c.statsHandler())
}

// statsHandler handles GET /v1/admin/cache/distributed/stats.
func (c *Cache) statsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats := c.Stats()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stats) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

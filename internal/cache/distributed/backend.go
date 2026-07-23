// Package distributed implements a distributed verification-result cache
// backed by a remote key-value store, the cache tier called out by
// Milestone 10. It mirrors the in-process cache in internal/cache but
// stores entries in a shared backend so that every node in a symkernel
// cluster observes the same warmed results.
//
// The remote store is reached through the Backend interface, which is
// the minimal key-value contract the cache needs: Get, Set (with a
// time-to-live), Del, and Ping. Two implementations ship with this
// package:
//
//   - MemoryBackend: an in-process map with TTL expiry. It is the
//     default and the one exercised by tests; it is also a reasonable
//     choice for single-node deployments.
//   - RedisBackend: speaks the Redis Serialization Protocol (RESP) over
//     a plain TCP connection using only the standard library, so the
//     cache can front a real Redis (or Redis-compatible) instance without
//     pulling in a third-party client. The roadmap's "Redis" is the
//     production backend; the interface is the seam that lets operators
//     swap in any Redis-compatible service.
//
// Tag-based invalidation is layered on top of the Backend: each tag maps
// to a JSON list of member keys stored under a reserved key, so
// InvalidateByTag fans out to a bulk Del. Within a single process the
// read-modify-write on the tag index is serialized; across processes the
// bulk Del is the source of truth and stale index entries are no-ops.
package distributed

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned by Backend.Get when a key is absent (or has
// expired). Callers should treat it as a cache miss, not a hard error.
var ErrNotFound = errors.New("distributed cache: key not found")

// Backend is the minimal key-value contract the distributed cache needs.
// Implementations must be safe for concurrent use.
type Backend interface {
	// Get returns the bytes stored under key. It returns ErrNotFound
	// (wrapping is permitted via errors.Is) when the key is absent.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores value under key with the given time-to-live. A ttl of
	// zero means the entry does not expire.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Del removes the given keys and returns the number that existed.
	Del(ctx context.Context, keys ...string) (int, error)

	// Ping verifies that the backend is reachable. It should be cheap.
	Ping(ctx context.Context) error
}

// namedBackend optionally names the backend for telemetry. Both shipped
// backends implement it; the Stats endpoint reports the name so operators
// can tell a memory backend from a Redis one.
type namedBackend interface {
	Name() string
}

// sizer optionally reports the number of live entries. MemoryBackend
// implements it; the Stats endpoint falls back to zero when the backend
// cannot answer cheaply (e.g. Redis without a DBSIZE round-trip).
type sizer interface {
	Len() int
}

// --- MemoryBackend ---

// memEntry is a single in-memory cache entry.
type memEntry struct {
	value     []byte
	expiresAt time.Time // zero ⇒ never expires
}

// MemoryBackend is an in-process Backend backed by a map. It applies
// time-to-live on read and is safe for concurrent use. It is the default
// backend and the one used by tests; it is also useful for single-node
// deployments that want the distributed-cache API without an external
// dependency.
type MemoryBackend struct {
	mu      sync.Mutex
	entries map[string]memEntry
	now     func() time.Time
}

// NewMemoryBackend returns an empty MemoryBackend using the system clock.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{
		entries: make(map[string]memEntry),
		now:     time.Now,
	}
}

// newMemoryBackendWithClock is the test constructor that injects a clock,
// so TTL expiry can be exercised deterministically without sleeping.
func newMemoryBackendWithClock(now func() time.Time) *MemoryBackend {
	return &MemoryBackend{
		entries: make(map[string]memEntry),
		now:     now,
	}
}

// Name reports the backend name for telemetry.
func (m *MemoryBackend) Name() string { return "memory" }

// Len returns the number of live entries, sweeping expired ones first.
func (m *MemoryBackend) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepLocked()
	return len(m.entries)
}

// Get returns the value for key, or ErrNotFound if it is absent or expired.
func (m *MemoryBackend) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.entries[key]
	if !ok {
		return nil, ErrNotFound
	}
	if !e.expiresAt.IsZero() && m.now().After(e.expiresAt) {
		delete(m.entries, key)
		return nil, ErrNotFound
	}
	// Copy so callers cannot mutate stored state through the slice header.
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

// Set stores value under key with the given ttl (zero ⇒ no expiry).
func (m *MemoryBackend) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	// Copy so a later caller mutation does not corrupt the stored entry.
	stored := make([]byte, len(value))
	copy(stored, value)

	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = m.now().Add(ttl)
	}

	m.mu.Lock()
	m.entries[key] = memEntry{value: stored, expiresAt: expiresAt}
	m.mu.Unlock()
	return nil
}

// Del removes the given keys and returns the number that existed.
func (m *MemoryBackend) Del(_ context.Context, keys ...string) (int, error) {
	n := 0
	m.mu.Lock()
	for _, k := range keys {
		if _, ok := m.entries[k]; ok {
			delete(m.entries, k)
			n++
		}
	}
	m.mu.Unlock()
	return n, nil
}

// Ping always succeeds for the in-memory backend.
func (m *MemoryBackend) Ping(_ context.Context) error { return nil }

// sweepLocked removes all expired entries. Caller must hold m.mu.
func (m *MemoryBackend) sweepLocked() {
	if len(m.entries) == 0 {
		return
	}
	now := m.now()
	for k, e := range m.entries {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(m.entries, k)
		}
	}
}

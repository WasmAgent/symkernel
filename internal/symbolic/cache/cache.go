// Package cache provides a bounded LRU cache for completed Z3 decisions.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	// EnvMaxEntries configures the maximum number of Z3 decisions retained.
	// A value of zero disables caching; invalid values use DefaultMaxEntries.
	EnvMaxEntries = "SYMKERNEL_Z3_CACHE_SIZE"

	// DefaultMaxEntries bounds memory use when EnvMaxEntries is not set.
	DefaultMaxEntries = 1_000
)

// Decision is a completed Z3 query result. Errors and timeout-driven unknown
// results are intentionally not cached by callers.
type Decision struct {
	Sat       string
	Model     map[string]any
	UnsatCore []string
}

// Stats is a snapshot of cache activity.
type Stats struct {
	Entries    int
	MaxEntries int
	Hits       uint64
	Misses     uint64
	Evictions  uint64
}

type entry struct {
	hash     string
	decision Decision
}

// Cache is a concurrency-safe LRU cache keyed by an assertion hash.
type Cache struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]*list.Element
	lru        *list.List
	hits       uint64
	misses     uint64
	evictions  uint64
}

// New creates a decision cache with maxEntries entries. A negative size uses
// DefaultMaxEntries; zero disables caching.
func New(maxEntries int) *Cache {
	if maxEntries < 0 {
		maxEntries = DefaultMaxEntries
	}
	return &Cache{
		maxEntries: maxEntries,
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
	}
}

// NewFromEnv creates a cache configured with SYMKERNEL_Z3_CACHE_SIZE.
func NewFromEnv() *Cache {
	maxEntries := DefaultMaxEntries
	if raw, ok := os.LookupEnv(EnvMaxEntries); ok {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			maxEntries = parsed
		}
	}
	return New(maxEntries)
}

// HashAssertions returns the stable key for an SMT-LIB assertion program.
func HashAssertions(assertions string) string {
	digest := sha256.Sum256([]byte(assertions))
	return hex.EncodeToString(digest[:])
}

// Get retrieves a completed decision and promotes it to most recently used.
func (c *Cache) Get(assertions string) (Decision, bool) {
	return c.GetByHash(HashAssertions(assertions))
}

// GetByHash retrieves a completed decision by its precomputed assertion hash.
func (c *Cache) GetByHash(hash string) (Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, ok := c.entries[hash]
	if !ok {
		atomic.AddUint64(&c.misses, 1)
		return Decision{}, false
	}
	c.lru.MoveToFront(element)
	atomic.AddUint64(&c.hits, 1)
	return cloneDecision(element.Value.(entry).decision), true
}

// Set stores a completed decision, evicting the least recently used entry
// when the configured capacity is reached.
func (c *Cache) Set(assertions string, decision Decision) {
	c.SetByHash(HashAssertions(assertions), decision)
}

// SetByHash stores a completed decision under a precomputed assertion hash.
func (c *Cache) SetByHash(hash string, decision Decision) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxEntries == 0 {
		return
	}
	if element, ok := c.entries[hash]; ok {
		element.Value = entry{hash: hash, decision: cloneDecision(decision)}
		c.lru.MoveToFront(element)
		return
	}

	c.entries[hash] = c.lru.PushFront(entry{hash: hash, decision: cloneDecision(decision)})
	if c.lru.Len() <= c.maxEntries {
		return
	}
	oldest := c.lru.Back()
	oldEntry := oldest.Value.(entry)
	delete(c.entries, oldEntry.hash)
	c.lru.Remove(oldest)
	atomic.AddUint64(&c.evictions, 1)
}

// Stats returns a point-in-time cache snapshot.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	entries := len(c.entries)
	maxEntries := c.maxEntries
	c.mu.Unlock()
	return Stats{
		Entries:    entries,
		MaxEntries: maxEntries,
		Hits:       atomic.LoadUint64(&c.hits),
		Misses:     atomic.LoadUint64(&c.misses),
		Evictions:  atomic.LoadUint64(&c.evictions),
	}
}

func cloneDecision(decision Decision) Decision {
	copy := Decision{
		Sat:       decision.Sat,
		UnsatCore: append([]string(nil), decision.UnsatCore...),
	}
	if decision.Model != nil {
		copy.Model = make(map[string]any, len(decision.Model))
		for key, value := range decision.Model {
			copy.Model[key] = value
		}
	}
	return copy
}

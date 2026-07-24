// Package sharding implements a consistent-hashing ring for distributing
// verification work across symkernel cluster nodes, the sharding primitive
// called out by Milestone 10.
//
// The ring maps string keys (typically verification request identifiers) to
// node IDs using virtual-node hashing: each physical node owns N virtual
// nodes spread around a 2^32 hash circle so that the distribution remains
// uniform even with a small number of members. When a node joins or leaves
// only its own keys migrate — the ring's primary design goal is minimal
// rebalancing.
//
// The package is organised into three concerns:
//
//   - Ring (ring.go): the hash ring itself — Lookup, Members, and the
//     join/leave mutations that recalculate virtual-node ownership.
//   - NodeManager (node_manager.go): higher-level node lifecycle that wraps
//     the ring, tracks health, and exposes the membership set.
//   - Metrics (metrics.go): per-shard counters (request count, error rate,
//     latency) that inform load-balancing decisions.
package sharding

import (
	"sort"
	"strconv"
	"sync"

	"github.com/cespare/xxhash/v2"
)

const (
	// defaultVirtualNodes is the number of virtual nodes each physical
	// node places on the ring. 150 is the conventional default for
	// consistent hashing with SHA-family hashes.
	defaultVirtualNodes = 150
)

// virtualNode represents one point on the hash circle.
type virtualNode struct {
	hash   uint64
	nodeID string
}

// Ring is a consistent-hashing ring that maps opaque keys to cluster nodes.
// It is safe for concurrent use.
type Ring struct {
	mu       sync.RWMutex
	virtuals []virtualNode // sorted by hash
	nodes    map[string]struct{}

	vnodesPerNode int
}

// NewRing creates an empty ring. Use AddNode to populate it.
func NewRing(opts ...RingOption) *Ring {
	r := &Ring{
		nodes:         make(map[string]struct{}),
		vnodesPerNode: defaultVirtualNodes,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// RingOption configures a Ring on construction.
type RingOption func(*Ring)

// WithVirtualNodes sets the number of virtual nodes per physical node.
func WithVirtualNodes(n int) RingOption {
	return func(r *Ring) {
		if n > 0 {
			r.vnodesPerNode = n
		}
	}
}

// AddNode inserts nodeID into the ring, creating virtual nodes at
// deterministic positions derived from the node ID plus a replica index.
func (r *Ring) AddNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addNodeLocked(nodeID)
}

func (r *Ring) addNodeLocked(nodeID string) {
	if _, exists := r.nodes[nodeID]; exists {
		return
	}
	r.nodes[nodeID] = struct{}{}
	for i := 0; i < r.vnodesPerNode; i++ {
		h := hashKey(nodeID + "#" + strconv.Itoa(i))
		r.virtuals = append(r.virtuals, virtualNode{hash: h, nodeID: nodeID})
	}
	sort.Slice(r.virtuals, func(i, j int) bool {
		return r.virtuals[i].hash < r.virtuals[j].hash
	})
}

// RemoveNode removes nodeID and all its virtual nodes from the ring.
func (r *Ring) RemoveNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeNodeLocked(nodeID)
}

func (r *Ring) removeNodeLocked(nodeID string) {
	if _, exists := r.nodes[nodeID]; !exists {
		return
	}
	delete(r.nodes, nodeID)
	filtered := r.virtuals[:0]
	for _, vn := range r.virtuals {
		if vn.nodeID != nodeID {
			filtered = append(filtered, vn)
		}
	}
	r.virtuals = filtered
}

// Lookup returns the node ID responsible for key. If the ring is empty it
// returns false.
func (r *Ring) Lookup(key string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lookupLocked(key)
}

func (r *Ring) lookupLocked(key string) (string, bool) {
	if len(r.virtuals) == 0 {
		return "", false
	}
	h := hashKey(key)
	// Binary search for the first virtual node with hash >= h.
	idx := sort.Search(len(r.virtuals), func(i int) bool {
		return r.virtuals[i].hash >= h
	})
	// Wrap around to the first node when past the end.
	if idx >= len(r.virtuals) {
		idx = 0
	}
	return r.virtuals[idx].nodeID, true
}

// Members returns the current set of physical node IDs.
func (r *Ring) Members() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for n := range r.nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Size returns the number of physical nodes on the ring.
func (r *Ring) Size() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.nodes)
}

// NodeCount returns the total number of virtual nodes on the ring.
func (r *Ring) NodeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.virtuals)
}

// MigrationKeys returns the set of keys that would change owner when
// fromNode is removed. It does NOT mutate the ring; the caller can
// compare the before/after owners to decide whether to drain or let
// keys expire naturally.
func (r *Ring) MigrationKeys(keys []string, fromNode string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Simulate the ring without fromNode.
	sim := &Ring{
		virtuals:      make([]virtualNode, 0, len(r.virtuals)),
		nodes:          make(map[string]struct{}, len(r.nodes)),
		vnodesPerNode:  r.vnodesPerNode,
	}
	for n := range r.nodes {
		if n == fromNode {
			continue
		}
		sim.nodes[n] = struct{}{}
	}
	for _, vn := range r.virtuals {
		if vn.nodeID != fromNode {
			sim.virtuals = append(sim.virtuals, vn)
		}
	}

	migrations := make(map[string]string)
	for _, k := range keys {
		old, _ := r.lookupLocked(k)
		new, ok := sim.lookupLocked(k)
		if ok && old != new && old == fromNode {
			migrations[k] = new
		}
	}
	return migrations
}

// hashKey computes a uint64 xxhash of the key. xxhash provides
// excellent distribution for short strings, which is critical for
// consistent-hashing rings where node names are typically short.
func hashKey(key string) uint64 {
	return xxhash.Sum64String(key)
}



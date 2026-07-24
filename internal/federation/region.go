package federation

import (
	"sort"
	"sync"
	"time"
)

// Region is a deployment region participating in federation.
type Region struct {
	// ID is the stable region identifier (e.g. "us-east-1").
	ID string `json:"id"`

	// Endpoint is the address of the region's symkernel cluster, used by a
	// real Transport to reach peer stores. Opaque to the registry.
	Endpoint string `json:"endpoint,omitempty"`

	// Priority is a static routing preference; lower is preferred. It breaks
	// ties when two regions report similar latency and lets operators pin a
	// primary region for cost or data-residency reasons.
	Priority int `json:"priority"`

	// Latency is the most recently observed round-trip time to the region,
	// the signal region-aware routing optimizes for. Zero means "unknown",
	// which ranks after all known latencies.
	Latency time.Duration `json:"latency"`

	// Healthy gates whether the region receives routed traffic and automatic
	// replication. A region marked unhealthy is skipped by the Router
	// (disaster-recovery failover) and by the auto-discovery replication
	// path, so a down region neither serves requests nor blocks convergence.
	Healthy bool `json:"healthy"`
}

// RegionRegistry tracks the regions known to this federation member, their
// observed latency, and health. It is safe for concurrent use.
type RegionRegistry struct {
	mu      sync.RWMutex
	self    string
	regions map[string]*Region
}

// NewRegionRegistry returns a registry seeded with the local region. self
// identifies the region this process runs in; if non-empty it is registered
// as healthy with zero (unknown) latency.
func NewRegionRegistry(self string) *RegionRegistry {
	r := &RegionRegistry{self: self, regions: make(map[string]*Region)}
	if self != "" {
		r.regions[self] = &Region{ID: self, Healthy: true}
	}
	return r
}

// Self returns the local region id.
func (r *RegionRegistry) Self() string { return r.self }

// Register upserts a region. Use this to announce a peer region (with its
// endpoint and an initial latency/health estimate) or to refresh the local
// region's metadata.
func (r *RegionRegistry) Register(reg Region) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regions[reg.ID] = &reg
}

// Unregister removes a region. The local region is never removed; calling
// Unregister with Self is a no-op so a member cannot de-list itself.
func (r *RegionRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == r.self {
		return
	}
	delete(r.regions, id)
}

// SetHealth updates the healthy flag for id. It returns ErrRegionNotFound if
// id is not registered.
func (r *RegionRegistry) SetHealth(id string, healthy bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.regions[id]
	if !ok {
		return ErrRegionNotFound
	}
	reg.Healthy = healthy
	return nil
}

// SetLatency records the observed round-trip time to id. It returns
// ErrRegionNotFound if id is not registered.
func (r *RegionRegistry) SetLatency(id string, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.regions[id]
	if !ok {
		return ErrRegionNotFound
	}
	reg.Latency = d
	return nil
}

// Get returns a snapshot of the region with id and whether it exists.
func (r *RegionRegistry) Get(id string) (Region, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.regions[id]
	if !ok {
		return Region{}, false
	}
	return *reg, true
}

// List returns snapshots of every region ordered by ID.
func (r *RegionRegistry) List() []Region {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Region, 0, len(r.regions))
	for _, reg := range r.regions {
		out = append(out, *reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Healthy returns the healthy regions ranked best-first by the routing key:
// known latency before unknown, then lower latency, then lower priority,
// then ID. It is the order the Router selects from.
func (r *RegionRegistry) Healthy() []Region {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Region, 0, len(r.regions))
	for _, reg := range r.regions {
		if reg.Healthy {
			out = append(out, *reg)
		}
	}
	return rankRegions(out)
}

// rankRegions returns a copy of in sorted by the routing key: regions with a
// known latency rank before unknown-latency ones, then by ascending latency,
// then ascending priority, then ID. The ordering is the heart of
// region-aware routing for latency optimization.
func rankRegions(in []Region) []Region {
	out := append([]Region(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		ak, bk := a.Latency > 0, b.Latency > 0
		if ak != bk {
			return ak // known latency ranks first
		}
		if a.Latency != b.Latency {
			return a.Latency < b.Latency
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})
	return out
}

// Router performs region-aware routing over a RegionRegistry. Among the
// healthy regions it selects the one with the lowest observed latency
// (latency optimization), breaking ties by Priority then ID, and it fails
// over to the next-best healthy region when the preferred region is
// unavailable (disaster recovery).
type Router struct {
	registry *RegionRegistry
}

// NewRouter returns a Router backed by reg.
func NewRouter(reg *RegionRegistry) *Router {
	return &Router{registry: reg}
}

// Route returns the best healthy region, or ErrRegionNotFound if no region
// is healthy. "Best" follows RegionRegistry.Healthy's ranking: lowest known
// latency, then priority, then ID.
func (rt *Router) Route() (Region, error) {
	healthy := rt.registry.Healthy()
	if len(healthy) == 0 {
		return Region{}, ErrRegionNotFound
	}
	return healthy[0], nil
}

// RouteFailover returns preferred when it is healthy; otherwise it returns
// the best healthy peer. This is the disaster-recovery routing path: a
// request normally bound for preferred is rerouted to a healthy peer the
// moment preferred is down. Returns ErrRegionNotFound if no region at all is
// healthy.
func (rt *Router) RouteFailover(preferred string) (Region, error) {
	healthy := rt.registry.Healthy()
	if len(healthy) == 0 {
		return Region{}, ErrRegionNotFound
	}
	if preferred != "" {
		for _, reg := range healthy {
			if reg.ID == preferred {
				return reg, nil
			}
		}
	}
	// preferred is absent or unhealthy; healthy is already best-first and
	// cannot contain an unhealthy preferred region.
	return healthy[0], nil
}

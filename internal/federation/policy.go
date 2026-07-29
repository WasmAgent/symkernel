// Package federation implements multi-region policy replication for the
// symkernel verification service, the production-orchestration tier called
// out by Milestone 11.
//
// A symkernel deployment may run in several regions for latency and
// availability. This package lets every region hold a local copy of the
// policy set and converge on the same view through an anti-entropy
// replication protocol (eventual consistency), and it routes work to the
// best healthy region for latency optimization and disaster recovery.
//
// The package is built from three cooperating pieces, each transport- and
// storage-agnostic so a production deployment can swap the network seam:
//
//   - RegionRegistry / Router: tracks the participating regions, their
//     observed latency and health, and selects a region for a request
//     preferring low latency with automatic failover to a healthy peer when
//     the preferred region is unavailable (disaster recovery).
//   - PolicyStore: the local, per-region store of versioned policies. A
//     MemoryStore ships for tests and single-process simulation.
//   - Replicator + Transport: a background anti-entropy loop reconciles the
//     local store against each peer region through the Transport seam,
//     pushing newer local revisions and pulling newer remote ones and
//     resolving concurrent writes by last-write-wins so the fleet converges.
//
// The Federation type wires the three pieces into a single façade and
// exposes telemetry through Stats / RegisterRoutes, mirroring the
// distributed-cache admin surface. As with internal/distributed, no real
// network client is required: the LocalTransport simulates a fleet of
// regions in one process so the replication protocol is exercised by tests.
package federation

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrPolicyNotFound is returned by PolicyStore.Get and Transport.GetPolicy
// when a policy is absent. Callers should treat it as a miss, not a hard
// error (errors.Is is the supported check).
var ErrPolicyNotFound = errors.New("federation: policy not found")

// ErrRegionNotFound is returned when a region id is unknown to the registry
// or transport.
var ErrRegionNotFound = errors.New("federation: region not found")

// Policy is a versioned policy artifact replicated across regions. Body is
// opaque to the federation layer (typically a serialized CEL expression, a
// composed-policy document, or a Z3 constraint) so the package can carry any
// policy shape symkernel supports.
type Policy struct {
	// ID is the stable policy identifier shared across all regions.
	ID string `json:"id"`

	// Version is the monotonic revision number assigned by the region that
	// authored this revision (OriginRegion). Replication compares versions to
	// decide direction; ties are broken by UpdatedAt (last-write-wins).
	Version uint64 `json:"version"`

	// Body is the opaque policy payload.
	Body []byte `json:"body"`

	// OriginRegion is the region that authored this Version. It is carried
	// for diagnostics and audit; it does not direct replication.
	OriginRegion string `json:"origin_region"`

	// UpdatedAt is the wall-clock time of the last write, used to resolve
	// concurrent writes from different regions (last-write-wins).
	UpdatedAt time.Time `json:"updated_at"`
}

// PolicyStore is the local, per-region policy storage contract. It holds the
// authoritative copy for policies authored in this region and a best-effort
// copy of policies replicated from peers. Implementations must be safe for
// concurrent use.
type PolicyStore interface {
	// Get returns the policy for id, or an error wrapping ErrPolicyNotFound
	// if it is absent.
	Get(ctx context.Context, id string) (*Policy, error)

	// Put upserts the policy keyed by ID. It does not bump Version; callers
	// assign a monotonic version on authored writes.
	Put(ctx context.Context, p *Policy) error

	// Delete removes the policy for id. Deleting an absent policy is a no-op.
	Delete(ctx context.Context, id string) error

	// List returns every policy currently held, ordered by ID.
	List(ctx context.Context) ([]*Policy, error)
}

// MemoryStore is an in-process PolicyStore backed by a map. It is the
// default store and the one used by tests; it is also a reasonable choice
// for single-region deployments that want the federation API without an
// external database. It is safe for concurrent use.
type MemoryStore struct {
	mu       sync.Mutex
	policies map[string]*Policy
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{policies: make(map[string]*Policy)}
}

// Get returns a copy of the policy for id, or ErrPolicyNotFound if absent.
func (s *MemoryStore) Get(_ context.Context, id string) (*Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[id]
	if !ok {
		return nil, ErrPolicyNotFound
	}
	return clonePolicy(p), nil
}

// Put stores a copy of p keyed by p.ID.
func (s *MemoryStore) Put(_ context.Context, p *Policy) error {
	if p == nil {
		return errors.New("federation: Put(nil policy)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[p.ID] = clonePolicy(p)
	return nil
}

// Delete removes the policy for id. It is a no-op for an absent id.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policies, id)
	return nil
}

// List returns copies of every held policy, ordered by ID.
func (s *MemoryStore) List(_ context.Context) ([]*Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Policy, 0, len(s.policies))
	for _, p := range s.policies {
		out = append(out, clonePolicy(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// clonePolicy returns a deep copy of p so callers cannot mutate stored state
// through the returned pointer or its Body slice.
func clonePolicy(p *Policy) *Policy {
	if p == nil {
		return nil
	}
	body := make([]byte, len(p.Body))
	copy(body, p.Body)
	return &Policy{
		ID:           p.ID,
		Version:      p.Version,
		Body:         body,
		OriginRegion: p.OriginRegion,
		UpdatedAt:    p.UpdatedAt,
	}
}

// ReplicationMode controls how aggressively authored policies propagate to
// peer regions.
type ReplicationMode int

const (
	// ReplicationAsync propagates policies via the background anti-entropy
	// loop only. Writes return as soon as the local store is updated; peers
	// observe the change within one scan interval (eventual consistency).
	// This is the default and maximizes write availability.
	ReplicationAsync ReplicationMode = iota

	// ReplicationSync additionally attempts an immediate one-shot push of a
	// newly authored policy to every target region before the write returns.
	// The local write is committed regardless (high availability); the
	// returned error, if any, reports which peers could not be reached so the
	// caller knows replication was partial.
	ReplicationSync
)

// ReplicationConfig governs which policies replicate to which regions and
// how often the anti-entropy loop runs.
type ReplicationConfig struct {
	// Mode selects async (background) or sync (immediate-push) propagation.
	// The zero value is ReplicationAsync.
	Mode ReplicationMode

	// Interval is the anti-entropy scan period. Zero defaults to 30 seconds
	// (see NewReplicator). Shorter intervals converge faster at the cost of
	// more cross-region traffic.
	Interval time.Duration

	// Regions is the allowlist of region IDs that receive replicated
	// policies. When non-empty it overrides automatic peer discovery and the
	// Replicator replicates to exactly these regions (minus the local one),
	// letting an operator pin replication to a subset for cost or
	// data-residency reasons. When empty the Replicator targets every healthy
	// peer region in the registry.
	Regions []string
}

// modeString returns the telemetry name for a replication mode.
func modeString(m ReplicationMode) string {
	switch m {
	case ReplicationSync:
		return "sync"
	default:
		return "async"
	}
}

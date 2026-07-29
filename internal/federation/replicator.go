package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// defaultReplicationInterval is the anti-entropy scan period applied when
// ReplicationConfig.Interval is unset.
const defaultReplicationInterval = 30 * time.Second

// Transport is the cross-region delivery seam: it lets the Replicator read
// and write policies on a peer region without coupling to a concrete network
// client. LocalTransport is the in-process implementation; a production
// deployment plugs in an HTTP client speaking the symkernel federation API.
type Transport interface {
	// GetPolicy fetches the current revision of id from region, returning an
	// error wrapping ErrPolicyNotFound when the region does not hold it.
	GetPolicy(ctx context.Context, region, id string) (*Policy, error)

	// PutPolicy upserts p on region.
	PutPolicy(ctx context.Context, region string, p *Policy) error

	// ListPolicies returns every policy held by region.
	ListPolicies(ctx context.Context, region string) ([]*Policy, error)

	// Ping verifies region reachability.
	Ping(ctx context.Context, region string) error
}

// LocalTransport is an in-process Transport that routes operations to a set
// of named PolicyStores, one per simulated region. It is the transport used
// by tests and by single-process multi-region simulation; a real deployment
// replaces it with an HTTP client. Operations on a region that has no
// mounted store return ErrRegionNotFound.
type LocalTransport struct {
	mu     sync.RWMutex
	stores map[string]PolicyStore
}

// NewLocalTransport returns a Transport with no regions mounted.
func NewLocalTransport() *LocalTransport {
	return &LocalTransport{stores: make(map[string]PolicyStore)}
}

// Mount binds store as the destination for region, replacing any previous
// binding. Mounting a region into a shared LocalTransport is how a
// single-process test simulates a fleet of peer regions.
func (t *LocalTransport) Mount(region string, store PolicyStore) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stores[region] = store
}

// Unmount removes the binding for region.
func (t *LocalTransport) Unmount(region string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.stores, region)
}

func (t *LocalTransport) storeFor(region string) (PolicyStore, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.stores[region]
	if !ok {
		return nil, ErrRegionNotFound
	}
	return s, nil
}

// GetPolicy fetches id from the store mounted at region.
func (t *LocalTransport) GetPolicy(ctx context.Context, region, id string) (*Policy, error) {
	s, err := t.storeFor(region)
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// PutPolicy upserts p on the store mounted at region.
func (t *LocalTransport) PutPolicy(ctx context.Context, region string, p *Policy) error {
	s, err := t.storeFor(region)
	if err != nil {
		return err
	}
	return s.Put(ctx, p)
}

// ListPolicies returns every policy held by the store mounted at region.
func (t *LocalTransport) ListPolicies(ctx context.Context, region string) ([]*Policy, error) {
	s, err := t.storeFor(region)
	if err != nil {
		return nil, err
	}
	return s.List(ctx)
}

// Ping reports whether a store is mounted at region.
func (t *LocalTransport) Ping(_ context.Context, region string) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.stores[region]; !ok {
		return ErrRegionNotFound
	}
	return nil
}

// Replicator drives the anti-entropy replication loop. On each scan it
// reconciles the local store against every target region reachable through
// the Transport: for each policy present on either side, the newer revision
// (by Version, ties by UpdatedAt) wins and is copied to the losing side.
// Repeated scans converge the fleet to the same view — eventual consistency.
//
// Target regions come from ReplicationConfig.Regions when that allowlist is
// non-empty, otherwise from the registry's healthy peers; the local (self)
// region is never a target.
type Replicator struct {
	self      string
	store     PolicyStore
	registry  *RegionRegistry
	transport Transport
	cfg       ReplicationConfig

	stopCh chan struct{}
	wg     sync.WaitGroup

	// Telemetry counters (atomic for lock-free reads on the hot path).
	scans   uint64
	pushed  uint64
	pulled  uint64
	skipped uint64
	errors  uint64
}

// NewReplicator returns a Replicator that reconciles store against the
// configured targets. A zero Interval defaults to 30 seconds.
func NewReplicator(self string, store PolicyStore, registry *RegionRegistry, transport Transport, cfg ReplicationConfig) *Replicator {
	if cfg.Interval <= 0 {
		cfg.Interval = defaultReplicationInterval
	}
	return &Replicator{
		self:      self,
		store:     store,
		registry:  registry,
		transport: transport,
		cfg:       cfg,
		stopCh:    make(chan struct{}),
	}
}

// Targets returns the region IDs this replicator pushes to and pulls from:
// the configured allowlist (minus self) when non-empty, otherwise the
// registry's healthy peers (minus self).
func (rep *Replicator) Targets() []string {
	if len(rep.cfg.Regions) > 0 {
		out := make([]string, 0, len(rep.cfg.Regions))
		seen := make(map[string]bool, len(rep.cfg.Regions))
		for _, id := range rep.cfg.Regions {
			if id == rep.self || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
		return out
	}
	healthy := rep.registry.Healthy()
	out := make([]string, 0, len(healthy))
	for _, reg := range healthy {
		if reg.ID == rep.self {
			continue
		}
		out = append(out, reg.ID)
	}
	return out
}

// Reconcile performs one anti-entropy pass over every target region and
// returns a joined error of any per-region failures. Partial failures do not
// abort the pass: a region that cannot be reached is skipped and the
// remaining regions still reconcile.
func (rep *Replicator) Reconcile(ctx context.Context) error {
	atomic.AddUint64(&rep.scans, 1)
	var errs []error
	for _, region := range rep.Targets() {
		if err := rep.reconcileRegion(ctx, region); err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", region, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileRegion reconciles the local store against a single peer.
func (rep *Replicator) reconcileRegion(ctx context.Context, region string) error {
	local, err := rep.store.List(ctx)
	if err != nil {
		atomic.AddUint64(&rep.errors, 1)
		return fmt.Errorf("list local: %w", err)
	}
	remote, err := rep.transport.ListPolicies(ctx, region)
	if err != nil {
		atomic.AddUint64(&rep.errors, 1)
		return fmt.Errorf("list remote: %w", err)
	}

	remoteByID := make(map[string]*Policy, len(remote))
	for _, p := range remote {
		remoteByID[p.ID] = p
	}

	var errs []error
	for _, lp := range local {
		rp := remoteByID[lp.ID]
		delete(remoteByID, lp.ID) // remainder becomes remote-only pulls
		if err := rep.reconcilePolicy(ctx, region, lp, rp); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rp := range remoteByID {
		if err := rep.reconcilePolicy(ctx, region, nil, rp); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcilePolicy reconciles a single policy across the local store and a
// peer region, copying the newer revision to the losing side. local/remote
// may be nil to signal "absent on this side".
func (rep *Replicator) reconcilePolicy(ctx context.Context, region string, local, remote *Policy) error {
	switch {
	case local == nil && remote == nil:
		return nil
	case local == nil:
		// remote-only: pull.
		if err := rep.store.Put(ctx, remote); err != nil {
			atomic.AddUint64(&rep.errors, 1)
			return fmt.Errorf("pull %q: %w", remote.ID, err)
		}
		atomic.AddUint64(&rep.pulled, 1)
		return nil
	case remote == nil:
		// local-only: push.
		if err := rep.transport.PutPolicy(ctx, region, local); err != nil {
			atomic.AddUint64(&rep.errors, 1)
			return fmt.Errorf("push %q: %w", local.ID, err)
		}
		atomic.AddUint64(&rep.pushed, 1)
		return nil
	}

	switch compareRevision(local, remote) {
	case 1: // local newer: push.
		if err := rep.transport.PutPolicy(ctx, region, local); err != nil {
			atomic.AddUint64(&rep.errors, 1)
			return fmt.Errorf("push %q: %w", local.ID, err)
		}
		atomic.AddUint64(&rep.pushed, 1)
	case -1: // remote newer: pull.
		if err := rep.store.Put(ctx, remote); err != nil {
			atomic.AddUint64(&rep.errors, 1)
			return fmt.Errorf("pull %q: %w", remote.ID, err)
		}
		atomic.AddUint64(&rep.pulled, 1)
	default:
		atomic.AddUint64(&rep.skipped, 1)
	}
	return nil
}

// compareRevision returns 1 if a is newer than b, -1 if older, 0 if equal.
// Newer means a higher Version; when versions tie — because two regions
// authored different revisions of the same policy concurrently — the later
// UpdatedAt wins (last-write-wins). Equal Version and UpdatedAt is a no-op.
func compareRevision(a, b *Policy) int {
	if a.Version != b.Version {
		if a.Version > b.Version {
			return 1
		}
		return -1
	}
	switch {
	case a.UpdatedAt.Before(b.UpdatedAt):
		return -1
	case a.UpdatedAt.After(b.UpdatedAt):
		return 1
	default:
		return 0
	}
}

// Start launches the background anti-entropy loop. Call Stop to terminate it.
// The loop is optional: callers may drive replication deterministically via
// Reconcile instead.
func (rep *Replicator) Start() {
	rep.wg.Add(1)
	go rep.loop()
}

func (rep *Replicator) loop() {
	defer rep.wg.Done()
	ticker := time.NewTicker(rep.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-rep.stopCh:
			return
		case <-ticker.C:
			_ = rep.Reconcile(context.Background())
		}
	}
}

// Stop terminates the background loop and waits for it to exit. It is safe
// to call Stop only after Start and only once.
func (rep *Replicator) Stop() {
	close(rep.stopCh)
	rep.wg.Wait()
}

// Federation wires a local PolicyStore, RegionRegistry, Transport, Router,
// and Replicator into a single multi-region policy-replication façade. It is
// the primary entry point for the federation tier; the zero value is
// unusable, use New.
type Federation struct {
	self       string
	store      PolicyStore
	registry   *RegionRegistry
	transport  Transport
	router     *Router
	replicator *Replicator
	cfg        ReplicationConfig

	// authored counts policies accepted via Put (the local-author path).
	authored uint64
	// syncPushes counts policies pushed immediately by sync-mode Put, which
	// bypasses the anti-entropy loop; it is separate from the Replicator's
	// push counter so operators can tell the two paths apart.
	syncPushes uint64
}

// Option configures a Federation during construction.
type Option func(*Federation)

// WithStore installs a specific local PolicyStore. Defaults to a MemoryStore.
func WithStore(s PolicyStore) Option { return func(f *Federation) { f.store = s } }

// WithRegistry installs a specific RegionRegistry. Defaults to one seeded
// with the local region.
func WithRegistry(r *RegionRegistry) Option { return func(f *Federation) { f.registry = r } }

// WithTransport installs a specific Transport. Defaults to a LocalTransport.
func WithTransport(t Transport) Option { return func(f *Federation) { f.transport = t } }

// WithConfig sets the replication configuration (mode, interval, allowlist).
func WithConfig(cfg ReplicationConfig) Option { return func(f *Federation) { f.cfg = cfg } }

// New creates a Federation for the local region self with the given options.
// Missing dependencies are defaulted so New("us-east-1") yields a usable,
// single-region federation; mount peer regions and stores on the Transport
// (or supply your own) to go multi-region.
func New(self string, opts ...Option) *Federation {
	f := &Federation{self: self}
	for _, o := range opts {
		o(f)
	}
	if f.store == nil {
		f.store = NewMemoryStore()
	}
	if f.registry == nil {
		f.registry = NewRegionRegistry(self)
	}
	if f.transport == nil {
		f.transport = NewLocalTransport()
	}
	if f.cfg.Interval <= 0 {
		f.cfg.Interval = defaultReplicationInterval
	}
	f.router = NewRouter(f.registry)
	f.replicator = NewReplicator(self, f.store, f.registry, f.transport, f.cfg)
	return f
}

// Store returns the local policy store.
func (f *Federation) Store() PolicyStore { return f.store }

// Registry returns the region registry.
func (f *Federation) Registry() *RegionRegistry { return f.registry }

// Router returns the region-aware router.
func (f *Federation) Router() *Router { return f.router }

// Transport returns the cross-region transport.
func (f *Federation) Transport() Transport { return f.transport }

// Replicator returns the anti-entropy replicator.
func (f *Federation) Replicator() *Replicator { return f.replicator }

// Regions returns a snapshot of every known region.
func (f *Federation) Regions() []Region { return f.registry.List() }

// SetRegionHealth updates a region's health gate, used by routing and
// auto-discovery replication. Returns ErrRegionNotFound for an unknown id.
func (f *Federation) SetRegionHealth(id string, healthy bool) error {
	return f.registry.SetHealth(id, healthy)
}

// SetRegionLatency records an observed round-trip time to a region, the
// signal region-aware routing optimizes for. Returns ErrRegionNotFound for an
// unknown id.
func (f *Federation) SetRegionLatency(id string, d time.Duration) error {
	return f.registry.SetLatency(id, d)
}

// Put authors a new revision of the policy id in the local store and
// propagates it according to the configured replication mode. In async mode
// (the default) the write returns once the local store is updated and the
// background loop (if started) carries it to peers. In sync mode the write
// additionally attempts an immediate push to every target region; the local
// revision is committed regardless, and a non-nil returned error reports
// which peers could not be reached so the caller knows replication was
// partial.
func (f *Federation) Put(ctx context.Context, id string, body []byte) (*Policy, error) {
	if id == "" {
		return nil, errors.New("federation: empty policy id")
	}
	cur, err := f.store.Get(ctx, id)
	if err != nil && !errors.Is(err, ErrPolicyNotFound) {
		return nil, err
	}
	p := &Policy{
		ID:           id,
		Version:      1,
		Body:         body,
		OriginRegion: f.self,
		UpdatedAt:    time.Now(),
	}
	if cur != nil {
		p.Version = cur.Version + 1
	}
	if err := f.store.Put(ctx, p); err != nil {
		return nil, err
	}
	atomic.AddUint64(&f.authored, 1)

	if f.cfg.Mode == ReplicationSync {
		// Local revision is committed; report partial replication only.
		if perr := f.pushNow(ctx, p); perr != nil {
			return p, fmt.Errorf("federation: replicated locally, partial push: %w", perr)
		}
	}
	return p, nil
}

// pushNow synchronously pushes p to every target region, joining per-region
// failures into one error.
func (f *Federation) pushNow(ctx context.Context, p *Policy) error {
	var errs []error
	for _, region := range f.replicator.Targets() {
		if err := f.transport.PutPolicy(ctx, region, p); err != nil {
			errs = append(errs, fmt.Errorf("region %q: %w", region, err))
			continue
		}
		atomic.AddUint64(&f.syncPushes, 1)
	}
	return errors.Join(errs...)
}

// Get returns the locally held revision of id, or ErrPolicyNotFound.
func (f *Federation) Get(ctx context.Context, id string) (*Policy, error) {
	return f.store.Get(ctx, id)
}

// Delete removes id from the local store. Deletion is local only: without a
// tombstone a peer that still holds the policy will re-replicate it on the
// next anti-entropy scan. Tombstone-based deletion is a follow-up; for now a
// delete intended to take fleet-wide effect should be expressed as a new
// "disabled" revision via Put.
func (f *Federation) Delete(ctx context.Context, id string) error {
	return f.store.Delete(ctx, id)
}

// Reconcile performs one anti-entropy pass against every target region. It
// is the explicit equivalent of one tick of the background loop and is the
// way tests and batch jobs drive deterministic convergence.
func (f *Federation) Reconcile(ctx context.Context) error {
	return f.replicator.Reconcile(ctx)
}

// Start launches the background anti-entropy loop. Call Stop to terminate.
func (f *Federation) Start() { f.replicator.Start() }

// Stop terminates the background loop and waits for it to exit.
func (f *Federation) Stop() { f.replicator.Stop() }

// FederationStats is a point-in-time snapshot of the federation state and
// replication telemetry.
type FederationStats struct {
	Self              string `json:"self"`
	Mode              string `json:"mode"`
	Interval          string `json:"interval"`
	Regions           int    `json:"regions"`
	HealthyRegions    int    `json:"healthy_regions"`
	Targets           int    `json:"targets"`
	Policies          int    `json:"policies"`
	Authored          uint64 `json:"authored"`
	SyncPushes        uint64 `json:"sync_pushes"`
	Scans             uint64 `json:"scans"`
	Pushed            uint64 `json:"pushed"`
	Pulled            uint64 `json:"pulled"`
	Skipped           uint64 `json:"skipped"`
	ReplicationErrors uint64 `json:"replication_errors"`
}

// Stats returns a point-in-time snapshot of federation state and the
// replication counters.
func (f *Federation) Stats() FederationStats {
	st := FederationStats{
		Self:     f.self,
		Mode:     modeString(f.cfg.Mode),
		Interval: f.cfg.Interval.String(),
		Authored:   atomic.LoadUint64(&f.authored),
		SyncPushes: atomic.LoadUint64(&f.syncPushes),
		Scans:      atomic.LoadUint64(&f.replicator.scans),
		Pushed:   atomic.LoadUint64(&f.replicator.pushed),
		Pulled:   atomic.LoadUint64(&f.replicator.pulled),
		Skipped:  atomic.LoadUint64(&f.replicator.skipped),
		ReplicationErrors: atomic.LoadUint64(&f.replicator.errors),
		Targets:  len(f.replicator.Targets()),
	}
	for _, reg := range f.registry.List() {
		st.Regions++
		if reg.Healthy {
			st.HealthyRegions++
		}
	}
	if list, err := f.store.List(context.Background()); err == nil {
		st.Policies = len(list)
	}
	return st
}

// RegisterRoutes mounts the federation admin endpoints on mux:
//
//	GET /v1/admin/federation/stats   — federation telemetry
//	GET /v1/admin/federation/regions — known regions and health
func (f *Federation) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/admin/federation/stats", f.statsHandler())
	mux.Handle("GET /v1/admin/federation/regions", f.regionsHandler())
}

// statsHandler handles GET /v1/admin/federation/stats.
func (f *Federation) statsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.Stats()) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

// regionsHandler handles GET /v1/admin/federation/regions.
func (f *Federation) regionsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.Regions()) //nolint:errcheck // partial write to ResponseWriter is unrecoverable
	}
}

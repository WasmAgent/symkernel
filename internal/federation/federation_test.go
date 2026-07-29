package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ---- MemoryStore tests ----

func TestMemoryStoreGetMissing(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Get(context.Background(), "nope")
	if !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("Get missing: got err=%v, want ErrPolicyNotFound", err)
	}
}

func TestMemoryStorePutGetDeleteList(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()

	if err := s.Put(ctx, &Policy{ID: "b", Version: 1, Body: []byte("B")}); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := s.Put(ctx, &Policy{ID: "a", Version: 1, Body: []byte("A")}); err != nil {
		t.Fatalf("Put a: %v", err)
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if string(got.Body) != "A" {
		t.Fatalf("Get a body = %q, want %q", got.Body, "A")
	}

	// List is ordered by ID.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("List = %+v, want [a,b]", list)
	}

	// Mutating a returned copy must not corrupt stored state.
	got.Body[0] = 'X'
	again, _ := s.Get(ctx, "a")
	if string(again.Body) != "A" {
		t.Fatalf("stored body mutated via returned copy: %q", again.Body)
	}

	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete a: %v", err)
	}
	if _, err := s.Get(ctx, "a"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("after Delete, got err=%v, want ErrPolicyNotFound", err)
	}
	// Delete of an absent id is a no-op.
	if err := s.Delete(ctx, "absent"); err != nil {
		t.Fatalf("Delete absent: unexpected err %v", err)
	}
}

func TestMemoryStorePutNil(t *testing.T) {
	if err := (NewMemoryStore()).Put(context.Background(), nil); err == nil {
		t.Fatal("Put(nil) returned nil, want error")
	}
}

// ---- RegionRegistry + Router tests ----

func TestRegionRegistrySelfProtected(t *testing.T) {
	r := NewRegionRegistry("us")
	r.Register(Region{ID: "eu", Healthy: true})
	r.Unregister("us") // no-op for self
	r.Unregister("eu")

	if _, ok := r.Get("us"); !ok {
		t.Fatal("self region was removed by Unregister(self)")
	}
	if _, ok := r.Get("eu"); ok {
		t.Fatal("eu should have been removed")
	}
}

func TestRegionRegistrySetHealthLatencyUnknown(t *testing.T) {
	r := NewRegionRegistry("us")
	if err := r.SetHealth("nope", true); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("SetHealth unknown: got err=%v, want ErrRegionNotFound", err)
	}
	if err := r.SetLatency("nope", 10*time.Millisecond); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("SetLatency unknown: got err=%v, want ErrRegionNotFound", err)
	}
}

func TestRouterRoutePicksLowestLatency(t *testing.T) {
	r := NewRegionRegistry("")
	r.Register(Region{ID: "near", Healthy: true, Latency: 10 * time.Millisecond})
	r.Register(Region{ID: "far", Healthy: true, Latency: 200 * time.Millisecond})
	// unknown-latency region ranks last even if "closer" by id/priority.
	r.Register(Region{ID: "zzz", Healthy: true, Latency: 0, Priority: -1})

	rt := NewRouter(r)
	got, err := rt.Route()
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got.ID != "near" {
		t.Fatalf("Route = %q, want near (lowest known latency)", got.ID)
	}
}

func TestRouterRouteNoHealthy(t *testing.T) {
	r := NewRegionRegistry("")
	r.Register(Region{ID: "a", Healthy: false, Latency: 1 * time.Millisecond})
	if _, err := (NewRouter(r)).Route(); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("Route with no healthy: got err=%v, want ErrRegionNotFound", err)
	}
}

func TestRouterRouteFailover(t *testing.T) {
	r := NewRegionRegistry("")
	r.Register(Region{ID: "primary", Healthy: false, Latency: 5 * time.Millisecond})
	r.Register(Region{ID: "backup", Healthy: true, Latency: 50 * time.Millisecond})

	rt := NewRouter(r)
	// primary down -> failover to backup.
	got, err := rt.RouteFailover("primary")
	if err != nil {
		t.Fatalf("RouteFailover: %v", err)
	}
	if got.ID != "backup" {
		t.Fatalf("RouteFailover = %q, want backup", got.ID)
	}
	// primary back up -> returns preferred.
	if err := r.SetHealth("primary", true); err != nil {
		t.Fatal(err)
	}
	got, _ = rt.RouteFailover("primary")
	if got.ID != "primary" {
		t.Fatalf("RouteFailover healthy preferred = %q, want primary", got.ID)
	}
}

func TestRouterRoutePriorityTiebreak(t *testing.T) {
	r := NewRegionRegistry("")
	// Equal latency: lower Priority wins.
	r.Register(Region{ID: "a", Healthy: true, Latency: 10 * time.Millisecond, Priority: 5})
	r.Register(Region{ID: "b", Healthy: true, Latency: 10 * time.Millisecond, Priority: 1})
	got, _ := NewRouter(r).Route()
	if got.ID != "b" {
		t.Fatalf("Route tie = %q, want b (lower priority)", got.ID)
	}
}

// ---- LocalTransport tests ----

func TestLocalTransportUnknownRegion(t *testing.T) {
	tt := NewLocalTransport()
	ctx := context.Background()
	if _, err := tt.GetPolicy(ctx, "x", "p"); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("GetPolicy unknown region: got err=%v, want ErrRegionNotFound", err)
	}
	if err := tt.PutPolicy(ctx, "x", &Policy{ID: "p"}); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("PutPolicy unknown region: got err=%v, want ErrRegionNotFound", err)
	}
	if _, err := tt.ListPolicies(ctx, "x"); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("ListPolicies unknown region: got err=%v, want ErrRegionNotFound", err)
	}
	if err := tt.Ping(ctx, "x"); !errors.Is(err, ErrRegionNotFound) {
		t.Fatalf("Ping unknown region: got err=%v, want ErrRegionNotFound", err)
	}
}

func TestLocalTransportMountRoundTrip(t *testing.T) {
	tt := NewLocalTransport()
	store := NewMemoryStore()
	tt.Mount("us", store)
	ctx := context.Background()

	if err := tt.PutPolicy(ctx, "us", &Policy{ID: "p", Version: 1, Body: []byte("hi")}); err != nil {
		t.Fatalf("PutPolicy: %v", err)
	}
	got, err := tt.GetPolicy(ctx, "us", "p")
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("Version = %d, want 1", got.Version)
	}
	list, err := tt.ListPolicies(ctx, "us")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListPolicies = %v (len %d), want 1", err, len(list))
	}
}

// ---- Replicator anti-entropy tests ----

// newFleet builds three region federations (a, b, c) sharing one transport
// and registry, each backed by its own MemoryStore mounted on the transport.
// It returns the federations keyed by id and the shared transport.
func newFleet(t *testing.T, cfg ReplicationConfig) (map[string]*Federation, *LocalTransport) {
	t.Helper()
	transport := NewLocalTransport()
	registry := NewRegionRegistry("")

	stores := map[string]*MemoryStore{}
	feds := map[string]*Federation{}
	for _, id := range []string{"a", "b", "c"} {
		s := NewMemoryStore()
		stores[id] = s
		transport.Mount(id, s)
		registry.Register(Region{ID: id, Healthy: true, Latency: 10 * time.Millisecond})
		feds[id] = New(id, WithStore(stores[id]), WithRegistry(registry), WithTransport(transport), WithConfig(cfg))
	}
	return feds, transport
}

func TestReplicatorPushAndPull(t *testing.T) {
	feds, _ := newFleet(t, ReplicationConfig{Interval: time.Second})
	ctx := context.Background()

	// a has a policy b does not: reconcile a => push to b and c.
	if err := feds["a"].store.Put(ctx, &Policy{ID: "p1", Version: 1, Body: []byte("a1"), OriginRegion: "a", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := feds["a"].Reconcile(ctx); err != nil {
		t.Fatalf("a.Reconcile: %v", err)
	}
	if got, err := feds["b"].store.Get(ctx, "p1"); err != nil || string(got.Body) != "a1" {
		t.Fatalf("b did not receive p1 via push: err=%v got=%v", err, got)
	}
	if got := atomic.LoadUint64(&feds["a"].replicator.pushed); got != 2 {
		t.Fatalf("pushed = %d, want 2 (b and c)", got)
	}

	// c now authors a policy a does not: reconcile c => pull into a.
	if err := feds["c"].store.Put(ctx, &Policy{ID: "p2", Version: 1, Body: []byte("c1"), OriginRegion: "c", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := feds["c"].Reconcile(ctx); err != nil {
		t.Fatalf("c.Reconcile: %v", err)
	}
	if got, err := feds["a"].store.Get(ctx, "p2"); err != nil || string(got.Body) != "c1" {
		t.Fatalf("a did not receive p2 via pull: err=%v got=%v", err, got)
	}
}

func TestReplicatorNewerRevisionWins(t *testing.T) {
	feds, _ := newFleet(t, ReplicationConfig{Interval: time.Second})
	ctx := context.Background()

	older := &Policy{ID: "p", Version: 1, Body: []byte("old"), OriginRegion: "a", UpdatedAt: time.UnixMilli(1000)}
	newer := &Policy{ID: "p", Version: 2, Body: []byte("new"), OriginRegion: "b", UpdatedAt: time.UnixMilli(2000)}
	_ = feds["a"].store.Put(ctx, older)
	_ = feds["b"].store.Put(ctx, newer)

	// a reconciles: local v1 < remote v2 => pull newer from b.
	if err := feds["a"].Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := feds["a"].store.Get(ctx, "p")
	if got.Version != 2 || string(got.Body) != "new" {
		t.Fatalf("a did not pull newer revision: v=%d body=%s", got.Version, got.Body)
	}
	// b reconciles: its v2 > a's (now v2) => equal => skip, no pull of stale.
	before := atomic.LoadUint64(&feds["b"].replicator.pulled)
	_ = feds["b"].Reconcile(ctx)
	if got := atomic.LoadUint64(&feds["b"].replicator.pulled); got != before {
		t.Fatalf("b pulled when revisions were equal: delta=%d", got-before)
	}
}

func TestReplicatorLastWriteWinsOnVersionTie(t *testing.T) {
	feds, _ := newFleet(t, ReplicationConfig{Interval: time.Second})
	ctx := context.Background()

	// Two regions concurrently author the same Version with different
	// timestamps: the later UpdatedAt must win.
	earlier := &Policy{ID: "p", Version: 5, Body: []byte("early"), OriginRegion: "a", UpdatedAt: time.UnixMilli(1000)}
	later := &Policy{ID: "p", Version: 5, Body: []byte("late"), OriginRegion: "b", UpdatedAt: time.UnixMilli(9000)}
	_ = feds["a"].store.Put(ctx, earlier)
	_ = feds["b"].store.Put(ctx, later)

	_ = feds["a"].Reconcile(ctx) // a sees b's later write => pull.
	got, _ := feds["a"].store.Get(ctx, "p")
	if string(got.Body) != "late" {
		t.Fatalf("LWW tie did not pick later write: %s", got.Body)
	}
}

func TestReplicatorEventualConsistency(t *testing.T) {
	feds, _ := newFleet(t, ReplicationConfig{Interval: time.Second})
	ctx := context.Background()

	// Each region authors a distinct policy.
	_ = feds["a"].store.Put(ctx, &Policy{ID: "pa", Version: 1, Body: []byte("A"), OriginRegion: "a", UpdatedAt: time.Now()})
	_ = feds["b"].store.Put(ctx, &Policy{ID: "pb", Version: 1, Body: []byte("B"), OriginRegion: "b", UpdatedAt: time.Now()})
	_ = feds["c"].store.Put(ctx, &Policy{ID: "pc", Version: 1, Body: []byte("C"), OriginRegion: "c", UpdatedAt: time.Now()})

	// Each region reconciles once.
	for _, id := range []string{"a", "b", "c"} {
		if err := feds[id].Reconcile(ctx); err != nil {
			t.Fatalf("%s.Reconcile: %v", id, err)
		}
	}
	// a pulled pb/pc on its pass, but to fully converge c needs a's pull of
	// pb propagated: run a second round.
	for _, id := range []string{"a", "b", "c"} {
		_ = feds[id].Reconcile(ctx)
	}

	for _, id := range []string{"a", "b", "c"} {
		list, err := feds[id].store.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 3 {
			ids := make([]string, len(list))
			for i, p := range list {
				ids[i] = p.ID
			}
			t.Fatalf("region %s has %d policies (%v), want 3 converged", id, len(list), ids)
		}
	}
}

func TestReplicatorAllowlistOverridesDiscovery(t *testing.T) {
	// cfg pins replication to "b" only, even though c is a healthy peer.
	feds, _ := newFleet(t, ReplicationConfig{Interval: time.Second, Regions: []string{"b"}})
	ctx := context.Background()

	_ = feds["a"].store.Put(ctx, &Policy{ID: "p", Version: 1, Body: []byte("x"), OriginRegion: "a", UpdatedAt: time.Now()})
	_ = feds["a"].Reconcile(ctx)

	if _, err := feds["b"].store.Get(ctx, "p"); err != nil {
		t.Fatalf("b (allowlisted) did not receive p: %v", err)
	}
	if _, err := feds["c"].store.Get(ctx, "p"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("c (not allowlisted) received p: %v", err)
	}
}

// ---- Federation façade tests ----

func TestFederationPutIncrementsVersion(t *testing.T) {
	f := New("us")
	ctx := context.Background()

	p1, err := f.Put(ctx, "policy-x", []byte("v1"))
	if err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if p1.Version != 1 || p1.OriginRegion != "us" {
		t.Fatalf("p1 = %+v, want version 1 origin us", p1)
	}
	p2, err := f.Put(ctx, "policy-x", []byte("v2"))
	if err != nil {
		t.Fatalf("Put v2: %v", err)
	}
	if p2.Version != 2 {
		t.Fatalf("p2.Version = %d, want 2", p2.Version)
	}
	got, _ := f.Get(ctx, "policy-x")
	if string(got.Body) != "v2" {
		t.Fatalf("Get body = %s, want v2", got.Body)
	}
	if f.Stats().Authored != 2 {
		t.Fatalf("Authored = %d, want 2", f.Stats().Authored)
	}
}

func TestFederationPutEmptyID(t *testing.T) {
	if _, err := (New("us")).Put(context.Background(), "", []byte("x")); err == nil {
		t.Fatal("Put(empty id) returned nil, want error")
	}
}

func TestFederationSyncModePushesImmediately(t *testing.T) {
	// Sync mode: Put pushes to targets before returning, no Reconcile needed.
	cfg := ReplicationConfig{Mode: ReplicationSync, Interval: time.Second}
	feds, _ := newFleet(t, cfg)
	ctx := context.Background()

	if _, err := feds["a"].Put(ctx, "hot", []byte("now")); err != nil {
		t.Fatalf("Put sync: %v", err)
	}
	for _, id := range []string{"b", "c"} {
		got, err := feds[id].Get(ctx, "hot")
		if err != nil || string(got.Body) != "now" {
			t.Fatalf("sync push did not reach %s: err=%v got=%v", id, err, got)
		}
	}
}

func TestFederationAsyncModeRequiresReconcile(t *testing.T) {
	// Async mode (default): Put updates local only until Reconcile.
	feds, _ := newFleet(t, ReplicationConfig{Mode: ReplicationAsync, Interval: time.Second})
	ctx := context.Background()

	if _, err := feds["a"].Put(ctx, "lazy", []byte("later")); err != nil {
		t.Fatal(err)
	}
	if _, err := feds["b"].Get(ctx, "lazy"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("async pushed before Reconcile: %v", err)
	}
	_ = feds["a"].Reconcile(ctx)
	if got, err := feds["b"].Get(ctx, "lazy"); err != nil || string(got.Body) != "later" {
		t.Fatalf("async did not propagate after Reconcile: err=%v got=%v", err, got)
	}
}

func TestFederationBackgroundLoopReplicates(t *testing.T) {
	// A short-interval background loop must propagate without manual Reconcile.
	cfg := ReplicationConfig{Mode: ReplicationAsync, Interval: 5 * time.Millisecond}
	feds, _ := newFleet(t, cfg)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		feds[id].Start()
		defer feds[id].Stop()
	}

	if _, err := feds["a"].Put(ctx, "bg", []byte("z")); err != nil {
		t.Fatal(err)
	}

	// Poll for convergence (bounded so the test stays fast and non-flaky).
	deadline := time.Now().Add(2 * time.Second)
	var converged bool
	for time.Now().Before(deadline) && !converged {
		converged = true
		for _, id := range []string{"a", "b", "c"} {
			if got, err := feds[id].Get(ctx, "bg"); err != nil || string(got.Body) != "z" {
				converged = false
				break
			}
		}
		if !converged {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !converged {
		t.Fatal("background loop did not converge within deadline")
	}
}

func TestFederationStats(t *testing.T) {
	cfg := ReplicationConfig{Mode: ReplicationSync, Interval: 250 * time.Millisecond}
	feds, _ := newFleet(t, cfg)
	ctx := context.Background()

	_, _ = feds["a"].Put(ctx, "p", []byte("x"))
	st := feds["a"].Stats()
	if st.Self != "a" {
		t.Fatalf("Self = %q, want a", st.Self)
	}
	if st.Mode != "sync" {
		t.Fatalf("Mode = %q, want sync", st.Mode)
	}
	if st.Regions != 3 || st.HealthyRegions != 3 {
		t.Fatalf("Regions=%d Healthy=%d, want 3/3", st.Regions, st.HealthyRegions)
	}
	if st.Authored != 1 {
		t.Fatalf("Authored = %d, want 1", st.Authored)
	}
	if st.SyncPushes < 2 {
		t.Fatalf("SyncPushes = %d, want >=2 (sync push to b and c)", st.SyncPushes)
	}
}

func TestFederationRegisterRoutes(t *testing.T) {
	f := New("us")
	f.Registry().Register(Region{ID: "eu", Healthy: true, Latency: 20 * time.Millisecond})
	ctx := context.Background()
	_, _ = f.Put(ctx, "p", []byte("x"))

	mux := http.NewServeMux()
	f.RegisterRoutes(mux)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// /stats returns the expected telemetry shape.
	resp, err := http.Get(ts.URL + "/v1/admin/federation/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	var stats FederationStats
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Self != "us" || stats.Regions != 2 || stats.Policies != 1 {
		t.Fatalf("stats = %+v, want self=us regions=2 policies=1", stats)
	}

	// /regions returns the region list.
	resp2, err := http.Get(ts.URL + "/v1/admin/federation/regions")
	if err != nil {
		t.Fatalf("GET regions: %v", err)
	}
	defer resp2.Body.Close()
	var regions []Region
	if err := json.NewDecoder(resp2.Body).Decode(&regions); err != nil {
		t.Fatalf("decode regions: %v", err)
	}
	if len(regions) != 2 {
		t.Fatalf("regions = %d, want 2", len(regions))
	}
}

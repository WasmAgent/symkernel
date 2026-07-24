package sharding

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"
)

// ---------- Ring tests ----------

func TestNewRingDefaults(t *testing.T) {
	r := NewRing()
	if r.Size() != 0 {
		t.Fatalf("expected empty ring, got %d nodes", r.Size())
	}
	if r.NodeCount() != 0 {
		t.Fatalf("expected 0 virtual nodes, got %d", r.NodeCount())
	}
}

func TestRingAddRemove(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	if r.Size() != 1 {
		t.Fatalf("expected 1 node, got %d", r.Size())
	}
	expectedVNodes := defaultVirtualNodes
	if r.NodeCount() != expectedVNodes {
		t.Fatalf("expected %d virtual nodes, got %d", expectedVNodes, r.NodeCount())
	}

	r.AddNode("node-B")
	if r.Size() != 2 {
		t.Fatalf("expected 2 nodes, got %d", r.Size())
	}
	if r.NodeCount() != expectedVNodes*2 {
		t.Fatalf("expected %d virtual nodes, got %d", expectedVNodes*2, r.NodeCount())
	}

	// Adding the same node again is idempotent.
	r.AddNode("node-A")
	if r.Size() != 2 {
		t.Fatalf("expected still 2 nodes after duplicate add, got %d", r.Size())
	}

	r.RemoveNode("node-A")
	if r.Size() != 1 {
		t.Fatalf("expected 1 node after remove, got %d", r.Size())
	}
	members := r.Members()
	if len(members) != 1 || members[0] != "node-B" {
		t.Fatalf("expected [node-B], got %v", members)
	}

	// Removing a non-existent node is a no-op.
	r.RemoveNode("node-Z")
	if r.Size() != 1 {
		t.Fatalf("expected still 1 node after removing nonexistent, got %d", r.Size())
	}
}

func TestRingLookup(t *testing.T) {
	r := NewRing()
	_, ok := r.Lookup("key")
	if ok {
		t.Fatal("expected lookup on empty ring to return false")
	}

	r.AddNode("node-A")
	r.AddNode("node-B")
	r.AddNode("node-C")

	node, ok := r.Lookup("some-key")
	if !ok {
		t.Fatal("expected lookup to succeed on non-empty ring")
	}
	if node != "node-A" && node != "node-B" && node != "node-C" {
		t.Fatalf("expected a known node, got %q", node)
	}
}

func TestRingLookupDeterministic(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	r.AddNode("node-B")

	// The same key must always map to the same node.
	n1, _ := r.Lookup("my-key")
	for i := 0; i < 100; i++ {
		n2, ok := r.Lookup("my-key")
		if !ok || n2 != n1 {
			t.Fatalf("non-deterministic lookup: first=%q got=%q", n1, n2)
		}
	}
}

func TestRingDistribution(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	r.AddNode("node-B")
	r.AddNode("node-C")

	counts := map[string]int{}
	const keys = 10000
	for i := 0; i < keys; i++ {
		node, _ := r.Lookup(fmt.Sprintf("key-%d", i))
		counts[node]++
	}
	// Each node should get roughly 1/3 of the keys. With 150 vnodes the
	// worst-case imbalance is <5%, but be generous in assertions.
	for _, node := range r.Members() {
		share := float64(counts[node]) / float64(keys)
		if share < 0.20 || share > 0.50 {
			t.Errorf("node %s: got %.2f%% of keys, expected ~33%%", node, share*100)
		}
	}
}

func TestRingMinimalRebalanceOnRemove(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	r.AddNode("node-B")
	r.AddNode("node-C")

	const keys = 10000
	before := map[string]string{}
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("key-%d", i)
		before[k], _ = r.Lookup(k)
	}

	r.RemoveNode("node-B")

	migrated := 0
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("key-%d", i)
		after, _ := r.Lookup(k)
		if before[k] != after {
			migrated++
		}
	}
	// Only keys owned by node-B should migrate. With uniform distribution
	// that is ~1/3 of keys. At most 40% is a generous bound.
	ratio := float64(migrated) / float64(keys)
	if ratio > 0.40 {
		t.Errorf("too many keys migrated after removing one of three nodes: %.2f%%", ratio*100)
	}
}

func TestRingWithVirtualNodes(t *testing.T) {
	r := NewRing(WithVirtualNodes(50))
	r.AddNode("node-A")
	if r.NodeCount() != 50 {
		t.Fatalf("expected 50 vnodes with custom option, got %d", r.NodeCount())
	}
}

func TestRingMembersSorted(t *testing.T) {
	r := NewRing()
	r.AddNode("charlie")
	r.AddNode("alpha")
	r.AddNode("bravo")
	members := r.Members()
	if !sort.StringsAreSorted(members) {
		t.Fatalf("Members() should be sorted, got %v", members)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
}

func TestRingMigrationKeys(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	r.AddNode("node-B")
	r.AddNode("node-C")

	keys := []string{"k1", "k2", "k3", "k4", "k5"}
	migrations := r.MigrationKeys(keys, "node-B")
	// Every migrated key must have been owned by node-B before.
	for k, dest := range migrations {
		owner, _ := r.Lookup(k)
		if owner != "node-B" {
			t.Errorf("migration key %q was owned by %q, not node-B", k, owner)
		}
		if dest == "node-B" {
			t.Errorf("destination for key %q should not be node-B itself", k)
		}
	}
}

func TestRingConcurrentSafe(t *testing.T) {
	r := NewRing()
	r.AddNode("node-A")
	r.AddNode("node-B")

	const goroutines = 20
	const iters = 500
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				r.Lookup(fmt.Sprintf("key-%d", j))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				r.AddNode(fmt.Sprintf("node-%d", j))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				r.RemoveNode(fmt.Sprintf("node-%d", j))
			}
		}()
	}
	wg.Wait()
	// If we reach here without panicking or data races, the test passes.
}

// ---------- NodeManager tests ----------

func TestNodeManagerJoinLeave(t *testing.T) {
	r := NewRing()
	nm := NewNodeManager(r, NodeManagerOptions{
		CheckInterval: 1 * time.Hour, // effectively disabled
	})

	nm.Join("node-A")
	nodes := nm.Nodes()
	if len(nodes) != 1 || nodes["node-A"].State != StateHealthy {
		t.Fatalf("expected node-A healthy, got %v", nodes)
	}
	if r.Size() != 1 {
		t.Fatalf("ring should have 1 node, got %d", r.Size())
	}

	nm.Leave("node-A")
	if len(nm.Nodes()) != 0 {
		t.Fatalf("expected 0 nodes after leave, got %d", len(nm.Nodes()))
	}
	if r.Size() != 0 {
		t.Fatalf("ring should be empty after leave, got %d", r.Size())
	}
}

func TestNodeManagerHeartbeat(t *testing.T) {
	r := NewRing()
	nm := NewNodeManager(r, NodeManagerOptions{
		CheckInterval: 1 * time.Hour,
	})

	// Heartbeat on unknown node auto-joins.
	nm.Heartbeat("node-A")
	if r.Size() != 1 {
		t.Fatalf("expected ring size 1 after heartbeat auto-join, got %d", r.Size())
	}

	// Duplicate join is idempotent.
	nm.Join("node-A")
	if r.Size() != 1 {
		t.Fatalf("expected still 1 node, got %d", r.Size())
	}
}

func TestNodeManagerHealthChecks(t *testing.T) {
	r := NewRing()
	nm := NewNodeManager(r, NodeManagerOptions{
		CheckInterval: 50 * time.Millisecond,
		SuspectAfter:  100 * time.Millisecond,
		DownAfter:     200 * time.Millisecond,
	})
	nm.Start()
	defer nm.Stop()

	nm.Join("node-A")
	nm.Join("node-B")

	// Both should be healthy initially.
	if len(nm.HealthyNodes()) != 2 {
		t.Fatalf("expected 2 healthy nodes, got %v", nm.HealthyNodes())
	}

	// Keep node-B alive while node-A misses heartbeats.
	stopHB := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				nm.Heartbeat("node-B")
			case <-stopHB:
				return
			}
		}
	}()

	// Let node-A miss heartbeats long enough to go suspect then down.
	time.Sleep(400 * time.Millisecond)
	close(stopHB)

	// node-A should have been evicted; node-B should remain.
	if len(nm.Nodes()) != 1 {
		t.Fatalf("expected 1 node after health check (node-A removed), got %d", len(nm.Nodes()))
	}
	if r.Size() != 1 {
		t.Fatalf("expected ring size 1 after eviction, got %d", r.Size())
	}
}

func TestNodeManagerOnChange(t *testing.T) {
	r := NewRing()
	var lastSnapshot map[string]*NodeInfo
	notifyCh := make(chan struct{}, 1)
	nm := NewNodeManager(r, NodeManagerOptions{
		CheckInterval: 1 * time.Hour,
		OnChange: func(snap map[string]*NodeInfo) {
			lastSnapshot = snap
			select {
			case notifyCh <- struct{}{}:
			default:
			}
		},
	})

	nm.Join("node-A")
	<-notifyCh
	if len(lastSnapshot) != 1 {
		t.Fatalf("expected 1 node in callback, got %d", len(lastSnapshot))
	}

	nm.Leave("node-A")
	<-notifyCh
	if len(lastSnapshot) != 0 {
		t.Fatalf("expected 0 nodes in callback after leave, got %d", len(lastSnapshot))
	}
}

func TestNodeManagerRing(t *testing.T) {
	r := NewRing()
	nm := NewNodeManager(r, NodeManagerOptions{})
	nm.Join("node-A")
	if nm.Ring() != r {
		t.Fatal("Ring() should return the same ring pointer")
	}
}

func TestNodeManagerHealthyNodes(t *testing.T) {
	r := NewRing()
	nm := NewNodeManager(r, NodeManagerOptions{
		CheckInterval: 1 * time.Hour,
	})
	nm.Join("A")
	nm.Join("B")
	h := nm.HealthyNodes()
	sort.Strings(h)
	if len(h) != 2 || h[0] != "A" || h[1] != "B" {
		t.Fatalf("expected [A B], got %v", h)
	}
}

// ---------- Metrics tests ----------

func TestShardMetricsRecordAndSnapshot(t *testing.T) {
	m := NewShardMetrics("node-A")
	m.Record(10*time.Millisecond, false)
	m.Record(20*time.Millisecond, true)
	m.Record(30*time.Millisecond, false)

	snap := m.Snapshot()
	if snap.NodeID != "node-A" {
		t.Fatalf("expected node-A, got %q", snap.NodeID)
	}
	if snap.RequestCount != 3 {
		t.Fatalf("expected 3 requests, got %d", snap.RequestCount)
	}
	if snap.ErrorCount != 1 {
		t.Fatalf("expected 1 error, got %d", snap.ErrorCount)
	}
	// avg latency = (10+20+30)ms / 3 = 20ms
	if snap.AvgLatency != 20*time.Millisecond {
		t.Fatalf("expected 20ms avg latency, got %v", snap.AvgLatency)
	}
	// error rate = 1/3
	if snap.ErrorRate < 0.33 || snap.ErrorRate > 0.34 {
		t.Fatalf("expected error rate ~0.333, got %f", snap.ErrorRate)
	}
}

func TestShardMetricsReset(t *testing.T) {
	m := NewShardMetrics("node-A")
	m.Record(5*time.Millisecond, false)
	m.Record(10*time.Millisecond, true)
	m.Reset()

	snap := m.Snapshot()
	if snap.RequestCount != 0 || snap.ErrorCount != 0 || snap.AvgLatency != 0 {
		t.Fatalf("expected zeroed metrics after reset, got %+v", snap)
	}
}

func TestShardMetricsNodeID(t *testing.T) {
	m := NewShardMetrics("x")
	if m.NodeID() != "x" {
		t.Fatalf("expected x, got %q", m.NodeID())
	}
}

func TestCollectorRegisterAndSnapshot(t *testing.T) {
	c := NewCollector()
	ma := NewShardMetrics("A")
	mb := NewShardMetrics("B")
	ma.Record(5*time.Millisecond, false)
	mb.Record(10*time.Millisecond, true)

	c.Register("A", ma)
	c.Register("B", mb)

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 stats, got %d", len(snap))
	}

	// Unregister.
	c.Register("A", nil)
	snap = c.Snapshot()
	if len(snap) != 1 || snap[0].NodeID != "B" {
		t.Fatalf("expected only B after unregister, got %v", snap)
	}
}

func TestCollectorLeastLoaded(t *testing.T) {
	c := NewCollector()
	_, ok := c.LeastLoaded()
	if ok {
		t.Fatal("expected false on empty collector")
	}

	ma := NewShardMetrics("A")
	ma.Record(0, false)
	ma.Record(0, false) // 2 requests
	mb := NewShardMetrics("B")
	mb.Record(0, false) // 1 request

	c.Register("A", ma)
	c.Register("B", mb)

	best, ok := c.LeastLoaded()
	if !ok || best != "B" {
		t.Fatalf("expected B as least loaded, got %q (ok=%v)", best, ok)
	}
}

func TestCollectorLeastLoadedTieBreak(t *testing.T) {
	c := NewCollector()
	ma := NewShardMetrics("A")
	mb := NewShardMetrics("B")
	// Both have 0 requests — any result is fine, just don't panic.
	c.Register("A", ma)
	c.Register("B", mb)
	_, ok := c.LeastLoaded()
	if !ok {
		t.Fatal("expected a result when tied")
	}
}

func TestShardMetricsZeroErrorRate(t *testing.T) {
	m := NewShardMetrics("x")
	snap := m.Snapshot()
	if snap.ErrorRate != 0 {
		t.Fatalf("expected 0 error rate, got %f", snap.ErrorRate)
	}
	if snap.AvgLatency != 0 {
		t.Fatalf("expected 0 avg latency, got %v", snap.AvgLatency)
	}
}

func TestShardMetricsConcurrent(t *testing.T) {
	m := NewShardMetrics("x")
	const goroutines = 10
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				m.Record(time.Duration(j)*time.Nanosecond, j%2 == 0)
			}
		}()
	}
	wg.Wait()

	snap := m.Snapshot()
	if snap.RequestCount != uint64(goroutines*iters) {
		t.Fatalf("expected %d requests, got %d", goroutines*iters, snap.RequestCount)
	}
}

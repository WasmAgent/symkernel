package sharding

import (
	"sync"
	"sync/atomic"
	"time"
)

// ShardMetrics tracks per-shard counters for load-balancing decisions.
// Each node in the cluster owns one ShardMetrics instance. All fields
// are read via atomic loads so the hot path (Record) stays lock-free.
type ShardMetrics struct {
	nodeID string

	requestCount  uint64 // total requests assigned to this shard
	errorCount    uint64 // requests that returned an error
	totalLatency  uint64 // cumulative latency in nanoseconds
	sampleCount   uint64 // number of latency samples

	// lastUpdatedAt is the wall-clock time of the most recent Record.
	// It is not atomic; it is read and written under the optional
	// external synchronisation used by the collector.
	lastUpdatedAt time.Time
}

// NewShardMetrics creates a new per-shard metrics collector.
func NewShardMetrics(nodeID string) *ShardMetrics {
	return &ShardMetrics{nodeID: nodeID}
}

// NodeID returns the node this metrics instance tracks.
func (m *ShardMetrics) NodeID() string {
	return m.nodeID
}

// Record increments the request counter and, if error is true, the error
// counter. If latency >= 0 it accumulates the latency sample.
func (m *ShardMetrics) Record(latency time.Duration, isError bool) {
	atomic.AddUint64(&m.requestCount, 1)
	if isError {
		atomic.AddUint64(&m.errorCount, 1)
	}
	if latency >= 0 {
		atomic.AddUint64(&m.totalLatency, uint64(latency))
		atomic.AddUint64(&m.sampleCount, 1)
	}
	m.lastUpdatedAt = time.Now()
}

// Snapshot returns a point-in-time copy of the metrics safe for reading
// without further synchronisation.
func (m *ShardMetrics) Snapshot() ShardStats {
	rc := atomic.LoadUint64(&m.requestCount)
	ec := atomic.LoadUint64(&m.errorCount)
	tl := atomic.LoadUint64(&m.totalLatency)
	sc := atomic.LoadUint64(&m.sampleCount)
	return ShardStats{
		NodeID:        m.nodeID,
		RequestCount:  rc,
		ErrorCount:    ec,
		TotalLatency:  tl,
		SampleCount:   sc,
		ErrorRate:      errorRate(rc, ec),
		AvgLatency:    avgLatency(tl, sc),
		LastUpdatedAt: m.lastUpdatedAt,
	}
}

// Reset zeroes all counters. Useful between test cases.
func (m *ShardMetrics) Reset() {
	atomic.StoreUint64(&m.requestCount, 0)
	atomic.StoreUint64(&m.errorCount, 0)
	atomic.StoreUint64(&m.totalLatency, 0)
	atomic.StoreUint64(&m.sampleCount, 0)
}

// ShardStats is a read-only snapshot of a node's metrics.
type ShardStats struct {
	NodeID        string    `json:"node_id"`
	RequestCount  uint64    `json:"request_count"`
	ErrorCount    uint64    `json:"error_count"`
	TotalLatency  uint64    `json:"total_latency_ns"`
	SampleCount   uint64    `json:"sample_count"`
	ErrorRate     float64   `json:"error_rate"`
	AvgLatency    time.Duration `json:"avg_latency"`
	LastUpdatedAt time.Time `json:"last_updated_at"`
}

// Collector aggregates per-node metrics into a cluster-wide view.
// It is safe for concurrent use.
type Collector struct {
	mu      sync.RWMutex
	metrics map[string]*ShardMetrics
}

// NewCollector creates a metrics collector.
func NewCollector() *Collector {
	return &Collector{
		metrics: make(map[string]*ShardMetrics),
	}
}

// Register adds or replaces the metrics for a node. If metrics is nil
// the node is removed.
func (c *Collector) Register(nodeID string, metrics *ShardMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if metrics == nil {
		delete(c.metrics, nodeID)
		return
	}
	c.metrics[nodeID] = metrics
}

// Snapshot returns a cluster-wide snapshot of all registered nodes' metrics.
func (c *Collector) Snapshot() []ShardStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ShardStats, 0, len(c.metrics))
	for _, m := range c.metrics {
		out = append(out, m.Snapshot())
	}
	return out
}

// LeastLoaded returns the node ID with the fewest outstanding requests,
// or false if no nodes are registered.
func (c *Collector) LeastLoaded() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.metrics) == 0 {
		return "", false
	}
	var bestID string
	var bestCount uint64 = 1<<63 - 1
	for id, m := range c.metrics {
		rc := atomic.LoadUint64(&m.requestCount)
		if rc < bestCount {
			bestCount = rc
			bestID = id
		}
	}
	return bestID, true
}

func errorRate(requests, errors uint64) float64 {
	if requests == 0 {
		return 0
	}
	return float64(errors) / float64(requests)
}

func avgLatency(totalNs, samples uint64) time.Duration {
	if samples == 0 {
		return 0
	}
	return time.Duration(totalNs / samples)
}

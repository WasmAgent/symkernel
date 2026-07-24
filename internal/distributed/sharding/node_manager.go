package sharding

import (
	"sync"
	"time"
)

// NodeState is the health state of a cluster node.
type NodeState int

const (
	// StateHealthy indicates the node is participating normally.
	StateHealthy NodeState = iota
	// StateSuspect indicates the node missed one heartbeat but has not
	// yet been marked down.
	StateSuspect
	// StateDown indicates the node has missed enough heartbeats to be
	// removed from the ring.
	StateDown
)

func (s NodeState) String() string {
	switch s {
	case StateSuspect:
		return "suspect"
	case StateDown:
		return "down"
	default:
		return "healthy"
	}
}

// NodeInfo holds metadata for a single cluster node.
type NodeInfo struct {
	ID        string
	State     NodeState
	JoinedAt  time.Time
	LastSeen  time.Time
	ShardLoad int // number of keys currently assigned
}

// NodeManager wraps a Ring with node lifecycle tracking, health monitoring,
// and automatic removal of nodes that fail heartbeats. It is safe for
// concurrent use.
type NodeManager struct {
	mu     sync.RWMutex
	ring   *Ring
	nodes  map[string]*NodeInfo
	opts   NodeManagerOptions
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NodeManagerOptions configures a NodeManager.
type NodeManagerOptions struct {
	// SuspectAfter is the duration of silence before a node moves from
	// Healthy to Suspect. Zero defaults to 2 * CheckInterval.
	SuspectAfter time.Duration

	// DownAfter is the duration of silence before a Suspect node is
	// removed from the ring. Zero defaults to 3 * CheckInterval.
	DownAfter time.Duration

	// CheckInterval is how often the manager scans for stale nodes.
	// Zero defaults to 5 seconds.
	CheckInterval time.Duration

	// OnChange is an optional callback invoked after the ring membership
	// changes. The map is a snapshot; callers must not modify it.
	OnChange func(map[string]*NodeInfo)
}

// NewNodeManager creates a NodeManager with the given ring and options.
func NewNodeManager(ring *Ring, opts NodeManagerOptions) *NodeManager {
	if opts.CheckInterval == 0 {
		opts.CheckInterval = 5 * time.Second
	}
	if opts.SuspectAfter == 0 {
		opts.SuspectAfter = 2 * opts.CheckInterval
	}
	if opts.DownAfter == 0 {
		opts.DownAfter = 3 * opts.CheckInterval
	}
	return &NodeManager{
		ring:   ring,
		nodes:  make(map[string]*NodeInfo),
		opts:   opts,
		stopCh: make(chan struct{}),
	}
}

// Start begins the health-check goroutine. Call Stop to terminate it.
func (nm *NodeManager) Start() {
	nm.wg.Add(1)
	go nm.checkLoop()
}

// Stop terminates the health-check goroutine and waits for it to finish.
func (nm *NodeManager) Stop() {
	close(nm.stopCh)
	nm.wg.Wait()
}

// Join registers nodeID on the ring and begins tracking its health.
// If the node is already registered it resets its LastSeen to now.
func (nm *NodeManager) Join(nodeID string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	now := time.Now()
	if info, ok := nm.nodes[nodeID]; ok {
		info.State = StateHealthy
		info.LastSeen = now
		return
	}
	nm.ring.AddNode(nodeID)
	nm.nodes[nodeID] = &NodeInfo{
		ID:       nodeID,
		State:    StateHealthy,
		JoinedAt: now,
		LastSeen: now,
	}
	nm.notifyLocked()
}

// Leave immediately removes nodeID from the ring without waiting for
// health checks. It is used for graceful shutdown.
func (nm *NodeManager) Leave(nodeID string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	delete(nm.nodes, nodeID)
	nm.ring.RemoveNode(nodeID)
	nm.notifyLocked()
}

// Heartbeat updates the LastSeen timestamp for nodeID, keeping it alive.
// If the node is not registered it is automatically joined.
func (nm *NodeManager) Heartbeat(nodeID string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	if info, ok := nm.nodes[nodeID]; ok {
		info.LastSeen = time.Now()
		if info.State != StateHealthy {
			info.State = StateHealthy
		}
		return
	}
	// Auto-join on first heartbeat.
	nm.ring.AddNode(nodeID)
	now := time.Now()
	nm.nodes[nodeID] = &NodeInfo{
		ID:       nodeID,
		State:    StateHealthy,
		JoinedAt: now,
		LastSeen: now,
	}
	nm.notifyLocked()
}

// Nodes returns a snapshot of current node info.
func (nm *NodeManager) Nodes() map[string]*NodeInfo {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	out := make(map[string]*NodeInfo, len(nm.nodes))
	for id, info := range nm.nodes {
		cp := *info
		out[id] = &cp
	}
	return out
}

// Ring returns the underlying consistent-hashing ring for direct lookups.
func (nm *NodeManager) Ring() *Ring {
	return nm.ring
}

// HealthyNodes returns IDs of nodes currently in StateHealthy.
func (nm *NodeManager) HealthyNodes() []string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	var out []string
	for id, info := range nm.nodes {
		if info.State == StateHealthy {
			out = append(out, id)
		}
	}
	return out
}

func (nm *NodeManager) checkLoop() {
	defer nm.wg.Done()
	ticker := time.NewTicker(nm.opts.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-nm.stopCh:
			return
		case <-ticker.C:
			nm.checkHealth()
		}
	}
}

func (nm *NodeManager) checkHealth() {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	now := time.Now()
	changed := false
	for id, info := range nm.nodes {
		elapsed := now.Sub(info.LastSeen)
		switch info.State {
		case StateHealthy:
			if elapsed >= nm.opts.SuspectAfter {
				info.State = StateSuspect
			}
		case StateSuspect:
			if elapsed >= nm.opts.DownAfter {
				info.State = StateDown
				nm.ring.RemoveNode(id)
				delete(nm.nodes, id)
				changed = true
			}
		}
	}
	if changed {
		nm.notifyLocked()
	}
}

func (nm *NodeManager) notifyLocked() {
	if nm.opts.OnChange != nil {
		snap := make(map[string]*NodeInfo, len(nm.nodes))
		for id, info := range nm.nodes {
			cp := *info
			snap[id] = &cp
		}
		nm.opts.OnChange(snap)
	}
}

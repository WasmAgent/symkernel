// Package coordination implements Raft-based leader election and work
// distribution for a symkernel distributed cluster, the coordination
// primitive called out by Milestone 10.
//
// A cluster of three or more nodes elects a single leader using the Raft
// consensus algorithm's leader-election protocol: randomized election
// timeouts, monotonically increasing terms, and term-gated majority
// voting. Only the leader may accept and distribute verification work.
// The leader periodically refreshes its quorum via heartbeats and steps
// down the moment it can no longer reach a majority of peers, which
// prevents split brain under a network partition. When the leader fails
// or is partitioned away, the remaining majority times out and elects a
// replacement, yielding automatic failover.
//
// Inter-node messages flow through a Transport interface. An in-memory
// transport is provided for tests and single-process clusters; a
// production deployment carries these messages over HashiCorp Memberlist
// (the member-discovery and gossip layer named in the milestone) or any
// other reliable point-to-point transport. The coordination logic itself
// is transport agnostic: a remote node simply calls Receive on receipt of
// a wire message.
//
// This package implements the leader-election and work-distribution core.
// It does not replicate a state-machine log; the symkernel work queue
// (internal/distributed/workqueue) and sharding ring
// (internal/distributed/sharding) build on top of the leader this
// package elects.
package coordination

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Role is the Raft role of a cluster node.
type Role int

const (
	// Follower is the passive role: the node responds to leaders and
	// candidates but initiates nothing on its own.
	Follower Role = iota
	// Candidate is the role a node enters when its election timer fires;
	// it solicits votes in an attempt to become leader.
	Candidate
	// Leader is the node currently holding the lease for the term. Only
	// the leader accepts and distributes work.
	Leader
)

func (r Role) String() string {
	switch r {
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	default:
		return "follower"
	}
}

// MessageType identifies an inter-node RPC.
type MessageType int

const (
	MsgRequestVote MessageType = iota
	MsgRequestVoteResp
	MsgAppendEntries
	MsgAppendEntriesResp
)

// Message is the envelope exchanged between nodes.
type Message struct {
	Type        MessageType
	Term        uint64
	From        string // sending node ID
	To          string // receiving node ID
	VoteGranted bool   // RequestVoteResp only
	Success     bool   // AppendEntriesResp only
	LeaderID    string // AppendEntries (heartbeat) only
}

// Transport delivers a Message to the node identified by m.To.
type Transport interface {
	Send(to string, m Message) error
}

// WorkItem is a unit of verification work the leader distributes across
// the cluster. The coordination package treats items opaquely; it only
// decides which node should handle each item.
type WorkItem struct {
	ID   string
	Tier string // "cel", "wazero", or "z3" (informational)
	Data []byte
}

// ErrNotLeader is returned when a non-leader node is asked to accept or
// distribute work.
var ErrNotLeader = errors.New("coordination: not leader")

const (
	defaultHeartbeatTicks  = 1
	defaultElectionMin     = 8
	defaultElectionMax     = 15
	defaultQuorumLossTicks = 5
	defaultQuorumAckTicks  = 3
	defaultTickInterval    = 50 * time.Millisecond
)

// NodeConfig configures a single cluster node.
type NodeConfig struct {
	// ID is the unique identifier of this node.
	ID string
	// Peers lists the IDs of the other cluster members (excluding ID).
	Peers []string

	// HeartbeatTicks is the interval, in ticks, between leader
	// heartbeats. Defaults to 1.
	HeartbeatTicks int
	// ElectionTicksMin and ElectionTicksMax bound the randomized
	// election timeout in ticks. A node becomes a candidate once its
	// election timer reaches a random value in this range. Defaults to
	// 8..15, comfortably above HeartbeatTicks so followers do not
	// spuriously start elections while a healthy leader is present.
	ElectionTicksMin int
	ElectionTicksMax int
	// QuorumLossTicks is how many consecutive ticks a leader may go
	// without confirming a majority before stepping down. Defaults to 5.
	QuorumLossTicks int
	// QuorumAckTicks is the freshness window, in ticks, within which a
	// peer heartbeat acknowledgement counts toward the leader's quorum.
	// Defaults to 3.
	QuorumAckTicks int

	// TickInterval is the real-time duration represented by one Tick
	// when the node is driven by Start. Defaults to 50ms.
	TickInterval time.Duration

	// Rand sources the randomized election timeout. Each node should
	// use its own generator so concurrently running nodes do not share
	// mutable state. If nil, the process-global generator is used.
	Rand *rand.Rand
}

// Node is a single participant in the coordination cluster.
type Node struct {
	cfg       NodeConfig
	transport Transport

	mu sync.Mutex

	id      string
	peers   []string
	members []string // sorted self + peers

	role            Role
	currentTerm     uint64
	votedFor        string
	leaderID        string
	electionTimeout int
	electionElapsed int

	// candidate state
	votes map[string]bool

	// leader state
	heartbeatElapsed int
	acks             map[string]uint64 // peer ID -> tick of last heartbeat ack
	quorumMissTicks  int

	// work distribution (leader only)
	rrIndex  int
	nodeLoad map[string]int

	tick   uint64
	inbox  []Message
	outbox []Message

	// driver goroutine
	running bool
	stopCh  chan struct{}
}

// NewNode creates a node with the given configuration and transport.
// Zero or non-positive timing fields fall back to package defaults.
func NewNode(cfg NodeConfig, transport Transport) *Node {
	if cfg.HeartbeatTicks <= 0 {
		cfg.HeartbeatTicks = defaultHeartbeatTicks
	}
	if cfg.ElectionTicksMin <= 0 {
		cfg.ElectionTicksMin = defaultElectionMin
	}
	if cfg.ElectionTicksMax <= cfg.ElectionTicksMin {
		// Preserve the default range width so randomised election timeouts
		// spread across a meaningful interval when the caller sets only Min.
		cfg.ElectionTicksMax = cfg.ElectionTicksMin + (defaultElectionMax - defaultElectionMin)
	}
	if cfg.QuorumLossTicks <= 0 {
		cfg.QuorumLossTicks = defaultQuorumLossTicks
	}
	if cfg.QuorumAckTicks <= 0 {
		cfg.QuorumAckTicks = defaultQuorumAckTicks
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = defaultTickInterval
	}
	members := append([]string{cfg.ID}, cfg.Peers...)
	sort.Strings(members)

	return &Node{
		cfg:             cfg,
		transport:       transport,
		id:              cfg.ID,
		peers:           append([]string(nil), cfg.Peers...),
		members:         members,
		role:            Follower,
		nodeLoad:        make(map[string]int),
		rrIndex:         len(members) - 1,
		electionTimeout: pickTimeout(cfg),
	}
}

func pickTimeout(cfg NodeConfig) int {
	lo := cfg.ElectionTicksMin
	hi := cfg.ElectionTicksMax
	if hi <= lo {
		return lo
	}
	span := hi - lo
	if cfg.Rand != nil {
		return lo + cfg.Rand.IntN(span)
	}
	return lo + rand.IntN(span)
}

// ID returns the node's identifier.
func (n *Node) ID() string { return n.id }

// Role returns the node's current Raft role.
func (n *Node) Role() Role {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.role
}

// LeaderID returns the ID of the node this node currently recognizes as
// leader, or the empty string if none is known.
func (n *Node) LeaderID() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.leaderID
}

// Term returns the node's current term.
func (n *Node) Term() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentTerm
}

// Receive accepts an inbound message from the transport. It is the
// entry point a production transport calls when a message arrives over
// the wire. Both Receive and Tick hold n.mu when touching n.inbox, so
// there is no data race. Appending to a nil slice in Go is well-defined
// (allocates a new backing array) — no message is ever lost even if Tick
// sets n.inbox = nil just before a concurrent Receive call arrives.
func (n *Node) Receive(m Message) {
	n.mu.Lock()
	n.inbox = append(n.inbox, m)
	n.mu.Unlock()
}

// Tick advances the node's election and heartbeat state machines by one
// tick, draining queued inbound messages first. Production callers drive
// Tick from Start's background goroutine; tests call it directly
// (typically via Cluster.TickN) for deterministic, timing-free coverage.
//
// Outbound messages generated while processing the tick are buffered and
// flushed after the node's mutex is released, so a node never holds its
// own lock while delivering to a peer. This avoids lock-ordering
// deadlocks when nodes run concurrently under Start.
func (n *Node) Tick() {
	n.mu.Lock()
	n.tick++
	inbox := n.inbox
	n.inbox = nil
	for _, m := range inbox {
		n.handleLocked(m)
	}
	n.stepTimersLocked()
	outbox := n.outbox
	n.outbox = nil
	n.mu.Unlock()
	for _, m := range outbox {
		_ = n.transport.Send(m.To, m)
	}
}

// Start launches a background goroutine that drives Tick at the node's
// TickInterval. Calling Start more than once is a no-op. Use Stop to
// halt the goroutine.
func (n *Node) Start() {
	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		return
	}
	n.running = true
	n.stopCh = make(chan struct{})
	interval := n.cfg.TickInterval
	n.mu.Unlock()
	go n.run(interval)
}

func (n *Node) run(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-n.stopCh:
			return
		case <-t.C:
			n.Tick()
		}
	}
}

// Stop halts the background goroutine started by Start. It is safe to
// call on a node that was never started.
func (n *Node) Stop() {
	n.mu.Lock()
	if !n.running {
		n.mu.Unlock()
		return
	}
	n.running = false
	close(n.stopCh)
	n.mu.Unlock()
}

// Assign distributes a single work item to a cluster member and returns
// the assigned node's ID. Distribution is round-robin across the full
// member set (leader and peers). Only the leader may assign work; a
// non-leader returns ErrNotLeader.
func (n *Node) Assign(item WorkItem) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.role != Leader {
		return "", ErrNotLeader
	}
	n.rrIndex = (n.rrIndex + 1) % len(n.members)
	target := n.members[n.rrIndex]
	n.nodeLoad[target]++
	return target, nil
}

// NodeLoad returns a snapshot of the number of work items this leader
// has assigned to each cluster member. The map always contains every
// member, with zero for members that have received no work.
func (n *Node) NodeLoad() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]int, len(n.members))
	for _, m := range n.members {
		out[m] = n.nodeLoad[m]
	}
	return out
}

// handleLocked dispatches an inbound message to the appropriate handler.
func (n *Node) handleLocked(m Message) {
	switch m.Type {
	case MsgRequestVote:
		n.handleRequestVoteLocked(m)
	case MsgRequestVoteResp:
		n.handleRequestVoteRespLocked(m)
	case MsgAppendEntries:
		n.handleAppendEntriesLocked(m)
	case MsgAppendEntriesResp:
		n.handleAppendEntriesRespLocked(m)
	}
}

// stepTimersLocked advances the role-specific timer. Caller holds n.mu.
func (n *Node) stepTimersLocked() {
	switch n.role {
	case Follower, Candidate:
		n.electionElapsed++
		if n.electionElapsed >= n.electionTimeout {
			n.startElectionLocked()
		}
	case Leader:
		n.heartbeatElapsed++
		if n.heartbeatElapsed >= n.cfg.HeartbeatTicks {
			n.broadcastHeartbeatLocked()
			n.heartbeatElapsed = 0
		}
		if n.hasQuorumLocked() {
			n.quorumMissTicks = 0
		} else {
			n.quorumMissTicks++
			if n.quorumMissTicks >= n.cfg.QuorumLossTicks {
				// Lost contact with a majority: step down to prevent
				// two leaders (split brain) in disjoint partitions.
				n.becomeFollowerLocked(n.currentTerm)
			}
		}
	}
}

// startElectionLocked transitions a node into the candidate role for a
// new term and solicits votes from all peers. Caller holds n.mu.
func (n *Node) startElectionLocked() {
	n.role = Candidate
	n.currentTerm++
	n.votedFor = n.id
	n.leaderID = ""
	n.votes = map[string]bool{n.id: true}
	n.electionTimeout = pickTimeout(n.cfg)
	n.electionElapsed = 0
	for _, peer := range n.peers {
		n.sendLocked(Message{
			Type: MsgRequestVote,
			Term: n.currentTerm,
			From: n.id,
			To:   peer,
		})
	}
	// A single-node cluster satisfies the quorum immediately.
	if len(n.votes) >= majorityOf(len(n.members)) {
		n.becomeLeaderLocked()
	}
}

// handleRequestVoteLocked grants or denies a vote. Caller holds n.mu.
func (n *Node) handleRequestVoteLocked(m Message) {
	if m.Term > n.currentTerm {
		// A higher term always wins; update and become follower so we
		// can vote in the new term.
		n.currentTerm = m.Term
		n.role = Follower
		n.votedFor = ""
		n.leaderID = ""
	}
	granted := false
	if m.Term == n.currentTerm && (n.votedFor == "" || n.votedFor == m.From) {
		n.votedFor = m.From
		n.leaderID = ""
		n.electionElapsed = 0
		granted = true
	}
	n.sendLocked(Message{
		Type:        MsgRequestVoteResp,
		Term:        n.currentTerm,
		From:        n.id,
		To:          m.From,
		VoteGranted: granted,
	})
}

// handleRequestVoteRespLocked records a vote and, on reaching a majority,
// promotes the candidate to leader. Caller holds n.mu.
func (n *Node) handleRequestVoteRespLocked(m Message) {
	if m.Term > n.currentTerm {
		n.becomeFollowerLocked(m.Term)
		return
	}
	if n.role != Candidate || m.Term != n.currentTerm || !m.VoteGranted {
		return
	}
	n.votes[m.From] = true
	if len(n.votes) >= majorityOf(len(n.members)) {
		n.becomeLeaderLocked()
	}
}

// handleAppendEntriesLocked processes a leader heartbeat. Caller holds
// n.mu.
func (n *Node) handleAppendEntriesLocked(m Message) {
	if m.Term < n.currentTerm {
		// Stale leader: refuse and reveal our term so it steps down.
		n.sendLocked(Message{
			Type:    MsgAppendEntriesResp,
			Term:    n.currentTerm,
			From:    n.id,
			To:      m.From,
			Success: false,
		})
		return
	}
	if m.Term > n.currentTerm {
		n.currentTerm = m.Term
		n.votedFor = ""
	}
	n.role = Follower
	n.leaderID = m.LeaderID
	n.electionElapsed = 0
	n.sendLocked(Message{
		Type:    MsgAppendEntriesResp,
		Term:    n.currentTerm,
		From:    n.id,
		To:      m.LeaderID,
		Success: true,
	})
}

// handleAppendEntriesRespLocked records a peer's heartbeat
// acknowledgement, or steps down if the peer reports a higher term.
// Caller holds n.mu.
func (n *Node) handleAppendEntriesRespLocked(m Message) {
	if m.Term > n.currentTerm {
		n.becomeFollowerLocked(m.Term)
		return
	}
	if n.role != Leader || m.Term != n.currentTerm {
		return
	}
	if m.Success {
		n.acks[m.From] = n.tick
	}
}

// becomeLeaderLocked promotes the node to leader and seeds an immediate
// heartbeat to establish authority. Caller holds n.mu.
func (n *Node) becomeLeaderLocked() {
	n.role = Leader
	n.leaderID = n.id
	n.votedFor = n.id
	n.acks = map[string]uint64{n.id: n.tick}
	n.quorumMissTicks = 0
	n.heartbeatElapsed = n.cfg.HeartbeatTicks // force immediate broadcast
}

// becomeFollowerLocked demotes the node to follower for the given term.
// Caller holds n.mu.
func (n *Node) becomeFollowerLocked(term uint64) {
	n.role = Follower
	n.currentTerm = term
	n.leaderID = ""
	n.votedFor = ""
	n.votes = nil
	n.acks = nil
	n.electionElapsed = 0
}

// broadcastHeartbeatLocked sends an empty AppendEntries (heartbeat) to
// every peer. Caller holds n.mu.
func (n *Node) broadcastHeartbeatLocked() {
	for _, peer := range n.peers {
		n.sendLocked(Message{
			Type:     MsgAppendEntries,
			Term:     n.currentTerm,
			From:     n.id,
			To:       peer,
			LeaderID: n.id,
		})
	}
}

// hasQuorumLocked reports whether the leader has heard from a majority
// of the cluster (including itself) within the freshness window. Caller
// holds n.mu.
func (n *Node) hasQuorumLocked() bool {
	count := 1 // self
	for _, peer := range n.peers {
		if t, ok := n.acks[peer]; ok && n.tick-t <= uint64(n.cfg.QuorumAckTicks) {
			count++
		}
	}
	return count >= majorityOf(len(n.members))
}

// sendLocked buffers an outbound message for delivery after the node's
// mutex is released. Caller holds n.mu.
func (n *Node) sendLocked(m Message) {
	n.outbox = append(n.outbox, m)
}

// majorityOf returns the smallest majority of n: n/2 + 1.
func majorityOf(n int) int { return n/2 + 1 }

// memTransport is an in-memory Transport that routes messages between
// nodes in the same process. It supports partitioning for failure
// testing: two endpoints in different partition groups cannot exchange
// messages.
type memTransport struct {
	mu     sync.Mutex
	nodes  map[string]*Node
	groups map[string]int // node ID -> partition group; 0 means connected
}

// Send delivers m to its target node unless a partition separates the
// sender and receiver, in which case the message is silently dropped.
func (t *memTransport) Send(to string, m Message) error {
	t.mu.Lock()
	if t.groups[m.From] != t.groups[to] {
		t.mu.Unlock()
		return nil
	}
	node := t.nodes[to]
	t.mu.Unlock()
	if node == nil {
		return fmt.Errorf("coordination: unknown node %q", to)
	}
	node.Receive(m)
	return nil
}

// Cluster wires a set of Nodes together over an in-memory transport. It
// is the primary test fixture and a usable single-process cluster.
type Cluster struct {
	transport    *memTransport
	nodes        map[string]*Node
	order        []string
	tickInterval time.Duration
	seed         uint64
}

// ClusterOption configures a Cluster.
type ClusterOption func(*Cluster)

// WithSeed makes election timeouts deterministic across runs by seeding
// the per-node random generators derived from this value.
func WithSeed(seed uint64) ClusterOption {
	return func(c *Cluster) { c.seed = seed }
}

// WithTickInterval overrides the TickInterval applied to every node when
// Start is called.
func WithTickInterval(d time.Duration) ClusterOption {
	return func(c *Cluster) { c.tickInterval = d }
}

// NewCluster creates a cluster of the given node IDs, fully connected
// over an in-memory transport. All nodes begin in the follower role.
func NewCluster(ids []string, opts ...ClusterOption) *Cluster {
	c := &Cluster{
		transport:    &memTransport{nodes: map[string]*Node{}, groups: map[string]int{}},
		nodes:        map[string]*Node{},
		tickInterval: defaultTickInterval,
		seed:         1,
	}
	for _, o := range opts {
		o(c)
	}
	for i, id := range ids {
		peers := make([]string, 0, len(ids)-1)
		for _, other := range ids {
			if other != id {
				peers = append(peers, other)
			}
		}
		sort.Strings(peers)
		// Each node gets its own generator so concurrently running
		// nodes never share mutable random state.
		rng := rand.New(rand.NewPCG(c.seed, uint64(i)+1))
		node := NewNode(NodeConfig{
			ID:           id,
			Peers:        peers,
			Rand:         rng,
			TickInterval: c.tickInterval,
		}, c.transport)
		c.nodes[id] = node
		c.order = append(c.order, id)
		c.transport.nodes[id] = node
	}
	return c
}

// Node returns the cluster member with the given ID.
func (c *Cluster) Node(id string) *Node { return c.nodes[id] }

// Nodes returns the cluster members in declaration order.
func (c *Cluster) Nodes() []*Node {
	out := make([]*Node, 0, len(c.order))
	for _, id := range c.order {
		out = append(out, c.nodes[id])
	}
	return out
}

// TickAll advances every node by one tick, in declaration order.
func (c *Cluster) TickAll() {
	for _, id := range c.order {
		c.nodes[id].Tick()
	}
}

// TickN runs TickAll n times.
func (c *Cluster) TickN(n int) {
	for i := 0; i < n; i++ {
		c.TickAll()
	}
}

// Leader returns the cluster's current leader, or nil if there is none
// or if more than one node believes itself to be leader (split brain).
func (c *Cluster) Leader() *Node {
	var leader *Node
	for _, id := range c.order {
		if c.nodes[id].Role() == Leader {
			if leader != nil {
				return nil // split brain
			}
			leader = c.nodes[id]
		}
	}
	return leader
}

// Start launches the background tick goroutine for every node.
func (c *Cluster) Start() {
	for _, n := range c.Nodes() {
		n.Start()
	}
}

// Stop halts every node's background goroutine.
func (c *Cluster) Stop() {
	for _, n := range c.Nodes() {
		n.Stop()
	}
}

// Partition splits the cluster into two disjoint groups that can no
// longer exchange messages; messages within a group still flow. It is
// the primary tool for exercising split-brain prevention and failover.
func (c *Cluster) Partition(groupA, groupB []string) {
	t := c.transport
	t.mu.Lock()
	defer t.mu.Unlock()
	t.groups = map[string]int{}
	for _, id := range groupA {
		t.groups[id] = 1
	}
	for _, id := range groupB {
		t.groups[id] = 2
	}
}

// Isolate drops all messages to and from id, simulating a node that has
// crashed or lost all connectivity while remaining in the member list.
func (c *Cluster) Isolate(id string) {
	t := c.transport
	t.mu.Lock()
	defer t.mu.Unlock()
	t.groups[id] = 99
}

// Heal restores full connectivity between all nodes.
func (c *Cluster) Heal() {
	t := c.transport
	t.mu.Lock()
	defer t.mu.Unlock()
	t.groups = map[string]int{}
}

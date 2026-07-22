package coordination

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestRoleString(t *testing.T) {
	cases := map[Role]string{
		Follower:  "follower",
		Candidate: "candidate",
		Leader:    "leader",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("role %d String() = %q, want %q", r, got, want)
		}
	}
}

func TestSingleNodeElectsSelf(t *testing.T) {
	c := NewCluster([]string{"a"}, WithSeed(1))
	c.TickN(20)
	if c.Leader() == nil {
		t.Fatal("expected the single node to elect itself leader")
	}
}

func TestThreeNodesElectExactlyOneLeader(t *testing.T) {
	c := NewCluster([]string{"a", "b", "c"}, WithSeed(7))
	c.TickN(60)
	lead := c.Leader()
	if lead == nil {
		t.Fatal("expected exactly one leader; got none (or split brain)")
	}
	// Every follower must recognize the elected leader.
	for _, n := range c.Nodes() {
		if n.ID() == lead.ID() {
			continue
		}
		if n.LeaderID() != lead.ID() {
			t.Errorf("node %q recognizes leader %q, want %q", n.ID(), n.LeaderID(), lead.ID())
		}
		if n.Role() == Leader {
			t.Errorf("node %q also considers itself leader (split brain)", n.ID())
		}
	}
}

func TestFiveNodesElectExactlyOneLeader(t *testing.T) {
	// The milestone requires 3+ node clusters; exercise a 5-node ring.
	c := NewCluster([]string{"a", "b", "c", "d", "e"}, WithSeed(13))
	c.TickN(80)
	if c.Leader() == nil {
		t.Fatal("expected exactly one leader in 5-node cluster")
	}
}

func TestLeaderDistributesWorkRoundRobin(t *testing.T) {
	c := NewCluster([]string{"a", "b", "c"}, WithSeed(3))
	c.TickN(60)
	lead := c.Leader()
	if lead == nil {
		t.Fatal("no leader elected")
	}

	// A non-leader must refuse to assign work.
	follower := firstFollower(c, lead)
	if follower == nil {
		t.Fatal("expected at least one follower")
	}
	if _, err := follower.Assign(WorkItem{ID: "x"}); !errors.Is(err, ErrNotLeader) {
		t.Errorf("non-leader Assign err = %v, want ErrNotLeader", err)
	}

	// The leader distributes work round-robin across all members.
	for i := 0; i < 6; i++ {
		got, err := lead.Assign(WorkItem{ID: strconv.Itoa(i), Tier: "cel"})
		if err != nil {
			t.Fatalf("Assign #%d: %v", i, err)
		}
		if got == "" {
			t.Fatalf("Assign #%d returned empty node ID", i)
		}
	}

	load := lead.NodeLoad()
	for _, id := range []string{"a", "b", "c"} {
		if load[id] != 2 {
			t.Errorf("node %q load = %d, want 2 (round-robin over 3 members, 6 items)", id, load[id])
		}
	}
}

func TestAutomaticFailover(t *testing.T) {
	c := NewCluster([]string{"a", "b", "c"}, WithSeed(11))
	c.TickN(60)
	old := c.Leader()
	if old == nil {
		t.Fatal("no initial leader")
	}

	// The leader disappears (isolated). The remaining majority must
	// elect a replacement, and the isolated node must step down.
	c.Isolate(old.ID())
	c.TickN(100)

	lead := c.Leader()
	if lead == nil {
		t.Fatal("no leader elected after failover (or split brain)")
	}
	if lead.ID() == old.ID() {
		t.Fatalf("isolated leader %q is still leader", old.ID())
	}
	if c.Node(old.ID()).Role() == Leader {
		t.Errorf("isolated node %q did not step down after losing quorum", old.ID())
	}
}

func TestSplitBrainMinorityCannotElect(t *testing.T) {
	// A 1-vs-4 partition: the lone minority node can never reach a
	// majority of 5 (which is 3) and must not elect itself.
	c := NewCluster([]string{"a", "b", "c", "d", "e"}, WithSeed(5))
	c.TickN(60)
	c.Partition([]string{"a"}, []string{"b", "c", "d", "e"})
	c.TickN(100)

	if c.Node("a").Role() == Leader {
		t.Fatal("minority node 'a' became leader — split brain")
	}
	lead := c.Leader()
	if lead == nil {
		t.Fatal("expected the majority side to elect a leader")
	}
	if lead.ID() == "a" {
		t.Fatal("leader must come from the majority side, not the isolated node")
	}
}

func TestBalancedPartitionYieldsNoLeaderThenHeals(t *testing.T) {
	// A 2-vs-2 partition in a 4-node cluster: neither side holds a
	// majority (3), so no leader may exist. After healing, the cluster
	// must recover a single leader.
	c := NewCluster([]string{"a", "b", "c", "d"}, WithSeed(9))
	c.TickN(60)
	c.Partition([]string{"a", "b"}, []string{"c", "d"})
	c.TickN(100)

	if lead := c.Leader(); lead != nil {
		// Any lingering leader must also have stepped down for lack of
		// quorum; a surviving leader here is a split-brain bug.
		t.Fatalf("node %q remained leader without a quorum — split brain", lead.ID())
	}

	c.Heal()
	c.TickN(100)
	if c.Leader() == nil {
		t.Fatal("no leader elected after healing the partition")
	}
}

func TestStartStopElectsLeader(t *testing.T) {
	c := NewCluster([]string{"a", "b", "c"}, WithSeed(2), WithTickInterval(5*time.Millisecond))
	c.Start()
	defer c.Stop()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.Leader() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected within timeout under real-time driver")
}

// firstFollower returns the first cluster node that is not the given
// leader.
func firstFollower(c *Cluster, leader *Node) *Node {
	for _, n := range c.Nodes() {
		if n.ID() != leader.ID() {
			return n
		}
	}
	return nil
}

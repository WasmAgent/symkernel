package governance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// --- SchemaVersion ---

func TestSchemaVersionString(t *testing.T) {
	v := SchemaVersion{Major: 2, Minor: 1, Patch: 0}
	if got := v.String(); got != "2.1.0" {
		t.Errorf("String() = %q, want %q", got, "2.1.0")
	}
}

func TestParseSchemaVersion(t *testing.T) {
	tests := []struct {
		input string
		want  SchemaVersion
		err   bool
	}{
		{"1.0.0", SchemaVersion{1, 0, 0}, false},
		{"0.0.1", SchemaVersion{0, 0, 1}, false},
		{"10.20.30", SchemaVersion{10, 20, 30}, false},
		{"bad", SchemaVersion{}, true},
		{"1.0", SchemaVersion{}, true},
		{"-1.0.0", SchemaVersion{}, true},
		{"", SchemaVersion{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseSchemaVersion(tt.input)
			if (err != nil) != tt.err {
				t.Errorf("ParseSchemaVersion(%q) error = %v, want err %v", tt.input, err, tt.err)
			}
			if !tt.err && got != tt.want {
				t.Errorf("ParseSchemaVersion(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- SchemaRegistry ---

func TestSchemaRegistryRegisterAndGet(t *testing.T) {
	r := NewSchemaRegistry()
	s := PolicySchema{
		ID:         "cel-v1",
		Version:    SchemaVersion{1, 0, 0},
		SchemaType: SchemaTypeCEL,
	}
	if err := r.Register(s); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	got, ok := r.Get("cel-v1", SchemaVersion{1, 0, 0})
	if !ok {
		t.Fatal("Get returned false")
	}
	if got.ID != "cel-v1" {
		t.Errorf("got ID %q, want %q", got.ID, "cel-v1")
	}
}

func TestSchemaRegistryDuplicate(t *testing.T) {
	r := NewSchemaRegistry()
	s := PolicySchema{ID: "x", Version: SchemaVersion{1, 0, 0}, SchemaType: SchemaTypeCEL}
	if err := r.Register(s); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register(s); err == nil {
		t.Fatal("expected error on duplicate register")
	}
}

func TestSchemaRegistryLatest(t *testing.T) {
	r := NewSchemaRegistry()
	for _, v := range []SchemaVersion{{1, 0, 0}, {1, 2, 0}, {1, 1, 0}} {
		_ = r.Register(PolicySchema{ID: "my-policy", Version: v, SchemaType: SchemaTypeCEL})
	}
	got, ok := r.Latest("my-policy")
	if !ok {
		t.Fatal("Latest returned false")
	}
	if got.Version != (SchemaVersion{1, 2, 0}) {
		t.Errorf("Latest version = %v, want 1.2.0", got.Version)
	}
}

func TestSchemaRegistryList(t *testing.T) {
	r := NewSchemaRegistry()
	_ = r.Register(PolicySchema{ID: "a", Version: SchemaVersion{1, 0, 0}, SchemaType: SchemaTypeCEL})
	_ = r.Register(PolicySchema{ID: "a", Version: SchemaVersion{2, 0, 0}, SchemaType: SchemaTypeCEL})
	_ = r.Register(PolicySchema{ID: "b", Version: SchemaVersion{1, 0, 0}, SchemaType: SchemaTypeZ3})

	all := r.List("")
	if len(all) != 3 {
		t.Errorf("List('') = %d entries, want 3", len(all))
	}
	filtered := r.List("a")
	if len(filtered) != 2 {
		t.Errorf("List('a') = %d entries, want 2", len(filtered))
	}
	// Sorted descending
	if !versionGt(filtered[0].Version, filtered[1].Version) {
		t.Error("List not sorted descending")
	}
}

func TestSchemaRegistryMigrationChain(t *testing.T) {
	r := NewSchemaRegistry()
	prev := &SchemaVersion{Major: 1, Minor: 0, Patch: 0}
	v2 := SchemaVersion{Major: 1, Minor: 1, Patch: 0}
	v3 := SchemaVersion{Major: 2, Minor: 0, Patch: 0}
	_ = r.Register(PolicySchema{ID: "p", Version: SchemaVersion{1, 0, 0}, SchemaType: SchemaTypeCEL})
	_ = r.Register(PolicySchema{ID: "p", Version: v2, PreviousVersion: prev, SchemaType: SchemaTypeCEL})
	v2Ptr := &SchemaVersion{Major: 1, Minor: 1, Patch: 0}
	_ = r.Register(PolicySchema{ID: "p", Version: v3, PreviousVersion: v2Ptr, SchemaType: SchemaTypeCEL})

	chain := r.MigrationChain("p", v3)
	if len(chain) != 3 {
		t.Fatalf("MigrationChain length = %d, want 3", len(chain))
	}
	if chain[0].Version != v3 {
		t.Errorf("chain[0].Version = %v, want 2.0.0", chain[0].Version)
	}
	if chain[2].Version != (SchemaVersion{1, 0, 0}) {
		t.Errorf("chain[2].Version = %v, want 1.0.0", chain[2].Version)
	}
}

// --- PolicyContent ---

func TestPolicyContentFingerprint(t *testing.T) {
	c := PolicyContent{SHA256: strings.Repeat("a", 64)}
	if got := c.Fingerprint(); got != "aaaaaaaaaaaa" {
		t.Errorf("Fingerprint() = %q, want 12 a's", got)
	}
	short := PolicyContent{SHA256: "abc"}
	if got := short.Fingerprint(); got != "abc" {
		t.Errorf("Fingerprint() short = %q, want %q", got, "abc")
	}
}

func TestValidateContentType(t *testing.T) {
	valid := []string{"application/cel", "application/smtlib2", "application/composed-policy", "application/wasm"}
	for _, ct := range valid {
		if err := ValidateContentType(ct); err != nil {
			t.Errorf("ValidateContentType(%q) unexpected error: %v", ct, err)
		}
	}
	if err := ValidateContentType("text/plain"); err == nil {
		t.Error("expected error for text/plain")
	}
}

// --- Lifecycle ---

func makeTestPolicy(id string, v SchemaVersion) *Policy {
	raw := []byte(fmt.Sprintf("policy %s@%s", id, v.String()))
	h := sha256.Sum256(raw)
	content := PolicyContent{
		Raw:         raw,
		ContentType: "application/cel",
		SHA256:      hex.EncodeToString(h[:]),
	}
	return NewPolicy(id, v, content, "cel-v1")
}

func TestLifecycleHappyPath(t *testing.T) {
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})

	steps := []struct {
		to     LifecycleState
		actor  string
		reason string
	}{
		{StatePendingApproval, "author", "ready for review"},
		{StateApproved, "reviewer-a", "looks good"},
		{StateStaged, "ops", "deploying to staging"},
		{StateDeployed, "ops", "promoting to prod"},
		{StateDeprecated, "ops", "superseded by v2"},
	}

	for _, step := range steps {
		rec, err := p.Transition(step.to, step.actor, step.reason)
		if err != nil {
			t.Fatalf("transition to %q: %v", step.to, err)
		}
		if rec.To != step.to {
			t.Errorf("rec.To = %q, want %q", rec.To, step.to)
		}
		if rec.Actor != step.actor {
			t.Errorf("rec.Actor = %q, want %q", rec.Actor, step.actor)
		}
	}

	if !p.IsTerminal() {
		t.Error("Deprecated should be terminal")
	}
}

func TestLifecycleInvalidTransition(t *testing.T) {
	p := makeTestPolicy("x", SchemaVersion{1, 0, 0})
	_, err := p.Transition(StateDeployed, "a", "skip")
	if err == nil {
		t.Fatal("expected error for draft->deployed")
	}
}

func TestLifecycleCanTransition(t *testing.T) {
	p := makeTestPolicy("x", SchemaVersion{1, 0, 0})
	if !p.CanTransition(StatePendingApproval) {
		t.Error("draft should be able to transition to pending_approval")
	}
	if p.CanTransition(StateDeployed) {
		t.Error("draft should NOT be able to transition to deployed")
	}
}

func TestLifecycleRollbackPath(t *testing.T) {
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
	_, _ = p.Transition(StatePendingApproval, "a", "")
	_, _ = p.Transition(StateApproved, "b", "")
	_, _ = p.Transition(StateStaged, "ops", "")
	_, _ = p.Transition(StateDeployed, "ops", "")

	rec, err := p.Transition(StateRolledBack, "ops", "error rate spike")
	if err != nil {
		t.Fatalf("rollback transition: %v", err)
	}
	if rec.From != StateDeployed || rec.To != StateRolledBack {
		t.Errorf("rollback record: from=%q to=%q", rec.From, rec.To)
	}

	// After rollback, should be able to go back to draft
	_, err = p.Transition(StateDraft, "author", "fixing")
	if err != nil {
		t.Fatalf("rolled_back->draft: %v", err)
	}
}

func TestIsFinalState(t *testing.T) {
	if !IsFinalState(StateDeprecated) {
		t.Error("Deprecated should be final")
	}
	if IsFinalState(StateDraft) {
		t.Error("Draft should not be final")
	}
}

// --- PolicyStore ---

func TestPolicyStoreAddAndGet(t *testing.T) {
	s := NewPolicyStore()
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
	if err := s.Add(p); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := s.Get("rl", SchemaVersion{1, 0, 0})
	if !ok || got.ID != "rl" {
		t.Error("Get failed")
	}
}

func TestPolicyStoreDuplicate(t *testing.T) {
	s := NewPolicyStore()
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
	_ = s.Add(p)
	if err := s.Add(p); err == nil {
		t.Error("expected error on duplicate add")
	}
}

func TestPolicyStoreLatest(t *testing.T) {
	s := NewPolicyStore()
	_ = s.Add(makeTestPolicy("rl", SchemaVersion{1, 0, 0}))
	_ = s.Add(makeTestPolicy("rl", SchemaVersion{1, 1, 0}))
	_ = s.Add(makeTestPolicy("rl", SchemaVersion{2, 0, 0}))

	got, ok := s.Latest("rl")
	if !ok {
		t.Fatal("Latest returned false")
	}
	if got.Version != (SchemaVersion{2, 0, 0}) {
		t.Errorf("Latest = %v, want 2.0.0", got.Version)
	}
}

func TestPolicyStoreListByState(t *testing.T) {
	s := NewPolicyStore()
	p1 := makeTestPolicy("a", SchemaVersion{1, 0, 0})
	p2 := makeTestPolicy("b", SchemaVersion{1, 0, 0})
	p3 := makeTestPolicy("c", SchemaVersion{1, 0, 0})
	_, _ = p1.Transition(StatePendingApproval, "a", "")
	_, _ = p3.Transition(StatePendingApproval, "a", "")
	_ = s.Add(p1)
	_ = s.Add(p2)
	_ = s.Add(p3)

	pending := s.ListByState(StatePendingApproval)
	if len(pending) != 2 {
		t.Errorf("ListByState(pending_approval) = %d, want 2", len(pending))
	}
}

// --- Approval ---

func TestApprovalWorkflowQuorum(t *testing.T) {
	w := NewApprovalWorkflow(2, []string{"alice", "bob", "carol"})

	if w.IsQuorumMet() {
		t.Error("quorum should not be met initially")
	}

	if err := w.Grant("alice", "LGTM"); err != nil {
		t.Fatalf("Grant alice: %v", err)
	}
	if w.IsQuorumMet() {
		t.Error("quorum should not be met with 1 approval")
	}

	if err := w.Grant("bob", "+1"); err != nil {
		t.Fatalf("Grant bob: %v", err)
	}
	if !w.IsQuorumMet() {
		t.Error("quorum should be met with 2 approvals")
	}
}

func TestApprovalWorkflowReject(t *testing.T) {
	w := NewApprovalWorkflow(2, []string{"alice", "bob"})
	if err := w.Reject("alice", "needs work"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if !w.IsRejected() {
		t.Error("should be rejected")
	}
	g, r := w.Quorum()
	if g != 0 || r != 2 {
		t.Errorf("Quorum() = %d/%d, want 0/2", g, r)
	}
}

func TestApprovalWorkflowUnauthorized(t *testing.T) {
	w := NewApprovalWorkflow(1, []string{"alice"})
	if err := w.Grant("eve", "hi"); err == nil {
		t.Error("expected error for unauthorized approver")
	}
}

func TestApprovalWorkflowDoubleVote(t *testing.T) {
	w := NewApprovalWorkflow(2, []string{"alice", "bob"})
	_ = w.Grant("alice", "")
	if err := w.Grant("alice", "again"); err == nil {
		t.Error("expected error on double vote")
	}
}

func TestApprovalWorkflowOpenApprovers(t *testing.T) {
	w := NewApprovalWorkflow(1, nil) // empty = anyone
	if err := w.Grant("anyone", ""); err != nil {
		t.Fatalf("open approval: %v", err)
	}
	if !w.IsQuorumMet() {
		t.Error("quorum should be met")
	}
}

// --- Staging ---

func TestStagingTable(t *testing.T) {
	tbl := NewStagingTable()
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})

	if err := tbl.Stage(p, EnvDev, "ops"); err != nil {
		t.Fatalf("Stage dev: %v", err)
	}
	sp, ok := tbl.Active("rl", EnvDev)
	if !ok || sp.Version != (SchemaVersion{1, 0, 0}) {
		t.Error("Active dev failed")
	}

	// Promote to staging
	if err := tbl.Stage(p, EnvStaging, "ops"); err != nil {
		t.Fatalf("Stage staging: %v", err)
	}
}

func TestCanPromoteTo(t *testing.T) {
	if !CanPromoteTo(EnvDev, EnvStaging) {
		t.Error("dev->staging should be valid")
	}
	if !CanPromoteTo(EnvStaging, EnvProd) {
		t.Error("staging->prod should be valid")
	}
	if CanPromoteTo(EnvDev, EnvProd) {
		t.Error("dev->prod should be invalid (not adjacent)")
	}
	if CanPromoteTo(EnvProd, EnvDev) {
		t.Error("prod->dev should be invalid (backwards)")
	}
}

func TestStagingTableActiveInEnv(t *testing.T) {
	tbl := NewStagingTable()
	p1 := makeTestPolicy("a", SchemaVersion{1, 0, 0})
	p2 := makeTestPolicy("b", SchemaVersion{1, 0, 0})
	_ = tbl.Stage(p1, EnvDev, "ops")
	_ = tbl.Stage(p2, EnvDev, "ops")

	devPolicies := tbl.ActiveInEnv(EnvDev)
	if len(devPolicies) != 2 {
		t.Errorf("ActiveInEnv(dev) = %d, want 2", len(devPolicies))
	}
}

// --- BlueGreen ---

func TestBlueGreenSwitch(t *testing.T) {
	bg := NewBlueGreen("rl", EnvProd)

	// Default active is empty string; deploy to blue first
	bg.DeployToSlot(SlotBlue, SchemaVersion{1, 0, 0}, "ops")
	bg.DeployToSlot(SlotGreen, SchemaVersion{1, 1, 0}, "ops")

	prev, err := bg.SwitchTraffic(SlotGreen, "ops")
	if err != nil {
		t.Fatalf("SwitchTraffic: %v", err)
	}
	if prev != "" {
		t.Errorf("previous slot = %q, want empty (no prior active)", prev)
	}
	if bg.ActiveSlot() != SlotGreen {
		t.Errorf("ActiveSlot = %q, want green", bg.ActiveSlot())
	}

	// Switch back to blue
	prev, err = bg.SwitchTraffic(SlotBlue, "ops")
	if err != nil {
		t.Fatalf("SwitchTraffic back: %v", err)
	}
	if prev != SlotGreen {
		t.Errorf("prev = %q, want green", prev)
	}
}

func TestBlueGreenInvalidSlot(t *testing.T) {
	bg := NewBlueGreen("rl", EnvProd)
	_, err := bg.SwitchTraffic("red", "ops")
	if err == nil {
		t.Error("expected error for invalid slot")
	}
}

func TestBlueGreenEmptySlot(t *testing.T) {
	bg := NewBlueGreen("rl", EnvProd)
	_, err := bg.SwitchTraffic(SlotBlue, "ops")
	if err == nil {
		t.Error("expected error for switching to empty slot")
	}
}

func TestBlueGreenBothDeployed(t *testing.T) {
	bg := NewBlueGreen("rl", EnvProd)
	if bg.BothDeployed() {
		t.Error("should not be both deployed initially")
	}
	bg.DeployToSlot(SlotBlue, SchemaVersion{1, 0, 0}, "ops")
	if bg.BothDeployed() {
		t.Error("only one deployed")
	}
	bg.DeployToSlot(SlotGreen, SchemaVersion{1, 1, 0}, "ops")
	if !bg.BothDeployed() {
		t.Error("both should be deployed")
	}
}

// --- Rollback ---

func TestRollbackManager(t *testing.T) {
	rm := NewRollbackManager()

	rm.RecordDeploy("rl", SchemaVersion{1, 0, 0}, "ops", EnvProd)
	rm.RecordDeploy("rl", SchemaVersion{1, 1, 0}, "ops", EnvProd)

	if !rm.CanRollback("rl") {
		t.Error("should be able to rollback")
	}

	prev, ok := rm.PreviousVersion("rl")
	if !ok || prev != (SchemaVersion{1, 0, 0}) {
		t.Error("PreviousVersion wrong")
	}

	hist := rm.History("rl")
	if len(hist) != 2 {
		t.Errorf("History length = %d, want 2", len(hist))
	}
}

func TestRollbackManagerCannotRollback(t *testing.T) {
	rm := NewRollbackManager()
	rm.RecordDeploy("rl", SchemaVersion{1, 0, 0}, "ops", EnvProd)
	if rm.CanRollback("rl") {
		t.Error("should not be able to rollback with only 1 deploy")
	}
}

func TestRollbackManagerRollback(t *testing.T) {
	rm := NewRollbackManager()
	rm.RecordDeploy("rl", SchemaVersion{1, 0, 0}, "ops", EnvProd)
	rm.RecordDeploy("rl", SchemaVersion{1, 1, 0}, "ops", EnvProd)

	p := makeTestPolicy("rl", SchemaVersion{1, 1, 0})
	_, _ = p.Transition(StatePendingApproval, "a", "")
	_, _ = p.Transition(StateApproved, "b", "")
	_, _ = p.Transition(StateStaged, "ops", "")
	_, _ = p.Transition(StateDeployed, "ops", "")

	rb, err := rm.Rollback(p, "ops", "error spike", EnvProd)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rb.PolicyID != "rl" {
		t.Errorf("rb.PolicyID = %q", rb.PolicyID)
	}
	if rb.FromVersion != (SchemaVersion{1, 1, 0}) {
		t.Errorf("rb.FromVersion = %v", rb.FromVersion)
	}
	if rb.ToVersion != (SchemaVersion{1, 0, 0}) {
		t.Errorf("rb.ToVersion = %v", rb.ToVersion)
	}
}

func TestRollbackManagerNoHistory(t *testing.T) {
	rm := NewRollbackManager()
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
	_, err := rm.Rollback(p, "ops", "nope", EnvProd)
	if err == nil {
		t.Error("expected error when no history")
	}
}

// --- Concurrency ---

func TestConcurrentTransitions(t *testing.T) {
	p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Transition(StatePendingApproval, "actor", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	successCount := 0
	for err := range errs {
		if err == nil {
			successCount++
		}
	}
	if successCount != 1 {
		t.Errorf("expected exactly 1 successful transition, got %d", successCount)
	}
}

func TestConcurrentApproval(t *testing.T) {
	w := NewApprovalWorkflow(2, []string{"a", "b", "c"})
	var wg sync.WaitGroup

	for _, actor := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			_ = w.Grant(a, "ok")
		}(actor)
	}
	wg.Wait()

	g, r := w.Quorum()
	if g != 3 || r != 2 {
		t.Errorf("Quorum = %d/%d, want 3/2", g, r)
	}
	if !w.IsQuorumMet() {
		t.Error("quorum should be met")
	}
}

func TestConcurrentStaging(t *testing.T) {
	tbl := NewStagingTable()
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := makeTestPolicy("rl", SchemaVersion{1, 0, 0})
			_ = tbl.Stage(p, EnvDev, "ops")
		}()
	}
	wg.Wait()

	_, ok := tbl.Active("rl", EnvDev)
	if !ok {
		t.Error("expected a policy to be staged")
	}
}

// --- versionGt helper ---

func TestVersionGt(t *testing.T) {
	tests := []struct {
		a, b SchemaVersion
		want bool
	}{
		{SchemaVersion{2, 0, 0}, SchemaVersion{1, 9, 9}, true},
		{SchemaVersion{1, 2, 0}, SchemaVersion{1, 1, 9}, true},
		{SchemaVersion{1, 1, 2}, SchemaVersion{1, 1, 1}, true},
		{SchemaVersion{1, 0, 0}, SchemaVersion{1, 0, 0}, false},
		{SchemaVersion{1, 0, 0}, SchemaVersion{2, 0, 0}, false},
	}
	for _, tt := range tests {
		if got := versionGt(tt.a, tt.b); got != tt.want {
			t.Errorf("versionGt(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

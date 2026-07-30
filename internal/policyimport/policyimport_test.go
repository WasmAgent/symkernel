package policyimport

import (
	"context"
	"errors"
	"testing"

	"github.com/WasmAgent/symkernel/internal/cel"
)

// evalCEL compiles and evaluates a translated CEL expression against vars,
// returning the boolean decision. It uses symkernel's own CEL substrate so the
// tests prove the translation runs on the real evaluator, not a mock.
func evalCEL(t *testing.T, expr string, vars map[string]any) bool {
	t.Helper()
	out, err := cel.Evaluate(context.Background(), expr, vars)
	if err != nil {
		t.Fatalf("cel.Evaluate(%q) error: %v", expr, err)
	}
	b, ok := out.(bool)
	if !ok {
		t.Fatalf("cel.Evaluate(%q) returned %T (%v), want bool", expr, out, out)
	}
	return b
}

func TestTranslateRego_DefaultDenyAllowAdmin(t *testing.T) {
	src := `package authz
default allow := false
allow if { input.user.role == "admin" }`

	res, err := TranslateRego(src, "allow")
	if err != nil {
		t.Fatalf("TranslateRego error: %v", err)
	}
	if res.Effect != EffectPermit {
		t.Errorf("Effect = %q, want permit", res.Effect)
	}

	// Equivalence: the CEL decision must match Rego's for every input.
	cases := []struct {
		role string
		want bool
	}{
		{"admin", true},
		{"user", false},
		{"", false},
	}
	for _, tc := range cases {
		vars := map[string]any{"input": map[string]any{
			"user": map[string]any{"role": tc.role},
		}}
		if got := evalCEL(t, res.CEL, vars); got != tc.want {
			t.Errorf("role=%q: CEL %q = %v, want %v", tc.role, res.CEL, got, tc.want)
		}
	}
}

func TestTranslateRego_MultipleRulesDisjunction(t *testing.T) {
	src := `package authz
default allow := false
allow if { input.user.role == "admin" }
allow if { input.method == "GET" }`

	res, err := TranslateRego(src, "allow")
	if err != nil {
		t.Fatalf("TranslateRego error: %v", err)
	}

	cases := []struct {
		role, method string
		want         bool
	}{
		{"admin", "POST", true}, // first rule
		{"user", "GET", true},   // second rule
		{"user", "POST", false}, // neither
		{"admin", "GET", true},  // both
	}
	for _, tc := range cases {
		vars := map[string]any{"input": map[string]any{
			"user":   map[string]any{"role": tc.role},
			"method": tc.method,
		}}
		if got := evalCEL(t, res.CEL, vars); got != tc.want {
			t.Errorf("role=%q method=%q: %q = %v, want %v", tc.role, tc.method, res.CEL, got, tc.want)
		}
	}
}

func TestTranslateRego_NumericComparisonAndConjunction(t *testing.T) {
	src := `package authz
default allow := false
allow if {
	input.user.age >= 18
	input.user.verified == true
}`

	res, err := TranslateRego(src, "allow")
	if err != nil {
		t.Fatalf("TranslateRego error: %v", err)
	}

	cases := []struct {
		age      int
		verified bool
		want     bool
	}{
		{21, true, true},
		{21, false, false},
		{17, true, false},
		{18, true, true},
	}
	for _, tc := range cases {
		vars := map[string]any{"input": map[string]any{
			"user": map[string]any{"age": tc.age, "verified": tc.verified},
		}}
		if got := evalCEL(t, res.CEL, vars); got != tc.want {
			t.Errorf("age=%d verified=%v: %q = %v, want %v", tc.age, tc.verified, res.CEL, got, tc.want)
		}
	}
}

func TestTranslateRego_Negation(t *testing.T) {
	src := `package authz
default allow := false
allow if { input.user.role != "banned" }`

	res, err := TranslateRego(src, "allow")
	if err != nil {
		t.Fatalf("TranslateRego error: %v", err)
	}
	for role, want := range map[string]bool{"banned": false, "member": true} {
		vars := map[string]any{"input": map[string]any{"user": map[string]any{"role": role}}}
		if got := evalCEL(t, res.CEL, vars); got != want {
			t.Errorf("role=%q: %q = %v, want %v", role, res.CEL, got, want)
		}
	}
}

func TestTranslateRego_FailClosed(t *testing.T) {
	cases := []struct {
		name, src, wantConstruct string
	}{
		{
			name: "unknown builtin",
			src: `package authz
allow if { count(input.items) > 3 }`,
			wantConstruct: "rego.builtin:count",
		},
		{
			name: "with modifier",
			src: `package authz
allow if { input.x == 1 with input as {} }`,
			wantConstruct: "rego.with",
		},
		{
			name: "every quantifier",
			src: `package authz
allow if { every x in input.xs { x > 0 } }`,
			wantConstruct: "rego.quantifier",
		},
		{
			name: "some quantifier",
			src: `package authz
allow if { some x in input.xs; x == 1 }`,
			wantConstruct: "rego.quantifier",
		},
		{
			name: "non-input root",
			src: `package authz
allow if { data.foo == 1 }`,
			wantConstruct: "rego.ref.root:data",
		},
		{
			name: "allow by default",
			src: `package authz
default allow := true`,
			wantConstruct: "rego.default-true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TranslateRego(tc.src, "allow")
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("want *UnsupportedError, got %v", err)
			}
			if ue.Construct != tc.wantConstruct {
				t.Errorf("Construct = %q, want %q", ue.Construct, tc.wantConstruct)
			}
		})
	}
}

func TestTranslateCedar_PermitWithConditions(t *testing.T) {
	src := `permit(
	principal == User::"alice",
	action == Action::"read",
	resource
)
when { resource.owner == "alice" };`

	res, err := TranslateCedar(src)
	if err != nil {
		t.Fatalf("TranslateCedar error: %v", err)
	}
	if res.Effect != EffectPermit {
		t.Errorf("Effect = %q, want permit", res.Effect)
	}

	cases := []struct {
		principal, action, owner string
		want                     bool
	}{
		{`User::"alice"`, `Action::"read"`, "alice", true},
		{`User::"bob"`, `Action::"read"`, "alice", false},    // wrong principal
		{`User::"alice"`, `Action::"write"`, "alice", false}, // wrong action
		{`User::"alice"`, `Action::"read"`, "bob", false},    // condition fails
	}
	for _, tc := range cases {
		vars := map[string]any{
			"principal": tc.principal,
			"action":    tc.action,
			"resource":  map[string]any{"owner": tc.owner},
			"context":   map[string]any{},
		}
		if got := evalCEL(t, res.CEL, vars); got != tc.want {
			t.Errorf("p=%q a=%q owner=%q: %q = %v, want %v",
				tc.principal, tc.action, tc.owner, res.CEL, got, tc.want)
		}
	}
}

func TestTranslateCedar_Forbid(t *testing.T) {
	src := `forbid(principal, action, resource)
when { context.mfa == false };`

	res, err := TranslateCedar(src)
	if err != nil {
		t.Fatalf("TranslateCedar error: %v", err)
	}
	if res.Effect != EffectForbid {
		t.Errorf("Effect = %q, want forbid", res.Effect)
	}
	// The CEL is the match condition; for a forbid policy, true means "deny".
	for mfa, wantMatch := range map[bool]bool{false: true, true: false} {
		vars := map[string]any{
			"principal": `User::"x"`,
			"action":    `Action::"y"`,
			"resource":  map[string]any{},
			"context":   map[string]any{"mfa": mfa},
		}
		if got := evalCEL(t, res.CEL, vars); got != wantMatch {
			t.Errorf("mfa=%v: %q = %v, want %v", mfa, res.CEL, got, wantMatch)
		}
	}
}

func TestTranslateCedar_NumericAndLogical(t *testing.T) {
	src := `permit(principal, action, resource)
when { context.age >= 18 && context.country == "US" };`

	res, err := TranslateCedar(src)
	if err != nil {
		t.Fatalf("TranslateCedar error: %v", err)
	}
	cases := []struct {
		age     int
		country string
		want    bool
	}{
		{21, "US", true},
		{21, "CA", false},
		{16, "US", false},
	}
	for _, tc := range cases {
		vars := map[string]any{
			"principal": `User::"x"`,
			"action":    `Action::"y"`,
			"resource":  map[string]any{},
			"context":   map[string]any{"age": tc.age, "country": tc.country},
		}
		if got := evalCEL(t, res.CEL, vars); got != tc.want {
			t.Errorf("age=%d country=%q: %q = %v, want %v", tc.age, tc.country, res.CEL, got, tc.want)
		}
	}
}

func TestTranslateCedar_FailClosed(t *testing.T) {
	cases := []struct {
		name, src, wantConstruct string
	}{
		{
			name:          "like operator",
			src:           `permit(principal, action, resource) when { resource.name like "*.txt" };`,
			wantConstruct: "cedar.node:like",
		},
		{
			name:          "decimal extension",
			src:           `permit(principal, action, resource) when { context.score.lessThan(decimal("1.5")) };`,
			wantConstruct: "cedar.node:extensionCall:lessThan",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := TranslateCedar(tc.src)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("want *UnsupportedError, got %v", err)
			}
			if ue.Construct != tc.wantConstruct {
				t.Errorf("Construct = %q, want %q", ue.Construct, tc.wantConstruct)
			}
		})
	}
}

func TestTranslateCedar_MultiPolicyRejected(t *testing.T) {
	src := `permit(principal, action, resource);
forbid(principal, action, resource);`
	_, err := TranslateCedar(src)
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UnsupportedError for multi-policy, got %v", err)
	}
	if ue.Construct != "cedar.multi-policy" {
		t.Errorf("Construct = %q, want cedar.multi-policy", ue.Construct)
	}
}

package policyimport

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"github.com/cedar-policy/cedar-go"
	"github.com/cedar-policy/cedar-go/types"
	"github.com/cedar-policy/cedar-go/x/exp/ast"
)

// TranslateCedar parses a single Cedar policy and translates it into an
// equivalent CEL expression. The document must contain exactly one policy;
// multi-policy sets are rejected (compose them at a higher level so each
// policy's effect stays explicit).
//
// The translation is fail-closed: any Cedar construct not explicitly handled
// yields an *UnsupportedError. The emitted CEL reads from the top-level
// identifiers `principal`, `action`, `resource`, and `context`, matching the
// Cedar request variables.
//
// Scope constraints (the `principal`/`action`/`resource` clauses) and the
// `when`/`unless` conditions are combined into a single boolean CEL expression
// that is true exactly when the policy matches. The caller pairs that with the
// returned Effect to decide allow vs. deny.
func TranslateCedar(src string) (Result, error) {
	ps, err := cedar.NewPolicySetFromBytes("policy.cedar", []byte(src))
	if err != nil {
		return Result{}, fmt.Errorf("cedar: parse: %w", err)
	}
	policies := maps.Collect(ps.All())
	if len(policies) != 1 {
		return Result{}, unsupported("cedar", "cedar.multi-policy",
			"expected exactly one policy, got %d; translate policies individually", len(policies))
	}

	var pol *cedar.Policy
	for _, p := range policies {
		pol = p
	}
	// AST() returns the public wrapper type; it is a named type over
	// x/exp/ast.Policy with an identical underlying layout, so the pointer
	// converts directly to the concrete AST we walk.
	a := (*ast.Policy)(pol.AST())

	c := &cedarTranslator{}
	conds, err := c.policy(a)
	if err != nil {
		return Result{}, err
	}

	effect := EffectForbid
	if a.Effect == ast.EffectPermit {
		effect = EffectPermit
	}
	return Result{CEL: conds, Effect: effect, SourceLang: "cedar"}, nil
}

type cedarTranslator struct{}

// policy renders the full match condition: the conjunction of the three scope
// constraints and every when/unless condition.
func (c *cedarTranslator) policy(p *ast.Policy) (string, error) {
	var terms []string

	if s, err := c.principalScope(p.Principal); err != nil {
		return "", err
	} else if s != "" {
		terms = append(terms, s)
	}
	if s, err := c.actionScope(p.Action); err != nil {
		return "", err
	} else if s != "" {
		terms = append(terms, s)
	}
	if s, err := c.resourceScope(p.Resource); err != nil {
		return "", err
	} else if s != "" {
		terms = append(terms, s)
	}

	for _, cond := range p.Conditions {
		body, err := c.node(cond.Body)
		if err != nil {
			return "", err
		}
		// `when { e }` requires e; `unless { e }` requires !e.
		if cond.Condition == ast.ConditionUnless {
			body = "!(" + body + ")"
		}
		terms = append(terms, body)
	}

	if len(terms) == 0 {
		// A policy with no scope constraints and no conditions matches
		// everything (Cedar `permit(principal, action, resource);`).
		return "true", nil
	}
	return strings.Join(wrap(terms), " && "), nil
}

// --- scope constraints -----------------------------------------------------

func (c *cedarTranslator) principalScope(s ast.IsPrincipalScopeNode) (string, error) {
	return c.scope("principal", s)
}

func (c *cedarTranslator) resourceScope(s ast.IsResourceScopeNode) (string, error) {
	return c.scope("resource", s)
}

// scope handles the principal/resource scope forms, which share node types.
func (c *cedarTranslator) scope(varName string, s ast.IsScopeNode) (string, error) {
	switch n := s.(type) {
	case ast.ScopeTypeAll:
		return "", nil // unconstrained
	case ast.ScopeTypeEq:
		return fmt.Sprintf("%s == %s", varName, celEntityUID(n.Entity)), nil
	case ast.ScopeTypeIn:
		// `principal in Group::"x"` — membership. CEL has no built-in entity
		// hierarchy, so we surface it as an `in` against a caller-provided
		// ancestor set keyed on the entity. We model it as equality-or-member:
		// the caller supplies principal plus its ancestors under `.ancestors`.
		return fmt.Sprintf("(%s == %s || %s in %s.ancestors)",
			varName, celEntityUID(n.Entity), celEntityUID(n.Entity), varName), nil
	case ast.ScopeTypeIs:
		return fmt.Sprintf("%s.__entity_type == %s", varName, celString(string(n.Type))), nil
	case ast.ScopeTypeIsIn:
		return fmt.Sprintf("(%s.__entity_type == %s && (%s == %s || %s in %s.ancestors))",
			varName, celString(string(n.Type)),
			varName, celEntityUID(n.Entity), celEntityUID(n.Entity), varName), nil
	default:
		return "", unsupported("cedar", fmt.Sprintf("cedar.scope:%T", s),
			"unhandled %s scope form", varName)
	}
}

func (c *cedarTranslator) actionScope(s ast.IsActionScopeNode) (string, error) {
	switch n := s.(type) {
	case ast.ScopeTypeAll:
		return "", nil
	case ast.ScopeTypeEq:
		return fmt.Sprintf("action == %s", celEntityUID(n.Entity)), nil
	case ast.ScopeTypeIn:
		return fmt.Sprintf("(action == %s || %s in action.ancestors)",
			celEntityUID(n.Entity), celEntityUID(n.Entity)), nil
	case ast.ScopeTypeInSet:
		parts := make([]string, 0, len(n.Entities))
		for _, e := range n.Entities {
			parts = append(parts, fmt.Sprintf("action == %s", celEntityUID(e)))
		}
		if len(parts) == 0 {
			return "false", nil
		}
		return "(" + strings.Join(parts, " || ") + ")", nil
	default:
		return "", unsupported("cedar", fmt.Sprintf("cedar.scope:%T", s),
			"unhandled action scope form")
	}
}

// --- expression nodes ------------------------------------------------------

func (c *cedarTranslator) node(n ast.IsNode) (string, error) {
	switch e := n.(type) {
	case ast.NodeValue:
		return c.value(e.Value)
	case ast.NodeTypeVariable:
		return string(e.Name), nil // principal|action|resource|context

	case ast.NodeTypeAccess:
		base, err := c.node(e.Arg)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.%s", base, string(e.Value)), nil
	case ast.NodeTypeHas:
		base, err := c.node(e.Arg)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("has(%s.%s)", base, string(e.Value)), nil

	case ast.NodeTypeEquals:
		return c.binary(e.Left, e.Right, "==")
	case ast.NodeTypeNotEquals:
		return c.binary(e.Left, e.Right, "!=")
	case ast.NodeTypeLessThan:
		return c.binary(e.Left, e.Right, "<")
	case ast.NodeTypeLessThanOrEqual:
		return c.binary(e.Left, e.Right, "<=")
	case ast.NodeTypeGreaterThan:
		return c.binary(e.Left, e.Right, ">")
	case ast.NodeTypeGreaterThanOrEqual:
		return c.binary(e.Left, e.Right, ">=")
	case ast.NodeTypeAnd:
		return c.binary(e.Left, e.Right, "&&")
	case ast.NodeTypeOr:
		return c.binary(e.Left, e.Right, "||")
	case ast.NodeTypeAdd:
		return c.binary(e.Left, e.Right, "+")
	case ast.NodeTypeSub:
		return c.binary(e.Left, e.Right, "-")
	case ast.NodeTypeMult:
		return c.binary(e.Left, e.Right, "*")

	case ast.NodeTypeNot:
		arg, err := c.node(e.Arg)
		if err != nil {
			return "", err
		}
		return "!(" + arg + ")", nil
	case ast.NodeTypeNegate:
		arg, err := c.node(e.Arg)
		if err != nil {
			return "", err
		}
		return "-(" + arg + ")", nil

	case ast.NodeTypeIn:
		// `x in y` — entity hierarchy membership. Model as
		// (x == y || x in y.ancestors) when y is a single entity; if y is a
		// set, CEL `in` handles membership directly.
		left, err := c.node(e.Left)
		if err != nil {
			return "", err
		}
		right, err := c.node(e.Right)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(%s == %s || %s in %s.ancestors)", left, right, left, right), nil

	case ast.NodeTypeContains:
		return c.method(e.Left, e.Right, "contains")
	case ast.NodeTypeContainsAll:
		return c.method(e.Left, e.Right, "containsAll")
	case ast.NodeTypeContainsAny:
		return c.method(e.Left, e.Right, "containsAny")

	case ast.NodeTypeIfThenElse:
		cond, err := c.node(e.If)
		if err != nil {
			return "", err
		}
		then, err := c.node(e.Then)
		if err != nil {
			return "", err
		}
		els, err := c.node(e.Else)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(%s ? %s : %s)", cond, then, els), nil

	case ast.NodeTypeSet:
		parts := make([]string, 0, len(e.Elements))
		for _, el := range e.Elements {
			s, err := c.node(el)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil

	case ast.NodeTypeRecord:
		parts := make([]string, 0, len(e.Elements))
		for _, el := range e.Elements {
			v, err := c.node(el.Value)
			if err != nil {
				return "", err
			}
			parts = append(parts, fmt.Sprintf("%s: %s", celString(string(el.Key)), v))
		}
		return "{" + strings.Join(parts, ", ") + "}", nil

	case ast.NodeTypeIs:
		left, err := c.node(e.Left)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.__entity_type == %s", left, celString(string(e.EntityType))), nil

	// Constructs we deliberately do not translate: their CEL semantics are
	// not a faithful 1:1 mapping, so we reject rather than approximate.
	case ast.NodeTypeLike:
		return "", unsupported("cedar", "cedar.node:like",
			"the `like` wildcard-pattern operator has no exact CEL equivalent")
	case ast.NodeTypeIsIn:
		return "", unsupported("cedar", "cedar.node:isIn",
			"combined `is ... in` in a condition is not translated; express as separate `is` and `in`")
	case ast.NodeTypeHasTag, ast.NodeTypeGetTag:
		return "", unsupported("cedar", "cedar.node:tag",
			"entity tags have no CEL equivalent")
	case ast.NodeTypeIsEmpty:
		return "", unsupported("cedar", "cedar.node:isEmpty",
			"isEmpty() is not translated; use `.size() == 0` semantics explicitly")
	case ast.NodeTypeExtensionCall:
		return "", unsupported("cedar", "cedar.node:extensionCall:"+string(e.Name),
			"extension/method call %q (decimal/ip/datetime/duration) has no CEL equivalent", string(e.Name))

	default:
		return "", unsupported("cedar", fmt.Sprintf("cedar.node:%T", n),
			"unhandled expression node")
	}
}

func (c *cedarTranslator) binary(l, r ast.IsNode, op string) (string, error) {
	ls, err := c.node(l)
	if err != nil {
		return "", err
	}
	rs, err := c.node(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s %s %s)", ls, op, rs), nil
}

func (c *cedarTranslator) method(recv, arg ast.IsNode, name string) (string, error) {
	rs, err := c.node(recv)
	if err != nil {
		return "", err
	}
	as, err := c.node(arg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.%s(%s)", rs, name, as), nil
}

// value renders a Cedar literal value into CEL.
func (c *cedarTranslator) value(v types.Value) (string, error) {
	switch val := v.(type) {
	case types.Boolean:
		return strconv.FormatBool(bool(val)), nil
	case types.Long:
		return strconv.FormatInt(int64(val), 10), nil
	case types.String:
		return celString(string(val)), nil
	case types.EntityUID:
		return celEntityUID(val), nil
	case types.Set:
		parts := make([]string, 0, val.Len())
		for el := range val.All() {
			s, err := c.value(el)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		sort.Strings(parts) // stable output; set order is non-deterministic
		return "[" + strings.Join(parts, ", ") + "]", nil
	case types.Record:
		type kv struct{ k, v string }
		var items []kv
		for k, ev := range val.All() {
			s, err := c.value(ev)
			if err != nil {
				return "", err
			}
			items = append(items, kv{celString(string(k)), s})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].k < items[j].k })
		parts := make([]string, 0, len(items))
		for _, it := range items {
			parts = append(parts, it.k+": "+it.v)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", unsupported("cedar", fmt.Sprintf("cedar.value:%T", v),
			"literal value type has no CEL equivalent")
	}
}

// celEntityUID renders a Cedar entity reference as a stable CEL string literal
// of the form `Type::"id"`. CEL has no native entity type, so entities are
// compared as opaque strings; both sides of a comparison render the same way.
func celEntityUID(e types.EntityUID) string {
	return celString(fmt.Sprintf("%s::%q", string(e.Type), string(e.ID)))
}

// wrap parenthesizes each already-formed term for safe && joining.
func wrap(terms []string) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = "(" + t + ")"
	}
	return out
}

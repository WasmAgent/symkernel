package policyimport

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
)

// TranslateRego parses a single Rego module and translates a boolean decision
// rule (conventionally `allow`) into an equivalent CEL expression.
//
// It handles the common default-deny idiom:
//
//	package authz
//	default allow := false
//	allow if { input.user.role == "admin" }
//	allow if { input.method == "GET" }
//
// Multiple `allow` rules are combined as a disjunction (the rule fires if ANY
// of them holds); the body of each rule is the conjunction of its expressions
// (all must hold). The `default allow := false` rule is the base case and does
// not contribute a term.
//
// The translation is fail-closed. Only a well-defined subset is supported:
// comparison/arithmetic built-ins over static `input.*` references and
// literals, plus negation. Any user-defined function call, unknown built-in,
// `with`, quantifier (`some`/`every`), or dynamic reference yields an
// *UnsupportedError. ruleName selects which rule to translate; pass "" to
// default to "allow".
func TranslateRego(src, ruleName string) (Result, error) {
	if ruleName == "" {
		ruleName = "allow"
	}
	mod, err := ast.ParseModuleWithOpts("policy.rego", src, ast.ParserOptions{RegoVersion: ast.RegoV1})
	if err != nil {
		return Result{}, fmt.Errorf("rego: parse: %w", err)
	}
	if mod == nil {
		return Result{}, unsupported("rego", "rego.empty", "empty module")
	}

	t := &regoTranslator{ruleName: ruleName}
	var disjuncts []string
	var sawDefaultFalse, sawRule bool

	for _, rule := range mod.Rules {
		if rule.Head == nil || string(rule.Head.Name) != ruleName {
			continue
		}
		if len(rule.Head.Args) > 0 {
			return Result{}, unsupported("rego", "rego.function",
				"rule %q takes arguments; functions are not translated", ruleName)
		}
		if rule.Head.Key != nil {
			return Result{}, unsupported("rego", "rego.partial-set",
				"partial-set/object rule %q is not translated", ruleName)
		}
		if rule.Default {
			// `default allow := false` (or true). A default of true would make
			// the policy allow-by-default; record it but it contributes no
			// condition term. We only accept the standard deny-by-default.
			if v, ok := boolTermValue(rule.Head.Value); ok {
				if v {
					return Result{}, unsupported("rego", "rego.default-true",
						"`default %s := true` (allow-by-default) is not translated", ruleName)
				}
				sawDefaultFalse = true
			}
			continue
		}
		if rule.Else != nil {
			return Result{}, unsupported("rego", "rego.else",
				"`else` clauses on rule %q are not translated", ruleName)
		}
		// The rule head value must be `true` (or generated true) for a boolean
		// allow rule. Reject rules that assign a non-boolean result.
		if v, ok := boolTermValue(rule.Head.Value); !ok || !v {
			return Result{}, unsupported("rego", "rego.non-bool-head",
				"rule %q does not produce boolean true", ruleName)
		}

		body, err := t.body(rule.Body)
		if err != nil {
			return Result{}, err
		}
		disjuncts = append(disjuncts, body)
		sawRule = true
	}

	if !sawRule {
		return Result{}, unsupported("rego", "rego.no-rule",
			"no translatable rule named %q found", ruleName)
	}
	_ = sawDefaultFalse // deny-by-default is the CEL default (false) already.

	cel := strings.Join(wrap(disjuncts), " || ")
	return Result{CEL: cel, Effect: EffectPermit, SourceLang: "rego"}, nil
}

type regoTranslator struct {
	ruleName string
}

// body renders a rule body (a slice of expressions) as their conjunction.
func (t *regoTranslator) body(b ast.Body) (string, error) {
	if len(b) == 0 {
		return "true", nil
	}
	terms := make([]string, 0, len(b))
	for _, expr := range b {
		s, err := t.expr(expr)
		if err != nil {
			return "", err
		}
		terms = append(terms, s)
	}
	return strings.Join(wrap(terms), " && "), nil
}

func (t *regoTranslator) expr(expr *ast.Expr) (string, error) {
	if len(expr.With) > 0 {
		return "", unsupported("rego", "rego.with",
			"`with` modifiers are not translated")
	}

	switch {
	case expr.IsCall():
		s, err := t.call(expr)
		if err != nil {
			return "", err
		}
		if expr.Negated {
			return "!(" + s + ")", nil
		}
		return s, nil

	case expr.IsEvery(), expr.IsSome():
		return "", unsupported("rego", "rego.quantifier",
			"`some`/`every` quantifiers are not translated")

	default:
		// A bare term used as a truthy expression, e.g. `input.enabled`.
		term, ok := expr.Terms.(*ast.Term)
		if !ok {
			return "", unsupported("rego", "rego.expr",
				"unsupported expression shape %T", expr.Terms)
		}
		s, err := t.term(term)
		if err != nil {
			return "", err
		}
		if expr.Negated {
			return "!(" + s + ")", nil
		}
		return s, nil
	}
}

// call renders a built-in call expression (operator + operands).
func (t *regoTranslator) call(expr *ast.Expr) (string, error) {
	op := expr.Operator()
	if op == nil {
		return "", unsupported("rego", "rego.call.no-operator",
			"call expression without an operator")
	}
	name := op.String()
	b, known := ast.BuiltinMap[name]
	if !known {
		return "", unsupported("rego", "rego.builtin:"+name,
			"unknown or user-defined function %q", name)
	}

	celOp, ok := regoInfixToCEL[b.Infix]
	if !ok {
		return "", unsupported("rego", "rego.builtin:"+name,
			"built-in %q (infix %q) has no CEL operator mapping", name, b.Infix)
	}

	operands := expr.Operands()
	if len(operands) != 2 {
		return "", unsupported("rego", "rego.builtin:"+name,
			"expected 2 operands for %q, got %d", name, len(operands))
	}
	l, err := t.term(operands[0])
	if err != nil {
		return "", err
	}
	r, err := t.term(operands[1])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s %s %s)", l, celOp, r), nil
}

// term renders a single term (literal, ref, or nested value).
func (t *regoTranslator) term(term *ast.Term) (string, error) {
	switch v := term.Value.(type) {
	case ast.Boolean:
		if bool(v) {
			return "true", nil
		}
		return "false", nil
	case ast.Number:
		return celNumber(string(v)), nil
	case ast.String:
		return celString(string(v)), nil
	case ast.Null:
		return "null", nil
	case ast.Ref:
		return t.ref(v)
	case ast.Call:
		// A function call appearing as a value (e.g. `count(input.x)` inside a
		// comparison). We only translate the fixed set of infix built-ins via
		// call(); any function used as a value is rejected with its name so the
		// failure is precise and fail-closed.
		name := "?"
		if op := v.Operator(); op != nil {
			name = op.String()
		}
		return "", unsupported("rego", "rego.builtin:"+name,
			"function %q used as a value is not translated", name)
	case ast.Var:
		return "", unsupported("rego", "rego.var",
			"bare variable %q is not translated (dynamic value)", string(v))
	case *ast.Array:
		parts := make([]string, 0, v.Len())
		var rerr error
		v.Foreach(func(el *ast.Term) {
			if rerr != nil {
				return
			}
			s, err := t.term(el)
			if err != nil {
				rerr = err
				return
			}
			parts = append(parts, s)
		})
		if rerr != nil {
			return "", rerr
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case ast.Set:
		var parts []string
		var rerr error
		v.Foreach(func(el *ast.Term) {
			if rerr != nil {
				return
			}
			s, err := t.term(el)
			if err != nil {
				rerr = err
				return
			}
			parts = append(parts, s)
		})
		if rerr != nil {
			return "", rerr
		}
		sort.Strings(parts) // stable; set order is non-deterministic
		return "[" + strings.Join(parts, ", ") + "]", nil
	default:
		return "", unsupported("rego", fmt.Sprintf("rego.term:%T", term.Value),
			"unsupported term value")
	}
}

// ref renders a reference like `input.user.role`. Only static field paths
// rooted at `input` are supported; dynamic (variable-indexed) segments and
// other roots (e.g. `data`) are rejected.
func (t *regoTranslator) ref(r ast.Ref) (string, error) {
	if len(r) == 0 {
		return "", unsupported("rego", "rego.ref.empty", "empty reference")
	}
	head, ok := r[0].Value.(ast.Var)
	if !ok {
		return "", unsupported("rego", "rego.ref.head",
			"reference does not start with a variable")
	}
	root := string(head)
	if root != "input" {
		return "", unsupported("rego", "rego.ref.root:"+root,
			"only `input`-rooted references are translated, got %q", root)
	}
	var b strings.Builder
	b.WriteString("input")
	for _, seg := range r[1:] {
		s, ok := seg.Value.(ast.String)
		if !ok {
			return "", unsupported("rego", "rego.ref.dynamic",
				"dynamic or non-string reference segment is not translated")
		}
		b.WriteByte('.')
		b.WriteString(string(s))
	}
	return b.String(), nil
}

// regoInfixToCEL maps Rego built-in infix operators to their CEL equivalents.
// Only operators with an exact CEL semantic match are listed; anything absent
// causes a fail-closed rejection in call().
var regoInfixToCEL = map[string]string{
	"==": "==",
	"!=": "!=",
	"<":  "<",
	"<=": "<=",
	">":  ">",
	">=": ">=",
	"+":  "+",
	"-":  "-",
	// `=` (unification) and `:=` (assignment) are intentionally excluded:
	// they are not boolean comparisons in the general case.
}

// boolTermValue extracts a boolean literal from a term, reporting whether the
// term was in fact a boolean.
func boolTermValue(term *ast.Term) (val bool, ok bool) {
	if term == nil {
		return false, false
	}
	b, isBool := term.Value.(ast.Boolean)
	if !isBool {
		return false, false
	}
	return bool(b), true
}

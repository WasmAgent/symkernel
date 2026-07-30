# OPA / Cedar compatibility — symkernel as a proof backend

> **Position:** symkernel is a Z3 formal-proof capability for teams already
> using OPA (Rego) or Cedar. Keep your existing policies; gain provable
> guarantees about them. You are **not** asked to migrate to a new policy
> language.

## Why

OPA/Rego and AWS Cedar are the two mainstream authorization policy languages.
Teams have invested in them. symkernel's differentiator is not "another policy
language" — it is **formal proof**: given a policy, prove properties about it
(e.g. "this policy can never grant access to an unverified principal") with an
SMT solver, rather than only evaluating it against concrete inputs.

To deliver that without forcing migration, symkernel accepts existing Rego and
Cedar policies and translates them into its CEL constraint substrate
(`internal/cel`), which the verify/SMT path (`internal/z3`) can then reason
about.

## What the translation layer does

`internal/policyimport` provides two entry points:

```go
func TranslateRego(src, ruleName string) (Result, error)   // ruleName "" ⇒ "allow"
func TranslateCedar(src string) (Result, error)
```

Each parses the source policy with the vendor's own parser
(`github.com/open-policy-agent/opa/v1/ast`, `github.com/cedar-policy/cedar-go`),
walks the resulting AST, and emits a CEL expression string that evaluates to a
boolean decision. The `Result` also carries the policy's `Effect`
(`permit`/`forbid` for Cedar; Rego `allow` rules are `permit`) so callers can
compose allow/deny decisions correctly.

The emitted CEL reads from the same request variables the source language uses:

- **Rego** reads the `input` document → CEL top-level `input`.
- **Cedar** reads `principal`, `action`, `resource`, `context` → CEL top-level
  identifiers of the same names.

## Fail-closed by design

A silent mistranslation is worse than no translation: it would let symkernel
"prove" a property about a policy that does not actually match the source. So
**every construct the translator does not explicitly understand produces an
`*UnsupportedError`, never a best-effort guess.** Callers get either a CEL
string that provably mirrors the source decision, or a precise error naming the
rejected construct (`UnsupportedError.Construct`, e.g. `rego.builtin:count`,
`cedar.node:like`).

### Supported subset

| Feature | Rego | Cedar |
|---|---|---|
| Comparisons `== != < <= > >=` | ✅ | ✅ |
| Boolean `&& \|\| !` | ✅ (implicit `&&` across body; `!` via `not`) | ✅ |
| Arithmetic `+ - *` | `+ -` | `+ - *` |
| Static field paths | `input.a.b` | `principal.x`, `context.y`, … |
| Literals (bool/number/string/set/record) | ✅ | ✅ |
| Membership | — | `in` (modelled via `== ` or `.ancestors`) |
| `has` presence check | — | ✅ (→ CEL `has()`) |
| Entity type test `is` | — | ✅ (→ `.__entity_type ==`) |
| Multiple `allow` rules | ✅ (disjunction) | — (one policy per call) |
| `default allow := false` | ✅ (deny-by-default) | — |

### Deliberately rejected (fail-closed)

- **Rego:** user-defined/unknown built-ins (`count`, `sum`, `regex.match`, …),
  `with` modifiers, `some`/`every` quantifiers, `else`, partial sets/objects,
  functions with args, non-`input` roots (`data.*`), dynamic references,
  `default allow := true` (allow-by-default).
- **Cedar:** `like` wildcard patterns, extension/method calls
  (`decimal`/`ip`/`datetime`/`duration`), entity tags (`hasTag`/`getTag`),
  `isEmpty()`, combined `is … in` in a condition, multi-policy documents.

These are not permanent limitations; they are the honest current boundary.
Anything on this list will translate the day a faithful CEL (or SMT) encoding
is added — until then it is rejected loudly.

## Worked example — Rego rule → CEL → Z3-verified invariant

This is the concrete "provable" story. Start with an ordinary OPA policy:

```rego
package authz
default allow := false
allow if { input.age >= 18 }
```

Translate it:

```go
res, _ := policyimport.TranslateRego(src, "allow")
// res.CEL    == "(((input.age >= 18)))"
// res.Effect == policyimport.EffectPermit
```

The CEL runs on symkernel's existing evaluator (`internal/cel.Evaluate`) — an
age of 21 yields `true`, an age of 16 yields `false`, matching Rego exactly.

Now add the formal proof. We want to guarantee a **safety invariant**: *the
policy can never admit a principal under 18.* Encode the policy guard and the
negation of the invariant as SMT, and ask Z3 whether they can hold together:

```
(declare-const age Int)
(assert (>= age 18))   ; the policy guard
(assert (<  age 18))   ; a principal who is nonetheless admitted while under 18
(check-sat)            ; ⇒ unsat
```

`unsat` means there is **no** age that both satisfies the policy and violates
the invariant — the property is proven, not merely tested. This is exactly what
`internal/z3.SolveConstraints` returns, and it is what symkernel adds on top of
an OPA/Cedar policy: not just "does this input pass?" but "is this property
true for *all* inputs?".

The same flow applies to Cedar:

```cedar
permit(principal, action, resource) when { context.clearance >= 3 };
```

→ CEL guard on `context.clearance` → Z3 proves no clearance below 3 is ever
permitted.

Both worked examples are exercised as tests in
`internal/policyimport/worked_example_test.go` (they skip automatically if the
`z3` binary is not on `PATH`).

## Current boundary between CEL and SMT

symkernel does **not** yet have an automatic CEL→SMT compiler; the CEL and SMT
substrates are separate stages (see `internal/composed`). For the class of
numeric/relational guards shown above the mapping is the identity comparison,
which is what makes the invariant directly provable today. A general
CEL→SMT lowering (so that *any* translated policy, not just numeric guards, can
be proven) is the natural follow-on to this compatibility layer and is tracked
separately — this document and the `policyimport` package are the first half of
that story: getting mainstream policies *into* a form symkernel can reason
about, honestly and fail-closed.

# policy-compliance-fixtures

Six policy-compliance verification tasks that serve as the shared input corpus
for the Milestone 3 comparative analysis in `bench/symbolic-comparison.md`
(parent issue: `bench/symbolic-comparison.md`). Each task describes a single
authorization / resource-access / data-protection policy together with:

- its **policy rules** (the declarative invariant the verifier must enforce),
- its **known paths / branches** (the reachable verdict branches, each with a
  concrete witness input that exercises it),
- a **concrete representation** (a CEL boolean expression runnable today via
  `POST /v1/verify/criterion` with `verify_method: "cel_expr"`), and
- a **symbolic representation** (an SMT-LIB2 formula runnable today via
  `POST /v1/verify/z3`, and the future symbolic-execution engine).

The two representations express the *same* allow-path condition so that
concrete testing and symbolic execution can be compared apples-to-apples on
path coverage and constraint-solving time.

This sub-issue only ships the fixtures. Running both engines over them and
writing the comparison document is the parent issue's job.

## Fixture set

The six tasks are ordered by increasing structural complexity, spanning the
range required by the acceptance criteria (simple condition → complex nested
policy):

| # | File | Domain | Complexity | Branches | Construct introduced |
|---|------|--------|------------|----------|-----------------------|
| 1 | [`auth-minimum-age.json`](auth-minimum-age.json)         | authorization | simple    | 2 | single relational condition |
| 2 | [`rbac-role-check.json`](rbac-role-check.json)           | authorization | simple    | 3 | disjunction / set membership |
| 3 | [`resource-quota.json`](resource-quota.json)             | resource-access | moderate | 2 | linear arithmetic over 3 vars |
| 4 | [`time-window-access.json`](time-window-access.json)     | resource-access | moderate | 3 | nested conjunction of bounds |
| 5 | [`tiered-rate-limit.json`](tiered-rate-limit.json)       | resource-access | complex  | 3 | mixed disjunction + conjunction |
| 6 | [`data-residency-geo.json`](data-residency-geo.json)     | data-protection | complex  | 4 | conjunctive predicate set + negated membership |

Every task carries a `complexity` field; together they cover simple
conditions, moderate constraints, and complex nested policies.

## Per-task summary

### 1. `auth-minimum-age` — Minimum-age authorization gate (simple)
**Policy.** Principal age MUST be >= 18.
**Paths.** `allow` (`age >= 18`, witness `age=18`) · `deny` (`age < 18`, witness `age=12`).
**CEL.** `age >= 18`  ·  **SMT.** `(assert (>= age 18))` → `sat`.
**Expected behavior.** Boundary `age == 18` is allowed (closed lower bound); both branches are independently feasible.

### 2. `rbac-role-check` — Role-based access control (simple, disjunctive)
**Policy.** Role MUST be in `{admin, editor}`.
**Paths.** `allow-admin` (`admin`) · `allow-editor` (`editor`) · `deny-other` (witness `viewer`).
**CEL.** `role == "admin" || role == "editor"`  ·  **SMT.** `(assert (or (= role 1) (= role 2)))` → `sat` (role codes admin=1, editor=2).
**Expected behavior.** Three feasible paths: one per allow-set member plus default deny.

### 3. `resource-quota` — Capacity-arithmetic gate (moderate)
**Policy.** `used + requested <= limit`.
**Paths.** `allow-within-quota` (witness `used=30,requested=20,limit=100`) · `deny-over-quota` (witness `used=90,requested=20,limit=100`).
**CEL.** `used + requested <= limit`  ·  **SMT.** `(assert (<= (+ used requested) limit))` → `sat`.
**Expected behavior.** Boundary (`used + requested == limit`) is allowed; tests additive capacity reasoning rather than equality.

### 4. `time-window-access` — Business-hours window (moderate, nested)
**Policy.** `9 <= hour <= 17` AND `1 <= day <= 5` (Mon–Fri).
**Paths.** `allow-business-hours` (witness `hour=12,day=3`) · `deny-off-hours` (witness `hour=23,day=3`) · `deny-weekend` (witness `hour=12,day=7`).
**CEL.** `hour >= 9 && hour <= 17 && day >= 1 && day <= 5`  ·  **SMT.** conjunction of four bounds → `sat`.
**Expected behavior.** Two independent ranged predicates; each fails on its own sub-path. `day` is 1..7 (1=Mon…7=Sun).

### 5. `tiered-rate-limit` — Tiered limit with burst (complex, mixed)
**Policy.** `request_count <= 100` OR (`request_count <= 200` AND `tier == "premium"`).
**Paths.** `allow-under-base` (witness `count=50,tier=free`) · `allow-premium-burst` (witness `count=150,tier=premium`) · `deny-over-limit` (witness `count=250,tier=premium`).
**CEL.** `request_count <= 100 || (request_count <= 200 && tier == "premium")`  ·  **SMT.** mixed disjunction/conjunction → `sat` (tier code premium=1).
**Expected behavior.** The burst path is reachable only through the nested conjunction; richest branching in the set.

### 6. `data-residency-geo` — Geo-fencing compliance (complex, conjunctive)
**Policy.** `region == "EU"` AND `country` NOT in `{"X","Y"}` AND `encrypted == true`.
**Paths.** `allow-compliant` · `deny-wrong-region` · `deny-blocked-country` · `deny-unencrypted` (witnesses in the file).
**CEL.** `region == "EU" && !(country in ["X", "Y"]) && encrypted == true`  ·  **SMT.** conjunction of equality + two negated equalities + boolean → `sat` (region EU=1; blocked X=10, Y=11).
**Expected behavior.** Most predicate-dense policy; three independent deny sub-paths plus one allow path.

## File format

Each `<task-id>.json` is a self-contained task with this shape:

```jsonc
{
  "id": "auth-minimum-age",            // stable identifier, matches file stem
  "title": "...",
  "category": "authorization",          // authorization | resource-access | data-protection
  "complexity": "simple",               // simple | moderate | complex
  "description": "...",                 // prose: what the policy enforces and why
  "policy_rules": ["..."],              // declarative rules the verifier must enforce
  "expected_behavior": "...",           // allow/deny semantics + boundary behaviour
  "paths": [                            // reachable verdict branches (path coverage map)
    { "id": "allow", "condition": "...", "verdict": "allow"|"deny",
      "feasible": true, "witness": { "...": ... } }
  ],
  "representations": {
    "cel":  { "endpoint": "POST /v1/verify/criterion", "verify_method": "cel_expr",
              "expr": "...", "context_template": { "...": ... } },
    "smt2": { "endpoint": "POST /v1/verify/z3",
              "declarations": "(declare-const ...)", "policy_assertion": "(assert ...)",
              "expected_check_sat": "sat", "notes": "..." }
  },
  "samples": [                          // concrete inputs + expected verdicts (golden cases)
    { "name": "...", "context": { "...": ... }, "expected_cel": true, "expected_path": "allow" }
  ]
}
```

`paths[].witness` and `samples[].context` are concrete inputs that exercise a
specific branch; the symbolic explorer uses the `paths` to enumerate coverage
while the concrete tester uses `samples` (and the CEL `expr`) as golden cases.

## Running the fixtures

### Concrete testing (CEL — available today)

POST a task's CEL representation to the criterion endpoint:

```bash
curl -s localhost:8080/v1/verify/criterion -d '{
  "criterion": {
    "id": "auth-minimum-age",
    "verify_method": "cel_expr",
    "arg": {"expr": "age >= 18", "context": {"age": 25}},
    "level": "hard", "category": "security"
  }
}'
# => {"ok":true,"criterionId":"auth-minimum-age"}
```

`bench/policy_compliance_fixtures_test.go` automates this: it loads every
`*.json` in this directory, then runs each task's CEL `expr` against every
`samples[].context` through the real `internal/cel` evaluator and asserts the
result equals `samples[].expected_cel`. This is the concrete-testing
acceptance criterion, pinned so it cannot silently regress.

### Symbolic execution (SMT-LIB2 — available today via Z3; full engine pending)

POST a task's SMT-LIB2 to the Z3 endpoint:

```bash
curl -s localhost:8080/v1/verify/z3 -d '{
  "input": {"constraints_smt2": "(declare-const age Int) (assert (>= age 18))"}
}'
# => check-sat: sat
```

Each task's `representations.smt2` is pre-shaped for this endpoint
(`declarations` + `policy_assertion`); the documented `expected_check_sat` is
the result for the allow-path formula. Encoding notes (role/region/country
integer codes, day-of-week numbering) are recorded in each file's `smt2.notes`
and in the per-task summaries above so the future `internal/symbolic`
path-explorer can consume them without re-deriving the mapping.

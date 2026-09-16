// Package taint tracks untrusted data through a small, explicit data-flow
// graph and checks security policies with the repository's Z3 integration.
package taint

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/WasmAgent/symkernel/internal/otel"
	"github.com/WasmAgent/symkernel/internal/z3"
	"github.com/google/uuid"
)

const defaultTimeout = 2 * time.Second

// Flow is one directed data-flow edge. Names identify values, variables, or
// operation outputs in the analysis graph.
type Flow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Operation describes a transformation or sink in the data-flow graph. An
// operation is a security-sensitive sink when Sensitive is true. Its name is
// also treated as an output, which makes a compact operation declaration such
// as {"name":"db.write","inputs":["request"],"sensitive":true} useful.
type Operation struct {
	Name       string   `json:"name"`
	Inputs     []string `json:"inputs,omitempty"`
	Outputs    []string `json:"outputs,omitempty"`
	Sensitive  bool     `json:"sensitive,omitempty"`
	Constraint string   `json:"constraint,omitempty"`
}

// Sink is an explicit security-sensitive operation. Sinks are useful when
// the flow graph is supplied independently of the operation list.
type Sink struct {
	Name       string   `json:"name"`
	Inputs     []string `json:"inputs,omitempty"`
	Constraint string   `json:"constraint,omitempty"`
}

// Input is the request accepted by Analyze and POST /v1/verify/taint.
//
// Constraints are execution assumptions when Policies is set. If Policies
// is empty, Constraints are treated as the security policy to prove. A
// policy is safe for a sink only when the assumptions plus its negation are
// unsatisfiable, so a satisfiable query returns a concrete counterexample.
type Input struct {
	Sources             []string            `json:"sources,omitempty"`
	Untrusted           []string            `json:"untrusted,omitempty"`
	Operations          []Operation         `json:"operations,omitempty"`
	Sinks               []Sink              `json:"sinks,omitempty"`
	SensitiveOperations []string            `json:"sensitive_operations,omitempty"`
	Sensitive           []string            `json:"sensitive,omitempty"`
	Flows               []Flow              `json:"flows,omitempty"`
	FlowMap             map[string][]string `json:"flow_map,omitempty"`
	Edges               []Flow              `json:"edges,omitempty"`
	DataFlow            []Flow              `json:"data_flow,omitempty"`
	Constraints         []string            `json:"constraints,omitempty"`
	Policies            []string            `json:"policies,omitempty"`
	PolicyConstraints   []string            `json:"policy_constraints,omitempty"`
	Model               map[string]any      `json:"model,omitempty"`
	TimeoutMs           int                 `json:"timeout_ms,omitempty"`
}

// UnmarshalJSON accepts both the edge-list form and the compact adjacency-map
// form for flows. The latter is useful for clients that already represent a
// graph as source-to-destinations, while Go callers get the typed edge list.
func (in *Input) UnmarshalJSON(data []byte) error {
	type inputAlias Input
	var wire struct {
		*inputAlias
		Flows json.RawMessage `json:"flows"`
	}
	wire.inputAlias = (*inputAlias)(in)
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Flows) == 0 || string(wire.Flows) == "null" {
		return nil
	}
	var edges []Flow
	if err := json.Unmarshal(wire.Flows, &edges); err == nil {
		in.Flows = edges
		return nil
	}
	var graph map[string][]string
	if err := json.Unmarshal(wire.Flows, &graph); err != nil {
		return fmt.Errorf("flows must be an edge list or adjacency map: %w", err)
	}
	in.FlowMap = graph
	return nil
}

// Finding reports whether a tainted value reaches one security-sensitive
// operation and, when a policy was supplied, the Z3 proof outcome.
type Finding struct {
	Sink       string         `json:"sink"`
	Sources    []string       `json:"sources,omitempty"`
	Tainted    bool           `json:"tainted"`
	Safe       bool           `json:"safe"`
	Sat        string         `json:"sat,omitempty"`
	Model      map[string]any `json:"model,omitempty"`
	Constraint string         `json:"constraint,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	SolverMs   int64          `json:"solver_ms,omitempty"`
}

// Result is the taint policy decision. Safe is false for every tainted sink
// with a satisfiable or unknown policy check, and for a tainted sink without a
// policy to check.
type Result struct {
	Safe       bool      `json:"safe"`
	Findings   []Finding `json:"findings,omitempty"`
	Violations []Finding `json:"violations,omitempty"`
	Checked    int       `json:"checked"`
	DecisionID string    `json:"decision_id"`
}

// TaintInput and TaintResult are descriptive aliases for callers that use
// the endpoint terminology.
type TaintInput = Input
type TaintResult = Result

// Solver is the small portion of the Z3 API needed by the taint analyzer.
// It keeps policy analysis unit-testable without starting a Z3 process.
type Solver interface {
	Solve(context.Context, string, map[string]any) (z3.Solution, error)
}

// ContextSolver is accepted by AnalyzeWithSolver for callers that already
// wrap z3.SolveConstraintsCtx directly.
type ContextSolver interface {
	SolveConstraintsCtx(context.Context, string, map[string]any) (z3.Solution, error)
}

type z3Solver struct{}

func (z3Solver) Solve(ctx context.Context, constraints string, model map[string]any) (z3.Solution, error) {
	return z3.SolveConstraintsCtx(ctx, constraints, model)
}

// Analyze tracks source labels through operations and checks every tainted
// sink using Z3.
func Analyze(ctx context.Context, in Input) (Result, error) {
	return AnalyzeWithSolver(ctx, in, z3Solver{})
}

// AnalyzeWithSolver is Analyze with an injectable SMT solver.
func AnalyzeWithSolver(ctx context.Context, in Input, solver any) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if solver == nil {
		return Result{}, fmt.Errorf("taint: solver is required")
	}

	tainted := make(map[string][]string, len(in.Sources)+len(in.Untrusted))
	for _, source := range append(append([]string(nil), in.Sources...), in.Untrusted...) {
		if source = strings.TrimSpace(source); source != "" {
			tainted[source] = appendUnique(tainted[source], source)
		}
	}

	// Apply explicit edges and operations to a fixed point. This permits
	// callers to list operations in any order and prevents an edge from being
	// missed merely because it appears after its destination.
	for changed := true; changed; {
		changed = false
		for _, edge := range in.Flows {
			if len(tainted[edge.From]) > 0 {
				changed = mergeSources(tainted, edge.To, tainted[edge.From]) || changed
			}
		}
		for from, destinations := range in.FlowMap {
			if len(tainted[from]) == 0 {
				continue
			}
			for _, destination := range destinations {
				changed = mergeSources(tainted, destination, tainted[from]) || changed
			}
		}
		for _, edge := range append(append([]Flow(nil), in.Edges...), in.DataFlow...) {
			if len(tainted[edge.From]) > 0 {
				changed = mergeSources(tainted, edge.To, tainted[edge.From]) || changed
			}
		}
		for _, operation := range in.Operations {
			var sources []string
			for _, input := range operation.Inputs {
				sources = appendUniqueAll(sources, tainted[input]...)
			}
			if len(sources) == 0 {
				continue
			}
			if mergeSources(tainted, operation.Name, sources) {
				changed = true
			}
			for _, output := range operation.Outputs {
				if mergeSources(tainted, output, sources) {
					changed = true
				}
			}
		}
	}

	policies := append([]string(nil), in.Policies...)
	if len(policies) == 0 {
		policies = append(policies, in.PolicyConstraints...)
	}
	assumptions := append([]string(nil), in.Constraints...)
	if len(policies) == 0 {
		policies, assumptions = assumptions, nil
	}

	sinks := make([]Sink, 0, len(in.Sinks)+len(in.Operations)+len(in.SensitiveOperations))
	sinks = append(sinks, in.Sinks...)
	for _, operation := range in.Operations {
		if operation.Sensitive {
			sinks = append(sinks, Sink{Name: operation.Name, Inputs: operation.Inputs, Constraint: operation.Constraint})
		}
	}
	for _, name := range append(append([]string(nil), in.SensitiveOperations...), in.Sensitive...) {
		sinks = append(sinks, Sink{Name: name})
	}

	result := Result{Safe: true, DecisionID: uuid.NewString()}
	for _, sink := range sinks {
		if strings.TrimSpace(sink.Name) == "" {
			continue
		}
		var sources []string
		for _, input := range sink.Inputs {
			sources = appendUniqueAll(sources, tainted[input]...)
		}
		if len(sources) == 0 {
			sources = appendUniqueAll(sources, tainted[sink.Name]...)
		}
		if len(sources) == 0 {
			continue
		}

		result.Checked++
		finding := Finding{Sink: sink.Name, Sources: sources, Tainted: true}
		checks := append([]string(nil), policies...)
		if strings.TrimSpace(sink.Constraint) != "" {
			checks = append(checks, sink.Constraint)
		}
		if len(checks) == 0 {
			finding.Reason = "tainted data reaches a sensitive operation without a policy"
			result.Safe = false
			result.Findings = append(result.Findings, finding)
			result.Violations = append(result.Violations, finding)
			continue
		}

		query, err := buildViolationQuery(assumptions, checks)
		if err != nil {
			return Result{}, fmt.Errorf("taint sink %q: %w", sink.Name, err)
		}
		model := inferredModel(query, in.Model)
		start := time.Now()
		solution, err := solve(ctx, solver, query, model)
		if err != nil {
			return Result{}, fmt.Errorf("taint sink %q: solve policy: %w", sink.Name, err)
		}
		finding.Sat = solution.Sat
		finding.Model = solution.Model
		finding.Constraint = query
		finding.SolverMs = elapsedMillis(time.Since(start))
		switch solution.Sat {
		case "unsat":
			finding.Safe = true
			finding.Reason = "Z3 proved the policy violation unreachable"
		default:
			finding.Safe = false
			result.Safe = false
			if solution.Sat == "sat" {
				finding.Reason = "Z3 found a tainted policy-violating model"
			} else {
				finding.Reason = "Z3 could not prove the policy violation unreachable"
			}
		}
		result.Findings = append(result.Findings, finding)
		if !finding.Safe {
			result.Violations = append(result.Violations, finding)
		}
	}
	return result, nil
}

func buildViolationQuery(assumptions, policies []string) (string, error) {
	assumptionExprs := expressions(assumptions)
	policyExprs := expressions(policies)
	if len(policyExprs) == 0 {
		return "", fmt.Errorf("policy is empty")
	}
	var b strings.Builder
	for _, expression := range assumptionExprs {
		fmt.Fprintf(&b, "(assert %s)\n", expression)
	}
	fmt.Fprintf(&b, "(assert (not (and %s)))\n", strings.Join(policyExprs, " "))
	return b.String(), nil
}

func solve(ctx context.Context, solver any, query string, model map[string]any) (z3.Solution, error) {
	switch solver := solver.(type) {
	case Solver:
		return solver.Solve(ctx, query, model)
	case ContextSolver:
		return solver.SolveConstraintsCtx(ctx, query, model)
	default:
		return z3.Solution{}, fmt.Errorf("taint: unsupported solver %T", solver)
	}
}

func expressions(inputs []string) []string {
	var result []string
	for _, input := range inputs {
		for _, line := range strings.Split(input, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "(assert ") && strings.HasSuffix(line, ")") {
				line = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "(assert "), ")"))
			}
			if line != "" {
				result = append(result, line)
			}
		}
	}
	return result
}

var symbolPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.$?/-]*`)

func inferredModel(query string, supplied map[string]any) map[string]any {
	model := make(map[string]any, len(supplied))
	for name, sort := range supplied {
		model[name] = sort
	}
	keywords := map[string]bool{"assert": true, "and": true, "or": true, "not": true, "ite": true, "true": true, "false": true, "let": true, "exists": true, "forall": true}
	for _, symbol := range symbolPattern.FindAllString(query, -1) {
		if !keywords[symbol] && !strings.HasPrefix(symbol, "bv") {
			if _, ok := model[symbol]; !ok {
				model[symbol] = "Int"
			}
		}
	}
	return model
}

func mergeSources(tainted map[string][]string, name string, sources []string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	before := len(tainted[name])
	for _, source := range sources {
		tainted[name] = appendUnique(tainted[name], source)
	}
	return len(tainted[name]) != before
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendUniqueAll(values []string, additions ...string) []string {
	for _, value := range additions {
		values = appendUnique(values, value)
	}
	return values
}

func elapsedMillis(elapsed time.Duration) int64 {
	if elapsed <= 0 {
		return 0
	}
	if millis := elapsed.Milliseconds(); millis > 0 {
		return millis
	}
	return 1
}

// Handler returns the POST /v1/verify/taint handler using Z3. Both a direct
// Input object and the project's common {"input":{...}} envelope are accepted.
func Handler() http.HandlerFunc {
	return HandlerWithSolver(z3Solver{})
}

// HandlerWithSolver exposes dependency injection for endpoint tests and
// callers embedding the analyzer with another Z3-compatible implementation.
func HandlerWithSolver(solver any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		payload := json.RawMessage(nil)
		if input, ok := raw["input"]; ok {
			payload = input
		} else {
			encoded, err := json.Marshal(raw)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
				return
			}
			payload = encoded
		}
		var input Input
		if err := json.Unmarshal(payload, &input); err != nil {
			http.Error(w, fmt.Sprintf("invalid request: %v", err), http.StatusBadRequest)
			return
		}
		timeout := defaultTimeout
		if input.TimeoutMs > 0 {
			timeout = time.Duration(input.TimeoutMs) * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		result, err := AnalyzeWithSolver(ctx, input, solver)
		if err != nil {
			if ctx.Err() != nil {
				http.Error(w, "taint analysis timed out", http.StatusGatewayTimeout)
			} else {
				http.Error(w, fmt.Sprintf("taint analysis failed: %v", err), http.StatusInternalServerError)
			}
			return
		}
		if decisionID := otel.DecisionIDFromContext(r.Context()); decisionID != "" {
			result.DecisionID = decisionID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

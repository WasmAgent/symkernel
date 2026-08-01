# Symbolic benchmark fixtures

Each `*.wasm.b64` file is a base64-encoded WebAssembly binary exporting
`run`. The manifest supplies concrete arguments used to validate that wazero
can execute the module and SMT-LIB path conditions used by the symbolic
benchmark.

The corpus intentionally covers the three control-flow patterns most likely
to cause path growth:

- `bounded-loop`: a decrementing loop with a bounded iteration count.
- `recursion`: a terminating recursive function with a base case.
- `data-dependent-branches`: nested branches controlled by two inputs.

Run `go test -v ./bench/symbolic` for the rendered result table and
`go test -bench . ./bench/symbolic` for repeated measurements.

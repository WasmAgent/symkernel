# Deployment Checklist

Pre-flight and post-deploy verification for symkerneld.

## Prerequisites

- [ ] Go 1.21+ installed (`go version`)
- [ ] Z3 SMT solver installed (`z3 --version`)
- [ ] libz3-dev installed (for CGO build: `apt-get install libz3-dev`)
- [ ] Docker running (for containerized deploys)
- [ ] `SYMKERNEL_CLIENT_TOKEN` generated and stored in secrets manager

## Build

```bash
# Verify CGO is enabled (required for Z3)
echo $CGO_ENABLED          # should be 1 (or unset, defaults to 1 on linux/amd64)

# Build the binary
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o symkerneld ./cmd/symkerneld

# Run tests
go test -short -timeout 120s ./...

# Lint
staticcheck ./...
```

## Container Build

```bash
docker build -t symkernel:latest .

# Verify the image starts
docker run --rm -e SYMKERNEL_CLIENT_TOKEN=test -p 8080:8080 symkernel:latest &
sleep 2
curl -sf http://localhost:8080/v1/audit/stats
```

## Cloudflare Containers

```bash
cd deploy
npx wrangler deploy
```

The `wrangler.toml` configures a single `standard-1` instance (1/2 vCPU, 4 GiB RAM).
See `deploy/wrangler.toml` for sizing notes and `deploy/worker/index.js` for routing.

## Schema Drift Check

```bash
# Pin schemas from wasmagent-js before release
make sync-schemas

# CI gate: verify schemas haven't drifted
make check-schemas
```

## Post-Deploy Verification

```bash
BASE=http://localhost:8080
TOKEN=$SYMKERNEL_CLIENT_TOKEN

# 1. Server is responding
curl -sf -H "Authorization: Bearer $TOKEN" $BASE/v1/audit/stats | jq .

# 2. CEL verification works
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  $BASE/v1/verify/cel \
  -d '{"input":{"expr":"1 + 1"}}' | jq .

# 3. Z3 verification works
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  $BASE/v1/verify/z3 \
  -d '{"assertion":"(declare-const x Int) (assert (> x 0)) (check-sat)"}' | jq .

# 4. Orchestrator routing works
curl -sf -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  $BASE/v1/orchestration/route \
  -d '{"query":"x > 0","context":{"x":1}}' | jq .

# 5. Cache stats endpoint responds
curl -sf -H "Authorization: Bearer $TOKEN" $BASE/v1/admin/cache/stats | jq .

# 6. Diagnostics endpoint responds (use a real decision_id from step 2 or 3)
curl -sf -H "Authorization: Bearer $TOKEN" \
  $BASE/v1/diagnostics/<decision_id> | jq .
```

## Rollback Procedure

1. Re-deploy the previous image tag or binary
2. Verify health using the post-deploy checks above
3. If schema drift was introduced, run `make sync-schemas` at the previous commit

## Security Notes

- `SYMKERNEL_CLIENT_TOKEN` must be set in production. When unset, auth middleware
  passes all requests through — suitable only for local development.
- Token is compared as a constant-time string via the `Authorization: Bearer` header.
- The Z3 solver runs as a subprocess (`z3 -in`); ensure the `z3` binary in `$PATH`
  is from a trusted package manager.
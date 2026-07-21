# Environment Variable Reference

All configuration for symkerneld is via environment variables.

## Server

| Variable | Default | Description |
|---|---|---|
| `SYMKERNEL_ADDR` | `:8080` | Listen address and port (e.g. `:9090`, `0.0.0.0:8080`) |

## Authentication

| Variable | Default | Description |
|---|---|---|
| `SYMKERNEL_CLIENT_TOKEN` | _(empty)_ | Bearer token for API authentication. **Must be set in production.** When empty, all requests pass through (local-dev mode). |

## Build-Time

| Variable | Default | Description |
|---|---|---|
| `CGO_ENABLED` | `1` (linux/amd64) | Must be `1` for Z3 SMT support. The go-z3 package uses CGO. Set to `0` to produce a CEL-only binary. |
| `PKG_CONFIG_PATH` | _(system)_ | Required only if libz3 is installed to a non-standard prefix, so `pkg-config` can locate `z3.pc` for correct `-I`/`-L` flags. |

## Planned (Milestone 8)

The following variables are defined in the Milestone 8 roadmap but not yet implemented:

| Variable | Planned Default | Description |
|---|---|---|
| `SYMKERNEL_RATE_LIMIT_QPS` | 100 | Token-bucket rate limit (queries per second) per client token |
| `SYMKERNEL_RATE_LIMIT_BURST` | 200 | Burst allowance for rate limiter |
| `SYMKERNEL_DECISION_LOG_PATH` | `/var/log/symkernel/decisions.jsonl` | Path for the append-only decision log |
| `SYMKERNEL_DECISION_LOG_BUCKET` | _(empty)_ | S3 bucket for daily decision log upload (optional) |

## Docker Image Defaults

The Dockerfile sets `SYMKERNEL_ADDR=:8080` and exposes port 8080. Override at runtime:

```bash
docker run -e SYMKERNEL_ADDR=:9090 -e SYMKERNEL_CLIENT_TOKEN=secret symkernel:latest
```

## Auth Behaviour

```
SYMKERNEL_CLIENT_TOKEN=""   → all requests accepted (dev mode)
SYMKERNEL_CLIENT_TOKEN="x"  → only requests with "Authorization: Bearer x" accepted
```

See `internal/auth/middleware.go` for the implementation.
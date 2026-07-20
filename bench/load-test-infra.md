# Z3 Load-Test Infrastructure

This document covers the load-testing infrastructure for the Z3 SMT solver
endpoint (`POST /v1/verify/z3`) deployed on Cloudflare Containers. It is the
prerequisite for the M3 measurement task (`bench/z3-load-test.md`, tracked
separately): the scripts and commands here let an operator measure p99
latency and scale-to-zero cold start under 10 concurrent Z3 requests, then
apply the M3 decision gate.

## Components

| Path | Purpose |
|------|---------|
| `bench/k6/z3-load.js` | k6 load test — 10 concurrent VUs, captures p99. |
| `bench/k6/z3-cold-start.js` | k6 cold-start probe — sequential requests, captures cold-start. |
| `deploy/wrangler.toml` | Cloudflare Containers deployment (binding `SYMKERNEL`). |
| `deploy/worker/index.js` | Worker entrypoint routing HTTP → the symkerneld Container. |
| `deploy/worker/package.json` | Worker npm manifest (`@cloudflare/containers`). |
| `Dockerfile` | symkerneld image (includes the `z3` binary + libz3). Built by wrangler from the repo root. |

## Endpoints

### Cloudflare Containers (deployed)
- **URL:** the Worker endpoint Cloudflare assigns on first deploy — a
  `*.workers.dev` URL whose host begins with the Worker name `symkernel`
  (see `name` in `deploy/wrangler.toml`). The full path is
  `https://symkernel.YOUR-SUBDOMAIN.workers.dev/v1/verify/z3`, where
  `YOUR-SUBDOMAIN` is your account's workers.dev subdomain (printed by
  `wrangler deploy`).
- The Worker (`deploy/worker/index.js`) is the public endpoint; it forwards
  every request to a single Container instance running the symkerneld image.
- **Auth:** `Authorization: Bearer <SYMKERNEL_CLIENT_TOKEN>` (Worker secret;
  see [Deploy](#deploy)).

### Local (docker-compose)
- **URL:** `http://localhost:${SYMKERNEL_PORT:-8080}/v1/verify/z3`
- Start: `docker compose up`
- **Auth:** `Authorization: Bearer <SYMKERNEL_CLIENT_TOKEN>` (set on the
  `symkerneld` service; the middleware rejects all requests if unset).

## Request / response format

Request (OPA envelope):
```json
{
  "input": {
    "constraints_smt2": "(declare-const x Int) (assert (> x 5))",
    "timeout_ms": 2000
  }
}
```
Response:
```json
{
  "result": { "sat": "sat", "model": { "x": "6" } },
  "decision_id": "<uuid>"
}
```

## Prerequisites

- **k6:** install from <https://k6.io/docs/get-started/installation/>
- **Docker:** must be running locally — `wrangler deploy` builds the
  container image via Docker (see [Deploy](#deploy)).

Both k6 scripts read these environment variables:
```sh
export SYMKERNEL_BASE_URL=https://symkernel.YOUR-SUBDOMAIN.workers.dev   # or http://localhost:8080
export SYMKERNEL_CLIENT_TOKEN=<your token>
```

## <a name="deploy"></a>Deploy (Cloudflare Containers)

`deploy/wrangler.toml` points the container `image` at `../Dockerfile`
(the repo-root Dockerfile), so a single `wrangler deploy` builds the
symkerneld image (with Z3 baked in), pushes it to Cloudflare's registry,
and deploys the Worker — no image tag or account ID is hard-coded anywhere.

1. **Install Worker deps & set the auth secret** (the token clients will
   send; must match on both sides):
   ```sh
   cd deploy/worker
   npm install
   npx wrangler secret put SYMKERNEL_CLIENT_TOKEN -c ../wrangler.toml   # paste the token
   ```
2. **Deploy** (from `deploy/worker/`; Docker must be running):
   ```sh
   npx wrangler deploy -c ../wrangler.toml
   ```
   wrangler prints the deployed `*.workers.dev` URL — use it as
   `SYMKERNEL_BASE_URL` for the k6 scripts below.
3. Wait a few minutes for the first Container instance to provision.

## Test commands

### p99 under load (10 concurrent)
```sh
k6 run --vus 10 --duration 30s bench/k6/z3-load.js
```
The script defaults to 10 VUs / 30s; override with `--vus` / `--duration`.

### Cold start (scale-to-zero)
First ensure the container is stopped (wait longer than `sleepAfter` (5m),
or stop it via the dashboard / `wrangler containers` CLI), then:
```sh
k6 run bench/k6/z3-cold-start.js
```

## Expected output format

### `z3-load.js`
k6 prints a summary; the lines of interest:
```
     checks.........................: 100.00% ✓ 3000 ✗ 0
     http_req_duration..............: avg=120ms min=80ms med=110ms max=900ms p(90)=180ms p(95)=220ms p(99)=340ms
     http_req_failed................: 0.00%   ✓ 0    ✗ 3000
     z3_solve_duration..............: avg=118ms min=80ms med=110ms max=900ms p(95)=220ms p(99)=340ms
   ✓ http_req_duration..............: p(99)<2000
   ✓ http_req_failed................: rate<0.01
```
**p99** = the `p(99)=...` value on the `http_req_duration` line.

### `z3-cold-start.js`
```
     request_duration...............: avg=1100ms min=90ms med=120ms max=4200ms p(90)=120ms p(95)=120ms
   ✓ request_duration...............: max<5000
```
**Cold start** = the `max=...` value on the `request_duration` line (the
first, cold request dominates the max; later iterations are warm). Subtract
the warm `med` to estimate cold-start overhead.

## Decision gate (M3)

Per `docs/15-milestones.md` M3:

- if **p99 > 2s** (`z3-load.js` fails `p(99)<2000`), **or**
- **scale-to-zero cold start > 5s** (`z3-cold-start.js` fails `max<5000`),

then document the migration path to a self-hosted VPS in
`bench/z3-load-test.md` (the measurement issue), and consider bumping
`instance_type` / `max_instances` in `deploy/wrangler.toml`.

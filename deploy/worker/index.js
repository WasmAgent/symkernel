// Cloudflare Worker entrypoint for symkernel.
//
// This Worker is the public endpoint for the deployment (Cloudflare
// assigns a *.workers.dev URL on first deploy). It routes every request
// to a single Container instance that runs the symkerneld image
// (../Dockerfile, built by wrangler from the repo root) and serves
// POST /v1/verify/z3.
//
// Deploy: see bench/load-test-infra.md. Run from deploy/worker/ after
// `npm install`:
//   npx wrangler deploy -c ../wrangler.toml

import { Container, getContainer } from "@cloudflare/containers";
import { env } from "cloudflare:workers";

// Container class — one instance per deployment (singleton routing via
// getContainer's default name). The instance runs the symkerneld image,
// which listens on :8080 (SYMKERNEL_ADDR=:8080 in the repo-root Dockerfile).
export class SymkernelContainer extends Container {
  // symkerneld listens on :8080; the Container class proxies fetch() here.
  defaultPort = 8080;
  // Scale the instance to zero after 5 minutes idle. This makes
  // scale-to-zero cold start measurable (bench/k6/z3-cold-start.js) while
  // keeping the instance warm across short gaps in normal traffic.
  sleepAfter = "5m";
  // Forward the auth token to the container so its Bearer middleware
  // (internal/auth/middleware.go) can validate Authorization headers.
  // SYMKERNEL_CLIENT_TOKEN is a Worker secret (set via `wrangler secret put`).
  envVars = {
    SYMKERNEL_ADDR: ":8080",
    SYMKERNEL_CLIENT_TOKEN: env.SYMKERNEL_CLIENT_TOKEN,
  };
}

export default {
  // Stateless singleton routing: every request goes to the same container
  // instance (getContainer with no name uses the default singleton). The
  // incoming request — including its Authorization header — is forwarded
  // unchanged, so the container's own auth middleware enforces the token.
  async fetch(request, workerEnv) {
    return getContainer(workerEnv.SYMKERNEL).fetch(request);
  },
};

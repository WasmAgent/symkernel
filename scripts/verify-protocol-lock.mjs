#!/usr/bin/env node
// SK-2: verify the downloaded @wasmagent/protocol tarball against the pinned
// vendor lock (schemas/protocol.lock.json) before its schemas are trusted.
//
// Usage: node scripts/verify-protocol-lock.mjs [tarball]
// Exits 0 on match, 1 on mismatch/missing lock.

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const lockPath = join(here, "..", "schemas", "protocol.lock.json");
const lock = JSON.parse(readFileSync(lockPath, "utf8"));

const tarball =
  process.argv[2] ?? `wasmagent-protocol-${lock.version}.tgz`;

let bytes;
try {
  bytes = readFileSync(tarball);
} catch {
  console.error(`verify-protocol-lock: tarball not found: ${tarball}`);
  process.exit(1);
}

const sha256 = createHash("sha256").update(bytes).digest("hex");
const sha512 = createHash("sha512").update(bytes).digest("hex");

if (lock.version !== "0.1.10") {
  console.error(
    `verify-protocol-lock: lock version ${lock.version} is not the certified 0.1.10`,
  );
  process.exit(1);
}
if (sha256 !== lock.tarball_sha256 || sha512 !== lock.tarball_sha512) {
  console.error("verify-protocol-lock: tarball digest mismatch");
  console.error(`  expected sha256 ${lock.tarball_sha256}`);
  console.error(`  actual   sha256 ${sha256}`);
  console.error(`  expected sha512 ${lock.tarball_sha512}`);
  console.error(`  actual   sha512 ${sha512}`);
  process.exit(1);
}

console.log(
  `verify-protocol-lock: ${lock.package}@${lock.version} verified (sha256 ${sha256.slice(0, 16)}…)`,
);

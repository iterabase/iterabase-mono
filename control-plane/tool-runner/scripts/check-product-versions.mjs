#!/usr/bin/env node
// Product tool versions are immutable (ARCH-007, DES-HOR-651-01): the gateway
// rejects a registration that reuses a published name+version with another
// digest, which would fail an upgraded runner at install time. Fail the build
// instead when a product tool changes bytes without a new version.
//
// Usage: check-product-versions.mjs [base-ref]   (default: origin/master)
// The base ref's full history is required (CI checks out with fetch-depth 0).
import { execFileSync } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const productRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "tools", "product");
const baseRef = process.argv[2] ?? process.env.PRODUCT_TOOLS_BASE_REF ?? "origin/master";

function git(...args) {
  return execFileSync("git", args, { cwd: top, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });
}
const top = execFileSync("git", ["rev-parse", "--show-toplevel"], { cwd: productRoot, encoding: "utf8" }).trim();

try {
  git("rev-parse", "--verify", `${baseRef}^{commit}`);
} catch (error) {
  console.error(`cannot resolve ${baseRef}; fetch it (CI checks out full history): ${error.stderr ?? error.message}`);
  process.exit(1);
}

// Every version ever published on the base branch stays reserved, including
// versions a later commit superseded: the gateway keeps them forever.
const prefix = relative(top, productRoot);
const published = new Map();
for (const commit of git("rev-list", baseRef, "--", prefix).split("\n").filter(Boolean)) {
  for (const path of git("ls-tree", "-r", "--name-only", commit, "--", prefix).split("\n")) {
    if (!path.endsWith("/manifest.json")) continue;
    const manifest = JSON.parse(git("show", `${commit}:${path}`));
    published.set(`${manifest.name}@${manifest.version}`, manifest.digest);
  }
}

const violations = [];
for (const entry of readdirSync(productRoot, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const manifest = JSON.parse(readFileSync(join(productRoot, entry.name, "manifest.json"), "utf8"));
  const key = `${manifest.name}@${manifest.version}`;
  const digest = published.get(key);
  if (digest !== undefined && digest !== manifest.digest) {
    violations.push(`${key}: digest changed from ${digest} to ${manifest.digest}; publish a new version`);
  }
}
if (violations.length) {
  console.error(`product tool versions published on ${baseRef} are immutable:\n  ${violations.join("\n  ")}`);
  process.exit(1);
}
console.log(`OK: ${published.size} product tool version(s) published on ${baseRef} are unchanged`);

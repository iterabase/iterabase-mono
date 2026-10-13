import {
  cpSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { basename, extname, join, relative } from "node:path";
import { unzipSync, zipSync } from "fflate";
import { requiredEnv } from "./env";

function secretVariants(secret: string): Buffer[] {
  return [Buffer.from(secret), Buffer.from(secret).toString("base64")].map(
    (value) => Buffer.from(value),
  );
}

function scrubBytes(
  input: Uint8Array,
  variants: Buffer[],
): Uint8Array<ArrayBuffer> {
  const output = new Uint8Array(input.length);
  output.set(input);
  const view = Buffer.from(output.buffer);
  for (const variant of variants) {
    let offset = view.indexOf(variant);
    while (offset >= 0) {
      view.fill("*".charCodeAt(0), offset, offset + variant.length);
      offset = view.indexOf(variant, offset + variant.length);
    }
  }
  return output;
}

function sanitizeTrace(path: string, variants: Buffer[]): void {
  const entries = unzipSync(readFileSync(path));
  for (const [name, content] of Object.entries(entries)) {
    entries[name] = scrubBytes(content, variants);
  }
  writeFileSync(path, zipSync(entries, { level: 6 }), { mode: 0o600 });
}

function walkFiles(root: string): string[] {
  const files: string[] = [];
  for (const name of readdirSync(root)) {
    const path = join(root, name);
    if (statSync(path).isDirectory()) files.push(...walkFiles(path));
    else files.push(path);
  }
  return files;
}

function assertNoSecret(path: string, variants: Buffer[]): void {
  if (basename(path) === "trace.zip") {
    const entries = unzipSync(readFileSync(path));
    for (const [name, content] of Object.entries(entries)) {
      for (const variant of variants) {
        if (Buffer.from(content).includes(variant)) {
          throw new Error(
            `sanitized trace entry still contains a credential: ${name}`,
          );
        }
      }
    }
    return;
  }
  const content = readFileSync(path);
  for (const variant of variants) {
    if (content.includes(variant)) {
      throw new Error(`browser artifact still contains a credential: ${path}`);
    }
  }
}

export default function globalTeardown(): void {
  const root = requiredEnv("ITERABASE_BROWSER_ARTIFACT_ROOT");
  const raw = join(root, "raw");
  const safe = join(root, "safe-opaque");
  const variants = secretVariants(requiredEnv("ITERABASE_BROWSER_PASSWORD"));

  rmSync(safe, { recursive: true, force: true });
  for (const path of walkFiles(raw)) {
    if (basename(path) === "trace.zip") {
      sanitizeTrace(path, variants);
    } else if (extname(path) !== ".png") {
      writeFileSync(path, scrubBytes(readFileSync(path), variants), {
        mode: 0o600,
      });
    }
    assertNoSecret(path, variants);
  }
  cpSync(raw, safe, { recursive: true, force: true });
  for (const path of walkFiles(safe)) assertNoSecret(path, variants);

  const network = join(root, "network.jsonl");
  writeFileSync(network, scrubBytes(readFileSync(network), variants), {
    mode: 0o600,
  });
  assertNoSecret(network, variants);
  rmSync(raw, { recursive: true, force: true });
  writeFileSync(
    join(root, "sanitized.marker"),
    `Playwright failure evidence sanitized; published root=${relative(root, safe) || "safe-opaque"}\n`,
    { mode: 0o600 },
  );
}

import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { chmod, copyFile, lstat, mkdir, readFile, readdir, rename, rm, stat, writeFile } from "node:fs/promises";
import { basename, dirname, join, posix, resolve } from "node:path";
import { Readable, Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import * as k8s from "@kubernetes/client-node";
import * as tar from "tar";
import type { MaterializerMetrics } from "./metrics.js";
import { sleep } from "./sleep.js";

export interface MaterializerConfig {
  // Release-versioned product layer baked into the tool-runner image
  // (DES-HOR-651-01). It is read-only and identical for every pod of a release.
  productRoot: string;
  productVersion: string;
  // Optional overlay client layer. Absent means a product-only generation.
  flux?: { namespace: string; sourceName: string };
  artifactsRoot: string;
  controlRoot: string;
  pollMs: number;
  maxArchiveBytes: number;
  maxExtractedBytes: number;
  maxGenerations: number;
}

export interface FluxArtifact { revision: string; digest: string; url: string }

// GenerationSource is one composed generation: the image product layer plus
// the exact Flux client artifact, if any. Its digest names the generation.
export interface GenerationSource { revision: string; digest: string; productDigest: string; client: FluxArtifact | null }

export class GenerationMaterializer {
  private custom: k8s.CustomObjectsApi | null = null;
  private lastDigest = "";
  private productDigest = "";

  constructor(private readonly cfg: MaterializerConfig, kubeConfig?: k8s.KubeConfig, private readonly metrics?: MaterializerMetrics) {
    if (!cfg.flux) return;
    const kc = kubeConfig ?? new k8s.KubeConfig();
    if (!kubeConfig) kc.loadFromCluster();
    this.custom = kc.makeApiClient(k8s.CustomObjectsApi);
  }

  async run(signal: AbortSignal): Promise<void> {
    await mkdir(join(this.cfg.artifactsRoot, "generations"), { recursive: true });
    await mkdir(this.cfg.controlRoot, { recursive: true });
    while (!signal.aborted) {
      const observedAt = Date.now();
      try {
        const source = await this.currentSource();
        if (source && source.digest !== this.lastDigest) {
          const bytes = await this.materialize(source);
          this.lastDigest = source.digest;
          this.metrics?.success(source.revision, source.digest, bytes, (Date.now() - observedAt) / 1000);
        }
      } catch (error) {
        this.metrics?.failure((Date.now() - observedAt) / 1000);
        console.error(JSON.stringify({ event: "materialization_failed", message: error instanceof Error ? error.message : String(error) }));
      }
      await sleep(this.cfg.pollMs, signal);
    }
  }

  // currentSource returns null only while a configured Flux client source has
  // no Ready artifact: a configured overlay is never silently skipped.
  async currentSource(): Promise<GenerationSource | null> {
    if (!this.productDigest) this.productDigest = await layerDigest(this.cfg.productRoot);
    const client = this.cfg.flux ? await this.currentArtifact() : null;
    if (this.cfg.flux && !client) return null;
    return composeSource(this.productDigest, this.cfg.productVersion, client);
  }

  async currentArtifact(): Promise<FluxArtifact | null> {
    if (!this.custom || !this.cfg.flux) return null;
    const response = await this.custom.getNamespacedCustomObject({
      group: "source.toolkit.fluxcd.io", version: "v1", namespace: this.cfg.flux.namespace,
      plural: "gitrepositories", name: this.cfg.flux.sourceName,
    });
    const object = response as unknown as { status?: { artifact?: Partial<FluxArtifact> } };
    const artifact = object.status?.artifact;
    if (!artifact?.revision || !artifact.digest || !artifact.url) return null;
    if (!/^sha256:[a-f0-9]{64}$/.test(artifact.digest)) throw new Error(`Flux artifact has unsupported digest ${artifact.digest}`);
    const url = new URL(artifact.url);
    if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("Flux artifact URL must be HTTP(S)");
    // Prevent a compromised status object from turning the materializer into an
    // arbitrary egress client. Flux's source-controller artifact host is fixed.
    if (!/^source-controller\.flux-system\.svc(?:\.cluster\.local)?\.?$/.test(url.hostname)) throw new Error(`Flux artifact host ${url.hostname} is not source-controller`);
    return artifact as FluxArtifact;
  }

  async materialize(source: GenerationSource): Promise<number> {
    const generationName = source.digest.slice("sha256:".length);
    const finalDir = join(this.cfg.artifactsRoot, "generations", generationName);
    try {
      if ((await stat(finalDir)).isDirectory()) { await this.publishCurrent(source, finalDir); return directoryBytes(finalDir); }
    } catch { /* absent */ }
    await this.reclaimReleased(generationName);
    const dirs = await generationDirs(this.cfg.artifactsRoot);
    if (dirs.length >= this.cfg.maxGenerations) throw new Error(`generation limit ${this.cfg.maxGenerations} reached while retained versions are pinned`);

    const archive = join(this.cfg.artifactsRoot, `.artifact-${generationName}.tgz`);
    const staging = join(this.cfg.artifactsRoot, `.staging-${generationName}`);
    await rm(archive, { force: true });
    await removeReadOnlyTree(staging);
    try {
      let expanded = 0;
      if (source.client) {
        await this.download(source.client, archive);
        expanded += await validateArchive(archive, this.cfg.maxExtractedBytes);
      }
      expanded += await directoryBytes(this.cfg.productRoot);
      const currentBytes = await totalGenerationBytes(this.cfg.artifactsRoot);
      if (currentBytes + expanded > this.cfg.maxExtractedBytes) throw new Error(`extracted generation storage would exceed ${this.cfg.maxExtractedBytes} bytes`);
      await mkdir(join(staging, "tools"), { recursive: true, mode: 0o700 });
      await copyLayer(this.cfg.productRoot, join(staging, "tools", "product"));
      if (source.client) {
        await tar.x({ file: archive, cwd: staging, gzip: true, filter: (path, entry) => allowedToolPath(path) && "type" in entry && entry.type === "File", noMtime: true });
      }
      // The product layer must match the digest this generation is named by;
      // the image is immutable, so a mismatch means the copy was corrupted.
      const copied = await layerDigest(join(staging, "tools", "product"));
      if (copied !== source.productDigest) throw new Error(`product layer digest mismatch: expected ${source.productDigest}, copied ${copied}`);
      // The runner container mounts /artifacts read-only. Keep materializer
      // ownership on the writable side so released generations can be reaped;
      // atomic rename is the publication boundary.
      await rename(staging, finalDir);
      await this.publishCurrent(source, finalDir);
      console.info(JSON.stringify({ event: "generation_materialized", revision: source.revision, digest: source.digest,
        productDigest: source.productDigest, clientDigest: source.client?.digest ?? null, bytes: expanded }));
      return expanded;
    } finally {
      await rm(archive, { force: true });
      await removeReadOnlyTree(staging);
    }
  }

  private async download(artifact: FluxArtifact, destination: string): Promise<void> {
    let response: Response;
    try {
      response = await fetch(artifact.url, { redirect: "error" });
    } catch (error) {
      const cause = (error as { cause?: { code?: string; message?: string } }).cause;
      throw new Error(`download Flux artifact ${artifact.url}: ${cause?.code ?? "fetch_failed"}: ${cause?.message ?? (error instanceof Error ? error.message : String(error))}`);
    }
    if (!response.ok || !response.body) throw new Error(`download Flux artifact: HTTP ${response.status}`);
    const claimedLength = Number(response.headers.get("content-length") ?? "0");
    if (claimedLength > this.cfg.maxArchiveBytes) throw new Error(`Flux archive exceeds ${this.cfg.maxArchiveBytes} bytes`);
    let size = 0;
    const maximum = this.cfg.maxArchiveBytes;
    const hash = createHash("sha256");
    const meter = new Transform({ transform(chunk: Buffer, _encoding, callback) {
      size += chunk.length;
      if (size > maximum) { callback(new Error(`Flux archive exceeds ${maximum} bytes`)); return; }
      hash.update(chunk); callback(null, chunk);
    } });
    await pipeline(Readable.fromWeb(response.body as never), meter, createWriteStream(destination, { mode: 0o600 }));
    const actual = `sha256:${hash.digest("hex")}`;
    if (actual !== artifact.digest) throw new Error(`Flux artifact digest mismatch: status ${artifact.digest}, downloaded ${actual}`);
  }

  private async publishCurrent(source: GenerationSource, directory: string): Promise<void> {
    const tmp = join(this.cfg.controlRoot, "current.json.tmp");
    await writeFile(tmp, JSON.stringify({ revision: source.revision, artifactDigest: source.digest, directory }), { mode: 0o600 });
    await rename(tmp, join(this.cfg.controlRoot, "current.json"));
  }

  private async reclaimReleased(incoming: string): Promise<void> {
    let retained = new Set<string>();
    try {
      const state = JSON.parse(await readFile(join(this.cfg.controlRoot, "runner-state.json"), "utf8")) as { retained?: string[] };
      retained = new Set((state.retained ?? []).map((digest) => digest.replace(/^sha256:/, "")));
    } catch { /* runner has not published state */ }
    // Never reap the currently published generation before its replacement has
    // downloaded and verified. The runner state can be absent or briefly stale
    // during startup; current.json is the materializer's own last-valid commit.
    try {
      const current = JSON.parse(await readFile(join(this.cfg.controlRoot, "current.json"), "utf8")) as { artifactDigest?: string };
      if (current.artifactDigest) retained.add(current.artifactDigest.replace(/^sha256:/, ""));
    } catch { /* no valid generation has been published yet */ }
    for (const dir of await generationDirs(this.cfg.artifactsRoot)) {
      const name = basename(dir);
      if (name !== incoming && !retained.has(name)) await rm(dir, { recursive: true, force: true });
    }
  }
}

export async function validateArchive(file: string, maxBytes: number): Promise<number> {
  const seen = new Set<string>();
  let total = 0;
  let invalid: Error | undefined;
  await tar.t({ file, gzip: true, onentry(entry) {
    if (invalid) return;
    const normalized = posix.normalize(entry.path);
    if (entry.path.startsWith("/") || normalized === ".." || normalized.startsWith("../") || normalized.includes("/../")) invalid = new Error(`unsafe archive path ${entry.path}`);
    else if (seen.has(normalized)) invalid = new Error(`duplicate archive path ${normalized}`);
    else if (["SymbolicLink", "Link", "CharacterDevice", "BlockDevice", "FIFO"].includes(entry.type)) invalid = new Error(`unsupported archive entry ${normalized} (${entry.type})`);
    else if (productPath(normalized)) invalid = new Error(`overlay tools/product is not allowed (${normalized}): product tools ship with the platform release; put customer tools in tools/client`);
    else {
      seen.add(normalized);
      if (entry.type === "File" && allowedToolPath(normalized)) {
        total += entry.size;
        if (total > maxBytes) invalid = new Error(`tool archive expands beyond ${maxBytes} bytes`);
      }
    }
  } });
  if (invalid) throw invalid;
  return total;
}

function allowedToolPath(path: string): boolean {
  const normalized = posix.normalize(path).replace(/^\.\//, "");
  return normalized === "tools" || normalized === "tools/client" || normalized.startsWith("tools/client/");
}

function productPath(path: string): boolean {
  const normalized = path.replace(/^\.\//, "").replace(/\/$/, "");
  return normalized === "tools/product" || normalized.startsWith("tools/product/");
}

// composeSource names a generation by both layers, so a platform upgrade (new
// product digest) and an overlay change (new Flux digest) each publish exactly
// one new immutable generation.
export function composeSource(productDigest: string, productVersion: string, client: FluxArtifact | null): GenerationSource {
  const hash = createHash("sha256");
  hash.update(`iterabase.io/tool-generation/v1\0product\0${productDigest}\0client\0${client?.digest ?? "none"}`);
  const revision = `product@${productVersion}` + (client ? `+client@${client.revision}` : "");
  return { revision, digest: `sha256:${hash.digest("hex")}`, productDigest, client };
}

// layerDigest hashes every regular file of a layer by sorted relative path.
// Links and special files are rejected: the layer is copied verbatim.
export async function layerDigest(root: string): Promise<string> {
  const hash = createHash("sha256");
  hash.update("iterabase.io/tool-layer/v1\0");
  for (const path of await layerFiles(root)) {
    const bytes = await readFile(join(root, path));
    hash.update(`${path}\0${bytes.byteLength}\0`);
    hash.update(bytes);
  }
  return `sha256:${hash.digest("hex")}`;
}

async function layerFiles(root: string): Promise<string[]> {
  const out: string[] = [];
  async function walk(relative: string): Promise<void> {
    for (const entry of await readdir(join(root, relative), { withFileTypes: true })) {
      const child = relative ? `${relative}/${entry.name}` : entry.name;
      const info = await lstat(join(root, child));
      if (info.isDirectory()) await walk(child);
      else if (info.isFile()) out.push(child);
      else throw new Error(`product layer entry ${child} is not a regular file or directory`);
    }
  }
  await walk("");
  return out.sort();
}

async function copyLayer(source: string, destination: string): Promise<void> {
  await mkdir(destination, { recursive: true, mode: 0o700 });
  for (const path of await layerFiles(source)) {
    await mkdir(dirname(join(destination, path)), { recursive: true, mode: 0o700 });
    await copyFile(join(source, path), join(destination, path));
  }
}

async function removeReadOnlyTree(root: string): Promise<void> {
  try {
    await chmod(root, 0o700);
    for (const entry of await readdir(root, { withFileTypes: true })) {
      const path = join(root, entry.name);
      if (entry.isDirectory()) await removeReadOnlyTree(path);
      else { await chmod(path, 0o600); await rm(path, { force: true }); }
    }
    await rm(root, { recursive: true, force: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
}

async function generationDirs(root: string): Promise<string[]> {
  const parent = join(root, "generations");
  try { return (await readdir(parent, { withFileTypes: true })).filter((entry) => entry.isDirectory()).map((entry) => join(parent, entry.name)).sort(); }
  catch { return []; }
}
async function directoryBytes(root: string): Promise<number> {
  let total = 0;
  async function walk(path: string): Promise<void> { for (const entry of await readdir(path, { withFileTypes: true })) { const child = join(path, entry.name); if (entry.isDirectory()) await walk(child); else if (entry.isFile()) total += (await stat(child)).size; } }
  await walk(root);
  return total;
}
async function totalGenerationBytes(root: string): Promise<number> {
  let total = 0;
  for (const dir of await generationDirs(root)) total += await directoryBytes(dir);
  return total;
}

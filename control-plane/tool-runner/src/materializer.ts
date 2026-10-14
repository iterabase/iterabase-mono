import { createHash } from "node:crypto";
import { chmod, copyFile, lstat, mkdir, readFile, readdir, rename, rm, stat, writeFile } from "node:fs/promises";
import { basename, dirname, join, posix } from "node:path";
import * as k8s from "@kubernetes/client-node";
import * as tar from "tar";
import type { MaterializerMetrics } from "./metrics.js";
import { sleep } from "./sleep.js";

export interface MaterializerConfig {
  // Release-versioned product layer baked into the tool-runner image
  // (DES-HOR-651-01). It is read-only and identical for every pod of a release.
  productRoot: string;
  productVersion: string;
  // Optional overlay client layer, read from the Forge-written pointer and
  // digest-named archive ConfigMaps (DES-HOR-593-05). Absent means a
  // product-only generation and no Kubernetes client at all.
  overlay?: { namespace: string; pointerName: string };
  artifactsRoot: string;
  controlRoot: string;
  pollMs: number;
  maxArchiveBytes: number;
  maxExtractedBytes: number;
  maxGenerations: number;
}

// OverlayArchive is the overlay client layer named by the pointer ConfigMap:
// the exact sha256 of the archive bytes, the immutable archive ConfigMap that
// must carry them, and Forge's opaque overlay revision (logs/metrics only).
export interface OverlayArchive { revision: string; digest: string; archive: string }

// GenerationSource is one composed generation: the image product layer plus
// the exact overlay client archive, if any. Its digest names the generation.
export interface GenerationSource { revision: string; digest: string; productDigest: string; client: OverlayArchive | null }

export const overlayArchiveKey = "tools.tar.gz";
const archivePrefix = "overlay-tools-";
const digestPattern = /^sha256:([a-f0-9]{64})$/;

export class GenerationMaterializer {
  private core: k8s.CoreV1Api | null = null;
  private lastDigest = "";
  private productDigest = "";

  constructor(private readonly cfg: MaterializerConfig, kubeConfig?: k8s.KubeConfig, private readonly metrics?: MaterializerMetrics) {
    if (!cfg.overlay) return;
    const kc = kubeConfig ?? new k8s.KubeConfig();
    if (!kubeConfig) kc.loadFromCluster();
    this.core = kc.makeApiClient(k8s.CoreV1Api);
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
          if (bytes !== null) {
            this.lastDigest = source.digest;
            this.metrics?.success(source.revision, source.digest, bytes, (Date.now() - observedAt) / 1000);
          }
        }
      } catch (error) {
        this.metrics?.failure((Date.now() - observedAt) / 1000);
        console.error(JSON.stringify({ event: "materialization_failed", message: error instanceof Error ? error.message : String(error) }));
      }
      await sleep(this.cfg.pollMs, signal);
    }
  }

  // currentSource returns null only while a configured overlay has no pointer
  // ConfigMap yet: a configured overlay is never silently skipped.
  async currentSource(): Promise<GenerationSource | null> {
    if (!this.productDigest) this.productDigest = await layerDigest(this.cfg.productRoot);
    const client = this.cfg.overlay ? await this.currentOverlay() : null;
    if (this.cfg.overlay && !client) return null;
    return composeSource(this.productDigest, this.cfg.productVersion, client);
  }

  // currentOverlay reads and validates the pointer ConfigMap. The archive
  // bytes are only read when a new generation must be materialized.
  async currentOverlay(): Promise<OverlayArchive | null> {
    if (!this.core || !this.cfg.overlay) return null;
    const pointer = await this.readConfigMap(this.cfg.overlay.pointerName);
    if (!pointer) return null;
    const ref = `${this.cfg.overlay.namespace}/${this.cfg.overlay.pointerName}`;
    const { digest, archive, revision } = pointer.data ?? {};
    const match = digestPattern.exec(digest ?? "");
    if (!match) throw new Error(`overlay pointer ${ref} has unsupported digest ${JSON.stringify(digest ?? null)}`);
    if (archive !== `${archivePrefix}${match[1]}`) throw new Error(`overlay pointer ${ref} names archive ${JSON.stringify(archive ?? null)}, expected ${archivePrefix}${match[1]}`);
    if (!revision) throw new Error(`overlay pointer ${ref} has no revision`);
    return { revision, digest: digest!, archive };
  }

  // materialize returns the published generation bytes, or null while the
  // overlay archive named by the pointer does not exist yet (not ready).
  async materialize(source: GenerationSource): Promise<number | null> {
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
        if (!(await this.fetchArchive(source.client, archive))) {
          console.info(JSON.stringify({ event: "overlay_archive_not_ready", revision: source.client.revision, archive: source.client.archive }));
          return null;
        }
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

  // fetchArchive reads the immutable archive ConfigMap, bounds it before
  // decoding, verifies its exact sha256 against the pointer digest, and writes
  // it privately for validation and extraction. False means it is absent.
  private async fetchArchive(client: OverlayArchive, destination: string): Promise<boolean> {
    const ref = `${this.cfg.overlay?.namespace}/${client.archive}`;
    const configMap = await this.readConfigMap(client.archive);
    if (!configMap) return false;
    if (configMap.immutable !== true) throw new Error(`overlay archive ${ref} is not immutable`);
    const encoded = configMap.binaryData?.[overlayArchiveKey];
    if (typeof encoded !== "string") throw new Error(`overlay archive ${ref} has no binaryData ${overlayArchiveKey}`);
    if (base64DecodedLength(encoded) > this.cfg.maxArchiveBytes) throw new Error(`overlay archive exceeds ${this.cfg.maxArchiveBytes} bytes`);
    const bytes = Buffer.from(encoded, "base64");
    if (bytes.byteLength > this.cfg.maxArchiveBytes) throw new Error(`overlay archive exceeds ${this.cfg.maxArchiveBytes} bytes`);
    const actual = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
    if (actual !== client.digest) throw new Error(`overlay archive digest mismatch: pointer ${client.digest}, archive ${actual}`);
    await writeFile(destination, bytes, { mode: 0o600 });
    return true;
  }

  private async readConfigMap(name: string): Promise<k8s.V1ConfigMap | null> {
    if (!this.core || !this.cfg.overlay) throw new Error("overlay source is not configured");
    try {
      return await this.core.readNamespacedConfigMap({ name, namespace: this.cfg.overlay.namespace });
    } catch (error) {
      if ((error as { code?: number }).code === 404) return null;
      throw error;
    }
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
// product digest) and an overlay change (new archive digest) each publish
// exactly one new immutable generation.
export function composeSource(productDigest: string, productVersion: string, client: OverlayArchive | null): GenerationSource {
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

// base64DecodedLength bounds an archive before any bytes are decoded.
function base64DecodedLength(encoded: string): number {
  const padding = encoded.endsWith("==") ? 2 : encoded.endsWith("=") ? 1 : 0;
  return Math.floor((encoded.length * 3) / 4) - padding;
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

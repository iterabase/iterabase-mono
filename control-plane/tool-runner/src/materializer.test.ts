import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as tar from "tar";
import { GenerationMaterializer, composeSource, layerDigest, overlayArchiveKey, validateArchive, type MaterializerConfig, type OverlayArchive } from "./materializer.js";

const roots: string[] = [];
afterEach(async () => {
  vi.restoreAllMocks();
  for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true });
});

async function root(): Promise<string> {
  const value = await mkdtemp(join(tmpdir(), "overlay-archive-"));
  roots.push(value);
  return value;
}

async function productLayer(directory: string, version: string): Promise<string> {
  const product = join(directory, `product-${version}`);
  await mkdir(join(product, "diagnostic"), { recursive: true });
  await writeFile(join(product, "diagnostic", "index.mjs"), `export const product=${JSON.stringify(version)};`);
  await writeFile(join(product, "diagnostic", "manifest.json"), JSON.stringify({ version }));
  return product;
}

async function config(directory: string, options: { overlay?: boolean; productVersion?: string; maxArchiveBytes?: number } = {}): Promise<MaterializerConfig> {
  const version = options.productVersion ?? "1.0.0";
  const cfg: MaterializerConfig = {
    productRoot: await productLayer(directory, version), productVersion: version,
    overlay: options.overlay === false ? undefined : { namespace: "iterabase-overlay", pointerName: "overlay-tools" },
    artifactsRoot: join(directory, "artifacts"), controlRoot: join(directory, "control"),
    pollMs: 10, maxArchiveBytes: options.maxArchiveBytes ?? 1024 * 1024, maxExtractedBytes: 1024 * 1024, maxGenerations: 8,
  };
  await mkdir(join(cfg.artifactsRoot, "generations"), { recursive: true });
  await mkdir(cfg.controlRoot, { recursive: true });
  return cfg;
}

// ConfigMap is the subset of a V1ConfigMap the materializer reads.
interface ConfigMap { immutable?: boolean; data?: Record<string, string>; binaryData?: Record<string, string> }

// cluster is a fake iterabase-overlay namespace: ConfigMaps by name, with
// every read recorded so tests can assert the exact names and namespace.
function cluster(objects: Record<string, ConfigMap> = {}): { objects: Record<string, ConfigMap>; reads: { name: string; namespace: string }[]; kubeConfig: never } {
  const reads: { name: string; namespace: string }[] = [];
  const api = {
    readNamespacedConfigMap: async (request: { name: string; namespace: string }) => {
      reads.push(request);
      const object = objects[request.name];
      if (!object) throw Object.assign(new Error("HTTP-Code: 404 Message: Unknown API Status Code!"), { code: 404 });
      return structuredClone(object);
    },
  };
  return { objects, reads, kubeConfig: { makeApiClient: () => api } as never };
}

function materializer(cfg: MaterializerConfig, objects: Record<string, ConfigMap>): GenerationMaterializer {
  return new GenerationMaterializer(cfg, cluster(objects).kubeConfig);
}

async function published(cfg: MaterializerConfig): Promise<{ revision: string; artifactDigest: string; directory: string }> {
  return JSON.parse(await readFile(join(cfg.controlRoot, "current.json"), "utf8"));
}

interface Fixture { archive: string; bytes: Buffer; overlay: OverlayArchive; objects: Record<string, ConfigMap> }

// overlayObjects is exactly what Forge writes: an immutable digest-named
// archive ConfigMap and the overlay-tools pointer naming it.
function overlayObjects(bytes: Buffer, revision: string): { overlay: OverlayArchive; objects: Record<string, ConfigMap> } {
  const hex = createHash("sha256").update(bytes).digest("hex");
  const overlay: OverlayArchive = { revision, digest: `sha256:${hex}`, archive: `overlay-tools-${hex}` };
  return {
    overlay,
    objects: {
      "overlay-tools": { data: { digest: overlay.digest, archive: overlay.archive, revision } },
      [overlay.archive]: { immutable: true, binaryData: { [overlayArchiveKey]: bytes.toString("base64") } },
    },
  };
}

async function archiveFixture(directory: string, version: string, layer: "client" | "product" = "client"): Promise<Fixture> {
  const source = join(directory, `source-${layer}-${version}`);
  const tool = join(source, "tools", layer, "echo");
  await mkdir(tool, { recursive: true });
  await writeFile(join(tool, "index.mjs"), `export const version=${JSON.stringify(version)};`);
  await writeFile(join(tool, "manifest.json"), JSON.stringify({ version }));
  const archive = join(directory, `artifact-${layer}-${version}.tgz`);
  await tar.c({ gzip: true, cwd: source, file: archive }, ["tools"]);
  const bytes = await readFile(archive);
  return { archive, bytes, ...overlayObjects(bytes, `overlay@${version}`) };
}

describe("overlay archive validation", () => {
  it("counts only regular client tool files", async () => {
    const directory = await root();
    await mkdir(join(directory, "tools", "client", "echo"), { recursive: true });
    await writeFile(join(directory, "tools", "client", "echo", "index.mjs"), "1234");
    await writeFile(join(directory, "README.md"), "ignored");
    const archive = join(directory, "artifact.tgz");
    await tar.c({ gzip: true, cwd: directory, file: archive }, ["tools", "README.md"]);
    await expect(validateArchive(archive, 1024)).resolves.toBe(4);
  });

  it("rejects links even outside the selected tool tree", async () => {
    const directory = await root();
    await mkdir(join(directory, "tools", "client"), { recursive: true });
    await writeFile(join(directory, "target"), "secret");
    const { symlink } = await import("node:fs/promises");
    await symlink("target", join(directory, "escape"));
    const archive = join(directory, "artifact.tgz");
    await tar.c({ gzip: true, cwd: directory, file: archive }, ["tools", "escape"]);
    await expect(validateArchive(archive, 1024)).rejects.toThrow("unsupported archive entry");
  });

  it("rejects an overlay product layer: product tools ship with the platform release", async () => {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1", "product");
    await expect(validateArchive(fixture.archive, 1024 * 1024)).rejects.toThrow("overlay tools/product");
  });
});

describe("generation identity", () => {
  it("names a generation by both the product layer and the client artifact", () => {
    const client: OverlayArchive = { revision: "overlay@a", digest: `sha256:${"a".repeat(64)}`, archive: `overlay-tools-${"a".repeat(64)}` };
    const productOnly = composeSource(`sha256:${"1".repeat(64)}`, "1.0.0", null);
    const composed = composeSource(`sha256:${"1".repeat(64)}`, "1.0.0", client);
    const upgraded = composeSource(`sha256:${"2".repeat(64)}`, "1.1.0", client);
    expect(new Set([productOnly.digest, composed.digest, upgraded.digest]).size).toBe(3);
    expect(productOnly.revision).toBe("product@1.0.0");
    expect(composed.revision).toBe("product@1.0.0+client@overlay@a");
    expect(composeSource(`sha256:${"1".repeat(64)}`, "1.0.0", client)).toEqual(composed);
  });

  it("hashes the product layer deterministically and rejects links", async () => {
    const directory = await root();
    const product = await productLayer(directory, "1.0.0");
    const first = await layerDigest(product);
    expect(await layerDigest(product)).toBe(first);
    await writeFile(join(product, "diagnostic", "index.mjs"), "changed");
    expect(await layerDigest(product)).not.toBe(first);
    const { symlink } = await import("node:fs/promises");
    await symlink("index.mjs", join(product, "diagnostic", "alias.mjs"));
    await expect(layerDigest(product)).rejects.toThrow("not a regular file");
  });
});

describe("generation materialization", () => {
  it("publishes a product-only generation without any overlay source or Kubernetes client", async () => {
    const directory = await root();
    const cfg = await config(directory, { overlay: false });
    const fake = cluster();
    const instance = new GenerationMaterializer(cfg, fake.kubeConfig);

    const source = await instance.currentSource();
    expect(source?.client).toBeNull();
    await instance.materialize(source!);

    const current = await published(cfg);
    expect(current.revision).toBe("product@1.0.0");
    expect(current.artifactDigest).toBe(source!.digest);
    expect(await readFile(join(current.directory, "tools", "product", "diagnostic", "index.mjs"), "utf8")).toContain("1.0.0");
    await expect(readdir(join(current.directory, "tools", "client"))).rejects.toThrow();
    expect(fake.reads).toEqual([]);
  });

  it("waits for a missing pointer instead of silently dropping the client layer", async () => {
    const directory = await root();
    const cfg = await config(directory);
    await expect(materializer(cfg, {}).currentSource()).resolves.toBeNull();
  });

  it("treats a pointer whose archive does not exist yet as not ready and keeps the last generation", async () => {
    const directory = await root();
    const first = await archiveFixture(directory, "v1");
    const second = await archiveFixture(directory, "v2");
    const cfg = await config(directory);
    const instance = materializer(cfg, first.objects);
    await instance.materialize((await instance.currentSource())!);
    const before = await readFile(join(cfg.controlRoot, "current.json"), "utf8");

    const pending = materializer(cfg, { "overlay-tools": second.objects["overlay-tools"]! });
    const source = (await pending.currentSource())!;
    await expect(pending.materialize(source)).resolves.toBeNull();
    expect(await readFile(join(cfg.controlRoot, "current.json"), "utf8")).toBe(before);
    expect((await readdir(cfg.artifactsRoot)).filter((name) => name.startsWith(".artifact-") || name.startsWith(".staging-"))).toEqual([]);
  });

  it("composes the image product layer with the exact verified overlay archive", async () => {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1");
    const cfg = await config(directory);
    const fake = cluster(fixture.objects);
    const instance = new GenerationMaterializer(cfg, fake.kubeConfig);

    const source = await instance.currentSource();
    expect(source?.client).toEqual(fixture.overlay);
    await instance.materialize(source!);

    const current = await published(cfg);
    expect(current.revision).toBe(`product@1.0.0+client@${fixture.overlay.revision}`);
    expect(current.artifactDigest).toBe(source!.digest);
    expect(await readFile(join(current.directory, "tools", "client", "echo", "index.mjs"), "utf8")).toContain("v1");
    expect(await readFile(join(current.directory, "tools", "product", "diagnostic", "index.mjs"), "utf8")).toContain("1.0.0");
    expect((await readdir(cfg.artifactsRoot)).filter((name) => name.startsWith(".artifact-") || name.startsWith(".staging-"))).toEqual([]);
    expect(fake.reads).toEqual([
      { name: "overlay-tools", namespace: "iterabase-overlay" },
      { name: fixture.overlay.archive, namespace: "iterabase-overlay" },
    ]);
  });

  it("publishes a composed generation for an overlay archive with no client tools", async () => {
    const directory = await root();
    const empty = join(directory, "empty");
    await mkdir(join(empty, "tools", "client"), { recursive: true });
    const archive = join(directory, "empty.tgz");
    await tar.c({ gzip: true, cwd: empty, file: archive }, ["tools"]);
    const { overlay, objects } = overlayObjects(await readFile(archive), "overlay@empty");
    const cfg = await config(directory);
    const instance = materializer(cfg, objects);

    const source = (await instance.currentSource())!;
    expect(source.client).toEqual(overlay);
    expect(source.digest).not.toBe(composeSource(source.productDigest, "1.0.0", null).digest);
    await instance.materialize(source);
    expect((await published(cfg)).artifactDigest).toBe(source.digest);
  });

  it("publishes one new generation when the platform release changes the product layer", async () => {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1");
    const before = await config(directory);
    const first = await materializer(before, fixture.objects).currentSource();
    await materializer(before, fixture.objects).materialize(first!);

    // An upgraded pod sees the same overlay archive with the new image layer.
    const after = { ...before, productRoot: await productLayer(directory, "1.1.0"), productVersion: "1.1.0" };
    const upgraded = await materializer(after, fixture.objects).currentSource();
    expect(upgraded!.digest).not.toBe(first!.digest);
    await materializer(after, fixture.objects).materialize(upgraded!);

    const current = await published(after);
    expect(current.artifactDigest).toBe(upgraded!.digest);
    expect(await readFile(join(current.directory, "tools", "product", "diagnostic", "index.mjs"), "utf8")).toContain("1.1.0");
    expect(await readFile(join(current.directory, "tools", "client", "echo", "index.mjs"), "utf8")).toContain("v1");
  });

  it("rejects an overlay carrying tools/product and keeps the last generation serving", async () => {
    const directory = await root();
    const good = await archiveFixture(directory, "v1");
    const bad = await archiveFixture(directory, "v2", "product");
    const cfg = await config(directory);
    const instance = materializer(cfg, good.objects);
    await instance.materialize((await instance.currentSource())!);
    const before = await readFile(join(cfg.controlRoot, "current.json"), "utf8");

    const rejected = materializer(cfg, bad.objects);
    await expect(rejected.materialize((await rejected.currentSource())!)).rejects.toThrow("overlay tools/product is not allowed");
    expect(await readFile(join(cfg.controlRoot, "current.json"), "utf8")).toBe(before);
  });

  it("preserves the last published generation when the next archive fails digest verification", async () => {
    const directory = await root();
    const first = await archiveFixture(directory, "v1");
    const second = await archiveFixture(directory, "v2");
    const cfg = await config(directory);
    const instance = materializer(cfg, first.objects);
    await instance.materialize((await instance.currentSource())!);
    const before = await readFile(join(cfg.controlRoot, "current.json"), "utf8");

    // The pointer names v2's digest and archive, but the archive holds v1 bytes.
    const tampered = { ...second.objects, [second.overlay.archive]: first.objects[first.overlay.archive]! };
    const next = materializer(cfg, tampered);
    const source = (await next.currentSource())!;
    await expect(next.materialize(source)).rejects.toThrow("overlay archive digest mismatch");

    expect(await readFile(join(cfg.controlRoot, "current.json"), "utf8")).toBe(before);
    const current = JSON.parse(before) as { directory: string };
    expect(await readFile(join(current.directory, "tools", "client", "echo", "index.mjs"), "utf8")).toContain("v1");
    await expect(readFile(join(cfg.artifactsRoot, "generations", source.digest.slice(7), "tools", "client", "echo", "index.mjs"))).rejects.toThrow();
  });
});

describe("overlay pointer and archive verification", () => {
  async function rejects(mutate: (fixture: Fixture) => Record<string, ConfigMap>, message: string, stage: "pointer" | "archive", options: { maxArchiveBytes?: number } = {}): Promise<void> {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1");
    const cfg = await config(directory, options);
    const instance = materializer(cfg, mutate(fixture));
    if (stage === "pointer") {
      await expect(instance.currentSource()).rejects.toThrow(message);
      return;
    }
    await expect(instance.materialize((await instance.currentSource())!)).rejects.toThrow(message);
    await expect(readFile(join(cfg.controlRoot, "current.json"))).rejects.toThrow();
    expect(await readdir(join(cfg.artifactsRoot, "generations"))).toEqual([]);
    expect((await readdir(cfg.artifactsRoot)).filter((name) => name.startsWith(".artifact-") || name.startsWith(".staging-"))).toEqual([]);
  }
  const pointer = (fixture: Fixture, data: Record<string, string>) => ({ ...fixture.objects, "overlay-tools": { data } });

  it("rejects a malformed pointer digest", async () => {
    await rejects((f) => pointer(f, { digest: f.overlay.digest.toUpperCase(), archive: f.overlay.archive, revision: "r" }), "unsupported digest", "pointer");
    await rejects((f) => pointer(f, { digest: f.overlay.digest.slice("sha256:".length), archive: f.overlay.archive, revision: "r" }), "unsupported digest", "pointer");
    await rejects((f) => pointer(f, { archive: f.overlay.archive, revision: "r" }), "unsupported digest", "pointer");
  });

  it("rejects a pointer naming an archive other than the digest-named ConfigMap", async () => {
    await rejects((f) => pointer(f, { digest: f.overlay.digest, archive: `overlay-tools-${"0".repeat(64)}`, revision: "r" }), "names archive", "pointer");
    await rejects((f) => pointer(f, { digest: f.overlay.digest, archive: "kube-root-ca.crt", revision: "r" }), "names archive", "pointer");
  });

  it("rejects a pointer without a revision", async () => {
    await rejects((f) => pointer(f, { digest: f.overlay.digest, archive: f.overlay.archive }), "has no revision", "pointer");
  });

  it("rejects a mutable archive ConfigMap", async () => {
    await rejects((f) => ({ ...f.objects, [f.overlay.archive]: { ...f.objects[f.overlay.archive], immutable: false } }), "is not immutable", "archive");
    await rejects((f) => ({ ...f.objects, [f.overlay.archive]: { binaryData: f.objects[f.overlay.archive]!.binaryData } }), "is not immutable", "archive");
  });

  it("rejects an archive ConfigMap without the tools.tar.gz key", async () => {
    await rejects((f) => ({ ...f.objects, [f.overlay.archive]: { immutable: true, binaryData: { "other.tgz": "AA==" } } }), "has no binaryData tools.tar.gz", "archive");
  });

  it("rejects an oversized archive before decoding it", async () => {
    const decode = vi.spyOn(Buffer, "from");
    await rejects((f) => f.objects, "overlay archive exceeds 16 bytes", "archive", { maxArchiveBytes: 16 });
    expect(decode.mock.calls.filter(([, encoding]) => encoding === "base64")).toEqual([]);
  });

  it("rejects path-traversal archive entries", async () => {
    const directory = await root();
    const source = join(directory, "traversal");
    await mkdir(join(source, "tools", "client"), { recursive: true });
    await writeFile(join(directory, "escape.mjs"), "export const escaped = true;");
    const archive = join(directory, "traversal.tgz");
    // tar strips ".." by default; preservePaths writes the hostile entry name.
    await tar.c({ gzip: true, cwd: source, file: archive, preservePaths: true }, ["tools", "tools/client/../../../escape.mjs"]);
    const { objects } = overlayObjects(await readFile(archive), "overlay@hostile");
    const cfg = await config(directory);
    const instance = materializer(cfg, objects);
    await expect(instance.materialize((await instance.currentSource())!)).rejects.toThrow("unsafe archive path");
    await expect(readFile(join(cfg.controlRoot, "current.json"))).rejects.toThrow();
    await expect(readFile(join(directory, "escape.mjs"), "utf8")).resolves.toBe("export const escaped = true;");
    expect(await readdir(join(cfg.artifactsRoot, "generations"))).toEqual([]);
  });
});

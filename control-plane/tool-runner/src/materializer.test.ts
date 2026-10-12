import { createHash } from "node:crypto";
import { mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import * as tar from "tar";
import { GenerationMaterializer, composeSource, layerDigest, validateArchive, type FluxArtifact, type MaterializerConfig } from "./materializer.js";

const roots: string[] = [];
const originalFetch = globalThis.fetch;
afterEach(async () => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
  for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true });
});

async function root(): Promise<string> {
  const value = await mkdtemp(join(tmpdir(), "flux-artifact-"));
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

async function config(directory: string, options: { flux?: boolean; productVersion?: string } = {}): Promise<MaterializerConfig> {
  const version = options.productVersion ?? "1.0.0";
  const cfg: MaterializerConfig = {
    productRoot: await productLayer(directory, version), productVersion: version,
    flux: options.flux === false ? undefined : { namespace: "flux-system", sourceName: "overlay" },
    artifactsRoot: join(directory, "artifacts"), controlRoot: join(directory, "control"),
    pollMs: 10, maxArchiveBytes: 1024 * 1024, maxExtractedBytes: 1024 * 1024, maxGenerations: 8,
  };
  await mkdir(join(cfg.artifactsRoot, "generations"), { recursive: true });
  await mkdir(cfg.controlRoot, { recursive: true });
  return cfg;
}

function materializer(cfg: MaterializerConfig, artifact: FluxArtifact | null): GenerationMaterializer {
  const kubeConfig = {
    makeApiClient: () => ({ getNamespacedCustomObject: async () => ({ status: artifact ? { artifact } : {} }) }),
  };
  return new GenerationMaterializer(cfg, kubeConfig as never);
}

async function published(cfg: MaterializerConfig): Promise<{ revision: string; artifactDigest: string; directory: string }> {
  return JSON.parse(await readFile(join(cfg.controlRoot, "current.json"), "utf8"));
}

async function archiveFixture(directory: string, version: string, layer: "client" | "product" = "client"): Promise<{ archive: string; bytes: Buffer; artifact: FluxArtifact }> {
  const source = join(directory, `source-${layer}-${version}`);
  const tool = join(source, "tools", layer, "echo");
  await mkdir(tool, { recursive: true });
  await writeFile(join(tool, "index.mjs"), `export const version=${JSON.stringify(version)};`);
  await writeFile(join(tool, "manifest.json"), JSON.stringify({ version }));
  const archive = join(directory, `artifact-${layer}-${version}.tgz`);
  await tar.c({ gzip: true, cwd: source, file: archive }, ["tools"]);
  const bytes = await readFile(archive);
  const digest = `sha256:${createHash("sha256").update(bytes).digest("hex")}`;
  return {
    archive, bytes,
    artifact: { revision: `master@sha1:${version}`, digest, url: `http://source-controller.flux-system.svc./gitrepository/overlay/${digest}.tar.gz` },
  };
}

function serve(bytes: Buffer): void {
  globalThis.fetch = vi.fn(async () => new Response(new Uint8Array(bytes), {
    status: 200,
    headers: { "content-length": String(bytes.byteLength) },
  })) as typeof fetch;
}

describe("Flux archive validation", () => {
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
    const client: FluxArtifact = { revision: "master@sha1:a", digest: `sha256:${"a".repeat(64)}`, url: "http://source-controller.flux-system.svc./a.tgz" };
    const productOnly = composeSource(`sha256:${"1".repeat(64)}`, "1.0.0", null);
    const composed = composeSource(`sha256:${"1".repeat(64)}`, "1.0.0", client);
    const upgraded = composeSource(`sha256:${"2".repeat(64)}`, "1.1.0", client);
    expect(new Set([productOnly.digest, composed.digest, upgraded.digest]).size).toBe(3);
    expect(productOnly.revision).toBe("product@1.0.0");
    expect(composed.revision).toBe("product@1.0.0+client@master@sha1:a");
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
  it("publishes a product-only generation without any Flux source", async () => {
    const directory = await root();
    const cfg = await config(directory, { flux: false });
    const instance = new GenerationMaterializer(cfg);
    globalThis.fetch = vi.fn() as typeof fetch;

    const source = await instance.currentSource();
    expect(source?.client).toBeNull();
    await instance.materialize(source!);

    const current = await published(cfg);
    expect(current.revision).toBe("product@1.0.0");
    expect(current.artifactDigest).toBe(source!.digest);
    expect(await readFile(join(current.directory, "tools", "product", "diagnostic", "index.mjs"), "utf8")).toContain("1.0.0");
    await expect(readdir(join(current.directory, "tools", "client"))).rejects.toThrow();
    expect(globalThis.fetch).not.toHaveBeenCalled();
  });

  it("waits for a configured Flux source instead of silently dropping the client layer", async () => {
    const directory = await root();
    const cfg = await config(directory);
    await expect(materializer(cfg, null).currentSource()).resolves.toBeNull();
  });

  it("composes the image product layer with the exact verified Flux client artifact", async () => {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1");
    const cfg = await config(directory);
    const instance = materializer(cfg, fixture.artifact);
    serve(fixture.bytes);

    const source = await instance.currentSource();
    expect(source?.client).toEqual(fixture.artifact);
    await instance.materialize(source!);

    const current = await published(cfg);
    expect(current.revision).toBe(`product@1.0.0+client@${fixture.artifact.revision}`);
    expect(current.artifactDigest).toBe(source!.digest);
    expect(await readFile(join(current.directory, "tools", "client", "echo", "index.mjs"), "utf8")).toContain("v1");
    expect(await readFile(join(current.directory, "tools", "product", "diagnostic", "index.mjs"), "utf8")).toContain("1.0.0");
    expect((await readdir(cfg.artifactsRoot)).filter((name) => name.startsWith(".artifact-") || name.startsWith(".staging-"))).toEqual([]);
    expect(globalThis.fetch).toHaveBeenCalledWith(fixture.artifact.url, { redirect: "error" });
  });

  it("publishes one new generation when the platform release changes the product layer", async () => {
    const directory = await root();
    const fixture = await archiveFixture(directory, "v1");
    const before = await config(directory);
    serve(fixture.bytes);
    const first = await materializer(before, fixture.artifact).currentSource();
    await materializer(before, fixture.artifact).materialize(first!);

    // An upgraded pod sees the same overlay artifact with the new image layer.
    const after = { ...before, productRoot: await productLayer(directory, "1.1.0"), productVersion: "1.1.0" };
    serve(fixture.bytes);
    const upgraded = await materializer(after, fixture.artifact).currentSource();
    expect(upgraded!.digest).not.toBe(first!.digest);
    await materializer(after, fixture.artifact).materialize(upgraded!);

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
    serve(good.bytes);
    const instance = materializer(cfg, good.artifact);
    await instance.materialize((await instance.currentSource())!);
    const before = await readFile(join(cfg.controlRoot, "current.json"), "utf8");

    serve(bad.bytes);
    const rejected = materializer(cfg, bad.artifact);
    await expect(rejected.materialize((await rejected.currentSource())!)).rejects.toThrow("overlay tools/product is not allowed");
    expect(await readFile(join(cfg.controlRoot, "current.json"), "utf8")).toBe(before);
  });

  it("preserves the last published generation when the next artifact fails digest verification", async () => {
    const directory = await root();
    const first = await archiveFixture(directory, "v1");
    const second = await archiveFixture(directory, "v2");
    const cfg = await config(directory);
    const instance = materializer(cfg, first.artifact);
    serve(first.bytes);
    await instance.materialize((await instance.currentSource())!);
    const before = await readFile(join(cfg.controlRoot, "current.json"), "utf8");

    serve(first.bytes);
    const next = materializer(cfg, second.artifact);
    const source = (await next.currentSource())!;
    await expect(next.materialize(source)).rejects.toThrow("Flux artifact digest mismatch");

    expect(await readFile(join(cfg.controlRoot, "current.json"), "utf8")).toBe(before);
    const current = JSON.parse(before) as { directory: string };
    expect(await readFile(join(current.directory, "tools", "client", "echo", "index.mjs"), "utf8")).toContain("v1");
    await expect(readFile(join(cfg.artifactsRoot, "generations", source.digest.slice(7), "tools", "client", "echo", "index.mjs"))).rejects.toThrow();
  });
});

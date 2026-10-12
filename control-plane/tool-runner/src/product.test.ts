import { cp, mkdtemp, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, describe, expect, it } from "vitest";
import { toolDigest } from "./canonical.js";
import { loadLayers } from "./manifest.js";

// The release-versioned product layer shipped in the tool-runner image
// (DES-HOR-651-01). Any invalid product tool fails this suite, and therefore CI,
// before it can reach an install.
const productRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "tools", "product");
const metadata = { revision: "test", artifactDigest: "test" };

const roots: string[] = [];
afterEach(async () => { for (const root of roots.splice(0)) await rm(root, { recursive: true, force: true }); });

describe("release-versioned product layer", () => {
  it("validates every product tool: manifest, digest, effect class, EN/PT templates and bundle identity", async () => {
    const directories = (await readdir(productRoot, { withFileTypes: true })).filter((entry) => entry.isDirectory());
    const generation = await loadLayers(productRoot, null, productRoot, metadata);
    expect(generation.tools).toHaveLength(directories.length);
    for (const tool of generation.tools) {
      expect(tool.layer).toBe("product");
      // Product tools are platform-owned and must stay inside the default
      // approved runner namespace so a fresh install can serve them.
      expect(tool.manifest.name.split(".")[0]).toBe("platform");
    }
  });

  it("ships the reference diagnostic tool and runs it without external effects", async () => {
    const generation = await loadLayers(productRoot, null, productRoot, metadata);
    const diagnostic = generation.tools.find((tool) => tool.manifest.name === "platform.diagnostic");
    expect(diagnostic?.manifest.effectClass).toBe("read_only");
    await expect(diagnostic!.module.invoke({} as never, { message: "hello" })).resolves.toEqual({
      result: { status: "ok", layer: "product", tool: "platform.diagnostic", version: diagnostic!.manifest.version, message: "hello" },
    });
  });

  it("lets a client tool with the same name override the product tool with a new version", async () => {
    const root = await mkdtemp(join(tmpdir(), "product-override-"));
    roots.push(root);
    const product = JSON.parse(await readFile(join(productRoot, "diagnostic", "manifest.json"), "utf8")) as Record<string, unknown>;
    const client = join(root, "tools", "client", "diagnostic");
    await mkdir(client, { recursive: true });
    const bundle = Buffer.from(`export const identity={name:"platform.diagnostic",version:"customer-1"};export async function invoke(){return {result:"client"}};`);
    const projection = { ...product, version: "customer-1" };
    delete projection.digest;
    await writeFile(join(client, "index.mjs"), bundle);
    await writeFile(join(client, "manifest.json"), JSON.stringify({ ...projection, digest: toolDigest(projection, bundle) }));

    const generation = await loadLayers(productRoot, join(root, "tools", "client"), root, metadata);
    const diagnostic = generation.tools.filter((tool) => tool.manifest.name === "platform.diagnostic");
    expect(diagnostic).toHaveLength(1);
    expect(diagnostic[0].layer).toBe("client");
    expect(diagnostic[0].manifest.version).toBe("customer-1");
  });

  it("fails validation when a product tool is broken", async () => {
    const root = await mkdtemp(join(tmpdir(), "product-broken-"));
    roots.push(root);
    await cp(productRoot, join(root, "product"), { recursive: true });
    await writeFile(join(root, "product", "diagnostic", "index.mjs"), "export const identity={name:'platform.diagnostic',version:'1.0.0'};export async function invoke(){return {result:'tampered'}};");
    await expect(loadLayers(join(root, "product"), null, root, metadata)).rejects.toThrow("digest mismatch");
  });

  it("fails validation when a correctly digested product write tool has a consequence template the gateway rejects", async () => {
    const root = await mkdtemp(join(tmpdir(), "product-template-"));
    roots.push(root);
    await cp(productRoot, join(root, "product"), { recursive: true });
    const dir = join(root, "product", "diagnostic");
    const projection = JSON.parse(await readFile(join(dir, "manifest.json"), "utf8")) as Record<string, unknown>;
    delete projection.digest;
    projection.effectClass = "non_idempotent_write";
    projection.consequenceSummaryTemplate = { localizedTemplates: { en: "Send to {{recipient}}", pt: "Enviar para {{recipient}}" }, argumentPaths: {} };
    await writeFile(join(dir, "manifest.json"), JSON.stringify({ ...projection, digest: toolDigest(projection, await readFile(join(dir, "index.mjs"))) }));
    await expect(loadLayers(join(root, "product"), null, root, metadata)).rejects.toThrow("requires an RFC 6901 argument path");
  });
});

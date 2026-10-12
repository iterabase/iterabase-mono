# Tool layers

The trusted tool runner serves two layers of gateway tools. Each tool is a
directory holding a `manifest.json` and a self-contained `index.mjs`; the
manifest `digest` binds the canonical manifest and the exact bundle bytes.

| Layer | Source | Released by | Holds |
| --- | --- | --- | --- |
| `product` | `control-plane/tool-runner/tools/product/` in this repository | Every control-plane release, inside the `control-plane-tool-runner` image at `/app/tools/product` | Integration tools every customer uses: mailbox read/send, browser operations, customer-network file and SQL reads through runners, later Teams |
| `client` | `tools/client/` in the customer's deployment overlay | The overlay's Flux `GitRepository` artifact | Customer-specific tools only, for example a customer's rate API |

Workflows, Skills, credential bindings and agent identities also live in the
overlay. They are not tools and are not part of either layer.

## Rules

- **Product tools are platform code.** They are reviewed, tested and released
  with the control-plane (DES-HOR-651-01). Never copy them into an overlay, and
  never load them from a mutable Git branch. The materializer rejects any
  overlay artifact that contains `tools/product`.
- **Client overrides product by logical name.** A client tool with the same
  name shadows the product tool, but it must use a new immutable version. Reusing
  the product version with different bytes rejects the whole generation.
- **Namespaces are gated.** The runner may publish only names whose first
  segment appears in `toolRunner.allowedToolNamespaces`. Product tools use the
  default `platform` namespace, so a fresh install can serve them with no
  overlay.
- **Versions are immutable.** The gateway keeps every published
  `name@version → digest` permanently. Change a product tool's bytes or manifest
  only together with a new `version`. CI enforces this against every version
  ever published on `master` (`scripts/check-product-versions.mjs`).

## Generations and upgrades

The materializer composes one generation from the image product layer and the
overlay client layer. The generation digest covers both, so a platform
upgrade and an overlay change each publish exactly one new immutable
generation. An upgrade swaps the product layer atomically: every product tool
comes from the same release image.

An upgrade replaces the runner pod, and the new image carries only the new
release's product versions. An attempt pinned to a product version that the
upgrade superseded keeps its exact pin. It is never substituted: the gateway
returns its retryable `tool_unavailable` (ARCH-007, DES-HOR-651-02). Before an
upgrade that bumps a product tool's version, drain the in-flight work that uses
that tool.

Without an overlay (`toolRunner.flux.enabled=false`) the runner serves a
product-only generation, and the chart renders no Flux credentials.

## Authoring a product tool

1. Create `tools/product/<short-name>/index.mjs`. Export
   `identity = { name: "platform.<name>", version }` and
   `async invoke(context, args)`. Import only explicit `node:` built-ins;
   bundles must be self-contained.
2. Write `manifest.json` (`apiVersion: iterabase.io/tool/v1`). Any tool whose
   effect class is not `read_only` must carry EN and PT
   `consequenceSummaryTemplate` texts. An `idempotent_write` tool also needs an
   `idempotencyProof`.
3. Compute the digest from `control-plane/tool-runner`:
   `npm run build && node dist/main.js digest tools/product/<short-name>`, and
   set it as `digest` in the manifest.
4. Run `make tool-runner-test` from `control-plane/`. It validates every product
   tool and the version gate. The image build also runs `validate`, so an
   invalid product tool fails CI before any install.

Overlay authors validate their client layer against a released product layer
with `docker run --rm -v "$PWD:/overlay:ro" <tool-runner-image> validate /overlay`.

`diagnostic/` (`platform.diagnostic`) is the reference product tool. It is a
read-only echo that proves the release path end to end.

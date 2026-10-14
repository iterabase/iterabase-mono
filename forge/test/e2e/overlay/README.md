# Forge E2E fixture overlay

The overlay Forge's real-host E2E scenarios and the CI previews deploy. It
follows the same contract as a customer overlay: a local directory that Forge
renders on the operator machine (`DES-HOR-593-03`). Forge ships only the
rendered result to the installation. Nothing clones, fetches, or reconciles the
overlay inside the cluster, and Forge does not install Flux (`DES-HOR-593-04`).

## Layout

```
overlay/
├── kustomization.yaml       # object instances: composes crds/client
├── values.yaml              # base Helm values (minimal E2E recipe)
├── values.client.yaml       # client Helm overrides (empty in the fixture)
├── crds/
│   ├── base/                # product base instances (empty)
│   └── client/              # client instances: ../base + overlay-probe.yaml
└── tools/
    ├── client/
    │   └── validation-echo/ # one directory per customer tool
    ├── manifest.schema.json
    └── README.md
```

## forge.yaml

```yaml
spec:
  overlay:
    path: ./overlay                              # relative to forge.yaml, or absolute
    valueFiles: [values.yaml, values.client.yaml]
    toolDirs: [tools/client]
```

E2E runs copy this directory and add `values.e2e.yaml` (the run's exact image
identities) as the last value file. `.github/scripts/preview.py` adds
`values.preview.yaml` the same way.

## What `forge apply` does with it

1. **Values.** Stages each value file on the host, in order, as
   `/var/lib/forge/overlay/<install>/NN-overlay.yaml`, followed by Forge's own
   `NN-forge.yaml` files, and passes them all to Helm (later file wins).
2. **Tools.** Packs every tool directory under `toolDirs` into a deterministic
   gzip tar with entries `tools/client/<tool>/...` (`DES-HOR-593-05`). Before
   the platform chart it writes the immutable ConfigMap
   `overlay-tools-<sha256 hex>` (binaryData `tools.tar.gz`) and repoints the
   ConfigMap `overlay-tools` (`digest`, `archive`, `revision`) in namespace
   `iterabase-overlay`. The tool runner's materializer reads the archive that
   pointer names. After the chart, superseded archives are pruned.
3. **Objects.** Builds `kustomization.yaml` in-process, sets
   `iterabase.com/owner=overlay`, `app.kubernetes.io/managed-by=forge`, and
   `iterabase.com/overlay-part=objects` on every object, and applies them after
   the platform chart. Apply refuses to overwrite a live object that lacks the
   overlay owner label. Overlay-owned objects the build no longer renders are
   pruned.
4. **Secrets.** An optional `secrets.yaml` declares Secrets whose values come
   from operator environment variables. Secret manifests in the kustomization
   are rejected.

Re-running `forge apply` is how a change reaches the installation. Removing
`overlay` from forge.yaml prunes the overlay-owned objects and tool ConfigMaps
on the next apply.

## Fixture contents

- `crds/client/overlay-probe.yaml` holds inert objects (namespace
  `forge-e2e-overlay` and ConfigMap `overlay-probe`). They prove owner
  labelling, pruning, and the ownership refusal without touching the platform
  release.
- `tools/client/validation-echo` is a reviewed, read-only canary. No AgentPool
  grants it. It proves that the tool runner loads the client tool from the
  delivered archive.

Product tools ship in the tool-runner image. An overlay carries only
`tools/client`.

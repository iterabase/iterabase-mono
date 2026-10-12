# Forge E2E tool bundles

This fixture contains a reviewed, read-only canary consumed by Forge's exact
Flux artifact E2E. It is intentionally unavailable to agents because the
fixture defines no AgentPool grant.

Like every overlay, it carries only `tools/client`. Product tools ship in the
tool-runner image with each platform release, and the materializer rejects an
overlay `tools/product` (HOR-651, `control-plane/tool-runner/tools/README.md`).

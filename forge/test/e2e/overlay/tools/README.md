# Forge E2E tool bundles

This fixture contains a reviewed, read-only canary. Forge packs `tools/client`
into the overlay tools archive ConfigMaps (`DES-HOR-593-05`), and the E2E
checks that the tool runner materializes and activates it. No AgentPool grants
it, so agents cannot use it.

Like every overlay, this fixture carries only `tools/client`. Product tools
ship in the tool-runner image with each platform release, and the materializer
rejects an overlay `tools/product` (HOR-651,
`control-plane/tool-runner/tools/README.md`).

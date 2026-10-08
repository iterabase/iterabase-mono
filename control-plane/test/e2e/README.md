# Control-plane deployed E2E

This is the control-plane owner's compiled `TestE2E` suite. It uses `testkit/e2e` mechanics while keeping product assertions here.

Each F2 scenario creates and deletes its own fresh Kind cluster. The reusable `deployedState` fixture installs the reviewed certificate-substrate and platform charts with verified control-plane API TLS, real PostgreSQL and MinIO, and the product services selected by each scenario. HOR-477 adds the source-built execution composition: AgentPool workers, durable dispatch, inference gateway, deterministic OpenAI-compatible backend, Flux-backed immutable tools, tool gateway/runner, artifacts, and human/consequence gates.

## Scenarios

- `deployed-control-plane` (the suite's CI smoke): one install, then four independent journeys that each depend only on readiness:
  - identity: the IdentityMapping delegated-token path, JWKS across API restart, and revocation when the mapping is deleted. API key scopes are proven by the F1 server `TestAPI`;
  - work: concurrent idempotent starts, list/detail/filter/timeline, blockers, feedback/revisions, immutable attempts, customer-safe projections, and ordered SSE reconnect after restart;
  - artifacts: upload/publication, work linking, download, MinIO/API restart persistence, admin deletion, and durable tombstones;
  - browser: locked Chromium/Playwright customer journeys over a stable Go-owned proxy to the verified deployed API, covering in-memory authentication, EN/PT portfolio/search/detail, blocker feedback/uploads/downloads, loading/error/SSE reconnect, customer-safe rendering, the automated accessibility baseline, keyboard use, and critical responsive layout.
- `deployed-execution-contracts`: exact source-built image composition, late-Secret AgentPool recovery with real discovery/invocation, worker SPIFFE/mTLS, in-flight cancellation and generation fencing on worker replacement, durable assignment and inference, immutable Flux tool registration and invocation attribution, concurrent duplicate idempotency, non-idempotent `outcome_unknown` without silent retry across runner recovery, artifact lineage, disposable-child/session isolation, human-gate resume, and exact consequential repetition confirmation.

The identity and execution scenarios are the green product-owner replacements for Forge's former `kind-controlplane-identity`, `kind-inference-contract`, and `kind-tool-runner-contract` scenarios. HOR-481 removed those direct-chart Kind scenarios after the replacement gates passed; Forge retains only real-host CPU/GPU substrate authority and explicitly non-authoritative dependent serving smoke.

## Commands

```bash
make -C control-plane test-e2e-unit
make -C control-plane test-e2e-deployed
make -C control-plane test-e2e-execution
make -C control-plane test-e2e
```

Scenarios run in the `source` fixture mode. CI builds each image once from the exact source SHA, pushes it to the preview registry, and exports `ITERABASE_E2E_FIXTURE_MODE=source`, `ITERABASE_E2E_SOURCE_SHA`, `ITERABASE_PLATFORM_LOCAL_CHART` (the platform chart directory with dependencies built; its `cert-manager-substrate` and `lvm-storage-substrate` siblings are installed from the same directory), and, per required image prefix (`CONTROL_PLANE`, `HARNESS`, `TOOL_RUNNER`, `INFERENCE_GATEWAY`, `FORGE_E2E_RUNTIME`), `<P>_IMAGE_REPO`, `<P>_IMAGE_TAG`, `<P>_IMAGE_DIGEST`, `<P>_IMAGE_CONFIG_DIGEST`, `<P>_IMAGE_SOURCE_SHA`, and the `FORGE_E2E_*_IMAGE_ARCHIVE` `docker save` archive of `repo:tag`. `CONTROL_PLANE` is always required.

Owner stages never build artifacts. Every scenario creates Kind, runs `import-runtime-images` to restore each supplied archive, verify its config digest and `org.opencontainers.image.revision` source label, and transport its exact reference into the new cluster, then executes the installs/assertions. Deployed identity checks use the imported single-platform manifest digest returned by Kind/CRI; they do not confuse it with the distinct config or registry digest. A runner-daemon load before cluster creation cannot satisfy this stage. With `ITERABASE_E2E_REQUIRED=true`, any skipped, blocked, or not-run stage fails the scenario.

Set `ITERABASE_E2E_DIAGNOSTICS` to retain failure evidence. Shared collection includes Kubernetes resources/events, pod descriptions/current/previous logs, Helm state, migration and object-store health, a customer-safe request ledger, and browser JSON/network evidence. Failed browser tests add synthetic screenshots and Playwright traces only after an owner sanitizer removes the ephemeral work key and Go independently rejects any retained credential literal. Bootstrap/work credentials are registered with the shared redactor before diagnostics can retain logs; request evidence excludes authorization headers and private request bodies.

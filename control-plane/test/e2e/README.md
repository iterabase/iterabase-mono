# Control-plane deployed E2E

This is the control-plane owner's compiled `TestE2E` suite. It uses `testkit/e2e` mechanics while keeping product assertions here.

Each F2 scenario creates and deletes its own fresh Kind cluster. The reusable `deployedState` fixture installs the reviewed certificate-substrate and platform charts with verified control-plane API TLS, real PostgreSQL and MinIO, and the product services selected by each scenario. HOR-477 adds the source-built execution composition: AgentPool workers, durable dispatch, inference gateway, deterministic OpenAI-compatible backend, Flux-backed immutable tools, tool gateway/runner, artifacts, and human/consequence gates.

## Scenarios

- `deployed-control-plane` (the suite's CI smoke): one install, then four independent journeys that each depend only on readiness:
  - identity: the IdentityMapping delegated-token path, JWKS across API restart, and revocation when the mapping is deleted. API key scopes are proven by the F1 server `TestAPI`;
  - work: concurrent idempotent starts, list/detail/filter/timeline, blockers, feedback/revisions, immutable attempts, customer-safe projections, and ordered SSE reconnect after restart;
  - artifacts: upload/publication, work linking, download, MinIO/API restart persistence, admin deletion, and durable tombstones;
  - browser: locked Chromium/Playwright customer journeys over a stable Go-owned proxy to the verified deployed API, covering in-memory authentication, EN/PT portfolio/search/detail, blocker feedback/uploads/downloads, loading/error/SSE reconnect, customer-safe rendering, the automated accessibility baseline, keyboard use, and critical responsive layout.
- `deployed-execution-contracts`: exact source/candidate image composition, late-Secret AgentPool recovery with real discovery/invocation, worker SPIFFE/mTLS, in-flight cancellation and generation fencing on worker replacement, durable assignment and inference, immutable Flux tool registration and invocation attribution, concurrent duplicate idempotency, non-idempotent `outcome_unknown` without silent retry across runner recovery, artifact lineage, disposable-child/session isolation, human-gate resume, and exact consequential repetition confirmation.

The identity and execution scenarios are the green product-owner replacements for Forge's former `kind-controlplane-identity`, `kind-inference-contract`, and `kind-tool-runner-contract` scenarios. HOR-481 removed those direct-chart Kind scenarios after the replacement gates passed; Forge retains only real-host CPU/GPU substrate authority and explicitly non-authoritative dependent serving smoke.

## Commands

```bash
make -C control-plane test-e2e-unit
make -C control-plane test-e2e-deployed
make -C control-plane test-e2e-execution
make -C control-plane test-e2e
```

Required source and candidate modes both consume the runtime manifest produced by `.github/scripts/e2e.py compose`. Artifact builders run before owner scenarios and build each affected image/chart only once from the exact source SHA using the reviewed release recipe. The composer resolves selected temporary or candidate images plus only explicit immutable baselines to exact archives/references, composes the exact platform/substrate chart pair, and exports the same `CONTROL_PLANE`, `HARNESS`, `TOOL_RUNNER`, `INFERENCE_GATEWAY`, and deterministic-runtime identities in both modes.

Owner stages never build artifacts or choose a mode-specific DAG. Every scenario creates Kind, runs `import-runtime-images` to restore each composer archive, verify its config digest/source label, and transport its exact reference into the new cluster, then executes the same installs/assertions. Deployed identity checks use the imported single-platform manifest digest returned by Kind/CRI; they do not confuse it with the distinct config or registry/index digest. A runner-daemon load before cluster creation cannot satisfy this stage. Required execution also writes one strict result record containing every declared stage and observed artifact identity; a skip, blocked/not-run stage, missing report, or identity mismatch fails the aggregate. Published mode remains local compatibility-only and is not a PR/candidate route.

Set `ITERABASE_E2E_DIAGNOSTICS` to retain failure evidence. Shared collection includes Kubernetes resources/events, pod descriptions/current/previous logs, Helm state, migration and object-store health, a customer-safe request ledger, and browser JSON/network evidence. Failed browser tests add synthetic screenshots and Playwright traces only after an owner sanitizer removes the ephemeral work key and Go independently rejects any retained credential literal. Bootstrap/work credentials are registered with the shared redactor before diagnostics can retain logs; request evidence excludes authorization headers and private request bodies.

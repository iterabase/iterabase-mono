# Deterministic repository-owned testing strategy

This is the durable policy for Iterabase end-to-end validation. The one-time migration inventory remains in [`AUDIT-2026-08.md`](AUDIT-2026-08.md); current scenario coverage comes only from compiled owner registrations.

## Ownership

Assertions stay with the behavior they authorize:

| Owner | Authoritative behavior |
| --- | --- |
| Control-plane | Identity, API authorization, work/artifacts, AgentPool, dispatch, harness, gateway/tool execution, recovery, and browser journeys. |
| Inference gateway | Routing, snapshot consumption, authentication enforcement, rate limiting, request transforms, and component-local transport behavior. Shared producer/consumer scenarios remain close to the named owner suite. |
| Charts | Rendering plus declarative install, upgrade, feature enablement, reapply, rollback, TLS, Services, persistence, component rollout, and observability client paths. |
| Forge | SSH/k3s bootstrap, reality-as-state reconciliation, source/overlay handoff, host migration, secret transport, and CPU/GPU substrate behavior. |
| OPO1 / production | Only evidence that cannot be represented without the real GPU, DNS/ACME, SMTP, firewall, customer storage/data, or resource envelope. |

A dependent layer may retain a composition smoke check, but it does not become duplicate authority. Product/chart assertions are not moved into `testkit/e2e`; the shared module owns mechanics only.

## Fixture tiers

| Tier | Boundary |
| --- | --- |
| **F0** | Pure/static process, parser, fake, or hermetic mechanics example. |
| **F1** | Local real process, envtest, testcontainer, or native protocol integration. |
| **F2** | Fresh isolated Kind cluster with real Kubernetes, Helm, and network boundaries. |
| **F3** | A fresh AWS EC2 CPU or GPU host per scenario run, booted from a baked fixture AMI, with a pinned host key, and terminated after the run (C1). |
| **P** | Mutable production confirmation that satisfies the strict criteria below. |

Tier is compiled scenario metadata, not an estimate of importance. A higher tier supplements rather than replaces faster owner authority.

## Fixture mode

Every suite execution records the `source` fixture mode: one full source SHA
plus an explicit dirty-worktree bit. Required runs use `ITERABASE_E2E_SOURCE_DIRTY=false`
and the exact commit's build-once artifacts (C3): images pulled by digest from
the preview registry, the source tree's charts, and the commit's Forge binary.
When `release.yml` validates a release, images of targets not being released
come from their published versions (C7). `charts/n-1-upgrade` additionally
receives the newest published platform-chart Release as an explicit,
checksum-pinned baseline.

There is no default inside the library, floating `latest`, matching-branch
lookup, or silent source→published fallback. The fixture record is printed
before scenarios execute.

## Suite entrypoint convention

Repository owners use one nested Go module and one top-level test:

```text
<owner>/test/e2e/go.mod
<owner>/test/e2e/*_test.go
TestE2E
```

`TestE2E` creates one `e2e.Suite`, registers typed scenarios/stages, and calls `Suite.Run`. Scenario metadata includes tier, references, the fixture mode, required artifacts, bounded Make/timeout/capacity data, and the selection fields `smoke`, `selected_by`, and `renders` (see [`../ci.md`](../ci.md#adding-a-scenario)). Scenario and stage names are lowercase kebab-case and unique in their scope.

Current entrypoints are:

- `charts/test/e2e`
- `control-plane/test/e2e`
- `forge/test/e2e`

Each has an F0 hermetic example so registration and shared execution remain testable without infrastructure. Examples are mechanics evidence, not product coverage. The runnable scenarios are:

| Suite | Scenario | Tier | Make target | Proves |
| --- | --- | --- | --- | --- |
| charts | `fresh-install` (smoke) | F2 | `test-e2e-install` | ordered certificate and LVM substrates, storage classes and claims, manager contract, issuer, workload identity, ingress planes |
| charts | `n-1-upgrade` | F2 | `test-e2e-n-1-upgrade` | newest published platform Release → head, reapply, roll back to N-1, forward again, with persisted state |
| charts | `observability` | F2 | `test-e2e-observability` | observability stack, monitor discovery, dashboards' client paths, Prometheus/Loki persistence |
| charts | `observability-tls` | F2 | `test-e2e-observability-tls` | the internal-TLS composition, including the former `internal-tls` assertions |
| control-plane | `deployed-control-plane` (smoke) | F2 | `test-e2e-deployed` | one install, then the identity, work, artifact, and locked-Chromium browser journeys as independent stages |
| control-plane | `deployed-execution-contracts` | F2 | `test-e2e-execution` | AgentPool, dispatch, harness, model and tool gateways, isolation, and durable recovery |
| forge | `cpu` (smoke) | F3 cpu | `test-e2e` | fresh-host bootstrap, GPU refusal, Flux handoff, inotify capacity, idempotent reapply, secrets, Flux reconciliation |
| forge | `cpu-workspace` | F3 cpu | `test-e2e-workspace` | LVM substrate, AgentPool workspaces, capacity gating, reboot/reapply, persisted bytes, non-purging destroy |
| forge | `gpu` | F3 gpu | `test-e2e-gpu` | GPU readiness, model-cache seeding, one real serving completion; optional `driver-upgrade` stage |

Forge registers no Kind scenario: its former Kind scenarios installed charts directly without exercising Forge (HOR-481).

Local commands:

```bash
make testkit-test       # shared race tests, every owner example, JSON/Markdown generation
make testkit-kind-example # explicit real Kind create/kubectl/delete validation
make e2e-catalogue      # JSON to stdout
make e2e-catalogue-check
make -C charts test-e2e-unit
make -C control-plane test-e2e-unit
make -C control-plane test-e2e-deployed  # fresh Kind + the CI-supplied source images
make -C control-plane test-e2e-execution
make -C charts test-e2e-install
make -C forge test-e2e-unit
```

## Compiled catalogue

`testkit/e2e/cmd/e2e-catalogue` reads the committed Go workspace, finds modules ending in `/test/e2e`, compiles each `TestE2E` in catalogue mode, and merges the emitted registrations in stable suite/scenario order. Catalogue mode never resolves a runtime fixture or provisions infrastructure.

JSON and Markdown are two renderings of the same compiled registrations. A golden test covers Markdown generation. No hand-maintained coverage YAML/JSON or release scenario map is allowed.

## Deterministic stage semantics

A scenario owns typed state and ordered stages. Dependencies are explicit and may reference only previously registered stages:

- duplicate names, unknown/forward dependencies, and invalid metadata fail before execution;
- a failed or skipped prerequisite suppresses only its transitive dependents;
- an `Optional` stage runs only when `ITERABASE_E2E_OPTIONAL_STAGES` names it, is recorded `not-selected` otherwise, and cannot be a dependency;
- independent stages continue, preserving useful fault localization;
- diagnostics run after a failure;
- every cleanup hook runs even if a prior cleanup hook fails.

External commands run exactly once with a positive timeout. Condition polling is bounded, observes immediately, and fails immediately on an observation error. Polling readiness is not permission to retry a failed assertion, scenario, install, request, or release gate. Required tests have no automatic retry or pass-on-retry status.

Fresh Kind clusters use a timestamp-plus-random DNS-safe name and a private temporary kubeconfig rather than `~/.kube/config`. Cleanup is idempotent. Stale clusters or local files therefore cannot satisfy or collide with a fresh-run contract.

## Shared mechanics

The testkit provides:

- one-shot bounded process execution with redacted retained output;
- unique Kind create/delete and local image loading;
- kubeconfig-bound `kubectl`, deterministic Helm values, exact chart validation, and loopback-only port forwarding;
- bounded plaintext HTTP and verified-CA/server-name TLS clients (no shared insecure-skip path);
- bounded readiness polling;
- Kubernetes resources/events, per-pod describe/current/previous logs, and per-release Helm state diagnostics;
- component-declared artifact collection;
- a Go seam that runs `npm ci`, invokes the locked Playwright binary with `--retries=0`, and collects declared traces/screenshots/reports.

Playwright/TypeScript owns browser assertions. Go owns fixture/runtime orchestration and process/artifact lifecycle. The control-plane browser owner uses the shared process seam, a Go-owned stable proxy to the verified deployed endpoint, and a Go restart coordinator; Playwright cannot provision or replace the stack.

## Failure evidence and secret handling

Process output and all text evidence pass through a shared redactor before persistence. Owners register exact runtime secret literals; structural rules also redact authorization/bearer values, credential-shaped keys, URL passwords, and private-key PEM blocks. Generic Kubernetes collection deliberately excludes Secret objects, and rendered Helm evidence strips every Secret `data`/`stringData` payload before shared redaction and persistence.

Component artifacts are fail-closed:

- text is copied only after redaction;
- opaque/binary bytes are rejected by default;
- an owner may explicitly declare an artifact **safe synthetic opaque** when its fixture cannot contain credentials or customer data.

That declaration is required for Playwright screenshots/traces and is part of the reviewable scenario code. The control-plane fixture is wholly synthetic; its owner sanitizes trace archive entries before declaration, deletes raw evidence, and independently rejects retained work-key literals before shared collection. Customer/production browser artifacts do not qualify.

A normal F2 failure bundle includes cluster resources, events, pod describes, current/previous logs, Helm list/state, revision history, effective values, hooks, status, process output, and declared component evidence. Forge F3 uses the same collector against its fetched kubeconfig, adds strictly pinned SSH, boot-ID, workspace/model-cache, and GPU-operator evidence, and records whether failure belongs to fixture readiness, Forge substrate/reconciliation/handoff, dependent smoke, or cleanup. Diagnostics are best effort and run before the host is terminated.

## Placement (C5)

Each behaviour is tested once, at the cheapest tier that can prove it:

- The control-plane identity, work, artifact, and browser journeys share one install in `deployed-control-plane`, as independent stages after readiness. API-key scope assertions stay in the F1 server `TestAPI`; the scenario keeps the IdentityMapping path, JWKS across restart, and revocation on mapping deletion.
- `observability-tls` owns the former `internal-tls` assertions (same values and CA chain). The gateway's `verify-full`/`rediss://` client configuration is a static `helm template` check (`charts/scripts/check-gateway-tls-client.sh`). Endpoint separation and Grafana UIDs are render-script checks; the sidecar-provisioning check stays live.
- The manager-contract RBAC checks live in `check-manager-contract.sh`; the live scenario has no fixed wait.
- `forge/cpu-workspace` owns the LVM foundation, two-worker pool, grow, reapply, and delete checks; `forge/cpu` no longer repeats them. Both run on separate parallel hosts.
- `forge/gpu` keeps one real serving completion. The GPU driver upgrade is the optional `driver-upgrade` stage: it runs when driver inputs change and always in full validation.
- The historical F0 lifecycle scenarios are deleted. `charts/n-1-upgrade` is the one version-boundary scenario: it installs the newest published platform-chart Release, upgrades to head, reapplies, rolls back to N-1, and recovers forward. It runs when its required artifacts change and in full validation.

## CI and release gates

[`../ci.md`](../ci.md) is the operational contract. In short:

- Pull requests and merge-queue commits run the jobs and scenarios chosen by the deterministic affected-graph selector (C2) from compiled metadata only. Documentation-only changes run nothing; CI changes run every CI job plus the three suite smokes; version-only changes run the install-readiness smoke; chart changes run the scenarios whose declared renders change; a path with no owner runs everything.
- On pull requests the F3 scenarios are narrowed by `selected_by`; the merge queue and the `e2e-real-machine` label select strictly (`DES-HOR-590-02`).
- Each image is built once per commit; Kind scenarios run in parallel on GitHub-hosted runners and each F3 scenario on its own fresh EC2 host, so nothing queues behind a shared fixture.
- Every selected scenario must pass with `ITERABASE_E2E_REQUIRED=true`: a skipped, blocked, or not-run mandatory stage fails it, and a missing fixture variable, unreachable host, host-key mismatch, or missing baked generation fails rather than skips. `E2E / required` treats a failed, canceled, or unexpectedly skipped job as incomplete.
- Full validation (`full-validation.yml`, C11) runs the whole catalogue, including the driver upgrade and `n-1-upgrade`, nightly when `master` changed, on demand, through the `full-validation` label, and for every release.
- A release validates exactly what ships: released targets use the commit's builds and every other referenced target its published artifact (C7). See [`../release.md`](../release.md).

## Production-only criteria

A check may remain tier P only when a representative isolated fixture cannot establish the claimed behavior without one of:

- the actual GPU/hardware or customer resource envelope;
- public DNS and ACME authorization;
- the real SMTP provider/mailbox;
- production firewall/routing topology;
- customer-owned storage or data-handling constraints.

Cost, test duration, missing automation, historical placement, or an inconvenient fixture do not make behavior production-only. Portable gaps receive an owner scenario/ticket. Production is confirmation rather than first discovery for portable behavior.

## Change control

Failure semantics, fixture resolution, artifact security, ownership boundaries, and scenario selection are architecture contracts. Changes require explicit approval under the root repository rules. Performance/load testing, flake dashboards, coverage-derived selection, mutable test-state caches, and generic public-framework scope remain non-goals.

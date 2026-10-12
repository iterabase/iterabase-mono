# Platform alert runbook

These alerts describe internal operator health. They never assert customer-result correctness. Preserve durable state and ambiguous external effects before restarting or retrying work.

## Common first response

1. Record alert labels, start time, deployed immutable image/chart versions, and recent rollout events.
2. Check `00 — Platform Overview`, then the linked component dashboard.
3. Check pod readiness/restarts, Kubernetes events, Service endpoints, and recent structured logs in Loki.
4. Do not repeat a consequential tool action when its outcome is unknown.
5. Prefer disabling new work or rolling back immutable pins over deleting durable state.

For AgentPool storage, `50 — Data and Storage` is a quick-glance orientation view, not a replacement for the alert-specific evidence below. Its workspace panels show actual filesystem free bytes/ratio, warning state, and fresh-credit gate state. Requested PVC size is planning metadata and must not be interpreted as a hard quota.

## IterabasePlatformTargetDown

Confirm the target pod exists and is Ready, the named metrics port is present, and the ServiceMonitor/PodMonitor selects it. Validate `/metrics` from inside the cluster. Roll back the affected image/chart pin if the listener failed after rollout.

## IterabasePlatformTargetAbsent

Confirm the component is intentionally enabled in the deployed values and listed under `observability.alerts.expectedTargets`. Check that its workload, Pod, Service endpoint, and ServiceMonitor/PodMonitor still exist and that Prometheus discovered the monitor. Disable the expectation only when the component is intentionally removed; do not use it to silence an unexplained disappearance.

## IterabasePostgreSQLUnavailable

Check PostgreSQL pod/volume health, Service endpoints, TLS certificate validity, authentication, and exporter logs. Stop write traffic before destructive recovery. Restore from the accepted backup procedure only with explicit authority.

## IterabaseRedisUnavailable

Check Redis pod, Service, AUTH/TLS Secret, and exporter. Redis is not durable authority; restore connectivity rather than reconstructing product state from Redis.

## IterabaseControlPlaneAPIHighErrorRate

Use `10 — Control Plane` to identify normalized failing routes. Check API logs, readiness probes, Postgres pool saturation, MinIO, and recent migrations. Preserve idempotency keys and artifact metadata during rollback.

## IterabaseArtifactOperationsFailing

Check API/gateway route breakdown, MinIO readiness, bucket credentials, TLS, immutable metadata lifecycle, and object digest failures. Preserve metadata tombstones and never overwrite immutable bytes during recovery.

## IterabaseManagerReconciliationFailures

Identify the controller label and affected CR status. Check manager logs, RBAC denials, Kubernetes events, invalid immutable definitions, and dependency readiness. Do not manually mutate operator-owned children unless the controller is paused and recovery is documented.

## IterabaseGatewayOutcomeUnknown

Treat this as a possible external effect, including one classified during gateway crash recovery. Find the durable invocation by time and operator logs, inspect its persisted consequence summary, and reconcile with the external system. Never automatically retry. Record the reconciliation decision before explicit follow-up action.

## IterabaseToolRunnerDisconnected

Check gateway and runner readiness, mTLS certificate/CA validity, Service DNS, approved runner identity, and stream errors. Existing pinned versions must not be substituted.

## IterabaseToolGenerationUnavailable

Inspect Flux `GitRepository` artifact status, materializer logs, archive bounds, manifest validation, and digest checks. Keep the last valid generation serving when present; never bypass digest or namespace validation.

## IterabaseToolMaterializationFailures

Compare the failing Flux revision/digest with the last successful generation. Validate the artifact archive and all product/client manifests using the runner authoring command before promoting a corrected immutable revision.

## IterabaseDispatchWithoutWorkers

Check AgentPool replicas/status, fixed-class RWO PVC/PV path, worker pod readiness, CSI-issued SPIFFE leaves, NetworkPolicy egress, workspace capacity gate, and dispatch mTLS. Pending durable work must remain queued; do not synthesize assignments.

## IterabaseDispatchWorkerLosses

Inspect worker restarts, node/network health, heartbeat delays, fencing generations, and lease expiry. Confirm first-terminal-writer behavior and that no fenced generation remains authorized.

## IterabaseHarnessDisconnected

Check the individual harness pod, dispatch DNS/port, certificate validity, and reconnect logs. A disconnected supervisor must not continue a child execution; confirm fail-closed termination and retained WAL behavior.

## IterabaseHarnessStorageUnavailable

Stop new AgentPool scheduling and inspect the pool's `StorageReady` reason, fixed `iterabase-agentpool-lvm-xfs` class, RWO/Filesystem PVC, CSI PV driver/XFS/VG/handle/topology, Ready LVMVolume, LVMNode `iterabase-data` identity, available blocks, ownership, and node/disk I/O events. A real write/fsync/mount failure fences the active turn and replaces the worker; never replay a lost turn or external effect automatically. Refuse any alternate/default/root fallback.

## IterabaseWorkspaceCapacityWarning

One AgentPool PVC is below 25% free. Record its `pool`-labelled durable `control_plane_dispatch_workspace_free_bytes`, capacity, ratio, matching `WorkspaceCapacityHealthy` condition, supporting worker observations, largest eligible session data, and active turns. Pause avoidable growth and notify the customer-owned capacity response. PVC size is immutable thick capacity and cannot be expanded online. Do not delete session data without explicit lifecycle authority.

## IterabaseWorkspaceCreditGated

The named pool PVC is at or below 20% free, or its durable gate has not yet reached the 25% reopen threshold. Confirm the matching `control_plane_dispatch_workspace_credit_gated{pool=...}=1`, only that AgentPool reports `WorkspaceCapacityHealthy=False` with reason `WorkspaceCapacityGateActive`, supporting worker observations agree, and none of that pool's unspent fresh credits remain. Another pool PVC stays independently eligible. Worker or dispatch replacement in the 20–25% band must retain this pool's gate. Do not abort an active turn solely for crossing the threshold; let it reach its normal terminal/ACK boundary, then verify no next credit appears. Reopen only after that PVC reaches at least 25%. Expansion/replacement/migration is unsupported; never switch classes or fall back to root.

## IterabaseWorkspaceStorageIOFailure

Treat this as actual claim/filesystem failure rather than a capacity warning. Capture bounded worker-loss, turn fencing, XFS, CSI mount, PVC/PV, LVMVolume, LVMNode, VG, kernel, and node-I/O evidence. Verify the Forge receipt plus exact ownership tag, selected-device/PV/VG UUID, and membership before any host action. Changed hardware, PV/VG identity, class, topology, ownership, or unexpected consumer fails closed. Do not recreate/adopt/extend storage or replay work automatically.

## IterabaseDataVGCapacityWarning

Aggregate `iterabase-data` free capacity is below 25%, independently of any one AgentPool filesystem ratio. Inspect `lvm_vg_free_size_bytes{name="iterabase-data"}`, total bytes, current thick LVs/LVMVolumes, pending PVC requests, missing-PV state, and recently deleted claims. A new thick claim can fail while existing pools remain healthy. Free only explicitly eligible claims/data; do not extend the VG, thin-provision, change classes, or use root storage.

## IterabaseLVMClaimPending

A managed `iterabase-lvm-xfs` or `iterabase-agentpool-lvm-xfs` claim remained Pending for ten minutes. Inspect consumer scheduling (including `WaitForFirstConsumer`), PVC events, OpenEBS controller/node pods, CSI registration, LVMNode VG identity/capacity, and any Failed/Pending LVMVolume. Report capacity exhaustion honestly; never create a default class or test-authored PV/LV repair.

## IterabaseHarnessReplayBacklog

Inspect harness WAL files/logs and dispatch event persistence/ACK logs. Verify cumulative ACK progress. Do not delete WAL state before confirming every durable event is committed or intentionally reconciled.

## IterabaseInferenceGatewayHighErrorRate

Break down by listener/model. Check snapshot freshness, model/backend availability, Redis/Postgres pools, backend latency, and gateway logs. Keep API-key and workload authorization paths distinct during diagnosis.

## IterabaseInferenceSnapshotStale

Check Postgres connectivity, LISTEN/NOTIFY health, fallback refresh logs, and last-refresh time. A stale gateway is intentionally not Ready; fix snapshot consumption rather than bypassing the freshness gate.

## IterabaseModelBackendUnavailable

Inspect ModelBackend/Model status, serving pod startup/readiness, GPU allocation/health, model files, and the inference snapshot. Do not route to an unpinned substitute model.

## IterabasePersistentVolumeAlmostFull

Identify the PVC and growth source. Confirm backups and retention policy before cleanup. Expand storage or publish an approved retention change; never delete Postgres/MinIO/Prometheus/Loki files directly.

## IterabaseContainerOOMKilled

The kernel killed the container at its memory limit. Identify the workload and what it was doing (harness turn, extraction, query, scrape) from logs and the `kube_pod_container_status_restarts_total` history. Compare its working set with its limit and with the single-node budget in [`charts/docs/sizing.md`](../../charts/docs/sizing.md). Raise a limit only within that budget, through a reviewed chart or overlay change; on a full node, reduce concurrency (AgentPool replicas, browser/extraction pools) instead. AgentPool workers and ModelBackends get a memory limit only from their own `spec.resources`; the release-namespace LimitRange defaults requests, never limits.

## IterabaseContainerMemoryNearLimit

The container's working set has stayed above 90% of its memory limit for 15 minutes. Treat it as an imminent OOM kill: find the load or leak driving growth, check the trend on the component dashboard, and decide between reducing load and a reviewed limit change within the [`charts/docs/sizing.md`](../../charts/docs/sizing.md) budget.

## IterabaseControlPlaneDatabasePoolSaturated

Identify the component target, then correlate request/reconciliation load, acquire wait, slow queries, and Postgres connection capacity. Do not raise limits without checking server capacity and query behavior.

## IterabaseInferenceDatabasePoolSaturated

Inspect API/workload concurrency, snapshot refresh behavior, active queries, and Postgres capacity. Preserve the snapshot freshness gate while remediating.

## IterabaseInferenceRedisPoolTimeouts

Inspect Redis availability/latency, blocked clients, gateway concurrency, and pool sizing. Rate-limit checks currently fail open on infrastructure errors; treat sustained timeouts as degraded protection.

## IterabaseCertificateExpiringSoon

This warning fires after a cert-manager Certificate remains past its scheduled renewal time for ten minutes, or when its remaining lifetime stays below one hour for ten minutes as an emergency fallback. A healthy short-lived leaf does not fire merely because its total duration is below a fixed threshold. Inspect the Certificate's `Ready`/`Issuing` conditions, `renewalTime`, CertificateRequest and issuer state, DNS challenge/API connectivity, and renewal events. Validate the renewed chain and rollout/reload path before considering manual replacement; successful renewal advances the exported renewal and expiration timestamps and resolves the alert.

## IterabasePrometheusRuleEvaluationFailures

Open Prometheus rule status and logs, identify the exact group/expression, and compare it with the deployed chart. Repair query/label drift; do not silence the failure by removing unrelated rules.

## IterabaseControlPlaneAPILatencyHigh

This alert exists only when an overlay sets an accepted threshold. Break down P95 by normalized route, then inspect request concurrency, Postgres pools, artifact transfers, and resource saturation.

## IterabaseInferenceLatencyHigh

This alert exists only when an overlay sets an accepted threshold. Inspect model queue depth, TTFT, token throughput, GPU utilization/memory, backend latency, and request/token shape before changing the threshold.

## IterabaseInferenceTTFTHigh

Inspect prompt size, queued/running requests, model cache pressure, GPU saturation, and backend startup/health. Adjust the threshold only through an accepted workload objective.

## IterabaseHarnessTurnLatencyHigh

Inspect model, tool, artifact, sandbox, replay, and child lifecycle latency. The threshold is workflow/deployment-specific and changes require an accepted operational objective.

## IterabaseModelQueueHigh

Correlate waiting and active requests with arrival rate, TTFT, GPU memory/cache, and model capacity. Reduce load or scale/tune only within the model deployment contract.

## IterabaseGPUUtilizationHigh

Correlate sustained utilization with queue depth, throughput, GPU memory, temperature, power, and XID errors. High utilization alone is not failure; this alert exists only when the overlay defines an accepted capacity threshold.

# Single-node sizing

This is the reviewed resource contract and capacity budget for one-node
installs (HOR-602, `REQ-041`). It covers the hosted baseline `m7i-flex.xlarge`
(4 vCPU / 16 GiB) and the scale tier `c7i-flex.2xlarge` (8 vCPU / 16 GiB).
Multi-node sizing, autoscaling and Graviton are out of scope.

## Resource contract

- **Every container sets its own resources.** Each container a shipped chart
  renders declares a CPU request, a memory request and a memory limit no
  lower than that request. This covers init containers, hook Jobs, upstream
  subcharts, and the Prometheus/Alertmanager specs the operator manages.
  `scripts/check-resource-limits.sh` (part of `make check`) renders every
  supported values profile and fails on a missing field. There are two
  exemptions: the `cert-manager-csi-driver` `node-driver-registrar` and
  `liveness-probe` sidecars, which have no upstream resources value. The check
  fails if either exemption stops rendering.
- **Memory limits make failure visible.** A container that exceeds its limit
  is OOM-killed inside its own cgroup, and `IterabaseContainerOOMKilled` fires.
  It cannot starve Postgres, the kubelet or k3s. `IterabaseContainerMemoryNearLimit`
  warns when the working set stays above 90% of the limit for 15 minutes.
  Both alerts are scoped to the release namespace.
- **No CPU limits.** CPU is compressible: throttling hurts latency and protects
  no neighbour. CPU requests reserve schedulable capacity and set the
  scheduler's admission decision.
- **Runtime-created pods get a default request (DES-HOR-602-02, amended).**
  The umbrella renders one `LimitRange`, `<release>-runtime-defaults`, in the
  release namespace. It sets only `defaultRequest` (25m CPU / 128Mi memory):
  no default limit and no `max`/`min`. It applies to AgentPool workers
  (`supervisor` and `init-wal`) and ModelBackend pods that the manager creates
  without resources, so they still count toward scheduling. It deliberately
  sets no memory limit. A ModelBackend always declares a GPU but often no
  memory, and a default limit would OOM-kill its vLLM server. A larger default
  request would also land on every unsized pod of an n-1 chart during a
  rollback, while the LimitRange briefly outlives the rollback, and the
  rollback would no longer fit the node. **Runtime pods have a memory limit only
  when the overlay sets one**: size AgentPools (recommended 250m / 256Mi
  request, 1Gi limit per worker, from the measurements below) and
  ModelBackends through their `spec.resources`.
- **Overcommit fails closed for sized pools.** When a pool with explicit
  requests asks for more workers than the node can admit, the surplus pods
  stay `Pending` (unschedulable) instead of overcommitting memory. Every
  reference run observed this directly. An unsized pool gets only the small
  default request, so size every pool.

## Kubelet reservation

k3s runs outside any pod. On the reference installs `k3s-server` held about
1.1 GiB RSS. Without a reservation, allocatable equals capacity and pod
requests can claim memory the node itself needs. Hosted and self-hosted
installs should reserve it through Forge's existing `k3s.extraArgs`:

```yaml
k3s:
  extraArgs: ["--kubelet-arg=system-reserved=cpu=250m,memory=1536Mi"]
```

The budgets below assume this reservation. The scale-tier run used it.

## Measured footprint (2026-10-12, platform chart from HOR-602)

The test was a fresh install on each instance type: Forge k3s v1.34.10+k3s1,
both substrates, and the platform with `values-tls.yaml` and
`values-observability.yaml`. The workflow was a single `agent_task` node
calling `deepseek-v4-flash` on Iterabase inference. Each worker served one
turn at a time. Memory and CPU were sampled every 15 seconds with
`kubectl top`. Peaks are per container. The worker request in each run label is the
per-container request every worker received (250m CPU in all runs). The runs
applied it through an earlier LimitRange default; installs now set it through
`AgentPool.spec.resources`.

| Run | Workers | Items (in flight) | Result | Node peak CPU / memory | Worker peak | Restarts / OOM |
| --- | --- | --- | --- | --- | --- | --- |
| 4 vCPU, no reservation, 512Mi worker request | 11 (CPU-request bound) | 44 (11) | 44 done | 1.18 cores / 5.3 GiB | 155Mi / 141m | 0 / 0 |
| 8 vCPU, reserved, 512Mi worker request | 17 (memory-request bound) | 51 (17) | 51 done | 2.19 cores / 6.1 GiB | 154Mi / 163m | 0 / 0 |
| 8 vCPU, reserved, 256Mi worker request | 26 (CPU-request bound) | 52 (26) | 52 done | 3.16 cores / 7.5 GiB | 147Mi / 140m | 0 / 0 |

In every run the surplus workers above the bound stayed `Pending`. In the 26-worker run, item latency (70–177 s) tracked queueing at the shared inference endpoint, while the node stayed under 40% CPU.

The rest of the platform (everything except workers) peaked at about 1.9–2.1
GiB and 0.33–0.38 cores combined. The largest consumers were Prometheus (~590Mi),
Postgres (~190Mi), Grafana (~185Mi) and Loki (~110Mi). Requests are set above
these peaks to leave room for data growth, so the budget is request-based. The
scheduler admits pods by requests, not by usage.

## Request budget per instance size

All figures are requests. `toolRunner` (125m / 320Mi) is included even though
the throwaway reference hosts ran without Flux. The AWS Load Balancer
Controller is a placeholder until HOR-593 pins it. Artifact processing is
reserved from the HOR-460 reference profile until HOR-461/HOR-462 measure it.

| Line | 4 vCPU / 16 GiB | 8 vCPU / 16 GiB |
| --- | --- | --- |
| Allocatable after `system-reserved` | 3750m / 13.8 GiB | 7750m / 13.8 GiB |
| kube-system (CoreDNS, metrics-server) | 200m / 0.14 GiB | 200m / 0.14 GiB |
| Platform charts incl. tool runner | 960m / 5.4 GiB | 960m / 5.4 GiB |
| AWS Load Balancer Controller (hosted, placeholder) | 50m / 0.25 GiB | 50m / 0.25 GiB |
| Artifact processing reservation (HOR-460 profile) | 500m / 3.5 GiB | 500m / 3.5 GiB |
| **Left for agent workers and browsers** | **2040m / 4.5 GiB** | **6040m / 4.5 GiB** |

## Stated concurrency

| | 4 vCPU / 16 GiB | 8 vCPU / 16 GiB |
| --- | --- | --- |
| Agent worker replicas (recommended sizing 250m / 256Mi) | 6 | 12 |
| Concurrent browser runs (provisional, 500m / 1 GiB each) | 1 | 1 |
| Concurrent extractions (provisional, within the artifact reservation) | 1 | 1 |
| Workers alone, no browser/artifact workloads (measured) | 11 (no reservation) | 26 (reserved) |

Agent worker concurrency is one active turn per worker. The 16 GiB tiers share
the same memory headroom once artifact processing is reserved, so the scale
tier's extra CPU buys twice the agent workers (CPU-bound on the baseline),
not more browser runs or concurrent extractions, which are memory-bound. More
of those needs more memory (a 32 GiB tier), not more CPU.

The browser and extraction figures are **provisional**. They come from this
budget, not from measurement, because managed browsers (HOR-594) and artifact
processing (HOR-461/HOR-462) had not landed. Those tickets must measure them
on both instance types, replace the provisional values here, and feed the
browser pool default and the AWS sheet of the financial model
(DES-HOR-602-01).

## Changing a limit

Raise a request or limit only within this budget, through a reviewed chart or
overlay change. On a full node, reduce concurrency (AgentPool replicas,
browser or extraction pools) rather than overcommitting memory. Re-run the
load procedure above when a workload's profile changes.

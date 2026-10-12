#!/usr/bin/env bash
set -euo pipefail

# Resource contract (HOR-602, DES-HOR-602-01/02): every container any shipped
# chart renders — init containers, hook Jobs, helm tests, upstream subcharts and
# operator-managed Prometheus/Alertmanager — declares a CPU request, a memory
# request and a memory limit no lower than that request. A single-node install
# shares one memory pool, so an unconstrained container can starve Postgres or
# the kubelet instead of failing visibly inside its own limit. CPU limits are
# deliberately not required: CPU is compressible and throttling hurts latency
# without protecting neighbours (see docs/sizing.md).
#
# The umbrella must also render the release-namespace LimitRange that gives
# runtime-created pods (AgentPool workers, ModelBackend pods) the chart never
# renders a default request, and nothing else: no default limit, no max/min.
#
# Every profile below must stay a superset of what a supported install enables;
# add a profile when a new values path introduces workloads.
umbrella=charts/iterabase-platform
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

render() {
  local name=$1
  shift
  helm template "$@" >"$tmp/$name.yaml"
}

render default opo1 "$umbrella" --namespace portable-system
render kind opo1 "$umbrella" --namespace portable-system -f values-kind.yaml
render observability-tls opo1 "$umbrella" --namespace portable-system \
  -f values-observability.yaml -f values-tls.yaml
render internal-observability opo1 "$umbrella" --namespace portable-system \
  -f values-observability.yaml -f values-tls.yaml -f values-internal-observability.yaml
render private-control-plane opo1 "$umbrella" --namespace portable-system \
  -f values-tls.yaml -f values-private-control-plane.yaml
# Every optional edge/substrate dependency a production overlay may enable.
render edge opo1 "$umbrella" --namespace portable-system \
  -f values-observability.yaml -f values-tls.yaml \
  --set metallb.enabled=true --set metallb-config.enabled=true \
  --set 'metallb-config.addresses={192.0.2.10/32}' \
  --set external-dns.enabled=true --set 'external-dns.domainFilters={example.com.}'
# Upgrade-only recovery Jobs render only when `lookup` finds live objects, which
# offline `helm template` never does. Render a copy whose lookups return
# fixtures for an earlier attempt's admission webhook and a RollingUpdate Loki
# gateway, so both Jobs face the same contract. A missing lookup string fails.
upgrade=$tmp/upgrade-chart
cp -R "$umbrella" "$upgrade"
python3 - "$upgrade/templates" <<'PY_FIXTURE'
import pathlib, sys
fixtures = {
    "loki-gateway-rollout-recovery.yaml": (
        'lookup "apps/v1" "Deployment" $namespace $gatewayName',
        'dict "spec" (dict "strategy" (dict "type" "RollingUpdate"))'),
    "internal-ingress-admission-recovery.yaml": (
        'lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration" "" $admissionName',
        'dict "metadata" (dict "name" $admissionName)'),
}
for name, (call, fixture) in fixtures.items():
    path = pathlib.Path(sys.argv[1]) / name
    source = path.read_text()
    if call not in source:
        sys.exit(f"{name}: lookup fixture target changed: {call}")
    path.write_text(source.replace(call, fixture))
PY_FIXTURE
render upgrade-recovery opo1 "$upgrade" --namespace portable-system \
  -f values-observability.yaml -f values-tls.yaml -f values-internal-observability.yaml
render control-plane control-plane charts/control-plane
render control-plane-full control-plane charts/control-plane \
  --set gateway.enabled=true --set dispatch.enabled=true --set toolRunner.enabled=true \
  --set artifact.enabled=true --set metrics.enabled=true --set postgresql.metrics.enabled=true
render cert-manager opo1-cert-manager charts/cert-manager-substrate --namespace portable-system \
  --set global.internalTLS.enabled=true --set global.internalTLS.platformRelease=opo1
render lvm-storage opo1-lvm charts/lvm-storage-substrate --namespace openebs \
  --set agentpool.authorizedManagerIdentity=system:serviceaccount:portable-system:opo1-control-plane-manager

python3 - "$tmp"/*.yaml <<'PY'
import os, sys, yaml

UNITS = {"Ki": 2**10, "Mi": 2**20, "Gi": 2**30, "Ti": 2**40,
         "k": 10**3, "M": 10**6, "G": 10**9, "T": 10**12}

def bytes_of(q):
    q = str(q)
    for suffix in sorted(UNITS, key=len, reverse=True):
        if q.endswith(suffix):
            return float(q[:-len(suffix)]) * UNITS[suffix]
    return float(q)

def pod_spec(doc):
    kind, spec = doc.get("kind"), doc.get("spec") or {}
    if kind == "Pod":
        return spec
    if kind == "CronJob":
        spec = spec.get("jobTemplate", {}).get("spec", {})
    if kind in ("Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob"):
        return spec.get("template", {}).get("spec", {})
    return None

def problems(resources):
    resources = resources or {}
    req, lim = resources.get("requests") or {}, resources.get("limits") or {}
    out = [f"missing {field}" for field, src in
           (("requests.cpu", req.get("cpu")), ("requests.memory", req.get("memory")),
            ("limits.memory", lim.get("memory"))) if src is None]
    if not out and bytes_of(lim["memory"]) < bytes_of(req["memory"]):
        out.append(f"limits.memory {lim['memory']} below requests.memory {req['memory']}")
    return out

# Upstream containers with no resources value to set. Each entry needs a reason
# and is re-reviewed on every dependency bump; keep this list empty when possible.
EXEMPT = {
    # cert-manager-csi-driver v0.15.0 sizes only its driver container. Both
    # sidecars are small static binaries; the platform release-namespace
    # LimitRange gives them a default request whenever their pods are recreated.
    ("DaemonSet", "cert-manager-csi-driver", "node-driver-registrar"),
    ("DaemonSet", "cert-manager-csi-driver", "liveness-probe"),
}

failures, checked, limitranges, exempted, recovery_jobs = [], 0, {}, set(), set()
for path in sys.argv[1:]:
    profile = os.path.basename(path)[:-5]
    with open(path) as f:
        for doc in yaml.safe_load_all(f):
            if not doc:
                continue
            kind, name = doc.get("kind"), doc.get("metadata", {}).get("name")
            if kind == "LimitRange":
                limitranges.setdefault(profile, []).append(doc)
                continue
            if profile == "upgrade-recovery" and kind == "Job":
                recovery_jobs.add(name)
            targets = []
            spec = pod_spec(doc)
            if spec is not None:
                for c in spec.get("initContainers", []) + spec.get("containers", []):
                    targets.append((c["name"], c.get("resources")))
            elif kind in ("Prometheus", "Alertmanager"):
                targets.append(("<spec>", doc["spec"].get("resources")))
            for container, resources in targets:
                if (kind, name, container) in EXEMPT:
                    exempted.add((kind, name, container))
                    continue
                checked += 1
                for p in problems(resources):
                    failures.append(f"{profile}: {kind}/{name} container {container}: {p}")

for profile in ("default", "kind", "observability-tls", "internal-observability",
                "private-control-plane", "edge"):
    ranges = limitranges.get(profile, [])
    container_limits = [l for r in ranges for l in r["spec"]["limits"] if l.get("type") == "Container"]
    if len(container_limits) != 1:
        failures.append(f"{profile}: expected exactly one Container LimitRange entry, found {len(container_limits)}")
        continue
    entry = container_limits[0]
    request = entry.get("defaultRequest") or {}
    for field in ("cpu", "memory"):
        if field not in request:
            failures.append(f"{profile}: LimitRange defaultRequest missing {field}")
    # A default limit OOM-kills unsized ModelBackend pods; max/min reject them.
    for bound in ("default", "max", "min", "maxLimitRequestRatio"):
        if bound in entry:
            failures.append(f"{profile}: LimitRange must only default requests (DES-HOR-602-02), found {bound}")

# The lookup fixtures must keep rendering both upgrade recovery Jobs.
for job in ("opo1-loki-gateway-rollout-recovery", "opo1-internal-ingress-nginx-admission-recovery"):
    if job not in recovery_jobs:
        failures.append(f"upgrade-recovery: Job/{job} no longer renders under its lookup fixture")

for stale in sorted(EXEMPT - exempted):
    failures.append(f"stale exemption no longer rendered: {stale}")

if failures:
    print("FAIL: resource contract violations:", file=sys.stderr)
    for f in failures:
        print(f"  {f}", file=sys.stderr)
    sys.exit(1)
print(f"resource contract ok: {checked} containers across {len(sys.argv) - 1} renders")
PY

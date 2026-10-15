#!/usr/bin/env bash
set -euo pipefail

render=${1:-/tmp/iterabase-platform.internal-observability.rendered.yaml}
release=${2:-opo1}
namespace=${3:-portable-system}
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

additional_only_values=$(mktemp)
additional_only_render=$(mktemp)
trap 'rm -f "$additional_only_values" "$additional_only_render"' EXIT
cat >"$additional_only_values" <<'YAML'
addresses: []
additionalPools:
  - name: internal
    addresses:
      - 10.0.20.200-10.0.20.215
    autoAssign: false
    interfaces:
      - eth0
YAML
helm template demo "$script_dir/../charts/metallb-config" \
  -f "$additional_only_values" >"$additional_only_render"

python3 - "$render" "$release" "$namespace" "$additional_only_render" \
  "$script_dir/../charts/iterabase-platform/templates/loki-gateway-rollout-recovery.yaml" \
  "$script_dir/../charts/iterabase-platform/values.yaml" <<'PY'
from __future__ import annotations

import sys
from pathlib import Path

import yaml

(
    render,
    release,
    namespace,
    additional_only_render,
    rollout_recovery_template,
    platform_values_path,
) = sys.argv[1:]
objects = [obj for obj in yaml.safe_load_all(Path(render).read_text()) if obj]
additional_only_objects = [
    obj for obj in yaml.safe_load_all(Path(additional_only_render).read_text()) if obj
]


def object_(kind: str, name: str, source: list[dict] | None = None) -> dict:
    source = objects if source is None else source
    matches = [
        obj
        for obj in source
        if obj.get("kind") == kind and obj.get("metadata", {}).get("name") == name
    ]
    if len(matches) != 1:
        raise AssertionError(f"{kind}/{name}: got {len(matches)} objects, want 1")
    return matches[0]


def annotations(obj: dict) -> dict:
    return obj.get("metadata", {}).get("annotations", {})


public_class = object_("IngressClass", "nginx")
internal_class = object_("IngressClass", "nginx-internal")
assert public_class["spec"]["controller"] == "k8s.io/ingress-nginx"
assert internal_class["spec"]["controller"] == "k8s.io/ingress-nginx-internal"

controller = object_("Deployment", f"{release}-internal-ingress-nginx-controller")
args = controller["spec"]["template"]["spec"]["containers"][0]["args"]
for expected in (
    "--controller-class=k8s.io/ingress-nginx-internal",
    "--ingress-class=nginx-internal",
    "--election-id=ingress-nginx-internal-leader",
    f"--publish-service=$(POD_NAMESPACE)/{release}-internal-ingress-nginx-controller",
):
    assert expected in args, f"internal controller missing {expected}"

# The one-replica gateway keeps hard anti-affinity and deliberately accepts a
# short Recreate interruption rather than deadlocking on a one-node rollout.
loki_gateway = object_("Deployment", f"{release}-loki-gateway")
assert loki_gateway["spec"]["replicas"] == 1
assert loki_gateway["spec"]["strategy"] == {
    "type": "Recreate",
    "rollingUpdate": None,
}
required_anti_affinity = loki_gateway["spec"]["template"]["spec"]["affinity"][
    "podAntiAffinity"
]["requiredDuringSchedulingIgnoredDuringExecution"]
assert required_anti_affinity[0]["topologyKey"] == "kubernetes.io/hostname"
assert required_anti_affinity[0]["labelSelector"]["matchLabels"] == {
    "app.kubernetes.io/component": "gateway",
    "app.kubernetes.io/instance": release,
    "app.kubernetes.io/name": "loki",
}

# Admission stays fail-closed, and its CA never depends on a Helm hook running:
# cert-manager issues the serving certificate and cainjector maintains the
# webhook caBundle (DES-HOR-677-01). No certgen hook Job renders.
admission_name = f"{release}-internal-ingress-nginx-admission"
webhook = object_("ValidatingWebhookConfiguration", admission_name)
assert webhook["webhooks"][0]["failurePolicy"] == "Fail"
assert webhook["webhooks"][0]["clientConfig"]["service"] == {
    "name": f"{release}-internal-ingress-nginx-controller-admission",
    "namespace": namespace,
    "port": 443,
    "path": "/networking/v1/ingresses",
}
assert annotations(webhook)["cert-manager.io/inject-ca-from"] == f"{namespace}/{admission_name}"
admission_cert = object_("Certificate", admission_name)
assert admission_cert["spec"]["secretName"] == admission_name
assert admission_cert["spec"]["issuerRef"]["name"] == f"{release}-internal-ingress-nginx-root-issuer"
certgen_jobs = [
    obj["metadata"]["name"]
    for obj in objects
    if obj.get("kind") == "Job" and obj["metadata"]["name"].startswith(admission_name)
]
assert not certgen_jobs, f"admission certgen/recovery hook Jobs still render: {certgen_jobs}"

rollout_source = Path(rollout_recovery_template).read_text()
for contract in (
    'lookup "apps/v1" "Deployment"',
    '$desiredStrategy := dig "deploymentStrategy" "type" "" $gateway',
    '(eq $desiredStrategy "Recreate")',
    'helm.sh/hook-weight: "-1"',
    'resources: ["deployments"]',
    'resourceNames: [{{ $gatewayName | quote }}]',
    "--field-manager=helm",
    '--patch={"spec":{"strategy":{"type":"Recreate","rollingUpdate":null}}}',
):
    assert contract in rollout_source, f"Loki rollout recovery lost {contract}"
platform_values = yaml.safe_load(Path(platform_values_path).read_text())
recovery_image = platform_values["observabilityUpgradeRecovery"]["image"]
assert recovery_image == {
    "repository": "registry.k8s.io/kubectl",
    "tag": "v1.31.0",
    "digest": "sha256:99b37df34bc4f99ee322521d4c85cb98c1ceb8f70ff0618bef84eec9fe1ebc20",
    "pullPolicy": "IfNotPresent",
}

public_service = object_("Service", f"{release}-ingress-nginx-controller")
internal_service = object_("Service", f"{release}-internal-ingress-nginx-controller")
assert public_service["spec"]["type"] == "LoadBalancer"
assert public_service["spec"]["ipFamilies"] == ["IPv6"]
assert internal_service["spec"]["type"] == "LoadBalancer"
assert internal_service["spec"]["ipFamilies"] == ["IPv4"]
assert annotations(internal_service)["metallb.io/address-pool"] == f"{release}-internal"
assert annotations(internal_service)["metallb.io/loadBalancerIPs"] == "10.0.20.200"

edge_pool = object_("IPAddressPool", f"{release}-edge")
internal_pool = object_("IPAddressPool", f"{release}-internal")
assert edge_pool["spec"]["addresses"] == ["2001:db8:30::100-2001:db8:30::10f"]
assert edge_pool["spec"]["autoAssign"] is True
assert internal_pool["spec"]["addresses"] == ["10.0.20.200-10.0.20.215"]
assert internal_pool["spec"]["autoAssign"] is False
internal_advertisement = object_("L2Advertisement", f"{release}-internal")
assert internal_advertisement["spec"]["ipAddressPools"] == [f"{release}-internal"]
assert internal_advertisement["spec"]["interfaces"] == ["eth0"]

# Named pools remain independently usable when the backward-compatible primary
# edge pool is intentionally empty.
additional_only_pool = object_("IPAddressPool", "demo-internal", additional_only_objects)
assert additional_only_pool["spec"]["addresses"] == ["10.0.20.200-10.0.20.215"]
additional_only_advertisement = object_(
    "L2Advertisement", "demo-internal", additional_only_objects
)
assert additional_only_advertisement["spec"]["ipAddressPools"] == ["demo-internal"]
assert additional_only_advertisement["spec"]["interfaces"] == ["eth0"]
assert not [
    obj
    for obj in additional_only_objects
    if obj.get("metadata", {}).get("name") == "demo-edge"
    and obj.get("kind") in {"IPAddressPool", "L2Advertisement"}
]

expected_ingresses = {
    f"{release}-grafana": (
        "grafana.internal.example.com",
        f"{release}-grafana",
        80,
        "grafana-internal-example-tls",
        f"{release}-grafana.{namespace}.svc",
    ),
    f"{release}-kube-prometheus-stack-prometheus": (
        "prometheus.internal.example.com",
        f"{release}-kube-prometheus-stack-prometheus",
        9090,
        "prometheus-internal-example-tls",
        f"{release}-kube-prometheus-stack-prometheus.{namespace}.svc",
    ),
    f"{release}-kube-prometheus-stack-alertmanager": (
        "alertmanager.internal.example.com",
        f"{release}-kube-prometheus-stack-alertmanager",
        9093,
        "alertmanager-internal-example-tls",
        f"{release}-kube-prometheus-stack-alertmanager.{namespace}.svc",
    ),
    f"{release}-loki-gateway": (
        "loki.internal.example.com",
        f"{release}-loki-gateway",
        80,
        "loki-internal-example-tls",
        None,
    ),
}

for name, (host, service, port, tls_secret, backend_identity) in expected_ingresses.items():
    ingress = object_("Ingress", name)
    spec = ingress["spec"]
    assert spec["ingressClassName"] == "nginx-internal"
    assert spec["rules"][0]["host"] == host
    backend = spec["rules"][0]["http"]["paths"][0]["backend"]["service"]
    assert backend["name"] == service
    assert backend["port"]["number"] == port
    assert spec["tls"] == [{"hosts": [host], "secretName": tls_secret}]
    ingress_annotations = annotations(ingress)
    assert ingress_annotations["cert-manager.io/cluster-issuer"] == "letsencrypt-prod"
    assert ingress_annotations["external-dns.alpha.kubernetes.io/hostname"] == host
    assert ingress_annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"] == "false"
    assert "nginx.ingress.kubernetes.io/auth-type" not in ingress_annotations
    if backend_identity is None:
        # The Loki gateway is the chart-owned HTTP reverse proxy; it separately
        # verifies the gateway-to-Loki internal-CA HTTPS hop.
        assert "nginx.ingress.kubernetes.io/backend-protocol" not in ingress_annotations
    else:
        assert ingress_annotations["nginx.ingress.kubernetes.io/backend-protocol"] == "HTTPS"
        assert ingress_annotations["nginx.ingress.kubernetes.io/proxy-ssl-verify"] == "on"
        assert ingress_annotations["nginx.ingress.kubernetes.io/proxy-ssl-server-name"] == "on"
        assert ingress_annotations["nginx.ingress.kubernetes.io/proxy-ssl-name"] == backend_identity

# No observability route may leak onto the public controller.
for ingress_name in expected_ingresses:
    assert object_("Ingress", ingress_name)["spec"]["ingressClassName"] != "nginx"

prometheus = object_("Prometheus", f"{release}-kube-prometheus-stack-prometheus")
alertmanager = object_("Alertmanager", f"{release}-kube-prometheus-stack-alertmanager")
assert prometheus["spec"]["externalUrl"] == "https://prometheus.internal.example.com"
assert alertmanager["spec"]["externalUrl"] == "https://alertmanager.internal.example.com"

# Both ingress planes remain observable when the full observability preset is on.
for name in (
    f"{release}-ingress-nginx-controller",
    f"{release}-internal-ingress-nginx-controller",
):
    object_("ServiceMonitor", name)

print("internal observability ingress contract: ok")
PY

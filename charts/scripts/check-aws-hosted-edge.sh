#!/usr/bin/env bash
# Hosted AWS edge contract (HOR-601). The hosted preset renders an instance-mode
# NLB for ingress-nginx through the AWS Load Balancer Controller, HTTPS only,
# with authenticated origin pulls on every public Ingress (DES-HOR-601-04). The
# controller alone runs on the host network to reach IMDS (DES-HOR-601-05) and
# tags everything with the environment its node policy is bound to
# (DES-HOR-601-02). Every non-hosted render stays exactly as before: no
# controller, no NLB class, no origin-pull CA or client-certificate checks.
set -euo pipefail

hosted=$1
release=$2
namespace=$3
root=$(cd "$(dirname "$0")/.." && pwd)
umbrella="$root/charts/iterabase-platform"

python3 - "$hosted" "$release" "$namespace" <<'PY'
import sys
import yaml

path, release, namespace = sys.argv[1:4]
docs = [d for d in yaml.safe_load_all(open(path, encoding="utf-8")) if d]
errors = []


def one(kind, name):
    found = [d for d in docs if d["kind"] == kind and d["metadata"]["name"] == name]
    if len(found) != 1:
        errors.append(f"expected exactly one {kind}/{name}, found {len(found)}")
        return None
    return found[0]


edge = one("Service", f"{release}-ingress-nginx-controller")
if edge:
    spec = edge["spec"]
    annotations = edge["metadata"].get("annotations", {})
    if spec.get("type") != "LoadBalancer" or spec.get("loadBalancerClass") != "service.k8s.aws/nlb":
        errors.append("edge Service must be a LoadBalancer of class service.k8s.aws/nlb")
    if annotations.get("service.beta.kubernetes.io/aws-load-balancer-nlb-target-type") != "instance":
        errors.append("edge NLB must target instances (PIVOT-ARCH-003)")
    if annotations.get("service.beta.kubernetes.io/aws-load-balancer-scheme") != "internet-facing":
        errors.append("edge NLB must be internet-facing")
    if sorted(p["port"] for p in spec["ports"]) != [443]:
        errors.append(f"edge NLB must listen on 443 only, got {[p['port'] for p in spec['ports']]}")

controller = one("Deployment", f"{release}-aws-load-balancer-controller")
if controller:
    pod = controller["spec"]["template"]["spec"]
    args = pod["containers"][0]["args"]
    if pod.get("hostNetwork") is not True or pod.get("dnsPolicy") != "ClusterFirstWithHostNet":
        errors.append("controller must run on the host network with ClusterFirstWithHostNet DNS")
    if controller["spec"].get("replicas") != 1:
        errors.append("controller must run one replica on a single-node environment")
    strategy = controller["spec"].get("strategy", {})
    if strategy.get("type") == "RollingUpdate" and strategy.get("rollingUpdate", {}).get("maxSurge") != 0:
        errors.append("controller holds host ports: its rolling update must not surge")
    for required in ("--aws-region=eu-west-1", "--aws-vpc-id=", "--default-tags=iterabase-env="):
        if not any(arg.startswith(required) for arg in args):
            errors.append(f"controller args missing {required}")
    env_tag = next(a for a in args if a.startswith("--default-tags=")).split("iterabase-env=", 1)[1].split(",")[0]
    if f"--cluster-name={env_tag}" not in args:
        errors.append("controller clusterName must equal its iterabase-env tag")

hostnet = [
    f"{d['kind']}/{d['metadata']['name']}"
    for d in docs
    if d["kind"] in ("Deployment", "DaemonSet", "StatefulSet")
    and d["spec"]["template"]["spec"].get("hostNetwork")
]
if hostnet != [f"Deployment/{release}-aws-load-balancer-controller"]:
    errors.append(f"only the load balancer controller may use the host network, got {hostnet}")

mutating = [d for d in docs if d["kind"] == "MutatingWebhookConfiguration"]
for config in mutating:
    for hook in config.get("webhooks", []):
        if any(r.get("resources") == ["services"] for r in hook.get("rules", [])):
            errors.append(f"{hook['name']} would claim every LoadBalancer Service; the edge names its class")
for config in (d for d in docs if d["kind"] == "ValidatingWebhookConfiguration"):
    for hook in config.get("webhooks", []):
        if any("ingresses" in r.get("resources", []) for r in hook.get("rules", [])) and hook["name"].endswith("elbv2.k8s.aws"):
            errors.append(f"{hook['name']} would block nginx Ingress writes while the controller is down")

if any(d["kind"] == "IngressClass" and d["metadata"]["name"] == "alb" for d in docs):
    errors.append("no ALB IngressClass on the hosted edge")

ca = one("Secret", f"{release}-origin-pull-ca")
if ca and set(ca.get("data", {})) != {"ca.crt"}:
    errors.append("origin-pull CA Secret must hold ca.crt only")

ingresses = [d for d in docs if d["kind"] == "Ingress"]
if len(ingresses) < 2:
    errors.append(f"expected the gateway and control-plane Ingresses, got {len(ingresses)}")
external_dns = one("Deployment", f"{release}-external-dns")
zones = []
if external_dns:
    zones = [
        a.split("=", 1)[1].rstrip(".")
        for a in external_dns["spec"]["template"]["spec"]["containers"][0]["args"]
        if a.startswith("--domain-filter=")
    ]
for ingress in ingresses:
    annotations = ingress["metadata"].get("annotations", {})
    # Cloudflare Universal SSL covers the apex and first-level subdomains only.
    for rule in ingress["spec"].get("rules", []):
        host = rule.get("host", "")
        if not any(host.endswith("." + z) and "." not in host[: -len(z) - 1] for z in zones):
            errors.append(f"Ingress/{ingress['metadata']['name']} host {host!r} must be one label below {zones}")
    want = {
        "nginx.ingress.kubernetes.io/auth-tls-secret": f"{namespace}/{release}-origin-pull-ca",
        "nginx.ingress.kubernetes.io/auth-tls-verify-client": "on",
        "nginx.ingress.kubernetes.io/auth-tls-verify-depth": "1",
    }
    for key, value in want.items():
        if annotations.get(key) != value:
            errors.append(f"Ingress/{ingress['metadata']['name']} {key}={annotations.get(key)!r}, want {value!r}")

if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
PY

# Fail-closed guards: each override must refuse to render.
expect_failure() {
  local reason=$1
  shift
  if helm template "$release" "$umbrella" --namespace "$namespace" \
    -f "$root/values-auth.yaml" -f "$root/values-tls.yaml" -f "$root/values-aws-hosted.yaml" "$@" >/dev/null 2>&1; then
    echo "hosted render accepted $reason" >&2
    exit 1
  fi
}
expect_failure "a hosted edge without origin pulls" --set global.originPull.enabled=false
expect_failure "a missing origin-pull CA" --set-string global.originPull.caCert=
expect_failure "a private key as the origin-pull CA" --set-string 'global.originPull.caCert=-----BEGIN CERTIFICATE-----
-----BEGIN PRIVATE KEY-----'
expect_failure "a controller without the environment tag" --set aws-load-balancer-controller.defaultTags=null
expect_failure "a clusterName different from the environment" --set aws-load-balancer-controller.clusterName=other
expect_failure "a controller off the host network" --set aws-load-balancer-controller.hostNetwork=false
expect_failure "a missing VPC" --set-string aws-load-balancer-controller.vpcId=
expect_failure "an edge Service without the NLB class" --set ingress-nginx.controller.service.loadBalancerClass=null

# Non-hosted renders are unchanged: none of the hosted edge appears.
for preset in "" "$root/values-kind.yaml" "$root/values-private-control-plane.yaml"; do
  render=$(helm template "$release" "$umbrella" --namespace "$namespace" -f "$root/values-auth.yaml" \
    -f "$root/values-tls.yaml" ${preset:+-f "$preset"})
  if grep -qE 'aws-load-balancer|origin-pull|auth-tls-|service\.k8s\.aws/nlb' <<<"$render"; then
    echo "non-hosted render ${preset:-default} contains hosted edge resources" >&2
    exit 1
  fi
done

echo "hosted AWS edge contract: ok"

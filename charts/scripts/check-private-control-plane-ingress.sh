#!/usr/bin/env bash
set -euo pipefail

render=${1:-/tmp/iterabase-platform.private-control-plane.rendered.yaml}
release=${2:-opo1}
namespace=${3:-portable-system}
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

collision_error=$(mktemp)
invalid_proxied_error=$(mktemp)
trap 'rm -f "$collision_error" "$invalid_proxied_error"' EXIT

if helm template collision "$script_dir/../charts/control-plane" -f "$script_dir/../values-control-plane-auth.yaml" \
  --namespace "$namespace" \
  --set global.internalTLS.enabled=true \
  --set ingress.enabled=true \
  --set ingress.host=app.example.com \
  --set ingress.tls.enabled=true \
  --set ingress.tls.clusterIssuer=letsencrypt-prod \
  --set ingress.tls.secretName=collision-control-plane-api-tls \
  >/dev/null 2>"$collision_error"; then
  echo "edge/internal TLS Secret collision unexpectedly rendered" >&2
  exit 1
fi
grep -q "must differ from the internal API TLS Secret" "$collision_error"

if helm template invalid-proxied "$script_dir/../charts/control-plane" -f "$script_dir/../values-control-plane-auth.yaml" \
  --namespace "$namespace" \
  --set ingress.enabled=true \
  --set ingress.host=app.example.com \
  --set ingress.externalDns.enabled=true \
  --set-string ingress.externalDns.cloudflareProxied=invalid \
  >/dev/null 2>"$invalid_proxied_error"; then
  echo "invalid Cloudflare proxied value unexpectedly rendered" >&2
  exit 1
fi
grep -q 'cloudflareProxied must be empty, "true", or "false"' "$invalid_proxied_error"

python3 - "$render" "$release" "$namespace" <<'PY'
from __future__ import annotations

import sys
from pathlib import Path

import yaml

render, release, namespace = sys.argv[1:]
objects = [obj for obj in yaml.safe_load_all(Path(render).read_text()) if obj]


def object_(kind: str, name: str) -> dict:
    matches = [
        obj
        for obj in objects
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

public_service = object_("Service", f"{release}-ingress-nginx-controller")
internal_service = object_("Service", f"{release}-internal-ingress-nginx-controller")
assert public_service["spec"]["ipFamilies"] == ["IPv6"]
assert internal_service["spec"]["ipFamilies"] == ["IPv4"]
assert annotations(internal_service)["metallb.io/address-pool"] == f"{release}-internal"
assert annotations(internal_service)["metallb.io/loadBalancerIPs"] == "10.0.20.200"

api_name = f"{release}-control-plane-api"
api_ingress = object_("Ingress", api_name)
api_spec = api_ingress["spec"]
assert api_spec["ingressClassName"] == "nginx-internal"
assert api_spec["rules"] == [
    {
        "host": "app.private.example.com",
        "http": {
            "paths": [
                {
                    "path": "/",
                    "pathType": "Prefix",
                    "backend": {
                        "service": {
                            "name": api_name,
                            "port": {"number": 8080},
                        }
                    },
                }
            ]
        },
    }
]
edge_secret = f"{release}-control-plane-api-ingress-tls"
internal_secret = f"{release}-control-plane-api-tls"
assert edge_secret != internal_secret
assert api_spec["tls"] == [
    {"hosts": ["app.private.example.com"], "secretName": edge_secret}
]
api_annotations = annotations(api_ingress)
assert api_annotations["cert-manager.io/cluster-issuer"] == "letsencrypt-prod"
assert api_annotations["external-dns.alpha.kubernetes.io/hostname"] == "app.private.example.com"
assert api_annotations["external-dns.alpha.kubernetes.io/target"] == "10.0.20.200"
assert api_annotations["external-dns.alpha.kubernetes.io/cloudflare-proxied"] == "false"
assert api_annotations["nginx.ingress.kubernetes.io/backend-protocol"] == "HTTPS"
assert api_annotations["nginx.ingress.kubernetes.io/proxy-ssl-secret"] == (
    f"{namespace}/{internal_secret}"
)
assert api_annotations["nginx.ingress.kubernetes.io/proxy-ssl-secret"] != (
    f"{namespace}/{release}-internal-ca-root"
)
assert api_annotations["nginx.ingress.kubernetes.io/proxy-ssl-verify"] == "on"
assert api_annotations["nginx.ingress.kubernetes.io/proxy-ssl-server-name"] == "on"
assert api_annotations["nginx.ingress.kubernetes.io/proxy-ssl-name"] == (
    f"{api_name}.{namespace}.svc"
)

api_certificate = object_("Certificate", internal_secret)
assert api_certificate["spec"]["secretName"] == internal_secret
assert not api_certificate["spec"].get("isCA", False)
assert f"{api_name}.{namespace}.svc" in api_certificate["spec"]["dnsNames"]
assert "app.private.example.com" not in api_certificate["spec"]["dnsNames"]
root_certificate = object_("Certificate", f"{release}-internal-ca-root")
assert root_certificate["spec"]["isCA"] is True

# The public inference API remains on the public controller; the private
# control-plane route cannot be loaded by that class.
gateway_ingress = object_("Ingress", f"{release}-gateway")
assert gateway_ingress["spec"]["ingressClassName"] == "nginx"
assert api_ingress["spec"]["ingressClassName"] != "nginx"

print("private control-plane ingress contract: ok")
PY

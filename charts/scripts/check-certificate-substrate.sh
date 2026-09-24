#!/usr/bin/env bash
set -euo pipefail

platform=charts/iterabase-platform
substrate=charts/cert-manager-substrate
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

chart_version() {
  awk '$1 == "version:" { print $2; exit }' "$1/Chart.yaml"
}

platform_version=$(chart_version "$platform")
substrate_version=$(chart_version "$substrate")
if [[ "$platform_version" != "$substrate_version" ]]; then
  echo "error: platform $platform_version and certificate substrate $substrate_version must be released together" >&2
  exit 1
fi

substrate_render=$(helm template release-cert-manager "$substrate" -n iterabase-system)
substrate_tls_render=$(helm template release-cert-manager "$substrate" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set global.internalTLS.platformRelease=release)
platform_render=$(helm template release "$platform" -n iterabase-system)

grep -q '^kind: CustomResourceDefinition$' <<<"$substrate_render"
grep -q 'app.kubernetes.io/name: cert-manager' <<<"$substrate_render"
! grep -Eq '^kind: (Certificate|Issuer|ClusterIssuer)$' <<<"$substrate_render"

# The TLS-on companion still owns only cert-manager runtime resources. Its
# ordered hook bootstraps the future platform release's existing CA resources,
# waits for all Ready conditions, and labels them for normal platform adoption.
grep -Fq 'app.kubernetes.io/component: internal-ca-bootstrap' <<<"$substrate_tls_render"
grep -Fq 'owner_release="release"' <<<"$substrate_tls_render"
grep -Fq 'meta.helm.sh/release-name: release' <<<"$substrate_tls_render"
grep -Fq 'kubectl wait --for=condition=Ready clusterissuer/internal-ca' <<<"$substrate_tls_render"
grep -Fq 'internal-ca-bootstrap=pass' <<<"$substrate_tls_render"
grep -Fq 'docker.io/alpine/k8s:1.34.1@sha256:ec714df3813b5405292860f8a1c55c5727bf8c33c88992f1e981efad8065547f' <<<"$substrate_tls_render"

grep -q '^kind: ClusterIssuer$' <<<"$platform_render"
grep -q '^kind: Certificate$' <<<"$platform_render"
! grep -q '# Source: .*cert-manager/templates/' <<<"$platform_render"

if grep -R -q 'helm.sh/hook' \
  charts/cert-issuers/templates \
  charts/control-plane/templates/certificate.yaml \
  charts/control-plane/templates/tool-runner.yaml \
  charts/observability/templates/stack-internal-tls.yaml \
  charts/postgresql/templates/certificate.yaml \
  charts/redis/templates/certificate.yaml; then
  echo "error: cert-manager consumers must be normal resources in the ordered platform release" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# HOR-528: exactly one internal-CA definition, and the ordered companion can
# never rewrite it.
#
# The bootstrap hook and the platform chart both materialize the same three
# objects. Any spec divergence between them makes cert-manager re-issue the root
# Certificate on the next platform reconcile (a rotation that invalidates every
# leaf already signed by the current authority), so the hook must create the
# platform-owned objects only when absent, verify them instead of re-applying
# them, and hold no patch/update RBAC. These checks fail the repository check
# before such a divergence can reach a cluster.
# ---------------------------------------------------------------------------
assert_shared_ca_definition() {
  local substrate_tls_file=$1
  local platform_tls_file=$2
  local expected_common_name=$3
  local expected_duration=$4

  python3 - "$substrate_tls_file" "$platform_tls_file" "$expected_common_name" "$expected_duration" release <<'PY'
import sys
from pathlib import Path

import yaml

substrate_path, platform_path, expected_common_name, expected_duration, expected_owner = sys.argv[1:6]


def objects(path):
    return [document for document in yaml.safe_load_all(Path(path).read_text()) if document]


platform_objects = objects(platform_path)
platform_keyed = {}
for document in platform_objects:
    metadata = document.get("metadata", {})
    kind = document.get("kind")
    if kind == "Certificate":
        platform_keyed[("Certificate", metadata.get("name"))] = document
    elif kind == "ClusterIssuer":
        platform_keyed[("ClusterIssuer", metadata.get("name"))] = document

expected_keys = {
    ("ClusterIssuer", "release-internal-ca-bootstrap"),
    ("Certificate", "release-internal-ca-root"),
    ("ClusterIssuer", "internal-ca"),
}
missing = sorted(expected_keys - set(platform_keyed))
if missing:
    raise SystemExit(f"platform render is missing internal CA objects: {missing}")

certificate = platform_keyed[("Certificate", "release-internal-ca-root")]["spec"]
if certificate.get("commonName") != expected_common_name or certificate.get("duration") != expected_duration:
    raise SystemExit(
        "platform internal CA Certificate does not resolve the shared global.internalTLS.ca contract: "
        f"commonName={certificate.get('commonName')!r} duration={certificate.get('duration')!r}"
    )

hook = None
for document in objects(substrate_path):
    if document.get("kind") != "Job":
        continue
    labels = document.get("spec", {}).get("template", {}).get("metadata", {}).get("labels", {}) or {}
    if labels.get("app.kubernetes.io/component") == "internal-ca-bootstrap":
        hook = document
        break
if hook is None:
    raise SystemExit("substrate TLS render has no internal-ca-bootstrap Job")

script = hook["spec"]["template"]["spec"]["containers"][0]["args"][0]

documents = {}
for block in script.split("cat >/tmp/internal-ca-"):
    if "<<" not in block:
        continue
    path, _, remainder = block.partition("\n")
    _, _, body = remainder.partition("\n")
    body, _, _ = body.rpartition("YAML")
    document = yaml.safe_load(body)
    documents[path.strip()] = document

if not documents:
    raise SystemExit("internal CA bootstrap hook no longer writes its ordered manifests")

keyed = {}
for path, document in documents.items():
    keyed[(document["kind"], document["metadata"]["name"])] = document

for key in sorted(expected_keys):
    hook_document = keyed.get(key)
    if hook_document is None:
        raise SystemExit(f"bootstrap hook does not render {key}")
    if hook_document["spec"] != platform_keyed[key]["spec"]:
        raise SystemExit(
            f"internal CA definition diverged between the bootstrap hook and the platform chart for {key}:\n"
            f"  hook:     {hook_document['spec']}\n"
            f"  platform: {platform_keyed[key]['spec']}"
        )
    hook_owner = hook_document["metadata"].get("annotations", {}).get("meta.helm.sh/release-name")
    if hook_owner != expected_owner:
        raise SystemExit(f"{key} bootstrap manifest must carry the platform release ownership: hook={hook_owner!r}")

required = ["create_only /tmp/internal-ca-bootstrap.yaml", "create_only /tmp/internal-ca-root.yaml",
            "create_only /tmp/internal-ca-issuer.yaml", "verify_spec_keys", "verify_spec_value"]
for snippet in required:
    if snippet not in script:
        raise SystemExit(f"internal CA bootstrap hook lost its create-only/verify contract: {snippet!r}")
if "kubectl apply" in script:
    raise SystemExit("internal CA bootstrap hook must not re-apply (rewrite) the platform-owned internal CA")
PY
}

assert_hook_rbac_is_create_only() {
  python3 - "$1" <<'PY'
import sys
from pathlib import Path

import yaml

documents = [document for document in yaml.safe_load_all(Path(sys.argv[1]).read_text()) if document]
checked = set()
for document in documents:
    if document.get("kind") not in ("ClusterRole", "Role"):
        continue
    labels = document.get("metadata", {}).get("labels", {}) or {}
    if labels.get("app.kubernetes.io/name") != "cert-manager-substrate":
        continue  # cert-manager's own controller RBAC, not the ordered hook
    for rule in document.get("rules", []) or []:
        resources = set(rule.get("resources", []) or [])
        if not resources & {"clusterissuers", "certificates"}:
            continue
        for verb in ("patch", "update", "delete"):
            if verb in rule.get("verbs", []) or []:
                raise SystemExit(
                    f"{document['kind']}/{document['metadata']['name']} grants {verb!r} on {sorted(resources)}: "
                    "the ordered companion must never rewrite the platform-owned internal CA"
                )
        checked.add(document["kind"])
if not checked:
    raise SystemExit("internal CA bootstrap hook no longer declares CA object RBAC")
PY
}

# Same values, both writers: identical CA spec (single authority).
helm template release-cert-manager "$substrate" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set global.internalTLS.platformRelease=release > "$workdir/substrate-tls.yaml"
helm template release "$platform" -n iterabase-system \
  --set global.internalTLS.enabled=true > "$workdir/platform-tls.yaml"
assert_shared_ca_definition "$workdir/substrate-tls.yaml" "$workdir/platform-tls.yaml" iterabase-internal-ca 87600h
assert_hook_rbac_is_create_only "$workdir/substrate-tls.yaml"

# A shared override moves both writers together; nothing can diverge silently.
helm template release-cert-manager "$substrate" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set global.internalTLS.platformRelease=release \
  --set global.internalTLS.ca.commonName=shared-internal-ca \
  --set global.internalTLS.ca.duration=43800h > "$workdir/substrate-shared.yaml"
helm template release "$platform" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set global.internalTLS.ca.commonName=shared-internal-ca \
  --set global.internalTLS.ca.duration=43800h > "$workdir/platform-shared.yaml"
assert_shared_ca_definition "$workdir/substrate-shared.yaml" "$workdir/platform-shared.yaml" shared-internal-ca 43800h

# A per-chart CA identity override fails closed instead of diverging from the
# authority the ordered companion already bootstrapped.
if helm template release "$platform" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set cert-issuers.internal.ca.commonName=divergent-ca >/dev/null 2>&1; then
  echo "error: cert-issuers.internal.ca.commonName must fail the render (HOR-528)" >&2
  exit 1
fi
if helm template release "$platform" -n iterabase-system \
  --set global.internalTLS.enabled=true \
  --set cert-issuers.internal.ca.duration=43800h >/dev/null 2>&1; then
  echo "error: cert-issuers.internal.ca.duration must fail the render (HOR-528)" >&2
  exit 1
fi

echo "OK: same-version certificate substrate orders the platform-owned internal CA before dependent platform workloads; platform owns hook-free issuers and leaves; one shared internal-CA definition is created once and never rewritten (HOR-528)"

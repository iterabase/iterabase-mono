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
# HOR-528: one internal-CA identity for every writer.
#
# The ordered companion's bootstrap hook and the platform chart both materialize
# the same three objects. Any spec divergence between them would make cert-manager
# re-issue the root Certificate on the next platform reconcile — a rotation that
# invalidates every leaf already signed by the current authority. Both therefore
# resolve one identity (global.internalTLS.ca.*, then the legacy
# cert-issuers.internal.ca.*, then the chart default), and these checks fail the
# repository check before a divergence can reach a cluster.
#
# The identity also reaches the hook through the container environment rather
# than being interpolated into its shell source: the hook holds cluster-scoped
# write access, so a `$`/backtick sequence in a values-provided string must never
# execute. The hook manifest's identity fields must stay quoted so a value such as
# `12345` renders as the CRD's string type, not a YAML integer.
# ---------------------------------------------------------------------------
assert_shared_ca_identity() {
  local substrate_tls_file=$1
  local platform_tls_file=$2
  local expected_common_name=$3
  local expected_duration=$4
  local hostile_payload=${5:-}

  python3 - "$substrate_tls_file" "$platform_tls_file" "$expected_common_name" "$expected_duration" release "$hostile_payload" <<'PY'
import re
import sys
from pathlib import Path

import yaml

substrate_path, platform_path, expected_common_name, expected_duration, expected_owner, hostile = sys.argv[1:7]


def objects(path):
    return [document for document in yaml.safe_load_all(Path(path).read_text()) if document]


platform_keyed = {}
for document in objects(platform_path):
    metadata = document.get("metadata", {})
    kind = document.get("kind")
    if kind in ("Certificate", "ClusterIssuer"):
        platform_keyed[(kind, metadata.get("name"))] = document

expected_keys = {
    ("ClusterIssuer", "release-internal-ca-bootstrap"),
    ("Certificate", "release-internal-ca-root"),
    ("ClusterIssuer", "internal-ca"),
}
missing = sorted(expected_keys - set(platform_keyed))
if missing:
    raise SystemExit(f"platform render is missing internal CA objects: {missing}")

certificate = platform_keyed[("Certificate", "release-internal-ca-root")]["spec"]
if certificate.get("commonName") != expected_common_name or str(certificate.get("duration")) != expected_duration:
    raise SystemExit(
        "platform internal CA Certificate does not resolve the shared internal-CA identity: "
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

container = hook["spec"]["template"]["spec"]["containers"][0]
script = container["args"][0]

environment = {entry.get("name"): entry.get("value") for entry in container.get("env", []) or []}
if environment.get("CA_COMMON_NAME") != expected_common_name or str(environment.get("CA_DURATION")) != expected_duration:
    raise SystemExit(
        "the bootstrap hook must receive the shared identity through its container environment: "
        f"env={environment!r} want commonName={expected_common_name!r} duration={expected_duration!r}"
    )
if not re.search(r'ca_common_name="\$\{CA_COMMON_NAME', script) or not re.search(r'ca_duration="\$\{CA_DURATION', script):
    raise SystemExit("the bootstrap hook script must read the identity from its container environment")

marker = 'cat >"$manifest" <<\'YAML\'\n'
if marker not in script:
    raise SystemExit("internal CA bootstrap hook no longer writes its ordered manifests")
shell_before, _, heredoc = script.partition(marker)
heredoc_body, _, shell_after = heredoc.partition("\nYAML\n")
if hostile and (hostile in shell_before or hostile in shell_after):
    raise SystemExit(f"a values-provided identity reached the hook shell source: {hostile!r}")
if not re.search(r'(?m)^\s*commonName:\s+"', heredoc_body) or not re.search(r'(?m)^\s*duration:\s+"', heredoc_body):
    raise SystemExit("the hook manifest must quote the shared identity fields so YAML typing matches the platform render")

hook_documents = [document for document in yaml.safe_load_all(heredoc_body) if document]
if len(hook_documents) != 3:
    raise SystemExit(f"bootstrap hook must render three internal CA objects, found {len(hook_documents)}")
keyed = {(document["kind"], document["metadata"]["name"]): document for document in hook_documents}

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
PY
}

render_pair() {
  local suffix=$1
  shift
  helm template release-cert-manager "$substrate" -n iterabase-system \
    --set global.internalTLS.enabled=true \
    --set global.internalTLS.platformRelease=release "$@" > "$workdir/substrate-$suffix.yaml"
  helm template release "$platform" -n iterabase-system \
    --set global.internalTLS.enabled=true "$@" > "$workdir/platform-$suffix.yaml"
}

# Defaults: one identity for both writers.
render_pair defaults
assert_shared_ca_identity "$workdir/substrate-defaults.yaml" "$workdir/platform-defaults.yaml" iterabase-internal-ca 87600h

# Shared override: both writers move together.
render_pair global --set global.internalTLS.ca.commonName=shared-internal-ca --set global.internalTLS.ca.duration=43800h
assert_shared_ca_identity "$workdir/substrate-global.yaml" "$workdir/platform-global.yaml" shared-internal-ca 43800h

# Legacy location still works and still agrees with the companion.
render_pair legacy --set cert-issuers.internal.ca.commonName=legacy-internal-ca --set cert-issuers.internal.ca.duration=21900h
assert_shared_ca_identity "$workdir/substrate-legacy.yaml" "$workdir/platform-legacy.yaml" legacy-internal-ca 21900h

# Both set: the shared value wins for both writers (no silent divergence).
render_pair both \
  --set global.internalTLS.ca.commonName=shared-internal-ca \
  --set global.internalTLS.ca.duration=43800h \
  --set cert-issuers.internal.ca.commonName=legacy-internal-ca \
  --set cert-issuers.internal.ca.duration=21900h
assert_shared_ca_identity "$workdir/substrate-both.yaml" "$workdir/platform-both.yaml" shared-internal-ca 43800h

# A partial override resolves per key for both writers (global commonName, legacy duration).
render_pair mixed \
  --set global.internalTLS.ca.commonName=shared-internal-ca \
  --set cert-issuers.internal.ca.duration=21900h
assert_shared_ca_identity "$workdir/substrate-mixed.yaml" "$workdir/platform-mixed.yaml" shared-internal-ca 21900h

# A numeric-looking identity stays a string in both renders (CRD string field).
render_pair numeric --set global.internalTLS.ca.commonName=12345
assert_shared_ca_identity "$workdir/substrate-numeric.yaml" "$workdir/platform-numeric.yaml" 12345 87600h

# A values-provided shell sequence never reaches the hook's shell source.
render_pair hostile --set-string global.internalTLS.ca.commonName='$(id)'
assert_shared_ca_identity "$workdir/substrate-hostile.yaml" "$workdir/platform-hostile.yaml" '$(id)' 87600h '$(id)'

echo "OK: same-version certificate substrate orders the platform-owned internal CA before dependent platform workloads; platform owns hook-free issuers and leaves; both writers render one shared internal-CA identity and the hook receives it outside its shell source (HOR-528)"

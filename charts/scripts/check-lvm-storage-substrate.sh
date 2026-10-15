#!/usr/bin/env bash
set -euo pipefail

platform=charts/iterabase-platform
substrate=charts/lvm-storage-substrate
certificates=charts/cert-manager-substrate
chart_version() { awk '$1 == "version:" { print $2; exit }' "$1/Chart.yaml"; }
platform_version=$(chart_version "$platform")
substrate_version=$(chart_version "$substrate")
certificate_version=$(chart_version "$certificates")
[[ "$platform_version" == "$substrate_version" && "$platform_version" == "$certificate_version" ]] || {
  echo "error: platform and both substrate companions must share one version" >&2
  exit 1
}

render=$(helm template release-lvm-storage "$substrate" -n iterabase-system --include-crds \
  --set-string agentpool.authorizedManagerIdentity=system:serviceaccount:iterabase-system:release-control-plane-manager)
[[ $(grep -c '^kind: CustomResourceDefinition$' <<<"$render") -eq 3 ]]
for crd in lvmnodes.local.openebs.io lvmvolumes.local.openebs.io lvmsnapshots.local.openebs.io; do
  [[ $(grep -Fc "name: $crd" <<<"$render") -eq 1 ]] || { echo "missing or duplicate $crd" >&2; exit 1; }
done
for forbidden in \
  volumesnapshots.snapshot.storage.k8s.io \
  volumesnapshotcontents.snapshot.storage.k8s.io volumesnapshotclasses.snapshot.storage.k8s.io \
  'kind: VolumeSnapshotClass' 'name: csi-snapshotter' 'name: snapshot-controller' \
  'name: openebs-lvm-snapshotter-role' 'sig-storage/csi-snapshotter' 'sig-storage/snapshot-controller'; do
  if grep -Fq "$forbidden" <<<"$render"; then
    echo "error: bounded LVM substrate rendered forbidden CSI/user snapshot surface: $forbidden" >&2
    exit 1
  fi
done
snapshot_rules=$(awk '/resources: \["lvmsnapshots"\]/{getline; sub(/^[[:space:]]+/, ""); if ($0 ~ /^verbs:/) print}' <<<"$render")
[[ "$snapshot_rules" == $'verbs: ["list", "watch"]\nverbs: ["list", "watch"]' ]] || {
  echo "error: LVMSnapshot authority must be exactly list/watch for controller and node drivers: $snapshot_rules" >&2
  exit 1
}
grep -Fq 'value: "false"' <<<"$render"
grep -Fq -- '--kubelet-dir=/var/lib/kubelet/' <<<"$render"
grep -Fq '/var/lib/kubelet/plugins_registry/' <<<"$render"
grep -Fq 'name: local.csi.openebs.io' <<<"$render"

for image in \
  'docker.io/openebs/lvm-driver:1.10.0@sha256:b5932b4df5cde1f72914f665a4e4b480afd38dbc6577d7552b9f509d52e8c094' \
  'registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0@sha256:d7138bcc3aa5f267403d45ad4292c95397e421ea17a0035888850f424c7de25d' \
  'registry.k8s.io/sig-storage/csi-resizer:v2.0.0@sha256:4a95d94e57ad82f6977cd8d4fdcfcfc0b83f02d990e4e7715b688c20970a906d' \
  'registry.k8s.io/sig-storage/csi-provisioner:v6.1.0@sha256:e5900dc98b0d02f317ea9572f3824983c47a2fb5d5fe2b160aedf1b99c14c9e4'; do
  grep -Fq "image: \"$image\"" <<<"$render" || { echo "missing exact image $image" >&2; exit 1; }
done

[[ $(grep -c '^kind: StorageClass$' <<<"$render") -eq 3 ]]
for class in iterabase-lvm-xfs iterabase-agentpool-lvm-xfs iterabase-signatures-lvm-xfs; do
  grep -Fq "name: $class" <<<"$render"
done
[[ $(grep -c 'provisioner: local.csi.openebs.io' <<<"$render") -eq 3 ]]
[[ $(grep -c 'vgpattern: \^iterabase-data\$' <<<"$render") -eq 3 ]]
[[ $(grep -c 'fsType: xfs' <<<"$render") -eq 3 ]]
[[ $(grep -c 'thinProvision: "no"' <<<"$render") -eq 3 ]]
[[ $(grep -c 'volumeBindingMode: WaitForFirstConsumer' <<<"$render") -eq 3 ]]
[[ $(grep -c 'reclaimPolicy: Delete' <<<"$render") -eq 3 ]]
[[ $(grep -c 'allowVolumeExpansion: true' <<<"$render") -eq 3 ]]
[[ $(grep -c 'storageclass.kubernetes.io/is-default-class: "false"' <<<"$render") -eq 3 ]]
grep -A14 'name: iterabase-lvm-xfs' <<<"$render" | grep -Fq 'shared: "no"'
grep -A15 'name: iterabase-agentpool-lvm-xfs' <<<"$render" | grep -Fq 'shared: "yes"'
# DES-HOR-461-05: the signature class is shared and admits exactly one claim.
grep -A16 'name: iterabase-signatures-lvm-xfs' <<<"$render" | grep -Fq 'shared: "yes"'
grep -Fq "expression: \"object.metadata.namespace == 'iterabase-system' && object.metadata.name == 'iterabase-control-plane-artifact-signatures'\"" <<<"$render" || {
  echo "error: the signature class must admit exactly the artifact signature claim" >&2
  exit 1
}

# The generated dependency itself must be the deterministic bounded derivative,
# not the untouched upstream archive with CSI snapshot files or broad authority.
dependency="$substrate/charts/lvm-localpv-1.10.0.tgz"
[[ -f "$dependency" ]]
members=$(tar tzf "$dependency")
grep -Fxq lvm-localpv/charts/crds/templates/lvmsnapshot.yaml <<<"$members" || {
  echo "error: generated dependency omitted the inert LVMSnapshot CRD" >&2
  exit 1
}
for removed in \
  lvm-localpv/charts/crds/templates/csi-volume-snapshot-class.yaml \
  lvm-localpv/charts/crds/templates/csi-volume-snapshot-content.yaml \
  lvm-localpv/charts/crds/templates/csi-volume-snapshot.yaml; do
  ! grep -Fxq "$removed" <<<"$members" || { echo "error: generated dependency retained $removed" >&2; exit 1; }
done
generated_runtime=$(for path in \
  lvm-localpv/templates/lvm-controller.yaml \
  lvm-localpv/templates/rbac.yaml \
  lvm-localpv/values.yaml \
  lvm-localpv/charts/crds/values.yaml; do tar xOzf "$dependency" "$path"; done)
if grep -E -q 'csi-snapshotter|snapshot-controller|snapshot[.]storage[.]k8s[.]io' <<<"$generated_runtime"; then
  echo "error: generated dependency retained CSI/user snapshot runtime authority" >&2
  exit 1
fi
grep -Fq 'resources: ["lvmsnapshots"]' <<<"$generated_runtime"
grep -A1 -F 'resources: ["lvmsnapshots"]' <<<"$generated_runtime" | grep -Fq 'verbs: ["list", "watch"]'

# DES-HOR-545-01 / DES-HOR-545-07: the AgentPool class must be gated by a
# fail-closed policy that permits only the exact manager to raise a request.
grep -Fq 'kind: ValidatingAdmissionPolicy' <<<"$render"
grep -Fq 'kind: ValidatingAdmissionPolicyBinding' <<<"$render"
grep -Fq 'name: iterabase-agentpool-claim-authority' <<<"$render"
grep -Fq 'failurePolicy: Fail' <<<"$render"
grep -Fq 'validationActions: [Deny]' <<<"$render"
grep -Fq 'request.userInfo.username == '\''system:serviceaccount:iterabase-system:release-control-plane-manager'\''' <<<"$render"
# Operation-aware: CREATE is manager-gated; UPDATE keeps identity fields
# immutable, permits only manager-owned request changes, and requires growth.
grep -Fq 'operations: ["CREATE", "UPDATE"]' <<<"$render"
grep -Fq 'request.operation != '\''CREATE'\'' || request.userInfo.username' <<<"$render"
grep -Fq 'request.operation != '\''UPDATE'\'' || (has(oldObject.spec)' <<<"$render"
grep -Fq 'object.spec.storageClassName == oldObject.spec.storageClassName' <<<"$render"
grep -Fq 'object.spec.accessModes == oldObject.spec.accessModes' <<<"$render"
grep -Fq 'Only the control-plane manager may change an AgentPool PVC storage request' <<<"$render"
grep -Fq 'quantity(object.spec.resources.requests.storage).isGreaterThan(quantity(oldObject.spec.resources.requests.storage))' <<<"$render"
grep -Fq 'Only the control-plane manager may change AgentPool resize observation evidence' <<<"$render"
grep -Fq 'Only the control-plane manager may change AgentPool resize capacity evidence' <<<"$render"
grep -Fq 'resources: ["persistentvolumeclaims"]' <<<"$render"

# DES-HOR-545-05: the inert provider schema is protected by a fail-closed,
# cluster-wide deny-all CREATE admission policy. No identity is exempt.
[[ $(grep -Fc 'name: iterabase-lvmsnapshot-create-deny' <<<"$render") -eq 2 ]]
grep -Fq 'iterabase.com/architecture-decision: DES-HOR-545-05' <<<"$render"
grep -Fq 'apiGroups: ["local.openebs.io"]' <<<"$render"
grep -Fq 'apiVersions: ["v1alpha1"]' <<<"$render"
grep -Fq 'operations: ["CREATE"]' <<<"$render"
grep -Fq 'resources: ["lvmsnapshots"]' <<<"$render"
grep -Fq 'expression: "false"' <<<"$render"
grep -Fq 'policyName: iterabase-lvmsnapshot-create-deny' <<<"$render"

for values in "" "-f values-observability.yaml"; do
  platform_render=$(helm template release "$platform" -f values-auth.yaml $values)
  # DES-HOR-461-05: the artifact signature PVC is the only claim on the
  # shared signature class; every other platform data claim stays on
  # iterabase-lvm-xfs.
  explicit_classes=$(awk '$1 == "storageClassName:" && NF > 1 {gsub(/"/, "", $2); print $2}' <<<"$platform_render" | grep -vx iterabase-signatures-lvm-xfs | sort -u)
  [[ "$explicit_classes" == iterabase-lvm-xfs ]] || {
    echo "error: every rendered platform data claim must explicitly use iterabase-lvm-xfs: $explicit_classes" >&2
    exit 1
  }
  signature_claims=$(python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
print("\n".join(d["metadata"]["name"] for d in docs
    if d.get("kind") == "PersistentVolumeClaim" and d["spec"].get("storageClassName") == "iterabase-signatures-lvm-xfs"))' <<<"$platform_render")
  [[ "$signature_claims" == release-control-plane-artifact-signatures ]] || {
    echo "error: only the artifact signature PVC may use iterabase-signatures-lvm-xfs: $signature_claims" >&2
    exit 1
  }
done

if helm template release "$platform" -f values-auth.yaml -f values-observability.yaml --set observability.kube-prometheus-stack.grafana.persistence.storageClassName=alternate >/dev/null 2>&1; then
  echo "error: observability accepted an alternate StorageClass" >&2
  exit 1
fi
if helm template release-lvm-storage "$substrate" --set lvm-localpv.enabled=false >/dev/null 2>&1; then
  echo "error: LVM substrate accepted disabled OpenEBS authority" >&2
  exit 1
fi

echo "OK: pinned expandable OpenEBS volume authority, grow-only AgentPool admission, inert LVMSnapshot deny boundary, CSI/user snapshot absence, narrowed RBAC, and exact managed classes"

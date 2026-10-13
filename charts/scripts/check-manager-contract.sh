#!/usr/bin/env bash
set -euo pipefail

rendered=$(helm template manager-contract charts/control-plane -f values-control-plane-auth.yaml)

for crd in agentpools.platform.iterabase.com modelbackends.platform.iterabase.com workflows.platform.iterabase.com; do
  if ! grep -q "^  name: ${crd}$" <<<"$rendered"; then
    echo "ERROR: missing manager-required CRD ${crd}" >&2
    exit 1
  fi
done
grep -q '^              persistentVolumes:$' <<<"$rendered" || {
  echo "ERROR: ModelBackend CRD omits managed persistentVolumes" >&2
  exit 1
}
grep -q '^                      - iterabase-lvm-xfs$' <<<"$(grep -A55 '^              persistentVolumes:$' <<<"$rendered")" || {
  echo "ERROR: ModelBackend managed storage omits the exact class enum" >&2
  exit 1
}
echo "OK: AgentPool, ModelBackend managed storage, and Workflow CRDs render"

rbac=$(helm template manager-contract charts/control-plane -f values-control-plane-auth.yaml --show-only templates/rbac.yaml)
for resource in configmaps persistentvolumeclaims persistentvolumes pods services secrets storageclasses lvmnodes lvmvolumes deployments networkpolicies agentpools modelbackends models workflows; do
  if ! grep -q "^  - ${resource}$" <<<"$rbac"; then
    echo "ERROR: manager ClusterRole is missing ${resource}" >&2
    exit 1
  fi
done
for subresource in agentpools/finalizers workflows/finalizers agentpools/status workflows/status; do
  if ! grep -q "^  - ${subresource}$" <<<"$rbac"; then
    echo "ERROR: manager ClusterRole is missing ${subresource}" >&2
    exit 1
  fi
done
for retired in identitymappings permissionpolicies egressroutes; do
  if grep -q "${retired}" <<<"$rendered"; then
    echo "ERROR: retired legacy authority resource ${retired} still renders (HOR-582)" >&2
    exit 1
  fi
done
echo "OK: manager ClusterRole renders all controller-required resources and no retired CRD"

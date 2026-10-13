#!/usr/bin/env bash
set -euo pipefail

# DES-HOR-597-01/03: the inference gateway runs as its own ServiceAccount with
# namespace-scoped `get` on Secrets only, and that ServiceAccount is kept by
# Helm so a rollback to a chart without it leaves the older gateway pod
# schedulable (its pod spec keeps the defaulted spec.serviceAccount alias).
# The charts/n-1-upgrade rollback stage proves the live behaviour.
release=gateway-rbac
namespace=portable-system

rendered=$(helm template "$release" charts/iterabase-platform --namespace "$namespace")
gateway() {
  yq eval "select(.kind == \"$1\" and .metadata.labels.\"app.kubernetes.io/name\" == \"inference-gateway\")" - <<<"$rendered"
}

fail() {
  echo "ERROR: inference gateway RBAC: $*" >&2
  exit 1
}

sa=$(gateway ServiceAccount)
[[ -n "$sa" ]] || fail 'no ServiceAccount rendered'
sa_name=$(yq eval '.metadata.name' - <<<"$sa")
[[ "$(yq eval '.metadata.annotations."helm.sh/resource-policy"' - <<<"$sa")" == keep ]] ||
  fail "ServiceAccount $sa_name must carry helm.sh/resource-policy: keep"

for kind in Role RoleBinding; do
  [[ "$(gateway "$kind" | yq eval '.metadata.annotations."helm.sh/resource-policy"' -)" == null ]] ||
    fail "$kind must not be kept: a rolled-back gateway must lose Secret access"
done

rules=$(gateway Role | yq eval -o=json -I=0 '.rules' -)
[[ "$rules" == '[{"apiGroups":[""],"resources":["secrets"],"verbs":["get"]}]' ]] ||
  fail "Role must grant only get on secrets, got $rules"

[[ "$(gateway Deployment | yq eval '.spec.template.spec.serviceAccountName' -)" == "$sa_name" ]] ||
  fail "Deployment must run as $sa_name"

echo 'OK: inference gateway runs as a kept ServiceAccount with get-only Secret access'

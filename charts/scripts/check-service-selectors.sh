#!/usr/bin/env bash
set -euo pipefail

assert_service_selector() {
  local chart="$1"
  local template="$2"
  local expected="$3"
  shift 3
  local rendered actual

  rendered=$(helm template selector-check "charts/$chart" \
    --set metrics.enabled=true \
    "$@" \
    --show-only "templates/$template")
  actual=$(awk '
    /^kind: Service$/ { service = 1; spec = 0; selector = 0; next }
    service && /^spec:$/ { spec = 1; next }
    service && spec && /^  selector:$/ { selector = 1; next }
    service && selector && /^    app\.kubernetes\.io\/component:/ { print $2; exit }
  ' <<<"$rendered")

  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: $chart/$template Service component selector: expected '$expected', got '${actual:-<missing>}'" >&2
    return 1
  fi
  echo "OK: $chart/$template Service selects component=$expected"
}

assert_service_selector postgresql service.yaml database
assert_service_selector postgresql exporter.yaml exporter
assert_service_selector redis service.yaml cache
assert_service_selector redis exporter.yaml exporter
assert_service_selector control-plane gateway.yaml gateway --set gateway.enabled=true
assert_service_selector control-plane dispatch.yaml dispatch \
  --set dispatch.enabled=true --set dispatch.defaultModel.id=test --set dispatch.defaultModel.api=test

# Datastore and exporter Services stay disjoint only while each workload's pod
# template carries exactly the component its Service selects. Together with the
# Service selectors above, this proves the endpoint separation the observability
# E2E previously observed live.
assert_workload_component() {
  local chart="$1"
  local template="$2"
  local expected="$3"
  local rendered actual

  rendered=$(helm template selector-check "charts/$chart" \
    --set metrics.enabled=true \
    --show-only "templates/$template")
  actual=$(yq eval 'select(.kind == "Deployment" or .kind == "StatefulSet") | .spec.template.metadata.labels."app.kubernetes.io/component"' - <<<"$rendered")

  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: $chart/$template pod template component: expected '$expected', got '${actual:-<missing>}'" >&2
    return 1
  fi
  echo "OK: $chart/$template pods carry component=$expected"
}

assert_workload_component postgresql statefulset.yaml database
assert_workload_component postgresql exporter.yaml exporter
assert_workload_component redis deployment.yaml cache
assert_workload_component redis exporter.yaml exporter

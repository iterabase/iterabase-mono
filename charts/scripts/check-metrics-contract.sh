#!/usr/bin/env bash
set -euo pipefail

cp=$(helm template metrics charts/control-plane -f values-control-plane-auth.yaml \
  --set metrics.enabled=true \
  --set gateway.enabled=true \
  --set dispatch.enabled=true \
  --set dispatch.defaultModel.id=e2e \
  --set dispatch.defaultModel.api=openai)
ig=$(helm template metrics charts/inference-gateway --set metrics.enabled=true)
dcgm=$(helm template metrics charts/observability \
  --set dcgmExporter.enabled=true \
  --show-only templates/dcgm-servicemonitor.yaml)
lvm=$(helm template metrics charts/observability \
  --show-only templates/lvm-storage-servicemonitor.yaml)
dcgm_off=$(helm template metrics-off charts/observability)
cp_off=$(helm template metrics-off charts/control-plane -f values-control-plane-auth.yaml)
ig_off=$(helm template metrics-off charts/inference-gateway)

for rendered in "$cp" "$ig"; do
  grep -q 'name: http-metrics' <<<"$rendered"
  grep -q 'name: METRICS_' <<<"$rendered"
  grep -q 'port: http-metrics' <<<"$rendered"
done
grep -q -- '--metrics-bind-address=:8080' <<<"$cp"
grep -q -- '--metrics-secure=false' <<<"$cp"
grep -q 'replacement: harness' <<<"$cp"
grep -q 'app.kubernetes.io/component: harness' <<<"$cp"

[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.namespaceSelector.any' <<<"$dcgm") == "true" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.selector.matchLabels.app' <<<"$dcgm") == "nvidia-dcgm-exporter" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.endpoints[0].port' <<<"$dcgm") == "gpu-metrics" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.endpoints[0].path' <<<"$dcgm") == "/metrics" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.selector.matchLabels.name' <<<"$lvm") == "openebs-lvm-node" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.selector.matchLabels."openebs.io/component-name"' <<<"$lvm") == "openebs-lvm-node" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.endpoints[0].port' <<<"$lvm") == "metrics" ]]
[[ $(yq eval 'select(.kind == "ServiceMonitor") | .spec.endpoints[0].path' <<<"$lvm") == "/metrics" ]]
if grep -q 'name: metrics-off-dcgm-exporter' <<<"$dcgm_off"; then
  echo 'ERROR: DCGM ServiceMonitor renders while disabled' >&2
  exit 1
fi

if grep -q 'name: http-metrics' <<<"$cp_off" || grep -q 'name: METRICS_ADDR' <<<"$cp_off"; then
  echo 'ERROR: control-plane metrics listener renders while disabled' >&2
  exit 1
fi
if grep -q 'name: http-metrics' <<<"$ig_off" || grep -q 'name: METRICS_ENABLED' <<<"$ig_off"; then
  echo 'ERROR: inference metrics listener renders while disabled' >&2
  exit 1
fi

echo 'OK: metrics-only listeners, named ports, OpenEBS/DCGM monitor discovery, manager binding, harness PodMonitor, and disabled paths render coherently'

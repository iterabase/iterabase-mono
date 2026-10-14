#!/usr/bin/env bash
set -euo pipefail

base=$(helm template alert-contract charts/observability --show-only templates/iterabase-alerts.yaml)
configured=$(helm template alert-contract charts/observability --show-only templates/iterabase-alerts.yaml \
  --set alerts.performance.apiP95Seconds=2 \
  --set alerts.performance.inferenceP95Seconds=30 \
  --set alerts.performance.inferenceTTFTP95Seconds=5 \
  --set alerts.performance.harnessTurnP95Seconds=600 \
  --set alerts.performance.modelQueueDepth=8 \
  --set alerts.performance.gpuUtilizationPercent=95)

if grep -Eq '(^|[^[:alnum:]_])http_requests_total' <<<"$base"; then
  echo 'ERROR: stale nonexistent http_requests_total alert contract remains' >&2
  exit 1
fi
alerts=$(grep -c '^        - alert: Iterabase' <<<"$base")
runbooks=$(grep -c 'runbook_url:' <<<"$base")
actions=$(grep -c 'first_action:' <<<"$base")
if [[ "$alerts" -ne "$runbooks" || "$alerts" -ne "$actions" ]]; then
  echo "ERROR: every default alert requires runbook and first action (alerts=$alerts runbooks=$runbooks actions=$actions)" >&2
  exit 1
fi
for alert in ControlPlaneAPILatencyHigh InferenceLatencyHigh InferenceTTFTHigh HarnessTurnLatencyHigh ModelQueueHigh GPUUtilizationHigh; do
  if grep -q "Iterabase$alert" <<<"$base"; then
    echo "ERROR: configurable performance alert Iterabase$alert rendered without an accepted threshold" >&2
    exit 1
  fi
  grep -q "Iterabase$alert" <<<"$configured" || {
    echo "ERROR: configured performance alert Iterabase$alert did not render" >&2
    exit 1
  }
done
for rule in 'iterabase:control_plane_api_error_ratio5m' 'iterabase:inference_error_ratio5m' 'iterabase:inference_ttft_p95_5m'; do
  grep -q "$rule" <<<"$base" || { echo "ERROR: missing recording rule $rule" >&2; exit 1; }
done
# The manager reconciliation alert must target the stable control-plane manager
# scrape identity so unrelated controller-runtime producers (MetalLB, GPU
# Operator, ingress) cannot trigger it.
grep -Fq 'controller_runtime_reconcile_total{result="error",component="manager"}' <<<"$base" || {
  echo 'ERROR: manager reconciliation alert must target the stable component="manager" scrape identity' >&2
  exit 1
}
for contract in \
  'control_plane_dispatch_workspace_free_ratio) < 0.25' \
  'control_plane_dispatch_workspace_credit_gated) == 1' \
  'control_plane_harness_storage_checks_total{result="fail"}' \
  'lvm_vg_free_size_bytes{name="iterabase-data"} / lvm_vg_total_size_bytes{name="iterabase-data"}' \
  'kube_persistentvolumeclaim_info{storageclass=~"iterabase-(agentpool-)?lvm-xfs"}' \
  'max by (namespace) (control_plane_artifact_scanner_ready) == 0' \
  'max by (namespace) (control_plane_artifact_signature_age_seconds) > 86400' \
  'control_plane_artifact_security_attempts_total{outcome=~"transient|lease_lost"}'; do
  grep -Fq "$contract" <<<"$base" || {
    echo "ERROR: missing workspace/artifact alert contract: $contract" >&2
    exit 1
  }
done
echo "OK: $alerts invariant alerts carry runbooks/actions; six performance alerts are threshold-gated; per-pool and aggregate OpenEBS LVM capacity/I/O alerts render"

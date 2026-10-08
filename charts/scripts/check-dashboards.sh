#!/usr/bin/env bash
set -euo pipefail

./scripts/render-iterabase-dashboards.py --check
rendered=$(helm template dashboard-contract charts/iterabase-platform -f values-observability.yaml)
labels=$(grep -c 'grafana_dashboard: "1"' <<<"$rendered")
folders=$(grep -c 'grafana_folder:' <<<"$rendered")
iterabase=$(grep -c 'grafana_folder: Iterabase' <<<"$rendered")
infrastructure=$(grep -c 'grafana_folder: Infrastructure' <<<"$rendered")
observability=$(grep -c 'grafana_folder: Observability' <<<"$rendered")
if [[ "$labels" -ne "$folders" ]]; then
  echo "ERROR: every provisioned dashboard must have an organized Grafana folder (dashboards=$labels folders=$folders)" >&2
  exit 1
fi
if [[ "$iterabase" -ne 7 || "$infrastructure" -ne 1 || "$observability" -ne 1 ]]; then
  echo "ERROR: expected organized dashboard suite Iterabase=7 Infrastructure=1 Observability=1; got $iterabase/$infrastructure/$observability" >&2
  exit 1
fi
for uid in platform-overview control-plane execution-runtime tool-runtime inference-model-serving data-storage platform-infrastructure; do
  grep -q '"uid": "iterabase-'"$uid"'"' <<<"$rendered" || {
    echo "ERROR: missing stable dashboard uid iterabase-$uid" >&2
    exit 1
  }
done
for uid in infrastructure-components observability-stack; do
  grep -q '"uid": "iterabase-'"$uid"'"' <<<"$rendered" || {
    echo "ERROR: missing stable auxiliary dashboard uid iterabase-$uid" >&2
    exit 1
  }
done
# The manager reconciliation panel must target the stable control-plane manager
# scrape identity so unrelated controller-runtime producers (MetalLB, GPU
# Operator, ingress) cannot be misattributed.
grep -Fq 'controller_runtime_reconcile_total{namespace=~\"$namespace\",result=\"error\",component=\"manager\"}' <<<"$rendered" || {
  echo 'ERROR: manager reconciliation dashboard panel must target the stable component="manager" scrape identity' >&2
  exit 1
}
for title in \
  'AgentPool PVC free bytes' \
  'AgentPool PVC free ratio' \
  'AgentPool capacity warnings' \
  'AgentPool credit gates' \
  'iterabase-data free bytes' \
  'iterabase-data free ratio'; do
  grep -Fq "\"title\": \"$title\"" <<<"$rendered" || {
    echo "ERROR: 50 — Data and Storage is missing dedicated workspace panel: $title" >&2
    exit 1
  }
done
for query in \
  'control_plane_dispatch_workspace_free_bytes' \
  'control_plane_dispatch_workspace_free_ratio' \
  'control_plane_dispatch_workspace_capacity_warning' \
  'control_plane_dispatch_workspace_credit_gated' \
  'lvm_vg_free_size_bytes' \
  'lvm_vg_total_size_bytes'; do
  grep -Fq "$query" <<<"$rendered" || {
    echo "ERROR: workspace dashboard contract is missing query fragment: $query" >&2
    exit 1
  }
done
# Each organized dashboard is provisioned under its exact stable UID, title,
# and Grafana folder. This replaces the live Grafana search assertion: the
# sidecar loads exactly these ConfigMaps.
identities=$(yq -o=json eval-all '[select(.kind == "ConfigMap" and .metadata.labels.grafana_dashboard == "1" and .metadata.annotations.grafana_folder != null and .metadata.annotations.grafana_folder != "Kubernetes") | {"folder": .metadata.annotations.grafana_folder, "json": (.data | to_entries | .[0].value)}]' - <<<"$rendered" \
  | jq -r '.[] | (.json | fromjson) as $dashboard | "\($dashboard.uid)|\($dashboard.title)|\(.folder)"' | sort)
expected=$(sort <<'IDENTITIES'
iterabase-platform-overview|00 — Platform Overview|Iterabase
iterabase-control-plane|10 — Control Plane|Iterabase
iterabase-execution-runtime|20 — Execution Runtime|Iterabase
iterabase-tool-runtime|30 — Tool Runtime|Iterabase
iterabase-inference-model-serving|40 — Inference and Model Serving|Iterabase
iterabase-data-storage|50 — Data and Storage|Iterabase
iterabase-platform-infrastructure|60 — Platform Infrastructure|Iterabase
iterabase-infrastructure-components|Infrastructure — Data, Edge and GPU|Infrastructure
iterabase-observability-stack|Observability — Metrics, Logs and Alerts|Observability
IDENTITIES
)
if [[ "$identities" != "$expected" ]]; then
  echo "ERROR: organized dashboard uid|title|folder identities differ:" >&2
  diff <(echo "$expected") <(echo "$identities") >&2 || true
  exit 1
fi
# Each dedicated workspace capacity panel has exactly one query carrying its
# own metric, not merely a title and a fragment somewhere in the dashboard.
panels=$(yq -o=json eval-all 'select(.kind == "ConfigMap" and .data["iterabase-data-storage.json"] != null) | .data["iterabase-data-storage.json"]' - <<<"$rendered" \
  | jq -r 'fromjson | .panels[] | select((.targets | length) == 1) | "\(.title)|\(.targets[0].expr)"')
while IFS='|' read -r title fragment; do
  grep -Fq "$title|" <<<"$panels" && grep -F "$title|" <<<"$panels" | grep -Fq "$fragment" || {
    echo "ERROR: 50 — Data and Storage panel '$title' is not a single query over $fragment" >&2
    exit 1
  }
done <<'PANELS'
AgentPool PVC free bytes|control_plane_dispatch_workspace_free_bytes
AgentPool PVC free ratio|control_plane_dispatch_workspace_free_ratio
AgentPool capacity warnings|control_plane_dispatch_workspace_capacity_warning
AgentPool credit gates|control_plane_dispatch_workspace_credit_gated
iterabase-data free bytes|lvm_vg_free_size_bytes
iterabase-data free ratio|lvm_vg_total_size_bytes
PANELS
echo "OK: $labels provisioned dashboards are organized across Kubernetes, Iterabase, Infrastructure, and Observability; stable UID/title/folder identities plus per-pool and aggregate LVM capacity panels are enforced"

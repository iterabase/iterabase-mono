#!/usr/bin/env python3
"""Render the reviewed Iterabase Grafana dashboard suite deterministically."""
from __future__ import annotations

import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "charts" / "observability" / "dashboards"
DS = {"type": "prometheus", "uid": "prometheus"}
NAMESPACE_JOBS = "control-plane|inference-gateway|postgresql|minio|.*dcgm-exporter.*"

DASHBOARDS = [
    ("iterabase-platform-overview", "00 — Platform Overview", [
        ("Healthy targets", 'sum(up{namespace=~"$namespace",job=~"control-plane|inference-gateway|postgresql|minio"})', "short", "stat"),
        ("Targets down", 'sum(up{namespace=~"$namespace",job=~"control-plane|inference-gateway|postgresql|minio"} == 0)', "short", "stat"),
        ("Platform request rate", 'sum(rate(control_plane_http_requests_total{namespace=~"$namespace"}[$__rate_interval])) + sum(rate(gateway_requests_total{namespace=~"$namespace"}[$__rate_interval]))', "reqps", "timeseries"),
        ("Server error rate", 'sum(rate(control_plane_http_requests_total{namespace=~"$namespace",status_class="5xx"}[$__rate_interval])) + sum(rate(gateway_requests_total{namespace=~"$namespace",status_class="5xx"}[$__rate_interval]))', "reqps", "timeseries"),
        ("Pending execution", 'sum(control_plane_dispatch_pending_work{namespace=~"$namespace"})', "short", "stat"),
        ("Firing alerts", 'sum(ALERTS{namespace=~"$namespace",alertstate="firing",alertname=~"Iterabase.*"})', "short", "stat"),
    ]),
    ("iterabase-control-plane", "10 — Control Plane", [
        ("API request rate by route", 'sum by (route) (rate(control_plane_http_requests_total{namespace=~"$namespace",component="api"}[$__rate_interval]))', "reqps", "timeseries"),
        ("API server error ratio", 'sum(rate(control_plane_http_requests_total{namespace=~"$namespace",component="api",status_class="5xx"}[$__rate_interval])) / clamp_min(sum(rate(control_plane_http_requests_total{namespace=~"$namespace",component="api"}[$__rate_interval])), 0.001)', "percentunit", "timeseries"),
        ("API P95 latency", 'histogram_quantile(0.95, sum by (le,route) (rate(control_plane_http_request_duration_seconds_bucket{namespace=~"$namespace",component="api"}[$__rate_interval])))', "s", "timeseries"),
        ("Manager reconciliation errors", 'sum by (controller) (rate(controller_runtime_reconcile_total{namespace=~"$namespace",result="error",component="manager"}[$__rate_interval]))', "ops", "timeseries"),
        ("Manager workqueue depth", 'sum by (name) (workqueue_depth{namespace=~"$namespace",job="control-plane"})', "short", "timeseries"),
        ("Database pool utilization", 'control_plane_database_pool_connections{namespace=~"$namespace",state="total"} / clamp_min(control_plane_database_pool_max_connections{namespace=~"$namespace"}, 1)', "percentunit", "timeseries"),
    ]),
    ("iterabase-execution-runtime", "20 — Execution Runtime", [
        ("Connected workers", 'sum(control_plane_dispatch_worker_connections{namespace=~"$namespace"})', "short", "stat"),
        ("Pending work", 'sum(control_plane_dispatch_pending_work{namespace=~"$namespace"})', "short", "stat"),
        ("Assignment rate", 'sum by (result) (rate(control_plane_dispatch_assignments_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Turn outcomes", 'sum by (outcome,reason) (rate(control_plane_dispatch_turns_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Harness dispatch connectivity", 'sum by (pod) (control_plane_harness_dispatch_connected{namespace=~"$namespace"})', "short", "timeseries"),
        ("Child RPC rate", 'sum by (operation) (rate(control_plane_harness_child_rpc_requests_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Pending event replays", 'sum by (pod) (control_plane_harness_pending_replays{namespace=~"$namespace"})', "short", "timeseries"),
        ("P95 turn duration", 'histogram_quantile(0.95, sum by (le,result) (rate(control_plane_harness_turn_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
    ]),
    ("iterabase-tool-runtime", "30 — Tool Runtime", [
        ("Connected runner streams", 'sum(control_plane_gateway_runner_connections{namespace=~"$namespace"})', "short", "stat"),
        ("Gateway invocation rate", 'sum by (effect_class,result) (rate(control_plane_gateway_invocations_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Unknown outcomes", '(sum(increase(control_plane_gateway_invocations_total{namespace=~"$namespace",result="outcome_unknown"}[$__rate_interval])) or vector(0)) + (sum(control_plane_gateway_recoveries_total{namespace=~"$namespace",result="outcome_unknown"}) or vector(0))', "short", "stat"),
        ("Gateway P95 invocation latency", 'histogram_quantile(0.95, sum by (le,effect_class) (rate(control_plane_gateway_invocation_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
        ("Tool runner gateway connection", 'sum(tool_runner_gateway_connected{namespace=~"$namespace"})', "short", "stat"),
        ("Ready tool generations", 'sum(tool_runner_generation_ready{namespace=~"$namespace"})', "short", "stat"),
        ("Materialization failures", 'sum(increase(tool_runner_materializations_total{namespace=~"$namespace",result="failure"}[$__rate_interval]))', "short", "timeseries"),
        ("Tool invocation P95 latency", 'histogram_quantile(0.95, sum by (le,result) (rate(tool_runner_invocation_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
    ]),
    ("iterabase-inference-model-serving", "40 — Inference and Model Serving", [
        ("Inference request rate", 'sum by (listener,model) (rate(gateway_requests_total{namespace=~"$namespace"}[$__rate_interval]))', "reqps", "timeseries"),
        ("Inference server error ratio", 'sum(rate(gateway_requests_total{namespace=~"$namespace",status_class="5xx"}[$__rate_interval])) / clamp_min(sum(rate(gateway_requests_total{namespace=~"$namespace"}[$__rate_interval])), 0.001)', "percentunit", "timeseries"),
        ("Inference P95 latency", 'histogram_quantile(0.95, sum by (le,model) (rate(gateway_request_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
        ("P95 time to first token", 'histogram_quantile(0.95, sum by (le,model) (rate(gateway_time_to_first_token_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
        ("Active inference streams", 'sum by (model) (gateway_active_streams{namespace=~"$namespace"})', "short", "timeseries"),
        ("Available model backends", 'sum by (model) (gateway_backend_health{namespace=~"$namespace"})', "short", "timeseries"),
        ("vLLM running requests", 'sum by (pod) ({__name__="vllm:num_requests_running",namespace=~"$namespace"})', "short", "timeseries"),
        ("GPU utilization", 'avg by (gpu) (DCGM_FI_DEV_GPU_UTIL{namespace=~"$namespace"})', "percent", "timeseries"),
    ]),
    ("iterabase-data-storage", "50 — Data and Storage", [
        ("PostgreSQL available", 'min(pg_up{namespace=~"$namespace"})', "short", "stat"),
        ("MinIO targets available", 'sum(up{namespace=~"$namespace",job="minio"})', "short", "stat"),
        ("AgentPool PVC free bytes", 'min by (pool) (control_plane_dispatch_workspace_free_bytes{namespace=~"$namespace"})', "bytes", "timeseries"),
        ("AgentPool PVC free ratio", 'min by (pool) (control_plane_dispatch_workspace_free_ratio{namespace=~"$namespace"})', "percentunit", "timeseries"),
        ("AgentPool capacity warnings", 'max by (pool) (control_plane_dispatch_workspace_capacity_warning{namespace=~"$namespace"})', "short", "timeseries"),
        ("AgentPool credit gates", 'max by (pool) (control_plane_dispatch_workspace_credit_gated{namespace=~"$namespace"})', "short", "timeseries"),
        ("iterabase-data free bytes", 'min(lvm_vg_free_size_bytes{namespace=~"$namespace",name="iterabase-data"})', "bytes", "stat"),
        ("iterabase-data free ratio", 'min(lvm_vg_free_size_bytes{namespace=~"$namespace",name="iterabase-data"} / lvm_vg_total_size_bytes{namespace=~"$namespace",name="iterabase-data"})', "percentunit", "stat"),
        ("Persistent volume utilization", 'max by (persistentvolumeclaim) (1 - kubelet_volume_stats_available_bytes{namespace=~"$namespace"} / kubelet_volume_stats_capacity_bytes{namespace=~"$namespace"})', "percentunit", "timeseries"),
        ("PostgreSQL connections", 'sum(pg_stat_activity_count{namespace=~"$namespace"})', "short", "timeseries"),
        ("Inference database pool utilization", 'inference_gateway_database_pool_connections{namespace=~"$namespace"} / clamp_min(inference_gateway_database_pool_max_connections{namespace=~"$namespace"}, 1)', "percentunit", "timeseries"),
    ]),
    ("iterabase-platform-infrastructure", "60 — Platform Infrastructure", [
        ("Platform targets down", 'sum(up{namespace=~"$namespace",job=~"control-plane|inference-gateway|postgresql|minio"} == 0)', "short", "stat"),
        ("Pod restarts", 'sum by (pod,container) (increase(kube_pod_container_status_restarts_total{namespace=~"$namespace"}[$__rate_interval]))', "short", "timeseries"),
        ("Certificate minimum lifetime", 'min(certmanager_certificate_expiration_timestamp_seconds{namespace=~"$namespace"} - time())', "s", "stat"),
        ("Namespace CPU usage", 'sum(rate(container_cpu_usage_seconds_total{namespace=~"$namespace",container!=""}[$__rate_interval]))', "cores", "timeseries"),
        ("Namespace memory working set", 'sum(container_memory_working_set_bytes{namespace=~"$namespace",container!=""})', "bytes", "timeseries"),
        ("Observability targets", 'sum(up{namespace=~"$namespace",job=~"prometheus.*|alertmanager.*|grafana.*|loki.*"})', "short", "stat"),
        ("Prometheus rule evaluation failures", 'sum(rate(prometheus_rule_evaluation_failures_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Firing Iterabase alerts", 'sum by (alertname,severity) (ALERTS{namespace=~"$namespace",alertstate="firing",alertname=~"Iterabase.*"})', "short", "timeseries"),
    ]),
]

AUXILIARY_DASHBOARDS = [
    ("infrastructure", "iterabase-infrastructure-components", "Infrastructure — Data, Edge and GPU", [
        ("PostgreSQL availability", 'min(pg_up{namespace=~"$namespace"})', "short", "stat"),
        ("PostgreSQL transaction rate", 'sum by (datname) (rate(pg_stat_database_xact_commit{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("PostgreSQL deadlocks", 'sum by (datname) (increase(pg_stat_database_deadlocks{namespace=~"$namespace"}[$__rate_interval]))', "short", "timeseries"),
        ("MinIO scrape availability", 'sum(up{namespace=~"$namespace",component="minio"})', "short", "stat"),
        ("Persistent volume utilization", 'max by (persistentvolumeclaim) (1 - kubelet_volume_stats_available_bytes{namespace=~"$namespace"} / kubelet_volume_stats_capacity_bytes{namespace=~"$namespace"})', "percentunit", "timeseries"),
        ("Ingress request rate", 'sum(rate(nginx_ingress_controller_requests{namespace=~"$namespace"}[$__rate_interval]))', "reqps", "timeseries"),
        ("Certificate lifetime", 'min by (name) (certmanager_certificate_expiration_timestamp_seconds{namespace=~"$namespace"} - time())', "s", "timeseries"),
        ("GPU utilization", 'avg by (gpu) (DCGM_FI_DEV_GPU_UTIL{namespace=~"$namespace"})', "percent", "timeseries"),
        ("GPU memory used", 'sum by (gpu) (DCGM_FI_DEV_FB_USED{namespace=~"$namespace"})', "MiB", "timeseries"),
    ]),
    ("observability", "iterabase-observability-stack", "Observability — Metrics, Logs and Alerts", [
        ("Prometheus targets down", 'sum(up{namespace=~"$namespace"} == 0)', "short", "stat"),
        ("Prometheus samples ingested", 'sum(rate(prometheus_tsdb_head_samples_appended_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Prometheus rule failures", 'sum(rate(prometheus_rule_evaluation_failures_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Firing alerts", 'sum by (severity) (ALERTS{namespace=~"$namespace",alertstate="firing"})', "short", "timeseries"),
        ("Alertmanager notifications failed", 'sum(rate(alertmanager_notifications_failed_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Loki request rate", 'sum by (status_code) (rate(loki_request_duration_seconds_count{namespace=~"$namespace"}[$__rate_interval]))', "reqps", "timeseries"),
        ("Loki request P95 latency", 'histogram_quantile(0.95, sum by (le) (rate(loki_request_duration_seconds_bucket{namespace=~"$namespace"}[$__rate_interval])))', "s", "timeseries"),
        ("Promtail dropped entries", 'sum(rate(promtail_dropped_entries_total{namespace=~"$namespace"}[$__rate_interval]))', "ops", "timeseries"),
        ("Grafana availability", 'sum(up{namespace=~"$namespace",job=~".*grafana.*"})', "short", "stat"),
        ("Prometheus storage", 'sum(prometheus_tsdb_storage_blocks_bytes{namespace=~"$namespace"})', "bytes", "timeseries"),
    ]),
]


def panel(pid: int, title: str, expr: str, unit: str, kind: str, y: int, x: int) -> dict:
    return {
        "id": pid, "title": title, "type": kind,
        "datasource": DS,
        "gridPos": {"h": 7, "w": 12, "x": x, "y": y},
        "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
        "options": {"legend": {"displayMode": "table", "placement": "right", "showLegend": kind != "stat"}, "tooltip": {"mode": "multi"}},
        "targets": [{"refId": "A", "expr": expr, "legendFormat": "{{component}} {{model}} {{route}} {{result}} {{pod}}", "range": kind != "stat", "instant": kind == "stat"}],
    }


def render(uid: str, title: str, specs: list[tuple[str, str, str, str]]) -> dict:
    panels = [panel(i + 1, *spec, (i // 2) * 7, (i % 2) * 12) for i, spec in enumerate(specs)]
    links = [{"title": other_title, "type": "link", "url": f"/d/{other_uid}", "targetBlank": False}
             for other_uid, other_title, _ in DASHBOARDS if other_uid != uid]
    return {
        "annotations": {"list": []}, "editable": False, "fiscalYearStartMonth": 0,
        "graphTooltip": 1, "id": None, "links": links, "liveNow": False,
        "panels": panels, "refresh": "30s", "schemaVersion": 39,
        "tags": ["iterabase", "production", "platform"],
        "templating": {"list": [{
            "name": "namespace", "label": "Namespace", "type": "query", "datasource": DS,
            "query": {"query": f'label_values(up{{job=~"{NAMESPACE_JOBS}"}}, namespace)', "refId": "VariableQuery"},
            "includeAll": True, "allValue": ".*", "refresh": 2, "sort": 1,
        }]},
        "time": {"from": "now-6h", "to": "now"}, "timepicker": {},
        "timezone": "browser", "title": title, "uid": uid, "version": 1,
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    expected = {f"{uid}.json": json.dumps(render(uid, title, specs), indent=2, ensure_ascii=False) + "\n"
                for uid, title, specs in DASHBOARDS}
    expected.update({f"{folder}/{uid}.json": json.dumps(render(uid, title, specs), indent=2, ensure_ascii=False) + "\n"
                     for folder, uid, title, specs in AUXILIARY_DASHBOARDS})
    if args.check:
        errors = [name for name, content in expected.items() if not (OUT / name).exists() or (OUT / name).read_text() != content]
        extras = sorted(str(p.relative_to(OUT)) for p in OUT.rglob("iterabase-*.json") if str(p.relative_to(OUT)) not in expected)
        if errors or extras:
            raise SystemExit(f"dashboard generation is stale: changed={errors} extras={extras}")
        return
    OUT.mkdir(parents=True, exist_ok=True)
    for existing in OUT.rglob("iterabase-*.json"):
        existing.unlink()
    for name, content in expected.items():
        path = OUT / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


if __name__ == "__main__":
    main()

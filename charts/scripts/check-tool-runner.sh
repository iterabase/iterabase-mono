#!/usr/bin/env bash
set -euo pipefail
render=$(helm template runner-check charts/control-plane -f values-control-plane-auth.yaml \
  --set postgresql.enabled=false \
  --set gateway.enabled=true \
  --set toolRunner.enabled=true \
  --set toolRunner.metrics.enabled=true \
  --set 'toolRunner.allowedToolNamespaces={platform,graph}')

grep -q 'app.kubernetes.io/component: tool-runner' <<<"$render"
grep -q 'spiffe://iterabase.local/tool-runners/default/overlay-tools' <<<"$render"
flux_rbac=$(awk '/# The materializer polls one exact GitRepository/{p=1} /^apiVersion: cert-manager.io/{p=0} p' <<<"$render")
grep -q '^kind: Role$' <<<"$flux_rbac"
grep -q '^kind: RoleBinding$' <<<"$flux_rbac"
grep -q '^  namespace: "flux-system"$' <<<"$flux_rbac"
grep -q 'resourceNames: \["overlay"\]' <<<"$flux_rbac"
! grep -q '^kind: ClusterRole' <<<"$flux_rbac"
grep -q 'mountPath: /artifacts, readOnly: true' <<<"$render"
grep -q 'name: TOOL_RUNNER_MAX_GENERATIONS' <<<"$render"
grep -q 'name: TOOL_RUNNER_DRAIN_MAX_AGE' <<<"$render"
grep -q 'name: MATERIALIZER_METRICS_PORT' <<<"$render"
grep -q 'name: TOOL_RUNNER_METRICS_PORT' <<<"$render"
grep -q 'kind: ServiceMonitor' <<<"$render"
grep -q 'port: mat-metrics' <<<"$render"
grep -q 'port: run-metrics' <<<"$render"

# The gateway consumes its static runner allow-list only at startup. The pod
# template checksum must change when an operator approves another namespace so
# a normal Helm reconcile rolls the Deployment without Forge special-casing it.
changed_render=$(helm template runner-check charts/control-plane -f values-control-plane-auth.yaml \
  --set postgresql.enabled=false \
  --set gateway.enabled=true \
  --set toolRunner.enabled=true \
  --set toolRunner.metrics.enabled=true \
  --set 'toolRunner.allowedToolNamespaces={platform,graph,client}')
gateway_checksum() {
  sed -n 's/.*checksum\/gateway-config: "\([a-f0-9]\{64\}\)".*/\1/p' <<<"$1"
}
checksum=$(gateway_checksum "$render")
changed_checksum=$(gateway_checksum "$changed_render")
test -n "$checksum"
test -n "$changed_checksum"
test "$checksum" != "$changed_checksum"

# Only materializer receives the projected kube-api mount; only runner receives
# mTLS material. Split the two container blocks for credential-boundary checks.
materializer=$(awk '/- name: materializer/{p=1} /- name: runner/{p=0} p' <<<"$render")
runner=$(awk '/- name: runner/{p=1} /^      volumes:/{p=0} p' <<<"$render")
grep -q 'name: kube-api' <<<"$materializer"
! grep -q 'name: runner-tls' <<<"$materializer"
grep -q 'name: runner-tls' <<<"$runner"
! grep -q 'name: kube-api' <<<"$runner"
grep -q 'rm -f /control/runner-ready; exec node /app/dist/main.js run' <<<"$runner"

grep -q 'name: FLUX_ENABLED, value: "true"' <<<"$materializer"

# Product-only install (DES-HOR-651-01): the product layer ships in the image,
# so disabling the overlay client layer removes every Flux credential surface.
product_only=$(helm template runner-check charts/control-plane -f values-control-plane-auth.yaml \
  --set postgresql.enabled=false \
  --set gateway.enabled=true \
  --set toolRunner.enabled=true \
  --set toolRunner.flux.enabled=false)
grep -q 'app.kubernetes.io/component: tool-runner' <<<"$product_only"
grep -q 'name: FLUX_ENABLED, value: "false"' <<<"$product_only"
! grep -q 'source.toolkit.fluxcd.io' <<<"$product_only"
! grep -q 'name: FLUX_SOURCE_NAME' <<<"$product_only"
! grep -q 'name: kube-api' <<<"$product_only"
! grep -q 'app.kubernetes.io/component: tool-materializer' <<<"$product_only"

echo "OK: tool runner renders with exact SPIFFE/Flux scoping, a Flux-free product-only mode, config-triggered gateway rollout, process-lifetime readiness, bounded generations, read-only artifacts, split credentials, and Prometheus scraping"

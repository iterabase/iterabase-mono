#!/usr/bin/env bash
# C7: the source tree at a release SHA pins the composition. Each component
# chart's appVersion equals its component VERSION file, and its images default
# to that appVersion, so the rendered tags are exactly the component version.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
status=0

app_version() { awk -F'"' '/^appVersion:/ {print $2}' "$root/charts/charts/$1/Chart.yaml"; }

check_link() {
  local chart=$1 component=$2 version app
  version=$(tr -d '[:space:]' < "$root/$component/VERSION")
  app=$(app_version "$chart")
  if [[ "$app" != "$version" ]]; then
    echo "charts/charts/$chart/Chart.yaml appVersion $app != $component/VERSION $version (run: make bump TARGET=$component)" >&2
    status=1
  fi
}

check_rendered_tags() {
  local chart=$1 version=$2 render
  shift 2
  render=$(helm template version-check "$root/charts/charts/$chart" "$@")
  while IFS= read -r image; do
    if [[ "${image##*:}" != "$version" ]]; then
      echo "charts/charts/$chart renders $image, not tag $version" >&2
      status=1
    fi
  done < <(awk '$1 == "image:" {gsub(/"/, "", $2); print $2}' <<<"$render" | grep -E '/(control-plane|control-plane-tool-runner|inference-gateway):')
}

platform=$(awk '/^version:/ {print $2}' "$root/charts/charts/iterabase-platform/Chart.yaml")
for substrate in cert-manager-substrate lvm-storage-substrate; do
  version=$(awk '/^version:/ {print $2}' "$root/charts/charts/$substrate/Chart.yaml")
  if [[ "$version" != "$platform" ]]; then
    # Forge resolves both companions at the platform chart version.
    echo "charts/charts/$substrate version $version != iterabase-platform $platform (run: make bump TARGET=iterabase-platform-chart)" >&2
    status=1
  fi
done

check_link control-plane control-plane
check_link inference-gateway inference-gateway
check_rendered_tags control-plane "$(app_version control-plane)" \
  --set postgresql.enabled=false --set gateway.enabled=true --set dispatch.enabled=true \
  --set dispatch.defaultModel.id=m --set dispatch.defaultModel.api=a --set toolRunner.enabled=true
check_rendered_tags inference-gateway "$(app_version inference-gateway)"

(( status == 0 )) && echo "OK: component chart appVersions equal their VERSION files and images default to them"
exit "$status"

#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
helm template iterabase "$root/charts/iterabase-platform" > "$tmp/default.yaml"
helm template iterabase "$root/charts/iterabase-platform" -f "$root/values-observability.yaml" > "$tmp/observability.yaml"
helm template iterabase "$root/charts/iterabase-platform" \
  --set external-dns.enabled=true --set reloader.enabled=true --set metallb.enabled=true \
  > "$tmp/optional.yaml"
helm template iterabase-cert-manager "$root/charts/cert-manager-substrate" > "$tmp/cert-manager.yaml"
helm template iterabase-lvm-storage "$root/charts/lvm-storage-substrate" \
  --set-string agentpool.authorizedManagerIdentity=system:serviceaccount:iterabase-system:iterabase-control-plane-manager > "$tmp/lvm-storage.yaml"

status=0
while IFS= read -r image; do
  image=${image#\"}; image=${image%\"}; image=${image#\'}; image=${image%\'}
  [[ -z "$image" ]] && continue
  [[ "$image" == ghcr.io/iterabase/* || "$image" == ghcr.io/nunocgoncalves/* ]] && continue
  if [[ "$image" != *@sha256:* ]]; then
    echo "remote runtime image is not digest-pinned: $image" >&2
    status=1
  fi
done < <(awk '$1 == "image:" {print $2}' "$tmp"/*.yaml | sort -u)
(( status == 0 ))
echo "remote runtime image digest contract: ok"

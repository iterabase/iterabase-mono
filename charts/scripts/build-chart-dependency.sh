#!/usr/bin/env bash
set -euo pipefail

chart=${1:?usage: build-chart-dependency.sh CHART_DIR}
repo_root=$(git rev-parse --show-toplevel)
if [[ "$(basename "$chart")" == lvm-storage-substrate ]]; then
  # The generated unpacked dependency contains upstream tag defaults and must
  # not participate in source-authority scanning on a subsequent build.
  rm -rf -- "$chart/charts/lvm-localpv" "$chart/charts"/.lvm-volume-only-*
fi
python3 "$repo_root/.github/scripts/remote_content.py" prepare-chart --chart "$chart"

if [[ "$(basename "$chart")" == lvm-storage-substrate ]]; then
  dependency_dir="$chart/charts"
  archive="$dependency_dir/lvm-localpv-1.10.0.tgz"
  python3 "$repo_root/charts/scripts/prepare-volume-only-lvm-chart.py" "$archive" "$dependency_dir"
  rm -f -- "$archive"
  helm package "$dependency_dir/lvm-localpv" --destination "$dependency_dir" >/dev/null
  rm -rf -- "$dependency_dir/lvm-localpv"
fi

if [[ "$(basename "$chart")" == cert-manager-substrate ]]; then
  # DES-HOR-602-03: size the upstream csi-driver sidecars, which have no
  # resources value, from the checksum-verified archive.
  dependency_dir="$chart/charts"
  archive="$dependency_dir/cert-manager-csi-driver-v0.15.0.tgz"
  python3 "$repo_root/charts/scripts/prepare-sized-csi-driver-chart.py" "$archive" "$dependency_dir"
  rm -f -- "$archive"
  helm package "$dependency_dir/cert-manager-csi-driver" --destination "$dependency_dir" >/dev/null
  rm -rf -- "$dependency_dir/cert-manager-csi-driver"
fi

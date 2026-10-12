#!/usr/bin/env bash
set -euo pipefail

name=${1:?usage: install_ci_tool.sh NAME VERSION}
version=${2:?usage: install_ci_tool.sh NAME VERSION}
repo_root=$(git rev-parse --show-toplevel)
manifest=${TOOLS_MANIFEST:-"$repo_root/.github/inputs/remote-content.json"}
download_dir=${TOOLS_DOWNLOAD_DIR:-"$HOME/.cache/iterabase/ci-tools"}
install_root=${TOOLS_INSTALL_ROOT:-"$RUNNER_TEMP/iterabase-ci-tools"}

case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) platform=linux-amd64 ;;
  Linux-aarch64|Linux-arm64) platform=linux-arm64 ;;
  *) echo "unsupported CI tool platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

python3 "$repo_root/.github/scripts/remote_content.py" validate >/dev/null
count=$(jq --arg name "$name" --arg version "$version" --arg platform "$platform" \
  '[.ci_tools[] | select(.name == $name and .version == $version and .platform == $platform)] | length' "$manifest")
[[ "$count" == 1 ]] || { echo "CI tool identity is missing or ambiguous: $name $version $platform" >&2; exit 1; }
IFS=$'\t' read -r checksum url < <(jq -r --arg name "$name" --arg version "$version" --arg platform "$platform" \
  '.ci_tools[] | select(.name == $name and .version == $version and .platform == $platform) | [.sha256,.url] | @tsv' "$manifest")

mkdir -p "$download_dir" "$install_root"
artifact="$download_dir/$checksum-${url##*/}"
if [[ -f "$artifact" ]] && printf '%s  %s\n' "$checksum" "$artifact" | sha256sum --check --status; then
  source=cache
else
  source=download
  rm -f "$artifact"
  python3 - "$repo_root" "$url" "$checksum" "$artifact" <<'PY'
import importlib.util
from pathlib import Path
import sys
spec = importlib.util.spec_from_file_location("remote_content", Path(sys.argv[1]) / ".github/scripts/remote_content.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.verified_download(sys.argv[2], sys.argv[3], Path(sys.argv[4]))
PY
fi
printf '%s  %s\n' "$checksum" "$artifact" | sha256sum --check --status || {
  rm -f "$artifact"
  echo "checksum verification failed for $name $version ($platform)" >&2
  exit 1
}

echo "ci-tool=$name version=$version platform=$platform source=$source sha256=$checksum"
case "$name" in
  go)
    destination="$install_root/go-$version"
    rm -rf "$destination"
    mkdir -p "$destination"
    tar -xzf "$artifact" -C "$destination"
    test -x "$destination/go/bin/go"
    "$destination/go/bin/go" version | grep -F "go$version " >/dev/null
    echo "GOROOT=$destination/go" >> "$GITHUB_ENV"
    echo "$destination/go/bin" >> "$GITHUB_PATH"
    echo "$HOME/go/bin" >> "$GITHUB_PATH"
    ;;
  node)
    destination="$install_root/node-$version"
    rm -rf "$destination"
    mkdir -p "$destination"
    tar -xJf "$artifact" -C "$destination" --strip-components=1
    test -x "$destination/bin/node"
    [[ $("$destination/bin/node" --version) == "v$version" ]]
    echo "$destination/bin" >> "$GITHUB_PATH"
    ;;
  envtest)
    destination="$install_root/envtest-$version"
    rm -rf "$destination"
    mkdir -p "$destination"
    tar -xzf "$artifact" -C "$destination" --strip-components=2
    for executable in kube-apiserver etcd kubectl; do
      test -x "$destination/$executable"
    done
    echo "KUBEBUILDER_ASSETS=$destination" >> "$GITHUB_ENV"
    ;;
  buildx)
    destination="$HOME/.docker/cli-plugins/docker-buildx"
    mkdir -p "$(dirname "$destination")"
    install -m 0755 "$artifact" "$destination"
    docker buildx version | grep -F "v$version " >/dev/null
    ;;
  goreleaser|syft)
    destination="$install_root/bin"
    mkdir -p "$destination"
    tar -xzf "$artifact" -C "$destination" "$name"
    chmod 0755 "$destination/$name"
    "$destination/$name" --version | grep -F "${version#v}" >/dev/null
    echo "$destination" >> "$GITHUB_PATH"
    ;;
  crane)
    destination="$install_root/bin"
    mkdir -p "$destination"
    tar -xzf "$artifact" -C "$destination" crane
    chmod 0755 "$destination/crane"
    "$destination/crane" version | grep -F "${version#v}" >/dev/null
    echo "$destination" >> "$GITHUB_PATH"
    ;;
  terraform|tflint)
    destination="$install_root/bin"
    mkdir -p "$destination"
    unzip -o -q "$artifact" "$name" -d "$destination"
    chmod 0755 "$destination/$name"
    "$destination/$name" --version | grep -F "$version" >/dev/null
    echo "$destination" >> "$GITHUB_PATH"
    ;;
  *)
    echo "no reviewed installer for CI tool $name" >&2
    exit 1
    ;;
esac

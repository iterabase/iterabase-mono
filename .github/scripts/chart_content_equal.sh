#!/usr/bin/env bash
# Exit 0 when two Helm chart archives hold the same files, byte for byte.
# Helm archives are not byte-reproducible (tar headers carry timestamps), and
# packaged charts embed their dependencies as nested .tgz archives with the same
# property, so both archives are unpacked recursively before comparing trees.
# Prints the differences and exits 1 when the contents differ.
set -euo pipefail

[[ $# -eq 2 && -f "$1" && -f "$2" ]] || { echo "usage: $0 <chart-a.tgz> <chart-b.tgz>" >&2; exit 2; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

unpack() {
  local archive=$1 dest=$2 nested
  mkdir -p "$dest"
  tar -xzf "$archive" -C "$dest"
  while IFS= read -r -d '' nested; do
    unpack "$nested" "${nested%.tgz}.unpacked"
    rm -f "$nested"
  done < <(find "$dest" -type f -name '*.tgz' -print0)
}

unpack "$1" "$work/a"
unpack "$2" "$work/b"
diff -r "$work/a" "$work/b"

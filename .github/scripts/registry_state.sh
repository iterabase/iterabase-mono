#!/usr/bin/env bash
# Print "present" or "absent" for one registry reference. Only a registry
# "not found" answer counts as absent; any other failure (auth, network,
# throttling) exits non-zero, so a publish step never mistakes an outage for a
# missing artifact and pushes over an existing version.
set -uo pipefail

[[ $# -eq 1 ]] || { echo "usage: $0 <registry reference>" >&2; exit 2; }
if output=$(crane manifest "$1" 2>&1); then
  echo present
elif grep -qE 'MANIFEST_UNKNOWN|NAME_UNKNOWN|NOT_FOUND|404 Not Found' <<<"$output"; then
  echo absent
else
  echo "::error::cannot tell whether $1 exists: $output" >&2
  exit 1
fi

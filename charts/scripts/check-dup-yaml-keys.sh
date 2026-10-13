#!/usr/bin/env bash
set -euo pipefail

# Regression guard (HOR-509): kubeconform and most YAML -> JSON pipelines silently
# keep the last-wins value of a duplicate mapping key, so a render bug like the
# inference-gateway workload Certificate's duplicated `app.kubernetes.io/component`
# (helper `gateway` + explicit `gateway-workload`) survives `make check` and only
# surfaces under strict whole-manifest validation. This check parses the complete
# rendered platform chart and FAILS if ANY mapping contains a duplicate key.
chart=charts/iterabase-platform

# Read the program from the heredoc (stdin) but the YAML document from a file
# path argument, so the render data is never consumed as the script source.
unique_yaml() {
  python3 - "$1" <<'PY'
import sys, yaml

class UniqueKeyLoader(yaml.SafeLoader):
    pass

def construct_mapping(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in mapping:
            raise ValueError(
                f"duplicate mapping key {key!r} at line {key_node.start_mark.line + 1}")
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping

UniqueKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, construct_mapping)

with open(sys.argv[1]) as f:
    text = f.read()
try:
    list(yaml.load_all(text, Loader=UniqueKeyLoader))
except Exception as exc:
    print(f"FAIL: {exc}", file=sys.stderr)
    sys.exit(1)
PY
}

render_and_check() {
  local name="$1"; shift
  local render tmp
  render=$(helm template "$name" "$chart" -f values-auth.yaml "$@")
  tmp=$(mktemp)
  printf '%s\n' "$render" > "$tmp"
  printf '== %s: %s resources\n' "$name" "$(grep -c '^kind:' <<<"$render")"
  if ! unique_yaml "$tmp"; then
    echo "error: duplicate YAML mapping key in '$name' full rendered chart" >&2
    rm -f "$tmp"
    exit 1
  fi
  rm -f "$tmp"
}

# Self-test: the guard must reject an intentional duplicate mapping key, proving
# it actually parses the document and can catch the HOR-509 defect class.
self_test=$(mktemp)
printf 'app.kubernetes.io/component: gateway\napp.kubernetes.io/component: gateway-workload\n' > "$self_test"
if unique_yaml "$self_test"; then
  echo "error: duplicate-key guard failed to reject an intentional duplicate" >&2
  rm -f "$self_test"
  exit 1
fi
rm -f "$self_test"

# The workload Certificate only renders when an overlay opts into
# inference-gateway.workload.enabled (OPO1 does), so exercise it explicitly in
# addition to the default, TLS, and observability full-chart presets.
render_and_check iterabase
render_and_check iterabase --set inference-gateway.workload.enabled=true
render_and_check iterabase --namespace portable-system -f values-tls.yaml
render_and_check iterabase --namespace portable-system -f values-observability.yaml

echo "OK: all full platform renders are free of duplicate YAML mapping keys"

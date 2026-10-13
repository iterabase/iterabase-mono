#!/usr/bin/env bash
set -euo pipefail

chart=charts/control-plane
namespace=trust-system
release=workload-trust
other_release=unrelated-trust
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

template="$chart/templates/gateway-tls.yaml"
python3 - "$template" "$chart/templates/dispatch-tls.yaml" <<'PY'
import pathlib
import sys

source = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
legacy = pathlib.Path(sys.argv[2])
required = [
    'lookup "v1" "Secret" .Release.Namespace $caName',
    '$ca = buildCustomCert (index $existingCA.data "tls.crt") (index $existingCA.data "tls.key")',
    'lookup "v1" "Secret" .Release.Namespace $dispatchTLSName',
    'range $k, $v := $existingDispatchTLS.data',
    'genSignedCert $dispatchName nil $dispatchDNSNames 3650 $ca',
]
missing = [snippet for snippet in required if snippet not in source]
if missing:
    raise SystemExit(f"shared workload CA template lost lookup/preservation contracts: {missing}")
if legacy.exists():
    raise SystemExit("dispatch TLS must not return to an independently evaluated template")
PY

helm template "$release" "$chart" -f values-control-plane-auth.yaml --namespace "$namespace" --set dispatch.enabled=true > "$workdir/rendered.yaml"
helm template "$other_release" "$chart" -f values-control-plane-auth.yaml --namespace "$namespace" --set dispatch.enabled=true > "$workdir/unrelated.yaml"

python3 - "$workdir/rendered.yaml" "$workdir" "$release" <<'PY'
import base64
import pathlib
import re
import sys

rendered, output, release = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
documents = rendered.read_text(encoding="utf-8").split("\n---")

def secret(name: str) -> str:
    matches = []
    for document in documents:
        if re.search(r"(?m)^kind: Secret$", document) and re.search(
            rf"(?m)^  name: {re.escape(name)}$", document
        ):
            matches.append(document)
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one rendered Secret {name}, found {len(matches)}")
    return matches[0]

def decode(document: str, key: str, destination: str) -> None:
    match = re.search(rf'(?m)^  {re.escape(key)}: "?([^"\n]+)"?$', document)
    if match is None:
        raise SystemExit(f"rendered Secret lacks {key}")
    (output / destination).write_bytes(base64.b64decode(match.group(1), validate=True))

ca = secret(f"{release}-control-plane-gateway-ca")
dispatch = secret(f"{release}-control-plane-dispatch-tls")
decode(ca, "ca.crt", "ca.pem")
decode(dispatch, "tls.crt", "dispatch.pem")
PY

python3 - "$workdir/unrelated.yaml" "$workdir" "$other_release" <<'PY'
import base64
import pathlib
import re
import sys

rendered, output, release = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
documents = rendered.read_text(encoding="utf-8").split("\n---")
name = f"{release}-control-plane-gateway-ca"
matches = [
    document for document in documents
    if re.search(r"(?m)^kind: Secret$", document)
    and re.search(rf"(?m)^  name: {re.escape(name)}$", document)
]
if len(matches) != 1:
    raise SystemExit(f"expected exactly one rendered Secret {name}, found {len(matches)}")
match = re.search(r'(?m)^  ca\.crt: "?([^"\n]+)"?$', matches[0])
if match is None:
    raise SystemExit("unrelated rendered CA Secret lacks ca.crt")
(output / "unrelated-ca.pem").write_bytes(base64.b64decode(match.group(1), validate=True))
PY

hostname="$release-control-plane-dispatch.$namespace.svc"
wrong_hostname="wrong-dispatch.$namespace.svc"
openssl verify -CAfile "$workdir/ca.pem" -verify_hostname "$hostname" "$workdir/dispatch.pem" >/dev/null
if openssl verify -CAfile "$workdir/ca.pem" -verify_hostname "$wrong_hostname" "$workdir/dispatch.pem" >/dev/null 2>&1; then
  echo "dispatch certificate unexpectedly verifies for wrong server identity $wrong_hostname" >&2
  exit 1
fi
if openssl verify -CAfile "$workdir/unrelated-ca.pem" "$workdir/dispatch.pem" >/dev/null 2>&1; then
  echo "dispatch certificate unexpectedly verifies against an unrelated workload CA" >&2
  exit 1
fi

echo "OK: fresh dispatch serving leaf reuses the shared workload CA and rejects a wrong server identity and unrelated CA"

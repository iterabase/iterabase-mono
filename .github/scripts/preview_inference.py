"""Preview inference proof on the supported workload path (DES-HOR-582-04).

The CI-only fixture mirrors the gateway's workload integration tests: a scoped
identity, pool, running turn and worker/generation assignment. Nothing bypasses
the gateway's certificate or live-assignment checks. A short-lived cert-manager
leaf belongs only to that fixture; cleanup revokes its authority and removes it.
Managed inference reaches the provider through central with the preview's installation certificate (DES-HOR-661-09); no provider credential is ever here.
"""
from __future__ import annotations

import base64
import json
import shlex
import struct
import uuid
import zlib


def smoke_image() -> str:
    """A real 64x64 PNG, for the provider's vision path."""
    raw = b"".join(b"\x00" + b"\x2a\x6f\xdb" * 64 for _ in range(64))

    def chunk(kind: bytes, data: bytes) -> bytes:
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 64, 64, 8, 2, 0, 0, 0))
           + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))
    return "data:image/png;base64," + base64.b64encode(png).decode()


def fixture(model_id: str, namespace: str = "iterabase-system") -> dict[str, str]:
    """Unique, disposable authorization state; no customer identity or API key."""
    identity, pool, run, turn = (str(uuid.uuid4()) for _ in range(4))
    name = f"preview-inference-{pool[:8]}"
    prefix = f"spiffe://iterabase.local/pools/{pool}/"
    model = model_id.replace("'", "''")
    seed = f"""BEGIN;
INSERT INTO identity.identities (id, key, kind, source)
VALUES ('{identity}', 'workflow:{namespace}/{name}', 'workflow', 'local');
INSERT INTO toolgateway.pools (id, key, name, spiffe_id_prefix)
VALUES ('{pool}', '{namespace}/{name}', '{name}', '{prefix}');
INSERT INTO runtime.workflow_runs (id, kind, scope_identity_id, session_id, session_dir, state, started_at)
VALUES ('{run}', 'chat', '{identity}', '{name}', '/tmp/{name}', 'running', now());
INSERT INTO runtime.run_pool_assignments (run_id, pool_id) VALUES ('{run}', '{pool}');
INSERT INTO runtime.turns (id, run_id, session_id, model, state, started_at)
VALUES ('{turn}', '{run}', '{name}', '{model}', 'running', now());
INSERT INTO runtime.turn_assignments
    (turn_id, run_id, pool_id, worker_id, fencing_generation, attempt_id, scope_identity_id, agent_pool_key)
VALUES ('{turn}', '{run}', '{pool}', '{name}', 1, '{run}', '{identity}', '{namespace}/{name}');
NOTIFY permissions_changed;
COMMIT;
"""
    cleanup = f"""BEGIN;
DELETE FROM runtime.workflow_runs WHERE id = '{run}';
DELETE FROM toolgateway.pools WHERE id = '{pool}';
DELETE FROM identity.identities WHERE id = '{identity}';
NOTIFY permissions_changed;
COMMIT;
"""
    certificate = json.dumps({
        "apiVersion": "cert-manager.io/v1", "kind": "Certificate",
        "metadata": {"name": name, "namespace": namespace, "labels": {"iterabase.com/preview-smoke": "inference"}},
        "spec": {"secretName": name, "duration": "1h", "renewBefore": "10m",
                 "issuerRef": {"name": "platform-spiffe-ca", "kind": "ClusterIssuer"},
                 "uris": [prefix + "workers/" + name], "usages": ["digital signature", "client auth"]},
    })
    return {"name": name, "run": run, "turn": turn, "seed": seed, "cleanup": cleanup, "certificate": certificate}


def smoke_script(target: str, inference_host: str, model_id: str,
                 namespace: str = "iterabase-system", release: str = "iterabase") -> str:
    """Host-only curl over verified HTTP/2 mTLS; print statuses, not payloads."""
    scope = fixture(model_id, namespace)
    vision = [{"role": "user", "content": [
        {"type": "text", "text": "Reply with one word: what colour is this image?"},
        {"type": "image_url", "image_url": {"url": smoke_image()}},
    ]}]
    bodies = {
        "text": {"model": model_id, "max_tokens": 16, "messages": [{"role": "user", "content": "Reply with: ok"}]},
        "vision": {"model": model_id, "max_tokens": 32, "messages": vision},
        "vision-stream": {"model": model_id, "max_tokens": 32, "stream": True, "messages": vision},
    }
    files = "".join(f"cat > \"$work/{name}.json\" <<'BODY'\n{json.dumps(body)}\nBODY\n" for name, body in bodies.items())
    dns = f"{release}-gateway.{namespace}.svc"
    customer_url = shlex.quote(f"{target}/v1/chat/completions")
    host_header = shlex.quote(f"Host: {inference_host}")
    return rf"""set -euo pipefail
umask 077
work=$(mktemp -d)
smoke_ok=false
psql() {{ sudo k3s kubectl exec -i -n {namespace} statefulset/{release}-postgresql -c postgresql -- \
  psql -U controlplane -d controlplane -At -v ON_ERROR_STOP=1; }}
cleanup() {{
  status=$?; trap - EXIT; set +e
  psql <<'CLEANUP' >/dev/null
{scope['cleanup']}CLEANUP
  db_status=$?
  sudo k3s kubectl delete -n {namespace} certificate/{scope['name']} secret/{scope['name']} \
    --ignore-not-found --wait=false >/dev/null
  cert_status=$?
  rm -rf "$work"
  file_status=$?
  if [ "$db_status" != 0 ] || [ "$cert_status" != 0 ] || [ "$file_status" != 0 ]; then
    echo 'inference smoke: fixture cleanup failed'; status=1
  elif [ "$status" = 0 ] && [ "$smoke_ok" = true ]; then
    echo 'inference smoke passed: bearer denied; workload mTLS text, vision and vision-stream completed; fencing and cleanup deny'
  fi
  exit "$status"
}}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
trap 'echo "inference smoke: failed at line $LINENO"' ERR
{files}curl --version | grep -q HTTP2 || {{ echo 'inference smoke: host curl needs HTTP/2'; exit 1; }}
# DES-HOR-582-01: the customer path must refuse a bearer, through ingress.
code=$(curl -sS -o "$work/bearer.out" -w '%{{http_code}}' -m 10 \
  -H {host_header} -H 'Authorization: Bearer cp-preview-retired' -H 'Content-Type: application/json' \
  --data-binary "@$work/text.json" {customer_url})
echo "inference smoke: customer bearer -> $code"
[ "$code" = 401 ]
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["error"]["code"] == "invalid_api_key"' "$work/bearer.out"
# CI fixture only: the workload middleware still checks every scope binding.
psql <<'SEED' >/dev/null
{scope['seed']}SEED
sudo k3s kubectl apply -f - <<'CERT' >/dev/null
{scope['certificate']}
CERT
sudo k3s kubectl wait -n {namespace} --for=condition=Ready certificate/{scope['name']} --timeout=180s >/dev/null
sudo k3s kubectl get secret/{scope['name']} -n {namespace} -o json | python3 -c '
import base64,json,pathlib,sys
secret=json.load(sys.stdin)["data"]
for key in ("tls.crt", "tls.key", "ca.crt"):
 pathlib.Path(sys.argv[1], key).write_bytes(base64.b64decode(secret[key], validate=True))' "$work"
ip=$(sudo k3s kubectl get service/{release}-gateway -n {namespace} -o jsonpath='{{.spec.clusterIP}}')
case "$ip" in *:*) ip="[$ip]" ;; esac
post() {{
  curl -sS --http2 --cacert "$work/ca.crt" --cert "$work/tls.crt" --key "$work/tls.key" \
    --resolve "{dns}:8443:$ip" -o "$work/$1.out" -w '%{{http_code}} %{{http_version}}' -m 90 \
    -H 'Content-Type: application/json' -H 'X-Iterabase-Run-Id: {scope['run']}' \
    -H 'X-Iterabase-Turn-Id: {scope['turn']}' -H "X-Iterabase-Fencing-Generation: $2" \
    --data-binary "@$work/$1.json" 'https://{dns}:8443/v1/chat/completions'
}}
# A valid cert is insufficient: a stale generation is denied on the live join.
result=$(post text 2); echo "inference smoke: fenced generation -> $result"
[ "$result" = '403 2' ]
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["error"]["code"] == "turn_not_authorized"' "$work/text.out"
# Completion assertions run once, never as readiness probes. Any transport
# error or unexpected response (including model_not_allowed) fails the smoke.
result=$(post text 1); echo "inference smoke: workload text -> $result"
[ "$result" = '200 2' ]
answered() {{ python3 -c 'import json,sys; r=json.load(open(sys.argv[1])); m=r["choices"][0]["message"]
assert r["usage"]["completion_tokens"] > 0 and any(m.get(k) for k in ("content", "reasoning_content", "reasoning"))' "$work/$1.out"; }}
answered text
result=$(post vision 1); echo "inference smoke: workload vision -> $result"
[ "$result" = '200 2' ]; answered vision
result=$(post vision-stream 1); echo "inference smoke: workload vision-stream -> $result"
[ "$result" = '200 2' ]
python3 -c 'import json,sys
parts=[line[5:].strip() for line in open(sys.argv[1]) if line.startswith("data:")]
assert parts.count("[DONE]") == 1 and parts[-1] == "[DONE]"
chunks=[json.loads(part) for part in parts[:-1]]
assert chunks and all("error" not in chunk for chunk in chunks)
assert any(any(choice.get("delta", {{}}).get(k) for k in ("content", "reasoning_content", "reasoning")) for chunk in chunks for choice in chunk.get("choices", []))' "$work/vision-stream.out"
# Removing the fixture must immediately revoke the still-valid certificate.
psql <<'REVOKE' >/dev/null
{scope['cleanup']}REVOKE
result=$(post text 1); echo "inference smoke: removed assignment -> $result"
[ "$result" = '403 2' ]
python3 -c 'import json,sys; assert json.load(open(sys.argv[1]))["error"]["code"] == "pool_not_authorized"' "$work/text.out"
smoke_ok=true
"""

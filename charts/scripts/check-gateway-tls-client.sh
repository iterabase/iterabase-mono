#!/usr/bin/env bash
set -euo pipefail

# Global internal TLS must make the inference gateway a verifying client of
# its datastore: PostgreSQL with sslmode=verify-full against the mounted
# internal CA root. The live observability-tls
# E2E proves the mounted bytes match the issued root and that the servers reject
# plaintext; this render proves the client configuration that relies on them.
release=gateway-tls
namespace=portable-system
ca_path=/etc/iterabase/internal-ca/ca.crt

rendered=$(helm template "$release" charts/iterabase-platform --namespace "$namespace" -f values-auth.yaml -f values-tls.yaml)
deployment=$(yq eval 'select(.kind == "Deployment" and .metadata.labels."app.kubernetes.io/name" == "inference-gateway")' - <<<"$rendered")
if [[ -z "$deployment" ]]; then
  echo "ERROR: internal-TLS render has no inference-gateway Deployment" >&2
  exit 1
fi

env_value() {
  yq eval ".spec.template.spec.containers[0].env[] | select(.name == \"$1\") | .value" - <<<"$deployment"
}

assert_equal() {
  local description="$1"
  local expected="$2"
  local actual="$3"

  if [[ "$actual" != "$expected" ]]; then
    echo "ERROR: internal-TLS inference gateway $description: expected '$expected', got '${actual:-<missing>}'" >&2
    return 1
  fi
}

database_url=$(env_value DATABASE_URL)
assert_equal 'DATABASE_URL TLS parameters' "sslmode=verify-full&sslrootcert=$ca_path" "${database_url##*\?}"
for name in REDIS_URL REDIS_TLS_CA_FILE; do
  assert_equal "$name (Redis was removed, DES-HOR-661-07)" '' "$(env_value "$name")"
done

mount=$(yq eval '.spec.template.spec.containers[0].volumeMounts[] | select(.mountPath == "'"${ca_path%/*}"'") | .name' - <<<"$deployment")
assert_equal 'internal CA mount volume' 'internal-ca' "$mount"
secret=$(yq eval '.spec.template.spec.volumes[] | select(.name == "internal-ca") | .secret.secretName' - <<<"$deployment")
assert_equal 'internal CA Secret' "$release-internal-ca-root" "$secret"
items=$(yq eval -o=json -I=0 '.spec.template.spec.volumes[] | select(.name == "internal-ca") | .secret.items' - <<<"$deployment")
assert_equal 'internal CA Secret items (never the CA private key)' '[{"key":"ca.crt","path":"ca.crt"}]' "$items"

echo 'OK: global internal TLS renders a verify-full PostgreSQL client against the mounted internal CA root'

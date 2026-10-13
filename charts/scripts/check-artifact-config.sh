#!/usr/bin/env bash
set -euo pipefail

render() {
  helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
    --set gateway.enabled=true \
    --set artifact.enabled=true \
    "$@"
}

assert_max_size() {
  local expected="$1"
  shift
  local rendered values count
  rendered=$(render "$@")
  values=$(awk '
    $1 == "-" && $2 == "name:" && $3 == "ARTIFACT_MAX_SIZE_BYTES" {
      getline
      if ($1 == "value:") {
        gsub(/"/, "", $2)
        print $2
      }
    }
  ' <<<"$rendered")
  count=$(printf '%s\n' "$values" | grep -c . || true)
  if [[ "$count" -ne 2 ]] || grep -qvx "$expected" <<<"$values"; then
    echo "ERROR: expected API + gateway ARTIFACT_MAX_SIZE_BYTES=$expected; rendered: ${values:-<missing>}" >&2
    return 1
  fi
  echo "OK: API + gateway render ARTIFACT_MAX_SIZE_BYTES=$expected as decimal integers"
}

assert_max_size 1073741824
assert_max_size 1048576 --set artifact.maxSizeBytes=1048576

gateway=$(helm template artifact-check charts/control-plane -f values-control-plane-auth.yaml \
  --set gateway.enabled=true \
  --set artifact.enabled=true \
  --show-only templates/gateway.yaml)
grep -q '^kind: Deployment$' <<<"$gateway"
grep -q '^kind: Service$' <<<"$gateway"
grep -q 'command: \["/gateway"\]' <<<"$gateway"
grep -q 'name: artifact-check-minio-artifacts' <<<"$gateway"
echo "OK: gateway Deployment + Service render with the dedicated artifact Secret"

provisioner=$(helm template artifact-check charts/minio \
  --show-only templates/artifact-provisioner.yaml)
grep -q '^kind: Job$' <<<"$provisioner"
grep -Eq '^  name: artifact-check-minio-artifact-provisioner-[0-9]+(-[0-9]+)+$' <<<"$provisioner"
grep -q '^    app.kubernetes.io/managed-by: Helm$' <<<"$provisioner"
grep -Fq 'mc mb --ignore-existing "local/$ARTIFACT_BUCKET"' <<<"$provisioner"
grep -Fq 'mc admin user add local "$ARTIFACT_ACCESS_KEY" "$ARTIFACT_SECRET_KEY"' <<<"$provisioner"
grep -Fq 'mc admin policy create local artifact-service /policy/policy.json' <<<"$provisioner"
grep -Fq 'mc admin policy attach local artifact-service --user "$ARTIFACT_ACCESS_KEY"' <<<"$provisioner"
if grep -q 'ttlSecondsAfterFinished:' <<<"$provisioner"; then
  echo "ERROR: artifact-provisioner Job must remain available to Helm for the release lifecycle" >&2
  exit 1
fi
if grep -q 'helm.sh/hook:' <<<"$provisioner"; then
  echo "ERROR: artifact-provisioner Job must remain an ordinary Helm resource" >&2
  exit 1
fi
echo "OK: MinIO artifact provisioner remains an ordinary retained Job with unchanged idempotent provisioning"

substrate=$(helm template artifact-cert-manager charts/cert-manager-substrate)
grep -q '^kind: CSIDriver$' <<<"$substrate"
grep -q '^  name: csi.cert-manager.io$' <<<"$substrate"
grep -q 'image: "quay.io/jetstack/cert-manager-csi-driver@sha256:49c82164a9b4ae13eb2ab74f57d07efe13edfbf40766975f062bcce48230ce87"' <<<"$substrate"
echo "OK: certificate substrate renders the pinned CSI driver for AgentPool leaves"

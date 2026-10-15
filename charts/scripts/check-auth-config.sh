#!/usr/bin/env bash
# Renders the always-on V2 authentication configuration (HOR-582) and asserts
# its contract: no auth-disabled mode, no legacy JWT or bootstrap key path, the
# shared auth env on the bootstrap init container and the api container, the
# SES and private-relay CA paths, and fail-closed rendering when any
# prerequisite is missing.
set -euo pipefail

chart=charts/control-plane
release=auth-check
values=(-f values-control-plane-auth.yaml)

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

expect_fail() {
  if "$@" >/dev/null 2>&1; then
    fail "expected render failure but it succeeded: $*"
  fi
}

rendered=$(helm template "$release" "$chart" "${values[@]}")

bootstrap_block=$(awk '/- name: bootstrap$/{flag=1} flag{print} /- name: api$/{if(flag){exit}}' <<<"$rendered")
grep -q -- '- bootstrap' <<<"$bootstrap_block" || fail "missing bootstrap init container"
if grep -q -- '--service-account\|--reset' <<<"$bootstrap_block"; then
  fail "the bootstrap init container still passes legacy key-issuance args"
fi

auth_env_count=$(grep -c 'name: AUTH_PUBLIC_ORIGIN' <<<"$rendered")
[[ "$auth_env_count" -eq 2 ]] || fail "expected the auth env on the bootstrap init container and the api container (got ${auth_env_count})"
for retired in AUTH_ENABLED JWT_SIGNING_KEY_PATH JWT_KEY_ID IDENTITY_MODE control-plane-jwt; do
  if grep -q "$retired" <<<"$rendered"; then
    fail "retired legacy configuration $retired is still rendered"
  fi
done
for retired in identitymappings permissionpolicies egressroutes; do
  if grep -q "$retired" <<<"$rendered"; then
    fail "retired CRD $retired is still rendered (CRD or RBAC)"
  fi
done

grep -q "^  name: ${release}-control-plane-auth-bootstrap$" <<<"$rendered" || fail "missing first-Admin Secret"

with_smtp=$(helm template "$release" "$chart" "${values[@]}" \
  --set auth.email.username=relay --set auth.email.existingSecret=relay-smtp)
grep -q 'name: relay-smtp' <<<"$with_smtp" || fail "SMTP credential Secret is not referenced"
grep -q 'key: smtp-password' <<<"$with_smtp" || fail "SMTP credential key is not referenced"

# Hosted installs: Amazon SES SMTP in the configured region with a
# per-environment credential.
ses=$(helm template "$release" "$chart" "${values[@]}" \
  --set auth.email.provider=ses --set auth.email.host= \
  --set auth.email.username=ses-user --set auth.email.existingSecret=ses-smtp)
grep -q 'value: "email-smtp.eu-west-1.amazonaws.com"' <<<"$ses" || fail "SES provider does not derive the regional SMTP host"
grep -q 'name: ses-smtp' <<<"$ses" || fail "SES SMTP credential Secret is not referenced"
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.provider=ses --set auth.email.host=
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.provider=sendgrid

# A private relay CA is mounted and named, never a verification bypass.
with_ca=$(helm template "$release" "$chart" "${values[@]}" --set auth.email.caSecret=relay-ca)
grep -q 'value: "/etc/control-plane/smtp-ca/ca.crt"' <<<"$with_ca" || fail "SMTP CA file is not configured"
grep -q 'secretName: "relay-ca"' <<<"$with_ca" || fail "SMTP CA Secret is not mounted"

# The installation name (DES-HOR-459-08) is an optional api-only seed.
if grep -q 'name: ORGANIZATION_NAME' <<<"$rendered"; then
  fail "an unset organization.name must not render ORGANIZATION_NAME"
fi
named=$(helm template "$release" "$chart" "${values[@]}" --set-string 'organization.name=Exemplo Logística')
[[ "$(grep -c 'name: ORGANIZATION_NAME' <<<"$named")" -eq 1 ]] || fail "organization.name must seed only the api container"
grep -A1 'name: ORGANIZATION_NAME' <<<"$named" | grep -q 'value: "Exemplo Logística"' || fail "organization.name is not passed to the api"

# Every install fails closed when a prerequisite is missing; there is no
# unauthenticated default.
expect_fail helm template "$release" "$chart"
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.publicOrigin=
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.mode=plain
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.publicOrigin=http://app.example.com
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.username=relay
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.host= --set auth.email.provider=smtp
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.email.from=
expect_fail helm template "$release" "$chart" "${values[@]}" --set auth.bootstrap.adminEmail=

echo "OK: always-on authentication configuration, SES, relay CA, organization name seed, and fail-closed prerequisites render correctly"

#!/usr/bin/env bash
set -euo pipefail

repository="${1:-iterabase/iterabase-mono}"
expected_reviewer="${RELEASE_REVIEWER:-nunocgoncalves}"
expected_write_key_title='iterabase protected release tags (validated)'
expected_write_key_public='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBGpAToV5oV2LesN/Kqsim3Nn0OBUItH9TocZOzRd/rz'

fail() {
  printf 'release security audit failed: %s\n' "$*" >&2
  exit 1
}

admin_endpoints="${AUDIT_ADMIN_ENDPOINTS:-true}"
write_key_title="not verified in this invocation"
immutable_release_setting="not verified (admin-only)"
if [[ "$admin_endpoints" == true ]]; then
  keys="$(gh api "repos/$repository/keys")"
  write_key_count="$(jq '[.[] | select(.read_only == false)] | length' <<<"$keys")"
  [[ "$write_key_count" == 1 ]] || fail "expected exactly one write deploy key, found $write_key_count"
  write_key_title="$(jq -r '.[] | select(.read_only == false) | .title' <<<"$keys")"
  write_key_public="$(jq -r '.[] | select(.read_only == false) | .key' <<<"$keys")"
  [[ "$write_key_title" == "$expected_write_key_title" ]] || fail "unexpected write deploy key: $write_key_title"
  [[ "$write_key_public" == "$expected_write_key_public" ]] || fail "write deploy key public identity changed"
elif [[ -n "${RELEASE_TAG_KEY_FILE:-}" ]]; then
  [[ -f "$RELEASE_TAG_KEY_FILE" ]] || fail "release tag key file is missing"
  # GitHub stores deploy keys as algorithm + key material, without comments.
  # Newer ssh-keygen versions preserve the private key's comment in `-y`
  # output, so compare only the two cryptographic identity fields.
  write_key_public="$(ssh-keygen -y -f "$RELEASE_TAG_KEY_FILE" | awk '{print $1 " " $2}')"
  [[ "$write_key_public" == "$expected_write_key_public" ]] || fail "environment release credential is not the reviewed deploy key"
  write_key_title="$expected_write_key_title (environment credential public identity verified)"
fi

environment="$(gh api "repos/$repository/environments/release")"
jq -e --arg reviewer "$expected_reviewer" '
  .name == "release" and
  .deployment_branch_policy.protected_branches == false and
  .deployment_branch_policy.custom_branch_policies == true and
  any(.protection_rules[];
    .type == "required_reviewers" and
    .prevent_self_review == false and
    any(.reviewers[]; .type == "User" and .reviewer.login == $reviewer))
' <<<"$environment" >/dev/null || fail "release environment protection does not match the approved contract"

branches="$(gh api "repos/$repository/environments/release/deployment-branch-policies")"
jq -e '.branch_policies | length == 1 and .[0].name == "master" and .[0].type == "branch"' \
  <<<"$branches" >/dev/null || fail "release environment must allow only the master branch"

rulesets="$(gh api "repos/$repository/rulesets" --paginate | jq -s 'add')"
ruleset_id="$(jq -r '
  [.[] | select(.name == "protected release tags" and .target == "tag" and .enforcement == "active")] |
  if length == 1 then (.[0].id | tostring) else empty end
' <<<"$rulesets")"
[[ "$ruleset_id" =~ ^[0-9]+$ ]] || fail "expected exactly one active protected release tags ruleset"
ruleset="$(gh api "repos/$repository/rulesets/$ruleset_id")"
jq -e --arg ruleset_id "$ruleset_id" '
  (.id | tostring) == $ruleset_id and
  .name == "protected release tags" and
  .target == "tag" and
  .enforcement == "active" and
  ([.rules[].type] | sort) == (["creation", "deletion", "non_fast_forward", "update"] | sort) and
  ([.conditions.ref_name.include[]] | sort) == ([
    "refs/tags/control-plane-v*",
    "refs/tags/inference-gateway-v*",
    "refs/tags/forge-v*",
    "refs/tags/control-plane-*",
    "refs/tags/inference-gateway-*",
    "refs/tags/iterabase-platform-*",
    "refs/tags/dry-run/**"
  ] | sort) and
  .conditions.ref_name.exclude == []
' <<<"$ruleset" >/dev/null || fail "release tag ruleset common contract does not match the approved authority"

if [[ "$admin_endpoints" == true ]]; then
  jq -e '
    (.bypass_actors | type == "array") and
    (.bypass_actors | length == 1) and
    .bypass_actors[0].actor_type == "DeployKey" and
    .bypass_actors[0].bypass_mode == "always"
  ' <<<"$ruleset" >/dev/null || fail "release tag ruleset bypass authority does not match the approved admin contract"

  permissions="$(gh api "repos/$repository/actions/permissions/workflow")"
  jq -e '.default_workflow_permissions == "read" and .can_approve_pull_request_reviews == false' \
    <<<"$permissions" >/dev/null || fail "default workflow token permissions are not read-only"

  if ! immutable="$(gh api "repos/$repository/immutable-releases")"; then
    fail "immutable releases setting is unavailable to the authenticated admin audit"
  fi
  jq -se '
    length == 1 and
    (.[0] | type == "object") and
    (.[0].enabled | type == "boolean") and
    .[0].enabled == true
  ' <<<"$immutable" >/dev/null || fail "immutable releases setting is not exactly enabled"
  immutable_release_setting="enabled"
fi

collaborators="$(gh api --paginate "repos/$repository/collaborators?affiliation=all&per_page=100" | jq -s 'add')"
writers="$(jq -c '[.[] | select(.permissions.admin == true or .permissions.maintain == true or .permissions.push == true) | .login] | unique | sort' <<<"$collaborators")"
[[ "$writers" == '["nunocgoncalves"]' ]] || fail "repository writer set must contain only nunocgoncalves"

if [[ "${AUDIT_REPOSITORY_SECRETS:-true}" == true ]]; then
  environment_secrets="$(gh api "repos/$repository/environments/release/secrets")"
  jq -e 'any(.secrets[]; .name == "RELEASE_TAG_SSH_KEY")' <<<"$environment_secrets" >/dev/null || \
    fail "release environment is missing RELEASE_TAG_SSH_KEY"
  repository_secrets="$(gh api "repos/$repository/actions/secrets")"
  jq -e 'all(.secrets[].name; test("^FORGE_E2E_") | not)' <<<"$repository_secrets" >/dev/null || \
    fail "permanent-fixture secrets remain; decommission them (C1)"
  repository_variables="$(gh api --paginate "repos/$repository/actions/variables?per_page=100" | jq -s '{variables: [.[].variables[]]}')"
  jq -e 'all(.variables[]; (.name | test("^FORGE_E2E_|DIGITALOCEAN|TOKEN|PRIVATE|CREDENTIAL")) | not)' \
    <<<"$repository_variables" >/dev/null || fail "repository variables expose a credential or permanent-fixture authority"
fi

# One publication path (C10): only release.yml may use the protected release
# environment or write repository contents; packages are written only by the
# build-once preview job and the release.
repo_root="$(git rev-parse --show-toplevel)"
workflows="$repo_root/.github/workflows"
[[ "$(grep -l '^    environment: release$' "$workflows"/*.yml)" == "$workflows/release.yml" ]] || \
  fail "only release.yml may use the protected release environment"
[[ "$(grep -l 'contents: write' "$workflows"/*.yml)" == "$workflows/release.yml" ]] || \
  fail "only release.yml may write repository contents"
mapfile -t package_writers < <(grep -l 'packages: write' "$workflows"/*.yml | sort)
expected_package_writers=("$workflows/e2e.yml" "$workflows/full-validation.yml" "$workflows/release.yml")
[[ "${package_writers[*]}" == "${expected_package_writers[*]}" ]] || \
  fail "packages are written outside the build-once preview job and the release: ${package_writers[*]}"
for legacy in release-candidate release-promote release-rehearsal release-rollback fixture-image-cache; do
  [[ ! -e "$workflows/$legacy.yml" ]] || fail "legacy workflow $legacy.yml remains"
done
! grep -q 'pull_request_target:' "$workflows"/*.yml || fail "no workflow may run privileged code for fork pull requests"

printf 'release security audit passed for %s\n' "$repository"
printf 'write deploy key: %s\n' "$write_key_title"
printf 'release ruleset id: %s\n' "$ruleset_id"
printf 'repository writers: %s\n' "$writers"
printf 'immutable releases setting: %s\n' "$immutable_release_setting"

#!/usr/bin/env python3
"""Contract tests for audit_release_security.sh against a fake `gh` on PATH.

The organization switch `deploy_keys_enabled_for_repositories` disables every
deploy key, including the release tag key, so the admin audit must fail closed
unless an owning organization reports it as true (HOR-643)."""

from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / ".github/scripts/audit_release_security.sh"
REPOSITORY = "example/iterabase-mono"

FAKE_GH = """#!/usr/bin/env python3
import json
import os
import subprocess
import sys

repository = "example/iterabase-mono"
args = sys.argv[2:]
endpoint = next(value for value in args if not value.startswith("-"))
with open(os.environ["GH_CALL_LOG"], "a", encoding="utf-8") as log:
    log.write(endpoint + "\\n")
admin_only = {f"repos/{repository}", "orgs/example", f"repos/{repository}/keys",
              f"repos/{repository}/immutable-releases"}
if endpoint in admin_only and os.environ["AUDIT_ADMIN_ENDPOINTS"] != "true":
    print(f"non-admin audit requested admin-only {endpoint}", file=sys.stderr)
    sys.exit(3)
ruleset = {
    "id": 123,
    "name": "protected release tags",
    "target": "tag",
    "enforcement": "active",
    "bypass_actors": [{"actor_type": "DeployKey", "bypass_mode": "always"}],
    "rules": [{"type": value} for value in ("creation", "deletion", "non_fast_forward", "update")],
    "conditions": {"ref_name": {"include": [
        "refs/tags/control-plane-v*",
        "refs/tags/inference-gateway-v*",
        "refs/tags/forge-v*",
        "refs/tags/control-plane-*",
        "refs/tags/inference-gateway-*",
        "refs/tags/iterabase-platform-*",
        "refs/tags/dry-run/**",
    ], "exclude": []}},
}
responses = {
    f"repos/{repository}/keys": [{
        "read_only": False,
        "title": "iterabase protected release tags (validated)",
        "key": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBGpAToV5oV2LesN/Kqsim3Nn0OBUItH9TocZOzRd/rz",
    }],
    f"repos/{repository}/environments/release": {
        "name": "release",
        "deployment_branch_policy": {"protected_branches": False, "custom_branch_policies": True},
        "protection_rules": [{
            "type": "required_reviewers",
            "prevent_self_review": False,
            "reviewers": [{"type": "User", "reviewer": {"login": "nunocgoncalves"}}],
        }],
    },
    f"repos/{repository}/environments/release/deployment-branch-policies": {
        "branch_policies": [{"name": "master", "type": "branch"}],
    },
    f"repos/{repository}/rulesets": [{
        "id": 123, "name": "protected release tags", "target": "tag", "enforcement": "active",
    }],
    f"repos/{repository}/rulesets/123": ruleset,
    f"repos/{repository}/actions/permissions/workflow": {
        "default_workflow_permissions": "read",
        "can_approve_pull_request_reviews": False,
    },
    f"repos/{repository}/immutable-releases": {"enabled": True, "enforced_by_owner": False},
    f"repos/{repository}/collaborators?affiliation=all&per_page=100": [{
        "login": "nunocgoncalves",
        "permissions": {"admin": True, "maintain": True, "push": True},
    }],
}
owner_type = os.environ["FAKE_OWNER_TYPE"]
if owner_type != "lookup-fails":
    responses[f"repos/{repository}"] = {"owner": {"type": owner_type}}
organization = os.environ.get("FAKE_ORG_JSON")
if organization is not None:
    responses["orgs/example"] = json.loads(organization)
if endpoint not in responses:
    print(f"unexpected endpoint: {endpoint}", file=sys.stderr)
    sys.exit(2)
body = json.dumps(responses[endpoint])
if "--jq" in args:
    expression = args[args.index("--jq") + 1]
    body = subprocess.run(["jq", "-r", expression], input=body, text=True,
                          check=True, stdout=subprocess.PIPE).stdout
print(body, end="")
"""


class ReleaseSecurityAuditTests(unittest.TestCase):
    def run_audit(
        self,
        *,
        admin: bool = True,
        owner_type: str = "Organization",
        organization: dict | None = None,
    ) -> tuple[subprocess.CompletedProcess[str], list[str]]:
        with tempfile.TemporaryDirectory() as value:
            directory = Path(value)
            fake_gh = directory / "gh"
            fake_gh.write_text(FAKE_GH, encoding="utf-8")
            fake_gh.chmod(0o755)
            call_log = directory / "calls"
            call_log.touch()
            env = {
                **os.environ,
                "PATH": f"{directory}:{os.environ['PATH']}",
                "GH_CALL_LOG": str(call_log),
                "AUDIT_ADMIN_ENDPOINTS": str(admin).lower(),
                "AUDIT_REPOSITORY_SECRETS": "false",
                "RELEASE_REVIEWER": "nunocgoncalves",
                "FAKE_OWNER_TYPE": owner_type,
            }
            env.pop("RELEASE_TAG_KEY_FILE", None)
            env.pop("FAKE_ORG_JSON", None)
            if organization is not None:
                env["FAKE_ORG_JSON"] = json.dumps(organization)
            completed = subprocess.run(
                [str(AUDIT), REPOSITORY],
                cwd=ROOT,
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            return completed, call_log.read_text(encoding="utf-8").splitlines()

    def test_organization_with_deploy_keys_enabled_passes(self) -> None:
        completed, calls = self.run_audit(
            organization={"deploy_keys_enabled_for_repositories": True}
        )
        self.assertEqual(0, completed.returncode, completed.stderr)
        self.assertIn("org deploy keys enabled: true", completed.stdout)
        self.assertIn("orgs/example", calls)

    def test_organization_without_enabled_deploy_keys_fails(self) -> None:
        for name, organization, shown in (
            ("false", {"deploy_keys_enabled_for_repositories": False}, "false"),
            ("null", {"deploy_keys_enabled_for_repositories": None}, "null"),
            ("missing", {}, "null"),
        ):
            with self.subTest(setting=name):
                completed, _ = self.run_audit(organization=organization)
                self.assertEqual(1, completed.returncode, completed.stdout)
                self.assertIn(
                    "release security audit failed: organization example does not allow "
                    f"deploy keys (deploy_keys_enabled_for_repositories={shown}); "
                    "the release tag key cannot push",
                    completed.stderr,
                )

    def test_user_owned_repository_skips_the_organization_check(self) -> None:
        completed, calls = self.run_audit(owner_type="User")
        self.assertEqual(0, completed.returncode, completed.stderr)
        self.assertIn(
            "org deploy keys enabled: not applicable (user-owned repository)",
            completed.stdout,
        )
        self.assertNotIn("orgs/example", calls)

    def test_failed_owner_lookup_fails_closed(self) -> None:
        completed, calls = self.run_audit(owner_type="lookup-fails")
        self.assertNotEqual(0, completed.returncode, completed.stdout)
        self.assertNotIn("release security audit passed", completed.stdout)
        self.assertNotIn("orgs/example", calls)

    def test_unexpected_owner_type_fails_closed(self) -> None:
        completed, _ = self.run_audit(owner_type="Enterprise")
        self.assertEqual(1, completed.returncode, completed.stdout)
        self.assertIn(
            "unexpected repository owner type: Enterprise", completed.stderr
        )

    def test_non_admin_audit_never_reads_owner_or_organization(self) -> None:
        completed, calls = self.run_audit(admin=False, owner_type="lookup-fails")
        self.assertEqual(0, completed.returncode, completed.stderr)
        self.assertIn(
            "org deploy keys enabled: not verified (admin-only)", completed.stdout
        )
        self.assertNotIn(f"repos/{REPOSITORY}", calls)
        self.assertNotIn("orgs/example", calls)


if __name__ == "__main__":
    unittest.main()

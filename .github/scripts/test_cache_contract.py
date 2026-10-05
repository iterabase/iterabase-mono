#!/usr/bin/env python3

from pathlib import Path
import re
import tempfile
import unittest

from content_digest import content_digest


ROOT = Path(__file__).resolve().parents[2]

# Upstream action.yml runtimes for these immutable refs, reviewed 2026-08-18.
# `aws-actions/configure-aws-credentials` was reviewed on 2026-10-05 (HOR-591):
# its action.yml declares `using: node24`, needs runner v2.327.1 or later, and
# `role-skip-session-tagging` keeps the trust policy to aud+sub only.
REVIEWED_EXTERNAL_ACTION_RUNTIMES = {
    "actions/attest-build-provenance@977bb373ede98d70efdf65b84cb5f73e068dcc2a": "composite",
    "actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9": "node24",
    "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1": "node24",
    "actions/download-artifact@3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c": "node24",
    "actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a": "node24",
    "aws-actions/configure-aws-credentials@e1253824e5c10ff9df46874f81ed3ec929e19cfd": "node24",
    "docker/build-push-action@53b7df96c91f9c12dcc8a07bcb9ccacbed38856a": "node24",
    "docker/login-action@dbcb813823bdd20940b903addbd779551569679f": "node24",
}


class CacheContractTests(unittest.TestCase):
    def test_lock_mutation_changes_content_key(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            lock = Path(directory) / "package-lock.json"
            lock.write_text('{"lockfileVersion":3}\n')
            before = content_digest([lock])
            lock.write_text('{"lockfileVersion":3,"packages":{"x":{}}}\n')
            self.assertNotEqual(before, content_digest([lock]))

    def test_source_mutation_changes_content_identity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            dockerfile = Path(directory) / "Dockerfile"
            source = Path(directory) / "main.go"
            dockerfile.write_text("FROM scratch\nCOPY main /main\n")
            source.write_text("package main\n")
            before = content_digest([dockerfile, source])
            source.write_text('package main\nconst version = "changed"\n')
            self.assertNotEqual(before, content_digest([dockerfile, source]))

    def test_cache_actions_have_no_fallback_restore_keys(self) -> None:
        actions = list((ROOT / ".github/actions").glob("*/action.yml"))
        self.assertTrue(actions)
        for action in actions:
            with self.subTest(action=action):
                self.assertNotIn("restore-keys:", action.read_text())

    def test_checked_go_tool_bin_is_on_subsequent_step_path(self) -> None:
        installer = (ROOT / ".github/scripts/install_ci_tool.sh").read_text()
        self.assertIn('echo "$HOME/go/bin" >> "$GITHUB_PATH"', installer)

    def test_node_cache_accepts_multiple_lockfiles(self) -> None:
        action = (ROOT / ".github/actions/setup-node/action.yml").read_text()
        self.assertIn("DEPENDENCY_PATHS:", action)
        self.assertIn('while IFS= read -r path', action)
        self.assertIn('files+=("$path")', action)
        self.assertIn('"${files[@]}"', action)

    def test_all_workspace_dependency_manifests_define_go_cache(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        for dependency in (
            "control-plane/go.sum",
            "inference-gateway/go.sum",
            "forge/go.sum",
            "forge/test/e2e/go.sum",
            "testkit/e2e/go.mod",
            "control-plane/test/e2e/go.mod",
            "charts/test/e2e/go.mod",
            "charts/test/e2e/go.sum",
            ".github/tools/go.sum",
            ".github/tools/control-plane/go.sum",
        ):
            self.assertIn(dependency, workflow)

    def test_external_actions_are_commit_pinned(self) -> None:
        workflow_files = list((ROOT / ".github").glob("**/*.yml"))
        references = []
        for workflow_file in workflow_files:
            references.extend(
                re.findall(r"uses:\s+([^\s]+)", workflow_file.read_text())
            )
        external = [reference for reference in references if not reference.startswith("./")]
        self.assertTrue(external)
        for reference in external:
            with self.subTest(reference=reference):
                self.assertRegex(reference, r"^[^@]+@[0-9a-f]{40}$")

    def test_external_action_runtimes_are_reviewed(self) -> None:
        workflow_files = list((ROOT / ".github").glob("**/*.yml"))
        references = {
            reference
            for workflow_file in workflow_files
            for reference in re.findall(r"uses:\s+([^\s]+)", workflow_file.read_text())
            if not reference.startswith("./")
        }
        self.assertEqual(references, set(REVIEWED_EXTERNAL_ACTION_RUNTIMES))
        self.assertNotIn("node20", REVIEWED_EXTERNAL_ACTION_RUNTIMES.values())

    def test_forbidden_mutable_state_is_not_cached(self) -> None:
        # Only an action that caches can cache mutable state. Scanning every
        # composite action also matched actions that merely name a credential
        # provider: `setup-aws-ci` references `aws-actions/configure-aws-credentials`
        # (HOR-591) and raises no cache at all.
        cache_actions = "\n".join(
            action.read_text()
            for action in (ROOT / ".github/actions").glob("*/action.yml")
            if "actions/cache@" in action.read_text()
        )
        self.assertTrue(cache_actions)
        for forbidden in (
            ".kube",
            "node_modules",
            "charts/*/charts",
            "postgres",
            "test-results",
            "credentials",
        ):
            with self.subTest(forbidden=forbidden):
                self.assertNotIn(forbidden, cache_actions)


if __name__ == "__main__":
    unittest.main()

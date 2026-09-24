#!/usr/bin/env python3

from __future__ import annotations

import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from collect_changed_paths import collect_changed_paths
from select_ci import OUTPUTS, selection, validate_needs

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / ".github/ci/path-selection-fixtures.json"


class StaticCIPathSelectionTests(unittest.TestCase):
    def test_fixture_matrix(self) -> None:
        fixtures = json.loads(FIXTURES.read_text(encoding="utf-8"))
        for fixture in fixtures:
            with self.subTest(fixture=fixture["name"]):
                result = selection(fixture["paths"])
                self.assertEqual(
                    set(fixture["true"]),
                    {name for name in OUTPUTS if result[name]},
                )
                self.assertEqual(
                    fixture["images"],
                    [image["name"] for image in result["image_matrix"]],
                )
                self.assertEqual(bool(fixture["true"]), result["any"])

    def test_unified_e2e_contract_changes_fan_out_static_owners(self) -> None:
        for path in (
            ".github/scripts/e2e.py",
            ".github/scripts/test_e2e.py",
            ".github/workflows/e2e.yml",
            "testkit/e2e/suite.go",
            "release/targets.json",
        ):
            with self.subTest(path=path):
                result = selection([path])
                if path == "release/targets.json":
                    # Release target authority remains covered by focused release
                    # checks; execution recipes are validated by test_e2e.py.
                    self.assertFalse(result["any"])
                else:
                    self.assertTrue(all(result[name] for name in OUTPUTS))

    def test_only_explicit_nonempty_docs_input_can_be_zero_work(self) -> None:
        self.assertEqual("docs-only", selection(["docs/ci.md"])["classification"])
        self.assertEqual("release-only", selection(["release/targets.json"])["classification"])
        for paths in ([], ["unknown/runtime.input"], ["../outside"], ["./docs/ci.md"]):
            with self.subTest(paths=paths), self.assertRaises(ValueError):
                selection(paths)

    def test_required_gate_rejects_missing_outputs_malformed_matrix_and_status_drift(self) -> None:
        selected = selection(["control-plane/internal/api/server.go"])
        outputs = {
            name: str(bool(selected[name])).lower() for name in OUTPUTS
        }
        outputs.update(
            {
                "image_matrix": json.dumps(selected["image_matrix"], separators=(",", ":")),
                "classification": selected["classification"],
                "selection": json.dumps(selected, sort_keys=True, separators=(",", ":")),
            }
        )
        jobs = {
            "control-plane": True,
            "ui": False,
            "harness": False,
            "tool-runner": False,
            "proto": False,
            "inference-gateway": False,
            "forge": False,
            "forge-fault-matrix": False,
            "charts": False,
            "nested-go-lint": False,
            "images": True,
        }
        needs = {"changes": {"result": "success", "outputs": outputs}}
        needs.update({name: {"result": "success" if value else "skipped"} for name, value in jobs.items()})
        validate_needs(selected, needs)
        for mutation in ("missing", "matrix", "status"):
            broken = json.loads(json.dumps(needs))
            if mutation == "missing":
                del broken["changes"]["outputs"]["forge_real_e2e"]
            elif mutation == "matrix":
                broken["changes"]["outputs"]["image_matrix"] = "not-json"
            else:
                broken["ui"]["result"] = "success"
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                validate_needs(selected, broken)


class ChangedPathCollectionTests(unittest.TestCase):
    def git(self, repo: Path, *args: str) -> str:
        completed = subprocess.run(
            ["git", "-C", str(repo), *args],
            check=True,
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        return completed.stdout.strip()

    def create_repo(self, directory: str) -> tuple[Path, str]:
        repo = Path(directory)
        self.git(repo, "init", "--quiet")
        self.git(repo, "config", "user.email", "ci@example.com")
        self.git(repo, "config", "user.name", "CI Fixture")
        source = repo / "control-plane/internal/api/moved.go"
        source.parent.mkdir(parents=True)
        source.write_text("package api\n", encoding="utf-8")
        self.git(repo, "add", "-A")
        self.git(repo, "commit", "--quiet", "-m", "base")
        return repo, self.git(repo, "rev-parse", "HEAD")

    def test_invalid_event_sha_and_ambiguous_empty_diff_fail(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo, head = self.create_repo(directory)
            for event, base, source in (
                ("unknown", head, head),
                ("pull_request", "short", head),
                ("pull_request", head, "short"),
            ):
                with self.subTest(event=event, base=base, source=source), self.assertRaises((ValueError, subprocess.CalledProcessError)):
                    collect_changed_paths(repo, event, base, source)
            # Equal commits produce a typed empty diff; the selector, not the
            # collector, rejects that ambiguity rather than calling it docs-only.
            select_all, paths = collect_changed_paths(repo, "push", head, head)
            self.assertFalse(select_all)
            self.assertEqual([], paths)
            with self.assertRaises(ValueError):
                selection(paths)

    def test_deletion_only_change_retains_source_owner(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo, base_sha = self.create_repo(directory)
            (repo / "control-plane/internal/api/moved.go").unlink()
            self.git(repo, "add", "-A")
            self.git(repo, "commit", "--quiet", "-m", "delete")
            select_all, paths = collect_changed_paths(
                repo, "push", base_sha, self.git(repo, "rev-parse", "HEAD")
            )
            self.assertFalse(select_all)
            self.assertEqual(["control-plane/internal/api/moved.go"], paths)
            self.assertTrue(selection(paths)["control_plane"])

    def test_cross_owner_move_reports_source_and_destination(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo, base_sha = self.create_repo(directory)
            destination = repo / "forge/internal/moved.go"
            destination.parent.mkdir(parents=True)
            self.git(
                repo,
                "mv",
                "control-plane/internal/api/moved.go",
                "forge/internal/moved.go",
            )
            self.git(repo, "commit", "--quiet", "-m", "move")
            select_all, paths = collect_changed_paths(
                repo,
                "pull_request",
                base_sha,
                self.git(repo, "rev-parse", "HEAD"),
            )
            self.assertFalse(select_all)
            self.assertEqual(
                {
                    "control-plane/internal/api/moved.go",
                    "forge/internal/moved.go",
                },
                set(paths),
            )
            result = selection(paths)
            self.assertTrue(result["control_plane"])
            self.assertTrue(result["forge"])


class WorkflowContractTests(unittest.TestCase):
    def test_ci_and_e2e_share_changed_path_collector_and_stable_contexts(self) -> None:
        workflows = {
            "CI": (ROOT / ".github/workflows/ci.yml").read_text(),
            "E2E": (ROOT / ".github/workflows/e2e.yml").read_text(),
        }
        for name, workflow in workflows.items():
            with self.subTest(workflow=name):
                self.assertIn(
                    "python3 .github/scripts/collect_changed_paths.py", workflow
                )
                self.assertNotIn("git diff --name-only", workflow)
                self.assertIn(f"name: {name} / required", workflow)

    def test_e2e_uses_exact_head_compiled_matrices_and_result_reconciliation(self) -> None:
        workflow = (ROOT / ".github/workflows/e2e.yml").read_text()
        for value in (
            "ref: ${{ github.event.pull_request.head.sha || github.sha }}",
            "test \"$(git rev-parse HEAD)\" = \"$SOURCE_SHA\"",
            "fromJSON(needs.plan.outputs.kind_matrix)",
            "fromJSON(needs.plan.outputs.real_machine_matrix)",
            "python3 .github/scripts/e2e.py compose",
            "python3 .github/scripts/e2e.py validate-results",
            "export FORGE_E2E_REQUIRE_CAPACITY=true",
            "uses: ./.github/actions/setup-permanent-fixture",
            "group: iterabase-permanent-fixture-${{ matrix.capacity }}",
            "queue: max",
        ):
            self.assertIn(value, workflow)
        self.assertNotIn("DIGITALOCEAN_TOKEN", workflow)
        for stale in (
            "schedule:",
            "workflow_dispatch:",
            "complete_catalogue",
            "gpu_policy_red_proof",
            "fixture_cleanup_red_proof",
            "gpu-red-proof:",
            "cleanup-red-proof:",
            "nightly",
            "Exact source",
            "Exact bundle",
            "control-plane-kind:",
            "control-plane-execution-kind:",
            "charts-runtime:",
            "nightly-kind:",
            "nightly-real-machine:",
            "prepare_pr_workspace_runtime.sh",
        ):
            self.assertNotIn(stale, workflow)

    def test_removed_nightly_fixture_is_absent_from_repository(self) -> None:
        self.assertFalse(
            (ROOT / ".github/ci/nightly-selection-fixture.json").exists()
        )

    def test_helm_inputs_use_exact_archive_authority_without_repository_indexes(self) -> None:
        helper = (ROOT / ".github/scripts/add_helm_repositories.sh").read_text()
        builder = (ROOT / "charts/scripts/build-chart-dependency.sh").read_text()
        manifest = json.loads((ROOT / ".github/inputs/remote-content.json").read_text())
        self.assertNotIn("helm repo add", helper)
        self.assertIn("remote_content.py", helper)
        self.assertIn("remote_content.py", builder)
        self.assertGreaterEqual(len(manifest["helm_charts"]), 9)
        self.assertTrue(all(len(item["sha256"]) == 64 for item in manifest["helm_charts"]))

    def test_harness_isolation_static_gate_remains_required(self) -> None:
        root_makefile = (ROOT / "Makefile").read_text()
        workflow = (ROOT / ".github/workflows/ci.yml").read_text()
        harness = workflow.split("  harness:\n", 1)[1].split(
            "\n  tool-runner:\n", 1
        )[0]
        self.assertIn("harness-isolation-test", root_makefile)
        self.assertIn("run: make harness-isolation-test", harness)
        self.assertNotIn("continue-on-error", harness)


if __name__ == "__main__":
    unittest.main()

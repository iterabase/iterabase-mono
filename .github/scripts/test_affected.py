#!/usr/bin/env python3
"""Table tests for affected.py built from real merged pull request diffs."""
from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import affected  # noqa: E402
from affected import Change  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parents[2]
RECIPES = {name: recipe for name, recipe in
           json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))["artifact_recipes"].items()
           if recipe.get("paths")}

CP = ["control-plane-image", "control-plane-chart", "iterabase-platform-chart", "cert-manager-substrate-chart",
      "lvm-storage-substrate-chart"]
CHARTS = CP + ["inference-gateway-image", "inference-gateway-chart"]
FORGE = CHARTS + ["forge-binary", "tool-runner-image", "harness-image"]


def scenario(name: str, tier: str, required: list[str], **extra: object) -> dict:
    return {"id": name, "metadata": {"name": name.split("/")[1], "tier": tier, "required_artifacts": required, **extra}}


# The post-C5 catalogue shape: one smoke scenario per suite, F3 selected by Forge.
CATALOGUE = {"schema_version": 2, "suites": [
    {"suite": {"name": "control-plane", "owner": "control-plane"}, "scenarios": [
        scenario("control-plane/hermetic-example", "F0", []),
        scenario("control-plane/deployed-control-plane", "F2", CP, smoke=True),
        scenario("control-plane/deployed-execution-contracts", "F2",
                 CP + ["harness-image", "tool-runner-image", "inference-gateway-image", "inference-gateway-chart",
                       "runtime-fixture-image"]),
    ]},
    {"suite": {"name": "charts", "owner": "charts"}, "scenarios": [
        scenario("charts/fresh-install", "F2", CHARTS, smoke=True),
        scenario("charts/observability", "F2", CHARTS + ["harness-image", "tool-runner-image"]),
        scenario("charts/observability-tls", "F2", CHARTS + ["harness-image", "tool-runner-image"]),
        scenario("charts/n-1-upgrade", "F2", CHARTS),
    ]},
    {"suite": {"name": "forge", "owner": "forge"}, "scenarios": [
        scenario("forge/cpu", "F3", FORGE, smoke=True, selected_by=["forge-binary"]),
        scenario("forge/cpu-workspace", "F3", FORGE + ["runtime-fixture-image"], selected_by=["forge-binary"]),
        scenario("forge/gpu", "F3", FORGE, selected_by=["forge-binary"]),
    ]},
]}
ALL_RUNNABLE = sorted(s["id"] for suite in CATALOGUE["suites"] for s in suite["scenarios"] if s["metadata"]["tier"] != "F0")
CP_SCENARIOS = ["control-plane/deployed-control-plane", "control-plane/deployed-execution-contracts"]
CHART_SCENARIOS = ["charts/fresh-install", "charts/n-1-upgrade", "charts/observability", "charts/observability-tls"]
FORGE_SCENARIOS = ["forge/cpu", "forge/cpu-workspace", "forge/gpu"]
SMOKE = ["charts/fresh-install", "control-plane/deployed-control-plane", "forge/cpu"]

# Go packages compiled into each binary, as `go list -deps` reports them.
GO_INPUTS = {
    "control-plane-image": {"dirs": {"control-plane/cmd/manager", "control-plane/cmd/api", "control-plane/internal/identity",
                                     "control-plane/internal/server"}, "embeds": {"control-plane/internal/store/migrations/001.sql"}},
    "inference-gateway-image": {"dirs": {"inference-gateway/cmd/gateway"}, "embeds": set()},
    "forge-binary": {"dirs": {"forge/cmd/forge", "forge/internal/cli", "forge/internal/config", "forge/internal/lifecycle",
                              "forge/internal/provisioner", "forge/internal/sshprovisioner"}, "embeds": set()},
}


def run(paths: list[str], **kwargs: object) -> affected.Selection:
    changes = [path if isinstance(path, Change) else Change(path) for path in paths]
    return affected.select(changes, CATALOGUE, RECIPES, GO_INPUTS, kwargs.get("render_changed"),  # type: ignore[arg-type]
                           strict=bool(kwargs.get("strict")))


class RealDiffTests(unittest.TestCase):
    def test_docs_only_selects_nothing(self) -> None:
        # PR 114: architecture records only.
        selection = run(["docs/architecture/foundry-common-spine.md", "docs/architecture/v2-authentication-authority.md"])
        self.assertEqual((selection.classification, selection.jobs, selection.scenarios), ("docs", [], []))

    def test_ci_only_selects_every_job_and_one_smoke_per_suite(self) -> None:
        # PR 131: AWS CI action, scripts and workflows plus a runbook.
        selection = run([".github/actions/setup-aws-ci/action.yml", ".github/scripts/aws_ci.py",
                         ".github/workflows/aws-ci-smoke.yml", "docs/runbooks/aws-ci.md"])
        self.assertEqual(selection.classification, "ci")
        self.assertEqual(selection.jobs, list(affected.ALL_JOBS))
        self.assertEqual(selection.scenarios, SMOKE)
        self.assertEqual(selection.artifacts, [])

    def test_version_file_only_selects_install_readiness(self) -> None:
        # PR 129: control-plane/VERSION bump alone.
        selection = run(["control-plane/VERSION"])
        self.assertEqual(selection.classification, "version")
        self.assertEqual(selection.scenarios, ["charts/fresh-install"])
        self.assertEqual(selection.jobs, ["charts", "ci-contract"])
        self.assertIn("control-plane-image", selection.artifacts)

    def test_linked_version_bump_with_chart_version_lines_is_still_version_only(self) -> None:
        # PR 115: make-bump shaped change across VERSION, Chart.yaml version lines and README.
        selection = run(["charts/README.md", Change("charts/charts/cert-manager-substrate/Chart.yaml", version_only=True),
                         Change("charts/charts/control-plane/Chart.yaml", version_only=True),
                         Change("charts/charts/iterabase-platform/Chart.yaml", version_only=True),
                         Change("charts/charts/lvm-storage-substrate/Chart.yaml", version_only=True),
                         "control-plane/VERSION"])
        self.assertEqual((selection.classification, selection.scenarios), ("version", ["charts/fresh-install"]))

    def test_chart_yaml_with_more_than_version_lines_is_a_chart_change(self) -> None:
        selection = run(["charts/charts/control-plane/Chart.yaml"])
        self.assertEqual(selection.classification, "selected")
        self.assertEqual(selection.scenarios, sorted(CP_SCENARIOS + CHART_SCENARIOS))

    def test_go_package_change_selects_owner_jobs_and_deploying_scenarios_not_forge(self) -> None:
        # PR 122: identity and server packages plus an integration test.
        selection = run(["control-plane/internal/identity/auth_integration_test.go",
                         "control-plane/internal/identity/credentials.go", "control-plane/internal/server/auth_api.go"])
        self.assertEqual(selection.jobs, ["control-plane"])
        self.assertEqual(selection.artifacts, ["control-plane-image"])
        self.assertEqual(selection.scenarios, sorted(CP_SCENARIOS + CHART_SCENARIOS))

    def test_merge_queue_strict_mode_adds_real_machine_scenarios(self) -> None:
        # DES-HOR-590-02: the same PR 122 diff in the merge queue also runs F3.
        paths = ["control-plane/internal/identity/credentials.go", "control-plane/internal/server/auth_api.go"]
        self.assertEqual(run(paths, strict=True).scenarios, sorted(CP_SCENARIOS + CHART_SCENARIOS + FORGE_SCENARIOS))

    def test_strict_mode_still_skips_what_nothing_deploys(self) -> None:
        self.assertEqual(run(["control-plane/internal/identity/auth_integration_test.go"], strict=True).scenarios, [])
        self.assertEqual(run(["control-plane/VERSION"], strict=True).scenarios, ["charts/fresh-install"])

    def test_go_test_only_change_builds_nothing(self) -> None:
        selection = run(["control-plane/internal/identity/auth_integration_test.go"])
        self.assertEqual((selection.jobs, selection.artifacts, selection.scenarios), (["control-plane"], [], []))

    def test_go_package_outside_the_binary_builds_nothing(self) -> None:
        selection = run(["control-plane/internal/devtools/fixture.go"])
        self.assertEqual((selection.artifacts, selection.scenarios), ([], []))

    def test_embedded_file_rebuilds_the_binary(self) -> None:
        self.assertEqual(run(["control-plane/internal/store/migrations/001.sql"]).artifacts, ["control-plane-image"])

    def test_harness_change_selects_harness_job_and_its_scenarios(self) -> None:
        # PR 123: harness TypeScript sources and tests.
        selection = run(["control-plane/harness/package.json", "control-plane/harness/src/child.ts",
                         "control-plane/harness/src/child.test.ts"])
        self.assertEqual(selection.jobs, ["harness"])
        self.assertEqual(selection.artifacts, ["harness-image"])  # not compiled into the Go binaries

    def test_ui_lockfile_rebuilds_the_control_plane_image(self) -> None:
        # PR 121: tool-runner and ui dependency updates.
        selection = run(["control-plane/tool-runner/package.json", "control-plane/ui/package-lock.json"])
        self.assertEqual(selection.jobs, ["tool-runner", "ui"])
        self.assertEqual(selection.artifacts, ["control-plane-image", "tool-runner-image"])

    def test_owner_test_change_selects_that_suite_only(self) -> None:
        # PR 120: Forge E2E module only.
        selection = run(["forge/test/e2e/internal/remotecluster/cluster.go", "forge/test/e2e/workspace_behavior_test.go"])
        self.assertEqual(selection.jobs, ["e2e-modules"])
        self.assertEqual((selection.artifacts, selection.scenarios), ([], FORGE_SCENARIOS))

    def test_forge_change_selects_forge_and_f3(self) -> None:
        # PR 97: Forge inotify reconciliation across cli, lifecycle and E2E.
        selection = run(["forge/VERSION", "forge/internal/cli/apply.go", "forge/internal/lifecycle/lifecycle.go",
                         "forge/test/e2e/host_inotify_test.go"])
        self.assertEqual(selection.jobs, ["charts", "ci-contract", "e2e-modules", "forge"])
        self.assertEqual(selection.artifacts, ["forge-binary"])
        self.assertEqual(selection.scenarios, ["charts/fresh-install"] + FORGE_SCENARIOS)

    def test_module_dependency_bump_rebuilds_every_go_binary(self) -> None:
        # PR 116: go.mod/go.sum across three modules and the Forge E2E module.
        selection = run(["control-plane/go.mod", "control-plane/go.sum", "forge/go.mod", "forge/test/e2e/go.mod",
                         "inference-gateway/go.mod", "control-plane/docs/moby-test-dependency-risk.md"])
        self.assertEqual(selection.artifacts, ["control-plane-image", "forge-binary", "inference-gateway-image"])
        self.assertEqual(selection.scenarios, ALL_RUNNABLE)

    def test_chart_render_diff_narrows_chart_scenarios(self) -> None:
        # PR 110 shape: TLS issuer and substrate templates.
        paths = ["charts/charts/cert-issuers/values.yaml", "charts/charts/cert-manager-substrate/values.yaml"]
        renders = {sid: sid == "charts/observability-tls" for sid in CP_SCENARIOS + CHART_SCENARIOS}
        selection = run(paths, render_changed=renders)
        self.assertEqual(selection.scenarios, ["charts/observability-tls"])
        self.assertEqual(selection.jobs, ["charts"])

    def test_chart_scenarios_without_render_inputs_stay_selected(self) -> None:
        selection = run(["charts/charts/cert-issuers/values.yaml"], render_changed={"charts/fresh-install": False})
        self.assertNotIn("charts/fresh-install", selection.scenarios)
        self.assertIn("charts/observability", selection.scenarios)

    def test_chart_scripts_run_the_chart_job_only(self) -> None:
        selection = run(["charts/scripts/check-certificate-substrate.sh"])
        self.assertEqual((selection.jobs, selection.scenarios), (["charts"], []))

    def test_mixed_ci_and_product_change_is_the_union(self) -> None:
        # PR 112 shape: remote content, workflows and a MinIO chart change.
        selection = run([".github/workflows/e2e.yml", "charts/charts/minio/values.yaml"])
        self.assertEqual(selection.classification, "selected")
        self.assertEqual(selection.jobs, list(affected.ALL_JOBS))
        self.assertEqual(selection.scenarios, sorted(set(SMOKE + CHART_SCENARIOS + CP_SCENARIOS)))

    def test_gpu_driver_inputs_select_the_driver_upgrade_stage(self) -> None:
        self.assertEqual(run([Change(".github/inputs/remote-content.json", driver_input=True)]).stages, ["driver-upgrade"])
        self.assertEqual(run([".github/inputs/remote-content.json"]).stages, [])
        self.assertEqual(run(["forge/internal/gpu/driver.go"]).stages, ["driver-upgrade"])

    def test_unknown_path_selects_everything(self) -> None:
        selection = run(["docs/x.md", "tools/new-thing.sh"])
        self.assertEqual(selection.classification, "all")
        self.assertEqual(selection.scenarios, ALL_RUNNABLE)
        self.assertEqual(selection.jobs, list(affected.ALL_JOBS))
        self.assertEqual(selection.stages, ["driver-upgrade"])

    def test_root_agent_instructions_are_documentation(self) -> None:
        # PR 127: AGENTS.md and dependency docs, plus dependabot config.
        selection = run(["AGENTS.md", "control-plane/docs/moby-test-dependency-risk.md", "docs/dependencies.md"])
        self.assertEqual(selection.classification, "docs")


class VersionLineTests(unittest.TestCase):
    def test_version_line_pattern(self) -> None:
        for line, expected in (('+version: 0.4.7', True), ('-appVersion: "0.0.40"', True), ('+    version: 0.5.5', True),
                               ('+  repository: file://../x', False), ('+description: x', False)):
            with self.subTest(line=line):
                self.assertEqual(bool(affected.VERSION_LINE.match(line)), expected)


if __name__ == "__main__":
    unittest.main()

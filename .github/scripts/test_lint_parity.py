#!/usr/bin/env python3
"""Enforce one explicit PR and release-candidate lint owner per root lint module.

The root Makefile `GO_MODULES` variable is the canonical local lint matrix.
Adding a module there without a named, selectable pull-request and
release-candidate lint owner, or wiring an owner that the required aggregate
does not consume, must fail here instead of merging silently.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import subprocess
import unittest

import release
from select_ci import NESTED_GO_LINT_MODULES, OUTPUTS, selection

ROOT = Path(__file__).resolve().parents[2]
MANIFEST = ROOT / ".github/ci/go-lint-owners.json"
MAKEFILE = ROOT / "Makefile"
JOB_HEADING = re.compile(r"^  ([a-z][a-z0-9-]*):\s*$", re.MULTILINE)
STEP_HEADING = re.compile(r"^      - ", re.MULTILINE)
NEEDS_ENTRY = re.compile(r"^      - ([a-z0-9-]+)$", re.MULTILINE)
# `run:` may be inline, literal (`|`), or folded (`>-`) before the make command.
MAKE_LINT = re.compile(
    r"^[ \t]*run:[ \t]*(?:[>|]-?[ \t]*\n)?[ \t]*make[^\n]*\blint\b",
    re.MULTILINE,
)
CANDIDATE_SUITES = ("control-plane", "inference-gateway", "forge", "charts")
CONFIG_NAMES = (".golangci.yml", ".golangci.yaml")
# Git pathspecs with a leading wildcard match the name at any depth.
CONFIG_PATTERNS = tuple(f"*{name}" for name in CONFIG_NAMES)
# The exclusion block this repository uses: `linters.exclusions.paths` under a
# two-space `linters:` mapping with four-space keys.
EXCLUSION_PATHS = re.compile(
    r"^  exclusions:\n(?:    [^\n]*\n)*?    paths:\n((?:      - [^\n]*\n)+)", re.MULTILINE
)
REPOSITORY_TICKET = re.compile(r"^[A-Z][A-Z0-9]+-\d+$")
SEPARATOR = chr(92)


def root_lint_modules() -> list[str]:
    match = re.search(r"^GO_MODULES\s*:=\s*(.+)$", MAKEFILE.read_text(encoding="utf-8"), re.MULTILINE)
    if match is None:
        raise AssertionError("the root Makefile must declare GO_MODULES")
    return match.group(1).split()


def target_recipe(makefile: Path, target: str) -> str:
    if not makefile.is_file():
        return ""
    match = re.search(
        rf"^{re.escape(target)}:[^\n]*\n((?:[ \t][^\n]*\n|\n)*)",
        makefile.read_text(encoding="utf-8"),
        re.MULTILINE,
    )
    return match.group(1) if match else ""


def tracked(*patterns: str) -> list[str]:
    completed = subprocess.run(
        ["git", "-C", str(ROOT), "ls-files", *patterns],
        check=True,
        text=True,
        stdout=subprocess.PIPE,
    )
    return [line for line in completed.stdout.splitlines() if line]


def yaml_scalar(value: str) -> str:
    """Unescape the YAML scalar forms this repository uses for exclusion patterns."""
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] == '"':
        return value[1:-1].replace(SEPARATOR * 2, SEPARATOR)
    if len(value) >= 2 and value[0] == value[-1] == "'":
        return value[1:-1].replace("''", "'")
    return value


def resolved_lint_config(module: str) -> str | None:
    """The repository-relative configuration golangci-lint resolves for a module."""
    path = ROOT / module
    for parent in [path, *path.parents]:
        for name in CONFIG_NAMES:
            candidate = parent / name
            if candidate.is_file():
                return str(candidate.relative_to(ROOT))
        if parent == ROOT:
            break
    return None


def excluded_sources(module: str, config: str | None) -> list[str]:
    """Module Go files the resolved configuration's path exclusions discard."""
    if config is None:
        return []
    config_path = ROOT / config
    match = EXCLUSION_PATHS.search(config_path.read_text(encoding="utf-8"))
    patterns = (
        [yaml_scalar(line.strip()[2:]) for line in match.group(1).splitlines()] if match else []
    )
    return sorted(
        str(path.relative_to(ROOT))
        for path in (ROOT / module).rglob("*.go")
        if any(re.search(pattern, str(path.relative_to(config_path.parent))) for pattern in patterns)
    )


def workflow_jobs(path: Path) -> dict[str, str]:
    text = path.read_text(encoding="utf-8")
    headings = [(match.group(1), match.start()) for match in JOB_HEADING.finditer(text)]
    jobs: dict[str, str] = {}
    for index, (name, start) in enumerate(headings):
        end = headings[index + 1][1] if index + 1 < len(headings) else len(text)
        jobs[name] = text[start:end]
    return jobs


def job_steps(job: str) -> list[str]:
    steps = []
    for step in STEP_HEADING.split(job)[1:]:
        if not step.endswith("\n"):
            step += "\n"
        steps.append(step)
    return steps


class GoLintOwnerContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.manifest = json.loads(MANIFEST.read_text(encoding="utf-8"))
        cls.entries = cls.manifest["modules"]
        cls.linter = cls.manifest["linter"]
        cls.pr_jobs = workflow_jobs(ROOT / cls.manifest["aggregates"]["pull_request"]["workflow"])
        cls.candidate_jobs = workflow_jobs(
            ROOT / cls.manifest["aggregates"]["release_candidate"]["workflow"]
        )

    def assert_module_lint(self, job: str, module: str) -> None:
        """The owner job must lint exactly this module with the pinned linter."""
        command = re.escape(self.linter["module_command"])
        target = re.escape(module)
        steps = job_steps(job)
        if any(
            re.search(rf"^[ \t]*run: {command}\s*$", step, re.MULTILINE)
            and re.search(rf"^[ \t]*working-directory: {target}\s*$", step, re.MULTILINE)
            for step in steps
        ):
            return
        recipe = target_recipe(ROOT / module / "Makefile", "lint")
        if "golangci" in recipe.lower() and "run ./..." in recipe:
            if any(
                MAKE_LINT.search(step)
                and re.search(rf"^[ \t]*working-directory: {target}\s*$", step, re.MULTILINE)
                for step in steps
            ):
                return
        self.fail(f"{module} owner job has no module-scoped pinned lint invocation")

    def test_manifest_records_one_reviewed_owner_per_module(self) -> None:
        self.assertEqual(1, self.manifest["schema_version"])
        self.assertEqual("golangci-lint", self.linter["tool"])
        self.assertEqual(".github/scripts/install_go_tool.sh golangci-lint", self.linter["install"])
        self.assertEqual("golangci-lint run ./...", self.linter["module_command"])
        for name, aggregate in self.manifest["aggregates"].items():
            with self.subTest(aggregate=name):
                self.assertTrue((ROOT / aggregate["workflow"]).is_file())
                self.assertTrue(aggregate["job"])
                self.assertTrue(aggregate["context"])
        modules = [entry["module"] for entry in self.entries]
        self.assertEqual(len(modules), len(set(modules)))
        for entry in self.entries:
            with self.subTest(module=entry["module"]):
                expected = {
                    "module",
                    "pr_job",
                    "pr_selection",
                    "candidate_job",
                    "candidate_suite",
                }
                if entry["pr_job"] == "nested-go-lint":
                    expected |= {"lint_config", "analysis_waiver"}
                self.assertEqual(expected, set(entry))
                self.assertIn(entry["pr_job"], self.pr_jobs)
                self.assertIn(entry["candidate_job"], self.candidate_jobs)
                self.assertIn(entry["pr_selection"], OUTPUTS)

    def test_pinned_linter_is_version_locked_and_matches_the_local_matrix(self) -> None:
        tools = (ROOT / ".github/tools/go.mod").read_text(encoding="utf-8")
        pinned = re.search(r"github\.com/golangci/golangci-lint/v2 v(\d+\.\d+\.\d+)", tools)
        self.assertIsNotNone(pinned, ".github/tools/go.mod must pin the CI linter version")
        installer = (ROOT / ".github/scripts/install_go_tool.sh").read_text(encoding="utf-8")
        self.assertIn("golangci-lint) module_dir=.github/tools", installer)
        self.assertIn("install -mod=readonly", installer)
        control_plane = (ROOT / "control-plane/Makefile").read_text(encoding="utf-8")
        self.assertIn(f"GOLANGCI_LINT_VERSION ?= v{pinned.group(1)}", control_plane)

    def test_every_root_lint_module_has_a_named_pr_and_candidate_owner(self) -> None:
        self.assertEqual(
            set(root_lint_modules()),
            {entry["module"] for entry in self.entries},
        )

    def test_root_lint_target_covers_every_declared_module(self) -> None:
        lint = target_recipe(MAKEFILE, "lint")
        self.assertTrue(lint, "the root Makefile must define a lint target")
        for module in root_lint_modules():
            with self.subTest(module=module):
                self.assertIn(module, lint)

    def test_nested_module_owner_set_matches_the_selector(self) -> None:
        nested = {entry["module"] for entry in self.entries if entry["pr_job"] == "nested-go-lint"}
        self.assertEqual(set(NESTED_GO_LINT_MODULES), nested)

    def test_pr_owner_jobs_install_the_pinned_linter_and_lint_their_module(self) -> None:
        for entry in self.entries:
            with self.subTest(module=entry["module"]):
                job = self.pr_jobs[entry["pr_job"]]
                self.assertIn(self.linter["install"], job)
                self.assert_module_lint(job, entry["module"])

    def test_pr_aggregate_and_selection_require_every_pr_owner(self) -> None:
        aggregate = self.manifest["aggregates"]["pull_request"]
        job = self.pr_jobs[aggregate["job"]]
        self.assertIn(f"name: {aggregate['context']}", job)
        needs = set(NEEDS_ENTRY.findall(job))
        for entry in self.entries:
            with self.subTest(module=entry["module"]):
                self.assertIn(entry["pr_job"], needs)
                self.assertTrue(selection([f"{entry['module']}/lint-probe.go"])[entry["pr_selection"]])

    def test_every_linter_configuration_selects_the_nested_lint_owner(self) -> None:
        """Config inheritance means any linter configuration can govern a nested module."""
        configs = tracked(*CONFIG_PATTERNS)
        self.assertTrue(configs, "the repository must keep reviewed linter configurations")
        for config in configs:
            with self.subTest(config=config):
                self.assertTrue(selection([config])["nested_go_lint"])

    def test_nested_module_analysis_scope_is_explicit_and_truthful(self) -> None:
        """A gate that analyzes nothing must be recorded as a tracked waiver."""
        documentation = (ROOT / "docs/ci.md").read_text(encoding="utf-8")
        nested = [entry for entry in self.entries if entry["pr_job"] == "nested-go-lint"]
        self.assertTrue(nested, "the nested Go lint owner must cover at least one module")
        for entry in nested:
            with self.subTest(module=entry["module"]):
                resolved = resolved_lint_config(entry["module"])
                self.assertEqual(resolved, entry["lint_config"])
                excluded = excluded_sources(entry["module"], resolved)
                waiver = entry["analysis_waiver"]
                if excluded:
                    self.assertRegex(waiver or "", REPOSITORY_TICKET)
                    self.assertTrue(
                        waiver in documentation,
                        f"{entry['module']} analysis waiver {waiver} must be disclosed in docs/ci.md",
                    )
                else:
                    self.assertIsNone(waiver)

    def test_shared_contract_changes_keep_every_nested_lint_owner_selected(self) -> None:
        """The reviewed contract set: ownership and toolchain authority, plus the
        selector, the PR workflow, root workspace inputs, and the shared testkit.
        Per-configuration coverage is derived in
        test_every_linter_configuration_selects_the_nested_lint_owner."""
        for path in (
            ".github/workflows/ci.yml",
            ".github/ci/go-lint-owners.json",
            ".github/scripts/install_go_tool.sh",
            ".github/scripts/select_ci.py",
            ".github/scripts/test_lint_parity.py",
            ".github/tools/go.mod",
            "Makefile",
            "go.work",
            "testkit/e2e/suite.go",
        ):
            with self.subTest(path=path):
                self.assertTrue(selection([path])["nested_go_lint"])

    def test_candidate_owner_jobs_lint_the_exact_requested_source(self) -> None:
        for entry in self.entries:
            with self.subTest(module=entry["module"]):
                job = self.candidate_jobs[entry["candidate_job"]]
                self.assertIn(self.linter["install"], job)
                self.assertIn('with: {ref: "${{ inputs.master_sha }}"}', job)
                self.assert_module_lint(job, entry["module"])

    def test_candidate_aggregate_and_selection_require_every_owner(self) -> None:
        aggregate = self.manifest["aggregates"]["release_candidate"]
        needs = set(NEEDS_ENTRY.findall(self.candidate_jobs[aggregate["job"]]))
        plan = {
            "source_suites": list(CANDIDATE_SUITES),
            "image_matrix": [],
            "chart_matrix": [],
            "kind_matrix": [],
            "real_machine_matrix": [],
            "execution_plan": {"artifact_build_matrix": []},
            "forge": False,
            "chart_runtime": False,
            "real_machine": False,
        }
        selected = release.candidate_job_selection(plan)
        self.assertEqual(needs, set(selected))
        for entry in self.entries:
            with self.subTest(module=entry["module"]):
                self.assertIn(entry["candidate_job"], needs)
                self.assertTrue(selected[entry["candidate_job"]])
        for suite in CANDIDATE_SUITES:
            suite_selected = release.candidate_job_selection({**plan, "source_suites": [suite]})
            for entry in self.entries:
                with self.subTest(suite=suite, module=entry["module"]):
                    self.assertEqual(
                        entry["candidate_suite"] in (None, suite),
                        suite_selected[entry["candidate_job"]],
                    )


if __name__ == "__main__":
    unittest.main()

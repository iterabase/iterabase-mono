#!/usr/bin/env python3
"""Deterministic affected-graph selection for CI jobs and E2E scenarios (C2).

Changed files map to artifacts through the release/targets.json recipe paths,
refined for Go binaries by `go list -deps -json`; artifacts map to scenarios
through the compiled E2E catalogue. The rules, applied in order:

  1. documentation-only changes select nothing;
  2. CI, testkit and release-contract changes select every CI job and one smoke
     scenario per suite;
  3. a change that only moves version fields selects the install-readiness smoke;
  4. a chart change selects the scenarios whose rendered manifests changed;
  5. GPU driver inputs select the optional driver-upgrade stage;
  6. any path without an owner selects everything.

Pull requests honour each scenario's compiled `selected_by` artifacts; the merge
queue and the `e2e-real-machine` label run strict, selecting a scenario whenever
any artifact it deploys changed (DES-HOR-590-02).

Rules 2-5 union with the ordinary owner selection of any other changed path.
`select()` is pure; the command-line entry point gathers its inputs from git,
go and helm. The output is one JSON document consumed by ci.yml and e2e.yml.
"""
from __future__ import annotations

import argparse
import dataclasses
import fnmatch
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
from typing import Any, Iterable

ROOT = pathlib.Path(__file__).resolve().parents[2]
RUNNABLE_TIERS = {"F2", "F3"}
DRIVER_UPGRADE_STAGE = "driver-upgrade"

# CI jobs in ci.yml, keyed by the paths that select them. First match wins per
# rule list; a path may select several jobs.
JOB_RULES: list[tuple[str, tuple[str, ...]]] = [
    ("control-plane/ui/**", ("ui",)),
    ("control-plane/harness/**", ("harness",)),
    ("control-plane/tool-runner/**", ("tool-runner",)),
    ("control-plane/proto/**", ("proto", "control-plane", "harness", "tool-runner")),
    ("control-plane/buf.*", ("proto", "control-plane", "harness", "tool-runner")),
    ("control-plane/test/e2e/**", ("e2e-modules",)),
    ("control-plane/**", ("control-plane",)),
    ("inference-gateway/**", ("inference-gateway",)),
    ("forge/test/e2e/**", ("e2e-modules",)),
    ("forge/**", ("forge",)),
    ("charts/test/e2e/**", ("e2e-modules",)),
    ("charts/**", ("charts",)),
    ("testkit/**", ("e2e-modules", "ci-contract")),
    ("release/**", ("ci-contract",)),
    (".github/**", ("ci-contract",)),
]
ALL_JOBS = tuple(sorted({job for _, jobs in JOB_RULES for job in jobs}))

DOC_PATTERNS = ("*.md", "docs/**", "**/docs/**", "LICENSE", "**/LICENSE")
CI_PATTERNS = (".github/**", ".githooks/**", "testkit/**", "release/**", "Makefile", "go.work", "go.work.sum",
               ".golangci.yml", ".golangci.yaml", ".gitignore", ".gitattributes")
OWNED_PREFIXES = ("control-plane/", "inference-gateway/", "forge/", "charts/", "testkit/", "release/", "docs/",
                  ".github/", ".githooks/")
VERSION_FILES = ("control-plane/VERSION", "inference-gateway/VERSION", "forge/VERSION")
# Inputs that change what the GPU driver-upgrade stage proves (rule 5).
DRIVER_INPUT_PATTERNS = ("forge/internal/gpu/**", "forge/internal/config/gpu*.go", "forge/test/e2e/gpu_upgrade_test.go")
DRIVER_REMOTE_CONTENT = re.compile(r"nvcr\.io/nvidia/driver|gpu-operator")
OWNER_TEST_PREFIX = re.compile(r"^([^/]+)/test/e2e/")


@dataclasses.dataclass(frozen=True)
class Change:
    """One changed path and the facts about its diff that the rules need."""
    path: str
    version_only: bool = False  # only version/appVersion/dependency-version lines changed
    driver_input: bool = False  # a remote-content change touching GPU driver inputs


@dataclasses.dataclass
class Selection:
    classification: str
    jobs: list[str]
    artifacts: list[str]
    scenarios: list[str]
    stages: list[str]
    reasons: dict[str, list[str]]

    def as_json(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


def matches(path: str, patterns: Iterable[str]) -> bool:
    return any(fnmatch.fnmatchcase(path, pattern) for pattern in patterns)


def is_doc(path: str) -> bool:
    return matches(path, DOC_PATTERNS)


def is_owned(path: str) -> bool:
    return path.startswith(OWNED_PREFIXES) or matches(path, CI_PATTERNS) or matches(path, DOC_PATTERNS)


def jobs_for(path: str) -> set[str]:
    selected: set[str] = set()
    for pattern, jobs in JOB_RULES:
        if fnmatch.fnmatchcase(path, pattern):
            selected.update(jobs)
            # Component subtrees are exclusive of their parent component job.
            if pattern.endswith("/**") and pattern.count("/") > 1:
                break
    return selected


def runnable(scenario: dict[str, Any]) -> bool:
    meta = scenario["metadata"]
    return meta.get("tier") in RUNNABLE_TIERS and not meta.get("production_only", False)


def scenarios_of(catalogue: dict[str, Any]) -> list[tuple[str, dict[str, Any]]]:
    """(owner, scenario) for every runnable scenario, in catalogue order."""
    return [(suite["suite"]["owner"], scenario)
            for suite in catalogue["suites"] for scenario in suite["scenarios"] if runnable(scenario)]


def artifact_affected(recipe: dict[str, Any], path: str, go_inputs: dict[str, set[str]] | None) -> bool:
    """Whether a changed path changes what this recipe builds."""
    if not matches(path, recipe.get("paths", ())) or is_doc(path):
        return False
    build = recipe.get("go_build")
    if not build:
        return True
    if matches(path, build.get("inputs", ())):
        return True
    if go_inputs is None:
        return True  # no dependency graph: stay conservative
    if path.endswith(".go"):
        return not path.endswith("_test.go") and os.path.dirname(path) in go_inputs["dirs"]
    return path in go_inputs["embeds"]


def select(
    changes: list[Change],
    catalogue: dict[str, Any],
    recipes: dict[str, dict[str, Any]],
    go_inputs: dict[str, dict[str, set[str]]] | None = None,
    render_changed: dict[str, bool] | None = None,
    strict: bool = False,
) -> Selection:
    """Select CI jobs, affected artifacts, E2E scenarios and optional stages."""
    go_inputs = go_inputs or {}
    render_changed = render_changed or {}
    reasons: dict[str, list[str]] = {}
    all_scenarios = scenarios_of(catalogue)

    def because(key: str, why: str) -> None:
        reasons.setdefault(key, []).append(why)

    relevant = [change for change in changes if not is_doc(change.path)]
    if not relevant:
        return Selection("docs" if changes else "none", [], [], [], [], {})

    unowned = [change.path for change in relevant if not is_owned(change.path)]
    if unowned:
        for path in unowned:
            because("all", f"{path} has no owner")
        return Selection("all", list(ALL_JOBS), sorted(recipes), sorted(scenario["id"] for _, scenario in all_scenarios),
                         [DRIVER_UPGRADE_STAGE], reasons)

    jobs: set[str] = set()
    version_artifacts: set[str] = set()
    product_artifacts: set[str] = set()
    scenarios: set[str] = set()
    stages: set[str] = set()

    ci_changes = [change for change in relevant if matches(change.path, CI_PATTERNS)]
    version_changes = [change for change in relevant if change not in ci_changes and (
        change.path in VERSION_FILES or (change.version_only and change.path.endswith("Chart.yaml")))]
    product_changes = [change for change in relevant if change not in ci_changes and change not in version_changes]

    if ci_changes:  # rule 2
        jobs.update(ALL_JOBS)
        for _, scenario in all_scenarios:
            if scenario["metadata"].get("smoke"):
                scenarios.add(scenario["id"])
                because(scenario["id"], "suite smoke for a CI change")
        for change in ci_changes:
            because("ci", change.path)
    if version_changes:  # rule 3
        jobs.update({"charts", "ci-contract"})
        for _, scenario in all_scenarios:
            if scenario["metadata"].get("smoke") and scenario["id"].startswith("charts/"):
                scenarios.add(scenario["id"])
                because(scenario["id"], "install readiness for a version change")
        for change in version_changes:
            version_artifacts.update(name for name, recipe in recipes.items()
                                     if matches(change.path, recipe.get("paths", ())))
    for change in relevant:  # rule 5
        if change.driver_input or matches(change.path, DRIVER_INPUT_PATTERNS):
            stages.add(DRIVER_UPGRADE_STAGE)
            because(DRIVER_UPGRADE_STAGE, change.path)

    owner_tests: set[str] = set()
    for change in product_changes:
        jobs.update(jobs_for(change.path))
        owner = OWNER_TEST_PREFIX.match(change.path)
        if owner:
            owner_tests.add(owner.group(1))
            because(f"owner:{owner.group(1)}", change.path)
            continue
        for name, recipe in recipes.items():
            if artifact_affected(recipe, change.path, go_inputs.get(name)):
                product_artifacts.add(name)
                because(f"artifact:{name}", change.path)

    charts = {name for name in product_artifacts if recipes[name].get("kind", "").startswith("chart")}
    builds = product_artifacts - charts
    for owner, scenario in all_scenarios:
        meta = scenario["metadata"]
        # selected_by narrows which changed artifacts select a scenario on pull
        # requests; strict selection uses every artifact the scenario deploys.
        narrowed = None if strict else meta.get("selected_by")
        sid, required = scenario["id"], set(narrowed or meta.get("required_artifacts", ()))
        if owner in owner_tests:
            scenarios.add(sid)
        elif required & builds:
            scenarios.add(sid)
            because(sid, f"requires {', '.join(sorted(required & builds))}")
        elif required & charts and render_changed.get(sid, True):  # rule 4
            scenarios.add(sid)
            because(sid, "rendered charts changed" if sid in render_changed else "chart changed, no render inputs")

    classification = "selected" if product_changes else "version" if version_changes and not ci_changes else "ci"
    return Selection(classification, sorted(jobs), sorted(product_artifacts | version_artifacts), sorted(scenarios),
                     sorted(stages), reasons)


# --------------------------------------------------------------------------- I/O


def git(*args: str) -> str:
    return subprocess.run(["git", *args], cwd=ROOT, check=True, capture_output=True, text=True).stdout


VERSION_LINE = re.compile(r'^[+-](version|appVersion): "?[0-9A-Za-z.+-]+"?\s*$|^[+-]    version: [0-9A-Za-z.+-]+\s*$')


def changed_files(base: str, head: str) -> list[Change]:
    out = git("diff", "--name-only", "--no-renames", f"{base}...{head}")
    changes = []
    for path in filter(None, out.splitlines()):
        version_only = driver = False
        if path.endswith("Chart.yaml"):
            lines = [line for line in git("diff", "-U0", f"{base}...{head}", "--", path).splitlines()
                     if line[:1] in "+-" and not line.startswith(("+++", "---"))]
            version_only = bool(lines) and all(VERSION_LINE.match(line) for line in lines)
        if path == ".github/inputs/remote-content.json":
            lines = git("diff", "-U0", f"{base}...{head}", "--", path)
            driver = bool(DRIVER_REMOTE_CONTENT.search(lines))
        changes.append(Change(path, version_only, driver))
    return changes


def go_build_inputs(recipe: dict[str, Any]) -> dict[str, set[str]]:
    """Package directories and embedded files compiled into a Go artifact."""
    build = recipe["go_build"]
    module_dir = ROOT / build["module"]
    out = subprocess.run(["go", "list", "-deps", "-json", *build["main"]], cwd=module_dir, check=True,
                         capture_output=True, text=True).stdout
    dirs, embeds = set(), set()
    for package in json.loads("[" + re.sub(r"}\s*{", "},{", out.strip()) + "]"):
        directory = pathlib.Path(package.get("Dir", ""))
        if package.get("Standard") or not directory.is_relative_to(ROOT):
            continue
        relative = directory.relative_to(ROOT).as_posix()
        dirs.add(relative)
        embeds.update(f"{relative}/{name}" for name in package.get("EmbedFiles", ()))
    return {"dirs": dirs, "embeds": embeds}


def render(tree: pathlib.Path, spec: dict[str, Any]) -> str | None:
    """Render one declared chart input, or None when it cannot be rendered."""
    chart = tree / spec["chart"]
    if subprocess.run(["helm", "dependency", "build", "--skip-refresh", str(chart)], capture_output=True).returncode != 0:
        return None
    args = ["helm", "template", "render-check", str(chart)]
    for values in spec.get("values", ()):
        args += ["--values", str(tree / values)]
    for key, value in sorted(spec.get("set", {}).items()):
        args += ["--set-string", f"{key}={value}"]
    result = subprocess.run(args, capture_output=True, text=True)
    return result.stdout if result.returncode == 0 else None


def renders_differ(base: str | None, head: str | None) -> bool:
    """A render that fails on either side counts as changed: selection never fails open."""
    return base is None or head is None or base != head


def render_diff(base: str, catalogue: dict[str, Any]) -> dict[str, bool]:
    """Whether each scenario's declared chart renders differ between base and the working tree."""
    specs = {scenario["id"]: scenario["metadata"]["renders"]
             for _, scenario in scenarios_of(catalogue) if scenario["metadata"].get("renders")}
    if not specs:
        return {}
    with tempfile.TemporaryDirectory() as directory:
        base_tree = pathlib.Path(directory) / "base"
        git("worktree", "add", "--detach", str(base_tree), base)
        try:
            return {sid: any(renders_differ(render(base_tree, spec), render(ROOT, spec)) for spec in renders)
                    for sid, renders in specs.items()}
        finally:
            git("worktree", "remove", "--force", str(base_tree))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base", help="base commit (pull request base or merge-group base)")
    parser.add_argument("--head", default="HEAD")
    parser.add_argument("--all", action="store_true", help="select everything (full validation)")
    parser.add_argument("--catalogue", required=True, help="compiled catalogue JSON (make e2e-catalogue)")
    parser.add_argument("--strict", action="store_true",
                        help="ignore selected_by (merge queue, or the e2e-real-machine label)")
    parser.add_argument("--output", help="write the selection JSON here as well as to stdout")
    args = parser.parse_args(argv)

    catalogue = json.loads(pathlib.Path(args.catalogue).read_text(encoding="utf-8"))
    recipes = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))["artifact_recipes"]
    recipes = {name: recipe for name, recipe in recipes.items() if recipe.get("paths")}
    if args.all or not args.base:
        changes = [Change("<all>")]  # unowned sentinel: everything
    else:
        changes = changed_files(args.base, args.head)
    paths = [change.path for change in changes]
    go_inputs = {name: go_build_inputs(recipe) for name, recipe in recipes.items()
                 if recipe.get("go_build") and any(matches(p, recipe["paths"]) for p in paths)}
    needs_render = args.base and not args.all and any(p.startswith("charts/charts/") for p in paths)
    renders = render_diff(args.base, catalogue) if needs_render and shutil.which("helm") else None
    selection = select(changes, catalogue, recipes, go_inputs, renders, strict=args.strict)
    document = json.dumps({**selection.as_json(), "strict": args.strict, "base": args.base, "head": args.head,
                           "paths": paths}, indent=2)
    if args.output:
        pathlib.Path(args.output).write_text(document + "\n", encoding="utf-8")
    print(document)
    return 0


if __name__ == "__main__":
    sys.exit(main())

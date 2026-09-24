#!/usr/bin/env python3
"""Plan, record, and verify affected-target build-once release bundles."""

from __future__ import annotations

import argparse
from functools import lru_cache
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
from typing import Any

import e2e as e2e_contract
import release_baseline

SEMVER = re.compile(r"^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$")
SHA = re.compile(r"^[0-9a-f]{40}$")
TARGET_NAMES = (
    "control-plane",
    "inference-gateway",
    "forge",
    "control-plane-chart",
    "inference-gateway-chart",
    "iterabase-platform-chart",
)
RUNNABLE_E2E_TIERS = {"F2", "F3"}
CANDIDATE_ALIAS_SCHEME = "source-run-attempt-v1"
CANDIDATE_REPOSITORY = "nunocgoncalves/iterabase-mono"
CANDIDATE_WORKFLOW = ".github/workflows/release-candidate.yml"
CANDIDATE_EVENT = "workflow_dispatch"
POSITIVE_INTEGER = re.compile(r"^[1-9][0-9]*$")


class ReleaseError(ValueError):
    """The repository release contract or candidate is invalid."""


def compact(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ReleaseError(f"cannot read {path}: {exc}") from exc
    if not isinstance(value, dict):
        raise ReleaseError(f"{path} must contain a JSON object")
    return value


@lru_cache(maxsize=4)
def load_scenario_catalogue(root_value: str | Path) -> dict[str, Any]:
    try:
        return e2e_contract.load_catalogue(Path(root_value).resolve())
    except e2e_contract.E2EError as exc:
        raise ReleaseError(str(exc)) from exc


def catalogue_scenarios(catalogue: dict[str, Any]) -> list[dict[str, Any]]:
    try:
        return e2e_contract.catalogue_scenarios(catalogue)
    except e2e_contract.E2EError as exc:
        raise ReleaseError(str(exc)) from exc


def require_semver(value: Any, label: str) -> str:
    if not isinstance(value, str) or not SEMVER.fullmatch(value):
        raise ReleaseError(f"{label} must be stable SemVer without a v prefix: {value!r}")
    return value


def candidate_image_alias(source_sha: Any, run_id: Any, run_attempt: Any) -> str:
    if not isinstance(source_sha, str) or not SHA.fullmatch(source_sha):
        raise ReleaseError("candidate alias source_sha must be a full lowercase commit SHA")
    if not isinstance(run_id, str) or not POSITIVE_INTEGER.fullmatch(run_id):
        raise ReleaseError("candidate alias run_id must be a positive integer")
    if not isinstance(run_attempt, str) or not POSITIVE_INTEGER.fullmatch(run_attempt):
        raise ReleaseError("candidate alias run_attempt must be a positive integer")
    return f"{source_sha}-{run_id}-{run_attempt}"


def validate_candidate_aliases(plan: dict[str, Any]) -> None:
    scheme = plan.get("candidate_alias_scheme")
    if scheme is None:
        # Promotion remains able to verify retained pre-HOR-523 schema-v3
        # candidates. Newly generated plans always declare the immutable scheme.
        return
    if scheme != CANDIDATE_ALIAS_SCHEME:
        raise ReleaseError(f"unsupported candidate alias scheme {scheme!r}")
    expected = candidate_image_alias(
        plan.get("source_sha"), plan.get("run_id"), plan.get("run_attempt")
    )
    for image in plan.get("image_matrix", []):
        if not isinstance(image, dict) or image.get("candidate_tag") != expected:
            raise ReleaseError("candidate image alias does not bind source SHA, run ID, and run attempt")


def read_version(path: Path) -> str:
    try:
        value = path.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise ReleaseError(f"cannot read version authority {path}: {exc}") from exc
    return require_semver(value, str(path))


def chart_metadata(path: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise ReleaseError(f"cannot read {path}: {exc}") from exc
    for raw in lines:
        match = re.match(r"^(name|version|appVersion):\s*[\"']?([^\"'#\s]+)", raw)
        if match:
            result[match.group(1)] = match.group(2)
    missing = {"name", "version", "appVersion"} - result.keys()
    if missing:
        raise ReleaseError(f"{path} is missing {', '.join(sorted(missing))}")
    require_semver(result["version"], f"{path} version")
    # Helm appVersion is an opaque application identity and may legitimately
    # include a v prefix (for example cert-manager v1.21.0).
    return result


def chart_value(path: Path, key: str) -> str:
    wanted = key.split(".")
    stack: list[tuple[int, str]] = []
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        raise ReleaseError(f"cannot read chart values {path}: {exc}") from exc
    for raw in lines:
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        match = re.match(r"^(\s*)([A-Za-z0-9_-]+):(?:\s*(.*?))?\s*$", raw)
        if match is None:
            continue
        indent = len(match.group(1))
        name = match.group(2)
        value = (match.group(3) or "").split(" #", 1)[0].strip()
        while stack and stack[-1][0] >= indent:
            stack.pop()
        current = [item[1] for item in stack] + [name]
        if value and current == wanted:
            return value.strip("\"'")
        if not value:
            stack.append((indent, name))
    raise ReleaseError(f"cannot resolve {key} from {path}")


def chart_image_version(path: Path, key: str = "image.tag") -> str:
    return require_semver(chart_value(path, key), f"{path} {key}")


def chart_dependencies(path: Path) -> list[dict[str, str]]:
    dependencies: list[dict[str, str]] = []
    current: dict[str, str] = {}
    in_dependencies = False
    for raw in path.read_text(encoding="utf-8").splitlines():
        if raw == "dependencies:":
            in_dependencies = True
            continue
        if not in_dependencies:
            continue
        name = re.match(r"^\s*-\s+name:\s*([^\s#]+)", raw)
        if name:
            if current.get("name") and current.get("version"):
                dependencies.append(current)
            current = {"name": name.group(1)}
            continue
        alias = re.match(r"^\s+alias:\s*([^\s#]+)", raw)
        if alias and current:
            # Helm aliases are the deployed dependency identity. Preserve that
            # name in release evidence instead of recording two indistinguishable
            # copies of the source chart.
            current["name"] = alias.group(1)
            continue
        version = re.match(r"^\s+version:\s*[\"']?([^\"'#\s]+)", raw)
        if version and current:
            current["version"] = version.group(1)
    if current.get("name") and current.get("version"):
        dependencies.append(current)
    return dependencies


def fixture_versions(root: Path) -> dict[str, str]:
    source = (root / "forge" / "test" / "e2e" / "chart_fixture_test.go").read_text(
        encoding="utf-8"
    )
    constants = {
        "platform_chart": "pinnedPlatformChartVersion",
        "control_plane_chart": "pinnedControlPlaneChartVersion",
        "certificate_migration_source": "certificateMigrationSourceVersion",
    }
    result: dict[str, str] = {}
    for name, constant in constants.items():
        match = re.search(rf'{constant}\s*=\s*"([^"]+)"', source)
        if match is None:
            raise ReleaseError(f"cannot resolve test fixture {constant}")
        result[name] = require_semver(match.group(1), constant)

    return result


def chart_transition_baselines(snapshot: dict[str, Any]) -> dict[str, list[dict[str, str]]]:
    """Project exact historical fixture identities from the pinned snapshot."""
    names = set(release_baseline.FIXTURE_ORDER) - {"certificate-migration-chart"}
    charts = []
    for item in snapshot["snapshot"]["fixtures"]:
        if item["name"] not in names:
            continue
        repository, version = item["reference"].rsplit(":", 1)
        charts.append(
            {
                "name": item["name"], "chart": item["chart"], "repository": repository,
                "version": version, "sha256": item["sha256"], "oci_digest": item["oci_digest"],
                "size": item["size"], "filename": item["filename"],
            }
        )
    if {item["name"] for item in charts} != names:
        raise ReleaseError("pinned baseline snapshot transition fixtures are incomplete")
    return {"charts": charts}


def validate_contract(
    targets: dict[str, Any], root: Path, catalogue: dict[str, Any] | None = None
) -> None:
    if targets.get("schema_version") != 4:
        raise ReleaseError("release targets schema_version must be 4")
    definitions = targets.get("targets")
    recipes = targets.get("artifact_recipes")
    if not isinstance(definitions, dict) or tuple(definitions) != TARGET_NAMES:
        raise ReleaseError("release target names or order do not match the workflow contract")
    if not isinstance(recipes, dict):
        raise ReleaseError("release artifact recipes are missing")
    if "published_baselines" in targets:
        raise ReleaseError("release targets must not contain hand-maintained published baselines")
    for name, recipe in recipes.items():
        if not isinstance(recipe, dict):
            raise ReleaseError(f"artifact recipe {name!r} is invalid")
        if recipe.get("kind") == "published-chart" and any(
            field in recipe for field in ("reference", "checksum", "digest", "version")
        ):
            raise ReleaseError(f"published fixture recipe {name!r} must not contain baseline identity")
    if "checksum" in recipes.get("forge-binary", {}):
        raise ReleaseError("Forge build recipe must not contain a published-baseline checksum")

    for target, definition in definitions.items():
        if not isinstance(definition, dict) or not isinstance(definition.get("tag_prefix"), str):
            raise ReleaseError(f"target {target} is incomplete")
        artifacts = definition.get("artifacts")
        if not isinstance(artifacts, list) or not artifacts:
            raise ReleaseError(f"target {target} has no artifact recipes")
        for artifact in artifacts:
            recipe = recipes.get(artifact)
            if not isinstance(recipe, dict) or recipe.get("target") != target:
                raise ReleaseError(f"target {target} has invalid artifact recipe {artifact!r}")
        if "version_file" in definition:
            read_version(root / definition["version_file"])
        elif "chart" in definition:
            chart = definition["chart"]
            metadata = chart_metadata(root / "charts" / "charts" / chart / "Chart.yaml")
            if metadata["name"] != chart:
                raise ReleaseError(f"chart target {target} points at {metadata['name']}")
        else:
            raise ReleaseError(f"target {target} has no version authority")

    compiled = catalogue or load_scenario_catalogue(root)
    try:
        e2e_contract.validate_catalogue_contract(compiled, targets)
    except e2e_contract.E2EError as exc:
        raise ReleaseError(str(exc)) from exc
    fixture_versions(root)


def repository_versions(root: Path, targets: dict[str, Any]) -> dict[str, str]:
    versions: dict[str, str] = {}
    for target, definition in targets["targets"].items():
        if "version_file" in definition:
            versions[target] = read_version(root / definition["version_file"])
        else:
            versions[target] = chart_metadata(
                root / "charts" / "charts" / definition["chart"] / "Chart.yaml"
            )["version"]
    return versions


def parse_targets(value: str | list[str] | tuple[str, ...]) -> list[str]:
    raw = value.split(",") if isinstance(value, str) else list(value)
    requested = [item.strip() for item in raw]
    if not requested or any(not item for item in requested):
        raise ReleaseError("targets must be a non-empty comma-separated target set")
    unknown = sorted(set(requested) - set(TARGET_NAMES))
    if unknown:
        raise ReleaseError(f"unknown release targets: {', '.join(unknown)}")
    duplicates = sorted({item for item in requested if requested.count(item) > 1})
    if duplicates:
        raise ReleaseError(f"duplicate release targets: {', '.join(duplicates)}")
    return [target for target in TARGET_NAMES if target in requested]


def source_suites_for_targets(selected: list[str]) -> list[str]:
    suites: list[str] = []
    for target in selected:
        suite = "charts" if target.endswith("-chart") else target
        if suite not in suites:
            suites.append(suite)
    return suites


def make_plan(
    targets: dict[str, Any],
    selected_targets: str | list[str],
    master_sha: str,
    run_id: str,
    root: Path,
    catalogue: dict[str, Any] | None = None,
    *,
    run_attempt: str = "1",
    repository: str = CANDIDATE_REPOSITORY,
    workflow: str = CANDIDATE_WORKFLOW,
    event: str = CANDIDATE_EVENT,
    control_sha: str | None = None,
    resolved_baseline: dict[str, Any] | None = None,
    baseline_anchor_release_id: int | None = None,
) -> dict[str, Any]:
    selected = parse_targets(selected_targets)
    candidate_tag = candidate_image_alias(master_sha, run_id, run_attempt)
    control_sha = control_sha or master_sha
    if repository != CANDIDATE_REPOSITORY:
        raise ReleaseError(f"candidate repository must be {CANDIDATE_REPOSITORY}")
    if workflow != CANDIDATE_WORKFLOW:
        raise ReleaseError(f"candidate workflow must be {CANDIDATE_WORKFLOW}")
    if event != CANDIDATE_EVENT:
        raise ReleaseError(f"candidate event must be {CANDIDATE_EVENT}")
    if not SHA.fullmatch(control_sha):
        raise ReleaseError("candidate control_sha must be a full lowercase commit SHA")
    if resolved_baseline is None:
        raise ReleaseError("candidate planning requires one pinned complete baseline snapshot")
    try:
        release_baseline.validate_snapshot(resolved_baseline, targets)
        resolved_baseline = json.loads(compact(resolved_baseline))
    except release_baseline.BaselineError as exc:
        raise ReleaseError(f"candidate baseline snapshot is invalid: {exc}") from exc
    anchor_release_id = resolved_baseline["snapshot"]["anchor"]["release_id"]
    if baseline_anchor_release_id is None or anchor_release_id != baseline_anchor_release_id:
        raise ReleaseError(
            f"pinned Latest Release ID {anchor_release_id} does not match required baseline_anchor_release_id {baseline_anchor_release_id}"
        )

    versions = repository_versions(root, targets)
    metadata = {
        name: chart_metadata(root / "charts" / "charts" / name / "Chart.yaml")
        for name in (
            "control-plane",
            "inference-gateway",
            "iterabase-platform",
            "cert-manager-substrate",
            "lvm-storage-substrate",
        )
    }
    fixtures = fixture_versions(root)
    fixture_index = release_baseline.artifact_map(resolved_baseline, targets)
    if fixture_index["certificate-migration-chart"]["version"] != fixtures["certificate_migration_source"]:
        raise ReleaseError("source certificate migration fixture version disagrees with the pinned snapshot")
    compiled_catalogue = catalogue or load_scenario_catalogue(root)
    validate_contract(targets, root, compiled_catalogue)
    releases: list[dict[str, Any]] = []
    images: list[dict[str, Any]] = []
    chart_matrix: list[dict[str, Any]] = []
    selected_chart_dependencies: list[dict[str, Any]] = []

    recipes = targets["artifact_recipes"]
    for target in selected:
        definition = targets["targets"][target]
        version = versions[target]
        artifact_types: list[str] = []
        target_recipes = [recipes[name] for name in definition["artifacts"]]
        image_recipes = [recipe for recipe in target_recipes if recipe["kind"] == "image"]
        if image_recipes:
            artifact_types.append("image")
            images.extend(
                {
                    "name": image["name"],
                    "artifact": next(name for name in definition["artifacts"] if recipes[name] is image),
                    "repository": image["repository"],
                    "context": image["context"],
                    "dockerfile": image["dockerfile"],
                    "build_args": e2e_contract.render_recipe_values(image["build_args"], version=version, source_sha=master_sha),
                    "labels": e2e_contract.render_recipe_values(image["labels"], version=version, source_sha=master_sha),
                    "build_args_text": "\n".join(e2e_contract.render_recipe_values(image["build_args"], version=version, source_sha=master_sha)),
                    "labels_text": "\n".join(e2e_contract.render_recipe_values(image["labels"], version=version, source_sha=master_sha)),
                    "recipe_sha256": e2e_contract.recipe_hash(image),
                    "target": target,
                    "version": version,
                    "candidate_tag": candidate_tag,
                }
                for image in image_recipes
            )
        chart_recipes = [recipe for recipe in target_recipes if recipe["kind"] == "chart"]
        if chart_recipes:
            artifact_types.append("chart")
            recipe = chart_recipes[0]
            chart = recipe["chart"]
            companions = recipe.get("companions", [])
            dependencies = chart_dependencies(root / "charts" / "charts" / chart / "Chart.yaml")
            selected_chart_dependencies.append({"target": target, "chart": chart, "dependencies": dependencies})
            for companion in companions:
                selected_chart_dependencies.append(
                    {
                        "target": target,
                        "chart": companion,
                        "dependencies": chart_dependencies(
                            root / "charts" / "charts" / companion / "Chart.yaml"
                        ),
                    }
                )
            chart_matrix.append(
                {
                    "target": target,
                    "chart": chart,
                    "version": version,
                    "companions": companions,
                    "recipe_sha256": e2e_contract.recipe_hash(recipe),
                    "companion_recipes": [
                        {
                            "chart": companion,
                            "artifact": name,
                            "recipe_sha256": e2e_contract.recipe_hash(recipes[name]),
                        }
                        for companion in companions
                        for name in definition["artifacts"]
                        if recipes[name].get("chart") == companion
                    ],
                }
            )
        if any(recipe["kind"] == "forge" for recipe in target_recipes):
            artifact_types.append("forge")
        releases.append(
            {
                "target": target,
                "version": version,
                "production_tag": f"{definition['tag_prefix']}{version}",
                "artifact_types": artifact_types,
            }
        )

    source_suites = source_suites_for_targets(selected)
    try:
        execution_plan = e2e_contract.make_plan(
            root,
            compiled_catalogue,
            targets,
            intent="candidate",
            source_sha=master_sha,
            targets=selected,
            resolved_baseline=resolved_baseline,
        )
    except e2e_contract.E2EError as exc:
        raise ReleaseError(str(exc)) from exc
    selected_scenario_ids = set(execution_plan["selected_scenario_ids"])
    scenarios = [
        scenario
        for scenario in catalogue_scenarios(compiled_catalogue)
        if scenario["id"] in selected_scenario_ids
    ]
    chart_runtime = False
    kind_matrix = execution_plan["kind_matrix"]
    real_machine_matrix = execution_plan["real_machine_matrix"]

    real_machine = bool(real_machine_matrix)
    transition_baselines = (
        chart_transition_baselines(resolved_baseline)
        if any(scenario["suite"]["owner"] == "charts" for scenario in scenarios)
        else {"charts": []}
    )
    planned_baselines: dict[str, dict[str, Any]] = {}
    for scenario in execution_plan["scenario_matrix"]:
        for artifact in scenario["artifacts"]:
            if artifact["custody"] == "published-baseline":
                planned_baselines.setdefault(artifact["name"], artifact)

    plan = {
        "schema_version": 4,
        "candidate_repository": repository,
        "candidate_workflow": workflow,
        "candidate_event": event,
        "candidate_control_sha": control_sha,
        "candidate_alias_scheme": CANDIDATE_ALIAS_SCHEME,
        "run_id": run_id,
        "run_attempt": run_attempt,
        "source_sha": master_sha,
        "baseline_anchor_release_id": baseline_anchor_release_id,
        "baseline_snapshot_sha256": resolved_baseline["snapshot_sha256"],
        "resolved_baseline": resolved_baseline,
        "targets": selected,
        "releases": releases,
        "source_suites": source_suites,
        "selected_scenarios": execution_plan["selected_scenario_ids"],
        "execution_plan": execution_plan,
        "kind_matrix": kind_matrix,
        "real_machine_matrix": real_machine_matrix,
        "real_machine": real_machine,
        "chart_runtime": chart_runtime,
        "image_matrix": images,
        "chart_matrix": chart_matrix,
        "forge": "forge" in selected,
        "forge_recipe_sha256": e2e_contract.recipe_hash(targets["artifact_recipes"]["forge-binary"]),
        "forge_goreleaser_config_sha256": e2e_contract.hash_file(
            root / targets["artifact_recipes"]["forge-binary"]["goreleaser_config"]
        ),
        "baseline_dependencies": {
            "snapshot_sha256": resolved_baseline["snapshot_sha256"],
            "artifacts": [planned_baselines[name] for name in sorted(planned_baselines)],
        },
        "transition_baselines": transition_baselines,
        "tested_with": {
            "repository_versions": versions,
            "chart_metadata": metadata,
            "selected_chart_dependencies": selected_chart_dependencies,
            "fixture_versions": fixtures,
            "transition_baselines": transition_baselines,
            "scenario_catalogue": {
                "schema_version": compiled_catalogue["schema_version"],
                "selected": [
                    {
                        "id": scenario["id"],
                        "metadata": scenario["metadata"],
                        "stages": scenario["stages"],
                    }
                    for scenario in scenarios
                ],
            },
        },
    }
    return plan


def candidate_job_selection(plan: dict[str, Any]) -> dict[str, bool]:
    source_suites = plan.get("source_suites")
    if not isinstance(source_suites, list) or any(
        not isinstance(suite, str) for suite in source_suites
    ):
        raise ReleaseError("candidate plan source_suites must be a list of names")
    for field in ("image_matrix", "chart_matrix", "kind_matrix"):
        if not isinstance(plan.get(field), list):
            raise ReleaseError(f"candidate plan {field} must be a list")
    if not isinstance(plan.get("real_machine_matrix"), list):
        raise ReleaseError("candidate plan real_machine_matrix must be a list")
    execution = plan.get("execution_plan")
    if not isinstance(execution, dict) or not isinstance(execution.get("artifact_build_matrix"), list):
        raise ReleaseError("candidate plan has no compiled execution plan")
    for field in ("forge", "chart_runtime", "real_machine"):
        if not isinstance(plan.get(field), bool):
            raise ReleaseError(f"candidate plan {field} must be a boolean")

    return {
        "preflight": True,
        "control-plane-source": "control-plane" in source_suites,
        "inference-gateway-source": "inference-gateway" in source_suites,
        "forge-source": "forge" in source_suites,
        "charts-source": "charts" in source_suites,
        # Nested Go E2E modules and the shared testkit back every owner's
        # compiled scenarios, so this lint owner is selected for every
        # candidate and is never skippable.
        "nested-go-lint": True,
        "image-candidates": bool(plan["image_matrix"]),
        "runtime-artifacts": bool(execution["artifact_build_matrix"]),
        "chart-candidate": bool(plan["chart_matrix"]),
        "forge-candidate": plan["forge"],
        "kind-candidates": bool(plan["kind_matrix"]),
        "real-machine-candidates": plan["real_machine"],
    }


def validate_candidate_job_results(
    plan: dict[str, Any], needs: dict[str, Any]
) -> dict[str, str]:
    selected = candidate_job_selection(plan)
    missing = sorted(set(selected) - set(needs))
    unexpected = sorted(set(needs) - set(selected))
    if missing or unexpected:
        raise ReleaseError(
            "candidate validation job set mismatch: "
            + compact({"missing": missing, "unexpected": unexpected})
        )

    results: dict[str, str] = {}
    incomplete: dict[str, dict[str, Any]] = {}
    for name, is_selected in selected.items():
        job = needs[name]
        result = job.get("result") if isinstance(job, dict) else None
        if not isinstance(result, str):
            raise ReleaseError(f"candidate validation job {name} has no result")
        results[name] = result
        if result == "success" or (result == "skipped" and not is_selected):
            continue
        incomplete[name] = {"result": result, "selected": is_selected}

    if incomplete:
        raise ReleaseError("candidate validation incomplete: " + compact(incomplete))
    return results


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def asset_records(directory: Path) -> list[dict[str, Any]]:
    if not directory.exists():
        return []
    return [
        {
            "path": str(path.relative_to(directory)),
            "sha256": sha256_file(path),
            "size": path.stat().st_size,
        }
        for path in sorted(directory.rglob("*"))
        if path.is_file()
    ]


def release_asset_paths(plan: dict[str, Any], candidate: Path, target: str) -> list[Path]:
    release = next((item for item in plan["releases"] if item["target"] == target), None)
    if release is None:
        raise ReleaseError(f"candidate has no release target {target!r}")
    artifact_types = set(release["artifact_types"])
    result = [
        candidate / "candidate-plan.json",
        candidate / "candidate-evidence.json",
        candidate / "baseline-snapshot.json",
    ]
    if "image" in artifact_types:
        for image in plan["image_matrix"]:
            if image["target"] == target:
                result.extend(
                    [
                        candidate / "assets/images" / f"candidate-{image['name']}.json",
                        candidate / "assets/images" / f"candidate-{image['name']}.spdx.json",
                    ]
                )
    if "chart" in artifact_types:
        chart = next(item for item in plan["chart_matrix"] if item["target"] == target)
        name = chart["chart"]
        result.extend(
            [
                candidate / "assets/charts" / f"candidate-chart-{name}.json",
                candidate / "assets/charts" / f"candidate-chart-{name}.spdx.json",
                candidate / "assets/charts" / f"checksums-{name}.txt",
            ]
        )
        for archive in [name, *chart["companions"]]:
            result.append(candidate / "assets/charts" / f"{archive}-{chart['version']}.tgz")
    if "forge" in artifact_types:
        result.extend(sorted(path for path in (candidate / "assets/forge").rglob("*") if path.is_file()))
    names = [path.name for path in result]
    if len(names) != len(set(names)):
        raise ReleaseError(f"release target {target} has duplicate asset filenames")
    for path in result:
        if not path.is_file():
            raise ReleaseError(f"release target {target} is missing candidate asset {path}")
    return result


def planned_release_manifest(
    plan: dict[str, Any], candidate_snapshot: dict[str, Any], target: str
) -> dict[str, Any]:
    release = next(item for item in plan["releases"] if item["target"] == target)
    body = candidate_snapshot["snapshot"]
    cohort = next(item for item in body["targets"] if item["target"] == target)
    names = ["candidate-plan.json", "candidate-evidence.json", "baseline-snapshot.json"]
    if "image" in release["artifact_types"]:
        for image in plan["image_matrix"]:
            if image["target"] == target:
                names.extend([f"candidate-{image['name']}.json", f"candidate-{image['name']}.spdx.json"])
    if "chart" in release["artifact_types"]:
        chart = next(item for item in plan["chart_matrix"] if item["target"] == target)
        names.extend(
            [
                f"candidate-chart-{chart['chart']}.json",
                f"candidate-chart-{chart['chart']}.spdx.json",
                f"checksums-{chart['chart']}.txt",
                *[f"{name}-{chart['version']}.tgz" for name in [chart["chart"], *chart["companions"]]],
            ]
        )
    if "forge" in release["artifact_types"]:
        names.extend(["candidate-forge.json", "checksums.txt"])
        for platform in release_baseline.FORGE_PLATFORMS:
            names.extend([f"forge_{release['version']}_{platform}.tar.gz", f"forge_{release['version']}_{platform}.tar.gz.sbom.json"])
    if len(names) != len(set(names)):
        raise ReleaseError(f"planned Release target {target} has duplicate asset names")
    return {
        "schema_version": 3,
        "mode": "planned-final",
        "release_id": None,
        "target": target,
        "version": release["version"],
        "tag": release["production_tag"],
        "source_sha": plan["source_sha"],
        "candidate_run_id": plan["run_id"],
        "candidate_run_attempt": plan["run_attempt"],
        "cohort_id": body["cohort_id"],
        "parent_anchor": body["parent_anchor"],
        "anchor_target": body["anchor_target"],
        "planned_final_snapshot_sha256": candidate_snapshot["snapshot_sha256"],
        "planned_asset_names": names,
        "artifacts": cohort["artifacts"],
        "release_metadata": {"title": release["production_tag"], "target_commitish": plan["source_sha"], "prerelease": False, "make_latest": False},
    }


def planned_release_manifests(
    plan: dict[str, Any], candidate_snapshot: dict[str, Any]
) -> list[dict[str, Any]]:
    return [planned_release_manifest(plan, candidate_snapshot, target) for target in plan["targets"]]


def release_manifest(plan: dict[str, Any], candidate: Path, target: str) -> dict[str, Any]:
    release = next(item for item in plan["releases"] if item["target"] == target)
    snapshot = load_json(candidate / "baseline-snapshot.json")
    contract = load_json(Path(__file__).resolve().parents[2] / "release" / "targets.json")
    try:
        release_baseline.validate_snapshot(snapshot, contract)
    except release_baseline.BaselineError as exc:
        raise ReleaseError(f"final baseline snapshot is invalid: {exc}") from exc
    body = snapshot["snapshot"]
    if body["mode"] != "published" or target not in body["selected_targets"]:
        raise ReleaseError(f"final baseline snapshot does not select release target {target}")
    cohort = next(item for item in body["targets"] if item["target"] == target)
    if (
        cohort["version"] != release["version"]
        or cohort["source_sha"] != plan["source_sha"]
        or cohort["release"]["tag"] != release["production_tag"]
    ):
        raise ReleaseError(f"final snapshot cohort {target} disagrees with the candidate release plan")
    release_id = cohort["release"]["id"]
    paths = release_asset_paths(plan, candidate, target)
    tag = release["production_tag"]
    notes = (
        f"Release of **{target} {release['version']}** from `{plan['source_sha']}` "
        f"as part of candidate run `{plan['run_id']}` and complete cohort `{body['cohort_id']}`.\n\n"
        "Built and validated once, staged non-Latest as a complete draft, and published without rebuilding. "
        "The attached schema-v3 manifest and shared baseline snapshot bind every exact member."
    )
    return {
        "schema_version": 3,
        "release_id": release_id,
        "target": target,
        "version": release["version"],
        "tag": tag,
        "source_sha": plan["source_sha"],
        "candidate_run_id": plan["run_id"],
        "candidate_run_attempt": plan["run_attempt"],
        "cohort_id": body["cohort_id"],
        "parent_anchor": body["parent_anchor"],
        "anchor_target": body["anchor_target"],
        "baseline_snapshot_sha256": snapshot["snapshot_sha256"],
        "release_metadata": {
            "title": tag,
            "notes": notes,
            "target_commitish": plan["source_sha"],
            "prerelease": False,
            "make_latest": False,
        },
        "assets": [
            {
                "name": path.name,
                "path": str(path.relative_to(candidate)),
                "size": path.stat().st_size,
                "sha256": sha256_file(path),
            }
            for path in paths
        ],
    }


def validate_release_manifest(manifest: dict[str, Any], candidate: Path, plan: dict[str, Any]) -> None:
    target = manifest.get("target")
    if not isinstance(target, str):
        raise ReleaseError("release manifest has no target")
    expected = release_manifest(plan, candidate, target)
    if manifest != expected:
        raise ReleaseError(f"release manifest for {target} does not match the complete candidate member set")
    names = [item.get("name") for item in manifest["assets"]]
    paths = [item.get("path") for item in manifest["assets"]]
    if len(names) != len(set(names)) or len(paths) != len(set(paths)):
        raise ReleaseError(f"release manifest for {target} has duplicate members")


def write_release_manifests(candidate: Path, output: Path) -> list[Path]:
    plan = verify_candidate(candidate)
    output.mkdir(parents=True, exist_ok=True)
    paths: list[Path] = []
    for target in plan["targets"]:
        manifest = release_manifest(plan, candidate, target)
        path = output / f"release-manifest-{target}.json"
        path.write_text(compact(manifest) + "\n", encoding="utf-8")
        paths.append(path)
    return paths


def verify_release_manifests(candidate: Path, directory: Path) -> list[dict[str, Any]]:
    plan = verify_candidate(candidate)
    expected_names = {f"release-manifest-{target}.json" for target in plan["targets"]}
    discovered = {path.name for path in directory.glob("release-manifest-*.json")}
    if discovered != expected_names:
        raise ReleaseError("release manifest set is missing, extra, or ambiguous")
    manifests = [load_json(directory / name) for name in sorted(expected_names)]
    for manifest in manifests:
        validate_release_manifest(manifest, candidate, plan)
    return manifests


def validate_candidate_assets(plan: dict[str, Any], assets: Path) -> None:
    validate_candidate_aliases(plan)
    if plan["image_matrix"]:
        expected = {item["name"]: item for item in plan["image_matrix"]}
        expected_files = {
            filename
            for name in expected
            for filename in (f"candidate-{name}.json", f"candidate-{name}.spdx.json")
        }
        actual_files = {path.name for path in (assets / "images").glob("*") if path.is_file()}
        if actual_files != expected_files:
            raise ReleaseError("candidate image file set is missing, extra, or ambiguous")
        discovered: set[str] = set()
        for metadata_path in sorted((assets / "images").glob("candidate-*.json")):
            if metadata_path.name.endswith(".spdx.json"):
                continue
            metadata = load_json(metadata_path)
            name = metadata.get("name")
            if not isinstance(name, str) or name not in expected:
                raise ReleaseError(f"{metadata_path} is unexpected candidate image metadata")
            if name in discovered:
                raise ReleaseError(f"candidate image metadata for {name} is duplicated")
            planned = expected[name]
            required_identity = {
                "schema_version": 2,
                "artifact_type": "image",
                "name": planned["name"],
                "target": planned["target"],
                "repository": planned["repository"],
                "candidate_tag": planned["candidate_tag"],
                "version": planned["version"],
                "source_sha": plan["source_sha"],
                "recipe_sha256": planned["recipe_sha256"],
            }
            for field, value in required_identity.items():
                if metadata.get(field) != value:
                    raise ReleaseError(
                        f"{metadata_path} {field} does not match the planned candidate identity"
                    )
            digest = metadata.get("digest")
            if not isinstance(digest, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise ReleaseError(f"{metadata_path} has no canonical digest")
            discovered.add(name)
        if discovered != set(expected):
            raise ReleaseError(
                f"candidate image evidence mismatch: {discovered} != {set(expected)}"
            )
    elif any(path.is_file() for path in (assets / "images").glob("*")):
        raise ReleaseError("candidate contains image files without a selected image target")

    expected_chart_files: set[str] = set()
    for chart_plan in plan["chart_matrix"]:
        chart = chart_plan["chart"]
        version = chart_plan["version"]
        metadata = load_json(assets / "charts" / f"candidate-chart-{chart}.json")
        for field, expected in {
            "schema_version": 2,
            "artifact_type": "chart",
            "target": chart_plan["target"],
            "chart": chart,
            "version": version,
            "source_sha": plan["source_sha"],
            "recipe_sha256": chart_plan["recipe_sha256"],
        }.items():
            if metadata.get(field) != expected:
                raise ReleaseError(f"candidate chart {chart} {field} does not match the plan")
        expected_archives = [chart, *chart_plan["companions"]]
        expected_chart_files.update(
            {
                f"candidate-chart-{chart}.json",
                f"candidate-chart-{chart}.spdx.json",
                f"checksums-{chart}.txt",
                *[f"{expected_chart}-{version}.tgz" for expected_chart in expected_archives],
                *[f"candidate-chart-{companion}.json" for companion in chart_plan["companions"]],
            }
        )
        for expected_chart in expected_archives:
            if not (assets / "charts" / f"{expected_chart}-{version}.tgz").is_file():
                raise ReleaseError(f"candidate chart archive for {expected_chart} is missing")
        if not (assets / "charts" / f"checksums-{chart}.txt").is_file():
            raise ReleaseError(f"candidate chart checksums for {chart} are missing")
    actual_chart_files = {
        path.name for path in (assets / "charts").glob("*") if path.is_file()
    }
    if actual_chart_files != expected_chart_files:
        raise ReleaseError("candidate chart file set is missing, extra, or ambiguous")

    if plan["forge"]:
        metadata = load_json(assets / "forge" / "candidate-forge.json")
        if metadata.get("source_sha") != plan["source_sha"] or metadata.get("recipe_sha256") != plan["forge_recipe_sha256"]:
            raise ReleaseError("Forge candidate source or recipe identity does not match the plan")
        archives = sorted((assets / "forge").glob("forge_*_*.tar.gz"))
        expected_platforms = {"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}
        found = {
            next((platform for platform in expected_platforms if platform in archive.name), "")
            for archive in archives
        }
        if found != expected_platforms:
            raise ReleaseError(f"Forge candidate platform matrix is incomplete: {found}")
        if not (assets / "forge" / "checksums.txt").is_file():
            raise ReleaseError("Forge candidate checksums are missing")
        expected_forge_files = {"candidate-forge.json", "checksums.txt"}
        for platform in expected_platforms:
            expected_forge_files.update(
                {
                    f"forge_{metadata['version']}_{platform}.tar.gz",
                    f"forge_{metadata['version']}_{platform}.tar.gz.sbom.json",
                }
            )
        actual_forge_files = {
            path.name for path in (assets / "forge").glob("*") if path.is_file()
        }
        if actual_forge_files != expected_forge_files:
            raise ReleaseError("Forge candidate file set is missing, extra, or ambiguous")
    elif any(path.is_file() for path in (assets / "forge").glob("*")):
        raise ReleaseError("candidate contains Forge files without a selected Forge target")


def assemble_evidence(
    plan: dict[str, Any], assets: Path, contract: dict[str, Any]
) -> dict[str, Any]:
    validate_candidate_assets(plan, assets)
    candidate_snapshot = release_baseline.build_candidate_snapshot(plan, assets, contract)
    planned_snapshot = release_baseline.planned_final_snapshot(candidate_snapshot, contract)
    candidate_root = assets.parent
    (candidate_root / "candidate-snapshot.json").write_text(
        compact(candidate_snapshot) + "\n", encoding="utf-8"
    )
    (candidate_root / "planned-final-snapshot.json").write_text(
        compact(planned_snapshot) + "\n", encoding="utf-8"
    )
    planned_manifests = planned_release_manifests(plan, planned_snapshot)
    planned_directory = candidate_root / "planned-release-manifests"
    planned_directory.mkdir(exist_ok=True)
    for manifest in planned_manifests:
        (planned_directory / f"release-manifest-{manifest['target']}.json").write_text(
            compact(manifest) + "\n", encoding="utf-8"
        )
    with tempfile.TemporaryDirectory(prefix="iterabase-candidate-plan-") as value:
        normalized_plan = Path(value) / "candidate-plan.json"
        normalized_plan.write_text(compact(plan) + "\n", encoding="utf-8")
        try:
            scenario_results = e2e_contract.validate_results(normalized_plan, assets / "results")
        except e2e_contract.E2EError as exc:
            raise ReleaseError(f"candidate scenario evidence is incomplete: {exc}") from exc
    records = asset_records(assets)
    if not records:
        raise ReleaseError("candidate has no recorded assets")
    candidate = {
        field: plan[field]
        for field in (
            "candidate_repository",
            "candidate_workflow",
            "candidate_event",
            "candidate_control_sha",
            "candidate_alias_scheme",
            "run_id",
            "run_attempt",
            "source_sha",
            "baseline_anchor_release_id",
            "baseline_snapshot_sha256",
            "targets",
            "releases",
        )
    }
    return {
        "schema_version": 4,
        "candidate": candidate,
        "tested_with": plan["tested_with"],
        "validation": {
            "status": "passed",
            "scenario_results": [
                {
                    "scenario_id": result["scenario_id"],
                    "stage_graph_sha256": result["stage_graph_sha256"],
                    "runtime_bundle_sha256": result["runtime_bundle_sha256"],
                    "stages": result["stages"],
                    "artifacts": result["artifacts"],
                }
                for result in scenario_results
            ],
        },
        "plan_sha256": hashlib.sha256((compact(plan) + "\n").encode()).hexdigest(),
        "candidate_snapshot_sha256": candidate_snapshot["snapshot_sha256"],
        "planned_final_snapshot_sha256": planned_snapshot["snapshot_sha256"],
        "planned_release_manifests_sha256": hashlib.sha256(compact(planned_manifests).encode()).hexdigest(),
        "assets": records,
    }


def verify_candidate(directory: Path) -> dict[str, Any]:
    plan_path = directory / "candidate-plan.json"
    evidence_path = directory / "candidate-evidence.json"
    assets = directory / "assets"
    plan = load_json(plan_path)
    evidence = load_json(evidence_path)
    contract = load_json(Path(__file__).resolve().parents[2] / "release" / "targets.json")
    candidate_snapshot = load_json(directory / "candidate-snapshot.json")
    planned_snapshot = load_json(directory / "planned-final-snapshot.json")
    planned_directory = directory / "planned-release-manifests"
    try:
        release_baseline.validate_snapshot(candidate_snapshot, contract)
        release_baseline.validate_snapshot(planned_snapshot, contract)
    except release_baseline.BaselineError as exc:
        raise ReleaseError(f"candidate snapshot is invalid: {exc}") from exc
    try:
        expected_candidate_snapshot = release_baseline.build_candidate_snapshot(plan, assets, contract)
        expected_planned_snapshot = release_baseline.planned_final_snapshot(expected_candidate_snapshot, contract)
    except release_baseline.BaselineError as exc:
        raise ReleaseError(f"candidate snapshots cannot be reproduced from retained assets: {exc}") from exc
    if candidate_snapshot != expected_candidate_snapshot or planned_snapshot != expected_planned_snapshot:
        raise ReleaseError("retained candidate/final snapshot plans do not match the exact plan and assets")
    expected_planned_manifests = planned_release_manifests(plan, planned_snapshot)
    expected_names = {f"release-manifest-{item['target']}.json" for item in expected_planned_manifests}
    if {path.name for path in planned_directory.glob("release-manifest-*.json")} != expected_names:
        raise ReleaseError("planned schema-v3 Release manifest set is incomplete")
    actual_planned_manifests = [
        load_json(planned_directory / f"release-manifest-{item['target']}.json")
        for item in expected_planned_manifests
    ]
    if actual_planned_manifests != expected_planned_manifests:
        raise ReleaseError("planned schema-v3 Release manifests do not match the complete candidate")
    if (
        evidence.get("candidate_snapshot_sha256") != candidate_snapshot.get("snapshot_sha256")
        or evidence.get("planned_final_snapshot_sha256") != planned_snapshot.get("snapshot_sha256")
        or evidence.get("planned_release_manifests_sha256") != hashlib.sha256(compact(expected_planned_manifests).encode()).hexdigest()
    ):
        raise ReleaseError("candidate evidence does not bind the complete candidate/final snapshot plans")
    required_authority = {
        "schema_version": 4,
        "candidate_repository": CANDIDATE_REPOSITORY,
        "candidate_workflow": CANDIDATE_WORKFLOW,
        "candidate_event": CANDIDATE_EVENT,
        "candidate_alias_scheme": CANDIDATE_ALIAS_SCHEME,
    }
    for field, expected in required_authority.items():
        if plan.get(field) != expected:
            raise ReleaseError(f"candidate plan {field} does not match promotion authority")
    if not SHA.fullmatch(str(plan.get("candidate_control_sha", ""))):
        raise ReleaseError("candidate plan has no exact workflow control SHA")
    if not POSITIVE_INTEGER.fullmatch(str(plan.get("run_id", ""))) or not POSITIVE_INTEGER.fullmatch(str(plan.get("run_attempt", ""))):
        raise ReleaseError("candidate plan has no exact run ID and attempt")
    if evidence.get("schema_version") != 4 or evidence.get("validation", {}).get("status") != "passed":
        raise ReleaseError("candidate evidence is not a passed schema-v4 record")
    expected_plan_hash = hashlib.sha256((compact(plan) + "\n").encode()).hexdigest()
    if evidence.get("plan_sha256") != expected_plan_hash:
        raise ReleaseError("candidate plan does not match evidence")
    actual = asset_records(assets)
    if evidence.get("assets") != actual:
        raise ReleaseError("candidate assets do not match recorded checksums")
    with tempfile.TemporaryDirectory(prefix="iterabase-candidate-plan-") as value:
        normalized_plan = Path(value) / "candidate-plan.json"
        normalized_plan.write_text(compact(plan) + "\n", encoding="utf-8")
        try:
            scenario_results = e2e_contract.validate_results(normalized_plan, assets / "results")
        except e2e_contract.E2EError as exc:
            raise ReleaseError(f"candidate scenario evidence is incomplete: {exc}") from exc
    recorded_results = evidence.get("validation", {}).get("scenario_results")
    actual_results = [
        {
            "scenario_id": result["scenario_id"],
            "stage_graph_sha256": result["stage_graph_sha256"],
            "runtime_bundle_sha256": result["runtime_bundle_sha256"],
            "stages": result["stages"],
            "artifacts": result["artifacts"],
        }
        for result in scenario_results
    ]
    if recorded_results != actual_results:
        raise ReleaseError("candidate evidence does not retain the exact scenario/stage/runtime result records")
    candidate = evidence.get("candidate", {})
    fields = [
        "candidate_repository",
        "candidate_workflow",
        "candidate_event",
        "candidate_control_sha",
        "candidate_alias_scheme",
        "run_id",
        "run_attempt",
        "source_sha",
        "baseline_anchor_release_id",
        "baseline_snapshot_sha256",
        "targets",
        "releases",
    ]
    for field in fields:
        if candidate.get(field) != plan.get(field):
            raise ReleaseError(f"candidate evidence {field} does not match plan")
    validate_candidate_assets(plan, assets)
    return plan


def write_github_outputs(path: Path, plan: dict[str, Any]) -> None:
    forge_release = next(
        (release for release in plan["releases"] if release["target"] == "forge"), None
    )
    outputs = {
        "plan": compact(plan),
        "targets": compact(plan["targets"]),
        "releases": compact(plan["releases"]),
        "forge_version": forge_release["version"] if forge_release else "",
        "forge_recipe_sha256": plan["forge_recipe_sha256"],
        "image_matrix": compact(plan["image_matrix"]),
        "chart_matrix": compact(plan["chart_matrix"]),
        "kind_matrix": compact(plan["kind_matrix"]),
        "real_machine_matrix": compact(plan["real_machine_matrix"]),
        "runtime_artifact_matrix": compact(plan["execution_plan"]["artifact_build_matrix"]),
        "has_runtime_artifacts": str(bool(plan["execution_plan"]["artifact_build_matrix"])).lower(),
        "has_images": str(bool(plan["image_matrix"])).lower(),
        "has_chart": str(bool(plan["chart_matrix"])).lower(),
        "has_forge": str(bool(plan["forge"])).lower(),
        "run_control_plane": str("control-plane" in plan["source_suites"]).lower(),
        "run_inference_gateway": str("inference-gateway" in plan["source_suites"]).lower(),
        "run_forge": str("forge" in plan["source_suites"]).lower(),
        "run_charts": str("charts" in plan["source_suites"]).lower(),
        "run_nested_go_lint": "true",
        "run_chart_runtime": str(plan["chart_runtime"]).lower(),
        "run_kind": str(bool(plan["kind_matrix"])).lower(),
        "run_real_machine": str(bool(plan["real_machine"])).lower(),
    }
    with path.open("a", encoding="utf-8") as output:
        for name, value in outputs.items():
            output.write(f"{name}={value}\n")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("validate")
    validate_jobs = sub.add_parser("validate-jobs")
    validate_jobs.add_argument("--plan", type=Path, required=True)
    validate_jobs.add_argument("--needs", type=Path, required=True)
    outputs = sub.add_parser("outputs")
    outputs.add_argument("--plan", type=Path, required=True)
    outputs.add_argument("--github-output", type=Path, required=True)
    plan = sub.add_parser("plan")
    plan.add_argument("--targets", required=True)
    plan.add_argument("--master-sha", required=True)
    plan.add_argument("--run-id", required=True)
    plan.add_argument("--run-attempt", required=True)
    plan.add_argument("--repository", required=True)
    plan.add_argument("--workflow", required=True)
    plan.add_argument("--event", required=True)
    plan.add_argument("--control-sha", required=True)
    plan.add_argument("--baseline-anchor-release-id", type=int, required=True)
    plan.add_argument("--output", type=Path, required=True)
    plan.add_argument("--github-output", type=Path)
    evidence = sub.add_parser("evidence")
    evidence.add_argument("--plan", type=Path, required=True)
    evidence.add_argument("--assets", type=Path, required=True)
    evidence.add_argument("--output", type=Path, required=True)
    verify = sub.add_parser("verify-candidate")
    verify.add_argument("--directory", type=Path, required=True)
    manifests = sub.add_parser("release-manifests")
    manifests.add_argument("--candidate", type=Path, required=True)
    manifests.add_argument("--output", type=Path, required=True)
    verify_manifests = sub.add_parser("verify-release-manifests")
    verify_manifests.add_argument("--candidate", type=Path, required=True)
    verify_manifests.add_argument("--directory", type=Path, required=True)
    final = sub.add_parser("final-snapshot")
    final.add_argument("--candidate", type=Path, required=True)
    final.add_argument("--release-ids", type=Path, required=True)
    final.add_argument("--chart-digests", type=Path, required=True)
    final.add_argument("--tag-objects", type=Path, required=True)
    final.add_argument("--output", type=Path, required=True)
    image_version = sub.add_parser("image-version")
    image_version.add_argument("--values", type=Path, required=True)
    image_version.add_argument("--key", required=True)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    root = Path(__file__).resolve().parents[2]
    targets = load_json(root / "release" / "targets.json")
    try:
        validate_contract(targets, root)
        if args.command == "validate":
            print("release contract valid")
        elif args.command == "validate-jobs":
            plan = load_json(args.plan)
            needs = load_json(args.needs)
            results = validate_candidate_job_results(plan, needs)
            print("candidate validation results:", compact(results))
        elif args.command == "outputs":
            write_github_outputs(args.github_output, load_json(args.plan))
        elif args.command == "plan":
            try:
                baseline = release_baseline.resolve_latest(
                    targets, expected_anchor_id=args.baseline_anchor_release_id
                )
            except release_baseline.BaselineError as exc:
                raise ReleaseError(str(exc)) from exc
            plan = make_plan(
                targets,
                args.targets,
                args.master_sha,
                args.run_id,
                root,
                run_attempt=args.run_attempt,
                repository=args.repository,
                workflow=args.workflow,
                event=args.event,
                control_sha=args.control_sha,
                resolved_baseline=baseline,
                baseline_anchor_release_id=args.baseline_anchor_release_id,
            )
            args.output.write_text(compact(plan) + "\n", encoding="utf-8")
            print(compact(plan))
            output = args.github_output
            if output is None and os.environ.get("GITHUB_OUTPUT"):
                output = Path(os.environ["GITHUB_OUTPUT"])
            if output:
                write_github_outputs(output, plan)
        elif args.command == "evidence":
            evidence = assemble_evidence(load_json(args.plan), args.assets, targets)
            args.output.write_text(compact(evidence) + "\n", encoding="utf-8")
            print(compact(evidence))
        elif args.command == "verify-candidate":
            print(compact(verify_candidate(args.directory)))
        elif args.command == "release-manifests":
            print(compact([str(path) for path in write_release_manifests(args.candidate, args.output)]))
        elif args.command == "verify-release-manifests":
            print(compact(verify_release_manifests(args.candidate, args.directory)))
        elif args.command == "final-snapshot":
            release_ids = load_json(args.release_ids)
            chart_digests = load_json(args.chart_digests)
            tag_objects = load_json(args.tag_objects)
            final = release_baseline.final_snapshot(
                load_json(args.candidate / "candidate-snapshot.json"),
                targets,
                {name: int(value) for name, value in release_ids.items()},
                {name: str(value) for name, value in chart_digests.items()},
                {
                    name: {field: str(identity[field]) for field in ("sha", "target_sha")}
                    for name, identity in tag_objects.items()
                    if isinstance(identity, dict)
                },
            )
            args.output.write_text(compact(final) + "\n", encoding="utf-8")
            print(compact(final))
        elif args.command == "image-version":
            print(chart_image_version(args.values, args.key))
    except ReleaseError as exc:
        print(f"release contract error: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

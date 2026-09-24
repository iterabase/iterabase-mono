#!/usr/bin/env python3
"""Select monorepo CI owners from changed paths.

The selector is deliberately repository-owned and fixture-tested so changes to
fan-out semantics are reviewed alongside the workflows they control.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import re
import sys

OUTPUTS = (
    "control_plane",
    "ui",
    "harness",
    "tool_runner",
    "proto",
    "inference_gateway",
    "forge",
    "forge_e2e",
    "forge_real_e2e",
    "charts",
    "nested_go_lint",
    "images",
)

# Shared Go E2E modules have no product owner job of their own and the shared
# testkit is consumed by every owner suite, so one dedicated job lints all four
# with the repository-pinned linter. `.github/scripts/test_lint_parity.py` keeps
# this list, the root Makefile GO_MODULES matrix, and the reviewed owner
# authority in `.github/ci/go-lint-owners.json` in agreement.
NESTED_GO_LINT_MODULES = (
    "charts/test/e2e",
    "control-plane/test/e2e",
    "forge/test/e2e",
    "testkit/e2e",
)
NESTED_GO_LINT_MODULE_PREFIXES = tuple(f"{module}/" for module in NESTED_GO_LINT_MODULES)

DOC_NAMES = {
    "AGENTS.md",
    "CHANGELOG.md",
    "CODE_OF_CONDUCT.md",
    "CONTRIBUTING.md",
    "LICENSE",
    "LICENSE.md",
    "README.md",
    "SECURITY.md",
}

RELEASE_ONLY_PATHS = {
    "control-plane/VERSION",
    "inference-gateway/VERSION",
    "forge/VERSION",
    "release/targets.json",
    ".github/scripts/release.py",
    ".github/scripts/test_release.py",
    ".github/scripts/audit_release_security.sh",
    ".github/workflows/release.yml",
    ".github/workflows/release-candidate.yml",
    ".github/workflows/release-promote.yml",
    ".github/workflows/release-rehearsal.yml",
}
KNOWN_TOP_LEVEL = {
    ".github",
    ".githooks",
    "charts",
    "control-plane",
    "docs",
    "forge",
    "inference-gateway",
    "release",
    "testkit",
}
KNOWN_ROOT_FILES = {
    ".gitignore",
    ".gitleaks.toml",
    "AGENTS.md",
    "Makefile",
    "README.md",
    "go.work",
    "go.work.sum",
}
SHA = re.compile(r"^[0-9a-f]{40}$")


def validate_paths(paths: list[str], *, allow_empty: bool = False) -> list[str]:
    if not isinstance(paths, list) or any(not isinstance(path, str) for path in paths):
        raise ValueError("changed paths must be a list of strings")
    if not paths and not allow_empty:
        raise ValueError("empty changed-path input is ambiguous")
    result: list[str] = []
    for path in paths:
        pure = PurePosixPath(path)
        if (
            not path
            or path != path.strip()
            or path.startswith(("/", "./"))
            or "\\" in path
            or pure.is_absolute()
            or any(part in {"", ".", ".."} for part in pure.parts)
            or pure.as_posix() != path
        ):
            raise ValueError(f"changed path is not canonical: {path!r}")
        top = pure.parts[0]
        if len(pure.parts) == 1:
            if path not in KNOWN_ROOT_FILES:
                raise ValueError(f"unknown repository path: {path}")
        elif top not in KNOWN_TOP_LEVEL:
            raise ValueError(f"unknown repository path: {path}")
        result.append(path)
    if len(result) != len(set(result)):
        raise ValueError("changed paths contain duplicates")
    return result


def validate_selection_record(record: object, *, expected_head: str | None = None) -> tuple[list[str], bool]:
    expected_fields = {"schema_version", "event_name", "base_sha", "head_sha", "select_all", "paths"}
    if not isinstance(record, dict) or set(record) != expected_fields or record.get("schema_version") != 1:
        raise ValueError("changed-path selection record must be one exact schema-v1 object")
    event = record.get("event_name")
    head = record.get("head_sha")
    base = record.get("base_sha")
    select_all = record.get("select_all")
    paths = record.get("paths")
    if event not in {"pull_request", "push", "workflow_dispatch"}:
        raise ValueError("changed-path selection record has an invalid event")
    if not isinstance(head, str) or not SHA.fullmatch(head) or (expected_head is not None and head != expected_head):
        raise ValueError("changed-path selection record has an invalid head SHA")
    if not isinstance(base, str) or (base and not SHA.fullmatch(base)):
        raise ValueError("changed-path selection record has an invalid base SHA")
    if event == "pull_request" and (not base or set(base) == {"0"}):
        raise ValueError("pull_request selection requires a nonzero base SHA")
    if not isinstance(select_all, bool) or not isinstance(paths, list):
        raise ValueError("changed-path selection record is incomplete")
    if select_all != (event == "workflow_dispatch"):
        raise ValueError("only workflow_dispatch may request all-owner selection")
    return validate_paths(paths, allow_empty=select_all), select_all


def is_documentation(path: str) -> bool:
    parts = Path(path).parts
    return (
        path.endswith(".md")
        or (parts and parts[0] == "docs")
        or Path(path).name in DOC_NAMES
    )


def selection(paths: list[str], select_all: bool = False) -> dict[str, object]:
    normalized = validate_paths(paths, allow_empty=select_all)
    if select_all and normalized:
        raise ValueError("all-owner selection cannot also contain changed paths")
    selected = {name: select_all for name in OUTPUTS}
    selected_images: set[str] = set()

    if select_all:
        selected_images.update(
            {
                "control-plane",
                "control-plane-harness",
                "control-plane-harness-isolation",
                "control-plane-tool-runner",
                "inference-gateway",
            }
        )
    else:
        for path in normalized:
            if is_documentation(path):
                continue

            # Release implementation and component version metadata are
            # validated by the lightweight release-contract tests in the
            # selector job. They do not change product binaries or scenarios.
            if path in RELEASE_ONLY_PATHS:
                continue

            # Selection logic, shared setup/actions, and the PR/E2E workflow
            # implementation can invalidate every affected-target decision, so
            # changes to those contracts deliberately fan out to every owner.
            shared_ci = path.startswith(".github/actions/") or path in {
                ".github/ci/path-selection-fixtures.json",
                ".github/scripts/collect_changed_paths.py",
                ".github/scripts/select_ci.py",
                ".github/scripts/test_select_ci.py",
                ".github/scripts/test_cache_contract.py",
                ".github/scripts/e2e.py",
                ".github/scripts/test_e2e.py",
                ".github/workflows/ci.yml",
                ".github/workflows/e2e.yml",
            }
            # Unknown GitHub automation remains conservative, but release-only
            # files above no longer force unrelated images, Kind, CPU, or GPU.
            if shared_ci or path.startswith((".github/", ".githooks/")) or len(PurePosixPath(path).parts) == 1:
                for name in OUTPUTS:
                    selected[name] = True
                selected_images.update(
                    {
                        "control-plane",
                        "control-plane-harness",
                        "control-plane-harness-isolation",
                        "control-plane-tool-runner",
                        "inference-gateway",
                    }
                )
                continue

            # Shared E2E mechanics and catalogue discovery can invalidate every
            # owner and release-suite selection, so they deliberately fan out.
            if path.startswith("testkit/"):
                for name in OUTPUTS:
                    selected[name] = True
                selected_images.update(
                    {
                        "control-plane",
                        "control-plane-harness",
                        "control-plane-harness-isolation",
                        "control-plane-tool-runner",
                        "inference-gateway",
                    }
                )
                continue

            # Generated release records are evidence, not product inputs.
            if path.startswith("release/"):
                continue

            # A change confined to a nested Go E2E module must select its lint
            # owner even when the owning product component is otherwise
            # untouched, so compilation or tests alone never stand in for lint.
            if path.startswith(NESTED_GO_LINT_MODULE_PREFIXES):
                selected["nested_go_lint"] = True

            if path.startswith("control-plane/"):
                relative = path.removeprefix("control-plane/")
                is_ui = relative.startswith("ui/")
                is_harness = relative.startswith("harness/")
                is_tool_runner = relative.startswith("tool-runner/")
                is_component_makefile = relative == "Makefile"
                is_proto = relative.startswith("proto/") or relative in {
                    "buf.gen.yaml",
                    "buf.gateway.gen.yaml",
                    "buf.yaml",
                } or relative.startswith("internal/harnessrpc/") or relative.startswith(
                    "internal/gatewayrpc/"
                )

                selected["ui"] |= is_ui
                selected["harness"] |= is_harness or is_proto or is_component_makefile
                selected["tool_runner"] |= is_tool_runner or is_proto
                selected["proto"] |= is_proto
                selected["control_plane"] |= is_component_makefile or not (
                    is_harness or is_tool_runner
                )

                if is_ui or is_component_makefile or not (is_harness or is_tool_runner):
                    selected_images.add("control-plane")
                if is_harness:
                    selected_images.add("control-plane-harness")
                    if relative.startswith("harness/isolation/"):
                        selected_images.add("control-plane-harness-isolation")
                if is_tool_runner:
                    selected_images.add("control-plane-tool-runner")
                if is_proto:
                    selected_images.update(
                        {"control-plane-harness", "control-plane-tool-runner"}
                    )
                selected["images"] = bool(selected_images)
                affects_workspace_e2e = (
                    relative.startswith("harness/src/storage-health")
                    or relative.startswith("harness/src/main")
                    or relative.startswith("internal/controller/agentpool_storage")
                    or relative.startswith("internal/controller/agentpool_controller")
                    or relative.startswith("internal/dispatch/")
                    or is_proto
                )
                selected["forge_e2e"] |= affects_workspace_e2e
                selected["forge_real_e2e"] |= affects_workspace_e2e
                continue

            if path.startswith("inference-gateway/"):
                selected["inference_gateway"] = True
                selected["images"] = True
                selected_images.add("inference-gateway")
                continue

            if path.startswith("forge/"):
                selected["forge"] = True
                relative = path.removeprefix("forge/")
                affects_e2e = (
                    relative.startswith("cmd/")
                    or relative.startswith("internal/")
                    or relative.startswith("test/e2e/")
                    or relative in {"go.mod", "go.sum", "Makefile"}
                )
                selected["forge_e2e"] |= affects_e2e
                selected["forge_real_e2e"] |= affects_e2e
                continue

            if path.startswith("charts/"):
                selected["charts"] = True
                # Current deterministic Kind scenarios compose local charts.
                selected["forge_e2e"] = True
                continue

    selected["image_matrix"] = [
        {"name": name, **IMAGE_CONFIG[name]} for name in sorted(selected_images)
    ]
    selected["any"] = any(bool(selected[name]) for name in OUTPUTS)
    meaningful = [path for path in normalized if not is_documentation(path)]
    if select_all:
        classification = "all"
    elif not meaningful:
        classification = "docs-only"
    elif all(path in RELEASE_ONLY_PATHS or path.startswith("release/") for path in meaningful):
        classification = "release-only"
    elif selected["any"]:
        classification = "selected"
    else:
        raise ValueError(f"non-documentation paths have no owner: {meaningful}")
    selected["classification"] = classification
    selected["input_paths"] = normalized
    return selected


def validate_needs(result: dict[str, object], needs: dict[str, object]) -> None:
    expected_jobs = {
        "control-plane": bool(result["control_plane"]),
        "ui": bool(result["ui"]),
        "harness": bool(result["harness"]),
        "tool-runner": bool(result["tool_runner"]),
        "proto": bool(result["proto"]),
        "inference-gateway": bool(result["inference_gateway"]),
        "forge": bool(result["forge"]),
        "forge-fault-matrix": bool(result["forge"]),
        "charts": bool(result["charts"]),
        "nested-go-lint": bool(result["nested_go_lint"]),
        "images": bool(result["images"]),
    }
    if set(needs) != {"changes", *expected_jobs}:
        raise ValueError("CI aggregate needs set does not match the workflow contract")
    changes = needs.get("changes")
    if not isinstance(changes, dict) or changes.get("result") != "success":
        raise ValueError("CI selection job did not succeed")
    outputs = changes.get("outputs")
    if not isinstance(outputs, dict):
        raise ValueError("CI selection job has no outputs")
    for name in OUTPUTS:
        if outputs.get(name) != str(bool(result[name])).lower():
            raise ValueError(f"CI selector output {name} is missing or malformed")
    try:
        matrix = json.loads(str(outputs.get("image_matrix", "")))
    except json.JSONDecodeError as exc:
        raise ValueError("CI image matrix is malformed") from exc
    if matrix != result["image_matrix"]:
        raise ValueError("CI image matrix does not match the typed selection")
    if outputs.get("classification") != result["classification"]:
        raise ValueError("CI selection classification does not match")
    if outputs.get("selection") != json.dumps(result, sort_keys=True, separators=(",", ":")):
        raise ValueError("CI typed selection output does not match")
    for job, selected_job in expected_jobs.items():
        value = needs[job]
        status = value.get("result") if isinstance(value, dict) else None
        expected = "success" if selected_job else "skipped"
        if status != expected:
            raise ValueError(f"CI job {job} is {status!r}; expected {expected!r}")


IMAGE_CONFIG = {
    "control-plane": {
        "context": "control-plane",
        "dockerfile": "control-plane/Dockerfile",
    },
    "control-plane-harness": {
        "context": "control-plane/harness",
        "dockerfile": "control-plane/harness/Dockerfile",
    },
    "control-plane-harness-isolation": {
        "context": "control-plane/harness",
        "dockerfile": "control-plane/harness/isolation/Dockerfile",
    },
    "control-plane-tool-runner": {
        "context": "control-plane/tool-runner",
        "dockerfile": "control-plane/tool-runner/Dockerfile",
    },
    "inference-gateway": {
        "context": "inference-gateway",
        "dockerfile": "inference-gateway/Dockerfile",
    },
}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("paths", nargs="*")
    parser.add_argument("--selection-file", type=Path)
    parser.add_argument("--selection-result-file", type=Path)
    parser.add_argument("--all", action="store_true", dest="select_all")
    parser.add_argument("--github-output", type=Path)
    parser.add_argument("--expected-head", default="")
    parser.add_argument("--validate-needs-env", default="")
    args = parser.parse_args()

    paths = list(args.paths)
    select_all = args.select_all
    selection_record = None
    supplied_result = None
    if args.selection_result_file:
        if paths or select_all or args.selection_file:
            parser.error("--selection-result-file cannot be combined with another selection input")
        supplied_result = json.loads(args.selection_result_file.read_text(encoding="utf-8"))
        if not isinstance(supplied_result, dict) or not isinstance(supplied_result.get("input_paths"), list):
            raise ValueError("typed CI selection result is incomplete")
        paths = supplied_result["input_paths"]
        select_all = supplied_result.get("classification") == "all"
    if args.selection_file:
        if paths or select_all:
            parser.error("--selection-file cannot be combined with paths or --all")
        selection_record = json.loads(args.selection_file.read_text(encoding="utf-8"))
        paths, select_all = validate_selection_record(
            selection_record, expected_head=args.expected_head or None
        )

    result = selection(paths, select_all)
    if supplied_result is not None and supplied_result != result:
        raise ValueError("typed CI selection result does not match its path classification")
    rendered = json.dumps(result, sort_keys=True)
    print(rendered)
    if args.validate_needs_env:
        try:
            needs = json.loads(os.environ.get(args.validate_needs_env, ""))
        except json.JSONDecodeError as exc:
            raise ValueError("CI aggregate needs are not valid JSON") from exc
        if not isinstance(needs, dict):
            raise ValueError("CI aggregate needs must be an object")
        validate_needs(result, needs)

    output_path = args.github_output
    if output_path is None and os.environ.get("GITHUB_OUTPUT"):
        output_path = Path(os.environ["GITHUB_OUTPUT"])
    if output_path:
        with output_path.open("a", encoding="utf-8") as output:
            output.write(f"selection={json.dumps(result, sort_keys=True, separators=(',', ':'))}\n")
            if selection_record is not None:
                output.write(f"path_record={json.dumps(selection_record, sort_keys=True, separators=(',', ':'))}\n")
            for name, value in result.items():
                if name in {"input_paths", "selection"}:
                    continue
                if isinstance(value, bool):
                    value = str(value).lower()
                elif not isinstance(value, str):
                    value = json.dumps(value, separators=(",", ":"))
                output.write(f"{name}={value}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())

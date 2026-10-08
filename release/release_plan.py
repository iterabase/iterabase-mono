#!/usr/bin/env python3
"""Plan one official release of an explicit target set at one commit (C7, C10).

The source tree at the release commit pins the composition: every member of the
set must carry a version that is not yet published, and every version a member
references in another target must either be released in the same set or
already be published. A missing reference fails and names the target; the set
is never expanded silently.

Usage: release_plan.py --targets control-plane,iterabase-platform-chart [--published-tags FILE]
"""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import subprocess
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHARTS = ROOT / "charts" / "charts"
OFFICIAL_IMAGES = "ghcr.io/iterabase"
OFFICIAL_CHARTS = "oci://ghcr.io/iterabase/iterabase-charts"


class ReleasePlanError(RuntimeError):
    """The requested release cannot be published exactly as asked."""


def chart_field(chart: str, field: str, root: pathlib.Path = ROOT) -> str:
    text = (root / "charts" / "charts" / chart / "Chart.yaml").read_text(encoding="utf-8")
    match = re.search(rf'^{field}:\s*"?([^"\n]+?)"?\s*$', text, re.MULTILINE)
    if not match:
        raise ReleasePlanError(f"charts/charts/{chart}/Chart.yaml has no {field}")
    return match.group(1)


def platform_dependency(chart: str, root: pathlib.Path = ROOT) -> str:
    text = (root / "charts" / "charts" / "iterabase-platform" / "Chart.yaml").read_text(encoding="utf-8")
    match = re.search(rf"^  - name: {re.escape(chart)}\n    version: (\S+)$", text, re.MULTILINE)
    if not match:
        raise ReleasePlanError(f"iterabase-platform does not depend on {chart}")
    return match.group(1)


def target_version(name: str, target: dict[str, Any], root: pathlib.Path = ROOT) -> str:
    if target.get("version_file"):
        return (root / target["version_file"]).read_text(encoding="utf-8").strip()
    return chart_field(target["chart"], "version", root)


def references(name: str, root: pathlib.Path = ROOT) -> list[tuple[str, str]]:
    """(target, version) pairs a target's artifacts name in other targets."""
    if name == "control-plane-chart":
        return [("control-plane", chart_field("control-plane", "appVersion", root))]
    if name == "inference-gateway-chart":
        return [("inference-gateway", chart_field("inference-gateway", "appVersion", root))]
    if name == "iterabase-platform-chart":
        return [("control-plane-chart", platform_dependency("control-plane", root)),
                ("inference-gateway-chart", platform_dependency("inference-gateway", root))]
    return []


def plan(selected: list[str], contract: dict[str, Any], published_tags: set[str], sha: str,
         root: pathlib.Path = ROOT) -> list[dict[str, Any]]:
    targets = contract["targets"]
    unknown = sorted(set(selected) - set(targets))
    if unknown or not selected:
        raise ReleasePlanError(f"unknown or empty target set: {', '.join(unknown) or '(none)'}")
    versions = {name: target_version(name, targets[name], root) for name in targets}
    tags = {name: f"{targets[name]['tag_prefix']}{versions[name]}" for name in targets}
    errors: list[str] = []
    for name in selected:
        if tags[name] in published_tags:
            errors.append(f"{name} {versions[name]} is already published as {tags[name]}; bump it (make bump TARGET={name})")
    for name in selected:
        for referenced, version in references(name, root):
            if referenced in selected and versions[referenced] == version:
                continue
            if f"{targets[referenced]['tag_prefix']}{version}" in published_tags:
                continue
            errors.append(f"{name} references {referenced} {version}, which is neither published nor in this release; "
                          f"add {referenced} to the target set or publish it first")
    if errors:
        raise ReleasePlanError("; ".join(errors))
    recipes = contract["artifact_recipes"]
    members = []
    for name in sorted(selected):
        artifacts = [{"recipe": recipe, "kind": recipes[recipe]["kind"],
                      **({"image": recipes[recipe]["name"]} if recipes[recipe]["kind"] == "image" else {}),
                      **({"chart": recipes[recipe]["chart"]} if "chart" in recipes[recipe] else {})}
                     for recipe in targets[name]["artifacts"]]
        members.append({"target": name, "version": versions[name], "tag": tags[name], "sha": sha,
                        "latest": name == "iterabase-platform-chart", "artifacts": artifacts})
    return members


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--targets", required=True, help="comma-separated release targets")
    parser.add_argument("--sha", required=True)
    parser.add_argument("--published-tags", help="file of published tags (default: git ls-remote --tags origin)")
    args = parser.parse_args(argv)
    if args.published_tags:
        published = set(pathlib.Path(args.published_tags).read_text(encoding="utf-8").split())
    else:
        output = subprocess.run(["git", "ls-remote", "--tags", "--refs", "origin"], cwd=ROOT, check=True,
                                capture_output=True, text=True).stdout
        published = {line.split("refs/tags/", 1)[1] for line in output.splitlines() if "refs/tags/" in line}
    contract = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))
    try:
        members = plan([target.strip() for target in args.targets.split(",") if target.strip()], contract, published, args.sha)
    except ReleasePlanError as error:
        print(f"release plan: {error}", file=sys.stderr)
        return 1
    print(json.dumps(members))
    return 0


if __name__ == "__main__":
    sys.exit(main())

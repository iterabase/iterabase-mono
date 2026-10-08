#!/usr/bin/env python3
"""Bump one release target and every field linked to its version (C7).

The source tree at a release SHA pins the composition, so a version change
must move every field that names it in the same commit:

  control-plane / inference-gateway   <component>/VERSION and the chart appVersion
  forge                               forge/VERSION
  <component>-chart                   the chart version and the platform dependency
  iterabase-platform-chart            platform version/appVersion and both substrates

Usage: make bump TARGET=<target> VERSION=<x.y.z>
"""
from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
CHARTS = ROOT / "charts" / "charts"
SEMVER = re.compile(r"^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[0-9A-Za-z.-]+)?$")
SUBSTRATES = ("cert-manager-substrate", "lvm-storage-substrate")


class BumpError(RuntimeError):
    """A bump that cannot be applied exactly."""


def replace_one(path: pathlib.Path, pattern: str, replacement: str) -> None:
    text = path.read_text(encoding="utf-8")
    updated, count = re.subn(pattern, replacement, text, count=1, flags=re.MULTILINE)
    if count != 1:
        raise BumpError(f"{path.relative_to(ROOT)}: expected exactly one match for {pattern!r}")
    path.write_text(updated, encoding="utf-8")


def set_chart_field(chart: str, field: str, version: str) -> None:
    replace_one(CHARTS / chart / "Chart.yaml", rf'^{field}: .*$', f'{field}: "{version}"' if field == "appVersion" else f"{field}: {version}")


def set_platform_dependency(chart: str, version: str) -> None:
    replace_one(
        CHARTS / "iterabase-platform" / "Chart.yaml",
        rf"^(  - name: {re.escape(chart)}\n    version: ).*$",
        rf"\g<1>{version}",
    )


def bump(target: str, version: str) -> list[str]:
    """Apply the bump and return the repository-relative files it changed."""
    if not SEMVER.match(version):
        raise BumpError(f"version {version!r} is not SemVer x.y.z")
    targets = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))["targets"]
    if target not in targets:
        raise BumpError(f"unknown target {target!r}; known: {', '.join(sorted(targets))}")
    changed: list[pathlib.Path] = []
    version_file = targets[target].get("version_file")
    if version_file:
        path = ROOT / version_file
        path.write_text(version + "\n", encoding="utf-8")
        changed.append(path)
        if (CHARTS / target / "Chart.yaml").exists():
            set_chart_field(target, "appVersion", version)
            changed.append(CHARTS / target / "Chart.yaml")
    elif target == "iterabase-platform-chart":
        set_chart_field("iterabase-platform", "version", version)
        set_chart_field("iterabase-platform", "appVersion", version)
        changed.append(CHARTS / "iterabase-platform" / "Chart.yaml")
        for substrate in SUBSTRATES:
            set_chart_field(substrate, "version", version)
            changed.append(CHARTS / substrate / "Chart.yaml")
    else:
        chart = target.removesuffix("-chart")
        set_chart_field(chart, "version", version)
        set_platform_dependency(chart, version)
        changed += [CHARTS / chart / "Chart.yaml", CHARTS / "iterabase-platform" / "Chart.yaml"]
    return sorted({str(path.relative_to(ROOT)) for path in changed})


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("target")
    parser.add_argument("version")
    args = parser.parse_args(argv)
    try:
        for path in bump(args.target, args.version):
            print(path)
    except BumpError as error:
        print(f"bump: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

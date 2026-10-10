#!/usr/bin/env python3
"""Prune preview package versions (C9).

Preview images are tagged `sha-<commit>` and preview charts
`<version>-pr.<N>.<run>` or `<version>-main.<run>` under
ghcr.io/iterabase/preview/*, which official identities never share, so this
cannot reach an official artifact. Rules:

  - a pull-request chart goes when its pull request is closed or it is older
    than 14 days;
  - staging keeps its newest 10 `-main.` charts;
  - an image goes when it is older than 14 days, unless its commit is an open
    pull request's head or one of master's last 10 commits.

The packages are named from the build recipes rather than listed: the job's
repository token can manage these repository-linked packages but may not list
the organisation's packages.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import pathlib
import re
import subprocess
import sys
import urllib.parse
from typing import Any

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import preview  # noqa: E402

ORG = "iterabase"
PREFIX = "preview/"
TARGETS = pathlib.Path(__file__).resolve().parents[2] / "release" / "targets.json"
MAX_AGE = dt.timedelta(days=14)
STAGING_KEPT = 10
PR_CHART = re.compile(r"^\d+\.\d+\.\d+-pr\.(\d+)\.\d+$")
MAIN_CHART = re.compile(r"^\d+\.\d+\.\d+-main\.\d+$")
IMAGE = re.compile(r"^sha-([0-9a-f]{40})$")


def plan(versions: list[dict[str, Any]], *, open_prs: set[int], keep_commits: set[str], now: dt.datetime) -> list[dict[str, Any]]:
    """The package versions to delete, each with its reason."""
    deletions: list[dict[str, Any]] = []
    main_charts = sorted((version for version in versions
                          if any(MAIN_CHART.match(tag) for tag in version["tags"])),
                         key=lambda version: version["created_at"], reverse=True)
    stale_main = {id(version) for version in main_charts[STAGING_KEPT:]}
    for version in versions:
        age = now - dt.datetime.fromisoformat(version["created_at"].replace("Z", "+00:00"))
        reason = ""
        for tag in version["tags"]:
            pr = PR_CHART.match(tag)
            image = IMAGE.match(tag)
            if pr and int(pr.group(1)) not in open_prs:
                reason = f"pull request {pr.group(1)} is closed"
            elif pr and age > MAX_AGE:
                reason = "older than 14 days"
            elif MAIN_CHART.match(tag) and id(version) in stale_main:
                reason = f"beyond staging's newest {STAGING_KEPT}"
            elif image and image.group(1) not in keep_commits and age > MAX_AGE:
                reason = "older than 14 days and not a live head"
        if reason:
            deletions.append({**version, "reason": reason})
    return deletions


def package_names(recipes: dict[str, dict[str, Any]]) -> list[str]:
    """Every preview package CI publishes: each image recipe and each preview chart."""
    images = sorted(recipe["name"] for recipe in recipes.values() if recipe["kind"] == "image")
    return [f"{PREFIX}{name}" for name in images] + [f"{PREFIX}charts/{chart}" for chart in preview.PREVIEW_CHART_NAMES]


class NotFound(Exception):
    pass


def gh(*args: str) -> Any:
    result = subprocess.run(["gh", "api", *args], capture_output=True, text=True)
    if result.returncode != 0:
        if "HTTP 404" in result.stderr:
            raise NotFound(args[-1])
        sys.exit(f"gh api {' '.join(args)} failed: {result.stderr.strip()}")
    return json.loads(result.stdout or "null")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--repository", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    pulls = gh("--paginate", f"repos/{args.repository}/pulls?state=open&per_page=100")
    open_prs = {pull["number"] for pull in pulls}
    keep = {pull["head"]["sha"] for pull in pulls}
    keep |= {commit["sha"] for commit in gh(f"repos/{args.repository}/commits?sha=master&per_page={STAGING_KEPT}")}
    packages = package_names(json.loads(TARGETS.read_text(encoding="utf-8"))["artifact_recipes"])
    now = dt.datetime.now(dt.timezone.utc)
    removed, skipped = [], []
    for package in packages:
        encoded = urllib.parse.quote(package, safe="")
        try:
            listed = gh("--paginate", f"orgs/{ORG}/packages/container/{encoded}/versions?per_page=100")
        except NotFound:
            # GHCR answers 404 both for a package never published and for one
            # this token cannot see, so a skip is always reported.
            skipped.append(f"{package}: not found or not accessible, skipped")
            continue
        versions = [{"id": version["id"], "created_at": version["created_at"],
                     "tags": version["metadata"]["container"]["tags"]} for version in listed]
        for version in plan(versions, open_prs=open_prs, keep_commits=keep, now=now):
            removed.append(f"{package} {','.join(version['tags']) or version['id']}: {version['reason']}")
            if not args.dry_run:
                gh("-X", "DELETE", f"orgs/{ORG}/packages/container/{encoded}/versions/{version['id']}")
    print("\n".join(removed + skipped) or "nothing to prune")
    if len(skipped) == len(packages):
        # Staging always publishes these packages, so seeing none is a token or
        # naming regression, never an empty registry.
        print("::error::no preview package was found or accessible", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Prepare one E2E scenario's inputs from the build-once artifacts (C3).

Images come from the preview registry by digest, exactly as the build job pushed
them, and are saved as archives so Kind and the F3 hosts load them without
registry credentials. Charts are the source tree's own (the composition the
source pins, C7), Forge is built from source, and the n-1-upgrade baseline is
the newest published platform-chart Release's immutable archives.

Writes KEY=value lines for $GITHUB_ENV; the owner suites read exactly these.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[2]
CHARTS = ROOT / "charts" / "charts"
IMAGE_PREFIX = {
    "control-plane-image": "CONTROL_PLANE",
    "harness-image": "HARNESS",
    "tool-runner-image": "TOOL_RUNNER",
    "inference-gateway-image": "INFERENCE_GATEWAY",
    "runtime-fixture-image": "FORGE_E2E_RUNTIME",
}
FORGE_ARCHIVE_ALIAS = {
    "control-plane-image": "FORGE_E2E_CONTROL_PLANE_IMAGE_ARCHIVE",
    "harness-image": "FORGE_E2E_HARNESS_IMAGE_ARCHIVE",
    "tool-runner-image": "FORGE_E2E_TOOL_RUNNER_IMAGE_ARCHIVE",
    "inference-gateway-image": "FORGE_E2E_INFERENCE_IMAGE_ARCHIVE",
    "runtime-fixture-image": "FORGE_E2E_RUNTIME_IMAGE_ARCHIVE",
}
FORGE_CHART_ARCHIVE = {
    "iterabase-platform": "FORGE_E2E_PLATFORM_CHART_ARCHIVE",
    "cert-manager-substrate": "FORGE_E2E_SUBSTRATE_CHART_ARCHIVE",
    "lvm-storage-substrate": "FORGE_E2E_LVM_STORAGE_CHART_ARCHIVE",
}
N1_CHARTS = {"PLATFORM": "iterabase-platform", "CERT_MANAGER": "cert-manager-substrate", "LVM_STORAGE": "lvm-storage-substrate"}
N1_NAMESPACES = ("ghcr.io/iterabase/iterabase-charts", "ghcr.io/nunocgoncalves/iterabase-charts")
SEMVER = re.compile(r"^\d+\.\d+\.\d+$")
PREVIEW_REGISTRY = "ghcr.io/iterabase/preview"


class InputsError(RuntimeError):
    """An input the scenario needs could not be prepared exactly."""


def run(*args: str, cwd: pathlib.Path = ROOT) -> str:
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


def scenario_metadata(catalogue: dict[str, Any], scenario_id: str) -> dict[str, Any]:
    for suite in catalogue["suites"]:
        for scenario in suite["scenarios"]:
            if scenario["id"] == scenario_id:
                return scenario["metadata"]
    raise InputsError(f"scenario {scenario_id} is not in the catalogue")


def image_inputs(name: str, image: dict[str, str], source_sha: str, workdir: pathlib.Path) -> dict[str, str]:
    """Pull one built image by digest, tag it, save it, and report its identities."""
    repository, tag, digest = image["repository"], image["tag"], image["digest"]
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest) or tag != f"sha-{source_sha}":
        raise InputsError(f"{name} build output is not an exact sha-{source_sha} digest: {image}")
    run("docker", "pull", "--quiet", f"{repository}@{digest}")
    run("docker", "tag", f"{repository}@{digest}", f"{repository}:{tag}")
    archive = workdir / f"{name}.tar"
    run("docker", "save", "--output", str(archive), f"{repository}:{tag}")
    config_digest = run("docker", "image", "inspect", "--format", "{{.Id}}", f"{repository}:{tag}")
    prefix = IMAGE_PREFIX[name]
    return {
        f"{prefix}_IMAGE_REPO": repository,
        f"{prefix}_IMAGE_TAG": tag,
        f"{prefix}_IMAGE_DIGEST": digest,
        f"{prefix}_IMAGE_CONFIG_DIGEST": config_digest,
        f"{prefix}_IMAGE_SOURCE_SHA": source_sha,
        f"{prefix}_IMAGE_ARCHIVE": str(archive),
        FORGE_ARCHIVE_ALIAS[name]: str(archive),
    }


OFFICIAL_NAMESPACES = ("ghcr.io/iterabase", "ghcr.io/nunocgoncalves")


def official_image_inputs(name: str, workdir: pathlib.Path) -> dict[str, str]:
    """A non-released target's already-published official image (C7 release validation)."""
    contract = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))
    recipe = contract["artifact_recipes"][name]
    version = (ROOT / contract["targets"][recipe["target"]]["version_file"]).read_text(encoding="utf-8").strip()
    for namespace in OFFICIAL_NAMESPACES:
        repository = f"{namespace}/{recipe['name']}"
        if subprocess.run(["docker", "pull", "--quiet", f"{repository}:{version}"], capture_output=True).returncode == 0:
            break
    else:
        raise InputsError(f"{recipe['name']} {version} is not published; release its target in this set")
    digest = run("docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", f"{repository}:{version}").split("@", 1)[1]
    revision = run("docker", "image", "inspect", "--format",
                   '{{index .Config.Labels "org.opencontainers.image.revision"}}', f"{repository}:{version}")
    archive = workdir / f"{name}.tar"
    run("docker", "save", "--output", str(archive), f"{repository}:{version}")
    prefix = IMAGE_PREFIX[name]
    return {
        f"{prefix}_IMAGE_REPO": repository,
        f"{prefix}_IMAGE_TAG": version,
        f"{prefix}_IMAGE_DIGEST": digest,
        f"{prefix}_IMAGE_CONFIG_DIGEST": run("docker", "image", "inspect", "--format", "{{.Id}}", f"{repository}:{version}"),
        f"{prefix}_IMAGE_SOURCE_SHA": revision,
        f"{prefix}_IMAGE_ARCHIVE": str(archive),
        FORGE_ARCHIVE_ALIAS[name]: str(archive),
    }


def released(name: str, release_targets: set[str]) -> bool:
    """Whether an image comes from this commit's build: always, unless validating a release."""
    if not release_targets:
        return True
    contract = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))
    target = contract["artifact_recipes"][name].get("target")
    return target is None or target in release_targets


def chart_version(chart: str) -> str:
    match = re.search(r"^version:\s*(\S+)\s*$", (CHARTS / chart / "Chart.yaml").read_text(encoding="utf-8"), re.MULTILINE)
    if not match:
        raise InputsError(f"{chart} Chart.yaml has no version")
    return match.group(1)


def chart_inputs(forge: bool, workdir: pathlib.Path) -> dict[str, str]:
    """The source tree's charts, dependencies built once; packaged for Forge hosts."""
    run("make", "-C", "charts", "build-deps")
    env = {
        "ITERABASE_PLATFORM_LOCAL_CHART": str(CHARTS / "iterabase-platform"),
        "ITERABASE_CHART_VERSION": chart_version("iterabase-platform"),
    }
    if forge:
        for chart, key in FORGE_CHART_ARCHIVE.items():
            run("helm", "package", str(CHARTS / chart), "--destination", str(workdir))
            env[key] = str(workdir / f"{chart}-{chart_version(chart)}.tgz")
    return env


def newest_platform_release() -> str:
    """The highest published iterabase-platform chart version, from its Release tags."""
    tags = run("gh", "release", "list", "--limit", "200", "--json", "tagName", "--jq", ".[].tagName").splitlines()
    versions = [tag.removeprefix("iterabase-platform-") for tag in tags if tag.startswith("iterabase-platform-")]
    versions = [version for version in versions if SEMVER.match(version)]
    if not versions:
        raise InputsError("no published iterabase-platform release exists for n-1-upgrade")
    return max(versions, key=lambda version: tuple(int(part) for part in version.split(".")))


def n1_inputs(workdir: pathlib.Path) -> dict[str, str]:
    """The newest platform Release's archives, hashed from the immutable Release assets."""
    version = newest_platform_release()
    tag = f"iterabase-platform-{version}"
    target = workdir / "n-1"
    target.mkdir(parents=True, exist_ok=True)
    run("gh", "release", "download", tag, "--dir", str(target), "--clobber",
        *[arg for chart in N1_CHARTS.values() for arg in ("--pattern", f"{chart}-{version}.tgz")])
    namespace = next((namespace for namespace in N1_NAMESPACES if subprocess.run(
        ["helm", "show", "chart", f"oci://{namespace}/iterabase-platform", "--version", version],
        capture_output=True).returncode == 0), "")
    if not namespace:
        raise InputsError(f"iterabase-platform {version} is in no published chart namespace")
    env: dict[str, str] = {}
    for key, chart in N1_CHARTS.items():
        archive = target / f"{chart}-{version}.tgz"
        env[f"ITERABASE_E2E_N1_{key}_REFERENCE"] = f"oci://{namespace}/{chart}:{version}"
        env[f"ITERABASE_E2E_N1_{key}_SHA256"] = hashlib.sha256(archive.read_bytes()).hexdigest()
        env[f"ITERABASE_E2E_N1_{key}_ARCHIVE"] = str(archive)
    return env


def prepare(scenario_id: str, catalogue: dict[str, Any], images: dict[str, dict[str, str]],
            source_sha: str, optional_stages: str, workdir: pathlib.Path,
            release_targets: set[str] | None = None) -> dict[str, str]:
    metadata = scenario_metadata(catalogue, scenario_id)
    required = set(metadata.get("required_artifacts", ()))
    env = {
        "ITERABASE_E2E_FIXTURE_MODE": "source",
        "ITERABASE_E2E_SOURCE_SHA": source_sha,
        "ITERABASE_E2E_SOURCE_DIRTY": "false",
        "ITERABASE_E2E_REQUIRED": "true",
        "ITERABASE_E2E_OPTIONAL_STAGES": optional_stages,
    }
    for name in sorted(required & set(IMAGE_PREFIX)):
        if not released(name, release_targets or set()):
            env.update(official_image_inputs(name, workdir))
            continue
        if name not in images:
            raise InputsError(f"{scenario_id} requires {name}, which the build job did not produce")
        env.update(image_inputs(name, images[name], source_sha, workdir))
    forge = "forge-binary" in required
    if required & {"iterabase-platform-chart", "control-plane-chart", "inference-gateway-chart"}:
        env.update(chart_inputs(forge, workdir))
    if forge:
        run("make", "-C", "forge", "build")
        env["FORGE_E2E_BINARY"] = str(ROOT / "forge" / "bin" / "forge")
    if scenario_id == "charts/n-1-upgrade":
        env.update(n1_inputs(workdir))
    return env


def build_matrix(source_sha: str) -> list[dict[str, str]]:
    """Every image recipe, built once per commit into the preview namespace (C3, C9)."""
    contract = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))
    matrix = []
    for name, recipe in sorted(contract["artifact_recipes"].items()):
        if recipe.get("kind") != "image":
            continue
        target = contract["targets"].get(recipe.get("target", ""), {})
        version_file = target.get("version_file")
        version = (ROOT / version_file).read_text(encoding="utf-8").strip() if version_file else "0.0.0"
        matrix.append({
            "name": name, "image": recipe["name"], "context": recipe["context"], "dockerfile": recipe["dockerfile"],
            "repository": f"{PREVIEW_REGISTRY}/{recipe['name']}", "tag": f"sha-{source_sha}", "version": version,
        })
    return matrix


def plan(selection: dict[str, Any], catalogue: dict[str, Any], source_sha: str) -> dict[str, Any]:
    """Split the selected scenarios into Kind (F2) and real-machine (F3) matrices."""
    kind, real = [], []
    for scenario_id in selection["scenarios"]:
        meta = scenario_metadata(catalogue, scenario_id)
        owner = scenario_id.split("/", 1)[0]
        entry = {"id": scenario_id, "name": scenario_id.replace("/", "-"), "owner": owner,
                 "make_target": meta["make_target"], "timeout": int(meta["timeout_minutes"]) + 10}
        if meta["tier"] == "F3":
            real.append({**entry, "capacity": meta["capacity"]})
        else:
            kind.append(entry)
    builds = build_matrix(source_sha) if selection["artifacts"] or kind or real else []
    return {"build": builds, "kind": kind, "real": real}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    commands = parser.add_subparsers(dest="command", required=True)
    planner = commands.add_parser("plan", help="emit the build, Kind and real-machine matrices")
    planner.add_argument("--selection", required=True)
    planner.add_argument("--catalogue", required=True)
    planner.add_argument("--source-sha", required=True)
    prepare_parser = commands.add_parser("prepare", help="prepare one scenario's inputs")
    prepare_parser.add_argument("--scenario", required=True)
    prepare_parser.add_argument("--catalogue", required=True)
    prepare_parser.add_argument("--images", required=True, help="JSON {recipe: {repository, tag, digest}}")
    prepare_parser.add_argument("--source-sha", required=True)
    prepare_parser.add_argument("--optional-stages", default="")
    prepare_parser.add_argument("--workdir", required=True)
    prepare_parser.add_argument("--release-targets", default="",
                                help="validating a release: images of other targets come from their published versions")
    prepare_parser.add_argument("--env-output", default=os.environ.get("GITHUB_ENV", ""))
    args = parser.parse_args(argv)
    if not re.fullmatch(r"[0-9a-f]{40}", args.source_sha):
        print(f"e2e inputs: source sha {args.source_sha!r} is not a full commit", file=sys.stderr)
        return 1
    catalogue = json.loads(pathlib.Path(args.catalogue).read_text(encoding="utf-8"))
    if args.command == "plan":
        result = plan(json.loads(pathlib.Path(args.selection).read_text(encoding="utf-8")), catalogue, args.source_sha)
        print(json.dumps(result))
        return 0
    workdir = pathlib.Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)
    try:
        release_targets = {target.strip() for target in args.release_targets.split(",") if target.strip()}
        env = prepare(args.scenario, catalogue, json.loads(args.images), args.source_sha, args.optional_stages, workdir,
                      release_targets)
    except (InputsError, subprocess.CalledProcessError) as error:
        detail = getattr(error, "stderr", "") or ""
        print(f"e2e inputs: {error}\n{detail}", file=sys.stderr)
        return 1
    lines = "".join(f"{key}={value}\n" for key, value in sorted(env.items()))
    if args.env_output:
        with open(args.env_output, "a", encoding="utf-8") as handle:
            handle.write(lines)
    print(lines, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())

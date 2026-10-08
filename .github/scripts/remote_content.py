#!/usr/bin/env python3
"""Validate and materialize repository-reviewed remote content identities."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from typing import Any, Callable
from urllib.error import URLError
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parents[2]
MANIFEST = ROOT / ".github/inputs/remote-content.json"
SHA256 = re.compile(r"^[0-9a-f]{64}$")
ACTION = re.compile(r"^\s*-?\s*uses:\s*([^\s#]+)")
FROM = re.compile(r"^\s*FROM\s+([^\s]+)", re.IGNORECASE)
SYNTAX = re.compile(r"^#\s*syntax=([^\s]+)")
CONTENT_DIGEST = re.compile(
    r"(?:sha256:|(?:digest|sha|sha256)\s*[:=]\s*[\"']?)([0-9a-f]{64})",
    re.IGNORECASE,
)
QUOTED_IMAGE = re.compile(
    r"[\"']((?:(?:[a-z0-9.-]+(?::[0-9]+)?/)?[a-z0-9._-]+/[a-z0-9._/-]+|(?:alpine|busybox|debian|postgres|redis)):[A-Za-z0-9_.+-]+(?:@sha256:[0-9a-f]{64})?)[\"']"
)
YAML_KEY = re.compile(r"^(\s*)([A-Za-z0-9_.-]+):(?:\s*(.*))?$")
PRODUCT_IMAGE_PREFIXES = (
    "ghcr.io/iterabase/",
    "ghcr.io/nunocgoncalves/",
    "docker.io/iterabase-e2e/",
    "iterabase-e2e/",
    "control-plane:",
    "control-plane-harness:",
    "control-plane-tool-runner:",
    "inference-gateway:",
)


class ContentError(ValueError):
    """Remote content authority is incomplete or inconsistent."""


def load_manifest(path: Path = MANIFEST) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ContentError(f"cannot read content manifest {path}: {exc}") from exc
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        raise ContentError("remote content manifest must be a schema-v1 object")
    return value


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def require_records(manifest: dict[str, Any], field: str, keys: tuple[str, ...]) -> list[dict[str, str]]:
    records = manifest.get(field)
    if not isinstance(records, list) or not records:
        raise ContentError(f"content manifest {field} must be a non-empty list")
    result: list[dict[str, str]] = []
    seen: set[tuple[str, ...]] = set()
    for record in records:
        if not isinstance(record, dict) or any(not isinstance(record.get(key), str) or not record[key] for key in keys):
            raise ContentError(f"content manifest {field} contains an incomplete record")
        identity_keys = tuple(
            key for key in keys if key not in {"url", "sha256", "digest", "installed_sha256"}
        ) or keys[:1]
        identity = tuple(record[key] for key in identity_keys)
        if identity in seen:
            raise ContentError(f"content manifest {field} duplicates {identity}")
        seen.add(identity)
        checksum = record.get("sha256") or str(record.get("digest", "")).removeprefix("sha256:")
        if not SHA256.fullmatch(checksum):
            raise ContentError(f"content manifest {field} has a non-canonical content digest")
        for key, value in record.items():
            if key.endswith("sha256") and (not isinstance(value, str) or not SHA256.fullmatch(value)):
                raise ContentError(f"content manifest {field} has a non-canonical {key}")
        url = record.get("url")
        if url is not None and not url.startswith("https://"):
            raise ContentError(f"content manifest {field} URL must use HTTPS")
        result.append(record)
    return result


def require_locked_tools(manifest: dict[str, Any]) -> list[dict[str, str]]:
    records = manifest.get("locked_tools")
    if not isinstance(records, list) or not records:
        raise ContentError("content manifest locked_tools must be a non-empty list")
    result: list[dict[str, str]] = []
    names: set[str] = set()
    for record in records:
        if not isinstance(record, dict):
            raise ContentError("content manifest locked_tools contains a non-object")
        ecosystem = record.get("ecosystem")
        required = {"name", "ecosystem", "package", "version", "lock"}
        if ecosystem == "go":
            required.add("module")
        elif ecosystem != "npm":
            raise ContentError(f"unsupported locked tool ecosystem {ecosystem!r}")
        if set(record) != required or any(not isinstance(record[key], str) or not record[key] for key in required):
            raise ContentError("content manifest locked_tools contains an incomplete or extra field")
        if record["name"] in names:
            raise ContentError(f"content manifest locked_tools duplicates {record['name']}")
        names.add(record["name"])
        result.append(record)
    return result


def go_mod_requirements(path: Path) -> dict[str, str]:
    requirements: dict[str, str] = {}
    in_block = False
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.split("//", 1)[0].strip()
        if line == "require (":
            in_block = True
            continue
        if in_block and line == ")":
            in_block = False
            continue
        if line.startswith("require "):
            parts = line.removeprefix("require ").split()
        elif in_block:
            parts = line.split()
        else:
            continue
        if len(parts) == 2:
            module, version = parts
            if module in requirements:
                raise ContentError(f"Go tool module duplicates {module} in {path}")
            requirements[module] = version
    return requirements


def go_tool_installers(path: Path) -> dict[str, dict[str, str]]:
    result: dict[str, dict[str, str]] = {}
    pattern = re.compile(
        r"^\s*([a-z0-9-]+)\) module_dir=([^;]+); package=([^;]+); variable=([A-Z0-9_]+) ;;$"
    )
    for raw in path.read_text(encoding="utf-8").splitlines():
        match = pattern.match(raw)
        if match is None:
            continue
        name, module_dir, package, variable = match.groups()
        if name in result:
            raise ContentError(f"Go tool installer duplicates {name}")
        result[name] = {
            "module_dir": module_dir,
            "package": package,
            "variable": variable,
        }
    return result


def forge_chart_records(path: Path) -> list[dict[str, str]]:
    source = path.read_text(encoding="utf-8")
    constants = dict(
        re.findall(
            r'^\s*(defaultGPUOperator(?:Chart|Version|Repository|SHA256))\s*=\s*"([^"]+)"',
            source,
            re.MULTILINE,
        )
    )
    expected_names = {
        "defaultGPUOperatorChart",
        "defaultGPUOperatorVersion",
        "defaultGPUOperatorRepository",
        "defaultGPUOperatorSHA256",
    }
    if set(constants) != expected_names:
        raise ContentError("Forge GPU chart source contains incomplete content authority")
    return [
        {
            "name": constants["defaultGPUOperatorChart"],
            "version": constants["defaultGPUOperatorVersion"],
            "repository": constants["defaultGPUOperatorRepository"],
            "sha256": constants["defaultGPUOperatorSHA256"],
        }
    ]


def forge_tool_records(path: Path) -> list[dict[str, str]]:
    records: list[dict[str, str]] = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        if not raw.lstrip().startswith('{name: "'):
            continue
        fields = dict(re.findall(r'(name|version|platform|url|sha256|installedSHA256): "([^"]+)"', raw))
        if set(fields) != {"name", "version", "platform", "url", "sha256", "installedSHA256"}:
            raise ContentError("Forge source contains an incomplete reviewed remote tool")
        records.append(
            {
                "name": fields["name"],
                "version": fields["version"],
                "platform": fields["platform"],
                "url": fields["url"],
                "sha256": fields["sha256"],
                "installed_sha256": fields["installedSHA256"],
            }
        )
    return records


def chart_dependencies(chart_yaml: Path) -> list[dict[str, str]]:
    dependencies: list[dict[str, str]] = []
    current: dict[str, str] | None = None
    in_dependencies = False
    for raw in chart_yaml.read_text(encoding="utf-8").splitlines():
        if raw == "dependencies:":
            in_dependencies = True
            continue
        if not in_dependencies:
            continue
        match = re.match(r"^\s*-\s+name:\s*['\"]?([^'\"\s#]+)", raw)
        if match:
            if current is not None:
                dependencies.append(current)
            current = {"name": match.group(1)}
            continue
        if current is None:
            continue
        for key in ("version", "repository"):
            match = re.match(rf"^\s+{key}:\s*['\"]?([^'\"\s#]+)", raw)
            if match:
                current[key] = match.group(1)
    if current is not None:
        dependencies.append(current)
    for dependency in dependencies:
        if set(dependency) != {"name", "version", "repository"}:
            raise ContentError(f"incomplete dependency in {chart_yaml}: {dependency}")
    return dependencies


def validate_action_pins(automation_files: list[Path], root: Path) -> None:
    for automation in automation_files:
        for line in automation.read_text(encoding="utf-8").splitlines():
            match = ACTION.match(line)
            if match is None or match.group(1).startswith("./"):
                continue
            action = match.group(1)
            ref = action.rsplit("@", 1)[1] if "@" in action else ""
            if not re.fullmatch(r"[0-9a-f]{40}", ref):
                raise ContentError(f"{automation.relative_to(root)} has a non-commit Action pin: {line.strip()}")


def runtime_source_files(root: Path) -> list[Path]:
    covered_roots = (
        root / "charts/charts",
        root / "charts/test/e2e",
        root / "forge/internal",
        root / "forge/test/e2e",
        root / "control-plane/internal",
        root / "control-plane/config/samples",
        root / "control-plane/test/e2e",
        root / "inference-gateway/internal",
        root / "testkit/e2e",
    )
    result: list[Path] = []
    for covered in covered_roots:
        for path in covered.rglob("*") if covered.is_dir() else []:
            if not path.is_file() or path.suffix not in {".go", ".yaml", ".yml"}:
                continue
            if path.name.endswith("_test.go") and not any(
                path.parts[index : index + 2] == ("test", "e2e")
                for index in range(len(path.parts) - 1)
            ):
                text = path.read_text(encoding="utf-8")
                if not any(call in text for call in ("testcontainers.Run", "postgres.Run", "tcredis.Run")):
                    continue
            result.append(path)
    for path in (
        root / "forge/forge.example.yaml",
        root / "inference-gateway/docker-compose.yml",
        root / ".github/workflows/ci.yml",
        root / ".github/workflows/release-candidate.yml",
    ):
        if path.is_file():
            result.append(path)
    return sorted(set(result))


def validate_yaml_image_blocks(path: Path, runtime_identities: set[str]) -> None:
    lines = path.read_text(encoding="utf-8").splitlines()
    for index, raw in enumerate(lines):
        match = YAML_KEY.match(raw)
        if match is None or not match.group(2).lower().endswith("image"):
            continue
        indent = len(match.group(1))
        scalar = (match.group(3) or "").strip().strip("'\"")
        if scalar and "{{" not in scalar:
            if ":" in scalar and "@sha256:" not in scalar and not scalar.startswith(PRODUCT_IMAGE_PREFIXES):
                raise ContentError(f"{path} has tag-only runtime image {scalar}")
            if "@sha256:" in scalar and scalar not in runtime_identities:
                raise ContentError(f"{path} has unreviewed runtime image identity {scalar}")
            continue
        values: dict[str, str] = {}
        for nested in lines[index + 1 :]:
            nested_match = YAML_KEY.match(nested)
            if nested_match is None:
                continue
            nested_indent = len(nested_match.group(1))
            if nested_indent <= indent:
                break
            key = nested_match.group(2)
            if key in {"repository", "registry", "image", "tag", "digest", "sha", "sha256"}:
                values[key] = (nested_match.group(3) or "").strip().strip("'\"")
        if not values:
            continue
        repository = values.get("repository", "")
        tag = values.get("tag", "")
        digest = values.get("digest") or values.get("sha") or values.get("sha256")
        if repository.startswith(PRODUCT_IMAGE_PREFIXES):
            continue
        if digest or "@sha256:" in tag:
            if repository and tag:
                if "@sha256:" in tag:
                    identity = f"{repository}:{tag}"
                else:
                    normalized = digest if str(digest).startswith("sha256:") else f"sha256:{digest}"
                    identity = f"{repository}:{tag}@{normalized}"
                if identity not in runtime_identities:
                    raise ContentError(f"{path} has unreviewed runtime image identity {identity}")
            elif values.get("registry") and values.get("image") and tag:
                normalized = digest if str(digest).startswith("sha256:") else f"sha256:{digest}"
                identity = f"{values['registry']}/{values['image']}:{tag.split('@', 1)[0]}@{normalized}"
                if identity not in runtime_identities:
                    raise ContentError(f"{path} has unreviewed runtime image identity {identity}")
            continue
        raise ContentError(f"{path} has a tag-only runtime image mapping: {values}")


def validate_runtime_authority(manifest: dict[str, Any], root: Path) -> None:
    runtime = require_records(manifest, "runtime_images", ("reference", "digest"))
    runtime_digests = {item["digest"].removeprefix("sha256:") for item in runtime}
    runtime_identities = {f"{item['reference']}@{item['digest']}" for item in runtime}
    all_reviewed_digests: set[str] = set()
    for records in manifest.values():
        if not isinstance(records, list):
            continue
        for record in records:
            if not isinstance(record, dict):
                continue
            for key, digest in record.items():
                if key != "digest" and not key.endswith("sha256"):
                    continue
                if isinstance(digest, str):
                    digest = digest.removeprefix("sha256:")
                    if SHA256.fullmatch(digest):
                        all_reviewed_digests.add(digest)
    discovered: set[str] = set()
    for path in runtime_source_files(root):
        text = path.read_text(encoding="utf-8")
        is_owner_unit_test = path.name.endswith("_test.go") and not any(
            path.parts[index : index + 2] == ("test", "e2e")
            for index in range(len(path.parts) - 1)
        )
        if not is_owner_unit_test:
            discovered.update(value.lower() for value in CONTENT_DIGEST.findall(text))
        if path.suffix in {".yaml", ".yml"}:
            validate_yaml_image_blocks(path, runtime_identities)
        for reference in QUOTED_IMAGE.findall(text):
            if reference.startswith("sha256:") or reference.startswith(PRODUCT_IMAGE_PREFIXES):
                continue
            if "@sha256:" not in reference:
                raise ContentError(f"{path} has tag-only runtime image literal {reference}")
            if reference not in runtime_identities:
                raise ContentError(f"{path} has unreviewed runtime image identity {reference}")
    unknown = discovered - all_reviewed_digests
    if unknown:
        raise ContentError(f"runtime source has unreviewed content digests: {sorted(unknown)}")
    unused = runtime_digests - discovered
    if unused:
        raise ContentError(f"runtime image authority has unused identities: {sorted(unused)}")


def validate_manifest(manifest: dict[str, Any], root: Path = ROOT) -> None:
    frontends = require_records(manifest, "docker_frontends", ("reference", "digest"))
    frontend_authority = {f"{item['reference']}@{item['digest']}" for item in frontends}
    images = require_records(manifest, "container_images", ("reference", "digest"))
    image_authority = {f"{item['reference']}@{item['digest']}" for item in images}
    discovered_frontends: set[str] = set()
    discovered_images: set[str] = set()
    for dockerfile in sorted(root.glob("**/Dockerfile")):
        if ".git" in dockerfile.parts:
            continue
        for line in dockerfile.read_text(encoding="utf-8").splitlines():
            syntax = SYNTAX.match(line)
            if syntax is not None:
                reference = syntax.group(1)
                if reference not in frontend_authority:
                    raise ContentError(f"{dockerfile.relative_to(root)} has unreviewed Dockerfile frontend {reference}")
                discovered_frontends.add(reference)
            match = FROM.match(line)
            if match is None or match.group(1) == "scratch":
                continue
            reference = match.group(1)
            if "@sha256:" not in reference or reference not in image_authority:
                raise ContentError(f"{dockerfile.relative_to(root)} has unreviewed FROM {reference}")
            discovered_images.add(reference)
    if discovered_frontends != frontend_authority:
        raise ContentError(f"Dockerfile frontend authority has unused or missing entries: {sorted(frontend_authority ^ discovered_frontends)}")
    if discovered_images != image_authority:
        raise ContentError(f"container image authority has unused or missing entries: {sorted(image_authority ^ discovered_images)}")

    validate_runtime_authority(manifest, root)

    chart_records = require_records(manifest, "helm_charts", ("name", "version", "repository", "url", "sha256"))
    chart_authority = {(item["name"], item["version"], item["repository"]) for item in chart_records}
    discovered_charts: set[tuple[str, str, str]] = set()
    for chart_yaml in sorted((root / "charts/charts").glob("*/Chart.yaml")):
        for dependency in chart_dependencies(chart_yaml):
            if dependency["repository"].startswith("file://"):
                continue
            identity = (dependency["name"], dependency["version"], dependency["repository"])
            if identity not in chart_authority:
                raise ContentError(f"{chart_yaml.relative_to(root)} has unreviewed remote chart {identity}")
            discovered_charts.add(identity)
    if discovered_charts != chart_authority:
        raise ContentError(f"Helm chart authority has unused or missing entries: {sorted(chart_authority ^ discovered_charts)}")

    forge_charts = require_records(manifest, "forge_helm_charts", ("name", "version", "repository", "sha256"))
    if forge_chart_records(root / "forge/internal/config/config.go") != forge_charts:
        raise ContentError("Forge chart source does not exactly match manifest authority")

    installers = require_records(manifest, "installers", ("name", "version", "url", "sha256"))
    forge_tools = require_records(manifest, "forge_tools", ("name", "version", "platform", "url", "sha256", "installed_sha256"))
    require_records(manifest, "tools", ("name", "version", "platform", "url", "sha256"))
    playwright_archives = require_records(
        manifest,
        "playwright_archives",
        ("name", "version", "revision", "platform", "url", "sha256", "directory", "executable", "installed_sha256"),
    )
    ci_tools = require_records(manifest, "ci_tools", ("name", "version", "platform", "url", "sha256"))
    locked_tools = require_locked_tools(manifest)
    ci_images = require_records(manifest, "ci_images", ("reference", "digest"))
    locks = manifest.get("delegated_locks")
    if not isinstance(locks, list) or not locks or len(locks) != len(set(locks)):
        raise ContentError("delegated lock inventory is empty or duplicated")
    for value in locks:
        if not isinstance(value, str) or PurePosixPath(value).is_absolute() or ".." in PurePosixPath(value).parts:
            raise ContentError(f"invalid delegated lock path {value!r}")
        if not (root / value).is_file():
            raise ContentError(f"delegated lock is missing: {value}")
    lock_set = set(locks)
    for item in locked_tools:
        if item["lock"] not in lock_set:
            raise ContentError(f"locked tool {item['name']} references an undeclared lock")

    go_locked = [item for item in locked_tools if item["ecosystem"] == "go"]
    for lock in sorted({item["lock"] for item in go_locked}):
        expected = {
            item["module"]: item["version"]
            for item in go_locked
            if item["lock"] == lock
        }
        actual = go_mod_requirements((root / lock).with_name("go.mod"))
        if actual != expected:
            raise ContentError(f"Go tool lock {lock} does not exactly match reviewed tool authority")
    installer_path = root / ".github/scripts/install_go_tool.sh"
    actual_go_installers = go_tool_installers(installer_path)
    expected_go_installers = {
        item["name"]: {
            "module_dir": str(PurePosixPath(item["lock"]).parent),
            "package": item["package"],
        }
        for item in go_locked
    }
    if set(actual_go_installers) != set(expected_go_installers):
        raise ContentError("Go tool installer inventory does not exactly match reviewed authority")
    for name, expected in expected_go_installers.items():
        actual = actual_go_installers[name]
        if actual["module_dir"] != expected["module_dir"] or actual["package"] != expected["package"]:
            raise ContentError(f"Go tool installer for {name} does not match reviewed authority")

    npm_locked = [item for item in locked_tools if item["ecosystem"] == "npm"]
    package_json_path = root / ".github/tools/protobuf/package.json"
    package_lock_path = root / ".github/tools/protobuf/package-lock.json"
    package_json = json.loads(package_json_path.read_text(encoding="utf-8"))
    package_lock = json.loads(package_lock_path.read_text(encoding="utf-8"))
    expected_npm = {item["package"]: item["version"] for item in npm_locked}
    if package_json.get("dependencies") != expected_npm:
        raise ContentError("Node tool package.json does not exactly match reviewed authority")
    for package, version in expected_npm.items():
        locked = package_lock.get("packages", {}).get(f"node_modules/{package}", {})
        if locked.get("version") != version or not str(locked.get("resolved", "")).startswith("https://") or not str(locked.get("integrity", "")).startswith("sha512-"):
            raise ContentError(f"Node tool {package} lacks exact package-lock content authority")
    node_installer = (root / ".github/scripts/install_node_tool.sh").read_text(encoding="utf-8")
    if set(re.findall(r"^\s*([a-z0-9-]+)\) tool_dir=", node_installer, re.MULTILINE)) != {item["name"] for item in npm_locked}:
        raise ContentError("Node tool installer inventory does not exactly match reviewed authority")
    if "npm ci --ignore-scripts" not in node_installer:
        raise ContentError("Node tools must install from package-lock without running package scripts")

    automation_files = [
        *sorted((root / ".github/workflows").glob("*.yml")),
        *sorted((root / ".github/actions").glob("*/action.yml")),
    ]
    validate_action_pins(automation_files, root)

    automation_text = "\n".join(path.read_text(encoding="utf-8") for path in automation_files)
    workflow_text = "\n".join(path.read_text(encoding="utf-8") for path in (root / ".github/workflows").glob("*.yml"))
    control_makefile = (root / "control-plane/Makefile").read_text(encoding="utf-8")
    if re.search(r"go install\s+\S+@", workflow_text) or re.search(r"go\s+install[^\n]*@", control_makefile):
        raise ContentError("Go tools must install from a repository tool module")
    if re.search(r"npm\s+install", control_makefile):
        raise ContentError("Node tools must install from a repository package lock")
    for name in ("golangci-lint", "controller-gen", "buf", "protoc-gen-go", "protoc-gen-connect-go"):
        if f"install_go_tool.sh {name}" not in workflow_text:
            raise ContentError(f"exact-head workflows do not install repository-locked {name}")
    if "install_node_tool.sh protoc-gen-es" not in workflow_text:
        raise ContentError("exact-head workflows do not install repository-locked protoc-gen-es")
    forbidden_installers = (
        "actions/setup-go@",
        "actions/setup-node@",
        "anchore/sbom-action@",
        "anchore/sbom-action/download-syft@",
        "docker/setup-buildx-action@",
        "goreleaser/goreleaser-action@",
    )
    for action in forbidden_installers:
        if action in automation_text:
            raise ContentError(f"action-installed tool lacks repository checksum authority: {action}")
    ci_authority = {(item["name"], item["version"], item["platform"]) for item in ci_tools}
    for platform in ("linux-amd64", "linux-arm64"):
        for name, pattern in (("go", r"go-version:\s*([0-9.]+)"), ("node", r"node-version:\s*([0-9.]+)")):
            for version in set(re.findall(pattern, workflow_text)):
                if (name, version, platform) not in ci_authority:
                    raise ContentError(f"workflow {name} {version} lacks {platform} checksum authority")
    literal_installs = set(
        re.findall(
            r"uses:\s+\./\.github/actions/setup-ci-tool\s*\n\s+with:\s+\{name:\s*(buildx|envtest|goreleaser|syft),\s*version:\s*([0-9.]+)\}",
            automation_text,
        )
    )
    literal_installs.update(
        re.findall(
            r"uses:\s+\./\.github/actions/setup-ci-tool\s*\n\s+with:\s*\n\s+name:\s*(buildx|envtest|goreleaser|syft)\s*\n\s+version:\s*([0-9.]+)",
            automation_text,
        )
    )
    for name, version in literal_installs:
        for platform in ("linux-amd64", "linux-arm64"):
            if (name, version, platform) not in ci_authority:
                raise ContentError(f"CI tool {name} {version} lacks {platform} checksum authority")
    expected_ci_tools = {(item["name"], item["version"]) for item in ci_tools if item["name"] in {"buildx", "envtest", "goreleaser", "syft"}}
    if literal_installs != expected_ci_tools:
        raise ContentError(f"CI tool authority has unused or unreviewed installers: {sorted(literal_installs ^ expected_ci_tools)}")
    package_lock = json.loads((root / "control-plane/test/e2e/playwright/package-lock.json").read_text(encoding="utf-8"))
    playwright_version = package_lock.get("packages", {}).get("node_modules/playwright-core", {}).get("version")
    archive_versions = {item["version"] for item in playwright_archives}
    if archive_versions != {playwright_version}:
        raise ContentError("Playwright archive authority does not match package-lock.json")
    browser_makefile = (root / "control-plane/Makefile").read_text(encoding="utf-8")
    if f"with: {{version: {playwright_version}}}" not in workflow_text or "--with-deps chromium" in workflow_text:
        raise ContentError("browser workflows do not consume only the reviewed Playwright archives")
    for item in playwright_archives:
        if f"{item['directory']}/INSTALLATION_COMPLETE" not in browser_makefile:
            raise ContentError(f"Playwright runtime does not require reviewed archive {item['name']}")
    ci_image_authority = {f"{item['reference']}@{item['digest']}" for item in ci_images}
    discovered_ci_images = {value for value in ci_image_authority if value in automation_text}
    if discovered_ci_images != ci_image_authority:
        raise ContentError(f"CI image authority has unused identities: {sorted(ci_image_authority - discovered_ci_images)}")
    if (root / ".github/tools/checksums.txt").exists():
        raise ContentError("legacy split tool checksum authority must be removed")

    forge_source_path = root / "forge/internal/sshprovisioner/remote_content.go"
    installer_text = forge_source_path.read_text(encoding="utf-8")
    expected_forge_tools = [
        {
            key: item[key]
            for key in ("name", "version", "platform", "url", "sha256", "installed_sha256")
        }
        for item in forge_tools
    ]
    actual_forge_tools = forge_tool_records(forge_source_path)
    if actual_forge_tools != expected_forge_tools:
        raise ContentError("Forge remote tool source does not exactly match manifest authority")
    installer_constants = dict(
        re.findall(
            r'^\s*(k3sInstallScriptURL|k3sInstallScriptSHA256)\s*=\s*"([^"]+)"',
            installer_text,
            re.MULTILINE,
        )
    )
    if len(installers) != 1 or installers[0]["name"] != "k3s" or installers[0]["version"] != "installer" or installer_constants != {
        "k3sInstallScriptURL": installers[0]["url"],
        "k3sInstallScriptSHA256": installers[0]["sha256"],
    }:
        raise ContentError("Forge installer source does not exactly match manifest authority")


def verified_download(
    url: str,
    expected_sha256: str,
    destination: Path,
    *,
    attempts: int = 4,
    opener: Callable[..., Any] = urlopen,
    sleeper: Callable[[float], None] = time.sleep,
) -> None:
    if not url.startswith("https://") or not SHA256.fullmatch(expected_sha256):
        raise ContentError("download identity must have an HTTPS URL and canonical SHA-256")
    if attempts < 1:
        raise ContentError("download attempts must be positive")
    destination.parent.mkdir(parents=True, exist_ok=True)
    for attempt in range(1, attempts + 1):
        try:
            request = Request(url, headers={"User-Agent": "iterabase-content-lock/1"})
            with opener(request, timeout=30) as response:
                content = response.read()
        except (OSError, URLError) as exc:
            if attempt == attempts:
                raise ContentError(f"transport failed after {attempts} attempts for {url}: {exc}") from exc
            sleeper(attempt * 2)
            continue
        actual = sha256_bytes(content)
        if actual != expected_sha256:
            raise ContentError(f"content checksum mismatch for {url}: expected {expected_sha256}, got {actual}")
        temporary = destination.with_suffix(destination.suffix + ".tmp")
        temporary.write_bytes(content)
        temporary.replace(destination)
        return


def validate_chart_archive(path: Path, name: str, version: str) -> None:
    try:
        with tarfile.open(path, "r:gz") as archive:
            members = [member for member in archive.getmembers() if member.name.count("/") == 1 and member.name.endswith("/Chart.yaml")]
            if len(members) != 1 or not members[0].isfile():
                raise ContentError(f"chart archive {path} has no unique Chart.yaml")
            stream = archive.extractfile(members[0])
            if stream is None:
                raise ContentError(f"chart archive {path} Chart.yaml is unreadable")
            metadata = stream.read().decode("utf-8")
    except (tarfile.TarError, UnicodeDecodeError) as exc:
        raise ContentError(f"invalid chart archive {path}: {exc}") from exc
    found: dict[str, str] = {}
    for line in metadata.splitlines():
        match = re.match(r"^(name|version):\s*['\"]?([^'\"\s#]+)", line)
        if match:
            found[match.group(1)] = match.group(2)
    if found != {"name": name, "version": version}:
        raise ContentError(f"chart archive identity mismatch for {path}: {found}")


def prepare_chart(chart: Path, manifest: dict[str, Any], cache: Path, visiting: set[Path] | None = None) -> None:
    chart = chart.resolve()
    visiting = set() if visiting is None else visiting
    if chart in visiting:
        raise ContentError(f"cyclic local chart dependency at {chart}")
    visiting.add(chart)
    dependencies = chart_dependencies(chart / "Chart.yaml")
    destination = chart / "charts"
    shutil.rmtree(destination, ignore_errors=True)
    destination.mkdir(parents=True, exist_ok=True)
    authority = {
        (item["name"], item["version"], item["repository"]): item
        for item in manifest["helm_charts"]
    }
    packaged: set[tuple[str, str]] = set()
    for dependency in dependencies:
        identity = (dependency["name"], dependency["version"])
        if identity in packaged:
            continue
        packaged.add(identity)
        repository = dependency["repository"]
        if repository.startswith("file://"):
            source = (chart / repository.removeprefix("file://")).resolve()
            if not source.is_dir():
                raise ContentError(f"local chart dependency does not exist: {source}")
            prepare_chart(source, manifest, cache, visiting.copy())
            subprocess.run(["helm", "package", str(source), "--destination", str(destination)], check=True)
            continue
        record = authority.get((dependency["name"], dependency["version"], repository))
        if record is None:
            raise ContentError(f"remote chart dependency is not content locked: {dependency}")
        archive_name = f"{dependency['name']}-{dependency['version']}.tgz"
        cached = cache / f"{record['sha256']}-{archive_name}"
        if not cached.exists():
            verified_download(record["url"], record["sha256"], cached)
        elif sha256_file(cached) != record["sha256"]:
            cached.unlink()
            raise ContentError(f"cached chart checksum mismatch for {archive_name}")
        validate_chart_archive(cached, dependency["name"], dependency["version"])
        shutil.copy2(cached, destination / archive_name)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    validate = commands.add_parser("validate")
    validate.add_argument("--manifest", type=Path, default=MANIFEST)
    prepare = commands.add_parser("prepare-chart")
    prepare.add_argument("--chart", type=Path, required=True)
    prepare.add_argument("--manifest", type=Path, default=MANIFEST)
    prepare.add_argument("--cache", type=Path, default=Path.home() / ".cache/iterabase/charts")
    return parser


def main() -> int:
    args = build_parser().parse_args()
    try:
        manifest = load_manifest(args.manifest)
        validate_manifest(manifest)
        if args.command == "prepare-chart":
            prepare_chart(args.chart, manifest, args.cache)
    except (ContentError, OSError, subprocess.CalledProcessError) as exc:
        print(f"remote content error: {exc}", file=sys.stderr)
        return 1
    print("remote content authority valid")
    return 0


if __name__ == "__main__":
    sys.exit(main())

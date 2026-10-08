#!/usr/bin/env python3
"""Pinned-image cache contract for the permanent CPU/GPU fixtures (HOR-588).

The fixture image cache is seeded on the host through the
`.github/workflows/fixture-image-cache.yml` workflow and consumed by the Forge
E2E harness. This module is the single source of truth for which pinned runtime
images each capacity caches, the deterministic generation hash, and the
per-image archive names. The seed workflow and the real-machine jobs both
derive their expectations from it, so the host cache cannot drift from
`.github/inputs/remote-content.json`. A seed dispatch also verifies a matching
generation archive by archive and repairs anything missing, truncated,
corrupt, or recorded without archive digests through the same staging and
atomic-swap path.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import shlex
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable

SCHEMA_VERSION = 1
# Bump when the on-host archive format or naming changes so every fixture
# re-seeds instead of trusting an incompatible generation.
SEED_FORMAT_VERSION = 4
CACHE_ROOT = "/var/lib/iterabase-e2e/image-cache"
CAPACITIES = ("cpu", "gpu")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256_HEX_RE = re.compile(r"^[0-9a-f]{64}$")
ARCHIVE_RE = re.compile(r"^[A-Za-z0-9._-]+\.tar$")


class FixtureImageCacheError(Exception):
    """Raised when the pinned-image cache contract is violated."""


def repository(reference: str) -> str:
    """Return the registry/repository path of an image reference."""
    name = reference.split("@", 1)[0]
    slash = name.rfind("/")
    colon = name.rfind(":")
    if colon > slash:
        name = name[:colon]
    return name


def is_gpu_only(reference: str) -> bool:
    """Return true when the pinned image is only needed by the GPU fixture."""
    path = repository(reference)
    if path.startswith("nvcr.io/"):
        return True
    if path.startswith("nvidia/"):
        return True
    return "vllm" in path


def load_runtime_images(root: Path) -> list[dict[str, str]]:
    """Load and validate the pinned runtime-image authority."""
    manifest_path = root / ".github/inputs/remote-content.json"
    try:
        data = json.loads(manifest_path.read_text(encoding="utf-8"))
    except OSError as error:
        raise FixtureImageCacheError(f"read {manifest_path}: {error}") from error
    except json.JSONDecodeError as error:
        raise FixtureImageCacheError(f"parse {manifest_path}: {error}") from error

    images = data.get("runtime_images") if isinstance(data, dict) else None
    if not isinstance(images, list) or not images:
        raise FixtureImageCacheError("remote-content.json must pin runtime_images")

    selected: list[dict[str, str]] = []
    seen: set[str] = set()
    for entry in images:
        if not isinstance(entry, dict):
            raise FixtureImageCacheError("runtime image entry must be an object")
        reference = entry.get("reference")
        digest = entry.get("digest")
        if not isinstance(reference, str) or not reference:
            raise FixtureImageCacheError("runtime image entry requires a reference")
        if not isinstance(digest, str) or not SHA256_RE.match(digest):
            raise FixtureImageCacheError(
                f"runtime image {reference} requires a sha256 digest"
            )
        if reference in seen:
            raise FixtureImageCacheError(f"duplicate runtime image {reference}")
        seen.add(reference)
        selected.append({"reference": reference, "digest": digest})
    return selected


def select_images(
    images: list[dict[str, str]], capacity: str
) -> list[dict[str, str]]:
    """Return the pinned images a fixture capacity caches in its AMI (C1).

    The GPU AMI carries everything, including the nvcr.io/NVIDIA/vLLM images:
    its root volume is sized for the cache and its imported copy. The CPU AMI
    leaves the GPU-only images out.
    """
    if capacity not in CAPACITIES:
        raise FixtureImageCacheError(
            f"unsupported capacity {capacity!r}; expected one of {CAPACITIES}"
        )
    if capacity == "gpu":
        return list(images)
    return [image for image in images if not is_gpu_only(image["reference"])]


def archive_name(reference: str, digest: str) -> str:
    """Return the deterministic archive file name for one pinned image."""
    sanitized = re.sub(r"[^A-Za-z0-9._-]+", "_", repository(reference))
    name = f"{sanitized}-{digest.split(':', 1)[1][:12]}.tar"
    if not ARCHIVE_RE.match(name):
        raise FixtureImageCacheError(f"unsafe archive name {name!r}")
    return name


def generation(capacity: str, images: list[dict[str, str]]) -> str:
    """Return the deterministic cache generation for a capacity image set."""
    canonical: dict[str, Any] = {
        "schema_version": SCHEMA_VERSION,
        "seed_format": SEED_FORMAT_VERSION,
        "capacity": capacity,
        "images": [
            {"reference": image["reference"], "digest": image["digest"]}
            for image in sorted(images, key=lambda item: item["reference"])
        ],
    }
    payload = json.dumps(canonical, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(payload).hexdigest()


def rewrite_docker_manifest(data: bytes, reference: str) -> bytes:
    """Bind the exact reference into a docker-save `manifest.json`.

    k3s's containerd imports docker-format archives by `RepoTags` and normalizes
    the resulting name (`busybox:1.37.0` -> `docker.io/library/busybox:1.37.0`),
    which is the same form `crictl` and kubelet resolve. crane names
    digest-pulled images `i-was-a-digest`, so the tag must be rewritten before
    the archive is imported.
    """
    try:
        manifest = json.loads(data)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise FixtureImageCacheError(f"decode docker manifest for {reference}: {error}") from error
    if not isinstance(manifest, list) or len(manifest) != 1 or not isinstance(manifest[0], dict):
        raise FixtureImageCacheError(f"docker manifest for {reference} is ambiguous")
    entry = manifest[0]
    if (
        not isinstance(entry.get("Config"), str)
        or not isinstance(entry.get("Layers"), list)
        or not entry["Layers"]
    ):
        raise FixtureImageCacheError(f"docker manifest for {reference} has no config or layers")
    entry["RepoTags"] = [reference]
    return json.dumps(manifest, separators=(",", ":")).encode("utf-8")


def docker_manifest_config_digest(data: bytes, reference: str) -> str:
    """Return the config digest recorded in a docker-save `manifest.json`.

    The consume side compares this against the imported image's CRI config
    digest, so a stale image under the same tag cannot silently satisfy the
    cache.
    """
    try:
        manifest = json.loads(data)
    except (json.JSONDecodeError, UnicodeDecodeError) as error:
        raise FixtureImageCacheError(f"decode docker manifest for {reference}: {error}") from error
    if not isinstance(manifest, list) or len(manifest) != 1 or not isinstance(manifest[0], dict):
        raise FixtureImageCacheError(f"docker manifest for {reference} is ambiguous")
    config = manifest[0].get("Config")
    if not isinstance(config, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", config):
        raise FixtureImageCacheError(f"docker manifest for {reference} has an invalid config digest")
    return config


def build_manifest(root: Path, capacity: str) -> dict[str, Any]:
    """Build the deterministic cache manifest a fixture capacity must hold."""
    images = select_images(load_runtime_images(root), capacity)
    archives = [archive_name(image["reference"], image["digest"]) for image in images]
    if len(set(archives)) != len(archives):
        raise FixtureImageCacheError("pinned image archive names are not unique")
    entries = [
        {
            "reference": image["reference"],
            "digest": image["digest"],
            "archive": archive,
        }
        for image, archive in zip(images, archives)
    ]
    entries.sort(key=lambda entry: entry["reference"])
    return {
        "schema_version": SCHEMA_VERSION,
        "seed_format": SEED_FORMAT_VERSION,
        "capacity": capacity,
        "generation": generation(capacity, images),
        "cache_root": CACHE_ROOT,
        "images": entries,
    }


# Remote verification emits one machine-readable line per archive so a seed
# dispatch can compare a matching generation against the digests recorded at
# seed time. A missing or unreadable archive exits non-zero; every other archive
# is still reported so one dispatch names all damage. A missing tool is named
# before any archive verdict, so the operator sees one cause, not 49 symptoms.
ARCHIVE_VERIFICATION_SCRIPT = """\
status=0
for tool in stat sha256sum; do
  command -v "$tool" >/dev/null 2>&1 || { printf "tool-missing %s\\n" "$tool"; exit 1; }
done
for path in "$@"; do
  if test ! -f "$path"; then printf "missing %s\\n" "$path"; status=1; continue; fi
  size=$(stat -c %s "$path") || { printf "unreadable %s\\n" "$path"; status=1; continue; }
  hash=$(sha256sum "$path" | cut -d " " -f1)
  printf "archive %s %s %s\\n" "$hash" "$size" "$path"
done
exit $status
"""

# The final swap keeps the destination generation on disk until the staged
# replacement is in place, and restores it when the replacement move fails, so
# repairing the live generation never leaves an empty window behind.
ATOMIC_SWAP_SCRIPT = """\
set -e
destination=$1
staging=$2
previous="${destination}.previous"
rm -rf "$previous"
if test -d "$destination"; then
  mv "$destination" "$previous"
fi
if ! mv "$staging" "$destination"; then
  if test -d "$previous"; then
    mv "$previous" "$destination"
  fi
  exit 1
fi
rm -rf "$previous"
"""


def verification_command(remote_dir: str, archives: list[str]) -> str:
    """Return the remote command that reports every archive's size and sha256."""
    paths = " ".join(
        shlex.quote(f"{remote_dir}/images/{archive}") for archive in archives
    )
    return (
        f"sudo bash -c {shlex.quote(ARCHIVE_VERIFICATION_SCRIPT)} "
        f"fixture-image-cache-verify {paths}"
    )


def atomic_swap_command(destination: str, staging: str) -> str:
    """Return the remote command that installs staging over destination."""
    return (
        f"sudo bash -c {shlex.quote(ATOMIC_SWAP_SCRIPT)} "
        f"fixture-image-cache-swap {shlex.quote(destination)} {shlex.quote(staging)}"
    )


def verification_verdict_lines(output: str) -> list[str]:
    """Return the per-archive verdict lines in a verification command's output."""
    prefixes = {"archive", "missing", "unreadable"}
    return [
        line
        for line in output.splitlines()
        if line.split() and line.split()[0] in prefixes
    ]


def recorded_archive_expectations(recorded: dict[str, Any]) -> dict[str, tuple[str, int]]:
    """Return the per-archive sha256/size a recorded generation.json must satisfy.

    A generation seeded before archive digests were recorded, or one with any
    incomplete entry, yields no expectations so the seeder repairs it instead
    of treating it as intact.
    """
    images = recorded.get("images")
    if not isinstance(images, list) or not images:
        return {}
    expectations: dict[str, tuple[str, int]] = {}
    for entry in images:
        if not isinstance(entry, dict):
            return {}
        archive = entry.get("archive")
        sha256 = entry.get("sha256")
        size = entry.get("size")
        if (
            not isinstance(archive, str)
            or not ARCHIVE_RE.match(archive)
            or not isinstance(sha256, str)
            or not SHA256_HEX_RE.match(sha256)
            or not isinstance(size, int)
            or isinstance(size, bool)
            or size <= 0
            or archive in expectations
        ):
            return {}
        expectations[archive] = (sha256, size)
    return expectations


def archive_verification_failures(
    *,
    images: list[dict[str, str]],
    expectations: dict[str, tuple[str, int]],
    remote_dir: str,
    output: str,
) -> list[str]:
    """Return why each expected archive fails the recorded digest contract."""
    verified: dict[str, tuple[str, int]] = {}
    verdicts: dict[str, str] = {}
    for line in output.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[0] in {"missing", "unreadable"}:
            verdicts[parts[1]] = parts[0]
        elif len(parts) == 4 and parts[0] == "archive":
            try:
                verified[parts[3]] = (parts[1], int(parts[2]))
            except ValueError:
                continue
    failures: list[str] = []
    for image in images:
        archive = image["archive"]
        path = f"{remote_dir}/images/{archive}"
        expectation = expectations.get(archive)
        if expectation is None:
            failures.append(f"archive {archive} has no recorded sha256/size")
            continue
        verdict = verdicts.get(path)
        if verdict is not None:
            failures.append(f"archive is {verdict}: {archive}")
            continue
        actual = verified.get(path)
        if actual is None:
            failures.append(f"archive was not verified: {archive}")
            continue
        actual_sha256, actual_size = actual
        expected_sha256, expected_size = expectation
        if actual_size != expected_size:
            failures.append(
                f"archive size mismatch for {archive}: {actual_size} != {expected_size}"
            )
        elif actual_sha256 != expected_sha256:
            failures.append(f"archive sha256 mismatch for {archive}")
    return failures


def verify_recorded_generation(
    *,
    images: list[dict[str, str]],
    recorded: dict[str, Any],
    remote_dir: str,
    capture: Callable[[str], subprocess.CompletedProcess[str]],
) -> list[str]:
    """Return the reason a matching generation is not intact, or [] when it is."""
    expectations = recorded_archive_expectations(recorded)
    if not expectations:
        return ["generation.json does not record per-archive sha256/size"]
    unknown = [
        image["archive"] for image in images if image["archive"] not in expectations
    ]
    if unknown:
        return [f"generation.json does not record archive {archive}" for archive in unknown]
    result = capture(
        verification_command(remote_dir, [image["archive"] for image in images])
    )
    if not verification_verdict_lines(result.stdout):
        details = (result.stderr or "").strip().splitlines() or (
            result.stdout or ""
        ).strip().splitlines()
        summary = details[0][:200] if details else "no verdict output"
        return [
            f"verification command failed (exit {result.returncode}): {summary}"
        ]
    return archive_verification_failures(
        images=images,
        expectations=expectations,
        remote_dir=remote_dir,
        output=result.stdout,
    )


def seed_fixture_image_cache(
    *,
    manifest: dict[str, Any],
    address: str,
    user: str,
    key: Path,
    host_key: Path,
    crane: Path,
    platform: str = "linux/amd64",
    dry_run: bool = False,
    runner: Any = subprocess.run,
) -> list[str]:
    """Seed one cache generation on a fixture and return the planned commands.

    Pulls every pinned image with the reviewed crane binary directly on the
    fixture, stages the generation under a temporary directory, and swaps it in
    atomically. A matching generation is verified archive by archive and left
    untouched only when every archive is intact.
    """
    capacity = manifest["capacity"]
    generation = manifest["generation"]
    remote_root = f"{manifest['cache_root']}/{capacity}"
    remote_dir = f"{remote_root}/{generation}"
    staging_dir = f"{remote_root}/.staging-{generation}"
    crane_remote = "/tmp/iterabase-fixture-crane"
    commands: list[str] = []

    ssh_base = [
        "ssh",
        "-i",
        str(key),
        "-o",
        "BatchMode=yes",
        "-o",
        "IdentitiesOnly=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        f"UserKnownHostsFile={host_key}",
        # Packing a multi-gigabyte GPU image prints nothing for minutes; keep the
        # connection alive so an idle network path cannot drop it.
        "-o",
        "ServerAliveInterval=30",
        f"{user}@{address}",
    ]
    scp_base = [
        "scp",
        "-i",
        str(key),
        "-o",
        "BatchMode=yes",
        "-o",
        "IdentitiesOnly=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        f"UserKnownHostsFile={host_key}",
    ]

    def ssh_command(remote_command: str) -> list[str]:
        return [*ssh_base, remote_command]

    def execute(
        command: list[str], stdin_text: str | None = None
    ) -> subprocess.CompletedProcess[str]:
        rendered = " ".join(shlex.quote(part) for part in command)
        commands.append(rendered)
        if dry_run:
            print(f"+ {rendered}")
            return subprocess.CompletedProcess(command, 0, "", "")
        return runner(command, check=True, input=stdin_text, text=stdin_text is not None)

    def capture(command: list[str]) -> str:
        rendered = " ".join(shlex.quote(part) for part in command)
        commands.append(rendered)
        if dry_run:
            print(f"+ {rendered}")
            return ""
        result = runner(command, check=True, capture_output=True, text=True)
        return result.stdout

    def probe(remote_command: str) -> subprocess.CompletedProcess[str]:
        """Run a read-only remote command that may exit non-zero on absence."""
        command = ssh_command(remote_command)
        commands.append(" ".join(shlex.quote(part) for part in command))
        return runner(command, capture_output=True, text=True)

    if not dry_run:
        recorded: Any = None
        probe_result = probe(
            f"sudo test -f {remote_dir}/generation.json && "
            f"sudo cat {remote_dir}/generation.json"
        )
        if probe_result.returncode == 0:
            try:
                recorded = json.loads(probe_result.stdout)
            except json.JSONDecodeError:
                recorded = None
        if (
            isinstance(recorded, dict)
            and recorded.get("generation") == generation
            and recorded.get("capacity") == capacity
        ):
            failures = verify_recorded_generation(
                images=manifest["images"],
                recorded=recorded,
                remote_dir=remote_dir,
                capture=probe,
            )
            if not failures:
                print(
                    f"fixture {address}: cache generation {generation} "
                    "already seeded and verified"
                )
                return commands
            for failure in failures:
                print(
                    f"fixture {address}: cache generation {generation} "
                    f"failed verification: {failure}; repairing",
                    file=sys.stderr,
                )

    execute(ssh_command(f"sudo mkdir -p {remote_root}"))
    execute(ssh_command(f"df -h {remote_root}"))
    execute(ssh_command(f"sudo rm -rf {staging_dir} && sudo mkdir -p {staging_dir}/images"))
    execute(
        [
            *scp_base,
            str(crane),
            f"{user}@{address}:{crane_remote}",
        ]
    )
    execute(ssh_command(f"chmod 0755 {crane_remote}"))
    for image in manifest["images"]:
        reference = f"{image['reference']}@{image['digest']}"
        archive = f"{staging_dir}/images/{image['archive']}"
        workdir = f"{staging_dir}/.work"
        execute(ssh_command(f"sudo rm -rf {workdir} && sudo mkdir -p {workdir}"))
        execute(
            ssh_command(
                f"sudo {crane_remote} pull --platform {platform} "
                f"{shlex.quote(reference)} {workdir}/image.tar"
            )
        )
        execute(ssh_command(f"sudo tar -xf {workdir}/image.tar -C {workdir}"))
        manifest_bytes = capture(ssh_command(f"sudo cat {workdir}/manifest.json"))
        if dry_run:
            execute(ssh_command(f"sudo tee {workdir}/manifest.json >/dev/null"))
        else:
            rewritten = rewrite_docker_manifest(
                manifest_bytes.encode("utf-8"), image["reference"]
            )
            execute(
                ssh_command(f"sudo tee {workdir}/manifest.json >/dev/null"),
                stdin_text=rewritten.decode("utf-8") + "\n",
            )
            image["config_digest"] = docker_manifest_config_digest(
                manifest_bytes.encode("utf-8"), image["reference"]
            )
        execute(ssh_command(f"sudo tar -C {workdir} -cf {shlex.quote(archive)} ."))
        archive_sha256 = capture(
            ssh_command(f"sudo sha256sum {shlex.quote(archive)}")
        )
        archive_size = capture(ssh_command(f"sudo stat -c %s {shlex.quote(archive)}"))
        if not dry_run:
            digest_fields = archive_sha256.split()
            if not digest_fields or not SHA256_HEX_RE.match(digest_fields[0]):
                raise FixtureImageCacheError(
                    f"unexpected sha256sum output for {archive!r}: {archive_sha256!r}"
                )
            try:
                image["size"] = int(archive_size.strip())
            except ValueError as error:
                raise FixtureImageCacheError(
                    f"unexpected size output for {archive!r}: {archive_size!r}"
                ) from error
            if image["size"] <= 0:
                raise FixtureImageCacheError(f"seeded archive {archive!r} is empty")
            image["sha256"] = digest_fields[0]
        execute(ssh_command(f"sudo rm -rf {workdir}"))

    rendered_manifest = json.dumps(manifest, indent=2) + "\n"
    execute(
        ssh_command(f"sudo tee {staging_dir}/generation.json >/dev/null"),
        stdin_text=rendered_manifest,
    )
    execute(ssh_command(atomic_swap_command(remote_dir, staging_dir)))
    # Prune only after the new generation is in place, so a failed seed leaves
    # the previous generation intact and usable.
    execute(
        ssh_command(
            f"sudo find {remote_root} -mindepth 1 -maxdepth 1 -type d "
            f"! -name {shlex.quote(generation)} -exec rm -rf {{}} +"
        )
    )
    execute(ssh_command(f"rm -f {crane_remote}"))
    return commands


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest="command", required=True)
    manifest = subcommands.add_parser(
        "manifest", help="emit the deterministic pinned-image cache manifest"
    )
    manifest.add_argument("--capacity", choices=CAPACITIES, required=True)
    manifest.add_argument(
        "--root",
        type=Path,
        default=Path(__file__).resolve().parents[2],
        help="repository root containing .github/inputs/remote-content.json",
    )
    manifest.add_argument(
        "--output", default="-", help="output path, or - for stdout"
    )
    seed = subcommands.add_parser(
        "seed", help="seed one capacity's pinned-image cache on a fixture"
    )
    seed.add_argument("--capacity", choices=CAPACITIES, required=True)
    seed.add_argument("--address", required=True)
    seed.add_argument("--user", required=True)
    seed.add_argument("--key", type=Path, required=True)
    seed.add_argument("--host-key", type=Path, required=True)
    seed.add_argument("--crane", type=Path, required=True)
    seed.add_argument(
        "--root",
        type=Path,
        default=Path(__file__).resolve().parents[2],
        help="repository root containing .github/inputs/remote-content.json",
    )
    seed.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)

    try:
        if args.command == "manifest":
            payload = build_manifest(args.root, args.capacity)
        else:
            seed_fixture_image_cache(
                manifest=build_manifest(args.root, args.capacity),
                address=args.address,
                user=args.user,
                key=args.key,
                host_key=args.host_key,
                crane=args.crane,
                dry_run=args.dry_run,
            )
            return 0
    except FixtureImageCacheError as error:
        print(f"fixture image cache: {error}", file=sys.stderr)
        return 1

    rendered = json.dumps(payload, indent=2, sort_keys=False) + "\n"
    if args.output == "-":
        sys.stdout.write(rendered)
    else:
        Path(args.output).write_text(rendered, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

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
atomic-swap path, and an incremental bake (DES-HOR-676-02) reuses every intact
archive an earlier generation on the same host already holds, so only changed
images are pulled.
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


# Pinned images no GPU scenario loads, by registry/repository path (DES-HOR-676-03).
# The GPU fixture runs the whole platform (Forge substrate, platform chart,
# overlay, Flux) plus the GPU operator offline from its cache, so it needs every
# shared image except these. Leaving them out keeps a change to them from
# rebaking the GPU AMI. An image missing here is cached on both fixtures, which
# is safe; list one here only with evidence that the GPU fixture never renders it.
CPU_FIXTURE_ONLY_REPOSITORIES = frozenset(
    {
        # The CPU LVM, workspace and inotify stages' pods (cpuFixtureState only).
        "debian",
        # The hosted AWS preset only; disabled in every fixture's values.
        "public.ecr.aws/eks/aws-load-balancer-controller",
        # Production DNS only; disabled with no E2E mode (platform values).
        "registry.k8s.io/external-dns/external-dns",
        # testcontainers on GitHub-hosted runners (ci.yml); no fixture runs it.
        "testcontainers/ryuk",
    }
)


def is_cpu_fixture_only(reference: str) -> bool:
    """Return true when the pinned image is never loaded on the GPU fixture."""
    return repository(reference) in CPU_FIXTURE_ONLY_REPOSITORIES


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

    The GPU AMI carries the nvcr.io/NVIDIA/vLLM images and every shared image
    its scenario loads; its root volume is sized for the cache and its imported
    copy. It leaves the CPU-fixture-only images out, so the GPU generation
    hashes only what the GPU fixture needs (DES-HOR-676-03). The CPU AMI leaves the
    GPU-only images out.
    """
    if capacity not in CAPACITIES:
        raise FixtureImageCacheError(
            f"unsupported capacity {capacity!r}; expected one of {CAPACITIES}"
        )
    if capacity == "gpu":
        return [image for image in images if not is_cpu_fixture_only(image["reference"])]
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
    return manifest_for(capacity, select_images(load_runtime_images(root), capacity))


def manifest_for(capacity: str, images: list[dict[str, str]]) -> dict[str, Any]:
    """Build the cache manifest for one capacity's selected images."""
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
    verdicts = archive_verdicts(
        images=images, expectations=expectations, remote_dir=remote_dir, output=output
    )
    return [failure for failure in verdicts.values() if failure is not None]


def archive_verdicts(
    *,
    images: list[dict[str, str]],
    expectations: dict[str, tuple[str, int]],
    remote_dir: str,
    output: str,
) -> dict[str, str | None]:
    """Return each expected archive's failure reason, or None when it is intact."""
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
    results: dict[str, str | None] = {}
    for image in images:
        archive = image["archive"]
        path = f"{remote_dir}/images/{archive}"
        expectation = expectations.get(archive)
        if expectation is None:
            results[archive] = f"archive {archive} has no recorded sha256/size"
            continue
        verdict = verdicts.get(path)
        if verdict is not None:
            results[archive] = f"archive is {verdict}: {archive}"
            continue
        actual = verified.get(path)
        if actual is None:
            results[archive] = f"archive was not verified: {archive}"
            continue
        actual_sha256, actual_size = actual
        expected_sha256, expected_size = expectation
        if actual_size != expected_size:
            results[archive] = (
                f"archive size mismatch for {archive}: {actual_size} != {expected_size}"
            )
        elif actual_sha256 != expected_sha256:
            results[archive] = f"archive sha256 mismatch for {archive}"
        else:
            results[archive] = None
    return results


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


def reusable_entries(recorded: Any, capacity: str) -> dict[str, dict[str, Any]]:
    """Return a recorded generation's complete entries by archive, or {}.

    Only a generation this seed format wrote for this capacity, with every
    archive's sha256 and size recorded, can lend archives to another.
    """
    if (
        not isinstance(recorded, dict)
        or recorded.get("schema_version") != SCHEMA_VERSION
        or recorded.get("seed_format") != SEED_FORMAT_VERSION
        or recorded.get("capacity") != capacity
        or not recorded_archive_expectations(recorded)
    ):
        return {}
    entries: dict[str, dict[str, Any]] = {}
    for entry in recorded["images"]:
        reference = entry.get("reference")
        digest = entry.get("digest")
        config_digest = entry.get("config_digest")
        if (
            isinstance(reference, str)
            and reference
            and isinstance(digest, str)
            and SHA256_RE.match(digest)
            and isinstance(config_digest, str)
            and SHA256_RE.match(config_digest)
        ):
            entries[entry["archive"]] = entry
    return entries


def reusable_archives(
    *,
    images: list[dict[str, str]],
    capacity: str,
    remote_root: str,
    capture: Callable[[str], subprocess.CompletedProcess[str]],
) -> tuple[dict[str, tuple[str, dict[str, Any]]], list[str]]:
    """Return the intact archives that earlier generations on the host hold.

    An incremental bake boots its builder from the previous fixture AMI, whose
    cache holds an earlier generation (DES-HOR-676-02). An archive is reused only when
    that generation records the same reference, digest and archive name with
    its sha256, size and config digest, and the archive on disk still matches
    the recorded sha256 and size. Returns {archive: (generation directory,
    recorded entry)} and why each rejected candidate must be pulled again.
    """
    listing = capture(
        f"sudo find {shlex.quote(remote_root)} -mindepth 2 -maxdepth 2 "
        "-name generation.json -print"
    )
    if listing.returncode != 0:
        return {}, []
    wanted = {image["archive"]: image for image in images}
    candidates: dict[str, tuple[str, dict[str, Any]]] = {}
    for path in sorted(listing.stdout.split()):
        directory = path.rsplit("/", 1)[0]
        if not SHA256_HEX_RE.match(directory.rsplit("/", 1)[-1]):
            continue
        result = capture(f"sudo cat {shlex.quote(path)}")
        if result.returncode != 0:
            continue
        try:
            recorded = json.loads(result.stdout)
        except json.JSONDecodeError:
            continue
        for archive, entry in reusable_entries(recorded, capacity).items():
            image = wanted.get(archive)
            if (
                image is not None
                and archive not in candidates
                and entry["reference"] == image["reference"]
                and entry["digest"] == image["digest"]
            ):
                candidates[archive] = (directory, entry)

    intact: dict[str, tuple[str, dict[str, Any]]] = {}
    rejected: list[str] = []
    for directory in sorted({directory for directory, _ in candidates.values()}):
        group = [
            wanted[archive]
            for archive, (source, _) in sorted(candidates.items())
            if source == directory
        ]
        expectations = {
            image["archive"]: (
                candidates[image["archive"]][1]["sha256"],
                candidates[image["archive"]][1]["size"],
            )
            for image in group
        }
        result = capture(
            verification_command(directory, [image["archive"] for image in group])
        )
        if not verification_verdict_lines(result.stdout):
            rejected.append(
                f"verification of {directory} failed (exit {result.returncode})"
            )
            continue
        verdicts = archive_verdicts(
            images=group,
            expectations=expectations,
            remote_dir=directory,
            output=result.stdout,
        )
        for archive, failure in verdicts.items():
            if failure is None:
                intact[archive] = candidates[archive]
            else:
                rejected.append(failure)
    return intact, rejected


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
    untouched only when every archive is intact. Otherwise every intact archive
    an earlier generation on the host already holds for the same reference and
    digest is hard-linked into the staged generation instead of pulled again,
    so an incremental bake writes only the changed images; the earlier
    generations, with the archives no longer pinned, are pruned after the swap.
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

    reusable: dict[str, tuple[str, dict[str, Any]]] = {}
    if not dry_run:
        reusable, rejected = reusable_archives(
            images=manifest["images"],
            capacity=capacity,
            remote_root=remote_root,
            capture=probe,
        )
        for reason in rejected:
            print(
                f"fixture {address}: not reusing an earlier archive: {reason}; pulling it",
                file=sys.stderr,
            )
        print(
            f"fixture {address}: reusing {len(reusable)} verified archives from earlier "
            f"generations; pulling {len(manifest['images']) - len(reusable)}"
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
        if image["archive"] in reusable:
            # A hard link on the same filesystem writes no data blocks, so the
            # AMI snapshot of an incremental bake carries only changed images.
            source_dir, entry = reusable[image["archive"]]
            source = f"{source_dir}/images/{image['archive']}"
            execute(ssh_command(f"sudo ln {shlex.quote(source)} {shlex.quote(archive)}"))
            image["config_digest"] = entry["config_digest"]
            image["size"] = entry["size"]
            image["sha256"] = entry["sha256"]
            continue
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

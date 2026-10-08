#!/usr/bin/env python3

from __future__ import annotations

import contextlib
import hashlib
import io
import json
from pathlib import Path
import random
import shlex
import subprocess
import sys
import tempfile
import unittest

import fixture_image_cache

ROOT = Path(__file__).resolve().parents[2]

DOCKER_MANIFEST = json.dumps(
    [
        {
            "Config": "sha256:" + "b" * 64,
            "RepoTags": ["busybox:1.37.0"],
            "Layers": ["436a1b1f.tar.gz"],
        }
    ]
)


def fake_archive_sha256(path: str) -> str:
    return hashlib.sha256(path.encode("utf-8")).hexdigest()


def fake_archive_size(path: str) -> int:
    return 1024 + len(path)


def remote_archive_path(manifest: dict[str, object], archive: str) -> str:
    return (
        f"{manifest['cache_root']}/{manifest['capacity']}/"
        f"{manifest['generation']}/images/{archive}"
    )


def recorded_generation(manifest: dict[str, object]) -> dict[str, object]:
    """Return the generation.json the current seeder would write for a manifest."""
    return {
        "schema_version": fixture_image_cache.SCHEMA_VERSION,
        "seed_format": fixture_image_cache.SEED_FORMAT_VERSION,
        "capacity": manifest["capacity"],
        "generation": manifest["generation"],
        "cache_root": manifest["cache_root"],
        "images": [
            {
                "reference": image["reference"],
                "digest": image["digest"],
                "archive": image["archive"],
                "sha256": fake_archive_sha256(remote_archive_path(manifest, image["archive"])),
                "size": fake_archive_size(remote_archive_path(manifest, image["archive"])),
            }
            for image in manifest["images"]
        ],
    }


class FakeFixtureHost:
    """SSH runner double for the pinned-image cache seed contract.

    Serves one recorded generation and an archive health map. Every archive is
    healthy unless the test breaks it: ``missing`` removes it, ``truncated``
    reports a different size, and ``corrupt`` reports a different sha256.
    """

    def __init__(
        self,
        *,
        generation_json: dict[str, object] | None,
        broken: dict[str, str] | None = None,
        verification_failure: tuple[int, str] | None = None,
    ) -> None:
        self.generation_json = generation_json
        self.broken = broken or {}
        self.verification_failure = verification_failure
        self.commands: list[str] = []
        self.generation_written: str | None = None

    def __call__(
        self, command: list[str], **kwargs: object
    ) -> subprocess.CompletedProcess[str]:
        rendered = " ".join(shlex.quote(part) for part in command)
        self.commands.append(rendered)
        if "fixture-image-cache-swap" in rendered:
            return subprocess.CompletedProcess(command, 0, "", "")
        if "sudo tee" in rendered and "generation.json" in rendered:
            self.generation_written = kwargs.get("input")  # type: ignore[assignment]
            return subprocess.CompletedProcess(command, 0, "", "")
        if "sudo test -f" in rendered and "generation.json" in rendered:
            if self.generation_json is None:
                return subprocess.CompletedProcess(command, 1, "", "absent")
            return subprocess.CompletedProcess(
                command, 0, json.dumps(self.generation_json), ""
            )
        if "bash -c" in rendered and "fixture-image-cache-verify" in rendered:
            if self.verification_failure is not None:
                returncode, stderr = self.verification_failure
                return subprocess.CompletedProcess(command, returncode, "", stderr)
            return subprocess.CompletedProcess(
                command, 0, self._verification_output(rendered), ""
            )
        if "manifest.json" in rendered:
            return subprocess.CompletedProcess(command, 0, DOCKER_MANIFEST, "")
        if "sha256sum" in rendered:
            path = shlex.split(rendered)[-1]
            return subprocess.CompletedProcess(
                command, 0, f"{fake_archive_sha256(path)}  {path}\n", ""
            )
        if "stat -c %s" in rendered:
            path = shlex.split(rendered)[-1]
            return subprocess.CompletedProcess(
                command, 0, f"{fake_archive_size(path)}\n", ""
            )
        return subprocess.CompletedProcess(command, 0, "", "")

    def _verification_output(self, rendered: str) -> str:
        remote_command = shlex.split(rendered)[-1]
        paths = [
            token for token in shlex.split(remote_command) if token.endswith(".tar")
        ]
        lines: list[str] = []
        for path in paths:
            archive = path.rsplit("/", 1)[-1]
            state = self.broken.get(archive)
            if state == "missing":
                lines.append(f"missing {path}")
            elif state == "unreadable":
                lines.append(f"unreadable {path}")
            elif state == "truncated":
                lines.append(
                    f"archive {fake_archive_sha256(path)} "
                    f"{fake_archive_size(path) + 1} {path}"
                )
            elif state == "corrupt":
                lines.append(f"archive {'0' * 64} {fake_archive_size(path)} {path}")
            else:
                lines.append(
                    f"archive {fake_archive_sha256(path)} "
                    f"{fake_archive_size(path)} {path}"
                )
        return "\n".join(lines) + "\n"


class FixtureImageCacheTests(unittest.TestCase):
    def test_capacities_cache_the_shared_platform_authority(self) -> None:
        images = fixture_image_cache.load_runtime_images(ROOT)
        cpu = fixture_image_cache.select_images(images, "cpu")
        gpu = fixture_image_cache.select_images(images, "gpu")

        self.assertEqual(len(gpu), len(cpu))
        cpu_refs = {image["reference"] for image in cpu}
        gpu_refs = {image["reference"] for image in gpu}
        self.assertEqual(cpu_refs, gpu_refs)
        self.assertFalse(any(fixture_image_cache.is_gpu_only(ref) for ref in gpu_refs))
        gpu_only = [image["reference"] for image in images if fixture_image_cache.is_gpu_only(image["reference"])]
        self.assertTrue(gpu_only)
        for reference in gpu_only:
            self.assertNotIn(reference, gpu_refs)
        for image in images:
            if not fixture_image_cache.is_gpu_only(image["reference"]):
                self.assertIn(image["reference"], gpu_refs)

    def test_gpu_only_classification_matches_reviewed_authority(self) -> None:
        images = fixture_image_cache.load_runtime_images(ROOT)
        gpu_only = [
            image["reference"]
            for image in images
            if fixture_image_cache.is_gpu_only(image["reference"])
        ]
        shared = [
            image["reference"]
            for image in images
            if not fixture_image_cache.is_gpu_only(image["reference"])
        ]
        self.assertTrue(any(ref.startswith("nvcr.io/") for ref in gpu_only))
        self.assertTrue(any(ref.startswith("nvidia/") for ref in gpu_only))
        self.assertTrue(any("vllm" in ref for ref in gpu_only))
        self.assertTrue(any(ref.startswith("ghcr.io/iterabase/iterabase-third-party/minio") for ref in shared))

    def test_generation_is_order_independent_and_capacity_bound(self) -> None:
        images = fixture_image_cache.load_runtime_images(ROOT)
        cpu = fixture_image_cache.select_images(images, "cpu")
        shuffled = list(cpu)
        random.Random(501).shuffle(shuffled)

        self.assertEqual(
            fixture_image_cache.generation("cpu", cpu),
            fixture_image_cache.generation("cpu", shuffled),
        )
        self.assertNotEqual(
            fixture_image_cache.generation("cpu", cpu),
            fixture_image_cache.generation("gpu", fixture_image_cache.select_images(images, "gpu")),
        )

    def test_generation_changes_when_a_digest_changes(self) -> None:
        images = fixture_image_cache.load_runtime_images(ROOT)
        cpu = fixture_image_cache.select_images(images, "cpu")
        mutated = [dict(image) for image in cpu]
        mutated[0]["digest"] = "sha256:" + "0" * 64

        self.assertNotEqual(
            fixture_image_cache.generation("cpu", cpu),
            fixture_image_cache.generation("cpu", mutated),
        )

    def test_archive_names_are_unique_safe_and_digest_bound(self) -> None:
        for capacity in fixture_image_cache.CAPACITIES:
            manifest = fixture_image_cache.build_manifest(ROOT, capacity)
            archives = [entry["archive"] for entry in manifest["images"]]
            self.assertEqual(len(archives), len(set(archives)))
            for entry in manifest["images"]:
                self.assertRegex(entry["archive"], fixture_image_cache.ARCHIVE_RE)
                self.assertTrue(entry["archive"].endswith(".tar"))
                self.assertNotIn("/", entry["archive"])

    def test_manifest_binds_generation_authority_and_cache_root(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "gpu")
        self.assertEqual(manifest["schema_version"], fixture_image_cache.SCHEMA_VERSION)
        self.assertEqual(manifest["capacity"], "gpu")
        self.assertEqual(manifest["cache_root"], fixture_image_cache.CACHE_ROOT)
        self.assertRegex(manifest["generation"], r"^[0-9a-f]{64}$")
        references = [entry["reference"] for entry in manifest["images"]]
        self.assertEqual(references, sorted(references))
        for entry in manifest["images"]:
            self.assertRegex(entry["digest"], fixture_image_cache.SHA256_RE)

    def test_seed_dry_run_plans_every_digest_bound_pull(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "gpu")
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            commands = fixture_image_cache.seed_fixture_image_cache(
                manifest=manifest,
                address="192.0.2.10",
                user="forge-ci",
                key=Path("/tmp/fixture-key"),
                host_key=Path("/tmp/fixture-host.pub"),
                crane=Path("/tmp/crane"),
                dry_run=True,
            )
        self.assertEqual(len(commands), len(manifest["images"]) * 9 + 9)
        printed = output.getvalue()
        for image in manifest["images"]:
            self.assertIn(f"{image['reference']}@{image['digest']}", printed)
            self.assertIn(image["archive"], printed)
        self.assertIn(f"{manifest['cache_root']}/gpu/{manifest['generation']}", printed)
        self.assertIn("StrictHostKeyChecking=yes", printed)
        self.assertIn("sha256sum", printed)
        self.assertIn("stat -c %s", printed)

    def test_seed_replays_without_a_runner(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "cpu")

        def failing_runner(*_args: object, **_kwargs: object) -> None:
            raise AssertionError("dry-run must not execute commands")

        commands = fixture_image_cache.seed_fixture_image_cache(
            manifest=manifest,
            address="192.0.2.10",
            user="forge-ci",
            key=Path("/tmp/fixture-key"),
            host_key=Path("/tmp/fixture-host.pub"),
            crane=Path("/tmp/crane"),
            dry_run=True,
            runner=failing_runner,
        )
        self.assertEqual(len(commands), len(manifest["images"]) * 9 + 9)

    def _seed_with_fake_host(
        self, manifest: dict[str, object], host: FakeFixtureHost
    ) -> tuple[list[str], str, str]:
        output = io.StringIO()
        errors = io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            commands = fixture_image_cache.seed_fixture_image_cache(
                manifest=manifest,
                address="192.0.2.10",
                user="forge-ci",
                key=Path("/tmp/fixture-key"),
                host_key=Path("/tmp/fixture-host.pub"),
                crane=Path("/tmp/crane"),
                runner=host,
            )
        return commands, output.getvalue(), errors.getvalue()

    def test_seed_leaves_a_verified_generation_untouched(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "cpu")
        host = FakeFixtureHost(generation_json=recorded_generation(manifest))

        commands, stdout, _ = self._seed_with_fake_host(manifest, host)

        self.assertIn("already seeded", stdout)
        self.assertTrue(
            any("fixture-image-cache-verify" in command for command in commands),
            "the seed must verify archives before trusting a matching generation",
        )
        self.assertFalse(any(".staging-" in command for command in commands))
        self.assertIsNone(host.generation_written)

    def test_seed_detects_and_repairs_every_broken_archive(self) -> None:
        for state, reason in (
            ("missing", "archive is missing"),
            ("unreadable", "archive is unreadable"),
            ("truncated", "archive size mismatch"),
            ("corrupt", "archive sha256 mismatch"),
        ):
            with self.subTest(state=state):
                manifest = fixture_image_cache.build_manifest(ROOT, "cpu")
                archive = manifest["images"][3]["archive"]
                host = FakeFixtureHost(
                    generation_json=recorded_generation(manifest),
                    broken={archive: state},
                )

                commands, stdout, stderr = self._seed_with_fake_host(manifest, host)

                self.assertNotIn("already seeded", stdout)
                self.assertIn(archive, stderr)
                self.assertIn(reason, stderr)
                self.assertNotIn("archive is absent", stderr)
                self.assertTrue(
                    any(".staging-" in command for command in commands),
                    "a broken archive must be repaired through staging",
                )
                self.assertTrue(
                    any("fixture-image-cache-swap" in command for command in commands),
                    "the repair must install the staged generation through the swap",
                )
                self.assertIsNotNone(host.generation_written)
                rewritten = json.loads(host.generation_written)
                self.assertEqual(len(rewritten["images"]), len(manifest["images"]))
                for image in rewritten["images"]:
                    self.assertRegex(image["sha256"], r"^[0-9a-f]{64}$")
                    self.assertGreater(image["size"], 0)

    def test_seed_reports_a_failed_verification_command_instead_of_every_archive(
        self,
    ) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "cpu")
        host = FakeFixtureHost(
            generation_json=recorded_generation(manifest),
            verification_failure=(
                255,
                "ssh: connect to host 192.0.2.10 port 22: Connection refused",
            ),
        )

        commands, stdout, stderr = self._seed_with_fake_host(manifest, host)

        self.assertNotIn("already seeded", stdout)
        self.assertIn("verification command failed (exit 255)", stderr)
        self.assertIn("Connection refused", stderr)
        self.assertNotIn("archive was not verified", stderr)
        self.assertTrue(
            any(".staging-" in command for command in commands),
            "a command failure must still fail closed into a repair",
        )

    def test_seed_repairs_a_legacy_generation_without_archive_digests(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "cpu")
        legacy = recorded_generation(manifest)
        for image in legacy["images"]:
            del image["sha256"]
            del image["size"]
        host = FakeFixtureHost(generation_json=legacy)

        commands, stdout, stderr = self._seed_with_fake_host(manifest, host)

        self.assertNotIn("already seeded", stdout)
        self.assertIn("sha256", stderr)
        self.assertTrue(any(".staging-" in command for command in commands))
        self.assertIsNotNone(host.generation_written)

    def test_atomic_swap_keeps_the_previous_generation_until_installed(self) -> None:
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            script = root / "swap.sh"
            script.write_text(fixture_image_cache.ATOMIC_SWAP_SCRIPT, encoding="utf-8")

            destination = root / "generation"
            destination.mkdir()
            (destination / "generation.json").write_text("old", encoding="utf-8")
            staging = root / ".staging-generation"
            staging.mkdir()
            (staging / "generation.json").write_text("new", encoding="utf-8")
            completed = subprocess.run(
                ["bash", str(script), str(destination), str(staging)],
                capture_output=True,
                text=True,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertEqual(
                (destination / "generation.json").read_text(encoding="utf-8"), "new"
            )
            self.assertFalse((root / "generation.previous").exists())
            self.assertFalse(staging.exists())

            keep = root / "generation-keep"
            keep.mkdir()
            (keep / "generation.json").write_text("keep", encoding="utf-8")
            completed = subprocess.run(
                ["bash", str(script), str(keep), str(root / "missing-staging")],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertEqual(
                (keep / "generation.json").read_text(encoding="utf-8"), "keep"
            )
            self.assertFalse((root / "generation-keep.previous").exists())

    def test_verification_script_names_a_missing_tool_instead_of_archives(self) -> None:
        completed = subprocess.run(
            [
                "/bin/bash",
                "-c",
                fixture_image_cache.ARCHIVE_VERIFICATION_SCRIPT,
                "fixture-image-cache-verify",
                "/tmp/unused.tar",
            ],
            env={"PATH": "/nonexistent"},
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("tool-missing stat", completed.stdout)

    def test_recorded_archive_expectations_reject_incomplete_records(self) -> None:
        manifest = fixture_image_cache.build_manifest(ROOT, "cpu")
        recorded = recorded_generation(manifest)
        self.assertEqual(
            len(fixture_image_cache.recorded_archive_expectations(recorded)),
            len(manifest["images"]),
        )
        for mutation in ("unprefixed", "zero", "text"):
            with self.subTest(mutation=mutation):
                broken = recorded_generation(manifest)
                if mutation == "unprefixed":
                    broken["images"][0]["sha256"] = "sha256:" + "a" * 64
                elif mutation == "zero":
                    broken["images"][0]["size"] = 0
                else:
                    broken["images"][0]["size"] = "123"
                self.assertEqual(
                    fixture_image_cache.recorded_archive_expectations(broken), {}
                )

    def test_rewrite_docker_manifest_binds_the_exact_reference(self) -> None:
        manifest = json.dumps(
            [
                {
                    "Config": "sha256:" + "b" * 64,
                    "RepoTags": ["index.docker.io/library/busybox:i-was-a-digest"],
                    "Layers": ["436a1b1f.tar.gz"],
                }
            ]
        ).encode()
        rewritten = json.loads(
            fixture_image_cache.rewrite_docker_manifest(manifest, "busybox:1.37.0")
        )
        self.assertEqual(rewritten[0]["RepoTags"], ["busybox:1.37.0"])
        self.assertEqual(rewritten[0]["Config"], "sha256:" + "b" * 64)
        self.assertEqual(rewritten[0]["Layers"], ["436a1b1f.tar.gz"])

    def test_rewrite_docker_manifest_rejects_ambiguous_manifests(self) -> None:
        valid_entry = {"Config": "sha256:" + "b" * 64, "Layers": ["436a1b1f.tar.gz"]}
        for payload in (
            b"{not json",
            json.dumps([valid_entry, valid_entry]).encode(),
            json.dumps([{"RepoTags": ["busybox:1.37.0"], "Layers": ["436a1b1f.tar.gz"]}]).encode(),
            json.dumps([{"Config": "sha256:" + "b" * 64, "Layers": []}]).encode(),
        ):
            with self.assertRaises(fixture_image_cache.FixtureImageCacheError):
                fixture_image_cache.rewrite_docker_manifest(payload, "busybox:1.37.0")

    def test_docker_manifest_config_digest_reads_the_config(self) -> None:
        config = "sha256:" + "b" * 64
        manifest = json.dumps(
            [{"Config": config, "RepoTags": ["busybox:1.37.0"], "Layers": ["436a1b1f.tar.gz"]}]
        ).encode()
        self.assertEqual(
            fixture_image_cache.docker_manifest_config_digest(manifest, "busybox:1.37.0"), config
        )
        for payload in (
            b"{not json",
            json.dumps([{"RepoTags": ["busybox:1.37.0"], "Layers": ["436a1b1f.tar.gz"]}]).encode(),
            json.dumps([{"Config": "sha256:short", "Layers": ["436a1b1f.tar.gz"]}]).encode(),
        ):
            with self.assertRaises(fixture_image_cache.FixtureImageCacheError):
                fixture_image_cache.docker_manifest_config_digest(payload, "busybox:1.37.0")

    def test_unknown_capacity_is_rejected(self) -> None:
        images = fixture_image_cache.load_runtime_images(ROOT)
        with self.assertRaisesRegex(
            fixture_image_cache.FixtureImageCacheError, "unsupported capacity"
        ):
            fixture_image_cache.select_images(images, "quantum")

    def test_invalid_authority_entries_are_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            manifest = root / ".github/inputs/remote-content.json"
            manifest.parent.mkdir(parents=True)
            cases = (
                {"runtime_images": []},
                {"runtime_images": [{"reference": "x", "digest": "sha256:short"}]},
                {
                    "runtime_images": [
                        {"reference": "x", "digest": "sha256:" + "a" * 64},
                        {"reference": "x", "digest": "sha256:" + "b" * 64},
                    ]
                },
            )
            for case in cases:
                manifest.write_text(json.dumps(case), encoding="utf-8")
                with self.assertRaises(fixture_image_cache.FixtureImageCacheError):
                    fixture_image_cache.load_runtime_images(root)
            manifest.write_text("{not json", encoding="utf-8")
            with self.assertRaises(fixture_image_cache.FixtureImageCacheError):
                fixture_image_cache.load_runtime_images(root)

    def test_cli_emits_the_same_manifest(self) -> None:
        script = Path(__file__).with_name("fixture_image_cache.py")
        completed = subprocess.run(
            [sys.executable, str(script), "manifest", "--capacity", "cpu"],
            cwd=ROOT,
            check=True,
            capture_output=True,
            text=True,
        )
        emitted = json.loads(completed.stdout)
        self.assertEqual(
            emitted, fixture_image_cache.build_manifest(ROOT, "cpu")
        )


if __name__ == "__main__":
    unittest.main()

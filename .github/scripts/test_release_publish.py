#!/usr/bin/env python3
"""Resumable release publish helpers (HOR-590 #141 review)."""

import io
import os
import pathlib
import subprocess
import tarfile
import tempfile
import unittest

SCRIPTS = pathlib.Path(__file__).resolve().parent


def tgz(files: dict[str, bytes], mtime: float) -> bytes:
    """A gzip tarball of files; mtime varies the archive bytes like helm package does."""
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        for name, data in sorted(files.items()):
            info = tarfile.TarInfo(name)
            info.size, info.mtime = len(data), mtime
            archive.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


def chart(values: bytes, mtime: float) -> bytes:
    """A packaged chart embedding one dependency chart as a nested archive."""
    dependency = tgz({"redis/Chart.yaml": b"name: redis\n"}, mtime)
    return tgz({"platform/Chart.yaml": b"name: platform\n", "platform/values.yaml": values,
                "platform/charts/redis-0.2.4.tgz": dependency}, mtime)


class ChartContentEqualTests(unittest.TestCase):
    def compare(self, a: bytes, b: bytes) -> int:
        with tempfile.TemporaryDirectory() as tmp:
            pa, pb = pathlib.Path(tmp, "a.tgz"), pathlib.Path(tmp, "b.tgz")
            pa.write_bytes(a)
            pb.write_bytes(b)
            return subprocess.run([SCRIPTS / "chart_content_equal.sh", pa, pb], capture_output=True).returncode

    def test_same_content_with_different_archive_bytes_is_equal(self):
        first, second = chart(b"a: 1\n", 1_000_000), chart(b"a: 1\n", 2_000_000)
        self.assertNotEqual(first, second, "the archives must differ byte-wise, like helm package output")
        self.assertEqual(self.compare(first, second), 0)

    def test_changed_content_is_not_equal(self):
        self.assertEqual(self.compare(chart(b"a: 1\n", 1), chart(b"a: 2\n", 1)), 1)

    def test_changed_nested_dependency_is_not_equal(self):
        base = chart(b"a: 1\n", 1)
        other_dependency = tgz({"redis/Chart.yaml": b"name: redis\nversion: 2\n"}, 1)
        changed = tgz({"platform/Chart.yaml": b"name: platform\n", "platform/values.yaml": b"a: 1\n",
                       "platform/charts/redis-0.2.4.tgz": other_dependency}, 1)
        self.assertEqual(self.compare(base, changed), 1)


class RegistryStateTests(unittest.TestCase):
    def state(self, crane_stderr: str, crane_exit: int) -> subprocess.CompletedProcess:
        with tempfile.TemporaryDirectory() as tmp:
            crane = pathlib.Path(tmp, "crane")
            crane.write_text(f"#!/bin/sh\nprintf '%s' '{crane_stderr}' >&2\nexit {crane_exit}\n")
            crane.chmod(0o755)
            env = {**os.environ, "PATH": f"{tmp}:{os.environ['PATH']}"}
            return subprocess.run([SCRIPTS / "registry_state.sh", "ghcr.io/iterabase/x:1"], env=env,
                                  capture_output=True, text=True)

    def test_existing_reference_is_present(self):
        result = self.state("", 0)
        self.assertEqual((result.returncode, result.stdout.strip()), (0, "present"))

    def test_registry_not_found_is_absent(self):
        for message in ("MANIFEST_UNKNOWN: manifest unknown", "NAME_UNKNOWN: repository name not known"):
            result = self.state(message, 1)
            self.assertEqual((result.returncode, result.stdout.strip()), (0, "absent"), message)

    def test_any_other_failure_is_an_error_never_absent(self):
        for message in ("UNAUTHORIZED: authentication required", "dial tcp: i/o timeout", "TOOMANYREQUESTS"):
            result = self.state(message, 1)
            self.assertEqual(result.returncode, 1, message)
            self.assertNotIn("absent", result.stdout)


class ReleaseSourceGuardTests(unittest.TestCase):
    def test_plan_releases_only_the_commit_the_workflow_runs_from(self):
        plan = (SCRIPTS.parent / "workflows" / "release.yml").read_text(encoding="utf-8").split("\n  validate:")[0]
        self.assertIn('[[ "$SHA" == "$GITHUB_SHA" ]]', plan, "DES-HOR-590-11: sha must be the workflow's own commit")


if __name__ == "__main__":
    unittest.main()

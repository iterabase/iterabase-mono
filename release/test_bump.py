#!/usr/bin/env python3
"""Table tests for release/bump.py against a copy of the real linked files."""
from __future__ import annotations

import pathlib
import re
import shutil
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import bump  # noqa: E402

REAL_ROOT = pathlib.Path(__file__).resolve().parent.parent
COPIED = ["release/targets.json", "control-plane/VERSION", "inference-gateway/VERSION", "forge/VERSION"]
CHARTS = ["control-plane", "inference-gateway", "iterabase-platform", *bump.SUBSTRATES]


def field(root: pathlib.Path, chart: str, name: str) -> str:
    text = (root / "charts" / "charts" / chart / "Chart.yaml").read_text(encoding="utf-8")
    return re.search(rf"^{name}: \"?([^\"\n]+)\"?$", text, re.MULTILINE).group(1)


def dependency(root: pathlib.Path, chart: str) -> str:
    text = (root / "charts" / "charts" / "iterabase-platform" / "Chart.yaml").read_text(encoding="utf-8")
    return re.search(rf"^  - name: {chart}\n    version: (.+)$", text, re.MULTILINE).group(1)


class BumpTests(unittest.TestCase):
    def setUp(self) -> None:
        self.root = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)
        for path in COPIED:
            (self.root / path).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy(REAL_ROOT / path, self.root / path)
        for chart in CHARTS:
            target = self.root / "charts" / "charts" / chart
            target.mkdir(parents=True)
            shutil.copy(REAL_ROOT / "charts" / "charts" / chart / "Chart.yaml", target / "Chart.yaml")
        for name, value in (("ROOT", self.root), ("CHARTS", self.root / "charts" / "charts")):
            patcher = mock.patch.object(bump, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_component_moves_version_file_and_chart_app_version(self) -> None:
        for target in ("control-plane", "inference-gateway"):
            with self.subTest(target=target):
                changed = bump.bump(target, "9.8.7")
                self.assertEqual((self.root / target / "VERSION").read_text(), "9.8.7\n")
                self.assertEqual(field(self.root, target, "appVersion"), "9.8.7")
                self.assertEqual(changed, sorted([f"{target}/VERSION", f"charts/charts/{target}/Chart.yaml"]))

    def test_forge_moves_only_its_version_file(self) -> None:
        self.assertEqual(bump.bump("forge", "1.0.0"), ["forge/VERSION"])
        self.assertEqual((self.root / "forge" / "VERSION").read_text(), "1.0.0\n")

    def test_component_chart_moves_chart_version_and_platform_dependency(self) -> None:
        for chart in ("control-plane", "inference-gateway"):
            with self.subTest(chart=chart):
                app_before = field(self.root, chart, "appVersion")
                bump.bump(f"{chart}-chart", "3.2.1")
                self.assertEqual(field(self.root, chart, "version"), "3.2.1")
                self.assertEqual(dependency(self.root, chart), "3.2.1")
                self.assertEqual(field(self.root, chart, "appVersion"), app_before)

    def test_platform_chart_moves_platform_and_both_substrates(self) -> None:
        bump.bump("iterabase-platform-chart", "0.5.0")
        self.assertEqual(field(self.root, "iterabase-platform", "version"), "0.5.0")
        self.assertEqual(field(self.root, "iterabase-platform", "appVersion"), "0.5.0")
        for substrate in bump.SUBSTRATES:
            self.assertEqual(field(self.root, substrate, "version"), "0.5.0")

    def test_rejects_unknown_target_and_non_semver(self) -> None:
        for target, version in (("nope", "1.0.0"), ("forge", "1.0"), ("forge", "v1.0.0")):
            with self.subTest(target=target, version=version):
                with self.assertRaises(bump.BumpError):
                    bump.bump(target, version)


if __name__ == "__main__":
    unittest.main()

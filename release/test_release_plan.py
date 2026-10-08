#!/usr/bin/env python3
"""Release composition tests (C7, C10) against the real targets and charts."""
from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import release_plan  # noqa: E402
from release_plan import ReleasePlanError  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent
CONTRACT = json.loads((ROOT / "release" / "targets.json").read_text(encoding="utf-8"))
VERSIONS = {name: release_plan.target_version(name, target) for name, target in CONTRACT["targets"].items()}
TAGS = {name: f"{CONTRACT['targets'][name]['tag_prefix']}{VERSIONS[name]}" for name in CONTRACT["targets"]}
SHA = "a" * 40


def published(*except_targets: str) -> set[str]:
    """Every target published at its current version except the named ones."""
    return {TAGS[name] for name in TAGS if name not in except_targets}


class ReleasePlanTests(unittest.TestCase):
    def test_a_target_whose_version_is_published_cannot_release(self) -> None:
        with self.assertRaisesRegex(ReleasePlanError, "already published.*make bump TARGET=forge"):
            release_plan.plan(["forge"], CONTRACT, published(), SHA)

    def test_unpublished_member_releases_with_its_tag(self) -> None:
        members = release_plan.plan(["forge"], CONTRACT, published("forge"), SHA)
        self.assertEqual([(m["target"], m["tag"], m["latest"]) for m in members], [("forge", TAGS["forge"], False)])

    def test_a_missing_reference_fails_and_names_the_target(self) -> None:
        # Releasing the control-plane chart whose appVersion image is unpublished and not in the set.
        with self.assertRaisesRegex(ReleasePlanError, r"control-plane-chart references control-plane .*add control-plane"):
            release_plan.plan(["control-plane-chart"], CONTRACT, published("control-plane", "control-plane-chart"), SHA)

    def test_releasing_the_referenced_target_together_satisfies_the_reference(self) -> None:
        members = release_plan.plan(["control-plane", "control-plane-chart"], CONTRACT,
                                    published("control-plane", "control-plane-chart"), SHA)
        self.assertEqual(sorted(m["target"] for m in members), ["control-plane", "control-plane-chart"])

    def test_platform_chart_references_both_component_charts_and_becomes_latest(self) -> None:
        with self.assertRaisesRegex(ReleasePlanError, "iterabase-platform-chart references inference-gateway-chart"):
            release_plan.plan(["iterabase-platform-chart"], CONTRACT,
                              published("iterabase-platform-chart", "inference-gateway-chart"), SHA)
        members = release_plan.plan(["iterabase-platform-chart"], CONTRACT, published("iterabase-platform-chart"), SHA)
        self.assertTrue(members[0]["latest"])
        self.assertEqual({a["recipe"] for a in members[0]["artifacts"]},
                         {"iterabase-platform-chart", "cert-manager-substrate-chart", "lvm-storage-substrate-chart"})

    def test_image_members_name_their_images(self) -> None:
        members = release_plan.plan(["control-plane"], CONTRACT, published("control-plane"), SHA)
        self.assertEqual(sorted(a["image"] for a in members[0]["artifacts"]),
                         ["control-plane", "control-plane-harness", "control-plane-tool-runner"])

    def test_unknown_or_empty_sets_fail(self) -> None:
        for selected in ([], ["nope"]):
            with self.subTest(selected=selected), self.assertRaises(ReleasePlanError):
                release_plan.plan(selected, CONTRACT, set(), SHA)


if __name__ == "__main__":
    unittest.main()

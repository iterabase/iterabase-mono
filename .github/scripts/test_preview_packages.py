#!/usr/bin/env python3
"""Preview package retention (C9)."""
from __future__ import annotations

import datetime as dt
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import preview_packages  # noqa: E402

NOW = dt.datetime(2026, 10, 20, tzinfo=dt.timezone.utc)
OLD, NEW = "2026-10-01T00:00:00Z", "2026-10-19T00:00:00Z"
LIVE, DEAD = "a" * 40, "b" * 40


def version(tag: str, created: str = NEW) -> dict:
    return {"id": tag, "created_at": created, "tags": [tag]}


def deleted(versions: list[dict]) -> list[str]:
    return [v["id"] for v in preview_packages.plan(versions, open_prs={7}, keep_commits={LIVE}, now=NOW)]


class PreviewRetentionTests(unittest.TestCase):
    def test_pull_request_charts_go_when_closed_or_old(self) -> None:
        self.assertEqual(deleted([version("0.4.7-pr.7.1"), version("0.4.7-pr.8.1"), version("0.4.7-pr.7.2", OLD)]),
                         ["0.4.7-pr.8.1", "0.4.7-pr.7.2"])

    def test_staging_keeps_its_newest_ten(self) -> None:
        charts = [version(f"0.4.7-main.{n}", f"2026-10-{n + 1:02d}T00:00:00Z") for n in range(1, 13)]
        self.assertEqual(sorted(deleted(charts)), ["0.4.7-main.1", "0.4.7-main.2"])

    def test_images_go_when_old_and_not_a_live_head(self) -> None:
        self.assertEqual(deleted([version(f"sha-{LIVE}", OLD), version(f"sha-{DEAD}", OLD), version(f"sha-{DEAD}")]),
                         [f"sha-{DEAD}"])

    def test_unrecognised_tags_are_never_deleted(self) -> None:
        self.assertEqual(deleted([version("0.4.7", OLD), version("latest", OLD)]), [])


if __name__ == "__main__":
    unittest.main()

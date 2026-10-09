#!/usr/bin/env python3

"""Guard the reviewed block-style Dependabot security-only configuration.

These are repository policy checks, not a general YAML parser or proof of
GitHub's live update behavior. Post-merge observation remains required.
"""

from pathlib import Path
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]
CONFIG = ROOT / ".github/dependabot.yml"


class DependabotTests(unittest.TestCase):
    def setUp(self) -> None:
        self.config = CONFIG.read_text(encoding="utf-8")
        self.entries = re.findall(
            r"(?ms)^  - package-ecosystem: (\S+)\n(.*?)(?=^  - package-ecosystem: |\Z)",
            self.config,
        )

    def test_security_options_use_implicit_default_branch(self) -> None:
        # Even explicitly naming the default branch makes the entry's options
        # inapplicable to security updates. Do not reintroduce this selector.
        self.assertNotRegex(
            self.config,
            r"(?m)^\s*['\"]?target-branch['\"]?\s*:",
            "target-branch disables the configured security-update options",
        )

    def test_required_contract_job_runs_the_policy_guard(self) -> None:
        workflow = (ROOT / ".github/workflows/ci.yml").read_text(encoding="utf-8")
        contract = re.search(
            r"(?ms)^  ci-contract:\n(.*?)(?=^  [\w-]+:|\Z)", workflow,
        )
        self.assertIsNotNone(contract, "the CI contract job must exist")
        self.assertRegex(contract[1], r"(?m)^        run: make dependabot-check$")
        aggregate = re.search(
            r"(?ms)^  required:\n(.*?)(?=^  [\w-]+:|\Z)", workflow,
        )
        self.assertIsNotNone(aggregate, "the required CI aggregate must exist")
        self.assertRegex(aggregate[1], r"(?m)^      - ci-contract$")

    def test_each_alert_capable_manifest_has_its_own_entry(self) -> None:
        tracked = subprocess.check_output(
            [
                "git", "-C", str(ROOT), "ls-files", "-z", "--",
                ":(glob)**/package-lock.json", ":(glob)**/go.mod",
            ],
            text=True,
        ).split("\0")
        expected = {
            ("npm" if Path(path).name == "package-lock.json" else "gomod",
             "/" + str(Path(path).parent))
            for path in tracked if path
        }
        expected.add(("github-actions", "/"))
        actual = []
        for ecosystem, body in self.entries:
            directory = re.findall(r"(?m)^    directory: (\S+)$", body)
            self.assertEqual(1, len(directory), "each entry must name one directory")
            self.assertNotRegex(body, r"(?m)^\s*directories\s*:")
            actual.append((ecosystem, directory[0]))
        self.assertCountEqual(expected, actual)

    def test_every_entry_disables_versions_and_groups_security_updates(self) -> None:
        self.assertTrue(self.entries, "the reviewed update entries must exist")
        for ecosystem, body in self.entries:
            with self.subTest(ecosystem=ecosystem, entry=body.splitlines()[0]):
                self.assertEqual(
                    ["0"],
                    re.findall(r"(?m)^    open-pull-requests-limit: (\S+)$", body),
                )
                self.assertRegex(
                    body,
                    r'(?m)^    groups:\n      security:\n'
                    r'        applies-to: security-updates\n'
                    r'        patterns:\n          - "\*"$',
                )


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""A job that calls a local reusable workflow must grant every permission the
called workflow's jobs declare: GitHub rejects the whole run at startup
otherwise, even for jobs it would skip (HOR-590 full-validation finding)."""

from pathlib import Path
import re
import unittest

WORKFLOWS = Path(__file__).resolve().parents[1] / "workflows"
LEVEL = {"none": 0, "read": 1, "write": 2}


def parse_jobs(path: Path) -> tuple[dict[str, str], dict[str, dict]]:
    """Top-level permissions and, per job, its permissions and local `uses`."""
    top: dict[str, str] = {}
    jobs: dict[str, dict] = {}
    section, job, block = None, None, None
    for raw in path.read_text(encoding="utf-8").splitlines():
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip())
        line = raw.strip()
        if indent == 0:
            section = line.rstrip(":")
            job, block = None, None
            continue
        if section == "permissions" and indent == 2:
            key, _, value = line.partition(":")
            top[key.strip()] = value.strip()
        if section != "jobs":
            continue
        if indent == 2 and line.endswith(":"):
            job = line[:-1]
            jobs[job] = {"permissions": {}, "uses": None}
            block = None
        elif job and indent == 4:
            block = line.rstrip(":") if line.endswith(":") else None
            match = re.fullmatch(r"uses:\s*\./\.github/workflows/([\w.-]+\.ya?ml)", line)
            if match:
                jobs[job]["uses"] = match.group(1)
        elif job and indent == 6 and block == "permissions":
            key, _, value = line.partition(":")
            jobs[job]["permissions"][key.strip()] = value.strip()
    return top, jobs


def required(workflow: str, seen: frozenset[str] = frozenset()) -> dict[str, str]:
    """The union of permissions a workflow's jobs need, through nested calls."""
    if workflow in seen:
        return {}
    top, jobs = parse_jobs(WORKFLOWS / workflow)
    need = dict(top)
    for job in jobs.values():
        sources = [job["permissions"] or top]
        if job["uses"]:
            sources.append(required(job["uses"], seen | {workflow}))
        for source in sources:
            for key, value in source.items():
                if LEVEL.get(value, 0) > LEVEL.get(need.get(key, "none"), 0):
                    need[key] = value
    return need


class ReusableWorkflowPermissionTests(unittest.TestCase):
    def test_callers_grant_what_called_workflows_need(self) -> None:
        checked = 0
        for path in sorted(WORKFLOWS.glob("*.y*ml")):
            top, jobs = parse_jobs(path)
            for name, job in jobs.items():
                if not job["uses"]:
                    continue
                granted = job["permissions"] or top
                for key, value in required(job["uses"]).items():
                    with self.subTest(caller=f"{path.name}:{name}", called=job["uses"], permission=key):
                        self.assertGreaterEqual(LEVEL.get(granted.get(key, "none"), 0), LEVEL[value],
                                                f"{path.name} job {name} grants {key}: {granted.get(key, 'none')}, "
                                                f"{job['uses']} needs {value}")
                checked += 1
        self.assertGreater(checked, 0, "no reusable workflow calls found; the parser is broken")

    def test_parser_reads_job_permissions_and_calls(self) -> None:
        _, jobs = parse_jobs(WORKFLOWS / "full-validation.yml")
        self.assertEqual(jobs["e2e"]["uses"], "e2e.yml")
        self.assertEqual(jobs["e2e"]["permissions"].get("id-token"), "write")
        self.assertEqual(required("e2e.yml").get("deployments"), "write")


if __name__ == "__main__":
    unittest.main()

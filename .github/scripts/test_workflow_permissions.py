#!/usr/bin/env python3
"""A job that calls a local reusable workflow must grant every permission the
called workflow's jobs declare: GitHub rejects the whole run at startup
otherwise, even for jobs it would skip (HOR-590 full-validation finding).
Optional preview jobs must also exclude Dependabot by PR author (HOR-642)."""

import ast
from pathlib import Path
import re
import unittest

WORKFLOWS = Path(__file__).resolve().parents[1] / "workflows"
LEVEL = {"none": 0, "read": 1, "write": 2}


def parse_jobs(path: Path) -> tuple[dict[str, str], dict[str, dict]]:
    """Top-level permissions and job permissions, calls, conditions and needs."""
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
            jobs[job] = {"permissions": {}, "uses": None, "if": "", "needs": ""}
            block = None
        elif job and indent == 4:
            block = line.rstrip(":") if line.endswith(":") else None
            match = re.fullmatch(r"uses:\s*\./\.github/workflows/([\w.-]+\.ya?ml)", line)
            if match:
                jobs[job]["uses"] = match.group(1)
            if line.startswith("if:"):
                condition = line.partition(":")[2].strip()
                block = "if" if condition in {">", ">-"} else None
                jobs[job]["if"] = "" if block else condition
            elif line.startswith("needs:"):
                jobs[job]["needs"] = line.partition(":")[2].strip()
        elif job and indent == 6 and block == "permissions":
            key, _, value = line.partition(":")
            jobs[job]["permissions"][key.strip()] = value.strip()
        elif job and indent > 4 and block == "if":
            jobs[job]["if"] += " " + line
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


def condition_matches(expression: str, context: dict) -> bool:
    """Evaluate the boolean/comparison subset used by the shipped preview guards.

    Read the actual workflow expression, not a second copy of its policy.
    Unsupported syntax fails the test rather than silently changing semantics.
    """
    expression = expression.strip()
    if expression.startswith("${{") and expression.endswith("}}"):
        expression = expression[3:-2].strip()
    expression = re.sub(
        r"'[^']*'|[A-Za-z_][\w.]*",
        lambda match: match[0] if match[0].startswith("'") else repr(context[match[0]]),
        expression,
    )
    expression = re.sub(r"!(?!=)", " not ", expression)
    expression = expression.replace("&&", " and ").replace("||", " or ")

    def evaluate(node: ast.AST):
        if isinstance(node, ast.Constant):
            return node.value
        if isinstance(node, ast.BoolOp):
            values = [bool(evaluate(value)) for value in node.values]
            return all(values) if isinstance(node.op, ast.And) else any(values)
        if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.Not):
            return not evaluate(node.operand)
        if isinstance(node, ast.Compare) and len(node.ops) == 1:
            left, right = evaluate(node.left), evaluate(node.comparators[0])
            if isinstance(node.ops[0], ast.Eq):
                return left == right
            if isinstance(node.ops[0], ast.NotEq):
                return left != right
        raise AssertionError(f"unsupported preview condition: {ast.dump(node)}")

    return bool(evaluate(ast.parse(expression.strip(), mode="eval").body))


class PreviewEligibilityTests(unittest.TestCase):
    def context(self, *, author="nunocgoncalves", actor="nunocgoncalves",
                event="pull_request", fork=False, build='[{"image":"control-plane"}]',
                full=False) -> dict:
        return {
            "github.event_name": event,
            "github.repository": "iterabase/iterabase-mono",
            "github.event.pull_request.head.repo.full_name": "contributor/fork" if fork else "iterabase/iterabase-mono",
            "github.event.pull_request.user.login": author,
            "github.actor": actor,
            "github.triggering_actor": actor,
            "needs.select.outputs.build": build,
            "inputs.all": full,
        }

    def test_preview_uses_pr_author_not_rerun_actor(self) -> None:
        _, jobs = parse_jobs(WORKFLOWS / "e2e.yml")
        for author in ("nunocgoncalves", "dependabot[bot]"):
            for actor in ("nunocgoncalves", "dependabot[bot]"):
                for fork in (False, True):
                    with self.subTest(author=author, actor=actor, fork=fork):
                        self.assertEqual(
                            condition_matches(jobs["preview"]["if"], self.context(author=author, actor=actor, fork=fork)),
                            author != "dependabot[bot]" and not fork,
                        )

    def test_staging_and_existing_no_preview_cases(self) -> None:
        _, jobs = parse_jobs(WORKFLOWS / "e2e.yml")
        cases = [
            ({"event": "push", "author": "", "actor": "dependabot[bot]"}, True),
            ({"event": "merge_group"}, False),
            ({"event": "workflow_dispatch"}, False),
            ({"build": "[]"}, False),
            ({"event": "push", "build": "[]"}, False),
            ({"full": True}, False),
            ({"event": "push", "full": True}, False),
        ]
        for changes, expected in cases:
            with self.subTest(**changes):
                self.assertEqual(condition_matches(jobs["preview"]["if"], self.context(**changes)), expected)

    def test_teardown_uses_pr_author_not_closing_actor(self) -> None:
        _, jobs = parse_jobs(WORKFLOWS / "preview.yml")
        for author in ("nunocgoncalves", "dependabot[bot]"):
            for actor in ("nunocgoncalves", "dependabot[bot]"):
                for fork in (False, True):
                    with self.subTest(author=author, actor=actor, fork=fork):
                        self.assertEqual(
                            condition_matches(jobs["teardown"]["if"], self.context(author=author, actor=actor, fork=fork)),
                            author != "dependabot[bot]" and not fork,
                        )

    def test_required_validation_has_no_bot_or_actor_filter(self) -> None:
        for workflow in ("ci.yml", "e2e.yml"):
            _, jobs = parse_jobs(WORKFLOWS / workflow)
            for name, job in jobs.items():
                if workflow == "e2e.yml" and name == "preview":
                    continue
                with self.subTest(workflow=workflow, job=name):
                    self.assertNotRegex(job["if"], r"(?i)dependabot|github\.(?:actor|triggering_actor)|pull_request\.user")
            self.assertEqual(jobs["required"]["if"], "always()")
        _, jobs = parse_jobs(WORKFLOWS / "e2e.yml")
        self.assertEqual(set(re.findall(r"[\w-]+", jobs["required"]["needs"])),
                         {"select", "build", "images", "kind", "bake", "real-machine"})


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Preview demo seed (DES-HOR-459-06): manifests and idempotent shell behaviour."""
from __future__ import annotations

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import preview_demo  # noqa: E402

PASSWORD = "preview-demo-secret-1234"

# Only curl and sudo (kubectl) are stubbed; the generated script runs for real.
STUB = r'''#!/usr/bin/env python3
import json,os,pathlib,sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ['STUB_ROOT'])
mode=os.environ['STUB_MODE']
with (root/'calls').open('a') as out: out.write(json.dumps([name,*args])+'\n')
if name == 'sleep': sys.exit(0)
if name == 'sudo':
  if 'apply' in args: (root/'applied').write_text(sys.stdin.read())
  sys.exit(0)
def opt(flag):
  return args[args.index(flag)+1] if flag in args else None
def header(prefix):
  return next((a.split(': ',1)[1] for a in args if a.startswith(prefix + ': ')), None)
body_file, headers_file, url = opt('-o'), opt('-D'), args[-1]
data = opt('--data')
if data and data.startswith('@'):
  (root/'sign-in').write_text(pathlib.Path(data[1:]).read_text())
def reply(status, body):
  pathlib.Path(body_file).write_text(json.dumps(body)); print(status, end='')
if url.endswith('/v1/auth/sign-in'):
  if mode == 'sign-in-failure': reply(401, {'error': 'no'}); sys.exit(0)
  pathlib.Path(headers_file).write_text('HTTP/1.1 200 OK\r\nSet-Cookie: __Host-iterabase_session=s3ss; Path=/; HttpOnly; Secure\r\n\r\n')
  reply(200, {'csrfToken': 'csrf-1', 'profile': {}})
elif url.endswith('/v1/work-items') and '-G' in args:
  titles = [a.split('=',1)[1] for a in args if a.startswith('search=')]
  reply(200, [{'title': t} for t in titles] if mode == 'replay' else [])
elif url.endswith('/v1/work-items'):
  key = header('Idempotency-Key')
  reply(200 if mode == 'replay' else 201, {'id': 'item-' + key})
elif url.endswith('/v1/artifacts'):
  reply(201, {'artifactId': 'artifact-' + str(len((root/'calls').read_text().splitlines()))})
elif url.endswith('/wait/stop'):
  # A stopped wait asks again; the second stop passes maxTransitions.
  item = url.split('/')[-3]
  stops = root / ('stops-' + item)
  n = int(stops.read_text()) + 1 if stops.exists() else 1
  stops.write_text(str(n))
  (root / ('state-' + item)).write_text('failed' if n >= 2 else 'blocked')
  reply(200, {'state': 'blocked'})
elif url.endswith('/blocker'):
  reply(200, {'id': 'blocker-' + url.split('/')[-2]})
elif '/v1/work-blockers/' in url:
  (root / ('state-' + url.split('/')[-2].removeprefix('blocker-'))).write_text('waiting')
  reply(200, {'state': 'resolved'})
elif '/v1/work-items/item-' in url:
  item = url.rsplit('/', 1)[1]
  state = root / ('state-' + item)
  reply(200, {'id': item, 'state': state.read_text() if state.exists() else 'waiting'})
else:
  reply(404, {})
'''


def workflows() -> list[dict]:
    return [doc for doc in preview_demo.manifests("model-a") if doc["kind"] == "Workflow"]


class Manifests(unittest.TestCase):
    def test_quotes_lines_are_typed(self) -> None:
        quotes = next(doc["spec"] for doc in workflows() if doc["spec"]["key"] == "demo/quotes")
        lines = quotes["coworker"]["onItsOwn"]
        self.assertEqual([line["kind"] for line in lines], ["reads", "reads", "sends", "does"])
        self.assertEqual(lines[0]["detail"]["en"], "quotes@exemplo.pt · emails and attachments")
        self.assertEqual(len(lines[2]["items"]), 6)
        self.assertIn("rates@northsealines.example", lines[2]["items"])
        self.assertNotIn("items", lines[0])

    def test_pool_never_runs_workers(self) -> None:
        pool = next(doc for doc in preview_demo.manifests("model-a") if doc["kind"] == "AgentPool")
        self.assertEqual(pool["spec"]["replicas"], 0)
        self.assertEqual(pool["spec"]["networkPolicy"], {"egress": "denied"})

    def test_workflows_declare_bilingual_coworkers(self) -> None:
        for workflow in workflows():
            spec = workflow["spec"]
            self.assertEqual(spec["poolRef"], preview_demo.POOL)
            self.assertNotIn("presentation", spec)
            lines = spec["coworker"]["onItsOwn"]
            for text in [spec["title"], spec["coworker"]["role"], spec["coworker"]["summary"],
                         *(line["text"] for line in lines), *(line["detail"] for line in lines if "detail" in line),
                         *spec["coworker"]["connections"]]:
                self.assertTrue(text["en"] and text["pt"], text)
            self.assertLessEqual(len(lines), 12)
            for line in lines:
                self.assertIn(line["kind"], {"reads", "sends", "does"})
                self.assertLessEqual(len(line.get("items", [])), 12)
                for item in line.get("items", []):
                    self.assertTrue(0 < len(item) <= 200, item)
            self.assertIn(spec["coworker"]["mark"]["shape"], {"round", "square", "petal"})

    def test_every_outcome_is_routed_or_terminal(self) -> None:
        for workflow in workflows():
            graph = workflow["spec"]["graph"]
            routed = {(edge["from"], edge["outcome"]) for edge in graph["edges"]}
            terminal = {(end["node"], end["outcome"]) for end in graph["terminalOutcomes"]}
            for node in graph["nodes"]:
                for outcome in node["outcomes"]:
                    self.assertIn((node["key"], outcome), routed | terminal, (workflow["metadata"]["name"], node["key"]))

    def test_work_starts_at_a_human_gate(self) -> None:
        for workflow in workflows():
            graph = workflow["spec"]["graph"]
            entry = next(node for node in graph["nodes"] if node["key"] == graph["entryNode"])
            self.assertEqual(entry["kind"], "human_gate")

    def test_steps_follow_the_graph(self) -> None:
        """Walk each item's steps through its workflow: every answer is an
        outcome of the gate the work is at, uploads answer artifact gates, and
        stops happen at waits."""
        by_key = {doc["spec"]["key"]: doc["spec"] for doc in workflows()}
        for key, workflow, _, _, steps, attached in preview_demo.WORK:
            self.assertIn(workflow, by_key, key)
            graph = by_key[workflow]["graph"]
            nodes = {node["key"]: node for node in graph["nodes"]}
            edges = {(edge["from"], edge["outcome"]): edge["to"] for edge in graph["edges"]}
            at = graph["entryNode"]
            for step in steps:
                node = nodes[at]
                if step[0] == "answer":
                    self.assertEqual(node["kind"], "human_gate", (key, at))
                    self.assertNotEqual(node["humanGate"]["type"], "artifact", (key, at))
                    self.assertIn(step[1], node["outcomes"], (key, at))
                    at = edges.get((at, step[1]), at)
                elif step[0] == "upload":
                    self.assertEqual(node["humanGate"]["type"], "artifact", (key, at))
                    self.assertIn(step[2], preview_demo.DOCS)
                    at = edges[(at, step[1])]
                elif step[0] == "stop":
                    self.assertEqual(node["kind"], "wait", (key, at))
                    at = edges[(at, "cancelled")]
                else:
                    self.assertEqual(step, ("until_failed",))
                    self.assertEqual(node["kind"], "wait", (key, at))
                    self.assertEqual(edges[(edges[(at, "cancelled")], "retry")], at, key)
            for name in attached:
                self.assertIn(name, preview_demo.DOCS)

    def test_customs_uses_every_node_kind_and_gate_type(self) -> None:
        customs = next(doc["spec"] for doc in workflows() if doc["spec"]["key"] == "demo/customs")
        nodes = customs["graph"]["nodes"]
        self.assertEqual({node["kind"] for node in nodes}, {"agent_task", "human_gate", "wait"})
        self.assertEqual({node["humanGate"]["type"] for node in nodes if node["kind"] == "human_gate"},
                         {"information", "artifact", "decision", "approval"})
        details = next(node for node in nodes if node["key"] == "details")
        kinds = {spec.get("type", "enum") for spec in details["humanGate"]["responseSchema"]["properties"].values()}
        self.assertEqual(kinds, {"string", "enum", "integer", "number", "boolean"})

    def test_open_wait_keys_are_unique(self) -> None:
        """An open wait's correlation key is unique in the installation, so no
        two demo items may wait on the same booking reference."""
        references = [step[2]["reference"] for entry in preview_demo.WORK for step in entry[4]
                      if step[0] == "answer" and "reference" in step[2]]
        self.assertEqual(len(references), len(set(references)))

    def test_documents_are_real_files(self) -> None:
        for name, (mime, data) in preview_demo.DOCS.items():
            if mime == "application/pdf":
                self.assertTrue(data.startswith(b"%PDF-1.4") and data.rstrip().endswith(b"%%EOF"), name)
            else:
                self.assertIn(b"Subject: ", data, name)


class Script(unittest.TestCase):
    def run_seed(self, mode: str) -> tuple[subprocess.CompletedProcess[str], pathlib.Path]:
        root = pathlib.Path(tempfile.mkdtemp())
        bin_dir = root / "bin"
        bin_dir.mkdir()
        for name in ("curl", "sudo", "sleep"):
            path = bin_dir / name
            path.write_text(STUB)
            path.chmod(0o755)
        script = preview_demo.seed_script("http://10.0.0.1:80", "pr-1-app.tail.ts.net",
                                          "admin@preview.iterabase.test", "model-a")
        env = {**os.environ, "PATH": f"{bin_dir}:{os.environ['PATH']}", "STUB_ROOT": str(root), "STUB_MODE": mode}
        remote = preview_demo.REMOTE.removeprefix("bash -c ").strip("'")
        result = subprocess.run(["bash", "-c", remote], input=f"{PASSWORD}\n{script}", env=env,
                                capture_output=True, text=True)
        return result, root

    def calls(self, root: pathlib.Path) -> list[list[str]]:
        return [json.loads(line) for line in (root / "calls").read_text().splitlines()]

    def test_seeds_new_work_and_answers_only_what_moves_on(self) -> None:
        result, root = self.run_seed("fresh")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("preview demo: Quotes, Rates and Customs seeded", result.stdout)
        curls = [call for call in self.calls(root) if call[0] == "curl"]
        starts = [call for call in curls if call[-1].endswith("/v1/work-items") and "-G" not in call]
        answers = [call for call in curls if "/v1/work-blockers/" in call[-1]]
        uploads = [call for call in curls if call[-1].endswith("/v1/artifacts")]
        stops = [call for call in curls if call[-1].endswith("/wait/stop")]
        self.assertEqual(len(starts), len(preview_demo.WORK))
        fixed = [step for entry in preview_demo.WORK for step in entry[4]]
        self.assertGreaterEqual(len(answers), sum(1 for step in fixed if step[0] in {"answer", "upload"}))
        self.assertEqual(len(uploads), sum(len(entry[5]) for entry in preview_demo.WORK)
                         + sum(1 for step in fixed if step[0] == "upload"))
        self.assertGreaterEqual(len(stops), sum(1 for step in fixed if step[0] in {"stop", "until_failed"}))
        for call in starts + answers + uploads + stops:
            self.assertIn("Origin: https://pr-1-app.tail.ts.net", call)
            self.assertIn("X-CSRF-Token: csrf-1", call)
            self.assertIn("Cookie: __Host-iterabase_session=s3ss", call)
        keys = [arg for call in starts for arg in call if arg.startswith("Idempotency-Key: ")]
        self.assertEqual(keys, [f"Idempotency-Key: {entry[0]}" for entry in preview_demo.WORK])
        self.assertIn("preview-demo-quotes", (root / "applied").read_text())

    def test_password_never_leaves_the_sign_in_body(self) -> None:
        result, root = self.run_seed("fresh")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads((root / "sign-in").read_text())["password"], PASSWORD)
        self.assertNotIn(PASSWORD, (root / "calls").read_text())
        self.assertNotIn(PASSWORD, result.stdout + result.stderr)
        self.assertNotIn(PASSWORD, preview_demo.seed_script("http://t", "h", "a@b.cd", "m"))

    def test_a_redeploy_changes_nothing(self) -> None:
        result, root = self.run_seed("replay")
        self.assertEqual(result.returncode, 0, result.stderr)
        curls = [call for call in self.calls(root) if call[0] == "curl"]
        self.assertFalse([call for call in curls if "/blocker" in call[-1] or "/v1/work-blockers/" in call[-1]
                          or call[-1].endswith("/v1/artifacts") or call[-1].endswith("/wait/stop")])

    def test_a_failed_sign_in_fails_the_deploy(self) -> None:
        result, _ = self.run_seed("sign-in-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("preview demo: sign-in returned 401", result.stdout)


if __name__ == "__main__":
    unittest.main()

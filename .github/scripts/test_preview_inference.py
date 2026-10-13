#!/usr/bin/env python3
"""Workload preview smoke generation and fail-closed shell behavior."""
from __future__ import annotations

import base64
import json
import os
import pathlib
import struct
import subprocess
import sys
import tempfile
import unittest
import zlib

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import preview  # noqa: E402
import preview_inference  # noqa: E402

# Only the shell mechanics are stubbed here. The database integration test uses
# the real migrations, and preview CI uses cert-manager + the actual gateway.
STUB = r'''#!/usr/bin/env python3
import base64,json,os,pathlib,sys
name=pathlib.Path(sys.argv[0]).name
args=sys.argv[1:]
root=pathlib.Path(os.environ['STUB_ROOT'])
mode=os.environ['STUB_MODE']
with (root/'calls').open('a') as out: out.write(json.dumps([name,*args])+'\n')
if name == 'sleep': sys.exit(0)
if name == 'sudo':
 if 'exec' in args:
  sql=sys.stdin.read()
  with (root/'sql').open('a') as out: out.write(sql)
  if 'DELETE FROM runtime.workflow_runs' in sql:
   (root/'revoked').touch()
   if mode == 'cleanup-failure': sys.exit(1)
 elif 'delete' in args and mode=='certificate-cleanup-failure': sys.exit(1)
 elif 'apply' in args: sys.stdin.read()
 elif 'get' in args and any(a.startswith('secret/') for a in args):
  print(json.dumps({'data':{k:base64.b64encode(v.encode()).decode() for k,v in
   {'tls.crt':'synthetic-cert','tls.key':'PRIVATE-KEY-MUST-NOT-LEAK','ca.crt':'synthetic-ca'}.items()}}))
 elif 'get' in args: print('10.43.0.9')
 sys.exit(0)
if '--version' in args:
 print('Features: HTTP2 SSL'); sys.exit(0)
out=pathlib.Path(args[args.index('-o')+1])
body=json.loads(pathlib.Path(args[args.index('--data-binary')+1][1:]).read_text())
headers=[args[i+1] for i,a in enumerate(args) if a=='-H']
if any(h.startswith('Authorization: Bearer') for h in headers):
 out.write_text(json.dumps({'error':{'code':'invalid_api_key'}}))
 print('200' if mode=='bearer-accepted' else '401',end=''); sys.exit(0)
assert '--http2' in args and '--cacert' in args and '--cert' in args and '--key' in args
assert '--insecure' not in args and '-k' not in args
assert pathlib.Path(args[args.index('--key')+1]).read_text()=='PRIVATE-KEY-MUST-NOT-LEAK'
if 'X-Iterabase-Fencing-Generation: 2' in headers or (root/'revoked').exists():
 out.write_text(json.dumps({'error':{'code':'pool_not_authorized' if (root/'revoked').exists() else 'turn_not_authorized'}}))
 print('403 2',end=''); sys.exit(0)
# Fail once, then allow success: a retry must never make the smoke pass.
fail_once={
 'timeout-once':(28,'000 0',None),
 'tls-once':(60,'000 0',None),
 'connection-once':(7,'000 0',None),
 'not-found-once':(0,'404 2','model_not_found'),
 'unavailable-once':(0,'503 2','scope_unavailable'),
 'backend-unavailable-once':(0,'503 2','model_unavailable'),
 'capabilities-pending':(0,'403 2','model_not_allowed'),
}
if mode in fail_once and not (root/'failure-injected').exists():
 (root/'failure-injected').touch()
 exit_code,status,error=fail_once[mode]
 if error is not None: out.write_text(json.dumps({'error':{'code':error}}))
 print(status,end=''); sys.exit(exit_code)
if mode=='workload-denied':
 out.write_text(json.dumps({'error':{'code':'invalid_workload_identity'}}))
 print('403 2',end=''); sys.exit(0)
if mode=='upstream-auth-failure':
 out.write_text(json.dumps({'error':{'code':'upstream_auth_failed'}}))
 print('502 2',end=''); sys.exit(0)
if body.get('stream'):
 data='data: '+json.dumps({'choices':[{'delta':{'content':'blue'}}]})+'\n\n'
 if mode=='stream-error': data+='data: '+json.dumps({'error':{'message':'PRIVATE-KEY-MUST-NOT-LEAK'}})+'\n\n'
 if mode!='missing-done': data+='data: [DONE]\n\n'
 if mode=='duplicate-done': data+='data: [DONE]\n\n'
 out.write_text(data)
else:
 out.write_text(json.dumps({'choices':[{'message':{'content':None,'reasoning_content':'blue'}}],
  'usage':{'completion_tokens':0 if mode=='empty-answer' else 2}}))
print('200 1.1' if mode=='http1' else '200 2',end='')
'''


class PreviewInferenceTests(unittest.TestCase):
    def test_real_png_and_safe_fixture(self) -> None:
        png = base64.b64decode(preview_inference.smoke_image().split(",", 1)[1], validate=True)
        self.assertEqual(png[:8], b"\x89PNG\r\n\x1a\n")
        size = struct.unpack(">I", png[8:12])[0]
        self.assertEqual(png[12:16], b"IHDR")
        self.assertEqual(struct.unpack(">II", png[16:24]), (64, 64))
        self.assertEqual(struct.unpack(">I", png[16+size:20+size])[0], zlib.crc32(png[12:16+size]) & 0xFFFFFFFF)
        scope = preview_inference.fixture("provider's-model")
        self.assertIn("'provider''s-model'", scope["seed"])
        self.assertIn("fencing_generation", scope["seed"])
        self.assertIn(scope["run"], scope["cleanup"])
        certificate = json.loads(scope["certificate"])
        self.assertEqual(certificate["spec"]["duration"], "1h")
        self.assertEqual(certificate["spec"]["usages"], ["digital signature", "client auth"])
        self.assertTrue(certificate["spec"]["uris"][0].endswith("/workers/" + scope["name"]))
        self.assertNotIn("api_keys", scope["seed"])

    def test_preview_enables_workload_listener(self) -> None:
        images = {name: {"repository": name, "tag": "sha-test"} for name in preview.PREVIEW_IMAGES}
        values = preview.preview_values(images, "model", {"app": "app.test", "inference": "inference.test"}, "admin@test.test")
        self.assertIn("workload:\n    enabled: true", values)

    def run_smoke(self, mode: str) -> tuple[subprocess.CompletedProcess[str], list[list[str]], str]:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            stub = root / "stub"
            stub.write_text(STUB, encoding="utf-8")
            stub.chmod(0o755)
            for name in ("sudo", "curl", "sleep"):
                (root / name).symlink_to(stub)
            script = preview_inference.smoke_script("http://10.43.0.1:80", "inference.preview.test", "model")
            syntax = subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True, check=False)
            self.assertEqual(syntax.returncode, 0, syntax.stderr)
            env = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                   "STUB_ROOT": str(root), "STUB_MODE": mode}
            result = subprocess.run(["bash", "-s"], input=script, text=True, capture_output=True, env=env, timeout=20, check=False)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            sql = (root / "sql").read_text()
            self.assertIn("DELETE FROM runtime.workflow_runs", sql, "cleanup runs even on failure")
            self.assertTrue(any("delete" in call and any(a.startswith("certificate/") for a in call) for call in calls))
            for call in calls:
                if "--key" in call:
                    self.assertFalse(pathlib.Path(call[call.index("--key")+1]).exists(), "private temp file removed")
            self.assertNotIn("PRIVATE-KEY-MUST-NOT-LEAK", result.stdout + result.stderr)
            return result, calls, sql

    def test_positive_and_both_authorization_negatives(self) -> None:
        result, calls, _ = self.run_smoke("success")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for stage in ("customer bearer -> 401", "fenced generation -> 403 2", "workload text -> 200 2",
                      "workload vision -> 200 2", "workload vision-stream -> 200 2", "removed assignment -> 403 2"):
            self.assertIn(stage, result.stdout)
        self.assertIn("inference smoke passed", result.stdout)
        self.assertFalse(any("logs" in call or "bootstrap" in call for call in calls))

    def test_completion_errors_are_not_retried(self) -> None:
        for mode in ("timeout-once", "tls-once", "connection-once", "not-found-once", "unavailable-once",
                     "backend-unavailable-once", "capabilities-pending"):
            with self.subTest(mode=mode):
                result, calls, sql = self.run_smoke(mode)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn("inference smoke passed", result.stdout)
                posts = [call for call in calls if call[0] == "curl" and "--data-binary" in call
                         and "X-Iterabase-Fencing-Generation: 1" in call]
                self.assertEqual(len(posts), 1, "a later successful completion must not mask the first failure")
                self.assertFalse(any(call[0] == "sleep" for call in calls), "completion assertions never poll")
                self.assertIn("NOTIFY permissions_changed", sql)

    def test_fail_closed_and_always_cleanup(self) -> None:
        for mode in ("bearer-accepted", "workload-denied", "upstream-auth-failure", "empty-answer", "http1", "stream-error",
                     "missing-done", "duplicate-done", "cleanup-failure", "certificate-cleanup-failure"):
            with self.subTest(mode=mode):
                result, _, _ = self.run_smoke(mode)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("inference smoke passed", result.stdout)
                if mode == "workload-denied":
                    self.assertNotIn("workload vision ->", result.stdout, "live-scope denial fails before vision")


if __name__ == "__main__":
    unittest.main()

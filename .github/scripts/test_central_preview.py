#!/usr/bin/env python3
"""Preview installations at production central (HOR-661, DES-HOR-661-09)."""
from __future__ import annotations

import argparse
import contextlib
import io
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import central_preview  # noqa: E402
import preview  # noqa: E402

CA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
ENV = {"CENTRAL_URL": "https://central.iterabase.test", "CENTRAL_CA_BUNDLE": CA}
# A stub forge: records argv, answers per STUB_MODE, never sees real keys.
STUB = r'''#!/usr/bin/env python3
import json,os,pathlib,sys
root=pathlib.Path(os.environ["STUB_ROOT"]); mode=os.environ.get("STUB_MODE","ok")
with (root/"calls").open("a") as out: out.write(json.dumps(sys.argv[1:])+"\n")
command=sys.argv[2]
if mode=="not-found" and command=="revoke":
    print("Error: central: 404 installation_not_found: unknown installation", file=sys.stderr); sys.exit(1)
if mode=="fail":
    print("Error: central: 403 operator_forbidden", file=sys.stderr); sys.exit(1)
if command=="issue":
    print("apiVersion: v1\nkind: Secret\nmetadata:\n  name: iterabase-central-client\ntype: kubernetes.io/tls\ndata:\n  tls.key: PRIVATE\n")
'''


USAGE_STUB = r'''#!/usr/bin/env python3
import base64,json,os,pathlib,sys
args=sys.argv[1:]; root=pathlib.Path(os.environ["STUB_ROOT"]); mode=os.environ["STUB_MODE"]
name=pathlib.Path(sys.argv[0]).name
with (root/"calls").open("a") as out: out.write(json.dumps([name,*args])+"\n")
if name=="sudo":
 assert "secret/iterabase-central-client" in args
 print(json.dumps({"data":{k:base64.b64encode(v.encode()).decode() for k,v in
  {"tls.crt":"synthetic-cert","tls.key":"PRIVATE-KEY-MUST-NOT-LEAK","ca.crt":"synthetic-ca"}.items()}}))
 sys.exit(0)
assert "--cert" in args and "--key" in args and "--cacert" in args and "--max-time" in args
assert "-k" not in args and "--insecure" not in args and "-L" not in args
key=pathlib.Path(args[args.index("--key")+1])
assert key.read_text()=="PRIVATE-KEY-MUST-NOT-LEAK" and key.stat().st_mode & 0o777==0o600
if mode=="transport-failure": sys.exit(7)
view={"installation_id":"pr-8" if mode=="wrong-installation" else "pr-7", "deployment":"internal",
 "billing_currency":"USD" if mode=="wrong-currency" else "EUR", "as_of":"2026-10-17T00:00:00Z",
 "period":{"kind":"month","start":"2026-10-01T00:00:00Z","end":"2026-11-01T00:00:00Z","used_credits":"0.000000"},
 "not-for-evidence":"PRIVATE-KEY-MUST-NOT-LEAK"}
pathlib.Path(args[args.index("-o")+1]).write_text("not JSON" if mode=="invalid-json" else json.dumps(view))
print("403" if mode=="forbidden" else "200",end="")
'''


class UsageReadTests(unittest.TestCase):
    def run_usage(self, mode: str) -> subprocess.CompletedProcess[str]:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            stub = root / "stub"
            stub.write_text(USAGE_STUB, encoding="utf-8")
            stub.chmod(0o755)
            for name in ("sudo", "curl"):
                (root / name).symlink_to(stub)
            central = central_preview.Central.from_env(root, env=ENV)
            script = central.usage_script("pr-7", "iterabase-system")
            syntax = subprocess.run(["bash", "-n"], input=script, text=True, capture_output=True, check=False)
            self.assertEqual(syntax.returncode, 0, syntax.stderr)
            env = {**os.environ, "PATH": str(root) + os.pathsep + os.environ["PATH"],
                   "STUB_ROOT": directory, "STUB_MODE": mode}
            result = subprocess.run(["bash", "-s"], input=script, text=True, capture_output=True, env=env,
                                    timeout=20, check=False)
            calls = [json.loads(line) for line in (root / "calls").read_text().splitlines()]
            curls = [call for call in calls if call[0] == "curl"]
            self.assertEqual(len(curls), 1, "usage reads are not retried")
            self.assertFalse(pathlib.Path(curls[0][curls[0].index("--key") + 1]).exists(), "private temp file removed")
            self.assertNotIn("PRIVATE-KEY-MUST-NOT-LEAK", result.stdout + result.stderr)
            return result

    def test_authenticated_usage_is_scoped_and_content_free(self) -> None:
        result = self.run_usage("success")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        view = json.loads(result.stdout)
        self.assertEqual(view["installation_id"], "pr-7")
        self.assertEqual(view["period"]["used_credits"], "0.000000", "internal usage is company-funded, not charged")
        self.assertNotIn("not-for-evidence", view)

    def test_usage_read_fails_closed_and_cleans_up(self) -> None:
        for mode in ("wrong-installation", "wrong-currency", "transport-failure", "forbidden", "invalid-json"):
            with self.subTest(mode=mode):
                self.assertNotEqual(self.run_usage(mode).returncode, 0)


class SettingsTests(unittest.TestCase):
    def test_requires_both_variables_without_fallback(self) -> None:
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
            central_preview.settings({"CENTRAL_CA_BUNDLE": CA})
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_CA_BUNDLE"):
            central_preview.settings({"CENTRAL_URL": ENV["CENTRAL_URL"]})
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
            central_preview.settings({**ENV, "CENTRAL_URL": "http://central.iterabase.test"})
        self.assertEqual(central_preview.settings(ENV), ("https://central.iterabase.test", CA + "\n"))

    def test_unset_or_blank_configuration_is_never_optional(self) -> None:
        for env in ({}, {"CENTRAL_URL": " ", "CENTRAL_CA_BUNDLE": ""}):
            with self.subTest(env=env), self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
                central_preview.settings(env)

    def test_only_preview_names(self) -> None:
        for name in ("pr-1", "pr-661", "staging"):
            self.assertEqual(central_preview.require_name(name), name)
        for name in ("acme", "pr-", "pr-01", "staging-2", "PR-1"):
            with self.assertRaises(central_preview.CentralError):
                central_preview.require_name(name)


class CentralTests(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.directory.name)
        stub = self.root / "forge"
        stub.write_text(STUB, encoding="utf-8")
        stub.chmod(0o755)
        self.central = central_preview.Central.from_env(self.root, forge=[str(stub)], env=ENV)

    def tearDown(self) -> None:
        self.directory.cleanup()

    def calls(self) -> list[list[str]]:
        return [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]

    def run_mode(self, mode: str):
        return mock.patch.dict(os.environ, {"STUB_ROOT": str(self.root), "STUB_MODE": mode})

    def test_register_is_an_internal_euro_installation_with_pinned_ca(self) -> None:
        with self.run_mode("ok"):
            self.central.register("pr-7")
        (call,) = self.calls()
        self.assertEqual(call[:9], ["central", "register", "--installation", "pr-7", "--deployment", "internal",
                                    "--plan", "none", "--currency"])
        self.assertEqual(call[call.index("--currency") + 1], "EUR")
        self.assertEqual(call[call.index("--central-url") + 1], "https://central.iterabase.test")
        ca_path = pathlib.Path(call[call.index("--ca-bundle") + 1])
        self.assertEqual(ca_path.read_text(encoding="utf-8"), CA + "\n")

    def test_secret_manifest_is_returned_not_printed(self) -> None:
        with self.run_mode("ok"), mock.patch("sys.stdout") as stdout:
            manifest = self.central.secret_manifest("staging", "iterabase-system")
        stdout.write.assert_not_called()
        self.assertIn("kind: Secret", manifest)
        call = self.calls()[0]
        self.assertIn("--secret-manifest", call)
        self.assertEqual(call[call.index("--secret-name") + 1], central_preview.SECRET_NAME)
        self.assertNotIn("--out", call)

    def test_revoke_tolerates_never_registered_but_not_other_failures(self) -> None:
        with self.run_mode("ok"):
            self.assertTrue(self.central.revoke("pr-7", "closed"))
        with self.run_mode("not-found"):
            self.assertFalse(self.central.revoke("pr-7", "closed"))
        with self.run_mode("fail"), self.assertRaisesRegex(central_preview.CentralError, "operator_forbidden"):
            self.central.revoke("pr-7", "closed")

    def test_cli_revokes_each_name_and_skips_empty(self) -> None:
        self.assertEqual(central_preview.main(["revoke", "--installations", "", "--reason", "reaped"]), 0)
        stub = (str(self.root / "forge"),)
        with self.run_mode("ok"), mock.patch.dict(os.environ, ENV), \
                mock.patch.object(central_preview.Central, "from_env",
                                  classmethod(lambda cls, workdir: cls(ENV["CENTRAL_URL"], self.root / "ca", stub))):
            code = central_preview.main(["revoke", "--installations", "pr-1,staging", "--reason", "reaped",
                                         "--workdir", str(self.root)])
        self.assertEqual(code, 0)
        self.assertEqual([call[3] for call in self.calls()], ["pr-1", "staging"])

    def test_cli_revoke_requires_central_for_every_nonempty_installation_list(self) -> None:
        for env, missing in (({}, "CENTRAL_URL"), ({"CENTRAL_URL": ENV["CENTRAL_URL"]}, "CENTRAL_CA_BUNDLE"),
                             ({"CENTRAL_CA_BUNDLE": CA}, "CENTRAL_URL")):
            with self.subTest(env=env), mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch.object(central_preview.Central, "_run") as operator, \
                    contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(central_preview.main(["revoke", "--installations", "pr-1", "--reason", "reaped"]), 1)
                self.assertIn(missing, stderr.getvalue())
                operator.assert_not_called()
        self.assertFalse((self.root / "calls").exists())


class PreviewDeployTests(unittest.TestCase):
    def args(self) -> argparse.Namespace:
        images = {name: {"repository": name, "tag": "sha-test"} for name in preview.PREVIEW_IMAGES}
        return argparse.Namespace(name="pr-7", images=json.dumps(images), address="192.0.2.1", host_key="pinned-host-key",
                                  ssh_key="/synthetic/ssh-key", data_device="/synthetic/data", run_number="1",
                                  source_sha="a" * 40, created="true", admin_email="admin@test.test",
                                  admin_mode="onboarding", model_id="deepseek-v4-flash")

    def test_missing_central_fails_deploy_before_any_side_effect(self) -> None:
        for env, missing in (({}, "CENTRAL_URL"), ({"CENTRAL_URL": " "}, "CENTRAL_URL"),
                             ({"CENTRAL_CA_BUNDLE": CA}, "CENTRAL_URL"),
                             ({"CENTRAL_URL": ENV["CENTRAL_URL"]}, "CENTRAL_CA_BUNDLE")):
            with self.subTest(env=env), mock.patch.dict(os.environ, env, clear=True), \
                    mock.patch.object(preview.tempfile, "mkdtemp") as workdir, \
                    mock.patch.object(preview, "Host") as host, \
                    mock.patch.object(preview, "package_charts") as packages, \
                    mock.patch.object(preview, "run", side_effect=AssertionError("no commands before central validation")) as command:
                with self.assertRaisesRegex(central_preview.CentralError, missing):
                    preview.deploy(self.args())
                workdir.assert_not_called()
                host.assert_not_called()
                packages.assert_not_called()
                command.assert_not_called()

    @contextlib.contextmanager
    def deployment(self):
        with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
            root = pathlib.Path(directory)
            stack.enter_context(mock.patch.dict(os.environ, ENV, clear=True))
            stack.enter_context(mock.patch.object(preview.tempfile, "mkdtemp", return_value=directory))
            stack.enter_context(mock.patch.object(preview.e2e_inputs, "chart_version", return_value="0.5.1"))
            mocks = {}
            for name in ("Host", "package_charts", "run", "forge_config", "tailnet_hosts", "overlay_archive",
                         "import_images", "central_identity", "managed_model", "seed_admin", "inference_smoke", "central_usage",
                         "serve_on_tailnet"):
                mocks[name] = stack.enter_context(mock.patch.object(preview, name))
            mocks["package_charts"].return_value = {"iterabase-platform": root / "chart.tgz"}
            mocks["forge_config"].return_value = root / "forge.yaml"
            mocks["tailnet_hosts"].return_value = {"app": "app.test", "inference": "inference.test"}
            mocks["overlay_archive"].return_value = b"synthetic overlay"
            mocks["serve_on_tailnet"].return_value = {"url": "https://app.test", "inference_url": "https://inference.test/v1"}
            mocks["apply"] = stack.enter_context(mock.patch.object(preview.subprocess, "run"))
            mocks["central"] = stack.enter_context(mock.patch.object(central_preview.Central, "from_env"))
            order = mock.Mock()
            for name in ("central_identity", "apply", "managed_model", "inference_smoke", "central_usage", "serve_on_tailnet"):
                order.attach_mock(mocks[name], name)
            yield mocks, order

    def test_deploy_always_installs_managed_identity_backend_and_runs_smoke(self) -> None:
        with self.deployment() as (calls, order):
            result = preview.deploy(self.args())
            host = calls["Host"].return_value
            calls["central_identity"].assert_called_once_with(host, calls["central"].return_value, "pr-7")
            calls["managed_model"].assert_called_once_with(host, "deepseek-v4-flash")
            calls["inference_smoke"].assert_called_once_with(host, calls["tailnet_hosts"].return_value, "deepseek-v4-flash")
            stages = [call[0] for call in order.mock_calls]
            self.assertLess(stages.index("central_identity"), stages.index("apply", stages.index("central_identity")))
            self.assertLess(stages.index("managed_model"), stages.index("inference_smoke"))
            calls["central_usage"].assert_called_once_with(host, calls["central"].return_value, "pr-7")
            self.assertLess(stages.index("inference_smoke"), stages.index("central_usage"))
            self.assertLess(stages.index("central_usage"), stages.index("serve_on_tailnet"))
            self.assertEqual(result["url"], "https://app.test")
            self.assertNotIn("managed_inference", result)

    def test_failed_managed_smoke_cannot_publish_a_successful_preview(self) -> None:
        with self.deployment() as (calls, _):
            calls["inference_smoke"].side_effect = preview.PreviewError("synthetic smoke failure")
            with self.assertRaisesRegex(preview.PreviewError, "synthetic smoke failure"):
                preview.deploy(self.args())
            calls["serve_on_tailnet"].assert_not_called()

    def test_failed_usage_read_cannot_publish_a_successful_preview(self) -> None:
        with self.deployment() as (calls, _):
            calls["central_usage"].side_effect = subprocess.CalledProcessError(1, "synthetic usage read")
            with self.assertRaises(subprocess.CalledProcessError):
                preview.deploy(self.args())
            calls["serve_on_tailnet"].assert_not_called()


class PreviewValuesTests(unittest.TestCase):
    def test_gateway_routes_managed_inference_through_central(self) -> None:
        images = {name: {"repository": name, "tag": "sha-test"} for name in preview.PREVIEW_IMAGES}
        values = preview.preview_values(images, {"app": "app.test", "inference": "inference.test"}, "admin@test.test",
                                        ("https://central.iterabase.test", CA + "\n"))
        gateway = values[values.index("inference-gateway:"):]
        expected = ("  central:\n    enabled: true\n    url: \"https://central.iterabase.test\"\n"
                    f"    tlsSecretName: \"{central_preview.SECRET_NAME}\"\n    caBundle: {json.dumps(CA + chr(10))}\n")
        self.assertIn(expected, gateway)

    def test_preview_no_longer_takes_a_provider_key(self) -> None:
        source = pathlib.Path(preview.__file__).read_text(encoding="utf-8")
        self.assertNotIn("PREVIEW_LLM_API_KEY", source)
        self.assertNotIn("--llm-base-url", source)
        self.assertIn("kind: managed", source)


if __name__ == "__main__":
    unittest.main()

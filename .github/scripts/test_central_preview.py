#!/usr/bin/env python3
"""Preview installations at production central (HOR-661, DES-HOR-661-09)."""
from __future__ import annotations

import json
import os
import pathlib
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


class SettingsTests(unittest.TestCase):
    def test_requires_both_variables_without_fallback(self) -> None:
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
            central_preview.settings({"CENTRAL_CA_BUNDLE": CA})
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_CA_BUNDLE"):
            central_preview.settings({"CENTRAL_URL": ENV["CENTRAL_URL"]})
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
            central_preview.settings({**ENV, "CENTRAL_URL": "http://central.iterabase.test"})
        self.assertEqual(central_preview.settings(ENV), ("https://central.iterabase.test", CA + "\n"))

    def test_configured_only_when_both_variables_are_set(self) -> None:
        self.assertFalse(central_preview.configured({}))
        self.assertFalse(central_preview.configured({"CENTRAL_URL": " ", "CENTRAL_CA_BUNDLE": ""}))
        self.assertTrue(central_preview.configured(ENV))
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_CA_BUNDLE"):
            central_preview.configured({"CENTRAL_URL": ENV["CENTRAL_URL"]})
        with self.assertRaisesRegex(central_preview.CentralError, "CENTRAL_URL"):
            central_preview.configured({"CENTRAL_CA_BUNDLE": CA})

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

    def test_cli_revoke_is_a_noop_until_central_is_configured(self) -> None:
        with mock.patch.dict(os.environ, {"CENTRAL_URL": "", "CENTRAL_CA_BUNDLE": ""}):
            self.assertEqual(central_preview.main(["revoke", "--installations", "pr-1", "--reason", "reaped"]), 0)
        self.assertFalse((self.root / "calls").exists())


class PreviewValuesTests(unittest.TestCase):
    def test_gateway_routes_managed_inference_through_central(self) -> None:
        images = {name: {"repository": name, "tag": "sha-test"} for name in preview.PREVIEW_IMAGES}
        values = preview.preview_values(images, {"app": "app.test", "inference": "inference.test"}, "admin@test.test",
                                        ("https://central.iterabase.test", CA + "\n"))
        gateway = values[values.index("inference-gateway:"):]
        expected = ("  central:\n    enabled: true\n    url: \"https://central.iterabase.test\"\n"
                    f"    tlsSecretName: \"{central_preview.SECRET_NAME}\"\n    caBundle: {json.dumps(CA + chr(10))}\n")
        self.assertIn(expected, gateway)

    def test_unconfigured_central_renders_no_managed_route(self) -> None:
        images = {name: {"repository": name, "tag": "sha-test"} for name in preview.PREVIEW_IMAGES}
        values = preview.preview_values(images, {"app": "app.test", "inference": "inference.test"}, "admin@test.test", None)
        self.assertNotIn("central:", values)
        self.assertIn("inference-gateway:", values)

    def test_preview_no_longer_takes_a_provider_key(self) -> None:
        source = pathlib.Path(preview.__file__).read_text(encoding="utf-8")
        self.assertNotIn("PREVIEW_LLM_API_KEY", source)
        self.assertNotIn("--llm-base-url", source)
        self.assertIn("kind: managed", source)


if __name__ == "__main__":
    unittest.main()

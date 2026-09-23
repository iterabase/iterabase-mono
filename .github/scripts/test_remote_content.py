#!/usr/bin/env python3

from __future__ import annotations

import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from urllib.error import URLError

import remote_content

ROOT = Path(__file__).resolve().parents[2]


class Response(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


class RemoteContentTests(unittest.TestCase):
    def test_repository_inventory_is_complete_and_exact(self) -> None:
        remote_content.validate_manifest(remote_content.load_manifest(), ROOT)

    def test_same_url_byte_substitution_fails_before_materialization(self) -> None:
        expected = hashlib.sha256(b"reviewed").hexdigest()
        with tempfile.TemporaryDirectory() as value:
            output = Path(value) / "artifact"
            calls = 0

            def substitute(_url: str, **_kwargs):
                nonlocal calls
                calls += 1
                return Response(b"substituted")

            with self.assertRaisesRegex(remote_content.ContentError, "checksum mismatch"):
                remote_content.verified_download(
                    "https://example.invalid/tool",
                    expected,
                    output,
                    attempts=4,
                    opener=substitute,
                    sleeper=lambda _delay: None,
                )
            self.assertEqual(1, calls, "content mismatch must not be retried")
            self.assertFalse(output.exists(), "unverified bytes must not materialize")

    def test_retry_recovers_transport_only_and_still_verifies_bytes(self) -> None:
        content = b"reviewed"
        expected = hashlib.sha256(content).hexdigest()
        responses = [URLError("reset"), Response(content)]

        def transport_then_content(_url: str, **_kwargs):
            response = responses.pop(0)
            if isinstance(response, Exception):
                raise response
            return response

        with tempfile.TemporaryDirectory() as value:
            output = Path(value) / "artifact"
            remote_content.verified_download(
                "https://example.invalid/tool",
                expected,
                output,
                attempts=2,
                opener=transport_then_content,
                sleeper=lambda _delay: None,
            )
            self.assertEqual(content, output.read_bytes())
            self.assertEqual([], responses)

    def test_manifest_rejects_duplicate_content_identity(self) -> None:
        manifest = remote_content.load_manifest()
        manifest["helm_charts"].append(dict(manifest["helm_charts"][0]))
        with self.assertRaisesRegex(remote_content.ContentError, "duplicates"):
            remote_content.validate_manifest(manifest, ROOT)

    def test_manifest_rejects_version_only_container_input(self) -> None:
        manifest = remote_content.load_manifest()
        manifest["container_images"][0]["digest"] = "latest"
        with self.assertRaisesRegex(remote_content.ContentError, "non-canonical"):
            remote_content.validate_manifest(manifest, ROOT)

    def test_runtime_discovery_rejects_unreviewed_and_unused_identities(self) -> None:
        reviewed = "a" * 64
        unreviewed = "b" * 64
        manifest = {
            "schema_version": 1,
            "runtime_images": [
                {"reference": "example/runtime:v1", "digest": f"sha256:{reviewed}"}
            ],
        }
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            runtime = root / "charts/charts/service/runtime.yaml"
            runtime.parent.mkdir(parents=True)
            runtime.write_text(f"image: example/other:v1@sha256:{unreviewed}\n")
            with self.assertRaisesRegex(remote_content.ContentError, "unreviewed"):
                remote_content.validate_runtime_authority(manifest, root)
            runtime.write_text(f"image: example/substitute:v1@sha256:{reviewed}\n")
            with self.assertRaisesRegex(remote_content.ContentError, "unreviewed runtime image identity"):
                remote_content.validate_runtime_authority(manifest, root)
            runtime.write_text("kind: ConfigMap\n")
            with self.assertRaisesRegex(remote_content.ContentError, "unused"):
                remote_content.validate_runtime_authority(manifest, root)
            runtime.write_text("image: example/runtime:v1\n")
            with self.assertRaisesRegex(remote_content.ContentError, "tag-only"):
                remote_content.validate_runtime_authority(manifest, root)

    def test_ci_tool_archives_and_buildkit_image_are_content_authoritative(self) -> None:
        manifest = remote_content.load_manifest()
        identities = {(item["name"], item["version"], item["platform"]) for item in manifest["ci_tools"]}
        for platform in ("linux-amd64", "linux-arm64"):
            for name, version in (("go", "1.26.5"), ("go", "1.25.12"), ("node", "24.19.0"), ("node", "22.23.2"), ("envtest", "1.37.0"), ("buildx", "0.36.1"), ("goreleaser", "2.12.7"), ("syft", "1.51.0")):
                self.assertIn((name, version, platform), identities)
        self.assertEqual(1, len(manifest["ci_images"]))
        self.assertIn("@sha256:", manifest["ci_images"][0]["reference"] + "@" + manifest["ci_images"][0]["digest"])
        self.assertEqual(
            {"chromium", "chromium-headless-shell", "ffmpeg"},
            {item["name"] for item in manifest["playwright_archives"]},
        )
        forge_identities = {(item["name"], item["version"], item["platform"]) for item in manifest["forge_tools"]}
        for platform in ("linux-amd64", "linux-arm64"):
            self.assertIn(("k3s", "v1.34.10+k3s1", platform), forge_identities)
            self.assertIn(("k3s-images", "v1.34.10+k3s1", platform), forge_identities)

    def test_locked_control_plane_and_protobuf_tools_are_exact(self) -> None:
        manifest = remote_content.load_manifest()
        self.assertEqual(
            {
                "buf", "controller-gen", "golangci-lint", "goimports", "kustomize",
                "protoc-gen-connect-go", "protoc-gen-es", "protoc-gen-go", "setup-envtest",
            },
            {item["name"] for item in manifest["locked_tools"]},
        )
        changed = json.loads(json.dumps(manifest))
        next(item for item in changed["locked_tools"] if item["name"] == "buf")["version"] = "v1.71.0"
        with self.assertRaisesRegex(remote_content.ContentError, "does not exactly match"):
            remote_content.validate_manifest(changed, ROOT)

    def test_forge_tool_inventory_is_bidirectional(self) -> None:
        manifest = remote_content.load_manifest()
        expected = [
            {key: item[key] for key in ("name", "version", "platform", "url", "sha256", "installed_sha256")}
            for item in manifest["forge_tools"]
        ]
        actual = remote_content.forge_tool_records(
            ROOT / "forge/internal/sshprovisioner/remote_content.go"
        )
        self.assertEqual(expected, actual)
        self.assertNotEqual(expected[:-1], actual, "an undeclared Forge source record must remain detectable")
        self.assertEqual(
            manifest["forge_helm_charts"],
            remote_content.forge_chart_records(ROOT / "forge/internal/config/config.go"),
        )
        control_makefile = (ROOT / "control-plane/Makefile").read_text(encoding="utf-8")
        self.assertIn("GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint", control_makefile)

    def test_missing_action_installed_tool_checksum_fails_inventory(self) -> None:
        manifest = remote_content.load_manifest()
        manifest["ci_tools"] = [
            item
            for item in manifest["ci_tools"]
            if not (item["name"] == "syft" and item["platform"] == "linux-arm64")
        ]
        with self.assertRaisesRegex(remote_content.ContentError, "lacks linux-arm64 checksum authority"):
            remote_content.validate_manifest(manifest, ROOT)

    def test_remote_action_requires_full_commit_pin(self) -> None:
        with tempfile.TemporaryDirectory() as value:
            root = Path(value)
            action = root / "action.yml"
            action.write_text("runs:\n  using: composite\n  steps:\n    - uses: actions/cache@v4\n", encoding="utf-8")
            with self.assertRaisesRegex(remote_content.ContentError, "non-commit Action pin"):
                remote_content.validate_action_pins([action], root)


if __name__ == "__main__":
    unittest.main()

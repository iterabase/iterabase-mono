#!/usr/bin/env python3
"""Deploy one preview environment with the commit's own Forge (C6, C9, C12).

A preview is a real customer-shaped install on its own spot host: the PR's
Forge binary applies the commit's complete artifact set, exactly like a
customer `forge apply`, and later pushes upgrade it in place. Images are the
build-once digests imported into the host's containerd; charts are the source
charts re-versioned `<version>-pr.<N>.<run>` / `<version>-main.<run>` and
published to the preview namespace; the CI-owned preview overlay is the public
overlay plus a host-local smudge filter carrying the preview values (the F3
mechanism). Inference routes to a hosted, capped OpenAI-compatible API.
"""
from __future__ import annotations

import argparse
import json
import os
import pathlib
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
from typing import Any

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import e2e_inputs  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parents[2]
CHARTS = ROOT / "charts" / "charts"
PREVIEW_CHARTS = "oci://ghcr.io/iterabase/preview/charts"
OVERLAY_REPO = "https://github.com/nunocgoncalves/iterabase-overlay.git"
OVERLAY_REF = "e2e"
RELEASE = "iterabase"
NAMESPACE = "iterabase-system"
HOST_CHARTS = "/var/lib/iterabase-preview/charts"
PREVIEW_IMAGES = ("control-plane-image", "tool-runner-image", "inference-gateway-image", "harness-image")


class PreviewError(RuntimeError):
    """The preview could not be deployed exactly."""


def run(*args: str, cwd: pathlib.Path = ROOT, stdin: str | None = None) -> str:
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True, input=stdin).stdout.strip()


class Host:
    """Pinned SSH to the preview host (HOR-521: the host key comes from its EC2 tag)."""

    def __init__(self, address: str, host_key: str, key_path: str, workdir: pathlib.Path) -> None:
        self.address, self.key_path = address, key_path
        self.known_hosts = workdir / "known_hosts"
        self.known_hosts.write_text(f"{address} {host_key}\n", encoding="utf-8")
        self.base = ["-i", key_path, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
                     "-o", "CheckHostIP=no", "-o", f"UserKnownHostsFile={self.known_hosts}"]

    def ssh(self, command: str, stdin: str | None = None) -> str:
        return run("ssh", *self.base, f"ubuntu@{self.address}", command, stdin=stdin)

    def copy(self, source: pathlib.Path, destination: str) -> None:
        run("scp", "-q", *self.base, str(source), f"ubuntu@{self.address}:{destination}")


def preview_version(base: str, name: str, run_number: str) -> str:
    """SemVer prerelease for a preview chart; never a release version (C9)."""
    if not re.fullmatch(r"\d+\.\d+\.\d+", base) or not run_number.isdigit():
        raise PreviewError(f"cannot derive a preview version from {base!r} and run {run_number!r}")
    suffix = "main" if name == "staging" else f"pr.{name.removeprefix('pr-')}"
    return f"{base}-{suffix}.{run_number}"


def package_charts(version: str, workdir: pathlib.Path) -> dict[str, pathlib.Path]:
    """Package the platform chart and its companions at the preview version and publish them."""
    # Build every chart's dependencies in the source tree first, so the staged
    # copy carries them for the platform and both substrates alike.
    run("make", "-C", "charts", "build-deps")
    staged = workdir / "charts"
    shutil.copytree(CHARTS, staged)
    for chart in ("iterabase-platform", "cert-manager-substrate", "lvm-storage-substrate"):
        manifest = staged / chart / "Chart.yaml"
        manifest.write_text(re.sub(r"^version:.*$", f"version: {version}", manifest.read_text(encoding="utf-8"),
                                   count=1, flags=re.MULTILINE), encoding="utf-8")
    archives = {}
    for chart in ("iterabase-platform", "cert-manager-substrate", "lvm-storage-substrate"):
        run("helm", "package", str(staged / chart), "--destination", str(workdir / "packages"))
        archives[chart] = workdir / "packages" / f"{chart}-{version}.tgz"
        run("helm", "push", str(archives[chart]), PREVIEW_CHARTS)
    return archives


def preview_values(images: dict[str, dict[str, str]], model: str) -> str:
    """The CI-owned preview overlay values (C9): build-once images and the hosted model.

    Block YAML, because the smudge filter appends it to the overlay's own
    values.client.yaml; scalars are JSON-quoted so no value can break the document.
    """
    def image(name: str) -> dict[str, str]:
        return {"repository": images[name]["repository"], "tag": images[name]["tag"], "pullPolicy": "IfNotPresent"}
    values = {
        "control-plane": {
            "image": image("control-plane-image"),
            "toolRunner": {"image": image("tool-runner-image")},
            "dispatch": {"enabled": True, "defaultModel": {"id": "preview-hosted", "api": "openai-completions"}},
            "postgresql": {"persistence": {"size": "5Gi"}},
        },
        "minio": {"persistence": {"size": "5Gi"}},
        "inference-gateway": {"image": image("inference-gateway-image")},
    }
    return "# CI-owned preview values (HOR-590 C6/C9); appended by a host-local smudge filter.\n" + block_yaml(values)


def block_yaml(value: dict[str, Any], indent: int = 0) -> str:
    lines = []
    for key, item in value.items():
        if isinstance(item, dict):
            lines.append(f"{' ' * indent}{key}:\n{block_yaml(item, indent + 2)}")
        else:
            lines.append(f"{' ' * indent}{key}: {json.dumps(item)}\n")
    return "".join(lines)


def smudge_script(values: str) -> str:
    """Install the host-local smudge filter that appends the preview values (the F3 mechanism)."""
    prefix = "/home/ubuntu/.iterabase-preview"
    return f"""set -euo pipefail
install -d -m 700 {prefix}
cat > {prefix}/values.yaml <<'VALUES'
{values}
VALUES
printf '#!/bin/sh\\ncat\\nprintf "\\\\n"\\ncat {prefix}/values.yaml\\n' > {prefix}/smudge
chmod 700 {prefix}/smudge
printf 'values.client.yaml filter=iterabase-preview\\n' > {prefix}/attributes
git config --global core.attributesFile {prefix}/attributes
git config --global filter.iterabase-preview.clean cat
git config --global filter.iterabase-preview.smudge {prefix}/smudge
git config --global filter.iterabase-preview.required true
"""


def forge_config(host: Host, data_device: str, version: str, workdir: pathlib.Path) -> pathlib.Path:
    config = workdir / "forge.yaml"
    config.write_text(f"""apiVersion: forge.horizonshift.io/v1alpha1
kind: Cluster
metadata:
  name: {RELEASE}
spec:
  mode: single-node
  dataStorage:
    devices:
      - {data_device}
  hosts:
    - address: {host.address}
      sshUser: ubuntu
      sshKeyPath: {host.key_path}
      sshTrustFile: {host.known_hosts}
      role: control-plane+worker
  k3s:
    version: v1.34.10+k3s1
    clusterCIDR: 10.42.0.0/16
    serviceCIDR: 10.43.0.0/16
    dualStack: true
    clusterCIDRv6: fd42::/48
    serviceCIDRv6: fd43::/112
    disable: [traefik, servicelb, local-storage]
  flux:
    enabled: true
    version: "v2.4.0"
  overlay:
    repo: {OVERLAY_REPO}
    ref: {OVERLAY_REF}
  chart:
    version: {version}
    repository: {HOST_CHARTS}/iterabase-platform
    release: {RELEASE}
    namespace: {NAMESPACE}
""", encoding="utf-8")
    return config


def import_images(host: Host, images: dict[str, dict[str, str]], source_sha: str, workdir: pathlib.Path) -> None:
    """Import the build-once images into the host's containerd, verified by config digest."""
    for name in PREVIEW_IMAGES:
        inputs = e2e_inputs.image_inputs(name, images[name], source_sha, workdir)
        prefix = e2e_inputs.IMAGE_PREFIX[name]
        archive = pathlib.Path(inputs[f"{prefix}_IMAGE_ARCHIVE"])
        host.copy(archive, f"/tmp/{archive.name}")
        host.ssh(f"sudo k3s ctr -n k8s.io images import /tmp/{shlex.quote(archive.name)} >/dev/null && rm -f /tmp/{shlex.quote(archive.name)}")
        observed = host.ssh(f"sudo k3s crictl inspecti -o json {shlex.quote(images[name]['repository'] + ':' + images[name]['tag'])}"
                            " | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"status\"][\"id\"])'")
        if observed != inputs[f"{prefix}_IMAGE_CONFIG_DIGEST"]:
            raise PreviewError(f"{name} imported as {observed}, expected {inputs[f'{prefix}_IMAGE_CONFIG_DIGEST']}")


def hosted_model(host: Host, base_url: str, model_id: str, api_key: str) -> None:
    """Route inference to the capped hosted API: an external ModelBackend and its credential (C6)."""
    manifest = f"""apiVersion: v1
kind: Secret
metadata: {{name: preview-hosted-llm, namespace: {NAMESPACE}}}
type: Opaque
stringData: {{key: {json.dumps(api_key)}}}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: ModelBackend
metadata: {{name: preview-hosted, namespace: {NAMESPACE}}}
spec:
  kind: external
  external: {{baseURL: {json.dumps(base_url)}, authRef: preview-hosted-llm}}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: Model
metadata: {{name: preview-hosted, namespace: {NAMESPACE}}}
spec: {{modelID: {json.dumps(model_id)}, displayName: Preview hosted model, backendRef: preview-hosted}}
---
apiVersion: platform.iterabase.com/v1alpha1
kind: IdentityMapping
metadata: {{name: qa-operator, namespace: {NAMESPACE}}}
spec:
  identity: {{kind: user, displayName: QA Operator}}
  bindings: [{{provider: teams, type: user, externalID: "aad:qa-operator"}}]
"""
    host.ssh("sudo k3s kubectl apply -f -", stdin=manifest)


# What the tailnet name serves: the control-plane API (Dashboard at /, API at
# /v1) on 443 and the OpenAI-compatible inference gateway on 8443. Both go to
# the Services directly, so the chart's ingress host names (which only E2E
# exercises) never have to match the preview's ts.net name.
TAILNET_SERVICES = (
    (443, "app.kubernetes.io/name=control-plane,app.kubernetes.io/component=api"),
    (8443, "app.kubernetes.io/name=inference-gateway"),
)


def service_target(host: Host, selector: str) -> str:
    """The Service's cluster address and http port, with the scheme it actually serves."""
    address = host.ssh(f"sudo k3s kubectl get svc -n {NAMESPACE} -l {shlex.quote(selector)} -o jsonpath="
                       "'{.items[0].spec.clusterIP}:{.items[0].spec.ports[?(@.name==\"http\")].port}'")
    if not re.fullmatch(r"[0-9a-f.:]+:\d+", address):
        raise PreviewError(f"no Service with an http port for {selector}: {address!r}")
    # internalTLS (when an overlay enables it) serves TLS on the same port.
    tls = host.ssh(f"curl -sk -o /dev/null -w '%{{http_code}}' -m 5 https://{address}/healthz || true")
    return f"https+insecure://{address}" if tls == "200" else f"http://{address}"


def serve_on_tailnet(host: Host) -> dict[str, str]:
    """Expose the API and the inference gateway on the tailnet over HTTPS; return their URLs."""
    host.ssh("sudo tailscale serve reset")
    for port, selector in TAILNET_SERVICES:
        target = service_target(host, selector)
        host.ssh(f"sudo tailscale serve --bg --https={port} {shlex.quote(target)} >/dev/null")
    dns = host.ssh("tailscale status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)[\"Self\"][\"DNSName\"].rstrip(\".\"))'")
    return {"url": f"https://{dns}", "inference_url": f"https://{dns}:8443/v1"}


def deploy(args: argparse.Namespace) -> dict[str, str]:
    workdir = pathlib.Path(tempfile.mkdtemp(prefix="iterabase-preview-"))
    images = json.loads(args.images)
    host = Host(args.address, args.host_key, args.ssh_key, workdir)
    base = e2e_inputs.chart_version("iterabase-platform")
    version = preview_version(base, args.name, args.run_number)
    archives = package_charts(version, workdir)
    forge = ROOT / "forge" / "bin" / "forge"
    run("make", "-C", "forge", "build")
    config = forge_config(host, args.data_device, version, workdir)
    env = {**os.environ, "FORGE_HOME": str(workdir / "forge-home"), "FORGE_OVERLAY_TOKEN": os.environ.get("GITHUB_TOKEN", "")}
    host.ssh(smudge_script(preview_values(images, args.model_id)))
    host.ssh(f"sudo install -d -o ubuntu {HOST_CHARTS}")
    for chart, archive in archives.items():
        host.copy(archive, f"/tmp/{archive.name}")
        host.ssh(f"rm -rf {HOST_CHARTS}/{chart} && tar -xzf /tmp/{archive.name} -C {HOST_CHARTS} && rm -f /tmp/{archive.name}")
    # A new host, or one whose first deploy failed before K3s was installed,
    # needs k3s and data storage before the images can be imported (the F3
    # sequence). The host's state decides, not whether this run launched it.
    if args.created == "true" or host.ssh("command -v k3s >/dev/null && echo yes || echo no").strip() != "yes":
        subprocess.run([str(forge), "apply", "--config", str(config), "--skip-chart", "--skip-gpu", "--skip-overlay",
                        "--skip-secrets", "--skip-flux"], env=env, check=True)
    import_images(host, images, args.source_sha, workdir)
    subprocess.run([str(forge), "apply", "--config", str(config)], env=env, check=True)
    hosted_model(host, args.llm_base_url, args.model_id, os.environ["PREVIEW_LLM_API_KEY"])
    return {**serve_on_tailnet(host), "chart_version": version}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--name", required=True)
    parser.add_argument("--run-number", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--images", required=True)
    parser.add_argument("--address", required=True)
    parser.add_argument("--host-key", required=True)
    parser.add_argument("--data-device", required=True)
    parser.add_argument("--created", required=True, choices=("true", "false"))
    parser.add_argument("--ssh-key", required=True)
    parser.add_argument("--llm-base-url", required=True)
    parser.add_argument("--model-id", required=True)
    args = parser.parse_args(argv)
    try:
        result = deploy(args)
    except (PreviewError, subprocess.CalledProcessError, e2e_inputs.InputsError) as error:
        print(f"preview: {error}\n{getattr(error, 'stderr', '') or ''}", file=sys.stderr)
        return 1
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
            handle.write("".join(f"{key}={value}\n" for key, value in result.items()))
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())

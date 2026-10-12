#!/usr/bin/env python3
"""Derive the sized cert-manager-csi-driver chart from its reviewed archive.

Upstream v0.15.0 (and every release through v0.16.0) exposes resources only
for the driver container; its node-driver-registrar and liveness-probe
sidecars render unconstrained. HOR-602 requires every chart-rendered container
to declare requests and a memory limit (DES-HOR-602-03). This fail-closed
transform adds `nodeDriverRegistrarResources` and `livenessProbeResources`
values, with matching strict-schema entries, after archive checksum
verification and before packaging into the certificate substrate.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile

SOURCE_SHA256 = "2ef022ee88e703167c4a4c05265469f02ad35a3b492f58716078b2e838eaf550"
CHART_NAME = "cert-manager-csi-driver"

# (values key, image values key, sidecar description)
SIDECARS = (
    ("nodeDriverRegistrarResources", "nodeDriverRegistrarImage", "node-driver-registrar"),
    ("livenessProbeResources", "livenessProbeImage", "liveness-probe"),
)


class TransformError(RuntimeError):
    pass


def read(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        raise TransformError(f"cannot read upstream chart file {path}: {exc}") from exc


def write(path: Path, text: str) -> None:
    path.write_text(text, encoding="utf-8")


def extract_reviewed_chart(archive_path: Path, destination: Path) -> Path:
    digest = hashlib.sha256(archive_path.read_bytes()).hexdigest()
    if digest != SOURCE_SHA256:
        raise TransformError(
            f"cert-manager-csi-driver chart checksum mismatch: expected {SOURCE_SHA256}, got {digest}"
        )
    try:
        archive = tarfile.open(archive_path, "r:gz")
    except tarfile.TarError as exc:
        raise TransformError(f"invalid cert-manager-csi-driver chart archive: {exc}") from exc
    with archive:
        for member in archive.getmembers():
            parts = Path(member.name).parts
            if not parts or parts[0] != CHART_NAME or any(part in {"", ".", ".."} for part in parts):
                raise TransformError(f"unsafe or unexpected chart archive member {member.name!r}")
            target = destination.joinpath(*parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            if not member.isfile():
                raise TransformError(f"unsupported chart archive member {member.name!r}")
            source = archive.extractfile(member)
            if source is None:
                raise TransformError(f"unreadable chart archive member {member.name!r}")
            target.parent.mkdir(parents=True, exist_ok=True)
            with source, target.open("wb") as output:
                shutil.copyfileobj(source, output)
            os.chmod(target, member.mode & 0o777)
    chart = destination / CHART_NAME
    if not (chart / "Chart.yaml").is_file():
        raise TransformError("reviewed archive did not contain the expected cert-manager-csi-driver chart")
    return chart


def transform(chart: Path) -> None:
    daemonset = chart / "templates/daemonset.yaml"
    text = read(daemonset)
    for key, image_key, _ in SIDECARS:
        anchor = f"          imagePullPolicy: {{{{ .Values.{image_key}.pullPolicy }}}}\n"
        if text.count(anchor) != 1:
            raise TransformError(f"upstream {image_key} pull policy anchor drifted")
        sized = (
            anchor
            + f"          {{{{- with .Values.{key} }}}}\n"
            + "          resources:\n"
            + "            {{- toYaml . | nindent 12 }}\n"
            + "          {{- end }}\n"
        )
        text = text.replace(anchor, sized)
    write(daemonset, text)

    values = chart / "values.yaml"
    values_text = read(values)
    for key, _, sidecar in SIDECARS:
        if f"\n{key}:" in values_text:
            raise TransformError(f"upstream values already define {key}")
        values_text += f"\n# Kubernetes resources for the {sidecar} sidecar (Iterabase, DES-HOR-602-03).\n{key}: {{}}\n"
    write(values, values_text)

    schema_path = chart / "values.schema.json"
    try:
        schema = json.loads(read(schema_path))
        defs = schema["$defs"]
        properties = defs["helm-values"]["properties"]
    except (KeyError, json.JSONDecodeError) as exc:
        raise TransformError(f"upstream values schema layout drifted: {exc}") from exc
    for key, _, sidecar in SIDECARS:
        ref = f"helm-values.{key}"
        if key in properties or ref in defs:
            raise TransformError(f"upstream values schema already defines {key}")
        properties[key] = {"$ref": f"#/$defs/{ref}"}
        defs[ref] = {
            "default": {},
            "description": f"Kubernetes resources for the {sidecar} sidecar.",
            "type": "object",
        }
    write(schema_path, json.dumps(schema, indent=2) + "\n")


def main() -> int:
    if len(sys.argv) != 3:
        print(f"usage: {Path(sys.argv[0]).name} ARCHIVE DESTINATION", file=sys.stderr)
        return 2
    archive = Path(sys.argv[1]).resolve()
    destination = Path(sys.argv[2]).resolve()
    destination.mkdir(parents=True, exist_ok=True)
    target = destination / CHART_NAME
    try:
        with tempfile.TemporaryDirectory(prefix=".csi-driver-sized-", dir=destination) as temporary:
            chart = extract_reviewed_chart(archive, Path(temporary))
            transform(chart)
            if target.exists():
                shutil.rmtree(target)
            shutil.move(str(chart), target)
    except (OSError, TransformError) as exc:
        print(f"sized cert-manager-csi-driver chart transform failed: {exc}", file=sys.stderr)
        return 1
    print(f"prepared sized {CHART_NAME} from reviewed archive {SOURCE_SHA256}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

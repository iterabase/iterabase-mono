#!/usr/bin/env python3
"""Tailscale API calls for preview environments (HOR-590 C6, DES-HOR-590-06).

A preview host joins the tailnet as an ephemeral tag:preview node and
advertises two Tailscale Services that route through ingress-nginx:
svc:<env>-app (Dashboard and API) and svc:<env>-inference (OpenAI-compatible
gateway). This script mints the host's one-time key, creates or updates the
two Services before deploy, deletes them and the host's tailnet node on
teardown, and prunes Services and nodes whose preview no longer exists. Credentials come from TAILSCALE_OAUTH_CLIENT_ID and
TAILSCALE_OAUTH_SECRET; no token or key is ever printed.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable

API = "https://api.tailscale.com/api/v2"
HOST_TAG = "tag:preview"
SERVICE_TAG = "tag:preview-svc"
SURFACES = ("app", "inference")
ENVIRONMENT = re.compile(r"^(pr-[1-9][0-9]{0,6}|staging)$")


class TailscaleError(RuntimeError):
    pass


def service_names(environment: str) -> list[str]:
    if not ENVIRONMENT.fullmatch(environment):
        raise TailscaleError(f"preview environment {environment!r} is not pr-<N> or staging")
    return [f"svc:{environment}-{surface}" for surface in SURFACES]


def service_body(name: str) -> dict[str, Any]:
    return {"name": name, "ports": ["tcp:443"], "tags": [SERVICE_TAG],
            "comment": "Iterabase preview (HOR-590); removed on teardown"}


def environment_of(service: str) -> str | None:
    """The preview environment a Service belongs to, or None when it is not a preview Service."""
    match = re.fullmatch(r"svc:(pr-[1-9][0-9]{0,6}|staging)-(app|inference)", service)
    return match.group(1) if match else None


Request = Callable[[str, str, dict[str, Any] | None], Any]


def api(token: str) -> Request:
    def request(method: str, path: str, body: dict[str, Any] | None = None) -> Any:
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"{API}{path}", data=data, method=method, headers={
            "Authorization": f"Bearer {token}", "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30) as response:
                raw = response.read()
        except urllib.error.HTTPError as error:
            if method in ("DELETE", "GET") and error.code == 404:
                return None
            detail = error.read().decode(errors="replace")[:300]
            raise TailscaleError(f"{method} {path}: HTTP {error.code}: {detail}") from None
        return json.loads(raw) if raw else None
    return request


def access_token() -> str:
    client_id, secret = os.environ.get("TAILSCALE_OAUTH_CLIENT_ID", ""), os.environ.get("TAILSCALE_OAUTH_SECRET", "")
    if not client_id or not secret:
        raise TailscaleError("TAILSCALE_OAUTH_CLIENT_ID and TAILSCALE_OAUTH_SECRET are required")
    data = urllib.parse.urlencode({"client_id": client_id, "client_secret": secret}).encode()
    try:
        with urllib.request.urlopen(urllib.request.Request(f"{API}/oauth/token", data=data), timeout=30) as response:
            return json.loads(response.read())["access_token"]
    except (urllib.error.HTTPError, KeyError) as error:
        raise TailscaleError(f"Tailscale OAuth token request failed: {error}") from None


def mint_host_key(request: Request) -> str:
    body = {"capabilities": {"devices": {"create": {"reusable": False, "ephemeral": True, "preauthorized": True,
                                                    "tags": [HOST_TAG]}}}, "expirySeconds": 3600}
    key = request("POST", "/tailnet/-/keys", body)["key"]
    if not key.startswith("tskey-auth-"):
        raise TailscaleError("Tailscale returned an unexpected auth key shape")
    return key


def services_up(request: Request, environment: str) -> list[str]:
    """Create each Service, or update it keeping its virtual addresses (an
    update must carry the existing IPv4 and IPv6 addrs)."""
    names = service_names(environment)
    for name in names:
        path = f"/tailnet/-/vip-services/{urllib.parse.quote(name, safe=':')}"
        body = service_body(name)
        existing = request("GET", path, None)
        if existing and existing.get("addrs"):
            body["addrs"] = existing["addrs"]
        request("PUT", path, body)
    return names


def services_down(request: Request, environment: str) -> list[str]:
    names = service_names(environment)
    for name in names:
        request("DELETE", f"/tailnet/-/vip-services/{urllib.parse.quote(name, safe=':')}", None)
    return names


def device_hostname(environment: str) -> str:
    service_names(environment)  # validates the environment
    return f"iterabase-{environment}"


def preview_devices(request: Request) -> list[dict[str, Any]]:
    """Tailnet devices that are preview hosts (tag:preview, iterabase-<env> hostname)."""
    devices = (request("GET", "/tailnet/-/devices", None) or {}).get("devices", [])
    return [device for device in devices if HOST_TAG in device.get("tags", [])
            and re.fullmatch(r"iterabase-(pr-[1-9][0-9]{0,6}|staging)", device.get("hostname", ""))]


def devices_down(request: Request, environment: str) -> list[str]:
    """Remove the preview's tailnet node now instead of waiting for ephemeral expiry."""
    hostname = device_hostname(environment)
    removed = []
    for device in preview_devices(request):
        if device["hostname"] == hostname:
            request("DELETE", f"/device/{urllib.parse.quote(str(device['id']), safe='')}", None)
            removed.append(device.get("name") or hostname)
    return removed


def devices_prune(request: Request, live: set[str]) -> list[str]:
    removed = []
    for device in preview_devices(request):
        if device["hostname"].removeprefix("iterabase-") in live:
            continue
        request("DELETE", f"/device/{urllib.parse.quote(str(device['id']), safe='')}", None)
        removed.append(device.get("name") or device["hostname"])
    return removed


def services_prune(request: Request, live: set[str]) -> list[str]:
    """Delete preview Services whose environment has no live preview host."""
    removed = []
    for service in (request("GET", "/tailnet/-/vip-services", None) or {}).get("vipServices", []):
        name = service.get("name", "")
        environment = environment_of(name)
        if environment is None or SERVICE_TAG not in service.get("tags", []) or environment in live:
            continue
        request("DELETE", f"/tailnet/-/vip-services/{urllib.parse.quote(name, safe=':')}", None)
        removed.append(name)
    return removed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    mint = sub.add_parser("mint-host-key", help="write a one-hour ephemeral tag:preview key to a 0600 file")
    mint.add_argument("--output", required=True)
    for name in ("up", "down"):
        sub.add_parser(name).add_argument("--environment", required=True)
    prune = sub.add_parser("prune", help="delete preview Services with no live preview")
    prune.add_argument("--live", default="", help="comma-separated live preview environments")
    args = parser.parse_args(argv)
    try:
        request = api(access_token())
        if args.command == "mint-host-key":
            key = mint_host_key(request)
            fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                handle.write(key)
            print(f"minted an ephemeral {HOST_TAG} key")
        elif args.command == "up":
            print(" ".join(services_up(request, args.environment)))
        elif args.command == "down":
            print(" ".join(services_down(request, args.environment) + devices_down(request, args.environment)))
        else:
            live = {item for item in args.live.split(",") if item}
            print(" ".join(services_prune(request, live) + devices_prune(request, live)) or "no orphaned preview Services or devices")
    except TailscaleError as error:
        print(f"::error::{error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())

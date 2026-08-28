#!/usr/bin/env python3
"""Validate and render the secret-free OpenCodex deployment contract."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping
from urllib.parse import urlsplit

if sys.version_info < (3, 11):
    raise SystemExit("deployment_config.py requires Python 3.11 or newer (tomllib)")

import tomllib


SCHEMA_VERSION = 1
GATEWAY_VERSION = "v0.3.0"
REMOTE_HOME = "/home/ubuntu/.codex-remote-opencodex"
CONFIG_ROOT = "/home/ubuntu/.config/opencodex-relay"
LISTEN_ADDRESS = "127.0.0.1:18180"
INTERACTIVE_LISTEN_ADDRESS = "127.0.0.1:18182"
LOCAL_OPENCODEX_URL = "http://127.0.0.1:10100/v1"

TOP_LEVEL_FIELDS = {
    "schema_version",
    "deployment",
    "api_origin",
    "ssh_access_hostname",
    "opencodex_version",
    "remote_routing_mode",
    "release_repository",
    "release_public_key_fingerprint",
    "profile",
    "cloudflare",
}
CLOUDFLARE_FIELDS = {"tunnel_id", "api_access_aud", "team_name"}
SECRET_KEY_PARTS = {
    "auth",
    "credential",
    "gateway_key",
    "oauth",
    "password",
    "private_key",
    "secret",
    "token",
}
INTERPOLATION_MARKERS = ("$" + "{", "$" + "(", chr(96), "%(", "{{", "}}")

DEPLOYMENT_RE = re.compile(r"^[a-z][a-z0-9-]{0,31}$")
HOST_LABEL_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
SEMVER_RE = re.compile(
    r"^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
    r"(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$"
)
REPOSITORY_RE = re.compile(
    r"^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,38})/"
    r"[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,99})$"
)
FINGERPRINT_RE = re.compile(r"^[0-9a-f]{64}$")
TUNNEL_ID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
AUD_RE = re.compile(r"^[A-Za-z0-9_-]{16,128}$")


class DeploymentConfigError(ValueError):
    """A stable, user-facing deployment contract failure."""


@dataclass(frozen=True)
class CloudflareConfig:
    tunnel_id: str
    api_access_aud: str
    team_name: str


@dataclass(frozen=True)
class DeploymentConfig:
    deployment: str
    api_origin: str
    ssh_access_hostname: str
    opencodex_version: str
    remote_routing_mode: str
    release_repository: str
    release_public_key_fingerprint: str
    profile: str
    cloudflare: CloudflareConfig

    @property
    def remote_mode(self) -> str:
        return "loopback" if self.remote_routing_mode == "local-relay" else "external"


def _fail(message: str) -> None:
    raise DeploymentConfigError(message)


def _require_string(document: Mapping[str, Any], key: str) -> str:
    value = document.get(key)
    if not isinstance(value, str) or not value:
        _fail(f"{key} must be a non-empty string")
    if value != value.strip():
        _fail(f"{key} must not have surrounding whitespace")
    if any(marker in value for marker in INTERPOLATION_MARKERS) or "$" in value:
        _fail(f"{key} contains interpolation or shell syntax")
    if any(ord(character) < 0x20 or ord(character) == 0x7F for character in value):
        _fail(f"{key} contains a control character")
    return value


def _check_secret_like_keys(value: Any, path: str = "") -> None:
    if not isinstance(value, Mapping):
        return
    for key, child in value.items():
        if not isinstance(key, str):
            _fail(f"{path or 'document'} contains a non-string key")
        normalized = key.lower().replace("-", "_")
        if any(part in normalized for part in SECRET_KEY_PARTS):
            _fail(f"secret-bearing field is forbidden: {path + '.' if path else ''}{key}")
        _check_secret_like_keys(child, f"{path}.{key}" if path else key)


def _check_fields(document: Mapping[str, Any], allowed: set[str], path: str) -> None:
    unknown = sorted(set(document) - allowed)
    missing = sorted(allowed - set(document))
    if unknown:
        _fail(f"unknown {path} field(s): {', '.join(unknown)}")
    if missing:
        _fail(f"missing {path} field(s): {', '.join(missing)}")


def _validate_hostname(value: str, field: str) -> str:
    if value != value.lower() or len(value) > 253 or value.endswith("."):
        _fail(f"{field} must be a canonical lowercase hostname")
    labels = value.split(".")
    if len(labels) < 2 or any(not HOST_LABEL_RE.fullmatch(label) for label in labels):
        _fail(f"{field} is not a valid DNS hostname")
    return value


def _validate_api_origin(value: str) -> str:
    parsed = urlsplit(value)
    try:
        port = parsed.port
    except ValueError:
        _fail("api_origin contains an invalid port")
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or port is not None
        or parsed.path
        or parsed.query
        or parsed.fragment
        or parsed.netloc != parsed.hostname
    ):
        _fail(
            "api_origin must be exactly https://<lowercase-hostname> "
            "with no port, path, query, or fragment"
        )
    _validate_hostname(parsed.hostname, "api_origin hostname")
    return value


def load_config(path: Path) -> DeploymentConfig:
    try:
        raw = path.read_bytes()
    except OSError as error:
        _fail(f"unable to read deployment TOML: {error}")
    try:
        document = tomllib.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, tomllib.TOMLDecodeError) as error:
        _fail(f"invalid UTF-8 TOML: {error}")
    if not isinstance(document, dict):
        _fail("deployment TOML root must be a table")

    _check_secret_like_keys(document)
    _check_fields(document, TOP_LEVEL_FIELDS, "top-level")
    if document["schema_version"] != SCHEMA_VERSION or isinstance(
        document["schema_version"], bool
    ):
        _fail(f"schema_version must be the integer {SCHEMA_VERSION}")

    cloudflare_document = document["cloudflare"]
    if not isinstance(cloudflare_document, dict):
        _fail("cloudflare must be a table")
    _check_fields(cloudflare_document, CLOUDFLARE_FIELDS, "cloudflare")

    deployment = _require_string(document, "deployment")
    if not DEPLOYMENT_RE.fullmatch(deployment):
        _fail("deployment must match [a-z][a-z0-9-]{0,31}")
    api_origin = _validate_api_origin(_require_string(document, "api_origin"))
    ssh_hostname = _validate_hostname(
        _require_string(document, "ssh_access_hostname"), "ssh_access_hostname"
    )
    if ssh_hostname == urlsplit(api_origin).hostname:
        _fail("ssh_access_hostname must be distinct from the API hostname")
    opencodex_version = _require_string(document, "opencodex_version")
    if not SEMVER_RE.fullmatch(opencodex_version):
        _fail("opencodex_version must be an exact semantic version")
    routing_mode = _require_string(document, "remote_routing_mode")
    if routing_mode not in {"relay", "local-relay"}:
        _fail("remote_routing_mode must be relay or local-relay")
    repository = _require_string(document, "release_repository")
    if not REPOSITORY_RE.fullmatch(repository):
        _fail("release_repository must be OWNER/REPOSITORY")
    fingerprint = _require_string(document, "release_public_key_fingerprint")
    if not FINGERPRINT_RE.fullmatch(fingerprint):
        _fail("release_public_key_fingerprint must be lowercase SHA-256")
    profile = _require_string(document, "profile")
    if profile not in {"native", "container"}:
        _fail("profile must be native or container")

    tunnel_id = _require_string(cloudflare_document, "tunnel_id")
    if not TUNNEL_ID_RE.fullmatch(tunnel_id):
        _fail("cloudflare.tunnel_id must be a canonical UUID")
    access_aud = _require_string(cloudflare_document, "api_access_aud")
    if not AUD_RE.fullmatch(access_aud):
        _fail("cloudflare.api_access_aud has an invalid identifier shape")
    team_name = _require_string(cloudflare_document, "team_name")
    if not HOST_LABEL_RE.fullmatch(team_name) or team_name != team_name.lower():
        _fail("cloudflare.team_name must be one lowercase DNS label")

    return DeploymentConfig(
        deployment=deployment,
        api_origin=api_origin,
        ssh_access_hostname=ssh_hostname,
        opencodex_version=opencodex_version,
        remote_routing_mode=routing_mode,
        release_repository=repository,
        release_public_key_fingerprint=fingerprint,
        profile=profile,
        cloudflare=CloudflareConfig(tunnel_id, access_aud, team_name),
    )


def _json_bytes(value: Any) -> bytes:
    return (
        json.dumps(value, indent=2, sort_keys=True, ensure_ascii=True) + "\n"
    ).encode("utf-8")


def _remote_document(config: DeploymentConfig) -> dict[str, Any]:
    return {
        "api_origin": config.api_origin,
        "mode": config.remote_mode,
        "remote_home": REMOTE_HOME,
        "routing_mode": config.remote_routing_mode,
        "schema_version": SCHEMA_VERSION,
    }


def _relay_document(config: DeploymentConfig) -> dict[str, Any]:
    local = config.remote_routing_mode == "local-relay"
    return {
        "catalog": {
            "codex_executable": f"{REMOTE_HOME}/packages/standalone/current/codex",
            "manage_app_server": False,
            "owner": "remote_manager" if local else "relay",
            "path": f"{REMOTE_HOME}/opencodex-catalog.json",
            "refresh_interval": "10m",
        },
        "connection_probe": {"enabled": False},
        "credentials": (
            {"source": "none"}
            if local
            else {"file": f"{CONFIG_ROOT}/credentials.env", "source": "file"}
        ),
        "listen_address": LISTEN_ADDRESS,
        "responses": {
            "model_modes": {},
            "scheduler": {"interactive_listen_address": INTERACTIVE_LISTEN_ADDRESS},
            "websocket_mode": "http_fallback" if local else "passthrough",
        },
        "upstream_base_url": (
            LOCAL_OPENCODEX_URL if local else f"{config.api_origin}/v1"
        ),
        "upstream_mode": "local_opencodex" if local else "external_gateway",
        "voice_enabled": False,
    }


def _cloudflared_bytes(config: DeploymentConfig) -> bytes:
    cloudflare = config.cloudflare
    text = f"""# Generated from deployment.toml. Contains identifiers, not credentials.
tunnel: {cloudflare.tunnel_id}
credentials-file: /etc/cloudflared/{cloudflare.tunnel_id}.json

ingress:
  - hostname: {urlsplit(config.api_origin).hostname}
    service: http://127.0.0.1:18080
    originRequest:
      connectTimeout: 30s
      httpHostHeader: 127.0.0.1:18080
      access:
        required: true
        teamName: {cloudflare.team_name}
        audTag:
          - {cloudflare.api_access_aud}
  - hostname: {config.ssh_access_hostname}
    service: ssh://127.0.0.1:22
  - service: http_status:404
"""
    return text.encode("utf-8")


def _install_arguments_bytes(config: DeploymentConfig) -> bytes:
    values = (
        f"--github-repo={config.release_repository}",
        f"--release-public-key-sha256={config.release_public_key_fingerprint}",
        f"--upstream={config.api_origin}/v1",
        f"--opencodex-version={config.opencodex_version}",
        f"--profile={config.profile}",
    )
    return ("\n".join(values) + "\n").encode("utf-8")


def _container_image_tag(config: DeploymentConfig) -> str:
    repository = config.release_repository.lower()
    return (
        f"ghcr.io/{repository}:{GATEWAY_VERSION}-ocx-"
        f"{config.opencodex_version}"
    )


def _container_environment_bytes(config: DeploymentConfig) -> bytes:
    values = (
        f"OPENCODEX_CONTAINER_IMAGE={_container_image_tag(config)}"
        "@REPLACE_WITH_RELEASE_MANIFEST_DIGEST",
        "OPENCODEX_UID=REPLACE_WITH_HOST_OPENCODEX_UID",
        "OPENCODEX_GID=REPLACE_WITH_HOST_OPENCODEX_GID",
    )
    return ("\n".join(values) + "\n").encode("utf-8")



def _compose_bytes(config: DeploymentConfig) -> bytes:
    text = """# Experimental OpenCodex-only profile generated from deployment.toml.
# Replace container.env placeholders from the signed release manifest and host account.
# Host networking preserves the fixed loopback API and OAuth callback contracts.
name: opencodex-container
services:
  opencodex:
    image: ${OPENCODEX_CONTAINER_IMAGE:?set an image pinned by digest}
    network_mode: host
    user: ${OPENCODEX_UID:?set the host opencodex uid}:${OPENCODEX_GID:?set the host opencodex gid}
    environment:
      HOME: /var/lib/opencodex
      NODE_ENV: production
      OCX_SERVICE: "1"
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    mem_limit: 800m
    memswap_limit: 2g
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=128m,mode=1777
    volumes:
      - /var/lib/opencodex:/var/lib/opencodex
    restart: unless-stopped
"""
    return text.encode("utf-8")


def render(config: DeploymentConfig, output_root: Path) -> tuple[Path, list[str]]:
    output_root.mkdir(parents=True, exist_ok=True)
    final = output_root / config.deployment
    temporary = Path(
        tempfile.mkdtemp(prefix=f".{config.deployment}.", dir=output_root)
    )
    files = {
        "cloudflared.yml": _cloudflared_bytes(config),
        "container.env.example": _container_environment_bytes(config),
        "compose.yaml": _compose_bytes(config),
        "install-args.txt": _install_arguments_bytes(config),
        "relay.json": _json_bytes(_relay_document(config)),
        "remote-opencodex.json": _json_bytes(_remote_document(config)),
    }
    try:
        for name, file_content in sorted(files.items()):
            destination = temporary / name
            destination.write_bytes(file_content)
            destination.chmod(0o644)
        if final.exists() or final.is_symlink():
            if final.is_symlink() or not final.is_dir():
                _fail(f"render target is not a regular directory: {final}")
            shutil.rmtree(final)
        os.replace(temporary, final)
    except Exception:
        shutil.rmtree(temporary, ignore_errors=True)
        raise
    return final, sorted(files)


def _summary(config: DeploymentConfig) -> dict[str, Any]:
    return {
        "deployment": config.deployment,
        "opencodex_version": config.opencodex_version,
        "profile": config.profile,
        "remote_routing_mode": config.remote_routing_mode,
        "schema_version": SCHEMA_VERSION,
        "valid": True,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("validate", "render"):
        subparser = subparsers.add_parser(command)
        subparser.add_argument("config", type=Path)
        if command == "render":
            subparser.add_argument(
                "--output-root", type=Path, default=Path(".generated")
            )
    arguments = parser.parse_args(argv)
    try:
        config = load_config(arguments.config)
        summary = _summary(config)
        if arguments.command == "render":
            output, files = render(config, arguments.output_root)
            summary.update({"files": files, "output": str(output)})
        print(json.dumps(summary, sort_keys=True, separators=(",", ":")))
        return 0
    except DeploymentConfigError as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

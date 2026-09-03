#!/usr/bin/env python3
"""Exercise the candidate through the real Relay/relayctl lifecycle on macOS.

This is intentionally a runner-only canary.  It builds the current checkout's
Go binaries, gives relayctl a compile-time TLS-loopback release source, uses an
isolated HOME and temporary default Keychain, and writes a promotion receipt
only after every owned resource and listener has been removed.
"""

from __future__ import annotations

import argparse
import base64
import copy
import hashlib
import http.server
import json
import os
import pathlib
import platform
import pwd
import re
import shlex
import shutil
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, NoReturn

import opencodex_runtime_apple_canary as apple
import opencodex_runtime_image_test as protocol
import opencodex_runtime_manifest as manifest_contract
import opencodex_upstream as upstream_contract


API_PORT = 18443
API_BASE = f"https://127.0.0.1:{API_PORT}"
RELAY_BASE = "http://127.0.0.1:18180"
RELAY_VERSION = "0.3.9"
MAX_OUTPUT = 4 * 1024 * 1024
MAX_RECEIPT = 64 * 1024
MAX_RELAYCTL_TRANSCRIPTS = 4 * 1024 * 1024
RUNTIME_CONTAINER = "opencodex-relay-runtime"
RUNTIME_OWNER_LABEL = "io.github.novelkr.opencodex.runtime.owner"
RUNTIME_INSTALLATION_LABEL = "io.github.novelkr.opencodex.runtime.installation"
UPGRADE_CHECKS = [
    "check",
    "stage",
    "activate",
    "stop_recreate",
    "relay_models",
    "relay_responses_sse",
    "relay_responses_websocket",
    "maintenance_rollback",
    "maintenance_update",
    "recover",
    "final_stop",
    "cleanup",
]
FIRST_RELEASE_CHECKS = [
    "check",
    "stage",
    "first_activation_recover",
    "stop_recreate",
    "relay_models",
    "relay_responses_sse",
    "relay_responses_websocket",
    "final_stop",
    "cleanup",
]
BASELINE_KEYS = {"artifact_version", "manifest_sha256", "index_digest"}
RECEIPT_KEYS = {
    "schema",
    "artifact_kind",
    "result",
    "source_revision",
    "workflow_run_id",
    "workflow_run_attempt",
    "scenario",
    "candidate_sha256",
    "index_digest",
    "arm64_digest",
    "baseline",
    "relay_sha256",
    "relayctl_sha256",
    "checks",
}


class LifecycleError(RuntimeError):
    pass


def fail(message: str) -> NoReturn:
    raise LifecycleError(message)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: pathlib.Path) -> str:
    if not path.is_file() or path.is_symlink():
        fail("canary input or binary is not a regular file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def canonical_receipt(value: dict[str, Any]) -> bytes:
    validate_receipt(value)
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_receipt(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != RECEIPT_KEYS:
        fail("lifecycle receipt has unsupported fields")
    if (
        type(value["schema"]) is not int
        or value["schema"] != 1
        or value["artifact_kind"] != "opencodex-runtime-lifecycle-canary"
        or value["result"] != "passed"
    ):
        fail("lifecycle receipt identity is invalid")
    manifest_contract.commit_string(value["source_revision"], "receipt source revision")
    manifest_contract.workflow_run_id(value["workflow_run_id"], "receipt workflow run ID")
    manifest_contract.positive_int64(value["workflow_run_attempt"], "receipt workflow run attempt")
    for key in ("candidate_sha256", "relay_sha256", "relayctl_sha256"):
        if not isinstance(value[key], str) or not manifest_contract.HEX_SHA256.fullmatch(value[key]):
            fail(f"lifecycle receipt {key} is invalid")
    for key in ("index_digest", "arm64_digest"):
        manifest_contract.digest_string(value[key], f"lifecycle receipt {key}")
    if value["index_digest"] == value["arm64_digest"]:
        fail("lifecycle receipt confuses the index and arm64 digests")
    scenario = value["scenario"]
    if scenario == "first_release":
        if value["baseline"] is not None or value["checks"] != FIRST_RELEASE_CHECKS:
            fail("first-release lifecycle receipt has an invalid baseline or check set")
    elif scenario == "upgrade":
        baseline = value["baseline"]
        if not isinstance(baseline, dict) or set(baseline) != BASELINE_KEYS:
            fail("upgrade lifecycle receipt baseline is invalid")
        manifest_contract.version_tuple(baseline["artifact_version"])
        if not isinstance(baseline["manifest_sha256"], str) or not manifest_contract.HEX_SHA256.fullmatch(baseline["manifest_sha256"]):
            fail("upgrade lifecycle receipt baseline hash is invalid")
        manifest_contract.digest_string(baseline["index_digest"], "upgrade baseline index digest")
        if value["checks"] != UPGRADE_CHECKS:
            fail("upgrade lifecycle receipt check set is incomplete or out of order")
    else:
        fail("lifecycle receipt scenario is invalid")
    if not isinstance(value["checks"], list):
        fail("lifecycle receipt checks are incomplete or out of order")
    return value


def verify_receipt(arguments: argparse.Namespace) -> None:
    candidate_bytes = manifest_contract.load_regular(arguments.candidate, "runtime candidate")
    candidate = manifest_contract.validate_candidate(
        manifest_contract.load_json_bytes(candidate_bytes, "runtime candidate")
    )
    if manifest_contract.canonical_candidate(candidate) != candidate_bytes:
        fail("runtime candidate is not canonical")
    receipt_bytes = manifest_contract.load_regular(
        arguments.receipt, "lifecycle receipt", MAX_RECEIPT
    )
    receipt = validate_receipt(
        manifest_contract.load_json_bytes(receipt_bytes, "lifecycle receipt", MAX_RECEIPT)
    )
    if canonical_receipt(receipt) != receipt_bytes:
        fail("lifecycle receipt is not canonical")
    expected = {
        "source_revision": arguments.source_revision,
        "workflow_run_id": arguments.workflow_run_id,
        "workflow_run_attempt": arguments.workflow_run_attempt,
        "candidate_sha256": sha256_bytes(candidate_bytes),
        "index_digest": arguments.index_digest,
        "arm64_digest": arguments.arm64_digest,
    }
    candidate_expected = {
        "source_revision": candidate["source_revision"],
        "workflow_run_id": candidate["workflow_run_id"],
        "workflow_run_attempt": candidate["workflow_run_attempt"],
        "candidate_sha256": sha256_bytes(candidate_bytes),
        "index_digest": candidate["image"]["index_digest"],
        "arm64_digest": candidate["image"]["platforms"][1]["digest"],
    }
    if expected != candidate_expected or any(receipt[key] != value for key, value in expected.items()):
        fail("lifecycle receipt is not bound to this candidate and workflow attempt")


def clean_environment(source: dict[str, str], home: pathlib.Path | None = None) -> dict[str, str]:
    allowed = {
        "PATH",
        "LANG",
        "LC_ALL",
        "LC_CTYPE",
        "TMPDIR",
        "DEVELOPER_DIR",
        "SDKROOT",
        "GOTOOLCHAIN",
        "GOPROXY",
        "GONOSUMDB",
        "GOPRIVATE",
        "GOMODCACHE",
    }
    result = {key: value for key, value in source.items() if key in allowed}
    result["PATH"] = source.get("PATH", "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin")
    result["LANG"] = source.get("LANG", "C.UTF-8")
    if home is not None:
        result["HOME"] = str(home)
        result["XDG_CONFIG_HOME"] = str(home / ".config")
        result["XDG_CACHE_HOME"] = str(home / ".cache")
    return result


def run(
    arguments: list[str],
    *,
    cwd: pathlib.Path | None = None,
    env: dict[str, str] | None = None,
    check: bool = True,
    timeout: int = 120,
    stdin: bytes | None = None,
) -> subprocess.CompletedProcess[bytes]:
    try:
        result = subprocess.run(
            arguments,
            cwd=cwd,
            env=env,
            input=stdin,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise LifecycleError("lifecycle command could not complete") from error
    if len(result.stdout) > MAX_OUTPUT or len(result.stderr) > MAX_OUTPUT:
        fail("lifecycle command output exceeds its bound")
    if check and result.returncode != 0:
        fail(f"lifecycle command failed: {pathlib.Path(arguments[0]).name}")
    return result


def command_json(
    relayctl: pathlib.Path,
    operation: list[str],
    config: pathlib.Path,
    codex: pathlib.Path,
    env: dict[str, str],
    *,
    check: bool = True,
    timeout: int = 900,
    captures: list[tuple[str, bytes]] | None = None,
) -> tuple[dict[str, Any] | None, subprocess.CompletedProcess[bytes]]:
    arguments = [str(relayctl), *operation, "--config", str(config), "--codex-config", str(codex), "--json"]
    result = run(arguments, env=env, check=False, timeout=timeout)
    capture_relayctl_transcript(captures, arguments, result.stdout, result.stderr)
    if result.returncode != 0:
        if check:
            fail("lifecycle command failed: opencodex-relayctl")
        return None, result
    value = manifest_contract.load_json_bytes(result.stdout, "relayctl receipt", MAX_RECEIPT)
    if not isinstance(value, dict):
        fail("relayctl receipt is not an object")
    return value, result


def capture_relayctl_transcript(
    captures: list[tuple[str, bytes]] | None,
    arguments: list[str],
    stdout: bytes,
    stderr: bytes,
) -> None:
    if len(stdout) > MAX_RECEIPT or len(stderr) > MAX_RECEIPT:
        fail("relayctl stdout or stderr exceeds its 64 KiB bound")
    if captures is None:
        return
    transcript = b"argv\0" + shlex.join(arguments).encode("utf-8") + b"\nstdout\0" + stdout + b"\nstderr\0" + stderr
    if sum(len(value) for _, value in captures) + len(transcript) > MAX_RELAYCTL_TRANSCRIPTS:
        fail("relayctl transcript collection exceeds its aggregate bound")
    operation = arguments[1] if len(arguments) > 1 else "unknown"
    captures.append((f"relayctl {operation} transcript", transcript))


def require_inspection(value: dict[str, Any], expected_state: str) -> None:
    required = {"schema_version", "ok", "state", "capability", "state_digest", "routing_generation", "recovery_required"}
    if (
        not required.issubset(value)
        or type(value.get("schema_version")) is not int
        or value.get("schema_version") != 1
        or value.get("ok") is not True
    ):
        fail("relayctl inspection schema is invalid")
    if value.get("state") != expected_state or value.get("recovery_required") is not (expected_state == "recovery_required"):
        fail(f"relayctl did not report {expected_state}")
    if not isinstance(value.get("state_digest"), str) or not manifest_contract.HEX_SHA256.fullmatch(value["state_digest"]):
        fail("relayctl state digest is invalid")
    generation = value.get("routing_generation")
    if not isinstance(generation, int) or isinstance(generation, bool) or generation < 1:
        fail("relayctl routing generation is invalid")


def mutation_arguments(prefix: str, witness: dict[str, Any], extra: list[str] | None = None) -> list[str]:
    result = [
        "container-runtime",
        prefix,
        "--expected-state-digest",
        witness["state_digest"],
        "--expected-routing-generation",
        str(witness["routing_generation"]),
    ]
    if extra:
        result.extend(extra)
    return result


def stage_arguments(check: dict[str, Any]) -> list[str]:
    candidate = check.get("candidate")
    if not isinstance(candidate, dict):
        fail("runtime check omitted its candidate")
    digest = candidate.get("manifest_sha256")
    if not isinstance(digest, str) or not manifest_contract.HEX_SHA256.fullmatch(digest):
        fail("runtime check candidate manifest hash is invalid")
    return mutation_arguments(
        "stage",
        check,
        ["--expected-manifest-sha256", digest],
    )


def activate_arguments(witness: dict[str, Any]) -> list[str]:
    return mutation_arguments("activate", witness, ["--confirm-desktop-exited"])


def stop_arguments(witness: dict[str, Any]) -> list[str]:
    return mutation_arguments("stop", witness, ["--confirm-desktop-exited"])


def recover_arguments(witness: dict[str, Any]) -> list[str]:
    return [
        "container-runtime",
        "recover",
        "--expected-state-digest",
        witness["state_digest"],
        "--confirm-desktop-exited",
    ]


class ReleaseFixture:
    def __init__(self, certificate: pathlib.Path, private_key: pathlib.Path) -> None:
        self.certificate = certificate
        self.private_key = private_key
        self.phase = "baseline"
        self.releases: dict[str, dict[str, Any]] = {}
        self.server: http.server.ThreadingHTTPServer | None = None
        self.thread: threading.Thread | None = None

    def add(self, name: str, release_id: int, manifest: bytes, signature: bytes) -> None:
        document = manifest_contract.validate_manifest(
            manifest_contract.load_json_bytes(manifest, f"{name} manifest")
        )
        version = document["artifact_version"]
        manifest_name = f"opencodex-runtime-{version}.json"
        signature_name = f"opencodex-runtime-{version}.sig"
        self.releases[name] = {
            "id": release_id,
            "version": version,
            "manifest": manifest,
            "signature": signature,
            "assets": {
                release_id * 10 + 1: (manifest_name, manifest),
                release_id * 10 + 2: (signature_name, signature),
            },
        }

    def release_json(self, entry: dict[str, Any]) -> dict[str, Any]:
        assets = []
        for asset_id, (name, data) in entry["assets"].items():
            assets.append({
                "id": asset_id,
                "name": name,
                "state": "uploaded",
                "digest": "sha256:" + sha256_bytes(data),
                "size": len(data),
            })
        tag = "opencodex-runtime-" + entry["version"]
        return {
            "id": entry["id"],
            "tag_name": tag,
            "draft": False,
            "prerelease": False,
            "immutable": True,
            "html_url": "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/" + tag,
            "assets": assets,
        }

    def start(self) -> None:
        fixture = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - stdlib callback name
                path = self.path
                prefix = "/repos/novelKR/OpenCodex-OCI-Gateway/releases"
                if path.startswith(prefix + "?per_page=100&page="):
                    if fixture.phase == "baseline":
                        visible = [fixture.releases["baseline"]]
                    else:
                        visible = [fixture.releases["candidate"]]
                        if "baseline" in fixture.releases:
                            visible.append(fixture.releases["baseline"])
                    self.respond_json([fixture.release_json(item) for item in visible])
                    return
                if path.startswith(prefix + "/assets/"):
                    try:
                        asset_id = int(path.rsplit("/", 1)[1])
                    except ValueError:
                        self.send_error(404)
                        return
                    for entry in fixture.releases.values():
                        if asset_id in entry["assets"]:
                            self.respond(entry["assets"][asset_id][1], "application/octet-stream")
                            return
                    self.send_error(404)
                    return
                if path.startswith(prefix + "/"):
                    try:
                        release_id = int(path.rsplit("/", 1)[1])
                    except ValueError:
                        self.send_error(404)
                        return
                    for entry in fixture.releases.values():
                        if entry["id"] == release_id:
                            self.respond_json(fixture.release_json(entry))
                            return
                    self.send_error(404)
                    return
                if path == "/v1/models":
                    self.respond_json({"object": "list", "data": [{"id": "fixture-external", "object": "model", "created": 1, "owned_by": "fixture"}]})
                    return
                self.send_error(404)

            def respond_json(self, value: Any) -> None:
                self.respond(json.dumps(value, separators=(",", ":")).encode("utf-8"), "application/json")

            def respond(self, body: bytes, content_type: str) -> None:
                self.send_response(200)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, _format: str, *_arguments: Any) -> None:
                return

        try:
            server = http.server.ThreadingHTTPServer(("127.0.0.1", API_PORT), Handler)
        except OSError as error:
            raise LifecycleError("lifecycle fixture port is occupied") from error
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(self.certificate, self.private_key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
        self.server = server
        self.thread = threading.Thread(target=server.serve_forever, daemon=True)
        self.thread.start()

    def close(self) -> None:
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        if self.thread is not None:
            self.thread.join(timeout=5)
            if self.thread.is_alive():
                fail("lifecycle release fixture did not stop")


class IsolatedKeychain:
    def __init__(self, path: pathlib.Path, env: dict[str, str]) -> None:
        self.path = path
        self.env = env
        self.search: list[str] = []
        self.default = ""
        self.active = False

    def enter(self) -> None:
        listed = run(["/usr/bin/security", "list-keychains", "-d", "user"], env=self.env)
        default = run(["/usr/bin/security", "default-keychain", "-d", "user"], env=self.env)
        self.search = shlex.split(listed.stdout.decode("utf-8", "strict"))
        values = shlex.split(default.stdout.decode("utf-8", "strict"))
        if (
            len(values) != 1
            or not self.search
            or values[0] not in self.search
            or not pathlib.Path(values[0]).is_absolute()
            or any(not pathlib.Path(item).is_absolute() for item in self.search)
        ):
            fail("current Keychain scope is invalid")
        self.default = values[0]
        run(["/usr/bin/security", "create-keychain", "-p", "", str(self.path)], env=self.env)
        self.active = True
        run(["/usr/bin/security", "unlock-keychain", "-p", "", str(self.path)], env=self.env)
        run(["/usr/bin/security", "set-keychain-settings", "-lut", "21600", str(self.path)], env=self.env)
        run(["/usr/bin/security", "list-keychains", "-d", "user", "-s", str(self.path)], env=self.env)
        run(["/usr/bin/security", "default-keychain", "-d", "user", "-s", str(self.path)], env=self.env)

    def close(self) -> None:
        errors: list[BaseException] = []
        if self.active:
            for arguments in (
                ["/usr/bin/security", "default-keychain", "-d", "user", "-s", self.default],
                ["/usr/bin/security", "list-keychains", "-d", "user", "-s", *self.search],
                ["/usr/bin/security", "delete-keychain", str(self.path)],
            ):
                try:
                    run(arguments, env=self.env)
                except BaseException as error:
                    errors.append(error)
        if errors or self.path.exists():
            fail("temporary Keychain cleanup failed")


def sign_manifest(
    manifest: bytes,
    private_key: pathlib.Path,
    temporary: pathlib.Path,
    name: str,
    crypto: pathlib.Path,
) -> bytes:
    source = temporary / f"{name}.json"
    signature = temporary / f"{name}.sig"
    source.write_bytes(manifest)
    source.chmod(0o600)
    run([
        str(crypto), "sign", "--private-key", str(private_key),
        "--input", str(source), "--output", str(signature),
    ])
    value = manifest_contract.load_regular(signature, "canary signature", 4096)
    try:
        decoded = base64.b64decode(value.strip(), validate=True)
    except ValueError as error:
        raise LifecycleError("canary signature is invalid") from error
    if len(decoded) != 64 or base64.b64encode(decoded) != value.strip():
        fail("canary signature is not canonical Ed25519 base64")
    return value


def verify_production_baseline(
    manifest: bytes,
    signature_text: bytes,
    public_key: pathlib.Path,
    temporary: pathlib.Path,
    crypto: pathlib.Path,
) -> dict[str, Any]:
    document = manifest_contract.validate_manifest(
        manifest_contract.load_json_bytes(manifest, "baseline manifest")
    )
    if manifest_contract.canonical_manifest(document) != manifest:
        fail("baseline manifest is not canonical")
    try:
        stripped = signature_text.strip()
        signature = base64.b64decode(stripped, validate=True)
    except ValueError as error:
        raise LifecycleError("baseline signature is invalid") from error
    if len(signature) != 64 or base64.b64encode(signature) != stripped:
        fail("baseline signature is not canonical Ed25519 base64")
    source = temporary / "baseline-production.json"
    signature_path = temporary / "baseline-production.sig"
    source.write_bytes(manifest)
    signature_path.write_bytes(signature_text)
    source.chmod(0o600)
    signature_path.chmod(0o600)
    verified = run([
        str(crypto), "verify", "--public-key", str(public_key),
        "--input", str(source), "--signature", str(signature_path),
    ], check=False)
    if verified.returncode != 0:
        fail("baseline manifest production signature is invalid")
    return document


def candidate_manifest(candidate: dict[str, Any], lock_path: pathlib.Path, trust_key_id: str) -> bytes:
    lock_bytes = manifest_contract.load_regular(lock_path, "upstream lock")
    lock = upstream_contract.validate_lock(
        upstream_contract.load_json_bytes(lock_bytes, "upstream lock")
    )
    if sha256_bytes(lock_bytes) != candidate["upstream_lock_sha256"] or candidate["artifact_version"] != f"{lock['version']}-r{lock['image_revision']}":
        fail("candidate and upstream lock differ")
    document = {
        "schema": 1,
        "artifact_kind": "opencodex-runtime-image",
        "artifact_version": candidate["artifact_version"],
        "release_sequence": candidate["release_sequence"],
        "channel": "stable",
        "source": {
            "repository": "novelKR/OpenCodex-OCI-Gateway",
            "revision": candidate["source_revision"],
            "upstream_lock_sha256": candidate["upstream_lock_sha256"],
        },
        "upstream": {
            "repository": lock["repository"],
            "release_id": lock["release"]["id"],
            "release_tag": lock["release"]["tag"],
            "version": lock["version"],
            "revision": lock["revision"],
            "npm_package": lock["npm"]["package"],
            "npm_integrity": lock["npm"]["integrity"],
        },
        "image": copy.deepcopy(candidate["image"]),
        "compatibility": {
            "minimum_relay_version": RELAY_VERSION,
            "minimum_macos": "26.0",
            "minimum_apple_container": "1.3.1",
            "management_api_revision": 1,
            "secret_delivery": "uds-v1",
            "state_format_revision": 1,
        },
        "canary": {
            "source_revision": candidate["source_revision"],
            "workflow_run_id": candidate["workflow_run_id"],
            "workflow_run_attempt": candidate["workflow_run_attempt"],
            "result": "passed",
        },
        "trust_key_id": trust_key_id,
    }
    return manifest_contract.canonical_manifest(document)


def require_newer_release_sequence(
    candidate: dict[str, Any], baseline: dict[str, Any]
) -> None:
    if candidate["release_sequence"] <= baseline["release_sequence"]:
        fail("candidate release sequence must be newer than the signed baseline")


def prepare_manifests(
    arguments: argparse.Namespace,
    temporary: pathlib.Path,
    crypto: pathlib.Path,
) -> dict[str, Any]:
    candidate_bytes = manifest_contract.load_regular(arguments.candidate, "runtime candidate")
    candidate = manifest_contract.validate_candidate(
        manifest_contract.load_json_bytes(candidate_bytes, "runtime candidate")
    )
    if manifest_contract.canonical_candidate(candidate) != candidate_bytes:
        fail("runtime candidate is not canonical")
    expected = (
        arguments.source_revision,
        arguments.workflow_run_id,
        arguments.workflow_run_attempt,
        arguments.index_digest,
        arguments.arm64_digest,
    )
    actual = (
        candidate["source_revision"],
        candidate["workflow_run_id"],
        candidate["workflow_run_attempt"],
        candidate["image"]["index_digest"],
        candidate["image"]["platforms"][1]["digest"],
    )
    if actual != expected:
        fail("runtime candidate is not bound to this canary invocation")
    private_key = temporary / "canary-ed25519.pem"
    public_key = temporary / "canary-ed25519.pub"
    public_der = temporary / "canary-ed25519.der"
    generated = run([str(crypto), "generate", "--directory", str(temporary)])
    generated_receipt = manifest_contract.load_json_bytes(
        generated.stdout, "canary crypto receipt", 4096
    )
    if (
        not isinstance(generated_receipt, dict)
        or set(generated_receipt) != {"schema", "trust_key_id"}
        or type(generated_receipt.get("schema")) is not int
        or generated_receipt.get("schema") != 1
    ):
        fail("canary crypto receipt is invalid")
    trust_key_id = sha256_file(public_der)
    if generated_receipt.get("trust_key_id") != trust_key_id:
        fail("canary crypto trust-key witness is invalid")
    candidate_canary_bytes = candidate_manifest(candidate, arguments.lock, trust_key_id)
    candidate_canary = manifest_contract.validate_manifest(
        manifest_contract.load_json_bytes(candidate_canary_bytes, "candidate canary manifest")
    )
    candidate_canary_signature = sign_manifest(
        candidate_canary_bytes, private_key, temporary, "candidate-canary", crypto
    )
    prepared: dict[str, Any] = {
        "candidate": candidate,
        "candidate_bytes": candidate_bytes,
        "candidate_manifest": candidate_canary,
        "candidate_manifest_bytes": candidate_canary_bytes,
        "candidate_signature": candidate_canary_signature,
        "public_key": public_key,
        "baseline": None,
        "baseline_production_bytes": None,
        "baseline_manifest_bytes": None,
        "baseline_signature": None,
    }
    if arguments.scenario == "upgrade":
        if arguments.baseline_manifest is None or arguments.baseline_signature is None or arguments.baseline_artifact_version is None:
            fail("upgrade lifecycle canary requires the selected baseline assets")
        baseline_production_bytes = manifest_contract.load_regular(arguments.baseline_manifest, "baseline manifest")
        baseline_production_signature = manifest_contract.load_regular(arguments.baseline_signature, "baseline signature", 4096)
        baseline = verify_production_baseline(
            baseline_production_bytes,
            baseline_production_signature,
            arguments.runtime_public_key,
            temporary,
            crypto,
        )
        if baseline["artifact_version"] != arguments.baseline_artifact_version:
            fail("baseline manifest version differs from the selected immutable release")
        if manifest_contract.version_tuple(baseline["artifact_version"]) >= manifest_contract.version_tuple(candidate["artifact_version"]):
            fail("lifecycle baseline must be older than the candidate")
        require_newer_release_sequence(candidate, baseline)
        baseline_canary = copy.deepcopy(baseline)
        baseline_canary["trust_key_id"] = trust_key_id
        baseline_canary_bytes = manifest_contract.canonical_manifest(baseline_canary)
        baseline_canary_signature = sign_manifest(
            baseline_canary_bytes, private_key, temporary, "baseline-canary", crypto
        )
        prepared.update({
            "baseline": baseline,
            "baseline_production_bytes": baseline_production_bytes,
            "baseline_manifest_bytes": baseline_canary_bytes,
            "baseline_signature": baseline_canary_signature,
        })
    elif arguments.scenario != "first_release" or any(
        value is not None
        for value in (
            arguments.baseline_manifest,
            arguments.baseline_signature,
            arguments.baseline_artifact_version,
        )
    ):
        fail("first-release lifecycle canary must not accept baseline inputs")
    return prepared


def go_build_environment(temporary: pathlib.Path) -> dict[str, str]:
    environment = clean_environment(os.environ)
    cache = temporary / "go-build-cache"
    home = temporary / "go-home"
    modules = temporary / "go-module-cache"
    cache.mkdir(mode=0o700, exist_ok=True)
    home.mkdir(mode=0o700, exist_ok=True)
    modules.mkdir(mode=0o700, exist_ok=True)
    environment["GOCACHE"] = str(cache)
    environment["GOMODCACHE"] = str(modules)
    environment["HOME"] = str(home)
    return environment


def build_crypto_helper(root: pathlib.Path, temporary: pathlib.Path) -> pathlib.Path:
    relay_root = root / "client" / "relay"
    crypto = temporary / "bin" / "opencodex-runtime-canary-crypto"
    crypto.parent.mkdir(parents=True, mode=0o700)
    run(
        [
            "go", "build", "-trimpath", "-buildvcs=false", "-o", str(crypto),
            "./cmd/opencodex-runtime-canary-crypto",
        ],
        cwd=relay_root,
        env=go_build_environment(temporary),
        timeout=600,
    )
    return crypto


def build_current_binaries(
    root: pathlib.Path,
    temporary: pathlib.Path,
    public_key: pathlib.Path,
    network_name: str,
) -> tuple[pathlib.Path, pathlib.Path]:
    relay_root = root / "client" / "relay"
    if not (relay_root / "go.mod").is_file():
        fail("Relay source tree is unavailable")
    bundle = temporary / "OpenCodexRelayCanary.app" / "Contents"
    helper = bundle / "Library" / "Helpers" / "opencodex-relayctl"
    relay = temporary / "bin" / "opencodex-relay"
    trust = bundle / "Resources" / "RuntimeTrust" / "opencodex-runtime-release-ed25519.pub"
    helper.parent.mkdir(parents=True, mode=0o700)
    relay.parent.mkdir(parents=True, mode=0o700)
    trust.parent.mkdir(parents=True, mode=0o700)
    shutil.copyfile(public_key, trust)
    trust.chmod(0o644)
    build_env = go_build_environment(temporary)
    common = ["go", "build", "-trimpath", "-buildvcs=false"]
    run(
        [
            *common,
            "-ldflags",
            (
                f"-s -w -X main.version={RELAY_VERSION} "
                f"-X main.runtimeCanaryAPIBaseURL={API_BASE} "
                "-X github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/containerruntime."
                f"runtimeCanaryNetworkName={network_name}"
            ),
            "-o",
            str(helper),
            "./cmd/opencodex-relayctl",
        ],
        cwd=relay_root,
        env=build_env,
        timeout=600,
    )
    run(
        [*common, "-ldflags", f"-s -w -X main.version={RELAY_VERSION}", "-o", str(relay), "./cmd/opencodex-relay"],
        cwd=relay_root,
        env=build_env,
        timeout=600,
    )
    return helper, relay


def write_runtime_config(path: pathlib.Path, fixture_name: str) -> None:
    document = {
        "hostname": "0.0.0.0",
        "port": 10100,
        "oauthOpenBrowser": False,
        "codexAutoStart": False,
        "defaultProvider": "runtime-contract",
        "providers": {
            "runtime-contract": {
                "adapter": "openai-chat",
                "baseUrl": f"http://{fixture_name}:18080/v1",
                "apiKey": "non-billing-fixture-key",
                "allowPrivateNetwork": True,
                "liveModels": False,
                "models": ["runtime-contract-model"],
            }
        },
    }
    path.write_text(json.dumps(document, separators=(",", ":")) + "\n", encoding="utf-8")
    path.chmod(0o600)


def write_relay_config(path: pathlib.Path, home: pathlib.Path) -> None:
    document = {
        "listen_address": "127.0.0.1:18180",
        "upstream_mode": "external_gateway",
        "upstream_base_url": API_BASE + "/v1",
        "voice_enabled": False,
        "credentials": {"source": "none", "authentication_profile": "none"},
        "responses": {
            "websocket_mode": "passthrough",
            "scheduler": {"interactive_listen_address": "127.0.0.1:18182"},
        },
        "catalog": {
            "owner": "remote_manager",
            "path": str(home / ".codex" / "external-catalog.json"),
            "refresh_interval": "10m",
            "manage_app_server": False,
            "codex_executable": "codex",
        },
    }
    path.parent.mkdir(parents=True, mode=0o700)
    path.write_text(json.dumps(document, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def wait_relay(
    relayctl: pathlib.Path,
    config: pathlib.Path,
    codex: pathlib.Path,
    env: dict[str, str],
    captures: list[tuple[str, bytes]],
) -> dict[str, Any]:
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            value, _ = command_json(
                relayctl,
                ["mode", "status"],
                config,
                codex,
                env,
                timeout=5,
                captures=captures,
            )
            if value is not None and value.get("relay_running") is True and value.get("generation", 0) >= 1:
                return value
        except (LifecycleError, OSError):
            pass
        time.sleep(0.25)
    fail("resident Relay did not become ready")


def read_keychain_token(env: dict[str, str], account: str, service: str) -> str:
    result = run([
        "/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w"
    ], env=env)
    value = result.stdout.strip().decode("ascii", "strict")
    try:
        decoded = base64.urlsafe_b64decode(value + "=")
    except ValueError as error:
        raise LifecycleError("runtime Keychain token is invalid") from error
    if len(value) != 43 or len(decoded) != 32 or base64.urlsafe_b64encode(decoded).decode("ascii").rstrip("=") != value:
        fail("runtime Keychain token is not canonical")
    return value


def model_from_relay() -> str:
    value = protocol.request_json(RELAY_BASE + "/v1/models")
    rows = value.get("data") if isinstance(value, dict) else None
    if not isinstance(rows, list):
        fail("Relay model response is invalid")
    for row in rows:
        identifier = row.get("id") if isinstance(row, dict) else None
        if isinstance(identifier, str) and identifier.endswith("runtime-contract-model"):
            return identifier
    fail("Relay did not expose the non-billing fixture model")


def runtime_state_root(home: pathlib.Path) -> pathlib.Path:
    return home / "Library" / "Application Support" / "OpenCodex Relay" / "ContainerRuntime"


def wait_for_transaction_phase(path: pathlib.Path, expected: str, process: subprocess.Popen[bytes]) -> None:
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        if process.poll() is not None:
            fail("candidate activation completed before crash injection")
        try:
            value = manifest_contract.load_json_bytes(path.read_bytes(), "runtime transaction journal", MAX_RECEIPT)
            if isinstance(value, dict) and value.get("phase") == expected:
                return
        except (OSError, manifest_contract.ContractError):
            pass
        time.sleep(0.005)
    fail("candidate activation never reached the crash-injection phase")


def verify_port_free(port: int) -> None:
    probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        probe.bind(("127.0.0.1", port))
    except OSError as error:
        raise LifecycleError(f"owned listener {port} remains after cleanup") from error
    finally:
        probe.close()


def cleanup_stale_sockets(before: set[str]) -> None:
    directory = pathlib.Path(f"/private/tmp/opencodex-relay-runtime-{os.geteuid()}")
    if not directory.exists():
        return
    if directory.is_symlink() or not directory.is_dir() or directory.stat().st_uid != os.geteuid() or directory.stat().st_mode & 0o077:
        fail("bootstrap socket directory is unsafe during cleanup")
    for entry in directory.iterdir():
        if entry.name in before:
            continue
        metadata = entry.lstat()
        if not re.fullmatch(r"b-[0-9a-f]{32}", entry.name) or not stat_is_socket(metadata.st_mode) or metadata.st_uid != os.geteuid():
            fail("unexpected bootstrap artifact prevents cleanup")
        entry.unlink()
    if {entry.name for entry in directory.iterdir()} != before:
        fail("bootstrap socket cleanup is incomplete")


def snapshot_bootstrap_sockets() -> set[str]:
    directory = pathlib.Path(f"/private/tmp/opencodex-relay-runtime-{os.geteuid()}")
    if not directory.exists():
        return set()
    metadata = directory.lstat()
    if directory.is_symlink() or not directory.is_dir() or metadata.st_uid != os.geteuid() or metadata.st_mode & 0o077:
        fail("bootstrap socket directory is unsafe before lifecycle execution")
    result = set()
    for entry in directory.iterdir():
        item = entry.lstat()
        if not re.fullmatch(r"b-[0-9a-f]{32}", entry.name) or not stat_is_socket(item.st_mode) or item.st_uid != os.geteuid():
            fail("bootstrap socket directory contains an unknown artifact")
        result.add(entry.name)
    return result


def stat_is_socket(mode: int) -> bool:
    import stat

    return stat.S_ISSOCK(mode)


def cleanup_fixed_runtime(home: pathlib.Path) -> None:
    state_path = runtime_state_root(home) / "state.json"
    if RUNTIME_CONTAINER not in apple.resource_names("container"):
        return
    result = apple.container("inspect", RUNTIME_CONTAINER, check=False)
    if result.returncode != 0:
        fail("listed fixed runtime could not be inspected during cleanup")
    inspected = apple.load_cli_json(result.stdout, "lifecycle runtime cleanup inspection")
    if not isinstance(inspected, list) or len(inspected) != 1 or not isinstance(inspected[0], dict):
        fail("runtime cleanup inspection is invalid")
    configuration = inspected[0].get("configuration")
    labels = configuration.get("labels") if isinstance(configuration, dict) else None
    if not state_path.is_file() or state_path.is_symlink():
        fail("runtime cleanup lacks its durable ownership witness")
    state = manifest_contract.load_json_bytes(state_path.read_bytes(), "runtime cleanup state", MAX_RECEIPT)
    installation = state.get("installation_id") if isinstance(state, dict) else None
    if not isinstance(labels, dict) or labels.get(RUNTIME_OWNER_LABEL) != "opencodex-relay" or labels.get(RUNTIME_INSTALLATION_LABEL) != installation:
        fail("fixed runtime is foreign and will not be removed")
    apple.container("delete", "--force", RUNTIME_CONTAINER, timeout=30)
    apple.require_resource_absent("container", RUNTIME_CONTAINER, [])


def require_container_network(
    name: str,
    network_name: str,
    markers: list[str],
) -> None:
    result = apple.container("inspect", name, markers=markers)
    inspected = apple.load_cli_json(
        result.stdout, "lifecycle canary container network inspection"
    )
    try:
        apple.validate_managed_container(
            inspected,
            name,
            expected_state="running",
            expected_network=network_name,
        )
    except apple.CanaryError as error:
        raise LifecycleError(
            "lifecycle canary container is not isolated on its exact network"
        ) from error


def stop_resident_relay(process: subprocess.Popen[bytes]) -> None:
    process.send_signal(signal.SIGTERM)
    try:
        returncode = process.wait(timeout=20)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=5)
        fail("resident Relay required forced termination")
    if returncode != 0:
        fail("resident Relay did not exit cleanly after SIGTERM")


def assert_no_runtime_secrets(
    home: pathlib.Path,
    config: pathlib.Path,
    relay_log: pathlib.Path,
    markers: list[str],
    relayctl_captures: list[tuple[str, bytes]],
    *,
    include_container: bool,
) -> None:
    captures = [
        (
            "Relay activity log",
            manifest_contract.load_regular(
                relay_log, "Relay activity log", MAX_OUTPUT
            ).decode("utf-8", "replace"),
        ),
    ]
    if include_container:
        captures.extend([
            (
                "Apple Container inspect",
                apple.container(
                    "inspect", RUNTIME_CONTAINER, markers=markers
                ).stdout,
            ),
            (
                "Apple Container logs",
                apple.container(
                    "logs", RUNTIME_CONTAINER, markers=markers
                ).stdout,
            ),
        ])
    captures.append(
        (
            "Apple Container list",
            apple.container(
                "list", "--all", "--format", "json", markers=markers
            ).stdout,
        )
    )
    for description, captured in captures:
        protocol.assert_no_secret(captured, markers, description)
    for description, captured in relayctl_captures:
        protocol.assert_no_secret(
            captured.decode("utf-8", "replace"), markers, description
        )

    paths = [
        runtime_state_root(home) / "state.json",
        pathlib.Path(str(config) + ".routing-state.json"),
        pathlib.Path(str(config) + ".routing-transaction.json"),
        pathlib.Path(str(config) + ".runtime-maintenance.json"),
        pathlib.Path(str(config) + ".runtime-routing.json"),
        *sorted(runtime_state_root(home).glob("transactions/*.json")),
    ]
    for path in paths:
        if not path.exists() and not path.is_symlink():
            continue
        captured = manifest_contract.load_regular(
            path, "runtime or routing journal", MAX_RECEIPT
        ).decode("utf-8", "strict")
        protocol.assert_no_secret(captured, markers, "runtime or routing journal")


def execute(arguments: argparse.Namespace) -> dict[str, Any]:
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        fail("lifecycle canary requires an Apple Silicon macOS runner")
    apple.capability_probe()
    root = pathlib.Path(__file__).resolve().parents[1]
    expected_runtime_public_key = (
        root / "config" / "trust" / "opencodex-runtime-release-ed25519.pub"
    )
    if arguments.runtime_public_key != expected_runtime_public_key:
        fail("runtime public key is not the canonical tracked trust root")
    head = run(["git", "rev-parse", "HEAD"], cwd=root).stdout.decode("ascii", "strict").strip()
    if head != arguments.source_revision:
        fail("lifecycle canary checkout is not the candidate source revision")
    private_tmp = pathlib.Path("/private/tmp")
    if not private_tmp.is_dir() or private_tmp.is_symlink():
        fail("/private/tmp is unavailable for the isolated lifecycle canary")
    before_sockets = snapshot_bootstrap_sockets()
    if before_sockets:
        fail("dedicated lifecycle runner has a pre-existing bootstrap socket")

    cleanup_errors: list[BaseException] = []
    completed: list[str] = []
    relay_process: subprocess.Popen[bytes] | None = None
    fixture_name = ""
    fixture_labels: dict[str, str] = {}
    network_name = ""
    network_labels: dict[str, str] = {}
    relayctl_captures: list[tuple[str, bytes]] = []
    keychain: IsolatedKeychain | None = None
    release_fixture: ReleaseFixture | None = None
    relay_log: pathlib.Path | None = None
    control_socket: pathlib.Path | None = None
    final_receipt: dict[str, Any] | None = None
    with tempfile.TemporaryDirectory(prefix="ocx-lifecycle-canary-", dir=private_tmp) as value:
        temporary = pathlib.Path(value)
        crypto = build_crypto_helper(root, temporary)
        prepared = prepare_manifests(arguments, temporary, crypto)
        candidate = prepared["candidate"]
        candidate_bytes = prepared["candidate_bytes"]
        baseline = prepared["baseline"]
        public_key = prepared["public_key"]
        certificate_key = temporary / "tls.key"
        certificate = temporary / "tls.crt"
        suffix = hashlib.sha256(
            f"{arguments.workflow_run_id}:{arguments.workflow_run_attempt}".encode()
        ).hexdigest()[:12]
        network_name = f"ocx-lifecycle-canary-{suffix}"
        relayctl, relay = build_current_binaries(
            root, temporary, public_key, network_name
        )
        release_fixture = ReleaseFixture(certificate, certificate_key)
        try:
            if arguments.scenario == "upgrade":
                release_fixture.add(
                    "baseline",
                    700000001,
                    prepared["baseline_manifest_bytes"],
                    prepared["baseline_signature"],
                )
                release_fixture.phase = "baseline"
            release_fixture.add(
                "candidate",
                700000002,
                prepared["candidate_manifest_bytes"],
                prepared["candidate_signature"],
            )
            if arguments.scenario == "first_release":
                release_fixture.phase = "candidate"
            release_fixture.start()

            home = temporary / "home"
            home.mkdir(mode=0o700)
            runtime_env = clean_environment(os.environ, home)
            runtime_env["SSL_CERT_FILE"] = str(certificate)
            config = home / ".config" / "opencodex-relay" / "relay.json"
            codex = home / ".codex" / "config.toml"
            codex.parent.mkdir(parents=True, mode=0o700)
            codex.write_text("", encoding="utf-8")
            codex.chmod(0o600)
            write_relay_config(config, home)
            adjacent_control = pathlib.Path(str(config) + ".routing-control.sock")
            if len(str(adjacent_control)) <= 96:
                control_socket = adjacent_control
            else:
                control_hash = hashlib.sha256(str(config).encode()).hexdigest()[:24]
                control_socket = pathlib.Path(runtime_env.get("TMPDIR", "/tmp")) / f"pw-ocx-routing-{control_hash}.sock"
            account = pwd.getpwuid(os.geteuid()).pw_name
            keychain = IsolatedKeychain(temporary / "lifecycle.keychain-db", runtime_env)
            keychain.enter()

            fixture_name = f"ocx-lifecycle-provider-{suffix}"
            fixture_labels = apple.ownership_labels(arguments.source_revision, arguments.workflow_run_id, arguments.workflow_run_attempt)
            network_labels = dict(fixture_labels)
            apple.require_name_available("container", fixture_name)
            apple.require_name_available("network", network_name)
            exact = candidate["image"]["repository"] + "@" + candidate["image"]["index_digest"]
            mock = root / "containers" / "opencodex" / "mock-provider.mjs"
            apple.container(
                "network",
                "create",
                "--internal",
                *apple.label_arguments(network_labels),
                network_name,
            )
            apple.require_owned_resource(
                "network", network_name, network_labels, []
            )
            apple.container(
                "run", "--detach", "--name", fixture_name, "--platform", "linux/arm64",
                "--network", network_name,
                "--read-only", "--cap-drop", "ALL", "--tmpfs", "/tmp",
                *apple.label_arguments(fixture_labels),
                "--mount", f"type=bind,source={mock},target=/fixture/mock-provider.mjs,readonly",
                "--entrypoint", "bun", exact, "/fixture/mock-provider.mjs", timeout=300,
            )
            apple.require_owned_resource(
                "container",
                fixture_name,
                fixture_labels,
                [],
                expected_state="running",
            )
            require_container_network(fixture_name, network_name, [])

            relay_log = temporary / "relay.log"
            with relay_log.open("wb") as log:
                relay_process = subprocess.Popen(
                    [str(relay), "--config", str(config)],
                    env=runtime_env,
                    stdout=log,
                    stderr=log,
                )
            wait_relay(
                relayctl, config, codex, runtime_env, relayctl_captures
            )

            selected = baseline if arguments.scenario == "upgrade" else candidate
            check, _ = command_json(
                relayctl,
                ["container-runtime", "check"],
                config,
                codex,
                runtime_env,
                captures=relayctl_captures,
            )
            if check is None or check.get("status") != "update_available" or check.get("candidate", {}).get("artifact_version") != selected["artifact_version"]:
                fail("runtime check did not select the signed scenario target")
            completed.append("check")
            staged, _ = command_json(
                relayctl,
                stage_arguments(check),
                config,
                codex,
                runtime_env,
                captures=relayctl_captures,
            )
            if staged is None:
                fail("initial stage did not return a receipt")
            require_inspection(staged, "stopped")
            completed.append("stage")

            def crash_and_recover(witness: dict[str, Any]) -> dict[str, Any]:
                transaction = runtime_state_root(home) / "transactions" / "active.json"
                activation = [
                    str(relayctl),
                    *activate_arguments(witness),
                    "--config", str(config),
                    "--codex-config", str(codex),
                    "--json",
                ]
                crashing = subprocess.Popen(
                    activation,
                    env=runtime_env,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE,
                )
                wait_for_transaction_phase(transaction, "new_started", crashing)
                os.kill(crashing.pid, signal.SIGSTOP)
                if crashing.poll() is not None:
                    fail("relayctl exited before crash injection was secured")
                os.kill(crashing.pid, signal.SIGKILL)
                crashing.wait(timeout=10)
                crash_stdout, crash_stderr = crashing.communicate(timeout=1)
                capture_relayctl_transcript(
                    relayctl_captures,
                    activation,
                    crash_stdout,
                    crash_stderr,
                )
                recovery, _ = command_json(
                    relayctl,
                    ["container-runtime", "inspect"],
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if recovery is None:
                    fail("crash inspection is missing")
                require_inspection(recovery, "recovery_required")
                recovered, _ = command_json(
                    relayctl,
                    recover_arguments(recovery),
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if recovered is None:
                    fail("runtime recovery did not return a receipt")
                require_inspection(recovered, "healthy")
                return recovered

            if arguments.scenario == "first_release":
                active = crash_and_recover(staged)
                if active.get("active", {}).get("index_digest") != candidate["image"]["index_digest"]:
                    fail("first activation recovery did not commit the exact candidate")
                completed.append("first_activation_recover")
            else:
                active, _ = command_json(
                    relayctl,
                    activate_arguments(staged),
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if active is None:
                    fail("baseline activation did not return a receipt")
                require_inspection(active, "healthy")
                completed.append("activate")

            stopped, _ = command_json(
                relayctl,
                stop_arguments(active),
                config,
                codex,
                runtime_env,
                captures=relayctl_captures,
            )
            if stopped is None:
                fail("baseline stop did not return a receipt")
            require_inspection(stopped, "stopped")
            generation_one = runtime_state_root(home) / "homes" / "generation-0001" / "config.json"
            write_runtime_config(generation_one, fixture_name)
            active, _ = command_json(
                relayctl,
                activate_arguments(stopped),
                config,
                codex,
                runtime_env,
                captures=relayctl_captures,
            )
            if active is None:
                fail("baseline recreation did not return a receipt")
            require_inspection(active, "healthy")
            completed.append("stop_recreate")

            api_token = read_keychain_token(runtime_env, account, "opencodex-relay-apple-container-api-auth-token")
            admin_token = read_keychain_token(runtime_env, account, "opencodex-relay-apple-container-admin-auth-token")
            if api_token == admin_token:
                fail("runtime Keychain tokens are not distinct")
            markers = protocol.secret_markers(api_token, admin_token)
            model = model_from_relay()
            completed.append("relay_models")
            protocol.post_sse(RELAY_BASE, model, "caller-canary-key")
            completed.append("relay_responses_sse")
            protocol.websocket_response(RELAY_BASE, model, "caller-canary-key")
            completed.append("relay_responses_websocket")

            current = active
            if arguments.scenario == "upgrade":
                release_fixture.phase = "candidate"
                candidate_check, _ = command_json(
                    relayctl,
                    ["container-runtime", "check"],
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if candidate_check is None or candidate_check.get("candidate", {}).get("artifact_version") != candidate["artifact_version"]:
                    fail("candidate check did not select the exact candidate")
                candidate_staged, _ = command_json(
                    relayctl,
                    stage_arguments(candidate_check),
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if candidate_staged is None:
                    fail("candidate stage did not return a receipt")
                require_inspection(candidate_staged, "healthy")

                fault = runtime_state_root(home) / "homes" / "generation-0001" / "lifecycle-canary-symlink"
                fault.symlink_to("config.json")
                failed, failure = command_json(
                    relayctl,
                    activate_arguments(candidate_staged),
                    config,
                    codex,
                    runtime_env,
                    check=False,
                    captures=relayctl_captures,
                )
                fault.unlink()
                if failed is not None or failure.returncode == 0:
                    fail("unsafe state generation did not force maintenance rollback")
                rolled_back, _ = command_json(
                    relayctl,
                    ["container-runtime", "inspect"],
                    config,
                    codex,
                    runtime_env,
                    captures=relayctl_captures,
                )
                if rolled_back is None:
                    fail("rollback inspection is missing")
                require_inspection(rolled_back, "healthy")
                if rolled_back.get("active", {}).get("artifact_version") != baseline["artifact_version"]:
                    fail("maintenance rollback did not preserve the baseline")
                if rolled_back["routing_generation"] != candidate_staged["routing_generation"] + 2:
                    fail("maintenance rollback did not consume its two-generation witness")
                model_from_relay()
                completed.append("maintenance_rollback")
                current = crash_and_recover(rolled_back)
                if current.get("active", {}).get("index_digest") != candidate["image"]["index_digest"]:
                    fail("runtime recovery did not commit the exact candidate")
                if current["routing_generation"] != rolled_back["routing_generation"] + 2:
                    fail("maintenance recovery did not commit its two-generation witness")
                completed.append("maintenance_update")
                completed.append("recover")
                model = model_from_relay()
                protocol.post_sse(RELAY_BASE, model, "caller-canary-key")
                protocol.websocket_response(RELAY_BASE, model, "caller-canary-key")

            # Inspect and logs exist only while the final exact runtime is
            # alive. Scan those surfaces before Stop deletes the fixed
            # container, then scan the remaining list and durable surfaces
            # again after the stopped state is committed.
            require_container_network(RUNTIME_CONTAINER, network_name, markers)
            require_container_network(fixture_name, network_name, markers)
            assert_no_runtime_secrets(
                home,
                config,
                relay_log,
                markers,
                relayctl_captures,
                include_container=True,
            )

            stopped, _ = command_json(
                relayctl,
                stop_arguments(current),
                config,
                codex,
                runtime_env,
                captures=relayctl_captures,
            )
            if stopped is None:
                fail("final runtime stop did not return a receipt")
            require_inspection(stopped, "stopped")
            completed.append("final_stop")

            assert_no_runtime_secrets(
                home,
                config,
                relay_log,
                markers,
                relayctl_captures,
                include_container=False,
            )

            # SIGTERM handlers can emit diagnostics after every earlier scan.
            # Stop the resident Relay and rescan its now-complete activity log
            # plus all durable/list surfaces before a passing receipt exists.
            stop_resident_relay(relay_process)
            relay_process = None
            assert_no_runtime_secrets(
                home,
                config,
                relay_log,
                markers,
                relayctl_captures,
                include_container=False,
            )

            final_receipt = {
                "schema": 1,
                "artifact_kind": "opencodex-runtime-lifecycle-canary",
                "result": "passed",
                "source_revision": arguments.source_revision,
                "workflow_run_id": arguments.workflow_run_id,
                "workflow_run_attempt": arguments.workflow_run_attempt,
                "candidate_sha256": sha256_bytes(candidate_bytes),
                "index_digest": candidate["image"]["index_digest"],
                "arm64_digest": candidate["image"]["platforms"][1]["digest"],
                "scenario": arguments.scenario,
                "baseline": None if baseline is None else {
                    "artifact_version": baseline["artifact_version"],
                    "manifest_sha256": sha256_bytes(prepared["baseline_production_bytes"]),
                    "index_digest": baseline["image"]["index_digest"],
                },
                "relay_sha256": sha256_file(relay),
                "relayctl_sha256": sha256_file(relayctl),
                "checks": completed,
            }
        finally:
            if relay_process is not None:
                try:
                    stop_resident_relay(relay_process)
                except BaseException as error:
                    cleanup_errors.append(error)
            if control_socket is not None and control_socket.exists():
                try:
                    metadata = control_socket.lstat()
                    if control_socket.is_symlink() or not stat_is_socket(metadata.st_mode) or metadata.st_uid != os.geteuid():
                        raise LifecycleError("resident Relay control socket became unsafe")
                    control_socket.unlink()
                    cleanup_errors.append(LifecycleError("resident Relay left its control socket behind"))
                except BaseException as error:
                    cleanup_errors.append(error)
            try:
                cleanup_fixed_runtime(home)
            except BaseException as error:
                cleanup_errors.append(error)
            if fixture_name:
                try:
                    if apple.require_owned_resource("container", fixture_name, fixture_labels, [], allow_missing=True):
                        apple.container("delete", "--force", fixture_name, timeout=30)
                    apple.require_resource_absent("container", fixture_name, [])
                except BaseException as error:
                    cleanup_errors.append(error)
            if network_name:
                try:
                    if apple.require_owned_resource(
                        "network",
                        network_name,
                        network_labels,
                        [],
                        allow_missing=True,
                    ):
                        apple.container(
                            "network", "delete", network_name, timeout=30
                        )
                    apple.require_resource_absent("network", network_name, [])
                except BaseException as error:
                    cleanup_errors.append(error)
            if release_fixture is not None:
                try:
                    release_fixture.close()
                except BaseException as error:
                    cleanup_errors.append(error)
            if keychain is not None:
                try:
                    keychain.close()
                except BaseException as error:
                    cleanup_errors.append(error)
            try:
                cleanup_stale_sockets(before_sockets)
                for port in (18180, 18182, 10210, API_PORT):
                    verify_port_free(port)
            except BaseException as error:
                cleanup_errors.append(error)
        if cleanup_errors:
            fail("lifecycle canary cleanup failed closed")
        expected_checks = FIRST_RELEASE_CHECKS[:-1] if arguments.scenario == "first_release" else UPGRADE_CHECKS[:-1]
        if final_receipt is None or completed != expected_checks:
            fail("lifecycle canary did not complete every required check")
        final_receipt["checks"] = [*completed, "cleanup"]
        validate_receipt(final_receipt)

    receipt_bytes = canonical_receipt(final_receipt)
    if arguments.receipt.exists() or arguments.receipt.is_symlink():
        fail("lifecycle receipt output must not already exist")
    descriptor = os.open(arguments.receipt, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(receipt_bytes)
        stream.flush()
        os.fsync(stream.fileno())
    return final_receipt


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    run_parser = commands.add_parser("run")
    run_parser.add_argument("--candidate", type=pathlib.Path, required=True)
    run_parser.add_argument("--scenario", choices=("first_release", "upgrade"), required=True)
    run_parser.add_argument("--baseline-artifact-version")
    run_parser.add_argument("--baseline-manifest", type=pathlib.Path)
    run_parser.add_argument("--baseline-signature", type=pathlib.Path)
    run_parser.add_argument("--runtime-public-key", type=pathlib.Path, required=True)
    run_parser.add_argument("--lock", type=pathlib.Path, required=True)
    run_parser.add_argument("--source-revision", required=True)
    run_parser.add_argument("--workflow-run-id", required=True)
    run_parser.add_argument("--workflow-run-attempt", type=int, required=True)
    run_parser.add_argument("--index-digest", required=True)
    run_parser.add_argument("--arm64-digest", required=True)
    run_parser.add_argument("--receipt", type=pathlib.Path, required=True)
    run_parser.set_defaults(handler=execute)

    verify = commands.add_parser("verify-receipt")
    verify.add_argument("--candidate", type=pathlib.Path, required=True)
    verify.add_argument("--receipt", type=pathlib.Path, required=True)
    verify.add_argument("--source-revision", required=True)
    verify.add_argument("--workflow-run-id", required=True)
    verify.add_argument("--workflow-run-attempt", type=int, required=True)
    verify.add_argument("--index-digest", required=True)
    verify.add_argument("--arm64-digest", required=True)
    verify.set_defaults(handler=verify_receipt)
    return result


def validate_cli(arguments: argparse.Namespace) -> None:
    manifest_contract.commit_string(arguments.source_revision, "source revision")
    manifest_contract.workflow_run_id(arguments.workflow_run_id, "workflow run ID")
    manifest_contract.positive_int64(arguments.workflow_run_attempt, "workflow run attempt")
    manifest_contract.digest_string(arguments.index_digest, "index digest")
    manifest_contract.digest_string(arguments.arm64_digest, "arm64 digest")
    if arguments.index_digest == arguments.arm64_digest:
        fail("index and arm64 digests must differ")
    if arguments.command == "run" and arguments.scenario == "upgrade":
        if arguments.baseline_artifact_version is None:
            fail("upgrade scenario requires a baseline artifact version")
        manifest_contract.version_tuple(arguments.baseline_artifact_version)


def main() -> int:
    try:
        arguments = parser().parse_args()
        validate_cli(arguments)
        result = arguments.handler(arguments)
        if arguments.command == "run":
            print(json.dumps({"schema": 1, "status": "passed", "index_digest": result["index_digest"]}, separators=(",", ":")))
        else:
            print(json.dumps({"schema": 1, "status": "verified", "index_digest": arguments.index_digest}, separators=(",", ":")))
        return 0
    except (
        LifecycleError,
        manifest_contract.ContractError,
        upstream_contract.ContractError,
        apple.CanaryError,
        protocol.ContractError,
        OSError,
        ValueError,
        subprocess.SubprocessError,
    ) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

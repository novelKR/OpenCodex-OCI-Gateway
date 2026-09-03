#!/usr/bin/env python3
"""Run the non-billing runtime image contract against Docker/Buildx.

Secrets enter the runtime only through a one-client Unix socket.  The test
never places either runtime token in an environment variable, command-line
argument, file, receipt, or diagnostic.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import http.client
import json
import os
import pathlib
import re
import secrets
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from typing import Any, Callable, NoReturn


MAX_FRAME = 4096
MAX_HTTP = 4 * 1024 * 1024
TOKEN_HEADER = "X-OpenCodex-API-Key"
CANCELLATION_STATUS_SCRIPT = (
    'const response=await fetch("http://127.0.0.1:18080/runtime-contract/cancellation");'
    'if(!response.ok)process.exit(1);process.stdout.write(await response.text())'
)


class ContractError(RuntimeError):
    pass


def fail(message: str) -> NoReturn:
    raise ContractError(message)


def run(*arguments: str, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        list(arguments), check=False, capture_output=True, text=True, timeout=timeout
    )
    if check and result.returncode != 0:
        # Docker output is printed only after secret non-disclosure scanning by
        # callers; command arguments themselves never contain runtime secrets.
        fail(f"command failed without exposing captured output: {arguments[0:3]}")
    return result


def docker(*arguments: str, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    return run("docker", *arguments, check=check, timeout=timeout)


def exact_token() -> str:
    value = base64.urlsafe_b64encode(secrets.token_bytes(32)).decode("ascii").rstrip("=")
    if len(value) != 43:
        fail("token generation failed")
    return value


def receive_exact(connection: socket.socket, count: int) -> bytes:
    chunks: list[bytes] = []
    received = 0
    while received < count:
        chunk = connection.recv(count - received)
        if not chunk:
            fail("bootstrap peer closed early")
        chunks.append(chunk)
        received += len(chunk)
    return b"".join(chunks)


class BootstrapServer:
    def __init__(self, path: pathlib.Path, api_token: str, admin_token: str) -> None:
        self.path = path
        self.api_token = api_token
        self.admin_token = admin_token
        self.error: BaseException | None = None
        self.ready = threading.Event()
        self.complete = threading.Event()
        self.thread = threading.Thread(target=self._serve, daemon=True)

    def start(self) -> None:
        self.thread.start()
        if not self.ready.wait(5):
            fail("bootstrap server did not become ready")

    def wait(self) -> None:
        if not self.complete.wait(15):
            fail("runtime did not consume its bootstrap secret")
        if self.error is not None:
            raise ContractError("bootstrap exchange failed") from self.error

    def _serve(self) -> None:
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            listener.bind(str(self.path))
            os.chmod(self.path, 0o600)
            listener.listen(1)
            listener.settimeout(12)
            self.ready.set()
            connection, _ = listener.accept()
            with connection:
                payload = json.dumps(
                    {
                        "schema": 1,
                        "api_auth_token": self.api_token,
                        "admin_auth_token": self.admin_token,
                    },
                    separators=(",", ":"),
                ).encode("utf-8")
                connection.sendall(struct.pack(">I", len(payload)) + payload)
                length = struct.unpack(">I", receive_exact(connection, 4))[0]
                if length <= 0 or length > MAX_FRAME:
                    fail("bootstrap acknowledgement length is invalid")
                acknowledgement = json.loads(receive_exact(connection, length))
                if acknowledgement != {"schema": 1, "accepted": True}:
                    fail("bootstrap acknowledgement is invalid")
                # A second client is forbidden by closing the only listener as
                # soon as the one acknowledgement succeeds.
        except BaseException as error:
            self.error = error
        finally:
            listener.close()
            try:
                self.path.unlink()
            except FileNotFoundError:
                pass
            self.complete.set()


def request_json(url: str, headers: dict[str, str] | None = None, expected: int = 200) -> Any:
    request = urllib.request.Request(url, headers=headers or {})
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            body = response.read(MAX_HTTP + 1)
            status = response.status
    except urllib.error.HTTPError as error:
        body = error.read(MAX_HTTP + 1)
        status = error.code
    if status != expected or len(body) > MAX_HTTP:
        fail(f"HTTP contract returned status {status}, expected {expected}")
    try:
        return json.loads(body)
    except json.JSONDecodeError as error:
        raise ContractError("HTTP contract returned invalid JSON") from error


def wait_health(base_url: str) -> dict[str, Any]:
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        try:
            value = request_json(base_url + "/healthz")
            if isinstance(value, dict):
                return value
        except (ContractError, OSError, urllib.error.URLError):
            pass
        time.sleep(0.5)
    fail("runtime health check timed out")


def post_sse(base_url: str, model: str, api_token: str) -> None:
    payload = json.dumps(
        {"model": model, "input": "runtime contract", "stream": True},
        separators=(",", ":"),
    )
    connection = http.client.HTTPConnection("127.0.0.1", int(base_url.rsplit(":", 1)[1]), timeout=15)
    connection.request(
        "POST",
        "/v1/responses",
        body=payload,
        headers={"Content-Type": "application/json", TOKEN_HEADER: api_token},
    )
    response = connection.getresponse()
    body = response.read(MAX_HTTP + 1)
    connection.close()
    if response.status != 200 or len(body) > MAX_HTTP:
        fail("Responses SSE request failed")
    text = body.decode("utf-8", errors="strict")
    if "response.completed" not in text or "runtime contract ok" not in text:
        fail("Responses SSE did not complete with the fixture output")


def cancel_sse(base_url: str, model: str, api_token: str) -> None:
    payload = json.dumps(
        {"model": model, "input": "runtime cancellation contract", "stream": True},
        separators=(",", ":"),
    )
    connection = http.client.HTTPConnection(
        "127.0.0.1", int(base_url.rsplit(":", 1)[1]), timeout=15
    )
    connection.request(
        "POST",
        "/v1/responses",
        body=payload,
        headers={"Content-Type": "application/json", TOKEN_HEADER: api_token},
    )
    response = connection.getresponse()
    if response.status != 200:
        connection.close()
        fail("Responses cancellation stream did not start")
    response.read(32)
    response.close()
    connection.close()


def cancellation_status(output: str) -> dict[str, int]:
    if not output or len(output.encode("utf-8")) > 4096:
        fail("fixture cancellation status size is invalid")
    try:
        def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
            result: dict[str, Any] = {}
            for key, item in pairs:
                if key in result:
                    fail("fixture cancellation status has a duplicate field")
                result[key] = item
            return result

        value = json.loads(output, object_pairs_hook=strict_object)
    except json.JSONDecodeError as error:
        raise ContractError("fixture cancellation status is invalid JSON") from error
    expected = {"schema", "started", "cancelled", "completed"}
    if (
        not isinstance(value, dict)
        or set(value) != expected
        or type(value.get("schema")) is not int
        or value.get("schema") != 1
    ):
        fail("fixture cancellation status schema is invalid")
    for key in ("started", "cancelled", "completed"):
        if not isinstance(value[key], int) or isinstance(value[key], bool) or value[key] < 0:
            fail("fixture cancellation status counter is invalid")
    return value


def wait_cancellation_observed(read_status: Callable[[], str]) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        value = cancellation_status(read_status())
        if value["completed"] != 0:
            fail("fixture cancellation stream completed instead of being aborted")
        if value["started"] == 1 and value["cancelled"] == 1:
            return
        if value["started"] > 1 or value["cancelled"] > 1:
            fail("fixture cancellation stream was not a single bounded request")
        time.sleep(0.25)
    fail("upstream fixture did not observe Responses cancellation")


def validate_version_output(output: str, expected_version: str) -> None:
    if output != f"opencodex {expected_version}\n":
        fail("ocx --version does not exactly match the upstream lock")


def verify_image_identity(arguments: argparse.Namespace, markers: list[str]) -> None:
    version = docker(
        "run", "--rm", "--entrypoint", "/opt/opencodex/node_modules/.bin/ocx",
        arguments.image, "--version",
    ).stdout
    assert_no_secret(version, markers, "ocx version output")
    validate_version_output(version, arguments.expected_version)

    label_output = docker(
        "image", "inspect", "--format", "{{json .Config.Labels}}", arguments.image
    ).stdout
    assert_no_secret(label_output, markers, "image labels")
    try:
        labels = json.loads(label_output)
    except json.JSONDecodeError as error:
        raise ContractError("image labels are not JSON") from error
    expected = {
        "org.opencontainers.image.version": arguments.expected_artifact_version,
        "io.github.novelkr.opencodex.upstream.version": arguments.expected_version,
        "io.github.novelkr.opencodex.upstream.revision": arguments.expected_upstream_revision,
        "io.github.novelkr.opencodex.public-core.revision": arguments.expected_source_revision,
    }
    if not isinstance(labels, dict) or any(labels.get(key) != value for key, value in expected.items()):
        fail("image OCI identity labels do not match the reviewed inputs")


def websocket_frame(payload: bytes) -> bytes:
    mask = secrets.token_bytes(4)
    length = len(payload)
    if length < 126:
        header = bytes((0x81, 0x80 | length))
    elif length <= 0xFFFF:
        header = bytes((0x81, 0xFE)) + struct.pack(">H", length)
    else:
        fail("WebSocket payload is too large")
    masked = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
    return header + mask + masked


def websocket_payload(connection: socket.socket) -> bytes:
    header = receive_exact(connection, 2)
    opcode = header[0] & 0x0F
    length = header[1] & 0x7F
    if header[1] & 0x80:
        fail("server WebSocket frame must not be masked")
    if length == 126:
        length = struct.unpack(">H", receive_exact(connection, 2))[0]
    elif length == 127:
        length = struct.unpack(">Q", receive_exact(connection, 8))[0]
    if length > MAX_HTTP:
        fail("server WebSocket frame exceeds limit")
    payload = receive_exact(connection, length)
    if opcode == 0x9:
        # Ping payload is small by protocol; reply with a masked pong.
        mask = secrets.token_bytes(4)
        masked = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        connection.sendall(bytes((0x8A, 0x80 | len(payload))) + mask + masked)
        return b""
    if opcode in {0x8, 0x2}:
        fail("WebSocket closed or emitted a binary frame before completion")
    return payload


def websocket_response(base_url: str, model: str, api_token: str) -> None:
    port = int(base_url.rsplit(":", 1)[1])
    key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
    connection = socket.create_connection(("127.0.0.1", port), timeout=10)
    connection.sendall(
        (
            "GET /v1/responses HTTP/1.1\r\n"
            f"Host: 127.0.0.1:{port}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            f"{TOKEN_HEADER}: {api_token}\r\n\r\n"
        ).encode("ascii")
    )
    response = b""
    while b"\r\n\r\n" not in response and len(response) < 64 * 1024:
        response += connection.recv(4096)
    if not response.startswith(b"HTTP/1.1 101 "):
        connection.close()
        fail("Responses WebSocket upgrade failed")
    request = json.dumps(
        {"type": "response.create", "model": model, "input": "runtime contract"},
        separators=(",", ":"),
    ).encode("utf-8")
    connection.sendall(websocket_frame(request))
    deadline = time.monotonic() + 20
    complete = False
    output = False
    while time.monotonic() < deadline and not complete:
        payload = websocket_payload(connection)
        if not payload:
            continue
        event = json.loads(payload)
        if event.get("type") == "response.output_text.delta" and event.get("delta"):
            output = True
        if event.get("type") == "response.completed":
            complete = True
    connection.close()
    if not complete or not output:
        fail("Responses WebSocket did not complete with output")


def secret_markers(api_token: str, admin_token: str) -> list[str]:
    markers: list[str] = []
    for value in (api_token, admin_token):
        markers.extend((value, value[:12], hashlib.sha256(value.encode()).hexdigest()))
    return markers


def assert_no_secret(data: str, markers: list[str], description: str) -> None:
    if any(marker in data for marker in markers):
        fail(f"{description} contains a secret marker")


def emit_runtime_diagnostics(container: str, markers: list[str]) -> None:
    state = docker(
        "inspect", "--format", "{{json .State}}", container, check=False, timeout=15
    )
    logs = docker("logs", "--tail", "200", container, check=False, timeout=15)
    diagnostic = "".join(
        (
            "runtime container state:\n",
            state.stdout,
            state.stderr,
            "runtime container logs:\n",
            logs.stdout,
            logs.stderr,
        )
    )
    assert_no_secret(diagnostic, markers, "runtime failure diagnostic")
    encoded = diagnostic.encode("utf-8", errors="replace")
    if len(encoded) > 64 * 1024:
        encoded = encoded[: 64 * 1024]
        diagnostic = encoded.decode("utf-8", errors="ignore") + "\n[diagnostic truncated]\n"
    print(diagnostic, file=sys.stderr, end="" if diagnostic.endswith("\n") else "\n")


def wait_health_with_diagnostics(
    base_url: str, container: str, markers: list[str]
) -> dict[str, Any]:
    try:
        return wait_health(base_url)
    except ContractError:
        emit_runtime_diagnostics(container, markers)
        raise


def main_contract(arguments: argparse.Namespace) -> None:
    if arguments.api_token or arguments.admin_token:
        fail("tokens may not be supplied through command-line options")
    api_token = exact_token()
    admin_token = exact_token()
    while admin_token == api_token:
        admin_token = exact_token()
    markers = secret_markers(api_token, admin_token)
    verify_image_identity(arguments, markers)
    suffix = secrets.token_hex(6)
    runtime_name = f"ocx-runtime-contract-{suffix}"
    fixture_name = f"ocx-runtime-provider-{suffix}"
    network_name = f"ocx-runtime-contract-{suffix}"
    root = pathlib.Path(__file__).resolve().parents[1]
    mock = root / "containers" / "opencodex" / "mock-provider.mjs"
    owned: list[str] = []
    network_created = False
    with tempfile.TemporaryDirectory(prefix="opencodex-runtime-contract-") as directory:
        temp = pathlib.Path(directory)
        home = temp / "home"
        home.mkdir(mode=0o700)
        config = {
            "hostname": "0.0.0.0",
            "port": 10100,
            "oauthOpenBrowser": False,
            "codexAutoStart": False,
            "websockets": True,
            "defaultProvider": "runtime-contract",
            "providers": {
                "runtime-contract": {
                    "adapter": "openai-chat",
                    "baseUrl": "http://fixture-provider:18080/v1",
                    "apiKey": "non-billing-fixture-key",
                    "allowPrivateNetwork": True,
                    "liveModels": False,
                    "models": ["runtime-contract-model"],
                }
            },
        }
        (home / "config.json").write_text(
            json.dumps(config, separators=(",", ":")) + "\n", encoding="utf-8"
        )
        os.chmod(home / "config.json", 0o600)
        try:
            if docker("container", "inspect", runtime_name, check=False).returncode == 0:
                fail("runtime contract container name is unexpectedly occupied")
            docker(
                "network", "create", "--label",
                "io.github.novelkr.opencodex.runtime-contract=1", network_name,
            )
            network_created = True
            fixture = docker(
                "run", "--detach", "--name", fixture_name,
                "--network", network_name, "--network-alias", "fixture-provider",
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--mount", f"type=bind,source={mock},target=/fixture/mock-provider.mjs,readonly",
                "--entrypoint", "bun", arguments.image, "/fixture/mock-provider.mjs",
            ).stdout.strip()
            owned.append(fixture)
            base_url = f"http://127.0.0.1:{arguments.host_port}"

            socket_path = temp / "bootstrap.sock"
            server = BootstrapServer(socket_path, api_token, admin_token)
            server.start()
            runtime = docker(
                "run", "--detach", "--name", runtime_name,
                "--network", network_name,
                "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--init", "--cpus", "2", "--memory", "1g",
                "--user", f"{os.getuid()}:{os.getgid()}",
                "--publish", f"127.0.0.1:{arguments.host_port}:10100/tcp",
                "--label", "io.github.novelkr.opencodex.runtime-contract=1",
                "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=128m,mode=1777",
                "--mount", f"type=bind,source={home},target=/var/lib/opencodex",
                "--mount", f"type=bind,source={socket_path},target=/run/opencodex/bootstrap.sock",
                arguments.image,
            ).stdout.strip()
            owned.append(runtime)
            server.wait()

            health = wait_health_with_diagnostics(base_url, runtime, markers)
            if health.get("service") != "opencodex" or health.get("status") != "ok" or health.get("port") != 10100:
                fail("health identity does not report guest service port 10100")
            request_json(base_url + "/v1/models", expected=401)
            request_json(base_url + "/v1/models", {TOKEN_HEADER: admin_token}, expected=401)
            models = request_json(base_url + "/v1/models", {TOKEN_HEADER: api_token})
            rows = models.get("data") if isinstance(models, dict) else None
            if not isinstance(rows, list):
                fail("authenticated model catalog is invalid")
            identifiers = [row.get("id") for row in rows if isinstance(row, dict) and isinstance(row.get("id"), str)]
            model = next((item for item in identifiers if item.endswith("runtime-contract-model")), "")
            if not model:
                fail("mock provider model is absent from the catalog")
            request_json(base_url + "/api/config", {TOKEN_HEADER: api_token}, expected=401)
            request_json(base_url + "/api/config", {TOKEN_HEADER: admin_token})
            post_sse(base_url, model, api_token)
            cancel_sse(base_url, model, api_token)
            wait_cancellation_observed(
                lambda: docker(
                    "exec", fixture, "bun", "-e", CANCELLATION_STATUS_SCRIPT
                ).stdout
            )
            wait_health_with_diagnostics(base_url, runtime, markers)
            websocket_response(base_url, model, api_token)

            for description, captured in (
                ("container inspect", docker("inspect", runtime).stdout),
                ("container list", docker("ps", "--all", "--format", "{{json .}}").stdout),
                ("container logs", docker("logs", runtime).stdout),
            ):
                assert_no_secret(captured, markers, description)
            inspection = json.loads(docker("inspect", runtime).stdout)[0]
            mounts = inspection.get("Mounts", [])
            destinations = {mount.get("Destination") for mount in mounts if isinstance(mount, dict)}
            if destinations != {"/var/lib/opencodex", "/run/opencodex/bootstrap.sock"}:
                fail("runtime has an unexpected host mount")
            bindings = inspection.get("HostConfig", {}).get("PortBindings", {}).get("10100/tcp", [])
            if not bindings or any(binding.get("HostIp") != "127.0.0.1" for binding in bindings):
                fail("runtime port is not bound only to numeric IPv4 loopback")
            docker("stop", "--time", "15", runtime, timeout=30)
            exit_code = docker("inspect", "--format", "{{.State.ExitCode}}", runtime).stdout.strip()
            if exit_code != "0":
                fail("runtime did not stop gracefully after SIGTERM")
            # A stopped instance cannot be restarted because its one-shot
            # socket is gone.  Recreate from the same exact image and state.
            docker("rm", runtime)
            owned.remove(runtime)

            second_api = exact_token()
            second_admin = exact_token()
            while second_admin == second_api:
                second_admin = exact_token()
            second_markers = secret_markers(second_api, second_admin)
            second_socket = temp / "bootstrap-second.sock"
            second_server = BootstrapServer(second_socket, second_api, second_admin)
            second_server.start()
            runtime = docker(
                "run", "--detach", "--name", runtime_name,
                "--network", network_name, "--read-only", "--cap-drop", "ALL",
                "--security-opt", "no-new-privileges", "--init",
                "--cpus", "2", "--memory", "1g",
                "--user", f"{os.getuid()}:{os.getgid()}",
                "--publish", f"127.0.0.1:{arguments.host_port}:10100/tcp",
                "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=128m,mode=1777",
                "--mount", f"type=bind,source={home},target=/var/lib/opencodex",
                "--mount", f"type=bind,source={second_socket},target=/run/opencodex/bootstrap.sock",
                arguments.image,
            ).stdout.strip()
            owned.append(runtime)
            second_server.wait()
            wait_health_with_diagnostics(base_url, runtime, second_markers)
            request_json(base_url + "/v1/models", {TOKEN_HEADER: second_api})
            request_json(base_url + "/v1/models", {TOKEN_HEADER: second_admin}, expected=401)
            request_json(base_url + "/api/config", {TOKEN_HEADER: second_api}, expected=401)
            request_json(base_url + "/api/config", {TOKEN_HEADER: second_admin})
            combined_markers = markers + second_markers
            for description, captured in (
                ("recreated container inspect", docker("inspect", runtime).stdout),
                ("recreated container list", docker("ps", "--all", "--format", "{{json .}}").stdout),
                ("recreated container logs", docker("logs", runtime).stdout),
            ):
                assert_no_secret(captured, combined_markers, description)
            docker("stop", "--time", "15", runtime, timeout=30)
        finally:
            for container in reversed(owned):
                docker("rm", "--force", container, check=False)
            if network_created:
                docker("network", "rm", network_name, check=False)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--image", required=True)
    result.add_argument("--host-port", type=int, default=10210)
    result.add_argument("--expected-version", required=True)
    result.add_argument("--expected-artifact-version", required=True)
    result.add_argument("--expected-upstream-revision", required=True)
    result.add_argument("--expected-source-revision", required=True)
    # Deliberate rejection surfaces so callers cannot accidentally extend this
    # tool into argv-based secret delivery.
    result.add_argument("--api-token", help=argparse.SUPPRESS)
    result.add_argument("--admin-token", help=argparse.SUPPRESS)
    return result


def main() -> int:
    try:
        arguments = parser().parse_args()
        if arguments.host_port != 10210:
            fail("runtime image contract requires fixed host port 10210")
        version_pattern = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
        if not re.fullmatch(version_pattern, arguments.expected_version):
            fail("expected version is invalid")
        artifact_match = re.fullmatch(
            rf"({version_pattern})-r([1-9][0-9]*)", arguments.expected_artifact_version
        )
        if artifact_match is None or artifact_match.group(1) != arguments.expected_version:
            fail("expected artifact version is invalid")
        for revision in (arguments.expected_upstream_revision, arguments.expected_source_revision):
            if not re.fullmatch(r"[0-9a-f]{40}", revision):
                fail("expected revision is invalid")
        main_contract(arguments)
        print('{"schema":1,"status":"passed"}')
        return 0
    except (ContractError, OSError, subprocess.SubprocessError, urllib.error.URLError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

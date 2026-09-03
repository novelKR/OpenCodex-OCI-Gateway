#!/usr/bin/env python3
"""Run the exact-digest OpenCodex runtime contract with Apple Container.

The canary is intentionally suitable only for the dedicated trusted
``opencodex-runtime-canary`` runner. It creates uniquely named resources,
refuses collisions, never changes Apple Container system state, and removes
only resources it created in the current run.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import pathlib
import platform
import plistlib
import re
import socket
import stat
import subprocess
import sys
import tempfile
import time
from typing import Any, NoReturn

import opencodex_runtime_image_test as protocol
import opencodex_runtime_manifest as manifest_contract
import opencodex_upstream as upstream_contract


ROOT = pathlib.Path(__file__).resolve().parents[1]
CONTAINER = pathlib.Path("/usr/local/bin/container")
MAX_COMMAND_OUTPUT = 4 * 1024 * 1024
RUNTIME_LABEL = "io.github.novelkr.opencodex.runtime-canary"
RUN_ID_LABEL = "io.github.novelkr.opencodex.runtime-canary.run-id"
RUN_ATTEMPT_LABEL = "io.github.novelkr.opencodex.runtime-canary.run-attempt"
APPLE_CONTAINER_IDENTIFIER = "com.apple.container.cli"
APPLE_CONTAINER_TEAM_IDENTIFIER = "UPBK2H6LZM"
APPLE_CONTAINER_RECEIPT = "com.apple.container-installer"
APPLE_CONTAINER_SIGNING_REQUIREMENT = (
    'anchor apple generic and identifier "com.apple.container.cli" '
    "and certificate 1[field.1.2.840.113635.100.6.2.6] exists "
    "and certificate leaf[field.1.2.840.113635.100.6.1.13] exists "
    'and certificate leaf[subject.OU] = "UPBK2H6LZM"'
)
SYSTEM_STATUS_KEYS = {
    "status",
    "appRoot",
    "installRoot",
    "apiServerVersion",
    "apiServerCommit",
    "apiServerBuild",
    "apiServerAppName",
}
MANAGED_CONTAINER_KEYS = {"id", "configuration", "status"}
CONTAINER_STATUS_REQUIRED_KEYS = {"state", "networks"}
CONTAINER_RUNTIME_STATES = {"unknown", "stopped", "running", "stopping"}
PUBLISHED_PORT_KEYS = {
    "hostAddress",
    "hostPort",
    "containerPort",
    "proto",
    "count",
}


class CanaryError(RuntimeError):
    pass


def fail(message: str) -> NoReturn:
    raise CanaryError(message)


def semver_tuple(value: str) -> tuple[int, int, int]:
    match = re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)
    if not match:
        fail("Apple Container version is not strict SemVer")
    return tuple(int(item) for item in match.groups())  # type: ignore[return-value]


def run(
    *arguments: str,
    check: bool = True,
    timeout: int = 120,
    markers: list[str] | None = None,
) -> subprocess.CompletedProcess[str]:
    try:
        result = subprocess.run(
            list(arguments),
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise CanaryError("canary command could not complete") from error
    if len(result.stdout.encode("utf-8")) > MAX_COMMAND_OUTPUT or len(
        result.stderr.encode("utf-8")
    ) > MAX_COMMAND_OUTPUT:
        fail("canary command output exceeds its bound")
    if markers:
        protocol.assert_no_secret(result.stdout, markers, "Apple Container stdout")
        protocol.assert_no_secret(result.stderr, markers, "Apple Container stderr")
    if check and result.returncode != 0:
        fail("canary command failed without exposing captured output")
    return result


def container(
    *arguments: str,
    check: bool = True,
    timeout: int = 120,
    markers: list[str] | None = None,
) -> subprocess.CompletedProcess[str]:
    return run(str(CONTAINER), *arguments, check=check, timeout=timeout, markers=markers)


def load_cli_json(output: str, description: str) -> Any:
    return manifest_contract.load_json_bytes(
        output.encode("utf-8"), description, MAX_COMMAND_OUTPUT
    )


def all_strings(value: Any) -> set[str]:
    result: set[str] = set()
    if isinstance(value, str):
        result.add(value)
    elif isinstance(value, dict):
        for key, item in value.items():
            if isinstance(key, str):
                result.add(key)
            result.update(all_strings(item))
    elif isinstance(value, list):
        for item in value:
            result.update(all_strings(item))
    return result


def validate_install_identity(
    codesign_details: str,
    receipt_bytes: bytes,
    receipt_files: str,
    cli_version: str,
) -> None:
    identifiers = re.findall(r"(?m)^Identifier=([^\r\n]+)$", codesign_details)
    if identifiers != [APPLE_CONTAINER_IDENTIFIER]:
        fail("Apple Container CLI code-signing identifier is not official")
    team_identifiers = re.findall(r"(?m)^TeamIdentifier=([^\r\n]+)$", codesign_details)
    if team_identifiers != [APPLE_CONTAINER_TEAM_IDENTIFIER]:
        fail("Apple Container CLI code-signing team is not official")
    try:
        receipt = plistlib.loads(receipt_bytes)
    except (plistlib.InvalidFileException, ValueError) as error:
        raise CanaryError("Apple Container installer receipt is invalid") from error
    if not isinstance(receipt, dict) or receipt.get("pkgid") != APPLE_CONTAINER_RECEIPT:
        fail("Apple Container installer receipt identity is invalid")
    receipt_version = receipt.get("pkg-version")
    if not isinstance(receipt_version, str) or semver_tuple(receipt_version) < (1, 3, 1):
        fail("Apple Container installer receipt is older than 1.3.1")
    if receipt_version != cli_version:
        fail("Apple Container CLI and installer receipt versions differ")
    installed_files = {
        line.strip().removeprefix("./")
        for line in receipt_files.splitlines()
        if line.strip()
    }
    if "bin/container" not in installed_files:
        fail("Apple Container installer receipt does not own /usr/local/bin/container")


def validate_protected_executable(
    path: pathlib.Path,
    *,
    trusted_root: pathlib.Path = pathlib.Path("/"),
    owner_uid: int = 0,
) -> None:
    path_text = str(path)
    root_text = str(trusted_root)
    if (
        not path.is_absolute()
        or not trusted_root.is_absolute()
        or os.path.normpath(path_text) != path_text
        or os.path.normpath(root_text) != root_text
        or path == trusted_root
        or owner_uid < 0
    ):
        fail("Apple Container CLI path is not canonical")
    try:
        relative = path.relative_to(trusted_root)
    except ValueError as error:
        raise CanaryError("Apple Container CLI escapes its trusted root") from error

    current = trusted_root
    candidates = [trusted_root]
    for component in relative.parts:
        if component in ("", ".", ".."):
            fail("Apple Container CLI path has an unsafe component")
        current = current / component
        candidates.append(current)
    for index, candidate in enumerate(candidates):
        try:
            metadata = candidate.lstat()
        except OSError as error:
            raise CanaryError("Apple Container CLI path metadata is unavailable") from error
        if stat.S_ISLNK(metadata.st_mode) or metadata.st_uid != owner_uid or metadata.st_mode & 0o022:
            fail("Apple Container CLI path is not protected")
        validate_no_extended_acl(candidate)
        is_last = index == len(candidates) - 1
        if is_last:
            if not stat.S_ISREG(metadata.st_mode) or not metadata.st_mode & 0o111:
                fail("Apple Container CLI is not a protected regular executable")
        elif not stat.S_ISDIR(metadata.st_mode):
            fail("Apple Container CLI parent path is not a protected directory")


def validate_no_extended_acl(path: pathlib.Path) -> None:
    if platform.system() != "Darwin":
        return
    # `ls -ld` can show `@` rather than `+` when a path has both an extended
    # attribute and an ACL. `-e` emits each ACL entry on an additional line,
    # so requiring exactly the single summary line is fail-closed for both
    # marker forms without trying to interpret ACL subjects or permissions.
    listing = run("/bin/ls", "-lde", str(path), timeout=10)
    lines = listing.stdout.splitlines()
    if len(lines) != 1:
        fail("Apple Container CLI extended ACL metadata is invalid")
    fields = lines[0].split(maxsplit=1)
    if not fields or re.fullmatch(r"[bcdlps-][rwxStTs-]{9}[@.]?", fields[0]) is None:
        fail("Apple Container CLI path has an extended ACL")


def validate_system_status(value: Any) -> None:
    if not isinstance(value, dict) or set(value) not in (
        SYSTEM_STATUS_KEYS,
        SYSTEM_STATUS_KEYS | {"logRoot"},
    ):
        fail("Apple Container system status schema is invalid")
    if any(not isinstance(value[key], str) for key in SYSTEM_STATUS_KEYS):
        fail("Apple Container system status field type is invalid")
    if "logRoot" in value and value["logRoot"] is not None and not isinstance(value["logRoot"], str):
        fail("Apple Container system log root type is invalid")
    if value["status"] != "running":
        fail("Apple Container system service is not running")
    for key in (
        "appRoot",
        "installRoot",
        "apiServerVersion",
        "apiServerCommit",
        "apiServerBuild",
        "apiServerAppName",
    ):
        if not value[key]:
            fail("Apple Container running system status is incomplete")


def validate_run_identity(
    candidate: dict[str, Any],
    source_revision: str,
    workflow_run_id: str,
    workflow_run_attempt: int,
) -> None:
    if candidate["source_revision"] != source_revision:
        fail("runtime candidate source revision differs from the canary checkout")
    if candidate["workflow_run_id"] != workflow_run_id:
        fail("runtime candidate workflow run ID differs from the canary run")
    if (
        not isinstance(workflow_run_attempt, int)
        or isinstance(workflow_run_attempt, bool)
        or workflow_run_attempt < 1
        or candidate["workflow_run_attempt"] != workflow_run_attempt
    ):
        fail("runtime candidate workflow run attempt differs from the canary run")


def capability_probe() -> None:
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        fail("Apple canary requires an Apple Silicon macOS host")
    macos = platform.mac_ver()[0]
    if not macos or semver_tuple(".".join((macos.split(".") + ["0", "0"])[:3])) < (26, 0, 0):
        fail("Apple canary requires macOS 26 or newer")
    validate_protected_executable(CONTAINER)
    run(
        "/usr/bin/codesign",
        "--verify",
        "--strict",
        f"-R={APPLE_CONTAINER_SIGNING_REQUIREMENT}",
        str(CONTAINER),
    )
    signature = run(
        "/usr/bin/codesign", "--display", "--verbose=4", str(CONTAINER)
    )
    versions = load_cli_json(
        container("system", "version", "--format", "json").stdout,
        "Apple Container system version",
    )
    if not isinstance(versions, list):
        fail("Apple Container version output is invalid")
    cli_versions = [
        row.get("version") for row in versions
        if isinstance(row, dict) and row.get("appName") == "container"
    ]
    if len(cli_versions) != 1 or not isinstance(cli_versions[0], str):
        fail("Apple Container CLI version is unavailable")
    if semver_tuple(cli_versions[0]) < (1, 3, 1):
        fail("Apple Container 1.3.1 or newer is required")
    receipt = run(
        "/usr/sbin/pkgutil", "--pkg-info-plist", APPLE_CONTAINER_RECEIPT
    )
    receipt_files = run("/usr/sbin/pkgutil", "--files", APPLE_CONTAINER_RECEIPT)
    validate_install_identity(
        signature.stdout + signature.stderr,
        receipt.stdout.encode("utf-8"),
        receipt_files.stdout,
        cli_versions[0],
    )
    services = [
        row for row in versions
        if isinstance(row, dict) and row.get("appName") == "container-apiserver"
    ]
    if len(services) != 1:
        fail("Apple Container system service is not available")
    status_result = container("system", "status", "--format", "json")
    status_value = load_cli_json(status_result.stdout, "Apple Container system status")
    validate_system_status(status_value)


def require_free_port() -> None:
    for family, address in (
        (socket.AF_INET, ("127.0.0.1", 10210)),
        (socket.AF_INET6, ("::1", 10210)),
    ):
        probe = socket.socket(family, socket.SOCK_STREAM)
        try:
            probe.bind(address)
        except OSError as error:
            raise CanaryError("fixed canary host port 10210 is occupied") from error
        finally:
            probe.close()


def resource_names(kind: str, markers: list[str] | None = None) -> set[str]:
    arguments = ("list", "--all", "--format", "json") if kind == "container" else (
        "network", "list", "--format", "json"
    )
    listed = load_cli_json(
        container(*arguments, markers=markers).stdout,
        f"Apple Container {kind} list",
    )
    if not isinstance(listed, list) or any(not isinstance(item, dict) for item in listed):
        fail(f"Apple Container {kind} list is invalid")
    identifiers = [item.get("id") for item in listed]
    if any(not isinstance(item, str) or not item for item in identifiers):
        fail(f"Apple Container {kind} list has an invalid identity")
    if len(identifiers) != len(set(identifiers)):
        fail(f"Apple Container {kind} list has duplicate identities")
    return set(identifiers)


def require_name_available(kind: str, name: str) -> None:
    if name in resource_names(kind):
        fail(f"foreign {kind} occupies the canary name")


def require_resource_absent(kind: str, name: str, markers: list[str]) -> None:
    if name in resource_names(kind, markers):
        fail(f"owned Apple Container {kind} remains after cleanup")


def ownership_labels(source_revision: str, run_id: str, run_attempt: int) -> dict[str, str]:
    return {
        RUNTIME_LABEL: source_revision,
        RUN_ID_LABEL: run_id,
        RUN_ATTEMPT_LABEL: str(run_attempt),
    }


def label_arguments(labels: dict[str, str]) -> list[str]:
    result: list[str] = []
    for key in sorted(labels):
        result.extend(("--label", f"{key}={labels[key]}"))
    return result


def validate_managed_container(
    inspected: Any,
    name: str,
    *,
    expected_state: str | None = None,
    expected_network: str | None = None,
) -> dict[str, Any]:
    if (
        not isinstance(inspected, list)
        or len(inspected) != 1
        or not isinstance(inspected[0], dict)
        or set(inspected[0]) != MANAGED_CONTAINER_KEYS
    ):
        fail("Apple Container inspect schema is invalid")
    managed = inspected[0]
    configuration = managed["configuration"]
    status = managed["status"]
    if (
        managed["id"] != name
        or not isinstance(configuration, dict)
        or configuration.get("id") != name
    ):
        fail("Apple Container inspect identity is invalid")
    if (
        not isinstance(status, dict)
        or set(status) not in (
            CONTAINER_STATUS_REQUIRED_KEYS,
            CONTAINER_STATUS_REQUIRED_KEYS | {"startedDate"},
        )
        or not isinstance(status.get("state"), str)
        or status.get("state") not in CONTAINER_RUNTIME_STATES
        or not isinstance(status.get("networks"), list)
        or any(not isinstance(network, dict) for network in status["networks"])
    ):
        fail("Apple Container inspect status schema is invalid")
    started_date = status.get("startedDate")
    if started_date is not None:
        if not isinstance(started_date, str):
            fail("Apple Container inspect start date is invalid")
        try:
            parsed_date = datetime.datetime.fromisoformat(
                started_date.replace("Z", "+00:00")
            )
        except ValueError as error:
            raise CanaryError("Apple Container inspect start date is invalid") from error
        if parsed_date.tzinfo is None:
            fail("Apple Container inspect start date is invalid")
    if expected_state is not None and status["state"] != expected_state:
        fail(f"Apple Container is not in the expected {expected_state} state")
    if expected_network is not None and (
        len(status["networks"]) != 1
        or status["networks"][0].get("network") != expected_network
    ):
        fail("Apple Container is not attached only to the exact canary network")
    return configuration


def require_owned_resource(
    kind: str,
    name: str,
    expected_labels: dict[str, str],
    markers: list[str],
    *,
    allow_missing: bool = False,
    expected_state: str | None = None,
    expected_network: str | None = None,
) -> bool:
    arguments = ("inspect", name) if kind == "container" else ("network", "inspect", name)
    result = container(*arguments, check=False, markers=markers)
    if result.returncode != 0:
        if allow_missing:
            return False
        fail(f"owned Apple Container {kind} disappeared before mutation")
    inspected = load_cli_json(
        result.stdout, f"owned Apple Container {kind} inspection"
    )
    if kind == "container":
        configuration = validate_managed_container(
            inspected,
            name,
            expected_state=expected_state,
            expected_network=expected_network,
        )
    elif (
        not isinstance(inspected, list)
        or len(inspected) != 1
        or not isinstance(inspected[0], dict)
        or inspected[0].get("id") != name
        or not isinstance(inspected[0].get("configuration"), dict)
    ):
        fail(f"owned Apple Container {kind} identity changed before mutation")
    else:
        configuration = inspected[0]["configuration"]
    labels = configuration.get("labels")
    if not isinstance(labels, dict) or any(labels.get(key) != value for key, value in expected_labels.items()):
        fail(f"owned Apple Container {kind} labels changed before mutation")
    return True


def cleanup_owned_resources(
    container_names: list[str],
    network_name: str | None,
    expected_labels: dict[str, str],
    markers: list[str],
) -> None:
    cleanup_failed = False

    for name in reversed(container_names):
        try:
            if require_owned_resource(
                "container",
                name,
                expected_labels,
                markers,
                allow_missing=True,
            ):
                deleted = container(
                    "delete",
                    "--force",
                    name,
                    check=False,
                    timeout=30,
                    markers=markers,
                )
                if deleted.returncode != 0:
                    raise CanaryError("owned Apple Container container cleanup failed")
            require_resource_absent("container", name, markers)
        except (
            CanaryError,
            manifest_contract.ContractError,
            protocol.ContractError,
            OSError,
            ValueError,
            json.JSONDecodeError,
        ):
            cleanup_failed = True

    if network_name is not None:
        try:
            if require_owned_resource(
                "network",
                network_name,
                expected_labels,
                markers,
                allow_missing=True,
            ):
                deleted = container(
                    "network",
                    "delete",
                    network_name,
                    check=False,
                    timeout=30,
                    markers=markers,
                )
                if deleted.returncode != 0:
                    raise CanaryError("owned Apple Container network cleanup failed")
            require_resource_absent("network", network_name, markers)
        except (
            CanaryError,
            manifest_contract.ContractError,
            protocol.ContractError,
            OSError,
            ValueError,
            json.JSONDecodeError,
        ):
            cleanup_failed = True

    if cleanup_failed:
        fail("Apple Container canary cleanup did not remove every owned resource")


def runtime_arguments(
    name: str,
    network: str,
    image: str,
    home: pathlib.Path,
    bootstrap_socket: pathlib.Path,
    labels: dict[str, str],
) -> list[str]:
    return [
        "run", "--detach", "--name", name,
        "--network", network,
        "--platform", "linux/arm64",
        "--read-only", "--cap-drop", "ALL", "--init",
        "--cpus", "2", "--memory", "1G",
        "--uid", str(os.getuid()), "--gid", str(os.getgid()),
        "--publish", "127.0.0.1:10210:10100/tcp",
        "--tmpfs", "/tmp",
        "--mount", f"type=bind,source={home},target=/var/lib/opencodex",
        "--mount", f"type=bind,source={bootstrap_socket},target=/run/opencodex/bootstrap.sock",
        *label_arguments(labels),
        image,
    ]


def validate_upstream_lock_binding(
    candidate: dict[str, Any], lock_bytes: bytes
) -> dict[str, Any]:
    if hashlib.sha256(lock_bytes).hexdigest() != candidate["upstream_lock_sha256"]:
        fail("tracked upstream lock differs from the runtime candidate")
    lock = upstream_contract.validate_lock(
        upstream_contract.load_json_bytes(lock_bytes, "upstream lock")
    )
    expected_artifact = f"{lock['version']}-r{lock['image_revision']}"
    if candidate["artifact_version"] != expected_artifact:
        fail("runtime candidate artifact version differs from the upstream lock")
    return lock


def validate_inspected_image(
    inspected: Any,
    candidate: dict[str, Any],
    exact_image: str,
    upstream_lock: dict[str, Any],
) -> None:
    if (
        not isinstance(inspected, list)
        or len(inspected) != 1
        or not isinstance(inspected[0], dict)
    ):
        fail("Apple Container image inspection schema is invalid")
    resource = inspected[0]
    configuration = resource.get("configuration")
    if not isinstance(configuration, dict) or configuration.get("name") != exact_image:
        fail("Apple Container image inspection reference is invalid")
    descriptor = configuration.get("descriptor")
    if (
        not isinstance(descriptor, dict)
        or descriptor.get("digest") != candidate["image"]["index_digest"]
    ):
        fail("Apple Container image inspection index digest is invalid")

    arm64_platforms = [
        platform_entry
        for platform_entry in candidate["image"]["platforms"]
        if platform_entry.get("os") == "linux" and platform_entry.get("arch") == "arm64"
    ]
    if len(arm64_platforms) != 1:
        fail("runtime candidate does not contain one linux/arm64 platform")
    expected_arm64_digest = arm64_platforms[0]["digest"]
    expected_labels = {
        "org.opencontainers.image.source": "https://github.com/novelKR/OpenCodex-OCI-Gateway",
        "org.opencontainers.image.version": candidate["artifact_version"],
        "io.github.novelkr.opencodex.upstream.version": upstream_lock["version"],
        "io.github.novelkr.opencodex.upstream.revision": upstream_lock["revision"],
        "io.github.novelkr.opencodex.public-core.revision": candidate["source_revision"],
    }
    variants = resource.get("variants")
    if not isinstance(variants, list) or not variants:
        fail("Apple Container image inspection variants are invalid")
    matched = False
    for variant in variants:
        if not isinstance(variant, dict) or not isinstance(variant.get("platform"), dict):
            fail("Apple Container image inspection variant schema is invalid")
        platform_entry = variant["platform"]
        if platform_entry.get("os") != "linux" or platform_entry.get("architecture") != "arm64":
            continue
        if matched:
            fail("Apple Container image inspection has duplicate linux/arm64 variants")
        if variant.get("digest") != expected_arm64_digest:
            fail("Apple Container image inspection arm64 digest is invalid")
        image_config = variant.get("config")
        if (
            not isinstance(image_config, dict)
            or image_config.get("os") != "linux"
            or image_config.get("architecture") != "arm64"
            or not isinstance(image_config.get("config"), dict)
        ):
            fail("Apple Container image inspection arm64 configuration is invalid")
        labels = image_config["config"].get("Labels")
        if not isinstance(labels, dict) or any(
            labels.get(key) != value for key, value in expected_labels.items()
        ):
            fail("Apple Container image labels do not match the candidate")
        matched = True
    if not matched:
        fail("Apple Container image inspection omitted the linux/arm64 variant")


def verify_image(
    candidate: dict[str, Any],
    exact_image: str,
    upstream_lock: dict[str, Any],
) -> None:
    container("image", "pull", "--progress", "none", "--platform", "linux/arm64", exact_image, timeout=600)
    inspected = load_cli_json(
        container("image", "inspect", exact_image).stdout,
        "Apple Container image inspection",
    )
    validate_inspected_image(inspected, candidate, exact_image, upstream_lock)


def verify_inspection(
    inspected: Any,
    name: str,
    home: pathlib.Path,
    bootstrap_socket: pathlib.Path,
    network_name: str,
    *,
    expected_image: str,
    expected_index_digest: str,
    expected_state: str,
) -> None:
    configuration = validate_managed_container(
        inspected,
        name,
        expected_state=expected_state,
        expected_network=network_name,
    )
    image = configuration.get("image")
    descriptor = image.get("descriptor") if isinstance(image, dict) else None
    if (
        not isinstance(image, dict)
        or image.get("reference") != expected_image
        or not isinstance(descriptor, dict)
        or descriptor.get("digest") != expected_index_digest
    ):
        fail("Apple Container inspection does not identify the exact runtime image")
    platform_entry = configuration.get("platform")
    if (
        not isinstance(platform_entry, dict)
        or platform_entry.get("os") != "linux"
        or platform_entry.get("architecture") != "arm64"
    ):
        fail("Apple Container inspection does not identify linux/arm64")
    strings = all_strings(configuration)
    for expected in (
        str(home),
        str(bootstrap_socket),
        "/var/lib/opencodex",
        "/run/opencodex/bootstrap.sock",
    ):
        if expected not in strings and not any(expected in item for item in strings):
            fail("Apple Container inspection omitted an expected confinement value")
    published_ports = configuration.get("publishedPorts")
    if (
        not isinstance(published_ports, list)
        or len(published_ports) != 1
        or not isinstance(published_ports[0], dict)
        or set(published_ports[0]) != PUBLISHED_PORT_KEYS
    ):
        fail("Apple Container inspection has an invalid published port schema")
    published_port = published_ports[0]
    if (
        published_port["hostAddress"] != "127.0.0.1"
        or not isinstance(published_port["hostPort"], int)
        or isinstance(published_port["hostPort"], bool)
        or published_port["hostPort"] != 10210
        or not isinstance(published_port["containerPort"], int)
        or isinstance(published_port["containerPort"], bool)
        or published_port["containerPort"] != 10100
        or published_port["proto"] != "tcp"
        or not isinstance(published_port["count"], int)
        or isinstance(published_port["count"], bool)
        or not isinstance(published_port["count"], int)
        or isinstance(published_port["count"], bool)
        or published_port["count"] != 1
    ):
        fail("Apple Container inspection does not contain the exact loopback port mapping")
    published_sockets = configuration.get("publishedSockets")
    if not isinstance(published_sockets, list) or published_sockets:
        fail("Apple Container inspection contains an unexpected published socket")
    host_mounts: set[tuple[str, str]] = set()

    def collect_host_mounts(value: Any) -> None:
        if isinstance(value, dict):
            source = value.get("source")
            destination = value.get("destination", value.get("target"))
            if (
                isinstance(source, str)
                and source.startswith("/")
                and isinstance(destination, str)
            ):
                host_mounts.add((source, destination))
            for item in value.values():
                collect_host_mounts(item)
        elif isinstance(value, list):
            for item in value:
                collect_host_mounts(item)

    collect_host_mounts(configuration)
    expected_mounts = {
        (str(home), "/var/lib/opencodex"),
        (str(bootstrap_socket), "/run/opencodex/bootstrap.sock"),
    }
    if host_mounts != expected_mounts:
        fail("Apple Container inspection contains a prohibited or unexpected host mount")
    mounts = configuration.get("mounts")
    if (
        not isinstance(mounts, list)
        or len(mounts) != 3
        or any(not isinstance(mount, dict) for mount in mounts)
    ):
        fail("Apple Container inspection has an invalid mount schema")
    tmpfs_mounts = [
        mount
        for mount in mounts
        if mount.get("destination", mount.get("target")) == "/tmp"
    ]
    if len(tmpfs_mounts) != 1:
        fail("Apple Container inspection does not contain the exact /tmp tmpfs")
    tmpfs_mount = tmpfs_mounts[0]
    mount_kind = tmpfs_mount.get("type", tmpfs_mount.get("kind"))
    mount_source = tmpfs_mount.get("source")
    if mount_kind is not None:
        if (
            not isinstance(mount_kind, str)
            or mount_kind.lower() != "tmpfs"
            or mount_source not in (None, "", "tmpfs")
        ):
            fail("Apple Container inspection does not contain the exact /tmp tmpfs")
    elif not isinstance(mount_source, str) or mount_source.lower() != "tmpfs":
        fail("Apple Container inspection does not contain the exact /tmp tmpfs")
    banned = {
        os.environ.get("HOME", ""),
        os.environ.get("SSH_AUTH_SOCK", ""),
        "/var/run/docker.sock",
        "/run/docker.sock",
        "/Library/Keychains",
    }
    if any(
        value
        and any(item == value or item.startswith(value.rstrip("/") + "/") for item in strings)
        for value in banned
    ):
        fail("Apple Container inspection contains a prohibited host mount")


def execute(arguments: argparse.Namespace) -> dict[str, Any]:
    candidate_path = arguments.candidate
    candidate_bytes = manifest_contract.load_regular(candidate_path, "runtime candidate")
    candidate = manifest_contract.validate_candidate(
        manifest_contract.load_json_bytes(candidate_bytes, "runtime candidate")
    )
    if manifest_contract.canonical_candidate(candidate) != candidate_bytes:
        fail("runtime candidate is not canonical")
    lock_bytes = manifest_contract.load_regular(
        ROOT / "containers" / "opencodex" / "upstream.lock.json",
        "tracked upstream lock",
    )
    upstream_lock = validate_upstream_lock_binding(candidate, lock_bytes)
    validate_run_identity(
        candidate,
        arguments.source_revision,
        arguments.workflow_run_id,
        arguments.workflow_run_attempt,
    )
    capability_probe()
    require_free_port()
    suffix = hashlib.sha256(
        f"{arguments.source_revision}:{arguments.workflow_run_id}:{arguments.workflow_run_attempt}".encode()
    ).hexdigest()[:12]
    runtime_name = f"ocx-runtime-canary-{suffix}"
    fixture_name = f"ocx-provider-canary-{suffix}"
    network_name = f"ocx-canary-{suffix}"
    for kind, name in (("container", runtime_name), ("container", fixture_name), ("network", network_name)):
        require_name_available(kind, name)

    exact_image = f"{candidate['image']['repository']}@{candidate['image']['index_digest']}"
    verify_image(candidate, exact_image, upstream_lock)
    owned_labels = ownership_labels(
        arguments.source_revision,
        arguments.workflow_run_id,
        arguments.workflow_run_attempt,
    )
    api_token = protocol.exact_token()
    admin_token = protocol.exact_token()
    while admin_token == api_token:
        admin_token = protocol.exact_token()
    markers = protocol.secret_markers(api_token, admin_token)
    all_markers = markers
    # Register mutation intent before invoking Apple CLI. A create/run command
    # can time out after the service has already materialized the labeled
    # resource; cleanup must still inspect and remove that exact owned object.
    container_intents = [fixture_name, runtime_name]
    private_tmp = pathlib.Path("/private/tmp")
    if not private_tmp.is_dir() or private_tmp.is_symlink():
        fail("owner-controlled /private/tmp is unavailable")
    with tempfile.TemporaryDirectory(prefix="ocx-apple-canary-", dir=private_tmp) as directory:
        temporary = pathlib.Path(directory)
        home = temporary / "home"
        home.mkdir(mode=0o700)
        mock = pathlib.Path(__file__).resolve().parents[1] / "containers" / "opencodex" / "mock-provider.mjs"
        config = {
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
        config_path = home / "config.json"
        config_path.write_text(json.dumps(config, separators=(",", ":")) + "\n", encoding="utf-8")
        config_path.chmod(0o600)
        try:
            container(
                "network", "create", "--internal", *label_arguments(owned_labels), network_name,
                markers=markers,
            )
            require_owned_resource(
                "network", network_name, owned_labels, markers
            )
            container(
                "run", "--detach", "--name", fixture_name,
                "--network", network_name, "--platform", "linux/arm64",
                "--read-only", "--cap-drop", "ALL",
                *label_arguments(owned_labels),
                "--mount", f"type=bind,source={mock},target=/fixture/mock-provider.mjs,readonly",
                "--entrypoint", "bun", exact_image, "/fixture/mock-provider.mjs",
                markers=markers,
            )
            require_owned_resource(
                "container",
                fixture_name,
                owned_labels,
                markers,
                expected_state="running",
                expected_network=network_name,
            )
            bootstrap_socket = temporary / "bootstrap.sock"
            secret_server = protocol.BootstrapServer(bootstrap_socket, api_token, admin_token)
            secret_server.start()
            container(
                *runtime_arguments(
                    runtime_name, network_name, exact_image, home, bootstrap_socket,
                    owned_labels,
                ),
                timeout=300,
                markers=markers,
            )
            secret_server.wait()

            base_url = "http://127.0.0.1:10210"
            health = protocol.wait_health(base_url)
            if health.get("service") != "opencodex" or health.get("status") != "ok" or health.get("port") != 10100:
                fail("Apple runtime health identity does not report guest port 10100")
            protocol.request_json(base_url + "/v1/models", expected=401)
            protocol.request_json(base_url + "/v1/models", {protocol.TOKEN_HEADER: admin_token}, expected=401)
            models = protocol.request_json(base_url + "/v1/models", {protocol.TOKEN_HEADER: api_token})
            rows = models.get("data") if isinstance(models, dict) else None
            identifiers = [
                row.get("id") for row in rows or []
                if isinstance(row, dict) and isinstance(row.get("id"), str)
            ]
            model = next((item for item in identifiers if item.endswith("runtime-contract-model")), "")
            if not model:
                fail("Apple runtime model catalog omitted the fixture model")
            protocol.request_json(base_url + "/api/config", {protocol.TOKEN_HEADER: api_token}, expected=401)
            protocol.request_json(base_url + "/api/config", {protocol.TOKEN_HEADER: admin_token})
            protocol.post_sse(base_url, model, api_token)
            protocol.cancel_sse(base_url, model, api_token)
            protocol.wait_cancellation_observed(
                lambda: container(
                    "exec",
                    fixture_name,
                    "bun",
                    "-e",
                    protocol.CANCELLATION_STATUS_SCRIPT,
                    markers=markers,
                ).stdout
            )
            protocol.wait_health(base_url)
            protocol.websocket_response(base_url, model, api_token)

            inspection_output = container("inspect", runtime_name, markers=markers).stdout
            listing_output = container("list", "--all", "--format", "json", markers=markers).stdout
            logs_output = container("logs", runtime_name, markers=markers).stdout
            protocol.assert_no_secret(inspection_output, markers, "Apple container inspect")
            protocol.assert_no_secret(listing_output, markers, "Apple container list")
            protocol.assert_no_secret(logs_output, markers, "Apple container logs")
            verify_inspection(
                load_cli_json(inspection_output, "Apple Container inspection"),
                runtime_name,
                home,
                bootstrap_socket,
                network_name,
                expected_image=exact_image,
                expected_index_digest=candidate["image"]["index_digest"],
                expected_state="running",
            )

            require_owned_resource(
                "container",
                runtime_name,
                owned_labels,
                markers,
                expected_state="running",
                expected_network=network_name,
            )
            container("stop", "--time", "15", runtime_name, timeout=30, markers=markers)
            require_owned_resource(
                "container",
                runtime_name,
                owned_labels,
                markers,
                expected_state="stopped",
            )
            for description, captured in (
                (
                    "stopped Apple container inspect",
                    container("inspect", runtime_name, markers=markers).stdout,
                ),
                (
                    "stopped Apple container list",
                    container(
                        "list", "--all", "--format", "json", markers=markers
                    ).stdout,
                ),
                (
                    "stopped Apple container logs",
                    container("logs", runtime_name, markers=markers).stdout,
                ),
            ):
                protocol.assert_no_secret(captured, markers, description)
            container("delete", runtime_name, markers=markers)
            require_resource_absent("container", runtime_name, markers)

            second_api = protocol.exact_token()
            second_admin = protocol.exact_token()
            while second_admin == second_api:
                second_admin = protocol.exact_token()
            second_markers = protocol.secret_markers(second_api, second_admin)
            all_markers = markers + second_markers
            second_socket = temporary / "bootstrap-second.sock"
            second_server = protocol.BootstrapServer(second_socket, second_api, second_admin)
            second_server.start()
            container(
                *runtime_arguments(
                    runtime_name, network_name, exact_image, home, second_socket,
                    owned_labels,
                ),
                timeout=300,
                markers=all_markers,
            )
            second_server.wait()
            protocol.wait_health(base_url)
            protocol.request_json(base_url + "/v1/models", {protocol.TOKEN_HEADER: second_api})
            second_inspection_output = container(
                "inspect", runtime_name, markers=all_markers
            ).stdout
            for description, captured in (
                ("recreated Apple container inspect", second_inspection_output),
                ("recreated Apple container list", container("list", "--all", "--format", "json", markers=all_markers).stdout),
                ("recreated Apple container logs", container("logs", runtime_name, markers=all_markers).stdout),
            ):
                protocol.assert_no_secret(captured, all_markers, description)
            verify_inspection(
                load_cli_json(
                    second_inspection_output,
                    "recreated Apple Container inspection",
                ),
                runtime_name,
                home,
                second_socket,
                network_name,
                expected_image=exact_image,
                expected_index_digest=candidate["image"]["index_digest"],
                expected_state="running",
            )
            require_owned_resource(
                "container",
                runtime_name,
                owned_labels,
                all_markers,
                expected_state="running",
                expected_network=network_name,
            )
            container("stop", "--time", "15", runtime_name, timeout=30, markers=all_markers)
            require_owned_resource(
                "container",
                runtime_name,
                owned_labels,
                all_markers,
                expected_state="stopped",
            )
            for description, captured in (
                (
                    "recreated stopped Apple container inspect",
                    container("inspect", runtime_name, markers=all_markers).stdout,
                ),
                (
                    "recreated stopped Apple container list",
                    container(
                        "list", "--all", "--format", "json", markers=all_markers
                    ).stdout,
                ),
                (
                    "recreated stopped Apple container logs",
                    container("logs", runtime_name, markers=all_markers).stdout,
                ),
            ):
                protocol.assert_no_secret(captured, all_markers, description)
        finally:
            cleanup_owned_resources(
                container_intents,
                network_name,
                owned_labels,
                all_markers,
            )

    # Container/network absence is not proof that a leaked listener was
    # released.  Refuse to produce a promotion receipt until both loopback
    # families can bind the fixed host port again.
    require_free_port()

    receipt = {
        "schema": 1,
        "result": "passed",
        "source_revision": arguments.source_revision,
        "workflow_run_id": arguments.workflow_run_id,
        "workflow_run_attempt": arguments.workflow_run_attempt,
        "candidate_sha256": hashlib.sha256(candidate_bytes).hexdigest(),
        "index_digest": candidate["image"]["index_digest"],
        "arm64_digest": candidate["image"]["platforms"][1]["digest"],
    }
    receipt_bytes = (json.dumps(receipt, indent=2) + "\n").encode("utf-8")
    if arguments.receipt.exists() or arguments.receipt.is_symlink():
        fail("canary receipt output must not already exist")
    descriptor = os.open(arguments.receipt, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as stream:
        stream.write(receipt_bytes)
        stream.flush()
        os.fsync(stream.fileno())
    return receipt


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--candidate", type=pathlib.Path, required=True)
    result.add_argument("--source-revision", required=True)
    result.add_argument("--workflow-run-id", required=True)
    result.add_argument("--workflow-run-attempt", type=int, required=True)
    result.add_argument("--receipt", type=pathlib.Path, required=True)
    return result


def main() -> int:
    try:
        receipt = execute(parser().parse_args())
        print(json.dumps({"schema": 1, "status": "passed", "index_digest": receipt["index_digest"]}, separators=(",", ":")))
        return 0
    except (CanaryError, manifest_contract.ContractError, protocol.ContractError, OSError, ValueError, json.JSONDecodeError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

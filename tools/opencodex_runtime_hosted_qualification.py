#!/usr/bin/env python3
"""Create and verify fail-closed GitHub-hosted runtime qualification receipts.

These receipts deliberately certify only OCI Linux/ARM64 execution and macOS
contract testing.  They can never authorize a stable Runtime Manifest or claim
that Apple Container was exercised.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from typing import Any, NoReturn


TOOLS = pathlib.Path(__file__).resolve().parent
if str(TOOLS) not in sys.path:
    sys.path.insert(0, str(TOOLS))

import opencodex_runtime_manifest as manifest_contract  # noqa: E402


MAX_RECEIPT_BYTES = 64 * 1024
HEX_SHA256 = re.compile(r"[0-9a-f]{64}")
KINDS = {
    "linux": "opencodex-runtime-linux-arm64-canary",
    "macos": "opencodex-runtime-macos-contract",
    "hosted": "opencodex-runtime-hosted-qualification",
    "public": "opencodex-runtime-public-candidate-verification",
}
LEVELS = {
    "linux": "linux-arm64-oci",
    "macos": "macos-contract",
    "hosted": "hosted-candidate",
    "public": "public-candidate",
}
IDENTITY_KEYS = {
    "artifact_version",
    "release_sequence",
    "source_revision",
    "workflow_run_id",
    "workflow_run_attempt",
    "candidate_sha256",
    "image",
}
BASE_KEYS = {
    "schema",
    "artifact_kind",
    "qualification_level",
    "result",
    "apple_container_live",
    "stable_promotion_eligible",
    *IDENTITY_KEYS,
}
IMAGE_KEYS = {"repository", "index_digest", "arm64_digest"}
RUNNER_KEYS = {"environment", "os", "arch"}
EVIDENCE_KEYS = {"linux_arm64_canary_sha256", "macos_contract_sha256"}


class ContractError(RuntimeError):
    """A hosted qualification input or receipt violated the strict contract."""


def fail(message: str) -> NoReturn:
    raise ContractError(message)


def canonical(document: dict[str, Any]) -> bytes:
    return (
        json.dumps(document, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    ).encode("utf-8")


def require_keys(value: Any, expected: set[str], description: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != expected:
        fail(f"{description} has unsupported fields")
    return value


def sha256_string(value: Any, description: str) -> str:
    if not isinstance(value, str) or HEX_SHA256.fullmatch(value) is None:
        fail(f"{description} is not a lowercase SHA-256 value")
    return value


def load_regular(path: pathlib.Path, description: str) -> bytes:
    if not path.is_file() or path.is_symlink():
        fail(f"{description} must be a regular file")
    data = path.read_bytes()
    if not data or len(data) > MAX_RECEIPT_BYTES:
        fail(f"{description} size is invalid")
    return data


def load_candidate(
    path: pathlib.Path,
    *,
    source_revision: str,
    workflow_run_id: str,
    workflow_run_attempt: int,
    index_digest: str,
    arm64_digest: str,
) -> tuple[dict[str, Any], bytes, dict[str, Any]]:
    data = load_regular(path, "runtime candidate")
    try:
        document = manifest_contract.load_json_bytes(data, "runtime candidate")
        candidate = manifest_contract.validate_candidate(document)
        if manifest_contract.canonical_candidate(candidate) != data:
            fail("runtime candidate bytes are not canonical")
        manifest_contract.commit_string(source_revision, "expected source revision")
        manifest_contract.workflow_run_id(workflow_run_id, "expected workflow run ID")
        manifest_contract.positive_int64(
            workflow_run_attempt, "expected workflow run attempt"
        )
        manifest_contract.digest_string(index_digest, "expected index digest")
        manifest_contract.digest_string(arm64_digest, "expected arm64 digest")
    except manifest_contract.ContractError as error:
        raise ContractError(str(error)) from error

    actual = {
        "source_revision": candidate["source_revision"],
        "workflow_run_id": candidate["workflow_run_id"],
        "workflow_run_attempt": candidate["workflow_run_attempt"],
        "index_digest": candidate["image"]["index_digest"],
        "arm64_digest": candidate["image"]["platforms"][1]["digest"],
    }
    expected = {
        "source_revision": source_revision,
        "workflow_run_id": workflow_run_id,
        "workflow_run_attempt": workflow_run_attempt,
        "index_digest": index_digest,
        "arm64_digest": arm64_digest,
    }
    for key, value in expected.items():
        if actual[key] != value:
            fail(f"runtime candidate {key} does not match the expected value")

    identity = {
        "artifact_version": candidate["artifact_version"],
        "release_sequence": candidate["release_sequence"],
        "source_revision": candidate["source_revision"],
        "workflow_run_id": candidate["workflow_run_id"],
        "workflow_run_attempt": candidate["workflow_run_attempt"],
        "candidate_sha256": hashlib.sha256(data).hexdigest(),
        "image": {
            "repository": candidate["image"]["repository"],
            "index_digest": candidate["image"]["index_digest"],
            "arm64_digest": candidate["image"]["platforms"][1]["digest"],
        },
    }
    return candidate, data, identity


def validate_identity(receipt: dict[str, Any]) -> None:
    try:
        manifest_contract.version_tuple(receipt["artifact_version"])
        manifest_contract.positive_integer(
            receipt["release_sequence"], "receipt release sequence"
        )
        manifest_contract.commit_string(
            receipt["source_revision"], "receipt source revision"
        )
        manifest_contract.workflow_run_id(
            receipt["workflow_run_id"], "receipt workflow run ID"
        )
        manifest_contract.positive_int64(
            receipt["workflow_run_attempt"], "receipt workflow run attempt"
        )
        sha256_string(receipt["candidate_sha256"], "candidate hash")
        image = require_keys(receipt["image"], IMAGE_KEYS, "receipt image")
        if image["repository"] != manifest_contract.RUNTIME_REPOSITORY:
            fail("receipt image repository is unsupported")
        manifest_contract.digest_string(image["index_digest"], "receipt index digest")
        manifest_contract.digest_string(image["arm64_digest"], "receipt arm64 digest")
        if image["index_digest"] == image["arm64_digest"]:
            fail("receipt index and arm64 digests must differ")
    except manifest_contract.ContractError as error:
        raise ContractError(str(error)) from error


def validate_receipt(document: Any, kind: str) -> dict[str, Any]:
    if kind not in KINDS:
        fail("receipt kind is unsupported")
    extra = {"runner"} if kind in {"linux", "macos"} else {"evidence"}
    if kind == "public":
        extra |= {
            "runner",
            "verification_workflow_run_id",
            "verification_workflow_run_attempt",
            "anonymous_exact_digest_pull",
            "public_ready",
        }
    receipt = require_keys(document, BASE_KEYS | extra, f"{kind} receipt")
    if type(receipt["schema"]) is not int or receipt["schema"] != 1:
        fail("receipt schema is unsupported")
    if receipt["artifact_kind"] != KINDS[kind]:
        fail("receipt artifact kind is unsupported")
    if receipt["qualification_level"] != LEVELS[kind]:
        fail("receipt qualification level is unsupported")
    if receipt["result"] != "passed":
        fail("receipt result is not passed")
    if receipt["apple_container_live"] is not False:
        fail("hosted evidence must not claim Apple Container live execution")
    if receipt["stable_promotion_eligible"] is not False:
        fail("hosted evidence must not authorize stable promotion")
    validate_identity(receipt)

    if kind in {"linux", "macos"}:
        runner = require_keys(receipt["runner"], RUNNER_KEYS, "receipt runner")
        expected = {
            "environment": "github-hosted",
            "os": "Linux" if kind == "linux" else "macOS",
            "arch": "ARM64",
        }
        if runner != expected:
            fail("receipt runner identity is unsupported")
    else:
        evidence = require_keys(receipt["evidence"], EVIDENCE_KEYS, "receipt evidence")
        for name, value in evidence.items():
            sha256_string(value, name)
        if evidence["linux_arm64_canary_sha256"] == evidence["macos_contract_sha256"]:
            fail("hosted evidence hashes must be distinct")

    if kind == "public":
        runner = require_keys(receipt["runner"], RUNNER_KEYS, "receipt runner")
        if runner != {
            "environment": "github-hosted",
            "os": "Linux",
            "arch": "ARM64",
        }:
            fail("public receipt runner identity is unsupported")
        try:
            manifest_contract.workflow_run_id(
                receipt["verification_workflow_run_id"],
                "verification workflow run ID",
            )
            manifest_contract.positive_int64(
                receipt["verification_workflow_run_attempt"],
                "verification workflow run attempt",
            )
        except manifest_contract.ContractError as error:
            raise ContractError(str(error)) from error
        if receipt["anonymous_exact_digest_pull"] is not True:
            fail("public receipt must attest an anonymous exact-digest pull")
        if receipt["public_ready"] is not True:
            fail("public receipt must explicitly mark the candidate public-ready")
    return receipt


def load_receipt(path: pathlib.Path, kind: str) -> tuple[dict[str, Any], bytes]:
    data = load_regular(path, f"{kind} receipt")
    try:
        document = manifest_contract.load_json_bytes(data, f"{kind} receipt")
    except manifest_contract.ContractError as error:
        raise ContractError(str(error)) from error
    receipt = validate_receipt(document, kind)
    if canonical(receipt) != data:
        fail(f"{kind} receipt bytes are not canonical")
    return receipt, data


def verify_binding(receipt: dict[str, Any], identity: dict[str, Any]) -> None:
    for key in IDENTITY_KEYS:
        if receipt[key] != identity[key]:
            fail(f"receipt {key} is not bound to the runtime candidate")


def write_new(path: pathlib.Path, document: dict[str, Any]) -> None:
    if path.exists() or path.is_symlink():
        fail("receipt output must not already exist")
    path.write_bytes(canonical(document))


def candidate_from_arguments(arguments: argparse.Namespace) -> dict[str, Any]:
    _, _, identity = load_candidate(
        arguments.candidate,
        source_revision=arguments.source_revision,
        workflow_run_id=arguments.workflow_run_id,
        workflow_run_attempt=arguments.workflow_run_attempt,
        index_digest=arguments.index_digest,
        arm64_digest=arguments.arm64_digest,
    )
    return identity


def base_receipt(kind: str, identity: dict[str, Any]) -> dict[str, Any]:
    return {
        "schema": 1,
        "artifact_kind": KINDS[kind],
        "qualification_level": LEVELS[kind],
        "result": "passed",
        "apple_container_live": False,
        "stable_promotion_eligible": False,
        **identity,
    }


def command_create_platform(arguments: argparse.Namespace, kind: str) -> int:
    expected_os = "Linux" if kind == "linux" else "macOS"
    if (
        arguments.runner_environment != "github-hosted"
        or arguments.runner_os != expected_os
        or arguments.runner_arch != "ARM64"
    ):
        fail(f"{kind} receipt requires a GitHub-hosted {expected_os}/ARM64 runner")
    identity = candidate_from_arguments(arguments)
    receipt = base_receipt(kind, identity)
    receipt["runner"] = {
        "environment": "github-hosted",
        "os": expected_os,
        "arch": "ARM64",
    }
    validate_receipt(receipt, kind)
    write_new(arguments.output, receipt)
    return 0


def command_verify_platform(arguments: argparse.Namespace, kind: str) -> int:
    identity = candidate_from_arguments(arguments)
    receipt, _ = load_receipt(arguments.receipt, kind)
    verify_binding(receipt, identity)
    print(json.dumps({"schema": 1, "status": "verified"}, separators=(",", ":")))
    return 0


def command_create_hosted(arguments: argparse.Namespace) -> int:
    identity = candidate_from_arguments(arguments)
    linux, linux_bytes = load_receipt(arguments.linux_receipt, "linux")
    macos, macos_bytes = load_receipt(arguments.macos_receipt, "macos")
    verify_binding(linux, identity)
    verify_binding(macos, identity)
    receipt = base_receipt("hosted", identity)
    receipt["evidence"] = {
        "linux_arm64_canary_sha256": hashlib.sha256(linux_bytes).hexdigest(),
        "macos_contract_sha256": hashlib.sha256(macos_bytes).hexdigest(),
    }
    validate_receipt(receipt, "hosted")
    write_new(arguments.output, receipt)
    return 0


def command_verify_hosted(arguments: argparse.Namespace) -> int:
    identity = candidate_from_arguments(arguments)
    receipt, _ = load_receipt(arguments.receipt, "hosted")
    verify_binding(receipt, identity)
    if arguments.linux_receipt is not None and arguments.macos_receipt is not None:
        linux, linux_bytes = load_receipt(arguments.linux_receipt, "linux")
        macos, macos_bytes = load_receipt(arguments.macos_receipt, "macos")
        verify_binding(linux, identity)
        verify_binding(macos, identity)
        expected = {
            "linux_arm64_canary_sha256": hashlib.sha256(linux_bytes).hexdigest(),
            "macos_contract_sha256": hashlib.sha256(macos_bytes).hexdigest(),
        }
        if receipt["evidence"] != expected:
            fail("hosted receipt evidence hashes do not match the component receipts")
    elif arguments.linux_receipt is not None or arguments.macos_receipt is not None:
        fail("both component receipts are required when verifying hosted evidence")
    print(json.dumps({"schema": 1, "status": "verified"}, separators=(",", ":")))
    return 0


def command_create_public(arguments: argparse.Namespace) -> int:
    if (
        arguments.runner_environment != "github-hosted"
        or arguments.runner_os != "Linux"
        or arguments.runner_arch != "ARM64"
    ):
        fail("public verification requires a GitHub-hosted Linux/ARM64 runner")
    identity = candidate_from_arguments(arguments)
    hosted, _ = load_receipt(arguments.hosted_receipt, "hosted")
    verify_binding(hosted, identity)
    try:
        manifest_contract.workflow_run_id(
            arguments.verification_workflow_run_id,
            "verification workflow run ID",
        )
        manifest_contract.positive_int64(
            arguments.verification_workflow_run_attempt,
            "verification workflow run attempt",
        )
    except manifest_contract.ContractError as error:
        raise ContractError(str(error)) from error
    receipt = base_receipt("public", identity)
    receipt["evidence"] = dict(hosted["evidence"])
    receipt["runner"] = {
        "environment": "github-hosted",
        "os": "Linux",
        "arch": "ARM64",
    }
    receipt.update(
        {
            "verification_workflow_run_id": arguments.verification_workflow_run_id,
            "verification_workflow_run_attempt": arguments.verification_workflow_run_attempt,
            "anonymous_exact_digest_pull": True,
            "public_ready": True,
        }
    )
    validate_receipt(receipt, "public")
    write_new(arguments.output, receipt)
    return 0


def command_verify_public(arguments: argparse.Namespace) -> int:
    identity = candidate_from_arguments(arguments)
    hosted, _ = load_receipt(arguments.hosted_receipt, "hosted")
    verify_binding(hosted, identity)
    receipt, _ = load_receipt(arguments.receipt, "public")
    verify_binding(receipt, identity)
    if receipt["evidence"] != hosted["evidence"]:
        fail("public receipt is not bound to the hosted qualification evidence")
    if arguments.verification_workflow_run_id is not None:
        try:
            manifest_contract.workflow_run_id(
                arguments.verification_workflow_run_id,
                "expected verification workflow run ID",
            )
        except manifest_contract.ContractError as error:
            raise ContractError(str(error)) from error
        if (
            receipt["verification_workflow_run_id"]
            != arguments.verification_workflow_run_id
        ):
            fail("public receipt verification workflow run ID differs")
    if arguments.verification_workflow_run_attempt is not None:
        try:
            manifest_contract.positive_int64(
                arguments.verification_workflow_run_attempt,
                "expected verification workflow run attempt",
            )
        except manifest_contract.ContractError as error:
            raise ContractError(str(error)) from error
        if (
            receipt["verification_workflow_run_attempt"]
            != arguments.verification_workflow_run_attempt
        ):
            fail("public receipt verification workflow run attempt differs")
    print(json.dumps({"schema": 1, "status": "verified"}, separators=(",", ":")))
    return 0


def add_candidate_arguments(command: argparse.ArgumentParser) -> None:
    command.add_argument("--candidate", type=pathlib.Path, required=True)
    command.add_argument("--source-revision", required=True)
    command.add_argument("--workflow-run-id", required=True)
    command.add_argument("--workflow-run-attempt", type=int, required=True)
    command.add_argument("--index-digest", required=True)
    command.add_argument("--arm64-digest", required=True)


def add_runner_arguments(command: argparse.ArgumentParser) -> None:
    command.add_argument("--runner-environment", required=True)
    command.add_argument("--runner-os", required=True)
    command.add_argument("--runner-arch", required=True)


def add_output(command: argparse.ArgumentParser) -> None:
    command.add_argument("--output", type=pathlib.Path, required=True)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)

    for name, kind in (("create-linux", "linux"), ("create-macos", "macos")):
        command = commands.add_parser(name)
        add_candidate_arguments(command)
        add_runner_arguments(command)
        add_output(command)
        command.set_defaults(handler=lambda args, selected=kind: command_create_platform(args, selected))

    for name, kind in (("verify-linux", "linux"), ("verify-macos", "macos")):
        command = commands.add_parser(name)
        add_candidate_arguments(command)
        command.add_argument("--receipt", type=pathlib.Path, required=True)
        command.set_defaults(handler=lambda args, selected=kind: command_verify_platform(args, selected))

    hosted = commands.add_parser("create-hosted")
    add_candidate_arguments(hosted)
    hosted.add_argument("--linux-receipt", type=pathlib.Path, required=True)
    hosted.add_argument("--macos-receipt", type=pathlib.Path, required=True)
    add_output(hosted)
    hosted.set_defaults(handler=command_create_hosted)

    verify_hosted = commands.add_parser("verify-hosted")
    add_candidate_arguments(verify_hosted)
    verify_hosted.add_argument("--receipt", type=pathlib.Path, required=True)
    verify_hosted.add_argument("--linux-receipt", type=pathlib.Path)
    verify_hosted.add_argument("--macos-receipt", type=pathlib.Path)
    verify_hosted.set_defaults(handler=command_verify_hosted)

    public = commands.add_parser("create-public")
    add_candidate_arguments(public)
    add_runner_arguments(public)
    public.add_argument("--hosted-receipt", type=pathlib.Path, required=True)
    public.add_argument("--verification-workflow-run-id", required=True)
    public.add_argument("--verification-workflow-run-attempt", type=int, required=True)
    add_output(public)
    public.set_defaults(handler=command_create_public)

    verify_public = commands.add_parser("verify-public")
    add_candidate_arguments(verify_public)
    verify_public.add_argument("--hosted-receipt", type=pathlib.Path, required=True)
    verify_public.add_argument("--receipt", type=pathlib.Path, required=True)
    verify_public.add_argument("--verification-workflow-run-id")
    verify_public.add_argument("--verification-workflow-run-attempt", type=int)
    verify_public.set_defaults(handler=command_verify_public)
    return result


def main() -> int:
    try:
        arguments = parser().parse_args()
        return arguments.handler(arguments)
    except (ContractError, OSError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

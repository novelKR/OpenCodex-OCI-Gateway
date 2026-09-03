#!/usr/bin/env python3
"""Build and strictly validate signed OpenCodex runtime release manifests.

This helper deliberately does not hold or read the release signing key.  The
release workflow writes canonical manifest bytes with this program and signs
those exact bytes with OpenSSL in the protected ``runtime-release``
environment.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import hashlib
import json
import math
import pathlib
import re
import sys
from typing import Any, NoReturn


MAX_MANIFEST_BYTES = 64 * 1024
MAX_ATTESTATION_BYTES = 32 * 1024 * 1024
UINT32_MAX = (1 << 32) - 1
UINT64_MAX = (1 << 64) - 1
INT64_MAX = (1 << 63) - 1
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
HEX_SHA256 = re.compile(r"[0-9a-f]{64}")
COMMIT = re.compile(r"[0-9a-f]{40}")
ARTIFACT_VERSION = re.compile(
    r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-r([1-9][0-9]*)"
)
RUNTIME_REPOSITORY = "ghcr.io/novelkr/opencodex-runtime"
SOURCE_REPOSITORY = "novelKR/OpenCodex-OCI-Gateway"
ARTIFACT_KIND = "opencodex-runtime-image"
CANDIDATE_KEYS = {
    "schema",
    "artifact_kind",
    "artifact_version",
    "release_sequence",
    "channel",
    "source_revision",
    "workflow_run_id",
    "workflow_run_attempt",
    "upstream_lock_sha256",
    "candidate_tag",
    "image",
    "attestations",
}
CANDIDATE_IMAGE_KEYS = {"repository", "index_digest", "platforms"}
CANDIDATE_ATTESTATION_KEYS = {"buildkit_sbom", "buildkit_provenance", "github_provenance"}

ROOT_KEYS = {
    "schema",
    "artifact_kind",
    "artifact_version",
    "release_sequence",
    "channel",
    "source",
    "upstream",
    "image",
    "compatibility",
    "canary",
    "trust_key_id",
}
SOURCE_KEYS = {"repository", "revision", "upstream_lock_sha256"}
UPSTREAM_KEYS = {
    "repository",
    "release_id",
    "release_tag",
    "version",
    "revision",
    "npm_package",
    "npm_integrity",
}
IMAGE_KEYS = {"repository", "index_digest", "platforms"}
PLATFORM_KEYS = {"os", "arch", "digest"}
COMPATIBILITY_KEYS = {
    "minimum_relay_version",
    "minimum_macos",
    "minimum_apple_container",
    "management_api_revision",
    "secret_delivery",
    "state_format_revision",
}
CANARY_KEYS = {
    "source_revision",
    "workflow_run_id",
    "workflow_run_attempt",
    "result",
}


class ContractError(RuntimeError):
    """A candidate or manifest violated the immutable runtime contract."""


def fail(message: str) -> NoReturn:
    raise ContractError(message)


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def reject_json_constant(value: str) -> NoReturn:
    fail(f"non-finite JSON number is unsupported: {value}")


def finite_json_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        fail("non-finite JSON number is unsupported")
    return parsed


def load_json_bytes(data: bytes, description: str, limit: int = MAX_MANIFEST_BYTES) -> Any:
    if not data or len(data) > limit:
        fail(f"{description} size is invalid")
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as error:
        raise ContractError(f"{description} is not UTF-8") from error
    try:
        return json.loads(
            text,
            object_pairs_hook=strict_object,
            parse_constant=reject_json_constant,
            parse_float=finite_json_float,
        )
    except ContractError:
        raise
    except json.JSONDecodeError as error:
        raise ContractError(f"{description} is not one complete JSON value") from error


def load_regular(path: pathlib.Path, description: str, limit: int = MAX_MANIFEST_BYTES) -> bytes:
    if not path.is_file() or path.is_symlink():
        fail(f"{description} must be a regular file")
    data = path.read_bytes()
    if not data or len(data) > limit:
        fail(f"{description} size is invalid")
    return data


def require_keys(value: Any, expected: set[str], description: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != expected:
        fail(f"{description} has unsupported fields")
    return value


def positive_integer(value: Any, description: str) -> int:
    if (
        not isinstance(value, int)
        or isinstance(value, bool)
        or value < 1
        or value > UINT64_MAX
    ):
        fail(f"{description} must be a positive UInt64 integer")
    return value


def exact_integer(value: Any, expected: int, description: str) -> int:
    if type(value) is not int or value != expected:
        fail(f"{description} is unsupported")
    return value


def positive_int64(value: Any, description: str) -> int:
    result = positive_integer(value, description)
    if result > INT64_MAX:
        fail(f"{description} exceeds the Int64 consumer contract")
    return result


def exact_string(value: Any, expected: str, description: str) -> str:
    if value != expected:
        fail(f"{description} is unsupported")
    return expected


def digest_string(value: Any, description: str) -> str:
    if not isinstance(value, str) or not DIGEST.fullmatch(value):
        fail(f"{description} is not an exact sha256 digest")
    return value


def commit_string(value: Any, description: str) -> str:
    if not isinstance(value, str) or not COMMIT.fullmatch(value):
        fail(f"{description} is not a full lowercase commit ID")
    return value


def version_tuple(value: str) -> tuple[int, int, int, int]:
    match = ARTIFACT_VERSION.fullmatch(value) if isinstance(value, str) else None
    if not match:
        fail("artifact_version must be strict <semver>-r<N>")
    parts = match.groups()
    if any(len(part) > 10 for part in parts[:3]):
        fail("artifact SemVer components exceed the UInt32 consumer contract")
    if len(parts[3]) > 20:
        fail("artifact image revision exceeds the UInt64 consumer contract")
    parsed = tuple(int(part) for part in parts)
    if any(part > UINT32_MAX for part in parsed[:3]):
        fail("artifact SemVer components exceed the UInt32 consumer contract")
    if parsed[3] > UINT64_MAX:
        fail("artifact image revision exceeds the UInt64 consumer contract")
    return parsed  # type: ignore[return-value]


def workflow_run_id(value: Any, description: str) -> str:
    if not isinstance(value, str) or not re.fullmatch(r"[1-9][0-9]*", value):
        fail(f"{description} is invalid")
    if len(value) > 20 or int(value) > UINT64_MAX:
        fail(f"{description} exceeds the UInt64 consumer contract")
    return value


def validate_manifest(document: Any) -> dict[str, Any]:
    root = require_keys(document, ROOT_KEYS, "runtime manifest")
    exact_integer(root["schema"], 1, "runtime manifest schema")
    exact_string(root["artifact_kind"], ARTIFACT_KIND, "artifact_kind")
    version_tuple(root["artifact_version"])
    positive_integer(root["release_sequence"], "release_sequence")
    exact_string(root["channel"], "stable", "channel")

    source = require_keys(root["source"], SOURCE_KEYS, "source")
    exact_string(source["repository"], SOURCE_REPOSITORY, "source repository")
    commit_string(source["revision"], "source revision")
    if not isinstance(source["upstream_lock_sha256"], str) or not HEX_SHA256.fullmatch(
        source["upstream_lock_sha256"]
    ):
        fail("upstream lock hash is invalid")

    upstream = require_keys(root["upstream"], UPSTREAM_KEYS, "upstream")
    exact_string(upstream["repository"], "lidge-jun/opencodex", "upstream repository")
    positive_int64(upstream["release_id"], "upstream release ID")
    if not isinstance(upstream["version"], str):
        fail("upstream version is invalid")
    artifact = version_tuple(root["artifact_version"])
    expected_version = ".".join(str(part) for part in artifact[:3])
    if upstream["version"] != expected_version or upstream["release_tag"] != f"v{expected_version}":
        fail("upstream version does not match artifact_version")
    commit_string(upstream["revision"], "upstream revision")
    exact_string(upstream["npm_package"], "@bitkyc08/opencodex", "npm package")
    integrity = upstream["npm_integrity"]
    if not isinstance(integrity, str) or not integrity.startswith("sha512-"):
        fail("npm integrity is invalid")
    try:
        integrity_digest = base64.b64decode(integrity.removeprefix("sha512-"), validate=True)
    except (binascii.Error, ValueError) as error:
        raise ContractError("npm integrity is invalid") from error
    if (
        len(integrity_digest) != 64
        or base64.b64encode(integrity_digest).decode("ascii") != integrity.removeprefix("sha512-")
    ):
        fail("npm integrity is invalid")

    image = require_keys(root["image"], IMAGE_KEYS, "image")
    exact_string(image["repository"], RUNTIME_REPOSITORY, "image repository")
    digest_string(image["index_digest"], "image index digest")
    if not isinstance(image["platforms"], list) or len(image["platforms"]) != 2:
        fail("image must contain exactly two executable platforms")
    expected_platforms = (("linux", "amd64"), ("linux", "arm64"))
    for position, expected in enumerate(expected_platforms):
        platform = require_keys(image["platforms"][position], PLATFORM_KEYS, "platform")
        if (platform["os"], platform["arch"]) != expected:
            fail("image platforms must be ordered linux/amd64 then linux/arm64")
        digest_string(platform["digest"], f"{expected[1]} image digest")
    if image["platforms"][0]["digest"] == image["platforms"][1]["digest"]:
        fail("platform image digests must be distinct")

    compatibility = require_keys(root["compatibility"], COMPATIBILITY_KEYS, "compatibility")
    expected_compatibility = {
        "minimum_relay_version": "0.3.9",
        "minimum_macos": "26.0",
        "minimum_apple_container": "1.3.1",
        "management_api_revision": 1,
        "secret_delivery": "uds-v1",
        "state_format_revision": 1,
    }
    exact_integer(
        compatibility["management_api_revision"],
        1,
        "management API revision",
    )
    exact_integer(
        compatibility["state_format_revision"],
        1,
        "state format revision",
    )
    if compatibility != expected_compatibility:
        fail("runtime compatibility contract is unsupported")

    canary = require_keys(root["canary"], CANARY_KEYS, "canary")
    canary_revision = commit_string(canary["source_revision"], "canary source revision")
    if canary_revision != source["revision"]:
        fail("canary source revision does not match source")
    workflow_run_id(canary["workflow_run_id"], "canary workflow run ID")
    positive_int64(canary["workflow_run_attempt"], "canary workflow run attempt")
    exact_string(canary["result"], "passed", "canary result")
    if not isinstance(root["trust_key_id"], str) or not HEX_SHA256.fullmatch(root["trust_key_id"]):
        fail("runtime trust key ID is invalid")
    return root


def canonical_manifest(document: dict[str, Any]) -> bytes:
    validate_manifest(document)
    return (
        json.dumps(document, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    ).encode("utf-8")


def validate_candidate(document: Any) -> dict[str, Any]:
    root = require_keys(document, CANDIDATE_KEYS, "runtime candidate")
    exact_integer(root["schema"], 1, "runtime candidate schema")
    exact_string(root["artifact_kind"], ARTIFACT_KIND, "artifact_kind")
    version_tuple(root["artifact_version"])
    positive_integer(root["release_sequence"], "release_sequence")
    exact_string(root["channel"], "candidate", "candidate channel")
    source_revision = commit_string(root["source_revision"], "candidate source revision")
    workflow_run_id(root["workflow_run_id"], "candidate workflow run ID")
    positive_int64(root["workflow_run_attempt"], "candidate workflow run attempt")
    if not isinstance(root["upstream_lock_sha256"], str) or not HEX_SHA256.fullmatch(
        root["upstream_lock_sha256"]
    ):
        fail("candidate upstream lock hash is invalid")
    expected_tag = f"candidate-{root['artifact_version']}-{source_revision}"
    if root["candidate_tag"] != expected_tag:
        fail("candidate tag is not bound to artifact and source revision")

    image = require_keys(root["image"], CANDIDATE_IMAGE_KEYS, "candidate image")
    exact_string(image["repository"], RUNTIME_REPOSITORY, "candidate image repository")
    digest_string(image["index_digest"], "candidate image index digest")
    if not isinstance(image["platforms"], list) or len(image["platforms"]) != 2:
        fail("candidate image must contain exactly two executable platforms")
    for position, expected in enumerate((("linux", "amd64"), ("linux", "arm64"))):
        platform = require_keys(image["platforms"][position], PLATFORM_KEYS, "candidate platform")
        if (platform["os"], platform["arch"]) != expected:
            fail("candidate platforms must be ordered linux/amd64 then linux/arm64")
        digest_string(platform["digest"], f"candidate {expected[1]} image digest")
    if image["platforms"][0]["digest"] == image["platforms"][1]["digest"]:
        fail("candidate platform image digests must be distinct")

    attestations = require_keys(root["attestations"], CANDIDATE_ATTESTATION_KEYS, "candidate attestations")
    expected_attestations = {
        "buildkit_sbom": True,
        "buildkit_provenance": "max",
        "github_provenance": True,
    }
    if (
        attestations["buildkit_sbom"] is not True
        or not isinstance(attestations["buildkit_provenance"], str)
        or attestations["github_provenance"] is not True
        or attestations != expected_attestations
    ):
        fail("candidate attestation contract is unsupported")
    return root


def canonical_candidate(document: dict[str, Any]) -> bytes:
    validate_candidate(document)
    return (
        json.dumps(document, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    ).encode("utf-8")


def inspect_index(document: Any, expected_index: str) -> dict[str, str]:
    if (
        not isinstance(document, dict)
        or set(document) - {"schemaVersion", "mediaType", "manifests", "annotations"}
        or document.get("schemaVersion") != 2
        or document.get("mediaType") != "application/vnd.oci.image.index.v1+json"
        or not isinstance(document.get("manifests"), list)
    ):
        fail("OCI index is invalid")
    images: dict[str, str] = {}
    attestation_subjects: list[str] = []
    for descriptor in document["manifests"]:
        if (
            not isinstance(descriptor, dict)
            or set(descriptor) - {"mediaType", "digest", "size", "platform", "annotations", "artifactType"}
            or descriptor.get("mediaType") != "application/vnd.oci.image.manifest.v1+json"
            or not isinstance(descriptor.get("size"), int)
            or isinstance(descriptor.get("size"), bool)
            or descriptor["size"] < 1
        ):
            fail("OCI index descriptor is invalid")
        digest = digest_string(descriptor.get("digest"), "OCI descriptor digest")
        platform = descriptor.get("platform")
        if not isinstance(platform, dict):
            fail("OCI descriptor platform is missing")
        os_name = platform.get("os")
        architecture = platform.get("architecture")
        if os_name == "linux" and architecture in {"amd64", "arm64"}:
            if set(platform) - {"os", "architecture", "variant", "os.version", "os.features"}:
                fail("executable OCI platform has unsupported fields")
            if architecture in images:
                fail("OCI index contains a duplicate executable platform")
            if platform.get("variant") not in (None, ""):
                fail("OCI executable platform variant is unsupported")
            images[architecture] = digest
            continue
        annotations = descriptor.get("annotations")
        if (
            os_name == "unknown"
            and architecture == "unknown"
            and isinstance(annotations, dict)
            and annotations.get("vnd.docker.reference.type") == "attestation-manifest"
            and isinstance(annotations.get("vnd.docker.reference.digest"), str)
            and DIGEST.fullmatch(annotations["vnd.docker.reference.digest"])
        ):
            attestation_subjects.append(annotations["vnd.docker.reference.digest"])
            continue
        fail("OCI index contains an unexpected non-executable descriptor")
    if set(images) != {"amd64", "arm64"}:
        fail("OCI index must contain exactly linux/amd64 and linux/arm64 images")
    if images["amd64"] == images["arm64"]:
        fail("OCI platform digests must be distinct")
    if sorted(attestation_subjects) != sorted(images.values()):
        fail("OCI index must contain BuildKit SBOM/provenance attestations")
    digest_string(expected_index, "expected index digest")
    return {
        "index_digest": expected_index,
        "amd64_digest": images["amd64"],
        "arm64_digest": images["arm64"],
        "attestation_descriptors": str(len(attestation_subjects)),
    }


def load_index(path: pathlib.Path, expected_digest: str) -> dict[str, str]:
    data = load_regular(path, "OCI index", 4 * 1024 * 1024)
    expected = digest_string(expected_digest, "expected index digest")
    actual = "sha256:" + hashlib.sha256(data).hexdigest()
    if actual != expected:
        fail("OCI index bytes do not match the expected digest")
    document = load_json_bytes(data, "OCI index", 4 * 1024 * 1024)
    return inspect_index(document, expected)


def validate_spdx_sbom(document: Any, platform: str) -> None:
    if not isinstance(document, dict):
        fail(f"{platform} BuildKit SBOM predicate is invalid")
    if (
        not isinstance(document.get("spdxVersion"), str)
        or re.fullmatch(r"SPDX-2\.[0-9]+", document["spdxVersion"]) is None
        or document.get("SPDXID") != "SPDXRef-DOCUMENT"
        or document.get("dataLicense") != "CC0-1.0"
        or not isinstance(document.get("documentNamespace"), str)
        or not document["documentNamespace"]
    ):
        fail(f"{platform} BuildKit SPDX SBOM identity is invalid")
    creation = document.get("creationInfo")
    if (
        not isinstance(creation, dict)
        or not isinstance(creation.get("created"), str)
        or not creation["created"]
        or not isinstance(creation.get("creators"), list)
        or not creation["creators"]
        or any(not isinstance(item, str) or not item for item in creation["creators"])
    ):
        fail(f"{platform} BuildKit SPDX SBOM creation metadata is invalid")


def validate_buildkit_provenance(document: Any, platform: str) -> None:
    if not isinstance(document, dict):
        fail(f"{platform} BuildKit provenance predicate is invalid")
    if "buildDefinition" in document or "runDetails" in document:
        validate_buildkit_provenance_v1(document, platform)
        return
    validate_buildkit_provenance_v02(document, platform)


def validate_materials(materials: Any, platform: str) -> None:
    if not isinstance(materials, list) or not materials:
        fail(f"{platform} BuildKit provenance materials are invalid")
    for material in materials:
        if (
            not isinstance(material, dict)
            or not isinstance(material.get("uri"), str)
            or not material["uri"]
            or not isinstance(material.get("digest"), dict)
            or not material["digest"]
            or any(
                not isinstance(key, str)
                or not key
                or not isinstance(value, str)
                or not value
                for key, value in material["digest"].items()
            )
        ):
            fail(f"{platform} BuildKit provenance material is invalid")


def validate_buildkit_provenance_v02(document: dict[str, Any], platform: str) -> None:
    if document.get("buildType") != "https://mobyproject.org/buildkit@v1":
        fail(f"{platform} BuildKit provenance build type is invalid")
    builder = document.get("builder")
    invocation = document.get("invocation")
    metadata = document.get("metadata")
    materials = document.get("materials")
    build_config = document.get("buildConfig")
    if (
        not isinstance(builder, dict)
        or not isinstance(builder.get("id"), str)
        or not isinstance(invocation, dict)
        or not isinstance(metadata, dict)
        or not isinstance(build_config, dict)
        or not isinstance(build_config.get("llbDefinition"), list)
        or not build_config["llbDefinition"]
    ):
        fail(f"{platform} BuildKit provenance structure is invalid")
    environment = invocation.get("environment")
    parameters = invocation.get("parameters")
    completeness = metadata.get("completeness")
    if (
        not isinstance(environment, dict)
        or environment.get("platform") != platform
        or not isinstance(parameters, dict)
        or not parameters
        or not isinstance(completeness, dict)
        or completeness.get("parameters") is not True
        or completeness.get("environment") is not True
    ):
        fail(f"{platform} BuildKit max provenance is incomplete")
    validate_materials(materials, platform)


def validate_buildkit_provenance_v1(document: dict[str, Any], platform: str) -> None:
    build_definition = document.get("buildDefinition")
    run_details = document.get("runDetails")
    if not isinstance(build_definition, dict) or not isinstance(run_details, dict):
        fail(f"{platform} BuildKit SLSA v1 provenance structure is invalid")
    if build_definition.get("buildType") != (
        "https://github.com/moby/buildkit/blob/master/docs/attestations/"
        "slsa-definitions.md"
    ):
        fail(f"{platform} BuildKit provenance build type is invalid")
    external = build_definition.get("externalParameters")
    internal = build_definition.get("internalParameters")
    dependencies = build_definition.get("resolvedDependencies")
    build_config = internal.get("buildConfig") if isinstance(internal, dict) else None
    request = external.get("request") if isinstance(external, dict) else None
    builder = run_details.get("builder")
    metadata = run_details.get("metadata")
    completeness = (
        metadata.get("buildkit_completeness")
        if isinstance(metadata, dict)
        else None
    )
    if (
        not isinstance(external, dict)
        or not isinstance(request, dict)
        or not request
        or not isinstance(internal, dict)
        or not isinstance(build_config, dict)
        or not isinstance(build_config.get("llbDefinition"), list)
        or not build_config["llbDefinition"]
        or not isinstance(builder, dict)
        or not isinstance(builder.get("id"), str)
        or not isinstance(metadata, dict)
        or not isinstance(completeness, dict)
        or completeness.get("request") is not True
    ):
        fail(f"{platform} BuildKit max provenance is incomplete")
    validate_materials(dependencies, platform)


def inspect_attestations(
    sbom_amd64: pathlib.Path,
    provenance_amd64: pathlib.Path,
    sbom_arm64: pathlib.Path,
    provenance_arm64: pathlib.Path,
) -> dict[str, str]:
    files = {
        "linux/amd64": (sbom_amd64, provenance_amd64),
        "linux/arm64": (sbom_arm64, provenance_arm64),
    }
    for platform, (sbom_path, provenance_path) in files.items():
        sbom = load_json_bytes(
            load_regular(
                sbom_path,
                f"{platform} BuildKit SBOM",
                MAX_ATTESTATION_BYTES,
            ),
            f"{platform} BuildKit SBOM",
            MAX_ATTESTATION_BYTES,
        )
        provenance = load_json_bytes(
            load_regular(
                provenance_path,
                f"{platform} BuildKit provenance",
                MAX_ATTESTATION_BYTES,
            ),
            f"{platform} BuildKit provenance",
            MAX_ATTESTATION_BYTES,
        )
        validate_spdx_sbom(sbom, platform)
        validate_buildkit_provenance(provenance, platform)
    return {
        "sbom": "spdx",
        "provenance": "buildkit-max",
        "platforms": "linux/amd64,linux/arm64",
    }


def command_inspect_index(arguments: argparse.Namespace) -> int:
    print(
        json.dumps(
            load_index(arguments.index, arguments.expected_digest),
            separators=(",", ":"),
            sort_keys=True,
        )
    )
    return 0


def command_inspect_attestations(arguments: argparse.Namespace) -> int:
    print(
        json.dumps(
            inspect_attestations(
                arguments.sbom_amd64,
                arguments.provenance_amd64,
                arguments.sbom_arm64,
                arguments.provenance_arm64,
            ),
            separators=(",", ":"),
            sort_keys=True,
        )
    )
    return 0


def command_create_candidate(arguments: argparse.Namespace) -> int:
    import opencodex_upstream  # type: ignore[import-not-found]

    lock_bytes = load_regular(arguments.lock, "upstream lock")
    lock = opencodex_upstream.validate_lock(
        opencodex_upstream.load_json_bytes(lock_bytes, "upstream lock")
    )
    expected_artifact = f"{lock['version']}-r{lock['image_revision']}"
    if arguments.artifact_version != expected_artifact:
        fail("candidate artifact version does not match upstream lock")
    descriptors = load_index(arguments.index, arguments.index_digest)
    inspect_attestations(
        arguments.sbom_amd64,
        arguments.provenance_amd64,
        arguments.sbom_arm64,
        arguments.provenance_arm64,
    )
    document = {
        "schema": 1,
        "artifact_kind": ARTIFACT_KIND,
        "artifact_version": arguments.artifact_version,
        "release_sequence": arguments.release_sequence,
        "channel": "candidate",
        "source_revision": arguments.source_revision,
        "workflow_run_id": arguments.workflow_run_id,
        "workflow_run_attempt": arguments.workflow_run_attempt,
        "upstream_lock_sha256": hashlib.sha256(lock_bytes).hexdigest(),
        "candidate_tag": f"candidate-{arguments.artifact_version}-{arguments.source_revision}",
        "image": {
            "repository": RUNTIME_REPOSITORY,
            "index_digest": descriptors["index_digest"],
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": descriptors["amd64_digest"]},
                {"os": "linux", "arch": "arm64", "digest": descriptors["arm64_digest"]},
            ],
        },
        "attestations": {
            "buildkit_sbom": True,
            "buildkit_provenance": "max",
            "github_provenance": True,
        },
    }
    if arguments.output.exists() or arguments.output.is_symlink():
        fail("candidate output must not already exist")
    arguments.output.write_bytes(canonical_candidate(document))
    return 0


def command_verify_candidate(arguments: argparse.Namespace) -> int:
    data = load_regular(arguments.candidate, "runtime candidate")
    candidate = validate_candidate(load_json_bytes(data, "runtime candidate"))
    if canonical_candidate(candidate) != data:
        fail("runtime candidate bytes are not canonical")
    expectations = {
        "artifact_version": arguments.artifact_version,
        "release_sequence": arguments.release_sequence,
        "source_revision": arguments.source_revision,
        "workflow_run_id": arguments.workflow_run_id,
        "workflow_run_attempt": arguments.workflow_run_attempt,
        "index_digest": arguments.index_digest,
    }
    actual = {
        "artifact_version": candidate["artifact_version"],
        "release_sequence": candidate["release_sequence"],
        "source_revision": candidate["source_revision"],
        "workflow_run_id": candidate["workflow_run_id"],
        "workflow_run_attempt": candidate["workflow_run_attempt"],
        "index_digest": candidate["image"]["index_digest"],
    }
    for key, expected in expectations.items():
        if expected is not None and actual[key] != expected:
            fail(f"runtime candidate {key} does not match the expected value")
    print(json.dumps({"schema": 1, "status": "verified", **actual}, separators=(",", ":"), sort_keys=True))
    return 0


def command_create(arguments: argparse.Namespace) -> int:
    # Import locally so this standalone release helper shares the detector's
    # strict lock parser without duplicating its npm/timestamp rules.
    import opencodex_upstream  # type: ignore[import-not-found]

    lock_bytes = load_regular(arguments.lock, "upstream lock")
    lock = opencodex_upstream.validate_lock(
        opencodex_upstream.load_json_bytes(lock_bytes, "upstream lock")
    )
    version_tuple(arguments.artifact_version)
    expected_artifact = f"{lock['version']}-r{lock['image_revision']}"
    if arguments.artifact_version != expected_artifact:
        fail("artifact version does not match upstream lock")
    descriptors = load_index(arguments.index, arguments.index_digest)
    inspect_attestations(
        arguments.sbom_amd64,
        arguments.provenance_amd64,
        arguments.sbom_arm64,
        arguments.provenance_arm64,
    )
    document = {
        "schema": 1,
        "artifact_kind": ARTIFACT_KIND,
        "artifact_version": arguments.artifact_version,
        "release_sequence": arguments.release_sequence,
        "channel": "stable",
        "source": {
            "repository": SOURCE_REPOSITORY,
            "revision": arguments.source_revision,
            "upstream_lock_sha256": hashlib.sha256(lock_bytes).hexdigest(),
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
        "image": {
            "repository": RUNTIME_REPOSITORY,
            "index_digest": descriptors["index_digest"],
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": descriptors["amd64_digest"]},
                {"os": "linux", "arch": "arm64", "digest": descriptors["arm64_digest"]},
            ],
        },
        "compatibility": {
            "minimum_relay_version": "0.3.9",
            "minimum_macos": "26.0",
            "minimum_apple_container": "1.3.1",
            "management_api_revision": 1,
            "secret_delivery": "uds-v1",
            "state_format_revision": 1,
        },
        "canary": {
            "source_revision": arguments.source_revision,
            "workflow_run_id": arguments.workflow_run_id,
            "workflow_run_attempt": arguments.workflow_run_attempt,
            "result": "passed",
        },
        "trust_key_id": arguments.trust_key_id,
    }
    if arguments.output.exists() or arguments.output.is_symlink():
        fail("manifest output must not already exist")
    arguments.output.write_bytes(canonical_manifest(document))
    return 0


def command_verify(arguments: argparse.Namespace) -> int:
    data = load_regular(arguments.manifest, "runtime manifest")
    manifest = validate_manifest(load_json_bytes(data, "runtime manifest"))
    if canonical_manifest(manifest) != data:
        fail("runtime manifest bytes are not canonical")
    expectations = {
        "artifact_version": arguments.artifact_version,
        "release_sequence": arguments.release_sequence,
        "source_revision": arguments.source_revision,
        "index_digest": arguments.index_digest,
        "trust_key_id": arguments.trust_key_id,
    }
    actual = {
        "artifact_version": manifest["artifact_version"],
        "release_sequence": manifest["release_sequence"],
        "source_revision": manifest["source"]["revision"],
        "index_digest": manifest["image"]["index_digest"],
        "trust_key_id": manifest["trust_key_id"],
    }
    for key, expected in expectations.items():
        if expected is not None and actual[key] != expected:
            fail(f"runtime manifest {key} does not match the expected value")
    print(json.dumps({"schema": 1, "status": "verified", **actual}, separators=(",", ":"), sort_keys=True))
    return 0


def parser() -> argparse.ArgumentParser:
    def add_attestation_arguments(command: argparse.ArgumentParser) -> None:
        command.add_argument("--sbom-amd64", type=pathlib.Path, required=True)
        command.add_argument("--provenance-amd64", type=pathlib.Path, required=True)
        command.add_argument("--sbom-arm64", type=pathlib.Path, required=True)
        command.add_argument("--provenance-arm64", type=pathlib.Path, required=True)

    result = argparse.ArgumentParser(description=__doc__)
    commands = result.add_subparsers(dest="command", required=True)
    inspect = commands.add_parser("inspect-index")
    inspect.add_argument("--index", type=pathlib.Path, required=True)
    inspect.add_argument("--expected-digest", required=True)
    inspect.set_defaults(handler=command_inspect_index)

    inspect_attestation = commands.add_parser("inspect-attestations")
    add_attestation_arguments(inspect_attestation)
    inspect_attestation.set_defaults(handler=command_inspect_attestations)

    candidate = commands.add_parser("create-candidate")
    candidate.add_argument("--lock", type=pathlib.Path, required=True)
    candidate.add_argument("--index", type=pathlib.Path, required=True)
    candidate.add_argument("--index-digest", required=True)
    candidate.add_argument("--artifact-version", required=True)
    candidate.add_argument("--release-sequence", type=int, required=True)
    candidate.add_argument("--source-revision", required=True)
    candidate.add_argument("--workflow-run-id", required=True)
    candidate.add_argument("--workflow-run-attempt", type=int, required=True)
    candidate.add_argument("--output", type=pathlib.Path, required=True)
    add_attestation_arguments(candidate)
    candidate.set_defaults(handler=command_create_candidate)

    verify_candidate = commands.add_parser("verify-candidate")
    verify_candidate.add_argument("--candidate", type=pathlib.Path, required=True)
    verify_candidate.add_argument("--artifact-version")
    verify_candidate.add_argument("--release-sequence", type=int)
    verify_candidate.add_argument("--source-revision")
    verify_candidate.add_argument("--workflow-run-id")
    verify_candidate.add_argument("--workflow-run-attempt", type=int)
    verify_candidate.add_argument("--index-digest")
    verify_candidate.set_defaults(handler=command_verify_candidate)

    create = commands.add_parser("create")
    create.add_argument("--lock", type=pathlib.Path, required=True)
    create.add_argument("--index", type=pathlib.Path, required=True)
    create.add_argument("--index-digest", required=True)
    create.add_argument("--artifact-version", required=True)
    create.add_argument("--release-sequence", type=int, required=True)
    create.add_argument("--source-revision", required=True)
    create.add_argument("--workflow-run-id", required=True)
    create.add_argument("--workflow-run-attempt", type=int, required=True)
    create.add_argument("--trust-key-id", required=True)
    create.add_argument("--output", type=pathlib.Path, required=True)
    add_attestation_arguments(create)
    create.set_defaults(handler=command_create)

    verify = commands.add_parser("verify")
    verify.add_argument("--manifest", type=pathlib.Path, required=True)
    verify.add_argument("--artifact-version")
    verify.add_argument("--release-sequence", type=int)
    verify.add_argument("--source-revision")
    verify.add_argument("--index-digest")
    verify.add_argument("--trust-key-id")
    verify.set_defaults(handler=command_verify)
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

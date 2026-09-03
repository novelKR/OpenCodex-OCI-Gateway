#!/usr/bin/env python3
"""Verify the locked OpenCodex npm artifact with npm and strict provenance policy.

The cryptographic verification is delegated to the repository-pinned npm CLI's
official ``npm audit signatures`` implementation.  This module then applies the
project policy to the verified DSSE bundles: exact package/tarball identity,
source repository, release workflow, and resolved Git commit.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import math
import os
import pathlib
import re
import ssl
import subprocess
import tarfile
import tempfile
import urllib.request
import urllib.parse
from dataclasses import dataclass
from typing import Any, NoReturn


NPM_PACKAGE = "@bitkyc08/opencodex"
UPSTREAM_REPOSITORY = "lidge-jun/opencodex"
NPM_REGISTRY = "https://registry.npmjs.org"
NPM_PROVENANCE_VERIFIER_VERSION = "11.19.1"
NPM_PROVENANCE_VERIFIER_TARBALL = (
    "https://registry.npmjs.org/npm/-/npm-11.19.1.tgz"
)
NPM_PROVENANCE_VERIFIER_INTEGRITY = (
    "sha512-ztsxKxt/kkIaAs+2i0GU6I+DRmUdrNasxTZKJe9TCdSjKxlhah/4r/"
    "hl5ygMD6XAg1qZ9c2TNomR4qgOydp10g=="
)
NPM_PROVENANCE_NODE_IMAGE = (
    "docker.io/library/node:24.20.0-bookworm-slim@"
    "sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e"
)
NPM_PROVENANCE_SIGSTORE_VERSION = "4.1.1"
NPM_PROVENANCE_IDENTITY_HELPER = "verify_npm_slsa_identity.cjs"
PUBLISH_PREDICATE = "https://github.com/npm/attestation/tree/main/specs/publish/v0.1"
PROVENANCE_PREDICATE = "https://slsa.dev/provenance/v1"
WORKFLOW_BUILD_TYPE = (
    "https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1"
)
EXPECTED_REPOSITORY_URL = "https://github.com/lidge-jun/opencodex"
EXPECTED_WORKFLOW_PATH = ".github/workflows/release.yml"
EXPECTED_SOURCE_REF = "refs/heads/main"
EXPECTED_BUILDER = "https://github.com/actions/runner/github-hosted"
EXPECTED_RESOLVED_URI = (
    "git+https://github.com/lidge-jun/opencodex@refs/heads/main"
)
EXPECTED_CERTIFICATE_IDENTITY = (
    "https://github.com/lidge-jun/opencodex/"
    ".github/workflows/release.yml@refs/heads/main"
)
EXPECTED_CERTIFICATE_ISSUER = "https://token.actions.githubusercontent.com"
MAX_JSON_BYTES = 4 * 1024 * 1024
MAX_NPM_CLI_BYTES = 32 * 1024 * 1024
MAX_NPM_CLI_MEMBERS = 5_000
MAX_NPM_CLI_DECLARED_BYTES = 64 * 1024 * 1024
MAX_DIAGNOSTIC_BYTES = 4_096
LOWER_SHA = re.compile(r"[0-9a-f]{40}")
LOWER_SHA512 = re.compile(r"[0-9a-f]{128}")


@dataclass(frozen=True)
class RegistrySignatureEvidence:
    version: str
    integrity: str
    sha512: str
    signature_count: int
    trusted_key_count: int


@dataclass(frozen=True)
class SLSAIdentityEvidence:
    bundle_sha256: str
    certificate_identity: str
    certificate_issuer: str
    verifier: str


class ContractError(RuntimeError):
    """Verified input did not satisfy the OpenCodex provenance policy."""


class AwaitingNPMProvenance(RuntimeError):
    """The exact npm version exists but its verification evidence is not ready."""


def fail(message: str) -> NoReturn:
    raise ContractError(message)


def strict_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    value: dict[str, Any] = {}
    for key, item in pairs:
        if key in value:
            fail(f"duplicate JSON key: {key}")
        value[key] = item
    return value


def reject_json_constant(value: str) -> NoReturn:
    fail(f"non-finite JSON number is unsupported: {value}")


def finite_json_float(value: str) -> float:
    parsed = float(value)
    if not math.isfinite(parsed):
        fail("non-finite JSON number is unsupported")
    return parsed


def load_json_bytes(data: bytes, description: str) -> Any:
    if not data or len(data) > MAX_JSON_BYTES:
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
    except (json.JSONDecodeError, ContractError) as error:
        if isinstance(error, ContractError):
            raise
        raise ContractError(f"{description} is not valid JSON") from error


def strict_base64(value: Any, description: str) -> bytes:
    if not isinstance(value, str) or not value:
        fail(f"{description} is invalid")
    try:
        decoded = base64.b64decode(value, validate=True)
    except (binascii.Error, ValueError) as error:
        raise ContractError(f"{description} is invalid") from error
    if not decoded or base64.b64encode(decoded).decode("ascii") != value:
        fail(f"{description} is not canonical base64")
    return decoded


def strict_unpadded_base64(value: Any, description: str) -> bytes:
    if not isinstance(value, str) or not value or "=" in value:
        fail(f"{description} is invalid")
    padded = value + "=" * ((4 - len(value) % 4) % 4)
    try:
        decoded = base64.b64decode(padded, validate=True)
    except (binascii.Error, ValueError) as error:
        raise ContractError(f"{description} is invalid") from error
    if not decoded or base64.b64encode(decoded).decode("ascii").rstrip("=") != value:
        fail(f"{description} is not canonical unpadded base64")
    return decoded


def sri_sha512_hex(integrity: str) -> str:
    if not isinstance(integrity, str) or not integrity.startswith("sha512-"):
        fail("npm integrity must use sha512 SRI")
    digest = strict_base64(integrity.removeprefix("sha512-"), "npm integrity")
    if len(digest) != hashlib.sha512().digest_size:
        fail("npm integrity digest is invalid")
    return digest.hex()


def expected_purl(version: str) -> str:
    return f"pkg:npm/%40bitkyc08/opencodex@{version}"


def expected_attestation_url(version: str) -> str:
    return (
        "https://registry.npmjs.org/-/npm/v1/attestations/"
        f"@bitkyc08%2fopencodex@{version}"
    )


def validate_registry_signature_evidence(
    metadata: Any,
    keys_document: Any,
    version: str,
    integrity: str,
    actual_sha512_hex: str,
) -> RegistrySignatureEvidence:
    """Bind npm's registry-signature inputs before trusting audit output.

    npm's JSON audit receipt combines registry signatures and attestations in
    one ``verified`` row. An attestation-only package can therefore appear in
    that row even when the registry key set is empty. Requiring the exact dist
    signature and its matching official registry key makes the pinned npm
    verifier's successful exit an independent registry-signature proof rather
    than an alias for the DSSE provenance proof.
    """
    if not isinstance(metadata, dict) or metadata.get("name") != NPM_PACKAGE or metadata.get("version") != version:
        fail("npm registry-signature package identity is invalid")
    dist = metadata.get("dist")
    if not isinstance(dist, dict) or dist.get("integrity") != integrity:
        fail("npm registry-signature integrity does not match the lock")
    expected_tarball = (
        f"{NPM_REGISTRY}/@bitkyc08/opencodex/-/opencodex-{version}.tgz"
    )
    if dist.get("tarball") != expected_tarball:
        fail("npm registry-signature tarball URL is invalid")
    signatures = dist.get("signatures")
    if signatures is None or signatures == []:
        raise AwaitingNPMProvenance("npm registry signature is not available")
    if not isinstance(signatures, list):
        fail("npm registry signature metadata is invalid")

    if not isinstance(keys_document, dict) or set(keys_document) != {"keys"}:
        fail("npm registry signing-key response is invalid")
    keys = keys_document.get("keys")
    if not isinstance(keys, list) or not keys:
        fail("npm registry has no trusted signing key")
    trusted: set[str] = set()
    for key in keys:
        if not isinstance(key, dict) or set(key) != {
            "expires",
            "keyid",
            "keytype",
            "scheme",
            "key",
        }:
            fail("npm registry signing-key metadata is invalid")
        keyid = key.get("keyid")
        public_key = strict_base64(key.get("key"), "npm registry signing key")
        if (
            not isinstance(keyid, str)
            or not keyid.startswith("SHA256:")
            or len(strict_unpadded_base64(keyid.removeprefix("SHA256:"), "npm registry key id"))
            != hashlib.sha256().digest_size
            or len(public_key) > 8 * 1024
            or key.get("keytype") != "ecdsa-sha2-nistp256"
            or key.get("scheme") != "ecdsa-sha2-nistp256"
            or keyid in trusted
            or key.get("expires") is not None
            and not isinstance(key.get("expires"), str)
        ):
            fail("npm registry signing-key metadata is invalid")
        trusted.add(keyid)

    seen: set[str] = set()
    matched = 0
    for signature in signatures:
        if not isinstance(signature, dict) or set(signature) != {"keyid", "sig"}:
            fail("npm registry signature metadata is invalid")
        keyid = signature.get("keyid")
        if not isinstance(keyid, str) or keyid in seen:
            fail("npm registry signature metadata is invalid")
        strict_base64(signature.get("sig"), "npm registry signature")
        seen.add(keyid)
        if keyid in trusted:
            matched += 1
    if matched == 0:
        fail("npm registry signature has no matching trusted key")
    expected_sha512 = sri_sha512_hex(integrity)
    if actual_sha512_hex != expected_sha512:
        fail("downloaded npm tarball does not match the registry signature input")
    return RegistrySignatureEvidence(
        version=version,
        integrity=integrity,
        sha512=expected_sha512,
        signature_count=len(signatures),
        trusted_key_count=len(trusted),
    )


def validate_attestation_metadata(value: Any, version: str) -> None:
    if not isinstance(value, dict) or set(value) != {"url", "provenance"}:
        fail("npm provenance metadata is invalid")
    if value.get("url") != expected_attestation_url(version):
        fail("npm provenance URL does not match the exact package version")
    provenance = value.get("provenance")
    if not isinstance(provenance, dict) or provenance != {
        "predicateType": PROVENANCE_PREDICATE
    }:
        fail("npm provenance predicate metadata is invalid")


def _validate_transparency_bundle(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != {
        "mediaType",
        "verificationMaterial",
        "dsseEnvelope",
    }:
        fail("npm attestation bundle schema is invalid")
    media_type = value.get("mediaType")
    if media_type not in {
        "application/vnd.dev.sigstore.bundle+json;version=0.2",
        "application/vnd.dev.sigstore.bundle.v0.3+json",
    }:
        fail("npm attestation bundle media type is unsupported")
    material = value.get("verificationMaterial")
    if not isinstance(material, dict):
        fail("npm attestation verification material is invalid")
    entries = material.get("tlogEntries")
    if not isinstance(entries, list) or not entries:
        fail("npm attestation has no transparency-log entry")
    for entry in entries:
        if not isinstance(entry, dict):
            fail("npm transparency-log entry is invalid")
        if not isinstance(entry.get("logIndex"), str) or not entry["logIndex"].isdigit():
            fail("npm transparency-log index is invalid")
        if not isinstance(entry.get("integratedTime"), str) or not entry["integratedTime"].isdigit():
            fail("npm transparency-log integrated time is invalid")
        promise = entry.get("inclusionPromise")
        proof = entry.get("inclusionProof")
        if not isinstance(promise, dict):
            fail("npm transparency-log inclusion promise is missing")
        strict_base64(promise.get("signedEntryTimestamp"), "npm transparency signed entry timestamp")
        if not isinstance(proof, dict):
            fail("npm transparency-log inclusion proof is missing")
        strict_base64(proof.get("rootHash"), "npm transparency root hash")
        hashes = proof.get("hashes")
        if not isinstance(hashes, list) or not hashes:
            fail("npm transparency-log inclusion proof is empty")
        for item in hashes:
            strict_base64(item, "npm transparency proof hash")
        checkpoint = proof.get("checkpoint")
        if not isinstance(checkpoint, dict) or not isinstance(checkpoint.get("envelope"), str) or not checkpoint["envelope"]:
            fail("npm transparency-log checkpoint is invalid")
        strict_base64(entry.get("canonicalizedBody"), "npm transparency canonical body")

    envelope = value.get("dsseEnvelope")
    if not isinstance(envelope, dict) or set(envelope) != {
        "payload",
        "payloadType",
        "signatures",
    }:
        fail("npm DSSE envelope schema is invalid")
    if envelope.get("payloadType") != "application/vnd.in-toto+json":
        fail("npm DSSE payload type is invalid")
    signatures = envelope.get("signatures")
    if not isinstance(signatures, list) or not signatures:
        fail("npm DSSE envelope has no signature")
    for signature in signatures:
        if not isinstance(signature, dict):
            fail("npm DSSE signature is invalid")
        strict_base64(signature.get("sig"), "npm DSSE signature")
    return load_json_bytes(
        strict_base64(envelope.get("payload"), "npm DSSE payload"),
        "npm DSSE statement",
    )


def _validate_subject(statement: dict[str, Any], version: str, sha512_hex: str) -> None:
    subject = statement.get("subject")
    if not isinstance(subject, list) or subject != [
        {"name": expected_purl(version), "digest": {"sha512": sha512_hex}}
    ]:
        fail("npm provenance subject does not match the downloaded tarball")


def _validate_publish_statement(
    statement: dict[str, Any], version: str, sha512_hex: str
) -> None:
    if not isinstance(statement, dict) or set(statement) != {
        "_type",
        "subject",
        "predicateType",
        "predicate",
    }:
        fail("npm publish attestation statement schema is invalid")
    if statement.get("_type") != "https://in-toto.io/Statement/v0.1":
        fail("npm publish attestation statement type is invalid")
    if statement.get("predicateType") != PUBLISH_PREDICATE:
        fail("npm publish attestation predicate type is invalid")
    _validate_subject(statement, version, sha512_hex)
    if statement.get("predicate") != {
        "name": NPM_PACKAGE,
        "version": version,
        "registry": NPM_REGISTRY,
    }:
        fail("npm publish attestation predicate is invalid")


def _validate_provenance_statement(
    statement: dict[str, Any], version: str, revision: str, sha512_hex: str
) -> None:
    if not isinstance(statement, dict) or set(statement) != {
        "_type",
        "subject",
        "predicateType",
        "predicate",
    }:
        fail("npm SLSA provenance statement schema is invalid")
    if statement.get("_type") != "https://in-toto.io/Statement/v1":
        fail("npm SLSA provenance statement type is invalid")
    if statement.get("predicateType") != PROVENANCE_PREDICATE:
        fail("npm SLSA provenance predicate type is invalid")
    _validate_subject(statement, version, sha512_hex)
    predicate = statement.get("predicate")
    if not isinstance(predicate, dict):
        fail("npm SLSA provenance predicate is invalid")
    definition = predicate.get("buildDefinition")
    details = predicate.get("runDetails")
    if not isinstance(definition, dict) or not isinstance(details, dict):
        fail("npm SLSA provenance build definition is invalid")
    if definition.get("buildType") != WORKFLOW_BUILD_TYPE:
        fail("npm provenance builder workflow type is not approved")
    external = definition.get("externalParameters")
    if not isinstance(external, dict) or external.get("workflow") != {
        "ref": EXPECTED_SOURCE_REF,
        "repository": EXPECTED_REPOSITORY_URL,
        "path": EXPECTED_WORKFLOW_PATH,
    }:
        fail("npm provenance source repository or workflow is not approved")
    dependencies = definition.get("resolvedDependencies")
    if dependencies != [
        {"uri": EXPECTED_RESOLVED_URI, "digest": {"gitCommit": revision}}
    ]:
        fail("npm provenance resolved source commit does not match the release tag")
    builder = details.get("builder")
    if not isinstance(builder, dict) or builder.get("id") != EXPECTED_BUILDER:
        fail("npm provenance builder is not GitHub-hosted")
    metadata = details.get("metadata")
    invocation = metadata.get("invocationId") if isinstance(metadata, dict) else None
    expected_prefix = f"https://github.com/{UPSTREAM_REPOSITORY}/actions/runs/"
    if not isinstance(invocation, str) or not invocation.startswith(expected_prefix):
        fail("npm provenance invocation identity is invalid")


def canonical_bundle_bytes(bundle: Any) -> bytes:
    if not isinstance(bundle, dict):
        fail("npm SLSA identity bundle is invalid")
    try:
        encoded = json.dumps(
            bundle,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")
    except (TypeError, ValueError) as error:
        raise ContractError("npm SLSA identity bundle is invalid") from error
    if not encoded or len(encoded) > MAX_JSON_BYTES:
        fail("npm SLSA identity bundle size is invalid")
    return encoded


def select_slsa_identity_bundle(document: Any, version: str) -> dict[str, Any]:
    """Select only the exact package's SLSA bundle for identity verification.

    This is intentionally a narrow pre-validation step. The complete receipt,
    statement, subject, and registry policy is checked again by
    ``validate_audit_receipt`` after the selected bundle's Fulcio identity has
    been cryptographically verified.
    """
    if not isinstance(document, dict) or set(document) != {
        "invalid",
        "missing",
        "verified",
    }:
        fail("npm audit-signatures receipt schema is invalid")
    verified = document.get("verified")
    if not isinstance(verified, list):
        fail("npm audit-signatures verified list is invalid")
    matches = [
        item
        for item in verified
        if isinstance(item, dict)
        and item.get("name") == NPM_PACKAGE
        and item.get("version") == version
    ]
    if len(matches) != 1 or len(verified) != 1:
        fail("npm audit-signatures verified an unexpected package identity")
    rows = matches[0].get("attestationBundles")
    if not isinstance(rows, list):
        fail("npm attestation bundle list is invalid")
    matches = [
        row
        for row in rows
        if isinstance(row, dict) and row.get("predicateType") == PROVENANCE_PREDICATE
    ]
    if len(matches) != 1:
        fail("npm SLSA provenance identity bundle is not unique")
    row = matches[0]
    if set(row) != {"predicateType", "bundle", "signedAccessSignatureUrl"}:
        fail("npm attestation bundle row schema is invalid")
    bundle = row.get("bundle")
    canonical_bundle_bytes(bundle)
    return bundle


def validate_slsa_identity_evidence(
    evidence: Any,
    bundle: dict[str, Any],
) -> None:
    expected_digest = hashlib.sha256(canonical_bundle_bytes(bundle)).hexdigest()
    if (
        not isinstance(evidence, SLSAIdentityEvidence)
        or evidence.bundle_sha256 != expected_digest
        or evidence.certificate_identity != EXPECTED_CERTIFICATE_IDENTITY
        or evidence.certificate_issuer != EXPECTED_CERTIFICATE_ISSUER
        or evidence.verifier
        != f"sigstore@{NPM_PROVENANCE_SIGSTORE_VERSION}"
    ):
        fail("npm SLSA provenance certificate identity was not verified")


def validate_audit_receipt(
    document: Any,
    version: str,
    revision: str,
    integrity: str,
    actual_sha512_hex: str,
    registry_evidence: RegistrySignatureEvidence,
    slsa_identity_evidence: SLSAIdentityEvidence,
) -> dict[str, Any]:
    if not LOWER_SHA.fullmatch(revision):
        fail("npm provenance revision is invalid")
    if not LOWER_SHA512.fullmatch(actual_sha512_hex):
        fail("downloaded npm tarball digest is invalid")
    expected_sha512 = sri_sha512_hex(integrity)
    if actual_sha512_hex != expected_sha512:
        fail("downloaded npm tarball does not match the locked SRI")
    if (
        not isinstance(registry_evidence, RegistrySignatureEvidence)
        or registry_evidence.version != version
        or registry_evidence.integrity != integrity
        or registry_evidence.sha512 != expected_sha512
        or registry_evidence.signature_count <= 0
        or registry_evidence.trusted_key_count <= 0
    ):
        fail("npm registry signature was not independently verified")
    if not isinstance(document, dict) or set(document) != {
        "invalid",
        "missing",
        "verified",
    }:
        fail("npm audit-signatures receipt schema is invalid")
    invalid = document.get("invalid")
    missing = document.get("missing")
    verified = document.get("verified")
    if not isinstance(invalid, list) or not isinstance(missing, list) or not isinstance(verified, list):
        fail("npm audit-signatures result lists are invalid")
    if invalid:
        fail("npm audit-signatures reported invalid signature or provenance evidence")
    if missing:
        raise AwaitingNPMProvenance(
            "npm registry signature or provenance evidence is not available"
        )
    matches = [
        item
        for item in verified
        if isinstance(item, dict)
        and item.get("name") == NPM_PACKAGE
        and item.get("version") == version
    ]
    if not matches and not verified:
        raise AwaitingNPMProvenance(
            "npm audit-signatures did not verify the exact OpenCodex package"
        )
    if len(matches) != 1 or len(verified) != 1:
        fail("npm audit-signatures verified an unexpected package identity")
    item = matches[0]
    if set(item) != {
        "name",
        "version",
        "location",
        "registry",
        "attestations",
        "attestationBundles",
    }:
        fail("npm verified-package receipt schema is invalid")
    if item.get("location") != "node_modules/@bitkyc08/opencodex":
        fail("npm verified-package location is invalid")
    if item.get("registry") != NPM_REGISTRY + "/":
        fail("npm verified-package registry is not approved")
    validate_attestation_metadata(item.get("attestations"), version)
    bundles = item.get("attestationBundles")
    if not isinstance(bundles, list):
        fail("npm attestation bundle list is invalid")
    if len(bundles) < 2:
        raise AwaitingNPMProvenance(
            "npm publish and SLSA provenance bundles are not both available"
        )
    if len(bundles) > 2:
        fail("npm attestation bundle count is invalid")
    by_type: dict[str, dict[str, Any]] = {}
    bundles_by_type: dict[str, dict[str, Any]] = {}
    for row in bundles:
        if not isinstance(row, dict) or set(row) != {
            "predicateType",
            "bundle",
            "signedAccessSignatureUrl",
        }:
            fail("npm attestation bundle row schema is invalid")
        predicate_type = row.get("predicateType")
        if predicate_type not in {PUBLISH_PREDICATE, PROVENANCE_PREDICATE}:
            fail("npm attestation bundle predicate type is unsupported")
        if predicate_type in by_type:
            fail("npm attestation contains duplicate predicate bundles")
        bundle = row.get("bundle")
        by_type[predicate_type] = _validate_transparency_bundle(bundle)
        bundles_by_type[predicate_type] = bundle
    if set(by_type) != {PUBLISH_PREDICATE, PROVENANCE_PREDICATE}:
        raise AwaitingNPMProvenance(
            "npm publish and SLSA provenance bundles are not both available"
        )
    _validate_publish_statement(by_type[PUBLISH_PREDICATE], version, expected_sha512)
    _validate_provenance_statement(
        by_type[PROVENANCE_PREDICATE], version, revision, expected_sha512
    )
    validate_slsa_identity_evidence(
        slsa_identity_evidence,
        bundles_by_type[PROVENANCE_PREDICATE],
    )
    return {
        "schema": 1,
        "status": "verified",
        "package": NPM_PACKAGE,
        "version": version,
        "revision": revision,
        "sha512": expected_sha512,
        "registry_signature": True,
        "slsa_provenance": True,
        "slsa_certificate_identity": EXPECTED_CERTIFICATE_IDENTITY,
        "slsa_certificate_issuer": EXPECTED_CERTIFICATE_ISSUER,
        "transparency_log": True,
        "verifier": f"npm@{NPM_PROVENANCE_VERIFIER_VERSION}",
    }


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        return None


def _download_verifier_tarball() -> bytes:
    opener = urllib.request.build_opener(
        _NoRedirect(),
        urllib.request.HTTPSHandler(context=ssl.create_default_context()),
    )
    request = urllib.request.Request(
        NPM_PROVENANCE_VERIFIER_TARBALL,
        headers={
            "Accept": "application/octet-stream",
            "User-Agent": "OpenCodex-OCI-Gateway-npm-provenance/1",
        },
    )
    with opener.open(request, timeout=30) as response:
        if response.geturl() != NPM_PROVENANCE_VERIFIER_TARBALL:
            fail("npm verifier tarball redirected unexpectedly")
        data = response.read(MAX_NPM_CLI_BYTES + 1)
    if not data or len(data) > MAX_NPM_CLI_BYTES:
        fail("npm verifier tarball size is invalid")
    actual = "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode("ascii")
    if actual != NPM_PROVENANCE_VERIFIER_INTEGRITY:
        fail("npm verifier tarball does not match the repository-pinned SRI")
    return data


def _download_registry_json(url: str, description: str) -> Any:
    opener = urllib.request.build_opener(
        _NoRedirect(),
        urllib.request.HTTPSHandler(context=ssl.create_default_context()),
    )
    request = urllib.request.Request(
        url,
        headers={
            "Accept": "application/json",
            "User-Agent": "OpenCodex-OCI-Gateway-npm-provenance/1",
        },
    )
    with opener.open(request, timeout=30) as response:
        if response.geturl() != url:
            fail(f"{description} redirected unexpectedly")
        data = response.read(MAX_JSON_BYTES + 1)
    if not data or len(data) > MAX_JSON_BYTES:
        fail(f"{description} size is invalid")
    return load_json_bytes(data, description)


def _extract_verifier(data: bytes, destination: pathlib.Path) -> pathlib.Path:
    archive_path = destination / "npm-verifier.tgz"
    archive_path.write_bytes(data)
    output = destination / "verifier"
    output.mkdir(mode=0o700)
    with tarfile.open(archive_path, mode="r:gz") as archive:
        declared = 0
        members = archive.getmembers()
        if not members or len(members) > MAX_NPM_CLI_MEMBERS:
            fail("npm verifier tarball member count is invalid")
        for member in members:
            declared += member.size
            if member.size < 0 or declared > MAX_NPM_CLI_DECLARED_BYTES:
                fail("npm verifier tarball declared size is invalid")
            parts = pathlib.PurePosixPath(member.name).parts
            if not parts or parts[0] != "package" or any(
                part in {"", ".", ".."} for part in parts
            ):
                fail("npm verifier tarball path is unsafe")
            relative = pathlib.Path(*parts[1:])
            target = output / relative
            if member.isdir():
                target.mkdir(mode=0o700, parents=True, exist_ok=True)
                continue
            if not member.isfile():
                fail("npm verifier tarball contains a non-regular member")
            target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                fail("npm verifier tarball member cannot be read")
            payload = source.read(member.size + 1)
            if len(payload) != member.size:
                fail("npm verifier tarball member size is inconsistent")
            target.write_bytes(payload)
            target.chmod(0o500 if member.mode & 0o111 else 0o400)
    archive_path.unlink()
    package = load_json_bytes((output / "package.json").read_bytes(), "npm verifier package")
    if (
        not isinstance(package, dict)
        or package.get("name") != "npm"
        or package.get("version") != NPM_PROVENANCE_VERIFIER_VERSION
    ):
        fail("npm verifier package identity is invalid")
    if not (output / "bin" / "npm-cli.js").is_file():
        fail("npm verifier entrypoint is missing")
    return output


def _bounded_diagnostic(value: str) -> str:
    encoded = value.encode("utf-8", errors="replace")[:MAX_DIAGNOSTIC_BYTES]
    return encoded.decode("utf-8", errors="replace").strip()


def _run_container(arguments: list[str], timeout: int) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            arguments,
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ContractError("unable to run the pinned npm provenance verifier") from error


def _identity_helper_path() -> pathlib.Path:
    path = pathlib.Path(__file__).resolve().with_name(NPM_PROVENANCE_IDENTITY_HELPER)
    if (
        not path.is_file()
        or path.is_symlink()
        or path.stat().st_size <= 0
        or path.stat().st_size > 64 * 1024
    ):
        fail("npm SLSA identity verifier helper is unavailable")
    return path


def _run_slsa_identity_verifier(
    verifier: pathlib.Path,
    root: pathlib.Path,
    bundle: dict[str, Any],
) -> SLSAIdentityEvidence:
    bundle_bytes = canonical_bundle_bytes(bundle)
    identity_input = root / "slsa-identity-bundle.json"
    identity_input.write_bytes(bundle_bytes)
    identity_input.chmod(0o400)
    helper = _identity_helper_path()
    command = [
        "docker",
        "run",
        "--rm",
        "--init",
        f"--user={os.getuid()}:{os.getgid()}",
        "--read-only",
        "--cap-drop=ALL",
        "--security-opt=no-new-privileges",
        "--network=bridge",
        "--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=128m,mode=1777",
        "--mount",
        f"type=bind,source={verifier},target=/verifier,readonly",
        "--mount",
        f"type=bind,source={identity_input},target=/input/slsa-bundle.json,readonly",
        "--mount",
        f"type=bind,source={helper},target=/identity-helper.cjs,readonly",
        "--env=HOME=/tmp",
        NPM_PROVENANCE_NODE_IMAGE,
        "node",
        "/identity-helper.cjs",
        "/input/slsa-bundle.json",
    ]
    result = _run_container(command, 180)
    if len(result.stdout.encode("utf-8")) > MAX_JSON_BYTES:
        fail("npm SLSA identity verifier receipt exceeds the size limit")
    if result.returncode != 0:
        diagnostic = _bounded_diagnostic(result.stderr)
        fail(f"npm SLSA provenance certificate identity verification failed: {diagnostic}")
    receipt = load_json_bytes(
        result.stdout.encode("utf-8"),
        "npm SLSA identity verifier receipt",
    )
    if not isinstance(receipt, dict) or set(receipt) != {
        "schema",
        "status",
        "bundle_sha256",
        "certificate_identity",
        "certificate_issuer",
        "verifier",
    }:
        fail("npm SLSA identity verifier receipt schema is invalid")
    evidence = SLSAIdentityEvidence(
        bundle_sha256=receipt.get("bundle_sha256"),
        certificate_identity=receipt.get("certificate_identity"),
        certificate_issuer=receipt.get("certificate_issuer"),
        verifier=receipt.get("verifier"),
    )
    if receipt.get("schema") != 1 or receipt.get("status") != "verified":
        fail("npm SLSA identity verifier did not report success")
    validate_slsa_identity_evidence(evidence, bundle)
    return evidence


def verify_live(
    version: str,
    revision: str,
    integrity: str,
    tarball_bytes: bytes,
) -> dict[str, Any]:
    if not re.fullmatch(
        r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)",
        version,
    ):
        fail("npm provenance version is invalid")
    if not LOWER_SHA.fullmatch(revision):
        fail("npm provenance revision is invalid")
    actual_sha512 = hashlib.sha512(tarball_bytes).hexdigest()
    if actual_sha512 != sri_sha512_hex(integrity):
        fail("downloaded npm tarball does not match the locked SRI")

    encoded_package = urllib.parse.quote(NPM_PACKAGE, safe="")
    encoded_version = urllib.parse.quote(version, safe="")
    metadata = _download_registry_json(
        f"{NPM_REGISTRY}/{encoded_package}/{encoded_version}",
        "npm exact-version signature metadata",
    )
    keys = _download_registry_json(
        f"{NPM_REGISTRY}/-/npm/v1/keys",
        "npm registry signing keys",
    )
    registry_evidence = validate_registry_signature_evidence(
        metadata, keys, version, integrity, actual_sha512
    )

    with tempfile.TemporaryDirectory(prefix="opencodex-npm-provenance-") as name:
        root = pathlib.Path(name)
        verifier = _extract_verifier(_download_verifier_tarball(), root)
        audit = root / "audit"
        package = audit / "node_modules" / "@bitkyc08" / "opencodex"
        package.mkdir(mode=0o700, parents=True)
        (audit / "package.json").write_text(
            json.dumps(
                {
                    "name": "opencodex-provenance-audit",
                    "version": "1.0.0",
                    "private": True,
                    "dependencies": {NPM_PACKAGE: version},
                },
                separators=(",", ":"),
            )
            + "\n",
            encoding="utf-8",
        )
        (package / "package.json").write_text(
            json.dumps({"name": NPM_PACKAGE, "version": version}, separators=(",", ":"))
            + "\n",
            encoding="utf-8",
        )
        common = [
            "docker",
            "run",
            "--rm",
            "--init",
            f"--user={os.getuid()}:{os.getgid()}",
            "--read-only",
            "--cap-drop=ALL",
            "--security-opt=no-new-privileges",
            "--network=bridge",
            "--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=128m,mode=1777",
            "--mount",
            f"type=bind,source={verifier},target=/verifier,readonly",
            "--mount",
            f"type=bind,source={audit},target=/audit,readonly",
            "--workdir=/audit",
            "--env=HOME=/tmp",
            "--env=npm_config_cache=/tmp/npm-cache",
            "--env=npm_config_registry=https://registry.npmjs.org/",
            "--env=npm_config_update_notifier=false",
            NPM_PROVENANCE_NODE_IMAGE,
            "node",
            "/verifier/bin/npm-cli.js",
        ]
        version_result = _run_container(common + ["--version"], 60)
        if (
            version_result.returncode != 0
            or version_result.stdout.strip() != NPM_PROVENANCE_VERIFIER_VERSION
        ):
            fail("pinned npm provenance verifier version assertion failed")
        result = _run_container(
            common + ["audit", "signatures", "--json", "--include-attestations"],
            180,
        )
        if len(result.stdout.encode("utf-8")) > MAX_JSON_BYTES:
            fail("npm audit-signatures receipt exceeds the size limit")
        try:
            receipt = load_json_bytes(
                result.stdout.encode("utf-8"),
                "npm audit-signatures receipt",
            )
            if result.returncode != 0:
                diagnostic = _bounded_diagnostic(result.stderr)
                fail(f"npm audit-signatures failed: {diagnostic}")
            slsa_bundle = select_slsa_identity_bundle(receipt, version)
            identity_evidence = _run_slsa_identity_verifier(
                verifier,
                root,
                slsa_bundle,
            )
            verified = validate_audit_receipt(
                receipt,
                version,
                revision,
                integrity,
                actual_sha512,
                registry_evidence,
                identity_evidence,
            )
        except AwaitingNPMProvenance:
            raise
        except ContractError as error:
            diagnostic = _bounded_diagnostic(result.stderr)
            if diagnostic:
                raise ContractError(f"{error}; npm verifier: {diagnostic}") from error
            raise
        return verified

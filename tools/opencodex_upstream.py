#!/usr/bin/env python3
"""Verify and update the immutable external OpenCodex release lock.

The detector treats GitHub and npm as mutually corroborating, untrusted inputs.
It never changes the upstream repository and it never writes the tracked lock
unless the separate ``apply`` command is invoked with a previously verified
candidate document.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import datetime
import hashlib
import io
import json
import math
import os
import pathlib
import re
import ssl
import sys
import tarfile
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, NoReturn


UPSTREAM_REPOSITORY = "lidge-jun/opencodex"
NPM_PACKAGE = "@bitkyc08/opencodex"
GITHUB_API = "https://api.github.com"
NPM_REGISTRY = "https://registry.npmjs.org"
MAX_RELEASE_PAGES = 5
RELEASES_PER_PAGE = 100
MAX_JSON_BYTES = 4 * 1024 * 1024
MAX_TARBALL_BYTES = 64 * 1024 * 1024
MAX_TAR_MEMBERS = 20_000
MAX_TAR_DECLARED_BYTES = 1024 * 1024 * 1024
MAX_PACKAGE_JSON_BYTES = 1024 * 1024
UINT32_MAX = (1 << 32) - 1
UINT64_MAX = (1 << 64) - 1
INT64_MAX = (1 << 63) - 1
STRICT_VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
LOWER_SHA = re.compile(r"[0-9a-f]{40}")
LOCK_KEYS = {"schema", "image_revision", "repository", "release", "version", "revision", "npm"}
RELEASE_KEYS = {"id", "tag", "published_at"}
NPM_KEYS = {"package", "version", "integrity", "tarball"}


class ContractError(RuntimeError):
    """An untrusted input violated the pinned supply-chain contract."""


class AwaitingNPM(RuntimeError):
    """A valid release exists but npm has not published the same version yet."""


def provenance_contract():
    try:
        import opencodex_npm_provenance  # type: ignore[import-not-found]
    except ModuleNotFoundError:
        from tools import opencodex_npm_provenance  # type: ignore[no-redef]
    return opencodex_npm_provenance


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


def load_json_bytes(data: bytes, description: str, maximum: int = MAX_JSON_BYTES) -> Any:
    if not data or len(data) > maximum:
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


def load_json_file(path: pathlib.Path, description: str) -> Any:
    if not path.is_file() or path.is_symlink():
        fail(f"{description} must be a regular file")
    return load_json_bytes(path.read_bytes(), description)


def version_tuple(value: str) -> tuple[int, int, int]:
    match = STRICT_VERSION.fullmatch(value) if isinstance(value, str) else None
    if not match:
        fail("version must be strict SemVer without prerelease or build metadata")
    parts = match.groups()
    if any(len(part) > 10 for part in parts):
        fail("version components exceed the UInt32 consumer contract")
    parsed = tuple(int(part) for part in parts)
    if any(part > UINT32_MAX for part in parsed):
        fail("version components exceed the UInt32 consumer contract")
    return parsed  # type: ignore[return-value]


def validate_timestamp(value: Any) -> str:
    if not isinstance(value, str) or not value.endswith("Z"):
        fail("release published_at must be an RFC3339 UTC timestamp")
    try:
        parsed = datetime.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise ContractError("release published_at is invalid") from error
    if parsed.tzinfo != datetime.timezone.utc or parsed.isoformat().replace("+00:00", "Z") != value:
        fail("release published_at is not canonical RFC3339 UTC")
    return value


def validate_sri(value: Any) -> str:
    if not isinstance(value, str) or not value.startswith("sha512-"):
        fail("npm integrity must use sha512 SRI")
    encoded = value.removeprefix("sha512-")
    try:
        digest = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as error:
        raise ContractError("npm integrity is not strict base64") from error
    if len(digest) != hashlib.sha512().digest_size or base64.b64encode(digest).decode("ascii") != encoded:
        fail("npm integrity digest is invalid")
    return value


def expected_tarball_path(version: str) -> str:
    return f"/@bitkyc08/opencodex/-/opencodex-{version}.tgz"


def validate_tarball_url(value: Any, version: str, allowed_origin: str = NPM_REGISTRY) -> str:
    if not isinstance(value, str) or len(value) > 2048:
        fail("npm tarball URL is invalid")
    parsed = urllib.parse.urlsplit(value)
    origin = urllib.parse.urlsplit(allowed_origin)
    if (
        parsed.scheme != origin.scheme
        or parsed.netloc != origin.netloc
        or parsed.path != expected_tarball_path(version)
        or parsed.query
        or parsed.fragment
        or parsed.username is not None
        or parsed.password is not None
    ):
        fail("npm tarball URL is not the canonical package URL")
    return value


def validate_lock(document: Any, allowed_tarball_origin: str = NPM_REGISTRY) -> dict[str, Any]:
    if not isinstance(document, dict) or set(document) != LOCK_KEYS:
        fail("upstream lock has unsupported fields")
    if type(document.get("schema")) is not int or document.get("schema") != 1:
        fail("upstream lock schema is unsupported")
    image_revision = document.get("image_revision")
    if (
        not isinstance(image_revision, int)
        or isinstance(image_revision, bool)
        or image_revision < 1
        or image_revision > UINT64_MAX
    ):
        fail("image_revision must be a positive integer")
    if document.get("repository") != UPSTREAM_REPOSITORY:
        fail("upstream repository is not approved")
    version = document.get("version")
    version_tuple(version)
    revision = document.get("revision")
    if not isinstance(revision, str) or not LOWER_SHA.fullmatch(revision):
        fail("upstream revision must be a full lowercase commit ID")

    release = document.get("release")
    if not isinstance(release, dict) or set(release) != RELEASE_KEYS:
        fail("upstream release identity has unsupported fields")
    release_id = release.get("id")
    if (
        not isinstance(release_id, int)
        or isinstance(release_id, bool)
        or release_id <= 0
        or release_id > INT64_MAX
    ):
        fail("upstream release ID must be a positive Int64 integer")
    if release.get("tag") != f"v{version}":
        fail("upstream release tag does not match version")
    validate_timestamp(release.get("published_at"))

    npm = document.get("npm")
    if not isinstance(npm, dict) or set(npm) != NPM_KEYS:
        fail("npm identity has unsupported fields")
    if npm.get("package") != NPM_PACKAGE or npm.get("version") != version:
        fail("npm package identity does not match the release")
    validate_sri(npm.get("integrity"))
    validate_tarball_url(npm.get("tarball"), version, allowed_tarball_origin)
    return document


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[no-untyped-def]
        return None


class NetworkClient:
    def __init__(self, github_api: str, npm_registry: str, github_token: str = "") -> None:
        self.github_api = github_api.rstrip("/")
        self.npm_registry = npm_registry.rstrip("/")
        self.github_token = github_token
        self.opener = urllib.request.build_opener(_NoRedirect())
        self.context = ssl.create_default_context()

    def _read(self, url: str, maximum: int, headers: dict[str, str] | None = None) -> bytes:
        request_headers = {
            "Accept": "application/vnd.github+json",
            "User-Agent": "OpenCodex-OCI-Gateway-upstream-detector/1",
        }
        request_headers.update(headers or {})
        request = urllib.request.Request(url, headers=request_headers)
        try:
            response = self.opener.open(request, timeout=30, context=self.context)  # type: ignore[call-arg]
        except TypeError:
            # OpenerDirector.open does not expose the context argument on older Python.
            response = self.opener.open(request, timeout=30)
        with response:
            final_url = response.geturl()
            if final_url != url:
                fail("network response redirected unexpectedly")
            content_length = response.headers.get("Content-Length")
            if content_length is not None:
                try:
                    declared = int(content_length)
                except ValueError as error:
                    raise ContractError("network response has invalid Content-Length") from error
                if declared < 0 or declared > maximum:
                    fail("network response exceeds size limit")
            data = response.read(maximum + 1)
            if len(data) > maximum:
                fail("network response exceeds size limit")
            return data

    def github_json(self, path: str) -> Any:
        headers = {"X-GitHub-Api-Version": "2022-11-28"}
        if self.github_token:
            headers["Authorization"] = f"Bearer {self.github_token}"
        return load_json_bytes(self._read(self.github_api + path, MAX_JSON_BYTES, headers), "GitHub response")

    def npm_version_json(self, package: str, version: str) -> Any:
        encoded = urllib.parse.quote(package, safe="")
        encoded_version = urllib.parse.quote(version, safe="")
        try:
            data = self._read(
                f"{self.npm_registry}/{encoded}/{encoded_version}",
                MAX_JSON_BYTES,
                {"Accept": "application/json"},
            )
        except urllib.error.HTTPError as error:
            if error.code == 404:
                raise AwaitingNPM("npm version metadata is not available") from error
            raise
        return load_json_bytes(data, "npm version metadata")

    def tarball_bytes(self, url: str) -> bytes:
        try:
            return self._read(
                url,
                MAX_TARBALL_BYTES,
                {"Accept": "application/octet-stream"},
            )
        except urllib.error.HTTPError as error:
            if error.code == 404:
                raise AwaitingNPM("npm tarball is not available") from error
            raise

    def verify_npm_provenance(
        self,
        version: str,
        revision: str,
        integrity: str,
        tarball: bytes,
    ) -> dict[str, Any]:
        if self.npm_registry != NPM_REGISTRY:
            fail("npm provenance verification requires the canonical public registry")
        opencodex_npm_provenance = provenance_contract()

        try:
            return opencodex_npm_provenance.verify_live(
                version,
                revision,
                integrity,
                tarball,
            )
        except opencodex_npm_provenance.AwaitingNPMProvenance as error:
            raise AwaitingNPM(str(error)) from error
        except opencodex_npm_provenance.ContractError as error:
            raise ContractError(str(error)) from error


def release_rows(client: Any) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for page in range(1, MAX_RELEASE_PAGES + 1):
        value = client.github_json(
            f"/repos/{UPSTREAM_REPOSITORY}/releases?per_page={RELEASES_PER_PAGE}&page={page}"
        )
        if not isinstance(value, list) or len(value) > RELEASES_PER_PAGE:
            fail("GitHub release page is invalid")
        for entry in value:
            if not isinstance(entry, dict):
                fail("GitHub release entry is invalid")
            rows.append(entry)
        if len(value) < RELEASES_PER_PAGE:
            return rows
    fail(f"GitHub release scan exceeded {MAX_RELEASE_PAGES} pages")


def choose_release(rows: list[dict[str, Any]]) -> dict[str, Any]:
    selected: tuple[tuple[int, int, int], dict[str, Any]] | None = None
    seen: set[tuple[int, int, int]] = set()
    for row in rows:
        if row.get("draft") is True or row.get("prerelease") is True:
            continue
        tag = row.get("tag_name")
        if not isinstance(tag, str) or not tag.startswith("v") or not STRICT_VERSION.fullmatch(tag[1:]):
            continue
        parsed = version_tuple(tag[1:])
        if parsed in seen:
            fail("GitHub has duplicate strict release versions")
        seen.add(parsed)
        if (
            not isinstance(row.get("id"), int)
            or isinstance(row.get("id"), bool)
            or row["id"] <= 0
            or row["id"] > INT64_MAX
        ):
            fail("GitHub release entry has an invalid ID")
        if selected is None or parsed > selected[0]:
            selected = parsed, row
    if selected is None:
        fail("GitHub has no stable strict vSEMVER release")
    return selected[1]


def verify_tarball(data: bytes, integrity: str, version: str) -> None:
    if not data or len(data) > MAX_TARBALL_BYTES:
        fail("npm tarball size is invalid")
    actual = "sha512-" + base64.b64encode(hashlib.sha512(data).digest()).decode("ascii")
    if actual != integrity:
        fail("npm tarball does not match SRI metadata")
    try:
        archive = tarfile.open(fileobj=io.BytesIO(data), mode="r:gz")
    except (tarfile.TarError, OSError) as error:
        raise ContractError("npm tarball is invalid") from error
    package_members: list[tarfile.TarInfo] = []
    declared = 0
    count = 0
    try:
        for member in archive:
            count += 1
            if count > MAX_TAR_MEMBERS:
                fail("npm tarball contains too many entries")
            if member.size < 0:
                fail("npm tarball contains a negative entry size")
            declared += member.size
            if declared > MAX_TAR_DECLARED_BYTES:
                fail("npm tarball declared size exceeds limit")
            if member.name == "package/package.json":
                package_members.append(member)
        if len(package_members) != 1 or not package_members[0].isfile():
            fail("npm tarball must contain one regular package/package.json")
        member = package_members[0]
        if member.size == 0 or member.size > MAX_PACKAGE_JSON_BYTES:
            fail("npm package.json size is invalid")
        extracted = archive.extractfile(member)
        if extracted is None:
            fail("npm package.json cannot be read")
        package = load_json_bytes(extracted.read(MAX_PACKAGE_JSON_BYTES + 1), "npm package.json", MAX_PACKAGE_JSON_BYTES)
    finally:
        archive.close()
    if not isinstance(package, dict) or package.get("name") != NPM_PACKAGE or package.get("version") != version:
        fail("npm tarball package identity does not match the release")


def verify_npm_artifact(
    client: Any,
    version: str,
    revision: str,
    npm_origin: str = NPM_REGISTRY,
) -> dict[str, str]:
    npm_version = client.npm_version_json(NPM_PACKAGE, version)
    if (
        not isinstance(npm_version, dict)
        or npm_version.get("name") != NPM_PACKAGE
        or npm_version.get("version") != version
    ):
        fail("npm version metadata identity is invalid")
    git_head = npm_version.get("gitHead")
    if git_head is None:
        raise AwaitingNPM("npm gitHead metadata is not available")
    if git_head != revision:
        fail("npm gitHead does not match the direct release tag commit")
    dist = npm_version.get("dist")
    if not isinstance(dist, dict):
        fail("npm version metadata has no dist identity")
    integrity = validate_sri(dist.get("integrity"))
    tarball = validate_tarball_url(dist.get("tarball"), version, npm_origin)
    attestations = dist.get("attestations")
    if attestations is None:
        raise AwaitingNPM("npm provenance metadata is not available")
    opencodex_npm_provenance = provenance_contract()

    try:
        opencodex_npm_provenance.validate_attestation_metadata(attestations, version)
    except opencodex_npm_provenance.ContractError as error:
        raise ContractError(str(error)) from error
    tarball_bytes = client.tarball_bytes(tarball)
    verify_tarball(tarball_bytes, integrity, version)
    client.verify_npm_provenance(version, revision, integrity, tarball_bytes)
    return {
        "package": NPM_PACKAGE,
        "version": version,
        "integrity": integrity,
        "tarball": tarball,
    }


def decode_repository_package(value: Any, revision: str, version: str) -> None:
    if not isinstance(value, dict) or value.get("encoding") != "base64":
        fail("upstream package.json response is invalid")
    content = value.get("content")
    size = value.get("size")
    if not isinstance(content, str) or not isinstance(size, int) or isinstance(size, bool):
        fail("upstream package.json metadata is invalid")
    if size <= 0 or size > MAX_PACKAGE_JSON_BYTES:
        fail("upstream package.json size is invalid")
    encoded = content.replace("\r", "").replace("\n", "")
    alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
    if not encoded or any(character not in alphabet for character in encoded):
        fail("upstream package.json content is invalid")
    try:
        decoded = base64.b64decode(encoded, validate=True)
    except (binascii.Error, ValueError) as error:
        raise ContractError("upstream package.json content is invalid") from error
    if base64.b64encode(decoded).decode("ascii") != encoded:
        fail("upstream package.json content is not canonical base64")
    if len(decoded) != size:
        fail("upstream package.json size does not match content")
    package = load_json_bytes(decoded, "upstream package.json", MAX_PACKAGE_JSON_BYTES)
    if not isinstance(package, dict) or package.get("name") != NPM_PACKAGE or package.get("version") != version:
        fail("upstream package.json identity does not match the release")
    response_sha = value.get("sha")
    if not isinstance(response_sha, str) or not re.fullmatch(r"[0-9a-f]{40}", response_sha):
        fail("upstream package.json blob identity is invalid")
    if not LOWER_SHA.fullmatch(revision):
        fail("upstream package.json revision is invalid")


def detect(client: Any, current: dict[str, Any], npm_origin: str = NPM_REGISTRY) -> tuple[str, dict[str, Any] | None]:
    validate_lock(current, npm_origin)
    listed = choose_release(release_rows(client))
    release_id = listed["id"]
    release = client.github_json(f"/repos/{UPSTREAM_REPOSITORY}/releases/{release_id}")
    if not isinstance(release, dict):
        fail("selected GitHub release response is invalid")
    if release.get("id") != release_id or release.get("tag_name") != listed.get("tag_name"):
        fail("selected GitHub release changed during verification")
    if release.get("draft") is not False or release.get("prerelease") is not False:
        fail("selected GitHub release is not stable")
    if release.get("immutable") is not True:
        fail("selected GitHub release is not immutable")
    tag = release.get("tag_name")
    if not isinstance(tag, str) or not tag.startswith("v"):
        fail("selected GitHub release tag is invalid")
    version = tag[1:]
    detected_version = version_tuple(version)
    published_at = validate_timestamp(release.get("published_at"))
    target = release.get("target_commitish")
    if not isinstance(target, str) or not target or len(target) > 255:
        fail("selected GitHub release target is invalid")

    encoded_tag = urllib.parse.quote(tag, safe="")
    tag_ref = client.github_json(f"/repos/{UPSTREAM_REPOSITORY}/git/ref/tags/{encoded_tag}")
    if not isinstance(tag_ref, dict) or not isinstance(tag_ref.get("object"), dict):
        fail("GitHub tag reference is invalid")
    tag_object = tag_ref["object"]
    if tag_object.get("type") != "commit":
        fail("upstream release tag must point directly to a commit")
    revision = tag_object.get("sha")
    if not isinstance(revision, str) or not LOWER_SHA.fullmatch(revision):
        fail("upstream release tag commit is invalid")

    encoded_target = urllib.parse.quote(target, safe="")
    target_commit = client.github_json(f"/repos/{UPSTREAM_REPOSITORY}/commits/{encoded_target}")
    if not isinstance(target_commit, dict) or target_commit.get("sha") != revision:
        fail("release target commit does not match its direct tag")

    contents = client.github_json(
        f"/repos/{UPSTREAM_REPOSITORY}/contents/package.json?ref={revision}"
    )
    decode_repository_package(contents, revision, version)

    current_version = version_tuple(current["version"])
    if detected_version < current_version:
        fail("detected upstream release would downgrade the lock")
    if detected_version == current_version:
        github_identity = {
            "repository": UPSTREAM_REPOSITORY,
            "release": {"id": release_id, "tag": tag, "published_at": published_at},
            "version": version,
            "revision": revision,
        }
        current_github_identity = {
            key: current[key]
            for key in ("repository", "release", "version", "revision")
        }
        if github_identity != current_github_identity:
            fail("same-version upstream GitHub release identity changed")

    try:
        npm_identity = verify_npm_artifact(client, version, revision, npm_origin)
    except AwaitingNPM as error:
        if detected_version > current_version:
            return "awaiting-npm", None
        fail(f"same-version {error}")

    candidate = {
        "schema": 1,
        "image_revision": 1,
        "repository": UPSTREAM_REPOSITORY,
        "release": {"id": release_id, "tag": tag, "published_at": published_at},
        "version": version,
        "revision": revision,
        "npm": npm_identity,
    }
    validate_lock(candidate, npm_origin)

    if detected_version == current_version:
        identity = dict(candidate)
        identity["image_revision"] = current["image_revision"]
        if identity != current:
            fail("same-version upstream release identity changed")
        return "up-to-date", None
    return "update-available", candidate


def canonical_lock(document: dict[str, Any]) -> bytes:
    return (json.dumps(document, indent=2, ensure_ascii=False) + "\n").encode("utf-8")


def atomic_write(path: pathlib.Path, data: bytes) -> None:
    if path.exists() and (not path.is_file() or path.is_symlink()):
        fail(f"refusing to replace non-regular file: {path}")
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary = pathlib.Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, 0o644)
        os.replace(temporary, path)
    finally:
        if temporary.exists():
            temporary.unlink()


def same_upstream_identity(left: dict[str, Any], right: dict[str, Any]) -> bool:
    return {key: value for key, value in left.items() if key != "image_revision"} == {
        key: value for key, value in right.items() if key != "image_revision"
    }


def apply_candidate(
    current_path: pathlib.Path,
    candidate_path: pathlib.Path,
    package_path: pathlib.Path,
    notices_path: pathlib.Path,
) -> None:
    current = validate_lock(load_json_file(current_path, "current upstream lock"))
    candidate = validate_lock(load_json_file(candidate_path, "candidate upstream lock"))
    current_version = version_tuple(current["version"])
    candidate_version = version_tuple(candidate["version"])
    valid_new_release = candidate_version > current_version and candidate["image_revision"] == 1
    valid_repackage = (
        candidate_version == current_version
        and same_upstream_identity(current, candidate)
        and candidate["image_revision"] == current["image_revision"] + 1
    )
    if not (valid_new_release or valid_repackage):
        fail("candidate is neither a newer release nor the next packaging revision")

    package = load_json_file(package_path, "container package.json")
    if not isinstance(package, dict) or not isinstance(package.get("dependencies"), dict):
        fail("container package.json structure is invalid")
    if set(package["dependencies"]) != {NPM_PACKAGE}:
        fail("container package.json has unexpected runtime dependencies")
    package["dependencies"][NPM_PACKAGE] = candidate["version"]

    notices = notices_path.read_text(encoding="utf-8")
    pattern = re.compile(r"The image installs @bitkyc08/opencodex [^ ]+ from npm")
    if len(pattern.findall(notices)) != 1:
        fail("upstream notices version sentence is not canonical")
    notices = pattern.sub(
        f"The image installs @bitkyc08/opencodex {candidate['version']} from npm",
        notices,
    )

    atomic_write(current_path, canonical_lock(candidate))
    atomic_write(package_path, (json.dumps(package, indent=2, ensure_ascii=False) + "\n").encode("utf-8"))
    atomic_write(notices_path, notices.encode("utf-8"))


def verify_tree(container: pathlib.Path) -> dict[str, Any]:
    lock = validate_lock(load_json_file(container / "upstream.lock.json", "upstream lock"))
    package = load_json_file(container / "package.json", "container package.json")
    if not isinstance(package, dict) or not isinstance(package.get("dependencies"), dict):
        fail("container package.json structure is invalid")
    if package["dependencies"].get(NPM_PACKAGE) != lock["version"]:
        fail("container package.json does not match upstream lock")
    notices = (container / "UPSTREAM_NOTICES.md").read_text(encoding="utf-8")
    if notices.count(f"@bitkyc08/opencodex {lock['version']} from npm") != 1:
        fail("upstream notices do not match upstream lock")
    bun_lock = (container / "bun.lock").read_text(encoding="utf-8")
    root_pattern = re.compile(
        r'(?m)^\s{8}"@bitkyc08/opencodex": "' + re.escape(lock["version"]) + r'",$'
    )
    package_pattern = re.compile(
        r'(?m)^\s{4}"@bitkyc08/opencodex": \["@bitkyc08/opencodex@'
        + re.escape(lock["version"])
        + r'".*"'
        + re.escape(lock["npm"]["integrity"])
        + r'"\],$'
    )
    if len(root_pattern.findall(bun_lock)) != 1 or len(package_pattern.findall(bun_lock)) != 1:
        fail("bun.lock root package or integrity does not match upstream lock")
    return lock


def command_detect(arguments: argparse.Namespace) -> int:
    current = validate_lock(load_json_file(arguments.lock, "current upstream lock"), arguments.npm_registry)
    token = os.environ.get(arguments.github_token_env, "") if arguments.github_token_env else ""
    client = NetworkClient(arguments.github_api, arguments.npm_registry, token)
    status, candidate = detect(client, current, arguments.npm_registry)
    output: dict[str, Any] = {"schema": 1, "status": status, "current_version": current["version"]}
    if candidate is not None:
        if arguments.candidate_output.exists() or arguments.candidate_output.is_symlink():
            fail("candidate output must not already exist")
        atomic_write(arguments.candidate_output, canonical_lock(candidate))
        output.update(
            {
                "version": candidate["version"],
                "image_revision": candidate["image_revision"],
                "release_id": candidate["release"]["id"],
                "revision": candidate["revision"],
            }
        )
    print(json.dumps(output, separators=(",", ":"), sort_keys=True))
    return 0


def command_apply(arguments: argparse.Namespace) -> int:
    apply_candidate(arguments.lock, arguments.candidate, arguments.package_json, arguments.notices)
    applied = validate_lock(load_json_file(arguments.lock, "applied upstream lock"))
    print(
        json.dumps(
            {
                "schema": 1,
                "status": "applied",
                "version": applied["version"],
                "image_revision": applied["image_revision"],
                "requires_bun_lock_refresh": True,
            },
            separators=(",", ":"),
            sort_keys=True,
        )
    )
    return 0


def command_verify(arguments: argparse.Namespace) -> int:
    verified = verify_tree(arguments.container)
    if getattr(arguments, "verify_provenance", False):
        npm_registry = getattr(arguments, "npm_registry", NPM_REGISTRY)
        client = NetworkClient(GITHUB_API, npm_registry)
        try:
            npm_identity = verify_npm_artifact(
                client,
                verified["version"],
                verified["revision"],
                npm_registry,
            )
        except AwaitingNPM as error:
            fail(f"locked npm provenance is unavailable: {error}")
        if npm_identity != verified["npm"]:
            fail("live npm artifact identity differs from the upstream lock")
    print(
        json.dumps(
            {
                "schema": 1,
                "status": "verified",
                "version": verified["version"],
                "image_revision": verified["image_revision"],
            },
            separators=(",", ":"),
            sort_keys=True,
        )
    )
    return 0


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(description=__doc__)
    subcommands = result.add_subparsers(dest="command", required=True)
    detector = subcommands.add_parser("detect", help="verify upstream and write a candidate lock")
    detector.add_argument("--lock", type=pathlib.Path, required=True)
    detector.add_argument("--candidate-output", type=pathlib.Path, required=True)
    detector.add_argument("--github-api", default=GITHUB_API)
    detector.add_argument("--npm-registry", default=NPM_REGISTRY)
    detector.add_argument("--github-token-env", default="GITHUB_TOKEN")
    detector.set_defaults(handler=command_detect)

    apply = subcommands.add_parser(
        "apply",
        help="apply one verified candidate before a pinned Bun lock refresh and verify-tree",
    )
    apply.add_argument("--lock", type=pathlib.Path, required=True)
    apply.add_argument("--candidate", type=pathlib.Path, required=True)
    apply.add_argument("--package-json", type=pathlib.Path, required=True)
    apply.add_argument("--notices", type=pathlib.Path, required=True)
    apply.set_defaults(handler=command_apply)

    verify = subcommands.add_parser("verify-tree", help="verify lock/package/bun/notices agreement")
    verify.add_argument("--container", type=pathlib.Path, required=True)
    verify.add_argument("--verify-provenance", action="store_true")
    verify.add_argument("--npm-registry", default=NPM_REGISTRY)
    verify.set_defaults(handler=command_verify)
    return result


def main() -> int:
    arguments = parser().parse_args()
    try:
        return arguments.handler(arguments)
    except AwaitingNPM:
        print('{"schema":1,"status":"awaiting-npm"}')
        return 0
    except (ContractError, OSError, urllib.error.URLError) as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

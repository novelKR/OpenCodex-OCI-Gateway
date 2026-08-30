#!/usr/bin/env python3
"""Reproduce reviewed OpenCodex npm teardown profiles from registry artifacts."""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import io
import json
import os
import pathlib
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.parse


REGISTRY = "https://registry.npmjs.org/"
PACKAGE_ROOT = "@bitkyc08/opencodex"
MAX_PACKAGES = 256
MAX_TARBALL_BYTES = 128 * 1024 * 1024
MAX_METADATA_BYTES = 32 * 1024 * 1024
MAX_TOTAL_BYTES = 512 * 1024 * 1024

# These profiles predate reproducible review manifests. Keep the exception
# explicit and closed: every profile added after this baseline must carry a
# registry-backed manifest that this tool reconstructs twice.
LEGACY_PROFILE_VARIANTS = {
    "npm_2_22_0_darwin_arm64_v1",
    "npm_2_22_0_darwin_arm64_v2",
    "npm_2_23_0_darwin_arm64_v1",
    "npm_2_24_0_darwin_arm64_v1",
    "npm_2_24_1_darwin_arm64_v1",
    "npm_2_24_2_darwin_arm64_v1",
    "npm_2_25_0_darwin_arm64_v1",
    "npm_2_26_0_darwin_arm64_v1",
    "npm_2_27_0_darwin_arm64_v1",
    "npm_2_28_0_darwin_arm64_v1",
    "npm_2_29_0_darwin_arm64_v1",
    "npm_2_31_0_darwin_arm64_v1",
    "npm_2_32_0_darwin_arm64_v1",
    "npm_2_32_1_darwin_arm64_v1",
    "npm_2_33_0_darwin_arm64_v1",
}


class ReviewError(RuntimeError):
    pass


def curl(url: str, *, accept_install_metadata: bool = False) -> bytes:
    command = [
        "/usr/bin/curl", "--fail", "--silent", "--show-error", "--location",
        "--retry", "3", "--max-time", "180",
    ]
    if accept_install_metadata:
        command.extend(["--header", "Accept: application/vnd.npm.install-v1+json"])
    command.append(url)
    result = subprocess.run(command, check=True, capture_output=True)
    maximum = MAX_METADATA_BYTES if accept_install_metadata else MAX_TARBALL_BYTES
    if len(result.stdout) > maximum:
        raise ReviewError(f"registry response exceeds bound: {url}")
    return result.stdout


def registry_distribution(identity: tuple[str, str]) -> tuple[tuple[str, str], dict[str, str]]:
    name, version = identity
    encoded = urllib.parse.quote(name, safe="@")
    metadata = json.loads(curl(REGISTRY + encoded, accept_install_metadata=True))
    release = metadata.get("versions", {}).get(version)
    if not isinstance(release, dict):
        raise ReviewError(f"registry version missing: {name}@{version}")
    distribution = release.get("dist", {})
    tarball = distribution.get("tarball", "")
    integrity = distribution.get("integrity", "")
    if not tarball.startswith(REGISTRY) or not integrity.startswith("sha512-"):
        raise ReviewError(f"untrusted registry metadata: {name}@{version}")
    return identity, {"tarball": tarball, "integrity": integrity}


def verify_integrity(payload: bytes, integrity: str) -> None:
    algorithm, encoded = integrity.split("-", 1)
    if algorithm != "sha512":
        raise ReviewError(f"unsupported integrity algorithm: {algorithm}")
    if hashlib.sha512(payload).digest() != base64.b64decode(encoded, validate=True):
        raise ReviewError("registry tarball integrity mismatch")


def safe_relative(value: str) -> pathlib.PurePosixPath:
    path = pathlib.PurePosixPath(value)
    if value == "" or path.is_absolute() or ".." in path.parts or path.as_posix() != value:
        raise ReviewError(f"unsafe relative path: {value}")
    return path


def extract_package(payload: bytes, destination: pathlib.Path) -> None:
    destination.mkdir(parents=True, exist_ok=False)
    directories: list[tuple[pathlib.Path, int]] = []
    extracted_bytes = 0
    with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as archive:
        for member in archive.getmembers():
            path = pathlib.PurePosixPath(member.name)
            if not path.parts or path.parts[0] != "package":
                raise ReviewError(f"unexpected tar path: {member.name}")
            relative = pathlib.PurePosixPath(*path.parts[1:])
            if not relative.parts:
                continue
            safe_relative(relative.as_posix())
            target = destination.joinpath(*relative.parts)
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
                directories.append((target, member.mode & 0o777))
            elif member.isfile():
                extracted_bytes += member.size
                if member.size < 0 or extracted_bytes > MAX_TOTAL_BYTES:
                    raise ReviewError("expanded package exceeds bound")
                target.parent.mkdir(parents=True, exist_ok=True)
                source = archive.extractfile(member)
                if source is None:
                    raise ReviewError(f"missing tar payload: {member.name}")
                with target.open("wb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(member.mode & 0o777)
            else:
                raise ReviewError(f"unsupported tar entry: {member.name}")
    for directory, mode in sorted(directories, key=lambda item: len(item[0].parts), reverse=True):
        directory.chmod(mode)
    destination.chmod(0o755)


def apply_transform(root: pathlib.Path, transform: dict[str, object]) -> None:
    if transform != {
        "kind": "bun_platform_binary_v1",
        "platform_package_path": "node_modules/@oven/bun-darwin-aarch64",
        "wrapper_package_path": "node_modules/bun",
        "source": "bin/bun",
        "destinations": ["bin/bun.exe", "bin/bunx.exe"],
    }:
        raise ReviewError("unreviewed postinstall transform")
    source = root / str(transform["platform_package_path"]) / str(transform["source"])
    destinations = [
        root / str(transform["wrapper_package_path"]) / str(value)
        for value in transform["destinations"]
    ]
    payload = source.read_bytes()
    if not payload or source.stat().st_mode & 0o111 == 0:
        raise ReviewError("reviewed Bun source is not executable")
    for destination in destinations:
        destination.unlink(missing_ok=True)
    destinations[0].write_bytes(payload)
    destinations[0].chmod(0o755)
    os.link(destinations[0], destinations[1])
    source.unlink()


def create_symlinks(root: pathlib.Path, symlinks: list[dict[str, str]]) -> None:
    resolved_root = root.resolve(strict=True)
    for item in symlinks:
        path = root.joinpath(*safe_relative(item["path"]).parts)
        target = item["target"]
        if not target or os.path.isabs(target):
            raise ReviewError(f"unsafe symlink target: {target}")
        path.parent.mkdir(parents=True, exist_ok=True)
        os.symlink(target, path)
        resolved = path.resolve(strict=True)
        if resolved_root not in resolved.parents and resolved != resolved_root:
            raise ReviewError(f"symlink escapes package root: {item['path']}")


def identity_for_package_json(root: pathlib.Path, directory: pathlib.Path) -> tuple[str, str] | None:
    if directory != root:
        parts = directory.relative_to(root).parts
        ordinary = len(parts) >= 2 and parts[-2] == "node_modules"
        scoped = len(parts) >= 3 and parts[-3] == "node_modules" and parts[-2].startswith("@")
        if not ordinary and not scoped:
            return None
    data = json.loads((directory / "package.json").read_text(encoding="utf-8"))
    name, version = data.get("name"), data.get("version")
    if not isinstance(name, str) or not isinstance(version, str):
        raise ReviewError(f"invalid package identity: {directory}")
    return name, version


def package_inventory(root: pathlib.Path) -> list[tuple[str, str, str]]:
    result = []
    for package_json in root.rglob("package.json"):
        identity = identity_for_package_json(root, package_json.parent)
        if identity is None:
            continue
        path = "." if package_json.parent == root else package_json.parent.relative_to(root).as_posix()
        result.append((path, *identity))
    return sorted(result)


def canonical_field(digest: object, value: bytes) -> None:
    digest.update(len(value).to_bytes(8, "big"))
    digest.update(value)


def closure_digest(root: pathlib.Path) -> str:
    digest = hashlib.sha256()
    canonical_field(digest, b"relay-reviewed-package-closure-v1")
    entries = [root]
    for directory, names, files in os.walk(root, topdown=True, followlinks=False):
        names.sort()
        files.sort()
        base = pathlib.Path(directory)
        entries.extend(base / name for name in names + files)
    entries = sorted(
        set(entries),
        key=lambda path: "." if path == root else path.relative_to(root).as_posix(),
    )
    for path in entries:
        relative = "." if path == root else path.relative_to(root).as_posix()
        info = path.lstat()
        canonical_field(digest, relative.encode())
        canonical_field(digest, bytes([1 if info.st_mode & 0o111 else 0]))
        if path.is_symlink():
            canonical_field(digest, b"symlink")
            canonical_field(digest, os.readlink(path).encode())
        elif path.is_dir():
            canonical_field(digest, b"directory")
        elif path.is_file():
            canonical_field(digest, b"file")
            canonical_field(digest, hashlib.sha256(path.read_bytes()).digest())
        else:
            raise ReviewError(f"unsupported closure entry: {path}")
    return digest.hexdigest()


def validate_manifest(manifest: dict[str, object]) -> None:
    if set(manifest) != {
        "schema_version", "platform", "architecture", "packages", "symlinks",
        "postinstall_transforms", "profile",
    }:
        raise ReviewError("unexpected review manifest field")
    if manifest.get("schema_version") != 1 or manifest.get("platform") != "darwin" or manifest.get("architecture") != "arm64":
        raise ReviewError("unsupported review manifest")
    packages = manifest.get("packages")
    symlinks = manifest.get("symlinks")
    transforms = manifest.get("postinstall_transforms")
    profile = manifest.get("profile")
    if not isinstance(packages, list) or not 1 <= len(packages) <= MAX_PACKAGES:
        raise ReviewError("invalid package inventory")
    if not isinstance(symlinks, list) or len(symlinks) > 32 or not isinstance(transforms, list) or len(transforms) > 8:
        raise ReviewError("invalid transform inventory")
    if not isinstance(profile, dict) or set(profile) != {
        "adapter_id", "architecture", "artifact_variant", "closure_sha256",
        "critical_modules", "package_name", "package_version", "platform",
        "registry_integrity",
    } or profile.get("package_name") != PACKAGE_ROOT:
        raise ReviewError("invalid profile identity")
    if profile.get("platform") != "darwin" or profile.get("architecture") != "arm64" or not all(
        isinstance(profile.get(key), str)
        for key in [
            "adapter_id", "artifact_variant", "closure_sha256", "package_version",
            "registry_integrity",
        ]
    ):
        raise ReviewError("invalid profile platform or field type")
    if re.fullmatch(r"npm_[0-9_]+_darwin_arm64_v[0-9]+", profile["artifact_variant"]) is None or \
            re.fullmatch(r"[a-z0-9_]+", profile["adapter_id"]) is None or \
            re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", profile["package_version"]) is None or \
            re.fullmatch(r"[0-9a-f]{64}", profile["closure_sha256"]) is None:
        raise ReviewError("invalid bounded profile identifier")
    critical_modules = profile.get("critical_modules")
    if not isinstance(critical_modules, dict) or not 1 <= len(critical_modules) <= 32:
        raise ReviewError("invalid critical module inventory")
    for relative, digest in critical_modules.items():
        if not isinstance(relative, str) or not isinstance(digest, str) or \
                re.fullmatch(r"[0-9a-f]{64}", digest) is None:
            raise ReviewError("invalid critical module entry")
        safe_relative(relative)
    paths = [item.get("path") for item in packages if isinstance(item, dict)]
    if len(paths) != len(packages) or len(set(paths)) != len(paths) or "." not in paths:
        raise ReviewError("duplicate or missing package path")
    for package in packages:
        if not isinstance(package, dict) or set(package) != {"path", "name", "version", "tarball", "integrity"}:
            raise ReviewError("unexpected package manifest field")
        if not all(isinstance(package[key], str) for key in package):
            raise ReviewError("invalid package manifest field type")
        if package["path"] != ".":
            safe_relative(package["path"])
        if not package["tarball"].startswith(REGISTRY) or not package["integrity"].startswith("sha512-"):
            raise ReviewError("untrusted package distribution")
    for symlink in symlinks:
        if not isinstance(symlink, dict) or set(symlink) != {"path", "target"} or not all(
            isinstance(symlink[key], str) for key in symlink
        ):
            raise ReviewError("invalid symlink manifest")
        safe_relative(symlink["path"])
    for transform in transforms:
        if not isinstance(transform, dict):
            raise ReviewError("invalid transform manifest")
    if profile.get("registry_integrity") != next(item for item in packages if item["path"] == ".")["integrity"]:
        raise ReviewError("root package integrity mismatch")


def reconstruct(manifest: dict[str, object], destination: pathlib.Path, payloads: dict[str, bytes]) -> None:
    packages = manifest["packages"]
    root_package = next(item for item in packages if item["path"] == ".")
    extract_package(payloads[root_package["integrity"]], destination)
    nested = sorted(
        (item for item in packages if item["path"] != "."),
        key=lambda item: (item["path"].count("/"), item["path"]),
    )
    for package in nested:
        path = safe_relative(package["path"])
        extract_package(payloads[package["integrity"]], destination.joinpath(*path.parts))
    create_symlinks(destination, manifest["symlinks"])
    for transform in manifest["postinstall_transforms"]:
        apply_transform(destination, transform)


def verify_adapter_preflight(repo: pathlib.Path, root: pathlib.Path, profile: dict[str, object], temporary: pathlib.Path) -> None:
    temporary.mkdir()
    execution = temporary / "adapter"
    home = temporary / "home"
    opencodex_home = temporary / "opencodex-home"
    execution.mkdir()
    home.mkdir()
    opencodex_home.mkdir()
    adapter_root = repo / "client/relay/internal/handoff/adapter"
    for name in ["relay_preserve_v1.ts", "relay_preserve_v1_shim.ts"]:
        shutil.copy2(adapter_root / name, execution / name)
    os.symlink(root, execution / "package")
    environment = os.environ.copy()
    environment.update({"HOME": str(home), "OPENCODEX_HOME": str(opencodex_home)})
    result = subprocess.run(
        [
            str(root / "node_modules/bun/bin/bun.exe"),
            str(execution / "relay_preserve_v1.ts"),
            "--adapter-id", str(profile["adapter_id"]), "--preflight",
        ],
        cwd=temporary,
        env=environment,
        check=True,
        capture_output=True,
        text=True,
        timeout=30,
    )
    receipt = json.loads(result.stdout)
    if receipt.get("operation") != "relay_preserving_teardown_preflight" or receipt.get("status") != "ready" or receipt.get("adapter_id") != profile["adapter_id"]:
        raise ReviewError("teardown adapter preflight failed")


def verify_source_registration(repo: pathlib.Path, profile: dict[str, object]) -> None:
    source = (repo / "client/relay/internal/handoff/teardown_profiles.go").read_text(encoding="utf-8")
    expected = re.compile(
        r'reviewedTeardownProfile\(\s*"' + re.escape(str(profile["package_version"])) +
        r'",\s*"' + re.escape(str(profile["artifact_variant"])) +
        r'",\s*"' + re.escape(str(profile["registry_integrity"])) +
        r'",\s*"' + re.escape(str(profile["closure_sha256"])) + r'"',
    )
    if expected.search(source) is None:
        raise ReviewError(f"profile is not registered in Go source: {profile['artifact_variant']}")


def verify_manifest_coverage(repo: pathlib.Path, profiles: list[dict[str, object]]) -> None:
    source = (repo / "client/relay/internal/handoff/teardown_profiles.go").read_text(encoding="utf-8")
    registered = set(re.findall(
        r'reviewedTeardownProfile\(\s*"[^"]+",\s*"(npm_[^"]+)"',
        source,
    ))
    manifested = {str(profile["artifact_variant"]) for profile in profiles}
    if len(manifested) != len(profiles):
        raise ReviewError("duplicate teardown profile review manifest")
    expected = LEGACY_PROFILE_VARIANTS | manifested
    if registered != expected:
        missing = sorted(registered - expected)
        stale = sorted(expected - registered)
        raise ReviewError(
            "teardown profile manifest coverage mismatch: "
            f"unreviewed={missing} stale={stale}"
        )


def check_manifest(repo: pathlib.Path, path: pathlib.Path) -> None:
    manifest = json.loads(path.read_text(encoding="utf-8"))
    validate_manifest(manifest)
    profile = manifest["profile"]
    verify_source_registration(repo, profile)
    packages = manifest["packages"]
    identities = {(item["name"], item["version"]) for item in packages}
    with concurrent.futures.ThreadPoolExecutor(max_workers=12) as pool:
        distributions = dict(pool.map(registry_distribution, sorted(identities)))
    for package in packages:
        if distributions[(package["name"], package["version"])] != {
            "tarball": package["tarball"], "integrity": package["integrity"],
        }:
            raise ReviewError(f"registry metadata changed: {package['name']}@{package['version']}")

    payloads: dict[str, bytes] = {}
    total = 0
    for package in packages:
        integrity = package["integrity"]
        if integrity in payloads:
            continue
        payload = curl(package["tarball"])
        verify_integrity(payload, integrity)
        total += len(payload)
        if total > MAX_TOTAL_BYTES:
            raise ReviewError("downloaded profile exceeds bound")
        payloads[integrity] = payload

    expected_inventory = sorted((item["path"], item["name"], item["version"]) for item in packages)
    with tempfile.TemporaryDirectory(prefix="opencodex-profile-review-") as temporary_name:
        temporary = pathlib.Path(temporary_name)
        roots = [temporary / "root-a", temporary / "root-b"]
        for root in roots:
            reconstruct(manifest, root, payloads)
            if package_inventory(root) != expected_inventory:
                raise ReviewError("reconstructed transitive package inventory differs")
            actual = closure_digest(root)
            if actual != profile["closure_sha256"]:
                raise ReviewError(f"closure mismatch: {actual}")
            for relative, expected in profile["critical_modules"].items():
                actual_module = hashlib.sha256((root / relative).read_bytes()).hexdigest()
                if actual_module != expected:
                    raise ReviewError(f"critical module mismatch: {relative}")
        if closure_digest(roots[0]) != closure_digest(roots[1]):
            raise ReviewError("independent reconstruction mismatch")
        verify_adapter_preflight(repo, roots[0], profile, temporary / "preflight-a")
        verify_adapter_preflight(repo, roots[1], profile, temporary / "preflight-b")
    print(f"profile={profile['artifact_variant']} packages={len(packages)} closure={profile['closure_sha256']} status=verified")


def audit_latest(repo: pathlib.Path) -> None:
    encoded = urllib.parse.quote(PACKAGE_ROOT, safe="@")
    metadata = json.loads(curl(REGISTRY + encoded, accept_install_metadata=True))
    latest = metadata.get("dist-tags", {}).get("latest")
    if not isinstance(latest, str):
        raise ReviewError("npm latest version unavailable")
    source = (repo / "client/relay/internal/handoff/teardown_profiles.go").read_text(encoding="utf-8")
    supported = set(re.findall(r'reviewedTeardownProfile\(\s*"([0-9]+\.[0-9]+\.[0-9]+)"', source))
    state = "supported" if latest in supported else "unsupported"
    line = f"OpenCodex npm latest: `{latest}` - **{state}** by Relay automatic-removal profiles."
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as output:
            output.write("### Teardown profile audit\n\n" + line + "\n")
    print(f"latest={latest} state={state}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--audit-latest", action="store_true")
    args = parser.parse_args()
    if not args.check and not args.audit_latest:
        parser.error("at least one action is required")
    repo = pathlib.Path(__file__).resolve().parents[3]
    if platform.system() != "Darwin" or platform.machine() not in {"arm64", "aarch64"}:
        raise ReviewError("profile reconstruction requires darwin/arm64")
    if args.check:
        manifests = sorted((repo / "client/relay/internal/handoff/review-manifests").glob("*.json"))
        if not manifests:
            raise ReviewError("no teardown profile review manifests")
        parsed = [json.loads(manifest.read_text(encoding="utf-8")) for manifest in manifests]
        for manifest, value in zip(manifests, parsed, strict=True):
            validate_manifest(value)
            if manifest.stem != value["profile"]["artifact_variant"]:
                raise ReviewError(f"review manifest filename mismatch: {manifest.name}")
        verify_manifest_coverage(repo, [value["profile"] for value in parsed])
        for manifest in manifests:
            check_manifest(repo, manifest)
    if args.audit_latest:
        audit_latest(repo)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (ReviewError, OSError, subprocess.SubprocessError, ValueError, json.JSONDecodeError) as error:
        print(f"teardown profile review failed: {error}", file=sys.stderr)
        raise SystemExit(1)

#!/usr/bin/env python3
"""Fail closed when a public export contains private or environment-specific data."""

from __future__ import annotations

import argparse
import ipaddress
import os
import pathlib
import re
import subprocess
import sys


TEXT_SUFFIXES = {
    "", ".c", ".go", ".h", ".html", ".in", ".json", ".lock", ".md",
    ".plist", ".py", ".service", ".sh", ".swift", ".toml", ".txt",
    ".yaml", ".yml",
}
SECRET_BEARING_TEXT_SUFFIXES = {".env", ".key", ".pem"}
FORBIDDEN_ROOT_PATHS = {".git", ".generated", "local", "opencodex", "research"}
FORBIDDEN_FILES = {
    ".DS_Store",
    "REDEPLOY_REQUIRED.md",
    "artifact-inventory.md",
    "artifact-inventory.ko.md",
    "operations-ledger.md",
    "operations-ledger.ko.md",
    "private-github-relay-operations.md",
    "private-github-relay-operations.ko.md",
}
RELEASE_SCRIPT_PATHS = {
    pathlib.PurePosixPath("client/relay/scripts/build-release.sh"),
    pathlib.PurePosixPath("client/relay/scripts/install-relay.sh"),
    pathlib.PurePosixPath("client/relay/scripts/publish-github-release.sh"),
}


def is_text_candidate(path: pathlib.Path) -> bool:
    name = path.name.lower().strip("#~")
    normalized = pathlib.PurePath(name)
    suffixes = {suffix.lower() for suffix in normalized.suffixes}
    return (
        normalized.suffix.lower() in TEXT_SUFFIXES
        or bool(suffixes & SECRET_BEARING_TEXT_SUFFIXES)
        or name.startswith(".env.")
    )


def fail(message: str) -> None:
    print(f"ERROR: {message}", file=sys.stderr)
    raise SystemExit(1)


def text_files(root: pathlib.Path, allow_git_metadata: bool = False):
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if relative.parts[0] == ".git" and allow_git_metadata:
            continue
        if path.is_symlink():
            fail(f"symlink is not permitted in public export: {path.relative_to(root)}")
        if not path.is_file():
            continue
        if relative.parts[0] in FORBIDDEN_ROOT_PATHS or ".git" in relative.parts:
            fail(f"forbidden path in public export: {relative}")
        if path.name in FORBIDDEN_FILES:
            fail(f"forbidden file in public export: {relative}")
        if is_text_candidate(path):
            try:
                yield relative, path.read_text(encoding="utf-8")
            except UnicodeDecodeError:
                fail(f"text-like file is not UTF-8: {relative}")


def scan_text(root: pathlib.Path, allow_git_metadata: bool = False) -> None:
    legacy = "-".join(("pw", "opencodex"))
    private_domain = "novelkr" + ".wiki"
    users_root = "/" + "Users" + "/"
    private_volume = "/" + "Volumes" + "/" + "DevData" + "/"
    patterns = {
        "legacy namespace": re.compile(re.escape(legacy), re.IGNORECASE),
        "private hostname": re.compile(re.escape(private_domain), re.IGNORECASE),
        "personal macOS path": re.compile(
            re.escape(users_root) + r"[^/]+/" + "|" + re.escape(private_volume)
        ),
        "private key material": re.compile(r"-----BEGIN (?:OPENSSH |RSA |EC )?PRIVATE KEY-----"),
        "specific SSH key filename": re.compile(r"IdentityFile[^\n]*[/ ]oci-[^\s]+\.key"),
    }
    ip_pattern = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")
    for relative, content in text_files(root, allow_git_metadata):
        for name, pattern in patterns.items():
            match = pattern.search(content)
            if match:
                line = content.count("\n", 0, match.start()) + 1
                fail(f"{name} found in {relative}:{line}")
        for match in ip_pattern.finditer(content):
            try:
                address = ipaddress.ip_address(match.group(0))
            except ValueError:
                continue
            if address.is_global:
                line = content.count("\n", 0, match.start()) + 1
                fail(f"global IPv4 address found in {relative}:{line}")
        if pathlib.PurePosixPath(relative.as_posix()) in RELEASE_SCRIPT_PATHS:
            release_patterns = {
                "removed Apple signing option": re.compile(
                    r"--(?:apple-signing-identity|apple-team-id|notary-keychain-profile)\b"
                ),
                "notarization command": re.compile(
                    r"(?m)^\s*(?:xcrun\s+)?(?:notarytool|stapler)\b"
                ),
                "removed manifest Team ID": re.compile(r'"team_id"\s*:'),
            }
            for name, pattern in release_patterns.items():
                match = pattern.search(content)
                if match:
                    line = content.count("\n", 0, match.start()) + 1
                    fail(f"{name} found in {relative}:{line}")


def verify_initial_history(root: pathlib.Path) -> None:
    count = subprocess.run(
        ["git", "-C", str(root), "rev-list", "--count", "--all"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if count != "1":
        fail(f"public history must contain exactly one commit before publication, found {count}")
    parents = subprocess.run(
        ["git", "-C", str(root), "rev-list", "--parents", "--all"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip().split()
    if len(parents) != 1:
        fail("public initial commit must have no parent")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?", type=pathlib.Path, default=pathlib.Path("."))
    parser.add_argument("--initial-history", action="store_true")
    arguments = parser.parse_args()
    root = arguments.root.resolve()
    if not root.is_dir():
        fail("public export root is not a directory")
    scan_text(root, arguments.initial_history)
    if arguments.initial_history:
        verify_initial_history(root)
    print("public_hygiene=ok")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

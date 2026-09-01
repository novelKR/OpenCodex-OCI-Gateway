#!/usr/bin/env python3
"""Validate the tracked macOS release build against prior public builds."""

from __future__ import annotations

import os
import pathlib
import re
import stat
import sys
from typing import NoReturn


CURRENT_PATTERN = re.compile(rb"[1-9][0-9]{0,3}\n?")
PREVIOUS_PATTERN = re.compile(r"[1-9][0-9]{0,3}")


def fail(message: str) -> NoReturn:
    raise SystemExit(f"ERROR: {message}")


def main() -> None:
    if len(sys.argv) != 3:
        fail("usage: validate-release-build-number.py FILE PREVIOUS_BUILD")
    source = pathlib.Path(sys.argv[1])
    previous = sys.argv[2]
    try:
        metadata = os.lstat(source)
    except OSError as error:
        fail(f"cannot inspect RELEASE_BUILD_NUMBER: {error}")
    if not stat.S_ISREG(metadata.st_mode):
        fail("RELEASE_BUILD_NUMBER must be a regular file, not a symlink")
    if metadata.st_size < 1 or metadata.st_size > 5:
        fail("RELEASE_BUILD_NUMBER must contain one integer from 1 through 9999")
    try:
        current_bytes = source.read_bytes()
    except OSError as error:
        fail(f"cannot read RELEASE_BUILD_NUMBER: {error}")
    if CURRENT_PATTERN.fullmatch(current_bytes) is None:
        fail("RELEASE_BUILD_NUMBER must contain one integer from 1 through 9999")
    if len(previous) > 32 or PREVIOUS_PATTERN.fullmatch(previous) is None:
        fail("previous public CFBundleVersion is invalid")
    current = current_bytes.decode("ascii").strip()
    if int(current) <= int(previous):
        fail("RELEASE_BUILD_NUMBER must be greater than every previous public app build")
    print(current)


if __name__ == "__main__":
    main()

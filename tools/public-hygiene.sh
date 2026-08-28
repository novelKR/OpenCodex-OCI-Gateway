#!/usr/bin/env bash
set -euo pipefail

readonly ROOT="${1:-.}"
shift || true
exec python3 "$ROOT/tools/public_hygiene.py" "$ROOT" "$@"

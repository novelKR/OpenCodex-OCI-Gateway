#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf 'usage: check-public-checkout.sh [REF]\n' >&2
}

[[ $# -le 1 ]] || {
  usage
  exit 2
}

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly ROOT="$(git -C "$SCRIPT_DIR/.." rev-parse --show-toplevel 2>/dev/null)" || \
  die 'script is not inside a Git checkout'
readonly REF="${1:-HEAD}"
[[ "$REF" == "HEAD" || "$REF" =~ ^[0-9a-f]{40}$ ]] || \
  die 'REF must be HEAD or a full lowercase commit ID'
readonly COMMIT="$(git -C "$ROOT" rev-parse --verify "${REF}^{commit}" 2>/dev/null)" || \
  die 'REF does not resolve to a commit'

for command in git tar python3; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done

temporary="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-public-checkout.XXXXXX")"
cleanup() {
  rm -rf -- "$temporary"
}
trap cleanup EXIT

git -C "$ROOT" archive --format=tar "$COMMIT" |
  tar -xf - -C "$temporary"
[[ -x "$temporary/tools/public-hygiene.sh" ]] || \
  die 'public checkout does not contain the hygiene entrypoint'
"$temporary/tools/public-hygiene.sh" "$temporary"
printf 'public_checkout=ok commit=%s\n' "$COMMIT"

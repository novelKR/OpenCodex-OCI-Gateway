#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 1 ]] || {
  printf 'usage: verify-release-ref.sh vSEMVER\n' >&2
  exit 2
}

readonly VERSION="$1"
[[ "$VERSION" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
  die 'release version must be vSEMVER'
[[ "${GITHUB_REF_TYPE:-}" == "tag" ]] || \
  die 'container release must run from an exact tag ref'
[[ "${GITHUB_REF_NAME:-}" == "$VERSION" ]] || \
  die 'release version does not match the workflow tag ref'
[[ "${GITHUB_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || \
  die 'GITHUB_SHA must be a full lowercase commit ID'

readonly TAG_REF="refs/tags/${VERSION}"
[[ "$(git cat-file -t "$TAG_REF" 2>/dev/null || true)" == "commit" ]] || \
  die 'release tag must exist locally and be lightweight'
readonly TAG_COMMIT="$(git rev-parse --verify "${TAG_REF}^{commit}")"
readonly HEAD_COMMIT="$(git rev-parse --verify 'HEAD^{commit}')"
[[ "$GITHUB_SHA" == "$TAG_COMMIT" ]] || \
  die 'workflow commit does not match the release tag'
[[ "$HEAD_COMMIT" == "$TAG_COMMIT" ]] || \
  die 'checked out commit does not match the release tag'

printf 'release_ref=ok version=%s commit=%s\n' "$VERSION" "$TAG_COMMIT"

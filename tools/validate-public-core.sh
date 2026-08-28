#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 1 ]] || {
  printf 'usage: validate-public-core.sh ROOT\n' >&2
  exit 2
}

input_root="$1"
[[ "$input_root" = /* ]] || die 'ROOT must be absolute'
[[ -d "$input_root" && ! -L "$input_root" ]] || \
  die 'ROOT must be a regular directory'
readonly INPUT_ROOT="$(cd -- "$input_root" && pwd -P)"
[[ ! -e "$INPUT_ROOT/.git" ]] || \
  die 'ROOT must be a plain public export without Git metadata'

for command in bash git go jq python3 swift tar; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done

temporary="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-public-validation.XXXXXX")"
cleanup() {
  rm -rf -- "$temporary"
}
trap cleanup EXIT

tar -C "$INPUT_ROOT" -cf - . | tar -xf - -C "$temporary"
"$temporary/tools/public-hygiene.sh" "$temporary"

git -C "$temporary" init --quiet -b validation
git -C "$temporary" add -f -A
git -C "$temporary" diff --cached --check

bash -n \
  "$temporary"/pilot/scripts/*.sh \
  "$temporary"/pilot/libexec/* \
  "$temporary"/ops/oci/*.sh \
  "$temporary"/client/relay/scripts/*.sh \
  "$temporary"/tools/*.sh

(
  cd -- "$temporary"
  python3 -m unittest discover -s pilot/tests -p 'test_*.py'
)
(
  cd -- "$temporary/client/relay"
  go test ./...
  go test -race ./...
  go vet ./...
)
(
  cd -- "$temporary/client/relay/macos/OpenCodexRelay"
  swift test
  swift build -c release
)

[[ -z "$(git -C "$temporary" diff --cached --name-only --diff-filter=U)" ]] || \
  die 'validation workspace contains unmerged paths'
printf 'public_validation=ok root=%s\n' "$INPUT_ROOT"

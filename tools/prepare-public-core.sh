#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf '%s\n' \
    'usage: prepare-public-core.sh --source-commit COMMIT --version vSEMVER' \
    '       --destination ABSOLUTE_PATH --author-name NAME --author-email EMAIL' >&2
}

source_commit=""
version=""
destination_input=""
author_name=""
author_email=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-commit)
      [[ -z "$source_commit" && $# -ge 2 ]] || die 'invalid or repeated --source-commit'
      source_commit="$2"
      shift 2
      ;;
    --version)
      [[ -z "$version" && $# -ge 2 ]] || die 'invalid or repeated --version'
      version="$2"
      shift 2
      ;;
    --destination)
      [[ -z "$destination_input" && $# -ge 2 ]] || die 'invalid or repeated --destination'
      destination_input="$2"
      shift 2
      ;;
    --author-name)
      [[ -z "$author_name" && $# -ge 2 ]] || die 'invalid or repeated --author-name'
      author_name="$2"
      shift 2
      ;;
    --author-email)
      [[ -z "$author_email" && $# -ge 2 ]] || die 'invalid or repeated --author-email'
      author_email="$2"
      shift 2
      ;;
    *)
      usage
      die "unknown argument: $1"
      ;;
  esac
done

[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || die '--source-commit must be a full lowercase commit ID'
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || \
  die '--version must be vSEMVER'
[[ -n "$author_name" && "$author_name" != *$'\n'* && "$author_name" != *$'\r'* ]] || \
  die '--author-name must be non-empty and single-line'
[[ "$author_email" =~ ^[^[:space:]@]+@[^[:space:]@]+$ ]] || \
  die '--author-email must be a single email address'
[[ "$destination_input" = /* ]] || die '--destination must be absolute'
[[ ! -e "$destination_input" && ! -L "$destination_input" ]] || \
  die 'destination must not already exist'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
root="$(git -C "$script_dir/.." rev-parse --show-toplevel 2>/dev/null)" || \
  die 'script is not inside a Git checkout'
readonly ROOT="$root"
readonly HEAD_COMMIT="$(git -C "$ROOT" rev-parse --verify 'HEAD^{commit}')"
[[ "$source_commit" == "$HEAD_COMMIT" ]] || die 'source commit must equal the current HEAD'
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=all)" ]] || \
  die 'source worktree must be clean'

for command in git python3 tar; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done

destination_parent="$(cd -- "$(dirname -- "$destination_input")" 2>/dev/null && pwd -P)" || \
  die 'destination parent must already exist'
destination_name="$(basename -- "$destination_input")"
[[ -n "$destination_name" && "$destination_name" != "/" ]] || die 'destination name is invalid'
readonly DESTINATION="${destination_parent}/${destination_name}"
case "$DESTINATION" in
  "$ROOT"|"$ROOT"/*) die 'destination must be outside the source checkout' ;;
esac
readonly EVIDENCE="${DESTINATION}.publication.json"
[[ ! -e "$EVIDENCE" && ! -L "$EVIDENCE" ]] || die 'publication evidence already exists'

temporary="$(mktemp -d "${destination_parent}/.public-core-prepare.XXXXXX")"
candidate="$temporary/staging"
validation_input="$temporary/validation-input"
evidence_candidate="$temporary/publication.json"
empty_git_config="$temporary/empty.gitconfig"
empty_git_template="$temporary/empty-template"
empty_hooks="$temporary/empty-hooks"
destination_installed=false
cleanup() {
  exit_code=$?
  trap - EXIT
  rm -rf -- "$temporary"
  if [[ "$exit_code" -ne 0 && "$destination_installed" == true ]]; then
    rm -rf -- "$DESTINATION"
  fi
  exit "$exit_code"
}
trap cleanup EXIT

mkdir "$empty_git_template" "$empty_hooks"
: > "$empty_git_config"

candidate_git() {
  GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$empty_git_config" \
    git -C "$candidate" -c core.hooksPath="$empty_hooks" "$@"
}

"$ROOT/tools/export-public-core.sh" "$candidate" "$source_commit"
exported_files="$(python3 - "$candidate" <<'PY'
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
print(sum(1 for path in root.rglob("*") if path.is_file()))
PY
)"

candidate_git init --quiet -b main --template="$empty_git_template"
candidate_git add -f -A
indexed_files="$(candidate_git ls-files -z |
  python3 -c 'import sys; print(sys.stdin.buffer.read().count(b"\0"))')"
[[ "$indexed_files" == "$exported_files" ]] || \
  die 'public export and candidate index file counts differ'
validated_tree="$(candidate_git write-tree)"

mkdir "$validation_input"
candidate_git archive --format=tar "$validated_tree" | tar -xf - -C "$validation_input"
validation_files="$(python3 - "$validation_input" <<'PY'
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
print(sum(1 for path in root.rglob("*") if path.is_file()))
PY
)"
[[ "$validation_files" == "$exported_files" ]] || \
  die 'validated tree archive and public export file counts differ'
"$validation_input/tools/validate-public-core.sh" "$validation_input"
[[ "$(candidate_git write-tree)" == "$validated_tree" ]] || \
  die 'candidate index changed during validation'

GIT_AUTHOR_NAME="$author_name" \
GIT_AUTHOR_EMAIL="$author_email" \
GIT_COMMITTER_NAME="$author_name" \
GIT_COMMITTER_EMAIL="$author_email" \
  candidate_git -c commit.gpgSign=false commit --quiet --no-gpg-sign \
    -m "Initial public Core ${version}"
public_commit="$(candidate_git rev-parse HEAD)"
public_tree="$(candidate_git rev-parse "${public_commit}^{tree}")"
[[ "$public_tree" == "$validated_tree" ]] || \
  die 'public commit tree differs from the validated tree'
[[ "$(candidate_git show -s --format=%B "$public_commit")" == \
  "Initial public Core ${version}" ]] || die 'public commit message is not canonical'
candidate_git -c tag.gpgSign=false tag "$version" "$public_commit"

"$candidate/tools/public-hygiene.sh" "$candidate" --initial-history
[[ "$(candidate_git symbolic-ref --short HEAD)" == "main" ]] || \
  die 'public staging branch is not main'
[[ "$(candidate_git rev-list --count --all)" == "1" ]] || \
  die 'public staging does not contain exactly one commit'
[[ "$(candidate_git rev-list --parents --all | awk '{print NF}')" == "1" ]] || \
  die 'public staging initial commit has a parent'
[[ "$(candidate_git rev-parse "${version}^{commit}")" == "$public_commit" ]] || \
  die 'public tag does not resolve to the initial commit'
[[ "$(candidate_git cat-file -t "refs/tags/${version}")" == "commit" ]] || \
  die 'public tag is not lightweight'
[[ -z "$(candidate_git status --porcelain --untracked-files=all --ignored=matching)" ]] || \
  die 'public staging worktree contains untracked or ignored files'
candidate_git fsck --strict >/dev/null

[[ "$(git -C "$ROOT" rev-parse --verify 'HEAD^{commit}')" == "$source_commit" ]] || \
  die 'source HEAD changed during preparation'
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=all)" ]] || \
  die 'source worktree changed during preparation'

source_tree="$(git -C "$ROOT" rev-parse "${source_commit}^{tree}")"
allowlist_blob="$(git -C "$ROOT" rev-parse "${source_commit}:config/public-export-allowlist.txt")"

python3 - "$evidence_candidate" "$source_commit" "$source_tree" "$allowlist_blob" \
  "$public_commit" "$public_tree" "$version" "$exported_files" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
document = {
    "schema_version": 1,
    "source_commit": sys.argv[2],
    "source_tree": sys.argv[3],
    "allowlist_blob": sys.argv[4],
    "public_commit": sys.argv[5],
    "public_tree": sys.argv[6],
    "tag": sys.argv[7],
    "exported_files": int(sys.argv[8]),
}
path.write_text(json.dumps(document, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY
chmod 0644 "$evidence_candidate"

mv -- "$candidate" "$DESTINATION"
destination_installed=true
mv -- "$evidence_candidate" "$EVIDENCE"
printf 'public_staging=%s evidence=%s commit=%s tag=%s files=%s\n' \
  "$DESTINATION" "$EVIDENCE" "$public_commit" "$version" "$exported_files"

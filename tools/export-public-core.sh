#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf 'usage: export-public-core.sh DESTINATION [REF]\n' >&2
}

[[ $# -ge 1 && $# -le 2 ]] || {
  usage
  exit 2
}

destination_input="$1"
ref_input="${2:-HEAD}"
[[ "$ref_input" == "HEAD" || "$ref_input" =~ ^[0-9a-f]{40}$ ]] || \
  die 'REF must be HEAD or a full lowercase commit ID'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
root="$(git -C "$script_dir/.." rev-parse --show-toplevel 2>/dev/null)" || \
  die 'script is not inside a Git checkout'
readonly ROOT="$root"
readonly ALLOWLIST="$ROOT/config/public-export-allowlist.txt"
readonly REF="$(git -C "$ROOT" rev-parse --verify "${ref_input}^{commit}" 2>/dev/null)" || \
  die 'REF does not resolve to a commit'
readonly HEAD_COMMIT="$(git -C "$ROOT" rev-parse --verify 'HEAD^{commit}')"

[[ "$REF" == "$HEAD_COMMIT" ]] || die 'REF must resolve to the current HEAD'
[[ -z "$(git -C "$ROOT" status --porcelain --untracked-files=all)" ]] || \
  die 'source worktree must be clean'
[[ -f "$ALLOWLIST" && ! -L "$ALLOWLIST" ]] || \
  die 'public export allowlist is missing or a symlink'
for command in git python3 tar; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done

[[ "$destination_input" = /* ]] || die 'destination must be absolute'
[[ ! -L "$destination_input" ]] || die 'destination must not be a symlink'
destination_parent="$(cd -- "$(dirname -- "$destination_input")" 2>/dev/null && pwd -P)" || \
  die 'destination parent must already exist'
destination_name="$(basename -- "$destination_input")"
[[ -n "$destination_name" && "$destination_name" != "/" ]] || \
  die 'destination name is invalid'
readonly DESTINATION="${destination_parent}/${destination_name}"
case "$DESTINATION" in
  "$ROOT"|"$ROOT"/*) die 'destination must be outside the source checkout' ;;
esac

destination_created=false
if [[ -e "$DESTINATION" ]]; then
  [[ -d "$DESTINATION" && -z "$(find "$DESTINATION" -mindepth 1 -maxdepth 1 -print -quit)" ]] || {
    printf 'ERROR: destination must not exist or must be empty\n' >&2
    exit 2
  }
else
  mkdir "$DESTINATION"
  destination_created=true
fi

completed=false
cleanup() {
  exit_code=$?
  trap - EXIT
  if [[ "$completed" != true ]]; then
    find "$DESTINATION" -depth -mindepth 1 -delete 2>/dev/null || true
    if [[ "$destination_created" == true ]]; then
      rmdir -- "$DESTINATION" 2>/dev/null || true
    fi
  fi
  exit "$exit_code"
}
trap cleanup EXIT

tracked=()
while IFS= read -r path; do tracked+=("$path"); done < <(git -C "$ROOT" ls-tree -r --name-only "$REF")
allowed=()
while IFS= read -r rule; do allowed+=("$rule"); done < <(sed -e 's/[[:space:]]*$//' -e '/^#/d' -e '/^$/d' "$ALLOWLIST")
selected=()
for path in "${tracked[@]}"; do
  for rule in "${allowed[@]}"; do
    if [[ "$rule" == */ && "$path" == "$rule"* ]] || [[ "$path" == "$rule" ]]; then
      case "$path" in
        pilot/tests/test_platform_operations_docs.py) ;;
        *) selected+=("$path") ;;
      esac
      break
    fi
  done
done

(("${#selected[@]}" > 0)) || {
  printf 'ERROR: allowlist selected no files\n' >&2
  exit 2
}
git -C "$ROOT" archive --format=tar "$REF" -- "${selected[@]}" |
  tar -xf - -C "$DESTINATION"
"$DESTINATION/tools/public-hygiene.sh" "$DESTINATION"
completed=true
printf 'public_export=%s files=%d commit=%s\n' "$DESTINATION" "${#selected[@]}" "$REF"

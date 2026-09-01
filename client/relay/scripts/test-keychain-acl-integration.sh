#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "${CI:-}" == "true" ]] || \
  die 'temporary Keychain integration is restricted to an ephemeral CI runner'
[[ -n "${RUNNER_TEMP:-}" && "$RUNNER_TEMP" = /* && -d "$RUNNER_TEMP" ]] || \
  die 'RUNNER_TEMP must name an existing absolute directory'

trim_keychain_output() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value#\"}"
  value="${value%\"}"
  printf '%s\n' "$value"
}

default_before="$(trim_keychain_output "$(security default-keychain -d user)")"
[[ -n "$default_before" ]] || die 'current user default Keychain is unavailable'

search_list_before=()
while IFS= read -r line; do
  keychain="$(trim_keychain_output "$line")"
  [[ -n "$keychain" ]] && search_list_before+=("$keychain")
done < <(security list-keychains -d user)
((${#search_list_before[@]} > 0)) || die 'current user Keychain search list is empty'

test_root="${RUNNER_TEMP}/opencodex-keychain-acl.${GITHUB_RUN_ID:-local}.${GITHUB_RUN_ATTEMPT:-0}"
default_keychain="${test_root}/default.keychain-db"
secondary_keychain="${test_root}/secondary.keychain-db"
password="$(openssl rand -hex 24)"
mkdir -m 0700 -- "$test_root"

cleanup() {
  exit_code=$?
  trap - EXIT INT TERM
  set +e
  cleanup_failed=false
  security list-keychains -d user -s "${search_list_before[@]}" >/dev/null 2>&1 || \
    cleanup_failed=true
  security default-keychain -d user -s "$default_before" >/dev/null 2>&1 || \
    cleanup_failed=true

  restored_default="$(trim_keychain_output "$(security default-keychain -d user 2>/dev/null)")"
  [[ "$restored_default" == "$default_before" ]] || cleanup_failed=true
  restored_search_list=()
  while IFS= read -r line; do
    keychain="$(trim_keychain_output "$line")"
    [[ -n "$keychain" ]] && restored_search_list+=("$keychain")
  done < <(security list-keychains -d user 2>/dev/null)
  if ((${#restored_search_list[@]} != ${#search_list_before[@]})); then
    cleanup_failed=true
  else
    for ((index = 0; index < ${#search_list_before[@]}; index += 1)); do
      [[ "${restored_search_list[$index]}" == "${search_list_before[$index]}" ]] || \
        cleanup_failed=true
    done
  fi

  security delete-keychain "$default_keychain" >/dev/null 2>&1 || cleanup_failed=true
  security delete-keychain "$secondary_keychain" >/dev/null 2>&1 || cleanup_failed=true
  find "$test_root" -depth -mindepth 1 -delete >/dev/null 2>&1 || cleanup_failed=true
  rmdir -- "$test_root" >/dev/null 2>&1 || cleanup_failed=true
  if [[ "$cleanup_failed" == true ]]; then
    printf 'ERROR: temporary Keychain integration did not restore user Keychain state\n' >&2
    exit 1
  fi
  exit "$exit_code"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

security create-keychain -p "$password" "$default_keychain"
security create-keychain -p "$password" "$secondary_keychain"
security set-keychain-settings -lut 21600 "$default_keychain"
security set-keychain-settings -lut 21600 "$secondary_keychain"
security unlock-keychain -p "$password" "$default_keychain"
security unlock-keychain -p "$password" "$secondary_keychain"
security list-keychains -d user -s "$default_keychain" "$secondary_keychain"
security default-keychain -d user -s "$default_keychain"

current_default="$(trim_keychain_output "$(security default-keychain -d user)")"
[[ "$current_default" == "$default_keychain" ]] || \
  die 'temporary default Keychain was not selected'

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
package_dir="$(cd -- "$script_dir/../macos/OpenCodexRelay" && pwd -P)"
cd -- "$package_dir"
OPENCODEX_RUN_KEYCHAIN_INTEGRATION=1 \
OPENCODEX_KEYCHAIN_INTEGRATION_SECONDARY_PATH="$secondary_keychain" \
  swift test \
    --filter GatewaySettingsTests/testSystemKeychainAdapterCreatesAndReplacesTemporaryACLItems

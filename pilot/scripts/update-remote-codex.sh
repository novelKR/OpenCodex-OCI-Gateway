#!/usr/bin/env bash
# Update the dedicated Codex standalone package without letting the official
# installer replace the user-facing Remote Control launcher.

set -Eeuo pipefail

readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_FILE="/home/ubuntu/.config/opencodex-relay/remote-opencodex.json"
readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"
readonly MANAGED_CODEX="${REMOTE_HOME_PATH}/packages/standalone/current/codex"
readonly CURRENT_LINK="${REMOTE_HOME_PATH}/packages/standalone/current"
readonly CATALOG="${REMOTE_HOME_PATH}/opencodex-catalog.json"
readonly RESTART_PENDING="${REMOTE_HOME_PATH}/catalog-restart-pending"
readonly RELAY_RESTART_PENDING="${CATALOG}.restart-pending"
readonly INSTALL_ROOT="/home/ubuntu/.local/lib/opencodex-relay"
readonly MANAGER="${INSTALL_ROOT}/manage-remote-codex-home.sh"
readonly WRAPPER_TARGET="/home/ubuntu/.local/bin/codex"
readonly INSTALLER_BIN_DIR="${INSTALL_ROOT}/codex-installer-bin"
readonly BACKUP_ROOT="${REMOTE_HOME_PATH}/.upgrade-backups"
readonly INSTALLER_URL="https://chatgpt.com/codex/install.sh"

before_target=""
before_version=""
backup_dir=""
installer=""
rollback_required=false

usage() {
  cat <<'USAGE'
Usage:
  update-remote-codex.sh status
  update-remote-codex.sh apply --allow-remote-interruption

apply downloads the official standalone Codex installer into the dedicated
CODEX_HOME. It may restart the managed app-server and disconnect active Remote
Control work, so the acknowledgement flag is required. The official installer
does not document a version-pin input; this path records before/after versions
and restores the previous standalone link and catalog if verification fails.
USAGE
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_owned_regular_file() {
  local path="$1"
  local expected_mode="$2"
  [[ -f "${path}" && ! -L "${path}" ]] || \
    die "required regular file is unavailable: ${path}"
  [[ "$(stat -c '%U:%G:%a' "${path}")" == "ubuntu:ubuntu:${expected_mode}" ]] || \
    die "${path} must be owned by ubuntu with mode ${expected_mode}"
}

require_layout() {
  [[ "$(id -un)" == "ubuntu" ]] || die "run this script as ubuntu, without sudo"
  require_owned_regular_file "${CONFIG_FILE}" 600
  require_owned_regular_file "${MANAGER}" 700
  [[ -d "${REMOTE_HOME_PATH}" && ! -L "${REMOTE_HOME_PATH}" ]] || \
    die "remote Codex home is unavailable: ${REMOTE_HOME_PATH}"
  [[ -L "${CURRENT_LINK}" && -x "${MANAGED_CODEX}" ]] || \
    die "managed standalone Codex is unavailable: ${MANAGED_CODEX}"
  [[ -x "${WRAPPER_TARGET}" ]] || \
    die "managed Codex wrapper is unavailable: ${WRAPPER_TARGET}"
  [[ -f "${CATALOG}" && ! -L "${CATALOG}" ]] || \
    die "remote catalog is unavailable: ${CATALOG}"
  for command_name in curl install jq mktemp mv readlink stat timeout; do
    command -v "${command_name}" >/dev/null || die "required command is missing: ${command_name}"
  done
  export CODEX_HOME="${REMOTE_HOME_PATH}"
}

codex_version() {
  "${MANAGED_CODEX}" --version | awk '{print $NF}'
}

routing_mode() {
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || \
    die "Remote configuration loader is unavailable: $CONFIG_LOADER"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"
  printf '%s\n' "$ROUTING_MODE"
}

verify_catalog() {
  local output
  local count
  output="$(mktemp "${REMOTE_HOME_PATH}/.debug-models.XXXXXX")"
  "${WRAPPER_TARGET}" debug models > "${output}"
  jq -e '.models | type == "array" and length > 0' "${output}" >/dev/null
  count="$(jq '.models | length' "${output}")"
  rm -f "${output}"
  printf 'effective_catalog_models=%s\n' "${count}"
}

verify_daemon() {
  local output
  local expected
  output="$(mktemp "${REMOTE_HOME_PATH}/.daemon-version.XXXXXX")"
  "${WRAPPER_TARGET}" app-server daemon version > "${output}"
  expected="$(codex_version)"
  jq -e --arg expected "${expected}" '
    .status == "running"
    and .managedCodexVersion == $expected
    and .cliVersion == $expected
    and .appServerVersion == $expected
  ' "${output}" >/dev/null
  rm -f "${output}"
  printf 'managed_app_server=running version=%s\n' "${expected}"
}

verify_proxy() {
  local output
  local proxy_status
  output="$(mktemp "${REMOTE_HOME_PATH}/.app-server-proxy.XXXXXX")"
  set +e
  printf 'GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n' | \
    timeout 10 "${WRAPPER_TARGET}" app-server proxy > "${output}" 2>&1
  proxy_status=$?
  set -e
  if ! grep -q '^HTTP/1.1 101 ' "${output}"; then
    rm -f "${output}"
    die "app-server proxy did not complete a WebSocket handshake (status ${proxy_status})"
  fi
  rm -f "${output}"
  printf 'app_server_proxy=websocket_101\n'
}

restore_current_link() {
  local restore_dir
  [[ -n "${before_target}" ]] || return 1
  restore_dir="$(mktemp -d "${REMOTE_HOME_PATH}/.restore-current.XXXXXX")"
  ln -s "${before_target}" "${restore_dir}/current"
  mv -Tf "${restore_dir}/current" "${CURRENT_LINK}"
  rmdir "${restore_dir}"
}

rollback() {
  local rollback_failed=false

  printf 'Codex update failed; restoring the previous standalone release.\n' >&2
  restore_current_link || rollback_failed=true
  if [[ -f "${backup_dir}/opencodex-catalog.json" && ! -L "${backup_dir}/opencodex-catalog.json" ]]; then
    install -m 0600 "${backup_dir}/opencodex-catalog.json" "${CATALOG}" || rollback_failed=true
  else
    rollback_failed=true
  fi
  "${MANAGER}" repair-wrapper >/dev/null 2>&1 || rollback_failed=true
  "${MANAGER}" restart-daemon >/dev/null 2>&1 || rollback_failed=true
  "${WRAPPER_TARGET}" app-server daemon enable-remote-control >/dev/null 2>&1 || rollback_failed=true
  "${MANAGER}" repair-wrapper >/dev/null 2>&1 || rollback_failed=true
  if [[ "${rollback_failed}" == "true" ]]; then
    printf 'CRITICAL: rollback could not be fully verified; inspect %s.\n' "${backup_dir}" >&2
    return 1
  fi
  printf 'Previous Codex standalone release, catalog, and Remote Control daemon were restored.\n' >&2
}

cleanup() {
  local status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  [[ -z "${installer}" ]] || rm -f "${installer}"
  if [[ "${status}" -ne 0 && "${rollback_required}" == "true" ]]; then
    rollback || status=70
  fi
  exit "${status}"
}

status() {
  "${MANAGER}" status
  printf 'managed_codex_version=%s\n' "$(codex_version)"
  if cmp -s "${WRAPPER_TARGET}" "${INSTALL_ROOT}/codex-remote-home-wrapper.sh"; then
    printf 'wrapper_source_match=1\n'
  else
    printf 'wrapper_source_match=0\n'
  fi
}

apply_update() {
  local after_version
  local current_routing
  local restart_required=false

  before_target="$(readlink "${CURRENT_LINK}")"
  [[ -n "${before_target}" ]] || die "could not read the current standalone release link"
  before_version="$(codex_version)"
  [[ "${before_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || \
    die "current Codex version is not an explicit semver: ${before_version}"
  current_routing="$(routing_mode)"
  status
  if [[ "${current_routing}" == "relay" || "${current_routing}" == "local-relay" ]]; then
    "${MANAGER}" verify-interactive-profile
    "${MANAGER}" verify-relay-health
  fi
  # Local-relay preserves a root selected by Codex/AppServer when it is either
  # the exact bounded policy model or one exact catalog-visible passthrough
  # model. Verify that classification without rewriting it during an unrelated
  # Codex update; other modes retain managed-default correction.
  if [[ "${current_routing}" == "local-relay" ]]; then
    "${MANAGER}" verify-default-model
  else
    "${MANAGER}" set-default-model --allow-remote-interruption
  fi

  install -d -m 0700 "${BACKUP_ROOT}"
  backup_dir="$(mktemp -d "${BACKUP_ROOT}/upgrade-${before_version}.XXXXXX")"
  cp -p "${CATALOG}" "${backup_dir}/opencodex-catalog.json"
  chmod 0600 "${backup_dir}/opencodex-catalog.json"
  rollback_required=true
  trap cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM

  install -d -m 0700 "${INSTALLER_BIN_DIR}"
  installer="$(mktemp "${REMOTE_HOME_PATH}/.codex-install.XXXXXX")"
  curl --fail --location --silent --show-error --max-time 60 "${INSTALLER_URL}" -o "${installer}"
  CODEX_HOME="${REMOTE_HOME_PATH}" \
    CODEX_INSTALL_DIR="${INSTALLER_BIN_DIR}" \
  CODEX_NON_INTERACTIVE=1 \
    sh "${installer}"
  rm -f "${installer}"
  installer=""

  after_version="$(codex_version)"
  [[ "${after_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || \
    die "updated Codex version is not an explicit semver: ${after_version}"
  if [[ "${after_version}" != "${before_version}" ]]; then
    restart_required=true
  fi
  "${MANAGER}" repair-wrapper
  "${MANAGER}" refresh
  if [[ -f "${RESTART_PENDING}" || -f "${RELAY_RESTART_PENDING}" ]]; then
    restart_required=true
  fi
  if [[ "${restart_required}" == "true" ]]; then
    "${MANAGER}" restart-daemon
  fi
  "${WRAPPER_TARGET}" app-server daemon enable-remote-control
  "${MANAGER}" repair-wrapper
  verify_catalog
  "${MANAGER}" verify-default-model
  if [[ "${current_routing}" == "relay" || "${current_routing}" == "local-relay" ]]; then
    "${MANAGER}" verify-interactive-profile
    "${MANAGER}" verify-relay-health
  fi
  verify_daemon
  verify_proxy

  rollback_required=false
  printf 'Codex standalone update completed: %s -> %s. Backup retained at %s.\n' \
    "${before_version}" "${after_version}" "${backup_dir}"
}

action="${1:-}"
case "${action}" in
  status)
    [[ "$#" -eq 1 ]] || { usage; exit 2; }
    require_layout
    status
    ;;
  apply)
    [[ "$#" -eq 2 && "${2:-}" == "--allow-remote-interruption" ]] || {
      usage
      exit 2
    }
    require_layout
    apply_update
    ;;
  *)
    usage
    exit 2
    ;;
esac

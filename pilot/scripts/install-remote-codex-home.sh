#!/usr/bin/env bash
# Install or refresh the non-secret automation assets for the dedicated Remote
# Control Codex home. Configuration, auth.json, and data-plane credentials are
# intentionally pre-existing prerequisites and are never copied by this script.

set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PILOT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly REPO_ROOT="$(cd -- "${PILOT_ROOT}/.." && pwd -P)"
readonly SYSTEMD_SOURCE_DIR="${PILOT_ROOT}/systemd"
readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"
readonly CONFIG_FILE="${CONFIG_DIR}/remote-opencodex.json"
readonly INSTALL_ROOT="/home/ubuntu/.local/lib/opencodex-relay"
readonly USER_SYSTEMD_DIR="/home/ubuntu/.config/systemd/user"

readonly CONFIG_LOADER_SOURCE="${SCRIPT_DIR}/load-remote-config.sh"
readonly MANAGER_SOURCE="${SCRIPT_DIR}/manage-remote-codex-home.sh"
readonly WRAPPER_SOURCE="${SCRIPT_DIR}/codex-remote-home-wrapper.sh"
readonly UPDATER_SOURCE="${SCRIPT_DIR}/update-remote-codex.sh"
readonly ROUTING_SOURCE="${SCRIPT_DIR}/configure-remote-codex-routing.sh"
readonly REMOTE_RELAY_SOURCE="${SCRIPT_DIR}/install-remote-codex-relay.sh"
readonly RELAY_INSTALLER_SOURCE="${REPO_ROOT}/client/relay/scripts/install-relay.sh"
readonly RELAY_SERVICE_SOURCE="${REPO_ROOT}/client/relay/scripts/install-service.sh"
readonly RELAY_SYSTEMD_SOURCE="${REPO_ROOT}/client/relay/systemd/opencodex-relay.service.in"
readonly CONFIG_LOADER_TARGET="${INSTALL_ROOT}/load-remote-config.sh"
readonly MANAGER_TARGET="${INSTALL_ROOT}/manage-remote-codex-home.sh"
readonly WRAPPER_TARGET="${INSTALL_ROOT}/codex-remote-home-wrapper.sh"
readonly UPDATER_TARGET="${INSTALL_ROOT}/update-remote-codex.sh"
readonly ROUTING_TARGET="${INSTALL_ROOT}/configure-remote-codex-routing.sh"
readonly REMOTE_RELAY_TARGET="${INSTALL_ROOT}/install-remote-codex-relay.sh"
readonly RELAY_INSTALLER_DIR="${INSTALL_ROOT}/relay-installer"
readonly RELAY_INSTALLER_TARGET="${RELAY_INSTALLER_DIR}/install-relay.sh"
readonly RELAY_SERVICE_TARGET="${RELAY_INSTALLER_DIR}/install-service.sh"
readonly RELAY_SYSTEMD_DIR="${INSTALL_ROOT}/systemd"
readonly RELAY_SYSTEMD_TARGET="${RELAY_SYSTEMD_DIR}/opencodex-relay.service.in"

routing_mode=""

usage() {
  cat <<'USAGE'
Usage:
  install-remote-codex-home.sh install [--bootstrap-remote-control] [--with-relay-bootstrap]

Run as ubuntu, without sudo. The base path works from a pilot/scripts checkout
and installs only reviewed executable and user-systemd assets. Add
--with-relay-bootstrap only from a complete repository checkout to install the
non-secret GitHub Release relay installer and Remote relay wrapper. It will not create or
copy auth.json, remote-opencodex.json, credentials.env, a GitHub token, or a release public key.
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

require_source_file() {
  local path="$1"
  [[ -f "${path}" && ! -L "${path}" ]] || \
    die "installer source is missing or unsafe: ${path}"
}

load_routing_mode() {
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER_SOURCE"
  load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"
  routing_mode="$ROUTING_MODE"
}

require_prerequisites() {
  [[ "$(id -un)" == "ubuntu" ]] || die "run this script as ubuntu, without sudo"
  [[ -d "${REMOTE_HOME_PATH}" && ! -L "${REMOTE_HOME_PATH}" ]] || \
    die "remote Codex home is unavailable: ${REMOTE_HOME_PATH}"
  require_owned_regular_file "${CONFIG_FILE}" 600
  require_owned_regular_file "${REMOTE_HOME_PATH}/auth.json" 600
  require_source_file "${CONFIG_LOADER_SOURCE}"
  require_source_file "${MANAGER_SOURCE}"
  require_source_file "${WRAPPER_SOURCE}"
  require_source_file "${UPDATER_SOURCE}"
  require_source_file "${ROUTING_SOURCE}"
  if [[ "${relay_bootstrap}" == true ]]; then
    require_source_file "${REMOTE_RELAY_SOURCE}"
    require_source_file "${RELAY_INSTALLER_SOURCE}"
    require_source_file "${RELAY_SERVICE_SOURCE}"
    require_source_file "${RELAY_SYSTEMD_SOURCE}"
  fi
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-catalog-refresh.service"
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-catalog-refresh.timer"
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-relay-catalog-activation.service"
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-relay-catalog-activation.timer"
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-codex-wrapper-repair.service"
  require_source_file "${SYSTEMD_SOURCE_DIR}/opencodex-remote-codex-wrapper-repair.path"
  command -v install >/dev/null || die "required command is missing: install"
  command -v systemctl >/dev/null || die "required command is missing: systemctl"
  load_routing_mode
}

install_assets() {
  install -d -m 0700 "${INSTALL_ROOT}" "${USER_SYSTEMD_DIR}"
  install -m 0600 "${CONFIG_LOADER_SOURCE}" "${CONFIG_LOADER_TARGET}"
  install -m 0700 "${MANAGER_SOURCE}" "${MANAGER_TARGET}"
  install -m 0700 "${WRAPPER_SOURCE}" "${WRAPPER_TARGET}"
  install -m 0700 "${UPDATER_SOURCE}" "${UPDATER_TARGET}"
  install -m 0700 "${ROUTING_SOURCE}" "${ROUTING_TARGET}"
  if [[ "${relay_bootstrap}" == true ]]; then
    install -d -m 0700 "${RELAY_INSTALLER_DIR}" "${RELAY_SYSTEMD_DIR}"
    install -m 0700 "${REMOTE_RELAY_SOURCE}" "${REMOTE_RELAY_TARGET}"
    install -m 0700 "${RELAY_INSTALLER_SOURCE}" "${RELAY_INSTALLER_TARGET}"
    install -m 0700 "${RELAY_SERVICE_SOURCE}" "${RELAY_SERVICE_TARGET}"
    install -m 0644 "${RELAY_SYSTEMD_SOURCE}" "${RELAY_SYSTEMD_TARGET}"
  fi
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-catalog-refresh.service" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-catalog-refresh.service"
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-catalog-refresh.timer" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-catalog-refresh.timer"
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-relay-catalog-activation.service" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-relay-catalog-activation.service"
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-relay-catalog-activation.timer" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-relay-catalog-activation.timer"
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-codex-wrapper-repair.service" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-codex-wrapper-repair.service"
  install -m 0644 \
    "${SYSTEMD_SOURCE_DIR}/opencodex-remote-codex-wrapper-repair.path" \
    "${USER_SYSTEMD_DIR}/opencodex-remote-codex-wrapper-repair.path"
  if [[ "${routing_mode}" == "relay" || "${routing_mode}" == "local-relay" ]]; then
    "${MANAGER_TARGET}" ensure-interactive-profile
  fi
  systemctl --user daemon-reload
  if [[ "${routing_mode}" == "relay" ]]; then
    systemctl --user enable --now opencodex-remote-relay-catalog-activation.timer
    systemctl --user disable --now opencodex-remote-catalog-refresh.timer
  else
    # legacy and local-relay keep the Remote manager as catalog writer and
    # activator. local-relay changes only the Native Codex data path.
    systemctl --user enable --now opencodex-remote-catalog-refresh.timer
    systemctl --user disable --now opencodex-remote-relay-catalog-activation.timer
  fi
  systemctl --user enable --now opencodex-remote-codex-wrapper-repair.path
  if [[ "${routing_mode}" == "relay" ]]; then
    # Enabling a Persistent timer on a long-lived login may immediately apply
    # an existing relay marker and restart the daemon. Wait for that one-shot
    # job before the installer performs its final status read.
    systemctl --user start opencodex-remote-relay-catalog-activation.service
  fi
  "${MANAGER_TARGET}" verify-default-model
  "${MANAGER_TARGET}" repair-wrapper
}

action="${1:-}"
bootstrap_remote_control=false
relay_bootstrap=false
[[ "${action}" == "install" ]] || { usage; exit 2; }
shift
while [[ "$#" -gt 0 ]]; do
  case "$1" in
    --bootstrap-remote-control) bootstrap_remote_control=true ;;
    --with-relay-bootstrap) relay_bootstrap=true ;;
    *) usage; exit 2 ;;
  esac
  shift
done

require_prerequisites
if [[ "${routing_mode}" == "relay" || "${routing_mode}" == "local-relay" ]]; then
  # Refuse an existing same-name user profile before replacing any installed
  # automation asset. Only the exact marker-owned profile is managed here.
  bash "${MANAGER_SOURCE}" check-interactive-profile-ownership >/dev/null
fi
install_assets
if [[ "${bootstrap_remote_control}" == "true" ]]; then
  "${MANAGER_TARGET}" bootstrap-remote-control
fi
"${MANAGER_TARGET}" verify-daemon
"${MANAGER_TARGET}" status
printf 'routing_mode=%s catalog_timer=%s relay_catalog_activation_timer=%s wrapper_repair_path=%s\n' \
  "${routing_mode}" \
  "$(systemctl --user is-active opencodex-remote-catalog-refresh.timer)" \
  "$(systemctl --user is-active opencodex-remote-relay-catalog-activation.timer)" \
  "$(systemctl --user is-active opencodex-remote-codex-wrapper-repair.path)"

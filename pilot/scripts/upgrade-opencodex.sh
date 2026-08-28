#!/usr/bin/env bash
# Upgrade the centrally managed OpenCodex package without changing its service
# identity, configuration, gateway boundary, or deployment version contract.

set -Eeuo pipefail

readonly PACKAGE_NAME="@bitkyc08/opencodex"
readonly SERVICE_NAME="opencodex.service"
readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"
readonly BACKUP_ROOT="/var/backups/opencodex"
readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"
readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SMOKE_TEST="${SCRIPT_DIR}/smoke-test.sh"
readonly EXPECTED_SMOKE_TEST_SHA256="44d5df7812a2e174ef14c26902d36a3c08a1a08f3d8d289791f6a9aa012b4fbf"
readonly OCX_HEALTH_URL="http://127.0.0.1:10100/healthz"
readonly ROLLBACK_HEALTH_ATTEMPTS=60

target_version=""
skip_smoke=false
service_was_active=false
service_was_enabled=false
service_active_state=""
rollback_required=false
backup_dir=""
current_version=""
opencodex_home=""
opencodex_prefix=""
package_manifest=""

usage() {
  cat <<'USAGE'
Usage:
  sudo ./upgrade-opencodex.sh check VERSION
  sudo ./upgrade-opencodex.sh apply VERSION [--skip-smoke]
  sudo ./upgrade-opencodex.sh adopt-current VERSION

VERSION must be an explicit stable or prerelease semver (for example 2.10.1 or
2.9.0-preview.1). This managed deployment intentionally rejects npm tags such
as latest and preview.

apply preserves the runtime contract's current prefix in /var/backups/opencodex,
restores it if installation, config validation, service startup, or the default
local smoke test fails, and only then records VERSION as the expected release.
--skip-smoke is accepted only when opencodex.service was already intentionally
stopped before the command; an active service must complete the managed smoke.

If apply finds VERSION already installed, it does not mutate the package. It
instead performs the same controlled state-adoption checks as adopt-current and
records VERSION only when the installed package metadata, active enabled service,
and local health endpoint agree. adopt-current skips the npm registry lookup and
is intended only for an already reviewed, running deployment that predates the
expected-version state file.
USAGE
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

require_explicit_version() {
  [[ "${target_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || \
    die "VERSION must be an explicit semver, not a mutable npm tag"
}

require_safe_root_file() {
  local path="$1"
  local expected_mode="$2"
  local label="$3"
  [[ -f "${path}" && ! -L "${path}" ]] || \
    die "${label} is missing or unsafe: ${path}"
  [[ "$(stat -c '%U:%G:%a' "${path}")" == "root:root:${expected_mode}" ]] || \
    die "${label} must be root:root ${expected_mode}: ${path}"
}

verify_companion_smoke() {
  local actual
  [[ -f "${SMOKE_TEST}" && ! -L "${SMOKE_TEST}" ]] || \
    die "local smoke test is missing or unsafe: ${SMOKE_TEST}"
  actual="$(sha256sum "${SMOKE_TEST}" | awk '{print $1}')"
  [[ "${actual}" == "${EXPECTED_SMOKE_TEST_SHA256}" ]] || \
    die "local smoke test SHA-256 mismatch: ${SMOKE_TEST}"
}

load_runtime_contract() {
  local description
  require_safe_root_file "${RUNTIME_ADAPTER}" 755 "runtime adapter"
  [[ -x "${RUNTIME_ADAPTER}" ]] || die "runtime adapter is not executable: ${RUNTIME_ADAPTER}"
  require_safe_root_file "${RUNTIME_CONFIG}" 644 "runtime contract"
  "${RUNTIME_ADAPTER}" check >/dev/null
  description="$("${RUNTIME_ADAPTER}" describe --json)" || \
    die "runtime adapter description could not be read"
  opencodex_home="$(jq -er '.home | select(type == "string" and length > 0)' <<< "${description}")" || \
    die "runtime adapter description has no valid home"
  opencodex_prefix="$(jq -er '.prefix | select(type == "string" and length > 0)' <<< "${description}")" || \
    die "runtime adapter description has no valid prefix"
  package_manifest="$(jq -er '.package_manifest | select(type == "string" and length > 0)' <<< "${description}")" || \
    die "runtime adapter description has no valid package manifest"
  [[ -d "${opencodex_prefix}" && ! -L "${opencodex_prefix}" ]] || \
    die "OpenCodex prefix is missing or unsafe: ${opencodex_prefix}"
  [[ -f "${package_manifest}" && ! -L "${package_manifest}" ]] || \
    die "OpenCodex package manifest is missing or unsafe: ${package_manifest}"
}

require_root_and_layout() {
  [[ "${EUID}" -eq 0 ]] || die "run as root"
  for command_name in awk cmp cp curl dirname install jq mktemp mv rm runuser sha256sum sleep stat systemctl; do
    command -v "${command_name}" >/dev/null || die "required command is missing: ${command_name}"
  done
  verify_companion_smoke
  load_runtime_contract
}

installed_version() {
  local runtime_version
  runtime_version="$(runuser -u opencodex -- \
    "${RUNTIME_ADAPTER}" ocx --version 2>/dev/null)" || return 1
  [[ "${runtime_version}" =~ ^opencodex[[:space:]]+([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?)$ ]] || \
    return 1
  printf '%s\n' "${BASH_REMATCH[1]}"
}

validate_installed_version() {
  local expected="$1"
  local actual
  actual="$(installed_version)" || \
    die "installed OpenCodex version could not be read from runtime or package metadata"
  [[ "${actual}" == "${expected}" ]] || \
    die "installed OpenCodex version is ${actual:-unavailable}, expected ${expected}"
}

validate_config() {
  runuser -u opencodex -- "${RUNTIME_ADAPTER}" ocx config validate
}

installed_package_version() {
  jq -er --arg package_name "${PACKAGE_NAME}" '
    if .name == $package_name and (.version | type == "string") then
      .version
    else
      empty
    end
  ' "${package_manifest}"
}

validate_installed_package_metadata() {
  local expected="$1"
  local actual
  actual="$(installed_package_version)" || \
    die "OpenCodex package metadata could not be read from ${package_manifest}"
  [[ "${actual}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || \
    die "installed package metadata is not an explicit semver: ${actual:-unavailable}"
  [[ "${actual}" == "${expected}" ]] || \
    die "installed package metadata is ${actual:-unavailable}, expected ${expected}"
}

verify_local_health_version() {
  local expected="$1"
  local failure_message="$2"
  local health
  health="$(curl --fail --silent --show-error --max-time 5 "${OCX_HEALTH_URL}")" || \
    die "${failure_message}"
  jq -e --arg version "${expected}" '
    .service == "opencodex"
    and .status == "ok"
    and .version == $version
  ' <<< "${health}" >/dev/null || die "${failure_message}"
}

verify_adoptable_running_state() {
  local expected="$1"
  systemctl is-active --quiet "${SERVICE_NAME}" || \
    die "${SERVICE_NAME} is not active; refusing to adopt deployment state"
  systemctl is-enabled --quiet "${SERVICE_NAME}" || \
    die "${SERVICE_NAME} is not enabled; refusing to adopt deployment state"
  verify_local_health_version "${expected}" \
    "OpenCodex local health identity/status/version is invalid; refusing to adopt deployment state"
}

verify_registry_version() {
  local available
  available="$(runuser -u opencodex -- \
    "${RUNTIME_ADAPTER}" npm view "${PACKAGE_NAME}@${target_version}" version --json | \
    jq -er 'if type == "array" then .[-1] else . end')"
  [[ "${available}" == "${target_version}" ]] || \
    die "npm registry did not resolve ${PACKAGE_NAME}@${target_version} exactly"
}

write_expected_version() {
  local version="$1"
  local candidate
  # runtime.json is public deployment metadata and must remain traversable by
  # the opencodex service account. Credentials in this directory stay 0600.
  install -d -o root -g root -m 0755 /etc/opencodex
  if [[ -e "${EXPECTED_VERSION_FILE}" || -L "${EXPECTED_VERSION_FILE}" ]]; then
    [[ -f "${EXPECTED_VERSION_FILE}" && ! -L "${EXPECTED_VERSION_FILE}" ]] || \
      die "expected-version state is unsafe: ${EXPECTED_VERSION_FILE}"
  fi
  candidate="$(mktemp /etc/opencodex/.expected-version.XXXXXX)"
  printf '%s\n' "${version}" > "${candidate}"
  chown root:root "${candidate}"
  chmod 0644 "${candidate}"
  mv -f "${candidate}" "${EXPECTED_VERSION_FILE}"
}

snapshot_expected_version() {
  if [[ -e "${EXPECTED_VERSION_FILE}" || -L "${EXPECTED_VERSION_FILE}" ]]; then
    [[ -f "${EXPECTED_VERSION_FILE}" && ! -L "${EXPECTED_VERSION_FILE}" ]] || \
      die "expected-version state is unsafe: ${EXPECTED_VERSION_FILE}"
    printf 'present\n' > "${backup_dir}/expected-version.state"
    cp -a -- "${EXPECTED_VERSION_FILE}" "${backup_dir}/expected-version"
    stat -c '%u:%g:%a' "${EXPECTED_VERSION_FILE}" > "${backup_dir}/expected-version.metadata"
  else
    printf 'absent\n' > "${backup_dir}/expected-version.state"
  fi
  chmod 0600 "${backup_dir}/expected-version.state"
  [[ ! -e "${backup_dir}/expected-version.metadata" ]] || \
    chmod 0600 "${backup_dir}/expected-version.metadata"
}

restore_expected_version() {
  local state
  local candidate
  state="$(<"${backup_dir}/expected-version.state")" || return 1
  case "${state}" in
    present)
      [[ -f "${backup_dir}/expected-version" && \
         ! -L "${backup_dir}/expected-version" ]] || return 1
      install -d -o root -g root -m 0755 "$(dirname -- "${EXPECTED_VERSION_FILE}")" || \
        return 1
      candidate="$(mktemp "$(dirname -- "${EXPECTED_VERSION_FILE}")/.expected-version.rollback.XXXXXX")" || \
        return 1
      rm -f -- "${candidate}" || return 1
      cp -a -- "${backup_dir}/expected-version" "${candidate}" || return 1
      mv -f -- "${candidate}" "${EXPECTED_VERSION_FILE}" || return 1
      ;;
    absent)
      rm -f -- "${EXPECTED_VERSION_FILE}" || return 1
      ;;
    *)
      return 1
      ;;
  esac
}

verify_restored_expected_version() {
  local state
  local expected_metadata
  state="$(<"${backup_dir}/expected-version.state")" || return 1
  case "${state}" in
    present)
      [[ -f "${EXPECTED_VERSION_FILE}" && ! -L "${EXPECTED_VERSION_FILE}" ]] || return 1
      cmp -s -- "${backup_dir}/expected-version" "${EXPECTED_VERSION_FILE}" || return 1
      expected_metadata="$(<"${backup_dir}/expected-version.metadata")" || return 1
      [[ "$(stat -c '%u:%g:%a' "${EXPECTED_VERSION_FILE}")" == "${expected_metadata}" ]] || \
        return 1
      ;;
    absent)
      [[ ! -e "${EXPECTED_VERSION_FILE}" && ! -L "${EXPECTED_VERSION_FILE}" ]] || return 1
      ;;
    *)
      return 1
      ;;
  esac
}

adopt_current_state() {
  local version="$1"
  validate_installed_version "${version}"
  validate_installed_package_metadata "${version}"
  validate_config
  verify_adoptable_running_state "${version}"
  write_expected_version "${version}"
  printf 'OpenCodex deployment state adopted at %s; no package or service mutation was made.\n' \
    "${version}"
}

restore_prefix_owner() {
  [[ -d "${opencodex_prefix}" && ! -L "${opencodex_prefix}" ]] || return 0
  chown -R root:root "${opencodex_prefix}"
}

verify_enabled_state() {
  if [[ "${service_was_enabled}" == "true" ]]; then
    systemctl is-enabled --quiet "${SERVICE_NAME}" || \
      die "${SERVICE_NAME} unexpectedly lost its enabled state"
  elif systemctl is-enabled --quiet "${SERVICE_NAME}"; then
    die "${SERVICE_NAME} unexpectedly became enabled"
  fi
}

read_stable_active_state() {
  local state
  state="$(systemctl show "${SERVICE_NAME}" --property=ActiveState --value 2>/dev/null)" || \
    die "${SERVICE_NAME} ActiveState could not be read"
  case "${state}" in
    active|inactive) printf '%s\n' "${state}" ;;
    failed|activating|deactivating|unknown|"")
      die "${SERVICE_NAME} ActiveState is not stable: ${state:-unknown}"
      ;;
    *)
      die "${SERVICE_NAME} ActiveState is unsupported: ${state}"
      ;;
  esac
}

verify_active_state_unchanged() {
  local current
  current="$(read_stable_active_state)"
  [[ "${current}" == "${service_active_state}" ]] || \
    die "${SERVICE_NAME} ActiveState changed from ${service_active_state} to ${current}; refusing mutation"
}

wait_for_rollback_health() {
  local expected="$1"
  local attempt
  local health
  for ((attempt = 1; attempt <= ROLLBACK_HEALTH_ATTEMPTS; attempt++)); do
    health="$(curl --fail --silent --show-error --max-time 1 \
      "${OCX_HEALTH_URL}" 2>/dev/null || true)"
    if jq -e --arg version "${expected}" '
      .service == "opencodex"
      and .status == "ok"
      and .version == $version
    ' <<< "${health}" >/dev/null 2>&1; then
      return 0
    fi
    if (( attempt < ROLLBACK_HEALTH_ATTEMPTS )); then
      sleep 1
    fi
  done
  return 1
}

rollback() {
  local rollback_failed=false
  local restored_package_version
  local restored_runtime_version

  printf 'Upgrade failed; restoring the previous OpenCodex prefix.\n' >&2
  if [[ "${service_was_active}" == "true" ]]; then
    systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || rollback_failed=true
  elif systemctl is-active --quiet "${SERVICE_NAME}"; then
    systemctl stop "${SERVICE_NAME}" >/dev/null 2>&1 || rollback_failed=true
  fi
  if [[ -e "${opencodex_prefix}" || -L "${opencodex_prefix}" ]]; then
    mv "${opencodex_prefix}" "${backup_dir}/failed-prefix" || rollback_failed=true
  fi
  if [[ -d "${backup_dir}/prefix" && ! -L "${backup_dir}/prefix" ]]; then
    cp -a "${backup_dir}/prefix" "${opencodex_prefix}" || rollback_failed=true
  else
    rollback_failed=true
  fi
  restore_prefix_owner || rollback_failed=true
  restore_expected_version || rollback_failed=true
  verify_restored_expected_version || rollback_failed=true

  "${RUNTIME_ADAPTER}" check >/dev/null 2>&1 || rollback_failed=true
  restored_package_version="$(installed_package_version 2>/dev/null || true)"
  [[ "${restored_package_version}" == "${current_version}" ]] || rollback_failed=true
  restored_runtime_version="$(runuser -u opencodex -- \
    "${RUNTIME_ADAPTER}" ocx --version 2>/dev/null || true)"
  [[ "${restored_runtime_version}" == "opencodex ${current_version}" ]] || rollback_failed=true
  runuser -u opencodex -- \
    "${RUNTIME_ADAPTER}" ocx config validate >/dev/null 2>&1 || rollback_failed=true

  if [[ "${service_was_enabled}" == "true" ]]; then
    if ! systemctl is-enabled --quiet "${SERVICE_NAME}"; then
      systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1 || rollback_failed=true
    fi
  elif systemctl is-enabled --quiet "${SERVICE_NAME}"; then
    systemctl disable "${SERVICE_NAME}" >/dev/null 2>&1 || rollback_failed=true
  fi
  if [[ "${service_was_active}" == "true" ]]; then
    systemctl start "${SERVICE_NAME}" >/dev/null 2>&1 || rollback_failed=true
    systemctl is-active --quiet "${SERVICE_NAME}" || rollback_failed=true
    wait_for_rollback_health "${current_version}" || rollback_failed=true
  elif systemctl is-active --quiet "${SERVICE_NAME}"; then
    rollback_failed=true
  fi
  if [[ "${service_was_enabled}" == "true" ]]; then
    systemctl is-enabled --quiet "${SERVICE_NAME}" || rollback_failed=true
  elif systemctl is-enabled --quiet "${SERVICE_NAME}"; then
    rollback_failed=true
  fi
  if [[ "${rollback_failed}" == "true" ]]; then
    printf 'CRITICAL: rollback could not be fully verified; inspect %s.\n' "${backup_dir}" >&2
    return 1
  fi
  printf 'Previous OpenCodex prefix and service state were restored.\n' >&2
}

cleanup() {
  local status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  if [[ "${status}" -ne 0 && "${rollback_required}" == "true" ]]; then
    rollback || status=70
  else
    restore_prefix_owner || status=70
    if [[ "${status}" -ne 0 && "${service_was_active}" == "true" ]]; then
      systemctl start "${SERVICE_NAME}" >/dev/null 2>&1 || status=70
    fi
  fi
  exit "${status}"
}

parse_arguments() {
  action="${1:-}"
  target_version="${2:-}"
  case "${action}" in
    check)
      [[ "$#" -eq 2 ]] || { usage; exit 2; }
      ;;
    apply)
      case "${3:-}" in
        "") ;;
        --skip-smoke) skip_smoke=true ;;
        *) usage; exit 2 ;;
      esac
      [[ "$#" -eq 2 || "$#" -eq 3 ]] || { usage; exit 2; }
      ;;
    adopt-current)
      [[ "$#" -eq 2 ]] || { usage; exit 2; }
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

action=""
parse_arguments "$@"
require_explicit_version
require_root_and_layout

if [[ "${action}" == "adopt-current" ]]; then
  adopt_current_state "${target_version}"
  exit 0
fi

current_version="$(installed_version)" || \
  die "installed OpenCodex version could not be read from runtime or package metadata"
require_explicit_version_for_current=false
if [[ "${current_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  require_explicit_version_for_current=true
fi
[[ "${require_explicit_version_for_current}" == "true" ]] || \
  die "current OpenCodex version is not an explicit semver: ${current_version:-unavailable}"
validate_installed_package_metadata "${current_version}"

verify_registry_version

if [[ "${action}" == "check" ]]; then
  printf 'installed_version=%s\n' "${current_version}"
  printf 'requested_version=%s\n' "${target_version}"
  printf 'service_active=%s service_enabled=%s\n' \
    "$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || true)" \
    "$(systemctl is-enabled "${SERVICE_NAME}" 2>/dev/null || true)"
  exit 0
fi

if [[ "${current_version}" == "${target_version}" ]]; then
  adopt_current_state "${target_version}"
  exit 0
fi

validate_config
service_active_state="$(read_stable_active_state)"
if [[ "${service_active_state}" == "active" ]]; then
  service_was_active=true
fi
if systemctl is-enabled --quiet "${SERVICE_NAME}"; then
  service_was_enabled=true
fi
if [[ "${service_was_active}" == "true" ]]; then
  verify_local_health_version "${current_version}" \
    "OpenCodex active baseline health identity/status/version is invalid; refusing to upgrade"
fi
if [[ "${skip_smoke}" == "true" && "${service_was_active}" == "true" ]]; then
  die "--skip-smoke is allowed only when ${SERVICE_NAME} was intentionally stopped before the upgrade"
fi
if [[ "${service_was_active}" != "true" && "${skip_smoke}" != "true" ]]; then
  die "${SERVICE_NAME} is not active; use --skip-smoke only for an intentionally stopped host"
fi
verify_active_state_unchanged

install -d -o root -g root -m 0700 "${BACKUP_ROOT}"
backup_dir="$(mktemp -d "${BACKUP_ROOT}/upgrade-${current_version}-to-${target_version}.XXXXXX")"
chmod 0700 "${backup_dir}"
cp -a "${opencodex_prefix}" "${backup_dir}/prefix"
snapshot_expected_version
verify_active_state_unchanged
rollback_required=true
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 131' QUIT
trap 'exit 143' TERM

if [[ "${service_was_active}" == "true" ]]; then
  systemctl stop "${SERVICE_NAME}"
fi

chown -R opencodex:opencodex "${opencodex_prefix}"
runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" npm install --global --prefix "${opencodex_prefix}" \
  --ignore-scripts \
  "${PACKAGE_NAME}@${target_version}"
runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" prepare-bundled-bun "${target_version}" >/dev/null
restore_prefix_owner
"${RUNTIME_ADAPTER}" check >/dev/null
validate_installed_version "${target_version}"
validate_installed_package_metadata "${target_version}"
validate_config

if [[ "${service_was_active}" == "true" ]]; then
  systemctl start "${SERVICE_NAME}"
  systemctl is-active --quiet "${SERVICE_NAME}"
fi
verify_enabled_state
if [[ "${skip_smoke}" != "true" ]]; then
  EXPECTED_OPENCODEX_VERSION="${target_version}" "${SMOKE_TEST}"
fi

write_expected_version "${target_version}"
rollback_required=false
printf 'OpenCodex upgraded from %s to %s. Backup retained at %s.\n' \
  "${current_version}" "${target_version}" "${backup_dir}"

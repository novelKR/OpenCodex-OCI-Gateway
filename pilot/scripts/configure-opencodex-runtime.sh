#!/usr/bin/env bash
# Validate or transactionally install the root-managed OpenCodex runtime adapter.

set -Eeuo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PILOT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly DEFAULT_INVOKER_SOURCE="${PILOT_ROOT}/libexec/opencodex-runtime"
readonly UNIT_SOURCE="${PILOT_ROOT}/systemd/opencodex.service"
readonly SERVICE_NAME="opencodex.service"
readonly CONFIG_LOGICAL="/etc/opencodex/runtime.json"
readonly INVOKER_LOGICAL="/usr/local/libexec/opencodex-runtime"
readonly UNIT_LOGICAL="/etc/systemd/system/opencodex.service"
readonly BACKUP_ROOT_LOGICAL="/var/backups/opencodex"
# Some hardened hosts mount /run noexec. Keep the transient, root-owned canary
# beneath an executable system directory; each child remains non-listable.
readonly CANARY_PARENT_LOGICAL="/usr/local"
readonly DEFAULT_HOME="/var/lib/opencodex"
readonly DEFAULT_PREFIX="/opt/opencodex"
readonly PACKAGE_NAME="@bitkyc08/opencodex"
readonly OCX_RELATIVE_PATH="lib/node_modules/${PACKAGE_NAME}/bin/ocx.mjs"
readonly HEALTH_URL="http://127.0.0.1:10100/healthz"

test_mode=false
test_root=""
systemctl_command="systemctl"
curl_command="curl"
invoker_source="${DEFAULT_INVOKER_SOURCE}"
managed_uid=0
health_attempts=60

action=""
node_arg=""
npm_arg=""
home_arg="${DEFAULT_HOME}"
prefix_arg="${DEFAULT_PREFIX}"
allow_service_restart=false
replace_legacy_dropin=""

node_bin=""
npm_cli=""
runtime_home=""
runtime_prefix=""
ocx_entry=""
runtime_bind_root=""
candidate_version=""
preflight_version_output=""
runtime_config=""
invoker_target=""
unit_target=""
backup_root=""
backup_dir=""
canary_parent=""
canary_dir=""
canary_adapter=""
canary_contract=""
service_was_active=false
service_was_enabled=false
admitted_service_state=""
rollback_required=false
service_restart_attempted=false
legacy_dropin_physical=""
legacy_dropin_was_present=false

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'USAGE'
Usage:
  sudo ./configure-opencodex-runtime.sh status
  sudo ./configure-opencodex-runtime.sh check \
    --node-bin PATH --npm-cli PATH [--home PATH] [--prefix PATH]
  sudo ./configure-opencodex-runtime.sh apply \
    --node-bin PATH --npm-cli PATH [--home PATH] [--prefix PATH] \
    --allow-service-restart [--replace-legacy-drop-in PATH]
USAGE
}

numeric_uid() {
  stat -c '%u' -- "$1" 2>/dev/null || stat -f '%u' -- "$1"
}

numeric_gid() {
  stat -c '%g' -- "$1" 2>/dev/null || stat -f '%g' -- "$1"
}

numeric_mode() {
  stat -c '%a' -- "$1" 2>/dev/null || stat -f '%Lp' -- "$1"
}

uid_is_trusted_for_candidate() {
  local candidate="$1"
  [[ "${candidate}" == "0" || (
    "${test_mode}" == "true" && "${candidate}" == "${managed_uid}"
  ) ]]
}

require_trusted_candidate_chain() {
  local path="$1"
  local uid
  local mode
  while :; do
    [[ -d "${path}" && ! -L "${path}" ]] || die "runtime ancestor is unsafe: ${path}"
    [[ "$(realpath -- "${path}")" == "${path}" ]] || die "runtime ancestor is noncanonical: ${path}"
    uid="$(numeric_uid "${path}")"
    uid_is_trusted_for_candidate "${uid}" || die "runtime ancestor has an unsafe owner: ${path}"
    mode="$(numeric_mode "${path}")"
    (( (8#${mode} & 8#022) == 0 )) || \
      die "runtime ancestor is group- or world-writable: ${path}"
    [[ "${path}" == "/" ]] && break
    path="$(dirname -- "${path}")"
  done
}

require_service_traversable_candidate_chain() {
  local path="$1"
  local service_uid="$2"
  local mode
  local owner_uid
  require_trusted_candidate_chain "${path}"
  while :; do
    mode="$(numeric_mode "${path}")"
    owner_uid="$(numeric_uid "${path}")"
    if [[ "${owner_uid}" == "${service_uid}" ]]; then
      (( (8#${mode} & 8#100) != 0 )) || \
        die "runtime home ancestor is not service-traversable: ${path}"
    else
      (( (8#${mode} & 8#001) != 0 )) || \
        die "runtime home ancestor is not service-traversable: ${path}"
    fi
    [[ "${path}" == "/" ]] && break
    path="$(dirname -- "${path}")"
  done
}

rooted_path() {
  local logical="$1"
  if [[ "${test_mode}" == "true" ]]; then
    printf '%s%s\n' "${test_root}" "${logical}"
  else
    printf '%s\n' "${logical}"
  fi
}

require_safe_test_tool() {
  local path="$1"
  local label="$2"
  local mode
  [[ "${path}" == /* && -f "${path}" && ! -L "${path}" && -x "${path}" ]] || \
    die "${label} test tool is unsafe"
  [[ "$(realpath -- "${path}")" == "${path}" ]] || die "${label} test tool must be canonical"
  [[ "$(numeric_uid "${path}")" == "$(id -u)" ]] || die "${label} test tool has an unsafe owner"
  mode="$(numeric_mode "${path}")"
  (( (8#${mode} & 8#022) == 0 )) || die "${label} test tool is group- or world-writable"
}

initialize_environment() {
  local requested_test_root="${OPENCODEX_RUNTIME_TEST_ROOT:-}"
  if [[ -n "${requested_test_root}" ]]; then
    [[ "${EUID}" -ne 0 ]] || die "test-root mode is forbidden for root"
    [[ "${requested_test_root}" == /* && -d "${requested_test_root}" && ! -L "${requested_test_root}" ]] || \
      die "test root is unsafe"
    requested_test_root="$(realpath -- "${requested_test_root}")"
    [[ "$(numeric_uid "${requested_test_root}")" == "$(id -u)" ]] || \
      die "test root must be owned by the invoking user"
    (( (8#$(numeric_mode "${requested_test_root}") & 8#022) == 0 )) || \
      die "test root must not be group- or world-writable"
    [[ -n "${OPENCODEX_RUNTIME_TEST_SYSTEMCTL:-}" ]] || die "test systemctl is required"
    [[ -n "${OPENCODEX_RUNTIME_TEST_CURL:-}" ]] || die "test curl is required"
    [[ -n "${OPENCODEX_RUNTIME_TEST_INVOKER_SOURCE:-}" ]] || \
      die "test invoker source is required"
    test_mode=true
    test_root="${requested_test_root}"
    systemctl_command="$(realpath -- "${OPENCODEX_RUNTIME_TEST_SYSTEMCTL}")"
    curl_command="$(realpath -- "${OPENCODEX_RUNTIME_TEST_CURL}")"
    invoker_source="$(realpath -- "${OPENCODEX_RUNTIME_TEST_INVOKER_SOURCE}")"
    managed_uid="$(id -u)"
    health_attempts=3
    require_safe_test_tool "${systemctl_command}" systemctl
    require_safe_test_tool "${curl_command}" curl
    require_safe_test_tool "${invoker_source}" "runtime adapter"
  else
    [[ -z "${OPENCODEX_RUNTIME_TEST_SYSTEMCTL:-}" &&
       -z "${OPENCODEX_RUNTIME_TEST_CURL:-}" &&
       -z "${OPENCODEX_RUNTIME_TEST_INVOKER_SOURCE:-}" ]] || \
      die "test hooks require OPENCODEX_RUNTIME_TEST_ROOT"
    [[ "${EUID}" -eq 0 ]] || die "run as root"
  fi

  runtime_config="$(rooted_path "${CONFIG_LOGICAL}")"
  invoker_target="$(rooted_path "${INVOKER_LOGICAL}")"
  unit_target="$(rooted_path "${UNIT_LOGICAL}")"
  backup_root="$(rooted_path "${BACKUP_ROOT_LOGICAL}")"
  canary_parent="$(rooted_path "${CANARY_PARENT_LOGICAL}")"
}

parse_arguments() {
  action="${1:-}"
  [[ -n "${action}" ]] || { usage; exit 2; }
  shift || true

  case "${action}" in
    status)
      [[ "$#" -eq 0 ]] || { usage; exit 2; }
      return
      ;;
    check|apply) ;;
    *)
      usage
      exit 2
      ;;
  esac

  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --node-bin)
        [[ "$#" -ge 2 ]] || die "--node-bin requires PATH"
        node_arg="$2"
        shift 2
        ;;
      --npm-cli)
        [[ "$#" -ge 2 ]] || die "--npm-cli requires PATH"
        npm_arg="$2"
        shift 2
        ;;
      --home)
        [[ "$#" -ge 2 ]] || die "--home requires PATH"
        home_arg="$2"
        shift 2
        ;;
      --prefix)
        [[ "$#" -ge 2 ]] || die "--prefix requires PATH"
        prefix_arg="$2"
        shift 2
        ;;
      --allow-service-restart)
        allow_service_restart=true
        shift
        ;;
      --replace-legacy-drop-in)
        [[ "$#" -ge 2 ]] || die "--replace-legacy-drop-in requires PATH"
        replace_legacy_dropin="$2"
        shift 2
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
  done

  [[ -n "${node_arg}" ]] || die "--node-bin is required"
  [[ -n "${npm_arg}" ]] || die "--npm-cli is required"
  if [[ "${action}" == "check" ]]; then
    [[ "${allow_service_restart}" == "false" && -z "${replace_legacy_dropin}" ]] || \
      die "check does not accept apply-only options"
  else
    [[ "${allow_service_restart}" == "true" ]] || \
      die "apply requires --allow-service-restart"
  fi
}

require_commands_and_assets() {
  local command_name
  for command_name in awk basename chown chmod cmp cp curl date dirname env grep id install jq mktemp mv realpath rm rmdir stat; do
    command -v "${command_name}" >/dev/null || die "required command is missing: ${command_name}"
  done
  if [[ "${test_mode}" != "true" ]]; then
    for command_name in runuser systemctl; do
      command -v "${command_name}" >/dev/null || die "required command is missing: ${command_name}"
    done
  fi
  if [[ "${action}" == "check" || "${action}" == "apply" ]]; then
    for path in "${invoker_source}"; do
      [[ -f "${path}" && ! -L "${path}" ]] || die "managed source asset is missing or unsafe: ${path}"
    done
  fi
  if [[ "${action}" == "apply" ]]; then
    [[ -f "${UNIT_SOURCE}" && ! -L "${UNIT_SOURCE}" ]] || \
      die "managed source asset is missing or unsafe: ${UNIT_SOURCE}"
  fi
}

canonical_existing() {
  local candidate="$1"
  local label="$2"
  local expected_kind="$3"
  local canonical
  local mode

  [[ "${candidate}" == /* ]] || die "${label} must be absolute"
  if LC_ALL=C printf '%s' "${candidate}" | grep -q '[[:cntrl:]]'; then
    die "${label} contains a control character"
  fi
  canonical="$(realpath -- "${candidate}")" || die "${label} could not be canonicalized"
  case "${expected_kind}" in
    directory) [[ -d "${canonical}" && ! -L "${canonical}" ]] || die "${label} must be a directory" ;;
    executable) [[ -f "${canonical}" && ! -L "${canonical}" && -x "${canonical}" ]] || die "${label} must be executable" ;;
    readable) [[ -f "${canonical}" && ! -L "${canonical}" && -r "${canonical}" ]] || die "${label} must be readable" ;;
    *) die "internal path kind is invalid" ;;
  esac
  if [[ "${expected_kind}" != "directory" ]]; then
    mode="$(numeric_mode "${canonical}")"
    (( (8#${mode} & 8#7000) == 0 )) || \
      die "${label} must not have setuid, setgid, or sticky bits"
    (( (8#${mode} & 8#022) == 0 )) || \
      die "${label} must not be group- or world-writable"
    if [[ "${expected_kind}" == "executable" ]]; then
      (( (8#${mode} & 8#005) == 8#005 )) || \
        die "${label} must be service-readable and executable"
    else
      (( (8#${mode} & 8#004) == 8#004 )) || \
        die "${label} must be service-readable"
    fi
  fi
  printf '%s\n' "${canonical}"
}

prepare_candidate() {
  local derived_ocx
  local manifest_path
  local service_uid
  local home_mode
  local prefix_mode
  if [[ "${test_mode}" == "true" ]]; then
    service_uid="$(id -u)"
  else
    service_uid="$(id -u opencodex)" || die "OpenCodex service user is unavailable"
  fi
  node_bin="$(canonical_existing "${node_arg}" "Node executable" executable)"
  npm_cli="$(canonical_existing "${npm_arg}" "npm CLI" readable)"
  uid_is_trusted_for_candidate "$(numeric_uid "${node_bin}")" || \
    die "Node executable has an unsafe owner"
  uid_is_trusted_for_candidate "$(numeric_uid "${npm_cli}")" || \
    die "npm CLI has an unsafe owner"
  require_service_traversable_candidate_chain "$(dirname -- "${node_bin}")" "${service_uid}"
  require_service_traversable_candidate_chain "$(dirname -- "${npm_cli}")" "${service_uid}"
  runtime_home="$(canonical_existing "${home_arg}" "runtime home" directory)"
  runtime_prefix="$(canonical_existing "${prefix_arg}" "runtime prefix" directory)"
  case "${runtime_home}" in
    /|/home|/home/*|/root|/root/*|/run/user|/run/user/*)
      die "runtime home is incompatible with ProtectHome=yes"
      ;;
  esac
  case "${runtime_prefix}" in
    /|/home|/home/*|/root|/root/*|/run/user|/run/user/*)
      die "runtime prefix is incompatible with ProtectHome=yes"
      ;;
  esac
  [[ "$(numeric_uid "${runtime_home}")" == "${service_uid}" ]] || \
    die "runtime home has an unsafe owner"
  home_mode="$(numeric_mode "${runtime_home}")"
  [[ "${home_mode}" == "700" ]] || die "runtime home mode must be 700"
  require_service_traversable_candidate_chain "$(dirname -- "${runtime_home}")" "${service_uid}"
  [[ "$(numeric_uid "${runtime_prefix}")" == "${managed_uid}" ]] || \
    die "runtime prefix has an unsafe owner"
  prefix_mode="$(numeric_mode "${runtime_prefix}")"
  (( (8#${prefix_mode} & 8#022) == 0 )) || \
    die "runtime prefix must not be group- or world-writable"
  (( (8#${prefix_mode} & 8#005) == 8#005 )) || \
    die "runtime prefix is not service-traversable"
  require_service_traversable_candidate_chain "$(dirname -- "${runtime_prefix}")" "${service_uid}"
  [[ "${runtime_prefix}" != "${runtime_home}" &&
     "${runtime_prefix}" != "${runtime_home}/"* &&
     "${runtime_home}" != "${runtime_prefix}/"* ]] || \
    die "runtime home and prefix must not overlap"
  derived_ocx="${runtime_prefix}/${OCX_RELATIVE_PATH}"
  ocx_entry="$(canonical_existing "${derived_ocx}" "OpenCodex CLI entry" readable)"
  [[ "${ocx_entry}" == "${derived_ocx}" ]] || \
    die "OpenCodex CLI entry must not traverse a symbolic-link component"
  manifest_path="${runtime_prefix}/lib/node_modules/${PACKAGE_NAME}/package.json"
  [[ "$(canonical_existing "${manifest_path}" "OpenCodex package manifest" readable)" == "${manifest_path}" ]] || \
    die "OpenCodex package manifest must not traverse a symbolic-link component"
  require_managed_candidate_package_path "${ocx_entry}"
  require_managed_candidate_package_path "${manifest_path}"
  jq -e --arg package_name "${PACKAGE_NAME}" '
    .name == $package_name
    and (.version | type == "string")
    and (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$"))
  ' "${runtime_prefix}/lib/node_modules/${PACKAGE_NAME}/package.json" >/dev/null || \
    die "OpenCodex package metadata is invalid"
  candidate_version="$(jq -er '.version' \
    "${runtime_prefix}/lib/node_modules/${PACKAGE_NAME}/package.json")"
  derive_runtime_bind_root
}

require_managed_candidate_package_path() {
  local path="$1"
  local current
  local mode
  [[ "$(numeric_uid "${path}")" == "${managed_uid}" ]] || \
    die "managed package file has an unsafe owner"
  current="$(dirname -- "${path}")"
  while :; do
    [[ -d "${current}" && ! -L "${current}" &&
       "$(realpath -- "${current}")" == "${current}" ]] || \
      die "managed package ancestor is unsafe"
    [[ "$(numeric_uid "${current}")" == "${managed_uid}" ]] || \
      die "managed package ancestor has an unsafe owner"
    mode="$(numeric_mode "${current}")"
    (( (8#${mode} & 8#022) == 0 && (8#${mode} & 8#001) != 0 )) || \
      die "managed package ancestor has an unsafe or non-traversable mode"
    [[ "${current}" == "${runtime_prefix}" ]] && break
    current="$(dirname -- "${current}")"
    [[ "${current}" == "${runtime_prefix}" || "${current}" == "${runtime_prefix}/"* ]] || \
      die "managed package ancestor escaped runtime prefix"
  done
}

runtime_root_for_path() {
  local path="$1"
  local protected_prefix="$2"
  local relative
  local owner
  local remainder
  local runtime_root_name
  [[ "${path}" == "${protected_prefix}/"* ]] || return 1
  relative="${path#"${protected_prefix}/"}"
  [[ "${relative}" == */* ]] || return 1
  owner="${relative%%/*}"
  remainder="${relative#*/}"
  [[ -n "${owner}" && "${remainder}" == */* ]] || return 1
  runtime_root_name="${remainder%%/*}"
  [[ -n "${runtime_root_name}" ]] || return 1
  printf '%s/%s/%s\n' "${protected_prefix}" "${owner}" "${runtime_root_name}"
}

derive_runtime_bind_root() {
  local protected_prefix="/home"
  local node_root=""
  local npm_root=""
  local node_hidden=false
  local npm_hidden=false
  local mode

  if [[ "${test_mode}" == "true" ]]; then
    protected_prefix="${test_root}/home"
  else
    case "${node_bin}" in
      /root/*|/run/user/*) die "Node executable is beneath a protected runtime root" ;;
    esac
    case "${npm_cli}" in
      /root/*|/run/user/*) die "npm CLI is beneath a protected runtime root" ;;
    esac
  fi
  if [[ "${node_bin}" == "${protected_prefix}/"* ]]; then node_hidden=true; fi
  if [[ "${npm_cli}" == "${protected_prefix}/"* ]]; then npm_hidden=true; fi
  if [[ "${node_hidden}" != "${npm_hidden}" ]]; then
    die "Node and npm must share one protected-home runtime root"
  fi
  if [[ "${node_hidden}" != "true" ]]; then
    runtime_bind_root=""
    return
  fi
  node_root="$(runtime_root_for_path "${node_bin}" "${protected_prefix}")" || \
    die "Node executable does not have a safe protected-home runtime root"
  npm_root="$(runtime_root_for_path "${npm_cli}" "${protected_prefix}")" || \
    die "npm CLI does not have a safe protected-home runtime root"
  [[ "${node_root}" == "${npm_root}" ]] || \
    die "Node and npm must share one protected-home runtime root"
  [[ -d "${node_root}" && ! -L "${node_root}" &&
     "$(realpath -- "${node_root}")" == "${node_root}" ]] || \
    die "protected-home runtime root is missing or noncanonical"
  mode="$(numeric_mode "${node_root}")"
  (( (8#${mode} & 8#022) == 0 )) || \
    die "protected-home runtime root must not be group- or world-writable"
  runtime_bind_root="${node_root}"
}

candidate_contract_json() {
  jq -cn \
    --argjson schema_version 1 \
    --arg runtime_kind node \
    --arg home "${runtime_home}" \
    --arg prefix "${runtime_prefix}" \
    --arg node_bin "${node_bin}" \
    --arg npm_cli "${npm_cli}" \
    --arg ocx_entry "${ocx_entry}" \
    '{
      schema_version: $schema_version,
      runtime_kind: $runtime_kind,
      home: $home,
      prefix: $prefix,
      node_bin: $node_bin,
      npm_cli: $npm_cli,
      ocx_entry: $ocx_entry
    }'
}

cleanup_canary() {
  local directory
  local failed=false
  [[ -n "${canary_dir}" ]] || return 0
  directory="${canary_dir}"
  canary_dir=""
  rm -f -- "${directory}/opencodex-runtime" || failed=true
  rm -f -- "${directory}/runtime.json" || failed=true
  rmdir -- "${directory}" || failed=true
  canary_adapter=""
  canary_contract=""
  [[ "${failed}" == "false" ]]
}

prepare_canary_parent() {
  local service_uid
  local mode
  [[ -d "${canary_parent}" && ! -L "${canary_parent}" &&
     "$(realpath -- "${canary_parent}")" == "${canary_parent}" ]] || \
    die "runtime canary parent is missing or unsafe"
  [[ "$(numeric_uid "${canary_parent}")" == "${managed_uid}" ]] || \
    die "runtime canary parent has an unsafe owner"
  mode="$(numeric_mode "${canary_parent}")"
  (( (8#${mode} & 8#022) == 0 && (8#${mode} & 8#001) != 0 )) || \
    die "runtime canary parent has an unsafe or non-traversable mode"
  if [[ "${test_mode}" == "true" ]]; then
    service_uid="$(id -u)"
  else
    service_uid="$(id -u opencodex)" || die "OpenCodex service user is unavailable"
  fi
  require_service_traversable_candidate_chain "${canary_parent}" "${service_uid}"
}

stage_isolated_canary() {
  prepare_canary_parent
  canary_dir="$(mktemp -d "${canary_parent}/opencodex-runtime-canary.XXXXXX")"
  if [[ "${test_mode}" != "true" ]]; then
    chown root:root "${canary_dir}"
  fi
  canary_adapter="${canary_dir}/opencodex-runtime"
  canary_contract="${canary_dir}/runtime.json"
  if [[ "${test_mode}" == "true" ]]; then
    install -m 0755 "${invoker_source}" "${canary_adapter}"
  else
    install -o root -g root -m 0755 "${invoker_source}" "${canary_adapter}"
  fi
  cmp -s -- "${invoker_source}" "${canary_adapter}" || \
    die "runtime canary adapter is not byte-identical to the reviewed source"
  (umask 077; candidate_contract_json > "${canary_contract}")
  if [[ "${test_mode}" != "true" ]]; then
    chown root:root "${canary_contract}"
  fi
  chmod 0644 "${canary_contract}"
  chmod 0711 "${canary_dir}"
  [[ ! -L "${canary_adapter}" && ! -L "${canary_contract}" &&
     "$(realpath -- "${canary_adapter}")" == "${canary_adapter}" &&
     "$(realpath -- "${canary_contract}")" == "${canary_contract}" &&
     "$(numeric_uid "${canary_adapter}")" == "${managed_uid}" &&
     "$(numeric_uid "${canary_contract}")" == "${managed_uid}" &&
     "$(numeric_mode "${canary_adapter}")" == "755" &&
     "$(numeric_mode "${canary_contract}")" == "644" &&
     "$(numeric_mode "${canary_dir}")" == "711" ]] || \
    die "runtime canary assets do not have the required ownership or mode"
}

run_canary_as_root() {
  env -i \
    OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG="${canary_contract}" \
    "${canary_adapter}" "$@"
}

run_canary_as_service() {
  if [[ "${test_mode}" == "true" ]]; then
    env -i \
      OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG="${canary_contract}" \
      "${canary_adapter}" "$@"
  else
    runuser -u opencodex -- env -i \
      OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG="${canary_contract}" \
      "${canary_adapter}" "$@"
  fi
}

validate_candidate_with_isolated_canary() {
  local described
  local version_output
  stage_isolated_canary
  run_canary_as_root check >/dev/null || \
    die "isolated runtime adapter check failed"
  described="$(run_canary_as_root describe --json)" || \
    die "isolated runtime adapter description failed"
  jq -e \
    --arg home "${runtime_home}" \
    --arg prefix "${runtime_prefix}" \
    --arg node_bin "${node_bin}" \
    --arg npm_cli "${npm_cli}" \
    --arg ocx_entry "${ocx_entry}" \
    --arg package_version "${candidate_version}" '
      .schema_version == 1
      and .runtime_kind == "node"
      and .home == $home
      and .prefix == $prefix
      and .node_bin == $node_bin
      and .npm_cli == $npm_cli
      and .ocx_entry == $ocx_entry
      and .package_version == $package_version
    ' <<< "${described}" >/dev/null || \
    die "isolated runtime adapter description disagrees with the candidate"
  version_output="$(run_canary_as_service ocx --version)" || \
    die "isolated OpenCodex version could not be read"
  [[ "${version_output}" == "opencodex ${candidate_version}" ]] || \
    die "isolated OpenCodex runtime and package versions disagree"
  run_canary_as_service ocx config validate >/dev/null || \
    die "isolated OpenCodex configuration is invalid"
  run_canary_as_service npm --version >/dev/null || \
    die "isolated npm CLI is not usable by the OpenCodex service user"
}

run_candidate_ocx() {
  (
    cd -- "${runtime_home}" || exit 1
    if [[ "${test_mode}" == "true" ]]; then
      env -i HOME="${runtime_home}" PATH="$(dirname -- "${node_bin}"):/usr/bin:/bin" \
        "${node_bin}" "${ocx_entry}" "$@"
    else
      runuser -u opencodex -- env -i \
        HOME="${runtime_home}" \
        PATH="$(dirname -- "${node_bin}"):/usr/bin:/bin" \
        "${node_bin}" "${ocx_entry}" "$@"
    fi
  )
}

run_candidate_npm() {
  (
    cd -- "${runtime_home}" || exit 1
    if [[ "${test_mode}" == "true" ]]; then
      env -i HOME="${runtime_home}" PATH="$(dirname -- "${node_bin}"):/usr/bin:/bin" \
        "${node_bin}" "${npm_cli}" "$@"
    else
      runuser -u opencodex -- env -i \
        HOME="${runtime_home}" \
        PATH="$(dirname -- "${node_bin}"):/usr/bin:/bin" \
        "${node_bin}" "${npm_cli}" "$@"
    fi
  )
}

validate_candidate_cli() {
  preflight_version_output="$(run_candidate_ocx --version)" || \
    die "candidate OpenCodex version could not be read"
  [[ "${preflight_version_output}" == "opencodex ${candidate_version}" ]] || \
    die "candidate OpenCodex runtime and package versions disagree"
  run_candidate_ocx config validate >/dev/null || \
    die "candidate OpenCodex configuration is invalid"
  run_candidate_npm --version >/dev/null || \
    die "candidate npm CLI is not usable by the OpenCodex service user"
}

candidate_cli_matches_preflight() {
  local restored_version
  restored_version="$(run_candidate_ocx --version)" || return 1
  [[ "${restored_version}" == "${preflight_version_output}" ]] || return 1
  run_candidate_ocx config validate >/dev/null
}

service_is_active() {
  "${systemctl_command}" is-active --quiet "${SERVICE_NAME}"
}

service_is_enabled() {
  "${systemctl_command}" is-enabled --quiet "${SERVICE_NAME}"
}

read_stable_service_state() {
  local state
  state="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ActiveState --value 2>/dev/null || true)"
  case "${state}" in
    active|inactive) printf '%s\n' "${state}" ;;
    *) die "${SERVICE_NAME} ActiveState must be exactly active or inactive, got ${state:-unavailable}" ;;
  esac
}

capture_stable_service_state() {
  admitted_service_state="$(read_stable_service_state)"
}

require_admitted_service_state() {
  local current
  current="$(read_stable_service_state)"
  [[ "${current}" == "${admitted_service_state}" ]] || \
    die "${SERVICE_NAME} ActiveState changed from ${admitted_service_state} to ${current}"
}

preflight_idle_service() {
  local status_json
  if [[ "${admitted_service_state}" != "active" ]]; then
    return
  fi
  status_json="$(run_candidate_ocx observe memory --json)" || \
    die "active OpenCodex lifecycle state could not be inspected"
  jq -e '
    .activeTurnCount == 0
    and .isDraining == false
  ' <<< "${status_json}" >/dev/null || \
    die "active OpenCodex is busy or draining; refusing service migration"
}

physical_dropin_path() {
  local logical="$1"
  local expected_directory="/etc/systemd/system/opencodex.service.d"
  local directory
  local name
  local physical
  if LC_ALL=C printf '%s' "${logical}" | grep -q '[[:cntrl:]]'; then
    die "legacy drop-in path contains a control character"
  fi
  directory="$(dirname -- "${logical}")"
  name="$(basename -- "${logical}")"
  [[ "${directory}" == "${expected_directory}" &&
     "${logical}" == "${expected_directory}/${name}" &&
     "${name}" == *.conf &&
     "${name}" != ".conf" ]] || \
    die "legacy drop-in must be an absolute .conf beneath opencodex.service.d"
  physical="$(rooted_path "${logical}")"
  require_safe_managed_file "${physical}" "explicit legacy drop-in" readable
  printf '%s\n' "${physical}"
}

check_effective_dropins() {
  local listed
  local path
  local physical
  if [[ -n "${replace_legacy_dropin}" ]]; then
    legacy_dropin_physical="$(physical_dropin_path "${replace_legacy_dropin}")"
  fi
  listed="$("${systemctl_command}" show "${SERVICE_NAME}" --property=DropInPaths --value 2>/dev/null || true)"
  for path in ${listed}; do
    [[ "${path}" == /* ]] || die "systemd reported an unsafe drop-in path"
    physical="$(rooted_path "${path}")"
    if [[ -n "${replace_legacy_dropin}" && "${path}" == "${replace_legacy_dropin}" ]]; then
      continue
    fi
    require_safe_managed_file "${physical}" "retained systemd drop-in" readable
    if grep -Eq '^[[:space:]]*ExecStart(Pre)?[[:space:]]*=' "${physical}"; then
      die "an unmanaged drop-in changes OpenCodex execution: ${path}"
    fi
  done
}

require_safe_managed_file() {
  local path="$1"
  local label="$2"
  local access="${3:-readable}"
  local mode
  [[ -f "${path}" && ! -L "${path}" &&
     "$(realpath -- "${path}")" == "${path}" ]] || \
    die "${label} is missing, noncanonical, or unsafe"
  [[ "$(numeric_uid "${path}")" == "${managed_uid}" ]] || \
    die "${label} has an unsafe owner"
  mode="$(numeric_mode "${path}")"
  (( (8#${mode} & 8#7022) == 0 )) || \
    die "${label} has unsafe mode or special bits"
  if [[ "${access}" == "executable" ]]; then
    (( (8#${mode} & 8#100) != 0 )) || die "${label} is not executable"
  fi
  require_trusted_candidate_chain "$(dirname -- "${path}")"
}

write_presence_snapshot() {
  local source="$1"
  local name="$2"
  local access="${3:-readable}"
  if [[ -e "${source}" || -L "${source}" ]]; then
    require_safe_managed_file "${source}" "rollback ${name}" "${access}"
    printf 'present\n' > "${backup_dir}/${name}.state"
    cp -p "${source}" "${backup_dir}/${name}"
  else
    printf 'absent\n' > "${backup_dir}/${name}.state"
  fi
}

write_directory_snapshot() {
  local source="$1"
  local name="$2"
  if [[ -e "${source}" || -L "${source}" ]]; then
    [[ -d "${source}" && ! -L "${source}" &&
       "$(realpath -- "${source}")" == "${source}" ]] || \
      die "managed directory is unsafe: ${source}"
    [[ "$(numeric_uid "${source}")" == "${managed_uid}" ]] || \
      die "managed directory has an unsafe owner: ${source}"
    (( (8#$(numeric_mode "${source}") & 8#7022) == 0 )) || \
      die "managed directory has an unsafe mode: ${source}"
    require_trusted_candidate_chain "$(dirname -- "${source}")"
    printf 'present\n' > "${backup_dir}/${name}.state"
    printf '%s\n' "$(numeric_uid "${source}")" > "${backup_dir}/${name}.uid"
    printf '%s\n' "$(numeric_gid "${source}")" > "${backup_dir}/${name}.gid"
    printf '%s\n' "$(numeric_mode "${source}")" > "${backup_dir}/${name}.mode"
  else
    printf 'absent\n' > "${backup_dir}/${name}.state"
  fi
}

make_snapshot() {
  if [[ "${admitted_service_state}" == "active" ]]; then service_was_active=true; fi
  if service_is_enabled; then service_was_enabled=true; fi

  if [[ "${test_mode}" == "true" ]]; then
    install -d -m 0700 "${backup_root}"
    backup_dir="$(mktemp -d "${backup_root}/runtime-migration-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")"
  else
    install -d -o root -g root -m 0700 "${backup_root}"
    backup_dir="$(mktemp -d "${backup_root}/runtime-migration-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")"
    chown root:root "${backup_dir}"
  fi
  chmod 0700 "${backup_dir}"
  printf '%s\n' "${service_was_active}" > "${backup_dir}/service-active"
  printf '%s\n' "${service_was_enabled}" > "${backup_dir}/service-enabled"
  write_presence_snapshot "${unit_target}" unit readable
  write_presence_snapshot "${runtime_config}" runtime.json readable
  write_presence_snapshot "${invoker_target}" runtime-adapter executable
  write_directory_snapshot "$(dirname -- "${runtime_config}")" config-parent
  write_directory_snapshot "$(dirname -- "${invoker_target}")" invoker-parent
  write_directory_snapshot "$(dirname -- "${unit_target}")" unit-parent
  if [[ -n "${replace_legacy_dropin}" ]]; then
    write_presence_snapshot "${legacy_dropin_physical}" legacy-drop-in readable
    if [[ "$(<"${backup_dir}/legacy-drop-in.state")" == "present" ]]; then
      legacy_dropin_was_present=true
    else
      die "explicit legacy drop-in does not exist"
    fi
  fi
}

prepare_static_managed_parent() {
  local directory="$1"
  local label="$2"
  local mode
  if [[ ! -e "${directory}" && ! -L "${directory}" ]]; then
    if [[ "${test_mode}" == "true" ]]; then
      install -d -m 0755 "${directory}"
    else
      install -d -o root -g root -m 0755 "${directory}"
    fi
  fi
  [[ -d "${directory}" && ! -L "${directory}" &&
     "$(realpath -- "${directory}")" == "${directory}" ]] || \
    die "${label} parent directory is unsafe"
  [[ "$(numeric_uid "${directory}")" == "${managed_uid}" ]] || \
    die "${label} parent directory has an unsafe owner"
  mode="$(numeric_mode "${directory}")"
  (( (8#${mode} & 8#022) == 0 && (8#${mode} & 8#005) == 8#005 )) || \
    die "${label} parent directory has an unsafe mode"
  require_trusted_candidate_chain "$(dirname -- "${directory}")"
}

prepare_target_parents() {
  local config_parent
  local mode
  config_parent="$(dirname -- "${runtime_config}")"
  if [[ -e "${config_parent}" || -L "${config_parent}" ]]; then
    [[ -d "${config_parent}" && ! -L "${config_parent}" &&
       "$(realpath -- "${config_parent}")" == "${config_parent}" ]] || \
      die "runtime config parent is unsafe"
    [[ "$(numeric_uid "${config_parent}")" == "${managed_uid}" ]] || \
      die "runtime config parent has an unsafe owner"
    mode="$(numeric_mode "${config_parent}")"
    (( (8#${mode} & 8#022) == 0 )) || die "runtime config parent has an unsafe mode"
  elif [[ "${test_mode}" == "true" ]]; then
    install -d -m 0755 "${config_parent}"
  else
    install -d -o root -g root -m 0755 "${config_parent}"
  fi
  if [[ "${test_mode}" != "true" ]]; then
    chown root:root "${config_parent}"
  fi
  chmod 0755 "${config_parent}"
  require_trusted_candidate_chain "$(dirname -- "${config_parent}")"
  prepare_static_managed_parent "$(dirname -- "${invoker_target}")" "runtime adapter"
  prepare_static_managed_parent "$(dirname -- "${unit_target}")" "systemd unit"
}

atomic_install() {
  local source="$1"
  local target="$2"
  local mode="$3"
  local directory
  local candidate
  directory="$(dirname -- "${target}")"
  [[ -d "${directory}" && ! -L "${directory}" ]] || \
    die "managed target parent is missing or unsafe: ${directory}"
  candidate="$(mktemp "${directory}/.$(basename -- "${target}").XXXXXX")"
  if [[ "${test_mode}" == "true" ]]; then
    if ! install -m "${mode}" "${source}" "${candidate}"; then
      rm -f -- "${candidate}"
      return 1
    fi
  else
    if ! install -o root -g root -m "${mode}" "${source}" "${candidate}"; then
      rm -f -- "${candidate}"
      return 1
    fi
  fi
  if [[ -L "${target}" ]]; then
    rm -f -- "${candidate}"
    die "managed target must not be a symbolic link: ${target}"
  fi
  if ! mv -f "${candidate}" "${target}"; then
    rm -f -- "${candidate}"
    return 1
  fi
}

render_unit_source() {
  local target="$1"
  local escaped_home="${runtime_home//\\/\\\\}"
  local escaped_bind="${runtime_bind_root//\\/\\\\}"
  escaped_home="${escaped_home//\"/\\\"}"
  escaped_home="${escaped_home//%/%%}"
  escaped_bind="${escaped_bind//\"/\\\"}"
  escaped_bind="${escaped_bind//%/%%}"
  awk \
    -v read_write="ReadWritePaths=\"${escaped_home}\"" \
    -v runtime_bind="${escaped_bind}" '
    /^ReadWritePaths=/ {
      print read_write
      found_read_write = 1
      next
    }
    /^ProtectHome=/ {
      if (runtime_bind != "") {
        print "ProtectHome=tmpfs"
        print "BindReadOnlyPaths=\"" runtime_bind "\""
      } else {
        print
      }
      found_protect_home = 1
      next
    }
    { print }
    END {
      if (!found_read_write || !found_protect_home) exit 1
    }
  ' "${UNIT_SOURCE}" > "${target}" || die "managed unit could not be rendered"
  chmod 0600 "${target}"
}

install_candidate_contract() {
  local source
  source="$(mktemp "${backup_dir}/runtime-json.XXXXXX")"
  candidate_contract_json > "${source}"
  chmod 0600 "${source}"
  atomic_install "${source}" "${runtime_config}" 0644
}

restore_snapshot_path() {
  local target="$1"
  local name="$2"
  local state
  local directory
  local candidate
  state="$(<"${backup_dir}/${name}.state")"
  if [[ "${state}" == "present" ]]; then
    directory="$(dirname -- "${target}")"
    [[ -d "${directory}" && ! -L "${directory}" ]] || return 1
    candidate="$(mktemp "${directory}/.$(basename -- "${target}").restore.XXXXXX")"
    if ! cp -p "${backup_dir}/${name}" "${candidate}"; then
      rm -f -- "${candidate}"
      return 1
    fi
    if [[ -L "${target}" ]]; then
      rm -f -- "${candidate}"
      return 1
    fi
    if ! mv -f "${candidate}" "${target}"; then
      rm -f -- "${candidate}"
      return 1
    fi
  elif [[ "${state}" == "absent" ]]; then
    rm -f -- "${target}"
  else
    return 1
  fi
}

restore_directory_snapshot() {
  local parent="$1"
  local name="$2"
  local state
  local uid
  local gid
  local mode
  state="$(<"${backup_dir}/${name}.state")"
  if [[ "${state}" == "present" ]]; then
    uid="$(<"${backup_dir}/${name}.uid")"
    gid="$(<"${backup_dir}/${name}.gid")"
    mode="$(<"${backup_dir}/${name}.mode")"
    chown "${uid}:${gid}" "${parent}" || return 1
    chmod "${mode}" "${parent}" || return 1
  elif [[ "${state}" == "absent" ]]; then
    rmdir -- "${parent}" || return 1
  else
    return 1
  fi
}

restore_service_state() {
  local failed=false
  if [[ "${service_was_enabled}" == "true" ]]; then
    if ! service_is_enabled; then
      "${systemctl_command}" enable "${SERVICE_NAME}" >/dev/null || failed=true
    fi
    service_is_enabled || failed=true
  else
    if service_is_enabled; then
      "${systemctl_command}" disable "${SERVICE_NAME}" >/dev/null 2>&1 || failed=true
    fi
    if service_is_enabled; then failed=true; fi
  fi
  if [[ "${service_was_active}" == "true" ]]; then
    if ! service_is_active; then
      "${systemctl_command}" start "${SERVICE_NAME}" >/dev/null || failed=true
    fi
    service_is_active || failed=true
  else
    if service_is_active; then
      "${systemctl_command}" stop "${SERVICE_NAME}" >/dev/null 2>&1 || failed=true
    fi
    if service_is_active; then failed=true; fi
  fi
  [[ "${failed}" == "false" ]]
}

restored_adapter_is_valid() {
  [[ "$(<"${backup_dir}/runtime.json.state")" == "present" &&
     "$(<"${backup_dir}/runtime-adapter.state")" == "present" ]] || return 0
  "${invoker_target}" check >/dev/null || return 1
  if [[ "${test_mode}" == "true" ]]; then
    "${invoker_target}" ocx config validate >/dev/null
  else
    runuser -u opencodex -- "${invoker_target}" ocx config validate >/dev/null
  fi
}

health_matches_version() {
  local expected_version="$1"
  local response
  local attempt
  for ((attempt = 1; attempt <= health_attempts; attempt++)); do
    response="$("${curl_command}" --fail --silent --show-error --max-time 5 "${HEALTH_URL}" 2>/dev/null || true)"
    if jq -e --arg version "${expected_version}" '
      .service == "opencodex"
      and .status == "ok"
      and .version == $version
    ' <<< "${response}" >/dev/null 2>&1; then
      return
    fi
    if [[ "${test_mode}" != "true" ]]; then sleep 1; fi
  done
  return 1
}

preflight_running_health() {
  if [[ "${admitted_service_state}" == "active" ]]; then
    health_matches_version "${candidate_version}" || \
      die "active OpenCodex health identity, status, or version is invalid"
  fi
}

rollback() {
  local failed=false
  printf 'Runtime migration failed; restoring the previous managed assets.\n' >&2
  if [[ "${service_restart_attempted}" == "true" ]]; then
    "${systemctl_command}" stop "${SERVICE_NAME}" >/dev/null 2>&1 || true
  fi
  restore_snapshot_path "${unit_target}" unit || failed=true
  restore_snapshot_path "${runtime_config}" runtime.json || failed=true
  restore_snapshot_path "${invoker_target}" runtime-adapter || failed=true
  if [[ -n "${replace_legacy_dropin}" ]]; then
    restore_snapshot_path "${legacy_dropin_physical}" legacy-drop-in || failed=true
  fi
  restore_directory_snapshot "$(dirname -- "${runtime_config}")" config-parent || failed=true
  restore_directory_snapshot "$(dirname -- "${invoker_target}")" invoker-parent || failed=true
  restore_directory_snapshot "$(dirname -- "${unit_target}")" unit-parent || failed=true
  "${systemctl_command}" daemon-reload >/dev/null || failed=true
  restore_service_state || failed=true
  candidate_cli_matches_preflight || failed=true
  restored_adapter_is_valid || failed=true
  if [[ "${service_was_active}" == "true" ]]; then
    health_matches_version "${candidate_version}" || failed=true
  fi
  if [[ "${failed}" == "true" ]]; then
    printf 'CRITICAL: runtime migration rollback could not be fully verified; inspect %s.\n' \
      "${backup_dir}" >&2
    return 1
  fi
  printf 'Previous runtime assets and service state were restored.\n' >&2
}

cleanup() {
  local status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  if ! cleanup_canary; then
    printf 'ERROR: isolated runtime canary cleanup failed.\n' >&2
    status=70
  fi
  if [[ "${status}" -ne 0 && "${rollback_required}" == "true" ]]; then
    rollback || status=70
  fi
  exit "${status}"
}

verify_effective_service() {
  local exec_start
  local exec_start_pre
  local protect_home
  local bind_read_only
  local effective_user
  local effective_group
  local no_new_privileges
  local protect_system
  local private_devices
  local private_tmp
  local restrict_namespaces
  local read_write_paths
  local capability_bounding_set
  local ambient_capabilities
  exec_start="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ExecStart --value)"
  exec_start_pre="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ExecStartPre --value)"
  [[ "${exec_start}" == *"${INVOKER_LOGICAL} ocx start --port 10100"* ]] || \
    die "effective ExecStart does not use the runtime adapter"
  [[ "${exec_start_pre}" == *"${INVOKER_LOGICAL} check"* ]] || \
    die "effective ExecStartPre does not validate the runtime adapter"
  [[ "${exec_start_pre}" == *"${INVOKER_LOGICAL} ocx config validate"* ]] || \
    die "effective ExecStartPre does not validate OpenCodex configuration"
  effective_user="$("${systemctl_command}" show "${SERVICE_NAME}" --property=User --value)"
  effective_group="$("${systemctl_command}" show "${SERVICE_NAME}" --property=Group --value)"
  no_new_privileges="$("${systemctl_command}" show "${SERVICE_NAME}" --property=NoNewPrivileges --value)"
  protect_system="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ProtectSystem --value)"
  private_devices="$("${systemctl_command}" show "${SERVICE_NAME}" --property=PrivateDevices --value)"
  private_tmp="$("${systemctl_command}" show "${SERVICE_NAME}" --property=PrivateTmp --value)"
  restrict_namespaces="$("${systemctl_command}" show "${SERVICE_NAME}" --property=RestrictNamespaces --value)"
  read_write_paths="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ReadWritePaths --value)"
  capability_bounding_set="$("${systemctl_command}" show "${SERVICE_NAME}" --property=CapabilityBoundingSet --value)"
  ambient_capabilities="$("${systemctl_command}" show "${SERVICE_NAME}" --property=AmbientCapabilities --value)"
  [[ "${effective_user}" == "opencodex" && "${effective_group}" == "opencodex" ]] || \
    die "effective service identity is not opencodex:opencodex"
  [[ "${no_new_privileges}" == "yes" &&
     "${protect_system}" == "strict" &&
     "${private_devices}" == "yes" &&
     "${private_tmp}" == "yes" &&
     "${restrict_namespaces}" == "yes" ]] || \
    die "effective service sandbox does not match the managed contract"
  [[ "${read_write_paths}" == "${runtime_home}" ]] || \
    die "effective ReadWritePaths does not equal the contracted runtime home"
  [[ -z "${capability_bounding_set}" && -z "${ambient_capabilities}" ]] || \
    die "effective service capabilities are not empty"
  protect_home="$("${systemctl_command}" show "${SERVICE_NAME}" --property=ProtectHome --value)"
  bind_read_only="$("${systemctl_command}" show "${SERVICE_NAME}" --property=BindReadOnlyPaths --value)"
  if [[ -n "${runtime_bind_root}" ]]; then
    [[ "${protect_home}" == "tmpfs" ]] || \
      die "effective ProtectHome does not expose only the pinned runtime"
    [[ "${bind_read_only}" == "${runtime_bind_root}" ]] || \
      die "effective BindReadOnlyPaths is not exactly the pinned runtime root"
  else
    [[ "${protect_home}" == "yes" ]] || \
      die "effective ProtectHome unexpectedly changed"
    [[ -z "${bind_read_only}" ]] || \
      die "effective BindReadOnlyPaths unexpectedly exposes a home path"
  fi
}

verify_health() {
  health_matches_version "${candidate_version}" || \
    die "OpenCodex health did not recover with the expected identity and version"
}

validate_installed_runtime() {
  "${invoker_target}" check >/dev/null
  if [[ "${test_mode}" == "true" ]]; then
    "${invoker_target}" ocx config validate >/dev/null
    "${invoker_target}" npm --version >/dev/null
  else
    runuser -u opencodex -- "${invoker_target}" ocx config validate >/dev/null
    runuser -u opencodex -- "${invoker_target}" npm --version >/dev/null
  fi
}

show_status() {
  [[ -x "${invoker_target}" && ! -L "${invoker_target}" ]] || \
    die "runtime adapter is not installed safely"
  "${invoker_target}" check >/dev/null
  printf 'runtime_adapter=valid\n'
  printf 'service_active=%s\n' "$("${systemctl_command}" is-active "${SERVICE_NAME}" 2>/dev/null || true)"
  printf 'service_enabled=%s\n' "$("${systemctl_command}" is-enabled "${SERVICE_NAME}" 2>/dev/null || true)"
}

parse_arguments "$@"
initialize_environment
require_commands_and_assets

if [[ "${action}" == "status" ]]; then
  show_status
  exit 0
fi

prepare_candidate
if [[ "${action}" == "check" ]]; then
  trap cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  validate_candidate_with_isolated_canary
  cleanup_canary || die "isolated runtime canary cleanup failed"
  printf 'runtime_candidate=valid\n'
  exit 0
fi
validate_candidate_cli

capture_stable_service_state
check_effective_dropins
preflight_idle_service
preflight_running_health
require_admitted_service_state
make_snapshot
rollback_required=true
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 131' QUIT
trap 'exit 143' TERM

require_admitted_service_state
if [[ "${service_was_active}" == "true" ]]; then
  # The initial candidate CLI idle check passed while the old proxy was still
  # running. Stop it before swapping the contract so no new admission can race
  # the candidate runtime, which may not be management-API compatible with the
  # old process.
  service_restart_attempted=true
  "${systemctl_command}" stop "${SERVICE_NAME}"
  if service_is_active; then
    die "${SERVICE_NAME} did not become inactive for runtime migration"
  fi
fi
prepare_target_parents
install_candidate_contract
atomic_install "${invoker_source}" "${invoker_target}" 0755
rendered_unit="$(mktemp "${backup_dir}/opencodex-service.XXXXXX")"
render_unit_source "${rendered_unit}"
atomic_install "${rendered_unit}" "${unit_target}" 0644
if [[ "${legacy_dropin_was_present}" == "true" ]]; then
  rm -f -- "${legacy_dropin_physical}"
fi

"${systemctl_command}" daemon-reload
validate_installed_runtime
verify_effective_service
if [[ "${service_was_active}" == "true" ]]; then
  "${systemctl_command}" start "${SERVICE_NAME}"
  service_is_active || die "${SERVICE_NAME} did not become active"
  verify_health
fi
if [[ "${service_was_enabled}" == "true" ]]; then
  service_is_enabled || die "${SERVICE_NAME} unexpectedly lost its enabled state"
elif service_is_enabled; then
  die "${SERVICE_NAME} unexpectedly became enabled"
fi
if [[ "${service_was_active}" == "false" ]]; then
  require_admitted_service_state
fi

rollback_required=false
printf 'OpenCodex runtime adapter installed. Backup retained at %s.\n' "${backup_dir}"

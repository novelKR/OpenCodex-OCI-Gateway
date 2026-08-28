#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly ASSET_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly OPENCODEX_VERSION="${OPENCODEX_VERSION:-}"
readonly OPENCODEX_HOME="/var/lib/opencodex"
readonly OPENCODEX_PREFIX="/opt/opencodex"
readonly PACKAGE_NAME="@bitkyc08/opencodex"
readonly PACKAGE_MANIFEST="${OPENCODEX_PREFIX}/lib/node_modules/${PACKAGE_NAME}/package.json"
readonly OCX_ENTRY="${OPENCODEX_PREFIX}/lib/node_modules/${PACKAGE_NAME}/bin/ocx.mjs"
readonly RUNTIME_ADAPTER_SOURCE="${ASSET_DIR}/libexec/opencodex-runtime"
readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"
readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"
readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"
readonly SYSTEMD_UNIT="/etc/systemd/system/opencodex.service"
readonly OS_RELEASE_FILE="/etc/os-release"
readonly BACKUP_ROOT="/var/backups/opencodex"
readonly BOOTSTRAP_MARKER="${BACKUP_ROOT}/bootstrap-fresh.pending"
readonly BOOTSTRAP_MARKER_CONTENT="opencodex-relay-bootstrap-fresh-v1"
readonly WEBSOCKET_PROXY_SOURCE="${ASSET_DIR}/nginx/opencodex-websocket-proxy.conf"
readonly WEBSOCKET_PROXY_TARGET="/etc/nginx/snippets/opencodex-websocket-proxy.conf"
readonly FEATURE_FLAGS_SOURCE="${ASSET_DIR}/nginx/opencodex-feature-flags.deny-all.conf"
readonly FEATURE_FLAGS_TARGET="/etc/nginx/private/opencodex-feature-flags.conf"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

atomic_install_asset() {
  local source="$1"
  local target="$2"
  local mode="$3"
  local candidate
  if [[ -e "${target}" || -L "${target}" ]]; then
    [[ -f "${target}" && ! -L "${target}" ]] || \
      die "managed target is not a safe regular file: ${target}"
  fi
  candidate="$(mktemp "$(dirname -- "${target}")/.$(basename -- "${target}").XXXXXX")"
  install -o root -g root -m "${mode}" "${source}" "${candidate}"
  mv -f "${candidate}" "${target}"
}

remove_fresh_deployment_artifacts() {
  local unit_was_present=false
  [[ ! -e "${SYSTEMD_UNIT}" && ! -L "${SYSTEMD_UNIT}" ]] || unit_was_present=true
  rm -f -- \
    "${SYSTEMD_UNIT}" \
    "${EXPECTED_VERSION_FILE}" \
    "${RUNTIME_CONFIG}" \
    "${RUNTIME_ADAPTER}"
  rm -rf -- "${OPENCODEX_PREFIX}"
  if [[ "${unit_was_present}" == "true" ]] && command -v systemctl >/dev/null; then
    systemctl daemon-reload >/dev/null 2>&1 || return 1
  fi
}

recover_interrupted_fresh_bootstrap() {
  [[ -e "${BOOTSTRAP_MARKER}" || -L "${BOOTSTRAP_MARKER}" ]] || return 0
  [[ -f "${BOOTSTRAP_MARKER}" && ! -L "${BOOTSTRAP_MARKER}" ]] || \
    die "bootstrap recovery marker is unsafe: ${BOOTSTRAP_MARKER}"
  [[ "$(stat -c '%U:%G:%a' "${BOOTSTRAP_MARKER}")" == "root:root:600" ]] || \
    die "bootstrap recovery marker must be root:root 600"
  [[ "$(<"${BOOTSTRAP_MARKER}")" == "${BOOTSTRAP_MARKER_CONTENT}" ]] || \
    die "bootstrap recovery marker content is invalid"
  remove_fresh_deployment_artifacts || \
    die "interrupted fresh bootstrap artifacts could not be removed"
  rm -f -- "${BOOTSTRAP_MARKER}"
}

create_bootstrap_marker() {
  local candidate
  install -d -o root -g root -m 0700 "${BACKUP_ROOT}"
  candidate="$(mktemp "${BACKUP_ROOT}/.bootstrap-fresh.pending.XXXXXX")"
  printf '%s\n' "${BOOTSTRAP_MARKER_CONTENT}" > "${candidate}"
  chown root:root "${candidate}"
  chmod 0600 "${candidate}"
  mv -f "${candidate}" "${BOOTSTRAP_MARKER}"
}

[[ "${EUID}" -eq 0 ]] || die "run as root"
[[ -r "${OS_RELEASE_FILE}" ]] || die "${OS_RELEASE_FILE} is missing"

# shellcheck source=/dev/null
source "${OS_RELEASE_FILE}"
[[ "${ID:-}" == "ubuntu" ]] || die "Ubuntu is required"
[[ "${VERSION_ID:-}" == "24.04" ]] || die "Ubuntu 24.04 is required"
[[ "$(dpkg --print-architecture)" == "amd64" ]] || die "amd64 is required"
[[ "${OPENCODEX_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || \
  die "OPENCODEX_VERSION must be an explicit semver version"
[[ -f "${RUNTIME_ADAPTER_SOURCE}" && ! -L "${RUNTIME_ADAPTER_SOURCE}" ]] || \
  die "runtime adapter source is missing or unsafe: ${RUNTIME_ADAPTER_SOURCE}"
recover_interrupted_fresh_bootstrap

# This script is intentionally fresh-host-only. Its recovery marker owns only an
# interrupted fresh install; unmarked state is never guessed or overwritten.
for managed_artifact in \
  "${SYSTEMD_UNIT}" \
  /run/systemd/system/opencodex.service \
  /usr/lib/systemd/system/opencodex.service \
  /lib/systemd/system/opencodex.service \
  "${OPENCODEX_PREFIX}" \
  "${PACKAGE_MANIFEST}" \
  "${RUNTIME_ADAPTER}" \
  "${RUNTIME_CONFIG}" \
  "${EXPECTED_VERSION_FILE}"; do
  if [[ -e "${managed_artifact}" || -L "${managed_artifact}" ]]; then
    die "existing or unowned partial OpenCodex deployment found at ${managed_artifact}; use upgrade-opencodex.sh only for a complete managed deployment, configure-opencodex-runtime.sh only for verified legacy migration, or recover the partial state explicitly"
  fi
done

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates curl jq logrotate nginx nodejs npm procps

# The pilot does not use NFS/RPC. Disable both the socket activator and service so
# rpcbind cannot retain a wildcard listener on TCP/UDP 111.
systemctl disable --now rpcbind.socket rpcbind.service >/dev/null 2>&1 || true

node_major="$(node -p 'Number(process.versions.node.split(".")[0])')"
[[ "${node_major}" -ge 18 ]] || die "Node.js 18 or newer is required"
node_bin="$(realpath -- "$(command -v node)")"
npm_cli="$(realpath -- "$(command -v npm)")"
[[ -f "${node_bin}" && ! -L "${node_bin}" && -x "${node_bin}" ]] || \
  die "canonical Node executable is missing or unsafe: ${node_bin}"
[[ -f "${npm_cli}" && ! -L "${npm_cli}" && -r "${npm_cli}" ]] || \
  die "canonical npm CLI is missing or unsafe: ${npm_cli}"
node_mode="$(stat -c '%a' "${node_bin}")"
npm_cli_mode="$(stat -c '%a' "${npm_cli}")"
(( (8#${node_mode} & 8#022) == 0 )) || \
  die "canonical Node executable is group- or world-writable: ${node_bin}"
(( (8#${npm_cli_mode} & 8#022) == 0 )) || \
  die "canonical npm CLI is group- or world-writable: ${npm_cli}"

if ! getent group opencodex >/dev/null; then
  groupadd --system opencodex
fi

if ! id opencodex >/dev/null 2>&1; then
  useradd \
    --system \
    --gid opencodex \
    --home-dir "${OPENCODEX_HOME}" \
    --create-home \
    --shell /usr/sbin/nologin \
    opencodex
fi

create_bootstrap_marker
bootstrap_rollback_required=true
install -d -o opencodex -g opencodex -m 0700 "${OPENCODEX_HOME}"
install -d -o root -g root -m 0755 "${OPENCODEX_PREFIX}"
install -d -o root -g root -m 0755 /usr/local/libexec
# The service account must traverse this directory to read runtime.json. Secret
# material beneath it remains root-owned 0600.
install -d -o root -g root -m 0755 /etc/opencodex

# Keep the runtime immutable to the service account outside package installation.
chown -R opencodex:opencodex "${OPENCODEX_PREFIX}"
restore_prefix_owner() {
  chown -R root:root "${OPENCODEX_PREFIX}"
}
cleanup() {
  status=$?
  rollback_succeeded=true
  trap - EXIT
  set +e
  restore_prefix_owner || true
  if [[ "${status}" -ne 0 && "${bootstrap_rollback_required}" == "true" ]]; then
    if ! remove_fresh_deployment_artifacts; then
      status=70
      rollback_succeeded=false
    fi
  fi
  if [[ "${bootstrap_rollback_required}" == "true" && \
        "${rollback_succeeded}" == "true" ]]; then
    rm -f -- "${BOOTSTRAP_MARKER}" || status=70
  fi
  exit "${status}"
}
trap cleanup EXIT

atomic_install_asset "${RUNTIME_ADAPTER_SOURCE}" "${RUNTIME_ADAPTER}" 0755
if [[ -e "${RUNTIME_CONFIG}" || -L "${RUNTIME_CONFIG}" ]]; then
  [[ -f "${RUNTIME_CONFIG}" && ! -L "${RUNTIME_CONFIG}" ]] || \
    die "managed target is not a safe regular file: ${RUNTIME_CONFIG}"
fi
runtime_candidate="$(mktemp /etc/opencodex/.runtime.json.XXXXXX)"
jq -n \
  --arg home "${OPENCODEX_HOME}" \
  --arg prefix "${OPENCODEX_PREFIX}" \
  --arg node_bin "${node_bin}" \
  --arg npm_cli "${npm_cli}" \
  --arg ocx_entry "${OCX_ENTRY}" \
  '{
    schema_version: 1,
    runtime_kind: "node",
    home: $home,
    prefix: $prefix,
    node_bin: $node_bin,
    npm_cli: $npm_cli,
    ocx_entry: $ocx_entry
  }' > "${runtime_candidate}"
chown root:root "${runtime_candidate}"
chmod 0644 "${runtime_candidate}"
mv -f "${runtime_candidate}" "${RUNTIME_CONFIG}"

runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" npm install --global --prefix "${OPENCODEX_PREFIX}" \
  --ignore-scripts \
  "${PACKAGE_NAME}@${OPENCODEX_VERSION}"
runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" prepare-bundled-bun "${OPENCODEX_VERSION}" >/dev/null
restore_prefix_owner

"${RUNTIME_ADAPTER}" check >/dev/null
installed_version="$(runuser -u opencodex -- "${RUNTIME_ADAPTER}" ocx --version)"
[[ "${installed_version}" == "opencodex ${OPENCODEX_VERSION}" ]] || \
  die "installed version ${installed_version} does not match ${OPENCODEX_VERSION}"
printf 'Installed %s\n' "${installed_version}"

atomic_install_asset \
  "${ASSET_DIR}/systemd/opencodex.service" \
  "${SYSTEMD_UNIT}" \
  0644
install -d -o root -g root -m 0700 /etc/nginx/private
expected_version_candidate="$(mktemp /etc/opencodex/.expected-version.XXXXXX)"
printf '%s\n' "${OPENCODEX_VERSION}" > "${expected_version_candidate}"
chown root:root "${expected_version_candidate}"
chmod 0644 "${expected_version_candidate}"
mv -f "${expected_version_candidate}" "${EXPECTED_VERSION_FILE}"
readonly NGINX_KEY_MAP="/etc/nginx/private/opencodex-api-key-map.conf"
if [[ -L "${NGINX_KEY_MAP}" ]]; then
  die "${NGINX_KEY_MAP} must not be a symlink"
elif [[ ! -e "${NGINX_KEY_MAP}" ]]; then
  install -o root -g root -m 0600 \
    "${ASSET_DIR}/nginx/opencodex-api-key-map.deny-all.conf" \
    "${NGINX_KEY_MAP}"
elif [[ ! -f "${NGINX_KEY_MAP}" ]]; then
  die "${NGINX_KEY_MAP} must be a regular file"
else
  chown root:root "${NGINX_KEY_MAP}"
  chmod 0600 "${NGINX_KEY_MAP}"
fi
install -o root -g root -m 0644 \
  "${ASSET_DIR}/nginx/opencodex-api.conf" \
  /etc/nginx/conf.d/opencodex-api.conf
install -d -o root -g root -m 0755 /etc/nginx/snippets
install -o root -g root -m 0644 \
  "${ASSET_DIR}/nginx/opencodex-proxy.conf" \
  /etc/nginx/snippets/opencodex-proxy.conf
install -o root -g root -m 0644 \
  "${WEBSOCKET_PROXY_SOURCE}" \
  "${WEBSOCKET_PROXY_TARGET}"
if [[ -L "${FEATURE_FLAGS_TARGET}" ]]; then
  die "${FEATURE_FLAGS_TARGET} must not be a symlink"
elif [[ ! -e "${FEATURE_FLAGS_TARGET}" ]]; then
  install -o root -g root -m 0600 \
    "${FEATURE_FLAGS_SOURCE}" \
    "${FEATURE_FLAGS_TARGET}"
elif [[ ! -f "${FEATURE_FLAGS_TARGET}" ]]; then
  die "${FEATURE_FLAGS_TARGET} must be a regular file"
else
  chown root:root "${FEATURE_FLAGS_TARGET}"
  chmod 0600 "${FEATURE_FLAGS_TARGET}"
fi
install -o root -g root -m 0644 \
  "${ASSET_DIR}/logrotate/opencodex" \
  /etc/logrotate.d/opencodex
install -o root -g root -m 0644 \
  "${ASSET_DIR}/sysctl/99-opencodex-memory.conf" \
  /etc/sysctl.d/99-opencodex-memory.conf

# The distribution default listens publicly on port 80; this pilot must be loopback-only.
rm -f /etc/nginx/sites-enabled/default

systemctl daemon-reload
sysctl --system >/dev/null
nginx -t
logrotate --debug /etc/logrotate.conf >/dev/null 2>&1

# Intentionally do not start or enable OpenCodex before interactive account setup.
systemctl disable opencodex.service >/dev/null 2>&1 || true
systemctl enable nginx.service >/dev/null
systemctl restart nginx.service

rm -f -- "${BOOTSTRAP_MARKER}"
bootstrap_rollback_required=false
printf '\nHost preparation complete. OpenCodex is installed but not started.\n'
printf 'Next: sudo -u opencodex %s ocx setup\n' "${RUNTIME_ADAPTER}"

#!/usr/bin/env bash
set -Eeuo pipefail

readonly KEY_DIR="/etc/opencodex"
readonly KEY_FILE="${KEY_DIR}/gateway-api-key"
readonly MAP_DIR="/etc/nginx/private"
readonly MAP_FILE="${MAP_DIR}/opencodex-api-key-map.conf"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "${EUID}" -eq 0 ]] || die "run as root"
command -v nginx >/dev/null || die "nginx is not installed"
command -v systemctl >/dev/null || die "systemctl is not available"

# The service account must be able to traverse /etc/opencodex to read the
# root-managed, mode-0644 runtime contract. Secrets in this directory remain
# root-owned mode 0600. Nginx's private directory has no shared runtime
# contract and stays root-only.
install -d -o root -g root -m 0755 "${KEY_DIR}"
install -d -o root -g root -m 0700 "${MAP_DIR}"
[[ ! -L "${KEY_FILE}" ]] || die "${KEY_FILE} must not be a symlink"
[[ ! -L "${MAP_FILE}" ]] || die "${MAP_FILE} must not be a symlink"
[[ ! -e "${KEY_FILE}" || -f "${KEY_FILE}" ]] || die "${KEY_FILE} must be a regular file"
[[ ! -e "${MAP_FILE}" || -f "${MAP_FILE}" ]] || die "${MAP_FILE} must be a regular file"

gateway_key=""
if [[ -t 0 ]]; then
  IFS= read -r -s -p "Gateway API key (32-128 base64url/hex characters): " gateway_key
  printf '\n' >&2
else
  IFS= read -r gateway_key || [[ -n "${gateway_key}" ]] || \
    die "failed to read the gateway API key from stdin"
fi
[[ "${gateway_key}" =~ ^[A-Za-z0-9_-]{32,128}$ ]] || \
  die "key must contain 32-128 characters from A-Z, a-z, 0-9, underscore, and hyphen"

umask 077
key_tmp="$(mktemp "${KEY_DIR}/.gateway-api-key.new.XXXXXX")"
map_tmp="$(mktemp "${MAP_DIR}/.opencodex-api-key-map.new.XXXXXX")"
backup_dir="$(mktemp -d "${KEY_DIR}/.gateway-key-backup.XXXXXX")"
had_key=false
had_map=false
rollback_needed=false

rollback() {
  if [[ "${had_key}" == "true" ]]; then
    install -o root -g root -m 0600 "${backup_dir}/gateway-api-key" "${KEY_FILE}"
  else
    rm -f "${KEY_FILE}"
  fi
  if [[ "${had_map}" == "true" ]]; then
    install -o root -g root -m 0600 "${backup_dir}/opencodex-api-key-map.conf" "${MAP_FILE}"
  else
    rm -f "${MAP_FILE}"
  fi
  nginx -t >/dev/null 2>&1 || true
  systemctl reload nginx.service >/dev/null 2>&1 || true
}

cleanup() {
  status=$?
  trap - EXIT
  if [[ "${status}" -ne 0 && "${rollback_needed}" == "true" ]]; then
    rollback
  fi
  rm -f "${key_tmp}" "${map_tmp}"
  rm -rf "${backup_dir}"
  gateway_key=""
  exit "${status}"
}
trap cleanup EXIT

if [[ -f "${KEY_FILE}" ]]; then
  had_key=true
  install -o root -g root -m 0600 "${KEY_FILE}" "${backup_dir}/gateway-api-key"
fi
if [[ -f "${MAP_FILE}" ]]; then
  had_map=true
  install -o root -g root -m 0600 "${MAP_FILE}" "${backup_dir}/opencodex-api-key-map.conf"
fi

printf '%s\n' "${gateway_key}" > "${key_tmp}"
printf '%s\n' \
  '# Managed by configure-gateway-key.sh. Do not copy this file into source control.' \
  'map $http_x_opencodex_api_key $opencodex_api_key_valid {' \
  '    default 0;' \
  "    ~^${gateway_key}$ 1;" \
  '}' > "${map_tmp}"
chown root:root "${key_tmp}" "${map_tmp}"
chmod 0600 "${key_tmp}" "${map_tmp}"

rollback_needed=true
mv -f "${key_tmp}" "${KEY_FILE}"
mv -f "${map_tmp}" "${MAP_FILE}"

nginx -t
systemctl reload nginx.service
rollback_needed=false

printf 'Gateway API key installed in root-only files and Nginx reloaded.\n'

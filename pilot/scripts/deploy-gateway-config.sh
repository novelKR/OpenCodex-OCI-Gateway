#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly PILOT_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly API_SOURCE="${PILOT_ROOT}/nginx/opencodex-api.conf"
readonly PROXY_SOURCE="${PILOT_ROOT}/nginx/opencodex-proxy.conf"
readonly WEBSOCKET_PROXY_SOURCE="${PILOT_ROOT}/nginx/opencodex-websocket-proxy.conf"
readonly FEATURE_FLAGS_SOURCE="${PILOT_ROOT}/nginx/opencodex-feature-flags.deny-all.conf"
readonly API_TARGET="/etc/nginx/conf.d/opencodex-api.conf"
readonly PROXY_TARGET="/etc/nginx/snippets/opencodex-proxy.conf"
readonly WEBSOCKET_PROXY_TARGET="/etc/nginx/snippets/opencodex-websocket-proxy.conf"
readonly GATEWAY_KEY_FILE="/etc/opencodex/gateway-api-key"
readonly FEATURE_FLAGS_FILE="/etc/nginx/private/opencodex-feature-flags.conf"

backup_dir=""
rollback_required=false
websocket_proxy_was_present=false
feature_flags_was_present=false

validate_feature_flags_file() {
  awk '
    /^[[:space:]]*($|#)/ { next }
    /^[[:space:]]*set[[:space:]]+\$opencodex_voice_enabled[[:space:]]+[01];[[:space:]]*$/ {
      count++
      next
    }
    { invalid = 1 }
    END { if (invalid || count != 1) exit 1 }
  ' "$1"
}

cleanup() {
  status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  if [[ "${rollback_required}" == "true" ]]; then
    printf 'Deployment failed; restoring the previous Nginx configuration.\n' >&2
    rollback_failed=false
    if ! install -o root -g root -m 0644 "${backup_dir}/opencodex-api.conf" "${API_TARGET}"; then
      printf 'CRITICAL: failed to restore %s.\n' "${API_TARGET}" >&2
      rollback_failed=true
    fi
    if ! install -o root -g root -m 0644 "${backup_dir}/opencodex-proxy.conf" "${PROXY_TARGET}"; then
      printf 'CRITICAL: failed to restore %s.\n' "${PROXY_TARGET}" >&2
      rollback_failed=true
    fi
    if [[ "${websocket_proxy_was_present}" == "true" ]]; then
      if ! install -o root -g root -m 0644 "${backup_dir}/opencodex-websocket-proxy.conf" "${WEBSOCKET_PROXY_TARGET}"; then
        printf 'CRITICAL: failed to restore %s.\n' "${WEBSOCKET_PROXY_TARGET}" >&2
        rollback_failed=true
      fi
    else
      rm -f -- "${WEBSOCKET_PROXY_TARGET}" || rollback_failed=true
    fi
    if [[ "${feature_flags_was_present}" == "false" ]]; then
      rm -f -- "${FEATURE_FLAGS_FILE}" || rollback_failed=true
    fi
    if [[ "${rollback_failed}" == "false" ]]; then
      if ! nginx -t; then
        printf 'CRITICAL: restored Nginx configuration did not validate.\n' >&2
        rollback_failed=true
      elif ! systemctl reload nginx.service; then
        printf 'WARNING: rollback reload failed; attempting a restart with the validated configuration.\n' >&2
        if ! systemctl restart nginx.service; then
          printf 'CRITICAL: rollback restart also failed.\n' >&2
          rollback_failed=true
        fi
      fi
      if ! systemctl is-active --quiet nginx.service; then
        printf 'CRITICAL: Nginx is not active after rollback.\n' >&2
        rollback_failed=true
      fi
    fi
    if [[ "${rollback_failed}" == "true" ]]; then
      status=70
      printf 'CRITICAL: Nginx rollback could not be verified; manual recovery is required.\n' >&2
    fi
  fi
  if [[ -n "${backup_dir}" ]] && ! rm -rf -- "${backup_dir}"; then
    printf 'CRITICAL: failed to remove the credential-bearing deployment directory.\n' >&2
    status=70
  fi
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 131' QUIT
trap 'exit 143' TERM

if [[ "${EUID}" -ne 0 ]]; then
  printf 'ERROR: run as root.\n' >&2
  exit 2
fi
for path in "${API_SOURCE}" "${PROXY_SOURCE}" "${WEBSOCKET_PROXY_SOURCE}" "${FEATURE_FLAGS_SOURCE}" "${API_TARGET}" "${PROXY_TARGET}"; do
  if [[ ! -f "${path}" || -L "${path}" ]]; then
    printf 'ERROR: required Nginx file is missing or a symbolic link: %s\n' "${path}" >&2
    exit 2
  fi
done
if ! validate_feature_flags_file "${FEATURE_FLAGS_SOURCE}"; then
  printf 'ERROR: source feature flags must contain exactly one Voice value of 0 or 1.\n' >&2
  exit 2
fi
if [[ ! -f "${GATEWAY_KEY_FILE}" || -L "${GATEWAY_KEY_FILE}" ]]; then
  printf 'ERROR: gateway key file is missing or unsafe.\n' >&2
  exit 2
fi
if [[ -e "${WEBSOCKET_PROXY_TARGET}" ]]; then
  [[ -f "${WEBSOCKET_PROXY_TARGET}" && ! -L "${WEBSOCKET_PROXY_TARGET}" ]] || {
    printf 'ERROR: required Nginx file is missing or a symbolic link: %s\n' "${WEBSOCKET_PROXY_TARGET}" >&2
    exit 2
  }
  websocket_proxy_was_present=true
fi
if [[ -e "${FEATURE_FLAGS_FILE}" ]]; then
  [[ -f "${FEATURE_FLAGS_FILE}" && ! -L "${FEATURE_FLAGS_FILE}" && \
      "$(stat -c '%U:%G:%a' "${FEATURE_FLAGS_FILE}")" == "root:root:600" ]] || {
    printf 'ERROR: feature flags file is unsafe: %s\n' "${FEATURE_FLAGS_FILE}" >&2
    exit 2
  }
  validate_feature_flags_file "${FEATURE_FLAGS_FILE}" || {
    printf 'ERROR: feature flags file must contain exactly one Voice value of 0 or 1.\n' >&2
    exit 2
  }
  feature_flags_was_present=true
fi

gateway_key="$(<"${GATEWAY_KEY_FILE}")"
if [[ "$(stat -c '%U:%G:%a' "${GATEWAY_KEY_FILE}")" != "root:root:600" ||
      ! "${gateway_key}" =~ ^[A-Za-z0-9_-]{32,128}$ ]]; then
  printf 'ERROR: gateway key ownership, mode, or format is invalid.\n' >&2
  exit 2
fi

umask 077
if [[ "$(stat -c '%U:%G' /run)" != "root:root" ]] ||
   (( (8#$(stat -c '%a' /run) & 8#022) != 0 )); then
  printf 'ERROR: /run must be root-owned and not group/other writable.\n' >&2
  exit 2
fi
backup_dir="$(mktemp -d /run/opencodex-gateway-deploy.XXXXXX)"
install -o root -g root -m 0600 "${API_TARGET}" "${backup_dir}/opencodex-api.conf"
install -o root -g root -m 0600 "${PROXY_TARGET}" "${backup_dir}/opencodex-proxy.conf"
if [[ "${websocket_proxy_was_present}" == "true" ]]; then
  install -o root -g root -m 0600 "${WEBSOCKET_PROXY_TARGET}" "${backup_dir}/opencodex-websocket-proxy.conf"
fi
if [[ "${feature_flags_was_present}" == "false" ]]; then
  install -o root -g root -m 0600 "${FEATURE_FLAGS_SOURCE}" "${FEATURE_FLAGS_FILE}"
fi
printf 'X-OpenCodex-API-Key: %s\nExpect:\n' "${gateway_key}" > "${backup_dir}/health.headers"
printf 'X-OpenCodex-API-Key: definitely-invalid-pilot-key\nExpect:\n' > "${backup_dir}/invalid.headers"
gateway_key=""

rollback_required=true
install -o root -g root -m 0644 "${API_SOURCE}" "${API_TARGET}"
install -o root -g root -m 0644 "${PROXY_SOURCE}" "${PROXY_TARGET}"
install -o root -g root -m 0644 "${WEBSOCKET_PROXY_SOURCE}" "${WEBSOCKET_PROXY_TARGET}"
nginx -t
systemctl reload nginx.service
systemctl is-active --quiet nginx.service

health_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 --header "@${backup_dir}/health.headers" \
  http://127.0.0.1:18080/__gateway_health || true)"
if [[ "${health_code}" != "200" ]]; then
  printf 'ERROR: authorized gateway health returned HTTP %s.\n' "${health_code:-000}" >&2
  exit 1
fi

invalid_code="$(curl --silent --show-error --output /dev/null \
  --dump-header "${backup_dir}/invalid-response.headers" --write-out '%{http_code}' \
  --max-time 10 --header "@${backup_dir}/invalid.headers" \
  http://127.0.0.1:18080/v1/models || true)"
if [[ "${invalid_code}" != "401" ]] || \
   ! tr -d '\r' < "${backup_dir}/invalid-response.headers" | \
     grep -Fxi -- 'X-OpenCodex-Gateway-Rejection: api-key' >/dev/null; then
  printf 'ERROR: invalid gateway key returned HTTP %s or lacked the rejection marker.\n' \
    "${invalid_code:-000}" >&2
  exit 1
fi

rollback_required=false
printf 'Nginx gateway configuration applied; authorized health and marked key rejection passed.\n'

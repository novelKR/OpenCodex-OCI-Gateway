#!/usr/bin/env bash
# Toggle reviewed public feature gates without exposing the OpenCodex dashboard.
set -euo pipefail

readonly FEATURE_FLAGS_FILE="/etc/nginx/private/opencodex-feature-flags.conf"

usage() {
  printf '%s\n' 'Usage: sudo ./configure-gateway-features.sh voice on|off'
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[[ "${EUID}" -eq 0 ]] || die 'run as root'
[[ "$#" -eq 2 && "$1" == "voice" ]] || { usage >&2; exit 2; }
case "$2" in
  on) value=1 ;;
  off) value=0 ;;
  *) usage >&2; exit 2 ;;
esac

[[ -f "${FEATURE_FLAGS_FILE}" && ! -L "${FEATURE_FLAGS_FILE}" ]] || die 'feature flags file is unavailable'
[[ "$(stat -c '%U:%G:%a' "${FEATURE_FLAGS_FILE}")" == "root:root:600" ]] || die 'feature flags file must be root:root 0600'
temporary="$(mktemp /etc/nginx/private/.opencodex-feature-flags.XXXXXX)"
backup="$(mktemp /run/opencodex-feature-flags.XXXXXX)"
cleanup() { rm -f -- "${temporary}" "${backup}"; }
trap cleanup EXIT
cp -p "${FEATURE_FLAGS_FILE}" "${backup}"
printf '# Root-owned public API feature flags.\nset $opencodex_voice_enabled %s;\n' "${value}" > "${temporary}"
chown root:root "${temporary}"
chmod 0600 "${temporary}"
mv -f "${temporary}" "${FEATURE_FLAGS_FILE}"
if ! nginx -t || ! systemctl reload nginx.service; then
  install -o root -g root -m 0600 "${backup}" "${FEATURE_FLAGS_FILE}"
  nginx -t && systemctl reload nginx.service || true
  die 'Nginx rejected the requested feature setting; previous state was restored'
fi
printf 'voice_enabled=%s\n' "${value}"

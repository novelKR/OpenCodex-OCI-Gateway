#!/usr/bin/env bash
# Install only the local-only development relay LaunchAgent. This script never
# references the production label, helper root, or service plist.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELAY_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly LABEL="io.github.novelkr.opencodex-relay.dev"
readonly PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
readonly TEMPLATE="${RELAY_ROOT}/macos/${LABEL}.plist.in"

usage() {
  printf '%s\n' 'Usage: install-local-dev-service.sh install --relay-bin PATH --config PATH | stop | uninstall | status'
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

safe_xml_path() {
  [[ "$1" != *$'\n'* && "$1" != *$'\r'* && "$1" != *'<'* && "$1" != *'>'* && \
     "$1" != *'&'* && "$1" != *'"'* && "$1" != *"'"* && "$1" != *'|'* && \
     "$1" != *'\\'* && "$1" != *'%'* ]]
}

action="${1:-}"
shift || true
case "$action" in
  install)
    [[ "${1:-}" == --relay-bin && "${3:-}" == --config && $# -eq 4 ]] || { usage >&2; exit 2; }
    relay_bin="$2"
    config_path="$4"
    [[ -x "$relay_bin" && ! -L "$relay_bin" ]] || die 'local development relay binary is unavailable or unsafe'
    [[ -f "$config_path" && ! -L "$config_path" ]] || die 'local development relay config is unavailable or unsafe'
    safe_xml_path "$relay_bin" && safe_xml_path "$config_path" && safe_xml_path "$HOME" || die 'service path contains unsupported XML characters'
    ;;
  stop|uninstall|status)
    [[ $# -eq 0 ]] || { usage >&2; exit 2; }
    ;;
  *) usage >&2; exit 2 ;;
esac

[[ "$(uname -s)" == Darwin ]] || die 'local development service is supported only on macOS'
uid="$(id -u)"

case "$action" in
  status)
    if launchctl print "gui/${uid}/${LABEL}" >/dev/null 2>&1; then
      printf 'relay_dev_service_active=true manager=launchd\n'
    else
      printf 'relay_dev_service_active=false manager=launchd\n'
    fi
    ;;
  stop)
    launchctl bootout "gui/${uid}" "$PLIST" >/dev/null 2>&1 || true
    printf 'relay_dev_service=stopped manager=launchd\n'
    ;;
  uninstall)
    launchctl bootout "gui/${uid}" "$PLIST" >/dev/null 2>&1 || true
    if [[ -e "$PLIST" || -L "$PLIST" ]]; then
      [[ -f "$PLIST" && ! -L "$PLIST" ]] || die 'local development service plist is unsafe'
      rm -f -- "$PLIST"
    fi
    printf 'relay_dev_service=uninstalled manager=launchd\n'
    ;;
  install)
    [[ -f "$TEMPLATE" && ! -L "$TEMPLATE" ]] || die 'local development LaunchAgent template is unavailable'
    mkdir -p "$(dirname -- "$PLIST")" "${HOME}/Library/Logs/opencodex-relay-dev"
    umask 077
    candidate="$(mktemp "${PLIST}.XXXXXX")"
    trap 'rm -f -- "${candidate:-}"' EXIT
    sed -e "s|__RELAY_BIN__|${relay_bin}|g" -e "s|__RELAY_CONFIG__|${config_path}|g" -e "s|__HOME__|${HOME}|g" \
      "$TEMPLATE" > "$candidate"
    chmod 0600 "$candidate"
    launchctl bootout "gui/${uid}" "$PLIST" >/dev/null 2>&1 || true
    mv -f -- "$candidate" "$PLIST"
    launchctl bootstrap "gui/${uid}" "$PLIST"
    launchctl kickstart -k "gui/${uid}/${LABEL}"
    printf 'relay_dev_service=installed manager=launchd\n'
    ;;
esac

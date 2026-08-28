#!/usr/bin/env bash
# Start, reuse, or stop the Cloudflare Access SSH tunnel used for the OCI Dashboard.
#
# This script deliberately delegates Access authentication to the ProxyCommand in
# ~/.ssh/config. It never stores or supplies a Cloudflare token, TOTP value, or
# SSH private-key path.
set -euo pipefail

SSH_BIN="${SSH_BIN:-ssh}"
OPEN_BIN="${OPEN_BIN:-open}"
SSH_HOST="${OCX_SSH_HOST:-ocx-ssh}"
DASHBOARD_PORT=11010
CALLBACK_PORT=1455
OPEN_BROWSER=true

usage() {
  cat <<'USAGE'
Usage:
  open-dashboard.sh [start] [--host SSH_ALIAS] [--no-open]
  open-dashboard.sh stop [--host SSH_ALIAS]
  open-dashboard.sh status [--host SSH_ALIAS]

Defaults:
  SSH alias:       ocx-ssh
  Dashboard port:  11010
  OAuth callback:  1455

The SSH alias must contain a Cloudflare Access ProxyCommand and these forwards:
  ProxyCommand /absolute/path/to/cloudflared access ssh --hostname %h
  LocalForward 11010 127.0.0.1:10100
  LocalForward 1455 127.0.0.1:1455

The script starts a dedicated SSH ControlMaster. It keeps the Dashboard tunnel
alive after the command exits; run `stop` to close it.
USAGE
}

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

is_safe_alias() {
  [[ "$1" =~ ^[A-Za-z0-9._-]+$ ]]
}

action=start
if [[ $# -gt 0 && "$1" != --* ]]; then
  action="$1"
  shift
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)
      [[ $# -ge 2 ]] || fail '--host requires an SSH alias'
      SSH_HOST="$2"
      shift 2
      ;;
    --no-open)
      OPEN_BROWSER=false
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

case "$action" in
  start|stop|status) ;;
  *)
    usage >&2
    fail "unknown action: $action"
    ;;
esac

is_safe_alias "$SSH_HOST" || fail 'SSH alias may contain only letters, numbers, dot, underscore, and hyphen'
command -v "$SSH_BIN" >/dev/null 2>&1 || fail "ssh executable not found: $SSH_BIN"

CONTROL_PATH="$HOME/.ssh/ocx-dashboard-${SSH_HOST}.sock"
DASHBOARD_URL="http://127.0.0.1:${DASHBOARD_PORT}/#codex-auth"

master_running() {
  "$SSH_BIN" -S "$CONTROL_PATH" -O check "$SSH_HOST" >/dev/null 2>&1
}

assert_access_alias() {
  local effective proxy_command
  effective="$($SSH_BIN -G "$SSH_HOST")" || fail "could not read SSH configuration for alias: $SSH_HOST"
  proxy_command="$(awk '$1 == "proxycommand" { $1=""; sub(/^ /, ""); print; exit }' <<<"$effective")"
  [[ "$proxy_command" == *"cloudflared access ssh"* ]] || fail \
    "SSH alias '$SSH_HOST' has no Cloudflare Access ProxyCommand; refusing a direct SSH path"
  awk -v port="$DASHBOARD_PORT" '$1 == "localforward" && $2 == port && $3 == "[127.0.0.1]:10100" { found = 1 } END { exit !found }' \
    <<<"$effective" || fail \
      "SSH alias '$SSH_HOST' must forward ${DASHBOARD_PORT} to 127.0.0.1:10100"
  awk -v port="$CALLBACK_PORT" '$1 == "localforward" && $2 == port && $3 == "[127.0.0.1]:1455" { found = 1 } END { exit !found }' \
    <<<"$effective" || fail \
      "SSH alias '$SSH_HOST' must forward ${CALLBACK_PORT} to 127.0.0.1:1455"
}

case "$action" in
  status)
    if master_running; then
      printf 'Dashboard tunnel is active: %s\n' "$DASHBOARD_URL"
      exit 0
    fi
    printf 'Dashboard tunnel is not active for SSH alias: %s\n' "$SSH_HOST"
    exit 1
    ;;
  stop)
    if ! master_running; then
      printf 'Dashboard tunnel is not active for SSH alias: %s\n' "$SSH_HOST"
      exit 0
    fi
    "$SSH_BIN" -S "$CONTROL_PATH" -O exit "$SSH_HOST"
    printf 'Dashboard tunnel stopped for SSH alias: %s\n' "$SSH_HOST"
    exit 0
    ;;
esac

assert_access_alias

if master_running; then
  printf 'Dashboard tunnel is already active: %s\n' "$DASHBOARD_URL"
else
  [[ ! -e "$CONTROL_PATH" ]] || fail \
    "stale or foreign control socket exists: $CONTROL_PATH (verify it is unused before removing it)"
  printf 'Starting Cloudflare Access SSH tunnel for %s...\n' "$SSH_HOST"
  printf 'Complete Cloudflare Access email/TOTP in the browser if prompted.\n'
  "$SSH_BIN" -M -f -N \
    -S "$CONTROL_PATH" \
    -o ExitOnForwardFailure=yes \
    -o ServerAliveInterval=15 \
    -o ServerAliveCountMax=3 \
    "$SSH_HOST"
  master_running || fail 'SSH reported success but the Dashboard tunnel control socket is unavailable'
  printf 'Dashboard tunnel is active: %s\n' "$DASHBOARD_URL"
fi

if [[ "$OPEN_BROWSER" == true ]]; then
  command -v "$OPEN_BIN" >/dev/null 2>&1 || fail "macOS open executable not found: $OPEN_BIN"
  "$OPEN_BIN" "$DASHBOARD_URL"
fi

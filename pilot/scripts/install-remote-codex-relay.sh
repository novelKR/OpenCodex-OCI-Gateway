#!/usr/bin/env bash
# Install a signed private-GitHub relay release for the dedicated Remote Codex
# home, then perform an explicit external- or local-relay routing switch.
set -euo pipefail

readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"
readonly REMOTE_CONFIG="${CONFIG_DIR}/remote-opencodex.json"
readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"
readonly RELAY_CONFIG="${CONFIG_DIR}/relay.json"
readonly RELAY_INSTALLER="/home/ubuntu/.local/lib/opencodex-relay/relay-installer/install-relay.sh"
readonly ROUTING="/home/ubuntu/.local/lib/opencodex-relay/configure-remote-codex-routing.sh"

usage() {
  cat <<'USAGE'
Usage:
  install-remote-codex-relay.sh install VERSION \
    --github-repo OWNER/REPO --github-token-file PATH --public-key PEM \
    --upstream HTTPS_URL --allow-remote-interruption \
    [--bounded-json-model MODEL] [--migrate-legacy]
  install-remote-codex-relay.sh install-local VERSION \
    --github-repo OWNER/REPO --github-token-file PATH --public-key PEM \
    --bounded-json-model MODEL \
    --allow-remote-interruption [--migrate-legacy]

Run as ubuntu, without sudo. The GitHub token file and public key are
pre-existing owner-only inputs; this script never copies their contents. It
installs the signed relay with the dedicated Remote catalog and then restarts
the managed AppServer. install requires MODE=external and selects the external
gateway. install-local requires MODE=loopback, fixes the upstream to the local
OpenCodex listener, keeps catalog ownership in the Remote manager, and selects
ROUTING_MODE=local-relay. Neither action falls back to the other upstream.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_version() {
  [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die 'VERSION must be explicit semver'
}

github_repo_valid() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]
}

require_external_remote_mode() {
  [[ -f "$REMOTE_CONFIG" && ! -L "$REMOTE_CONFIG" ]] || \
    die "Remote configuration is unavailable: $REMOTE_CONFIG"
  [[ "$(stat -c '%U:%G:%a' "$REMOTE_CONFIG")" == "ubuntu:ubuntu:600" ]] || \
    die "Remote configuration must be owned by ubuntu with mode 600"
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || \
    die "Remote configuration loader is unavailable: $CONFIG_LOADER"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$REMOTE_CONFIG" "$REMOTE_HOME_PATH"
  [[ "${REMOTE_HOME:-}" == "$REMOTE_HOME_PATH" ]] || \
    die "REMOTE_HOME in $REMOTE_CONFIG is not the approved Remote home"
  [[ "${MODE:-}" == "external" ]] || \
    die "MODE=external is required; do not install the external relay on a loopback OpenCodex host"
}

require_loopback_remote_mode() {
  [[ -f "$REMOTE_CONFIG" && ! -L "$REMOTE_CONFIG" ]] || \
    die "Remote configuration is unavailable: $REMOTE_CONFIG"
  [[ "$(stat -c '%U:%G:%a' "$REMOTE_CONFIG")" == "ubuntu:ubuntu:600" ]] || \
    die "Remote configuration must be owned by ubuntu with mode 600"
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || \
    die "Remote configuration loader is unavailable: $CONFIG_LOADER"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$REMOTE_CONFIG" "$REMOTE_HOME_PATH"
  [[ "${REMOTE_HOME:-}" == "$REMOTE_HOME_PATH" ]] || \
    die "REMOTE_HOME in $REMOTE_CONFIG is not the approved Remote home"
  [[ "${MODE:-}" == "loopback" ]] || \
    die "MODE=loopback is required for local-relay installation"
}

verify_installed_relay_config() {
  local model
  [[ -f "$RELAY_CONFIG" && ! -L "$RELAY_CONFIG" ]] || \
    die "installed relay configuration is unavailable: $RELAY_CONFIG"
  [[ "$(stat -c '%U:%G:%a' "$RELAY_CONFIG")" == "ubuntu:ubuntu:600" ]] || \
    die "installed relay configuration must be owned by ubuntu with mode 600"
  if [[ "$action" == "install-local" ]]; then
    jq -e --arg catalog "${REMOTE_HOME_PATH}/opencodex-catalog.json" '
      .upstream_mode == "local_opencodex"
      and .upstream_base_url == "http://127.0.0.1:10100/v1"
      and .credentials.source == "none"
      and .responses.websocket_mode == "http_fallback"
      and .catalog.owner == "remote_manager"
      and .catalog.path == $catalog
      and .catalog.manage_app_server == false
    ' "$RELAY_CONFIG" >/dev/null || \
      die 'installed local relay config does not satisfy the Remote local-relay contract'
  fi
  for model in "${bounded_json_models[@]+"${bounded_json_models[@]}"}"; do
    jq -e --arg model "$model" '
      (.responses.model_modes // {})
      | to_entries
      | map(select((.key | ascii_downcase) == ($model | ascii_downcase) and .value == "bounded_json"))
      | length == 1
    ' "$RELAY_CONFIG" >/dev/null || \
      die "installed relay config is missing bounded_json policy for model: $model"
  done
}

action="${1:-}"
[[ "$action" == "install" || "$action" == "install-local" ]] || { usage >&2; exit 2; }
shift

version="${1:-}"
[[ -n "$version" ]] || { usage >&2; exit 2; }
shift
require_version "$version"

github_repo=""
github_token_file=""
public_key=""
upstream=""
allow_interruption=false
migrate_legacy=false
bounded_json_models=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --github-repo) github_repo="${2:-}"; shift 2 ;;
    --github-token-file) github_token_file="${2:-}"; shift 2 ;;
    --public-key) public_key="${2:-}"; shift 2 ;;
    --upstream) upstream="${2:-}"; shift 2 ;;
    --bounded-json-model) bounded_json_models+=("${2:-}"); shift 2 ;;
    --allow-remote-interruption) allow_interruption=true; shift ;;
    --migrate-legacy) migrate_legacy=true; shift ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ "$(id -un)" == "ubuntu" ]] || die 'run this script as ubuntu, without sudo'
if [[ "$action" == "install" ]]; then
  require_external_remote_mode
else
  require_loopback_remote_mode
fi
github_repo_valid "$github_repo" || die '--github-repo must be OWNER/REPO'
[[ -f "$github_token_file" && ! -L "$github_token_file" ]] || \
  die '--github-token-file must be an existing regular file'
[[ -f "$public_key" && ! -L "$public_key" ]] || die '--public-key must be an existing regular PEM file'
if [[ "$action" == "install" ]]; then
  [[ "$upstream" =~ ^https://[^/?#]+/v1$ ]] || die '--upstream must be an HTTPS /v1 URL'
else
  [[ -z "$upstream" ]] || die '--upstream is fixed for install-local and must not be supplied'
  ((${#bounded_json_models[@]} > 0)) || die 'install-local requires at least one --bounded-json-model'
  upstream="http://127.0.0.1:10100/v1"
fi
for model in "${bounded_json_models[@]+"${bounded_json_models[@]}"}"; do
  [[ -n "$model" && "${model# }" == "$model" && "${model% }" == "$model" ]] || \
    die '--bounded-json-model values must be non-empty and have no surrounding spaces'
done
[[ "$allow_interruption" == true ]] || die '--allow-remote-interruption is required'
[[ -x "$RELAY_INSTALLER" ]] || die "Remote relay installer is unavailable: $RELAY_INSTALLER"
[[ -x "$ROUTING" ]] || die "Remote routing helper is unavailable: $ROUTING"
[[ -d "$REMOTE_HOME_PATH" && ! -L "$REMOTE_HOME_PATH" ]] || \
  die "Remote Codex home is unavailable: $REMOTE_HOME_PATH"

# This read-only service-account check must run before the signed installer can
# change relay binaries, service state, or Native Codex routing.
if [[ "$action" == "install-local" ]]; then
  remote_codex_config="${REMOTE_HOME_PATH}/config.toml"
  [[ -f "$remote_codex_config" && ! -L "$remote_codex_config" ]] || \
    die "Remote Codex config is unavailable: $remote_codex_config"
  [[ "$(stat -c '%U:%G:%a' "$remote_codex_config")" == "ubuntu:ubuntu:600" ]] || \
    die 'Remote Codex config must be owned by ubuntu with mode 600'
  "$ROUTING" verify-video-bridge-disabled
fi

installer_args=(
  install "$version"
  --github-repo "$github_repo"
  --github-token-file "$github_token_file"
  --public-key "$public_key"
  --upstream "$upstream"
  --config "${CONFIG_DIR}/relay.json"
  --codex-config "${REMOTE_HOME_PATH}/config.toml"
  --catalog-path "${REMOTE_HOME_PATH}/opencodex-catalog.json"
  --codex-executable "${REMOTE_HOME_PATH}/packages/standalone/current/codex"
  --manage-app-server false
  --defer-codex-routing
)
if [[ "$action" == "install-local" ]]; then
  installer_args+=(
    --upstream-mode local_opencodex
    --credentials none
    --responses-websocket-mode http_fallback
    --catalog-owner remote_manager
  )
fi
for model in "${bounded_json_models[@]+"${bounded_json_models[@]}"}"; do
  installer_args+=(--bounded-json-model "$model")
done
"$RELAY_INSTALLER" "${installer_args[@]}"
verify_installed_relay_config

if [[ "$action" == "install-local" ]]; then
  routing_args=(enable-local-relay --allow-remote-interruption)
else
  routing_args=(enable-relay --allow-remote-interruption)
fi
[[ "$migrate_legacy" == true ]] && routing_args+=(--migrate-legacy)
"$ROUTING" "${routing_args[@]}"
"$ROUTING" status
printf 'remote_relay_installed=%s routing=%s github_repo=%s\n' "$version" \
  "$(if [[ "$action" == "install-local" ]]; then printf local-relay; else printf relay; fi)" \
  "$github_repo"

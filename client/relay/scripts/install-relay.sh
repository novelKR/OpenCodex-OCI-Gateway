#!/usr/bin/env bash
# Install a signed static relay release without storing data-plane credentials.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly INSTALL_ROOT="${HOME}/.local/lib/opencodex-relay/relay"
readonly DEFAULT_CONFIG="${XDG_CONFIG_HOME:-${HOME}/.config}/opencodex-relay/relay.json"
readonly DEFAULT_CODEX_CONFIG="${HOME}/.codex/config.toml"
readonly INTERACTIVE_PROFILE_BASENAME="opencodex-relay-interactive.config.toml"
readonly INTERACTIVE_PROFILE_MARKER="# opencodex-relay-managed-interactive-profile-v1"
readonly THIRD_PARTY_NOTICES_FILE="THIRD_PARTY_NOTICES.md"
readonly MACOS_MENU_BAR_BUNDLE="OpenCodexRelay.app"
readonly MACOS_MENU_BAR_ZIP="${MACOS_MENU_BAR_BUNDLE}.zip"
readonly MACOS_MENU_BAR_BUNDLE_ID="io.github.novelkr.opencodex-relay"
readonly MACOS_GUARD_HELPER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.helper"
readonly MACOS_GUARD_HELPER_NAME="OpenCodexRelayPrivilegedHelper"
readonly MACOS_GUARD_INSTALLER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.installer"
readonly MACOS_GUARD_INSTALLER_NAME="OpenCodexRelayHelperInstaller"
readonly TRUSTED_CODEX_BUNDLE_ID="com.openai.codex"
readonly TRUSTED_CODEX_TEAM_ID="2DC432GLL2"
readonly MACOS_MENU_BAR_LINK="${HOME}/Applications/${MACOS_MENU_BAR_BUNDLE}"
readonly MACOS_MENU_BAR_BINDING_DIR="${HOME}/Library/Application Support/OpenCodexRelay"
readonly MACOS_MENU_BAR_BINDING="${MACOS_MENU_BAR_BINDING_DIR}/routing-binding.json"

usage() {
  cat <<'USAGE'
Usage:
  install-relay.sh install VERSION (--release-base-url HTTPS_URL | --github-repo OWNER/REPO [--github-token-file PATH]) --public-key PEM --upstream URL [--upstream-mode external_gateway|local_opencodex] [--credentials keychain|file|none] [--responses-websocket-mode passthrough|http_fallback] [--bounded-json-model MODEL] [--catalog-owner relay|remote_manager] [--config PATH] [--codex-config PATH] [--catalog-path PATH] [--codex-executable PATH] [--manage-app-server true|false --app-server-home ABSOLUTE_PATH] [--migrate-legacy] [--defer-codex-routing]
  install-relay.sh uninstall [--config PATH] [--codex-config PATH] [--confirm-desktop-exited]

install verifies the signed release manifest and SHA-256 before atomically
selecting legacy raw helpers or a revision-4 darwin/arm64 signed MenuBar app
bundle. Revision 4 keeps relay helpers inside an ad-hoc-signed app and preserves
the current helper path through an internal bundle symlink.
Public GitHub Releases require no token. An optional current-user-owned mode-0600
token file may be supplied to avoid anonymous API rate limits; it never enters
relay.json, Codex config, or the service unit.
Secrets must already be registered in the macOS Keychain or Linux credentials.env.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_ed25519_public_key() {
  local path="$1"
  openssl pkey -pubin -in "$path" -text -noout 2>/dev/null | grep -Eq '^ED25519 Public-Key:' || \
    die 'release public key must be an Ed25519 public PEM'
}

sha256() {
  if command -v shasum >/dev/null; then shasum -a 256 "$1" | awk '{print $1}'; else sha256sum "$1" | awk '{print $1}'; fi
}

decode_base64() {
  local source="$1"
  local destination="$2"
  if base64 -D < "$source" > "$destination" 2>/dev/null; then
    return 0
  fi
  base64 --decode < "$source" > "$destination" 2>/dev/null || die 'release signature is not valid base64'
}

artifact_row() {
  local manifest="$1"
  local goos="$2"
  local goarch="$3"
  local file="$4"
  tr -d '\n' < "$manifest" | \
    sed -nE "s#.*\\{\\\"os\\\":\\\"${goos}\\\",\\\"arch\\\":\\\"${goarch}\\\",\\\"file\\\":\\\"${file}\\\",\\\"url\\\":\\\"([^\\\"]+)\\\",\\\"sha256\\\":\\\"([0-9a-f]{64})\\\"\\}.*#\\1|\\2#p"
}

component_artifact_row() {
  local manifest="$1"
  local goos="$2"
  local goarch="$3"
  local component="$4"
  jq -er \
    --arg os "$goos" --arg arch "$goarch" --arg component "$component" '
      [.artifacts[] | select(
        .os == $os and .arch == $arch and .component == $component and
        (.file | type == "string") and (.url | type == "string") and
        (.sha256 | type == "string")
      )]
      | select(length == 1)
      | .[0]
      | "\(.file)|\(.url)|\(.sha256)|\(.bundle_id // \"\")|\(.signing_mode // \"\")"
    ' "$manifest"
}

document_row() {
  local manifest="$1"
  local file="$2"
  jq -er --arg file "$file" '
    .documents
    | select(type == "array" and length == 1)
    | .[0]
    | select(.file == $file)
    | select((.url | type == "string") and (.sha256 | type == "string"))
    | "\(.url)|\(.sha256)"
  ' "$manifest"
}

manifest_string() {
  local manifest="$1"
  local field="$2"
  tr -d '\n' < "$manifest" | sed -nE "s#.*\"${field}\":\"([^\"]+)\".*#\1#p"
}

manifest_integer() {
  local manifest="$1"
  local field="$2"
  tr -d '\n' < "$manifest" | sed -nE "s#.*\"${field}\":([0-9]+).*#\1#p"
}

configured_upstream() {
  jq -er '.upstream_base_url | select(type == "string" and length > 0)' "$1"
}

configured_upstream_mode() {
  jq -er '
    (.upstream_mode // "")
    | select(type == "string")
    | if . == "" then "external_gateway" else . end
  ' "$1"
}

enable_macos_connection_probe() {
  local path="$1"
  local candidate
  [[ -f "$path" && ! -L "$path" ]] || die "relay configuration is unavailable for macOS connection probe setup: $path"
  candidate="$(mktemp "${path}.connection-probe.XXXXXX")"
  jq -e '
    .connection_probe = ((.connection_probe // {}) + {
      enabled: true
    })
  ' "$path" > "$candidate" || { rm -f -- "$candidate"; die 'unable to update macOS connection probe configuration'; }
  chmod 0600 "$candidate"
  mv -f -- "$candidate" "$path" || { rm -f -- "$candidate"; die 'unable to atomically enable macOS connection probing'; }
}

require_current_user_mode_600() {
  local path="$1"
  local ownership
  case "$(uname -s)" in
    Darwin) ownership="$(stat -f '%u:%Lp' "$path")" ;;
    Linux) ownership="$(stat -c '%u:%a' "$path")" ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  [[ "$ownership" == "$(id -u):600" ]] || \
    die "managed interactive profile must be owned by the current user with mode 0600: $path"
}

canonical_install_path() {
  local path="$1"
  local parent
  local base
  local resolved_parent
  [[ -n "$path" ]] || return 1
  if [[ "$path" != /* ]]; then
    path="$(pwd -P)/${path}"
  fi
  parent="$(dirname -- "$path")"
  base="$(basename -- "$path")"
  [[ "$base" != . && "$base" != / ]] || return 1
  mkdir -p -- "$parent" || return 1
  resolved_parent="$(cd -- "$parent" && pwd -P)" || return 1
  printf '%s/%s\n' "$resolved_parent" "$base"
}

preflight_menu_bar_binding() {
  local path="$1"
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    return 0
  fi
  [[ -f "$path" && ! -L "$path" ]] || \
    die "existing macOS MenuBar routing binding is unsafe: $path"
  require_current_user_mode_600 "$path"
}

write_menu_bar_binding() {
  local relay_config="$1"
  local codex_path="$2"
  local candidate
  preflight_menu_bar_binding "$MACOS_MENU_BAR_BINDING"
  if [[ -e "$MACOS_MENU_BAR_BINDING_DIR" || -L "$MACOS_MENU_BAR_BINDING_DIR" ]]; then
    [[ -d "$MACOS_MENU_BAR_BINDING_DIR" && ! -L "$MACOS_MENU_BAR_BINDING_DIR" ]] || \
      die "macOS MenuBar binding directory is unsafe: $MACOS_MENU_BAR_BINDING_DIR"
  else
    mkdir -p -- "$MACOS_MENU_BAR_BINDING_DIR" || die 'unable to create macOS MenuBar binding directory'
  fi
  chmod 0700 "$MACOS_MENU_BAR_BINDING_DIR" || die 'unable to protect macOS MenuBar binding directory'
  candidate="$(mktemp "${MACOS_MENU_BAR_BINDING}.XXXXXX")"
  jq -n --arg relay "$relay_config" --arg codex "$codex_path" \
    '{schema: 1, relay_config: $relay, codex_config: $codex}' > "$candidate" || {
      rm -f -- "$candidate"
      die 'unable to render macOS MenuBar routing binding'
    }
  chmod 0600 "$candidate"
  mv -f -- "$candidate" "$MACOS_MENU_BAR_BINDING" || {
    rm -f -- "$candidate"
    die 'unable to atomically publish macOS MenuBar routing binding'
  }
}

selected_managed_menu_bar_app() {
  local app=""
  local target=""
  if [[ -e "$MACOS_MENU_BAR_LINK" || -L "$MACOS_MENU_BAR_LINK" ]]; then
    [[ -L "$MACOS_MENU_BAR_LINK" ]] ||
      die 'existing macOS MenuBar app path is not a managed symbolic link'
    app="$(readlink "$MACOS_MENU_BAR_LINK")" ||
      die 'unable to inspect the existing macOS MenuBar app'
  elif [[ -L "${INSTALL_ROOT}/current" ]]; then
    target="$(readlink "${INSTALL_ROOT}/current")" ||
      die 'unable to inspect the selected relay release'
    if [[ "$target" == */"${MACOS_MENU_BAR_BUNDLE}"/Contents/Library/Helpers ]]; then
      app="${INSTALL_ROOT}/${target%/Contents/Library/Helpers}"
    else
      return 1
    fi
  else
    return 1
  fi
  [[ "$app" == "${INSTALL_ROOT}/"*"/${MACOS_MENU_BAR_BUNDLE}" &&
     -d "$app" && ! -L "$app" ]] ||
    die 'existing macOS MenuBar app target is unsafe'
  printf '%s\n' "$app"
}

require_manual_homebrew_guard_absent() {
  [[ "$(uname -s)" == Darwin ]] || return 0
  local app
  app="$(selected_managed_menu_bar_app)" || return 0
  local installer="${app}/Contents/Library/Helpers/${MACOS_GUARD_INSTALLER_NAME}"
  [[ -x "$installer" && ! -L "$installer" ]] || die 'Homebrew guard installer is unavailable'
  local status_json
  status_json="$("$installer" status --json 2>/dev/null)" || die 'unable to inspect the manual Homebrew guard'
  local state
  state="$(jq -er '.state' <<<"$status_json")" || die 'manual Homebrew guard status is invalid'
  case "$state" in
    install_required) return 0 ;;
    recovery_required)
      die "recover the manual Homebrew guard first: sudo -- '${installer}' recover"
      ;;
    ready|update_required)
      die "remove the manual Homebrew guard first: sudo -- '${installer}' uninstall"
      ;;
    *) die 'manual Homebrew guard status is unavailable; refusing uninstall' ;;
  esac
}

managed_interactive_profile_shape() {
  local path="$1"
  awk -v marker="$INTERACTIVE_PROFILE_MARKER" '
    NR == 1 { if ($0 != marker) exit 1; next }
    NR == 2 { if ($0 !~ /^openai_base_url = ".*"$/) exit 1; next }
    NR == 3 { if ($0 !~ /^model_catalog_json = ".*"$/) exit 1; next }
    { exit 1 }
    END { if (NR != 3) exit 1 }
  ' "$path"
}

preflight_interactive_profile() {
  local path="$1"
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    return 0
  fi
  [[ -f "$path" && ! -L "$path" ]] || \
    die "existing interactive profile is unsafe: $path"
  require_current_user_mode_600 "$path"
  managed_interactive_profile_shape "$path" || \
    die "existing $INTERACTIVE_PROFILE_BASENAME is not owned by opencodex-relay; move it aside or merge it manually"
}

relay_listeners() {
  local config="$1"
  jq -er '
    def go_empty_default($fallback):
      if . == null or . == "" then $fallback else . end;
    def numeric_loopback_listener:
      (try capture("^(127\\.0\\.0\\.1|\\[::1\\]):(?<port>[0-9]+)$") catch null) as $match
      | $match != null
      and (($match.port | tonumber) >= 1)
      and (($match.port | tonumber) <= 65535);
    ((.listen_address // "127.0.0.1:18180") | select(type == "string" and length > 0)) as $general
    | (.responses.scheduler.interactive_listen_address
        | go_empty_default(if ($general | startswith("[::1]:")) then "[::1]:18182" else "127.0.0.1:18182" end)) as $interactive
    | select(($interactive | numeric_loopback_listener) and $interactive != $general)
    | [$general, $interactive] | @tsv
  ' "$config"
}

listener_http_url() {
  local listener="$1"
  printf 'http://%s\n' "$listener"
}

# Revision 1/2 releases predate the routing controller and their embedded
# relayctl has no request/apply contract. Keep this narrowly scoped writer only
# so an already-reviewed legacy release remains recoverable; revisions 3/4 never
# call it. New macOS enrollment must use the controller-owned apply path.
legacy_write_interactive_profile() {
  local config="$1"
  local path="$2"
  local listeners
  local general_listener
  local interactive_listener
  local interactive_url
  local catalog
  local encoded_url
  local encoded_catalog
  local candidate
  preflight_interactive_profile "$path"
  listeners="$(relay_listeners "$config")" || die 'relay configuration has no supported distinct interactive loopback listener'
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  [[ -n "$general_listener" ]] || die 'relay configuration has no general listener'
  interactive_url="$(listener_http_url "$interactive_listener")/v1"
  catalog="$(jq -er '.catalog.path | select(type == "string" and length > 0)' "$config")" || \
    die 'relay configuration has no model catalog path for the interactive profile'
  encoded_url="$(jq -Rn --arg value "$interactive_url" '$value')"
  encoded_catalog="$(jq -Rn --arg value "$catalog" '$value')"
  mkdir -p -- "$(dirname -- "$path")"
  candidate="$(mktemp "${path}.XXXXXX")"
  {
    printf '%s\n' "$INTERACTIVE_PROFILE_MARKER"
    printf 'openai_base_url = %s\n' "$encoded_url"
    printf 'model_catalog_json = %s\n' "$encoded_catalog"
  } > "$candidate"
  chmod 0600 "$candidate"
  managed_interactive_profile_shape "$candidate" || {
    rm -f -- "$candidate"
    die 'generated legacy interactive profile failed its two-key safety check'
  }
  if [[ -f "$path" ]] && cmp -s "$candidate" "$path"; then
    rm -f -- "$candidate"
    return 0
  fi
  mv -f -- "$candidate" "$path" || {
    rm -f -- "$candidate"
    die 'unable to atomically publish legacy interactive profile'
  }
}

active_local_runtime_is_acknowledged() {
  local config="$1"
  local relayctl="$2"
  local codex_path="$3"
  local status

  # A canonical external relay may retain an explicitly enrolled Local profile.
  # Accept its derived runtime only after the resident relayctl status proves
  # that this exact bound state is live, acknowledged, and still fail-closed to
  # the fixed 10100 identity/catalog contract. A missing helper or any
  # unacknowledged state deliberately falls back to the canonical comparison,
  # which rejects a surprise Local listener rather than inferring a fallback.
  [[ -n "$relayctl" && -x "$relayctl" && -n "$codex_path" ]] || return 1
  status="$("$relayctl" mode status --config "$config" --codex-config "$codex_path" --json 2>/dev/null)" || return 1
  jq -e --slurpfile cfg "$config" '
    ($cfg[0]) as $c
    | ($c.local_opencodex // null) as $local
    | (($c.upstream_mode // "external_gateway") == "external_gateway")
    and (($c.catalog.owner // "relay") == "relay")
    and ($local | type == "object")
    and ($local.upstream_base_url == "http://127.0.0.1:10100/v1" or $local.upstream_base_url == "http://[::1]:10100/v1")
    and ($local.catalog_path | type == "string" and startswith("/") and . != $c.catalog.path)
    and .schema_version == 2
    and .desired_backend == "local_opencodex"
    and .applied_backend == "local_opencodex"
    and .phase == "relay_active"
    and .relay_admission == "allow"
    and .catalog_refresh == "run"
    and .relay_running == true
    and .connection.local_relay == "healthy"
    and .connection.routing_sync == "acknowledged"
    and .connection.local_opencodex == "ready"
    and .connection.catalog == "running"
  ' <<< "$status" >/dev/null
}

health_matches_listener_lane() {
  local health="$1"
  local config="$2"
  local expected_lane="$3"
  local expected_general="$4"
  local expected_interactive="$5"
  local runtime_profile="$6"
  jq -e --slurpfile cfg "$config" \
    --arg lane "$expected_lane" \
    --arg general "$expected_general" \
    --arg interactive "$expected_interactive" \
    --arg runtime_profile "$runtime_profile" '
      def nonnegative_integer:
        type == "number" and floor == . and . >= 0;
      def go_zero_default($fallback):
        if . == null or . == 0 then $fallback else . end;
      def go_empty_default($fallback):
        if . == null or . == "" then $fallback else . end;
      ($cfg[0]) as $c
      | ($c.local_opencodex // null) as $local
      | ($c.responses.scheduler // {}) as $s
      | .ok == true
      and .listener_lane == $lane
      and .general_listener == $general
      and .interactive_listener == $interactive
      and (
        if $runtime_profile == "local_opencodex" then
          .upstream_mode == "local_opencodex"
          and .upstream_base_url == $local.upstream_base_url
          and .catalog_owner == "relay"
        elif $runtime_profile == "canonical" then
          .upstream_mode == ($c.upstream_mode | go_empty_default("external_gateway"))
          and .upstream_base_url == $c.upstream_base_url
          and .catalog_owner == ($c.catalog.owner | go_empty_default("relay"))
        else
          false
        end
      )
      and .responses_websocket_mode == ($c.responses.websocket_mode | go_empty_default("passthrough"))
      and ((.responses_models // []) | sort) == ((($c.responses.model_modes // {}) | keys) | sort)
      and .responses_normalizer == (((($c.responses.model_modes // {}) | length) > 0))
      and (.active_requests | nonnegative_integer)
      and (.active_classifications | nonnegative_integer)
      and (.pending_requests | nonnegative_integer)
      and (.pending_encoded_bytes | nonnegative_integer)
      and (.active_general_upstream | nonnegative_integer)
      and (.active_interactive_upstream | nonnegative_integer)
      and (.active_transforms | nonnegative_integer)
      and (.active_deliveries | nonnegative_integer)
      and (.capacity_rejections | nonnegative_integer)
      and (.scheduler_limits.max_classifications == ($s.max_classifications | go_zero_default(8)))
      and (.scheduler_limits.max_pending_requests == ($s.max_pending_requests | go_zero_default(24)))
      and (.scheduler_limits.max_pending_encoded_bytes == ($s.max_pending_encoded_bytes | go_zero_default(536870912)))
      and (.scheduler_limits.queue_timeout_ms == ($s.queue_timeout_ms | go_zero_default(60000)))
      and (.scheduler_limits.max_general_upstream == ($s.max_general_upstream | go_zero_default(4)))
      and (.scheduler_limits.interactive_reserved_upstream == ($s.interactive_reserved_upstream | go_zero_default(1)))
      and (.scheduler_limits.max_concurrent_transforms == ($s.max_concurrent_transforms | go_zero_default(2)))
      and (.scheduler_limits.max_open_deliveries == ($s.max_open_deliveries | go_zero_default(16)))
    ' <<< "$health" >/dev/null
}

verify_dual_listener_health_once() {
  local config="$1"
  local relayctl="${2:-}"
  local codex_path="${3:-}"
  local listeners
  local general_listener
  local interactive_listener
  local general_health
  local interactive_health
  listeners="$(relay_listeners "$config")" || return 1
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  general_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "$(listener_http_url "$general_listener")/__relay/healthz")" || return 1
  interactive_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "$(listener_http_url "$interactive_listener")/__relay/healthz")" || return 1
  if health_matches_listener_lane "$general_health" "$config" general "$general_listener" "$interactive_listener" canonical && \
     health_matches_listener_lane "$interactive_health" "$config" interactive "$general_listener" "$interactive_listener" canonical; then
    return 0
  fi
  # Only a canonical mismatch may enter the Local exception. This avoids an
  # otherwise unnecessary status/preflight call for the normal External and
  # legacy-static profiles, while a Local health shape is accepted only after
  # the independent relayctl acknowledgement above proves it is the selected
  # derived runtime.
  active_local_runtime_is_acknowledged "$config" "$relayctl" "$codex_path" || return 1
  health_matches_listener_lane "$general_health" "$config" general "$general_listener" "$interactive_listener" local_opencodex && \
    health_matches_listener_lane "$interactive_health" "$config" interactive "$general_listener" "$interactive_listener" local_opencodex
}

wait_for_dual_listener_health() {
  local config="$1"
  local relayctl="${2:-}"
  local codex_path="${3:-}"
  local attempt
  for attempt in {1..20}; do
    if verify_dual_listener_health_once "$config" "$relayctl" "$codex_path"; then
      printf 'relay_dual_listener_health=ready attempts=%s\n' "$attempt"
      return 0
    fi
    sleep 1
  done
  die 'relay general and interactive loopback listeners did not reach the reviewed health contract'
}

verify_requested_routing_state() {
  local relayctl="$1"
  local relay_config="$2"
  local codex_path="$3"
  local status
  local phase
  local admission
  local catalog
  local running
  local routing_sync
  status="$("$relayctl" mode status --config "$relay_config" --codex-config "$codex_path" --json)" || \
    die 'unable to read routing status after service activation'
  phase="$(jq -er '.phase | strings' <<<"$status")" || die 'routing status phase is invalid after service activation'
  admission="$(jq -er '.relay_admission | strings' <<<"$status")" || die 'routing status admission is invalid after service activation'
  catalog="$(jq -er '.catalog_refresh | strings' <<<"$status")" || die 'routing status catalog state is invalid after service activation'
  running="$(jq -er '.relay_running | booleans | tostring' <<<"$status")" || die 'routing status relay state is invalid after service activation'
  routing_sync="$(jq -er '.connection.routing_sync | strings' <<<"$status")" || die 'routing status synchronization is invalid after service activation'
  [[ "$running" == true ]] || die 'resident relay did not become healthy after service activation'
  [[ "$routing_sync" == acknowledged ]] || die 'resident relay did not acknowledge the requested routing state'
  case "$phase:$admission:$catalog" in
    relay_active:allow:run)
      printf 'codex_routing=relay_active\n'
      ;;
    relay_pending_restart:deny:pause)
      printf 'codex_routing=relay_pending_restart\n'
      ;;
    *)
      die "routing request was not acknowledged safely after service activation: ${phase}/${admission}/${catalog}"
      ;;
  esac
}

seed_deferred_routing_state() {
  local relayctl="$1"
  local relay_config="$2"
  local codex_path="$3"
  local status
  local applied
  status="$("$relayctl" mode status --config "$relay_config" --codex-config "$codex_path" --json)" || \
    die 'unable to inspect routing before deferred service activation'
  applied="$(jq -er '.applied_backend | select(. == "external" or . == "local_opencodex" or . == "none")' <<<"$status")" || \
    die 'deferred routing is ambiguous; resolve it with relayctl mode recover before installing the service'
  # A same-mode request persists the exact canonical binding without changing
  # any Codex TOML/profile artifact. This prevents a new native installation
  # from being mistaken for legacy relay_active when the resident watcher
  # first starts.
  if [[ "$applied" == none ]]; then
    applied=native
  fi
  "$relayctl" mode request "$applied" --config "$relay_config" --codex-config "$codex_path" >/dev/null
}

request_install_routing_state() {
  local relayctl="$1"
  local relay_config="$2"
  local codex_path="$3"
  local status
  local applied
  status="$("$relayctl" mode status --config "$relay_config" --codex-config "$codex_path" --json)" || \
    die 'unable to inspect routing before service activation'
  applied="$(jq -er '.applied_backend | select(. == "external" or . == "local_opencodex" or . == "none")' <<<"$status")" || \
    die 'routing is ambiguous; resolve it with relayctl mode recover before installing the service'
  # A fresh native install intentionally defaults to the configured relay
  # topology.  Canonical macOS/external installs therefore choose External,
  # while the preserved Linux static local_opencodex/remote-manager contract
  # requests Local rather than trying an unsupported External profile.  An
  # existing Local selection is likewise preserved exactly: changing it to
  # External during an update would violate the explicit no-fallback contract.
  if [[ "$applied" == none ]]; then
    case "$(configured_upstream_mode "$relay_config")" in
      external_gateway) applied=external ;;
      local_opencodex) applied=local_opencodex ;;
      *) die 'relay config has an unsupported upstream mode for routing initialization' ;;
    esac
  fi
  "$relayctl" mode request "$applied" --config "$relay_config" --codex-config "$codex_path" >/dev/null
}

verify_deferred_routing_state() {
  local relayctl="$1"
  local relay_config="$2"
  local codex_path="$3"
  local status
  local phase
  local admission
  local catalog
  local running
  local routing_sync
  status="$("$relayctl" mode status --config "$relay_config" --codex-config "$codex_path" --json)" || \
    die 'unable to read deferred routing status after service activation'
  phase="$(jq -er '.phase | strings' <<<"$status")" || die 'deferred routing status phase is invalid'
  admission="$(jq -er '.relay_admission | strings' <<<"$status")" || die 'deferred routing status admission is invalid'
  catalog="$(jq -er '.catalog_refresh | strings' <<<"$status")" || die 'deferred routing status catalog state is invalid'
  running="$(jq -er '.relay_running | booleans | tostring' <<<"$status")" || die 'deferred routing relay state is invalid'
  routing_sync="$(jq -er '.connection.routing_sync | strings' <<<"$status")" || die 'deferred routing synchronization is invalid'
  [[ "$running" == true ]] || die 'resident relay did not become healthy after deferred service activation'
  [[ "$routing_sync" == acknowledged ]] || die 'resident relay did not acknowledge the deferred routing state'
  case "$phase:$admission:$catalog" in
    relay_active:allow:run|native_active:deny:pause)
      printf 'codex_routing=%s\n' "$phase"
      ;;
    *)
      die "deferred routing was not acknowledged safely after service activation: ${phase}/${admission}/${catalog}"
      ;;
  esac
}

service_snapshot_value() {
  local key="$1"
  sed -nE "s/^${key}=([A-Za-z0-9_-]+)$/\\1/p" "${transaction_dir}/service/state"
}

managed_active_relay_has_matching_health() {
  [[ -n "${previous_current_target:-}" ]] || return 1
  [[ "$(service_snapshot_value artifact_present)" == true ]] || return 1
  [[ "$(service_snapshot_value active)" == true ]] || return 1
  verify_dual_listener_health_once "$config_path" "${relayctl_path:-}" "${codex_config:-}"
}

require_interactive_listener_available() {
  local listeners
  local general_listener
  local interactive_listener
  local port
  if managed_active_relay_has_matching_health; then
    printf 'interactive_listener_preflight=managed-active\n'
    return 0
  fi
  listeners="$(relay_listeners "$config_path")" || \
    die 'relay configuration has no supported distinct interactive loopback listener'
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  port="${interactive_listener##*:}"
  case "$(uname -s)" in
    Darwin)
      command -v lsof >/dev/null || die 'lsof is required for interactive listener availability checks on macOS'
      if lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | grep -q .; then
        die "interactive relay listener port is already occupied: $interactive_listener"
      fi
      ;;
    Linux)
      command -v ss >/dev/null || die 'ss is required for interactive listener availability checks on Linux'
      if ss -lntH "sport = :${port}" 2>/dev/null | grep -q .; then
        die "interactive relay listener port is already occupied: $interactive_listener"
      fi
      ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  printf 'interactive_listener_preflight=free listener=%s\n' "$interactive_listener"
}

normalize_target() {
  case "$(uname -s):$(uname -m)" in
    Darwin:arm64) printf 'darwin arm64\n' ;;
    Linux:x86_64|Linux:amd64) printf 'linux amd64\n' ;;
    Linux:aarch64|Linux:arm64) printf 'linux arm64\n' ;;
    *) die "unsupported platform: $(uname -s)/$(uname -m)" ;;
  esac
}

verify_and_extract_macos_bundle() {
  local archive="$1"
  local expected_hash="$2"
  local expected_bundle_id="$3"
  local expected_signing_mode="$4"
  local destination="$5"
  local compatibility_revision="$6"
  [[ "$compatibility_revision" == 4 && "$expected_signing_mode" == adhoc ]] || \
    die 'macOS bundle signing contract is unsupported'
  local entry
  local entries
  local app
  local actual_bundle_id
  local trusted_codex_bundle_id
  local trusted_codex_team_id
  local guard_helper
  local guard_installer
  local helper_cdhash
  local helper_requirement

  [[ "$(sha256 "$archive")" == "$expected_hash" ]] || die 'macOS bundle SHA-256 does not match manifest'
  command -v ditto >/dev/null || die 'ditto is required for the macOS app bundle'
  command -v unzip >/dev/null || die 'unzip is required for macOS app bundle shape validation'
  command -v codesign >/dev/null || die 'codesign is required for macOS app bundle validation'
  entries="$(unzip -Z1 "$archive")" || die 'macOS bundle archive cannot be listed'
  [[ -n "$entries" ]] || die 'macOS bundle archive is empty'
  while IFS= read -r entry; do
    [[ "$entry" != /* && "$entry" != *'..'* && "$entry" != *'//'* ]] || die 'macOS bundle archive contains an unsafe path'
    [[ "$entry" == "$MACOS_MENU_BAR_BUNDLE" || "$entry" == "${MACOS_MENU_BAR_BUNDLE}/"* ]] || \
      die 'macOS bundle archive must contain exactly one app bundle root'
  done <<< "$entries"
  ditto -x -k "$archive" "$destination" || die 'unable to extract macOS app bundle'
  [[ "$(find "$destination" -mindepth 1 -maxdepth 1 -print | wc -l | tr -d ' ')" == 1 ]] || \
    die 'macOS bundle archive contains unexpected top-level paths'
  app="${destination}/${MACOS_MENU_BAR_BUNDLE}"
  [[ -d "$app" && ! -L "$app" ]] || die 'macOS bundle root is unavailable or unsafe'
  if find "$app" -type l -print -quit | grep -q .; then
    die 'macOS bundle must not contain symbolic links'
  fi
  actual_bundle_id="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" || \
    die 'macOS bundle Info.plist is invalid'
  [[ "$actual_bundle_id" == "$expected_bundle_id" && "$actual_bundle_id" == "$MACOS_MENU_BAR_BUNDLE_ID" ]] || \
    die 'macOS bundle identifier does not match the signed manifest'
  icon_file="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIconFile' "${app}/Contents/Info.plist" 2>/dev/null)" ||
    die 'macOS bundle icon metadata is unavailable'
  [[ "$icon_file" == AppIcon.icns && -f "${app}/Contents/Resources/AppIcon.icns" &&
     ! -L "${app}/Contents/Resources/AppIcon.icns" ]] || die 'macOS bundle app icon is invalid'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :LSUIElement' "${app}/Contents/Info.plist" 2>/dev/null)" == false ]] ||
    die 'macOS bundle must remain visible in the Dock'
  trusted_codex_bundle_id="$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexTrustedCodexBundleIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" || \
    die 'macOS bundle Codex Desktop identifier is unavailable'
  trusted_codex_team_id="$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexTrustedCodexTeamIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" || \
    die 'macOS bundle Codex Desktop Team ID is unavailable'
  [[ "$trusted_codex_bundle_id" == "$TRUSTED_CODEX_BUNDLE_ID" && \
     "$trusted_codex_team_id" == "$TRUSTED_CODEX_TEAM_ID" ]] || \
    die 'macOS bundle Codex Desktop trust identity is not reviewed'

  [[ -x "${app}/Contents/Library/Helpers/opencodex-relay" && \
     ! -L "${app}/Contents/Library/Helpers/opencodex-relay" && \
     -x "${app}/Contents/Library/Helpers/opencodex-relayctl" && \
     ! -L "${app}/Contents/Library/Helpers/opencodex-relayctl" && \
     -x "${app}/Contents/MacOS/OpenCodexRelay" && \
     ! -L "${app}/Contents/MacOS/OpenCodexRelay" ]] || \
    die 'macOS bundle does not contain the required regular signed executables'
  guard_helper="${app}/Contents/Library/HelperTools/${MACOS_GUARD_HELPER_NAME}"
  guard_installer="${app}/Contents/Library/Helpers/${MACOS_GUARD_INSTALLER_NAME}"
  [[ -x "$guard_helper" && ! -L "$guard_helper" && -x "$guard_installer" && ! -L "$guard_installer" ]] ||
    die 'revision 4 Homebrew guard manual-installer bundle shape is invalid'
  [[ ! -e "${app}/Contents/Library/LaunchDaemons" && ! -L "${app}/Contents/Library/LaunchDaemons" ]] ||
    die 'revision 4 bundle must not embed an SMAppService LaunchDaemon'
  helper_cdhash="$(codesign -dvvv "$guard_helper" 2>&1 | sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p')"
  [[ "$helper_cdhash" =~ ^[0-9a-f]{40,128}$ ]] || die 'revision 4 helper CDHash is unavailable'
  helper_requirement="cdhash H\"${helper_cdhash}\""
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardBackend' "${app}/Contents/Info.plist" 2>/dev/null)" == manual_admin &&
     "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardMachService' "${app}/Contents/Info.plist" 2>/dev/null)" == "io.github.novelkr.opencodex-relay.homebrew-guard" &&
     "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardInstallerExecutable' "${app}/Contents/Info.plist" 2>/dev/null)" == "$MACOS_GUARD_INSTALLER_NAME" &&
     "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardHelperRequirement' "${app}/Contents/Info.plist" 2>/dev/null)" == "$helper_requirement" ]] ||
    die 'revision 4 Homebrew guard CDHash metadata is invalid'
  codesign --verify --deep --strict --verbose=2 "$app" || die 'macOS bundle codesign verification failed'
  local component
  local details
  for component in \
    "${app}/Contents/Library/Helpers/opencodex-relay" \
    "${app}/Contents/Library/Helpers/opencodex-relayctl" \
    "$guard_helper" \
    "$guard_installer" \
    "${app}/Contents/MacOS/OpenCodexRelay" \
    "$app"; do
    details="$(codesign -dvvv --verbose=4 "$component" 2>&1)"
    grep -Fx 'Signature=adhoc' <<<"$details" >/dev/null || die "component is not ad-hoc signed: $component"
    grep -Fx 'TeamIdentifier=not set' <<<"$details" >/dev/null || die "component unexpectedly has an Apple Team ID: $component"
    grep -E '^CodeDirectory .*flags=.*\(.*runtime.*\)' <<<"$details" >/dev/null || \
      die "component does not use the Hardened Runtime: $component"
  done
  codesign -dv --verbose=4 "$guard_helper" 2>&1 | grep -Fx "Identifier=${MACOS_GUARD_HELPER_IDENTIFIER}" >/dev/null ||
    die 'revision 4 Homebrew guard helper identifier is invalid'
  codesign -dv --verbose=4 "$guard_installer" 2>&1 | grep -Fx "Identifier=${MACOS_GUARD_INSTALLER_IDENTIFIER}" >/dev/null ||
    die 'revision 4 Homebrew guard installer identifier is invalid'
}

replace_current_link() {
  local candidate="$1"
  local current="$2"
  [[ -L "$candidate" ]] || {
    printf '%s\n' 'ERROR: current link candidate is not a symbolic link' >&2
    return 1
  }
  if [[ -e "$current" || -L "$current" ]]; then
    [[ -L "$current" ]] || {
      printf 'ERROR: existing current target is not a symbolic link: %s\n' "$current" >&2
      return 1
    }
  fi
  case "$(uname -s)" in
    Darwin)
      mv -fh "$candidate" "$current" || {
        rm -f -- "$candidate"
        return 1
      }
      ;;
    Linux)
      mv -fT "$candidate" "$current" || {
        rm -f -- "$candidate"
        return 1
      }
      ;;
    *)
      printf 'ERROR: unsupported operating system: %s\n' "$(uname -s)" >&2
      return 1
      ;;
  esac
}

restore_current_link() {
  local current="$1"
  local previous_target="$2"
  local candidate
  if [[ -z "$previous_target" ]]; then
    if [[ ! -e "$current" && ! -L "$current" ]]; then
      return 0
    fi
    [[ -L "$current" ]] || return 1
    rm -f -- "$current"
    return 0
  fi
  [[ "$previous_target" != /* && "$previous_target" != *".."* ]] || return 1
  candidate="${INSTALL_ROOT}/.current.rollback.$$"
  ln -s "$previous_target" "$candidate" || return 1
  replace_current_link "$candidate" "$current"
}

snapshot_managed_menu_link() {
  local path="$1"
  local snapshot_prefix="$2"
  local target
  if [[ ! -e "$path" && ! -L "$path" ]]; then
    printf 'present=false\n' > "${snapshot_prefix}.state"
    chmod 0600 "${snapshot_prefix}.state"
    return 0
  fi
  [[ -L "$path" ]] || die "existing macOS MenuBar app path is not a managed symbolic link: $path"
  target="$(readlink "$path")" || die "unable to read existing macOS MenuBar app link: $path"
  [[ "$target" == "${INSTALL_ROOT}/"*"/${MACOS_MENU_BAR_BUNDLE}" ]] || \
    die "existing macOS MenuBar app link is not managed by opencodex-relay: $path"
  printf 'present=true\ntarget=%s\n' "$target" > "${snapshot_prefix}.state"
  chmod 0600 "${snapshot_prefix}.state"
}

replace_managed_menu_link() {
  local target="$1"
  local path="$2"
  local candidate
  [[ "$target" == "${INSTALL_ROOT}/"*"/${MACOS_MENU_BAR_BUNDLE}" ]] || return 1
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -L "$path" ]] || return 1
  fi
  mkdir -p -- "$(dirname -- "$path")" || return 1
  candidate="$(dirname -- "$path")/.${MACOS_MENU_BAR_BUNDLE}.candidate.$$"
  ln -s "$target" "$candidate" || return 1
  mv -f -- "$candidate" "$path" || { rm -f -- "$candidate"; return 1; }
}

restore_managed_menu_link() {
  local path="$1"
  local snapshot_prefix="$2"
  local state
  local target
  [[ -f "${snapshot_prefix}.state" && ! -L "${snapshot_prefix}.state" ]] || return 1
  state="$(sed -nE 's/^present=(true|false)$/\1/p' "${snapshot_prefix}.state")"
  case "$state" in
    true)
      target="$(sed -nE 's/^target=(.*)$/\1/p' "${snapshot_prefix}.state")"
      [[ "$target" == "${INSTALL_ROOT}/"*"/${MACOS_MENU_BAR_BUNDLE}" ]] || return 1
      replace_managed_menu_link "$target" "$path"
      ;;
    false)
      if [[ -e "$path" || -L "$path" ]]; then
        [[ -L "$path" ]] || return 1
        rm -f -- "$path"
      fi
      ;;
    *) return 1 ;;
  esac
}

snapshot_regular_file() {
  local path="$1"
  local snapshot_prefix="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "existing file is unsafe for install rollback: $path"
    cp -p -- "$path" "${snapshot_prefix}.file"
    printf 'present=true\n' > "${snapshot_prefix}.state"
  else
    printf 'present=false\n' > "${snapshot_prefix}.state"
  fi
  chmod 0600 "${snapshot_prefix}.state"
}

snapshot_owner_only_control_file() {
  local path="$1"
  local snapshot_prefix="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "existing routing control file is unsafe for install rollback: $path"
    require_current_user_mode_600 "$path"
  fi
  snapshot_regular_file "$path" "$snapshot_prefix"
}

restore_regular_file_snapshot() {
  local path="$1"
  local snapshot_prefix="$2"
  local state
  local candidate
  [[ -f "${snapshot_prefix}.state" && ! -L "${snapshot_prefix}.state" ]] || return 1
  state="$(sed -nE 's/^present=(true|false)$/\1/p' "${snapshot_prefix}.state")"
  case "$state" in
    true)
      [[ -f "${snapshot_prefix}.file" && ! -L "${snapshot_prefix}.file" ]] || return 1
      mkdir -p -- "$(dirname -- "$path")" || return 1
      candidate="$(mktemp "${path}.rollback.XXXXXX")" || return 1
      if ! cp -p -- "${snapshot_prefix}.file" "$candidate"; then
        rm -f -- "$candidate"
        return 1
      fi
      mv -f -- "$candidate" "$path"
      ;;
    false)
      if [[ -e "$path" || -L "$path" ]]; then
        [[ -f "$path" && ! -L "$path" ]] || return 1
        rm -f -- "$path"
      fi
      ;;
    *) return 1 ;;
  esac
}

rollback_install_transaction() {
  local current="$1"
  local previous_target="$2"
  local relay_config="$3"
  local relay_snapshot="$4"
  local codex_config="$5"
  local codex_snapshot="$6"
  local interactive_profile="$7"
  local interactive_profile_snapshot="$8"
  local routing_state="$9"
  local routing_state_snapshot="${10}"
  local routing_initialized="${11}"
  local routing_initialized_snapshot="${12}"
  local routing_journal="${13}"
  local routing_journal_snapshot="${14}"
  local local_enrollment="${15}"
  local local_enrollment_snapshot="${16}"
  local local_catalog="${17}"
  local local_catalog_snapshot="${18}"
  local local_catalog_pending="${19}"
  local local_catalog_pending_snapshot="${20}"
  local service_snapshot="${21}"
  local menu_bar_link="${22:-}"
  local menu_bar_link_snapshot="${23:-}"
  local menu_bar_binding="${24:-}"
  local menu_bar_binding_snapshot="${25:-}"
  local rollback_failed=false

	# Stop the candidate before restoring an admitting state/configuration. If
	# the failure happened after service activation, restoring relay_active
	# first would let that candidate briefly forward through a mismatched
	# profile. restore-snapshot below reactivates only the captured service.
	if ! "${SCRIPT_DIR}/install-service.sh" stop; then
	  printf 'CRITICAL: unable to stop the candidate relay service before rollback.\n' >&2
	  rollback_failed=true
	fi
  if ! restore_current_link "$current" "$previous_target"; then
    printf 'CRITICAL: unable to restore the previous relay target.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$relay_config" "$relay_snapshot"; then
    printf 'CRITICAL: unable to restore the previous relay configuration.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$codex_config" "$codex_snapshot"; then
    printf 'CRITICAL: unable to restore the previous Codex routing configuration.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$interactive_profile" "$interactive_profile_snapshot"; then
    printf 'CRITICAL: unable to restore the previous Codex interactive profile.\n' >&2
    rollback_failed=true
  fi
  # Restore the full durable routing tuple before resurrecting the prior
  # service. The initialized sentinel distinguishes an untouched legacy relay
  # from a deleted state file, so leaving any candidate member behind can
  # permanently park (or incorrectly admit) the restored service.
  if ! restore_regular_file_snapshot "$routing_state" "$routing_state_snapshot"; then
    printf 'CRITICAL: unable to restore the previous routing state.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$routing_initialized" "$routing_initialized_snapshot"; then
    printf 'CRITICAL: unable to restore the previous routing initialization marker.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$routing_journal" "$routing_journal_snapshot"; then
    printf 'CRITICAL: unable to restore the previous routing transaction journal.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$local_enrollment" "$local_enrollment_snapshot"; then
    printf 'CRITICAL: unable to restore the previous Local OpenCodex enrollment receipt.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$local_catalog" "$local_catalog_snapshot"; then
    printf 'CRITICAL: unable to restore the previous Local OpenCodex catalog.\n' >&2
    rollback_failed=true
  fi
  if ! restore_regular_file_snapshot "$local_catalog_pending" "$local_catalog_pending_snapshot"; then
    printf 'CRITICAL: unable to restore the previous Local OpenCodex catalog marker.\n' >&2
    rollback_failed=true
  fi
  if [[ -n "$menu_bar_link" ]] && ! restore_managed_menu_link "$menu_bar_link" "$menu_bar_link_snapshot"; then
    printf 'CRITICAL: unable to restore the previous macOS MenuBar app link.\n' >&2
    rollback_failed=true
  fi
  if [[ -n "$menu_bar_binding" ]] && ! restore_regular_file_snapshot "$menu_bar_binding" "$menu_bar_binding_snapshot"; then
    printf 'CRITICAL: unable to restore the previous macOS MenuBar routing binding.\n' >&2
    rollback_failed=true
  fi
  # Restore every file the service consumes before restoring an active service.
  # Otherwise the old process could briefly restart against the failed candidate
  # configuration while the remaining snapshot is still being applied.
  if ! "${SCRIPT_DIR}/install-service.sh" restore-snapshot --directory "$service_snapshot"; then
    printf 'CRITICAL: unable to restore the previous relay service artifact or manager state.\n' >&2
    rollback_failed=true
  fi
  [[ "$rollback_failed" == false ]]
}

remove_new_install_artifact() {
  local selected=""
  local expected_target="${version}/${goos}-${goarch}"
  local expected_current_target="$expected_target"
  if [[ "${macos_bundle_mode:-false}" == true ]]; then
    expected_current_target+="/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers"
  fi
  [[ "${install_dir_created:-false}" == true ]] || return 0
  [[ -n "${install_dir:-}" && "$install_dir" == "${INSTALL_ROOT}/${expected_target}" ]] || return 1
  if [[ -L "${current_path:-}" ]]; then
    selected="$(readlink "$current_path")" || return 1
  elif [[ -e "${current_path:-}" ]]; then
    return 1
  fi
  # Never delete a candidate which is still selected after an incomplete
  # rollback. Existing signed releases are preserved because only the directory
  # moved into place by this invocation sets install_dir_created=true.
  [[ "$selected" != "$expected_current_target" ]] || return 1
  [[ -d "$install_dir" && ! -L "$install_dir" ]] || return 1
  rm -rf -- "$install_dir" || return 1
  if [[ "${version_dir_created:-false}" == true ]]; then
    rmdir "${INSTALL_ROOT}/${version}" 2>/dev/null || true
  fi
}

finish_install() {
  local status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  if ((status != 0)) && [[ "${install_transaction_active:-false}" == true ]]; then
    printf 'ERROR: relay installation failed after the rollback snapshot; restoring the previous release, service, and routing state.\n' >&2
    if ! rollback_install_transaction \
      "$current_path" "$previous_current_target" \
      "$config_path" "${transaction_dir}/relay-config" \
      "$codex_config" "${transaction_dir}/codex-config" \
      "$interactive_profile" "${transaction_dir}/interactive-profile" \
      "$routing_state_path" "${transaction_dir}/routing-state" \
      "$routing_initialized_path" "${transaction_dir}/routing-initialized" \
      "$routing_journal_path" "${transaction_dir}/routing-journal" \
      "$local_enrollment_path" "${transaction_dir}/local-enrollment" \
      "$local_catalog_path" "${transaction_dir}/local-catalog" \
      "$local_catalog_pending_path" "${transaction_dir}/local-catalog-pending" \
      "${transaction_dir}/service" \
      "${menu_bar_link:-}" "${transaction_dir}/menu-bar-link" \
      "${menu_bar_binding:-}" "${transaction_dir}/menu-bar-binding"; then
      status=70
    fi
  fi
  if ((status != 0)) && [[ "${install_dir_created:-false}" == true ]]; then
    if ! remove_new_install_artifact; then
      printf 'CRITICAL: unable to remove the unselected release artifact created by the failed install.\n' >&2
      status=70
    fi
  fi
  rm -rf -- "${tmp:-}" "${staging_dir:-}"
  exit "$status"
}

require_version() {
  [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die 'VERSION must be explicit semver'
}

require_private_token_file() {
  local path="$1"
  local ownership
  [[ -f "$path" && ! -L "$path" ]] || die "GitHub token file is unavailable or unsafe: $path"
  case "$(uname -s)" in
    Darwin) ownership="$(stat -f '%u:%Lp' "$path")" ;;
    Linux) ownership="$(stat -c '%u:%a' "$path")" ;;
    *) die "unsupported operating system: $(uname -s)" ;;
  esac
  [[ "$ownership" == "$(id -u):600" ]] || \
    die 'GitHub token file must be owned by the current user with mode 0600'
}

github_repo_valid() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]]
}

read_github_token() {
  github_token="$(<"$github_token_file")"
  [[ -n "$github_token" && "$github_token" != *[[:space:]]* && \
     "$github_token" =~ ^[A-Za-z0-9._=-]+$ ]] || \
    die 'GitHub token file must contain one non-empty token without whitespace'
}

write_github_curl_config() {
  local path="$1"
  umask 077
  : > "$path"
  chmod 0600 "$path"
  if [[ -n "$github_token" ]]; then
    printf 'header = "Authorization: Bearer %s"\n' "$github_token" >> "$path"
  fi
  printf '%s\n' 'header = "X-GitHub-Api-Version: 2022-11-28"' >> "$path"
}

github_release_state() {
  local tag_name
  local is_draft
  local is_immutable
  curl --config "$github_curl_config" --fail --silent --show-error \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${github_repo}/releases/tags/${version}" \
    -o "$github_release_json" || \
    die "unable to read GitHub release ${github_repo}@${version}"
  tag_name="$(jq -er '.tag_name | strings' "$github_release_json")" || \
    die 'GitHub release metadata has no valid tag name'
  is_draft="$(jq -er '.draft | booleans | tostring' "$github_release_json")" || \
    die 'GitHub release metadata has no valid draft state'
  is_immutable="$(jq -er '.immutable | booleans | tostring' "$github_release_json")" || \
    die 'GitHub release metadata has no valid immutable state'
  [[ "$tag_name" == "$version" ]] || die 'GitHub release tag does not match the requested version'
  [[ "$is_draft" == "false" ]] || die 'GitHub release is still a draft'
  [[ "$is_immutable" == "true" ]] || \
    die 'GitHub release is not immutable; enable release immutability before enrolling clients'
}

expected_github_download_url() {
  local file="$1"
  printf 'https://github.com/%s/releases/download/%s/%s\n' "$github_repo" "$version" "$file"
}

github_asset_api_url() {
  local file="$1"
  local asset_url
  local asset_id
  asset_url="$(jq -er --arg file "$file" '
    [.assets[] | select(.name == $file and .state == "uploaded") | .url]
    | if length == 1 then .[0] else empty end
  ' "$github_release_json")" || die "GitHub release has no unique uploaded asset: $file"
  [[ "$asset_url" == "https://api.github.com/repos/${github_repo}/releases/assets/"* ]] || \
    die 'GitHub release asset URL is outside the selected repository'
  asset_id="${asset_url##*/}"
  [[ "$asset_id" =~ ^[0-9]+$ ]] || die 'GitHub release asset URL is malformed'
  printf '%s\n' "$asset_url"
}

download_github_asset() {
  local file="$1"
  local destination="$2"
  local asset_url
  local headers
  local status
  local redirect_url
  asset_url="$(github_asset_api_url "$file")"
  headers="$(mktemp "${tmp}/github-asset-headers.XXXXXX")"
  rm -f "$destination"
  status="$(curl --config "$github_curl_config" --fail --silent --show-error \
    --dump-header "$headers" --write-out '%{http_code}' \
    -H 'Accept: application/octet-stream' "$asset_url" -o "$destination")" || \
    die "unable to download GitHub release asset: $file"
  case "$status" in
    200) ;;
    302)
      redirect_url="$(sed -nE 's/^[Ll]ocation:[[:space:]]*([^[:space:]\r]+).*/\1/p' "$headers" | head -n 1)"
      [[ "$redirect_url" =~ ^https://[^[:space:]]+$ ]] || \
        die 'GitHub release asset redirect is missing or unsafe'
      rm -f "$destination"
      curl --fail --location --silent --show-error "$redirect_url" -o "$destination" || \
        die "unable to follow GitHub release asset redirect: $file"
      ;;
    *) die "unexpected GitHub release asset status ${status}: $file" ;;
  esac
  [[ -f "$destination" && ! -L "$destination" ]] || \
    die "GitHub release did not provide the expected asset: $file"
}

action="${1:-}"
[[ -n "$action" ]] || { usage >&2; exit 2; }
shift || true

config_path="$DEFAULT_CONFIG"
codex_config="$DEFAULT_CODEX_CONFIG"
catalog_path=""
codex_executable=""
manage_app_server="false"
app_server_home=""
legacy_migration=false
defer_codex_routing=false
github_repo=""
github_token_file=""
github_token=""
github_curl_config=""
github_release_json=""
release_source=""
upstream_mode="external_gateway"
credential_source=""
responses_websocket_mode=""
catalog_owner=""
bounded_json_models=()
install_transaction_active=false
install_dir_created=false
version_dir_created=false
menu_bar_link=""
menu_bar_binding=""
macos_bundle_mode=false
routing_state_path=""
routing_initialized_path=""
routing_journal_path=""
local_enrollment_path=""
local_catalog_path=""
local_catalog_pending_path=""

case "$action" in
  install)
    version="${1:-}"
    [[ -n "$version" ]] || { usage >&2; exit 2; }
    shift
    require_version "$version"
    base_url=""; public_key=""; upstream=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --release-base-url) base_url="${2:-}"; shift 2 ;;
        --github-repo) github_repo="${2:-}"; shift 2 ;;
        --github-token-file) github_token_file="${2:-}"; shift 2 ;;
        --public-key) public_key="${2:-}"; shift 2 ;;
        --upstream) upstream="${2:-}"; shift 2 ;;
        --upstream-mode) upstream_mode="${2:-}"; shift 2 ;;
        --credentials) credential_source="${2:-}"; shift 2 ;;
        --responses-websocket-mode) responses_websocket_mode="${2:-}"; shift 2 ;;
        --bounded-json-model) bounded_json_models+=("${2:-}"); shift 2 ;;
        --catalog-owner) catalog_owner="${2:-}"; shift 2 ;;
        --config) config_path="${2:-}"; shift 2 ;;
        --codex-config) codex_config="${2:-}"; shift 2 ;;
        --catalog-path) catalog_path="${2:-}"; shift 2 ;;
        --codex-executable) codex_executable="${2:-}"; shift 2 ;;
        --manage-app-server) manage_app_server="${2:-}"; shift 2 ;;
        --app-server-home) app_server_home="${2:-}"; shift 2 ;;
        --migrate-legacy) legacy_migration=true; shift ;;
        --defer-codex-routing) defer_codex_routing=true; shift ;;
        *) usage >&2; die "unknown argument: $1" ;;
      esac
    done
    if [[ -n "$base_url" && ( -n "$github_repo" || -n "$github_token_file" ) ]]; then
      die '--release-base-url and --github-repo are mutually exclusive'
    fi
    if [[ -n "$github_repo" || -n "$github_token_file" ]]; then
      [[ -n "$github_repo" ]] || die '--github-token-file requires --github-repo'
      github_repo_valid "$github_repo" || die '--github-repo must be OWNER/REPO'
      [[ -z "$github_token_file" ]] || require_private_token_file "$github_token_file"
      release_source="github"
    else
      [[ "$base_url" =~ ^https://[^/?#]+(/[^?#]*)?$ ]] || \
        die '--release-base-url must be HTTPS when GitHub release options are absent'
      [[ ! "$base_url" =~ ^https://github\.com/[^/]+/[^/]+/releases/download$ ]] || \
        die 'GitHub release URLs require --github-repo and --github-token-file'
      release_source="https"
    fi
    case "$upstream_mode" in
      external_gateway)
        [[ "$upstream" =~ ^https://[^/?#]+/v1$ ]] || die '--upstream must be an HTTPS /v1 URL for external_gateway'
        credential_source="${credential_source:-$(if [[ "$(uname -s)" == "Darwin" ]]; then printf keychain; else printf file; fi)}"
        catalog_owner="${catalog_owner:-relay}"
        [[ "$credential_source" == "keychain" || "$credential_source" == "file" ]] || \
          die '--credentials must be keychain or file for external_gateway'
        [[ "$catalog_owner" == "relay" ]] || die '--catalog-owner must be relay for external_gateway'
        ;;
      local_opencodex)
        [[ "$upstream" == "http://127.0.0.1:10100/v1" || "$upstream" == "http://[::1]:10100/v1" ]] || \
          die '--upstream must be the fixed numeric loopback OpenCodex URL for local_opencodex'
        credential_source="${credential_source:-none}"
        catalog_owner="${catalog_owner:-remote_manager}"
        [[ "$credential_source" == "none" ]] || die '--credentials must be none for local_opencodex'
        [[ "$catalog_owner" == "remote_manager" ]] || die '--catalog-owner must be remote_manager for local_opencodex'
        ;;
      *) die '--upstream-mode must be external_gateway or local_opencodex' ;;
    esac
    if [[ "$(uname -s)" == Darwin && "$upstream_mode" != external_gateway ]]; then
      die 'macOS MenuBar installs require external_gateway as the canonical profile; enroll Local OpenCodex through the explicit MenuBar handoff'
    fi
    if ((${#bounded_json_models[@]} > 0)); then
      responses_websocket_mode="${responses_websocket_mode:-http_fallback}"
    else
      responses_websocket_mode="${responses_websocket_mode:-passthrough}"
    fi
    [[ "$responses_websocket_mode" == "passthrough" || "$responses_websocket_mode" == "http_fallback" ]] || \
      die '--responses-websocket-mode must be passthrough or http_fallback'
    [[ "$manage_app_server" == "true" || "$manage_app_server" == "false" ]] || die '--manage-app-server must be true or false'
    [[ "$manage_app_server" != "true" || "$app_server_home" == /* ]] || \
      die '--app-server-home must be an absolute path when --manage-app-server=true'
    config_path="$(canonical_install_path "$config_path")" || die '--config must name a file below a writable canonical directory'
    codex_config="$(canonical_install_path "$codex_config")" || die '--codex-config must name a file below a writable canonical directory'
    [[ -f "$public_key" && ! -L "$public_key" ]] || die '--public-key must be a regular PEM file'
    if [[ "$release_source" == "github" ]]; then
      command -v curl >/dev/null || die 'curl is required for --github-repo installs'
      command -v jq >/dev/null || die 'jq is required for --github-repo installs'
      if [[ -n "$github_token_file" ]]; then
        read_github_token
      fi
    else
      command -v curl >/dev/null || die 'curl is required'
    fi
    command -v openssl >/dev/null || die 'openssl is required'
    command -v base64 >/dev/null || die 'base64 is required'
    command -v jq >/dev/null || die 'jq is required'
    command -v shasum >/dev/null || command -v sha256sum >/dev/null || die 'sha256 tool is required'
    require_ed25519_public_key "$public_key"
    interactive_profile="$(dirname -- "$codex_config")/${INTERACTIVE_PROFILE_BASENAME}"
    preflight_interactive_profile "$interactive_profile"
    read -r goos goarch <<<"$(normalize_target)"
    tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay.XXXXXX")"
    trap finish_install EXIT
    trap 'exit 129' HUP
    trap 'exit 130' INT
    trap 'exit 131' QUIT
    trap 'exit 143' TERM
    if [[ "$release_source" == "github" ]]; then
      github_curl_config="${tmp}/github-api.curl"
      github_release_json="${tmp}/github-release.json"
      write_github_curl_config "$github_curl_config"
      github_release_state
    fi
    manifest="${tmp}/manifest.json"
    signature="${tmp}/manifest.sig"
    signature_binary="${tmp}/manifest.sig.bin"
    if [[ "$release_source" == "github" ]]; then
      download_github_asset "manifest-${version}.json" "$manifest"
      download_github_asset "manifest-${version}.sig" "$signature"
    else
      curl --fail --location --silent --show-error "${base_url%/}/${version}/manifest-${version}.json" -o "$manifest"
      curl --fail --location --silent --show-error "${base_url%/}/${version}/manifest-${version}.sig" -o "$signature"
    fi
    decode_base64 "$signature" "$signature_binary"
    openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin -in "$manifest" -sigfile "$signature_binary" >/dev/null || die 'release manifest signature is invalid'
    manifest_version="$(manifest_string "$manifest" version)"
    [[ -n "$manifest_version" ]] || die 'release manifest has no valid version'
    [[ "$manifest_version" == "$version" ]] || die 'release manifest version does not match the requested version'
    compatibility_revision="$(manifest_integer "$manifest" compatibility_revision)"
    [[ "$compatibility_revision" == 1 || "$compatibility_revision" == 2 || "$compatibility_revision" == 4 ]] || \
      die 'release manifest compatibility revision is unsupported'
    jq -e --argjson revision "$compatibility_revision" '
      (.artifacts | type == "array") and
      (if $revision == 1 then
        (keys | sort == ["artifacts", "compatibility_revision", "version"])
      else
        (keys | sort == ["artifacts", "compatibility_revision", "documents", "version"])
        and (.documents | type == "array")
      end)
    ' "$manifest" >/dev/null || die 'release manifest contains unknown or malformed top-level fields'
    notices_url=""
    notices_hash=""
    if [[ "$compatibility_revision" == 2 || "$compatibility_revision" == 4 ]]; then
      jq -e '
        (.documents | length == 1) and
        (.documents[0] | keys | sort == ["file", "sha256", "url"])
      ' "$manifest" >/dev/null || die 'release manifest document contains unknown fields'
      row="$(document_row "$manifest" "$THIRD_PARTY_NOTICES_FILE")" || \
        die 'release manifest has no unique THIRD_PARTY_NOTICES.md document'
      IFS='|' read -r notices_url notices_hash <<<"$row"
      [[ "$notices_url" =~ ^https:// ]] || die 'manifest third-party notices URL must be HTTPS'
      [[ "$notices_hash" =~ ^[0-9a-f]{64}$ ]] || die 'manifest third-party notices checksum is invalid'
      if [[ "$release_source" == "github" ]]; then
        [[ "$notices_url" == "$(expected_github_download_url "$THIRD_PARTY_NOTICES_FILE")" ]] || \
          die 'manifest third-party notices URL does not match the selected GitHub release'
      fi
    fi
    if [[ "$compatibility_revision" == 4 ]]; then
      jq -e '
        (.artifacts | length == 5) and
        ([.artifacts[] | select(.os == "linux")]
          | length == 4
          and all(.[]; keys | sort == ["arch", "component", "file", "os", "sha256", "url"]))
      ' "$manifest" >/dev/null || die 'revision 4 Linux artifacts contain unknown or incomplete fields'
    fi
    macos_bundle_mode=false
    if [[ "$compatibility_revision" == 4 && "$goos" == darwin ]]; then
      macos_bundle_mode=true
      row="$(component_artifact_row "$manifest" "$goos" "$goarch" macos_menu_bar_bundle)" || \
        die 'revision 4 manifest has no unique macOS MenuBar bundle'
      IFS='|' read -r bundle_file bundle_url bundle_hash bundle_id bundle_signing_mode <<<"$row"
      [[ "$bundle_file" == "$MACOS_MENU_BAR_ZIP" && "$bundle_url" =~ ^https:// && \
         "$bundle_hash" =~ ^[0-9a-f]{64}$ && "$bundle_id" == "$MACOS_MENU_BAR_BUNDLE_ID" && \
         "$bundle_signing_mode" == adhoc ]] || die 'revision 4 macOS MenuBar bundle metadata is invalid'
      jq -e '
        [.artifacts[] | select(.component == "macos_menu_bar_bundle")]
        | length == 1
        and (.[0] | keys | sort == ["arch", "bundle_id", "component", "file", "os", "sha256", "signing_mode", "url"])
      ' "$manifest" >/dev/null || die 'revision 4 macOS artifact contains unknown fields'
      if [[ "$release_source" == github ]]; then
        [[ "$bundle_url" == "$(expected_github_download_url "$bundle_file")" ]] || \
          die 'manifest macOS bundle URL does not match the selected GitHub release'
      fi
    else
      relay_file="opencodex-relay_${goos}_${goarch}"
      relayctl_file="opencodex-relayctl_${goos}_${goarch}"
      if [[ "$compatibility_revision" == 4 ]]; then
        row="$(component_artifact_row "$manifest" "$goos" "$goarch" relay)" || \
          die "revision 4 manifest has no relay for ${goos}/${goarch}"
        IFS='|' read -r selected_relay_file relay_url relay_hash ignored_bundle_id ignored_signing_mode <<<"$row"
        [[ "$selected_relay_file" == "$relay_file" && -z "$ignored_bundle_id" && -z "$ignored_signing_mode" ]] || \
          die 'revision 4 relay component metadata is invalid'
        row="$(component_artifact_row "$manifest" "$goos" "$goarch" relayctl)" || \
          die "revision 4 manifest has no relayctl for ${goos}/${goarch}"
        IFS='|' read -r selected_relayctl_file relayctl_url relayctl_hash ignored_bundle_id ignored_signing_mode <<<"$row"
        [[ "$selected_relayctl_file" == "$relayctl_file" && -z "$ignored_bundle_id" && -z "$ignored_signing_mode" ]] || \
          die 'revision 4 relayctl component metadata is invalid'
      else
        row="$(artifact_row "$manifest" "$goos" "$goarch" "$relay_file")"
        [[ -n "$row" ]] || die "manifest has no artifact for ${goos}/${goarch}"
        IFS='|' read -r relay_url relay_hash <<<"$row"
        row="$(artifact_row "$manifest" "$goos" "$goarch" "$relayctl_file")"
        [[ -n "$row" ]] || die "manifest has no relayctl artifact for ${goos}/${goarch}"
        IFS='|' read -r relayctl_url relayctl_hash <<<"$row"
      fi
      [[ "$relay_url" =~ ^https:// && "$relay_hash" =~ ^[0-9a-f]{64}$ ]] || \
        die 'manifest relay metadata is invalid'
      [[ "$relayctl_url" =~ ^https:// && "$relayctl_hash" =~ ^[0-9a-f]{64}$ ]] || \
        die 'manifest relayctl metadata is invalid'
      if [[ "$release_source" == github ]]; then
        [[ "$relay_url" == "$(expected_github_download_url "$relay_file")" ]] || \
          die 'manifest relay URL does not match the selected GitHub release'
        [[ "$relayctl_url" == "$(expected_github_download_url "$relayctl_file")" ]] || \
          die 'manifest relayctl URL does not match the selected GitHub release'
      fi
    fi
    # A revision-4 MenuBar embeds its own routing helper. Installing a raw legacy
    # release over that control surface would leave a stale signed app pointing
    # at a different routing contract. Refuse before touching config/current/
    # service state rather than claiming an unsafe in-place downgrade.
    if [[ "$(uname -s)" == Darwin && "$macos_bundle_mode" != true && ( -e "$MACOS_MENU_BAR_LINK" || -L "$MACOS_MENU_BAR_LINK" || -e "$MACOS_MENU_BAR_BINDING" || -L "$MACOS_MENU_BAR_BINDING" ) ]]; then
      die 'legacy macOS downgrade requires a completed OpenCodexRelay uninstall first; the revision-4 MenuBar control surface is still registered'
    fi
    if [[ ! -e "${INSTALL_ROOT}/${version}" && ! -L "${INSTALL_ROOT}/${version}" ]]; then
      version_dir_created=true
    fi
    install -d -m 0700 "$INSTALL_ROOT" "${INSTALL_ROOT}/${version}"
    staging_dir="$(mktemp -d "${INSTALL_ROOT}/.stage-${version}-${goos}-${goarch}.XXXXXX")"
    install_dir="${INSTALL_ROOT}/${version}/${goos}-${goarch}"
    notices_path="${staging_dir}/${THIRD_PARTY_NOTICES_FILE}"
    if [[ "$macos_bundle_mode" == true ]]; then
      bundle_archive="${tmp}/${bundle_file}"
      if [[ "$release_source" == github ]]; then
        download_github_asset "$bundle_file" "$bundle_archive"
      else
        curl --fail --location --silent --show-error "$bundle_url" -o "$bundle_archive"
      fi
      verify_and_extract_macos_bundle "$bundle_archive" "$bundle_hash" "$bundle_id" "$bundle_signing_mode" "$staging_dir" "$compatibility_revision"
      printf '%s\n' "$bundle_hash" > "${staging_dir}/.bundle-sha256"
      chmod 0600 "${staging_dir}/.bundle-sha256"
      relay_path="${staging_dir}/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers/opencodex-relay"
      relayctl_path="${staging_dir}/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers/opencodex-relayctl"
    else
      relay_path="${staging_dir}/opencodex-relay"
      relayctl_path="${staging_dir}/opencodex-relayctl"
      if [[ "$release_source" == github ]]; then
        download_github_asset "$relay_file" "$relay_path"
      else
        curl --fail --location --silent --show-error "$relay_url" -o "$relay_path"
      fi
      [[ "$(sha256 "$relay_path")" == "$relay_hash" ]] || die 'relay SHA-256 does not match manifest'
      if [[ "$release_source" == github ]]; then
        download_github_asset "$relayctl_file" "$relayctl_path"
      else
        curl --fail --location --silent --show-error "$relayctl_url" -o "$relayctl_path"
      fi
      [[ "$(sha256 "$relayctl_path")" == "$relayctl_hash" ]] || die 'relayctl SHA-256 does not match manifest'
      chmod 0700 "$relay_path" "$relayctl_path"
    fi
    if [[ "$compatibility_revision" == 2 || "$compatibility_revision" == 4 ]]; then
      if [[ "$release_source" == "github" ]]; then
        download_github_asset "$THIRD_PARTY_NOTICES_FILE" "$notices_path"
      else
        curl --fail --location --silent --show-error "$notices_url" -o "$notices_path"
      fi
      [[ "$(sha256 "$notices_path")" == "$notices_hash" ]] || \
        die 'THIRD_PARTY_NOTICES.md SHA-256 does not match manifest'
      chmod 0644 "$notices_path"
    fi
    if [[ -e "$install_dir" || -L "$install_dir" ]]; then
      [[ -d "$install_dir" && ! -L "$install_dir" ]] || die "existing release target is unsafe: $install_dir"
      if [[ "$macos_bundle_mode" == true ]]; then
        existing_app="${install_dir}/${MACOS_MENU_BAR_BUNDLE}"
        [[ -d "$existing_app" && ! -L "$existing_app" && \
           -x "${existing_app}/Contents/Library/Helpers/opencodex-relay" && \
           -x "${existing_app}/Contents/Library/Helpers/opencodex-relayctl" && \
           -f "${install_dir}/.bundle-sha256" && ! -L "${install_dir}/.bundle-sha256" && \
           "$(<"${install_dir}/.bundle-sha256")" == "$bundle_hash" ]] || \
          die "existing macOS release target is incomplete or differs from the signed manifest: $install_dir"
        codesign --verify --deep --strict --verbose=2 "$existing_app" || die 'existing macOS bundle codesign validation failed'
        existing_codesign="$(codesign -dvvv --verbose=4 "$existing_app" 2>&1)"
        grep -Fx 'Signature=adhoc' <<<"$existing_codesign" >/dev/null || die 'existing macOS bundle is not ad-hoc signed'
        grep -Fx 'TeamIdentifier=not set' <<<"$existing_codesign" >/dev/null || die 'existing macOS bundle unexpectedly has an Apple Team ID'
        grep -E '^CodeDirectory .*flags=.*\(.*runtime.*\)' <<<"$existing_codesign" >/dev/null || \
          die 'existing macOS bundle does not use the Hardened Runtime'
      else
        [[ -x "${install_dir}/opencodex-relay" && -x "${install_dir}/opencodex-relayctl" ]] || \
          die "existing release target is incomplete: $install_dir"
        [[ "$(sha256 "${install_dir}/opencodex-relay")" == "$relay_hash" && \
           "$(sha256 "${install_dir}/opencodex-relayctl")" == "$relayctl_hash" ]] || \
          die "existing release target differs from the signed manifest: $install_dir"
      fi
      if [[ "$compatibility_revision" == 2 || "$compatibility_revision" == 4 ]]; then
        [[ -f "${install_dir}/${THIRD_PARTY_NOTICES_FILE}" && ! -L "${install_dir}/${THIRD_PARTY_NOTICES_FILE}" ]] || \
          die "existing release target has no third-party notices: $install_dir"
        [[ "$(sha256 "${install_dir}/${THIRD_PARTY_NOTICES_FILE}")" == "$notices_hash" ]] || \
          die "existing release target third-party notices differ from the signed manifest: $install_dir"
      fi
      rm -rf -- "$staging_dir"
    else
      mv "$staging_dir" "$install_dir"
      install_dir_created=true
    fi
    if [[ "$macos_bundle_mode" == true ]]; then
      relay_path="${install_dir}/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers/opencodex-relay"
      relayctl_path="${install_dir}/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers/opencodex-relayctl"
    else
      relay_path="${install_dir}/opencodex-relay"
      relayctl_path="${install_dir}/opencodex-relayctl"
    fi
    # Snapshot every mutable consumer-facing state before relayctl init,
    # migration, or enable can change it. A failed service activation must not
    # leave native Codex routing pointed at a relay which never came online.
    current_path="${INSTALL_ROOT}/current"
    previous_current_target=""
    if [[ -L "$current_path" ]]; then
      previous_current_target="$(readlink "$current_path")"
      [[ "$previous_current_target" != /* && "$previous_current_target" != *".."* ]] || \
        die 'existing current relay target is unsafe'
    elif [[ -e "$current_path" ]]; then
      die "existing current relay target is unsafe: $current_path"
    fi
    transaction_dir="${tmp}/install-transaction"
    install -d -m 0700 "$transaction_dir" "${transaction_dir}/service"
    preflight_interactive_profile "$interactive_profile"
    routing_state_path="${config_path}.routing-state.json"
    routing_initialized_path="${config_path}.routing-initialized"
    routing_journal_path="${config_path}.routing-transaction.json"
	local_enrollment_path="${config_path}.local-opencodex-enrollment.json"
	local_catalog_path="$(dirname -- "$codex_config")/opencodex-relay-local-catalog.json"
	if [[ -f "$config_path" && ! -L "$config_path" ]]; then
	  configured_local_catalog="$(jq -er '.local_opencodex.catalog_path // empty' "$config_path" 2>/dev/null || true)"
	  if [[ -n "$configured_local_catalog" ]]; then
	    [[ "$configured_local_catalog" == /* && "$configured_local_catalog" != *".."* ]] || \
	      die 'existing Local OpenCodex catalog path is unsafe'
	    local_catalog_path="$configured_local_catalog"
	  fi
	fi
	local_catalog_pending_path="${local_catalog_path}.restart-pending"
    snapshot_regular_file "$config_path" "${transaction_dir}/relay-config"
    snapshot_regular_file "$codex_config" "${transaction_dir}/codex-config"
    snapshot_regular_file "$interactive_profile" "${transaction_dir}/interactive-profile"
    snapshot_owner_only_control_file "$routing_state_path" "${transaction_dir}/routing-state"
    snapshot_owner_only_control_file "$routing_initialized_path" "${transaction_dir}/routing-initialized"
    snapshot_owner_only_control_file "$routing_journal_path" "${transaction_dir}/routing-journal"
	    snapshot_owner_only_control_file "$local_enrollment_path" "${transaction_dir}/local-enrollment"
	    snapshot_regular_file "$local_catalog_path" "${transaction_dir}/local-catalog"
	    snapshot_regular_file "$local_catalog_pending_path" "${transaction_dir}/local-catalog-pending"
    if [[ "$macos_bundle_mode" == true ]]; then
      menu_bar_link="$MACOS_MENU_BAR_LINK"
      menu_bar_binding="$MACOS_MENU_BAR_BINDING"
      snapshot_managed_menu_link "$menu_bar_link" "${transaction_dir}/menu-bar-link"
      preflight_menu_bar_binding "$menu_bar_binding"
      snapshot_regular_file "$menu_bar_binding" "${transaction_dir}/menu-bar-binding"
    fi
    "${SCRIPT_DIR}/install-service.sh" snapshot --directory "${transaction_dir}/service" || \
      die 'unable to snapshot the prior relay service artifact and manager state'
    # From this point onward every non-zero exit runs the same rollback. This
    # covers config validation, migration, durable routing intent, current-link
    # swap, and service activation rather than only the final service command.
    install_transaction_active=true
    if [[ -e "$config_path" || -L "$config_path" ]]; then
      [[ -f "$config_path" && ! -L "$config_path" ]] || die "existing relay config is unsafe: $config_path"
	  jq -e '(.installation_scope // "production") == "production"' "$config_path" >/dev/null || \
		die 'production installer refuses a local_development relay config'
      [[ "$(configured_upstream "$config_path")" == "$upstream" ]] || \
        die 'existing relay config has a different upstream; inspect it or explicitly replace it with relayctl init --force'
      [[ "$(configured_upstream_mode "$config_path")" == "$upstream_mode" ]] || \
        die 'existing relay config has a different upstream mode; inspect it or explicitly replace it with relayctl init --force'
      jq -e \
        --arg credentials "$credential_source" \
        --arg websocket "$responses_websocket_mode" \
        --arg owner "$catalog_owner" \
        --argjson manage "$manage_app_server" '
          def go_empty_default($fallback):
            if . == null or . == "" then $fallback else . end;
          (.credentials.source // $credentials) == $credentials
          and (.responses.websocket_mode | go_empty_default("passthrough")) == $websocket
          and (.catalog.owner | go_empty_default("relay")) == $owner
          and (.catalog.manage_app_server // false) == $manage
        ' "$config_path" >/dev/null || \
        die 'existing relay config does not match the requested credential, Responses, catalog-owner, or AppServer policy; inspect it or explicitly replace it with relayctl init --force'
      if [[ -n "$catalog_path" ]]; then
        jq -e --arg expected "$catalog_path" '.catalog.path == $expected' "$config_path" >/dev/null || \
          die 'existing relay config has a different catalog path; inspect it or explicitly replace it with relayctl init --force'
      fi
      if [[ -n "$codex_executable" ]]; then
        jq -e --arg expected "$codex_executable" '.catalog.codex_executable == $expected' "$config_path" >/dev/null || \
          die 'existing relay config has a different Codex executable; inspect it or explicitly replace it with relayctl init --force'
      fi
      for model in "${bounded_json_models[@]+"${bounded_json_models[@]}"}"; do
        jq -e --arg model "$model" '
          (.responses.model_modes // {})
          | to_entries
          | map(select((.key | ascii_downcase) == ($model | ascii_downcase) and .value == "bounded_json"))
          | length == 1
        ' "$config_path" >/dev/null || \
          die "existing relay config is missing the requested bounded_json model policy: $model"
      done
    else
      init_args=(
        init --upstream "$upstream" --upstream-mode "$upstream_mode"
        --credentials "$credential_source"
        --responses-websocket-mode "$responses_websocket_mode"
        --catalog-owner "$catalog_owner"
        --config "$config_path"
      )
      for model in "${bounded_json_models[@]+"${bounded_json_models[@]}"}"; do
        init_args+=(--bounded-json-model "$model")
      done
      [[ -z "$catalog_path" ]] || init_args+=(--catalog-path "$catalog_path")
      [[ -z "$codex_executable" ]] || init_args+=(--codex-executable "$codex_executable")
      # Go's bool flags must use --flag=false. With a separate false argument,
      # flag.Parse stops at that non-flag and silently ignores later Remote paths.
      init_args+=(--manage-app-server="${manage_app_server}")
      [[ -z "$app_server_home" ]] || init_args+=(--app-server-home "$app_server_home")
      if [[ "$macos_bundle_mode" == true && "$upstream_mode" == external_gateway ]]; then
        init_args+=(--connection-probe-enabled)
      fi
      "$relayctl_path" "${init_args[@]}"
    fi
    if [[ "$macos_bundle_mode" == true && "$upstream_mode" == external_gateway ]]; then
      enable_macos_connection_probe "$config_path"
    fi
    "$relay_path" --config "$config_path" --check
    require_interactive_listener_available
    if [[ "$legacy_migration" == true && "$defer_codex_routing" == false ]]; then
      "$relayctl_path" migrate-legacy --codex-config "$codex_config"
    fi
    if [[ "$compatibility_revision" == 4 ]]; then
      if [[ "$defer_codex_routing" == false ]]; then
        # This is deliberately intent-only. The service starts parked when a
        # native Codex config needs to move to relay routing; the MenuBar (or
        # an explicit CLI operator) must first confirm the Desktop has exited
        # and run `mode apply` before any owned TOML/profile mutation occurs.
		request_install_routing_state "$relayctl_path" "$config_path" "$codex_config"
      else
        seed_deferred_routing_state "$relayctl_path" "$config_path" "$codex_config"
      fi
    elif [[ "$defer_codex_routing" == false ]]; then
      # A reviewed revision 1/2 rollback binary has no routing controller.
      # Retain its historic setup path only for compatibility; users must
      # close Desktop themselves before enrolling such a legacy release.
      legacy_write_interactive_profile "$config_path" "$interactive_profile"
      "$relayctl_path" enable --config "$config_path" --codex-config "$codex_config"
    fi
    # Keep the previously selected release authoritative until the downloaded
    # binaries, existing/new config, credentials, and Codex routing all pass.
    # install-service resolves this link, so switch it only immediately before
    # the service is installed or restarted.
    current_candidate="${INSTALL_ROOT}/.current.$$"
    current_target="${version}/${goos}-${goarch}"
    if [[ "$macos_bundle_mode" == true ]]; then
      current_target+="/${MACOS_MENU_BAR_BUNDLE}/Contents/Library/Helpers"
    fi
    ln -s "$current_target" "$current_candidate"
    replace_current_link "$current_candidate" "$current_path" || die 'unable to select the verified relay release'
    service_install_status=0
    "${SCRIPT_DIR}/install-service.sh" install --config "$config_path" || service_install_status=$?
    if ((service_install_status != 0)); then
      exit "$service_install_status"
    fi
    wait_for_dual_listener_health "$config_path" "$relayctl_path" "$codex_config"
    if [[ "$compatibility_revision" == 4 ]]; then
      if [[ "$defer_codex_routing" == false ]]; then
        verify_requested_routing_state "$relayctl_path" "$config_path" "$codex_config"
      else
        verify_deferred_routing_state "$relayctl_path" "$config_path" "$codex_config"
      fi
    fi
    if [[ "$macos_bundle_mode" == true ]]; then
      installed_app="${install_dir}/${MACOS_MENU_BAR_BUNDLE}"
      write_menu_bar_binding "$config_path" "$codex_config"
      replace_managed_menu_link "$installed_app" "$menu_bar_link" || \
        die 'unable to select the verified macOS MenuBar app'
      printf 'macos_app_installed=%s gatekeeper_approval=manual\n' "$menu_bar_link"
      printf 'gatekeeper_next_step=open_OpenCodexRelay.app_in_Finder_then_use_Privacy_&_Security_Open_Anyway_if_blocked\n'
    fi
    install_transaction_active=false
    install_dir_created=false
    if [[ "$compatibility_revision" != 4 ]]; then
      printf 'relay_installed=%s target=%s/%s codex_routing=legacy_compatibility\n' "$version" "$goos" "$goarch"
    elif [[ "$defer_codex_routing" == true ]]; then
      printf 'relay_installed=%s target=%s/%s codex_routing=deferred\n' "$version" "$goos" "$goarch"
    else
      printf 'relay_installed=%s target=%s/%s codex_routing=relay_requested desktop_restart_required=true\n' "$version" "$goos" "$goarch"
      printf 'next_step=quit_the_registered_Codex_Desktop_then_run_opencodex-relayctl_mode_apply_or_use_the_MenuBar_app\n'
    fi
    ;;
  uninstall)
    confirm_desktop_exited=false
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --config) config_path="${2:-}"; shift 2 ;;
        --codex-config) codex_config="${2:-}"; shift 2 ;;
        --confirm-desktop-exited) confirm_desktop_exited=true; shift ;;
        *) usage >&2; die "unknown argument: $1" ;;
      esac
    done
    config_path="$(canonical_install_path "$config_path")" || die '--config must name a file below a writable canonical directory'
    codex_config="$(canonical_install_path "$codex_config")" || die '--codex-config must name a file below a writable canonical directory'
    interactive_profile="$(dirname -- "$codex_config")/${INTERACTIVE_PROFILE_BASENAME}"
    preflight_interactive_profile "$interactive_profile"
    if [[ "$(uname -s)" == Darwin ]]; then
      preflight_menu_bar_binding "$MACOS_MENU_BAR_BINDING"
    fi
    current="${INSTALL_ROOT}/current/opencodex-relayctl"
    [[ -x "$current" ]] || die 'current relayctl is unavailable; retain the service and use the MenuBar/CLI recovery flow after inspection'
    relayctl_usage="$("$current" 2>&1 || true)"
    if grep -Fq 'opencodex-relayctl mode status' <<<"$relayctl_usage"; then
      "$current" mode request native --config "$config_path" --codex-config "$codex_config"
      uninstall_status="$("$current" mode status --config "$config_path" --codex-config "$codex_config" --json)" || \
        die 'unable to read safe routing status before uninstall'
      uninstall_phase="$(jq -er '.phase | strings' <<<"$uninstall_status")" || \
        die 'routing status is malformed before uninstall'
      case "$uninstall_phase" in
        native_active)
          ;;
        native_pending_restart)
          if [[ "$confirm_desktop_exited" != true ]]; then
            printf 'relay_uninstall=pending_native_restart\n'
            printf 'next_step=quit_Codex_Desktop_then_rerun_install-relay.sh_uninstall_with_--confirm-desktop-exited_or_use_the_MenuBar_app\n'
            exit 3
          fi
          "$current" mode apply --confirm-desktop-exited --config "$config_path" --codex-config "$codex_config"
          ;;
        *)
          die "routing is ${uninstall_phase}; resolve it with the MenuBar or relayctl mode recover before uninstalling the service"
          ;;
      esac
    else
      # Revision 1/2 relayctl has no routing controller. Do not let the old
      # immediate disable path mutate a running Desktop route without the same
      # explicit exit acknowledgement required by the v3 flow.
      if [[ "$confirm_desktop_exited" != true ]]; then
        printf 'relay_uninstall=pending_legacy_desktop_exit\n'
        printf 'next_step=quit_Codex_Desktop_then_rerun_install-relay.sh_uninstall_with_--confirm-desktop-exited\n'
        exit 3
      fi
      "$current" disable --codex-config "$codex_config"
    fi
    if [[ -e "$interactive_profile" || -L "$interactive_profile" ]]; then
      die 'native routing apply did not remove the managed interactive profile; service remains installed for recovery'
    fi
    if [[ "$(uname -s)" == Darwin ]]; then
      require_manual_homebrew_guard_absent
      current_target="$(readlink "${INSTALL_ROOT}/current")" || die 'unable to inspect selected macOS relay release before unregistering login item'
      if [[ "$current_target" == */"${MACOS_MENU_BAR_BUNDLE}"/Contents/Library/Helpers ]]; then
        menu_bar_login_helper="${INSTALL_ROOT}/${current_target%/Contents/Library/Helpers}/Contents/MacOS/OpenCodexRelay"
        [[ -x "$menu_bar_login_helper" ]] || die 'selected macOS MenuBar login helper is unavailable'
        login_registration="$("$menu_bar_login_helper" --uninstall-login 2>/dev/null)" || \
          die 'unable to unregister the macOS MenuBar login item'
        [[ "$login_registration" == login_registration=disabled ]] || \
          die 'macOS MenuBar login item did not confirm removal'
      fi
    fi
    "${SCRIPT_DIR}/install-service.sh" uninstall || \
      die 'native routing is active but the relay service could not be uninstalled; service state was retained for inspection'
    if [[ "$(uname -s)" == Darwin && ( -e "$MACOS_MENU_BAR_LINK" || -L "$MACOS_MENU_BAR_LINK" ) ]]; then
      [[ -L "$MACOS_MENU_BAR_LINK" ]] || die "macOS MenuBar app path is not a managed symbolic link: $MACOS_MENU_BAR_LINK"
      menu_link_target="$(readlink "$MACOS_MENU_BAR_LINK")" || die 'unable to read macOS MenuBar app link'
      [[ "$menu_link_target" == "${INSTALL_ROOT}/"*"/${MACOS_MENU_BAR_BUNDLE}" ]] || \
        die "macOS MenuBar app link is not managed by opencodex-relay: $MACOS_MENU_BAR_LINK"
      rm -f -- "$MACOS_MENU_BAR_LINK"
    fi
    if [[ "$(uname -s)" == Darwin && ( -e "$MACOS_MENU_BAR_BINDING" || -L "$MACOS_MENU_BAR_BINDING" ) ]]; then
      preflight_menu_bar_binding "$MACOS_MENU_BAR_BINDING"
      rm -f -- "$MACOS_MENU_BAR_BINDING"
      rmdir "$MACOS_MENU_BAR_BINDING_DIR" 2>/dev/null || true
    fi
    trap - EXIT
    printf 'relay_service=uninstalled config_retained=%s\n' "$config_path"
    ;;
  *) usage >&2; exit 2 ;;
esac

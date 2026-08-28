#!/usr/bin/env bash
# Route a dedicated Remote Control Codex home through the existing native
# OpenAI relay, either to a colocated OpenCodex or to the external gateway.
set -euo pipefail

readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"
readonly CONFIG_FILE="${CONFIG_DIR}/remote-opencodex.json"
readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"
readonly CODEX_CONFIG="${REMOTE_HOME_PATH}/config.toml"
readonly RELAY_CONFIG="${CONFIG_DIR}/relay.json"
readonly INTERACTIVE_PROFILE="${REMOTE_HOME_PATH}/opencodex-relay-interactive.config.toml"
readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"
readonly RELAYCTL="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relayctl"
readonly MANAGER="/home/ubuntu/.local/lib/opencodex-relay/manage-remote-codex-home.sh"
readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"
readonly SUDO_BIN="/usr/bin/sudo"
readonly CATALOG_REFRESH_TIMER="opencodex-remote-catalog-refresh.timer"
readonly RELAY_ACTIVATION_TIMER="opencodex-remote-relay-catalog-activation.timer"
readonly DEFAULT_BOUNDED_CODEX_MODEL="opencode-go-responses/gpt-5.6-luna"

routing_transaction_active=false
routing_transaction_dir=""
catalog_refresh_enabled=""
catalog_refresh_active=""
relay_activation_enabled=""
relay_activation_active=""

usage() {
  cat <<'USAGE'
Usage:
  configure-remote-codex-routing.sh status
  configure-remote-codex-routing.sh verify-video-bridge-disabled
  configure-remote-codex-routing.sh enable-relay --allow-remote-interruption [--migrate-legacy]
  configure-remote-codex-routing.sh enable-local-relay --allow-remote-interruption [--migrate-legacy]

Both enable actions change only the dedicated Remote Codex home and require an
already installed, healthy local relay. enable-relay is valid only for
MODE=external with an external_gateway relay whose catalog owner is relay. It
enables only the relay catalog activation timer. enable-local-relay is valid
only for MODE=loopback with a local_opencodex relay whose catalog owner is
remote_manager. It keeps the Remote manager catalog refresh timer enabled and
the relay catalog activation timer disabled.

--migrate-legacy is explicit: it backs up and removes only a documented
legacy root model_provider (pw_opencodex, opencodex, or pw_opencodex_remote)
or the exact old local loopback base URL (127.0.0.1:10100 or localhost:10100).
It also removes the associated root model_catalog_json; provider tables are
retained. Do not use it for another custom provider or arbitrary base URL.

Local activation reads the effective colocated OpenCodex configuration as the
opencodex service account. It fails closed unless that configuration validates
and images.videoBridgeEnabled is false or absent. No operator attestation can
override this check.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_owned_regular_file() {
  local path="$1"
  local mode="$2"
  [[ -f "$path" && ! -L "$path" ]] || die "required regular file is unavailable: $path"
  [[ "$(stat -c '%U:%G:%a' "$path")" == "ubuntu:ubuntu:${mode}" ]] || \
    die "$path must be owned by ubuntu with mode $mode"
}

read_routing_mode() {
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || \
    die "Remote configuration loader is unavailable: $CONFIG_LOADER"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"
  printf '%s\n' "$ROUTING_MODE"
}

read_remote_mode() {
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || \
    die "Remote configuration loader is unavailable: $CONFIG_LOADER"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"
  printf '%s\n' "$MODE"
}

verify_video_bridge_disabled() {
  local runtime_description
  local runtime_home
  local opencodex_config
  if [[ ! -x "$SUDO_BIN" ]]; then
    die "passwordless service-account config verification is unavailable: $SUDO_BIN"
  fi
  runtime_description="$("$SUDO_BIN" -n -- "$RUNTIME_ADAPTER" describe --json)" || \
    die 'the OpenCodex Runtime Adapter or its contract is unavailable'
  runtime_home="$(jq -er '
    .home
    | select(
        type == "string"
        and length > 1
        and startswith("/")
        and (test("[\u0000-\u001f\u007f]") | not)
      )
  ' <<< "$runtime_description")" || \
    die 'the OpenCodex Runtime Adapter description has no valid home'
  opencodex_config="${runtime_home}/.opencodex/config.json"

  # Keep the effective configuration off disk. jq retains only the boolean
  # decision, and pipefail makes an adapter/ocx/sudo failure fail closed.
  if ! "$SUDO_BIN" -n -u opencodex -- env -u OPENCODEX_HOME -u CODEX_HOME \
      "$RUNTIME_ADAPTER" ocx config validate --json | \
      jq -e --arg source "$opencodex_config" \
        '.ok == true and .source == $source' >/dev/null; then
    die 'OpenCodex config validation did not identify the active service configuration'
  fi
  if ! "$SUDO_BIN" -n -u opencodex -- env -u OPENCODEX_HOME -u CODEX_HOME \
      "$RUNTIME_ADAPTER" ocx config show --json | \
      jq -e '
        ((.images // {}) | type == "object")
        and ((.images.videoBridgeEnabled // false) == false)
      ' >/dev/null; then
    die 'local Responses normalization requires OpenCodex images.videoBridgeEnabled=false'
  fi
  printf 'opencodex_video_bridge=disabled source=%s\n' "$opencodex_config"
}

timer_enabled_state() {
  local unit="$1"
  local state
  state="$(systemctl --user is-enabled "$unit" 2>/dev/null || true)"
  case "$state" in
    enabled|disabled) printf '%s\n' "$state" ;;
    *) die "cannot snapshot enabled state for $unit: ${state:-unknown}" ;;
  esac
}

timer_active_state() {
  local unit="$1"
  local state
  state="$(systemctl --user is-active "$unit" 2>/dev/null || true)"
  case "$state" in
    active|inactive) printf '%s\n' "$state" ;;
    *) die "cannot snapshot active state for $unit: ${state:-unknown}" ;;
  esac
}

restore_timer_state() {
  local unit="$1"
  local enabled="$2"
  local active="$3"
  local restore_failed=false
  if [[ "$enabled" == enabled ]]; then
    systemctl --user enable "$unit" >/dev/null || restore_failed=true
  else
    systemctl --user disable "$unit" >/dev/null || restore_failed=true
  fi
  if [[ "$active" == active ]]; then
    systemctl --user start "$unit" || restore_failed=true
  else
    systemctl --user stop "$unit" || restore_failed=true
  fi
  [[ "$restore_failed" == false ]]
}

restore_routing_file() {
  local path="$1"
  local snapshot="$2"
  local candidate
  [[ -f "$snapshot" && ! -L "$snapshot" ]] || return 1
  candidate="$(mktemp "${path}.rollback.XXXXXX")" || return 1
  if ! cp -p -- "$snapshot" "$candidate"; then
    rm -f -- "$candidate"
    return 1
  fi
  mv -f -- "$candidate" "$path"
}

snapshot_optional_routing_file() {
  local path="$1"
  local prefix="$2"
  if [[ -e "$path" || -L "$path" ]]; then
    [[ -f "$path" && ! -L "$path" ]] || die "routing transaction file is unsafe: $path"
    cp -p -- "$path" "${prefix}.file"
    printf 'present=true\n' > "${prefix}.state"
  else
    printf 'present=false\n' > "${prefix}.state"
  fi
  chmod 0600 "${prefix}.state"
}

restore_optional_routing_file() {
  local path="$1"
  local prefix="$2"
  local state
  state="$(sed -nE 's/^present=(true|false)$/\1/p' "${prefix}.state")"
  case "$state" in
    true) restore_routing_file "$path" "${prefix}.file" ;;
    false)
      if [[ -e "$path" || -L "$path" ]]; then
        [[ -f "$path" && ! -L "$path" ]] || return 1
        rm -f -- "$path"
      fi
      ;;
    *) return 1 ;;
  esac
}

rollback_routing() {
  local rollback_failed=false
  restore_routing_file "$CONFIG_FILE" "${routing_transaction_dir}/remote-opencodex.json" || rollback_failed=true
  restore_routing_file "$CODEX_CONFIG" "${routing_transaction_dir}/config.toml" || rollback_failed=true
  restore_optional_routing_file "$INTERACTIVE_PROFILE" "${routing_transaction_dir}/interactive-profile" || rollback_failed=true
  restore_timer_state "$CATALOG_REFRESH_TIMER" "$catalog_refresh_enabled" "$catalog_refresh_active" || rollback_failed=true
  restore_timer_state "$RELAY_ACTIVATION_TIMER" "$relay_activation_enabled" "$relay_activation_active" || rollback_failed=true
  # Re-read the restored routing configuration before restarting so the daemon
  # cannot remain on a candidate route whose file snapshots were rolled back.
  "$MANAGER" restart-daemon || rollback_failed=true
  [[ "$rollback_failed" == false ]]
}

finish_routing_transaction() {
  local status=$?
  trap - EXIT
  trap '' HUP INT QUIT TERM
  set +e
  if ((status != 0)) && [[ "$routing_transaction_active" == true ]]; then
    printf 'ERROR: relay routing activation failed; restoring Codex routing and timer state.\n' >&2
    rollback_routing || status=70
  fi
  rm -rf -- "${routing_transaction_dir:-}"
  exit "$status"
}

begin_routing_transaction() {
  routing_transaction_dir="$(mktemp -d "${CONFIG_DIR}/.routing-transaction.XXXXXX")"
  chmod 0700 "$routing_transaction_dir"
  trap finish_routing_transaction EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  cp -p -- "$CONFIG_FILE" "${routing_transaction_dir}/remote-opencodex.json"
  cp -p -- "$CODEX_CONFIG" "${routing_transaction_dir}/config.toml"
  snapshot_optional_routing_file "$INTERACTIVE_PROFILE" "${routing_transaction_dir}/interactive-profile"
  catalog_refresh_enabled="$(timer_enabled_state "$CATALOG_REFRESH_TIMER")"
  catalog_refresh_active="$(timer_active_state "$CATALOG_REFRESH_TIMER")"
  relay_activation_enabled="$(timer_enabled_state "$RELAY_ACTIVATION_TIMER")"
  relay_activation_active="$(timer_active_state "$RELAY_ACTIVATION_TIMER")"
  routing_transaction_active=true
}

commit_routing_transaction() {
  routing_transaction_active=false
  rm -rf -- "$routing_transaction_dir"
  routing_transaction_dir=""
  trap - EXIT HUP INT QUIT TERM
}

codex_root_model() {
  awk '
    BEGIN { in_root = 1 }
    /^[[:space:]]*\[/ { in_root = 0 }
    in_root && /^[[:space:]]*model[[:space:]]*=/ {
      count++
      value = $0
      sub(/^[[:space:]]*model[[:space:]]*=[[:space:]]*"/, "", value)
      sub(/"[[:space:]]*(#.*)?$/, "", value)
      result = value
    }
    END {
      if (count != 1) exit 3
      print result
    }
  ' "$CODEX_CONFIG" || die 'Codex config must contain one simple quoted root model before relay activation'
}

write_routing_mode() {
  local desired="$1"
  local candidate

  case "$desired" in
    relay|local-relay) ;;
    *) die "unsupported routing mode transition: $desired" ;;
  esac
  candidate="$(mktemp "${CONFIG_FILE}.XXXXXX")"
  if ! jq -e --arg desired "$desired" \
      '.routing_mode = $desired' "$CONFIG_FILE" > "$candidate"; then
    rm -f -- "$candidate"
    die 'unable to update Remote routing JSON'
  fi
  chmod 0600 "$candidate"
  chown ubuntu:ubuntu "$candidate"
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  if ! load_remote_config "$candidate" "$REMOTE_HOME_PATH"; then
    rm -f -- "$candidate"
    die 'updated Remote routing JSON failed strict validation'
  fi
  mv -f "$candidate" "$CONFIG_FILE"
}

relay_listeners() {
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
  ' "$RELAY_CONFIG"
}

health_matches_listener_lane() {
  local health="$1"
  local expected_lane="$2"
  local expected_general="$3"
  local expected_interactive="$4"
  jq -e --slurpfile cfg "$RELAY_CONFIG" \
    --arg lane "$expected_lane" \
    --arg general "$expected_general" \
    --arg interactive "$expected_interactive" '
      def nonnegative_integer:
        type == "number" and floor == . and . >= 0;
      def go_zero_default($fallback):
        if . == null or . == 0 then $fallback else . end;
      def go_empty_default($fallback):
        if . == null or . == "" then $fallback else . end;
      ($cfg[0]) as $c
      | ($c.responses.scheduler // {}) as $s
      | .ok == true
      and .listener_lane == $lane
      and .general_listener == $general
      and .interactive_listener == $interactive
      and .upstream_mode == ($c.upstream_mode | go_empty_default("external_gateway"))
      and .upstream_base_url == $c.upstream_base_url
      and .catalog_owner == ($c.catalog.owner | go_empty_default("relay"))
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

require_dual_listener_health() {
  local listeners
  local general_listener
  local interactive_listener
  local general_health
  local interactive_health
  listeners="$(relay_listeners)" || die 'relay configuration has no supported distinct interactive loopback listener'
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  general_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "http://${general_listener}/__relay/healthz")" || die 'the relay general listener is unavailable'
  interactive_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "http://${interactive_listener}/__relay/healthz")" || die 'the relay interactive listener is unavailable'
  health_matches_listener_lane "$general_health" general "$general_listener" "$interactive_listener" || \
    die 'the relay general listener does not match the reviewed scheduler health contract'
  health_matches_listener_lane "$interactive_health" interactive "$general_listener" "$interactive_listener" || \
    die 'the relay interactive listener does not match the reviewed scheduler health contract'
}

relay_default_policy_enabled() {
  jq -e --arg model "$DEFAULT_BOUNDED_CODEX_MODEL" '
    (.responses.model_modes // {})
    | to_entries
    | map(select((.key | ascii_downcase) == ($model | ascii_downcase)
        and .value == "bounded_json"))
    | length == 1
  ' "$RELAY_CONFIG" >/dev/null
}

require_default_bounded_root_model() {
  local selected_model
  selected_model="$(codex_root_model)"
  jq -en --arg selected "$selected_model" --arg expected "$DEFAULT_BOUNDED_CODEX_MODEL" '
    ($selected | ascii_downcase) == ($expected | ascii_downcase)
  ' >/dev/null || \
    die "the selected Codex root model must be $DEFAULT_BOUNDED_CODEX_MODEL"
  relay_default_policy_enabled || \
    die "the relay policy must enable bounded_json for $DEFAULT_BOUNDED_CODEX_MODEL"
}

require_layout() {
  local desired_routing="$1"
  local remote_mode
  [[ "$(id -un)" == "ubuntu" ]] || die 'run this script as ubuntu, without sudo'
  require_owned_regular_file "$CONFIG_FILE" 600
  require_owned_regular_file "$RELAY_CONFIG" 600
  require_owned_regular_file "$CODEX_CONFIG" 600
  [[ -x "$RELAY" ]] || die "relay is unavailable: $RELAY"
  [[ -x "$RELAYCTL" ]] || die "relayctl is unavailable: $RELAYCTL"
  [[ -x "$MANAGER" ]] || die "Remote Codex manager is unavailable: $MANAGER"
  "$RELAY" --config "$RELAY_CONFIG" --check >/dev/null || \
    die 'the installed relay rejected its configuration or credential access'
  "$MANAGER" check-interactive-profile-ownership >/dev/null
  remote_mode="$(read_remote_mode)"
  case "$desired_routing" in
    relay)
      [[ "$remote_mode" == "external" ]] || \
        die 'ROUTING_MODE=relay requires MODE=external'
      jq -e --arg catalog "${REMOTE_HOME_PATH}/opencodex-catalog.json" '
        def go_empty_default($fallback):
          if . == null or . == "" then $fallback else . end;
        (.upstream_mode | go_empty_default("external_gateway")) == "external_gateway"
        and (.catalog.owner | go_empty_default("relay")) == "relay"
        and .catalog.path == $catalog
        and .catalog.manage_app_server == false
      ' "$RELAY_CONFIG" >/dev/null || \
        die 'external relay config must use external_gateway with catalog.owner=relay'
      ;;
    local-relay)
      [[ "$remote_mode" == "loopback" ]] || \
        die 'ROUTING_MODE=local-relay requires MODE=loopback'
      jq -e --arg catalog "${REMOTE_HOME_PATH}/opencodex-catalog.json" '
        .upstream_mode == "local_opencodex"
        and (.upstream_base_url == "http://127.0.0.1:10100/v1"
          or .upstream_base_url == "http://[::1]:10100/v1")
        and .credentials.source == "none"
        and .responses.websocket_mode == "http_fallback"
        and ((.responses.model_modes // {}) | length) > 0
        and .catalog.owner == "remote_manager"
        and .catalog.path == $catalog
        and .catalog.manage_app_server == false
      ' "$RELAY_CONFIG" >/dev/null || \
        die 'local relay config must use local_opencodex with catalog.owner=remote_manager'
      ;;
    *) die "unsupported relay routing mode: $desired_routing" ;;
  esac
  require_dual_listener_health
}

status() {
  local current
  local expected
  current="$(read_routing_mode)"
  case "$current" in
    relay|local-relay) require_layout "$current" ;;
    legacy)
      if [[ "$(read_remote_mode)" == "loopback" ]]; then
        expected=local-relay
      else
        expected=relay
      fi
      require_layout "$expected"
      ;;
    *) die "unsupported routing mode: $current" ;;
  esac
  printf 'routing_mode=%s\n' "$current"
  "$RELAYCTL" status --config "$RELAY_CONFIG" --codex-config "$CODEX_CONFIG"
  "$MANAGER" status
}

enable_relay() {
  local migrate_legacy="$1"
  # Existing external relays may intentionally have no bounded model policy.
  # Do not turn the local normalizer's exact-model gate into an external
  # compatibility requirement.
  require_layout relay
  if relay_default_policy_enabled; then
    require_default_bounded_root_model
  fi
  begin_routing_transaction
  if [[ "$migrate_legacy" == true ]]; then
    "$RELAYCTL" migrate-legacy --codex-config "$CODEX_CONFIG"
  fi
  "$RELAYCTL" enable --config "$RELAY_CONFIG" --codex-config "$CODEX_CONFIG"
  write_routing_mode relay
  "$MANAGER" verify-default-model
  "$MANAGER" ensure-interactive-profile
  "$MANAGER" restart-daemon
  systemctl --user enable --now opencodex-remote-relay-catalog-activation.timer
  systemctl --user disable --now opencodex-remote-catalog-refresh.timer
  commit_routing_transaction
  printf 'remote_codex_routing=relay\n'
}

enable_local_relay() {
  local migrate_legacy="$1"
  verify_video_bridge_disabled
  require_layout local-relay
  begin_routing_transaction
  if [[ "$migrate_legacy" == true ]]; then
    "$RELAYCTL" migrate-legacy --codex-config "$CODEX_CONFIG"
  fi
  "$RELAYCTL" enable --config "$RELAY_CONFIG" --codex-config "$CODEX_CONFIG"
  write_routing_mode local-relay
  "$MANAGER" verify-default-model
  "$MANAGER" ensure-interactive-profile
  "$MANAGER" restart-daemon
  systemctl --user enable --now opencodex-remote-catalog-refresh.timer
  systemctl --user disable --now opencodex-remote-relay-catalog-activation.timer
  commit_routing_transaction
  printf 'remote_codex_routing=local-relay\n'
}

action="${1:-}"
case "$action" in
  status)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    status
    ;;
  verify-video-bridge-disabled)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    verify_video_bridge_disabled
    ;;
  enable-relay)
    [[ "$#" -ge 2 && "$#" -le 3 ]] || { usage >&2; exit 2; }
    [[ "${2:-}" == "--allow-remote-interruption" ]] || { usage >&2; exit 2; }
    migrate_legacy=false
    if [[ "$#" -eq 3 ]]; then
      [[ "${3:-}" == "--migrate-legacy" ]] || { usage >&2; exit 2; }
      migrate_legacy=true
    fi
    enable_relay "$migrate_legacy"
    ;;
  enable-local-relay)
    [[ "$#" -ge 2 && "$#" -le 3 ]] || { usage >&2; exit 2; }
    [[ "${2:-}" == "--allow-remote-interruption" ]] || { usage >&2; exit 2; }
    migrate_legacy=false
    if [[ "$#" -eq 3 ]]; then
      [[ "${3:-}" == "--migrate-legacy" ]] || { usage >&2; exit 2; }
      migrate_legacy=true
    fi
    enable_local_relay "$migrate_legacy"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

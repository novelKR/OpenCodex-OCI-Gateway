#!/usr/bin/env bash
# Maintain the Remote-only Codex home without exposing data-plane credentials.

set -euo pipefail

readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"
readonly CONFIG_FILE="${CONFIG_DIR}/remote-opencodex.json"
readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"
readonly CREDENTIAL_FILE="${CONFIG_DIR}/credentials.env"
readonly MANAGED_CODEX="${REMOTE_HOME_PATH}/packages/standalone/current/codex"
readonly REMOTE_CODEX_CONFIG="${REMOTE_HOME_PATH}/config.toml"
readonly CATALOG="${REMOTE_HOME_PATH}/opencodex-catalog.json"
readonly RESTART_PENDING="${REMOTE_HOME_PATH}/catalog-restart-pending"
readonly RELAY_RESTART_PENDING="${CATALOG}.restart-pending"
readonly INSTALL_ROOT="/home/ubuntu/.local/lib/opencodex-relay"
readonly WRAPPER_SOURCE="${INSTALL_ROOT}/codex-remote-home-wrapper.sh"
readonly WRAPPER_TARGET="/home/ubuntu/.local/bin/codex"
readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"
readonly RELAY_CONFIG="${CONFIG_DIR}/relay.json"
readonly INTERACTIVE_PROFILE="${REMOTE_HOME_PATH}/opencodex-relay-interactive.config.toml"
readonly INTERACTIVE_PROFILE_MARKER="# opencodex-relay-managed-interactive-profile-v1"
readonly DEFAULT_CODEX_MODEL="gpt-5.6-luna"
readonly DEFAULT_BOUNDED_CODEX_MODEL="opencode-go-responses/gpt-5.6-luna"

usage() {
  cat <<'USAGE'
Usage:
  manage-remote-codex-home.sh refresh [--restart]
  manage-remote-codex-home.sh apply-relay-catalog
  manage-remote-codex-home.sh status
  manage-remote-codex-home.sh bootstrap-remote-control
  manage-remote-codex-home.sh repair-wrapper
  manage-remote-codex-home.sh restart-daemon
  manage-remote-codex-home.sh verify-daemon
  manage-remote-codex-home.sh recover-daemon --allow-remote-interruption
  manage-remote-codex-home.sh isolate-home-project-config --allow-remote-interruption
  manage-remote-codex-home.sh verify-home-project-config
  manage-remote-codex-home.sh set-default-model --allow-remote-interruption
  manage-remote-codex-home.sh verify-default-model
  manage-remote-codex-home.sh check-interactive-profile-ownership
  manage-remote-codex-home.sh ensure-interactive-profile
  manage-remote-codex-home.sh verify-interactive-profile
  manage-remote-codex-home.sh verify-relay-health

In legacy and local-relay modes refresh writes a verified catalog atomically.
In local-relay mode the Remote manager remains the sole catalog writer and
activator while Native Codex traffic passes through the local relay. In external
relay mode the relay is the only catalog writer and refresh never applies its
pending marker, including when --restart is supplied. apply-relay-catalog is
the sole manager action that checks that external-relay marker and activates an
idle Remote AppServer without fetching or rewriting the catalog.

restart-daemon clears restart-pending only after the running AppServer reports
the same version as the managed standalone Codex binary. verify-daemon checks
that invariant without changing state.

recover-daemon is an explicit takeover path for an older AppServer which uses
the approved Remote home but is no longer owned by codex app-server daemon. It
only terminates the exact daemon pid-update loop and Unix-socket AppServer
command shapes, then bootstraps the current managed daemon. A requested
restart may use the same narrow fallback only after normal Codex restart
rejects that exact approved pair.

isolate-home-project-config explicitly marks /home/ubuntu as untrusted in the
dedicated Remote config before restarting the managed daemon. This prevents an
ordinary /home/ubuntu/.codex/config.toml from overriding the dedicated Remote
home when Codex is invoked from the SSH login directory.

Without the managed bounded_json relay policy, set-default-model atomically sets
only the dedicated Remote root model to gpt-5.6-luna and restarts the managed
daemon when the value changed. Local-relay preserves an existing root when it
either matches one exact bounded_json policy or appears exactly once in the
materialized catalog; the latter remains byte-exact passthrough. External relay
selects opencode-go-responses/gpt-5.6-luna only when that exact managed policy is
enabled.
Cursor models are absent from the current centrally distributed catalog because
that adapter was removed. If the central catalog later reintroduces one, it is
still never selected as the managed default.

opencodex-relay-interactive.config.toml is a marker-owned, atomic side-session
profile. It sets only openai_base_url for the reserved interactive listener and the
same model_catalog_json used by the general 18180 route. Existing unmarked
same-name files are never overwritten. Selection remains explicit with
`codex --profile opencodex-relay-interactive`.
USAGE
}

require_owned_file() {
  local path="$1"
  local expected_mode="$2"
  [[ -f "$path" && ! -L "$path" ]] || {
    printf 'ERROR: required regular file is unavailable: %s\n' "$path" >&2
    exit 2
  }
  [[ "$(stat -c '%U:%G:%a' "$path")" == "ubuntu:ubuntu:${expected_mode}" ]] || {
    printf 'ERROR: %s must be owned by ubuntu with mode %s.\n' "$path" "$expected_mode" >&2
    exit 2
  }
}

routing_uses_relay() {
  [[ "$ROUTING_MODE" == "relay" || "$ROUTING_MODE" == "local-relay" ]]
}

clear_edge_credentials() {
  unset CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY
}

require_configuration() {
  [[ "$(id -un)" == "ubuntu" ]] || {
    printf 'ERROR: run this script as ubuntu.\n' >&2
    exit 2
  }
  require_owned_file "$CONFIG_FILE" 600
  [[ -f "$CONFIG_LOADER" && ! -L "$CONFIG_LOADER" ]] || {
    printf 'ERROR: Remote configuration loader is unavailable: %s\n' "$CONFIG_LOADER" >&2
    exit 2
  }
  # shellcheck source=/dev/null
  . "$CONFIG_LOADER"
  load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"
  [[ "${REMOTE_HOME:-}" == "$REMOTE_HOME_PATH" ]] || {
    printf 'ERROR: REMOTE_HOME in %s is not the approved remote home.\n' "$CONFIG_FILE" >&2
    exit 2
  }
  case "${MODE:-}" in
    loopback|external) ;;
    *)
      printf 'ERROR: MODE in %s must be loopback or external.\n' "$CONFIG_FILE" >&2
      exit 2
      ;;
  esac
  ROUTING_MODE="${ROUTING_MODE:-legacy}"
  case "${ROUTING_MODE}" in
    legacy|relay|local-relay) ;;
    *)
      printf 'ERROR: ROUTING_MODE in %s must be legacy, relay, or local-relay.\n' "$CONFIG_FILE" >&2
      exit 2
      ;;
  esac
  if [[ "${ROUTING_MODE}" == "relay" && "${MODE}" != "external" ]]; then
    printf 'ERROR: ROUTING_MODE=relay is supported only with MODE=external.\n' >&2
    exit 2
  fi
  if [[ "${ROUTING_MODE}" == "local-relay" && "${MODE}" != "loopback" ]]; then
    printf 'ERROR: ROUTING_MODE=local-relay is supported only with MODE=loopback.\n' >&2
    exit 2
  fi
  if [[ "$MODE" == "loopback" ]] || routing_uses_relay; then
    # Daemon lifecycle commands invoke the managed Codex binary directly, not
    # only through the user-facing wrapper. Local paths and both relay modes
    # may not leak edge admission credentials into those native processes.
    clear_edge_credentials
  fi
  [[ -x "$MANAGED_CODEX" ]] || {
    printf 'ERROR: managed standalone Codex is missing: %s\n' "$MANAGED_CODEX" >&2
    exit 2
  }
  export CODEX_HOME="$REMOTE_HOME_PATH"
}

managed_interactive_profile_shape() {
  awk -v marker="$INTERACTIVE_PROFILE_MARKER" '
    NR == 1 { if ($0 != marker) exit 1; next }
    NR == 2 { if ($0 !~ /^openai_base_url = ".*"$/) exit 1; next }
    NR == 3 { if ($0 !~ /^model_catalog_json = ".*"$/) exit 1; next }
    { exit 1 }
    END { if (NR != 3) exit 1 }
  ' "$INTERACTIVE_PROFILE"
}

check_interactive_profile_ownership() {
  require_configuration
  if [[ ! -e "$INTERACTIVE_PROFILE" && ! -L "$INTERACTIVE_PROFILE" ]]; then
    printf 'interactive_profile_state=absent\n'
    return 0
  fi
  require_owned_file "$INTERACTIVE_PROFILE" 600
  managed_interactive_profile_shape || {
    printf 'ERROR: existing %s is not owned by opencodex-relay; move it aside or merge it manually.\n' \
      "$INTERACTIVE_PROFILE" >&2
    return 1
  }
  printf 'interactive_profile_state=managed\n'
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

listener_http_url() {
  printf 'http://%s\n' "$1"
}

render_interactive_profile() {
  local destination="$1"
  local listeners
  local general_listener
  local interactive_listener
  local interactive_url
  local catalog
  local encoded_url
  local encoded_catalog
  listeners="$(relay_listeners)" || return 1
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  [[ -n "$general_listener" ]] || return 1
  interactive_url="$(listener_http_url "$interactive_listener")/v1"
  catalog="$(jq -er '.catalog.path | select(type == "string" and length > 0)' "$RELAY_CONFIG")" || return 1
  encoded_url="$(jq -Rn --arg value "$interactive_url" '$value')"
  encoded_catalog="$(jq -Rn --arg value "$catalog" '$value')"
  {
    printf '%s\n' "$INTERACTIVE_PROFILE_MARKER"
    printf 'openai_base_url = %s\n' "$encoded_url"
    printf 'model_catalog_json = %s\n' "$encoded_catalog"
  } > "$destination"
}

ensure_interactive_profile() {
  local candidate
  require_configuration
  routing_uses_relay || {
    printf 'ERROR: the interactive relay profile is valid only in relay or local-relay mode.\n' >&2
    return 1
  }
  require_owned_file "$RELAY_CONFIG" 600
  check_interactive_profile_ownership >/dev/null
  candidate="$(mktemp "${INTERACTIVE_PROFILE}.XXXXXX")"
  if ! render_interactive_profile "$candidate"; then
    rm -f -- "$candidate"
    printf 'ERROR: relay configuration has no supported distinct interactive listener or catalog path.\n' >&2
    return 1
  fi
  chmod 0600 "$candidate"
  if ! check_interactive_profile_ownership >/dev/null; then
    rm -f -- "$candidate"
    return 1
  fi
  if [[ -f "$INTERACTIVE_PROFILE" ]] && cmp -s "$candidate" "$INTERACTIVE_PROFILE"; then
    rm -f -- "$candidate"
    printf 'interactive_profile_changed=0 path=%s\n' "$INTERACTIVE_PROFILE"
    return 0
  fi
  if ! mv -f -- "$candidate" "$INTERACTIVE_PROFILE"; then
    rm -f -- "$candidate"
    printf 'ERROR: unable to atomically publish the managed interactive profile.\n' >&2
    return 1
  fi
  printf 'interactive_profile_changed=1 path=%s\n' "$INTERACTIVE_PROFILE"
}

verify_interactive_profile() {
  local expected
  require_configuration
  routing_uses_relay || {
    printf 'ERROR: the interactive relay profile is valid only in relay or local-relay mode.\n' >&2
    return 1
  }
  require_owned_file "$RELAY_CONFIG" 600
  check_interactive_profile_ownership >/dev/null
  [[ -f "$INTERACTIVE_PROFILE" ]] || {
    printf 'ERROR: managed interactive profile is missing: %s\n' "$INTERACTIVE_PROFILE" >&2
    return 1
  }
  expected="$(mktemp "${REMOTE_HOME_PATH}/.interactive-profile-verify.XXXXXX")"
  if ! render_interactive_profile "$expected"; then
    rm -f -- "$expected"
    printf 'ERROR: relay configuration has no supported distinct interactive listener or catalog path.\n' >&2
    return 1
  fi
  if ! cmp -s "$expected" "$INTERACTIVE_PROFILE"; then
    rm -f -- "$expected"
    printf 'ERROR: managed interactive profile does not match the reviewed relay configuration.\n' >&2
    return 1
  fi
  rm -f -- "$expected"
  printf 'interactive_profile_match=1 path=%s\n' "$INTERACTIVE_PROFILE"
}

home_project_trust() {
  awk '
    /^\[projects\."\/home\/ubuntu"\][[:space:]]*$/ { in_target = 1; next }
    /^\[/ { in_target = 0 }
    in_target && /^[[:space:]]*trust_level[[:space:]]*=/ {
      value = $0
      sub(/^[[:space:]]*trust_level[[:space:]]*=[[:space:]]*"?/, "", value)
      sub(/"?[[:space:]]*(#.*)?$/, "", value)
      print value
      exit
    }
  ' "$REMOTE_CODEX_CONFIG"
}

remote_default_model() {
  awk '
    BEGIN { in_root = 1 }
    /^[[:space:]]*\[/ { in_root = 0 }
    in_root && /^[[:space:]]*model[[:space:]]*=/ {
      count++
      value = $0
      sub(/^[[:space:]]*model[[:space:]]*=[[:space:]]*/, "", value)
      if (value !~ /^"[^"]+"[[:space:]]*(#.*)?$/) exit 4
      sub(/^"/, "", value)
      sub(/"[[:space:]]*(#.*)?$/, "", value)
      result = value
    }
    END {
      if (count > 1) exit 3
      if (count == 1) print result
    }
  ' "$REMOTE_CODEX_CONFIG" || {
    printf 'ERROR: Remote config must contain at most one simple quoted root model assignment.\n' >&2
    return 1
  }
}

expected_default_model() {
  local policy_state

  if [[ "$ROUTING_MODE" == "local-relay" ]]; then
    local_relay_selected_model || return 1
    return 0
  fi
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    policy_state="$(relay_default_policy_state)" || return 1
    if [[ "$policy_state" == "enabled" ]]; then
      printf '%s\n' "$DEFAULT_BOUNDED_CODEX_MODEL"
      return 0
    fi
  fi
  printf '%s\n' "$DEFAULT_CODEX_MODEL"
}

relay_default_policy_state() {
  require_owned_file "$RELAY_CONFIG" 600
  jq -er --arg model "$DEFAULT_BOUNDED_CODEX_MODEL" '
    (.responses.model_modes // {}) as $modes
    | if ($modes | type) != "object" then
        error("responses.model_modes must be an object")
      else ($modes | to_entries) as $entries
      | if any($entries[];
          (.key | length) == 0
          or (.key | gsub("^\\s+|\\s+$"; "")) != .key
          or .value != "bounded_json") then
          error("responses.model_modes contains an invalid key or value")
        elif ([$entries[] | .key | ascii_downcase] | unique | length) != ($entries | length) then
          error("responses.model_modes contains case-insensitive duplicate keys")
        elif ([$entries[]
        | select((.key | ascii_downcase) == ($model | ascii_downcase)
          and .value == "bounded_json")] | length) == 1 then
          "enabled"
        else
          "disabled"
        end
      end
  ' "$RELAY_CONFIG" || {
    printf 'ERROR: relay policy configuration is invalid: %s.\n' "$RELAY_CONFIG" >&2
    return 1
  }
}

local_relay_model_state() {
  local catalog_matches
  local current
  local policy_matches

  if [[ "$ROUTING_MODE" != "local-relay" ]]; then
    jq -cn --arg model "$DEFAULT_CODEX_MODEL" --arg relay_mode "passthrough" \
      '{model: $model, relay_mode: $relay_mode}'
    return 0
  fi
  require_owned_file "$RELAY_CONFIG" 600
  current="$(remote_default_model)" || return 1
  jq -en --arg model "$current" '
    ($model | length) > 0
    and (($model | gsub("^\\s+|\\s+$"; "")) == $model)
  ' >/dev/null || {
    printf 'ERROR: local-relay requires a non-empty root model without surrounding whitespace; found %s.\n' \
      "${current:-unset}" >&2
    return 1
  }
  require_owned_file "$CATALOG" 600
  catalog_matches="$(
    jq -ers --arg model "$current" '
      def valid_model_identifier:
        type == "string"
        and length > 0
        and (gsub("^\\s+|\\s+$"; "") == .);
      if length != 1 then
        error("catalog must contain exactly one JSON value")
      else .[0]
      end
      | .models as $entries
      | if ($entries | type) != "array" then
          error("models is not an array")
        elif ($entries | length) == 0 then
          error("models is empty")
        elif (all($entries[];
          ((.slug // .id // "") | valid_model_identifier)) | not) then
          error("model identifier is missing or has surrounding whitespace")
        else ($entries | map(.slug // .id)) as $ids
        | if ($ids | unique | length) != ($ids | length) then
            error("duplicate model identifier")
          else
            [$ids[] | select(. == $model)] | length
          end
        end
    ' "$CATALOG"
  )" || {
    printf 'ERROR: local-relay catalog is invalid: %s.\n' "$CATALOG" >&2
    return 1
  }

  policy_matches="$(
    jq -ers --arg model "$current" '
      if length != 1 then
        error("relay config must contain exactly one JSON value")
      else .[0]
      end
      | (.responses.model_modes // {}) as $modes
      | if ($modes | type) != "object" then
          error("responses.model_modes must be an object")
        else ($modes | to_entries) as $entries
        | if ($entries | length) == 0 then
            error("responses.model_modes must not be empty")
          elif any($entries[];
            (.key | length) == 0
            or (.key | gsub("^\\s+|\\s+$"; "")) != .key
            or .value != "bounded_json") then
            error("responses.model_modes contains an invalid key or value")
          elif ([$entries[] | .key | ascii_downcase] | unique | length) != ($entries | length) then
            error("responses.model_modes contains case-insensitive duplicate keys")
          else
            [$entries[]
              | select((.key | ascii_downcase) == ($model | ascii_downcase)
                and .value == "bounded_json")]
            | length
          end
        end
    ' "$RELAY_CONFIG"
  )" || {
    printf 'ERROR: local-relay bounded_json policy is invalid: %s.\n' "$RELAY_CONFIG" >&2
    return 1
  }
  if [[ "$policy_matches" == "1" ]]; then
    jq -cn --arg model "$current" --arg relay_mode "bounded_json" \
      '{model: $model, relay_mode: $relay_mode}'
    return 0
  fi
  [[ "$catalog_matches" == "1" ]] || {
    printf 'ERROR: local-relay root model %s is neither one bounded_json policy nor one exact catalog model.\n' \
      "$current" >&2
    return 1
  }
  jq -cn --arg model "$current" --arg relay_mode "passthrough" \
    '{model: $model, relay_mode: $relay_mode}'
}

local_relay_selected_model() {
  local state
  state="$(local_relay_model_state)" || return 1
  jq -er '.model | select(type == "string" and length > 0)' <<< "$state"
}

verify_default_model() {
  local current
  local expected
  require_configuration
  require_owned_file "$REMOTE_CODEX_CONFIG" 600
  current="$(remote_default_model)" || return 1
  expected="$(expected_default_model)" || return 1
  [[ "$current" == "$expected" ]] || {
    printf 'ERROR: Remote default model is %s; expected %s.\n' \
      "${current:-unset}" "$expected" >&2
    return 1
  }
  printf 'default_model_match=1 model=%s\n' "$expected"
}

set_default_model() {
  local backup
  local candidate
  local current
  local expected
  local stamp

  require_configuration
  require_owned_file "$REMOTE_CODEX_CONFIG" 600
  current="$(remote_default_model)" || return 1
  expected="$(expected_default_model)" || return 1
  if [[ "$current" == "$expected" ]]; then
    printf 'default_model_changed=0 model=%s\n' "$expected"
    return 0
  fi

  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  backup="${REMOTE_CODEX_CONFIG}.pre-default-model-${stamp}"
  cp -p "$REMOTE_CODEX_CONFIG" "$backup"
  chmod 0600 "$backup"
  candidate="$(mktemp "${REMOTE_CODEX_CONFIG}.XXXXXX")"
  if ! awk -v desired="$expected" '
    function write_default() {
      if (!written) print "model = \"" desired "\""
      written = 1
    }
    BEGIN { in_root = 1 }
    in_root && /^[[:space:]]*model[[:space:]]*=/ {
      model_count++
      write_default()
      next
    }
    in_root && /^[[:space:]]*\[/ {
      write_default()
      in_root = 0
      print
      next
    }
    { print }
    END {
      if (in_root) write_default()
      if (model_count > 1) exit 3
    }
  ' "$REMOTE_CODEX_CONFIG" > "$candidate"; then
    rm -f "$candidate"
    printf 'ERROR: Remote config has duplicate root model assignments; refusing to rewrite it.\n' >&2
    return 2
  fi
  chmod 0600 "$candidate"
  mv -f "$candidate" "$REMOTE_CODEX_CONFIG"
  printf 'default_model_backup=%s\n' "$backup"
  restart_daemon
  verify_default_model
  printf 'default_model_changed=1 model=%s\n' "$expected"
}

isolate_home_project_config() {
  local current_trust
  local candidate
  local backup
  local stamp

  require_configuration
  require_owned_file "$REMOTE_CODEX_CONFIG" 600
  current_trust="$(home_project_trust)"
  if [[ "$current_trust" == "untrusted" ]]; then
    printf 'home_project_config_isolated=0\n'
    return 0
  fi
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  backup="${REMOTE_CODEX_CONFIG}.pre-home-project-isolation-${stamp}"
  cp -p "$REMOTE_CODEX_CONFIG" "$backup"
  chmod 0600 "$backup"
  candidate="$(mktemp "${REMOTE_CODEX_CONFIG}.XXXXXX")"
  awk '
    function finish_target() {
      if (in_target && !wrote_trust) print "trust_level = \"untrusted\""
      in_target = 0
    }
    /^\[projects\."\/home\/ubuntu"\][[:space:]]*$/ {
      finish_target()
      target_sections++
      in_target = 1
      print
      next
    }
    /^\[/ {
      finish_target()
      print
      next
    }
    in_target && /^[[:space:]]*trust_level[[:space:]]*=/ {
      if (!wrote_trust) print "trust_level = \"untrusted\""
      wrote_trust = 1
      next
    }
    { print }
    END {
      finish_target()
      if (target_sections == 0) {
        print ""
        print "[projects.\"/home/ubuntu\"]"
        print "trust_level = \"untrusted\""
      }
      if (target_sections > 1) exit 3
    }
  ' "$REMOTE_CODEX_CONFIG" > "$candidate" || {
    rm -f "$candidate"
    printf 'ERROR: Remote config has duplicate /home/ubuntu project sections; refusing to rewrite it.\n' >&2
    return 2
  }
  chmod 0600 "$candidate"
  mv -f "$candidate" "$REMOTE_CODEX_CONFIG"
  printf 'home_project_config_backup=%s\n' "$backup"
  restart_daemon
  [[ "$(home_project_trust)" == "untrusted" ]] || {
    printf 'ERROR: failed to isolate /home/ubuntu project configuration.\n' >&2
    return 1
  }
  printf 'home_project_config_isolated=1\n'
}

repair_wrapper() {
  local candidate
  local backup
  local stamp

  require_configuration
  [[ -f "$WRAPPER_SOURCE" && ! -L "$WRAPPER_SOURCE" ]] || {
    printf 'ERROR: wrapper source is unavailable: %s\n' "$WRAPPER_SOURCE" >&2
    exit 2
  }
  [[ "$(stat -c '%U:%G:%a' "$WRAPPER_SOURCE")" == "ubuntu:ubuntu:700" ]] || {
    printf 'ERROR: %s must be owned by ubuntu with mode 0700.\n' "$WRAPPER_SOURCE" >&2
    exit 2
  }
  if [[ -f "$WRAPPER_TARGET" && ! -L "$WRAPPER_TARGET" ]] && cmp -s "$WRAPPER_SOURCE" "$WRAPPER_TARGET"; then
    printf 'wrapper_repaired=0\n'
    return 0
  fi

  if [[ -e "$WRAPPER_TARGET" || -L "$WRAPPER_TARGET" ]]; then
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    backup="${WRAPPER_TARGET}.pre-managed-repair-${stamp}"
    cp -a "$WRAPPER_TARGET" "$backup"
    printf 'wrapper_backup=%s\n' "$backup"
  fi
  candidate="$(mktemp "${WRAPPER_TARGET}.XXXXXX")"
  install -m 0700 "$WRAPPER_SOURCE" "$candidate"
  mv -f "$candidate" "$WRAPPER_TARGET"
  chmod 0700 "$WRAPPER_TARGET"
  printf 'wrapper_repaired=1\n'
}

load_external_credentials() {
  [[ "$MODE" == "external" && "$ROUTING_MODE" == "legacy" ]] || return 0
  [[ -d "$CONFIG_DIR" && ! -L "$CONFIG_DIR" ]] || {
    printf 'ERROR: credential directory is unavailable: %s\n' "$CONFIG_DIR" >&2
    exit 2
  }
  [[ "$(stat -c '%U:%G:%a' "$CONFIG_DIR")" == "ubuntu:ubuntu:700" ]] || {
    printf 'ERROR: %s must be owned by ubuntu with mode 0700.\n' "$CONFIG_DIR" >&2
    exit 2
  }
  require_owned_file "$CREDENTIAL_FILE" 600
  unset CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY
  local line name value
  local seen_client_id=false
  local seen_client_secret=false
  local seen_gateway_key=false
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || {
      printf 'ERROR: credentials.env contains a malformed line.\n' >&2
      exit 2
    }
    name="${line%%=*}"
    value="${line#*=}"
    if [[ "${value}" == \"*\" || "${value}" == \'*\' ]]; then
      [[ "${#value}" -ge 2 && "${value}" == "${value:0:1}"*"${value:0:1}" ]] || {
        printf 'ERROR: credentials.env contains an unterminated quoted value.\n' >&2
        exit 2
      }
      value="${value:1:${#value}-2}"
    fi
    [[ -n "${value}" ]] || {
      printf 'ERROR: credentials.env contains an empty credential.\n' >&2
      exit 2
    }
    case "${name}" in
      CF_ACCESS_CLIENT_ID)
        [[ "${seen_client_id}" == false ]] || { printf 'ERROR: credentials.env duplicates CF_ACCESS_CLIENT_ID.\n' >&2; exit 2; }
        CF_ACCESS_CLIENT_ID="${value}"; seen_client_id=true ;;
      CF_ACCESS_CLIENT_SECRET)
        [[ "${seen_client_secret}" == false ]] || { printf 'ERROR: credentials.env duplicates CF_ACCESS_CLIENT_SECRET.\n' >&2; exit 2; }
        CF_ACCESS_CLIENT_SECRET="${value}"; seen_client_secret=true ;;
      OPENCODEX_GATEWAY_API_KEY)
        [[ "${seen_gateway_key}" == false ]] || { printf 'ERROR: credentials.env duplicates OPENCODEX_GATEWAY_API_KEY.\n' >&2; exit 2; }
        OPENCODEX_GATEWAY_API_KEY="${value}"; seen_gateway_key=true ;;
      *)
        printf 'ERROR: credentials.env contains an unsupported variable.\n' >&2
        exit 2
        ;;
    esac
  done < "$CREDENTIAL_FILE"
  for variable in CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY; do
    [[ -n "${!variable:-}" ]] || {
      printf 'ERROR: %s is required for external OpenCodex routing.\n' "$variable" >&2
      exit 2
    }
  done
  export CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY
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

dual_relay_health() {
  local listeners
  local general_listener
  local interactive_listener
  local general_health
  local interactive_health
  listeners="$(relay_listeners)" || return 1
  IFS=$'\t' read -r general_listener interactive_listener <<< "$listeners"
  general_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "$(listener_http_url "$general_listener")/__relay/healthz")" || return 1
  interactive_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 3 \
    "$(listener_http_url "$interactive_listener")/__relay/healthz")" || return 1
  health_matches_listener_lane "$general_health" general "$general_listener" "$interactive_listener" || return 1
  health_matches_listener_lane "$interactive_health" interactive "$general_listener" "$interactive_listener" || return 1
  printf '%s\n' "$general_health"
}

require_relay() {
  local general_health
  [[ -f "$RELAY_CONFIG" && ! -L "$RELAY_CONFIG" && \
      "$(stat -c '%U:%G:%a' "$RELAY_CONFIG")" == "ubuntu:ubuntu:600" ]] || {
    printf 'ERROR: relay configuration is unavailable: %s\n' "$RELAY_CONFIG" >&2
    exit 2
  }
  [[ -x "$RELAY" ]] || {
    printf 'ERROR: installed relay is unavailable: %s\n' "$RELAY" >&2
    exit 2
  }
  "$RELAY" --config "$RELAY_CONFIG" --check >/dev/null || {
    printf 'ERROR: the installed relay rejected its configuration or credential access.\n' >&2
    exit 2
  }
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    jq -e --arg catalog "$CATALOG" '
      def go_empty_default($fallback):
        if . == null or . == "" then $fallback else . end;
      (.upstream_mode | go_empty_default("external_gateway")) == "external_gateway"
      and (.catalog.owner | go_empty_default("relay")) == "relay"
      and .catalog.path == $catalog
      and .catalog.manage_app_server == false
    ' "$RELAY_CONFIG" >/dev/null || {
      printf 'ERROR: external relay configuration does not match the managed Remote home.\n' >&2
      exit 2
    }
  else
    local_relay_model_state >/dev/null || exit 2
    jq -e --arg catalog "$CATALOG" '
      .upstream_mode == "local_opencodex"
      and (.upstream_base_url == "http://127.0.0.1:10100/v1"
        or .upstream_base_url == "http://[::1]:10100/v1")
      and .credentials.source == "none"
      and .responses.websocket_mode == "http_fallback"
      and ((.responses.model_modes // {}) | length) > 0
      and .catalog.owner == "remote_manager"
      and .catalog.path == $catalog
      and .catalog.manage_app_server == false
    ' "$RELAY_CONFIG" >/dev/null || {
      printf 'ERROR: local relay configuration does not match the managed Remote home.\n' >&2
      exit 2
    }
  fi
  verify_interactive_profile >/dev/null || exit 2
  general_health="$(dual_relay_health)" || {
    printf 'ERROR: the relay general/interactive listeners do not match the reviewed health contract.\n' >&2
    exit 2
  }
}

relay_active_requests() {
  local health
  health="$(dual_relay_health)"
  jq -er '
    select(.ok == true)
    | .active_requests
    | if type == "number" and floor == . and . >= 0 then . else error("relay active_requests is invalid") end
  ' <<< "$health"
}

catalog_restart_pending() {
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    [[ -f "$RESTART_PENDING" || -f "$RELAY_RESTART_PENDING" ]]
    return
  fi
  [[ -f "$RESTART_PENDING" ]]
}

clear_catalog_restart_pending() {
  rm -f "$RESTART_PENDING"
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    rm -f "$RELAY_RESTART_PENDING"
  fi
}

apply_relay_catalog() {
  local active_requests

  require_configuration
  if [[ "$ROUTING_MODE" != "relay" ]]; then
    printf 'relay_catalog_activation=not_applicable\n'
    return 0
  fi
  repair_wrapper
  require_relay
  if [[ ! -f "$RELAY_RESTART_PENDING" ]]; then
    printf 'relay_catalog_activation=pending=0\n'
    return 0
  fi
  active_requests="$(relay_active_requests)"
  if ((active_requests > 0)); then
    printf 'relay_catalog_activation=pending=1 active_requests=%s\n' "$active_requests"
    return 0
  fi
  restart_daemon
  printf 'relay_catalog_activation=applied\n'
}

codex_version() {
  "$MANAGED_CODEX" --version | awk '{print $NF}'
}

daemon_version_json() {
  "$MANAGED_CODEX" app-server daemon version
}

daemon_version_matches_managed() {
  local expected="$1"
  local output="$2"
  jq -e --arg expected "$expected" '
    .status == "running"
    and .managedCodexVersion == $expected
    and .cliVersion == $expected
    and .appServerVersion == $expected
  ' <<< "$output" >/dev/null
}

wait_for_daemon_version() {
  local expected
  local output=""
  local attempt
  expected="$(codex_version)"
  [[ "$expected" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || {
    printf 'ERROR: managed Codex version is not explicit semver: %s\n' "$expected" >&2
    return 2
  }
  for attempt in {1..20}; do
    if output="$(daemon_version_json 2>/dev/null)" && daemon_version_matches_managed "$expected" "$output"; then
      printf 'daemon_version_match=1 version=%s\n' "$expected"
      return 0
    fi
    sleep 1
  done
  printf 'ERROR: managed Codex %s did not replace the running AppServer within 20 seconds.\n' "$expected" >&2
  if [[ -n "$output" ]]; then
    jq -c '{status, managedCodexVersion, cliVersion, appServerVersion}' <<< "$output" >&2 || true
  fi
  return 1
}

foreign_daemon_pids() {
  local proc
  local pid
  local command_line
  local process_home
  local owner
  local current_uid
  current_uid="$(id -u)"
  for proc in /proc/[0-9]*; do
    pid="${proc##*/}"
    owner="$(/usr/bin/stat -c '%u' "$proc" 2>/dev/null || true)"
    [[ "$owner" == "$current_uid" ]] || continue
    command_line="$(/usr/bin/tr '\0' ' ' < "${proc}/cmdline" 2>/dev/null || true)"
    case "$command_line" in
      "${MANAGED_CODEX} app-server daemon pid-update-loop "*|"${MANAGED_CODEX} app-server daemon pid-update-loop") ;;
      "${MANAGED_CODEX} -c features.code_mode_host=true app-server --listen unix://"*) ;;
      *) continue ;;
    esac
    process_home="$(/usr/bin/tr '\0' '\n' < "${proc}/environ" 2>/dev/null | /usr/bin/sed -n 's/^CODEX_HOME=//p' | /usr/bin/head -n 1)"
    [[ "$process_home" == "$REMOTE_HOME_PATH" ]] || continue
    printf '%s\n' "$pid"
  done
}

recover_daemon() {
  local pid
  local remaining
  local attempt
  local -a pids=()

  require_configuration
  load_external_credentials
  if routing_uses_relay; then require_relay; fi
  mapfile -t pids < <(foreign_daemon_pids)
  ((${#pids[@]} > 0)) || {
    printf 'ERROR: no approved foreign AppServer process was found; refusing a broad process kill.\n' >&2
    return 1
  }
  for pid in "${pids[@]}"; do
    kill -TERM "$pid"
  done
  for attempt in {1..20}; do
    remaining=false
    for pid in "${pids[@]}"; do
      if kill -0 "$pid" 2>/dev/null; then
        remaining=true
      fi
    done
    [[ "$remaining" == false ]] && break
    sleep 1
  done
  if [[ "$remaining" == true ]]; then
    printf 'ERROR: approved foreign AppServer process did not exit after TERM; refusing SIGKILL.\n' >&2
    return 1
  fi
  "$MANAGED_CODEX" app-server daemon bootstrap --remote-control
  wait_for_daemon_version
  clear_catalog_restart_pending
  repair_wrapper
  printf 'daemon_recovered=1 version=%s\n' "$(codex_version)"
}

fetch_catalog() {
  local version
  local temporary
  local normalized
  local old_hash=""
  local new_hash
  local stamp
  local backup
  local http_status

  version="$(codex_version)"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    printf 'ERROR: unable to parse Codex version: %s\n' "$version" >&2
    exit 2
  }
  temporary="$(mktemp "${REMOTE_HOME_PATH}/.catalog.XXXXXX")"
  normalized="$(mktemp "${REMOTE_HOME_PATH}/.catalog.normalized.XXXXXX")"
  trap 'rm -f -- "${temporary:-}" "${normalized:-}"' EXIT

  if [[ "$MODE" == "loopback" ]]; then
    curl --fail --location --silent --show-error --max-time 60 \
      "http://127.0.0.1:10100/v1/models?client_version=${version}" \
      -o "$temporary" || return 2
  elif [[ "$ROUTING_MODE" == "relay" ]]; then
    require_relay
    curl --fail --location --silent --show-error --max-time 60 \
      "http://127.0.0.1:18180/v1/models?client_version=${version}" \
      -o "$temporary" || return 2
  else
    load_external_credentials
    http_status="$(curl --silent --show-error --max-time 60 \
      -H "CF-Access-Client-Id: ${CF_ACCESS_CLIENT_ID}" \
      -H "CF-Access-Client-Secret: ${CF_ACCESS_CLIENT_SECRET}" \
      -H "X-OpenCodex-API-Key: ${OPENCODEX_GATEWAY_API_KEY}" \
      "${API_ORIGIN}/v1/models?client_version=${version}" \
      -o "$temporary" --write-out '%{http_code}')" || return 2
    [[ "$http_status" == "200" ]] || {
      printf 'ERROR: external catalog returned HTTP %s; redirects are not allowed.\n' \
        "${http_status:-000}" >&2
      return 2
    }
  fi

  jq -e '
    (.models // .data) as $entries
    | if ($entries | type) != "array" then error("models/data is not an array") else $entries end
    | map(select(((.visibility // "") | tostring | ascii_downcase) != "hide"))
    | if length == 0 then error("no visible models") else . end
    | if all(.[]; ((.slug // .id // "") | type == "string" and length > 0)) then . else error("model identifier is missing") end
    | if ((map(.slug // .id) | length) == (map(.slug // .id) | unique | length)) then . else error("duplicate model identifier") end
    | {models: .}
  ' "$temporary" > "$normalized" || return 2
  chmod 0600 "$normalized"

  if [[ -f "$CATALOG" ]]; then
    old_hash="$(sha256sum "$CATALOG" | awk '{print $1}')"
  fi
  new_hash="$(sha256sum "$normalized" | awk '{print $1}')"
  if [[ "$old_hash" == "$new_hash" ]]; then
    printf 'catalog_changed=0 version=%s models=%s\n' \
      "$version" "$(jq '.models | length' "$CATALOG")"
    return 10
  fi

  if [[ -f "$CATALOG" ]]; then
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    backup="${CATALOG}.pre-refresh-${stamp}"
    cp -p "$CATALOG" "$backup"
    chmod 0600 "$backup"
  fi
  mv -f "$normalized" "$CATALOG"
  normalized=""
  touch "$RESTART_PENDING"
  chmod 0600 "$CATALOG" "$RESTART_PENDING"
  printf 'catalog_changed=1 version=%s models=%s restart_pending=%s\n' \
    "$version" "$(jq '.models | length' "$CATALOG")" "$RESTART_PENDING"
  return 0
}

refresh() {
  local restart=false
  case "${1:-}" in
    "") ;;
    --restart) restart=true ;;
    *)
      usage
      exit 2
      ;;
  esac

  require_configuration
  repair_wrapper
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    printf 'relay_catalog_refresh=owned_by_relay\n'
    return 0
  fi
  local fetch_status
  set +e
  fetch_catalog
  fetch_status=$?
  set -e
  case "$fetch_status" in
    0|10) ;;
    *) return "$fetch_status" ;;
  esac
  if [[ "$restart" == true ]] && catalog_restart_pending; then
    restart_daemon
  fi
}

restart_daemon() {
  local restart_output

  require_configuration
  load_external_credentials
  if routing_uses_relay; then require_relay; fi
  if ! restart_output="$("$MANAGED_CODEX" app-server daemon restart 2>&1)"; then
    # Codex 0.147 can report this exact dedicated Remote daemon as unmanaged
    # after an earlier successful restart. The caller already selected an
    # interrupting lifecycle operation, so recover only its two exact command
    # shapes instead of leaving a verified catalog restart marker stale.
    printf 'daemon_restart_fallback=approved_foreign_daemon\n' >&2
    if ! recover_daemon; then
      printf 'ERROR: daemon restart and approved ownership recovery both failed: %s\n' "$restart_output" >&2
      return 1
    fi
    return 0
  fi
  printf '%s\n' "$restart_output"
  wait_for_daemon_version
  clear_catalog_restart_pending
  repair_wrapper
  printf 'daemon_restarted=1\n'
}

status() {
  local default_model
  local expected_model
  local home_trust
  local local_relay_state=""
  local relay_mode=""
  require_configuration
  if [[ "$ROUTING_MODE" == "relay" ]]; then
    require_relay
    printf 'routing_mode=relay\n'
  elif [[ "$ROUTING_MODE" == "local-relay" ]]; then
    require_relay
    printf 'routing_mode=local-relay\n'
  else
    printf 'routing_mode=legacy\n'
  fi
  printf 'catalog_models=%s\n' "$(jq '.models | length' "$CATALOG")"
  if catalog_restart_pending; then
    printf 'restart_pending=1\n'
  else
    printf 'restart_pending=0\n'
  fi
  home_trust="$(home_project_trust)"
  printf 'home_project_trust=%s\n' "${home_trust:-unset}"
  default_model="$(remote_default_model)" || return 1
  expected_model="$(expected_default_model)" || return 1
  if [[ "$ROUTING_MODE" == "local-relay" ]]; then
    local_relay_state="$(local_relay_model_state)" || return 1
    relay_mode="$(jq -er '.relay_mode | select(. == "bounded_json" or . == "passthrough")' \
      <<< "$local_relay_state")"
  fi
  printf 'default_model=%s\n' "${default_model:-unset}"
  if [[ -n "$relay_mode" ]]; then
    printf 'default_model_relay_mode=%s\n' "$relay_mode"
  fi
  if [[ "$default_model" == "$expected_model" ]]; then
    printf 'default_model_match=1\n'
  else
    printf 'default_model_match=0 expected=%s\n' "$expected_model"
  fi
  daemon_version_json
}

bootstrap_remote_control() {
  require_configuration
  load_external_credentials
  if routing_uses_relay; then require_relay; fi
  "$MANAGED_CODEX" app-server daemon bootstrap --remote-control
  repair_wrapper
}

action="${1:-}"
case "$action" in
  refresh)
    refresh "${2:-}"
    ;;
  apply-relay-catalog)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    apply_relay_catalog
    ;;
  status)
    status
    ;;
  bootstrap-remote-control)
    bootstrap_remote_control
    ;;
  repair-wrapper)
    repair_wrapper
    ;;
  restart-daemon)
    restart_daemon
    ;;
  verify-daemon)
    require_configuration
    wait_for_daemon_version
    ;;
  recover-daemon)
    [[ "$#" -eq 2 && "${2:-}" == "--allow-remote-interruption" ]] || {
      usage
      exit 2
    }
    recover_daemon
    ;;
  isolate-home-project-config)
    [[ "$#" -eq 2 && "${2:-}" == "--allow-remote-interruption" ]] || {
      usage
      exit 2
    }
    isolate_home_project_config
    ;;
  verify-home-project-config)
    require_configuration
    require_owned_file "$REMOTE_CODEX_CONFIG" 600
    [[ "$(home_project_trust)" == "untrusted" ]] || {
      printf 'ERROR: /home/ubuntu is not untrusted in the dedicated Remote config.\n' >&2
      exit 1
    }
    printf 'home_project_config_isolated=1\n'
    ;;
  set-default-model)
    [[ "$#" -eq 2 && "${2:-}" == "--allow-remote-interruption" ]] || {
      usage
      exit 2
    }
    set_default_model
    ;;
  verify-default-model)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    verify_default_model
    ;;
  check-interactive-profile-ownership)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    check_interactive_profile_ownership
    ;;
  ensure-interactive-profile)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    ensure_interactive_profile
    ;;
  verify-interactive-profile)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    verify_interactive_profile
    ;;
  verify-relay-health)
    [[ "$#" -eq 1 ]] || { usage >&2; exit 2; }
    require_configuration
    routing_uses_relay || {
      printf 'ERROR: relay health verification is valid only in relay or local-relay mode.\n' >&2
      exit 2
    }
    require_relay
    printf 'relay_dual_listener_health=verified\n'
    ;;
  *)
    usage
    exit 2
    ;;
esac

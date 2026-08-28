#!/usr/bin/env bash
# Route every user-facing Codex invocation through the dedicated Remote home.
# This keeps the interactive CLI, SSH app-server, and Remote Control daemon on
# one provider, catalog, auth, and managed-standalone state boundary.

set -euo pipefail

readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"
readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"
readonly CONFIG_FILE="${CONFIG_DIR}/remote-opencodex.json"
readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"
readonly CREDENTIAL_FILE="${CONFIG_DIR}/credentials.env"
readonly MANAGED_CODEX="${REMOTE_HOME_PATH}/packages/standalone/current/codex"
readonly MANAGER="/home/ubuntu/.local/lib/opencodex-relay/manage-remote-codex-home.sh"
readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"
readonly RELAY_CONFIG="${CONFIG_DIR}/relay.json"
readonly RELAY_HEALTH_URL="http://127.0.0.1:18180/__relay/healthz"

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

[[ "$(id -un)" == "ubuntu" ]] || {
  printf 'ERROR: this wrapper must run as ubuntu.\n' >&2
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

[[ -x "$MANAGED_CODEX" ]] || {
  printf 'ERROR: managed standalone Codex is missing: %s\n' "$MANAGED_CODEX" >&2
  exit 2
}
[[ -x "$MANAGER" ]] || {
  printf 'ERROR: Remote Codex manager is missing: %s\n' "$MANAGER" >&2
  exit 2
}

load_external_credentials() {
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

require_relay() {
	local health
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
    jq -e '
      def go_empty_default($fallback):
        if . == null or . == "" then $fallback else . end;
      (.upstream_mode | go_empty_default("external_gateway")) == "external_gateway"
      and (.catalog.owner | go_empty_default("relay")) == "relay"
      and .catalog.manage_app_server == false
    ' "$RELAY_CONFIG" >/dev/null || {
      printf 'ERROR: external relay configuration does not match ROUTING_MODE=relay.\n' >&2
      exit 2
    }
  else
    jq -e '
      .upstream_mode == "local_opencodex"
      and (.upstream_base_url == "http://127.0.0.1:10100/v1"
        or .upstream_base_url == "http://[::1]:10100/v1")
      and .credentials.source == "none"
      and .catalog.owner == "remote_manager"
      and .catalog.manage_app_server == false
    ' "$RELAY_CONFIG" >/dev/null || {
      printf 'ERROR: local relay configuration does not match ROUTING_MODE=local-relay.\n' >&2
      exit 2
    }
  fi
  health="$(curl --fail --silent --show-error --max-time 3 "$RELAY_HEALTH_URL")" || {
    printf 'ERROR: the local opencodex-relay relay is unavailable.\n' >&2
    exit 2
  }
  jq -e --slurpfile cfg "$RELAY_CONFIG" '
    def go_empty_default($fallback):
      if . == null or . == "" then $fallback else . end;
    ($cfg[0]) as $c
    | .ok == true
    and .upstream_mode == ($c.upstream_mode | go_empty_default("external_gateway"))
    and .upstream_base_url == $c.upstream_base_url
    and .catalog_owner == ($c.catalog.owner | go_empty_default("relay"))
    and .responses_websocket_mode == ($c.responses.websocket_mode | go_empty_default("passthrough"))
    and ((.responses_models // []) | sort) == ((($c.responses.model_modes // {}) | keys) | sort)
    and .responses_normalizer == (((($c.responses.model_modes // {}) | length) > 0))
  ' <<< "$health" >/dev/null || {
    printf 'ERROR: the running relay does not match the reviewed relay configuration.\n' >&2
    exit 2
  }
  "$MANAGER" verify-relay-health >/dev/null
}

if [[ "$ROUTING_MODE" == "legacy" && "$MODE" == "external" ]]; then
  load_external_credentials
else
  # Native Codex needs edge credentials only for the historical direct external
  # route. Loopback and both relay modes must never inherit them.
  unset CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY
fi
if [[ "$ROUTING_MODE" == "relay" || "$ROUTING_MODE" == "local-relay" ]]; then
  require_relay
fi

export CODEX_HOME="$REMOTE_HOME_PATH"
exec "$MANAGED_CODEX" "$@"

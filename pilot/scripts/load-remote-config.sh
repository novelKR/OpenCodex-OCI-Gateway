#!/usr/bin/env bash
# Strictly load the generated, non-secret Remote Codex JSON contract.

load_remote_config() {
  local config_file="$1"
  local expected_remote_home="$2"
  local tuple
  local extra=""

  [[ "$expected_remote_home" == /* && "$expected_remote_home" != *..* ]] || {
    printf 'ERROR: expected Remote home is not a clean absolute path.\n' >&2
    return 2
  }
  [[ -f "$config_file" && ! -L "$config_file" ]] || {
    printf 'ERROR: Remote configuration is unavailable: %s\n' "$config_file" >&2
    return 2
  }
  command -v jq >/dev/null || {
    printf 'ERROR: jq is required to parse Remote configuration.\n' >&2
    return 2
  }

  tuple="$(
    jq -er --arg expected_remote_home "$expected_remote_home" '
      def exact_keys($expected):
        type == "object"
        and ((keys | sort) == ($expected | sort));
      select(exact_keys([
        "api_origin",
        "mode",
        "remote_home",
        "routing_mode",
        "schema_version"
      ]))
      | select(.schema_version == 1)
      | select(.remote_home == $expected_remote_home)
      | select(.mode == "loopback" or .mode == "external")
      | select(
          .routing_mode == "legacy"
          or .routing_mode == "relay"
          or .routing_mode == "local-relay"
        )
      | select(
          (.routing_mode != "relay" or .mode == "external")
          and (.routing_mode != "local-relay" or .mode == "loopback")
        )
      | select(
          .api_origin
          | type == "string"
          and test(
            "^https://[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
            + "(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$"
          )
        )
      | [
          (.schema_version | tostring),
          .remote_home,
          .mode,
          .routing_mode,
          .api_origin
        ]
      | @tsv
    ' "$config_file"
  )" || {
    printf 'ERROR: Remote configuration has an unsupported or unsafe schema: %s\n' \
      "$config_file" >&2
    return 2
  }

  IFS=$'\t' read -r REMOTE_CONFIG_SCHEMA REMOTE_HOME MODE ROUTING_MODE API_ORIGIN extra \
    <<< "$tuple"
  [[ -z "$extra" && "$REMOTE_CONFIG_SCHEMA" == "1" ]] || {
    printf 'ERROR: Remote configuration parser returned an invalid field count.\n' >&2
    return 2
  }
  export REMOTE_CONFIG_SCHEMA REMOTE_HOME MODE ROUTING_MODE API_ORIGIN
}

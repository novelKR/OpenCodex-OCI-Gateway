#!/usr/bin/env bash
# Install only the current user's relay service. Credential values never enter
# the service definition or its environment.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELAY_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly RELAY_BIN="${HOME}/.local/lib/opencodex-relay/relay/current/opencodex-relay"

usage() {
  printf '%s\n' 'Usage (macOS mutations are internal to install-relay.sh): install-service.sh install --config PATH | uninstall | stop | status | restore --was-active true|false | snapshot --directory PATH | restore-snapshot --directory PATH'
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_macos_source_lifecycle_reservation() {
  local root="${HOME}/.local/lib/opencodex-relay/relay"
  local marker="${root}/.source-install-reservation.json"
  local token="${OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION:-}" root_mode marker_mode recorded
  [[ "$token" =~ ^[0-9a-f]{64}$ ]] || \
    die 'macOS service mutation requires an active Relay source lifecycle reservation'
  [[ -d "$root" && ! -L "$root" && -f "$marker" && ! -L "$marker" ]] || \
    die 'macOS service mutation has no safe Relay source lifecycle reservation'
  root_mode="$(stat -f '%u:%Lp' "$root")" || die 'unable to inspect Relay source lifecycle root'
  marker_mode="$(stat -f '%u:%Lp' "$marker")" || die 'unable to inspect Relay source lifecycle marker'
  [[ "$root_mode" == "$(id -u):700" && "$marker_mode" == "$(id -u):600" ]] || \
    die 'Relay source lifecycle reservation ownership or mode is unsafe'
  recorded="$(jq -er '
    select(.schema_version == 1 and .scope == "production")
    | select(keys | sort == ["schema_version", "scope", "token"])
    | .token | select(type == "string" and test("^[0-9a-f]{64}$"))
  ' "$marker")" || die 'Relay source lifecycle reservation is invalid'
  [[ "$recorded" == "$token" ]] || die 'Relay source lifecycle reservation token does not match'
}

safe_path() {
  [[ "$1" != *$'\n'* && "$1" != *$'\r'* && "$1" != *'<'* && "$1" != *'>'* && \
     "$1" != *'&'* && "$1" != *'"'* && "$1" != *"'"* && "$1" != *'|'* && \
     "$1" != *'\'* && "$1" != *'%'* ]]
}

require_snapshot_directory() {
  local directory="$1"
  [[ -d "$directory" && ! -L "$directory" ]] || die "service snapshot directory is unavailable or unsafe: $directory"
}

write_snapshot_state() {
  local directory="$1"
  local artifact_present="$2"
  local active="$3"
  local enabled="${4:-}"
  umask 077
  {
    printf 'artifact_present=%s\n' "$artifact_present"
    printf 'active=%s\n' "$active"
    [[ -z "$enabled" ]] || printf 'enabled=%s\n' "$enabled"
  } > "${directory}/state"
  chmod 0600 "${directory}/state"
}

snapshot_state_value() {
  local directory="$1"
  local key="$2"
  local value
  value="$(sed -nE "s/^${key}=([A-Za-z0-9_-]+)$/\\1/p" "${directory}/state")"
  [[ "$value" =~ ^[A-Za-z0-9_-]+$ ]] || die "service snapshot has no valid ${key} value"
  printf '%s\n' "$value"
}

snapshot_service_file() {
  local service_file="$1"
  local directory="$2"
  if [[ -e "$service_file" || -L "$service_file" ]]; then
    [[ -f "$service_file" && ! -L "$service_file" ]] || die "existing relay service file is unsafe: $service_file"
    cp -p -- "$service_file" "${directory}/service-file"
    printf 'true\n'
  else
    printf 'false\n'
  fi
}

restore_service_file() {
  local service_file="$1"
  local directory="$2"
  local artifact_present="$3"
  local candidate
  case "$artifact_present" in
    true)
      [[ -f "${directory}/service-file" && ! -L "${directory}/service-file" ]] || \
        die 'service snapshot is missing its prior service file'
      mkdir -p -- "$(dirname -- "$service_file")"
      candidate="$(mktemp "${service_file}.rollback.XXXXXX")"
      cp -p -- "${directory}/service-file" "$candidate"
      mv -f -- "$candidate" "$service_file"
      ;;
    false)
      if [[ -e "$service_file" || -L "$service_file" ]]; then
        [[ -f "$service_file" && ! -L "$service_file" ]] || \
          die "new relay service file is unsafe: $service_file"
        rm -f -- "$service_file"
      fi
      ;;
    *) die 'service snapshot has an invalid artifact state' ;;
  esac
}

systemd_enabled_state() {
  local state
  state="$(systemctl --user is-enabled opencodex-relay.service 2>/dev/null || true)"
  state="${state//$'\n'/}"
  state="${state//$'\r'/}"
  case "$state" in
    enabled|enabled-runtime|disabled|not-found|static|indirect|generated|transient|masked|masked-runtime) ;;
    *) die "unable to capture a supported systemd enable state: ${state:-empty}" ;;
  esac
  printf '%s\n' "$state"
}

restore_systemd_enabled_state() {
  local state="$1"
  case "$state" in
    enabled) systemctl --user enable opencodex-relay.service ;;
    enabled-runtime) systemctl --user enable --runtime opencodex-relay.service ;;
    disabled|not-found|static|indirect|generated|transient)
      systemctl --user disable opencodex-relay.service >/dev/null 2>&1 || true
      ;;
    masked) systemctl --user mask opencodex-relay.service ;;
    masked-runtime) systemctl --user mask --runtime opencodex-relay.service ;;
    *) die 'service snapshot has an unsupported systemd enable state' ;;
  esac
}

launchd_enabled_state() {
  local uid="$1"
  local label="$2"
  local disabled
  disabled="$(launchctl print-disabled "gui/${uid}" 2>/dev/null)" || \
    die 'unable to capture the launchd enabled state'
  if grep -Eq '"com\.opencodex-relay\.relay"[[:space:]]*=>[[:space:]]*true' <<<"$disabled"; then
    printf 'disabled\n'
  else
    printf 'enabled\n'
  fi
}

restore_launchd_enabled_state() {
  local uid="$1"
  local label="$2"
  local state="$3"
  case "$state" in
    enabled) launchctl enable "gui/${uid}/${label}" ;;
    disabled) launchctl disable "gui/${uid}/${label}" ;;
    *) die 'service snapshot has an invalid launchd enabled state' ;;
  esac
}

action="${1:-}"
shift || true
config_path=""
was_active=""
snapshot_directory=""
case "$action" in
  install)
    [[ "${1:-}" == "--config" && $# -eq 2 ]] || { usage >&2; exit 2; }
    config_path="$2"
    [[ -x "$RELAY_BIN" ]] || die "relay binary is unavailable: $RELAY_BIN"
    [[ -f "$config_path" && ! -L "$config_path" ]] || \
      die "relay config is unavailable or unsafe: $config_path"
    safe_path "$RELAY_BIN" && safe_path "$config_path" || die 'relay service path contains unsupported XML/control characters'
    ;;
  uninstall|stop)
    [[ $# -eq 0 ]] || { usage >&2; exit 2; }
    ;;
  status)
    [[ $# -eq 0 ]] || { usage >&2; exit 2; }
    ;;
  restore)
    [[ "${1:-}" == "--was-active" && $# -eq 2 ]] || { usage >&2; exit 2; }
    was_active="$2"
    [[ "$was_active" == "true" || "$was_active" == "false" ]] || {
      printf '%s\n' 'ERROR: --was-active must be true or false' >&2
      exit 2
    }
    ;;
  snapshot|restore-snapshot)
    [[ "${1:-}" == "--directory" && $# -eq 2 ]] || { usage >&2; exit 2; }
    snapshot_directory="$2"
    require_snapshot_directory "$snapshot_directory"
    ;;
  *) usage >&2; exit 2 ;;
esac

case "$(uname -s)" in
  Darwin)
    label="io.github.novelkr.opencodex-relay"
    plist_dir="${HOME}/Library/LaunchAgents"
    plist="${plist_dir}/${label}.plist"
    uid="$(id -u)"
    case "$action" in
      install|uninstall|stop|restore|restore-snapshot)
        require_macos_source_lifecycle_reservation
        ;;
    esac
    if [[ "$action" == status ]]; then
      if launchctl print "gui/${uid}/${label}" >/dev/null 2>&1; then
        printf 'relay_service_active=true manager=launchd\n'
      else
        printf 'relay_service_active=false manager=launchd\n'
      fi
      exit 0
    fi
    if [[ "$action" == uninstall ]]; then
      launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
      rm -f "$plist"
      printf 'relay_service=uninstalled manager=launchd\n'
      exit 0
    fi
    if [[ "$action" == stop ]]; then
      launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
      printf 'relay_service=stopped manager=launchd\n'
      exit 0
    fi
    if [[ "$action" == snapshot ]]; then
      artifact_present="$(snapshot_service_file "$plist" "$snapshot_directory")"
      if launchctl print "gui/${uid}/${label}" >/dev/null 2>&1; then
        active=true
      else
        active=false
      fi
      [[ "$active" != true || "$artifact_present" == true ]] || \
        die 'cannot snapshot an active launchd relay without its prior plist'
      enabled="$(launchd_enabled_state "$uid" "$label")"
      write_snapshot_state "$snapshot_directory" "$artifact_present" "$active" "$enabled"
      printf 'relay_service=snapshotted manager=launchd active=%s enabled=%s artifact_present=%s\n' "$active" "$enabled" "$artifact_present"
      exit 0
    fi
    if [[ "$action" == restore-snapshot ]]; then
      artifact_present="$(snapshot_state_value "$snapshot_directory" artifact_present)"
      active="$(snapshot_state_value "$snapshot_directory" active)"
      enabled="$(snapshot_state_value "$snapshot_directory" enabled)"
      [[ "$active" == true || "$active" == false ]] || die 'service snapshot has an invalid launchd active state'
      [[ "$active" != true || "$enabled" == enabled ]] || \
        die 'service snapshot has an invalid active disabled launchd state'
      launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
      restore_service_file "$plist" "$snapshot_directory" "$artifact_present"
      restore_launchd_enabled_state "$uid" "$label" "$enabled"
      if [[ "$active" == true ]]; then
        [[ -f "$plist" ]] || die 'cannot restore an active launchd relay without its prior plist'
        launchctl bootstrap "gui/${uid}" "$plist"
        launchctl kickstart -k "gui/${uid}/${label}"
      fi
      printf 'relay_service=restored-snapshot manager=launchd active=%s enabled=%s artifact_present=%s\n' "$active" "$enabled" "$artifact_present"
      exit 0
    fi
    if [[ "$action" == restore ]]; then
      launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
      if [[ "$was_active" == true ]]; then
        [[ -x "$RELAY_BIN" && -f "$plist" ]] || die 'cannot restore an active launchd relay without its binary and plist'
        launchctl bootstrap "gui/${uid}" "$plist"
        launchctl kickstart -k "gui/${uid}/${label}"
      fi
      printf 'relay_service=restored manager=launchd active=%s\n' "$was_active"
      exit 0
    fi
    mkdir -p "$plist_dir" "${HOME}/Library/Logs/opencodex-relay"
    umask 077
    candidate="$(mktemp "${plist}.XXXXXX")"
    sed \
      -e "s|__RELAY_BIN__|${RELAY_BIN}|g" \
      -e "s|__RELAY_CONFIG__|${config_path}|g" \
      -e "s|__HOME__|${HOME}|g" \
      "${RELAY_ROOT}/macos/io.github.novelkr.opencodex-relay.plist.in" > "$candidate"
    chmod 0600 "$candidate"
    launchctl bootout "gui/${uid}" "$plist" >/dev/null 2>&1 || true
    mv -f "$candidate" "$plist"
    launchctl bootstrap "gui/${uid}" "$plist"
    launchctl kickstart -k "gui/${uid}/${label}"
    printf 'relay_service=installed manager=launchd\n'
    ;;
  Linux)
    unit_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
    unit="${unit_dir}/opencodex-relay.service"
    command -v systemctl >/dev/null || die 'systemctl --user is required on Linux'
    if [[ "$action" == status ]]; then
      if systemctl --user is-active --quiet opencodex-relay.service; then
        printf 'relay_service_active=true manager=systemd-user\n'
      else
        printf 'relay_service_active=false manager=systemd-user\n'
      fi
      exit 0
    fi
    if [[ "$action" == uninstall ]]; then
      systemctl --user disable --now opencodex-relay.service >/dev/null 2>&1 || true
      rm -f "$unit"
      systemctl --user daemon-reload
      printf 'relay_service=uninstalled manager=systemd-user\n'
      exit 0
    fi
    if [[ "$action" == stop ]]; then
      systemctl --user stop opencodex-relay.service >/dev/null 2>&1 || true
      printf 'relay_service=stopped manager=systemd-user\n'
      exit 0
    fi
    if [[ "$action" == snapshot ]]; then
      artifact_present="$(snapshot_service_file "$unit" "$snapshot_directory")"
      if systemctl --user is-active --quiet opencodex-relay.service; then
        active=true
      else
        active=false
      fi
      enabled="$(systemd_enabled_state)"
      if [[ "$artifact_present" != true && ( "$active" == true || "$enabled" != not-found ) ]]; then
        die 'cannot snapshot an existing systemd relay without its prior unit file'
      fi
      write_snapshot_state "$snapshot_directory" "$artifact_present" "$active" "$enabled"
      printf 'relay_service=snapshotted manager=systemd-user active=%s enabled=%s artifact_present=%s\n' "$active" "$enabled" "$artifact_present"
      exit 0
    fi
    if [[ "$action" == restore-snapshot ]]; then
      artifact_present="$(snapshot_state_value "$snapshot_directory" artifact_present)"
      active="$(snapshot_state_value "$snapshot_directory" active)"
      enabled="$(snapshot_state_value "$snapshot_directory" enabled)"
      [[ "$active" == true || "$active" == false ]] || die 'service snapshot has an invalid systemd active state'
      systemctl --user stop opencodex-relay.service >/dev/null 2>&1 || true
      restore_service_file "$unit" "$snapshot_directory" "$artifact_present"
      systemctl --user daemon-reload
      restore_systemd_enabled_state "$enabled"
      if [[ "$active" == true ]]; then
        [[ -f "$unit" ]] || die 'cannot restore an active systemd relay without its prior unit'
        systemctl --user restart opencodex-relay.service
      else
        systemctl --user reset-failed opencodex-relay.service >/dev/null 2>&1 || true
      fi
      printf 'relay_service=restored-snapshot manager=systemd-user active=%s enabled=%s artifact_present=%s\n' "$active" "$enabled" "$artifact_present"
      exit 0
    fi
    if [[ "$action" == restore ]]; then
      systemctl --user daemon-reload
      if [[ "$was_active" == true ]]; then
        [[ -x "$RELAY_BIN" ]] || die 'cannot restore an active systemd relay without its binary'
        systemctl --user restart opencodex-relay.service
      else
        systemctl --user stop opencodex-relay.service >/dev/null 2>&1 || true
        systemctl --user reset-failed opencodex-relay.service >/dev/null 2>&1 || true
      fi
      printf 'relay_service=restored manager=systemd-user active=%s\n' "$was_active"
      exit 0
    fi
    command -v jq >/dev/null || die 'jq is required on Linux'
    command -v realpath >/dev/null || die 'realpath is required on Linux'
    [[ -f "$config_path" && ! -L "$config_path" ]] || die 'relay config must be a regular file'
    catalog_path="$(jq -er '.catalog.path | select(type == "string" and startswith("/"))' "$config_path")" || \
      die 'relay config catalog.path must be an absolute path'
    catalog_parent="$(realpath -m -- "$(dirname -- "$catalog_path")")"
    home_root="$(realpath -m -- "$HOME")"
    case "$catalog_parent" in
      "$home_root"|"$home_root"/*) ;;
      *) die 'relay catalog.path must remain within the user home for systemd sandboxing' ;;
    esac
    safe_path "$catalog_parent" || die 'catalog parent contains unsupported systemd/sed characters'
    umask 077
    mkdir -p "$catalog_parent"
    mkdir -p "$unit_dir"
    candidate="$(mktemp "${unit}.XXXXXX")"
    sed \
      -e "s|__RELAY_BIN__|${RELAY_BIN}|g" \
      -e "s|__RELAY_CONFIG__|${config_path}|g" \
      -e "s|__CATALOG_PARENT__|${catalog_parent}|g" \
      "${RELAY_ROOT}/systemd/opencodex-relay.service.in" > "$candidate"
    chmod 0600 "$candidate"
    mv -f "$candidate" "$unit"
    systemctl --user daemon-reload
    systemctl --user enable opencodex-relay.service
    if systemctl --user is-active --quiet opencodex-relay.service; then
      systemctl --user restart opencodex-relay.service
      service_action=restarted
    else
      systemctl --user start opencodex-relay.service
      service_action=started
    fi
    printf 'relay_service=installed manager=systemd-user action=%s\n' "$service_action"
    ;;
  *) die "unsupported operating system: $(uname -s)" ;;
esac

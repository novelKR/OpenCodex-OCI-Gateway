#!/usr/bin/env bash
# Install an offline-only, ad-hoc-signed macOS local development bundle.
# It deliberately has no release URL, updater, notarization, Gatekeeper
# assessment, quarantine removal, or production-root fallback.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELAY_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly KEYCHAIN_HELPER="${SCRIPT_DIR}/keychain-local-dev-public-key.swift"
readonly SERVICE_HELPER="${SCRIPT_DIR}/install-local-dev-service.sh"
readonly INSTALL_ROOT="${HOME}/.local/lib/opencodex-relay/relay-dev"
readonly PENDING_ROOT="${INSTALL_ROOT}/pending"
readonly DEV_CONFIG_DIR="${HOME}/.config/opencodex-relay/relay-dev"
# This namespace is deliberately fixed for the local-only distribution.
# Callers that need an isolated custom Codex home can still pass --config
# explicitly, but an ambient XDG_CONFIG_HOME must never redirect the default
# dev installation into a production or organization-managed config root.
readonly DEFAULT_CONFIG="${DEV_CONFIG_DIR}/relay.json"
readonly DEFAULT_CODEX_CONFIG="${HOME}/.codex/config.toml"
readonly APP_NAME="OpenCodexRelay Dev.app"
readonly APP_ZIP="${APP_NAME}.zip"
readonly BUNDLE_ID="io.github.novelkr.opencodex-relay.dev"
readonly GUARD_DAEMON_PLIST="io.github.novelkr.opencodex-relay.homebrew-guard.dev.plist"
readonly GUARD_HELPER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.helper.dev"
readonly GUARD_HELPER_NAME="OpenCodexRelayPrivilegedHelper"
readonly GUARD_INSTALLER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.installer.dev"
readonly GUARD_INSTALLER_NAME="OpenCodexRelayHelperInstaller"
readonly GUARD_MANUAL_SERVICE="io.github.novelkr.opencodex-relay.homebrew-guard.manual.dev"
readonly TRUSTED_CODEX_BUNDLE_ID="com.openai.codex"
readonly TRUSTED_CODEX_TEAM_ID="2DC432GLL2"
readonly APP_LINK="${HOME}/Applications/${APP_NAME}"
readonly BINDING_DIR="${HOME}/Library/Application Support/OpenCodexRelayDev"
readonly BINDING_PATH="${BINDING_DIR}/routing-binding.json"
readonly SERVICE_PLIST="${HOME}/Library/LaunchAgents/io.github.novelkr.opencodex-relay.dev.plist"
readonly NOTICES_FILE="THIRD_PARTY_NOTICES.md"
readonly KEY_PREFIX="opencodex-relay-local-dev-trust-"
readonly SOURCE_INSTALL_RESERVATION_NAME=".source-install-reservation.json"

usage() {
  cat <<'USAGE'
Usage:
  install-local-dev.sh trust enroll --keychain-service SERVICE --public-key PEM --expected-fingerprint SHA256
  install-local-dev.sh trust replace --keychain-service SERVICE --old-fingerprint SHA256 --public-key PEM --expected-fingerprint SHA256
  install-local-dev.sh install VERSION --source-dir DIR --upstream HTTPS_V1_URL
      --acknowledge-local-development-source (--acknowledge-local-source | --keychain-service SERVICE)
      [--config PATH] [--codex-config PATH] [--catalog-path PATH] [--codex-executable PATH]
  install-local-dev.sh uninstall [--config PATH] [--codex-config PATH] [--confirm-desktop-exited]

This installer accepts only an offline local source directory. It never
downloads artifacts, registers a login item, removes quarantine, changes
Gatekeeper, or modifies a production relay installation.

Any --config path must be a clean absolute path beneath
~/.config/opencodex-relay/relay-dev/.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

sha256() {
  if command -v shasum >/dev/null; then shasum -a 256 "$1" | awk '{print $1}'; else sha256sum "$1" | awk '{print $1}'; fi
}

decode_base64() {
  local source="$1" destination="$2"
  if base64 -D < "$source" > "$destination" 2>/dev/null; then return 0; fi
  base64 --decode < "$source" > "$destination" 2>/dev/null || die 'local development signature is not valid base64'
}

require_ed25519_public_key() {
  openssl pkey -pubin -in "$1" -text -noout 2>/dev/null | grep -Eq '^ED25519 Public-Key:' || \
    die 'local development public key must be an Ed25519 public PEM'
}

safe_absolute_path() {
  [[ "$1" == /* && "$1" != *$'\n'* && "$1" != *$'\r'* && "$1" != *'//'* && \
     "$1" != *'/./'* && "$1" != */. && "$1" != *'/../'* && "$1" != */.. && "$1" != ./* ]]
}

require_local_dev_config_path() {
  local path="$1" suffix parent component
  safe_absolute_path "$path" || die '--config must be a clean absolute path'
  [[ "$path" == "${DEV_CONFIG_DIR}/"* ]] || \
    die "--config must stay beneath ${DEV_CONFIG_DIR}/"
  suffix="${path#"${DEV_CONFIG_DIR}/"}"
  [[ -n "$suffix" && "$suffix" != "$path" ]] || \
    die "--config must name a file beneath ${DEV_CONFIG_DIR}/"
  for parent in "${HOME}/.config" "${HOME}/.config/opencodex-relay" "$DEV_CONFIG_DIR"; do
    if [[ -e "$parent" || -L "$parent" ]]; then
      [[ -d "$parent" && ! -L "$parent" ]] || die "local development config parent is unsafe: $parent"
    fi
  done
  parent="$DEV_CONFIG_DIR"
  while [[ "$suffix" == */* ]]; do
    component="${suffix%%/*}"
    parent="${parent}/${component}"
    if [[ -e "$parent" || -L "$parent" ]]; then
      [[ -d "$parent" && ! -L "$parent" ]] || die "local development config parent is unsafe: $parent"
    fi
    suffix="${suffix#*/}"
  done
}

ensure_local_dev_config_parent() {
  local config="$1" config_parent suffix parent component canonical_root canonical_parent
  require_local_dev_config_path "$config"
  config_parent="${config%/*}"
  suffix="${config_parent#"${HOME}/"}"
  [[ -n "$suffix" && "$suffix" != "$config_parent" ]] || \
    die "local development config parent is outside ${HOME}"
  parent="$HOME"
  while [[ -n "$suffix" ]]; do
    if [[ "$suffix" == */* ]]; then
      component="${suffix%%/*}"
      suffix="${suffix#*/}"
    else
      component="$suffix"
      suffix=""
    fi
    parent="${parent}/${component}"
    if [[ ! -e "$parent" && ! -L "$parent" ]]; then
      mkdir -m 0700 -- "$parent" || die "unable to create local development config parent: $parent"
    fi
    [[ -d "$parent" && ! -L "$parent" ]] || die "local development config parent is unsafe: $parent"
  done
  canonical_root="$(cd -P -- "$DEV_CONFIG_DIR" && pwd -P)" || \
    die 'unable to resolve local development config root'
  canonical_parent="$(cd -P -- "$config_parent" && pwd -P)" || \
    die 'unable to resolve local development config parent'
  [[ "$canonical_parent" == "$canonical_root" || "$canonical_parent" == "${canonical_root}/"* ]] || \
    die 'local development config parent resolves outside the dev namespace'
}

require_regular_or_absent() {
  local path="$1"
  [[ ! -e "$path" && ! -L "$path" ]] && return 0
  [[ -f "$path" && ! -L "$path" ]] || die "path is unsafe: $path"
}

require_owner_mode_600_if_present() {
  local path="$1" mode
  [[ -f "$path" && ! -L "$path" ]] || return 0
  mode="$(stat -f '%u:%Lp' "$path")"
  [[ "$mode" == "$(id -u):600" ]] || die "owner-only mode 0600 is required: $path"
}

require_owner_directory_mode() {
  local path="$1" expected_mode="$2" actual
  [[ -d "$path" && ! -L "$path" ]] || die "owner-only directory is unavailable or unsafe: $path"
  actual="$(stat -f '%u:%Lp' "$path")"
  [[ "$actual" == "$(id -u):${expected_mode}" ]] || \
    die "owner-only directory mode ${expected_mode} is required: $path"
}

ensure_local_dev_install_root() {
  local suffix="${INSTALL_ROOT#"${HOME}/"}" parent="$HOME" component actual owner mode
  [[ "$suffix" != "$INSTALL_ROOT" && -n "$suffix" ]] || die 'local development install root is outside HOME'
  while [[ -n "$suffix" ]]; do
    if [[ "$suffix" == */* ]]; then
      component="${suffix%%/*}"
      suffix="${suffix#*/}"
    else
      component="$suffix"
      suffix=""
    fi
    parent="${parent}/${component}"
    if [[ ! -e "$parent" && ! -L "$parent" ]]; then
      mkdir -m 0700 -- "$parent" || die 'unable to create local development install root'
    fi
    [[ -d "$parent" && ! -L "$parent" ]] || die 'local development install root parent is unsafe'
    actual="$(stat -f '%u:%Lp' "$parent")"
    owner="${actual%%:*}"
    mode="${actual#*:}"
    [[ "$owner" == "$(id -u)" && $((8#$mode & 8#22)) -eq 0 ]] || \
      die 'local development install root parent has unsafe ownership or mode'
  done
  chmod 0700 "$INSTALL_ROOT" || die 'unable to protect local development install root'
  require_owner_directory_mode "$INSTALL_ROOT" 700
}

require_local_dev_config_leaves_or_absent() {
  local config="$1" leaf maintenance
  maintenance="${config}.runtime-maintenance.json"
  for leaf in \
    "$config" \
    "${config}.routing-state.json" \
    "${config}.routing-initialized" \
    "${config}.routing-transaction.json" \
    "$maintenance"; do
    require_regular_or_absent "$leaf"
  done
  [[ ! -e "$maintenance" && ! -L "$maintenance" ]] || \
    die 'runtime maintenance must be recovered before changing the local development installation'
}

snapshot_runtime_maintenance_absence() {
  local path="$1" snapshot="$2"
  [[ ! -e "$path" && ! -L "$path" ]] || \
    die 'runtime maintenance must be recovered before changing the local development installation'
  [[ ! -e "${snapshot}.state" && ! -L "${snapshot}.state" && \
     ! -e "${snapshot}.data" && ! -L "${snapshot}.data" ]] || \
    die 'runtime maintenance rollback snapshot destination is unsafe'
  printf 'present=false\n' > "${snapshot}.state"
  chmod 0600 "${snapshot}.state"
}

verify_runtime_maintenance_absence_snapshot() {
  local path="$1" snapshot="$2" present
  [[ -f "${snapshot}.state" && ! -L "${snapshot}.state" && \
     ! -e "${snapshot}.data" && ! -L "${snapshot}.data" ]] || return 1
  present="$(sed -nE 's/^present=(true|false)$/\1/p' "${snapshot}.state")"
  [[ "$present" == false ]] || return 1
  # Never remove a maintenance journal which appeared while the installer was
  # active. Its own recovery protocol, not installer rollback, owns that leaf.
  [[ ! -e "$path" && ! -L "$path" ]]
}

local_dev_config_leaves_present() {
  local config="$1" leaf
  for leaf in \
    "$config" \
    "${config}.routing-state.json" \
    "${config}.routing-initialized" \
    "${config}.routing-transaction.json" \
    "${config}.runtime-maintenance.json"; do
    [[ -e "$leaf" || -L "$leaf" ]] && return 0
  done
  return 1
}

require_managed_dev_link_or_absent() {
  local path="$1" target
  [[ ! -e "$path" && ! -L "$path" ]] && return 0
  [[ -L "$path" ]] || die "existing local development link is unsafe: $path"
  target="$(readlink "$path")"
  if [[ "$path" == "${INSTALL_ROOT}/current" ]]; then
    [[ "$target" != /* && "$target" != *'..'* && "$target" == */"${APP_NAME}"/Contents/Library/Helpers ]] || \
      die "existing current link is not managed by the local development install root"
  else
    [[ "$target" == "${INSTALL_ROOT}/"* ]] || die "existing link is not managed by the local development install root: $path"
  fi
}

require_local_dev_uninstall_artifacts_safe() {
  local config="$1"
  require_local_dev_config_leaves_or_absent "$config"

  if [[ -e "$INSTALL_ROOT" || -L "$INSTALL_ROOT" ]]; then
    [[ -d "$INSTALL_ROOT" && ! -L "$INSTALL_ROOT" ]] || die 'local development install root is unsafe'
  fi
  require_managed_dev_link_or_absent "${INSTALL_ROOT}/current"
  require_managed_dev_link_or_absent "$APP_LINK"

  if [[ -e "$BINDING_DIR" || -L "$BINDING_DIR" ]]; then
    [[ -d "$BINDING_DIR" && ! -L "$BINDING_DIR" ]] || die 'local development binding directory is unsafe'
  fi
  require_regular_or_absent "$BINDING_PATH"
  require_owner_mode_600_if_present "$BINDING_PATH"
  require_regular_or_absent "$SERVICE_PLIST"
}

local_dev_uninstall_artifacts_present() {
  local config="$1" path
  local_dev_config_leaves_present "$config" && return 0
  for path in "$INSTALL_ROOT" "$APP_LINK" "$BINDING_DIR" "$BINDING_PATH" "$SERVICE_PLIST"; do
    [[ -e "$path" || -L "$path" ]] && return 0
  done
  return 1
}

local_dev_service_is_active() {
  [[ "$(uname -s)" == Darwin ]] || return 1
  command -v launchctl >/dev/null || die 'launchctl is required to inspect local development service ownership'
  local uid
  uid="$(id -u)"
  launchctl print "gui/${uid}/io.github.novelkr.opencodex-relay.dev" >/dev/null 2>&1
}

require_managed_local_dev_relayctl() {
  local relayctl="$1"
  [[ -d "$INSTALL_ROOT" && ! -L "$INSTALL_ROOT" ]] || \
    die 'local development install root is unavailable; refusing uninstall'
  [[ -L "${INSTALL_ROOT}/current" ]] || \
    die 'managed local development current helper link is unavailable; refusing uninstall'
  require_managed_dev_link_or_absent "${INSTALL_ROOT}/current"
  [[ -x "$relayctl" && -f "$relayctl" && ! -L "$relayctl" ]] || \
    die 'managed local development relayctl helper is unavailable or unsafe; refusing uninstall'
}

snapshot_local_source_file() {
  local source="$1" destination="$2" name="$3"
  [[ -f "$source" && ! -L "$source" ]] || die "local source file is unavailable or unsafe: ${name}"
  [[ ! -e "$destination" && ! -L "$destination" ]] || die "local source snapshot destination is unsafe: ${name}"
  cp -pP -- "$source" "$destination" || die "unable to snapshot local source file: ${name}"
  [[ -f "$destination" && ! -L "$destination" ]] || die "local source snapshot is unavailable or unsafe: ${name}"
  chmod 0600 "$destination" || die "unable to protect local source snapshot: ${name}"
  require_owner_mode_600_if_present "$destination"
}

require_keychain_service() {
  [[ "$1" == ${KEY_PREFIX}* && "$1" =~ ^[A-Za-z0-9._-]{1,160}$ ]] || \
    die "Keychain trust service must begin with ${KEY_PREFIX} and contain only safe characters"
}

snapshot_file() {
  local source="$1" destination="$2"
  require_regular_or_absent "$source"
  if [[ -f "$source" ]]; then
    cp -p -- "$source" "${destination}.data"
    printf 'present=true\n' > "${destination}.state"
  else
    printf 'present=false\n' > "${destination}.state"
  fi
  chmod 0600 "${destination}.state" "${destination}.data" 2>/dev/null || true
}

restore_file() {
  local destination="$1" snapshot="$2"
  local present
  present="$(sed -nE 's/^present=(true|false)$/\1/p' "${snapshot}.state")"
  [[ "$present" == true || "$present" == false ]] || return 1
  if [[ "$present" == true ]]; then
    [[ -f "${snapshot}.data" && ! -L "${snapshot}.data" ]] || return 1
    mkdir -p "$(dirname -- "$destination")"
    local candidate
    candidate="$(mktemp "${destination}.rollback.XXXXXX")" || return 1
    cp -p -- "${snapshot}.data" "$candidate" || { rm -f -- "$candidate"; return 1; }
    mv -f -- "$candidate" "$destination" || { rm -f -- "$candidate"; return 1; }
  else
    if [[ -e "$destination" || -L "$destination" ]]; then
      [[ -f "$destination" && ! -L "$destination" ]] || return 1
      rm -f -- "$destination"
    fi
  fi
}

snapshot_link() {
  local source="$1" destination="$2"
  if [[ ! -e "$source" && ! -L "$source" ]]; then
    printf 'present=false\n' > "${destination}.state"
  else
    [[ -L "$source" ]] || die "existing managed link is unsafe: $source"
    printf 'present=true\ntarget=%s\n' "$(readlink "$source")" > "${destination}.state"
  fi
  chmod 0600 "${destination}.state"
}

restore_link() {
  local destination="$1" snapshot="$2"
  local present target
  present="$(sed -nE 's/^present=(true|false)$/\1/p' "${snapshot}.state")"
  [[ "$present" == true || "$present" == false ]] || return 1
  if [[ "$present" == false ]]; then
    if [[ -e "$destination" || -L "$destination" ]]; then
      [[ -L "$destination" ]] || return 1
      rm -f -- "$destination"
    fi
    return 0
  fi
  target="$(sed -nE 's/^target=(.+)$/\1/p' "${snapshot}.state")"
  [[ -n "$target" && "$target" != /* && "$target" != *'..'* ]] || return 1
  mkdir -p "$(dirname -- "$destination")"
  local candidate="$(dirname -- "$destination")/.restore.$$.link"
  ln -s "$target" "$candidate" || return 1
  mv -fh "$candidate" "$destination" || { rm -f -- "$candidate"; return 1; }
}

manifest_valid() {
  local manifest="$1" version="$2"
  jq -e --arg version "$version" --arg bundle "$APP_ZIP" --arg bundle_id "$BUNDLE_ID" --arg notices "$NOTICES_FILE" '
    (keys | sort) == ["artifacts","distribution","documents","schema","source_commit","version"]
    and (.schema == 1 or .schema == 2 or .schema == 3)
    and .distribution == "local_development"
    and .version == $version
    and (.source_commit | type == "string" and test("^[0-9a-f]{40}$"))
    and (.artifacts | type == "array" and length == 1)
    and (.artifacts[0] | keys | sort) == ["arch","bundle_id","component","file","os","sha256"]
    and .artifacts[0].os == "darwin" and .artifacts[0].arch == "arm64"
    and .artifacts[0].component == "macos_menu_bar_bundle"
    and .artifacts[0].file == $bundle and .artifacts[0].bundle_id == $bundle_id
    and (.artifacts[0].sha256 | type == "string" and test("^[0-9a-f]{64}$"))
    and (.documents | type == "array" and length == 1)
    and (.documents[0] | keys | sort) == ["file","sha256"]
    and .documents[0].file == $notices
    and (.documents[0].sha256 | type == "string" and test("^[0-9a-f]{64}$"))
  ' "$manifest" >/dev/null
}

verify_bundle_shape() {
  local archive="$1" expected_hash="$2" destination="$3" manifest_schema="$4"
  [[ "$manifest_schema" == 1 || "$manifest_schema" == 2 || "$manifest_schema" == 3 ]] || die 'local development manifest schema is unsupported'
  [[ "$(sha256 "$archive")" == "$expected_hash" ]] || die 'local development bundle checksum does not match its manifest'
  local entries entry
  entries="$(unzip -Z1 "$archive")" || die 'local development bundle cannot be listed'
  [[ -n "$entries" ]] || die 'local development bundle is empty'
  while IFS= read -r entry; do
    [[ "$entry" == "$APP_NAME" || "$entry" == "${APP_NAME}/"* ]] || die 'local development bundle has an unexpected top-level path'
    [[ "$entry" != /* && "$entry" != *'..'* && "$entry" != *'//'* ]] || die 'local development bundle contains an unsafe path'
  done <<< "$entries"
  ditto -x -k "$archive" "$destination" || die 'unable to extract local development bundle'
  local app="${destination}/${APP_NAME}"
  [[ -d "$app" && ! -L "$app" ]] || die 'local development app bundle is unavailable or unsafe'
  if find "$app" -type l -print -quit | grep -q .; then die 'local development app bundle must not contain symbolic links'; fi
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" == "$BUNDLE_ID" ]] || die 'local development app bundle identifier is invalid'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexDistributionFlavor' "${app}/Contents/Info.plist" 2>/dev/null)" == local_development ]] || die 'local development app flavor is invalid'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIconFile' "${app}/Contents/Info.plist" 2>/dev/null)" == AppIcon.icns &&
     -f "${app}/Contents/Resources/AppIcon.icns" && ! -L "${app}/Contents/Resources/AppIcon.icns" ]] ||
    die 'local development bundle app icon is invalid'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :LSUIElement' "${app}/Contents/Info.plist" 2>/dev/null)" == false ]] ||
    die 'local development bundle must remain visible in the Dock'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexTrustedCodexBundleIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" == "$TRUSTED_CODEX_BUNDLE_ID" ]] || \
    die 'local development bundle Codex Desktop identifier is not reviewed'
  [[ "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexTrustedCodexTeamIdentifier' "${app}/Contents/Info.plist" 2>/dev/null)" == "$TRUSTED_CODEX_TEAM_ID" ]] || \
    die 'local development bundle Codex Desktop Team ID is not reviewed'
  local runtime_public_key="${app}/Contents/Resources/RuntimeTrust/opencodex-runtime-release-ed25519.pub"
  [[ -f "$runtime_public_key" && ! -L "$runtime_public_key" ]] || \
    die 'local development runtime release trust key is unavailable or unsafe'
  require_ed25519_public_key "$runtime_public_key"
  [[ -x "${app}/Contents/MacOS/OpenCodexRelay" && ! -L "${app}/Contents/MacOS/OpenCodexRelay" && \
     -x "${app}/Contents/Library/Helpers/opencodex-relay" && ! -L "${app}/Contents/Library/Helpers/opencodex-relay" && \
     -x "${app}/Contents/Library/Helpers/opencodex-relayctl" && ! -L "${app}/Contents/Library/Helpers/opencodex-relayctl" ]] || die 'local development bundle helper shape is invalid'
  if [[ "$manifest_schema" == 2 || "$manifest_schema" == 3 ]]; then
    local guard_helper="${app}/Contents/Library/HelperTools/${GUARD_HELPER_NAME}"
    [[ -x "$guard_helper" && ! -L "$guard_helper" ]] ||
      die 'local development Homebrew guard bundle shape is invalid'
    if [[ "$manifest_schema" == 2 ]]; then
      local guard_plist="${app}/Contents/Library/LaunchDaemons/${GUARD_DAEMON_PLIST}"
      [[ -f "$guard_plist" && ! -L "$guard_plist" &&
         "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardDaemonPlist' "${app}/Contents/Info.plist" 2>/dev/null)" == "$GUARD_DAEMON_PLIST" &&
         "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardMachService' "${app}/Contents/Info.plist" 2>/dev/null)" == "io.github.novelkr.opencodex-relay.homebrew-guard.dev" &&
         "$(/usr/libexec/PlistBuddy -c 'Print :Label' "$guard_plist" 2>/dev/null)" == "io.github.novelkr.opencodex-relay.homebrew-guard.dev" &&
         "$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:7' "$guard_plist" 2>/dev/null)" == "embedded_app_cdhash" ]] ||
        die 'local development Homebrew guard metadata is invalid'
    fi
    codesign --verify --strict --verbose=2 "$guard_helper" ||
      die 'local development Homebrew guard helper signature is invalid'
    codesign -dv --verbose=4 "$guard_helper" 2>&1 | grep -Fx "Identifier=${GUARD_HELPER_IDENTIFIER}" >/dev/null ||
      die 'local development Homebrew guard helper identifier is invalid'
    local guard_cdhash
    guard_cdhash="$(codesign -dvvv "$guard_helper" 2>&1 | sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p')"
    [[ "$guard_cdhash" =~ ^[0-9a-f]{40,128}$ &&
       "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardHelperRequirement' "${app}/Contents/Info.plist" 2>/dev/null)" == "cdhash H\"${guard_cdhash}\"" ]] ||
      die 'local development Homebrew guard helper CDHash binding is invalid'
    if [[ "$manifest_schema" == 3 ]]; then
      local guard_installer="${app}/Contents/Library/Helpers/${GUARD_INSTALLER_NAME}"
      [[ -x "$guard_installer" && ! -L "$guard_installer" ]] ||
        die 'local development Homebrew guard installer is unavailable'
      [[ "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardBackend' "${app}/Contents/Info.plist" 2>/dev/null)" == manual_admin &&
         "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardMachService' "${app}/Contents/Info.plist" 2>/dev/null)" == "$GUARD_MANUAL_SERVICE" &&
         "$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardInstallerExecutable' "${app}/Contents/Info.plist" 2>/dev/null)" == "$GUARD_INSTALLER_NAME" &&
         ! -e "${app}/Contents/Library/LaunchDaemons" ]] ||
        die 'local development manual Homebrew guard metadata is invalid'
      codesign --verify --strict --verbose=2 "$guard_installer" ||
        die 'local development Homebrew guard installer signature is invalid'
      codesign -dv --verbose=4 "$guard_installer" 2>&1 | grep -Fx "Identifier=${GUARD_INSTALLER_IDENTIFIER}" >/dev/null ||
        die 'local development Homebrew guard installer identifier is invalid'
    fi
  fi
  codesign --verify --deep --strict --verbose=2 "$app" || die 'local development bundle ad-hoc signature is invalid'
}

code_hash() {
  local value
  value="$(codesign -dvvv "$1" 2>&1 | sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p')"
  [[ "$value" =~ ^[0-9a-f]{40,128}$ ]] || die 'local development candidate CDHash is unavailable'
  printf '%s\n' "$value"
}

bundle_metadata_manifest() {
  local root="$1" path relative kind mode path_hash
  [[ -d "$root" && ! -L "$root" ]] || return 1
  if find "$root" ! -type f ! -type d -print -quit | grep -q .; then return 1; fi
  while IFS= read -r -d '' path; do
    relative="${path#"$root"}"
    relative="${relative#/}"
    [[ -n "$relative" ]] || relative='.'
    if [[ -d "$path" && ! -L "$path" ]]; then
      kind=d
    elif [[ -f "$path" && ! -L "$path" ]]; then
      kind=f
    else
      return 1
    fi
    mode="$(stat -f '%Lp' "$path")" || return 1
    [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
    path_hash="$(printf '%s' "$relative" | shasum -a 256 | awk '{print $1}')"
    [[ "$path_hash" =~ ^[0-9a-f]{64}$ ]] || return 1
    printf '%s:%s:%s\n' "$path_hash" "$kind" "$mode"
  done < <(find "$root" -print0) | LC_ALL=C sort
}

bundle_metadata_matches() {
  local pending_manifest extracted_manifest
  pending_manifest="$(bundle_metadata_manifest "$1")" || return 1
  extracted_manifest="$(bundle_metadata_manifest "$2")" || return 1
  [[ "$pending_manifest" == "$extracted_manifest" ]]
}

pending_candidate_matches() {
  local pending_app="$1" extracted_app="$2" marker="$3" version="$4" artifact_hash="$5"
  [[ -d "$pending_app" && ! -L "$pending_app" && -f "$marker" && ! -L "$marker" ]] || return 1
  require_owner_mode_600_if_present "$marker"
  jq -e --arg version "$version" --arg sha256 "$artifact_hash" '
    (keys | sort) == ["artifact_sha256","schema","version"]
    and .schema == 1 and .version == $version and .artifact_sha256 == $sha256
  ' "$marker" >/dev/null || return 1
  if find "$pending_app" -type l -print -quit | grep -q .; then return 1; fi
  if find "$extracted_app" -type l -print -quit | grep -q .; then return 1; fi
  bundle_metadata_matches "$pending_app" "$extracted_app" || return 1
  codesign --verify --deep --strict --verbose=2 "$pending_app" >/dev/null 2>&1 || return 1
  diff -qr -- "$pending_app" "$extracted_app" >/dev/null 2>&1 || return 1
  local relative
  for relative in \
    'Contents/MacOS/OpenCodexRelay' \
    'Contents/Library/Helpers/opencodex-relay' \
    'Contents/Library/Helpers/opencodex-relayctl' \
    "Contents/Library/HelperTools/${GUARD_HELPER_NAME}" \
    "Contents/Library/Helpers/${GUARD_INSTALLER_NAME}"; do
    [[ -x "${pending_app}/${relative}" && -f "${pending_app}/${relative}" &&
       ! -L "${pending_app}/${relative}" &&
       -x "${extracted_app}/${relative}" && -f "${extracted_app}/${relative}" &&
       ! -L "${extracted_app}/${relative}" ]] || return 1
    [[ "$(code_hash "${pending_app}/${relative}")" == "$(code_hash "${extracted_app}/${relative}")" ]] || return 1
  done
}

prepare_manual_helper_candidate() {
  local version="$1" extracted_app="$2" artifact_hash="$3"
  local pending_dir="${PENDING_ROOT}/${version}" pending_app="${PENDING_ROOT}/${version}/${APP_NAME}"
  local marker="${pending_dir}/candidate.json" candidate status action installer main
  pending_candidate_active=false
  pending_candidate_dir=""

  if [[ -e "$pending_dir" || -L "$pending_dir" ]]; then
    require_owner_directory_mode "$PENDING_ROOT" 700
    require_owner_directory_mode "$pending_dir" 700
    pending_candidate_matches "$pending_app" "$extracted_app" "$marker" "$version" "$artifact_hash" || \
      die 'pending local development helper candidate does not match the verified source artifact'
    app="$pending_app"
    pending_candidate_active=true
    pending_candidate_dir="$pending_dir"
  else
    app="$extracted_app"
  fi

  main="${app}/Contents/MacOS/OpenCodexRelay"
  [[ -x "$main" && -f "$main" && ! -L "$main" ]] || die 'local development candidate controller is unavailable'
  status="$("$main" --homebrew-guard-status 2>/dev/null)" || \
    die 'unable to inspect the candidate development Homebrew guard'
  [[ "$status" == homebrew_guard_registration=ready ]] && return 0

  if [[ "$pending_candidate_active" != true ]]; then
    if [[ -e "$PENDING_ROOT" || -L "$PENDING_ROOT" ]]; then
      require_owner_directory_mode "$PENDING_ROOT" 700
    else
      mkdir -p "$PENDING_ROOT"
      chmod 0700 "$PENDING_ROOT"
      require_owner_directory_mode "$PENDING_ROOT" 700
    fi
    candidate="$(mktemp -d "${PENDING_ROOT}/.candidate.${version}.XXXXXX")"
    chmod 0700 "$candidate"
    mv "$extracted_app" "${candidate}/${APP_NAME}"
    jq -n --arg version "$version" --arg sha256 "$artifact_hash" \
      '{schema:1,version:$version,artifact_sha256:$sha256}' > "${candidate}/candidate.json"
    chmod 0600 "${candidate}/candidate.json"
    mv "$candidate" "$pending_dir"
    app="$pending_app"
    pending_candidate_active=true
    pending_candidate_dir="$pending_dir"
  fi

  case "$status" in
    homebrew_guard_registration=manual_install_required) action=install ;;
    homebrew_guard_registration=manual_update_required|homebrew_guard_registration=daemon_launch_failed) action=update ;;
    homebrew_guard_registration=manual_installer_recovery_required|homebrew_guard_registration=recovery_required) action=recover ;;
    homebrew_guard_registration=busy)
      die 'active Homebrew protection must be restored before preparing the development helper'
      ;;
    *) die 'candidate development Homebrew guard status is unavailable; refusing app replacement' ;;
  esac
  installer="${app}/Contents/Library/Helpers/${GUARD_INSTALLER_NAME}"
  [[ -x "$installer" && -f "$installer" && ! -L "$installer" ]] || \
    die 'pending development helper installer is unavailable'
  printf 'local_dev_install=pending helper_state=%s\n' "${status#homebrew_guard_registration=}"
  printf 'Run the fixed candidate installer, then rerun the same install command:\n  sudo %q %q\n' "$installer" "$action"
  return 75
}

wait_for_parked_health() {
  local config="$1"
  local port general interactive health
  general="$(jq -er '.listen_address' "$config")"
  interactive="$(jq -er '.responses.scheduler.interactive_listen_address' "$config")"
  for port in "$general" "$interactive"; do
    for _ in {1..20}; do
      health="$(curl --fail --silent --show-error --noproxy '*' --max-time 2 "http://${port}/__relay/healthz" 2>/dev/null || true)"
      if jq -e '.ok == true and .relay_admission == "deny" and .catalog_refresh == "pause"' <<< "$health" >/dev/null 2>&1; then
        break
      fi
      sleep 1
    done
    jq -e '.ok == true and .relay_admission == "deny" and .catalog_refresh == "pause"' <<< "$health" >/dev/null || die 'local development relay did not reach parked health state'
  done
}

active_local_dev_runtime_is_acknowledged() {
  local config="$1"
  local relayctl="$2"
  local codex_path="$3"
  local status

  # This upgrade exception is deliberately status-only. The candidate helper
  # validates the durable routing/config witnesses and the resident relay
  # supplies the health projection; the installer neither reads nor forwards
  # the Apple Container API/Admin tokens.
  [[ -x "$relayctl" && -n "$codex_path" ]] || return 1
  status="$("$relayctl" mode status --config "$config" --codex-config "$codex_path" --json 2>/dev/null)" || return 1
  jq -er --slurpfile cfg "$config" '
    ($cfg[0]) as $c
    | ($c.local_opencodex // null) as $local
    | ($c.local_apple_container // null) as $apple
    | select(
        $c.installation_scope == "local_development"
        and $c.listen_address == "127.0.0.1:18190"
        and $c.responses.scheduler.interactive_listen_address == "127.0.0.1:18192"
        and $c.upstream_mode == "external_gateway"
        and (($c.catalog.owner // "relay") == "relay")
        and .schema_version == 4
        and .phase == "relay_active"
        and .relay_admission == "allow"
        and .catalog_refresh == "run"
        and .relay_running == true
        and .connection.local_relay == "healthy"
        and .connection.routing_sync == "acknowledged"
        and .connection.local_opencodex == "ready"
        and .connection.catalog == "running"
      )
    | if (
        .desired_backend == "local_opencodex"
        and .applied_backend == "local_opencodex"
        and ($local | type == "object")
        and ($local.upstream_base_url == "http://127.0.0.1:10100/v1" or $local.upstream_base_url == "http://[::1]:10100/v1")
        and ($local.catalog_path | type == "string" and startswith("/") and endswith("/opencodex-relay-dev-local-catalog.json") and . != $c.catalog.path)
        and (($apple == null) or $local.catalog_path != $apple.catalog_path)
      ) then
        "local_opencodex"
      elif (
        .desired_backend == "local_apple_container"
        and .applied_backend == "local_apple_container"
        and ($apple | type == "object")
        and $apple.upstream_base_url == "http://127.0.0.1:10210/v1"
        and ($apple.catalog_path | type == "string" and startswith("/") and endswith("/opencodex-relay-dev-apple-container-catalog.json") and . != $c.catalog.path)
        and (($local == null) or $apple.catalog_path != $local.catalog_path)
        and (($apple.credential_account // "") | type == "string")
      ) then
        "local_apple_container"
      else
        empty
      end
  ' <<< "$status"
}

local_dev_health_matches_listener_lane() {
  local health="$1"
  local config="$2"
  local expected_lane="$3"
  local runtime_profile="$4"
  jq -e --slurpfile cfg "$config" \
    --arg lane "$expected_lane" \
    --arg runtime_profile "$runtime_profile" '
      def nonnegative_integer:
        type == "number" and floor == . and . >= 0;
      def go_zero_default($fallback):
        if . == null or . == 0 then $fallback else . end;
      def go_empty_default($fallback):
        if . == null or . == "" then $fallback else . end;
      ($cfg[0]) as $c
      | ($c.local_opencodex // null) as $local
      | ($c.local_apple_container // null) as $apple
      | ($c.responses.scheduler // {}) as $s
      | .ok == true
      and .listener_lane == $lane
      and .general_listener == "127.0.0.1:18190"
      and .interactive_listener == "127.0.0.1:18192"
      and (
        if $runtime_profile == "local_opencodex" then
          .upstream_mode == "local_opencodex"
          and .upstream_base_url == $local.upstream_base_url
          and .catalog_owner == "relay"
        elif $runtime_profile == "local_apple_container" then
          .upstream_mode == "local_apple_container"
          and .upstream_base_url == $apple.upstream_base_url
          and .catalog_owner == "relay"
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

verify_active_local_dev_runtime_health_once() {
  local config="$1"
  local relayctl="$2"
  local codex_path="$3"
  local expected_profile="$4"
  local observed_profile
  local general_health
  local interactive_health
  observed_profile="$(active_local_dev_runtime_is_acknowledged "$config" "$relayctl" "$codex_path")" || return 1
  [[ "$observed_profile" == "$expected_profile" ]] || return 1
  general_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 2 \
    'http://127.0.0.1:18190/__relay/healthz' 2>/dev/null)" || return 1
  interactive_health="$(curl --fail --silent --show-error --noproxy '*' --max-time 2 \
    'http://127.0.0.1:18192/__relay/healthz' 2>/dev/null)" || return 1
  local_dev_health_matches_listener_lane "$general_health" "$config" general "$expected_profile" && \
    local_dev_health_matches_listener_lane "$interactive_health" "$config" interactive "$expected_profile"
}

wait_for_active_local_dev_runtime_health() {
  local config="$1"
  local relayctl="$2"
  local codex_path="$3"
  local expected_profile="$4"
  local attempt
  for attempt in {1..20}; do
    if verify_active_local_dev_runtime_health_once "$config" "$relayctl" "$codex_path" "$expected_profile"; then
      printf 'relay_dual_listener_health=ready runtime_profile=%s attempts=%s\n' "$expected_profile" "$attempt"
      return 0
    fi
    sleep 1
  done
  die 'local development relay did not preserve the acknowledged Local runtime health contract'
}

prepare_existing_homebrew_guard_for_replacement() {
  local replacement_app="${1:-}"
  guard_restore_helper=""
  [[ -e "$APP_LINK" || -L "$APP_LINK" ]] || return 0
  require_managed_dev_link_or_absent "$APP_LINK"
  local app
  app="$(readlink "$APP_LINK")" || die 'unable to inspect the existing local development app'
  [[ "$app" == "${INSTALL_ROOT}/"*"/${APP_NAME}" && -d "$app" && ! -L "$app" ]] ||
    die 'existing local development app target is unsafe'
  local info="$app/Contents/Info.plist"
  if ! /usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardDaemonPlist' "$info" >/dev/null 2>&1; then
    return 0
  fi
  local helper="$app/Contents/MacOS/OpenCodexRelay"
  [[ -x "$helper" && ! -L "$helper" ]] ||
    die 'existing local development Homebrew guard controller is unavailable'
  local result
  result="$("$helper" --homebrew-guard-status 2>/dev/null)" ||
    die 'unable to inspect the existing local development Homebrew guard'
  local backend
  backend="$(/usr/libexec/PlistBuddy -c 'Print :OpenCodexHomebrewGuardBackend' "$info" 2>/dev/null || true)"
  local replacement_ready=false replacement_controller="${replacement_app}/Contents/MacOS/OpenCodexRelay"
  if [[ -x "$replacement_controller" && -f "$replacement_controller" && ! -L "$replacement_controller" && \
        "$("$replacement_controller" --homebrew-guard-status 2>/dev/null || true)" == homebrew_guard_registration=ready ]]; then
    replacement_ready=true
  fi
  if [[ "$backend" == manual_admin ]]; then
    case "$result" in
      homebrew_guard_registration=ready|homebrew_guard_registration=manual_install_required)
        return 0
        ;;
      homebrew_guard_registration=busy|homebrew_guard_registration=recovery_required)
        die 'active Homebrew protection must be restored before replacing the local development app'
        ;;
      homebrew_guard_registration=manual_update_required|homebrew_guard_registration=daemon_launch_failed|homebrew_guard_registration=unavailable)
        [[ "$replacement_ready" == true ]] || \
          die 'update or restart the development Homebrew guard before replacing the local development app'
        return 0
        ;;
      *)
        die 'local development manual Homebrew guard status is unavailable; refusing app replacement'
        ;;
    esac
  fi
  case "$result" in
    homebrew_guard_registration=ready)
      result="$("$helper" --homebrew-guard-unregister 2>/dev/null)" ||
        die 'active Homebrew protection must be restored before replacing the local development app'
      [[ "$result" == homebrew_guard_registration=not_registered ]] ||
        die 'local development Homebrew guard did not confirm unregister'
      guard_restore_helper="$helper"
      ;;
    homebrew_guard_registration=not_registered)
      ;;
    homebrew_guard_registration=approval_required)
      result="$("$helper" --homebrew-guard-unregister 2>/dev/null)" ||
        die 'unable to cancel the pending local development Homebrew guard registration'
      [[ "$result" == homebrew_guard_registration=not_registered ]] ||
        die 'local development Homebrew guard did not confirm pending registration removal'
      guard_restore_helper="$helper"
      ;;
    homebrew_guard_registration=busy|homebrew_guard_registration=recovery_required)
      die 'active Homebrew protection must be restored before replacing the local development app'
      ;;
    homebrew_guard_registration=unavailable)
      [[ "$replacement_ready" == true ]] || \
        die 'local development Homebrew guard status is unavailable; refusing app replacement'
      result="$("$helper" --homebrew-guard-unregister 2>/dev/null)" || \
        die 'unable to remove the obsolete local development Homebrew guard registration'
      [[ "$result" == homebrew_guard_registration=not_registered ]] || \
        die 'obsolete local development Homebrew guard registration was not removed'
      guard_restore_helper="$helper"
      ;;
    *)
      die 'local development Homebrew guard status is unavailable; refusing app replacement'
      ;;
  esac
}


restore_previous_homebrew_guard_registration() {
  local helper="${guard_restore_helper:-}"
  [[ -n "$helper" ]] || return 0
  [[ -x "$helper" && ! -L "$helper" ]] || return 1
  local result
  result="$("$helper" --homebrew-guard-register 2>/dev/null)" || return 1
  [[ "$result" == homebrew_guard_registration=ready ||
     "$result" == homebrew_guard_registration=approval_required ]]
}

finish_local_dev_uninstall() {
  local status=$?
  local retain_recovery_evidence=false
  trap - EXIT HUP INT QUIT TERM
  set +e
  if ((status != 0)) && [[ "${uninstall_guard_transaction_active:-false}" == true ]]; then
    if ! restore_previous_homebrew_guard_registration; then
      printf 'CRITICAL: unable to restore the prior Homebrew guard registration after failed uninstall.\n' >&2
      status=70
    fi
  fi
  if ((status != 0)) && [[ "${source_uninstall_destructive_active:-false}" == true ]]; then
    retain_recovery_evidence=true
    printf 'CRITICAL: local-development uninstall stopped after destructive teardown began; lifecycle reservation remains active.\n' >&2
  elif [[ "${source_install_reservation_active:-false}" == true ]]; then
    if ! release_local_dev_source_install_lifecycle; then
      printf 'CRITICAL: unable to release the local-development source-uninstall lifecycle reservation.\n' >&2
      status=70
      retain_recovery_evidence=true
    fi
  fi
  if [[ "$retain_recovery_evidence" == false && -n "${tmp:-}" && -d "$tmp" && ! -L "$tmp" ]]; then
    rm -rf -- "$tmp"
  elif [[ "$retain_recovery_evidence" == true ]]; then
    printf 'CRITICAL: local-development source-uninstall lifecycle recovery helper was retained at %s.\n' \
      "${tmp:-unknown}" >&2
  fi
  exit "$status"
}
cleanup_install_workspace() {
  local status="${1:-1}" workspace="${tmp:-}"
  local retain_recovery_evidence=false
  trap - EXIT HUP INT QUIT TERM
  rm -rf -- "${staging_dir:-}" "${transaction_dir:-}"
  if [[ "${source_install_reservation_active:-false}" == true && -z "${source_install_reservation_token:-}" ]] && \
     ! load_local_dev_source_install_reservation_recovery; then
    printf 'CRITICAL: unable to recover the local-development source-install lifecycle reservation token.\n' >&2
    status=70
    retain_recovery_evidence=true
  fi
  if [[ "${source_install_reservation_active:-false}" == true && "$retain_recovery_evidence" == false ]]; then
    release_args=(
      lifecycle release-source-install --scope local_development
      --token "$source_install_reservation_token" --json
    )
    if [[ "${source_install_reservation_root_created:-false}" == true && "${pending_candidate_active:-false}" != true ]]; then
      release_args+=(--remove-created-root)
    fi
    if ! "$source_install_reservation_relayctl" "${release_args[@]}" >/dev/null; then
      printf 'CRITICAL: unable to release the local-development source-install lifecycle reservation.\n' >&2
      status=70
      retain_recovery_evidence=true
    else
      source_install_reservation_active=false
      unset OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION
      rm -f -- "${source_install_reservation_recovery_path:-}"
    fi
  fi
  if [[ "$retain_recovery_evidence" == false && -n "$workspace" && -d "$workspace" && ! -L "$workspace" ]]; then
    rm -rf -- "$workspace"
  elif [[ "$retain_recovery_evidence" == true ]]; then
    printf 'CRITICAL: local-development source-install lifecycle recovery helper was retained at %s.\n' \
      "${workspace:-unknown}" >&2
  fi
  exit "$status"
}

rollback_install() {
  local status="$1"
  local rollback_failed=false service_was_present
  [[ "${install_transaction_active:-false}" == true ]] || return "$status"
  trap - EXIT HUP INT QUIT TERM
  set +e
  if ! "$SERVICE_HELPER" stop >/dev/null 2>&1; then
    printf 'CRITICAL: unable to stop the local-development candidate before rollback.\n' >&2
    rollback_failed=true
  fi
  if ! restore_link "$APP_LINK" "${transaction_dir}/app-link"; then
    printf 'CRITICAL: unable to restore the local-development app link.\n' >&2
    rollback_failed=true
  fi
  if ! restore_previous_homebrew_guard_registration; then
    printf 'CRITICAL: unable to restore the prior Homebrew guard registration after rollback.\n' >&2
    rollback_failed=true
  fi
  for restore_spec in \
    "$BINDING_PATH|${transaction_dir}/binding|binding" \
    "$config_path|${transaction_dir}/config|configuration" \
    "${config_path}.routing-state.json|${transaction_dir}/routing-state|routing state" \
    "${config_path}.routing-initialized|${transaction_dir}/routing-initialized|routing initialization marker" \
    "${config_path}.routing-transaction.json|${transaction_dir}/routing-journal|routing journal" \
    "$SERVICE_PLIST|${transaction_dir}/service|service plist"; do
    IFS='|' read -r restore_path restore_snapshot restore_label <<<"$restore_spec"
    if ! restore_file "$restore_path" "$restore_snapshot"; then
      printf 'CRITICAL: unable to restore the prior local-development %s.\n' "$restore_label" >&2
      rollback_failed=true
    fi
  done
  if ! verify_runtime_maintenance_absence_snapshot \
    "${config_path}.runtime-maintenance.json" "${transaction_dir}/runtime-maintenance"; then
    printf 'CRITICAL: runtime maintenance appeared during local-development install; it was retained for recovery.\n' >&2
    rollback_failed=true
  fi
  if ! restore_file "$local_runtime_catalog_path" "${transaction_dir}/local-runtime-catalog"; then
    printf 'CRITICAL: unable to restore the prior local-development OpenCodex catalog.\n' >&2
    rollback_failed=true
  fi
  if ! restore_file "$local_runtime_catalog_pending_path" "${transaction_dir}/local-runtime-catalog-pending"; then
    printf 'CRITICAL: unable to restore the prior local-development OpenCodex catalog marker.\n' >&2
    rollback_failed=true
  fi
  if ! restore_file "$apple_runtime_catalog_path" "${transaction_dir}/apple-runtime-catalog"; then
    printf 'CRITICAL: unable to restore the prior local-development Apple Container catalog.\n' >&2
    rollback_failed=true
  fi
  if ! restore_file "$apple_runtime_catalog_pending_path" "${transaction_dir}/apple-runtime-catalog-pending"; then
    printf 'CRITICAL: unable to restore the prior local-development Apple Container catalog marker.\n' >&2
    rollback_failed=true
  fi
  if ! restore_link "${INSTALL_ROOT}/current" "${transaction_dir}/current"; then
    printf 'CRITICAL: unable to restore the prior local-development current target.\n' >&2
    rollback_failed=true
  fi
  service_was_present="$(sed -nE 's/^present=(true|false)$/\1/p' "${transaction_dir}/service.state")"
  if [[ "$service_was_present" != true && "$service_was_present" != false ]]; then
    printf 'CRITICAL: local-development service rollback evidence is invalid.\n' >&2
    rollback_failed=true
  elif [[ "$service_was_present" == true ]] && \
       ! "$SERVICE_HELPER" install --relay-bin "${INSTALL_ROOT}/current/opencodex-relay" --config "$config_path" >/dev/null 2>&1; then
    printf 'CRITICAL: unable to reactivate the prior local-development service.\n' >&2
    rollback_failed=true
  fi
  if [[ "$rollback_failed" == false && "${install_dir_created:-false}" == true && -n "${install_dir:-}" ]]; then
    if ! rm -rf -- "${install_dir}"; then
      printf 'CRITICAL: unable to remove the unselected local-development candidate after rollback.\n' >&2
      rollback_failed=true
    fi
  fi
  if [[ "$rollback_failed" == false && "${source_install_reservation_active:-false}" == true && \
        -z "${source_install_reservation_token:-}" ]] && \
     ! load_local_dev_source_install_reservation_recovery; then
    printf 'CRITICAL: unable to recover the local-development source-install lifecycle reservation token.\n' >&2
    status=70
    rollback_failed=true
  fi
  if [[ "$rollback_failed" == false && "${source_install_reservation_active:-false}" == true ]]; then
    release_args=(
      lifecycle release-source-install --scope local_development
      --token "$source_install_reservation_token" --json
    )
    if [[ "${source_install_reservation_root_created:-false}" == true && "${pending_candidate_active:-false}" != true ]]; then
      release_args+=(--remove-created-root)
    fi
    if ! "$source_install_reservation_relayctl" "${release_args[@]}" >/dev/null; then
      printf 'CRITICAL: unable to release the local-development source-install lifecycle reservation.\n' >&2
      status=70
      rollback_failed=true
    else
      source_install_reservation_active=false
      unset OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION
      rm -f -- "${source_install_reservation_recovery_path:-}"
    fi
  fi
  if [[ "$rollback_failed" == false ]]; then
    if ! rm -rf -- "${staging_dir:-}"; then
      printf 'CRITICAL: unable to remove local-development rollback staging.\n' >&2
      status=70
    fi
    if ! rm -rf -- "${transaction_dir:-}"; then
      printf 'CRITICAL: unable to remove completed local-development rollback evidence.\n' >&2
      status=70
    fi
  else
    status=70
    printf 'CRITICAL: rollback is incomplete; transaction evidence, candidate artifacts, and lifecycle reservation were retained at %s.\n' \
      "$transaction_dir" >&2
  fi
  if [[ "$rollback_failed" == false ]]; then
    rm -rf -- "${tmp:-}"
  fi
  exit "$status"
}

local_dev_source_install_lifecycle_capable() {
  local helper="$1" capability
  [[ -x "$helper" && -f "$helper" && ! -L "$helper" ]] || return 1
  capability="$("$helper" lifecycle source-install-capability --json 2>/dev/null)" || return 1
  jq -e '.schema_version == 2 and .state == "ready" and (keys | sort == ["schema_version", "state"])' \
    <<<"$capability" >/dev/null
}

load_local_dev_source_install_reservation_recovery() {
  local path="${source_install_reservation_recovery_path:-}" ownership
  local recovered_token recovered_root_created
  [[ -n "$path" && -f "$path" && ! -L "$path" ]] || return 1
  ownership="$(stat -f '%u:%Lp' "$path" 2>/dev/null)" || return 1
  [[ "$ownership" == "$(id -u):600" ]] || return 1
  jq -e '
    (keys | sort == ["root_created", "schema_version", "scope", "token"])
    and .schema_version == 1 and .scope == "local_development"
    and (.token | type == "string" and test("^[0-9a-f]{64}$"))
    and (.root_created | type == "boolean")
  ' "$path" >/dev/null || return 1
  recovered_token="$(jq -er '.token' "$path")" || return 1
  recovered_root_created="$(jq -er 'if .root_created then "true" else "false" end' "$path")" || return 1
  source_install_reservation_token="$recovered_token"
  source_install_reservation_root_created="$recovered_root_created"
}

select_local_dev_source_install_lifecycle_helper() {
  local preferred="$1" candidate
  for candidate in \
    "$preferred" \
    "${INSTALL_ROOT}/current/opencodex-relayctl" \
    "${HOME}/.local/lib/opencodex-relay/relay/current/opencodex-relayctl"; do
    [[ -n "$candidate" ]] || continue
    if local_dev_source_install_lifecycle_capable "$candidate"; then
      printf '%s\n' "$candidate"
      return 0
    fi
  done
  return 1
}

reserve_local_dev_source_install_lifecycle() {
  local preferred_helper="${1:-${relayctl_bin:-}}" selected_helper reservation_json
  selected_helper="$(select_local_dev_source_install_lifecycle_helper "$preferred_helper")" || \
    die 'a lifecycle-capable installed or target relayctl is required for local-development installation and removal'
  source_install_reservation_relayctl="${tmp}/reservation-relayctl"
  cp -p -- "$selected_helper" "$source_install_reservation_relayctl"
  chmod 0700 "$source_install_reservation_relayctl"
  source_install_reservation_recovery_path="${tmp}/source-install-reservation.json"
  source_install_reservation_active=true
  reservation_json="$("$source_install_reservation_relayctl" \
    lifecycle reserve-source-install --scope local_development \
    --recovery-file "$source_install_reservation_recovery_path" --json)" || \
    die 'unable to reserve the local-development source-install lifecycle'
  load_local_dev_source_install_reservation_recovery || \
    die 'source-install durable recovery response is invalid'
  jq -e --arg token "$source_install_reservation_token" \
    --argjson root_created "$source_install_reservation_root_created" '
    (keys | sort == ["root_created", "schema_version", "scope", "token"])
    and .schema_version == 1 and .scope == "local_development"
    and .token == $token and .root_created == $root_created
  ' <<<"$reservation_json" >/dev/null || die 'source-install reservation response is invalid'
  export OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION="$source_install_reservation_token"
}

clear_local_dev_install_root_preserving_reservation() {
  local entry
  local -a entries=()
  [[ -d "$INSTALL_ROOT" && ! -L "$INSTALL_ROOT" ]] || return 1
  shopt -s dotglob nullglob
  entries=("$INSTALL_ROOT"/*)
  shopt -u dotglob nullglob
  for entry in "${entries[@]}"; do
    [[ "$(basename -- "$entry")" == "$SOURCE_INSTALL_RESERVATION_NAME" ]] && continue
    rm -rf -- "$entry" || return 1
  done
}

release_local_dev_source_install_lifecycle() {
  [[ "${source_install_reservation_active:-false}" == true ]] || return 0
  if [[ -z "${source_install_reservation_token:-}" ]] && ! load_local_dev_source_install_reservation_recovery; then
    return 1
  fi
  if ! "$source_install_reservation_relayctl" lifecycle release-source-install \
    --scope local_development --token "$source_install_reservation_token" --json >/dev/null; then
    return 1
  fi
  source_install_reservation_active=false
  unset OPENCODEX_RELAY_SOURCE_INSTALL_RESERVATION
  rm -f -- "${source_install_reservation_recovery_path:-}"
}

install_local_dev() {
  local version="${1:-}"
  shift || true
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die 'VERSION must be explicit semver'
  local source_dir="" upstream="" keychain_service="" acknowledge_development_source=false acknowledge_source=false
  local source_install_reservation_active=false source_install_reservation_token=""
  local source_install_reservation_root_created=false source_install_reservation_relayctl=""
  local source_install_reservation_recovery_path=""
  local source_uninstall_destructive_active=false
  local local_runtime_catalog_path="" local_runtime_catalog_pending_path=""
  local apple_runtime_catalog_path="" apple_runtime_catalog_pending_path=""
  config_path="$DEFAULT_CONFIG"
  local codex_config="$DEFAULT_CODEX_CONFIG" catalog_path="" codex_executable=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --source-dir) source_dir="${2:-}"; shift 2 ;;
      --upstream) upstream="${2:-}"; shift 2 ;;
      --keychain-service) keychain_service="${2:-}"; shift 2 ;;
      --acknowledge-local-development-source) acknowledge_development_source=true; shift ;;
      # Deprecated compatibility alias.
      --acknowledge-unsigned-local-build) acknowledge_development_source=true; shift ;;
      --acknowledge-local-source) acknowledge_source=true; shift ;;
      --config) config_path="${2:-}"; shift 2 ;;
      --codex-config) codex_config="${2:-}"; shift 2 ;;
      --catalog-path) catalog_path="${2:-}"; shift 2 ;;
      --codex-executable) codex_executable="${2:-}"; shift 2 ;;
      *) usage >&2; die "unknown argument: $1" ;;
    esac
  done
  [[ "$acknowledge_development_source" == true ]] || die '--acknowledge-local-development-source is required'
  [[ "$upstream" =~ ^https://[^/?#]+/v1$ ]] || die '--upstream must be an HTTPS /v1 URL'
  [[ -n "$source_dir" && -d "$source_dir" && ! -L "$source_dir" ]] || die '--source-dir must be a regular local directory'
  [[ -z "$keychain_service" || "$acknowledge_source" == false ]] || die 'choose either --keychain-service or --acknowledge-local-source'
  [[ -n "$keychain_service" || "$acknowledge_source" == true ]] || die 'choose --acknowledge-local-source or --keychain-service'
  [[ -z "$keychain_service" ]] || require_keychain_service "$keychain_service"
  [[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || die 'local development installs require macOS Apple Silicon'
  for command in openssl base64 jq shasum ditto unzip codesign curl diff; do command -v "$command" >/dev/null || die "$command is required"; done
  [[ -x "$SERVICE_HELPER" || -f "$SERVICE_HELPER" ]] || die 'local development service helper is unavailable'
  [[ -f "$KEYCHAIN_HELPER" && ! -L "$KEYCHAIN_HELPER" ]] || die 'local development Keychain helper is unavailable'
  require_local_dev_config_path "$config_path"
  safe_absolute_path "$codex_config" || die '--codex-config must be a clean absolute path'
  local expected_catalog_path="$(dirname -- "$codex_config")/opencodex-relay-dev-external-catalog.json"
  if [[ -z "$catalog_path" ]]; then catalog_path="$expected_catalog_path"; fi
  safe_absolute_path "$catalog_path" || die '--catalog-path must be a clean absolute path'
  [[ "$catalog_path" == "$expected_catalog_path" ]] || \
    die '--catalog-path must stay in the selected Codex home local-development namespace'
  require_local_dev_config_leaves_or_absent "$config_path"
  require_regular_or_absent "$codex_config"
  if [[ -e "$BINDING_PATH" || -L "$BINDING_PATH" ]]; then
    require_regular_or_absent "$BINDING_PATH"
    require_owner_mode_600_if_present "$BINDING_PATH"
  fi
  require_managed_dev_link_or_absent "${INSTALL_ROOT}/current"
  require_managed_dev_link_or_absent "$APP_LINK"

  local tmp
  local original_umask
  original_umask="$(umask)"
  umask 077
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-local-dev.XXXXXX")" || {
    umask "$original_umask"
    die 'unable to create local development source snapshot'
  }
  trap 'cleanup_install_workspace $?' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  umask "$original_umask"
  chmod 0700 "$tmp" || die 'unable to protect local development source snapshot'

  local source_snapshot="${tmp}/source"
  mkdir -p "$source_snapshot"
  chmod 0700 "$source_snapshot" || die 'unable to protect local development source snapshot'
  local manifest="${source_snapshot}/local-dev-manifest-${version}.json"
  local signature="${source_snapshot}/local-dev-manifest-${version}.sig"
  local bundle="${source_snapshot}/${APP_ZIP}"
  local notices="${source_snapshot}/${NOTICES_FILE}"
  snapshot_local_source_file "${source_dir}/local-dev-manifest-${version}.json" "$manifest" "local-dev-manifest-${version}.json"
  snapshot_local_source_file "${source_dir}/local-dev-manifest-${version}.sig" "$signature" "local-dev-manifest-${version}.sig"
  snapshot_local_source_file "${source_dir}/${APP_ZIP}" "$bundle" "$APP_ZIP"
  snapshot_local_source_file "${source_dir}/${NOTICES_FILE}" "$notices" "$NOTICES_FILE"
  local public_key
  if [[ -n "$keychain_service" ]]; then
    public_key="${tmp}/trusted-public.pem"
  else
    public_key="${source_snapshot}/local-dev-public-key.pem"
    snapshot_local_source_file "${source_dir}/local-dev-public-key.pem" "$public_key" 'local-dev-public-key.pem'
  fi

  manifest_valid "$manifest" "$version" || die 'local development manifest is invalid or contains production metadata'
  if [[ -n "$keychain_service" ]]; then
    (umask 077; swift "$KEYCHAIN_HELPER" read "$keychain_service" > "$public_key") 2>/dev/null || die 'Keychain local development trust key is unavailable'
    [[ -f "$public_key" && ! -L "$public_key" ]] || die 'Keychain local development trust key is unsafe'
    chmod 0600 "$public_key" || die 'unable to protect Keychain local development trust key'
    require_owner_mode_600_if_present "$public_key"
  fi
  require_ed25519_public_key "$public_key"
  decode_base64 "$signature" "${tmp}/manifest.sig.bin"
  openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin -in "$manifest" -sigfile "${tmp}/manifest.sig.bin" >/dev/null || die 'local development manifest signature is invalid'
  [[ "$(sha256 "$notices")" == "$(jq -er '.documents[0].sha256' "$manifest")" ]] || die 'local development notices checksum does not match manifest'
  staging_dir="${tmp}/bundle"
  local manifest_schema="$(jq -er '.schema' "$manifest")"
  verify_bundle_shape "$bundle" "$(jq -er '.artifacts[0].sha256' "$manifest")" "$staging_dir" "$manifest_schema"
  local app="${staging_dir}/${APP_NAME}"
  local pending_candidate_active=false pending_candidate_dir=""
  local helper_dir="${app}/Contents/Library/Helpers"
  local relay_bin="${helper_dir}/opencodex-relay"
  local relayctl_bin="${helper_dir}/opencodex-relayctl"

  # Reserve the fixed dev root before pending-helper, config, runtime, binding,
  # or LaunchAgent writes. The marker is interpreted under the same user
  # lifecycle lock as integration and standalone removal.
  reserve_local_dev_source_install_lifecycle
  if [[ "$manifest_schema" == 3 ]]; then
    ensure_local_dev_install_root
    prepare_manual_helper_candidate \
      "$version" "$app" "$(jq -er '.artifacts[0].sha256' "$manifest")"
  fi
  ensure_local_dev_config_parent "$config_path"
  require_local_dev_config_leaves_or_absent "$config_path"
  ensure_local_dev_install_root

  transaction_dir="$(mktemp -d "${INSTALL_ROOT}/.transaction.${version}.XXXXXX")"
  staging_dir="${INSTALL_ROOT}/.stage.${version}.XXXXXX"
  mkdir -p "$staging_dir"
  # Keep unenrolled catalog rollback slots transaction-local. An enrolled
  # profile is strict-loaded again by relayctl before it can be preserved, but
  # resolve its fixed, non-secret catalog leaf now so every candidate-start
  # mutation has a rollback snapshot before the transaction trap is armed.
  local_runtime_catalog_path="${transaction_dir}/unconfigured-local-catalog"
  apple_runtime_catalog_path="${transaction_dir}/unconfigured-apple-catalog"
  if [[ -f "$config_path" ]]; then
    local configured_runtime_catalog configured_apple_runtime_catalog
    configured_runtime_catalog="$(jq -er '.local_opencodex.catalog_path // empty' "$config_path" 2>/dev/null || true)"
    if [[ -n "$configured_runtime_catalog" ]]; then
      [[ "$(basename -- "$configured_runtime_catalog")" == opencodex-relay-dev-local-catalog.json ]] && \
        safe_absolute_path "$configured_runtime_catalog" || \
        die 'existing local development OpenCodex catalog path is unsafe'
      local_runtime_catalog_path="$configured_runtime_catalog"
    fi
    configured_apple_runtime_catalog="$(jq -er '.local_apple_container.catalog_path // empty' "$config_path" 2>/dev/null || true)"
    if [[ -n "$configured_apple_runtime_catalog" ]]; then
      [[ "$(basename -- "$configured_apple_runtime_catalog")" == opencodex-relay-dev-apple-container-catalog.json ]] && \
        safe_absolute_path "$configured_apple_runtime_catalog" || \
        die 'existing local development Apple Container catalog path is unsafe'
      apple_runtime_catalog_path="$configured_apple_runtime_catalog"
    fi
  fi
  local_runtime_catalog_pending_path="${local_runtime_catalog_path}.restart-pending"
  apple_runtime_catalog_pending_path="${apple_runtime_catalog_path}.restart-pending"
  snapshot_file "$config_path" "${transaction_dir}/config"
  snapshot_file "${config_path}.routing-state.json" "${transaction_dir}/routing-state"
  snapshot_file "${config_path}.routing-initialized" "${transaction_dir}/routing-initialized"
  snapshot_file "${config_path}.routing-transaction.json" "${transaction_dir}/routing-journal"
  snapshot_runtime_maintenance_absence \
    "${config_path}.runtime-maintenance.json" "${transaction_dir}/runtime-maintenance"
  snapshot_file "$local_runtime_catalog_path" "${transaction_dir}/local-runtime-catalog"
  snapshot_file "$local_runtime_catalog_pending_path" "${transaction_dir}/local-runtime-catalog-pending"
  snapshot_file "$apple_runtime_catalog_path" "${transaction_dir}/apple-runtime-catalog"
  snapshot_file "$apple_runtime_catalog_pending_path" "${transaction_dir}/apple-runtime-catalog-pending"
  snapshot_file "$BINDING_PATH" "${transaction_dir}/binding"
  snapshot_file "$SERVICE_PLIST" "${transaction_dir}/service"
  snapshot_link "${INSTALL_ROOT}/current" "${transaction_dir}/current"
  snapshot_link "$APP_LINK" "${transaction_dir}/app-link"
  install_transaction_active=true
  trap 'rollback_install $?' EXIT

  if [[ -f "$config_path" ]]; then
    jq -e --arg upstream "$upstream" --arg catalog "$catalog_path" '
      (.installation_scope == "local_development") and .listen_address == "127.0.0.1:18190"
      and .responses.scheduler.interactive_listen_address == "127.0.0.1:18192"
      and .upstream_mode == "external_gateway" and .upstream_base_url == $upstream
      and .catalog.path == $catalog
    ' "$config_path" >/dev/null || die 'existing config is not a compatible local_development relay config'
  else
    init_args=(init --upstream "$upstream" --credentials keychain --listen 127.0.0.1:18190 --interactive-listen 127.0.0.1:18192 --installation-scope local_development --catalog-path "$catalog_path" --config "$config_path")
    [[ -z "$codex_executable" ]] || init_args+=(--codex-executable "$codex_executable")
    "$relayctl_bin" "${init_args[@]}"
  fi
  local install_routing_state="native_parked"
  local expected_runtime_profile="native_parked"
  if ! "$relayctl_bin" mode seed-native --config "$config_path" --codex-config "$codex_config" --json >/dev/null 2>&1; then
    local preserved_runtime_profile=""
    if preserved_runtime_profile="$(active_local_dev_runtime_is_acknowledged "$config_path" "$relayctl_bin" "$codex_config")"; then
      case "$preserved_runtime_profile" in
        local_opencodex|local_apple_container)
          expected_runtime_profile="$preserved_runtime_profile"
          install_routing_state="${preserved_runtime_profile}_preserved"
          ;;
        *) die 'existing local development routing state returned an unsupported Local runtime profile' ;;
      esac
    else
      # An existing local-development bundle may also be upgraded specifically
      # so its Control Center can repair an orphaned recovery epoch. Never
      # reseed or rewrite that epoch. Accept it only when the new helper
      # independently validates the exact bound status and value-free
      # native-repair inspection.
      local recovery_status="${tmp}/recovery-status.json"
      local repair_inspection="${tmp}/native-repair-inspection.json"
      "$relayctl_bin" mode status --config "$config_path" --codex-config "$codex_config" --json > "$recovery_status" || \
        die 'existing local development routing state is not upgradeable'
      local recovery_generation
      recovery_generation="$(jq -er '
        select(.schema_version == 4 and .phase == "recovery_required"
          and .generation > 0 and .relay_admission == "deny"
          and .catalog_refresh == "pause") | .generation
      ' "$recovery_status")" || die 'existing local development routing state is not safely parked for repair'
      "$relayctl_bin" mode inspect-native-repair \
        --expected-routing-generation "$recovery_generation" \
        --config "$config_path" --codex-config "$codex_config" --json > "$repair_inspection" || \
        die 'existing local development recovery state is not eligible for bounded native repair'
      jq -e --argjson generation "$recovery_generation" '
        .schema_version == 1 and .generation == $generation
        and .phase == "recovery_required"
        and (.kind == "state_only" or .kind == "local_relay"
          or .kind == "opencodex" or .kind == "unavailable")
        and (.openai_base_url | type == "boolean")
        and (.model_catalog_json | type == "boolean")
        and (.reason | type == "string")
      ' "$repair_inspection" >/dev/null || \
        die 'existing local development native repair inspection is invalid'
      install_routing_state="recovery_preserved"
    fi
  fi

  local install_dir="${INSTALL_ROOT}/${version}/darwin-arm64"
  [[ ! -e "$install_dir" && ! -L "$install_dir" ]] || die 'local development version directory already exists'
  mkdir -p "$(dirname -- "$install_dir")"
  if [[ "$pending_candidate_active" == true ]]; then
    ditto "$app" "$staging_dir/${APP_NAME}"
  else
    mv "$app" "$staging_dir/${APP_NAME}"
  fi
  mkdir -p "$install_dir"
	install_dir_created=true
  mv "$staging_dir/${APP_NAME}" "$install_dir/${APP_NAME}"
  local current_candidate="${INSTALL_ROOT}/.current.${version}.$$"
  ln -s "${version}/darwin-arm64/${APP_NAME}/Contents/Library/Helpers" "$current_candidate"
  mv -fh "$current_candidate" "${INSTALL_ROOT}/current"
  "$SERVICE_HELPER" install --relay-bin "${INSTALL_ROOT}/current/opencodex-relay" --config "$config_path"
  case "$expected_runtime_profile" in
    native_parked) wait_for_parked_health "$config_path" ;;
    local_opencodex|local_apple_container)
      wait_for_active_local_dev_runtime_health \
        "$config_path" "$relayctl_bin" "$codex_config" "$expected_runtime_profile"
      ;;
    *) die 'local development installer lost its expected routing health profile' ;;
  esac

  if [[ -e "$BINDING_DIR" || -L "$BINDING_DIR" ]]; then
    [[ -d "$BINDING_DIR" && ! -L "$BINDING_DIR" ]] || die 'local development binding directory is unsafe'
  else
    mkdir -p "$BINDING_DIR"
  fi
  mkdir -p "$(dirname -- "$APP_LINK")"
  chmod 0700 "$BINDING_DIR"
  local app_candidate="$(dirname -- "$APP_LINK")/.${APP_NAME}.candidate.$$"
  ln -s "${install_dir}/${APP_NAME}" "$app_candidate"
  local binding_candidate
  binding_candidate="$(mktemp "${BINDING_PATH}.XXXXXX")"
  jq -n --arg relay "$config_path" --arg codex "$codex_config" '{schema:1, relay_config:$relay, codex_config:$codex}' > "$binding_candidate"
  chmod 0600 "$binding_candidate"
  prepare_existing_homebrew_guard_for_replacement "${install_dir}/${APP_NAME}"
  mv -fh "$app_candidate" "$APP_LINK"
  mv -f -- "$binding_candidate" "$BINDING_PATH"

  # Disable rollback before releasing admission while catchable signals are
  # ignored. A failed release is retried by the EXIT handler without reverting
  # an already verified candidate through an unreserved service helper.
  trap '' HUP INT QUIT TERM
  install_transaction_active=false
  local source_install_release_status=0
  release_local_dev_source_install_lifecycle || source_install_release_status=$?
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  ((source_install_release_status == 0)) || \
    die 'unable to release the local-development source-install lifecycle reservation'
  trap - EXIT HUP INT QUIT TERM
  rm -rf -- "$tmp" "$staging_dir" "$transaction_dir"
  if [[ "$pending_candidate_active" == true ]]; then
    rm -rf -- "$pending_candidate_dir"
    rmdir "$PENDING_ROOT" 2>/dev/null || true
  fi
  printf 'local_dev_installed=true version=%s routing=%s distribution=local_development\n' "$version" "$install_routing_state"
  printf 'Open the app manually and approve macOS execution if requested. Login-item registration is optional and is never performed by this installer.\n'
}

trust_command() {
  local action="${1:-}"
  shift || true
  local service="" public_key="" expected="" old=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --keychain-service) service="${2:-}"; shift 2 ;;
      --public-key) public_key="${2:-}"; shift 2 ;;
      --expected-fingerprint) expected="${2:-}"; shift 2 ;;
      --old-fingerprint) old="${2:-}"; shift 2 ;;
      *) usage >&2; die "unknown argument: $1" ;;
    esac
  done
  [[ "$action" == enroll || "$action" == replace ]] || die 'trust requires enroll or replace'
  require_keychain_service "$service"
  [[ -f "$public_key" && ! -L "$public_key" ]] || die '--public-key must be a regular PEM file'
  [[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die '--expected-fingerprint must be lowercase SHA-256'
  require_ed25519_public_key "$public_key"
  [[ "$(sha256 "$public_key")" == "$expected" ]] || die '--expected-fingerprint does not match the supplied public key'
  command -v swift >/dev/null || die 'swift is required for Keychain trust enrollment'
  [[ -f "$KEYCHAIN_HELPER" && ! -L "$KEYCHAIN_HELPER" ]] || die 'local development Keychain helper is unavailable'
  case "$action" in
    enroll)
      [[ -z "$old" ]] || die '--old-fingerprint is valid only for trust replace'
      swift "$KEYCHAIN_HELPER" enroll "$service" "$public_key" "$expected"
      ;;
    replace)
      [[ "$old" =~ ^[0-9a-f]{64}$ ]] || die '--old-fingerprint must be lowercase SHA-256'
      swift "$KEYCHAIN_HELPER" replace "$service" "$old" "$public_key" "$expected"
      ;;
  esac
  printf 'local_dev_trust=%s service=%s fingerprint=%s\n' "$action" "$service" "$expected"
}

uninstall_local_dev() {
  config_path="$DEFAULT_CONFIG"
  local codex_config="$DEFAULT_CODEX_CONFIG" confirmed=false
  local source_install_reservation_active=false source_install_reservation_token=""
  local source_install_reservation_root_created=false source_install_reservation_relayctl=""
  local source_install_reservation_recovery_path=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --config) config_path="${2:-}"; shift 2 ;;
      --codex-config) codex_config="${2:-}"; shift 2 ;;
      --confirm-desktop-exited) confirmed=true; shift ;;
      *) usage >&2; die "unknown argument: $1" ;;
    esac
  done
  require_local_dev_config_path "$config_path"
  safe_absolute_path "$codex_config" || die '--codex-config must be a clean absolute path'

  require_local_dev_uninstall_artifacts_safe "$config_path"
  if [[ ! -f "$config_path" ]]; then
    if local_dev_uninstall_artifacts_present "$config_path" || local_dev_service_is_active; then
      die 'local development install is orphaned without its config; refusing to delete artifacts'
    fi
    printf 'local_dev_uninstalled=true\n'
    return 0
  fi

  [[ -x "$SERVICE_HELPER" && -f "$SERVICE_HELPER" && ! -L "$SERVICE_HELPER" ]] || \
    die 'local development service helper is unavailable or unsafe; refusing uninstall'
  jq -e '.installation_scope == "local_development"' "$config_path" >/dev/null || \
    die 'refusing to uninstall a non-local-development config'

  local relayctl="${INSTALL_ROOT}/current/opencodex-relayctl"
  require_managed_local_dev_relayctl "$relayctl"

  local original_umask
  original_umask="$(umask)"
  umask 077
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-local-dev-uninstall.XXXXXX")" || {
    umask "$original_umask"
    die 'unable to create the local-development source-uninstall lifecycle workspace'
  }
  umask "$original_umask"
  chmod 0700 "$tmp" || die 'unable to protect the local-development source-uninstall lifecycle workspace'
  trap finish_local_dev_uninstall EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  reserve_local_dev_source_install_lifecycle "$relayctl"

  local status applied
  status="$("$relayctl" mode status --config "$config_path" --codex-config "$codex_config" --json)" || \
    die 'inspect local development routing before uninstall'
  applied="$(jq -er '.applied_backend | select(type == "string")' <<< "$status")" || \
    die 'local development routing status is invalid; refusing uninstall'
  if [[ "$applied" != none ]]; then
    [[ "$confirmed" == true ]] || die 'close the selected Codex Desktop app and pass --confirm-desktop-exited before removing active local-development routing'
    "$relayctl" mode request native --config "$config_path" --codex-config "$codex_config" >/dev/null
    "$relayctl" mode apply --confirm-desktop-exited --config "$config_path" --codex-config "$codex_config" >/dev/null
  fi

  # The status projection is not sufficient proof of native ownership. The
  # controller verifies the local-dev scope, state binding, no journal, and
  # absence of both dev and foreign routing artifacts before any deletion.
  "$relayctl" mode verify-native --config "$config_path" --codex-config "$codex_config" --json >/dev/null || \
    die 'local development routing is not verified native; refusing uninstall'
  require_local_dev_uninstall_artifacts_safe "$config_path"
  [[ -f "$config_path" ]] || die 'local development config disappeared during verification; refusing uninstall'
  jq -e '.installation_scope == "local_development"' "$config_path" >/dev/null || \
    die 'local development config changed during verification; refusing uninstall'
  require_managed_local_dev_relayctl "$relayctl"
  uninstall_guard_transaction_active=true
  source_uninstall_destructive_active=true
  prepare_existing_homebrew_guard_for_replacement "$APP_LINK"

  local service_status service_was_active=false
  service_status="$("$SERVICE_HELPER" status)" || \
    die 'unable to inspect local development service before uninstall'
  case "$service_status" in
    'relay_dev_service_active=true manager=launchd') service_was_active=true ;;
    'relay_dev_service_active=false manager=launchd') ;;
    *) die 'local development service status is invalid; refusing uninstall' ;;
  esac
  "$SERVICE_HELPER" stop >/dev/null || die 'unable to stop local development service before uninstall'

  # Verify once more after stopping the watcher. A concurrent route mutation
  # must not turn the native proof into deletion of a live relay override.
  if ! "$relayctl" mode verify-native --config "$config_path" --codex-config "$codex_config" --json >/dev/null; then
    if [[ "$service_was_active" == true ]]; then
      [[ -x "${INSTALL_ROOT}/current/opencodex-relay" && -f "${INSTALL_ROOT}/current/opencodex-relay" && \
         ! -L "${INSTALL_ROOT}/current/opencodex-relay" && -f "$config_path" && ! -L "$config_path" ]] || \
        die 'native verification changed after service stop and the prior service cannot be safely restored'
      "$SERVICE_HELPER" install --relay-bin "${INSTALL_ROOT}/current/opencodex-relay" --config "$config_path" >/dev/null || \
        die 'native verification changed after service stop and the prior service could not be restored'
    fi
    die 'local development routing is not verified native after service stop; refusing uninstall'
  fi
  require_local_dev_uninstall_artifacts_safe "$config_path"
  [[ -f "$config_path" ]] || die 'local development config disappeared after service stop; refusing uninstall'
  jq -e '.installation_scope == "local_development"' "$config_path" >/dev/null || \
    die 'local development config changed after service stop; refusing uninstall'
  require_managed_local_dev_relayctl "$relayctl"

  "$SERVICE_HELPER" uninstall
  if [[ -e "$APP_LINK" || -L "$APP_LINK" ]]; then
    require_managed_dev_link_or_absent "$APP_LINK"
    rm -f -- "$APP_LINK"
  fi
  if [[ -e "$BINDING_PATH" || -L "$BINDING_PATH" ]]; then
    require_regular_or_absent "$BINDING_PATH"
    rm -f -- "$BINDING_PATH"
  fi
  rmdir "$BINDING_DIR" 2>/dev/null || true
  rm -f -- "$config_path" "${config_path}.routing-state.json" "${config_path}.routing-initialized" \
    "${config_path}.routing-transaction.json" "${config_path}.runtime-maintenance.json"
  if [[ -e "$INSTALL_ROOT" || -L "$INSTALL_ROOT" ]]; then
    [[ -d "$INSTALL_ROOT" && ! -L "$INSTALL_ROOT" ]] || die 'local development install root is unsafe'
    clear_local_dev_install_root_preserving_reservation || \
      die 'unable to clear the local development install root while retaining lifecycle admission'
  fi
  guard_restore_helper=""
  uninstall_guard_transaction_active=false
  trap '' HUP INT QUIT TERM
  source_uninstall_destructive_active=false
  local source_install_release_status=0
  release_local_dev_source_install_lifecycle || source_install_release_status=$?
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 131' QUIT
  trap 'exit 143' TERM
  ((source_install_release_status == 0)) || \
    die 'unable to release the local-development source-uninstall lifecycle reservation'
  rmdir "$INSTALL_ROOT" 2>/dev/null || true
  rm -rf -- "$tmp"
  trap - EXIT HUP INT QUIT TERM
  printf 'local_dev_uninstalled=true\n'
}

action="${1:-}"
shift || true
case "$action" in
  trust) trust_command "$@" ;;
  install) install_local_dev "$@" ;;
  uninstall) uninstall_local_dev "$@" ;;
  *) usage >&2; exit 2 ;;
esac

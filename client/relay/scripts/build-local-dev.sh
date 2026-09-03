#!/usr/bin/env bash
# Build an offline-only, ad-hoc-signed macOS development bundle. This is not a
# production release builder and intentionally has no URL, notarization, or
# Gatekeeper-assessment path.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELAY_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly REPO_ROOT="$(cd -- "${RELAY_ROOT}/../.." && pwd -P)"
readonly KEYCHAIN_HELPER="${SCRIPT_DIR}/keychain-signing-key.swift"
readonly APP_ROOT="${RELAY_ROOT}/macos/OpenCodexRelay"
readonly INFO_TEMPLATE="${APP_ROOT}/Resources/Info.local-dev.plist"
readonly APP_ICON="${APP_ROOT}/Resources/AppIcon.icns"
readonly RUNTIME_PUBLIC_KEY_SOURCE="${REPO_ROOT}/config/trust/opencodex-runtime-release-ed25519.pub"
readonly INFO_LOCALIZATIONS="${APP_ROOT}/Resources/InfoPlist.local-dev"
readonly APP_NAME="OpenCodexRelay Dev.app"
readonly BUNDLE_FILE="${APP_NAME}.zip"
readonly BUNDLE_ID="io.github.novelkr.opencodex-relay.dev"
readonly GUARD_HELPER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.helper.dev"
readonly GUARD_HELPER_NAME="OpenCodexRelayPrivilegedHelper"
readonly GUARD_INSTALLER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.installer.dev"
readonly GUARD_INSTALLER_NAME="OpenCodexRelayHelperInstaller"
readonly GUARD_MANUAL_SERVICE="io.github.novelkr.opencodex-relay.homebrew-guard.manual.dev"
readonly TRUSTED_CODEX_BUNDLE_ID="com.openai.codex"
readonly TRUSTED_CODEX_TEAM_ID="2DC432GLL2"
readonly NOTICES_FILE="THIRD_PARTY_NOTICES.md"
readonly LOCALIZATION_BUNDLE="OpenCodexRelay_OpenCodexRelayLocalization.bundle"

usage() {
  cat <<'USAGE'
Usage:
  build-local-dev.sh VERSION (--signing-key PEM | --signing-key-keychain-service SERVICE) --output DIR [--swift-disable-sandbox]

Creates a macOS arm64 local-only development source directory. The source tree
must be a clean Git commit. The app and its helpers receive ad-hoc signatures;
the output is not notarized and is never suitable for the production installer.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

plist_string() {
  local plist="$1"
  local key="$2"
  if [[ -x /usr/libexec/PlistBuddy ]]; then
    /usr/libexec/PlistBuddy -c "Print :${key}" "$plist"
    return
  fi
  command -v python3 >/dev/null || return 1
  python3 - "$plist" "$key" <<'PY'
import plistlib
import sys

with open(sys.argv[1], "rb") as stream:
    value = plistlib.load(stream).get(sys.argv[2])
if not isinstance(value, str):
    raise SystemExit(1)
print(value)
PY
}

verify_reviewed_codex_identity() {
  local plist="$1"
  [[ "$(plist_string "$plist" OpenCodexTrustedCodexBundleIdentifier 2>/dev/null)" == "$TRUSTED_CODEX_BUNDLE_ID" ]] || \
    die 'local development bundle has an unreviewed Codex Desktop bundle identifier'
  [[ "$(plist_string "$plist" OpenCodexTrustedCodexTeamIdentifier 2>/dev/null)" == "$TRUSTED_CODEX_TEAM_ID" ]] || \
    die 'local development bundle has an unreviewed Codex Desktop Team ID'
}

sha256() {
  if command -v shasum >/dev/null; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

codesign_cdhash() {
  local value
  value="$(codesign -dvvv "$1" 2>&1 | sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p')"
  [[ "$value" =~ ^[0-9a-f]{40,128}$ ]] || die "signed component has no unique CDHash: $1"
  printf '%s\n' "$value"
}

require_ed25519_private_key() {
  openssl pkey -in "$1" -text -noout 2>/dev/null | grep -Eq '^ED25519 Private-Key:' || \
    die 'local development signing key must be an Ed25519 private PEM'
}

[[ $# -ge 1 ]] || { usage >&2; exit 2; }
version="$1"
shift
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die 'VERSION must be explicit semver'
bundle_short_version="$version"
bundle_build_version="${version%%-*}"

signing_key=""
signing_key_service=""
output_dir=""
swift_disable_sandbox=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --signing-key) signing_key="${2:-}"; shift 2 ;;
    --signing-key-keychain-service) signing_key_service="${2:-}"; shift 2 ;;
    --output) output_dir="${2:-}"; shift 2 ;;
    --swift-disable-sandbox) swift_disable_sandbox=true; shift ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ -n "$output_dir" ]] || die '--output is required'
[[ -z "$signing_key" || -z "$signing_key_service" ]] || die '--signing-key and --signing-key-keychain-service are mutually exclusive'
[[ -n "$signing_key" || -n "$signing_key_service" ]] || die 'one signing-key source is required'
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || die 'local development builds require macOS Apple Silicon'
command -v go >/dev/null || die 'Go toolchain is required'
command -v swift >/dev/null || die 'Swift toolchain is required'
command -v codesign >/dev/null || die 'codesign is required for ad-hoc signing'
command -v ditto >/dev/null || die 'ditto is required'
command -v plutil >/dev/null || die 'plutil is required'
command -v openssl >/dev/null || die 'openssl is required'
command -v base64 >/dev/null || die 'base64 is required'
[[ -f "$INFO_TEMPLATE" && ! -L "$INFO_TEMPLATE" ]] || die 'local development Info.plist template is unavailable'
[[ -f "$APP_ICON" && ! -L "$APP_ICON" ]] || die 'local development app icon is unavailable'
[[ -f "$RUNTIME_PUBLIC_KEY_SOURCE" && ! -L "$RUNTIME_PUBLIC_KEY_SOURCE" ]] || \
  die 'tracked runtime release public key is unavailable'
openssl pkey -pubin -in "$RUNTIME_PUBLIC_KEY_SOURCE" -text -noout 2>/dev/null | grep -Eq '^ED25519 Public-Key:' || \
  die 'tracked runtime release public key must be provisioned as an Ed25519 public PEM'
[[ -d "$INFO_LOCALIZATIONS" && ! -L "$INFO_LOCALIZATIONS" ]] || die 'local development InfoPlist localization resources are unavailable'
[[ -f "${RELAY_ROOT}/${NOTICES_FILE}" && ! -L "${RELAY_ROOT}/${NOTICES_FILE}" ]] || die 'third-party notices are unavailable'

verify_reviewed_codex_identity "$INFO_TEMPLATE"
[[ -z "$(git -C "$REPO_ROOT" status --porcelain --untracked-files=all)" ]] || \
  die 'local development builds require a clean Git worktree'
source_commit="$(git -C "$REPO_ROOT" rev-parse --verify HEAD)" || die 'resolve source commit'
[[ "$source_commit" =~ ^[0-9a-f]{40}$ ]] || die 'source commit is invalid'

key_temp=""
bundle_temp=""
cleanup() {
  [[ -z "$key_temp" ]] || rm -rf -- "$key_temp"
  [[ -z "$bundle_temp" ]] || rm -rf -- "$bundle_temp"
}
trap cleanup EXIT

if [[ -n "$signing_key_service" ]]; then
  [[ "$signing_key_service" != *$'\n'* && "$signing_key_service" != *$'\r'* ]] || die 'keychain service is unsafe'
  [[ -f "$KEYCHAIN_HELPER" && ! -L "$KEYCHAIN_HELPER" ]] || die 'Keychain signing-key helper is unavailable'
  key_temp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-local-dev-key.XXXXXX")"
  signing_key="${key_temp}/ed25519.pem"
  umask 077
  swift "$KEYCHAIN_HELPER" read "$signing_key_service" > "$signing_key" 2>/dev/null || die 'Keychain signing key is unavailable'
  chmod 0600 "$signing_key"
else
  [[ -f "$signing_key" && ! -L "$signing_key" ]] || die '--signing-key must be a regular PEM file'
fi
require_ed25519_private_key "$signing_key"

mkdir -p "$output_dir"
[[ -d "$output_dir" && ! -L "$output_dir" ]] || die '--output must be a non-symlink directory'
for file in "$BUNDLE_FILE" "local-dev-manifest-${version}.json" "local-dev-manifest-${version}.sig" "local-dev-public-key.pem" "$NOTICES_FILE"; do
  [[ ! -e "${output_dir}/${file}" && ! -L "${output_dir}/${file}" ]] || die "output already contains ${file}"
done

bundle_temp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-local-dev-bundle.XXXXXX")"
app_dir="${bundle_temp}/${APP_NAME}"
helpers_dir="${app_dir}/Contents/Library/Helpers"
guard_helpers_dir="${app_dir}/Contents/Library/HelperTools"
runtime_trust_dir="${app_dir}/Contents/Resources/RuntimeTrust"
mkdir -p "${app_dir}/Contents/MacOS" "$helpers_dir" "$guard_helpers_dir" \
  "${app_dir}/Contents/Resources" "$runtime_trust_dir"
install -m 0644 "$APP_ICON" "${app_dir}/Contents/Resources/AppIcon.icns"
install -m 0644 "$RUNTIME_PUBLIC_KEY_SOURCE" \
  "${runtime_trust_dir}/opencodex-runtime-release-ed25519.pub"
cmp -s "$RUNTIME_PUBLIC_KEY_SOURCE" \
  "${runtime_trust_dir}/opencodex-runtime-release-ed25519.pub" || \
  die 'bundled runtime public key bytes differ from the tracked trust root'

build_helper() {
  local command="$1"
  local destination="$2"
  # A local-only source build must never turn a missing module cache into a
  # transparent network fetch. It fails closed instead; the reviewed source
  # snapshot and its already-available module cache are the entire input.
  (cd "$RELAY_ROOT" && GOPROXY=off GOSUMDB=off CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${version}" -o "$destination" "./cmd/${command}")
  chmod 0755 "$destination"
}

build_helper opencodex-relay "${helpers_dir}/opencodex-relay"
build_helper opencodex-relayctl "${helpers_dir}/opencodex-relayctl"
swift_args=(-c release --arch arm64)
if [[ "$swift_disable_sandbox" == true ]]; then swift_args+=(--disable-sandbox); fi
(cd "$APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build "${swift_args[@]}" --target OpenCodexRelayLocalization)
(cd "$APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build "${swift_args[@]}")
swift_bin_dir="$(cd "$APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build "${swift_args[@]}" --show-bin-path)"
app_executable="${app_dir}/Contents/MacOS/OpenCodexRelay"
guard_helper="${guard_helpers_dir}/${GUARD_HELPER_NAME}"
guard_installer="${helpers_dir}/${GUARD_INSTALLER_NAME}"
[[ -x "${swift_bin_dir}/OpenCodexRelay" ]] || die 'MenuBar executable was not built'
[[ -x "${swift_bin_dir}/${GUARD_HELPER_NAME}" ]] || die 'Homebrew guard helper was not built'
[[ -x "${swift_bin_dir}/${GUARD_INSTALLER_NAME}" ]] || die 'Homebrew guard development installer was not built'
install -m 0755 "${swift_bin_dir}/OpenCodexRelay" "$app_executable"
install -m 0755 "${swift_bin_dir}/${GUARD_HELPER_NAME}" "$guard_helper"
install -m 0755 "${swift_bin_dir}/${GUARD_INSTALLER_NAME}" "$guard_installer"

# The helper is signed first so its immutable CDHash can be embedded in the
# app-side XPC requirement. The helper derives the final signed app CDHash from
# its containing bundle at launch, avoiding a self-referential resource seal.
codesign --force --sign - --identifier "$GUARD_HELPER_IDENTIFIER" "$guard_helper"
helper_cdhash="$(codesign_cdhash "$guard_helper")"
helper_requirement="cdhash H\"${helper_cdhash}\""

sed -e "s/__SHORT_VERSION__/${bundle_short_version}/g" \
    -e "s/__BUILD_VERSION__/${bundle_build_version}/g" \
    -e "s|__HELPER_REQUIREMENT__|${helper_requirement}|g" \
  "$INFO_TEMPLATE" > "${app_dir}/Contents/Info.plist"
plutil -lint "${app_dir}/Contents/Info.plist" >/dev/null
chmod 0644 "${app_dir}/Contents/Info.plist"
if grep -Eq '__[A-Z0-9_]+__' "${app_dir}/Contents/Info.plist"; then
  die 'local development bundle contains an unresolved helper placeholder'
fi

verify_reviewed_codex_identity "${app_dir}/Contents/Info.plist"
[[ "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardHelperRequirement)" == "$helper_requirement" ]] ||
  die 'local development helper code requirement is invalid'
[[ "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardBackend)" == "manual_admin" &&
   "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardMachService)" == "$GUARD_MANUAL_SERVICE" &&
   "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardInstallerExecutable)" == "$GUARD_INSTALLER_NAME" ]] ||
  die 'local development Homebrew guard backend metadata is invalid'
localization_bundle="${swift_bin_dir}/${LOCALIZATION_BUNDLE}"
[[ -d "$localization_bundle" && ! -L "$localization_bundle" ]] || die 'SwiftPM localization resource bundle is unavailable'
[[ -f "${localization_bundle}/en.lproj/Localizable.strings" && ! -L "${localization_bundle}/en.lproj/Localizable.strings" ]] || \
  die 'SwiftPM English localization catalog is unavailable'
[[ -f "${localization_bundle}/ko.lproj/Localizable.strings" && ! -L "${localization_bundle}/ko.lproj/Localizable.strings" ]] || \
  die 'SwiftPM Korean localization catalog is unavailable'
ditto "$localization_bundle" "${app_dir}/Contents/Resources/${LOCALIZATION_BUNDLE}"
for locale in en ko; do
  info_strings="${INFO_LOCALIZATIONS}/${locale}.lproj/InfoPlist.strings"
  [[ -f "$info_strings" && ! -L "$info_strings" ]] || die "InfoPlist localization is unavailable for ${locale}"
  mkdir -p "${app_dir}/Contents/Resources/${locale}.lproj"
  install -m 0644 "$info_strings" "${app_dir}/Contents/Resources/${locale}.lproj/InfoPlist.strings"
done

# This is structural ad-hoc signing only. It does not establish a publisher
# identity and the script intentionally never asks notarytool, stapler, or
# spctl to assess the resulting app.
codesign --force --sign - "${helpers_dir}/opencodex-relay"
codesign --force --sign - "${helpers_dir}/opencodex-relayctl"
codesign --force --sign - --identifier "$GUARD_INSTALLER_IDENTIFIER" "$guard_installer"
codesign --force --sign - --identifier "$BUNDLE_ID" "$app_executable"
codesign --force --sign - "$app_dir"
codesign --verify --strict --verbose=2 "$guard_helper"
codesign -dv --verbose=4 "$guard_helper" 2>&1 | grep -Fx "Identifier=${GUARD_HELPER_IDENTIFIER}" >/dev/null ||
  die 'local development Homebrew guard helper identifier is invalid'
codesign --verify --strict --verbose=2 "$guard_installer"
codesign -dv --verbose=4 "$guard_installer" 2>&1 | grep -Fx "Identifier=${GUARD_INSTALLER_IDENTIFIER}" >/dev/null ||
  die 'local development Homebrew guard installer identifier is invalid'
[[ "$(codesign_cdhash "$guard_helper")" == "$helper_cdhash" ]] ||
  die 'local development Homebrew guard helper CDHash changed during outer signing'
codesign --verify --deep --strict --verbose=2 "$app_dir"

bundle_path="${output_dir}/${BUNDLE_FILE}"
ditto -c -k --keepParent "$app_dir" "$bundle_path"
chmod 0644 "$bundle_path"
bundle_hash="$(sha256 "$bundle_path")"
notices_path="${output_dir}/${NOTICES_FILE}"
install -m 0644 "${RELAY_ROOT}/${NOTICES_FILE}" "$notices_path"
notices_hash="$(sha256 "$notices_path")"
public_key="${output_dir}/local-dev-public-key.pem"
openssl pkey -in "$signing_key" -pubout > "$public_key"
chmod 0644 "$public_key"

manifest="${output_dir}/local-dev-manifest-${version}.json"
signature="${output_dir}/local-dev-manifest-${version}.sig"
printf '{"schema":3,"distribution":"local_development","version":"%s","source_commit":"%s","artifacts":[{"os":"darwin","arch":"arm64","component":"macos_menu_bar_bundle","file":"%s","sha256":"%s","bundle_id":"%s"}],"documents":[{"file":"%s","sha256":"%s"}]}\n' \
  "$version" "$source_commit" "$BUNDLE_FILE" "$bundle_hash" "$BUNDLE_ID" "$NOTICES_FILE" "$notices_hash" > "$manifest"
openssl pkeyutl -sign -rawin -inkey "$signing_key" -in "$manifest" | base64 | tr -d '\n' > "$signature"
printf '\n' >> "$signature"
chmod 0644 "$manifest" "$signature"

printf 'local_dev_manifest=%s\nlocal_dev_signature=%s\nlocal_dev_public_key=%s\nlocal_dev_bundle=%s\nsource_commit=%s\n' \
  "$manifest" "$signature" "$public_key" "$bundle_path" "$source_commit"

#!/usr/bin/env bash
# Build the reviewed relay targets and sign one immutable release manifest.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly RELAY_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"
readonly KEYCHAIN_HELPER="${SCRIPT_DIR}/keychain-signing-key.swift"
readonly BUILD_NUMBER_VALIDATOR="${SCRIPT_DIR}/validate-release-build-number.py"
readonly THIRD_PARTY_NOTICES_FILE="THIRD_PARTY_NOTICES.md"
readonly THIRD_PARTY_NOTICES_SOURCE="${RELAY_ROOT}/${THIRD_PARTY_NOTICES_FILE}"
readonly RELEASE_BUILD_NUMBER_SOURCE="${RELAY_ROOT}/RELEASE_BUILD_NUMBER"
readonly RELEASE_PUBLIC_KEY_SOURCE="${RELAY_ROOT}/../../config/trust/opencodex-relay-release-ed25519.pub"
readonly MACOS_APP_ROOT="${RELAY_ROOT}/macos/OpenCodexRelay"
readonly MACOS_INFO_TEMPLATE="${MACOS_APP_ROOT}/Resources/Info.plist"
readonly MACOS_APP_ICON="${MACOS_APP_ROOT}/Resources/AppIcon.icns"
readonly MACOS_INFO_LOCALIZATIONS="${MACOS_APP_ROOT}/Resources/InfoPlist.production"
readonly MACOS_BUNDLE_NAME="OpenCodexRelay.app"
readonly MACOS_BUNDLE_ID="io.github.novelkr.opencodex-relay"
readonly MACOS_GUARD_HELPER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.helper"
readonly MACOS_GUARD_HELPER_NAME="OpenCodexRelayPrivilegedHelper"
readonly MACOS_GUARD_INSTALLER_IDENTIFIER="io.github.novelkr.opencodex-relay.homebrew-guard.installer"
readonly MACOS_GUARD_INSTALLER_NAME="OpenCodexRelayHelperInstaller"
readonly TRUSTED_CODEX_BUNDLE_ID="com.openai.codex"
readonly TRUSTED_CODEX_TEAM_ID="2DC432GLL2"
readonly MACOS_LOCALIZATION_BUNDLE="OpenCodexRelay_OpenCodexRelayLocalization.bundle"

usage() {
  cat <<'USAGE'
Usage:
  build-release.sh VERSION (--base-url HTTPS_URL | --github-repo OWNER/REPO) \\
    (--signing-key PEM | --signing-key-keychain-service SERVICE) \\
    --previous-build-number NUMERIC_VERSION --output DIR

Builds Linux amd64/arm64 relay and relayctl binaries plus a self-contained,
ad-hoc-signed darwin/arm64 OpenCodexRelay.app.zip. The macOS bundle uses the
Hardened Runtime, contains the two Go helpers, the manual privileged-helper
installer, and the tracked release trust key, and is the only darwin artifact in
revision 4. RELEASE_BUILD_NUMBER supplies the monotonically increasing numeric
CFBundleVersion. No Apple developer account or notarization credential is used.
The Ed25519 private key remains an off-repository release-workstation input.
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
    die 'macOS bundle has an unreviewed Codex Desktop bundle identifier'
  [[ "$(plist_string "$plist" OpenCodexTrustedCodexTeamIdentifier 2>/dev/null)" == "$TRUSTED_CODEX_TEAM_ID" ]] || \
    die 'macOS bundle has an unreviewed Codex Desktop Team ID'
}

require_ed25519_private_key() {
  local path="$1"
  command -v openssl >/dev/null || die 'openssl is required for Ed25519 signing'
  openssl pkey -in "$path" -text -noout 2>/dev/null | grep -Eq '^ED25519 Private-Key:' || \
    die 'release signing key must be an Ed25519 private PEM'
}

[[ $# -ge 1 ]] || { usage >&2; exit 2; }
version="$1"
shift
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die 'VERSION must be explicit semver'
# CFBundleVersion is an independently increasing distribution build identifier.
# Keep the full SemVer for the user-facing marketing version so prereleases remain
# distinguishable while every distributed app gets a unique numeric build value.
bundle_short_version="$version"

base_url=""
github_repo=""
signing_key=""
signing_key_keychain_service=""
output_dir=""
previous_build_number=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base-url) base_url="${2:-}"; shift 2 ;;
    --github-repo) github_repo="${2:-}"; shift 2 ;;
    --signing-key) signing_key="${2:-}"; shift 2 ;;
    --signing-key-keychain-service) signing_key_keychain_service="${2:-}"; shift 2 ;;
    --previous-build-number) previous_build_number="${2:-}"; shift 2 ;;
    --output) output_dir="${2:-}"; shift 2 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ -n "$previous_build_number" ]] || die '--previous-build-number is required'
command -v python3 >/dev/null || die 'python3 is required for release build-number comparison'
[[ -f "$BUILD_NUMBER_VALIDATOR" && ! -L "$BUILD_NUMBER_VALIDATOR" ]] || \
  die 'release build-number validator is unavailable'
bundle_build_version="$(python3 "$BUILD_NUMBER_VALIDATOR" \
  "$RELEASE_BUILD_NUMBER_SOURCE" "$previous_build_number")" || \
  die 'release build-number validation failed'

if [[ -n "$base_url" && -n "$github_repo" ]]; then
  die '--base-url and --github-repo are mutually exclusive'
fi
if [[ -n "$github_repo" ]]; then
  [[ "$github_repo" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || \
    die '--github-repo must be OWNER/REPO'
  base_url="https://github.com/${github_repo}/releases/download"
else
  [[ "$base_url" =~ ^https://[^/?#]+(/[^?#]*)?$ ]] || die '--base-url must be HTTPS'
  [[ ! "$base_url" =~ ^https://github\.com/[^/]+/[^/]+/releases/download$ ]] || \
    die 'GitHub release URLs require --github-repo'
fi
if [[ -n "$signing_key" && -n "$signing_key_keychain_service" ]]; then
  die '--signing-key and --signing-key-keychain-service are mutually exclusive'
fi
keychain_tmp=""
release_tmp=""
cleanup() {
  [[ -z "$keychain_tmp" ]] || rm -rf -- "$keychain_tmp"
  [[ -z "$release_tmp" ]] || rm -rf -- "$release_tmp"
}
trap cleanup EXIT
if [[ -n "$signing_key_keychain_service" ]]; then
  [[ "$(uname -s)" == "Darwin" ]] || die '--signing-key-keychain-service is supported only on macOS'
  [[ "$signing_key_keychain_service" != *$'\n'* && "$signing_key_keychain_service" != *$'\r'* ]] || \
    die '--signing-key-keychain-service contains an unsafe character'
  command -v swift >/dev/null || die 'swift is required for the Keychain signing-key source'
  [[ -f "$KEYCHAIN_HELPER" && ! -L "$KEYCHAIN_HELPER" ]] || die 'Keychain signing-key helper is unavailable'
  keychain_tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-signing-key.XXXXXX")"
  signing_key="${keychain_tmp}/release-ed25519.pem"
  umask 077
  swift "$KEYCHAIN_HELPER" read "$signing_key_keychain_service" > "$signing_key" 2>/dev/null || \
    die 'Keychain signing-key item is unavailable'
  chmod 0600 "$signing_key"
  [[ -s "$signing_key" ]] || die 'Keychain signing-key item is empty'
else
  [[ -f "$signing_key" && ! -L "$signing_key" ]] || die '--signing-key must be a regular file'
fi
require_ed25519_private_key "$signing_key"
[[ -n "$output_dir" ]] || die '--output is required'
[[ "$(uname -s)" == "Darwin" ]] || die 'revision 4 release builds require a macOS release workstation'
command -v go >/dev/null || die 'Go toolchain is required'
command -v swift >/dev/null || die 'Swift toolchain is required'
command -v codesign >/dev/null || die 'codesign is required'
command -v ditto >/dev/null || die 'ditto is required'
command -v plutil >/dev/null || die 'plutil is required'
command -v openssl >/dev/null || die 'openssl is required for Ed25519 signing'
command -v shasum >/dev/null || command -v sha256sum >/dev/null || die 'sha256 tool is required'
[[ -f "$THIRD_PARTY_NOTICES_SOURCE" && ! -L "$THIRD_PARTY_NOTICES_SOURCE" ]] || \
  die 'THIRD_PARTY_NOTICES.md must be a regular repository file'
[[ -f "$RELEASE_PUBLIC_KEY_SOURCE" && ! -L "$RELEASE_PUBLIC_KEY_SOURCE" ]] || \
  die 'tracked release public key must be a regular file'
openssl pkey -pubin -in "$RELEASE_PUBLIC_KEY_SOURCE" -text -noout 2>/dev/null | grep -Eq '^ED25519 Public-Key:' || \
  die 'tracked release public key must be an Ed25519 public PEM'
[[ -f "$MACOS_INFO_TEMPLATE" && ! -L "$MACOS_INFO_TEMPLATE" ]] || \
  die 'macOS Info.plist template is unavailable'
[[ -f "$MACOS_APP_ICON" && ! -L "$MACOS_APP_ICON" ]] || die 'macOS app icon is unavailable'
[[ -d "$MACOS_INFO_LOCALIZATIONS" && ! -L "$MACOS_INFO_LOCALIZATIONS" ]] || \
  die 'macOS InfoPlist localization resources are unavailable'

verify_reviewed_codex_identity "$MACOS_INFO_TEMPLATE"
mkdir -p "$output_dir"
umask 077

sha256() {
  if command -v shasum >/dev/null; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

build_go_binary() {
  local goos="$1"
  local goarch="$2"
  local command="$3"
  local destination="$4"
  (cd "$RELAY_ROOT" && \
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${version}" \
        -o "$destination" "./cmd/${command}")
  chmod 0755 "$destination"
}

codesign_cdhash() {
  local target="$1"
  local value
  value="$(codesign -dvvv "$target" 2>&1 | sed -nE 's/^CDHash=([0-9a-f]+)$/\1/p')"
  [[ "$value" =~ ^[0-9a-f]{40,128}$ ]] || die "ad-hoc component has no CDHash: $target"
  printf '%s\n' "$value"
}

verify_adhoc_hardened() {
  local target="$1"
  local details
  details="$(codesign -dvvv --verbose=4 "$target" 2>&1)"
  grep -Fx 'Signature=adhoc' <<<"$details" >/dev/null || die "component is not ad-hoc signed: $target"
  grep -Fx 'TeamIdentifier=not set' <<<"$details" >/dev/null || die "component unexpectedly has an Apple Team ID: $target"
  grep -E '^CodeDirectory .*flags=.*\(.*runtime.*\)' <<<"$details" >/dev/null || \
    die "component does not use the Hardened Runtime: $target"
}

stage_app_localizations() {
  local app_resources="$1"
  local swift_bin_dir="$2"
  local locale
  local source_strings
  local localization_bundle="${swift_bin_dir}/${MACOS_LOCALIZATION_BUNDLE}"

  [[ -d "$localization_bundle" && ! -L "$localization_bundle" ]] || \
    die 'SwiftPM localization resource bundle is unavailable'
  [[ -f "${localization_bundle}/en.lproj/Localizable.strings" && ! -L "${localization_bundle}/en.lproj/Localizable.strings" ]] || \
    die 'SwiftPM English localization catalog is unavailable'
  [[ -f "${localization_bundle}/ko.lproj/Localizable.strings" && ! -L "${localization_bundle}/ko.lproj/Localizable.strings" ]] || \
    die 'SwiftPM Korean localization catalog is unavailable'
  ditto "$localization_bundle" "${app_resources}/${MACOS_LOCALIZATION_BUNDLE}"

  for locale in en ko; do
    source_strings="${MACOS_INFO_LOCALIZATIONS}/${locale}.lproj/InfoPlist.strings"
    [[ -f "$source_strings" && ! -L "$source_strings" ]] || \
      die "InfoPlist localization is unavailable for ${locale}"
    mkdir -p "${app_resources}/${locale}.lproj"
    install -m 0644 "$source_strings" "${app_resources}/${locale}.lproj/InfoPlist.strings"
  done
}

artifact_json=""
append_artifact() {
  local row="$1"
  if [[ -n "$artifact_json" ]]; then artifact_json+=","; fi
  artifact_json+="$row"
}

for target in "linux amd64" "linux arm64"; do
  read -r goos goarch <<<"$target"
  for command in opencodex-relay opencodex-relayctl; do
    file="${command}_${goos}_${goarch}"
    path="${output_dir}/${file}"
    component="relay"
    [[ "$command" == "opencodex-relayctl" ]] && component="relayctl"
    build_go_binary "$goos" "$goarch" "$command" "$path"
    hash="$(sha256 "$path")"
    url="${base_url%/}/${version}/${file}"
    append_artifact "{\"os\":\"${goos}\",\"arch\":\"${goarch}\",\"component\":\"${component}\",\"file\":\"${file}\",\"url\":\"${url}\",\"sha256\":\"${hash}\"}"
  done
done

release_tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-macos-release.XXXXXX")"
app_dir="${release_tmp}/${MACOS_BUNDLE_NAME}"
helpers_dir="${app_dir}/Contents/Library/Helpers"
guard_helpers_dir="${app_dir}/Contents/Library/HelperTools"
release_trust_dir="${app_dir}/Contents/Resources/ReleaseTrust"
bundled_release_public_key="${release_trust_dir}/opencodex-relay-release-ed25519.pub"
mkdir -p "${app_dir}/Contents/MacOS" "$helpers_dir" "$guard_helpers_dir" \
  "${app_dir}/Contents/Resources" "$release_trust_dir"
install -m 0644 "$MACOS_APP_ICON" "${app_dir}/Contents/Resources/AppIcon.icns"
install -m 0644 "$RELEASE_PUBLIC_KEY_SOURCE" "$bundled_release_public_key"
cmp -s "$RELEASE_PUBLIC_KEY_SOURCE" "$bundled_release_public_key" || \
  die 'bundled release public key bytes differ from the tracked trust root'
trusted_public_der="${release_tmp}/tracked-release-public-key.der"
bundled_public_der="${release_tmp}/bundled-release-public-key.der"
openssl pkey -pubin -in "$RELEASE_PUBLIC_KEY_SOURCE" -outform DER > "$trusted_public_der"
openssl pkey -pubin -in "$bundled_release_public_key" -outform DER > "$bundled_public_der"
cmp -s "$trusted_public_der" "$bundled_public_der" || \
  die 'bundled release public key fingerprint differs from the tracked trust root'
release_trust_key_id="$(sha256 "$trusted_public_der")"
build_go_binary darwin arm64 opencodex-relay "${helpers_dir}/opencodex-relay"
build_go_binary darwin arm64 opencodex-relayctl "${helpers_dir}/opencodex-relayctl"

(cd "$MACOS_APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build -c release --arch arm64 --target OpenCodexRelayLocalization)
(cd "$MACOS_APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build -c release --arch arm64)
swift_bin_dir="$(cd "$MACOS_APP_ROOT" && MACOSX_DEPLOYMENT_TARGET=26.0 swift build -c release --arch arm64 --show-bin-path)"
app_executable="${app_dir}/Contents/MacOS/OpenCodexRelay"
guard_helper="${guard_helpers_dir}/${MACOS_GUARD_HELPER_NAME}"
guard_installer="${helpers_dir}/${MACOS_GUARD_INSTALLER_NAME}"
[[ -x "${swift_bin_dir}/OpenCodexRelay" ]] || die 'Swift MenuBar executable was not built'
[[ -x "${swift_bin_dir}/${MACOS_GUARD_HELPER_NAME}" ]] || die 'Swift Homebrew guard helper was not built'
[[ -x "${swift_bin_dir}/${MACOS_GUARD_INSTALLER_NAME}" ]] || die 'Swift Homebrew guard installer was not built'
install -m 0755 "${swift_bin_dir}/OpenCodexRelay" "$app_executable"
install -m 0755 "${swift_bin_dir}/${MACOS_GUARD_HELPER_NAME}" "$guard_helper"
install -m 0755 "${swift_bin_dir}/${MACOS_GUARD_INSTALLER_NAME}" "$guard_installer"

# Sign the helper first so its immutable CDHash can be embedded in the app-side
# XPC requirement. The root installer later binds the installed daemon to the
# exact app and installer CDHashes from this same release.
codesign --force --sign - --options runtime \
  --identifier "$MACOS_GUARD_HELPER_IDENTIFIER" "$guard_helper"
helper_cdhash="$(codesign_cdhash "$guard_helper")"
helper_requirement="cdhash H\"${helper_cdhash}\""

sed -e "s/__SHORT_VERSION__/${bundle_short_version}/g" \
    -e "s/__BUILD_VERSION__/${bundle_build_version}/g" \
    -e "s|__HELPER_REQUIREMENT__|${helper_requirement}|g" \
  "$MACOS_INFO_TEMPLATE" > "${app_dir}/Contents/Info.plist"
plutil -lint "${app_dir}/Contents/Info.plist" >/dev/null
chmod 0644 "${app_dir}/Contents/Info.plist"
if grep -Eq '__[A-Z0-9_]+__' "${app_dir}/Contents/Info.plist"; then
  die 'production bundle contains an unresolved helper placeholder'
fi
stage_app_localizations "${app_dir}/Contents/Resources" "$swift_bin_dir"
verify_reviewed_codex_identity "${app_dir}/Contents/Info.plist"
[[ "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardHelperRequirement)" == "$helper_requirement" ]] ||
  die 'production helper code requirement is invalid'
[[ "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardBackend)" == manual_admin &&
   "$(plist_string "${app_dir}/Contents/Info.plist" OpenCodexHomebrewGuardInstallerExecutable)" == "$MACOS_GUARD_INSTALLER_NAME" ]] ||
  die 'production Homebrew guard manual-installer metadata is invalid'

codesign --force --sign - --options runtime \
  "${helpers_dir}/opencodex-relay"
codesign --force --sign - --options runtime \
  "${helpers_dir}/opencodex-relayctl"
codesign --force --sign - --options runtime \
  --identifier "$MACOS_GUARD_INSTALLER_IDENTIFIER" "$guard_installer"
codesign --force --sign - --options runtime \
  --identifier "$MACOS_BUNDLE_ID" "$app_executable"
codesign --force --sign - --options runtime "$app_dir"
codesign --verify --strict --verbose=2 "$guard_helper"
codesign -dv --verbose=4 "$guard_helper" 2>&1 | grep -Fx "Identifier=${MACOS_GUARD_HELPER_IDENTIFIER}" >/dev/null ||
  die 'production Homebrew guard helper identifier is invalid'
codesign --verify --strict --verbose=2 "$guard_installer"
codesign -dv --verbose=4 "$guard_installer" 2>&1 | grep -Fx "Identifier=${MACOS_GUARD_INSTALLER_IDENTIFIER}" >/dev/null ||
  die 'production Homebrew guard installer identifier is invalid'
[[ "$(codesign_cdhash "$guard_helper")" == "$helper_cdhash" ]] ||
  die 'production Homebrew guard helper CDHash changed during outer signing'
codesign --verify --deep --strict --verbose=2 "$app_dir"
for signed_component in \
  "${helpers_dir}/opencodex-relay" \
  "${helpers_dir}/opencodex-relayctl" \
  "$guard_helper" \
  "$guard_installer" \
  "$app_executable" \
  "$app_dir"; do
  verify_adhoc_hardened "$signed_component"
done

bundle_file="OpenCodexRelay.app.zip"
bundle_path="${output_dir}/${bundle_file}"
ditto -c -k --keepParent "$app_dir" "$bundle_path"
chmod 0644 "$bundle_path"
bundle_hash="$(sha256 "$bundle_path")"
bundle_url="${base_url%/}/${version}/${bundle_file}"
append_artifact "{\"os\":\"darwin\",\"arch\":\"arm64\",\"component\":\"macos_menu_bar_bundle\",\"file\":\"${bundle_file}\",\"url\":\"${bundle_url}\",\"sha256\":\"${bundle_hash}\",\"bundle_id\":\"${MACOS_BUNDLE_ID}\",\"signing_mode\":\"adhoc\"}"

notices="${output_dir}/${THIRD_PARTY_NOTICES_FILE}"
install -m 0644 "$THIRD_PARTY_NOTICES_SOURCE" "$notices"
notices_hash="$(sha256 "$notices")"
notices_url="${base_url%/}/${version}/${THIRD_PARTY_NOTICES_FILE}"
manifest="${output_dir}/manifest-${version}.json"
signature="${output_dir}/manifest-${version}.sig"
printf '{"version":"%s","compatibility_revision":4,"artifacts":[%s],"documents":[{"file":"%s","url":"%s","sha256":"%s"}]}\n' \
  "$version" "$artifact_json" "$THIRD_PARTY_NOTICES_FILE" "$notices_url" "$notices_hash" > "$manifest"
openssl pkeyutl -sign -rawin -inkey "$signing_key" -in "$manifest" | base64 | tr -d '\n' > "$signature"
printf '\n' >> "$signature"
chmod 0644 "$manifest" "$signature"
printf 'release_manifest=%s\nrelease_signature=%s\nmacos_bundle=%s\nrelease_build_number=%s\nrelease_trust_key_id=%s\n' \
  "$manifest" "$signature" "$bundle_path" "$bundle_build_version" "$release_trust_key_id"
if [[ -n "$github_repo" ]]; then
  printf 'github_release_repo=%s github_release_tag=%s\n' "$github_repo" "$version"
fi

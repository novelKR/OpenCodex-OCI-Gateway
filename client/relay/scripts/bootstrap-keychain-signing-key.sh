#!/usr/bin/env bash
# Generate one Ed25519 release key directly into the current macOS login Keychain.
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly KEYCHAIN_HELPER="${SCRIPT_DIR}/keychain-signing-key.swift"

usage() {
  cat <<'USAGE'
Usage:
  bootstrap-keychain-signing-key.sh --service SERVICE --public-key-out PEM

Creates one new Ed25519 private key as a Keychain generic-password item for the
current macOS login account. It never writes the private PEM outside an
owner-only temporary directory, never overwrites an existing item, and writes
only the corresponding public PEM to --public-key-out.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

service=""
public_key_out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --service) service="${2:-}"; shift 2 ;;
    --public-key-out) public_key_out="${2:-}"; shift 2 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ "$(uname -s)" == "Darwin" ]] || die 'Keychain signing-key bootstrap is supported only on macOS'
[[ -n "$service" && "$service" != *$'\n'* && "$service" != *$'\r'* ]] || die '--service is required and must not contain a newline'
[[ -n "$public_key_out" ]] || die '--public-key-out is required'
[[ -d "$(dirname -- "$public_key_out")" ]] || die '--public-key-out parent directory is unavailable'
[[ ! -e "$public_key_out" && ! -L "$public_key_out" ]] || die '--public-key-out must not already exist'
[[ -f "$KEYCHAIN_HELPER" && ! -L "$KEYCHAIN_HELPER" ]] || die 'Keychain helper is unavailable'
command -v openssl >/dev/null || die 'openssl is required for Ed25519 key generation'
command -v swift >/dev/null || die 'swift is required for macOS Keychain access'

tmp="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-relay-keychain-bootstrap.XXXXXX")"
cleanup() { rm -rf -- "$tmp"; }
trap cleanup EXIT
umask 077

private_key="${tmp}/release-ed25519.pem"
retrieved_key="${tmp}/release-ed25519-readback.pem"
public_key="${tmp}/release-ed25519.pub"
openssl genpkey -algorithm Ed25519 -out "$private_key"
swift "$KEYCHAIN_HELPER" store "$private_key" "$service"
swift "$KEYCHAIN_HELPER" read "$service" > "$retrieved_key"
cmp -s "$private_key" "$retrieved_key" || die 'Keychain readback did not match the generated signing key'
openssl pkey -in "$retrieved_key" -pubout -out "$public_key"
install -m 0644 "$public_key" "$public_key_out"

printf 'keychain_service=%s\n' "$service"
printf 'public_key_sha256=%s\n' "$(shasum -a 256 "$public_key_out" | awk '{print $1}')"

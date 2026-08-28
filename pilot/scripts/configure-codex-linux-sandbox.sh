#!/usr/bin/env bash
# Enable Codex's Ubuntu 24.04 Linux sandbox through the distribution bwrap
# package and its narrow AppArmor exception. This intentionally preserves the
# global AppArmor unprivileged-user-namespace restriction.

set -euo pipefail

readonly PROFILE_SOURCE="/usr/share/apparmor/extra-profiles/bwrap-userns-restrict"
readonly PROFILE_TARGET="/etc/apparmor.d/bwrap-userns-restrict"
readonly BWRAP="/usr/bin/bwrap"
target_user="ubuntu"

usage() {
  cat <<'USAGE'
Usage:
  sudo ./configure-codex-linux-sandbox.sh [--user USER]

Installs Ubuntu's bubblewrap and AppArmor profile packages, loads the narrow
bwrap user-namespace profile, and verifies bubblewrap as USER. It does not set
kernel.apparmor_restrict_unprivileged_userns=0.
USAGE
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

if [[ "${1:-}" == "--user" ]]; then
  target_user="${2:-}"
  [[ -n "${target_user}" && "$#" -eq 2 ]] || { usage; exit 2; }
elif [[ "$#" -ne 0 ]]; then
  usage
  exit 2
fi

[[ "${EUID}" -eq 0 ]] || die "run as root"
[[ "${target_user}" =~ ^[a-z_][a-z0-9_-]*$ ]] || die "USER has an unsafe name"
getent passwd "${target_user}" >/dev/null || die "USER does not exist: ${target_user}"
[[ -r /etc/os-release ]] || die "/etc/os-release is missing"
# shellcheck source=/dev/null
source /etc/os-release
[[ "${ID:-}" == "ubuntu" && "${VERSION_ID:-}" == "24.04" ]] || \
  die "this script is scoped to Ubuntu 24.04"

for command_name in apt-get getent install runuser sysctl; do
  command -v "${command_name}" >/dev/null || die "required command is missing: ${command_name}"
done

apt-get update
apt-get install -y --no-install-recommends bubblewrap apparmor-profiles apparmor-utils
for command_name in aa-status apparmor_parser; do
  command -v "${command_name}" >/dev/null || die "required command is missing after installation: ${command_name}"
done
[[ -x "${BWRAP}" ]] || die "bubblewrap executable is missing: ${BWRAP}"
[[ -f "${PROFILE_SOURCE}" && ! -L "${PROFILE_SOURCE}" ]] || \
  die "Ubuntu bwrap AppArmor profile is missing: ${PROFILE_SOURCE}"

install -o root -g root -m 0644 "${PROFILE_SOURCE}" "${PROFILE_TARGET}"
apparmor_parser -r "${PROFILE_TARGET}"
aa-status --enabled
aa_status="$(aa-status)"
grep -Eq '^[[:space:]]+bwrap$' <<< "${aa_status}" || \
  die "bwrap AppArmor profile did not load in enforce mode"
grep -Eq '^[[:space:]]+unpriv_bwrap$' <<< "${aa_status}" || \
  die "unpriv_bwrap AppArmor profile did not load in enforce mode"

max_user_namespaces="$(sysctl -n user.max_user_namespaces)"
[[ "${max_user_namespaces}" =~ ^[1-9][0-9]*$ ]] || \
  die "user.max_user_namespaces must be greater than zero"
if [[ -r /proc/sys/kernel/unprivileged_userns_clone ]]; then
  [[ "$(sysctl -n kernel.unprivileged_userns_clone)" == "1" ]] || \
    die "kernel.unprivileged_userns_clone must be enabled"
fi

"${BWRAP}" --version
runuser -u "${target_user}" -- "${BWRAP}" --unshare-user --ro-bind / / --proc /proc true

if [[ -r /proc/sys/kernel/apparmor_restrict_unprivileged_userns ]]; then
  printf 'apparmor_unprivileged_userns_restriction=%s\n' \
    "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns)"
fi
printf 'codex_linux_sandbox=ready user=%s bwrap=%s\n' "${target_user}" "${BWRAP}"

#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly ASSET_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly SWAP_FILE="/swapfile"
readonly REQUESTED_SIZE="${1:-4G}"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ "${EUID}" -eq 0 ]] || die "run as root"
case "${REQUESTED_SIZE}" in
  2G|4G) ;;
  *) die "size must be 2G or 4G" ;;
esac
readonly EXPECTED_BYTES="$(( ${REQUESTED_SIZE%G} * 1024 * 1024 * 1024 ))"
readonly PAGE_BYTES="$(getconf PAGESIZE)"
readonly EXPECTED_ACTIVE_BYTES="$(( EXPECTED_BYTES - PAGE_BYTES ))"

if swapon --noheadings --show=NAME | grep -Fxq "${SWAP_FILE}"; then
  file_bytes="$(stat -c '%s' "${SWAP_FILE}")"
  actual_bytes="$(swapon --bytes --noheadings --show=NAME,SIZE | awk -v name="${SWAP_FILE}" '$1 == name { print $2 }')"
  [[ "${file_bytes}" == "${EXPECTED_BYTES}" ]] || \
    die "${SWAP_FILE} has ${file_bytes} file bytes, expected ${EXPECTED_BYTES}; inspect before resizing"
  [[ "${actual_bytes}" == "${EXPECTED_ACTIVE_BYTES}" ]] || \
    die "${SWAP_FILE} has ${actual_bytes} usable bytes, expected ${EXPECTED_ACTIVE_BYTES}; inspect before resizing"
  printf '%s is already active at the requested size; leaving it unchanged.\n' "${SWAP_FILE}"
else
  if [[ -e "${SWAP_FILE}" ]]; then
    die "${SWAP_FILE} exists but is not active; inspect it before continuing"
  fi

  if ! fallocate -l "${REQUESTED_SIZE}" "${SWAP_FILE}"; then
    count_gib="${REQUESTED_SIZE%G}"
    dd if=/dev/zero of="${SWAP_FILE}" bs=1M count="$((count_gib * 1024))" status=progress
  fi
  chmod 0600 "${SWAP_FILE}"
  mkswap "${SWAP_FILE}"
  swapon "${SWAP_FILE}"
fi

if ! grep -Eq '^/swapfile[[:space:]]+none[[:space:]]+swap([[:space:]]|$)' /etc/fstab; then
  printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
fi

install -o root -g root -m 0644 \
  "${ASSET_DIR}/sysctl/99-opencodex-memory.conf" \
  /etc/sysctl.d/99-opencodex-memory.conf
sysctl --system >/dev/null
swapon --show
free -h

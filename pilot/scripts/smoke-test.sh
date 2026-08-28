#!/usr/bin/env bash
set -uo pipefail

readonly OCX_URL="http://127.0.0.1:10100"
readonly GATEWAY_URL="http://127.0.0.1:18080"
readonly PACKAGE_NAME="@bitkyc08/opencodex"
readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"
readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"
readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"
readonly GATEWAY_KEY_FILE="/etc/opencodex/gateway-api-key"
readonly GATEWAY_MAP_FILE="/etc/nginx/private/opencodex-api-key-map.conf"
readonly FEATURE_FLAGS_FILE="/etc/nginx/private/opencodex-feature-flags.conf"
readonly GATEWAY_CONFIG_FILE="/etc/nginx/conf.d/opencodex-api.conf"
readonly EXPECTED_GATEWAY_REJECTION_HEADER="X-OpenCodex-Gateway-Rejection: api-key"
readonly EXPECTED_GENERATION_SAFETY_LIMIT="32"
readonly EXPECTED_SWAP_FILE_BYTES="4294967296"
readonly PAGE_BYTES="$(getconf PAGESIZE)"
readonly EXPECTED_ACTIVE_SWAP_BYTES="$(( EXPECTED_SWAP_FILE_BYTES - PAGE_BYTES ))"

failures=0
hold_pid=""
tmp_dir=""
expected_opencodex_version="${EXPECTED_OPENCODEX_VERSION:-}"
legacy_expected_version_fallback=false
opencodex_home=""
opencodex_prefix=""
package_manifest=""

pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

check_safe_root_file() {
  local path="$1"
  local expected_mode="$2"
  local label="$3"
  if [[ -f "${path}" && ! -L "${path}" ]] && \
     [[ "$(stat -c '%U:%G:%a' "${path}")" == "root:root:${expected_mode}" ]]; then
    pass "${label} is root:root ${expected_mode}"
    return 0
  fi
  fail "${label} is missing, unsafe, or not root:root ${expected_mode}: ${path}"
  return 1
}

check_safe_root_directory() {
  local path="$1"
  local expected_mode="$2"
  local label="$3"
  if [[ -d "${path}" && ! -L "${path}" ]] && \
     [[ "$(stat -c '%U:%G:%a' "${path}")" == "root:root:${expected_mode}" ]]; then
    pass "${label} is root:root ${expected_mode}"
    return 0
  fi
  fail "${label} is missing, unsafe, or not root:root ${expected_mode}: ${path}"
  return 1
}

cleanup() {
  if [[ -n "${hold_pid}" ]] && kill -0 "${hold_pid}" 2>/dev/null; then
    kill "${hold_pid}" 2>/dev/null || true
    wait "${hold_pid}" 2>/dev/null || true
  fi
  [[ -z "${tmp_dir}" ]] || rm -rf "${tmp_dir}"
}
trap cleanup EXIT INT TERM

if [[ "${EUID}" -ne 0 ]]; then
  printf 'ERROR: run as root so key permissions, systemd, swap, and logrotate can be verified.\n' >&2
  exit 2
fi

if [[ -z "${expected_opencodex_version}" ]]; then
  if [[ -f "${EXPECTED_VERSION_FILE}" && ! -L "${EXPECTED_VERSION_FILE}" && \
        "$(stat -c '%U:%G:%a' "${EXPECTED_VERSION_FILE}")" == "root:root:644" ]]; then
    expected_opencodex_version="$(<"${EXPECTED_VERSION_FILE}")"
  else
    # Existing pilot hosts may predate the state file. Use the currently
    # deployed, source-verified baseline until upgrade-opencodex.sh writes an
    # explicit expected version.
    expected_opencodex_version="2.10.1"
    legacy_expected_version_fallback=true
  fi
fi
if [[ ! "${expected_opencodex_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  printf 'ERROR: expected OpenCodex version is not an explicit semver: %s\n' \
    "${expected_opencodex_version}" >&2
  exit 2
fi

for command_name in awk curl grep head jq logrotate nginx ps runuser ss sshd stat swapon sysctl systemctl tr; do
  if command -v "${command_name}" >/dev/null; then
    pass "${command_name} is installed"
  else
    fail "${command_name} is not installed"
  fi
done
if ((failures > 0)); then
  printf '%d prerequisite check(s) failed.\n' "${failures}" >&2
  exit 1
fi

runtime_contract_ready=true
check_safe_root_directory "$(dirname -- "${RUNTIME_CONFIG}")" 755 \
  "runtime contract directory" || runtime_contract_ready=false
check_safe_root_file "${RUNTIME_ADAPTER}" 755 "runtime adapter" || runtime_contract_ready=false
[[ -x "${RUNTIME_ADAPTER}" ]] || {
  fail "runtime adapter is not executable"
  runtime_contract_ready=false
}
check_safe_root_file "${RUNTIME_CONFIG}" 644 "runtime contract" || runtime_contract_ready=false
if [[ "${runtime_contract_ready}" == "true" ]]; then
  if "${RUNTIME_ADAPTER}" check >/dev/null; then
    pass "runtime adapter contract is valid"
  else
    fail "runtime adapter contract validation failed"
    runtime_contract_ready=false
  fi
fi
if [[ "${runtime_contract_ready}" == "true" ]]; then
  runtime_description="$("${RUNTIME_ADAPTER}" describe --json 2>/dev/null || true)"
  opencodex_home="$(jq -er '.home | select(type == "string" and length > 0)' \
    <<< "${runtime_description}" 2>/dev/null || true)"
  opencodex_prefix="$(jq -er '.prefix | select(type == "string" and length > 0)' \
    <<< "${runtime_description}" 2>/dev/null || true)"
  package_manifest="$(jq -er '.package_manifest | select(type == "string" and length > 0)' \
    <<< "${runtime_description}" 2>/dev/null || true)"
  if [[ -n "${opencodex_home}" && -n "${opencodex_prefix}" && \
        -n "${package_manifest}" ]]; then
    pass "runtime adapter description is complete"
  else
    fail "runtime adapter description is incomplete"
    runtime_contract_ready=false
  fi
fi
if [[ "${runtime_contract_ready}" != "true" ]]; then
  printf '%d prerequisite check(s) failed.\n' "${failures}" >&2
  exit 1
fi

sshd_effective_config="$(sshd -T 2>/dev/null || true)"
check_sshd_setting() {
  local setting="$1"
  local expected="$2"
  local actual
  actual="$(awk -v setting="${setting}" '$1 == setting { print $2; exit }' <<< "${sshd_effective_config}")"
  if [[ "${actual}" == "${expected}" ]]; then
    pass "sshd ${setting}=${expected}"
  else
    fail "sshd ${setting} is ${actual:-unset}; expected ${expected}"
  fi
}
if [[ -n "${sshd_effective_config}" ]]; then
  check_sshd_setting pubkeyauthentication yes
  check_sshd_setting passwordauthentication no
  check_sshd_setting kbdinteractiveauthentication no
  check_sshd_setting hostbasedauthentication no
  check_sshd_setting gssapiauthentication no
  check_sshd_setting permitemptypasswords no
else
  fail "effective sshd configuration could not be read"
fi

tmp_dir="$(mktemp -d)"
valid_header_file="${tmp_dir}/valid.headers"
invalid_header_file="${tmp_dir}/invalid.headers"
missing_key_response_headers="${tmp_dir}/missing-key-response.headers"
invalid_key_response_headers="${tmp_dir}/invalid-key-response.headers"
overlap_response_headers="${tmp_dir}/overlap-response.headers"
responses_websocket_headers="${tmp_dir}/responses-websocket.headers"
hold_payload="${tmp_dir}/hold.json"
second_payload="${tmp_dir}/second.json"
logrotate_debug="${tmp_dir}/logrotate-debug.txt"

curl_code() {
  local method="$1"
  local url="$2"
  local header_file="${3:-}"
  local data_file="${4:-}"
  local max_time="${5:-10}"
  local response_headers="${6:-}"
  local code
  local -a args=(
    --silent
    --output /dev/null
    --write-out '%{http_code}'
    --max-time "${max_time}"
  )
  if [[ -n "${header_file}" ]]; then
    args+=(--header "@${header_file}")
  fi
  if [[ -n "${response_headers}" ]]; then
    args+=(--dump-header "${response_headers}")
  fi
  if [[ "${method}" != "GET" ]]; then
    args+=(--request "${method}")
  fi
  if [[ -n "${data_file}" ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@${data_file}")
  fi
  if ! code="$(curl "${args[@]}" "${url}")"; then
    code="000"
  fi
  printf '%s' "${code}"
}

for unit in opencodex nginx; do
  if systemctl is-active --quiet "${unit}.service"; then
    pass "${unit}.service is active"
  else
    fail "${unit}.service is not active"
  fi
  if systemctl is-enabled --quiet "${unit}.service"; then
    pass "${unit}.service is enabled"
  else
    fail "${unit}.service is not enabled"
  fi
done

for rpc_unit in rpcbind.socket rpcbind.service; do
  rpc_enabled_state="$(systemctl is-enabled "${rpc_unit}" 2>/dev/null || true)"
  if systemctl is-active --quiet "${rpc_unit}"; then
    fail "${rpc_unit} is active but NFS/RPC is outside the pilot scope"
  elif [[ "${rpc_enabled_state}" == "enabled" || "${rpc_enabled_state}" == "enabled-runtime" || \
          "${rpc_enabled_state}" == "linked" || "${rpc_enabled_state}" == "linked-runtime" ]]; then
    fail "${rpc_unit} is enabled but NFS/RPC is outside the pilot scope"
  else
    pass "${rpc_unit} is inactive and not enabled (${rpc_enabled_state:-absent})"
  fi
done

installed_version="$(runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" ocx --version 2>/dev/null || true)"
if [[ -z "${installed_version}" && "${legacy_expected_version_fallback}" == "true" && \
      -f "${package_manifest}" && ! -L "${package_manifest}" ]]; then
  package_version="$(jq -er --arg package_name "${PACKAGE_NAME}" '
    if .name == $package_name and (.version | type == "string") then
      .version
    else
      empty
    end
  ' "${package_manifest}" 2>/dev/null || true)"
  if [[ "${package_version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
    installed_version="opencodex ${package_version}"
  fi
fi
if [[ "${installed_version}" == "opencodex ${expected_opencodex_version}" ]]; then
  pass "OpenCodex version is exactly ${expected_opencodex_version}"
else
  fail "OpenCodex version is not exactly ${expected_opencodex_version} (${installed_version:-unavailable})"
fi
package_version="$(jq -er --arg package_name "${PACKAGE_NAME}" '
  if .name == $package_name and (.version | type == "string") then
    .version
  else
    empty
  end
' "${package_manifest}" 2>/dev/null || true)"
if [[ "${package_version}" == "${expected_opencodex_version}" ]]; then
  pass "OpenCodex package metadata is exactly ${expected_opencodex_version}"
else
  fail "OpenCodex package metadata is ${package_version:-unavailable}; expected ${expected_opencodex_version}"
fi

declared_bun_version="$(jq -er '
  .dependencies.bun
  | select(type == "string")
  | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$"))
' "${package_manifest}" 2>/dev/null || true)"
service_memory="$(runuser -u opencodex -- \
  "${RUNTIME_ADAPTER}" ocx observe memory --json 2>/dev/null || true)"
if [[ -n "${declared_bun_version}" ]] && \
   jq -e --arg version "${declared_bun_version}" \
     '.bunVersion == $version and .bunRuntimeSource == "bundled"' \
     <<< "${service_memory}" >/dev/null 2>&1; then
  pass "Service uses the bundled Bun runtime at exactly ${declared_bun_version}"
else
  fail "Service Bun runtime does not match the exact bundled dependency (${declared_bun_version:-unavailable})"
fi

unit_user="$(systemctl show opencodex.service --property=User --value)"
unit_group="$(systemctl show opencodex.service --property=Group --value)"
main_pid="$(systemctl show opencodex.service --property=MainPID --value)"
if [[ "${unit_user}" == "opencodex" && "${unit_group}" == "opencodex" ]]; then
  pass "OpenCodex unit uses the dedicated user and group"
else
  fail "OpenCodex unit identity is ${unit_user:-unset}:${unit_group:-unset}"
fi
if [[ "${main_pid}" =~ ^[1-9][0-9]*$ ]] && \
   [[ "$(ps -o user= -p "${main_pid}" 2>/dev/null | awk '{print $1}')" == "opencodex" ]]; then
  pass "OpenCodex main process runs as opencodex"
else
  fail "OpenCodex main process owner could not be verified"
fi

unit_exec_start_pre="$(systemctl show opencodex.service --property=ExecStartPre --value)"
unit_exec_start="$(systemctl show opencodex.service --property=ExecStart --value)"
if grep -F -- "argv[]=${RUNTIME_ADAPTER} check" \
    <<< "${unit_exec_start_pre}" >/dev/null; then
  pass "OpenCodex ExecStartPre validates the runtime adapter"
else
  fail "OpenCodex ExecStartPre does not validate the runtime adapter"
fi
if grep -F -- "argv[]=${RUNTIME_ADAPTER} ocx config validate" \
    <<< "${unit_exec_start_pre}" >/dev/null; then
  pass "OpenCodex ExecStartPre uses the runtime adapter"
else
  fail "OpenCodex ExecStartPre does not use the runtime adapter"
fi
if grep -F -- "argv[]=${RUNTIME_ADAPTER} ocx start --port 10100" \
    <<< "${unit_exec_start}" >/dev/null; then
  pass "OpenCodex ExecStart uses the runtime adapter"
else
  fail "OpenCodex ExecStart does not use the runtime adapter"
fi

check_unit_value() {
  local property="$1"
  local expected="$2"
  local actual
  actual="$(systemctl show opencodex.service --property="${property}" --value)"
  if [[ "${actual}" == "${expected}" ]]; then
    pass "${property}=${expected}"
  else
    fail "${property} is ${actual:-unset}; expected ${expected}"
  fi
}
check_unit_value MemoryHigh 681574400
check_unit_value MemoryMax 838860800
check_unit_value MemorySwapMax 2147483648
check_unit_value TasksMax 256
check_unit_value Restart always

unit_environment="$(systemctl show opencodex.service --property=Environment --value)"
if grep -Eq '(^|[[:space:]])OCX_SERVICE=1($|[[:space:]])' <<< "${unit_environment}"; then
  pass "OCX_SERVICE=1 is present in the service environment"
else
  fail "OCX_SERVICE=1 is missing from the service environment"
fi

mapfile -t pilot_listeners < <(ss -lntH | awk '$4 ~ /:(10100|18080)$/ {print $4}')
seen_ocx=false
seen_gateway=false
for listener in "${pilot_listeners[@]}"; do
  case "${listener}" in
    127.0.0.1:10100) seen_ocx=true ;;
    127.0.0.1:18080) seen_gateway=true ;;
    *) fail "non-loopback pilot listener found: ${listener}" ;;
  esac
done
[[ "${seen_ocx}" == "true" ]] && pass "OpenCodex listens on 127.0.0.1:10100" || fail "OpenCodex loopback listener is missing"
[[ "${seen_gateway}" == "true" ]] && pass "Nginx listens on 127.0.0.1:18080" || fail "Nginx loopback listener is missing"

mapfile -t wildcard_tcp_listeners < <(ss -lntH | awk '$4 ~ /^(0\.0\.0\.0|\*|\[::\]):/ {print $4}')
seen_ssh=false
for listener in "${wildcard_tcp_listeners[@]}"; do
  case "${listener}" in
    0.0.0.0:22|\*:22|\[::\]:22) seen_ssh=true; pass "SSH is the only allowed wildcard TCP listener (${listener})" ;;
    *) fail "unexpected wildcard TCP listener found: ${listener}" ;;
  esac
done
[[ "${seen_ssh}" == "true" ]] && pass "OpenSSH wildcard listener on port 22 is available" || fail "OpenSSH wildcard listener on port 22 is missing"
if ss -lnutH | awk '$4 ~ /:111$/ {found=1} END {exit !found}'; then
  fail "TCP or UDP port 111 is still listening"
else
  pass "TCP and UDP port 111 have no listener"
fi

if curl --fail --silent --show-error --max-time 5 "${OCX_URL}/healthz" | jq -e . >/dev/null; then
  pass "OpenCodex health endpoint returns JSON"
else
  fail "OpenCodex health endpoint failed"
fi

gateway_key=""
if [[ -f "${GATEWAY_KEY_FILE}" && ! -L "${GATEWAY_KEY_FILE}" ]]; then
  key_stat="$(stat -c '%U:%G:%a' "${GATEWAY_KEY_FILE}")"
  [[ "${key_stat}" == "root:root:600" ]] && pass "gateway key file is root:root 0600" || fail "gateway key file mode is ${key_stat}"
  gateway_key="$(<"${GATEWAY_KEY_FILE}")"
  [[ "${gateway_key}" =~ ^[A-Za-z0-9_-]{32,128}$ ]] && pass "gateway key has the required format" || fail "gateway key has an invalid format"
else
  fail "gateway key file is missing or unsafe"
fi

if [[ -f "${GATEWAY_MAP_FILE}" && ! -L "${GATEWAY_MAP_FILE}" ]]; then
  map_stat="$(stat -c '%U:%G:%a' "${GATEWAY_MAP_FILE}")"
  [[ "${map_stat}" == "root:root:600" ]] && pass "Nginx key map is root:root 0600" || fail "Nginx key map mode is ${map_stat}"
else
  fail "Nginx key map is missing or unsafe"
fi

voice_enabled=""
if [[ -f "${FEATURE_FLAGS_FILE}" && ! -L "${FEATURE_FLAGS_FILE}" && \
      "$(stat -c '%U:%G:%a' "${FEATURE_FLAGS_FILE}")" == "root:root:600" ]]; then
  voice_enabled="$(awk '$1 == "set" && $2 == "$opencodex_voice_enabled" { gsub(";", "", $3); print $3; exit }' "${FEATURE_FLAGS_FILE}")"
  case "${voice_enabled}" in
    0|1) pass "voice feature flag is explicit (${voice_enabled})" ;;
    *) fail "voice feature flag is invalid or missing" ;;
  esac
else
  fail "voice feature flags file is missing or unsafe"
fi

if [[ -n "${gateway_key}" ]]; then
  printf 'X-OpenCodex-API-Key: %s\nExpect:\n' "${gateway_key}" > "${valid_header_file}"
  printf 'X-OpenCodex-API-Key: definitely-invalid-pilot-key\nExpect:\n' > "${invalid_header_file}"
  printf '%s\n' \
    "X-OpenCodex-API-Key: ${gateway_key}" \
    'Connection: Upgrade' \
    'Upgrade: websocket' \
    'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
    'Sec-WebSocket-Version: 13' \
    'Expect:' > "${responses_websocket_headers}"
  chmod 0600 "${valid_header_file}" "${invalid_header_file}" "${responses_websocket_headers}"

  code="$(curl_code GET "${GATEWAY_URL}/v1/models" "" "" 10 "${missing_key_response_headers}")"
  if [[ "${code}" == "401" ]] && \
     tr -d '\r' < "${missing_key_response_headers}" | \
       grep -Fxi -- "${EXPECTED_GATEWAY_REJECTION_HEADER}" >/dev/null; then
    pass "gateway rejects a missing API key with the origin marker"
  else
    fail "missing API key returned HTTP ${code} or lacked the origin marker"
  fi

  code="$(curl_code GET "${GATEWAY_URL}/v1/models" "${invalid_header_file}" "" 10 "${invalid_key_response_headers}")"
  if [[ "${code}" == "401" ]] && \
     tr -d '\r' < "${invalid_key_response_headers}" | \
       grep -Fxi -- "${EXPECTED_GATEWAY_REJECTION_HEADER}" >/dev/null; then
    pass "gateway rejects an invalid API key with the origin marker"
  else
    fail "invalid API key returned HTTP ${code} or lacked the origin marker"
  fi

  code="$(curl_code GET "${GATEWAY_URL}/__gateway_health" "${valid_header_file}")"
  [[ "${code}" == "200" ]] && pass "authorized gateway health is 200" || fail "authorized gateway health returned HTTP ${code}"

  if [[ "${voice_enabled}" == "0" ]]; then
    # /v1/live accepts POST when enabled. Probe it with that allowed method so
    # the feature gate, rather than limit_except's GET denial, proves it is off.
    for method_path in "POST /v1/live" "GET /v1/realtime"; do
      method="${method_path%% *}"
      path="${method_path#* }"
      code="$(curl_code "${method}" "${GATEWAY_URL}${path}" "${valid_header_file}")"
      [[ "${code}" == "404" ]] && pass "gateway keeps Voice disabled at ${path}" || fail "disabled Voice path ${path} returned HTTP ${code}"
    done
  else
    pass "Voice is explicitly enabled; validate a real media session separately"
  fi

  code="$(curl_code GET "${GATEWAY_URL}/v1/models" "${valid_header_file}" "" 30)"
  [[ "${code}" == "200" ]] && pass "authorized /v1/models is 200" || fail "authorized /v1/models returned HTTP ${code}"

  for path in / /healthz /api/config /v1/chat/completions; do
    code="$(curl_code GET "${GATEWAY_URL}${path}" "${valid_header_file}")"
    [[ "${code}" == "404" ]] && pass "gateway blocks ${path}" || fail "blocked path ${path} returned HTTP ${code}"
  done

  # Hold one authorized request body open, then prove Nginx still admits a sibling
  # endpoint and that a disabled Responses WebSocket probe still reaches OpenCodex's
  # deterministic 426 fallback. The held request never reaches routing. The sibling
  # body is deliberately malformed: it must reach OpenCodex and return its local 400
  # before provider/model routing, rather than making this edge control depend on an
  # upstream model's time-to-first-byte. Real generation overlap is exercised by the
  # credentialed external smoke.
  {
    printf '{"model":"concurrency-probe","input":"'
    head -c 1048576 /dev/zero | tr '\0' x
    printf '"}'
  } > "${hold_payload}"
  printf '{"model":\n' > "${second_payload}"
  curl \
    --silent \
    --output /dev/null \
    --max-time 20 \
    --limit-rate 8k \
    --request POST \
    --header "@${valid_header_file}" \
    --header 'Content-Type: application/json' \
    --data-binary "@${hold_payload}" \
    "${GATEWAY_URL}/v1/responses" &
  hold_pid=$!
  sleep 1
  if kill -0 "${hold_pid}" 2>/dev/null; then
    code="$(curl_code POST "${GATEWAY_URL}/v1/responses/compact" "${valid_header_file}" "${second_payload}" 5 "${overlap_response_headers}")"
    if [[ "${code}" == "400" ]] && \
       ! grep -Fqi 'x-opencodex-concurrency-limit:' "${overlap_response_headers}"; then
      pass "generation endpoints admit the local parse control during an HTTP generation"
    else
      fail "generation overlap control returned HTTP ${code}, expected local 400 without the Nginx safety guard"
    fi
    code="$(curl_code GET "${GATEWAY_URL}/v1/responses" "${responses_websocket_headers}" "" 5)"
    [[ "${code}" == "426" ]] \
      && pass "Responses WebSocket fallback remains 426 during an HTTP generation" \
      || fail "Responses WebSocket fallback returned HTTP ${code}, expected 426"
  else
    fail "the held overlap probe exited before the sibling requests"
  fi
  kill "${hold_pid}" 2>/dev/null || true
  wait "${hold_pid}" 2>/dev/null || true
  hold_pid=""
fi

generation_guard_count="$(grep -Fc \
  "limit_conn opencodex_generation ${EXPECTED_GENERATION_SAFETY_LIMIT};" \
  "${GATEWAY_CONFIG_FILE}" 2>/dev/null || true)"
websocket_guard_count="$(grep -Fc \
  "limit_conn opencodex_responses_websocket ${EXPECTED_GENERATION_SAFETY_LIMIT};" \
  "${GATEWAY_CONFIG_FILE}" 2>/dev/null || true)"
if [[ "${generation_guard_count}" == "5" && "${websocket_guard_count}" == "1" ]]; then
  pass "Nginx uses separate generation and Responses WebSocket safety guards at ${EXPECTED_GENERATION_SAFETY_LIMIT}"
else
  fail "Nginx safety guards are generation=${generation_guard_count:-0}, websocket=${websocket_guard_count:-0}; expected 5 and 1"
fi

if nginx -t >/dev/null 2>&1; then
  pass "Nginx configuration is valid"
else
  fail "Nginx configuration validation failed"
fi

if logrotate --debug /etc/logrotate.conf > "${logrotate_debug}" 2>&1; then
  pass "logrotate configuration is valid and has no duplicate entries"
else
  fail "logrotate configuration validation failed"
fi

swap_file_bytes="$(stat -c '%s' /swapfile 2>/dev/null || true)"
swap_active_bytes="$(swapon --bytes --noheadings --show=NAME,SIZE | awk '$1 == "/swapfile" {print $2}')"
if [[ "${swap_file_bytes}" == "${EXPECTED_SWAP_FILE_BYTES}" && \
      "${swap_active_bytes}" == "${EXPECTED_ACTIVE_SWAP_BYTES}" ]]; then
  pass "/swapfile is a 4 GiB file with one page reserved by mkswap"
else
  fail "/swapfile is ${swap_file_bytes:-missing} file bytes and ${swap_active_bytes:-inactive} usable bytes; expected ${EXPECTED_SWAP_FILE_BYTES} and ${EXPECTED_ACTIVE_SWAP_BYTES}"
fi
if grep -Eq '^/swapfile[[:space:]]+none[[:space:]]+swap([[:space:]]|$)' /etc/fstab; then
  pass "/swapfile is persistent in /etc/fstab"
else
  fail "/swapfile is missing from /etc/fstab"
fi
[[ "$(sysctl -n vm.swappiness)" == "10" ]] && pass "vm.swappiness=10" || fail "vm.swappiness is not 10"
[[ "$(sysctl -n vm.vfs_cache_pressure)" == "50" ]] && pass "vm.vfs_cache_pressure=50" || fail "vm.vfs_cache_pressure is not 50"

gateway_key=""
if ((failures > 0)); then
  printf '%d smoke test(s) failed.\n' "${failures}" >&2
  exit 1
fi

printf 'All local smoke tests passed. SSE, interactive Cloudflare SSH, Access/Tunnel, and reboot tests remain separate.\n'

#!/usr/bin/env bash
set -euo pipefail

readonly PUBLIC_BASE_URL="${PUBLIC_BASE_URL:-}"
readonly GATEWAY_KEY_FILE="${GATEWAY_KEY_FILE:-/etc/opencodex/gateway-api-key}"
readonly EXPECTED_ACCESS_AUD="${EXPECTED_ACCESS_AUD:-}"
readonly EXPECTED_GATEWAY_MARKER="pw-api-v1"
readonly EXPECTED_GATEWAY_REJECTION_HEADER="X-OpenCodex-Gateway-Rejection: api-key"
readonly MAX_OVERLAP_ATTEMPTS=3
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SSE_VALIDATOR="${SCRIPT_DIR}/validate-responses-sse.py"

failures=0
tmp_dir=""
sse_pid=""
cf_access_client_id=""
cf_access_client_secret=""
gateway_key=""

pass() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

cleanup() {
  if [[ -n "${sse_pid}" ]] && kill -0 "${sse_pid}" 2>/dev/null; then
    kill "${sse_pid}" 2>/dev/null || true
    wait "${sse_pid}" 2>/dev/null || true
  fi
  [[ -z "${tmp_dir}" ]] || rm -rf -- "${tmp_dir}"
  cf_access_client_id=""
  cf_access_client_secret=""
  gateway_key=""
}
trap cleanup EXIT INT TERM

if [[ "${EUID}" -ne 0 ]]; then
  printf 'ERROR: run as root so the gateway key can be read without copying it.\n' >&2
  exit 2
fi
if [[ ! "${PUBLIC_BASE_URL}" =~ ^https://[^/?#]+$ ]]; then
  printf 'ERROR: set PUBLIC_BASE_URL to the HTTPS origin without a path.\n' >&2
  exit 2
fi
if [[ ! "${EXPECTED_ACCESS_AUD}" =~ ^[A-Fa-f0-9]{64}$ ]]; then
  printf 'ERROR: set EXPECTED_ACCESS_AUD to the 64-character Access application AUD.\n' >&2
  exit 2
fi
if [[ ! -t 0 || ! -t 1 ]]; then
  printf 'ERROR: run from an interactive terminal; credentials are read from hidden prompts.\n' >&2
  exit 2
fi

for command_name in curl grep jq mktemp python3 seq stat timeout tr; do
  if ! command -v "${command_name}" >/dev/null; then
    printf 'ERROR: required command is missing: %s\n' "${command_name}" >&2
    exit 2
  fi
done
if [[ ! -f "${GATEWAY_KEY_FILE}" || -L "${GATEWAY_KEY_FILE}" ]]; then
  printf 'ERROR: gateway key file is missing or unsafe.\n' >&2
  exit 2
fi
if [[ "$(stat -c '%U:%G:%a' "${GATEWAY_KEY_FILE}")" != "root:root:600" ]]; then
  printf 'ERROR: gateway key file must be root:root 0600.\n' >&2
  exit 2
fi
if [[ ! -f "${SSE_VALIDATOR}" || -L "${SSE_VALIDATOR}" ]]; then
  printf 'ERROR: SSE validator is missing or unsafe.\n' >&2
  exit 2
fi

read -r -s -p 'Cloudflare Access Client ID: ' cf_access_client_id
printf '\n'
read -r -s -p 'Cloudflare Access Client Secret: ' cf_access_client_secret
printf '\n'
if [[ -z "${cf_access_client_id}" || -z "${cf_access_client_secret}" ]]; then
  printf 'ERROR: both Cloudflare Access values are required.\n' >&2
  exit 2
fi

gateway_key="$(<"${GATEWAY_KEY_FILE}")"
if [[ ! "${gateway_key}" =~ ^[A-Za-z0-9_-]{32,128}$ ]]; then
  printf 'ERROR: gateway key has an invalid format.\n' >&2
  exit 2
fi
umask 077
tmp_dir="$(mktemp -d)"
readonly valid_headers="${tmp_dir}/valid.headers"
readonly invalid_gateway_headers="${tmp_dir}/invalid-gateway.headers"
readonly gateway_only_headers="${tmp_dir}/gateway-only.headers"
readonly access_denied_headers="${tmp_dir}/access-denied.headers"
readonly invalid_gateway_response_headers="${tmp_dir}/invalid-gateway-response.headers"
readonly models_headers="${tmp_dir}/models.headers"
readonly models_body="${tmp_dir}/models.json"
readonly live_payload="${tmp_dir}/live.json"
readonly second_payload="${tmp_dir}/second.json"
readonly sse_headers="${tmp_dir}/sse.headers"
readonly sse_body="${tmp_dir}/sse.txt"
readonly sse_code_file="${tmp_dir}/sse.code"
readonly second_headers="${tmp_dir}/second.headers"
readonly second_body="${tmp_dir}/second.txt"
readonly second_code_file="${tmp_dir}/second.code"

printf '%s\n' \
  "CF-Access-Client-Id: ${cf_access_client_id}" \
  "CF-Access-Client-Secret: ${cf_access_client_secret}" \
  "X-OpenCodex-API-Key: ${gateway_key}" \
  'Expect:' > "${valid_headers}"
printf '%s\n' \
  "CF-Access-Client-Id: ${cf_access_client_id}" \
  "CF-Access-Client-Secret: ${cf_access_client_secret}" \
  'X-OpenCodex-API-Key: definitely-invalid-pilot-key' \
  'Expect:' > "${invalid_gateway_headers}"
printf '%s\n' \
  "X-OpenCodex-API-Key: ${gateway_key}" \
  'Expect:' > "${gateway_only_headers}"

http_code="$(curl --silent --show-error --output /dev/null --dump-header "${access_denied_headers}" \
  --write-out '%{http_code}' --max-time 30 --header "@${gateway_only_headers}" \
  "${PUBLIC_BASE_URL}/v1/models" || true)"
if [[ "${http_code}" == "401" ]] && \
   grep -Fqi "cf-access-aud: ${EXPECTED_ACCESS_AUD}" "${access_denied_headers}"; then
  pass 'Cloudflare Access rejects a valid gateway request without a service token'
else
  fail "gateway-only request was HTTP ${http_code:-000} or lacked the expected Cloudflare Access AUD"
fi

models_ok=false
http_code="$(curl --silent --show-error --output "${models_body}" --dump-header "${models_headers}" \
  --write-out '%{http_code}' --max-time 60 --header "@${valid_headers}" \
  "${PUBLIC_BASE_URL}/v1/models" || true)"
if [[ "${http_code}" == "200" ]] && \
   jq -e '.data | type == "array"' "${models_body}" >/dev/null && \
   grep -Fqi "x-opencodex-gateway: ${EXPECTED_GATEWAY_MARKER}" "${models_headers}"; then
  models_ok=true
  pass 'valid Access and gateway credentials reach /v1/models'
else
  fail "authorized /v1/models returned HTTP ${http_code:-000}, invalid JSON, or no origin marker"
fi

http_code="$(curl --silent --show-error --output /dev/null \
  --dump-header "${invalid_gateway_response_headers}" --write-out '%{http_code}' \
  --max-time 30 --header "@${invalid_gateway_headers}" "${PUBLIC_BASE_URL}/v1/models" || true)"
if [[ "${http_code}" == "401" ]] && \
   tr -d '\r' < "${invalid_gateway_response_headers}" | \
     grep -Fxi -- "${EXPECTED_GATEWAY_REJECTION_HEADER}" >/dev/null; then
  pass 'Nginx rejects an invalid gateway key after Access admission'
else
  fail "invalid gateway key was HTTP ${http_code:-000} or lacked the Nginx rejection marker"
fi

http_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 30 --header "@${valid_headers}" "${PUBLIC_BASE_URL}/api/config" || true)"
[[ "${http_code}" == "404" ]] \
  && pass 'the public route blocks the OpenCodex management API' \
  || fail "authorized /api/config returned HTTP ${http_code:-000}, expected 404"

model_id="${MODEL_ID:-}"
if [[ "${models_ok}" == "true" && -z "${model_id}" ]]; then
  model_id="$(jq -r 'first(.data[]?.id // empty) // empty' "${models_body}" 2>/dev/null || true)"
fi
if [[ "${models_ok}" != "true" ]]; then
  fail 'live SSE test skipped because the authorized model listing failed'
elif [[ -z "${model_id}" ]]; then
  fail 'no model ID is available for the live SSE test'
else
  printf 'Live test model: %s\n' "${model_id}"
  jq -nc --arg model "${model_id}" \
    '{model:$model,input:[{type:"message",role:"user",content:[{type:"input_text",text:"Write the integers 1 through 250, one per line, with no commentary or abbreviation."}]}],stream:true,store:false,max_output_tokens:1400}' \
    > "${live_payload}"
  jq -nc --arg model "${model_id}" \
    '{model:$model,input:[{type:"message",role:"user",content:[{type:"input_text",text:"Reply with exactly OK."}]}],stream:true,store:false,max_output_tokens:16}' \
    > "${second_payload}"

  overlap_verified=false
  sse_verified=false
  for attempt in $(seq 1 "${MAX_OVERLAP_ATTEMPTS}"); do
    : > "${sse_headers}"
    : > "${sse_body}"
    : > "${sse_code_file}"
    : > "${second_headers}"
    : > "${second_body}"
    : > "${second_code_file}"

    timeout 240 curl --no-buffer --silent --show-error \
      --output "${sse_body}" --dump-header "${sse_headers}" --write-out '%{http_code}' \
      --header "@${valid_headers}" --header 'Content-Type: application/json' \
      --data-binary "@${live_payload}" "${PUBLIC_BASE_URL}/v1/responses" \
      > "${sse_code_file}" &
    sse_pid=$!

    stream_created=false
    for _ in $(seq 1 120); do
      if python3 "${SSE_VALIDATOR}" has-event "${sse_body}" response.created \
           >/dev/null 2>&1 && kill -0 "${sse_pid}" 2>/dev/null; then
        stream_created=true
        break
      fi
      if ! kill -0 "${sse_pid}" 2>/dev/null; then
        break
      fi
      sleep 0.25
    done

    overlap_verified_this_attempt=false
    if [[ "${stream_created}" == "true" ]] && kill -0 "${sse_pid}" 2>/dev/null; then
      if timeout 120 curl --no-buffer --silent --show-error \
        --output "${second_body}" --dump-header "${second_headers}" --write-out '%{http_code}' \
        --header "@${valid_headers}" --header 'Content-Type: application/json' \
        --data-binary "@${second_payload}" "${PUBLIC_BASE_URL}/v1/responses" \
        > "${second_code_file}"; then
        second_code="$(<"${second_code_file}")"
        if [[ "${second_code}" == "200" ]] && \
           second_summary="$(python3 "${SSE_VALIDATOR}" validate "${second_headers}" "${second_body}" 2>&1)"; then
          overlap_verified_this_attempt=true
        else
          printf 'INFO: overlap attempt %d returned HTTP %s or invalid SSE: %s\n' \
            "${attempt}" "${second_code:-000}" "${second_summary:-no summary}"
        fi
      else
        printf 'INFO: overlap attempt %d failed or timed out.\n' "${attempt}"
      fi
    fi

    if wait "${sse_pid}"; then
      sse_pid=""
      sse_code="$(<"${sse_code_file}")"
      if [[ "${sse_code}" == "200" ]] && \
         sse_summary="$(python3 "${SSE_VALIDATOR}" validate "${sse_headers}" "${sse_body}" 2>&1)"; then
        sse_verified=true
      else
        fail "public SSE attempt ${attempt} failed validation (HTTP ${sse_code:-000}): ${sse_summary:-no summary}"
        break
      fi
    else
      sse_pid=""
      fail "public SSE attempt ${attempt} failed or timed out"
      break
    fi

    if [[ "${overlap_verified_this_attempt}" == "true" ]]; then
      overlap_verified=true
      break
    fi
    if ((attempt < MAX_OVERLAP_ATTEMPTS)); then
      printf 'INFO: overlap attempt %d was inconclusive; retrying.\n' "${attempt}"
    fi
  done

  [[ "${sse_verified}" == "true" ]] \
    && pass "authenticated public SSE completed (${sse_summary})" \
    || fail 'no authenticated public SSE request completed successfully'
  [[ "${overlap_verified}" == "true" ]] \
    && pass "a sibling public Responses request completed while the first stream was active (${second_summary})" \
    || fail "parallel Responses admission remained unproven after ${MAX_OVERLAP_ATTEMPTS} bounded attempts"
fi

if ((failures > 0)); then
  printf '%d external smoke test(s) failed.\n' "${failures}" >&2
  exit 1
fi

printf 'All external Access, gateway, SSE, and overlap tests passed.\n'

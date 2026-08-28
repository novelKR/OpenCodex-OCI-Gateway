#!/usr/bin/env bash

set -Eeuo pipefail

readonly CONFIG_FILE="${OCI_CONFIG_FILE:-${HOME}/.oci/config}"
readonly PROFILE_NAME="${OCI_PROFILE:-OCI_SESSION}"
readonly REGION_NAME="${OCI_REGION:-ap-osaka-1}"
readonly AVAILABILITY_DOMAIN="${OCI_AVAILABILITY_DOMAIN:-}"
readonly INSTANCE_NAME="${OCI_INSTANCE_NAME:-A1-Ubuntu-2404}"
readonly SUBNET_NAME="${OCI_SUBNET_NAME:-A1-Osaka-Public-Subnet}"
readonly PUBLIC_KEY_FILE="${OCI_SSH_PUBLIC_KEY_FILE:-${HOME}/.ssh/oci-a1-osaka.pub}"
readonly RETRY_INTERVAL_SECONDS="${OCI_RETRY_INTERVAL_SECONDS:-60}"
readonly SESSION_REFRESH_SECONDS="${OCI_SESSION_REFRESH_SECONDS:-1800}"
readonly SHAPE_OCPUS="${OCI_OCPUS:-2}"
readonly SHAPE_MEMORY_GB="${OCI_MEMORY_GB:-12}"
readonly BOOT_VOLUME_GB="${OCI_BOOT_VOLUME_GB:-50}"

die() {
  printf 'FATAL %s\n' "$*" >&2
  exit 2
}

for command_name in oci jq awk; do
  command -v "${command_name}" >/dev/null 2>&1 || die "${command_name} is required"
done

[[ -r "${CONFIG_FILE}" ]] || die "OCI config is not readable: ${CONFIG_FILE}"
[[ -n "${AVAILABILITY_DOMAIN}" ]] || die "OCI_AVAILABILITY_DOMAIN is required"
[[ -r "${PUBLIC_KEY_FILE}" ]] || die "SSH public key is not readable: ${PUBLIC_KEY_FILE}"

for numeric_value in \
  "${RETRY_INTERVAL_SECONDS}" \
  "${SESSION_REFRESH_SECONDS}" \
  "${SHAPE_OCPUS}" \
  "${SHAPE_MEMORY_GB}" \
  "${BOOT_VOLUME_GB}"; do
  [[ "${numeric_value}" =~ ^[1-9][0-9]*$ ]] || die "numeric settings must be positive integers"
done

[[ "${INSTANCE_NAME}" != *"'"* ]] || die "OCI_INSTANCE_NAME must not contain a single quote"

OCI_BASE=(
  --config-file "$CONFIG_FILE"
  --profile "$PROFILE_NAME"
  --auth security_token
  --region "$REGION_NAME"
)

TENANCY_ID="${OCI_TENANCY_ID:-$(awk -F= '$1 == "tenancy" { print $2; exit }' "$CONFIG_FILE")}"
if [[ ! "$TENANCY_ID" =~ ^ocid1\.tenancy\. ]]; then
  echo "FATAL invalid tenancy identifier"
  exit 2
fi
COMPARTMENT_ID="${OCI_COMPARTMENT_ID:-${TENANCY_ID}}"
if [[ ! "${COMPARTMENT_ID}" =~ ^ocid1\.(tenancy|compartment)\. ]]; then
  die "invalid compartment identifier"
fi

SUBNET_ID=$(oci network subnet list \
  --compartment-id "$COMPARTMENT_ID" \
  --display-name "$SUBNET_NAME" \
  --all \
  "${OCI_BASE[@]}" \
  --query 'data[0].id' \
  --raw-output)
if [[ ! "$SUBNET_ID" =~ ^ocid1\.subnet\. ]]; then
  echo "FATAL subnet not found"
  exit 3
fi

IMAGE_ID=$(oci compute image list \
  --compartment-id "$COMPARTMENT_ID" \
  --shape VM.Standard.A1.Flex \
  --operating-system 'Canonical Ubuntu' \
  --operating-system-version 24.04 \
  --lifecycle-state AVAILABLE \
  --sort-by TIMECREATED \
  --sort-order DESC \
  --limit 1 \
  "${OCI_BASE[@]}" \
  --query 'data[0].id' \
  --raw-output)
if [[ ! "$IMAGE_ID" =~ ^ocid1\.image\. ]]; then
  echo "FATAL compatible Ubuntu image not found"
  exit 4
fi

SHAPE_CONFIG="$(jq -nc \
  --argjson ocpus "${SHAPE_OCPUS}" \
  --argjson memory "${SHAPE_MEMORY_GB}" \
  '{ocpus:$ocpus,memoryInGBs:$memory}')"
SHAPE_AVAILABILITIES="$(jq -nc \
  --argjson config "${SHAPE_CONFIG}" \
  '[{instanceShape:"VM.Standard.A1.Flex",instanceShapeConfig:$config}]')"
last_refresh=$(date +%s)
attempt=0

while true; do
  attempt=$((attempt + 1))
  now=$(date +%s)

  if (( now - last_refresh >= SESSION_REFRESH_SECONDS )); then
    if oci session refresh \
      --config-file "$CONFIG_FILE" \
      --profile "$PROFILE_NAME" \
      --auth security_token >/dev/null; then
      last_refresh=$now
      printf '%s SESSION_REFRESHED\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    else
      printf '%s SESSION_REFRESH_FAILED\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    fi
  fi

  if existing_instances=$(oci compute instance list \
    --compartment-id "$COMPARTMENT_ID" \
    --display-name "$INSTANCE_NAME" \
    --all \
    "${OCI_BASE[@]}" \
    2>&1); then
    :
  else
    printf '%s INSTANCE_LOOKUP_ERROR attempt=%d\n%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$attempt" "$existing_instances" >&2
    exit 5
  fi
  existing_id=$(jq -r \
    --arg ad "$AVAILABILITY_DOMAIN" \
    --argjson ocpus "$SHAPE_OCPUS" \
    --argjson memory "$SHAPE_MEMORY_GB" \
    '
      first(
        .data[]?
        | select(
            .["availability-domain"] == $ad
            and .shape == "VM.Standard.A1.Flex"
            and .["lifecycle-state"] != "TERMINATED"
            and .["lifecycle-state"] != "TERMINATING"
            and .["shape-config"].ocpus == $ocpus
            and .["shape-config"]["memory-in-gbs"] == $memory
          )
        | .id
      ) // empty
    ' <<<"${existing_instances}" 2>/dev/null || true)
  if [[ "$existing_id" =~ ^ocid1\.instance\. ]]; then
    printf '%s INSTANCE_FOUND %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$existing_id"
    exit 0
  fi

  if capacity=$(oci compute compute-capacity-report create \
    --compartment-id "$COMPARTMENT_ID" \
    --availability-domain "$AVAILABILITY_DOMAIN" \
    --shape-availabilities "$SHAPE_AVAILABILITIES" \
    "${OCI_BASE[@]}" \
    --connection-timeout 30 \
    --read-timeout 60 \
    --no-retry \
    --query 'data."shape-availabilities"[0]."availability-status"' \
    --raw-output 2>&1); then
    capacity_rc=0
  else
    capacity_rc=$?
  fi

  if (( capacity_rc != 0 )); then
    printf '%s CAPACITY_CHECK_ERROR attempt=%d\n%s\n' \
      "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$attempt" "$capacity" >&2
    exit 6
  fi

  printf '%s CAPACITY=%s attempt=%d\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$capacity" "$attempt"
  if [[ "$capacity" != "AVAILABLE" ]]; then
    sleep "$RETRY_INTERVAL_SECONDS"
    continue
  fi

  if launch_output=$(oci compute instance launch \
    --compartment-id "$COMPARTMENT_ID" \
    --availability-domain "$AVAILABILITY_DOMAIN" \
    --shape VM.Standard.A1.Flex \
    --shape-config "$SHAPE_CONFIG" \
    --image-id "$IMAGE_ID" \
    --subnet-id "$SUBNET_ID" \
    --assign-public-ip true \
    --display-name "$INSTANCE_NAME" \
    --hostname-label a1ubuntu \
    --ssh-authorized-keys-file "$PUBLIC_KEY_FILE" \
    --boot-volume-size-in-gbs "$BOOT_VOLUME_GB" \
    "${OCI_BASE[@]}" \
    --connection-timeout 60 \
    --read-timeout 180 \
    --no-retry \
    --query 'data.{id:id,state:"lifecycle-state"}' 2>&1); then
    launch_rc=0
  else
    launch_rc=$?
  fi

  if (( launch_rc == 0 )); then
    printf '%s LAUNCH_ACCEPTED %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$launch_output"
    exit 0
  fi

  if [[ "$launch_output" == *"Out of host capacity"* ]]; then
    printf '%s LAUNCH_RACE_OUT_OF_CAPACITY attempt=%d\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$attempt"
    sleep "$RETRY_INTERVAL_SECONDS"
    continue
  fi

  printf '%s LAUNCH_ERROR attempt=%d\n%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$attempt" "$launch_output" >&2
  exit 7
done

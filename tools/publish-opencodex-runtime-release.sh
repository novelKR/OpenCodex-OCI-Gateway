#!/usr/bin/env bash
# Publish exactly one signed runtime manifest pair without changing releases/latest.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage:
  publish-opencodex-runtime-release.sh ARTIFACT_VERSION --repo OWNER/REPO \
    --source-revision COMMIT --input DIR --public-key PEM

The repository must be public with immutable releases enabled. The input
directory must contain exactly opencodex-runtime-ARTIFACT_VERSION.json and its
.sig file. Existing tags and releases are never moved or overwritten.
USAGE
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

sha256_file() {
  if command -v shasum >/dev/null; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    sha256sum "$1" | awk '{print $1}'
  fi
}

decode_base64() {
  local source="$1"
  local destination="$2"
  if base64 -D < "$source" > "$destination" 2>/dev/null; then
    return 0
  fi
  base64 --decode < "$source" > "$destination" 2>/dev/null || \
    die 'runtime signature is not valid base64'
}

artifact_version="${1:-}"
[[ -n "$artifact_version" ]] || { usage >&2; exit 2; }
shift
[[ "$artifact_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-r([1-9][0-9]*)$ ]] || \
  die 'ARTIFACT_VERSION must be strict <semver>-r<N>'

repository=""
source_revision=""
input_dir=""
public_key=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo) repository="${2:-}"; shift 2 ;;
    --source-revision) source_revision="${2:-}"; shift 2 ;;
    --input) input_dir="${2:-}"; shift 2 ;;
    --public-key) public_key="${2:-}"; shift 2 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ "$repository" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || \
  die '--repo must be OWNER/REPO'
[[ "$source_revision" =~ ^[0-9a-f]{40}$ ]] || \
  die '--source-revision must be a full lowercase commit ID'
[[ -d "$input_dir" && ! -L "$input_dir" ]] || die '--input must be a regular directory'
[[ -f "$public_key" && ! -L "$public_key" ]] || die '--public-key must be a regular PEM file'
for command in base64 cmp gh jq openssl python3; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done
command -v shasum >/dev/null || command -v sha256sum >/dev/null || \
  die 'a SHA-256 command is required'

readonly release_tag="opencodex-runtime-${artifact_version}"
readonly manifest_name="${release_tag}.json"
readonly signature_name="${release_tag}.sig"
readonly manifest="${input_dir}/${manifest_name}"
readonly signature="${input_dir}/${signature_name}"
[[ -f "$manifest" && ! -L "$manifest" && -s "$manifest" ]] || die 'runtime manifest is unavailable'
[[ -f "$signature" && ! -L "$signature" && -s "$signature" ]] || die 'runtime signature is unavailable'
actual_files="$(find "$input_dir" -mindepth 1 -maxdepth 1 -exec basename {} \; | LC_ALL=C sort)"
expected_files="$(printf '%s\n%s\n' "$manifest_name" "$signature_name" | LC_ALL=C sort)"
[[ "$actual_files" == "$expected_files" ]] || die 'runtime release input is not the exact two-asset set'

readonly script_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-runtime-release.XXXXXX")"
release_attempted=false
release_complete=false
release_operation=""
manifest_digest=""
signature_digest=""
manifest_size=""
signature_size=""
cleanup() {
  local status=$?
  set +e
  if [[ "$status" -ne 0 && "$release_attempted" == true && "$release_complete" != true ]]; then
    cleanup_release="${temporary}/cleanup-release.json"
    cleanup_release_error="${temporary}/cleanup-release.error"
    if GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
      "repos/${repository}/releases/tags/${release_tag}" \
      >"$cleanup_release" 2>"$cleanup_release_error"; then
      if jq -e '.immutable == true' "$cleanup_release" >/dev/null 2>&1; then
        printf 'ERROR: incomplete runtime release became immutable; stop runtime enrollment and investigate manually\n' >&2
      elif jq -e \
        --arg tag "$release_tag" \
        --arg revision "$source_revision" \
        --arg operation "opencodex-runtime-release-operation:${release_operation}" \
        --arg manifest "$manifest_name" \
        --arg signature "$signature_name" \
        --arg manifest_digest "$manifest_digest" \
        --arg signature_digest "$signature_digest" \
        --argjson manifest_size "$manifest_size" \
        --argjson signature_size "$signature_size" '
          .tag_name == $tag
          and .target_commitish == $revision
          and .draft == true
          and .prerelease == false
          and (.body | type == "string" and contains($operation))
          and (.assets | type == "array")
          and (([.assets[].name] | length) == ([.assets[].name] | unique | length))
          and all(.assets[];
            (.state == "starter" or .state == "uploaded")
            and if .name == $manifest then
              (.state != "uploaded" or (.digest == $manifest_digest and .size == $manifest_size))
            elif .name == $signature then
              (.state != "uploaded" or (.digest == $signature_digest and .size == $signature_size))
            else false end
          )
        ' "$cleanup_release" >/dev/null 2>&1; then
        if GH_PROMPT_DISABLED=1 GH_HOST=github.com gh release delete "$release_tag" \
          --repo "$repository" --cleanup-tag --yes >/dev/null 2>&1; then
          printf 'ERROR: removed the incomplete runtime release and its newly created tag\n' >&2
        else
          printf 'ERROR: incomplete runtime release cleanup failed; resolve it manually before retrying\n' >&2
        fi
      else
        printf 'ERROR: refusing to remove an incomplete runtime release without the exact operation, target, and asset witness\n' >&2
      fi
    elif grep -F 'HTTP 404' "$cleanup_release_error" >/dev/null 2>&1; then
      cleanup_tag="${temporary}/cleanup-tag.json"
      cleanup_tag_error="${temporary}/cleanup-tag.error"
      if GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
        "repos/${repository}/git/ref/tags/${release_tag}" \
        >"$cleanup_tag" 2>"$cleanup_tag_error"; then
        printf 'ERROR: an incomplete runtime tag exists without an attributable draft release; resolve it manually before retrying\n' >&2
      elif ! grep -F 'HTTP 404' "$cleanup_tag_error" >/dev/null 2>&1; then
        printf 'ERROR: unable to determine whether incomplete runtime release state remains; resolve it manually before retrying\n' >&2
      fi
    else
      printf 'ERROR: unable to read back the incomplete runtime release; resolve it manually before retrying\n' >&2
    fi
  fi
  rm -rf -- "$temporary"
  return "$status"
}
trap cleanup EXIT

openssl pkey -pubin -in "$public_key" -text -noout 2>/dev/null | grep -Eq '^ED25519 Public-Key:' || \
  die 'runtime public key must be an Ed25519 public PEM'
public_der="${temporary}/runtime-public.der"
openssl pkey -pubin -in "$public_key" -outform DER > "$public_der"
trust_key_id="$(sha256_file "$public_der")"
signature_binary="${temporary}/runtime.sig.bin"
decode_base64 "$signature" "$signature_binary"
[[ "$(wc -c < "$signature_binary" | tr -d ' ')" == 64 ]] || die 'runtime Ed25519 signature has an invalid size'
openssl pkeyutl -verify -pubin -inkey "$public_key" -rawin \
  -in "$manifest" -sigfile "$signature_binary" >/dev/null || \
  die 'runtime manifest signature is invalid'
PYTHONPATH="${script_root}/tools" python3 "${script_root}/tools/opencodex_runtime_manifest.py" verify \
  --manifest "$manifest" \
  --artifact-version "$artifact_version" \
  --source-revision "$source_revision" \
  --trust-key-id "$trust_key_id" >/dev/null
manifest_digest="sha256:$(sha256_file "$manifest")"
signature_digest="sha256:$(sha256_file "$signature")"
manifest_size="$(wc -c < "$manifest" | tr -d ' ')"
signature_size="$(wc -c < "$signature" | tr -d ' ')"
[[ "$manifest_size" =~ ^[1-9][0-9]*$ && "$signature_size" =~ ^[1-9][0-9]*$ ]] || \
  die 'runtime release asset size is invalid'
operation_input="${temporary}/operation-input"
printf '%s\0%s\0%s\0%s\0%s\n' \
  "$repository" "$release_tag" "$source_revision" "$manifest_digest" "$signature_digest" \
  > "$operation_input"
release_operation="$(sha256_file "$operation_input")"
[[ "$release_operation" =~ ^[0-9a-f]{64}$ ]] || die 'unable to derive the bounded release operation witness'

visibility="$(GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api "repos/${repository}" --jq .visibility)" || \
  die 'GitHub authentication cannot read the runtime release repository'
[[ "$visibility" == public ]] || die 'runtime release repository must be public'
remote_source="$(GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
  "repos/${repository}/git/commits/${source_revision}" --jq .sha)" || \
  die 'unable to resolve the reviewed runtime source commit'
[[ "$remote_source" == "$source_revision" ]] || die 'runtime source commit readback differs from the reviewed revision'

verify_latest_isolation() {
  local error_file="$1"
  local latest_tag
  if latest_tag="$(GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
    "repos/${repository}/releases/latest" --jq .tag_name 2>"$error_file")"; then
    [[ "$latest_tag" != "$release_tag" ]] || die 'runtime release unexpectedly replaced releases/latest'
  elif ! grep -F 'HTTP 404' "$error_file" >/dev/null; then
    die 'unable to verify the releases/latest isolation policy'
  fi
}

verify_published_release_json() {
  local release_json="$1"
  jq -e \
    --arg tag "$release_tag" \
    --arg revision "$source_revision" \
    --arg operation "opencodex-runtime-release-operation:${release_operation}" \
    --arg manifest "$manifest_name" \
    --arg signature "$signature_name" \
    --arg manifest_digest "$manifest_digest" \
    --arg signature_digest "$signature_digest" \
    --argjson manifest_size "$manifest_size" \
    --argjson signature_size "$signature_size" '
      .tag_name == $tag
      and .target_commitish == $revision
      and .draft == false
      and .prerelease == false
      and .immutable == true
      and (.body | type == "string" and contains($operation))
      and (([.assets[].name] | sort) == ([$manifest, $signature] | sort))
      and all(.assets[];
        .state == "uploaded"
        and if .name == $manifest then
          (.digest == $manifest_digest and .size == $manifest_size)
        elif .name == $signature then
          (.digest == $signature_digest and .size == $signature_size)
        else false end
      )
    ' "$release_json" >/dev/null || \
    die 'existing immutable runtime release differs from the exact retry input'
}

verify_release_tag_revision() {
  local tag_revision
  tag_revision="$(GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
    "repos/${repository}/git/ref/tags/${release_tag}" --jq '.object | select(.type == "commit") | .sha')" || \
    die 'runtime release tag is not a lightweight commit tag'
  [[ "$tag_revision" == "$source_revision" ]] || die 'runtime release tag does not match the source revision'
}

existing_release="${temporary}/existing-release.json"
existing_release_error="${temporary}/existing-release.error"
if GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
  "repos/${repository}/releases/tags/${release_tag}" \
  >"$existing_release" 2>"$existing_release_error"; then
  verify_published_release_json "$existing_release"
  verify_release_tag_revision
  verify_latest_isolation "${temporary}/existing-latest.error"
  release_complete=true
  printf 'runtime_release=%s source=%s immutable=true latest=false assets=2 retry=verified\n' \
    "$release_tag" "$source_revision"
  exit 0
elif ! grep -F 'HTTP 404' "$existing_release_error" >/dev/null; then
  die 'unable to determine whether the runtime GitHub Release already exists'
fi

tag_error="${temporary}/tag.error"
if GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
  "repos/${repository}/git/ref/tags/${release_tag}" >/dev/null 2>"$tag_error"; then
  die 'runtime release tag already exists and will not be moved'
fi
grep -F 'HTTP 404' "$tag_error" >/dev/null || die 'unable to prove the runtime release tag is absent'

notes="${temporary}/release-notes.md"
cat > "$notes" <<EOF
# OpenCodex runtime ${artifact_version}

This immutable release publishes the signed runtime manifest for the exact OCI
index digest recorded in \`${manifest_name}\`. The image itself is pulled by
digest from \`ghcr.io/novelkr/opencodex-runtime\`; the GitHub Release contains
exactly the manifest and detached Ed25519 signature.

This runtime release intentionally uses \`make_latest=false\` so it does not
replace the OpenCodex Relay application release selected by \`releases/latest\`.

<!-- opencodex-runtime-release-operation:${release_operation} -->
EOF

release_attempted=true
GH_PROMPT_DISABLED=1 GH_HOST=github.com gh release create "$release_tag" \
  "$manifest" "$signature" \
  --repo "$repository" \
  --target "$source_revision" \
  --title "OpenCodex runtime ${artifact_version}" \
  --notes-file "$notes" \
  --draft \
  --latest=false >/dev/null

verify_release() {
  local expected_draft="$1"
  local release_json="${temporary}/release-${expected_draft}.json"
  GH_PROMPT_DISABLED=1 GH_HOST=github.com gh release view "$release_tag" \
    --repo "$repository" \
    --json tagName,targetCommitish,isDraft,isPrerelease,body,assets > "$release_json" || \
    die 'unable to read back the runtime GitHub Release'
  jq -e \
    --arg tag "$release_tag" \
    --arg revision "$source_revision" \
    --argjson draft "$expected_draft" \
    --arg operation "opencodex-runtime-release-operation:${release_operation}" \
    --arg manifest "$manifest_name" \
    --arg signature "$signature_name" \
    --arg manifest_digest "$manifest_digest" \
    --arg signature_digest "$signature_digest" \
    --argjson manifest_size "$manifest_size" \
    --argjson signature_size "$signature_size" '
      .tagName == $tag
      and .targetCommitish == $revision
      and .isDraft == $draft
      and .isPrerelease == false
      and (.body | type == "string" and contains($operation))
      and (([.assets[].name] | sort) == ([$manifest, $signature] | sort))
      and all(.assets[];
        .state == "uploaded"
        and if .name == $manifest then
          (.digest == $manifest_digest and .size == $manifest_size)
        elif .name == $signature then
          (.digest == $signature_digest and .size == $signature_size)
        else false end
      )
    ' "$release_json" >/dev/null || die 'runtime GitHub Release readback differs from the requested state'
}

verify_release true
GH_PROMPT_DISABLED=1 GH_HOST=github.com gh release edit "$release_tag" \
  --repo "$repository" --draft=false --latest=false >/dev/null
verify_release false

published="${temporary}/published.json"
GH_PROMPT_DISABLED=1 GH_HOST=github.com gh api \
  "repos/${repository}/releases/tags/${release_tag}" > "$published" || \
  die 'unable to read back the published runtime release'
verify_published_release_json "$published"
verify_release_tag_revision
verify_latest_isolation "${temporary}/latest.error"

release_complete=true
printf 'runtime_release=%s source=%s immutable=true latest=false assets=2\n' \
  "$release_tag" "$source_revision"

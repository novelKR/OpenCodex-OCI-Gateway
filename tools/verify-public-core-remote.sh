#!/usr/bin/env bash
set -euo pipefail

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  printf '%s\n' \
    'usage: verify-public-core-remote.sh --phase private-staging|public-ci' \
    '       --repo OWNER/REPO --repository-id ID --evidence FILE' \
    '       [--timeout-seconds 1800]' >&2
}

phase=""
repo=""
repository_id=""
evidence=""
timeout_seconds="1800"
timeout_set=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --phase)
      [[ -z "$phase" && $# -ge 2 ]] || die 'invalid or repeated --phase'
      phase="$2"
      shift 2
      ;;
    --repo)
      [[ -z "$repo" && $# -ge 2 ]] || die 'invalid or repeated --repo'
      repo="$2"
      shift 2
      ;;
    --repository-id)
      [[ -z "$repository_id" && $# -ge 2 ]] || die 'invalid or repeated --repository-id'
      repository_id="$2"
      shift 2
      ;;
    --evidence)
      [[ -z "$evidence" && $# -ge 2 ]] || die 'invalid or repeated --evidence'
      evidence="$2"
      shift 2
      ;;
    --timeout-seconds)
      [[ "$timeout_set" == false && $# -ge 2 ]] || die 'invalid or repeated --timeout-seconds'
      timeout_seconds="$2"
      timeout_set=true
      shift 2
      ;;
    *)
      usage
      die "unknown argument: $1"
      ;;
  esac
done

case "$phase" in
  private-staging|public-ci) ;;
  *) die '--phase must be private-staging or public-ci' ;;
esac
[[ "$repo" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || \
  die '--repo must be OWNER/REPO'
[[ "$repository_id" =~ ^[A-Za-z0-9_=-]+$ ]] || die '--repository-id is invalid'
[[ "$evidence" = /* && -f "$evidence" && ! -L "$evidence" ]] || \
  die '--evidence must be an absolute regular file'
[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || die '--timeout-seconds must be a positive integer'

for command in cmp gh git python3 sort tar; do
  command -v "$command" >/dev/null || die "required command is unavailable: ${command}"
done
export GH_PROMPT_DISABLED=1
export GH_HOST=github.com
readonly WORKFLOW_RUN_LIMIT=100

evidence_tuple="$(python3 - "$evidence" <<'PY'
import json
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
try:
    document = json.loads(path.read_text(encoding="utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
    raise SystemExit(f"invalid publication evidence: {error}")
expected = {
    "schema_version",
    "source_commit",
    "source_tree",
    "allowlist_blob",
    "public_commit",
    "public_tree",
    "tag",
    "exported_files",
}
if type(document) is not dict or set(document) != expected:
    raise SystemExit("publication evidence has unknown or missing fields")
if document["schema_version"] != 1:
    raise SystemExit("unsupported publication evidence schema")
for key in ("source_commit", "source_tree", "allowlist_blob", "public_commit", "public_tree"):
    if type(document[key]) is not str or not re.fullmatch(r"[0-9a-f]{40}", document[key]):
        raise SystemExit(f"publication evidence {key} is invalid")
if type(document["tag"]) is not str or not re.fullmatch(
    r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)",
    document["tag"],
):
    raise SystemExit("publication evidence tag is invalid")
if type(document["exported_files"]) is not int or document["exported_files"] <= 0:
    raise SystemExit("publication evidence exported_files is invalid")
print(
    document["public_commit"],
    document["public_tree"],
    document["tag"],
    document["exported_files"],
    sep="\t",
)
PY
)" || die 'publication evidence validation failed'
IFS=$'\t' read -r public_commit public_tree tag exported_files extra <<<"$evidence_tuple"
[[ -z "${extra:-}" ]] || die 'publication evidence parser returned extra fields'

metadata="$(gh repo view "$repo" \
  --json id,nameWithOwner,isPrivate,isFork,isMirror,defaultBranchRef \
  --jq '[.id,.nameWithOwner,(.isPrivate|tostring),(.isFork|tostring),(.isMirror|tostring),(.defaultBranchRef.name // "")] | @tsv'
)" || die 'unable to read repository metadata'
IFS=$'\t' read -r actual_id actual_repo actual_private actual_fork actual_mirror default_branch metadata_extra <<<"$metadata"
[[ -z "${metadata_extra:-}" ]] || die 'repository metadata parser returned extra fields'
[[ "$actual_id" == "$repository_id" ]] || die 'repository ID does not match the approved staging repository'
[[ "$actual_repo" == "$repo" ]] || die 'repository name does not match'
[[ "$actual_fork" == "false" && "$actual_mirror" == "false" ]] || \
  die 'public Core repository must not be a fork or mirror'
[[ "$default_branch" == "main" ]] || die 'repository default branch is not main'
if [[ "$phase" == "private-staging" ]]; then
  [[ "$actual_private" == "true" ]] || die 'private-staging phase requires a PRIVATE repository'
else
  [[ "$actual_private" == "false" ]] || die 'public-ci phase requires a PUBLIC repository'
fi

temporary="$(mktemp -d "${TMPDIR:-/tmp}/opencodex-public-remote.XXXXXX")"
cleanup() {
  rm -rf -- "$temporary"
}
trap cleanup EXIT
mirror="$temporary/repository.git"
archive="$temporary/archive"
mkdir "$archive"

gh repo clone "$repo" "$mirror" -- --mirror --quiet || die 'unable to clone the approved repository'
git -C "$mirror" for-each-ref --format='%(refname)' refs/heads refs/tags |
  LC_ALL=C sort > "$temporary/actual-refs"
printf 'refs/heads/main\nrefs/tags/%s\n' "$tag" |
  LC_ALL=C sort > "$temporary/expected-refs"
cmp -s "$temporary/expected-refs" "$temporary/actual-refs" || \
  die 'remote branch or tag set is incomplete or unexpected'

main_commit="$(git -C "$mirror" rev-parse 'refs/heads/main^{commit}')"
tag_object="$(git -C "$mirror" rev-parse "refs/tags/${tag}")"
[[ "$(git -C "$mirror" cat-file -t "refs/tags/${tag}")" == "commit" ]] || \
  die 'remote publication tag is not lightweight'
[[ "$main_commit" == "$public_commit" && "$tag_object" == "$public_commit" ]] || \
  die 'remote main or tag does not match publication evidence'
[[ "$(git -C "$mirror" rev-list --count --all)" == "1" ]] || \
  die 'remote history does not contain exactly one commit'
[[ "$(git -C "$mirror" rev-list --parents --all | awk '{print NF}')" == "1" ]] || \
  die 'remote initial commit has a parent'
[[ "$(git -C "$mirror" rev-parse "${public_commit}^{tree}")" == "$public_tree" ]] || \
  die 'remote tree does not match publication evidence'

git -C "$mirror" archive --format=tar "$public_commit" | tar -xf - -C "$archive"
actual_files="$(find "$archive" -type f | wc -l | tr -d ' ')"
[[ "$actual_files" == "$exported_files" ]] || \
  die 'remote archive file count does not match publication evidence'
"$archive/tools/public-hygiene.sh" "$archive"

workflow_runs() {
  local workflow_name="$1"
  local event_filter="${2:-}"
  local runs_json
  if [[ -n "$event_filter" ]]; then
    runs_json="$(gh run list -R "$repo" --commit "$public_commit" \
      --workflow "$workflow_name" --event "$event_filter" --all \
      --limit "$WORKFLOW_RUN_LIMIT" \
      --json databaseId,status,conclusion,workflowName,event)" || return 1
  else
    runs_json="$(gh run list -R "$repo" --commit "$public_commit" \
      --workflow "$workflow_name" --all --limit "$WORKFLOW_RUN_LIMIT" \
      --json databaseId,status,conclusion,workflowName,event)" || return 1
  fi
  python3 -c '
import json, sys
runs = json.load(sys.stdin)
limit = int(sys.argv[1])
if type(runs) is not list:
    raise SystemExit("workflow run response is not an array")
if len(runs) >= limit:
    raise SystemExit("workflow run evidence reached the safety limit")
for run in runs:
    if type(run) is not dict or type(run.get("databaseId")) is not int:
        raise SystemExit("workflow run response contains an invalid run")
    if type(run.get("status")) is not str:
        raise SystemExit("workflow run response contains an invalid status")
' "$WORKFLOW_RUN_LIMIT" <<<"$runs_json" || return 1
  printf '%s\n' "$runs_json"
}

verify_private_workflow_skipped() {
  workflow_name="$1"
  runs_json="$(workflow_runs "$workflow_name")" || \
    die "unable to read complete ${workflow_name} workflow run evidence"
  run_ids="$(python3 -c '
import json, sys
runs = json.load(sys.stdin)
if type(runs) is not list:
    raise SystemExit("workflow run response is not an array")
for run in runs:
    if run.get("status") != "completed":
        raise SystemExit("PRIVATE workflow run is not completed")
    print(run.get("databaseId", ""))
' <<<"$runs_json")" || die "PRIVATE ${workflow_name} run is active or invalid"
  while IFS= read -r run_id; do
    [[ -n "$run_id" ]] || continue
    jobs_json="$(gh run view "$run_id" -R "$repo" --json jobs)" || \
      die "unable to read ${workflow_name} jobs"
    python3 -c '
import json, sys
document = json.load(sys.stdin)
jobs = document.get("jobs")
if type(jobs) is not list or not jobs:
    raise SystemExit("PRIVATE workflow run has no readable jobs")
for job in jobs:
    if job.get("status") != "completed" or job.get("conclusion") != "skipped":
        raise SystemExit("PRIVATE workflow contains a non-skipped job")
' <<<"$jobs_json" || die "PRIVATE ${workflow_name} workflow dispatched a runner job"
  done <<<"$run_ids"
}

public_workflow_state() {
  local workflow_name="$1"
  local runs_json
  runs_json="$(workflow_runs "$workflow_name" workflow_dispatch)" || return 1
  python3 -c '
import json, sys
runs = json.load(sys.stdin)
if any(run.get("status") == "completed" and run.get("conclusion") == "success" for run in runs):
    print("success")
elif any(run.get("status") == "completed" for run in runs):
    print("failure")
else:
    print("pending")
' <<<"$runs_json"
}

if [[ "$phase" == "private-staging" ]]; then
  verify_private_workflow_skipped "CI"
  verify_private_workflow_skipped "CodeQL"
  verify_private_workflow_skipped "Publish experimental OpenCodex image"
  printf 'public_remote=ok phase=private-staging repo=%s commit=%s actions=skipped\n' \
    "$repo" "$public_commit"
  exit 0
fi

deadline=$((SECONDS + timeout_seconds))
while true; do
  ci_state="$(public_workflow_state "CI")" || \
    die 'unable to read complete CI workflow_dispatch evidence'
  codeql_state="$(public_workflow_state "CodeQL")" || \
    die 'unable to read complete CodeQL workflow_dispatch evidence'
  [[ "$ci_state" != "failure" ]] || die 'public CI completed without success'
  [[ "$codeql_state" != "failure" ]] || die 'public CodeQL completed without success'
  if [[ "$ci_state" == "success" && "$codeql_state" == "success" ]]; then
    break
  fi
  (( SECONDS < deadline )) || die 'timed out waiting for successful public CI and CodeQL'
  sleep 15
done
printf 'public_remote=ok phase=public-ci repo=%s commit=%s ci=success codeql=success\n' \
  "$repo" "$public_commit"

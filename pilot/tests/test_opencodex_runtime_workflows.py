#!/usr/bin/env python3

import base64
import hashlib
import os
import pathlib
import re
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class RuntimeWorkflowContractTests(unittest.TestCase):
    def text(self, relative: str) -> str:
        return (ROOT / relative).read_text(encoding="utf-8")

    def test_upstream_watch_separates_read_only_detection_from_app_writer(self):
        workflow = self.text(".github/workflows/opencodex-upstream-watch.yml")
        self.assertIn('cron: "23 */6 * * *"', workflow)
        self.assertIn("workflow_dispatch:", workflow)
        self.assertIn("permissions: {}", workflow)
        detect = workflow.split("  detect:\n", 1)[1].split("\n  propose:\n", 1)[0]
        propose = workflow.split("\n  propose:\n", 1)[1]
        self.assertIn("contents: read", detect)
        self.assertIn("timeout-minutes: 15", detect)
        self.assertNotIn("contents: write", detect)
        self.assertNotIn("pull-requests: write", detect)
        self.assertIn("environment: upstream-watch", propose)
        self.assertIn("timeout-minutes: 30", propose)
        self.assertIn("candidate_sha256: ${{ steps.detect.outputs.candidate_sha256 }}", detect)
        self.assertIn("downloaded candidate bytes differ from the detector witness", propose)
        self.assertIn("actions/create-github-app-token@", propose)
        self.assertIn("permission-contents: write", propose)
        self.assertIn("permission-pull-requests: write", propose)
        self.assertNotIn("permission-actions:", propose)
        self.assertNotIn("permission-workflows:", propose)
        self.assertNotIn("permission-packages:", propose)
        self.assertIn('branch="automation/opencodex-${VERSION}-r1"', propose)
        self.assertIn("existing automation branch is divergent", propose)
        self.assertIn('[[ "$pr_count" == 0 || "$pr_count" == 1 ]]', propose)
        self.assertIn("identical automation branch has multiple open pull requests", propose)
        self.assertIn('if [[ "$pr_count" == 0 ]]; then', propose)
        self.assertEqual(propose.count("gh pr create"), 2)
        self.assertIn(".autoMergeRequest == null", propose)
        self.assertIn(".isCrossRepository == false", propose)
        self.assertIn("gh pr create", propose)
        self.assertNotIn("gh pr merge", propose)
        self.assertNotIn("--auto", propose)
        self.assertNotIn("--force", propose)
        self.assertNotRegex(propose, r"git\s+push[^\n]*(?:refs/heads/)?main")
        self.assertIn("Bun lock regeneration is not deterministic", propose)
        self.assertIn('["git", "ls-files", "--others", "--exclude-standard"]', propose)
        self.assertEqual(propose.count("BUN_INSTALL_CACHE_DIR=/tmp/bun-cache"), 2)
        for path in (
            "containers/opencodex/UPSTREAM_NOTICES.md",
            "containers/opencodex/bun.lock",
            "containers/opencodex/package.json",
            "containers/opencodex/upstream.lock.json",
        ):
            self.assertIn(path, propose)

    def test_pr_runtime_image_check_is_always_present_and_never_pushes(self):
        workflow = self.text(".github/workflows/ci.yml")
        runtime = workflow.split("\n  runtime-image:\n", 1)[1]
        self.assertIn("name: runtime-image", runtime)
        self.assertIn("runtime-image: not applicable", runtime)
        self.assertIn('".github/workflows/ci.yml"', runtime)
        self.assertIn("--target runtime", runtime)
        self.assertIn('for architecture in amd64 arm64; do', runtime)
        self.assertIn("opencodex_runtime_image_test.py", runtime)
        self.assertNotIn("docker/login-action", runtime)
        self.assertNotIn("push: true", runtime)
        self.assertNotRegex(runtime, r"docker\s+(?:image\s+)?push")
        self.assertNotIn("DOCKER_CONFIG: ${{ runner.temp }}", workflow)
        self.assertIn(
            'export DOCKER_CONFIG="${RUNNER_TEMP}/opencodex-runtime-docker-config"',
            runtime,
        )
        self.assertIn("$GITHUB_ENV", runtime)
        self.assertIn(
            "BUILDX_DOWNLOAD_URL: https://github.com/docker/buildx/releases/"
            "download/v0.30.1/buildx-v0.30.1.linux-amd64",
            runtime,
        )
        self.assertIn(
            "BUILDX_SHA256: "
            "c37114fcd034025ec68e224657c8a5a850df472ded3ddcbca75ad3a7ebb9710d",
            runtime,
        )
        self.assertIn(
            "BUILDX_VERSION_OUTPUT: github.com/docker/buildx v0.30.1 "
            "9e66234aa13328a5e75b75aa5574e1ca6d6d9c01",
            runtime,
        )
        self.assertIn(
            "BINFMT_IMAGE: docker.io/tonistiigi/binfmt:qemu-v10.2.3@sha256:"
            "400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0",
            runtime,
        )
        self.assertIn(
            "BUILDKIT_IMAGE: docker.io/moby/buildkit:v0.26.1@sha256:"
            "8290a3b1183f45fb0c7ccd2faa7aa88eeb9af0ea85ff54458cbd8cbdb576e721",
            runtime,
        )
        self.assertIn("Install the reviewed Buildx CLI before first execution", runtime)
        self.assertIn("$BUILDX_DOWNLOAD_URL", runtime)
        self.assertIn('sha256sum "$download"', runtime)
        self.assertIn('install -m 0555 "$download" "$plugin"', runtime)
        self.assertLess(
            runtime.index('sha256sum "$download"'),
            runtime.index('install -m 0555 "$download" "$plugin"'),
        )
        self.assertLess(
            runtime.index('install -m 0555 "$download" "$plugin"'),
            runtime.index("docker buildx version"),
        )
        self.assertIn("cache-image: false", runtime)
        self.assertIn("image: ${{ env.BINFMT_IMAGE }}", runtime)
        self.assertIn("platforms: arm64", runtime)
        self.assertIn("cache-binary: false", runtime)
        self.assertIn("image=${{ env.BUILDKIT_IMAGE }}", runtime)
        self.assertIn("BUILDX_NODES: ${{ steps.buildx.outputs.nodes }}", runtime)
        self.assertIn("configured_image=", runtime)
        self.assertIn("container_image_id=", runtime)
        self.assertIn("expected_image_id=", runtime)
        self.assertNotIn("tonistiigi/binfmt:latest", runtime)
        self.assertNotIn("moby/buildkit:buildx-stable-1", runtime)

    def test_hosted_candidate_pipeline_is_digest_bound_and_never_promotes(self):
        workflow = self.text(".github/workflows/opencodex-runtime.yml")
        candidate = workflow.split("  candidate:\n", 1)[1].split(
            "\n  linux_arm64_canary:\n", 1
        )[0]
        linux = workflow.split("\n  linux_arm64_canary:\n", 1)[1].split(
            "\n  macos_contract:\n", 1
        )[0]
        macos = workflow.split("\n  macos_contract:\n", 1)[1].split(
            "\n  hosted_qualification:\n", 1
        )[0]
        qualification = workflow.split("\n  hosted_qualification:\n", 1)[1].split(
            "\n  verify_public_candidate:\n", 1
        )[0]
        public = workflow.split("\n  verify_public_candidate:\n", 1)[1]

        self.assertIn("branches: [main]", workflow)
        self.assertIn("workflow_dispatch:", workflow)
        dispatch = workflow.split("  workflow_dispatch:\n", 1)[1].split(
            "\npermissions:", 1
        )[0]
        self.assertEqual(dispatch.count("candidate_run_id:"), 1)
        self.assertNotIn("digest:", dispatch)
        self.assertNotIn("tag:", dispatch)
        self.assertNotIn("pull_request:", workflow)
        self.assertNotIn("DOCKER_CONFIG: ${{ runner.temp }}", workflow)
        self.assertEqual(
            re.findall(r"^    runs-on: (.+)$", workflow, flags=re.MULTILINE),
            [
                "ubuntu-24.04",
                "ubuntu-24.04-arm",
                "macos-26",
                "ubuntu-24.04",
                "ubuntu-24.04-arm",
            ],
        )
        self.assertGreaterEqual(
            workflow.count('[[ "$RUNNER_ENVIRONMENT" == github-hosted ]]'), 5
        )

        self.assertIn("environment: runtime-candidate", candidate)
        self.assertIn("runs-on: ubuntu-24.04", candidate)
        self.assertIn("packages: write", candidate)
        self.assertIn("id-token: write", candidate)
        self.assertIn("attestations: write", candidate)
        self.assertIn('candidate_tag="candidate-${artifact_version}-${GITHUB_SHA}"', candidate)
        self.assertIn("target: runtime", candidate)
        self.assertIn("platforms: linux/amd64,linux/arm64", candidate)
        self.assertIn("sbom: true", candidate)
        self.assertIn("provenance: mode=max", candidate)
        self.assertEqual(workflow.count("docker/build-push-action@"), 1)
        self.assertIn("subject-digest: ${{ steps.build.outputs.digest }}", candidate)
        self.assertIn("create-candidate", candidate)
        for platform in ("linux/amd64", "linux/arm64"):
            self.assertIn(f'(index .SBOM "{platform}").SPDX', candidate)
            self.assertIn(f'(index .Provenance "{platform}").SLSA', candidate)

        self.assertIn("runs-on: ubuntu-24.04-arm", linux)
        self.assertIn('exact="${RUNTIME_IMAGE}@${index_digest}"', linux)
        self.assertIn('docker pull --platform linux/arm64 "$exact"', linux)
        self.assertIn("opencodex_runtime_image_test.py", linux)
        self.assertIn("create-linux", linux)
        self.assertIn("verify-linux", linux)
        self.assertIn('--runner-environment "$RUNNER_ENVIRONMENT"', linux)
        self.assertIn("arm64_digest", linux)
        self.assertNotIn("docker/build-push-action", linux)
        self.assertNotRegex(linux, r"\bdocker\s+(?:image\s+)?push\b")
        self.assertNotRegex(linux, r"\bdocker\s+build\b")

        self.assertIn("runs-on: macos-26", macos)
        self.assertIn("go test ./...", macos)
        self.assertIn("go test -race ./...", macos)
        self.assertIn("go vet ./...", macos)
        self.assertIn("swift test", macos)
        self.assertIn("swift build -c release -Xswiftc -warnings-as-errors", macos)
        self.assertIn("test-keychain-acl-integration.sh", macos)
        self.assertIn("test_opencodex_runtime_apple_canary", macos)
        self.assertIn("test_opencodex_runtime_lifecycle_canary", macos)
        self.assertIn("create-macos", macos)
        self.assertIn('--runner-environment "$RUNNER_ENVIRONMENT"', macos)
        self.assertIn("apple_container_live=false", macos)
        self.assertNotIn("/usr/local/bin/container", macos)
        self.assertNotRegex(macos, r"\bcontainer\s+(?:run|system|start|stop)\b")

        self.assertIn("needs: [candidate, linux_arm64_canary, macos_contract]", qualification)
        self.assertIn("runs-on: ubuntu-24.04", qualification)
        self.assertIn("packages: read", qualification)
        self.assertIn("docker/login-action", qualification)
        self.assertIn("gh attestation verify", qualification)
        self.assertIn("--deny-self-hosted-runners", qualification)
        self.assertIn("create-hosted", qualification)
        self.assertIn("verify-hosted", qualification)
        self.assertIn("qualification_level: `hosted-candidate`", qualification)
        self.assertIn("apple_container_live: `false`", qualification)
        self.assertIn("stable_promotion_eligible: `false`", qualification)

        self.assertIn("runs-on: ubuntu-24.04-arm", public)
        self.assertIn("actions: read", public)
        self.assertIn("actions/runs/${CANDIDATE_RUN_ID}", public)
        self.assertIn("run.get(\"event\") != \"push\"", public)
        self.assertIn("run.get(\"head_branch\") != \"main\"", public)
        self.assertIn("run.get(\"conclusion\") != \"success\"", public)
        self.assertIn("steps.candidate_run.outputs.source_revision", public)
        self.assertIn("steps.candidate_run.outputs.run_attempt", public)
        self.assertIn("steps.candidate_run.outputs.run_number", public)
        self.assertIn("--release-sequence '${{ steps.candidate_run.outputs.run_number }}'", public)
        self.assertIn("opencodex_upstream.py verify-tree", public)
        self.assertIn("upstream_lock_sha256", public)
        self.assertIn("github-token: ${{ github.token }}", public)
        self.assertIn("run-id: ${{ steps.candidate_run.outputs.run_id }}", public)
        self.assertIn("Prove anonymous exact-digest pull without registry credentials", public)
        self.assertIn("printf '{\"auths\":{}}", public)
        self.assertIn('docker pull --platform linux/arm64 "$exact"', public)
        self.assertIn("create-public", public)
        self.assertIn('--runner-environment "$RUNNER_ENVIRONMENT"', public)
        self.assertIn("verify-public", public)
        self.assertIn("public_ready: `true`", public)
        self.assertIn("stable_promotion_eligible: `false`", public)
        self.assertNotIn("docker/login-action", public)
        self.assertNotIn("GH_TOKEN:", public.split("Prove anonymous exact-digest pull", 1)[1])

        for forbidden in (
            "  apple_canary:",
            "  lifecycle_canary:",
            "  promote:",
            "runs-on: [self-hosted",
            "OPENCODEX_RUNTIME_CANARY_ENABLED",
            "OPENCODEX_RUNTIME_RELEASE_ENABLED",
            "environment: runtime-release",
            "OPENCODEX_RUNTIME_RELEASE_SIGNING_KEY_B64_V1",
            "publish-opencodex-runtime-release.sh",
            "docker buildx imagetools create --tag",
        ):
            self.assertNotIn(forbidden, workflow)
        self.assertNotIn(":latest", workflow)

    def test_runtime_builder_tools_are_immutable_before_privileged_use(self):
        workflow = self.text(".github/workflows/opencodex-runtime.yml")
        candidate = workflow.split("  candidate:\n", 1)[1].split(
            "\n  linux_arm64_canary:\n", 1
        )[0]

        buildx_url = (
            "https://github.com/docker/buildx/releases/download/v0.30.1/"
            "buildx-v0.30.1.linux-amd64"
        )
        buildx_sha = (
            "c37114fcd034025ec68e224657c8a5a850df472ded3ddcbca75ad3a7ebb9710d"
        )
        buildx_commit = "9e66234aa13328a5e75b75aa5574e1ca6d6d9c01"
        binfmt_digest = (
            "sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
        )
        buildkit_digest = (
            "sha256:8290a3b1183f45fb0c7ccd2faa7aa88eeb9af0ea85ff54458cbd8cbdb576e721"
        )

        self.assertIn(f"BUILDX_DOWNLOAD_URL: {buildx_url}", workflow)
        self.assertIn(f"BUILDX_SHA256: {buildx_sha}", workflow)
        self.assertIn(
            f"BUILDX_VERSION_OUTPUT: github.com/docker/buildx v0.30.1 {buildx_commit}",
            workflow,
        )
        self.assertIn(
            f"BINFMT_IMAGE: docker.io/tonistiigi/binfmt:qemu-v10.2.3@{binfmt_digest}",
            workflow,
        )
        self.assertIn(
            f"BUILDKIT_IMAGE: docker.io/moby/buildkit:v0.26.1@{buildkit_digest}",
            workflow,
        )
        self.assertEqual(
            workflow.count("Install the reviewed Buildx CLI before first execution"),
            1,
        )
        self.assertEqual(workflow.count("docker/setup-buildx-action@"), 1)

        candidate_installer = candidate.split(
            "Install the reviewed Buildx CLI before first execution", 1
        )[1].split("- uses: docker/setup-qemu-action@", 1)[0]
        self.assertIn("curl --fail --location --proto '=https' --tlsv1.2", candidate_installer)
        self.assertIn('"$BUILDX_DOWNLOAD_URL"', candidate_installer)
        self.assertIn('sha256sum "$download"', candidate_installer)
        self.assertIn('install -m 0555 "$download" "$plugin"', candidate_installer)
        self.assertIn('docker buildx version', candidate_installer)
        self.assertLess(
            candidate_installer.index('sha256sum "$download"'),
            candidate_installer.index('install -m 0555 "$download" "$plugin"'),
        )
        self.assertLess(
            candidate_installer.index('install -m 0555 "$download" "$plugin"'),
            candidate_installer.index('docker buildx version'),
        )

        qemu = candidate.split("- uses: docker/setup-qemu-action@", 1)[1].split(
            "- id: buildx", 1
        )[0]
        self.assertIn("cache-image: false", qemu)
        self.assertIn("image: ${{ env.BINFMT_IMAGE }}", qemu)
        self.assertIn("platforms: arm64", qemu)
        self.assertNotIn("platforms: all", qemu)

        buildx = candidate.split("- id: buildx", 1)[1].split(
            "- name: Verify the reviewed Buildx binary and BuildKit builder image", 1
        )[0]
        self.assertIn("docker/setup-buildx-action@", buildx)
        self.assertIn("cache-binary: false", buildx)
        self.assertIn("image=${{ env.BUILDKIT_IMAGE }}", buildx)
        self.assertNotIn("version:", buildx)
        self.assertIn("BUILDX_NODES: ${{ steps.buildx.outputs.nodes }}", candidate)
        self.assertIn("configured_image=", candidate)
        self.assertIn(buildkit_digest, candidate)
        self.assertIn("container_image_id=", candidate)
        self.assertIn("expected_image_id=", candidate)
        self.assertNotIn("tonistiigi/binfmt:latest", workflow)
        self.assertNotIn("moby/buildkit:buildx-stable-1", workflow)

    def test_runtime_release_publisher_is_immutable_two_asset_and_latest_false(self):
        publisher = self.text("tools/publish-opencodex-runtime-release.sh")
        self.assertIn("release input is not the exact two-asset set", publisher)
        self.assertIn('find "$input_dir" -mindepth 1 -maxdepth 1 -exec basename', publisher)
        self.assertGreaterEqual(publisher.count("--latest=false"), 2)
        self.assertIn(".immutable == true", publisher)
        self.assertIn("unexpectedly replaced releases/latest", publisher)
        self.assertIn("already exists and will not be moved", publisher)
        self.assertIn("existing immutable runtime release differs from the exact retry input", publisher)
        self.assertIn("retry=verified", publisher)
        self.assertIn('git/commits/${source_revision}', publisher)
        self.assertNotIn("commits/main", publisher)
        self.assertIn("release_attempted=true", publisher)
        self.assertIn("opencodex-runtime-release-operation:", publisher)
        self.assertIn("exact operation, target, and asset witness", publisher)
        self.assertIn("an incomplete runtime tag exists without an attributable draft release", publisher)
        draft_verifier = publisher.split("verify_release() {", 1)[1].split(
            "verify_release true", 1
        )[0]
        for field in (
            'manifest_digest "$manifest_digest"',
            'signature_digest "$signature_digest"',
            'manifest_size "$manifest_size"',
            'signature_size "$signature_size"',
            '.state == "uploaded"',
            '.digest == $manifest_digest and .size == $manifest_size',
            '.digest == $signature_digest and .size == $signature_size',
        ):
            self.assertIn(field, draft_verifier)
        self.assertLess(
            publisher.index("verify_release true"),
            publisher.index('gh release edit "$release_tag"'),
        )
        self.assertLess(
            publisher.index("release_attempted=true"),
            publisher.index('gh release create "$release_tag"'),
        )
        self.assertNotIn("--clobber", publisher)

    def test_runtime_release_publisher_rejects_corrupt_draft_assets_before_edit(self):
        from pilot.tests.test_opencodex_runtime_manifest import (
            manifest_document,
            runtime,
        )

        publisher = ROOT / "tools" / "publish-opencodex-runtime-release.sh"
        artifact_version = "2.40.0-r1"
        source_revision = "1" * 40
        release_tag = f"opencodex-runtime-{artifact_version}"

        with tempfile.TemporaryDirectory() as temporary_name:
            temporary = pathlib.Path(temporary_name)
            assets = temporary / "assets"
            assets.mkdir()
            private_key = temporary / "runtime-private.pem"
            public_key = temporary / "runtime-public.pem"
            public_der = temporary / "runtime-public.der"
            signature_binary = temporary / "runtime.sig.bin"
            manifest = assets / f"{release_tag}.json"
            signature = assets / f"{release_tag}.sig"
            subprocess.run(
                [
                    "openssl",
                    "genpkey",
                    "-algorithm",
                    "ED25519",
                    "-out",
                    str(private_key),
                ],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                [
                    "openssl",
                    "pkey",
                    "-in",
                    str(private_key),
                    "-pubout",
                    "-out",
                    str(public_key),
                ],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                [
                    "openssl",
                    "pkey",
                    "-pubin",
                    "-in",
                    str(public_key),
                    "-outform",
                    "DER",
                    "-out",
                    str(public_der),
                ],
                check=True,
                capture_output=True,
            )
            document = manifest_document()
            document["trust_key_id"] = hashlib.sha256(
                public_der.read_bytes()
            ).hexdigest()
            manifest.write_bytes(runtime.canonical_manifest(document))
            subprocess.run(
                [
                    "openssl",
                    "pkeyutl",
                    "-sign",
                    "-inkey",
                    str(private_key),
                    "-rawin",
                    "-in",
                    str(manifest),
                    "-out",
                    str(signature_binary),
                ],
                check=True,
                capture_output=True,
            )
            signature.write_text(
                base64.b64encode(signature_binary.read_bytes()).decode("ascii") + "\n",
                encoding="utf-8",
            )

            fake_bin = temporary / "bin"
            fake_bin.mkdir()
            fake_gh = fake_bin / "gh"
            fake_gh.write_text(
                """#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >> "$FAKE_GH_LOG"

asset_json() {
  local naming="$1"
  local manifest_digest="sha256:$(shasum -a 256 "$FAKE_MANIFEST" | awk '{print $1}')"
  local signature_digest="sha256:$(shasum -a 256 "$FAKE_SIGNATURE" | awk '{print $1}')"
  local manifest_size signature_size draft immutable
  manifest_size="$(wc -c < "$FAKE_MANIFEST" | tr -d ' ')"
  signature_size="$(wc -c < "$FAKE_SIGNATURE" | tr -d ' ')"
  draft="$(cat "$FAKE_GH_STATE/draft")"
  immutable=false
  [[ "$draft" == true ]] || immutable=true
  if [[ "$FAKE_DRAFT_CORRUPTION" == manifest-digest ]]; then
    manifest_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  elif [[ "$FAKE_DRAFT_CORRUPTION" == manifest-size ]]; then
    manifest_size="$((manifest_size + 1))"
  elif [[ "$FAKE_DRAFT_CORRUPTION" == signature-digest ]]; then
    signature_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  elif [[ "$FAKE_DRAFT_CORRUPTION" == signature-size ]]; then
    signature_size="$((signature_size + 1))"
  fi
  if [[ "$naming" == camel ]]; then
    jq -n \
      --arg tag "$FAKE_RELEASE_TAG" \
      --arg revision "$FAKE_SOURCE_REVISION" \
      --arg body "$(cat "$FAKE_GH_STATE/notes")" \
      --arg manifest "$(basename "$FAKE_MANIFEST")" \
      --arg signature "$(basename "$FAKE_SIGNATURE")" \
      --arg manifest_digest "$manifest_digest" \
      --arg signature_digest "$signature_digest" \
      --argjson manifest_size "$manifest_size" \
      --argjson signature_size "$signature_size" \
      --argjson draft "$draft" \
      '{tagName:$tag,targetCommitish:$revision,isDraft:$draft,isPrerelease:false,body:$body,assets:[{name:$manifest,state:"uploaded",digest:$manifest_digest,size:$manifest_size},{name:$signature,state:"uploaded",digest:$signature_digest,size:$signature_size}]}'
  else
    jq -n \
      --arg tag "$FAKE_RELEASE_TAG" \
      --arg revision "$FAKE_SOURCE_REVISION" \
      --arg body "$(cat "$FAKE_GH_STATE/notes")" \
      --arg manifest "$(basename "$FAKE_MANIFEST")" \
      --arg signature "$(basename "$FAKE_SIGNATURE")" \
      --arg manifest_digest "$manifest_digest" \
      --arg signature_digest "$signature_digest" \
      --argjson manifest_size "$manifest_size" \
      --argjson signature_size "$signature_size" \
      --argjson draft "$draft" \
      --argjson immutable "$immutable" \
      '{tag_name:$tag,target_commitish:$revision,draft:$draft,prerelease:false,immutable:$immutable,body:$body,assets:[{name:$manifest,state:"uploaded",digest:$manifest_digest,size:$manifest_size},{name:$signature,state:"uploaded",digest:$signature_digest,size:$signature_size}]}'
  fi
}

if [[ ${1:-} == api ]]; then
  endpoint="${2:-}"
  if [[ "$endpoint" == "repos/${FAKE_REPOSITORY}" ]]; then
    printf 'public\\n'
    exit 0
  fi
  if [[ "$endpoint" == "repos/${FAKE_REPOSITORY}/git/commits/${FAKE_SOURCE_REVISION}" ]]; then
    printf '%s\\n' "$FAKE_SOURCE_REVISION"
    exit 0
  fi
  if [[ "$endpoint" == "repos/${FAKE_REPOSITORY}/releases/tags/${FAKE_RELEASE_TAG}" ]]; then
    if [[ -f "$FAKE_GH_STATE/created" ]]; then
      asset_json snake
      exit 0
    fi
    printf 'HTTP 404\\n' >&2
    exit 1
  fi
  if [[ "$endpoint" == "repos/${FAKE_REPOSITORY}/git/ref/tags/${FAKE_RELEASE_TAG}" ]]; then
    if [[ -f "$FAKE_GH_STATE/created" ]]; then
      printf '%s\\n' "$FAKE_SOURCE_REVISION"
      exit 0
    fi
    printf 'HTTP 404\\n' >&2
    exit 1
  fi
  if [[ "$endpoint" == "repos/${FAKE_REPOSITORY}/releases/latest" ]]; then
    printf '0.3.9\\n'
    exit 0
  fi
fi

if [[ ${1:-} == release && ${2:-} == create ]]; then
  notes_file=""
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == --notes-file ]]; then
      notes_file="$2"
      shift 2
    else
      shift
    fi
  done
  [[ -n "$notes_file" ]]
  cp "$notes_file" "$FAKE_GH_STATE/notes"
  : > "$FAKE_GH_STATE/created"
  printf 'true\\n' > "$FAKE_GH_STATE/draft"
  exit 0
fi

if [[ ${1:-} == release && ${2:-} == view ]]; then
  asset_json camel
  exit 0
fi

if [[ ${1:-} == release && ${2:-} == edit ]]; then
  : > "$FAKE_GH_STATE/edited"
  printf 'false\\n' > "$FAKE_GH_STATE/draft"
  exit 0
fi

if [[ ${1:-} == release && ${2:-} == delete ]]; then
  : > "$FAKE_GH_STATE/deleted"
  exit 0
fi

printf 'unexpected fake gh call: %s\\n' "$*" >&2
exit 2
""",
                encoding="utf-8",
            )
            fake_gh.chmod(0o700)

            def run_publisher(corruption):
                state = temporary / f"state-{corruption}"
                state.mkdir()
                log = state / "gh.log"
                environment = os.environ.copy()
                environment.update(
                    {
                        "PATH": f"{fake_bin}:{environment['PATH']}",
                        "TMPDIR": str(temporary),
                        "FAKE_GH_LOG": str(log),
                        "FAKE_GH_STATE": str(state),
                        "FAKE_DRAFT_CORRUPTION": corruption,
                        "FAKE_MANIFEST": str(manifest),
                        "FAKE_SIGNATURE": str(signature),
                        "FAKE_RELEASE_TAG": release_tag,
                        "FAKE_REPOSITORY": "novelKR/OpenCodex-OCI-Gateway",
                        "FAKE_SOURCE_REVISION": source_revision,
                    }
                )
                result = subprocess.run(
                    [
                        "bash",
                        str(publisher),
                        artifact_version,
                        "--repo",
                        environment["FAKE_REPOSITORY"],
                        "--source-revision",
                        source_revision,
                        "--input",
                        str(assets),
                        "--public-key",
                        str(public_key),
                    ],
                    cwd=ROOT,
                    env=environment,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                return result, log.read_text(encoding="utf-8")

            successful, successful_calls = run_publisher("none")
            self.assertEqual(successful.returncode, 0, successful.stderr)
            self.assertIn("immutable=true latest=false assets=2", successful.stdout)
            self.assertIn("release edit", successful_calls)
            self.assertNotIn("release delete", successful_calls)

            for corruption in (
                "manifest-digest",
                "manifest-size",
                "signature-digest",
                "signature-size",
            ):
                with self.subTest(corruption=corruption):
                    result, calls = run_publisher(corruption)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(
                        "runtime GitHub Release readback differs from the requested state",
                        result.stderr,
                    )
                    self.assertIn("release create", calls)
                    self.assertNotIn("release edit", calls)
                    self.assertNotIn("release delete", calls)

    def test_legacy_gateway_keeps_the_gateway_target(self):
        workflow = self.text(".github/workflows/container-release.yml")
        self.assertIn("target: gateway", workflow)
        self.assertIn("opencodex-oci-gateway", workflow)
        self.assertNotIn("ghcr.io/novelkr/opencodex-runtime", workflow)

        dockerfile = self.text("containers/opencodex/Dockerfile")
        common, runtime_and_gateway = dockerfile.split("FROM common AS runtime", 1)
        runtime, gateway = runtime_and_gateway.split("FROM common AS gateway", 1)
        self.assertGreater(
            dockerfile.rfind("FROM common AS gateway"),
            dockerfile.rfind("FROM common AS runtime"),
            "gateway must remain the default final stage for target-less builds",
        )
        self.assertNotIn("ENV OPENCODEX_HOME=", common)
        self.assertIn("ENV OPENCODEX_HOME=/var/lib/opencodex", runtime)
        self.assertNotIn("ENV OPENCODEX_HOME=", gateway)
        self.assertIn(
            'ENTRYPOINT ["/opt/opencodex/node_modules/.bin/ocx"]',
            gateway,
        )

    def test_public_export_includes_every_new_supply_contract(self):
        allowlist = set(
            self.text("config/public-export-allowlist.txt").splitlines()
        )
        for path in (
            "config/trust/opencodex-runtime-release-ed25519.pub",
            "tools/opencodex_runtime_apple_canary.py",
            "tools/opencodex_runtime_hosted_qualification.py",
            "tools/opencodex_runtime_image_test.py",
            "tools/opencodex_runtime_lifecycle_canary.py",
            "tools/opencodex_runtime_manifest.py",
            "tools/opencodex_upstream.py",
            "tools/publish-opencodex-runtime-release.sh",
        ):
            self.assertIn(path, allowlist)

    def test_runtime_trust_root_is_separate_from_the_relay_release_key(self):
        runtime_key = (ROOT / "config/trust/opencodex-runtime-release-ed25519.pub").read_bytes()
        relay_key = (ROOT / "config/trust/opencodex-relay-release-ed25519.pub").read_bytes()
        self.assertTrue(runtime_key.startswith(b"-----BEGIN PUBLIC KEY-----\n"))
        self.assertTrue(runtime_key.endswith(b"-----END PUBLIC KEY-----\n"))
        self.assertNotEqual(runtime_key, relay_key)


if __name__ == "__main__":
    unittest.main()

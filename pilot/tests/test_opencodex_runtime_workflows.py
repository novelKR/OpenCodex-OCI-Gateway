#!/usr/bin/env python3

import json
import os
import pathlib
import pwd
import re
import stat
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]


class RuntimeWorkflowContractTests(unittest.TestCase):
    def text(self, relative: str) -> str:
        return (ROOT / relative).read_text(encoding="utf-8")

    def test_github_hosted_candidate_docker_baseline_matches_input_policy(self):
        if os.environ.get("GITHUB_ACTIONS") != "true":
            self.skipTest("GitHub-hosted candidate input policy")

        def require(condition: bool, phase: str) -> None:
            if not condition:
                raise self.failureException(
                    f"candidate Docker baseline rejected: {phase}"
                )

        require(os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted", "environment")
        require(os.environ.get("RUNNER_OS") == "Linux", "operating-system")
        require(os.environ.get("RUNNER_ARCH") == "X64", "architecture")
        require(pwd.getpwuid(os.getuid()).pw_name == "runner", "user")

        expected_home = "/home/runner"
        require(os.environ.get("HOME") == expected_home, "environment-home")
        require(pwd.getpwuid(os.getuid()).pw_dir == expected_home, "passwd-home")
        node_home = subprocess.run(
            ["node", "-p", 'require("os").homedir()'],
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        require(node_home == expected_home, "node-home")

        home = pathlib.Path(expected_home)
        home_stat = home.lstat()
        require(stat.S_ISDIR(home_stat.st_mode), "runner-home-type")
        require(not stat.S_ISLNK(home_stat.st_mode), "runner-home-symlink")
        require(home_stat.st_uid == os.getuid(), "runner-home-owner")

        docker_config = home / ".docker"
        if not os.path.lexists(docker_config):
            return
        docker_stat = docker_config.lstat()
        require(stat.S_ISDIR(docker_stat.st_mode), "docker-home-type")
        require(not stat.S_ISLNK(docker_stat.st_mode), "docker-home-symlink")
        require(docker_stat.st_uid == os.getuid(), "docker-home-owner")
        require(
            stat.S_IMODE(docker_stat.st_mode) in {0o700, 0o755},
            "docker-home-mode",
        )

        children = list(docker_config.iterdir())
        require(
            all(path.name == "config.json" for path in children),
            "docker-home-children",
        )
        config = docker_config / "config.json"
        if not os.path.lexists(config):
            return
        config_stat = config.lstat()
        require(stat.S_ISREG(config_stat.st_mode), "docker-config-type")
        require(not stat.S_ISLNK(config_stat.st_mode), "docker-config-symlink")
        require(config_stat.st_uid == os.getuid(), "docker-config-owner")
        require(config_stat.st_nlink == 1, "docker-config-link-count")
        require(
            stat.S_IMODE(config_stat.st_mode) in {0o600, 0o644},
            "docker-config-mode",
        )
        require(1 <= config_stat.st_size <= 16384, "docker-config-size")

        def strict_object(pairs):
            value = {}
            for key, item in pairs:
                if key in value:
                    raise ValueError("duplicate field")
                value[key] = item
            return value

        try:
            document = json.loads(
                config.read_text(encoding="utf-8"), object_pairs_hook=strict_object
            )
        except (OSError, UnicodeError, ValueError):
            raise self.failureException(
                "candidate Docker baseline rejected: docker-config-json"
            ) from None
        require(
            isinstance(document, dict) and set(document) == {"auths"},
            "docker-config-fields",
        )
        auths = document["auths"]
        docker_hub_aliases = {
            "docker.io",
            "index.docker.io",
            "registry-1.docker.io",
            "https://index.docker.io/v1/",
        }
        require(
            isinstance(auths, dict)
            and len(auths) <= 1
            and set(auths).issubset(docker_hub_aliases),
            "docker-config-registry",
        )
        for entry in auths.values():
            require(
                isinstance(entry, dict) and set(entry) == {"auth"},
                "docker-config-entry-fields",
            )
            auth = entry["auth"]
            require(
                isinstance(auth, str) and 1 <= len(auth) <= 4096,
                "docker-config-entry",
            )

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
        self.assertIn("timeout-minutes: 60", runtime)
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

    def test_candidate_attestation_uses_canonical_owner_only_registry_credentials(self):
        workflow = self.text(".github/workflows/opencodex-runtime.yml")
        candidate = workflow.split("  candidate:\n", 1)[1].split(
            "\n  linux_arm64_canary:\n", 1
        )[0]
        linux = workflow.split("\n  linux_arm64_canary:\n", 1)[1].split(
            "\n  macos_contract:\n", 1
        )[0]
        qualification = workflow.split("\n  hosted_qualification:\n", 1)[1].split(
            "\n  verify_public_candidate:\n", 1
        )[0]
        installer = candidate.split(
            "Install the reviewed Buildx CLI before first execution", 1
        )[1].split("- uses: docker/setup-qemu-action@", 1)[0]
        credential_check = candidate.split(
            "Verify the attestation registry credential binding", 1
        )[1].split("- name: Attest the exact candidate index", 1)[0]
        attest = candidate.split("- name: Attest the exact candidate index", 1)[
            1
        ].split("- name: Read back index, platforms, attestations", 1)[0]

        candidate_permissions = candidate.split("    permissions:\n", 1)[1].split(
            "    outputs:\n", 1
        )[0]
        self.assertEqual(
            [line.strip() for line in candidate_permissions.splitlines() if line.strip()],
            [
                "attestations: write",
                "contents: read",
                "id-token: write",
                "packages: write",
            ],
        )

        for expected in (
            "fail() {",
            "ERROR: candidate Docker preflight rejected %s",
            '[[ "$RUNNER_ENVIRONMENT" == github-hosted ]]',
            '[[ "$RUNNER_OS" == Linux && "$RUNNER_ARCH" == X64 ]]',
            '[[ "$(id -un)" == runner ]]',
            'expected_home="/home/runner"',
            'pwd.getpwuid(os.getuid()).pw_dir',
            'node -p \'require("os").homedir()\'',
            '[[ "$HOME" == "$expected_home" ]]',
            '[[ "$passwd_home" == "$expected_home" ]]',
            '[[ "$node_home" == "$expected_home" ]]',
            '[[ -d "$expected_home" && ! -L "$expected_home" ]]',
            '[[ "$(stat -c \'%u\' "$expected_home")" == "$(id -u)" ]]',
            '[[ "$DOCKER_CONFIG" == "${expected_home}/.docker" ]]',
            'config="${DOCKER_CONFIG}/config.json"',
            'if [[ -e "$DOCKER_CONFIG" || -L "$DOCKER_CONFIG" ]]',
            '[[ -d "$DOCKER_CONFIG" && ! -L "$DOCKER_CONFIG" ]]',
            'directory_mode="$(stat -c \'%a\' "$DOCKER_CONFIG")"',
            '[[ "$directory_mode" == 700 || "$directory_mode" == 755 ]]',
            'find "$DOCKER_CONFIG" -mindepth 1 -maxdepth 1 ! -name config.json',
            'chmod 0700 "$DOCKER_CONFIG"',
            "docker-home-normalization",
            'if [[ -e "$config" || -L "$config" ]]',
            '[[ -f "$config" && ! -L "$config" ]]',
            'config_mode="$(stat -c \'%a\' "$config")"',
            '[[ "$config_mode" == 600 || "$config_mode" == 644 ]]',
            'chmod 0600 "$config"',
            "docker-config-normalization",
            '$(id -u):600:1',
            '"$size" -le 16384',
            "DOCKER_HUB_ALIASES = {",
            '"https://index.docker.io/v1/"',
            'len(auths) > 1',
            'set(auths).issubset(DOCKER_HUB_ALIASES)',
            'install -d -m 0700 "$DOCKER_CONFIG" "$plugin_dir"',
            '$(id -u):700',
        ):
            self.assertIn(expected, installer)
        candidate_header = candidate.split("    permissions:\n", 1)[0]
        self.assertIn("    env:\n      DOCKER_CONFIG: /home/runner/.docker", candidate_header)
        self.assertEqual(workflow.count("DOCKER_CONFIG: /home/runner/.docker"), 1)
        self.assertNotIn(
            '${RUNNER_TEMP}/opencodex-runtime-docker-config', candidate
        )
        self.assertNotIn("DOCKER_CONFIG=%s", installer)
        self.assertNotRegex(candidate, r"(?m)^\s*(?:export\s+)?HOME=")
        self.assertNotRegex(candidate, r"(?m)^\s+HOME:")

        docker_home_input = installer.split(
            'if [[ -e "$DOCKER_CONFIG" || -L "$DOCKER_CONFIG" ]]', 1
        )[1].split('chmod 0700 "$DOCKER_CONFIG"', 1)[0]
        docker_config_input = installer.split(
            'if [[ -e "$config" || -L "$config" ]]', 1
        )[1].split('chmod 0600 "$config"', 1)[0]
        self.assertNotIn("$(id -u):700", docker_home_input)
        self.assertNotIn("$(id -u):600:1", docker_config_input)
        self.assertLess(
            installer.index('directory_mode="$(stat -c \'%a\' "$DOCKER_CONFIG")"'),
            installer.index('chmod 0700 "$DOCKER_CONFIG"'),
        )
        self.assertLess(
            installer.index('config_mode="$(stat -c \'%a\' "$config")"'),
            installer.index('chmod 0600 "$config"'),
        )

        hosted_policy = self.text(
            "pilot/tests/test_opencodex_runtime_workflows.py"
        ).split(
            "def test_github_hosted_candidate_docker_baseline_matches_input_policy",
            1,
        )[1].split("def test_upstream_watch_separates_read_only_detection", 1)[0]
        for expected in (
            'os.environ.get("GITHUB_ACTIONS") != "true"',
            'os.environ.get("RUNNER_ENVIRONMENT") == "github-hosted"',
            'os.environ.get("RUNNER_OS") == "Linux"',
            'os.environ.get("RUNNER_ARCH") == "X64"',
            'pwd.getpwuid(os.getuid()).pw_name == "runner"',
            'expected_home = "/home/runner"',
            "stat.S_IMODE(docker_stat.st_mode) in {0o700, 0o755}",
            "config_stat.st_nlink == 1",
            "stat.S_IMODE(config_stat.st_mode) in {0o600, 0o644}",
            'set(document) == {"auths"}',
            "set(auths).issubset(docker_hub_aliases)",
        ):
            self.assertIn(expected, hosted_policy)
        for forbidden in (
            "print(",
            "hashlib",
            "base64",
            "GITHUB_OUTPUT",
            "GITHUB_STEP_SUMMARY",
            "upload-artifact",
        ):
            self.assertNotIn(forbidden, hosted_policy)

        login = candidate.split("- name: Log into ghcr.io", 1)[1].split(
            "- name: Bind candidate identity", 1
        )[0]
        self.assertEqual(candidate.count("- name: Log into ghcr.io"), 1)
        self.assertEqual(candidate.count("docker/login-action@"), 1)
        self.assertIn("uses: docker/login-action@", login)
        self.assertIn("registry: ghcr.io", login)
        self.assertIn("username: ${{ github.actor }}", login)
        self.assertIn("password: ${{ github.token }}", login)
        self.assertIn("logout: true", login)

        for expected in (
            '[[ "$DOCKER_CONFIG" == "${canonical_home}/.docker" ]]',
            '[[ -d "$DOCKER_CONFIG" && ! -L "$DOCKER_CONFIG" ]]',
            'config="${DOCKER_CONFIG}/config.json"',
            '[[ -f "$config" && ! -L "$config" ]]',
            "stat -c '%u'",
            "stat -c '%h'",
            "stat -c '%s'",
            '"$size" -ge 1',
            '"$size" -le 16384',
            'chmod 0600 "$config"',
            "stat -c '%a'",
            "object_pairs_hook=strict_object",
            "duplicate Docker credential configuration field",
            'set(document) != {"auths"}',
            '"ghcr.io" not in auths',
            'set(auths) - {"ghcr.io"}',
            "len(other_registries) > 1",
            "other_registries.issubset(docker_hub_aliases)",
            'set(entry) != {"auth"}',
            "not 1 <= len(auth) <= 4096",
        ):
            self.assertIn(expected, credential_check)
        self.assertNotIn("print(auth", credential_check)
        self.assertNotRegex(credential_check, r"\b(?:cp|ln)\s+")
        for forbidden in (
            "base64",
            "hashlib",
            "sys.stdout",
            "GITHUB_OUTPUT",
            "GITHUB_STEP_SUMMARY",
            "upload-artifact",
            "docker login",
        ):
            self.assertNotIn(forbidden, credential_check)

        expected_docker_hub_aliases = {
            "docker.io",
            "index.docker.io",
            "registry-1.docker.io",
            "https://index.docker.io/v1/",
        }
        for source, marker in (
            (installer, "DOCKER_HUB_ALIASES = {"),
            (credential_check, "docker_hub_aliases = {"),
        ):
            alias_block = source.split(marker, 1)[1].split("}", 1)[0]
            aliases = re.findall(r'^\s*"([^"]+)",\s*$', alias_block, re.MULTILINE)
            self.assertEqual(len(aliases), len(expected_docker_hub_aliases))
            self.assertEqual(set(aliases), expected_docker_hub_aliases)
        self.assertNotIn("credsStore", candidate)
        self.assertNotIn("credHelpers", candidate)
        self.assertNotRegex(
            candidate,
            r'(?m)^\s*(?:cp|ln)\b[^\n]*(?:DOCKER_CONFIG|/home/runner/\.docker)',
        )

        self.assertEqual(candidate.count("actions/attest-build-provenance@"), 1)
        self.assertIn("subject-name: ${{ env.RUNTIME_IMAGE }}", attest)
        self.assertIn("subject-digest: ${{ steps.build.outputs.digest }}", attest)
        self.assertIn("push-to-registry: true", attest)

        self.assertLess(
            candidate.index("docker/login-action@"),
            candidate.index("Build and push the two-platform candidate exactly once"),
        )
        self.assertLess(
            candidate.index("Build and push the two-platform candidate exactly once"),
            candidate.index("Verify the attestation registry credential binding"),
        )
        self.assertLess(
            candidate.index("Verify the attestation registry credential binding"),
            candidate.index("Attest the exact candidate index"),
        )
        self.assertLess(
            candidate.index("Attest the exact candidate index"),
            candidate.index("Read back index, platforms, attestations"),
        )
        self.assertLess(
            candidate.index("Read back index, platforms, attestations"),
            candidate.index("Preserve the exact candidate witness"),
        )

        self.assertIn(
            '${RUNNER_TEMP}/opencodex-runtime-arm64-docker-config', linux
        )
        self.assertIn(
            '${RUNNER_TEMP}/opencodex-runtime-qualification-docker-config',
            qualification,
        )
        candidate_artifact = candidate.split(
            "Preserve the exact candidate witness", 1
        )[1]
        self.assertIn(
            "path: ${{ runner.temp }}/opencodex-runtime-candidate",
            candidate_artifact,
        )
        self.assertNotIn("$DOCKER_CONFIG", candidate_artifact)
        self.assertNotIn("/home/runner/.docker", candidate_artifact)
        self.assertNotIn("${HOME}", candidate_artifact)
        self.assertNotIn("$HOME", candidate_artifact)

    def test_npm_provenance_is_pinned_and_rechecked_at_build_boundaries(self):
        helper = self.text("tools/opencodex_npm_provenance.py")
        identity_helper = self.text("tools/verify_npm_slsa_identity.cjs")
        upstream = self.text("tools/opencodex_upstream.py")
        ci = self.text(".github/workflows/ci.yml")
        runtime = self.text(".github/workflows/opencodex-runtime.yml")
        gateway_release = self.text(".github/workflows/container-release.yml")
        watch = self.text(".github/workflows/opencodex-upstream-watch.yml")

        self.assertIn('NPM_PROVENANCE_VERIFIER_VERSION = "11.19.1"', helper)
        self.assertIn(
            "sha512-ztsxKxt/kkIaAs+2i0GU6I+DRmUdrNasxTZKJe9TCdSjKxlhah/4r/",
            helper,
        )
        self.assertIn(
            "node:24.20.0-bookworm-slim@",
            helper,
        )
        self.assertIn(
            "sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e",
            helper,
        )
        self.assertIn(
            '["audit", "signatures", "--json", "--include-attestations"]',
            helper,
        )
        self.assertIn('f"--user={os.getuid()}:{os.getgid()}"', helper)
        self.assertIn("certificateIdentityURI: CERTIFICATE_IDENTITY_PATTERN", identity_helper)
        self.assertIn("certificateIssuer: CERTIFICATE_ISSUER", identity_helper)
        self.assertIn("^https://github\\\\.com/lidge-jun/opencodex/", identity_helper)
        self.assertIn("https://token.actions.githubusercontent.com", identity_helper)
        self.assertIn("validate_slsa_identity_evidence", helper)
        self.assertIn("npm_version_json(NPM_PACKAGE, version)", upstream)
        self.assertIn('git_head != revision', upstream)
        self.assertIn("client.verify_npm_provenance", upstream)
        for workflow in (ci, runtime, gateway_release, watch):
            self.assertIn("--verify-provenance", workflow)

    def test_candidate_only_tree_has_no_stable_creator_or_publisher(self):
        manifest = self.text("tools/opencodex_runtime_manifest.py")
        self.assertFalse((ROOT / "tools/publish-opencodex-runtime-release.sh").exists())
        self.assertNotIn('commands.add_parser("create")', manifest)
        self.assertNotIn("def command_create(", manifest)
        self.assertIn('commands.add_parser("create-candidate")', manifest)
        self.assertIn('commands.add_parser("verify-candidate")', manifest)
        self.assertIn('commands.add_parser("verify")', manifest)

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
            "tools/opencodex_npm_provenance.py",
            "tools/opencodex_upstream.py",
            "tools/verify_npm_slsa_identity.cjs",
        ):
            self.assertIn(path, allowlist)
        self.assertNotIn("tools/publish-opencodex-runtime-release.sh", allowlist)

    def test_runtime_trust_root_is_separate_from_the_relay_release_key(self):
        runtime_key = (ROOT / "config/trust/opencodex-runtime-release-ed25519.pub").read_bytes()
        relay_key = (ROOT / "config/trust/opencodex-relay-release-ed25519.pub").read_bytes()
        self.assertTrue(runtime_key.startswith(b"-----BEGIN PUBLIC KEY-----\n"))
        self.assertTrue(runtime_key.endswith(b"-----END PUBLIC KEY-----\n"))
        self.assertNotEqual(runtime_key, relay_key)


if __name__ == "__main__":
    unittest.main()

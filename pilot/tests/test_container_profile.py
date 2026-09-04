#!/usr/bin/env python3

import json
import os
import pathlib
import re
import shlex
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
CONTAINER = ROOT / "containers" / "opencodex"
COMPOSE = CONTAINER / "compose.experimental.yaml"
DOCKERFILE = CONTAINER / "Dockerfile"
DOCKERIGNORE = CONTAINER / ".dockerignore"
UNIT = ROOT / "pilot" / "systemd" / "opencodex-container.service"
HELPER = ROOT / "pilot" / "libexec" / "opencodex-container"
WORKFLOW = ROOT / ".github" / "workflows" / "container-release.yml"
ROOT_README = ROOT / "README.md"
GATEWAY_GUIDE = ROOT / "docs" / "gateway.md"
CONTAINER_PROFILE = ROOT / "docs" / "container-profile.md"
RUNTIME_README = CONTAINER / "README.md"
RUNTIME_README_KO = CONTAINER / "README.ko.md"


def local_markdown_targets(document: pathlib.Path) -> list[pathlib.Path]:
    targets: list[pathlib.Path] = []
    text = document.read_text(encoding="utf-8")
    for match in re.finditer(r"(?<!!)\[[^\]]+\]\(([^)]+)\)", text):
        destination = match.group(1).strip()
        if destination.startswith("<") and destination.endswith(">"):
            destination = destination[1:-1]
        if (
            not destination
            or destination.startswith("#")
            or re.match(r"^[A-Za-z][A-Za-z0-9+.-]*:", destination)
        ):
            continue
        target = destination.split("#", 1)[0]
        if target:
            targets.append((document.parent / target).resolve())
    return targets


class ContainerProfileTests(unittest.TestCase):
    def test_repository_readme_is_a_two_track_hub_with_resolvable_guides(self) -> None:
        root_readme = ROOT_README.read_text(encoding="utf-8")
        self.assertTrue(
            root_readme.startswith("# OpenCodex OCI Gateway and Runtime\n")
        )
        for expected in (
            "ghcr.io/novelkr/opencodex-oci-gateway",
            "ghcr.io/novelkr/opencodex-runtime",
            "docs/gateway.md",
            "containers/opencodex/README.md",
            "containers/opencodex/README.ko.md",
            "Latest",
        ):
            self.assertIn(expected, root_readme)

        documents = (
            ROOT_README,
            GATEWAY_GUIDE,
            CONTAINER_PROFILE,
            RUNTIME_README,
            RUNTIME_README_KO,
        )
        for document in documents:
            self.assertTrue(document.is_file(), document)
            for target in local_markdown_targets(document):
                with self.subTest(document=document, target=target):
                    self.assertTrue(target.exists(), target)

    def test_runtime_readmes_share_candidate_and_watcher_contracts(self) -> None:
        english = RUNTIME_README.read_text(encoding="utf-8")
        korean = RUNTIME_README_KO.read_text(encoding="utf-8")
        self.assertIn("English is the canonical version", english)
        self.assertIn("정본", korean)

        shared_contracts = (
            "ghcr.io/novelkr/opencodex-runtime",
            "ghcr.io/novelkr/opencodex-oci-gateway",
            "linux/amd64",
            "linux/arm64",
            "candidate-only",
            "sha256-*",
            "--bundle-from-oci",
            "GitHub Attestations API",
            "public_ready=true",
            "anonymous_exact_digest_pull=true",
            "apple_container_live=false",
            "stable_promotion_eligible=false",
            "OPENCODEX_UPSTREAM_WATCH_APP_CLIENT_ID",
            "OPENCODEX_UPSTREAM_WATCH_APP_PRIVATE_KEY",
            "/run/opencodex/bootstrap.sock",
            "10001:10001",
            "10100",
            "config/trust/opencodex-runtime-release-ed25519.pub",
        )
        for document in (english, korean):
            for expected in shared_contracts:
                with self.subTest(expected=expected):
                    self.assertIn(expected, document)
            self.assertNotRegex(document, r"sha256:[0-9a-f]{64}")
            self.assertNotRegex(
                document,
                r"candidate-[0-9]+\.[0-9]+\.[0-9]+-r[0-9]+-[0-9a-f]{40}",
            )
            self.assertNotRegex(document, r"\b[0-9]+\.[0-9]+\.[0-9]+\b")
            self.assertNotRegex(document, r"actions/runs/[0-9]+")

    def test_runtime_metadata_is_isolated_from_the_gateway_stage(self) -> None:
        dockerfile = DOCKERFILE.read_text(encoding="utf-8")
        common, remainder = dockerfile.split("FROM common AS runtime\n", 1)
        runtime, gateway = remainder.split("FROM common AS gateway\n", 1)
        metadata = (
            'org.opencontainers.image.title="OpenCodex Runtime Candidate"',
            "org.opencontainers.image.description=",
            "org.opencontainers.image.documentation=",
        )
        for value in metadata:
            self.assertIn(value, runtime)
            self.assertNotIn(value, common)
            self.assertNotIn(value, gateway)
        self.assertTrue(dockerfile.rstrip().endswith('CMD ["start", "--port", "10100"]'))

    def test_container_profile_states_actual_authority_and_acceptance_limits(self) -> None:
        profile = CONTAINER_PROFILE.read_text(encoding="utf-8")
        normalized = " ".join(profile.split())
        self.assertIn("This profile containerizes only OpenCodex", normalized)
        self.assertNotIn("This profile containers only OpenCodex", normalized)
        self.assertIn("authoritative upstream release and provenance record", normalized)
        self.assertIn("Contents read/write", normalized)
        self.assertIn("Pull requests read/write", normalized)
        self.assertIn("The workflow—not the App permission model", normalized)
        self.assertIn("GitHub Attestations API", normalized)
        self.assertIn("public_ready=true", normalized)
        self.assertIn("apple_container_live=false", normalized)
        self.assertIn("stable_promotion_eligible=false", normalized)
        self.assertIn("corresponding private half was not retained", normalized)
        self.assertIn("not a production signing authority", normalized)

    def test_compose_enforces_experimental_security_contract(self) -> None:
        environment = os.environ.copy()
        environment.update(
            {
                "OPENCODEX_CONTAINER_IMAGE":
                    "ghcr.io/novelkr/opencodex-oci-gateway@sha256:" + "a" * 64,
                "OPENCODEX_UID": "10001",
                "OPENCODEX_GID": "10001",
            }
        )
        result = subprocess.run(
            ["docker", "compose", "-f", str(COMPOSE), "config"],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = result.stdout
        self.assertIn("network_mode: host", rendered)
        self.assertIn("read_only: true", rendered)
        self.assertIn("- ALL", rendered)
        self.assertIn("no-new-privileges:true", rendered)
        self.assertIn("pids_limit: 256", rendered)
        self.assertNotIn("published:", rendered)
        self.assertNotIn("docker.sock", rendered)
        self.assertIn(
            "source: /var/lib/opencodex\n"
            "        target: /var/lib/opencodex",
            rendered,
        )

    def test_image_inputs_are_exactly_locked(self) -> None:
        dockerfile = DOCKERFILE.read_text(encoding="utf-8")
        package = (DOCKERFILE.parent / "package.json").read_text(encoding="utf-8")
        package_document = json.loads(package)
        upstream_lock = json.loads(
            (DOCKERFILE.parent / "upstream.lock.json").read_text(encoding="utf-8")
        )
        lockfile = (DOCKERFILE.parent / "bun.lock").read_text(encoding="utf-8")
        notices = (DOCKERFILE.parent / "UPSTREAM_NOTICES.md").read_text(encoding="utf-8")
        workflow = WORKFLOW.read_text(encoding="utf-8")
        opencodex_version = package_document["dependencies"]["@bitkyc08/opencodex"]
        self.assertIn(
            "oven/bun:1.4.0-alpine@sha256:"
            "07235578f79ef8c6f97d94aee7938e76f5cdba5f21ae5dbfdd3d3d38058437eb",
            dockerfile,
        )
        self.assertEqual(opencodex_version, upstream_lock["version"])
        self.assertEqual(upstream_lock["npm"]["version"], upstream_lock["version"])
        self.assertIn("COPY package.json bun.lock upstream.lock.json ./", dockerfile)
        self.assertIn("installed.version !== lock.version", dockerfile)
        self.assertNotIn('installed.version !== "', dockerfile)
        self.assertIn(
            f'"@bitkyc08/opencodex": "{upstream_lock["version"]}"', package
        )
        self.assertIn(
            f'"@bitkyc08/opencodex": "{upstream_lock["version"]}"', lockfile
        )
        self.assertIn(upstream_lock["npm"]["integrity"], lockfile)
        self.assertIn(
            f"@bitkyc08/opencodex {upstream_lock['version']}", notices
        )
        self.assertIn("oven/bun:1.4.0-alpine", notices)
        self.assertIn("containers/opencodex/package.json", workflow)
        self.assertIn(
            "tags: ghcr.io/novelkr/opencodex-oci-gateway:"
            "${{ steps.version.outputs.value }}-ocx-"
            "${{ steps.version.outputs.opencodex }}",
            workflow,
        )
        self.assertNotIn(f"-ocx-{opencodex_version}\n", workflow)
        self.assertNotIn("-ocx-2.22.0", workflow)

    def test_every_local_dockerfile_copy_source_is_in_the_context_allowlist(self) -> None:
        dockerfile = DOCKERFILE.read_text(encoding="utf-8")
        ignore_lines = {
            line.strip()
            for line in DOCKERIGNORE.read_text(encoding="utf-8").splitlines()
            if line.strip() and not line.lstrip().startswith("#")
        }
        self.assertIn("*", ignore_lines)
        self.assertNotIn("!README.md", ignore_lines)
        self.assertNotIn("!README.ko.md", ignore_lines)
        copied_sources: set[str] = set()
        for line in dockerfile.splitlines():
            if not line.startswith("COPY "):
                continue
            tokens = shlex.split(line)
            operands = [token for token in tokens[1:] if not token.startswith("--")]
            self.assertGreaterEqual(len(operands), 2, line)
            copied_sources.update(operands[:-1])

        self.assertTrue(copied_sources)
        for source in copied_sources:
            self.assertNotIn("*", source, "COPY globs need an explicit context contract")
            self.assertIn(f"!{source}", ignore_lines, source)

    def test_systemd_profiles_are_mutually_exclusive_and_digest_gated(self) -> None:
        unit = UNIT.read_text(encoding="utf-8")
        helper = HELPER.read_text(encoding="utf-8")
        self.assertIn("Conflicts=opencodex.service", unit)
        self.assertIn("opencodex-container verify", unit)
        self.assertIn("@sha256:", helper)
        self.assertIn("docker image inspect", helper)
        self.assertNotIn("/var/lib/docker", unit)
        self.assertIn("all($s.volumes[];", helper)


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3

import json
import os
import pathlib
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


class ContainerProfileTests(unittest.TestCase):
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

#!/usr/bin/env python3

import json
import os
import pathlib
import subprocess
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
CONTAINER = ROOT / "containers" / "opencodex"
COMPOSE = CONTAINER / "compose.experimental.yaml"
DOCKERFILE = CONTAINER / "Dockerfile"
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
        lockfile = (DOCKERFILE.parent / "bun.lock").read_text(encoding="utf-8")
        notices = (DOCKERFILE.parent / "UPSTREAM_NOTICES.md").read_text(encoding="utf-8")
        workflow = WORKFLOW.read_text(encoding="utf-8")
        opencodex_version = package_document["dependencies"]["@bitkyc08/opencodex"]
        self.assertIn(
            "oven/bun:1.4.0-alpine@sha256:"
            "07235578f79ef8c6f97d94aee7938e76f5cdba5f21ae5dbfdd3d3d38058437eb",
            dockerfile,
        )
        self.assertIn('= "2.33.0"', dockerfile)
        self.assertIn('"@bitkyc08/opencodex": "2.33.0"', package)
        self.assertIn('"@bitkyc08/opencodex": "2.33.0"', lockfile)
        self.assertIn(
            "sha512-lZISJQa+oTiIeyydQ1llUFOYH15FkfTsQdbGku/KPPAmrzYJmOTetDDq0/rZt6SERuP5BY9nXnBUKTlvCoK60A==",
            lockfile,
        )
        self.assertIn("@bitkyc08/opencodex 2.33.0", notices)
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

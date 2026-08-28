#!/usr/bin/env python3

import hashlib
import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
REPO_ROOT = PILOT.parent
TOOL = PILOT / "tools" / "deployment_config.py"
EXAMPLE = REPO_ROOT / "config" / "deployment.example.toml"


def load_tool():
    spec = importlib.util.spec_from_file_location("deployment_config", TOOL)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


@unittest.skipIf(sys.version_info < (3, 11), "deployment renderer requires Python 3.11+")
class DeploymentConfigTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.tool = load_tool()

    def valid_text(self) -> str:
        return EXAMPLE.read_text(encoding="utf-8")

    def write(self, root: Path, text: str) -> Path:
        path = root / "deployment.toml"
        path.write_text(text, encoding="utf-8")
        return path

    def test_example_validates_and_contains_no_secret_field(self) -> None:
        config = self.tool.load_config(EXAMPLE)
        self.assertEqual(config.deployment, "example")
        self.assertEqual(config.remote_mode, "external")
        lowered = self.valid_text().lower()
        forbidden_fields = (
            "token =",
            "secret =",
            "password =",
            "private_key =",
            "gateway_key =",
        )
        for forbidden in forbidden_fields:
            self.assertNotIn(forbidden, lowered)

    def test_unknown_and_secret_bearing_fields_fail_closed(self) -> None:
        cases = {
            "unknown": self.valid_text() + '\nunexpected = "value"\n',
            "token": self.valid_text() + '\napi_token = "not-a-real-secret"\n',
            "nested secret": self.valid_text().replace(
                'team_name = "example-team"',
                'team_name = "example-team"\nclient_secret = "not-a-real-secret"',
            ),
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, text in cases.items():
                with self.subTest(name=name):
                    with self.assertRaises(self.tool.DeploymentConfigError):
                        self.tool.load_config(self.write(root, text))

    def test_interpolation_shell_url_path_and_bad_hostname_are_rejected(self) -> None:
        cases = {
            "environment": self.valid_text().replace(
                "https://api.example.com", "https://" + "$" + "{API_HOST}"
            ),
            "shell": self.valid_text().replace(
                "example-team", "$" + "(id)"
            ),
            "path": self.valid_text().replace(
                "https://api.example.com", "https://api.example.com/v1"
            ),
            "query": self.valid_text().replace(
                "https://api.example.com", "https://api.example.com?x=1"
            ),
            "port": self.valid_text().replace(
                "https://api.example.com", "https://api.example.com:443"
            ),
            "hostname": self.valid_text().replace("ssh.example.com", "LOCALHOST"),
        }
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name, text in cases.items():
                with self.subTest(name=name):
                    with self.assertRaises(self.tool.DeploymentConfigError):
                        self.tool.load_config(self.write(root, text))

    def test_render_is_deterministic_and_uses_fixed_security_ports(self) -> None:
        config = self.tool.load_config(EXAMPLE)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            first, names = self.tool.render(config, root)
            first_hashes = {
                name: hashlib.sha256((first / name).read_bytes()).hexdigest()
                for name in names
            }
            second, second_names = self.tool.render(config, root)
            second_hashes = {
                name: hashlib.sha256((second / name).read_bytes()).hexdigest()
                for name in second_names
            }
            self.assertEqual(first_hashes, second_hashes)
            self.assertEqual(names, second_names)
            remote = json.loads(
                (second / "remote-opencodex.json").read_text(encoding="utf-8")
            )
            relay = json.loads((second / "relay.json").read_text(encoding="utf-8"))
            self.assertEqual(remote["api_origin"], "https://api.example.com")
            self.assertEqual(relay["listen_address"], "127.0.0.1:18180")
            self.assertEqual(
                relay["upstream_base_url"], "https://api.example.com/v1"
            )
            compose = (second / "compose.yaml").read_text(encoding="utf-8")
            self.assertIn("network_mode: host", compose)
            self.assertIn("read_only: true", compose)
            self.assertNotIn("ports:", compose)

    def test_local_relay_render_has_no_edge_credential_source(self) -> None:
        text = self.valid_text().replace(
            'remote_routing_mode = "relay"',
            'remote_routing_mode = "local-relay"',
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = self.tool.load_config(self.write(root, text))
            output, _ = self.tool.render(config, root / "out")
            relay = json.loads((output / "relay.json").read_text(encoding="utf-8"))
            remote = json.loads(
                (output / "remote-opencodex.json").read_text(encoding="utf-8")
            )
            self.assertEqual(relay["credentials"], {"source": "none"})
            self.assertEqual(
                relay["upstream_base_url"], "http://127.0.0.1:10100/v1"
            )
            self.assertEqual(remote["mode"], "loopback")

    def test_legacy_routing_mode_is_not_a_public_deployment_target(self) -> None:
        text = self.valid_text().replace(
            'remote_routing_mode = "relay"',
            'remote_routing_mode = "legacy"',
        )
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(
                self.tool.DeploymentConfigError,
                "remote_routing_mode must be relay or local-relay",
            ):
                self.tool.load_config(self.write(Path(directory), text))

    def test_cli_returns_stable_json(self) -> None:
        result = subprocess.run(
            [sys.executable, str(TOOL), "validate", str(EXAMPLE)],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        summary = json.loads(result.stdout)
        self.assertEqual(summary["schema_version"], 1)
        self.assertTrue(summary["valid"])


if __name__ == "__main__":
    unittest.main()

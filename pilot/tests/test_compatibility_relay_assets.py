#!/usr/bin/env python3

import base64
import hashlib
import json
import os
import plistlib
import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
REPO_ROOT = PILOT.parent
RELAY = REPO_ROOT / "client" / "relay"
TRUSTED_CODEX_BUNDLE_ID = "com.openai.codex"
TRUSTED_CODEX_TEAM_ID = "2DC432GLL2"


def relay_scheduler_health(
    lane: str, interactive_listener: str = "127.0.0.1:18182"
) -> dict[str, object]:
    return {
        "ok": True,
        "listener_lane": lane,
        "general_listener": "127.0.0.1:18180",
        "interactive_listener": interactive_listener,
        "upstream_mode": "external_gateway",
        "upstream_base_url": "https://example.test/v1",
        "catalog_owner": "relay",
        "responses_websocket_mode": "passthrough",
        "responses_models": [],
        "responses_normalizer": False,
        "active_requests": 0,
        "active_classifications": 0,
        "pending_requests": 0,
        "pending_encoded_bytes": 0,
        "active_general_upstream": 0,
        "active_interactive_upstream": 0,
        "active_transforms": 0,
        "active_deliveries": 0,
        "capacity_rejections": 0,
        "scheduler_limits": {
            "max_classifications": 8,
            "max_pending_requests": 24,
            "max_pending_encoded_bytes": 536870912,
            "queue_timeout_ms": 60000,
            "max_general_upstream": 4,
            "interactive_reserved_upstream": 1,
            "max_concurrent_transforms": 2,
            "max_open_deliveries": 16,
        },
    }


class CompatibilityRelayAssetTests(unittest.TestCase):
    def test_relay_shell_assets_parse(self) -> None:
        scripts = sorted((RELAY / "scripts").glob("*.sh"))
        self.assertTrue(scripts)
        result = subprocess.run(
            ["bash", "-n", *(str(path) for path in scripts)],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_container_runtime_activation_uses_the_exact_desktop_exit_gate(self) -> None:
        sources = RELAY / "macos" / "OpenCodexRelay" / "Sources" / "OpenCodexRelay"
        view = (sources / "ContainerRuntimeView.swift").read_text(encoding="utf-8")
        model = (sources / "MenuBarModel.swift").read_text(encoding="utf-8")
        self.assertIn("model.activateContainerRuntime(controller)", view)
        self.assertNotIn("activateConfirmed(desktopExited: true)", view)

        start = model.index("func activateContainerRuntime(")
        end = model.index("private func runDesktopExitCheckedCommand", start)
        activation = model[start:end]
        exit_check = activation.index("ensureVerifiedDesktopExited(at: desktopURL)")
        mutation = activation.index("activateAfterVerifiedDesktopExit(expected: witness)")
        self.assertLess(exit_check, mutation)
        self.assertIn("let witness = controller.activationWitness", activation)
        self.assertIn("defer { self.isBusy = false }", activation)

    def test_credential_loader_is_not_hidden_by_a_broad_secret_pattern(self) -> None:
        ignore_lines = (REPO_ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
        self.assertNotIn("credentials*", ignore_lines)
        self.assertIn("credentials.env", ignore_lines)
        self.assertTrue((RELAY / "internal" / "credentials" / "credentials.go").is_file())
        self.assertTrue((RELAY / "internal" / "credentials" / "credentials_test.go").is_file())

    def test_gateway_keeps_an_exact_allowlist_and_voice_closed_by_default(self) -> None:
        api = (PILOT / "nginx" / "opencodex-api.conf").read_text(encoding="utf-8")
        websocket = (PILOT / "nginx" / "opencodex-websocket-proxy.conf").read_text(encoding="utf-8")
        flags = (PILOT / "nginx" / "opencodex-feature-flags.deny-all.conf").read_text(encoding="utf-8")
        for route in (
            "location = /v1/models",
            "location = /v1/responses",
            "location = /v1/responses/compact",
            "location = /v1/images/generations",
            "location = /v1/images/edits",
            "location ~ ^/v1/opencodex/artifacts/[^/]+$",
            "location = /v1/alpha/search",
        ):
            self.assertIn(route, api)
        self.assertNotIn("location /v1/", api)
        self.assertEqual(api.count("if ($opencodex_voice_enabled != 1) { return 404; }"), 5)
        self.assertNotIn("if ($opencodex_voice_enabled = 0)", api)
        self.assertIn("set $opencodex_voice_enabled 0;", flags)
        self.assertIn('proxy_set_header Connection "upgrade";', websocket)
        self.assertIn('proxy_set_header X-OpenCodex-API-Key "";', websocket)
        self.assertIn('proxy_set_header CF-Access-Client-Secret "";', websocket)

    def test_gateway_uses_separate_emergency_guards_without_serializing_turns(self) -> None:
        api = (PILOT / "nginx" / "opencodex-api.conf").read_text(encoding="utf-8")
        self.assertEqual(api.count("limit_conn opencodex_generation 32;"), 5)
        self.assertEqual(api.count("limit_conn opencodex_responses_websocket 32;"), 1)
        self.assertNotIn("limit_conn opencodex_generation 1;", api)
        self.assertIn("Gateway generation safety ceiling reached.", api)
        self.assertIn("X-OpenCodex-WebSocket-Concurrency-Limit 32", api)

    def test_gateway_deployment_installs_and_rolls_back_feature_assets(self) -> None:
        bootstrap = (PILOT / "scripts" / "bootstrap-host.sh").read_text(encoding="utf-8")
        deploy = (PILOT / "scripts" / "deploy-gateway-config.sh").read_text(encoding="utf-8")
        feature = (PILOT / "scripts" / "configure-gateway-features.sh").read_text(encoding="utf-8")
        for content in (bootstrap, deploy):
            self.assertIn("opencodex-websocket-proxy.conf", content)
            self.assertIn("opencodex-feature-flags", content)
        self.assertIn("voice on|off", feature)
        self.assertIn("nginx -t", feature)
        self.assertIn("previous state was restored", feature)
        self.assertIn("validate_feature_flags_file", deploy)
        self.assertIn("count != 1", deploy)

    def test_linux_service_uses_configured_catalog_parent_and_reexecs_active_relay(self) -> None:
        installer = RELAY / "scripts" / "install-service.sh"
        for active, enabled in ((True, "enabled"), (False, "disabled")):
            with self.subTest(active=active, enabled=enabled), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                home = root / "home"
                fake_bin = root / "bin"
                config_dir = home / ".config" / "opencodex-relay"
                catalog_parent = home / ".codex-remote-opencodex"
                relay_bin = home / ".local" / "lib" / "opencodex-relay" / "relay" / "current" / "opencodex-relay"
                relay_bin.parent.mkdir(parents=True)
                relay_bin.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                relay_bin.chmod(0o700)
                config_dir.mkdir(parents=True)
                catalog_parent.mkdir()
                config_path = config_dir / "relay.json"
                config_path.write_text(
                    json.dumps({"catalog": {"path": str(catalog_parent / "catalog.json")}}),
                    encoding="utf-8",
                )
                fake_bin.mkdir()
                uname = fake_bin / "uname"
                uname.write_text("#!/bin/sh\nprintf 'Linux\\n'\n", encoding="utf-8")
                uname.chmod(0o700)
                realpath = fake_bin / "realpath"
                realpath.write_text(
                    "#!/bin/sh\n"
                    "if [ \"${1:-}\" = -m ]; then shift; fi\n"
                    "if [ \"${1:-}\" = -- ]; then shift; fi\n"
                    "/usr/bin/python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' \"$1\"\n",
                    encoding="utf-8",
                )
                realpath.chmod(0o700)
                systemctl_log = root / "systemctl.log"
                systemctl = fake_bin / "systemctl"
                systemctl.write_text(
                    "#!/bin/sh\n"
                    "printf '%s\\n' \"$*\" >> \"$SYSTEMCTL_LOG\"\n"
                    "if [ \"${1:-}\" = --user ]; then shift; fi\n"
                    "if [ \"${1:-}\" = is-active ]; then [ \"$SERVICE_ACTIVE\" = 1 ]; exit; fi\n"
                    "if [ \"${1:-}\" = is-enabled ]; then\n"
                    "  printf '%s\\n' \"$SERVICE_ENABLED\"\n"
                    "  [ \"$SERVICE_ENABLED\" = enabled ] && exit 0 || exit 1\n"
                    "fi\n"
                    "exit 0\n",
                    encoding="utf-8",
                )
                systemctl.chmod(0o700)
                env = os.environ.copy()
                env.update(
                    {
                        "HOME": str(home),
                        "XDG_CONFIG_HOME": str(home / ".config"),
                        "PATH": str(fake_bin) + os.pathsep + env["PATH"],
                        "SYSTEMCTL_LOG": str(systemctl_log),
                        "SERVICE_ACTIVE": "1" if active else "0",
                        "SERVICE_ENABLED": enabled,
                    }
                )
                result = subprocess.run(
                    ["bash", str(installer), "install", "--config", str(config_path)],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                unit = (home / ".config" / "systemd" / "user" / "opencodex-relay.service").read_text(encoding="utf-8")
                self.assertIn(f'ReadWritePaths="{catalog_parent.resolve()}"', unit)
                self.assertNotIn("__CATALOG_PARENT__", unit)
                calls = systemctl_log.read_text(encoding="utf-8").splitlines()
                self.assertIn("--user enable opencodex-relay.service", calls)
                if active:
                    self.assertIn("--user restart opencodex-relay.service", calls)
                    self.assertNotIn("--user start opencodex-relay.service", calls)
                else:
                    self.assertIn("--user start opencodex-relay.service", calls)
                    self.assertNotIn("--user restart opencodex-relay.service", calls)
                status = subprocess.run(
                    ["bash", str(installer), "status"],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                self.assertEqual(status.returncode, 0, status.stderr)
                self.assertIn(f"relay_service_active={'true' if active else 'false'}", status.stdout)
                snapshot_dir = root / "service-snapshot"
                snapshot_dir.mkdir()
                snapshot = subprocess.run(
                    ["bash", str(installer), "snapshot", "--directory", str(snapshot_dir)],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                self.assertEqual(snapshot.returncode, 0, snapshot.stderr)
                expected_unit = unit
                unit_path = home / ".config" / "systemd" / "user" / "opencodex-relay.service"
                unit_path.write_text("[Unit]\nDescription=damaged\n", encoding="utf-8")
                restored_snapshot = subprocess.run(
                    ["bash", str(installer), "restore-snapshot", "--directory", str(snapshot_dir)],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                self.assertEqual(restored_snapshot.returncode, 0, restored_snapshot.stderr)
                self.assertEqual(unit_path.read_text(encoding="utf-8"), expected_unit)
                restored = subprocess.run(
                    ["bash", str(installer), "restore", "--was-active", "true" if active else "false"],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=env,
                )
                self.assertEqual(restored.returncode, 0, restored.stderr)
                calls = systemctl_log.read_text(encoding="utf-8").splitlines()
                if active:
                    self.assertGreaterEqual(calls.count("--user restart opencodex-relay.service"), 3)
                    self.assertGreaterEqual(calls.count("--user enable opencodex-relay.service"), 2)
                else:
                    self.assertIn("--user stop opencodex-relay.service", calls)
                    self.assertIn("--user reset-failed opencodex-relay.service", calls)
                    self.assertIn("--user disable opencodex-relay.service", calls)

    def test_fresh_linux_install_activation_failure_restores_routing_and_unit(self) -> None:
        installer_path = RELAY / "scripts" / "install-relay.sh"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            asset_dir = root / "assets"
            fake_bin = root / "bin"
            tmp_dir = root / "tmp"
            for path in (home, asset_dir, fake_bin, tmp_dir):
                path.mkdir()

            private_key = root / "private.pem"
            public_key = root / "public.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(private_key)],
                check=True,
                capture_output=True,
                text=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
                text=True,
            )

            version = "1.2.3"
            relay_name = "opencodex-relay_linux_amd64"
            relayctl_name = "opencodex-relayctl_linux_amd64"
            (asset_dir / relay_name).write_text(
                "#!/usr/bin/env bash\n"
                "[[ \"${FAIL_RELAY_CHECK:-0}\" != 1 ]]\n",
                encoding="utf-8",
            )
            (asset_dir / relayctl_name).write_text(
                "#!/usr/bin/env bash\n"
                "set -eu\n"
                "if [[ \"${1:-}\" == init ]]; then\n"
                "  shift\n"
                "  while [[ $# -gt 0 ]]; do\n"
                "    if [[ \"$1\" == --config ]]; then config=\"$2\"; shift 2; else shift; fi\n"
                "  done\n"
                "  mkdir -p \"$(dirname \"$config\")\"\n"
                "  printf '{\"upstream_base_url\":\"https://example.test/v1\",\"catalog\":{\"path\":\"%s\"}}\\n' \"$HOME/.codex/opencodex-relay-catalog.json\" > \"$config\"\n"
                "  chmod 600 \"$config\"\n"
                "fi\n"
                "if [[ \"${1:-}\" == enable ]]; then\n"
                "  shift\n"
                "  while [[ $# -gt 0 ]]; do\n"
                "    if [[ \"$1\" == --codex-config ]]; then codex_config=\"$2\"; shift 2; else shift; fi\n"
                "  done\n"
                "  mkdir -p \"$(dirname \"$codex_config\")\"\n"
                "  printf '%s\\n' '# fake relay routing' > \"$codex_config\"\n"
                "  chmod 600 \"$codex_config\"\n"
                "fi\n",
                encoding="utf-8",
            )
            relayctl_script = (asset_dir / relayctl_name).read_text(encoding="utf-8")
            relayctl_script = relayctl_script.replace(
                "if [[ \"${1:-}\" == enable ]]; then\n",
                "if [[ \"${1:-}\" == mode ]]; then\n"
                "  case \"${2:-}\" in\n"
                "    status) printf '%s\\n' '{\"applied_backend\":\"none\"}' ;;\n"
                "    request) exit 0 ;;\n"
                "    *) exit 64 ;;\n"
                "  esac\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"${1:-}\" == enable ]]; then\n",
            )
            (asset_dir / relayctl_name).write_text(relayctl_script, encoding="utf-8")
            for path in (asset_dir / relay_name, asset_dir / relayctl_name):
                path.chmod(0o755)

            arm_relay_name = "opencodex-relay_linux_arm64"
            arm_relayctl_name = "opencodex-relayctl_linux_arm64"
            app_name = "OpenCodexRelay.app.zip"
            notices_name = "THIRD_PARTY_NOTICES.md"
            for name in (arm_relay_name, arm_relayctl_name, app_name, notices_name):
                (asset_dir / name).write_text(f"fixture {name}\n", encoding="utf-8")

            def digest(name: str) -> str:
                return hashlib.sha256((asset_dir / name).read_bytes()).hexdigest()

            release_base = "https://releases.example.test"
            public_der = subprocess.run(
                ["openssl", "pkey", "-pubin", "-in", str(public_key), "-outform", "DER"],
                check=True,
                capture_output=True,
            ).stdout
            manifest = {
                "version": version,
                "compatibility_revision": 5,
                "artifacts": [
                    {
                        "os": "linux",
                        "arch": "amd64",
                        "component": "relay",
                        "file": relay_name,
                        "url": f"{release_base}/{version}/{relay_name}",
                        "sha256": digest(relay_name),
                    },
                    {
                        "os": "linux",
                        "arch": "amd64",
                        "component": "relayctl",
                        "file": relayctl_name,
                        "url": f"{release_base}/{version}/{relayctl_name}",
                        "sha256": digest(relayctl_name),
                    },
                    {
                        "os": "linux",
                        "arch": "arm64",
                        "component": "relay",
                        "file": arm_relay_name,
                        "url": f"{release_base}/{version}/{arm_relay_name}",
                        "sha256": digest(arm_relay_name),
                    },
                    {
                        "os": "linux",
                        "arch": "arm64",
                        "component": "relayctl",
                        "file": arm_relayctl_name,
                        "url": f"{release_base}/{version}/{arm_relayctl_name}",
                        "sha256": digest(arm_relayctl_name),
                    },
                    {
                        "os": "darwin",
                        "arch": "arm64",
                        "component": "macos_menu_bar_bundle",
                        "file": app_name,
                        "url": f"{release_base}/{version}/{app_name}",
                        "sha256": digest(app_name),
                        "bundle_id": "io.github.novelkr.opencodex-relay",
                        "signing_mode": "adhoc",
                        "minimum_macos_version": "26.0",
                        "integration_protocol": 1,
                        "helper_protocol": 1,
                    },
                ],
                "documents": [
                    {
                        "file": notices_name,
                        "url": f"{release_base}/{version}/{notices_name}",
                        "sha256": digest(notices_name),
                    }
                ],
                "channel": "stable",
                "minimum_updater_version": "0.3.8-rc.6",
                "trust_key_id": hashlib.sha256(public_der).hexdigest(),
            }
            manifest_path = asset_dir / f"manifest-{version}.json"
            manifest_path.write_text(json.dumps(manifest, separators=(",", ":")) + "\n", encoding="utf-8")
            signature_path = asset_dir / f"manifest-{version}.sig"
            signed = subprocess.run(
                ["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(private_key), "-in", str(manifest_path)],
                check=True,
                capture_output=True,
            )
            signature_path.write_text(base64.b64encode(signed.stdout).decode("ascii") + "\n", encoding="utf-8")

            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "output= url=\n"
                "while [[ $# -gt 0 ]]; do\n"
                "  case \"$1\" in\n"
                "    -o) output=\"$2\"; shift 2 ;;\n"
                "    --fail|--location|--silent|--show-error) shift ;;\n"
                "    *) url=\"$1\"; shift ;;\n"
                "  esac\n"
                "done\n"
                "file=\"${url##*/}\"\n"
                "cp \"$FAKE_ASSET_DIR/$file\" \"$output\"\n",
                encoding="utf-8",
            )
            (fake_bin / "uname").write_text(
                "#!/usr/bin/env bash\n"
                "case \"${1:-}\" in\n"
                "  -m) printf 'x86_64\\n' ;;\n"
                "  *) printf 'Linux\\n' ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            (fake_bin / "stat").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ \"${1:-}\" == -c ]]; then printf '%s:600\\n' \"$(id -u)\"; else exec /usr/bin/stat \"$@\"; fi\n",
                encoding="utf-8",
            )
            (fake_bin / "jq").write_text(
                "#!/usr/bin/env bash\n"
                "exec /usr/bin/jq \"$@\"\n",
                encoding="utf-8",
            )
            (fake_bin / "realpath").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ \"${1:-}\" == -m ]]; then shift; fi\n"
                "if [[ \"${1:-}\" == -- ]]; then shift; fi\n"
                "/usr/bin/python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' \"$1\"\n",
                encoding="utf-8",
            )
            (fake_bin / "mv").write_text(
                "#!/usr/bin/env bash\n"
                "set -eu\n"
                "if [[ \"${1:-}\" == -fT ]]; then shift; fi\n"
                "exec /bin/mv \"$@\"\n",
                encoding="utf-8",
            )
            systemctl_log = root / "systemctl.log"
            systemctl_failure = root / "systemctl.failed"
            (fake_bin / "systemctl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "printf '%s\\n' \"$*\" >> \"$FAKE_SYSTEMCTL_LOG\"\n"
                "if [[ \"${1:-}\" == --user ]]; then shift; fi\n"
                "case \"${1:-}\" in\n"
                "  is-active) [[ \"${FAKE_SYSTEMCTL_ACTIVE:-0}\" == 1 ]] ;;\n"
                "  is-enabled)\n"
                "    printf '%s\\n' \"${FAKE_SYSTEMCTL_ENABLED:-not-found}\"\n"
                "    [[ \"${FAKE_SYSTEMCTL_ENABLED:-not-found}\" == enabled || \"${FAKE_SYSTEMCTL_ENABLED:-not-found}\" == enabled-runtime ]]\n"
                "    ;;\n"
                "  start)\n"
                "    if [[ \"${FAIL_SYSTEMCTL_START_ONCE:-0}\" == 1 && ! -e \"$FAKE_SYSTEMCTL_FAILURE\" ]]; then\n"
                "      : > \"$FAKE_SYSTEMCTL_FAILURE\"\n"
                "      exit 87\n"
                "    fi\n"
                "    ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            for path in fake_bin.iterdir():
                path.chmod(0o755)

            environment = os.environ | {
                "HOME": str(home),
                "XDG_CONFIG_HOME": str(home / ".config"),
                "TMPDIR": str(tmp_dir),
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "FAKE_ASSET_DIR": str(asset_dir),
                "FAKE_SYSTEMCTL_LOG": str(systemctl_log),
                "FAKE_SYSTEMCTL_FAILURE": str(systemctl_failure),
                "FAKE_SYSTEMCTL_ACTIVE": "0",
                "FAKE_SYSTEMCTL_ENABLED": "not-found",
                "FAIL_SYSTEMCTL_START_ONCE": "1",
            }
            result = subprocess.run(
                [
                    "bash",
                    str(installer_path),
                    "install",
                    version,
                    "--release-base-url",
                    release_base,
                    "--public-key",
                    str(public_key),
                    "--upstream",
                    "https://example.test/v1",
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("restoring the previous release, service, and routing state", result.stderr)
            relay_root = home / ".local" / "lib" / "opencodex-relay" / "relay"
            self.assertFalse((relay_root / "current").is_symlink())
            self.assertFalse((home / ".config" / "opencodex-relay" / "relay.json").exists())
            self.assertFalse((home / ".codex" / "config.toml").exists())
            self.assertFalse(
                (home / ".config" / "systemd" / "user" / "opencodex-relay.service").exists()
            )
            calls = systemctl_log.read_text(encoding="utf-8").splitlines()
            self.assertIn("--user disable opencodex-relay.service", calls)
            self.assertIn("--user stop opencodex-relay.service", calls)

    def test_relay_uses_the_native_openai_override_without_embedded_credentials(self) -> None:
        config = (RELAY / "internal" / "codexconfig" / "config.go").read_text(encoding="utf-8")
        proxy = (RELAY / "internal" / "proxy" / "server.go").read_text(encoding="utf-8")
        service = (RELAY / "systemd" / "opencodex-relay.service.in").read_text(encoding="utf-8")
        self.assertIn('`openai_base_url = "`', config)
        self.assertIn("model_catalog_json", config)
        self.assertIn('request.Header.Del("X-OpenCodex-API-Key")', proxy)
        self.assertIn('request.Header.Set("X-OpenCodex-API-Key", values.GatewayKey)', proxy)
        self.assertNotIn("CF_ACCESS_CLIENT_SECRET", service)
        self.assertIn("ProtectHome=read-only", service)
        self.assertIn("__CATALOG_PARENT__", service)

    def test_manual_catalog_refresh_reports_the_resident_single_writer(self) -> None:
        relayctl = (RELAY / "cmd" / "opencodex-relayctl" / "main.go").read_text(encoding="utf-8")
        command = relayctl[relayctl.index("func catalogCommand"):relayctl.index("func releaseCommand")]
        self.assertNotIn("ApplyIfIdle", command)
        self.assertIn("catalog_refresh=owned_by_resident", command)

    def test_pilot_runbook_matches_the_public_http_and_websocket_allowlist(self) -> None:
        runbook = (PILOT / "README.md").read_text(encoding="utf-8")
        for route in (
            "/v1/models",
            "/v1/responses",
            "/v1/responses/compact",
            "/v1/images/generations",
            "/v1/images/edits",
            "/v1/opencodex/artifacts/{id}",
            "/v1/alpha/search",
        ):
            self.assertIn(route, runbook)
        self.assertIn("Responses WebSocket", runbook)
        self.assertIn("Voice WebSocket", runbook)

    def test_relay_docs_cover_supported_targets_and_double_opt_in(self) -> None:
        readme = (RELAY / "README.md").read_text(encoding="utf-8")
        doc = (REPO_ROOT / "docs" / "local-codex-relay.md").read_text(encoding="utf-8")
        for target in ("darwin/arm64", "linux/amd64", "linux/arm64"):
            self.assertIn(target, readme)
        self.assertIn("voice_enabled", readme)
        self.assertIn("configure-gateway-features.sh voice", readme)
        self.assertIn("shared device admission", doc)

    def test_canonical_relay_docs_cover_design_lifecycle_and_evidence_boundaries(self) -> None:
        english = (REPO_ROOT / "docs" / "local-codex-relay.md").read_text(encoding="utf-8")
        korean = (REPO_ROOT / "docs" / "local-codex-relay.ko.md").read_text(encoding="utf-8")
        for content in (english, korean):
            self.assertIn("019fd7cc-0f03-76c3-9da1-4d36a7bf85a7", content)
            self.assertIn("manage_app_server=false", content)
            self.assertIn("Voice double opt-in", content)
            self.assertIn("openai_base_url", content)
            self.assertIn("model_catalog_json", content)
            self.assertIn("opencodex-relay-interactive.config.toml", content)
            self.assertIn("listener_lane", content)
            self.assertIn("codex --profile opencodex-relay-interactive", content)
        self.assertIn("sole catalog writer", english)
        self.assertRegex(korean, r"유일한 catalog\s+writer")
        self.assertIn("Design decision and the OpenCodex pattern reused", english)
        self.assertIn("Catalog and AppServer lifecycle", english)
        self.assertIn("Static checks do not prove live availability", english)
        self.assertIn("app_server_home", english)
        self.assertIn("Automatic AppServer restart defaults to", english)
        self.assertIn("설계 결정과 OpenCodex에서 차용한 부분", korean)
        self.assertIn("Catalog lifecycle과 AppServer activation", korean)
        self.assertIn("정적 검증은", korean)
        self.assertIn("app_server_home", korean)
        self.assertIn("자동 AppServer restart opt-in", korean)

    def test_active_docs_do_not_restore_the_legacy_custom_provider_path(self) -> None:
        active_docs = (
            REPO_ROOT / "pilot" / "README.md",
            REPO_ROOT / "docs" / "ssh-and-client-access.md",
            REPO_ROOT / "docs" / "ssh-and-client-access.ko.md",
        )
        for path in active_docs:
            content = path.read_text(encoding="utf-8")
            with self.subTest(path=path):
                self.assertNotIn("[model_providers.pw_opencodex]", content)
                self.assertNotIn("codex --profile opencodex-relay", content)

    def test_repository_indexes_link_the_canonical_relay_docs(self) -> None:
        root = (REPO_ROOT / "README.md").read_text(encoding="utf-8")
        root_ko_path = REPO_ROOT / "README.ko.md"
        root_ko = root_ko_path.read_text(encoding="utf-8") if root_ko_path.exists() else ""
        component = (RELAY / "README.ko.md").read_text(encoding="utf-8")
        self.assertIn("docs/local-codex-relay.ko.md", root)
        if root_ko:
            self.assertIn("docs/local-codex-relay.ko.md", root_ko)
        self.assertNotIn("docs/native-codex-opencodex-adapter.ko.md", root)
        if root_ko:
            self.assertIn("docs/native-codex-opencodex-adapter.ko.md", root_ko)
        self.assertIn("../../docs/local-codex-relay.ko.md", component)

    def test_adapter_dossiers_preserve_deployment_evidence_and_residual_boundaries(self) -> None:
        if not (REPO_ROOT / "docs" / "native-codex-opencodex-adapter.md").exists():
            self.skipTest("private deployment evidence dossier is not exported")
        english = (REPO_ROOT / "docs" / "native-codex-opencodex-adapter.md").read_text(encoding="utf-8")
        korean = (REPO_ROOT / "docs" / "native-codex-opencodex-adapter.ko.md").read_text(encoding="utf-8")
        for content in (english, korean):
            self.assertIn("gpt-5.3-codex-spark", content)
            self.assertIn("gpt-5.6-luna", content)
            self.assertIn("MODE=external", content)
            self.assertIn("MODE=loopback", content)
            self.assertIn("0.147.0", content)
            self.assertIn("43", content)
            self.assertIn("40", content)
            self.assertIn("26", content)
            self.assertIn("visibility", content)
        self.assertIn("not proof that a host remains healthy later", english)
        self.assertRegex(korean, r"정상이라는 증거는\s+아니다")
        self.assertIn("one writer, one activator", english.lower())
        self.assertIn("하나의 writer, 하나의 activator", korean)
        self.assertIn("central Cursor adapter", english)
        self.assertIn("중앙 Cursor adapter", korean)

    def test_new_documentation_relative_links_resolve(self) -> None:
        documents = (
            REPO_ROOT / "README.md",
            REPO_ROOT / "README.ko.md",
            RELAY / "README.md",
            RELAY / "README.ko.md",
            REPO_ROOT / "docs" / "local-codex-relay.md",
            REPO_ROOT / "docs" / "local-codex-relay.ko.md",
            REPO_ROOT / "docs" / "native-codex-opencodex-adapter.md",
            REPO_ROOT / "docs" / "native-codex-opencodex-adapter.ko.md",
            REPO_ROOT / "docs" / "private-github-relay-operations.md",
            REPO_ROOT / "docs" / "private-github-relay-operations.ko.md",
            REPO_ROOT / "docs" / "updates.md",
            REPO_ROOT / "docs" / "updates.ko.md",
        )
        for document in documents:
            if not document.exists():
                continue
            content = document.read_text(encoding="utf-8")
            for raw_target in re.findall(r"\[[^\]]+\]\(([^)]+)\)", content):
                target = raw_target.split("#", 1)[0]
                if not target or "://" in target:
                    continue
                if document.parent == REPO_ROOT and target == "opencodex/":
                    self.assertIn("/opencodex/", (REPO_ROOT / ".gitignore").read_text(encoding="utf-8"))
                    continue
                resolved = (document.parent / target).resolve()
                with self.subTest(document=document, target=target):
                    self.assertTrue(resolved.exists(), f"missing link target: {resolved}")

    def test_private_github_operations_runbook_preserves_live_state_and_repeatability(self) -> None:
        if not (REPO_ROOT / "docs" / "private-github-relay-operations.md").exists():
            self.skipTest("private release operations are not exported")
        english = (REPO_ROOT / "docs" / "private-github-relay-operations.md").read_text(encoding="utf-8")
        korean = (REPO_ROOT / "docs" / "private-github-relay-operations.ko.md").read_text(encoding="utf-8")
        for content in (english, korean):
            self.assertIn("gpt-5.3-codex-spark", content)
            self.assertIn("--with-relay-bootstrap", content)
            self.assertIn("--allow-remote-interruption", content)
            self.assertIn("Contents: read", content)
            self.assertIn("Release record", content)
        self.assertIn("novelKR/opencodex-relay-releases@0.1.0", english)
        self.assertIn("private GitHub Release 게시 | 과거 기록·재배포 필요", korean)
        self.assertIn("REDEPLOY_REQUIRED.md", english)
        self.assertIn("REDEPLOY_REQUIRED.md", korean)
        self.assertIn("bootstrap-keychain-signing-key.sh", english)
        self.assertIn("bootstrap-keychain-signing-key.sh", korean)
        self.assertIn("Keep implementation state separate from live state", english)
        self.assertIn("상태를 먼저 구분한다", korean)
        for content in (english, korean):
            self.assertIn("MODE=external", content)
            self.assertIn("MODE=loopback", content)
        self.assertNotIn("Roll out x86 first", english)
        self.assertNotIn("x86을 먼저 검증", korean)

    def test_installer_validates_credentials_before_rewriting_codex_routing(self) -> None:
        installer = (RELAY / "scripts" / "install-relay.sh").read_text(encoding="utf-8")
        current_switch = 'replace_current_link "$current_candidate" "$current_path"'
        request = 'request_install_routing_state "$relayctl_path" "$config_path" "$codex_config"'
        self.assertLess(
            installer.index('"$relay_path" --config "$config_path" --check'),
            installer.index(request),
        )
        self.assertLess(
            installer.index(request),
            installer.index(current_switch),
        )
        self.assertLess(
            installer.index(current_switch),
            installer.index('"${SCRIPT_DIR}/install-service.sh" install --config "$config_path"'),
        )
        self.assertIn('"${SCRIPT_DIR}/install-service.sh" snapshot --directory', installer)
        self.assertIn('"${SCRIPT_DIR}/install-service.sh" restore-snapshot --directory', installer)
        self.assertIn("restoring the previous release, service, and routing state", installer)
        self.assertIn("snapshot_regular_file \"$codex_config\"", installer)
        self.assertIn("snapshot_regular_file \"$config_path\"", installer)
        self.assertIn("snapshot_regular_file \"$interactive_profile\"", installer)
        self.assertIn("preflight_interactive_profile", installer)
        self.assertIn("legacy_write_interactive_profile", installer)
        self.assertIn('component_manifest_revision "$compatibility_revision"', installer)
        self.assertIn("snapshot_owner_only_control_file \"$routing_state_path\"", installer)
        self.assertIn("snapshot_owner_only_control_file \"$routing_initialized_path\"", installer)
        self.assertIn("snapshot_owner_only_control_file \"$routing_journal_path\"", installer)
        self.assertIn("snapshot_runtime_maintenance_absence \"$runtime_maintenance_path\"", installer)
        self.assertIn("require_runtime_maintenance_absent \"$runtime_maintenance_path\"", installer)
        self.assertIn(
            "verify_runtime_maintenance_absence_snapshot \"$runtime_maintenance\"",
            installer,
        )
        self.assertIn("it was retained for recovery", installer)
        self.assertNotIn(
            "snapshot_owner_only_control_file \"$runtime_maintenance_path\"",
            installer,
        )
        self.assertNotIn(
            "restore_regular_file_snapshot \"$runtime_maintenance\"",
            installer,
        )
        self.assertIn("require_interactive_listener_available", installer)
        self.assertIn("wait_for_dual_listener_health", installer)
        self.assertIn("listener_lane", installer)
        self.assertIn("interactive_reserved_upstream", installer)
        self.assertIn("existing relay config has a different upstream", installer)
        self.assertIn('local_opencodex) applied=local_opencodex', installer)
        self.assertIn("active_local_runtime_is_acknowledged", installer)
        self.assertIn('.applied_backend == "local_opencodex"', installer)
        self.assertIn('.applied_backend == "local_apple_container"', installer)
        self.assertIn('"$interactive_listener" "$runtime_profile"', installer)
        self.assertIn('http://127.0.0.1:10210/v1', installer)
        self.assertIn('mode request "$applied"', installer)
        self.assertIn(".schema_version == 4", installer)
        self.assertLess(
            installer.index("reserve_source_install_lifecycle", installer.index('case "$action" in')),
            installer.index('install -d -m 0700 "$INSTALL_ROOT"'),
        )
        self.assertIn("release_source_install_lifecycle", installer)
        service = (RELAY / "scripts" / "install-service.sh").read_text(encoding="utf-8")
        self.assertIn('[[ -f "$config_path" && ! -L "$config_path" ]]', service)

    def test_installers_preserve_only_a_healthy_acknowledged_apple_runtime(self) -> None:
        production = (RELAY / "scripts" / "install-relay.sh").read_text(encoding="utf-8")
        local_dev = (RELAY / "scripts" / "install-local-dev.sh").read_text(encoding="utf-8")

        production_local_check = production[
            production.index("active_local_runtime_is_acknowledged()") :
            production.index("wait_for_dual_listener_health()")
        ]
        local_dev_check = local_dev[
            local_dev.index("active_local_dev_runtime_is_acknowledged()") :
            local_dev.index("prepare_existing_homebrew_guard_for_replacement()")
        ]
        for check in (production_local_check, local_dev_check):
            self.assertIn('local_apple_container', check)
            self.assertIn('http://127.0.0.1:10210/v1', check)
            self.assertIn('.connection.routing_sync == "acknowledged"', check)
            self.assertIn('.connection.local_opencodex == "ready"', check)
            self.assertIn('.connection.catalog == "running"', check)
            self.assertNotIn('find-generic-password', check)
            self.assertNotIn('apple-container-admin-auth-token', check)
            self.assertNotIn('apple-container-api-auth-token', check)

        production_install = production[production.index('case "$action" in') :]
        self.assertLess(
            production_install.index('wait_for_dual_listener_health "$config_path"'),
            production_install.index('install_transaction_active=false'),
        )
        local_dev_install = local_dev[local_dev.index("install_local_dev()") :]
        self.assertIn(
            'snapshot_regular_file "$apple_catalog_path" "${transaction_dir}/apple-catalog"',
            production_install,
        )
        self.assertIn(
            'restore_regular_file_snapshot "$apple_catalog" "$apple_catalog_snapshot"',
            production,
        )
        self.assertLess(
            production_install.index('snapshot_runtime_maintenance_absence "$runtime_maintenance_path"'),
            production_install.index('snapshot_regular_file "$apple_catalog_path"'),
        )
        self.assertIn(
            'snapshot_file "$apple_runtime_catalog_path" "${transaction_dir}/apple-runtime-catalog"',
            local_dev_install,
        )
        self.assertIn(
            'restore_file "$apple_runtime_catalog_path" "${transaction_dir}/apple-runtime-catalog"',
            local_dev,
        )
        self.assertLess(
            local_dev_install.index("snapshot_runtime_maintenance_absence"),
            local_dev_install.index('snapshot_file "$apple_runtime_catalog_path"'),
        )
        self.assertLess(
            local_dev_install.index("trap 'rollback_install $?' EXIT"),
            local_dev_install.index("wait_for_active_local_dev_runtime_health"),
        )
        self.assertLess(
            local_dev_install.index("wait_for_active_local_dev_runtime_health"),
            local_dev_install.index("install_transaction_active=false"),
        )

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            fake_bin = root / "bin"
            home.mkdir()
            fake_bin.mkdir()
            codex_config = home / ".codex" / "config.toml"
            codex_config.parent.mkdir()
            codex_config.write_text('model = "fixture"\n', encoding="utf-8")

            production_config = root / "production.json"
            production_catalog = home / ".codex" / "opencodex-relay-catalog.json"
            production_local_catalog = home / ".codex" / "opencodex-relay-local-catalog.json"
            production_apple_catalog = home / ".codex" / "opencodex-relay-apple-container-catalog.json"
            production_config.write_text(
                json.dumps(
                    {
                        "installation_scope": "production",
                        "listen_address": "127.0.0.1:18180",
                        "upstream_mode": "external_gateway",
                        "upstream_base_url": "https://example.test/v1",
                        "responses": {"scheduler": {"interactive_listen_address": "127.0.0.1:18182"}},
                        "catalog": {"owner": "relay", "path": str(production_catalog)},
                        "local_opencodex": {
                            "upstream_base_url": "http://127.0.0.1:10100/v1",
                            "catalog_path": str(production_local_catalog),
                        },
                        "local_apple_container": {
                            "upstream_base_url": "http://127.0.0.1:10210/v1",
                            "catalog_path": str(production_apple_catalog),
                        },
                    },
                    separators=(",", ":"),
                )
                + "\n",
                encoding="utf-8",
            )
            local_dev_config = root / "local-dev.json"
            local_dev_catalog = home / ".codex" / "opencodex-relay-dev-external-catalog.json"
            local_dev_local_catalog = home / ".codex" / "opencodex-relay-dev-local-catalog.json"
            local_dev_apple_catalog = home / ".codex" / "opencodex-relay-dev-apple-container-catalog.json"
            local_dev_config.write_text(
                json.dumps(
                    {
                        "installation_scope": "local_development",
                        "listen_address": "127.0.0.1:18190",
                        "upstream_mode": "external_gateway",
                        "upstream_base_url": "https://example.test/v1",
                        "responses": {"scheduler": {"interactive_listen_address": "127.0.0.1:18192"}},
                        "catalog": {"owner": "relay", "path": str(local_dev_catalog)},
                        "local_opencodex": {
                            "upstream_base_url": "http://127.0.0.1:10100/v1",
                            "catalog_path": str(local_dev_local_catalog),
                        },
                        "local_apple_container": {
                            "upstream_base_url": "http://127.0.0.1:10210/v1",
                            "catalog_path": str(local_dev_apple_catalog),
                        },
                    },
                    separators=(",", ":"),
                )
                + "\n",
                encoding="utf-8",
            )
            valid_status = root / "status.json"
            valid_status.write_text(
                json.dumps(
                    {
                        "schema_version": 4,
                        "desired_backend": "local_apple_container",
                        "applied_backend": "local_apple_container",
                        "phase": "relay_active",
                        "relay_admission": "allow",
                        "catalog_refresh": "run",
                        "relay_running": True,
                        "connection": {
                            "local_relay": "healthy",
                            "routing_sync": "acknowledged",
                            "local_opencodex": "ready",
                            "catalog": "running",
                        },
                    },
                    separators=(",", ":"),
                )
                + "\n",
                encoding="utf-8",
            )
            recovery_status = root / "recovery-status.json"
            recovery_payload = json.loads(valid_status.read_text(encoding="utf-8"))
            recovery_payload["phase"] = "recovery_required"
            recovery_payload["relay_admission"] = "deny"
            recovery_status.write_text(json.dumps(recovery_payload) + "\n", encoding="utf-8")
            native_status = root / "native-status.json"
            native_payload = json.loads(valid_status.read_text(encoding="utf-8"))
            native_payload["desired_backend"] = "local_opencodex"
            native_payload["applied_backend"] = "local_opencodex"
            native_status.write_text(json.dumps(native_payload) + "\n", encoding="utf-8")

            production_general = root / "production-general.json"
            production_interactive = root / "production-interactive.json"
            local_dev_general = root / "local-dev-general.json"
            local_dev_interactive = root / "local-dev-interactive.json"
            for path, lane, general, interactive in (
                (production_general, "general", "127.0.0.1:18180", "127.0.0.1:18182"),
                (production_interactive, "interactive", "127.0.0.1:18180", "127.0.0.1:18182"),
                (local_dev_general, "general", "127.0.0.1:18190", "127.0.0.1:18192"),
                (local_dev_interactive, "interactive", "127.0.0.1:18190", "127.0.0.1:18192"),
            ):
                health = relay_scheduler_health(lane, interactive)
                health["general_listener"] = general
                health["upstream_mode"] = "local_apple_container"
                health["upstream_base_url"] = "http://127.0.0.1:10210/v1"
                path.write_text(json.dumps(health) + "\n", encoding="utf-8")
            production_native_general = root / "production-native-general.json"
            production_native_interactive = root / "production-native-interactive.json"
            local_dev_native_general = root / "local-dev-native-general.json"
            local_dev_native_interactive = root / "local-dev-native-interactive.json"
            for path, lane, general, interactive in (
                (production_native_general, "general", "127.0.0.1:18180", "127.0.0.1:18182"),
                (production_native_interactive, "interactive", "127.0.0.1:18180", "127.0.0.1:18182"),
                (local_dev_native_general, "general", "127.0.0.1:18190", "127.0.0.1:18192"),
                (local_dev_native_interactive, "interactive", "127.0.0.1:18190", "127.0.0.1:18192"),
            ):
                health = relay_scheduler_health(lane, interactive)
                health["general_listener"] = general
                health["upstream_mode"] = "local_opencodex"
                health["upstream_base_url"] = "http://127.0.0.1:10100/v1"
                path.write_text(json.dumps(health) + "\n", encoding="utf-8")

            relayctl_log = root / "relayctl.log"
            relayctl = fake_bin / "relayctl"
            relayctl.write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "printf '%s\\n' \"$*\" >> \"$FAKE_RELAYCTL_LOG\"\n"
                "[[ \"${1:-}\" == mode && \"${2:-}\" == status ]] || exit 64\n"
                "cat \"$FAKE_STATUS\"\n",
                encoding="utf-8",
            )
            relayctl.chmod(0o700)
            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "url=\n"
                "for arg in \"$@\"; do [[ \"$arg\" != http://* ]] || url=\"$arg\"; done\n"
                "case \"$url\" in\n"
                "  http://127.0.0.1:18180/__relay/healthz) cat \"$FAKE_PRODUCTION_GENERAL\" ;;\n"
                "  http://127.0.0.1:18182/__relay/healthz) cat \"$FAKE_PRODUCTION_INTERACTIVE\" ;;\n"
                "  http://127.0.0.1:18190/__relay/healthz) cat \"$FAKE_LOCAL_DEV_GENERAL\" ;;\n"
                "  http://127.0.0.1:18192/__relay/healthz) cat \"$FAKE_LOCAL_DEV_INTERACTIVE\" ;;\n"
                "  *) exit 65 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            (fake_bin / "curl").chmod(0o700)

            production_prelude = root / "production-prelude.sh"
            production_prelude.write_text(
                production[: production.index('\naction="${1:-}"')], encoding="utf-8"
            )
            local_dev_prelude = root / "local-dev-prelude.sh"
            local_dev_prelude.write_text(
                local_dev[: local_dev.index('\naction="${1:-}"')], encoding="utf-8"
            )
            environment = os.environ | {
                "HOME": str(home),
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "FAKE_RELAYCTL_LOG": str(relayctl_log),
                "FAKE_STATUS": str(valid_status),
                "FAKE_PRODUCTION_GENERAL": str(production_general),
                "FAKE_PRODUCTION_INTERACTIVE": str(production_interactive),
                "FAKE_LOCAL_DEV_GENERAL": str(local_dev_general),
                "FAKE_LOCAL_DEV_INTERACTIVE": str(local_dev_interactive),
            }
            production_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; [[ "$(active_local_runtime_is_acknowledged "$2" "$3" "$4")" == local_apple_container ]]; verify_dual_listener_health_once "$2" "$3" "$4"',
                    "installer-test",
                    str(production_prelude),
                    str(production_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(production_result.returncode, 0, production_result.stderr)
            local_dev_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; [[ "$(active_local_dev_runtime_is_acknowledged "$2" "$3" "$4")" == local_apple_container ]]; verify_active_local_dev_runtime_health_once "$2" "$3" "$4" local_apple_container',
                    "installer-test",
                    str(local_dev_prelude),
                    str(local_dev_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(local_dev_result.returncode, 0, local_dev_result.stderr)

            native_environment = environment | {
                "FAKE_STATUS": str(native_status),
                "FAKE_PRODUCTION_GENERAL": str(production_native_general),
                "FAKE_PRODUCTION_INTERACTIVE": str(production_native_interactive),
                "FAKE_LOCAL_DEV_GENERAL": str(local_dev_native_general),
                "FAKE_LOCAL_DEV_INTERACTIVE": str(local_dev_native_interactive),
            }
            production_native_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; [[ "$(active_local_runtime_is_acknowledged "$2" "$3" "$4")" == local_opencodex ]]; verify_dual_listener_health_once "$2" "$3" "$4"',
                    "installer-test",
                    str(production_prelude),
                    str(production_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=native_environment,
            )
            self.assertEqual(production_native_result.returncode, 0, production_native_result.stderr)
            local_dev_native_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; [[ "$(active_local_dev_runtime_is_acknowledged "$2" "$3" "$4")" == local_opencodex ]]; verify_active_local_dev_runtime_health_once "$2" "$3" "$4" local_opencodex',
                    "installer-test",
                    str(local_dev_prelude),
                    str(local_dev_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=native_environment,
            )
            self.assertEqual(local_dev_native_result.returncode, 0, local_dev_native_result.stderr)
            calls = relayctl_log.read_text(encoding="utf-8").splitlines()
            self.assertTrue(calls)
            for call in calls:
                self.assertEqual(
                    call,
                    f"mode status --config {production_config if str(production_config) in call else local_dev_config} --codex-config {codex_config} --json",
                )
                self.assertNotIn("token", call.lower())

            recovered = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; active_local_runtime_is_acknowledged "$2" "$3" "$4"',
                    "installer-test",
                    str(production_prelude),
                    str(production_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAKE_STATUS": str(recovery_status)},
            )
            self.assertNotEqual(recovered.returncode, 0)

            mismatched_health = root / "mismatched-apple-health.json"
            mismatched_payload = json.loads(production_general.read_text(encoding="utf-8"))
            mismatched_payload["upstream_base_url"] = "http://127.0.0.1:10100/v1"
            mismatched_health.write_text(json.dumps(mismatched_payload) + "\n", encoding="utf-8")
            mismatched = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; verify_dual_listener_health_once "$2" "$3" "$4"',
                    "installer-test",
                    str(production_prelude),
                    str(production_config),
                    str(relayctl),
                    str(codex_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAKE_PRODUCTION_GENERAL": str(mismatched_health)},
            )
            self.assertNotEqual(mismatched.returncode, 0)

            maintenance = Path(f"{production_config}.runtime-maintenance.json")
            maintenance.write_text("malformed\n", encoding="utf-8")
            maintenance_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; require_runtime_maintenance_absent "$2"',
                    "installer-test",
                    str(production_prelude),
                    str(maintenance),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(maintenance_result.returncode, 0)
            self.assertTrue(maintenance.is_file())

            local_dev_maintenance = Path(f"{local_dev_config}.runtime-maintenance.json")
            local_dev_maintenance.write_text("{}\n", encoding="utf-8")
            local_dev_maintenance_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; require_local_dev_config_leaves_or_absent "$2"',
                    "installer-test",
                    str(local_dev_prelude),
                    str(local_dev_config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(local_dev_maintenance_result.returncode, 0)
            self.assertTrue(local_dev_maintenance.is_file())

            # Exercise the same EXIT rollback used after a candidate service
            # or health failure. Both Local catalog generations and their
            # restart markers must return to the captured bytes before the old
            # service could be reactivated.
            rollback_transaction = root / "rollback-transaction"
            rollback_transaction.mkdir()
            rollback_config = root / "rollback-config.json"
            rollback_config.write_text('{"fixture":true}\n', encoding="utf-8")
            rollback_local_catalog = root / "opencodex-relay-dev-local-catalog.json"
            rollback_local_pending = Path(f"{rollback_local_catalog}.restart-pending")
            rollback_apple_catalog = root / "opencodex-relay-dev-apple-container-catalog.json"
            rollback_apple_pending = Path(f"{rollback_apple_catalog}.restart-pending")
            for path, content in (
                (rollback_local_catalog, "old local catalog\n"),
                (rollback_local_pending, "old local pending\n"),
                (rollback_apple_catalog, "old apple catalog\n"),
                (rollback_apple_pending, "old apple pending\n"),
            ):
                path.write_text(content, encoding="utf-8")

            def snapshot_fixture(path: Path, name: str) -> None:
                prefix = rollback_transaction / name
                if path.is_file():
                    Path(f"{prefix}.data").write_bytes(path.read_bytes())
                    Path(f"{prefix}.state").write_text("present=true\n", encoding="utf-8")
                else:
                    Path(f"{prefix}.state").write_text("present=false\n", encoding="utf-8")

            snapshot_fixture(rollback_config, "config")
            for absent_path, name in (
                (Path(f"{rollback_config}.routing-state.json"), "routing-state"),
                (Path(f"{rollback_config}.routing-initialized"), "routing-initialized"),
                (Path(f"{rollback_config}.routing-transaction.json"), "routing-journal"),
                (home / "Library" / "Application Support" / "OpenCodexRelayDev" / "routing-binding.json", "binding"),
                (home / "Library" / "LaunchAgents" / "io.github.novelkr.opencodex-relay.dev.plist", "service"),
            ):
                snapshot_fixture(absent_path, name)
            Path(f"{rollback_transaction / 'runtime-maintenance'}.state").write_text(
                "present=false\n", encoding="utf-8"
            )
            snapshot_fixture(rollback_local_catalog, "local-runtime-catalog")
            snapshot_fixture(rollback_local_pending, "local-runtime-catalog-pending")
            snapshot_fixture(rollback_apple_catalog, "apple-runtime-catalog")
            snapshot_fixture(rollback_apple_pending, "apple-runtime-catalog-pending")
            Path(f"{rollback_transaction / 'app-link'}.state").write_text(
                "present=false\n", encoding="utf-8"
            )
            Path(f"{rollback_transaction / 'current'}.state").write_text(
                "present=false\n", encoding="utf-8"
            )
            rollback_local_catalog.write_text("candidate local catalog\n", encoding="utf-8")
            rollback_local_pending.write_text("candidate local pending\n", encoding="utf-8")
            rollback_apple_catalog.write_text("candidate apple catalog\n", encoding="utf-8")
            rollback_apple_pending.write_text("candidate apple pending\n", encoding="utf-8")
            fake_local_dev_service = root / "install-local-dev-service.sh"
            fake_local_dev_service.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            fake_local_dev_service.chmod(0o700)
            rollback_workspace = root / "rollback-workspace"
            rollback_staging = root / "rollback-staging"
            rollback_workspace.mkdir()
            rollback_staging.mkdir()
            rollback_result = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; config_path="$2"; transaction_dir="$3"; '
                    'local_runtime_catalog_path="$4"; local_runtime_catalog_pending_path="$5"; '
                    'apple_runtime_catalog_path="$6"; apple_runtime_catalog_pending_path="$7"; '
                    'tmp="$8"; staging_dir="$9"; install_transaction_active=true; '
                    'source_install_reservation_active=false; install_dir_created=false; '
                    'guard_restore_helper=""; rollback_install 86',
                    "installer-test",
                    str(local_dev_prelude),
                    str(rollback_config),
                    str(rollback_transaction),
                    str(rollback_local_catalog),
                    str(rollback_local_pending),
                    str(rollback_apple_catalog),
                    str(rollback_apple_pending),
                    str(rollback_workspace),
                    str(rollback_staging),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(rollback_result.returncode, 86, rollback_result.stderr)
            self.assertEqual(rollback_local_catalog.read_text(encoding="utf-8"), "old local catalog\n")
            self.assertEqual(rollback_local_pending.read_text(encoding="utf-8"), "old local pending\n")
            self.assertEqual(rollback_apple_catalog.read_text(encoding="utf-8"), "old apple catalog\n")
            self.assertEqual(rollback_apple_pending.read_text(encoding="utf-8"), "old apple pending\n")
            self.assertFalse(rollback_transaction.exists())

    def test_public_github_release_path_is_explicit_and_fail_closed(self) -> None:
        installer_path = RELAY / "scripts" / "install-relay.sh"
        publisher_path = RELAY / "scripts" / "publish-github-release.sh"
        builder_path = RELAY / "scripts" / "build-release.sh"
        installer = installer_path.read_text(encoding="utf-8")
        publisher = publisher_path.read_text(encoding="utf-8")
        builder = builder_path.read_text(encoding="utf-8")
        service = (RELAY / "systemd" / "opencodex-relay.service.in").read_text(encoding="utf-8")

        self.assertTrue(os.access(publisher_path, os.X_OK))
        self.assertIn("--github-repo OWNER/REPO [--github-token-file PATH]", installer)
        self.assertIn("--release-base-url and --github-repo are mutually exclusive", installer)
        self.assertIn("Public GitHub Releases require no token", installer)
        self.assertIn("GitHub token file must be owned by the current user with mode 0600", installer)
        self.assertIn("curl --config \"$github_curl_config\"", installer)
        self.assertIn("jq is required for --github-repo installs", installer)
        self.assertIn("GitHub release is not immutable", installer)
        self.assertNotIn("gh release download", installer)
        self.assertNotIn("GH_TOKEN=", installer)
        self.assertIn("pw_opencodex_remote", (RELAY / "internal" / "codexconfig" / "config.go").read_text(encoding="utf-8"))
        self.assertNotIn("GITHUB_TOKEN", service)
        self.assertIn("--github-repo OWNER/REPO", builder)
        self.assertIn("github_release_repo", builder)
        self.assertIn("--previous-build-number NUMERIC_BUILD", builder)
        self.assertIn("RELEASE_BUILD_NUMBER", builder)
        self.assertIn("Contents/Resources/ReleaseTrust", builder)
        self.assertIn("bundled release public key bytes differ", builder)
        self.assertIn("bundled release public key fingerprint differs", builder)
        self.assertIn("Contents/Resources/RuntimeTrust", builder)
        self.assertIn("bundled runtime public key bytes differ", builder)
        self.assertIn("bundled runtime public key fingerprint differs", builder)
        self.assertIn(
            "opencodex-runtime-release-ed25519.pub",
            installer,
        )
        self.assertIn("--signing-key-keychain-service SERVICE", builder)
        self.assertIn('swift "$KEYCHAIN_HELPER" read', builder)
        keychain_helper = RELAY / "scripts" / "keychain-signing-key.swift"
        keychain_bootstrap = RELAY / "scripts" / "bootstrap-keychain-signing-key.sh"
        self.assertTrue(keychain_helper.is_file())
        self.assertTrue(os.access(keychain_bootstrap, os.X_OK))
        self.assertIn("SecItemCopyMatching", keychain_helper.read_text(encoding="utf-8"))
        self.assertIn("SecItemAdd", keychain_helper.read_text(encoding="utf-8"))
        self.assertIn("--public-key-out PEM", keychain_bootstrap.read_text(encoding="utf-8"))
        self.assertIn("--signing-key and --signing-key-keychain-service are mutually exclusive", builder)
        self.assertIn('init_args+=(--manage-app-server="${manage_app_server}")', installer)
        self.assertIn('manage_app_server="false"', installer)
        self.assertIn('--app-server-home must be an absolute path', installer)
        self.assertNotIn('--manage-app-server "$manage_app_server"', installer)
        self.assertIn("GitHub release repository must be public", publisher)
        self.assertIn('gh api "repos/${repo}" --jq .visibility', publisher)
        self.assertNotIn("gh api user", publisher)
        self.assertIn("--draft=false", publisher)
        self.assertIn("isImmutable", publisher)
        self.assertIn("isPrerelease", publisher)
        self.assertIn("release_create_flags+=(--prerelease --latest=false)", publisher)
        self.assertIn("release_create_flags+=(--latest)", publisher)
        self.assertIn("verify_release_asset_digests", publisher)
        self.assertIn("--json tagName,isDraft,isPrerelease,isImmutable", publisher)
        self.assertIn('"repos/${repo}/releases/latest" --jq .tag_name', publisher)
        self.assertIn("--release-notes-fragment FILE", publisher)
        self.assertIn("--json body", publisher)
        self.assertIn("GitHub release body differs from the reviewed release notes", publisher)
        self.assertIn('gh release delete "$version"', publisher)
        self.assertIn('release_files=("$manifest" "$signature" "$notices")', publisher)
        self.assertIn('--notes-file "$release_notes"', publisher)
        self.assertNotIn('--notes "Manifest-signed', publisher)
        for release_notes_heading in (
            "## Choose your download",
            "## Install or update",
            "## Security and verification",
            "## macOS first launch",
            "## Source",
        ):
            self.assertIn(release_notes_heading, publisher)
        self.assertIn("gh attestation verify FILE", publisher)
        self.assertIn("Open Anyway", publisher)
        self.assertIn("`1755` directories carrying only the sticky bit", publisher)
        self.assertIn("setuid/setgid", publisher)
        self.assertLess(publisher.index("verify_release_assets true"), publisher.index("--draft=false"))
        self.assertLess(publisher.index("verify_release_body"), publisher.index("--draft=false"))
        self.assertIn('verify_release_assets false', publisher)
        self.assertIn('"compatibility_revision":5', builder)
        self.assertIn(r'\"minimum_macos_version\":\"${MINIMUM_MACOS_VERSION}\"', builder)
        self.assertIn('"minimum_updater_version":"%s"', builder)
        self.assertIn('"trust_key_id":"%s"', builder)
        self.assertIn(r'\"signing_mode\":\"adhoc\"', builder)
        self.assertNotIn("--apple-signing-identity", builder)
        self.assertNotIn("--apple-team-id", builder)
        self.assertNotIn("--notary-keychain-profile", builder)
        resources = RELAY / "macos" / "OpenCodexRelay" / "Resources"
        self.assertFalse(
            (resources / "io.github.novelkr.opencodex-relay.homebrew-guard.plist").exists()
        )
        self.assertFalse(
            (resources / "io.github.novelkr.opencodex-relay.homebrew-guard.dev.plist").exists()
        )
        self.assertNotRegex(builder, r"(?m)^\s*(?:xcrun\s+)?(?:notarytool|stapler)\b")
        self.assertIn('"documents":[{"file":"%s"', builder)
        self.assertIn('[[ "$1" == 4 || "$1" == 5 ]]', installer)
        self.assertIn("release manifest is not canonical JSON or contains duplicate fields", installer)
        self.assertIn("revision 5 manifest channel does not match the release version", installer)
        self.assertIn("revision 5 trust key ID does not match the provided public key", installer)
        self.assertIn("revision 5 minimum macOS version is invalid", installer)
        self.assertIn("require_ed25519_private_key", builder)
        self.assertIn("require_ed25519_public_key", publisher)
        self.assertIn('THIRD_PARTY_NOTICES.md SHA-256 does not match manifest', installer)
        self.assertIn("gatekeeper_approval=manual", installer)
        self.assertIn("Privacy_&_Security_Open_Anyway", installer)
        self.assertNotRegex(installer, r"(?m)^\s*(?:xcrun\s+)?(?:notarytool|stapler)\b")
        self.assertNotIn("spctl --master-disable", installer)
        self.assertNotIn("xattr -d", installer)
        # The installer must finish after selecting the verified app. Finder's
        # first launch and Gatekeeper approval are deliberately user-owned, so
        # a blocked launch cannot enter the installation rollback path.
        self.assertNotIn("--install-login", installer)
        self.assertNotRegex(installer, r"(?m)^\s*(?:/usr/bin/)?open\b")

        english_install = (REPO_ROOT / "docs" / "local-codex-relay.md").read_text(
            encoding="utf-8"
        )
        korean_install = (REPO_ROOT / "docs" / "local-codex-relay.ko.md").read_text(
            encoding="utf-8"
        )
        self.assertIn("Open Anyway", english_install)
        self.assertIn("about one hour", english_install)
        self.assertIn("약 1시간", korean_install)
        self.assertIn("별도 승인", korean_install)

        conflict = subprocess.run(
            [
                "bash",
                str(installer_path),
                "install",
                "1.2.3",
                "--release-base-url",
                "https://example.test/releases",
                "--github-repo",
                "owner/repo",
                "--github-token-file",
                "/tmp/token",
                "--public-key",
                "/tmp/public.pem",
                "--upstream",
                "https://example.test/v1",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(conflict.returncode, 0)
        self.assertIn("mutually exclusive", conflict.stderr)

        missing_appserver_home = subprocess.run(
            [
                "bash",
                str(installer_path),
                "install",
                "1.2.3",
                "--release-base-url",
                "https://example.test/releases",
                "--public-key",
                "/tmp/public.pem",
                "--upstream",
                "https://example.test/v1",
                "--manage-app-server",
                "true",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(missing_appserver_home.returncode, 0)
        self.assertIn("--app-server-home", missing_appserver_home.stderr)

        with tempfile.TemporaryDirectory() as directory:
            token = Path(directory) / "github-release.token"
            token.write_text("github_pat_example", encoding="utf-8")
            token.chmod(0o644)
            unsafe_token = subprocess.run(
                [
                    "bash",
                    str(installer_path),
                    "install",
                    "1.2.3",
                    "--github-repo",
                    "owner/repo",
                    "--github-token-file",
                    str(token),
                    "--public-key",
                    "/tmp/public.pem",
                    "--upstream",
                    "https://example.test/v1",
                ],
                check=False,
                capture_output=True,
                text=True,
            )
        self.assertNotEqual(unsafe_token.returncode, 0)
        self.assertIn("mode 0600", unsafe_token.stderr)

    def test_standalone_installer_strictly_accepts_revision_five_before_download(self) -> None:
        installer_path = RELAY / "scripts" / "install-relay.sh"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            assets = root / "assets"
            fake_bin = root / "bin"
            tmp_dir = root / "tmp"
            for path in (home, assets, fake_bin, tmp_dir):
                path.mkdir()

            private_key = root / "private.pem"
            public_key = root / "public.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(private_key)],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
            )
            public_der = subprocess.run(
                ["openssl", "pkey", "-pubin", "-in", str(public_key), "-outform", "DER"],
                check=True,
                capture_output=True,
            ).stdout
            trust_key_id = hashlib.sha256(public_der).hexdigest()
            version = "1.2.3-rc.1"
            base_url = "https://releases.example.test"
            manifest_path = assets / f"manifest-{version}.json"
            signature_path = assets / f"manifest-{version}.sig"
            artifact_attempt = root / "artifact-attempted"

            def linux_artifact(arch: str, component: str) -> dict[str, object]:
                command = "opencodex-relayctl" if component == "relayctl" else "opencodex-relay"
                file = f"{command}_linux_{arch}"
                return {
                    "os": "linux",
                    "arch": arch,
                    "component": component,
                    "file": file,
                    "url": f"{base_url}/{version}/{file}",
                    "sha256": "0" * 64,
                }

            valid_manifest: dict[str, object] = {
                "version": version,
                "compatibility_revision": 5,
                "artifacts": [
                    linux_artifact("amd64", "relay"),
                    linux_artifact("amd64", "relayctl"),
                    linux_artifact("arm64", "relay"),
                    linux_artifact("arm64", "relayctl"),
                    {
                        "os": "darwin",
                        "arch": "arm64",
                        "component": "macos_menu_bar_bundle",
                        "file": "OpenCodexRelay.app.zip",
                        "url": f"{base_url}/{version}/OpenCodexRelay.app.zip",
                        "sha256": "1" * 64,
                        "bundle_id": "io.github.novelkr.opencodex-relay",
                        "signing_mode": "adhoc",
                        "minimum_macos_version": "26.0",
                        "integration_protocol": 1,
                        "helper_protocol": 1,
                    },
                ],
                "documents": [
                    {
                        "file": "THIRD_PARTY_NOTICES.md",
                        "url": f"{base_url}/{version}/THIRD_PARTY_NOTICES.md",
                        "sha256": "2" * 64,
                    }
                ],
                "channel": "preview",
                "minimum_updater_version": "0.3.8-rc.6",
                "trust_key_id": trust_key_id,
            }

            (fake_bin / "uname").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ \"${1:-}\" == -m ]]; then printf 'x86_64\\n'; else printf 'Linux\\n'; fi\n",
                encoding="utf-8",
            )
            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "output= url=\n"
                "while [[ $# -gt 0 ]]; do\n"
                "  case \"$1\" in\n"
                "    -o) output=\"$2\"; shift 2 ;;\n"
                "    --fail|--location|--silent|--show-error) shift ;;\n"
                "    *) url=\"$1\"; shift ;;\n"
                "  esac\n"
                "done\n"
                "case \"${url##*/}\" in\n"
                f"  manifest-{version}.json|manifest-{version}.sig) cp \"$FAKE_ASSET_DIR/${{url##*/}}\" \"$output\" ;;\n"
                "  *) : > \"$FAKE_ARTIFACT_ATTEMPT\"; exit 44 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            for path in fake_bin.iterdir():
                path.chmod(0o755)

            environment = os.environ | {
                "HOME": str(home),
                "XDG_CONFIG_HOME": str(home / ".config"),
                "TMPDIR": str(tmp_dir),
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "FAKE_ASSET_DIR": str(assets),
                "FAKE_ARTIFACT_ATTEMPT": str(artifact_attempt),
            }
            command = [
                "bash",
                str(installer_path),
                "install",
                version,
                "--release-base-url",
                base_url,
                "--public-key",
                str(public_key),
                "--upstream",
                "https://example.test/v1",
            ]

            def run_manifest(
                payload: dict[str, object], *, raw: str | None = None
            ) -> subprocess.CompletedProcess[str]:
                artifact_attempt.unlink(missing_ok=True)
                manifest_path.write_text(
                    raw if raw is not None else json.dumps(payload, separators=(",", ":")) + "\n",
                    encoding="utf-8",
                )
                signed = subprocess.run(
                    [
                        "openssl",
                        "pkeyutl",
                        "-sign",
                        "-rawin",
                        "-inkey",
                        str(private_key),
                        "-in",
                        str(manifest_path),
                    ],
                    check=True,
                    capture_output=True,
                ).stdout
                signature_path.write_text(
                    base64.b64encode(signed).decode("ascii") + "\n", encoding="ascii"
                )
                return subprocess.run(
                    command,
                    check=False,
                    capture_output=True,
                    text=True,
                    env=environment,
                )

            accepted = run_manifest(valid_manifest)
            self.assertEqual(accepted.returncode, 44, accepted.stderr)
            self.assertTrue(artifact_attempt.is_file())

            revision_four = json.loads(json.dumps(valid_manifest))
            revision_four["compatibility_revision"] = 4
            revision_four.pop("channel")
            revision_four.pop("minimum_updater_version")
            revision_four.pop("trust_key_id")
            for field in ("minimum_macos_version", "integration_protocol", "helper_protocol"):
                revision_four["artifacts"][4].pop(field)
            accepted_revision_four = run_manifest(revision_four)
            self.assertEqual(accepted_revision_four.returncode, 44, accepted_revision_four.stderr)
            self.assertTrue(artifact_attempt.is_file())

            invalid_cases: tuple[tuple[dict[str, object], str], ...] = (
                (valid_manifest | {"unknown": True}, "unknown or malformed top-level fields"),
                (valid_manifest | {"channel": "stable"}, "channel does not match"),
                (
                    valid_manifest | {"minimum_updater_version": "0.3.8-06"},
                    "minimum updater version is not strict SemVer",
                ),
                (valid_manifest | {"trust_key_id": "f" * 64}, "trust key ID does not match"),
            )
            for payload, message in invalid_cases:
                rejected = run_manifest(payload)
                self.assertNotEqual(rejected.returncode, 0)
                self.assertIn(message, rejected.stderr)
                self.assertFalse(artifact_attempt.exists())

            invalid_artifacts = json.loads(json.dumps(valid_manifest))
            invalid_artifacts["artifacts"][4]["helper_protocol"] = 2
            rejected_protocol = run_manifest(invalid_artifacts)
            self.assertIn("artifacts contain unknown, incomplete, or unsupported fields", rejected_protocol.stderr)
            self.assertFalse(artifact_attempt.exists())

            compact = json.dumps(valid_manifest, separators=(",", ":"))
            duplicate = compact.replace('"channel":"preview"', '"channel":"preview","channel":"preview"') + "\n"
            rejected_duplicate = run_manifest(valid_manifest, raw=duplicate)
            self.assertIn("duplicate fields", rejected_duplicate.stderr)
            self.assertFalse(artifact_attempt.exists())

            for invalid_version in ("01.2.3", "1.2.3-01", "1.2.3+build", "1.2.3-alpha..1"):
                invalid_version_result = subprocess.run(
                    ["bash", str(installer_path), "install", invalid_version],
                    check=False,
                    capture_output=True,
                    text=True,
                )
                self.assertNotEqual(invalid_version_result.returncode, 0)
                self.assertIn("strict SemVer", invalid_version_result.stderr)

    def test_release_builder_signs_full_third_party_notice_in_exact_asset_set(self) -> None:
        notice_path = RELAY / "THIRD_PARTY_NOTICES.md"
        notice = notice_path.read_text(encoding="utf-8")
        embedded_license = notice.split("```text\n", 1)[1].split("\n```\n", 1)[0] + "\n"
        self.assertEqual(
            hashlib.sha256(embedded_license.encode("utf-8")).hexdigest(),
            "0d9e582ee4bff57bf1189c9e514e6da7ce277f9cd3bc2d488b22fbb39a6d87cf",
        )
        self.assertIn("Files: internal/snapref/*", embedded_license)
        self.assertIn("THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS", embedded_license)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake_bin = root / "bin"
            output = root / "release"
            fake_bin.mkdir()
            fake_go = fake_bin / "go"
            fake_go.write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "output=\n"
                "while [[ $# -gt 0 ]]; do\n"
                "  if [[ $1 == -o ]]; then output=$2; shift 2; else shift; fi\n"
                "done\n"
                "[[ -n $output ]]\n"
                "printf '#!/bin/sh\\nexit 0\\n' > \"$output\"\n",
                encoding="utf-8",
            )
            fake_go.chmod(0o755)
            fake_swift_bin = root / "swift-bin"
            fake_swift = fake_bin / "swift"
            fake_swift.write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "mkdir -p \"$FAKE_SWIFT_BIN\"\n"
                "mkdir -p \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/en.lproj\" \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/ko.lproj\"\n"
                "printf '#!/usr/bin/env bash\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay\"\n"
                "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelay\"\n"
                "printf '#!/usr/bin/env bash\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelayPrivilegedHelper\"\n"
                "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelayPrivilegedHelper\"\n"
                "printf '#!/usr/bin/env bash\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelayHelperInstaller\"\n"
                "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelayHelperInstaller\"\n"
                "printf '\\\"language.label\\\" = \\\"Language\\\";\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/en.lproj/Localizable.strings\"\n"
                "printf '\\\"language.label\\\" = \\\"언어\\\";\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/ko.lproj/Localizable.strings\"\n"
                "if [[ \" $* \" == *\" --show-bin-path \"* ]]; then printf '%s\\n' \"$FAKE_SWIFT_BIN\"; fi\n",
                encoding="utf-8",
            )
            fake_swift.chmod(0o755)
            for name, body in {
                "codesign": (
                    "#!/usr/bin/env bash\n"
                    "target=${!#}\n"
                    "if [[ \" $* \" == *\" -d\"* ]]; then\n"
                    "  printf 'CDHash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\nSignature=adhoc\\nTeamIdentifier=not set\\nCodeDirectory v=20500 size=1 flags=0x10002(adhoc,runtime) hashes=1+0 location=embedded\\n' >&2\n"
                    "  if [[ $target == *OpenCodexRelayHelperInstaller ]]; then\n"
                    "    printf 'Identifier=io.github.novelkr.opencodex-relay.homebrew-guard.installer\\n' >&2\n"
                    "  else\n"
                    "    printf 'Identifier=io.github.novelkr.opencodex-relay.homebrew-guard.helper\\n' >&2\n"
                    "  fi\n"
                    "fi\n"
                    "exit 0\n"
                ),
                "plutil": "#!/usr/bin/env bash\nexit 0\n",
                "ditto": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "if [[ \"${1:-}\" == \"-c\" ]]; then\n"
                    "  while [[ $# -gt 2 ]]; do shift; done\n"
                    "  source=\"$1\"\n"
                    "  destination=\"$2\"\n"
                    "  (cd \"$(dirname \"$source\")\" && find \"$(basename \"$source\")\" -print | LC_ALL=C sort) > \"$destination\"\n"
                    "else\n"
                    "  source=\"$1\"\n"
                    "  destination=\"$2\"\n"
                    "  cp -R \"$source\" \"$destination\"\n"
                    "fi\n"
                ),
                "uname": "#!/usr/bin/env bash\nprintf 'Darwin\\n'\n",
            }.items():
                tool = fake_bin / name
                tool.write_text(body, encoding="utf-8")
                tool.chmod(0o755)
            private_key = root / "private.pem"
            public_key = root / "public.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(private_key)],
                check=True,
                capture_output=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
            )
            release_build_number = (
                RELAY / "RELEASE_BUILD_NUMBER"
            ).read_text(encoding="ascii").strip()
            self.assertRegex(release_build_number, r"^[1-9][0-9]{0,3}$")
            self.assertLessEqual(int(release_build_number), 9999)
            validator = RELAY / "scripts" / "validate-release-build-number.py"
            for invalid_value in (b"0\n", b"10000\n", b"1\n2\n", b"build\n"):
                invalid_source = root / "invalid-build-number"
                invalid_source.write_bytes(invalid_value)
                invalid_build_number = subprocess.run(
                    ["python3", str(validator), str(invalid_source), "0.3.8"],
                    check=False,
                    capture_output=True,
                    text=True,
                )
                self.assertNotEqual(invalid_build_number.returncode, 0)
                self.assertIn("one integer from 1 through 9999", invalid_build_number.stderr)
            for invalid_previous in ("0", "10000", "0.3.8", "0100", "build"):
                invalid_previous_build = subprocess.run(
                    [
                        "python3",
                        str(validator),
                        str(RELAY / "RELEASE_BUILD_NUMBER"),
                        invalid_previous,
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                )
                self.assertNotEqual(invalid_previous_build.returncode, 0)
                self.assertIn(
                    "previous public CFBundleVersion is invalid",
                    invalid_previous_build.stderr,
                )
            build_prefix = [
                "bash",
                str(RELAY / "scripts" / "build-release.sh"),
                "1.2.3",
                "--github-repo",
                "owner/private-release",
                "--signing-key",
                str(private_key),
            ]
            missing_previous = subprocess.run(
                [*build_prefix, "--output", str(output)],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(missing_previous.returncode, 0)
            self.assertIn("--previous-build-number is required", missing_previous.stderr)
            non_increasing = subprocess.run(
                [
                    *build_prefix,
                    "--previous-build-number",
                    release_build_number,
                    "--output",
                    str(output),
                ],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(non_increasing.returncode, 0)
            self.assertIn("must be greater", non_increasing.stderr)
            result = subprocess.run(
                [
                    *build_prefix,
                    "--previous-build-number",
                    "1000",
                    "--output",
                    str(output),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "FAKE_SWIFT_BIN": str(fake_swift_bin),
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            expected_assets = {
                "THIRD_PARTY_NOTICES.md",
                "manifest-1.2.3.json",
                "manifest-1.2.3.sig",
                "opencodex-relay_linux_amd64",
                "opencodex-relay_linux_arm64",
                "opencodex-relayctl_linux_amd64",
                "opencodex-relayctl_linux_arm64",
                "OpenCodexRelay.app.zip",
            }
            self.assertEqual({path.name for path in output.iterdir()}, expected_assets)
            archive_entries = (output / "OpenCodexRelay.app.zip").read_text(
                encoding="utf-8"
            ).splitlines()
            bundle_root = (
                "OpenCodexRelay.app/Contents/Resources/"
                "OpenCodexRelay_OpenCodexRelayLocalization.bundle"
            )
            self.assertIn(f"{bundle_root}/en.lproj/Localizable.strings", archive_entries)
            self.assertIn(f"{bundle_root}/ko.lproj/Localizable.strings", archive_entries)
            self.assertIn(
                "OpenCodexRelay.app/Contents/Library/HelperTools/"
                "OpenCodexRelayPrivilegedHelper",
                archive_entries,
            )
            self.assertNotIn(
                "OpenCodexRelay.app/Contents/Library/LaunchDaemons/"
                "io.github.novelkr.opencodex-relay.homebrew-guard.plist",
                archive_entries,
            )
            self.assertIn(
                "OpenCodexRelay.app/Contents/Library/Helpers/"
                "OpenCodexRelayHelperInstaller",
                archive_entries,
            )
            self.assertIn(
                "OpenCodexRelay.app/Contents/Resources/ReleaseTrust/"
                "opencodex-relay-release-ed25519.pub",
                archive_entries,
            )
            self.assertNotIn(
                "OpenCodexRelay.app/"
                "OpenCodexRelay_OpenCodexRelayLocalization.bundle"
                "/en.lproj/Localizable.strings",
                archive_entries,
            )
            manifest_path = output / "manifest-1.2.3.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertEqual(manifest["compatibility_revision"], 5)
            self.assertEqual(manifest["channel"], "stable")
            self.assertEqual(manifest["minimum_updater_version"], "0.3.8-rc.6")
            self.assertRegex(manifest["trust_key_id"], r"^[0-9a-f]{64}$")
            self.assertEqual(len(manifest["artifacts"]), 5)
            macos = next(
                artifact
                for artifact in manifest["artifacts"]
                if artifact["component"] == "macos_menu_bar_bundle"
            )
            self.assertEqual(macos["signing_mode"], "adhoc")
            self.assertEqual(macos["minimum_macos_version"], "26.0")
            self.assertEqual(macos["integration_protocol"], 1)
            self.assertEqual(macos["helper_protocol"], 1)
            self.assertNotIn("team_id", macos)
            self.assertEqual(len(manifest["documents"]), 1)
            document = manifest["documents"][0]
            self.assertEqual(document["file"], "THIRD_PARTY_NOTICES.md")
            self.assertEqual(
                document["url"],
                "https://github.com/owner/private-release/releases/download/1.2.3/THIRD_PARTY_NOTICES.md",
            )
            self.assertEqual(
                document["sha256"],
                hashlib.sha256((output / document["file"]).read_bytes()).hexdigest(),
            )
            signature_binary = root / "manifest.sig.bin"
            signature_binary.write_bytes(
                base64.b64decode((output / "manifest-1.2.3.sig").read_text(encoding="ascii"))
            )
            verified = subprocess.run(
                [
                    "openssl",
                    "pkeyutl",
                    "-verify",
                    "-pubin",
                    "-inkey",
                    str(public_key),
                    "-rawin",
                    "-in",
                    str(manifest_path),
                    "-sigfile",
                    str(signature_binary),
                ],
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertEqual(verified.returncode, 0, verified.stderr)

            # This publisher unit uses an ephemeral signing key. Bind the
            # fixture manifest to that key and re-sign it so revision 5 trust
            # validation succeeds before exercising notice tampering. The
            # production build itself remains bound to the tracked app key.
            public_der = subprocess.run(
                ["openssl", "pkey", "-pubin", "-in", str(public_key), "-outform", "DER"],
                check=True,
                capture_output=True,
            ).stdout
            ephemeral_key_id = hashlib.sha256(public_der).hexdigest()
            manifest_bytes = manifest_path.read_bytes()
            tracked_key_id = manifest["trust_key_id"].encode("ascii")
            self.assertEqual(manifest_bytes.count(tracked_key_id), 1)
            manifest_path.write_bytes(manifest_bytes.replace(tracked_key_id, ephemeral_key_id.encode("ascii")))
            subprocess.run(
                [
                    "openssl",
                    "pkeyutl",
                    "-sign",
                    "-inkey",
                    str(private_key),
                    "-rawin",
                    "-in",
                    str(manifest_path),
                    "-out",
                    str(signature_binary),
                ],
                check=True,
                capture_output=True,
            )
            (output / "manifest-1.2.3.sig").write_bytes(
                base64.b64encode(signature_binary.read_bytes()) + b"\n"
            )

            gh_state = root / "gh-state"
            gh_state.mkdir()
            gh_log = gh_state / "calls.log"
            release_fragment = root / "release-fragment.md"
            release_fragment.write_text(
                "## What changed\n\n- Stable publisher test fragment.\n",
                encoding="utf-8",
            )
            fake_gh = fake_bin / "gh"
            fake_gh.write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "printf '%s\\n' \"$*\" >> \"$FAKE_GH_LOG\"\n"
                "if [[ ${1:-} == api && ${2:-} == repos/*/releases/latest ]]; then if [[ -n ${FAKE_LATEST_TAG:-} ]]; then printf '%s\\n' \"$FAKE_LATEST_TAG\"; else cat \"$FAKE_GH_STATE/tag\"; fi; exit 0; fi\n"
                "if [[ ${1:-} == api && ${2:-} == repos/* ]]; then printf 'public\\n'; exit 0; fi\n"
                "if [[ ${1:-} == release && ${2:-} == view ]]; then\n"
                "  [[ -f $FAKE_GH_STATE/created ]] || exit 1\n"
                "  if [[ \" $* \" == *\" --json tagName,isDraft,isPrerelease,isImmutable \"* ]]; then immutable=true; [[ ${FAKE_GH_MUTABLE:-0} != 1 ]] || immutable=false; printf '%s\\t%s\\t%s\\t%s\\n' \"$(cat \"$FAKE_GH_STATE/tag\")\" \"$(cat \"$FAKE_GH_STATE/draft\")\" \"$(cat \"$FAKE_GH_STATE/prerelease\")\" \"$immutable\"; exit 0; fi\n"
                "  if [[ \" $* \" == *\" --json isDraft,isPrerelease \"* ]]; then printf '%s\\t%s\\n' \"$(cat \"$FAKE_GH_STATE/draft\")\" \"$(cat \"$FAKE_GH_STATE/prerelease\")\"; exit 0; fi\n"
                "  if [[ \" $* \" == *\" --json body \"* ]]; then if [[ ${FAKE_GH_CORRUPT_BODY:-0} == 1 ]]; then printf 'corrupted body\\n' | jq -Rs '{body:.}'; else jq -Rs '{body:.}' \"$FAKE_GH_STATE/notes\"; fi; exit 0; fi\n"
                "  if [[ \" $* \" == *\" --json assets \"* && \" $* \" == *\" --jq \"* ]]; then cat \"$FAKE_GH_STATE/assets\"; exit 0; fi\n"
                "  if [[ \" $* \" == *\" --json assets \"* ]]; then first=true; printf '{\"assets\":['; while IFS= read -r name; do $first || printf ','; first=false; digest=$(shasum -a 256 \"$FAKE_ASSET_DIR/$name\" | awk '{print $1}'); [[ ${FAKE_GH_CORRUPT_DIGEST:-0} != 1 ]] || digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; jq -jn --arg name \"$name\" --arg digest \"sha256:$digest\" '{name:$name,state:\"uploaded\",digest:$digest}'; done < \"$FAKE_GH_STATE/assets\"; printf ']}\\n'; exit 0; fi\n"
                "  exit 0\n"
                "fi\n"
                "if [[ ${1:-} == release && ${2:-} == create ]]; then\n"
                "  printf '%s\\n' \"$3\" > \"$FAKE_GH_STATE/tag\"\n"
                "  shift 3\n"
                "  : > \"$FAKE_GH_STATE/assets\"\n"
                "  printf 'false\\n' > \"$FAKE_GH_STATE/prerelease\"\n"
                "  while [[ $# -gt 0 ]]; do\n"
                "    case $1 in\n"
                "      --repo|--title) shift 2 ;;\n"
                "      --notes-file) cp \"$2\" \"$FAKE_GH_STATE/notes\"; shift 2 ;;\n"
                "      --draft|--latest|--latest=false) shift ;;\n"
                "      --prerelease) printf 'true\\n' > \"$FAKE_GH_STATE/prerelease\"; shift ;;\n"
                "      *) name=$(basename -- \"$1\"); if [[ ${FAKE_GH_MISSING_ASSET:-0} != 1 || $name != OpenCodexRelay.app.zip ]]; then printf '%s\\n' \"$name\" >> \"$FAKE_GH_STATE/assets\"; fi; if [[ ${FAKE_GH_DUPLICATE_ASSET:-0} == 1 && $name == OpenCodexRelay.app.zip ]]; then printf '%s\\n' \"$name\" >> \"$FAKE_GH_STATE/assets\"; fi; shift ;;\n"
                "    esac\n"
                "  done\n"
                "  : > \"$FAKE_GH_STATE/created\"\n"
                "  printf 'true\\n' > \"$FAKE_GH_STATE/draft\"\n"
                "  exit 0\n"
                "fi\n"
                "if [[ ${1:-} == release && ${2:-} == edit ]]; then printf 'false\\n' > \"$FAKE_GH_STATE/draft\"; [[ \" $* \" != *\" --prerelease=false \"* ]] || printf 'false\\n' > \"$FAKE_GH_STATE/prerelease\"; if [[ ${FAKE_GH_CORRUPT_AFTER_EDIT:-0} == 1 ]]; then printf 'true\\n' > \"$FAKE_GH_STATE/prerelease\"; fi; if [[ ${FAKE_GH_CORRUPT_TAG:-0} == 1 ]]; then printf '9.9.9\\n' > \"$FAKE_GH_STATE/tag\"; fi; exit 0; fi\n"
                "if [[ ${1:-} == release && ${2:-} == delete ]]; then : > \"$FAKE_GH_STATE/deleted\"; rm -f \"$FAKE_GH_STATE/created\"; exit 0; fi\n"
                "exit 2\n",
                encoding="utf-8",
            )
            fake_gh.chmod(0o755)
            publish_command = [
                "bash",
                str(RELAY / "scripts" / "publish-github-release.sh"),
                "1.2.3",
                "--repo",
                "owner/private-release",
                "--input",
                str(output),
                "--public-key",
                str(public_key),
                "--release-notes-fragment",
                str(release_fragment),
            ]
            publish_environment = os.environ | {
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "FAKE_GH_LOG": str(gh_log),
                "FAKE_GH_STATE": str(gh_state),
                "FAKE_ASSET_DIR": str(output),
            }
            built_notices = output / "THIRD_PARTY_NOTICES.md"
            original_notices = built_notices.read_bytes()
            built_notices.write_bytes(b"tampered notice\n")
            tampered_publish = subprocess.run(
                publish_command,
                check=False,
                capture_output=True,
                text=True,
                env=publish_environment,
            )
            self.assertNotEqual(tampered_publish.returncode, 0)
            self.assertIn("does not bind THIRD_PARTY_NOTICES.md", tampered_publish.stderr)
            self.assertNotIn("release create", gh_log.read_text(encoding="utf-8"))
            built_notices.write_bytes(original_notices)

            for invalid_version in (
                "01.2.3",
                "1.02.3",
                "1.2.03",
                "1.2.3-01",
                "1.2.3-rc..1",
                "1.2.3-rc.",
            ):
                invalid_publish = subprocess.run(
                    [
                        "bash",
                        str(RELAY / "scripts" / "publish-github-release.sh"),
                        invalid_version,
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=publish_environment,
                )
                self.assertNotEqual(invalid_publish.returncode, 0)
                self.assertIn("strict SemVer", invalid_publish.stderr)

            for variable, label in (
                ("FAKE_GH_MISSING_ASSET", "missing"),
                ("FAKE_GH_DUPLICATE_ASSET", "duplicate"),
            ):
                invalid_assets_state = root / f"gh-{label}-asset-state"
                invalid_assets_state.mkdir()
                invalid_assets_log = invalid_assets_state / "calls.log"
                invalid_assets_publish = subprocess.run(
                    publish_command,
                    check=False,
                    capture_output=True,
                    text=True,
                    env=publish_environment
                    | {
                        "FAKE_GH_LOG": str(invalid_assets_log),
                        "FAKE_GH_STATE": str(invalid_assets_state),
                        variable: "1",
                    },
                )
                self.assertNotEqual(invalid_assets_publish.returncode, 0)
                self.assertIn("asset set is incomplete or unexpected", invalid_assets_publish.stderr)
                self.assertTrue((invalid_assets_state / "deleted").is_file())

            corrupt_body_state = root / "gh-corrupt-body-state"
            corrupt_body_state.mkdir()
            corrupt_body_log = corrupt_body_state / "calls.log"
            corrupt_body_environment = publish_environment | {
                "FAKE_GH_LOG": str(corrupt_body_log),
                "FAKE_GH_STATE": str(corrupt_body_state),
                "FAKE_GH_CORRUPT_BODY": "1",
            }
            corrupt_body_publish = subprocess.run(
                publish_command,
                check=False,
                capture_output=True,
                text=True,
                env=corrupt_body_environment,
            )
            self.assertNotEqual(corrupt_body_publish.returncode, 0)
            self.assertIn(
                "release body differs from the reviewed release notes",
                corrupt_body_publish.stderr,
            )
            corrupt_body_calls = corrupt_body_log.read_text(encoding="utf-8")
            self.assertIn("release delete", corrupt_body_calls)
            self.assertNotIn("release edit", corrupt_body_calls)
            self.assertTrue((corrupt_body_state / "deleted").is_file())

            corrupt_final_state = root / "gh-corrupt-final-state"
            corrupt_final_state.mkdir()
            corrupt_final_log = corrupt_final_state / "calls.log"
            corrupt_final_environment = publish_environment | {
                "FAKE_GH_LOG": str(corrupt_final_log),
                "FAKE_GH_STATE": str(corrupt_final_state),
                "FAKE_GH_CORRUPT_AFTER_EDIT": "1",
            }
            corrupt_final_publish = subprocess.run(
                publish_command,
                check=False,
                capture_output=True,
                text=True,
                env=corrupt_final_environment,
            )
            self.assertNotEqual(corrupt_final_publish.returncode, 0)
            corrupt_final_calls = corrupt_final_log.read_text(encoding="utf-8")
            self.assertLess(
                corrupt_final_calls.index("release edit"),
                corrupt_final_calls.index("release delete"),
            )
            self.assertTrue((corrupt_final_state / "deleted").is_file())

            for variable, label, expected_error in (
                (
                    "FAKE_GH_CORRUPT_DIGEST",
                    "corrupt-digest",
                    "asset digest differs from the uploaded file",
                ),
                (
                    "FAKE_GH_MUTABLE",
                    "mutable",
                    "not the expected immutable tag/draft/prerelease state",
                ),
                (
                    "FAKE_GH_CORRUPT_TAG",
                    "wrong-tag",
                    "not the expected immutable tag/draft/prerelease state",
                ),
            ):
                invalid_final_state = root / f"gh-{label}-state"
                invalid_final_state.mkdir()
                invalid_final_log = invalid_final_state / "calls.log"
                invalid_final_publish = subprocess.run(
                    publish_command,
                    check=False,
                    capture_output=True,
                    text=True,
                    env=publish_environment
                    | {
                        "FAKE_GH_LOG": str(invalid_final_log),
                        "FAKE_GH_STATE": str(invalid_final_state),
                        variable: "1",
                    },
                )
                self.assertNotEqual(invalid_final_publish.returncode, 0)
                self.assertIn(expected_error, invalid_final_publish.stderr)
                self.assertTrue((invalid_final_state / "deleted").is_file())

            published = subprocess.run(
                publish_command,
                check=False,
                capture_output=True,
                text=True,
                env=publish_environment,
            )
            self.assertEqual(published.returncode, 0, published.stderr)
            self.assertIn("prerelease=false", published.stdout)
            self.assertIn("latest=true", published.stdout)
            self.assertIn("immutable=true", published.stdout)
            self.assertEqual(
                set((gh_state / "assets").read_text(encoding="utf-8").splitlines()),
                expected_assets,
            )
            release_notes = (gh_state / "notes").read_text(encoding="utf-8")
            self.assertIn("# OpenCodex Relay 1.2.3", release_notes)
            self.assertIn("Stable publisher test fragment.", release_notes)
            self.assertNotIn("This is a pre-release", release_notes)
            self.assertIn("## Choose your download", release_notes)
            self.assertIn("`OpenCodexRelay.app.zip`", release_notes)
            self.assertIn("## Install or update", release_notes)
            self.assertIn("## Security and verification", release_notes)
            self.assertIn(
                "gh attestation verify FILE --repo owner/private-release",
                release_notes,
            )
            self.assertIn("--source-ref refs/tags/1.2.3", release_notes)
            self.assertIn("## macOS first launch", release_notes)
            self.assertIn("Open Anyway", release_notes)
            self.assertIn(
                "https://github.com/owner/private-release/tree/1.2.3",
                release_notes,
            )
            self.assertNotIn("__VERSION__", release_notes)
            self.assertNotIn("__REPOSITORY__", release_notes)
            gh_calls = gh_log.read_text(encoding="utf-8")
            self.assertLess(gh_calls.index("release create"), gh_calls.index("release edit"))
            self.assertIn("--prerelease=false", gh_calls)
            self.assertIn("--latest", gh_calls)
            self.assertNotIn("--latest=false", gh_calls)

            prerelease_version = "1.2.4-rc.1"
            prerelease_output = root / "prerelease-output"
            prerelease_output.mkdir()
            for artifact in output.iterdir():
                if artifact.name.startswith("manifest-"):
                    continue
                shutil.copy2(artifact, prerelease_output / artifact.name)
            prerelease_manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            prerelease_manifest["version"] = prerelease_version
            prerelease_manifest["channel"] = "preview"
            for entry in [
                *prerelease_manifest["artifacts"],
                *prerelease_manifest["documents"],
            ]:
                entry["url"] = entry["url"].replace(
                    "/releases/download/1.2.3/",
                    f"/releases/download/{prerelease_version}/",
                )
            prerelease_manifest_path = (
                prerelease_output / f"manifest-{prerelease_version}.json"
            )
            prerelease_manifest_path.write_text(
                json.dumps(prerelease_manifest, separators=(",", ":")) + "\n",
                encoding="utf-8",
            )
            prerelease_signature = subprocess.run(
                [
                    "openssl",
                    "pkeyutl",
                    "-sign",
                    "-rawin",
                    "-inkey",
                    str(private_key),
                    "-in",
                    str(prerelease_manifest_path),
                ],
                check=True,
                capture_output=True,
            ).stdout
            (
                prerelease_output / f"manifest-{prerelease_version}.sig"
            ).write_text(
                base64.b64encode(prerelease_signature).decode("ascii") + "\n",
                encoding="ascii",
            )
            prerelease_fragment = root / "prerelease-fragment.md"
            prerelease_fragment.write_text(
                "## What changed\n\n- First-run Settings CTA evaluation.\n",
                encoding="utf-8",
            )
            prerelease_state = root / "gh-prerelease-state"
            prerelease_state.mkdir()
            prerelease_log = prerelease_state / "calls.log"
            prerelease_environment = publish_environment | {
                "FAKE_GH_LOG": str(prerelease_log),
                "FAKE_GH_STATE": str(prerelease_state),
                "FAKE_ASSET_DIR": str(prerelease_output),
                "FAKE_LATEST_TAG": "1.2.3",
            }
            prerelease_publish = subprocess.run(
                [
                    "bash",
                    str(RELAY / "scripts" / "publish-github-release.sh"),
                    prerelease_version,
                    "--repo",
                    "owner/private-release",
                    "--input",
                    str(prerelease_output),
                    "--public-key",
                    str(public_key),
                    "--release-notes-fragment",
                    str(prerelease_fragment),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=prerelease_environment,
            )
            self.assertEqual(
                prerelease_publish.returncode,
                0,
                prerelease_publish.stderr,
            )
            self.assertIn("prerelease=true", prerelease_publish.stdout)
            self.assertIn("latest=false", prerelease_publish.stdout)
            self.assertEqual(
                (prerelease_state / "prerelease").read_text(encoding="utf-8"),
                "true\n",
            )
            prerelease_notes = (prerelease_state / "notes").read_text(
                encoding="utf-8"
            )
            self.assertIn("This is a pre-release", prerelease_notes)
            self.assertIn("First-run Settings CTA evaluation.", prerelease_notes)
            self.assertIn(
                "--prerelease",
                prerelease_log.read_text(encoding="utf-8"),
            )

    def test_private_github_release_installs_only_after_api_redirect_and_signature_checks(self) -> None:
        installer_path = RELAY / "scripts" / "install-relay.sh"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            asset_dir = root / "assets"
            fake_bin = root / "bin"
            tmp_dir = root / "tmp"
            home.mkdir()
            asset_dir.mkdir()
            fake_bin.mkdir()
            tmp_dir.mkdir()

            private_key = root / "private.pem"
            public_key = root / "public.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(private_key)],
                check=True,
                capture_output=True,
                text=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
                text=True,
            )

            relay_name = "opencodex-relay_darwin_arm64"
            relayctl_name = "opencodex-relayctl_darwin_arm64"
            notices_name = "THIRD_PARTY_NOTICES.md"
            (asset_dir / relay_name).write_text(
                "#!/usr/bin/env bash\n"
                "[[ \"${FAIL_RELAY_CHECK:-0}\" != 1 ]]\n",
                encoding="utf-8",
            )
            (asset_dir / relayctl_name).write_text(
                "#!/usr/bin/env bash\n"
                "set -eu\n"
                "if [[ \"${1:-}\" == lifecycle ]]; then\n"
                "  case \"${2:-}\" in\n"
                "    source-install-capability) printf '%s\\n' '{\"schema_version\":2,\"state\":\"ready\"}' ;;\n"
                "    reserve-source-install)\n"
                "      recovery_file=\n"
                "      while [[ $# -gt 0 ]]; do\n"
                "        if [[ \"$1\" == --recovery-file ]]; then recovery_file=\"$2\"; shift 2; else shift; fi\n"
                "      done\n"
                "      [[ -n \"$recovery_file\" ]]\n"
                "      reservation_root=\"$HOME/.local/lib/opencodex-relay/relay\"\n"
                "      mkdir -p \"$reservation_root\"; chmod 700 \"$reservation_root\"\n"
                "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"production\",\"token\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"root_created\":false}' > \"$recovery_file\"\n"
                "      chmod 600 \"$recovery_file\"\n"
                "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"production\",\"token\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"}' > \"$reservation_root/.source-install-reservation.json\"\n"
                "      chmod 600 \"$reservation_root/.source-install-reservation.json\"\n"
                "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"production\",\"token\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"root_created\":false}' ;;\n"
                "    release-source-install)\n"
                "      rm -f \"$HOME/.local/lib/opencodex-relay/relay/.source-install-reservation.json\"\n"
                "      printf '%s\\n' '{\"schema_version\":1,\"state\":\"released\"}' ;;\n"
                "    *) exit 64 ;;\n"
                "  esac\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"${1:-}\" == init ]]; then\n"
                "  shift\n"
                "  while [[ $# -gt 0 ]]; do\n"
                "    if [[ \"$1\" == --config ]]; then config=\"$2\"; shift 2; else shift; fi\n"
                "  done\n"
                "  mkdir -p \"$(dirname \"$config\")\"\n"
                "  printf '%s\\n' \"{\\\"listen_address\\\":\\\"127.0.0.1:18180\\\","
                "\\\"upstream_base_url\\\":\\\"https://example.test/v1\\\","
                "\\\"catalog\\\":{\\\"path\\\":\\\"$HOME/.codex/opencodex-relay-catalog.json\\\","
                "\\\"manage_app_server\\\":false}}\" > \"$config\"\n"
                "  chmod 600 \"$config\"\n"
                "fi\n"
                "if [[ \"${1:-}\" == enable ]]; then\n"
                "  shift\n"
                "  while [[ $# -gt 0 ]]; do\n"
                "    if [[ \"$1\" == --codex-config ]]; then codex_config=\"$2\"; shift 2; else shift; fi\n"
                "  done\n"
                "  mkdir -p \"$(dirname \"$codex_config\")\"\n"
                "  printf '%s\\n' '# fake relay routing' > \"$codex_config\"\n"
                "  chmod 600 \"$codex_config\"\n"
                "  if [[ \"${FAIL_RELAYCTL_ENABLE_AFTER_WRITE:-0}\" == 1 ]]; then exit 86; fi\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            for path in (asset_dir / relay_name, asset_dir / relayctl_name):
                path.chmod(0o755)
            notices_content = "third-party license terms\n"
            (asset_dir / notices_name).write_text(notices_content, encoding="utf-8")

            def digest(name: str) -> str:
                return hashlib.sha256((asset_dir / name).read_bytes()).hexdigest()

            version = "1.2.3"
            repo = "owner/private-release"
            manifest = {
                "version": version,
                "compatibility_revision": 2,
                "artifacts": [
                    {
                        "os": "darwin",
                        "arch": "arm64",
                        "file": relay_name,
                        "url": f"https://github.com/{repo}/releases/download/{version}/{relay_name}",
                        "sha256": digest(relay_name),
                    },
                    {
                        "os": "darwin",
                        "arch": "arm64",
                        "file": relayctl_name,
                        "url": f"https://github.com/{repo}/releases/download/{version}/{relayctl_name}",
                        "sha256": digest(relayctl_name),
                    },
                ],
                "documents": [
                    {
                        "file": notices_name,
                        "url": f"https://github.com/{repo}/releases/download/{version}/{notices_name}",
                        "sha256": digest(notices_name),
                    }
                ],
            }
            manifest_path = asset_dir / f"manifest-{version}.json"
            manifest_path.write_text(json.dumps(manifest, separators=(",", ":")) + "\n", encoding="utf-8")
            signature_path = asset_dir / f"manifest-{version}.sig"
            signed = subprocess.run(
                ["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(private_key), "-in", str(manifest_path)],
                check=True,
                capture_output=True,
            )
            signature_path.write_text(base64.b64encode(signed.stdout).decode("ascii") + "\n", encoding="utf-8")

            release_metadata = {
                "tag_name": version,
                "draft": False,
                "immutable": True,
                "assets": [
                    {"name": manifest_path.name, "state": "uploaded", "url": f"https://api.github.com/repos/{repo}/releases/assets/1"},
                    {"name": signature_path.name, "state": "uploaded", "url": f"https://api.github.com/repos/{repo}/releases/assets/2"},
                    {"name": relay_name, "state": "uploaded", "url": f"https://api.github.com/repos/{repo}/releases/assets/3"},
                    {"name": relayctl_name, "state": "uploaded", "url": f"https://api.github.com/repos/{repo}/releases/assets/4"},
                    {"name": notices_name, "state": "uploaded", "url": f"https://api.github.com/repos/{repo}/releases/assets/5"},
                ],
            }
            metadata_path = root / "release.json"
            metadata_path.write_text(json.dumps(release_metadata), encoding="utf-8")
            general_health_path = root / "general-health.json"
            interactive_health_path = root / "interactive-health.json"
            general_health_path.write_text(
                json.dumps(relay_scheduler_health("general")) + "\n", encoding="utf-8"
            )
            interactive_health_path.write_text(
                json.dumps(relay_scheduler_health("interactive")) + "\n", encoding="utf-8"
            )
            invalid_interactive_health_path = root / "invalid-interactive-health.json"
            invalid_interactive_health_path.write_text(
                json.dumps(relay_scheduler_health("general")) + "\n", encoding="utf-8"
            )

            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "output= headers= url= has_config=0\n"
                "while [[ $# -gt 0 ]]; do\n"
                "  case \"$1\" in\n"
                "    --config) has_config=1; shift 2 ;;\n"
                "    --dump-header) headers=\"$2\"; shift 2 ;;\n"
                "    --write-out|-H|-o|--noproxy|--max-time)\n"
                "      if [[ \"$1\" == -o ]]; then output=\"$2\"; fi\n"
                "      shift 2 ;;\n"
                "    --fail|--silent|--show-error|--location) shift ;;\n"
                "    *) url=\"$1\"; shift ;;\n"
                "  esac\n"
                "done\n"
                "if [[ \"$url\" == https://api.github.com/repos/owner/private-release/releases/tags/1.2.3 ]]; then\n"
                "  [[ $has_config -eq 1 ]] || exit 41\n"
                "  cp \"$FAKE_RELEASE_METADATA\" \"$output\"\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"$url\" == https://api.github.com/repos/owner/private-release/releases/assets/* ]]; then\n"
                "  [[ $has_config -eq 1 ]] || exit 42\n"
                "  id=\"${url##*/}\"\n"
                "  printf 'Location: https://download.example.test/assets/%s\\r\\n' \"$id\" > \"$headers\"\n"
                "  : > \"$output\"\n"
                "  printf 302\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"$url\" == https://download.example.test/assets/* ]]; then\n"
                "  [[ $has_config -eq 0 ]] || exit 43\n"
                "  id=\"${url##*/}\"\n"
                "  case \"$id\" in\n"
                f"    1) file=\"manifest-{version}.json\" ;;\n"
                f"    2) file=\"manifest-{version}.sig\" ;;\n"
                f"    3) file=\"{relay_name}\" ;;\n"
                f"    4) file=\"{relayctl_name}\" ;;\n"
                f"    5) file=\"{notices_name}\" ;;\n"
                "    *) exit 44 ;;\n"
                "  esac\n"
                "  cp \"$FAKE_ASSET_DIR/$file\" \"$output\"\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"$url\" == http://127.0.0.1:18180/__relay/healthz ]]; then\n"
                "  cat \"$FAKE_GENERAL_HEALTH\"\n"
                "  exit 0\n"
                "fi\n"
                "if [[ \"$url\" == \"http://$FAKE_INTERACTIVE_LISTENER/__relay/healthz\" ]]; then\n"
                "  cat \"$FAKE_INTERACTIVE_HEALTH\"\n"
                "  exit 0\n"
                "fi\n"
                "exit 45\n",
                encoding="utf-8",
            )
            launchctl_log = root / "launchctl.log"
            launchctl_fail_marker = root / "launchctl.failed"
            (fake_bin / "launchctl").write_text(
                "#!/usr/bin/env bash\n"
                "set -euo pipefail\n"
                "printf '%s\\n' \"$*\" >> \"$FAKE_LAUNCHCTL_LOG\"\n"
                "if [[ \"${1:-}\" == print ]]; then\n"
                "  [[ \"${FAKE_LAUNCHCTL_ACTIVE:-0}\" == 1 ]]\n"
                "  exit\n"
                "fi\n"
                "if [[ \"${1:-}\" == bootstrap && \"${FAIL_LAUNCHCTL_BOOTSTRAP_ONCE:-0}\" == 1 && ! -e \"$FAKE_LAUNCHCTL_FAIL_MARKER\" ]]; then\n"
                "  : > \"$FAKE_LAUNCHCTL_FAIL_MARKER\"\n"
                "  exit 87\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            (fake_bin / "lsof").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ ${FAIL_INTERACTIVE_PORT_OCCUPIED:-0} == 1 ]]; then "
                "printf '%s\\n' 'relay 123 user 1u IPv4 TCP 127.0.0.1:18182 (LISTEN)'; exit 0; fi\n"
                "exit 1\n",
                encoding="utf-8",
            )
            (fake_bin / "ss").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ ${FAIL_INTERACTIVE_PORT_OCCUPIED:-0} == 1 ]]; then "
                "printf '%s\\n' 'LISTEN 0 128 127.0.0.1:18182 0.0.0.0:*'; fi\n",
                encoding="utf-8",
            )
            (fake_bin / "sleep").write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            (fake_bin / "uname").write_text(
                "#!/usr/bin/env bash\n"
                "case \"${1:-}\" in\n"
                "  -s) printf 'Darwin\\n' ;;\n"
                "  -m) printf 'arm64\\n' ;;\n"
                "  *) printf 'Darwin\\n' ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            (fake_bin / "stat").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ $(/usr/bin/uname -s) == Linux && ${1:-} == -f && ${2:-} == '%u:%Lp' ]]; then\n"
                "  exec /usr/bin/stat -c '%u:%a' \"$3\"\n"
                "fi\n"
                "exec /usr/bin/stat \"$@\"\n",
                encoding="utf-8",
            )
            (fake_bin / "mv").write_text(
                "#!/usr/bin/env bash\n"
                "if [[ $(/usr/bin/uname -s) == Linux && ${1:-} == -fh ]]; then\n"
                "  shift\n"
                "  exec /bin/mv -fT \"$@\"\n"
                "fi\n"
                "exec /bin/mv \"$@\"\n",
                encoding="utf-8",
            )
            for path in (
                fake_bin / "curl",
                fake_bin / "launchctl",
                fake_bin / "lsof",
                fake_bin / "mv",
                fake_bin / "ss",
                fake_bin / "sleep",
                fake_bin / "stat",
                fake_bin / "uname",
            ):
                path.chmod(0o755)

            token_dir = home / ".config" / "opencodex-relay"
            token_dir.mkdir(parents=True)
            token_path = token_dir / "github-release.token"
            token_path.write_text("github_pat_example", encoding="utf-8")
            token_path.chmod(0o600)
            environment = os.environ | {
                "HOME": str(home),
                "TMPDIR": str(tmp_dir),
                "XDG_CONFIG_HOME": str(home / ".config"),
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "FAKE_ASSET_DIR": str(asset_dir),
                "FAKE_RELEASE_METADATA": str(metadata_path),
                "FAKE_GENERAL_HEALTH": str(general_health_path),
                "FAKE_INTERACTIVE_HEALTH": str(interactive_health_path),
                "FAKE_INTERACTIVE_LISTENER": "127.0.0.1:18182",
                "FAKE_LAUNCHCTL_LOG": str(launchctl_log),
                "FAKE_LAUNCHCTL_FAIL_MARKER": str(launchctl_fail_marker),
                "FAKE_LAUNCHCTL_ACTIVE": "1",
            }
            install_root = home / ".local" / "lib" / "opencodex-relay" / "relay"
            old_target = install_root / "0.9.0" / "darwin-arm64"
            old_target.mkdir(parents=True)
            for name in ("opencodex-relay", "opencodex-relayctl"):
                path = old_target / name
                path.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
                path.chmod(0o700)
            current = install_root / "current"
            current.symlink_to("0.9.0/darwin-arm64")
            config_path = token_dir / "relay.json"
            catalog_path = home / ".codex" / "opencodex-relay-catalog.json"
            config_path.write_text(
                json.dumps(
                    {
                        "listen_address": "127.0.0.1:18180",
                        "upstream_mode": "",
                        "upstream_base_url": "https://example.test/v1",
                        "responses": {
                            "websocket_mode": "",
                            "scheduler": {
                                "interactive_listen_address": "",
                                "max_classifications": 0,
                                "max_pending_requests": 0,
                                "max_pending_encoded_bytes": 0,
                                "queue_timeout_ms": 0,
                                "max_general_upstream": 0,
                                "interactive_reserved_upstream": 0,
                                "max_concurrent_transforms": 0,
                                "max_open_deliveries": 0,
                            },
                        },
                        "catalog": {
                            "owner": "",
                            "path": str(catalog_path),
                            "manage_app_server": False,
                        },
                    },
                    indent=2,
                )
                + "\n",
                encoding="utf-8",
            )
            config_path.chmod(0o600)
            original_relay_config = config_path.read_text(encoding="utf-8")
            old_plist = home / "Library" / "LaunchAgents" / "io.github.novelkr.opencodex-relay.plist"
            old_plist.parent.mkdir(parents=True)
            old_plist.write_text("<plist>old relay service</plist>\n", encoding="utf-8")
            old_plist.chmod(0o600)
            old_codex_config = home / ".codex" / "config.toml"
            old_codex_config.parent.mkdir(parents=True)
            old_codex_config.write_text('model = "old"\n', encoding="utf-8")
            old_codex_config.chmod(0o600)
            command = [
                "bash",
                str(installer_path),
                "install",
                version,
                "--github-repo",
                repo,
                "--github-token-file",
                str(token_path),
                "--public-key",
                str(public_key),
                "--upstream",
                "https://example.test/v1",
            ]
            (asset_dir / notices_name).write_text("tampered notice\n", encoding="utf-8")
            tampered_notice = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(tampered_notice.returncode, 0)
            self.assertIn("THIRD_PARTY_NOTICES.md SHA-256 does not match manifest", tampered_notice.stderr)
            self.assertEqual(os.readlink(current), "0.9.0/darwin-arm64")
            (asset_dir / notices_name).write_text(notices_content, encoding="utf-8")

            failed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAIL_RELAY_CHECK": "1"},
            )
            self.assertNotEqual(failed.returncode, 0)
            self.assertIn("failed after the rollback snapshot", failed.stderr)
            self.assertEqual(os.readlink(current), "0.9.0/darwin-arm64")
            self.assertEqual(config_path.read_text(encoding="utf-8"), original_relay_config)
            self.assertEqual(old_codex_config.read_text(encoding="utf-8"), 'model = "old"\n')
            self.assertEqual(old_plist.read_text(encoding="utf-8"), "<plist>old relay service</plist>\n")
            self.assertFalse((install_root / version / "darwin-arm64").exists())
            interactive_profile = home / ".codex" / "opencodex-relay-interactive.config.toml"
            self.assertFalse(interactive_profile.exists())

            old_interactive_profile = (
                "# opencodex-relay-managed-interactive-profile-v1\n"
                'openai_base_url = "http://127.0.0.1:19999/v1"\n'
                f'model_catalog_json = "{home / ".codex" / "old-catalog.json"}"\n'
            )
            interactive_profile.write_text(old_interactive_profile, encoding="utf-8")
            interactive_profile.chmod(0o600)

            # relayctl can fail after it has already replaced the native Codex
            # routing file. The install-wide EXIT transaction must restore that
            # partial mutation even though the service step was never reached.
            enable_failed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAIL_RELAYCTL_ENABLE_AFTER_WRITE": "1"},
            )
            self.assertNotEqual(enable_failed.returncode, 0)
            self.assertIn("failed after the rollback snapshot", enable_failed.stderr)
            self.assertEqual(os.readlink(current), "0.9.0/darwin-arm64")
            self.assertEqual(config_path.read_text(encoding="utf-8"), original_relay_config)
            self.assertEqual(old_codex_config.read_text(encoding="utf-8"), 'model = "old"\n')
            self.assertEqual(old_plist.read_text(encoding="utf-8"), "<plist>old relay service</plist>\n")
            self.assertEqual(interactive_profile.read_text(encoding="utf-8"), old_interactive_profile)
            self.assertFalse((install_root / version / "darwin-arm64").exists())

            health_failed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAKE_INTERACTIVE_HEALTH": str(invalid_interactive_health_path)},
            )
            self.assertNotEqual(health_failed.returncode, 0)
            self.assertIn("did not reach the reviewed health contract", health_failed.stderr)
            self.assertEqual(os.readlink(current), "0.9.0/darwin-arm64")
            self.assertEqual(config_path.read_text(encoding="utf-8"), original_relay_config)
            self.assertEqual(old_codex_config.read_text(encoding="utf-8"), 'model = "old"\n')
            self.assertEqual(old_plist.read_text(encoding="utf-8"), "<plist>old relay service</plist>\n")
            self.assertEqual(interactive_profile.read_text(encoding="utf-8"), old_interactive_profile)
            self.assertFalse((install_root / version / "darwin-arm64").exists())

            service_failed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"FAIL_LAUNCHCTL_BOOTSTRAP_ONCE": "1"},
            )
            self.assertNotEqual(service_failed.returncode, 0)
            self.assertIn("restoring the previous release, service, and routing state", service_failed.stderr)
            self.assertEqual(os.readlink(current), "0.9.0/darwin-arm64")
            self.assertEqual(old_plist.read_text(encoding="utf-8"), "<plist>old relay service</plist>\n")
            self.assertEqual(old_codex_config.read_text(encoding="utf-8"), 'model = "old"\n')
            self.assertEqual(interactive_profile.read_text(encoding="utf-8"), old_interactive_profile)
            self.assertFalse((install_root / version / "darwin-arm64").exists())
            launchctl_calls = launchctl_log.read_text(encoding="utf-8").splitlines()
            self.assertGreaterEqual(sum(call.startswith("bootstrap ") for call in launchctl_calls), 2)
            self.assertTrue(any(call.startswith("kickstart ") for call in launchctl_calls))

            result = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("relay_installed=1.2.3 target=darwin/arm64", result.stdout)
            self.assertTrue(current.is_symlink())
            self.assertEqual(os.readlink(current), "1.2.3/darwin-arm64")
            installed_notices = install_root / version / "darwin-arm64" / notices_name
            self.assertEqual(installed_notices.read_text(encoding="utf-8"), notices_content)
            self.assertEqual(installed_notices.stat().st_mode & 0o777, 0o644)
            self.assertEqual(
                interactive_profile.read_text(encoding="utf-8"),
                "# opencodex-relay-managed-interactive-profile-v1\n"
                'openai_base_url = "http://127.0.0.1:18182/v1"\n'
                f'model_catalog_json = "{catalog_path}"\n',
            )

            custom_listener = "127.0.0.1:19222"
            custom_config = json.loads(config_path.read_text(encoding="utf-8"))
            custom_config["responses"] = {
                "scheduler": {"interactive_listen_address": custom_listener}
            }
            config_path.write_text(
                json.dumps(custom_config, separators=(",", ":")) + "\n",
                encoding="utf-8",
            )
            custom_general_health = root / "custom-general-health.json"
            custom_interactive_health = root / "custom-interactive-health.json"
            custom_general_health.write_text(
                json.dumps(relay_scheduler_health("general", custom_listener)) + "\n",
                encoding="utf-8",
            )
            custom_interactive_health.write_text(
                json.dumps(relay_scheduler_health("interactive", custom_listener)) + "\n",
                encoding="utf-8",
            )
            custom_result = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment
                | {
                    "FAKE_GENERAL_HEALTH": str(custom_general_health),
                    "FAKE_INTERACTIVE_HEALTH": str(custom_interactive_health),
                    "FAKE_INTERACTIVE_LISTENER": custom_listener,
                },
            )
            self.assertEqual(custom_result.returncode, 0, custom_result.stderr)
            self.assertIn(
                'openai_base_url = "http://127.0.0.1:19222/v1"',
                interactive_profile.read_text(encoding="utf-8"),
            )

            installed_notices.write_text("tampered installed notice\n", encoding="utf-8")
            existing_notice_failed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=environment
                | {
                    "FAKE_GENERAL_HEALTH": str(custom_general_health),
                    "FAKE_INTERACTIVE_HEALTH": str(custom_interactive_health),
                    "FAKE_INTERACTIVE_LISTENER": custom_listener,
                },
            )
            self.assertNotEqual(existing_notice_failed.returncode, 0)
            self.assertIn(
                "existing release target third-party notices differ from the signed manifest",
                existing_notice_failed.stderr,
            )
            installed_notices.write_text(notices_content, encoding="utf-8")

            # A first enrollment has no previous relay target, native routing,
            # or LaunchAgent. An activation failure must restore that exact
            # absence instead of leaving Codex pointed at 127.0.0.1:18180.
            fresh_home = root / "fresh-home"
            fresh_home.mkdir()
            fresh_token_dir = fresh_home / ".config" / "opencodex-relay"
            fresh_token_dir.mkdir(parents=True)
            fresh_token = fresh_token_dir / "github-release.token"
            fresh_token.write_text("github_pat_example", encoding="utf-8")
            fresh_token.chmod(0o600)
            fresh_launchctl_log = root / "fresh-launchctl.log"
            fresh_launchctl_failure = root / "fresh-launchctl.failed"
            fresh_command = command.copy()
            fresh_command[fresh_command.index(str(token_path))] = str(fresh_token)
            fresh_failed = subprocess.run(
                fresh_command,
                check=False,
                capture_output=True,
                text=True,
                env=environment
                | {
                    "HOME": str(fresh_home),
                    "FAKE_LAUNCHCTL_ACTIVE": "0",
                    "FAKE_LAUNCHCTL_LOG": str(fresh_launchctl_log),
                    "FAKE_LAUNCHCTL_FAIL_MARKER": str(fresh_launchctl_failure),
                    "FAIL_LAUNCHCTL_BOOTSTRAP_ONCE": "1",
                },
            )
            self.assertNotEqual(fresh_failed.returncode, 0)
            self.assertIn("restoring the previous release, service, and routing state", fresh_failed.stderr)
            fresh_root = fresh_home / ".local" / "lib" / "opencodex-relay" / "relay"
            self.assertFalse((fresh_root / "current").is_symlink())
            self.assertFalse((fresh_token_dir / "relay.json").exists())
            self.assertFalse((fresh_home / ".codex" / "config.toml").exists())
            self.assertFalse(
                (fresh_home / ".codex" / "opencodex-relay-interactive.config.toml").exists()
            )
            self.assertFalse(
                (fresh_home / "Library" / "LaunchAgents" / "io.github.novelkr.opencodex-relay.plist").exists()
            )
            self.assertFalse((fresh_root / version / "darwin-arm64").exists())

            unmanaged_home = root / "unmanaged-home"
            unmanaged_home.mkdir()
            unmanaged_token_dir = unmanaged_home / ".config" / "opencodex-relay"
            unmanaged_token_dir.mkdir(parents=True)
            unmanaged_token = unmanaged_token_dir / "github-release.token"
            unmanaged_token.write_text("github_pat_example", encoding="utf-8")
            unmanaged_token.chmod(0o600)
            unmanaged_profile = unmanaged_home / ".codex" / "opencodex-relay-interactive.config.toml"
            unmanaged_profile.parent.mkdir(parents=True)
            unmanaged_profile.write_text(
                'openai_base_url = "http://127.0.0.1:19191/v1"\n', encoding="utf-8"
            )
            unmanaged_profile.chmod(0o600)
            unmanaged_command = command.copy()
            unmanaged_command[unmanaged_command.index(str(token_path))] = str(unmanaged_token)
            unmanaged = subprocess.run(
                unmanaged_command,
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"HOME": str(unmanaged_home)},
            )
            self.assertNotEqual(unmanaged.returncode, 0)
            self.assertIn("is not owned by opencodex-relay", unmanaged.stderr)
            self.assertEqual(
                unmanaged_profile.read_text(encoding="utf-8"),
                'openai_base_url = "http://127.0.0.1:19191/v1"\n',
            )
            self.assertFalse(
                (unmanaged_home / ".local" / "lib" / "opencodex-relay" / "relay" / version).exists()
            )

            occupied_home = root / "occupied-home"
            occupied_home.mkdir()
            occupied_token_dir = occupied_home / ".config" / "opencodex-relay"
            occupied_token_dir.mkdir(parents=True)
            occupied_token = occupied_token_dir / "github-release.token"
            occupied_token.write_text("github_pat_example", encoding="utf-8")
            occupied_token.chmod(0o600)
            occupied_command = command.copy()
            occupied_command[occupied_command.index(str(token_path))] = str(occupied_token)
            occupied = subprocess.run(
                occupied_command,
                check=False,
                capture_output=True,
                text=True,
                env=environment
                | {
                    "HOME": str(occupied_home),
                    "FAKE_LAUNCHCTL_ACTIVE": "0",
                    "FAIL_INTERACTIVE_PORT_OCCUPIED": "1",
                },
            )
            self.assertNotEqual(occupied.returncode, 0)
            self.assertIn("interactive relay listener port is already occupied", occupied.stderr)
            self.assertFalse((occupied_token_dir / "relay.json").exists())
            self.assertFalse(
                (occupied_home / ".codex" / "opencodex-relay-interactive.config.toml").exists()
            )
            self.assertFalse(
                (occupied_home / ".local" / "lib" / "opencodex-relay" / "relay" / version).exists()
            )

    def test_macos_menu_bar_trust_anchor_is_pinned_across_distribution_boundaries(self) -> None:
        app_root = RELAY / "macos" / "OpenCodexRelay"
        resource_root = app_root / "Resources"

        for info_template in ("Info.plist", "Info.local-dev.plist"):
            with (resource_root / info_template).open("rb") as source:
                info = plistlib.load(source)
            self.assertEqual(info["OpenCodexTrustedCodexBundleIdentifier"], TRUSTED_CODEX_BUNDLE_ID)
            self.assertEqual(info["OpenCodexTrustedCodexTeamIdentifier"], TRUSTED_CODEX_TEAM_ID)
            self.assertEqual(info["CFBundleIconFile"], "AppIcon.icns")
            self.assertFalse(info["LSUIElement"])
            self.assertNotIn("OpenCodexHomebrewGuardDaemonPlist", info)
            self.assertNotIn("OpenCodexHomebrewGuardLegacyDaemonPlist", info)
            self.assertEqual(info["OpenCodexHomebrewGuardBackend"], "manual_admin")
            self.assertEqual(info["OpenCodexRuntimeMode"], "managed")
            self.assertEqual(
                info["OpenCodexHomebrewGuardInstallerExecutable"],
                "OpenCodexRelayHelperInstaller",
            )
            self.assertIn("OpenCodexHomebrewGuardMachService", info)
            self.assertIn("OpenCodexHomebrewGuardHelperRequirement", info)
            self.assertIn("OpenCodexHomebrewGuardHelperVersion", info)

        app_icon = resource_root / "AppIcon.icns"
        self.assertTrue(app_icon.is_file())
        self.assertEqual(app_icon.read_bytes()[:4], b"icns")
        self.assertTrue((resource_root / "AppIcon.png").is_file())

        scripts = (
            "build-release.sh",
            "build-local-dev.sh",
            "install-relay.sh",
            "install-local-dev.sh",
        )
        for script_name in scripts:
            script = (RELAY / "scripts" / script_name).read_text(encoding="utf-8")
            self.assertIn(
                f'readonly TRUSTED_CODEX_BUNDLE_ID="{TRUSTED_CODEX_BUNDLE_ID}"',
                script,
            )
            self.assertIn(
                f'readonly TRUSTED_CODEX_TEAM_ID="{TRUSTED_CODEX_TEAM_ID}"',
                script,
            )
            self.assertIn("AppIcon.icns", script)

        for script_name in ("install-relay.sh", "install-local-dev.sh"):
            script = (RELAY / "scripts" / script_name).read_text(encoding="utf-8")
            self.assertIn("visible in the Dock", script)

    def test_local_development_distribution_is_explicitly_isolated(self) -> None:
        builder = (RELAY / "scripts" / "build-local-dev.sh").read_text(encoding="utf-8")
        installer = (RELAY / "scripts" / "install-local-dev.sh").read_text(encoding="utf-8")
        service = (RELAY / "scripts" / "install-local-dev-service.sh").read_text(encoding="utf-8")
        production_installer = (RELAY / "scripts" / "install-relay.sh").read_text(encoding="utf-8")
        dev_plist = (RELAY / "macos" / "io.github.novelkr.opencodex-relay.dev.plist.in").read_text(encoding="utf-8")
        dev_info = (RELAY / "macos" / "OpenCodexRelay" / "Resources" / "Info.local-dev.plist").read_text(encoding="utf-8")
        relayctl = (RELAY / "cmd" / "opencodex-relayctl" / "main.go").read_text(encoding="utf-8")
        native_restore = (RELAY / "internal" / "handoff" / "native_restore.go").read_text(encoding="utf-8")
        native_repair = (RELAY / "internal" / "routing" / "native_repair.go").read_text(encoding="utf-8")
        dev_installer = (
            RELAY
            / "macos"
            / "OpenCodexRelay"
            / "Sources"
            / "OpenCodexRelayHelperInstallerCore"
            / "HelperInstaller.swift"
        ).read_text(encoding="utf-8")
        dev_installer_main = (
            RELAY
            / "macos"
            / "OpenCodexRelay"
            / "Sources"
            / "OpenCodexRelayHelperInstaller"
            / "main.swift"
        ).read_text(encoding="utf-8")

        self.assertIn("clean Git worktree", builder)
        self.assertIn("GOPROXY=off", builder)
        self.assertIn("codesign --force --sign -", builder)
        self.assertNotRegex(builder, r"(?m)^\s*(?:xcrun\s+)?notarytool\b")
        self.assertNotRegex(builder, r"(?m)^\s*(?:xcrun\s+)?stapler\b")
        self.assertNotRegex(builder, r"(?m)^\s*spctl\b")
        self.assertIn('"distribution":"local_development"', builder)
        self.assertIn("--acknowledge-unsigned-local-build", installer)
        self.assertIn("--acknowledge-local-development-source", installer)
        self.assertIn("distribution=local_development", installer)
        self.assertNotIn("distribution=unsigned_ad_hoc", installer)
        self.assertIn("--acknowledge-local-source", installer)
        self.assertIn("mode seed-native", installer)
        self.assertIn("mode inspect-native-repair", installer)
        self.assertIn("inspect-native-repair-owner", relayctl)
        self.assertIn("native_owner_busy", relayctl)
        self.assertIn("native_owner_configuration_invalid", relayctl)
        self.assertIn("native_owner_restore_failed", relayctl)
        self.assertIn("native_owner_result_invalid", relayctl)
        self.assertIn("retryable_no_mutation", native_restore)
        self.assertIn("200 * time.Millisecond", native_repair)
        self.assertIn("500 * time.Millisecond", native_repair)
        self.assertIn("OwnerRestoreAttempts", native_repair)
        self.assertIn("recovery_preserved", installer)
        self.assertIn('DEV_CONFIG_DIR="${HOME}/.config/opencodex-relay/relay-dev"', installer)
        self.assertIn('.local/lib/opencodex-relay/relay-dev', installer)
        self.assertIn("127.0.0.1:18190", installer)
        self.assertIn("127.0.0.1:18192", installer)
        self.assertIn("OpenCodexRelay Dev.app", installer)
        self.assertNotIn("release-base-url", installer)
        self.assertNotIn("github-repo", installer)
        self.assertNotIn("xattr", installer)
        self.assertIn("io.github.novelkr.opencodex-relay.dev", service)
        self.assertIn("io.github.novelkr.opencodex-relay.dev", dev_plist)
        self.assertIn("io.github.novelkr.opencodex-relay.dev", dev_info)
        self.assertIn("local_development", dev_info)
        self.assertIn("production installer refuses a local_development relay config", production_installer)
        self.assertIn('"schema":3', builder)
        self.assertIn("Contents/Library/HelperTools", builder)
        self.assertIn("--homebrew-guard-unregister", installer)
        self.assertIn("homebrew_guard_registration=recovery_required", installer)
        self.assertIn("require_manual_homebrew_guard_absent", production_installer)
        self.assertIn('PENDING_ROOT="${INSTALL_ROOT}/pending"', installer)
        self.assertIn("prepare_manual_helper_candidate", installer)
        self.assertIn("pending_candidate_matches", installer)
        self.assertIn("bundle_metadata_manifest", installer)
        self.assertIn("bundle_metadata_matches", installer)
        self.assertIn("Contents/Library/Helpers/opencodex-relay", installer)
        self.assertIn("Contents/Library/Helpers/opencodex-relayctl", installer)
        self.assertIn("same install command", installer)
        install_body = installer[installer.index("install_local_dev()") :]
        self.assertLess(
            install_body.index("reserve_local_dev_source_install_lifecycle"),
            install_body.index("prepare_manual_helper_candidate"),
        )
        self.assertLess(
            install_body.index("prepare_manual_helper_candidate"),
            install_body.index('ensure_local_dev_config_parent "$config_path"'),
        )
        self.assertIn('[[ "$relay_bin" == "${INSTALL_ROOT}/current/"* ]]', service)
        self.assertIn('case failurePhase = "failure_phase"', dev_installer)
        self.assertIn('case failureReason = "failure_reason"', dev_installer)
        self.assertIn('case rollbackResult = "rollback_result"', dev_installer)
        self.assertIn("daemonStartRejected", dev_installer)
        self.assertIn("probeTimeout", dev_installer)
        self.assertIn("HelperInstallerDiagnostics.receipt", dev_installer_main)
        self.assertIn("diagnosedPreflight", dev_installer_main)
        self.assertIn("helperVersion = artifacts.helperVersion", dev_installer_main)

        self.assertIn("snapshot_local_source_file", installer)
        self.assertIn("cp -pP --", installer)
        self.assertIn("source_snapshot", installer)
        self.assertIn("mode verify-native", installer)
        self.assertIn("orphaned without its config", installer)
        self.assertIn("local_dev_service_is_active", installer)
        self.assertIn("require_local_dev_config_path", installer)
        self.assertIn("ensure_local_dev_config_parent", installer)
        self.assertGreaterEqual(installer.count("mode verify-native"), 2)
        self.assertIn('"$SERVICE_HELPER" stop', installer)
        self.assertIn('relay_dev_service_active=true manager=launchd', installer)
        self.assertIn("prior service could not be restored", installer)
        self.assertIn("${config}.runtime-maintenance.json", installer)
        self.assertIn("${config_path}.runtime-maintenance.json", installer)
        self.assertIn("runtime maintenance must be recovered", installer)
        self.assertIn("snapshot_runtime_maintenance_absence", installer)
        self.assertIn("verify_runtime_maintenance_absence_snapshot", installer)
        self.assertIn("it was retained for recovery", installer)
        self.assertNotIn(
            'snapshot_file "${config_path}.runtime-maintenance.json"',
            installer,
        )
        self.assertNotIn(
            "runtime-maintenance.json|${transaction_dir}/runtime-maintenance|runtime maintenance journal",
            installer,
        )
        self.assertIn('select(.schema_version == 4 and .phase == "recovery_required"', installer)

    def test_macos_menu_bar_localization_resources_are_staged_before_signing(self) -> None:
        app_root = RELAY / "macos" / "OpenCodexRelay"
        package = (app_root / "Package.swift").read_text(encoding="utf-8")
        production_builder = (RELAY / "scripts" / "build-release.sh").read_text(encoding="utf-8")
        development_builder = (RELAY / "scripts" / "build-local-dev.sh").read_text(encoding="utf-8")
        localization_source = (
            app_root / "Sources" / "OpenCodexRelayLocalization" / "RelayLocalization.swift"
        ).read_text(encoding="utf-8")

        self.assertIn('defaultLocalization: "ko"', package)
        self.assertIn('name: "OpenCodexRelayLocalization"', package)
        self.assertIn('"OpenCodexRelayLocalization",', package)
        self.assertIn("AppLanguageSelection", localization_source)
        self.assertIn("LocalizationStore", localization_source)
        self.assertIn("AppLocalizer", localization_source)
        self.assertIn("AppLanguageDescriptor", localization_source)

        for locale in ("en", "ko"):
            catalog = (
                app_root
                / "Sources"
                / "OpenCodexRelayLocalization"
                / "Resources"
                / f"{locale}.lproj"
                / "Localizable.strings"
            )
            self.assertTrue(catalog.is_file())
            self.assertIn('"language.label"', catalog.read_text(encoding="utf-8"))
            for flavor in ("production", "local-dev"):
                info_strings = (
                    app_root
                    / "Resources"
                    / f"InfoPlist.{flavor}"
                    / f"{locale}.lproj"
                    / "InfoPlist.strings"
                )
                self.assertTrue(info_strings.is_file())

        for info_template in ("Info.plist", "Info.local-dev.plist"):
            info = (app_root / "Resources" / info_template).read_text(encoding="utf-8")
            self.assertIn("CFBundleLocalizations", info)
            self.assertIn("<string>en</string>", info)
            self.assertIn("<string>ko</string>", info)

        self.assertIn(
            'ditto "$localization_bundle" "${app_resources}/${MACOS_LOCALIZATION_BUNDLE}"',
            production_builder,
        )
        self.assertIn(
            'ditto "$localization_bundle" "${app_dir}/Contents/Resources/${LOCALIZATION_BUNDLE}"',
            development_builder,
        )
        for builder in (production_builder, development_builder):
            self.assertIn("OpenCodexRelay_OpenCodexRelayLocalization.bundle", builder)
            self.assertIn("en.lproj/Localizable.strings", builder)
            self.assertIn("ko.lproj/Localizable.strings", builder)
            self.assertLess(
                builder.index("Localizable.strings"),
                builder.rindex("codesign --force --sign")
            )

    def test_local_development_bundle_stages_localization_resources_under_contents_resources(
        self,
    ) -> None:
        """Exercise the shipped dev archive layout without macOS signing tools."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake_bin = root / "bin"
            swift_bin = root / "swift-bin"
            output = root / "output"
            fake_bin.mkdir()

            tools = {
                "git": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "if [[ \" $* \" == *\" status \"* ]]; then exit 0; fi\n"
                    "if [[ \" $* \" == *\" rev-parse \"* ]]; then\n"
                    "  printf '0123456789abcdef0123456789abcdef01234567\\n'\n"
                    "  exit 0\n"
                    "fi\n"
                    "exit 2\n"
                ),
                "uname": (
                    "#!/usr/bin/env bash\n"
                    "case \"${1:-}\" in\n"
                    "  -s) printf 'Darwin\\n' ;;\n"
                    "  -m) printf 'arm64\\n' ;;\n"
                    "  *) exit 2 ;;\n"
                    "esac\n"
                ),
                "go": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "output=\n"
                    "while [[ $# -gt 0 ]]; do\n"
                    "  if [[ $1 == -o ]]; then output=$2; shift 2; else shift; fi\n"
                    "done\n"
                    "[[ -n $output ]]\n"
                    "printf '#!/bin/sh\\nexit 0\\n' > \"$output\"\n"
                ),
                "swift": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "mkdir -p \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/en.lproj\" \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/ko.lproj\"\n"
                    "printf '#!/bin/sh\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay\"\n"
                    "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelay\"\n"
                    "printf '#!/bin/sh\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelayPrivilegedHelper\"\n"
                    "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelayPrivilegedHelper\"\n"
                    "printf '#!/bin/sh\\nexit 0\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelayHelperInstaller\"\n"
                    "chmod 755 \"$FAKE_SWIFT_BIN/OpenCodexRelayHelperInstaller\"\n"
                    "printf '\\\"language.label\\\" = \\\"Language\\\";\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/en.lproj/Localizable.strings\"\n"
                    "printf '\\\"language.label\\\" = \\\"언어\\\";\\n' > \"$FAKE_SWIFT_BIN/OpenCodexRelay_OpenCodexRelayLocalization.bundle/ko.lproj/Localizable.strings\"\n"
                    "if [[ \" $* \" == *\" --show-bin-path \"* ]]; then printf '%s\\n' \"$FAKE_SWIFT_BIN\"; fi\n"
                ),
                "codesign": (
                    "#!/usr/bin/env bash\n"
                    "if [[ \" $* \" == *\" -dvvv \"* ]]; then printf 'CDHash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\n' >&2; fi\n"
                    "if [[ \" $* \" == *\" -dv \"* ]]; then\n"
                    "  if [[ \"${!#}\" == *OpenCodexRelayHelperInstaller ]]; then\n"
                    "    printf 'Identifier=io.github.novelkr.opencodex-relay.homebrew-guard.installer.dev\\n' >&2\n"
                    "  else\n"
                    "    printf 'Identifier=io.github.novelkr.opencodex-relay.homebrew-guard.helper.dev\\n' >&2\n"
                    "  fi\n"
                    "fi\n"
                    "exit 0\n"
                ),
                "plutil": "#!/usr/bin/env bash\nexit 0\n",
                "ditto": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "if [[ \"${1:-}\" == \"-c\" ]]; then\n"
                    "  while [[ $# -gt 2 ]]; do shift; done\n"
                    "  source=\"$1\"\n"
                    "  destination=\"$2\"\n"
                    "  (cd \"$(dirname \"$source\")\" && find \"$(basename \"$source\")\" -print | LC_ALL=C sort) > \"$destination\"\n"
                    "else\n"
                    "  cp -R \"$1\" \"$2\"\n"
                    "fi\n"
                ),
            }
            for name, content in tools.items():
                path = fake_bin / name
                path.write_text(content, encoding="utf-8")
                path.chmod(0o755)

            signing_key = root / "local-dev-ed25519.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(signing_key)],
                check=True,
                capture_output=True,
            )
            result = subprocess.run(
                [
                    "bash",
                    str(RELAY / "scripts" / "build-local-dev.sh"),
                    "1.2.3",
                    "--signing-key",
                    str(signing_key),
                    "--output",
                    str(output),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "FAKE_SWIFT_BIN": str(swift_bin),
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            archive_entries = (output / "OpenCodexRelay Dev.app.zip").read_text(
                encoding="utf-8"
            ).splitlines()
            bundle_root = (
                "OpenCodexRelay Dev.app/Contents/Resources/"
                "OpenCodexRelay_OpenCodexRelayLocalization.bundle"
            )
            self.assertIn(f"{bundle_root}/en.lproj/Localizable.strings", archive_entries)
            self.assertIn(f"{bundle_root}/ko.lproj/Localizable.strings", archive_entries)
            self.assertNotIn(
                "OpenCodexRelay Dev.app/"
                "OpenCodexRelay_OpenCodexRelayLocalization.bundle"
                "/ko.lproj/Localizable.strings",
                archive_entries,
            )
            local_manifest = json.loads(
                (output / "local-dev-manifest-1.2.3.json").read_text(encoding="utf-8")
            )
            self.assertEqual(local_manifest["schema"], 3)
            self.assertIn(
                "OpenCodexRelay Dev.app/Contents/Library/HelperTools/"
                "OpenCodexRelayPrivilegedHelper",
                archive_entries,
            )
            self.assertNotIn(
                "OpenCodexRelay Dev.app/Contents/Library/LaunchDaemons/"
                "io.github.novelkr.opencodex-relay.homebrew-guard.dev.plist",
                archive_entries,
            )
            self.assertIn(
                "OpenCodexRelay Dev.app/Contents/Library/Helpers/"
                "OpenCodexRelayHelperInstaller",
                archive_entries,
            )

    def test_local_development_uninstall_fails_closed_without_config_or_helper(self) -> None:
        installer = RELAY / "scripts" / "install-local-dev.sh"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            home.mkdir()
            env = os.environ | {"HOME": str(home)}
            install_root = home / ".local" / "lib" / "opencodex-relay" / "relay-dev"
            install_root.mkdir(parents=True)

            orphan = subprocess.run(
                ["bash", str(installer), "uninstall"],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )
            self.assertNotEqual(orphan.returncode, 0)
            self.assertIn("orphaned without its config", orphan.stderr)
            self.assertTrue(install_root.is_dir())

            config = home / ".config" / "opencodex-relay" / "relay-dev" / "relay.json"
            config.parent.mkdir(parents=True)
            config.write_text('{"installation_scope":"local_development"}\n', encoding="utf-8")
            config.chmod(0o600)
            missing_helper = subprocess.run(
                ["bash", str(installer), "uninstall"],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )
            self.assertNotEqual(missing_helper.returncode, 0)
            self.assertIn("current helper link is unavailable", missing_helper.stderr)
            self.assertTrue(config.is_file())
            self.assertTrue(install_root.is_dir())

            symlink_home = root / "symlink-home"
            production_config_root = root / "production-config-root"
            (symlink_home / ".config" / "opencodex-relay").mkdir(parents=True)
            production_config_root.mkdir()
            (symlink_home / ".config" / "opencodex-relay" / "relay-dev").symlink_to(
                production_config_root, target_is_directory=True
            )
            symlinked = subprocess.run(
                ["bash", str(installer), "uninstall"],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ | {"HOME": str(symlink_home)},
            )
            self.assertNotEqual(symlinked.returncode, 0)
            self.assertIn("local development config parent is unsafe", symlinked.stderr)
            self.assertTrue((symlink_home / ".config" / "opencodex-relay" / "relay-dev").is_symlink())
            self.assertEqual(list(production_config_root.iterdir()), [])

    @unittest.skipUnless(os.uname().sysname == "Darwin", "pending helper fixture requires macOS tools")
    def test_local_development_helper_candidate_is_resumable_before_app_mutation(self) -> None:
        installer = RELAY / "scripts" / "install-local-dev.sh"
        version = "1.2.3-relay-preserve.6"
        artifact_hash = "a" * 64
        app_name = "OpenCodexRelay Dev.app"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            fake_bin = root / "bin"
            home.mkdir()
            fake_bin.mkdir()
            ready_marker = root / "ready"
            current_marker = home / "current-app-unchanged"
            current_marker.write_text("current\n", encoding="utf-8")

            library = root / "installer-functions.sh"
            source = installer.read_text(encoding="utf-8")
            library.write_text(
                source.split('\naction="${1:-}"\n', maxsplit=1)[0] + "\n",
                encoding="utf-8",
            )

            codesign = fake_bin / "codesign"
            codesign.write_text(
                "#!/bin/sh\n"
                "if printf '%s\\n' \"$*\" | grep -q -- '-dvvv'; then\n"
                "  printf '%s\\n' 'CDHash=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >&2\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            codesign.chmod(0o700)

            def make_candidate(path: Path) -> Path:
                app = path / app_name
                main = app / "Contents" / "MacOS" / "OpenCodexRelay"
                helper = (
                    app
                    / "Contents"
                    / "Library"
                    / "HelperTools"
                    / "OpenCodexRelayPrivilegedHelper"
                )
                bundled_installer = (
                    app
                    / "Contents"
                    / "Library"
                    / "Helpers"
                    / "OpenCodexRelayHelperInstaller"
                )
                relay = app / "Contents" / "Library" / "Helpers" / "opencodex-relay"
                relayctl = (
                    app / "Contents" / "Library" / "Helpers" / "opencodex-relayctl"
                )
                for executable in (main, relay, relayctl, helper, bundled_installer):
                    executable.parent.mkdir(parents=True, exist_ok=True)
                    executable.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
                    executable.chmod(0o700)
                main.write_text(
                    "#!/bin/sh\n"
                    "if [ -f \"$READY_MARKER\" ]; then\n"
                    "  printf '%s\\n' homebrew_guard_registration=ready\n"
                    "else\n"
                    "  printf '%s\\n' homebrew_guard_registration=manual_install_required\n"
                    "fi\n",
                    encoding="utf-8",
                )
                main.chmod(0o700)
                return app

            first_root = root / "first"
            first_root.mkdir()
            first_app = make_candidate(first_root)
            environment = os.environ | {
                "HOME": str(home),
                "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                "READY_MARKER": str(ready_marker),
            }
            first = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; app="$2"; prepare_manual_helper_candidate "$3" "$2" "$4"',
                    "fixture",
                    str(library),
                    str(first_app),
                    version,
                    artifact_hash,
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(first.returncode, 75, first.stderr)
            self.assertIn("local_dev_install=pending", first.stdout)
            self.assertIn("sudo", first.stdout)
            pending = (
                home
                / ".local"
                / "lib"
                / "opencodex-relay"
                / "relay-dev"
                / "pending"
                / version
            )
            self.assertTrue((pending / app_name).is_dir())
            self.assertEqual(current_marker.read_text(encoding="utf-8"), "current\n")

            ready_marker.touch()
            second_root = root / "second"
            second_root.mkdir()
            second_app = make_candidate(second_root)
            second = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; app="$2"; prepare_manual_helper_candidate "$3" "$2" "$4"; printf "candidate=%s\\n" "$app"',
                    "fixture",
                    str(library),
                    str(second_app),
                    version,
                    artifact_hash,
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertIn(f"candidate={pending / app_name}", second.stdout)
            self.assertTrue(second_app.is_dir())
            self.assertEqual(current_marker.read_text(encoding="utf-8"), "current\n")

            executable_relatives = (
                Path("Contents/MacOS/OpenCodexRelay"),
                Path("Contents/Library/Helpers/opencodex-relay"),
                Path("Contents/Library/Helpers/opencodex-relayctl"),
                Path("Contents/Library/HelperTools/OpenCodexRelayPrivilegedHelper"),
                Path("Contents/Library/Helpers/OpenCodexRelayHelperInstaller"),
            )
            for index, relative in enumerate(executable_relatives):
                pending_executable = pending / app_name / relative
                pending_executable.chmod(0o600)
                mode_root = root / f"mode-{index}"
                mode_root.mkdir()
                mode_app = make_candidate(mode_root)
                byte_comparison = subprocess.run(
                    ["diff", "-qr", str(pending / app_name), str(mode_app)],
                    check=False,
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(byte_comparison.returncode, 0, byte_comparison.stderr)

                rejected = subprocess.run(
                    [
                        "bash",
                        "-c",
                        'source "$1"; app="$2"; prepare_manual_helper_candidate "$3" "$2" "$4"',
                        "fixture",
                        str(library),
                        str(mode_app),
                        version,
                        artifact_hash,
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=environment,
                )
                self.assertNotEqual(rejected.returncode, 0)
                self.assertIn(
                    "does not match the verified source artifact", rejected.stderr
                )
                self.assertEqual(
                    current_marker.read_text(encoding="utf-8"), "current\n"
                )

                pending_executable.chmod(0o700)
                resumed = subprocess.run(
                    [
                        "bash",
                        "-c",
                        'source "$1"; app="$2"; prepare_manual_helper_candidate "$3" "$2" "$4"',
                        "fixture",
                        str(library),
                        str(mode_app),
                        version,
                        artifact_hash,
                    ],
                    check=False,
                    capture_output=True,
                    text=True,
                    env=environment,
                )
                self.assertEqual(resumed.returncode, 0, resumed.stderr)

            changed_root = root / "changed"
            changed_root.mkdir()
            changed_app = make_candidate(changed_root)
            changed_resource = changed_app / "Contents" / "Resources" / "changed.txt"
            changed_resource.parent.mkdir(parents=True)
            changed_resource.write_text("changed\n", encoding="utf-8")
            changed = subprocess.run(
                [
                    "bash",
                    "-c",
                    'source "$1"; app="$2"; prepare_manual_helper_candidate "$3" "$2" "$4"',
                    "fixture",
                    str(library),
                    str(changed_app),
                    version,
                    artifact_hash,
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(changed.returncode, 0)
            self.assertIn("does not match the verified source artifact", changed.stderr)
            self.assertEqual(current_marker.read_text(encoding="utf-8"), "current\n")

            symlink_home = root / "symlink-home"
            external_root = root / "external-root"
            symlink_home.mkdir()
            external_root.mkdir()
            (symlink_home / ".local").symlink_to(external_root, target_is_directory=True)
            unsafe = subprocess.run(
                ["bash", "-c", 'source "$1"; ensure_local_dev_install_root', "fixture", str(library)],
                check=False,
                capture_output=True,
                text=True,
                env=environment | {"HOME": str(symlink_home)},
            )
            self.assertNotEqual(unsafe.returncode, 0)
            self.assertIn("install root parent is unsafe", unsafe.stderr)
            self.assertFalse((external_root / "lib").exists())

    @unittest.skipUnless(os.uname().sysname == "Darwin", "local-dev bundle fixture requires macOS tools")
    def test_local_development_installer_uses_frozen_source_snapshot(self) -> None:
        installer = RELAY / "scripts" / "install-local-dev.sh"
        version = "1.2.3"
        app_name = "OpenCodexRelay Dev.app"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            home = root / "home"
            source = root / "source"
            fake_bin = root / "bin"
            tmp_dir = root / "tmp"
            for path in (home, source, fake_bin, tmp_dir):
                path.mkdir()

            app = source / app_name
            helpers = app / "Contents" / "Library" / "Helpers"
            helpers.mkdir(parents=True)
            (app / "Contents" / "MacOS").mkdir()
            resources = app / "Contents" / "Resources"
            resources.mkdir()
            (resources / "AppIcon.icns").write_bytes(b"icnsfixture")
            runtime_trust = resources / "RuntimeTrust"
            runtime_trust.mkdir()
            shutil.copyfile(
                REPO_ROOT / "config" / "trust" / "opencodex-runtime-release-ed25519.pub",
                runtime_trust / "opencodex-runtime-release-ed25519.pub",
            )
            (app / "Contents" / "Info.plist").write_text(
                "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n"
                "<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" "
                "\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n"
                "<plist version=\"1.0\"><dict>\n"
                "<key>CFBundleIdentifier</key><string>io.github.novelkr.opencodex-relay.dev</string>\n"
                "<key>CFBundleIconFile</key><string>AppIcon.icns</string>\n"
                "<key>LSUIElement</key><false/>\n"
                "<key>OpenCodexDistributionFlavor</key><string>local_development</string>\n"
                "<key>OpenCodexTrustedCodexBundleIdentifier</key><string>com.openai.codex</string>\n"
                "<key>OpenCodexTrustedCodexTeamIdentifier</key><string>2DC432GLL2</string>\n"
                "</dict></plist>\n",
                encoding="utf-8",
            )
            executable_files = {
                app / "Contents" / "MacOS" / "OpenCodexRelay": "#!/bin/sh\nprintf original-app\\n\n",
                helpers / "opencodex-relay": "#!/bin/sh\nexit 0\n",
                helpers / "opencodex-relayctl": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "if [[ \"${1:-}\" == lifecycle ]]; then\n"
                    "  case \"${2:-}\" in\n"
                    "    source-install-capability) printf '%s\\n' '{\"schema_version\":2,\"state\":\"ready\"}' ;;\n"
                    "    reserve-source-install)\n"
                    "      recovery_file=\n"
                    "      while [[ $# -gt 0 ]]; do\n"
                    "        if [[ \"$1\" == --recovery-file ]]; then recovery_file=\"$2\"; shift 2; else shift; fi\n"
                    "      done\n"
                    "      [[ -n \"$recovery_file\" ]]\n"
                    "      reservation_root=\"$HOME/.local/lib/opencodex-relay/relay-dev\"\n"
                    "      mkdir -p \"$reservation_root\"; chmod 700 \"$reservation_root\"\n"
                    "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"local_development\",\"token\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"root_created\":true}' > \"$recovery_file\"\n"
                    "      chmod 600 \"$recovery_file\"\n"
                    "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"local_development\",\"token\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"}' > \"$reservation_root/.source-install-reservation.json\"\n"
                    "      chmod 600 \"$reservation_root/.source-install-reservation.json\"\n"
                    "      printf '%s\\n' '{\"schema_version\":1,\"scope\":\"local_development\",\"token\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"root_created\":true}' ;;\n"
                    "    release-source-install)\n"
                    "      rm -f \"$HOME/.local/lib/opencodex-relay/relay-dev/.source-install-reservation.json\"\n"
                    "      printf '%s\\n' '{\"schema_version\":1,\"state\":\"released\"}' ;;\n"
                    "    *) exit 64 ;;\n"
                    "  esac\n"
                    "  exit 0\n"
                    "fi\n"
                    "if [[ \"${1:-}\" == init ]]; then\n"
                    "  shift\n"
                    "  config= upstream= catalog=\n"
                    "  while [[ $# -gt 0 ]]; do\n"
                    "    case \"$1\" in\n"
                    "      --config) config=\"$2\"; shift 2 ;;\n"
                    "      --upstream) upstream=\"$2\"; shift 2 ;;\n"
                    "      --catalog-path) catalog=\"$2\"; shift 2 ;;\n"
                    "      *) shift ;;\n"
                    "    esac\n"
                    "  done\n"
                    "  mkdir -p \"$(dirname -- \"$config\")\"\n"
                    "  printf '{\\\"installation_scope\\\":\\\"local_development\\\",\\\"listen_address\\\":\\\"127.0.0.1:18190\\\",\\\"responses\\\":{\\\"scheduler\\\":{\\\"interactive_listen_address\\\":\\\"127.0.0.1:18192\\\"}},\\\"upstream_mode\\\":\\\"external_gateway\\\",\\\"upstream_base_url\\\":\\\"%s\\\",\\\"catalog\\\":{\\\"path\\\":\\\"%s\\\"}}\\n' \"$upstream\" \"$catalog\" > \"$config\"\n"
                    "  chmod 600 \"$config\"\n"
                    "  exit 0\n"
                    "fi\n"
                    "if [[ \"${1:-}\" == mode ]]; then\n"
                    "  case \"${2:-}\" in\n"
                    "    seed-native|request|apply) exit 0 ;;\n"
                    "    status) printf '{\\\"applied_backend\\\":\\\"none\\\"}\\n'; exit 0 ;;\n"
                    "    verify-native)\n"
                    "      counter=\"${FAKE_VERIFY_COUNTER:-}\"\n"
                    "      if [[ -n \"$counter\" ]]; then\n"
                    "        count=0; [[ ! -f \"$counter\" ]] || count=\"$(cat \"$counter\")\"\n"
                    "        count=$((count + 1)); printf '%s\\n' \"$count\" > \"$counter\"\n"
                    "        [[ \"${FAKE_VERIFY_FAIL_ON_SECOND:-0}\" != 1 || \"$count\" -lt 2 ]] || exit 65\n"
                    "      fi\n"
                    "      printf '{}\\n'; exit 0 ;;\n"
                    "  esac\n"
                    "fi\n"
                    "exit 64\n"
                ),
            }
            for path, content in executable_files.items():
                path.write_text(content, encoding="utf-8")
                path.chmod(0o700)

            bundle = source / f"{app_name}.zip"
            subprocess.run(
                ["ditto", "-c", "-k", "--keepParent", str(app), str(bundle)],
                check=True,
                capture_output=True,
                text=True,
            )
            notices = source / "THIRD_PARTY_NOTICES.md"
            notices.write_text("original notices\n", encoding="utf-8")

            private_key = root / "private.pem"
            public_key = source / "local-dev-public-key.pem"
            subprocess.run(
                ["openssl", "genpkey", "-algorithm", "Ed25519", "-out", str(private_key)],
                check=True,
                capture_output=True,
                text=True,
            )
            subprocess.run(
                ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
                check=True,
                capture_output=True,
                text=True,
            )
            manifest_path = source / f"local-dev-manifest-{version}.json"
            manifest = {
                "schema": 1,
                "distribution": "local_development",
                "version": version,
                "source_commit": "0" * 40,
                "artifacts": [
                    {
                        "os": "darwin",
                        "arch": "arm64",
                        "component": "macos_menu_bar_bundle",
                        "file": bundle.name,
                        "bundle_id": "io.github.novelkr.opencodex-relay.dev",
                        "sha256": hashlib.sha256(bundle.read_bytes()).hexdigest(),
                    }
                ],
                "documents": [
                    {
                        "file": notices.name,
                        "sha256": hashlib.sha256(notices.read_bytes()).hexdigest(),
                    }
                ],
            }
            manifest_path.write_text(json.dumps(manifest, separators=(",", ":")) + "\n", encoding="utf-8")
            signed = subprocess.run(
                ["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(private_key), "-in", str(manifest_path)],
                check=True,
                capture_output=True,
            )
            (source / f"local-dev-manifest-{version}.sig").write_text(
                base64.b64encode(signed.stdout).decode("ascii") + "\n", encoding="utf-8"
            )

            mutation_marker = root / "source-mutated"
            real_jq = shutil.which("jq")
            self.assertIsNotNone(real_jq)
            fake_commands = {
                "codesign": "#!/bin/sh\nexit 0\n",
                "curl": (
                    "#!/bin/sh\n"
                    "printf '%s\\n' '{\"ok\":true,\"relay_admission\":\"deny\",\"catalog_refresh\":\"pause\"}'\n"
                ),
                "launchctl": (
                    "#!/bin/sh\n"
                    "if [ -n \"${FAKE_LAUNCHCTL_LOG:-}\" ]; then printf '%s\\n' \"$*\" >> \"$FAKE_LAUNCHCTL_LOG\"; fi\n"
                    "exit 0\n"
                ),
                "jq": (
                    "#!/usr/bin/env bash\n"
                    "set -euo pipefail\n"
                    "if [[ ! -e \"$MUTATION_MARKER\" ]]; then\n"
                    "  : > \"$MUTATION_MARKER\"\n"
                    f"  printf '{{}}\\n' > \"$SOURCE_DIR/local-dev-manifest-{version}.json\"\n"
                    f"  printf tampered > \"$SOURCE_DIR/{bundle.name}\"\n"
                    "  printf tampered > \"$SOURCE_DIR/THIRD_PARTY_NOTICES.md\"\n"
                    "fi\n"
                    "exec \"$REAL_JQ\" \"$@\"\n"
                ),
            }
            for name, content in fake_commands.items():
                path = fake_bin / name
                path.write_text(content, encoding="utf-8")
                path.chmod(0o700)

            config = home / ".config" / "opencodex-relay" / "relay-dev" / "nested" / "relay.json"
            result = subprocess.run(
                [
                    "bash",
                    str(installer),
                    "install",
                    version,
                    "--source-dir",
                    str(source),
                    "--upstream",
                    "https://example.test/v1",
                    "--acknowledge-unsigned-local-build",
                    "--acknowledge-local-source",
                    "--config",
                    str(config),
                ],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "HOME": str(home),
                    "TMPDIR": str(tmp_dir),
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "REAL_JQ": real_jq or "",
                    "SOURCE_DIR": str(source),
                    "MUTATION_MARKER": str(mutation_marker),
                },
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(mutation_marker.is_file())
            self.assertEqual(manifest_path.read_text(encoding="utf-8"), "{}\n")
            self.assertEqual(config.parent.stat().st_mode & 0o077, 0)
            installed_app = (
                home
                / ".local"
                / "lib"
                / "opencodex-relay"
                / "relay-dev"
                / version
                / "darwin-arm64"
                / app_name
            )
            self.assertEqual(
                (installed_app / "Contents" / "MacOS" / "OpenCodexRelay").read_text(encoding="utf-8"),
                "#!/bin/sh\nprintf original-app\\n\n",
            )
            self.assertEqual(list(tmp_dir.glob("opencodex-relay-local-dev.*")), [])

            install_root = home / ".local" / "lib" / "opencodex-relay" / "relay-dev"
            launchctl_log = root / "launchctl.log"
            verify_counter = root / "verify-counter"
            failed_uninstall = subprocess.run(
                ["bash", str(installer), "uninstall", "--config", str(config)],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "HOME": str(home),
                    "TMPDIR": str(tmp_dir),
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "REAL_JQ": real_jq or "",
                    "SOURCE_DIR": str(source),
                    "MUTATION_MARKER": str(mutation_marker),
                    "FAKE_LAUNCHCTL_LOG": str(launchctl_log),
                    "FAKE_VERIFY_COUNTER": str(verify_counter),
                    "FAKE_VERIFY_FAIL_ON_SECOND": "1",
                },
            )
            self.assertNotEqual(failed_uninstall.returncode, 0)
            self.assertIn("not verified native after service stop", failed_uninstall.stderr)
            self.assertTrue(config.is_file())
            self.assertTrue(install_root.is_dir())
            self.assertGreaterEqual(
                sum(call.startswith("bootstrap ") for call in launchctl_log.read_text(encoding="utf-8").splitlines()),
                1,
            )

            completed_uninstall = subprocess.run(
                ["bash", str(installer), "uninstall", "--config", str(config)],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "HOME": str(home),
                    "TMPDIR": str(tmp_dir),
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "REAL_JQ": real_jq or "",
                    "SOURCE_DIR": str(source),
                    "MUTATION_MARKER": str(mutation_marker),
                    "FAKE_LAUNCHCTL_LOG": str(launchctl_log),
                },
            )
            self.assertEqual(completed_uninstall.returncode, 0, completed_uninstall.stderr)
            self.assertFalse(config.exists())
            self.assertFalse(install_root.exists())


if __name__ == "__main__":
    unittest.main()

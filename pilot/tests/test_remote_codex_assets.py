#!/usr/bin/env python3

import hashlib
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
REMOTE_CONFIG_LOADER = PILOT / "scripts" / "load-remote-config.sh"
INSTALLER = PILOT / "scripts" / "install-remote-codex-home.sh"
SCRIPTS = (
    PILOT / "scripts" / "codex-remote-home-wrapper.sh",
    PILOT / "scripts" / "configure-remote-codex-routing.sh",
    PILOT / "scripts" / "manage-remote-codex-home.sh",
    INSTALLER,
)
CATALOG_SERVICE = PILOT / "systemd" / "opencodex-remote-catalog-refresh.service"
WRAPPER_REPAIR_PATH = PILOT / "systemd" / "opencodex-remote-codex-wrapper-repair.path"
BARE_MODEL = "gpt-5.6-luna"
BOUNDED_MODEL = "opencode-go-responses/gpt-5.6-luna"

def remote_config_json(remote_home: Path, mode: str, routing_mode: str) -> str:
    return json.dumps({"api_origin": "https://gateway.example.test", "mode": mode, "remote_home": str(remote_home), "routing_mode": routing_mode, "schema_version": 1}, sort_keys=True) + "\n"


class RemoteCodexAssetsTests(unittest.TestCase):
    def create_local_relay_fixture(
        self,
        root: Path,
        model: str,
        catalog_models: object,
        model_modes: object,
    ) -> dict[str, object]:
        remote_home = root / "remote-home"
        config_dir = root / "config"
        install_root = root / "install"
        fake_bin = root / "bin"
        user_bin = root / "user-bin"
        for path in (remote_home, config_dir, install_root, fake_bin, user_bin):
            path.mkdir(parents=True)

        config_file = config_dir / "remote-opencodex.json"
        config_file.write_text(
            remote_config_json(remote_home, "loopback", "local-relay"),
            encoding="utf-8",
        )
        config_file.chmod(0o600)

        codex_config = remote_home / "config.toml"
        codex_config.write_text(
            f'model = "{model}"\n\n[projects."/home/ubuntu"]\ntrust_level = "untrusted"\n',
            encoding="utf-8",
        )
        codex_config.chmod(0o600)

        catalog = remote_home / "opencodex-catalog.json"
        catalog.write_text(json.dumps({"models": catalog_models}) + "\n", encoding="utf-8")
        catalog.chmod(0o600)

        relay_config = config_dir / "relay.json"
        relay_document = {
            "listen_address": "127.0.0.1:18180",
            "upstream_mode": "local_opencodex",
            "upstream_base_url": "http://127.0.0.1:10100/v1",
            "credentials": {"source": "none"},
            "responses": {
                "websocket_mode": "http_fallback",
                "model_modes": model_modes,
                "scheduler": {"interactive_listen_address": "127.0.0.1:18182"},
            },
            "catalog": {
                "owner": "remote_manager",
                "path": str(catalog),
                "manage_app_server": False,
            },
        }
        relay_config.write_text(json.dumps(relay_document) + "\n", encoding="utf-8")
        relay_config.chmod(0o600)

        profile = remote_home / "opencodex-relay-interactive.config.toml"
        profile.write_text(
            "# opencodex-relay-managed-interactive-profile-v1\n"
            'openai_base_url = "http://127.0.0.1:18182/v1"\n'
            f'model_catalog_json = "{catalog}"\n',
            encoding="utf-8",
        )
        profile.chmod(0o600)

        managed_log = root / "managed.log"
        daemon_json = root / "daemon.json"
        daemon_json.write_text(
            json.dumps(
                {
                    "status": "running",
                    "managedCodexVersion": "0.149.1",
                    "cliVersion": "0.149.1",
                    "appServerVersion": "0.149.1",
                }
            )
            + "\n",
            encoding="utf-8",
        )
        managed_codex = remote_home / "packages" / "standalone" / "current" / "codex"
        managed_codex.parent.mkdir(parents=True)
        managed_codex.write_text(
            "#!/usr/bin/env bash\n"
            "if [[ ${1:-} == --version ]]; then printf 'codex-cli 0.149.1\\n'; exit 0; fi\n"
            "if [[ ${1:-} == app-server && ${2:-} == daemon && ${3:-} == version ]]; then "
            "cat \"$FAKE_DAEMON_JSON\"; exit 0; fi\n"
            "printf '%s|%s\\n' \"${CODEX_HOME:-unset}\" \"$*\" >> \"$FAKE_MANAGED_LOG\"\n"
            "exit 73\n",
            encoding="utf-8",
        )

        relay = root / "relay"
        relay.write_text(
            "#!/usr/bin/env bash\n[[ \"$*\" == *\"--check\"* ]]\n",
            encoding="utf-8",
        )

        scheduler_limits = {
            "max_classifications": 8,
            "max_pending_requests": 24,
            "max_pending_encoded_bytes": 536870912,
            "queue_timeout_ms": 60000,
            "max_general_upstream": 4,
            "interactive_reserved_upstream": 1,
            "max_concurrent_transforms": 2,
            "max_open_deliveries": 16,
        }
        health_base = {
            "ok": True,
            "general_listener": "127.0.0.1:18180",
            "interactive_listener": "127.0.0.1:18182",
            "upstream_mode": "local_opencodex",
            "upstream_base_url": "http://127.0.0.1:10100/v1",
            "catalog_owner": "remote_manager",
            "responses_websocket_mode": "http_fallback",
            "responses_models": list(model_modes),
            "responses_normalizer": bool(model_modes),
            "active_requests": 0,
            "active_classifications": 0,
            "pending_requests": 0,
            "pending_encoded_bytes": 0,
            "active_general_upstream": 0,
            "active_interactive_upstream": 0,
            "active_transforms": 0,
            "active_deliveries": 0,
            "capacity_rejections": 0,
            "scheduler_limits": scheduler_limits,
        }
        general_health = root / "general-health.json"
        interactive_health = root / "interactive-health.json"
        general_health.write_text(
            json.dumps(health_base | {"listener_lane": "general"}) + "\n",
            encoding="utf-8",
        )
        interactive_health.write_text(
            json.dumps(health_base | {"listener_lane": "interactive"}) + "\n",
            encoding="utf-8",
        )

        (fake_bin / "id").write_text(
            "#!/usr/bin/env bash\nprintf 'ubuntu\\n'\n", encoding="utf-8"
        )
        (fake_bin / "stat").write_text(
            "#!/usr/bin/env bash\nprintf 'ubuntu:ubuntu:600\\n'\n", encoding="utf-8"
        )
        (fake_bin / "curl").write_text(
            "#!/usr/bin/env bash\n"
            "if [[ \"$*\" == *18182* ]]; then cat \"$FAKE_INTERACTIVE_HEALTH\"; "
            "else cat \"$FAKE_GENERAL_HEALTH\"; fi\n",
            encoding="utf-8",
        )

        manager = root / "manager.sh"
        manager_content = SCRIPTS[2].read_text(encoding="utf-8")
        replacements = {
            'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"': f'readonly REMOTE_HOME_PATH="{remote_home}"',
            'readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"': f'readonly CONFIG_DIR="{config_dir}"',
            'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"': f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
            'readonly INSTALL_ROOT="/home/ubuntu/.local/lib/opencodex-relay"': f'readonly INSTALL_ROOT="{install_root}"',
            'readonly WRAPPER_TARGET="/home/ubuntu/.local/bin/codex"': f'readonly WRAPPER_TARGET="{user_bin / "codex"}"',
            'readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"': f'readonly RELAY="{relay}"',
        }
        for before, after in replacements.items():
            self.assertIn(before, manager_content)
            manager_content = manager_content.replace(before, after)
        manager.write_text(manager_content, encoding="utf-8")

        for path in (
            managed_codex,
            relay,
            manager,
            fake_bin / "id",
            fake_bin / "stat",
            fake_bin / "curl",
        ):
            path.chmod(0o700)

        environment = os.environ | {
            "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
            "FAKE_DAEMON_JSON": str(daemon_json),
            "FAKE_GENERAL_HEALTH": str(general_health),
            "FAKE_INTERACTIVE_HEALTH": str(interactive_health),
            "FAKE_MANAGED_LOG": str(managed_log),
        }
        return {
            "catalog": catalog,
            "codex_config": codex_config,
            "config_dir": config_dir,
            "daemon_json": daemon_json,
            "environment": environment,
            "managed_log": managed_log,
            "manager": manager,
            "relay": relay,
            "remote_home": remote_home,
        }

    def run_fixture_manager(
        self, fixture: dict[str, object], *args: str
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(fixture["manager"]), *args],
            check=False,
            capture_output=True,
            text=True,
            env=fixture["environment"],
        )

    def test_scripts_are_valid_bash(self) -> None:
        result = subprocess.run(
            ["bash", "-n", *(str(path) for path in SCRIPTS)],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_changed_catalog_restarts_managed_app_server(self) -> None:
        content = CATALOG_SERVICE.read_text(encoding="utf-8")
        self.assertIn("manage-remote-codex-home.sh refresh --restart", content)

    def test_daemon_restart_waits_for_version_equality_before_clearing_pending(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        self.assertIn("manage-remote-codex-home.sh verify-daemon", manager)
        self.assertIn(".managedCodexVersion == $expected", manager)
        self.assertIn(".appServerVersion == $expected", manager)
        restart_block = manager[manager.index("restart_daemon() {"):manager.index("status() {")]
        self.assertLess(
            restart_block.index("wait_for_daemon_version"),
            restart_block.index("clear_catalog_restart_pending"),
        )

    def test_foreign_daemon_recovery_is_explicit_and_narrow(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        self.assertIn("recover-daemon --allow-remote-interruption", manager)
        self.assertIn("foreign_daemon_pids", manager)
        self.assertIn("app-server daemon pid-update-loop", manager)
        self.assertIn("app-server --listen unix://", manager)
        self.assertIn("refusing a broad process kill", manager)
        self.assertIn("refusing SIGKILL", manager)
        self.assertIn("app-server daemon bootstrap --remote-control", manager)

    def test_remote_home_prevents_ordinary_home_project_config_from_overriding_it(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        self.assertIn("isolate-home-project-config --allow-remote-interruption", manager)
        self.assertIn("verify-home-project-config", manager)
        self.assertIn('readonly REMOTE_CODEX_CONFIG="${REMOTE_HOME_PATH}/config.toml"', manager)
        self.assertIn('[projects.\\"/home/ubuntu\\"]', manager)
        self.assertIn('trust_level = \\"untrusted\\"', manager)
        self.assertIn("duplicate /home/ubuntu project sections", manager)

    def test_remote_home_has_an_explicit_native_luna_default(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        self.assertIn('readonly DEFAULT_CODEX_MODEL="gpt-5.6-luna"', manager)
        self.assertIn(
            'readonly DEFAULT_BOUNDED_CODEX_MODEL="opencode-go-responses/gpt-5.6-luna"',
            manager,
        )
        self.assertIn("set-default-model --allow-remote-interruption", manager)
        self.assertIn("verify-default-model", manager)
        self.assertIn("expected_default_model", manager)
        self.assertIn("relay_default_policy_state", manager)
        self.assertIn("local_relay_model_state", manager)
        self.assertIn("neither one bounded_json policy nor one exact catalog model", manager)
        self.assertIn("default_model_relay_mode", manager)
        self.assertIn("duplicate root model assignments", manager)
        self.assertNotRegex(manager, r'DEFAULT_CODEX_MODEL="cursor/')

    def test_local_relay_classifies_policy_and_catalog_models(self) -> None:
        cases = (
            (
                "bounded policy",
                BOUNDED_MODEL,
                [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                {"OpenCode-Go-Responses/GPT-5.6-Luna": "bounded_json"},
                "bounded_json",
            ),
            (
                "bare catalog passthrough",
                BARE_MODEL,
                [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "passthrough",
            ),
            (
                "qualified catalog passthrough",
                "openai/gpt-5.6-luna",
                [{"id": BOUNDED_MODEL}, {"slug": "openai/gpt-5.6-luna"}],
                {BOUNDED_MODEL: "bounded_json"},
                "passthrough",
            ),
        )
        for name, model, catalog_models, model_modes, relay_mode in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = self.create_local_relay_fixture(
                    Path(directory), model, catalog_models, model_modes
                )
                result = self.run_fixture_manager(fixture, "status")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"default_model={model}\n", result.stdout)
                self.assertIn(f"default_model_relay_mode={relay_mode}\n", result.stdout)
                self.assertIn("default_model_match=1\n", result.stdout)

    def test_local_relay_set_default_preserves_both_valid_modes(self) -> None:
        for model in (BARE_MODEL, BOUNDED_MODEL):
            with self.subTest(model=model), tempfile.TemporaryDirectory() as directory:
                fixture = self.create_local_relay_fixture(
                    Path(directory),
                    model,
                    [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                    {BOUNDED_MODEL: "bounded_json"},
                )
                codex_config = fixture["codex_config"]
                before = codex_config.read_bytes()
                before_mtime = codex_config.stat().st_mtime_ns
                result = self.run_fixture_manager(
                    fixture, "set-default-model", "--allow-remote-interruption"
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"default_model_changed=0 model={model}", result.stdout)
                self.assertEqual(codex_config.read_bytes(), before)
                self.assertEqual(codex_config.stat().st_mtime_ns, before_mtime)
                self.assertFalse(fixture["managed_log"].exists())
                self.assertEqual(
                    list(fixture["remote_home"].glob("config.toml.pre-default-model-*")),
                    [],
                )

    def test_local_relay_rejects_invalid_state_without_mutation(self) -> None:
        scenarios = (
            (
                "unknown model",
                "unknown-model",
                [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "neither one bounded_json policy nor one exact catalog model",
            ),
            (
                "catalog case mismatch",
                "GPT-5.6-LUNA",
                [{"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "neither one bounded_json policy nor one exact catalog model",
            ),
            (
                "duplicate policy",
                BARE_MODEL,
                [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json", BOUNDED_MODEL.upper(): "bounded_json"},
                "bounded_json policy is invalid",
            ),
            (
                "malformed policy",
                BARE_MODEL,
                [{"id": BARE_MODEL}],
                [],
                "bounded_json policy is invalid",
            ),
            (
                "duplicate catalog",
                BARE_MODEL,
                [{"id": BARE_MODEL}, {"slug": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "catalog is invalid",
            ),
            (
                "malformed catalog",
                BARE_MODEL,
                {"id": BARE_MODEL},
                {BOUNDED_MODEL: "bounded_json"},
                "catalog is invalid",
            ),
            (
                "whitespace-only root",
                "   ",
                [{"id": "   "}],
                {BOUNDED_MODEL: "bounded_json"},
                "root model without surrounding whitespace",
            ),
            (
                "catalog identifier with surrounding whitespace",
                BOUNDED_MODEL,
                [{"id": f" {BARE_MODEL} "}],
                {BOUNDED_MODEL: "bounded_json"},
                "catalog is invalid",
            ),
        )
        for name, model, catalog_models, model_modes, error in scenarios:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = self.create_local_relay_fixture(
                    Path(directory), model, catalog_models, model_modes
                )
                codex_config = fixture["codex_config"]
                before = codex_config.read_bytes()
                before_hash = hashlib.sha256(before).hexdigest()
                before_mtime = codex_config.stat().st_mtime_ns
                daemon_json = fixture["daemon_json"]
                daemon_before = daemon_json.read_bytes()
                daemon_mtime = daemon_json.stat().st_mtime_ns
                result = self.run_fixture_manager(
                    fixture, "set-default-model", "--allow-remote-interruption"
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(error, result.stderr)
                self.assertEqual(codex_config.read_bytes(), before)
                self.assertEqual(
                    hashlib.sha256(codex_config.read_bytes()).hexdigest(), before_hash
                )
                self.assertEqual(codex_config.stat().st_mtime_ns, before_mtime)
                self.assertEqual(daemon_json.read_bytes(), daemon_before)
                self.assertEqual(daemon_json.stat().st_mtime_ns, daemon_mtime)
                self.assertFalse(fixture["managed_log"].exists())
                self.assertEqual(
                    list(fixture["remote_home"].glob("config.toml.pre-default-model-*")), []
                )

    def test_local_relay_rejects_multiple_json_documents_without_mutation(self) -> None:
        cases = (
            (
                "catalog",
                BOUNDED_MODEL,
                [{"id": BOUNDED_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "catalog",
                "catalog is invalid",
            ),
            (
                "relay policy",
                BARE_MODEL,
                [{"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
                "relay",
                "bounded_json policy is invalid",
            ),
        )
        for name, model, catalog_models, model_modes, duplicate, error in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = self.create_local_relay_fixture(
                    Path(directory), model, catalog_models, model_modes
                )
                if duplicate == "catalog":
                    duplicate_path = fixture["catalog"]
                else:
                    duplicate_path = fixture["config_dir"] / "relay.json"
                document = duplicate_path.read_bytes()
                duplicate_path.write_bytes(document + document)

                codex_config = fixture["codex_config"]
                before = codex_config.read_bytes()
                before_hash = hashlib.sha256(before).hexdigest()
                before_mtime = codex_config.stat().st_mtime_ns
                daemon_json = fixture["daemon_json"]
                daemon_before = daemon_json.read_bytes()
                daemon_mtime = daemon_json.stat().st_mtime_ns

                result = self.run_fixture_manager(
                    fixture, "set-default-model", "--allow-remote-interruption"
                )

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(error, result.stderr)
                self.assertEqual(codex_config.read_bytes(), before)
                self.assertEqual(
                    hashlib.sha256(codex_config.read_bytes()).hexdigest(), before_hash
                )
                self.assertEqual(codex_config.stat().st_mtime_ns, before_mtime)
                self.assertEqual(daemon_json.read_bytes(), daemon_before)
                self.assertEqual(daemon_json.stat().st_mtime_ns, daemon_mtime)
                self.assertFalse(fixture["managed_log"].exists())
                self.assertEqual(
                    list(fixture["remote_home"].glob("config.toml.pre-default-model-*")), []
                )

    def test_local_relay_rejects_missing_catalog_and_invalid_root_assignments(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.create_local_relay_fixture(
                Path(directory),
                BOUNDED_MODEL,
                [{"id": BOUNDED_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
            )
            fixture["catalog"].unlink()
            result = self.run_fixture_manager(fixture, "verify-default-model")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("required regular file is unavailable", result.stderr)

        for name, contents in (
            ("missing", '[projects."/home/ubuntu"]\ntrust_level = "untrusted"\n'),
            ("duplicate", f'model = "{BARE_MODEL}"\nmodel = "{BOUNDED_MODEL}"\n'),
            ("malformed", f"model = {BARE_MODEL}\n"),
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                fixture = self.create_local_relay_fixture(
                    Path(directory),
                    BARE_MODEL,
                    [{"id": BARE_MODEL}],
                    {BOUNDED_MODEL: "bounded_json"},
                )
                codex_config = fixture["codex_config"]
                codex_config.write_text(contents, encoding="utf-8")
                before = codex_config.read_bytes()
                before_mtime = codex_config.stat().st_mtime_ns
                result = self.run_fixture_manager(
                    fixture, "set-default-model", "--allow-remote-interruption"
                )
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(codex_config.read_bytes(), before)
                self.assertEqual(codex_config.stat().st_mtime_ns, before_mtime)
                self.assertFalse(fixture["managed_log"].exists())

    def test_wrapper_allows_catalog_visible_bare_root_and_blocks_unknown_root(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture = self.create_local_relay_fixture(
                root,
                BARE_MODEL,
                [{"id": BOUNDED_MODEL}, {"id": BARE_MODEL}],
                {BOUNDED_MODEL: "bounded_json"},
            )
            manager_gate = root / "manager-gate"
            manager_gate.write_text(
                "#!/usr/bin/env bash\n"
                "[[ ${1:-} == verify-relay-health ]] || exit 51\n"
                "exec bash \"$FAKE_REAL_MANAGER\" verify-default-model\n",
                encoding="utf-8",
            )
            manager_gate.chmod(0o700)

            wrapper = root / "wrapper.sh"
            wrapper_content = SCRIPTS[0].read_text(encoding="utf-8")
            replacements = {
                'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"': f'readonly REMOTE_HOME_PATH="{fixture["remote_home"]}"',
                'readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"': f'readonly CONFIG_DIR="{fixture["config_dir"]}"',
                'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"': f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
                'readonly MANAGER="/home/ubuntu/.local/lib/opencodex-relay/manage-remote-codex-home.sh"': f'readonly MANAGER="{manager_gate}"',
                'readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"': f'readonly RELAY="{fixture["relay"]}"',
            }
            for before, after in replacements.items():
                self.assertIn(before, wrapper_content)
                wrapper_content = wrapper_content.replace(before, after)
            wrapper.write_text(wrapper_content, encoding="utf-8")
            wrapper.chmod(0o700)
            environment = fixture["environment"] | {
                "FAKE_REAL_MANAGER": str(fixture["manager"]),
            }

            accepted = subprocess.run(
                ["bash", str(wrapper), "bare-wrapper-test"],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertEqual(accepted.returncode, 73, accepted.stderr)
            managed_log = fixture["managed_log"]
            self.assertEqual(
                managed_log.read_text(encoding="utf-8").strip(),
                f'{fixture["remote_home"]}|bare-wrapper-test',
            )

            managed_log.unlink()
            fixture["codex_config"].write_text(
                'model = "unknown-model"\n', encoding="utf-8"
            )
            rejected = subprocess.run(
                ["bash", str(wrapper), "unknown-wrapper-test"],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            self.assertNotEqual(rejected.returncode, 73)
            self.assertIn("neither one bounded_json policy nor one exact catalog model", rejected.stderr)
            self.assertFalse(managed_log.exists())

    def test_launcher_repair_watches_the_user_facing_codex_path(self) -> None:
        content = WRAPPER_REPAIR_PATH.read_text(encoding="utf-8")
        self.assertIn("PathChanged=/home/ubuntu/.local/bin/codex", content)
        self.assertIn("opencodex-remote-codex-wrapper-repair.service", content)

    def test_relay_mode_is_explicit_and_keeps_credentials_out_of_native_codex(self) -> None:
        wrapper = SCRIPTS[0].read_text(encoding="utf-8")
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        routing = SCRIPTS[1].read_text(encoding="utf-8")
        self.assertIn('load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"', wrapper)
        self.assertNotIn('. "$CONFIG_FILE"', wrapper)
        self.assertIn("unset CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY", wrapper)
        self.assertNotIn('. "$CREDENTIAL_FILE"', wrapper)
        self.assertNotIn('. "$CREDENTIAL_FILE"', manager)
        self.assertIn("export CF_ACCESS_CLIENT_ID CF_ACCESS_CLIENT_SECRET OPENCODEX_GATEWAY_API_KEY", manager)
        self.assertIn("write_routing_mode relay", routing)
        self.assertIn("--allow-remote-interruption", routing)
        self.assertIn(".models // .data", manager)
        self.assertIn('ascii_downcase) != "hide"', manager)
        self.assertIn('RELAY_RESTART_PENDING="${CATALOG}.restart-pending"', manager)

    def test_wrapper_accepts_go_empty_default_sentinels(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            remote_home = root / "remote-home"
            config_dir = root / "config"
            fake_bin = root / "bin"
            for path in (remote_home, config_dir, fake_bin):
                path.mkdir(parents=True)

            config_file = config_dir / "remote-opencodex.json"
            config_file.write_text(
                remote_config_json(remote_home, "external", "relay"),
                encoding="utf-8",
            )
            config_file.chmod(0o600)
            relay_config = config_dir / "relay.json"
            relay_document = {
                "upstream_mode": "",
                "upstream_base_url": "https://gateway.example.test/v1",
                "responses": {
                    "websocket_mode": "",
                    "model_modes": {},
                    "scheduler": {"max_classifications": 0},
                },
                "catalog": {"owner": "", "manage_app_server": False},
            }
            relay_config.write_text(json.dumps(relay_document) + "\n", encoding="utf-8")
            relay_config.chmod(0o600)
            health = root / "health.json"
            health.write_text(
                json.dumps(
                    {
                        "ok": True,
                        "upstream_mode": "external_gateway",
                        "upstream_base_url": "https://gateway.example.test/v1",
                        "catalog_owner": "relay",
                        "responses_websocket_mode": "passthrough",
                        "responses_models": [],
                        "responses_normalizer": False,
                    }
                )
                + "\n",
                encoding="utf-8",
            )

            managed_codex = remote_home / "packages" / "standalone" / "current" / "codex"
            managed_codex.parent.mkdir(parents=True)
            managed_log = root / "managed.log"
            managed_codex.write_text(
                "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" > \"$FAKE_MANAGED_LOG\"\nexit 73\n",
                encoding="utf-8",
            )
            manager = root / "manager"
            manager.write_text(
                "#!/usr/bin/env bash\n[[ ${1:-} == verify-relay-health ]]\n",
                encoding="utf-8",
            )
            relay = root / "relay"
            relay.write_text(
                "#!/usr/bin/env python3\n"
                "import json, sys\n"
                "args = sys.argv[1:]\n"
                "if '--check' not in args or '--config' not in args: raise SystemExit(40)\n"
                "config = json.load(open(args[args.index('--config') + 1], encoding='utf-8'))\n"
                "value = (((config.get('responses') or {}).get('scheduler') or {})"
                ".get('max_classifications'))\n"
                "if value is not None and type(value) is not int: raise SystemExit(41)\n",
                encoding="utf-8",
            )
            (fake_bin / "id").write_text(
                "#!/usr/bin/env bash\nprintf 'ubuntu\\n'\n", encoding="utf-8"
            )
            (fake_bin / "stat").write_text(
                "#!/usr/bin/env bash\nprintf 'ubuntu:ubuntu:600\\n'\n", encoding="utf-8"
            )
            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\ncat \"$FAKE_HEALTH\"\n", encoding="utf-8"
            )
            for path in (
                managed_codex,
                manager,
                relay,
                fake_bin / "id",
                fake_bin / "stat",
                fake_bin / "curl",
            ):
                path.chmod(0o700)

            wrapper = root / "wrapper.sh"
            content = SCRIPTS[0].read_text(encoding="utf-8")
            replacements = {
                'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"': f'readonly REMOTE_HOME_PATH="{remote_home}"',
                'readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"': f'readonly CONFIG_DIR="{config_dir}"',
                'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"': f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
                'readonly MANAGER="/home/ubuntu/.local/lib/opencodex-relay/manage-remote-codex-home.sh"': f'readonly MANAGER="{manager}"',
                'readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"': f'readonly RELAY="{relay}"',
            }
            for before, after in replacements.items():
                self.assertIn(before, content)
                content = content.replace(before, after)
            wrapper.write_text(content, encoding="utf-8")
            wrapper.chmod(0o700)

            result = subprocess.run(
                ["bash", str(wrapper), "wrapper-default-test"],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "FAKE_HEALTH": str(health),
                    "FAKE_MANAGED_LOG": str(managed_log),
                },
            )
            self.assertEqual(result.returncode, 73, result.stderr)
            self.assertEqual(managed_log.read_text(encoding="utf-8").strip(), "wrapper-default-test")

            managed_log.unlink()
            relay_document["responses"]["scheduler"]["max_classifications"] = 8.0
            relay_config.write_text(json.dumps(relay_document) + "\n", encoding="utf-8")
            invalid = subprocess.run(
                ["bash", str(wrapper), "wrapper-float-test"],
                check=False,
                capture_output=True,
                text=True,
                env=os.environ
                | {
                    "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
                    "FAKE_HEALTH": str(health),
                    "FAKE_MANAGED_LOG": str(managed_log),
                },
            )
            self.assertNotEqual(invalid.returncode, 73)
            self.assertIn("installed relay rejected", invalid.stderr)
            self.assertFalse(managed_log.exists())

    def test_local_relay_mode_keeps_remote_catalog_ownership_and_mode_coupling(self) -> None:
        wrapper = SCRIPTS[0].read_text(encoding="utf-8")
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        routing = SCRIPTS[1].read_text(encoding="utf-8")
        installer = INSTALLER.read_text(encoding="utf-8")

        for content in (wrapper, manager, routing, installer):
            self.assertIn("local-relay", content)
        for content in (wrapper, manager, routing):
            self.assertIn("local_opencodex", content)
            self.assertIn("remote_manager", content)
        self.assertIn("ROUTING_MODE=local-relay is supported only with MODE=loopback", wrapper)
        self.assertIn("ROUTING_MODE=local-relay is supported only with MODE=loopback", manager)
        self.assertIn("ROUTING_MODE=local-relay requires MODE=loopback", routing)
        self.assertIn("ROUTING_MODE=relay requires MODE=external", routing)
        self.assertIn("write_routing_mode local-relay", routing)
        self.assertIn("verify_video_bridge_disabled", routing)
        self.assertNotIn("confirm-video-bridge-disabled", routing)
        self.assertIn("begin_routing_transaction", routing)
        self.assertIn("rollback_routing", routing)
        self.assertIn("restore_timer_state", routing)
        self.assertIn("require_layout relay", routing)
        self.assertIn("require_layout local-relay", routing)
        self.assertNotIn("require_selected_model_policy", routing)
        self.assertEqual(routing.count(".routing_mode = $desired"), 1)
        self.assertIn("snapshot_optional_routing_file", routing)
        self.assertIn('"$MANAGER" ensure-interactive-profile', routing)
        self.assertIn("require_dual_listener_health", routing)
        self.assertIn("listener_lane", routing)
        self.assertIn("interactive_reserved_upstream", routing)

        self.assertIn("opencodex-relay-interactive.config.toml", manager)
        self.assertIn("# opencodex-relay-managed-interactive-profile-v1", manager)
        self.assertIn("ensure-interactive-profile", manager)
        self.assertIn("verify-interactive-profile", manager)
        self.assertIn("verify-relay-health", manager)
        self.assertIn("active_interactive_upstream", manager)
        self.assertIn("scheduler_limits.max_open_deliveries", manager)
        self.assertIn('"${MANAGER_TARGET}" ensure-interactive-profile', installer)
        self.assertIn("check-interactive-profile-ownership", installer)
        self.assertNotIn('exec "$MANAGED_CODEX" --profile', wrapper)

        local_enable = routing[
            routing.index("enable_local_relay() {"):
            routing.index('action="${1:-}"')
        ]
        self.assertLess(
            local_enable.index("write_routing_mode local-relay"),
            local_enable.index('"$MANAGER" verify-default-model'),
        )
        self.assertLess(
            local_enable.index('"$MANAGER" verify-default-model'),
            local_enable.index('"$MANAGER" restart-daemon'),
        )
        self.assertLess(
            local_enable.index('"$MANAGER" restart-daemon'),
            local_enable.index(
                "systemctl --user enable --now opencodex-remote-catalog-refresh.timer"
            ),
        )
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-relay-catalog-activation.timer",
            local_enable,
        )

        refresh = manager[manager.index("refresh() {"):manager.index("restart_daemon() {")]
        self.assertEqual(refresh.count('[[ "$ROUTING_MODE" == "relay" ]]'), 1)
        self.assertIn("relay_catalog_refresh=owned_by_relay", refresh)
        self.assertIn('if [[ "$MODE" == "loopback" ]]; then', manager)
        self.assertIn("clear_edge_credentials", manager)
        self.assertIn(
            '[[ "$ROUTING_MODE" == "relay" || "$ROUTING_MODE" == "local-relay" ]]',
            wrapper,
        )

        timer_branch = installer[
            installer.index('if [[ "${routing_mode}" == "relay" ]]; then'):
            installer.index(
                "systemctl --user enable --now opencodex-remote-codex-wrapper-repair.path"
            )
        ]
        self.assertIn("legacy and local-relay keep the Remote manager", timer_branch)
        self.assertIn(
            "systemctl --user enable --now opencodex-remote-catalog-refresh.timer",
            timer_branch,
        )

    def test_relay_refresh_never_competes_with_the_dedicated_activator(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        refresh = manager[manager.index("refresh() {"):manager.index("restart_daemon() {")]
        self.assertIn("relay_catalog_refresh=owned_by_relay", refresh)
        self.assertNotIn("apply_relay_catalog", refresh)

        installer = INSTALLER.read_text(encoding="utf-8")
        self.assertIn('if [[ "${routing_mode}" == "relay" ]]; then', installer)
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-catalog-refresh.timer",
            installer,
        )
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-relay-catalog-activation.timer",
            installer,
        )
        self.assertIn('load_remote_config "$CONFIG_FILE" "$REMOTE_HOME_PATH"', installer)

        routing = SCRIPTS[1].read_text(encoding="utf-8")
        restart_index = routing.index('"$MANAGER" restart-daemon')
        enable_index = routing.index(
            "systemctl --user enable --now opencodex-remote-relay-catalog-activation.timer"
        )
        disable_index = routing.index(
            "systemctl --user disable --now opencodex-remote-catalog-refresh.timer"
        )
        self.assertLess(restart_index, enable_index)
        self.assertLess(enable_index, disable_index)

    def test_catalog_keeps_every_visible_model_without_host_entitlement_filtering(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        self.assertIn('ascii_downcase) != "hide"', manager)
        self.assertNotIn("CATALOG_EXCLUDE_MODELS", manager)
        self.assertNotIn("catalog_exclusions_json", manager)

    def test_external_catalog_refresh_rejects_redirects_and_requires_http_200(self) -> None:
        manager = SCRIPTS[2].read_text(encoding="utf-8")
        external = manager[
            manager.index("load_external_credentials\n", manager.index("fetch_catalog()")):
            manager.index("  fi\n\n  jq -e", manager.index("fetch_catalog()"))
        ]
        self.assertNotIn("--location", external)
        self.assertIn("--write-out '%{http_code}'", external)
        self.assertIn('[[ "$http_status" == "200" ]]', external)


if __name__ == "__main__":
    unittest.main()

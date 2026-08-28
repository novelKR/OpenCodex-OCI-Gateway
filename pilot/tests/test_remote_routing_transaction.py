#!/usr/bin/env python3

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
REMOTE_CONFIG_LOADER = PILOT / "scripts" / "load-remote-config.sh"
ROUTING = PILOT / "scripts" / "configure-remote-codex-routing.sh"
MANAGER = PILOT / "scripts" / "manage-remote-codex-home.sh"
QUALIFIED_MODEL = "opencode-go-responses/gpt-5.6-luna"
SCHEDULER_DEFAULTS = {
    "max_classifications": 8,
    "max_pending_requests": 24,
    "max_pending_encoded_bytes": 536870912,
    "queue_timeout_ms": 60000,
    "max_general_upstream": 4,
    "interactive_reserved_upstream": 1,
    "max_concurrent_transforms": 2,
    "max_open_deliveries": 16,
}

def remote_config_json(remote_home: Path, mode: str, routing_mode: str) -> str:
    return json.dumps({"api_origin": "https://gateway.example.test", "mode": mode, "remote_home": str(remote_home), "routing_mode": routing_mode, "schema_version": 1}, sort_keys=True) + "\n"


def write_fake_relay_validator(path: Path) -> None:
    fields = repr(tuple(SCHEDULER_DEFAULTS))
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json, sys\n"
        "args = sys.argv[1:]\n"
        "if '--check' not in args or '--config' not in args: raise SystemExit(40)\n"
        "config = json.load(open(args[args.index('--config') + 1], encoding='utf-8'))\n"
        "scheduler = ((config.get('responses') or {}).get('scheduler') or {})\n"
        f"fields = {fields}\n"
        "if any(value is not None and type(value) is not int "
        "for value in (scheduler.get(field) for field in fields)): raise SystemExit(41)\n",
        encoding="utf-8",
    )
    path.chmod(0o700)


def scheduler_health(relay: dict[str, object], lane: str) -> dict[str, object]:
    responses = relay.get("responses") or {}
    scheduler = responses.get("scheduler") or {}
    catalog = relay.get("catalog") or {}
    general_listener = relay.get("listen_address") or "127.0.0.1:18180"
    default_interactive = (
        "[::1]:18182" if str(general_listener).startswith("[::1]:") else "127.0.0.1:18182"
    )
    interactive_listener = scheduler.get("interactive_listen_address") or default_interactive
    model_modes = responses.get("model_modes") or {}
    scheduler_limits = {
        field: default if scheduler.get(field) in (None, 0) else scheduler[field]
        for field, default in SCHEDULER_DEFAULTS.items()
    }
    return {
        "ok": True,
        "listener_lane": lane,
        "general_listener": general_listener,
        "interactive_listener": interactive_listener,
        "upstream_mode": relay.get("upstream_mode") or "external_gateway",
        "upstream_base_url": relay["upstream_base_url"],
        "catalog_owner": catalog.get("owner") or "relay",
        "responses_websocket_mode": responses.get("websocket_mode") or "passthrough",
        "responses_models": sorted(model_modes.keys()),
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


class RemoteRoutingTransactionTests(unittest.TestCase):
    def make_manager_policy_fixture(
        self,
        root: Path,
        *,
        websocket_mode: str,
        model_modes: dict[str, str],
        interactive_listener: str = "127.0.0.1:18182",
    ) -> dict[str, object]:
        remote_home = root / "remote-home"
        config_dir = root / "config"
        fake_bin = root / "bin"
        for directory in (remote_home, config_dir, fake_bin):
            directory.mkdir(parents=True)

        config_file = config_dir / "remote-opencodex.json"
        config_file.write_text(
            remote_config_json(remote_home, "loopback", "local-relay"),
            encoding="utf-8",
        )
        config_file.chmod(0o600)
        codex_config = remote_home / "config.toml"
        codex_config.write_text(f'model = "{QUALIFIED_MODEL}"\n', encoding="utf-8")
        codex_config.chmod(0o600)

        relay = {
            "listen_address": "127.0.0.1:18180",
            "upstream_mode": "local_opencodex",
            "upstream_base_url": "http://127.0.0.1:10100/v1",
            "credentials": {"source": "none"},
            "responses": {
                "websocket_mode": websocket_mode,
                "model_modes": model_modes,
                "scheduler": {"interactive_listen_address": interactive_listener},
            },
            "catalog": {
                "owner": "remote_manager",
                "path": str(remote_home / "opencodex-catalog.json"),
                "manage_app_server": False,
            },
        }
        relay_config = config_dir / "relay.json"
        relay_config.write_text(json.dumps(relay) + "\n", encoding="utf-8")
        relay_config.chmod(0o600)
        catalog = remote_home / "opencodex-catalog.json"
        catalog_models = list(model_modes) or [QUALIFIED_MODEL]
        catalog.write_text(
            json.dumps({"models": [{"id": model} for model in catalog_models]}) + "\n",
            encoding="utf-8",
        )
        catalog.chmod(0o600)
        general_health = root / "general-health.json"
        interactive_health = root / "interactive-health.json"
        general_health.write_text(json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8")
        interactive_health.write_text(
            json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
        )
        interactive_profile = remote_home / "opencodex-relay-interactive.config.toml"
        interactive_profile.write_text(
            "# opencodex-relay-managed-interactive-profile-v1\n"
            f'openai_base_url = "http://{interactive_listener}/v1"\n'
            f'model_catalog_json = "{remote_home / "opencodex-catalog.json"}"\n',
            encoding="utf-8",
        )
        interactive_profile.chmod(0o600)

        managed_codex = remote_home / "packages" / "standalone" / "current" / "codex"
        managed_codex.parent.mkdir(parents=True)
        managed_log = root / "managed-codex.log"
        managed_codex.write_text(
            "#!/usr/bin/env bash\n"
            "printf '%s\\n' \"$*\" >> \"$FAKE_MANAGED_LOG\"\n"
            "exit 73\n",
            encoding="utf-8",
        )
        managed_codex.chmod(0o700)
        (fake_bin / "id").write_text("#!/usr/bin/env bash\nprintf 'ubuntu\\n'\n", encoding="utf-8")
        (fake_bin / "stat").write_text(
            "#!/usr/bin/env bash\nprintf 'ubuntu:ubuntu:600\\n'\n", encoding="utf-8"
        )
        (fake_bin / "curl").write_text(
            "#!/usr/bin/env bash\n"
            "if [[ $* == *\"$FAKE_INTERACTIVE_LISTENER/\"* ]]; then cat \"$FAKE_INTERACTIVE_HEALTH\"; "
            "else cat \"$FAKE_GENERAL_HEALTH\"; fi\n",
            encoding="utf-8",
        )
        relay = fake_bin / "relay"
        write_fake_relay_validator(relay)
        for executable in (fake_bin / "id", fake_bin / "stat", fake_bin / "curl"):
            executable.chmod(0o755)

        script = root / "manager.sh"
        content = MANAGER.read_text(encoding="utf-8")
        replacements = {
            'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"': f'readonly REMOTE_HOME_PATH="{remote_home}"',
            'readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"': f'readonly CONFIG_DIR="{config_dir}"',
            'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"': f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
            'readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"': f'readonly RELAY="{relay}"',
        }
        for before, after in replacements.items():
            self.assertIn(before, content)
            content = content.replace(before, after)
        script.write_text(content, encoding="utf-8")
        script.chmod(0o700)
        environment = os.environ | {
            "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
            "FAKE_GENERAL_HEALTH": str(general_health),
            "FAKE_INTERACTIVE_HEALTH": str(interactive_health),
            "FAKE_INTERACTIVE_LISTENER": interactive_listener,
            "FAKE_MANAGED_LOG": str(managed_log),
        }
        return {
            "script": script,
            "env": environment,
            "remote_config": config_file,
            "codex_config": codex_config,
            "relay_config": relay_config,
            "managed_log": managed_log,
            "general_health": general_health,
            "interactive_health": interactive_health,
        }

    def make_fixture(self, root: Path, *, mode: str = "loopback") -> dict[str, object]:
        remote_home = root / "remote-home"
        config_dir = root / "config"
        fake_bin = root / "bin"
        state_dir = root / "systemctl-state"
        for directory in (remote_home, config_dir, fake_bin, state_dir):
            directory.mkdir(parents=True)

        config_file = config_dir / "remote-opencodex.json"
        config_file.write_text(
            remote_config_json(remote_home, mode, "legacy"),
            encoding="utf-8",
        )
        config_file.chmod(0o600)
        codex_config = remote_home / "config.toml"
        root_model = QUALIFIED_MODEL if mode == "loopback" else "gpt-5.6-luna"
        codex_config.write_text(f'model = "{root_model}"\n# original routing\n', encoding="utf-8")
        codex_config.chmod(0o600)

        relay_config = config_dir / "relay.json"
        if mode == "loopback":
            relay = {
                "listen_address": "127.0.0.1:18180",
                "upstream_mode": "local_opencodex",
                "upstream_base_url": "http://127.0.0.1:10100/v1",
                "credentials": {"source": "none"},
                "responses": {
                    "websocket_mode": "http_fallback",
                    "model_modes": {QUALIFIED_MODEL: "bounded_json"},
                },
                "catalog": {
                    "owner": "remote_manager",
                    "path": str(remote_home / "opencodex-catalog.json"),
                    "manage_app_server": False,
                },
            }
        else:
            credential_file = config_dir / "credentials.env"
            credential_file.write_text(
                "CF_ACCESS_CLIENT_ID=test-id\n"
                "CF_ACCESS_CLIENT_SECRET=test-secret\n"
                "OPENCODEX_GATEWAY_API_KEY=test-gateway-key\n",
                encoding="utf-8",
            )
            credential_file.chmod(0o600)
            relay = {
                "listen_address": "127.0.0.1:18180",
                "upstream_mode": "external_gateway",
                "upstream_base_url": "https://gateway.example.test/v1",
                "credentials": {"source": "file", "file": str(credential_file)},
                "responses": {"websocket_mode": "passthrough", "model_modes": {}},
                "catalog": {
                    "owner": "relay",
                    "path": str(remote_home / "opencodex-catalog.json"),
                    "manage_app_server": False,
                },
            }
        relay_config.write_text(json.dumps(relay) + "\n", encoding="utf-8")
        relay_config.chmod(0o600)

        general_health = root / "general-health.json"
        interactive_health = root / "interactive-health.json"
        general_health.write_text(json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8")
        interactive_health.write_text(
            json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
        )

        relayctl = fake_bin / "relayctl"
        relayctl.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "action=${1:-}; shift || true\n"
            "if [[ $action == enable ]]; then\n"
            "  codex=\n"
            "  while [[ $# -gt 0 ]]; do\n"
            "    if [[ $1 == --codex-config ]]; then codex=$2; shift 2; else shift; fi\n"
            "  done\n"
            "  printf 'model = \"%s\"\\n# candidate relay routing\\n' \"$FAKE_ROOT_MODEL\" > \"$codex\"\n"
            "  chmod 600 \"$codex\"\n"
            "fi\n"
            "exit 0\n",
            encoding="utf-8",
        )
        manager = fake_bin / "manager"
        manager.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "printf '%s\\n' \"${1:-}\" >> \"$FAKE_MANAGER_LOG\"\n"
            "if [[ ${1:-} == check-interactive-profile-ownership && -e $FAKE_INTERACTIVE_PROFILE ]]; then\n"
            "  IFS= read -r first < \"$FAKE_INTERACTIVE_PROFILE\"\n"
            "  [[ $first == '# opencodex-relay-managed-interactive-profile-v1' ]] || exit 92\n"
            "fi\n"
            "if [[ ${1:-} == ensure-interactive-profile ]]; then\n"
            "  printf '%s\\n' '# opencodex-relay-managed-interactive-profile-v1' "
            "    'openai_base_url = \"http://127.0.0.1:18182/v1\"' "
            "    \"model_catalog_json = \\\"$FAKE_CATALOG\\\"\" > \"$FAKE_INTERACTIVE_PROFILE\"\n"
            "  chmod 600 \"$FAKE_INTERACTIVE_PROFILE\"\n"
            "fi\n"
            "if [[ ${1:-} == restart-daemon && ${FAIL_MANAGER_ONCE:-0} == 1 && ! -e $FAKE_MANAGER_FAIL_MARKER ]]; then\n"
            "  : > \"$FAKE_MANAGER_FAIL_MARKER\"\n"
            "  exit 91\n"
            "fi\n"
            "exit 0\n",
            encoding="utf-8",
        )
        for executable in (relayctl, manager):
            executable.chmod(0o700)
        relay_binary = fake_bin / "relay"
        write_fake_relay_validator(relay_binary)
        runtime_adapter = fake_bin / "opencodex-runtime"
        runtime_adapter.write_text(
            "#!/usr/bin/env bash\n"
            "printf 'runtime adapter must be invoked through sudo in this fixture\\n' >&2\n"
            "exit 96\n",
            encoding="utf-8",
        )
        runtime_adapter.chmod(0o755)

        (fake_bin / "id").write_text("#!/usr/bin/env bash\nprintf 'ubuntu\\n'\n", encoding="utf-8")
        (fake_bin / "stat").write_text("#!/usr/bin/env bash\nprintf 'ubuntu:ubuntu:600\\n'\n", encoding="utf-8")
        (fake_bin / "chown").write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
        (fake_bin / "curl").write_text(
            "#!/usr/bin/env bash\n"
            "if [[ $* == *\"$FAKE_INTERACTIVE_LISTENER/\"* ]]; then cat \"$FAKE_INTERACTIVE_HEALTH\"; "
            "else cat \"$FAKE_GENERAL_HEALTH\"; fi\n",
            encoding="utf-8",
        )
        fake_sudo = fake_bin / "sudo"
        fake_sudo.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "printf '%s\\n' \"$*\" >> \"$FAKE_SUDO_LOG\"\n"
            "runtime_home=${FAKE_RUNTIME_HOME:-/var/lib/opencodex}\n"
            "if [[ $* == *' describe --json' ]]; then\n"
            "  [[ ${FAKE_RUNTIME_AVAILABLE:-1} == 1 ]] || exit 94\n"
            "  if [[ ${FAKE_RUNTIME_DESCRIPTION_VALID:-1} != 1 ]]; then\n"
            "    printf '%s\\n' '{\"runtime_kind\":\"node\"}'\n"
            "  else\n"
            "    printf '{\"schema_version\":1,\"runtime_kind\":\"node\","
            "\"home\":\"%s\",\"prefix\":\"%s\",\"node_bin\":\"%s\","
            "\"npm_cli\":\"%s\",\"ocx_entry\":\"%s\",\"package_manifest\":\"%s\","
            "\"package_version\":\"2.17.0\"}\\n' "
            "\"$runtime_home\" "
            "\"${FAKE_RUNTIME_PREFIX:-/opt/opencodex}\" "
            "\"${FAKE_RUNTIME_NODE:-/custom/runtime/node}\" "
            "\"${FAKE_RUNTIME_NPM:-/custom/runtime/npm-cli.js}\" "
            "\"${FAKE_RUNTIME_OCX:-/opt/opencodex/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs}\" "
            "\"${FAKE_RUNTIME_MANIFEST:-/opt/opencodex/lib/node_modules/@bitkyc08/opencodex/package.json}\"\n"
            "  fi\n"
            "  exit 0\n"
            "fi\n"
            "if [[ $* == *' config validate --json' ]]; then\n"
            "  [[ ${FAKE_ADAPTER_OCX_AVAILABLE:-1} == 1 ]] || exit 95\n"
            "  if [[ ${FAKE_CONFIG_VALID:-1} == 1 ]]; then\n"
            "    printf '{\"ok\":true,\"source\":\"%s\"}\\n' "
            "\"${FAKE_CONFIG_SOURCE:-${runtime_home}/.opencodex/config.json}\"\n"
            "  else\n"
            "    printf '%s\\n' '{\"ok\":false,\"error\":\"invalid\"}'\n"
            "  fi\n"
            "  exit 0\n"
            "fi\n"
            "if [[ $* == *' config show --json' ]]; then\n"
            "  [[ ${FAKE_ADAPTER_OCX_AVAILABLE:-1} == 1 ]] || exit 95\n"
            "  printf '{\"images\":{\"videoBridgeEnabled\":%s}}\\n' \"${FAKE_VIDEO_BRIDGE:-false}\"\n"
            "  exit 0\n"
            "fi\n"
            "exit 97\n",
            encoding="utf-8",
        )
        systemctl = fake_bin / "systemctl"
        systemctl.write_text(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "[[ ${1:-} == --user ]] && shift\n"
            "action=${1:-}; shift || true\n"
            "case $action in\n"
            "  is-enabled|is-active) cat \"$FAKE_SYSTEMCTL_STATE/${1}.${action#is-}\"; exit 0 ;;\n"
            "  enable|disable|start|stop)\n"
            "    now=false\n"
            "    if [[ ${1:-} == --now ]]; then now=true; shift; fi\n"
            "    unit=$1\n"
            "    case $action in\n"
            "      enable) printf enabled > \"$FAKE_SYSTEMCTL_STATE/$unit.enabled\"; [[ $now == false ]] || printf active > \"$FAKE_SYSTEMCTL_STATE/$unit.active\" ;;\n"
            "      disable) printf disabled > \"$FAKE_SYSTEMCTL_STATE/$unit.enabled\"; [[ $now == false ]] || printf inactive > \"$FAKE_SYSTEMCTL_STATE/$unit.active\" ;;\n"
            "      start) printf active > \"$FAKE_SYSTEMCTL_STATE/$unit.active\" ;;\n"
            "      stop) printf inactive > \"$FAKE_SYSTEMCTL_STATE/$unit.active\" ;;\n"
            "    esac\n"
            "    if [[ ${FAIL_SYSTEMCTL_ACTION:-} == $action && ${FAIL_SYSTEMCTL_UNIT:-} == $unit && ! -e $FAKE_SYSTEMCTL_FAIL_MARKER ]]; then\n"
            "      : > \"$FAKE_SYSTEMCTL_FAIL_MARKER\"\n"
            "      exit 88\n"
            "    fi\n"
            "    ;;\n"
            "  *) exit 98 ;;\n"
            "esac\n",
            encoding="utf-8",
        )
        for executable in (fake_bin / "id", fake_bin / "stat", fake_bin / "chown", fake_bin / "curl", fake_sudo, systemctl):
            executable.chmod(0o755)

        refresh = "opencodex-remote-catalog-refresh.timer"
        activation = "opencodex-remote-relay-catalog-activation.timer"
        for unit, enabled, active in (
            (refresh, "disabled", "inactive"),
            (activation, "enabled", "active"),
        ):
            (state_dir / f"{unit}.enabled").write_text(enabled, encoding="utf-8")
            (state_dir / f"{unit}.active").write_text(active, encoding="utf-8")

        script = root / "configure-routing.sh"
        content = ROUTING.read_text(encoding="utf-8")
        replacements = {
            'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"': f'readonly REMOTE_HOME_PATH="{remote_home}"',
            'readonly CONFIG_DIR="/home/ubuntu/.config/opencodex-relay"': f'readonly CONFIG_DIR="{config_dir}"',
            'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"': f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
            'readonly RELAY="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relay"': f'readonly RELAY="{relay_binary}"',
            'readonly RELAYCTL="/home/ubuntu/.local/lib/opencodex-relay/relay/current/opencodex-relayctl"': f'readonly RELAYCTL="{relayctl}"',
            'readonly MANAGER="/home/ubuntu/.local/lib/opencodex-relay/manage-remote-codex-home.sh"': f'readonly MANAGER="{manager}"',
            'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"': f'readonly RUNTIME_ADAPTER="{runtime_adapter}"',
            'readonly SUDO_BIN="/usr/bin/sudo"': f'readonly SUDO_BIN="{fake_sudo}"',
        }
        for before, after in replacements.items():
            self.assertIn(before, content)
            content = content.replace(before, after)
        script.write_text(content, encoding="utf-8")
        script.chmod(0o700)

        environment = os.environ | {
            "PATH": str(fake_bin) + os.pathsep + os.environ["PATH"],
            "FAKE_GENERAL_HEALTH": str(general_health),
            "FAKE_INTERACTIVE_HEALTH": str(interactive_health),
            "FAKE_INTERACTIVE_LISTENER": "127.0.0.1:18182",
            "FAKE_INTERACTIVE_PROFILE": str(remote_home / "opencodex-relay-interactive.config.toml"),
            "FAKE_CATALOG": str(remote_home / "opencodex-catalog.json"),
            "FAKE_MANAGER_LOG": str(root / "manager.log"),
            "FAKE_MANAGER_FAIL_MARKER": str(root / "manager.failed"),
            "FAKE_ROOT_MODEL": root_model,
            "FAKE_SUDO_LOG": str(root / "sudo.log"),
            "FAKE_SYSTEMCTL_STATE": str(state_dir),
            "FAKE_SYSTEMCTL_FAIL_MARKER": str(root / "systemctl.failed"),
        }
        return {
            "script": script,
            "env": environment,
            "config": config_file,
            "relay_config": relay_config,
            "codex": codex_config,
            "general_health": general_health,
            "interactive_health": interactive_health,
            "state": state_dir,
            "manager_log": root / "manager.log",
            "sudo_log": root / "sudo.log",
            "runtime_adapter": runtime_adapter,
        }

    def run_script(self, fixture: dict[str, object], *args: str, extra_env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        environment = dict(fixture["env"])
        if extra_env:
            environment.update(extra_env)
        return subprocess.run(
            ["bash", str(fixture["script"]), *args],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )

    def test_video_bridge_preflight_uses_runtime_adapter_and_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory))
            custom_home = f"{directory}/custom-opencodex-home"
            custom_prefix = f"{directory}/custom-opencodex-prefix"
            custom_node = f"{directory}/custom-node/bin/node"
            runtime_env = {
                "FAKE_RUNTIME_HOME": custom_home,
                "FAKE_RUNTIME_PREFIX": custom_prefix,
                "FAKE_RUNTIME_NODE": custom_node,
                "FAKE_RUNTIME_NPM": f"{directory}/custom-node/lib/npm-cli.js",
                "FAKE_RUNTIME_OCX": (
                    f"{custom_prefix}/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs"
                ),
                "FAKE_RUNTIME_MANIFEST": (
                    f"{custom_prefix}/lib/node_modules/@bitkyc08/opencodex/package.json"
                ),
            }
            ok = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env,
            )
            self.assertEqual(ok.returncode, 0, ok.stderr)
            self.assertIn(
                f"source={custom_home}/.opencodex/config.json",
                ok.stdout,
            )
            sudo_calls = Path(fixture["sudo_log"]).read_text(encoding="utf-8").splitlines()
            adapter = str(fixture["runtime_adapter"])
            self.assertEqual(sudo_calls[0], f"-n -- {adapter} describe --json")
            self.assertEqual(
                sudo_calls[1:],
                [
                    (
                        "-n -u opencodex -- env -u OPENCODEX_HOME -u CODEX_HOME "
                        f"{adapter} ocx config validate --json"
                    ),
                    (
                        "-n -u opencodex -- env -u OPENCODEX_HOME -u CODEX_HOME "
                        f"{adapter} ocx config show --json"
                    ),
                ],
            )
            joined_sudo_calls = "\n".join(sudo_calls)
            self.assertNotIn(custom_node, joined_sudo_calls)
            self.assertNotIn(custom_prefix, joined_sudo_calls)
            self.assertNotIn(
                "/home/linuxbrew/.linuxbrew/opt/node@24/bin/node",
                joined_sudo_calls,
            )
            self.assertNotIn(
                "/opt/opencodex/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs",
                joined_sudo_calls,
            )
            self.assertNotIn("/home/linuxbrew/.linuxbrew/opt/node@24/bin/node", ROUTING.read_text())
            self.assertNotIn(
                "/opt/opencodex/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs",
                ROUTING.read_text(),
            )

            enabled = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_VIDEO_BRIDGE": "true"},
            )
            self.assertNotEqual(enabled.returncode, 0)
            self.assertIn("videoBridgeEnabled=false", enabled.stderr)

            invalid = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_CONFIG_VALID": "0"},
            )
            self.assertNotEqual(invalid.returncode, 0)
            self.assertIn("did not identify the active service configuration", invalid.stderr)

            wrong_source = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_CONFIG_SOURCE": "/unexpected/config.json"},
            )
            self.assertNotEqual(wrong_source.returncode, 0)
            self.assertIn("did not identify the active service configuration", wrong_source.stderr)

            missing_runtime = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_RUNTIME_AVAILABLE": "0"},
            )
            self.assertNotEqual(missing_runtime.returncode, 0)
            self.assertIn("Runtime Adapter or its contract is unavailable", missing_runtime.stderr)

            malformed_description = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_RUNTIME_DESCRIPTION_VALID": "0"},
            )
            self.assertNotEqual(malformed_description.returncode, 0)
            self.assertIn("description has no valid home", malformed_description.stderr)

            unavailable_ocx = self.run_script(
                fixture,
                "verify-video-bridge-disabled",
                extra_env=runtime_env | {"FAKE_ADAPTER_OCX_AVAILABLE": "0"},
            )
            self.assertNotEqual(unavailable_ocx.returncode, 0)
            self.assertIn("did not identify the active service configuration", unavailable_ocx.stderr)

    def test_local_activation_rejects_runtime_source_drift_before_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory))
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            state = Path(fixture["state"])
            original_timer_state = {
                path.name: path.read_text(encoding="utf-8") for path in state.iterdir()
            }

            result = self.run_script(
                fixture,
                "enable-local-relay",
                "--allow-remote-interruption",
                extra_env={"FAKE_CONFIG_SOURCE": "/unexpected/config.json"},
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("did not identify the active service configuration", result.stderr)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertEqual(
                {
                    path.name: path.read_text(encoding="utf-8")
                    for path in state.iterdir()
                },
                original_timer_state,
            )
            self.assertFalse(Path(fixture["manager_log"]).exists())

    def test_local_activation_restores_files_and_timers_after_manager_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory))
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            result = self.run_script(
                fixture,
                "enable-local-relay",
                "--allow-remote-interruption",
                extra_env={"FAIL_MANAGER_ONCE": "1"},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("restoring Codex routing and timer state", result.stderr)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertFalse(
                (Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml").exists()
            )
            calls = Path(fixture["manager_log"]).read_text(encoding="utf-8").splitlines()
            self.assertEqual(calls.count("restart-daemon"), 2)

    def test_local_activation_restores_exact_timer_state_after_timer_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory))
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            state = Path(fixture["state"])
            before = {path.name: path.read_text(encoding="utf-8") for path in state.iterdir()}
            result = self.run_script(
                fixture,
                "enable-local-relay",
                "--allow-remote-interruption",
                extra_env={
                    "FAIL_SYSTEMCTL_ACTION": "disable",
                    "FAIL_SYSTEMCTL_UNIT": "opencodex-remote-relay-catalog-activation.timer",
                },
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertFalse(
                (Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml").exists()
            )
            after = {path.name: path.read_text(encoding="utf-8") for path in state.iterdir()}
            self.assertEqual(after, before)

    def test_external_relay_without_model_policy_remains_compatible_and_writes_one_mode(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            remote_config = json.loads(Path(fixture["config"]).read_text(encoding="utf-8"))
            self.assertEqual(remote_config["routing_mode"], "relay")
            profile = Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml"
            self.assertEqual(
                profile.read_text(encoding="utf-8").splitlines(),
                [
                    "# opencodex-relay-managed-interactive-profile-v1",
                    'openai_base_url = "http://127.0.0.1:18182/v1"',
                    f'model_catalog_json = "{Path(fixture["codex"]).parent / "opencodex-catalog.json"}"',
                ],
            )

    def test_external_bounded_policy_requires_exact_root_before_routing_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            relay_path = Path(fixture["relay_config"])
            relay = json.loads(relay_path.read_text(encoding="utf-8"))
            relay["responses"] = {
                "websocket_mode": "http_fallback",
                "model_modes": {QUALIFIED_MODEL: "bounded_json"},
            }
            relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
            Path(fixture["general_health"]).write_text(
                json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8"
            )
            Path(fixture["interactive_health"]).write_text(
                json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
            )
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()

            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
            )

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("selected Codex root model must be", result.stderr)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertFalse(
                (Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml").exists()
            )
            self.assertEqual(
                Path(fixture["manager_log"]).read_text(encoding="utf-8").splitlines(),
                ["check-interactive-profile-ownership"],
            )

    def test_external_bounded_policy_routes_exact_root_and_verifies_before_restart(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            relay_path = Path(fixture["relay_config"])
            relay = json.loads(relay_path.read_text(encoding="utf-8"))
            relay["responses"] = {
                "websocket_mode": "http_fallback",
                "model_modes": {QUALIFIED_MODEL: "bounded_json"},
            }
            relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
            Path(fixture["general_health"]).write_text(
                json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8"
            )
            Path(fixture["interactive_health"]).write_text(
                json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
            )
            Path(fixture["codex"]).write_text(
                f'model = "{QUALIFIED_MODEL}"\n', encoding="utf-8"
            )
            environment = dict(fixture["env"])
            environment["FAKE_ROOT_MODEL"] = QUALIFIED_MODEL

            result = subprocess.run(
                [
                    "bash",
                    str(fixture["script"]),
                    "enable-relay",
                    "--allow-remote-interruption",
                ],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )

            self.assertEqual(result.returncode, 0, result.stderr)
            calls = Path(fixture["manager_log"]).read_text(encoding="utf-8").splitlines()
            self.assertLess(calls.index("verify-default-model"), calls.index("restart-daemon"))

    def test_routing_accepts_go_zero_and_empty_default_sentinels(self) -> None:
        for value in (0, 8, 12):
            with self.subTest(max_classifications=value), tempfile.TemporaryDirectory() as directory:
                fixture = self.make_fixture(Path(directory), mode="external")
                relay_path = Path(fixture["relay_config"])
                relay = json.loads(relay_path.read_text(encoding="utf-8"))
                relay["upstream_mode"] = ""
                relay["catalog"]["owner"] = ""
                relay["responses"]["websocket_mode"] = ""
                relay["responses"]["scheduler"] = {
                    "interactive_listen_address": "",
                    "max_classifications": value,
                    "max_pending_requests": 0,
                    "max_pending_encoded_bytes": 0,
                    "queue_timeout_ms": 0,
                    "max_general_upstream": 0,
                    "interactive_reserved_upstream": 0,
                    "max_concurrent_transforms": 0,
                    "max_open_deliveries": 0,
                }
                relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
                Path(fixture["general_health"]).write_text(
                    json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8"
                )
                Path(fixture["interactive_health"]).write_text(
                    json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
                )

                result = self.run_script(
                    fixture,
                    "enable-relay",
                    "--allow-remote-interruption",
                )
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_routing_rejects_float_integer_before_matching_health(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            relay_path = Path(fixture["relay_config"])
            relay = json.loads(relay_path.read_text(encoding="utf-8"))
            relay["responses"]["scheduler"] = {"max_classifications": 8.0}
            relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
            Path(fixture["general_health"]).write_text(
                json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8"
            )
            Path(fixture["interactive_health"]).write_text(
                json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
            )

            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("installed relay rejected", result.stderr)
            self.assertFalse(Path(fixture["manager_log"]).exists())

    def test_manager_accepts_go_zero_and_empty_default_sentinels(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
            )
            remote_config = Path(fixture["remote_config"])
            remote_config.write_text(
                remote_config_json(Path(directory) / "remote-home", "external", "relay"),
                encoding="utf-8",
            )
            relay_path = Path(fixture["relay_config"])
            relay = json.loads(relay_path.read_text(encoding="utf-8"))
            relay["upstream_mode"] = ""
            relay["upstream_base_url"] = "https://gateway.example.test/v1"
            credential_file = Path(directory) / "config" / "credentials.env"
            credential_file.write_text(
                "CF_ACCESS_CLIENT_ID=test-id\n"
                "CF_ACCESS_CLIENT_SECRET=test-secret\n"
                "OPENCODEX_GATEWAY_API_KEY=test-gateway-key\n",
                encoding="utf-8",
            )
            credential_file.chmod(0o600)
            relay["credentials"] = {"source": "file", "file": str(credential_file)}
            relay["catalog"]["owner"] = ""
            relay["responses"] = {
                "websocket_mode": "",
                "model_modes": {},
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
            }
            relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
            effective_health = scheduler_health(relay, "general")
            Path(fixture["general_health"]).write_text(
                json.dumps(effective_health) + "\n", encoding="utf-8"
            )
            Path(fixture["interactive_health"]).write_text(
                json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
            )

            result = subprocess.run(
                ["bash", str(fixture["script"]), "verify-relay-health"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("relay_dual_listener_health=verified", result.stdout)

    def test_manager_rejects_float_integer_before_matching_health(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
            )
            relay_path = Path(fixture["relay_config"])
            relay = json.loads(relay_path.read_text(encoding="utf-8"))
            relay["responses"]["scheduler"]["max_classifications"] = 8.0
            relay_path.write_text(json.dumps(relay) + "\n", encoding="utf-8")
            Path(fixture["general_health"]).write_text(
                json.dumps(scheduler_health(relay, "general")) + "\n", encoding="utf-8"
            )
            Path(fixture["interactive_health"]).write_text(
                json.dumps(scheduler_health(relay, "interactive")) + "\n", encoding="utf-8"
            )

            result = subprocess.run(
                ["bash", str(fixture["script"]), "verify-relay-health"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("installed relay rejected", result.stderr)

    def test_external_activation_also_restores_routing_after_manager_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
                extra_env={"FAIL_MANAGER_ONCE": "1"},
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("restoring Codex routing and timer state", result.stderr)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertFalse(
                (Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml").exists()
            )
            calls = Path(fixture["manager_log"]).read_text(encoding="utf-8").splitlines()
            self.assertEqual(calls.count("restart-daemon"), 2)

    def test_external_activation_restores_exact_timer_state_after_timer_failure(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            state = Path(fixture["state"])
            refresh = "opencodex-remote-catalog-refresh.timer"
            activation = "opencodex-remote-relay-catalog-activation.timer"
            (state / f"{refresh}.enabled").write_text("enabled", encoding="utf-8")
            (state / f"{refresh}.active").write_text("active", encoding="utf-8")
            (state / f"{activation}.enabled").write_text("disabled", encoding="utf-8")
            (state / f"{activation}.active").write_text("inactive", encoding="utf-8")
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            before = {path.name: path.read_text(encoding="utf-8") for path in state.iterdir()}

            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
                extra_env={
                    "FAIL_SYSTEMCTL_ACTION": "disable",
                    "FAIL_SYSTEMCTL_UNIT": refresh,
                },
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            after = {path.name: path.read_text(encoding="utf-8") for path in state.iterdir()}
            self.assertEqual(after, before)

    def test_unmanaged_interactive_profile_blocks_routing_before_mutation(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_fixture(Path(directory), mode="external")
            original_config = Path(fixture["config"]).read_bytes()
            original_codex = Path(fixture["codex"]).read_bytes()
            profile = Path(fixture["codex"]).parent / "opencodex-relay-interactive.config.toml"
            profile.write_text('openai_base_url = "http://127.0.0.1:19000/v1"\n', encoding="utf-8")
            profile.chmod(0o600)
            result = self.run_script(
                fixture,
                "enable-relay",
                "--allow-remote-interruption",
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(Path(fixture["config"]).read_bytes(), original_config)
            self.assertEqual(Path(fixture["codex"]).read_bytes(), original_codex)
            self.assertEqual(
                profile.read_text(encoding="utf-8"),
                'openai_base_url = "http://127.0.0.1:19000/v1"\n',
            )
            calls = Path(fixture["manager_log"]).read_text(encoding="utf-8").splitlines()
            self.assertEqual(calls, ["check-interactive-profile-ownership"])

    def test_local_manager_rejects_normalizer_policy_drift_before_codex_lifecycle(self) -> None:
        cases = (
            ("passthrough", {QUALIFIED_MODEL: "bounded_json"}),
            ("http_fallback", {}),
            ("http_fallback", {"opencode-go-responses/other-model": "bounded_json"}),
        )
        with tempfile.TemporaryDirectory() as directory:
            for index, (websocket_mode, model_modes) in enumerate(cases):
                with self.subTest(websocket_mode=websocket_mode, model_modes=model_modes):
                    root = Path(directory) / str(index)
                    fixture = self.make_manager_policy_fixture(
                        root,
                        websocket_mode=websocket_mode,
                        model_modes=model_modes,
                    )
                    result = subprocess.run(
                        ["bash", str(fixture["script"]), "bootstrap-remote-control"],
                        check=False,
                        capture_output=True,
                        text=True,
                        env=fixture["env"],
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(Path(fixture["managed_log"]).exists())

    def test_local_manager_allows_exact_normalizer_policy_to_reach_codex_lifecycle(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
            )
            result = subprocess.run(
                ["bash", str(fixture["script"]), "bootstrap-remote-control"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertEqual(result.returncode, 73, result.stderr)
            self.assertEqual(
                Path(fixture["managed_log"]).read_text(encoding="utf-8").strip(),
                "app-server daemon bootstrap --remote-control",
            )

    def test_local_manager_preserves_an_arbitrary_provider_qualified_policy_model(self) -> None:
        selected = "custom-responses/example-model"
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={selected: "bounded_json"},
            )
            Path(fixture["codex_config"]).write_text(
                f'model = "{selected}"\n', encoding="utf-8"
            )
            result = subprocess.run(
                ["bash", str(fixture["script"]), "verify-default-model"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn(f"model={selected}", result.stdout)

    def test_external_manager_requires_exact_bounded_default_only_when_policy_enabled(self) -> None:
        cases = (
            ({QUALIFIED_MODEL: "bounded_json"}, QUALIFIED_MODEL, True),
            ({QUALIFIED_MODEL: "bounded_json"}, "gpt-5.6-luna", False),
            ({}, "gpt-5.6-luna", True),
            ({"opencode-go-responses/other-model": "bounded_json"}, "gpt-5.6-luna", True),
        )
        with tempfile.TemporaryDirectory() as directory:
            for index, (model_modes, root_model, should_pass) in enumerate(cases):
                with self.subTest(model_modes=model_modes, root_model=root_model):
                    root = Path(directory) / str(index)
                    fixture = self.make_manager_policy_fixture(
                        root,
                        websocket_mode="http_fallback",
                        model_modes=model_modes,
                    )
                    Path(fixture["remote_config"]).write_text(
                        remote_config_json(root / "remote-home", "external", "relay"),
                        encoding="utf-8",
                    )
                    Path(fixture["codex_config"]).write_text(
                        f'model = "{root_model}"\n', encoding="utf-8"
                    )
                    result = subprocess.run(
                        ["bash", str(fixture["script"]), "verify-default-model"],
                        check=False,
                        capture_output=True,
                        text=True,
                        env=fixture["env"],
                    )
                    if should_pass:
                        self.assertEqual(result.returncode, 0, result.stderr)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn(
                            "expected opencode-go-responses/gpt-5.6-luna", result.stderr
                        )

    def test_external_manager_rejects_malformed_policy_instead_of_downgrading(self) -> None:
        cases = (
            {QUALIFIED_MODEL: "passthrough"},
            {QUALIFIED_MODEL: "bounded_json", QUALIFIED_MODEL.upper(): "bounded_json"},
            {f" {QUALIFIED_MODEL}": "bounded_json"},
            {"": "bounded_json"},
        )
        with tempfile.TemporaryDirectory() as directory:
            for index, model_modes in enumerate(cases):
                with self.subTest(model_modes=model_modes):
                    root = Path(directory) / str(index)
                    fixture = self.make_manager_policy_fixture(
                        root,
                        websocket_mode="http_fallback",
                        model_modes=model_modes,
                    )
                    Path(fixture["remote_config"]).write_text(
                        remote_config_json(root / "remote-home", "external", "relay"),
                        encoding="utf-8",
                    )
                    Path(fixture["codex_config"]).write_text(
                        'model = "gpt-5.6-luna"\n', encoding="utf-8"
                    )
                    result = subprocess.run(
                        ["bash", str(fixture["script"]), "verify-default-model"],
                        check=False,
                        capture_output=True,
                        text=True,
                        env=fixture["env"],
                    )
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("relay policy configuration is invalid", result.stderr)

    def test_manager_generates_only_two_settings_for_configured_interactive_port(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
                interactive_listener="127.0.0.1:19222",
            )
            profile = Path(directory) / "remote-home" / "opencodex-relay-interactive.config.toml"
            profile.unlink()
            result = subprocess.run(
                ["bash", str(fixture["script"]), "ensure-interactive-profile"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            lines = profile.read_text(encoding="utf-8").splitlines()
            self.assertEqual(
                lines,
                [
                    "# opencodex-relay-managed-interactive-profile-v1",
                    'openai_base_url = "http://127.0.0.1:19222/v1"',
                    f'model_catalog_json = "{Path(directory) / "remote-home" / "opencodex-catalog.json"}"',
                ],
            )
            self.assertFalse(any(line.startswith(("model =", "reasoning", "agents.")) for line in lines))

    def test_manager_never_overwrites_an_unmanaged_same_name_profile(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
            )
            profile = Path(directory) / "remote-home" / "opencodex-relay-interactive.config.toml"
            unmanaged = 'openai_base_url = "http://127.0.0.1:19991/v1"\nmodel = "user-choice"\n'
            profile.write_text(unmanaged, encoding="utf-8")
            profile.chmod(0o600)
            result = subprocess.run(
                ["bash", str(fixture["script"]), "ensure-interactive-profile"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("is not owned by opencodex-relay", result.stderr)
            self.assertEqual(profile.read_text(encoding="utf-8"), unmanaged)

    def test_manager_rejects_scheduler_limit_health_drift_before_codex_lifecycle(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            fixture = self.make_manager_policy_fixture(
                Path(directory),
                websocket_mode="http_fallback",
                model_modes={QUALIFIED_MODEL: "bounded_json"},
            )
            health = json.loads(Path(fixture["general_health"]).read_text(encoding="utf-8"))
            health["scheduler_limits"]["max_general_upstream"] = 99
            Path(fixture["general_health"]).write_text(json.dumps(health) + "\n", encoding="utf-8")
            result = subprocess.run(
                ["bash", str(fixture["script"]), "bootstrap-remote-control"],
                check=False,
                capture_output=True,
                text=True,
                env=fixture["env"],
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(Path(fixture["managed_log"]).exists())


if __name__ == "__main__":
    unittest.main()

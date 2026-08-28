#!/usr/bin/env python3

import hashlib
import json
import os
import re
import subprocess
import tempfile
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
SCRIPTS_DIR = PILOT / "scripts"
REMOTE_CONFIG_LOADER = SCRIPTS_DIR / "load-remote-config.sh"
REPO_ROOT = PILOT.parent

def remote_config_json(remote_home: Path, mode: str, routing_mode: str) -> str:
    return json.dumps({"api_origin": "https://gateway.example.test", "mode": mode, "remote_home": str(remote_home), "routing_mode": routing_mode, "schema_version": 1}, sort_keys=True) + "\n"


class UpdateAssetsTests(unittest.TestCase):
    def run_script(self, name: str, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(SCRIPTS_DIR / name), *args],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_all_pilot_shell_scripts_parse(self) -> None:
        scripts = sorted(SCRIPTS_DIR.glob("*.sh"))
        self.assertTrue(scripts)
        result = subprocess.run(
            ["bash", "-n", *(str(path) for path in scripts)],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_new_operational_scripts_are_executable(self) -> None:
        for name in (
            "codex-remote-home-wrapper.sh",
            "configure-codex-linux-sandbox.sh",
            "configure-opencodex-runtime.sh",
            "configure-remote-codex-routing.sh",
            "install-remote-codex-home.sh",
            "install-remote-codex-relay.sh",
            "manage-remote-codex-home.sh",
            "update-remote-codex.sh",
            "upgrade-opencodex.sh",
        ):
            self.assertTrue(os.access(SCRIPTS_DIR / name, os.X_OK), name)

    def test_opencodex_upgrade_is_explicit_versioned_and_smoke_gated(self) -> None:
        content = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        self.assertIn(
            '"${RUNTIME_ADAPTER}" npm view "${PACKAGE_NAME}@${target_version}" version --json',
            content,
        )
        self.assertIn('EXPECTED_OPENCODEX_VERSION="${target_version}" "${SMOKE_TEST}"', content)
        self.assertIn("EXPECTED_SMOKE_TEST_SHA256", content)
        self.assertIn("verify_companion_smoke", content)
        self.assertLess(
            content.index("verify_companion_smoke\n"),
            content.index("load_runtime_contract\n"),
        )
        self.assertLess(
            content.index("require_root_and_layout\n"),
            content.index("verify_registry_version\n"),
        )
        self.assertIn("adopt-current", content)
        self.assertIn("validate_installed_package_metadata", content)
        self.assertIn("verify_adoptable_running_state", content)
        self.assertIn('write_expected_version "${target_version}"', content)
        self.assertIn('adopt_current_state "${target_version}"', content)
        self.assertIn("snapshot_expected_version", content)
        self.assertIn("restore_expected_version", content)
        self.assertIn("verify_restored_expected_version", content)
        self.assertIn("readonly ROLLBACK_HEALTH_ATTEMPTS=60", content)
        self.assertIn("wait_for_rollback_health", content)
        self.assertIn('"${RUNTIME_ADAPTER}" check >/dev/null 2>&1 || rollback_failed=true', content)
        self.assertIn('.service == "opencodex"', content)
        self.assertIn('.status == "ok"', content)
        self.assertIn('.version == $version', content)
        self.assertIn(
            'verify_local_health_version "${current_version}"',
            content,
        )
        installed_start = content.index("installed_version() {")
        installed_end = content.index("\n}\n\nvalidate_installed_version", installed_start)
        self.assertNotIn("installed_package_version", content[installed_start:installed_end])
        self.assertIn(
            '[[ "${skip_smoke}" == "true" && "${service_was_active}" == "true" ]]',
            content,
        )
        self.assertIn("read_stable_active_state", content)
        self.assertIn("failed|activating|deactivating|unknown", content)
        self.assertGreaterEqual(content.count("verify_active_state_unchanged"), 3)
        current_manifest_check = content.index(
            'validate_installed_package_metadata "${current_version}"'
        )
        self.assertLess(current_manifest_check, content.index("verify_registry_version\n"))
        adopt_start = content.index("adopt_current_state() {")
        adopt_end = content.index("\n}\n\nrestore_prefix_owner", adopt_start)
        adopt_body = content[adopt_start:adopt_end]
        self.assertIn('validate_installed_version "${version}"', adopt_body)
        self.assertIn('validate_installed_package_metadata "${version}"', adopt_body)
        self.assertIn("validate_config", adopt_body)
        self.assertIn('verify_adoptable_running_state "${version}"', adopt_body)
        install_position = content.index('"${RUNTIME_ADAPTER}" npm install')
        post_install_manifest = content.index(
            'validate_installed_package_metadata "${target_version}"',
            install_position,
        )
        self.assertLess(install_position, post_install_manifest)
        self.assertLess(post_install_manifest, content.index("validate_config\n", install_position))
        self.assertIn("rollback", content)
        self.assertIn("/var/backups/opencodex", content)

    def test_runtime_adapter_is_the_only_managed_node_entrypoint(self) -> None:
        upgrade = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        smoke = (SCRIPTS_DIR / "smoke-test.sh").read_text(encoding="utf-8")
        bootstrap = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        adapter_path = "/usr/local/libexec/opencodex-runtime"
        config_path = "/etc/opencodex/runtime.json"

        for content in (upgrade, smoke, bootstrap):
            self.assertIn(adapter_path, content)
            self.assertIn(config_path, content)
        for content in (upgrade, smoke):
            self.assertIn('"${RUNTIME_ADAPTER}" check', content)
            self.assertIn('"${RUNTIME_ADAPTER}" describe --json', content)
            self.assertIn('"${RUNTIME_ADAPTER}" ocx --version', content)
            self.assertNotIn('readonly OCX_BIN=', content)
            self.assertNotIn("/opt/opencodex/bin/ocx", content)

        self.assertIn('"${RUNTIME_ADAPTER}" npm install', upgrade)
        self.assertIn('"${RUNTIME_ADAPTER}" npm install', bootstrap)
        self.assertNotRegex(upgrade, r"(?m)^\s*npm\s+(view|install)\b")
        self.assertNotRegex(bootstrap, r"(?m)^\s*npm\s+install\b")
        self.assertNotIn("command -v node", upgrade)
        self.assertNotIn("command -v npm", upgrade)

    def test_managed_installs_prepare_only_the_pinned_bun_runtime(self) -> None:
        upgrade = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        bootstrap = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        smoke = (SCRIPTS_DIR / "smoke-test.sh").read_text(encoding="utf-8")

        for content in (upgrade, bootstrap):
            self.assertEqual(content.count("--ignore-scripts"), 1)
            self.assertNotIn("--allow-scripts", content)
            self.assertNotIn("--dangerously-allow-all-scripts", content)
            self.assertNotIn("npm config set allow-scripts", content)
            install_position = content.index('"${RUNTIME_ADAPTER}" npm install')
            prepare_position = content.index(
                '"${RUNTIME_ADAPTER}" prepare-bundled-bun', install_position
            )
            restore_position = content.index("restore_prefix_owner", prepare_position)
            root_check_position = content.index(
                '"${RUNTIME_ADAPTER}" check >/dev/null', restore_position
            )
            self.assertLess(install_position, prepare_position)
            self.assertLess(prepare_position, restore_position)
            self.assertLess(restore_position, root_check_position)
        self.assertIn(".dependencies.bun", smoke)
        self.assertIn(".bunVersion == $version", smoke)
        self.assertIn('.bunRuntimeSource == "bundled"', smoke)

    def test_smoke_generation_overlap_uses_a_local_parse_control(self) -> None:
        smoke = (SCRIPTS_DIR / "smoke-test.sh").read_text(encoding="utf-8")
        self.assertIn('printf \'{"model":\\n\' > "${second_payload}"', smoke)
        self.assertIn('[[ "${code}" == "400" ]]', smoke)
        self.assertIn("before provider/model routing", smoke)
        self.assertIn("credentialed external smoke", smoke)

    def test_upgrade_embeds_the_exact_companion_smoke_hash(self) -> None:
        upgrade = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        match = re.search(
            r'readonly EXPECTED_SMOKE_TEST_SHA256="([0-9a-f]{64})"',
            upgrade,
        )
        self.assertIsNotNone(match)
        actual = hashlib.sha256((SCRIPTS_DIR / "smoke-test.sh").read_bytes()).hexdigest()
        self.assertEqual(match.group(1), actual)

    def test_upgrade_rejects_a_modified_companion_smoke_before_runtime_access(self) -> None:
        source = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            script = root / "upgrade-opencodex.sh"
            smoke = root / "smoke-test.sh"
            fake_bin = root / "bin"
            fake_bin.mkdir()
            # This test exercises the real ordering without requiring host root.
            script.write_text(
                source.replace(
                    '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                    '[[ 0 -eq 0 ]] || die "run as root"',
                ),
                encoding="utf-8",
            )
            smoke.write_bytes((SCRIPTS_DIR / "smoke-test.sh").read_bytes() + b"\n# tampered\n")
            for name in ("runuser", "systemctl"):
                fake = fake_bin / name
                fake.write_text("#!/usr/bin/env bash\nexit 99\n", encoding="utf-8")
                fake.chmod(0o755)
            result = subprocess.run(
                ["bash", str(script), "check", "2.17.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("local smoke test SHA-256 mismatch", result.stderr)
        self.assertNotIn("runtime adapter", result.stderr)

    def test_upgrade_rejects_missing_adapter_before_service_or_registry_access(self) -> None:
        source = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            script = root / "upgrade-opencodex.sh"
            smoke = root / "smoke-test.sh"
            fake_bin = root / "bin"
            access_log = root / "access.log"
            fake_bin.mkdir()
            transformed = source.replace(
                '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                '[[ 0 -eq 0 ]] || die "run as root"',
            ).replace(
                'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"',
                f'readonly RUNTIME_ADAPTER="{root / "missing-adapter"}"',
            ).replace(
                'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
                f'readonly RUNTIME_CONFIG="{root / "runtime.json"}"',
            )
            script.write_text(transformed, encoding="utf-8")
            smoke.write_bytes((SCRIPTS_DIR / "smoke-test.sh").read_bytes())
            for name in ("runuser", "systemctl"):
                fake = fake_bin / name
                fake.write_text(
                    f"#!/usr/bin/env bash\nprintf '%s\\n' {name} >> {access_log}\nexit 99\n",
                    encoding="utf-8",
                )
                fake.chmod(0o755)
            result = subprocess.run(
                ["bash", str(script), "check", "2.17.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            runtime_was_accessed = access_log.exists()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime adapter is missing or unsafe", result.stderr)
        self.assertFalse(runtime_was_accessed)

    def test_upgrade_rejects_unsafe_runtime_config_before_invoking_adapter(self) -> None:
        source = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            script = root / "upgrade-opencodex.sh"
            smoke = root / "smoke-test.sh"
            adapter = root / "runtime-adapter"
            config = root / "runtime.json"
            fake_bin = root / "bin"
            access_log = root / "access.log"
            fake_bin.mkdir()
            transformed = source.replace(
                '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                '[[ 0 -eq 0 ]] || die "run as root"',
            ).replace(
                'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"',
                f'readonly RUNTIME_ADAPTER="{adapter}"',
            ).replace(
                'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
                f'readonly RUNTIME_CONFIG="{config}"',
            )
            script.write_text(transformed, encoding="utf-8")
            smoke.write_bytes((SCRIPTS_DIR / "smoke-test.sh").read_bytes())
            adapter.write_text(
                f"#!/usr/bin/env bash\nprintf '%s\\n' adapter >> {access_log}\nexit 99\n",
                encoding="utf-8",
            )
            adapter.chmod(0o755)
            config.write_text("{}\n", encoding="utf-8")
            stat = fake_bin / "stat"
            stat.write_text(
                "#!/usr/bin/env bash\n"
                f'if [[ "${{*: -1}}" == "{adapter}" ]]; then\n'
                "  printf 'root:root:755\\n'\n"
                "else\n"
                "  printf 'root:root:600\\n'\n"
                "fi\n",
                encoding="utf-8",
            )
            stat.chmod(0o755)
            for name in ("runuser", "systemctl"):
                fake = fake_bin / name
                fake.write_text(
                    f"#!/usr/bin/env bash\nprintf '%s\\n' {name} >> {access_log}\nexit 99\n",
                    encoding="utf-8",
                )
                fake.chmod(0o755)
            result = subprocess.run(
                ["bash", str(script), "check", "2.17.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            runtime_was_accessed = access_log.exists()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime contract must be root:root 644", result.stderr)
        self.assertFalse(runtime_was_accessed)

    def test_upgrade_returns_70_when_restored_health_or_config_is_invalid(self) -> None:
        source = (SCRIPTS_DIR / "upgrade-opencodex.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            script = root / "upgrade-opencodex.sh"
            smoke = root / "smoke-test.sh"
            adapter = root / "runtime-adapter"
            config = root / "runtime.json"
            home = root / "home"
            prefix = root / "prefix"
            manifest = prefix / "lib/node_modules/@bitkyc08/opencodex/package.json"
            expected_version = root / "expected-version"
            backup_root = root / "backups"
            config_count = root / "config-count"
            curl_count = root / "curl-count"
            sleep_log = root / "sleep.log"
            npm_install_log = root / "npm-install.log"
            service_mutation_log = root / "service-mutation.log"
            fake_bin = root / "bin"
            fake_bin.mkdir()
            home.mkdir()
            manifest.parent.mkdir(parents=True)
            manifest.write_text(
                '{"name":"@bitkyc08/opencodex","version":"2.17.0"}\n',
                encoding="utf-8",
            )
            config.write_text("{}\n", encoding="utf-8")
            transformed = source.replace(
                '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                '[[ 0 -eq 0 ]] || die "run as root"',
            ).replace(
                'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"',
                f'readonly RUNTIME_ADAPTER="{adapter}"',
            ).replace(
                'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
                f'readonly RUNTIME_CONFIG="{config}"',
            ).replace(
                'readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"',
                f'readonly EXPECTED_VERSION_FILE="{expected_version}"',
            ).replace(
                'readonly BACKUP_ROOT="/var/backups/opencodex"',
                f'readonly BACKUP_ROOT="{backup_root}"',
            )
            script.write_text(transformed, encoding="utf-8")
            smoke.write_bytes((SCRIPTS_DIR / "smoke-test.sh").read_bytes())
            description = json.dumps(
                {
                    "home": str(home),
                    "prefix": str(prefix),
                    "package_manifest": str(manifest),
                },
                separators=(",", ":"),
            )
            adapter.write_text(
                "#!/usr/bin/env bash\n"
                "set -eu\n"
                f'manifest="{manifest}"\n'
                f'config_count="{config_count}"\n'
                'case "${1:-}" in\n'
                "  check) exit 0 ;;\n"
                f"  describe) printf '%s\\n' '{description}' ;;\n"
                "  prepare-bundled-bun)\n"
                "    version=\"$(jq -r .version \"${manifest}\")\"\n"
                "    [[ \"${2:-}\" == \"${version}\" ]] || exit 1\n"
                "    printf 'opencodex %s\\n' \"${version}\"\n"
                "    ;;\n"
                "  ocx)\n"
                "    shift\n"
                '    if [[ "${1:-}" == "--version" ]]; then\n'
                "      version=\"$(jq -r .version \"${manifest}\")\"\n"
                "      printf 'opencodex %s\\n' \"${version}\"\n"
                '    elif [[ "${1:-}" == "config" && "${2:-}" == "validate" ]]; then\n'
                "      count=0\n"
                '      [[ ! -f "${config_count}" ]] || count="$(<"${config_count}")"\n'
                "      count=$((count + 1))\n"
                '      printf "%s\\n" "${count}" > "${config_count}"\n'
                '      [[ "${count}" != "2" ]]\n'
                "    else\n"
                "      exit 2\n"
                "    fi\n"
                "    ;;\n"
                "  npm)\n"
                "    shift\n"
                '    if [[ "${1:-}" == "view" ]]; then\n'
                "      printf '\"2.18.0\"\\n'\n"
                '    elif [[ "${1:-}" == "install" ]]; then\n'
                f'      printf "install\\n" >> "{npm_install_log}"\n'
                "      printf '%s\\n' "
                "'{\"name\":\"@bitkyc08/opencodex\",\"version\":\"2.18.0\"}' "
                '> "${manifest}"\n'
                "    else\n"
                "      exit 2\n"
                "    fi\n"
                "    ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            adapter.chmod(0o755)

            runuser = fake_bin / "runuser"
            runuser.write_text(
                "#!/usr/bin/env bash\n"
                "while (($#)); do\n"
                '  if [[ "$1" == "--" ]]; then shift; exec "$@"; fi\n'
                "  shift\n"
                "done\n"
                "exit 2\n",
                encoding="utf-8",
            )
            runuser.chmod(0o755)
            systemctl = fake_bin / "systemctl"
            systemctl.write_text(
                "#!/usr/bin/env bash\n"
                'case "${1:-}" in\n'
                "  show) printf 'active\\n'; exit 0 ;;\n"
                "  is-active|is-enabled|start|stop|enable|disable) exit 0 ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            systemctl.chmod(0o755)
            curl = fake_bin / "curl"
            curl.write_text(
                "#!/usr/bin/env bash\n"
                "count=0\n"
                f'[[ ! -f "{curl_count}" ]] || count="$(<"{curl_count}")"\n'
                "count=$((count + 1))\n"
                f'printf "%s\\n" "${{count}}" > "{curl_count}"\n'
                'if [[ "${count}" == "1" ]]; then\n'
                "  printf '%s\\n' "
                "'{\"service\":\"opencodex\",\"status\":\"ok\",\"version\":\"2.17.0\"}'\n"
                "else\n"
                "  printf '%s\\n' "
                "'{\"service\":\"opencodex\",\"status\":\"degraded\",\"version\":\"2.17.0\"}'\n"
                "fi\n",
                encoding="utf-8",
            )
            curl.chmod(0o755)
            stat = fake_bin / "stat"
            stat.write_text(
                "#!/usr/bin/env bash\n"
                f'case "${{*: -1}}" in\n'
                f'  "{adapter}") printf "root:root:755\\n" ;;\n'
                f'  "{config}") printf "root:root:644\\n" ;;\n'
                '  *) exec /usr/bin/stat "$@" ;;\n'
                "esac\n",
                encoding="utf-8",
            )
            stat.chmod(0o755)
            install = fake_bin / "install"
            install.write_text(
                "#!/usr/bin/env bash\n"
                "args=()\n"
                "while (($#)); do\n"
                '  case "$1" in\n'
                "    -o|-g) shift 2 ;;\n"
                '    *) args+=("$1"); shift ;;\n'
                "  esac\n"
                "done\n"
                'exec /usr/bin/install "${args[@]}"\n',
                encoding="utf-8",
            )
            install.chmod(0o755)
            chown = fake_bin / "chown"
            chown.write_text("#!/usr/bin/env bash\nexit 0\n", encoding="utf-8")
            chown.chmod(0o755)
            sleep = fake_bin / "sleep"
            sleep.write_text(
                f"#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> {sleep_log}\nexit 0\n",
                encoding="utf-8",
            )
            sleep.chmod(0o755)
            systemctl.write_text(
                "#!/usr/bin/env bash\n"
                'case "${1:-}" in\n'
                "  show) printf 'activating\\n'; exit 0 ;;\n"
                "  is-enabled) exit 0 ;;\n"
                f"  start|stop|enable|disable) printf '%s\\n' \"$1\" >> {service_mutation_log}; exit 0 ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            unstable_result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            unstable_left_package_unchanged = (
                json.loads(manifest.read_text(encoding="utf-8"))["version"] == "2.17.0"
            )
            unstable_created_no_backup = not backup_root.exists()
            unstable_invoked_no_npm_install = not npm_install_log.exists()
            unstable_invoked_no_service_mutation = not service_mutation_log.exists()
            config_count.unlink()
            systemctl.write_text(
                "#!/usr/bin/env bash\n"
                'case "${1:-}" in\n'
                "  show) printf 'active\\n'; exit 0 ;;\n"
                "  is-active|is-enabled|start|stop|enable|disable) exit 0 ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            skip_result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0", "--skip-smoke"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            skip_left_package_unchanged = (
                json.loads(manifest.read_text(encoding="utf-8"))["version"] == "2.17.0"
            )
            skip_created_no_backup = not backup_root.exists()
            config_count.unlink()
            curl_count.unlink()
            result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            restored_version = json.loads(manifest.read_text(encoding="utf-8"))["version"]
            expected_was_restored_absent = not expected_version.exists()
            config_count.unlink()
            adapter.write_text(
                adapter.read_text(encoding="utf-8").replace(
                    '[[ "${count}" != "2" ]]',
                    '[[ "${count}" != "2" && "${count}" != "3" ]]',
                ),
                encoding="utf-8",
            )
            curl.write_text(
                "#!/usr/bin/env bash\n"
                "printf '%s\\n' "
                "'{\"service\":\"opencodex\",\"status\":\"ok\",\"version\":\"2.17.0\"}'\n",
                encoding="utf-8",
            )
            config_result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            config_count.unlink()
            systemctl.write_text(
                "#!/usr/bin/env bash\n"
                'case "${1:-}" in\n'
                "  show) printf 'inactive\\n'; exit 0 ;;\n"
                "  is-active) exit 1 ;;\n"
                "  is-enabled|start|stop|enable|disable) exit 0 ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            backups_before_stopped_apply = len(list(backup_root.glob("upgrade-*")))
            stopped_result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0", "--skip-smoke"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            stopped_apply_reached_backup = (
                len(list(backup_root.glob("upgrade-*"))) > backups_before_stopped_apply
            )
            config_count.unlink()
            adapter.write_text(
                adapter.read_text(encoding="utf-8").replace(
                    '[[ "${count}" != "2" && "${count}" != "3" ]]',
                    '[[ "${count}" != "2" ]]',
                ),
                encoding="utf-8",
            )
            systemctl.write_text(
                "#!/usr/bin/env bash\n"
                'case "${1:-}" in\n'
                "  show) printf 'active\\n'; exit 0 ;;\n"
                "  is-active|is-enabled|start|stop|enable|disable) exit 0 ;;\n"
                "  *) exit 2 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            curl_count.unlink(missing_ok=True)
            sleep_log.unlink(missing_ok=True)
            curl.write_text(
                "#!/usr/bin/env bash\n"
                "count=0\n"
                f'[[ ! -f "{curl_count}" ]] || count="$(<"{curl_count}")"\n'
                "count=$((count + 1))\n"
                f'printf "%s\\n" "${{count}}" > "{curl_count}"\n'
                'case "${count}" in\n'
                "  1|4) printf '%s\\n' "
                "'{\"service\":\"opencodex\",\"status\":\"ok\",\"version\":\"2.17.0\"}' ;;\n"
                "  *) exit 22 ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            delayed_result = subprocess.run(
                ["bash", str(script), "apply", "2.18.0"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            delayed_sleep_count = (
                len(sleep_log.read_text(encoding="utf-8").splitlines())
                if sleep_log.exists()
                else 0
            )
        self.assertNotEqual(unstable_result.returncode, 0)
        self.assertIn("ActiveState is not stable: activating", unstable_result.stderr)
        self.assertTrue(unstable_left_package_unchanged)
        self.assertTrue(unstable_created_no_backup)
        self.assertTrue(unstable_invoked_no_npm_install)
        self.assertTrue(unstable_invoked_no_service_mutation)
        self.assertNotEqual(skip_result.returncode, 0)
        self.assertIn("--skip-smoke is allowed only when", skip_result.stderr)
        self.assertTrue(skip_left_package_unchanged)
        self.assertTrue(skip_created_no_backup)
        self.assertEqual(result.returncode, 70, result.stderr)
        self.assertIn("CRITICAL: rollback could not be fully verified", result.stderr)
        self.assertNotIn("Previous OpenCodex prefix and service state were restored", result.stderr)
        self.assertEqual(restored_version, "2.17.0")
        self.assertTrue(expected_was_restored_absent)
        self.assertEqual(config_result.returncode, 70, config_result.stderr)
        self.assertIn("CRITICAL: rollback could not be fully verified", config_result.stderr)
        self.assertNotIn("--skip-smoke is allowed only when", stopped_result.stderr)
        self.assertTrue(stopped_apply_reached_backup)
        self.assertEqual(delayed_result.returncode, 1, delayed_result.stderr)
        self.assertIn(
            "Previous OpenCodex prefix and service state were restored",
            delayed_result.stderr,
        )
        self.assertNotIn("CRITICAL:", delayed_result.stderr)
        self.assertEqual(delayed_sleep_count, 2)

    def test_bootstrap_installs_runtime_assets_only_on_a_fresh_host(self) -> None:
        content = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        self.assertIn('RUNTIME_ADAPTER_SOURCE="${ASSET_DIR}/libexec/opencodex-runtime"', content)
        self.assertIn("atomic_install_asset", content)
        self.assertIn('mv -f "${runtime_candidate}" "${RUNTIME_CONFIG}"', content)
        self.assertIn("schema_version: 1", content)
        self.assertIn('runtime_kind: "node"', content)
        self.assertIn("--arg node_bin", content)
        self.assertIn("--arg npm_cli", content)
        self.assertIn("--arg ocx_entry", content)
        self.assertIn("existing or unowned partial OpenCodex deployment found", content)
        self.assertIn("use upgrade-opencodex.sh", content)
        self.assertIn("configure-opencodex-runtime.sh only for verified legacy migration", content)
        self.assertIn('/run/systemd/system/opencodex.service', content)
        self.assertIn('/usr/lib/systemd/system/opencodex.service', content)
        self.assertIn('/lib/systemd/system/opencodex.service', content)
        preflight = content.index("for managed_artifact in")
        self.assertLess(preflight, content.index("apt-get update"))
        self.assertLess(preflight, content.index("systemctl disable --now rpcbind.socket"))
        self.assertLess(preflight, content.index('"${RUNTIME_ADAPTER}" npm install'))
        self.assertNotIn("opencodex_was_active", content)
        self.assertNotIn("opencodex_was_enabled", content)
        self.assertLess(
            content.index('atomic_install_asset "${RUNTIME_ADAPTER_SOURCE}"'),
            content.index('"${RUNTIME_ADAPTER}" npm install'),
        )
        self.assertLess(
            content.index('"${RUNTIME_ADAPTER}" npm install'),
            content.index('"${RUNTIME_ADAPTER}" check'),
        )
        self.assertIn("BOOTSTRAP_MARKER_CONTENT", content)
        self.assertIn("recover_interrupted_fresh_bootstrap", content)
        self.assertIn("remove_fresh_deployment_artifacts", content)
        self.assertLess(
            content.index("create_bootstrap_marker\n"),
            content.index('atomic_install_asset "${RUNTIME_ADAPTER_SOURCE}"'),
        )
        self.assertLess(
            content.index("trap cleanup EXIT"),
            content.index('atomic_install_asset "${RUNTIME_ADAPTER_SOURCE}"'),
        )
        self.assertLess(
            content.rindex('rm -f -- "${BOOTSTRAP_MARKER}"'),
            content.rindex("bootstrap_rollback_required=false"),
        )

    def test_bootstrap_rejects_second_or_partial_install_before_mutation(self) -> None:
        source = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        adapter_source = (PILOT / "libexec" / "opencodex-runtime").read_bytes()
        for artifact_kind in ("package_manifest", "dangling_runtime_config"):
            with self.subTest(artifact_kind=artifact_kind), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                scripts = root / "pilot" / "scripts"
                libexec = root / "pilot" / "libexec"
                fake_bin = root / "bin"
                scripts.mkdir(parents=True)
                libexec.mkdir(parents=True)
                fake_bin.mkdir()
                home = root / "home"
                prefix = root / "prefix"
                adapter = root / "installed-runtime-adapter"
                runtime_config = root / "runtime.json"
                expected_version = root / "expected-version"
                unit = root / "opencodex.service"
                os_release = root / "os-release"
                access_log = root / "mutation.log"
                script = scripts / "bootstrap-host.sh"
                transformed = source.replace(
                    '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                    '[[ 0 -eq 0 ]] || die "run as root"',
                ).replace(
                    'readonly OPENCODEX_HOME="/var/lib/opencodex"',
                    f'readonly OPENCODEX_HOME="{home}"',
                ).replace(
                    'readonly OPENCODEX_PREFIX="/opt/opencodex"',
                    f'readonly OPENCODEX_PREFIX="{prefix}"',
                ).replace(
                    'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"',
                    f'readonly RUNTIME_ADAPTER="{adapter}"',
                ).replace(
                    'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
                    f'readonly RUNTIME_CONFIG="{runtime_config}"',
                ).replace(
                    'readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"',
                    f'readonly EXPECTED_VERSION_FILE="{expected_version}"',
                ).replace(
                    'readonly SYSTEMD_UNIT="/etc/systemd/system/opencodex.service"',
                    f'readonly SYSTEMD_UNIT="{unit}"',
                ).replace(
                    'readonly OS_RELEASE_FILE="/etc/os-release"',
                    f'readonly OS_RELEASE_FILE="{os_release}"',
                )
                script.write_text(transformed, encoding="utf-8")
                (libexec / "opencodex-runtime").write_bytes(adapter_source)
                os_release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n', encoding="utf-8")
                if artifact_kind == "package_manifest":
                    manifest = prefix / "lib/node_modules/@bitkyc08/opencodex/package.json"
                    manifest.parent.mkdir(parents=True)
                    manifest.write_text("{}\n", encoding="utf-8")
                else:
                    runtime_config.symlink_to(root / "missing-target")

                dpkg = fake_bin / "dpkg"
                dpkg.write_text(
                    "#!/usr/bin/env bash\n"
                    '[[ "${1:-}" == "--print-architecture" ]] && printf "amd64\\n"\n',
                    encoding="utf-8",
                )
                dpkg.chmod(0o755)
                for name in ("apt-get", "systemctl", "runuser", "npm"):
                    fake = fake_bin / name
                    fake.write_text(
                        f"#!/usr/bin/env bash\nprintf '%s\\n' {name} >> {access_log}\nexit 99\n",
                        encoding="utf-8",
                    )
                    fake.chmod(0o755)
                result = subprocess.run(
                    ["bash", str(script)],
                    check=False,
                    capture_output=True,
                    text=True,
                    env={
                        **os.environ,
                        "OPENCODEX_VERSION": "2.17.0",
                        "PATH": f"{fake_bin}:{os.environ['PATH']}",
                    },
                )
                mutation_was_attempted = access_log.exists()
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("existing or unowned partial OpenCodex deployment found", result.stderr)
            self.assertFalse(mutation_was_attempted)

    def test_bootstrap_requires_an_explicit_version_before_mutation(self) -> None:
        source = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            script = root / "bootstrap-host.sh"
            os_release = root / "os-release"
            fake_bin = root / "bin"
            access_log = root / "mutation.log"
            fake_bin.mkdir()
            transformed = source.replace(
                '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                '[[ 0 -eq 0 ]] || die "run as root"',
            ).replace(
                'readonly OS_RELEASE_FILE="/etc/os-release"',
                f'readonly OS_RELEASE_FILE="{os_release}"',
            )
            script.write_text(transformed, encoding="utf-8")
            os_release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n', encoding="utf-8")
            dpkg = fake_bin / "dpkg"
            dpkg.write_text(
                "#!/usr/bin/env bash\n"
                '[[ "${1:-}" == "--print-architecture" ]] && printf "amd64\\n"\n',
                encoding="utf-8",
            )
            dpkg.chmod(0o755)
            for name in ("apt-get", "systemctl", "runuser", "npm"):
                fake = fake_bin / name
                fake.write_text(
                    f"#!/usr/bin/env bash\nprintf '%s\\n' {name} >> {access_log}\nexit 99\n",
                    encoding="utf-8",
                )
                fake.chmod(0o755)
            environment = {**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"}
            environment.pop("OPENCODEX_VERSION", None)
            result = subprocess.run(
                ["bash", str(script)],
                check=False,
                capture_output=True,
                text=True,
                env=environment,
            )
            mutation_was_attempted = access_log.exists()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("OPENCODEX_VERSION must be an explicit semver", result.stderr)
        self.assertFalse(mutation_was_attempted)

    def test_bootstrap_recovers_owned_interrupted_fresh_state_before_retry(self) -> None:
        source = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        adapter_source = (PILOT / "libexec" / "opencodex-runtime").read_bytes()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            scripts = root / "pilot" / "scripts"
            libexec = root / "pilot" / "libexec"
            fake_bin = root / "bin"
            scripts.mkdir(parents=True)
            libexec.mkdir(parents=True)
            fake_bin.mkdir()
            home = root / "home"
            prefix = root / "prefix"
            adapter = root / "installed-runtime-adapter"
            runtime_config = root / "runtime.json"
            expected_version = root / "expected-version"
            unit = root / "opencodex.service"
            backup_root = root / "backups"
            marker = backup_root / "bootstrap-fresh.pending"
            os_release = root / "os-release"
            apt_log = root / "apt.log"
            script = scripts / "bootstrap-host.sh"
            transformed = source.replace(
                '[[ "${EUID}" -eq 0 ]] || die "run as root"',
                '[[ 0 -eq 0 ]] || die "run as root"',
            ).replace(
                'readonly OPENCODEX_HOME="/var/lib/opencodex"',
                f'readonly OPENCODEX_HOME="{home}"',
            ).replace(
                'readonly OPENCODEX_PREFIX="/opt/opencodex"',
                f'readonly OPENCODEX_PREFIX="{prefix}"',
            ).replace(
                'readonly RUNTIME_ADAPTER="/usr/local/libexec/opencodex-runtime"',
                f'readonly RUNTIME_ADAPTER="{adapter}"',
            ).replace(
                'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
                f'readonly RUNTIME_CONFIG="{runtime_config}"',
            ).replace(
                'readonly EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"',
                f'readonly EXPECTED_VERSION_FILE="{expected_version}"',
            ).replace(
                'readonly SYSTEMD_UNIT="/etc/systemd/system/opencodex.service"',
                f'readonly SYSTEMD_UNIT="{unit}"',
            ).replace(
                'readonly OS_RELEASE_FILE="/etc/os-release"',
                f'readonly OS_RELEASE_FILE="{os_release}"',
            ).replace(
                'readonly BACKUP_ROOT="/var/backups/opencodex"',
                f'readonly BACKUP_ROOT="{backup_root}"',
            )
            script.write_text(transformed, encoding="utf-8")
            (libexec / "opencodex-runtime").write_bytes(adapter_source)
            os_release.write_text('ID=ubuntu\nVERSION_ID="24.04"\n', encoding="utf-8")
            prefix.mkdir()
            (prefix / "partial-package").write_text("partial\n", encoding="utf-8")
            adapter.write_text("partial\n", encoding="utf-8")
            runtime_config.write_text("{}\n", encoding="utf-8")
            expected_version.write_text("2.17.0\n", encoding="utf-8")
            backup_root.mkdir()
            marker.write_text("opencodex-relay-bootstrap-fresh-v1\n", encoding="utf-8")

            dpkg = fake_bin / "dpkg"
            dpkg.write_text(
                "#!/usr/bin/env bash\n"
                '[[ "${1:-}" == "--print-architecture" ]] && printf "amd64\\n"\n',
                encoding="utf-8",
            )
            dpkg.chmod(0o755)
            stat = fake_bin / "stat"
            stat.write_text(
                "#!/usr/bin/env bash\n"
                f'if [[ "${{*: -1}}" == "{marker}" ]]; then\n'
                "  printf 'root:root:600\\n'\n"
                "else\n"
                '  exec /usr/bin/stat "$@"\n'
                "fi\n",
                encoding="utf-8",
            )
            stat.chmod(0o755)
            apt = fake_bin / "apt-get"
            apt.write_text(
                f"#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> {apt_log}\nexit 99\n",
                encoding="utf-8",
            )
            apt.chmod(0o755)
            result = subprocess.run(
                ["bash", str(script)],
                check=False,
                capture_output=True,
                text=True,
                env={
                    **os.environ,
                    "OPENCODEX_VERSION": "2.17.0",
                    "PATH": f"{fake_bin}:{os.environ['PATH']}",
                },
            )
            recovered = all(
                not path.exists() and not path.is_symlink()
                for path in (prefix, adapter, runtime_config, expected_version, marker)
            )
            retry_reached_apt = apt_log.exists()
        self.assertEqual(result.returncode, 99, result.stderr)
        self.assertTrue(recovered)
        self.assertTrue(retry_reached_apt)
        self.assertNotIn("unowned partial", result.stderr)

    def test_bootstrap_failure_cleanup_removes_fresh_managed_artifacts(self) -> None:
        source = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        remove_start = source.index("remove_fresh_deployment_artifacts() {")
        remove_end = source.index("\n}\n\nrecover_interrupted_fresh_bootstrap", remove_start) + 2
        cleanup_start = source.index("cleanup() {")
        cleanup_end = source.index("\n}\ntrap cleanup EXIT", cleanup_start) + 2
        remove_function = source[remove_start:remove_end]
        cleanup_function = source[cleanup_start:cleanup_end]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            prefix = root / "prefix"
            adapter = root / "runtime-adapter"
            runtime_config = root / "runtime.json"
            expected_version = root / "expected-version"
            unit = root / "opencodex.service"
            marker = root / "bootstrap-fresh.pending"
            prefix.mkdir()
            (prefix / "partial-package").write_text("partial\n", encoding="utf-8")
            for path in (adapter, runtime_config, expected_version):
                path.write_text("partial\n", encoding="utf-8")
            marker.write_text("opencodex-relay-bootstrap-fresh-v1\n", encoding="utf-8")
            harness = root / "cleanup-harness.sh"
            harness.write_text(
                "#!/usr/bin/env bash\n"
                "set -uo pipefail\n"
                f'readonly SYSTEMD_UNIT="{unit}"\n'
                f'readonly EXPECTED_VERSION_FILE="{expected_version}"\n'
                f'readonly RUNTIME_CONFIG="{runtime_config}"\n'
                f'readonly RUNTIME_ADAPTER="{adapter}"\n'
                f'readonly OPENCODEX_PREFIX="{prefix}"\n'
                f'readonly BOOTSTRAP_MARKER="{marker}"\n'
                "bootstrap_rollback_required=true\n"
                f"{remove_function}\n"
                "restore_prefix_owner() { :; }\n"
                f"{cleanup_function}\n"
                "trap cleanup EXIT\n"
                "false\n",
                encoding="utf-8",
            )
            result = subprocess.run(
                ["bash", str(harness)],
                check=False,
                capture_output=True,
                text=True,
            )
            cleaned = all(
                not path.exists() and not path.is_symlink()
                for path in (prefix, adapter, runtime_config, expected_version, marker)
            )
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertTrue(cleaned)

    def test_runtime_contract_directory_stays_traversable_without_exposing_credentials(self) -> None:
        bootstrap = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        gateway_key = (SCRIPTS_DIR / "configure-gateway-key.sh").read_text(encoding="utf-8")
        smoke = (SCRIPTS_DIR / "smoke-test.sh").read_text(encoding="utf-8")
        self.assertIn(
            "install -d -o root -g root -m 0755 /etc/opencodex",
            bootstrap,
        )
        self.assertIn('install -d -o root -g root -m 0755 "${KEY_DIR}"', gateway_key)
        self.assertIn('install -d -o root -g root -m 0700 "${MAP_DIR}"', gateway_key)
        self.assertIn('chmod 0600 "${key_tmp}"', gateway_key)
        self.assertIn('chmod 0644 "${runtime_candidate}"', bootstrap)
        self.assertIn(
            'check_safe_root_directory "$(dirname -- "${RUNTIME_CONFIG}")" 755',
            smoke,
        )

    def test_pilot_readme_uses_explicit_fresh_bootstrap_and_runtime_adapter(self) -> None:
        content = (PILOT / "README.md").read_text(encoding="utf-8")
        self.assertIn('VERSION=\'REPLACE_WITH_REVIEWED_EXACT_VERSION\'', content)
        self.assertIn(
            'sudo env OPENCODEX_VERSION="${VERSION}" ./scripts/bootstrap-host.sh',
            content,
        )
        self.assertIn("신규 호스트 전용", content)
        self.assertIn("upgrade-opencodex.sh", content)
        self.assertIn("configure-opencodex-runtime.sh", content)
        self.assertIn(
            "sudo -u opencodex /usr/local/libexec/opencodex-runtime ocx setup",
            content,
        )
        self.assertNotIn("/opt/opencodex/bin/ocx", content)

    def test_remote_updater_isolated_installer_and_verifies_remote_transport(self) -> None:
        content = (SCRIPTS_DIR / "update-remote-codex.sh").read_text(encoding="utf-8")
        self.assertIn('CODEX_HOME="${REMOTE_HOME_PATH}"', content)
        self.assertIn('CODEX_INSTALL_DIR="${INSTALLER_BIN_DIR}"', content)
        self.assertIn("CODEX_NON_INTERACTIVE=1", content)
        self.assertIn("--allow-remote-interruption", content)
        self.assertIn('"${MANAGER}" restart-daemon', content)
        self.assertIn('"${WRAPPER_TARGET}" app-server daemon enable-remote-control', content)
        self.assertIn('export CODEX_HOME="${REMOTE_HOME_PATH}"', content)
        self.assertIn(".managedCodexVersion == $expected", content)
        self.assertIn(".appServerVersion == $expected", content)
        self.assertIn("verify_proxy", content)
        self.assertIn("restore_current_link", content)
        self.assertIn('"${MANAGER}" set-default-model --allow-remote-interruption', content)
        self.assertIn('"${MANAGER}" verify-default-model', content)
        self.assertIn('if [[ "${current_routing}" == "local-relay" ]]; then', content)
        self.assertGreaterEqual(content.count('"${MANAGER}" verify-interactive-profile'), 2)
        self.assertGreaterEqual(content.count('"${MANAGER}" verify-relay-health'), 2)
        local_start = content.index('if [[ "${current_routing}" == "local-relay" ]]; then')
        local_branch = content[local_start:content.index("\n  fi", local_start)]
        self.assertLess(
            local_branch.index('"${MANAGER}" verify-default-model'),
            local_branch.index("else"),
        )

    def test_remote_updater_preserves_bare_local_relay_root_before_and_after(self) -> None:
        source = (SCRIPTS_DIR / "update-remote-codex.sh").read_text(encoding="utf-8")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            remote_home = root / "remote-home"
            config_dir = root / "config"
            install_root = root / "install"
            user_bin = root / "user-bin"
            fake_bin = root / "fake-bin"
            release = remote_home / "packages" / "standalone" / "releases" / "0.149.1"
            for path in (config_dir, install_root, user_bin, fake_bin, release):
                path.mkdir(parents=True)

            current = remote_home / "packages" / "standalone" / "current"
            current.symlink_to(release)
            managed_codex = release / "codex"
            managed_codex.write_text(
                "#!/usr/bin/env bash\n"
                "[[ \"$1\" == --version ]] || exit 81\n"
                "printf 'codex-cli 0.149.1\\n'\n",
                encoding="utf-8",
            )

            remote_config = config_dir / "remote-opencodex.json"
            remote_config.write_text(
                remote_config_json(remote_home, "loopback", "local-relay"),
                encoding="utf-8",
            )
            remote_config.chmod(0o600)
            root_config = remote_home / "config.toml"
            root_config.write_text(
                'model = "gpt-5.6-luna"\n\n'
                '[projects."/home/ubuntu"]\ntrust_level = "untrusted"\n',
                encoding="utf-8",
            )
            root_config.chmod(0o600)
            catalog = remote_home / "opencodex-catalog.json"
            catalog.write_text(
                json.dumps(
                    {
                        "models": [
                            {"id": "gpt-5.6-luna"},
                            {"id": "opencode-go-responses/gpt-5.6-luna"},
                        ]
                    }
                )
                + "\n",
                encoding="utf-8",
            )
            catalog.chmod(0o600)

            manager_log = root / "manager.log"
            manager = install_root / "manage-remote-codex-home.sh"
            manager.write_text(
                "#!/usr/bin/env bash\n"
                "set -eu\n"
                f'root_config="{root_config}"\n'
                f'log="{manager_log}"\n'
                'printf \'%s\\n\' "$1" >> "$log"\n'
                'case "$1" in\n'
                "  verify-default-model)\n"
                '    grep -Fxq \'model = "gpt-5.6-luna"\' "$root_config"\n'
                "    printf 'default_model_match=1 model=gpt-5.6-luna\\n'\n"
                "    ;;\n"
                "  status|verify-interactive-profile|verify-relay-health|repair-wrapper|refresh) ;;\n"
                "  restart-daemon|set-default-model) exit 82 ;;\n"
                "  *) exit 83 ;;\n"
                "esac\n",
                encoding="utf-8",
            )

            daemon_state = json.dumps(
                {
                    "status": "running",
                    "managedCodexVersion": "0.149.1",
                    "cliVersion": "0.149.1",
                    "appServerVersion": "0.149.1",
                }
            )
            wrapper = user_bin / "codex"
            wrapper_content = (
                "#!/usr/bin/env bash\n"
                f'catalog="{catalog}"\n'
                'case "$*" in\n'
                '  "debug models") cat "$catalog" ;;\n'
                '  "app-server daemon enable-remote-control") ;;\n'
                f'  "app-server daemon version") printf \'%s\\n\' \'{daemon_state}\' ;;\n'
                '  "app-server proxy") cat >/dev/null; '
                "printf 'HTTP/1.1 101 Switching Protocols\\r\\n\\r\\n' ;;\n"
                "  *) exit 84 ;;\n"
                "esac\n"
            )
            wrapper.write_text(wrapper_content, encoding="utf-8")
            (install_root / "codex-remote-home-wrapper.sh").write_text(
                wrapper_content, encoding="utf-8"
            )

            updater = root / "update-remote-codex.sh"
            replacements = {
                'readonly REMOTE_HOME_PATH="/home/ubuntu/.codex-remote-opencodex"':
                    f'readonly REMOTE_HOME_PATH="{remote_home}"',
                'readonly CONFIG_FILE="/home/ubuntu/.config/opencodex-relay/remote-opencodex.json"':
                    f'readonly CONFIG_FILE="{remote_config}"',
                'readonly CONFIG_LOADER="/home/ubuntu/.local/lib/opencodex-relay/load-remote-config.sh"':
                    f'readonly CONFIG_LOADER="{REMOTE_CONFIG_LOADER}"',
                'readonly INSTALL_ROOT="/home/ubuntu/.local/lib/opencodex-relay"':
                    f'readonly INSTALL_ROOT="{install_root}"',
                'readonly WRAPPER_TARGET="/home/ubuntu/.local/bin/codex"':
                    f'readonly WRAPPER_TARGET="{wrapper}"',
            }
            for before, after in replacements.items():
                self.assertIn(before, source)
                source = source.replace(before, after)
            updater.write_text(source, encoding="utf-8")

            (fake_bin / "id").write_text(
                "#!/usr/bin/env bash\n"
                '[[ "$1" == -un ]] && { printf \'ubuntu\\n\'; exit 0; }\n'
                'exec /usr/bin/id "$@"\n',
                encoding="utf-8",
            )
            (fake_bin / "stat").write_text(
                "#!/usr/bin/env bash\n"
                f'if [[ "$3" == "{remote_config}" ]]; then printf \'ubuntu:ubuntu:600\\n\'; '
                f'elif [[ "$3" == "{manager}" ]]; then printf \'ubuntu:ubuntu:700\\n\'; '
                'else exec /usr/bin/stat "$@"; fi\n',
                encoding="utf-8",
            )
            (fake_bin / "curl").write_text(
                "#!/usr/bin/env bash\n"
                "output=\n"
                "while (($# > 0)); do\n"
                '  if [[ "$1" == -o ]]; then output="$2"; shift 2; else shift; fi\n'
                "done\n"
                '[[ -n "$output" ]] || exit 85\n'
                "printf '%s\\n' '#!/usr/bin/env sh' 'exit 0' > \"$output\"\n",
                encoding="utf-8",
            )
            (fake_bin / "timeout").write_text(
                "#!/usr/bin/env bash\nshift\nexec \"$@\"\n",
                encoding="utf-8",
            )
            for path in (
                managed_codex,
                manager,
                wrapper,
                updater,
                fake_bin / "id",
                fake_bin / "stat",
                fake_bin / "curl",
                fake_bin / "timeout",
            ):
                path.chmod(0o700)

            before = root_config.read_bytes()
            before_hash = hashlib.sha256(before).hexdigest()
            before_mtime = root_config.stat().st_mtime_ns
            result = subprocess.run(
                ["bash", str(updater), "apply", "--allow-remote-interruption"],
                check=False,
                capture_output=True,
                text=True,
                env={**os.environ, "PATH": f"{fake_bin}:{os.environ['PATH']}"},
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            calls = manager_log.read_text(encoding="utf-8").splitlines()
            self.assertEqual(root_config.read_bytes(), before)
            self.assertEqual(
                hashlib.sha256(root_config.read_bytes()).hexdigest(), before_hash
            )
            self.assertEqual(root_config.stat().st_mtime_ns, before_mtime)
            self.assertEqual(calls.count("verify-default-model"), 2)
            self.assertNotIn("set-default-model", calls)
            self.assertNotIn("restart-daemon", calls)
            self.assertLess(
                calls.index("verify-default-model"), calls.index("repair-wrapper")
            )
            self.assertLess(
                calls.index("refresh"), len(calls) - 1 - calls[::-1].index("verify-default-model")
            )

    def test_remote_manager_uses_only_the_approved_recovery_fallback_after_a_refused_restart(self) -> None:
        content = (SCRIPTS_DIR / "manage-remote-codex-home.sh").read_text(encoding="utf-8")
        self.assertIn("daemon_restart_fallback=approved_foreign_daemon", content)
        self.assertIn("if ! recover_daemon; then", content)
        self.assertIn("refusing SIGKILL", content)
        self.assertLess(
            content.index('"$MANAGED_CODEX" app-server daemon restart'),
            content.index("daemon_restart_fallback=approved_foreign_daemon"),
        )

    def test_relay_catalog_has_one_writer_and_a_separate_idle_activation_timer(self) -> None:
        manager = (SCRIPTS_DIR / "manage-remote-codex-home.sh").read_text(encoding="utf-8")
        installer = (SCRIPTS_DIR / "install-remote-codex-home.sh").read_text(encoding="utf-8")
        service = (PILOT / "systemd" / "opencodex-remote-relay-catalog-activation.service").read_text(encoding="utf-8")
        timer = (PILOT / "systemd" / "opencodex-remote-relay-catalog-activation.timer").read_text(encoding="utf-8")
        self.assertIn("apply-relay-catalog", manager)
        self.assertIn("relay_catalog_refresh=owned_by_relay", manager)
        self.assertIn('if [[ "$ROUTING_MODE" == "relay" ]]; then', manager)
        self.assertIn("relay_active_requests", manager)
        refresh = manager[manager.index("refresh() {"):manager.index("restart_daemon() {")]
        self.assertNotIn("apply_relay_catalog", refresh)
        self.assertIn("opencodex-remote-relay-catalog-activation", installer)
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-catalog-refresh.timer",
            installer,
        )
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-relay-catalog-activation.timer",
            installer,
        )
        routing = (SCRIPTS_DIR / "configure-remote-codex-routing.sh").read_text(encoding="utf-8")
        self.assertIn(
            "systemctl --user enable --now opencodex-remote-relay-catalog-activation.timer",
            routing,
        )
        self.assertIn(
            "systemctl --user disable --now opencodex-remote-catalog-refresh.timer",
            routing,
        )
        self.assertIn("apply-relay-catalog", service)
        self.assertIn("OnUnitActiveSec=1min", timer)

    def test_remote_installer_deploys_all_managed_assets(self) -> None:
        content = (SCRIPTS_DIR / "install-remote-codex-home.sh").read_text(encoding="utf-8")
        self.assertIn("update-remote-codex.sh", content)
        self.assertIn("opencodex-remote-catalog-refresh.timer", content)
        self.assertIn("opencodex-remote-relay-catalog-activation.timer", content)
        self.assertIn("opencodex-remote-relay-catalog-activation.service", content)
        self.assertIn("systemctl --user start opencodex-remote-relay-catalog-activation.service", content)
        self.assertIn("opencodex-remote-codex-wrapper-repair.path", content)
        self.assertIn("install-remote-codex-relay.sh", content)
        self.assertIn("relay-installer", content)
        self.assertIn("RELAY_SYSTEMD_SOURCE", content)
        self.assertIn("opencodex-relay.service.in", content)
        self.assertIn("--with-relay-bootstrap", content)
        self.assertIn("auth.json", content)
        self.assertIn("will not create or", content)
        self.assertIn('"${MANAGER_TARGET}" verify-default-model', content)
        bootstrap = content.index('"${MANAGER_TARGET}" bootstrap-remote-control')
        verify = content.index('"${MANAGER_TARGET}" verify-daemon')
        self.assertLess(bootstrap, verify)

    def test_deployed_defect_line_has_an_explicit_redeployment_hold(self) -> None:
        marker = REPO_ROOT / "REDEPLOY_REQUIRED.md"
        if not marker.is_file():
            self.skipTest("private deployment redeploy evidence is not exported")
        content = marker.read_text(encoding="utf-8")
        self.assertIn("review pending", content)
        self.assertIn("already deployed", content)
        self.assertIn("0.1.0", content)
        self.assertIn("No command", content)
        self.assertNotIn(
            "REDEPLOY_REQUIRED.md",
            (REPO_ROOT / "README.md").read_text(encoding="utf-8"),
        )
        self.assertIn(
            "REDEPLOY_REQUIRED.md",
            (REPO_ROOT / "README.ko.md").read_text(encoding="utf-8"),
        )

    def test_remote_relay_installer_requires_a_signed_github_release_and_an_explicit_interrupt(self) -> None:
        content = (SCRIPTS_DIR / "install-remote-codex-relay.sh").read_text(encoding="utf-8")
        self.assertIn("--github-repo OWNER/REPO", content)
        self.assertIn("--github-token-file PATH", content)
        self.assertIn("--allow-remote-interruption is required", content)
        self.assertIn("--manage-app-server false", content)
        self.assertIn("--defer-codex-routing", content)
        self.assertIn("routing_args=(enable-relay --allow-remote-interruption)", content)
        self.assertIn("routing_args+=(--migrate-legacy)", content)
        self.assertIn("--migrate-legacy", content)
        self.assertIn("require_external_remote_mode", content)
        self.assertIn("MODE=external is required", content)
        self.assertIn("require_loopback_remote_mode", content)
        self.assertIn("MODE=loopback is required", content)
        self.assertIn("install-local VERSION", content)
        self.assertIn("--upstream-mode local_opencodex", content)
        self.assertIn("--credentials none", content)
        self.assertIn("--catalog-owner remote_manager", content)
        self.assertIn("routing_args=(enable-local-relay --allow-remote-interruption)", content)
        self.assertIn('"$ROUTING" "${routing_args[@]}"', content)
        self.assertIn("install-local requires at least one --bounded-json-model", content)
        self.assertNotIn("require_local_policy_matches_root_model", content)
        self.assertNotIn("install-local requires a provider-qualified Remote root model", content)
        self.assertIn("verify_installed_relay_config", content)
        self.assertIn("installed local relay config does not satisfy", content)
        self.assertIn("installed relay config is missing bounded_json policy", content)
        self.assertLess(
            content.index('"$RELAY_INSTALLER" "${installer_args[@]}"'),
            content.index("verify_installed_relay_config\n"),
        )
        self.assertLess(
            content.index("verify_installed_relay_config\n"),
            content.index('"$ROUTING" "${routing_args[@]}"'),
        )
        self.assertLess(
            content.index('if [[ "$action" == "install" ]]; then\n  require_external_remote_mode'),
            content.index('"$RELAY_INSTALLER" "${installer_args[@]}"'),
        )

    def test_linux_sandbox_setup_uses_the_narrow_apparmor_profile(self) -> None:
        content = (SCRIPTS_DIR / "configure-codex-linux-sandbox.sh").read_text(encoding="utf-8")
        self.assertIn("bubblewrap apparmor-profiles apparmor-utils", content)
        self.assertIn("bwrap-userns-restrict", content)
        self.assertIn("--unshare-user", content)
        self.assertNotRegex(
            content,
            r"sysctl\s+.*apparmor_restrict_unprivileged_userns=0",
        )

    def test_expected_version_contract_is_written_and_consumed(self) -> None:
        bootstrap = (SCRIPTS_DIR / "bootstrap-host.sh").read_text(encoding="utf-8")
        smoke = (SCRIPTS_DIR / "smoke-test.sh").read_text(encoding="utf-8")
        self.assertIn('OPENCODEX_VERSION="${OPENCODEX_VERSION:-}"', bootstrap)
        self.assertIn('EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"', bootstrap)
        self.assertIn('EXPECTED_VERSION_FILE="/etc/opencodex/expected-version"', smoke)
        self.assertIn("EXPECTED_OPENCODEX_VERSION", smoke)
        self.assertIn('expected_opencodex_version="2.10.1"', smoke)
        self.assertIn('package_manifest="$(jq -er', smoke)
        self.assertIn("legacy_expected_version_fallback", smoke)

    def test_bilingual_update_runbooks_link_to_the_managed_commands(self) -> None:
        for name in ("updates.md", "updates.ko.md"):
            content = (REPO_ROOT / "docs" / name).read_text(encoding="utf-8")
            self.assertIn("upgrade-opencodex.sh", content)
            self.assertIn("update-remote-codex.sh", content)
            self.assertIn("install-remote-codex-home.sh", content)
            self.assertIn("configure-codex-linux-sandbox.sh", content)
            self.assertIn("verify-daemon", content)
            self.assertIn("recover-daemon --allow-remote-interruption", content)
            self.assertIn("isolate-home-project-config --allow-remote-interruption", content)

    def test_first_remote_install_runbooks_bootstrap_on_the_first_invocation(self) -> None:
        for path in (
            PILOT / "README.md",
            REPO_ROOT / "docs" / "updates.md",
            REPO_ROOT / "docs" / "updates.ko.md",
        ):
            content = path.read_text(encoding="utf-8")
            bootstrap = "install-remote-codex-home.sh install --bootstrap-remote-control"
            self.assertIn(bootstrap, content)
            self.assertEqual(content.index("install-remote-codex-home.sh install"), content.index(bootstrap))

    def test_updaters_reject_unsafe_or_incomplete_invocations_before_host_access(self) -> None:
        opencodex = self.run_script("upgrade-opencodex.sh", "check", "latest")
        self.assertNotEqual(opencodex.returncode, 0)
        self.assertIn("explicit semver", opencodex.stderr)

        adoption = self.run_script("upgrade-opencodex.sh", "adopt-current", "latest")
        self.assertNotEqual(adoption.returncode, 0)
        self.assertIn("explicit semver", adoption.stderr)

        remote = self.run_script("update-remote-codex.sh", "apply")
        self.assertEqual(remote.returncode, 2)
        self.assertIn("allow-remote-interruption", remote.stdout)

        installer = self.run_script("install-remote-codex-home.sh", "install", "--unexpected")
        self.assertEqual(installer.returncode, 2)
        self.assertIn("bootstrap-remote-control", installer.stdout)


if __name__ == "__main__":
    unittest.main()

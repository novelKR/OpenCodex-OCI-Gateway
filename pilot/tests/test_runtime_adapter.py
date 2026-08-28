#!/usr/bin/env python3

import json
import os
import pwd
import shutil
import signal
import subprocess
import tempfile
import time
import unittest
from pathlib import Path


PILOT = Path(__file__).resolve().parents[1]
INVOKER_SOURCE = PILOT / "libexec" / "opencodex-runtime"
CONFIGURE = PILOT / "scripts" / "configure-opencodex-runtime.sh"
UNIT_SOURCE = PILOT / "systemd" / "opencodex.service"


def write_executable(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")
    path.chmod(0o755)


class RuntimeFixture:
    def __init__(self, root: Path) -> None:
        self.root = root.resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        self.root.chmod(0o700)
        self.home = self.root / "home"
        self.prefix = self.root / "prefix"
        self.runtime = self.root / "runtime"
        self.config = self.root / "runtime.json"
        self.invoker = self.root / "opencodex-runtime"
        self.busy_marker = self.root / "busy"
        self.invalid_config_marker = self.root / "invalid-config"
        self.home.mkdir()
        self.home.chmod(0o700)
        self.prefix.mkdir()
        self.runtime.mkdir()

        self.node = self.runtime / "node"
        self.npm = self.runtime / "npm-cli.js"
        self.package_dir = (
            self.prefix / "lib" / "node_modules" / "@bitkyc08" / "opencodex"
        )
        self.ocx = self.package_dir / "bin" / "ocx.mjs"
        self.manifest = self.package_dir / "package.json"
        self.bun_dir = self.package_dir / "node_modules" / "bun"
        self.bun_manifest = self.bun_dir / "package.json"
        self.bun_install = self.bun_dir / "install.js"
        self.bun_binary = self.bun_dir / "bin" / "bun.exe"

        write_executable(
            self.node,
            "#!/bin/bash\n"
            "entry=$1\n"
            "shift\n"
            "exec /bin/bash \"$entry\" \"$@\"\n",
        )
        self.npm.write_text(
            "#!/bin/bash\n"
            "printf 'npm-home=%s\\n' \"$HOME\"\n"
            "printf 'npm-cwd=%s\\n' \"$PWD\"\n"
            "printf 'npm-args='\n"
            "printf '<%s>' \"$@\"\n"
            "printf '\\n'\n",
            encoding="utf-8",
        )
        self.npm.chmod(0o644)
        self.install_package()
        self.write_config()
        self.write_patched_invoker(self.invoker, self.config)

    def install_package(self) -> None:
        (self.package_dir / "bin").mkdir(parents=True, exist_ok=True)
        self.ocx.write_text(
            "#!/bin/bash\n"
            f"busy_marker={str(self.busy_marker)!r}\n"
            "if [[ ${1:-} == --version ]]; then\n"
            "  [[ -z ${OPENCODEX_BUN_PATH+x} && -z ${NODE_PATH+x} ]] || exit 90\n"
            "  printf 'opencodex 2.17.0\\n'\n"
            "elif [[ ${1:-} == config && ${2:-} == validate ]]; then\n"
            f"  [[ ! -e {str(self.invalid_config_marker)!r} ]] || exit 1\n"
            "  printf 'Config is valid.\\n'\n"
            "elif [[ ${1:-} == observe && ${2:-} == memory && ${3:-} == --json ]]; then\n"
            "  if [[ -e $busy_marker ]]; then count=1; else count=0; fi\n"
            "  printf '{\"activeTurnCount\":%s,\"isDraining\":false}\\n' \"$count\"\n"
            "elif [[ ${1:-} == signal ]]; then\n"
            "  printf '%s\\n' \"$$\"\n"
            "  trap 'exit 42' TERM\n"
            "  while :; do sleep 1; done\n"
            "else\n"
            "  printf 'ocx-home=%s\\n' \"$HOME\"\n"
            "  printf 'ocx-cwd=%s\\n' \"$PWD\"\n"
            "  printf 'ocx-path=%s\\n' \"$PATH\"\n"
            "  printf 'ocx-args='\n"
            "  printf '<%s>' \"$@\"\n"
            "  printf '\\n'\n"
            "  printf 'ocx-stderr\\n' >&2\n"
            "  [[ ${1:-} != exit-23 ]] || exit 23\n"
            "fi\n",
            encoding="utf-8",
        )
        self.ocx.chmod(0o644)
        self.manifest.write_text(
            json.dumps(
                {
                    "name": "@bitkyc08/opencodex",
                    "version": "2.17.0",
                    "dependencies": {"bun": "1.4.0"},
                }
            ),
            encoding="utf-8",
        )
        self.manifest.chmod(0o644)
        self.bun_dir.mkdir(parents=True)
        self.bun_manifest.write_text(
            json.dumps({"name": "bun", "version": "1.4.0"}),
            encoding="utf-8",
        )
        self.bun_manifest.chmod(0o644)
        self.bun_install.write_text("// fixture installer\n", encoding="utf-8")
        self.bun_install.chmod(0o644)
        self.bun_binary.parent.mkdir()
        self.bun_binary.touch()
        os.truncate(self.bun_binary, 1_000_001)
        self.bun_binary.chmod(0o755)

    def write_config(self, **updates: object) -> None:
        data: dict[str, object] = {
            "schema_version": 1,
            "runtime_kind": "node",
            "home": str(self.home),
            "prefix": str(self.prefix),
            "node_bin": str(self.node),
            "npm_cli": str(self.npm),
            "ocx_entry": str(self.ocx),
        }
        data.update(updates)
        self.config.write_text(json.dumps(data), encoding="utf-8")
        self.config.chmod(0o644)

    @staticmethod
    def write_patched_invoker(target: Path, config: Path) -> None:
        target.parent.mkdir(parents=True, exist_ok=True)
        content = INVOKER_SOURCE.read_text(encoding="utf-8")
        content = content.replace(
            'readonly RUNTIME_CONFIG="/etc/opencodex/runtime.json"',
            f"readonly RUNTIME_CONFIG={str(config)!r}",
        )
        content = content.replace(
            'readonly REQUIRED_UID="0"',
            f'readonly REQUIRED_UID="{os.getuid()}"',
        )
        content = content.replace(
            'readonly SERVICE_USER="opencodex"',
            f'readonly SERVICE_USER="{pwd.getpwuid(os.getuid()).pw_name}"',
        )
        content = content.replace(
            'readonly TRUSTED_RUNTIME_UIDS="0"',
            f'readonly TRUSTED_RUNTIME_UIDS="0 {os.getuid()}"',
        )
        target.write_text(content, encoding="utf-8")
        target.chmod(0o755)

    def run(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(self.invoker), *args],
            check=False,
            capture_output=True,
            text=True,
            env={
                "PATH": "/definitely/not/inherited",
                "NODE_OPTIONS": "--invalid-option",
                "NODE_PATH": "/untrusted/node-path",
                "OPENCODEX_BUN_PATH": "/untrusted/bun",
            },
        )

    def model_service_owned_installation(self, *, root_owned_prefix: bool = False) -> None:
        content = self.invoker.read_text(encoding="utf-8")
        content = content.replace(
            f'readonly REQUIRED_UID="{os.getuid()}"',
            'readonly REQUIRED_UID="0"',
        )
        original = (
            "numeric_uid() {\n"
            "  stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"\n"
            "}\n"
        )
        root_prefix_case = ""
        if root_owned_prefix:
            root_prefix_case = f"|{str(self.prefix)!r}|{str(self.prefix)!r}/*"
        replacement = (
            "numeric_uid() {\n"
            "  case \"$1\" in\n"
            f"    {str(self.config)!r}|{str(self.invoker)!r}{root_prefix_case}) "
            "printf '%s\\n' '0' ;;\n"
            "    *) stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\" ;;\n"
            "  esac\n"
            "}\n"
        )
        self.assert_source_contains(content, original)
        self.invoker.write_text(content.replace(original, replacement), encoding="utf-8")
        self.invoker.chmod(0o755)

    def stage_canary_pair(self, name: str = "fixture") -> tuple[Path, Path]:
        directory = self.root / f"opencodex-runtime-canary.{name}"
        directory.mkdir(mode=0o711)
        adapter = directory / "opencodex-runtime"
        contract = directory / "runtime.json"
        shutil.copy2(self.invoker, adapter)
        shutil.copy2(self.config, contract)
        adapter.chmod(0o755)
        contract.chmod(0o644)
        return adapter, contract

    @staticmethod
    def run_canary(
        adapter: Path,
        contract: str | Path,
        *args: str,
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(adapter), *args],
            check=False,
            capture_output=True,
            text=True,
            env={
                "PATH": "/definitely/not/inherited",
                "OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG": str(contract),
            },
        )

    def report_special_mode_for(self, path: Path, mode: int) -> None:
        content = self.invoker.read_text(encoding="utf-8")
        original = (
            "numeric_mode() {\n"
            "  stat -c '%a' -- \"$1\" 2>/dev/null || stat -f '%Lp' -- \"$1\"\n"
            "}\n"
        )
        replacement = (
            "numeric_mode() {\n"
            f"  if [[ $1 == {str(path)!r} ]]; then printf '%s\\n' '{mode:o}'; "
            "else stat -c '%a' -- \"$1\" 2>/dev/null || stat -f '%Lp' -- \"$1\"; fi\n"
            "}\n"
        )
        self.assert_source_contains(content, original)
        self.invoker.write_text(content.replace(original, replacement), encoding="utf-8")
        self.invoker.chmod(0o755)

    @staticmethod
    def assert_source_contains(content: str, expected: str) -> None:
        if expected not in content:
            raise AssertionError("runtime fixture source patch target is missing")


class RuntimeAdapterTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.fixture = RuntimeFixture(Path(self.temporary.name))

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_check_and_describe_validate_the_exact_contract(self) -> None:
        checked = self.fixture.run("check")
        self.assertEqual(checked.returncode, 0, checked.stderr)
        self.assertEqual(checked.stdout, "runtime_adapter=valid\n")

        described = self.fixture.run("describe", "--json")
        self.assertEqual(described.returncode, 0, described.stderr)
        body = json.loads(described.stdout)
        self.assertEqual(
            set(body),
            {
                "schema_version",
                "runtime_kind",
                "home",
                "prefix",
                "node_bin",
                "npm_cli",
                "ocx_entry",
                "package_manifest",
                "package_version",
            },
        )
        self.assertEqual(body["package_version"], "2.17.0")
        self.assertEqual(body["package_manifest"], str(self.fixture.manifest))

    def test_internal_canary_contract_accepts_only_a_safe_colocated_pair(self) -> None:
        adapter, contract = self.fixture.stage_canary_pair()
        checked = self.fixture.run_canary(adapter, contract, "check")
        self.assertEqual(checked.returncode, 0, checked.stderr)

        outside = self.fixture.run_canary(
            adapter,
            self.fixture.config,
            "check",
        )
        self.assertNotEqual(outside.returncode, 0)
        self.assertIn("colocated managed pair", outside.stderr)

        contract.unlink()
        contract.symlink_to(self.fixture.config)
        linked = self.fixture.run_canary(adapter, contract, "check")
        self.assertNotEqual(linked.returncode, 0)
        self.assertIn("must not be a symbolic link", linked.stderr)

        contract.unlink()
        shutil.copy2(self.fixture.config, contract)
        contract.chmod(0o600)
        wrong_mode = self.fixture.run_canary(adapter, contract, "check")
        self.assertNotEqual(wrong_mode.returncode, 0)
        self.assertIn("mode must be 644", wrong_mode.stderr)

        contract.chmod(0o644)
        adapter.parent.chmod(0o755)
        listable = self.fixture.run_canary(adapter, contract, "check")
        self.assertNotEqual(listable.returncode, 0)
        self.assertIn("managed non-listable runtime directory", listable.stderr)

    def test_internal_canary_contract_rejects_unsafe_owner_and_control_path(self) -> None:
        adapter, contract = self.fixture.stage_canary_pair()
        unsafe_uid = 1 if os.getuid() != 1 else 2
        content = adapter.read_text(encoding="utf-8")
        original = (
            "numeric_uid() {\n"
            "  stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"\n"
            "}\n"
        )
        replacement = (
            "numeric_uid() {\n"
            f"  if [[ $1 == {str(contract)!r} ]]; then printf '%s\\n' '{unsafe_uid}'; "
            "else stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"; fi\n"
            "}\n"
        )
        self.fixture.assert_source_contains(content, original)
        adapter.write_text(content.replace(original, replacement), encoding="utf-8")
        adapter.chmod(0o755)
        wrong_owner = self.fixture.run_canary(adapter, contract, "check")
        self.assertNotEqual(wrong_owner.returncode, 0)
        self.assertIn("must be owned by uid", wrong_owner.stderr)

        RuntimeFixture.write_patched_invoker(adapter, self.fixture.config)
        control_path = f"{contract}\x7f"
        control = self.fixture.run_canary(adapter, control_path, "check")
        self.assertNotEqual(control.returncode, 0)
        self.assertIn("control character", control.stderr)

    def test_production_adapter_path_rejects_internal_canary_selection(self) -> None:
        adapter, contract = self.fixture.stage_canary_pair("production")
        content = adapter.read_text(encoding="utf-8")
        original = (
            'readonly PRODUCTION_INVOKER="/usr/local/libexec/opencodex-runtime"'
        )
        self.fixture.assert_source_contains(content, original)
        adapter.write_text(
            content.replace(
                original,
                f"readonly PRODUCTION_INVOKER={str(adapter)!r}",
            ),
            encoding="utf-8",
        )
        adapter.chmod(0o755)
        result = self.fixture.run_canary(adapter, contract, "check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("production runtime adapter rejects", result.stderr)

    def test_ocx_preserves_arguments_exit_streams_and_uses_contract_environment(self) -> None:
        result = self.fixture.run("ocx", "alpha beta", "*", "$HOME")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"ocx-home={self.fixture.home}", result.stdout)
        self.assertIn(f"ocx-cwd={self.fixture.home}", result.stdout)
        self.assertIn("ocx-args=<alpha beta><*><$HOME>", result.stdout)
        self.assertIn(f"ocx-path={self.fixture.node.parent}:", result.stdout)
        self.assertNotIn("/definitely/not/inherited", result.stdout)
        self.assertEqual(result.stderr, "ocx-stderr\n")

        failed = self.fixture.run("ocx", "exit-23")
        self.assertEqual(failed.returncode, 23)

    def test_ocx_exec_chain_preserves_pid_and_signal_exit(self) -> None:
        process = subprocess.Popen(
            [str(self.fixture.invoker), "ocx", "signal"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        assert process.stdout is not None
        reported_pid = int(process.stdout.readline().strip())
        self.assertEqual(reported_pid, process.pid)
        process.send_signal(signal.SIGTERM)
        self.assertEqual(process.wait(timeout=3), 42)
        process.communicate(timeout=1)

    def test_prepare_bundled_bun_accepts_only_the_service_owned_install_window(self) -> None:
        self.fixture.model_service_owned_installation()

        prepared = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        self.assertEqual(prepared.stdout, "opencodex 2.17.0\n")

        normal = self.fixture.run("ocx", "--version")
        self.assertNotEqual(normal.returncode, 0)
        self.assertIn("runtime prefix must be owned by uid 0", normal.stderr)

    def test_prepare_bundled_bun_rejects_root_owned_or_replaceable_prefix(self) -> None:
        self.fixture.model_service_owned_installation(root_owned_prefix=True)
        root_owned = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(root_owned.returncode, 0)
        self.assertIn("requires a service-owned runtime prefix", root_owned.stderr)

        self.fixture = RuntimeFixture(Path(self.temporary.name) / "replaceable")
        self.fixture.prefix.chmod(0o775)
        replaceable = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(replaceable.returncode, 0)
        self.assertIn("group- or world-writable", replaceable.stderr)

    def test_prepare_bundled_bun_rejects_invalid_interface_and_versions(self) -> None:
        for args in (
            ("prepare-bundled-bun",),
            ("prepare-bundled-bun", "2.17.0", "extra"),
        ):
            with self.subTest(args=args):
                result = self.fixture.run(*args)
                self.assertEqual(result.returncode, 2)
                self.assertIn("Usage:", result.stdout)

        self.fixture.model_service_owned_installation()
        invalid = self.fixture.run("prepare-bundled-bun", "latest")
        self.assertNotEqual(invalid.returncode, 0)
        self.assertIn("expected OpenCodex version is invalid", invalid.stderr)
        mismatch = self.fixture.run("prepare-bundled-bun", "2.18.0")
        self.assertNotEqual(mismatch.returncode, 0)
        self.assertIn("installed OpenCodex version is 2.17.0", mismatch.stderr)

    def test_prepare_bundled_bun_rejects_unpinned_or_mismatched_bun(self) -> None:
        self.fixture.manifest.write_text(
            json.dumps(
                {
                    "name": "@bitkyc08/opencodex",
                    "version": "2.17.0",
                    "dependencies": {"bun": "^1.4.0"},
                }
            ),
            encoding="utf-8",
        )
        self.fixture.model_service_owned_installation()
        unpinned = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(unpinned.returncode, 0)
        self.assertIn("must pin an exact bundled Bun dependency", unpinned.stderr)

        self.fixture = RuntimeFixture(Path(self.temporary.name) / "mismatch")
        self.fixture.bun_manifest.write_text(
            json.dumps({"name": "bun", "version": "1.3.0"}),
            encoding="utf-8",
        )
        self.fixture.model_service_owned_installation()
        mismatch = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(mismatch.returncode, 0)
        self.assertIn("does not match the pinned dependency", mismatch.stderr)

    def test_prepare_bundled_bun_rejects_unsafe_installer_path(self) -> None:
        real_installer = self.fixture.bun_dir / "real-install.js"
        self.fixture.bun_install.rename(real_installer)
        self.fixture.bun_install.symlink_to(real_installer)
        self.fixture.model_service_owned_installation()
        linked = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(linked.returncode, 0)
        self.assertIn("must not be a symbolic link", linked.stderr)

    def test_prepare_bundled_bun_rejects_unsafe_existing_binary(self) -> None:
        outside = self.fixture.root / "outside-bun"
        outside.touch()
        os.truncate(outside, 1_000_001)
        outside.chmod(0o755)
        self.fixture.bun_binary.unlink()
        self.fixture.bun_binary.symlink_to(outside)
        self.fixture.model_service_owned_installation()

        linked = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(linked.returncode, 0)
        self.assertIn("must not be a symbolic link", linked.stderr)

        self.fixture.bun_binary.unlink()
        self.fixture.bun_binary.touch()
        os.truncate(self.fixture.bun_binary, 1_000_001)
        self.fixture.bun_binary.chmod(0o775)
        unsafe_mode = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(unsafe_mode.returncode, 0)
        self.assertIn("group- or world-writable", unsafe_mode.stderr)

    def test_prepare_bundled_bun_revalidates_binary_created_by_launcher(self) -> None:
        outside = self.fixture.root / "outside-bun"
        outside.touch()
        os.truncate(outside, 1_000_001)
        outside.chmod(0o755)
        content = self.fixture.ocx.read_text(encoding="utf-8")
        marker = "  [[ -z ${OPENCODEX_BUN_PATH+x} && -z ${NODE_PATH+x} ]] || exit 90\n"
        self.assertIn(marker, content)
        replacement = (
            marker
            + f"  rm -f -- {str(self.fixture.bun_binary)!r}\n"
            + f"  ln -s -- {str(outside)!r} {str(self.fixture.bun_binary)!r}\n"
        )
        self.fixture.ocx.write_text(content.replace(marker, replacement), encoding="utf-8")
        self.fixture.ocx.chmod(0o644)
        self.fixture.model_service_owned_installation()

        replaced = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(replaced.returncode, 0)
        self.assertIn("must not be a symbolic link", replaced.stderr)

    def test_prepare_bundled_bun_requires_a_real_binary_after_launcher(self) -> None:
        os.truncate(self.fixture.bun_binary, 450)
        self.fixture.model_service_owned_installation()

        incomplete = self.fixture.run("prepare-bundled-bun", "2.17.0")
        self.assertNotEqual(incomplete.returncode, 0)
        self.assertIn("missing or incomplete", incomplete.stderr)

    def test_normal_adapter_rejects_mutable_bundled_bun_symlink(self) -> None:
        outside = self.fixture.root / "outside-bun"
        outside.touch()
        os.truncate(outside, 1_000_001)
        outside.chmod(0o755)
        self.fixture.bun_binary.unlink()
        self.fixture.bun_binary.symlink_to(outside)

        checked = self.fixture.run("check")
        self.assertNotEqual(checked.returncode, 0)
        self.assertIn("must not be a symbolic link", checked.stderr)

    def test_npm_works_before_opencodex_package_exists(self) -> None:
        shutil.rmtree(self.fixture.package_dir)
        result = self.fixture.run("npm", "install", "--global", "pkg@1.2.3")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"npm-home={self.fixture.home}", result.stdout)
        self.assertIn("npm-args=<install><--global><pkg@1.2.3>", result.stdout)
        checked = self.fixture.run("check")
        self.assertNotEqual(checked.returncode, 0)
        self.assertIn("OpenCodex CLI entry", checked.stderr)

    def test_npm_view_accepts_a_root_owned_prefix_for_the_service_user(self) -> None:
        content = self.fixture.invoker.read_text(encoding="utf-8")
        content = content.replace(
            f'readonly REQUIRED_UID="{os.getuid()}"',
            'readonly REQUIRED_UID="0"',
        )
        original = (
            "numeric_uid() {\n"
            "  stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"\n"
            "}\n"
        )
        replacement = (
            "numeric_uid() {\n"
            "  case \"$1\" in\n"
            f"    {str(self.fixture.config)!r}|{str(self.fixture.invoker)!r}|"
            f"{str(self.fixture.node)!r}|{str(self.fixture.npm)!r}|"
            f"{str(self.fixture.prefix)!r}|{str(self.fixture.prefix)!r}/*) "
            "printf '%s\\n' '0' ;;\n"
            f"    *) printf '%s\\n' '{os.getuid()}' ;;\n"
            "  esac\n"
            "}\n"
        )
        self.fixture.assert_source_contains(content, original)
        self.fixture.invoker.write_text(
            content.replace(original, replacement),
            encoding="utf-8",
        )
        self.fixture.invoker.chmod(0o755)
        result = self.fixture.run("npm", "view", "pkg@1.2.3", "version")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("npm-args=<view><pkg@1.2.3><version>", result.stdout)

    def test_schema_unknown_missing_type_and_control_values_fail_closed(self) -> None:
        cases = (
            {"unknown": True},
            {"runtime_kind": "container"},
            {"schema_version": 2},
            {"home": "relative"},
            {"home": "/tmp/\u0007bad"},
            {"node_bin": 3},
        )
        for updates in cases:
            with self.subTest(updates=updates):
                self.fixture.write_config(**updates)
                result = self.fixture.run("check")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("runtime contract schema is invalid", result.stderr)
        data = json.loads(self.fixture.config.read_text(encoding="utf-8"))
        del data["npm_cli"]
        self.fixture.config.write_text(json.dumps(data), encoding="utf-8")
        self.fixture.config.chmod(0o644)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime contract schema is invalid", result.stderr)

    def test_contract_and_execution_path_metadata_fail_closed(self) -> None:
        self.fixture.config.chmod(0o600)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("mode must be 644", result.stderr)

        self.fixture.config.chmod(0o644)
        self.fixture.node.chmod(0o775)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("group- or world-writable", result.stderr)

        self.fixture.node.chmod(0o755)
        self.fixture.report_special_mode_for(self.fixture.node, 0o4755)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("setuid, setgid, or sticky", result.stderr)

        RuntimeFixture.write_patched_invoker(
            self.fixture.invoker,
            self.fixture.config,
        )
        self.fixture.report_special_mode_for(self.fixture.npm, 0o2755)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("setuid, setgid, or sticky", result.stderr)

        RuntimeFixture.write_patched_invoker(
            self.fixture.invoker,
            self.fixture.config,
        )
        for unsafe_mode in (0o500, 0o600, 0o755):
            self.fixture.home.chmod(unsafe_mode)
            result = self.fixture.run("check")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("runtime home mode must be 700", result.stderr)

        self.fixture.home.chmod(0o700)
        real_node = self.fixture.runtime / "real-node"
        self.fixture.node.rename(real_node)
        self.fixture.node.symlink_to(real_node)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symbolic link", result.stderr)

    def test_replaceable_runtime_parent_is_rejected_before_execution(self) -> None:
        self.fixture.runtime.chmod(0o775)
        result = self.fixture.run("ocx", "must-not-run")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime ancestor is group- or world-writable", result.stderr)
        self.assertNotIn("ocx-args", result.stdout)

    def test_replaceable_home_parent_is_rejected_before_execution(self) -> None:
        unsafe_parent = self.fixture.root / "unsafe-home-parent"
        unsafe_parent.mkdir()
        unsafe_parent.chmod(0o775)
        alternate_home = unsafe_parent / "home"
        alternate_home.mkdir(mode=0o700)
        self.fixture.write_config(home=str(alternate_home))
        result = self.fixture.run("ocx", "must-not-run")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime ancestor is group- or world-writable", result.stderr)
        self.assertNotIn("ocx-args", result.stdout)

    def test_nontraversable_home_parent_is_rejected_before_execution(self) -> None:
        unsafe_parent = self.fixture.root / "nontraversable-home-parent"
        unsafe_parent.mkdir(mode=0o700)
        alternate_home = unsafe_parent / "home"
        alternate_home.mkdir(mode=0o700)
        self.fixture.write_config(home=str(alternate_home))
        content = self.fixture.invoker.read_text(encoding="utf-8")
        original = (
            "numeric_uid() {\n"
            "  stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"\n"
            "}\n"
        )
        replacement = (
            "numeric_uid() {\n"
            f"  if [[ $1 == {str(unsafe_parent)!r} ]]; then printf '%s\\n' '0'; "
            "else stat -c '%u' -- \"$1\" 2>/dev/null || stat -f '%u' -- \"$1\"; fi\n"
            "}\n"
        )
        self.fixture.assert_source_contains(content, original)
        self.fixture.invoker.write_text(
            content.replace(original, replacement),
            encoding="utf-8",
        )
        self.fixture.invoker.chmod(0o755)
        result = self.fixture.run("ocx", "must-not-run")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime home ancestor is not service-traversable", result.stderr)
        self.assertNotIn("ocx-args", result.stdout)

    def test_untrusted_runtime_owner_is_rejected_before_execution(self) -> None:
        content = self.fixture.invoker.read_text(encoding="utf-8")
        content = content.replace(
            f'readonly TRUSTED_RUNTIME_UIDS="0 {os.getuid()}"',
            'readonly TRUSTED_RUNTIME_UIDS="0"',
        )
        self.fixture.invoker.write_text(content, encoding="utf-8")
        self.fixture.invoker.chmod(0o755)
        result = self.fixture.run("ocx", "must-not-run")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe owner", result.stderr)
        self.assertNotIn("ocx-args", result.stdout)

    def test_npm_rejects_a_replaceable_service_owned_prefix(self) -> None:
        shutil.rmtree(self.fixture.package_dir)
        self.fixture.prefix.chmod(0o775)
        result = self.fixture.run("npm", "install", "pkg")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime prefix must not be group- or world-writable", result.stderr)
        self.assertNotIn("npm-args", result.stdout)

    def test_npm_cli_must_be_service_readable(self) -> None:
        self.fixture.npm.chmod(0o600)
        result = self.fixture.run("npm", "--version")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be service-readable", result.stderr)
        self.assertNotIn("npm-args", result.stdout)

    def test_noncanonical_path_and_wrong_package_metadata_are_rejected(self) -> None:
        noncanonical = str(self.fixture.home / ".." / "home")
        self.fixture.write_config(home=noncanonical)
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be canonical", result.stderr)

        self.fixture.write_config()
        self.fixture.manifest.write_text(
            json.dumps({"name": "wrong", "version": "2.17.0"}),
            encoding="utf-8",
        )
        result = self.fixture.run("describe", "--json")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("package metadata is invalid", result.stderr)

    def test_protected_or_overlapping_home_and_prefix_are_rejected(self) -> None:
        self.fixture.write_config(home="/")
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("runtime contract schema is invalid", result.stderr)

        self.fixture.write_config(
            home=str(self.fixture.root),
            prefix=str(self.fixture.prefix),
        )
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must not overlap", result.stderr)


class RuntimeConfigureFixture:
    def __init__(self, root: Path) -> None:
        self.test_root = (root / "root").resolve()
        self.test_root.mkdir(mode=0o700)
        self.canary_parent = self.test_root / "usr" / "local"
        self.canary_parent.mkdir(parents=True, mode=0o755)
        self.runtime = RuntimeFixture((root / "candidate").resolve())
        self.state = (root / "state").resolve()
        self.state.mkdir(mode=0o700)
        self.systemctl = (root / "tools" / "systemctl").resolve()
        self.curl = (root / "tools" / "curl").resolve()
        self.patched_invoker = (root / "tools" / "opencodex-runtime").resolve()
        RuntimeFixture.write_patched_invoker(
            self.patched_invoker,
            self.test_root / "etc" / "opencodex" / "runtime.json",
        )
        self._write_tools()
        self.unit = self.test_root / "etc" / "systemd" / "system" / "opencodex.service"
        self.contract = self.test_root / "etc" / "opencodex" / "runtime.json"
        self.invoker = (
            self.test_root / "usr" / "local" / "libexec" / "opencodex-runtime"
        )
        self.unit.parent.mkdir(parents=True)
        self.contract.parent.mkdir(parents=True)
        self.invoker.parent.mkdir(parents=True)
        self.unit.write_text("old-unit\n", encoding="utf-8")
        self.contract.write_text("old-contract\n", encoding="utf-8")
        self.invoker.write_text("#!/bin/bash\nprintf old\\n\n", encoding="utf-8")
        self.invoker.chmod(0o755)
        (self.state / "active").write_text("1", encoding="utf-8")
        (self.state / "enabled").write_text("1", encoding="utf-8")
        (self.state / "dropins").write_text("", encoding="utf-8")

    def _write_tools(self) -> None:
        write_executable(
            self.systemctl,
            "#!/bin/bash\n"
            "set -euo pipefail\n"
            f"state={str(self.state)!r}\n"
            "printf '%s\\n' \"$*\" >> \"$state/calls\"\n"
            "cmd=${1:-}; shift || true\n"
            "case \"$cmd\" in\n"
            "  is-active)\n"
            "    [[ -e $state/active ]] || exit 3\n"
            "    [[ ${1:-} == --quiet ]] || printf 'active\\n'\n"
            "    ;;\n"
            "  is-enabled)\n"
            "    [[ -e $state/enabled ]] || exit 1\n"
            "    [[ ${1:-} == --quiet ]] || printf 'enabled\\n'\n"
            "    ;;\n"
            "  show)\n"
            "    case \"$*\" in\n"
            "      *DropInPaths*) cat \"$state/dropins\" ;;\n"
            "      *ActiveState*) if [[ -e $state/lifecycle-state ]]; then cat \"$state/lifecycle-state\"; elif [[ -e $state/active ]]; then printf 'active\\n'; else printf 'inactive\\n'; fi ;;\n"
            "      *CapabilityBoundingSet*) printf '\\n' ;;\n"
            "      *AmbientCapabilities*) printf '\\n' ;;\n"
            "      *NoNewPrivileges*) printf 'yes\\n' ;;\n"
            "      *ProtectSystem*) printf 'strict\\n' ;;\n"
            "      *PrivateDevices*) printf 'yes\\n' ;;\n"
            "      *PrivateTmp*) printf 'yes\\n' ;;\n"
            "      *RestrictNamespaces*) printf 'yes\\n' ;;\n"
            "      *User*) if [[ -e $state/bad-user ]]; then printf 'root\\n'; else printf 'opencodex\\n'; fi ;;\n"
            "      *Group*) printf 'opencodex\\n' ;;\n"
            "      *ProtectHome*) sed -n 's/^ProtectHome=//p' \"$FAKE_UNIT\" ;;\n"
            "      *BindReadOnlyPaths*) sed -n 's/^BindReadOnlyPaths=\"\\(.*\\)\"$/\\1/p' \"$FAKE_UNIT\"; [[ ! -e $state/extra-bind ]] || printf '/unexpected\\n' ;;\n"
            "      *ReadWritePaths*) sed -n 's/^ReadWritePaths=\"\\(.*\\)\"$/\\1/p' \"$FAKE_UNIT\" ;;\n"
            "      *ExecStartPre*) printf '/usr/local/libexec/opencodex-runtime check ; /usr/local/libexec/opencodex-runtime ocx config validate\\n' ;;\n"
            "      *ExecStart*) printf '/usr/local/libexec/opencodex-runtime ocx start --port 10100\\n' ;;\n"
            "      *) exit 1 ;;\n"
            "    esac\n"
            "    ;;\n"
            "  daemon-reload)\n"
            "    if [[ -e $state/poison-config-on-reload ]]; then touch \"$FAKE_INVALID_CONFIG_MARKER\"; fi\n"
            "    if [[ -e $state/poison-health-on-reload ]]; then touch \"$state/bad-health\"; fi\n"
            "    if [[ -e $state/activate-on-reload ]]; then touch \"$state/active\"; fi\n"
            "    if [[ -e $state/deactivate-on-reload ]]; then rm -f \"$state/active\"; fi\n"
            "    [[ ! -e $state/fail-daemon-reload ]] || exit 1\n"
            "    ;;\n"
            "  restart) touch \"$state/active\" ;;\n"
            "  start)\n"
            "    if [[ -e $state/fail-start ]]; then rm -f \"$state/fail-start\"; exit 1; fi\n"
            "    touch \"$state/active\"\n"
            "    ;;\n"
            "  stop) rm -f \"$state/active\" ;;\n"
            "  enable) touch \"$state/enabled\" ;;\n"
            "  disable) rm -f \"$state/enabled\" ;;\n"
            "  *) exit 1 ;;\n"
            "esac\n",
        )
        write_executable(
            self.curl,
            "#!/bin/bash\n"
            f"state={str(self.state)!r}\n"
            "if [[ -e $state/bad-health ]]; then\n"
            "  printf '{\"status\":\"bad\",\"service\":\"foreign\",\"version\":\"0.0.0\"}\\n'\n"
            "else\n"
            "  printf '{\"status\":\"ok\",\"service\":\"opencodex\",\"version\":\"2.17.0\"}\\n'\n"
            "fi\n",
        )

    def env(self) -> dict[str, str]:
        return {
            **os.environ,
            "OPENCODEX_RUNTIME_TEST_ROOT": str(self.test_root),
            "OPENCODEX_RUNTIME_TEST_SYSTEMCTL": str(self.systemctl),
            "OPENCODEX_RUNTIME_TEST_CURL": str(self.curl),
            "OPENCODEX_RUNTIME_TEST_INVOKER_SOURCE": str(self.patched_invoker),
            "FAKE_UNIT": str(self.unit),
            "FAKE_INVALID_CONFIG_MARKER": str(self.runtime.invalid_config_marker),
        }

    def run(self, action: str, *extra: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                "bash",
                str(CONFIGURE),
                action,
                "--node-bin",
                str(self.runtime.node),
                "--npm-cli",
                str(self.runtime.npm),
                "--home",
                str(self.runtime.home),
                "--prefix",
                str(self.runtime.prefix),
                *extra,
            ],
            check=False,
            capture_output=True,
            text=True,
            env=self.env(),
        )

    def command(self, action: str, *extra: str) -> list[str]:
        return [
            "bash",
            str(CONFIGURE),
            action,
            "--node-bin",
            str(self.runtime.node),
            "--npm-cli",
            str(self.runtime.npm),
            "--home",
            str(self.runtime.home),
            "--prefix",
            str(self.runtime.prefix),
            *extra,
        ]

    def canary_directories(self) -> list[Path]:
        return list(
            self.canary_parent.glob("opencodex-runtime-canary.*")
        )

    def instrument_canary_audit(self) -> Path:
        audit = self.state / "canary-audit"
        content = self.patched_invoker.read_text(encoding="utf-8")
        marker = 'action="${1:-}"'
        instrumentation = (
            'if [[ "${OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG+x}" == "x" ]]; then\n'
            f"  printf '%s\\t%s\\t%s\\t%s\\t%s\\t%s\\n' \"$*\" "
            '"${BASH_SOURCE[0]}" '
            '"${OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG}" '
            '"$(numeric_mode "${BASH_SOURCE[0]}")" '
            '"$(numeric_mode "${OPENCODEX_RUNTIME_INTERNAL_CANARY_CONFIG}")" '
            '"$(numeric_mode "$(dirname -- "${BASH_SOURCE[0]}")")" '
            f">> {str(audit)!r}\n"
            "fi\n\n"
            f"{marker}"
        )
        if marker not in content:
            raise AssertionError("runtime adapter action marker is missing")
        self.patched_invoker.write_text(
            content.replace(marker, instrumentation, 1),
            encoding="utf-8",
        )
        self.patched_invoker.chmod(0o755)
        return audit


class RuntimeConfigureTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.fixture = RuntimeConfigureFixture(Path(self.temporary.name))

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_check_is_read_only_and_apply_installs_atomic_managed_assets(self) -> None:
        old_contract = self.fixture.contract.read_bytes()
        old_invoker = self.fixture.invoker.read_bytes()
        checked = self.fixture.run("check")
        self.assertEqual(checked.returncode, 0, checked.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertEqual(self.fixture.contract.read_bytes(), old_contract)
        self.assertEqual(self.fixture.invoker.read_bytes(), old_invoker)
        self.assertEqual(self.fixture.canary_directories(), [])
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

        applied = self.fixture.run("apply", "--allow-service-restart")
        self.assertEqual(applied.returncode, 0, applied.stderr)
        contract = json.loads(self.fixture.contract.read_text(encoding="utf-8"))
        self.assertEqual(contract["schema_version"], 1)
        self.assertEqual(contract["runtime_kind"], "node")
        self.assertEqual(contract["home"], str(self.fixture.runtime.home))
        self.assertEqual(self.fixture.contract.stat().st_mode & 0o777, 0o644)
        self.assertEqual(self.fixture.invoker.stat().st_mode & 0o777, 0o755)
        unit = self.fixture.unit.read_text(encoding="utf-8")
        self.assertIn(
            "ExecStartPre=/usr/local/libexec/opencodex-runtime check", unit
        )
        self.assertIn(
            f'ReadWritePaths="{self.fixture.runtime.home}"',
            unit,
        )
        backups = list(
            (self.fixture.test_root / "var" / "backups" / "opencodex").glob(
                "runtime-migration-*"
            )
        )
        self.assertEqual(len(backups), 1)
        self.assertRegex(
            backups[0].name,
            r"^runtime-migration-\d{8}T\d{6}Z\.[A-Za-z0-9]+$",
        )
        self.assertEqual((backups[0] / "unit").read_text(encoding="utf-8"), "old-unit\n")
        self.assertTrue((self.fixture.state / "active").exists())
        self.assertTrue((self.fixture.state / "enabled").exists())
        calls = (self.fixture.state / "calls").read_text(encoding="utf-8").splitlines()
        self.assertLess(calls.index("stop opencodex.service"), calls.index("daemon-reload"))
        self.assertLess(calls.index("daemon-reload"), calls.index("start opencodex.service"))
        self.assertNotIn("restart opencodex.service", calls)

    def test_check_executes_the_byte_identical_isolated_adapter_boundary(self) -> None:
        audit = self.fixture.instrument_canary_audit()
        old_unit = self.fixture.unit.read_bytes()
        old_contract = self.fixture.contract.read_bytes()
        old_invoker = self.fixture.invoker.read_bytes()
        result = self.fixture.run("check")
        self.assertEqual(result.returncode, 0, result.stderr)
        records = [
            line.split("\t")
            for line in audit.read_text(encoding="utf-8").splitlines()
        ]
        self.assertEqual(
            [record[0] for record in records],
            [
                "check",
                "describe --json",
                "ocx --version",
                "ocx config validate",
                "npm --version",
            ],
        )
        self.assertTrue(all(len(record) == 6 for record in records))
        self.assertEqual({record[1] for record in records}, {records[0][1]})
        self.assertEqual({record[2] for record in records}, {records[0][2]})
        self.assertEqual({record[3] for record in records}, {"755"})
        self.assertEqual({record[4] for record in records}, {"644"})
        self.assertEqual({record[5] for record in records}, {"711"})
        self.assertEqual(
            Path(records[0][1]).parent,
            Path(records[0][2]).parent,
        )
        self.assertEqual(Path(records[0][1]).name, "opencodex-runtime")
        self.assertEqual(Path(records[0][2]).name, "runtime.json")
        self.assertFalse(Path(records[0][1]).exists())
        self.assertFalse(Path(records[0][2]).exists())
        self.assertEqual(self.fixture.canary_directories(), [])
        self.assertEqual(self.fixture.unit.read_bytes(), old_unit)
        self.assertEqual(self.fixture.contract.read_bytes(), old_contract)
        self.assertEqual(self.fixture.invoker.read_bytes(), old_invoker)
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )
        self.assertFalse((self.fixture.state / "calls").exists())
        configure_source = CONFIGURE.read_text(encoding="utf-8")
        self.assertIn(
            'cmp -s -- "${invoker_source}" "${canary_adapter}"',
            configure_source,
        )

    def test_check_failure_removes_the_isolated_canary_without_mutation(self) -> None:
        self.fixture.runtime.invalid_config_marker.touch()
        old_unit = self.fixture.unit.read_bytes()
        old_contract = self.fixture.contract.read_bytes()
        old_invoker = self.fixture.invoker.read_bytes()
        result = self.fixture.run("check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("isolated OpenCodex configuration is invalid", result.stderr)
        self.assertEqual(self.fixture.canary_directories(), [])
        self.assertEqual(self.fixture.unit.read_bytes(), old_unit)
        self.assertEqual(self.fixture.contract.read_bytes(), old_contract)
        self.assertEqual(self.fixture.invoker.read_bytes(), old_invoker)
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

    def test_check_signal_removes_the_isolated_canary_without_mutation(self) -> None:
        marker = self.fixture.state / "canary-blocked"
        content = self.fixture.runtime.ocx.read_text(encoding="utf-8")
        original = (
            "elif [[ ${1:-} == config && ${2:-} == validate ]]; then\n"
            f"  [[ ! -e {str(self.fixture.runtime.invalid_config_marker)!r} ]] || exit 1\n"
            "  printf 'Config is valid.\\n'\n"
        )
        replacement = (
            "elif [[ ${1:-} == config && ${2:-} == validate ]]; then\n"
            f"  touch {str(marker)!r}\n"
            "  trap 'exit 42' TERM INT HUP QUIT\n"
            "  while :; do sleep 1; done\n"
        )
        self.assertIn(original, content)
        self.fixture.runtime.ocx.write_text(
            content.replace(original, replacement),
            encoding="utf-8",
        )
        old_unit = self.fixture.unit.read_bytes()
        old_contract = self.fixture.contract.read_bytes()
        old_invoker = self.fixture.invoker.read_bytes()
        process = subprocess.Popen(
            self.fixture.command("check"),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=self.fixture.env(),
            start_new_session=True,
        )
        deadline = time.monotonic() + 30
        while not marker.exists() and process.poll() is None:
            if time.monotonic() >= deadline:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=5)
                self.fail("isolated canary did not reach the blocking command")
            time.sleep(0.05)
        self.assertIsNone(process.poll())
        os.killpg(process.pid, signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=10)
        self.assertNotEqual(process.returncode, 0, (stdout, stderr))
        self.assertNotIn("canary cleanup failed", stderr)
        self.assertEqual(self.fixture.canary_directories(), [])
        self.assertEqual(self.fixture.unit.read_bytes(), old_unit)
        self.assertEqual(self.fixture.contract.read_bytes(), old_contract)
        self.assertEqual(self.fixture.invoker.read_bytes(), old_invoker)
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

    def test_busy_service_fails_before_snapshot_or_mutation(self) -> None:
        self.fixture.runtime.busy_marker.write_text("1", encoding="utf-8")
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("busy or draining", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )
        calls = (self.fixture.state / "calls").read_text(encoding="utf-8")
        self.assertNotIn("stop opencodex.service", calls)

    def test_transitional_service_states_fail_before_snapshot_or_mutation(self) -> None:
        for state in ("activating", "deactivating"):
            with self.subTest(state=state):
                (self.fixture.state / "lifecycle-state").write_text(
                    f"{state}\n",
                    encoding="utf-8",
                )
                result = self.fixture.run("apply", "--allow-service-restart")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(
                    "ActiveState must be exactly active or inactive",
                    result.stderr,
                )
                self.assertEqual(
                    self.fixture.unit.read_text(encoding="utf-8"),
                    "old-unit\n",
                )
                self.assertEqual(
                    self.fixture.contract.read_text(encoding="utf-8"),
                    "old-contract\n",
                )
                self.assertFalse(
                    (
                        self.fixture.test_root
                        / "var"
                        / "backups"
                        / "opencodex"
                    ).exists()
                )
                (self.fixture.state / "lifecycle-state").unlink()

    def test_unhealthy_service_fails_before_snapshot_or_mutation(self) -> None:
        (self.fixture.state / "bad-health").touch()
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("health identity, status, or version is invalid", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

    def test_unreadable_npm_fails_before_snapshot_or_mutation(self) -> None:
        self.fixture.runtime.npm.chmod(0o600)
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must be service-readable", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

    def test_unsafe_existing_rollback_asset_fails_before_mutation(self) -> None:
        self.fixture.unit.chmod(0o666)
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("rollback unit has unsafe mode", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertEqual(
            self.fixture.contract.read_text(encoding="utf-8"),
            "old-contract\n",
        )

    def test_start_failure_restores_assets_and_service_state(self) -> None:
        self.fixture.unit.chmod(0o600)
        self.fixture.contract.parent.chmod(0o700)
        (self.fixture.state / "fail-start").write_text("1", encoding="utf-8")
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("restoring the previous managed assets", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")
        self.assertEqual(self.fixture.unit.stat().st_mode & 0o777, 0o600)
        self.assertEqual(
            self.fixture.contract.parent.stat().st_mode & 0o777,
            0o700,
        )
        self.assertEqual(
            self.fixture.contract.read_text(encoding="utf-8"), "old-contract\n"
        )
        self.assertIn("printf old", self.fixture.invoker.read_text(encoding="utf-8"))
        self.assertTrue((self.fixture.state / "active").exists())
        self.assertTrue((self.fixture.state / "enabled").exists())

    def test_active_swap_stops_before_install_without_requerying_old_proxy(self) -> None:
        marker = self.fixture.state / "reject-new-contract-observe"
        marker.touch()
        content = self.fixture.runtime.ocx.read_text(encoding="utf-8")
        original = (
            "elif [[ ${1:-} == observe && ${2:-} == memory && ${3:-} == --json ]]; then\n"
            "  if [[ -e $busy_marker ]]; then count=1; else count=0; fi\n"
        )
        replacement = (
            "elif [[ ${1:-} == observe && ${2:-} == memory && ${3:-} == --json ]]; then\n"
            f"  if [[ -e {str(marker)!r} ]] && grep -q schema_version {str(self.fixture.contract)!r}; then\n"
            "    printf 'Proxy is not running.\\n' >&2\n"
            "    exit 1\n"
            "  fi\n"
            "  if [[ -e $busy_marker ]]; then count=1; else count=0; fi\n"
        )
        self.assertIn(original, content)
        self.fixture.runtime.ocx.write_text(
            content.replace(original, replacement), encoding="utf-8"
        )
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = (self.fixture.state / "calls").read_text(encoding="utf-8").splitlines()
        self.assertLess(calls.index("stop opencodex.service"), calls.index("daemon-reload"))
        self.assertLess(calls.index("daemon-reload"), calls.index("start opencodex.service"))

    def test_effective_identity_and_bind_drift_fail_before_restart(self) -> None:
        for marker, expected in (
            ("bad-user", "service identity"),
            ("extra-bind", "BindReadOnlyPaths"),
        ):
            with self.subTest(marker=marker):
                Path(self.temporary.name, marker).touch()
                (self.fixture.state / marker).touch()
                result = self.fixture.run("apply", "--allow-service-restart")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)
                calls = (self.fixture.state / "calls").read_text(encoding="utf-8")
                self.assertNotIn("restart opencodex.service", calls)
                self.assertEqual(
                    self.fixture.unit.read_text(encoding="utf-8"),
                    "old-unit\n",
                )
                (self.fixture.state / marker).unlink()
                (self.fixture.state / "calls").write_text("", encoding="utf-8")

    def test_rollback_reports_unverified_when_health_or_config_cannot_be_restored(self) -> None:
        (self.fixture.state / "bad-user").touch()
        (self.fixture.state / "poison-health-on-reload").touch()
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertEqual(result.returncode, 70)
        self.assertIn("rollback could not be fully verified", result.stderr)

        (self.fixture.state / "bad-user").unlink()
        (self.fixture.state / "poison-health-on-reload").unlink()
        (self.fixture.state / "bad-health").unlink()
        (self.fixture.state / "poison-config-on-reload").touch()
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertEqual(result.returncode, 70)
        self.assertIn("rollback could not be fully verified", result.stderr)

    def test_rollback_restores_service_state_even_before_restart_attempt(self) -> None:
        (self.fixture.state / "bad-user").touch()
        (self.fixture.state / "deactivate-on-reload").touch()
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.fixture.state / "active").exists())

        (self.fixture.state / "deactivate-on-reload").unlink()
        (self.fixture.state / "active").unlink()
        (self.fixture.state / "activate-on-reload").touch()
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((self.fixture.state / "active").exists())

    def test_explicit_legacy_dropin_is_snapshotted_and_removed(self) -> None:
        logical = "/etc/systemd/system/opencodex.service.d/legacy.conf"
        physical = self.fixture.test_root / logical.lstrip("/")
        physical.parent.mkdir(parents=True)
        physical.write_text(
            "[Service]\nExecStart=\nExecStart=/legacy/ocx start\n", encoding="utf-8"
        )
        (self.fixture.state / "dropins").write_text(logical, encoding="utf-8")
        result = self.fixture.run(
            "apply",
            "--allow-service-restart",
            "--replace-legacy-drop-in",
            logical,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(physical.exists())
        backup = next(
            (self.fixture.test_root / "var" / "backups" / "opencodex").glob(
                "runtime-migration-*"
            )
        )
        self.assertIn(
            "/legacy/ocx",
            (backup / "legacy-drop-in").read_text(encoding="utf-8"),
        )

    def test_writable_legacy_dropin_is_rejected_before_snapshot(self) -> None:
        logical = "/etc/systemd/system/opencodex.service.d/legacy.conf"
        physical = self.fixture.test_root / logical.lstrip("/")
        physical.parent.mkdir(parents=True)
        physical.write_text("[Service]\nExecStart=/legacy\n", encoding="utf-8")
        physical.chmod(0o666)
        (self.fixture.state / "dropins").write_text(logical, encoding="utf-8")
        result = self.fixture.run(
            "apply",
            "--allow-service-restart",
            "--replace-legacy-drop-in",
            logical,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsafe mode", result.stderr)
        self.assertFalse(
            (self.fixture.test_root / "var" / "backups" / "opencodex").exists()
        )

    def test_unapproved_execution_dropin_and_missing_restart_consent_fail_closed(self) -> None:
        logical = "/etc/systemd/system/opencodex.service.d/unmanaged.conf"
        physical = self.fixture.test_root / logical.lstrip("/")
        physical.parent.mkdir(parents=True)
        physical.write_text("[Service]\nExecStart=/unmanaged\n", encoding="utf-8")
        (self.fixture.state / "dropins").write_text(logical, encoding="utf-8")
        result = self.fixture.run("apply", "--allow-service-restart")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unmanaged drop-in", result.stderr)

        result = self.fixture.run("apply")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("requires --allow-service-restart", result.stderr)

    def test_nested_or_noncanonical_legacy_dropin_path_is_rejected(self) -> None:
        nested = (
            "/etc/systemd/system/opencodex.service.d/nested/legacy.conf"
        )
        result = self.fixture.run(
            "apply",
            "--allow-service-restart",
            "--replace-legacy-drop-in",
            nested,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("absolute .conf beneath", result.stderr)

        traversing = (
            "/etc/systemd/system/opencodex.service.d/../"
            "opencodex.service.d/legacy.conf"
        )
        result = self.fixture.run(
            "apply",
            "--allow-service-restart",
            "--replace-legacy-drop-in",
            traversing,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("absolute .conf beneath", result.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")

    def test_test_hooks_are_rejected_without_an_explicit_safe_test_root(self) -> None:
        env = {**os.environ, "OPENCODEX_RUNTIME_TEST_SYSTEMCTL": str(self.fixture.systemctl)}
        result = subprocess.run(
            ["bash", str(CONFIGURE), "status"],
            check=False,
            capture_output=True,
            text=True,
            env=env,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("test hooks require", result.stderr)

    def test_base_unit_uses_only_the_runtime_adapter_for_opencodex(self) -> None:
        unit = UNIT_SOURCE.read_text(encoding="utf-8")
        self.assertIn(
            "ExecStartPre=/usr/local/libexec/opencodex-runtime check", unit
        )
        self.assertIn(
            "ExecStartPre=/usr/local/libexec/opencodex-runtime ocx config validate",
            unit,
        )
        self.assertIn(
            "ExecStart=/usr/local/libexec/opencodex-runtime ocx start --port 10100",
            unit,
        )
        self.assertNotIn("/opt/opencodex/bin/ocx", unit)
        self.assertNotIn("WorkingDirectory=", unit)
        self.assertNotIn("Environment=HOME=", unit)

    def test_protected_home_runtime_gets_only_a_narrow_read_only_bind(self) -> None:
        runtime_root = self.fixture.test_root / "home" / "linuxbrew" / ".runtime"
        node = runtime_root / "bin" / "node"
        npm = runtime_root / "lib" / "npm-cli.js"
        node.parent.mkdir(parents=True)
        npm.parent.mkdir(parents=True)
        shutil.copy2(self.fixture.runtime.node, node)
        shutil.copy2(self.fixture.runtime.npm, npm)
        node.chmod(0o755)
        npm.chmod(0o644)

        result = self.fixture.run(
            "apply",
            "--node-bin",
            str(node),
            "--npm-cli",
            str(npm),
            "--allow-service-restart",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        unit = self.fixture.unit.read_text(encoding="utf-8")
        self.assertIn("ProtectHome=tmpfs", unit)
        self.assertIn(f'BindReadOnlyPaths="{runtime_root}"', unit)
        self.assertNotIn(
            f'BindReadOnlyPaths="{self.fixture.test_root / "home" / "linuxbrew"}"',
            unit,
        )

    def test_different_protected_home_runtime_roots_are_rejected(self) -> None:
        first = self.fixture.test_root / "home" / "linuxbrew" / ".node" / "bin" / "node"
        second = self.fixture.test_root / "home" / "linuxbrew" / ".npm" / "npm-cli.js"
        first.parent.mkdir(parents=True)
        second.parent.mkdir(parents=True)
        shutil.copy2(self.fixture.runtime.node, first)
        shutil.copy2(self.fixture.runtime.npm, second)
        first.chmod(0o755)
        second.chmod(0o644)
        result = self.fixture.run(
            "check",
            "--node-bin",
            str(first),
            "--npm-cli",
            str(second),
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("share one protected-home runtime root", result.stderr)

    def test_protected_and_overlapping_layouts_fail_before_mutation(self) -> None:
        protected = self.fixture.run(
            "check",
            "--home",
            "/",
        )
        self.assertNotEqual(protected.returncode, 0)
        self.assertIn("ProtectHome=yes", protected.stderr)

        overlapping = self.fixture.run(
            "check",
            "--home",
            str(self.fixture.runtime.root),
        )
        self.assertNotEqual(overlapping.returncode, 0)
        self.assertIn("must not overlap", overlapping.stderr)
        self.assertEqual(self.fixture.unit.read_text(encoding="utf-8"), "old-unit\n")


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3

import copy
import importlib.util
import inspect
import json
import os
import pathlib
import plistlib
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
TOOLS = ROOT / "tools"
sys.path.insert(0, str(TOOLS))
MODULE_PATH = TOOLS / "opencodex_runtime_apple_canary.py"
SPEC = importlib.util.spec_from_file_location("opencodex_runtime_apple_canary", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
canary = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(canary)

SOURCE_REVISION = "b" * 40
INDEX_DIGEST = "sha256:" + "a" * 64
AMD64_DIGEST = "sha256:" + "c" * 64
ARM64_DIGEST = "sha256:" + "d" * 64
EXACT_IMAGE = "ghcr.io/novelkr/opencodex-runtime@" + INDEX_DIGEST


def runtime_candidate():
    return {
        "artifact_version": "2.40.0-r1",
        "source_revision": SOURCE_REVISION,
        "image": {
            "index_digest": INDEX_DIGEST,
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": AMD64_DIGEST},
                {"os": "linux", "arch": "arm64", "digest": ARM64_DIGEST},
            ],
        },
    }


def upstream_lock():
    return {
        "schema": 1,
        "image_revision": 1,
        "repository": "lidge-jun/opencodex",
        "release": {
            "id": 381148440,
            "tag": "v2.40.0",
            "published_at": "2026-09-02T10:08:02Z",
        },
        "version": "2.40.0",
        "revision": "35ff3a462e786bd5efc394dfb1a8a5cc946e454f",
        "npm": {
            "package": "@bitkyc08/opencodex",
            "version": "2.40.0",
            "integrity": "sha512-Tc/Q60gjsBUMHjQ65VshJAv+zpeHkIIbu6qltAA8skq07rA69IHiSvXKAwRBRId4lc+vSvVO/qa/cuOny8MJkg==",
            "tarball": "https://registry.npmjs.org/@bitkyc08/opencodex/-/opencodex-2.40.0.tgz",
        },
    }


def image_variant(
    os_name="linux",
    architecture="arm64",
    digest=ARM64_DIGEST,
    labels=None,
):
    if labels is None:
        labels = {
            "org.opencontainers.image.source": "https://github.com/novelKR/OpenCodex-OCI-Gateway",
            "org.opencontainers.image.version": "2.40.0-r1",
            "io.github.novelkr.opencodex.upstream.version": "2.40.0",
            "io.github.novelkr.opencodex.upstream.revision": "35ff3a462e786bd5efc394dfb1a8a5cc946e454f",
            "io.github.novelkr.opencodex.public-core.revision": SOURCE_REVISION,
        }
    return {
        "platform": {"os": os_name, "architecture": architecture},
        "digest": digest,
        "config": {
            "os": os_name,
            "architecture": architecture,
            "config": {"Labels": labels},
        },
    }


def managed_image():
    return [
        {
            "configuration": {
                "name": EXACT_IMAGE,
                "descriptor": {"digest": INDEX_DIGEST},
            },
            "variants": [image_variant()],
        }
    ]


def managed_container(
    *,
    state="running",
    host_address="127.0.0.1",
    host_port=10210,
    container_port=10100,
    proto="tcp",
    count=1,
):
    name = "runtime-name"
    return [
        {
            "id": name,
            "configuration": {
                "id": name,
                "labels": canary.ownership_labels("b" * 40, "1234", 2),
                "image": {
                    "reference": EXACT_IMAGE,
                    "descriptor": {"digest": INDEX_DIGEST},
                },
                "platform": {"os": "linux", "architecture": "arm64"},
                "mounts": [
                    {
                        "source": "/private/tmp/fixture/home",
                        "destination": "/var/lib/opencodex",
                    },
                    {
                        "source": "/private/tmp/fixture/bootstrap.sock",
                        "destination": "/run/opencodex/bootstrap.sock",
                    },
                    {"type": "tmpfs", "destination": "/tmp"},
                ],
                "publishedPorts": [
                    {
                        "hostAddress": host_address,
                        "hostPort": host_port,
                        "containerPort": container_port,
                        "proto": proto,
                        "count": count,
                    }
                ],
                "publishedSockets": [],
            },
            "status": {
                "state": state,
                "networks": [{"network": "ocx-canary-network"}],
                "startedDate": "2026-09-03T00:00:00Z",
            },
        }
    ]


class AppleCanaryContractTests(unittest.TestCase):
    def verify_runtime_inspection(self, inspected):
        canary.verify_inspection(
            inspected,
            "runtime-name",
            pathlib.Path("/private/tmp/fixture/home"),
            pathlib.Path("/private/tmp/fixture/bootstrap.sock"),
            "ocx-canary-network",
            expected_image=EXACT_IMAGE,
            expected_index_digest=INDEX_DIGEST,
            expected_state="running",
        )

    def test_production_and_canary_share_the_official_cli_trust_contract(self):
        production = (
            ROOT
            / "client"
            / "relay"
            / "internal"
            / "containerruntime"
            / "apple_cli.go"
        ).read_text(encoding="utf-8")
        for expected in (
            canary.APPLE_CONTAINER_IDENTIFIER,
            canary.APPLE_CONTAINER_TEAM_IDENTIFIER,
            canary.APPLE_CONTAINER_SIGNING_REQUIREMENT,
            '"-R=" + appleCLISigningRequirement',
            "protectedExecutable(appleContainerExecutable, string(filepath.Separator), 0)",
        ):
            with self.subTest(expected=expected):
                self.assertIn(expected, production)

    def test_system_status_requires_the_official_running_json_shape(self):
        running = {
            "status": "running",
            "appRoot": "/var/empty/runtime-canary/Library/Application Support/com.apple.container",
            "installRoot": "/usr/local",
            "logRoot": None,
            "apiServerVersion": "container-apiserver version 1.3.1",
            "apiServerCommit": "a" * 40,
            "apiServerBuild": "release",
            "apiServerAppName": "container-apiserver",
        }
        canary.validate_system_status(running)
        for change in (
            {"status": "not running"},
            {"status": True},
            {"unexpected": "field"},
            {"apiServerVersion": ""},
        ):
            candidate = running | change
            with self.subTest(change=change), self.assertRaises(canary.CanaryError):
                canary.validate_system_status(candidate)

    def test_canary_identity_binds_source_run_and_attempt(self):
        candidate = {
            "source_revision": "a" * 40,
            "workflow_run_id": "1234",
            "workflow_run_attempt": 2,
        }
        canary.validate_run_identity(candidate, "a" * 40, "1234", 2)
        for source, run_id, attempt in (
            ("b" * 40, "1234", 2),
            ("a" * 40, "1235", 2),
            ("a" * 40, "1234", 1),
            ("a" * 40, "1234", 0),
        ):
            with self.subTest(source=source, run_id=run_id, attempt=attempt), self.assertRaises(
                canary.CanaryError
            ):
                canary.validate_run_identity(candidate, source, run_id, attempt)

    def test_install_identity_requires_official_code_identifier_and_matching_receipt(self):
        receipt = plistlib.dumps(
            {
                "pkgid": "com.apple.container-installer",
                "pkg-version": "1.3.1",
            }
        )
        canary.validate_install_identity(
            "Executable=/usr/local/bin/container\n"
            "Identifier=com.apple.container.cli\n"
            "TeamIdentifier=UPBK2H6LZM\n",
            receipt,
            "bin/container\nbin/container-apiserver\n",
            "1.3.1",
        )
        cases = (
            (
                "Identifier=example.container\n",
                receipt,
                "bin/container\n",
                "1.3.1",
            ),
            (
                "Identifier=com.apple.container.cli\nTeamIdentifier=UPBK2H6LZM\n",
                plistlib.dumps(
                    {
                        "pkgid": "com.apple.container-installer",
                        "pkg-version": "1.3.0",
                    }
                ),
                "bin/container\n",
                "1.3.1",
            ),
            (
                "Identifier=com.apple.container.cli\nTeamIdentifier=UPBK2H6LZM\n",
                receipt,
                "bin/container-apiserver\n",
                "1.3.1",
            ),
            (
                "Identifier=com.apple.container.cli\nTeamIdentifier=UPBK2H6LZM\n",
                receipt,
                "bin/container\n",
                "1.3.2",
            ),
        )
        for codesign, candidate_receipt, files, version in cases:
            with self.subTest(version=version, files=files), self.assertRaises(
                canary.CanaryError
            ):
                canary.validate_install_identity(
                    codesign, candidate_receipt, files, version
                )

        with self.assertRaises(canary.CanaryError):
            canary.validate_install_identity(
                "Identifier=com.apple.container.cli\nTeamIdentifier=OTHERTEAM1\n",
                receipt,
                "bin/container\n",
                "1.3.1",
            )

    def test_cli_path_requires_protected_owner_components_and_regular_executable(self):
        with tempfile.TemporaryDirectory() as temporary:
            trusted_root = pathlib.Path(temporary)
            parent = trusted_root / "usr" / "local" / "bin"
            parent.mkdir(parents=True, mode=0o700)
            executable = parent / "container"
            executable.write_bytes(b"fixture")
            executable.chmod(0o700)

            canary.validate_protected_executable(
                executable, trusted_root=trusted_root, owner_uid=os.getuid()
            )

            if sys.platform == "darwin":
                subprocess.run(
                    [
                        "/bin/chmod",
                        "+a",
                        "everyone allow add_file,delete_child",
                        str(parent),
                    ],
                    check=True,
                    capture_output=True,
                )
                try:
                    with self.assertRaisesRegex(canary.CanaryError, "extended ACL"):
                        canary.validate_protected_executable(
                            executable,
                            trusted_root=trusted_root,
                            owner_uid=os.getuid(),
                        )
                finally:
                    subprocess.run(
                        ["/bin/chmod", "-N", str(parent)],
                        check=True,
                        capture_output=True,
                    )

            parent.chmod(0o720)
            with self.assertRaisesRegex(canary.CanaryError, "not protected"):
                canary.validate_protected_executable(
                    executable, trusted_root=trusted_root, owner_uid=os.getuid()
                )
            parent.chmod(0o700)

            with self.assertRaisesRegex(canary.CanaryError, "not protected"):
                canary.validate_protected_executable(
                    executable, trusted_root=trusted_root, owner_uid=os.getuid() + 1
                )

            executable.unlink()
            executable.symlink_to(trusted_root / "replacement")
            with self.assertRaisesRegex(canary.CanaryError, "not protected"):
                canary.validate_protected_executable(
                    executable, trusted_root=trusted_root, owner_uid=os.getuid()
                )

    def test_runtime_arguments_use_exact_digest_socket_and_loopback_without_secrets(self):
        image = "ghcr.io/novelkr/opencodex-runtime@sha256:" + "a" * 64
        home = pathlib.Path("/private/tmp/fixture/home")
        secret_socket = pathlib.Path("/private/tmp/fixture/bootstrap.sock")
        arguments = canary.runtime_arguments(
            "runtime-name",
            "network-name",
            image,
            home,
            secret_socket,
            canary.ownership_labels("b" * 40, "1234", 2),
        )
        rendered = "\n".join(arguments)
        self.assertIn(image, arguments)
        self.assertIn("127.0.0.1:10210:10100/tcp", arguments)
        self.assertIn(
            f"type=bind,source={secret_socket},target=/run/opencodex/bootstrap.sock",
            arguments,
        )
        self.assertIn("--read-only", arguments)
        self.assertIn("--cap-drop", arguments)
        self.assertIn("ALL", arguments)
        self.assertIn("--uid", arguments)
        self.assertEqual(arguments[arguments.index("--uid") + 1], str(os.getuid()))
        self.assertIn("--gid", arguments)
        self.assertEqual(arguments[arguments.index("--gid") + 1], str(os.getgid()))
        self.assertNotIn("--user", arguments)
        self.assertNotIn("--env", arguments)
        self.assertNotIn("--env-file", arguments)
        self.assertNotIn("OPENCODEX_API_AUTH_TOKEN", rendered)
        self.assertNotIn("OPENCODEX_ADMIN_AUTH_TOKEN", rendered)
        self.assertIn(
            "io.github.novelkr.opencodex.runtime-canary.run-id=1234", arguments
        )
        self.assertIn(
            "io.github.novelkr.opencodex.runtime-canary.run-attempt=2", arguments
        )

    def test_image_inspection_binds_index_arm64_digest_and_labels_to_one_variant(self):
        canary.validate_inspected_image(
            managed_image(), runtime_candidate(), EXACT_IMAGE, upstream_lock()
        )

        cases = []
        wrong_index = managed_image()
        wrong_index[0]["configuration"]["descriptor"]["digest"] = "sha256:" + "9" * 64
        cases.append(wrong_index)

        unrelated_spoof = managed_image()
        unrelated_spoof[0]["variants"][0]["digest"] = "sha256:" + "9" * 64
        unrelated_spoof[0]["spoof"] = {
            "digest": ARM64_DIGEST,
            "Labels": image_variant()["config"]["config"]["Labels"],
        }
        cases.append(unrelated_spoof)

        split_variants = managed_image()
        del split_variants[0]["variants"][0]["config"]["config"]["Labels"]
        split_variants[0]["variants"].append(
            image_variant(
                os_name="linux",
                architecture="amd64",
                digest=AMD64_DIGEST,
            )
        )
        cases.append(split_variants)

        duplicate_arm64 = managed_image()
        duplicate_arm64[0]["variants"].append(image_variant())
        cases.append(duplicate_arm64)

        for inspected in cases:
            with self.subTest(inspected=inspected), self.assertRaises(canary.CanaryError):
                canary.validate_inspected_image(
                    inspected, runtime_candidate(), EXACT_IMAGE, upstream_lock()
                )

    def test_candidate_is_bound_to_the_exact_tracked_upstream_lock_bytes(self):
        lock_bytes = (json.dumps(upstream_lock(), indent=2) + "\n").encode("utf-8")
        candidate = runtime_candidate()
        candidate["upstream_lock_sha256"] = canary.hashlib.sha256(lock_bytes).hexdigest()
        self.assertEqual(
            canary.validate_upstream_lock_binding(candidate, lock_bytes)["version"],
            "2.40.0",
        )
        with self.assertRaisesRegex(canary.CanaryError, "differs"):
            canary.validate_upstream_lock_binding(candidate, lock_bytes + b" ")

    def test_version_parser_and_secret_scanner_fail_closed(self):
        self.assertEqual(canary.semver_tuple("1.3.1"), (1, 3, 1))
        for value in ("v1.3.1", "1.3", "1.03.1", "1.3.1-rc.1"):
            with self.subTest(value=value), self.assertRaises(canary.CanaryError):
                canary.semver_tuple(value)

        token = "A" * 43
        markers = canary.protocol.secret_markers(token, "B" * 43)
        with self.assertRaises(canary.protocol.ContractError):
            canary.protocol.assert_no_secret(
                f"prefix={token[:12]}", markers, "fixture inspection"
            )

    def test_managed_container_requires_exact_identity_and_runtime_state(self):
        running = managed_container()
        configuration = canary.validate_managed_container(
            running, "runtime-name", expected_state="running"
        )
        self.assertEqual(configuration["id"], "runtime-name")

        stopped = managed_container(state="stopped")
        canary.validate_managed_container(
            stopped, "runtime-name", expected_state="stopped"
        )

        cases = []
        extra_top_level = copy.deepcopy(running)
        extra_top_level[0]["unexpected"] = True
        cases.append(extra_top_level)
        missing_top_level = copy.deepcopy(running)
        del missing_top_level[0]["status"]
        cases.append(missing_top_level)
        mismatched_configuration_id = copy.deepcopy(running)
        mismatched_configuration_id[0]["configuration"]["id"] = "foreign"
        cases.append(mismatched_configuration_id)
        boolean_state = copy.deepcopy(running)
        boolean_state[0]["status"]["state"] = True
        cases.append(boolean_state)
        unknown_state_value = copy.deepcopy(running)
        unknown_state_value[0]["status"]["state"] = "exited"
        cases.append(unknown_state_value)
        unexpected_status_field = copy.deepcopy(running)
        unexpected_status_field[0]["status"]["exitStatus"] = 0
        cases.append(unexpected_status_field)
        naive_started_date = copy.deepcopy(running)
        naive_started_date[0]["status"]["startedDate"] = "2026-09-03T00:00:00"
        cases.append(naive_started_date)
        for inspected in cases:
            with self.subTest(inspected=inspected), self.assertRaises(canary.CanaryError):
                canary.validate_managed_container(
                    inspected, "runtime-name", expected_state="running"
                )

        with self.assertRaisesRegex(canary.CanaryError, "expected running"):
            canary.validate_managed_container(
                stopped, "runtime-name", expected_state="running"
            )

    def test_inspection_requires_only_the_exact_loopback_port_mapping(self):
        inspected = managed_container()
        self.verify_runtime_inspection(inspected)

        cases = []
        public_address = copy.deepcopy(inspected)
        public_address[0]["configuration"]["publishedPorts"][0]["hostAddress"] = "0.0.0.0"
        cases.append(public_address)
        second_mapping = copy.deepcopy(inspected)
        second_mapping[0]["configuration"]["publishedPorts"].append(
            {
                "hostAddress": "127.0.0.1",
                "hostPort": 10211,
                "containerPort": 10100,
                "proto": "tcp",
                "count": 1,
            }
        )
        cases.append(second_mapping)
        wrong_guest_port = copy.deepcopy(inspected)
        wrong_guest_port[0]["configuration"]["publishedPorts"][0]["containerPort"] = 10101
        cases.append(wrong_guest_port)
        boolean_host_port = copy.deepcopy(inspected)
        boolean_host_port[0]["configuration"]["publishedPorts"][0]["hostPort"] = True
        cases.append(boolean_host_port)
        extra_port_field = copy.deepcopy(inspected)
        extra_port_field[0]["configuration"]["publishedPorts"][0]["unexpected"] = 1
        cases.append(extra_port_field)
        published_socket = copy.deepcopy(inspected)
        published_socket[0]["configuration"]["publishedSockets"] = [
            {"hostPath": "/private/tmp/foreign.sock"}
        ]
        cases.append(published_socket)
        wrong_network = copy.deepcopy(inspected)
        wrong_network[0]["status"]["networks"] = [{"network": "default"}]
        cases.append(wrong_network)
        multiple_networks = copy.deepcopy(inspected)
        multiple_networks[0]["status"]["networks"].append(
            {"network": "default"}
        )
        cases.append(multiple_networks)
        missing_network = copy.deepcopy(inspected)
        missing_network[0]["status"]["networks"] = []
        cases.append(missing_network)
        wrong_image = copy.deepcopy(inspected)
        wrong_image[0]["configuration"]["image"]["reference"] = (
            "ghcr.io/novelkr/opencodex-runtime@sha256:" + "9" * 64
        )
        cases.append(wrong_image)
        wrong_index = copy.deepcopy(inspected)
        wrong_index[0]["configuration"]["image"]["descriptor"]["digest"] = (
            "sha256:" + "9" * 64
        )
        cases.append(wrong_index)
        wrong_platform = copy.deepcopy(inspected)
        wrong_platform[0]["configuration"]["platform"]["architecture"] = "amd64"
        cases.append(wrong_platform)
        for candidate in cases:
            with self.subTest(candidate=candidate), self.assertRaises(canary.CanaryError):
                self.verify_runtime_inspection(candidate)

    def test_inspection_rejects_exact_prohibited_host_mounts(self):
        previous = os.environ.get("HOME")
        os.environ["HOME"] = "/var/empty/runtime-canary"
        try:
            inspected = managed_container()
            self.verify_runtime_inspection(inspected)
            inspected[0]["configuration"]["mounts"].append(
                {"source": "/var/empty/runtime-canary", "destination": "/host-home"}
            )
            with self.assertRaisesRegex(canary.CanaryError, "prohibited"):
                self.verify_runtime_inspection(inspected)
            inspected[0]["configuration"]["mounts"][-1] = {
                "source": "/Library/Keychains/System.keychain",
                "destination": "/host-keychain",
            }
            with self.assertRaisesRegex(canary.CanaryError, "unexpected host mount"):
                self.verify_runtime_inspection(inspected)
        finally:
            if previous is None:
                os.environ.pop("HOME", None)
            else:
                os.environ["HOME"] = previous

    def test_inspection_requires_exact_tmpfs_mount(self):
        inspected = managed_container()
        self.verify_runtime_inspection(inspected)

        missing = copy.deepcopy(inspected)
        missing[0]["configuration"]["mounts"] = missing[0]["configuration"]["mounts"][:2]
        with self.assertRaisesRegex(canary.CanaryError, "mount schema"):
            self.verify_runtime_inspection(missing)

        duplicate = copy.deepcopy(inspected)
        duplicate[0]["configuration"]["mounts"].append(
            {"source": "tmpfs", "destination": "/tmp"}
        )
        with self.assertRaisesRegex(canary.CanaryError, "mount schema"):
            self.verify_runtime_inspection(duplicate)

        wrong_type = copy.deepcopy(inspected)
        wrong_type[0]["configuration"]["mounts"][-1] = {
            "type": "virtiofs",
            "destination": "/tmp",
        }
        with self.assertRaisesRegex(canary.CanaryError, "exact /tmp tmpfs"):
            self.verify_runtime_inspection(wrong_type)

        conflicting = copy.deepcopy(inspected)
        conflicting[0]["configuration"]["mounts"][-1] = {
            "type": "bind",
            "source": "tmpfs",
            "destination": "/tmp",
        }
        with self.assertRaisesRegex(canary.CanaryError, "exact /tmp tmpfs"):
            self.verify_runtime_inspection(conflicting)

        source_only = copy.deepcopy(inspected)
        source_only[0]["configuration"]["mounts"][-1] = {
            "source": "tmpfs",
            "destination": "/tmp",
        }
        self.verify_runtime_inspection(source_only)

    def test_cleanup_requires_successful_deletion_and_absence_readback(self):
        labels = canary.ownership_labels("b" * 40, "1234", 2)
        calls = []

        def fake_container(*arguments, **kwargs):
            calls.append(arguments)
            if arguments in (
                ("inspect", "runtime-name"),
                ("network", "inspect", "network-name"),
            ):
                if arguments[0] == "inspect":
                    payload = managed_container(state="stopped")
                else:
                    payload = [
                        {
                            "id": "network-name",
                            "configuration": {"labels": labels},
                        }
                    ]
                return subprocess.CompletedProcess(arguments, 0, json.dumps(payload), "")
            if arguments in (
                ("delete", "--force", "runtime-name"),
                ("network", "delete", "network-name"),
            ):
                return subprocess.CompletedProcess(arguments, 0, "", "")
            if arguments == ("list", "--all", "--format", "json"):
                return subprocess.CompletedProcess(arguments, 0, "[]", "")
            if arguments == ("network", "list", "--format", "json"):
                return subprocess.CompletedProcess(arguments, 0, "[]", "")
            raise AssertionError(arguments)

        with mock.patch.object(canary, "container", side_effect=fake_container):
            canary.cleanup_owned_resources(
                ["runtime-name"], "network-name", labels, ["secret-marker"]
            )
        self.assertIn(("delete", "--force", "runtime-name"), calls)
        self.assertIn(("network", "delete", "network-name"), calls)
        self.assertIn(("list", "--all", "--format", "json"), calls)
        self.assertIn(("network", "list", "--format", "json"), calls)

    def test_cleanup_fails_closed_on_delete_error_or_residual_resource(self):
        labels = canary.ownership_labels("b" * 40, "1234", 2)

        def fake_owned(kind, name, expected_labels, markers, **kwargs):
            return True

        delete_error = subprocess.CompletedProcess(("delete",), 1, "", "")
        with (
            mock.patch.object(canary, "require_owned_resource", side_effect=fake_owned),
            mock.patch.object(canary, "container", return_value=delete_error),
            mock.patch.object(canary, "require_resource_absent"),
            self.assertRaisesRegex(canary.CanaryError, "cleanup did not remove"),
        ):
            canary.cleanup_owned_resources(["runtime-name"], None, labels, [])

        delete_success = subprocess.CompletedProcess(("delete",), 0, "", "")
        with (
            mock.patch.object(canary, "require_owned_resource", side_effect=fake_owned),
            mock.patch.object(canary, "container", return_value=delete_success),
            mock.patch.object(
                canary,
                "require_resource_absent",
                side_effect=canary.CanaryError("residual resource"),
            ),
            self.assertRaisesRegex(canary.CanaryError, "cleanup did not remove"),
        ):
            canary.cleanup_owned_resources(["runtime-name"], None, labels, [])

    def test_recreate_requires_absence_readback_before_dropping_ownership(self):
        source = inspect.getsource(canary.execute)
        delete = source.index('container("delete", runtime_name, markers=markers)')
        readback = source.index(
            'require_resource_absent("container", runtime_name, markers)', delete
        )
        recreate = source.index("second_api = protocol.exact_token()", readback)
        self.assertLess(delete, readback)
        self.assertLess(readback, recreate)

    def test_each_stopped_runtime_is_rescanned_before_deletion_or_cleanup(self):
        source = inspect.getsource(canary.execute)
        first_stop = source.index(
            'container("stop", "--time", "15", runtime_name, timeout=30, markers=markers)'
        )
        first_shutdown_logs = source.index(
            '"stopped Apple container logs"', first_stop
        )
        first_delete = source.index(
            'container("delete", runtime_name, markers=markers)', first_shutdown_logs
        )
        second_stop = source.index(
            'container("stop", "--time", "15", runtime_name, timeout=30, markers=all_markers)',
            first_delete,
        )
        second_shutdown_logs = source.index(
            '"recreated stopped Apple container logs"', second_stop
        )
        cleanup = source.index("cleanup_owned_resources(", second_shutdown_logs)
        self.assertLess(first_stop, first_shutdown_logs)
        self.assertLess(first_shutdown_logs, first_delete)
        self.assertLess(second_stop, second_shutdown_logs)
        self.assertLess(second_shutdown_logs, cleanup)

    def test_final_port_readback_precedes_passing_receipt(self):
        source = inspect.getsource(canary.execute)
        cleanup = source.index("cleanup_owned_resources(")
        final_port_readback = source.index("require_free_port()", cleanup)
        receipt = source.index("receipt = {", final_port_readback)
        self.assertLess(cleanup, final_port_readback)
        self.assertLess(final_port_readback, receipt)

    def test_cleanup_tracks_mutation_intent_before_any_create_command(self):
        source = inspect.getsource(canary.execute)
        intent = source.index("container_intents = [fixture_name, runtime_name]")
        network_create = source.index('"network", "create"')
        fixture_run = source.index('"run", "--detach", "--name", fixture_name')
        runtime_run = source.index("*runtime_arguments(")
        cleanup = source.index("cleanup_owned_resources(", runtime_run)
        self.assertLess(intent, network_create)
        self.assertLess(intent, fixture_run)
        self.assertLess(intent, runtime_run)
        self.assertIn("container_intents", source[cleanup:])
        self.assertIn("network_name", source[cleanup:])
        self.assertNotIn("created_containers", source)


if __name__ == "__main__":
    unittest.main()

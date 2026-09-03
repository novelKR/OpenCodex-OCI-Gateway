#!/usr/bin/env python3

import copy
import importlib.util
import inspect
import pathlib
import tempfile
import types
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
TOOLS = ROOT / "tools"
import sys

sys.path.insert(0, str(TOOLS))
SPEC = importlib.util.spec_from_file_location(
    "opencodex_runtime_lifecycle_canary",
    TOOLS / "opencodex_runtime_lifecycle_canary.py",
)
assert SPEC is not None and SPEC.loader is not None
lifecycle = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(lifecycle)


def candidate_document():
    return {
        "schema": 1,
        "artifact_kind": "opencodex-runtime-image",
        "artifact_version": "2.40.0-r1",
        "release_sequence": 101,
        "channel": "candidate",
        "source_revision": "1" * 40,
        "workflow_run_id": "4567",
        "workflow_run_attempt": 2,
        "upstream_lock_sha256": "2" * 64,
        "candidate_tag": "candidate-2.40.0-r1-" + "1" * 40,
        "image": {
            "repository": "ghcr.io/novelkr/opencodex-runtime",
            "index_digest": "sha256:" + "a" * 64,
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": "sha256:" + "b" * 64},
                {"os": "linux", "arch": "arm64", "digest": "sha256:" + "c" * 64},
            ],
        },
        "attestations": {
            "buildkit_sbom": True,
            "buildkit_provenance": "max",
            "github_provenance": True,
        },
    }


def receipt(scenario="upgrade"):
    value = {
        "schema": 1,
        "artifact_kind": "opencodex-runtime-lifecycle-canary",
        "result": "passed",
        "source_revision": "1" * 40,
        "workflow_run_id": "4567",
        "workflow_run_attempt": 2,
        "scenario": scenario,
        "candidate_sha256": "d" * 64,
        "index_digest": "sha256:" + "a" * 64,
        "arm64_digest": "sha256:" + "c" * 64,
        "baseline": {
            "artifact_version": "2.39.0-r1",
            "manifest_sha256": "e" * 64,
            "index_digest": "sha256:" + "f" * 64,
        },
        "relay_sha256": "6" * 64,
        "relayctl_sha256": "7" * 64,
        "checks": list(lifecycle.UPGRADE_CHECKS),
    }
    if scenario == "first_release":
        value["baseline"] = None
        value["checks"] = list(lifecycle.FIRST_RELEASE_CHECKS)
    return value


class LifecycleCanaryContractTests(unittest.TestCase):
    def test_runtime_mutation_arguments_require_fresh_desktop_exit_confirmation(self):
        witness = {"state_digest": "a" * 64, "routing_generation": 17}
        self.assertIn("--confirm-desktop-exited", lifecycle.activate_arguments(witness))
        self.assertIn("--confirm-desktop-exited", lifecycle.stop_arguments(witness))
        self.assertIn("--confirm-desktop-exited", lifecycle.recover_arguments(witness))

    def test_accepts_distinct_first_release_and_upgrade_receipts(self):
        for scenario in ("first_release", "upgrade"):
            with self.subTest(scenario=scenario):
                value = receipt(scenario)
                self.assertIs(lifecycle.validate_receipt(value), value)
                self.assertEqual(
                    lifecycle.canonical_receipt(value),
                    (lifecycle.json.dumps(value, indent=2, sort_keys=True) + "\n").encode(),
                )

    def test_first_release_cannot_claim_upgrade_and_upgrade_requires_baseline(self):
        first = receipt("first_release")
        first["checks"] = list(lifecycle.UPGRADE_CHECKS)
        with self.assertRaisesRegex(lifecycle.LifecycleError, "first-release"):
            lifecycle.validate_receipt(first)

        upgrade = receipt("upgrade")
        upgrade["baseline"] = None
        with self.assertRaisesRegex(lifecycle.LifecycleError, "baseline"):
            lifecycle.validate_receipt(upgrade)

        missing = receipt("upgrade")
        missing["checks"].remove("maintenance_rollback")
        with self.assertRaisesRegex(lifecycle.LifecycleError, "check set"):
            lifecycle.validate_receipt(missing)

    def test_candidate_sequence_must_exceed_authenticated_baseline(self):
        candidate = candidate_document()
        baseline = {"release_sequence": candidate["release_sequence"]}
        with self.assertRaisesRegex(lifecycle.LifecycleError, "newer"):
            lifecycle.require_newer_release_sequence(candidate, baseline)
        candidate["release_sequence"] += 1
        lifecycle.require_newer_release_sequence(candidate, baseline)

    def test_rejects_unknown_fields_wrong_identity_and_digest_alias(self):
        mutations = (
            lambda value: value.update(extra=True),
            lambda value: value.update(schema=True),
            lambda value: value.update(result="failed"),
            lambda value: value.update(arm64_digest=value["index_digest"]),
            lambda value: value.update(workflow_run_attempt=0),
        )
        for mutation in mutations:
            value = receipt()
            mutation(value)
            with self.assertRaises(
                (lifecycle.LifecycleError, lifecycle.manifest_contract.ContractError)
            ):
                lifecycle.validate_receipt(value)

    def test_verify_receipt_binds_candidate_bytes_source_run_attempt_and_digests(self):
        candidate = candidate_document()
        candidate_bytes = lifecycle.manifest_contract.canonical_candidate(candidate)
        value = receipt("first_release")
        value["candidate_sha256"] = lifecycle.sha256_bytes(candidate_bytes)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate_path = root / "candidate.json"
            receipt_path = root / "receipt.json"
            candidate_path.write_bytes(candidate_bytes)
            receipt_path.write_bytes(lifecycle.canonical_receipt(value))
            arguments = types.SimpleNamespace(
                candidate=candidate_path,
                receipt=receipt_path,
                source_revision=candidate["source_revision"],
                workflow_run_id=candidate["workflow_run_id"],
                workflow_run_attempt=candidate["workflow_run_attempt"],
                index_digest=candidate["image"]["index_digest"],
                arm64_digest=candidate["image"]["platforms"][1]["digest"],
            )
            lifecycle.verify_receipt(arguments)
            for field, bad in (
                ("workflow_run_attempt", 3),
                ("index_digest", "sha256:" + "8" * 64),
                ("source_revision", "9" * 40),
            ):
                changed = copy.copy(arguments)
                setattr(changed, field, bad)
                with self.subTest(field=field), self.assertRaisesRegex(
                    lifecycle.LifecycleError, "not bound"
                ):
                    lifecycle.verify_receipt(changed)

            receipt_path.write_bytes(
                lifecycle.json.dumps(value, separators=(",", ":")).encode()
            )
            with self.assertRaisesRegex(lifecycle.LifecycleError, "canonical"):
                lifecycle.verify_receipt(arguments)

    def test_runtime_environment_drops_workflow_and_signing_credentials(self):
        environment = lifecycle.clean_environment(
            {
                "PATH": "/bin",
                "GH_TOKEN": "github-secret",
                "RUNTIME_SIGNING_KEY_B64": "signing-secret",
                "SSH_AUTH_SOCK": "/private/tmp/agent.sock",
                "LANG": "C",
            },
            pathlib.Path("/private/tmp/canary-home"),
        )
        self.assertEqual(environment["PATH"], "/bin")
        self.assertEqual(environment["HOME"], "/private/tmp/canary-home")
        self.assertNotIn("GH_TOKEN", environment)
        self.assertNotIn("RUNTIME_SIGNING_KEY_B64", environment)
        self.assertNotIn("SSH_AUTH_SOCK", environment)

    def test_go_build_environment_uses_only_isolated_writable_state(self):
        with tempfile.TemporaryDirectory() as directory:
            temporary = pathlib.Path(directory)
            with mock.patch.dict(
                lifecycle.os.environ,
                {
                    "HOME": "/var/empty/runtime-user",
                    "GOMODCACHE": "/var/empty/runtime-user/go/pkg/mod",
                    "GH_TOKEN": "secret",
                },
                clear=True,
            ):
                environment = lifecycle.go_build_environment(temporary)
            self.assertEqual(environment["HOME"], str(temporary / "go-home"))
            self.assertEqual(
                environment["GOMODCACHE"], str(temporary / "go-module-cache")
            )
            self.assertEqual(
                environment["GOCACHE"], str(temporary / "go-build-cache")
            )
            self.assertNotIn("GH_TOKEN", environment)
            for name in ("go-home", "go-module-cache", "go-build-cache"):
                self.assertEqual((temporary / name).stat().st_mode & 0o777, 0o700)

    def test_fixed_runtime_cleanup_distinguishes_absence_from_inspect_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            home = pathlib.Path(directory)
            with mock.patch.object(
                lifecycle.apple, "resource_names", return_value=set()
            ), mock.patch.object(lifecycle.apple, "container") as container:
                lifecycle.cleanup_fixed_runtime(home)
                container.assert_not_called()

            failed = types.SimpleNamespace(returncode=1, stdout="", stderr="failed")
            with mock.patch.object(
                lifecycle.apple,
                "resource_names",
                return_value={lifecycle.RUNTIME_CONTAINER},
            ), mock.patch.object(
                lifecycle.apple, "container", return_value=failed
            ):
                with self.assertRaisesRegex(
                    lifecycle.LifecycleError, "could not be inspected"
                ):
                    lifecycle.cleanup_fixed_runtime(home)

    def test_resident_relay_requires_zero_sigterm_exit(self):
        process = mock.Mock()
        process.wait.return_value = 3
        with self.assertRaisesRegex(lifecycle.LifecycleError, "exit cleanly"):
            lifecycle.stop_resident_relay(process)
        process.send_signal.assert_called_once_with(lifecycle.signal.SIGTERM)
        process.kill.assert_not_called()

    def test_release_fixture_close_requires_thread_shutdown(self):
        fixture = lifecycle.ReleaseFixture(pathlib.Path("cert"), pathlib.Path("key"))
        fixture.server = mock.Mock()
        fixture.thread = mock.Mock()
        fixture.thread.is_alive.return_value = True
        with self.assertRaisesRegex(lifecycle.LifecycleError, "did not stop"):
            fixture.close()
        fixture.server.shutdown.assert_called_once_with()
        fixture.server.server_close.assert_called_once_with()
        fixture.thread.join.assert_called_once_with(timeout=5)

    def test_secret_scan_reads_exact_runtime_surfaces_and_durable_journals(self):
        with tempfile.TemporaryDirectory() as directory:
            home = pathlib.Path(directory) / "home"
            config = home / ".config" / "opencodex-relay" / "relay.json"
            relay_log = pathlib.Path(directory) / "relay.log"
            relay_log.write_text("relay-clean\n", encoding="utf-8")
            state = lifecycle.runtime_state_root(home) / "state.json"
            state.parent.mkdir(parents=True)
            state.write_text("{}\n", encoding="utf-8")
            routing = pathlib.Path(str(config) + ".routing-state.json")
            routing.parent.mkdir(parents=True)
            routing.write_text("{}\n", encoding="utf-8")
            runtime_routing = pathlib.Path(str(config) + ".runtime-routing.json")
            runtime_routing.write_text("{}\n", encoding="utf-8")
            command = mock.Mock(
                side_effect=[
                    types.SimpleNamespace(stdout="inspect-clean"),
                    types.SimpleNamespace(stdout="logs-clean"),
                    types.SimpleNamespace(stdout="list-clean"),
                ]
            )
            checked: list[str] = []

            def remember(value, _markers, description):
                captured = (
                    value.decode("utf-8", "replace")
                    if isinstance(value, bytes)
                    else value
                )
                checked.append(description + ":" + captured)

            with mock.patch.object(lifecycle.apple, "container", command), mock.patch.object(
                lifecycle.protocol, "assert_no_secret", side_effect=remember
            ):
                lifecycle.assert_no_runtime_secrets(
                    home,
                    config,
                    relay_log,
                    ["secret-marker"],
                    [("relayctl transcript", b"argv-clean\nstdout-clean\nstderr-clean")],
                    include_container=True,
                )

            self.assertEqual(
                command.call_args_list,
                [
                    mock.call(
                        "inspect",
                        lifecycle.RUNTIME_CONTAINER,
                        markers=["secret-marker"],
                    ),
                    mock.call(
                        "logs",
                        lifecycle.RUNTIME_CONTAINER,
                        markers=["secret-marker"],
                    ),
                    mock.call(
                        "list",
                        "--all",
                        "--format",
                        "json",
                        markers=["secret-marker"],
                    ),
                ],
            )
            self.assertTrue(any(item.startswith("Apple Container inspect:") for item in checked))
            self.assertTrue(any(item.startswith("Apple Container logs:") for item in checked))
            self.assertTrue(any(item.startswith("Apple Container list:") for item in checked))
            self.assertTrue(any(item.startswith("relayctl transcript:") for item in checked))
            self.assertEqual(
                sum(item.startswith("runtime or routing journal:") for item in checked),
                3,
            )

    def test_relayctl_transcripts_are_bounded_and_scanned_after_tokens_exist(self):
        captures: list[tuple[str, bytes]] = []
        lifecycle.capture_relayctl_transcript(
            captures,
            ["/trusted/relayctl", "container-runtime", "inspect"],
            b'{"ok":true}\n',
            b"bounded diagnostic\n",
        )
        self.assertEqual(len(captures), 1)
        self.assertIn(b"argv\0", captures[0][1])
        self.assertIn(b"stdout\0", captures[0][1])
        self.assertIn(b"stderr\0", captures[0][1])
        with self.assertRaisesRegex(lifecycle.LifecycleError, "64 KiB"):
            lifecycle.capture_relayctl_transcript(
                captures,
                ["relayctl", "inspect"],
                b"x" * (lifecycle.MAX_RECEIPT + 1),
                b"",
            )

    def test_lifecycle_canary_uses_one_owned_internal_network(self):
        source = inspect.getsource(lifecycle.execute)
        build = source.index("build_current_binaries")
        create = source.index('"create",\n                "--internal"')
        fixture_run = source.index('"--network", network_name', create)
        runtime_verify = source.index(
            "require_container_network(RUNTIME_CONTAINER", fixture_run
        )
        fixture_cleanup = source.index(
            'apple.container("delete", "--force", fixture_name', runtime_verify
        )
        network_cleanup = source.index(
            '"network", "delete", network_name', fixture_cleanup
        )
        self.assertLess(build, create)
        self.assertLess(create, fixture_run)
        self.assertLess(fixture_run, runtime_verify)
        self.assertLess(runtime_verify, fixture_cleanup)
        self.assertLess(fixture_cleanup, network_cleanup)

        build_source = inspect.getsource(lifecycle.build_current_binaries)
        self.assertIn("runtimeCanaryNetworkName={network_name}", build_source)

    def test_exact_container_network_rejects_default_or_multiple_membership(self):
        valid = [{
            "id": "runtime",
            "configuration": {"id": "runtime"},
            "status": {
                "state": "running",
                "networks": [{"network": "ocx-lifecycle-canary-012345abcdef"}],
            },
        }]
        result = types.SimpleNamespace(stdout=lifecycle.json.dumps(valid))
        with mock.patch.object(
            lifecycle.apple, "container", return_value=result
        ):
            lifecycle.require_container_network(
                "runtime", "ocx-lifecycle-canary-012345abcdef", []
            )
        for networks in (
            [{"network": "default"}],
            [
                {"network": "default"},
                {"network": "ocx-lifecycle-canary-012345abcdef"},
            ],
            [],
        ):
            invalid = copy.deepcopy(valid)
            invalid[0]["status"]["networks"] = networks
            result = types.SimpleNamespace(stdout=lifecycle.json.dumps(invalid))
            with self.subTest(networks=networks), mock.patch.object(
                lifecycle.apple, "container", return_value=result
            ), self.assertRaisesRegex(lifecycle.LifecycleError, "exact network"):
                lifecycle.require_container_network(
                    "runtime", "ocx-lifecycle-canary-012345abcdef", []
                )

    def test_live_secret_surfaces_are_scanned_before_final_stop(self):
        source = inspect.getsource(lifecycle.execute)
        live_scan = source.index("include_container=True")
        final_stop = source.index("stop_arguments(current)")
        durable_scan = source.index("include_container=False")
        relay_shutdown = source.index("stop_resident_relay(relay_process)")
        post_shutdown_scan = source.index("include_container=False", relay_shutdown)
        receipt = source.index("final_receipt = {", post_shutdown_scan)
        self.assertLess(live_scan, final_stop)
        self.assertLess(final_stop, durable_scan)
        self.assertLess(durable_scan, relay_shutdown)
        self.assertLess(relay_shutdown, post_shutdown_scan)
        self.assertLess(post_shutdown_scan, receipt)


if __name__ == "__main__":
    unittest.main()

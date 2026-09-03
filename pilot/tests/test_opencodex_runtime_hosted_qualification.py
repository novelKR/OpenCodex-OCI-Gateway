#!/usr/bin/env python3

import argparse
import copy
import importlib.util
import json
import pathlib
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
TOOLS = ROOT / "tools"
MODULE_PATH = TOOLS / "opencodex_runtime_hosted_qualification.py"
SPEC = importlib.util.spec_from_file_location(
    "opencodex_runtime_hosted_qualification", MODULE_PATH
)
assert SPEC is not None and SPEC.loader is not None
hosted = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(hosted)

SOURCE = "1" * 40
INDEX = "sha256:" + "a" * 64
AMD64 = "sha256:" + "b" * 64
ARM64 = "sha256:" + "c" * 64
RUN_ID = "4567"
RUN_ATTEMPT = 2


def candidate_document():
    return {
        "schema": 1,
        "artifact_kind": "opencodex-runtime-image",
        "artifact_version": "2.40.0-r1",
        "release_sequence": 101,
        "channel": "candidate",
        "source_revision": SOURCE,
        "workflow_run_id": RUN_ID,
        "workflow_run_attempt": RUN_ATTEMPT,
        "upstream_lock_sha256": "d" * 64,
        "candidate_tag": f"candidate-2.40.0-r1-{SOURCE}",
        "image": {
            "repository": "ghcr.io/novelkr/opencodex-runtime",
            "index_digest": INDEX,
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": AMD64},
                {"os": "linux", "arch": "arm64", "digest": ARM64},
            ],
        },
        "attestations": {
            "buildkit_sbom": True,
            "buildkit_provenance": "max",
            "github_provenance": True,
        },
    }


def common(candidate: pathlib.Path):
    return {
        "candidate": candidate,
        "source_revision": SOURCE,
        "workflow_run_id": RUN_ID,
        "workflow_run_attempt": RUN_ATTEMPT,
        "index_digest": INDEX,
        "arm64_digest": ARM64,
    }


class HostedQualificationTests(unittest.TestCase):
    def create_chain(self, root: pathlib.Path):
        candidate = root / "candidate.json"
        candidate.write_bytes(hosted.manifest_contract.canonical_candidate(candidate_document()))
        linux = root / "linux.json"
        macos = root / "macos.json"
        qualification = root / "hosted.json"
        public = root / "public.json"

        self.assertEqual(
            hosted.command_create_platform(
                argparse.Namespace(
                    **common(candidate),
                    runner_environment="github-hosted",
                    runner_os="Linux",
                    runner_arch="ARM64",
                    output=linux,
                ),
                "linux",
            ),
            0,
        )
        self.assertEqual(
            hosted.command_create_platform(
                argparse.Namespace(
                    **common(candidate),
                    runner_environment="github-hosted",
                    runner_os="macOS",
                    runner_arch="ARM64",
                    output=macos,
                ),
                "macos",
            ),
            0,
        )
        self.assertEqual(
            hosted.command_create_hosted(
                argparse.Namespace(
                    **common(candidate),
                    linux_receipt=linux,
                    macos_receipt=macos,
                    output=qualification,
                )
            ),
            0,
        )
        self.assertEqual(
            hosted.command_create_public(
                argparse.Namespace(
                    **common(candidate),
                    runner_environment="github-hosted",
                    runner_os="Linux",
                    runner_arch="ARM64",
                    hosted_receipt=qualification,
                    verification_workflow_run_id="9001",
                    verification_workflow_run_attempt=3,
                    output=public,
                )
            ),
            0,
        )
        return candidate, linux, macos, qualification, public

    def test_receipt_chain_is_candidate_bound_and_never_stable_or_apple_live(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = self.create_chain(pathlib.Path(directory))
            candidate, linux, macos, qualification, public = paths
            for kind, path in zip(
                ("linux", "macos", "hosted", "public"), paths[1:]
            ):
                with self.subTest(kind=kind):
                    receipt, data = hosted.load_receipt(path, kind)
                    self.assertEqual(data, hosted.canonical(receipt))
                    self.assertFalse(receipt["apple_container_live"])
                    self.assertFalse(receipt["stable_promotion_eligible"])
                    self.assertEqual(receipt["source_revision"], SOURCE)
                    self.assertEqual(receipt["workflow_run_id"], RUN_ID)
                    self.assertEqual(receipt["workflow_run_attempt"], RUN_ATTEMPT)
                    self.assertEqual(receipt["image"]["index_digest"], INDEX)
                    self.assertEqual(receipt["image"]["arm64_digest"], ARM64)
            public_receipt, _ = hosted.load_receipt(public, "public")
            self.assertTrue(public_receipt["anonymous_exact_digest_pull"])
            self.assertTrue(public_receipt["public_ready"])
            self.assertEqual(public_receipt["qualification_level"], "public-candidate")
            self.assertEqual(
                public_receipt["runner"],
                {"environment": "github-hosted", "os": "Linux", "arch": "ARM64"},
            )

            verify = argparse.Namespace(
                **common(candidate),
                receipt=qualification,
                linux_receipt=linux,
                macos_receipt=macos,
            )
            self.assertEqual(hosted.command_verify_hosted(verify), 0)

    def test_rejects_unknown_duplicate_trailing_noncanonical_and_oversized_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            _, linux, _, _, _ = self.create_chain(root)
            document = json.loads(linux.read_text(encoding="utf-8"))

            cases = {
                "unknown": hosted.canonical({**document, "extra": True}),
                "duplicate": b'{"schema":1,"schema":1}\n',
                "trailing": hosted.canonical(document) + b"{}\n",
                "noncanonical": (json.dumps(document) + "\n").encode("utf-8"),
                "oversized": b"x" * (hosted.MAX_RECEIPT_BYTES + 1),
            }
            for name, data in cases.items():
                with self.subTest(name=name):
                    path = root / f"bad-{name}.json"
                    path.write_bytes(data)
                    with self.assertRaises(hosted.ContractError):
                        hosted.load_receipt(path, "linux")

    def test_rejects_stable_apple_live_runner_and_candidate_mismatch_claims(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate, linux, _, qualification, _ = self.create_chain(root)
            original = json.loads(linux.read_text(encoding="utf-8"))
            mutations = {
                "stable": lambda value: value.update(stable_promotion_eligible=True),
                "apple": lambda value: value.update(apple_container_live=True),
                "runner": lambda value: value["runner"].update(environment="self-hosted"),
                "digest": lambda value: value["image"].update(
                    arm64_digest="sha256:" + "e" * 64
                ),
            }
            for name, mutate in mutations.items():
                with self.subTest(name=name):
                    value = copy.deepcopy(original)
                    mutate(value)
                    path = root / f"claim-{name}.json"
                    path.write_bytes(hosted.canonical(value))
                    with self.assertRaises(hosted.ContractError):
                        if name == "digest":
                            receipt, _ = hosted.load_receipt(path, "linux")
                            _, _, identity = hosted.load_candidate(
                                candidate,
                                source_revision=SOURCE,
                                workflow_run_id=RUN_ID,
                                workflow_run_attempt=RUN_ATTEMPT,
                                index_digest=INDEX,
                                arm64_digest=ARM64,
                            )
                            hosted.verify_binding(receipt, identity)
                        else:
                            hosted.load_receipt(path, "linux")

            hosted_receipt = json.loads(qualification.read_text(encoding="utf-8"))
            hosted_receipt["evidence"]["linux_arm64_canary_sha256"] = "f" * 64
            altered = root / "hosted-altered.json"
            altered.write_bytes(hosted.canonical(hosted_receipt))
            with self.assertRaisesRegex(hosted.ContractError, "evidence hashes"):
                hosted.command_verify_hosted(
                    argparse.Namespace(
                        **common(candidate),
                        receipt=altered,
                        linux_receipt=linux,
                        macos_receipt=root / "macos.json",
                    )
                )

    def test_platform_receipt_requires_expected_github_hosted_runner_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate = root / "candidate.json"
            candidate.write_bytes(
                hosted.manifest_contract.canonical_candidate(candidate_document())
            )
            for runner_environment, runner_os, runner_arch in (
                ("github-hosted", "Linux", "X64"),
                ("github-hosted", "macOS", "ARM64"),
                ("self-hosted", "Linux", "ARM64"),
            ):
                with self.subTest(
                    runner_environment=runner_environment,
                    runner_os=runner_os,
                    runner_arch=runner_arch,
                ):
                    with self.assertRaisesRegex(hosted.ContractError, "Linux/ARM64"):
                        hosted.command_create_platform(
                            argparse.Namespace(
                                **common(candidate),
                                runner_environment=runner_environment,
                                runner_os=runner_os,
                                runner_arch=runner_arch,
                                output=root / f"{runner_os}-{runner_arch}.json",
                            ),
                            "linux",
                        )

    def test_public_receipt_rejects_self_hosted_runner_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            candidate, _, _, qualification, _ = self.create_chain(root)
            with self.assertRaisesRegex(hosted.ContractError, "GitHub-hosted Linux/ARM64"):
                hosted.command_create_public(
                    argparse.Namespace(
                        **common(candidate),
                        runner_environment="self-hosted",
                        runner_os="Linux",
                        runner_arch="ARM64",
                        hosted_receipt=qualification,
                        verification_workflow_run_id="9002",
                        verification_workflow_run_attempt=1,
                        output=root / "rejected-public.json",
                    )
                )


if __name__ == "__main__":
    unittest.main()

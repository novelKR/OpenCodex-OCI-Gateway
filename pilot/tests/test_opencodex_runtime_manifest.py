#!/usr/bin/env python3

import copy
import base64
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "tools" / "opencodex_runtime_manifest.py"
SPEC = importlib.util.spec_from_file_location("opencodex_runtime_manifest", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
runtime = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime)

DIGESTS = {
    "index": "sha256:" + "a" * 64,
    "amd64": "sha256:" + "b" * 64,
    "arm64": "sha256:" + "c" * 64,
}


def index_document():
    return {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": DIGESTS["amd64"],
                "size": 100,
                "platform": {"os": "linux", "architecture": "amd64"},
            },
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": DIGESTS["arm64"],
                "size": 101,
                "platform": {"os": "linux", "architecture": "arm64"},
            },
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": "sha256:" + "d" * 64,
                "size": 102,
                "platform": {"os": "unknown", "architecture": "unknown"},
                "annotations": {
                    "vnd.docker.reference.type": "attestation-manifest",
                    "vnd.docker.reference.digest": DIGESTS["amd64"],
                },
            },
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": "sha256:" + "e" * 64,
                "size": 103,
                "platform": {"os": "unknown", "architecture": "unknown"},
                "annotations": {
                    "vnd.docker.reference.type": "attestation-manifest",
                    "vnd.docker.reference.digest": DIGESTS["arm64"],
                },
            },
        ],
    }


def sbom_document():
    return {
        "spdxVersion": "SPDX-2.3",
        "SPDXID": "SPDXRef-DOCUMENT",
        "dataLicense": "CC0-1.0",
        "name": "runtime",
        "documentNamespace": "https://example.invalid/spdx/runtime",
        "creationInfo": {
            "created": "2026-09-03T00:00:00Z",
            "creators": ["Tool: buildkit-test"],
        },
        "packages": [],
    }


def provenance_document(builder_platform="linux/amd64"):
    return {
        "builder": {"id": ""},
        "buildType": "https://mobyproject.org/buildkit@v1",
        "materials": [
            {
                "uri": "pkg:docker/library/bun@sha256:fixture",
                "digest": {"sha256": "1" * 64},
            }
        ],
        "invocation": {
            "parameters": {"frontend": "dockerfile.v0"},
            "environment": {"platform": builder_platform},
        },
        "buildConfig": {"llbDefinition": [{"id": "step0", "op": {}}]},
        "metadata": {
            "completeness": {
                "parameters": True,
                "environment": True,
                "materials": False,
            }
        },
    }


def provenance_v1_document(builder_platform="linux/amd64"):
    return {
        "buildDefinition": {
            "buildType": (
                "https://github.com/moby/buildkit/blob/master/docs/attestations/"
                "slsa-definitions.md"
            ),
            "externalParameters": {
                "request": {"frontend": "dockerfile.v0", "locals": []}
            },
            "internalParameters": {
                "builderPlatform": builder_platform,
                "buildConfig": {
                    "llbDefinition": [{"id": "step0", "op": {}}]
                }
            },
            "resolvedDependencies": [
                {
                    "uri": "pkg:docker/library/bun@sha256:fixture",
                    "digest": {"sha256": "1" * 64},
                }
            ],
        },
        "runDetails": {
            "builder": {"id": ""},
            "metadata": {
                "buildkit_completeness": {
                    "request": True,
                    "resolvedDependencies": False,
                }
            },
        },
    }


def manifest_document():
    return {
        "schema": 1,
        "artifact_kind": "opencodex-runtime-image",
        "artifact_version": "2.40.0-r1",
        "release_sequence": 101,
        "channel": "stable",
        "source": {
            "repository": "novelKR/OpenCodex-OCI-Gateway",
            "revision": "1" * 40,
            "upstream_lock_sha256": "2" * 64,
        },
        "upstream": {
            "repository": "lidge-jun/opencodex",
            "release_id": 381148440,
            "release_tag": "v2.40.0",
            "version": "2.40.0",
            "revision": "3" * 40,
            "npm_package": "@bitkyc08/opencodex",
            "npm_integrity": "sha512-" + base64.b64encode(bytes(64)).decode("ascii"),
        },
        "image": {
            "repository": "ghcr.io/novelkr/opencodex-runtime",
            "index_digest": DIGESTS["index"],
            "platforms": [
                {"os": "linux", "arch": "amd64", "digest": DIGESTS["amd64"]},
                {"os": "linux", "arch": "arm64", "digest": DIGESTS["arm64"]},
            ],
        },
        "compatibility": {
            "minimum_relay_version": "0.3.9",
            "minimum_macos": "26.0",
            "minimum_apple_container": "1.3.1",
            "management_api_revision": 1,
            "secret_delivery": "uds-v1",
            "state_format_revision": 1,
        },
        "canary": {
            "source_revision": "1" * 40,
            "workflow_run_id": "4567",
            "workflow_run_attempt": 1,
            "result": "passed",
        },
        "trust_key_id": "4" * 64,
    }


class RuntimeManifestTests(unittest.TestCase):
    def test_accepts_exact_stable_schema_and_canonical_bytes(self):
        document = manifest_document()
        self.assertIs(runtime.validate_manifest(document), document)
        canonical = runtime.canonical_manifest(document)
        self.assertTrue(canonical.endswith(b"\n"))
        self.assertEqual(runtime.load_json_bytes(canonical, "fixture"), document)
        reordered = json.dumps(document, indent=2).encode("utf-8") + b"\n"
        self.assertNotEqual(reordered, canonical)

    def test_rejects_unknown_duplicate_trailing_and_oversized_json(self):
        value = manifest_document()
        value["extra"] = True
        with self.assertRaisesRegex(runtime.ContractError, "unsupported fields"):
            runtime.validate_manifest(value)
        with self.assertRaisesRegex(runtime.ContractError, "duplicate JSON key"):
            runtime.load_json_bytes(b'{"schema":1,"schema":1}', "fixture")
        for constant in (b"NaN", b"Infinity", b"-Infinity", b"1e999"):
            with self.subTest(constant=constant), self.assertRaisesRegex(
                runtime.ContractError, "non-finite JSON number"
            ):
                runtime.load_json_bytes(b'{"value":' + constant + b"}", "fixture")
        with self.assertRaisesRegex(runtime.ContractError, "complete JSON"):
            runtime.load_json_bytes(b"{} {}", "fixture")
        with self.assertRaisesRegex(runtime.ContractError, "size"):
            runtime.load_json_bytes(b"x" * (runtime.MAX_MANIFEST_BYTES + 1), "fixture")

    def test_versions_sequences_and_workflow_ids_fit_consumer_integer_widths(self):
        self.assertEqual(
            runtime.version_tuple(f"{runtime.UINT32_MAX}.0.0-r{runtime.UINT64_MAX}"),
            (runtime.UINT32_MAX, 0, 0, runtime.UINT64_MAX),
        )
        for value, message in (
            (f"{runtime.UINT32_MAX + 1}.0.0-r1", "UInt32"),
            (f"1.0.0-r{runtime.UINT64_MAX + 1}", "UInt64"),
            (f"{'9' * 5000}.0.0-r1", "UInt32"),
            (f"1.0.0-r{'9' * 5000}", "UInt64"),
        ):
            with self.subTest(value=value), self.assertRaisesRegex(
                runtime.ContractError, message
            ):
                runtime.version_tuple(value)

        for field in (
            "release_sequence",
            "release_id",
            "workflow_run_id",
            "workflow_run_attempt",
        ):
            value = manifest_document()
            if field == "workflow_run_id":
                value["canary"][field] = str(runtime.UINT64_MAX + 1)
            elif field == "workflow_run_attempt":
                value["canary"][field] = runtime.INT64_MAX + 1
            elif field == "release_id":
                value["upstream"][field] = runtime.INT64_MAX + 1
            else:
                value[field] = runtime.UINT64_MAX + 1
            with self.subTest(field=field), self.assertRaises(runtime.ContractError):
                runtime.validate_manifest(value)

    def test_rejects_wrong_kind_channel_platform_digest_and_sequence(self):
        mutations = {
            "kind": lambda value: value.update(artifact_kind="other"),
            "channel": lambda value: value.update(channel="candidate"),
            "sequence": lambda value: value.update(release_sequence=0),
            "index": lambda value: value["image"].update(index_digest="latest"),
            "platform": lambda value: value["image"]["platforms"][0].update(arch="arm64"),
            "minimum": lambda value: value["compatibility"].update(minimum_macos="25.0"),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                value = copy.deepcopy(manifest_document())
                mutation(value)
                with self.assertRaises(runtime.ContractError):
                    runtime.validate_manifest(value)

    def test_rejects_boolean_aliases_for_integer_schema_and_revisions(self):
        for field in ("schema", "management_api_revision", "state_format_revision"):
            value = copy.deepcopy(manifest_document())
            if field == "schema":
                value[field] = True
            else:
                value["compatibility"][field] = True
            with self.subTest(field=field), self.assertRaises(runtime.ContractError):
                runtime.validate_manifest(value)

    def test_index_requires_two_executable_platforms_and_attestations(self):
        inspected = runtime.inspect_index(index_document(), DIGESTS["index"])
        self.assertEqual(inspected["amd64_digest"], DIGESTS["amd64"])
        self.assertEqual(inspected["arm64_digest"], DIGESTS["arm64"])
        self.assertEqual(inspected["attestation_descriptors"], "2")

        no_attestation = index_document()
        no_attestation["manifests"] = no_attestation["manifests"][:2]
        with self.assertRaisesRegex(runtime.ContractError, "attestations"):
            runtime.inspect_index(no_attestation, DIGESTS["index"])

        duplicate = index_document()
        duplicate["manifests"][1]["platform"]["architecture"] = "amd64"
        with self.assertRaisesRegex(runtime.ContractError, "duplicate"):
            runtime.inspect_index(duplicate, DIGESTS["index"])

        foreign = index_document()
        foreign["manifests"].append(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "digest": "sha256:" + "f" * 64,
                "size": 104,
                "platform": {"os": "windows", "architecture": "amd64"},
            }
        )
        with self.assertRaisesRegex(runtime.ContractError, "unexpected"):
            runtime.inspect_index(foreign, DIGESTS["index"])

    def test_index_file_bytes_are_bound_to_the_declared_digest(self):
        data = json.dumps(index_document(), separators=(",", ":")).encode("utf-8")
        actual = "sha256:" + hashlib.sha256(data).hexdigest()
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "index.json"
            path.write_bytes(data)
            self.assertEqual(runtime.load_index(path, actual)["index_digest"], actual)
            with self.assertRaisesRegex(runtime.ContractError, "bytes do not match"):
                runtime.load_index(path, DIGESTS["index"])

    def test_attestation_readback_requires_spdx_and_buildkit_max_for_each_platform(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            paths = []
            for suffix in ("amd64", "arm64"):
                sbom = root / f"sbom-{suffix}.json"
                provenance = root / f"provenance-{suffix}.json"
                sbom.write_text(json.dumps(sbom_document()), encoding="utf-8")
                provenance.write_text(
                    json.dumps(provenance_document()), encoding="utf-8"
                )
                paths.extend((sbom, provenance))
            self.assertEqual(
                runtime.inspect_attestations(*paths, "linux/amd64"),
                {
                    "sbom": "spdx",
                    "provenance": "buildkit-max",
                    "platforms": "linux/amd64,linux/arm64",
                },
            )
            with self.assertRaisesRegex(runtime.ContractError, "builder platform is invalid"):
                runtime.inspect_attestations(*paths, "linux/s390x")

            bad_sbom = sbom_document()
            bad_sbom.pop("SPDXID")
            paths[0].write_text(json.dumps(bad_sbom), encoding="utf-8")
            with self.assertRaisesRegex(runtime.ContractError, "SPDX"):
                runtime.inspect_attestations(*paths, "linux/amd64")

            paths[0].write_text(json.dumps(sbom_document()), encoding="utf-8")
            provenance_only = provenance_document()
            paths[0].write_text(json.dumps(provenance_only), encoding="utf-8")
            with self.assertRaisesRegex(runtime.ContractError, "SPDX"):
                runtime.inspect_attestations(*paths, "linux/amd64")

            paths[0].write_text(json.dumps(sbom_document()), encoding="utf-8")
            incomplete = provenance_document()
            incomplete["metadata"]["completeness"]["parameters"] = False
            paths[3].write_text(json.dumps(incomplete), encoding="utf-8")
            with self.assertRaisesRegex(runtime.ContractError, "incomplete"):
                runtime.inspect_attestations(*paths, "linux/amd64")

            paths[3].write_text(
                json.dumps(provenance_v1_document()), encoding="utf-8"
            )
            self.assertEqual(
                runtime.inspect_attestations(*paths, "linux/amd64")["provenance"],
                "buildkit-max",
            )
            v1_min = provenance_v1_document()
            v1_min["buildDefinition"].pop("internalParameters")
            paths[3].write_text(json.dumps(v1_min), encoding="utf-8")
            with self.assertRaisesRegex(runtime.ContractError, "incomplete"):
                runtime.inspect_attestations(*paths, "linux/amd64")

            paths[3].write_text(
                json.dumps(provenance_document("linux/arm64")), encoding="utf-8"
            )
            with self.assertRaisesRegex(runtime.ContractError, "builder platform"):
                runtime.inspect_attestations(*paths, "linux/amd64")

            wrong_v1_builder = provenance_v1_document(builder_platform="linux/arm64")
            paths[3].write_text(json.dumps(wrong_v1_builder), encoding="utf-8")
            with self.assertRaisesRegex(runtime.ContractError, "builder platform"):
                runtime.inspect_attestations(*paths, "linux/amd64")

    def test_verify_expectations_do_not_accept_rollback_or_digest_substitution(self):
        document = manifest_document()
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "manifest.json"
            path.write_bytes(runtime.canonical_manifest(document))
            arguments = type(
                "Arguments",
                (),
                {
                    "manifest": path,
                    "artifact_version": "2.40.0-r1",
                    "release_sequence": 102,
                    "source_revision": "1" * 40,
                    "index_digest": DIGESTS["index"],
                    "trust_key_id": "4" * 64,
                },
            )()
            with self.assertRaisesRegex(runtime.ContractError, "release_sequence"):
                runtime.command_verify(arguments)

    def test_candidate_binds_source_tag_index_lock_and_attestations(self):
        source = ROOT / "containers" / "opencodex" / "upstream.lock.json"
        document = {
            "schema": 1,
            "artifact_kind": "opencodex-runtime-image",
            "artifact_version": "2.40.0-r1",
            "release_sequence": 987654,
            "channel": "candidate",
            "source_revision": "1" * 40,
            "workflow_run_id": "987654",
            "workflow_run_attempt": 2,
            "upstream_lock_sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
            "candidate_tag": f"candidate-2.40.0-r1-{'1' * 40}",
            "image": {
                "repository": "ghcr.io/novelkr/opencodex-runtime",
                "index_digest": DIGESTS["index"],
                "platforms": [
                    {"os": "linux", "arch": "amd64", "digest": DIGESTS["amd64"]},
                    {"os": "linux", "arch": "arm64", "digest": DIGESTS["arm64"]},
                ],
            },
            "attestations": {
                "buildkit_sbom": True,
                "buildkit_provenance": "max",
                "github_provenance": True,
            },
        }
        self.assertIs(runtime.validate_candidate(document), document)
        canonical = runtime.canonical_candidate(document)
        self.assertEqual(runtime.load_json_bytes(canonical, "candidate"), document)

        for name, mutation in {
            "tag": lambda value: value.update(candidate_tag="candidate-2.40.0-r1-latest"),
            "source": lambda value: value.update(source_revision="main"),
            "channel": lambda value: value.update(channel="stable"),
            "attestation": lambda value: value["attestations"].update(github_provenance=False),
            "numeric attestation boolean": lambda value: value["attestations"].update(buildkit_sbom=1),
            "boolean schema": lambda value: value.update(schema=True),
            "platform": lambda value: value["image"]["platforms"].reverse(),
        }.items():
            with self.subTest(name=name):
                changed = copy.deepcopy(document)
                mutation(changed)
                with self.assertRaises(runtime.ContractError):
                    runtime.validate_candidate(changed)


if __name__ == "__main__":
    unittest.main()

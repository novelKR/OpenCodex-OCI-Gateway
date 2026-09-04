#!/usr/bin/env python3

import base64
import copy
import hashlib
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest import mock

from tools import opencodex_npm_provenance as provenance


VERSION = "2.40.0"
REVISION = "35ff3a462e786bd5efc394dfb1a8a5cc946e454f"
TARBALL = b"deterministic npm tarball fixture"
SHA512 = hashlib.sha512(TARBALL).hexdigest()
INTEGRITY = "sha512-" + base64.b64encode(hashlib.sha512(TARBALL).digest()).decode()


def encoded(value):
    return base64.b64encode(value).decode("ascii")


def bundle(predicate_type, statement, publish=False):
    verification = {
        "tlogEntries": [
            {
                "logIndex": "123",
                "integratedTime": "1788343655",
                "inclusionPromise": {"signedEntryTimestamp": encoded(b"set")},
                "inclusionProof": {
                    "rootHash": encoded(b"root"),
                    "hashes": [encoded(b"hash")],
                    "checkpoint": {"envelope": "rekor checkpoint"},
                },
                "canonicalizedBody": encoded(b"canonical body"),
            }
        ],
        "timestampVerificationData": {"rfc3161Timestamps": []},
    }
    if publish:
        verification["publicKey"] = {"hint": "repository signing key"}
        media_type = "application/vnd.dev.sigstore.bundle+json;version=0.2"
    else:
        verification["certificate"] = {"rawBytes": encoded(b"certificate")}
        media_type = "application/vnd.dev.sigstore.bundle.v0.3+json"
    return {
        "predicateType": predicate_type,
        "bundle": {
            "mediaType": media_type,
            "verificationMaterial": verification,
            "dsseEnvelope": {
                "payload": encoded(
                    json.dumps(statement, separators=(",", ":")).encode("utf-8")
                ),
                "payloadType": "application/vnd.in-toto+json",
                "signatures": [{"sig": encoded(b"signature"), "keyid": "fixture"}],
            },
        },
        "signedAccessSignatureUrl": "",
    }


def subject():
    return [
        {
            "name": provenance.expected_purl(VERSION),
            "digest": {"sha512": SHA512},
        }
    ]


def publish_statement():
    return {
        "_type": "https://in-toto.io/Statement/v0.1",
        "subject": subject(),
        "predicateType": provenance.PUBLISH_PREDICATE,
        "predicate": {
            "name": provenance.NPM_PACKAGE,
            "version": VERSION,
            "registry": provenance.NPM_REGISTRY,
        },
    }


def provenance_statement():
    return {
        "_type": "https://in-toto.io/Statement/v1",
        "subject": subject(),
        "predicateType": provenance.PROVENANCE_PREDICATE,
        "predicate": {
            "buildDefinition": {
                "buildType": provenance.WORKFLOW_BUILD_TYPE,
                "externalParameters": {
                    "workflow": {
                        "ref": provenance.EXPECTED_SOURCE_REF,
                        "repository": provenance.EXPECTED_REPOSITORY_URL,
                        "path": provenance.EXPECTED_WORKFLOW_PATH,
                    }
                },
                "internalParameters": {
                    "github": {
                        "event_name": "workflow_dispatch",
                        "repository_id": "1273824907",
                        "repository_owner_id": "243035832",
                    }
                },
                "resolvedDependencies": [
                    {
                        "uri": provenance.EXPECTED_RESOLVED_URI,
                        "digest": {"gitCommit": REVISION},
                    }
                ],
            },
            "runDetails": {
                "builder": {"id": provenance.EXPECTED_BUILDER},
                "metadata": {
                    "invocationId": (
                        "https://github.com/lidge-jun/opencodex/actions/runs/1/attempts/1"
                    )
                },
            },
        },
    }


def audit_receipt():
    return {
        "invalid": [],
        "missing": [],
        "verified": [
            {
                "name": provenance.NPM_PACKAGE,
                "version": VERSION,
                "location": "node_modules/@bitkyc08/opencodex",
                "registry": provenance.NPM_REGISTRY + "/",
                "attestations": {
                    "url": provenance.expected_attestation_url(VERSION),
                    "provenance": {
                        "predicateType": provenance.PROVENANCE_PREDICATE
                    },
                },
                "attestationBundles": [
                    bundle(
                        provenance.PUBLISH_PREDICATE,
                        publish_statement(),
                        publish=True,
                    ),
                    bundle(
                        provenance.PROVENANCE_PREDICATE,
                        provenance_statement(),
                    ),
                ],
            }
        ],
    }


def registry_evidence(keys=True, signatures=None):
    keyid = "SHA256:" + encoded(hashlib.sha256(b"registry-key").digest()).rstrip("=")
    metadata = {
        "name": provenance.NPM_PACKAGE,
        "version": VERSION,
        "dist": {
            "integrity": INTEGRITY,
            "tarball": (
                provenance.NPM_REGISTRY
                + f"/@bitkyc08/opencodex/-/opencodex-{VERSION}.tgz"
            ),
            "signatures": (
                signatures
                if signatures is not None
                else [{"keyid": keyid, "sig": encoded(b"registry-signature")}]
            ),
        },
    }
    keys_document = {
        "keys": (
            [{
                "expires": None,
                "keyid": keyid,
                "keytype": "ecdsa-sha2-nistp256",
                "scheme": "ecdsa-sha2-nistp256",
                "key": encoded(b"registry-key"),
            }]
            if keys
            else []
        )
    }
    return provenance.validate_registry_signature_evidence(
        metadata, keys_document, VERSION, INTEGRITY, SHA512
    )


def identity_evidence(value):
    identity_bundle = provenance.select_slsa_identity_bundle(value, VERSION)
    return provenance.SLSAIdentityEvidence(
        bundle_sha256=hashlib.sha256(
            provenance.canonical_bundle_bytes(identity_bundle)
        ).hexdigest(),
        certificate_identity=provenance.EXPECTED_CERTIFICATE_IDENTITY,
        certificate_issuer=provenance.EXPECTED_CERTIFICATE_ISSUER,
        verifier=f"sigstore@{provenance.NPM_PROVENANCE_SIGSTORE_VERSION}",
    )


class OpenCodexNPMProvenanceTests(unittest.TestCase):
    def verify(self, value):
        try:
            evidence = identity_evidence(value)
        except provenance.ContractError:
            # Tests for missing/invalid receipt structure must reach the
            # receipt classifier before independent identity evidence is
            # considered. Operational verify_live never synthesizes evidence.
            evidence = identity_evidence(audit_receipt())
        return provenance.validate_audit_receipt(
            value,
            VERSION,
            REVISION,
            INTEGRITY,
            SHA512,
            registry_evidence(),
            evidence,
        )

    def test_accepts_exact_official_npm_and_slsa_evidence(self):
        result = self.verify(audit_receipt())
        self.assertEqual(result["revision"], REVISION)
        self.assertEqual(result["sha512"], SHA512)
        self.assertEqual(result["verifier"], "npm@11.19.1")
        self.assertIs(result["registry_signature"], True)
        self.assertIs(result["slsa_provenance"], True)
        self.assertEqual(
            result["slsa_certificate_identity"],
            provenance.EXPECTED_CERTIFICATE_IDENTITY,
        )
        self.assertEqual(
            result["slsa_certificate_issuer"],
            provenance.EXPECTED_CERTIFICATE_ISSUER,
        )
        self.assertIs(result["transparency_log"], True)
        self.assertIn("sha256:ba849c60", provenance.NPM_PROVENANCE_NODE_IMAGE)

    def test_rejects_subject_or_downloaded_tarball_digest_mismatch(self):
        value = audit_receipt()
        payload = provenance_statement()
        payload["subject"][0]["digest"]["sha512"] = "0" * 128
        value["verified"][0]["attestationBundles"][1] = bundle(
            provenance.PROVENANCE_PREDICATE, payload
        )
        with self.assertRaisesRegex(provenance.ContractError, "subject"):
            self.verify(value)

        with self.assertRaisesRegex(provenance.ContractError, "downloaded npm tarball"):
            provenance.validate_audit_receipt(
                audit_receipt(),
                VERSION,
                REVISION,
                INTEGRITY,
                "1" * 128,
                registry_evidence(),
                identity_evidence(audit_receipt()),
            )

    def test_certificate_identity_evidence_is_bound_to_exact_slsa_bundle(self):
        value = audit_receipt()
        evidence = identity_evidence(value)
        statement = provenance_statement()
        statement["predicate"]["runDetails"]["metadata"]["invocationId"] = (
            "https://github.com/lidge-jun/opencodex/actions/runs/2/attempts/1"
        )
        value["verified"][0]["attestationBundles"][1] = bundle(
            provenance.PROVENANCE_PREDICATE,
            statement,
        )
        with self.assertRaisesRegex(provenance.ContractError, "certificate identity"):
            provenance.validate_audit_receipt(
                value,
                VERSION,
                REVISION,
                INTEGRITY,
                SHA512,
                registry_evidence(),
                evidence,
            )

        for bad_evidence in (
            provenance.SLSAIdentityEvidence(
                bundle_sha256=hashlib.sha256(
                    provenance.canonical_bundle_bytes(
                        provenance.select_slsa_identity_bundle(value, VERSION)
                    )
                ).hexdigest(),
                certificate_identity=(
                    "https://github.com/attacker/repository/"
                    ".github/workflows/release.yml@refs/heads/main"
                ),
                certificate_issuer=provenance.EXPECTED_CERTIFICATE_ISSUER,
                verifier="sigstore@4.1.1",
            ),
            provenance.SLSAIdentityEvidence(
                bundle_sha256="0" * 64,
                certificate_identity=provenance.EXPECTED_CERTIFICATE_IDENTITY,
                certificate_issuer=provenance.EXPECTED_CERTIFICATE_ISSUER,
                verifier="sigstore@4.1.1",
            ),
            provenance.SLSAIdentityEvidence(
                bundle_sha256=hashlib.sha256(
                    provenance.canonical_bundle_bytes(
                        provenance.select_slsa_identity_bundle(value, VERSION)
                    )
                ).hexdigest(),
                certificate_identity=provenance.EXPECTED_CERTIFICATE_IDENTITY,
                certificate_issuer="https://issuer.example.invalid",
                verifier="sigstore@4.1.1",
            ),
        ):
            with self.assertRaisesRegex(provenance.ContractError, "certificate identity"):
                provenance.validate_audit_receipt(
                    value,
                    VERSION,
                    REVISION,
                    INTEGRITY,
                    SHA512,
                    registry_evidence(),
                    bad_evidence,
                )

    def test_identity_helper_invocation_receipt_is_digest_bound_and_not_configurable(self):
        value = audit_receipt()
        identity_bundle = provenance.select_slsa_identity_bundle(value, VERSION)
        encoded = provenance.canonical_bundle_bytes(identity_bundle)
        receipt = {
            "schema": 1,
            "status": "verified",
            "bundle_sha256": hashlib.sha256(encoded).hexdigest(),
            "certificate_identity": provenance.EXPECTED_CERTIFICATE_IDENTITY,
            "certificate_issuer": provenance.EXPECTED_CERTIFICATE_ISSUER,
            "verifier": "sigstore@4.1.1",
        }
        captured = []
        with tempfile.TemporaryDirectory() as name:
            root = pathlib.Path(name)
            verifier = root / "verifier"
            verifier.mkdir()

            def run(arguments, timeout):
                captured.append((arguments, timeout))
                return subprocess.CompletedProcess(
                    arguments,
                    0,
                    stdout=json.dumps(receipt, separators=(",", ":")) + "\n",
                    stderr="",
                )

            with mock.patch.object(provenance, "_run_container", side_effect=run):
                evidence = provenance._run_slsa_identity_verifier(
                    verifier,
                    root,
                    identity_bundle,
                )
            self.assertEqual(evidence.bundle_sha256, receipt["bundle_sha256"])
            self.assertEqual(
                (root / "slsa-identity-bundle.json").read_bytes(),
                encoded,
            )

        self.assertEqual(len(captured), 1)
        arguments, timeout = captured[0]
        self.assertEqual(timeout, 180)
        self.assertEqual(arguments[-3:], [
            "node",
            "/identity-helper.cjs",
            "/input/slsa-bundle.json",
        ])
        self.assertNotIn(provenance.EXPECTED_CERTIFICATE_IDENTITY, arguments)
        helper = provenance._identity_helper_path().read_text(encoding="utf-8")
        self.assertIn(
            "^https://github\\\\.com/lidge-jun/opencodex/\\\\.github/"
            "workflows/release\\\\.yml@refs/heads/main$",
            helper,
        )
        self.assertNotIn("attacker/repository", helper)
        self.assertIn(
            "certificateIssuer: CERTIFICATE_ISSUER",
            helper,
        )
        self.assertIn(provenance.EXPECTED_CERTIFICATE_ISSUER, helper)

    def test_rejects_unapproved_repository_or_workflow(self):
        mutations = {
            "repository": lambda statement: statement["predicate"]["buildDefinition"][
                "externalParameters"
            ]["workflow"].update(repository="https://github.com/attacker/repository"),
            "workflow": lambda statement: statement["predicate"]["buildDefinition"][
                "externalParameters"
            ]["workflow"].update(path=".github/workflows/other.yml"),
            "ref": lambda statement: statement["predicate"]["buildDefinition"][
                "externalParameters"
            ]["workflow"].update(ref="refs/heads/other"),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                statement = provenance_statement()
                mutate(statement)
                value = audit_receipt()
                value["verified"][0]["attestationBundles"][1] = bundle(
                    provenance.PROVENANCE_PREDICATE, statement
                )
                with self.assertRaisesRegex(
                    provenance.ContractError, "repository or workflow"
                ):
                    self.verify(value)

    def test_rejects_resolved_commit_or_builder_mismatch(self):
        statement = provenance_statement()
        statement["predicate"]["buildDefinition"]["resolvedDependencies"][0][
            "digest"
        ]["gitCommit"] = "0" * 40
        value = audit_receipt()
        value["verified"][0]["attestationBundles"][1] = bundle(
            provenance.PROVENANCE_PREDICATE, statement
        )
        with self.assertRaisesRegex(provenance.ContractError, "resolved source commit"):
            self.verify(value)

        statement = provenance_statement()
        statement["predicate"]["runDetails"]["builder"]["id"] = "untrusted"
        value = audit_receipt()
        value["verified"][0]["attestationBundles"][1] = bundle(
            provenance.PROVENANCE_PREDICATE, statement
        )
        with self.assertRaisesRegex(provenance.ContractError, "GitHub-hosted"):
            self.verify(value)

    def test_rejects_invalid_signature_or_transparency_evidence(self):
        value = audit_receipt()
        value["invalid"] = [{"name": provenance.NPM_PACKAGE, "version": VERSION}]
        with self.assertRaisesRegex(provenance.ContractError, "invalid signature"):
            self.verify(value)

        value = audit_receipt()
        value["verified"][0]["attestationBundles"][1]["bundle"][
            "verificationMaterial"
        ]["tlogEntries"] = []
        with self.assertRaisesRegex(provenance.ContractError, "transparency-log"):
            self.verify(value)

    def test_missing_registry_or_provenance_evidence_is_awaiting(self):
        value = audit_receipt()
        value["missing"] = [
            {"name": provenance.NPM_PACKAGE, "version": VERSION}
        ]
        with self.assertRaises(provenance.AwaitingNPMProvenance):
            self.verify(value)

        value = audit_receipt()
        value["verified"][0]["attestationBundles"] = value["verified"][0][
            "attestationBundles"
        ][:1]
        with self.assertRaises(provenance.AwaitingNPMProvenance):
            self.verify(value)

    def test_attestations_without_registry_keys_are_rejected(self):
        with self.assertRaisesRegex(provenance.ContractError, "no trusted signing key"):
            registry_evidence(keys=False)

    def test_registry_accepts_multiple_signatures_from_the_same_trusted_key(self):
        keyid = "SHA256:" + encoded(
            hashlib.sha256(b"registry-key").digest()
        ).rstrip("=")
        evidence = registry_evidence(
            signatures=[
                {"keyid": keyid, "sig": encoded(b"registry-signature-one")},
                {"keyid": keyid, "sig": encoded(b"registry-signature-two")},
            ]
        )
        self.assertEqual(evidence.signature_count, 2)
        self.assertEqual(evidence.trusted_key_count, 1)

    def test_registry_rejects_malformed_repeated_key_signature(self):
        keyid = "SHA256:" + encoded(
            hashlib.sha256(b"registry-key").digest()
        ).rstrip("=")
        with self.assertRaisesRegex(provenance.ContractError, "registry signature"):
            registry_evidence(
                signatures=[
                    {"keyid": keyid, "sig": encoded(b"registry-signature-one")},
                    {"keyid": keyid, "sig": "not base64"},
                ]
            )

    def test_present_unexpected_package_or_extra_bundle_fails_immediately(self):
        value = audit_receipt()
        value["verified"][0]["name"] = "attacker-package"
        with self.assertRaisesRegex(provenance.ContractError, "package identity"):
            self.verify(value)

        value = audit_receipt()
        value["verified"][0]["attestationBundles"].append(
            copy.deepcopy(value["verified"][0]["attestationBundles"][0])
        )
        with self.assertRaisesRegex(provenance.ContractError, "bundle count"):
            self.verify(value)

    def test_rejects_metadata_registry_and_predicate_mismatch(self):
        value = audit_receipt()
        value["verified"][0]["registry"] = "https://registry.example.invalid/"
        with self.assertRaisesRegex(provenance.ContractError, "registry"):
            self.verify(value)

        value = audit_receipt()
        value["verified"][0]["attestations"]["provenance"][
            "predicateType"
        ] = "https://slsa.dev/provenance/v0.2"
        with self.assertRaisesRegex(provenance.ContractError, "predicate metadata"):
            self.verify(value)

    def test_strict_json_rejects_duplicate_keys_and_trailing_data(self):
        with self.assertRaisesRegex(provenance.ContractError, "duplicate JSON key"):
            provenance.load_json_bytes(b'{"invalid":[],"invalid":[]}', "fixture")
        with self.assertRaisesRegex(provenance.ContractError, "not valid JSON"):
            provenance.load_json_bytes(b'{"invalid":[]} trailing', "fixture")

    def test_policy_mutations_do_not_change_the_valid_fixture(self):
        original = audit_receipt()
        cloned = copy.deepcopy(original)
        self.verify(cloned)
        self.assertEqual(cloned, original)


if __name__ == "__main__":
    unittest.main()

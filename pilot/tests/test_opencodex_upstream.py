#!/usr/bin/env python3

import base64
import copy
import hashlib
import importlib.util
import io
import json
import pathlib
import tarfile
import tempfile
import unittest
from unittest import mock


ROOT = pathlib.Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "tools" / "opencodex_upstream.py"
SPEC = importlib.util.spec_from_file_location("opencodex_upstream", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
upstream = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(upstream)


REVISION = "a" * 40


def package_tarball(version="2.41.0", name=upstream.NPM_PACKAGE):
    body = json.dumps({"name": name, "version": version}, separators=(",", ":")).encode()
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        entry = tarfile.TarInfo("package/package.json")
        entry.size = len(body)
        entry.mtime = 0
        archive.addfile(entry, io.BytesIO(body))
    return stream.getvalue()


def lock(version="2.40.0", revision="b" * 40, release_id=40, image_revision=1):
    tarball = upstream.NPM_REGISTRY + upstream.expected_tarball_path(version)
    return {
        "schema": 1,
        "image_revision": image_revision,
        "repository": upstream.UPSTREAM_REPOSITORY,
        "release": {
            "id": release_id,
            "tag": f"v{version}",
            "published_at": "2026-09-02T10:08:02Z",
        },
        "version": version,
        "revision": revision,
        "npm": {
            "package": upstream.NPM_PACKAGE,
            "version": version,
            "integrity": "sha512-" + base64.b64encode(b"x" * 64).decode(),
            "tarball": tarball,
        },
    }


class FakeClient:
    def __init__(self, version="2.41.0"):
        self.version = version
        self.release_id = 41
        self.tarball = package_tarball(version)
        self.integrity = "sha512-" + base64.b64encode(
            hashlib.sha512(self.tarball).digest()
        ).decode()
        self.pages = [[self.listed_release()]]
        self.release = self.full_release()
        self.tag_object = {"object": {"type": "commit", "sha": REVISION}}
        self.commit = {"sha": REVISION}
        package = json.dumps(
            {"name": upstream.NPM_PACKAGE, "version": version}, separators=(",", ":")
        ).encode()
        self.contents = {
            "encoding": "base64",
            "content": base64.b64encode(package).decode(),
            "size": len(package),
            "sha": "c" * 40,
        }
        self.npm = {
            "name": upstream.NPM_PACKAGE,
            "versions": {
                version: {
                    "name": upstream.NPM_PACKAGE,
                    "version": version,
                    "gitHead": REVISION,
                    "dist": {
                        "integrity": self.integrity,
                        "tarball": upstream.NPM_REGISTRY
                        + upstream.expected_tarball_path(version),
                        "attestations": {
                            "url": (
                                "https://registry.npmjs.org/-/npm/v1/attestations/"
                                f"@bitkyc08%2fopencodex@{version}"
                            ),
                            "provenance": {
                                "predicateType": "https://slsa.dev/provenance/v1"
                            },
                        },
                    },
                }
            },
        }
        self.github_calls = []
        self.provenance_error = None

    def listed_release(self, **overrides):
        value = {
            "id": self.release_id,
            "tag_name": f"v{self.version}",
            "draft": False,
            "prerelease": False,
        }
        value.update(overrides)
        return value

    def full_release(self, **overrides):
        value = {
            **self.listed_release(),
            "immutable": True,
            "published_at": "2026-09-03T10:08:02Z",
            "target_commitish": REVISION,
        }
        value.update(overrides)
        return value

    def github_json(self, path):
        self.github_calls.append(path)
        if "/releases?" in path:
            page = int(path.rsplit("page=", 1)[1])
            return copy.deepcopy(self.pages[page - 1] if page <= len(self.pages) else [])
        if path.endswith(f"/releases/{self.release_id}"):
            return copy.deepcopy(self.release)
        if "/git/ref/tags/" in path:
            return copy.deepcopy(self.tag_object)
        if "/commits/" in path:
            return copy.deepcopy(self.commit)
        if "/contents/package.json?" in path:
            return copy.deepcopy(self.contents)
        raise AssertionError(path)

    def npm_version_json(self, package, version):
        self.asserted_package = package
        self.asserted_version = version
        versions = self.npm["versions"]
        if version not in versions:
            raise upstream.AwaitingNPM("npm metadata disappeared")
        return copy.deepcopy(versions[version])

    def tarball_bytes(self, url):
        self.asserted_tarball = url
        return self.tarball

    def verify_npm_provenance(self, version, revision, integrity, tarball):
        if self.provenance_error is not None:
            raise self.provenance_error
        self.asserted_provenance = (version, revision, integrity, tarball)
        return {"status": "verified"}


class OpenCodexUpstreamTests(unittest.TestCase):
    def test_selects_highest_strict_stable_release_from_unsorted_rows(self):
        client = FakeClient()
        client.pages = [[
            client.listed_release(id=1, tag_name="v2.39.0"),
            client.listed_release(id=2, tag_name="v99.0.0-rc.1", prerelease=True),
            client.listed_release(id=3, tag_name="release-99"),
            client.listed_release(id=4, tag_name="v2.50.0", draft=True),
            client.listed_release(),
        ]]
        status, candidate = upstream.detect(client, lock())
        self.assertEqual(status, "update-available")
        self.assertEqual(candidate["version"], "2.41.0")
        self.assertEqual(candidate["image_revision"], 1)
        self.assertEqual(client.asserted_version, "2.41.0")
        self.assertTrue(any(path.endswith("/releases/41") for path in client.github_calls))

    def test_release_scan_is_bounded_to_five_complete_pages(self):
        client = FakeClient()
        filler = [
            {"id": index + 1000, "tag_name": f"invalid-{index}", "draft": False, "prerelease": False}
            for index in range(100)
        ]
        client.pages = [copy.deepcopy(filler) for _ in range(5)]
        with self.assertRaisesRegex(upstream.ContractError, "exceeded 5 pages"):
            upstream.detect(client, lock())

    def test_detector_reads_the_second_page_when_first_is_full(self):
        client = FakeClient()
        filler = [
            {"id": index + 1000, "tag_name": f"invalid-{index}", "draft": False, "prerelease": False}
            for index in range(100)
        ]
        client.pages = [filler, [client.listed_release()]]
        status, candidate = upstream.detect(client, lock())
        self.assertEqual((status, candidate["version"]), ("update-available", "2.41.0"))

    def test_rejects_mutable_annotated_or_mismatched_release_identity(self):
        mutations = {
            "immutable": lambda client: client.release.update(immutable=False),
            "annotated": lambda client: client.tag_object["object"].update(type="tag"),
            "target": lambda client: client.commit.update(sha="d" * 40),
            "reread": lambda client: client.release.update(tag_name="v2.42.0"),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                client = FakeClient()
                mutate(client)
                with self.assertRaises(upstream.ContractError):
                    upstream.detect(client, lock())

    def test_rejects_repository_package_identity_mismatch(self):
        client = FakeClient()
        package = json.dumps({"name": "other", "version": client.version}).encode()
        client.contents["content"] = base64.b64encode(package).decode()
        client.contents["size"] = len(package)
        with self.assertRaisesRegex(upstream.ContractError, "package.json identity"):
            upstream.detect(client, lock())

    def test_accepts_github_content_api_base64_line_wrapping_only(self):
        client = FakeClient()
        content = client.contents["content"]
        client.contents["content"] = "\n".join(
            content[index : index + 8] for index in range(0, len(content), 8)
        ) + "\n"
        status, candidate = upstream.detect(client, lock())
        self.assertEqual(status, "update-available")
        self.assertEqual(candidate["version"], client.version)

        client = FakeClient()
        client.contents["content"] += " "
        with self.assertRaisesRegex(upstream.ContractError, "content is invalid"):
            upstream.detect(client, lock())

    def test_absent_npm_version_is_a_quiet_propagation_delay(self):
        client = FakeClient()
        client.npm["versions"] = {}
        status, candidate = upstream.detect(client, lock())
        self.assertEqual((status, candidate), ("awaiting-npm", None))

        class MissingPackageClient(FakeClient):
            def npm_version_json(self, package, version):
                raise upstream.AwaitingNPM("not propagated")

        status, candidate = upstream.detect(MissingPackageClient(), lock())
        self.assertEqual((status, candidate), ("awaiting-npm", None))

    def test_absent_new_tarball_is_a_quiet_propagation_delay(self):
        class MissingTarballClient(FakeClient):
            def tarball_bytes(self, url):
                raise upstream.AwaitingNPM("tarball not propagated")

        status, candidate = upstream.detect(MissingTarballClient(), lock())
        self.assertEqual((status, candidate), ("awaiting-npm", None))

        network = upstream.NetworkClient(
            upstream.GITHUB_API,
            upstream.NPM_REGISTRY,
        )
        missing = upstream.urllib.error.HTTPError(
            "https://registry.npmjs.org/package.tgz", 404, "missing", {}, None
        )
        with (
            mock.patch.object(network, "_read", side_effect=missing),
            self.assertRaises(upstream.AwaitingNPM),
        ):
            network.tarball_bytes("https://registry.npmjs.org/package.tgz")

    def test_same_version_missing_tarball_is_not_misclassified_as_propagation(self):
        class MissingTarballClient(FakeClient):
            def tarball_bytes(self, url):
                raise upstream.AwaitingNPM("tarball disappeared")

        client = MissingTarballClient(version="2.40.0")
        current = lock(
            version="2.40.0", revision=REVISION, release_id=client.release_id
        )
        current["release"]["published_at"] = client.release["published_at"]
        current["npm"]["integrity"] = client.integrity
        with self.assertRaisesRegex(upstream.ContractError, "tarball disappeared"):
            upstream.detect(client, current)

    def test_same_version_missing_npm_is_not_misclassified_as_propagation(self):
        client = FakeClient(version="2.40.0")
        current = lock(
            version="2.40.0", revision=REVISION, release_id=client.release_id
        )
        current["release"]["published_at"] = client.release["published_at"]
        current["npm"]["integrity"] = client.integrity
        client.npm["versions"] = {}
        with self.assertRaisesRegex(upstream.ContractError, "metadata disappeared"):
            upstream.detect(client, current)

    def test_present_but_mismatched_npm_metadata_fails(self):
        client = FakeClient()
        client.npm["versions"][client.version]["name"] = "other"
        with self.assertRaisesRegex(upstream.ContractError, "npm version metadata identity"):
            upstream.detect(client, lock())

    def test_npm_git_head_and_provenance_metadata_are_commit_bound(self):
        client = FakeClient()
        client.npm["versions"][client.version]["gitHead"] = "d" * 40
        with self.assertRaisesRegex(upstream.ContractError, "gitHead"):
            upstream.detect(client, lock())

        client = FakeClient()
        client.npm["versions"][client.version]["dist"]["attestations"][
            "provenance"
        ]["predicateType"] = "https://slsa.dev/provenance/v0.2"
        with self.assertRaisesRegex(upstream.ContractError, "predicate metadata"):
            upstream.detect(client, lock())

    def test_absent_new_provenance_is_a_quiet_propagation_delay(self):
        client = FakeClient()
        del client.npm["versions"][client.version]["dist"]["attestations"]
        self.assertEqual(upstream.detect(client, lock()), ("awaiting-npm", None))

        client = FakeClient()
        client.provenance_error = upstream.AwaitingNPM("provenance bundle pending")
        self.assertEqual(upstream.detect(client, lock()), ("awaiting-npm", None))

    def test_same_version_missing_provenance_fails_closed(self):
        client = FakeClient(version="2.40.0")
        current = lock(
            version="2.40.0", revision=REVISION, release_id=client.release_id
        )
        current["release"]["published_at"] = client.release["published_at"]
        current["npm"]["integrity"] = client.integrity
        del client.npm["versions"][client.version]["dist"]["attestations"]
        with self.assertRaisesRegex(upstream.ContractError, "same-version npm provenance"):
            upstream.detect(client, current)

    def test_tarball_sri_and_internal_package_are_verified(self):
        client = FakeClient()
        client.tarball += b"tampered"
        with self.assertRaisesRegex(upstream.ContractError, "SRI"):
            upstream.detect(client, lock())

        client = FakeClient()
        client.tarball = package_tarball(client.version, name="other")
        client.integrity = "sha512-" + base64.b64encode(
            hashlib.sha512(client.tarball).digest()
        ).decode()
        client.npm["versions"][client.version]["dist"]["integrity"] = client.integrity
        with self.assertRaisesRegex(upstream.ContractError, "tarball package identity"):
            upstream.detect(client, lock())

        client = FakeClient()
        client.provenance_error = upstream.ContractError(
            "npm provenance subject does not match the downloaded tarball"
        )
        with self.assertRaisesRegex(upstream.ContractError, "provenance subject"):
            upstream.detect(client, lock())

    def test_same_version_is_current_only_for_identical_immutable_identity(self):
        client = FakeClient(version="2.40.0")
        current = lock(
            version="2.40.0", revision=REVISION, release_id=client.release_id, image_revision=7
        )
        current["release"]["published_at"] = client.release["published_at"]
        current["npm"]["integrity"] = client.integrity
        status, candidate = upstream.detect(client, current)
        self.assertEqual((status, candidate), ("up-to-date", None))

        mutations = {
            "release ID": lambda value: value["release"].update(id=value["release"]["id"] + 1),
            "published timestamp": lambda value: value["release"].update(published_at="2026-09-03T10:08:03Z"),
            "commit": lambda value: value.update(revision="d" * 40),
            "integrity": lambda value: value["npm"].update(
                integrity="sha512-" + base64.b64encode(bytes(64)).decode()
            ),
        }
        for name, mutation in mutations.items():
            with self.subTest(name=name):
                changed = copy.deepcopy(current)
                mutation(changed)
                with self.assertRaisesRegex(upstream.ContractError, "same-version"):
                    upstream.detect(client, changed)

    def test_detected_lower_version_is_rejected(self):
        client = FakeClient(version="2.39.0")
        client.npm["versions"] = {}
        with self.assertRaisesRegex(upstream.ContractError, "downgrade"):
            upstream.detect(client, lock(version="2.40.0"))

    def test_lock_rejects_unknown_duplicate_and_noncanonical_tarball(self):
        value = lock()
        value["extra"] = True
        with self.assertRaisesRegex(upstream.ContractError, "unsupported fields"):
            upstream.validate_lock(value)
        value = lock()
        value["schema"] = True
        with self.assertRaisesRegex(upstream.ContractError, "schema"):
            upstream.validate_lock(value)
        duplicate = b'{"schema":1,"schema":1}'
        with self.assertRaisesRegex(upstream.ContractError, "duplicate JSON key"):
            upstream.load_json_bytes(duplicate, "fixture")
        for constant in (b"NaN", b"Infinity", b"-Infinity", b"1e999"):
            with self.subTest(constant=constant), self.assertRaisesRegex(
                upstream.ContractError, "non-finite JSON number"
            ):
                upstream.load_json_bytes(b'{"value":' + constant + b"}", "fixture")
        value = lock()
        value["npm"]["tarball"] = "https://example.test/package.tgz"
        with self.assertRaisesRegex(upstream.ContractError, "canonical package URL"):
            upstream.validate_lock(value)

    def test_versions_and_persisted_identifiers_fit_consumer_integer_widths(self):
        self.assertEqual(
            upstream.version_tuple(f"{upstream.UINT32_MAX}.0.0"),
            (upstream.UINT32_MAX, 0, 0),
        )
        with self.assertRaisesRegex(upstream.ContractError, "UInt32"):
            upstream.version_tuple(f"{upstream.UINT32_MAX + 1}.0.0")
        with self.assertRaisesRegex(upstream.ContractError, "UInt32"):
            upstream.version_tuple(f"{'9' * 5000}.0.0")

        for field in ("image_revision", "release_id"):
            value = lock()
            if field == "image_revision":
                value[field] = upstream.UINT64_MAX + 1
            else:
                value["release"]["id"] = upstream.INT64_MAX + 1
            with self.subTest(field=field), self.assertRaises(upstream.ContractError):
                upstream.validate_lock(value)

    def test_apply_allows_only_next_packaging_revision_and_is_deterministic(self):
        source = ROOT / "containers" / "opencodex"
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            for name in ("upstream.lock.json", "package.json", "bun.lock", "UPSTREAM_NOTICES.md"):
                (root / name).write_bytes((source / name).read_bytes())
            current = upstream.validate_lock(
                upstream.load_json_file(root / "upstream.lock.json", "lock")
            )
            candidate = copy.deepcopy(current)
            candidate["image_revision"] += 1
            candidate_path = root / "candidate.json"
            candidate_path.write_bytes(upstream.canonical_lock(candidate))
            upstream.apply_candidate(
                root / "upstream.lock.json",
                candidate_path,
                root / "package.json",
                root / "UPSTREAM_NOTICES.md",
            )
            self.assertEqual(upstream.verify_tree(root)["image_revision"], 2)
            self.assertEqual((root / "upstream.lock.json").read_bytes(), upstream.canonical_lock(candidate))
            arguments = type(
                "Arguments",
                (),
                {
                    "lock": root / "upstream.lock.json",
                    "candidate": root / "candidate-next.json",
                    "package_json": root / "package.json",
                    "notices": root / "UPSTREAM_NOTICES.md",
                },
            )()
            candidate_next = copy.deepcopy(candidate)
            candidate_next["image_revision"] += 1
            arguments.candidate.write_bytes(upstream.canonical_lock(candidate_next))
            with mock.patch("builtins.print") as output:
                upstream.command_apply(arguments)
            receipt = json.loads(output.call_args.args[0])
            self.assertIs(receipt["requires_bun_lock_refresh"], True)
            with self.assertRaisesRegex(upstream.ContractError, "neither a newer release"):
                upstream.apply_candidate(
                    root / "upstream.lock.json",
                    candidate_path,
                    root / "package.json",
                    root / "UPSTREAM_NOTICES.md",
                )

    def test_detect_command_atomically_writes_candidate_and_machine_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            lock_path = root / "upstream.lock.json"
            candidate_path = root / "candidate.json"
            lock_path.write_bytes(upstream.canonical_lock(lock()))
            arguments = type(
                "Arguments",
                (),
                {
                    "lock": lock_path,
                    "candidate_output": candidate_path,
                    "github_api": upstream.GITHUB_API,
                    "npm_registry": upstream.NPM_REGISTRY,
                    "github_token_env": "",
                },
            )()
            client = FakeClient()
            with mock.patch.object(upstream, "NetworkClient", return_value=client), mock.patch(
                "builtins.print"
            ) as output:
                self.assertEqual(upstream.command_detect(arguments), 0)
            receipt = json.loads(output.call_args.args[0])
            self.assertEqual(
                receipt,
                {
                    "schema": 1,
                    "status": "update-available",
                    "current_version": "2.40.0",
                    "version": "2.41.0",
                    "image_revision": 1,
                    "release_id": 41,
                    "revision": REVISION,
                },
            )
            candidate = upstream.validate_lock(
                upstream.load_json_file(candidate_path, "candidate")
            )
            self.assertEqual(candidate_path.read_bytes(), upstream.canonical_lock(candidate))
            self.assertFalse(any(root.glob(".candidate.json.*")))


if __name__ == "__main__":
    unittest.main()

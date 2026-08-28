#!/usr/bin/env python3

import pathlib
import subprocess
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
HYGIENE = ROOT / "tools" / "public_hygiene.py"


class PublicHygieneTests(unittest.TestCase):
    def run_hygiene(self, root: pathlib.Path, *arguments: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["python3", str(HYGIENE), str(root), *arguments],
            check=False,
            capture_output=True,
            text=True,
        )

    def initialize(self, root: pathlib.Path) -> None:
        subprocess.run(["git", "init", "-b", "main", str(root)], check=True, capture_output=True)
        subprocess.run(
            ["git", "-C", str(root), "config", "user.name", "Public Test"],
            check=True,
        )
        subprocess.run(
            ["git", "-C", str(root), "config", "user.email", "public-test@example.invalid"],
            check=True,
        )

    def commit(self, root: pathlib.Path, message: str) -> None:
        subprocess.run(["git", "-C", str(root), "add", "."], check=True)
        subprocess.run(
            ["git", "-C", str(root), "commit", "-m", message],
            check=True,
            capture_output=True,
        )

    def test_initial_history_allows_only_root_git_metadata(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "README.md").write_text("# Safe public tree\n", encoding="utf-8")
            self.initialize(root)
            self.commit(root, "Initial public Core")

            result = self.run_hygiene(root, "--initial-history")
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("public_hygiene=ok", result.stdout)

            plain = self.run_hygiene(root)
            self.assertNotEqual(plain.returncode, 0)
            self.assertIn("forbidden path", plain.stderr)

    def test_initial_history_rejects_a_second_commit(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            readme = root / "README.md"
            readme.write_text("# Safe public tree\n", encoding="utf-8")
            self.initialize(root)
            self.commit(root, "Initial public Core")
            readme.write_text("# Safe public tree\n\nsecond\n", encoding="utf-8")
            self.commit(root, "Second commit")

            result = self.run_hygiene(root, "--initial-history")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("exactly one commit", result.stderr)

    def test_release_scripts_reject_removed_apple_distribution_contracts(self) -> None:
        for token, expected in (
            ("--apple-team-id ABCDE12345", "removed Apple signing option"),
            ("xcrun notarytool submit app.zip", "notarization command"),
            ('printf \'{\"team_id\":\"ABCDE12345\"}\\n\'', "removed manifest Team ID"),
        ):
            with self.subTest(token=token), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                script = root / "client" / "relay" / "scripts" / "build-release.sh"
                script.parent.mkdir(parents=True)
                script.write_text("#!/bin/sh\n" + token + "\n", encoding="utf-8")

                result = self.run_hygiene(root)

                self.assertNotEqual(result.returncode, 0)
                self.assertIn(expected, result.stderr)

    def test_secret_bearing_text_extensions_are_scanned(self) -> None:
        private_key = (
            "-----BEGIN " + "PRIVATE KEY-----\n"
            "not-a-real-private-key\n"
            "-----END PRIVATE KEY-----\n"
        )
        for name in (
            "release.pem",
            "release.key",
            ".env",
            ".env.production",
            "credentials.env.backup",
            "RELEASE.PEM",
            "release.pem~",
            "#release.key#",
            ".env.production~",
        ):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                (root / name).write_text(private_key, encoding="utf-8")

                result = self.run_hygiene(root)

                self.assertNotEqual(result.returncode, 0)
                self.assertIn("private key material", result.stderr)

    def test_safe_public_pem_and_environment_example_are_allowed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "release-ed25519.pub.pem").write_text(
                "-----BEGIN PUBLIC KEY-----\nnot-a-real-public-key\n"
                "-----END PUBLIC KEY-----\n",
                encoding="utf-8",
            )
            (root / ".env.example").write_text(
                "API_TOKEN=REPLACE_WITH_TOKEN\n", encoding="utf-8"
            )

            result = self.run_hygiene(root)

            self.assertEqual(result.returncode, 0, result.stderr)

    def test_secret_bearing_text_extension_must_be_utf8(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "release.key").write_bytes(b"\xff\xfe\x00")

            result = self.run_hygiene(root)

            self.assertNotEqual(result.returncode, 0)
            self.assertIn("text-like file is not UTF-8", result.stderr)


if __name__ == "__main__":
    unittest.main()

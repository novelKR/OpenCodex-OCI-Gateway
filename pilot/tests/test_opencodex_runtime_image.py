#!/usr/bin/env python3

import base64
import importlib.util
import json
import pathlib
import socket
import struct
import tempfile
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "tools" / "opencodex_runtime_image_test.py"
SPEC = importlib.util.spec_from_file_location("opencodex_runtime_image_test", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
runtime_image = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runtime_image)


class RuntimeImageContractTests(unittest.TestCase):
    def test_version_output_is_exact(self):
        runtime_image.validate_version_output("opencodex 2.40.0\n", "2.40.0")
        for output in (
            "2.40.0\n",
            "opencodex 2.40.0 dirty\n",
            "warning 1.0.0\nopencodex 2.40.0\n",
            "opencodex 2.40.0",
        ):
            with self.subTest(output=output), self.assertRaises(
                runtime_image.ContractError
            ):
                runtime_image.validate_version_output(output, "2.40.0")

    def test_generated_tokens_are_distinct_sized_base64url_values(self):
        left = runtime_image.exact_token()
        right = runtime_image.exact_token()
        self.assertNotEqual(left, right)
        for token in (left, right):
            self.assertEqual(len(token), 43)
            decoded = base64.urlsafe_b64decode(token + "=")
            self.assertEqual(len(decoded), 32)
            self.assertEqual(
                base64.urlsafe_b64encode(decoded).decode("ascii").rstrip("="), token
            )

    def test_cancellation_status_requires_one_observed_upstream_abort(self):
        statuses = iter(
            (
                '{"schema":1,"started":0,"cancelled":0,"completed":0}',
                '{"schema":1,"started":1,"cancelled":0,"completed":0}',
                '{"schema":1,"started":1,"cancelled":1,"completed":0}',
            )
        )
        runtime_image.wait_cancellation_observed(lambda: next(statuses))
        for invalid in (
            '{"schema":true,"started":1,"cancelled":1,"completed":0}',
            '{"schema":1,"started":1,"cancelled":0,"completed":1}',
            '{"schema":1,"started":1,"cancelled":true,"completed":0}',
            '{"schema":1,"started":1,"cancelled":1,"completed":0,"extra":0}',
            '{"schema":1,"started":1,"started":1,"cancelled":1,"completed":0}',
        ):
            with self.subTest(invalid=invalid), self.assertRaises(
                runtime_image.ContractError
            ):
                runtime_image.wait_cancellation_observed(lambda: invalid)

    def test_bootstrap_server_serves_one_canonical_frame_then_removes_socket(self):
        api_token = "A" * 43
        admin_token = "B" * 43
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "bootstrap.sock"
            server = runtime_image.BootstrapServer(path, api_token, admin_token)
            server.start()
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                client.connect(str(path))
                length = struct.unpack(">I", runtime_image.receive_exact(client, 4))[0]
                payload = runtime_image.receive_exact(client, length)
                self.assertEqual(
                    payload,
                    json.dumps(
                        {
                            "schema": 1,
                            "api_auth_token": api_token,
                            "admin_auth_token": admin_token,
                        },
                        separators=(",", ":"),
                    ).encode("utf-8"),
                )
                acknowledgement = json.dumps(
                    {"schema": 1, "accepted": True}, separators=(",", ":")
                ).encode("utf-8")
                client.sendall(struct.pack(">I", len(acknowledgement)) + acknowledgement)
            server.wait()
            self.assertFalse(path.exists())
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as second:
                with self.assertRaises(OSError):
                    second.connect(str(path))

    def test_bootstrap_server_rejects_early_disconnect(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "bootstrap.sock"
            server = runtime_image.BootstrapServer(path, "A" * 43, "B" * 43)
            server.start()
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                client.connect(str(path))
                runtime_image.receive_exact(client, 4)
            with self.assertRaisesRegex(runtime_image.ContractError, "exchange failed"):
                server.wait()


if __name__ == "__main__":
    unittest.main()

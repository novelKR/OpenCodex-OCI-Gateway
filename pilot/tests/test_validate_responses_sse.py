#!/usr/bin/env python3

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


VALIDATOR = Path(__file__).resolve().parents[1] / "scripts" / "validate-responses-sse.py"


def frame(event_type: str, payload: dict) -> str:
    return f"event: {event_type}\ndata: {json.dumps(payload)}\n\n"


class ResponsesSseValidatorTests(unittest.TestCase):
    def run_validator(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(VALIDATOR), *args],
            check=False,
            capture_output=True,
            text=True,
        )

    def write_capture(self, headers: str, body: str) -> tuple[tempfile.TemporaryDirectory, Path, Path]:
        temporary = tempfile.TemporaryDirectory()
        root = Path(temporary.name)
        headers_path = root / "headers"
        body_path = root / "body"
        headers_path.write_text(headers, encoding="iso-8859-1")
        body_path.write_text(body, encoding="utf-8")
        return temporary, headers_path, body_path

    def valid_body(self) -> str:
        return "".join(
            [
                frame("response.created", {"type": "response.created"}),
                frame(
                    "response.output_text.delta",
                    {"type": "response.output_text.delta", "delta": "OK"},
                ),
                frame(
                    "response.completed",
                    {"type": "response.completed", "response": {"status": "completed"}},
                ),
            ]
        )

    def test_valid_complete_stream(self) -> None:
        temporary, headers, body = self.write_capture(
            "HTTP/2 200\r\nContent-Type: text/event-stream; charset=utf-8\r\n\r\n",
            self.valid_body(),
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("validate", str(headers), str(body))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("terminal=response.completed", result.stdout)

    def test_partial_single_newline_is_not_dispatched(self) -> None:
        temporary, _, body = self.write_capture(
            "",
            'data: {"type":"response.created"}\n',
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("has-event", str(body), "response.created")
        self.assertEqual(result.returncode, 1, result.stderr)

    def test_rejects_non_sse_content_type(self) -> None:
        temporary, headers, body = self.write_capture(
            "HTTP/2 200\r\nContent-Type: application/json\r\n\r\n",
            self.valid_body(),
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("validate", str(headers), str(body))
        self.assertEqual(result.returncode, 2)
        self.assertIn("unexpected Content-Type", result.stderr)

    def test_rejects_failed_event(self) -> None:
        body_text = frame("response.created", {"type": "response.created"})
        body_text += frame("response.failed", {"type": "response.failed"})
        temporary, headers, body = self.write_capture(
            "Content-Type: text/event-stream\r\n\r\n",
            body_text,
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("validate", str(headers), str(body))
        self.assertEqual(result.returncode, 2)
        self.assertIn("terminal error event", result.stderr)

    def test_rejects_event_after_completed(self) -> None:
        body_text = self.valid_body()
        body_text += frame("response.output_text.done", {"type": "response.output_text.done"})
        temporary, headers, body = self.write_capture(
            "Content-Type: text/event-stream\r\n\r\n",
            body_text,
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("validate", str(headers), str(body))
        self.assertEqual(result.returncode, 2)
        self.assertIn("single final terminal event", result.stderr)

    def test_rejects_chat_completions_done_sentinel(self) -> None:
        body_text = self.valid_body() + "data: [DONE]\n\n"
        temporary, headers, body = self.write_capture(
            "Content-Type: text/event-stream\r\n\r\n",
            body_text,
        )
        self.addCleanup(temporary.cleanup)
        result = self.run_validator("validate", str(headers), str(body))
        self.assertEqual(result.returncode, 2)
        self.assertIn("non-JSON [DONE]", result.stderr)


if __name__ == "__main__":
    unittest.main()

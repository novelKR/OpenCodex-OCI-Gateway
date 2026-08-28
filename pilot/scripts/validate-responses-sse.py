#!/usr/bin/env python3
"""Validate or inspect an OpenAI Responses API SSE capture without printing content."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


class ValidationError(Exception):
    pass


def parse_frames(path: Path, *, allow_partial: bool) -> list[tuple[str, dict[str, Any]]]:
    raw = path.read_bytes()
    try:
        text = raw.decode("utf-8", errors="ignore" if allow_partial else "strict")
    except UnicodeDecodeError as exc:
        raise ValidationError(f"SSE body is not valid UTF-8: {exc}") from exc

    text = text.replace("\r\n", "\n").replace("\r", "\n")
    frames: list[tuple[str, dict[str, Any]]] = []
    event_name = ""
    data_lines: list[str] = []

    def dispatch() -> None:
        nonlocal event_name, data_lines
        if not data_lines:
            event_name = ""
            return
        data = "\n".join(data_lines)
        event_name_local = event_name
        event_name = ""
        data_lines = []
        if data == "[DONE]":
            raise ValidationError("Responses SSE contained a non-JSON [DONE] frame")
        try:
            payload = json.loads(data)
        except json.JSONDecodeError as exc:
            raise ValidationError(f"SSE data frame is not JSON: {exc}") from exc
        if not isinstance(payload, dict):
            raise ValidationError("SSE data frame is not a JSON object")
        payload_type = payload.get("type")
        if payload_type is not None and not isinstance(payload_type, str):
            raise ValidationError("SSE payload type is not a string")
        if event_name_local and payload_type and event_name_local != payload_type:
            raise ValidationError(
                f"SSE event name {event_name_local!r} does not match payload type {payload_type!r}"
            )
        frame_type = payload_type or event_name_local
        if not frame_type:
            raise ValidationError("SSE frame has no event type")
        frames.append((frame_type, payload))

    for raw_line in text.splitlines(keepends=True):
        line_complete = raw_line.endswith("\n")
        line = raw_line[:-1] if line_complete else raw_line
        if not line_complete and allow_partial:
            break
        if line == "":
            dispatch()
            continue
        if line.startswith(":"):
            continue
        field, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        if field == "event":
            event_name = value
        elif field == "data":
            data_lines.append(value)

    if event_name or data_lines:
        if allow_partial:
            return frames
        raise ValidationError("SSE stream ended with an incomplete frame")
    return frames


def last_content_type(path: Path) -> str:
    values = []
    for line in path.read_text(encoding="iso-8859-1").splitlines():
        name, separator, value = line.partition(":")
        if separator and name.lower() == "content-type":
            values.append(value.strip())
    if not values:
        raise ValidationError("response headers contain no Content-Type")
    return values[-1]


def validate_complete(headers_path: Path, body_path: Path) -> None:
    content_type = last_content_type(headers_path)
    if content_type.split(";", 1)[0].strip().lower() != "text/event-stream":
        raise ValidationError(f"unexpected Content-Type: {content_type}")

    frames = parse_frames(body_path, allow_partial=False)
    if not frames:
        raise ValidationError("SSE stream contained no events")
    types = [frame_type for frame_type, _ in frames]
    if types[0] != "response.created":
        raise ValidationError(f"first SSE event was {types[0]!r}, not response.created")

    forbidden = {"response.failed", "response.incomplete", "error"}
    rejected = [
        frame_type
        for frame_type in types
        if frame_type in forbidden or frame_type.endswith(".error")
    ]
    if rejected:
        raise ValidationError(f"SSE stream contained terminal error event: {rejected[0]}")

    deltas = [
        payload.get("delta")
        for frame_type, payload in frames
        if frame_type == "response.output_text.delta"
    ]
    if not any(isinstance(delta, str) and delta for delta in deltas):
        raise ValidationError("SSE stream contained no non-empty output text delta")
    if types.count("response.completed") != 1 or types[-1] != "response.completed":
        raise ValidationError("response.completed was not the single final terminal event")

    completed_payload = frames[-1][1]
    response = completed_payload.get("response")
    if isinstance(response, dict) and response.get("status") not in (None, "completed"):
        raise ValidationError("response.completed payload did not report completed status")
    print(f"events={len(frames)} text_deltas={len(deltas)} terminal=response.completed")


def main() -> int:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    event_parser = subparsers.add_parser("has-event")
    event_parser.add_argument("body", type=Path)
    event_parser.add_argument("event")

    validate_parser = subparsers.add_parser("validate")
    validate_parser.add_argument("headers", type=Path)
    validate_parser.add_argument("body", type=Path)

    args = parser.parse_args()
    try:
        if args.command == "has-event":
            frames = parse_frames(args.body, allow_partial=True)
            return 0 if any(frame_type == args.event for frame_type, _ in frames) else 1
        validate_complete(args.headers, args.body)
        return 0
    except (OSError, ValidationError) as exc:
        print(f"SSE validation error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

"""Deterministic presentation state for the interactive operator TUI."""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
from dataclasses import dataclass

from .codex_activity import (
    CodexActivityEvent,
    CodexActivityKind,
    CodexActivityRole,
)


DEFAULT_DISPLAY_BYTES = 64 * 1024
DEFAULT_SCROLLBACK_BYTES = 2 * 1024 * 1024
DEFAULT_SCROLLBACK_ENTRIES = 800


@dataclass(frozen=True, slots=True)
class ActivityDisplayEntry:
    """One logical, safely renderable item in the TUI scrollback."""

    key: str
    label: str
    category: str
    body: str

    @property
    def size_bytes(self) -> int:
        return len(self.label.encode("utf-8")) + len(self.body.encode("utf-8"))


@dataclass(slots=True)
class ActivityFollowState:
    """Small testable model for live-follow and unread activity state."""

    following: bool = True
    new_events: int = 0
    scroll_y: float = 0.0

    def scrolled(self, scroll_y: float) -> None:
        self.following = False
        self.scroll_y = scroll_y

    def arrived(self, count: int = 1) -> None:
        if not self.following:
            self.new_events += count

    def return_to_live(self) -> None:
        self.following = True
        self.new_events = 0


class OperatorActivityModel:
    """Coalesces semantic activity into bounded, terminal-safe display items."""

    def __init__(
        self,
        *,
        max_entries: int = DEFAULT_SCROLLBACK_ENTRIES,
        max_bytes: int = DEFAULT_SCROLLBACK_BYTES,
        display_bytes: int = DEFAULT_DISPLAY_BYTES,
    ) -> None:
        if max_entries < 2 or max_bytes < 1 or display_bytes < 1:
            raise ValueError("TUI display bounds must be positive")
        self._max_entries = max_entries
        self._max_bytes = max_bytes
        self._display_bytes = display_bytes
        self._entries: list[ActivityDisplayEntry] = []
        self._positions: dict[str, int] = {}
        self._message_text: dict[str, str] = {}
        self._reasoning_text: dict[str, dict[int, str]] = {}
        self._tool_data: dict[str, dict[str, object]] = {}
        self._build_number = 0
        self._active_build_key: str | None = None
        self._build_phases: dict[str, list[str]] = {}
        self._operator_number = 0
        self._discarded = False
        self._latest_reviewer_message: tuple[int, str] | None = None

    @property
    def entries(self) -> tuple[ActivityDisplayEntry, ...]:
        return tuple(self._entries)

    def append_operator_output(self, text: str) -> None:
        self._operator_number += 1
        self._upsert(
            f"operator:{self._operator_number}",
            "Operator",
            "operator",
            safe_display_text(text, self._display_bytes),
        )

    def consume(self, event: CodexActivityEvent) -> None:
        kind = event.kind
        if kind in {
            CodexActivityKind.AGENT_TEXT_DELTA,
            CodexActivityKind.AGENT_MESSAGE,
        }:
            self._consume_message(event)
        elif kind in {
            CodexActivityKind.AGENT_REASONING_DELTA,
            CodexActivityKind.AGENT_REASONING_SUMMARY,
        }:
            self._consume_reasoning(event)
        elif kind in {
            CodexActivityKind.TOOL_STARTED,
            CodexActivityKind.TOOL_COMPLETED,
            CodexActivityKind.TOOL_FAILED,
        }:
            self._consume_tool(event)
        elif kind in {
            CodexActivityKind.BUILD_STARTED,
            CodexActivityKind.BUILD_COMPILE_COMPLETED,
            CodexActivityKind.BUILD_CANDIDATE_STARTED,
            CodexActivityKind.BUILD_CANDIDATE_READY,
            CodexActivityKind.BUILD_PROTOCOL_VALIDATED,
            CodexActivityKind.BUILD_CANDIDATE_FAILED,
            CodexActivityKind.BUILD_COMPLETED,
        }:
            self._consume_build(event)
        else:
            self._consume_lifecycle(event)

    def render_text(self) -> str:
        blocks: list[str] = []
        for entry in self._entries:
            header = entry.label
            if entry.body:
                blocks.append(f"{header}\n{_indent(entry.body)}")
            else:
                blocks.append(header)
        return "\n\n".join(blocks)

    def _consume_message(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "message")
        text = event.data.get("text")
        if not isinstance(text, str):
            return
        if event.kind is CodexActivityKind.AGENT_TEXT_DELTA:
            text = self._message_text.get(key, "") + text
        self._message_text[key] = text
        if event.role is CodexActivityRole.REVIEWER:
            self._latest_reviewer_message = _text_identity(text)
        self._upsert(
            key,
            self._role_name(event.role),
            event.role.value,
            safe_display_text(text, self._display_bytes),
        )
        if event.kind is CodexActivityKind.AGENT_MESSAGE:
            self._message_text.pop(key, None)

    def _consume_reasoning(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "reasoning")
        parts = self._reasoning_text.setdefault(key, {})
        if event.kind is CodexActivityKind.AGENT_REASONING_DELTA:
            text = event.data.get("text")
            index = event.data.get("summary_index")
            if not isinstance(text, str) or not isinstance(index, int):
                return
            parts[index] = parts.get(index, "") + text
        else:
            summary = event.data.get("summary")
            if not isinstance(summary, list) or not all(
                isinstance(part, str) for part in summary
            ):
                return
            parts.clear()
            parts.update(enumerate(summary))
        text = "\n".join(parts[index] for index in sorted(parts))
        self._upsert(
            key,
            f"{self._role_name(event.role)} · reasoning summary",
            "reasoning",
            safe_display_text(text, self._display_bytes),
        )
        if event.kind is CodexActivityKind.AGENT_REASONING_SUMMARY:
            self._reasoning_text.pop(key, None)

    def _consume_tool(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "tool")
        state = self._tool_data.setdefault(key, {})
        if event.kind is CodexActivityKind.TOOL_STARTED:
            state.update(event.data)
            state["state"] = "running"
        else:
            state.update(event.data)
            state["state"] = (
                "failed"
                if event.kind is CodexActivityKind.TOOL_FAILED
                else "completed"
            )
        tool = state.get("tool")
        if not isinstance(tool, str):
            tool = "unknown"
        body = self._format_tool(tool, state)
        result = state.get("result")
        if (
            tool == "review"
            and isinstance(result, str)
            and _text_identity(result) == self._latest_reviewer_message
        ):
            body = self._format_tool(tool, {**state, "result": None})
            body += "\nreview result returned to Sol"
        self._upsert(
            key,
            f"{self._role_name(event.role)} · tool",
            "tool-failed" if state.get("state") == "failed" else "tool",
            body,
        )
        if event.kind is CodexActivityKind.TOOL_STARTED:
            self._tool_data[key] = {"tool": tool}
        else:
            self._tool_data.pop(key, None)

    def _format_tool(self, tool: str, state: dict[str, object]) -> str:
        arguments = state.get("arguments")
        if not isinstance(arguments, dict):
            arguments = {}
        lines = [tool + _format_inline_arguments(tool, arguments)]
        if tool == "write":
            content = arguments.get("data", arguments.get("content"))
            encoding = arguments.get("encoding")
            if content is not None:
                lines.extend(("", self._format_tool_payload(content, encoding)))
        elif tool == "request_feature":
            title = arguments.get("title")
            description = arguments.get("description")
            if isinstance(title, str):
                lines.append("Title: " + safe_display_text(title, self._display_bytes))
            if isinstance(description, str):
                lines.append(
                    "Description:\n"
                    + _indent(safe_display_text(description, self._display_bytes))
                )

        state_name = state.get("state")
        if state_name == "running":
            lines.append("[running]")
        elif state_name == "failed":
            lines.append("[failed]")
        elif state_name == "completed":
            lines.append("[completed]")

        result = state.get("result")
        if result is not None and result != "":
            lines.append("Result:\n" + _indent(self._format_tool_payload(result)))
        error = state.get("error")
        if error is not None and error != "":
            lines.append(
                "Error:\n" + _indent(self._format_tool_payload(error))
            )
        return "\n".join(lines)

    def _format_tool_payload(
        self,
        value: object,
        encoding: object = None,
    ) -> str:
        if isinstance(value, bytes):
            return safe_display_bytes(value, self._display_bytes)
        if isinstance(value, str):
            if encoding == "base64":
                try:
                    decoded = base64.b64decode(value, validate=True)
                except (binascii.Error, ValueError):
                    return safe_display_text(value, self._display_bytes)
                return (
                    f"binary ({len(decoded)} bytes, base64): "
                    + safe_display_text(value, self._display_bytes)
                )
            return safe_display_text(value, self._display_bytes)
        if isinstance(value, dict) and "output" in value:
            status = value.get("status")
            output = value.get("output")
            rendered = f"status={safe_display_text(str(status))}"
            if output is not None and output != b"" and output != "":
                rendered += "\n" + self._format_tool_payload(output)
            return rendered
        return safe_display_text(
            json.dumps(value, ensure_ascii=False, sort_keys=True, default=str),
            self._display_bytes,
        )

    def _consume_build(self, event: CodexActivityEvent) -> None:
        if event.kind is CodexActivityKind.BUILD_STARTED:
            self._build_number += 1
            key = f"build:{event.generation}:{self._build_number}"
            self._active_build_key = key
            phases = ["started"]
            self._build_phases[key] = phases
        else:
            key = self._active_build_key
            if key is None:
                self._build_number += 1
                key = f"build:{event.generation}:{self._build_number}"
                self._active_build_key = key
                self._build_phases[key] = []
            phases = self._build_phases[key]
            result = event.data.get("result", event.data.get("status"))
            detail = "" if result is None else f" ({safe_display_text(str(result))})"
            phase = {
                CodexActivityKind.BUILD_COMPILE_COMPLETED: "compile/link",
                CodexActivityKind.BUILD_CANDIDATE_STARTED: "candidate boot",
                CodexActivityKind.BUILD_CANDIDATE_READY: "READY",
                CodexActivityKind.BUILD_PROTOCOL_VALIDATED: "protocol validation",
                CodexActivityKind.BUILD_CANDIDATE_FAILED: "candidate validation",
                CodexActivityKind.BUILD_COMPLETED: "build",
            }[event.kind]
            if event.kind in {
                CodexActivityKind.BUILD_CANDIDATE_STARTED,
            }:
                phases.append(f"{phase} …")
            elif event.kind is CodexActivityKind.BUILD_COMPILE_COMPLETED:
                success = result == "success"
                phases.append(
                    f"{phase} {'✓' if success else '✗'}{detail}"
                )
            elif event.kind in {
                CodexActivityKind.BUILD_CANDIDATE_FAILED,
            }:
                phases.append(f"{phase} ✗{detail}")
            elif event.kind is CodexActivityKind.BUILD_COMPLETED:
                success = result in {0, "0", "success"}
                phases.append(("success ✓" if success else "failed ✗") + detail)
                self._active_build_key = None
            else:
                phases.append(f"{phase} ✓{detail}")
        self._upsert(key, "Trusted build", "build", "\n".join(phases))

    def _consume_lifecycle(self, event: CodexActivityEvent) -> None:
        role = self._role_name(event.role)
        if event.kind is CodexActivityKind.REVIEW_STARTED:
            label = "Luna · review"
            body = "started"
        elif event.kind is CodexActivityKind.REVIEW_COMPLETED:
            label = "Luna · review"
            body = "completed"
        elif event.kind is CodexActivityKind.REVIEW_CANCELLED:
            label = "Luna · review"
            body = "cancelled"
        elif event.kind is CodexActivityKind.REVIEW_FAILED:
            label = "Luna · review"
            body = "failed"
        else:
            label = role
            body = event.kind.value
        useful = {
            key: value
            for key, value in event.data.items()
            if key not in {"model", "reasoning_effort", "service_tier"}
        }
        if useful:
            body += " " + safe_display_text(
                json.dumps(useful, ensure_ascii=False, sort_keys=True, default=str),
                self._display_bytes,
            )
        self._upsert(
            f"lifecycle:{event.sequence}",
            label,
            event.role.value,
            body,
        )

    def _correlation_key(self, event: CodexActivityEvent, suffix: str) -> str:
        item = event.item_id or f"sequence-{event.sequence}"
        return ":".join(
            (
                event.role.value,
                event.thread_id or "thread",
                event.turn_id or "turn",
                item,
                suffix,
            )
        )

    def _upsert(self, key: str, label: str, category: str, body: str) -> None:
        entry = ActivityDisplayEntry(key, label, category, body)
        position = self._positions.get(key)
        if position is None:
            self._positions[key] = len(self._entries)
            self._entries.append(entry)
        else:
            self._entries[position] = entry
        self._trim()

    def _trim(self) -> None:
        self._entries = [
            entry
            for entry in self._entries
            if entry.key != "scrollback:discarded"
        ]
        total = sum(entry.size_bytes for entry in self._entries)
        discarded = False
        reserve_marker = self._discarded or (
            len(self._entries) > self._max_entries
            or total > self._max_bytes
        )
        allowed_entries = self._max_entries - (1 if reserve_marker else 0)
        while (
            len(self._entries) > allowed_entries
            or total > self._max_bytes
        ) and len(self._entries) > 1:
            removed = self._entries.pop(0)
            total -= removed.size_bytes
            self._forget(removed.key)
            discarded = True
        if discarded:
            self._discarded = True
        if self._discarded:
            marker = ActivityDisplayEntry(
                "scrollback:discarded",
                "Harness",
                "harness",
                "… older live activity discarded from UI scrollback …",
            )
            self._entries.insert(0, marker)
        self._positions = {
            entry.key: index for index, entry in enumerate(self._entries)
        }

    def _forget(self, key: str) -> None:
        self._message_text.pop(key, None)
        self._reasoning_text.pop(key, None)
        self._tool_data.pop(key, None)
        self._build_phases.pop(key, None)

    @staticmethod
    def _role_name(role: CodexActivityRole) -> str:
        return {
            CodexActivityRole.IMPLEMENTOR: "Sol",
            CodexActivityRole.REVIEWER: "Luna",
            CodexActivityRole.HARNESS: "Harness",
        }[role]


def safe_display_text(text: str, limit_bytes: int = DEFAULT_DISPLAY_BYTES) -> str:
    """Escape terminal controls while retaining normal newlines and tabs."""
    encoded = text.encode("utf-8", errors="backslashreplace")
    text = encoded.decode("utf-8")
    original_size = len(encoded)
    remaining = 0
    if len(encoded) > limit_bytes:
        encoded = encoded[:limit_bytes]
        while True:
            try:
                text = encoded.decode("utf-8")
                break
            except UnicodeDecodeError:
                encoded = encoded[:-1]
        remaining = original_size - len(encoded)
    escaped: list[str] = []
    for character in text:
        codepoint = ord(character)
        if character == "\n":
            escaped.append("\n")
        elif character == "\t":
            escaped.append("    ")
        elif character == "\r":
            escaped.append("\\r")
        elif codepoint <= 0x1F or codepoint == 0x7F or 0x80 <= codepoint <= 0x9F:
            escaped.append(f"\\x{codepoint:02x}")
        else:
            escaped.append(character)
    if remaining:
        escaped.append(f"\n… {remaining} bytes more in activity payload …")
    return "".join(escaped)


def safe_display_bytes(data: bytes, limit_bytes: int = DEFAULT_DISPLAY_BYTES) -> str:
    try:
        return safe_display_text(data.decode("utf-8"), limit_bytes)
    except UnicodeDecodeError:
        shown = data[: min(len(data), max(1, limit_bytes // 2))]
        suffix = ""
        if len(shown) < len(data):
            suffix = f" … {len(data) - len(shown)} bytes more …"
        return f"binary ({len(data)} bytes): {shown.hex(' ')}{suffix}"


def _format_inline_arguments(tool: str, arguments: dict[str, object]) -> str:
    omitted = {"data", "content", "description"}
    parts: list[str] = []
    for key, value in arguments.items():
        if key in omitted:
            continue
        if tool == "request_feature" and key == "title":
            continue
        if isinstance(value, (str, int, float, bool)) or value is None:
            rendered = safe_display_text(str(value))
        else:
            rendered = safe_display_text(
                json.dumps(value, ensure_ascii=False, sort_keys=True, default=str)
            )
        parts.append(f"{safe_display_text(str(key))}={rendered}")
    return " " + " ".join(parts) if parts else ""


def _indent(text: str) -> str:
    return "\n".join(f"  {line}" for line in text.splitlines() or [""])


def _text_identity(text: str) -> tuple[int, str]:
    encoded = text.encode("utf-8", errors="backslashreplace")
    return len(encoded), hashlib.sha256(encoded).hexdigest()

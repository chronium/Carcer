"""Deterministic presentation state for the interactive operator TUI."""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
from collections.abc import Mapping
from dataclasses import dataclass, replace
from enum import StrEnum

from .codex_activity import CodexActivityEvent, CodexActivityKind, CodexActivityRole


DEFAULT_DISPLAY_BYTES = 64 * 1024
DEFAULT_SCROLLBACK_BYTES = 2 * 1024 * 1024
DEFAULT_SCROLLBACK_ENTRIES = 800
SUMMARY_DISPLAY_BYTES = 1024


class ActivityDisplayKind(StrEnum):
    MESSAGE = "message"
    REASONING = "reasoning"
    TOOL = "tool"
    FEATURE_REQUEST = "feature-request"
    BUILD = "build"
    OPERATOR = "operator"
    LIFECYCLE = "lifecycle"
    NOTICE = "notice"


class ActivityDisplayState(StrEnum):
    PENDING = "pending"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"
    INTERRUPTED = "interrupted"
    CANCELLED = "cancelled"


class FeatureRequestRecordingState(StrEnum):
    RECORDING = "recording"
    RECORDED = "recorded"
    FAILED = "failed"


class FeatureRequestTrustedStatus(StrEnum):
    PENDING = "pending"
    APPROVED = "approved"
    DENIED = "denied"


@dataclass(frozen=True, slots=True)
class AgentMessagePresentation:
    role: CodexActivityRole
    text: str
    finalized: bool


@dataclass(frozen=True, slots=True)
class ReasoningPresentation:
    role: CodexActivityRole
    text: str
    finalized: bool


@dataclass(frozen=True, slots=True)
class ToolDetailPresentation:
    text: str
    byte_count: int
    line_count: int | None
    binary: bool
    truncated: bool


@dataclass(frozen=True, slots=True)
class ToolPresentation:
    role: CodexActivityRole
    tool: str
    state: ActivityDisplayState
    summary: str
    detail: ToolDetailPresentation | None = None
    result_note: str = ""


@dataclass(frozen=True, slots=True)
class FeatureRequestPresentation:
    role: CodexActivityRole
    recording_state: FeatureRequestRecordingState
    trusted_status: FeatureRequestTrustedStatus | None
    title: str
    description: str
    request_id: str = ""
    error: str = ""


@dataclass(frozen=True, slots=True)
class BuildPhasePresentation:
    name: str
    state: ActivityDisplayState


@dataclass(frozen=True, slots=True)
class BuildPresentation:
    state: ActivityDisplayState
    phases: tuple[BuildPhasePresentation, ...]
    diagnostic: str = ""


@dataclass(frozen=True, slots=True)
class OperatorPresentation:
    command: str | None
    output: str
    finalized: bool


@dataclass(frozen=True, slots=True)
class LifecyclePresentation:
    role: CodexActivityRole
    title: str
    detail: str
    state: ActivityDisplayState


@dataclass(frozen=True, slots=True)
class NoticePresentation:
    title: str
    text: str


ActivityPresentation = (
    AgentMessagePresentation
    | ReasoningPresentation
    | ToolPresentation
    | FeatureRequestPresentation
    | BuildPresentation
    | OperatorPresentation
    | LifecyclePresentation
    | NoticePresentation
)


@dataclass(frozen=True, slots=True)
class ActivityDisplayEntry:
    """One logical, safely renderable item in the TUI scrollback."""

    key: str
    kind: ActivityDisplayKind
    presentation: ActivityPresentation

    @property
    def size_bytes(self) -> int:
        return len(_presentation_text(self.presentation).encode("utf-8"))


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
        self._tool_presentations: dict[str, ToolPresentation] = {}
        self._build_number = 0
        self._active_build_key: str | None = None
        self._builds: dict[str, BuildPresentation] = {}
        self._operator_number = 0
        self._active_operator_key: str | None = None
        self._discarded = False
        self._latest_reviewer_message: tuple[int, str] | None = None
        self._revision = 0

    @property
    def entries(self) -> tuple[ActivityDisplayEntry, ...]:
        return tuple(self._entries)

    @property
    def revision(self) -> int:
        return self._revision

    def begin_operator_block(self, command: str | None = None) -> str:
        self.finish_operator_block()
        self._operator_number += 1
        key = f"operator:{self._operator_number}"
        self._active_operator_key = key
        self._upsert_entry(
            ActivityDisplayEntry(
                key,
                ActivityDisplayKind.OPERATOR,
                OperatorPresentation(
                    None
                    if command is None
                    else safe_display_text(command, SUMMARY_DISPLAY_BYTES),
                    "",
                    False,
                ),
            )
        )
        return key

    def append_operator_output(self, text: str) -> bool:
        before = self._revision
        key = self._active_operator_key or self.begin_operator_block()
        entry = self._entries[self._positions[key]]
        presentation = entry.presentation
        if not isinstance(presentation, OperatorPresentation):
            raise RuntimeError("active operator block has the wrong presentation")
        rendered = safe_display_text(text, self._display_bytes)
        separator = "\n" if presentation.output else ""
        self._upsert_entry(
            replace(
                entry,
                presentation=replace(
                    presentation,
                    output=safe_display_text(
                        presentation.output + separator + rendered,
                        self._display_bytes,
                    ),
                ),
            )
        )
        return self._revision != before

    def finish_operator_block(self) -> bool:
        key = self._active_operator_key
        if key is None:
            return False
        self._active_operator_key = None
        position = self._positions.get(key)
        if position is None:
            return False
        entry = self._entries[position]
        presentation = entry.presentation
        if not isinstance(presentation, OperatorPresentation):
            return False
        before = self._revision
        self._upsert_entry(
            replace(entry, presentation=replace(presentation, finalized=True))
        )
        return self._revision != before

    def consume(self, event: CodexActivityEvent) -> bool:
        before = self._revision
        kind = event.kind
        if kind in {CodexActivityKind.AGENT_TEXT_DELTA, CodexActivityKind.AGENT_MESSAGE}:
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
        return self._revision != before

    def render_text(self) -> str:
        return "\n\n".join(_entry_text(entry) for entry in self._entries)

    def _consume_message(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "message")
        text = event.data.get("text")
        if not isinstance(text, str):
            return
        finalized = event.kind is CodexActivityKind.AGENT_MESSAGE
        if not finalized:
            text = self._message_text.get(key, "") + text
        self._message_text[key] = text
        if event.role is CodexActivityRole.REVIEWER:
            self._latest_reviewer_message = _text_identity(text)
        self._upsert_entry(
            ActivityDisplayEntry(
                key,
                ActivityDisplayKind.MESSAGE,
                AgentMessagePresentation(
                    event.role,
                    safe_display_text(text, self._display_bytes),
                    finalized,
                ),
            )
        )
        if finalized:
            self._message_text.pop(key, None)

    def _consume_reasoning(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "reasoning")
        parts = self._reasoning_text.setdefault(key, {})
        finalized = event.kind is CodexActivityKind.AGENT_REASONING_SUMMARY
        if not finalized:
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
        if not text.strip():
            if finalized:
                self._reasoning_text.pop(key, None)
                self._remove(key)
            return
        self._upsert_entry(
            ActivityDisplayEntry(
                key,
                ActivityDisplayKind.REASONING,
                ReasoningPresentation(
                    event.role,
                    safe_display_text(text, self._display_bytes),
                    finalized,
                ),
            )
        )
        if finalized:
            self._reasoning_text.pop(key, None)

    def _consume_tool(self, event: CodexActivityEvent) -> None:
        key = self._correlation_key(event, "tool")
        existing = self._tool_presentations.get(key)
        tool = event.data.get("tool")
        if not isinstance(tool, str):
            tool = existing.tool if existing is not None else "unknown"
        arguments = event.data.get("arguments")
        if not isinstance(arguments, dict):
            arguments = {}
        state = {
            CodexActivityKind.TOOL_STARTED: ActivityDisplayState.RUNNING,
            CodexActivityKind.TOOL_COMPLETED: ActivityDisplayState.COMPLETED,
            CodexActivityKind.TOOL_FAILED: ActivityDisplayState.FAILED,
        }[event.kind]
        if tool == "request_feature":
            self._consume_feature_request(key, event, arguments)
            return
        summary = _tool_summary(tool, arguments)
        detail = self._tool_detail(tool, arguments, event.data, existing)
        result_note = ""
        result = event.data.get("result")
        if (
            tool == "review"
            and isinstance(result, str)
            and _text_identity(result) == self._latest_reviewer_message
        ):
            detail = None
            result_note = "result returned to Sol"
        presentation = ToolPresentation(
            event.role, tool, state, summary, detail, result_note
        )
        self._tool_presentations[key] = presentation
        self._upsert_entry(
            ActivityDisplayEntry(key, ActivityDisplayKind.TOOL, presentation)
        )
        if event.kind is not CodexActivityKind.TOOL_STARTED:
            self._tool_presentations.pop(key, None)

    def _consume_feature_request(
        self,
        key: str,
        event: CodexActivityEvent,
        arguments: dict[str, object],
    ) -> None:
        title = arguments.get("title")
        description = arguments.get("description")
        recording_state = {
            CodexActivityKind.TOOL_STARTED: FeatureRequestRecordingState.RECORDING,
            CodexActivityKind.TOOL_COMPLETED: FeatureRequestRecordingState.RECORDED,
            CodexActivityKind.TOOL_FAILED: FeatureRequestRecordingState.FAILED,
        }[event.kind]
        trusted_status = (
            FeatureRequestTrustedStatus.PENDING
            if recording_state is FeatureRequestRecordingState.RECORDED
            else None
        )
        request_id = _feature_request_id(event.data.get("result"))
        error = (
            _feature_request_error(event.data)
            if recording_state is FeatureRequestRecordingState.FAILED
            else ""
        )
        self._upsert_entry(
            ActivityDisplayEntry(
                key,
                ActivityDisplayKind.FEATURE_REQUEST,
                FeatureRequestPresentation(
                    event.role,
                    recording_state,
                    trusted_status,
                    safe_display_text(
                        title
                        if isinstance(title, str)
                        else "External capability request",
                        SUMMARY_DISPLAY_BYTES,
                    ),
                    safe_display_text(
                        description if isinstance(description, str) else "",
                        self._display_bytes,
                    ),
                    request_id,
                    error,
                ),
            )
        )

    def _tool_detail(
        self,
        tool: str,
        arguments: dict[str, object],
        data: dict[str, object],
        existing: ToolPresentation | None,
    ) -> ToolDetailPresentation | None:
        if tool == "write":
            content = arguments.get("data", arguments.get("content"))
            if content is not None:
                return _payload_presentation(
                    content, self._display_bytes, arguments.get("encoding")
                )
        error = data.get("error")
        if not _empty_payload(error):
            return _payload_presentation(error, self._display_bytes)
        result = data.get("result")
        if (
            isinstance(result, dict)
            and result.get("status") == 0
            and _empty_payload(result.get("output"))
        ):
            return existing.detail if existing is not None else None
        if not _empty_payload(result):
            return _payload_presentation(result, self._display_bytes)
        return existing.detail if existing is not None else None

    def _consume_build(self, event: CodexActivityEvent) -> None:
        if event.kind is CodexActivityKind.BUILD_STARTED:
            self._build_number += 1
            key = f"build:{event.generation}:{self._build_number}"
            self._active_build_key = key
            build = BuildPresentation(
                ActivityDisplayState.RUNNING,
                (
                    BuildPhasePresentation("compile/link", ActivityDisplayState.RUNNING),
                    BuildPhasePresentation("candidate boot", ActivityDisplayState.PENDING),
                    BuildPhasePresentation("READY", ActivityDisplayState.PENDING),
                    BuildPhasePresentation("protocol", ActivityDisplayState.PENDING),
                ),
            )
        else:
            key = self._active_build_key
            if key is None:
                self._build_number += 1
                key = f"build:{event.generation}:{self._build_number}"
                self._active_build_key = key
                build = BuildPresentation(
                    ActivityDisplayState.RUNNING,
                    tuple(
                        BuildPhasePresentation(name, ActivityDisplayState.PENDING)
                        for name in ("compile/link", "candidate boot", "READY", "protocol")
                    ),
                )
            else:
                build = self._builds[key]
            build = self._advance_build(build, event)
        self._builds[key] = build
        self._upsert_entry(ActivityDisplayEntry(key, ActivityDisplayKind.BUILD, build))
        if event.kind is CodexActivityKind.BUILD_COMPLETED:
            self._active_build_key = None

    def _advance_build(
        self, build: BuildPresentation, event: CodexActivityEvent
    ) -> BuildPresentation:
        phases = list(build.phases)
        result = event.data.get("result", event.data.get("status"))
        diagnostic = build.diagnostic
        state = build.state
        if event.kind is CodexActivityKind.BUILD_COMPILE_COMPLETED:
            success = result == "success"
            phases[0] = replace(
                phases[0],
                state=ActivityDisplayState.COMPLETED
                if success
                else ActivityDisplayState.FAILED,
            )
            if not success:
                state = ActivityDisplayState.FAILED
                diagnostic = _build_diagnostic(event.data, self._display_bytes)
        elif event.kind is CodexActivityKind.BUILD_CANDIDATE_STARTED:
            phases[1] = replace(phases[1], state=ActivityDisplayState.RUNNING)
        elif event.kind is CodexActivityKind.BUILD_CANDIDATE_READY:
            phases[1] = replace(phases[1], state=ActivityDisplayState.COMPLETED)
            phases[2] = replace(phases[2], state=ActivityDisplayState.COMPLETED)
            phases[3] = replace(phases[3], state=ActivityDisplayState.RUNNING)
        elif event.kind is CodexActivityKind.BUILD_PROTOCOL_VALIDATED:
            phases[3] = replace(phases[3], state=ActivityDisplayState.COMPLETED)
        elif event.kind is CodexActivityKind.BUILD_CANDIDATE_FAILED:
            for index in (1, 2, 3):
                if phases[index].state is not ActivityDisplayState.COMPLETED:
                    phases[index] = replace(
                        phases[index], state=ActivityDisplayState.FAILED
                    )
                    break
            state = ActivityDisplayState.FAILED
            diagnostic = _build_diagnostic(event.data, self._display_bytes)
        elif event.kind is CodexActivityKind.BUILD_COMPLETED:
            success = result in {0, "0", "success"}
            state = (
                ActivityDisplayState.COMPLETED
                if success
                else ActivityDisplayState.FAILED
            )
            if not success and not diagnostic:
                diagnostic = _build_diagnostic(event.data, self._display_bytes)
        return BuildPresentation(state, tuple(phases), diagnostic)

    def _consume_lifecycle(self, event: CodexActivityEvent) -> None:
        if event.kind in {
            CodexActivityKind.SESSION_STARTED,
            CodexActivityKind.SESSION_STOPPED,
            CodexActivityKind.TURN_STARTED,
            CodexActivityKind.TURN_COMPLETED,
            CodexActivityKind.REVIEW_STARTED,
            CodexActivityKind.REVIEW_COMPLETED,
        }:
            return
        state = {
            CodexActivityKind.TURN_INTERRUPTED: ActivityDisplayState.INTERRUPTED,
            CodexActivityKind.TURN_FAILED: ActivityDisplayState.FAILED,
            CodexActivityKind.REVIEW_CANCELLED: ActivityDisplayState.CANCELLED,
            CodexActivityKind.REVIEW_FAILED: ActivityDisplayState.FAILED,
        }.get(event.kind, ActivityDisplayState.FAILED)
        useful = {
            key: value
            for key, value in event.data.items()
            if key
            not in {
                "model",
                "reasoning_effort",
                "reasoning_summary",
                "service_tier",
                "service_tier_name",
                "agent_contract_version",
            }
        }
        detail = ""
        if useful:
            detail = safe_display_text(
                json.dumps(useful, ensure_ascii=False, sort_keys=True, default=str),
                self._display_bytes,
            )
        self._upsert_entry(
            ActivityDisplayEntry(
                f"lifecycle:{event.sequence}",
                ActivityDisplayKind.LIFECYCLE,
                LifecyclePresentation(
                    event.role, event.kind.value, detail, state
                ),
            )
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

    def _upsert_entry(self, entry: ActivityDisplayEntry) -> None:
        position = self._positions.get(entry.key)
        if position is None:
            self._positions[entry.key] = len(self._entries)
            self._entries.append(entry)
            self._revision += 1
        elif self._entries[position] != entry:
            self._entries[position] = entry
            self._revision += 1
        self._trim()

    def _remove(self, key: str) -> None:
        position = self._positions.get(key)
        if position is None:
            return
        self._entries.pop(position)
        self._forget(key)
        self._positions = {entry.key: index for index, entry in enumerate(self._entries)}
        self._revision += 1

    def _trim(self) -> None:
        self._entries = [
            entry for entry in self._entries if entry.key != "scrollback:discarded"
        ]
        total = sum(entry.size_bytes for entry in self._entries)
        discarded = False
        reserve_marker = self._discarded or (
            len(self._entries) > self._max_entries or total > self._max_bytes
        )
        allowed_entries = self._max_entries - (1 if reserve_marker else 0)
        while (
            len(self._entries) > allowed_entries or total > self._max_bytes
        ) and len(self._entries) > 1:
            removed = self._entries.pop(0)
            total -= removed.size_bytes
            self._forget(removed.key)
            discarded = True
        if discarded:
            self._discarded = True
            self._revision += 1
        if self._discarded:
            self._entries.insert(
                0,
                ActivityDisplayEntry(
                    "scrollback:discarded",
                    ActivityDisplayKind.NOTICE,
                    NoticePresentation(
                        "Harness",
                        "… older live activity discarded from UI scrollback …",
                    ),
                ),
            )
        self._positions = {entry.key: index for index, entry in enumerate(self._entries)}

    def _forget(self, key: str) -> None:
        self._message_text.pop(key, None)
        self._reasoning_text.pop(key, None)
        self._tool_presentations.pop(key, None)
        self._builds.pop(key, None)
        if self._active_build_key == key:
            self._active_build_key = None
        if self._active_operator_key == key:
            self._active_operator_key = None


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


def _payload_presentation(
    value: object, limit_bytes: int, encoding: object = None
) -> ToolDetailPresentation:
    if isinstance(value, dict) and "output" in value:
        output = value.get("output", b"")
        if not _empty_payload(output):
            return _payload_presentation(output, limit_bytes)
        value = f"status={value.get('status')}"
    if isinstance(value, bytes):
        try:
            decoded = value.decode("utf-8")
        except UnicodeDecodeError:
            return ToolDetailPresentation(
                safe_display_bytes(value, limit_bytes),
                len(value),
                None,
                True,
                len(value) > max(1, limit_bytes // 2),
            )
        return ToolDetailPresentation(
            safe_display_text(decoded, limit_bytes),
            len(value),
            _line_count(decoded),
            False,
            len(value) > limit_bytes,
        )
    if isinstance(value, str) and encoding == "base64":
        try:
            decoded = base64.b64decode(value, validate=True)
        except (binascii.Error, ValueError):
            pass
        else:
            return ToolDetailPresentation(
                f"binary ({len(decoded)} bytes, base64): "
                + safe_display_text(value, limit_bytes),
                len(decoded),
                None,
                True,
                len(value.encode("utf-8")) > limit_bytes,
            )
    if not isinstance(value, str):
        value = json.dumps(value, ensure_ascii=False, sort_keys=True, default=str)
    encoded = value.encode("utf-8", errors="backslashreplace")
    return ToolDetailPresentation(
        safe_display_text(value, limit_bytes),
        len(encoded),
        _line_count(value),
        False,
        len(encoded) > limit_bytes,
    )


def _feature_request_id(result: object) -> str:
    if not isinstance(result, Mapping) or result.get("status") != 0:
        return ""
    output = result.get("output")
    if isinstance(output, bytes):
        try:
            value = output.decode("ascii")
        except UnicodeDecodeError:
            return ""
    elif isinstance(output, str):
        value = output
    else:
        return ""
    if not value.isascii() or not value.isdecimal() or value.startswith("0"):
        return ""
    return value


def _feature_request_error(data: Mapping[str, object]) -> str:
    error = data.get("error")
    if not _empty_payload(error):
        return _one_line(_payload_presentation(error, SUMMARY_DISPLAY_BYTES).text)
    result = data.get("result")
    if isinstance(result, Mapping):
        output = result.get("output")
        if not _empty_payload(output):
            return _one_line(
                _payload_presentation(output, SUMMARY_DISPLAY_BYTES).text
            )
    return "request recording failed"


def _tool_summary(tool: str, arguments: dict[str, object]) -> str:
    def value(name: str) -> str | None:
        candidate = arguments.get(name)
        if isinstance(candidate, (str, int)):
            return safe_display_text(str(candidate), SUMMARY_DISPLAY_BYTES)
        return None

    path = value("path")
    if tool in {"read", "write", "remove"} and path is not None:
        extras: list[str] = []
        if tool in {"read", "write"}:
            offset = value("offset")
            if offset is not None and offset != "0":
                extras.append(f"offset={offset}")
        if tool == "read":
            length = value("length")
            if length is not None:
                extras.append(f"length={length}")
        return path + ("  " + " ".join(extras) if extras else "")
    if tool == "truncate" and path is not None:
        size = value("size") or value("length")
        return path if size is None else f"{path} → {size} bytes"
    if tool == "list":
        return value("prefix") or "guest source"
    if tool == "review":
        return value("focus") or "independent review"
    if tool == "finish_generation":
        return "validated successor and handoff"
    if tool == "build":
        return "exact current source"
    if arguments:
        return safe_display_text(
            _one_line(
                json.dumps(arguments, ensure_ascii=False, sort_keys=True, default=str)
            ),
            SUMMARY_DISPLAY_BYTES,
        )
    return ""


def _build_diagnostic(data: dict[str, object], limit: int) -> str:
    useful = {
        key: value
        for key, value in data.items()
        if key != "status" or value not in {None, ""}
    }
    if not useful:
        return ""
    if set(useful) == {"result"}:
        return safe_display_text(str(useful["result"]), limit)
    return safe_display_text(
        json.dumps(useful, ensure_ascii=False, sort_keys=True, default=str), limit
    )


def _entry_text(entry: ActivityDisplayEntry) -> str:
    presentation = entry.presentation
    if isinstance(presentation, AgentMessagePresentation):
        return f"{_role_name(presentation.role)}\n{presentation.text}"
    if isinstance(presentation, ReasoningPresentation):
        return f"{_role_name(presentation.role)} · reasoning\n{presentation.text}"
    if isinstance(presentation, ToolPresentation):
        text = (
            f"{_role_name(presentation.role)} · {presentation.tool} "
            f"{presentation.summary}"
        ).rstrip()
        if presentation.detail is not None:
            text += "\n" + presentation.detail.text
        return text
    if isinstance(presentation, FeatureRequestPresentation):
        lines = [
            "Feature request",
            presentation.title,
            presentation.description,
            presentation.recording_state.value,
        ]
        if presentation.request_id:
            lines.append(f"request {presentation.request_id}")
        if presentation.trusted_status is not None:
            lines.append(
                f"trusted status: {presentation.trusted_status.value} · not provisioned"
            )
        if presentation.error:
            lines.append(presentation.error)
        return "\n".join(line for line in lines if line)
    if isinstance(presentation, BuildPresentation):
        return "Trusted build\n" + "\n".join(
            f"{phase.name} {phase.state.value}" for phase in presentation.phases
        )
    if isinstance(presentation, OperatorPresentation):
        command = (
            "" if presentation.command is None else f"codexos> {presentation.command}\n"
        )
        return "Operator\n" + command + presentation.output
    if isinstance(presentation, LifecyclePresentation):
        return (
            f"{_role_name(presentation.role)} · {presentation.title}\n"
            f"{presentation.detail}"
        )
    return presentation.title + ("\n" + presentation.text if presentation.text else "")


def _presentation_text(presentation: ActivityPresentation) -> str:
    if isinstance(presentation, (AgentMessagePresentation, ReasoningPresentation)):
        return presentation.text
    if isinstance(presentation, ToolPresentation):
        return "\n".join(
            part
            for part in (
                presentation.tool,
                presentation.summary,
                presentation.detail.text if presentation.detail else "",
                presentation.result_note,
            )
            if part
        )
    if isinstance(presentation, FeatureRequestPresentation):
        return "\n".join(
            (
                presentation.recording_state.value,
                presentation.trusted_status.value
                if presentation.trusted_status is not None
                else "",
                presentation.title,
                presentation.description,
                presentation.request_id,
                presentation.error,
            )
        )
    if isinstance(presentation, BuildPresentation):
        return "\n".join(
            [
                *(phase.name + phase.state.value for phase in presentation.phases),
                presentation.diagnostic,
            ]
        )
    if isinstance(presentation, OperatorPresentation):
        return "\n".join((presentation.command or "", presentation.output))
    if isinstance(presentation, LifecyclePresentation):
        return "\n".join((presentation.title, presentation.detail))
    return "\n".join((presentation.title, presentation.text))


def _role_name(role: CodexActivityRole) -> str:
    return {
        CodexActivityRole.IMPLEMENTOR: "Sol",
        CodexActivityRole.REVIEWER: "Luna",
        CodexActivityRole.HARNESS: "Harness",
    }[role]


def _line_count(text: str) -> int:
    if not text:
        return 0
    return text.count("\n") + (0 if text.endswith("\n") else 1)


def _one_line(text: str) -> str:
    return " ".join(text.split())


def _empty_payload(value: object) -> bool:
    return value is None or value == "" or value == b""


def _text_identity(text: str) -> tuple[int, str]:
    encoded = text.encode("utf-8", errors="backslashreplace")
    return len(encoded), hashlib.sha256(encoded).hexdigest()

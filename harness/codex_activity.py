"""Optional in-memory live activity for trusted operator observation."""

from __future__ import annotations

import queue
import threading
from collections.abc import Mapping
from dataclasses import dataclass
from enum import StrEnum


class CodexActivityRole(StrEnum):
    IMPLEMENTOR = "implementor"
    REVIEWER = "reviewer"
    HARNESS = "harness"


class CodexActivityKind(StrEnum):
    SESSION_STARTED = "session.started"
    SESSION_STOPPED = "session.stopped"
    TURN_STARTED = "turn.started"
    TURN_COMPLETED = "turn.completed"
    TURN_INTERRUPTED = "turn.interrupted"
    TURN_FAILED = "turn.failed"
    AGENT_MESSAGE = "agent.message"
    AGENT_TEXT_DELTA = "agent.text_delta"
    AGENT_REASONING_SUMMARY = "agent.reasoning_summary"
    AGENT_REASONING_DELTA = "agent.reasoning_delta"
    TOOL_STARTED = "tool.started"
    TOOL_COMPLETED = "tool.completed"
    TOOL_FAILED = "tool.failed"
    REVIEW_STARTED = "review.started"
    REVIEW_COMPLETED = "review.completed"
    REVIEW_CANCELLED = "review.cancelled"
    REVIEW_FAILED = "review.failed"
    EXIT_INTERVIEW_STARTED = "exit_interview.started"
    EXIT_INTERVIEW_QUESTION = "exit_interview.question"
    EXIT_INTERVIEW_ENDED = "exit_interview.ended"
    BUILD_STARTED = "build.started"
    BUILD_COMPILE_COMPLETED = "build.compile_completed"
    BUILD_CANDIDATE_STARTED = "build.candidate_started"
    BUILD_CANDIDATE_READY = "build.candidate_ready"
    BUILD_PROTOCOL_VALIDATED = "build.protocol_validated"
    BUILD_CANDIDATE_FAILED = "build.candidate_failed"
    BUILD_COMPLETED = "build.completed"


@dataclass(frozen=True, slots=True)
class CodexActivityEvent:
    sequence: int
    generation: int | None
    role: CodexActivityRole
    kind: CodexActivityKind
    data: Mapping[str, object]
    thread_id: str | None = None
    turn_id: str | None = None
    item_id: str | None = None


class CodexActivityStream:
    """An unbounded ordered queue whose producers never invoke consumers."""

    def __init__(self) -> None:
        self._events: queue.Queue[CodexActivityEvent] = queue.Queue()
        self._sequence = 0
        self._lock = threading.Lock()

    def publish(
        self,
        generation: int | None,
        role: CodexActivityRole,
        kind: CodexActivityKind,
        data: Mapping[str, object] | None = None,
        *,
        thread_id: str | None = None,
        turn_id: str | None = None,
        item_id: str | None = None,
    ) -> CodexActivityEvent:
        with self._lock:
            self._sequence += 1
            event = CodexActivityEvent(
                sequence=self._sequence,
                generation=generation,
                role=role,
                kind=kind,
                data=dict(data or {}),
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=item_id,
            )
            self._events.put_nowait(event)
        return event

    def next_event(self, timeout: float | None = None) -> CodexActivityEvent:
        return self._events.get(timeout=timeout)

    def drain(self) -> tuple[CodexActivityEvent, ...]:
        events: list[CodexActivityEvent] = []
        while True:
            try:
                events.append(self._events.get_nowait())
            except queue.Empty:
                return tuple(events)


def publish_activity(
    stream: CodexActivityStream | None,
    generation: int | None,
    role: CodexActivityRole,
    kind: CodexActivityKind,
    data: Mapping[str, object] | None = None,
    *,
    thread_id: str | None = None,
    turn_id: str | None = None,
    item_id: str | None = None,
) -> None:
    """Publish without allowing observation failure to affect the experiment."""
    if stream is None:
        return
    try:
        stream.publish(
            generation,
            role,
            kind,
            data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=item_id,
        )
    except Exception:
        pass


def publish_renderable_codex_notification(
    stream: CodexActivityStream | None,
    generation: int | None,
    role: CodexActivityRole,
    message: Mapping[str, object],
    thread_id: str,
    turn_id: str,
) -> None:
    """Publish only app-server content explicitly exposed as renderable text."""
    if stream is None:
        return
    method = message.get("method")
    params = message.get("params")
    if not isinstance(params, dict):
        return
    if params.get("threadId") != thread_id or params.get("turnId") != turn_id:
        return
    item_id = params.get("itemId")
    if item_id is not None and not isinstance(item_id, str):
        return

    if method == "item/agentMessage/delta":
        delta = params.get("delta")
        if isinstance(delta, str):
            publish_activity(
                stream,
                generation,
                role,
                CodexActivityKind.AGENT_TEXT_DELTA,
                {"text": delta},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=item_id,
            )
        return
    if method == "item/reasoning/summaryTextDelta":
        delta = params.get("delta")
        summary_index = params.get("summaryIndex")
        if isinstance(delta, str) and isinstance(summary_index, int):
            publish_activity(
                stream,
                generation,
                role,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": delta, "summary_index": summary_index},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=item_id,
            )
        return
    if method != "item/completed":
        return
    item = params.get("item")
    if not isinstance(item, dict):
        return
    completed_item_id = item.get("id")
    if isinstance(completed_item_id, str):
        item_id = completed_item_id
    if item.get("type") == "agentMessage":
        text = item.get("text")
        if not isinstance(text, str):
            return
        data: dict[str, object] = {"text": text}
        phase = item.get("phase")
        if isinstance(phase, str):
            data["phase"] = phase
        publish_activity(
            stream,
            generation,
            role,
            CodexActivityKind.AGENT_MESSAGE,
            data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=item_id,
        )
    elif item.get("type") == "reasoning":
        summary = item.get("summary")
        if isinstance(summary, list) and all(
            isinstance(part, str) for part in summary
        ):
            publish_activity(
                stream,
                generation,
                role,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": list(summary)},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=item_id,
            )

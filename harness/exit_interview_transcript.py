"""Human-facing persistence for completed exit-interview transcripts."""

from __future__ import annotations

import os
import tempfile
import threading
from dataclasses import dataclass, field
from pathlib import Path

from .codex_activity import CodexActivityKind, RenderableCodexActivity


class ExitInterviewTranscriptError(RuntimeError):
    """An exit-interview artifact could not be recorded safely."""


@dataclass(frozen=True, slots=True)
class ExitInterviewMetadata:
    run: str
    generation: int
    agent_contract_version: int
    model: str
    reasoning_effort: str
    reasoning_summary: str
    service_tier: str


@dataclass(frozen=True, slots=True)
class ExitInterviewTurn:
    number: int
    question: str
    reasoning_summaries: tuple[str, ...]
    response: str | None
    status: str


@dataclass(frozen=True, slots=True)
class ExitInterviewTranscriptSnapshot:
    metadata: ExitInterviewMetadata
    turns: tuple[ExitInterviewTurn, ...]


@dataclass(frozen=True, slots=True)
class ExitInterviewArtifact:
    path: Path
    relative_path: Path
    already_recorded: bool


@dataclass(slots=True)
class _ReasoningItem:
    parts: dict[int, str] = field(default_factory=dict)

    def completed(self, summary: list[str]) -> None:
        self.parts = dict(enumerate(summary))

    def text(self) -> tuple[str, ...]:
        return tuple(
            text
            for _, text in sorted(self.parts.items())
            if text.strip()
        )


@dataclass(slots=True)
class _InterviewTurn:
    number: int
    question: str
    turn_id: str
    reasoning_order: list[str] = field(default_factory=list)
    reasoning: dict[str, _ReasoningItem] = field(default_factory=dict)
    response: str | None = None
    status: str = "running"

    def reasoning_item(self, item_id: str) -> _ReasoningItem:
        item = self.reasoning.get(item_id)
        if item is None:
            item = _ReasoningItem()
            self.reasoning[item_id] = item
            self.reasoning_order.append(item_id)
        return item

    def snapshot(self) -> ExitInterviewTurn:
        summaries: list[str] = []
        for item_id in self.reasoning_order:
            summaries.extend(self.reasoning[item_id].text())
        return ExitInterviewTurn(
            self.number,
            self.question,
            tuple(summaries),
            self.response,
            self.status,
        )


class ExitInterviewTranscript:
    """Thread-safe generation-local interview text captured from trusted inputs."""

    def __init__(self, metadata: ExitInterviewMetadata) -> None:
        self._metadata = metadata
        self._turns: list[_InterviewTurn] = []
        self._active: _InterviewTurn | None = None
        self._lock = threading.Lock()

    def begin_turn(self, number: int, question: str, turn_id: str) -> None:
        with self._lock:
            if self._active is not None:
                raise RuntimeError("an exit-interview transcript turn is active")
            turn = _InterviewTurn(number, question, turn_id)
            self._turns.append(turn)
            self._active = turn

    def observe(self, activity: RenderableCodexActivity, turn_id: str) -> None:
        with self._lock:
            turn = self._active
            if turn is None or turn.turn_id != turn_id:
                return
            if activity.kind is CodexActivityKind.AGENT_REASONING_DELTA:
                text = activity.data.get("text")
                index = activity.data.get("summary_index")
                if not isinstance(text, str) or not isinstance(index, int):
                    return
                item = turn.reasoning_item(activity.item_id or "reasoning")
                item.parts[index] = item.parts.get(index, "") + text
            elif activity.kind is CodexActivityKind.AGENT_REASONING_SUMMARY:
                summary = activity.data.get("summary")
                if not isinstance(summary, list) or not all(
                    isinstance(part, str) for part in summary
                ):
                    return
                turn.reasoning_item(
                    activity.item_id or "reasoning"
                ).completed(summary)

    def finish_turn(
        self,
        turn_id: str,
        *,
        response: str | None,
        status: str,
    ) -> None:
        with self._lock:
            turn = self._active
            if turn is None or turn.turn_id != turn_id:
                return
            turn.response = response
            turn.status = status
            self._active = None

    def snapshot(self) -> ExitInterviewTranscriptSnapshot:
        with self._lock:
            return ExitInterviewTranscriptSnapshot(
                self._metadata,
                tuple(turn.snapshot() for turn in self._turns),
            )


class ExitInterviewArtifactStore:
    """Atomically add run-scoped transcript artifacts to a repository worktree."""

    def __init__(self, repository: str | Path, run_directory: str | Path) -> None:
        self._repository = Path(repository).resolve()
        if not self._repository.is_dir():
            raise ExitInterviewTranscriptError(
                f"interview repository is unavailable: {self._repository}"
            )
        self._run = Path(run_directory).resolve().name
        if not self._run or Path(self._run).name != self._run:
            raise ExitInterviewTranscriptError(
                "run directory cannot form an interview artifact namespace"
            )

    def persist(
        self,
        transcript: ExitInterviewTranscriptSnapshot,
        outcome: str,
    ) -> ExitInterviewArtifact | None:
        if not transcript.turns:
            return None
        if transcript.metadata.run != self._run:
            raise ExitInterviewTranscriptError(
                "interview transcript belongs to another run"
            )
        relative = (
            Path("artifacts")
            / "interviews"
            / self._run
            / f"generation-{transcript.metadata.generation:04d}.md"
        )
        output = self._repository / relative
        contents = render_exit_interview_markdown(transcript, outcome).encode(
            "utf-8"
        )
        existing = self._existing(output)
        if existing is not None:
            if existing == contents:
                return ExitInterviewArtifact(output, relative, True)
            raise ExitInterviewTranscriptError(
                f"conflicting exit-interview artifact already exists: {relative}"
            )

        directory = self._ensure_directory(relative.parent)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=".generation-interview-",
            suffix=".tmp",
            dir=directory,
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as stream:
                os.fchmod(stream.fileno(), 0o644)
                stream.write(contents)
                stream.flush()
                os.fsync(stream.fileno())
            try:
                os.link(temporary, output)
            except FileExistsError:
                existing = self._existing(output)
                if existing != contents:
                    raise ExitInterviewTranscriptError(
                        "conflicting exit-interview artifact appeared while "
                        f"recording: {relative}"
                    )
                return ExitInterviewArtifact(output, relative, True)
        finally:
            temporary.unlink(missing_ok=True)
        return ExitInterviewArtifact(output, relative, False)

    def _ensure_directory(self, relative: Path) -> Path:
        current = self._repository
        for part in relative.parts:
            current = current / part
            if current.is_symlink():
                raise ExitInterviewTranscriptError(
                    f"interview artifact directory must not be a symlink: {current}"
                )
            current.mkdir(exist_ok=True)
            if not current.is_dir():
                raise ExitInterviewTranscriptError(
                    f"interview artifact directory is unavailable: {current}"
                )
        return current

    @staticmethod
    def _existing(path: Path) -> bytes | None:
        if path.is_symlink():
            raise ExitInterviewTranscriptError(
                f"exit-interview artifact must not be a symlink: {path}"
            )
        try:
            return path.read_bytes()
        except FileNotFoundError:
            return None
        except IsADirectoryError as error:
            raise ExitInterviewTranscriptError(
                f"exit-interview artifact path is not a file: {path}"
            ) from error


def render_exit_interview_markdown(
    transcript: ExitInterviewTranscriptSnapshot,
    outcome: str,
) -> str:
    metadata = transcript.metadata
    lines = [
        "# CodexOS Exit Interview",
        "",
        f"Run: {_normalize_text(metadata.run)}",
        f"Generation: {metadata.generation}",
        f"Agent Contract: {metadata.agent_contract_version}",
        f"Model: {_normalize_text(metadata.model)}",
        f"Reasoning effort: {_normalize_text(metadata.reasoning_effort)}",
        f"Reasoning summary: {_normalize_text(metadata.reasoning_summary)}",
        f"Service tier: {_normalize_text(metadata.service_tier)}",
        f"Interview status: {_normalize_text(outcome)}",
    ]
    for turn in transcript.turns:
        lines.extend(
            [
                "",
                f"## Question {turn.number}",
                "",
                "### Operator",
                "",
                _normalize_text(turn.question).rstrip("\n"),
            ]
        )
        if turn.reasoning_summaries:
            lines.extend(["", "### Sol — reasoning summary", ""])
            lines.append(
                "\n\n".join(
                    _normalize_text(summary).rstrip("\n")
                    for summary in turn.reasoning_summaries
                )
            )
        if turn.response is not None:
            lines.extend(
                [
                    "",
                    "### Sol",
                    "",
                    _normalize_text(turn.response).rstrip("\n"),
                ]
            )
        if turn.status != "completed":
            lines.extend(["", f"Turn status: {_normalize_text(turn.status)}"])
    return "\n".join(lines).rstrip() + "\n"


def _normalize_text(text: str) -> str:
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")
    rendered: list[str] = []
    for character in normalized:
        codepoint = ord(character)
        if character in {"\n", "\t"} or codepoint >= 0x20 and not (
            0x7F <= codepoint <= 0x9F
        ):
            rendered.append(character)
        elif codepoint <= 0xFF:
            rendered.append(f"\\x{codepoint:02x}")
        else:
            rendered.append(f"\\u{codepoint:04x}")
    return "".join(rendered)

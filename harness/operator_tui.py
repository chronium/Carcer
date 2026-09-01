"""Full-screen interactive frontend for the trusted operator console."""

from __future__ import annotations

import queue
import threading
import time
from dataclasses import dataclass
from pathlib import Path

from rich.console import Group
from rich.markdown import Markdown as RichMarkdown
from rich.padding import Padding
from rich.text import Text
from textual import events
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical, VerticalScroll
from textual.content import Content, Span
from textual.message import Message
from textual.widgets import Input, Static
from textual.timer import Timer

from .codex_activity import CodexActivityStream
from .generation_git import GenerationGitRecorder
from .generation_runtime import CodexOSRun, RuntimeState
from .operator_console import OperatorConsole
from .operator_tui_model import (
    ActivityDisplayKind,
    ActivityDisplayEntry,
    ActivityDisplayState,
    ActivityFollowState,
    AgentMessagePresentation,
    BuildPresentation,
    FeatureRequestPresentation,
    FeatureRequestRecordingState,
    InterviewQuestionPresentation,
    LifecyclePresentation,
    NoticePresentation,
    OperatorPresentation,
    OperatorActivityModel,
    ReasoningPresentation,
    ToolPresentation,
    safe_display_text,
)


PAUSE_CONFIRMATION_SECONDS = 2.5

_ROLE_NAMES = {"implementor": "Sol", "reviewer": "Luna", "harness": "Harness"}
_ROLE_STYLES = {
    "implementor": "bold cyan",
    "reviewer": "bold magenta",
    "harness": "bold green",
}
_STATE_MARKERS = {
    ActivityDisplayState.PENDING: "·",
    ActivityDisplayState.RUNNING: "…",
    ActivityDisplayState.COMPLETED: "✓",
    ActivityDisplayState.FAILED: "✗",
    ActivityDisplayState.INTERRUPTED: "!",
    ActivityDisplayState.CANCELLED: "!",
}


class ActivityRow:
    """Shared keyed-entry behavior for specialized transcript widgets."""

    @property
    def entry(self) -> ActivityDisplayEntry:
        return self._entry


class AgentMessageRow(Static, ActivityRow):
    """Primary Sol/Luna output: plain while streaming, Markdown when final."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, AgentMessagePresentation)
        self._entry = entry
        super().__init__(
            _agent_message_renderable(presentation),
            markup=False,
            classes=f"activity-row agent-message-row {presentation.role.value}",
        )

    @property
    def renders_markdown(self) -> bool:
        presentation = _presentation(self._entry, AgentMessagePresentation)
        return presentation.finalized

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, AgentMessagePresentation)
        self.update(_agent_message_renderable(presentation))
        self._entry = entry


class ReasoningRow(Static, ActivityRow):
    """A deliberately subordinate renderable reasoning summary."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, ReasoningPresentation)
        self._entry = entry
        super().__init__(_reasoning_content(presentation), classes="activity-row reasoning-row")

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, ReasoningPresentation)
        self.update(_reasoning_content(presentation))
        self._entry = entry


class ToolDetailToggleRequested(Message):
    """A mouse-only request to toggle one tool's bounded detail."""


class ToolDetailToggle(Static):
    def on_click(self, event: events.Click) -> None:
        event.stop()
        self.post_message(ToolDetailToggleRequested())


class ToolRow(Vertical, ActivityRow):
    """Compact first-class dynamic tool activity with optional bounded detail."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, ToolPresentation)
        self._header = Static(
            _tool_header(presentation),
            classes=f"tool-header {presentation.role.value}",
        )
        self._meta = Static("", classes="tool-meta")
        self._toggle = ToolDetailToggle("", classes="tool-detail-toggle")
        self._detail = Static("", markup=False, classes="tool-detail")
        self._detail_close = ToolDetailToggle(
            "▴ collapse details", classes="tool-detail-toggle tool-detail-close"
        )
        self._detail_expanded = presentation.state is ActivityDisplayState.FAILED
        self._user_toggled = False
        self._entry = entry
        super().__init__()
        self.add_class("activity-row")
        self._update_detail(presentation)

    @property
    def detail_expanded(self) -> bool:
        return self._detail_expanded

    def compose(self) -> ComposeResult:
        yield self._header
        yield self._meta
        yield self._toggle
        yield self._detail
        yield self._detail_close

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, ToolPresentation)
        previous = _presentation(self._entry, ToolPresentation)
        self._header.update(_tool_header(presentation))
        if (
            presentation.state is ActivityDisplayState.FAILED
            and previous.state is not ActivityDisplayState.FAILED
            and not self._user_toggled
        ):
            self._detail_expanded = True
        self._entry = entry
        self._update_detail(presentation)

    def on_tool_detail_toggle_requested(
        self, event: ToolDetailToggleRequested
    ) -> None:
        event.stop()
        presentation = _presentation(self._entry, ToolPresentation)
        if presentation.detail is None:
            return
        self._detail_expanded = not self._detail_expanded
        self._user_toggled = True
        self._update_detail(presentation)
        try:
            command_input = self.app.query_one("#command-input", Input)
        except Exception:
            command_input = None
        if command_input is not None and not command_input.disabled:
            command_input.focus()
        app = self.app
        if isinstance(app, OperatorTui):
            app.call_after_refresh(app._sync_activity_anchor)

    def _update_detail(self, presentation: ToolPresentation) -> None:
        detail = presentation.detail
        note_parts: list[str] = []
        if detail is not None:
            note_parts.append(_payload_summary(detail.byte_count, detail.line_count))
        if presentation.result_note:
            note_parts.append(presentation.result_note)
        self._meta.update("  ".join(part for part in note_parts if part))
        if detail is None:
            self._toggle.display = False
            self._detail.display = False
            self._detail_close.display = False
            return
        self._toggle.display = True
        self._toggle.update("▾ details" if self._detail_expanded else "▸ details")
        self._detail.update(detail.text)
        self._detail.display = self._detail_expanded
        self._detail_close.display = self._detail_expanded


class FeatureRequestRow(Static, ActivityRow):
    """Prominent but non-authoritative external capability request."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, FeatureRequestPresentation)
        self._entry = entry
        super().__init__(
            _feature_content(presentation),
            classes="activity-row feature-request-row",
        )

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, FeatureRequestPresentation)
        self.update(_feature_content(presentation))
        self._entry = entry


class TrustedBuildRow(Static, ActivityRow):
    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, BuildPresentation)
        self._entry = entry
        super().__init__(
            _build_content(presentation),
            classes="activity-row trusted-build-row",
        )

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, BuildPresentation)
        self.update(_build_content(presentation))
        self._entry = entry


class OperatorRow(Static, ActivityRow):
    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, OperatorPresentation)
        self._entry = entry
        super().__init__(
            _operator_content(presentation),
            classes="activity-row operator-row",
        )

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, OperatorPresentation)
        self._entry = entry
        self.update(_operator_content(presentation))


class InterviewQuestionRow(Static, ActivityRow):
    """A human retrospective question, distinct from operator command output."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, InterviewQuestionPresentation)
        self._entry = entry
        super().__init__(
            Group(
                Text("You", style="bold bright_white"),
                Padding(Text(presentation.text), (0, 0, 0, 2)),
            ),
            markup=False,
            classes="activity-row interview-question-row",
        )

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, InterviewQuestionPresentation)
        self.update(
            Group(
                Text("You", style="bold bright_white"),
                Padding(Text(presentation.text), (0, 0, 0, 2)),
            )
        )
        self._entry = entry


class LifecycleRow(Static, ActivityRow):
    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, LifecyclePresentation)
        self._entry = entry
        super().__init__(
            _lifecycle_content(presentation),
            classes="activity-row lifecycle-content",
        )

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        presentation = _presentation(entry, LifecyclePresentation)
        self.update(_lifecycle_content(presentation))
        self._entry = entry


class NoticeRow(Static, ActivityRow):
    def __init__(self, entry: ActivityDisplayEntry) -> None:
        presentation = _presentation(entry, NoticePresentation)
        self._entry = entry
        super().__init__(
            presentation.text,
            markup=False,
            classes="activity-row notice-content",
        )


class ActivityTranscript(Vertical):
    """Incrementally reconcile ordered model entries to keyed row widgets."""

    def __init__(self) -> None:
        super().__init__(id="activity-rows")
        self._rows: dict[str, ActivityRow] = {}
        self._entries: dict[str, ActivityDisplayEntry] = {}
        self._order: tuple[str, ...] = ()

    @property
    def row_keys(self) -> tuple[str, ...]:
        return self._order

    @property
    def rows(self) -> tuple[ActivityRow, ...]:
        return tuple(self._rows[key] for key in self._order)

    def row_for(self, key: str) -> ActivityRow:
        return self._rows[key]

    def on_resize(self, event: events.Resize) -> None:
        app = self.app
        if isinstance(app, OperatorTui):
            app._sync_activity_anchor()

    def reconcile(self, entries: tuple[ActivityDisplayEntry, ...]) -> None:
        desired_order = tuple(entry.key for entry in entries)
        desired_keys = set(desired_order)
        entries_by_key = {entry.key: entry for entry in entries}

        for key in self._order:
            if key not in desired_keys:
                self._rows.pop(key).remove()
                self._entries.pop(key, None)

        for entry in entries:
            row = self._rows.get(entry.key)
            if row is not None and self._entries[entry.key] != entry:
                row.update_entry(entry)
                self._entries[entry.key] = entry

        index = 0
        while index < len(desired_order):
            if desired_order[index] in self._rows:
                index += 1
                continue
            missing: list[ActivityRow] = []
            while index < len(desired_order):
                key = desired_order[index]
                if key in self._rows:
                    break
                entry = entries_by_key[key]
                row = _activity_row(entry)
                self._rows[key] = row
                self._entries[key] = entry
                missing.append(row)
                index += 1
            if index < len(desired_order):
                self.mount(*missing, before=self._rows[desired_order[index]])
            else:
                self.mount(*missing)

        for index, key in enumerate(desired_order):
            row = self._rows[key]
            current = self.children[index]
            if current is not row:
                self.move_child(row, before=current)

        self._order = desired_order

def _activity_row(entry: ActivityDisplayEntry) -> ActivityRow:
    row_type: type[ActivityRow] = {
        ActivityDisplayKind.MESSAGE: AgentMessageRow,
        ActivityDisplayKind.REASONING: ReasoningRow,
        ActivityDisplayKind.TOOL: ToolRow,
        ActivityDisplayKind.FEATURE_REQUEST: FeatureRequestRow,
        ActivityDisplayKind.BUILD: TrustedBuildRow,
        ActivityDisplayKind.OPERATOR: OperatorRow,
        ActivityDisplayKind.INTERVIEW_QUESTION: InterviewQuestionRow,
        ActivityDisplayKind.LIFECYCLE: LifecycleRow,
        ActivityDisplayKind.NOTICE: NoticeRow,
    }[entry.kind]
    return row_type(entry)


def _presentation(entry: ActivityDisplayEntry, expected: type[object]):
    presentation = entry.presentation
    if not isinstance(presentation, expected):
        raise TypeError(
            f"{entry.kind.value} entry has invalid {type(presentation).__name__} presentation"
        )
    return presentation


def _agent_message_renderable(presentation: AgentMessagePresentation) -> Group:
    role = presentation.role.value
    label = _ROLE_NAMES[role]
    if presentation.turn_phase == "planning":
        label += " · planning"
    heading = Text(label, style=_ROLE_STYLES[role])
    body = (
        RichMarkdown(presentation.text, hyperlinks=False)
        if presentation.finalized
        else Text(presentation.text)
    )
    return Group(heading, Padding(body, (0, 0, 0, 2)))


def _reasoning_content(presentation: ReasoningPresentation) -> Content:
    role = _ROLE_NAMES[presentation.role.value]
    phase = " · planning" if presentation.turn_phase == "planning" else ""
    heading = f"  ◇ {role}{phase} · reasoning summary"
    body = "\n".join(f"    {line}" for line in presentation.text.splitlines())
    text = heading + ("\n" + body if body else "")
    return Content(text, [Span(2, len(text), "italic bright_blue")])


def _tool_header(presentation: ToolPresentation) -> Content:
    role = _ROLE_NAMES[presentation.role.value]
    marker = _STATE_MARKERS[presentation.state]
    text = f"  ● {role} · {presentation.tool}"
    if presentation.summary:
        text += f"  {presentation.summary}"
    text += f"  {marker}"
    marker_start = len(text) - len(marker)
    marker_style = (
        "bold red"
        if presentation.state is ActivityDisplayState.FAILED
        else "bold green"
        if presentation.state is ActivityDisplayState.COMPLETED
        else "bold yellow"
    )
    return Content(
        text,
        [
            Span(2, 3, "bold yellow"),
            Span(4, 4 + len(role), _ROLE_STYLES[presentation.role.value]),
            Span(marker_start, len(text), marker_style),
        ],
    )


def _payload_summary(byte_count: int, line_count: int | None) -> str:
    size = _human_size(byte_count)
    if line_count is None:
        return size
    unit = "line" if line_count == 1 else "lines"
    return f"{size} · {line_count} {unit}"


def _human_size(size: int) -> str:
    if size < 1024:
        return f"{size} B"
    if size < 1024 * 1024:
        return f"{size / 1024:.1f} KiB"
    return f"{size / (1024 * 1024):.1f} MiB"


def _feature_content(presentation: FeatureRequestPresentation) -> Content:
    marker = {
        FeatureRequestRecordingState.RECORDING: "…",
        FeatureRequestRecordingState.RECORDED: "✓",
        FeatureRequestRecordingState.FAILED: "✗",
    }[presentation.recording_state]
    if presentation.recording_state is FeatureRequestRecordingState.RECORDING:
        recording = "recording…"
    elif presentation.recording_state is FeatureRequestRecordingState.RECORDED:
        request = (
            f"request {presentation.request_id}"
            if presentation.request_id
            else "request"
        )
        recording = f"recorded · {request}  {marker}"
    else:
        recording = f"failed  {marker}"
    lines = [
        "Feature request",
        presentation.title,
        presentation.description,
        recording,
    ]
    if presentation.initial_status is not None:
        lines.append(f"initial status: {presentation.initial_status.value}")
        lines.append("recording did not provision the capability")
    if presentation.error:
        lines.append(presentation.error)
    text = "\n".join(line for line in lines if line)
    title_start = text.find(presentation.title)
    recording_start = text.find(recording)
    spans = [Span(0, len("Feature request"), "bold yellow")]
    if title_start >= 0:
        spans.append(Span(title_start, title_start + len(presentation.title), "bold"))
    if recording_start >= 0:
        spans.append(
            Span(
                recording_start,
                recording_start + len(recording),
                "bold red"
                if presentation.recording_state
                is FeatureRequestRecordingState.FAILED
                else "yellow",
            )
        )
    return Content(text, spans)


def _build_content(presentation: BuildPresentation) -> Content:
    heading = "── Trusted build"
    phase_lines: list[str] = []
    spans = [Span(0, len(heading), "bold green")]
    cursor = len(heading) + 1
    for phase in presentation.phases:
        marker = _STATE_MARKERS[phase.state]
        line = f"   {phase.name:<18} {marker}"
        phase_lines.append(line)
        marker_start = cursor + len(line) - len(marker)
        style = (
            "bold red"
            if phase.state is ActivityDisplayState.FAILED
            else "bold green"
            if phase.state is ActivityDisplayState.COMPLETED
            else "bold yellow"
            if phase.state is ActivityDisplayState.RUNNING
            else "dim"
        )
        spans.append(Span(marker_start, marker_start + len(marker), style))
        cursor += len(line) + 1
    text = heading + "\n" + "\n".join(phase_lines)
    if presentation.diagnostic:
        diagnostic = "\n".join(
            f"   {line}" for line in presentation.diagnostic.splitlines()
        )
        start = len(text) + 1
        text += "\n" + diagnostic
        spans.append(Span(start, len(text), "red"))
    return Content(text, spans)


def _operator_content(presentation: OperatorPresentation) -> Content:
    lines = ["Operator"]
    if presentation.command is not None:
        lines.append(f"  codexos> {presentation.command}")
    if presentation.output:
        lines.extend(f"  {line}" for line in presentation.output.splitlines())
    text = "\n".join(lines)
    spans = [Span(0, len("Operator"), "bold")]
    if presentation.command is not None:
        command_start = len("Operator\n  ")
        spans.append(
            Span(
                command_start,
                command_start + len("codexos> " + presentation.command),
                "cyan",
            )
        )
    return Content(text, spans)


def _lifecycle_content(presentation: LifecyclePresentation) -> Content:
    role = _ROLE_NAMES[presentation.role.value]
    marker = _STATE_MARKERS[presentation.state]
    text = f"{marker} {role} · {presentation.title}"
    if presentation.detail:
        text += "\n  " + presentation.detail.replace("\n", "\n  ")
    return Content(text, [Span(0, len(marker), "bold red")])


@dataclass(slots=True)
class _Confirmation:
    prompt: str
    completed: threading.Event
    accepted: bool = False


class _CommandExecutor:
    """Run authoritative console commands one at a time outside the UI loop."""

    def __init__(self, app: OperatorTui, console: OperatorConsole) -> None:
        self._app = app
        self._console = console
        self._commands: queue.Queue[str | None] = queue.Queue()
        self._thread = threading.Thread(
            target=self._run,
            name="codexos-operator-command",
            daemon=True,
        )
        self._lifecycle_lock = threading.Lock()
        self._started = False
        self._stop_requested = False

    def start(self) -> None:
        with self._lifecycle_lock:
            if self._started:
                return
            if self._stop_requested:
                raise RuntimeError("operator command executor is stopping")
            self._thread.start()
            self._started = True

    def submit(self, command: str) -> None:
        self._commands.put_nowait(command)

    def stop(self) -> None:
        self._app.cancel_confirmation()
        with self._lifecycle_lock:
            if self._stop_requested:
                return
            self._stop_requested = True
            self._commands.put_nowait(None)

    def join(self, timeout: float = 10.0) -> None:
        with self._lifecycle_lock:
            started = self._started
        if not started:
            return
        self._thread.join(timeout)
        if self._thread.is_alive():
            raise RuntimeError(
                "operator command executor did not stop within "
                f"{timeout:.1f} seconds"
            )

    @property
    def is_alive(self) -> bool:
        return self._thread.is_alive()

    def _run(self) -> None:
        try:
            self._console.show_startup()
            self._notify(self._app.command_finished, False)
            while True:
                command = self._commands.get()
                if command is None:
                    return
                should_exit = self._console.execute_line(command)
                self._notify(self._app.command_finished, should_exit)
                if should_exit:
                    return
        except Exception as error:
            self._notify(self._app.command_crashed, str(error))
        finally:
            self._console.shutdown()

    def _notify(self, callback: object, *arguments: object) -> None:
        if self._app.closing:
            return
        try:
            self._app.call_from_thread(callback, *arguments)
        except RuntimeError:
            # The terminal may already be restoring after an external failure.
            pass


class OperatorTui(App[None]):
    """Interactive display that delegates all command meaning to OperatorConsole."""

    CSS = """
    Screen {
        layout: vertical;
        background: $surface;
    }
    #status-header {
        height: 1;
        padding: 0 1;
        color: $text;
        background: $primary-background;
    }
    #activity-scroll {
        height: 1fr;
        scrollbar-size-vertical: 1;
        padding: 0 1;
    }
    #activity-rows {
        width: 1fr;
        height: auto;
        padding-bottom: 1;
    }
    .activity-row {
        width: 1fr;
        height: auto;
        margin-bottom: 1;
    }
    .activity-row:last-child {
        margin-bottom: 0;
    }
    .tool-header {
        width: 1fr;
        height: auto;
    }
    .tool-meta, .tool-detail-toggle {
        width: 1fr;
        height: auto;
        padding-left: 4;
        color: $text-muted;
    }
    .tool-detail-toggle {
        color: $accent;
    }
    .tool-detail-toggle:hover {
        text-style: underline;
    }
    .tool-detail {
        width: 1fr;
        height: auto;
        margin-top: 1;
        margin-left: 4;
        padding: 0 1;
        border-left: solid $primary;
        color: $text-muted;
    }
    .feature-request-row {
        width: 1fr;
        height: auto;
        padding: 0 1;
        border-left: thick $warning;
        background: $surface;
    }
    .lifecycle-content {
        height: auto;
        color: $error;
    }
    .notice-content {
        height: auto;
        color: $text-muted;
        text-style: italic;
    }
    #follow-indicator {
        height: 1;
        padding: 0 1;
        content-align: right middle;
        color: $warning;
    }
    #command-row {
        height: 3;
        padding: 0 1;
        border-top: solid $primary;
    }
    #command-prompt {
        width: auto;
        height: 1;
        margin-top: 1;
        padding-right: 1;
        color: $accent;
    }
    #command-input {
        width: 1fr;
        border: none;
        padding: 0;
        margin-top: 1;
        height: 1;
        background: transparent;
    }
    """

    BINDINGS = [
        Binding("pageup", "activity_page_up", "Scroll up", priority=True),
        Binding("pagedown", "activity_page_down", "Scroll down", priority=True),
        Binding("end", "activity_end", "Follow live", priority=True),
        Binding("escape", "pause_shortcut", "Pause", show=False, priority=True),
        Binding("ctrl+c", "safe_quit", "Quit", show=False, priority=True),
        Binding("ctrl+d", "safe_quit", "Quit", show=False, priority=True),
    ]

    def __init__(
        self,
        runtime: CodexOSRun,
        activity_stream: CodexActivityStream,
        *,
        codex_executable: str = "codex",
        codex_auth_file: str | Path | None = None,
        objective: str | None = None,
        reviewer_codex_executable: str = "codex",
        reviewer_auth_file: str | Path | None = None,
        git_recorder: GenerationGitRecorder | None = None,
        interview_repository: str | Path | None = None,
    ) -> None:
        super().__init__()
        self._runtime = runtime
        self._activity_stream = activity_stream
        self._operator_output: queue.SimpleQueue[str] = queue.SimpleQueue()
        self._model = OperatorActivityModel()
        self._follow = ActivityFollowState()
        self._busy = True
        self._frontend_closing = False
        self._confirmation: _Confirmation | None = None
        self._confirmation_lock = threading.Lock()
        self._pause_armed_deadline: float | None = None
        self._pause_armed_generation: int | None = None
        self._pause_arm_timer: Timer | None = None
        self._console = OperatorConsole(
            runtime,
            codex_executable=codex_executable,
            codex_auth_file=codex_auth_file,
            objective=objective,
            reviewer_codex_executable=reviewer_codex_executable,
            reviewer_auth_file=reviewer_auth_file,
            git_recorder=git_recorder,
            interview_repository=interview_repository,
            output_handler=self._receive_operator_output,
            confirmation_handler=self._request_confirmation,
        )
        self._executor = _CommandExecutor(self, self._console)

    @property
    def activity_model(self) -> OperatorActivityModel:
        return self._model

    @property
    def operator_console(self) -> OperatorConsole:
        return self._console

    @property
    def closing(self) -> bool:
        return self._frontend_closing

    def compose(self) -> ComposeResult:
        yield Static("", id="status-header")
        with VerticalScroll(id="activity-scroll"):
            yield ActivityTranscript()
        yield Static("", id="follow-indicator")
        with Horizontal(id="command-row"):
            yield Static(self._console.input_prompt.strip(), id="command-prompt")
            yield Input(id="command-input", disabled=True)

    def on_mount(self) -> None:
        self._refresh_header()
        self.call_after_refresh(self._sync_activity_anchor)
        self.set_interval(0.05, self._drain_activity)
        self.set_interval(0.25, self._refresh_header)
        self._model.begin_operator_block()
        self._executor.start()

    def on_unmount(self) -> None:
        self._frontend_closing = True
        self._disarm_pause(refresh=False)
        self._executor.stop()

    def _receive_operator_output(self, text: str) -> None:
        if self._frontend_closing:
            return
        self._operator_output.put(text)

    def _append_operator_output(self, text: str) -> None:
        if self._model.append_operator_output(text):
            self._activity_changed(1)

    def _request_confirmation(self, prompt: str) -> bool:
        confirmation = _Confirmation(prompt, threading.Event())
        with self._confirmation_lock:
            if self._frontend_closing:
                return False
            if self._confirmation is not None:
                return False
            self._confirmation = confirmation
        try:
            self.call_from_thread(self._show_confirmation, prompt)
        except RuntimeError:
            with self._confirmation_lock:
                self._confirmation = None
            return False
        confirmation.completed.wait()
        return confirmation.accepted

    def _show_confirmation(self, prompt: str) -> None:
        safe_prompt = safe_display_text(prompt).replace("\n", " / ").strip()
        self.query_one("#command-prompt", Static).update(safe_prompt)
        input_widget = self.query_one("#command-input", Input)
        input_widget.disabled = False
        input_widget.value = ""
        input_widget.focus()

    def cancel_confirmation(self) -> None:
        with self._confirmation_lock:
            confirmation = self._confirmation
            self._confirmation = None
        if confirmation is not None:
            confirmation.accepted = False
            confirmation.completed.set()

    def command_finished(self, should_exit: bool) -> None:
        changed = self._drain_operator_output()
        changed += int(self._model.finish_operator_block())
        if changed:
            self._activity_changed(changed)
        if should_exit:
            self._frontend_closing = True
            self.exit()
            return
        self._busy = False
        prompt = self.query_one("#command-prompt", Static)
        prompt.update(self._console.input_prompt.strip())
        input_widget = self.query_one("#command-input", Input)
        input_widget.disabled = False
        input_widget.focus()
        self._refresh_header()

    def command_crashed(self, error: str) -> None:
        changed = self._drain_operator_output()
        self._model.append_operator_output(
            "Operator console failed: " + safe_display_text(error)
        )
        self._model.finish_operator_block()
        self._activity_changed(changed + 1)
        self._frontend_closing = True
        self.exit()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        value = event.value
        event.input.value = ""
        self._disarm_pause(refresh=False)
        with self._confirmation_lock:
            confirmation = self._confirmation
            if confirmation is not None:
                self._confirmation = None
        if confirmation is not None:
            confirmation.accepted = value.strip() in {"y", "Y"}
            confirmation.completed.set()
            event.input.disabled = True
            self.query_one("#command-prompt", Static).update(
                self._console.input_prompt.strip()
            )
            return
        if self._busy:
            return
        if not value.strip():
            return
        self._submit_operator_command(value, event.input)

    def on_key(self, event: events.Key) -> None:
        if event.key != "escape" and self._pause_armed_deadline is not None:
            self._disarm_pause()

    def _submit_operator_command(
        self,
        value: str,
        input_widget: Input,
    ) -> None:
        interview_question = (
            self._console.exit_interview_state == "idle"
            and value.strip() not in {"end", "end-interview", "quit"}
        )
        self._model.begin_operator_block(None if interview_question else value)
        self._activity_changed(1)
        self._busy = True
        input_widget.disabled = True
        self._refresh_header()
        self._executor.submit(value)

    def _drain_activity(self) -> None:
        changed = self._drain_operator_output()
        events_to_render = self._activity_stream.drain()
        for activity_event in events_to_render:
            changed += int(self._model.consume(activity_event))
        if changed:
            self._activity_changed(changed)

    def _drain_operator_output(self) -> int:
        changed = 0
        while True:
            try:
                if self._model.append_operator_output(
                    self._operator_output.get_nowait()
                ):
                    changed += 1
            except queue.Empty:
                break
        return changed

    def _activity_changed(self, count: int) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        pane.anchor(False)
        self._follow.arrived(count)
        self.query_one(ActivityTranscript).reconcile(self._model.entries)
        self.call_after_refresh(self._sync_activity_anchor)
        self._refresh_follow_indicator()

    def _sync_activity_anchor(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        if self._follow.following and pane.max_scroll_y > 0:
            pane.anchor()
            return
        was_anchored = pane.is_anchored
        pane.anchor(False)
        if self._follow.following:
            # Textual 8.2.8 may leave a negative scroll position when anchored
            # content shrinks to fit; scroll_to() is a no-op without overflow.
            pane.scroll_target_y = 0
            pane.set_scroll(None, 0)
            if was_anchored:
                pane.refresh(layout=True)

    def _refresh_header(self) -> None:
        try:
            generation = self._runtime.generation_number
            state = self._runtime.state.name
            pending = sum(
                request.status == "pending"
                for request in self._runtime.feature_requests()
            )
        except Exception:
            generation = None
            state = "unknown"
            pending = 0
        if self._pause_armed_deadline is not None and (
            state != RuntimeState.RUNNING.name
            or generation != self._pause_armed_generation
        ):
            self._disarm_pause(refresh=False)
        busy = "operator busy" if self._busy else "operator idle"
        interview = self._console.exit_interview_state
        if interview == "answering":
            runtime_status = "EXIT INTERVIEW · Sol answering"
        elif interview == "idle":
            runtime_status = "EXIT INTERVIEW · Sol idle"
        elif interview == "available":
            runtime_status = f"{state} · exit interview available"
        else:
            runtime_status = f"{state} · Sol {self._console.codex_turn_state}"
        header = (
            f"CodexOS   {self._runtime.run_directory.name} · "
            f"gen {generation if generation is not None else '-'} · "
            f"{runtime_status} · {pending} pending · {busy}"
        )
        self.query_one("#status-header", Static).update(header)
        self._refresh_follow_indicator()

    def _refresh_follow_indicator(self) -> None:
        if self._follow.following:
            text = "live"
        else:
            text = f"↓ {self._follow.new_events} new   End: return to live"
        pause_hint = self._pause_hint()
        if pause_hint:
            text += f"   {pause_hint}"
        self.query_one("#follow-indicator", Static).update(text)

    def _pause_hint(self) -> str:
        if self._busy:
            return ""
        with self._confirmation_lock:
            if self._confirmation is not None:
                return ""
        if self._runtime.state is not RuntimeState.RUNNING:
            return ""
        if (
            self._pause_armed_deadline is not None
            and self._pause_armed_generation == self._runtime.generation_number
            and time.monotonic() <= self._pause_armed_deadline
        ):
            return "Esc again to confirm pause"
        return "Esc: pause"

    def _arm_pause(self) -> None:
        self._disarm_pause(refresh=False)
        self._pause_armed_deadline = (
            time.monotonic() + PAUSE_CONFIRMATION_SECONDS
        )
        self._pause_armed_generation = self._runtime.generation_number
        self._pause_arm_timer = self.set_timer(
            PAUSE_CONFIRMATION_SECONDS,
            self._pause_arm_expired,
        )
        self._refresh_follow_indicator()

    def _pause_arm_expired(self) -> None:
        deadline = self._pause_armed_deadline
        if deadline is None:
            return
        remaining = deadline - time.monotonic()
        if remaining > 0:
            self._pause_arm_timer = self.set_timer(
                remaining,
                self._pause_arm_expired,
            )
            return
        self._pause_arm_timer = None
        self._disarm_pause()

    def _disarm_pause(self, *, refresh: bool = True) -> None:
        timer = self._pause_arm_timer
        self._pause_arm_timer = None
        self._pause_armed_deadline = None
        self._pause_armed_generation = None
        if timer is not None:
            timer.stop()
        if refresh and self.is_mounted:
            self._refresh_follow_indicator()

    def _cancel_tui_confirmation(self) -> bool:
        with self._confirmation_lock:
            confirmation = self._confirmation
            self._confirmation = None
        if confirmation is None:
            return False
        confirmation.accepted = False
        confirmation.completed.set()
        input_widget = self.query_one("#command-input", Input)
        input_widget.disabled = True
        self.query_one("#command-prompt", Static).update(
            self._console.input_prompt.strip()
        )
        self._disarm_pause()
        return True

    def action_activity_page_up(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.anchor(False)
        pane.scroll_page_up(animate=False)
        self._refresh_follow_indicator()

    def action_activity_page_down(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.anchor(False)
        pane.scroll_page_down(animate=False)
        self._refresh_follow_indicator()

    def action_activity_end(self) -> None:
        self._follow.return_to_live()
        self._sync_activity_anchor()
        self._refresh_follow_indicator()

    def action_pause_shortcut(self) -> None:
        if self._cancel_tui_confirmation():
            return
        if self._busy or self._runtime.state is not RuntimeState.RUNNING:
            self._disarm_pause()
            return
        generation = self._runtime.generation_number
        if (
            self._pause_armed_deadline is not None
            and self._pause_armed_generation == generation
            and time.monotonic() <= self._pause_armed_deadline
        ):
            self._disarm_pause(refresh=False)
            self._submit_operator_command(
                "pause",
                self.query_one("#command-input", Input),
            )
            return
        self._arm_pause()

    def action_safe_quit(self) -> None:
        self._disarm_pause(refresh=False)
        if self._busy:
            return
        input_widget = self.query_one("#command-input", Input)
        input_widget.value = ""
        self._busy = True
        input_widget.disabled = True
        self._executor.submit("quit")

    def on_mouse_scroll_up(self, event: events.MouseScrollUp) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.anchor(False)
        self._refresh_follow_indicator()

    def on_mouse_scroll_down(self, event: events.MouseScrollDown) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.anchor(False)
        self._refresh_follow_indicator()


def run_operator_tui(
    runtime: CodexOSRun,
    activity_stream: CodexActivityStream,
    *,
    git_recorder: GenerationGitRecorder | None = None,
    interview_repository: str | Path | None = None,
) -> None:
    """Run the full-screen frontend and restore the terminal on every exit."""
    app: OperatorTui | None = None
    try:
        app = OperatorTui(
            runtime,
            activity_stream,
            git_recorder=git_recorder,
            interview_repository=interview_repository,
        )
        app.run()
    finally:
        if app is None:
            runtime.stop()
        else:
            try:
                app._executor.stop()
                app._executor.join()
            finally:
                app.operator_console.shutdown()

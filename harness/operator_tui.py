"""Full-screen interactive frontend for the trusted operator console."""

from __future__ import annotations

import queue
import threading
import time
from dataclasses import dataclass
from pathlib import Path

from textual import events
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical, VerticalScroll
from textual.content import Content, Span
from textual.widgets import Input, Static
from textual.timer import Timer

from .codex_activity import CodexActivityStream
from .generation_git import GenerationGitRecorder
from .generation_runtime import CodexOSRun, RuntimeState
from .operator_console import OperatorConsole
from .operator_tui_model import (
    ActivityDisplayEntry,
    ActivityFollowState,
    OperatorActivityModel,
    safe_display_text,
)


PAUSE_CONFIRMATION_SECONDS = 2.5

_ACTIVITY_STYLES = {
    "implementor": "bold cyan",
    "reviewer": "bold magenta",
    "reasoning": "italic bright_blue",
    "tool": "bold yellow",
    "tool-failed": "bold red",
    "build": "bold green",
    "operator": "bold white",
    "harness": "bold green",
}


class ActivityRow(Static):
    """One mounted transcript widget for one stable logical activity key."""

    def __init__(self, entry: ActivityDisplayEntry) -> None:
        self._entry = entry
        super().__init__(_render_activity_entry(entry), markup=False)

    @property
    def entry(self) -> ActivityDisplayEntry:
        return self._entry

    def update_entry(self, entry: ActivityDisplayEntry) -> None:
        if entry == self._entry:
            return
        self._entry = entry
        self.update(_render_activity_entry(entry))


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
                row = ActivityRow(entry)
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


def _render_activity_entry(entry: ActivityDisplayEntry) -> Content:
    parts = [entry.label]
    spans = [
        Span(
            0,
            len(entry.label),
            _ACTIVITY_STYLES.get(entry.category, "bold"),
        )
    ]
    if entry.body:
        parts.append(
            "\n" + "\n".join(
                f"  {line}" for line in entry.body.splitlines() or [""]
            )
        )
    return Content("".join(parts), spans)


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
    }
    ActivityRow {
        width: 1fr;
        height: auto;
        margin-bottom: 1;
    }
    ActivityRow:last-child {
        margin-bottom: 0;
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
            yield Static("codexos>", id="command-prompt")
            yield Input(id="command-input", disabled=True)

    def on_mount(self) -> None:
        self._refresh_header()
        self.query_one("#activity-scroll", VerticalScroll).anchor()
        self.set_interval(0.05, self._drain_activity)
        self.set_interval(0.25, self._refresh_header)
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
        self._model.append_operator_output(text)
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
        if should_exit:
            self._frontend_closing = True
            self.exit()
            return
        self._busy = False
        prompt = self.query_one("#command-prompt", Static)
        prompt.update("codexos>")
        input_widget = self.query_one("#command-input", Input)
        input_widget.disabled = False
        input_widget.focus()
        self._refresh_header()

    def command_crashed(self, error: str) -> None:
        self._model.append_operator_output(
            "Operator console failed: " + safe_display_text(error)
        )
        self._activity_changed(1)
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
            self.query_one("#command-prompt", Static).update("codexos>")
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
        self._model.append_operator_output(f"codexos> {value}")
        self._activity_changed(1)
        self._busy = True
        input_widget.disabled = True
        self._refresh_header()
        self._executor.submit(value)

    def _drain_activity(self) -> None:
        operator_count = 0
        while True:
            try:
                self._model.append_operator_output(
                    self._operator_output.get_nowait()
                )
                operator_count += 1
            except queue.Empty:
                break
        events_to_render = self._activity_stream.drain()
        if not events_to_render and not operator_count:
            return
        for activity_event in events_to_render:
            self._model.consume(activity_event)
        self._activity_changed(len(events_to_render) + operator_count)

    def _activity_changed(self, count: int) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.arrived(count)
        self.query_one(ActivityTranscript).reconcile(self._model.entries)
        if self._follow.following:
            pane.anchor()
        else:
            pane.anchor(False)
        self._refresh_follow_indicator()

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
        busy = "command busy" if self._busy else "command idle"
        header = (
            f"{self._runtime.run_directory.name}   "
            f"generation {generation if generation is not None else '-'}   "
            f"{state}   Sol: {self._console.codex_turn_state}   "
            f"features: {pending} pending   {busy}"
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
        self.query_one("#command-prompt", Static).update("codexos>")
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
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.return_to_live()
        pane.anchor()
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
) -> None:
    """Run the full-screen frontend and restore the terminal on every exit."""
    app: OperatorTui | None = None
    try:
        app = OperatorTui(runtime, activity_stream, git_recorder=git_recorder)
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

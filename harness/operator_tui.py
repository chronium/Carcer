"""Full-screen interactive frontend for the trusted operator console."""

from __future__ import annotations

import queue
import threading
from dataclasses import dataclass
from pathlib import Path

from textual import events
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, VerticalScroll
from textual.content import Content, Span
from textual.widgets import Input, Static

from .codex_activity import CodexActivityStream
from .generation_git import GenerationGitRecorder
from .generation_runtime import CodexOSRun
from .operator_console import OperatorConsole
from .operator_tui_model import (
    ActivityFollowState,
    OperatorActivityModel,
    safe_display_text,
)


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
        self._started = False

    def start(self) -> None:
        self._started = True
        self._thread.start()

    def submit(self, command: str) -> None:
        self._commands.put_nowait(command)

    def stop(self) -> None:
        self._commands.put_nowait(None)
        self._app.cancel_confirmation()

    def join(self, timeout: float = 10.0) -> None:
        if self._started:
            self._thread.join(timeout)

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
    #activity {
        width: 1fr;
        height: auto;
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
        self._closing = False
        self._confirmation: _Confirmation | None = None
        self._confirmation_lock = threading.Lock()
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
        return self._closing

    def compose(self) -> ComposeResult:
        yield Static("", id="status-header")
        with VerticalScroll(id="activity-scroll"):
            yield Static("", id="activity", markup=False)
        yield Static("", id="follow-indicator")
        with Horizontal(id="command-row"):
            yield Static("codexos>", id="command-prompt")
            yield Input(id="command-input", disabled=True)

    def on_mount(self) -> None:
        self._refresh_header()
        self.set_interval(0.05, self._drain_activity)
        self.set_interval(0.25, self._refresh_header)
        self._executor.start()

    def on_unmount(self) -> None:
        self._closing = True
        self.cancel_confirmation()
        self._executor.stop()
        self._executor.join()

    def _receive_operator_output(self, text: str) -> None:
        if self._closing:
            return
        self._operator_output.put(text)

    def _append_operator_output(self, text: str) -> None:
        self._model.append_operator_output(text)
        self._activity_changed(1)

    def _request_confirmation(self, prompt: str) -> bool:
        confirmation = _Confirmation(prompt, threading.Event())
        with self._confirmation_lock:
            if self._closing:
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
            self._closing = True
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
        self._closing = True
        self.exit()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        value = event.value
        event.input.value = ""
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
        self._model.append_operator_output(f"codexos> {value}")
        self._activity_changed(1)
        self._busy = True
        event.input.disabled = True
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
        saved_y = pane.scroll_y
        self._follow.arrived(count)
        self.query_one("#activity", Static).update(self._render_activity())
        if self._follow.following:
            self.call_after_refresh(pane.scroll_end, animate=False, immediate=True)
        else:
            self.call_after_refresh(
                pane.scroll_to,
                y=saved_y,
                animate=False,
                immediate=True,
            )
        self._refresh_follow_indicator()

    def _render_activity(self) -> Content:
        parts: list[str] = []
        spans: list[Span] = []
        length = 0
        styles = {
            "implementor": "bold cyan",
            "reviewer": "bold magenta",
            "reasoning": "italic bright_blue",
            "tool": "bold yellow",
            "tool-failed": "bold red",
            "build": "bold green",
            "operator": "bold white",
            "harness": "bold green",
        }
        for index, entry in enumerate(self._model.entries):
            if index:
                parts.append("\n\n")
                length += 2
            parts.append(entry.label)
            spans.append(
                Span(
                    length,
                    length + len(entry.label),
                    styles.get(entry.category, "bold"),
                )
            )
            length += len(entry.label)
            if entry.body:
                body = "\n" + "\n".join(
                    f"  {line}" for line in entry.body.splitlines() or [""]
                )
                parts.append(body)
                length += len(body)
        return Content("".join(parts), spans)

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
        self.query_one("#follow-indicator", Static).update(text)

    def action_activity_page_up(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.scroll_page_up(animate=False)
        self._refresh_follow_indicator()

    def action_activity_page_down(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        pane.scroll_page_down(animate=False)
        self._refresh_follow_indicator()

    def action_activity_end(self) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.return_to_live()
        pane.scroll_end(animate=False, immediate=True)
        self._refresh_follow_indicator()

    def action_safe_quit(self) -> None:
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
        self._refresh_follow_indicator()

    def on_mouse_scroll_down(self, event: events.MouseScrollDown) -> None:
        pane = self.query_one("#activity-scroll", VerticalScroll)
        self._follow.scrolled(pane.scroll_y)
        self._refresh_follow_indicator()


def run_operator_tui(
    runtime: CodexOSRun,
    activity_stream: CodexActivityStream,
    *,
    git_recorder: GenerationGitRecorder | None = None,
) -> None:
    """Run the full-screen frontend and restore the terminal on every exit."""
    app = OperatorTui(runtime, activity_stream, git_recorder=git_recorder)
    try:
        app.run()
    finally:
        app.cancel_confirmation()
        app._executor.stop()
        app._executor.join()
        app.operator_console.shutdown()

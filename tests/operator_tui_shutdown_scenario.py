"""Bounded subprocess scenarios for the Textual shutdown lifecycle tests."""

from __future__ import annotations

import argparse
import tempfile
import threading
from pathlib import Path
from unittest.mock import Mock

from textual.widgets import Input

from harness.codex_activity import CodexActivityStream
from harness.generation_runtime import RuntimeState
from harness.operator_tui import OperatorTui
from harness.codex_generation_worker import CodexGenerationSessionMode


class _RetainedSession:
    def __init__(self) -> None:
        self.mode = CodexGenerationSessionMode.RETAINED_AT_GATE
        self.healthy = True
        self.closed = False
        self.interrupted = threading.Event()

    def interrupt_turn(self, timeout_seconds: float) -> None:
        self.interrupted.set()

    def close(self) -> None:
        self.closed = True
        self.interrupted.set()


def _runtime(path: Path, state: RuntimeState) -> tuple[Mock, list[str]]:
    runtime = Mock()
    runtime.run_directory = path
    runtime.state = state
    runtime.generation_number = 6
    runtime.active_pid = 1234 if state is RuntimeState.RUNNING else None
    runtime.pending_generation_finish = None
    runtime.previous_handoff = None
    runtime.feature_requests.return_value = ()
    runtime.archived_generations.return_value = ()
    transitions: list[str] = []

    def stop() -> None:
        if runtime.state is not RuntimeState.STOPPED:
            transitions.append("stopped")
            runtime.state = RuntimeState.STOPPED
            runtime.active_pid = None

    runtime.stop.side_effect = stop
    return runtime, transitions


class _ScenarioTui(OperatorTui):
    def __init__(self, *arguments: object, scenario: str, **keywords: object) -> None:
        self._scenario = scenario
        super().__init__(*arguments, **keywords)  # type: ignore[arg-type]

    def on_mount(self) -> None:
        super().on_mount()
        self.set_timer(0.05, self._exercise_scenario)

    def _exercise_scenario(self) -> None:
        if self._busy:
            self.set_timer(0.025, self._exercise_scenario)
            return
        if self._scenario == "ctrl-c":
            self.action_safe_quit()
            return
        input_widget = self.query_one("#command-input", Input)
        input_widget.value = "quit"
        self.on_input_submitted(Input.Submitted(input_widget, "quit"))
        if self._scenario == "pending-confirmation":
            self.set_timer(0.025, self._exit_pending_confirmation)

    def _exit_pending_confirmation(self) -> None:
        if self._confirmation is None:
            self.set_timer(0.025, self._exit_pending_confirmation)
            return
        self.exit()


def _exercise(scenario: str, *, terminal: bool = False) -> None:
    state = (
        RuntimeState.RUNNING
        if scenario == "pending-confirmation"
        else RuntimeState.AWAITING_NEXT_GENERATION
    )
    with tempfile.TemporaryDirectory() as temporary:
        runtime, transitions = _runtime(Path(temporary), state)
        app = _ScenarioTui(
            runtime,
            CodexActivityStream(),
            scenario=scenario,
        )
        if scenario == "command-failure":
            app.operator_console.execute_line = Mock(
                side_effect=RuntimeError("simulated command failure")
            )
        retained: _RetainedSession | None = None
        interview_turn: threading.Thread | None = None
        if scenario in {"interview-idle", "interview-active"}:
            retained = _RetainedSession()
            app.operator_console._session = retained  # type: ignore[assignment]
            app.operator_console._session_generation = 6
            app.operator_console._interview_open = True
            runtime.pending_generation_finish = object()
            if scenario == "interview-active":
                interview_turn = threading.Thread(
                    target=retained.interrupted.wait,
                    name="synthetic-exit-interview-turn",
                )
                interview_turn.start()
                app.operator_console._turn_thread = interview_turn

        app.run(headless=not terminal, size=(90, 24))

        # This is deliberately outside Textual's event loop and terminal lifecycle.
        app._executor.stop()
        app._executor.join(2.0)
        app.operator_console.shutdown()
        if app._executor.is_alive:
            raise AssertionError("operator command executor survived TUI exit")
        if transitions != ["stopped"]:
            raise AssertionError(
                f"runtime had unexpected meaningful stop transitions: {transitions}"
            )
        if retained is not None and not retained.closed:
            raise AssertionError("retained interview session survived TUI exit")
        if interview_turn is not None and interview_turn.is_alive():
            raise AssertionError("interview turn survived TUI exit")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "scenario",
        choices=(
            "quit",
            "ctrl-c",
            "command-failure",
            "pending-confirmation",
            "interview-idle",
            "interview-active",
        ),
    )
    parser.add_argument("--terminal", action="store_true")
    arguments = parser.parse_args()
    _exercise(arguments.scenario, terminal=arguments.terminal)


if __name__ == "__main__":
    main()

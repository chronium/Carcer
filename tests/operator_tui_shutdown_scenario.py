"""Bounded subprocess scenarios for the Textual shutdown lifecycle tests."""

from __future__ import annotations

import argparse
import tempfile
from pathlib import Path
from unittest.mock import Mock

from textual.widgets import Input

from harness.codex_activity import CodexActivityStream
from harness.generation_runtime import RuntimeState
from harness.operator_tui import OperatorTui


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


def _exercise(scenario: str) -> None:
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

        app.run(headless=True, size=(90, 24))

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


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "scenario",
        choices=("quit", "ctrl-c", "command-failure", "pending-confirmation"),
    )
    arguments = parser.parse_args()
    _exercise(arguments.scenario)


if __name__ == "__main__":
    main()

import contextlib
import io
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from textual.widgets import Input
from textual.containers import VerticalScroll

from harness.codex_activity import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
)
from harness.generation_runtime import RuntimeState
from harness.operator_console import main
from harness.operator_tui import OperatorTui, run_operator_tui


def _runtime(path: Path, state: RuntimeState = RuntimeState.STOPPED) -> Mock:
    runtime = Mock()
    runtime.run_directory = path
    runtime.state = state
    runtime.generation_number = 6 if state is not RuntimeState.STOPPED else None
    runtime.active_pid = 1234 if state is RuntimeState.RUNNING else None
    runtime.pending_generation_finish = None
    runtime.previous_handoff = None
    runtime.feature_requests.return_value = ()
    runtime.archived_generations.return_value = ()
    return runtime


class OperatorTuiInteractionTests(unittest.IsolatedAsyncioTestCase):
    async def test_command_output_and_activity_coexist_without_input_corruption(
        self,
    ) -> None:
        runtime = _runtime(Path("/tmp/tui-test"))
        stream = CodexActivityStream()
        app = OperatorTui(runtime, stream)
        async with app.run_test(size=(100, 30)) as pilot:
            await pilot.pause(0.15)
            input_widget = app.query_one("#command-input", Input)
            await pilot.click("#command-input")
            await pilot.press("s", "t", "a", "t")
            stream.publish(
                6,
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.AGENT_TEXT_DELTA,
                {"text": "Working asynchronously."},
                thread_id="thread",
                turn_id="turn",
                item_id="message",
            )
            await pilot.pause(0.15)
            self.assertEqual(input_widget.value, "stat")
            await pilot.resize_terminal(120, 36)
            self.assertEqual(input_widget.value, "stat")
            await pilot.press("u", "s", "enter")
            await pilot.pause(0.15)
            rendered = app.activity_model.render_text()
            self.assertIn("Working asynchronously.", rendered)
            self.assertIn("State: STOPPED", rendered)
            app.exit()

        self.assertEqual(runtime.stop.call_count, 1)

    async def test_manual_scroll_accumulates_new_activity_and_end_follows(self) -> None:
        runtime = _runtime(Path("/tmp/tui-scroll"))
        stream = CodexActivityStream()
        app = OperatorTui(runtime, stream)
        async with app.run_test(size=(80, 18)) as pilot:
            await pilot.pause(0.15)
            for index in range(50):
                stream.publish(
                    6,
                    CodexActivityRole.IMPLEMENTOR,
                    CodexActivityKind.AGENT_MESSAGE,
                    {"text": f"message {index} " + "x" * 80},
                    thread_id="thread",
                    turn_id="turn",
                    item_id=f"message-{index}",
                )
            await pilot.pause(0.2)
            await pilot.press("pageup")
            await pilot.pause(0.05)
            pane = app.query_one("#activity-scroll", VerticalScroll)
            reading_position = pane.scroll_y
            stream.publish(
                6,
                CodexActivityRole.REVIEWER,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": "new reviewer message"},
                thread_id="review-thread",
                turn_id="review-turn",
                item_id="review-message",
            )
            await pilot.pause(0.15)
            self.assertFalse(app._follow.following)
            self.assertGreater(app._follow.new_events, 0)
            self.assertAlmostEqual(pane.scroll_y, reading_position)
            await pilot.press("pagedown")
            self.assertFalse(app._follow.following)
            await pilot.press("end")
            self.assertTrue(app._follow.following)
            self.assertEqual(app._follow.new_events, 0)
            app.exit()

    async def test_confirmation_stays_inside_tui_and_defaults_to_no(self) -> None:
        runtime = _runtime(Path("/tmp/tui-confirm"), RuntimeState.RUNNING)
        app = OperatorTui(runtime, CodexActivityStream())
        async with app.run_test(size=(90, 24)) as pilot:
            await pilot.pause(0.15)
            await pilot.click("#command-input")
            await pilot.press("q", "u", "i", "t", "enter")
            await pilot.pause(0.15)
            prompt = str(app.query_one("#command-prompt").render())
            self.assertIn("Stop the run without archiving", prompt)
            await pilot.press("enter")
            await pilot.pause(0.15)
            self.assertIn("Quit cancelled.", app.activity_model.render_text())
            self.assertFalse(app._busy)
            app.exit()

class _TtyStream(io.StringIO):
    def isatty(self) -> bool:
        return True


class OperatorTuiSelectionTests(unittest.TestCase):
    def test_failed_tui_initialization_still_stops_runtime(self) -> None:
        runtime = _runtime(Path("/tmp/tui-failure"))
        with patch.object(
            OperatorTui,
            "run",
            side_effect=RuntimeError("simulated TUI initialization failure"),
        ):
            with self.assertRaisesRegex(RuntimeError, "simulated TUI"):
                run_operator_tui(runtime, CodexActivityStream())
        runtime.stop.assert_called_once_with()

    def test_plain_mode_does_not_create_unused_activity_stream(self) -> None:
        runtime = _runtime(Path("/tmp/plain-selection"))
        observability = Mock()
        with (
            tempfile.TemporaryDirectory() as temporary,
            patch(
                "harness.operator_console.ExperimentObservability",
                return_value=observability,
            ),
            patch(
                "harness.operator_console.CodexOSRun", return_value=runtime
            ) as runtime_type,
            patch("harness.operator_console.OperatorConsole") as console_type,
        ):
            result = main(
                [
                    "--run-directory",
                    temporary,
                    "--initial-iso",
                    str(Path(temporary) / "seed.iso"),
                    "--plain",
                ],
                _TtyStream(),
                _TtyStream(),
            )
        self.assertEqual(result, 0)
        self.assertIsNone(runtime_type.call_args.kwargs["activity_stream"])
        console_type.return_value.run.assert_called_once_with()

    def test_interactive_default_passes_one_stream_to_runtime_and_tui(self) -> None:
        runtime = _runtime(Path("/tmp/tui-selection"))
        observability = Mock()
        environment = {**os.environ, "TERM": "xterm-256color"}
        with (
            tempfile.TemporaryDirectory() as temporary,
            patch.dict(os.environ, environment, clear=True),
            patch(
                "harness.operator_console.ExperimentObservability",
                return_value=observability,
            ),
            patch(
                "harness.operator_console.CodexOSRun", return_value=runtime
            ) as runtime_type,
            patch("harness.operator_tui.run_operator_tui") as run_tui,
        ):
            result = main(
                [
                    "--run-directory",
                    temporary,
                    "--initial-iso",
                    str(Path(temporary) / "seed.iso"),
                ],
                _TtyStream(),
                _TtyStream(),
            )
        stream = runtime_type.call_args.kwargs["activity_stream"]
        self.assertIsInstance(stream, CodexActivityStream)
        self.assertIs(run_tui.call_args.args[1], stream)
        self.assertEqual(result, 0)

    def test_explicit_tui_rejects_noninteractive_streams(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                main(
                    [
                        "--run-directory",
                        "/tmp/tui-nontty",
                        "--initial-iso",
                        "/tmp/seed.iso",
                        "--tui",
                    ],
                    io.StringIO(),
                    io.StringIO(),
                )
        self.assertEqual(caught.exception.code, 2)
if __name__ == "__main__":
    unittest.main()

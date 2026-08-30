import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from textual.app import App, ComposeResult
from textual.widgets import Input
from textual.containers import VerticalScroll

from harness.codex_activity import (
    CodexActivityEvent,
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
)
from harness.generation_runtime import RuntimeState
from harness.operator_console import main
from harness.operator_tui import (
    ActivityTranscript,
    OperatorTui,
    run_operator_tui,
)
from harness.operator_tui_model import (
    ActivityDisplayEntry,
    OperatorActivityModel,
)


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


class _TranscriptApp(App[None]):
    CSS = """
    #activity-scroll { height: 1fr; }
    #activity-rows { width: 1fr; height: auto; }
    ActivityRow { width: 1fr; height: auto; margin-bottom: 1; }
    """

    def compose(self) -> ComposeResult:
        with VerticalScroll(id="activity-scroll"):
            yield ActivityTranscript()


def _entry(index: int, *, body: str | None = None) -> ActivityDisplayEntry:
    return ActivityDisplayEntry(
        f"entry:{index}",
        f"Entry {index}",
        "operator",
        body if body is not None else f"body {index}",
    )


class ActivityTranscriptTests(unittest.IsolatedAsyncioTestCase):
    async def test_keyed_rows_mount_update_and_remove_incrementally(self) -> None:
        app = _TranscriptApp()
        async with app.run_test(size=(90, 24)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            initial = (_entry(1), _entry(2), _entry(3))
            transcript.reconcile(initial)
            await pilot.pause()

            self.assertEqual(
                transcript.row_keys,
                tuple(item.key for item in initial),
            )
            first, second, third = transcript.rows
            self.assertEqual(tuple(transcript.children), (first, second, third))

            fourth = _entry(4)
            transcript.reconcile((*initial, fourth))
            await pilot.pause()
            self.assertEqual(transcript.rows[:3], (first, second, third))
            self.assertIs(transcript.row_for(fourth.key), transcript.rows[-1])

            updated_second = _entry(2, body="updated body")
            with (
                patch.object(
                    first,
                    "update_entry",
                    wraps=first.update_entry,
                ) as first_update,
                patch.object(
                    third,
                    "update_entry",
                    wraps=third.update_entry,
                ) as third_update,
            ):
                transcript.reconcile(
                    (initial[0], updated_second, initial[2], fourth)
                )
                first_update.assert_not_called()
                third_update.assert_not_called()
            self.assertIs(transcript.row_for(updated_second.key), second)
            self.assertEqual(second.entry, updated_second)

            transcript.reconcile((initial[2], updated_second, initial[0], fourth))
            self.assertEqual(
                tuple(transcript.children),
                (third, second, first, transcript.rows[-1]),
            )
            self.assertIs(transcript.row_for(initial[0].key), first)

            transcript.reconcile((updated_second, initial[2], fourth))
            await pilot.pause()
            self.assertNotIn(initial[0].key, transcript.row_keys)
            self.assertFalse(first.is_attached)
            self.assertEqual(
                tuple(transcript.children),
                (second, third, transcript.rows[-1]),
            )

    async def test_scrollback_trimming_unmounts_discarded_rows(self) -> None:
        app = _TranscriptApp()
        model = OperatorActivityModel(max_entries=4, max_bytes=400)
        async with app.run_test(size=(90, 24)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            for index in range(3):
                model.append_operator_output(f"line {index}")
            transcript.reconcile(model.entries)
            await pilot.pause()
            oldest_key = model.entries[0].key
            oldest_row = transcript.row_for(oldest_key)

            for index in range(3, 10):
                model.append_operator_output(f"line {index}")
            transcript.reconcile(model.entries)
            await pilot.pause()

            self.assertEqual(
                transcript.row_keys,
                tuple(entry.key for entry in model.entries),
            )
            self.assertLessEqual(len(transcript.rows), 4)
            self.assertEqual(transcript.row_keys[0], "scrollback:discarded")
            self.assertEqual(
                tuple(row.entry.key for row in transcript.children),
                transcript.row_keys,
            )
            self.assertFalse(oldest_row.is_attached)

    async def test_hundreds_of_rows_and_tail_streaming_preserve_row_identity(self) -> None:
        app = _TranscriptApp()
        model = OperatorActivityModel()
        for index in range(299):
            model.append_operator_output(f"history {index}")
        model.consume(
            CodexActivityEvent(
                1,
                6,
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.AGENT_TEXT_DELTA,
                {"text": "streamed update 0"},
                thread_id="thread",
                turn_id="turn",
                item_id="tail",
            )
        )
        async with app.run_test(size=(100, 30)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            entries = model.entries
            transcript.reconcile(entries)
            await pilot.pause()
            first = transcript.row_for(entries[0].key)
            middle = transcript.row_for(entries[150].key)
            tail = transcript.row_for(entries[-1].key)

            with (
                patch.object(
                    first,
                    "update_entry",
                    wraps=first.update_entry,
                ) as first_update,
                patch.object(
                    middle,
                    "update_entry",
                    wraps=middle.update_entry,
                ) as middle_update,
            ):
                for update in range(1, 101):
                    model.consume(
                        CodexActivityEvent(
                            update + 1,
                            6,
                            CodexActivityRole.IMPLEMENTOR,
                            CodexActivityKind.AGENT_TEXT_DELTA,
                            {"text": f"\nstreamed update {update}"},
                            thread_id="thread",
                            turn_id="turn",
                            item_id="tail",
                        )
                    )
                    transcript.reconcile(model.entries)
                first_update.assert_not_called()
                middle_update.assert_not_called()

            self.assertIs(transcript.row_for(entries[0].key), first)
            self.assertIs(transcript.row_for(entries[150].key), middle)
            self.assertIs(transcript.row_for(entries[-1].key), tail)
            self.assertIn("streamed update 100", tail.entry.body)
            self.assertEqual(len(transcript.rows), len(entries))


class OperatorTuiInteractionTests(unittest.IsolatedAsyncioTestCase):
    async def test_two_escape_presses_submit_authoritative_pause(self) -> None:
        runtime = _runtime(Path("/tmp/tui-escape-pause"), RuntimeState.RUNNING)
        app = OperatorTui(runtime, CodexActivityStream())
        submit = Mock()
        app._executor.submit = submit
        async with app.run_test(size=(90, 24)) as pilot:
            await pilot.pause(0.15)
            input_widget = app.query_one("#command-input", Input)
            input_widget.value = "partially typed"

            await pilot.press("escape")
            submit.assert_not_called()
            runtime.pause.assert_not_called()
            self.assertEqual(input_widget.value, "partially typed")
            self.assertIn(
                "Esc again to confirm pause",
                str(app.query_one("#follow-indicator").render()),
            )

            await pilot.press("escape")
            submit.assert_called_once_with("pause")
            runtime.pause.assert_not_called()
            self.assertEqual(input_widget.value, "partially typed")
            app.exit()

        app._executor.join(2.0)

    async def test_escape_pause_expires_and_typing_disarms_it(self) -> None:
        runtime = _runtime(Path("/tmp/tui-escape-timeout"), RuntimeState.RUNNING)
        with patch("harness.operator_tui.PAUSE_CONFIRMATION_SECONDS", 0.05):
            app = OperatorTui(runtime, CodexActivityStream())
            submit = Mock()
            app._executor.submit = submit
            async with app.run_test(size=(90, 24)) as pilot:
                await pilot.pause(0.15)
                input_widget = app.query_one("#command-input", Input)
                input_widget.value = "keep"
                await pilot.press("escape")
                await pilot.pause(0.1)
                self.assertIsNone(app._pause_armed_deadline)
                self.assertIn(
                    "Esc: pause",
                    str(app.query_one("#follow-indicator").render()),
                )
                submit.assert_not_called()

                await pilot.press("escape")
                await pilot.press("x")
                self.assertIsNone(app._pause_armed_deadline)
                self.assertEqual(input_widget.value, "keepx")
                submit.assert_not_called()
                runtime.pause.assert_not_called()
                app.exit()

            app._executor.join(2.0)

    async def test_escape_pause_is_disarmed_by_runtime_transition(self) -> None:
        runtime = _runtime(Path("/tmp/tui-escape-state"), RuntimeState.RUNNING)
        app = OperatorTui(runtime, CodexActivityStream())
        submit = Mock()
        app._executor.submit = submit
        async with app.run_test(size=(90, 24)) as pilot:
            await pilot.pause(0.15)
            await pilot.press("escape")
            self.assertIsNotNone(app._pause_armed_deadline)

            runtime.generation_number += 1
            app._refresh_header()
            self.assertIsNone(app._pause_armed_deadline)

            for state in (
                RuntimeState.PAUSED,
                RuntimeState.AWAITING_NEXT_GENERATION,
                RuntimeState.STOPPED,
            ):
                runtime.state = state
                app._refresh_header()
                await pilot.press("escape")
                submit.assert_not_called()
                self.assertNotIn(
                    "Esc: pause",
                    str(app.query_one("#follow-indicator").render()),
                )
            runtime.pause.assert_not_called()
            app.exit()

        app._executor.join(2.0)

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

        app._executor.join(2.0)
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
            pane = app.query_one("#activity-scroll", VerticalScroll)
            self.assertTrue(pane.is_vertical_scroll_end)
            await pilot.press("pageup")
            await pilot.pause(0.05)
            input_widget = app.query_one("#command-input", Input)
            reading_position = pane.scroll_y
            self.assertIs(app.focused, input_widget)
            stream.publish(
                6,
                CodexActivityRole.REVIEWER,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": "new reviewer message"},
                thread_id="review-thread",
                turn_id="review-turn",
                item_id="review-message",
            )
            app._drain_activity()
            await pilot.pause()
            self.assertFalse(app._follow.following)
            self.assertGreater(app._follow.new_events, 0)
            self.assertAlmostEqual(pane.scroll_y, reading_position)
            transcript = app.query_one(ActivityTranscript)
            reviewer_key = app.activity_model.entries[-1].key
            reviewer_row = transcript.row_for(reviewer_key)

            stream.publish(
                6,
                CodexActivityRole.REVIEWER,
                CodexActivityKind.AGENT_MESSAGE,
                {
                    "text": "new reviewer message\n"
                    + "streamed below the viewport\n" * 20
                },
                thread_id="review-thread",
                turn_id="review-turn",
                item_id="review-message",
            )
            app._drain_activity()
            await pilot.pause()
            self.assertIs(transcript.row_for(reviewer_key), reviewer_row)
            self.assertAlmostEqual(pane.scroll_y, reading_position)
            self.assertGreaterEqual(app._follow.new_events, 2)
            self.assertIs(app.focused, input_widget)
            await pilot.press("pagedown")
            self.assertFalse(app._follow.following)
            self.assertIs(app.focused, input_widget)
            await pilot.press("end")
            self.assertTrue(app._follow.following)
            self.assertEqual(app._follow.new_events, 0)
            self.assertTrue(pane.is_vertical_scroll_end)
            self.assertIs(app.focused, input_widget)
            app.exit()

        app._executor.join(2.0)

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
            await pilot.press("escape")
            await pilot.pause(0.15)
            self.assertIn("Quit cancelled.", app.activity_model.render_text())
            self.assertFalse(app._busy)
            self.assertIsNone(app._pause_armed_deadline)
            runtime.pause.assert_not_called()
            app.exit()

        app._executor.join(2.0)

class _TtyStream(io.StringIO):
    def isatty(self) -> bool:
        return True


class OperatorTuiSelectionTests(unittest.TestCase):
    def _run_shutdown_scenario(self, scenario: str) -> None:
        try:
            completed = subprocess.run(
                [
                    sys.executable,
                    "-m",
                    "tests.operator_tui_shutdown_scenario",
                    scenario,
                ],
                cwd=Path(__file__).parent.parent,
                capture_output=True,
                text=True,
                timeout=5.0,
                check=False,
            )
        except subprocess.TimeoutExpired as error:
            self.fail(f"TUI shutdown scenario {scenario!r} deadlocked: {error}")
        self.assertEqual(
            completed.returncode,
            0,
            msg=(
                f"TUI shutdown scenario {scenario!r} failed\n"
                f"stdout:\n{completed.stdout}\n"
                f"stderr:\n{completed.stderr}"
            ),
        )

    def test_quit_completes_without_command_worker_ui_deadlock(self) -> None:
        self._run_shutdown_scenario("quit")

    def test_ctrl_c_completes_safe_quit(self) -> None:
        self._run_shutdown_scenario("ctrl-c")

    def test_command_exception_exits_and_stops_executor(self) -> None:
        self._run_shutdown_scenario("command-failure")

    def test_pending_confirmation_is_cancelled_during_shutdown(self) -> None:
        self._run_shutdown_scenario("pending-confirmation")

    def test_executor_cleanup_is_idempotent_before_start(self) -> None:
        runtime = _runtime(Path("/tmp/tui-idempotent-cleanup"))
        app = OperatorTui(runtime, CodexActivityStream())

        app._executor.stop()
        app._executor.stop()
        app._executor.join(0.01)
        app._executor.join(0.01)
        app.operator_console.shutdown()
        app.operator_console.shutdown()

        self.assertFalse(app._executor.is_alive)
        runtime.stop.assert_called_once_with()

    def test_tui_constructor_failure_stops_started_runtime(self) -> None:
        runtime = _runtime(Path("/tmp/tui-constructor-failure"))
        with patch(
            "harness.operator_tui.OperatorTui",
            side_effect=RuntimeError("simulated TUI constructor failure"),
        ):
            with self.assertRaisesRegex(RuntimeError, "constructor failure"):
                run_operator_tui(runtime, CodexActivityStream())
        runtime.stop.assert_called_once_with()

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

    def test_lazy_tui_import_failure_stops_runtime_and_observability(self) -> None:
        runtime = _runtime(Path("/tmp/tui-import-failure"))
        observability = Mock()
        environment = {**os.environ, "TERM": "xterm-256color"}
        with (
            tempfile.TemporaryDirectory() as temporary,
            patch.dict(os.environ, environment, clear=True),
            patch.dict(sys.modules, {"harness.operator_tui": None}),
            patch(
                "harness.operator_console.ExperimentObservability",
                return_value=observability,
            ),
            patch(
                "harness.operator_console.CodexOSRun", return_value=runtime
            ),
        ):
            with self.assertRaises(ModuleNotFoundError):
                main(
                    [
                        "--run-directory",
                        temporary,
                        "--initial-iso",
                        str(Path(temporary) / "seed.iso"),
                    ],
                    _TtyStream(),
                    _TtyStream(),
                )
        runtime.stop.assert_called_once_with()
        observability.close.assert_called_once_with()

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

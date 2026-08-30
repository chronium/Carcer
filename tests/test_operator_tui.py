import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from rich.markdown import Markdown as RichMarkdown
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
    AgentMessageRow,
    ActivityTranscript,
    FeatureRequestRow,
    LifecycleRow,
    OperatorTui,
    OperatorRow,
    ReasoningRow,
    ToolDetailToggleRequested,
    ToolRow,
    TrustedBuildRow,
    run_operator_tui,
)
from harness.operator_tui_model import (
    ActivityDisplayKind,
    ActivityDisplayEntry,
    OperatorPresentation,
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
    .activity-row { width: 1fr; height: auto; margin-bottom: 1; }
    """

    def compose(self) -> ComposeResult:
        with VerticalScroll(id="activity-scroll"):
            yield ActivityTranscript()


def _entry(index: int, *, body: str | None = None) -> ActivityDisplayEntry:
    return ActivityDisplayEntry(
        f"entry:{index}",
        ActivityDisplayKind.OPERATOR,
        OperatorPresentation(
            None,
            body if body is not None else f"body {index}",
            True,
        ),
    )


def _activity_event(
    sequence: int,
    kind: CodexActivityKind,
    data: dict[str, object],
    *,
    role: CodexActivityRole = CodexActivityRole.IMPLEMENTOR,
    item_id: str | None = "item",
) -> CodexActivityEvent:
    return CodexActivityEvent(
        sequence,
        7,
        role,
        kind,
        data,
        thread_id="thread",
        turn_id="turn",
        item_id=item_id,
    )


def _representative_activity_model() -> OperatorActivityModel:
    model = OperatorActivityModel()
    model.begin_operator_block("inspect 7")
    model.append_operator_output("Generation 7\nOutcome: completed")
    model.finish_operator_block()
    events: list[CodexActivityEvent] = [
        _activity_event(1, CodexActivityKind.SESSION_STARTED, {"model": "sol"}),
        _activity_event(2, CodexActivityKind.TURN_STARTED, {}),
        _activity_event(
            3,
            CodexActivityKind.AGENT_TEXT_DELTA,
            {"text": "Inspecting task ownership."},
            item_id="sol-message",
        ),
        _activity_event(
            4,
            CodexActivityKind.AGENT_MESSAGE,
            {"text": "I inspected **task ownership** before editing."},
            item_id="sol-message",
        ),
        _activity_event(
            5,
            CodexActivityKind.AGENT_REASONING_SUMMARY,
            {"summary": ["The allocation count is fixed; derive ownership dynamically."]},
            item_id="sol-reasoning",
        ),
    ]
    tool_calls = [
        ("read", {"path": "seed/tasks.c", "offset": 0, "length": 10600}, {"status": 0, "output": b"int task;\n" * 1000}),
        ("write", {"path": "seed/tasks.c", "offset": 0, "data": "int task;\n" * 900}, {"status": 0, "output": b""}),
        ("truncate", {"path": "seed/tasks.c", "length": 10162}, {"status": 0, "output": b""}),
        ("remove", {"path": "seed/old.c"}, {"status": 0, "output": b""}),
    ]
    sequence = 6
    for index, (tool, arguments, result) in enumerate(tool_calls):
        events.append(
            _activity_event(
                sequence,
                CodexActivityKind.TOOL_COMPLETED,
                {"tool": tool, "arguments": arguments, "result": result},
                item_id=f"tool-{index}",
            )
        )
        sequence += 1
    events.extend(
        [
            _activity_event(
                sequence,
                CodexActivityKind.TOOL_FAILED,
                {"tool": "read", "arguments": {"path": "seed/missing.c", "offset": 0, "length": 1}, "error": "file not found"},
                item_id="failed-tool",
            ),
            _activity_event(sequence + 1, CodexActivityKind.BUILD_STARTED, {}, item_id=None),
            _activity_event(sequence + 2, CodexActivityKind.BUILD_COMPILE_COMPLETED, {"result": "success"}, item_id=None),
            _activity_event(sequence + 3, CodexActivityKind.BUILD_CANDIDATE_STARTED, {}, item_id=None),
            _activity_event(sequence + 4, CodexActivityKind.BUILD_CANDIDATE_READY, {}, item_id=None),
            _activity_event(sequence + 5, CodexActivityKind.BUILD_PROTOCOL_VALIDATED, {}, item_id=None),
            _activity_event(sequence + 6, CodexActivityKind.BUILD_COMPLETED, {"status": 0}, item_id=None),
            _activity_event(sequence + 7, CodexActivityKind.TOOL_COMPLETED, {"tool": "review", "arguments": {"focus": "correctness"}, "result": "Review result."}, item_id="review"),
            _activity_event(sequence + 8, CodexActivityKind.AGENT_REASONING_SUMMARY, {"summary": ["Checking page restoration."]}, role=CodexActivityRole.REVIEWER, item_id="luna-reasoning"),
            _activity_event(sequence + 9, CodexActivityKind.AGENT_MESSAGE, {"text": "The ownership accounting is sound."}, role=CodexActivityRole.REVIEWER, item_id="luna-message"),
            _activity_event(sequence + 10, CodexActivityKind.TURN_COMPLETED, {}),
            _activity_event(sequence + 11, CodexActivityKind.REVIEW_COMPLETED, {}, role=CodexActivityRole.REVIEWER),
            _activity_event(sequence + 12, CodexActivityKind.TURN_INTERRUPTED, {"status": "interrupted"}, item_id=None),
            _activity_event(sequence + 13, CodexActivityKind.TOOL_STARTED, {"tool": "request_feature", "arguments": {"title": "External capability", "description": "A trusted-environment capability is justified."}}, item_id="feature"),
        ]
    )
    for event in events:
        model.consume(event)
    return model


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
                model.begin_operator_block()
                model.append_operator_output(f"line {index}")
                model.finish_operator_block()
            transcript.reconcile(model.entries)
            await pilot.pause()
            oldest_key = model.entries[0].key
            oldest_row = transcript.row_for(oldest_key)

            for index in range(3, 10):
                model.begin_operator_block()
                model.append_operator_output(f"line {index}")
                model.finish_operator_block()
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
            model.begin_operator_block()
            model.append_operator_output(f"history {index}")
            model.finish_operator_block()
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
            self.assertIn("streamed update 100", tail.entry.presentation.text)
            self.assertEqual(len(transcript.rows), len(entries))


class SpecializedTranscriptRowTests(unittest.IsolatedAsyncioTestCase):
    async def test_representative_transcript_is_structural_at_common_sizes(self) -> None:
        model = _representative_activity_model()
        self.assertNotIn("turn.completed", model.render_text())
        self.assertNotIn("session.started", model.render_text())
        for size in ((80, 24), (100, 30), (140, 40)):
            with self.subTest(size=size):
                app = _TranscriptApp()
                async with app.run_test(size=size) as pilot:
                    transcript = app.query_one(ActivityTranscript)
                    transcript.reconcile(model.entries)
                    await pilot.pause()
                    self.assertEqual(transcript.row_keys, tuple(entry.key for entry in model.entries))
                    self.assertTrue(any(isinstance(row, AgentMessageRow) for row in transcript.rows))
                    self.assertTrue(any(isinstance(row, ToolRow) for row in transcript.rows))
                    self.assertTrue(any(isinstance(row, TrustedBuildRow) for row in transcript.rows))
                    self.assertTrue(any(isinstance(row, FeatureRequestRow) for row in transcript.rows))
                    self.assertEqual(
                        app.query_one("#activity-scroll", VerticalScroll).max_scroll_x,
                        0,
                    )

    async def test_streaming_message_finalizes_as_markdown_in_same_row(self) -> None:
        model = OperatorActivityModel()
        model.begin_operator_block()
        model.append_operator_output("Stable operator output")
        model.finish_operator_block()
        model.consume(
            _activity_event(
                1,
                CodexActivityKind.AGENT_TEXT_DELTA,
                {"text": "Streaming **plain**"},
                item_id="message",
            )
        )
        app = _TranscriptApp()
        async with app.run_test(size=(90, 24)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            transcript.reconcile(model.entries)
            await pilot.pause()
            unrelated = transcript.rows[0]
            row = transcript.rows[1]
            self.assertIsInstance(row, AgentMessageRow)
            self.assertFalse(row.renders_markdown)

            model.consume(
                _activity_event(
                    2,
                    CodexActivityKind.AGENT_MESSAGE,
                    {"text": "Final **Markdown** message.\x1b[2J"},
                    item_id="message",
                )
            )
            transcript.reconcile(model.entries)
            await pilot.pause()
            self.assertIs(transcript.rows[0], unrelated)
            self.assertIs(transcript.rows[1], row)
            self.assertTrue(row.renders_markdown)
            self.assertIn("\\x1b[2J", row.entry.presentation.text)
            self.assertNotIn("\x1b", row.entry.presentation.text)
            renderable = row._Static__content.renderables[1].renderable
            self.assertIsInstance(renderable, RichMarkdown)

    async def test_representative_activity_selects_specialized_rows(self) -> None:
        model = OperatorActivityModel()
        model.begin_operator_block("inspect 7")
        model.append_operator_output("Generation 7\nOutcome: completed")
        model.finish_operator_block()
        events = [
            _activity_event(
                1,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": "Sol result."},
                item_id="sol",
            ),
            _activity_event(
                2,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Inspect ownership."]},
                item_id="reasoning",
            ),
            _activity_event(
                3,
                CodexActivityKind.TOOL_COMPLETED,
                {
                    "tool": "read",
                    "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 4096},
                    "result": {"status": 0, "output": b"int task;\n" * 100},
                },
                item_id="read",
            ),
            _activity_event(4, CodexActivityKind.BUILD_STARTED, {}, item_id=None),
            _activity_event(
                5,
                CodexActivityKind.TOOL_STARTED,
                {
                    "tool": "request_feature",
                    "arguments": {"title": "External capability", "description": "Needed for later work."},
                },
                item_id="feature",
            ),
            _activity_event(
                6,
                CodexActivityKind.TURN_FAILED,
                {"status": "failed"},
                item_id=None,
            ),
        ]
        for event in events:
            model.consume(event)
        app = _TranscriptApp()
        async with app.run_test(size=(100, 30)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            transcript.reconcile(model.entries)
            await pilot.pause()
            self.assertEqual(
                tuple(type(row) for row in transcript.rows),
                (
                    OperatorRow,
                    AgentMessageRow,
                    ReasoningRow,
                    ToolRow,
                    TrustedBuildRow,
                    FeatureRequestRow,
                    LifecycleRow,
                ),
            )

    async def test_tool_completion_and_build_phases_preserve_outer_rows(self) -> None:
        model = OperatorActivityModel()
        call = {"tool": "read", "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 64}}
        model.consume(
            _activity_event(1, CodexActivityKind.TOOL_STARTED, call, item_id="read")
        )
        model.consume(_activity_event(2, CodexActivityKind.BUILD_STARTED, {}, item_id=None))
        app = _TranscriptApp()
        async with app.run_test(size=(90, 24)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            transcript.reconcile(model.entries)
            await pilot.pause()
            tool_row, build_row = transcript.rows

            model.consume(
                _activity_event(
                    3,
                    CodexActivityKind.TOOL_COMPLETED,
                    {**call, "result": {"status": 0, "output": b"source\n"}},
                    item_id="read",
                )
            )
            model.consume(
                _activity_event(
                    4,
                    CodexActivityKind.BUILD_COMPILE_COMPLETED,
                    {"result": "success"},
                    item_id=None,
                )
            )
            transcript.reconcile(model.entries)
            await pilot.pause()
            self.assertIs(transcript.rows[0], tool_row)
            self.assertIs(transcript.rows[1], build_row)
            self.assertFalse(tool_row.detail_expanded)
            self.assertIn("compile/link", str(build_row.render()))

    async def test_failed_tool_detail_defaults_visible(self) -> None:
        model = OperatorActivityModel()
        model.consume(
            _activity_event(
                1,
                CodexActivityKind.TOOL_FAILED,
                {
                    "tool": "read",
                    "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 4},
                    "error": "guest read failed",
                },
                item_id="failed",
            )
        )
        app = _TranscriptApp()
        async with app.run_test(size=(90, 24)) as pilot:
            transcript = app.query_one(ActivityTranscript)
            transcript.reconcile(model.entries)
            await pilot.pause()
            row = transcript.rows[0]
            self.assertIsInstance(row, ToolRow)
            self.assertTrue(row.detail_expanded)
            self.assertTrue(row.query_one(".tool-detail").display)
            self.assertIn("guest read failed", str(row.query_one(".tool-detail").render()))


class OperatorTuiInteractionTests(unittest.IsolatedAsyncioTestCase):
    async def test_tool_detail_toggle_preserves_input_and_historical_scroll(self) -> None:
        runtime = _runtime(Path("/tmp/tui-tool-detail"))
        stream = CodexActivityStream()
        app = OperatorTui(runtime, stream)
        async with app.run_test(size=(80, 18)) as pilot:
            await pilot.pause(0.15)
            for index in range(30):
                stream.publish(
                    6,
                    CodexActivityRole.IMPLEMENTOR,
                    CodexActivityKind.AGENT_MESSAGE,
                    {"text": f"history {index} " + "x" * 60},
                    thread_id="thread",
                    turn_id="turn",
                    item_id=f"history-{index}",
                )
            stream.publish(
                6,
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.TOOL_COMPLETED,
                {
                    "tool": "read",
                    "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 8192},
                    "result": {"status": 0, "output": b"source line\n" * 200},
                },
                thread_id="thread",
                turn_id="turn",
                item_id="read",
            )
            await pilot.pause(0.2)
            input_widget = app.query_one("#command-input", Input)
            input_widget.value = "partially typed"
            input_widget.focus()
            transcript = app.query_one(ActivityTranscript)
            pane = app.query_one("#activity-scroll", VerticalScroll)
            tool_row = transcript.rows[-1]
            self.assertIsInstance(tool_row, ToolRow)
            self.assertFalse(tool_row.detail_expanded)

            app.action_activity_end()
            await pilot.pause()
            await pilot.click(tool_row._toggle, offset=(1, 0))
            await pilot.pause()
            self.assertTrue(tool_row.detail_expanded)
            self.assertTrue(app._follow.following)
            self.assertTrue(pane.is_vertical_scroll_end)
            self.assertEqual(input_widget.value, "partially typed")
            self.assertIs(app.focused, input_widget)
            await pilot.click(tool_row._detail_close, offset=(1, 0))
            await pilot.pause()
            self.assertFalse(tool_row.detail_expanded)

            await pilot.press("pageup")
            await pilot.pause()
            reading_position = pane.scroll_y
            tool_row.post_message(ToolDetailToggleRequested())
            await pilot.pause()
            self.assertTrue(tool_row.detail_expanded)
            self.assertAlmostEqual(pane.scroll_y, reading_position)
            self.assertEqual(input_widget.value, "partially typed")
            self.assertIs(app.focused, input_widget)
            app.exit()

        app._executor.join(2.0)

    async def test_live_follow_anchors_only_after_content_overflows(self) -> None:
        runtime = _runtime(Path("/tmp/tui-short-content"))
        app = OperatorTui(runtime, CodexActivityStream())
        app._executor.start = Mock()
        async with app.run_test(size=(90, 24)) as pilot:
            await pilot.pause()
            await pilot.pause()
            pane = app.query_one("#activity-scroll", VerticalScroll)
            transcript = app.query_one(ActivityTranscript)

            self.assertEqual(pane.max_scroll_y, 0)
            self.assertEqual(pane.scroll_y, 0)
            self.assertFalse(pane.is_anchored)

            for index in range(3):
                app._model.begin_operator_block()
                app._model.append_operator_output(f"short row {index}")
                app._model.finish_operator_block()
            app._activity_changed(3)
            await pilot.pause()
            await pilot.pause()

            self.assertEqual(pane.max_scroll_y, 0)
            self.assertEqual(pane.scroll_y, 0)
            self.assertFalse(pane.is_anchored)
            self.assertEqual(
                transcript.region.y,
                pane.scrollable_content_region.y,
            )
            app._follow.scrolled(0)
            app.action_activity_end()
            self.assertEqual(pane.scroll_y, 0)
            self.assertFalse(pane.is_anchored)

            for index in range(3, 24):
                app._model.begin_operator_block()
                app._model.append_operator_output(f"overflow row {index}")
                app._model.finish_operator_block()
            app._activity_changed(21)
            await pilot.pause()
            await pilot.pause()

            self.assertGreater(pane.max_scroll_y, 0)
            self.assertEqual(pane.scroll_y, pane.max_scroll_y)
            self.assertTrue(pane.is_anchored)

            await pilot.resize_terminal(90, 100)
            await pilot.pause()
            await pilot.pause()
            self.assertEqual(pane.max_scroll_y, 0)
            self.assertEqual(pane.scroll_y, 0)
            self.assertFalse(pane.is_anchored)
            self.assertEqual(
                transcript.region.y,
                pane.scrollable_content_region.y,
            )
            app.exit()

        app._executor.join(2.0)

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
            operator_entries = [
                entry
                for entry in app.activity_model.entries
                if entry.kind is ActivityDisplayKind.OPERATOR
            ]
            command_blocks = [
                entry.presentation
                for entry in operator_entries
                if entry.presentation.command == "status"
            ]
            self.assertEqual(len(command_blocks), 1)
            self.assertIn("State: STOPPED", command_blocks[0].output)
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

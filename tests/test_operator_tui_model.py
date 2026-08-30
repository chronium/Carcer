import unittest

from harness.codex_activity import (
    CodexActivityEvent,
    CodexActivityKind,
    CodexActivityRole,
)
from harness.operator_tui_model import (
    ActivityDisplayKind,
    ActivityDisplayState,
    ActivityFollowState,
    AgentMessagePresentation,
    BuildPresentation,
    FeatureRequestPresentation,
    FeatureRequestRecordingState,
    FeatureRequestInitialStatus,
    InterviewQuestionPresentation,
    LifecyclePresentation,
    OperatorActivityModel,
    OperatorPresentation,
    ReasoningPresentation,
    ToolPresentation,
)


def _event(
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


class OperatorActivityModelTests(unittest.TestCase):
    def test_exit_interview_question_is_distinct_and_bounded(self) -> None:
        model = OperatorActivityModel(display_bytes=32)

        model.consume(
            _event(
                1,
                CodexActivityKind.EXIT_INTERVIEW_QUESTION,
                {"text": "Why this choice?\x1b[2J"},
                role=CodexActivityRole.HARNESS,
            )
        )

        self.assertEqual(len(model.entries), 1)
        entry = model.entries[0]
        self.assertEqual(entry.kind, ActivityDisplayKind.INTERVIEW_QUESTION)
        self.assertEqual(
            entry.presentation,
            InterviewQuestionPresentation("Why this choice?\\x1b[2J"),
        )

    def test_messages_keep_role_finality_and_stable_key(self) -> None:
        model = OperatorActivityModel()
        model.consume(_event(1, CodexActivityKind.AGENT_TEXT_DELTA, {"text": "Inspect "}))
        first = model.entries[0]
        self.assertEqual(first.kind, ActivityDisplayKind.MESSAGE)
        self.assertEqual(
            first.presentation,
            AgentMessagePresentation(
                CodexActivityRole.IMPLEMENTOR, "Inspect ", False
            ),
        )

        model.consume(
            _event(2, CodexActivityKind.AGENT_TEXT_DELTA, {"text": "state."})
        )
        model.consume(
            _event(
                3,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": "**Inspect state.**"},
            )
        )
        final = model.entries[0]
        self.assertEqual(final.key, first.key)
        self.assertEqual(len(model.entries), 1)
        self.assertEqual(
            final.presentation,
            AgentMessagePresentation(
                CodexActivityRole.IMPLEMENTOR, "**Inspect state.**", True
            ),
        )

        model.consume(
            _event(
                4,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": "Reviewer result."},
                role=CodexActivityRole.REVIEWER,
                item_id="review-message",
            )
        )
        reviewer = model.entries[1].presentation
        self.assertIsInstance(reviewer, AgentMessagePresentation)
        self.assertEqual(reviewer.role, CodexActivityRole.REVIEWER)

    def test_reasoning_items_are_distinct_and_empty_summaries_are_suppressed(self) -> None:
        model = OperatorActivityModel()
        model.consume(
            _event(
                1,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "", "summary_index": 0},
                item_id="empty-delta",
            )
        )
        model.consume(
            _event(
                2,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": []},
                item_id="empty-complete",
            )
        )
        self.assertEqual(model.entries, ())

        model.consume(
            _event(
                3,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "Check ", "summary_index": 0},
                item_id="reasoning-a",
            )
        )
        first_key = model.entries[0].key
        model.consume(
            _event(
                4,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "ownership.", "summary_index": 0},
                item_id="reasoning-a",
            )
        )
        model.consume(
            _event(
                5,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Check ownership."]},
                item_id="reasoning-a",
            )
        )
        presentation = model.entries[0].presentation
        self.assertEqual(model.entries[0].key, first_key)
        self.assertEqual(
            presentation,
            ReasoningPresentation(
                CodexActivityRole.IMPLEMENTOR, "Check ownership.", True
            ),
        )

        model.consume(
            _event(
                6,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Independent check."]},
                role=CodexActivityRole.REVIEWER,
                item_id="reasoning-b",
            )
        )
        self.assertEqual(len(model.entries), 2)
        self.assertEqual(
            model.entries[1].presentation.role, CodexActivityRole.REVIEWER
        )

    def test_tool_state_summary_and_bounded_detail_remain_structured(self) -> None:
        model = OperatorActivityModel(display_bytes=80)
        source = "int task;\n" * 20
        started = {
            "namespace": "codexos",
            "tool": "write",
            "arguments": {
                "path": "seed/tasks.c",
                "offset": 12,
                "data": source,
            },
        }
        model.consume(_event(1, CodexActivityKind.TOOL_STARTED, started, item_id="w"))
        entry = model.entries[0]
        tool = entry.presentation
        self.assertEqual(entry.kind, ActivityDisplayKind.TOOL)
        self.assertIsInstance(tool, ToolPresentation)
        self.assertEqual(tool.tool, "write")
        self.assertEqual(tool.state, ActivityDisplayState.RUNNING)
        self.assertIn("seed/tasks.c", tool.summary)
        self.assertNotIn("int task", tool.summary)
        self.assertIsNotNone(tool.detail)
        self.assertEqual(tool.detail.byte_count, len(source.encode()))
        self.assertTrue(tool.detail.truncated)
        self.assertLessEqual(len(tool.detail.text.encode()), 160)

        model.consume(
            _event(
                2,
                CodexActivityKind.TOOL_COMPLETED,
                {**started, "success": True, "result": {"status": 0, "output": b""}},
                item_id="w",
            )
        )
        completed = model.entries[0].presentation
        self.assertEqual(model.entries[0].key, entry.key)
        self.assertEqual(completed.state, ActivityDisplayState.COMPLETED)
        self.assertEqual(completed.detail.byte_count, len(source.encode()))

        model.consume(
            _event(
                3,
                CodexActivityKind.TOOL_COMPLETED,
                {
                    "tool": "remove",
                    "arguments": {"path": "seed/old.c"},
                    "result": {"status": 0, "output": b""},
                },
                item_id="remove",
            )
        )
        self.assertIsNone(model.entries[1].presentation.detail)

    def test_failed_tool_retains_error_detail_and_binary_is_deterministic(self) -> None:
        model = OperatorActivityModel(display_bytes=32)
        started = {"tool": "read", "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 64}}
        model.consume(_event(1, CodexActivityKind.TOOL_STARTED, started, item_id="r"))
        model.consume(
            _event(
                2,
                CodexActivityKind.TOOL_FAILED,
                {**started, "success": False, "result": {"status": 7, "output": b"\xff\x00\x1b"}},
                item_id="r",
            )
        )
        tool = model.entries[0].presentation
        self.assertEqual(tool.state, ActivityDisplayState.FAILED)
        self.assertTrue(tool.detail.binary)
        self.assertIn("ff 00 1b", tool.detail.text)

        model.consume(
            _event(
                3,
                CodexActivityKind.TOOL_FAILED,
                {"tool": "unknown", "arguments": {}, "error": "bad\x1b[2J"},
                item_id="bad",
            )
        )
        unknown = model.entries[1].presentation
        self.assertIn("\\x1b[2J", unknown.detail.text)
        self.assertNotIn("\x1b", model.render_text())

    def test_failed_write_prioritizes_diagnostic_over_source_payload(self) -> None:
        source = "int attempted_write;\n" * 20
        write = {
            "tool": "write",
            "arguments": {
                "path": "seed/tasks.c",
                "offset": 0,
                "data": source,
            },
        }
        model = OperatorActivityModel()
        model.consume(
            _event(
                1,
                CodexActivityKind.TOOL_FAILED,
                {**write, "error": "guest write failed"},
                item_id="write-error",
            )
        )
        error_failure = model.entries[0].presentation
        self.assertEqual(error_failure.state, ActivityDisplayState.FAILED)
        self.assertEqual(error_failure.detail.text, "guest write failed")
        self.assertNotIn("attempted_write", error_failure.detail.text)

        model.consume(
            _event(
                2,
                CodexActivityKind.TOOL_FAILED,
                {
                    **write,
                    "result": {"status": 2, "output": b"write rejected by guest"},
                },
                item_id="write-result",
            )
        )
        result_failure = model.entries[1].presentation
        self.assertEqual(result_failure.state, ActivityDisplayState.FAILED)
        self.assertEqual(result_failure.detail.text, "write rejected by guest")
        self.assertNotIn("attempted_write", result_failure.detail.text)

    def test_feature_request_records_only_creation_time_status(self) -> None:
        model = OperatorActivityModel()
        request = {
            "tool": "request_feature",
            "arguments": {"title": "External capacity", "description": "Need Δ"},
        }
        model.consume(_event(1, CodexActivityKind.TOOL_STARTED, request, item_id="f"))
        first = model.entries[0]
        self.assertEqual(first.kind, ActivityDisplayKind.FEATURE_REQUEST)
        self.assertEqual(
            first.presentation,
            FeatureRequestPresentation(
                CodexActivityRole.IMPLEMENTOR,
                FeatureRequestRecordingState.RECORDING,
                None,
                "External capacity",
                "Need Δ",
                "",
                "",
            ),
        )
        self.assertIn("recording", model.render_text())
        self.assertNotIn("pending", model.render_text())
        self.assertNotIn("completed", model.render_text())
        model.consume(
            _event(
                2,
                CodexActivityKind.TOOL_COMPLETED,
                {**request, "result": {"status": 0, "output": b"4"}},
                item_id="f",
            )
        )
        self.assertEqual(model.entries[0].key, first.key)
        recorded = model.entries[0].presentation
        self.assertEqual(
            recorded.recording_state, FeatureRequestRecordingState.RECORDED
        )
        self.assertEqual(
            recorded.initial_status, FeatureRequestInitialStatus.PENDING
        )
        self.assertEqual(recorded.request_id, "4")
        self.assertIn("recorded", model.render_text())
        self.assertIn("initial status: pending", model.render_text())
        self.assertIn(
            "recording did not provision the capability", model.render_text()
        )
        self.assertNotIn("trusted status", model.render_text())
        self.assertNotIn("approved", model.render_text())
        self.assertNotIn("denied", model.render_text())
        self.assertNotIn("completed", model.render_text())

        failed_request = {
            "tool": "request_feature",
            "arguments": {"title": "Another request", "description": "Need it"},
        }
        model.consume(
            _event(
                3,
                CodexActivityKind.TOOL_FAILED,
                {**failed_request, "error": "request rejected"},
                item_id="failed-feature",
            )
        )
        failed = model.entries[1].presentation
        self.assertEqual(
            failed.recording_state, FeatureRequestRecordingState.FAILED
        )
        self.assertIsNone(failed.initial_status)
        self.assertEqual(failed.request_id, "")
        self.assertEqual(failed.error, "request rejected")

    def test_build_phases_are_structured_and_later_builds_get_new_keys(self) -> None:
        model = OperatorActivityModel()
        kinds = [
            (CodexActivityKind.BUILD_STARTED, {}),
            (CodexActivityKind.BUILD_COMPILE_COMPLETED, {"result": "success"}),
            (CodexActivityKind.BUILD_CANDIDATE_STARTED, {}),
            (CodexActivityKind.BUILD_CANDIDATE_READY, {}),
            (CodexActivityKind.BUILD_PROTOCOL_VALIDATED, {}),
            (CodexActivityKind.BUILD_COMPLETED, {"status": 0}),
        ]
        first_key = ""
        for sequence, (kind, data) in enumerate(kinds, 1):
            model.consume(_event(sequence, kind, data, item_id=None))
            self.assertEqual(len(model.entries), 1)
            first_key = first_key or model.entries[0].key
            self.assertEqual(model.entries[0].key, first_key)
        build = model.entries[0].presentation
        self.assertIsInstance(build, BuildPresentation)
        self.assertEqual(build.state, ActivityDisplayState.COMPLETED)
        self.assertTrue(
            all(phase.state is ActivityDisplayState.COMPLETED for phase in build.phases)
        )

        model.consume(_event(7, CodexActivityKind.BUILD_STARTED, {}, item_id=None))
        self.assertEqual(len(model.entries), 2)
        self.assertNotEqual(model.entries[1].key, first_key)

    def test_review_result_deduplication_is_exact(self) -> None:
        model = OperatorActivityModel()
        review_text = "One issue found."
        model.consume(
            _event(
                1,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": review_text},
                role=CodexActivityRole.REVIEWER,
                item_id="review-message",
            )
        )
        call = {"tool": "review", "arguments": {"focus": "correctness"}}
        model.consume(_event(2, CodexActivityKind.TOOL_STARTED, call, item_id="review"))
        model.consume(
            _event(
                3,
                CodexActivityKind.TOOL_COMPLETED,
                {**call, "result": review_text},
                item_id="review",
            )
        )
        tool = model.entries[1].presentation
        self.assertEqual(tool.result_note, "result returned to Sol")
        self.assertIsNone(tool.detail)
        self.assertEqual(model.render_text().count(review_text), 1)

    def test_routine_lifecycle_is_filtered_but_abnormal_is_visible(self) -> None:
        model = OperatorActivityModel()
        for sequence, kind in enumerate(
            (
                CodexActivityKind.SESSION_STARTED,
                CodexActivityKind.TURN_STARTED,
                CodexActivityKind.TURN_COMPLETED,
                CodexActivityKind.REVIEW_STARTED,
                CodexActivityKind.REVIEW_COMPLETED,
                CodexActivityKind.SESSION_STOPPED,
            ),
            1,
        ):
            self.assertFalse(model.consume(_event(sequence, kind, {"model": "x"})))
        self.assertEqual(model.entries, ())

        model.consume(
            _event(
                7,
                CodexActivityKind.TURN_FAILED,
                {"status": "failed", "model": "not repeated"},
            )
        )
        entry = model.entries[0]
        self.assertEqual(entry.kind, ActivityDisplayKind.LIFECYCLE)
        self.assertIsInstance(entry.presentation, LifecyclePresentation)
        self.assertEqual(entry.presentation.state, ActivityDisplayState.FAILED)
        self.assertNotIn("model", entry.presentation.detail)

    def test_operator_output_groups_startup_and_commands_without_blank_rows(self) -> None:
        model = OperatorActivityModel()
        startup_key = model.begin_operator_block()
        model.append_operator_output("CodexOS operator console")
        model.append_operator_output("")
        model.append_operator_output("State: RUNNING")
        model.finish_operator_block()
        self.assertEqual(len(model.entries), 1)
        startup = model.entries[0].presentation
        self.assertIsInstance(startup, OperatorPresentation)
        self.assertEqual(startup.output, "CodexOS operator console\n\nState: RUNNING")
        self.assertTrue(startup.finalized)

        command_key = model.begin_operator_block("inspect 7")
        model.append_operator_output("Generation 7")
        model.consume(
            _event(1, CodexActivityKind.AGENT_MESSAGE, {"text": "Async output."})
        )
        model.append_operator_output("Outcome: completed")
        model.finish_operator_block()
        self.assertEqual([entry.key for entry in model.entries[:2]], [startup_key, command_key])
        self.assertEqual(model.entries[1].presentation.command, "inspect 7")
        self.assertIn("Outcome: completed", model.entries[1].presentation.output)
        self.assertEqual(model.entries[2].kind, ActivityDisplayKind.MESSAGE)

    def test_scrollback_bounds_richer_entries_and_retains_discard_marker(self) -> None:
        model = OperatorActivityModel(max_entries=4, max_bytes=160)
        for index in range(10):
            model.begin_operator_block(f"status {index}")
            model.append_operator_output(f"line {index}")
            model.finish_operator_block()
        self.assertLessEqual(len(model.entries), 4)
        self.assertEqual(model.entries[0].key, "scrollback:discarded")
        self.assertIn("older live activity discarded", model.render_text())

    def test_manual_scroll_tracks_new_events_until_return_to_live(self) -> None:
        follow = ActivityFollowState()
        follow.scrolled(42.0)
        follow.arrived(3)
        self.assertFalse(follow.following)
        self.assertEqual(follow.scroll_y, 42.0)
        self.assertEqual(follow.new_events, 3)
        follow.return_to_live()
        self.assertTrue(follow.following)
        self.assertEqual(follow.new_events, 0)


if __name__ == "__main__":
    unittest.main()

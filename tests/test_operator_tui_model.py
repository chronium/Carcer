import unittest

from harness.codex_activity import (
    CodexActivityEvent,
    CodexActivityKind,
    CodexActivityRole,
)
from harness.operator_tui_model import (
    ActivityFollowState,
    OperatorActivityModel,
    safe_display_bytes,
    safe_display_text,
)


def _event(
    sequence: int,
    kind: CodexActivityKind,
    data: dict[str, object],
    *,
    role: CodexActivityRole = CodexActivityRole.IMPLEMENTOR,
    item_id: str | None = "item-1",
) -> CodexActivityEvent:
    return CodexActivityEvent(
        sequence,
        6,
        role,
        kind,
        data,
        thread_id="thread-1",
        turn_id="turn-1",
        item_id=item_id,
    )


class OperatorActivityModelTests(unittest.TestCase):
    def test_agent_and_reasoning_deltas_coalesce_with_completed_items(self) -> None:
        model = OperatorActivityModel()
        model.consume(
            _event(1, CodexActivityKind.AGENT_TEXT_DELTA, {"text": "Inspect "})
        )
        model.consume(
            _event(2, CodexActivityKind.AGENT_TEXT_DELTA, {"text": "state."})
        )
        model.consume(
            _event(3, CodexActivityKind.AGENT_MESSAGE, {"text": "Inspect state."})
        )
        model.consume(
            _event(
                4,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "Check ", "summary_index": 0},
                item_id="reasoning-1",
            )
        )
        model.consume(
            _event(
                5,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Check scheduler."]},
                item_id="reasoning-1",
            )
        )

        rendered = model.render_text()
        self.assertEqual(rendered.count("Inspect state."), 1)
        self.assertEqual(rendered.count("Check scheduler."), 1)
        self.assertNotIn("Check \n", rendered)
        self.assertIn("Sol · reasoning summary", rendered)

    def test_empty_reasoning_is_suppressed_and_nonempty_items_stay_distinct(
        self,
    ) -> None:
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
                {"summary": ["", "  "]},
                item_id="empty-completion",
            )
        )
        self.assertEqual(model.entries, ())

        model.consume(
            _event(
                3,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "Inspect ", "summary_index": 0},
                item_id="sol-reasoning",
            )
        )
        model.consume(
            _event(
                4,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "state.", "summary_index": 0},
                item_id="sol-reasoning",
            )
        )
        self.assertEqual(len(model.entries), 1)
        self.assertIn("Inspect state.", model.entries[0].body)
        model.consume(
            _event(
                5,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Inspected state."]},
                item_id="sol-reasoning",
            )
        )
        self.assertEqual(len(model.entries), 1)
        self.assertEqual(model.entries[0].body, "Inspected state.")

        model.consume(
            _event(
                6,
                CodexActivityKind.AGENT_REASONING_DELTA,
                {"text": "Transient", "summary_index": 0},
                item_id="transient",
            )
        )
        model.consume(
            _event(
                7,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": []},
                item_id="transient",
            )
        )
        self.assertEqual(len(model.entries), 1)
        self.assertNotIn("Transient", model.render_text())

        model.consume(
            _event(
                8,
                CodexActivityKind.AGENT_REASONING_SUMMARY,
                {"summary": ["Reviewer summary."]},
                role=CodexActivityRole.REVIEWER,
                item_id="luna-reasoning",
            )
        )
        self.assertEqual(len(model.entries), 2)
        self.assertEqual(
            [entry.label for entry in model.entries],
            ["Sol · reasoning summary", "Luna · reasoning summary"],
        )

    def test_tool_lifecycle_coalesces_and_failed_tool_is_final(self) -> None:
        model = OperatorActivityModel()
        started = {
            "namespace": "codexos",
            "tool": "read",
            "arguments": {"path": "seed/main.c", "offset": 0, "length": 100},
        }
        model.consume(_event(1, CodexActivityKind.TOOL_STARTED, started))
        model.consume(
            _event(
                2,
                CodexActivityKind.TOOL_FAILED,
                {**started, "success": False, "error": "guest failure"},
            )
        )

        self.assertEqual(len(model.entries), 1)
        self.assertIn("[failed]", model.entries[0].body)
        self.assertNotIn("[running]", model.entries[0].body)
        self.assertIn("guest failure", model.entries[0].body)

    def test_utf8_write_feature_request_and_read_result_are_visible(self) -> None:
        model = OperatorActivityModel()
        write = {
            "namespace": "codexos",
            "tool": "write",
            "arguments": {
                "path": "seed/tasks.c",
                "offset": 12,
                "encoding": "utf8",
                "data": "void schedule(void) {\n\treturn;\n}",
            },
        }
        request = {
            "namespace": "codexos",
            "tool": "request_feature",
            "arguments": {"title": "External capacity", "description": "Need Δ"},
        }
        read = {
            "namespace": "codexos",
            "tool": "read",
            "arguments": {"path": "seed/tasks.c", "offset": 0, "length": 64},
        }
        model.consume(_event(1, CodexActivityKind.TOOL_STARTED, write, item_id="w"))
        model.consume(_event(2, CodexActivityKind.TOOL_STARTED, request, item_id="f"))
        model.consume(_event(3, CodexActivityKind.TOOL_STARTED, read, item_id="r"))
        model.consume(
            _event(
                4,
                CodexActivityKind.TOOL_COMPLETED,
                {**read, "result": {"status": 0, "output": b"source text"}},
                item_id="r",
            )
        )

        rendered = model.render_text()
        self.assertIn("void schedule(void)", rendered)
        self.assertIn("Title: External capacity", rendered)
        self.assertIn("Description:", rendered)
        self.assertIn("source text", rendered)

    def test_terminal_controls_binary_and_large_text_are_safe(self) -> None:
        hostile = "before\x1b[2J\r\x00\u0085after\nnext"
        rendered = safe_display_text(hostile)
        self.assertNotIn("\x1b", rendered)
        self.assertIn("\\x1b[2J", rendered)
        self.assertIn("\\r", rendered)
        self.assertIn("\\x00", rendered)
        self.assertIn("\nnext", rendered)

        binary = safe_display_bytes(b"\xff\x00\x1b")
        self.assertIn("binary (3 bytes)", binary)
        self.assertIn("ff 00 1b", binary)

        truncated = safe_display_text("x" * 100, 16)
        self.assertIn("84 bytes more in activity payload", truncated)

    def test_nested_review_is_distinct_and_outer_result_is_not_repeated(self) -> None:
        model = OperatorActivityModel()
        review_text = "One issue found."
        model.consume(
            _event(
                1,
                CodexActivityKind.AGENT_MESSAGE,
                {"text": review_text},
                role=CodexActivityRole.REVIEWER,
                item_id="luna-message",
            )
        )
        review = {
            "namespace": None,
            "tool": "review",
            "arguments": {"focus": "correctness"},
        }
        model.consume(
            _event(2, CodexActivityKind.TOOL_STARTED, review, item_id="review")
        )
        model.consume(
            _event(
                3,
                CodexActivityKind.TOOL_COMPLETED,
                {**review, "result": review_text},
                item_id="review",
            )
        )

        rendered = model.render_text()
        self.assertEqual(rendered.count(review_text), 1)
        self.assertIn("Luna", rendered)
        self.assertIn("review result returned to Sol", rendered)

    def test_build_phases_update_one_coherent_entry(self) -> None:
        model = OperatorActivityModel()
        kinds = (
            (CodexActivityKind.BUILD_STARTED, {}),
            (CodexActivityKind.BUILD_COMPILE_COMPLETED, {"result": "success"}),
            (CodexActivityKind.BUILD_CANDIDATE_STARTED, {}),
            (CodexActivityKind.BUILD_CANDIDATE_READY, {}),
            (CodexActivityKind.BUILD_PROTOCOL_VALIDATED, {}),
            (CodexActivityKind.BUILD_COMPLETED, {"status": 0}),
        )
        for sequence, (kind, data) in enumerate(kinds, 1):
            model.consume(_event(sequence, kind, data, item_id=None))

        self.assertEqual(len(model.entries), 1)
        body = model.entries[0].body
        self.assertIn("compile/link ✓", body)
        self.assertIn("candidate boot …", body)
        self.assertIn("READY ✓", body)
        self.assertIn("protocol validation ✓", body)
        self.assertIn("success ✓", body)

        model.consume(_event(7, CodexActivityKind.BUILD_STARTED, {}, item_id=None))
        model.consume(
            _event(
                8,
                CodexActivityKind.BUILD_COMPILE_COMPLETED,
                {"result": "build_failure"},
                item_id=None,
            )
        )
        model.consume(
            _event(
                9,
                CodexActivityKind.BUILD_COMPLETED,
                {"status": 1},
                item_id=None,
            )
        )
        self.assertEqual(len(model.entries), 2)
        self.assertIn("compile/link ✗", model.entries[-1].body)
        self.assertIn("failed ✗", model.entries[-1].body)

    def test_scrollback_is_bounded_with_discard_marker(self) -> None:
        model = OperatorActivityModel(max_entries=4, max_bytes=400)
        for index in range(10):
            model.append_operator_output(f"line {index}")
        self.assertLessEqual(len(model.entries), 4)
        self.assertIn("older live activity discarded", model.render_text())
        self.assertIn("line 9", model.render_text())

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

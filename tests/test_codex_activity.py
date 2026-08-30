import threading
import unittest

from harness import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
)
from harness.codex_activity import (
    publish_activity,
    publish_renderable_codex_notification,
)


class CodexActivityStreamTests(unittest.TestCase):
    def test_concurrent_publication_is_ordered_and_preserves_payloads(self) -> None:
        stream = CodexActivityStream()
        start = threading.Barrier(5)

        def publish(worker: int) -> None:
            start.wait()
            for value in range(40):
                stream.publish(
                    7,
                    CodexActivityRole.IMPLEMENTOR,
                    CodexActivityKind.TOOL_STARTED,
                    {"worker": worker, "value": value, "bytes": b"\x00src"},
                )

        threads = [threading.Thread(target=publish, args=(index,)) for index in range(4)]
        for thread in threads:
            thread.start()
        start.wait()
        for thread in threads:
            thread.join()

        events = stream.drain()
        self.assertEqual(
            [event.sequence for event in events],
            list(range(1, len(events) + 1)),
        )
        self.assertTrue(
            all(event.generation == 7 for event in events)
        )
        self.assertTrue(
            all(event.role is CodexActivityRole.IMPLEMENTOR for event in events)
        )
        self.assertTrue(all(event.data["bytes"] == b"\x00src" for event in events))

    def test_only_explicit_renderable_reasoning_summary_is_published(self) -> None:
        stream = CodexActivityStream()
        common = {
            "threadId": "thread-1",
            "turnId": "turn-1",
            "itemId": "reasoning-1",
        }
        publish_renderable_codex_notification(
            stream,
            3,
            CodexActivityRole.REVIEWER,
            {
                "method": "item/reasoning/summaryTextDelta",
                "params": {**common, "summaryIndex": 0, "delta": "Checking ABI."},
            },
            "thread-1",
            "turn-1",
        )
        publish_renderable_codex_notification(
            stream,
            3,
            CodexActivityRole.REVIEWER,
            {
                "method": "item/reasoning/textDelta",
                "params": {**common, "contentIndex": 0, "delta": "private detail"},
            },
            "thread-1",
            "turn-1",
        )

        events = stream.drain()
        self.assertEqual(len(events), 1)
        self.assertIs(events[0].kind, CodexActivityKind.AGENT_REASONING_DELTA)
        self.assertEqual(events[0].data["text"], "Checking ABI.")

    def test_publication_failure_is_ignored(self) -> None:
        class BrokenStream(CodexActivityStream):
            def publish(self, *args, **kwargs):
                raise RuntimeError("observer failed")

        publish_activity(
            BrokenStream(),
            0,
            CodexActivityRole.HARNESS,
            CodexActivityKind.BUILD_STARTED,
        )


if __name__ == "__main__":
    unittest.main()

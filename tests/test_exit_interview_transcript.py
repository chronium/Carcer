import tempfile
import unittest
from pathlib import Path

from harness.codex_activity import CodexActivityKind, RenderableCodexActivity
from harness.exit_interview_transcript import (
    ExitInterviewArtifactStore,
    ExitInterviewMetadata,
    ExitInterviewTranscript,
    ExitInterviewTranscriptError,
)


class ExitInterviewTranscriptTests(unittest.TestCase):
    def test_records_ordered_turns_atomically_and_idempotently(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            run = root / "experiment-042"
            repository.mkdir()
            run.mkdir()
            transcript = ExitInterviewTranscript(_metadata("experiment-042", 10))

            transcript.begin_turn(1, "Why this path?\nExact λ\x1b[2J", "turn-1")
            transcript.observe(
                RenderableCodexActivity(
                    CodexActivityKind.AGENT_REASONING_DELTA,
                    {"text": "First ", "summary_index": 0},
                    "reasoning-1",
                ),
                "turn-1",
            )
            transcript.observe(
                RenderableCodexActivity(
                    CodexActivityKind.AGENT_REASONING_DELTA,
                    {"text": "summary.", "summary_index": 0},
                    "reasoning-1",
                ),
                "turn-1",
            )
            transcript.observe(
                RenderableCodexActivity(
                    CodexActivityKind.AGENT_REASONING_SUMMARY,
                    {"summary": ["Authoritative summary.", "Second summary."]},
                    "reasoning-1",
                ),
                "turn-1",
            )
            transcript.finish_turn(
                "turn-1", response="First answer.\n\x07", status="completed"
            )
            transcript.begin_turn(2, "What followed?", "turn-2")
            transcript.finish_turn(
                "turn-2", response="Second answer.", status="completed"
            )

            store = ExitInterviewArtifactStore(repository, run)
            first = store.persist(transcript.snapshot(), "completed")
            second = store.persist(transcript.snapshot(), "completed")

            self.assertIsNotNone(first)
            self.assertIsNotNone(second)
            assert first is not None and second is not None
            self.assertEqual(
                first.relative_path,
                Path("artifacts/interviews/experiment-042/generation-0010.md"),
            )
            self.assertFalse(first.already_recorded)
            self.assertTrue(second.already_recorded)
            text = first.path.read_text(encoding="utf-8")
            for expected in (
                "Run: experiment-042",
                "Generation: 10",
                "Agent Contract: 4",
                "Model: gpt-5.6-sol",
                "Reasoning effort: high",
                "Reasoning summary: auto",
                "Service tier: priority",
                "Interview status: completed",
                "## Question 1",
                "Why this path?\nExact λ\\x1b[2J",
                "Authoritative summary.\n\nSecond summary.",
                "First answer.\n\\x07",
                "## Question 2",
                "What followed?",
                "Second answer.",
            ):
                self.assertIn(expected, text)
            self.assertNotIn("First summary.", text)
            transcript.begin_turn(3, "Late failed turn", "turn-3")
            transcript.finish_turn("turn-3", response=None, status="failed")
            with self.assertRaisesRegex(
                ExitInterviewTranscriptError,
                "conflicting exit-interview artifact",
            ):
                store.persist(transcript.snapshot(), "failed")
            self.assertEqual(first.path.read_text(encoding="utf-8"), text)

    def test_skips_empty_and_rejects_conflicting_existing_artifact(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            run = root / "experiment-043"
            repository.mkdir()
            run.mkdir()
            store = ExitInterviewArtifactStore(repository, run)
            empty = ExitInterviewTranscript(_metadata("experiment-043", 7))
            self.assertIsNone(store.persist(empty.snapshot(), "completed"))
            self.assertFalse((repository / "artifacts").exists())

            transcript = ExitInterviewTranscript(_metadata("experiment-043", 7))
            transcript.begin_turn(1, "Question", "turn")
            transcript.finish_turn("turn", response="Answer", status="completed")
            artifact = store.persist(transcript.snapshot(), "completed")
            self.assertIsNotNone(artifact)
            assert artifact is not None
            artifact.path.write_text(
                "conflicting historical record\n", encoding="utf-8"
            )

            with self.assertRaisesRegex(
                ExitInterviewTranscriptError,
                "conflicting exit-interview artifact",
            ):
                store.persist(transcript.snapshot(), "completed")
            self.assertEqual(
                artifact.path.read_text(encoding="utf-8"),
                "conflicting historical record\n",
            )

    def test_partial_transcript_is_marked_without_inventing_an_answer(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            run = root / "experiment-044"
            repository.mkdir()
            run.mkdir()
            transcript = ExitInterviewTranscript(_metadata("experiment-044", 3))
            transcript.begin_turn(1, "Interrupted question", "turn")
            transcript.observe(
                RenderableCodexActivity(
                    CodexActivityKind.AGENT_REASONING_DELTA,
                    {"text": "Visible partial summary", "summary_index": 0},
                    "reasoning",
                ),
                "turn",
            )

            artifact = ExitInterviewArtifactStore(repository, run).persist(
                transcript.snapshot(), "interrupted"
            )

            self.assertIsNotNone(artifact)
            assert artifact is not None
            text = artifact.path.read_text(encoding="utf-8")
            self.assertIn("Interview status: interrupted", text)
            self.assertIn("Visible partial summary", text)
            self.assertIn("Turn status: running", text)
            self.assertNotIn("### Sol\n", text)


def _metadata(run: str, generation: int) -> ExitInterviewMetadata:
    return ExitInterviewMetadata(
        run,
        generation,
        4,
        "gpt-5.6-sol",
        "high",
        "auto",
        "priority",
    )


if __name__ == "__main__":
    unittest.main()

import hashlib
import json
import tempfile
import unittest
from pathlib import Path

from harness.planning_evidence import (
    PlanningEvidenceError,
    PlanningEvidenceStore,
)


class PlanningEvidenceTests(unittest.TestCase):
    def test_completed_plan_is_exact_immutable_generation_evidence(self) -> None:
        response = "Inspect first.\n\nThen choose independently. Ω"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            evidence = PlanningEvidenceStore(root).begin(16, "thread-sol")
            evidence.record_started("turn-plan")
            identity = evidence.complete("completed", response)

            directory = root / "planning-evidence/generation-0016"
            manifest = json.loads(
                (directory / "manifest.json").read_text(encoding="utf-8")
            )
            encoded = response.encode("utf-8")
            self.assertEqual((directory / "response.txt").read_bytes(), encoded)
            self.assertEqual(identity.size, len(encoded))
            self.assertEqual(identity.sha256, hashlib.sha256(encoded).hexdigest())
            self.assertEqual(manifest["outcome"], "completed")
            self.assertEqual(manifest["thread_id"], "thread-sol")
            self.assertEqual(manifest["turn_id"], "turn-plan")
            self.assertEqual(
                [attempt["outcome"] for attempt in manifest["attempts"]],
                ["completed"],
            )
            self.assertEqual(manifest["response_bytes"], len(encoded))
            self.assertEqual(
                manifest["response_sha256"],
                hashlib.sha256(encoded).hexdigest(),
            )
            with self.assertRaises(PlanningEvidenceError):
                PlanningEvidenceStore(root).begin(16, "another-thread")

    def test_interrupted_and_failed_plans_remain_truthful(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            interrupted = PlanningEvidenceStore(root).begin(4, "thread-4")
            interrupted.record_started("turn-4")
            interrupted.complete("interrupted", "Partial plan.")
            interrupted_manifest = json.loads(
                (
                    root
                    / "planning-evidence/generation-0004/manifest.json"
                ).read_text(encoding="utf-8")
            )
            self.assertEqual(interrupted_manifest["outcome"], "incomplete")
            self.assertEqual(interrupted_manifest["stage"], "awaiting_resume")
            self.assertEqual(
                interrupted_manifest["attempts"][0]["outcome"],
                "interrupted",
            )
            self.assertFalse(
                (root / "planning-evidence/generation-0004/response.txt").exists()
            )
            self.assertEqual(
                (
                    root
                    / "planning-evidence/generation-0004"
                    / interrupted_manifest["attempts"][0]["response_file"]
                ).read_text(encoding="utf-8"),
                "Partial plan.",
            )

            interrupted.record_started("turn-5")
            interrupted.complete("completed", "Final successful plan.")
            failed = PlanningEvidenceStore(root).begin(5, "thread-5")
            failed.record_started("turn-5")
            failed.fail()

            completed_manifest = json.loads(
                (
                    root
                    / "planning-evidence/generation-0004/manifest.json"
                ).read_text(encoding="utf-8")
            )
            failed_manifest = json.loads(
                (
                    root
                    / "planning-evidence/generation-0005/manifest.json"
                ).read_text(encoding="utf-8")
            )
            self.assertEqual(completed_manifest["outcome"], "completed")
            self.assertEqual(
                [
                    attempt["outcome"]
                    for attempt in completed_manifest["attempts"]
                ],
                ["interrupted", "completed"],
            )
            self.assertEqual(
                (
                    root / "planning-evidence/generation-0004/response.txt"
                ).read_text(encoding="utf-8"),
                "Final successful plan.",
            )
            self.assertEqual(failed_manifest["outcome"], "failed")
            self.assertEqual(
                failed_manifest["attempts"][0]["outcome"],
                "failed",
            )
            self.assertNotIn("response_file", failed_manifest)


if __name__ == "__main__":
    unittest.main()

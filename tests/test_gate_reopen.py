from __future__ import annotations

import hashlib
import io
import json
import os
import shutil
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path

from harness import (
    TEST_HARDWARE_PROFILE,
    CodexOSRun,
    ExperimentObservability,
    FeatureRequestStore,
    GenerationGitRecorder,
    RuntimeState,
    SnapshotFile,
)
from harness.codex_generation_worker import _implementor_prompt
from harness.operator_console import main
from tests.test_codex_generation_worker import _build_seed
from tests.test_generation_git import (
    _archive_aborted,
    _archive_completed,
    _create_repository,
    _generation_tag,
    _git,
)


class ArchivedGateReopenTests(unittest.TestCase):
    def test_completed_gate_restores_exact_successor_without_mutating_archive(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            run = Path(temporary) / "run"
            continued_pid: int | None = None
            handoff = "Exact archived handoff λ."
            snapshot = _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
                handoff=handoff,
            )
            before = _file_hashes(run / "generation-0000")
            observability = ExperimentObservability(run)
            observability.record("run_started", None, {})
            observability.record("operator_quit", 0, {"result": "success"})
            observability.close()
            old_events = (run / "events.jsonl").read_bytes()

            reopened_observability = ExperimentObservability(run)
            runtime = CodexOSRun(
                run,
                hardware_profile=TEST_HARDWARE_PROFILE,
                observability=reopened_observability,
            )
            runtime.reopen_at_gate()

            self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
            self.assertEqual(runtime.generation_number, 0)
            self.assertIsNone(runtime.active_pid)
            self.assertEqual(runtime.previous_handoff, handoff)
            pending = runtime.pending_generation_finish
            self.assertIsNotNone(pending)
            self.assertEqual(pending.handoff_message, handoff)
            self.assertEqual(pending.source_snapshot, snapshot)
            self.assertEqual(
                pending.kernel_elf,
                run / "generation-0000" / "successor" / "kernel.elf",
            )
            self.assertEqual(
                pending.iso,
                run / "generation-0000" / "successor" / "codexos.iso",
            )
            self.assertEqual(_file_hashes(run / "generation-0000"), before)

            runtime.stop()
            reopened_observability.close()
            current_events = (run / "events.jsonl").read_bytes()
            self.assertTrue(current_events.startswith(old_events))
            events = [
                json.loads(line)
                for line in current_events.decode("utf-8").splitlines()
            ]
            self.assertEqual(
                [event["sequence"] for event in events],
                list(range(1, len(events) + 1)),
            )
            reopened = [
                event for event in events
                if event["event"] == "run_reopened_at_gate"
            ]
            self.assertEqual(len(reopened), 1)
            self.assertEqual(
                reopened[0]["data"],
                {"latest_outcome": "completed", "successor_selected": True},
            )
            self.assertEqual(
                [event["event"] for event in events].count("run_started"),
                1,
            )

    def test_aborted_gate_has_no_successor_and_retains_rollback_archive(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            run = Path(temporary) / "run"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            _archive_aborted(run, 1, 0, "successor")

            runtime = CodexOSRun(run, hardware_profile=TEST_HARDWARE_PROFILE)
            runtime.reopen_at_gate()

            self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
            self.assertEqual(runtime.generation_number, 1)
            self.assertIsNone(runtime.active_pid)
            self.assertIsNone(runtime.pending_generation_finish)
            self.assertIsNone(runtime.previous_handoff)
            self.assertEqual(runtime.inspect_generation(0).outcome, "completed")
            runtime.stop()

    def test_reopen_rejects_non_authoritative_gate_state(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            empty = CodexOSRun(root / "empty")
            with self.assertRaisesRegex(RuntimeError, "no archived generation"):
                empty.reopen_at_gate()

            malformed = root / "malformed"
            archive = malformed / "generation-0000"
            archive.mkdir(parents=True)
            (archive / "metadata.json").write_text("not JSON", encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "archive is invalid"):
                CodexOSRun(malformed).reopen_at_gate()

            inconsistent = root / "inconsistent"
            _archive_completed(
                inconsistent,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"zero\n")],
            )
            _archive_completed(
                inconsistent,
                2,
                0,
                "rollback",
                [SnapshotFile("seed/kernel.c", b"two\n")],
            )
            with self.assertRaisesRegex(ValueError, "not contiguous"):
                CodexOSRun(inconsistent).reopen_at_gate()

            missing_successor = root / "missing-successor"
            _archive_completed(
                missing_successor,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            (
                missing_successor
                / "generation-0000"
                / "successor"
                / "codexos.iso"
            ).unlink()
            with self.assertRaisesRegex(ValueError, "artifact is missing"):
                CodexOSRun(missing_successor).reopen_at_gate()

            partial = root / "partial"
            _archive_completed(
                partial,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            (partial / ".generation-0001-active").mkdir()
            with self.assertRaisesRegex(RuntimeError, "partial generation state"):
                CodexOSRun(partial).reopen_at_gate()

    def test_feature_and_git_provenance_continue_across_reopen(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "experiment-reopen"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
            )
            store = FeatureRequestStore(run)
            approved = store.create(0, "Approved capability", "Exact text λ")
            pending = store.create(0, "Still pending", "Not advertised")
            denied = store.create(0, "Denied capability", "Not advertised")
            store.approve(approved.id)
            store.deny(denied.id)

            repository, _ = _create_repository(root / "repository")
            recorder = GenerationGitRecorder(repository, run, "test-base")
            first = recorder.reconcile()
            self.assertEqual(len(first), 1)
            tag = _generation_tag(run, 0)
            tag_target = _git(repository, "rev-parse", tag).strip()
            tag_object = _git(repository, "cat-file", "tag", tag)

            runtime = CodexOSRun(run, hardware_profile=TEST_HARDWARE_PROFILE)
            runtime.reopen_at_gate()
            requests = runtime.feature_requests()
            self.assertEqual(
                [(item.id, item.status) for item in requests],
                [
                    (approved.id, "approved"),
                    (pending.id, "pending"),
                    (denied.id, "denied"),
                ],
            )
            prompt = _implementor_prompt(runtime, None)
            approved_text = (
                f"#{approved.id}: {approved.title}\n{approved.description}"
            )
            self.assertIn(approved_text, prompt)
            self.assertNotIn(pending.title, prompt)
            self.assertNotIn(denied.title, prompt)

            second = recorder.reconcile()
            self.assertEqual(len(second), 1)
            self.assertTrue(second[0].already_recorded)
            self.assertEqual(_git(repository, "rev-parse", tag).strip(), tag_target)
            self.assertEqual(_git(repository, "cat-file", "tag", tag), tag_object)
            runtime.stop()

    def test_cli_requires_an_explicit_new_run_or_gate_reopen_mode(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = io.StringIO()
            result = main(
                ["--run-directory", str(root / "empty"), "--resume-at-gate"],
                io.StringIO(""),
                output,
            )
            self.assertEqual(result, 1)
            self.assertIn("no archived generation gate", output.getvalue())

            with redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    main(
                        [
                            "--run-directory",
                            str(root / "invalid"),
                            "--initial-iso",
                            str(root / "seed.iso"),
                            "--resume-at-gate",
                        ]
                    )


class ArchivedGateReopenQemuIntegrationTest(unittest.TestCase):
    def test_completed_gate_reopens_and_boots_only_after_explicit_continue(
        self,
    ) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        handoff = "Disposable gate-reopen integration handoff."

        with tempfile.TemporaryDirectory() as temporary:
            run = Path(temporary) / "run"
            first = CodexOSRun(
                run,
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            try:
                first.start(image)
                build = first.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = first.invoke_tool(
                    "finish_generation",
                    [handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0, finish.output.decode())
                self.assertIs(first.state, RuntimeState.AWAITING_NEXT_GENERATION)
                archive_hashes = _file_hashes(run / "generation-0000")
            finally:
                first.stop()

            reopened = CodexOSRun(
                run,
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            try:
                reopened.reopen_at_gate()
                self.assertIsNone(reopened.active_pid)
                self.assertEqual(_file_hashes(run / "generation-0000"), archive_hashes)
                reopened.continue_generation()
                self.assertIs(reopened.state, RuntimeState.RUNNING)
                self.assertEqual(reopened.generation_number, 1)
                self.assertIsNotNone(reopened.active_pid)
                continued_pid = reopened.active_pid
                self.assertEqual(reopened.previous_handoff, handoff)
                self.assertIn("list", reopened.list_tools())
                self.assertEqual(_file_hashes(run / "generation-0000"), archive_hashes)
            finally:
                reopened.stop()
            self.assertIsNotNone(continued_pid)
            with self.assertRaises(ProcessLookupError):
                os.kill(continued_pid, 0)
            self.assertEqual(
                [path.name for path in run.iterdir() if path.name.startswith(".")],
                [],
            )


def _file_hashes(root: Path) -> dict[str, str]:
    return {
        path.relative_to(root).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


if __name__ == "__main__":
    unittest.main()

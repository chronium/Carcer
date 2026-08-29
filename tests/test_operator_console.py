import io
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from collections.abc import Callable
from pathlib import Path
from unittest.mock import Mock

from harness import (
    ArchivedGeneration,
    CodexOSRun,
    PendingGenerationFinish,
    RuntimeState,
)
from harness.operator_console import OperatorConsole, main


class OperatorConsoleCommandTests(unittest.TestCase):
    def test_help_status_input_errors_and_runtime_errors(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(Path(temporary), RuntimeState.RUNNING)
            runtime.generation_number = 7
            runtime.active_pid = 12345
            runtime.previous_handoff = "Continue carefully."
            runtime.pause.side_effect = RuntimeError("pause failed")
            runtime.continue_generation.side_effect = RuntimeError(
                "run is not awaiting a generation"
            )
            output = io.StringIO()
            OperatorConsole(
                runtime,
                io.StringIO(
                    "help\nstatus\ninspect\nrollback nope\nunknown\n"
                    "pause\ncontinue\nquit\ny\n"
                ),
                output,
            ).run()

            text = output.getvalue()
            self.assertIn("pause       pause the running generation", text)
            self.assertIn("Generation: 7", text)
            self.assertIn("State: RUNNING", text)
            self.assertIn("Previous handoff:", text)
            self.assertIn("Usage: inspect N", text)
            self.assertIn("Usage: rollback N", text)
            self.assertIn("Unknown command. Type 'help'.", text)
            self.assertIn("Error: pause failed", text)
            self.assertIn("Error: run is not awaiting a generation", text)
            self.assertGreaterEqual(text.count("codexos> "), 8)

    def test_abort_and_rollback_require_literal_confirmation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(Path(temporary), RuntimeState.RUNNING)
            runtime.generation_number = 1

            def abort() -> None:
                runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                runtime.active_pid = None

            def rollback(parent: int) -> None:
                self.assertEqual(parent, 0)
                runtime.generation_number = 2
                runtime.state = RuntimeState.RUNNING
                runtime.active_pid = 2222

            runtime.abort_generation.side_effect = abort
            runtime.fork_from_generation.side_effect = rollback
            output = io.StringIO()
            OperatorConsole(
                runtime,
                io.StringIO(
                    "abort\nno\nabort\nY\nrollback 0\nno\n"
                    "rollback 0\ny\nquit\ny\n"
                ),
                output,
            ).run()

            self.assertEqual(runtime.abort_generation.call_count, 1)
            runtime.fork_from_generation.assert_called_once_with(0)
            text = output.getvalue()
            self.assertIn("Abort cancelled.", text)
            self.assertIn("Generation 1 aborted.", text)
            self.assertIn("Rollback cancelled.", text)
            self.assertIn("Generation 2 started from generation 0.", text)

    def test_quit_confirmation_eof_cleanup_and_awaiting_quit(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            paused = _mock_runtime(root / "paused", RuntimeState.PAUSED)
            paused_output = io.StringIO()
            OperatorConsole(
                paused,
                io.StringIO("quit\nno\nquit\ny\n"),
                paused_output,
            ).run()
            self.assertIn("Quit cancelled.", paused_output.getvalue())
            self.assertIn(
                "Stop the run without archiving generation 0?",
                paused_output.getvalue(),
            )

            awaiting = _mock_runtime(
                root / "awaiting",
                RuntimeState.AWAITING_NEXT_GENERATION,
            )
            awaiting_output = io.StringIO()
            OperatorConsole(
                awaiting,
                io.StringIO("quit\n"),
                awaiting_output,
            ).run()
            self.assertNotIn(
                "Stop the run without archiving",
                awaiting_output.getvalue(),
            )

            running = _mock_runtime(root / "eof", RuntimeState.RUNNING)
            eof_output = io.StringIO()
            OperatorConsole(running, io.StringIO(""), eof_output).run()
            self.assertIn("Input closed; stopping CodexOS run.", eof_output.getvalue())
            self.assertTrue(running.stop.called)

            interrupted = _mock_runtime(
                root / "interrupted",
                RuntimeState.RUNNING,
            )
            interrupted_input = Mock()
            interrupted_input.readline.side_effect = KeyboardInterrupt
            interrupted_output = io.StringIO()
            OperatorConsole(
                interrupted,
                interrupted_input,
                interrupted_output,
            ).run()
            self.assertIn(
                "Interrupted; stopping CodexOS run.",
                interrupted_output.getvalue(),
            )
            self.assertTrue(interrupted.stop.called)

    def test_inspect_aborted_archive_and_cli_startup_failure(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = _mock_runtime(root / "run", RuntimeState.STOPPED)
            runtime.inspect_generation.return_value = ArchivedGeneration(
                generation=1,
                parent_generation=0,
                transition="successor",
                outcome="aborted",
                archive_path=root / "run" / "generation-0001",
                handoff=None,
            )
            output = io.StringIO()
            OperatorConsole(
                runtime,
                io.StringIO("inspect 1\nquit\n"),
                output,
            ).run()
            text = output.getvalue()
            self.assertIn("Outcome: aborted", text)
            self.assertIn("Generation aborted by operator.", text)
            self.assertNotIn("source snapshot", text)

            cli_output = io.StringIO()
            result = main(
                [
                    "--run-directory",
                    str(root / "cli-run"),
                    "--initial-iso",
                    str(root / "missing.iso"),
                ],
                io.StringIO(""),
                cli_output,
            )
            self.assertEqual(result, 1)
            self.assertIn("failed to start CodexOS", cli_output.getvalue())

            malformed_run = root / "malformed-run"
            malformed_archive = malformed_run / "generation-0000"
            malformed_archive.mkdir(parents=True)
            (malformed_archive / "metadata.json").write_bytes(b"not JSON")
            malformed_output = io.StringIO()
            OperatorConsole(
                CodexOSRun(malformed_run),
                io.StringIO("history\nquit\n"),
                malformed_output,
            ).run()
            self.assertIn(
                "Error: generation 0 archive is invalid",
                malformed_output.getvalue(),
            )


class OperatorConsoleIntegrationTest(unittest.TestCase):
    def test_real_console_dispatches_generation_controls(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* OPERATOR-CONSOLE-SUCCESSOR */\n"
        handoff = "Continue from the console-selected successor."

        with tempfile.TemporaryDirectory() as temporary:
            run_directory = Path(temporary) / "run"
            runtime = CodexOSRun(run_directory, qemu)
            observed: dict[str, int] = {}
            try:
                runtime.start(image)
                write = runtime.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel)).encode("ascii"),
                        mutation,
                    ],
                )
                self.assertEqual(write.status, 0)
                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0)
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )

                def before_pause() -> None:
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    observed["generation_one"] = runtime.active_pid

                def before_resume() -> None:
                    self.assertIs(runtime.state, RuntimeState.PAUSED)
                    self.assertEqual(
                        runtime.active_pid,
                        observed["generation_one"],
                    )

                def before_abort() -> None:
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    self.assertEqual(
                        runtime.active_pid,
                        observed["generation_one"],
                    )

                def before_rollback() -> None:
                    self.assertIs(
                        runtime.state,
                        RuntimeState.AWAITING_NEXT_GENERATION,
                    )
                    self.assertEqual(runtime.generation_number, 1)
                    self.assertIsNone(runtime.active_pid)

                def before_quit() -> None:
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    self.assertEqual(runtime.generation_number, 2)
                    observed["generation_two"] = runtime.active_pid

                scripted_input = _ObservedInput(
                    [
                        ("status\n", None),
                        ("history\n", None),
                        ("inspect 0\n", None),
                        ("continue\n", None),
                        ("pause\n", before_pause),
                        ("resume\n", before_resume),
                        ("abort\n", before_abort),
                        ("y\n", None),
                        ("rollback 0\n", before_rollback),
                        ("y\n", None),
                        ("quit\n", before_quit),
                        ("y\n", None),
                    ]
                )
                output = io.StringIO()
                OperatorConsole(runtime, scripted_input, output).run()

                self.assertIs(runtime.state, RuntimeState.STOPPED)
                self.assertIsNone(runtime.active_pid)
                for pid in observed.values():
                    with self.assertRaises(ProcessLookupError):
                        os.kill(pid, 0)
                generation_zero = run_directory / "generation-0000"
                generation_one = run_directory / "generation-0001"
                self.assertTrue(generation_zero.is_dir())
                self.assertTrue(generation_one.is_dir())
                self.assertFalse((run_directory / "generation-0002").exists())
                self.assertEqual(
                    json.loads((generation_one / "metadata.json").read_text())[
                        "outcome"
                    ],
                    "aborted",
                )

                text = output.getvalue()
                self.assertIn("Generation 0 complete", text)
                self.assertIn("Generation 1 paused", text)
                self.assertIn("Generation 1 resumed", text)
                self.assertIn("Generation 1 aborted", text)
                self.assertIn("Generation 2 started from generation 0", text)
                self.assertIn("GEN   PARENT   TRANSITION   OUTCOME", text)
                self.assertIn("Outcome: completed", text)
            finally:
                runtime.stop()

        self.assertEqual(
            (repository / "seed" / "kernel.c").read_bytes(),
            original_kernel,
        )


class _ObservedInput:
    def __init__(
        self,
        steps: list[tuple[str, Callable[[], None] | None]],
    ) -> None:
        self._steps = iter(steps)

    def readline(self) -> str:
        try:
            line, observation = next(self._steps)
        except StopIteration:
            return ""
        if observation is not None:
            observation()
        return line


def _mock_runtime(run_directory: Path, state: RuntimeState) -> Mock:
    runtime = Mock(spec=CodexOSRun)
    runtime.run_directory = run_directory
    runtime.state = state
    runtime.generation_number = 0
    runtime.active_pid = 1111 if state in {
        RuntimeState.RUNNING,
        RuntimeState.PAUSED,
    } else None
    runtime.previous_handoff = None
    runtime.pending_generation_finish = None
    runtime.archived_generations.return_value = []

    def stop() -> None:
        runtime.state = RuntimeState.STOPPED
        runtime.active_pid = None

    runtime.stop.side_effect = stop
    return runtime


def _build_seed(repository: Path) -> Path:
    make = shutil.which("make")
    if make is None:
        raise AssertionError("make must be installed")
    build = subprocess.run(
        [make, "seed"],
        cwd=repository,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=60.0,
        check=False,
    )
    if build.returncode != 0:
        raise AssertionError(build.stdout)
    return repository / "build" / "seed" / "codexos-seed.iso"


if __name__ == "__main__":
    unittest.main()

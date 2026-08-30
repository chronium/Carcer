import contextlib
import io
import json
import os
import shutil
import subprocess
import tempfile
import threading
import unittest
from collections.abc import Callable
from pathlib import Path
from unittest.mock import Mock

from harness import (
    TEST_HARDWARE_PROFILE,
    ArchivedGeneration,
    CodexGenerationWorkerError,
    CodexOSRun,
    FeatureRequest,
    GenerationGitRecorder,
    GenerationGitRecorderError,
    PendingGenerationFinish,
    RuntimeState,
    ToolResult,
)
from harness.operator_console import OperatorConsole, main
from harness.codex_generation_worker import CodexGenerationSessionMode
from tests.test_codex_generation_worker import (
    _assert_process_dead,
    _fake_codex,
    _wait_for,
)

_TEST_HARDWARE = TEST_HARDWARE_PROFILE.manifest(
    "QEMU emulator version test"
)


class OperatorConsoleCommandTests(unittest.TestCase):
    def test_historical_gate_does_not_reconstruct_an_interview_session(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(
                Path(temporary) / "run", RuntimeState.AWAITING_NEXT_GENERATION
            )
            runtime.pending_generation_finish = PendingGenerationFinish(
                "historical handoff",
                b"snapshot",
                Path(temporary) / "kernel",
                Path(temporary) / "iso",
            )
            output = io.StringIO()
            console = OperatorConsole(runtime, io.StringIO(), output)

            console.execute_line("interview")

            self.assertIsNone(console._session)
            self.assertIn("No live generation session", output.getvalue())
            self.assertIn("original Sol thread", output.getvalue())

    def test_gate_transitions_close_retained_session_before_runtime_changes(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(
                Path(temporary) / "run", RuntimeState.AWAITING_NEXT_GENERATION
            )
            runtime.pending_generation_finish = PendingGenerationFinish(
                "handoff", b"snapshot", Path(temporary) / "kernel", Path(temporary) / "iso"
            )
            calls: list[str] = []
            session = Mock()
            session.mode = CodexGenerationSessionMode.RETAINED_AT_GATE
            session.close.side_effect = lambda: calls.append("close")
            runtime.continue_generation.side_effect = lambda: calls.append("continue")
            console = OperatorConsole(runtime, io.StringIO(), io.StringIO())
            console._session = session
            console._session_generation = 0

            console._continue_generation()

            self.assertEqual(calls, ["close", "continue"])
            self.assertIsNone(console._session)

            rollback_session = Mock()
            rollback_session.mode = CodexGenerationSessionMode.RETAINED_AT_GATE
            rollback_session.close.side_effect = lambda: calls.append("rollback-close")
            runtime.fork_from_generation.side_effect = lambda parent: calls.append(
                f"fork-{parent}"
            )
            console._session = rollback_session
            console._session_generation = 0
            console._confirmation_handler = lambda prompt: True

            console._rollback(0)

            self.assertEqual(calls[-2:], ["rollback-close", "fork-0"])
            self.assertIsNone(console._session)

    def test_shutdown_closes_idle_retained_interview_session_idempotently(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(
                Path(temporary) / "run", RuntimeState.AWAITING_NEXT_GENERATION
            )
            session = Mock()
            session.mode = CodexGenerationSessionMode.RETAINED_AT_GATE
            console = OperatorConsole(runtime, io.StringIO(), io.StringIO())
            console._session = session
            console._session_generation = 0

            console.shutdown()
            console.shutdown()

            session.close.assert_called_once_with()
            runtime.stop.assert_called_once_with()

    def test_feature_request_terminal_rendering_escapes_untrusted_controls(
        self,
    ) -> None:
        hostile_title = "Capability λ\nFake status\r\x1b[2J\t\x00\u0085"
        hostile_description = (
            "Normal Unicode: Δ\nSecond line\x1b[31m\x07\r\u009b"
        )
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary)
            request = runtime._feature_request_store.create(
                3,
                hostile_title,
                hostile_description,
            )
            runtime._state = RuntimeState.AWAITING_NEXT_GENERATION
            runtime._generation_number = 3
            output = io.StringIO()
            OperatorConsole(
                runtime,
                io.StringIO("features\nfeature 1\nquit\n"),
                output,
            ).run()

            persisted = runtime.feature_request(request.id)
            self.assertEqual(persisted.title, hostile_title)
            self.assertEqual(persisted.description, hostile_description)

            text = output.getvalue()
            safe_title = (
                r"Capability λ\nFake status\r\x1b[2J\t\x00\x85"
            )
            self.assertIn(safe_title, text)
            self.assertNotIn("\nFake status", text)
            self.assertNotIn("\x1b", text)
            self.assertNotIn("\u0085", text)
            self.assertNotIn("\u009b", text)
            self.assertIn("  Normal Unicode: Δ\n", text)
            self.assertIn(
                r"  Second line\x1b[31m\x07\r\x9b",
                text,
            )

    def test_feature_request_commands_confirm_and_persist_gate_decisions(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary)
            first = runtime._feature_request_store.create(
                3,
                "Capability λ",
                "First line.\nSecond line.",
            )
            second = runtime._feature_request_store.create(
                4,
                "Another capability",
                "",
            )
            third = runtime._feature_request_store.create(
                4,
                "Still pending",
                "May cross a generation gate.",
            )
            runtime._state = RuntimeState.AWAITING_NEXT_GENERATION
            runtime._generation_number = 4
            output = io.StringIO()
            console = OperatorConsole(
                runtime,
                io.StringIO(
                    "status\n"
                    "features\n"
                    "feature 1\n"
                    "feature-approve 1\nn\n"
                    "feature-approve 1\ny\n"
                    "feature-deny 2\nY\n"
                    "quit\n"
                ),
                output,
            )
            console.run()

            self.assertEqual(runtime.feature_request(first.id).status, "approved")
            self.assertEqual(runtime.feature_request(second.id).status, "denied")
            self.assertEqual(runtime.feature_request(third.id).status, "pending")
            text = output.getvalue()
            self.assertIn("ID   GEN   STATUS     TITLE", text)
            self.assertIn("Feature request: #1", text)
            self.assertIn("  Second line.", text)
            self.assertIn("Feature approval cancelled.", text)
            self.assertIn("Feature request #1 approved.", text)
            self.assertIn("Feature request #2 denied.", text)
            self.assertIn("Pending feature requests: 3", text)
            self.assertIn("#3  Still pending", text)

    def test_pending_feature_request_does_not_block_explicit_continuation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(
                Path(temporary) / "run",
                RuntimeState.AWAITING_NEXT_GENERATION,
            )
            runtime.feature_requests.return_value = (
                FeatureRequest(1, 0, "Pending", "Description", "pending"),
            )
            runtime.pending_generation_finish = PendingGenerationFinish(
                "handoff",
                b"snapshot",
                Path(temporary) / "kernel.elf",
                Path(temporary) / "codexos.iso",
            )
            output = io.StringIO()
            console = OperatorConsole(runtime, io.StringIO(), output)

            console._continue_generation()

            runtime.continue_generation.assert_called_once_with()

    def test_feature_decision_outside_gate_remains_runtime_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary)
            request = runtime._feature_request_store.create(0, "Title", "Description")
            console = OperatorConsole(runtime, io.StringIO("y\n"), io.StringIO())
            with self.assertRaisesRegex(RuntimeError, "only while awaiting"):
                console._approve_feature(request.id)
            self.assertEqual(runtime.feature_request(request.id).status, "pending")

    def test_git_reconciliation_startup_retry_and_failure_is_bookkeeping(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = _mock_runtime(
                Path(temporary) / "run",
                RuntimeState.AWAITING_NEXT_GENERATION,
            )
            recorder = Mock(spec=GenerationGitRecorder)
            recorder.reconcile.side_effect = [
                GenerationGitRecorderError("temporary local failure"),
                [],
                GenerationGitRecorderError("retryable conflict"),
            ]
            output = io.StringIO()
            console = OperatorConsole(
                runtime,
                io.StringIO("git-record\nquit\n"),
                output,
                git_recorder=recorder,
            )
            console.run()

            self.assertEqual(recorder.reconcile.call_count, 2)
            self.assertIn(
                "Git provenance error: temporary local failure",
                output.getvalue(),
            )
            self.assertIn("Git provenance is up to date.", output.getvalue())

            runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
            console._execute(["git-record"])
            self.assertIs(
                runtime.state,
                RuntimeState.AWAITING_NEXT_GENERATION,
            )
            self.assertIn(
                "Git provenance error: retryable conflict",
                output.getvalue(),
            )

    def test_git_record_reports_unconfigured_and_cli_requires_both_options(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = _mock_runtime(root / "run", RuntimeState.STOPPED)
            output = io.StringIO()
            OperatorConsole(
                runtime,
                io.StringIO("git-record\nquit\n"),
                output,
            ).run()
            self.assertIn("Git provenance is not configured.", output.getvalue())

            with contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error:
                    main(
                        [
                            "--run-directory",
                            str(root / "cli-run"),
                            "--initial-iso",
                            str(root / "seed.iso"),
                            "--git-repository",
                            str(root / "repository"),
                        ],
                        io.StringIO(),
                        io.StringIO(),
                    )
            self.assertEqual(error.exception.code, 2)

    def test_agent_reuses_one_generation_session_only_on_explicit_command(self) -> None:
        scenario = {
            "turns": [
                {"final_message": "First idle result."},
                {"final_message": "Second idle result."},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
                output = io.StringIO()
                holder: dict[str, OperatorConsole] = {}

                def wait_for_idle() -> None:
                    _wait_for(
                        lambda: holder["console"]._turn_thread is None
                    )

                scripted_input = _ObservedInput(
                    [
                        ("agent\n", None),
                        ("status\n", wait_for_idle),
                        ("agent\n", None),
                        ("status\n", wait_for_idle),
                        ("quit\n", None),
                        ("y\n", None),
                    ]
                )
                console = OperatorConsole(
                    runtime,
                    scripted_input,
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                holder["console"] = console
                console.run()

                record = fake.record()
                methods = [item.get("method") for item in record["messages"]]
                self.assertEqual(methods.count("thread/start"), 1)
                self.assertEqual(methods.count("turn/start"), 2)
                self.assertNotIn("thread/resume", methods)
                self.assertNotIn("thread/fork", methods)
                self.assertIn("First idle result.", output.getvalue())
                self.assertIn("Second idle result.", output.getvalue())
                self.assertEqual(
                    output.getvalue().count("Codex turn started for generation 0."),
                    2,
                )

    def test_pause_does_not_touch_qemu_when_turn_interrupt_never_completes(
        self,
    ) -> None:
        scenario = {
            "turns": [
                {
                    "hold_for_interrupt": True,
                    "interrupt_terminal": False,
                }
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
                console = OperatorConsole(
                    runtime,
                    io.StringIO(),
                    io.StringIO(),
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                    interrupt_timeout_seconds=0.05,
                )
                console._start_agent()
                _wait_for(
                    lambda: console._session is not None
                    and console._session.active_turn
                )
                with self.assertRaisesRegex(
                    RuntimeError,
                    "did not reach interrupted state",
                ):
                    console._pause()
                runtime.pause.assert_not_called()
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                console._terminate_agent_session(interrupt=False)

    def test_pause_resume_reuses_the_active_generation_session(self) -> None:
        scenario = {
            "turns": [
                {"hold_for_interrupt": True},
                {"final_message": "Continued after operator pause."},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)

                def pause() -> None:
                    runtime.state = RuntimeState.PAUSED

                def resume() -> None:
                    runtime.state = RuntimeState.RUNNING

                runtime.pause.side_effect = pause
                runtime.resume.side_effect = resume
                output = io.StringIO()
                console = OperatorConsole(
                    runtime,
                    io.StringIO(),
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                console._start_agent()
                _wait_for(
                    lambda: console._session is not None
                    and console._session.active_turn
                )
                session = console._session
                pid = session.process_pid
                thread_id = session.thread_id

                console._pause()
                self.assertIs(runtime.state, RuntimeState.PAUSED)
                self.assertIs(console._session, session)
                self.assertFalse(session.active_turn)
                console._resume()
                _wait_for(lambda: console._turn_thread is None)

                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(session.process_pid, pid)
                self.assertEqual(session.thread_id, thread_id)
                record = fake.record()
                turns = [
                    item
                    for item in record["messages"]
                    if item.get("method") == "turn/start"
                ]
                self.assertEqual(len(turns), 2)
                self.assertEqual(
                    turns[1]["params"]["input"][0]["text"],
                    "Continue working on the current CodexOS generation "
                    "after the operator pause.",
                )
                self.assertIn("same session", output.getvalue())
                console._terminate_agent_session(interrupt=False)

    def test_pause_fails_if_interrupted_tool_does_not_quiesce(self) -> None:
        scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {"tool": "build", "arguments": {}},
                    ]
                }
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
                tool_started = threading.Event()
                release_tool = threading.Event()

                def invoke_tool(name: str, arguments: list[bytes]) -> ToolResult:
                    tool_started.set()
                    release_tool.wait(1.0)
                    return ToolResult(0, b"")

                runtime.invoke_tool.side_effect = invoke_tool
                console = OperatorConsole(
                    runtime,
                    io.StringIO(),
                    io.StringIO(),
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                    interrupt_timeout_seconds=0.05,
                )
                console._start_agent()
                self.assertTrue(tool_started.wait(1.0))

                with self.assertRaisesRegex(
                    CodexGenerationWorkerError,
                    "did not quiesce",
                ):
                    console._pause()

                runtime.pause.assert_not_called()
                runtime.abort_generation.assert_not_called()
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                release_tool.set()
                session = console._session
                self.assertIsNotNone(session)
                assert session is not None
                _wait_for(lambda: session._active_tool_calls == 0)
                console._terminate_agent_session(interrupt=False)

    def test_status_history_inspect_and_quit_remain_available_during_turn(
        self,
    ) -> None:
        scenario = {"turns": [{"hold_for_interrupt": True}]}
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
                runtime.archived_generations.return_value = [
                    ArchivedGeneration(
                        generation=0,
                        parent_generation=None,
                        transition="initial",
                        outcome="completed",
                        archive_path=root / "run" / "generation-0000",
                        handoff="Archived handoff.",
                        hardware=_TEST_HARDWARE,
                    )
                ]
                runtime.inspect_generation.return_value = (
                    runtime.archived_generations.return_value[0]
                )
                output = io.StringIO()
                holder: dict[str, OperatorConsole] = {}

                def wait_active() -> None:
                    _wait_for(
                        lambda: holder["console"]._session is not None
                        and holder["console"]._session.active_turn
                    )

                console = OperatorConsole(
                    runtime,
                    _ObservedInput(
                        [
                            ("agent\n", None),
                            ("status\n", wait_active),
                            ("history\n", None),
                            ("inspect 0\n", None),
                            ("quit\n", None),
                            ("y\n", None),
                        ]
                    ),
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                holder["console"] = console
                console.run()

                text = output.getvalue()
                self.assertIn("Codex session: active", text)
                self.assertIn("Codex turn: running", text)
                self.assertIn("GEN   PARENT", text)
                self.assertIn("Outcome: completed", text)
                self.assertIn(
                    f"Hardware profile: {TEST_HARDWARE_PROFILE.profile}",
                    text,
                )
                self.assertIn(
                    f"Profile: {TEST_HARDWARE_PROFILE.profile}",
                    text,
                )
                self.assertIn(
                    "CPU: "
                    f"{TEST_HARDWARE_PROFILE.cpu_model} x "
                    f"{TEST_HARDWARE_PROFILE.vcpus}",
                    text,
                )
                runtime.abort_generation.assert_not_called()
                self.assertTrue(runtime.stop.called)
                _assert_process_dead(self, fake.record()["pid"])

    def test_cooperative_finish_retains_interview_then_next_agent_is_fresh(
        self,
    ) -> None:
        marker = "EXIT-INTERVIEW-SECRET-91f-console"
        first_scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "tool": "finish_generation",
                            "arguments": {"handoff": "Fresh successor context."},
                        }
                    ]
                },
                {
                    "visible_activity": [
                        {
                            "kind": "reasoning_completed",
                            "summary": ["First interview summary."],
                        }
                    ],
                    "final_message": "Retrospective answer.",
                },
                {
                    "visible_activity": [
                        {
                            "kind": "reasoning_completed",
                            "summary": ["Second interview summary."],
                        }
                    ],
                    "final_message": "Second retrospective answer.",
                },
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            repository.mkdir()
            fake = _fake_codex(first_scenario, root / "fake-codex")
            runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
            pending = PendingGenerationFinish(
                "Fresh successor context.",
                b"snapshot",
                root / "kernel.elf",
                root / "codexos.iso",
            )

            def invoke(name: str, arguments: list[bytes]) -> ToolResult:
                if name == "finish_generation":
                    runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                    runtime.active_pid = None
                    runtime.pending_generation_finish = pending
                    runtime.previous_handoff = pending.handoff_message
                return ToolResult(0, b"")

            def continue_generation() -> None:
                runtime.generation_number = 1
                runtime.state = RuntimeState.RUNNING
                runtime.active_pid = 2222
                runtime.pending_generation_finish = None
                runtime.previous_handoff = pending.handoff_message

            runtime.invoke_tool.side_effect = invoke
            runtime.continue_generation.side_effect = continue_generation
            holder: dict[str, OperatorConsole] = {}

            def wait_for_retained_session() -> None:
                console = holder["console"]
                _wait_for(
                    lambda: runtime.state
                    is RuntimeState.AWAITING_NEXT_GENERATION
                    and console.exit_interview_state == "available"
                )

            def wait_for_interview_turn() -> None:
                _wait_for(lambda: holder["console"]._turn_thread is None)

            def prepare_successor() -> None:
                console = holder["console"]
                self.assertIsNone(console._session)
                fake.scenario_path.write_text(
                    json.dumps({"final_message": "Fresh generation turn."}),
                    encoding="utf-8",
                )

            def wait_successor_turn() -> None:
                _wait_for(lambda: holder["console"]._turn_thread is None)

            output = io.StringIO()
            recorder = Mock(spec=GenerationGitRecorder)
            console = OperatorConsole(
                runtime,
                _ObservedInput(
                    [
                        ("agent\n", None),
                        ("interview\n", wait_for_retained_session),
                        (marker + "\n", None),
                        ("What did you verify?\n", wait_for_interview_turn),
                        ("end\n", wait_for_interview_turn),
                        ("continue\n", prepare_successor),
                        ("agent\n", None),
                        ("quit\n", wait_successor_turn),
                        ("y\n", None),
                    ]
                ),
                output,
                codex_executable=str(fake.executable),
                codex_auth_file=fake.auth_file,
                git_recorder=recorder,
                interview_repository=repository,
            )
            holder["console"] = console
            console.run()

            records = fake.records()
            self.assertEqual(len(records), 2)
            generation_record = next(
                record for record in records if len(record["turn_ids"]) == 3
            )
            successor_record = next(
                record for record in records if len(record["turn_ids"]) == 1
            )
            self.assertNotEqual(
                generation_record["pid"], successor_record["pid"]
            )
            self.assertNotEqual(
                generation_record["thread_id"],
                successor_record["thread_id"],
            )
            for record in records:
                methods = [item.get("method") for item in record["messages"]]
                self.assertEqual(methods.count("thread/start"), 1)
                self.assertNotIn("thread/resume", methods)
                self.assertNotIn("thread/fork", methods)
            successor_prompts = [
                item["params"]["input"][0]["text"]
                for record in records
                for item in record["messages"]
                if item.get("method") == "turn/start"
                and "Fresh successor context."
                in item["params"]["input"][0]["text"]
            ]
            self.assertEqual(len(successor_prompts), 1)
            all_successor_text = json.dumps(successor_record, ensure_ascii=False)
            self.assertNotIn(marker, all_successor_text)
            artifact = (
                repository
                / "artifacts"
                / "interviews"
                / "run"
                / "generation-0000.md"
            )
            transcript = artifact.read_text(encoding="utf-8")
            self.assertIn(marker, transcript)
            self.assertIn("## Question 1", transcript)
            self.assertIn("First interview summary.", transcript)
            self.assertIn("Retrospective answer.", transcript)
            self.assertIn("## Question 2", transcript)
            self.assertIn("What did you verify?", transcript)
            self.assertIn("Second interview summary.", transcript)
            self.assertIn("Second retrospective answer.", transcript)
            self.assertIn("Agent Contract: 5", transcript)
            self.assertIn("Interview status: completed", transcript)
            self.assertNotIn("Exit interview question sent.", transcript)
            self.assertNotIn("Generation 0 completed cooperatively", transcript)
            self.assertEqual(runtime.previous_handoff, "Fresh successor context.")
            self.assertIn("Generation 0 completed cooperatively", output.getvalue())
            self.assertIn("Exit interview started", output.getvalue())
            self.assertIn("Retrospective answer", output.getvalue())
            self.assertIn(str(artifact.relative_to(repository)), output.getvalue())
            self.assertEqual(recorder.reconcile.call_count, 2)

    def test_quit_interrupts_interview_and_persists_partial_transcript(self) -> None:
        marker = "EXIT-INTERVIEW-PARTIAL-SECRET"
        scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "tool": "finish_generation",
                            "arguments": {"handoff": "Frozen handoff."},
                        }
                    ]
                },
                {
                    "visible_activity": [
                        {
                            "kind": "reasoning_summary_delta",
                            "text": "Visible partial summary.",
                        },
                        {
                            "kind": "reasoning_text_delta",
                            "text": "PRIVATE RAW REASONING",
                        },
                    ],
                    "hold_for_interrupt": True,
                },
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            repository.mkdir()
            fake = _fake_codex(scenario, root / "fake-codex")
            runtime = _mock_runtime(root / "experiment-quit", RuntimeState.RUNNING)

            def finish(name: str, arguments: list[bytes]) -> ToolResult:
                self.assertEqual(name, "finish_generation")
                runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                runtime.active_pid = None
                runtime.pending_generation_finish = PendingGenerationFinish(
                    "Frozen handoff.",
                    b"frozen snapshot",
                    root / "kernel.elf",
                    root / "codexos.iso",
                )
                runtime.previous_handoff = "Frozen handoff."
                return ToolResult(0, b"")

            runtime.invoke_tool.side_effect = finish
            holder: dict[str, OperatorConsole] = {}

            def wait_for_gate() -> None:
                _wait_for(
                    lambda: holder["console"].exit_interview_state == "available"
                )

            def wait_for_interview_turn() -> None:
                _wait_for(
                    lambda: holder["console"]._session is not None
                    and holder["console"]._session.active_turn
                )

            output = io.StringIO()
            console = OperatorConsole(
                runtime,
                _ObservedInput(
                    [
                        ("agent\n", None),
                        ("interview\n", wait_for_gate),
                        (marker + "\n", None),
                        ("quit\n", wait_for_interview_turn),
                    ]
                ),
                output,
                codex_executable=str(fake.executable),
                codex_auth_file=fake.auth_file,
                interview_repository=repository,
            )
            holder["console"] = console
            console.run()

            artifact = (
                repository
                / "artifacts"
                / "interviews"
                / "experiment-quit"
                / "generation-0000.md"
            )
            transcript = artifact.read_text(encoding="utf-8")
            self.assertIn("Interview status: interrupted", transcript)
            self.assertIn(marker, transcript)
            self.assertIn("Visible partial summary.", transcript)
            self.assertIn("Turn status: interrupted", transcript)
            self.assertNotIn("PRIVATE RAW REASONING", transcript)
            self.assertIs(runtime.state, RuntimeState.STOPPED)
            self.assertIn(str(artifact.relative_to(repository)), output.getvalue())
            _assert_process_dead(self, fake.record()["pid"])

    def test_end_interrupts_active_interview_before_persisting(self) -> None:
        marker = "EXIT-INTERVIEW-END-ACTIVE"
        scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "tool": "finish_generation",
                            "arguments": {"handoff": "Frozen handoff."},
                        }
                    ]
                },
                {
                    "visible_activity": [
                        {
                            "kind": "reasoning_summary_delta",
                            "text": "Captured before explicit end.",
                        }
                    ],
                    "hold_for_interrupt": True,
                },
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository = root / "repository"
            repository.mkdir()
            fake = _fake_codex(scenario, root / "fake-codex")
            runtime = _mock_runtime(root / "experiment-end", RuntimeState.RUNNING)

            def finish(name: str, arguments: list[bytes]) -> ToolResult:
                self.assertEqual(name, "finish_generation")
                runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                runtime.active_pid = None
                runtime.pending_generation_finish = PendingGenerationFinish(
                    "Frozen handoff.",
                    b"frozen snapshot",
                    root / "kernel.elf",
                    root / "codexos.iso",
                )
                runtime.previous_handoff = "Frozen handoff."
                return ToolResult(0, b"")

            runtime.invoke_tool.side_effect = finish
            holder: dict[str, OperatorConsole] = {}

            def wait_for_gate() -> None:
                _wait_for(
                    lambda: holder["console"].exit_interview_state == "available"
                )

            def wait_for_active_interview_turn() -> None:
                _wait_for(
                    lambda: holder["console"]._session is not None
                    and holder["console"]._session.active_turn
                )

            output = io.StringIO()
            console = OperatorConsole(
                runtime,
                _ObservedInput(
                    [
                        ("agent\n", None),
                        ("interview\n", wait_for_gate),
                        (marker + "\n", None),
                        ("end\n", wait_for_active_interview_turn),
                        ("status\n", None),
                        ("quit\n", None),
                    ]
                ),
                output,
                codex_executable=str(fake.executable),
                codex_auth_file=fake.auth_file,
                interview_repository=repository,
            )
            holder["console"] = console
            console.run()

            artifact = (
                repository
                / "artifacts"
                / "interviews"
                / "experiment-end"
                / "generation-0000.md"
            )
            transcript = artifact.read_text(encoding="utf-8")
            self.assertIn("Interview status: interrupted", transcript)
            self.assertNotIn("Interview status: completed", transcript)
            self.assertIn(marker, transcript)
            self.assertIn("Captured before explicit end.", transcript)
            self.assertIn("Turn status: interrupted", transcript)
            self.assertNotIn("### Sol\n", transcript)
            methods = [
                message.get("method") for message in fake.record()["messages"]
            ]
            self.assertEqual(methods.count("turn/interrupt"), 1)
            self.assertIn("State: AWAITING_NEXT_GENERATION", output.getvalue())
            self.assertIn("Codex session: none", output.getvalue())
            self.assertIs(runtime.state, RuntimeState.STOPPED)
            _assert_process_dead(self, fake.record()["pid"])

    def test_failed_turn_after_finish_is_not_retained_for_interview(self) -> None:
        scenario = {
            "tool_calls": [
                {
                    "tool": "finish_generation",
                    "arguments": {"handoff": "Frozen despite turn failure."},
                }
            ],
            "turn_status": "failed",
            "turn_error": {"message": "synthetic terminal failure"},
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)

                def finish(name: str, arguments: list[bytes]) -> ToolResult:
                    runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                    runtime.active_pid = None
                    runtime.pending_generation_finish = PendingGenerationFinish(
                        "Frozen despite turn failure.",
                        b"snapshot",
                        root / "kernel.elf",
                        root / "codexos.iso",
                    )
                    return ToolResult(0, b"")

                runtime.invoke_tool.side_effect = finish
                holder: dict[str, OperatorConsole] = {}

                def wait_for_failure() -> None:
                    _wait_for(
                        lambda: runtime.state
                        is RuntimeState.AWAITING_NEXT_GENERATION
                        and holder["console"]._turn_thread is None
                        and holder["console"]._session is None
                    )

                output = io.StringIO()
                console = OperatorConsole(
                    runtime,
                    _ObservedInput(
                        [("agent\n", None), ("quit\n", wait_for_failure)]
                    ),
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                holder["console"] = console
                console.run()

                self.assertEqual(console.exit_interview_state, "unavailable")
                self.assertIn("Codex session failed", output.getvalue())
                _assert_process_dead(self, fake.record()["pid"])

    def test_eof_cleans_up_active_codex_session_before_runtime(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(
                {"turns": [{"hold_for_interrupt": True}]},
                root / "fake-codex",
            ) as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)
                holder: dict[str, OperatorConsole] = {}

                def wait_active() -> None:
                    _wait_for(
                        lambda: holder["console"]._session is not None
                        and holder["console"]._session.active_turn
                    )

                output = io.StringIO()
                console = OperatorConsole(
                    runtime,
                    _ObservedInput(
                        [("agent\n", None), ("status\n", wait_active)]
                    ),
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                holder["console"] = console
                console.run()

                self.assertIn("Input closed", output.getvalue())
                runtime.abort_generation.assert_not_called()
                self.assertTrue(runtime.stop.called)
                _assert_process_dead(self, fake.record()["pid"])

    def test_abort_hard_closes_session_when_interrupt_does_not_finish(self) -> None:
        scenario = {
            "turns": [
                {
                    "hold_for_interrupt": True,
                    "interrupt_terminal": False,
                }
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with _fake_codex(scenario, root / "fake-codex") as fake:
                runtime = _mock_runtime(root / "run", RuntimeState.RUNNING)

                def abort() -> None:
                    runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                    runtime.active_pid = None

                runtime.abort_generation.side_effect = abort
                recorder = Mock(spec=GenerationGitRecorder)
                console = OperatorConsole(
                    runtime,
                    io.StringIO("y\n"),
                    io.StringIO(),
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                    git_recorder=recorder,
                    interrupt_timeout_seconds=0.05,
                )
                console._start_agent()
                _wait_for(
                    lambda: console._session is not None
                    and console._session.active_turn
                )
                pid = console._session.process_pid

                console._abort()

                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIsNone(console._session)
                runtime.abort_generation.assert_called_once_with()
                recorder.reconcile.assert_called_once_with()
                _assert_process_dead(self, pid)

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
                hardware=_TEST_HARDWARE,
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
            self.assertIn("Hardware:", text)
            self.assertIn("Writable disks: none", text)
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
    def test_pause_waits_for_blocked_guest_tool_before_qemu_pause(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "tool": "read",
                            "arguments": {
                                "path": "seed/kernel.c",
                                "offset": 0,
                                "length": 1,
                            },
                        }
                    ]
                },
                {"final_message": "Continued on the same generation."},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            fake = _fake_codex(scenario, root / "fake-codex")
            tool_started = threading.Event()
            release_tool = threading.Event()
            try:
                runtime.start(image)
                qemu_pid = runtime.active_pid
                self.assertIsNotNone(qemu_pid)
                invoke_tool = runtime.invoke_tool

                def blocked_invoke(
                    name: str,
                    arguments: list[bytes],
                ) -> ToolResult:
                    tool_started.set()
                    self.assertTrue(release_tool.wait(2.0))
                    return invoke_tool(name, arguments)

                runtime.invoke_tool = Mock(side_effect=blocked_invoke)
                console = OperatorConsole(
                    runtime,
                    io.StringIO(),
                    io.StringIO(),
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                    interrupt_timeout_seconds=2.0,
                )
                console._start_agent()
                self.assertTrue(tool_started.wait(1.0))
                session = console._session
                self.assertIsNotNone(session)
                assert session is not None
                codex_pid = session.process_pid
                thread_id = session.thread_id
                pause_errors: list[BaseException] = []

                def pause_console() -> None:
                    try:
                        console._pause()
                    except BaseException as error:
                        pause_errors.append(error)

                pause_thread = threading.Thread(target=pause_console)
                pause_thread.start()
                _wait_for(lambda: session._last_turn_status == "interrupted")
                self.assertTrue(pause_thread.is_alive())
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.active_pid, qemu_pid)
                os.kill(qemu_pid, 0)

                release_tool.set()
                pause_thread.join(2.0)
                self.assertFalse(pause_thread.is_alive())
                self.assertEqual(pause_errors, [])
                self.assertIs(runtime.state, RuntimeState.PAUSED)
                self.assertEqual(runtime.active_pid, qemu_pid)
                os.kill(qemu_pid, 0)
                self.assertEqual(session._active_tool_calls, 0)
                self.assertEqual(session.process_pid, codex_pid)
                self.assertEqual(session.thread_id, thread_id)
                self.assertIsNone(console._turn_thread)

                console._resume()
                _wait_for(lambda: console._turn_thread is None)
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.active_pid, qemu_pid)
                self.assertEqual(session.process_pid, codex_pid)
                self.assertEqual(session.thread_id, thread_id)
                console._terminate_agent_session(interrupt=False)
            finally:
                release_tool.set()
                runtime.stop()

    def test_quit_active_turn_stops_qemu_without_archiving(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            fake = _fake_codex(
                {"turns": [{"hold_for_interrupt": True}]},
                root / "fake-codex",
            )
            holder: dict[str, OperatorConsole] = {}
            try:
                runtime.start(image)
                qemu_pid = runtime.active_pid

                def wait_active() -> None:
                    _wait_for(
                        lambda: holder["console"]._session is not None
                        and holder["console"]._session.active_turn
                    )

                console = OperatorConsole(
                    runtime,
                    _ObservedInput(
                        [
                            ("agent\n", None),
                            ("quit\n", wait_active),
                            ("y\n", None),
                        ]
                    ),
                    io.StringIO(),
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                holder["console"] = console
                console.run()

                self.assertIs(runtime.state, RuntimeState.STOPPED)
                self.assertFalse((root / "run" / "generation-0000").exists())
                _assert_process_dead(self, qemu_pid)
                _assert_process_dead(self, fake.record()["pid"])
            finally:
                runtime.stop()

    def test_pause_cancels_active_reviewer_and_preserves_implementor_session(
        self,
    ) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        implementor_scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "namespace": None,
                            "tool": "review",
                            "arguments": {},
                        }
                    ],
                    "hold_for_interrupt": True,
                }
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "turns": [{"hold_for_interrupt": True}],
        }

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            implementor = _fake_codex(
                implementor_scenario,
                root / "implementor",
            )
            reviewer = _fake_codex(reviewer_scenario, root / "reviewer")
            holder: dict[str, OperatorConsole] = {}
            observed: dict[str, int] = {}
            try:
                runtime.start(image)
                qemu_pid = runtime.active_pid

                def before_pause() -> None:
                    _wait_for(lambda: bool(reviewer.records()))
                    console = holder["console"]
                    _wait_for(
                        lambda: console._session is not None
                        and console._session.active_turn
                    )
                    observed["reviewer"] = reviewer.records()[0]["pid"]
                    observed["implementor"] = console._session.process_pid

                def before_quit() -> None:
                    self.assertIs(runtime.state, RuntimeState.PAUSED)
                    self.assertEqual(runtime.active_pid, qemu_pid)
                    _assert_process_dead(self, observed["reviewer"])
                    os.kill(observed["implementor"], 0)

                output = io.StringIO()
                console = OperatorConsole(
                    runtime,
                    _ObservedInput(
                        [
                            ("agent\n", None),
                            ("pause\n", before_pause),
                            ("quit\n", before_quit),
                            ("y\n", None),
                        ]
                    ),
                    output,
                    codex_executable=str(implementor.executable),
                    codex_auth_file=implementor.auth_file,
                    reviewer_codex_executable=str(reviewer.executable),
                    reviewer_auth_file=reviewer.auth_file,
                )
                holder["console"] = console
                console.run()

                self.assertIs(runtime.state, RuntimeState.STOPPED)
                self.assertIsNone(runtime.active_pid)
                _assert_process_dead(self, observed["implementor"])
                self.assertFalse((root / "run" / "generation-0000").exists())
                response = implementor.record()["tool_results"][0]["result"]
                self.assertFalse(response["success"])
                self.assertIn("Generation 0 paused", output.getvalue())
            finally:
                runtime.stop()

    def test_real_console_dispatches_generation_controls(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* OPERATOR-CONSOLE-SUCCESSOR */\n"
        handoff = "Continue from the console-selected successor."

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run_directory = root / "run"
            runtime = CodexOSRun(
                run_directory,
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            observed: dict[str, int] = {}
            agent_observed: dict[str, object] = {}
            fake = _fake_codex(
                {
                    "turns": [
                        {"hold_for_interrupt": True},
                        {"hold_for_interrupt": True},
                    ]
                },
                root / "fake-codex",
            )
            console_holder: dict[str, OperatorConsole] = {}
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
                    console = console_holder["console"]
                    _wait_for(
                        lambda: console._session is not None
                        and console._session.active_turn
                    )
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    observed["generation_one"] = runtime.active_pid
                    agent_observed["pid"] = console._session.process_pid
                    agent_observed["thread"] = console._session.thread_id

                def before_agent() -> None:
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    self.assertEqual(runtime.generation_number, 1)
                    self.assertIsNone(console_holder["console"]._session)

                def before_resume() -> None:
                    console = console_holder["console"]
                    self.assertIs(runtime.state, RuntimeState.PAUSED)
                    self.assertEqual(
                        runtime.active_pid,
                        observed["generation_one"],
                    )
                    self.assertEqual(
                        console._session.process_pid,
                        agent_observed["pid"],
                    )
                    self.assertEqual(
                        console._session.thread_id,
                        agent_observed["thread"],
                    )

                def before_abort() -> None:
                    console = console_holder["console"]
                    _wait_for(
                        lambda: console._session is not None
                        and console._session.active_turn
                    )
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    self.assertEqual(
                        runtime.active_pid,
                        observed["generation_one"],
                    )
                    self.assertEqual(
                        console._session.process_pid,
                        agent_observed["pid"],
                    )
                    self.assertEqual(
                        console._session.thread_id,
                        agent_observed["thread"],
                    )
                    read = runtime.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            b"0",
                            str(len(original_kernel) + len(mutation)).encode(
                                "ascii"
                            ),
                        ],
                    )
                    self.assertEqual(read.status, 0)
                    self.assertEqual(
                        read.output,
                        original_kernel + mutation,
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
                    self.assertIsNone(console_holder["console"]._session)
                    observed["generation_two"] = runtime.active_pid

                scripted_input = _ObservedInput(
                    [
                        ("status\n", None),
                        ("history\n", None),
                        ("inspect 0\n", None),
                        ("continue\n", None),
                        ("agent\n", before_agent),
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
                console = OperatorConsole(
                    runtime,
                    scripted_input,
                    output,
                    codex_executable=str(fake.executable),
                    codex_auth_file=fake.auth_file,
                )
                console_holder["console"] = console
                console.run()

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
                agent_record = fake.record()
                methods = [
                    message.get("method")
                    for message in agent_record["messages"]
                ]
                self.assertEqual(methods.count("thread/start"), 1)
                self.assertEqual(methods.count("turn/start"), 2)
                self.assertEqual(methods.count("turn/interrupt"), 2)
                _assert_process_dead(self, agent_record["pid"])
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
    runtime.feature_requests.return_value = ()
    runtime.run_directory = run_directory
    runtime.state = state
    runtime.generation_number = 0
    runtime.active_pid = 1111 if state in {
        RuntimeState.RUNNING,
        RuntimeState.PAUSED,
    } else None
    runtime.previous_handoff = None
    runtime.pending_generation_finish = None
    runtime.hardware_profile = TEST_HARDWARE_PROFILE
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

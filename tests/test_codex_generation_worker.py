import base64
import json
import os
import shutil
import subprocess
import tempfile
import threading
import time
import tomllib
import unittest
from collections.abc import Callable
from pathlib import Path
from unittest.mock import Mock

from harness import (
    CodexGenerationSession,
    CodexGenerationWorker,
    CodexGenerationWorkerError,
    CodexOSRun,
    RuntimeState,
    ToolResult,
)

_TOOLS = [
    "list",
    "read",
    "write",
    "truncate",
    "remove",
    "build",
    "finish_generation",
]


class CodexGenerationWorkerProtocolTests(unittest.TestCase):
    def test_fresh_protocol_dynamic_tools_validation_and_cleanup(self) -> None:
        scenario = {
            "server_requests": [
                "item/commandExecution/requestApproval",
                "item/permissions/requestApproval",
            ],
            "tool_calls": [
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "length": 3,
                    },
                },
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": -1,
                        "length": 3,
                    },
                },
                {
                    "tool": "write",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "encoding": "base64",
                        "data": "not valid base64!",
                    },
                },
            ],
            "final_message": "Inspected the running guest.",
        }
        with _fake_codex(scenario) as fake:
            runtime = Mock(spec=CodexOSRun)
            runtime.state = RuntimeState.RUNNING
            runtime.previous_handoff = None
            runtime.current_transition = "initial"
            runtime.invoke_tool.return_value = ToolResult(7, b"\xff\x00A")
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            result = worker.run_generation(runtime)
            record = fake.record()

            self.assertEqual(result.turn_status, "completed")
            self.assertEqual(result.final_message, "Inspected the running guest.")
            self.assertEqual(
                result.summary,
                "Codex turn completed; generation is still running.",
            )
            runtime.invoke_tool.assert_called_once_with(
                "read", [b"seed/kernel.c", b"0", b"3"]
            )
            messages = record["messages"]
            methods = [message.get("method") for message in messages]
            self.assertEqual(
                methods[:6],
                [
                    "initialize",
                    "initialized",
                    "account/read",
                    "model/list",
                    "thread/start",
                    "turn/start",
                ],
            )
            self.assertNotIn("thread/resume", methods)
            self.assertNotIn("thread/fork", methods)

            initialize = _request(messages, "initialize")
            self.assertTrue(
                initialize["params"]["capabilities"]["experimentalApi"]
            )
            thread = _request(messages, "thread/start")["params"]
            self.assertTrue(thread["ephemeral"])
            self.assertTrue(thread["cwd"].startswith("/tmp/codexos-codex-worker-"))
            self.assertFalse(
                Path(thread["cwd"]).is_relative_to(Path.cwd())
            )
            self.assertNotEqual(thread["cwd"], record["codex_home"])
            self.assertEqual(thread["permissions"], "codexos-implementor")
            self.assertEqual(thread["approvalPolicy"], "never")
            dynamic_tools = thread["dynamicTools"]
            self.assertEqual(len(dynamic_tools), 2)
            self.assertEqual(dynamic_tools[0]["name"], "codexos")
            self.assertEqual(
                [tool["name"] for tool in dynamic_tools[0]["tools"]],
                _TOOLS,
            )
            self.assertEqual(dynamic_tools[1]["type"], "function")
            self.assertEqual(dynamic_tools[1]["name"], "review")
            config = tomllib.loads(record["config"])
            profile = config["permissions"]["codexos-implementor"]
            self.assertEqual(profile["filesystem"][":root"], "deny")
            self.assertEqual(
                profile["filesystem"][":workspace_roots"]["."],
                "write",
            )
            self.assertFalse(profile["network"]["enabled"])
            self.assertEqual(config["web_search"], "disabled")
            self.assertFalse(config["features"]["apps"])
            self.assertFalse(config["features"]["plugins"])
            self.assertFalse(config["agents"]["enabled"])

            turn = _request(messages, "turn/start")["params"]
            prompt_text = turn["input"][0]["text"]
            self.assertIn(
                "You are developing CodexOS from inside its current "
                "running generation.",
                prompt_text,
            )
            self.assertIn("Previous generation handoff: none.", prompt_text)
            self.assertEqual(turn["effort"], "high")
            self.assertEqual(turn["model"], "gpt-5.6-sol")

            approvals = record["server_responses"]
            self.assertEqual(approvals[0]["result"], {"decision": "decline"})
            self.assertFalse(
                approvals[1]["result"]["permissions"]["network"]["enabled"]
            )
            first_tool = json.loads(
                record["tool_results"][0]["result"]["contentItems"][0][
                    "text"
                ]
            )
            self.assertTrue(record["tool_results"][0]["result"]["success"])
            self.assertEqual(first_tool["status"], 7)
            self.assertEqual(first_tool["encoding"], "base64")
            self.assertEqual(
                base64.b64decode(first_tool["output"]),
                b"\xff\x00A",
            )
            self.assertFalse(record["tool_results"][1]["result"]["success"])
            self.assertFalse(record["tool_results"][2]["result"]["success"])
            _assert_process_dead(self, record["pid"])

    def test_process_cleanup_on_malformed_app_server_output(self) -> None:
        with _fake_codex({"failure": "malformed_json"}) as fake:
            runtime = Mock(spec=CodexOSRun)
            runtime.state = RuntimeState.RUNNING
            runtime.previous_handoff = None
            runtime.current_transition = "initial"
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError, "malformed JSON"
            ):
                worker.run_generation(runtime)
            _assert_process_dead(self, fake.record()["pid"])

    def test_rejects_unavailable_model_without_starting_a_turn(self) -> None:
        with _fake_codex({}) as fake:
            runtime = Mock(spec=CodexOSRun)
            runtime.state = RuntimeState.RUNNING
            runtime.previous_handoff = None
            runtime.current_transition = "initial"
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError, "model is unavailable"
            ):
                worker.run_generation(runtime, model="missing-model")
            record = fake.record()
            methods = [message.get("method") for message in record.get("messages", [])]
            self.assertNotIn("turn/start", methods)
            _assert_process_dead(self, record["pid"])

    def test_surfaces_turn_failure_and_cleans_up(self) -> None:
        scenario = {
            "turn_status": "failed",
            "turn_error": {"message": "synthetic model failure"},
        }
        with _fake_codex(scenario) as fake:
            runtime = Mock(spec=CodexOSRun)
            runtime.state = RuntimeState.RUNNING
            runtime.previous_handoff = None
            runtime.current_transition = "initial"
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError, "synthetic model failure"
            ):
                worker.run_generation(runtime)
            _assert_process_dead(self, fake.record()["pid"])


class CodexGenerationSessionProtocolTests(unittest.TestCase):
    def test_multiple_turns_reuse_one_process_and_thread(self) -> None:
        scenario = {
            "turns": [
                {"final_message": "First turn complete."},
                {"final_message": "Second turn complete."},
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
            )
            first = session.run_initial_turn()
            first_record = fake.record()
            self.assertEqual(first.final_message, "First turn complete.")
            self.assertEqual(
                sum(
                    item.get("method") == "turn/start"
                    for item in first_record["messages"]
                ),
                1,
            )
            pid = session.process_pid
            thread_id = session.thread_id

            second = session.run_continuation_turn()
            record = fake.record()
            self.assertEqual(second.final_message, "Second turn complete.")
            self.assertEqual(session.process_pid, pid)
            self.assertEqual(session.thread_id, thread_id)
            methods = [item.get("method") for item in record["messages"]]
            self.assertEqual(methods.count("thread/start"), 1)
            self.assertEqual(methods.count("turn/start"), 2)
            self.assertNotIn("thread/resume", methods)
            self.assertNotIn("thread/fork", methods)
            turns = [
                item
                for item in record["messages"]
                if item.get("method") == "turn/start"
            ]
            self.assertEqual(
                turns[1]["params"]["input"][0]["text"],
                "Continue working on the current CodexOS generation.",
            )
            session.close()
            _assert_process_dead(self, pid)

    def test_interrupt_completes_and_same_thread_can_continue(self) -> None:
        scenario = {
            "turns": [
                {"hold_for_interrupt": True},
                {"final_message": "Continued after pause."},
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
            )
            results: list[object] = []
            turn = threading.Thread(
                target=lambda: results.append(session.run_initial_turn())
            )
            turn.start()
            _wait_for(lambda: session.active_turn)
            pid = session.process_pid
            thread_id = session.thread_id

            session.interrupt_turn(1.0)
            turn.join(1.0)
            self.assertFalse(turn.is_alive())
            self.assertEqual(results[0].turn_status, "interrupted")

            continued = session.run_continuation_turn("Mechanical continuation.")
            self.assertEqual(continued.final_message, "Continued after pause.")
            self.assertEqual(session.process_pid, pid)
            self.assertEqual(session.thread_id, thread_id)
            record = fake.record()
            methods = [item.get("method") for item in record["messages"]]
            self.assertEqual(methods.count("turn/interrupt"), 1)
            self.assertEqual(methods.count("thread/start"), 1)
            session.close()

    def test_interrupt_requires_terminal_interrupted_notification(self) -> None:
        scenario = {
            "turns": [
                {
                    "hold_for_interrupt": True,
                    "interrupt_terminal": False,
                }
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
            )
            failures: list[BaseException] = []

            def run_turn() -> None:
                try:
                    session.run_initial_turn()
                except BaseException as error:
                    failures.append(error)

            turn = threading.Thread(target=run_turn)
            turn.start()
            _wait_for(lambda: session.active_turn)
            with self.assertRaisesRegex(
                CodexGenerationWorkerError,
                "did not reach interrupted state",
            ):
                session.interrupt_turn(0.05)
            self.assertTrue(session.active_turn)
            self.assertIs(runtime.state, RuntimeState.RUNNING)
            session.close()
            turn.join(1.0)
            self.assertFalse(turn.is_alive())
            self.assertTrue(failures)


class CodexGenerationWorkerIntegrationTest(unittest.TestCase):
    def test_real_guest_bridge_finishes_and_next_generation_is_fresh(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* CODEX-WORKER-BRIDGE */\n"
        handoff = "Continue from the Codex worker bridge."

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(root / "run", qemu)
            first_scenario = {
                "assert_dead_processes_in": str(root / "fake-reviewer"),
                "tool_calls": [
                    {"tool": "list", "arguments": {}},
                    {
                        "tool": "read",
                        "arguments": {
                            "path": "seed/kernel.c",
                            "offset": 0,
                            "length": len(original_kernel),
                        },
                    },
                    {
                        "tool": "write",
                        "arguments": {
                            "path": "seed/kernel.c",
                            "offset": len(original_kernel),
                            "encoding": "base64",
                            "data": base64.b64encode(mutation).decode("ascii"),
                        },
                    },
                    {
                        "namespace": None,
                        "tool": "review",
                        "arguments": {
                            "focus": "correctness",
                            "request": "Review the live mutation.",
                        },
                    },
                    {"tool": "build", "arguments": {}},
                    {
                        "tool": "finish_generation",
                        "arguments": {"handoff": handoff},
                    },
                    {
                        "tool": "read",
                        "arguments": {
                            "path": "seed/kernel.c",
                            "offset": 0,
                            "length": 1,
                        },
                    },
                ],
                "final_message": "Generation finished through CodexOS.",
            }
            reviewer_scenario = {
                "model": "gpt-5.6-luna",
                "permission_profile": "codexos-reviewer",
                "tool_calls": [
                    {"tool": "list", "arguments": {}},
                    {
                        "tool": "read",
                        "arguments": {
                            "path": "seed/kernel.c",
                            "offset": len(original_kernel),
                            "length": len(mutation),
                        },
                    },
                ],
                "final_message": "Blocking\n- Advisory finding from reviewer.",
            }
            try:
                runtime.start(image)
                generation_zero_pid = runtime.active_pid
                self.assertIsNotNone(generation_zero_pid)
                with _fake_codex(first_scenario, root / "fake-one") as first:
                    with _fake_codex(
                        reviewer_scenario,
                        root / "fake-reviewer",
                    ) as reviewer:
                        first_worker = CodexGenerationWorker(
                            first.executable,
                            first.auth_file,
                            reviewer_codex_executable=reviewer.executable,
                            reviewer_auth_file=reviewer.auth_file,
                        )
                        first_result = first_worker.run_generation(runtime)
                        first_record = first.record()
                        reviewer_record = reviewer.record()

                self.assertIs(
                    first_result.runtime_state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
                self.assertIsNone(runtime.active_pid)
                _assert_process_dead(self, generation_zero_pid)
                self.assertEqual(
                    (repository / "seed" / "kernel.c").read_bytes(),
                    original_kernel,
                )
                archive = runtime.run_directory / "generation-0000"
                self.assertIn(
                    mutation,
                    (archive / "source" / "seed" / "kernel.c").read_bytes(),
                )
                self.assertEqual(
                    (archive / "handoff.txt").read_text(encoding="utf-8"),
                    handoff,
                )
                review_response = first_record["tool_results"][3]["result"]
                self.assertTrue(review_response["success"])
                self.assertEqual(
                    review_response["contentItems"][0]["text"],
                    "Blocking\n- Advisory finding from reviewer.",
                )
                self.assertTrue(
                    first_record["dead_process_checks"][3][0]["dead"]
                )
                reviewed_bytes = json.loads(
                    reviewer_record["tool_results"][1]["result"]["contentItems"][
                        0
                    ]["text"]
                )
                self.assertEqual(reviewed_bytes["output"], mutation.decode())
                _assert_process_dead(self, reviewer_record["pid"])
                finish_response = first_record["tool_results"][-2]["result"]
                self.assertTrue(finish_response["success"])
                after_finish = first_record["tool_results"][-1]["result"]
                self.assertFalse(after_finish["success"])
                self.assertIn(
                    "not running",
                    after_finish["contentItems"][0]["text"],
                )

                runtime.continue_generation()
                self.assertEqual(runtime.generation_number, 1)
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                second_scenario = {
                    "tool_calls": [
                        {
                            "tool": "read",
                            "arguments": {
                                "path": "seed/kernel.c",
                                "offset": len(original_kernel),
                                "length": len(mutation),
                            },
                        }
                    ],
                    "final_message": "Fresh successor session inspected the source.",
                }
                with _fake_codex(second_scenario, root / "fake-two") as second:
                    second_worker = CodexGenerationWorker(
                        second.executable,
                        second.auth_file,
                    )
                    second_result = second_worker.run_generation(runtime)
                    second_record = second.record()

                self.assertIs(second_result.runtime_state, RuntimeState.RUNNING)
                self.assertEqual(
                    second_result.summary,
                    "Codex turn completed; generation is still running.",
                )
                first_messages = first_record["messages"]
                second_messages = second_record["messages"]
                self.assertNotEqual(first_record["pid"], second_record["pid"])
                self.assertNotEqual(
                    first_record["thread_id"],
                    second_record["thread_id"],
                )
                for method in ("thread/resume", "thread/fork"):
                    self.assertNotIn(
                        method,
                        [message.get("method") for message in second_messages],
                    )
                second_prompt = _request(second_messages, "turn/start")[
                    "params"
                ]["input"][0]["text"]
                self.assertIn(handoff, second_prompt)
                self.assertNotIn(
                    "Generation finished through CodexOS.",
                    second_prompt,
                )
                second_tool = json.loads(
                    second_record["tool_results"][0]["result"][
                        "contentItems"
                    ][0]["text"]
                )
                self.assertEqual(second_tool["output"], mutation.decode())
                self.assertIsNotNone(runtime.active_pid)
            finally:
                runtime.stop()
            self.assertIsNone(runtime.active_pid)


class _FakeCodex:
    def __init__(
        self,
        root: Path,
        scenario: dict[str, object],
        cleanup_root: bool = False,
    ) -> None:
        self.root = root
        self._cleanup_root = cleanup_root
        self.root.mkdir(parents=True, exist_ok=True)
        source = Path(__file__).with_name("fake_codex_app_server.py")
        self.executable = self.root / "codex"
        shutil.copyfile(source, self.executable)
        self.executable.chmod(0o755)
        self.auth_file = self.root / "auth.json"
        self.auth_file.write_text("fake login", encoding="utf-8")
        self.scenario_path = self.root / "scenario.json"
        self.scenario_path.write_text(json.dumps(scenario), encoding="utf-8")
        self.record_path = self.root / "record.json"

    def __enter__(self) -> "_FakeCodex":
        return self

    def __exit__(self, *args: object) -> None:
        if self._cleanup_root:
            shutil.rmtree(self.root)

    def record(self) -> dict[str, object]:
        return json.loads(self.record_path.read_text(encoding="utf-8"))

    def records(self) -> list[dict[str, object]]:
        return [
            json.loads(path.read_text(encoding="utf-8"))
            for path in sorted(self.root.glob("record-*.json"))
        ]


def _fake_codex(
    scenario: dict[str, object],
    root: Path | None = None,
) -> _FakeCodex:
    if root is not None:
        return _FakeCodex(root, scenario)
    temporary = tempfile.mkdtemp(prefix="codexos-fake-codex-")
    return _FakeCodex(Path(temporary), scenario, cleanup_root=True)


def _request(
    messages: list[dict[str, object]],
    method: str,
) -> dict[str, object]:
    return next(message for message in messages if message.get("method") == method)


def _runtime_mock() -> Mock:
    runtime = Mock(spec=CodexOSRun)
    runtime.state = RuntimeState.RUNNING
    runtime.generation_number = 0
    runtime.previous_handoff = None
    runtime.current_transition = "initial"
    return runtime


def _wait_for(condition: Callable[[], bool], timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(0.01)
    raise AssertionError("condition was not met before timeout")


def _assert_process_dead(test: unittest.TestCase, pid: int) -> None:
    with test.assertRaises(ProcessLookupError):
        os.kill(pid, 0)


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

import base64
import json
import os
import shutil
import subprocess
import tempfile
import tomllib
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from harness import (
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
            namespace = thread["dynamicTools"]
            self.assertEqual(len(namespace), 1)
            self.assertEqual(namespace[0]["name"], "codexos")
            self.assertEqual(
                [tool["name"] for tool in namespace[0]["tools"]],
                _TOOLS,
            )
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
            try:
                runtime.start(image)
                generation_zero_pid = runtime.active_pid
                self.assertIsNotNone(generation_zero_pid)
                with _fake_codex(first_scenario, root / "fake-one") as first:
                    first_worker = CodexGenerationWorker(
                        first.executable,
                        first.auth_file,
                    )
                    first_result = first_worker.run_generation(runtime)
                    first_record = first.record()

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
        self.environment = patch.dict(
            os.environ,
            {
                "CODEXOS_FAKE_SCENARIO": str(self.scenario_path),
                "CODEXOS_FAKE_RECORD": str(self.record_path),
            },
        )

    def __enter__(self) -> "_FakeCodex":
        self.environment.start()
        return self

    def __exit__(self, *args: object) -> None:
        self.environment.stop()
        if self._cleanup_root:
            shutil.rmtree(self.root)

    def record(self) -> dict[str, object]:
        return json.loads(self.record_path.read_text(encoding="utf-8"))


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

import os
import shutil
import tempfile
import threading
import tomllib
import unittest
import warnings
from pathlib import Path
from unittest.mock import Mock, patch

from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from harness import (
    TEST_HARDWARE_PROFILE,
    CodexGenerationSession,
    CodexGenerationWorker,
    CodexOSRun,
    RuntimeState,
    ToolResult,
)
from harness.observability import ExperimentObservability
from harness.codex_review_worker import DEFAULT_REVIEWER_SERVICE_TIER
from tests.test_codex_generation_worker import (
    _assert_process_dead,
    _build_seed,
    _fake_codex,
    _expected_token_metric_values,
    _metric_values,
    _request,
    _token_usage,
    _wait_for,
)


class CodexReviewerProtocolTests(unittest.TestCase):
    def test_malformed_token_usage_does_not_fail_review(self) -> None:
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"tool": "list", "arguments": {}},
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "token_usage_params": [
                {
                    "tokenUsage": {
                        "last": {},
                        "total": {"inputTokens": 1},
                    }
                }
            ],
            "final_message": "No meaningful issues found.",
        }
        with tempfile.TemporaryDirectory() as temporary:
            with _fake_codex(implementor_scenario) as implementor:
                with _fake_codex(reviewer_scenario) as reviewer:
                    reader = InMemoryMetricReader()
                    observability = ExperimentObservability(
                        temporary,
                        metric_readers=[reader],
                    )
                    runtime = _runtime_mock()
                    runtime.generation_number = 0
                    runtime.observability = observability
                    runtime.invoke_tool.return_value = ToolResult(
                        0, b"seed/kernel.c\n"
                    )
                    worker = CodexGenerationWorker(
                        implementor.executable,
                        implementor.auth_file,
                        reviewer_codex_executable=reviewer.executable,
                        reviewer_auth_file=reviewer.auth_file,
                    )

                    with warnings.catch_warnings():
                        warnings.simplefilter("error")
                        result = worker.run_generation(runtime)

                    tool_results = implementor.record()["tool_results"]
                    self.assertEqual(result.turn_status, "completed")
                    self.assertTrue(tool_results[0]["result"]["success"])
                    self.assertIn(
                        "No meaningful issues found.",
                        tool_results[0]["result"]["contentItems"][0]["text"],
                    )
                    self.assertTrue(tool_results[1]["result"]["success"])
                    runtime.invoke_tool.assert_called_once_with("list", [])
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    runtime.pause.assert_not_called()
                    runtime.stop.assert_not_called()
                    self.assertFalse(observability.healthy)
                    self.assertIn(
                        "reviewer token usage telemetry was ignored",
                        observability.degraded_reason or "",
                    )
                    self.assertFalse(
                        any(
                            name.startswith("codexos_model_")
                            for name in _metric_values(reader)
                        )
                    )
                    observability.close()

    def test_each_reviewer_uses_an_independent_token_baseline(self) -> None:
        usage = _token_usage(80, 25, 30, 12)
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"namespace": None, "tool": "review", "arguments": {}},
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "token_usage": usage,
            "final_message": "No meaningful issues found.",
        }
        with tempfile.TemporaryDirectory() as temporary:
            with _fake_codex(implementor_scenario) as implementor:
                with _fake_codex(reviewer_scenario) as reviewer:
                    reader = InMemoryMetricReader()
                    observability = ExperimentObservability(
                        temporary,
                        metric_readers=[reader],
                    )
                    runtime = _runtime_mock()
                    runtime.generation_number = 0
                    runtime.observability = observability

                    CodexGenerationWorker(
                        implementor.executable,
                        implementor.auth_file,
                        reviewer_codex_executable=reviewer.executable,
                        reviewer_auth_file=reviewer.auth_file,
                    ).run_generation(runtime)

                    expected = {
                        name: value * 2
                        for name, value in _expected_token_metric_values(
                            usage
                        ).items()
                    }
                    self.assertEqual(_metric_values(reader), expected)
                    metrics = reader.get_metrics_data()
                    for resource in metrics.resource_metrics:
                        for scope in resource.scope_metrics:
                            for metric in scope.metrics:
                                if metric.name.startswith("codexos_model_"):
                                    self.assertEqual(
                                        dict(
                                            metric.data.data_points[
                                                0
                                            ].attributes
                                        ),
                                        {
                                            "model": "gpt-5.6-luna",
                                            "role": "reviewer",
                                        },
                                    )
                    self.assertTrue(observability.healthy)
                    observability.close()

    def test_active_review_is_cancelled_before_implementor_interrupt(self) -> None:
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
            with _fake_codex(
                implementor_scenario,
                root / "implementor",
            ) as implementor:
                with _fake_codex(
                    reviewer_scenario,
                    root / "reviewer",
                ) as reviewer:
                    runtime = _runtime_mock()
                    session = CodexGenerationSession(
                        runtime,
                        implementor.executable,
                        implementor.auth_file,
                        reviewer_codex_executable=reviewer.executable,
                        reviewer_auth_file=reviewer.auth_file,
                    )
                    results: list[object] = []
                    turn = threading.Thread(
                        target=lambda: results.append(
                            session.run_initial_turn()
                        )
                    )
                    turn.start()
                    _wait_for(lambda: bool(reviewer.records()))
                    reviewer_pid = reviewer.records()[0]["pid"]

                    session.interrupt_turn(2.0)
                    turn.join(2.0)
                    self.assertFalse(turn.is_alive())
                    self.assertEqual(results[0].turn_status, "interrupted")
                    _assert_process_dead(self, reviewer_pid)
                    _wait_for(
                        lambda: bool(
                            implementor.record().get("tool_results")
                        )
                    )
                    response = implementor.record()["tool_results"][0]["result"]
                    self.assertFalse(response["success"])
                    self.assertIs(runtime.state, RuntimeState.RUNNING)
                    self.assertIsNotNone(session.process_pid)
                    session.close()

    def test_two_reviews_are_fresh_read_only_consultations(self) -> None:
        review_text = "Blocking\n- A concrete advisory finding."
        implementor_scenario = {
            "tool_calls": [
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {
                        "focus": "design",
                        "request": "Review the current direction.",
                    },
                },
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {},
                },
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "server_requests": [
                "item/commandExecution/requestApproval",
                "item/permissions/requestApproval",
            ],
            "tool_calls": [
                {"tool": "list", "arguments": {}},
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "length": 4,
                    },
                },
            ],
            "final_message": review_text,
        }
        with _fake_codex(implementor_scenario) as implementor:
            with _fake_codex(reviewer_scenario) as reviewer:
                runtime = _runtime_mock()
                runtime.invoke_tool.side_effect = [
                    ToolResult(0, b"seed/kernel.c\n"),
                    ToolResult(0, b"code"),
                    ToolResult(0, b"seed/kernel.c\n"),
                    ToolResult(0, b"code"),
                ]
                worker = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                )

                worker.run_generation(
                    runtime,
                    objective="Improve the current bootstrap safely.",
                )

                implementor_record = implementor.record()
                reviewer_records = reviewer.records()

        self.assertEqual(len(reviewer_records), 2)
        self.assertNotEqual(
            reviewer_records[0]["pid"],
            reviewer_records[1]["pid"],
        )
        self.assertNotEqual(
            reviewer_records[0]["thread_id"],
            reviewer_records[1]["thread_id"],
        )
        for record in reviewer_records:
            methods = [message.get("method") for message in record["messages"]]
            self.assertIn("thread/start", methods)
            self.assertNotIn("thread/resume", methods)
            self.assertNotIn("thread/fork", methods)
            thread = _request(record["messages"], "thread/start")["params"]
            self.assertTrue(thread["ephemeral"])
            self.assertEqual(thread["model"], "gpt-5.6-luna")
            self.assertEqual(
                thread["serviceTier"],
                DEFAULT_REVIEWER_SERVICE_TIER,
            )
            self.assertEqual(thread["permissions"], "codexos-reviewer")
            self.assertEqual(len(thread["dynamicTools"]), 1)
            namespace = thread["dynamicTools"][0]
            self.assertEqual(namespace["name"], "codexos")
            self.assertEqual(
                [tool["name"] for tool in namespace["tools"]],
                ["list", "read"],
            )
            turn = _request(record["messages"], "turn/start")["params"]
            self.assertEqual(turn["model"], "gpt-5.6-luna")
            self.assertEqual(turn["effort"], "high")
            self.assertEqual(
                turn["serviceTier"],
                DEFAULT_REVIEWER_SERVICE_TIER,
            )
            config = tomllib.loads(record["config"])
            profile = config["permissions"]["codexos-reviewer"]
            self.assertEqual(
                profile["filesystem"][":workspace_roots"]["."],
                "read",
            )
            self.assertFalse(profile["network"]["enabled"])
            self.assertFalse(config["features"]["multi_agent"])
            self.assertFalse(config["features"]["plugins"])
            self.assertEqual(
                record["server_responses"][0]["result"],
                {"decision": "decline"},
            )
            self.assertFalse(
                record["server_responses"][1]["result"]["permissions"][
                    "network"
                ]["enabled"]
            )
            _assert_process_dead(self, record["pid"])

        first_prompt = _request(
            reviewer_records[0]["messages"],
            "turn/start",
        )["params"]["input"][0]["text"]
        second_prompt = _request(
            reviewer_records[1]["messages"],
            "turn/start",
        )["params"]["input"][0]["text"]
        self.assertIn("Review focus: design", first_prompt)
        self.assertIn("Review the current direction.", first_prompt)
        self.assertIn("Improve the current bootstrap safely.", first_prompt)
        self.assertIn("Review focus: general", second_prompt)
        self.assertIn("No additional review request", second_prompt)
        self.assertNotIn(review_text, second_prompt)
        for response in implementor_record["tool_results"]:
            self.assertTrue(response["result"]["success"])
            self.assertEqual(
                response["result"]["contentItems"][0]["text"],
                review_text,
            )

    def test_reviewer_rejects_mutation_and_mismatched_calls(self) -> None:
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}}
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "tool_calls": [
                {
                    "tool": "list",
                    "arguments": {},
                    "thread_id": "wrong-thread",
                },
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "length": 1,
                    },
                    "turn_id": "wrong-turn",
                },
                {
                    "tool": "write",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "data": "x",
                    },
                },
                {"tool": "list", "arguments": {}, "omit_call_id": True},
                {"tool": "list", "arguments": {}},
            ],
            "final_message": "No meaningful issues found.",
        }
        with _fake_codex(implementor_scenario) as implementor:
            with _fake_codex(reviewer_scenario) as reviewer:
                runtime = _runtime_mock()
                runtime.invoke_tool.return_value = ToolResult(0, b"")
                worker = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                )
                worker.run_generation(runtime)
                reviewer_record = reviewer.record()

        runtime.invoke_tool.assert_called_once_with("list", [])
        results = [response["result"] for response in reviewer_record["tool_results"]]
        self.assertEqual([result["success"] for result in results], [False] * 4 + [True])

    def test_implementor_rejects_mismatched_calls_before_runtime(self) -> None:
        scenario = {
            "tool_calls": [
                {
                    "tool": "list",
                    "arguments": {},
                    "thread_id": "wrong-thread",
                },
                {
                    "tool": "list",
                    "arguments": {},
                    "turn_id": "wrong-turn",
                },
                {"tool": "list", "arguments": {}, "call_id": ""},
                {"tool": "list", "arguments": {}},
            ]
        }
        with _fake_codex(scenario) as implementor:
            runtime = _runtime_mock()
            runtime.invoke_tool.return_value = ToolResult(0, b"")
            CodexGenerationWorker(
                implementor.executable,
                implementor.auth_file,
            ).run_generation(runtime)
            record = implementor.record()

        runtime.invoke_tool.assert_called_once_with("list", [])
        self.assertEqual(
            [entry["result"]["success"] for entry in record["tool_results"]],
            [False, False, False, True],
        )

    def test_bad_review_arguments_do_not_start_reviewer(self) -> None:
        scenario = {
            "tool_calls": [
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {"focus": "style"},
                },
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {"request": "x" * (8 * 1024 + 1)},
                },
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {"extra": True},
                },
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {"request": None},
                },
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {"request": "\ud800"},
                },
            ]
        }
        with _fake_codex(scenario) as implementor:
            with _fake_codex({}) as reviewer:
                runtime = _runtime_mock()
                worker = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                )
                worker.run_generation(runtime)
                record = implementor.record()
                self.assertEqual(reviewer.records(), [])

        self.assertEqual(runtime.invoke_tool.call_count, 0)
        self.assertTrue(
            all(not item["result"]["success"] for item in record["tool_results"])
        )

    def test_reviewer_failure_is_isolated_from_implementor_and_runtime(self) -> None:
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"tool": "list", "arguments": {}},
            ],
            "final_message": "Implementor continued after review failure.",
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "turn_status": "failed",
            "turn_error": {"message": "synthetic reviewer failure"},
        }
        with _fake_codex(implementor_scenario) as implementor:
            with _fake_codex(reviewer_scenario) as reviewer:
                runtime = _runtime_mock()
                runtime.invoke_tool.return_value = ToolResult(0, b"")
                result = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                ).run_generation(runtime)
                implementor_record = implementor.record()
                reviewer_record = reviewer.record()

        self.assertEqual(result.turn_status, "completed")
        self.assertIs(runtime.state, RuntimeState.RUNNING)
        self.assertFalse(implementor_record["tool_results"][0]["result"]["success"])
        self.assertIn(
            "synthetic reviewer failure",
            implementor_record["tool_results"][0]["result"]["contentItems"][0][
                "text"
            ],
        )
        self.assertTrue(implementor_record["tool_results"][1]["result"]["success"])
        runtime.invoke_tool.assert_called_once_with("list", [])
        _assert_process_dead(self, reviewer_record["pid"])

    def test_unavailable_reviewer_fast_tier_is_an_isolated_review_failure(
        self,
    ) -> None:
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"tool": "list", "arguments": {}},
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "service_tiers": [],
        }
        with _fake_codex(implementor_scenario) as implementor:
            with _fake_codex(reviewer_scenario) as reviewer:
                runtime = _runtime_mock()
                runtime.invoke_tool.return_value = ToolResult(0, b"")
                result = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                ).run_generation(runtime)
                response = implementor.record()["tool_results"]
                reviewer_record = reviewer.record()

        self.assertEqual(result.turn_status, "completed")
        self.assertFalse(response[0]["result"]["success"])
        self.assertIn(
            "does not support service tier 'priority'",
            response[0]["result"]["contentItems"][0]["text"],
        )
        self.assertTrue(response[1]["result"]["success"])
        runtime.invoke_tool.assert_called_once_with("list", [])
        self.assertIs(runtime.state, RuntimeState.RUNNING)
        _assert_process_dead(self, reviewer_record["pid"])

    def test_reviewer_setup_oserror_is_isolated_from_implementor(self) -> None:
        scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"tool": "list", "arguments": {}},
            ],
            "final_message": "Implementor continued after setup failure.",
        }
        real_symlink = os.symlink
        reviewer_roots: list[Path] = []

        def symlink(source: object, destination: object, *args: object) -> None:
            path = Path(destination)
            if path.parent.parent.name.startswith("codexos-reviewer-"):
                reviewer_roots.append(path.parent.parent)
                raise OSError("synthetic reviewer filesystem failure")
            real_symlink(source, destination, *args)

        with _fake_codex(scenario) as implementor:
            with _fake_codex({}) as reviewer:
                runtime = _runtime_mock()
                runtime.invoke_tool.return_value = ToolResult(0, b"seed/kernel.c\n")
                worker = CodexGenerationWorker(
                    implementor.executable,
                    implementor.auth_file,
                    reviewer_codex_executable=reviewer.executable,
                    reviewer_auth_file=reviewer.auth_file,
                )
                with patch(
                    "harness.codex_app_server.os.symlink",
                    side_effect=symlink,
                ):
                    result = worker.run_generation(runtime)
                record = implementor.record()
                self.assertEqual(reviewer.records(), [])

        self.assertEqual(result.turn_status, "completed")
        self.assertIs(runtime.state, RuntimeState.RUNNING)
        review = record["tool_results"][0]["result"]
        self.assertFalse(review["success"])
        self.assertIn(
            "synthetic reviewer filesystem failure",
            review["contentItems"][0]["text"],
        )
        self.assertTrue(record["tool_results"][1]["result"]["success"])
        runtime.invoke_tool.assert_called_once_with("list", [])
        self.assertEqual(len(reviewer_roots), 1)
        self.assertFalse(reviewer_roots[0].exists())


class CodexReviewerQemuIntegrationTests(unittest.TestCase):
    def test_failed_review_leaves_the_real_generation_running(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        image = _build_seed(repository)
        implementor_scenario = {
            "tool_calls": [
                {"namespace": None, "tool": "review", "arguments": {}},
                {"tool": "list", "arguments": {}},
            ]
        }
        reviewer_scenario = {
            "model": "gpt-5.6-luna",
            "permission_profile": "codexos-reviewer",
            "turn_status": "failed",
            "turn_error": {"message": "synthetic reviewer failure"},
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            try:
                runtime.start(image)
                pid = runtime.active_pid
                with _fake_codex(
                    implementor_scenario,
                    root / "fake-implementor",
                ) as implementor:
                    with _fake_codex(
                        reviewer_scenario,
                        root / "fake-reviewer",
                    ) as reviewer:
                        CodexGenerationWorker(
                            implementor.executable,
                            implementor.auth_file,
                            reviewer_codex_executable=reviewer.executable,
                            reviewer_auth_file=reviewer.auth_file,
                        ).run_generation(runtime)
                        record = implementor.record()

                self.assertFalse(record["tool_results"][0]["result"]["success"])
                self.assertTrue(record["tool_results"][1]["result"]["success"])
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.active_pid, pid)
                self.assertIsNotNone(pid)
                os.kill(pid, 0)
            finally:
                runtime.stop()
            self.assertIsNone(runtime.active_pid)


@unittest.skipUnless(
    os.environ.get("CODEXOS_REAL_REVIEWER_SMOKE") == "1",
    "set CODEXOS_REAL_REVIEWER_SMOKE=1 for the paid Luna reviewer smoke test",
)
class RealCodexReviewerSmokeTest(unittest.TestCase):
    def test_luna_reviews_the_live_seed_read_only(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        image = _build_seed(repository)
        scenario = {
            "tool_calls": [
                {
                    "namespace": None,
                    "tool": "review",
                    "arguments": {
                        "focus": "general",
                        "request": (
                            "Review the current seed implementation and report "
                            "only meaningful issues."
                        ),
                    },
                }
            ]
        }
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            try:
                runtime.start(image)
                pid = runtime.active_pid
                with _fake_codex(scenario, root / "fake-implementor") as fake:
                    result = CodexGenerationWorker(
                        fake.executable,
                        fake.auth_file,
                        reviewer_codex_executable="codex",
                        reviewer_auth_file=Path.home() / ".codex" / "auth.json",
                    ).run_generation(
                        runtime,
                        objective="Inspect the current seed before further work.",
                    )
                    response = fake.record()["tool_results"][0]["result"]

                self.assertTrue(response["success"], response)
                review = response["contentItems"][0]["text"]
                self.assertTrue(
                    any(
                        marker in review
                        for marker in (
                            "Blocking",
                            "Non-blocking",
                            "Suggestions",
                            "No meaningful issues found",
                        )
                    ),
                    review,
                )
                self.assertIs(result.runtime_state, RuntimeState.RUNNING)
                self.assertEqual(runtime.active_pid, pid)
            finally:
                runtime.stop()
            self.assertIsNone(runtime.active_pid)


def _runtime_mock() -> Mock:
    runtime = Mock(spec=CodexOSRun)
    runtime.state = RuntimeState.RUNNING
    runtime.previous_handoff = None
    runtime.current_transition = "initial"
    runtime.hardware_profile = TEST_HARDWARE_PROFILE
    runtime.feature_requests.return_value = ()
    return runtime


if __name__ == "__main__":
    unittest.main()

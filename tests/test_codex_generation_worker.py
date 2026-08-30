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
import warnings
from collections.abc import Callable
from dataclasses import replace
from pathlib import Path
from unittest.mock import Mock

from opentelemetry.sdk.metrics.export import InMemoryMetricReader

from harness import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
    TEST_HARDWARE_PROFILE,
    CodexGenerationSession,
    CodexGenerationWorker,
    CodexGenerationWorkerError,
    CodexOSRun,
    FeatureRequest,
    PendingGenerationFinish,
    RuntimeState,
    ToolResult,
)
from harness.codex_app_server import (
    CodexAppServerError,
    CumulativeTokenUsage,
    token_usage_delta_from_notification,
)
from harness.codex_generation_worker import (
    AGENT_CONTRACT_VERSION,
    DEFAULT_REASONING_SUMMARY,
    DEFAULT_SERVICE_TIER,
    CodexGenerationSessionMode,
    _implementor_prompt,
)
from harness.observability import ExperimentObservability
from harness.exit_interview_transcript import ExitInterviewArtifactStore

_TOOLS = [
    "list",
    "read",
    "write",
    "truncate",
    "remove",
    "build",
    "finish_generation",
    "request_feature",
    "list_requests",
]


class CodexGenerationWorkerProtocolTests(unittest.TestCase):
    def test_live_activity_captures_renderable_text_and_tool_outcomes(self) -> None:
        source = "void changed(void) {\n  emit(\"\x1b[source\");\n}\n"
        scenario = {
            "visible_activity": [
                {"kind": "agent_delta", "text": "Inspecting source."},
                {
                    "kind": "reasoning_summary_delta",
                    "text": "Checking the current implementation.",
                },
                {
                    "kind": "reasoning_completed",
                    "summary": ["Checked the implementation."],
                    "content": ["opaque reasoning must not be surfaced"],
                },
                {
                    "kind": "reasoning_text_delta",
                    "text": "private reasoning delta",
                },
            ],
            "tool_calls": [
                {
                    "tool": "write",
                    "arguments": {
                        "path": "seed/new.c",
                        "offset": 0,
                        "data": source,
                    },
                },
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/new.c",
                        "offset": 0,
                        "length": len(source),
                    },
                },
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/new.c",
                        "offset": -1,
                        "length": 1,
                    },
                },
            ],
            "final_message": "Activity turn complete.",
        }
        with _fake_codex(scenario) as fake:
            stream = CodexActivityStream()
            runtime = _runtime_mock()
            runtime.invoke_tool.side_effect = [
                ToolResult(0, b""),
                ToolResult(7, source.encode()),
            ]

            result = CodexGenerationWorker(
                fake.executable,
                fake.auth_file,
                activity_stream=stream,
            ).run_generation(runtime)

        self.assertEqual(result.turn_status, "completed")
        events = stream.drain()
        self.assertEqual(
            [event.sequence for event in events],
            list(range(1, len(events) + 1)),
        )
        self.assertTrue(
            all(event.generation == runtime.generation_number for event in events)
        )
        self.assertTrue(
            all(event.role is CodexActivityRole.IMPLEMENTOR for event in events)
        )
        by_kind: dict[CodexActivityKind, list[object]] = {}
        for event in events:
            by_kind.setdefault(event.kind, []).append(event)
        self.assertEqual(
            by_kind[CodexActivityKind.AGENT_TEXT_DELTA][0].data["text"],
            "Inspecting source.",
        )
        self.assertEqual(
            by_kind[CodexActivityKind.AGENT_REASONING_DELTA][0].data["text"],
            "Checking the current implementation.",
        )
        self.assertEqual(
            by_kind[CodexActivityKind.AGENT_REASONING_SUMMARY][0].data["summary"],
            ["Checked the implementation."],
        )
        self.assertFalse(
            any(
                "private reasoning" in repr(event.data)
                or "opaque reasoning" in repr(event.data)
                for event in events
            )
        )
        tool_started = by_kind[CodexActivityKind.TOOL_STARTED]
        self.assertEqual(tool_started[0].data["arguments"]["data"], source)
        failed = by_kind[CodexActivityKind.TOOL_FAILED]
        self.assertEqual(failed[0].data["result"]["status"], 7)
        self.assertEqual(failed[0].data["result"]["output"], source.encode())
        self.assertIn("non-negative", failed[1].data["error"])
        self.assertEqual(
            by_kind[CodexActivityKind.AGENT_MESSAGE][-1].data["text"],
            "Activity turn complete.",
        )
        self.assertIn(CodexActivityKind.SESSION_STOPPED, by_kind)

    def test_broken_activity_observer_does_not_change_turn_behavior(self) -> None:
        class BrokenStream(CodexActivityStream):
            def publish(self, *args, **kwargs):
                raise RuntimeError("observer failed")

        with _fake_codex(
            {"tool_calls": [{"tool": "list", "arguments": {}}]}
        ) as fake:
            runtime = _runtime_mock()
            runtime.invoke_tool.return_value = ToolResult(0, b"seed/kernel.c\n")

            result = CodexGenerationWorker(
                fake.executable,
                fake.auth_file,
                activity_stream=BrokenStream(),
            ).run_generation(runtime)

        self.assertEqual(result.turn_status, "completed")
        runtime.invoke_tool.assert_called_once_with("list", [])
        self.assertIs(runtime.state, RuntimeState.RUNNING)

    def test_live_activity_payload_is_not_persisted_to_events_jsonl(self) -> None:
        marker = "LIVE-ACTIVITY-SOURCE-MARKER"
        request_marker = "LIVE-ACTIVITY-REQUEST-MARKER"
        scenario = {
            "tool_calls": [
                {
                    "tool": "write",
                    "arguments": {
                        "path": "seed/private.c",
                        "offset": 0,
                        "data": marker,
                    },
                },
                {"tool": "list_requests", "arguments": {}},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary, _fake_codex(
            scenario
        ) as fake:
            stream = CodexActivityStream()
            observability = ExperimentObservability(temporary)
            runtime = _runtime_mock()
            runtime.observability = observability
            runtime.invoke_tool.return_value = ToolResult(0, b"")
            runtime.feature_requests.return_value = (
                FeatureRequest(
                    3,
                    0,
                    request_marker,
                    "Request text stays out of operational telemetry.",
                    "denied",
                ),
            )

            CodexGenerationWorker(
                fake.executable,
                fake.auth_file,
                activity_stream=stream,
            ).run_generation(runtime)
            observability.close()

            activity = stream.drain()
            self.assertTrue(any(marker in repr(event.data) for event in activity))
            self.assertTrue(
                any(request_marker in repr(event.data) for event in activity)
            )
            events = (Path(temporary) / "events.jsonl").read_text(encoding="utf-8")
            self.assertNotIn(marker, events)
            self.assertNotIn(request_marker, events)

    def test_initial_prompt_states_the_behavioral_contract(self) -> None:
        runtime = _runtime_mock()
        runtime.previous_handoff = "Exact predecessor handoff."
        runtime.current_transition = "rollback"
        runtime.feature_requests.return_value = (
            FeatureRequest(
                1, 0, "Approved capability", "Exact description.", "approved"
            ),
            FeatureRequest(
                2, 0, "Pending capability", "Not approved.", "pending"
            ),
            FeatureRequest(
                3, 0, "Denied capability", "Not approved.", "denied"
            ),
        )

        prompt = _implementor_prompt(runtime, "Trusted operator objective.")

        for required in (
            "genuinely general-purpose operating system",
            "first major interactive userland milestone",
            "not the definition or final purpose",
            "supplied Doom executable and data must remain immutable",
            "ordinary user workload",
            "generic userland mechanisms",
            "no Doom-specific behavior or special scheduling treatment",
            "preemptive execution",
            "does not voluntarily yield, block, or enter the kernel",
            "must not prevent another runnable user workload from making progress",
            "Doom must run concurrently with an unrelated user workload",
            "programs unknown to you during development",
            "future or supplied workloads specify required observable outcomes "
            "only",
            "neither grant nor imply any supporting trusted-environment "
            "capability",
            "Do not assume an absent trusted-environment capability will appear "
            "later",
            "development continues after Doom is playable",
            "not a prescribed kernel architecture or implementation sequence",
            "neither Unix, POSIX, System V",
            "improve the guest-side development environment and tooling",
            "conversation history does not survive a generation boundary",
            "Trusted tools available to you",
            "Inspect the persistent mutable CodexOS guest source",
            "do not expose the trusted host repository or host filesystem",
            "Modify the persistent mutable CodexOS guest source",
            "Compile and link the exact current persistent mutable CodexOS "
            "guest source",
            "boots under the current trusted hardware profile",
            "reaches the canonical READY state",
            "speaks the canonical development protocol",
            "Permanently end the current generation",
            "matches the latest successful validated build",
            "handoff for the fresh successor session",
            "distinguish implemented end-to-end capabilities and explicitly "
            "provisioned trusted capabilities from unresolved dependencies or "
            "assumptions",
            "do not describe a future path as available unless all required steps "
            "are implemented or explicitly provisioned",
            "advisory request to the human operator",
            "capability of the trusted external environment",
            "rather than human implementation of CodexOS kernel or userland "
            "functionality",
            "does not itself provision or change anything",
            "may remain pending or be denied",
            "does not require depending on it, waiting for it, or stopping "
            "guest-side work",
            "a local workaround does not by itself make that trusted-environment "
            "request inappropriate",
            "- list_requests:",
            "authoritative run-level external feature requests",
            "Pending requests are recorded advisory requests, not provisioned or "
            "promised",
            "approved requests have already been provisioned",
            "denied requests are unavailable under that request",
            "read-only tool does not modify requests",
            "not as a substitute for implementing functionality that belongs "
            "inside CodexOS",
            "fresh independent reviewer",
            "through restricted read-only tools",
            "reviewer is advisory and cannot modify CodexOS",
            "response and transcript do not automatically become memory",
            "Provisioning one external capability does not imply or grant any "
            "other trusted-environment capability",
            "Trusted provided-asset host services",
            "immutable opaque trusted inputs available to guest code",
            "list_provided_assets takes no arguments",
            "<id><TAB><filename><TAB><size-decimal><TAB><sha256-hex><NEWLINE>",
            "read_provided_asset takes exactly three arguments",
            "Length is at most 1 MiB",
            "Invalid requests fail rather than being truncated",
            "supplies no guest filesystem, installation location, archive "
            "extraction, compiler, runtime, executable compatibility",
            "Asset IDs and filenames do not prescribe how their bytes should be "
            "used",
            "Exact predecessor handoff.",
            "Later lineage was abandoned.",
            "Trusted operator objective.",
            "#1: Approved capability\nExact description.",
        ):
            self.assertIn(required, prompt)
        self.assertNotIn("Pending capability", prompt)
        self.assertNotIn("Denied capability", prompt)
        for steering in (
            "57 bytes",
            "64 KiB",
            "additional source capacity",
            "asset transport",
            "Limine module",
            "keyboard input",
            "exit interview",
        ):
            self.assertNotIn(steering, prompt)
        self.assertNotIn("every workaround", prompt.lower())

    def test_agent_contract_version_is_six(self) -> None:
        self.assertEqual(AGENT_CONTRACT_VERSION, 6)

    def test_initial_prompt_hardware_is_derived_from_profile(self) -> None:
        profiles = (
            TEST_HARDWARE_PROFILE,
            replace(
                TEST_HARDWARE_PROFILE,
                profile="prompt-alt-v1",
                accelerator="kvm",
                cpu_model="host",
                vcpus=TEST_HARDWARE_PROFILE.vcpus + 2,
                memory_mib=TEST_HARDWARE_PROFILE.memory_mib + 384,
            ),
        )
        prompts: list[str] = []
        for profile in profiles:
            runtime = _runtime_mock()
            runtime.hardware_profile = profile
            prompt = _implementor_prompt(runtime, None)
            prompts.append(prompt)
            writable = ", ".join(profile.writable_block_devices) or "none"
            for expected in (
                f"Profile: {profile.profile}",
                f"Machine: {profile.machine}",
                f"Accelerator: {profile.accelerator}",
                f"CPU: {profile.cpu_model}",
                f"vCPUs: {profile.vcpus}",
                f"RAM: {profile.memory_mib} MiB",
                f"Graphics: {profile.graphics}",
                f"Network interfaces: {profile.network}",
                f"Writable block devices: {writable}",
            ):
                self.assertIn(expected, prompt)
        self.assertNotEqual(prompts[0], prompts[1])

    def test_malformed_token_usage_degrades_observability_not_session(self) -> None:
        accepted = _token_usage(100, 30, 40, 10)
        missing = dict(accepted)
        del missing["reasoningOutputTokens"]
        wrong_type = dict(accepted, inputTokens="invalid")
        negative = dict(accepted, cachedInputTokens=-1)
        decreased = _token_usage(99, 30, 40, 10)
        excessive_cache = _token_usage(100, 101, 40, 10)
        excessive_reasoning = _token_usage(100, 30, 40, 41)
        decreased_uncached = _token_usage(110, 45, 40, 10)
        final = _token_usage(180, 60, 70, 20)
        scenario = {
            "turns": [
                {
                    "token_usage_params": [
                        _token_usage_params(accepted),
                        _token_usage_params(missing),
                        _token_usage_params(wrong_type),
                        _token_usage_params(negative),
                        _token_usage_params(decreased),
                        _token_usage_params(excessive_cache),
                        _token_usage_params(excessive_reasoning),
                        _token_usage_params(decreased_uncached),
                        _token_usage_params(final),
                    ]
                },
                {"tool_calls": [{"tool": "list", "arguments": {}}]},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary, _fake_codex(
            scenario
        ) as fake:
            reader = InMemoryMetricReader()
            observability = ExperimentObservability(
                temporary,
                metric_readers=[reader],
            )
            runtime = _runtime_mock()
            runtime.observability = observability
            runtime.invoke_tool.return_value = ToolResult(0, b"seed/kernel.c\n")
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
            )

            with warnings.catch_warnings():
                warnings.simplefilter("error")
                first = session.run_initial_turn()
                second = session.run_continuation_turn()

            self.assertEqual(first.turn_status, "completed")
            self.assertEqual(second.turn_status, "completed")
            self.assertTrue(session.healthy)
            self.assertIs(runtime.state, RuntimeState.RUNNING)
            runtime.invoke_tool.assert_called_once_with("list", [])
            runtime.pause.assert_not_called()
            runtime.stop.assert_not_called()
            self.assertFalse(observability.healthy)
            self.assertIn(
                "implementor token usage telemetry was ignored",
                observability.degraded_reason or "",
            )
            self.assertEqual(
                _metric_values(reader),
                _expected_token_metric_values(final),
            )
            session.close()
            observability.close()

    def test_authoritative_breakdown_ignores_unused_total_tokens(self) -> None:
        usage = _token_usage(123, 10, 45, 20)
        usage["totalTokens"] = "irrelevant upstream aggregate"
        with tempfile.TemporaryDirectory() as temporary, _fake_codex(
            {"token_usage": usage}
        ) as fake:
            reader = InMemoryMetricReader()
            observability = ExperimentObservability(
                temporary,
                metric_readers=[reader],
            )
            runtime = _runtime_mock()
            runtime.observability = observability

            CodexGenerationWorker(fake.executable, fake.auth_file).run_generation(
                runtime
            )

            values = {}
            metrics = reader.get_metrics_data()
            for resource in metrics.resource_metrics:
                for scope in resource.scope_metrics:
                    for metric in scope.metrics:
                        if metric.name.startswith("codexos_model_"):
                            values[metric.name] = metric.data.data_points[0].value
                            self.assertEqual(
                                dict(metric.data.data_points[0].attributes),
                                {
                                    "model": "gpt-5.6-sol",
                                    "role": "implementor",
                                },
                            )
            self.assertEqual(values, _expected_token_metric_values(usage))
            observability.close()

    def test_cumulative_token_usage_adds_only_positive_deltas(self) -> None:
        first = _token_usage(100, 10, 20, 5)
        second = _token_usage(150, 40, 35, 8)
        final = _token_usage(210, 75, 55, 13)
        scenario = {
            "turns": [
                {
                    "token_usage_params": [
                        _token_usage_params(first),
                        _token_usage_params(first),
                    ]
                },
                {
                    "token_usage_params": [
                        _token_usage_params(second),
                        _token_usage_params(final),
                        _token_usage_params(final),
                    ]
                },
            ]
        }
        with tempfile.TemporaryDirectory() as temporary, _fake_codex(
            scenario
        ) as fake:
            reader = InMemoryMetricReader()
            observability = ExperimentObservability(
                temporary,
                metric_readers=[reader],
            )
            runtime = _runtime_mock()
            runtime.observability = observability
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
            )
            with warnings.catch_warnings():
                warnings.simplefilter("error")
                first_result = session.run_initial_turn()
                second_result = session.run_continuation_turn()

            self.assertEqual(first_result.turn_status, "completed")
            self.assertEqual(second_result.turn_status, "completed")
            self.assertTrue(session.healthy)
            self.assertEqual(
                _metric_values(reader),
                _expected_token_metric_values(final),
            )
            self.assertTrue(observability.healthy)
            session.close()
            observability.close()

    def test_rejects_invalid_cumulative_snapshots(self) -> None:
        previous_usage = _token_usage(100, 30, 40, 10)
        previous, _ = token_usage_delta_from_notification(
            _complete_token_notification(previous_usage),
            "thread-1",
            "turn-1",
            CumulativeTokenUsage(),
        )
        missing = dict(previous_usage)
        del missing["cachedInputTokens"]
        cases = {
            "missing field": missing,
            "wrong type": dict(previous_usage, outputTokens="40"),
            "negative": dict(previous_usage, reasoningOutputTokens=-1),
            "decreasing cumulative": _token_usage(99, 30, 40, 10),
            "decreasing cached": _token_usage(100, 29, 40, 10),
            "decreasing output": _token_usage(100, 30, 39, 10),
            "decreasing reasoning": _token_usage(100, 30, 40, 9),
            "cached exceeds input": _token_usage(100, 101, 40, 10),
            "reasoning exceeds output": _token_usage(100, 30, 40, 41),
            "uncached decreases": _token_usage(110, 45, 40, 10),
        }

        for description, usage in cases.items():
            with self.subTest(description=description):
                with self.assertRaises(CodexAppServerError):
                    token_usage_delta_from_notification(
                        _complete_token_notification(usage),
                        "thread-1",
                        "turn-1",
                        previous,
                    )

    def test_cumulative_snapshot_returns_exact_named_deltas(self) -> None:
        first_usage = _token_usage(100, 35, 40, 12)
        second_usage = _token_usage(165, 70, 58, 19)
        first, _ = token_usage_delta_from_notification(
            _complete_token_notification(first_usage),
            "thread-1",
            "turn-1",
            CumulativeTokenUsage(),
        )

        _, delta = token_usage_delta_from_notification(
            _complete_token_notification(second_usage),
            "thread-1",
            "turn-1",
            first,
        )

        self.assertEqual(
            delta.input_tokens,
            second_usage["inputTokens"] - first_usage["inputTokens"],
        )
        self.assertEqual(
            delta.cached_input_tokens,
            second_usage["cachedInputTokens"]
            - first_usage["cachedInputTokens"],
        )
        self.assertEqual(
            delta.uncached_input_tokens,
            (
                second_usage["inputTokens"]
                - second_usage["cachedInputTokens"]
            )
            - (
                first_usage["inputTokens"]
                - first_usage["cachedInputTokens"]
            ),
        )
        self.assertEqual(
            delta.output_tokens,
            second_usage["outputTokens"] - first_usage["outputTokens"],
        )
        self.assertEqual(
            delta.reasoning_output_tokens,
            second_usage["reasoningOutputTokens"]
            - first_usage["reasoningOutputTokens"],
        )

    def test_feature_request_bridge_and_approved_prompt_are_concrete(self) -> None:
        approved_title = "Approved title\nraw\r\x1b[2J"
        approved_description = "Approved description\nraw\x07\u009b"
        scenario = {
            "tool_calls": [
                {
                    "tool": "request_feature",
                    "arguments": {
                        "title": "External capability λ",
                        "description": "Please provision this outside CodexOS.",
                    },
                },
                {
                    "tool": "request_feature",
                    "arguments": {
                        "title": "x" * 257,
                        "description": "must not reach the guest",
                    },
                },
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            runtime.feature_requests.return_value = (
                FeatureRequest(
                    3, 0, approved_title, approved_description, "approved"
                ),
                FeatureRequest(
                    4, 0, "Pending title", "Pending description", "pending"
                ),
                FeatureRequest(
                    5, 0, "Denied title", "Denied description", "denied"
                ),
            )
            runtime.invoke_tool.return_value = ToolResult(0, b"17")

            CodexGenerationWorker(fake.executable, fake.auth_file).run_generation(
                runtime
            )
            record = fake.record()

            runtime.invoke_tool.assert_called_once_with(
                "request_feature",
                [
                    "External capability λ".encode(),
                    b"Please provision this outside CodexOS.",
                ],
            )
            first = record["tool_results"][0]["result"]
            self.assertTrue(first["success"])
            self.assertEqual(
                json.loads(first["contentItems"][0]["text"])["output"],
                "17",
            )
            self.assertFalse(record["tool_results"][1]["result"]["success"])

            turn = _request(record["messages"], "turn/start")
            prompt = turn["params"]["input"][0]["text"]
            self.assertIn("Approved external feature requests for this run:", prompt)
            self.assertIn(
                f"#3: {approved_title}\n{approved_description}",
                prompt,
            )
            self.assertNotIn("Pending title", prompt)
            self.assertNotIn("Denied title", prompt)
            self.assertIn("- request_feature:", prompt)
            self.assertIn("- list_requests:", prompt)
            self.assertIn(
                "capability of the trusted external environment",
                prompt,
            )

    def test_list_requests_is_read_only_exact_and_idempotent(self) -> None:
        requests = (
            FeatureRequest(9, 4, "Denied λ", "Exact denied text.\n次", "denied"),
            FeatureRequest(2, 1, "Pending", "Exact pending text.", "pending"),
            FeatureRequest(5, 3, "Approved", "Exact approved text.", "approved"),
        )
        scenario = {
            "tool_calls": [
                {"tool": "list_requests", "arguments": {}},
                {"tool": "list_requests", "arguments": {"unexpected": True}},
                {"tool": "list_requests", "arguments": {}},
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            runtime.feature_requests.return_value = requests

            CodexGenerationWorker(fake.executable, fake.auth_file).run_generation(
                runtime
            )
            results = fake.record()["tool_results"]

        first = _dynamic_result_output(results[0])
        third = _dynamic_result_output(results[2])
        self.assertEqual(first, third)
        self.assertEqual(
            first,
            {
                "requests": [
                    {
                        "description": "Exact pending text.",
                        "generation": 1,
                        "id": 2,
                        "status": "pending",
                        "title": "Pending",
                    },
                    {
                        "description": "Exact approved text.",
                        "generation": 3,
                        "id": 5,
                        "status": "approved",
                        "title": "Approved",
                    },
                    {
                        "description": "Exact denied text.\n次",
                        "generation": 4,
                        "id": 9,
                        "status": "denied",
                        "title": "Denied λ",
                    },
                ]
            },
        )
        self.assertFalse(results[1]["result"]["success"])
        self.assertIn(
            "unexpected argument: unexpected",
            results[1]["result"]["contentItems"][0]["text"],
        )
        runtime.invoke_tool.assert_not_called()

    def test_request_then_list_requests_observes_new_pending_record(self) -> None:
        requests: list[FeatureRequest] = []
        scenario = {
            "tool_calls": [
                {"tool": "list_requests", "arguments": {}},
                {
                    "tool": "request_feature",
                    "arguments": {
                        "title": "External capability λ",
                        "description": "Exact advisory request.",
                    },
                },
                {"tool": "list_requests", "arguments": {}},
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            runtime.feature_requests.side_effect = lambda: tuple(requests)

            def invoke(tool: str, arguments: list[bytes]) -> ToolResult:
                self.assertEqual(tool, "request_feature")
                self.assertEqual(
                    arguments,
                    ["External capability λ".encode(), b"Exact advisory request."],
                )
                requests.append(
                    FeatureRequest(
                        1,
                        runtime.generation_number,
                        "External capability λ",
                        "Exact advisory request.",
                        "pending",
                    )
                )
                return ToolResult(0, b"1")

            runtime.invoke_tool.side_effect = invoke
            CodexGenerationWorker(fake.executable, fake.auth_file).run_generation(
                runtime
            )
            results = fake.record()["tool_results"]

        self.assertEqual(_dynamic_result_output(results[0]), {"requests": []})
        self.assertEqual(
            _dynamic_result_output(results[2]),
            {
                "requests": [
                    {
                        "description": "Exact advisory request.",
                        "generation": runtime.generation_number,
                        "id": 1,
                        "status": "pending",
                        "title": "External capability λ",
                    }
                ]
            },
        )
        runtime.invoke_tool.assert_called_once()

    def test_fresh_session_lists_current_authoritative_decisions(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(
                Path(temporary) / "run",
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            first = runtime._feature_request_store.create(
                10,
                "Provisioned capability",
                "Exact provisioned scope.",
            )
            second = runtime._feature_request_store.create(
                10,
                "Denied capability",
                "Exact denied scope.",
            )
            runtime._state = RuntimeState.RUNNING
            runtime._generation_number = 10
            runtime._current_transition = "successor"
            runtime._previous_handoff = "Project rationale without request records."

            with _fake_codex(
                {"tool_calls": [{"tool": "list_requests", "arguments": {}}]}
            ) as first_fake:
                CodexGenerationWorker(
                    first_fake.executable,
                    first_fake.auth_file,
                ).run_generation(runtime)
                pending = _dynamic_result_output(
                    first_fake.record()["tool_results"][0]
                )

            runtime._state = RuntimeState.AWAITING_NEXT_GENERATION
            runtime.approve_feature_request(first.id)
            runtime.deny_feature_request(second.id)
            runtime._state = RuntimeState.RUNNING
            runtime._generation_number = 11

            with _fake_codex(
                {"tool_calls": [{"tool": "list_requests", "arguments": {}}]}
            ) as successor_fake:
                CodexGenerationWorker(
                    successor_fake.executable,
                    successor_fake.auth_file,
                ).run_generation(runtime)
                record = successor_fake.record()
                current = _dynamic_result_output(record["tool_results"][0])

            self.assertEqual(
                [request["status"] for request in pending["requests"]],
                ["pending", "pending"],
            )
            self.assertEqual(
                [request["status"] for request in current["requests"]],
                ["approved", "denied"],
            )
            prompt = _request(record["messages"], "turn/start")["params"][
                "input"
            ][0]["text"]
            self.assertIn("Project rationale without request records.", prompt)
            self.assertNotIn("Denied capability", prompt)

    def test_list_requests_is_not_invoked_automatically(self) -> None:
        with _fake_codex({"final_message": "No tools requested."}) as fake:
            runtime = _runtime_mock()
            CodexGenerationWorker(fake.executable, fake.auth_file).run_generation(
                runtime
            )
            record = fake.record()

        self.assertEqual(record["tool_results"], [])
        runtime.invoke_tool.assert_not_called()
        self.assertEqual(runtime.feature_requests.call_count, 1)
        prompt = _request(record["messages"], "turn/start")["params"][
            "input"
        ][0]["text"]
        self.assertIn("Approved external feature requests for this run: none.", prompt)
        for steering in (
            "check list_requests before every decision",
            "create a request when the list is empty",
            "an empty request list is a problem",
        ):
            self.assertNotIn(steering, prompt.lower())

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
            runtime.hardware_profile = TEST_HARDWARE_PROFILE
            runtime.feature_requests.return_value = ()
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
            self.assertEqual(thread["serviceTier"], DEFAULT_SERVICE_TIER)
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
            descriptions = {
                tool["name"]: tool["description"]
                for tool in dynamic_tools[0]["tools"]
            }
            self.assertIn(
                "persistent mutable CodexOS guest source",
                descriptions["read"],
            )
            self.assertIn(
                "boots under the current trusted hardware profile",
                descriptions["build"],
            )
            self.assertIn(
                "Permanently end the current generation",
                descriptions["finish_generation"],
            )
            self.assertIn(
                "matches the latest successful validated build",
                descriptions["finish_generation"],
            )
            self.assertIn(
                "distinguish implemented end-to-end capabilities",
                descriptions["finish_generation"],
            )
            self.assertIn(
                "unresolved dependencies or assumptions",
                descriptions["finish_generation"],
            )
            self.assertIn(
                "capability of the trusted external environment",
                descriptions["request_feature"],
            )
            self.assertIn(
                "rather than human implementation of CodexOS kernel or "
                "userland functionality",
                descriptions["request_feature"],
            )
            self.assertIn(
                "does not itself provision or change anything",
                descriptions["request_feature"],
            )
            self.assertIn(
                "may remain pending or be denied",
                descriptions["request_feature"],
            )
            self.assertIn(
                "does not require depending on it, waiting for it, or stopping "
                "guest-side work",
                descriptions["request_feature"],
            )
            self.assertIn(
                "a local workaround does not by itself make that trusted-"
                "environment request inappropriate",
                descriptions["request_feature"],
            )
            list_requests = next(
                tool
                for tool in dynamic_tools[0]["tools"]
                if tool["name"] == "list_requests"
            )
            self.assertEqual(
                list_requests["inputSchema"],
                {
                    "type": "object",
                    "properties": {},
                    "additionalProperties": False,
                },
            )
            for expected in (
                "authoritative run-level external feature requests",
                "pending, approved, or denied status",
                "not provisioned or promised",
                "no ETA or approval probability",
                "approved requests have already been provisioned",
                "exact provisioned scope",
                "denied requests are unavailable",
                "read-only tool does not modify requests",
            ):
                self.assertIn(expected, list_requests["description"])
            for steering in (
                "before every decision",
                "when the list is empty",
                "create a request",
                "source capacity",
                "Doom assets",
            ):
                self.assertNotIn(steering, list_requests["description"])
            review_description = dynamic_tools[1]["description"]
            for expected in (
                "fresh independent reviewer",
                "restricted read-only tools",
                "advisory",
                "cannot modify CodexOS",
            ):
                self.assertIn(expected, review_description)
            config = tomllib.loads(record["config"])
            profile = config["permissions"]["codexos-implementor"]
            self.assertEqual(profile["filesystem"][":root"], "deny")
            self.assertEqual(
                profile["filesystem"][":workspace_roots"]["."],
                "write",
            )
            self.assertFalse(profile["network"]["enabled"])
            interview_profile = config["permissions"]["codexos-interview"]
            self.assertEqual(interview_profile["filesystem"][":root"], "deny")
            self.assertEqual(
                interview_profile["filesystem"][":workspace_roots"]["."],
                "read",
            )
            self.assertFalse(interview_profile["network"]["enabled"])
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
            self.assertEqual(turn["summary"], DEFAULT_REASONING_SUMMARY)
            self.assertEqual(turn["model"], "gpt-5.6-sol")
            self.assertEqual(turn["serviceTier"], DEFAULT_SERVICE_TIER)

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
            runtime.hardware_profile = TEST_HARDWARE_PROFILE
            runtime.feature_requests.return_value = ()
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
            runtime.hardware_profile = TEST_HARDWARE_PROFILE
            runtime.feature_requests.return_value = ()
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError, "model is unavailable"
            ):
                worker.run_generation(runtime, model="missing-model")
            record = fake.record()
            methods = [message.get("method") for message in record.get("messages", [])]
            self.assertNotIn("turn/start", methods)
            _assert_process_dead(self, record["pid"])

    def test_rejects_unavailable_or_malformed_service_tier_catalog(self) -> None:
        cases = (
            (
                {"service_tiers": []},
                "does not support service tier 'priority'",
            ),
            (
                {"service_tiers": "not a catalog list"},
                "malformed service-tier capabilities",
            ),
        )
        for scenario, message in cases:
            with self.subTest(message=message):
                with _fake_codex(scenario) as fake:
                    runtime = _runtime_mock()
                    worker = CodexGenerationWorker(
                        fake.executable,
                        fake.auth_file,
                    )

                    with self.assertRaisesRegex(
                        CodexGenerationWorkerError,
                        message,
                    ):
                        worker.run_generation(runtime)
                    _assert_process_dead(self, fake.record()["pid"])

    def test_rejects_unknown_reasoning_summary_without_fallback(self) -> None:
        with _fake_codex({}) as fake:
            runtime = _runtime_mock()
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError,
                "unsupported reasoning summary setting",
            ):
                worker.run_generation(
                    runtime,
                    reasoning_summary="automatic",
                )
            record = fake.record()
            methods = [
                message.get("method")
                for message in record.get("messages", [])
            ]
            self.assertNotIn("thread/start", methods)
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
            runtime.hardware_profile = TEST_HARDWARE_PROFILE
            runtime.feature_requests.return_value = ()
            worker = CodexGenerationWorker(fake.executable, fake.auth_file)

            with self.assertRaisesRegex(
                CodexGenerationWorkerError, "synthetic model failure"
            ):
                worker.run_generation(runtime)
            _assert_process_dead(self, fake.record()["pid"])


class CodexGenerationSessionProtocolTests(unittest.TestCase):
    def test_exit_interview_cannot_change_frozen_artifacts_or_successor_context(
        self,
    ) -> None:
        marker = "EXIT-INTERVIEW-SECRET-91f-immutability"
        scenario = {
            "turns": [
                {
                    "tool_calls": [
                        {
                            "tool": "finish_generation",
                            "arguments": {"handoff": "Immutable handoff."},
                        }
                    ]
                },
                {"final_message": "Historical explanation only."},
            ]
        }
        with tempfile.TemporaryDirectory() as temporary, _fake_codex(
            scenario
        ) as fake:
            root = Path(temporary)
            archive = root / "generation-0000"
            successor = archive / "successor"
            successor.mkdir(parents=True)
            files = {
                archive / "source.snapshot": b"frozen source snapshot",
                archive / "handoff.txt": b"Immutable handoff.",
                archive / "metadata.json": b'{"outcome":"completed"}',
                successor / "kernel.elf": b"frozen kernel",
                successor / "codexos.iso": b"frozen iso",
            }
            for path, content in files.items():
                path.write_bytes(content)
            repository = root / "repository"
            repository.mkdir()
            subprocess.run(["git", "init", "-q"], cwd=repository, check=True)
            subprocess.run(
                ["git", "config", "user.name", "CodexOS Test"],
                cwd=repository,
                check=True,
            )
            subprocess.run(
                ["git", "config", "user.email", "codexos@example.invalid"],
                cwd=repository,
                check=True,
            )
            (repository / "seed.txt").write_text("generation source\n")
            subprocess.run(["git", "add", "seed.txt"], cwd=repository, check=True)
            subprocess.run(
                ["git", "commit", "-qm", "generation"],
                cwd=repository,
                check=True,
            )
            subprocess.run(
                [
                    "git",
                    "tag",
                    "-a",
                    "experiment-test/generation-0000",
                    "-m",
                    "Frozen generation tag",
                ],
                cwd=repository,
                check=True,
            )
            tag_object = subprocess.check_output(
                ["git", "rev-parse", "experiment-test/generation-0000"],
                cwd=repository,
                text=True,
            )
            commit = subprocess.check_output(
                ["git", "rev-list", "-n", "1", "experiment-test/generation-0000"],
                cwd=repository,
                text=True,
            )
            before_files = {path: path.read_bytes() for path in files}

            observability = ExperimentObservability(root / "run")
            runtime = _runtime_mock()
            runtime.observability = observability
            runtime.run_directory = root / "experiment-test"
            runtime.run_directory.mkdir()
            runtime.previous_handoff = None
            approved = FeatureRequest(
                1, 0, "Approved external capability", "Frozen decision.", "approved"
            )
            denied = FeatureRequest(
                2,
                0,
                "INTERVIEW-REQUEST-LEDGER-MARKER",
                "Denied request stays outside interview artifacts.",
                "denied",
            )
            runtime.feature_requests.return_value = (approved, denied)

            pending = PendingGenerationFinish(
                "Immutable handoff.",
                files[archive / "source.snapshot"],
                successor / "kernel.elf",
                successor / "codexos.iso",
            )

            def finish(tool: str, arguments: list[bytes]) -> ToolResult:
                self.assertEqual(tool, "finish_generation")
                runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                runtime.pending_generation_finish = pending
                runtime.previous_handoff = pending.handoff_message
                return ToolResult(0, b"")

            runtime.invoke_tool.side_effect = finish
            session = CodexGenerationSession(runtime, fake.executable, fake.auth_file)
            session.run_initial_turn()
            session.retain_for_exit_interview()
            session.begin_exit_interview()
            session.run_exit_interview_turn(marker)
            transcript = session.exit_interview_transcript()
            self.assertIsNotNone(transcript)
            assert transcript is not None
            artifact = ExitInterviewArtifactStore(
                repository,
                runtime.run_directory,
            ).persist(transcript, "completed")
            self.assertIsNotNone(artifact)
            assert artifact is not None
            session.end_exit_interview()
            observability.close()

            self.assertIs(runtime.pending_generation_finish, pending)
            self.assertEqual(
                {path: path.read_bytes() for path in files}, before_files
            )
            self.assertEqual(runtime.feature_requests(), (approved, denied))
            self.assertEqual(
                subprocess.check_output(
                    ["git", "rev-parse", "experiment-test/generation-0000"],
                    cwd=repository,
                    text=True,
                ),
                tag_object,
            )
            self.assertEqual(
                subprocess.check_output(
                    ["git", "rev-list", "-n", "1", "experiment-test/generation-0000"],
                    cwd=repository,
                    text=True,
                ),
                commit,
            )
            successor_prompt = _implementor_prompt(runtime, None)
            self.assertNotIn(marker, successor_prompt)
            artifact_text = artifact.path.read_text(encoding="utf-8")
            self.assertIn(marker, artifact_text)
            self.assertNotIn("INTERVIEW-REQUEST-LEDGER-MARKER", artifact_text)
            self.assertNotIn(marker, "".join(path.read_text(errors="ignore") for path in files))
            self.assertNotIn(
                marker,
                (root / "run" / "events.jsonl").read_text(encoding="utf-8"),
            )
            events = [
                json.loads(line)
                for line in (root / "run" / "events.jsonl")
                .read_text(encoding="utf-8")
                .splitlines()
            ]
            event_names = [event["event"] for event in events]
            self.assertIn("exit_interview_started", event_names)
            self.assertIn("exit_interview_turn_started", event_names)
            self.assertIn("exit_interview_turn_completed", event_names)
            self.assertIn("exit_interview_ended", event_names)
            interview_started = next(
                event
                for event in events
                if event["event"] == "exit_interview_turn_started"
            )
            self.assertEqual(interview_started["data"]["model"], "gpt-5.6-sol")
            self.assertEqual(interview_started["data"]["reasoning_effort"], "high")
            self.assertEqual(interview_started["data"]["reasoning_summary"], "auto")
            self.assertEqual(interview_started["data"]["service_tier"], "priority")

    def test_exit_interview_reuses_thread_with_read_only_turn_and_denies_tools(
        self,
    ) -> None:
        marker = "EXIT-INTERVIEW-SECRET-91f"
        request_marker = "REQUEST-LEDGER-MUST-NOT-ENTER-INTERVIEW"
        denied_tools = [
            "list",
            "read",
            "write",
            "truncate",
            "remove",
            "build",
            "finish_generation",
            "request_feature",
            "list_requests",
            "list_provided_assets",
            "read_provided_asset",
            "review",
        ]
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
                    "tool_calls": [
                        {
                            "namespace": None if tool == "review" else "codexos",
                            "tool": tool,
                            "arguments": {},
                        }
                        for tool in denied_tools
                    ],
                    "visible_activity": [
                        {
                            "kind": "reasoning_completed",
                            "summary": [
                                "First explicit summary.",
                                "Then another.",
                            ],
                            "content": ["PRIVATE-REASONING-MUST-NOT-PERSIST"],
                        },
                        {
                            "kind": "reasoning_text_delta",
                            "text": "PRIVATE-RAW-DELTA-MUST-NOT-PERSIST",
                        },
                    ],
                    "final_message": "Retrospective answer.",
                },
                {
                    "visible_activity": [
                        {
                            "kind": "reasoning_completed",
                            "summary": ["Second-turn summary."],
                        }
                    ],
                    "final_message": "Second retrospective answer.",
                },
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            runtime.pending_generation_finish = None
            runtime.feature_requests.return_value = (
                FeatureRequest(
                    8,
                    0,
                    request_marker,
                    "Authoritative denied request text.",
                    "denied",
                ),
            )

            def invoke(tool: str, arguments: list[bytes]) -> ToolResult:
                self.assertEqual(tool, "finish_generation")
                runtime.state = RuntimeState.AWAITING_NEXT_GENERATION
                runtime.pending_generation_finish = object()
                return ToolResult(0, b"")

            runtime.invoke_tool.side_effect = invoke
            activity = CodexActivityStream()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
                activity_stream=activity,
            )

            session.run_initial_turn()
            request_reads_before_interview = runtime.feature_requests.call_count
            original_thread = session.thread_id
            original_pid = session.process_pid
            session.retain_for_exit_interview()
            self.assertIs(
                session.mode, CodexGenerationSessionMode.RETAINED_AT_GATE
            )
            with self.assertRaisesRegex(RuntimeError, "unavailable after finish"):
                session.run_continuation_turn()

            session.begin_exit_interview()
            first = session.run_exit_interview_turn(marker)
            second = session.run_exit_interview_turn("Why this design?")

            self.assertEqual(first.final_message, "Retrospective answer.")
            self.assertEqual(second.final_message, "Second retrospective answer.")
            transcript = session.exit_interview_transcript()
            self.assertIsNotNone(transcript)
            assert transcript is not None
            self.assertEqual(
                [turn.question for turn in transcript.turns],
                [marker, "Why this design?"],
            )
            self.assertEqual(
                transcript.turns[0].reasoning_summaries,
                ("First explicit summary.", "Then another."),
            )
            self.assertEqual(
                transcript.turns[1].reasoning_summaries,
                ("Second-turn summary.",),
            )
            self.assertEqual(
                [turn.response for turn in transcript.turns],
                ["Retrospective answer.", "Second retrospective answer."],
            )
            self.assertNotIn("PRIVATE", repr(transcript))
            self.assertNotIn(request_marker, repr(transcript))
            self.assertEqual(session.thread_id, original_thread)
            self.assertEqual(session.process_pid, original_pid)
            runtime.invoke_tool.assert_called_once()
            self.assertEqual(
                runtime.feature_requests.call_count,
                request_reads_before_interview,
            )
            record = fake.record()
            turns = [
                message["params"]
                for message in record["messages"]
                if message.get("method") == "turn/start"
            ]
            self.assertEqual(len(turns), 3)
            for turn in turns[1:]:
                self.assertEqual(turn["threadId"], original_thread)
                self.assertEqual(turn["model"], "gpt-5.6-sol")
                self.assertEqual(turn["effort"], "high")
                self.assertEqual(turn["summary"], "auto")
                self.assertEqual(turn["serviceTier"], "priority")
                self.assertEqual(turn["permissions"], "codexos-interview")
                self.assertEqual(turn["runtimeWorkspaceRoots"], [])
            prompt = turns[1]["input"][0]["text"]
            self.assertIn("read-only exit interview", prompt)
            self.assertIn("Operator question:\n" + marker, prompt)
            self.assertNotIn("dynamicTools", turns[1])
            self.assertTrue(
                all(not response["result"]["success"] for response in record["server_responses"][-len(denied_tools):])
            )
            events = activity.drain()
            questions = [
                event
                for event in events
                if event.kind is CodexActivityKind.EXIT_INTERVIEW_QUESTION
            ]
            self.assertEqual([event.data["text"] for event in questions], [marker, "Why this design?"])

            session.end_exit_interview()
            self.assertIs(session.mode, CodexGenerationSessionMode.CLOSED)
            _assert_process_dead(self, original_pid)

    def test_multiple_turns_reuse_one_process_and_thread(self) -> None:
        scenario = {
            "turns": [
                {"final_message": "First turn complete."},
                {"final_message": "Second turn complete."},
            ]
        }
        with _fake_codex(scenario) as fake:
            runtime = _runtime_mock()
            activity = CodexActivityStream()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
                activity_stream=activity,
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
            self.assertTrue(
                all(
                    turn["params"]["serviceTier"] == DEFAULT_SERVICE_TIER
                    for turn in turns
                )
            )
            self.assertTrue(
                all(
                    turn["params"]["summary"] == DEFAULT_REASONING_SUMMARY
                    for turn in turns
                )
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
            activity = CodexActivityStream()
            session = CodexGenerationSession(
                runtime,
                fake.executable,
                fake.auth_file,
                activity_stream=activity,
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
            turns = [
                item for item in record["messages"]
                if item.get("method") == "turn/start"
            ]
            self.assertTrue(
                all(
                    item["params"]["serviceTier"] == DEFAULT_SERVICE_TIER
                    for item in turns
                )
            )
            self.assertTrue(
                all(
                    item["params"]["summary"] == DEFAULT_REASONING_SUMMARY
                    for item in turns
                )
            )
            self.assertTrue(
                any(
                    event.kind is CodexActivityKind.TURN_INTERRUPTED
                    for event in activity.drain()
                )
            )
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

    def test_timed_out_interrupt_request_does_not_remain_pending(self) -> None:
        scenario = {
            "turns": [
                {
                    "hold_for_interrupt": True,
                    "interrupt_response": False,
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
            turn = threading.Thread(target=session.run_initial_turn)
            turn.start()
            _wait_for(lambda: session.active_turn)

            with self.assertRaisesRegex(CodexAppServerError, "timed out"):
                session.interrupt_turn(0.05)

            server = session._server
            self.assertIsNotNone(server)
            assert server is not None
            self.assertEqual(server._pending, {})
            session.close()
            turn.join(1.0)
            self.assertFalse(turn.is_alive())


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
            runtime = CodexOSRun(
                root / "run",
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
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


def _dynamic_result_output(result: dict[str, object]) -> dict[str, object]:
    bridge = json.loads(result["result"]["contentItems"][0]["text"])
    return json.loads(bridge["output"])


def _runtime_mock() -> Mock:
    runtime = Mock(spec=CodexOSRun)
    runtime.state = RuntimeState.RUNNING
    runtime.generation_number = 0
    runtime.run_directory = Path("/tmp/codexos-test-run")
    runtime.previous_handoff = None
    runtime.current_transition = "initial"
    runtime.hardware_profile = TEST_HARDWARE_PROFILE
    runtime.feature_requests.return_value = ()
    return runtime


def _token_usage(
    input_tokens: int,
    cached_input_tokens: int,
    output_tokens: int,
    reasoning_output_tokens: int,
) -> dict[str, object]:
    return {
        "cacheWriteInputTokens": 0,
        "cachedInputTokens": cached_input_tokens,
        "inputTokens": input_tokens,
        "outputTokens": output_tokens,
        "reasoningOutputTokens": reasoning_output_tokens,
        "totalTokens": input_tokens + output_tokens,
    }


def _token_usage_params(usage: dict[str, object]) -> dict[str, object]:
    return {"tokenUsage": {"last": usage, "total": usage}}


def _complete_token_notification(
    usage: dict[str, object],
) -> dict[str, object]:
    return {
        "threadId": "thread-1",
        "turnId": "turn-1",
        **_token_usage_params(usage),
    }


def _expected_token_metric_values(
    usage: dict[str, object],
) -> dict[str, int]:
    input_tokens = int(usage["inputTokens"])
    cached_input_tokens = int(usage["cachedInputTokens"])
    return {
        "codexos_model_input_tokens_total": input_tokens,
        "codexos_model_cached_input_tokens_total": cached_input_tokens,
        "codexos_model_uncached_input_tokens_total": (
            input_tokens - cached_input_tokens
        ),
        "codexos_model_output_tokens_total": int(usage["outputTokens"]),
        "codexos_model_reasoning_output_tokens_total": int(
            usage["reasoningOutputTokens"]
        ),
    }


def _metric_values(reader: InMemoryMetricReader) -> dict[str, int]:
    values: dict[str, int] = {}
    metrics = reader.get_metrics_data()
    for resource in metrics.resource_metrics:
        for scope in resource.scope_metrics:
            for metric in scope.metrics:
                if metric.name.startswith("codexos_model_"):
                    values[metric.name] = metric.data.data_points[0].value
    return values


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

from __future__ import annotations

import base64
import json
import shutil
import threading
import time
import unittest
import warnings
from datetime import UTC, datetime
from pathlib import Path
from tempfile import TemporaryDirectory

from opentelemetry.sdk.metrics.export import (
    InMemoryMetricReader,
    MetricExporter,
    MetricExportResult,
    PeriodicExportingMetricReader,
)

from harness.observability import (
    ExperimentObservability,
    ExperimentObservabilityError,
)
from harness.codex_generation_worker import (
    AGENT_CONTRACT_VERSION,
    DEFAULT_REASONING_SUMMARY,
    DEFAULT_SERVICE_TIER,
)
from harness.codex_review_worker import (
    DEFAULT_REVIEWER_REASONING_SUMMARY,
    DEFAULT_REVIEWER_SERVICE_TIER,
)
from harness import (
    TEST_HARDWARE_PROFILE,
    CodexGenerationWorker,
    CodexOSRun,
    GenerationGitRecorder,
    RuntimeState,
)
from harness.operator_console import OperatorConsole
from tests.test_codex_generation_worker import _build_seed, _fake_codex
from tests.test_generation_git import _create_repository


class ExperimentObservabilityTests(unittest.TestCase):
    def test_jsonl_is_serialized_and_reopens_at_next_sequence(self) -> None:
        with TemporaryDirectory() as temporary:
            observability = ExperimentObservability(temporary)

            def write_events(worker: int) -> None:
                for value in range(20):
                    observability.record(
                        "tool_started",
                        worker,
                        {"tool": "read", "value": value},
                    )

            threads = [
                threading.Thread(target=write_events, args=(worker,))
                for worker in range(4)
            ]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
            observability.close()

            reopened = ExperimentObservability(temporary)
            reopened.record("run_stopped", None, {})
            reopened.close()

            lines = (Path(temporary) / "events.jsonl").read_text(
                encoding="utf-8"
            ).splitlines()
            events = [json.loads(line) for line in lines]
            self.assertEqual(
                [event["sequence"] for event in events],
                list(range(1, 82)),
            )
            self.assertTrue(all(event["schema_version"] == 1 for event in events))
            self.assertTrue(
                all(
                    datetime.fromisoformat(
                        event["timestamp"].removesuffix("Z") + "+00:00"
                    ).tzinfo
                    == UTC
                    for event in events
                )
            )

    def test_malformed_existing_log_is_not_rewritten(self) -> None:
        with TemporaryDirectory() as temporary:
            path = Path(temporary) / "events.jsonl"
            original = b'{"not":"an event"}\n'
            path.write_bytes(original)
            with self.assertRaisesRegex(
                ExperimentObservabilityError, "invalid envelope"
            ):
                ExperimentObservability(temporary)
            self.assertEqual(path.read_bytes(), original)

    def test_metrics_use_only_bounded_attributes(self) -> None:
        with TemporaryDirectory() as temporary:
            reader = InMemoryMetricReader()
            observability = ExperimentObservability(
                temporary,
                metric_readers=[reader],
            )
            observability.record(
                "tool_completed",
                47,
                {
                    "tool": "read",
                    "status": 0,
                    "duration_seconds": 0.25,
                    "input_bytes": 18,
                    "output_bytes": 9,
                    "path": "seed/kernel.c",
                    "request_id": 12345,
                },
            )
            observability.record(
                "build_completed",
                47,
                {
                    "status": 1,
                    "duration_seconds": 1.5,
                    "diagnostics_bytes": 200,
                },
            )
            observability.record(
                "codex_session_started",
                47,
                {
                    "model": "gpt-5.6-sol",
                    "reasoning_effort": "high",
                    "reasoning_summary": DEFAULT_REASONING_SUMMARY,
                    "service_tier": DEFAULT_SERVICE_TIER,
                    "service_tier_name": "Fast",
                    "agent_contract_version": AGENT_CONTRACT_VERSION,
                },
            )
            observability.record(
                "codex_turn_completed",
                47,
                {
                    "model": "gpt-5.6-sol",
                    "reasoning_effort": "high",
                    "reasoning_summary": DEFAULT_REASONING_SUMMARY,
                    "service_tier": DEFAULT_SERVICE_TIER,
                    "service_tier_name": "Fast",
                    "agent_contract_version": AGENT_CONTRACT_VERSION,
                    "turn_number": 1,
                    "duration_seconds": 2.0,
                    "result": "completed",
                },
            )
            observability.record(
                "review_completed",
                47,
                {
                    "model": "gpt-5.6-luna",
                    "reasoning_effort": "high",
                    "reasoning_summary": DEFAULT_REVIEWER_REASONING_SUMMARY,
                    "service_tier": DEFAULT_REVIEWER_SERVICE_TIER,
                    "service_tier_name": "Fast",
                    "focus": "security",
                    "duration_seconds": 0.5,
                },
            )
            observability.record(
                "generation_completed",
                47,
                {
                    "outcome": "completed",
                    "transition": "rollback",
                    "duration_seconds": 3.0,
                },
            )
            observability.record(
                "operator_pause", 47, {"result": "success"}
            )
            observability.record(
                "feature_requested",
                47,
                {"request_id": 12345, "request_generation": 47},
            )
            observability.record_model_tokens(
                model="gpt-5.6-sol",
                role="implementor",
                input_tokens=100,
                cached_input_tokens=40,
                uncached_input_tokens=60,
                output_tokens=25,
                reasoning_output_tokens=10,
            )

            points = _metric_points(reader)
            names = {name for name, _ in points}
            self.assertIn("codexos_tool_calls_total", names)
            self.assertIn("codexos_builds_total", names)
            self.assertIn("codexos_codex_turns_total", names)
            self.assertIn("codexos_reviews_total", names)
            self.assertIn("codexos_generations_total", names)
            self.assertIn("codexos_operator_actions_total", names)
            self.assertIn("codexos_feature_requests_total", names)
            self.assertIn("codexos_model_input_tokens_total", names)
            self.assertIn(
                "codexos_model_cached_input_tokens_total", names
            )
            self.assertIn(
                "codexos_model_uncached_input_tokens_total", names
            )
            self.assertIn("codexos_model_output_tokens_total", names)
            self.assertIn(
                "codexos_model_reasoning_output_tokens_total", names
            )
            for _, attributes in points:
                self.assertNotIn("path", attributes)
                self.assertNotIn("generation", attributes)
                self.assertNotIn("request_id", attributes)
                self.assertNotIn("call_id", attributes)
            metrics = reader.get_metrics_data()
            for resource in metrics.resource_metrics:
                for scope in resource.scope_metrics:
                    for metric in scope.metrics:
                        if metric.name.startswith("codexos_model_"):
                            self.assertEqual(metric.unit, "{token}")
                            self.assertEqual(
                                dict(metric.data.data_points[0].attributes),
                                {
                                    "model": "gpt-5.6-sol",
                                    "role": "implementor",
                                },
                            )
            observability.close()

    def test_event_record_contains_no_excluded_bodies(self) -> None:
        with TemporaryDirectory() as temporary:
            observability = ExperimentObservability(temporary)
            observability.record(
                "tool_completed",
                0,
                {
                    "tool": "write",
                    "status": 0,
                    "duration_seconds": 0.1,
                    "input_bytes": 17,
                    "output_bytes": 0,
                    "path": "seed/kernel.c",
                },
            )
            observability.record(
                "build_completed",
                0,
                {"status": 0, "duration_seconds": 1.0, "diagnostics_bytes": 0},
            )
            observability.close()
            content = (Path(temporary) / "events.jsonl").read_text(
                encoding="utf-8"
            )
            for excluded in (
                "SOURCE-CONTENT-SECRET",
                "HANDOFF-SECRET",
                "REVIEW-SECRET",
                "TRANSCRIPT-SECRET",
                "COMPILER-DIAGNOSTIC-SECRET",
            ):
                self.assertNotIn(excluded, content)

    def test_metric_export_failure_does_not_affect_local_events(self) -> None:
        with TemporaryDirectory() as temporary:
            exporter = _FailingMetricExporter()
            reader = PeriodicExportingMetricReader(
                exporter,
                export_interval_millis=10,
                export_timeout_millis=10,
            )
            observability = ExperimentObservability(
                temporary,
                metric_readers=[reader],
            )
            observability.record(
                "operator_pause", 0, {"result": "success"}
            )
            self.assertTrue(exporter.called.wait(1.0))
            started = time.monotonic()
            observability.close()
            self.assertLess(time.monotonic() - started, 1.0)
            event = json.loads(
                (Path(temporary) / "events.jsonl").read_text(encoding="utf-8")
            )
            self.assertEqual(event["event"], "operator_pause")
            self.assertEqual(event["data"]["result"], "success")

    def test_local_recording_failure_is_reported_as_degraded(self) -> None:
        with TemporaryDirectory() as temporary:
            observability = ExperimentObservability(temporary)
            actual_output = observability._output
            observability._output = _FailingEventOutput()
            with warnings.catch_warnings(record=True) as caught:
                warnings.simplefilter("always")
                observability.record(
                    "tool_completed",
                    0,
                    {
                        "tool": "read",
                        "status": 0,
                        "duration_seconds": 0.1,
                    },
                )
            self.assertFalse(observability.healthy)
            self.assertIn(
                "local event recording failed",
                observability.degraded_reason or "",
            )
            self.assertTrue(
                any(
                    "observability degraded" in str(item.message)
                    for item in caught
                )
            )
            observability._output = actual_output
            observability.close()


class ExperimentObservabilityQemuIntegrationTest(unittest.TestCase):
    def test_real_generation_records_complete_observable_lifecycle(self) -> None:
        source_repository = Path(__file__).resolve().parents[1]
        image = _build_seed(source_repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (source_repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* OBSERVABILITY-INTEGRATION */\n"
        implementor_scenario = {
            "tool_calls": [
                {
                    "tool": "read",
                    "arguments": {
                        "path": "seed/kernel.c",
                        "offset": 0,
                        "length": 32,
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
                    "arguments": {"focus": "correctness"},
                },
                {"tool": "build", "arguments": {}},
                {
                    "tool": "request_feature",
                    "arguments": {
                        "title": "Observability test capability",
                        "description": "Synthetic integration request.",
                    },
                },
                {
                    "tool": "finish_generation",
                    "arguments": {"handoff": "OBSERVABILITY-HANDOFF-SECRET"},
                },
            ],
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
            "final_message": "REVIEW-RESPONSE-SECRET",
        }

        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            run_directory = root / "run"
            repository, _ = _create_repository(
                root / "repository",
                source_seed=source_repository / "seed",
            )
            observability = ExperimentObservability(run_directory)
            runtime = CodexOSRun(
                run_directory,
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
                observability=observability,
            )
            with _fake_codex(
                implementor_scenario, root / "implementor"
            ) as implementor:
                with _fake_codex(
                    reviewer_scenario, root / "reviewer"
                ) as reviewer:
                    try:
                        runtime.start(image)
                        CodexGenerationWorker(
                            implementor.executable,
                            implementor.auth_file,
                            reviewer_codex_executable=reviewer.executable,
                            reviewer_auth_file=reviewer.auth_file,
                        ).run_generation(runtime)
                        self.assertIs(
                            runtime.state,
                            RuntimeState.AWAITING_NEXT_GENERATION,
                        )
                        self.assertIsNone(runtime.active_pid)
                        request = runtime.feature_requests()[0]
                        runtime.approve_feature_request(request.id)
                        recorder = GenerationGitRecorder(
                            repository,
                            run_directory,
                            "test-base",
                        )
                        console = OperatorConsole(
                            runtime,
                            git_recorder=recorder,
                        )
                        self.assertTrue(console._reconcile_git())
                    finally:
                        runtime.stop()
                        observability.close()

            events = [
                json.loads(line)
                for line in (run_directory / "events.jsonl")
                .read_text(encoding="utf-8")
                .splitlines()
            ]
            names = [event["event"] for event in events]
            for expected in (
                "generation_started",
                "codex_session_started",
                "codex_turn_started",
                "tool_app_server_queued",
                "tool_guest_invocation_started",
                "serial_host_service_request_received",
                "serial_host_service_response_prepared",
                "serial_protocol_write",
                "serial_tool_response_received",
                "tool_guest_invocation_completed",
                "review_started",
                "review_completed",
                "build_completed",
                "feature_requested",
                "generation_completed",
                "feature_approved",
                "git_generation_recorded",
                "codex_turn_completed",
                "codex_session_stopped",
                "run_stopped",
            ):
                self.assertIn(expected, names)
            completed_tools = [
                event["data"]["tool"]
                for event in events
                if event["event"] == "tool_completed"
            ]
            for tool in (
                "read",
                "write",
                "build",
                "request_feature",
                "finish_generation",
            ):
                self.assertIn(tool, completed_tools)
            serial_events = [
                event
                for event in events
                if event["event"].startswith("serial_")
            ]
            self.assertTrue(serial_events)
            self.assertFalse(
                any("payload" in event["data"] for event in serial_events)
            )
            self.assertLess(
                names.index("generation_started"),
                names.index("generation_completed"),
            )
            self.assertLess(
                names.index("feature_requested"),
                names.index("feature_approved"),
            )
            generation_started = next(
                event for event in events
                if event["event"] == "generation_started"
            )
            self.assertEqual(
                generation_started["data"]["hardware_profile"],
                TEST_HARDWARE_PROFILE.profile,
            )
            self.assertEqual(
                generation_started["data"]["vcpus"],
                TEST_HARDWARE_PROFILE.vcpus,
            )
            self.assertEqual(
                generation_started["data"]["memory_mib"],
                TEST_HARDWARE_PROFILE.memory_mib,
            )
            implementor_started = next(
                event
                for event in events
                if event["event"] == "codex_session_started"
            )
            self.assertEqual(
                implementor_started["data"]["service_tier"],
                DEFAULT_SERVICE_TIER,
            )
            self.assertEqual(
                implementor_started["data"]["service_tier_name"],
                "Fast",
            )
            self.assertEqual(
                implementor_started["data"]["reasoning_summary"],
                DEFAULT_REASONING_SUMMARY,
            )
            self.assertEqual(
                implementor_started["data"]["agent_contract_version"],
                AGENT_CONTRACT_VERSION,
            )
            review_started = next(
                event for event in events if event["event"] == "review_started"
            )
            self.assertEqual(
                review_started["data"]["service_tier"],
                DEFAULT_REVIEWER_SERVICE_TIER,
            )
            self.assertEqual(
                review_started["data"]["service_tier_name"],
                "Fast",
            )
            self.assertEqual(
                review_started["data"]["reasoning_summary"],
                DEFAULT_REVIEWER_REASONING_SUMMARY,
            )
            serialized = json.dumps(events, ensure_ascii=False)
            self.assertNotIn("OBSERVABILITY-HANDOFF-SECRET", serialized)
            self.assertNotIn("REVIEW-RESPONSE-SECRET", serialized)
            self.assertNotIn(mutation.decode("ascii").strip(), serialized)


def _metric_points(
    reader: InMemoryMetricReader,
) -> list[tuple[str, dict[str, object]]]:
    data = reader.get_metrics_data()
    points: list[tuple[str, dict[str, object]]] = []
    for resource in data.resource_metrics:
        for scope in resource.scope_metrics:
            for metric in scope.metrics:
                for point in metric.data.data_points:
                    points.append((metric.name, dict(point.attributes)))
    return points


class _FailingMetricExporter(MetricExporter):
    def __init__(self) -> None:
        super().__init__()
        self.called = threading.Event()

    def export(self, metrics_data, timeout_millis=10_000, **kwargs):
        self.called.set()
        return MetricExportResult.FAILURE

    def force_flush(self, timeout_millis=10_000) -> bool:
        return True

    def shutdown(self, timeout_millis=30_000, **kwargs) -> None:
        return None


class _FailingEventOutput:
    def write(self, value: str) -> int:
        raise OSError("synthetic local write failure")


if __name__ == "__main__":
    unittest.main()

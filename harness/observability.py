"""Durable run events and concrete CodexOS experiment metrics."""

from __future__ import annotations

import json
import os
import threading
import warnings
from collections.abc import Mapping, Sequence
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from opentelemetry.exporter.otlp.proto.http.metric_exporter import (
    OTLPMetricExporter,
)
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import (
    MetricReader,
    PeriodicExportingMetricReader,
)
from opentelemetry.sdk.resources import Resource

_SCHEMA_VERSION = 1
_DURABLE_EVENTS = {
    "generation_completed",
    "generation_aborted",
    "feature_approved",
    "feature_denied",
}
_RUNTIME_STATES = (
    "stopped",
    "running",
    "paused",
    "awaiting_next_generation",
)
_SHUTDOWN_TIMEOUT_MILLIS = 2_000


class ExperimentObservabilityError(RuntimeError):
    """The durable local experiment event record is invalid."""


class ExperimentObservability:
    """Own one run's JSONL activity record and OpenTelemetry metrics."""

    def __init__(
        self,
        run_directory: str | Path,
        *,
        otlp_endpoint: str | None = None,
        metric_readers: Sequence[MetricReader] = (),
    ) -> None:
        self._run_directory = Path(run_directory).resolve()
        self._run_directory.mkdir(parents=True, exist_ok=True)
        self._path = self._run_directory / "events.jsonl"
        self._lock = threading.Lock()
        self._health_lock = threading.Lock()
        self._sequence = self._read_last_sequence()
        self._degraded_reason: str | None = None
        self._warning_emitted = False
        self._closed = False

        readers = list(metric_readers)
        if otlp_endpoint is not None:
            endpoint = otlp_endpoint.rstrip("/") + "/v1/metrics"
            exporter = OTLPMetricExporter(endpoint=endpoint, timeout=2.0)
            readers.append(
                PeriodicExportingMetricReader(
                    exporter,
                    export_interval_millis=5_000,
                    export_timeout_millis=2_000,
                )
            )
        resource = Resource.create(
            {
                "service.name": "codexos-harness",
                "codexos.run": self._run_directory.name,
            }
        )
        self._meter_provider = MeterProvider(
            metric_readers=readers,
            resource=resource,
            shutdown_on_exit=False,
        )
        meter = self._meter_provider.get_meter("codexos-harness")

        self._generation_current = meter.create_gauge(
            "codexos_generation_current"
        )
        self._generation_state = meter.create_gauge(
            "codexos_generation_state"
        )
        self._tool_calls = meter.create_counter("codexos_tool_calls_total")
        self._tool_duration = meter.create_histogram(
            "codexos_tool_duration_seconds",
            unit="s",
        )
        self._tool_input_bytes = meter.create_counter(
            "codexos_tool_input_bytes_total",
            unit="By",
        )
        self._tool_output_bytes = meter.create_counter(
            "codexos_tool_output_bytes_total",
            unit="By",
        )
        self._builds = meter.create_counter("codexos_builds_total")
        self._build_duration = meter.create_histogram(
            "codexos_build_duration_seconds",
            unit="s",
        )
        self._codex_turns = meter.create_counter(
            "codexos_codex_turns_total"
        )
        self._codex_turn_duration = meter.create_histogram(
            "codexos_codex_turn_duration_seconds",
            unit="s",
        )
        self._codex_session_starts = meter.create_counter(
            "codexos_codex_session_starts_total"
        )
        self._reviews = meter.create_counter("codexos_reviews_total")
        self._review_duration = meter.create_histogram(
            "codexos_review_duration_seconds",
            unit="s",
        )
        self._generations = meter.create_counter(
            "codexos_generations_total"
        )
        self._generation_duration = meter.create_histogram(
            "codexos_generation_duration_seconds",
            unit="s",
        )
        self._operator_actions = meter.create_counter(
            "codexos_operator_actions_total"
        )
        self._feature_requests = meter.create_counter(
            "codexos_feature_requests_total"
        )
        self._feature_requests_pending = meter.create_gauge(
            "codexos_feature_requests_pending"
        )
        self._model_input_tokens = meter.create_counter(
            "codexos_model_input_tokens_total",
            unit="{token}",
        )
        self._model_output_tokens = meter.create_counter(
            "codexos_model_output_tokens_total",
            unit="{token}",
        )
        self.set_runtime_state(None, "stopped")

        try:
            self._output = self._path.open("a", encoding="utf-8", newline="")
        except OSError as error:
            raise ExperimentObservabilityError(
                f"could not open event log {self._path}: {error}"
            ) from error

    @property
    def path(self) -> Path:
        return self._path

    @property
    def healthy(self) -> bool:
        with self._health_lock:
            return self._degraded_reason is None

    @property
    def degraded_reason(self) -> str | None:
        with self._health_lock:
            return self._degraded_reason

    def record(
        self,
        event: str,
        generation: int | None,
        data: Mapping[str, object] | None = None,
    ) -> None:
        """Append one trusted event without changing experiment behavior."""
        payload = dict(data or {})
        with self._lock:
            if self._closed:
                return
            sequence = self._sequence + 1
            envelope = {
                "schema_version": _SCHEMA_VERSION,
                "sequence": sequence,
                "timestamp": datetime.now(UTC).isoformat(timespec="microseconds")
                .replace("+00:00", "Z"),
                "event": event,
                "generation": generation,
                "data": payload,
            }
            try:
                line = json.dumps(
                    envelope,
                    ensure_ascii=False,
                    separators=(",", ":"),
                    sort_keys=True,
                )
                self._output.write(line + "\n")
                self._output.flush()
                if event in _DURABLE_EVENTS:
                    os.fsync(self._output.fileno())
                self._sequence = sequence
            except (OSError, TypeError, UnicodeError, ValueError) as error:
                self._degrade(f"local event recording failed: {error}")
        self._record_metrics(event, payload)

    def set_runtime_state(
        self,
        generation: int | None,
        state: str,
    ) -> None:
        try:
            if generation is not None:
                self._generation_current.set(generation)
            for known_state in _RUNTIME_STATES:
                self._generation_state.set(
                    1 if known_state == state else 0,
                    {"state": known_state},
                )
        except (RuntimeError, TypeError, ValueError) as error:
            self._degrade(f"runtime-state metric recording failed: {error}")

    def set_feature_requests_pending(self, count: int) -> None:
        try:
            self._feature_requests_pending.set(count)
        except (RuntimeError, TypeError, ValueError) as error:
            self._degrade(f"feature-request metric recording failed: {error}")

    def record_model_tokens(
        self,
        *,
        model: str,
        role: str,
        input_tokens: int,
        output_tokens: int,
    ) -> None:
        try:
            attributes = {"model": model, "role": role}
            self._model_input_tokens.add(input_tokens, attributes)
            self._model_output_tokens.add(output_tokens, attributes)
        except (RuntimeError, TypeError, ValueError) as error:
            self._degrade(f"token metric recording failed: {error}")

    def degrade(self, reason: str) -> None:
        """Report a telemetry problem without changing experiment behavior."""
        self._degrade(reason)

    def close(self) -> None:
        with self._lock:
            if self._closed:
                return
            self._closed = True
            try:
                self._output.flush()
            except OSError as error:
                self._degrade(f"local event flush failed: {error}")
            try:
                self._output.close()
            except OSError as error:
                self._degrade(f"local event close failed: {error}")
        try:
            self._meter_provider.force_flush(_SHUTDOWN_TIMEOUT_MILLIS)
        except Exception as error:  # OpenTelemetry exporters must not affect the run.
            warnings.warn(f"OpenTelemetry metric flush failed: {error}")
        try:
            self._meter_provider.shutdown(_SHUTDOWN_TIMEOUT_MILLIS)
        except Exception as error:  # OpenTelemetry exporters must not affect the run.
            warnings.warn(f"OpenTelemetry metric shutdown failed: {error}")

    def _record_metrics(self, event: str, data: Mapping[str, object]) -> None:
        try:
            if event == "tool_completed":
                tool = str(data["tool"])
                attributes = {
                    "tool": tool,
                    "status": _tool_status(data.get("status")),
                }
                self._tool_calls.add(1, attributes)
                self._tool_duration.record(
                    float(data["duration_seconds"]), {"tool": tool}
                )
                self._tool_input_bytes.add(
                    int(data.get("input_bytes", 0)), {"tool": tool}
                )
                self._tool_output_bytes.add(
                    int(data.get("output_bytes", 0)), {"tool": tool}
                )
            elif event == "build_completed":
                self._builds.add(1, {"result": _build_result(data.get("status"))})
                self._build_duration.record(float(data["duration_seconds"]))
            elif event == "codex_session_started":
                self._codex_session_starts.add(
                    1,
                    {
                        "model": str(data["model"]),
                        "effort": str(data["reasoning_effort"]),
                    },
                )
            elif event in {
                "codex_turn_completed",
                "codex_turn_interrupted",
                "codex_turn_failed",
            }:
                attributes = {
                    "model": str(data["model"]),
                    "effort": str(data["reasoning_effort"]),
                    "result": event.removeprefix("codex_turn_"),
                }
                self._codex_turns.add(1, attributes)
                self._codex_turn_duration.record(
                    float(data["duration_seconds"]),
                    {key: attributes[key] for key in ("model", "effort")},
                )
            elif event in {"review_completed", "review_failed", "review_cancelled"}:
                attributes = {
                    "model": str(data["model"]),
                    "effort": str(data["reasoning_effort"]),
                    "focus": str(data["focus"]),
                    "result": event.removeprefix("review_"),
                }
                self._reviews.add(1, attributes)
                self._review_duration.record(
                    float(data["duration_seconds"]),
                    {key: attributes[key] for key in ("model", "effort", "focus")},
                )
            elif event in {"generation_completed", "generation_aborted"}:
                attributes = {
                    "outcome": event.removeprefix("generation_"),
                    "transition": str(data["transition"]),
                }
                self._generations.add(1, attributes)
                self._generation_duration.record(
                    float(data["duration_seconds"]), attributes
                )
            elif event.startswith("operator_"):
                self._operator_actions.add(
                    1,
                    {
                        "action": event.removeprefix("operator_"),
                        "result": str(data["result"]),
                    },
                )
            elif event in {"feature_requested", "feature_approved", "feature_denied"}:
                self._feature_requests.add(
                    1, {"event": event.removeprefix("feature_")}
                )
        except (KeyError, TypeError, ValueError) as error:
            self._degrade(f"metric recording failed for {event}: {error}")

    def _read_last_sequence(self) -> int:
        if not self._path.exists():
            return 0
        if self._path.is_symlink() or not self._path.is_file():
            raise ExperimentObservabilityError(
                f"event log is not a regular file: {self._path}"
            )
        previous = 0
        try:
            with self._path.open("r", encoding="utf-8", newline="") as source:
                for line_number, line in enumerate(source, start=1):
                    if not line.endswith("\n"):
                        raise ExperimentObservabilityError(
                            f"event log line {line_number} is incomplete"
                        )
                    try:
                        value = json.loads(line)
                    except json.JSONDecodeError as error:
                        raise ExperimentObservabilityError(
                            f"event log line {line_number} is malformed"
                        ) from error
                    _validate_envelope(value, line_number, previous)
                    previous = value["sequence"]
        except UnicodeDecodeError as error:
            raise ExperimentObservabilityError(
                "event log is not valid UTF-8"
            ) from error
        except OSError as error:
            raise ExperimentObservabilityError(
                f"could not read event log {self._path}: {error}"
            ) from error
        return previous

    def _degrade(self, reason: str) -> None:
        emit_warning = False
        with self._health_lock:
            if self._degraded_reason is None:
                self._degraded_reason = reason
            if not self._warning_emitted:
                self._warning_emitted = True
                emit_warning = True
        if emit_warning:
            try:
                warnings.warn(f"CodexOS observability degraded: {reason}")
            except Warning:
                # Warning policy must not turn telemetry into experiment control.
                pass


def _validate_envelope(value: Any, line_number: int, previous: int) -> None:
    required = {
        "schema_version",
        "sequence",
        "timestamp",
        "event",
        "generation",
        "data",
    }
    if not isinstance(value, dict) or set(value) != required:
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid envelope"
        )
    sequence = value["sequence"]
    generation = value["generation"]
    if value["schema_version"] != _SCHEMA_VERSION:
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an unsupported schema version"
        )
    if type(sequence) is not int or sequence <= previous:
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid sequence"
        )
    if generation is not None and (type(generation) is not int or generation < 0):
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid generation"
        )
    if not isinstance(value["event"], str) or not value["event"]:
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid event name"
        )
    if not isinstance(value["data"], dict):
        raise ExperimentObservabilityError(
            f"event log line {line_number} has invalid event data"
        )
    timestamp = value["timestamp"]
    if not isinstance(timestamp, str) or not timestamp.endswith("Z"):
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid timestamp"
        )
    try:
        parsed = datetime.fromisoformat(timestamp.removesuffix("Z") + "+00:00")
    except ValueError as error:
        raise ExperimentObservabilityError(
            f"event log line {line_number} has an invalid timestamp"
        ) from error
    if parsed.tzinfo != UTC:
        raise ExperimentObservabilityError(
            f"event log line {line_number} timestamp is not UTC"
        )


def _tool_status(value: object) -> str:
    if value == 0:
        return "success"
    if value == 1:
        return "guest_failure"
    if value == 2:
        return "harness_failure"
    return "other_failure"


def _build_result(value: object) -> str:
    if value == 0:
        return "success"
    if value == 1:
        return "guest_failure"
    return "harness_failure"

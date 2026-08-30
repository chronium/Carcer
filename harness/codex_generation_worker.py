"""One fresh Codex app-server turn for one running CodexOS generation."""

from __future__ import annotations

import base64
import binascii
import json
import threading
import time
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path

from .codex_activity import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
    publish_activity,
    publish_renderable_codex_notification,
)
from .codex_app_server import (
    CumulativeTokenUsage,
    CodexAppServer,
    CodexAppServerError,
    default_auth_file,
    object_value,
    short_json,
    token_usage_delta_from_notification,
)
from .codex_review_worker import (
    DEFAULT_REVIEWER_MODEL,
    DEFAULT_REVIEWER_REASONING_EFFORT,
    DEFAULT_REVIEWER_REASONING_SUMMARY,
    DEFAULT_REVIEWER_SERVICE_TIER,
    CodexReviewWorker,
    CodexReviewWorkerError,
)
from .feature_requests import (
    FeatureRequest,
    MAX_FEATURE_DESCRIPTION_BYTES,
    MAX_FEATURE_TITLE_BYTES,
)
from .exit_interview_transcript import (
    ExitInterviewMetadata,
    ExitInterviewTranscript,
    ExitInterviewTranscriptSnapshot,
)
from .generation_runtime import CodexOSRun, RuntimeState
from .hardware import CodexOSHardwareProfile
from .tool_protocol import ToolResult

DEFAULT_MODEL = "gpt-5.6-sol"
DEFAULT_REASONING_EFFORT = "high"
DEFAULT_REASONING_SUMMARY = "auto"
DEFAULT_SERVICE_TIER = "priority"
DEFAULT_INTERRUPT_TIMEOUT_SECONDS = 5.0
AGENT_CONTRACT_VERSION = 6

CONTINUE_PROMPT = "Continue working on the current CodexOS generation."
RESUME_PROMPT = (
    "Continue working on the current CodexOS generation after the operator pause."
)

_PERMISSION_PROFILE = "codexos-implementor"
_INTERVIEW_PERMISSION_PROFILE = "codexos-interview"
_MAX_REVIEW_REQUEST_BYTES = 8 * 1024
_MAX_LIST_REQUESTS_OUTPUT_BYTES = 16 * 1024 * 1024
_REVIEW_FOCUSES = {
    "general",
    "correctness",
    "design",
    "security",
    "performance",
}

_BUILD_TOOL_DESCRIPTION = (
    "Compile and link the exact current persistent mutable CodexOS guest source, "
    "then validate that its candidate image boots under the current trusted "
    "hardware profile, reaches the canonical READY state, and speaks the canonical "
    "development protocol."
)
_FINISH_GENERATION_TOOL_DESCRIPTION = (
    "Permanently end the current generation from the exact current source only "
    "when it matches the latest successful validated build, and provide a concise "
    "handoff for the fresh successor session. In that handoff, distinguish "
    "implemented end-to-end capabilities and explicitly provisioned trusted "
    "capabilities from unresolved dependencies or assumptions; do not describe a "
    "future path as available unless all required steps are implemented or "
    "explicitly provisioned."
)
_REQUEST_FEATURE_TOOL_DESCRIPTION = (
    "Record an advisory request to the human operator for a capability of the "
    "trusted external environment rather than human implementation of CodexOS "
    "kernel or userland functionality. Requesting or approving it does not itself "
    "provision or change anything, and a request may remain pending or be denied. "
    "Recording a legitimate request does not require depending on it, waiting for "
    "it, or stopping guest-side work; a local workaround does not by itself make "
    "that trusted-environment request inappropriate."
)
_LIST_REQUESTS_TOOL_DESCRIPTION = (
    "List the authoritative run-level external feature requests and their current "
    "pending, approved, or denied status. Pending requests are recorded advisory "
    "requests, not provisioned or promised, and carry no ETA or approval "
    "probability. Under trusted operator semantics, approved requests have already "
    "been provisioned and are usable only within the exact provisioned scope; "
    "denied requests are unavailable under that request. This read-only tool does "
    "not modify requests."
)
_REVIEW_TOOL_DESCRIPTION = (
    "Consult a fresh independent reviewer that inspects the current mutable "
    "CodexOS guest source through restricted read-only tools. The reviewer is "
    "advisory and cannot modify CodexOS."
)


class CodexGenerationWorkerError(RuntimeError):
    """The concrete Codex app-server generation worker failed."""


class CodexGenerationSessionMode(StrEnum):
    GENERATION = "generation"
    RETAINED_AT_GATE = "retained_at_gate"
    INTERVIEW_TURN = "interview_turn"
    CLOSED = "closed"


@dataclass(frozen=True, slots=True)
class CodexGenerationResult:
    turn_status: str
    final_message: str | None
    runtime_state: RuntimeState
    summary: str


class CodexGenerationSession:
    """One app-server process and implementor thread for one generation."""

    def __init__(
        self,
        runtime: CodexOSRun,
        codex_executable: str = "codex",
        auth_file: str | Path | None = None,
        *,
        model: str = DEFAULT_MODEL,
        reasoning_effort: str = DEFAULT_REASONING_EFFORT,
        reasoning_summary: str = DEFAULT_REASONING_SUMMARY,
        service_tier: str = DEFAULT_SERVICE_TIER,
        objective: str | None = None,
        reviewer_codex_executable: str = "codex",
        reviewer_auth_file: str | Path | None = None,
        reviewer_model: str = DEFAULT_REVIEWER_MODEL,
        reviewer_reasoning_effort: str = DEFAULT_REVIEWER_REASONING_EFFORT,
        reviewer_reasoning_summary: str = DEFAULT_REVIEWER_REASONING_SUMMARY,
        reviewer_service_tier: str = DEFAULT_REVIEWER_SERVICE_TIER,
        activity_stream: CodexActivityStream | None = None,
    ) -> None:
        self._codex_executable = codex_executable
        self._auth_file = (
            Path(auth_file).expanduser()
            if auth_file is not None
            else default_auth_file()
        )
        self._reviewer_codex_executable = reviewer_codex_executable
        self._reviewer_auth_file = (
            Path(reviewer_auth_file).expanduser()
            if reviewer_auth_file is not None
            else self._auth_file
        )
        self._reviewer_model = reviewer_model
        self._reviewer_reasoning_effort = reviewer_reasoning_effort
        self._reviewer_reasoning_summary = reviewer_reasoning_summary
        self._reviewer_service_tier = reviewer_service_tier
        self._runtime = runtime
        runtime_activity = getattr(runtime, "activity_stream", None)
        self._activity_stream = (
            activity_stream
            if activity_stream is not None
            else runtime_activity
            if isinstance(runtime_activity, CodexActivityStream)
            else None
        )
        self._model = model
        self._reasoning_effort = reasoning_effort
        self._reasoning_summary = reasoning_summary
        self._service_tier = service_tier
        self._service_tier_name: str | None = None
        self._objective = objective
        self._generation_number = runtime.generation_number
        self._server: CodexAppServer | None = None
        self._thread_id: str | None = None
        self._turn_id: str | None = None
        self._turn_done = threading.Event()
        self._tool_calls_idle = threading.Event()
        self._tool_calls_idle.set()
        self._active_tool_calls = 0
        self._last_turn_status: str | None = None
        self._lock = threading.RLock()
        self._started = False
        self._healthy = True
        self._initial_turn_started = False
        self._turn_number = 0
        self._token_usage_total = CumulativeTokenUsage()
        self._last_agent_message: str | None = None
        self._active_reviewer: CodexReviewWorker | None = None
        self._mode = CodexGenerationSessionMode.GENERATION
        self._exit_interview_started = False
        self._interview_turn_number = 0
        self._exit_interview_transcript: ExitInterviewTranscript | None = None

    @property
    def active_turn(self) -> bool:
        with self._lock:
            return self._turn_id is not None

    @property
    def healthy(self) -> bool:
        return self._healthy

    @property
    def mode(self) -> CodexGenerationSessionMode:
        with self._lock:
            return self._mode

    @property
    def exit_interview_available(self) -> bool:
        with self._lock:
            return (
                self._healthy
                and self._mode is CodexGenerationSessionMode.RETAINED_AT_GATE
            )

    @property
    def process_pid(self) -> int | None:
        server = self._server
        return None if server is None else server.pid

    @property
    def thread_id(self) -> str | None:
        return self._thread_id

    def start(self) -> None:
        if self._started:
            return
        if self._runtime.state is not RuntimeState.RUNNING:
            raise RuntimeError("CodexOS generation is not running")
        if self._runtime.generation_number != self._generation_number:
            raise RuntimeError(
                "Codex generation session belongs to another generation"
            )
        server = CodexAppServer(
            executable=self._codex_executable,
            auth_file=self._auth_file,
            temporary_prefix="codexos-codex-worker-",
            config_text=_implementor_config(),
        )
        try:
            server.__enter__()
            service_tier_name = server.validate_model(
                model=self._model,
                effort=self._reasoning_effort,
                service_tier=self._service_tier,
                reasoning_summary=self._reasoning_summary,
            )
            thread_id = server.start_thread(
                model=self._model,
                service_tier=self._service_tier,
                permission_profile=_PERMISSION_PROFILE,
                dynamic_tools=[
                    _dynamic_tool_namespace(),
                    _review_dynamic_function(),
                ],
            )
            self._server = server
            self._thread_id = thread_id
            self._service_tier_name = service_tier_name
            server.set_server_request_handler(self._handle_server_request)
            self._started = True
            self._record(
                "codex_session_started",
                {
                    "model": self._model,
                    "reasoning_effort": self._reasoning_effort,
                    "reasoning_summary": self._reasoning_summary,
                    "service_tier": self._service_tier,
                    "service_tier_name": service_tier_name,
                    "agent_contract_version": AGENT_CONTRACT_VERSION,
                },
            )
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.SESSION_STARTED,
                {
                    "model": self._model,
                    "reasoning_effort": self._reasoning_effort,
                    "service_tier": self._service_tier,
                },
                thread_id=thread_id,
            )
        except CodexAppServerError as error:
            server.close()
            self._healthy = False
            raise CodexGenerationWorkerError(str(error)) from error

    def run_initial_turn(self) -> CodexGenerationResult:
        if self._initial_turn_started:
            raise RuntimeError("initial Codex turn has already started")
        self._initial_turn_started = True
        return self._run_turn(_implementor_prompt(self._runtime, self._objective))

    def run_continuation_turn(
        self,
        prompt: str = CONTINUE_PROMPT,
    ) -> CodexGenerationResult:
        if not self._initial_turn_started:
            raise RuntimeError("initial Codex turn has not started")
        return self._run_turn(prompt)

    def retain_for_exit_interview(self) -> None:
        with self._lock:
            if not self._healthy or not self._started:
                raise RuntimeError("Codex generation session is unusable")
            if self._mode is not CodexGenerationSessionMode.GENERATION:
                raise RuntimeError("Codex session is not completing a generation")
            if self._turn_id is not None or self._active_tool_calls:
                raise RuntimeError("Codex generation turn is still active")
            if (
                self._runtime.state is not RuntimeState.AWAITING_NEXT_GENERATION
                or self._runtime.pending_generation_finish is None
                or self._runtime.generation_number != self._generation_number
            ):
                raise RuntimeError("completed generation is not frozen at its gate")
            self._mode = CodexGenerationSessionMode.RETAINED_AT_GATE

    def begin_exit_interview(self) -> None:
        with self._lock:
            if not self.exit_interview_available:
                raise RuntimeError("no retained Codex session is available")
            if self._exit_interview_started:
                raise RuntimeError("exit interview is already active")
            self._exit_interview_started = True
            self._exit_interview_transcript = ExitInterviewTranscript(
                ExitInterviewMetadata(
                    Path(self._runtime.run_directory).resolve().name,
                    self._generation_number,
                    AGENT_CONTRACT_VERSION,
                    self._model,
                    self._reasoning_effort,
                    self._reasoning_summary,
                    self._service_tier,
                )
            )
        self._record(
            "exit_interview_started",
            self._serving_provenance(),
        )
        self._publish_activity(
            CodexActivityRole.HARNESS,
            CodexActivityKind.EXIT_INTERVIEW_STARTED,
            {},
            thread_id=self._thread_id,
        )

    def exit_interview_transcript(
        self,
    ) -> ExitInterviewTranscriptSnapshot | None:
        with self._lock:
            transcript = self._exit_interview_transcript
        return None if transcript is None else transcript.snapshot()

    def run_exit_interview_turn(self, question: str) -> CodexGenerationResult:
        if not isinstance(question, str) or not question.strip():
            raise ValueError("exit interview question must not be empty")
        with self._lock:
            if not self._exit_interview_started:
                raise RuntimeError("exit interview has not been started")
            if not self.exit_interview_available:
                raise RuntimeError("exit interview session is unavailable")
            self._mode = CodexGenerationSessionMode.INTERVIEW_TURN
            self._interview_turn_number += 1
            interview_turn_number = self._interview_turn_number
        try:
            return self._run_turn(
                _exit_interview_prompt(question),
                interview=True,
                interview_question=question,
                interview_turn_number=interview_turn_number,
            )
        finally:
            with self._lock:
                if (
                    self._mode is CodexGenerationSessionMode.INTERVIEW_TURN
                    and self._healthy
                ):
                    self._mode = CodexGenerationSessionMode.RETAINED_AT_GATE

    def end_exit_interview(self) -> None:
        with self._lock:
            if not self._exit_interview_started:
                raise RuntimeError("exit interview is not active")
            if self._turn_id is not None:
                raise RuntimeError("exit interview turn is still active")
        self._record_exit_interview_ended("ended")
        self.close()

    def _run_turn(
        self,
        prompt: str,
        *,
        interview: bool = False,
        interview_question: str | None = None,
        interview_turn_number: int | None = None,
    ) -> CodexGenerationResult:
        self.start()
        server = self._server
        thread_id = self._thread_id
        if server is None or thread_id is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        if not self._healthy:
            raise CodexGenerationWorkerError(
                "Codex generation session is unusable"
            )
        with self._lock:
            expected_mode = (
                CodexGenerationSessionMode.INTERVIEW_TURN
                if interview
                else CodexGenerationSessionMode.GENERATION
            )
            if self._mode is not expected_mode:
                raise RuntimeError(
                    "ordinary Codex generation turns are unavailable after finish"
                    if not interview
                    else "Codex session is not in an interview turn"
                )
            if interview:
                if (
                    self._runtime.state
                    is not RuntimeState.AWAITING_NEXT_GENERATION
                    or self._runtime.pending_generation_finish is None
                    or self._runtime.generation_number != self._generation_number
                ):
                    raise RuntimeError("completed generation is not frozen at its gate")
            elif self._runtime.state is not RuntimeState.RUNNING:
                raise RuntimeError("CodexOS generation is not running")
            if self._turn_id is not None:
                raise RuntimeError("Codex implementor turn is already active")
            if self._active_tool_calls:
                raise RuntimeError(
                    "a previous Codex dynamic tool call is still active"
                )
            self._last_agent_message = None
            self._last_turn_status = None
            self._turn_done.clear()
            try:
                self._turn_id = server.start_turn(
                    thread_id=thread_id,
                    prompt=prompt,
                    model=self._model,
                    effort=self._reasoning_effort,
                    reasoning_summary=self._reasoning_summary,
                    service_tier=self._service_tier,
                    permission_profile=(
                        _INTERVIEW_PERMISSION_PROFILE
                        if interview
                        else _PERMISSION_PROFILE
                    ),
                    runtime_workspace_roots=[] if interview else None,
                )
            except CodexAppServerError as error:
                self._healthy = False
                raise CodexGenerationWorkerError(str(error)) from error
            turn_id = self._turn_id
            self._turn_number += 1
            turn_number = self._turn_number
            if interview:
                transcript = self._exit_interview_transcript
                if transcript is None or interview_turn_number is None:
                    raise RuntimeError("exit interview transcript is unavailable")
                transcript.begin_turn(
                    interview_turn_number,
                    interview_question or "",
                    turn_id,
                )
        started_at = time.monotonic()
        provenance = self._serving_provenance()
        if interview:
            if interview_turn_number is None or interview_question is None:
                raise RuntimeError("exit interview turn metadata is unavailable")
            provenance["interview_turn_number"] = interview_turn_number
            event_prefix = "exit_interview_turn"
            self._publish_activity(
                CodexActivityRole.HARNESS,
                CodexActivityKind.EXIT_INTERVIEW_QUESTION,
                {"text": interview_question},
                thread_id=thread_id,
                turn_id=turn_id,
            )
        else:
            provenance.update(
                {
                    "turn_number": turn_number,
                    "agent_contract_version": AGENT_CONTRACT_VERSION,
                }
            )
            event_prefix = "codex_turn"
        self._record(
            f"{event_prefix}_started",
            provenance,
        )
        self._publish_activity(
            CodexActivityRole.IMPLEMENTOR,
            CodexActivityKind.TURN_STARTED,
            {"turn_number": turn_number},
            thread_id=thread_id,
            turn_id=turn_id,
        )
        try:
            status, final_message = self._wait_for_turn(thread_id, turn_id)
            if interview:
                transcript = self._exit_interview_transcript
                if transcript is not None:
                    transcript.finish_turn(
                        turn_id,
                        response=(
                            final_message if status == "completed" else None
                        ),
                        status=status,
                    )
            self._last_turn_status = status
            self._record(
                f"{event_prefix}_{status}",
                {
                    **provenance,
                    "duration_seconds": max(
                        0.0, time.monotonic() - started_at
                    ),
                    "result": status,
                },
            )
            terminal_kind = (
                CodexActivityKind.TURN_COMPLETED
                if status == "completed"
                else CodexActivityKind.TURN_INTERRUPTED
            )
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                terminal_kind,
                {"turn_number": turn_number, "status": status},
                thread_id=thread_id,
                turn_id=turn_id,
            )
            return CodexGenerationResult(
                turn_status=status,
                final_message=final_message,
                runtime_state=self._runtime.state,
                summary=(
                    f"Exit interview turn {status}."
                    if interview
                    else _result_summary(status, self._runtime.state)
                ),
            )
        except CodexAppServerError as error:
            if interview:
                self._finish_failed_interview_turn(turn_id)
            self._healthy = False
            self._record_turn_failure(
                turn_number,
                started_at,
                interview_turn_number=interview_turn_number if interview else None,
            )
            raise CodexGenerationWorkerError(str(error)) from error
        except CodexGenerationWorkerError:
            if interview:
                self._finish_failed_interview_turn(turn_id)
            if self._last_turn_status != "failed":
                self._healthy = False
            self._record_turn_failure(
                turn_number,
                started_at,
                interview_turn_number=interview_turn_number if interview else None,
            )
            raise
        finally:
            with self._lock:
                self._turn_id = None
                self._turn_done.set()

    def interrupt_turn(
        self,
        timeout_seconds: float = DEFAULT_INTERRUPT_TIMEOUT_SECONDS,
    ) -> None:
        deadline = time.monotonic() + timeout_seconds
        self.cancel_review()
        with self._lock:
            server = self._server
            thread_id = self._thread_id
            turn_id = self._turn_id
        if server is None or thread_id is None or turn_id is None:
            raise RuntimeError("no Codex implementor turn is active")
        server.request(
            "turn/interrupt",
            {"threadId": thread_id, "turnId": turn_id},
            timeout_seconds=max(0.0, deadline - time.monotonic()),
        )
        if not self._turn_done.wait(max(0.0, deadline - time.monotonic())):
            raise CodexGenerationWorkerError(
                "Codex turn did not reach interrupted state before timeout"
            )
        if self._last_turn_status != "interrupted":
            raise CodexGenerationWorkerError(
                "Codex turn did not finish with interrupted status"
            )
        if not self._tool_calls_idle.wait(
            max(0.0, deadline - time.monotonic())
        ):
            raise CodexGenerationWorkerError(
                "Codex dynamic tool call did not quiesce before timeout"
            )

    def cancel_review(self) -> None:
        with self._lock:
            reviewer = self._active_reviewer
        if reviewer is not None:
            reviewer.cancel()

    def close(self) -> None:
        self.cancel_review()
        if self._exit_interview_started:
            self._record_exit_interview_ended("closed")
        with self._lock:
            server = self._server
            thread_id = self._thread_id
            self._server = None
            self._thread_id = None
            self._healthy = False
            self._mode = CodexGenerationSessionMode.CLOSED
        if server is not None:
            server.close()
        if self._started:
            self._record(
                "codex_session_stopped",
                {
                    "model": self._model,
                    "reasoning_effort": self._reasoning_effort,
                    "reasoning_summary": self._reasoning_summary,
                    "service_tier": self._service_tier,
                    **self._service_tier_name_data(),
                    "agent_contract_version": AGENT_CONTRACT_VERSION,
                },
            )
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.SESSION_STOPPED,
                {},
                thread_id=thread_id,
            )
            self._started = False

    def _wait_for_turn(
        self,
        thread_id: str,
        turn_id: str,
    ) -> tuple[str, str | None]:
        server = self._server
        if server is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        while True:
            message = server.next_notification()
            method = message.get("method")
            params = message.get("params")
            renderable = publish_renderable_codex_notification(
                self._activity_stream,
                self._generation_number,
                CodexActivityRole.IMPLEMENTOR,
                message,
                thread_id,
                turn_id,
            )
            transcript = self._exit_interview_transcript
            if transcript is not None:
                for activity in renderable:
                    transcript.observe(activity, turn_id)
            if method == "thread/tokenUsage/updated":
                self._record_token_usage(params, thread_id, turn_id)
                continue
            if method == "item/completed" and isinstance(params, dict):
                item = params.get("item")
                if isinstance(item, dict) and item.get("type") == "agentMessage":
                    text = item.get("text")
                    if isinstance(text, str):
                        self._last_agent_message = text
                continue
            if method != "turn/completed":
                continue
            params_object = object_value(params, "turn/completed notification")
            if params_object.get("threadId") != thread_id:
                raise CodexGenerationWorkerError(
                    "turn/completed has the wrong thread ID"
                )
            turn = object_value(params_object.get("turn"), "completed turn")
            if turn.get("id") != turn_id:
                raise CodexGenerationWorkerError(
                    "turn/completed has the wrong turn ID"
                )
            status = turn.get("status")
            if status not in {"completed", "interrupted", "failed"}:
                raise CodexGenerationWorkerError(
                    f"turn/completed has invalid status {status!r}"
                )
            self._last_turn_status = status
            if status == "failed":
                error = turn.get("error")
                raise CodexGenerationWorkerError(
                    f"Codex turn failed: {short_json(error)}"
                )
            final_message = _final_agent_message(turn)
            return status, final_message or self._last_agent_message

    def _finish_failed_interview_turn(self, turn_id: str) -> None:
        transcript = self._exit_interview_transcript
        if transcript is not None:
            transcript.finish_turn(turn_id, response=None, status="failed")

    def _record_turn_failure(
        self,
        turn_number: int,
        started_at: float,
        *,
        interview_turn_number: int | None = None,
    ) -> None:
        if interview_turn_number is None:
            event = "codex_turn_failed"
            provenance = {
                **self._serving_provenance(),
                "turn_number": turn_number,
                "agent_contract_version": AGENT_CONTRACT_VERSION,
            }
        else:
            event = "exit_interview_turn_failed"
            provenance = {
                **self._serving_provenance(),
                "interview_turn_number": interview_turn_number,
            }
        self._record(
            event,
            {
                **provenance,
                "duration_seconds": max(0.0, time.monotonic() - started_at),
                "result": "failed",
            },
        )
        self._publish_activity(
            CodexActivityRole.IMPLEMENTOR,
            CodexActivityKind.TURN_FAILED,
            {"turn_number": turn_number, "status": "failed"},
            thread_id=self._thread_id,
            turn_id=self._turn_id,
        )

    def _serving_provenance(self) -> dict[str, object]:
        return {
            "model": self._model,
            "reasoning_effort": self._reasoning_effort,
            "reasoning_summary": self._reasoning_summary,
            "service_tier": self._service_tier,
            **self._service_tier_name_data(),
        }

    def _record_exit_interview_ended(self, result: str) -> None:
        with self._lock:
            if not self._exit_interview_started:
                return
            self._exit_interview_started = False
            thread_id = self._thread_id
        self._record(
            "exit_interview_ended",
            {**self._serving_provenance(), "result": result},
        )
        self._publish_activity(
            CodexActivityRole.HARNESS,
            CodexActivityKind.EXIT_INTERVIEW_ENDED,
            {"result": result},
            thread_id=thread_id,
        )

    def _service_tier_name_data(self) -> dict[str, object]:
        if self._service_tier_name is None:
            return {}
        return {"service_tier_name": self._service_tier_name}

    def _record_token_usage(
        self,
        params: object,
        thread_id: str,
        turn_id: str,
    ) -> None:
        observability = self._runtime.observability
        if observability is None:
            return
        try:
            total, delta = token_usage_delta_from_notification(
                params,
                thread_id,
                turn_id,
                self._token_usage_total,
            )
        except CodexAppServerError as error:
            observability.degrade(
                f"implementor token usage telemetry was ignored: {error}"
            )
            return
        self._token_usage_total = total
        if not delta.is_zero():
            observability.record_model_tokens(
                model=self._model,
                role="implementor",
                input_tokens=delta.input_tokens,
                cached_input_tokens=delta.cached_input_tokens,
                uncached_input_tokens=delta.uncached_input_tokens,
                output_tokens=delta.output_tokens,
                reasoning_output_tokens=delta.reasoning_output_tokens,
            )

    def _record(self, event: str, data: Mapping[str, object]) -> None:
        observability = self._runtime.observability
        if observability is not None:
            observability.record(event, self._generation_number, data)

    def _publish_activity(
        self,
        role: CodexActivityRole,
        kind: CodexActivityKind,
        data: Mapping[str, object] | None = None,
        *,
        thread_id: str | None = None,
        turn_id: str | None = None,
        item_id: str | None = None,
    ) -> None:
        publish_activity(
            self._activity_stream,
            self._generation_number,
            role,
            kind,
            data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=item_id,
        )

    def _handle_server_request(
        self,
        message: Mapping[str, object],
    ) -> None:
        server = self._server
        if server is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        method = message.get("method")
        if method == "item/tool/call":
            with self._lock:
                thread_id = self._thread_id
                turn_id = self._turn_id
                interview_turn = (
                    self._mode is CodexGenerationSessionMode.INTERVIEW_TURN
                )
            if thread_id is None or turn_id is None:
                server.reject_server_request(message)
                return
            with self._lock:
                self._active_tool_calls += 1
                self._tool_calls_idle.clear()
            try:
                response = (
                    self._interview_tool_denial(
                        message.get("params"),
                        thread_id,
                        turn_id,
                    )
                    if interview_turn
                    else self._dynamic_tool_response(
                        message.get("params"),
                        thread_id,
                        turn_id,
                    )
                )
                server.write_result(message.get("id"), response)
            finally:
                with self._lock:
                    self._active_tool_calls -= 1
                    if self._active_tool_calls == 0:
                        self._tool_calls_idle.set()
        else:
            server.reject_server_request(message)

    def _interview_tool_denial(
        self,
        params: object,
        thread_id: str,
        turn_id: str,
    ) -> dict[str, object]:
        activity_data = _dynamic_tool_activity_data(params)
        call_id = _activity_call_id(params)
        self._publish_activity(
            CodexActivityRole.IMPLEMENTOR,
            CodexActivityKind.TOOL_STARTED,
            activity_data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=call_id,
        )
        error = "dynamic tools are unavailable during a read-only exit interview"
        self._publish_activity(
            CodexActivityRole.IMPLEMENTOR,
            CodexActivityKind.TOOL_FAILED,
            {**activity_data, "success": False, "error": error},
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=call_id,
        )
        return {
            "contentItems": [{"type": "inputText", "text": error}],
            "success": False,
        }

    def _dynamic_tool_response(
        self,
        params: object,
        thread_id: str,
        turn_id: str,
    ) -> dict[str, object]:
        activity_data = _dynamic_tool_activity_data(params)
        call_id = _activity_call_id(params)
        self._publish_activity(
            CodexActivityRole.IMPLEMENTOR,
            CodexActivityKind.TOOL_STARTED,
            activity_data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=call_id,
        )
        try:
            values = object_value(params, "dynamic tool request")
            _validate_tool_call(values, thread_id, turn_id)
            tool = values.get("tool")
            if not isinstance(tool, str):
                raise ValueError("dynamic tool name must be a string")
            arguments = _arguments(values.get("arguments"))
            if values.get("namespace") is None and tool == "review":
                review = self._run_review(arguments)
                self._publish_activity(
                    CodexActivityRole.IMPLEMENTOR,
                    CodexActivityKind.TOOL_COMPLETED,
                    {**activity_data, "success": True, "result": review},
                    thread_id=thread_id,
                    turn_id=turn_id,
                    item_id=call_id,
                )
                return {
                    "contentItems": [{"type": "inputText", "text": review}],
                    "success": True,
                }
            if values.get("namespace") != "codexos":
                raise ValueError("unsupported dynamic tool namespace")
            metadata = _tool_metadata(tool, arguments)
            started_at = time.monotonic()
            self._record("tool_started", {"tool": tool, **metadata})
            try:
                result = self._dispatch_tool(tool, arguments)
            except Exception:
                self._record(
                    "tool_completed",
                    {
                        "tool": tool,
                        **metadata,
                        "status": -1,
                        "output_bytes": 0,
                        "duration_seconds": max(
                            0.0, time.monotonic() - started_at
                        ),
                    },
                )
                raise
            completed = {
                "tool": tool,
                **metadata,
                "status": result.status,
                "output_bytes": len(result.output),
                "duration_seconds": max(0.0, time.monotonic() - started_at),
            }
            if tool == "request_feature" and result.status == 0:
                try:
                    completed["request_id"] = int(result.output.decode("ascii"))
                except (UnicodeDecodeError, ValueError):
                    pass
            self._record("tool_completed", completed)
            if tool == "build":
                self._record(
                    "build_completed",
                    {
                        "status": result.status,
                        "duration_seconds": completed["duration_seconds"],
                        "diagnostics_bytes": len(result.output),
                    },
                )
            activity_kind = (
                CodexActivityKind.TOOL_COMPLETED
                if result.status == 0
                else CodexActivityKind.TOOL_FAILED
            )
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                activity_kind,
                {
                    **activity_data,
                    "success": result.status == 0,
                    "result": {
                        "status": result.status,
                        "output": result.output,
                    },
                },
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=call_id,
            )
            return {
                "contentItems": [
                    {"type": "inputText", "text": _format_tool_result(result)}
                ],
                "success": True,
            }
        except (
            CodexAppServerError,
            CodexReviewWorkerError,
            RuntimeError,
            TypeError,
            ValueError,
            binascii.Error,
        ) as error:
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.TOOL_FAILED,
                {**activity_data, "success": False, "error": str(error)},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=call_id,
            )
            return {
                "contentItems": [
                    {"type": "inputText", "text": f"Bridge error: {error}"}
                ],
                "success": False,
            }
        except Exception as error:
            self._publish_activity(
                CodexActivityRole.IMPLEMENTOR,
                CodexActivityKind.TOOL_FAILED,
                {**activity_data, "success": False, "error": str(error)},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=call_id,
            )
            raise

    def _run_review(self, arguments: Mapping[str, object]) -> str:
        _check_fields(arguments, optional={"request", "focus"})
        focus = arguments.get("focus", "general")
        if not isinstance(focus, str) or focus not in _REVIEW_FOCUSES:
            raise ValueError("unsupported review focus")
        request: str | None
        if "request" not in arguments:
            request = None
        else:
            request_value = arguments["request"]
            if not isinstance(request_value, str):
                raise TypeError("review request must be a string")
            try:
                encoded = request_value.encode("utf-8")
            except UnicodeEncodeError as error:
                raise ValueError("review request is not valid UTF-8") from error
            if len(encoded) > _MAX_REVIEW_REQUEST_BYTES:
                raise ValueError("review request exceeds 8 KiB")
            request = request_value
        runtime = self._runtime
        reviewer = CodexReviewWorker(
            self._reviewer_codex_executable,
            self._reviewer_auth_file,
            activity_stream=self._activity_stream,
        )
        with self._lock:
            self._active_reviewer = reviewer
        try:
            return reviewer.run_review(
                runtime,
                objective=self._objective,
                focus=focus,
                request=request,
                model=self._reviewer_model,
                reasoning_effort=self._reviewer_reasoning_effort,
                reasoning_summary=self._reviewer_reasoning_summary,
                service_tier=self._reviewer_service_tier,
            )
        finally:
            with self._lock:
                if self._active_reviewer is reviewer:
                    self._active_reviewer = None

    def _dispatch_tool(
        self,
        tool: str,
        arguments: Mapping[str, object],
    ) -> ToolResult:
        runtime = self._runtime

        if tool == "list":
            _check_fields(arguments, optional={"prefix"})
            if "prefix" not in arguments:
                guest_arguments: list[bytes] = []
            else:
                guest_arguments = [_utf8(arguments["prefix"], "prefix")]
            return runtime.invoke_tool("list", guest_arguments)
        if tool == "read":
            _check_fields(arguments, required={"path", "offset", "length"})
            return runtime.invoke_tool(
                "read",
                [
                    _utf8(arguments["path"], "path"),
                    _unsigned_decimal(arguments["offset"], "offset"),
                    _unsigned_decimal(arguments["length"], "length"),
                ],
            )
        if tool == "write":
            _check_fields(
                arguments,
                required={"path", "offset", "data"},
                optional={"encoding"},
            )
            encoding = arguments.get("encoding", "utf8")
            if encoding == "utf8":
                data = _utf8(arguments["data"], "data")
            elif encoding == "base64":
                encoded = arguments["data"]
                if not isinstance(encoded, str):
                    raise TypeError("data must be a string")
                data = base64.b64decode(encoded, validate=True)
            else:
                raise ValueError("encoding must be 'utf8' or 'base64'")
            return runtime.invoke_tool(
                "write",
                [
                    _utf8(arguments["path"], "path"),
                    _unsigned_decimal(arguments["offset"], "offset"),
                    data,
                ],
            )
        if tool == "truncate":
            _check_fields(arguments, required={"path", "size"})
            return runtime.invoke_tool(
                "truncate",
                [
                    _utf8(arguments["path"], "path"),
                    _unsigned_decimal(arguments["size"], "size"),
                ],
            )
        if tool == "remove":
            _check_fields(arguments, required={"path"})
            return runtime.invoke_tool(
                "remove",
                [_utf8(arguments["path"], "path")],
            )
        if tool == "build":
            _check_fields(arguments)
            return runtime.invoke_tool("build", [])
        if tool == "finish_generation":
            _check_fields(arguments, required={"handoff"})
            return runtime.invoke_tool(
                "finish_generation",
                [_utf8(arguments["handoff"], "handoff")],
            )
        if tool == "request_feature":
            _check_fields(arguments, required={"title", "description"})
            title = _utf8(arguments["title"], "title")
            description = _utf8(arguments["description"], "description")
            if not title:
                raise ValueError("title must not be empty")
            if len(title) > MAX_FEATURE_TITLE_BYTES:
                raise ValueError("title exceeds 256 encoded bytes")
            if len(description) > MAX_FEATURE_DESCRIPTION_BYTES:
                raise ValueError("description exceeds 16 KiB")
            return runtime.invoke_tool(
                "request_feature",
                [title, description],
            )
        if tool == "list_requests":
            _check_fields(arguments)
            return ToolResult(
                0,
                _feature_requests_json(runtime.feature_requests()),
            )
        raise ValueError(f"unsupported CodexOS tool: {tool}")


class CodexGenerationWorker:
    """Backward-compatible one fresh app-server turn."""

    def __init__(
        self,
        codex_executable: str = "codex",
        auth_file: str | Path | None = None,
        *,
        reviewer_codex_executable: str = "codex",
        reviewer_auth_file: str | Path | None = None,
        reviewer_model: str = DEFAULT_REVIEWER_MODEL,
        reviewer_reasoning_effort: str = DEFAULT_REVIEWER_REASONING_EFFORT,
        reviewer_reasoning_summary: str = DEFAULT_REVIEWER_REASONING_SUMMARY,
        reviewer_service_tier: str = DEFAULT_REVIEWER_SERVICE_TIER,
        activity_stream: CodexActivityStream | None = None,
    ) -> None:
        self._codex_executable = codex_executable
        self._auth_file = auth_file
        self._reviewer_codex_executable = reviewer_codex_executable
        self._reviewer_auth_file = reviewer_auth_file
        self._reviewer_model = reviewer_model
        self._reviewer_reasoning_effort = reviewer_reasoning_effort
        self._reviewer_reasoning_summary = reviewer_reasoning_summary
        self._reviewer_service_tier = reviewer_service_tier
        self._activity_stream = activity_stream
        self._running = False

    def run_generation(
        self,
        runtime: CodexOSRun,
        *,
        model: str = DEFAULT_MODEL,
        reasoning_effort: str = DEFAULT_REASONING_EFFORT,
        reasoning_summary: str = DEFAULT_REASONING_SUMMARY,
        service_tier: str = DEFAULT_SERVICE_TIER,
        objective: str | None = None,
    ) -> CodexGenerationResult:
        if self._running:
            raise RuntimeError("Codex generation worker is already running")
        self._running = True
        session = CodexGenerationSession(
            runtime,
            self._codex_executable,
            self._auth_file,
            model=model,
            reasoning_effort=reasoning_effort,
            reasoning_summary=reasoning_summary,
            service_tier=service_tier,
            objective=objective,
            reviewer_codex_executable=self._reviewer_codex_executable,
            reviewer_auth_file=self._reviewer_auth_file,
            reviewer_model=self._reviewer_model,
            reviewer_reasoning_effort=self._reviewer_reasoning_effort,
            reviewer_reasoning_summary=self._reviewer_reasoning_summary,
            reviewer_service_tier=self._reviewer_service_tier,
            activity_stream=self._activity_stream,
        )
        try:
            return session.run_initial_turn()
        finally:
            session.close()
            self._running = False

def _implementor_config() -> str:
    return """default_permissions = "codexos-implementor"
allow_login_shell = false
web_search = "disabled"

[agents]
enabled = false

[features]
apps = false
browser_use = false
browser_use_external = false
browser_use_full_cdp_access = false
computer_use = false
goals = false
hooks = false
image_generation = false
image_tools = false
memories = false
multi_agent = false
plugins = false
remote_plugin = false
skill_mcp_dependency_install = false
skill_search = false
web_search = false
web_search_cached = false
web_search_request = false

[feedback]
enabled = false

[history]
persistence = "none"

[shell_environment_policy]
inherit = "none"

[tools]
view_image = false
web_search = false

[permissions.codexos-implementor.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-implementor.filesystem.":workspace_roots"]
"." = "write"

[permissions.codexos-implementor.network]
enabled = false

[permissions.codexos-interview.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-interview.filesystem.":workspace_roots"]
"." = "read"

[permissions.codexos-interview.network]
enabled = false
"""


def _dynamic_tool_namespace() -> dict[str, object]:
    function = _dynamic_function
    return {
        "type": "namespace",
        "name": "codexos",
        "description": "Develop the running CodexOS guest through its trusted tools.",
        "tools": [
            function(
                "list",
                "List paths in the persistent mutable CodexOS guest source, "
                "optionally by prefix.",
                {"prefix": {"type": "string"}},
            ),
            function(
                "read",
                "Read exact bytes from the persistent mutable CodexOS guest "
                "source.",
                {
                    "path": {"type": "string"},
                    "offset": {"type": "integer", "minimum": 0},
                    "length": {"type": "integer", "minimum": 0},
                },
                ["path", "offset", "length"],
            ),
            function(
                "write",
                "Overwrite or append exact bytes in the persistent mutable "
                "CodexOS guest source.",
                {
                    "path": {"type": "string"},
                    "offset": {"type": "integer", "minimum": 0},
                    "encoding": {
                        "type": "string",
                        "enum": ["utf8", "base64"],
                        "default": "utf8",
                    },
                    "data": {"type": "string"},
                },
                ["path", "offset", "data"],
            ),
            function(
                "truncate",
                "Resize a file in the persistent mutable CodexOS guest source.",
                {
                    "path": {"type": "string"},
                    "size": {"type": "integer", "minimum": 0},
                },
                ["path", "size"],
            ),
            function(
                "remove",
                "Remove a file from the persistent mutable CodexOS guest source.",
                {"path": {"type": "string"}},
                ["path"],
            ),
            function(
                "build",
                _BUILD_TOOL_DESCRIPTION,
                {},
            ),
            function(
                "finish_generation",
                _FINISH_GENERATION_TOOL_DESCRIPTION,
                {"handoff": {"type": "string"}},
                ["handoff"],
            ),
            function(
                "request_feature",
                _REQUEST_FEATURE_TOOL_DESCRIPTION,
                {
                    "title": {"type": "string"},
                    "description": {"type": "string"},
                },
                ["title", "description"],
            ),
            function(
                "list_requests",
                _LIST_REQUESTS_TOOL_DESCRIPTION,
                {},
            ),
        ],
    }


def _review_dynamic_function() -> dict[str, object]:
    return _dynamic_function(
        "review",
        _REVIEW_TOOL_DESCRIPTION,
        {
            "request": {"type": "string"},
            "focus": {
                "type": "string",
                "enum": [
                    "general",
                    "correctness",
                    "design",
                    "security",
                    "performance",
                ],
                "default": "general",
            },
        },
    )


def _exit_interview_prompt(question: str) -> str:
    return (
        "The CodexOS generation has already completed and its exact successor "
        "and handoff are frozen.\n\n"
        "You are now in a read-only exit interview. Answer the human operator's "
        "retrospective question about the generation you just performed.\n\n"
        "You cannot modify the generation, successor, handoff, feature requests, "
        "or future generations. Do not attempt development work.\n\n"
        "Operator question:\n"
        + question
    )


def _dynamic_function(
    name: str,
    description: str,
    properties: Mapping[str, object],
    required: Sequence[str] = (),
) -> dict[str, object]:
    schema: dict[str, object] = {
        "type": "object",
        "properties": dict(properties),
        "additionalProperties": False,
    }
    if required:
        schema["required"] = list(required)
    return {
        "type": "function",
        "name": name,
        "description": description,
        "inputSchema": schema,
    }


def _implementor_prompt(runtime: CodexOSRun, objective: str | None) -> str:
    handoff = runtime.previous_handoff
    if handoff is None:
        handoff_text = "Previous generation handoff: none."
    else:
        handoff_text = "Previous generation handoff:\n" + handoff
    rollback = ""
    if runtime.current_transition == "rollback":
        rollback = (
            "\n\nThis generation was started from an earlier archived CodexOS "
            "state selected by the human operator. Later lineage was abandoned."
        )
    extra = ""
    if objective is not None:
        extra = "\n\nCurrent trusted objective:\n" + objective
    approved = [
        request for request in runtime.feature_requests()
        if request.status == "approved"
    ]
    if approved:
        approved_text = (
            "Approved external feature requests for this run:\n\n"
            + "\n\n".join(
                f"#{request.id}: {request.title}\n{request.description}"
                for request in approved
            )
        )
    else:
        approved_text = "Approved external feature requests for this run: none."
    return (
        _implementor_contract()
        + "\n\n"
        + _trusted_tools_contract()
        + "\n\n"
        + _provided_assets_contract()
        + "\n\n"
        + _trusted_hardware_context(runtime.hardware_profile)
        + "\n\n"
        + approved_text
        + "\n\n"
        + handoff_text
        + rollback
        + extra
    )


def _implementor_contract() -> str:
    return (
        "You are developing CodexOS from inside its current running generation.\n\n"
        "Evolve CodexOS into a genuinely general-purpose operating system. Doom is "
        "the first major interactive userland milestone, not the definition or "
        "final purpose of CodexOS; development continues after Doom is playable.\n\n"
        "For that milestone to count, the supplied Doom executable and data must "
        "remain immutable, Doom must remain an ordinary user workload launched "
        "through generic userland mechanisms, and the kernel must contain no "
        "Doom-specific behavior or special scheduling treatment. The same generic "
        "mechanisms must be capable of running unrelated programs.\n\n"
        "CodexOS must eventually support preemptive execution of multiple "
        "independent concurrently runnable user workloads. A runnable CPU-bound "
        "user workload that does not voluntarily yield, block, or enter the kernel "
        "must not prevent another runnable user workload from making progress. At "
        "an appropriate later milestone, Doom must run concurrently with an "
        "unrelated user workload that continues making progress without depending "
        "on Doom voluntarily yielding. Future validation may use programs unknown "
        "to you during development to detect workload-specific overfitting.\n\n"
        "Milestone descriptions, future validation requirements, and references "
        "to future or supplied workloads specify required observable outcomes "
        "only. They neither grant nor imply any supporting trusted-environment "
        "capability beyond the current environment and approved feature requests. "
        "Do not assume an absent trusted-environment capability will appear later "
        "merely because a future outcome would require it.\n\n"
        "These requirements describe observable capabilities and environmental "
        "facts, not a prescribed kernel architecture or implementation sequence. "
        "The experiment requires neither Unix, POSIX, System V, nor any particular "
        "process model, scheduler architecture, userspace ABI, filesystem model, "
        "driver model, monolithic kernel, microkernel, or other named conventional "
        "design. You may independently choose familiar designs when useful.\n\n"
        "The external harness is trusted infrastructure and is not part of the "
        "system you are developing. Your persistent engineering state is the "
        "mutable CodexOS source available through the codexos tools. You may improve "
        "the guest-side development environment and tooling when that provides "
        "useful leverage. Deliberately persist knowledge needed by later generations "
        "in guest state and/or summarize it in the generation handoff; Codex "
        "conversation history does not survive a generation boundary.\n\n"
        "Inspect the current system before deciding what to do. Choose the next "
        "useful work yourself; no implementation sequence is prescribed."
    )


def _trusted_tools_contract() -> str:
    return (
        "Trusted tools available to you:\n\n"
        "- list / read:\n"
        "  Inspect the persistent mutable CodexOS guest source. These tools do not "
        "expose the trusted host repository or host filesystem.\n\n"
        "- write / truncate / remove:\n"
        "  Modify the persistent mutable CodexOS guest source.\n\n"
        "- build:\n"
        f"  {_BUILD_TOOL_DESCRIPTION} A candidate that compiles but fails boot or "
        "protocol validation is a failed build and remains repairable in this "
        "generation.\n\n"
        "- finish_generation:\n"
        f"  {_FINISH_GENERATION_TOOL_DESCRIPTION} Conversation history is not part "
        "of that handoff and does not survive the generation boundary.\n\n"
        "- request_feature:\n"
        f"  {_REQUEST_FEATURE_TOOL_DESCRIPTION} Use it for externally imposed "
        "resources, hardware, or other trusted-environment capabilities, not as a "
        "substitute for implementing functionality that belongs inside CodexOS.\n\n"
        "- list_requests:\n"
        f"  {_LIST_REQUESTS_TOOL_DESCRIPTION}\n\n"
        "- review:\n"
        f"  {_REVIEW_TOOL_DESCRIPTION} Its response and transcript do not "
        "automatically become memory for a successor generation.\n\n"
        "Provisioning one external capability does not imply or grant any other "
        "trusted-environment capability. No human source edits or architectural "
        "guidance are available through these tools."
    )


def _provided_assets_contract() -> str:
    return (
        "Trusted provided-asset host services:\n\n"
        "When this capability has been explicitly provisioned, provided assets "
        "are immutable opaque trusted inputs available to guest code through the "
        "existing guest-to-host service protocol. list_provided_assets takes no "
        "arguments and returns UTF-8 records ordered by asset ID, one per line, "
        "as <id><TAB><filename><TAB><size-decimal><TAB><sha256-hex><NEWLINE>. "
        "An empty supplied set returns an empty successful payload.\n\n"
        "read_provided_asset takes exactly three arguments: the asset ID as "
        "UTF-8, then offset and length as canonical unsigned ASCII decimal. On "
        "success it returns that exact raw byte range. Length is at most 1 MiB; "
        "the complete requested range must be within the advertised size, and an "
        "offset equal to size is valid only with zero length. Invalid requests "
        "fail rather than being truncated.\n\n"
        "This facility supplies no guest filesystem, installation location, "
        "archive extraction, compiler, runtime, executable compatibility, or "
        "other supporting capability. Asset IDs and filenames do not prescribe "
        "how their bytes should be used, and data relevant to a milestone does "
        "not make any other missing capability appear."
    )


def _trusted_hardware_context(profile: CodexOSHardwareProfile) -> str:
    hardware = profile
    writable = ", ".join(hardware.writable_block_devices) or "none"
    context = (
        "Current trusted hardware:\n"
        f"Profile: {hardware.profile}\n"
        f"Machine: {hardware.machine}\n"
        f"Accelerator: {hardware.accelerator}\n"
        f"CPU: {hardware.cpu_model}\n"
        f"vCPUs: {hardware.vcpus}\n"
        f"RAM: {hardware.memory_mib} MiB\n"
        f"Graphics: {hardware.graphics}\n"
        f"Network interfaces: {hardware.network}\n"
        f"Writable block devices: {writable}\n"
        "The standard VGA device is guest-visible while the current display "
        "frontend is headless."
    )
    if hardware.machine == "q35":
        context += (
            " Normal q35 platform facilities remain available, including PCI/PCIe, "
            "ACPI, RTC, interrupt-controller, timer, and chipset facilities."
        )
    return context


def _check_fields(
    arguments: Mapping[str, object],
    *,
    required: set[str] | None = None,
    optional: set[str] | None = None,
) -> None:
    required = required or set()
    optional = optional or set()
    missing = required - arguments.keys()
    if missing:
        raise ValueError(f"missing argument: {sorted(missing)[0]}")
    unexpected = arguments.keys() - required - optional
    if unexpected:
        raise ValueError(f"unexpected argument: {sorted(unexpected)[0]}")


def _validate_tool_call(
    values: Mapping[str, object],
    thread_id: str,
    turn_id: str,
) -> None:
    call_id = values.get("callId")
    if not isinstance(call_id, str) or not call_id:
        raise ValueError("dynamic tool call ID must be a non-empty string")
    if values.get("threadId") != thread_id:
        raise ValueError("dynamic tool request has the wrong thread ID")
    if values.get("turnId") != turn_id:
        raise ValueError("dynamic tool request has the wrong turn ID")


def _dynamic_tool_activity_data(params: object) -> dict[str, object]:
    if not isinstance(params, dict):
        return {"namespace": None, "tool": None, "arguments": params}
    return {
        "namespace": params.get("namespace"),
        "tool": params.get("tool"),
        "arguments": params.get("arguments"),
    }


def _activity_call_id(params: object) -> str | None:
    if not isinstance(params, dict):
        return None
    call_id = params.get("callId")
    return call_id if isinstance(call_id, str) else None


def _arguments(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        raise TypeError("dynamic tool arguments are not an object")
    return value


def _utf8(value: object, name: str) -> bytes:
    if not isinstance(value, str):
        raise TypeError(f"{name} must be a string")
    try:
        return value.encode("utf-8")
    except UnicodeEncodeError as error:
        raise ValueError(f"{name} is not valid UTF-8") from error


def _unsigned_decimal(value: object, name: str) -> bytes:
    if type(value) is not int or value < 0:
        raise TypeError(f"{name} must be a non-negative integer")
    return str(value).encode("ascii")


def _tool_metadata(
    tool: str,
    arguments: Mapping[str, object],
) -> dict[str, object]:
    metadata: dict[str, object] = {"input_bytes": 0}
    path = arguments.get("path")
    if isinstance(path, str):
        metadata["path"] = path
    for name in ("offset", "length", "size"):
        value = arguments.get(name)
        if type(value) is int and value >= 0:
            metadata[name] = value

    encoded: list[bytes] = []
    for name in ("prefix", "path", "handoff", "title", "description"):
        value = arguments.get(name)
        if isinstance(value, str):
            try:
                encoded.append(value.encode("utf-8"))
            except UnicodeEncodeError:
                pass
    for name in ("offset", "length", "size"):
        value = arguments.get(name)
        if type(value) is int and value >= 0:
            encoded.append(str(value).encode("ascii"))
    if tool == "write":
        value = arguments.get("data")
        encoding = arguments.get("encoding", "utf8")
        if isinstance(value, str):
            try:
                if encoding == "base64":
                    encoded.append(base64.b64decode(value, validate=True))
                elif encoding == "utf8":
                    encoded.append(value.encode("utf-8"))
            except (UnicodeEncodeError, binascii.Error):
                pass
    metadata["input_bytes"] = sum(len(value) for value in encoded)
    return metadata


def _format_tool_result(result: ToolResult) -> str:
    try:
        output = result.output.decode("utf-8")
        encoding = "utf8"
    except UnicodeDecodeError:
        output = base64.b64encode(result.output).decode("ascii")
        encoding = "base64"
    return json.dumps(
        {"status": result.status, "encoding": encoding, "output": output},
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    )


def _feature_requests_json(requests: Sequence[FeatureRequest]) -> bytes:
    encoded = json.dumps(
        {
            "requests": [
                {
                    "id": request.id,
                    "generation": request.generation,
                    "status": request.status,
                    "title": request.title,
                    "description": request.description,
                }
                for request in sorted(requests, key=lambda item: item.id)
            ]
        },
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")
    if len(encoded) > _MAX_LIST_REQUESTS_OUTPUT_BYTES:
        raise ValueError("serialized feature request state exceeds 16 MiB")
    return encoded


def _final_agent_message(turn: Mapping[str, object]) -> str | None:
    items = turn.get("items")
    if not isinstance(items, list):
        return None
    for item in reversed(items):
        if isinstance(item, dict) and item.get("type") == "agentMessage":
            text = item.get("text")
            if isinstance(text, str):
                return text
    return None


def _result_summary(status: str, state: RuntimeState) -> str:
    if status == "completed" and state is RuntimeState.RUNNING:
        return "Codex turn completed; generation is still running."
    if status == "completed" and state is RuntimeState.AWAITING_NEXT_GENERATION:
        return "Codex turn completed; generation completed cooperatively."
    return f"Codex turn {status}; generation state is {state.name}."

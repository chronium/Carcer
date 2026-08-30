"""Fresh read-only Codex consultation for one running CodexOS generation."""

from __future__ import annotations

import base64
import json
import threading
import time
from collections.abc import Mapping
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
from .generation_runtime import CodexOSRun, RuntimeState
from .tool_protocol import ToolResult

DEFAULT_REVIEWER_MODEL = "gpt-5.6-luna"
DEFAULT_REVIEWER_REASONING_EFFORT = "high"
DEFAULT_REVIEWER_REASONING_SUMMARY = "auto"
DEFAULT_REVIEWER_SERVICE_TIER = "priority"

_PERMISSION_PROFILE = "codexos-reviewer"


class CodexReviewWorkerError(RuntimeError):
    """One concrete Codex reviewer consultation failed."""


class CodexReviewWorker:
    """Run one fresh, read-only reviewer turn."""

    def __init__(
        self,
        codex_executable: str = "codex",
        auth_file: str | Path | None = None,
        *,
        activity_stream: CodexActivityStream | None = None,
    ) -> None:
        self._codex_executable = codex_executable
        self._auth_file = (
            Path(auth_file).expanduser()
            if auth_file is not None
            else default_auth_file()
        )
        self._lock = threading.Lock()
        self._server: CodexAppServer | None = None
        self._cancelled = threading.Event()
        self._activity_stream = activity_stream

    def cancel(self) -> None:
        self._cancelled.set()
        with self._lock:
            server = self._server
        if server is not None:
            server.close()

    def run_review(
        self,
        runtime: CodexOSRun,
        *,
        objective: str | None,
        focus: str,
        request: str | None,
        model: str = DEFAULT_REVIEWER_MODEL,
        reasoning_effort: str = DEFAULT_REVIEWER_REASONING_EFFORT,
        reasoning_summary: str = DEFAULT_REVIEWER_REASONING_SUMMARY,
        service_tier: str = DEFAULT_REVIEWER_SERVICE_TIER,
    ) -> str:
        if runtime.state is not RuntimeState.RUNNING:
            raise CodexReviewWorkerError("CodexOS generation is not running")
        if self._cancelled.is_set():
            raise CodexReviewWorkerError(
                "Codex reviewer consultation was cancelled"
            )
        started_at = time.monotonic()
        outcome = "failed"
        service_tier_name: str | None = None
        try:
            with CodexAppServer(
                executable=self._codex_executable,
                auth_file=self._auth_file,
                temporary_prefix="codexos-reviewer-",
                config_text=_reviewer_config(),
            ) as server:
                with self._lock:
                    self._server = server
                if self._cancelled.is_set():
                    raise CodexAppServerError(
                        "Codex reviewer consultation was cancelled"
                    )
                service_tier_name = server.validate_model(
                    model=model,
                    effort=reasoning_effort,
                    service_tier=service_tier,
                    reasoning_summary=reasoning_summary,
                )
                self._record(
                    runtime,
                    "review_started",
                    model,
                    reasoning_effort,
                    reasoning_summary,
                    service_tier,
                    service_tier_name,
                    focus,
                    None,
                )
                self._publish(
                    runtime,
                    CodexActivityKind.REVIEW_STARTED,
                    {
                        "model": model,
                        "reasoning_effort": reasoning_effort,
                        "service_tier": service_tier,
                        "focus": focus,
                    },
                )
                thread_id = server.start_thread(
                    model=model,
                    service_tier=service_tier,
                    permission_profile=_PERMISSION_PROFILE,
                    dynamic_tools=[_reviewer_tool_namespace()],
                    require_read_only=True,
                )
                turn_ready = threading.Event()
                turn_value: list[str] = []
                server.set_server_request_handler(
                    lambda message: self._handle_ready_server_request(
                        server,
                        runtime,
                        message,
                        thread_id,
                        turn_ready,
                        turn_value,
                    )
                )
                try:
                    turn_id = server.start_turn(
                        thread_id=thread_id,
                        prompt=_reviewer_prompt(objective, focus, request),
                        model=model,
                        effort=reasoning_effort,
                        reasoning_summary=reasoning_summary,
                        service_tier=service_tier,
                        permission_profile=_PERMISSION_PROFILE,
                    )
                    turn_value.append(turn_id)
                    self._publish(
                        runtime,
                        CodexActivityKind.TURN_STARTED,
                        {"focus": focus},
                        thread_id=thread_id,
                        turn_id=turn_id,
                    )
                finally:
                    turn_ready.set()
                result = self._wait_for_turn(
                    server,
                    runtime,
                    thread_id,
                    turn_id,
                    model,
                )
                outcome = "completed"
                return result
        except CodexAppServerError as error:
            raise CodexReviewWorkerError(str(error)) from error
        finally:
            with self._lock:
                self._server = None
            if self._cancelled.is_set():
                outcome = "cancelled"
            self._record(
                runtime,
                f"review_{outcome}",
                model,
                reasoning_effort,
                reasoning_summary,
                service_tier,
                service_tier_name,
                focus,
                max(0.0, time.monotonic() - started_at),
            )
            review_kind = {
                "completed": CodexActivityKind.REVIEW_COMPLETED,
                "cancelled": CodexActivityKind.REVIEW_CANCELLED,
                "failed": CodexActivityKind.REVIEW_FAILED,
            }[outcome]
            self._publish(
                runtime,
                review_kind,
                {
                    "model": model,
                    "reasoning_effort": reasoning_effort,
                    "service_tier": service_tier,
                    "focus": focus,
                    "duration_seconds": max(
                        0.0, time.monotonic() - started_at
                    ),
                },
            )

    def _wait_for_turn(
        self,
        server: CodexAppServer,
        runtime: CodexOSRun,
        thread_id: str,
        turn_id: str,
        model: str,
    ) -> str:
        last_agent_message: str | None = None
        token_usage_total = CumulativeTokenUsage()
        while True:
            message = server.next_notification()
            method = message.get("method")
            params = message.get("params")
            publish_renderable_codex_notification(
                self._activity_stream,
                runtime.generation_number,
                CodexActivityRole.REVIEWER,
                message,
                thread_id,
                turn_id,
            )
            if method == "thread/tokenUsage/updated":
                observability = runtime.observability
                if observability is None:
                    continue
                try:
                    token_usage_total, delta = (
                        token_usage_delta_from_notification(
                            params,
                            thread_id,
                            turn_id,
                            token_usage_total,
                        )
                    )
                except CodexAppServerError as error:
                    observability.degrade(
                        f"reviewer token usage telemetry was ignored: {error}"
                    )
                    continue
                if not delta.is_zero():
                    observability.record_model_tokens(
                        model=model,
                        role="reviewer",
                        input_tokens=delta.input_tokens,
                        cached_input_tokens=delta.cached_input_tokens,
                        uncached_input_tokens=delta.uncached_input_tokens,
                        output_tokens=delta.output_tokens,
                        reasoning_output_tokens=delta.reasoning_output_tokens,
                    )
                continue
            if method == "item/completed" and isinstance(params, dict):
                item = params.get("item")
                if isinstance(item, dict) and item.get("type") == "agentMessage":
                    text = item.get("text")
                    if isinstance(text, str):
                        last_agent_message = text
                continue
            if method != "turn/completed":
                continue
            values = object_value(params, "turn/completed notification")
            if values.get("threadId") != thread_id:
                raise CodexReviewWorkerError(
                    "review turn/completed has the wrong thread ID"
                )
            turn = object_value(values.get("turn"), "completed review turn")
            if turn.get("id") != turn_id:
                raise CodexReviewWorkerError(
                    "review turn/completed has the wrong turn ID"
                )
            status = turn.get("status")
            if status != "completed":
                if status not in {"interrupted", "failed"}:
                    raise CodexReviewWorkerError(
                        f"review turn has invalid status {status!r}"
                    )
                kind = (
                    CodexActivityKind.TURN_INTERRUPTED
                    if status == "interrupted"
                    else CodexActivityKind.TURN_FAILED
                )
                self._publish(
                    runtime,
                    kind,
                    {"status": status},
                    thread_id=thread_id,
                    turn_id=turn_id,
                )
                raise CodexReviewWorkerError(
                    f"Codex reviewer turn {status}: {short_json(turn.get('error'))}"
                )
            final_message = _final_agent_message(turn) or last_agent_message
            if final_message is None:
                raise CodexReviewWorkerError(
                    "Codex reviewer completed without a final response"
                )
            self._publish(
                runtime,
                CodexActivityKind.TURN_COMPLETED,
                {"status": "completed"},
                thread_id=thread_id,
                turn_id=turn_id,
            )
            return final_message

    def _publish(
        self,
        runtime: CodexOSRun,
        kind: CodexActivityKind,
        data: Mapping[str, object] | None = None,
        *,
        thread_id: str | None = None,
        turn_id: str | None = None,
        item_id: str | None = None,
    ) -> None:
        publish_activity(
            self._activity_stream,
            runtime.generation_number,
            CodexActivityRole.REVIEWER,
            kind,
            data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=item_id,
        )

    @staticmethod
    def _record(
        runtime: CodexOSRun,
        event: str,
        model: str,
        reasoning_effort: str,
        reasoning_summary: str,
        service_tier: str,
        service_tier_name: str | None,
        focus: str,
        duration_seconds: float | None,
    ) -> None:
        if runtime.observability is None:
            return
        data: dict[str, object] = {
            "model": model,
            "reasoning_effort": reasoning_effort,
            "reasoning_summary": reasoning_summary,
            "service_tier": service_tier,
            "focus": focus,
        }
        if service_tier_name is not None:
            data["service_tier_name"] = service_tier_name
        if duration_seconds is not None:
            data["duration_seconds"] = duration_seconds
        runtime.observability.record(
            event,
            runtime.generation_number,
            data,
        )

    def _handle_ready_server_request(
        self,
        server: CodexAppServer,
        runtime: CodexOSRun,
        message: Mapping[str, object],
        thread_id: str,
        turn_ready: threading.Event,
        turn_value: list[str],
    ) -> None:
        turn_ready.wait()
        if not turn_value:
            server.reject_server_request(message)
            return
        self._handle_server_request(
            server,
            runtime,
            message,
            thread_id,
            turn_value[0],
        )

    def _handle_server_request(
        self,
        server: CodexAppServer,
        runtime: CodexOSRun,
        message: Mapping[str, object],
        thread_id: str,
        turn_id: str,
    ) -> None:
        if message.get("method") != "item/tool/call":
            server.reject_server_request(message)
            return
        response = self._dynamic_tool_response(
            runtime,
            message.get("params"),
            thread_id,
            turn_id,
        )
        server.write_result(message.get("id"), response)

    def _dynamic_tool_response(
        self,
        runtime: CodexOSRun,
        params: object,
        thread_id: str,
        turn_id: str,
    ) -> dict[str, object]:
        activity_data = _dynamic_tool_activity_data(params)
        call_id = _activity_call_id(params)
        self._publish(
            runtime,
            CodexActivityKind.TOOL_STARTED,
            activity_data,
            thread_id=thread_id,
            turn_id=turn_id,
            item_id=call_id,
        )
        try:
            values = object_value(params, "reviewer dynamic tool request")
            _validate_tool_call(values, thread_id, turn_id)
            if values.get("namespace") != "codexos":
                raise ValueError("unsupported reviewer dynamic tool namespace")
            tool = values.get("tool")
            if not isinstance(tool, str):
                raise ValueError("reviewer dynamic tool name must be a string")
            arguments = _arguments(values.get("arguments"))
            result = _dispatch_read_only_tool(runtime, tool, arguments)
            activity_kind = (
                CodexActivityKind.TOOL_COMPLETED
                if result.status == 0
                else CodexActivityKind.TOOL_FAILED
            )
            self._publish(
                runtime,
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
        except (CodexAppServerError, RuntimeError, TypeError, ValueError) as error:
            self._publish(
                runtime,
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
            self._publish(
                runtime,
                CodexActivityKind.TOOL_FAILED,
                {**activity_data, "success": False, "error": str(error)},
                thread_id=thread_id,
                turn_id=turn_id,
                item_id=call_id,
            )
            raise


def _dispatch_read_only_tool(
    runtime: CodexOSRun,
    tool: str,
    arguments: Mapping[str, object],
) -> ToolResult:
    if tool == "list":
        _check_fields(arguments, optional={"prefix"})
        guest_arguments = (
            []
            if "prefix" not in arguments
            else [_utf8(arguments["prefix"], "prefix")]
        )
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
    raise ValueError(f"unsupported reviewer CodexOS tool: {tool}")


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


def _reviewer_tool_namespace() -> dict[str, object]:
    return {
        "type": "namespace",
        "name": "codexos",
        "description": "Read the current mutable CodexOS guest source.",
        "tools": [
            _dynamic_function(
                "list",
                "List current mutable guest source paths, optionally by prefix.",
                {"prefix": {"type": "string"}},
            ),
            _dynamic_function(
                "read",
                "Read exact bytes from a current mutable guest source file.",
                {
                    "path": {"type": "string"},
                    "offset": {"type": "integer", "minimum": 0},
                    "length": {"type": "integer", "minimum": 0},
                },
                ["path", "offset", "length"],
            ),
        ],
    }


def _dynamic_function(
    name: str,
    description: str,
    properties: Mapping[str, object],
    required: list[str] | None = None,
) -> dict[str, object]:
    schema: dict[str, object] = {
        "type": "object",
        "properties": dict(properties),
        "additionalProperties": False,
    }
    if required:
        schema["required"] = required
    return {
        "type": "function",
        "name": name,
        "description": description,
        "inputSchema": schema,
    }


def _reviewer_prompt(
    objective: str | None,
    focus: str,
    request: str | None,
) -> str:
    objective_text = (
        "No trusted per-generation objective was supplied."
        if objective is None
        else "Trusted current objective:\n" + objective
    )
    request_text = (
        "No additional review request was supplied."
        if request is None
        else "Implementor review request:\n" + request
    )
    return (
        "You are a read-only reviewer for the current CodexOS generation.\n\n"
        "CodexOS is an autonomous experiment evolving toward a general-purpose "
        "operating system. Doom is the first major interactive userland milestone, "
        "not the final purpose of the OS.\n\n"
        "Another Codex implementor is actively developing the current mutable "
        "source. You are here only to inspect that work and provide an independent "
        "technical review.\n\n"
        "Read enough of the current source through the available codexos tools to "
        "understand the work in context.\n\n"
        "Identify only issues that genuinely matter to the success of the current "
        "work, including where relevant correctness bugs, logic errors, security "
        "vulnerabilities, design flaws, incorrect assumptions, unnecessary "
        "complexity that materially increases risk, performance problems that "
        "materially matter, and divergence from the stated objective.\n\n"
        "For every finding, explain the issue, its concrete impact, and a specific "
        "suggested change. Categorize findings as Blocking, Non-blocking, or "
        "Suggestions. Blocking findings must be addressed for the current work to "
        "succeed. Non-blocking findings are real problems worth addressing. "
        "Suggestions are lower-priority improvements with a real expected impact.\n\n"
        "Do not report formatting or naming preferences, comment grammar, stylistic "
        "taste, minor refactors, speculative abstractions, generic best practices "
        "with no concrete impact, or alternative designs merely because you prefer "
        "them.\n\n"
        "Do not redesign CodexOS, prescribe an unrelated architecture, modify "
        "anything, build anything, or try to finish the generation. If you find no "
        "meaningful issues, say exactly that clearly. Your findings are advisory; "
        "the implementor decides what to do with them.\n\n"
        f"Review focus: {focus}. Prioritize that focus, while still reporting any "
        "blocking issue you discover.\n\n"
        + objective_text
        + "\n\n"
        + request_text
    )


def _reviewer_config() -> str:
    return """default_permissions = "codexos-reviewer"
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

[permissions.codexos-reviewer.filesystem]
":root" = "deny"
":minimal" = "read"
":tmpdir" = "deny"
":slash_tmp" = "deny"

[permissions.codexos-reviewer.filesystem.":workspace_roots"]
"." = "read"

[permissions.codexos-reviewer.network]
enabled = false
"""


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


def _arguments(value: object) -> dict[str, object]:
    if not isinstance(value, dict):
        raise TypeError("dynamic tool arguments are not an object")
    return value


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

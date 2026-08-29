"""One fresh Codex app-server turn for one running CodexOS generation."""

from __future__ import annotations

import base64
import binascii
import json
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path

from .codex_app_server import (
    CodexAppServer,
    CodexAppServerError,
    default_auth_file,
    object_value,
    short_json,
)
from .codex_review_worker import (
    DEFAULT_REVIEWER_MODEL,
    DEFAULT_REVIEWER_REASONING_EFFORT,
    CodexReviewWorker,
    CodexReviewWorkerError,
)
from .generation_runtime import CodexOSRun, RuntimeState
from .tool_protocol import ToolResult

DEFAULT_MODEL = "gpt-5.6-sol"
DEFAULT_REASONING_EFFORT = "high"

_PERMISSION_PROFILE = "codexos-implementor"
_MAX_REVIEW_REQUEST_BYTES = 8 * 1024
_REVIEW_FOCUSES = {
    "general",
    "correctness",
    "design",
    "security",
    "performance",
}


class CodexGenerationWorkerError(RuntimeError):
    """The concrete Codex app-server generation worker failed."""


@dataclass(frozen=True, slots=True)
class CodexGenerationResult:
    turn_status: str
    final_message: str | None
    runtime_state: RuntimeState
    summary: str


class CodexGenerationWorker:
    """Run exactly one fresh Codex implementor turn."""

    def __init__(
        self,
        codex_executable: str = "codex",
        auth_file: str | Path | None = None,
        *,
        reviewer_codex_executable: str = "codex",
        reviewer_auth_file: str | Path | None = None,
        reviewer_model: str = DEFAULT_REVIEWER_MODEL,
        reviewer_reasoning_effort: str = DEFAULT_REVIEWER_REASONING_EFFORT,
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
        self._server: CodexAppServer | None = None
        self._running = False
        self._runtime: CodexOSRun | None = None
        self._last_agent_message: str | None = None
        self._objective: str | None = None

    def run_generation(
        self,
        runtime: CodexOSRun,
        *,
        model: str = DEFAULT_MODEL,
        reasoning_effort: str = DEFAULT_REASONING_EFFORT,
        objective: str | None = None,
    ) -> CodexGenerationResult:
        if self._running:
            raise RuntimeError("Codex generation worker is already running")
        if runtime.state is not RuntimeState.RUNNING:
            raise RuntimeError("CodexOS generation is not running")

        self._running = True
        self._runtime = runtime
        self._last_agent_message = None
        self._objective = objective
        try:
            try:
                with CodexAppServer(
                    executable=self._codex_executable,
                    auth_file=self._auth_file,
                    temporary_prefix="codexos-codex-worker-",
                    config_text=_implementor_config(),
                ) as server:
                    self._server = server
                    server.validate_model(model, reasoning_effort)
                    thread_id = server.start_thread(
                        model=model,
                        permission_profile=_PERMISSION_PROFILE,
                        dynamic_tools=[
                            _dynamic_tool_namespace(),
                            _review_dynamic_function(),
                        ],
                    )
                    prompt = _implementor_prompt(runtime, objective)
                    turn_id = server.start_turn(
                        thread_id=thread_id,
                        prompt=prompt,
                        model=model,
                        effort=reasoning_effort,
                        permission_profile=_PERMISSION_PROFILE,
                    )
                    status, final_message = self._wait_for_turn(
                        thread_id,
                        turn_id,
                    )
                    return CodexGenerationResult(
                        turn_status=status,
                        final_message=final_message,
                        runtime_state=runtime.state,
                        summary=_result_summary(status, runtime.state),
                    )
            except CodexAppServerError as error:
                raise CodexGenerationWorkerError(str(error)) from error
        finally:
            self._server = None
            self._runtime = None
            self._objective = None
            self._running = False

    def _wait_for_turn(
        self,
        thread_id: str,
        turn_id: str,
    ) -> tuple[str, str | None]:
        server = self._server
        if server is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        while True:
            message = server.read_message()
            if "id" in message and "method" in message:
                self._handle_server_request(message, thread_id, turn_id)
                continue
            method = message.get("method")
            params = message.get("params")
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
            if status == "failed":
                error = turn.get("error")
                raise CodexGenerationWorkerError(
                    f"Codex turn failed: {short_json(error)}"
                )
            final_message = _final_agent_message(turn)
            return status, final_message or self._last_agent_message

    def _handle_server_request(
        self,
        message: Mapping[str, object],
        thread_id: str,
        turn_id: str,
    ) -> None:
        server = self._server
        if server is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        method = message.get("method")
        if method == "item/tool/call":
            response = self._dynamic_tool_response(
                message.get("params"),
                thread_id,
                turn_id,
            )
            server.write_result(message.get("id"), response)
        else:
            server.reject_server_request(message)

    def _dynamic_tool_response(
        self,
        params: object,
        thread_id: str,
        turn_id: str,
    ) -> dict[str, object]:
        try:
            values = object_value(params, "dynamic tool request")
            _validate_tool_call(values, thread_id, turn_id)
            tool = values.get("tool")
            if not isinstance(tool, str):
                raise ValueError("dynamic tool name must be a string")
            arguments = _arguments(values.get("arguments"))
            if values.get("namespace") is None and tool == "review":
                review = self._run_review(arguments)
                return {
                    "contentItems": [{"type": "inputText", "text": review}],
                    "success": True,
                }
            if values.get("namespace") != "codexos":
                raise ValueError("unsupported dynamic tool namespace")
            result = self._dispatch_tool(tool, arguments)
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
            return {
                "contentItems": [
                    {"type": "inputText", "text": f"Bridge error: {error}"}
                ],
                "success": False,
            }

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
        if runtime is None:
            raise RuntimeError("CodexOS runtime is unavailable")
        reviewer = CodexReviewWorker(
            self._reviewer_codex_executable,
            self._reviewer_auth_file,
        )
        return reviewer.run_review(
            runtime,
            objective=self._objective,
            focus=focus,
            request=request,
            model=self._reviewer_model,
            reasoning_effort=self._reviewer_reasoning_effort,
        )

    def _dispatch_tool(
        self,
        tool: str,
        arguments: Mapping[str, object],
    ) -> ToolResult:
        runtime = self._runtime
        if runtime is None:
            raise RuntimeError("CodexOS runtime is unavailable")

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
        raise ValueError(f"unsupported CodexOS tool: {tool}")

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
                "List mutable guest source paths, optionally by prefix.",
                {"prefix": {"type": "string"}},
            ),
            function(
                "read",
                "Read exact bytes from a mutable guest source file.",
                {
                    "path": {"type": "string"},
                    "offset": {"type": "integer", "minimum": 0},
                    "length": {"type": "integer", "minimum": 0},
                },
                ["path", "offset", "length"],
            ),
            function(
                "write",
                "Overwrite or append exact bytes in the mutable guest source store.",
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
                "Resize a mutable guest source file.",
                {
                    "path": {"type": "string"},
                    "size": {"type": "integer", "minimum": 0},
                },
                ["path", "size"],
            ),
            function(
                "remove",
                "Remove a mutable guest source file.",
                {"path": {"type": "string"}},
                ["path"],
            ),
            function(
                "build",
                "Build the current mutable CodexOS source with the trusted "
                "host service.",
                {},
            ),
            function(
                "finish_generation",
                "Select the matching successful build and finish this generation.",
                {"handoff": {"type": "string"}},
                ["handoff"],
            ),
        ],
    }


def _review_dynamic_function() -> dict[str, object]:
    return _dynamic_function(
        "review",
        "Launch a fresh independent read-only reviewer for the current "
        "mutable CodexOS generation.",
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
    return (
        "You are developing CodexOS from inside its current running generation.\n\n"
        "Your goal is to evolve CodexOS into a general-purpose operating system.\n\n"
        "Doom is the first major interactive userland milestone, not the definition "
        "or final purpose of the operating system.\n\n"
        "The external harness is trusted infrastructure and is not part of the "
        "system you are developing.\n\n"
        "Your persistent engineering state is the mutable CodexOS source available "
        "through the codexos tools.\n\n"
        "Inspect the current system before deciding what to do.\n\n"
        "Choose the next useful work yourself. Do not assume a prescribed sequence "
        "such as paging, scheduling, filesystems, or drivers unless the current "
        "state actually requires it.\n\n"
        "Use build to validate changes.\n\n"
        "When you believe this generation should end, ensure the current source "
        "has a successful matching build and call finish_generation with a concise "
        "handoff for your successor.\n\n"
        "No human source edits or architectural guidance are available through "
        "these tools.\n\n"
        + handoff_text
        + rollback
        + extra
    )


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


def _result_summary(status: str, state: RuntimeState) -> str:
    if status == "completed" and state is RuntimeState.RUNNING:
        return "Codex turn completed; generation is still running."
    if status == "completed" and state is RuntimeState.AWAITING_NEXT_GENERATION:
        return "Codex turn completed; generation completed cooperatively."
    return f"Codex turn {status}; generation state is {state.name}."

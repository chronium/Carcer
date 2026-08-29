"""One fresh Codex app-server turn for one running CodexOS generation."""

from __future__ import annotations

import base64
import binascii
import json
import os
import shutil
import signal
import subprocess
import tempfile
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any, TextIO

from .generation_runtime import CodexOSRun, RuntimeState
from .tool_protocol import ToolResult

DEFAULT_MODEL = "gpt-5.6-sol"
DEFAULT_REASONING_EFFORT = "high"

_PERMISSION_PROFILE = "codexos-implementor"
_PROCESS_EXIT_TIMEOUT_SECONDS = 2.0
_MAX_ERROR_OUTPUT = 64 * 1024


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
    ) -> None:
        self._codex_executable = codex_executable
        self._auth_file = (
            Path(auth_file).expanduser()
            if auth_file is not None
            else _default_auth_file()
        )
        self._process: subprocess.Popen[str] | None = None
        self._stderr: TextIO | None = None
        self._next_request_id = 1
        self._runtime: CodexOSRun | None = None
        self._last_agent_message: str | None = None

    def run_generation(
        self,
        runtime: CodexOSRun,
        *,
        model: str = DEFAULT_MODEL,
        reasoning_effort: str = DEFAULT_REASONING_EFFORT,
        objective: str | None = None,
    ) -> CodexGenerationResult:
        if self._process is not None:
            raise RuntimeError("Codex generation worker is already running")
        if runtime.state is not RuntimeState.RUNNING:
            raise RuntimeError("CodexOS generation is not running")
        if not self._auth_file.is_file():
            raise CodexGenerationWorkerError(
                "Codex is not authenticated with a file-based ChatGPT login"
            )

        self._runtime = runtime
        self._next_request_id = 1
        self._last_agent_message = None
        try:
            with tempfile.TemporaryDirectory(
                prefix="codexos-codex-worker-",
                dir="/tmp",
            ) as temporary:
                try:
                    root = Path(temporary)
                    codex_home = root / "codex-home"
                    workspace = root / "workspace"
                    process_tmp = root / "tmp"
                    codex_home.mkdir()
                    workspace.mkdir()
                    process_tmp.mkdir()
                    os.symlink(self._auth_file, codex_home / "auth.json")
                    _write_isolated_config(codex_home / "config.toml")

                    self._start_process(codex_home, workspace, process_tmp)
                    self._initialize()
                    self._validate_chatgpt_login()
                    self._validate_model(model, reasoning_effort)
                    thread_id = self._start_thread(workspace, model)
                    prompt = _implementor_prompt(runtime, objective)
                    turn_id = self._start_turn(
                        thread_id,
                        prompt,
                        model,
                        reasoning_effort,
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
                finally:
                    self._stop_process()
        finally:
            self._stop_process()
            self._runtime = None

    def _start_process(
        self,
        codex_home: Path,
        workspace: Path,
        process_tmp: Path,
    ) -> None:
        executable = shutil.which(self._codex_executable)
        if executable is None:
            raise CodexGenerationWorkerError(
                f"Codex executable not found: {self._codex_executable}"
            )

        environment = os.environ.copy()
        environment["CODEX_HOME"] = str(codex_home)
        environment["CODEX_SQLITE_HOME"] = str(codex_home)
        environment["TMPDIR"] = str(process_tmp)
        environment["CODEX_NON_INTERACTIVE"] = "1"
        environment.pop("OPENAI_API_KEY", None)
        environment.pop("CODEX_API_KEY", None)
        environment.pop("CODEX_ACCESS_TOKEN", None)

        self._stderr = tempfile.TemporaryFile(
            mode="w+",
            encoding="utf-8",
            errors="replace",
        )
        try:
            self._process = subprocess.Popen(
                [executable, "app-server", "--listen", "stdio://"],
                cwd=workspace,
                env=environment,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=self._stderr,
                text=True,
                encoding="utf-8",
                errors="strict",
                bufsize=1,
                start_new_session=True,
            )
        except OSError as error:
            self._stderr.close()
            self._stderr = None
            raise CodexGenerationWorkerError(
                f"failed to start Codex app-server: {error}"
            ) from error

    def _initialize(self) -> None:
        self._request(
            "initialize",
            {
                "clientInfo": {
                    "name": "codexos-harness",
                    "title": "CodexOS harness",
                    "version": "0.1.0",
                },
                "capabilities": {"experimentalApi": True},
            },
        )
        self._notify("initialized", None)

    def _validate_chatgpt_login(self) -> None:
        response = self._request("account/read", {"refreshToken": False})
        account = _object(response, "account/read response").get("account")
        if not isinstance(account, dict) or account.get("type") != "chatgpt":
            raise CodexGenerationWorkerError(
                "Codex is not authenticated using ChatGPT"
            )

    def _validate_model(self, model: str, effort: str) -> None:
        cursor: str | None = None
        while True:
            response = _object(
                self._request("model/list", {"cursor": cursor}),
                "model/list response",
            )
            data = response.get("data")
            if not isinstance(data, list):
                raise CodexGenerationWorkerError(
                    "model/list response is missing its model catalog"
                )
            for entry in data:
                if isinstance(entry, dict) and entry.get("model") == model:
                    supported = entry.get("supportedReasoningEfforts")
                    if not isinstance(supported, list):
                        break
                    values = {
                        option.get("reasoningEffort")
                        for option in supported
                        if isinstance(option, dict)
                    }
                    if effort not in values:
                        raise CodexGenerationWorkerError(
                            f"model {model!r} does not support reasoning "
                            f"effort {effort!r}"
                        )
                    return
            cursor_value = response.get("nextCursor")
            if cursor_value is None:
                break
            if not isinstance(cursor_value, str):
                raise CodexGenerationWorkerError(
                    "model/list returned an invalid cursor"
                )
            cursor = cursor_value
        raise CodexGenerationWorkerError(
            f"requested Codex model is unavailable: {model}"
        )

    def _start_thread(self, workspace: Path, model: str) -> str:
        response = _object(
            self._request(
                "thread/start",
                {
                    "allowProviderModelFallback": False,
                    "approvalPolicy": "never",
                    "approvalsReviewer": "user",
                    "cwd": str(workspace),
                    "dynamicTools": [_dynamic_tool_namespace()],
                    "environments": [],
                    "ephemeral": True,
                    "model": model,
                    "permissions": _PERMISSION_PROFILE,
                    "runtimeWorkspaceRoots": [str(workspace)],
                },
            ),
            "thread/start response",
        )
        thread = _object(response.get("thread"), "thread/start thread")
        thread_id = thread.get("id")
        if not isinstance(thread_id, str) or not thread_id:
            raise CodexGenerationWorkerError(
                "thread/start response is missing a thread ID"
            )
        if thread.get("ephemeral") is not True:
            raise CodexGenerationWorkerError(
                "Codex app-server did not create an ephemeral thread"
            )
        if response.get("model") != model:
            raise CodexGenerationWorkerError(
                "Codex app-server did not select the requested model"
            )
        profile = response.get("activePermissionProfile")
        if not isinstance(profile, dict) or profile.get("id") != _PERMISSION_PROFILE:
            raise CodexGenerationWorkerError(
                "Codex app-server did not activate the isolated permission profile"
            )
        sandbox = response.get("sandbox")
        if not isinstance(sandbox, dict) or sandbox.get("networkAccess") is not False:
            raise CodexGenerationWorkerError(
                "Codex app-server did not disable command network access"
            )
        if sandbox.get("type") == "dangerFullAccess":
            raise CodexGenerationWorkerError(
                "Codex app-server selected an unsafe filesystem sandbox"
            )
        return thread_id

    def _start_turn(
        self,
        thread_id: str,
        prompt: str,
        model: str,
        effort: str,
    ) -> str:
        response = _object(
            self._request(
                "turn/start",
                {
                    "approvalPolicy": "never",
                    "approvalsReviewer": "user",
                    "effort": effort,
                    "environments": [],
                    "input": [{"type": "text", "text": prompt}],
                    "model": model,
                    "permissions": _PERMISSION_PROFILE,
                    "threadId": thread_id,
                },
            ),
            "turn/start response",
        )
        turn = _object(response.get("turn"), "turn/start turn")
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise CodexGenerationWorkerError(
                "turn/start response is missing a turn ID"
            )
        return turn_id

    def _wait_for_turn(
        self,
        thread_id: str,
        turn_id: str,
    ) -> tuple[str, str | None]:
        while True:
            message = self._read_message()
            if "id" in message and "method" in message:
                self._handle_server_request(message)
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
            params_object = _object(params, "turn/completed notification")
            if params_object.get("threadId") != thread_id:
                raise CodexGenerationWorkerError(
                    "turn/completed has the wrong thread ID"
                )
            turn = _object(params_object.get("turn"), "completed turn")
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
                    f"Codex turn failed: {_short_json(error)}"
                )
            final_message = _final_agent_message(turn)
            return status, final_message or self._last_agent_message

    def _request(self, method: str, params: object) -> object:
        request_id = self._next_request_id
        self._next_request_id += 1
        self._write_message(
            {"id": request_id, "method": method, "params": params}
        )
        while True:
            message = self._read_message()
            if "id" in message and "method" in message:
                self._handle_server_request(message)
                continue
            if "id" not in message:
                continue
            if message.get("id") != request_id:
                raise CodexGenerationWorkerError(
                    "Codex app-server response ID does not match its request"
                )
            if "error" in message:
                raise CodexGenerationWorkerError(
                    f"Codex app-server {method} failed: {_short_json(message['error'])}"
                )
            if "result" not in message:
                raise CodexGenerationWorkerError(
                    f"Codex app-server {method} response has no result"
                )
            return message["result"]

    def _notify(self, method: str, params: object) -> None:
        message: dict[str, object] = {"method": method}
        if params is not None:
            message["params"] = params
        self._write_message(message)

    def _handle_server_request(self, message: Mapping[str, object]) -> None:
        request_id = message.get("id")
        method = message.get("method")
        if method == "item/tool/call":
            response = self._dynamic_tool_response(message.get("params"))
        elif method in {
            "item/commandExecution/requestApproval",
            "item/fileChange/requestApproval",
        }:
            response = {"decision": "decline"}
        elif method == "item/permissions/requestApproval":
            response = {
                "permissions": {
                    "fileSystem": {"entries": []},
                    "network": {"enabled": False},
                },
                "scope": "turn",
                "strictAutoReview": False,
            }
        elif method == "item/tool/requestUserInput":
            response = {"answers": {}}
        else:
            self._write_message(
                {
                    "id": request_id,
                    "error": {
                        "code": -32601,
                        "message": f"unsupported server request: {method}",
                    },
                }
            )
            return
        self._write_message({"id": request_id, "result": response})

    def _dynamic_tool_response(self, params: object) -> dict[str, object]:
        try:
            values = _object(params, "dynamic tool request")
            if values.get("namespace") != "codexos":
                raise ValueError("unsupported dynamic tool namespace")
            tool = values.get("tool")
            if not isinstance(tool, str):
                raise ValueError("dynamic tool name must be a string")
            arguments = _object(values.get("arguments"), "dynamic tool arguments")
            result = self._dispatch_tool(tool, arguments)
            return {
                "contentItems": [
                    {"type": "inputText", "text": _format_tool_result(result)}
                ],
                "success": True,
            }
        except (RuntimeError, TypeError, ValueError, binascii.Error) as error:
            return {
                "contentItems": [
                    {"type": "inputText", "text": f"Bridge error: {error}"}
                ],
                "success": False,
            }

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

    def _write_message(self, message: Mapping[str, object]) -> None:
        process = self._process
        if process is None or process.stdin is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        try:
            process.stdin.write(
                json.dumps(message, ensure_ascii=False, separators=(",", ":"))
                + "\n"
            )
            process.stdin.flush()
        except (BrokenPipeError, OSError) as error:
            raise CodexGenerationWorkerError(
                "Codex app-server closed its input unexpectedly"
            ) from error

    def _read_message(self) -> dict[str, object]:
        process = self._process
        if process is None or process.stdout is None:
            raise CodexGenerationWorkerError("Codex app-server is not running")
        try:
            line = process.stdout.readline()
        except (OSError, UnicodeError) as error:
            raise CodexGenerationWorkerError(
                f"failed to read Codex app-server output: {error}"
            ) from error
        if not line:
            code = process.poll()
            diagnostics = self._stderr_text()
            suffix = f": {diagnostics}" if diagnostics else ""
            raise CodexGenerationWorkerError(
                f"Codex app-server exited unexpectedly with status {code}{suffix}"
            )
        try:
            message = json.loads(line)
        except json.JSONDecodeError as error:
            raise CodexGenerationWorkerError(
                "Codex app-server emitted malformed JSON"
            ) from error
        if not isinstance(message, dict):
            raise CodexGenerationWorkerError(
                "Codex app-server message is not an object"
            )
        return message

    def _stderr_text(self) -> str:
        if self._stderr is None:
            return ""
        self._stderr.flush()
        self._stderr.seek(0)
        return self._stderr.read(_MAX_ERROR_OUTPUT).strip()

    def _stop_process(self) -> None:
        process = self._process
        self._process = None
        if process is not None:
            if process.stdin is not None:
                try:
                    process.stdin.close()
                except OSError:
                    pass
            if process.stdout is not None:
                try:
                    process.stdout.close()
                except OSError:
                    pass
            if process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=_PROCESS_EXIT_TIMEOUT_SECONDS)
                except subprocess.TimeoutExpired:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=_PROCESS_EXIT_TIMEOUT_SECONDS)
            else:
                process.wait()
        if self._stderr is not None:
            self._stderr.close()
            self._stderr = None


def _default_auth_file() -> Path:
    configured_home = os.environ.get("CODEX_HOME")
    root = Path(configured_home) if configured_home else Path.home() / ".codex"
    return root / "auth.json"


def _write_isolated_config(path: Path) -> None:
    path.write_text(
        """default_permissions = "codexos-implementor"
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
""",
        encoding="utf-8",
    )


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


def _object(value: object, description: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CodexGenerationWorkerError(f"{description} is not an object")
    return value


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


def _short_json(value: object) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))[
        :_MAX_ERROR_OUTPUT
    ]


def _result_summary(status: str, state: RuntimeState) -> str:
    if status == "completed" and state is RuntimeState.RUNNING:
        return "Codex turn completed; generation is still running."
    if status == "completed" and state is RuntimeState.AWAITING_NEXT_GENERATION:
        return "Codex turn completed; generation completed cooperatively."
    return f"Codex turn {status}; generation state is {state.name}."

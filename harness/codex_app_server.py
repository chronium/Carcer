"""Private Codex app-server process and JSON-RPC mechanics."""

from __future__ import annotations

import json
import os
import shutil
import signal
import subprocess
import tempfile
import threading
from queue import Empty, Full, Queue
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any, TextIO

_PROCESS_EXIT_TIMEOUT_SECONDS = 2.0
MAX_ERROR_OUTPUT = 64 * 1024
_CLOSED = object()


class CodexAppServerError(RuntimeError):
    """The concrete Codex app-server process or protocol failed."""


class CodexAppServer:
    """One fresh app-server process with one isolated home and workspace."""

    def __init__(
        self,
        *,
        executable: str,
        auth_file: Path,
        temporary_prefix: str,
        config_text: str,
    ) -> None:
        self._executable = executable
        self._auth_file = auth_file
        self._temporary_prefix = temporary_prefix
        self._config_text = config_text
        self._temporary: tempfile.TemporaryDirectory[str] | None = None
        self._process: subprocess.Popen[str] | None = None
        self._stderr: TextIO | None = None
        self._next_request_id = 1
        self._request_lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._pending_lock = threading.Lock()
        self._pending: dict[int, Queue[object]] = {}
        self._notifications: Queue[object] = Queue()
        self._server_requests: Queue[object] = Queue()
        self._server_request_handler: (
            Callable[[Mapping[str, object]], None] | None
        ) = None
        self._handler_lock = threading.Lock()
        self._reader: threading.Thread | None = None
        self._request_handler: threading.Thread | None = None
        self._reader_error: CodexAppServerError | None = None
        self._closing = threading.Event()
        self._close_lock = threading.Lock()
        self.workspace: Path | None = None

    @property
    def pid(self) -> int | None:
        process = self._process
        return None if process is None else process.pid

    def __enter__(self) -> CodexAppServer:
        try:
            if not self._auth_file.is_file():
                raise CodexAppServerError(
                    "Codex is not authenticated with a file-based ChatGPT login"
                )
            self._temporary = tempfile.TemporaryDirectory(
                prefix=self._temporary_prefix,
                dir="/tmp",
            )
            root = Path(self._temporary.name)
            codex_home = root / "codex-home"
            self.workspace = root / "workspace"
            process_tmp = root / "tmp"
            codex_home.mkdir()
            self.workspace.mkdir()
            process_tmp.mkdir()
            os.symlink(self._auth_file, codex_home / "auth.json")
            (codex_home / "config.toml").write_text(
                self._config_text,
                encoding="utf-8",
            )
            self._start_process(codex_home, self.workspace, process_tmp)
            self.initialize()
            self.validate_chatgpt_login()
        except OSError as error:
            try:
                self.close()
            except OSError as cleanup_error:
                raise CodexAppServerError(
                    "failed to clean up an app-server after isolated setup "
                    f"failed: {cleanup_error}"
                ) from error
            raise CodexAppServerError(
                f"failed to prepare isolated Codex app-server state: {error}"
            ) from error
        except BaseException:
            self.close()
            raise
        return self

    def __exit__(self, *args: object) -> None:
        self.close()

    def initialize(self) -> None:
        self.request(
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
        self.notify("initialized", None)

    def validate_chatgpt_login(self) -> None:
        response = object_value(
            self.request("account/read", {"refreshToken": False}),
            "account/read response",
        )
        account = response.get("account")
        if not isinstance(account, dict) or account.get("type") != "chatgpt":
            raise CodexAppServerError(
                "Codex is not authenticated using ChatGPT"
            )

    def validate_model(self, model: str, effort: str) -> None:
        cursor: str | None = None
        while True:
            response = object_value(
                self.request("model/list", {"cursor": cursor}),
                "model/list response",
            )
            data = response.get("data")
            if not isinstance(data, list):
                raise CodexAppServerError(
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
                        raise CodexAppServerError(
                            f"model {model!r} does not support reasoning "
                            f"effort {effort!r}"
                        )
                    return
            cursor_value = response.get("nextCursor")
            if cursor_value is None:
                break
            if not isinstance(cursor_value, str):
                raise CodexAppServerError(
                    "model/list returned an invalid cursor"
                )
            cursor = cursor_value
        raise CodexAppServerError(
            f"requested Codex model is unavailable: {model}"
        )

    def start_thread(
        self,
        *,
        model: str,
        permission_profile: str,
        dynamic_tools: list[dict[str, object]],
        require_read_only: bool = False,
    ) -> str:
        workspace = self.workspace
        if workspace is None:
            raise CodexAppServerError("Codex app-server is not running")
        response = object_value(
            self.request(
                "thread/start",
                {
                    "allowProviderModelFallback": False,
                    "approvalPolicy": "never",
                    "approvalsReviewer": "user",
                    "cwd": str(workspace),
                    "dynamicTools": dynamic_tools,
                    "environments": [],
                    "ephemeral": True,
                    "model": model,
                    "permissions": permission_profile,
                    "runtimeWorkspaceRoots": [str(workspace)],
                },
            ),
            "thread/start response",
        )
        thread = object_value(response.get("thread"), "thread/start thread")
        thread_id = thread.get("id")
        if not isinstance(thread_id, str) or not thread_id:
            raise CodexAppServerError(
                "thread/start response is missing a thread ID"
            )
        if thread.get("ephemeral") is not True:
            raise CodexAppServerError(
                "Codex app-server did not create an ephemeral thread"
            )
        if response.get("model") != model:
            raise CodexAppServerError(
                "Codex app-server did not select the requested model"
            )
        profile = response.get("activePermissionProfile")
        if not isinstance(profile, dict) or profile.get("id") != permission_profile:
            raise CodexAppServerError(
                "Codex app-server did not activate the isolated permission profile"
            )
        sandbox = response.get("sandbox")
        if not isinstance(sandbox, dict) or sandbox.get("networkAccess") is not False:
            raise CodexAppServerError(
                "Codex app-server did not disable command network access"
            )
        if sandbox.get("type") == "dangerFullAccess":
            raise CodexAppServerError(
                "Codex app-server selected an unsafe filesystem sandbox"
            )
        if require_read_only and sandbox.get("type") != "readOnly":
            raise CodexAppServerError(
                "Codex app-server did not activate a read-only filesystem sandbox"
            )
        return thread_id

    def start_turn(
        self,
        *,
        thread_id: str,
        prompt: str,
        model: str,
        effort: str,
        permission_profile: str,
    ) -> str:
        response = object_value(
            self.request(
                "turn/start",
                {
                    "approvalPolicy": "never",
                    "approvalsReviewer": "user",
                    "effort": effort,
                    "environments": [],
                    "input": [{"type": "text", "text": prompt}],
                    "model": model,
                    "permissions": permission_profile,
                    "threadId": thread_id,
                },
            ),
            "turn/start response",
        )
        turn = object_value(response.get("turn"), "turn/start turn")
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise CodexAppServerError(
                "turn/start response is missing a turn ID"
            )
        return turn_id

    def request(
        self,
        method: str,
        params: object,
        *,
        timeout_seconds: float | None = None,
    ) -> object:
        with self._request_lock:
            request_id = self._next_request_id
            self._next_request_id += 1
        response_queue: Queue[object] = Queue(maxsize=1)
        with self._pending_lock:
            self._raise_reader_error()
            self._pending[request_id] = response_queue
        try:
            self.write_message(
                {"id": request_id, "method": method, "params": params}
            )
            try:
                response = response_queue.get(timeout=timeout_seconds)
            except Empty as error:
                raise CodexAppServerError(
                    f"Codex app-server {method} timed out"
                ) from error
        finally:
            with self._pending_lock:
                self._pending.pop(request_id, None)
        if isinstance(response, CodexAppServerError):
            raise response
        message = object_value(response, f"{method} response")
        if "error" in message:
            raise CodexAppServerError(
                f"Codex app-server {method} failed: {short_json(message['error'])}"
            )
        if "result" not in message:
            raise CodexAppServerError(
                f"Codex app-server {method} response has no result"
            )
        return message["result"]

    def notify(self, method: str, params: object) -> None:
        message: dict[str, object] = {"method": method}
        if params is not None:
            message["params"] = params
        self.write_message(message)

    def set_server_request_handler(
        self,
        handler: Callable[[Mapping[str, object]], None] | None,
    ) -> None:
        with self._handler_lock:
            self._server_request_handler = handler

    def next_notification(
        self,
        timeout_seconds: float | None = None,
    ) -> dict[str, object]:
        try:
            value = self._notifications.get(timeout=timeout_seconds)
        except Empty as error:
            raise CodexAppServerError(
                "timed out waiting for Codex app-server notification"
            ) from error
        if isinstance(value, CodexAppServerError):
            raise value
        if value is _CLOSED:
            raise CodexAppServerError("Codex app-server is closed")
        return object_value(value, "Codex app-server notification")

    def reject_server_request(self, message: Mapping[str, object]) -> None:
        request_id = message.get("id")
        method = message.get("method")
        if method in {
            "item/commandExecution/requestApproval",
            "item/fileChange/requestApproval",
        }:
            response: object = {"decision": "decline"}
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
            self.write_message(
                {
                    "id": request_id,
                    "error": {
                        "code": -32601,
                        "message": f"unsupported server request: {method}",
                    },
                }
            )
            return
        self.write_message({"id": request_id, "result": response})

    def write_result(self, request_id: object, result: object) -> None:
        self.write_message({"id": request_id, "result": result})

    def write_message(self, message: Mapping[str, object]) -> None:
        process = self._process
        if process is None or process.stdin is None:
            raise CodexAppServerError("Codex app-server is not running")
        try:
            with self._write_lock:
                process.stdin.write(
                    json.dumps(message, ensure_ascii=False, separators=(",", ":"))
                    + "\n"
                )
                process.stdin.flush()
        except (BrokenPipeError, OSError) as error:
            raise CodexAppServerError(
                "Codex app-server closed its input unexpectedly"
            ) from error

    def read_message(self) -> dict[str, object]:
        return self.next_notification()

    def _read_message(self) -> dict[str, object]:
        process = self._process
        if process is None or process.stdout is None:
            raise CodexAppServerError("Codex app-server is not running")
        try:
            line = process.stdout.readline()
        except (OSError, UnicodeError) as error:
            raise CodexAppServerError(
                f"failed to read Codex app-server output: {error}"
            ) from error
        if not line:
            code = process.poll()
            diagnostics = self._stderr_text()
            suffix = f": {diagnostics}" if diagnostics else ""
            raise CodexAppServerError(
                f"Codex app-server exited unexpectedly with status {code}{suffix}"
            )
        try:
            message = json.loads(line)
        except json.JSONDecodeError as error:
            raise CodexAppServerError(
                "Codex app-server emitted malformed JSON"
            ) from error
        if not isinstance(message, dict):
            raise CodexAppServerError(
                "Codex app-server message is not an object"
            )
        return message

    def close(self) -> None:
        with self._close_lock:
            self._close()

    def _close(self) -> None:
        self._closing.set()
        process = self._process
        self._process = None
        if process is not None:
            if process.stdin is not None:
                try:
                    process.stdin.close()
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
            if process.stdout is not None:
                try:
                    process.stdout.close()
                except OSError:
                    pass
        self._notifications.put(_CLOSED)
        self._server_requests.put(_CLOSED)
        with self._pending_lock:
            pending = list(self._pending.values())
        for response in pending:
            try:
                response.put_nowait(
                    CodexAppServerError("Codex app-server is closed")
                )
            except Full:
                pass
        for thread in (self._reader, self._request_handler):
            if thread is not None and thread is not threading.current_thread():
                thread.join(timeout=_PROCESS_EXIT_TIMEOUT_SECONDS)
        self._reader = None
        self._request_handler = None
        if self._stderr is not None:
            self._stderr.close()
            self._stderr = None
        if self._temporary is not None:
            self._temporary.cleanup()
            self._temporary = None
        self.workspace = None

    def _start_process(
        self,
        codex_home: Path,
        workspace: Path,
        process_tmp: Path,
    ) -> None:
        executable = shutil.which(self._executable)
        if executable is None:
            raise CodexAppServerError(
                f"Codex executable not found: {self._executable}"
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
            raise CodexAppServerError(
                f"failed to start Codex app-server: {error}"
            ) from error
        self._reader = threading.Thread(
            target=self._read_loop,
            name="codex-app-server-reader",
            daemon=True,
        )
        self._request_handler = threading.Thread(
            target=self._server_request_loop,
            name="codex-app-server-requests",
            daemon=True,
        )
        self._reader.start()
        self._request_handler.start()

    def _read_loop(self) -> None:
        try:
            while not self._closing.is_set():
                message = self._read_message()
                request_id = message.get("id")
                if request_id is not None and "method" in message:
                    self._server_requests.put(message)
                elif request_id is not None:
                    if type(request_id) is not int:
                        raise CodexAppServerError(
                            "Codex app-server response ID is not an integer"
                        )
                    with self._pending_lock:
                        pending = self._pending.get(request_id)
                    if pending is None:
                        raise CodexAppServerError(
                            "Codex app-server response ID does not match a request"
                        )
                    try:
                        pending.put_nowait(message)
                    except Full as error:
                        raise CodexAppServerError(
                            "Codex app-server sent a duplicate response"
                        ) from error
                else:
                    self._notifications.put(message)
        except CodexAppServerError as error:
            if not self._closing.is_set():
                self._fail_reader(error)

    def _server_request_loop(self) -> None:
        while not self._closing.is_set():
            message = self._server_requests.get()
            if message is _CLOSED:
                return
            try:
                values = object_value(message, "server request")
                with self._handler_lock:
                    handler = self._server_request_handler
                if handler is None:
                    self.reject_server_request(values)
                else:
                    handler(values)
            except CodexAppServerError as error:
                if not self._closing.is_set():
                    self._fail_reader(error)
                return

    def _fail_reader(self, error: CodexAppServerError) -> None:
        self._reader_error = error
        with self._pending_lock:
            pending = list(self._pending.values())
        for response in pending:
            try:
                response.put_nowait(error)
            except Full:
                pass
        self._notifications.put(error)

    def _raise_reader_error(self) -> None:
        if self._reader_error is not None:
            raise self._reader_error

    def _stderr_text(self) -> str:
        if self._stderr is None:
            return ""
        self._stderr.flush()
        self._stderr.seek(0)
        return self._stderr.read(MAX_ERROR_OUTPUT).strip()


def default_auth_file() -> Path:
    configured_home = os.environ.get("CODEX_HOME")
    root = Path(configured_home) if configured_home else Path.home() / ".codex"
    return root / "auth.json"


def object_value(value: object, description: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CodexAppServerError(f"{description} is not an object")
    return value


def short_json(value: object) -> str:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))[
        :MAX_ERROR_OUTPUT
    ]


def token_usage_from_notification(
    params: object,
    thread_id: str,
    turn_id: str,
) -> tuple[int, int]:
    """Read exact per-response usage from the installed v2 notification."""
    values = object_value(params, "thread/tokenUsage/updated notification")
    if values.get("threadId") != thread_id or values.get("turnId") != turn_id:
        raise CodexAppServerError(
            "thread/tokenUsage/updated has the wrong thread or turn ID"
        )
    token_usage = object_value(values.get("tokenUsage"), "token usage")
    last = object_value(token_usage.get("last"), "last token usage")
    input_tokens = last.get("inputTokens")
    output_tokens = last.get("outputTokens")
    if (
        type(input_tokens) is not int
        or input_tokens < 0
        or type(output_tokens) is not int
        or output_tokens < 0
    ):
        raise CodexAppServerError("token usage has invalid counts")
    return input_tokens, output_tokens

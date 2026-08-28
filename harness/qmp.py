"""Minimal synchronous client for QEMU's machine protocol."""

from __future__ import annotations

import json
import socket
import time
from os import PathLike
from typing import TextIO

_TIMEOUT_SECONDS = 5.0


class QmpError(RuntimeError):
    """A QMP protocol or command error."""


class QmpClient:
    """Control one QEMU instance through a Unix-domain QMP socket."""

    def __init__(self, socket_path: str | PathLike[str]) -> None:
        self._socket_path = str(socket_path)
        self._socket: socket.socket | None = None
        self._stream: TextIO | None = None
        self._next_id = 1

    def connect(self) -> None:
        """Connect, validate QEMU's greeting, and negotiate QMP capabilities."""
        if self._socket is not None:
            raise QmpError("QMP client is already connected")

        deadline = time.monotonic() + _TIMEOUT_SECONDS
        while True:
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                connection.connect(self._socket_path)
            except (FileNotFoundError, ConnectionRefusedError) as error:
                connection.close()
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise QmpError(
                        f"timed out connecting to QMP socket {self._socket_path}"
                    ) from error
                time.sleep(min(0.01, remaining))
                continue
            except BaseException:
                connection.close()
                raise

            connection.settimeout(_TIMEOUT_SECONDS)
            self._socket = connection
            try:
                self._stream = connection.makefile("rw", encoding="utf-8", newline="\n")
                self._validate_greeting(self._read_message())
                self._execute("qmp_capabilities")
            except BaseException:
                self.close()
                raise
            return

    def query_status(self) -> str:
        result = self._execute("query-status")
        if not isinstance(result, dict) or not isinstance(result.get("status"), str):
            raise QmpError("query-status returned an invalid response")
        return result["status"]

    def stop(self) -> None:
        self._execute("stop")

    def cont(self) -> None:
        self._execute("cont")

    def quit(self) -> None:
        self._execute("quit")

    def close(self) -> None:
        stream, connection = self._stream, self._socket
        self._stream = None
        self._socket = None
        try:
            if stream is not None:
                stream.close()
        finally:
            if connection is not None:
                connection.close()

    def __enter__(self) -> QmpClient:
        self.connect()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    @staticmethod
    def _validate_greeting(greeting: dict[str, object]) -> None:
        qmp = greeting.get("QMP")
        if (
            not isinstance(qmp, dict)
            or not isinstance(qmp.get("version"), dict)
            or not isinstance(qmp.get("capabilities"), list)
        ):
            raise QmpError("invalid QMP greeting")

    def _execute(self, command: str) -> object:
        if self._stream is None:
            raise QmpError("QMP client is not connected")

        request_id = self._next_id
        self._next_id += 1
        request = {"execute": command, "id": request_id}
        self._stream.write(json.dumps(request, separators=(",", ":")) + "\r\n")
        self._stream.flush()

        while True:
            response = self._read_message()
            if "event" in response:
                continue
            if response.get("id") != request_id:
                raise QmpError(
                    f"{command} received response for unexpected id {response.get('id')!r}"
                )

            error = response.get("error")
            if error is not None:
                if not isinstance(error, dict):
                    raise QmpError(f"{command} returned an invalid error response")
                error_class = error.get("class", "unknown error")
                description = error.get("desc", "no description")
                raise QmpError(f"{command} failed: {error_class}: {description}")
            if "return" not in response:
                raise QmpError(f"{command} returned no result")
            return response["return"]

    def _read_message(self) -> dict[str, object]:
        if self._stream is None:
            raise QmpError("QMP client is not connected")

        line = self._stream.readline()
        if not line:
            raise QmpError("QMP connection closed")
        try:
            message = json.loads(line)
        except json.JSONDecodeError as error:
            raise QmpError("QMP returned invalid JSON") from error
        if not isinstance(message, dict):
            raise QmpError("QMP returned a non-object message")
        return message

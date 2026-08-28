"""Raw synchronous transport for a QEMU serial Unix socket."""

from __future__ import annotations

import socket
import time
from os import PathLike

_CONNECT_TIMEOUT_SECONDS = 5.0


class SerialError(RuntimeError):
    """A serial connection state or closure error."""


class SerialConnection:
    """Exchange raw bytes through one QEMU serial Unix socket."""

    def __init__(self, socket_path: str | PathLike[str]) -> None:
        self._socket_path = str(socket_path)
        self._socket: socket.socket | None = None

    def connect(self) -> None:
        if self._socket is not None:
            raise SerialError("serial connection is already connected")

        deadline = time.monotonic() + _CONNECT_TIMEOUT_SECONDS
        while True:
            connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                connection.connect(self._socket_path)
            except (FileNotFoundError, ConnectionRefusedError) as error:
                connection.close()
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise SerialError(
                        f"timed out connecting to serial socket {self._socket_path}"
                    ) from error
                time.sleep(min(0.01, remaining))
                continue
            except BaseException:
                connection.close()
                raise

            self._socket = connection
            return

    def write(self, data: bytes) -> None:
        connection = self._require_socket()
        try:
            connection.sendall(data)
        except ConnectionError as error:
            self.close()
            raise SerialError("serial connection closed") from error

    def read(self, max_bytes: int, timeout_seconds: float) -> bytes:
        if max_bytes <= 0:
            raise ValueError("max_bytes must be positive")

        connection = self._require_socket()
        previous_timeout = connection.gettimeout()
        connection.settimeout(timeout_seconds)
        try:
            data = connection.recv(max_bytes)
        except ConnectionError as error:
            self.close()
            raise SerialError("serial connection closed") from error
        finally:
            if self._socket is connection:
                connection.settimeout(previous_timeout)

        if not data:
            self.close()
            raise SerialError("serial connection closed")
        return data

    def close(self) -> None:
        connection = self._socket
        self._socket = None
        if connection is not None:
            connection.close()

    def __enter__(self) -> SerialConnection:
        self.connect()
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def _require_socket(self) -> socket.socket:
        if self._socket is None:
            raise SerialError("serial connection is not connected")
        return self._socket

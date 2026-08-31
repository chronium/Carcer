"""Raw synchronous transport for a QEMU serial Unix socket."""

from __future__ import annotations

import select
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

    def pump(
        self,
        max_read_bytes: int,
        outgoing: memoryview | None,
        timeout_seconds: float,
    ) -> tuple[bytes | None, int]:
        """Perform one bounded duplex I/O opportunity on the serial socket."""
        if max_read_bytes <= 0:
            raise ValueError("max_read_bytes must be positive")
        if timeout_seconds < 0:
            raise ValueError("serial pump timeout must not be negative")

        connection = self._require_socket()
        write_sockets = [connection] if outgoing else []
        try:
            readable, writable, _ = select.select(
                [connection],
                write_sockets,
                [],
                timeout_seconds,
            )
            incoming: bytes | None = None
            if readable:
                try:
                    incoming = connection.recv(max_read_bytes, socket.MSG_DONTWAIT)
                except BlockingIOError:
                    incoming = None
                if incoming == b"":
                    self.close()
                    raise SerialError("serial connection closed")

            sent = 0
            if writable and outgoing:
                try:
                    sent = connection.send(outgoing, socket.MSG_DONTWAIT)
                except BlockingIOError:
                    sent = 0
                if sent == 0:
                    self.close()
                    raise SerialError("serial connection closed")
            return incoming, sent
        except (ConnectionError, OSError, ValueError) as error:
            self.close()
            raise SerialError("serial connection closed") from error

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

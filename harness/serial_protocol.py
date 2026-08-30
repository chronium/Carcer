"""Single-reader dispatcher for one CodexOS guest serial protocol."""

from __future__ import annotations

import threading
import time

from .framing import MAGIC, Frame, FramingError, encode_frame, extract_frame
from .host_service_protocol import (
    HOST_SERVICE_REQUEST,
    HostServiceHandler,
    HostServiceProtocolError,
    create_host_service_response,
    decode_host_service_request,
)
from .serial import SerialConnection, SerialError

READY_MARKER = b"CODEXOS-SEED-READY\n"
_MAX_DIAGNOSTIC_BYTES = 4 * 1024
_READER_POLL_SECONDS = 0.5
_CLOSE_TIMEOUT_SECONDS = 5.0
_MAX_HOST_DIAGNOSTIC_BYTES = 4 * 1024


class SerialProtocolDispatcher:
    """Own all reads and route frames for one running guest serial link."""

    def __init__(
        self,
        serial: SerialConnection,
        *,
        startup_host_services: HostServiceHandler | None = None,
        host_services: HostServiceHandler | None = None,
    ) -> None:
        self._serial = serial
        self._startup_host_services = startup_host_services
        self._host_services = host_services
        self._condition = threading.Condition()
        self._exchange_lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._reader: threading.Thread | None = None
        self._started = False
        self._ready = False
        self._closing = False
        self._failure: BaseException | None = None
        self._pending_response = False
        self._response: Frame | None = None
        self._host_service_active = False
        self._host_service_window = 0
        self._startup_buffer = bytearray()
        self._frame_buffer = bytearray()
        self._diagnostic = bytearray()

    @property
    def startup_diagnostic(self) -> bytes:
        with self._condition:
            return bytes(self._diagnostic + self._startup_buffer)[
                :_MAX_DIAGNOSTIC_BYTES
            ]

    @property
    def reader_alive(self) -> bool:
        reader = self._reader
        return reader is not None and reader.is_alive()

    def start(self) -> None:
        """Start reading startup output and looking for canonical READY."""
        self._start(ready=False)

    def start_ready(self) -> None:
        """Start framed dispatch for a connection whose READY was consumed."""
        self._start(ready=True)

    def wait_until_ready(self, timeout_seconds: float) -> None:
        if timeout_seconds <= 0:
            raise ValueError("guest readiness timeout must be positive")
        deadline = time.monotonic() + timeout_seconds
        with self._condition:
            if not self._started:
                raise RuntimeError("serial protocol dispatcher has not started")
            while not self._ready and self._failure is None:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("timed out waiting for CODEXOS-SEED-READY")
                self._condition.wait(remaining)
            if not self._ready:
                self._raise_failure_locked()

    def exchange(self, request: Frame, timeout_seconds: float) -> Frame:
        """Write one harness request and wait for its routed guest response."""
        if timeout_seconds <= 0:
            raise ValueError("response timeout must be positive")
        with self._exchange_lock:
            with self._condition:
                if not self._ready:
                    raise RuntimeError("guest serial protocol is not ready")
                self._raise_failure_locked()
                if self._closing:
                    raise SerialError("serial protocol dispatcher is closed")
                self._pending_response = True
                self._response = None
                host_service_window = self._host_service_window
            try:
                self._write(encode_frame(request))
                deadline = time.monotonic() + timeout_seconds
                with self._condition:
                    while self._response is None and self._failure is None:
                        if self._host_service_active:
                            self._condition.wait()
                            continue
                        if self._host_service_window != host_service_window:
                            host_service_window = self._host_service_window
                            deadline = time.monotonic() + timeout_seconds
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            raise TimeoutError("timed out waiting for tool response")
                        self._condition.wait(remaining)
                    self._raise_failure_locked()
                    if self._response is None:
                        raise RuntimeError("serial response routing failed")
                    return self._response
            finally:
                with self._condition:
                    self._pending_response = False
                    self._response = None

    def close(self) -> None:
        """Stop the sole reader and close its serial connection."""
        with self._condition:
            if self._closing:
                reader = self._reader
            else:
                self._closing = True
                if self._failure is None:
                    self._failure = SerialError("serial protocol dispatcher is closed")
                self._condition.notify_all()
                reader = self._reader
        self._serial.close()
        if reader is not None and reader is not threading.current_thread():
            reader.join(_CLOSE_TIMEOUT_SECONDS)
            if reader.is_alive():
                raise SerialError("serial protocol reader did not stop")

    def _start(self, *, ready: bool) -> None:
        with self._condition:
            if self._started:
                raise RuntimeError("serial protocol dispatcher has already started")
            if self._closing:
                raise RuntimeError("serial protocol dispatcher is closed")
            self._started = True
            self._ready = ready
            self._reader = threading.Thread(
                target=self._read_loop,
                name="codexos-serial-protocol",
            )
            self._reader.start()

    def _read_loop(self) -> None:
        try:
            while True:
                with self._condition:
                    if self._closing:
                        return
                try:
                    chunk = self._serial.read(4096, _READER_POLL_SECONDS)
                except TimeoutError:
                    continue
                if self._ready:
                    self._frame_buffer.extend(chunk)
                    self._consume_frames()
                else:
                    self._startup_buffer.extend(chunk)
                    self._consume_startup()
        except BaseException as error:
            with self._condition:
                if not self._closing:
                    self._failure = error
                    self._condition.notify_all()

    def _consume_startup(self) -> None:
        while self._startup_buffer:
            if self._startup_buffer.startswith(READY_MARKER):
                del self._startup_buffer[: len(READY_MARKER)]
                with self._condition:
                    self._ready = True
                    self._condition.notify_all()
                if self._startup_buffer:
                    self._frame_buffer.extend(self._startup_buffer)
                    self._startup_buffer.clear()
                    self._consume_frames()
                return
            if (
                self._startup_host_services is not None
                and self._startup_buffer.startswith(MAGIC)
            ):
                frame = extract_frame(self._startup_buffer)
                if frame is None:
                    return
                if frame.message_type != HOST_SERVICE_REQUEST:
                    raise FramingError(
                        "received a non-host-service frame before CODEXOS-SEED-READY"
                    )
                self._dispatch_host_service(frame, self._startup_host_services)
                continue
            if READY_MARKER.startswith(self._startup_buffer) or (
                self._startup_host_services is not None
                and MAGIC.startswith(self._startup_buffer)
            ):
                return
            if len(self._diagnostic) < _MAX_DIAGNOSTIC_BYTES:
                self._diagnostic.append(self._startup_buffer[0])
            del self._startup_buffer[0]

    def _consume_frames(self) -> None:
        while self._frame_buffer:
            frame = extract_frame(self._frame_buffer)
            if frame is None:
                return
            if frame.message_type == HOST_SERVICE_REQUEST:
                self._dispatch_host_service(frame, self._host_services)
                continue
            with self._condition:
                if not self._pending_response or self._response is not None:
                    raise FramingError(
                        "received an unexpected guest tool-protocol response"
                    )
                self._response = frame
                self._condition.notify_all()

    def _dispatch_host_service(
        self,
        frame: Frame,
        handler: HostServiceHandler | None,
    ) -> None:
        try:
            with self._condition:
                self._host_service_active = True
                self._condition.notify_all()
            try:
                request = decode_host_service_request(frame)
            except HostServiceProtocolError as error:
                if frame.request_id == 0:
                    raise
                response = create_host_service_response(
                    frame.request_id,
                    1,
                    _bounded_diagnostic(str(error)),
                )
            else:
                if handler is None:
                    response = create_host_service_response(
                        request.request_id,
                        1,
                        b"host services are not available",
                    )
                else:
                    try:
                        response = handler.handle_request(request)
                    except Exception as error:
                        response = create_host_service_response(
                            request.request_id,
                            2,
                            _bounded_diagnostic(
                                "trusted host-service failure: " + str(error)
                            ),
                        )
            self._write(encode_frame(response))
        finally:
            with self._condition:
                self._host_service_active = False
                self._host_service_window += 1
                self._condition.notify_all()

    def _write(self, data: bytes) -> None:
        with self._write_lock:
            self._serial.write(data)

    def _raise_failure_locked(self) -> None:
        if self._failure is not None:
            raise self._failure


def _bounded_diagnostic(message: str) -> bytes:
    return message.encode("utf-8", errors="replace")[:_MAX_HOST_DIAGNOSTIC_BYTES]

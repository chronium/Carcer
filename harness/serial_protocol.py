"""Single-reader dispatcher for one CodexOS guest serial protocol."""

from __future__ import annotations

import threading
import time
from collections import deque
from collections.abc import Callable, Mapping
from dataclasses import dataclass

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
_READER_POLL_SECONDS = 0.01
_CLOSE_TIMEOUT_SECONDS = 5.0
_MAX_HOST_DIAGNOSTIC_BYTES = 4 * 1024
_WRITE_CHUNK_BYTES = 16 * 1024
_WRITE_STALL_TIMEOUT_SECONDS = 5.0
_WRITE_PROGRESS_BYTES = 64 * 1024


@dataclass(slots=True)
class _QueuedWrite:
    data: bytes
    kind: str
    request_id: int
    scope: str | None = None
    offset: int = 0
    next_progress: int = _WRITE_PROGRESS_BYTES
    stall_deadline: float | None = None
    complete: bool = False
    terminal_phase: str | None = None


class SerialProtocolDispatcher:
    """Own all reads and route frames for one running guest serial link."""

    def __init__(
        self,
        serial: SerialConnection,
        *,
        startup_host_services: HostServiceHandler | None = None,
        background_host_services: HostServiceHandler | None = None,
        exchange_host_services: HostServiceHandler | None = None,
        event_recorder: Callable[[str, Mapping[str, object]], None] | None = None,
    ) -> None:
        self._serial = serial
        self._startup_host_services = startup_host_services
        self._background_host_services = background_host_services
        self._exchange_host_services = exchange_host_services
        self._event_recorder = event_recorder
        self._condition = threading.Condition()
        self._exchange_lock = threading.Lock()
        self._reader: threading.Thread | None = None
        self._started = False
        self._ready = False
        self._closing = False
        self._failure: BaseException | None = None
        self._pending_response = False
        self._response: Frame | None = None
        self._host_service_active = 0
        self._host_service_window = 0
        self._startup_buffer = bytearray()
        self._frame_buffer = bytearray()
        self._diagnostic = bytearray()
        self._writes: deque[_QueuedWrite] = deque()

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
                queued = self._queue_write(
                    encode_frame(request),
                    kind="tool_request",
                    request_id=request.request_id,
                )
                self._wait_for_write(queued)
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
                if self._writes:
                    self._record_write_terminal(
                        self._writes[0],
                        "write_cancelled",
                    )
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
                    queued = self._writes[0] if self._writes else None
                    if queued is not None and queued.stall_deadline is None:
                        queued.stall_deadline = (
                            time.monotonic() + _WRITE_STALL_TIMEOUT_SECONDS
                        )
                        self._record_write_event(queued, "write_started")
                    outgoing = (
                        memoryview(queued.data)[
                            queued.offset : queued.offset + _WRITE_CHUNK_BYTES
                        ]
                        if queued is not None
                        else None
                    )
                chunk, sent = self._serial.pump(
                    4096,
                    outgoing,
                    _READER_POLL_SECONDS,
                )
                if chunk is not None:
                    if self._ready:
                        self._frame_buffer.extend(chunk)
                        self._consume_frames()
                    else:
                        self._startup_buffer.extend(chunk)
                        self._consume_startup()
                if queued is not None:
                    self._advance_write(queued, sent)
        except BaseException as error:
            with self._condition:
                if not self._closing:
                    self._fail_active_write_locked(error)
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
                self._dispatch_host_service(
                    frame,
                    self._startup_host_services,
                    "startup",
                )
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
                with self._condition:
                    exchange = self._pending_response
                    handler = (
                        self._exchange_host_services
                        if exchange
                        else self._background_host_services
                    )
                self._dispatch_host_service(
                    frame,
                    handler,
                    "exchange" if exchange else "background",
                )
                continue
            with self._condition:
                if not self._pending_response or self._response is not None:
                    raise FramingError(
                        "received an unexpected guest tool-protocol response"
                    )
                self._pending_response = False
                self._response = frame
                self._record(
                    "serial_tool_response_received",
                    {
                        "request_id": frame.request_id,
                        "response_bytes": len(frame.payload),
                    },
                )
                self._condition.notify_all()

    def _dispatch_host_service(
        self,
        frame: Frame,
        handler: HostServiceHandler | None,
        scope: str,
    ) -> None:
        response_queued = False
        try:
            with self._condition:
                self._host_service_active += 1
                self._record(
                    "serial_host_service_request_received",
                    {
                        "request_id": frame.request_id,
                        "request_bytes": len(frame.payload),
                        "scope": scope,
                    },
                )
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
            encoded = encode_frame(response)
            self._record(
                "serial_host_service_response_prepared",
                {
                    "request_id": response.request_id,
                    "response_bytes": len(encoded),
                    "scope": scope,
                },
            )
            self._queue_write(
                encoded,
                kind="host_response",
                request_id=response.request_id,
                scope=scope,
            )
            response_queued = True
        finally:
            if not response_queued:
                with self._condition:
                    self._finish_host_service_locked()

    def _queue_write(
        self,
        data: bytes,
        *,
        kind: str,
        request_id: int,
        scope: str | None = None,
    ) -> _QueuedWrite:
        queued = _QueuedWrite(data, kind, request_id, scope)
        with self._condition:
            self._raise_failure_locked()
            if self._closing:
                raise SerialError("serial protocol dispatcher is closed")
            self._writes.append(queued)
            self._condition.notify_all()
        return queued

    def _wait_for_write(self, queued: _QueuedWrite) -> None:
        with self._condition:
            while not queued.complete and self._failure is None:
                self._condition.wait()
            self._raise_failure_locked()
            if not queued.complete:
                raise SerialError("serial protocol write did not complete")

    def _advance_write(self, queued: _QueuedWrite, sent: int) -> None:
        now = time.monotonic()
        with self._condition:
            if not self._writes or self._writes[0] is not queued:
                return
            if sent:
                queued.offset += sent
                queued.stall_deadline = now + _WRITE_STALL_TIMEOUT_SECONDS
                if queued.kind == "host_response":
                    while queued.offset >= queued.next_progress:
                        self._record_write_event(queued, "write_progress")
                        queued.next_progress += _WRITE_PROGRESS_BYTES
            elif queued.stall_deadline is not None and now >= queued.stall_deadline:
                self._record_write_terminal(queued, "write_timed_out")
                raise TimeoutError("timed out writing serial protocol frame")
            if queued.offset != len(queued.data):
                return
            self._writes.popleft()
            queued.complete = True
            self._record_write_terminal(queued, "write_completed")
            if queued.kind == "host_response":
                self._finish_host_service_locked()
            self._condition.notify_all()

    def _fail_active_write_locked(self, error: BaseException) -> None:
        if self._writes:
            self._record_write_terminal(
                self._writes[0],
                "write_failed",
                error=type(error).__name__,
            )
        if self._host_service_active:
            self._host_service_window += self._host_service_active
            self._host_service_active = 0
            self._condition.notify_all()

    def _finish_host_service_locked(self) -> None:
        if self._host_service_active <= 0:
            return
        self._host_service_active -= 1
        self._host_service_window += 1
        self._condition.notify_all()

    def _record_write_event(
        self,
        queued: _QueuedWrite,
        phase: str,
        *,
        error: str | None = None,
    ) -> None:
        data: dict[str, object] = {
            "request_id": queued.request_id,
            "phase": phase,
            "bytes_sent": queued.offset,
            "total_bytes": len(queued.data),
            "write_kind": queued.kind,
        }
        if queued.scope is not None:
            data["scope"] = queued.scope
        if error is not None:
            data["error"] = error
        self._record("serial_protocol_write", data)

    def _record_write_terminal(
        self,
        queued: _QueuedWrite,
        phase: str,
        *,
        error: str | None = None,
    ) -> None:
        if queued.terminal_phase is not None:
            return
        queued.terminal_phase = phase
        self._record_write_event(queued, phase, error=error)

    def _record(self, event: str, data: Mapping[str, object]) -> None:
        recorder = self._event_recorder
        if recorder is None:
            return
        try:
            recorder(event, data)
        except Exception:
            # Transport behavior must not depend on passive observability.
            pass

    def _raise_failure_locked(self) -> None:
        if self._failure is not None:
            raise self._failure


def _bounded_diagnostic(message: str) -> bytes:
    return message.encode("utf-8", errors="replace")[:_MAX_HOST_DIAGNOSTIC_BYTES]

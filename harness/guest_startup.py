"""Canonical CodexOS guest readiness observation."""

from __future__ import annotations

import time

from .framing import MAGIC, FramingError, encode_frame, read_frame
from .host_service_protocol import (
    HostServiceHandler,
    HostServiceProtocolError,
    decode_host_service_request,
)
from .serial import SerialConnection, SerialError

READY_MARKER = b"CODEXOS-SEED-READY\n"
_MAX_DIAGNOSTIC_BYTES = 4 * 1024


class GuestReadyError(RuntimeError):
    """The guest failed before its canonical development protocol was ready."""

    def __init__(self, reason: str, serial_output: bytes) -> None:
        self.reason = reason
        self.serial_output = serial_output[:_MAX_DIAGNOSTIC_BYTES]
        message = reason
        if self.serial_output:
            message += (
                "\nSerial before failure:\n"
                + escape_diagnostic_bytes(self.serial_output)
            )
        super().__init__(message)


def wait_for_ready(
    serial: SerialConnection,
    timeout_seconds: float,
    provided_assets: HostServiceHandler | None = None,
) -> None:
    """Wait for READY and service configured asset requests during startup."""
    if timeout_seconds <= 0:
        raise ValueError("guest readiness timeout must be positive")

    deadline = time.monotonic() + timeout_seconds
    diagnostic = bytearray()
    undecided = bytearray()
    tokens = (READY_MARKER,) if provided_assets is None else (READY_MARKER, MAGIC)
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise GuestReadyError(
                "timed out waiting for CODEXOS-SEED-READY",
                bytes(diagnostic + undecided),
            )
        try:
            undecided.extend(serial.read(1, min(0.5, remaining)))
        except TimeoutError:
            continue
        except SerialError as error:
            raise GuestReadyError(
                "serial connection closed before CODEXOS-SEED-READY",
                bytes(diagnostic + undecided),
            ) from error

        while undecided:
            if undecided == READY_MARKER:
                return
            if provided_assets is not None and undecided == MAGIC:
                _service_startup_request(
                    serial,
                    provided_assets,
                    deadline,
                    diagnostic,
                )
                undecided.clear()
                break
            if any(token.startswith(undecided) for token in tokens):
                break
            if len(diagnostic) < _MAX_DIAGNOSTIC_BYTES:
                diagnostic.append(undecided[0])
            del undecided[0]


def _service_startup_request(
    serial: SerialConnection,
    provided_assets: HostServiceHandler,
    deadline: float,
    diagnostic: bytearray,
) -> None:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise GuestReadyError(
            "timed out reading a provided-asset request before READY",
            bytes(diagnostic),
        )
    try:
        frame = read_frame(_MagicPrefixedSerial(serial), remaining)
        request = decode_host_service_request(frame)
        serial.write(encode_frame(provided_assets.handle_request(request)))
    except (FramingError, HostServiceProtocolError, OSError, SerialError) as error:
        raise GuestReadyError(
            "invalid provided-asset request before CODEXOS-SEED-READY: "
            + str(error),
            bytes(diagnostic),
        ) from error


class _MagicPrefixedSerial:
    """Replay consumed frame magic before continuing from the real serial link."""

    def __init__(self, serial: SerialConnection) -> None:
        self._serial = serial
        self._prefix = bytearray(MAGIC)

    def read(self, max_bytes: int, timeout_seconds: float) -> bytes:
        if self._prefix:
            count = min(max_bytes, len(self._prefix))
            result = bytes(self._prefix[:count])
            del self._prefix[:count]
            return result
        return self._serial.read(max_bytes, timeout_seconds)


def escape_diagnostic_bytes(data: bytes) -> str:
    """Render untrusted serial bytes without terminal control effects."""
    rendered: list[str] = []
    for value in data[:_MAX_DIAGNOSTIC_BYTES]:
        if value == 0x0A:
            rendered.append("\\n")
        elif value == 0x0D:
            rendered.append("\\r")
        elif value == 0x09:
            rendered.append("\\t")
        elif 0x20 <= value <= 0x7E:
            rendered.append(chr(value))
        else:
            rendered.append(f"\\x{value:02x}")
    if len(data) > _MAX_DIAGNOSTIC_BYTES:
        rendered.append("...[truncated]")
    return "".join(rendered)

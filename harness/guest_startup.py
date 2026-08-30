"""Canonical CodexOS guest readiness observation."""

from __future__ import annotations

import time

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
) -> None:
    """Wait for READY while retaining only bounded diagnostic serial bytes."""
    if timeout_seconds <= 0:
        raise ValueError("guest readiness timeout must be positive")

    deadline = time.monotonic() + timeout_seconds
    diagnostic = bytearray()
    marker_tail = bytearray()
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise GuestReadyError(
                "timed out waiting for CODEXOS-SEED-READY",
                bytes(diagnostic),
            )
        try:
            chunk = serial.read(4096, min(0.5, remaining))
        except TimeoutError:
            continue
        except SerialError as error:
            raise GuestReadyError(
                "serial connection closed before CODEXOS-SEED-READY",
                bytes(diagnostic),
            ) from error

        if len(diagnostic) < _MAX_DIAGNOSTIC_BYTES:
            available = _MAX_DIAGNOSTIC_BYTES - len(diagnostic)
            diagnostic.extend(chunk[:available])
        combined = bytes(marker_tail) + chunk
        if READY_MARKER in combined:
            return
        tail_length = len(READY_MARKER) - 1
        marker_tail[:] = combined[-tail_length:]


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

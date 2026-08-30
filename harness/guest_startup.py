"""Canonical CodexOS guest readiness observation."""

from __future__ import annotations

from .framing import FramingError
from .serial import SerialError
from .serial_protocol import READY_MARKER, SerialProtocolDispatcher

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
    dispatcher: SerialProtocolDispatcher,
    timeout_seconds: float,
) -> None:
    """Wait for canonical READY through the connection's sole reader."""
    dispatcher.start()
    try:
        dispatcher.wait_until_ready(timeout_seconds)
    except TimeoutError as error:
        raise GuestReadyError(
            "timed out waiting for CODEXOS-SEED-READY",
            dispatcher.startup_diagnostic,
        ) from error
    except SerialError as error:
        raise GuestReadyError(
            "serial connection closed before CODEXOS-SEED-READY",
            dispatcher.startup_diagnostic,
        ) from error
    except (FramingError, OSError) as error:
        raise GuestReadyError(
            "invalid provided-asset request before CODEXOS-SEED-READY: "
            + str(error),
            dispatcher.startup_diagnostic,
        ) from error


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

"""Binary framing for the CodexOS serial byte stream."""

from __future__ import annotations

import struct
import time
from dataclasses import dataclass

from .serial import SerialConnection, SerialError

MAGIC = b"CXOS"
PROTOCOL_VERSION = 1
MAX_PAYLOAD_SIZE = 16 * 1024 * 1024

_HEADER = struct.Struct("<4sHHII")


class FramingError(RuntimeError):
    """A malformed or incomplete CodexOS frame."""


@dataclass(frozen=True, slots=True)
class Frame:
    message_type: int
    request_id: int
    payload: bytes


def encode_frame(frame: Frame) -> bytes:
    """Encode one frame into its exact wire representation."""
    if not 0 <= frame.message_type <= 0xFFFF:
        raise ValueError("message type must be an unsigned 16-bit value")
    if not 0 <= frame.request_id <= 0xFFFFFFFF:
        raise ValueError("request ID must be an unsigned 32-bit value")
    if len(frame.payload) > MAX_PAYLOAD_SIZE:
        raise FramingError("payload exceeds the 16 MiB version 1 limit")

    header = _HEADER.pack(
        MAGIC,
        PROTOCOL_VERSION,
        frame.message_type,
        frame.request_id,
        len(frame.payload),
    )
    return header + frame.payload


def read_frame(
    connection: SerialConnection,
    timeout_seconds: float = 5.0,
) -> Frame:
    """Read and validate one complete frame from a serial connection."""
    deadline = time.monotonic() + timeout_seconds
    header = _read_exact(connection, _HEADER.size, deadline, "header")
    magic, version, message_type, request_id, payload_length = _HEADER.unpack(header)

    if magic != MAGIC:
        raise FramingError(f"invalid frame magic {magic!r}")
    if version != PROTOCOL_VERSION:
        raise FramingError(f"unsupported protocol version {version}")
    if payload_length > MAX_PAYLOAD_SIZE:
        raise FramingError(
            f"payload length {payload_length} exceeds the 16 MiB version 1 limit"
        )

    payload = _read_exact(connection, payload_length, deadline, "payload")
    return Frame(message_type=message_type, request_id=request_id, payload=payload)


def extract_frame(buffer: bytearray) -> Frame | None:
    """Remove and return one complete frame from an incremental byte buffer."""
    prefix_length = min(len(buffer), len(MAGIC))
    if buffer[:prefix_length] != MAGIC[:prefix_length]:
        raise FramingError(f"invalid frame magic {bytes(buffer[:4])!r}")
    if len(buffer) < _HEADER.size:
        return None

    magic, version, message_type, request_id, payload_length = _HEADER.unpack_from(
        buffer
    )
    if magic != MAGIC:
        raise FramingError(f"invalid frame magic {magic!r}")
    if version != PROTOCOL_VERSION:
        raise FramingError(f"unsupported protocol version {version}")
    if payload_length > MAX_PAYLOAD_SIZE:
        raise FramingError(
            f"payload length {payload_length} exceeds the 16 MiB version 1 limit"
        )

    frame_length = _HEADER.size + payload_length
    if len(buffer) < frame_length:
        return None
    payload = bytes(buffer[_HEADER.size:frame_length])
    del buffer[:frame_length]
    return Frame(message_type=message_type, request_id=request_id, payload=payload)


def _read_exact(
    connection: SerialConnection,
    size: int,
    deadline: float,
    part: str,
) -> bytes:
    data = bytearray()
    while len(data) < size:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError(f"timed out reading frame {part}")
        try:
            data.extend(connection.read(size - len(data), remaining))
        except SerialError as error:
            raise FramingError(f"connection closed while reading frame {part}") from error
    return bytes(data)

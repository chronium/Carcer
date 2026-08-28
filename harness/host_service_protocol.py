"""Version 1 host-service request and response codecs."""

import struct
from dataclasses import dataclass

from .framing import MAX_PAYLOAD_SIZE, Frame


_HOST_SERVICE_REQUEST = 0x0003
_HOST_SERVICE_RESPONSE = 0x8003
_MAX_SERVICE_NAME_LENGTH = 255
_MAX_ARGUMENTS = 64
_UINT16 = struct.Struct("<H")
_UINT32 = struct.Struct("<I")


class HostServiceProtocolError(RuntimeError):
    """Raised when a host-service protocol frame is malformed."""


@dataclass(frozen=True, slots=True)
class HostServiceRequest:
    """A validated request from the guest for a host service."""

    request_id: int
    service_name: str
    arguments: tuple[bytes, ...]


def decode_host_service_request(frame: Frame) -> HostServiceRequest:
    """Decode and validate one version 1 host-service request frame."""

    if frame.message_type != _HOST_SERVICE_REQUEST:
        raise HostServiceProtocolError(
            f"expected HOST_SERVICE_REQUEST, got message type 0x{frame.message_type:04x}"
        )
    if frame.request_id == 0:
        raise HostServiceProtocolError("host-service request ID must be non-zero")

    payload = frame.payload
    if len(payload) > MAX_PAYLOAD_SIZE:
        raise HostServiceProtocolError("host-service request payload exceeds 16 MiB")

    offset = 0
    service_name_length, offset = _read_integer(
        payload, offset, _UINT16, "service name length"
    )
    if service_name_length == 0:
        raise HostServiceProtocolError("service name must not be empty")
    if service_name_length > _MAX_SERVICE_NAME_LENGTH:
        raise HostServiceProtocolError("service name exceeds 255 encoded bytes")

    service_name_bytes, offset = _read_bytes(
        payload, offset, service_name_length, "service name"
    )
    try:
        service_name = service_name_bytes.decode("utf-8")
    except UnicodeDecodeError as error:
        raise HostServiceProtocolError("service name is not valid UTF-8") from error

    argument_count, offset = _read_integer(
        payload, offset, _UINT16, "argument count"
    )
    if argument_count > _MAX_ARGUMENTS:
        raise HostServiceProtocolError("argument count exceeds 64")

    arguments: list[bytes] = []
    for _ in range(argument_count):
        argument_length, offset = _read_integer(
            payload, offset, _UINT32, "argument length"
        )
        argument, offset = _read_bytes(payload, offset, argument_length, "argument")
        arguments.append(argument)

    if offset != len(payload):
        raise HostServiceProtocolError("unexpected trailing data in host-service request")

    return HostServiceRequest(frame.request_id, service_name, tuple(arguments))


def create_host_service_response(request_id: int, status: int, output: bytes) -> Frame:
    """Create one version 1 host-service response frame."""

    if not 1 <= request_id <= 0xFFFFFFFF:
        raise ValueError("host-service response request ID must be a non-zero uint32")
    if not 0 <= status <= 0xFFFFFFFF:
        raise ValueError("host-service response status must be a uint32")
    if _UINT32.size + len(output) > MAX_PAYLOAD_SIZE:
        raise ValueError("host-service response payload exceeds 16 MiB")

    return Frame(
        message_type=_HOST_SERVICE_RESPONSE,
        request_id=request_id,
        payload=_UINT32.pack(status) + output,
    )


def _read_integer(
    payload: bytes, offset: int, field: struct.Struct, description: str
) -> tuple[int, int]:
    end = offset + field.size
    if end > len(payload):
        raise HostServiceProtocolError(f"truncated {description}")
    return field.unpack_from(payload, offset)[0], end


def _read_bytes(
    payload: bytes, offset: int, length: int, description: str
) -> tuple[bytes, int]:
    end = offset + length
    if end > len(payload):
        raise HostServiceProtocolError(f"truncated {description}")
    return payload[offset:end], end

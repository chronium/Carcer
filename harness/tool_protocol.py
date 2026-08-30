"""Version 1 protocol for discovering and invoking guest tools."""

from __future__ import annotations

import struct
from collections.abc import Sequence
from dataclasses import dataclass

from .framing import MAX_PAYLOAD_SIZE, Frame
from .serial_protocol import SerialProtocolDispatcher

_LIST_TOOLS_REQUEST = 0x0001
_LIST_TOOLS_RESPONSE = 0x8001
_INVOKE_TOOL_REQUEST = 0x0002
_INVOKE_TOOL_RESPONSE = 0x8002

_MAX_TOOLS = 256
_MAX_TOOL_NAME_LENGTH = 255
_MAX_ARGUMENTS = 64
_RESPONSE_TIMEOUT_SECONDS = 5.0

_UINT16 = struct.Struct("<H")
_UINT32 = struct.Struct("<I")


class ToolProtocolError(RuntimeError):
    """A malformed or mismatched tool protocol response."""


@dataclass(frozen=True, slots=True)
class ToolResult:
    status: int
    output: bytes


class ToolClient:
    """Synchronously discover and invoke tools provided by one guest."""

    def __init__(
        self,
        dispatcher: SerialProtocolDispatcher,
    ) -> None:
        self._dispatcher = dispatcher
        self._next_request_id = 1

    def list_tools(self) -> list[str]:
        payload = self._exchange(
            _LIST_TOOLS_REQUEST,
            _LIST_TOOLS_RESPONSE,
            b"",
        )
        return _parse_tool_list(payload)

    def invoke_tool(
        self,
        name: str,
        arguments: Sequence[bytes],
    ) -> ToolResult:
        payload = _encode_invoke_request(name, arguments)
        response = self._exchange(
            _INVOKE_TOOL_REQUEST,
            _INVOKE_TOOL_RESPONSE,
            payload,
        )
        if len(response) < _UINT32.size:
            raise ToolProtocolError("invoke response is missing its status")
        status = _UINT32.unpack_from(response)[0]
        return ToolResult(status=status, output=response[_UINT32.size :])

    def _exchange(
        self,
        request_type: int,
        response_type: int,
        payload: bytes,
    ) -> bytes:
        request_id = self._next_request_id
        self._next_request_id = 1 if request_id == 0xFFFFFFFF else request_id + 1

        request = Frame(
            message_type=request_type,
            request_id=request_id,
            payload=payload,
        )
        response = self._dispatcher.exchange(request, _RESPONSE_TIMEOUT_SECONDS)

        if response.request_id != request_id:
            raise ToolProtocolError(
                f"response request ID {response.request_id} does not match {request_id}"
            )
        if response.message_type != response_type:
            raise ToolProtocolError(
                f"response message type 0x{response.message_type:04x} "
                f"does not match 0x{response_type:04x}"
            )
        return response.payload


def _parse_tool_list(payload: bytes) -> list[str]:
    if len(payload) < _UINT16.size:
        raise ToolProtocolError("list response is missing the tool count")

    tool_count = _UINT16.unpack_from(payload)[0]
    if tool_count > _MAX_TOOLS:
        raise ToolProtocolError(f"list response contains {tool_count} tools; maximum is 256")

    offset = _UINT16.size
    tools: list[str] = []
    for _ in range(tool_count):
        if len(payload) - offset < _UINT16.size:
            raise ToolProtocolError("list response has a truncated name length")
        name_length = _UINT16.unpack_from(payload, offset)[0]
        offset += _UINT16.size
        if name_length == 0:
            raise ToolProtocolError("list response contains an empty tool name")
        if name_length > _MAX_TOOL_NAME_LENGTH:
            raise ToolProtocolError("list response tool name exceeds 255 bytes")
        if len(payload) - offset < name_length:
            raise ToolProtocolError("list response has a truncated tool name")

        encoded_name = payload[offset : offset + name_length]
        offset += name_length
        try:
            tools.append(encoded_name.decode("utf-8"))
        except UnicodeDecodeError as error:
            raise ToolProtocolError("list response contains invalid UTF-8") from error

    if offset != len(payload):
        raise ToolProtocolError("list response contains unexpected trailing data")
    return tools


def _encode_invoke_request(name: str, arguments: Sequence[bytes]) -> bytes:
    try:
        encoded_name = name.encode("utf-8")
    except UnicodeEncodeError as error:
        raise ValueError("tool name is not valid UTF-8") from error
    if not encoded_name:
        raise ValueError("tool name must not be empty")
    if len(encoded_name) > _MAX_TOOL_NAME_LENGTH:
        raise ValueError("tool name exceeds 255 UTF-8 bytes")
    if len(arguments) > _MAX_ARGUMENTS:
        raise ValueError("an invocation may contain at most 64 arguments")

    parts = [
        _UINT16.pack(len(encoded_name)),
        encoded_name,
        _UINT16.pack(len(arguments)),
    ]
    payload_length = sum(len(part) for part in parts)
    for argument in arguments:
        argument_length = len(argument)
        if payload_length + _UINT32.size + argument_length > MAX_PAYLOAD_SIZE:
            raise ValueError("invocation payload exceeds the 16 MiB frame limit")
        parts.extend((_UINT32.pack(argument_length), argument))
        payload_length += _UINT32.size + argument_length
    return b"".join(parts)

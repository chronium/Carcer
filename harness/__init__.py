"""External AgentOS harness."""

from .framing import Frame, FramingError, encode_frame, read_frame
from .host_service_protocol import (
    HostServiceProtocolError,
    HostServiceRequest,
    create_host_service_response,
    decode_host_service_request,
)
from .qmp import QmpClient, QmpError
from .qemu import QemuProcessController
from .serial import SerialConnection, SerialError
from .tool_protocol import ToolClient, ToolProtocolError, ToolResult

__all__ = [
    "Frame",
    "FramingError",
    "HostServiceProtocolError",
    "HostServiceRequest",
    "QmpClient",
    "QmpError",
    "QemuProcessController",
    "SerialConnection",
    "SerialError",
    "ToolClient",
    "ToolProtocolError",
    "ToolResult",
    "create_host_service_response",
    "decode_host_service_request",
    "encode_frame",
    "read_frame",
]

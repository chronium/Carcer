"""External CodexOS harness."""

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
from .source_snapshot import (
    SnapshotFile,
    SourceSnapshotError,
    decode_source_snapshot,
    encode_source_snapshot,
)
from .tool_protocol import ToolClient, ToolProtocolError, ToolResult
from .trusted_build import BuildResult, BuildStatus, build_source_snapshot

__all__ = [
    "BuildResult",
    "BuildStatus",
    "Frame",
    "FramingError",
    "HostServiceProtocolError",
    "HostServiceRequest",
    "QmpClient",
    "QmpError",
    "QemuProcessController",
    "SerialConnection",
    "SerialError",
    "SnapshotFile",
    "SourceSnapshotError",
    "ToolClient",
    "ToolProtocolError",
    "ToolResult",
    "build_source_snapshot",
    "create_host_service_response",
    "decode_host_service_request",
    "decode_source_snapshot",
    "encode_frame",
    "encode_source_snapshot",
    "read_frame",
]

"""External AgentOS harness."""

from .framing import Frame, FramingError, encode_frame, read_frame
from .qmp import QmpClient, QmpError
from .qemu import QemuProcessController
from .serial import SerialConnection, SerialError

__all__ = [
    "Frame",
    "FramingError",
    "QmpClient",
    "QmpError",
    "QemuProcessController",
    "SerialConnection",
    "SerialError",
    "encode_frame",
    "read_frame",
]

import socket
import struct
import tempfile
import unittest
from contextlib import contextmanager
from pathlib import Path

from harness import (
    Frame,
    SerialConnection,
    ToolClient,
    ToolProtocolError,
    ToolResult,
    encode_frame,
)


@contextmanager
def connected_serial_peer():
    with tempfile.TemporaryDirectory() as temporary_directory:
        socket_path = Path(temporary_directory) / "serial.sock"
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
            listener.bind(str(socket_path))
            listener.listen(1)
            with SerialConnection(socket_path) as serial:
                peer, _ = listener.accept()
                with peer:
                    yield serial, peer


def receive_peer_frame(peer: socket.socket) -> Frame:
    header = receive_exact(peer, 16)
    magic, version, message_type, request_id, payload_length = struct.unpack(
        "<4sHHII", header
    )
    if magic != b"CXOS" or version != 1:
        raise AssertionError("client sent an invalid frame header")
    return Frame(message_type, request_id, receive_exact(peer, payload_length))


def receive_exact(peer: socket.socket, size: int) -> bytes:
    data = bytearray()
    while len(data) < size:
        chunk = peer.recv(size - len(data))
        if not chunk:
            raise AssertionError("client closed while sending a frame")
        data.extend(chunk)
    return bytes(data)


class ToolProtocolIntegrationTest(unittest.TestCase):
    def test_list_tools_decodes_utf8_names(self) -> None:
        names = ["list", "café", "工具"]
        payload = struct.pack("<H", len(names)) + b"".join(
            struct.pack("<H", len(name.encode("utf-8"))) + name.encode("utf-8")
            for name in names
        )

        with connected_serial_peer() as (serial, peer):
            peer.sendall(encode_frame(Frame(0x8001, 1, payload)))
            self.assertEqual(ToolClient(serial).list_tools(), names)

            request = receive_peer_frame(peer)
            self.assertEqual(request, Frame(0x0001, 1, b""))

    def test_invoke_tool_preserves_binary_arguments_and_output(self) -> None:
        arguments = [b"", b"\x00\xffargument", bytes(range(256))]
        output = b"\xff\x00binary output\x00"

        with connected_serial_peer() as (serial, peer):
            peer.sendall(encode_frame(Frame(0x8002, 1, struct.pack("<I", 7) + output)))
            result = ToolClient(serial).invoke_tool("binary.tool", arguments)
            self.assertEqual(result, ToolResult(status=7, output=output))

            request = receive_peer_frame(peer)
            self.assertEqual((request.message_type, request.request_id), (0x0002, 1))
            offset = 0
            name_length = struct.unpack_from("<H", request.payload, offset)[0]
            offset += 2
            self.assertEqual(request.payload[offset : offset + name_length], b"binary.tool")
            offset += name_length
            argument_count = struct.unpack_from("<H", request.payload, offset)[0]
            offset += 2
            decoded_arguments = []
            for _ in range(argument_count):
                argument_length = struct.unpack_from("<I", request.payload, offset)[0]
                offset += 4
                decoded_arguments.append(request.payload[offset : offset + argument_length])
                offset += argument_length
            self.assertEqual(decoded_arguments, arguments)
            self.assertEqual(offset, len(request.payload))

    def test_rejects_mismatched_request_id_and_message_type(self) -> None:
        with connected_serial_peer() as (serial, peer):
            client = ToolClient(serial)

            peer.sendall(encode_frame(Frame(0x8001, 99, struct.pack("<H", 0))))
            with self.assertRaisesRegex(ToolProtocolError, "request ID"):
                client.list_tools()

            peer.sendall(encode_frame(Frame(0x8002, 2, struct.pack("<I", 0))))
            with self.assertRaisesRegex(ToolProtocolError, "message type"):
                client.list_tools()

    def test_rejects_malformed_length_prefixed_payloads(self) -> None:
        with connected_serial_peer() as (serial, peer):
            client = ToolClient(serial)

            truncated = struct.pack("<HH", 1, 5) + b"ab"
            peer.sendall(encode_frame(Frame(0x8001, 1, truncated)))
            with self.assertRaisesRegex(ToolProtocolError, "truncated tool name"):
                client.list_tools()

            peer.sendall(encode_frame(Frame(0x8001, 2, struct.pack("<H", 0) + b"x")))
            with self.assertRaisesRegex(ToolProtocolError, "trailing data"):
                client.list_tools()

            invalid_utf8 = struct.pack("<HH", 1, 1) + b"\xff"
            peer.sendall(encode_frame(Frame(0x8001, 3, invalid_utf8)))
            with self.assertRaisesRegex(ToolProtocolError, "UTF-8"):
                client.list_tools()


if __name__ == "__main__":
    unittest.main()

import socket
import struct
import tempfile
import unittest
from contextlib import contextmanager
from pathlib import Path

from harness import Frame, FramingError, SerialConnection, encode_frame, read_frame


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


class FramingIntegrationTest(unittest.TestCase):
    def test_exact_wire_encoding_round_trips_arbitrary_payload(self) -> None:
        payload = bytes(range(256)) + b"\x00\xffCodexOS\x00"
        frame = Frame(message_type=0xBEEF, request_id=0xDEADBEEF, payload=payload)
        encoded = encode_frame(frame)

        expected_header = struct.pack(
            "<4sHHII", b"CXOS", 1, 0xBEEF, 0xDEADBEEF, len(payload)
        )
        self.assertEqual(encoded, expected_header + payload)

        with connected_serial_peer() as (serial, peer):
            peer.sendall(encoded)
            self.assertEqual(read_frame(serial), frame)

    def test_fragmented_frame_and_partial_frame_closure(self) -> None:
        frame = Frame(message_type=7, request_id=42, payload=b"fragmented\x00payload")
        encoded = encode_frame(frame)

        with connected_serial_peer() as (serial, peer):
            for fragment in (
                encoded[:3],
                encoded[3:11],
                encoded[11:18],
                encoded[18:],
            ):
                peer.sendall(fragment)
            self.assertEqual(read_frame(serial), frame)

            partial = encode_frame(Frame(message_type=8, request_id=43, payload=b"data"))
            peer.sendall(partial[:-2])
            peer.shutdown(socket.SHUT_WR)
            with self.assertRaisesRegex(FramingError, "closed.*payload"):
                read_frame(serial)

    def test_rejects_invalid_header_fields_before_reading_payload(self) -> None:
        with connected_serial_peer() as (serial, peer):
            peer.sendall(struct.pack("<4sHHII", b"NOPE", 1, 0, 0, 0))
            with self.assertRaisesRegex(FramingError, "magic"):
                read_frame(serial)

            peer.sendall(struct.pack("<4sHHII", b"CXOS", 2, 0, 0, 0))
            with self.assertRaisesRegex(FramingError, "version"):
                read_frame(serial)

            peer.sendall(
                struct.pack("<4sHHII", b"CXOS", 1, 0, 0, 16 * 1024 * 1024 + 1)
            )
            with self.assertRaisesRegex(FramingError, "exceeds"):
                read_frame(serial)


if __name__ == "__main__":
    unittest.main()

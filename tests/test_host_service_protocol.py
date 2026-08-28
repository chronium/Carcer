import struct
import unittest

from harness.framing import Frame
from harness.host_service_protocol import (
    HostServiceProtocolError,
    create_host_service_response,
    decode_host_service_request,
)


class HostServiceProtocolTests(unittest.TestCase):
    def test_decodes_utf8_name_and_binary_arguments(self) -> None:
        service_name = "编译"
        encoded_name = service_name.encode("utf-8")
        arguments = (b"", b"\x00\xffbinary\x00", bytes(range(256)))
        payload = struct.pack("<H", len(encoded_name)) + encoded_name
        payload += struct.pack("<H", len(arguments))
        for argument in arguments:
            payload += struct.pack("<I", len(argument)) + argument

        request = decode_host_service_request(Frame(0x0003, 0x12345678, payload))

        self.assertEqual(request.request_id, 0x12345678)
        self.assertEqual(request.service_name, service_name)
        self.assertEqual(request.arguments, arguments)

    def test_response_preserves_status_and_binary_output(self) -> None:
        output = b"\x00result\xff\x00"

        frame = create_host_service_response(37, 0xA0B0C0D0, output)

        self.assertEqual(frame.message_type, 0x8003)
        self.assertEqual(frame.request_id, 37)
        self.assertEqual(struct.unpack("<I", frame.payload[:4])[0], 0xA0B0C0D0)
        self.assertEqual(frame.payload[4:], output)

    def test_rejects_reserved_request_id_and_wrong_message_type(self) -> None:
        with self.assertRaisesRegex(HostServiceProtocolError, "non-zero"):
            decode_host_service_request(Frame(0x0003, 0, b""))

        with self.assertRaisesRegex(HostServiceProtocolError, "message type"):
            decode_host_service_request(Frame(0x0002, 1, b""))

    def test_rejects_malformed_length_prefixed_payloads(self) -> None:
        with self.assertRaisesRegex(
            HostServiceProtocolError, "truncated service name"
        ):
            decode_host_service_request(Frame(0x0003, 1, struct.pack("<H", 4) + b"ab"))

        truncated_argument = struct.pack("<H", 1) + b"x" + struct.pack("<H", 1)
        truncated_argument += struct.pack("<I", 3) + b"a"
        with self.assertRaisesRegex(HostServiceProtocolError, "truncated argument"):
            decode_host_service_request(Frame(0x0003, 1, truncated_argument))

        trailing_data = struct.pack("<H", 1) + b"x" + struct.pack("<H", 0) + b"extra"
        with self.assertRaisesRegex(HostServiceProtocolError, "trailing"):
            decode_host_service_request(Frame(0x0003, 1, trailing_data))

        invalid_utf8 = struct.pack("<H", 1) + b"\xff" + struct.pack("<H", 0)
        with self.assertRaisesRegex(HostServiceProtocolError, "UTF-8"):
            decode_host_service_request(Frame(0x0003, 1, invalid_utf8))

    def test_rejects_name_and_argument_limits(self) -> None:
        empty_name = struct.pack("<HH", 0, 0)
        with self.assertRaisesRegex(HostServiceProtocolError, "empty"):
            decode_host_service_request(Frame(0x0003, 1, empty_name))

        long_name = struct.pack("<H", 256) + (b"x" * 256) + struct.pack("<H", 0)
        with self.assertRaisesRegex(HostServiceProtocolError, "255"):
            decode_host_service_request(Frame(0x0003, 1, long_name))

        too_many_arguments = struct.pack("<H", 1) + b"x" + struct.pack("<H", 65)
        with self.assertRaisesRegex(HostServiceProtocolError, "64"):
            decode_host_service_request(Frame(0x0003, 1, too_many_arguments))


if __name__ == "__main__":
    unittest.main()

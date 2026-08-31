import socket
import shutil
import struct
import tempfile
import threading
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from pathlib import Path
from unittest.mock import patch

from harness import (
    TEST_HARDWARE_PROFILE,
    CandidateBootValidator,
    BuildStatus,
    CodexOSHostServices,
    Frame,
    ProvidedAssets,
    SerialConnection,
    SerialError,
    SerialProtocolDispatcher,
    ToolClient,
    ToolProtocolError,
    ToolResult,
    create_host_service_response,
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


@contextmanager
def connected_protocol(
    exchange_host_services=None,
    *,
    background_host_services=None,
    event_recorder=None,
):
    with connected_serial_peer() as (serial, peer):
        protocol = SerialProtocolDispatcher(
            serial,
            background_host_services=background_host_services,
            exchange_host_services=exchange_host_services,
            event_recorder=event_recorder,
        )
        protocol.start_ready()
        try:
            yield protocol, peer
        finally:
            protocol.close()


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


def relay_host_response_as_tool_response(
    peer: socket.socket,
    tool_request_id: int,
    *,
    byte_at_a_time: bool,
    delay_seconds: float = 0.0,
) -> bytes:
    """Model the G13 nested host-response relay without buffering its payload."""
    header = receive_exact(peer, 16)
    magic, version, message_type, host_request_id, payload_length = struct.unpack(
        "<4sHHII", header
    )
    if (magic, version, message_type) != (b"CXOS", 1, 0x8003):
        raise AssertionError("dispatcher sent an invalid host-service response")
    status = receive_exact(peer, 4)
    peer.sendall(
        struct.pack(
            "<4sHHII",
            b"CXOS",
            1,
            0x8002,
            tool_request_id,
            payload_length,
        )
        + status
    )
    output = bytearray()
    remaining = payload_length - len(status)
    read_size = 1 if byte_at_a_time else 257
    while remaining:
        chunk = peer.recv(min(read_size, remaining))
        if not chunk:
            raise AssertionError("dispatcher closed during host-response relay")
        output.extend(chunk)
        peer.sendall(chunk)
        remaining -= len(chunk)
        if delay_seconds:
            time.sleep(delay_seconds)
    if host_request_id == 0:
        raise AssertionError("host-service response used request ID zero")
    return bytes(output)


def host_request(request_id: int, name: str, arguments: tuple[bytes, ...]) -> bytes:
    encoded_name = name.encode("utf-8")
    payload = bytearray(struct.pack("<H", len(encoded_name)) + encoded_name)
    payload.extend(struct.pack("<H", len(arguments)))
    for argument in arguments:
        payload.extend(struct.pack("<I", len(argument)))
        payload.extend(argument)
    return encode_frame(Frame(0x0003, request_id, bytes(payload)))


class ToolProtocolIntegrationTest(unittest.TestCase):
    def test_large_host_response_is_duplex_pumped_and_dispatcher_remains_usable(
        self,
    ) -> None:
        payload = bytes(range(256)) * 4096

        class HostServices:
            def __init__(self) -> None:
                self.requests = []

            def handle_request(self, request):
                self.requests.append(request.service_name)
                output = (
                    payload
                    if request.service_name == "read_provided_asset"
                    else b"ok"
                )
                return create_host_service_response(request.request_id, 0, output)

        services = HostServices()
        events = []
        with (
            connected_protocol(
                services,
                event_recorder=lambda event, data: events.append(
                    (event, dict(data))
                ),
            ) as (protocol, peer),
            ThreadPoolExecutor() as pool,
        ):
            client = ToolClient(protocol)
            large = pool.submit(client.invoke_tool, "read_provided_asset", [])
            large_request = receive_peer_frame(peer)
            peer.sendall(host_request(71, "read_provided_asset", ()))
            relayed = relay_host_response_as_tool_response(
                peer,
                large_request.request_id,
                byte_at_a_time=True,
            )
            self.assertEqual(len(relayed), 1024 * 1024)
            self.assertEqual(relayed, payload)
            self.assertEqual(large.result(10), ToolResult(0, payload))

            ordinary = pool.submit(client.list_tools)
            ordinary_request = receive_peer_frame(peer)
            peer.sendall(
                encode_frame(
                    Frame(0x8001, ordinary_request.request_id, struct.pack("<H", 0))
                )
            )
            self.assertEqual(ordinary.result(2), [])

            build = pool.submit(client.invoke_tool, "build", [])
            build_request = receive_peer_frame(peer)
            peer.sendall(host_request(72, "build", ()))
            build_host_response = receive_peer_frame(peer)
            self.assertEqual(build_host_response.request_id, 72)
            peer.sendall(
                encode_frame(
                    Frame(
                        0x8002,
                        build_request.request_id,
                        struct.pack("<I", 0) + b"built",
                    )
                )
            )
            self.assertEqual(build.result(2), ToolResult(0, b"built"))
            self.assertEqual(services.requests, ["read_provided_asset", "build"])
            host_write_phases = [
                data["phase"]
                for event, data in events
                if event == "serial_protocol_write"
                and data["write_kind"] == "host_response"
                and data["request_id"] == 71
            ]
            self.assertEqual(host_write_phases[0], "write_started")
            self.assertIn("write_progress", host_write_phases)
            self.assertEqual(host_write_phases[-1], "write_completed")
            self.assertTrue(
                any(
                    event == "serial_host_service_response_prepared"
                    and data["response_bytes"] == len(payload) + 20
                    for event, data in events
                )
            )
            self.assertTrue(
                any(event == "serial_tool_response_received" for event, _ in events)
            )
            self.assertFalse(any("payload" in data for _, data in events))

    def test_duplex_pump_handles_small_buffers_and_slow_peer(self) -> None:
        payload = bytes(range(251)) * 2089

        class HostService:
            def handle_request(self, request):
                return create_host_service_response(request.request_id, 0, payload)

        with (
            connected_serial_peer() as (serial, peer),
            ThreadPoolExecutor() as pool,
        ):
            connection = serial._require_socket()
            for endpoint in (connection, peer):
                endpoint.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
                endpoint.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
            protocol = SerialProtocolDispatcher(
                serial,
                exchange_host_services=HostService(),
            )
            protocol.start_ready()
            try:
                result = pool.submit(ToolClient(protocol).invoke_tool, "read", [])
                request = receive_peer_frame(peer)
                peer.sendall(host_request(73, "read_provided_asset", ()))
                relayed = relay_host_response_as_tool_response(
                    peer,
                    request.request_id,
                    byte_at_a_time=False,
                    delay_seconds=0.0001,
                )
                self.assertEqual(relayed, payload)
                self.assertEqual(result.result(10), ToolResult(0, payload))
            finally:
                protocol.close()

    def test_non_reading_peer_fails_boundedly_and_close_stops_reader(self) -> None:
        payload = b"x" * (1024 * 1024)

        class HostService:
            def handle_request(self, request):
                return create_host_service_response(request.request_id, 0, payload)

        with (
            connected_serial_peer() as (serial, peer),
            ThreadPoolExecutor() as pool,
            patch("harness.serial_protocol._WRITE_STALL_TIMEOUT_SECONDS", 0.05),
        ):
            connection = serial._require_socket()
            connection.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
            peer.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
            protocol = SerialProtocolDispatcher(
                serial,
                exchange_host_services=HostService(),
            )
            protocol.start_ready()
            result = pool.submit(ToolClient(protocol).invoke_tool, "read", [])
            receive_peer_frame(peer)
            peer.sendall(host_request(74, "read_provided_asset", ()))
            with self.assertRaisesRegex(TimeoutError, "writing serial protocol"):
                result.result(2)
            protocol.close()
            self.assertFalse(protocol.reader_alive)

    def test_close_interrupts_large_host_response_without_deadlock(self) -> None:
        payload = b"y" * (1024 * 1024)

        class HostService:
            def handle_request(self, request):
                return create_host_service_response(request.request_id, 0, payload)

        with connected_serial_peer() as (serial, peer), ThreadPoolExecutor() as pool:
            connection = serial._require_socket()
            connection.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
            peer.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
            protocol = SerialProtocolDispatcher(
                serial,
                exchange_host_services=HostService(),
            )
            protocol.start_ready()
            result = pool.submit(ToolClient(protocol).invoke_tool, "read", [])
            receive_peer_frame(peer)
            peer.sendall(host_request(75, "read_provided_asset", ()))
            time.sleep(0.05)
            protocol.close()
            with self.assertRaisesRegex(SerialError, "closed"):
                result.result(2)
            self.assertFalse(protocol.reader_alive)

    def test_list_tools_decodes_utf8_names(self) -> None:
        names = ["list", "café", "工具"]
        payload = struct.pack("<H", len(names)) + b"".join(
            struct.pack("<H", len(name.encode("utf-8"))) + name.encode("utf-8")
            for name in names
        )
        with connected_protocol() as (protocol, peer), ThreadPoolExecutor() as pool:
            result = pool.submit(ToolClient(protocol).list_tools)
            request = receive_peer_frame(peer)
            self.assertEqual(request, Frame(0x0001, 1, b""))
            peer.sendall(encode_frame(Frame(0x8001, 1, payload)))
            self.assertEqual(result.result(2), names)

    def test_invoke_tool_preserves_binary_arguments_and_output(self) -> None:
        arguments = [b"", b"\x00\xffargument", bytes(range(256))]
        output = b"\xff\x00binary output\x00"
        with connected_protocol() as (protocol, peer), ThreadPoolExecutor() as pool:
            result = pool.submit(
                ToolClient(protocol).invoke_tool,
                "binary.tool",
                arguments,
            )
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
            peer.sendall(
                encode_frame(Frame(0x8002, 1, struct.pack("<I", 7) + output))
            )
            self.assertEqual(result.result(2), ToolResult(status=7, output=output))

    def test_handles_host_service_while_waiting_for_tool_response(self) -> None:
        tool_output = b"original tool result"
        with tempfile.TemporaryDirectory() as temporary:
            qemu = shutil.which("qemu-system-x86_64")
            self.assertIsNotNone(qemu)
            host_services = CodexOSHostServices(
                Path(temporary) / "staging",
                CandidateBootValidator(qemu, TEST_HARDWARE_PROFILE),
            )
            with (
                connected_protocol(host_services) as (protocol, peer),
                ThreadPoolExecutor() as pool,
            ):
                result = pool.submit(ToolClient(protocol).invoke_tool, "future", [])
                tool_request = receive_peer_frame(peer)
                self.assertEqual(
                    (tool_request.message_type, tool_request.request_id),
                    (0x0002, 1),
                )
                peer.sendall(host_request(77, "unknown", ()))
                host_response = receive_peer_frame(peer)
                self.assertEqual(
                    (host_response.message_type, host_response.request_id),
                    (0x8003, 77),
                )
                self.assertNotEqual(
                    struct.unpack_from("<I", host_response.payload)[0],
                    0,
                )
                peer.sendall(
                    encode_frame(
                        Frame(0x8002, 1, struct.pack("<I", 7) + tool_output)
                    )
                )
                self.assertEqual(result.result(2), ToolResult(7, tool_output))

    def test_host_service_execution_does_not_consume_tool_response_timeout(
        self,
    ) -> None:
        handler_started = threading.Event()
        release_handler = threading.Event()

        class SlowHostService:
            def handle_request(self, request):
                handler_started.set()
                if not release_handler.wait(1.0):
                    raise AssertionError("test did not release host-service handler")
                return create_host_service_response(
                    request.request_id,
                    0,
                    b"trusted work completed",
                )

        with (
            connected_protocol(SlowHostService()) as (protocol, peer),
            ThreadPoolExecutor() as pool,
            patch(
                "harness.tool_protocol._RESPONSE_TIMEOUT_SECONDS",
                0.05,
            ),
        ):
            client = ToolClient(protocol)
            first = pool.submit(client.list_tools)
            first_request = receive_peer_frame(peer)
            peer.sendall(host_request(78, "slow_trusted_service", ()))
            self.assertTrue(handler_started.wait(1.0))
            time.sleep(0.12)
            self.assertFalse(first.done())

            release_handler.set()
            host_response = receive_peer_frame(peer)
            self.assertEqual(
                (host_response.message_type, host_response.request_id),
                (0x8003, 78),
            )
            self.assertEqual(
                host_response.payload,
                struct.pack("<I", 0) + b"trusted work completed",
            )
            peer.sendall(
                encode_frame(
                    Frame(
                        0x8001,
                        first_request.request_id,
                        struct.pack("<H", 0),
                    )
                )
            )
            self.assertEqual(first.result(1.0), [])

            second = pool.submit(client.list_tools)
            second_request = receive_peer_frame(peer)
            peer.sendall(
                encode_frame(
                    Frame(
                        0x8001,
                        second_request.request_id,
                        struct.pack("<H", 0),
                    )
                )
            )
            self.assertEqual(second.result(1.0), [])

    def test_rejects_mismatched_request_id_and_message_type(self) -> None:
        with connected_protocol() as (protocol, peer), ThreadPoolExecutor() as pool:
            client = ToolClient(protocol)
            first = pool.submit(client.list_tools)
            receive_peer_frame(peer)
            peer.sendall(encode_frame(Frame(0x8001, 99, struct.pack("<H", 0))))
            with self.assertRaisesRegex(ToolProtocolError, "request ID"):
                first.result(2)
            second = pool.submit(client.list_tools)
            receive_peer_frame(peer)
            peer.sendall(encode_frame(Frame(0x8002, 2, struct.pack("<I", 0))))
            with self.assertRaisesRegex(ToolProtocolError, "message type"):
                second.result(2)

    def test_rejects_malformed_length_prefixed_payloads(self) -> None:
        with connected_protocol() as (protocol, peer), ThreadPoolExecutor() as pool:
            client = ToolClient(protocol)
            responses = (
                (Frame(0x8001, 1, struct.pack("<HH", 1, 5) + b"ab"), "truncated tool name"),
                (Frame(0x8001, 2, struct.pack("<H", 0) + b"x"), "trailing data"),
                (Frame(0x8001, 3, struct.pack("<HH", 1, 1) + b"\xff"), "UTF-8"),
            )
            for frame, message in responses:
                result = pool.submit(client.list_tools)
                receive_peer_frame(peer)
                peer.sendall(encode_frame(frame))
                with self.assertRaisesRegex(ToolProtocolError, message):
                    result.result(2)

    def test_idle_provided_asset_requests_are_serviced_after_ready(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            asset = supplied / "alpha"
            asset.mkdir(parents=True)
            data = b"idle post-ready bytes\x00\xff"
            (asset / "payload.bin").write_bytes(data)
            assets = ProvidedAssets.from_directory(supplied)
            with connected_protocol(
                background_host_services=assets,
            ) as (protocol, peer):
                self.assertTrue(protocol.reader_alive)
                peer.sendall(host_request(41, "list_provided_assets", ()))
                listed = receive_peer_frame(peer)
                self.assertEqual((listed.message_type, listed.request_id), (0x8003, 41))
                self.assertEqual(struct.unpack_from("<I", listed.payload)[0], 0)
                self.assertIn(b"alpha\tpayload.bin", listed.payload[4:])
                for request_id, offset, length, expected in (
                    (42, 0, 4, data[:4]),
                    (43, 5, 7, data[5:12]),
                    (44, len(data), 0, b""),
                ):
                    peer.sendall(
                        host_request(
                            request_id,
                            "read_provided_asset",
                            (b"alpha", str(offset).encode(), str(length).encode()),
                        )
                    )
                    response = receive_peer_frame(peer)
                    self.assertEqual(response.request_id, request_id)
                    self.assertEqual(
                        (struct.unpack_from("<I", response.payload)[0], response.payload[4:]),
                        (0, expected),
                    )

    def test_idle_guest_cannot_reach_exchange_host_services(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            asset = supplied / "alpha"
            asset.mkdir(parents=True)
            (asset / "payload.bin").write_bytes(b"scoped asset bytes")
            assets = ProvidedAssets.from_directory(supplied)

            class ExchangeHostServices:
                def __init__(self) -> None:
                    self.requests = []

                def handle_request(self, request):
                    self.requests.append(request)
                    if request.service_name in {
                        "list_provided_assets",
                        "read_provided_asset",
                    }:
                        return assets.handle_request(request)
                    return create_host_service_response(
                        request.request_id,
                        0,
                        b"exchange service completed",
                    )

            exchange_services = ExchangeHostServices()
            with (
                connected_protocol(
                    exchange_services,
                    background_host_services=assets,
                ) as (protocol, peer),
                ThreadPoolExecutor() as pool,
            ):
                for request_id, service_name in enumerate(
                    ("build", "finish_generation", "request_feature"),
                    51,
                ):
                    peer.sendall(host_request(request_id, service_name, ()))
                    response = receive_peer_frame(peer)
                    status = struct.unpack_from("<I", response.payload)[0]
                    diagnostic = response.payload[4:]
                    self.assertEqual(response.request_id, request_id)
                    self.assertEqual(status, 1)
                    self.assertTrue(diagnostic)
                    self.assertLessEqual(len(diagnostic), 4 * 1024)
                self.assertEqual(exchange_services.requests, [])

                client = ToolClient(protocol)
                ordinary = pool.submit(client.list_tools)
                ordinary_request = receive_peer_frame(peer)
                peer.sendall(
                    encode_frame(
                        Frame(
                            0x8001,
                            ordinary_request.request_id,
                            struct.pack("<H", 0),
                        )
                    )
                    + host_request(60, "build", ())
                )
                self.assertEqual(ordinary.result(1.0), [])
                after_response = receive_peer_frame(peer)
                self.assertEqual(after_response.request_id, 60)
                self.assertEqual(
                    struct.unpack_from("<I", after_response.payload)[0],
                    1,
                )
                self.assertEqual(exchange_services.requests, [])

                build = pool.submit(client.invoke_tool, "build", [])
                build_request = receive_peer_frame(peer)
                peer.sendall(host_request(61, "build", ()))
                build_service_response = receive_peer_frame(peer)
                self.assertEqual(
                    (build_service_response.request_id, build_service_response.payload),
                    (61, struct.pack("<I", 0) + b"exchange service completed"),
                )

                peer.sendall(host_request(62, "list_provided_assets", ()))
                asset_response = receive_peer_frame(peer)
                self.assertEqual(asset_response.request_id, 62)
                self.assertEqual(struct.unpack_from("<I", asset_response.payload)[0], 0)
                self.assertIn(b"alpha\tpayload.bin", asset_response.payload[4:])

                peer.sendall(
                    encode_frame(
                        Frame(
                            0x8002,
                            build_request.request_id,
                            struct.pack("<I", 0) + b"tool build completed",
                        )
                    )
                )
                self.assertEqual(
                    build.result(1.0),
                    ToolResult(0, b"tool build completed"),
                )
                self.assertEqual(
                    [request.service_name for request in exchange_services.requests],
                    ["build", "list_provided_assets"],
                )

    def test_candidate_validation_services_assets_after_ready(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            asset = supplied / "alpha"
            asset.mkdir(parents=True)
            (asset / "payload.bin").write_bytes(b"candidate asset bytes")
            assets = ProvidedAssets.from_directory(supplied)
            validator = CandidateBootValidator(
                "unused-qemu",
                TEST_HARDWARE_PROFILE,
                provided_assets=assets,
            )
            with connected_serial_peer() as (serial, peer), ThreadPoolExecutor() as pool:
                protocol = SerialProtocolDispatcher(
                    serial,
                    startup_host_services=assets,
                    background_host_services=assets,
                    exchange_host_services=assets,
                )
                try:
                    result = pool.submit(validator._validate_guest, protocol)
                    peer.sendall(b"CODEXOS-SEED-READY\n")
                    tool_request = receive_peer_frame(peer)
                    self.assertEqual(tool_request.message_type, 0x0001)
                    peer.sendall(host_request(61, "list_provided_assets", ()))
                    asset_response = receive_peer_frame(peer)
                    self.assertEqual(
                        (asset_response.message_type, asset_response.request_id),
                        (0x8003, 61),
                    )
                    self.assertEqual(
                        struct.unpack_from("<I", asset_response.payload)[0],
                        0,
                    )
                    peer.sendall(
                        encode_frame(
                            Frame(
                                0x8001,
                                tool_request.request_id,
                                struct.pack("<H", 0),
                            )
                        )
                    )
                    self.assertEqual(result.result(2).status, BuildStatus.SUCCESS)
                finally:
                    protocol.close()

    def test_malformed_host_request_does_not_desynchronize_next_frame(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            asset = supplied / "alpha"
            asset.mkdir(parents=True)
            (asset / "data").write_bytes(b"bytes")
            assets = ProvidedAssets.from_directory(supplied)
            with connected_protocol(
                background_host_services=assets,
            ) as (_protocol, peer):
                peer.sendall(
                    encode_frame(Frame(0x0003, 51, b"\x05\x00bad"))
                    + host_request(52, "list_provided_assets", ())
                )
                malformed = receive_peer_frame(peer)
                valid = receive_peer_frame(peer)
                self.assertEqual(malformed.request_id, 51)
                self.assertEqual(struct.unpack_from("<I", malformed.payload)[0], 1)
                self.assertEqual(valid.request_id, 52)
                self.assertEqual(struct.unpack_from("<I", valid.payload)[0], 0)

    def test_dispatcher_is_the_only_serial_reader_and_stops_boundedly(self) -> None:
        with connected_serial_peer() as (serial, peer):
            reader_threads: set[int] = set()
            original_pump = serial.pump

            def tracked_pump(max_read_bytes, outgoing, timeout_seconds):
                reader_threads.add(threading.get_ident())
                return original_pump(max_read_bytes, outgoing, timeout_seconds)

            serial.pump = tracked_pump  # type: ignore[method-assign]
            protocol = SerialProtocolDispatcher(serial)
            protocol.start_ready()
            try:
                with ThreadPoolExecutor() as pool:
                    result = pool.submit(ToolClient(protocol).list_tools)
                    request = receive_peer_frame(peer)
                    peer.sendall(
                        encode_frame(
                            Frame(0x8001, request.request_id, struct.pack("<H", 0))
                        )
                    )
                    self.assertEqual(result.result(2), [])
                self.assertEqual(len(reader_threads), 1)
            finally:
                protocol.close()
            self.assertFalse(protocol.reader_alive)

    def test_close_interrupts_an_outstanding_exchange_without_deadlock(self) -> None:
        with connected_serial_peer() as (serial, peer), ThreadPoolExecutor() as pool:
            protocol = SerialProtocolDispatcher(serial)
            protocol.start_ready()
            result = pool.submit(ToolClient(protocol).list_tools)
            receive_peer_frame(peer)
            protocol.close()
            with self.assertRaisesRegex(SerialError, "closed"):
                result.result(2)
            self.assertFalse(protocol.reader_alive)


if __name__ == "__main__":
    unittest.main()

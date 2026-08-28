import shutil
import socket
import tempfile
import unittest
from pathlib import Path

from harness import QemuProcessController, SerialConnection, SerialError


class SerialIntegrationTest(unittest.TestCase):
    def test_connects_to_qemu_serial_socket_and_cleans_up(self) -> None:
        executable = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(executable, "qemu-system-x86_64 must be installed")

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            serial_socket_path = temporary_path / "serial.sock"
            serial = SerialConnection(serial_socket_path)

            with QemuProcessController(executable) as controller:
                controller.start(
                    [
                        "-S",
                        "-display",
                        "none",
                        "-monitor",
                        "none",
                        "-nodefaults",
                    ],
                    stdout_path=temporary_path / "qemu.stdout",
                    stderr_path=temporary_path / "qemu.stderr",
                    serial_socket_path=serial_socket_path,
                )

                with serial:
                    self.assertTrue(controller.is_running)

                with self.assertRaisesRegex(SerialError, "not connected"):
                    serial.write(b"after close")

            self.assertFalse(controller.is_running)

    def test_raw_bytes_timeout_and_peer_closure(self) -> None:
        with tempfile.TemporaryDirectory() as temporary_directory:
            socket_path = Path(temporary_directory) / "serial.sock"

            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
                listener.bind(str(socket_path))
                listener.listen(1)

                serial = SerialConnection(socket_path)
                with serial:
                    peer, _ = listener.accept()
                    with peer:
                        outgoing = b"\x00host to guest\xff"
                        serial.write(outgoing)
                        self.assertEqual(
                            b"".join(peer.recv(1) for _ in outgoing), outgoing
                        )

                        with self.assertRaises(TimeoutError):
                            serial.read(1, timeout_seconds=0.01)

                        incoming = b"\xfeguest to host\x00"
                        peer.sendall(incoming)
                        self.assertEqual(
                            b"".join(
                                serial.read(1, timeout_seconds=0.5) for _ in incoming
                            ),
                            incoming,
                        )

                    with self.assertRaisesRegex(SerialError, "closed"):
                        serial.read(1, timeout_seconds=0.5)

                with self.assertRaisesRegex(SerialError, "not connected"):
                    serial.read(1, timeout_seconds=0.5)


if __name__ == "__main__":
    unittest.main()

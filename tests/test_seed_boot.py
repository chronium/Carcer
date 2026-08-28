import os
import shutil
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from harness import QemuProcessController, SerialConnection, SerialError, ToolClient


class SeedBootIntegrationTest(unittest.TestCase):
    def test_real_seed_boots_and_lists_no_tools(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        make = shutil.which("make")
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(make, "make must be installed")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")

        build = subprocess.run(
            [make, "seed"],
            cwd=repository,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=60.0,
            check=False,
        )
        self.assertEqual(build.returncode, 0, build.stdout)
        image = repository / "build" / "seed" / "codexos-seed.iso"
        self.assertTrue(image.is_file())

        marker = b"CODEXOS-SEED-READY\n"
        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            serial_socket_path = temporary_path / "serial.sock"
            serial = SerialConnection(serial_socket_path)
            controller = QemuProcessController(qemu)

            with controller:
                controller.start(
                    [
                        "-machine",
                        "q35,accel=kvm:tcg",
                        "-m",
                        "128M",
                        "-cdrom",
                        str(image),
                        "-boot",
                        "order=d",
                        "-display",
                        "none",
                        "-monitor",
                        "none",
                        "-nic",
                        "none",
                        "-no-reboot",
                    ],
                    stdout_path=temporary_path / "qemu.stdout",
                    stderr_path=temporary_path / "qemu.stderr",
                    serial_socket_path=serial_socket_path,
                )
                pid = controller.pid
                self.assertIsNotNone(pid)

                received = bytearray()
                deadline = time.monotonic() + 10.0
                with serial:
                    while marker not in received:
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            self.fail(f"seed marker not received; serial bytes: {received!r}")
                        try:
                            received.extend(
                                serial.read(4096, timeout_seconds=min(0.5, remaining))
                            )
                        except TimeoutError:
                            continue

                    self.assertEqual(ToolClient(serial).list_tools(), [])
                    self.assertTrue(controller.is_running)

                with self.assertRaisesRegex(SerialError, "not connected"):
                    serial.read(1, timeout_seconds=0.1)

            self.assertFalse(controller.is_running)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)


if __name__ == "__main__":
    unittest.main()

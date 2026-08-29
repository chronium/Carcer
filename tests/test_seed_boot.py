import os
import shutil
import struct
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from harness import QemuProcessController, SerialConnection, SerialError, ToolClient


class SeedBootIntegrationTest(unittest.TestCase):
    def test_real_seed_lists_and_reads_its_source(self) -> None:
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

                    client = ToolClient(serial)
                    self.assertEqual(client.list_tools(), ["list", "read"])

                    paths = ["seed/kernel.c", "seed/limine.conf", "seed/linker.ld"]
                    listed = client.invoke_tool("list", [])
                    self.assertEqual(listed.status, 0)
                    self.assertEqual(
                        listed.output,
                        "".join(f"{path}\n" for path in paths).encode(),
                    )

                    prefixed = client.invoke_tool("list", [b"seed/li"])
                    self.assertEqual(prefixed.status, 0)
                    self.assertEqual(
                        prefixed.output,
                        b"seed/limine.conf\nseed/linker.ld\n",
                    )

                    kernel_source = (repository / "seed" / "kernel.c").read_bytes()
                    read_all = client.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            b"0",
                            str(len(kernel_source)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_all.status, 0)
                    self.assertEqual(read_all.output, kernel_source)

                    offset = 37
                    length = 83
                    read_range = client.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            str(offset).encode(),
                            str(length).encode(),
                        ],
                    )
                    self.assertEqual(read_range.status, 0)
                    self.assertEqual(
                        read_range.output,
                        kernel_source[offset : offset + length],
                    )

                    oversized = struct.pack(
                        "<4sHHII", b"CXOS", 1, 0x0001, 6, 16 * 1024 * 1024 + 1
                    )
                    valid_request = struct.pack("<4sHHII", b"CXOS", 1, 0x0001, 7, 0)
                    serial.write(oversized + valid_request)
                    with self.assertRaises(TimeoutError):
                        serial.read(1, timeout_seconds=0.25)

                    self.assertTrue(controller.is_running)

                with self.assertRaisesRegex(SerialError, "not connected"):
                    serial.read(1, timeout_seconds=0.1)

            self.assertFalse(controller.is_running)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)


if __name__ == "__main__":
    unittest.main()

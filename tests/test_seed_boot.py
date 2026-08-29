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
    def test_real_seed_mutates_its_ram_source_store(self) -> None:
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
                    self.assertEqual(
                        client.list_tools(),
                        ["list", "read", "write", "truncate", "remove"],
                    )

                    paths = [
                        "seed/files.c",
                        "seed/files.h",
                        "seed/kernel.c",
                        "seed/limine.conf",
                        "seed/linker.ld",
                        "seed/protocol.c",
                        "seed/protocol.h",
                        "seed/serial.c",
                        "seed/serial.h",
                        "seed/tools.c",
                        "seed/tools.h",
                    ]
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

                    serial_source = (repository / "seed" / "serial.c").read_bytes()
                    read_serial = client.invoke_tool(
                        "read",
                        [
                            b"seed/serial.c",
                            b"0",
                            str(len(serial_source)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_serial.status, 0)
                    self.assertEqual(read_serial.output, serial_source)

                    mutation_offset = len(kernel_source)
                    mutation = b"RAM"
                    write_source = client.invoke_tool(
                        "write",
                        [
                            b"seed/kernel.c",
                            str(mutation_offset).encode("ascii"),
                            mutation,
                        ],
                    )
                    self.assertEqual(write_source.status, 0)
                    self.assertEqual(write_source.output, b"")
                    mutated_kernel = bytearray(kernel_source)
                    mutated_kernel[
                        mutation_offset : mutation_offset + len(mutation)
                    ] = mutation
                    read_mutated = client.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            b"0",
                            str(len(mutated_kernel)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_mutated.status, 0)
                    self.assertEqual(read_mutated.output, mutated_kernel)
                    read_serial_after_growth = client.invoke_tool(
                        "read",
                        [
                            b"seed/serial.c",
                            b"0",
                            str(len(serial_source)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_serial_after_growth.status, 0)
                    self.assertEqual(read_serial_after_growth.output, serial_source)
                    self.assertEqual(
                        (repository / "seed" / "kernel.c").read_bytes(),
                        kernel_source,
                    )

                    scratch_path = "seed/scratch.bin"
                    scratch_data = b"\x00new\xff"
                    create = client.invoke_tool(
                        "write",
                        [scratch_path.encode(), b"0", scratch_data],
                    )
                    self.assertEqual(create.status, 0)
                    self.assertEqual(create.output, b"")

                    listed_with_scratch = client.invoke_tool("list", [])
                    self.assertEqual(listed_with_scratch.status, 0)
                    self.assertEqual(
                        listed_with_scratch.output,
                        "".join(
                            f"{path}\n" for path in sorted(paths + [scratch_path])
                        ).encode(),
                    )

                    read_scratch = client.invoke_tool(
                        "read",
                        [
                            scratch_path.encode(),
                            b"0",
                            str(len(scratch_data)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_scratch.status, 0)
                    self.assertEqual(read_scratch.output, scratch_data)

                    grown_size = len(scratch_data) + 5
                    grow = client.invoke_tool(
                        "truncate",
                        [scratch_path.encode(), str(grown_size).encode("ascii")],
                    )
                    self.assertEqual(grow.status, 0)
                    self.assertEqual(grow.output, b"")
                    grown_data = scratch_data + bytes(5)
                    read_grown = client.invoke_tool(
                        "read",
                        [scratch_path.encode(), b"0", str(grown_size).encode("ascii")],
                    )
                    self.assertEqual(read_grown.status, 0)
                    self.assertEqual(read_grown.output, grown_data)

                    shrink = client.invoke_tool(
                        "truncate",
                        [scratch_path.encode(), b"3"],
                    )
                    self.assertEqual(shrink.status, 0)
                    self.assertEqual(shrink.output, b"")
                    shrunk_data = grown_data[:3]
                    read_shrunk = client.invoke_tool(
                        "read",
                        [scratch_path.encode(), b"0", b"3"],
                    )
                    self.assertEqual(read_shrunk.status, 0)
                    self.assertEqual(read_shrunk.output, shrunk_data)

                    extension = b"tail\x00"
                    append = client.invoke_tool(
                        "write",
                        [scratch_path.encode(), b"3", extension],
                    )
                    self.assertEqual(append.status, 0)
                    self.assertEqual(append.output, b"")
                    extended_data = shrunk_data + extension
                    read_extended = client.invoke_tool(
                        "read",
                        [
                            scratch_path.encode(),
                            b"0",
                            str(len(extended_data)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_extended.status, 0)
                    self.assertEqual(read_extended.output, extended_data)

                    invalid_offset = client.invoke_tool(
                        "write",
                        [
                            scratch_path.encode(),
                            str(len(extended_data) + 1).encode("ascii"),
                            b"x",
                        ],
                    )
                    self.assertNotEqual(invalid_offset.status, 0)
                    self.assertEqual(invalid_offset.output, b"")
                    read_after_failure = client.invoke_tool(
                        "read",
                        [
                            scratch_path.encode(),
                            b"0",
                            str(len(extended_data)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_after_failure.status, 0)
                    self.assertEqual(read_after_failure.output, extended_data)

                    remove = client.invoke_tool(
                        "remove",
                        [scratch_path.encode()],
                    )
                    self.assertEqual(remove.status, 0)
                    self.assertEqual(remove.output, b"")
                    listed_after_remove = client.invoke_tool("list", [])
                    self.assertEqual(listed_after_remove.status, 0)
                    self.assertEqual(
                        listed_after_remove.output,
                        "".join(f"{path}\n" for path in paths).encode(),
                    )
                    read_removed = client.invoke_tool(
                        "read",
                        [scratch_path.encode(), b"0", b"1"],
                    )
                    self.assertNotEqual(read_removed.status, 0)
                    self.assertEqual(read_removed.output, b"")
                    self.assertEqual(
                        (repository / "seed" / "kernel.c").read_bytes(),
                        kernel_source,
                    )

                    oversized = struct.pack(
                        "<4sHHII", b"CXOS", 1, 0x0001, 24, 16 * 1024 * 1024 + 1
                    )
                    valid_request = struct.pack("<4sHHII", b"CXOS", 1, 0x0001, 25, 0)
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

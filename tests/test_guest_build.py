import os
import shutil
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from harness import (
    TEST_HARDWARE_PROFILE,
    CandidateBootValidator,
    CodexOSHostServices,
    QemuProcessController,
    SerialConnection,
    ToolClient,
)

_TOOLS = [
    "list",
    "read",
    "write",
    "truncate",
    "remove",
    "build",
    "finish_generation",
    "request_feature",
]
_READY = b"CODEXOS-SEED-READY\n"


class GuestBuildIntegrationTest(unittest.TestCase):
    def test_real_guest_builds_and_boots_its_mutated_source(self) -> None:
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
        initial_iso = repository / "build" / "seed" / "codexos-seed.iso"
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* CODEXOS-SELF-BUILD-MUTATION */\n"
        invalid_c = b"\nthis is not valid C;\n"

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            host_services = CodexOSHostServices(
                temporary_path / "staging",
                CandidateBootValidator(
                    qemu,
                    TEST_HARDWARE_PROFILE,
                    temporary_parent=temporary_path,
                ),
            )

            first_serial_path = temporary_path / "first-serial.sock"
            first_serial = SerialConnection(first_serial_path)
            first_controller = QemuProcessController(qemu)
            with first_controller:
                first_controller.start(
                    _qemu_arguments(initial_iso),
                    stdout_path=temporary_path / "first-qemu.stdout",
                    stderr_path=temporary_path / "first-qemu.stderr",
                    serial_socket_path=first_serial_path,
                )
                first_pid = first_controller.pid
                self.assertIsNotNone(first_pid)

                with first_serial:
                    _wait_for_ready(first_serial)
                    client = ToolClient(first_serial, host_services)
                    self.assertEqual(client.list_tools(), _TOOLS)

                    append = client.invoke_tool(
                        "write",
                        [
                            b"seed/kernel.c",
                            str(len(original_kernel)).encode("ascii"),
                            mutation,
                        ],
                    )
                    self.assertEqual(append.status, 0)

                    ignored = client.invoke_tool(
                        "write",
                        [b"seed/../not-buildable.c", b"0", b"not valid C"],
                    )
                    self.assertEqual(ignored.status, 0)

                    success = client.invoke_tool("build", [])
                    self.assertEqual(success.status, 0, success.output.decode())
                    successful_artifacts = host_services.latest_successful_build
                    self.assertIsNotNone(successful_artifacts)
                    self.assertTrue(successful_artifacts.kernel_elf.is_file())
                    self.assertTrue(successful_artifacts.iso.is_file())

                    invalid_offset = len(original_kernel) + len(mutation)
                    break_source = client.invoke_tool(
                        "write",
                        [
                            b"seed/kernel.c",
                            str(invalid_offset).encode("ascii"),
                            invalid_c,
                        ],
                    )
                    self.assertEqual(break_source.status, 0)

                    failure = client.invoke_tool("build", [])
                    self.assertEqual(failure.status, 1)
                    self.assertIn(b"kernel.c", failure.output)
                    self.assertIn(b"error:", failure.output)
                    self.assertEqual(
                        host_services.latest_successful_build,
                        successful_artifacts,
                    )
                    self.assertTrue(successful_artifacts.iso.is_file())

                    repair = client.invoke_tool(
                        "truncate",
                        [b"seed/kernel.c", str(invalid_offset).encode("ascii")],
                    )
                    self.assertEqual(repair.status, 0)
                    self.assertTrue(first_controller.is_running)
                    self.assertEqual(
                        (repository / "seed" / "kernel.c").read_bytes(),
                        original_kernel,
                    )

            self.assertFalse(first_controller.is_running)
            with self.assertRaises(ProcessLookupError):
                os.kill(first_pid, 0)

            second_serial_path = temporary_path / "second-serial.sock"
            second_serial = SerialConnection(second_serial_path)
            second_controller = QemuProcessController(qemu)
            with second_controller:
                second_controller.start(
                    _qemu_arguments(successful_artifacts.iso),
                    stdout_path=temporary_path / "second-qemu.stdout",
                    stderr_path=temporary_path / "second-qemu.stderr",
                    serial_socket_path=second_serial_path,
                )
                second_pid = second_controller.pid
                self.assertIsNotNone(second_pid)

                with second_serial:
                    _wait_for_ready(second_serial)
                    successor = ToolClient(second_serial)
                    self.assertEqual(successor.list_tools(), _TOOLS)
                    expected_kernel = original_kernel + mutation
                    read_kernel = successor.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            b"0",
                            str(len(expected_kernel)).encode("ascii"),
                        ],
                    )
                    self.assertEqual(read_kernel.status, 0)
                    self.assertEqual(read_kernel.output, expected_kernel)
                    listed = successor.invoke_tool("list", [b"seed/build"])
                    self.assertEqual(listed.status, 0)
                    self.assertEqual(listed.output, b"seed/build.c\nseed/build.h\n")
                    self.assertTrue(second_controller.is_running)

            self.assertFalse(second_controller.is_running)
            with self.assertRaises(ProcessLookupError):
                os.kill(second_pid, 0)
            self.assertEqual(
                (repository / "seed" / "kernel.c").read_bytes(),
                original_kernel,
            )


def _qemu_arguments(image: Path) -> list[str]:
    return [
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
    ]


def _wait_for_ready(serial: SerialConnection) -> None:
    received = bytearray()
    deadline = time.monotonic() + 10.0
    while _READY not in received:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise AssertionError(f"seed marker not received: {received!r}")
        try:
            received.extend(serial.read(4096, min(0.5, remaining)))
        except TimeoutError:
            continue


if __name__ == "__main__":
    unittest.main()

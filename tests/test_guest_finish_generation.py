import os
import shutil
import subprocess
import tempfile
import time
import unittest
from contextlib import contextmanager
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


class GuestFinishGenerationIntegrationTest(unittest.TestCase):
    def test_accepts_finish_for_the_exact_successful_source(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* FINISH-GENERATION-SUCCESS */\n"
        handoff = "Continue from this successor.\nKeep the source intact."

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            host_services = CodexOSHostServices(
                temporary_path / "staging",
                _candidate_validator(temporary_path),
            )
            with _running_guest(image, temporary_path, "accepted") as (
                controller,
                serial,
            ):
                client = ToolClient(serial, host_services)
                self.assertEqual(client.list_tools(), _TOOLS)

                invalid_utf8 = client.invoke_tool("finish_generation", [b"\xff"])
                self.assertEqual(invalid_utf8.status, 1)
                self.assertEqual(invalid_utf8.output, b"")
                oversized = client.invoke_tool(
                    "finish_generation",
                    [b"x" * (16 * 1024 + 1)],
                )
                self.assertEqual(oversized.status, 1)
                self.assertEqual(oversized.output, b"")
                self.assertIsNone(host_services.pending_generation_finish)

                write = client.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel)).encode("ascii"),
                        mutation,
                    ],
                )
                self.assertEqual(write.status, 0)
                build = client.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                artifacts = host_services.latest_successful_build
                self.assertIsNotNone(artifacts)

                finish = client.invoke_tool(
                    "finish_generation",
                    [handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0)
                self.assertEqual(finish.output, b"")
                pending = host_services.pending_generation_finish
                self.assertIsNotNone(pending)
                self.assertEqual(pending.handoff_message, handoff)
                self.assertEqual(pending.source_snapshot, artifacts.source_snapshot)
                self.assertEqual(pending.kernel_elf, artifacts.kernel_elf)
                self.assertEqual(pending.iso, artifacts.iso)
                self.assertEqual(client.list_tools(), _TOOLS)
                self.assertTrue(controller.is_running)

        self.assertEqual(
            (repository / "seed" / "kernel.c").read_bytes(),
            original_kernel,
        )

    def test_rejects_finish_after_source_changes_since_build(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        built_mutation = b"\n/* FINISH-GENERATION-BUILT */\n"
        later_mutation = b"\n/* FINISH-GENERATION-LATER-EDIT */\n"

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            host_services = CodexOSHostServices(
                temporary_path / "staging",
                _candidate_validator(temporary_path),
            )
            with _running_guest(image, temporary_path, "mismatch") as (
                controller,
                serial,
            ):
                client = ToolClient(serial, host_services)
                first_write = client.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel)).encode("ascii"),
                        built_mutation,
                    ],
                )
                self.assertEqual(first_write.status, 0)
                build = client.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                artifacts = host_services.latest_successful_build
                self.assertIsNotNone(artifacts)

                second_write = client.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel) + len(built_mutation)).encode("ascii"),
                        later_mutation,
                    ],
                )
                self.assertEqual(second_write.status, 0)
                finish = client.invoke_tool(
                    "finish_generation",
                    [b"Do not discard the later edit."],
                )
                self.assertEqual(finish.status, 1)
                self.assertIn(
                    b"current source differs from the latest successful build",
                    finish.output,
                )
                self.assertIsNone(host_services.pending_generation_finish)
                self.assertIs(host_services.latest_successful_build, artifacts)
                self.assertTrue(controller.is_running)

        self.assertEqual(
            (repository / "seed" / "kernel.c").read_bytes(),
            original_kernel,
        )


def _build_seed(repository: Path) -> Path:
    make = shutil.which("make")
    if make is None:
        raise AssertionError("make must be installed")
    build = subprocess.run(
        [make, "seed"],
        cwd=repository,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=60.0,
        check=False,
    )
    if build.returncode != 0:
        raise AssertionError(build.stdout)
    return repository / "build" / "seed" / "codexos-seed.iso"


@contextmanager
def _running_guest(image: Path, temporary: Path, name: str):
    qemu = shutil.which("qemu-system-x86_64")
    if qemu is None:
        raise AssertionError("qemu-system-x86_64 must be installed")
    serial_path = temporary / f"{name}-serial.sock"
    serial = SerialConnection(serial_path)
    controller = QemuProcessController(qemu)
    with controller:
        controller.start(
            _qemu_arguments(image),
            stdout_path=temporary / f"{name}-qemu.stdout",
            stderr_path=temporary / f"{name}-qemu.stderr",
            serial_socket_path=serial_path,
        )
        pid = controller.pid
        if pid is None:
            raise AssertionError("QEMU did not expose a PID")
        with serial:
            _wait_for_ready(serial)
            yield controller, serial
    if controller.is_running:
        raise AssertionError("QEMU remained alive after cleanup")
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        pass
    else:
        raise AssertionError("QEMU PID remained alive after cleanup")


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


def _candidate_validator(temporary: Path) -> CandidateBootValidator:
    qemu = shutil.which("qemu-system-x86_64")
    if qemu is None:
        raise AssertionError("qemu-system-x86_64 must be installed")
    return CandidateBootValidator(
        qemu,
        TEST_HARDWARE_PROFILE,
        temporary_parent=temporary,
    )


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

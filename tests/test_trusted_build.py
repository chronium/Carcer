import os
import shutil
import tempfile
import time
import unittest
from pathlib import Path

from harness import (
    BuildStatus,
    QemuProcessController,
    SerialConnection,
    SnapshotFile,
    ToolClient,
    build_source_snapshot,
    encode_source_snapshot,
)


class TrustedBuildIntegrationTests(unittest.TestCase):
    def test_builds_and_boots_current_guest_snapshot(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        files = _current_seed_files(repository)
        original = {entry.path: entry.content for entry in files}
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            output = temporary_path / "output"
            result = build_source_snapshot(encode_source_snapshot(files), output)

            self.assertEqual(result.status, BuildStatus.SUCCESS, result.diagnostics)
            self.assertIsNotNone(result.kernel_elf)
            self.assertIsNotNone(result.iso)
            self.assertEqual(
                {path.name for path in output.iterdir()},
                {"kernel.elf", "codexos.iso"},
            )
            self.assertEqual(
                {entry.path: (repository / entry.path).read_bytes() for entry in files},
                original,
            )

            serial_path = temporary_path / "serial.sock"
            serial = SerialConnection(serial_path)
            controller = QemuProcessController(qemu)
            marker = b"CODEXOS-SEED-READY\n"
            with controller:
                controller.start(
                    [
                        "-machine",
                        "q35,accel=kvm:tcg",
                        "-m",
                        "128M",
                        "-cdrom",
                        str(result.iso),
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
                    serial_socket_path=serial_path,
                )
                pid = controller.pid
                self.assertIsNotNone(pid)

                received = bytearray()
                deadline = time.monotonic() + 10.0
                with serial:
                    while marker not in received:
                        remaining = deadline - time.monotonic()
                        if remaining <= 0:
                            self.fail(f"built seed marker not received: {received!r}")
                        try:
                            received.extend(
                                serial.read(4096, timeout_seconds=min(0.5, remaining))
                            )
                        except TimeoutError:
                            continue

                    client = ToolClient(serial)
                    self.assertEqual(
                        client.list_tools(),
                        [
                            "list",
                            "read",
                            "write",
                            "truncate",
                            "remove",
                            "build",
                            "finish_generation",
                            "request_feature",
                        ],
                    )
                    listed = client.invoke_tool("list", [])
                    self.assertEqual(listed.status, 0)
                    expected_paths = sorted(original)
                    self.assertEqual(
                        listed.output,
                        "".join(f"{path}\n" for path in expected_paths).encode(),
                    )
                    kernel = client.invoke_tool(
                        "read",
                        [
                            b"seed/kernel.c",
                            b"0",
                            str(len(original["seed/kernel.c"])).encode("ascii"),
                        ],
                    )
                    self.assertEqual(kernel.status, 0)
                    self.assertEqual(kernel.output, original["seed/kernel.c"])
                    self.assertTrue(controller.is_running)

            self.assertFalse(controller.is_running)
            with self.assertRaises(ProcessLookupError):
                os.kill(pid, 0)

    def test_returns_bounded_diagnostics_for_invalid_guest_c(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        files = list(_current_seed_files(repository))
        kernel = next(
            index
            for index, entry in enumerate(files)
            if entry.path == "seed/kernel.c"
        )
        files[kernel] = SnapshotFile("seed/kernel.c", b"this is not valid C;\n")

        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            result = build_source_snapshot(encode_source_snapshot(files), output)

            self.assertEqual(result.status, BuildStatus.BUILD_FAILURE)
            self.assertIn("kernel.c", result.diagnostics)
            self.assertIn("error:", result.diagnostics)
            self.assertLessEqual(len(result.diagnostics), 64 * 1024)
            self.assertFalse(output.exists())

    def test_sandbox_blocks_assembler_and_linker_filesystem_escape(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        current_files = _current_seed_files(repository)

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            binary_secret = temporary_path / "outside-secret.bin"
            binary_secret.write_bytes(b"UNIQUE-CODEXOS-SANDBOX-SECRET")
            linker_include = temporary_path / "outside-secret.ld"
            linker_include.write_text(
                "PROVIDE(codexos_outside_secret = 0x1234);\n",
                encoding="ascii",
            )

            assembly_files = current_files + (
                SnapshotFile(
                    "seed/escape.S",
                    (
                        ".section .rodata\n"
                        f'.incbin "{binary_secret}"\n'
                    ).encode("utf-8"),
                ),
            )
            linker_files = tuple(
                SnapshotFile(
                    entry.path,
                    (
                        f'INCLUDE "{linker_include}"\n'.encode("utf-8")
                        + entry.content
                    )
                    if entry.path == "seed/linker.ld"
                    else entry.content,
                )
                for entry in current_files
            )

            for name, files, external_path in (
                ("assembler", assembly_files, binary_secret),
                ("linker", linker_files, linker_include),
            ):
                with self.subTest(consumer=name):
                    output = temporary_path / f"{name}-output"
                    result = build_source_snapshot(
                        encode_source_snapshot(files),
                        output,
                    )

                    self.assertEqual(result.status, BuildStatus.BUILD_FAILURE)
                    self.assertIn(str(external_path), result.diagnostics)
                    self.assertFalse(output.exists())


def _current_seed_files(repository: Path) -> tuple[SnapshotFile, ...]:
    paths = (
        "seed/build.c",
        "seed/build.h",
        "seed/files.c",
        "seed/files.h",
        "seed/kernel.c",
        "seed/limine.conf",
        "seed/linker.ld",
        "seed/protocol.c",
        "seed/protocol.h",
        "seed/serial.c",
        "seed/serial.h",
        "seed/source_snapshot.c",
        "seed/source_snapshot.h",
        "seed/tools.c",
        "seed/tools.h",
    )
    return tuple(
        SnapshotFile(path, (repository / path).read_bytes()) for path in paths
    )


if __name__ == "__main__":
    unittest.main()

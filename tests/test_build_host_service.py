import struct
import tempfile
import unittest
from pathlib import Path

from harness import (
    BuildHostService,
    HostServiceRequest,
    SnapshotFile,
    encode_source_snapshot,
)


class BuildHostServiceIntegrationTests(unittest.TestCase):
    def test_stages_success_and_keeps_it_after_compile_failure(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        files = _current_seed_files(repository)

        with tempfile.TemporaryDirectory() as temporary:
            service = BuildHostService(Path(temporary) / "staging")
            success = service.handle_request(
                HostServiceRequest(
                    41,
                    "build",
                    (encode_source_snapshot(files),),
                )
            )

            success_status, success_diagnostics = _response(success.payload)
            self.assertEqual(success.message_type, 0x8003)
            self.assertEqual(success.request_id, 41)
            self.assertEqual(success_status, 0, success_diagnostics.decode())
            artifacts = service.latest_successful_build
            self.assertIsNotNone(artifacts)
            self.assertTrue(artifacts.kernel_elf.is_file())
            self.assertTrue(artifacts.iso.is_file())
            self.assertEqual(artifacts.kernel_elf.name, "kernel.elf")
            self.assertEqual(artifacts.iso.name, "codexos.iso")

            broken_files = tuple(
                SnapshotFile(entry.path, b"this is not valid C;\n")
                if entry.path == "seed/kernel.c"
                else entry
                for entry in files
            )
            failure = service.handle_request(
                HostServiceRequest(
                    42,
                    "build",
                    (encode_source_snapshot(broken_files),),
                )
            )

            failure_status, failure_diagnostics = _response(failure.payload)
            self.assertEqual(failure.request_id, 42)
            self.assertEqual(failure_status, 1)
            self.assertIn(b"kernel.c", failure_diagnostics)
            self.assertIn(b"error:", failure_diagnostics)
            self.assertLessEqual(len(failure_diagnostics), 64 * 1024)
            self.assertEqual(service.latest_successful_build, artifacts)
            self.assertTrue(artifacts.kernel_elf.is_file())
            self.assertTrue(artifacts.iso.is_file())

    def test_invalid_snapshot_and_unknown_service_return_failures(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        valid_snapshot = encode_source_snapshot(_current_seed_files(repository))

        with tempfile.TemporaryDirectory() as temporary:
            staging = Path(temporary) / "staging"
            service = BuildHostService(staging)

            invalid = service.handle_request(
                HostServiceRequest(50, "build", (b"\x01",))
            )
            invalid_status, invalid_diagnostics = _response(invalid.payload)
            self.assertEqual(invalid_status, 2)
            self.assertIn(b"truncated", invalid_diagnostics)
            self.assertIsNone(service.latest_successful_build)

            attempts_before_unknown = tuple(staging.iterdir())
            unknown = service.handle_request(
                HostServiceRequest(51, "unknown", (valid_snapshot,))
            )
            unknown_status, _ = _response(unknown.payload)
            self.assertNotEqual(unknown_status, 0)
            self.assertEqual(tuple(staging.iterdir()), attempts_before_unknown)
            self.assertIsNone(service.latest_successful_build)


def _response(payload: bytes) -> tuple[int, bytes]:
    return struct.unpack_from("<I", payload)[0], payload[4:]


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
        "seed/tools.c",
        "seed/tools.h",
    )
    return tuple(
        SnapshotFile(path, (repository / path).read_bytes()) for path in paths
    )


if __name__ == "__main__":
    unittest.main()

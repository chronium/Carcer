import struct
import tempfile
import unittest
from pathlib import Path

from harness import (
    BuildHostService,
    CodexOSHostServices,
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
            self.assertEqual(artifacts.source_snapshot, encode_source_snapshot(files))

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


class GenerationFinishHostServiceIntegrationTests(unittest.TestCase):
    def test_accepts_matching_build_and_freezes_selected_successor(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        snapshot = encode_source_snapshot(_current_seed_files(repository))

        with tempfile.TemporaryDirectory() as temporary:
            service = CodexOSHostServices(Path(temporary) / "staging")
            build = service.handle_request(
                HostServiceRequest(60, "build", (snapshot,))
            )
            build_status, build_diagnostics = _response(build.payload)
            self.assertEqual(build_status, 0, build_diagnostics.decode())
            artifacts = service.latest_successful_build
            self.assertIsNotNone(artifacts)

            handoff = "Continue from the validated successor. λ"
            finish = service.handle_request(
                HostServiceRequest(
                    61,
                    "finish_generation",
                    (handoff.encode("utf-8"), snapshot),
                )
            )
            self.assertEqual(_response(finish.payload), (0, b""))
            pending = service.pending_generation_finish
            self.assertIsNotNone(pending)
            self.assertEqual(pending.handoff_message, handoff)
            self.assertEqual(pending.source_snapshot, snapshot)
            self.assertEqual(pending.kernel_elf, artifacts.kernel_elf)
            self.assertEqual(pending.iso, artifacts.iso)

            second_finish = service.handle_request(
                HostServiceRequest(62, "finish_generation", (b"replacement", snapshot))
            )
            self.assertEqual(_response(second_finish.payload)[0], 2)
            later_build = service.handle_request(
                HostServiceRequest(63, "build", (snapshot,))
            )
            self.assertNotEqual(_response(later_build.payload)[0], 0)
            self.assertIs(service.pending_generation_finish, pending)
            self.assertIs(service.latest_successful_build, artifacts)

    def test_rejects_invalid_or_unbuilt_source_and_preserves_prior_build(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        files = _current_seed_files(repository)
        snapshot = encode_source_snapshot(files)

        with tempfile.TemporaryDirectory() as temporary:
            no_build = CodexOSHostServices(Path(temporary) / "no-build")
            no_success = no_build.handle_request(
                HostServiceRequest(70, "finish_generation", (b"handoff", snapshot))
            )
            self.assertEqual(_response(no_success.payload)[0], 1)
            self.assertIsNone(no_build.pending_generation_finish)

            invalid_utf8 = no_build.handle_request(
                HostServiceRequest(71, "finish_generation", (b"\xff", snapshot))
            )
            self.assertEqual(_response(invalid_utf8.payload)[0], 2)
            oversized = no_build.handle_request(
                HostServiceRequest(
                    72,
                    "finish_generation",
                    (b"x" * (16 * 1024 + 1), snapshot),
                )
            )
            self.assertEqual(_response(oversized.payload)[0], 2)
            malformed = no_build.handle_request(
                HostServiceRequest(73, "finish_generation", (b"", b"\x01"))
            )
            self.assertEqual(_response(malformed.payload)[0], 2)
            self.assertIsNone(no_build.pending_generation_finish)

            service = CodexOSHostServices(Path(temporary) / "staging")
            success = service.handle_request(
                HostServiceRequest(74, "build", (snapshot,))
            )
            success_status, success_diagnostics = _response(success.payload)
            self.assertEqual(success_status, 0, success_diagnostics.decode())
            artifacts = service.latest_successful_build
            self.assertIsNotNone(artifacts)

            changed_snapshot = encode_source_snapshot(
                files + (SnapshotFile("seed/after-build.txt", b"new edit"),)
            )
            mismatch = service.handle_request(
                HostServiceRequest(
                    75,
                    "finish_generation",
                    (b"handoff", changed_snapshot),
                )
            )
            self.assertEqual(_response(mismatch.payload)[0], 1)
            self.assertIsNone(service.pending_generation_finish)

            broken_files = tuple(
                SnapshotFile(entry.path, b"this is not valid C;\n")
                if entry.path == "seed/kernel.c"
                else entry
                for entry in files
            )
            failed_build = service.handle_request(
                HostServiceRequest(
                    76,
                    "build",
                    (encode_source_snapshot(broken_files),),
                )
            )
            failed_status, failed_diagnostics = _response(failed_build.payload)
            self.assertEqual(failed_status, 1)
            self.assertIn(b"error:", failed_diagnostics)
            self.assertIs(service.latest_successful_build, artifacts)
            self.assertEqual(artifacts.source_snapshot, snapshot)

            finish = service.handle_request(
                HostServiceRequest(77, "finish_generation", (b"handoff", snapshot))
            )
            self.assertEqual(_response(finish.payload), (0, b""))
            self.assertIsNotNone(service.pending_generation_finish)


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

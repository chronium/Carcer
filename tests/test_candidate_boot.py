import os
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from harness import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
    EXPERIMENT_HARDWARE_PROFILE,
    TEST_HARDWARE_PROFILE,
    BuildHostService,
    BuildStatus,
    CandidateBootValidator,
    CodexOSHostServices,
    HostServiceRequest,
    QemuProcessController,
    SerialConnection,
    SnapshotFile,
    build_source_snapshot,
    encode_source_snapshot,
)
from harness.guest_startup import GuestReadyError, wait_for_ready
from harness.tool_protocol import ToolClient
from tests.test_build_host_service import _current_seed_files, _response


class CandidateBootValidationIntegrationTests(unittest.TestCase):
    def test_successful_candidate_boots_speaks_protocol_and_is_cleaned_up(
        self,
    ) -> None:
        repository = Path(__file__).resolve().parents[1]
        qemu = _qemu()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            activity = CodexActivityStream()
            build = build_source_snapshot(
                encode_source_snapshot(_current_seed_files(repository)),
                root / "compiled",
            )
            self.assertEqual(build.status, BuildStatus.SUCCESS, build.diagnostics)
            validator = CandidateBootValidator(
                qemu,
                TEST_HARDWARE_PROFILE,
                temporary_parent=root,
                activity_stream=activity,
                generation=4,
            )

            pids, result = _validate_tracking_process(validator, build.iso)

            self.assertEqual(result.status, BuildStatus.SUCCESS, result.diagnostics)
            events = activity.drain()
            self.assertEqual(
                [event.kind for event in events],
                [
                    CodexActivityKind.BUILD_CANDIDATE_STARTED,
                    CodexActivityKind.BUILD_CANDIDATE_READY,
                    CodexActivityKind.BUILD_PROTOCOL_VALIDATED,
                ],
            )
            self.assertTrue(all(event.generation == 4 for event in events))
            self.assertTrue(
                all(event.role is CodexActivityRole.HARNESS for event in events)
            )
            _assert_candidate_cleanup(self, root, pids)

    def test_compile_valid_nonbooting_and_protocol_broken_candidates_fail(
        self,
    ) -> None:
        repository = Path(__file__).resolve().parents[1]
        qemu = _qemu()
        files = _current_seed_files(repository)
        nonbooting = _replace_kernel(
            files,
            b'"CODEXOS-SEED-READY\\n"',
            b'"CODEXOS-NOT-READY\\n"',
        )
        protocol_broken = _replace_kernel(
            files,
            b"    protocol_loop();\n",
            (
                b'    static const uint8_t broken[] = "BROKEN-PROTOCOL!";\n'
                b"    serial_write_bytes(broken, sizeof(broken) - 1u);\n"
                b"    for (;;) { __asm__ volatile(\"pause\"); }\n"
            ),
        )
        early_exit = _replace_kernel(
            files,
            b"    serial_write_bytes(ready, sizeof(ready) - 1u);\n",
            b'    (void)ready;\n    __asm__ volatile("ud2");\n',
        )

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name, candidate, expected in (
                ("no-ready", nonbooting, "timed out waiting"),
                ("bad-protocol", protocol_broken, "list-tools exchange failed"),
                ("early-exit", early_exit, "before CODEXOS-SEED-READY"),
            ):
                with self.subTest(candidate=name):
                    activity = CodexActivityStream()
                    build = build_source_snapshot(
                        encode_source_snapshot(candidate),
                        root / f"compiled-{name}",
                    )
                    self.assertEqual(
                        build.status,
                        BuildStatus.SUCCESS,
                        build.diagnostics,
                    )
                    validator = CandidateBootValidator(
                        qemu,
                        TEST_HARDWARE_PROFILE,
                        ready_timeout_seconds=0.25,
                        temporary_parent=root,
                        activity_stream=activity,
                        generation=5,
                    )
                    pids, result = _validate_tracking_process(
                        validator,
                        build.iso,
                    )
                    self.assertEqual(
                        result.status,
                        BuildStatus.BUILD_FAILURE,
                        result.diagnostics,
                    )
                    self.assertIn("Trusted compilation succeeded", result.diagnostics)
                    self.assertIn(expected, result.diagnostics)
                    if name == "no-ready":
                        self.assertIn(
                            "Candidate serial before failure",
                            result.diagnostics,
                        )
                        self.assertIn(
                            r"CODEXOS-NOT-READY\n",
                            result.diagnostics,
                        )
                    self.assertIs(
                        activity.drain()[-1].kind,
                        CodexActivityKind.BUILD_CANDIDATE_FAILED,
                    )
                    _assert_candidate_cleanup(self, root, pids)

    def test_later_boot_failure_preserves_previous_success_and_blocks_finish(
        self,
    ) -> None:
        repository = Path(__file__).resolve().parents[1]
        qemu = _qemu()
        valid_snapshot = encode_source_snapshot(_current_seed_files(repository))
        invalid_snapshot = encode_source_snapshot(
            _replace_kernel(
                _current_seed_files(repository),
                b'"CODEXOS-SEED-READY\\n"',
                b'"CODEXOS-NOT-READY\\n"',
            )
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            services = CodexOSHostServices(
                root / "staging",
                CandidateBootValidator(
                    qemu,
                    TEST_HARDWARE_PROFILE,
                    ready_timeout_seconds=0.25,
                    temporary_parent=root,
                ),
            )
            first = services.handle_request(
                HostServiceRequest(1, "build", (valid_snapshot,))
            )
            self.assertEqual(_response(first.payload)[0], 0)
            successful = services.latest_successful_build
            self.assertIsNotNone(successful)

            failed = services.handle_request(
                HostServiceRequest(2, "build", (invalid_snapshot,))
            )
            status, diagnostics = _response(failed.payload)
            self.assertEqual(status, 1)
            self.assertIn(b"Candidate boot validation failed", diagnostics)
            self.assertIs(services.latest_successful_build, successful)

            finish = services.handle_request(
                HostServiceRequest(
                    3,
                    "finish_generation",
                    (b"must not finish this candidate", invalid_snapshot),
                )
            )
            self.assertEqual(_response(finish.payload)[0], 1)
            self.assertIsNone(services.pending_generation_finish)

    def test_validator_infrastructure_failure_is_status_two(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        snapshot = encode_source_snapshot(_current_seed_files(repository))
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            service = BuildHostService(
                root / "staging",
                CandidateBootValidator(
                    str(root / "missing-qemu"),
                    TEST_HARDWARE_PROFILE,
                    temporary_parent=root,
                ),
            )

            response = service.handle_request(
                HostServiceRequest(1, "build", (snapshot,))
            )

            status, diagnostics = _response(response.payload)
            self.assertEqual(status, 2)
            self.assertIn(b"could not start candidate QEMU", diagnostics)
            self.assertIsNone(service.latest_successful_build)
            self.assertEqual(list(root.glob("codexos-candidate-*")), [])

    def test_candidate_cleanup_survives_an_interrupting_host_exception(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            build = build_source_snapshot(
                encode_source_snapshot(_current_seed_files(repository)),
                root / "compiled",
            )
            self.assertEqual(build.status, BuildStatus.SUCCESS, build.diagnostics)
            validator = CandidateBootValidator(
                _qemu(),
                TEST_HARDWARE_PROFILE,
                temporary_parent=root,
            )
            pids: list[int] = []
            original_start = QemuProcessController.start

            def start(controller, *args, **kwargs):
                original_start(controller, *args, **kwargs)
                if controller.pid is not None:
                    pids.append(controller.pid)

            with patch.object(QemuProcessController, "start", new=start):
                with patch.object(
                    ToolClient,
                    "list_tools",
                    side_effect=KeyboardInterrupt,
                ):
                    with self.assertRaises(KeyboardInterrupt):
                        validator.validate(build.iso)
            _assert_candidate_cleanup(self, root, pids)


class GuestReadinessDiagnosticsTests(unittest.TestCase):
    def test_timeout_includes_bounded_terminal_safe_serial(self) -> None:
        serial = Mock(spec=SerialConnection)
        calls = 0

        def read(_size: int, _timeout: float) -> bytes:
            nonlocal calls
            calls += 1
            if calls == 1:
                return b"PRE-READY\n\x1b[2J\x00"
            raise TimeoutError

        serial.read.side_effect = read
        with self.assertRaises(GuestReadyError) as caught:
            wait_for_ready(serial, 0.01)

        message = str(caught.exception)
        self.assertIn("timed out waiting for CODEXOS-SEED-READY", message)
        self.assertIn(r"PRE-READY\n\x1b[2J\x00", message)
        self.assertNotIn("\x1b", message)


@unittest.skipUnless(
    shutil.which("qemu-system-x86_64")
    and Path("/dev/kvm").exists()
    and os.access("/dev/kvm", os.R_OK | os.W_OK),
    "experiment candidate validation requires accessible /dev/kvm",
)
class CandidateBootExperimentKvmIntegrationTest(unittest.TestCase):
    def test_seed_candidate_validates_under_exact_experiment_profile(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            build = build_source_snapshot(
                encode_source_snapshot(_current_seed_files(repository)),
                root / "compiled",
            )
            self.assertEqual(build.status, BuildStatus.SUCCESS, build.diagnostics)
            validator = CandidateBootValidator(
                _qemu(),
                EXPERIMENT_HARDWARE_PROFILE,
                temporary_parent=root,
            )

            pids, result = _validate_tracking_process(validator, build.iso)

            self.assertEqual(result.status, BuildStatus.SUCCESS, result.diagnostics)
            _assert_candidate_cleanup(self, root, pids)


def _replace_kernel(
    files: tuple[SnapshotFile, ...],
    old: bytes,
    new: bytes,
) -> tuple[SnapshotFile, ...]:
    replaced = False
    result: list[SnapshotFile] = []
    for entry in files:
        if entry.path == "seed/kernel.c":
            if old not in entry.content:
                raise AssertionError(f"kernel fixture does not contain {old!r}")
            result.append(SnapshotFile(entry.path, entry.content.replace(old, new, 1)))
            replaced = True
        else:
            result.append(entry)
    if not replaced:
        raise AssertionError("kernel fixture is missing")
    return tuple(result)


def _validate_tracking_process(
    validator: CandidateBootValidator,
    iso: Path | None,
):
    if iso is None:
        raise AssertionError("trusted compilation returned no ISO")
    pids: list[int] = []
    original_start = QemuProcessController.start

    def start(controller, *args, **kwargs):
        original_start(controller, *args, **kwargs)
        if controller.pid is not None:
            pids.append(controller.pid)

    with patch.object(QemuProcessController, "start", new=start):
        result = validator.validate(iso)
    return pids, result


def _assert_candidate_cleanup(
    test: unittest.TestCase,
    root: Path,
    pids: list[int],
) -> None:
    test.assertTrue(pids)
    for pid in pids:
        with test.assertRaises(ProcessLookupError):
            os.kill(pid, 0)
    test.assertEqual(list(root.glob("codexos-candidate-*")), [])


def _qemu() -> str:
    executable = shutil.which("qemu-system-x86_64")
    if executable is None:
        raise AssertionError("qemu-system-x86_64 must be installed")
    return executable


if __name__ == "__main__":
    unittest.main()

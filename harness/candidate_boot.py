"""Trusted ephemeral boot validation for one built CodexOS candidate."""

from __future__ import annotations

import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .framing import FramingError
from .guest_startup import (
    GuestReadyError,
    escape_diagnostic_bytes,
    wait_for_ready,
)
from .hardware import CodexOSHardwareProfile
from .qemu import QemuProcessController
from .qmp import QmpClient, QmpError
from .serial import SerialConnection, SerialError
from .tool_protocol import ToolClient, ToolProtocolError
from .trusted_build import BuildStatus

_DEFAULT_READY_TIMEOUT_SECONDS = 10.0
_QEMU_EXIT_TIMEOUT_SECONDS = 5.0
_MAX_DIAGNOSTICS = 64 * 1024


@dataclass(frozen=True, slots=True)
class CandidateBootResult:
    status: BuildStatus
    diagnostics: str


class CandidateBootValidator:
    """Boot one exact candidate ISO and verify its canonical tool protocol."""

    def __init__(
        self,
        qemu_executable: str,
        hardware_profile: CodexOSHardwareProfile,
        *,
        ready_timeout_seconds: float = _DEFAULT_READY_TIMEOUT_SECONDS,
        temporary_parent: str | Path | None = None,
    ) -> None:
        if ready_timeout_seconds <= 0:
            raise ValueError("candidate readiness timeout must be positive")
        self._qemu_executable = qemu_executable
        self._hardware_profile = hardware_profile
        self._ready_timeout_seconds = ready_timeout_seconds
        self._temporary_parent = (
            None if temporary_parent is None else Path(temporary_parent)
        )

    def validate(self, candidate_iso: str | Path) -> CandidateBootResult:
        """Return guest failure or harness failure without leaking QEMU state."""
        try:
            workspace = tempfile.TemporaryDirectory(
                prefix="codexos-candidate-",
                dir=self._temporary_parent,
            )
        except OSError as error:
            return _harness_failure(f"could not create candidate workspace: {error}")

        result: CandidateBootResult | None = None
        cleanup_error: str | None = None
        try:
            result, cleanup_error = self._validate_in_workspace(
                Path(workspace.name),
                Path(candidate_iso),
            )
        finally:
            try:
                workspace.cleanup()
            except OSError as error:
                cleanup_error = f"could not remove candidate workspace: {error}"

        if cleanup_error is not None:
            return _harness_failure(cleanup_error)
        if result is None:
            raise RuntimeError("candidate validator produced no result")
        return result

    def _validate_in_workspace(
        self,
        workspace: Path,
        candidate_iso: Path,
    ) -> tuple[CandidateBootResult, str | None]:
        qmp_path = workspace / "qmp.sock"
        serial_path = workspace / "serial.sock"
        qmp = QmpClient(qmp_path)
        serial = SerialConnection(serial_path)
        controller = QemuProcessController(self._qemu_executable)
        qmp_connected = False
        result: CandidateBootResult

        try:
            try:
                self._hardware_profile.require_available()
                arguments = self._hardware_profile.qemu_arguments(
                    candidate_iso,
                    qmp_path,
                    serial_path,
                )
                # Establish trusted control before allowing untrusted guest
                # execution, so launch failures remain distinguishable from
                # a candidate that exits during boot.
                arguments.append("-S")
                controller.start(
                    arguments,
                    stdout_path=workspace / "qemu.stdout",
                    stderr_path=workspace / "qemu.stderr",
                )
            except (OSError, RuntimeError, subprocess.SubprocessError) as error:
                result = _harness_failure(
                    f"could not start candidate QEMU: {error}"
                )
            else:
                try:
                    qmp.connect()
                    qmp_connected = True
                    serial.connect()
                except (OSError, QmpError, SerialError) as error:
                    result = _harness_failure(
                        f"could not establish candidate QEMU control: {error}"
                    )
                else:
                    try:
                        qmp.cont()
                    except (OSError, QmpError) as error:
                        if controller.is_running:
                            result = _harness_failure(
                                f"could not start candidate execution: {error}"
                            )
                        else:
                            result = _guest_failure(
                                "candidate QEMU exited before "
                                "CODEXOS-SEED-READY"
                            )
                    else:
                        result = self._validate_guest(serial)
        finally:
            cleanup_errors: list[str] = []
            try:
                serial.close()
            except OSError as error:
                cleanup_errors.append(f"serial close failed: {error}")
            if qmp_connected:
                try:
                    qmp.quit()
                except (OSError, QmpError):
                    # The bounded process-controller fallback below remains
                    # authoritative when the guest has already exited or QMP
                    # cannot complete a graceful quit.
                    pass
            try:
                qmp.close()
            except OSError:
                # QmpClient clears and closes the underlying socket in its
                # own finally path; a dead guest may make stream flush fail.
                pass
            try:
                controller.stop(timeout_seconds=_QEMU_EXIT_TIMEOUT_SECONDS)
            except (OSError, subprocess.SubprocessError) as error:
                cleanup_errors.append(f"QEMU termination failed: {error}")
            if controller.is_running:
                cleanup_errors.append("candidate QEMU remained running")

        cleanup_error = "; ".join(cleanup_errors) if cleanup_errors else None
        return result, cleanup_error

    def _validate_guest(self, serial: SerialConnection) -> CandidateBootResult:
        try:
            wait_for_ready(serial, self._ready_timeout_seconds)
        except GuestReadyError as error:
            detail = error.reason
            if error.serial_output:
                detail += (
                    "\nCandidate serial before failure:\n"
                    + escape_diagnostic_bytes(error.serial_output)
                )
            return _guest_failure(detail)

        try:
            ToolClient(serial).list_tools()
        except (
            FramingError,
            OSError,
            SerialError,
            TimeoutError,
            ToolProtocolError,
        ) as error:
            return _guest_failure(
                "canonical list-tools exchange failed: " + str(error)
            )
        except RuntimeError as error:
            return _harness_failure(
                "candidate protocol validator failed internally: " + str(error)
            )
        return CandidateBootResult(BuildStatus.SUCCESS, "")


def _guest_failure(detail: str) -> CandidateBootResult:
    return CandidateBootResult(
        BuildStatus.BUILD_FAILURE,
        _bounded(
            "Trusted compilation succeeded.\n"
            "Candidate boot validation failed:\n"
            + detail
        ),
    )


def _harness_failure(detail: str) -> CandidateBootResult:
    return CandidateBootResult(
        BuildStatus.HARNESS_FAILURE,
        _bounded(
            "Trusted compilation succeeded.\n"
            "Candidate boot validation could not run safely:\n"
            + detail
        ),
    )


def _bounded(value: str) -> str:
    encoded = value.encode("utf-8", errors="replace")
    return encoded[:_MAX_DIAGNOSTICS].decode("utf-8", errors="ignore")

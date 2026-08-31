"""Trusted ephemeral boot validation for one built CodexOS candidate."""

from __future__ import annotations

import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .codex_activity import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
    publish_activity,
)
from .framing import FramingError
from .forensic_provenance import BuildAttemptEvidence, FileIdentity
from .guest_startup import (
    GuestReadyError,
    escape_diagnostic_bytes,
    wait_for_ready,
)
from .hardware import CodexOSHardwareProfile
from .qemu import QemuProcessController
from .qmp import QmpClient, QmpError
from .provided_assets import ProvidedAssets
from .serial import SerialConnection, SerialError
from .serial_protocol import SerialProtocolDispatcher
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
        activity_stream: CodexActivityStream | None = None,
        generation: int | None = None,
        provided_assets: ProvidedAssets | None = None,
    ) -> None:
        if ready_timeout_seconds <= 0:
            raise ValueError("candidate readiness timeout must be positive")
        self._qemu_executable = qemu_executable
        self._hardware_profile = hardware_profile
        self._ready_timeout_seconds = ready_timeout_seconds
        self._temporary_parent = (
            None if temporary_parent is None else Path(temporary_parent)
        )
        self._activity_stream = activity_stream
        self._generation = generation
        self._provided_assets = provided_assets

    def validate(
        self,
        candidate_iso: str | Path,
        *,
        evidence: BuildAttemptEvidence | None = None,
        iso_identity: FileIdentity | None = None,
    ) -> CandidateBootResult:
        """Return guest failure or harness failure without leaking QEMU state."""
        identity_data = _identity_data(evidence, iso_identity)
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_candidate_validation_started",
                "candidate_started",
                expected_iso_sha256=iso_identity.sha256 if iso_identity else None,
                expected_iso_bytes=iso_identity.size if iso_identity else None,
            )
        self._publish(CodexActivityKind.BUILD_CANDIDATE_STARTED, identity_data)
        try:
            workspace = tempfile.TemporaryDirectory(
                prefix="codexos-candidate-",
                dir=self._temporary_parent,
            )
        except OSError as error:
            result = _harness_failure(
                f"could not create candidate workspace: {error}"
            )
            self._publish_candidate_failure(result, identity_data)
            if evidence is not None:
                evidence.record_candidate_stage(
                    "build_candidate_validation_completed",
                    "candidate_completed",
                    outcome=result.status.value,
                )
            return result

        result: CandidateBootResult | None = None
        cleanup_error: str | None = None
        try:
            result, cleanup_error = self._validate_in_workspace(
                Path(workspace.name),
                Path(candidate_iso),
                evidence,
                identity_data,
            )
        except BaseException as error:
            self._publish(
                CodexActivityKind.BUILD_CANDIDATE_FAILED,
                {
                    "result": "exception",
                    "error": type(error).__name__,
                    **identity_data,
                },
            )
            raise
        finally:
            try:
                workspace.cleanup()
            except OSError as error:
                cleanup_error = f"could not remove candidate workspace: {error}"

        if cleanup_error is not None:
            result = _harness_failure(cleanup_error)
            self._publish_candidate_failure(result, identity_data)
            if evidence is not None:
                evidence.record_candidate_stage(
                    "build_candidate_validation_completed",
                    "candidate_completed",
                    outcome=result.status.value,
                )
            return result
        if result is None:
            raise RuntimeError("candidate validator produced no result")
        if result.status is not BuildStatus.SUCCESS:
            self._publish_candidate_failure(result, identity_data)
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_candidate_validation_completed",
                "candidate_completed",
                outcome=result.status.value,
            )
        return result

    def _validate_in_workspace(
        self,
        workspace: Path,
        candidate_iso: Path,
        evidence: BuildAttemptEvidence | None,
        identity_data: dict[str, object],
    ) -> tuple[CandidateBootResult, str | None]:
        qmp_path = workspace / "qmp.sock"
        serial_path = workspace / "serial.sock"
        qmp = QmpClient(qmp_path)
        serial = SerialConnection(serial_path)
        protocol: SerialProtocolDispatcher | None = None
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
                if evidence is not None:
                    evidence.record_candidate_stage(
                        "build_candidate_qemu_started",
                        "candidate_qemu_started",
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
                        protocol = SerialProtocolDispatcher(
                            serial,
                            startup_host_services=self._provided_assets,
                            background_host_services=self._provided_assets,
                            exchange_host_services=self._provided_assets,
                        )
                        result = self._validate_guest(
                            protocol,
                            evidence,
                            identity_data,
                        )
        finally:
            cleanup_errors: list[str] = []
            try:
                if protocol is None:
                    serial.close()
                else:
                    protocol.close()
            except (OSError, SerialError) as error:
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

    def _validate_guest(
        self,
        protocol: SerialProtocolDispatcher,
        evidence: BuildAttemptEvidence | None = None,
        identity_data: dict[str, object] | None = None,
    ) -> CandidateBootResult:
        try:
            wait_for_ready(
                protocol,
                self._ready_timeout_seconds,
            )
        except GuestReadyError as error:
            detail = error.reason
            if error.serial_output:
                detail += (
                    "\nCandidate serial before failure:\n"
                    + escape_diagnostic_bytes(error.serial_output)
                )
            return _guest_failure(detail)
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_candidate_ready_observed",
                "ready_observed",
                ready=True,
            )
        self._publish(
            CodexActivityKind.BUILD_CANDIDATE_READY,
            identity_data,
        )

        if evidence is not None:
            evidence.record_candidate_stage(
                "build_protocol_validation_started",
                "protocol_validation_started",
            )
        try:
            client = ToolClient(protocol)
            client.list_tools()
        except (
            FramingError,
            OSError,
            SerialError,
            TimeoutError,
            ToolProtocolError,
        ) as error:
            if evidence is not None:
                evidence.record_candidate_stage(
                    "build_protocol_validation_completed",
                    "protocol_validation_failed",
                    outcome="build_failure",
                )
            return _guest_failure(
                "canonical list-tools exchange failed: " + str(error)
            )
        except RuntimeError as error:
            if evidence is not None:
                evidence.record_candidate_stage(
                    "build_protocol_validation_completed",
                    "protocol_validation_failed",
                    outcome="harness_failure",
                )
            return _harness_failure(
                "candidate protocol validator failed internally: " + str(error)
            )
        if evidence is not None:
            evidence.record_candidate_stage(
                "build_protocol_validation_completed",
                "protocol_validated",
                outcome="success",
                protocol_validated=True,
            )
        self._publish(CodexActivityKind.BUILD_PROTOCOL_VALIDATED, identity_data)
        return CandidateBootResult(BuildStatus.SUCCESS, "")

    def _publish_candidate_failure(
        self,
        result: CandidateBootResult,
        identity_data: dict[str, object] | None = None,
    ) -> None:
        self._publish(
            CodexActivityKind.BUILD_CANDIDATE_FAILED,
            {"result": result.status.value, **(identity_data or {})},
        )

    def _publish(
        self,
        kind: CodexActivityKind,
        data: dict[str, object] | None = None,
    ) -> None:
        publish_activity(
            self._activity_stream,
            self._generation,
            CodexActivityRole.HARNESS,
            kind,
            data,
        )


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


def _identity_data(
    evidence: BuildAttemptEvidence | None,
    iso_identity: FileIdentity | None,
) -> dict[str, object]:
    data: dict[str, object] = {}
    if evidence is not None:
        data["build_attempt_id"] = evidence.attempt_id
    if iso_identity is not None:
        data["iso_sha256"] = iso_identity.sha256
        data["iso_bytes"] = iso_identity.size
    return data

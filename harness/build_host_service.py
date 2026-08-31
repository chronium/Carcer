"""Concrete host-side implementation of the CodexOS build service."""

from __future__ import annotations

import tempfile
from dataclasses import dataclass
from pathlib import Path

from .candidate_boot import CandidateBootValidator
from .codex_activity import (
    CodexActivityKind,
    CodexActivityRole,
    CodexActivityStream,
    publish_activity,
)
from .framing import Frame
from .forensic_provenance import (
    BuildAttemptEvidence,
    BuildReviewProvenance,
    FileIdentity,
    ForensicProvenanceError,
)
from .host_service_protocol import (
    HostServiceRequest,
    create_host_service_response,
)
from .trusted_build import BuildStatus, build_source_snapshot
from .source_snapshot import SourceSnapshotError, decode_source_snapshot

_BUILD_SUCCESS = 0
_BUILD_FAILURE = 1
_BUILD_HARNESS_FAILURE = 2


@dataclass(frozen=True, slots=True)
class StagedBuildArtifacts:
    kernel_elf: Path
    iso: Path
    source_snapshot: bytes
    build_attempt_id: str | None = None
    source_identity: FileIdentity | None = None
    kernel_identity: FileIdentity | None = None
    iso_identity: FileIdentity | None = None
    evidence: BuildAttemptEvidence | None = None


class BuildHostService:
    """Synchronously service build requests into trusted staging storage."""

    def __init__(
        self,
        staging_directory: str | Path,
        candidate_validator: CandidateBootValidator,
        *,
        activity_stream: CodexActivityStream | None = None,
        generation: int | None = None,
        provenance: BuildReviewProvenance | None = None,
    ) -> None:
        self._staging_directory = Path(staging_directory)
        self._staging_directory.mkdir(parents=True, exist_ok=True)
        self._candidate_validator = candidate_validator
        self._activity_stream = activity_stream
        self._generation = generation
        self._provenance = provenance
        self._latest_successful_build: StagedBuildArtifacts | None = None

    @property
    def latest_successful_build(self) -> StagedBuildArtifacts | None:
        return self._latest_successful_build

    def handle_request(self, request: HostServiceRequest) -> Frame:
        if request.service_name != "build":
            return create_host_service_response(
                request.request_id,
                1,
                f"unknown host service: {request.service_name}".encode("utf-8"),
            )
        evidence: BuildAttemptEvidence | None = None
        if self._provenance is not None:
            if self._generation is None:
                raise RuntimeError("build provenance requires a generation")
            snapshot = request.arguments[0] if len(request.arguments) == 1 else None
            try:
                evidence = self._provenance.begin_build(self._generation, snapshot)
            except ForensicProvenanceError:
                return create_host_service_response(
                    request.request_id,
                    _BUILD_HARNESS_FAILURE,
                    b"cannot record trusted build provenance",
                )
        activity_data = (
            {"build_attempt_id": evidence.attempt_id}
            if evidence is not None
            else None
        )
        self._publish(CodexActivityKind.BUILD_STARTED, activity_data)
        if len(request.arguments) != 1:
            if evidence is not None:
                try:
                    evidence.record_final("harness_failure")
                except ForensicProvenanceError:
                    pass
            self._publish(
                CodexActivityKind.BUILD_COMPLETED,
                {"status": _BUILD_HARNESS_FAILURE, **(activity_data or {})},
            )
            return create_host_service_response(
                request.request_id,
                _BUILD_HARNESS_FAILURE,
                b"build expects exactly one source snapshot argument",
            )

        try:
            attempt = Path(
                tempfile.mkdtemp(prefix="build-attempt-", dir=self._staging_directory)
            )
        except OSError:
            if evidence is not None:
                try:
                    evidence.record_final("harness_failure")
                except ForensicProvenanceError:
                    pass
            self._publish(
                CodexActivityKind.BUILD_COMPLETED,
                {"status": _BUILD_HARNESS_FAILURE, **(activity_data or {})},
            )
            return create_host_service_response(
                request.request_id,
                _BUILD_HARNESS_FAILURE,
                b"cannot create build attempt storage",
            )
        snapshot = request.arguments[0]
        if evidence is not None:
            try:
                decoded = decode_source_snapshot(snapshot)
            except SourceSnapshotError:
                pass
            else:
                try:
                    evidence.record_decoded(
                        len(decoded),
                        sum(len(entry.content) for entry in decoded),
                    )
                except ForensicProvenanceError:
                    return self._provenance_failure(request, activity_data)
        result = build_source_snapshot(snapshot, attempt)
        self._publish(
            CodexActivityKind.BUILD_COMPILE_COMPLETED,
            {"result": result.status.value, **(activity_data or {})},
        )
        if result.status is BuildStatus.SUCCESS:
            if result.kernel_elf is None or result.iso is None:
                if evidence is not None:
                    try:
                        evidence.record_final("harness_failure")
                    except ForensicProvenanceError:
                        pass
                self._publish(
                    CodexActivityKind.BUILD_COMPLETED,
                    {"status": _BUILD_HARNESS_FAILURE, **(activity_data or {})},
                )
                return create_host_service_response(
                    request.request_id,
                    _BUILD_HARNESS_FAILURE,
                    b"trusted build returned no artifacts",
                )
            try:
                kernel_identity = FileIdentity.from_path(result.kernel_elf)
                iso_identity = FileIdentity.from_path(result.iso)
                if evidence is not None:
                    evidence.record_artifacts(kernel_identity, iso_identity)
                validation = self._candidate_validator.validate(
                    result.iso,
                    evidence=evidence,
                    iso_identity=iso_identity,
                )
            except (OSError, ForensicProvenanceError):
                self._publish(
                    CodexActivityKind.BUILD_COMPLETED,
                    {"status": _BUILD_HARNESS_FAILURE, **(activity_data or {})},
                )
                return create_host_service_response(
                    request.request_id,
                    _BUILD_HARNESS_FAILURE,
                    b"cannot record trusted build artifact provenance",
                )
            if validation.status is BuildStatus.SUCCESS:
                if evidence is not None:
                    try:
                        evidence.record_latest_success(snapshot)
                    except ForensicProvenanceError:
                        return self._provenance_failure(request, activity_data)
                self._latest_successful_build = StagedBuildArtifacts(
                    result.kernel_elf,
                    result.iso,
                    snapshot,
                    evidence.attempt_id if evidence is not None else None,
                    evidence.source_identity if evidence is not None else None,
                    kernel_identity,
                    iso_identity,
                    evidence,
                )
                if evidence is not None:
                    evidence.record_latest_success_update()
                status = _BUILD_SUCCESS
            elif validation.status is BuildStatus.BUILD_FAILURE:
                status = _BUILD_FAILURE
            else:
                status = _BUILD_HARNESS_FAILURE
            diagnostics = validation.diagnostics.encode("utf-8")
        elif result.status is BuildStatus.BUILD_FAILURE:
            status = _BUILD_FAILURE
            diagnostics = result.diagnostics.encode("utf-8")
            if evidence is not None:
                try:
                    evidence.record_compile_failure("build_failure")
                except ForensicProvenanceError:
                    return self._provenance_failure(request, activity_data)
        else:
            status = _BUILD_HARNESS_FAILURE
            diagnostics = result.diagnostics.encode("utf-8")
            if evidence is not None:
                try:
                    evidence.record_compile_failure("harness_failure")
                except ForensicProvenanceError:
                    return self._provenance_failure(request, activity_data)

        if evidence is not None and result.status is BuildStatus.SUCCESS:
            if status != _BUILD_SUCCESS:
                try:
                    evidence.record_final(
                        "build_failure"
                        if status == _BUILD_FAILURE
                        else "harness_failure"
                    )
                except ForensicProvenanceError:
                    return self._provenance_failure(request, activity_data)

        self._publish(
            CodexActivityKind.BUILD_COMPLETED,
            {"status": status, **(activity_data or {})},
        )
        return create_host_service_response(
            request.request_id,
            status,
            diagnostics,
        )

    def _provenance_failure(
        self,
        request: HostServiceRequest,
        activity_data: dict[str, object] | None,
    ) -> Frame:
        self._publish(
            CodexActivityKind.BUILD_COMPLETED,
            {"status": _BUILD_HARNESS_FAILURE, **(activity_data or {})},
        )
        return create_host_service_response(
            request.request_id,
            _BUILD_HARNESS_FAILURE,
            b"cannot record trusted build provenance",
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

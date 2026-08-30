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
from .host_service_protocol import (
    HostServiceRequest,
    create_host_service_response,
)
from .trusted_build import BuildStatus, build_source_snapshot

_BUILD_SUCCESS = 0
_BUILD_FAILURE = 1
_BUILD_HARNESS_FAILURE = 2


@dataclass(frozen=True, slots=True)
class StagedBuildArtifacts:
    kernel_elf: Path
    iso: Path
    source_snapshot: bytes


class BuildHostService:
    """Synchronously service build requests into trusted staging storage."""

    def __init__(
        self,
        staging_directory: str | Path,
        candidate_validator: CandidateBootValidator,
        *,
        activity_stream: CodexActivityStream | None = None,
        generation: int | None = None,
    ) -> None:
        self._staging_directory = Path(staging_directory)
        self._staging_directory.mkdir(parents=True, exist_ok=True)
        self._candidate_validator = candidate_validator
        self._activity_stream = activity_stream
        self._generation = generation
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
        self._publish(CodexActivityKind.BUILD_STARTED)
        if len(request.arguments) != 1:
            self._publish(
                CodexActivityKind.BUILD_COMPLETED,
                {"status": _BUILD_HARNESS_FAILURE},
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
            self._publish(
                CodexActivityKind.BUILD_COMPLETED,
                {"status": _BUILD_HARNESS_FAILURE},
            )
            return create_host_service_response(
                request.request_id,
                _BUILD_HARNESS_FAILURE,
                b"cannot create build attempt storage",
            )
        result = build_source_snapshot(request.arguments[0], attempt)
        self._publish(
            CodexActivityKind.BUILD_COMPILE_COMPLETED,
            {"result": result.status.value},
        )
        if result.status is BuildStatus.SUCCESS:
            if result.kernel_elf is None or result.iso is None:
                self._publish(
                    CodexActivityKind.BUILD_COMPLETED,
                    {"status": _BUILD_HARNESS_FAILURE},
                )
                return create_host_service_response(
                    request.request_id,
                    _BUILD_HARNESS_FAILURE,
                    b"trusted build returned no artifacts",
                )
            validation = self._candidate_validator.validate(result.iso)
            if validation.status is BuildStatus.SUCCESS:
                self._latest_successful_build = StagedBuildArtifacts(
                    result.kernel_elf,
                    result.iso,
                    request.arguments[0],
                )
                status = _BUILD_SUCCESS
            elif validation.status is BuildStatus.BUILD_FAILURE:
                status = _BUILD_FAILURE
            else:
                status = _BUILD_HARNESS_FAILURE
            diagnostics = validation.diagnostics.encode("utf-8")
        elif result.status is BuildStatus.BUILD_FAILURE:
            status = _BUILD_FAILURE
            diagnostics = result.diagnostics.encode("utf-8")
        else:
            status = _BUILD_HARNESS_FAILURE
            diagnostics = result.diagnostics.encode("utf-8")

        self._publish(
            CodexActivityKind.BUILD_COMPLETED,
            {"status": status},
        )
        return create_host_service_response(
            request.request_id,
            status,
            diagnostics,
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

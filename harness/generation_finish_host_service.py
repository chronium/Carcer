"""Concrete CodexOS host services and pending generation finish state."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

from .build_host_service import BuildHostService, StagedBuildArtifacts
from .framing import Frame
from .host_service_protocol import (
    HostServiceRequest,
    create_host_service_response,
)
from .source_snapshot import SourceSnapshotError, decode_source_snapshot

_FINISH_ACCEPTED = 0
_FINISH_REJECTED = 1
_FINISH_HARNESS_FAILURE = 2
_MAX_HANDOFF_SIZE = 16 * 1024


@dataclass(frozen=True, slots=True)
class PendingGenerationFinish:
    handoff_message: str
    source_snapshot: bytes
    kernel_elf: Path
    iso: Path


class CodexOSHostServices:
    """Synchronously dispatch the concrete CodexOS host services."""

    def __init__(self, staging_directory: str | Path) -> None:
        self._build_service = BuildHostService(staging_directory)
        self._pending_finish: PendingGenerationFinish | None = None

    @property
    def latest_successful_build(self) -> StagedBuildArtifacts | None:
        return self._build_service.latest_successful_build

    @property
    def pending_generation_finish(self) -> PendingGenerationFinish | None:
        return self._pending_finish

    def handle_request(self, request: HostServiceRequest) -> Frame:
        if request.service_name == "build":
            if self._pending_finish is not None:
                return create_host_service_response(
                    request.request_id,
                    2,
                    b"build rejected after generation finish was accepted",
                )
            return self._build_service.handle_request(request)
        if request.service_name == "finish_generation":
            return self._finish_generation(request)
        return create_host_service_response(
            request.request_id,
            1,
            f"unknown host service: {request.service_name}".encode("utf-8"),
        )

    def _finish_generation(self, request: HostServiceRequest) -> Frame:
        if self._pending_finish is not None:
            return self._finish_response(
                request,
                _FINISH_REJECTED,
                b"generation finish has already been accepted",
            )
        if len(request.arguments) != 2:
            return self._finish_response(
                request,
                _FINISH_HARNESS_FAILURE,
                b"finish_generation expects a handoff and source snapshot",
            )

        encoded_handoff, source_snapshot = request.arguments
        if len(encoded_handoff) > _MAX_HANDOFF_SIZE:
            return self._finish_response(
                request,
                _FINISH_HARNESS_FAILURE,
                b"handoff message exceeds 16 KiB",
            )
        try:
            handoff = encoded_handoff.decode("utf-8")
        except UnicodeDecodeError:
            return self._finish_response(
                request,
                _FINISH_HARNESS_FAILURE,
                b"handoff message is not valid UTF-8",
            )
        try:
            decode_source_snapshot(source_snapshot)
        except SourceSnapshotError as error:
            return self._finish_response(
                request,
                _FINISH_HARNESS_FAILURE,
                str(error).encode("utf-8"),
            )

        successful_build = self._build_service.latest_successful_build
        if successful_build is None:
            return self._finish_response(
                request,
                _FINISH_REJECTED,
                b"no successful build is available",
            )
        if source_snapshot != successful_build.source_snapshot:
            return self._finish_response(
                request,
                _FINISH_REJECTED,
                b"current source differs from the latest successful build",
            )

        self._pending_finish = PendingGenerationFinish(
            handoff,
            source_snapshot,
            successful_build.kernel_elf,
            successful_build.iso,
        )
        return self._finish_response(request, _FINISH_ACCEPTED, b"")

    @staticmethod
    def _finish_response(
        request: HostServiceRequest,
        status: int,
        output: bytes,
    ) -> Frame:
        return create_host_service_response(request.request_id, status, output)

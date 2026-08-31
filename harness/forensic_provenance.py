"""Immutable run-local evidence for trusted builds and reviewer source reads."""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .observability import ExperimentObservability

_SCHEMA_VERSION = 1


class ForensicProvenanceError(RuntimeError):
    """Trusted forensic evidence could not be recorded safely."""


@dataclass(frozen=True, slots=True)
class FileIdentity:
    sha256: str
    size: int

    @classmethod
    def from_path(cls, path: Path) -> "FileIdentity":
        digest = hashlib.sha256()
        size = 0
        with path.open("rb") as input_file:
            while chunk := input_file.read(1024 * 1024):
                digest.update(chunk)
                size += len(chunk)
        return cls(digest.hexdigest(), size)

    def as_json(self) -> dict[str, object]:
        return {"sha256": self.sha256, "size": self.size}


class BuildReviewProvenance:
    """Allocate immutable identities and atomically update their evidence."""

    def __init__(
        self,
        run_directory: str | Path,
        observability: ExperimentObservability | None = None,
    ) -> None:
        self._root = Path(run_directory) / "build-review-provenance"
        self._observability = observability
        self._lock = threading.Lock()

    def begin_build(
        self,
        generation: int,
        snapshot: bytes | None,
    ) -> "BuildAttemptEvidence":
        directory, attempt_id = self._allocate(generation, "build")
        manifest: dict[str, Any] = {
            "schema_version": _SCHEMA_VERSION,
            "kind": "build_attempt",
            "generation": generation,
            "attempt_id": attempt_id,
            "stage": "received",
            "outcome": "incomplete",
        }
        if snapshot is not None:
            manifest["source_snapshot"] = {
                "sha256": hashlib.sha256(snapshot).hexdigest(),
                "size": len(snapshot),
                "decoded": False,
            }
        _atomic_json(directory / "manifest.json", manifest)
        evidence = BuildAttemptEvidence(
            directory,
            manifest,
            self._observability,
        )
        evidence.record_event("build_attempt_received")
        return evidence

    def begin_review(self, generation: int) -> "ReviewEvidence":
        directory, review_id = self._allocate(generation, "review")
        manifest: dict[str, Any] = {
            "schema_version": _SCHEMA_VERSION,
            "kind": "review",
            "generation": generation,
            "review_id": review_id,
            "stage": "started",
            "outcome": "incomplete",
            "source_reads": [],
        }
        _atomic_json(directory / "manifest.json", manifest)
        return ReviewEvidence(directory, manifest, self._observability)

    def _allocate(self, generation: int, kind: str) -> tuple[Path, str]:
        generation_directory = self._root / f"generation-{generation:04d}"
        try:
            generation_directory.mkdir(parents=True, exist_ok=True)
            if (
                generation_directory.is_symlink()
                or not generation_directory.is_dir()
            ):
                raise ForensicProvenanceError(
                    "generation provenance directory is unsafe"
                )
            with self._lock:
                sequence = self._next_sequence(
                    generation_directory,
                    generation,
                    kind,
                )
                while True:
                    identifier = f"{kind}-{sequence:06d}"
                    directory = generation_directory / identifier
                    try:
                        directory.mkdir()
                    except FileExistsError:
                        sequence += 1
                        continue
                    return directory, identifier
        except OSError as error:
            raise ForensicProvenanceError(
                f"cannot allocate {kind} provenance: {error}"
            ) from error

    @staticmethod
    def _next_sequence(
        generation_directory: Path,
        generation: int,
        kind: str,
    ) -> int:
        highest = 0
        for directory in generation_directory.glob(f"{kind}-*"):
            prefix = f"{kind}-"
            suffix = directory.name.removeprefix(prefix)
            if (
                directory.is_symlink()
                or not directory.is_dir()
                or len(suffix) != 6
                or not suffix.isascii()
                or not suffix.isdecimal()
                or int(suffix) < 1
            ):
                raise ForensicProvenanceError(
                    f"malformed {kind} provenance entry: {directory.name}"
                )
            highest = max(highest, int(suffix))
            manifest_path = directory / "manifest.json"
            if not manifest_path.exists():
                continue
            try:
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
                raise ForensicProvenanceError(
                    f"malformed {kind} provenance manifest: {directory.name}"
                ) from error
            identifier_key = "attempt_id" if kind == "build" else "review_id"
            expected_kind = "build_attempt" if kind == "build" else "review"
            if (
                not isinstance(manifest, dict)
                or manifest.get("schema_version") != _SCHEMA_VERSION
                or manifest.get("kind") != expected_kind
                or manifest.get("generation") != generation
                or manifest.get(identifier_key) != directory.name
            ):
                raise ForensicProvenanceError(
                    f"unsupported or inconsistent {kind} provenance: "
                    f"{directory.name}"
                )
        return highest + 1


class BuildAttemptEvidence:
    def __init__(
        self,
        directory: Path,
        manifest: dict[str, Any],
        observability: ExperimentObservability | None,
    ) -> None:
        self._directory = directory
        self._manifest = manifest
        self._observability = observability

    @property
    def attempt_id(self) -> str:
        return str(self._manifest["attempt_id"])

    @property
    def generation(self) -> int:
        return int(self._manifest["generation"])

    @property
    def source_identity(self) -> FileIdentity:
        value = self._manifest.get("source_snapshot")
        if not isinstance(value, dict):
            raise ForensicProvenanceError("build attempt has no source identity")
        return FileIdentity(str(value["sha256"]), int(value["size"]))

    @property
    def kernel_identity(self) -> FileIdentity:
        return self._file_identity("kernel")

    @property
    def iso_identity(self) -> FileIdentity:
        return self._file_identity("iso")

    def record_decoded(self, file_count: int, content_size: int) -> None:
        source = self._manifest.get("source_snapshot")
        if not isinstance(source, dict):
            raise ForensicProvenanceError("decoded build has no source snapshot")
        source.update(
            {"decoded": True, "file_count": file_count, "content_size": content_size}
        )
        self._update("decoded")
        self.record_event("build_attempt_decoded")

    def record_compile_failure(self, outcome: str) -> None:
        self._manifest["compile"] = {"outcome": outcome}
        self._update("compilation_completed")
        self.record_event("build_compilation_completed", {"outcome": outcome})
        self.record_final(outcome)

    def record_artifacts(
        self,
        kernel: FileIdentity,
        iso: FileIdentity,
    ) -> None:
        self._manifest["compile"] = {"outcome": "success"}
        self._update("compilation_completed")
        self.record_event("build_compilation_completed", {"outcome": "success"})
        self._manifest["artifacts"] = {
            "kernel": kernel.as_json(),
            "iso": iso.as_json(),
        }
        self._update("artifacts_produced")
        self.record_event("build_artifacts_produced")

    def record_candidate_stage(
        self,
        event: str,
        stage: str,
        **data: object,
    ) -> None:
        candidate = self._manifest.setdefault("candidate_validation", {})
        if not isinstance(candidate, dict):
            raise ForensicProvenanceError("candidate provenance is malformed")
        candidate["stage"] = stage
        candidate.update(data)
        self._update(stage)
        self.record_event(event, data)

    def record_final(self, outcome: str) -> None:
        self._complete(outcome)
        self.record_event("build_attempt_completed", {"outcome": outcome})

    def record_latest_success(self, snapshot: bytes) -> None:
        source_path = self._directory / "source.snapshot"
        _atomic_bytes(source_path, snapshot)
        self._manifest["outcome"] = "success"
        self._manifest["source_snapshot_file"] = "source.snapshot"
        self._manifest["latest_success"] = {
            "ready": True,
            "protocol_validated": True,
            "source_snapshot": self.source_identity.as_json(),
            "kernel": self.kernel_identity.as_json(),
            "iso": self.iso_identity.as_json(),
        }
        self._update("latest_success")
        self.record_event("build_attempt_completed", {"outcome": "success"})

    def record_latest_success_update(self) -> None:
        self.record_event("build_latest_success_updated")

    def aborted_archive_manifest(self) -> dict[str, object]:
        latest = self._manifest.get("latest_success")
        if not isinstance(latest, dict):
            raise ForensicProvenanceError("build is not a latest success")
        return {
            "schema_version": _SCHEMA_VERSION,
            "generation": self.generation,
            "build_attempt_id": self.attempt_id,
            "source_snapshot": latest["source_snapshot"],
            "kernel": latest["kernel"],
            "iso": latest["iso"],
            "ready": True,
            "protocol_validated": True,
        }

    def record_event(
        self,
        event: str,
        extra: dict[str, object] | None = None,
    ) -> None:
        if self._observability is None:
            return
        data: dict[str, object] = {
            "build_attempt_id": self.attempt_id,
            "stage": self._manifest["stage"],
        }
        source = self._manifest.get("source_snapshot")
        if isinstance(source, dict):
            data.update(
                {
                    "source_snapshot_sha256": source["sha256"],
                    "source_snapshot_bytes": source["size"],
                }
            )
        artifacts = self._manifest.get("artifacts")
        if isinstance(artifacts, dict):
            for name in ("kernel", "iso"):
                identity = artifacts.get(name)
                if isinstance(identity, dict):
                    data[f"{name}_sha256"] = identity["sha256"]
                    data[f"{name}_bytes"] = identity["size"]
        if extra:
            data.update(extra)
        self._observability.record(event, self.generation, data)

    def _file_identity(self, name: str) -> FileIdentity:
        artifacts = self._manifest.get("artifacts")
        value = artifacts.get(name) if isinstance(artifacts, dict) else None
        if not isinstance(value, dict):
            raise ForensicProvenanceError(f"build attempt has no {name} identity")
        return FileIdentity(str(value["sha256"]), int(value["size"]))

    def _complete(self, outcome: str) -> None:
        self._manifest["outcome"] = outcome
        self._update("completed")

    def _update(self, stage: str) -> None:
        self._manifest["stage"] = stage
        _atomic_json(self._directory / "manifest.json", self._manifest)


class ReviewEvidence:
    def __init__(
        self,
        directory: Path,
        manifest: dict[str, Any],
        observability: ExperimentObservability | None,
    ) -> None:
        self._directory = directory
        self._manifest = manifest
        self._observability = observability

    @property
    def review_id(self) -> str:
        return str(self._manifest["review_id"])

    @property
    def generation(self) -> int:
        return int(self._manifest["generation"])

    def record_source_read(
        self,
        path: str,
        offset: int,
        length: int,
        status: int,
        output: bytes,
    ) -> None:
        reads = self._manifest["source_reads"]
        sequence = len(reads) + 1
        filename = f"read-{sequence:06d}.bin"
        _atomic_bytes(self._directory / filename, output)
        reads.append(
            {
                "sequence": sequence,
                "path": path,
                "offset": offset,
                "length": length,
                "status": status,
                "returned_bytes": len(output),
                "sha256": hashlib.sha256(output).hexdigest(),
                "content_file": filename,
            }
        )
        self._manifest["stage"] = "source_read"
        _atomic_json(self._directory / "manifest.json", self._manifest)
        if self._observability is not None:
            self._observability.record(
                "review_source_read",
                self.generation,
                {
                    "review_id": self.review_id,
                    "source_path": path,
                    "offset": offset,
                    "length": length,
                    "status": status,
                    "returned_bytes": len(output),
                    "returned_sha256": hashlib.sha256(output).hexdigest(),
                },
            )

    def complete(self, outcome: str) -> None:
        self._manifest["stage"] = "completed"
        self._manifest["outcome"] = outcome
        _atomic_json(self._directory / "manifest.json", self._manifest)


def _atomic_json(path: Path, value: object) -> None:
    _atomic_bytes(
        path,
        (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8"),
    )


def _atomic_bytes(path: Path, value: bytes) -> None:
    temporary: Path | None = None
    try:
        descriptor, name = tempfile.mkstemp(prefix=f".{path.name}-", dir=path.parent)
        temporary = Path(name)
        with os.fdopen(descriptor, "wb") as output:
            output.write(value)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        temporary = None
    except OSError as error:
        raise ForensicProvenanceError(
            f"cannot write forensic provenance {path.name}: {error}"
        ) from error
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)

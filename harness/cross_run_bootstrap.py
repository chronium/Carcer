"""Immutable cross-run continuation bootstrap provenance."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .feature_requests import FeatureRequest, FeatureRequestStore

CROSS_RUN_BOOTSTRAP_MANIFEST = "cross-run-bootstrap.json"
CROSS_RUN_BOOTSTRAP_HANDOFF = "cross-run-handoff.txt"
_SCHEMA_VERSION = 1
_COPY_CHUNK_BYTES = 1024 * 1024


class CrossRunBootstrapError(RuntimeError):
    """Cross-run continuation state is invalid or cannot be initialized."""


@dataclass(frozen=True, slots=True)
class CrossRunBootstrap:
    source_run: str
    source_generation: int
    handoff: str
    successor_iso_sha256: str
    successor_iso_size: int
    feature_ledger_sha256: str
    inherited_request_ids: tuple[int, ...]
    git_base_ref: str
    git_base_commit: str

    def verify_initial_iso(self, initial_iso: Path) -> None:
        identity = _file_identity(initial_iso)
        if identity != (self.successor_iso_sha256, self.successor_iso_size):
            raise CrossRunBootstrapError(
                "initial ISO does not match cross-run bootstrap provenance"
            )


def initialize_cross_run_bootstrap(
    destination_run: str | Path,
    initial_iso: str | Path,
    source_run: str | Path,
    source_generation: int,
    git_repository: str | Path,
    git_base_ref: str,
) -> CrossRunBootstrap:
    """Validate one completed source generation and atomically seed a new run."""
    destination = Path(destination_run).resolve()
    source = Path(source_run).resolve()
    image = Path(initial_iso).resolve()
    if type(source_generation) is not int or source_generation < 0:
        raise CrossRunBootstrapError(
            "inherited generation must be a non-negative integer"
        )
    if destination.exists():
        raise CrossRunBootstrapError(
            "cross-run bootstrap requires a fresh destination run"
        )
    if source.is_symlink() or not source.is_dir():
        raise CrossRunBootstrapError(
            f"source run is unavailable: {source}"
        )
    if source == destination:
        raise CrossRunBootstrapError(
            "source and destination runs must be different"
        )
    if not git_base_ref:
        raise CrossRunBootstrapError("Git base ref must not be empty")

    try:
        from .generation_runtime import CodexOSRun

        source_runtime = CodexOSRun(source)
        archives = source_runtime.archived_generations()
        CodexOSRun._validate_archived_history(archives)
        archived = source_runtime.inspect_generation(source_generation)
        if archived.outcome != "completed" or archived.handoff is None:
            raise CrossRunBootstrapError(
                "inherited generation did not complete cooperatively"
            )
        successor_iso = archived.archive_path / "successor" / "codexos.iso"
        if not _files_equal(image, successor_iso):
            raise CrossRunBootstrapError(
                "initial ISO is not byte-identical to the inherited successor"
            )
        requests = FeatureRequestStore(source).requests()
    except CrossRunBootstrapError:
        raise
    except (OSError, RuntimeError, ValueError) as error:
        raise CrossRunBootstrapError(
            f"source run cannot be inherited: {error}"
        ) from error

    iso_sha256, iso_size = _file_identity(successor_iso)
    handoff_bytes = archived.handoff.encode("utf-8")
    ledger_bytes = _feature_ledger_bytes(requests)
    request_ids = tuple(request.id for request in requests)

    destination.parent.mkdir(parents=True, exist_ok=True)
    wrapper = Path(
        tempfile.mkdtemp(
            prefix=".cross-run-bootstrap-",
            dir=destination.parent,
        )
    )
    candidate = wrapper / destination.name
    candidate.mkdir()
    try:
        from .generation_git import GenerationGitRecorder

        recorder = GenerationGitRecorder(
            git_repository,
            candidate,
            git_base_ref,
        )
        bootstrap = CrossRunBootstrap(
            source_run=source.name,
            source_generation=source_generation,
            handoff=archived.handoff,
            successor_iso_sha256=iso_sha256,
            successor_iso_size=iso_size,
            feature_ledger_sha256=hashlib.sha256(ledger_bytes).hexdigest(),
            inherited_request_ids=request_ids,
            git_base_ref=git_base_ref,
            git_base_commit=recorder.base_commit,
        )
        _write_durable(candidate / CROSS_RUN_BOOTSTRAP_HANDOFF, handoff_bytes)
        FeatureRequestStore(candidate).import_requests(requests)
        imported_ledger = _feature_ledger_bytes(
            FeatureRequestStore(candidate).requests()
        )
        if imported_ledger != ledger_bytes:
            raise CrossRunBootstrapError(
                "inherited feature-request ledger changed during initialization"
            )
        _write_durable(
            candidate / CROSS_RUN_BOOTSTRAP_MANIFEST,
            _encode_manifest(bootstrap, handoff_bytes, len(requests)),
        )
        _fsync_directory(candidate / "feature-requests", optional=True)
        _fsync_directory(candidate)
        if destination.exists():
            raise CrossRunBootstrapError(
                "cross-run bootstrap destination appeared during initialization"
            )
        candidate.rename(destination)
        _fsync_directory(destination.parent)
        wrapper.rmdir()
        return bootstrap
    except BaseException:
        if wrapper.exists():
            shutil.rmtree(wrapper)
        raise


def load_cross_run_bootstrap(
    run_directory: str | Path,
) -> CrossRunBootstrap | None:
    """Load and validate persisted bootstrap provenance without its source run."""
    run = Path(run_directory).resolve()
    manifest_path = run / CROSS_RUN_BOOTSTRAP_MANIFEST
    handoff_path = run / CROSS_RUN_BOOTSTRAP_HANDOFF
    if not manifest_path.exists() and not handoff_path.exists():
        return None
    if (
        manifest_path.is_symlink()
        or not manifest_path.is_file()
        or handoff_path.is_symlink()
        or not handoff_path.is_file()
    ):
        raise CrossRunBootstrapError(
            "cross-run bootstrap provenance is incomplete"
        )
    try:
        value = json.loads(manifest_path.read_bytes().decode("utf-8"))
        handoff_bytes = handoff_path.read_bytes()
        handoff = handoff_bytes.decode("utf-8")
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CrossRunBootstrapError(
            "cross-run bootstrap provenance is malformed"
        ) from error
    bootstrap, expected_handoff = _decode_manifest(value)
    if expected_handoff != _bytes_identity(handoff_bytes):
        raise CrossRunBootstrapError(
            "cross-run bootstrap handoff identity does not match"
        )
    return CrossRunBootstrap(
        source_run=bootstrap.source_run,
        source_generation=bootstrap.source_generation,
        handoff=handoff,
        successor_iso_sha256=bootstrap.successor_iso_sha256,
        successor_iso_size=bootstrap.successor_iso_size,
        feature_ledger_sha256=bootstrap.feature_ledger_sha256,
        inherited_request_ids=bootstrap.inherited_request_ids,
        git_base_ref=bootstrap.git_base_ref,
        git_base_commit=bootstrap.git_base_commit,
    )


def _feature_ledger_bytes(requests: tuple[FeatureRequest, ...]) -> bytes:
    value = {
        "requests": [
            {
                "description": request.description,
                "generation": request.generation,
                "id": request.id,
                "status": request.status,
                "title": request.title,
            }
            for request in requests
        ]
    }
    return (
        json.dumps(
            value,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        )
        + "\n"
    ).encode("utf-8")


def _encode_manifest(
    bootstrap: CrossRunBootstrap,
    handoff_bytes: bytes,
    request_count: int,
) -> bytes:
    value = {
        "schema_version": _SCHEMA_VERSION,
        "source": {
            "run": bootstrap.source_run,
            "generation": bootstrap.source_generation,
        },
        "successor_iso": {
            "sha256": bootstrap.successor_iso_sha256,
            "size": bootstrap.successor_iso_size,
        },
        "handoff": {
            "file": CROSS_RUN_BOOTSTRAP_HANDOFF,
            "sha256": hashlib.sha256(handoff_bytes).hexdigest(),
            "size": len(handoff_bytes),
        },
        "feature_requests": {
            "count": request_count,
            "ids": list(bootstrap.inherited_request_ids),
            "sha256": bootstrap.feature_ledger_sha256,
        },
        "git_base": {
            "ref": bootstrap.git_base_ref,
            "commit": bootstrap.git_base_commit,
        },
    }
    return (
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")


def _decode_manifest(
    value: object,
) -> tuple[CrossRunBootstrap, tuple[str, int]]:
    if not isinstance(value, dict) or set(value) != {
        "schema_version",
        "source",
        "successor_iso",
        "handoff",
        "feature_requests",
        "git_base",
    }:
        raise CrossRunBootstrapError(
            "cross-run bootstrap manifest has invalid fields"
        )
    if value["schema_version"] != _SCHEMA_VERSION:
        raise CrossRunBootstrapError(
            "cross-run bootstrap schema version is unsupported"
        )
    source = _mapping(value["source"], {"run", "generation"}, "source")
    iso = _mapping(value["successor_iso"], {"sha256", "size"}, "ISO")
    handoff = _mapping(
        value["handoff"], {"file", "sha256", "size"}, "handoff"
    )
    requests = _mapping(
        value["feature_requests"], {"count", "ids", "sha256"}, "ledger"
    )
    git_base = _mapping(value["git_base"], {"ref", "commit"}, "Git base")
    source_run = source["run"]
    generation = source["generation"]
    if (
        not isinstance(source_run, str)
        or not source_run
        or source_run in {".", ".."}
        or Path(source_run).name != source_run
        or type(generation) is not int
        or generation < 0
    ):
        raise CrossRunBootstrapError("cross-run bootstrap source is invalid")
    iso_identity = _manifest_identity(iso, "successor ISO")
    handoff_identity = _manifest_identity(handoff, "handoff")
    if handoff["file"] != CROSS_RUN_BOOTSTRAP_HANDOFF:
        raise CrossRunBootstrapError("cross-run bootstrap handoff file is invalid")
    count = requests["count"]
    ids = requests["ids"]
    if (
        type(count) is not int
        or count < 0
        or not isinstance(ids, list)
        or any(type(item) is not int or item <= 0 for item in ids)
        or ids != sorted(set(ids))
        or count != len(ids)
    ):
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature-request identity is invalid"
        )
    ledger_hash = _sha256(requests["sha256"], "feature-request ledger")
    git_ref = git_base["ref"]
    git_commit = git_base["commit"]
    if not isinstance(git_ref, str) or not git_ref:
        raise CrossRunBootstrapError("cross-run bootstrap Git ref is invalid")
    _sha256(git_commit, "Git commit", allow_non_sha256=True)
    return (
        CrossRunBootstrap(
            source_run,
            generation,
            "",
            iso_identity[0],
            iso_identity[1],
            ledger_hash,
            tuple(ids),
            git_ref,
            git_commit,
        ),
        handoff_identity,
    )


def _mapping(value: object, fields: set[str], label: str) -> dict[str, object]:
    if not isinstance(value, dict) or set(value) != fields:
        raise CrossRunBootstrapError(
            f"cross-run bootstrap {label} has invalid fields"
        )
    return value


def _manifest_identity(
    value: dict[str, object], label: str
) -> tuple[str, int]:
    digest = _sha256(value["sha256"], label)
    size = value["size"]
    if type(size) is not int or size < 0:
        raise CrossRunBootstrapError(
            f"cross-run bootstrap {label} size is invalid"
        )
    return digest, size


def _sha256(value: object, label: str, *, allow_non_sha256: bool = False) -> str:
    minimum = 40 if allow_non_sha256 else 64
    if (
        not isinstance(value, str)
        or len(value) < minimum
        or (not allow_non_sha256 and len(value) != 64)
        or any(character not in "0123456789abcdef" for character in value)
    ):
        raise CrossRunBootstrapError(
            f"cross-run bootstrap {label} digest is invalid"
        )
    return value


def _files_equal(left: Path, right: Path) -> bool:
    if left.is_symlink() or not left.is_file():
        raise CrossRunBootstrapError(f"initial ISO is unavailable: {left}")
    if right.is_symlink() or not right.is_file():
        raise CrossRunBootstrapError(
            "inherited successor ISO is unavailable"
        )
    if left.stat().st_size != right.stat().st_size:
        return False
    with left.open("rb") as left_file, right.open("rb") as right_file:
        while True:
            left_chunk = left_file.read(_COPY_CHUNK_BYTES)
            right_chunk = right_file.read(_COPY_CHUNK_BYTES)
            if left_chunk != right_chunk:
                return False
            if not left_chunk:
                return True


def _file_identity(path: Path) -> tuple[str, int]:
    if path.is_symlink() or not path.is_file():
        raise CrossRunBootstrapError(f"file is unavailable: {path}")
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as input_file:
        while chunk := input_file.read(_COPY_CHUNK_BYTES):
            digest.update(chunk)
            size += len(chunk)
    return digest.hexdigest(), size


def _bytes_identity(contents: bytes) -> tuple[str, int]:
    return hashlib.sha256(contents).hexdigest(), len(contents)


def _write_durable(path: Path, contents: bytes) -> None:
    with path.open("xb") as output:
        output.write(contents)
        output.flush()
        os.fsync(output.fileno())


def _fsync_directory(path: Path, *, optional: bool = False) -> None:
    if optional and not path.exists():
        return
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)

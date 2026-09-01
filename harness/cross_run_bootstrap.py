"""Immutable cross-run continuation bootstrap provenance."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .feature_requests import (
    FeatureRequest,
    FeatureRequestError,
    FeatureRequestStore,
    decode_feature_request,
)

CROSS_RUN_BOOTSTRAP_MANIFEST = "cross-run-bootstrap.json"
CROSS_RUN_BOOTSTRAP_HANDOFF = "cross-run-handoff.txt"
CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER = "cross-run-feature-requests.json"
_SCHEMA_VERSION = 2
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
    feature_ledger_size: int
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
        if not archives or archives[-1].generation != source_generation:
            raise CrossRunBootstrapError(
                "inherited generation must be the latest source-run archive"
            )
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
        if any(
            request.generation > source_generation for request in requests
        ):
            raise CrossRunBootstrapError(
                "source feature-request ledger contains a request from a "
                "generation newer than the inherited generation"
            )
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
    expected_git_base_ref = (
        f"{source.name}/generation-{source_generation:04d}"
    )
    if git_base_ref != expected_git_base_ref:
        raise CrossRunBootstrapError(
            "Git base ref must be the inherited generation tag: "
            + expected_git_base_ref
        )

    destination.parent.mkdir(parents=True, exist_ok=True)
    wrapper = Path(
        tempfile.mkdtemp(
            prefix=".cross-run-bootstrap-",
            dir=destination.parent,
        )
    )
    candidate = wrapper / destination.name
    candidate.mkdir()
    published = False
    try:
        from .generation_git import GenerationGitRecorder

        recorder = GenerationGitRecorder(
            git_repository,
            candidate,
            git_base_ref,
        )
        generation_tag_commit = recorder.generation_tag_commit(
            expected_git_base_ref
        )
        if generation_tag_commit != recorder.base_commit:
            raise CrossRunBootstrapError(
                "Git base commit does not match the inherited generation tag"
            )
        bootstrap = CrossRunBootstrap(
            source_run=source.name,
            source_generation=source_generation,
            handoff=archived.handoff,
            successor_iso_sha256=iso_sha256,
            successor_iso_size=iso_size,
            feature_ledger_sha256=hashlib.sha256(ledger_bytes).hexdigest(),
            feature_ledger_size=len(ledger_bytes),
            inherited_request_ids=request_ids,
            git_base_ref=git_base_ref,
            git_base_commit=recorder.base_commit,
        )
        _write_durable(candidate / CROSS_RUN_BOOTSTRAP_HANDOFF, handoff_bytes)
        _write_durable(
            candidate / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER,
            ledger_bytes,
        )
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
        published = True
        _fsync_directory(destination.parent)
        wrapper.rmdir()
        return bootstrap
    except BaseException:
        if published and destination.exists():
            shutil.rmtree(destination)
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
    ledger_path = run / CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER
    if (
        not manifest_path.exists()
        and not handoff_path.exists()
        and not ledger_path.exists()
    ):
        return None
    if (
        manifest_path.is_symlink()
        or not manifest_path.is_file()
        or handoff_path.is_symlink()
        or not handoff_path.is_file()
        or ledger_path.is_symlink()
        or not ledger_path.is_file()
    ):
        raise CrossRunBootstrapError(
            "cross-run bootstrap provenance is incomplete"
        )
    try:
        value = json.loads(manifest_path.read_bytes().decode("utf-8"))
        handoff_bytes = handoff_path.read_bytes()
        handoff = handoff_bytes.decode("utf-8")
        ledger_bytes = ledger_path.read_bytes()
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CrossRunBootstrapError(
            "cross-run bootstrap provenance is malformed"
        ) from error
    bootstrap, expected_handoff = _decode_manifest(value)
    if expected_handoff != _bytes_identity(handoff_bytes):
        raise CrossRunBootstrapError(
            "cross-run bootstrap handoff identity does not match"
        )
    inherited_requests = _decode_feature_ledger(ledger_bytes)
    if (
        _bytes_identity(ledger_bytes)
        != (bootstrap.feature_ledger_sha256, bootstrap.feature_ledger_size)
        or tuple(request.id for request in inherited_requests)
        != bootstrap.inherited_request_ids
    ):
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger identity does not match"
        )
    _validate_inherited_requests(run, inherited_requests)
    return CrossRunBootstrap(
        source_run=bootstrap.source_run,
        source_generation=bootstrap.source_generation,
        handoff=handoff,
        successor_iso_sha256=bootstrap.successor_iso_sha256,
        successor_iso_size=bootstrap.successor_iso_size,
        feature_ledger_sha256=bootstrap.feature_ledger_sha256,
        feature_ledger_size=bootstrap.feature_ledger_size,
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
            "file": CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER,
            "ids": list(bootstrap.inherited_request_ids),
            "sha256": bootstrap.feature_ledger_sha256,
            "size": bootstrap.feature_ledger_size,
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
        value["feature_requests"],
        {"count", "file", "ids", "sha256", "size"},
        "ledger",
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
    if requests["file"] != CROSS_RUN_BOOTSTRAP_FEATURE_LEDGER:
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger file is invalid"
        )
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
    ledger_size = requests["size"]
    if type(ledger_size) is not int or ledger_size < 0:
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger size is invalid"
        )
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
            ledger_size,
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


def _decode_feature_ledger(contents: bytes) -> tuple[FeatureRequest, ...]:
    try:
        value = json.loads(contents.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger is malformed"
        ) from error
    if not isinstance(value, dict) or set(value) != {"requests"}:
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger has invalid fields"
        )
    records = value["requests"]
    if not isinstance(records, list):
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger is invalid"
        )
    requests: list[FeatureRequest] = []
    for record in records:
        if not isinstance(record, dict) or set(record) != {
            "description",
            "generation",
            "id",
            "status",
            "title",
        }:
            raise CrossRunBootstrapError(
                "cross-run bootstrap feature ledger record is invalid"
            )
        try:
            request = decode_feature_request(record)
        except FeatureRequestError as error:
            raise CrossRunBootstrapError(
                "cross-run bootstrap feature ledger record is invalid"
            ) from error
        requests.append(request)
    decoded = tuple(requests)
    if (
        tuple(request.id for request in decoded)
        != tuple(sorted({request.id for request in decoded}))
        or _feature_ledger_bytes(decoded) != contents
    ):
        raise CrossRunBootstrapError(
            "cross-run bootstrap feature ledger is not canonical"
        )
    return decoded


def _validate_inherited_requests(
    run: Path,
    inherited: tuple[FeatureRequest, ...],
) -> None:
    try:
        current = FeatureRequestStore(run).requests()
    except RuntimeError as error:
        raise CrossRunBootstrapError(
            f"inherited feature-request state is invalid: {error}"
        ) from error
    current_by_id = {request.id: request for request in current}
    generation_archived = any(
        path.name.startswith("generation-") and path.is_dir()
        for path in run.iterdir()
    )
    inherited_ids = {request.id for request in inherited}
    maximum_inherited_id = max(inherited_ids, default=0)
    for original in inherited:
        observed = current_by_id.get(original.id)
        if observed is None:
            raise CrossRunBootstrapError(
                f"inherited feature request #{original.id} is missing"
            )
        if (
            observed.generation != original.generation
            or observed.title != original.title
            or observed.description != original.description
            or (
                observed.status != original.status
                and not (
                    generation_archived
                    and original.status == "pending"
                    and observed.status in {"approved", "denied"}
                )
            )
        ):
            raise CrossRunBootstrapError(
                f"inherited feature request #{original.id} was altered"
            )
    if any(
        request.id not in inherited_ids
        and request.id <= maximum_inherited_id
        for request in current
    ):
        raise CrossRunBootstrapError(
            "new feature request collides with an inherited identity"
        )


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

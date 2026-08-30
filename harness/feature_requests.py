"""Persistent run-level CodexOS feature requests."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path

MAX_FEATURE_TITLE_BYTES = 256
MAX_FEATURE_DESCRIPTION_BYTES = 16 * 1024
_STATUSES = {"pending", "approved", "denied"}


class FeatureRequestError(RuntimeError):
    """Persisted feature-request state is invalid or cannot be updated."""


@dataclass(frozen=True, slots=True)
class FeatureRequest:
    id: int
    generation: int
    title: str
    description: str
    status: str


class FeatureRequestStore:
    """Store the concrete feature requests belonging to one CodexOS run."""

    def __init__(self, run_directory: str | Path) -> None:
        self._directory = Path(run_directory).resolve() / "feature-requests"
        self._requests = self._read_all()

    def requests(self) -> tuple[FeatureRequest, ...]:
        self._refresh()
        return tuple(
            self._requests[request_id] for request_id in sorted(self._requests)
        )

    def request(self, request_id: int) -> FeatureRequest:
        _validate_request_id(request_id)
        self._refresh()
        return self._cached_request(request_id)

    def _cached_request(self, request_id: int) -> FeatureRequest:
        try:
            return self._requests[request_id]
        except KeyError as error:
            raise FeatureRequestError(
                f"feature request #{request_id} does not exist"
            ) from error

    def create(
        self,
        generation: int,
        title: str,
        description: str,
    ) -> FeatureRequest:
        _validate_generation(generation)
        _validate_text(title, description)
        self._refresh()
        request_id = max(self._requests, default=0) + 1
        request = FeatureRequest(
            request_id,
            generation,
            title,
            description,
            "pending",
        )
        self._write(request, replace=False)
        self._requests[request_id] = request
        return request

    def approve(self, request_id: int) -> FeatureRequest:
        return self._set_status(request_id, "approved")

    def deny(self, request_id: int) -> FeatureRequest:
        return self._set_status(request_id, "denied")

    def _set_status(self, request_id: int, status: str) -> FeatureRequest:
        self._refresh()
        _validate_request_id(request_id)
        current = self._cached_request(request_id)
        if current.status != "pending":
            raise FeatureRequestError(
                f"feature request #{request_id} is already {current.status}"
            )
        updated = FeatureRequest(
            current.id,
            current.generation,
            current.title,
            current.description,
            status,
        )
        self._write(updated, replace=True)
        self._requests[request_id] = updated
        return updated

    def _refresh(self) -> None:
        self._requests = self._read_all()

    def _read_all(self) -> dict[int, FeatureRequest]:
        if not self._directory.exists():
            return {}
        if self._directory.is_symlink() or not self._directory.is_dir():
            raise FeatureRequestError(
                f"feature-request store is not a directory: {self._directory}"
            )

        requests: dict[int, FeatureRequest] = {}
        for path in sorted(self._directory.iterdir(), key=lambda item: item.name):
            request_id = _request_id_from_name(path.name)
            if path.is_symlink() or not path.is_file():
                raise FeatureRequestError(
                    f"invalid feature-request record: {path.name}"
                )
            try:
                value = json.loads(path.read_bytes().decode("utf-8"))
            except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
                raise FeatureRequestError(
                    f"malformed feature-request record: {path.name}"
                ) from error
            request = _decode_request(value)
            if request.id != request_id or request.id in requests:
                raise FeatureRequestError(
                    f"conflicting feature-request record: {path.name}"
                )
            requests[request.id] = request
        return requests

    def _write(self, request: FeatureRequest, *, replace: bool) -> None:
        try:
            self._directory.mkdir(parents=True, exist_ok=True)
            if self._directory.is_symlink() or not self._directory.is_dir():
                raise FeatureRequestError(
                    f"feature-request store is not a directory: {self._directory}"
                )
            destination = self._directory / _request_name(request.id)
            if not replace and destination.exists():
                raise FeatureRequestError(
                    f"feature request #{request.id} already exists"
                )
            encoded = (
                json.dumps(
                    {
                        "description": request.description,
                        "generation": request.generation,
                        "id": request.id,
                        "status": request.status,
                        "title": request.title,
                    },
                    ensure_ascii=False,
                    indent=2,
                    sort_keys=True,
                )
                + "\n"
            ).encode("utf-8")
            descriptor, temporary_name = tempfile.mkstemp(
                prefix=f".{destination.name}.",
                dir=self._directory,
            )
            temporary = Path(temporary_name)
            try:
                with os.fdopen(descriptor, "wb") as output:
                    output.write(encoded)
                    output.flush()
                    os.fsync(output.fileno())
                if not replace and destination.exists():
                    raise FeatureRequestError(
                        f"feature request #{request.id} already exists"
                    )
                os.replace(temporary, destination)
            finally:
                if temporary.exists():
                    temporary.unlink()
        except FeatureRequestError:
            raise
        except OSError as error:
            raise FeatureRequestError(
                f"could not persist feature request #{request.id}: {error}"
            ) from error


def _request_name(request_id: int) -> str:
    return f"request-{request_id:06d}.json"


def _request_id_from_name(name: str) -> int:
    prefix = "request-"
    suffix = ".json"
    if not name.startswith(prefix) or not name.endswith(suffix):
        raise FeatureRequestError(f"invalid feature-request filename: {name}")
    encoded_id = name[len(prefix) : -len(suffix)]
    if not encoded_id.isascii() or not encoded_id.isdecimal():
        raise FeatureRequestError(f"invalid feature-request filename: {name}")
    request_id = int(encoded_id)
    if request_id <= 0 or name != _request_name(request_id):
        raise FeatureRequestError(f"invalid feature-request filename: {name}")
    return request_id


def _decode_request(value: object) -> FeatureRequest:
    if not isinstance(value, dict) or set(value) != {
        "id",
        "generation",
        "title",
        "description",
        "status",
    }:
        raise FeatureRequestError("feature-request record has invalid fields")
    request_id = value["id"]
    generation = value["generation"]
    title = value["title"]
    description = value["description"]
    status = value["status"]
    _validate_request_id(request_id)
    _validate_generation(generation)
    if not isinstance(title, str) or not isinstance(description, str):
        raise FeatureRequestError("feature-request text must be strings")
    _validate_text(title, description)
    if not isinstance(status, str) or status not in _STATUSES:
        raise FeatureRequestError("feature-request status is invalid")
    return FeatureRequest(request_id, generation, title, description, status)


def _validate_request_id(request_id: object) -> None:
    if type(request_id) is not int or request_id <= 0:
        raise FeatureRequestError("feature-request ID must be a positive integer")


def _validate_generation(generation: object) -> None:
    if type(generation) is not int or generation < 0:
        raise FeatureRequestError("feature-request generation is invalid")


def _validate_text(title: str, description: str) -> None:
    if not isinstance(title, str) or not isinstance(description, str):
        raise FeatureRequestError("feature-request text must be strings")
    try:
        encoded_title = title.encode("utf-8")
        encoded_description = description.encode("utf-8")
    except UnicodeEncodeError as error:
        raise FeatureRequestError("feature-request text is not valid UTF-8") from error
    if not encoded_title:
        raise FeatureRequestError("feature-request title must not be empty")
    if len(encoded_title) > MAX_FEATURE_TITLE_BYTES:
        raise FeatureRequestError("feature-request title exceeds 256 bytes")
    if len(encoded_description) > MAX_FEATURE_DESCRIPTION_BYTES:
        raise FeatureRequestError("feature-request description exceeds 16 KiB")

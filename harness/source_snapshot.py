"""Codec and path validation for bounded CodexOS source snapshots."""

from __future__ import annotations

import struct
from collections.abc import Iterable
from dataclasses import dataclass

_MAX_FILES = 128
_MAX_PATH_LENGTH = 255
_MAX_CONTENT_SIZE = 64 * 1024


class SourceSnapshotError(ValueError):
    """A malformed or unsafe source snapshot."""


@dataclass(frozen=True)
class SnapshotFile:
    path: str
    content: bytes


def encode_source_snapshot(files: Iterable[SnapshotFile]) -> bytes:
    entries = tuple(files)
    _validate_entries(entries)

    encoded = bytearray(struct.pack("<H", len(entries)))
    for entry in entries:
        path = entry.path.encode("utf-8")
        encoded.extend(struct.pack("<H", len(path)))
        encoded.extend(path)
        encoded.extend(struct.pack("<I", len(entry.content)))
        encoded.extend(entry.content)
    return bytes(encoded)


def decode_source_snapshot(data: bytes) -> tuple[SnapshotFile, ...]:
    view = memoryview(data)
    offset = 0

    def take(length: int) -> memoryview:
        nonlocal offset
        if length > len(view) - offset:
            raise SourceSnapshotError("truncated source snapshot")
        result = view[offset : offset + length]
        offset += length
        return result

    file_count = struct.unpack("<H", take(2))[0]
    if file_count > _MAX_FILES:
        raise SourceSnapshotError("source snapshot contains more than 128 files")

    entries: list[SnapshotFile] = []
    total_content = 0
    for _ in range(file_count):
        path_length = struct.unpack("<H", take(2))[0]
        if path_length == 0 or path_length > _MAX_PATH_LENGTH:
            raise SourceSnapshotError("source path length must be 1 through 255")
        try:
            path = bytes(take(path_length)).decode("utf-8")
        except UnicodeDecodeError as error:
            raise SourceSnapshotError("source path is not valid UTF-8") from error
        content_length = struct.unpack("<I", take(4))[0]
        total_content += content_length
        if total_content > _MAX_CONTENT_SIZE:
            raise SourceSnapshotError("source snapshot content exceeds 64 KiB")
        entries.append(SnapshotFile(path, bytes(take(content_length))))

    if offset != len(view):
        raise SourceSnapshotError("unexpected trailing source snapshot data")
    result = tuple(entries)
    _validate_entries(result)
    return result


def _validate_entries(entries: tuple[SnapshotFile, ...]) -> None:
    if len(entries) > _MAX_FILES:
        raise SourceSnapshotError("source snapshot contains more than 128 files")

    paths: set[str] = set()
    total_content = 0
    for entry in entries:
        try:
            encoded_path = entry.path.encode("utf-8")
        except UnicodeEncodeError as error:
            raise SourceSnapshotError("source path is not valid UTF-8") from error
        if not 1 <= len(encoded_path) <= _MAX_PATH_LENGTH:
            raise SourceSnapshotError("source path length must be 1 through 255")
        _validate_build_path(entry.path)
        if entry.path in paths:
            raise SourceSnapshotError(f"duplicate source path: {entry.path}")
        paths.add(entry.path)
        total_content += len(entry.content)
        if total_content > _MAX_CONTENT_SIZE:
            raise SourceSnapshotError("source snapshot content exceeds 64 KiB")


def _validate_build_path(path: str) -> None:
    if "\0" in path or path.startswith("/"):
        raise SourceSnapshotError(f"unsafe source path: {path!r}")
    components = path.split("/")
    if (
        len(components) < 2
        or components[0] != "seed"
        or any(component in {"", ".", ".."} for component in components)
    ):
        raise SourceSnapshotError(f"unsafe source path: {path!r}")

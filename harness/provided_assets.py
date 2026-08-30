"""Immutable operator-provided assets and their guest host services."""

from __future__ import annotations

import hashlib
import io
import json
import os
import re
import stat
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .framing import Frame
from .host_service_protocol import HostServiceRequest, create_host_service_response

PROVIDED_ASSETS_MANIFEST = "provided-assets.json"
MAX_PROVIDED_ASSET_ID_BYTES = 64
MAX_PROVIDED_ASSET_FILENAME_BYTES = 255
MAX_PROVIDED_ASSET_READ_BYTES = 1024 * 1024

_MANIFEST_SCHEMA_VERSION = 1
_MAX_DIAGNOSTIC_BYTES = 1024
_MAX_UINT64 = (1 << 64) - 1
_ASSET_ID = re.compile(r"[a-z0-9]+(?:-[a-z0-9]+)*\Z")


class ProvidedAssetsError(RuntimeError):
    """Provided-asset input or persisted provenance is invalid."""


@dataclass(frozen=True, slots=True)
class ProvidedAsset:
    id: str
    filename: str
    data: bytes
    sha256: str

    @property
    def size(self) -> int:
        return len(self.data)


class ProvidedAssets:
    """One completely frozen set of opaque named assets."""

    def __init__(self, assets: tuple[ProvidedAsset, ...]) -> None:
        ordered = tuple(sorted(assets, key=lambda asset: asset.id))
        if len({asset.id for asset in ordered}) != len(ordered):
            raise ProvidedAssetsError("provided asset IDs are not unique")
        self._assets = ordered
        self._by_id = {asset.id: asset for asset in ordered}

    @classmethod
    def from_directory(cls, directory: str | Path) -> ProvidedAssets:
        root = Path(directory)
        try:
            root_stat = root.lstat()
        except OSError as error:
            raise ProvidedAssetsError(
                f"provided-assets directory is unavailable: {error}"
            ) from error
        if stat.S_ISLNK(root_stat.st_mode) or not stat.S_ISDIR(root_stat.st_mode):
            raise ProvidedAssetsError("provided-assets path must be a real directory")

        assets: list[ProvidedAsset] = []
        try:
            children = sorted(root.iterdir(), key=lambda path: path.name)
        except OSError as error:
            raise ProvidedAssetsError(
                f"could not inspect provided-assets directory: {error}"
            ) from error
        for child in children:
            asset_id = child.name
            _validate_asset_id(asset_id)
            try:
                child_stat = child.lstat()
            except OSError as error:
                raise ProvidedAssetsError(
                    f"could not inspect provided asset {asset_id!r}: {error}"
                ) from error
            if stat.S_ISLNK(child_stat.st_mode) or not stat.S_ISDIR(
                child_stat.st_mode
            ):
                raise ProvidedAssetsError(
                    f"provided asset {asset_id!r} must be a real directory"
                )
            filename, data = _derive_asset(child, asset_id)
            assets.append(
                ProvidedAsset(
                    asset_id,
                    filename,
                    data,
                    hashlib.sha256(data).hexdigest(),
                )
            )
        return cls(tuple(assets))

    @property
    def assets(self) -> tuple[ProvidedAsset, ...]:
        return self._assets

    def manifest_object(self) -> dict[str, object]:
        return {
            "schema_version": _MANIFEST_SCHEMA_VERSION,
            "assets": [
                {
                    "filename": asset.filename,
                    "id": asset.id,
                    "sha256": asset.sha256,
                    "size": asset.size,
                }
                for asset in self._assets
            ],
        }

    def descriptor_bytes(self) -> bytes:
        return "".join(
            f"{asset.id}\t{asset.filename}\t{asset.size}\t{asset.sha256}\n"
            for asset in self._assets
        ).encode("utf-8")

    def handle_request(self, request: HostServiceRequest) -> Frame:
        if request.service_name == "list_provided_assets":
            if request.arguments:
                return _response(
                    request,
                    1,
                    b"list_provided_assets expects no arguments",
                )
            try:
                return create_host_service_response(
                    request.request_id,
                    0,
                    self.descriptor_bytes(),
                )
            except ValueError:
                return _response(
                    request,
                    2,
                    b"provided-asset descriptor list exceeds the frame limit",
                )

        if request.service_name == "read_provided_asset":
            return self._read(request)
        return _response(
            request,
            1,
            f"unknown host service: {request.service_name}".encode("utf-8"),
        )

    def _read(self, request: HostServiceRequest) -> Frame:
        if len(request.arguments) != 3:
            return _response(
                request,
                1,
                b"read_provided_asset expects asset ID, offset, and length",
            )
        encoded_id, encoded_offset, encoded_length = request.arguments
        try:
            asset_id = encoded_id.decode("utf-8")
            _validate_asset_id(asset_id)
            offset = _canonical_uint64(encoded_offset, "offset")
            length = _canonical_uint64(encoded_length, "length")
        except (ProvidedAssetsError, UnicodeDecodeError, ValueError) as error:
            return _response(request, 1, str(error).encode("utf-8"))
        if length > MAX_PROVIDED_ASSET_READ_BYTES:
            return _response(
                request,
                1,
                b"provided-asset read length exceeds 1 MiB",
            )
        asset = self._by_id.get(asset_id)
        if asset is None:
            return _response(request, 1, b"provided asset does not exist")
        if offset > _MAX_UINT64 - length or offset + length > asset.size:
            return _response(request, 1, b"provided-asset read is out of range")
        return create_host_service_response(
            request.request_id,
            0,
            asset.data[offset : offset + length],
        )


def configure_provided_assets(
    run_directory: str | Path,
    external_directory: str | Path | None,
) -> ProvidedAssets | None:
    """Freeze and verify one run's explicitly supplied asset set."""

    run = Path(run_directory).resolve()
    manifest_path = run / PROVIDED_ASSETS_MANIFEST
    if external_directory is None:
        if manifest_path.exists() or manifest_path.is_symlink():
            raise ProvidedAssetsError(
                "run records provided assets; --provided-assets is required"
            )
        return None

    snapshot = ProvidedAssets.from_directory(external_directory)
    if manifest_path.exists() or manifest_path.is_symlink():
        recorded = _read_manifest(manifest_path)
        expected = snapshot.manifest_object()
        if recorded != expected:
            raise ProvidedAssetsError(
                "supplied provided-assets set does not match run provenance"
            )
        return snapshot

    _write_manifest_once(manifest_path, snapshot.manifest_object())
    return snapshot


def _derive_asset(directory: Path, asset_id: str) -> tuple[str, bytes]:
    entries = sorted(
        _walk_asset(directory, Path()),
        key=lambda entry: entry[0].as_posix(),
    )
    if len(entries) == 1 and entries[0][1] == "file":
        relative, _, _, data = entries[0]
        if len(relative.parts) == 1:
            filename = relative.name
            _validate_filename(filename)
            return filename, data

    output = io.BytesIO()
    try:
        with tarfile.open(
            fileobj=output,
            mode="w",
            format=tarfile.PAX_FORMAT,
            dereference=False,
        ) as archive:
            for relative, entry_type, mode, data in entries:
                name = relative.as_posix()
                information = tarfile.TarInfo(
                    name + "/" if entry_type == "directory" else name
                )
                information.mtime = 0
                information.uid = 0
                information.gid = 0
                information.uname = ""
                information.gname = ""
                if entry_type == "directory":
                    information.type = tarfile.DIRTYPE
                    information.mode = 0o755
                    information.size = 0
                    archive.addfile(information)
                else:
                    information.type = tarfile.REGTYPE
                    information.mode = 0o755 if mode & 0o111 else 0o644
                    information.size = len(data)
                    archive.addfile(information, io.BytesIO(data))
    except (OSError, tarfile.TarError, ValueError) as error:
        raise ProvidedAssetsError(
            f"could not derive provided asset {asset_id!r}: {error}"
        ) from error
    return f"{asset_id}.tar", output.getvalue()


def _walk_asset(
    directory: Path,
    relative_directory: Path,
) -> list[tuple[Path, str, int, bytes]]:
    result: list[tuple[Path, str, int, bytes]] = []
    try:
        children = sorted(directory.iterdir(), key=lambda path: path.name)
    except OSError as error:
        raise ProvidedAssetsError(
            f"could not inspect provided asset: {error}"
        ) from error
    for child in children:
        _validate_filename(child.name)
        relative = relative_directory / child.name
        try:
            metadata = child.lstat()
        except OSError as error:
            raise ProvidedAssetsError(
                "could not inspect provided asset entry "
                f"{relative.as_posix()!r}: {error}"
            ) from error
        if stat.S_ISLNK(metadata.st_mode):
            raise ProvidedAssetsError(
                f"provided asset contains a symlink: {relative.as_posix()!r}"
            )
        if stat.S_ISDIR(metadata.st_mode):
            result.append((relative, "directory", metadata.st_mode, b""))
            result.extend(_walk_asset(child, relative))
        elif stat.S_ISREG(metadata.st_mode):
            result.append(
                (relative, "file", metadata.st_mode, _read_regular_file(child))
            )
        else:
            raise ProvidedAssetsError(
                "provided asset contains a special filesystem entry: "
                + repr(relative.as_posix())
            )
    return result


def _read_regular_file(path: Path) -> bytes:
    flags = os.O_RDONLY
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
        with os.fdopen(descriptor, "rb") as source:
            if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
                raise ProvidedAssetsError("provided asset entry is not a regular file")
            return source.read()
    except ProvidedAssetsError:
        raise
    except OSError as error:
        raise ProvidedAssetsError(
            f"could not read provided asset entry: {error}"
        ) from error


def _validate_asset_id(asset_id: str) -> None:
    try:
        encoded = asset_id.encode("utf-8")
    except UnicodeEncodeError as error:
        raise ProvidedAssetsError("provided asset ID is not valid UTF-8") from error
    if len(encoded) > MAX_PROVIDED_ASSET_ID_BYTES:
        raise ProvidedAssetsError("provided asset ID exceeds 64 bytes")
    if not _ASSET_ID.fullmatch(asset_id):
        raise ProvidedAssetsError(f"invalid provided asset ID: {asset_id!r}")


def _validate_filename(filename: str) -> None:
    try:
        encoded = filename.encode("utf-8")
    except UnicodeEncodeError as error:
        raise ProvidedAssetsError(
            "provided asset filename is not valid UTF-8"
        ) from error
    if not encoded or len(encoded) > MAX_PROVIDED_ASSET_FILENAME_BYTES:
        raise ProvidedAssetsError("provided asset filename has an invalid length")
    if filename in {".", ".."} or "/" in filename or "\\" in filename:
        raise ProvidedAssetsError(f"unsafe provided asset filename: {filename!r}")
    if any(
        ord(character) < 0x20 or 0x7F <= ord(character) <= 0x9F
        for character in filename
    ):
        raise ProvidedAssetsError(f"unsafe provided asset filename: {filename!r}")


def _canonical_uint64(encoded: bytes, name: str) -> int:
    if not encoded or any(value < 0x30 or value > 0x39 for value in encoded):
        raise ValueError(f"{name} is not canonical unsigned ASCII decimal")
    if len(encoded) > 1 and encoded.startswith(b"0"):
        raise ValueError(f"{name} is not canonical unsigned ASCII decimal")
    value = int(encoded)
    if value > _MAX_UINT64:
        raise ValueError(f"{name} exceeds uint64")
    return value


def _read_manifest(path: Path) -> dict[str, object]:
    if path.is_symlink() or not path.is_file():
        raise ProvidedAssetsError("provided-assets provenance is not a regular file")
    try:
        value = json.loads(path.read_bytes().decode("utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ProvidedAssetsError("provided-assets provenance is malformed") from error
    _validate_manifest(value)
    return value


def _validate_manifest(value: object) -> None:
    if not isinstance(value, dict) or set(value) != {"schema_version", "assets"}:
        raise ProvidedAssetsError("provided-assets provenance has invalid fields")
    if (
        type(value["schema_version"]) is not int
        or value["schema_version"] != _MANIFEST_SCHEMA_VERSION
    ):
        raise ProvidedAssetsError(
            "provided-assets provenance has an unsupported schema"
        )
    assets = value["assets"]
    if not isinstance(assets, list):
        raise ProvidedAssetsError("provided-assets provenance has invalid assets")
    previous_id: str | None = None
    for item in assets:
        if not isinstance(item, dict) or set(item) != {
            "filename",
            "id",
            "sha256",
            "size",
        }:
            raise ProvidedAssetsError(
                "provided-assets provenance has invalid asset fields"
            )
        asset_id = item["id"]
        filename = item["filename"]
        size = item["size"]
        digest = item["sha256"]
        if not isinstance(asset_id, str) or not isinstance(filename, str):
            raise ProvidedAssetsError("provided-assets provenance has invalid text")
        _validate_asset_id(asset_id)
        _validate_filename(filename)
        if previous_id is not None and asset_id <= previous_id:
            raise ProvidedAssetsError("provided-assets provenance is not ID ordered")
        previous_id = asset_id
        if type(size) is not int or size < 0 or size > _MAX_UINT64:
            raise ProvidedAssetsError("provided-assets provenance has invalid size")
        if (
            not isinstance(digest, str)
            or len(digest) != 64
            or any(character not in "0123456789abcdef" for character in digest)
        ):
            raise ProvidedAssetsError("provided-assets provenance has invalid SHA-256")


def _write_manifest_once(path: Path, value: dict[str, object]) -> None:
    encoded = (
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{path.name}.",
            dir=path.parent,
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as output:
                output.write(encoded)
                output.flush()
                os.fsync(output.fileno())
            try:
                os.link(temporary, path)
            except FileExistsError:
                if _read_manifest(path) != value:
                    raise ProvidedAssetsError(
                        "provided-assets provenance was initialized concurrently "
                        "with different contents"
                    )
        finally:
            if temporary.exists():
                temporary.unlink()
    except ProvidedAssetsError:
        raise
    except OSError as error:
        raise ProvidedAssetsError(
            f"could not persist provided-assets provenance: {error}"
        ) from error


def _response(request: HostServiceRequest, status: int, output: bytes) -> Frame:
    return create_host_service_response(
        request.request_id,
        status,
        output[:_MAX_DIAGNOSTIC_BYTES],
    )

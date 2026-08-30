from __future__ import annotations

import hashlib
import io
import json
import os
import struct
import tarfile
import tempfile
import unittest
from collections.abc import Callable
from pathlib import Path
from unittest.mock import Mock, patch

from harness import (
    MAX_PROVIDED_ASSET_READ_BYTES,
    PROVIDED_ASSETS_MANIFEST,
    TEST_HARDWARE_PROFILE,
    CandidateBootValidator,
    CodexOSHostServices,
    CodexOSRun,
    Frame,
    GenerationGitRecorder,
    HostServiceRequest,
    ProvidedAssets,
    ProvidedAssetsError,
    RuntimeState,
    SnapshotFile,
    configure_provided_assets,
    encode_frame,
)
from harness.codex_generation_worker import _implementor_prompt
from harness.guest_startup import wait_for_ready
from harness.serial_protocol import SerialProtocolDispatcher
from harness.operator_console import main
from tests.test_generation_git import (
    _archive_completed,
    _create_repository,
    _git,
    _git_result,
)


class ProvidedAssetDerivationTests(unittest.TestCase):
    def test_single_files_are_exact_and_descriptors_are_id_ordered(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            supplied = root / "supplied"
            binary = b"\x00\xffopaque\nbytes"
            original_tar = b"not re-archived\x00tar bytes"
            _asset_file(supplied, "zeta", "PROGRAM.EXE", binary)
            _asset_file(supplied, "alpha-one", "source.tar", original_tar)

            assets = ProvidedAssets.from_directory(supplied)

            self.assertEqual(
                [(asset.id, asset.filename, asset.data) for asset in assets.assets],
                [
                    ("alpha-one", "source.tar", original_tar),
                    ("zeta", "PROGRAM.EXE", binary),
                ],
            )
            expected = "".join(
                f"{asset.id}\t{asset.filename}\t{len(asset.data)}\t"
                f"{hashlib.sha256(asset.data).hexdigest()}\n"
                for asset in assets.assets
            ).encode("utf-8")
            self.assertEqual(assets.descriptor_bytes(), expected)

    def test_tree_tar_is_uncompressed_normalized_and_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            first = root / "first"
            second = root / "second"
            for supplied in (first, second):
                tree = supplied / "program"
                (tree / "src" / "empty").mkdir(parents=True)
                (tree / "README").write_bytes(b"read me\n")
                (tree / "src.txt").write_bytes(b"lexical peer\n")
                executable = tree / "src" / "run.bin"
                executable.write_bytes(b"program\x00")
                executable.chmod(0o700)
            os.utime(first / "program" / "README", (1, 1))
            os.utime(second / "program" / "README", (1_900_000_000,) * 2)

            first_asset = ProvidedAssets.from_directory(first).assets[0]
            second_asset = ProvidedAssets.from_directory(second).assets[0]

            self.assertEqual(first_asset.filename, "program.tar")
            self.assertEqual(first_asset.data, second_asset.data)
            self.assertEqual(first_asset.sha256, second_asset.sha256)
            self.assertFalse(first_asset.data.startswith(b"\x1f\x8b"))
            with tarfile.open(
                fileobj=io.BytesIO(first_asset.data), mode="r:"
            ) as archive:
                members = archive.getmembers()
                self.assertEqual(
                    [member.name for member in members],
                    ["README", "src", "src.txt", "src/empty", "src/run.bin"],
                )
                for member in members:
                    self.assertEqual(member.mtime, 0)
                    self.assertEqual((member.uid, member.gid), (0, 0))
                    self.assertEqual((member.uname, member.gname), ("", ""))
                self.assertEqual(archive.extractfile("README").read(), b"read me\n")
                self.assertEqual(archive.getmember("README").mode, 0o644)
                self.assertEqual(archive.getmember("src/run.bin").mode, 0o755)
                self.assertTrue(archive.getmember("src/empty").isdir())

    def test_rejects_invalid_layout_names_symlinks_and_special_files(self) -> None:
        cases: list[tuple[str, Callable[[Path], None]]] = [
            ("invalid ID", lambda root: (root / "Bad_ID").mkdir()),
            ("non-directory", lambda root: (root / "alpha").write_bytes(b"x")),
            (
                "symlink asset",
                lambda root: (root / "alpha").symlink_to(root / "target"),
            ),
            (
                "nested symlink",
                lambda root: _nested_symlink(root),
            ),
            (
                "control filename",
                lambda root: _asset_file(root, "alpha", "bad\nname", b"x"),
            ),
        ]
        if hasattr(os, "mkfifo"):
            cases.append(("special entry", lambda root: _asset_fifo(root)))
        for label, prepare in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as temporary:
                supplied = Path(temporary) / "supplied"
                supplied.mkdir()
                prepare(supplied)
                with self.assertRaises(ProvidedAssetsError):
                    ProvidedAssets.from_directory(supplied)

    def test_rejects_overlong_id_and_invalid_utf8_filename(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            _asset_file(supplied, "a" * 65, "file", b"x")
            with self.assertRaisesRegex(ProvidedAssetsError, "64 bytes"):
                ProvidedAssets.from_directory(supplied)

        if os.name == "posix":
            with tempfile.TemporaryDirectory() as temporary:
                supplied = os.fsencode(Path(temporary) / "supplied")
                os.mkdir(supplied)
                asset = supplied + b"/alpha"
                os.mkdir(asset)
                descriptor = os.open(asset + b"/\xff", os.O_WRONLY | os.O_CREAT, 0o600)
                os.close(descriptor)
                with self.assertRaisesRegex(ProvidedAssetsError, "valid UTF-8"):
                    ProvidedAssets.from_directory(os.fsdecode(supplied))


class ProvidedAssetProvenanceTests(unittest.TestCase):
    def test_freezes_bytes_and_writes_metadata_without_path_or_contents(self) -> None:
        marker = b"PROVIDED-ASSET-PRIVATE-MARKER-811"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "run"
            supplied = root / "external-one"
            source = _asset_file(supplied, "alpha", "payload.bin", marker)

            snapshot = configure_provided_assets(run, supplied)
            self.assertIsNotNone(snapshot)
            manifest = run / PROVIDED_ASSETS_MANIFEST
            encoded = manifest.read_bytes()
            self.assertNotIn(marker, encoded)
            self.assertNotIn(str(supplied).encode(), encoded)
            value = json.loads(encoded.decode("utf-8"))
            self.assertEqual(value["schema_version"], 1)
            self.assertEqual(value["assets"][0]["size"], len(marker))
            self.assertEqual(
                [path.name for path in run.iterdir()],
                [PROVIDED_ASSETS_MANIFEST],
            )

            source.write_bytes(b"replacement")
            response = snapshot.handle_request(
                HostServiceRequest(1, "read_provided_asset", (b"alpha", b"0", b"33"))
            )
            self.assertEqual(_response(response.payload), (0, marker))

    def test_reopen_requires_an_identical_explicit_set_but_not_same_path(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "run"
            first = root / "first"
            second = root / "second"
            _asset_file(first, "alpha", "data.bin", b"same bytes")
            _asset_file(second, "alpha", "data.bin", b"same bytes")
            configure_provided_assets(run, first)

            reopened = configure_provided_assets(run, second)
            self.assertEqual(reopened.assets[0].data, b"same bytes")
            with self.assertRaisesRegex(ProvidedAssetsError, "required"):
                configure_provided_assets(run, None)

            (second / "alpha" / "data.bin").write_bytes(b"changed")
            with self.assertRaisesRegex(ProvidedAssetsError, "does not match"):
                configure_provided_assets(run, second)
            (second / "alpha" / "data.bin").write_bytes(b"same bytes")
            _asset_file(second, "beta", "other", b"new")
            with self.assertRaisesRegex(ProvidedAssetsError, "does not match"):
                configure_provided_assets(run, second)

    def test_existing_gate_initialization_does_not_mutate_archive(self) -> None:
        marker = b"ASSET-MUST-NOT-ENTER-ARCHIVE-971"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "run"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"archived source\n")],
            )
            supplied = root / "supplied"
            _asset_file(supplied, "alpha", "opaque.bin", marker)
            before = _tree_bytes(run / "generation-0000")

            runtime = CodexOSRun(
                run,
                hardware_profile=TEST_HARDWARE_PROFILE,
                provided_assets_directory=supplied,
            )
            runtime.reopen_at_gate()

            self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
            self.assertEqual(_tree_bytes(run / "generation-0000"), before)
            self.assertNotIn(marker, b"".join(before.values()))
            self.assertTrue((run / PROVIDED_ASSETS_MANIFEST).is_file())
            runtime.stop()
            with self.assertRaisesRegex(ProvidedAssetsError, "required"):
                CodexOSRun(run).reopen_at_gate()

    def test_invalid_gate_does_not_initialize_new_asset_provenance(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "run"
            supplied = root / "supplied"
            _asset_file(supplied, "alpha", "data", b"bytes")
            with self.assertRaisesRegex(RuntimeError, "no archived generation"):
                CodexOSRun(
                    run,
                    provided_assets_directory=supplied,
                ).reopen_at_gate()
            self.assertFalse((run / PROVIDED_ASSETS_MANIFEST).exists())

    def test_cli_rejects_invalid_assets_before_boot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            supplied = root / "supplied"
            supplied.mkdir()
            (supplied / "not-an-asset-directory").write_bytes(b"invalid")
            image = root / "initial.iso"
            image.write_bytes(b"image")
            output = io.StringIO()
            with patch.object(CodexOSRun, "_boot_generation") as boot:
                result = main(
                    [
                        "--run-directory",
                        str(root / "run"),
                        "--initial-iso",
                        str(image),
                        "--provided-assets",
                        str(supplied),
                        "--plain",
                    ],
                    io.StringIO(),
                    output,
                )
            self.assertEqual(result, 1)
            self.assertIn("must be a real directory", output.getvalue())
            boot.assert_not_called()


class ProvidedAssetHostServiceTests(unittest.TestCase):
    def test_asset_service_is_available_before_canonical_ready(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            _asset_file(supplied, "alpha", "data", b"startup bytes")
            assets = ProvidedAssets.from_directory(supplied)
            service_name = b"list_provided_assets"
            payload = (
                struct.pack("<H", len(service_name))
                + service_name
                + struct.pack("<H", 0)
            )
            serial = _ScriptedSerial(
                b"boot diagnostic\n"
                + encode_frame(Frame(0x0003, 17, payload))
                + b"CODEXOS-SEED-READY\n"
            )

            protocol = SerialProtocolDispatcher(
                serial,
                startup_host_services=assets,
                background_host_services=assets,
                exchange_host_services=assets,
            )
            try:
                wait_for_ready(protocol, 1.0)
            finally:
                protocol.close()

            self.assertEqual(len(serial.writes), 1)
            response = serial.writes[0]
            magic, version, message_type, request_id, length = struct.unpack(
                "<4sHHII", response[:16]
            )
            self.assertEqual((magic, version), (b"CXOS", 1))
            self.assertEqual((message_type, request_id), (0x8003, 17))
            self.assertEqual(length, len(response) - 16)
            status, descriptor = _response(response[16:])
            self.assertEqual(status, 0)
            self.assertEqual(descriptor, assets.descriptor_bytes())

    def test_lists_empty_and_reads_exact_binary_ranges(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            empty = root / "empty"
            empty.mkdir()
            self.assertEqual(
                _response(
                    ProvidedAssets.from_directory(empty)
                    .handle_request(HostServiceRequest(1, "list_provided_assets", ()))
                    .payload
                ),
                (0, b""),
            )
            malformed_list = ProvidedAssets.from_directory(empty).handle_request(
                HostServiceRequest(7, "list_provided_assets", (b"unexpected",))
            )
            self.assertEqual(_response(malformed_list.payload)[0], 1)
            unconfigured = CodexOSHostServices(
                root / "staging",
                CandidateBootValidator("unused-qemu", TEST_HARDWARE_PROFILE),
            ).handle_request(
                HostServiceRequest(8, "list_provided_assets", ())
            )
            self.assertEqual(_response(unconfigured.payload)[0], 1)

            supplied = root / "supplied"
            data = b"\x00\xffabcdef\x00"
            _asset_file(supplied, "alpha", "payload.bin", data)
            assets = ProvidedAssets.from_directory(supplied)
            for request_id, offset, length, expected in (
                (2, 0, 0, b""),
                (3, 0, 2, data[:2]),
                (4, 2, 4, data[2:6]),
                (5, len(data) - 2, 2, data[-2:]),
                (6, len(data), 0, b""),
            ):
                response = assets.handle_request(
                    HostServiceRequest(
                        request_id,
                        "read_provided_asset",
                        (b"alpha", str(offset).encode(), str(length).encode()),
                    )
                )
                self.assertEqual(_response(response.payload), (0, expected))

    def test_rejects_malformed_and_out_of_range_reads_without_truncation(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            _asset_file(supplied, "alpha", "data", b"0123456789")
            assets = ProvidedAssets.from_directory(supplied)
            invalid = (
                (),
                (b"alpha", b"0"),
                (b"\xff", b"0", b"0"),
                (b"missing", b"0", b"0"),
                (b"alpha", b"", b"0"),
                (b"alpha", b"00", b"0"),
                (b"alpha", b"+1", b"0"),
                (b"alpha", str(1 << 64).encode(), b"0"),
                (b"alpha", b"0", str(MAX_PROVIDED_ASSET_READ_BYTES + 1).encode()),
                (b"alpha", b"10", b"1"),
                (b"alpha", b"9", b"2"),
                (b"alpha", str((1 << 64) - 1).encode(), b"2"),
            )
            for request_id, arguments in enumerate(invalid, 10):
                with self.subTest(arguments=arguments):
                    response = assets.handle_request(
                        HostServiceRequest(
                            request_id,
                            "read_provided_asset",
                            arguments,
                        )
                    )
                    status, diagnostic = _response(response.payload)
                    self.assertEqual(status, 1)
                    self.assertTrue(diagnostic)
                    self.assertLessEqual(len(diagnostic), 1024)

    def test_maximum_read_and_active_dispatch_use_only_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            supplied = root / "supplied"
            data = os.urandom(MAX_PROVIDED_ASSET_READ_BYTES)
            _asset_file(supplied, "alpha", "binary", data)
            assets = ProvidedAssets.from_directory(supplied)
            services = CodexOSHostServices(
                root / "staging",
                CandidateBootValidator("unused-qemu", TEST_HARDWARE_PROFILE),
                provided_assets=assets,
            )

            listed = services.handle_request(
                HostServiceRequest(1, "list_provided_assets", ())
            )
            self.assertEqual(_response(listed.payload)[0], 0)
            read = services.handle_request(
                HostServiceRequest(
                    2,
                    "read_provided_asset",
                    (b"alpha", b"0", str(len(data)).encode()),
                )
            )
            self.assertEqual(_response(read.payload), (0, data))

    def test_candidate_protocol_client_receives_same_frozen_store(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            supplied = Path(temporary) / "supplied"
            _asset_file(supplied, "alpha", "data", b"candidate bytes")
            assets = ProvidedAssets.from_directory(supplied)
            validator = CandidateBootValidator(
                "unused-qemu",
                TEST_HARDWARE_PROFILE,
                provided_assets=assets,
            )
            protocol = Mock(spec=SerialProtocolDispatcher)
            with (
                patch("harness.candidate_boot.wait_for_ready") as ready,
                patch("harness.candidate_boot.ToolClient") as client,
            ):
                result = validator._validate_guest(protocol)

            self.assertEqual(result.status.value, "success")
            ready.assert_called_once_with(protocol, 10.0)
            client.assert_called_once_with(protocol)
            client.return_value.list_tools.assert_called_once_with()

    def test_runtime_wires_one_snapshot_to_active_and_candidate_services(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            supplied = root / "supplied"
            _asset_file(supplied, "alpha", "data", b"shared frozen bytes")
            image = root / "initial.iso"
            image.write_bytes(b"test image")
            runtime = CodexOSRun(
                root / "run",
                "test-qemu",
                hardware_profile=TEST_HARDWARE_PROFILE,
                provided_assets_directory=supplied,
            )
            controller = Mock()
            controller.is_running = False
            controller.pid = 123
            with (
                patch(
                    "harness.generation_runtime.discover_qemu_version",
                    return_value="test qemu",
                ),
                patch(
                    "harness.generation_runtime.QemuProcessController",
                    return_value=controller,
                ),
                patch("harness.generation_runtime.QmpClient"),
                patch("harness.generation_runtime.SerialConnection"),
                patch("harness.generation_runtime.wait_for_ready") as ready,
            ):
                runtime.start(image)
                active = runtime._host_services
                self.assertIsNotNone(active)
                frozen = runtime._provided_assets
                self.assertIs(active._provided_assets, frozen)
                self.assertIs(
                    active._build_service._candidate_validator._provided_assets,
                    frozen,
                )
                ready.assert_called_once()
                protocol = ready.call_args.args[0]
                self.assertIs(protocol._startup_host_services, frozen)
                self.assertIs(protocol._background_host_services, frozen)
                self.assertIs(protocol._exchange_host_services, active)
                runtime.stop()


class ProvidedAssetAutonomousIsolationTests(unittest.TestCase):
    def test_prompt_and_archive_do_not_receive_asset_identity_or_bytes(self) -> None:
        marker = b"AUTONOMOUS-ASSET-CONTEXT-SECRET-441"
        asset_id = "private-alpha"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "run"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"source\n")],
                handoff="Frozen project handoff.",
            )
            supplied = root / "supplied"
            _asset_file(supplied, asset_id, "opaque.bin", marker)
            runtime = CodexOSRun(
                run,
                hardware_profile=TEST_HARDWARE_PROFILE,
                provided_assets_directory=supplied,
            )
            runtime.reopen_at_gate()

            prompt = _implementor_prompt(runtime, None)
            self.assertIn("list_provided_assets", prompt)
            self.assertIn("read_provided_asset", prompt)
            self.assertNotIn(asset_id, prompt)
            self.assertNotIn(marker.decode(), prompt)
            archive_bytes = b"".join(_tree_bytes(run / "generation-0000").values())
            self.assertNotIn(marker, archive_bytes)
            self.assertNotIn(
                marker,
                (run / PROVIDED_ASSETS_MANIFEST).read_bytes(),
            )
            runtime.stop()

    def test_generation_git_reconciliation_ignores_asset_provenance(self) -> None:
        marker = b"GIT-MUST-NOT-RECORD-PROVIDED-ASSET-772"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run = root / "experiment-assets"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"generation source\n")],
            )
            supplied = root / "supplied"
            _asset_file(supplied, "alpha", "opaque.bin", marker)
            runtime = CodexOSRun(
                run,
                hardware_profile=TEST_HARDWARE_PROFILE,
                provided_assets_directory=supplied,
            )
            runtime.reopen_at_gate()
            archive_before = _tree_bytes(run / "generation-0000")
            manifest_before = (run / PROVIDED_ASSETS_MANIFEST).read_bytes()
            repository, _ = _create_repository(root / "repository")

            record = GenerationGitRecorder(
                repository,
                run,
                "test-base",
            ).reconcile()[0]

            tree = _git(repository, "ls-tree", "-r", "--name-only", record.commit)
            self.assertNotIn(PROVIDED_ASSETS_MANIFEST, tree)
            self.assertNotEqual(
                _git_result(
                    repository,
                    "grep",
                    "-F",
                    marker.decode("ascii"),
                    record.commit,
                ).returncode,
                0,
            )
            self.assertEqual(_tree_bytes(run / "generation-0000"), archive_before)
            self.assertEqual(
                (run / PROVIDED_ASSETS_MANIFEST).read_bytes(),
                manifest_before,
            )
            runtime.stop()


def _asset_file(root: Path, asset_id: str, filename: str, data: bytes) -> Path:
    directory = root / asset_id
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / filename
    path.write_bytes(data)
    return path


def _nested_symlink(root: Path) -> None:
    directory = root / "alpha"
    directory.mkdir()
    target = root / "target"
    target.write_bytes(b"target")
    (directory / "link").symlink_to(target)


def _asset_fifo(root: Path) -> None:
    directory = root / "alpha"
    directory.mkdir()
    os.mkfifo(directory / "pipe")


def _response(payload: bytes) -> tuple[int, bytes]:
    return struct.unpack_from("<I", payload)[0], payload[4:]


def _tree_bytes(root: Path) -> dict[str, bytes]:
    return {
        path.relative_to(root).as_posix(): path.read_bytes()
        for path in sorted(root.rglob("*"))
        if path.is_file()
    }


class _ScriptedSerial:
    def __init__(self, incoming: bytes) -> None:
        self._incoming = bytearray(incoming)
        self.writes: list[bytes] = []

    def read(self, max_bytes: int, _timeout_seconds: float) -> bytes:
        if not self._incoming:
            raise TimeoutError
        count = min(max_bytes, len(self._incoming))
        result = bytes(self._incoming[:count])
        del self._incoming[:count]
        return result

    def write(self, data: bytes) -> None:
        self.writes.append(data)

    def close(self) -> None:
        pass


if __name__ == "__main__":
    unittest.main()

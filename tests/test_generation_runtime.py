import json
import os
import shutil
import struct
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from harness import CodexOSRun, RuntimeState, SourceSnapshotError
from harness.generation_runtime import _materialize_snapshot

_TOOLS = [
    "list",
    "read",
    "write",
    "truncate",
    "remove",
    "build",
    "finish_generation",
]


class GenerationRuntimeIntegrationTest(unittest.TestCase):
    def test_preserves_history_when_forking_from_archived_generation(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation_a = b"\n/* GENERATION-RUNTIME-A */\n"
        mutation_b = b"\n/* GENERATION-RUNTIME-B */\n"
        mutation_c = b"\n/* GENERATION-RUNTIME-C */\n"
        generation_zero_handoff = "Generation zero selected mutation A."
        generation_one_handoff = "Generation one selected mutation B."
        generation_two_handoff = "Generation two followed the rollback fork."

        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            run_directory = temporary_path / "run"
            initial_iso = temporary_path / "initial.iso"
            shutil.copyfile(image, initial_iso)
            initial_iso_bytes = initial_iso.read_bytes()
            runtime = CodexOSRun(run_directory, qemu)
            try:
                runtime.start(initial_iso)
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.generation_number, 0)
                generation_zero_pid = runtime.active_pid
                self.assertIsNotNone(generation_zero_pid)
                self.assertEqual(runtime.list_tools(), _TOOLS)
                with self.assertRaisesRegex(RuntimeError, "not awaiting"):
                    runtime.fork_from_generation(0)
                initial_iso.unlink()

                write = runtime.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel)).encode("ascii"),
                        mutation_a,
                    ],
                )
                self.assertEqual(write.status, 0)
                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [generation_zero_handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0)
                self.assertEqual(finish.output, b"")

                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertEqual(runtime.generation_number, 0)
                self.assertIsNone(runtime.active_pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(generation_zero_pid, 0)
                with self.assertRaisesRegex(RuntimeError, "not running"):
                    runtime.list_tools()

                archive = run_directory / "generation-0000"
                self.assertEqual(
                    {path.name for path in run_directory.iterdir()},
                    {"generation-0000"},
                )
                pending = runtime.pending_generation_finish
                self.assertIsNotNone(pending)
                self.assertEqual(
                    runtime.previous_handoff,
                    generation_zero_handoff,
                )
                self.assertEqual(
                    (archive / "handoff.txt").read_bytes(),
                    generation_zero_handoff.encode("utf-8"),
                )
                self.assertEqual(
                    (archive / "source.snapshot").read_bytes(),
                    pending.source_snapshot,
                )
                expected_a = original_kernel + mutation_a
                self.assertEqual(
                    (archive / "source" / "seed" / "kernel.c").read_bytes(),
                    expected_a,
                )
                self.assertEqual(
                    pending.kernel_elf,
                    archive / "successor" / "kernel.elf",
                )
                self.assertEqual(
                    pending.iso,
                    archive / "successor" / "codexos.iso",
                )
                self.assertTrue(pending.kernel_elf.is_file())
                self.assertTrue(pending.iso.is_file())
                self.assertTrue((archive / "qemu.stdout").is_file())
                self.assertTrue((archive / "qemu.stderr").is_file())
                self.assertEqual(
                    (archive / "boot" / "codexos.iso").read_bytes(),
                    initial_iso_bytes,
                )
                self.assertEqual(
                    json.loads((archive / "metadata.json").read_text()),
                    {
                        "generation": 0,
                        "parent_generation": None,
                        "transition": "initial",
                    },
                )
                self.assertEqual(
                    {path.name for path in archive.iterdir()},
                    {
                        "boot",
                        "handoff.txt",
                        "metadata.json",
                        "source.snapshot",
                        "source",
                        "successor",
                        "qemu.stdout",
                        "qemu.stderr",
                    },
                )

                archived_snapshot = (archive / "source.snapshot").read_bytes()
                archived_boot = (archive / "boot" / "codexos.iso").read_bytes()
                successor_boot = pending.iso.read_bytes()
                time.sleep(0.1)
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIsNone(runtime.active_pid)
                self.assertEqual(
                    (archive / "source.snapshot").read_bytes(),
                    archived_snapshot,
                )
                self.assertEqual(
                    (archive / "boot" / "codexos.iso").read_bytes(),
                    archived_boot,
                )

                runtime.continue_generation()
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.generation_number, 1)
                self.assertIsNotNone(runtime.active_pid)
                generation_one_pid = runtime.active_pid
                self.assertEqual(
                    runtime.previous_handoff,
                    generation_zero_handoff,
                )
                self.assertIsNone(runtime.pending_generation_finish)
                read = runtime.invoke_tool(
                    "read",
                    [
                        b"seed/kernel.c",
                        b"0",
                        str(len(expected_a)).encode("ascii"),
                    ],
                )
                self.assertEqual(read.status, 0)
                self.assertEqual(read.output, expected_a)

                write = runtime.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(expected_a)).encode("ascii"),
                        mutation_b,
                    ],
                )
                self.assertEqual(write.status, 0)
                expected_b = expected_a + mutation_b

                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [generation_one_handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0)
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIsNone(runtime.active_pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(generation_one_pid, 0)

                generation_one_archive = run_directory / "generation-0001"
                self.assertEqual(
                    (
                        generation_one_archive / "boot" / "codexos.iso"
                    ).read_bytes(),
                    successor_boot,
                )
                self.assertEqual(
                    json.loads(
                        (generation_one_archive / "metadata.json").read_text()
                    ),
                    {
                        "generation": 1,
                        "parent_generation": 0,
                        "transition": "successor",
                    },
                )
                self.assertEqual(
                    (archive / "boot" / "codexos.iso").read_bytes(),
                    initial_iso_bytes,
                )

                generation_zero_contents = _archive_contents(archive)
                generation_one_contents = _archive_contents(
                    generation_one_archive
                )

                with self.assertRaisesRegex(ValueError, "negative"):
                    runtime.fork_from_generation(-1)
                with self.assertRaisesRegex(ValueError, "earlier"):
                    runtime.fork_from_generation(1)
                with self.assertRaisesRegex(ValueError, "earlier"):
                    runtime.fork_from_generation(2)

                hidden_archive = run_directory / ".generation-0000-hidden"
                archive.rename(hidden_archive)
                try:
                    with self.assertRaisesRegex(FileNotFoundError, "missing"):
                        runtime.fork_from_generation(0)
                finally:
                    hidden_archive.rename(archive)

                successor_iso = archive / "successor" / "codexos.iso"
                hidden_iso = archive / "successor" / ".codexos.iso-hidden"
                successor_iso.rename(hidden_iso)
                try:
                    with self.assertRaisesRegex(FileNotFoundError, "missing"):
                        runtime.fork_from_generation(0)
                finally:
                    hidden_iso.rename(successor_iso)

                metadata_path = archive / "metadata.json"
                metadata_bytes = metadata_path.read_bytes()
                metadata_path.write_bytes(b"not JSON")
                try:
                    with self.assertRaisesRegex(ValueError, "malformed"):
                        runtime.fork_from_generation(0)
                finally:
                    metadata_path.write_bytes(metadata_bytes)

                handoff_path = archive / "handoff.txt"
                handoff_bytes = handoff_path.read_bytes()
                handoff_path.write_bytes(b"\xff")
                try:
                    with self.assertRaisesRegex(ValueError, "UTF-8"):
                        runtime.fork_from_generation(0)
                finally:
                    handoff_path.write_bytes(handoff_bytes)

                self.assertEqual(
                    _archive_contents(archive),
                    generation_zero_contents,
                )
                self.assertEqual(
                    _archive_contents(generation_one_archive),
                    generation_one_contents,
                )
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIsNone(runtime.active_pid)

                selected_successor = successor_iso.read_bytes()
                runtime.fork_from_generation(0)
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.generation_number, 2)
                generation_two_pid = runtime.active_pid
                self.assertIsNotNone(generation_two_pid)
                self.assertEqual(
                    runtime.previous_handoff,
                    generation_zero_handoff,
                )
                self.assertNotEqual(
                    runtime.previous_handoff,
                    generation_one_handoff,
                )
                self.assertIsNone(runtime.pending_generation_finish)

                read = runtime.invoke_tool(
                    "read",
                    [
                        b"seed/kernel.c",
                        b"0",
                        str(len(expected_b)).encode("ascii"),
                    ],
                )
                self.assertEqual(read.status, 0)
                self.assertEqual(read.output, expected_a)
                self.assertIn(mutation_a, read.output)
                self.assertNotIn(mutation_b, read.output)

                write = runtime.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(expected_a)).encode("ascii"),
                        mutation_c,
                    ],
                )
                self.assertEqual(write.status, 0)
                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [generation_two_handoff.encode("utf-8")],
                )
                self.assertEqual(finish.status, 0)
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                self.assertIsNone(runtime.active_pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(generation_two_pid, 0)

                generation_two_archive = run_directory / "generation-0002"
                self.assertEqual(
                    json.loads(
                        (generation_two_archive / "metadata.json").read_text()
                    ),
                    {
                        "generation": 2,
                        "parent_generation": 0,
                        "transition": "rollback",
                    },
                )
                self.assertEqual(
                    (
                        generation_two_archive / "boot" / "codexos.iso"
                    ).read_bytes(),
                    selected_successor,
                )
                self.assertEqual(
                    (
                        generation_two_archive
                        / "source"
                        / "seed"
                        / "kernel.c"
                    ).read_bytes(),
                    expected_a + mutation_c,
                )
                self.assertEqual(
                    _archive_contents(archive),
                    generation_zero_contents,
                )
                self.assertEqual(
                    _archive_contents(generation_one_archive),
                    generation_one_contents,
                )

                runtime.stop()
                self.assertIs(runtime.state, RuntimeState.STOPPED)
                self.assertIsNone(runtime.active_pid)
                runtime.stop()
                self.assertTrue(archive.is_dir())
                self.assertEqual(
                    {path.name for path in run_directory.iterdir()},
                    {
                        "generation-0000",
                        "generation-0001",
                        "generation-0002",
                    },
                )
            finally:
                runtime.stop()

        self.assertEqual(
            (repository / "seed" / "kernel.c").read_bytes(),
            original_kernel,
        )


class GenerationRuntimeStateTests(unittest.TestCase):
    def test_rejects_continuation_from_stopped_and_stop_is_idempotent(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(Path(temporary) / "run")
            with self.assertRaisesRegex(RuntimeError, "not awaiting"):
                runtime.continue_generation()
            with self.assertRaisesRegex(RuntimeError, "not awaiting"):
                runtime.fork_from_generation(0)
            runtime.stop()
            runtime.stop()
            self.assertIs(runtime.state, RuntimeState.STOPPED)

    def test_archive_materialization_rejects_path_escape(self) -> None:
        unsafe_path = b"seed/../../outside"
        snapshot = (
            struct.pack("<HH", 1, len(unsafe_path))
            + unsafe_path
            + struct.pack("<I", 6)
            + b"secret"
        )
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(SourceSnapshotError, "unsafe"):
                _materialize_snapshot(snapshot, root / "source")
            self.assertFalse((root / "outside").exists())


def _build_seed(repository: Path) -> Path:
    make = shutil.which("make")
    if make is None:
        raise AssertionError("make must be installed")
    build = subprocess.run(
        [make, "seed"],
        cwd=repository,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=60.0,
        check=False,
    )
    if build.returncode != 0:
        raise AssertionError(build.stdout)
    return repository / "build" / "seed" / "codexos-seed.iso"


def _archive_contents(archive: Path) -> dict[str, bytes]:
    return {
        path.relative_to(archive).as_posix(): path.read_bytes()
        for path in sorted(archive.rglob("*"))
        if path.is_file()
    }


if __name__ == "__main__":
    unittest.main()

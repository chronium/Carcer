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
    def test_finishes_archives_waits_and_explicitly_starts_successor(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()
        mutation = b"\n/* GENERATION-RUNTIME-SUCCESSOR */\n"
        handoff = "Generation zero selected this exact successor."

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
                initial_iso.unlink()

                write = runtime.invoke_tool(
                    "write",
                    [
                        b"seed/kernel.c",
                        str(len(original_kernel)).encode("ascii"),
                        mutation,
                    ],
                )
                self.assertEqual(write.status, 0)
                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [handoff.encode("utf-8")],
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
                self.assertEqual(runtime.previous_handoff, handoff)
                self.assertEqual(
                    (archive / "handoff.txt").read_bytes(),
                    handoff.encode("utf-8"),
                )
                self.assertEqual(
                    (archive / "source.snapshot").read_bytes(),
                    pending.source_snapshot,
                )
                expected_kernel = original_kernel + mutation
                self.assertEqual(
                    (archive / "source" / "seed" / "kernel.c").read_bytes(),
                    expected_kernel,
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
                self.assertEqual(runtime.previous_handoff, handoff)
                self.assertIsNone(runtime.pending_generation_finish)
                read = runtime.invoke_tool(
                    "read",
                    [
                        b"seed/kernel.c",
                        b"0",
                        str(len(expected_kernel)).encode("ascii"),
                    ],
                )
                self.assertEqual(read.status, 0)
                self.assertEqual(read.output, expected_kernel)

                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                generation_one_handoff = "Generation one selected its successor."
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

                runtime.stop()
                self.assertIs(runtime.state, RuntimeState.STOPPED)
                self.assertIsNone(runtime.active_pid)
                runtime.stop()
                self.assertTrue(archive.is_dir())
                self.assertEqual(
                    {path.name for path in run_directory.iterdir()},
                    {"generation-0000", "generation-0001"},
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


if __name__ == "__main__":
    unittest.main()

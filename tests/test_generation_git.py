import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from harness import (
    TEST_HARDWARE_PROFILE,
    CodexOSRun,
    GenerationGitRecorder,
    GenerationGitRecorderError,
    RuntimeState,
    SnapshotFile,
    encode_source_snapshot,
)
from tests.test_codex_generation_worker import _build_seed


class GenerationGitRecorderTests(unittest.TestCase):
    def test_reconciles_completed_lineage_without_touching_worktree(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, base = _create_repository(root / "repository")
            run = root / "run"
            snapshot_zero = _archive_completed(
                run,
                0,
                None,
                "initial",
                [
                    SnapshotFile("seed/kernel.c", b"generation zero\n"),
                    SnapshotFile("seed/deleted.c", b"removed later\n"),
                ],
            )
            snapshot_one = _archive_completed(
                run,
                1,
                0,
                "successor",
                [SnapshotFile("seed/kernel.c", b"generation zero\none\n")],
            )
            snapshot_two = _archive_completed(
                run,
                2,
                1,
                "successor",
                [SnapshotFile("seed/kernel.c", b"generation zero\none\n")],
            )
            _archive_aborted(run, 3, 2, "successor")
            snapshot_four = _archive_completed(
                run,
                4,
                0,
                "rollback",
                [SnapshotFile("seed/kernel.c", b"generation zero\nrollback\n")],
            )
            hook = repository / ".git" / "hooks" / "pre-commit"
            hook.write_text("#!/bin/sh\nexit 1\n", encoding="utf-8")
            hook.chmod(0o755)
            (repository / "README.md").write_text(
                "uncommitted developer change\n",
                encoding="utf-8",
            )
            status_before = _git(repository, "status", "--porcelain")
            head_before = _git(repository, "rev-parse", "HEAD")
            branch_before = _git(repository, "symbolic-ref", "--short", "HEAD")
            archives_before = _archive_bytes(run)

            recorder = GenerationGitRecorder(repository, run, "test-base")
            records = recorder.reconcile()

            self.assertEqual([item.generation for item in records], [0, 1, 2, 4])
            self.assertTrue(all(not item.already_recorded for item in records))
            commits = {
                generation: _git(
                    repository,
                    "rev-parse",
                    f"experiment/generation-{generation:04d}^{{commit}}",
                ).strip()
                for generation in (0, 1, 2, 4)
            }
            self.assertEqual(_parent(repository, commits[0]), base)
            self.assertEqual(_parent(repository, commits[1]), commits[0])
            self.assertEqual(_parent(repository, commits[2]), commits[1])
            self.assertEqual(_parent(repository, commits[4]), commits[0])
            self.assertNotEqual(commits[1], commits[2])
            self.assertEqual(
                _git(repository, "show", f"{commits[1]}:seed/kernel.c"),
                "generation zero\none\n",
            )
            self.assertEqual(
                _git(repository, "show", f"{commits[4]}:seed/kernel.c"),
                "generation zero\nrollback\n",
            )
            deleted = _git_result(
                repository,
                "cat-file",
                "-e",
                f"{commits[1]}:seed/deleted.c",
            )
            self.assertNotEqual(deleted.returncode, 0)
            self.assertEqual(
                _git(repository, "show", f"{commits[0]}:README.md"),
                "trusted base\n",
            )
            self.assertEqual(
                _git(repository, "show", "-s", "--format=%an <%ae>", commits[0]),
                "Existing Developer <developer@example.invalid>\n",
            )
            expected_message = (
                "CodexOS generation 0\n\n"
                "Generation: 0\n"
                "Parent-Generation: none\n"
                "Transition: initial\n"
                "Outcome: completed\n"
                "Source-Snapshot-SHA256: "
                f"{hashlib.sha256(snapshot_zero).hexdigest()}\n"
                "Recorded-By: CodexOS harness\n"
            )
            self.assertEqual(_commit_message(repository, commits[0]), expected_message)
            self.assertNotEqual(snapshot_zero, snapshot_one)
            self.assertEqual(snapshot_one, snapshot_two)
            self.assertNotEqual(snapshot_zero, snapshot_four)
            self.assertNotEqual(
                _git_result(
                    repository,
                    "show-ref",
                    "--verify",
                    "--quiet",
                    "refs/tags/experiment/generation-0003",
                ).returncode,
                0,
            )
            self.assertEqual(
                _git(repository, "cat-file", "-t", "experiment/generation-0000"),
                "commit\n",
            )

            second = recorder.reconcile()
            self.assertTrue(all(item.already_recorded for item in second))
            self.assertEqual(
                [item.commit for item in second],
                [commits[generation] for generation in (0, 1, 2, 4)],
            )
            self.assertEqual(_git(repository, "status", "--porcelain"), status_before)
            self.assertEqual(_git(repository, "rev-parse", "HEAD"), head_before)
            self.assertEqual(
                _git(repository, "symbolic-ref", "--short", "HEAD"),
                branch_before,
            )
            self.assertEqual(
                _git(repository, "worktree", "list", "--porcelain").count(
                    "worktree "
                ),
                1,
            )
            self.assertEqual(_archive_bytes(run), archives_before)

    def test_conflicting_generation_tag_is_not_rewritten(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, base = _create_repository(root / "repository")
            run = root / "run"
            _archive_completed(
                run,
                0,
                None,
                "initial",
                [SnapshotFile("seed/kernel.c", b"generation zero\n")],
            )
            _git(
                repository,
                "tag",
                "--no-sign",
                "--no-annotate",
                "experiment/generation-0000",
                base,
            )

            recorder = GenerationGitRecorder(repository, run, "test-base")
            with self.assertRaisesRegex(
                GenerationGitRecorderError,
                "conflicting ancestry",
            ):
                recorder.reconcile()
            self.assertEqual(
                _git(
                    repository,
                    "rev-parse",
                    "experiment/generation-0000^{commit}",
                ).strip(),
                base,
            )

    def test_aborted_generation_with_existing_tag_is_a_conflict(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, base = _create_repository(root / "repository")
            run = root / "run"
            _archive_aborted(run, 0, None, "initial")
            _git(
                repository,
                "tag",
                "--no-sign",
                "--no-annotate",
                "experiment/generation-0000",
                base,
            )

            with self.assertRaisesRegex(
                GenerationGitRecorderError,
                "aborted generation has a conflicting Git tag",
            ):
                GenerationGitRecorder(repository, run, "test-base").reconcile()
            self.assertEqual(
                _git(
                    repository,
                    "rev-parse",
                    "experiment/generation-0000^{commit}",
                ).strip(),
                base,
            )


class GenerationGitRecorderQemuIntegrationTest(unittest.TestCase):
    def test_real_runtime_rollback_lineage_becomes_git_branch(self) -> None:
        source_repository = Path(__file__).resolve().parents[1]
        image = _build_seed(source_repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu, "qemu-system-x86_64 must be installed")
        original_kernel = (source_repository / "seed" / "kernel.c").read_bytes()
        mutation_a = b"\n/* GIT-PROVENANCE-A */\n"
        mutation_b = b"\n/* GIT-PROVENANCE-B */\n"
        mutation_c = b"\n/* GIT-PROVENANCE-C */\n"

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repository, base = _create_repository(
                root / "repository",
                source_seed=source_repository / "seed",
            )
            run_directory = root / "run"
            runtime = CodexOSRun(
                run_directory,
                qemu,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            recorder: GenerationGitRecorder | None = None
            try:
                runtime.start(image)
                _append(runtime, len(original_kernel), mutation_a)
                _build_and_finish(runtime, "Generation zero handoff.")
                recorder = GenerationGitRecorder(
                    repository,
                    run_directory,
                    "test-base",
                )
                recorder.reconcile()

                runtime.continue_generation()
                _append(runtime, len(original_kernel + mutation_a), mutation_b)
                _build_and_finish(runtime, "Generation one handoff.")
                recorder.reconcile()

                runtime.fork_from_generation(0)
                _append(runtime, len(original_kernel + mutation_a), mutation_c)
                _build_and_finish(runtime, "Rollback generation handoff.")
                archives_before = _archive_bytes(run_directory)
                recorder.reconcile()

                commits = {
                    generation: _git(
                        repository,
                        "rev-parse",
                        f"experiment/generation-{generation:04d}^{{commit}}",
                    ).strip()
                    for generation in (0, 1, 2)
                }
                self.assertEqual(_parent(repository, commits[0]), base)
                self.assertEqual(_parent(repository, commits[1]), commits[0])
                self.assertEqual(_parent(repository, commits[2]), commits[0])
                self.assertEqual(
                    _git(repository, "show", f"{commits[0]}:seed/kernel.c").encode(),
                    original_kernel + mutation_a,
                )
                self.assertEqual(
                    _git(repository, "show", f"{commits[1]}:seed/kernel.c").encode(),
                    original_kernel + mutation_a + mutation_b,
                )
                rollback_source = _git(
                    repository,
                    "show",
                    f"{commits[2]}:seed/kernel.c",
                ).encode()
                self.assertEqual(
                    rollback_source,
                    original_kernel + mutation_a + mutation_c,
                )
                self.assertNotIn(mutation_b, rollback_source)
                self.assertEqual(_archive_bytes(run_directory), archives_before)
                self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
            finally:
                runtime.stop()


def _create_repository(
    path: Path,
    *,
    source_seed: Path | None = None,
) -> tuple[Path, str]:
    path.mkdir()
    _git(path, "init", "-b", "main")
    _git(path, "config", "user.name", "Existing Developer")
    _git(path, "config", "user.email", "developer@example.invalid")
    (path / "README.md").write_text("trusted base\n", encoding="utf-8")
    if source_seed is None:
        seed = path / "seed"
        seed.mkdir()
        (seed / "kernel.c").write_text("base kernel\n", encoding="utf-8")
        (seed / "deleted.c").write_text("base deletion\n", encoding="utf-8")
    else:
        shutil.copytree(source_seed, path / "seed")
    _git(path, "add", "README.md", "seed")
    _git(path, "commit", "-m", "Trusted experiment base")
    base = _git(path, "rev-parse", "HEAD").strip()
    _git(path, "tag", "--no-sign", "--no-annotate", "test-base", base)
    return path, base


def _archive_completed(
    run: Path,
    generation: int,
    parent: int | None,
    transition: str,
    files: list[SnapshotFile],
) -> bytes:
    archive = run / f"generation-{generation:04d}"
    (archive / "boot").mkdir(parents=True)
    (archive / "source").mkdir()
    (archive / "successor").mkdir()
    metadata = {
        "generation": generation,
        "outcome": "completed",
        "parent_generation": parent,
        "transition": transition,
    }
    (archive / "metadata.json").write_text(
        json.dumps(metadata, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    _write_test_hardware(archive)
    snapshot = encode_source_snapshot(files)
    (archive / "source.snapshot").write_bytes(snapshot)
    for entry in files:
        output = archive / "source" / entry.path
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(entry.content)
    (archive / "handoff.txt").write_text(
        f"Handoff from generation {generation}.",
        encoding="utf-8",
    )
    (archive / "boot" / "codexos.iso").write_bytes(b"boot")
    (archive / "successor" / "kernel.elf").write_bytes(b"kernel")
    (archive / "successor" / "codexos.iso").write_bytes(b"successor")
    (archive / "qemu.stdout").write_bytes(b"")
    (archive / "qemu.stderr").write_bytes(b"")
    return snapshot


def _archive_aborted(
    run: Path,
    generation: int,
    parent: int | None,
    transition: str,
) -> None:
    archive = run / f"generation-{generation:04d}"
    (archive / "boot").mkdir(parents=True)
    metadata = {
        "generation": generation,
        "outcome": "aborted",
        "parent_generation": parent,
        "transition": transition,
    }
    (archive / "metadata.json").write_text(
        json.dumps(metadata, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    _write_test_hardware(archive)
    (archive / "boot" / "codexos.iso").write_bytes(b"boot")
    (archive / "aborted.txt").write_bytes(b"Generation aborted by operator.")
    (archive / "qemu.stdout").write_bytes(b"")
    (archive / "qemu.stderr").write_bytes(b"")


def _write_test_hardware(archive: Path) -> None:
    manifest = TEST_HARDWARE_PROFILE.manifest(
        "QEMU emulator version test"
    )
    (archive / "hardware.json").write_text(
        json.dumps(manifest.as_json_object(), indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def _append(runtime: CodexOSRun, offset: int, data: bytes) -> None:
    result = runtime.invoke_tool(
        "write",
        [b"seed/kernel.c", str(offset).encode("ascii"), data],
    )
    if result.status != 0:
        raise AssertionError(result.output)


def _build_and_finish(runtime: CodexOSRun, handoff: str) -> None:
    build = runtime.invoke_tool("build", [])
    if build.status != 0:
        raise AssertionError(build.output.decode("utf-8", errors="replace"))
    finish = runtime.invoke_tool("finish_generation", [handoff.encode("utf-8")])
    if finish.status != 0:
        raise AssertionError(finish.output.decode("utf-8", errors="replace"))


def _parent(repository: Path, commit: str) -> str:
    values = _git(repository, "rev-list", "--parents", "-n", "1", commit).split()
    if len(values) != 2:
        raise AssertionError(values)
    return values[1]


def _commit_message(repository: Path, commit: str) -> str:
    raw = _git_result(repository, "cat-file", "commit", commit).stdout
    return raw.split(b"\n\n", 1)[1].decode("utf-8")


def _archive_bytes(run: Path) -> dict[str, bytes]:
    return {
        str(path.relative_to(run)): path.read_bytes()
        for path in run.rglob("*")
        if path.is_file()
    }


def _git(repository: Path, *arguments: str) -> str:
    result = _git_result(repository, *arguments)
    if result.returncode != 0:
        raise AssertionError(result.stdout.decode("utf-8", errors="replace"))
    return result.stdout.decode("utf-8")


def _git_result(
    repository: Path,
    *arguments: str,
) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        ["git", "-C", str(repository), *arguments],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=30.0,
        check=False,
    )


if __name__ == "__main__":
    unittest.main()

"""Derived local Git provenance for completed CodexOS generations."""

from __future__ import annotations

import hashlib
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

from .generation_runtime import ArchivedGeneration, CodexOSRun
from .source_snapshot import SnapshotFile, decode_source_snapshot

_GIT_TIMEOUT_SECONDS = 30.0
_MAX_GIT_DIAGNOSTICS = 16 * 1024


class GenerationGitRecorderError(RuntimeError):
    """Local generation provenance could not be recorded safely."""


@dataclass(frozen=True, slots=True)
class GenerationGitRecord:
    generation: int
    tag: str
    commit: str
    already_recorded: bool


class GenerationGitRecorder:
    """Derive immutable local generation commits from authoritative archives."""

    def __init__(
        self,
        repository: str | Path,
        run_directory: str | Path,
        base_ref: str,
    ) -> None:
        git = shutil.which("git")
        if git is None:
            raise GenerationGitRecorderError("git executable is unavailable")
        self._git_executable = git
        self._repository = Path(repository).resolve()
        self._run_directory = Path(run_directory).resolve()
        if not base_ref:
            raise GenerationGitRecorderError("Git base ref must not be empty")
        if self._run_directory.is_symlink() or not self._run_directory.is_dir():
            raise GenerationGitRecorderError(
                f"run directory is unavailable: {self._run_directory}"
            )
        top_level = self._git_text(
            self._repository,
            ["rev-parse", "--show-toplevel"],
        ).strip()
        if Path(top_level).resolve() != self._repository:
            raise GenerationGitRecorderError(
                "Git repository path must be its worktree root"
            )
        self._base_commit = self._resolve_commit(base_ref)

    def reconcile(self) -> list[GenerationGitRecord]:
        try:
            archives = CodexOSRun(self._run_directory).archived_generations()
            records: list[GenerationGitRecord] = []
            for archive in archives:
                if archive.outcome == "completed":
                    records.append(self._record(archive))
                elif self._resolve_optional_tag(
                    _generation_tag(archive.generation)
                ) is not None:
                    raise GenerationGitRecorderError(
                        "aborted generation has a conflicting Git tag: "
                        + _generation_tag(archive.generation)
                    )
            return records
        except GenerationGitRecorderError:
            raise
        except (OSError, ValueError) as error:
            raise GenerationGitRecorderError(str(error)) from error

    def _record(self, archive: ArchivedGeneration) -> GenerationGitRecord:
        snapshot_path = archive.archive_path / "source.snapshot"
        if snapshot_path.is_symlink() or not snapshot_path.is_file():
            raise GenerationGitRecorderError(
                f"generation {archive.generation} source snapshot is unavailable"
            )
        snapshot = snapshot_path.read_bytes()
        entries = decode_source_snapshot(snapshot)
        parent = self._parent_commit(archive)
        tag = _generation_tag(archive.generation)
        message = _commit_message(archive, snapshot)

        existing = self._resolve_optional_tag(tag)
        if existing is not None:
            self._verify_existing(tag, existing, parent, message)
            return GenerationGitRecord(
                archive.generation,
                tag,
                existing,
                True,
            )

        with tempfile.TemporaryDirectory(
            prefix=f"codexos-generation-{archive.generation:04d}-git-"
        ) as temporary:
            worktree = Path(temporary) / "worktree"
            self._git(
                self._repository,
                ["worktree", "add", "--detach", str(worktree), parent],
            )
            failed = False
            try:
                self._replace_seed_tree(worktree, entries)
                self._git(worktree, ["add", "-f", "-A", "--", "seed"])
                self._verify_worktree_scope(worktree)
                self._verify_index(worktree, entries)
                self._git(
                    worktree,
                    ["commit", "--allow-empty", "-m", message.rstrip("\n")],
                )
                commit = self._resolve_commit("HEAD", worktree)
                self._git(
                    self._repository,
                    ["tag", "--no-sign", "--no-annotate", tag, commit],
                )
            except BaseException:
                failed = True
                raise
            finally:
                cleanup = self._git(
                    self._repository,
                    ["worktree", "remove", "--force", str(worktree)],
                    check=False,
                )
                if cleanup.returncode != 0 and worktree.exists():
                    shutil.rmtree(worktree)
                self._git(
                    self._repository,
                    ["worktree", "prune"],
                    check=False,
                )
                if cleanup.returncode != 0 and not failed:
                    raise GenerationGitRecorderError(
                        "failed to remove temporary Git worktree: "
                        + _diagnostics(cleanup.stdout)
                    )

        return GenerationGitRecord(
            archive.generation,
            tag,
            commit,
            False,
        )

    def _parent_commit(self, archive: ArchivedGeneration) -> str:
        if archive.generation == 0:
            return self._base_commit
        parent = archive.parent_generation
        if parent is None:
            raise GenerationGitRecorderError(
                f"generation {archive.generation} has no parent generation"
            )
        tag = _generation_tag(parent)
        commit = self._resolve_optional_tag(tag)
        if commit is None:
            raise GenerationGitRecorderError(
                f"required completed parent tag is missing: {tag}"
            )
        return commit

    def _verify_existing(
        self,
        tag: str,
        commit: str,
        parent: str,
        message: str,
    ) -> None:
        ancestry = self._git_text(
            self._repository,
            ["rev-list", "--parents", "-n", "1", commit],
        ).split()
        if ancestry != [commit, parent]:
            raise GenerationGitRecorderError(
                f"generation tag has conflicting ancestry: {tag}"
            )
        raw_commit = self._git(
            self._repository,
            ["cat-file", "commit", commit],
        ).stdout
        separator = raw_commit.find(b"\n\n")
        if separator < 0:
            raise GenerationGitRecorderError(
                f"generation tag points to a malformed commit: {tag}"
            )
        try:
            existing_message = raw_commit[separator + 2 :].decode("utf-8")
        except UnicodeDecodeError as error:
            raise GenerationGitRecorderError(
                f"generation tag commit message is not UTF-8: {tag}"
            ) from error
        if existing_message != message:
            raise GenerationGitRecorderError(
                f"generation tag has conflicting provenance: {tag}"
            )

    def _replace_seed_tree(
        self,
        worktree: Path,
        entries: tuple[SnapshotFile, ...],
    ) -> None:
        seed = worktree / "seed"
        if seed.is_symlink() or seed.is_file():
            seed.unlink()
        elif seed.exists():
            shutil.rmtree(seed)
        seed.mkdir()
        seed_root = seed.resolve()
        for entry in entries:
            output = (worktree / entry.path).resolve(strict=False)
            if not output.is_relative_to(seed_root):
                raise GenerationGitRecorderError(
                    f"source path escapes temporary seed tree: {entry.path!r}"
                )
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(entry.content)

    def _verify_worktree_scope(self, worktree: Path) -> None:
        status = self._git(
            worktree,
            ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
        ).stdout
        records = status.split(b"\0")
        index = 0
        while index < len(records):
            record = records[index]
            index += 1
            if not record:
                continue
            if len(record) < 4 or record[2:3] != b" ":
                raise GenerationGitRecorderError(
                    "temporary Git worktree status is malformed"
                )
            status_code = record[:2]
            self._verify_seed_path(record[3:])
            if b"R" in status_code or b"C" in status_code:
                if index >= len(records) or not records[index]:
                    raise GenerationGitRecorderError(
                        "temporary Git worktree status is malformed"
                    )
                self._verify_seed_path(records[index])
                index += 1

    def _verify_index(
        self,
        worktree: Path,
        entries: tuple[SnapshotFile, ...],
    ) -> None:
        indexed: dict[bytes, str] = {}
        output = self._git(
            worktree,
            ["ls-files", "-s", "-z", "--", "seed"],
        ).stdout
        for record in output.split(b"\0"):
            if not record:
                continue
            metadata, separator, path = record.partition(b"\t")
            fields = metadata.split()
            if not separator or len(fields) != 3 or fields[2] != b"0":
                raise GenerationGitRecorderError(
                    "temporary Git index is malformed"
                )
            self._verify_seed_path(path)
            indexed[path] = fields[1].decode("ascii")

        expected = {entry.path.encode("utf-8"): entry for entry in entries}
        if set(indexed) != set(expected):
            raise GenerationGitRecorderError(
                "Git cannot represent the source snapshot exactly"
            )
        for path, entry in expected.items():
            blob = self._git(
                worktree,
                ["cat-file", "blob", indexed[path]],
            ).stdout
            if blob != entry.content:
                raise GenerationGitRecorderError(
                    f"Git changed source bytes while staging {entry.path!r}"
                )

    @staticmethod
    def _verify_seed_path(path: bytes) -> None:
        if path != b"seed" and not path.startswith(b"seed/"):
            raise GenerationGitRecorderError(
                "temporary Git worktree changed content outside seed/"
            )

    def _resolve_optional_tag(self, tag: str) -> str | None:
        exists = self._git(
            self._repository,
            ["show-ref", "--verify", "--quiet", f"refs/tags/{tag}"],
            check=False,
        )
        if exists.returncode == 1:
            return None
        if exists.returncode != 0:
            raise GenerationGitRecorderError(
                f"failed to inspect generation tag {tag}: "
                + _diagnostics(exists.stdout)
            )
        return self._resolve_commit(tag)

    def _resolve_commit(self, ref: str, location: Path | None = None) -> str:
        return self._git_text(
            location or self._repository,
            ["rev-parse", "--verify", "--end-of-options", f"{ref}^{{commit}}"],
        ).strip()

    def _git_text(self, location: Path, arguments: list[str]) -> str:
        try:
            return self._git(location, arguments).stdout.decode("utf-8")
        except UnicodeDecodeError as error:
            raise GenerationGitRecorderError(
                "Git returned non-UTF-8 output"
            ) from error

    def _git(
        self,
        location: Path,
        arguments: list[str],
        *,
        check: bool = True,
    ) -> subprocess.CompletedProcess[bytes]:
        command = [
            self._git_executable,
            "-c",
            "core.hooksPath=/dev/null",
            "-C",
            str(location),
            *arguments,
        ]
        try:
            result = subprocess.run(
                command,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                timeout=_GIT_TIMEOUT_SECONDS,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise GenerationGitRecorderError(
                f"Git command failed: {arguments[0]}: {error}"
            ) from error
        if check and result.returncode != 0:
            raise GenerationGitRecorderError(
                f"Git command failed: {arguments[0]}: "
                + _diagnostics(result.stdout)
            )
        return result


def _generation_tag(generation: int) -> str:
    return f"experiment/generation-{generation:04d}"


def _commit_message(archive: ArchivedGeneration, snapshot: bytes) -> str:
    parent = (
        "none"
        if archive.parent_generation is None
        else str(archive.parent_generation)
    )
    digest = hashlib.sha256(snapshot).hexdigest()
    return (
        f"CodexOS generation {archive.generation}\n\n"
        f"Generation: {archive.generation}\n"
        f"Parent-Generation: {parent}\n"
        f"Transition: {archive.transition}\n"
        "Outcome: completed\n"
        f"Source-Snapshot-SHA256: {digest}\n"
        "Recorded-By: CodexOS harness\n"
    )


def _diagnostics(output: bytes) -> str:
    return output[:_MAX_GIT_DIAGNOSTICS].decode("utf-8", errors="replace").strip()

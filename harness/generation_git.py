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
        self._run_identifier = self._run_directory.name
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
        if self._run_identifier == "experiment":
            raise GenerationGitRecorderError(
                "run identifier 'experiment' is reserved for legacy generation tags"
            )
        for ref in (
            "refs/tags/" + _generation_tag(self._run_identifier, 0),
            "refs/heads/" + _lineage_branch(self._run_identifier, 0),
        ):
            valid_ref = self._git(
                self._repository,
                ["check-ref-format", ref],
                check=False,
            )
            if valid_ref.returncode != 0:
                raise GenerationGitRecorderError(
                    "run-directory basename cannot form a Git tag namespace "
                    "or lineage branch namespace: "
                    + self._run_identifier
                )
        self._base_commit = self._resolve_commit(base_ref)

    @property
    def base_commit(self) -> str:
        """The immutable commit selected by the configured base ref."""
        return self._base_commit

    def generation_tag_commit(self, tag: str) -> str:
        """Resolve one required immutable annotated generation tag."""
        commit = self._resolve_optional_tag(tag)
        if commit is None:
            raise GenerationGitRecorderError(
                f"required completed generation tag is missing: {tag}"
            )
        object_type = self._git_text(
            self._repository,
            ["cat-file", "-t", f"refs/tags/{tag}"],
        ).strip()
        if object_type != "tag":
            raise GenerationGitRecorderError(
                f"generation tag is not annotated: {tag}"
            )
        return commit

    def reconcile(self) -> list[GenerationGitRecord]:
        try:
            archives = CodexOSRun(self._run_directory).archived_generations()
            CodexOSRun._validate_archived_history(archives)
            records: list[GenerationGitRecord] = []
            for archive in archives:
                if archive.outcome == "completed":
                    records.append(self._record(archive))
                elif self._resolve_optional_tag(
                    _generation_tag(
                        self._run_identifier,
                        archive.generation,
                    )
                ) is not None:
                    raise GenerationGitRecorderError(
                        "aborted generation has a conflicting Git tag: "
                        + _generation_tag(
                            self._run_identifier,
                            archive.generation,
                        )
                    )
            self._reconcile_lineages(archives, records)
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
        tag = _generation_tag(self._run_identifier, archive.generation)
        message = _commit_message(archive, snapshot)
        tag_message = _tag_message(archive, self._run_identifier)

        existing = self._resolve_optional_tag(tag)
        if existing is not None:
            self._verify_existing(
                tag,
                existing,
                parent,
                message,
                tag_message,
            )
            return GenerationGitRecord(
                archive.generation,
                tag,
                existing,
                True,
            )

        with tempfile.TemporaryDirectory(
            prefix=f"codexos-generation-{archive.generation:04d}-git-"
        ) as temporary:
            tag_message_path = Path(temporary) / "tag-message"
            tag_message_path.write_text(tag_message, encoding="utf-8")
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
                    [
                        "tag",
                        "--annotate",
                        "--no-sign",
                        "--cleanup=verbatim",
                        "--file",
                        str(tag_message_path),
                        "--",
                        tag,
                        commit,
                    ],
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
        tag = _generation_tag(self._run_identifier, parent)
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
        tag_message: str,
    ) -> None:
        object_type = self._git_text(
            self._repository,
            ["cat-file", "-t", f"refs/tags/{tag}"],
        ).strip()
        if object_type != "tag":
            raise GenerationGitRecorderError(
                f"generation tag is not annotated: {tag}"
            )
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
        raw_tag = self._git(
            self._repository,
            ["cat-file", "tag", f"refs/tags/{tag}"],
        ).stdout
        tag_separator = raw_tag.find(b"\n\n")
        if tag_separator < 0:
            raise GenerationGitRecorderError(
                f"generation tag object is malformed: {tag}"
            )
        tag_headers = raw_tag[:tag_separator].splitlines()
        if (
            f"object {commit}".encode("ascii") not in tag_headers
            or b"type commit" not in tag_headers
        ):
            raise GenerationGitRecorderError(
                f"generation tag does not directly target its commit: {tag}"
            )
        try:
            existing_tag_message = raw_tag[tag_separator + 2 :].decode("utf-8")
        except UnicodeDecodeError as error:
            raise GenerationGitRecorderError(
                f"generation tag message is not UTF-8: {tag}"
            ) from error
        if existing_tag_message != tag_message:
            raise GenerationGitRecorderError(
                f"generation tag has conflicting annotation: {tag}"
            )

    def _reconcile_lineages(
        self,
        archives: list[ArchivedGeneration],
        records: list[GenerationGitRecord],
    ) -> None:
        commits = {record.generation: record.commit for record in records}
        generation_lineage: dict[int, int] = {}
        lineage_commits: dict[int, list[tuple[int, str]]] = {}
        next_lineage = 0
        for archive in archives:
            if archive.outcome != "completed":
                continue
            if archive.generation == 0:
                lineage = next_lineage
                next_lineage += 1
            elif archive.transition == "rollback":
                lineage = next_lineage
                next_lineage += 1
            else:
                parent = archive.parent_generation
                try:
                    lineage = generation_lineage[parent]
                except KeyError as error:
                    raise GenerationGitRecorderError(
                        f"generation {archive.generation} has no completed "
                        "lineage parent"
                    ) from error
            generation_lineage[archive.generation] = lineage
            lineage_commits.setdefault(lineage, []).append(
                (archive.generation, commits[archive.generation])
            )

        expected = {
            _lineage_branch(self._run_identifier, lineage): entries[-1][1]
            for lineage, entries in lineage_commits.items()
        }
        self._reject_unexpected_lineage_refs(set(expected))
        active_lineage = max(lineage_commits, default=None)
        updates: list[tuple[str, str, str]] = []
        for lineage, entries in lineage_commits.items():
            branch = _lineage_branch(self._run_identifier, lineage)
            expected_commit = entries[-1][1]
            existing = self._resolve_optional_branch(branch)
            if existing is None:
                updates.append((branch, expected_commit, ""))
                continue
            if existing == expected_commit:
                continue
            known_positions = {
                commit: position
                for position, (_, commit) in enumerate(entries)
            }
            position = known_positions.get(existing)
            if (
                lineage != active_lineage
                or position is None
                or self._branch_has_reached(branch, expected_commit)
            ):
                raise GenerationGitRecorderError(
                    f"lineage branch has conflicting provenance: {branch}"
                )
            if position >= len(entries) - 1 or not self._is_ancestor(
                existing,
                expected_commit,
            ):
                raise GenerationGitRecorderError(
                    f"lineage branch cannot be fast-forwarded safely: {branch}"
                )
            updates.append((branch, expected_commit, existing))

        for branch, new_commit, old_commit in updates:
            self._git(
                self._repository,
                [
                    "update-ref",
                    "--create-reflog",
                    "-m",
                    "CodexOS lineage reconciliation",
                    f"refs/heads/{branch}",
                    new_commit,
                    old_commit,
                ],
            )

    def _reject_unexpected_lineage_refs(self, expected: set[str]) -> None:
        prefix = f"refs/heads/{self._run_identifier}/lineage-"
        refs = self._git_text(
            self._repository,
            [
                "for-each-ref",
                "--format=%(refname)",
                f"refs/heads/{self._run_identifier}",
            ],
        ).splitlines()
        for ref in refs:
            if not ref.startswith(prefix):
                continue
            suffix = ref.removeprefix(prefix)
            if (
                suffix.isascii()
                and suffix.isdecimal()
                and ref == "refs/heads/" + _lineage_branch(
                    self._run_identifier,
                    int(suffix),
                )
                and ref.removeprefix("refs/heads/") not in expected
            ):
                raise GenerationGitRecorderError(
                    f"unexpected lineage branch conflicts with archives: {ref}"
                )

    def _resolve_optional_branch(self, branch: str) -> str | None:
        ref = f"refs/heads/{branch}"
        exists = self._git(
            self._repository,
            ["show-ref", "--verify", "--quiet", ref],
            check=False,
        )
        if exists.returncode == 1:
            return None
        if exists.returncode != 0:
            raise GenerationGitRecorderError(
                f"failed to inspect lineage branch {branch}: "
                + _diagnostics(exists.stdout)
            )
        symbolic = self._git(
            self._repository,
            ["symbolic-ref", "--quiet", ref],
            check=False,
        )
        if symbolic.returncode == 0:
            raise GenerationGitRecorderError(
                f"lineage branch must be a direct branch ref: {branch}"
            )
        if symbolic.returncode != 1:
            raise GenerationGitRecorderError(
                f"failed to inspect lineage branch {branch}: "
                + _diagnostics(symbolic.stdout)
            )
        target = self._git_text(
            self._repository,
            ["rev-parse", "--verify", "--end-of-options", ref],
        ).strip()
        object_type = self._git_text(
            self._repository,
            ["cat-file", "-t", target],
        ).strip()
        if object_type != "commit":
            raise GenerationGitRecorderError(
                f"lineage branch does not directly target a commit: {branch}"
            )
        return target

    def _branch_has_reached(self, branch: str, commit: str) -> bool:
        ref = f"refs/heads/{branch}"
        exists = self._git(
            self._repository,
            ["reflog", "exists", ref],
            check=False,
        )
        if exists.returncode == 1:
            return False
        if exists.returncode != 0:
            raise GenerationGitRecorderError(
                f"failed to inspect lineage branch history {branch}: "
                + _diagnostics(exists.stdout)
            )
        history = self._git_text(
            self._repository,
            ["reflog", "show", "--format=%H", ref],
        ).splitlines()
        return commit in history

    def _is_ancestor(self, ancestor: str, descendant: str) -> bool:
        result = self._git(
            self._repository,
            ["merge-base", "--is-ancestor", ancestor, descendant],
            check=False,
        )
        if result.returncode == 0:
            return True
        if result.returncode == 1:
            return False
        raise GenerationGitRecorderError(
            "failed to verify lineage branch ancestry: "
            + _diagnostics(result.stdout)
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


def _generation_tag(run_identifier: str, generation: int) -> str:
    return f"{run_identifier}/generation-{generation:04d}"


def _lineage_branch(run_identifier: str, lineage: int) -> str:
    return f"{run_identifier}/lineage-{lineage:04d}"


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


def _tag_message(archive: ArchivedGeneration, run_identifier: str) -> str:
    parent = (
        "none"
        if archive.parent_generation is None
        else str(archive.parent_generation)
    )
    if archive.handoff is None:
        raise GenerationGitRecorderError(
            f"completed generation {archive.generation} has no handoff"
        )
    return (
        f"CodexOS generation {archive.generation}\n\n"
        f"Run: {run_identifier}\n"
        f"Generation: {archive.generation}\n"
        f"Parent-Generation: {parent}\n"
        f"Transition: {archive.transition}\n"
        "Outcome: completed\n\n"
        "Handoff:\n"
        f"{archive.handoff}"
    )


def _diagnostics(output: bytes) -> str:
    return output[:_MAX_GIT_DIAGNOSTICS].decode("utf-8", errors="replace").strip()

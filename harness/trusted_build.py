"""Fixed trusted build procedure for an untrusted CodexOS source snapshot."""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import BinaryIO

from .source_snapshot import SnapshotFile, SourceSnapshotError, decode_source_snapshot

_DIAGNOSTIC_LIMIT = 64 * 1024
_STEP_TIMEOUT_SECONDS = 60.0
_C_FLAGS = (
    "-std=c11",
    "-O2",
    "-Wall",
    "-Wextra",
    "-Werror",
    "-ffreestanding",
    "-fno-stack-protector",
    "-fno-pic",
    "-fno-pie",
    "-fno-asynchronous-unwind-tables",
    "-m64",
    "-march=x86-64",
    "-mno-red-zone",
    "-mno-mmx",
    "-mno-sse",
    "-mno-sse2",
    "-mcmodel=kernel",
)
_ASSEMBLY_FLAGS = _C_FLAGS[5:]
_LINK_FLAGS = ("-static", "--build-id=none", "-z", "max-page-size=0x1000")


class BuildStatus(Enum):
    SUCCESS = "success"
    BUILD_FAILURE = "build_failure"
    HARNESS_FAILURE = "harness_failure"


@dataclass(frozen=True)
class BuildResult:
    status: BuildStatus
    diagnostics: str
    kernel_elf: Path | None = None
    iso: Path | None = None


class _BuildFailure(RuntimeError):
    pass


@dataclass(frozen=True)
class _SandboxMounts:
    bwrap: str
    toolchain: Path
    trusted: Path
    xorriso: Path
    libraries: tuple[tuple[Path, Path], ...]


def build_source_snapshot(
    snapshot_data: bytes,
    output_directory: str | os.PathLike[str],
) -> BuildResult:
    """Build one validated snapshot without executing guest build instructions."""
    try:
        files = decode_source_snapshot(snapshot_data)
        _validate_required_inputs(files)
        executables = _find_executables()
        repository = Path(__file__).resolve().parents[1]
        limine = _limine_inputs(repository)
        output = Path(output_directory).resolve(strict=False)
        if output == repository or output.is_relative_to(repository):
            raise SourceSnapshotError(
                "build output directory must be outside the repository"
            )
    except (OSError, SourceSnapshotError) as error:
        return BuildResult(BuildStatus.HARNESS_FAILURE, str(error))

    try:
        with tempfile.TemporaryDirectory(prefix="codexos-build-") as temporary:
            workspace = Path(temporary).resolve()
            _materialize(files, workspace)
            build = workspace / "build"
            objects = build / "objects"
            iso_root = build / "iso-root"
            objects.mkdir(parents=True)
            trusted_limine = _copy_limine_inputs(limine, workspace)

            diagnostics_path = build / "diagnostics.log"
            with diagnostics_path.open("w+b") as diagnostics:
                limine_tool = workspace / "trusted" / "limine" / "limine"
                try:
                    _run(
                        [
                            executables["cc"],
                            "-std=c99",
                            "-O2",
                            str(trusted_limine["limine.c"]),
                            "-o",
                            str(limine_tool),
                        ],
                        workspace,
                        diagnostics,
                    )
                    sandbox = _prepare_sandbox(
                        executables,
                        limine_tool,
                        workspace / "trusted",
                    )
                except _BuildFailure as error:
                    return BuildResult(
                        BuildStatus.HARNESS_FAILURE,
                        _read_diagnostics(diagnostics, str(error)),
                    )

                try:
                    object_paths = _compile_guest_sources(
                        files,
                        workspace,
                        objects,
                        executables["x86_64-elf-gcc"],
                        sandbox,
                        diagnostics,
                    )
                    embedded_source = build / "embedded-sources.c"
                    embedded_source.write_text(
                        _render_embedded_sources(files),
                        encoding="utf-8",
                        newline="\n",
                    )
                    embedded_object = objects / "embedded-sources.o"
                    _run_sandboxed(
                        [
                            executables["x86_64-elf-gcc"],
                            *_C_FLAGS,
                            "-I",
                            "/workspace/seed",
                            "-c",
                            _sandbox_path(embedded_source, workspace),
                            "-o",
                            _sandbox_path(embedded_object, workspace),
                        ],
                        workspace,
                        sandbox,
                        diagnostics,
                    )
                    object_paths.append(embedded_object)

                    kernel = build / "kernel.elf"
                    _run_sandboxed(
                        [
                            executables["x86_64-elf-ld"],
                            *_LINK_FLAGS,
                            "-T",
                            "/workspace/seed/linker.ld",
                            *(
                                _sandbox_path(path, workspace)
                                for path in object_paths
                            ),
                            "-o",
                            _sandbox_path(kernel, workspace),
                        ],
                        workspace,
                        sandbox,
                        diagnostics,
                    )

                    iso = _create_iso(
                        workspace,
                        iso_root,
                        kernel,
                        limine_tool,
                        trusted_limine,
                        executables["xorriso"],
                        sandbox,
                        diagnostics,
                    )
                except _BuildFailure as error:
                    return BuildResult(
                        BuildStatus.BUILD_FAILURE,
                        _read_diagnostics(diagnostics, str(error)),
                    )

                diagnostics_text = _read_diagnostics(diagnostics)
                output.mkdir(parents=True, exist_ok=True)
                final_kernel = output / "kernel.elf"
                final_iso = output / "codexos.iso"
                if os.path.lexists(final_kernel) or os.path.lexists(final_iso):
                    return BuildResult(
                        BuildStatus.HARNESS_FAILURE,
                        "build output files already exist",
                    )
                shutil.copyfile(kernel, final_kernel)
                shutil.copyfile(iso, final_iso)
                return BuildResult(
                    BuildStatus.SUCCESS,
                    diagnostics_text,
                    final_kernel,
                    final_iso,
                )
    except (OSError, SourceSnapshotError) as error:
        return BuildResult(BuildStatus.HARNESS_FAILURE, str(error))


def _validate_required_inputs(files: tuple[SnapshotFile, ...]) -> None:
    paths = {entry.path for entry in files}
    required = {"seed/files.h", "seed/linker.ld", "seed/limine.conf"}
    missing = sorted(required - paths)
    if missing:
        raise SourceSnapshotError(f"missing required build input: {missing[0]}")
    if not any(entry.path.endswith((".c", ".S")) for entry in files):
        raise SourceSnapshotError("source snapshot contains no C or assembly source")


def _find_executables() -> dict[str, str]:
    result: dict[str, str] = {}
    for name in (
        "bwrap",
        "cc",
        "ldd",
        "x86_64-elf-gcc",
        "x86_64-elf-ld",
        "xorriso",
    ):
        executable = shutil.which(name)
        if executable is None:
            raise SourceSnapshotError(f"missing required build utility: {name}")
        result[name] = executable
    return result


def _limine_inputs(repository: Path) -> dict[str, Path]:
    directory = repository / "third_party" / "limine"
    names = (
        "limine.c",
        "limine-bios-hdd.h",
        "limine-bios.sys",
        "limine-bios-cd.bin",
    )
    result = {name: directory / name for name in names}
    for name, path in result.items():
        if not path.is_file():
            raise SourceSnapshotError(f"missing pinned Limine input: {name}")
    return result


def _copy_limine_inputs(inputs: dict[str, Path], workspace: Path) -> dict[str, Path]:
    directory = workspace / "trusted" / "limine"
    directory.mkdir(parents=True)
    copied: dict[str, Path] = {}
    for name, source in inputs.items():
        destination = directory / name
        shutil.copyfile(source, destination)
        copied[name] = destination
    return copied


def _materialize(files: tuple[SnapshotFile, ...], workspace: Path) -> None:
    for entry in files:
        destination = (workspace / entry.path).resolve(strict=False)
        if not destination.is_relative_to(workspace):
            raise SourceSnapshotError(f"source path escapes build workspace: {entry.path}")
        try:
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(entry.content)
        except OSError as error:
            raise SourceSnapshotError(
                f"cannot materialize source path {entry.path!r}: {error}"
            ) from error


def _prepare_sandbox(
    executables: dict[str, str],
    limine_tool: Path,
    trusted: Path,
) -> _SandboxMounts:
    compiler = Path(executables["x86_64-elf-gcc"]).resolve()
    linker = Path(executables["x86_64-elf-ld"]).resolve()
    toolchain = compiler.parent.parent
    if not linker.is_relative_to(toolchain):
        raise SourceSnapshotError("cross compiler and linker have different prefixes")

    compiler_programs = tuple(
        Path(
            _capture_trusted(
                [str(compiler), f"-print-prog-name={program}"]
            ).strip()
        ).resolve()
        for program in ("cc1", "as")
    )
    for program in compiler_programs:
        if not program.is_file() or not program.is_relative_to(toolchain):
            raise SourceSnapshotError(
                f"cross compiler program is outside its prefix: {program}"
            )

    xorriso = Path(executables["xorriso"]).resolve()
    libraries = _runtime_libraries(
        executables["ldd"],
        (compiler, linker, xorriso, limine_tool, *compiler_programs),
    )
    return _SandboxMounts(
        executables["bwrap"],
        toolchain,
        trusted,
        xorriso,
        libraries,
    )


def _capture_trusted(command: list[str]) -> str:
    try:
        completed = subprocess.run(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=_STEP_TIMEOUT_SECONDS,
            check=False,
            shell=False,
            text=True,
            env={"LC_ALL": "C", "PATH": os.defpath},
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise SourceSnapshotError(f"cannot inspect build utility: {command[0]}") from error
    if completed.returncode != 0:
        raise SourceSnapshotError(f"cannot inspect build utility: {command[0]}")
    return completed.stdout


def _runtime_libraries(
    ldd: str,
    executables: tuple[Path, ...],
) -> tuple[tuple[Path, Path], ...]:
    libraries: dict[Path, Path] = {}
    for executable in executables:
        output = _capture_trusted([ldd, str(executable)])
        for line in output.splitlines():
            fields = line.strip().split()
            if not fields or fields[0] == "linux-vdso.so.1":
                continue
            if len(fields) >= 3 and fields[1] == "=>":
                if fields[2] == "not":
                    raise SourceSnapshotError(
                        f"missing runtime library for build utility: {fields[0]}"
                    )
                destination = Path(fields[2])
            elif fields[0].startswith("/"):
                destination = Path(fields[0])
            else:
                continue
            source = destination.resolve()
            if not source.is_file():
                raise SourceSnapshotError(
                    f"missing runtime library for build utility: {destination}"
                )
            libraries[destination] = source
    return tuple(sorted(libraries.items(), key=lambda item: str(item[0])))


def _compile_guest_sources(
    files: tuple[SnapshotFile, ...],
    workspace: Path,
    objects: Path,
    compiler: str,
    sandbox: _SandboxMounts,
    diagnostics: BinaryIO,
) -> list[Path]:
    source_paths = sorted(
        entry.path for entry in files if entry.path.endswith((".c", ".S"))
    )
    object_paths: list[Path] = []
    for index, source_path in enumerate(source_paths):
        source = workspace / source_path
        object_path = objects / f"guest-{index}.o"
        flags = _C_FLAGS if source_path.endswith(".c") else _ASSEMBLY_FLAGS
        _run_sandboxed(
            [
                compiler,
                *flags,
                "-I",
                "/workspace/seed",
                "-c",
                _sandbox_path(source, workspace),
                "-o",
                _sandbox_path(object_path, workspace),
            ],
            workspace,
            sandbox,
            diagnostics,
        )
        object_paths.append(object_path)
    return object_paths


def _render_embedded_sources(files: tuple[SnapshotFile, ...]) -> str:
    entries = sorted(files, key=lambda entry: entry.path)
    lines = ['#include "files.h"', ""]
    for index, entry in enumerate(entries):
        path = entry.path.encode("utf-8")
        lines.append(_byte_array(f"embedded_path_{index}", path, read_only=True))
        lines.append(
            _byte_array(f"embedded_content_{index}", entry.content, read_only=False)
        )
    lines.extend(["", "const struct embedded_file initial_files[] = {"])
    for index, entry in enumerate(entries):
        lines.extend(
            [
                "    {",
                f"        embedded_path_{index},",
                f"        {len(entry.path.encode('utf-8'))}u,",
                f"        embedded_content_{index},",
                f"        embedded_content_{index} + {len(entry.content)}u,",
                "    },",
            ]
        )
    lines.extend(
        [
            "};",
            "",
            (
                "const uint32_t initial_file_count = "
                "sizeof(initial_files) / sizeof(initial_files[0]);"
            ),
        ]
    )
    return "\n".join(lines) + "\n"


def _byte_array(name: str, data: bytes, *, read_only: bool) -> str:
    declaration = "static const uint8_t" if read_only else "static uint8_t"
    if not data:
        return f"{declaration} {name}[1] = {{0}};"
    values = ", ".join(f"0x{byte:02x}" for byte in data)
    return f"{declaration} {name}[] = {{{values}}};"


def _create_iso(
    workspace: Path,
    iso_root: Path,
    kernel: Path,
    limine_tool: Path,
    limine: dict[str, Path],
    xorriso: str,
    sandbox: _SandboxMounts,
    diagnostics: BinaryIO,
) -> Path:
    boot = iso_root / "boot"
    limine_directory = boot / "limine"
    limine_directory.mkdir(parents=True)
    shutil.copyfile(kernel, boot / "kernel.elf")
    shutil.copyfile(workspace / "seed" / "limine.conf", limine_directory / "limine.conf")
    shutil.copyfile(limine["limine-bios.sys"], limine_directory / "limine-bios.sys")
    shutil.copyfile(
        limine["limine-bios-cd.bin"],
        limine_directory / "limine-bios-cd.bin",
    )

    iso = workspace / "build" / "codexos.iso"
    _run_sandboxed(
        [
            xorriso,
            "-as",
            "mkisofs",
            "-R",
            "-r",
            "-J",
            "-V",
            "CODEXOS_SEED",
            "--modification-date=2020010100000000",
            "--set_all_file_dates",
            "2020010100000000",
            "-b",
            "boot/limine/limine-bios-cd.bin",
            "-no-emul-boot",
            "-boot-load-size",
            "4",
            "-boot-info-table",
            _sandbox_path(iso_root, workspace),
            "-o",
            _sandbox_path(iso, workspace),
        ],
        workspace,
        sandbox,
        diagnostics,
    )
    _run_sandboxed(
        [
            _sandbox_path(limine_tool, workspace),
            "bios-install",
            _sandbox_path(iso, workspace),
        ],
        workspace,
        sandbox,
        diagnostics,
    )
    return iso


def _sandbox_path(path: Path, workspace: Path) -> str:
    try:
        relative = path.resolve().relative_to(workspace)
    except ValueError as error:
        raise SourceSnapshotError(
            f"build path is outside the temporary workspace: {path}"
        ) from error
    return (Path("/workspace") / relative).as_posix()


def _run_sandboxed(
    command: list[str],
    workspace: Path,
    sandbox: _SandboxMounts,
    diagnostics: BinaryIO,
) -> None:
    directories = _sandbox_directories(
        (sandbox.toolchain, sandbox.xorriso)
        + tuple(destination for destination, _ in sandbox.libraries)
    )
    wrapped = [
        sandbox.bwrap,
        "--unshare-all",
        "--die-with-parent",
        "--new-session",
        "--clearenv",
        "--setenv",
        "HOME",
        "/nonexistent",
        "--setenv",
        "LC_ALL",
        "C",
        "--setenv",
        "PATH",
        f"{sandbox.toolchain / 'bin'}:/usr/bin",
        "--setenv",
        "TMPDIR",
        "/tmp",
        "--hostname",
        "codexos-build",
        "--cap-drop",
        "ALL",
        "--dir",
        "/workspace",
        "--bind",
        str(workspace),
        "/workspace",
        "--ro-bind",
        str(sandbox.trusted),
        "/workspace/trusted",
        "--dir",
        "/tmp",
        "--tmpfs",
        "/tmp",
        "--dev",
        "/dev",
    ]
    for directory in directories:
        wrapped.extend(("--dir", str(directory)))
    wrapped.extend(
        (
            "--ro-bind",
            str(sandbox.toolchain),
            str(sandbox.toolchain),
            "--ro-bind",
            str(sandbox.xorriso),
            str(sandbox.xorriso),
        )
    )
    for destination, source in sandbox.libraries:
        wrapped.extend(("--ro-bind", str(source), str(destination)))
    wrapped.extend(("--chdir", "/workspace", "--", *command))
    _run(wrapped, workspace, diagnostics)


def _sandbox_directories(paths: tuple[Path, ...]) -> tuple[Path, ...]:
    directories: set[Path] = set()
    for path in paths:
        parent = path.parent
        while parent != Path("/"):
            directories.add(parent)
            parent = parent.parent
    return tuple(sorted(directories, key=lambda path: (len(path.parts), str(path))))


def _run(command: list[str], workspace: Path, diagnostics: BinaryIO) -> None:
    try:
        completed = subprocess.run(
            command,
            cwd=workspace,
            stdin=subprocess.DEVNULL,
            stdout=diagnostics,
            stderr=subprocess.STDOUT,
            timeout=_STEP_TIMEOUT_SECONDS,
            check=False,
            shell=False,
        )
    except subprocess.TimeoutExpired as error:
        raise _BuildFailure(f"build step timed out: {command[0]}") from error
    if completed.returncode != 0:
        raise _BuildFailure(
            f"build step failed with exit code {completed.returncode}: {command[0]}"
        )


def _read_diagnostics(diagnostics: BinaryIO, message: str = "") -> str:
    diagnostics.flush()
    diagnostics.seek(0)
    captured = diagnostics.read(_DIAGNOSTIC_LIMIT + 1)
    text = captured[:_DIAGNOSTIC_LIMIT].decode("utf-8", errors="replace")
    if len(captured) > _DIAGNOSTIC_LIMIT:
        text += "\n[diagnostics truncated]\n"
    if message:
        text = f"{message}\n{text}"
    return text[:_DIAGNOSTIC_LIMIT]

"""Concrete CodexOS runtime across manually gated generations."""

from __future__ import annotations

import json
import shutil
import tempfile
import time
from collections.abc import Sequence
from enum import Enum
from pathlib import Path

from .generation_finish_host_service import (
    CodexOSHostServices,
    PendingGenerationFinish,
)
from .qemu import QemuProcessController
from .qmp import QmpClient, QmpError
from .serial import SerialConnection
from .source_snapshot import decode_source_snapshot
from .tool_protocol import ToolClient, ToolResult

_READY_MARKER = b"CODEXOS-SEED-READY\n"
_ABORT_MARKER = b"Generation aborted by operator."
_STARTUP_TIMEOUT_SECONDS = 10.0
_QEMU_EXIT_TIMEOUT_SECONDS = 2.0


class RuntimeState(Enum):
    STOPPED = "stopped"
    RUNNING = "running"
    PAUSED = "paused"
    AWAITING_NEXT_GENERATION = "awaiting_next_generation"


class CodexOSRun:
    """Run one concrete CodexOS lineage with explicit generation gates."""

    def __init__(
        self,
        run_directory: str | Path,
        qemu_executable: str = "qemu-system-x86_64",
    ) -> None:
        self._run_directory = Path(run_directory).resolve()
        self._run_directory.mkdir(parents=True, exist_ok=True)
        self._qemu_executable = qemu_executable
        self._state = RuntimeState.STOPPED
        self._generation_number: int | None = None
        self._current_boot_image: Path | None = None
        self._current_parent_generation: int | None = None
        self._current_transition: str | None = None
        self._previous_handoff: str | None = None
        self._pending_finish: PendingGenerationFinish | None = None

        self._workspace: tempfile.TemporaryDirectory[str] | None = None
        self._stdout_path: Path | None = None
        self._stderr_path: Path | None = None
        self._controller: QemuProcessController | None = None
        self._qmp: QmpClient | None = None
        self._serial: SerialConnection | None = None
        self._host_services: CodexOSHostServices | None = None
        self._tool_client: ToolClient | None = None

    @property
    def state(self) -> RuntimeState:
        return self._state

    @property
    def generation_number(self) -> int | None:
        return self._generation_number

    @property
    def active_pid(self) -> int | None:
        if self._controller is None:
            return None
        return self._controller.pid

    @property
    def previous_handoff(self) -> str | None:
        return self._previous_handoff

    @property
    def pending_generation_finish(self) -> PendingGenerationFinish | None:
        return self._pending_finish

    def start(self, initial_iso: str | Path) -> None:
        if self._state is not RuntimeState.STOPPED:
            raise RuntimeError("CodexOS run is not stopped")
        if self._generation_number is not None:
            raise RuntimeError("CodexOS run has already been started")

        image = Path(initial_iso).resolve()
        if not image.is_file():
            raise FileNotFoundError(image)
        self._boot_generation(0, image, None, "initial")
        self._generation_number = 0
        self._state = RuntimeState.RUNNING

    def list_tools(self) -> list[str]:
        client = self._require_running_client()
        result = client.list_tools()
        self._finish_if_requested()
        return result

    def invoke_tool(
        self,
        name: str,
        arguments: Sequence[bytes],
    ) -> ToolResult:
        client = self._require_running_client()
        result = client.invoke_tool(name, arguments)
        self._finish_if_requested()
        return result

    def pause(self) -> None:
        if self._state is not RuntimeState.RUNNING:
            raise RuntimeError("CodexOS generation is not running")
        if self._qmp is None:
            raise RuntimeError("CodexOS QMP connection is unavailable")

        self._qmp.stop()
        self._state = RuntimeState.PAUSED
        status = self._qmp.query_status()
        if status != "paused":
            raise QmpError(f"QEMU did not pause; status is {status!r}")

    def resume(self) -> None:
        if self._state is not RuntimeState.PAUSED:
            raise RuntimeError("CodexOS generation is not paused")
        if self._qmp is None:
            raise RuntimeError("CodexOS QMP connection is unavailable")

        self._qmp.cont()
        status = self._qmp.query_status()
        if status != "running":
            raise QmpError(f"QEMU did not resume; status is {status!r}")
        self._state = RuntimeState.RUNNING

    def continue_generation(self) -> None:
        if self._state is not RuntimeState.AWAITING_NEXT_GENERATION:
            raise RuntimeError("CodexOS run is not awaiting a generation")
        pending = self._pending_finish
        if pending is None or self._generation_number is None:
            raise RuntimeError("CodexOS run has no selected successor")

        next_generation = self._generation_number + 1
        self._boot_generation(
            next_generation,
            pending.iso,
            self._generation_number,
            "successor",
        )
        self._generation_number = next_generation
        self._pending_finish = None
        self._state = RuntimeState.RUNNING

    def fork_from_generation(self, generation_number: int) -> None:
        if self._state is not RuntimeState.AWAITING_NEXT_GENERATION:
            raise RuntimeError("CodexOS run is not awaiting a generation")
        if self._generation_number is None:
            raise RuntimeError("CodexOS generation number is unavailable")
        if self.active_pid is not None:
            raise RuntimeError("CodexOS QEMU process is still running")
        if type(generation_number) is not int:
            raise TypeError("generation number must be an integer")
        if generation_number < 0:
            raise ValueError("generation number must not be negative")
        if generation_number > self._generation_number:
            raise ValueError("fork parent must be an earlier generation")

        image, handoff = self._load_fork_generation(generation_number)
        if generation_number == self._generation_number:
            raise ValueError("fork parent must be an earlier generation")
        next_generation = self._generation_number + 1
        self._boot_generation(
            next_generation,
            image,
            generation_number,
            "rollback",
        )
        self._generation_number = next_generation
        self._previous_handoff = handoff
        self._pending_finish = None
        self._state = RuntimeState.RUNNING

    def abort_generation(self) -> None:
        if self._state not in {RuntimeState.RUNNING, RuntimeState.PAUSED}:
            raise RuntimeError("CodexOS generation cannot be aborted")
        if self._generation_number is None:
            raise RuntimeError("CodexOS generation number is unavailable")

        archive_staging: Path | None = None
        try:
            archive_staging, archive_final = self._prepare_aborted_archive()
            self._shutdown_qemu()
            if self._stdout_path is None or self._stderr_path is None:
                raise RuntimeError("QEMU log paths are unavailable")
            shutil.copyfile(
                self._stdout_path,
                archive_staging / "qemu.stdout",
            )
            shutil.copyfile(
                self._stderr_path,
                archive_staging / "qemu.stderr",
            )
            archive_staging.rename(archive_final)
            self._pending_finish = None
            self._previous_handoff = None
            self._state = RuntimeState.AWAITING_NEXT_GENERATION
        except BaseException:
            self._shutdown_qemu()
            self._pending_finish = None
            self._previous_handoff = None
            self._state = RuntimeState.STOPPED
            raise
        finally:
            self._cleanup_workspace()
            if archive_staging is not None and archive_staging.exists():
                shutil.rmtree(archive_staging)

    def stop(self) -> None:
        if self._state is RuntimeState.STOPPED:
            return
        if self._state in {RuntimeState.RUNNING, RuntimeState.PAUSED}:
            self._shutdown_qemu()
            self._cleanup_workspace()
        self._pending_finish = None
        self._previous_handoff = None
        self._current_boot_image = None
        self._current_parent_generation = None
        self._current_transition = None
        self._state = RuntimeState.STOPPED

    def _require_running_client(self) -> ToolClient:
        if self._state is not RuntimeState.RUNNING or self._tool_client is None:
            raise RuntimeError("CodexOS generation is not running")
        return self._tool_client

    def _load_fork_generation(self, generation_number: int) -> tuple[Path, str]:
        archive = self._run_directory / f"generation-{generation_number:04d}"
        if archive.is_symlink() or not archive.is_dir():
            raise FileNotFoundError(f"generation archive is missing: {archive}")

        metadata_path = archive / "metadata.json"
        if metadata_path.is_symlink() or not metadata_path.is_file():
            raise FileNotFoundError(
                f"generation archive artifact is missing: {metadata_path}"
            )
        try:
            metadata = json.loads(metadata_path.read_bytes().decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise ValueError(
                "generation archive metadata is malformed"
            ) from error
        outcome = _validate_generation_metadata(metadata, generation_number)
        if outcome != "completed":
            raise ValueError("aborted generation cannot be a rollback parent")

        handoff_path = archive / "handoff.txt"
        successor = archive / "successor"
        if successor.is_symlink() or not successor.is_dir():
            raise FileNotFoundError(
                f"generation archive artifact is missing: {successor}"
            )
        image = successor / "codexos.iso"
        for required in (handoff_path, image):
            if required.is_symlink() or not required.is_file():
                raise FileNotFoundError(
                    f"generation archive artifact is missing: {required}"
                )

        try:
            handoff = handoff_path.read_bytes().decode("utf-8")
        except UnicodeDecodeError as error:
            raise ValueError("generation handoff is not valid UTF-8") from error
        return image, handoff

    def _boot_generation(
        self,
        generation_number: int,
        image: Path,
        parent_generation: int | None,
        transition: str,
    ) -> None:
        workspace = tempfile.TemporaryDirectory(
            prefix=f".generation-{generation_number:04d}-",
            dir=self._run_directory,
        )
        workspace_path = Path(workspace.name)
        boot_image = workspace_path / "boot.iso"
        stdout_path = workspace_path / "qemu.stdout"
        stderr_path = workspace_path / "qemu.stderr"
        qmp_path = workspace_path / "qmp.sock"
        serial_path = workspace_path / "serial.sock"
        controller = QemuProcessController(self._qemu_executable)
        qmp = QmpClient(qmp_path)
        serial = SerialConnection(serial_path)
        host_services = CodexOSHostServices(workspace_path / "builds")

        self._workspace = workspace
        self._stdout_path = stdout_path
        self._stderr_path = stderr_path
        self._controller = controller
        self._qmp = qmp
        self._serial = serial
        self._host_services = host_services
        self._tool_client = None

        try:
            shutil.copyfile(image, boot_image)
            controller.start(
                _qemu_arguments(boot_image),
                stdout_path=stdout_path,
                stderr_path=stderr_path,
                qmp_socket_path=qmp_path,
                serial_socket_path=serial_path,
            )
            qmp.connect()
            serial.connect()
            _wait_for_ready(serial)
            self._tool_client = ToolClient(serial, host_services)
            self._current_boot_image = boot_image
            self._current_parent_generation = parent_generation
            self._current_transition = transition
        except BaseException:
            self._shutdown_qemu()
            self._cleanup_workspace()
            raise

    def _finish_if_requested(self) -> None:
        if self._host_services is None:
            return
        pending = self._host_services.pending_generation_finish
        if pending is not None:
            self._complete_generation(pending)

    def _complete_generation(self, pending: PendingGenerationFinish) -> None:
        if self._generation_number is None:
            raise RuntimeError("CodexOS generation number is unavailable")

        archive_staging: Path | None = None
        archive_final: Path | None = None
        try:
            archive_staging, archive_final = self._prepare_archive(pending)
            self._shutdown_qemu()
            if self._stdout_path is None or self._stderr_path is None:
                raise RuntimeError("QEMU log paths are unavailable")
            shutil.copyfile(
                self._stdout_path,
                archive_staging / "qemu.stdout",
            )
            shutil.copyfile(
                self._stderr_path,
                archive_staging / "qemu.stderr",
            )
            archive_staging.rename(archive_final)
            self._pending_finish = PendingGenerationFinish(
                pending.handoff_message,
                pending.source_snapshot,
                archive_final / "successor" / "kernel.elf",
                archive_final / "successor" / "codexos.iso",
            )
            self._previous_handoff = pending.handoff_message
            self._current_boot_image = None
            self._state = RuntimeState.AWAITING_NEXT_GENERATION
        except BaseException:
            self._shutdown_qemu()
            self._pending_finish = None
            self._state = RuntimeState.STOPPED
            raise
        finally:
            self._cleanup_workspace()
            if archive_staging is not None and archive_staging.exists():
                shutil.rmtree(archive_staging)

    def _prepare_archive(
        self,
        pending: PendingGenerationFinish,
    ) -> tuple[Path, Path]:
        if self._generation_number is None:
            raise RuntimeError("CodexOS generation number is unavailable")
        if self._current_boot_image is None:
            raise RuntimeError("CodexOS boot image is unavailable")
        if self._current_transition is None:
            raise RuntimeError("CodexOS generation transition is unavailable")
        archive_final = self._run_directory / (
            f"generation-{self._generation_number:04d}"
        )
        if archive_final.exists():
            raise FileExistsError(archive_final)
        archive_staging = Path(
            tempfile.mkdtemp(
                prefix=f".generation-{self._generation_number:04d}-archive-",
                dir=self._run_directory,
            )
        )

        try:
            boot = archive_staging / "boot"
            boot.mkdir()
            shutil.copyfile(self._current_boot_image, boot / "codexos.iso")
            metadata = {
                "generation": self._generation_number,
                "outcome": "completed",
                "parent_generation": self._current_parent_generation,
                "transition": self._current_transition,
            }
            (archive_staging / "metadata.json").write_bytes(
                (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode(
                    "utf-8"
                )
            )
            (archive_staging / "handoff.txt").write_bytes(
                pending.handoff_message.encode("utf-8")
            )
            (archive_staging / "source.snapshot").write_bytes(
                pending.source_snapshot
            )
            _materialize_snapshot(
                pending.source_snapshot,
                archive_staging / "source",
            )
            successor = archive_staging / "successor"
            successor.mkdir()
            shutil.copyfile(pending.kernel_elf, successor / "kernel.elf")
            shutil.copyfile(pending.iso, successor / "codexos.iso")
        except BaseException:
            shutil.rmtree(archive_staging)
            raise
        return archive_staging, archive_final

    def _prepare_aborted_archive(self) -> tuple[Path, Path]:
        if self._generation_number is None:
            raise RuntimeError("CodexOS generation number is unavailable")
        if self._current_boot_image is None:
            raise RuntimeError("CodexOS boot image is unavailable")
        if self._current_transition is None:
            raise RuntimeError("CodexOS generation transition is unavailable")
        archive_final = self._run_directory / (
            f"generation-{self._generation_number:04d}"
        )
        if archive_final.exists():
            raise FileExistsError(archive_final)
        archive_staging = Path(
            tempfile.mkdtemp(
                prefix=f".generation-{self._generation_number:04d}-archive-",
                dir=self._run_directory,
            )
        )

        try:
            boot = archive_staging / "boot"
            boot.mkdir()
            shutil.copyfile(self._current_boot_image, boot / "codexos.iso")
            metadata = {
                "generation": self._generation_number,
                "outcome": "aborted",
                "parent_generation": self._current_parent_generation,
                "transition": self._current_transition,
            }
            (archive_staging / "metadata.json").write_bytes(
                (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode(
                    "utf-8"
                )
            )
            (archive_staging / "aborted.txt").write_bytes(_ABORT_MARKER)
        except BaseException:
            shutil.rmtree(archive_staging)
            raise
        return archive_staging, archive_final

    def _shutdown_qemu(self) -> None:
        controller = self._controller
        qmp = self._qmp
        serial = self._serial

        try:
            if qmp is not None and controller is not None and controller.is_running:
                try:
                    qmp.quit()
                except (OSError, QmpError):
                    pass
                deadline = time.monotonic() + _QEMU_EXIT_TIMEOUT_SECONDS
                while controller.is_running and time.monotonic() < deadline:
                    time.sleep(0.01)
            if controller is not None and controller.is_running:
                controller.stop(timeout_seconds=_QEMU_EXIT_TIMEOUT_SECONDS)
        finally:
            if serial is not None:
                serial.close()
            if qmp is not None:
                qmp.close()
            if controller is not None:
                controller.stop(timeout_seconds=_QEMU_EXIT_TIMEOUT_SECONDS)
            self._tool_client = None
            self._host_services = None
            self._serial = None
            self._qmp = None
            self._controller = None

    def _cleanup_workspace(self) -> None:
        workspace = self._workspace
        self._workspace = None
        if workspace is not None:
            workspace.cleanup()
        self._stdout_path = None
        self._stderr_path = None
        self._current_boot_image = None
        self._current_parent_generation = None
        self._current_transition = None


def _materialize_snapshot(snapshot: bytes, destination: Path) -> None:
    source_root = destination.resolve()
    source_root.mkdir(parents=True)
    for entry in decode_source_snapshot(snapshot):
        output = (source_root / entry.path).resolve(strict=False)
        if not output.is_relative_to(source_root):
            raise ValueError(f"source path escapes archive: {entry.path!r}")
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(entry.content)


def _validate_generation_metadata(
    metadata: object,
    expected_generation: int,
) -> str:
    if not isinstance(metadata, dict) or set(metadata) != {
        "generation",
        "outcome",
        "parent_generation",
        "transition",
    }:
        raise ValueError("generation archive metadata is malformed")

    generation = metadata["generation"]
    outcome = metadata["outcome"]
    parent = metadata["parent_generation"]
    transition = metadata["transition"]
    if type(generation) is not int or generation != expected_generation:
        raise ValueError("generation archive metadata has the wrong generation")
    if outcome not in {"completed", "aborted"}:
        raise ValueError("generation archive metadata is malformed")
    if generation == 0:
        if parent is not None or transition != "initial":
            raise ValueError("generation archive metadata is malformed")
        return outcome
    if type(parent) is not int or parent < 0 or parent >= generation:
        raise ValueError("generation archive metadata is malformed")
    if transition not in {"successor", "rollback"}:
        raise ValueError("generation archive metadata is malformed")
    return outcome


def _wait_for_ready(serial: SerialConnection) -> None:
    received = bytearray()
    deadline = time.monotonic() + _STARTUP_TIMEOUT_SECONDS
    while _READY_MARKER not in received:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("timed out waiting for CODEXOS-SEED-READY")
        try:
            received.extend(serial.read(4096, min(0.5, remaining)))
        except TimeoutError:
            continue


def _qemu_arguments(image: Path) -> list[str]:
    return [
        "-machine",
        "q35,accel=kvm:tcg",
        "-m",
        "128M",
        "-cdrom",
        str(image),
        "-boot",
        "order=d",
        "-display",
        "none",
        "-monitor",
        "none",
        "-nic",
        "none",
        "-no-reboot",
    ]

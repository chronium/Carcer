"""Concrete CodexOS runtime across manually gated generations."""

from __future__ import annotations

import hashlib
import json
import shutil
import tempfile
import time
from collections.abc import Sequence
from dataclasses import dataclass
from enum import Enum
from pathlib import Path

from .candidate_boot import CandidateBootValidator
from .codex_activity import CodexActivityStream
from .feature_requests import FeatureRequest, FeatureRequestStore
from .generation_finish_host_service import (
    CodexOSHostServices,
    PendingGenerationFinish,
)
from .hardware import (
    EXPERIMENT_HARDWARE_PROFILE,
    CodexOSHardwareProfile,
    HardwareManifest,
    discover_qemu_version,
    validate_hardware_manifest,
)
from .guest_startup import wait_for_ready
from .observability import ExperimentObservability
from .provided_assets import ProvidedAssets, configure_provided_assets
from .qemu import QemuProcessController
from .qmp import QmpClient, QmpError
from .serial import SerialConnection
from .serial_protocol import SerialProtocolDispatcher
from .source_snapshot import decode_source_snapshot
from .tool_protocol import ToolClient, ToolResult

_ABORT_MARKER = b"Generation aborted by operator."
_STARTUP_TIMEOUT_SECONDS = 10.0
_QEMU_EXIT_TIMEOUT_SECONDS = 2.0


class RuntimeState(Enum):
    STOPPED = "stopped"
    RUNNING = "running"
    PAUSED = "paused"
    AWAITING_NEXT_GENERATION = "awaiting_next_generation"


@dataclass(frozen=True)
class ArchivedGeneration:
    generation: int
    parent_generation: int | None
    transition: str
    outcome: str
    archive_path: Path
    handoff: str | None
    hardware: HardwareManifest


class CodexOSRun:
    """Run one concrete CodexOS lineage with explicit generation gates."""

    def __init__(
        self,
        run_directory: str | Path,
        qemu_executable: str = "qemu-system-x86_64",
        *,
        hardware_profile: CodexOSHardwareProfile = EXPERIMENT_HARDWARE_PROFILE,
        observability: ExperimentObservability | None = None,
        activity_stream: CodexActivityStream | None = None,
        provided_assets_directory: str | Path | None = None,
    ) -> None:
        self._run_directory = Path(run_directory).resolve()
        self._run_directory.mkdir(parents=True, exist_ok=True)
        self._feature_request_store = FeatureRequestStore(self._run_directory)
        self._observability = observability
        self._activity_stream = activity_stream
        self._provided_assets_directory = provided_assets_directory
        self._provided_assets: ProvidedAssets | None = None
        self._provided_assets_configured = False
        if observability is not None:
            observability.set_feature_requests_pending(
                sum(
                    request.status == "pending"
                    for request in self._feature_request_store.requests()
                )
            )
        self._qemu_executable = qemu_executable
        self._hardware_profile = hardware_profile
        self._state = RuntimeState.STOPPED
        self._generation_number: int | None = None
        self._current_boot_image: Path | None = None
        self._current_parent_generation: int | None = None
        self._current_transition: str | None = None
        self._current_hardware: HardwareManifest | None = None
        self._previous_handoff: str | None = None
        self._pending_finish: PendingGenerationFinish | None = None
        self._generation_started_at: float | None = None
        self._run_started = False

        self._workspace: tempfile.TemporaryDirectory[str] | None = None
        self._stdout_path: Path | None = None
        self._stderr_path: Path | None = None
        self._controller: QemuProcessController | None = None
        self._qmp: QmpClient | None = None
        self._serial: SerialConnection | None = None
        self._serial_protocol: SerialProtocolDispatcher | None = None
        self._host_services: CodexOSHostServices | None = None
        self._tool_client: ToolClient | None = None

    @property
    def state(self) -> RuntimeState:
        return self._state

    @property
    def run_directory(self) -> Path:
        return self._run_directory

    @property
    def observability(self) -> ExperimentObservability | None:
        return self._observability

    @property
    def activity_stream(self) -> CodexActivityStream | None:
        return self._activity_stream

    @property
    def hardware_profile(self) -> CodexOSHardwareProfile:
        return self._hardware_profile

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
    def current_transition(self) -> str | None:
        """How the currently running generation entered the lineage."""
        return self._current_transition

    @property
    def pending_generation_finish(self) -> PendingGenerationFinish | None:
        return self._pending_finish

    def feature_requests(self) -> tuple[FeatureRequest, ...]:
        return self._feature_request_store.requests()

    def feature_request(self, request_id: int) -> FeatureRequest:
        return self._feature_request_store.request(request_id)

    def approve_feature_request(self, request_id: int) -> FeatureRequest:
        self._require_feature_decision_gate()
        request = self._feature_request_store.approve(request_id)
        self._record_feature_decision("feature_approved", request)
        return request

    def deny_feature_request(self, request_id: int) -> FeatureRequest:
        self._require_feature_decision_gate()
        request = self._feature_request_store.deny(request_id)
        self._record_feature_decision("feature_denied", request)
        return request

    def archived_generations(self) -> list[ArchivedGeneration]:
        generations: list[ArchivedGeneration] = []
        for path in self._run_directory.iterdir():
            if not path.name.startswith("generation-"):
                continue
            suffix = path.name.removeprefix("generation-")
            if (
                not suffix.isascii()
                or not suffix.isdecimal()
                or path.name != f"generation-{int(suffix):04d}"
            ):
                raise ValueError(f"invalid generation archive: {path.name}")
            generations.append(self.inspect_generation(int(suffix)))
        generations.sort(key=lambda item: item.generation)
        return generations

    def inspect_generation(self, generation_number: int) -> ArchivedGeneration:
        if type(generation_number) is not int or generation_number < 0:
            raise ValueError("generation number must be a non-negative integer")
        try:
            return self._read_archived_generation(generation_number)
        except (OSError, ValueError) as error:
            raise ValueError(
                f"generation {generation_number} archive is invalid: {error}"
            ) from error

    def start(self, initial_iso: str | Path) -> None:
        if self._state is not RuntimeState.STOPPED:
            raise RuntimeError("CodexOS run is not stopped")
        if self._generation_number is not None:
            raise RuntimeError("CodexOS run has already been started")

        image = Path(initial_iso).resolve()
        if not image.is_file():
            raise FileNotFoundError(image)
        self._configure_provided_assets()
        self._boot_generation(0, image, None, "initial")
        self._generation_number = 0
        self._state = RuntimeState.RUNNING
        self._generation_started_at = time.monotonic()
        self._run_started = True
        self._record("run_started", None, {})
        self._record_generation_started()

    def reopen_at_gate(self) -> None:
        """Restore one validated archived generation gate without booting."""
        if self._state is not RuntimeState.STOPPED:
            raise RuntimeError("CodexOS run is not stopped")
        if self._generation_number is not None:
            raise RuntimeError("CodexOS run has already been opened")
        partial = sorted(
            path.name
            for path in self._run_directory.iterdir()
            if path.name.startswith(".generation-")
        )
        if partial:
            raise RuntimeError(
                "run contains partial generation state: " + ", ".join(partial)
            )

        archives = self.archived_generations()
        if not archives:
            raise RuntimeError("run has no archived generation gate")
        self._validate_archived_history(archives)
        self._configure_provided_assets()
        latest = archives[-1]
        pending: PendingGenerationFinish | None = None
        previous_handoff: str | None = None
        if latest.outcome == "completed":
            if latest.handoff is None:
                raise ValueError("completed generation handoff is unavailable")
            snapshot = (latest.archive_path / "source.snapshot").read_bytes()
            pending = PendingGenerationFinish(
                latest.handoff,
                snapshot,
                latest.archive_path / "successor" / "kernel.elf",
                latest.archive_path / "successor" / "codexos.iso",
            )
            previous_handoff = latest.handoff

        self._generation_number = latest.generation
        self._pending_finish = pending
        self._previous_handoff = previous_handoff
        self._state = RuntimeState.AWAITING_NEXT_GENERATION
        self._run_started = True
        self._record(
            "run_reopened_at_gate",
            latest.generation,
            {
                "latest_outcome": latest.outcome,
                "successor_selected": pending is not None,
            },
        )

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
        self._set_observed_state()
        status = self._qmp.query_status()
        if status != "paused":
            raise QmpError(f"QEMU did not pause; status is {status!r}")
        self._record("generation_paused", self._generation_number, {})

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
        self._record("generation_resumed", self._generation_number, {})

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
        self._generation_started_at = time.monotonic()
        self._record_generation_started()

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
        self._generation_started_at = time.monotonic()
        self._record(
            "generation_rollback_started",
            next_generation,
            {"parent_generation": generation_number},
        )
        self._record_generation_started()

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
            self._record_generation_outcome("aborted")
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
        if self._run_started:
            self._record("run_stopped", None, {})
            self._run_started = False

    def _require_running_client(self) -> ToolClient:
        if self._state is not RuntimeState.RUNNING or self._tool_client is None:
            raise RuntimeError("CodexOS generation is not running")
        return self._tool_client

    def _require_feature_decision_gate(self) -> None:
        if self._state is not RuntimeState.AWAITING_NEXT_GENERATION:
            raise RuntimeError(
                "feature requests may be decided only while awaiting a generation"
            )

    @staticmethod
    def _validate_archived_history(
        archives: Sequence[ArchivedGeneration],
    ) -> None:
        by_generation = {archive.generation: archive for archive in archives}
        if len(by_generation) != len(archives) or sorted(by_generation) != list(
            range(len(archives))
        ):
            raise ValueError("generation archive history is not contiguous")
        for archive in archives[1:]:
            parent_number = archive.parent_generation
            parent = by_generation.get(parent_number)
            if parent is None or parent.outcome != "completed":
                raise ValueError(
                    f"generation {archive.generation} has no completed parent"
                )
            if (
                archive.transition == "successor"
                and parent_number != archive.generation - 1
            ):
                raise ValueError(
                    f"generation {archive.generation} has invalid successor ancestry"
                )
            if (
                archive.transition == "rollback"
                and parent_number == archive.generation - 1
            ):
                raise ValueError(
                    f"generation {archive.generation} has invalid rollback ancestry"
                )

    def _load_fork_generation(self, generation_number: int) -> tuple[Path, str]:
        archived = self._read_archived_generation(generation_number)
        if archived.outcome != "completed":
            raise ValueError("aborted generation cannot be a rollback parent")
        if archived.handoff is None:
            raise ValueError("completed generation handoff is unavailable")
        return (
            archived.archive_path / "successor" / "codexos.iso",
            archived.handoff,
        )

    def _read_archived_generation(
        self,
        generation_number: int,
    ) -> ArchivedGeneration:
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

        hardware_path = archive / "hardware.json"
        if hardware_path.is_symlink() or not hardware_path.is_file():
            raise FileNotFoundError(
                f"generation archive artifact is missing: {hardware_path}"
            )
        try:
            hardware_value = json.loads(
                hardware_path.read_bytes().decode("utf-8")
            )
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise ValueError(
                "generation hardware manifest is malformed"
            ) from error
        hardware = validate_hardware_manifest(hardware_value)

        boot = archive / "boot"
        common_files = (
            boot / "codexos.iso",
            archive / "qemu.stdout",
            archive / "qemu.stderr",
        )
        if boot.is_symlink() or not boot.is_dir():
            raise FileNotFoundError(
                f"generation archive artifact is missing: {boot}"
            )
        for required in common_files:
            if required.is_symlink() or not required.is_file():
                raise FileNotFoundError(
                    f"generation archive artifact is missing: {required}"
                )

        handoff: str | None = None
        if outcome == "completed":
            handoff_path = archive / "handoff.txt"
            source_snapshot = archive / "source.snapshot"
            source = archive / "source"
            successor = archive / "successor"
            for directory in (source, successor):
                if directory.is_symlink() or not directory.is_dir():
                    raise FileNotFoundError(
                        f"generation archive artifact is missing: {directory}"
                    )
            for required in (
                handoff_path,
                source_snapshot,
                successor / "kernel.elf",
                successor / "codexos.iso",
            ):
                if required.is_symlink() or not required.is_file():
                    raise FileNotFoundError(
                        f"generation archive artifact is missing: {required}"
                    )
            try:
                handoff = handoff_path.read_bytes().decode("utf-8")
            except UnicodeDecodeError as error:
                raise ValueError(
                    "generation handoff is not valid UTF-8"
                ) from error
            decode_source_snapshot(source_snapshot.read_bytes())
            expected_names = {
                "boot",
                "metadata.json",
                "hardware.json",
                "handoff.txt",
                "source.snapshot",
                "source",
                "successor",
                "qemu.stdout",
                "qemu.stderr",
            }
        else:
            aborted = archive / "aborted.txt"
            if aborted.is_symlink() or not aborted.is_file():
                raise FileNotFoundError(
                    f"generation archive artifact is missing: {aborted}"
                )
            if aborted.read_bytes() != _ABORT_MARKER:
                raise ValueError("generation abort marker is malformed")
            expected_names = {
                "boot",
                "metadata.json",
                "hardware.json",
                "aborted.txt",
                "qemu.stdout",
                "qemu.stderr",
            }

        if {path.name for path in archive.iterdir()} != expected_names:
            raise ValueError(
                f"generation {generation_number} archive has invalid contents"
            )
        return ArchivedGeneration(
            generation=generation_number,
            parent_generation=metadata["parent_generation"],
            transition=metadata["transition"],
            outcome=outcome,
            archive_path=archive,
            handoff=handoff,
            hardware=hardware,
        )

    def _boot_generation(
        self,
        generation_number: int,
        image: Path,
        parent_generation: int | None,
        transition: str,
    ) -> None:
        self._hardware_profile.require_available()
        hardware = self._hardware_profile.manifest(
            discover_qemu_version(self._qemu_executable)
        )
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
        host_services = CodexOSHostServices(
            workspace_path / "builds",
            CandidateBootValidator(
                self._qemu_executable,
                self._hardware_profile,
                activity_stream=self._activity_stream,
                generation=generation_number,
                provided_assets=self._provided_assets,
            ),
            feature_request_store=self._feature_request_store,
            generation=generation_number,
            observability=self._observability,
            activity_stream=self._activity_stream,
            provided_assets=self._provided_assets,
        )

        self._workspace = workspace
        self._stdout_path = stdout_path
        self._stderr_path = stderr_path
        self._controller = controller
        self._qmp = qmp
        self._serial = serial
        self._serial_protocol = None
        self._host_services = host_services
        self._tool_client = None
        self._current_hardware = hardware

        try:
            shutil.copyfile(image, boot_image)
            controller.start(
                self._hardware_profile.qemu_arguments(
                    boot_image,
                    qmp_path,
                    serial_path,
                ),
                stdout_path=stdout_path,
                stderr_path=stderr_path,
            )
            qmp.connect()
            serial.connect()
            protocol = SerialProtocolDispatcher(
                serial,
                startup_host_services=self._provided_assets,
                background_host_services=self._provided_assets,
                exchange_host_services=host_services,
            )
            self._serial_protocol = protocol
            wait_for_ready(
                protocol,
                _STARTUP_TIMEOUT_SECONDS,
            )
            self._tool_client = ToolClient(protocol)
            self._current_boot_image = boot_image
            self._current_parent_generation = parent_generation
            self._current_transition = transition
        except BaseException:
            self._shutdown_qemu()
            self._cleanup_workspace()
            raise

    def _configure_provided_assets(self) -> None:
        if self._provided_assets_configured:
            return
        self._provided_assets = configure_provided_assets(
            self._run_directory,
            self._provided_assets_directory,
        )
        self._provided_assets_configured = True

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
            self._record_generation_outcome(
                "completed",
                source_snapshot=pending.source_snapshot,
            )
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
        if self._current_hardware is None:
            raise RuntimeError("CodexOS hardware manifest is unavailable")
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
            _write_hardware_manifest(
                archive_staging / "hardware.json",
                self._current_hardware,
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
        if self._current_hardware is None:
            raise RuntimeError("CodexOS hardware manifest is unavailable")
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
            _write_hardware_manifest(
                archive_staging / "hardware.json",
                self._current_hardware,
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
        protocol = self._serial_protocol

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
            try:
                if protocol is not None:
                    protocol.close()
                elif serial is not None:
                    serial.close()
            finally:
                try:
                    if qmp is not None:
                        qmp.close()
                finally:
                    try:
                        if controller is not None:
                            controller.stop(
                                timeout_seconds=_QEMU_EXIT_TIMEOUT_SECONDS
                            )
                    finally:
                        self._tool_client = None
                        self._host_services = None
                        self._serial = None
                        self._serial_protocol = None
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
        self._current_hardware = None

    def _record_generation_started(self) -> None:
        self._record(
            "generation_started",
            self._generation_number,
            {
                "transition": self._current_transition,
                "parent_generation": self._current_parent_generation,
                "qemu_pid": self.active_pid,
                "hardware_profile": self._hardware_profile.profile,
                "vcpus": self._hardware_profile.vcpus,
                "memory_mib": self._hardware_profile.memory_mib,
            },
        )

    def _record_generation_outcome(
        self,
        outcome: str,
        *,
        source_snapshot: bytes | None = None,
    ) -> None:
        started = self._generation_started_at
        data: dict[str, object] = {
            "transition": self._current_transition,
            "parent_generation": self._current_parent_generation,
            "duration_seconds": (
                max(0.0, time.monotonic() - started) if started is not None else 0.0
            ),
        }
        if source_snapshot is not None:
            data["source_snapshot_sha256"] = hashlib.sha256(
                source_snapshot
            ).hexdigest()
        self._record(
            f"generation_{outcome}",
            self._generation_number,
            data,
        )
        self._generation_started_at = None

    def _record_feature_decision(
        self,
        event: str,
        request: FeatureRequest,
    ) -> None:
        self._record(
            event,
            self._generation_number,
            {
                "request_id": request.id,
                "request_generation": request.generation,
            },
        )
        if self._observability is not None:
            self._observability.set_feature_requests_pending(
                sum(
                    item.status == "pending"
                    for item in self._feature_request_store.requests()
                )
            )

    def _record(
        self,
        event: str,
        generation: int | None,
        data: dict[str, object],
    ) -> None:
        if self._observability is not None:
            self._observability.record(event, generation, data)
            self._set_observed_state()

    def _set_observed_state(self) -> None:
        if self._observability is not None:
            self._observability.set_runtime_state(
                self._generation_number,
                self._state.value,
            )


def _materialize_snapshot(snapshot: bytes, destination: Path) -> None:
    source_root = destination.resolve()
    source_root.mkdir(parents=True)
    for entry in decode_source_snapshot(snapshot):
        output = (source_root / entry.path).resolve(strict=False)
        if not output.is_relative_to(source_root):
            raise ValueError(f"source path escapes archive: {entry.path!r}")
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_bytes(entry.content)


def _write_hardware_manifest(
    path: Path,
    manifest: HardwareManifest,
) -> None:
    path.write_bytes(
        (
            json.dumps(manifest.as_json_object(), indent=2, sort_keys=True)
            + "\n"
        ).encode("utf-8")
    )


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

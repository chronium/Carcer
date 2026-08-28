"""Lifecycle control for one QEMU process."""

from __future__ import annotations

import subprocess
from collections.abc import Sequence
from os import PathLike
from pathlib import Path
from typing import BinaryIO


class QemuProcessController:
    """Start and stop one QEMU process at a time."""

    def __init__(self, executable: str = "qemu-system-x86_64") -> None:
        self._executable = executable
        self._process: subprocess.Popen[bytes] | None = None
        self._stdout: BinaryIO | None = None
        self._stderr: BinaryIO | None = None

    @property
    def is_running(self) -> bool:
        self._reap_exited_process()
        return self._process is not None

    @property
    def pid(self) -> int | None:
        self._reap_exited_process()
        return self._process.pid if self._process is not None else None

    def start(
        self,
        arguments: Sequence[str],
        *,
        stdout_path: str | PathLike[str],
        stderr_path: str | PathLike[str],
    ) -> None:
        """Start QEMU with output captured in the given files."""
        self._reap_exited_process()
        if self._process is not None:
            raise RuntimeError("QEMU is already running")

        self._stdout = Path(stdout_path).open("wb")
        try:
            self._stderr = Path(stderr_path).open("wb")
            self._process = subprocess.Popen(
                [self._executable, *arguments],
                stdout=self._stdout,
                stderr=self._stderr,
            )
        except BaseException:
            self._close_logs()
            raise

    def stop(self, timeout_seconds: float = 5.0) -> None:
        """Terminate QEMU, killing it if it does not exit before the timeout."""
        self._reap_exited_process()
        process = self._process
        if process is None:
            return

        try:
            process.terminate()
            try:
                process.wait(timeout=timeout_seconds)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=timeout_seconds)
        finally:
            if process.poll() is not None:
                self._process = None
            self._close_logs()

    def close(self) -> None:
        self.stop()

    def __enter__(self) -> QemuProcessController:
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def _reap_exited_process(self) -> None:
        if self._process is not None and self._process.poll() is not None:
            self._process = None
            self._close_logs()

    def _close_logs(self) -> None:
        if self._stdout is not None:
            self._stdout.close()
            self._stdout = None
        if self._stderr is not None:
            self._stderr.close()
            self._stderr = None

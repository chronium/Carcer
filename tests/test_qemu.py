import os
import shutil
import tempfile
import unittest
from pathlib import Path

from harness import QemuProcessController


class QemuProcessControllerIntegrationTest(unittest.TestCase):
    def test_qemu_lifecycle(self) -> None:
        executable = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(executable, "qemu-system-x86_64 must be installed")

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            stdout_path = temporary_path / "qemu.stdout"
            stderr_path = temporary_path / "qemu.stderr"
            arguments = [
                "-S",
                "-display",
                "none",
                "-monitor",
                "none",
                "-serial",
                "none",
                "-nodefaults",
                "-machine",
                "none",
            ]

            with QemuProcessController(executable) as controller:
                controller.start(
                    arguments,
                    stdout_path=stdout_path,
                    stderr_path=stderr_path,
                )

                self.assertTrue(controller.is_running)
                pid = controller.pid
                self.assertIsNotNone(pid)
                os.kill(pid, 0)

                with self.assertRaisesRegex(RuntimeError, "already running"):
                    controller.start(
                        arguments,
                        stdout_path=stdout_path,
                        stderr_path=stderr_path,
                    )

                controller.stop(timeout_seconds=2.0)

                self.assertFalse(controller.is_running)
                self.assertIsNone(controller.pid)
                with self.assertRaises(ProcessLookupError):
                    os.kill(pid, 0)

            self.assertTrue(stdout_path.is_file())
            self.assertTrue(stderr_path.is_file())


if __name__ == "__main__":
    unittest.main()

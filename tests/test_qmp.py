import shutil
import tempfile
import time
import unittest
from pathlib import Path

from harness import QmpClient, QemuProcessController


class QmpIntegrationTest(unittest.TestCase):
    def test_qmp_lifecycle(self) -> None:
        executable = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(executable, "qemu-system-x86_64 must be installed")

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            qmp_socket_path = temporary_path / "qmp.sock"

            with QemuProcessController(executable) as controller:
                controller.start(
                    [
                        "-display",
                        "none",
                        "-monitor",
                        "none",
                        "-serial",
                        "none",
                        "-nodefaults",
                        "-machine",
                        "none",
                    ],
                    stdout_path=temporary_path / "qemu.stdout",
                    stderr_path=temporary_path / "qemu.stderr",
                    qmp_socket_path=qmp_socket_path,
                )

                with QmpClient(qmp_socket_path) as qmp:
                    self.assertEqual(qmp.query_status(), "running")

                    qmp.stop()
                    self.assertEqual(qmp.query_status(), "paused")

                    qmp.cont()
                    self.assertEqual(qmp.query_status(), "running")

                    qmp.quit()

                deadline = time.monotonic() + 2.0
                while controller.is_running and time.monotonic() < deadline:
                    time.sleep(0.01)
                self.assertFalse(controller.is_running)


if __name__ == "__main__":
    unittest.main()

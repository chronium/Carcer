import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from harness import TEST_HARDWARE_PROFILE, CodexOSRun, RuntimeState

_TOOLS = [
    "list",
    "read",
    "write",
    "truncate",
    "remove",
    "build",
    "finish_generation",
    "request_feature",
]


class GuestFeatureRequestIntegrationTest(unittest.TestCase):
    def test_real_guest_records_requests_without_changing_source(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        original_kernel = (repository / "seed" / "kernel.c").read_bytes()

        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(
                temporary,
                hardware_profile=TEST_HARDWARE_PROFILE,
            )
            try:
                runtime.start(image)
                self.assertEqual(runtime.list_tools(), _TOOLS)
                guest_kernel = runtime.invoke_tool(
                    "read",
                    [b"seed/kernel.c", b"0", str(len(original_kernel)).encode()],
                )
                self.assertEqual(guest_kernel.output, original_kernel)

                invalid = (
                    [b"", b"description"],
                    [b"\xff", b"description"],
                    [b"x" * 257, b"description"],
                    [b"title", b"x" * (16 * 1024 + 1)],
                )
                for arguments in invalid:
                    result = runtime.invoke_tool("request_feature", arguments)
                    self.assertNotEqual(result.status, 0)
                self.assertEqual(runtime.feature_requests(), ())

                first = runtime.invoke_tool(
                    "request_feature",
                    ["Capabilitate λ".encode(), "Descriere Unicode: Δ".encode()],
                )
                second = runtime.invoke_tool(
                    "request_feature",
                    [b"Second request", b""],
                )
                self.assertEqual(first.status, 0)
                self.assertEqual(first.output, b"1")
                self.assertEqual(second.status, 0)
                self.assertEqual(second.output, b"2")
                requests = runtime.feature_requests()
                self.assertEqual(requests[0].generation, 0)
                self.assertEqual(requests[0].title, "Capabilitate λ")
                self.assertEqual(requests[0].description, "Descriere Unicode: Δ")
                self.assertEqual(requests[1].id, 2)
                self.assertIsNotNone(runtime.active_pid)

                unchanged = runtime.invoke_tool(
                    "read",
                    [b"seed/kernel.c", b"0", str(len(original_kernel)).encode()],
                )
                self.assertEqual(unchanged.output, original_kernel)

                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [b"Feature requests survive this boundary."],
                )
                self.assertEqual(finish.status, 0, finish.output)
                self.assertIs(runtime.state, RuntimeState.AWAITING_NEXT_GENERATION)
                self.assertIsNone(runtime.active_pid)
                self.assertEqual(
                    [(item.id, item.generation) for item in runtime.feature_requests()],
                    [(1, 0), (2, 0)],
                )
            finally:
                runtime.stop()

        self.assertEqual(
            (repository / "seed" / "kernel.c").read_bytes(),
            original_kernel,
        )


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

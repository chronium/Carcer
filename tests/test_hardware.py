import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from harness import (
    EXPERIMENT_HARDWARE_PROFILE,
    TEST_HARDWARE_PROFILE,
    CodexOSRun,
    RuntimeState,
)
from harness.hardware import validate_hardware_manifest


class CodexOSHardwareProfileTests(unittest.TestCase):
    def test_runtime_defaults_to_the_experiment_profile(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary)
            self.assertIs(
                runtime.hardware_profile,
                EXPERIMENT_HARDWARE_PROFILE,
            )

    def test_experiment_profile_builds_the_frozen_qemu_command(self) -> None:
        boot = Path("/trusted/boot.iso")
        qmp = Path("/trusted/qmp.sock")
        serial = Path("/trusted/serial.sock")
        arguments = EXPERIMENT_HARDWARE_PROFILE.qemu_arguments(
            boot,
            qmp,
            serial,
        )

        self.assertEqual(
            _option_values(arguments, "-machine"),
            ["q35,accel=kvm,pcspk-audiodev=codexos-noaudio"],
        )
        self.assertEqual(_option_values(arguments, "-cpu"), ["host"])
        self.assertEqual(_option_values(arguments, "-smp"), ["4"])
        self.assertEqual(_option_values(arguments, "-m"), ["8192M"])
        self.assertIn("-nodefaults", arguments)
        self.assertIn("-no-reboot", arguments)
        self.assertEqual(_option_values(arguments, "-display"), ["none"])
        self.assertEqual(_option_values(arguments, "-nic"), ["none"])
        self.assertEqual(
            _option_values(arguments, "-audiodev"),
            ["none,id=codexos-noaudio"],
        )
        self.assertEqual(
            _option_values(arguments, "-device"),
            [
                "ide-cd,drive=codexos-boot-cd,bootindex=1",
                "VGA",
                "isa-serial,chardev=codexos-com1,index=0",
            ],
        )
        block_nodes = [
            json.loads(value)
            for value in _option_values(arguments, "-blockdev")
        ]
        self.assertEqual(
            block_nodes,
            [
                {
                    "driver": "file",
                    "filename": str(boot),
                    "node-name": "codexos-boot-file",
                    "read-only": True,
                },
                {
                    "driver": "raw",
                    "file": "codexos-boot-file",
                    "node-name": "codexos-boot-cd",
                    "read-only": True,
                },
            ],
        )
        self.assertEqual(
            _option_values(arguments, "-chardev"),
            [
                "socket,id=codexos-com1,"
                f"path={serial},server=on,wait=off"
            ],
        )
        self.assertEqual(
            _option_values(arguments, "-qmp"),
            [f"unix:{qmp},server=on,wait=off"],
        )
        self.assertNotIn("-cdrom", arguments)
        self.assertNotIn("-drive", arguments)

    def test_hardware_manifest_accepts_only_the_concrete_v1_shape(self) -> None:
        value = TEST_HARDWARE_PROFILE.manifest(
            "QEMU emulator version test"
        ).as_json_object()
        self.assertEqual(validate_hardware_manifest(value).profile, "test-v1")
        normalized = value["qemu_arguments"]
        self.assertIn("<BOOT_ISO>", " ".join(normalized))
        self.assertIn("<QMP_SOCKET>", " ".join(normalized))
        self.assertIn("<SERIAL_SOCKET>", " ".join(normalized))

        malformed = dict(value)
        malformed["unexpected"] = True
        with self.assertRaisesRegex(ValueError, "malformed"):
            validate_hardware_manifest(malformed)

        invalid = dict(value)
        invalid["network"] = "e1000"
        with self.assertRaisesRegex(ValueError, "peripheral"):
            validate_hardware_manifest(invalid)


@unittest.skipUnless(
    shutil.which("qemu-system-x86_64")
    and Path("/dev/kvm").exists()
    and os.access("/dev/kvm", os.R_OK | os.W_OK),
    "experiment-v1 requires accessible /dev/kvm",
)
class ExperimentHardwareKvmIntegrationTest(unittest.TestCase):
    def test_real_experiment_profile_boots_builds_pauses_and_stops(self) -> None:
        repository = Path(__file__).resolve().parents[1]
        image = _build_seed(repository)
        qemu = shutil.which("qemu-system-x86_64")
        self.assertIsNotNone(qemu)

        with tempfile.TemporaryDirectory() as temporary:
            runtime = CodexOSRun(temporary, qemu)
            try:
                runtime.start(image)
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertIn("build", runtime.list_tools())
                build = runtime.invoke_tool("build", [])
                self.assertEqual(build.status, 0, build.output.decode())
                pid = runtime.active_pid
                runtime.pause()
                self.assertIs(runtime.state, RuntimeState.PAUSED)
                self.assertEqual(runtime.active_pid, pid)
                runtime.resume()
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                self.assertEqual(runtime.active_pid, pid)
                finish = runtime.invoke_tool(
                    "finish_generation",
                    [b"KVM hardware profile verification."],
                )
                self.assertEqual(finish.status, 0, finish.output.decode())
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                completed = runtime.inspect_generation(0)
                self.assertEqual(completed.outcome, "completed")
                self.assertEqual(
                    completed.hardware.profile,
                    "experiment-v1",
                )
                self.assertEqual(completed.hardware.accelerator, "kvm")
                self.assertEqual(completed.hardware.cpu_model, "host")
                self.assertEqual(completed.hardware.vcpus, 4)
                self.assertEqual(completed.hardware.memory_mib, 8192)

                runtime.continue_generation()
                self.assertIs(runtime.state, RuntimeState.RUNNING)
                runtime.abort_generation()
                self.assertIs(
                    runtime.state,
                    RuntimeState.AWAITING_NEXT_GENERATION,
                )
                aborted = runtime.inspect_generation(1)
                self.assertEqual(aborted.outcome, "aborted")
                self.assertEqual(aborted.hardware, completed.hardware)
            finally:
                runtime.stop()
            self.assertIsNone(runtime.active_pid)


def _option_values(arguments: list[str], option: str) -> list[str]:
    return [
        arguments[index + 1]
        for index, value in enumerate(arguments[:-1])
        if value == option
    ]


def _build_seed(repository: Path) -> Path:
    result = subprocess.run(
        ["make", "seed"],
        cwd=repository,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=60.0,
        check=False,
    )
    if result.returncode != 0:
        raise AssertionError(result.stdout)
    return repository / "build" / "seed" / "codexos-seed.iso"


if __name__ == "__main__":
    unittest.main()

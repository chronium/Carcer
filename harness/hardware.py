"""Concrete virtual hardware profiles for CodexOS generations."""

from __future__ import annotations

import json
import os
import re
import subprocess
from dataclasses import dataclass
from pathlib import Path

_PROFILE_PATTERN = re.compile(r"[a-z0-9][a-z0-9-]{0,63}")
_CPU_PATTERN = re.compile(r"[A-Za-z0-9_.+-]{1,64}")
_QEMU_VERSION_LIMIT = 256


@dataclass(frozen=True)
class HardwareManifest:
    schema_version: int
    profile: str
    machine: str
    accelerator: str
    cpu_model: str
    vcpus: int
    memory_mib: int
    graphics: str
    network: str
    writable_block_devices: tuple[str, ...]
    qemu_version: str
    qemu_arguments: tuple[str, ...]

    def as_json_object(self) -> dict[str, object]:
        return {
            "schema_version": self.schema_version,
            "profile": self.profile,
            "machine": self.machine,
            "accelerator": self.accelerator,
            "cpu_model": self.cpu_model,
            "vcpus": self.vcpus,
            "memory_mib": self.memory_mib,
            "graphics": self.graphics,
            "network": self.network,
            "writable_block_devices": list(self.writable_block_devices),
            "qemu_version": self.qemu_version,
            "qemu_arguments": list(self.qemu_arguments),
        }


@dataclass(frozen=True)
class CodexOSHardwareProfile:
    """One trusted, fixed QEMU hardware configuration."""

    profile: str
    machine: str
    accelerator: str
    cpu_model: str
    vcpus: int
    memory_mib: int
    graphics: str = "std-vga"
    network: str = "none"
    writable_block_devices: tuple[str, ...] = ()

    def __post_init__(self) -> None:
        _validate_profile_values(
            self.profile,
            self.machine,
            self.accelerator,
            self.cpu_model,
            self.vcpus,
            self.memory_mib,
            self.graphics,
            self.network,
            self.writable_block_devices,
        )

    def require_available(self) -> None:
        if self.accelerator != "kvm":
            return
        kvm = Path("/dev/kvm")
        if not kvm.exists() or not os.access(kvm, os.R_OK | os.W_OK):
            raise RuntimeError(
                f"{self.profile} requires KVM, but /dev/kvm is unavailable"
            )

    def qemu_arguments(
        self,
        boot_iso: Path,
        qmp_socket: Path,
        serial_socket: Path,
    ) -> list[str]:
        boot_file = json.dumps(
            {
                "driver": "file",
                "filename": str(boot_iso),
                "node-name": "codexos-boot-file",
                "read-only": True,
            },
            separators=(",", ":"),
        )
        boot_raw = json.dumps(
            {
                "driver": "raw",
                "file": "codexos-boot-file",
                "node-name": "codexos-boot-cd",
                "read-only": True,
            },
            separators=(",", ":"),
        )
        return [
            "-machine",
            (
                f"{self.machine},accel={self.accelerator},"
                "pcspk-audiodev=codexos-noaudio"
            ),
            "-cpu",
            self.cpu_model,
            "-smp",
            str(self.vcpus),
            "-m",
            f"{self.memory_mib}M",
            "-nodefaults",
            "-display",
            "none",
            "-monitor",
            "none",
            "-no-reboot",
            "-nic",
            "none",
            "-audiodev",
            "none,id=codexos-noaudio",
            "-blockdev",
            boot_file,
            "-blockdev",
            boot_raw,
            "-device",
            "ide-cd,drive=codexos-boot-cd,bootindex=1",
            "-device",
            "VGA",
            "-chardev",
            (
                "socket,id=codexos-com1,"
                f"path={serial_socket},server=on,wait=off"
            ),
            "-device",
            "isa-serial,chardev=codexos-com1,index=0",
            "-qmp",
            f"unix:{qmp_socket},server=on,wait=off",
        ]

    def manifest(self, qemu_version: str) -> HardwareManifest:
        normalized_arguments = self.qemu_arguments(
            Path("<BOOT_ISO>"),
            Path("<QMP_SOCKET>"),
            Path("<SERIAL_SOCKET>"),
        )
        return validate_hardware_manifest(
            {
                "schema_version": 1,
                "profile": self.profile,
                "machine": self.machine,
                "accelerator": self.accelerator,
                "cpu_model": self.cpu_model,
                "vcpus": self.vcpus,
                "memory_mib": self.memory_mib,
                "graphics": self.graphics,
                "network": self.network,
                "writable_block_devices": list(
                    self.writable_block_devices
                ),
                "qemu_version": qemu_version,
                "qemu_arguments": normalized_arguments,
            }
        )


def discover_qemu_version(executable: str) -> str:
    try:
        result = subprocess.run(
            [executable, "--version"],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=5.0,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        raise RuntimeError(f"could not determine QEMU version: {error}") from error
    if result.returncode != 0:
        raise RuntimeError("could not determine QEMU version")
    lines = result.stdout.splitlines()
    if not lines:
        raise RuntimeError("QEMU version output is empty")
    version = lines[0]
    if (
        not version
        or len(version.encode("utf-8")) > _QEMU_VERSION_LIMIT
        or not version.isprintable()
    ):
        raise RuntimeError("QEMU version output is invalid")
    return version


def validate_hardware_manifest(value: object) -> HardwareManifest:
    expected = {
        "schema_version",
        "profile",
        "machine",
        "accelerator",
        "cpu_model",
        "vcpus",
        "memory_mib",
        "graphics",
        "network",
        "writable_block_devices",
        "qemu_version",
        "qemu_arguments",
    }
    if not isinstance(value, dict) or set(value) != expected:
        raise ValueError("generation hardware manifest is malformed")
    if type(value["schema_version"]) is not int or value["schema_version"] != 1:
        raise ValueError("generation hardware manifest is malformed")

    writable = value["writable_block_devices"]
    if not isinstance(writable, list) or writable:
        raise ValueError("generation hardware manifest is malformed")
    _validate_profile_values(
        value["profile"],
        value["machine"],
        value["accelerator"],
        value["cpu_model"],
        value["vcpus"],
        value["memory_mib"],
        value["graphics"],
        value["network"],
        tuple(writable),
    )
    qemu_version = value["qemu_version"]
    if (
        not isinstance(qemu_version, str)
        or not qemu_version
        or len(qemu_version.encode("utf-8")) > _QEMU_VERSION_LIMIT
        or not qemu_version.isprintable()
    ):
        raise ValueError("generation hardware manifest is malformed")
    qemu_arguments = value["qemu_arguments"]
    profile = CodexOSHardwareProfile(
        profile=value["profile"],
        machine=value["machine"],
        accelerator=value["accelerator"],
        cpu_model=value["cpu_model"],
        vcpus=value["vcpus"],
        memory_mib=value["memory_mib"],
        graphics=value["graphics"],
        network=value["network"],
        writable_block_devices=tuple(writable),
    )
    expected_arguments = profile.qemu_arguments(
        Path("<BOOT_ISO>"),
        Path("<QMP_SOCKET>"),
        Path("<SERIAL_SOCKET>"),
    )
    if qemu_arguments != expected_arguments:
        raise ValueError("generation hardware manifest is malformed")
    return HardwareManifest(
        schema_version=1,
        profile=value["profile"],
        machine=value["machine"],
        accelerator=value["accelerator"],
        cpu_model=value["cpu_model"],
        vcpus=value["vcpus"],
        memory_mib=value["memory_mib"],
        graphics=value["graphics"],
        network=value["network"],
        writable_block_devices=tuple(writable),
        qemu_version=qemu_version,
        qemu_arguments=tuple(qemu_arguments),
    )


def _validate_profile_values(
    profile: object,
    machine: object,
    accelerator: object,
    cpu_model: object,
    vcpus: object,
    memory_mib: object,
    graphics: object,
    network: object,
    writable_block_devices: object,
) -> None:
    if not isinstance(profile, str) or _PROFILE_PATTERN.fullmatch(profile) is None:
        raise ValueError("invalid CodexOS hardware profile")
    if machine != "q35":
        raise ValueError("invalid CodexOS hardware machine")
    if accelerator not in {"kvm", "kvm:tcg"}:
        raise ValueError("invalid CodexOS hardware accelerator")
    if not isinstance(cpu_model, str) or _CPU_PATTERN.fullmatch(cpu_model) is None:
        raise ValueError("invalid CodexOS hardware CPU model")
    if type(vcpus) is not int or not 1 <= vcpus <= 256:
        raise ValueError("invalid CodexOS hardware vCPU count")
    if type(memory_mib) is not int or not 1 <= memory_mib <= 1_048_576:
        raise ValueError("invalid CodexOS hardware memory size")
    if graphics != "std-vga" or network != "none":
        raise ValueError("invalid CodexOS peripheral hardware")
    if writable_block_devices != ():
        raise ValueError("writable block devices are not supported")


EXPERIMENT_HARDWARE_PROFILE = CodexOSHardwareProfile(
    profile="experiment-v1",
    machine="q35",
    accelerator="kvm",
    cpu_model="host",
    vcpus=4,
    memory_mib=8192,
)

TEST_HARDWARE_PROFILE = CodexOSHardwareProfile(
    profile="test-v1",
    machine="q35",
    accelerator="kvm:tcg",
    cpu_model="qemu64",
    vcpus=1,
    memory_mib=128,
)

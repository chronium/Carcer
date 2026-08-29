# CodexOS experiment hardware, version 1

The first autonomous experiment uses one fixed nested-guest hardware profile,
`experiment-v1`. Host capacity does not change this profile:

| Component | Version-1 contract |
| --- | --- |
| Machine | q35 |
| Accelerator | KVM only; no TCG fallback |
| CPU | `host`, 4 vCPUs |
| Memory | 8192 MiB |
| Boot medium | CodexOS/Limine ISO as an explicit read-only CD-ROM |
| Control | QMP Unix socket |
| Guest/harness transport | COM1 serial Unix socket |
| Graphics | One standard VGA device |
| Display frontend | None (headless) |
| Network interfaces | None |
| Writable block devices | None |
| Audio | None |
| Automatic reboot | Disabled |

QEMU is launched with default peripheral creation disabled. The CD-ROM, COM1,
QMP endpoint, VGA device, and disabled audio backend are explicit. Hardware
inherent to q35—including its chipset, PCI/PCIe buses, ACPI, RTC, interrupt
controllers, timers, and basic platform input—remains present. The profile
constrains the bootstrap without artificially constraining what CodexOS may
discover and build later.

KVM is part of the experiment contract. If `/dev/kvm` is unavailable or not
accessible, `experiment-v1` startup fails; it never silently switches to TCG.
A separate fixed `test-v1` profile keeps automated real-QEMU tests portable and
small, but it is not the production default and is not an experiment profile.

There is no NIC in version 1. This is stronger than filtering Internet access:
the guest receives no network-interface device. There is also no writable disk,
including no scratch disk. Writable guest storage would introduce lineage state
that the current generation archives and rollback operation do not snapshot.
Adding such storage requires explicit archive and rollback semantics first.

The VGA device is guest-visible from generation 0 while QEMU remains headless.
A future spectator frontend can display that same baseline device without
requiring guest-visible graphics hardware to appear accidentally later.

Every completed or aborted generation archive contains `hardware.json`, which
records the trusted profile values and the actual QEMU version used for that
incarnation. It also records the normalized QEMU arguments with boot, serial,
and QMP paths replaced by fixed placeholders, so temporary host paths never
enter the archive. Rollback selects source and the successor ISO from the
chosen historical generation; it does not restore that generation's hardware
manifest. The new generation always records the current trusted profile it
actually receives.

Feature-request approval does not change this profile automatically. If a
future approved request justifies a trusted hardware change, the harness change
must deliberately use a new profile identifier, and subsequent archives must
record that new profile. Persistent source Git provenance remains limited to
the CodexOS source snapshot and does not include `hardware.json`.

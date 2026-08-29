# CodexOS build host service, version 1

The `build` host service uses the request and response envelope defined in
[`host-service-protocol.md`](host-service-protocol.md). This document defines
only the service-specific argument and result semantics.

The service name is the UTF-8 string `build`. Its request contains exactly one
argument: the complete binary version 1 source snapshot defined in
[`source-snapshot.md`](source-snapshot.md). The guest supplies no command,
toolchain selection, host path, or other build configuration.

For a `build` response, status has these service-specific meanings:

| Status | Meaning |
| --- | --- |
| `0` | The build succeeded |
| `1` | Guest source compilation, assembly, linking, or image construction failed |
| `2` | The request, snapshot, trusted harness, or build environment failed |

The response output is the trusted builder's bounded textual diagnostics
encoded as UTF-8. Empty diagnostics are valid. Kernel and ISO bytes and host
filesystem paths are never returned through this protocol.

On success, the harness retains `kernel.elf` and `codexos.iso` in host-owned
staging storage selected by trusted harness code. Each attempt uses fresh
storage. A failed attempt does not replace the latest successful artifacts.
Generation switching, rebooting, and artifact archival are not part of this
service.

# CodexOS finish-generation host service, version 1

The `finish_generation` host service uses the request and response envelope
defined in [`host-service-protocol.md`](host-service-protocol.md). This
document defines only the service-specific arguments, results, and pending
transition semantics.

The request contains exactly two arguments:

1. `handoff_message`: valid UTF-8 text from zero through 16 KiB encoded bytes.
2. `current_source_snapshot`: one complete validated version 1 source snapshot
   as defined in [`source-snapshot.md`](source-snapshot.md).

The current source snapshot must be byte-for-byte identical to the validated
snapshot retained for the latest successful `build` service invocation. This
prevents edits made after a successful build from being silently discarded.

Response status has these service-specific meanings:

| Status | Meaning |
| --- | --- |
| `0` | The generation finish was accepted |
| `1` | No matching successful build exists, or a finish was already accepted |
| `2` | The request was malformed or the trusted harness failed |

Success output is empty. Rejection output is a short UTF-8 explanation.

On success, the trusted harness records one pending transition containing the
handoff text, exact source snapshot, and corresponding successful kernel ELF
and bootable ISO. Once accepted, another finish request and subsequent build
requests are rejected by the same host-service session. Acceptance does not
stop QEMU, close serial communication, archive a generation, or start the
successor.

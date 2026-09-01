# Go harness architecture

This is the ownership plan for the Go harness. It records concrete current
responsibilities; packages are added only when their first working behavior is
implemented. The Python implementation remains authoritative during migration.

## Ownership

`cmd/codexos` owns only process entry and exit. Cobra owns the compatible CLI
surface. Viper will be used only if configuration or environment binding proves
clearer than direct Cobra flag handling.

`internal/operator` coordinates opening a run, line-oriented commands, operator
confirmations, and shutdown order. `internal/tui` owns the Bubble Tea v2 model,
cursed rendering, terminal interaction, and translation of operator/runtime
events into views. Neither package owns experiment state.

`internal/experiment` owns the run and generation state machine. It coordinates
the concrete QEMU, guest, build, store, provenance, observability, and agent
operations. It does not hide them behind general provider or service layers.

`internal/guest` owns byte-level framing, source snapshot and tool/host-service
protocols, the serial transport, and the single-reader duplex dispatcher. The
dispatcher owns the sole read loop and all ordered writes. One serialized tool
exchange waits on dispatcher state, while host-service work runs synchronously on
the read loop so its response is queued before later frames are handled.

`internal/qemu` owns the hardware profile, QEMU child process, QMP connection,
and bounded process shutdown. `internal/build` owns the fixed trusted build and
candidate validation operation. Neither accepts a guest-provided command.

`internal/store` owns compatible on-disk records and atomic/durable publication.
`internal/provenance` owns build/review/planning evidence and Git lineage. These
are separate because Git reconciliation and evidence allocation have lifecycle
rules beyond generic file storage.

`internal/codexapp` owns one app-server subprocess and its JSONL request,
notification, catalog, interrupt, and shutdown protocol. `internal/agent` owns
planning, implementation, review, and interview sessions and their distinct
tool/capability policies. `internal/observability` owns structured events,
bounded metrics, optional OTLP export, and the non-persistent activity stream.

## State machine

The authoritative Python run states are:

```text
STOPPED --start/reopen+continue--> RUNNING --pause--> PAUSED
   ^                                |  ^              |
   |                                |  +----resume----+
   |                                |
   |                           finish/abort
   |                                v
   +----------stop--------- AWAITING_NEXT_GENERATION
                                  |
                       continue or rollback/fork
                                  v
                               RUNNING
```

A cooperative finish freezes and archives a validated selected successor before
entering the gate. An abort archives the aborted generation without a successor.
Pause retains the same guest and Codex session; a gate permanently ends that
generation, and continuation creates a fresh Codex session. Reopening a gate is
read-only validation until the operator explicitly continues or rolls back.

## Persistence rules

Readers validate complete schemas, canonical names, identities, ancestry, and
hashes before accepting state. Writers follow the Python operation order: stage
content in the target filesystem, flush file content when required, publish by
atomic rename, flush the containing directory when required, and recover only
from independently verified immutable content. Existing complete records are
never silently rewritten. Malformed or future state fails closed.

Go must load Python-generated state before it is allowed to publish compatible
state. Bidirectional fixtures will then prove that Python accepts Go output.
Until those checks cover a format, its row in the parity matrix remains partial
or not started.

## Concurrency and cancellation

Operation lifetimes accept `context.Context`. A long-lived goroutine has one
documented owner and a bounded join path. The serial dispatcher has one reader,
bounded request and write queues, dispatcher-owned ordered output, read-first
servicing, and an independent progress-based write-stall deadline. Trusted nested
host computation and active response emission suspend the ordinary guest response
timeout without disabling that write-stall deadline. App-server, QEMU, QMP,
telemetry, activity, and TUI workers follow the same owned-shutdown rule.

Pure codecs do not start goroutines and accept bounded byte slices. Transport
owners apply deadlines and cancellation around blocking I/O.

## Current implemented slice

`internal/guest` currently provides the version 1 frame, source snapshot,
host-service, and tool payload codecs, the raw serial connection, the sole-reader
duplex dispatcher, canonical READY handling, and the request-matching tool client.
The dispatcher bounds queued output and write stalls, preserves read-first nested
host-service handling, and owns all framed reads and ordered writes.
`internal/store` provides the compatible
feature-request ledger, including sparse imports, authoritative refreshes, and
atomic synced record replacement. `internal/provenance` records planning
allocation, attempts, interruption/resumption, exact private responses, and
public response identities in byte-compatible manifests. It also captures the
filtered exit-interview transcript and atomically publishes one immutable,
run-scoped Markdown artifact. `internal/qemu`
provides the fixed hardware profiles and archived manifest codec plus the
synchronous, context-aware QMP client with bounded message reads. The QEMU
controller owns one direct child, its parent log descriptors, and one bounded
wait path; it deliberately matches Python by not creating a process group. Go
tests cover exact
encoding, bounds, malformed input, fragmentation/coalescing, canonical
snapshots, persistence failure, and cross-language output. Black-box tests run
the Python reference modules without importing optional production dependencies.
Synthetic Unix peers exercise QMP lifecycle and failure behavior. No Go code
currently starts QEMU, contacts Codex, or changes the operational entry point.
`internal/codexapp` currently provides a bounded UTF-8 JSONL message codec,
validates cumulative token-usage notifications, and derives non-duplicating
metric deltas; it does not yet start an app-server process.

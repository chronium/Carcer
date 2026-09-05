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
atomic synced record replacement. It also freezes provided assets into exact
single-file or deterministic PAX-tar bytes, maintains append-only activation
revisions, and serves bounded descriptor/read requests. Cross-run bootstrap
validates an immutable completed source generation, successor ISO, annotated Git
base, handoff, and feature ledger before atomically publishing a fresh run.
`internal/provenance`
records planning allocation, attempts, interruption/resumption, exact private
responses, and public response identities in byte-compatible manifests. It also captures the
filtered exit-interview transcript and atomically publishes one immutable,
run-scoped Markdown artifact. Build and review evidence records exact source and
artifact identities, preserves incomplete attempts, and publishes only validated
latest-success evidence. Generation Git reconciliation creates exact commits and
annotated tags from immutable archives, updates only validated run-scoped lineage
refs, and leaves the configured developer worktree untouched. `internal/qemu`
provides the fixed hardware profiles and archived manifest codec plus the
synchronous, context-aware QMP client with bounded message reads. The QEMU
controller owns one direct child, its parent log descriptors, and one bounded
wait path; it deliberately matches Python by not creating a process group. Go
tests cover exact
encoding, bounds, malformed input, fragmentation/coalescing, canonical
snapshots, persistence failure, and cross-language output. Black-box tests run
the Python reference modules without importing optional production dependencies.
`internal/observability` owns a validated append-only event log with sequence
recovery and a bounded in-memory activity stream that exposes only explicitly
renderable app-server text. Its OpenTelemetry owner records the fixed metric
set with bounded low-cardinality attributes and optionally adds a bounded
OTLP/HTTP exporter; telemetry failures never control the experiment.
Synthetic Unix peers exercise QMP lifecycle and failure behavior. Candidate
validation can start only a disposable QEMU, and reviewer tests contact only a
synthetic app server. The side-by-side Go command does not change the Python
default or access live experiment state.
`internal/codexapp` owns an isolated one-shot app-server process, sole JSONL
reader, ordered writer, concurrent request routing, bounded notification and
server-request queues, catalog/policy validation, interrupts, and TERM/KILL
shutdown. It also validates cumulative token usage and derives non-duplicating
metric deltas. The generation session uses that client for same-thread planning
and implementation turns with per-turn cancellation and bounded callback joins.
`internal/agent` also owns a fresh, isolated reviewer process, workspace, and
thread for each consultation. A review request closes the originating Sol turn
to new tools, quiesces it, captures source atomically under the live runtime's
serialized operation lock, and exposes only snapshot-backed read tools to Luna.
Snapshot construction selects the same `seed/` source paths as a guest build;
it requests the guest's newline-delimited list with that exact prefix, so
non-source filenames cannot affect decoding. CR and LF are forbidden in
canonical source paths, while every selected path and byte is independently
validated by the trusted length-framed source-snapshot codec.
The exact findings return through one trusted continuation on the same Sol
process, thread, and phase. Request, proposal, snapshot, and findings bytes are
private evidence; operational events carry only identities and digests. The
reviewer attributes activity and token metrics and has a bounded cancellation
and process-reaping path. Generation
sessions expose the distinct planning and implementation permission profiles,
freeze their discovered dynamic-tool set for one ephemeral thread, retain
private planning evidence, and keep planning continuations in that thread.
Implementor and reviewer sessions share one turn-scoped dynamic-call router:
writing a JSON-RPC result is only an attempt, while a matching terminal
`dynamicToolCall` item proves delivery. Unresolved planning calls fail the
attempt retryably and block implementation, and both session types join their
tool callbacks before advancing or removing isolated state. Review yields are
the deliberate exception: their originating request is rejected after admission,
recorded as `yielded` rather than delivered, and replaced by the trusted
continuation lifecycle instead of a long-running dynamic-tool response.
After a matching frozen finish, that same healthy thread may be retained at the
gate for tool-less read-only exit-interview turns. Retention leases the exact
completed-generation gate, so continuation and rollback cannot invalidate the
selected successor before session retirement releases it; an admitted question
is reserved before its app-server turn ID exists. Only renderable reasoning
summaries and final answers enter the isolated transcript, while questions and
answers never enter generation events or successor context. A mutex-protected
one-shot worker owns fresh-session construction and bounded retirement. The
operator controller joins this session to the live runtime, preserves it across
pause/resume, and retires it before continuation, rollback, abort completion, or
shutdown.
`internal/build` performs the fixed trusted source-snapshot build in a fresh
workspace. It copies pinned Limine inputs, discovers and validates the fixed
toolchain, runs guest compilation and ISO construction under bubblewrap without
interpreting guest commands, bounds diagnostics, kills cancelled process groups,
and publishes only non-colliding artifacts. The candidate validator owns one
disposable QEMU, establishes QMP and serial controls while execution is paused,
then requires canonical READY and list-tools proofs before success. Its bounded
cleanup path quits or stops and reaps the child even after cancellation. The
guest-facing host-service owner joins those operations, retains only the latest
fully validated successor, freezes a matching finish request, records advisory
feature requests, and routes explicitly configured provided assets.
`internal/experiment` publishes and loads immutable completed or aborted
generation archives, validates their ancestry and exact contents, and can
reopen a stopped run at a gate without starting any process. Its concrete live
owner composes one QEMU child, QMP client, serial connection, dispatcher, tool
client, candidate validator, and host-service session. It boots only after
trusted cross-run and provided-asset state is restored; streams boot,
successor, and log files into archives without collecting them in RAM; verifies
the candidate-proven kernel and ISO identities during that copy; and supports
pause/resume, completion, paused abort, boot-first continuation, and rollback.
Run-owned cancellation interrupts blocked guest/build work before shutdown
joins resources, while failed cleanup retains ownership and diagnostics for a
retry.
`internal/operator` defines the Cobra startup surface and validates the same
opening-mode, Git-provenance, cross-run inheritance, and display-mode
relationships as the Python entry point before invoking its concrete runner.
The same package checks both terminal streams and `TERM`, makes untrusted
terminal controls inert, and parses the exact plain-console `ask TEXT` form
without changing the question body. One console/controller owns all operator
commands and the generation session for both frontends. The runner initializes
cross-run state before observability, validates recorded Git identity before
boot, constructs the live runtime, and shuts down runtime, event log, then
metrics. `cmd/codexos` remains a thin side-by-side process entry point; Python
remains the default and only approved live entry point.
`internal/tui` provides the frontend-independent operator activity model. It
coalesces attributed message, reasoning, tool, feature-request, build,
operator, interview, and abnormal-lifecycle events into typed immutable
snapshots; applies independent entry, byte, and payload bounds; and makes all
untrusted terminal controls inert. Its Bubble Tea v2 application uses the
cursed renderer for one full-screen session, owns line-based scroll/follow and
tool-detail presentation, serializes prompts and confirmations, routes
asynchronous operator output, cancels command work on shutdown, boundedly joins
the active command, and invokes integration cleanup only after Bubble Tea
returns. Streaming messages remain literal while finalized messages are rendered
with bounded Glamour Markdown and a frontend-only cache; the activity model
retains canonical text. Caller cancellation is relayed as a graceful Bubble Tea
quit while command work uses a separately cancelled context, ensuring the Linux
terminal reader is joined before its descriptor is closed on harness-owned
shutdown paths. It delegates command meaning, session ownership, interview
Markdown, and runtime shutdown to the same console used by plain mode.

## Per-run source capacity

`internal/sourcecapacity` defines the two supported Go content budgets and their
small durable record. `internal/experiment` owns existing-run gate provisioning and
freezes the effective record in new provisioned archives. Cross-run bootstrap
can explicitly provision a destination budget before atomic publication using
`--inherit-source-capacity`; seed-only startup retains its default. Guest snapshot/capture,
review, trusted build/finish, archive loading, Git provenance, and cross-run
inheritance explicitly select the current run or historical archive budget.
Defaults remain 64 KiB; no process-global budget or general resource registry is
introduced. See [run source capacity](run-source-capacity.md) for semantics,
framing limits, compatibility, and post-merge provisioning of request #4.

## Optional Linux bootstrap service

`internal/bootstrap` owns strict job requests, bounded captured-input transfer,
rootless worker lifecycle, safe frozen output collection and immutable opaque
artifact storage. `cmd/codexos-bootstrap` is the fixed dedicated-account worker;
it is installed separately and receives no harness/run storage access.
`internal/experiment` owns inactive-gate provisioning, invocation scope and
cancellation, generation reference freezing, and selected-parent authorization.
`internal/store` includes selected artifact copying in atomic cross-run bootstrap.
The existing guest dispatcher and agent delivery/review lifecycle remain in place.
The two dynamic guest helpers are exposed only when the guest advertises them.
See [operator setup](bootstrap-service-operations.md) and the
[Go host-service contract](bootstrap-host-services.md). This service is disabled
by default and does not fulfill or change the status of feature request #3.

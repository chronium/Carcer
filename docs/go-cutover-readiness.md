# Go cutover readiness

Status: **not ready for cutover**. The Python harness remains fully operational
and is still the only operational entry point.

## Completed capabilities

The Go implementation has compatible codecs for serial frames, source snapshots,
host-service requests/responses, tool lists, tool invocation requests, and tool
results. Its sole-reader serial dispatcher owns canonical READY detection,
read-first duplex pumping, ordered partial writes, nested host-service routing,
tool response matching, and bounded write/shutdown deadlines. The tool client
implements discovery, invocation, request-ID rollover, and response validation.
These components are not yet wired into candidate boot or a generation runtime.
Go also reads and writes the compatible feature-request store, including inherited
sparse IDs and durable decisions; gate-only decision enforcement remains with the
Python runtime. Provided-assets snapshots, deterministic PAX archives, append-only
activation manifests, and bounded guest list/read handling are implemented;
operator and generation routing remain unwired. The synchronous QMP client
implements Unix-socket retry, greeting and capability negotiation, status/control
commands, event skipping, deadlines, and cancellation. The two fixed hardware profiles, exact QEMU arguments, strict
archived manifest codec, KVM availability check, bounded QEMU version discovery,
and direct-child QEMU controller are implemented. Planning evidence allocation,
attempt history, exact private responses, digest publication, filtered
exit-interview capture, and immutable Markdown publication are implemented.
Build and review evidence includes immutable sequence allocation, exact byte
identities, incomplete-attempt preservation, source-read capture, and
fail-closed latest-success publication. These provenance components are not yet
wired to Go build or Codex sessions. Generation Git reconciliation preserves the
configured base commit, exact source trees/messages/annotations, rollback
lineages, immutable conflicts, and the developer worktree; operator integration
remains unwired. The app-server slice provides a
bounded UTF-8 JSONL codec, validates cumulative token-usage notifications, and
derives exact non-duplicating deltas; process and session ownership remain
unimplemented. The local observability slice validates and appends sequenced
JSONL events without allowing recording failures to control the experiment. Its
bounded activity stream preserves concurrent ordering and excludes raw reasoning;
metrics and OTLP export remain unimplemented.
Cross-run bootstrap can initialize and reload a fresh run from a validated latest
completed generation, preserving exact successor-ISO, handoff, feature-ledger,
and annotated Git-base identities. Generation lifecycle wiring remains separate.

## Verification performed

The current milestone is verified with `go test ./...`. Tests cover exact bytes,
round trips, malformed and oversized input, fragmentation, coalescing, and fuzz
properties. Black-box conformance tests compare wire bytes, feature records, and
planning, build, and review evidence trees against the Python modules and
provided-assets bytes and manifests against Python, and exercise Python-to-Go
and Go-to-Python feature loading. Synthetic Unix-socket
peers verify exact QMP requests, fragmented input, asynchronous events, protocol
errors, connection retry, and cancellation without starting QEMU. Hardware conformance compares
exact command arguments and archived manifest bytes with the Python module;
malformed manifest parsing also has a fuzz target. Synthetic subprocesses cover
normal exit, duplicate start, cancellation, restart, log failures, and forced
kill, while an optional disposable `-machine none` QEMU test exercises the real
controller/QMP boundary without KVM or a guest image.

Serial verification includes fragmented READY and frames, read-before-write
ordering, forced partial writes, nested host requests, response-timeout suspension,
scope routing, malformed-request recovery, cancellation, peer closure, write
stall/failure, callback reentrancy, large host responses, progress events, and
bounded shutdown. The dispatcher and tool client are not yet wired into a guest
lifecycle.

## Remaining gaps and known differences

Most persistence and all generation lifecycle, Codex session, observability, CLI,
operator, and TUI capabilities remain to be implemented. Go `ReadFrame` relies on
its transport owner for deadlines, while Python's public helper currently applies
a five-second deadline itself; the dispatcher provides the bounded production
transport path.

There are no intentional wire-format differences in the implemented codecs.
Feature IDs and generation numbers are represented as uint64 in Go; unlike
Python's unbounded integers, larger persisted values are rejected. Such values
cannot describe an operable CodexOS generation, but this remains a documented
validation difference. Planning and exit-interview generation numbers are also
uint64. Planning thread/turn IDs and transcript text must be valid UTF-8 in Go;
Python can represent lone Unicode surrogates, although the real app-server
protocol supplies valid Unicode strings. The Go QMP client
rejects an individual message larger than 1 MiB; the Python reader has no
explicit message bound. Normal QEMU greetings, events, and replies are far below
that defensive limit. Go also requires response IDs to be unsigned JSON integer
tokens; Python's general value equality would accept unusual equivalents such as
`1.0` or `true`. QEMU emits integer request IDs, so compliant traffic is
unchanged. Go bounds a hardware manifest and captured QEMU version output at 1
MiB and requires QEMU command paths to be valid UTF-8; Python has no explicit
bounds and can represent surrogate-escaped paths. Operable CodexOS paths and
normal QEMU version output remain well inside these conservative constraints.
The Python controller reaps an exited QEMU when `is_running` or `pid` is read;
Go uses one owned wait goroutine and may close the parent log descriptors sooner,
while preserving the reported process state and archived log contents.
Python's serial dispatcher uses an unbounded write deque. Go instead fails closed
when more than 8 writes or approximately 32 MiB are queued, preventing a trusted
host-service burst from growing memory without bound; ordinary traffic remains
well below both limits.
Go rejects an individual app-server JSONL message above 16 MiB, while Python's
line reader has no explicit bound. Normal protocol messages are substantially
smaller; the bound prevents malformed output from exhausting harness memory.
Go also bounds an individual durable event at 16 MiB and queues at most 4096
live activity events. Python leaves both paths unbounded; normal metadata-only
events remain far below these limits, and live observation remains non-controlling.
Go bounds combined stdout/stderr from each Git plumbing command at 32 MiB;
Python captures command output without an explicit limit. Valid snapshots are
already bounded and ordinary ref/status output is substantially smaller.
Cross-run loading bounds each provenance file at 16 MiB and each Git verification
command at 1 MiB of combined output. Python does not impose those explicit
limits; valid handoffs, ledgers, manifests, and Git identities are much smaller.

## Operational risks

The highest risks are durable archive compatibility, fail-closed provenance,
single-reader duplex serial behavior during large nested responses, generation
gate semantics, and same-thread planning-to-implementation continuity. No Go
component should be used on live experiment state while these remain unverified.

## Recommended staged cutover

After the parity matrix is complete: validate read-only loading against copies of
Python runs; perform disposable Python-to-Go and Go-to-Python resumptions; run
the recorded non-interactive and TUI scenarios; shadow structured output without
controlling a live run; then require explicit human approval for any entry-point
change. Keep the Python command available through the staged period.

## Rollback procedure

Before any future cutover, stop at a generation gate, preserve the immutable
archive and harness version, and verify Python can read the last Go-published
state. Rollback selects the unchanged Python entry point and must not rewrite the
archive, Git refs, feature ledger, provided-assets provenance, or transcript
artifacts. Any incompatibility blocks rollback/cutover and is investigated on a
copy, never repaired in live state.

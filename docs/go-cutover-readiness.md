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
fail-closed latest-success publication. Review evidence is wired to the isolated
Go reviewer, and build evidence is wired to the joined trusted compile and
candidate-validation host service. Other Codex-session wiring remains incomplete.
Generation Git reconciliation preserves the
configured base commit, exact source trees/messages/annotations, rollback
lineages, immutable conflicts, and the developer worktree; operator integration
remains unwired. The app-server client owns an isolated one-shot process,
concurrent JSONL request routing, notifications and server requests, catalog and
thread/turn policy validation, interrupts, bounded diagnostics/queues, and
TERM/KILL reaping. The isolated reviewer creates a fresh process, workspace, and
thread per consultation, exposes only dynamically discovered read-only guest
tools, records source-read evidence, publishes activity and bounded token
metrics, and cleans up on cancellation. The isolated generation session runs
planning and implementation in one ephemeral app-server thread, enforces their
distinct permission and dynamic-tool policies, records fail-closed private
planning evidence, supports interrupted planning continuation, and bounds turn,
tool, and process shutdown. A matching frozen finish can retain that same
healthy thread for read-only exit-interview turns with no workspace roots or
dynamic tools. A runtime-held gate lease blocks continuation and rollback until
session retirement, and turn admission is reserved before the app server
returns an ID. The transcript accepts only renderable reasoning summaries and
final answers; direct close preserves a failed partial turn without inventing a
response. The single-flight compatibility worker always retires its fresh
session. Generation/operator orchestration remains unimplemented. The local
observability slice validates and appends sequenced JSONL events without allowing
recording failures to control the experiment. Its bounded activity stream
preserves concurrent ordering and excludes raw reasoning. The fixed
OpenTelemetry metric set preserves Python names, units, and public attributes,
and an explicitly configured OTLP/HTTP exporter has bounded export and shutdown
deadlines.
Cross-run bootstrap can initialize and reload a fresh run from a validated latest
completed generation, preserving exact successor-ISO, handoff, feature-ledger,
and annotated Git-base identities. Generation lifecycle wiring remains separate.
The trusted build operation validates and materializes a source snapshot, uses
only fixed host compiler/linker/Limine/xorriso commands inside bubblewrap, bounds
diagnostics and subprocess lifetime, and publishes kernel and ISO artifacts
without overwriting an existing result. The guest-facing build service creates a
fresh attempt, records fail-closed provenance, compiles the exact snapshot,
requires the disposable candidate proof, and retains only the most recent
validated success. The concrete host-service owner also freezes a finish request
only when its exact snapshot matches that success, records feature requests, and
routes frozen provided assets. The candidate validator boots only in a fresh
workspace, establishes QMP and serial before continuing execution, proves READY
and list-tools behavior, records forensic stages, and reaps the disposable QEMU
on every tested outcome.
The process-free lifecycle core writes and reloads immutable completed and
aborted generation archives, rejects partial or inconsistent histories, restores
an archived gate without booting, and requires an explicit successor or rollback
selection. It does not yet perform the selected QEMU boot or create the required
fresh Codex session.
The Cobra command surface validates the Python startup flags, mutually exclusive
opening and display modes, paired Git and inheritance options, and automatic
TUI selection before handing control to a runner. The concrete runner, operator
commands, and Go process entry point are not yet implemented. The
display probe itself requires both streams to be TTYs and rejects empty or
`dumb` terminals, matching Python.
The TUI activity model preserves attribution and semantic coalescing for
messages, exposed reasoning summaries, tools, feature requests, build phases,
operator output, interview questions, and abnormal lifecycle events. Its typed
snapshots are immutable to callers, its scrollback and payload display are
independently bounded, and hostile terminal controls are escaped. The Bubble
Tea v2 cursed application adds line-based live-follow, clickable bounded tool
details, single-line command/paste input, confirmations, the two-Escape pause
gesture, recoverable command errors, and cancellable post-restoration shutdown.
Finalized Markdown rendering, setup-failure restoration coverage, the upstream
terminal-reader shutdown race, and concrete operator/runtime wiring remain
incomplete.

## Verification performed

The current milestone passes both `go test ./... -timeout=180s` and
`go test -race ./... -timeout=240s` with external hard timeouts. The retained
session packages also pass five consecutive race-enabled runs, and process
checks find no surviving `agent.test` helpers. Tests cover exact bytes,
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

The Python candidate-validation integration test has a pre-existing timing
flake under the available TCG fallback: its 250 ms READY deadline can expire
before serial diagnostics arrive. An isolated run can pass, while an unmodified
`main` archive with the same pinned Limine fixture failed diagnostic assertions
in five of the first six completed repetitions; candidate rejection and cleanup
still occurred. This evidence is limited to that test and is not used to excuse
unrelated Python failures.

## Remaining gaps and known differences

Generation orchestration, live build/finish routing, exit-interview artifact
finalization in the operator flow, concrete CLI startup, operator commands,
finalized TUI Markdown, and concrete TUI integration remain to be implemented.
Go `ReadFrame` relies on
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
Go also limits either app-server notification queue to 256 messages and 32 MiB
of encoded input. Python's queues are unbounded; overflow fails the isolated
transport and cannot silently discard protocol state.
Go also bounds an individual durable event at 16 MiB and queues at most 4096
live activity events. Python leaves both paths unbounded; normal metadata-only
events remain far below these limits, and live observation remains non-controlling.
Go bounds combined stdout/stderr from each Git plumbing command at 32 MiB;
Python captures command output without an explicit limit. Valid snapshots are
already bounded and ordinary ref/status output is substantially smaller.
Cross-run loading bounds each provenance file at 16 MiB and each Git verification
command at 1 MiB of combined output. Python does not impose those explicit
limits; valid handoffs, ledgers, manifests, and Git identities are much smaller.
Generation archive loading similarly bounds metadata at 64 KiB, hardware and
forensic manifests at 1 MiB, handoffs at 16 KiB, and source snapshots at 1 MiB.
It also rejects symlinks anywhere under archived boot, source, or successor
trees, whereas Python checks their roots and named required files. Valid archives
produced by either harness contain no such symlinks and remain compatible.
Go bounds metric label values at 128 UTF-8 bytes and maps unknown reviewer
focuses, operator actions, and token roles to fixed fallback values. Python
passes those trusted strings through unchanged. Current harness catalogs and
event producers use the preserved values; normalization prevents malformed
telemetry from creating unbounded label cardinality.

## Operational risks

The highest risks are durable archive compatibility, fail-closed provenance,
single-reader duplex serial behavior during large nested responses, generation
gate semantics, and same-thread planning-to-implementation continuity. Bubble
Tea v2.0.9's Linux terminal reader also races its cancel-reader cleanup under a
real pseudo-terminal; the non-race PTY test proves restoration, but interactive
cutover remains blocked until that dependency race is fixed or safely worked
around. No Go component should be used on live experiment state while these
remain unverified.

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

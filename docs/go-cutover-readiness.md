# Go cutover readiness

Status: **not ready for cutover**. The Python harness remains fully operational
and is still the default and only approved live-experiment entry point. The
side-by-side Go command now exists for disposable verification.

## Completed capabilities

The Go implementation has compatible codecs for serial frames, source snapshots,
host-service requests/responses, tool lists, tool invocation requests, and tool
results. Its sole-reader serial dispatcher owns canonical READY detection,
read-first duplex pumping, ordered partial writes, nested host-service routing,
tool response matching, and bounded write/shutdown deadlines. The tool client
implements discovery, invocation, request-ID rollover, and response validation.
These components are now joined to candidate boot and the concrete generation
runtime. Go also reads and writes the compatible feature-request store, including
inherited sparse IDs, durable decisions, guest recording, and gate-only decision
enforcement. Provided-assets snapshots, deterministic PAX archives, append-only
activation manifests, and bounded guest list/read handling are routed through
operator flags, live startup, background, and exchange scopes. The synchronous
QMP client implements Unix-socket retry, greeting and capability negotiation,
status/control commands, event skipping, deadlines, and cancellation. The two
fixed hardware profiles, exact QEMU arguments, strict archived manifest codec,
KVM availability check, bounded QEMU version discovery, and direct-child QEMU
controller are implemented. Planning evidence allocation,
attempt history, exact private responses, digest publication, filtered
exit-interview capture, and immutable Markdown publication are implemented.
Build and review evidence includes immutable sequence allocation, exact byte
identities, incomplete-attempt preservation, source-read capture, and
fail-closed latest-success publication. Review evidence is wired to the isolated
Go reviewer, build evidence is wired to the joined trusted compile and
candidate-validation host service, and the operator owns one fresh generation
session across planning, implementation, pause/resume, and the frozen gate.
Generation Git reconciliation preserves the
configured base commit, exact source trees/messages/annotations, rollback
lineages, immutable conflicts, and the developer worktree. Startup validates a
cross-run bootstrap's literal base ref and resolved commit before boot, while
operator commands reconcile immutable generation records. The app-server client
owns an isolated one-shot process, concurrent JSONL request routing,
notifications and server requests, catalog and thread/turn policy validation,
interrupts, bounded diagnostics/queues, and TERM/KILL reaping. The isolated
reviewer creates a fresh process, workspace, and thread per consultation,
exposes only dynamically discovered read-only guest
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
response. The concrete operator controller owns that session, reserves turns
before subprocess admission, reuses it only within one generation, persists
completed or partial exit interviews, and retires it before every gate transition
or shutdown. The single-flight compatibility worker remains available for its
isolated compatibility boundary. The local
observability slice validates and appends sequenced JSONL events without allowing
recording failures to control the experiment. Its bounded activity stream
preserves concurrent ordering and excludes raw reasoning. The fixed
OpenTelemetry metric set preserves Python names, units, and public attributes,
and an explicitly configured OTLP/HTTP exporter has bounded export and shutdown
deadlines.
Cross-run bootstrap can initialize and reload a fresh run from a validated latest
completed generation, preserving exact successor-ISO, handoff, feature-ledger,
and annotated Git-base identities. The live runtime verifies and applies that
bootstrap before generation zero; the concrete runner performs initialization,
recorded-Git validation, and initial-ISO verification before starting the runtime.
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
The lifecycle core writes and reloads immutable completed and aborted generation
archives, rejects partial or inconsistent histories, restores an archived gate
without booting, and requires an explicit successor or rollback selection. Its
live owner now performs the selected boot, pause/resume, paused abort, completion,
continuation, and rollback over one owned QEMU/QMP/serial stack. Large artifacts
and logs are streamed into archive staging, and successor kernel/ISO identities
are checked during the copy against the candidate proof. Run cancellation
interrupts blocked guest/build work before joining resources; a shutdown failure
keeps ownership instead of falsely reporting a stopped run. The operator now
creates and retires the required fresh Codex session and preserves it across a
pause without allowing it to cross a generation boundary.
The Cobra command surface validates the Python startup flags, mutually exclusive
opening and display modes, paired Git and inheritance options, and automatic
TUI selection before handing control to the concrete runner. `cmd/codexos`
preserves parser-versus-startup exit classification and startup output streams;
the runner orders bootstrap, observability, runtime, Git, frontend, and cleanup
ownership without changing the Python default entry point. The line frontend
implements the complete operator command set. The display probe requires both
streams to be TTYs and rejects empty or `dumb` terminals, matching Python.
The TUI activity model preserves attribution and semantic coalescing for
messages, exposed reasoning summaries, tools, feature requests, build phases,
operator output, interview questions, and abnormal lifecycle events. Its typed
snapshots are immutable to callers, its scrollback and payload display are
independently bounded, and hostile terminal controls are escaped. The Bubble
Tea v2 cursed application adds line-based live-follow, clickable bounded tool
details, single-line command/paste input, confirmations, the two-Escape pause
gesture, recoverable command errors, asynchronous console output, and cancellable
post-restoration shutdown. It uses the same authoritative console/controller as
plain mode and boundedly joins an active command before integration cleanup.
Immutable interview Markdown is finalized by the console's provenance owner.
Streaming agent messages remain literal and finalized messages are rendered by
bounded Glamour Markdown without changing canonical activity text or enabling
terminal hyperlinks. Caller cancellation and external shutdown cancel command
work but send Bubble Tea a graceful quit, so its Linux terminal reader is joined
before descriptor close. The separately built command completes a disposable
generation through that TUI boundary and renders the frozen completed gate
before restoring the terminal and reaping every child.

## Verification performed

The current milestone passes both `go test ./... -count=1 -timeout=240s` and
`go test -race ./... -count=1 -timeout=300s` with the disposable socket/QEMU
tests enabled. The operator, TUI, and command packages also pass ten consecutive
race-enabled runs, and process checks find no surviving `agent.test`, operator,
or QEMU helpers. A process-free runner integration reopens an immutable aborted
gate through the real plain frontend and verifies observable startup-before-stop
ordering. The real Linux pseudo-terminal restoration regression passes 100
consecutive race-enabled runs for both direct application shutdown and caller
context cancellation. A separately built `cmd/codexos` executable also reopens
a disposable archived gate through both the plain frontend and a real
pseudo-terminal, exits cleanly on `quit` or `SIGTERM`, preserves event order,
and restores terminal state. A separately built command also completes a full
planning/build/finish generation through its real TUI using standalone QEMU and
Codex peers plus disposable build tools resolved through normal `PATH` and
`CODEX_HOME` behavior; it reaches the completed gate, shuts down from the PTY,
restores terminal state, and reaps every child. The concrete runner also starts
a fresh generation against a separately built standalone QEMU/QMP/serial peer, reports status,
pauses, permanently aborts with operator confirmation, publishes the aborted
archive, and leaves no live workspace. A second concrete-runner path uses
separately built standalone QEMU and Codex app-server peers to complete planning
and implementation in one thread, dispatch an ordinary guest tool, perform
nested build and finish host-service calls, run the fixed trusted build, boot and
exercise the candidate validator against a distinct synthetic QMP/serial peer,
freeze the exact successful snapshot, publish the completed generation-zero
archive, retain and then retire the same healthy Codex session at the gate, boot
the selected successor, archive its abort, roll back to generation zero's
successor in a later generation, archive that abort, and leave no child or live
workspace. A third runner scenario creates a completed source generation and
annotated Git provenance, atomically bootstraps a fresh destination from its
selected successor, exposes the inherited handoff and pending/approved feature
requests, archives an abort, and reaps its QEMU child. Tests cover exact bytes,
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
controller/QMP boundary without KVM or a guest image. An opt-in Linux acceptance
uses the available real cross-toolchain, bubblewrap, xorriso, pinned Limine
inputs, and QEMU to compile the canonical seed, boot its real ISO under the disposable
`test-v1` profile, observe READY, complete the canonical list-tools exchange,
and clean up the candidate workspace. It passes ten consecutive race-enabled
runs under the available TCG fallback.

Serial verification includes fragmented READY and frames, read-before-write
ordering, forced partial writes, nested host requests, response-timeout suspension,
scope routing, malformed-request recovery, cancellation, peer closure, write
stall/failure, callback reentrancy, large host responses, progress events, and
bounded shutdown through the concrete live guest lifecycle.

The Python reference suite ran 281 tests under the locked `uv` environment. Its
only failure was the following pre-existing candidate-validation timing
condition; one paid reviewer smoke test was skipped by design. The Python
harness/tests/lock files on this branch are byte-identical to `main`.
The candidate-validation integration test has a pre-existing timing
flake under the available TCG fallback: its 250 ms READY deadline can expire
before serial diagnostics arrive. An isolated run can pass, while an unmodified
`main` archive with the same pinned Limine fixture failed diagnostic assertions
in five of the first six completed repetitions; candidate rejection and cleanup
still occurred. This evidence is limited to that test and is not used to excuse
unrelated Python failures.

## Remaining gaps and known differences

Required remaining work is operational verification rather than another missing
owner layer: exercise the real seed image under the exact KVM-only
`experiment-v1` profile when `/dev/kvm` is available, and rerun the complete
reference suite when the documented TCG candidate timing test can succeed. The
same real image has passed candidate validation under the disposable `test-v1`
profile with TCG fallback. The plain frontend is exercised through a complete
live planning/build/finish/continue/rollback scenario; the TUI and separately
built Go command are exercised through a full planning/build/finish generation
and completed gate. Go `ReadFrame` relies on
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

### Recorded real-image candidate acceptance

From the repository root, with the required cross-toolchain and QEMU available,
the exact race-enabled command is:

```text
CODEXOS_REAL_IMAGE_ACCEPTANCE=1 GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/build -run '^TestRealSeedImageBuildsAndPassesCandidateValidation$' -count=10 -timeout=300s
```

`internal/build/real_image_acceptance_linux_test.go` reads the canonical seed
inputs, encodes their real source snapshot, and invokes the production trusted
build operation with explicitly resolved host utilities. It then boots the
resulting ISO in a fresh real QEMU process under `qemu.TestHardwareProfile`,
which falls back from KVM to TCG, and requires both the READY marker and the
canonical list-tools response. Every build and candidate workspace is temporary;
the validator must stop QEMU and remove its workspace before returning. The test
is opt-in so the standard offline suite does not acquire toolchain or QEMU
requirements. It uses no network, credentials, Codex session, telemetry, or live
experiment state. The exact `experiment-v1` profile remains separately gated on
an accessible `/dev/kvm`.

### Recorded disposable binary gate acceptance

From the repository root, the exact race-enabled command is:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./cmd/codexos -run '^TestCodexOSBinaryOperatesAtDisposableGate$' -count=20 -timeout=240s
```

`cmd/codexos/acceptance_linux_test.go` builds the actual command rather than
re-entering a Go test helper. Each case creates a fresh temporary aborted archive
with `experiment.WriteAbortedArchive` and `qemu.TestHardwareProfile`. Plain mode
receives `quit` over a pipe. TUI mode owns a fresh `/dev/ptmx` pair, receives
`SIGTERM` only after raw mode is observed, and must restore the original termios.
Neither case starts QEMU, Codex, a build, telemetry, or live state.

### Recorded disposable full binary/TUI lifecycle acceptance

From the repository root, the exact race-enabled command is:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./cmd/codexos -run '^TestCodexOSBinaryCompletesDisposableGenerationThroughTUI$' -count=10 -timeout=300s
```

This test builds the actual `cmd/codexos` command plus standalone QEMU and Codex
fixtures, places disposable fixed build utilities first in `PATH`, and supplies
a file-backed fake ChatGPT login through `CODEX_HOME`. It invokes only public CLI
flags and normal process configuration—there is no test-only command flag or
runner seam. A real Linux pseudo-terminal submits `agent`, and the command must
complete planning and implementation in one thread, dispatch the ordinary and
nested guest tools, build and validate the synthetic candidate, publish the
completed archive and handoff, render the completed gate, and quit from the TUI.
The test requires ordered lifecycle evidence, exact successor ISO bytes, terminal
restoration, no live workspace, and explicit reaping of the active QEMU,
candidate QEMU, and Codex children. The synthetic candidate peer proves protocol
ownership but not independent bootability of its fixture ISO. Because the public
command correctly selects the production hardware profile, this test requires
`/dev/kvm` and skips when that profile is unavailable even though its fake QEMU
does not consume KVM. No network, real Codex session, paid review, production
telemetry, or live experiment state is used.

### Recorded disposable live runner acceptance

From the repository root, the exact race-enabled command is:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/operator -run '^TestRunnerStartsPausesAndAbortsDisposableGeneration$' -count=10 -timeout=180s
```

`internal/operator/testdata/fakeqemu` is built as a standalone executable; it is
not a recursively invoked Go test binary. The runner receives that trusted path,
`qemu.TestHardwareProfile`, a fresh short `/tmp/codexos-runner-*` run directory,
and a synthetic initial ISO through an unexported acceptance-only configuration
boundary. The ordinary plain console drives `status`, `pause`, confirmed
`abort`, and `quit`. The test requires ordered start/pause/abort/stop events, an
immutable aborted generation-zero archive, and removal of the live generation
workspace. It invokes no Codex session, compiler, candidate VM, telemetry
endpoint, or live state.

### Recorded disposable large-transfer shutdown acceptance

From the repository root, the exact race-enabled commands are:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/guest -run '^TestDispatcher(LargeHostResponseOneByteProgressAndFraming|CloseDuringBlockedLargeHostResponse|AbortDuringBlockedLargeHostResponse)$' -count=10 -timeout=180s
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/operator -run '^TestRunnerAbortsDuringBlockedLargeHostResponse$' -count=10 -timeout=180s
```

The dispatcher scenario relays an exact 1 MiB G13-style host response one byte
at a time, then completes an ordinary tool exchange and a nested build exchange.
It also verifies bounded cancellation and close against a transport that cannot
make write progress. The operator scenario freezes a real 1 MiB provided asset,
boots the separately built QEMU peer with 1 KiB socket buffers, waits until the
host response has begun and made progress while the peer deliberately does not
read it, and issues a confirmed abort. QEMU teardown produces one terminal
`write_failed` event after the progress events; the abort still publishes its
immutable archive, removes the workspace, quits, and reaps the QEMU child within
the bounded test deadline. Neither scenario uses network, Codex, compilation,
paid review, production telemetry, or live experiment state.

### Recorded disposable cross-run bootstrap acceptance

From the repository root, the exact race-enabled command is:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/operator -run '^TestRunnerBootsCrossRunInheritanceWithGitProvenance$' -count=10 -timeout=180s
```

The test creates an immutable completed source generation, pending and approved
feature requests, and its annotated generation Git tag. It then drives the real
plain runner to atomically initialize a fresh destination, verify and boot the
source successor ISO, expose the inherited handoff and feature ledger, archive a
confirmed abort, and quit. The assertions cover the persisted bootstrap
identities, exact boot bytes, feature statuses, archive metadata, removal of the
live workspace, and reaping of the separately built standalone QEMU peer. No
network, Codex session, compiler, candidate VM, paid review, production telemetry,
or live experiment state is used.

### Recorded disposable completed-generation transitions

From the repository root, the exact race-enabled command is:

```text
GOCACHE=/tmp/codexos-go-cache GOMODCACHE=/tmp/codexos-go-modcache GOPATH=/tmp/codexos-go-path go test -race ./internal/operator -run '^TestRunnerCompletesContinuesAndRollsBackDisposableGeneration$' -count=10 -timeout=180s
```

`internal/operator/testdata/fakecodex` and
`internal/operator/testdata/fakeqemu` are built as standalone executables, never
as recursively invoked test binaries. The test uses an ordinary pipe-driven
plain console, a file-backed fake ChatGPT login, a short fresh `/tmp/co-live-*`
run root, and fixed disposable compiler, linker, bubblewrap, xorriso, and Limine
fixtures. The active serial peer advertises the canonical guest tools and nests
real build and finish host-service requests inside their tool exchanges. A
separate synthetic `-S` QEMU peer must pass READY and list-tools candidate
validation. This exercises the complete validator ownership and protocol path;
it does not claim that the fixture's synthetic ISO is independently bootable.
The test requires exact handoff and source-snapshot preservation, matching
materialized source, selected kernel/ISO artifacts, successful immutable build
and candidate provenance, ordered lifecycle evidence, clean gate-session
retirement, a generation-one selected-successor boot and abort, a generation-two
rollback boot from generation zero and abort, no live workspace, and explicit
proof that every recorded QEMU and Codex child PID has been reaped. The short run
root is intentional because Linux limits Unix-domain socket paths while the
candidate validator adds nested temporary directories. No network, production
QEMU, real Codex session, paid review, production telemetry, or live experiment
state is used.

## Operational risks

The highest risks are durable archive compatibility, fail-closed provenance,
and real-image behavior under the exact KVM-only experiment profile. The
terminal reader's unsafe
forced-cancellation path is avoided by application-owned graceful shutdown and
covered with a real pseudo-terminal under the race detector. Bubble Tea v2.0.9
can still use its upstream forced path after an internal renderer/input error or
panic; that residual dependency failure path should be removed by an upstream
upgrade or re-evaluated before cutover. No Go component should be used on live
experiment state before the remaining cutover verification and explicit human
approval.

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

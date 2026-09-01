# Go cutover readiness

Status: **not ready for cutover**. The Python harness remains fully operational
and is still the only operational entry point.

## Completed capabilities

The Go implementation has compatible pure codecs for serial frames, source
snapshots, host-service requests/responses, tool lists, tool invocation requests,
and tool results. It also reads and writes the compatible feature-request store,
including inherited sparse IDs and durable decisions. It does not yet own a
serial connection or experiment process, and gate-only decision enforcement
remains with the Python runtime. The synchronous QMP client implements Unix
socket retry, greeting and capability negotiation, status/control commands,
event skipping, deadlines, and cancellation, but no Go component starts or owns
a QEMU process yet. The two fixed hardware profiles, exact QEMU arguments,
strict archived manifest codec, KVM availability check, and bounded QEMU version
discovery are implemented. A direct-child QEMU controller owns log descriptors,
asynchronous reaping, and bounded TERM/KILL shutdown. Planning evidence
allocation, attempt history, exact private responses, and digest publication are
implemented, but are not yet wired to a Go Codex session.

## Verification performed

The current milestone is verified with `go test ./...`. Tests cover exact bytes,
round trips, malformed and oversized input, fragmentation, coalescing, and fuzz
properties. Black-box conformance tests compare wire bytes, feature records, and
planning-evidence trees against the Python modules and exercise Python-to-Go and
Go-to-Python feature loading. Synthetic Unix-socket peers verify exact QMP
requests, fragmented input, asynchronous events, protocol errors, connection
retry, and cancellation without starting QEMU. Hardware conformance compares
exact command arguments and archived manifest bytes with the Python module;
malformed manifest parsing also has a fuzz target. Synthetic subprocesses cover
normal exit, duplicate start, cancellation, restart, log failures, and forced
kill, while an optional disposable `-machine none` QEMU test exercises the real
controller/QMP boundary without KVM or a guest image.

## Remaining gaps and known differences

All process, persistence, lifecycle, Codex session, observability, CLI, operator,
and TUI capabilities remain to be implemented. Tool request-ID allocation and
response matching await the serial dispatcher. Go `ReadFrame` relies on its
transport owner for deadlines, while Python's public helper currently applies a
five-second deadline itself; the eventual Go serial transport must preserve the
observable timeout.

There are no intentional wire-format differences in the implemented codecs.
Feature IDs and generation numbers are represented as uint64 in Go; unlike
Python's unbounded integers, larger persisted values are rejected. Such values
cannot describe an operable CodexOS generation, but this remains a documented
validation difference. Planning thread/turn IDs and response text must also be
valid UTF-8 in Go; Python can represent lone Unicode surrogates in IDs, although
the real app-server protocol supplies valid Unicode strings. The Go QMP client
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

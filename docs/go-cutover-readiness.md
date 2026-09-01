# Go cutover readiness

Status: **not ready for cutover**. The Python harness remains fully operational
and is still the only operational entry point.

## Completed capabilities

The Go implementation has compatible pure codecs for serial frames, source
snapshots, host-service requests/responses, tool lists, tool invocation requests,
and tool results. It also reads and writes the compatible feature-request store,
including inherited sparse IDs and durable decisions. It does not yet own a
serial connection or experiment process, and gate-only decision enforcement
remains with the Python runtime. Planning evidence allocation, attempt history,
exact private responses, and digest publication are implemented, but are not yet
wired to a Go Codex session.

## Verification performed

The current milestone is verified with `go test ./...`. Tests cover exact bytes,
round trips, malformed and oversized input, fragmentation, coalescing, and fuzz
properties. Black-box conformance tests compare wire bytes, feature records, and
planning-evidence trees against the Python modules and exercise Python-to-Go and
Go-to-Python feature loading.

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
the real app-server protocol supplies valid Unicode strings.

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

# Go serial receive deadlines

While a tool exchange waits for its response, Go treats the five-second response
timeout as an **idle** budget. New bytes in a valid incoming frame prefix renew
it. Each incoming frame also has a fixed fifteen-minute limit, measured from
its first observed bytes; continued progress cannot renew that limit. This
allows a maximum-size 16 MiB frame at approximately 19 KiB/s. Cancellation can
end the exchange earlier.

A complete header must identify the outstanding tool request and matching
response type, or a nonzero guest-originated host-service request. Header
fragments count as progress only while consistent with framing magic; completing
the header validates version, length, ID, and type before granting more time.
Payload bytes remain opaque and are never scanned for framing magic.

The same receive budget covers a large nested host-service request, such as a
source snapshot. Once that frame is complete, existing host-service execution
and outgoing-write deadline behavior applies. Finishing the host response
starts a fresh receive idle budget. This is a per-frame transfer bound, not an
overall wall-clock bound on a tool that invokes host services.

Timeout and cancellation permanently fail the dispatcher and close serial.
Later requests return the stored failure without sending another request.
Malformed or mismatched responses also permanently fail the dispatcher. There
is no retry, failure-reset, reconnect, mutation replay, or live recovery path.
Timer wakeups recheck progress, completion, and host activity under the receive
lock before declaring a timeout.

Receive events record header identity, advertised wire size, and observed wire
bytes at the first complete header and then at 64 KiB intervals. Timeout events
record the outstanding request ID, partial wire byte count, and reason. These
records contain no payload. Python and the wire format are unchanged.

## Experiment-004, generation 11: 2026-09-05

Read-only inspection identified live harness PID 428257, QEMU PID 428427, and
app-server PID 428440. The recorded clean harness revision is
`3072891409599a45f4abbd2cece2e29f029a8be1`. Its dispatcher and tool-client sources
are unchanged from `7fb8184`. The executable copied through `/proc/428257/exe`
has SHA-256 `22bbbcc9a56e3b0a7e1e8ec6d9e91b6c1c22c1c2d4704093b9f078026450503f`.

The UTC event sequence is:

| Time | Evidence |
| --- | --- |
| 14:04:44.934677 | Build `build-000003` became the latest validated candidate. |
| 14:05:26.613446 | A subsequent bootstrap job completed successfully, after three additional host-test source writes. |
| 14:05:46.780016 / 14:05:46.957897 | Writes to `seed/user/inspect.c.inc` and `seed/hosttests/inspect-test.c.inc` succeeded; these final edits were not validated afterward. |
| 14:06:08.456997 | Import of the 715,493-byte supplied asset into `assets/DOOM.EXE` succeeded. |
| 14:06:22.792527 | The read request, serial ID 73, finished sending. |
| 14:06:27.792688 | The invocation failed, five seconds later. App-server logs contain `Bridge error: timed out waiting for tool response`. |
| 14:06:37.630661 onward | An 8,192-byte retry and later calls failed immediately, with no further serial request writes. |

The harness no longer had a serial connection; its QMP connection and the
original QEMU remained present. The old dispatcher buffers partial frames
without extending the response deadline, then closes serial and retains its
terminal error. These observations establish a bridge failure, not a guest hang.
Old logs do not reveal the original partial-response byte count or the guest's
execution position after disconnection.

Evidence was copied outside the repository to
`/shared/codexos-forensics/experiment-004-generation-0011-uj2mqap7/`:
event log, QEMU logs, generation/build metadata, running executable, process
identity, app-server SQLite files and WALs, copied boot ISO, and the last
validated source snapshot. SQLite inspection used separate copies; the live
databases were not opened by the investigation. Source-file metadata was stable
during each database-file copy; this is not an atomic multi-file RAM snapshot.

An offline reconstruction in `reconstructed-source/` contains 62 files and
174,626 content bytes. It combines the validated snapshot with the acknowledged
post-build writes, including both final edits. Logged literals were decoded
without executing logged code, and final argument sizes match the event log.
The accompanying manifest records file hashes and derivation. This copy has not
been compared with live RAM or build-validated; it does not capture other mutable
guest files or execution state.

## Disposable reproduction

A separate QEMU booted a copy of the same ISO with one vCPU, 1 GiB RAM, KVM,
network disabled, and a private serial socket. No Codex session was started.
The test wrote a synthetic 715,493-byte binary file in bounded chunks and read
it back through the Go tool client. Its payload includes framing magic bytes.

* Before the fix: read failed after 5.010 seconds with **304,877 incoming wire
  bytes already observed**. This demonstrates a progressing transfer killed by
  the response deadline.
* After the fix: read completed in 13.396 seconds with **715,513 wire bytes**
  (16-byte header, 4-byte status, 715,493 output bytes), exact output equality,
  and a successful subsequent list-tools request.

Both disposable QEMUs were reaped. The temporary reproduction test and its
output are retained with the forensic evidence, outside the repository.
Committed regressions use disposable serial peers for slow large responses,
fragmented headers, large nested requests, silence, truncated payloads,
cancellation, a continuously progressing transfer that reaches the absolute
limit, and rejection of stale headers before their payload arrives.

The full Go suite, full race suite, and `go vet -p=1 ./...` passed serially with
`GOMAXPROCS=2`, `GOMEMLIMIT=768MiB`, and test flags `-p=1 -parallel=1 -count=1`.
Ordinary and race package timeouts were 300 and 420 seconds respectively. The
canonical seed rebuild acceptance was not run; the real-image reproduction used
the copied generation ISO. The final process audit found only the original
QEMU, no remaining temporary test executables, and unchanged process start times
for the live harness, QEMU, and app-server. The live event log size was unchanged.

## Recovery choices

The original harness, QEMU, app-server, and live guest source were left intact.
No live serial/QMP command, restart, replacement, failure reset, or mutation
replay was performed.

1. **Preserve the live processes while preparing a separate recovery procedure.**
   A trustworthy protocol boundary and a way to retain guest state must be
   established before using a new serial client. The failed dispatcher has no
   supported reattachment path. A QMP or memory-based salvage procedure would
   require separate planning and intervention; it was not tested here.
2. **Use the offline source reconstruction as a recovery input.** It preserves
   the recorded source changes but requires independent validation and explicit
   intervention provenance. It cannot restore the running generation's complete
   memory or execution state.
3. **Restart from the last validated candidate only after an explicit decision
   to discard later state.** That candidate predates the final source edits,
   additional host-test files, and imported asset. No restart is authorized by
   this investigation or fix.

Reconnecting blindly is unsafe: an unknown response suffix may remain, and the
binary payload can contain `CXOS`. The wire protocol has no delimiter, checksum,
or resynchronization handshake. Clearing the stored error cannot recover the
closed transport or prove that a new request starts at a valid frame boundary.

# Go bridge for guest task and immutable asset tools

The bridge recognizes `run`, `reap` and `import_provided_asset` only when each is
advertised by the current guest. Discovery remains the existing intersection with
the harness registry, frozen for one fresh session. All three tools are available
only during implementation: launching, importing and consuming a completed task
result mutate guest state. Planning, review and exit interviews cannot invoke them.

Contracts below were inspected in Experiment 4 Generation 10's `seed/tools.c`,
`seed/tasks.c`, `seed/files.c`, `seed/build.c` and `seed/USER_ABI.md`. These are guest
helper bindings, not new host services.

| Tool | JSON arguments | Guest wire arguments | Guest status 0 output |
| --- | --- | --- | --- |
| `run` | `{"path":"ram/program.cxe"}` | `path` | Decimal task slot ID |
| `reap` | `{"task_id":1}` | Unsigned decimal `task_id` | `running`, or decimal unsigned 64-bit exit status |
| `import_provided_asset` | `{"id":"asset-id","path":"ram/asset"}` | `id`, then `path` | Empty output |

Schemas reject missing/extra arguments and incorrect types. Paths are 1–255
encoded bytes of valid UTF-8. They retain the guest's length-delimited semantics,
including embedded NUL, relative components and leading slashes; the harness does
not normalize them or interpret them as host paths. The asset ID must be nonempty
UTF-8. The guest matches it against the supplied asset list; the bridge does not
apply the bootstrap-artifact ID's separate 255-byte/no-NUL restrictions. Existing
tool-invocation payload admission still applies.

`task_id` is an integer in `[0,4294967295]`, forwarded as decimal. Generation 10's
wire parser accepts decimal digit spans, including leading zeroes. JSON callers
use an integer, not a decimal string. The guest determines whether the slot is
valid, occupied and unreserved; slot zero and other unusable IDs produce guest
tool failures. IDs can be reused after consumption and are not durable handles.

## Guest execution and result semantics

`run` loads the named RAM file using the guest's existing CXE1/CXE2 loaders. It
returns a task ID without waiting for completion. It provides no arguments,
environment, host execution or new executable compatibility.

`reap` reports unreserved runnable, sleeping or waiting tasks as `running` without
consuming them. For a completed task, it consumes the result and returns the exit
status as decimal text, including zero and values above the signed 64-bit range.
A guest fault has exit status `18446744073709551615`; that remains a successful
reap with tool status 0. A free, invalid or reserved target produces a nonzero
tool status instead. Reaping a consumed ID fails unless the slot has been reused.

The bridge retains the response's status and output bytes. It does not parse exit
statuses into JSON numbers or convert a nonzero exit status into tool failure.
The app-server response and ordinary delivery-confirmation lifecycle remain
separate from the guest tool status; a delivered guest failure is still a delivered
result and produces failure activity. Transport/bridge errors retain their normal
unsuccessful response behavior.

## Guest immutable import

Generation 10 lists supplied assets, selects the exact asset ID and advertised
size, then reads the entire asset in chunks up to 1 MiB. It accepts at most 64 MiB,
requires a new destination, removes a partial file on read/write failure, and seals
the completed RAM file immutable. Zero-sized assets are created and sealed without
a range read. Existing destinations fail. Loading, copying, failure cleanup and
sealing are entirely guest-owned; the existing host services only list and read
frozen asset bytes. Import neither extracts nor launches the result. Ordinary guest
RAM/source-persistence rules still apply.

These bindings do not approve request #6, provision assets or execution services,
change request statuses, or start a generation.

## Disposable real-guest exercise

The opt-in Go test copies a selected ISO to temporary storage and boots one
`test-v1` QEMU VM (one vCPU, 128 MiB, no network or writable block devices). It
uses the production serial dispatcher, tool client, bridge forwarding and frozen
provided-asset service with temporary binary fixtures. It opens no experiment run
and no Codex session. Guest RAM paths used by the exercise are outside `seed/`.
The original ISO is checked unchanged, and QEMU is stopped and reaped on exit.

```sh
CODEXOS_GUEST_TASK_TOOL_ISO=/path/to/archived/codexos.iso \
GOMAXPROCS=2 GOMEMLIMIT=768MiB \
go test -v -p=1 -parallel=1 ./internal/agent \
  -run '^TestGuestTaskToolsRealGuest$' -count=1 -timeout=120s
```

This executes real guest imports, immutable-file rejection, loading, sleeping,
reaping, faults and full-width exit statuses. It is separate from the simulated
app-server tests that verify result delivery and phase policies. It does not
validate a workload such as Doom, arbitrary executable formats, or the full
experiment hardware profile.

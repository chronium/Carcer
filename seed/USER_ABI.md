# CodexOS user ABI

## CXE1 executable files

A CXE1 file is an ordinary file-store object. Integers are unsigned
little-endian values. Its 16-byte header is:

- bytes 0..3: ASCII `CXE1`
- bytes 4..7: image byte count
- bytes 8..11: entry offset within the image
- bytes 12..15: flags, currently zero

The header is followed by exactly the image byte count. The generic
`task_create_user_file(path,length)` loader validates the whole file, copies
the image into a private address space, and starts at image base plus entry
offset. Thus the backing file may be changed or removed after a successful
load without changing the running task.

A CXE1 image is a nonempty flat byte image based at `0x400000`. It may span
up to 511 pages (2,093,056 bytes), and its entry offset must identify a byte
inside the image. All image pages are currently readable, writable, and
executable. Zero-filled tail bytes in the final image page are available to
the program. A separate zeroed one-page stack ends at `0x600000`. These are
current implementation limits, not workload semantics.

Each task's address-space allocation is sized from its image page count and
is privately owned. Exit or destruction reclaims the dynamically sized
allocation only after execution has switched to another address space.

## System calls

Ring-3 code executes `int 0x80` with the call number in RAX and arguments in
the normal integer registers. Call 0 is `exit(status)`, with status in RDI;
it does not return. Unknown calls return `UINT64_MAX` in RAX. Timer
interrupts preempt user code, so CPU-bound tasks need not yield or make system
calls for other runnable tasks to progress.

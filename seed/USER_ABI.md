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

Current CXE1 limits are one nonempty image page (at most 4096 bytes), entry
inside the image, image base `0x400000`, and a zeroed one-page stack ending at
`0x600000`. These are current implementation limits, not workload semantics.

## System calls

Ring-3 code executes `int 0x80` with the call number in RAX and arguments in
the normal integer registers. Call 0 is `exit(status)`, with status in RDI;
it does not return. Unknown calls return `UINT64_MAX` in RAX. Timer
interrupts preempt user code, so CPU-bound tasks need not yield or make system
calls for other runnable tasks to progress.

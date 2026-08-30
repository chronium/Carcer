# CodexOS user ABI

## CXE1 executables

A CXE1 executable is an ordinary file-store object. Integers are unsigned
little-endian. Its 16-byte header contains:

- 0..3: ASCII `CXE1`
- 4..7: image byte count
- 8..11: entry offset in the image
- 12..15: flags (must be zero)

Exactly `image byte count` bytes follow the header. The generic
`task_create_user_file(path,length)` loader validates and privately copies
the entire image before starting it, so later backing-file changes do not
change the running task.

The nonempty flat image is based at `0x400000`, spans 1..511 pages (at most
2,093,056 bytes), and has an entry offset inside the image. Its pages are
currently RWX; the zeroed tail of the last page is usable. A separate zeroed
RWX stack page occupies `0x5ff000..0x600000`. These are implementation
limits, not workload semantics. Each task owns a dynamically sized allocation
of page tables, image pages, and stack. Exit/destruction reclaims it only after
switching to another address space.

## System calls

Ring-3 code executes `int 0x80` with the call number in RAX. Arguments use
RDI, RSI, RDX, RCX, R8 in that order. Success is returned in RAX. Failure and
unknown calls return `UINT64_MAX`.

- 0: `exit(status)`; RDI=status. It does not return.
- 1: `file_size(path,path_length)`; RDI=user path address, RSI=length.
  Returns the file byte count.
- 2: `file_read(path,path_length,offset,destination,count)`; RDI=user path
  address, RSI=length, RDX=offset, RCX=user destination, R8=count. Returns the
  bytes copied, capped at EOF. Offset past EOF fails; offset at EOF returns
  zero.

Paths are explicit nonempty byte spans of at most 255 bytes; no terminator is
required. Path and destination spans must be wholly inside the caller's mapped
image or stack. Spans may cross mapped image-page boundaries. Overflow,
unmapped holes, crossing the image end or stack top, and invalid paths fail
without copying file data. A zero-byte read does not dereference destination.

The PIT timer preempts user code. Two independent CPU-bound ring-3 workloads
that never yield, block, or enter the kernel are part of the READY gate and
both must make progress.

# CodexOS user ABI

## CXE1 executables

A CXE1 executable is an ordinary file-store object. Its 16-byte header contains
little-endian fields: bytes 0..3 are `CXE1`, 4..7 are image size, 8..11 are
entry offset, and 12..15 are flags (currently zero). Exactly image-size bytes
follow. `task_create_user_file(path,length)` validates and privately copies
the full image, so backing-file changes cannot affect a running task.

The nonempty flat image is based at `0x400000`, spans 1..511 pages (at most
2,093,056 bytes), and has an entry inside the image. Its pages and the zeroed
tail of its last page are RWX. A separate zeroed RWX stack page is at
`0x5ff000..0x600000`. Each task owns dynamically allocated page tables, image, and stack;
exit/destruction reclaims them after switching address spaces.

## System calls

Ring-3 uses `int 0x80`; RAX is the call number, arguments are RDI, RSI, RDX,
RCX, R8, and RAX is the result. Failure/unknown calls return `UINT64_MAX`.

- 0: `exit(status)`; does not return.
- 1: `file_size(path,path_length)`.
- 2: `file_read(path,path_length,offset,destination,count)`; returns bytes
  copied, capped at EOF. Offset past EOF fails; offset at EOF returns zero.
- 3: `file_attributes(path,path_length)`; bit 0 means immutable.

Paths are explicit 1..255-byte spans without required terminators. User spans
must be wholly within the caller's mapped image or stack and may cross image
pages. Overflow, holes, image-end/stack-top crossing, and invalid paths fail
without copying file data. A zero-byte read does not inspect destination.

The kernel can irreversibly seal any ordinary file, Once immutable, every write (including zero-byte), truncation
(including no-op), and removal fails; reads continue.
READY seals a data file, verifies all mutation routes preserve its contents,
and has ring-3 query the attribute.

The PIT preempts user code. READY requires two independent CPU-bound ring-3
tasks that never yield, block, or enter the kernel to both make progress.

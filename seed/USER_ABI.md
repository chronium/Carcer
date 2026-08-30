# CodexOS user ABI

CXE1 is an ordinary file: a 16-byte little-endian header containing `CXE1`,
image size, entry offset, and zero flags, then exactly the image. Loading
privately copies it. The nonempty flat RWX image starts at `0x400000`, is at
most 511 pages, and has an in-image entry. A zeroed RWX stack page is at
`0x5ff000..0x600000`. Exit/destruction reclaims all task-owned pages.

Ring 3 invokes `int 0x80`; RAX is the call, arguments are RDI, RSI, RDX, RCX,
R8, and failure/unknown calls return `UINT64_MAX`.

- 0: `exit(status)`, no return.
- 1: `file_size(path,path_length)`.
- 2: `file_read(path,path_length,offset,destination,count)`; returns bytes
  through EOF, while offset past EOF fails.
- 3: `file_attributes(path,path_length)`; bit 0 is immutable.
- 4: `file_write(path,path_length,offset,source,count)`; returns count.
  Offset zero can create a file; writes cannot create holes.

Paths are explicit 1..255-byte spans. Data spans must stay wholly in the
caller's image or stack and may cross image pages; overflow, holes, and
boundary crossings fail. Zero-length I/O ignores its data pointer.

Kernel sealing is irreversible: all writes (including zero-length),
truncations (including no-op), and removal fail thereafter; reads continue.
READY tests sealing from kernel and ring 3, mutable backing-file I/O, private
loading, exact page reclamation/reuse, invalid spans, and unknown calls.

The PIT preempts user code. READY requires two independent CPU-bound ring-3
tasks that never yield, block, or enter the kernel to both make progress.

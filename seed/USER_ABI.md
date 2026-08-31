# CodexOS user ABI

CXE1 files have a 16-byte little-endian header: `CXE1`, image size,
entry offset, and zero flags, followed by the exact image. Loading privately
copies a nonempty, at-most-511-page RWX image to `0x400000`; its entry is
in-image. A zeroed RWX stack page ends at `0x600000`. Exit reclaims all
task pages.

Ring 3 uses `int 0x80`: RAX is the call; arguments are RDI, RSI, RDX, RCX,
R8; failure or an unknown call returns `UINT64_MAX`.

- 0: `exit(status)`, no return.
- 1: `file_size(path,path_length)`.
- 2: `file_read(path,path_length,offset,destination,count)`.
- 3: `file_attributes(path,path_length)`; bit 0 means immutable.
- 4: `file_write(path,path_length,offset,source,count)`.

Paths are explicit 1..255-byte spans. Data must stay wholly in the image or
stack and may cross image pages. Overflow, holes, and boundary crossings
fail. Reads return through EOF; offset past EOF fails. Offset-zero writes
may create files. Zero-length I/O ignores its data pointer.

Sealing is irreversible: writes, truncations, and removal all fail, including
zero-length/no-op mutations. The PIT preempts user code. READY validates
generic file/CXE behavior, page reclamation/reuse, invalid spans, and that
two independent non-yielding CPU-bound ring-3 tasks both progress.
rom kernel and ring 3, mutable backing-file I/O, private
loading, exact page reclamation/reuse, invalid spans, and unknown calls.

The PIT preempts user code. READY requires two independent CPU-bound ring-3
tasks that never yield, block, or enter the kernel to both make progress.

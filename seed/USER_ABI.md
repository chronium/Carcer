# CodexOS user ABI
CXE1 (legacy): `CXE1`, LE32 image size, LE32 entry offset, zero LE32 flags, then <=511 image pages. The fixed 0x400000 image is RWX for compatibility.

CXE2: a 24-byte header (`CXE2`, LE32 segment count, LE64 entry, zero LE64 reserved) followed by 1..16 32-byte segment records: LE64 virtual address, LE64 memory size, LE32 file offset, LE32 file size, LE32 flags, zero LE32 reserved. Flag bits are R=1, W=2, X=4. R is required and W+X is rejected. Virtual addresses and memory sizes are page aligned; file size is <= memory size; segments cannot overlap. The entry must be file-backed in an executable segment. At most 16,384 pages may be loaded in [0x400000,0x3ffef000). File tails are zero-filled. Text can be RX; data and BSS are RW+NX.

Each task has a private address space, guarded 64 KiB RW+NX stack ending at 0x40000000, and page-aligned break after its image. `brk` growth is RW+NX. PIT scheduling preempts ring 3 and immutable-file copy windows; faults exit only that task.

`int 0x80`: RAX call; RDI, RSI, RDX, RCX, R8, R9 arguments; UINT64_MAX failure.
0 exit; 1 file size; 2 file read; 3 attributes (immutable bit 0); 4 file write;
5 spawn; 6 reap (0 active, 1 consumed); 7 brk; 8 monotonic ticks since boot (100 Hz); 9 display info; 10 display present; 11 sleep.

Sleep uses RDI=relative 100 Hz ticks. Zero returns immediately; a deadline overflow fails. A valid nonzero sleep blocks and later resumes with RAX=0. Reap reports runnable and sleeping tasks as active. Paths are 1..255-byte UTF-8 spans; buffers may cross pages. Protocol run and syscall spawn share both loaders. Exits and faults become zombies and reserve slots until reap.

Display info uses RDI=output and RSI=capacity. It fails for capacity<32 or an invalid destination; otherwise it writes and returns 32 bytes: LE32 size=32,width,height,pitch,format=1,zero[3]. Format 1 is XRGB8888. Present uses RDI source, RSI stride, RDX x, RCX y, R8 width, R9 height; rectangles are nonempty, in bounds, and prevalidated. The framebuffer is kernel-owned.

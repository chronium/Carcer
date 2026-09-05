# CodexOS user ABI
CXE1 (legacy): `CXE1`, LE32 image size, LE32 entry offset, zero LE32 flags, then <=511 image pages. The fixed 0x400000 image is RWX for compatibility.

CXE2: a 24-byte header (`CXE2`, LE32 segment count, LE64 entry, zero LE64 reserved) followed by 1..16 32-byte segment records: LE64 virtual address, LE64 memory size, LE32 file offset, LE32 file size, LE32 flags, zero LE32 reserved. Flag bits are R=1, W=2, X=4. R is required and W+X is rejected. Virtual addresses and memory sizes are page aligned; file size is <= memory size; segments cannot overlap. The entry must be file-backed in an executable segment. At most 16,384 pages may be loaded in [0x400000,0x3ffef000). File tails are zero-filled. Text can be RX; data and BSS are RW+NX.

Each task has a private address space, guarded 64 KiB RW+NX stack ending at 0x40000000, and page-aligned break after its image. `brk` growth is RW+NX. PIT scheduling preempts ring 3 and immutable-file copy windows; faults exit only that task.

`int 0x80`: RAX call; RDI, RSI, RDX, RCX, R8, R9 arguments; UINT64_MAX failure.
0 exit; 1 file size; 2 file read; 3 attributes (immutable bit 0); 4 file write;
5 spawn; 6 reap (0 active, 1 consumed); 7 brk; 8 monotonic ticks since boot (100 Hz); 9 display info; 10 display present; 11 sleep; 12 blocking wait.

Sleep uses RDI=relative 100 Hz ticks. Zero returns immediately; a deadline overflow fails. A valid nonzero sleep blocks and later resumes with RAX=0. Reap reports runnable, sleeping, and waiting tasks as active unless their result is reserved by a blocking waiter. Paths are 1..255-byte UTF-8 spans; buffers may cross pages. Protocol run and syscall spawn share both loaders. Exits and faults become zombies and reserve slots until reap.

Blocking wait (12) uses RDI=target task ID and RSI=an 8-byte writable status destination. On normal completion it writes the target's 64-bit exit status and returns RAX=1, consuming the result; a fault has status UINT64_MAX. An already completed, unreserved target returns immediately. Otherwise the caller stops being runnable until completion or cancellation. No polling or voluntary yielding by the target is required. All registers except RAX, including the stack pointer and supported FP state, are preserved.

Only one blocking waiter may reserve a target's result. Another blocking wait or nonblocking reap (6), including a development-protocol reap, fails with UINT64_MAX / tool failure while that reservation exists. Nonblocking reap returns 0 for an unreserved waiting task, just as for runnable or sleeping tasks. Wait rejects an invalid/free/kernel target, self-wait, a dependency cycle, or an invalid destination before reserving or consuming anything. Failure leaves the destination unchanged and returns UINT64_MAX.

Kernel-side cancellation of the target wakes its waiter with failure, without writing a status. Cancellation of a blocked waiter releases its reservation. Completion delivery and cancellation finish before a target slot can be reused; a pending wait cannot silently transfer to a replacement task. Task IDs remain reusable slot indices, however: an old ID submitted *after* reuse names the current occupant. There is no ownership model or user cancellation syscall. Waiting on an indefinitely running task can block indefinitely; there is no wait timeout.

Display info uses RDI=output and RSI=capacity. It fails for capacity<32 or an invalid destination; otherwise it writes and returns 32 bytes: LE32 size=32,width,height,pitch,format=1,zero[3]. Format 1 is XRGB8888. Present uses RDI source, RSI stride, RDX x, RCX y, R8 width, R9 height; rectangles are nonempty, in bounds, and prevalidated. The framebuffer is kernel-owned.

CPU baseline: x87/MMX and SSE/SSE2. FXSAVE64/FXRSTOR64 preserve numerical registers, FCW/FSW, MXCSR and all 16 XMM registers across preemption, syscalls and sleep. New tasks have FCW=0x037f, empty x87 tags, MXCSR=0x1f80 and zero payloads. FP exceptions terminate the offending task.

CR0 enables native floating-point exceptions and clears EM/TS; CR4 enables OSFXSR/OSXMMEXCPT. OSXSAVE, FSGSBASE, PKE, CET, PKS and UINTR are disabled. AVX and other XSAVE-managed extensions are outside this ABI; hardware CPUID feature bits alone do not establish OS support. No TLS base ABI exists.

Kernel C uses general-regs-only; CPU setup precedes other initialization. Each entry saves its own FP image, loads a clean environment before C, and restores the selected context after C. Nested kernel/user continuations retain separate images. Every restore first sanitizes x87 metadata via fnclex/emms/fildl of a fixed kernel constant. On affected AMD CPUs, nonexception FIP/FDP/FOP may reflect this sanitation site instead of original pointers. This prevents prior-context metadata leakage; affected-AMD hardware has not been separately validated.

Freestanding C/assembly development: see sdk/README.md and sdk/cx.h. The guest-owned
SDK uses the approved host bootstrap executor to build static CXE2 files and
provides startup, all current syscall wrappers and a small runtime. This adds no
new kernel syscall or user launch convention; argc/argv, TLS and full libc remain
absent. Exact separately compiled fixtures under seed/user/ run in boot regression
through the same loader as ordinary spawn/run.

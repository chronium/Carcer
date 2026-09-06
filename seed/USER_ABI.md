# CodexOS user ABI
CXE1 (legacy): `CXE1`, LE32 image size, LE32 entry offset, zero LE32 flags, then <=511 image pages. The fixed 0x400000 image is RWX for compatibility.

CXE2: a 24-byte header (`CXE2`, LE32 segment count, LE64 entry, zero LE64 reserved) followed by 1..16 32-byte segment records: LE64 virtual address, LE64 memory size, LE32 file offset, LE32 file size, LE32 flags, zero LE32 reserved. Flag bits are R=1, W=2, X=4. R is required and W+X is rejected. Virtual addresses and memory sizes are page aligned; file size is <= memory size; segments cannot overlap. The entry must be file-backed in an executable segment. At most 16,384 pages may be loaded in [0x400000,0x3ffef000). File tails are zero-filled. Text can be RX; data and BSS are RW+NX.

Each task has a private address space, guarded 64 KiB RW+NX stack ending at 0x40000000, and page-aligned break after its image. `brk` growth is RW+NX. PIT scheduling preempts ring 3 and immutable-file copy windows; faults exit only that task.

`int 0x80`: RAX call; RDI, RSI, RDX, RCX, R8, R9 arguments; UINT64_MAX failure.
0 exit; 1 file size; 2 file read; 3 attributes (immutable bit 0); 4 file write;
5 spawn; 6 reap (0 active, 1 consumed); 7 brk; 8 monotonic ticks since boot (100 Hz); 9 display info; 10 display present; 11 sleep; 12 blocking wait; 13 spawn with arguments; 14 file control; 15 keyboard history; 16 namespace snapshot; 17 supervised spawn with arguments; 18 supervised child control; 19 open file; 20 file handle operations.

Sleep uses RDI=relative 100 Hz ticks. Zero returns immediately; a deadline overflow fails. A valid nonzero sleep blocks and later resumes with RAX=0. Reap reports runnable, sleeping, and waiting tasks as active unless their result is reserved by a blocking waiter. Paths are 1..255-byte UTF-8 spans; buffers may cross pages. Protocol run and syscall spawn share both loaders. Exits and faults become zombies and reserve slots until reap.

Blocking wait (12) uses RDI=target task ID and RSI=an 8-byte writable status destination. On normal completion it writes the target's 64-bit exit status and returns RAX=1, consuming the result; a fault has status UINT64_MAX. An already completed, unreserved target returns immediately. Otherwise the caller stops being runnable until completion or cancellation. No polling or voluntary yielding by the target is required. All registers except RAX, including the stack pointer and supported FP state, are preserved.

Only one blocking waiter may reserve a target's result. Another blocking wait or nonblocking reap (6), including a development-protocol reap, fails with UINT64_MAX / tool failure while that reservation exists. Nonblocking reap returns 0 for an unreserved waiting task, just as for runnable or sleeping tasks. Wait rejects an invalid/free/kernel target, self-wait, a dependency cycle, or an invalid destination before reserving or consuming anything. Failure leaves the destination unchanged and returns UINT64_MAX.

Kernel-side cancellation of the target wakes its waiter with failure, without writing a status. Cancellation of a blocked waiter releases its reservation. Completion delivery and cancellation finish before a target slot can be reused; a pending wait cannot silently transfer to a replacement task. Task IDs remain reusable slot indices, however: an old ID submitted *after* reuse names the current occupant. Legacy tasks have no ownership or user cancellation; supervised children use calls17/18 below. Waiting on an indefinitely running task can block indefinitely; there is no wait timeout.

Display info uses RDI=output and RSI=capacity. It fails for capacity<32 or an invalid destination; otherwise it writes and returns 32 bytes: LE32 size=32,width,height,pitch,format=1,zero[3]. Format 1 is XRGB8888. Present uses RDI source, RSI stride, RDX x, RCX y, R8 width, R9 height; rectangles are nonempty, in bounds, and prevalidated. The framebuffer is kernel-owned.

CPU baseline: x87/MMX and SSE/SSE2. FXSAVE64/FXRSTOR64 preserve numerical registers, FCW/FSW, MXCSR and all 16 XMM registers across preemption, syscalls and sleep. New tasks have FCW=0x037f, empty x87 tags, MXCSR=0x1f80 and zero payloads. FP exceptions terminate the offending task.

CR0 enables native floating-point exceptions and clears EM/TS; CR4 enables OSFXSR/OSXMMEXCPT. OSXSAVE, FSGSBASE, PKE, CET, PKS and UINTR are disabled. AVX and other XSAVE-managed extensions are outside this ABI; hardware CPUID feature bits alone do not establish OS support. No TLS base ABI exists.

Kernel C uses general-regs-only; CPU setup precedes other initialization. Each entry saves its own FP image, loads a clean environment before C, and restores the selected context after C. Nested kernel/user continuations retain separate images. Every restore first sanitizes x87 metadata via fnclex/emms/fildl of a fixed kernel constant. On affected AMD CPUs, nonexception FIP/FDP/FOP may reflect this sanitation site instead of original pointers. This prevents prior-context metadata leakage; affected-AMD hardware has not been separately validated.

Freestanding C/assembly development: see sdk/README.md and sdk/cx.h. The guest-owned
SDK uses the approved host bootstrap executor to build static CXE2 files and
provides startup, all current syscall wrappers and a small runtime. This adds no
TLS or full libc support. Syscall 13 adds argument launch as documented below.
Exact separately compiled fixtures under seed/user/ run in boot regression
through the same loader as ordinary spawn/run.

## Argument launch (13)

RDI=executable path address, RSI=path byte length, RDX=argument-span vector,
RCX=argument count. Each span is two little-endian uint64 fields: address, length.
Return is a reusable task slot ID or UINT64_MAX. No implicit argv[0] is added.
Count is 0..32. Total string bytes PLUS one terminator per argument is <=4096.
Empty strings are permitted and their addresses are ignored. Nonempty strings
must be readable and contain no NUL; bytes need not be UTF-8. Paths retain
their existing UTF-8/path rules. With count zero the vector address is ignored.
Invalid vectors, length/range overflow, bad strings, loader/resource errors fail
without publishing a runnable child or consuming a slot. GPRs except RAX and
the supported FP state follow the existing syscall preservation rules.

The kernel snapshots all spans/strings under the scheduling CPU's interrupt lock
before loading. On success it writes independent NUL-terminated strings and a
NULL-terminated uint64 pointer vector into the child's private RW/NX stack.
At entry RDI=argc, RSI=argv, RSP is 16-byte aligned and equals argv. Strings and
vector are at/above initial RSP; the C call stack grows below them. The maximum
argument payload/vector uses 4368 of the 65536 mapped stack bytes, including
alignment. Arguments are writable by the child without modifying the parent.
All image permissions, guards, heap bounds and loader rules remain unchanged.

Legacy syscall 5, protocol run(path), and raw tuser launches retain RDI=RSI=0
and RSP=0x40000000. An SDK main(int argc,char **argv) sees argc=0, argv=NULL
on legacy launch. In contrast, argument launch with zero arguments supplies a
valid vector containing one NULL. Both main(void) and main(int,char **) are
supported by SDK startup; no environment, TLS or shell syntax is implied.
See LAUNCH.md for an ordinary userland file-driven launcher and checksum tool.

## File control (14)

RDI=path address, RSI=path byte length, RDX=operation. Return0 on success or
UINT64_MAX on failure. Paths follow the existing byte-span UTF-8 rules.

- Operation0: create a new empty mutable file exclusively; RCX=R8=0.
  Existing paths (including sealed ones) fail without changing contents.
- Operation1: resize an existing mutable file to RCX bytes (0..UINT32_MAX);
  R8=0. Growth is zero-filled. It does not create missing files.
- Operation2: remove an existing mutable file; RCX=R8=0.
- Operation3: rename source to destination span at RCX with length R8.
  Both paths are snapshotted before mutation. An existing mutable destination
  is replaced; a sealed source OR destination fails. Renaming a mutable path
  to itself succeeds. Rename requires no spare namespace slot or data allocation.

Unused R9 is ignored. Unsupported operations/reserved operand combinations,
bad spans, missing sources, immutable endpoints and resource failure return
failure. Each namespace operation is serialized with interrupts disabled.
No ownership, stable open handles, directory hierarchy or disk persistence is
implied. Existing path-backed stream readers follow their path after rename.

## Keyboard history (15)

See INPUT.md for the exact24-byte record, cursor, overflow and availability
contract. RSI=0 queries availability and ignores other operands. Otherwise
RDI=writable event array, RSI=capacity1..64, RDX=readable/writable uint64 cursor.
The event and cursor ranges must not overlap. Failure changes neither range;
successful empty reads preserve event bytes and update the cursor. Each caller
owns its cursor; reading does not consume events for another caller.

## Namespace snapshot (16)

RDI=destination array of272-byte records; RSI=capacity in records.
RSI=0 returns the current file count and ignores the destination. Otherwise
capacity must be1..128 and fit the entire current namespace. Returns record
count or UINT64_MAX. Other registers are ignored. A query followed by a read is
not a transaction; capacity128 accommodates the current fixed namespace limit.

Each record: LE32 size,LE32 attributes,LE16 path length,LE16 zero,LE32 zero,
256 path bytes. Path length is1..255; unused path bytes are zero, including
path[length]. Paths are exact UTF-8 byte spans and may contain embedded NUL.
Attributes bit0 is immutable. Records use unsigned-byte lexical path order.
The snapshot has no kernel pointers or stable file identity/handle.

The entire written span (count*272 bytes) is prevalidated writable, including
cross-page boundaries, before any record is written. Failure writes nothing;
spare capacity remains untouched and is not validated. A zero-file snapshot
writes nothing. The snapshot copies names/metadata atomically under the existing
single scheduling CPU's syscall interrupt lock. Contents and future namespace
operations are not part of the snapshot. No directory tree or access-control
model is added. sdk/cx.h defines cx_file_record and cx_files.

## Supervised children (17 and 18)

Call17 has exactly call13's path and argument-span inputs and validation, but
returns an opaque nonzero64-bit child handle instead of a reusable slot index.
UINT64_MAX is failure and is never a handle. Handles identify a single successful
supervised launch, increase monotonically and never wrap/recur during a boot.
Exhaustion rejects further supervised launches. Failed launches publish no child
and consume no handle. Handles have no meaning across reboot/generation changes.

The calling task is the sole owner of that child, even if the owner itself was
launched through a legacy API. Passing a handle to another task does not transfer
control. Call18 and all cleanup are serialized with launch/completion on the
single scheduling CPU. No handle can accidentally resolve to a replacement task.
Legacy calls6/12 and protocol reap reject supervised children even if supplied
their raw slot index. Existing unsupervised spawn/wait/reap contracts remain.

Call18: RDI=handle, RSI=operation (0 poll,1 blocking wait,2 stop),
RDX=8-byte writable status destination. RCX/R8/R9 are ignored.
The full destination span is validated before any side effect, including poll
of an active child. Unaligned and cross-page writable words are supported.

- Return0: poll found an active child. Status is unchanged.
- Return1: the child completed normally or faulted; its full64-bit exit status
  is written and the result/handle is consumed. This applies to all operations,
  including stop of an already completed child. A fault still uses UINT64_MAX;
  it is not distinguishable from explicit exit with that status.
- Return2: stop terminated an active child and consumed its handle. Status is
  set to UINT64_MAX. The return value distinguishes termination from completion.
- UINT64_MAX: invalid operation, handle, owner, destination, reservation or wait
  dependency. Status is unchanged; no result is consumed and no child is stopped.

Blocking wait retains all GPRs except RAX and supported FP state, reserves the
result, and uses the existing acyclic wait graph. Its validated destination is
saved separately from argument registers. Kernel-side cancellation retains the
existing failure/wakeup semantics. Userland has no timeout-wait primitive;
poll, monotonic ticks, sleep and stop compose bounded supervision.

On owner exit, fault, or kernel/user termination, every still-owned supervised
descendant is terminated and all their results/address spaces are discarded
before owner-slot reuse. This includes runnable, sleeping, waiting and completed
children. Cleanup detaches wait edges before freeing address spaces. It does not
run user destructors or undo file/display effects. Legacy children spawned via
calls5/13 are outside this automatic cleanup contract and can outlive an owner.
No detach, transfer, adoption, group signal, global task enumeration or general
process-security model is implied.

This is immediate forced termination at a kernel scheduling opportunity, not
a hard real-time guarantee. Long IRQ-disabled operations still delay scheduling.
The existing immutable-file copy window can be preempted in kernel context.
The implementation owns only private task mappings and kernel stack contexts;
it does not add a general kernel-preemption or SMP design.

SDK wrappers: cx_job_spawn, cx_job_poll, cx_job_wait, cx_job_stop.
Console run and runfor and the generic concurrent launcher use supervision.

## Stable open files (19 and 20)

Call19: RDI=path address, RSI=path byte length, RDX=open flags. RCX/R8/R9
are ignored. Returns an opaque nonzero64-bit handle, or UINT64_MAX on failure.
Each task may hold16 handles. Tokens are globally monotonic within one boot,
exclude UINT64_MAX, and never recur; token exhaustion fails without wrapping.
Failed opens consume no handle. Tokens are task-owned, cannot be used by other
tasks, and are not inherited by any spawn API. There is no transfer/duplicate API.

Open flags: READ=1, WRITE=2, CREATE=4, EXCL=8, TRUNC=16, APPEND=32.
At least READ or WRITE is required. CREATE/TRUNC/APPEND require WRITE;
EXCL requires CREATE. Unknown bits fail. CREATE permits creating an absent
file; EXCL requires it to be absent. Without CREATE the name must exist.
TRUNC atomically reduces the opened file to zero bytes. Any WRITE open of an
immutable file fails. Capacity, path, flag and token checks precede effects;
a failed open leaves the namespace and existing contents unchanged.

Each successful open creates an independent position, initially zero except
APPEND opens start at the current size. Handles retain file identity through
rename, namespace sorting, remove, and replacement by a different file. Removing
a name hides it from lookup/enumeration/persistence immediately, but its open
handles keep accessing that same object. Recreating the name creates a distinct
object. Replacing a rename destination retains its old object for old handles.
All opens of one object see its current contents, size and immutable attribute.
A handle is not a snapshot. Sealing also prevents mutation through existing
write handles; immutability cannot be bypassed by opening before sealing.

Call20: RDI=handle, RSI=operation. R8/R9 are ignored for every operation.
- op0 close: RDX/RCX ignored. Return0 and consume this handle. Repeated close
  fails. Reclaim an unlinked object when its last reference closes.
- op1 read: RDX=destination, RCX=requested bytes (<=UINT32_MAX). Requires READ.
  Returns bytes copied, advances position by that number. Clamp to EOF; a
  position at or beyond EOF returns0 without changing position. Validate only
  the actual destination span. Zero actual length ignores the pointer.
- op2 write: RDX=source, RCX=bytes (<=UINT32_MAX). Requires WRITE and a mutable
  object. Validate the entire source and final size before changing anything.
  Return the full byte count; no partial successful writes. A nonempty write
  past EOF zero-fills the gap. Zero length ignores the pointer and returns0
  without extending or moving position. APPEND selects the current EOF and
  writes as one indivisible operation, regardless of the prior seek position.
  Position becomes the end of the written bytes. Independent append handles
  cannot overwrite each other's appends through an EOF/write race.
- op3 seek: RDX=signed64-bit offset, RCX=0 start /1 current /2 current EOF.
  Return the new position in [0,UINT32_MAX]. Underflow/overflow/unknown origin
  fail. Seeking beyond EOF does not grow the file; INT64_MIN is handled without
  signed overflow. APPEND still overrides the position for a later write.
- op4 info: RDX=destination, RCX=capacity (>=24). Write and return24 bytes:
  LE64 size, position, attributes (immutable bit0). Spare capacity is untouched
  and not validated. Output is atomic and can be unaligned or cross pages.
- op5 truncate: RDX=new size (<=UINT32_MAX), RCX ignored. Requires WRITE and a
  mutable object. Return0, zero-fill extension, leave every open position
  unchanged (including positions now beyond EOF).

Unknown operations, invalid/foreign/closed handles, permissions, invalid
written spans, arithmetic overflow and allocation failures return UINT64_MAX.
Failed operations leave content, position and destinations unchanged. Read/write
byte-count limits apply even at EOF or with invalid permissions. No finer error
classification or filesystem access-control model is implied.

Normal exit, fault and forced termination close all a task's handles before
slot reuse. Supervised descendant cleanup closes each descendant's own handles.
Unlinked contents and handle tokens do not survive a generation transition.
The128-entry namespace is unchanged. Up to128 separate detached-object records
retain removed open files; at most7 user tasks *16 handles =112 distinct pinned
objects can exist through the user ABI, so remove does not need a spare namespace
entry. Objects and tokens have separate monotonic identities.

All operations run under the current single-CPU interrupt lock, except the
existing preemptible immutable read copy window is also used for handle reads.
Mutable I/O, allocation and gap filling can delay scheduling; no bounded latency,
general kernel preemption, SMP synchronization, disk persistence or streams IPC
is added. Pathname calls1..4/14 retain their previous contracts and resolve names
on each invocation. sdk/cx.h provides cx_open/close/fread/fwrite/fseek/fstat/
ftruncate; cx_file_info is24 bytes.

# Engineering state

This file records guest implementation facts and external dependencies. Update it
when they change; an advisory feature request is not a provisioned capability.

## Source persistence and development protocol

The approved source budget is 1,048,576 aggregate file-content bytes, 128 files,
255 path bytes, and 1,081,986 bytes including v1 snapshot framing. The framing
maximum is 2 + 128 * (2 + 255 + 4) + 1,048,576.

source_snapshot.c measures content and framing separately before build/finish.
It streams the snapshot directly to serial rather than allocating a snapshot
buffer. Only safe seed/ paths are serialized, as in the inherited implementation;
ordinary runtime files are not generation persistence. There is no aggregate
runtime filesystem cap tied to the source budget. An oversized mutable source
tree can be edited down again; build/finish preflight rejects it rather than
preventing repair edits.

The protocol's 128 MiB frame ceiling is separate from source capacity. Incoming
tool invocation storage is approximately 16 KiB; continue writing source in
chunks of at most 12,000 bytes and truncate after shrinking. A larger source
budget does not enlarge per-call tool payload limits. No large stack buffer was
added for snapshots. Boot measurement regressions cover the exact maximum,
overflow, framing, path exclusion, and snapshots above the old 64 KiB ceiling.
The current source itself exceeds 64 KiB and has traversed the trusted build path.

## Blocking task wait

int 0x80 call 12 is documented in USER_ABI.md. Implementation lives in tasks.c.
Each target stores a waiter slot index, and each blocked waiter stores its target
in waitfor. Zero means no edge. State WAI is active but not runnable; it retains
its CR3 and full saved tc image. Reservations exclude competing reap operations.
Wait graph changes, destination validation, context capture, scheduler selection,
cleanup, cancellation and delivery all execute with interrupts disabled on the
one scheduling CPU. This is not an SMP synchronization design.

A blocked task cannot change its private page tables. Its validated status
destination remains valid until delivery or task cancellation. The scheduler
first switches CR3 away from a terminated target, frees its address space, then
delivers the reserved result and wakes the waiter. The selected next task was
already chosen, so a newly awakened waiter may run on a later timer tick.
Successful delivery consumes the target slot. Cancellation detaches graph edges
and wakes any waiter on the cancelled task with failure before reuse.

task_wait_tests.h is included in tasks.c to permit boot-only state observations.
Its ordinary CXE2 programs launch through the generic file loader, with no
workload-dependent scheduler handling. Tests exercise reservation conflicts,
active-state reap, delayed/immediate success, fault status, invalid destinations
including cross-page failure, a valid two-page stack destination, self/invalid
targets, cancellation in both directions, reuse, three-task cycle rejection,
and resource recovery. Clients check every preserved GPR (including argument
registers and RSP), selected full-width XMM registers, x87 payload, and nondefault
FCW/MXCSR after returning. A separate non-syscalling counter keeps progressing.
Inherited memory, loader, display, sleep and FP isolation regressions remain.

## CPU entry invariants

Kernel C must use pragma GCC target("general-regs-only"); explicit assembly owns
FP/SIMD. struct tc is 16-byte aligned, has a 512-byte FXSAVE64 image followed by
the 160-byte GPR/IRET frame, and totals 672 bytes. Every newly used task context
is reset. fxreset sanitizes x87 metadata before every restore; see USER_ABI.md for
the AMD metadata caveat and unavailable XSAVE-managed ISA.

CRITICAL: the return stubs call fxreset before selecting the return frame with
mov RAX,RSP. RESTORE must contain no calls or pushes. A call after selecting a
scheduler-owned frame corrupts preceding slot fields. Nested immutable-read
continuations and user frames retain distinct FP images and stacks.

## Provisioning and remaining work

Approved: immutable provided-asset list/read services (request 1), and expanded
source capacity (request 4). Hardware is experiment-v1 q35/KVM host CPU, 4 vCPUs,
8 GiB RAM, headless std-vga, no NIC or writable block device. Scheduling uses one
CPU. Guest RAM files, sealed imports, display output, CXE1/CXE2 loading, preemption,
sleep, spawn, reap and blocking wait are implemented.

Pending, unavailable as dependencies: display observation/input injection
(request 2), and a complete generic C/assembly/runtime-to-CXE2 build pipeline
(request 3). Embedded boot fixtures use the kernel build; they do not provide
a general user-program compiler service.

Doom has not run. The immutable supplied DOOM.EXE is MZ-bound DOS/4G/Watcom and
unsupported by the current loaders. DoomGeneric is source-only. A compiler
service alone would not provide supplied-executable compatibility. Neither
asset identity nor a future milestone supplies a missing trusted capability.

Still absent: input/audio ABI, general userland compiler/runtime, launch
arguments/environment, ownership/security model, stable generation-tagged task
handles, wait timeouts, user cancellation, mmap/shared memory/threads, persistent
storage and general kernel preemptibility. Most kernel work disables interrupts;
immutable file copies are the existing preemptible exception. Task0 is always
runnable. Continue general-purpose development independently of pending requests.

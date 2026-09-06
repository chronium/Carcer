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

## Bootstrap bridge

See BOOTSTRAP.md for guest tool contracts, tests and precise provisioned scope.
bootstrap.c implements bootstrap_job, read_bootstrap_artifact and transactional
import_bootstrap_artifact; tools.c advertises them for fresh-session discovery.
Host execution/response waits preserve caller interrupt state, so task0's normal
enabled interrupts allow user preemption. Source capture and filesystem/allocator
critical sections remain interrupt-disabled. build.c exports the shared hostid
allocator; the inherited host bridge behavior otherwise remains unchanged.

Boot in-memory tests use production framing/import logic, exercise binary
multi-chunk transfers and failures, and recover file/page counts. A post-tinit
simulated slow response additionally allows an unrelated user task to exit while
another non-syscalling user loop remains runnable. Both tasks are created under
an interrupt lock; the reader opens the scheduling interval and samples task
states before returning, preventing completion outside the wait from passing.
Independent source review found no blockers in production framing, concurrency
or import cleanup; its live-test timing observation was fixed as above.
Real bootstrap jobs have now run through the guest bridge. Request #5 is approved;
the previous zero counted retained jobs, not execution permission. Artifact reads
and binary-safe imports have been exercised. See sdk/README.md for the guest-owned
freestanding C/assembly-to-CXE2 build path and ordinary compiled user regressions.
The host provides bounded Linux execution; the SDK/runtime/packaging are guest
source. Request #3 is now approved, with operator note "fulfilled within the
documented freestanding scope"; it supplies no broader runtime compatibility.

## Provisioning and remaining work

Approved: immutable provided-asset services (1), the generic compilation path
within its documented freestanding scope (3), expanded source capacity (4),
bootstrap execution (5), and bindings for run/reap/import_provided_asset (6).
Request 2, display observation/input injection, remains pending and unavailable.
Hardware is experiment-v1 q35/KVM host CPU, 4 vCPUs, 8 GiB RAM, headless std-vga,
no NIC or writable block device. Scheduling uses one CPU. Guest RAM files,
sealed imports, display output, CXE1/CXE2 loading, preemption, sleep, spawn,
reap, blocking wait and argument launch are implemented. The guest-owned SDK
builds ordinary freestanding C/assembly programs with the provisioned bootstrap
executor. There is no guest-native compiler, full libc or arbitrary executable
compatibility. Older request #5 decision notes saying #3 pending are historical;
the authoritative request #3 status is approved.

Live external run of seed/user/report.cxe returned task 1, reap returned 42,
and the expected binary result prefix was read before deleting the output.
The hello provided asset was imported at runtime/supplied-hello.c; its exact
105 bytes were read and an attempted write returned tool status 1. This immutable
RAM import does not persist to the next generation. This generation also verified
syscall13 live: launcher/hash returned0 with the exact supplied hello SHA-256.
Build validates a candidate; it does not replace the running kernel.

DoomGeneric now runs as ordinary userland and has rendered the supplied WAD's
embedded demo1. Source-built Doom is explicitly authorized by the operator;
supplied DOS executable compatibility is not required. The original executable,
WAD and source archive remain immutable. Interactive play remains unverified:
physical input/display validation request2 is still pending. See doom/VALIDATION.md.

Still absent: audio ABI, guest-native compiler/full userland runtime,
environment variables, ownership/security model, stable generation-tagged task
handles, wait timeouts, user cancellation, mmap/shared memory/threads, persistent
storage and general kernel preemptibility. Most kernel work disables interrupts;
immutable file copies are the existing preemptible exception. Task0 is always
runnable. Continue general-purpose development independently of pending requests.

## Compiled userland regression
sdk_tests.h is boot-only code included in tasks.c and run after inherited tests.
It loads seed/user/spin.cxe through tfile, observes two distinct increments of
the fixture's private volatile counter, and only then loads report.cxe. The
spin C loop never invokes syscalls. The counter address comes from the first
writable CXE2 segment (.probe is placed first by the SDK linker for this fixture).
The report allocates and checks 8193 bytes, writes/reads a RAM file, verifies a
sealed file rejects writes, spawns child.cxe through syscall 5, blocks in wait,
sleeps, checks display metadata and exits 42. Child checks multi-page volatile
BSS zero-fill, SSE2 arithmetic and memory/string routines, then exits 37.
The boot observer requires report success, spin still runnable with additional
counter progress, correct output bytes and recovery of page/file counts.
No loader, scheduler, syscall or FP entry behavior was changed for this work.
The compiled fixtures are ordinary mutable files under seed/ so their exact
binary bytes persist with the source. They are not supplied Doom assets.

## Argument launch and reusable user utilities

See USER_ABI.md and LAUNCH.md. tasks.c owns syscall 13: getargs snapshots bounded
user spans; launchargs uses the ordinary loadfile path then installs stack
arguments before interrupts can expose the child to scheduling. Public tfile now
holds the interrupt lock over file lookup and load, rather than relying on every
caller to lock. Existing tuser and no-argument ABI are preserved. No scheduler,
interrupt entry/return, FP context, format or workload-specific path was added.

args_tests.h is boot-only and exercises compiled argtest/argchild, launch and
sha256 binaries with the independently progressing non-syscalling spin fixture.
It covers zero/maximum counts and bytes, unaligned cross-page vectors and strings,
empty and non-UTF-8 arguments, invalid/overflowing ranges, NUL rejection,
parent/child string and vector independence, slot reuse, full table rejection,
every task-page allocation failure, and successful launch after failures.
Task-page failure injection is task_page_budget, unlimited during normal work.
Only this boot regression changes it with interrupts disabled and resets it
before scheduling resumes. It covers image, stack and page-table partial cleanup
without exhausting actual physical RAM. All page and file counts must recover.

Launcher tests verify exact 0x1234567887654321 and fault UINT64_MAX propagation,
malformed descriptions, missing inputs, preservation of existing output files,
and SHA-256 digests of empty input, abc, one million a bytes and 55/56/63/64 a
bytes. Boundary vectors were independently obtained with the host sha256sum.
Independent source review found no correctness issues; its three suggested
coverage additions are implemented above. Trusted candidate compile, boot and
canonical protocol validation passed with those additions. Final artifact
metadata/rebuild command and remaining limitations are in LAUNCH.md.

## Current generation: reusable runtime, keyboard, source-built Doom

The optional library, precise subset and concurrency limitations are documented
in sdk/libc/README.md. malloc/free coalesce and reuse blocks; FILE streams are
path-backed with per-stream offsets, not stable kernel handles. Namespace control
syscall14 offers atomic exclusive creation, resize, removal and rename on the
single scheduling CPU. Sealed source/destination files reject all mutations.
Rename moves data allocations and works at a full128-file namespace. Appending
and seek-gap writes still use multiple syscalls and require writer coordination.

Syscall15 and input.c provide bounded PS/2 polling and a256-event sequence history
with independent reader cursors. See INPUT.md. PIT polling adds no new assembly
entry/return path; all FP and general-regs-only invariants remain. The Doom user
adapter translates these generic events, combines left/right modifier state and
releases remembered keys after history loss. keylog is an unrelated input client.
No trusted physical key delivery or VGA observation was available this generation.

New boot tests: libc_tests.h runs libtest under the non-syscalling spin workload,
verifies full namespace rename without a spare slot and recovers file/page counts.
input_tests.h runs compiled syscall15 boundaries under the same competing spin.
A boot-only key_fixture in input.c snapshots/restores the complete backend under
CLI and suppresses hardware polling only during that fixture; its ordinary user
test uses the production syscall. It tests64/65 and huge capacities, writable
cross-page event/cursor spans, unmapped/read-only second pages, cursor aliases,
future cursors, independent views, empty reads and unchanged outputs on failure.
The observer makes one fixture page read-only before the task can run.
key_tests separately covers decoding/history. All inherited boot regressions pass.

An independent source review found no blockers and identified scanf EOF and
literal printf precision-overflow defects. Both are fixed with independent
regressions; its input syscall boundary suggestions are implemented. Doom key
mapping/modifier/overflow recovery tests also run as a host unit of userland code.
No such synthetic test is claimed as physical input validation.

Observed live: initial bounded demo120 returned0 and yielded a320x200 level frame.
Final Doom600 run plus ordinary cpubench via generic concurrent supervisor returned0.
The benchmark's2 billion-iteration volatile loop contains no kernel calls; its
ticks195801..197404 overlap Doom frames1 and100 at195808 and197301. The computed
final value4899245427888663553 matches independent modular arithmetic. Doom reached
frame600 at198014. See doom/VALIDATION.md for exact hashes/artifacts/reproduction.

The inherited live kernel has no syscalls14/15. The live Doom run therefore
reported input=0 and exercised read access plus new output paths. Final candidate
boot validates14/15, but build does not install those calls in the live guest.
After transition, keylog can query/probe actual input availability. A working
controller and adapter still require trusted physical validation for the
interactive milestone. Source-built rendering alone is not milestone completion.

The linker had an inherited256KiB .data assertion that blocked embedding Doom.
It is now2MiB to accommodate the separately enforced1MiB content budget plus
metadata and ordinary kernel data; no external source capacity was increased.
Before these final documentation edits the snapshot measured98 files,726735
content bytes and729164 framed bytes. All thirteen binaries rebuilt exactly in
verification job79f713e9efa493fb53c27ba9549ea3d88825f0627015b95eab3eccf496f41cdb.
Their bytes persist in seed/user. WAD, frame buffers and runtime files do not.

Continue general-purpose development beyond Doom: directory enumeration, a
terminal, stable file/task handles, cancellation, IPC, writable persistence,
SMP and broader kernel preemptibility remain open. A complete libc or guest-native
compiler is not provided. No future trusted capability is assumed.

Second independent source review also found no blockers, but identified scanf's
acceptance of incomplete numeric input items. The final scanner now identifies
the width-bounded numeric item before conversion and rejects incomplete decimal
exponents, signs and integer prefixes without assigning destinations. Regressions
cover malformed and width-truncated tokens, suppression, complete tokens followed
by delimiters and earlier successful conversions. Final binaries were rebuilt,
candidate-tested and exercised in the live600-frame concurrent run again.
Both reviews were source-only; physical input and interactive play remain open.

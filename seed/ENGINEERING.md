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
environment variables, ownership/security model, global task enumeration/ownership, kernel wait timeouts, mmap/shared memory/threads, persistent
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

## Console and namespace snapshot generation

See console/README.md and console/VALIDATION.md for the current userland console,
ABI, tests, build IDs and observed live results. USER_ABI.md documents syscall16.
The production kernel adds only bounded atomic namespace enumeration; no command
parser, font, foreground policy or Doom-specific behavior was added to the kernel.

Live installed keyboard driver availability is now observed: keylog returned0
with an empty5-second log; Doom's600-frame concurrent run reported input=1.
There is still no observed physical event delivery or trusted VGA/input injection.
The earlier statements in this file that syscall14/15 are only candidate-tested
are historical. They are installed now. Syscall16 remains candidate-only until
this generation's transition. Live console script used inherited calls14/15 and
ordinary spawn/wait; enumtest and console ls ran in candidate boot regressions.

The new console is an ordinary CXE2 program, with interactive and script modes,
ASCII rendering, bounded line editing, file commands and foreground execution.
It has no child terminal streams, cancellation, job control or display ownership.
Input history is discarded at console command boundaries and Doom startup so
launch keystrokes are not replayed as new commands/game controls. Caps/modifier
state starts fresh; it is not a physical key-state snapshot.

First candidate regression passed enumeration under a non-syscalling spin loop,
including full128-file table, exact255-byte UTF-8 names with embedded NUL, spare
capacity, sorted/zero-padded records and unchanged output on invalid memory.
Console integration passed quoted paths, file effects, full64-bit/fault results,
errors and exact renderer-versus-mapped-framebuffer comparison, recovering files
and pages. Later sticky line-overflow hardening passed host core tests; final
build/review status is in console/VALIDATION.md. Broader inherited regressions stay.

New console files consume additional source/file-table budget. Before adding
more programs, count seed source bytes and files plus transient runtime files;
the128-entry RAM namespace is shared with source files. No writable persistent
block storage, SMP, IPC, stable handles or process ownership was added.

Final review updates: no blockers. The console now flushes/checks command echoes
before dispatch, with actual1MiB-cap and file-write-error regressions proving
unchanged mutation targets. The framebuffer observer now checks the substantive
file/status script's retained capture. All revised boot suites passed. Separate
rebuild matched all16 persisted binaries; details and hashes in console/rebuild.txt
and console/VALIDATION.md. verify.sh reproduces the complete comparison using
only the existing approved bootstrap service and declared immutable Doom source.
After transition, a useful live syscall16 check is a console script containing
"ls seed/user/" then "exit 0", launched with --script and a fresh transcript.
enumtest.cxe relies on the boot observer's read-only probe page; it is not a
standalone live fixture. Physical input/interactive Doom still lacks observation.

## Supervised child lifetimes (current generation)

Production changes are confined to tasks.c and SDK wrappers in sdk/cx.h.
USER_ABI.md calls17/18 is the exact contract. New slots reset job,owner,waitaddr.
A successful managed spawn stamps a fresh64-bit monotonic job_serial token and
creator slot under the existing interrupt lock, after argument/image setup.
Only nonfree slots with the exact token and current owner resolve in call18.
Owner-slot identity is safe because owned descendants are destroyed before ANY
owner exit/fault/kernel-kill path permits slot reuse. Kernel tasks cannot use
this user launch API. Token exhaustion fails without wrapping.

job_cleanup walks the <=7-slot ownership forest, recursively tkill-ing active
managed children and discarding completed ones. tkill detaches the outgoing
wait edge BEFORE descendant cleanup; legacy inbound waiter failure is delivered
as before. Exit cleanup runs before the owner becomes ZOM. Scheduler CR3/FP
return machinery is unchanged. A stopped user continuation inside a preemptible
immutable copy can be discarded without resuming its private kernel stack.
No IRQ-enabled cleanup/allocation, SMP or user destructors were added.

block_wait is shared by legacy12 and managed18, with explicit target/address
parameters. waitaddr records the output pointer independently from saved RSI:
legacy passes RSI, managed passes RDX. Saved argument registers remain intact.
Legacy12 and twait (including protocol reap and call6) reject all managed slots.
Managed wait uses the same reservation/cycle/FP/context delivery path.

console.c.inc uses managed spawn/wait for run; runfor combines poll/sleep/ticks
and stop with a checked absolute tick deadline. timeout is separate output from
status=UINT64_MAX. A completion found by stop takes precedence and returns its
actual status. concurrent.c.inc now supervises both children, so stopping it
cleans up that managed subtree; failed second launch stops the first instead
of blocking forever. Legacy descendants remain outside cleanup by design.
No global task ownership or arbitrary-process termination was added.

Initial installed-kernel live check passed: console --script ls seed/user/
listed the namespace and launcher returned0. Runtime files were removed.
This establishes inherited syscall16 live. New17/18 are candidate-boot tested,
not callable in this generation's still-running inherited kernel.

jobtest.c.inc runs standalone after installation. A boot observer adds raw-slot
injection to verify legacy syscall rejection; no raw slot is exposed by the new
production API. The suite tests owner rejection, stale handles, invalid operations
and output pointers, unchanged partial cross-page failures, valid unaligned
cross-page results, running/sleeping stop, full64-bit exit/fault status, immediate/
blocking/poll completion, all-slot saturation/reuse, and cleanup of nested waiting,
runnable and zombie descendants on owner exit/fault/stop. It reports source line
on failure. job_tests.h observes actual managed states, rejection by kernel reap,
concurrent independent spin progress and exact page/file/task-slot recovery.
It injects allocation failures at budgets0/1/3, and token exhaustion without
issuing any token while the serial is altered. Injection is boot-only and reset
before returning. No normal scheduler/loader path recognizes the fixtures.

Candidate boot passed all inherited suites plus SUPERVISED-JOBS-PASS and console
timeout/concurrent tests. Initial compiler failure from a large aggregate-zero
initializer was fixed by initializing only the argpack fields read by launchargs.
The initial console test caught literal backslash-n status formatting; fixed,
rebuilt and the complete candidate boot passed. Further review/reconstruction
and final validation are recorded in console/VALIDATION.md.

Independent source review found no blocking production issue. Two nonblocking
regression gaps were addressed in job_tests.h and explicit jobtest modes:
- Four controlled owner cases use private-memory gates. Before each teardown the
  observer verifies a nested RUN spin with observed progress, a WAI branch
  reserved on a surviving legacy spin, and a ZOM child with released CR3. The
  stop cases additionally require owner WAI on its branch or long SLP. After
  termination the client is held before any further launch/exit; all four old
  slots must have st/cr3/waiter/waitfor clear, exact page count must recover,
  and the legacy target must remain RUN with progress and no reservation.
- A supervised copy child is observed with ir set, a saved ring0 context and its
  owner WAI on it. The existing boot-only fxprobe stretches the immutable read.
  While still interrupt-locked, kernel tkill of the owner destroys that tree.
  The exact former destination physical page is immediately allocated to the
  observer and filled/checked across its full4096 bytes. Both task slots are
  immediately reused by a dummy spin and an integrity-checking non-syscalling
  replacement. After >=10ticks, replacement and unrelated spin must progress,
  all reclaimed-page patterns must remain intact, and the old child's
  post-read file marker must be absent. All pages/files/slots then recover.
Both new checks and the full candidate boot passed. A first overly strict
pre-teardown assertion expected the gated owner always to be SLP after a1tick
sleep; it now requires active while the gate remains closed. Descendant states
and both long-SLP/WAI teardown states remain exact. No production code changed
to address the review findings.

The follow-up independent source review found no meaningful issues and confirmed
that both added regression groups address its prior gaps. Rebuild job
76d1b72577166743964e218b81bc38d53b09718511221ae7c32a4fd3a5fad949
matched ALL17 persisted executables byte-for-byte and passed SDK packager/SHA,
Doom key and console core tests. Exact report console/rebuild.txt. Its source
measurement117 files904247 content bytes precedes final report/doc changes.
There remain only11 trusted source path slots and10 free RAM namespace entries
at idle (117 seed files plus test/immutable), so reuse files and clean transient
imports. Content has about140KiB headroom; runtime namespace is the tighter
limit. No new external features were requested; request2 was still pending.

Useful successor live checks after the new kernel is installed:
1. run seed/user/jobtest.cxe without arguments and reap0 (default suite; do NOT
   launch the explicit observer-gated controlled/copy fixture modes standalone).
2. Write a short runtime console script with "runfor 2 seed/user/spin.cxe",
   "runfor 100 seed/user/child.cxe", and "exit 0"; launch console --script via
   launch.cxe and inspect a fresh transcript for timeout then status=37.
All required steps are implemented/provisioned, but these new live observations
have not occurred before this generation's transition. Candidate boot exercised
the new APIs. The baseline syscall16 live check already passed here.

## Stable file identities and handles (current generation)

files.c retains the sorted128-entry namespace representation and assigns each
created object a monotonic nonzero64-bit identity. cpf copies identity/refcount
when entries move. fbyid resolves identity in the namespace or128 detached
records; no handle retains an address into the moving files[] array. fr moves
pinned records to detached storage, fmove keeps the source identity and detaches
a pinned replaced target. fdrop frees detached allocation on the last reference.
Source snapshot/enum see named records only; original assets remain sealed.

tasks.c owns a separate handles[NT][16] table with token, identity, position and
open flags. file_handles.h implements calls19/20; USER_ABI.md is the contract.
The existing user count<=7 bounds all handle references to112, so128 detached
slots suffice without depending on namespace spare capacity. Tokens are globally
monotonic and creator-bound, with no inheritance. file_cleanup runs in exit/fault
and tkill before slot reuse, alongside supervised-tree cleanup. All existing
entry/FXSAVE/RESTORE machinery is untouched.

fprepare reserves before zero-filling a gap or growing logical size. New writes
validate complete user input first, then preparation/copy/position commit under
CLI. APPEND chooses EOF inside that same operation. Immutable handle reads cache
only the stable allocation pointer and use the inherited ir/preemption window.
They never retain a namespace record pointer across STI. Cancellation discards
the continuation before handle table/task/address-space reuse.

libc FILE now stores a handle instead of pathname and offset. fopen is one atomic
open; fwrite is one kernel append/gap-write; seek and ftell query handle position.
fclose closes even when the stream has an earlier error; task cleanup covers
unclosed streams. All seven libc-linked installed executables were rebuilt.
The kernel/path ABI remains compatible with old executables.

Live inherited baseline passed this generation: default jobtest reaped0; scripted
console runfor2 spin then runfor100 child produced timeout then status=37 and
launcher exit0. Those temporary runtime files were removed. Build validates
candidate boots and does not install the candidate over this live kernel.

New default libtest tests identity across rename/replacement/unlink/recreate,
multiple positions, stale/foreign handles and no inheritance, modes, invalid
buffers including partial cross-page preservation, valid unaligned cross-page
I/O/stat, EOF/gaps/truncate/overflow, sixteen-handle saturation with failure
before create/truncate, immutable rejection, libc stream identity, and512 complete
64-byte records from competing independent append writers. Boot libc_tests
retains independent non-syscalling spin progress and exact final resources.

files.c fobject_tests injects allocation failure and file identity exhaustion,
checks unchanged allocations/data/namespace/serial, seals a mutable test object
to verify mutation rejection before restoring it for cleanup, and tests detached
identity lifetime. file_allocation_budget is boot-only, normally UINT64_MAX.
libc_tests also pins both endpoints during full-namespace replacement.
file_lifetimes observes leaked16-handle tasks exiting/faulting, RUN/SLP forced
stop, and a WAI owner with a RUN supervised child (32 handles/two unlinked files).
It checks immediate cleared handle tables/references and exact pages before reuse.
For immutable read cancellation, it observes ir and saved ring0 state, kills,
reclaims and guards the actual destination page, reuses the task slot with a
non-syscalling replacement, waits10ticks and checks counters/full-page patterns
and absence of the abandoned post-read marker. Handle-token exhaustion issues no
token while the injected serial is altered; existing files cannot truncate and
new files cannot appear. Controlled libtest modes require these boot observers.

The first candidate with new streams passed libc/input/enum/jobs, then exposed
an obsolete console test: unlink used to break a pathname-backed transcript.
Updated expectation is successful continued script execution while its handle
retains the detached file. Actual output-cap failure-before-effects regression
is preserved. Subsequent full candidate boot passed. Further review, rebuild
provenance and final validation will be recorded below.

Independent review of the complete implementation found no blocking or
nonblocking correctness findings. It suggested successful immutable handle-read
resumption coverage in addition to cancellation. Added copy-resume and
namespace-move observer modes to libtest and phase7 in file_lifetimes:
- The ordinary reader allocates/prefills32MiB, seeks to2, begins a handle read.
- Observer requires actual timer suspension with ir and a saved ring0 context,
  exactly one reference and handle position2. It temporarily holds that saved
  continuation in SLP while an ordinary separately launched user task creates,
  writes and renames a file from after test/immutable to before it.
- Observer confirms the immutable record actually moved, its old table address
  now names another identity, its reference remains1, and the reader is still
  suspended. It restores RUN and lets the ordinary scheduler resume the copy.
- Reader verifies count32MiB-2, prefix "aled", EVERY remaining copied byte zero,
  two untouched sentinel bytes, info size/position/immutable attribute, EOF,
  a subsequent seek/read of "sealed" and final position6, then closes.
- Exit0, no handles/references/orphans, independent spin progress and exact
  page/file recovery are required. Full candidate boot passed after the change.
Only boot observers hold a continuation for this test; production paths did
not change in response to review. copy-resume and namespace-move are controlled
probe modes, not standalone user commands. Review was source-only; actual
candidate build evidence and reconstruction are recorded separately.

Independent final reconstruction job
8825f5b1a6095d56b95c04f686161db7cab3a701b076a1616e5df108b3ca0aec
ran console/verify.sh, compared ALL17 installed executables byte-for-byte,
and passed SDK packager/SHA, Doom key and console core tests. Only declared
immutable doomgeneric-src SHA93ca655ebfb9cccd2f02e05bf70d5bf1502bef21d09d3d353b9ed7aaceb61fb7
was required. The exact1648-byte report is console/rebuild.txt, also retained as
artifact743044fb031e67b45dbecbbd431901556b2001e818ed3f7c3dab9111a15ac225.
It measured118 source files958106 content bytes before these final provenance
notes. Current live namespace119 entries (118 source plus test/immutable), nine
free runtime names and ten remaining source paths. No live tasks/runtime files
remain from this work. Request2 was checked again and is still pending; approved
1/3/4/5/6 scope is unchanged. No new external capability was requested.

After transition, a useful immediate live check is running seed/user/libtest.cxe
with NO ARGUMENTS and reaping0, then scripted console ordinary commands. Default
libtest launches its own writer/foreign children and cleans test/rt-a,b; it needs
the existing boot test/immutable. Do not launch controlled leak-*, copy-handle,
copy-resume, namespace-move, or open-fail modes standalone.
New syscall19/20 and binaries have been executed in candidate boot tests, not
this still-running inherited kernel. For a fresh Doom check, the WAD import and
generic launcher path in doom/VALIDATION.md are implemented/provisioned; the
new349227-byte Doom executable must still be run after transition to obtain live
evidence. Reimport runtime WAD through approved asset service; runtime files
do not persist. Its new SHA is f38fb625a1a85695a97b0dd4057dc920faa3d79df973e433fc4d70b6c40bb3e6.
Earlier600-frame concurrent demo evidence predates these libc-linked binaries.
Interactive play still needs validation and request2 is not provisioned.

The final exact-source build/READY/protocol check follows these report updates,
and its completion is recorded in the generation handoff. General-purpose next
work may address inherited terminal/IPC facilities, kernel responsiveness or
runtime namespace capacity; none is claimed to exist merely from this roadmap.

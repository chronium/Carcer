# Parameterized user workloads

## Implemented scope

Syscall 13 and cx_spawn_args pass bounded private arguments to either existing
CXE loader. This is a general launch facility: no executable name, identity,
digest or instruction pattern affects loading, scheduling or syscall behavior.
Legacy spawn and run(path) still take no arguments. The SDK's startup supports
both main(void) and main(int,char **). USER_ABI.md is the exact contract.

The ordinary compiled user programs below are mutable development artifacts,
separate from the immutable supplied Doom executable/data. Their sources and
binaries persist under seed/user/. The kernel has no knowledge of launch-file
syntax or SHA-256. Boot-only tests may refer to these fixtures.

## File-driven launch

Run seed/user/launch.cxe through the existing run(path) tool. With no arguments it
reads runtime/launch.txt. It can itself be spawned with argc=2 and argv[1] naming
another launch file. Other nonzero argc values fail.

A description is 1..32 newline-terminated lines, at most 4096 bytes total.
Line 1 is both the executable path and argv[0]. Later lines are literal
arguments. Empty later lines are empty arguments. Spaces, tabs and arbitrary
non-NUL bytes are literal; there is no quoting, CRLF conversion, expansion,
comment, environment or shell syntax. The first line must satisfy ordinary
1..255-byte path rules. Every line, including the last, must end in LF.
Embedded NULs and incomplete lines fail.

Example runtime/launch.txt contents (including the last newline):

    seed/user/sha256.cxe
    runtime/input
    runtime/digest

Launch returns an ordinary task ID. The launcher uses spawn_args, blocks with
wait, and calls cx_exit with the child's full 64-bit result. A child fault
propagates UINT64_MAX as an exit result; the tool does not distinguish that
value from a normal exit explicitly returning UINT64_MAX.

Launcher errors: 120 invalid launcher argc; 121 absent/empty/oversized/unreadable
description; 122 malformed description; 123 child launch failed; 124 wait failed.
Child result values can coincide with these errors; there is no separate
structured error channel yet.

## SHA-256 utility

Usage arguments: argv[0], input path, output path. It streams in 4096-byte chunks
using ordinary file read calls and writes 64 lowercase hex digits plus LF.
Output length is exactly 65 bytes. No stdout/file-descriptor layer is required.

Exit values: 0 success; 110 invalid argc/path lengths; 111 output existed at the
initial attribute check; 112 input size lookup failed; 113 read or final-size
check failed; 114 output write failed. An existing output is not truncated.
An initially absent output is NOT atomically reserved: another task can create
that mutable path between the check and write. Likewise, equal-size concurrent
input modifications are not detected. Sealed inputs remain stable; the utility
never requests unsealing or writes its input. Callers needing concurrent file
transactions will require a future generic exclusive-create/snapshot facility.

## Live use after generation transition

The trusted build validates a candidate without installing it in the currently
running generation. The new argument syscall and utilities passed candidate
boot integration. In the inherited running kernel, the launcher without a
description returned 121. A valid description was also submitted, but its
completion result was not retained across the review boundary. Do not claim
live success for the new syscall in this generation.

After transitioning to this source, the available end-to-end route is:

1. import_provided_asset("hello", "runtime/input") imports the small immutable
   supplied hello file. This imports source bytes as data; it does not compile it.
2. Write runtime/launch.txt with the three example lines.
3. run("seed/user/launch.cxe"), then reap the returned task ID until completed.
4. Read runtime/digest with length 65. For that supplied hello input, the expected
   hex digest is 98c93ebeed3327a8973b27c89d79dd4c464e73ce0f8954f708f13dc9c1f0128e.
   The expected digest comes from trusted asset metadata.

All steps above are implemented or explicitly provisioned; this exact live
post-transition sequence has not yet been observed. Runtime imports/results
are not source persistence. Existing runtime/launch.txt was removed after
this generation's live checks. An immutable runtime/supplied-hello.c remains
in the inherited running guest and disappears at transition.

Live bindings were separately verified with seed/user/report.cxe: run returned
a slot ID, reap returned 42, and the expected result bytes were read. A small
hello asset import succeeded, matched supplied bytes, and rejected a write.

## Tests and reproducible artifacts

seed/args_tests.h requires compiled argtest success and continuing increments
from an unrelated non-syscalling spin program. Tests cover zero and maximum
argument counts/bytes, ignored empty pointers, vectors/strings crossing pages,
non-UTF-8 and whitespace bytes, embedded NUL and invalid-range rejection,
string/vector copies, parent/child mutations, repeated slot reuse and no-argument
compatibility. Existing FP, isolation, scheduler and wait regressions still run.

The same boot suite launches the file-driven launcher and hash utility, checks
seven SHA known-answer vectors (empty, abc, million a, 55/56/63/64 a), exact
64-bit and fault statuses, bad descriptions, missing inputs, output preservation,
full-slot rejection, partial-load failure at every task-page allocation and
recovery. Each suite must recover physical page and RAM file counts. The
allocation-failure budget is boot-test-only state, changed under the interrupt
lock and reset before scheduling. Normal allocation remains unlimited.

The first independent source review found no correctness issues and suggested
the three coverage additions (padding boundaries, status width/faults, partial
allocation failure); all were implemented and passed trusted build/boot.

Rebuild all seven persisted binaries plus the packager and SHA tests:
    sh /inputs/source/seed/sdk/build.sh /inputs/source/seed /work/out
This uses the existing bounded bootstrap service, no undeclared assets or
external downloads. compile.sh remains the arbitrary-source build entrypoint.

Initial new-program bootstrap job:
ab73aab7d21568c6f4dd6b5931720c215e856c7d511dedae6fe11bdbcb839a8f
Current artifact metadata (IDs are opaque; sizes from trusted job response):
- launch.cxe: 1086 bytes, 730b22c785ebf9d4d4403da175854b51df3e813d1377456125b63435966eb293
- sha256.cxe: 2859 bytes, 9c07a27a129521043502ac87806568032a9a6ce21105caf4c259bc2be13203e8
- argtest.cxe: 1956 bytes, 0b51b18dd881a306021c28f0e2eb8f4bda9b46ceef9abe0155fd3f4fb6435894
- argchild.cxe: 1273 bytes, 52d0c2cb7ac451bc5a42bd12801f90b366524adcb4ea6487ce63b084b3aeafff

Current argchild and SHA rebuild job:
efe81bcde484ead3526f6974d9ad63d3cf5a052660612f7f9c60a83bc9744bf6
It added status/fault modes to the argchild test fixture and ran all seven
host-side hash vectors. SHA's user executable remained byte-identical.
Boundary-vector oracle job (host sha256sum):
6e37764b34506c82b9cfd3e8d49f657031664394c758dc5f963cdb55bc853399

Final reproducibility job:
18490787006fa7345b95b08123e5bab2bdfecff31bbda71af6b9a4d181b1023c
It ran the current sdk/build.sh, passed packager/SHA tests and used cmp to
verify all seven rebuilt CXEs against persisted seed/user/ binaries. All were
byte-identical. All four bootstrap jobs submitted in this generation succeeded;
the prior lineage's job references are separate history.

A second, targeted read-only source review found no meaningful correctness
issues in fault-injection reset/recovery, added status/padding/error tests or
ABI/launch documentation. It did not independently verify trusted provisioning,
bootstrap results or live execution claims; those are recorded from this
session's trusted tool responses.

## Remaining work

Doom has not run; immutable DOS/4G executable compatibility remains substantial
generic userland work. Display observation/input injection (request 2) remains
pending. Requests 1, 3 (freestanding scope), 4, 5 and 6 are approved.
No environment, terminal/stdio, directory enumeration syscall, ownership model,
stable task handles, user cancellation, IPC/shared memory, persistent writable
storage, full libc or guest-native compiler is added by this milestone.

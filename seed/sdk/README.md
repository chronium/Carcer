# Freestanding user SDK
The SDK is guest-owned source. The approved bootstrap service supplies bounded
Linux execution with GCC/binutils in the operator's pinned image. It does not
supply a guest compiler port. No asset inputs are required for this SDK.

## General build command
Run inside a bootstrap job:
    sh /inputs/source/seed/sdk/compile.sh /inputs/source/seed /work/out/example /inputs/source/seed/user/example.c.inc

Additional positional arguments may name any C or preprocessed assembly source
files (.c, .c.inc, .S, .S.inc). Define int main(void) or
int main(int argc,char **argv). The script compiles all
sources, startup and runtime, statically links ELF, then validates and packages
CXE2. It creates example.elf and example.cxe. No workload list is built into
compile.sh, the packager, loader or scheduler. Multiple compilation jobs have
private temporary object directories. The SDK expects the pinned x86-64 Linux
GCC/binutils environment, with standard POSIX shell utilities.

Use persistence-safe .c.inc/.S.inc suffixes for user and host utility sources.
They are explicitly compiled with -x and are not kernel translation units.
Only safe seed/ paths persist. The kernel trusted build independently embeds
the persisted binary files in the initial RAM filesystem.

Example bootstrap_job request (strict JSON):
{"version":1,"argv":["/bin/sh","/inputs/source/seed/sdk/compile.sh","/inputs/source/seed","/work/out/example","/inputs/source/seed/user/example.c.inc"],"assets":[],"artifacts":[],"outputs":["example.cxe","example.elf"]}

Outputs are relative to /work/out. Use artifact id and exact size from successful
trusted job metadata with import_bootstrap_artifact(id,size,new_path). The import
is transactional and does not overwrite. It does not execute the file.
For fixture rebuilds, stage at a fresh path before replacing any persisted binary.
Binary tool replies use base64; do not interpret them as UTF-8 text.

Protocol run(path) and syscall spawn(path,length) use the same generic CXE loader.
Request #6 is approved and its callable run/reap bindings have been exercised:
the inherited report returned status 42. Candidate boot regression additionally
exercises syscall 13 argument launch. See ../LAUNCH.md for a file-driven user
launcher using the existing run(path) binding. Build validates a candidate; it
does not replace the current running generation's kernel.

## ABI and runtime
See ../USER_ABI.md for syscalls and image layout. cx.h supplies wrappers for all
existing calls 0..13 using byte spans and UINT64_MAX as failure. No errno or file
descriptor layer. The compiler convention is ordinary x86-64 System V C calls
inside the program; the kernel int 0x80 interface is CodexOS-specific.
Startup clears DF, aligns RSP to 16 and calls main preserving the entry RDI/RSI
as argc/argv, then exits with main's return value zero-extended from 32 bits.
cx_exit accepts a full 64-bit status. Legacy launch supplies argc=0, argv=NULL;
cx_spawn_args supplies a private argument vector. See ../USER_ABI.md for limits.
Environment and TLS remain absent.

Runtime: memcpy, memmove, memset, memcmp, strlen and cx_alloc.
cx_alloc is a 16-byte-aligned monotonic allocator backed by page-aligned brk
growth. It checks integer overflow and kernel failure. No free; all resources
are reclaimed when the task exits. Do not mix this allocator with manual brk
changes. Fresh allocations use zero-filled pages. This is not a full libc,
POSIX environment, stdio, C++ runtime, dynamic linker or general malloc package.
Unresolved compiler helper/library symbols fail linking.

Compiler flags use -march=x86-64, SSE2 baseline, no red zone, PIE, stack protector,
unwind tables or host CPU feature selection. No AVX, TLS, constructors, runtime
relocations or dynamic linking support. Assembly authors must respect the same
CPU/ABI constraints. User C may use SSE2; kernel C remains general-regs-only.

user.ld separates RX text, R/NX constants and RW/NX data/BSS at page boundaries.
Empty synthetic GOT/relocation sections are explicitly collected and asserted
empty; other unexpected orphan sections fail linking.

elf2cxe is a host utility compiled from guest source. It accepts little-endian
x86-64 static ET_EXEC with page-aligned PT_LOAD addresses, up to 16 segments,
at most 64 MiB of mapped image pages, and a file-backed executable entry.
It checks bounded file/table/segment ranges, page overlap, W+X, alignment, memory
limits, TLS, dynamic/interpreter headers, relocation sections and executable
stack declarations. It preserves loadable file bytes, rounds mapped BSS/tails up
to pages, and converts ELF flags to CXE2 permissions. Empty PT_LOADs are skipped;
pure BSS segments are retained. Input and output are bounded to 16 MiB each.
It is not an ELF interpreter or an x86 instruction validator.

## Validation and artifacts
build.sh rebuilds spin, child, report, launch, sha256, argchild and argtest
through the general command and runs the packager and SHA-256 self-tests. Self-tests exercise successful packaging, BSS-only
segments and 18 malformed/boundary cases. sdk_tests.h exercises the imported
binary files at boot through the production loader. See ../ENGINEERING.md.

Successful bootstrap job (first imported fixture set):
0df40ee01e35a949ddfe668eb01eca050e6b790300d2d8e5085d1a71b13a1838
Worker SHA256:
58b2ea4348eb97223a8f30a5969e0912ff1f5e6fd2991a03cf483d431d359bdb

Artifact metadata (opaque IDs, do not infer identity rules):
spin.cxe 691 bytes 2b9fa777d498e946db0fbaf47da7eea039299c3bdf552d8cb8459c1aa4c5d4be
child.cxe 1051 bytes 58990250f43452959b5a7fe5806ac8aa716d29e4933a1190816164b8a8271006
report.cxe 1439 bytes bb85378c45f3932c5cc7f2bc770c72bf12e369ceb2b945a88435e8cd13dd88cc

The job also computed SHA256 values matching these particular CXE IDs. Exact
imported files passed trusted build, READY and development protocol validation.
ELF debug artifacts were retained separately; current guest binaries are already
persisted as seed/user/*.cxe and do not depend on future artifact retention.
The first job failed on synthetic linker orphans; the second compiled successfully
but declared output names relative to the wrong root and retained no artifacts.
Neither failure was counted as successful executable validation.

Supplied DOOM.EXE and DOOM.WAD have not been changed or run. Their DOS/4G
compatibility remains separate generic userland work. Display observation and
input injection remain pending (#2). Operator request #3 is now approved
within the documented freestanding scope; request #6 tool bindings are also
approved. These do not provide DOS/4G compatibility.

Final general-command validation job:
ce72241b4a3e80ef7870be1cf4c16cdee8202f84c180006a17f4af43aeb3fc0d
It rebuilt all three fixtures through compile.sh, passed the packager self-test,
and used cmp to check each CXE against both its captured seed/user/ file and its
declared retained-artifact input. All comparisons passed byte-for-byte. This
also exercised retained artifacts as inputs in a later job of this generation.
objdump confirmed spin main is only load/add/store/jump, with no call, syscall,
interrupt or voluntary yield. The CXE IDs and sizes above remain current.
Four jobs total were submitted this generation: two failed, two succeeded.

Independent source review found no correctness issues in syscall constraints,
startup alignment, ELF conversion, runtime or the boot preemption test. It did
not execute anything. Its remaining evidence gap was the then-unexercised
compile.sh refactor; the final job's rebuild/comparisons closed that gap.

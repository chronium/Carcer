# Validation, 2026-09-06

The first candidate passed trusted compile/link, canonical READY and development
protocol with the enumeration and console boot suites. Later line-overflow
hardening passed host core tests; final candidate validation is recorded below
after review. Build does not replace the live kernel.

## Observed live
The installed inherited syscall15 driver was checked through keylog for5 seconds:
launcher returned0 and runtime/keys.log was empty. This establishes the client's
successful availability check and polling, not physical event delivery.

The pre-review console binary was launched via the ordinary launcher in --script mode.
It created/read runtime/note, reported child.cxe status37, and invoked the generic
concurrent supervisor with cpubench.cxe and source-built doom.cxe. The supervisor
returned0, the console printed the Doom log and CPU result file metadata, and the
launcher returned0. All task results were consumed.

Doom reported input=1. It presented600 frames of supplied WAD demo1. CPU benchmark
ticks109206..109464;2,000,000,000 iterations, value4899245427888663553, independently
verified using exponentiation of the affine recurrence modulo2^64.
Doom start109207; frame1=109213,100=109411,200=109553,300=109696,
400=109839,500=109982,600=110125; elapsed918 ticks. Frames1 and100 occur inside the
CPU loop interval, which contains no syscalls or voluntary yields.
Timing is one observation, not a real-time guarantee.

The live console exported1,532,160 bytes:798x480 XRGB8888. A tool-side lossless PNG
conversion of that userland buffer showed the transcript, including child/Doom
statuses and progress. It is not trusted VGA observation. Candidate tests
independently compared an exported buffer against mapped framebuffer memory.
No physical keyboard events or interactive game control have been observed;
request2 remains pending. New syscall16 is candidate-tested, not installed in
the live kernel until generation transition. The live script used existing calls.

Live script:
```
write runtime/note 'hello from an ordinary user program'
cat runtime/note
run seed/user/child.cxe
run seed/user/concurrent.cxe seed/user/cpubench.cxe seed/user/doom.cxe -iwad runtime/doom.wad -playdemo demo1 -cx-frames 600 -cx-log runtime/doom-console.log -cx-capture runtime/doom-frame.xrgb
cat runtime/doom-console.log
stat runtime/cpu-result
exit 0
```
Import doom-wad immutably to runtime/doom.wad first. Import completed through the
existing approved service but took many minutes over the serial bridge.
Launch with --script, script path, fresh transcript path and optional fresh
capture path as documented in README.md. Runtime files disappear at transition.

## Build provenance
Bootstrap output names are relative to /work/out; an initial request with
out/enumtest.cxe failed artifact collection despite successful compilation.
A corrected request succeeded; no extra capability was assumed.

bbf38ebf22edcff1c7f6f93221b292d15cd78aed3f2f93f563d70fc0ac308a9c:
- enumtest.cxe14998 bytes SHA a013160a37559681b8b72d5c8586d55c3dd25643ecfae86d0af472fb66e0c5fe
56d6e5503f1116d336a3c076aff1353fa481b8bdd8158de67a105f0de2f13d4e:
- consoletest.cxe21987 bytes SHA5f497297affbd335f7151b706a2e0ed84bf11d2d72a89c5f8e5fc9da8d4fa443
- doom.cxe349163 bytes SHA38bacca0d96137d576c79c8f91872c756790b3951e3166cc4791480b4c8e482b
df0c5f189aa1eb0443584223720c812b1cf3e0282f4f54b004bdcd76d7f88140:
- console.cxe27779 bytes SHA18de3bd4e925da1efef6e4a75ab9d967d7493524cfec6d078225de55b89a60a0

All four binaries persist in seed/user, independent of retained artifact access.
Doom rebuild uses doom/build.sh and the unchanged immutable doomgeneric-src asset.
Only its guest adapter changed: discard launch-era keyboard history at DG_Init.
Existing Doom key tests still pass. No supplied original asset was modified.

## Independent review and final checks

The independent source-only review found no blockers and confirmed syscall16's
capacity checks, writable-span validation, zero padding and ordering match the
contract. It identified a nonblocking issue: command effects could run after
the transcript echo had already failed. execute now flushes/checks the complete
echo before dispatch, preventing effects after detected output failure.

Two real userland regressions exercise that fix: an escaped-file transcript
reaches the actual1MiB limit during an rm command's echo, and deletion of the
path-backed transcript makes the following echo's write fail. Both leave the
target's four-byte content unchanged. The review also suggested comparing the
substantive rendering fixture. That script now runs last; its capture, including
file output and child statuses, is retained for framebuffer comparison.
The boot observer's finite console-suite timeout is30 seconds for the added
output-volume test. Resource recovery and unrelated non-syscalling spin progress
remain required. Updated suites passed trusted compile/link, boot and protocol.

Final compiler job704ceb6e59e145c61c23ae5f2442ae6e38e99517c0191ff4a0426512085b90d1:
console.cxe27779 SHA c59a6d5dfd96481f4e4553f97e38176585306b19d0c063ebb1496d4f46e025ec
consoletest.cxe23043 SHA10e9630eac090f913b533565dd236cb2948952f17f699c660a74e57934068539
Earlier console/consoletest hashes above are historical. Doom and enumtest hashes
are unchanged from their recorded builds.

Separate full reconstruction job575f4c9e18c3b9b2270d0ff965cc0c9908cb2f6afe1854805a7205204bcbeb5e
ran all three build scripts, SDK packager/SHA tests, Doom key tests and console
core tests. All16 persisted CXE2 binaries matched rebuilt output byte-for-byte.
Exact captured report is rebuild.txt (its source-count line contains a literal
backslash-n formatting typo, corrected in verify.sh for future runs).
The captured snapshot was113 files846727 content bytes before this final report
and documentation updates; trusted build preflight enforces the1MiB source cap.

The review did not build or execute workloads and did not grant any external
capability. No pending feature request was assumed approved. Final exact-source
build is performed after documentation changes before generation completion.

Post-review live smoke check used the final console binary to create/read a fresh
runtime/note and run child.cxe. The transcript contained "final console revision"
and status37; the launcher returned0. New enumeration remains candidate-tested
until transition. All live task results were consumed.

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

## Supervised task generation, 2026-09-06
The installed inherited console was first run with a script containing
ls seed/user/ and exit0. Launcher returned0 and its transcript listed the full
installed user namespace. All runtime script/launch/log files were removed.

New production calls17/18 provide creator-bound lifetime handles, poll/wait/stop,
and automatic supervised descendant teardown. console run/runfor and concurrent
use these generic APIs. Fault/exit status remains full64-bit; forced stop is
distinguished by return2 and console timeout output. USER_ABI.md is authoritative.

Initial compiled fixtures came from job
80b68a517e0d37caaba1a3dac3a62af670f468eaf82f6251ce4bc0a9f845c0d9.
The first kernel link caught a compiler-emitted memset from a large test
initializer; replaced it with initialization of the fields actually read.
The next candidate passed the job suite but console regression detected a
literal backslash-n output mistake. After correction and console rebuild job
e0e6b0a50fe2a456867cd089f9cc256c1cfd774a3013929e764c0c4a575e7144,
the entire candidate boot/protocol passed. The concurrent launcher migration
and its real console failure/timeout tests were built in
9685b735f43b24b1c4517458a2ce8a73841ab7e6fb5b56c591ec1a8228504d79;
the full candidate again passed.

Independent source review found no production blockers, but requested stronger
regressions for immutable-copy cancellation and immediate ownership teardown.
Both were added. Updated jobtest compiled in
252d152890ec882a96de3ed74286ec9886b4056c39bc4b0e5689a9c93f754d19.
The full candidate passed after correcting a test assumption about a1tick
gated owner's transient sleep state. Exact pre-teardown descendant states and
post-teardown resource/edge checks remain mandatory.

The controlled suite gates each owner-exit/fault/stop case, verifies RUN/WAI/ZOM
descendants (including a reservation on a surviving legacy task), and holds the
client before its next launch/exit to require immediate slot/CR3/wait-edge/page
recovery. Stop cases require WAI and long SLP owners individually.
The copy test observes a managed child suspended in kernel context with ir set
and its owner WAI, then kernel-kills the owner under the same interrupt lock.
It reclaims and guards the exact former copy destination page, immediately
reuses both task slots, and requires replacement memory integrity, continuing
independent progress, no old post-read marker and complete resource recovery.
Boot-only fxprobe lengthens the normal read window; no production scheduler,
loader, copy or kill path recognizes a fixture name or behavior.

Candidate boot is new-kernel execution evidence, not replacement of the running
kernel. New17/18 and updated console/concurrent have not been run through live
tools in this generation because the installed kernel is the inherited one.
After transition run seed/user/jobtest.cxe without arguments, then a console
script with runfor and an ordinary subsequent child for useful live confirmation.
Special controlled/copy fixture modes require the boot observer.

Current changed binaries before final reconstruction:
- console.cxe28483 SHA405f3d7363688d82a139293d9c7bfc053afa6c940f358cd382b0b44cfc34b642
- consoletest.cxe24003 SHAafff9fcfd5410bc79890e00b1190a3f0edf922b1fa60f8812d1deb23197d9b73
- concurrent.cxe20131 SHAc65b6d8294e7817588200c3d4c5a35d4bc6f100bdc69646f4eedaa8a15c1da01
- jobtest.cxe6739 SHAbfb8b1a5d7bf2045865f8bae8f20e12aa4a41e60614dcd8d2c6a508333ea2d1f

Request2 remains pending, checked in this generation. Physical interactive Doom
has no new verification. Supplied original assets and Doom binary are unchanged.

### Follow-up review and independent reconstruction
The second independent source review found no meaningful issues and confirmed
that both added regression groups substantively cover the requested scenarios.
It was source-only and did not independently execute tests.

Reconstruction job76d1b72577166743964e218b81bc38d53b09718511221ae7c32a4fd3a5fad949
ran console/verify.sh: sdk/build.sh, doom/build.sh and console/build.sh, packager/
SHA-256/Doom-key/console-core tests, and byte comparison of ALL17 persisted CXE2
executables. Every comparison passed. The only immutable input declared was
doomgeneric-src with SHA93ca655ebfb9cccd2f02e05bf70d5bf1502bef21d09d3d353b9ed7aaceb61fb7.
Exact report is console/rebuild.txt, artifact
65a9556966b1a6d22f680009cdeed3028af5806a10f9d2b8dcdee8f557cfb9e4 (1648bytes).
That snapshot contained117 source files and904247 content bytes, before these
final report/notes updates. User binaries all persist under seed/user; boot
does not depend on retained artifact access.

Final generation validation uses a trusted build of the exact source AFTER
these report/document updates and requires successful compile/link, canonical
READY and development protocol before generation completion. Its result is
recorded in the generation handoff. Physical VGA/input verification remains
unavailable under pending request2; no approval or future arrival is assumed.

## Stable file handle generation

The inherited default jobtest ran live and reaped0. An inherited live scripted
console ran runfor2 spin followed by runfor100 child, recording timeout then
status=37; launcher reaped0. Its three runtime files were removed.

All seven libc-linked installed programs were rebuilt for new calls19/20:
Doom, libtest, inputtest, keylog, concurrent, console and consoletest. This includes
the ordinary Doom port and does not modify the supplied immutable archive/assets.
New programs execute in validated candidate boots; build does not update the
still-running kernel. The rebuilt Doom itself has not been executed in this
generation, so inherited concurrent demo evidence remains historical evidence
for the earlier executable. Physical interactive Doom remains unverified.

Stable stream semantics required one console regression update: a script that
removes its transcript now continues successfully and can remove its next
target. The transcript's unnamed object is reclaimed at close/exit. The previous
expectation that pathname removal causes a stream write failure is obsolete.
The1MiB output-cap test continues to require failure before dispatching a mutating
command whose echo cannot finish. All existing render/transcript/foreground/
timeout/supervision and other boot suites pass with the new streams.

New libtest and boot observer evidence is documented in ENGINEERING.md's stable
handle sections and sdk/libc/README.md. Tests cover competing independent append
writers with512 complete records, stable file identity, limits/failure behavior,
cleanup on every task termination route, full namespace pinned replacement, and
both cancellation and successful resumption of preempted immutable handle reads.
The resumption test proves a separate ordinary task actually reordered the
namespace before the reader completed. Count, every copied byte, position and
complete resource recovery are checked.

Independent source review found no correctness issues and suggested the
successful-resumption companion test; implemented it and passed the full
candidate boot. Review does not prove execution or external provisioning.
Final reproducibility report and final build status follow below.

Final reconstruction job8825f5b1a6095d56b95c04f686161db7cab3a701b076a1616e5df108b3ca0aec
matched ALL17 installed executables byte-for-byte. Packager/SHA, Doom keys and
console core tests passed. console/rebuild.txt is the exact report, with hashes
for every binary and the118-file958106-byte source measurement before final
provenance notes. Report artifact743044fb031e67b45dbecbbd431901556b2001e818ed3f7c3dab9111a15ac225,
1648 bytes. No artifact is required to boot: all programs persist under seed/user.
Final report updates are included in the final trusted build before transition.

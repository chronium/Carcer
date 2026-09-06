# Observed validation, 2026-09-06

## Live workload execution
The inherited kernel already supports the ordinary loader, arguments, waits,
file reads/writes, display and preemption. New syscall14/15 functionality was
validated in candidate boots, not installed into that live kernel by build.

- Imported supplied hello immutably; launcher/hash returned0 and its65-byte
  output matched asset SHA98c93ebeed3327a8973b27c89d79dd4c464e73ce0f8954f708f13dc9c1f0128e.
- Imported supplied doom-wad immutably to runtime/doom.wad. Its original asset
  remains unchanged. The large serial import took several minutes; keep individual
  development reads small. It completed successfully without a new capability.
- Initial source-built Doom ran embedded demo1 to120 presented frames, returned0,
  and wrote256000 bytes to runtime/frame.xrgb. A tool-side PNG conversion of this
  userland buffer showed the level, player weapon and HUD. This was not a trusted
  observation of VGA scanout or input, and does not prove interactive play.
- Final349099-byte Doom ran demo1 to600 frames via the generic concurrent
  supervisor beside an ordinary finite non-syscalling CPU benchmark. The launcher
  returned0 after both completed. All involved task results were consumed.
- Doom reported input=0 in that live run, as expected with the inherited kernel
  lacking syscall15. The final adapter implements keys but physical input remains
  unverified. Request2 was checked again and remains pending.

Measured guest ticks (100Hz ABI):
CPU benchmark start195801, finish197404, iterations2000000000.
Its volatile multiply/add loop makes no syscalls or voluntary yields.
Final value4899245427888663553 independently matched exponentiation of its
affine recurrence modulo2^64; this validates the reported computation.

Doom started195802. Progress:
frame1=195808,100=197301,200=197442,300=197585,400=197728,
500=197871,600=198014; total elapsed2212 ticks.
Doom frames1 and100 fall strictly inside the benchmark's uninterrupted user
computation interval. The kernel's inherited spin tests separately establish
preemption of an indefinitely runnable loop that never enters the kernel.

Final320x200 XRGB8888 frame256000 bytes:
9fbe2662f3455c16bf36e0e0ffd85219351dc745705d233e18032c6934f474ca
SHA-256 computed by the ordinary guest SHA utility after both workloads exited.

## Reproduction after a generation transition
Reimport doom-wad to runtime/doom.wad. Runtime files do not persist.
For the concurrent run, write these LF-terminated lines to runtime/launch.txt:
```
seed/user/concurrent.cxe
seed/user/cpubench.cxe
seed/user/doom.cxe
-iwad
runtime/doom.wad
-playdemo
demo1
-cx-frames
600
-cx-log
runtime/doom-final.log
-cx-capture
runtime/frame-final.xrgb
```
Run seed/user/launch.cxe and reap its returned task ID. Then read the log,
frame and runtime/cpu-result. The CPU result is four LE64 words:
start tick,end tick,iteration count,final recurrence value.
Times and frame hash above are observations of one run, not universal oracles
for scheduling-dependent repetitions. Use fresh output paths if retaining logs.
concurrent launches arbitrary programs; its source contains no Doom path.

## Builds and source review
Final userland compilation job:
f240446320a548c135a4f050c1aaa69ae0f25881ec1f91b2cf5090d328af0a6a
Artifacts (opaque IDs, all persisted binaries also present under seed/user):
doom.cxe 349099 10f3fab9ba8205d2d4bb2dbbb3317fb7515a5332d339653eebcae83bd852a523
libtest.cxe 24675 77397d5aae1d6201ab120afaca143736ddcbeba8199e8f1b1fc1c69c55b8bd0c
inputtest.cxe 33763 6990baa1e6af2aaf12f4922ab30773b5c2a06acc2c9376cfe1a6a3b454b6547c
keylog.cxe 20291 45e87c7aca20d40242f9381b1220ee23380c8ab40fe80ec9143c601ba7785714
concurrent.cxe 20131 5d4658009eba8163239bcadfe5630116f55242033788d6d6e1a9ca1fa9993fd8
cpubench.cxe 894 3d6a134b86a3bac0468d987e6e69022f80ef0f3198971e7c53bc3ca2e116c301

Independent final rebuild/comparison:
79f713e9efa493fb53c27ba9549ea3d88825f0627015b95eab3eccf496f41cdb
rebuild.txt records actual SHA-256 values and the snapshot sizes before final
documentation changes. All13 binaries matched byte-for-byte; packager, SHA and
Doom key-translation tests passed. Source build command is doom/build.sh with
declared immutable doomgeneric-src; the existing sdk/build.sh remains generic.
Bootstrap execution is approved and remains subject to service limits.
No complete host libc, DOS runtime, native guest compiler or network was assumed.

Trusted candidate builds passed compile/link, canonical READY and protocol with
all inherited and new boot regressions. Initial build failures were repaired:
the inherited256KiB data assertion, and a warnings-as-errors indentation issue
during early library compilation. One early dependency audit exceeded diagnostic
limits; a bounded report succeeded. Failed diagnostics granted no extra resources.

First independent source review found no blocking issue; scanf EOF and literal
precision overflow findings were corrected and tested. Its requested input
syscall boundary tests and Doom keyboard adapter are now implemented. The review
was source-only and did not independently execute workloads or verify hardware.

Second source review found no blockers and identified incomplete numeric-token
acceptance in scanf. This was corrected with explicit width-bounded token
recognition and regressions. The final rebuild and live evidence above include
that correction. A bootstrap source capture/build was also in progress during
the final concurrent run; existing serial snapshot capture disables interrupts
and can delay all tasks. These timings establish overlapping progress, not a
real-time or frame-rate guarantee. An earlier pre-correction600-frame run without
that concurrent capture took918 guest ticks; its benchmark interval139123..139407
also included Doom frames1 and100. The final349099-byte binary is the one retained.

All supplied original assets are unchanged. The binary, source adaptations and
library are separate guest-authored outputs. This result is a rendering/userland
milestone; interactive Doom and physical keyboard/VGA validation are not claimed.

## Successor generation live check

The source-built adapter now discards retained launch-era history at startup.
A live ordinary console -> generic concurrent supervisor -> Doom/CPU run returned0.
Doom reported input=1 and600 frames; benchmark ticks109206..109464 overlap Doom
frame1 at109213 and frame100 at109411. Benchmark output matched the independently
computed recurrence. Details, final binary IDs and transcript are recorded in
console/VALIDATION.md. The historical input=0 results above describe the earlier
live kernel. Physical delivery/interactive play are still unverified; request2
remains pending. Supplied assets remain immutable and kernel scheduling generic.

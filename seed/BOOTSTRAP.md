# Bootstrap development bridge

## Implemented tools

These are guest-owned tools advertised by tools.c. Discovery is fixed for each
fresh session, so a validated generation transition is needed before newly
advertised tools become callable. They are development tools, not user syscalls.

- bootstrap_job(json): one UTF-8 JSON request argument, 1..12,000 bytes, no NUL.
  The guest forwards the exact bytes and a consistent current v1 source snapshot
  as two host-service arguments. It does not parse or rewrite JSON. Strict schema,
  asset hashes, artifact authorization, job admission and resource enforcement
  belong to the provisioned trusted service. Invalid JSON is not authorized by
  passing guest transport validation.
- read_bootstrap_artifact(id, offset, length): opaque UTF-8 ID, 1..255 bytes, no
  NUL; canonical unsigned decimal offset and length, no signs or leading zeroes.
  Offset fits uint64; length is 0..1,048,576; offset+length must not overflow.
  A successful response must contain exactly the requested bytes. Host failure
  status and diagnostics pass through unchanged.
- import_bootstrap_artifact(id, length, path): the same ID rules; length is a
  canonical decimal count in 0..33,554,432; path uses ordinary RAM-file rules.
  Reads [0,length) in chunks of at most 1 MiB, stages privately, then publishes
  one ordinary mutable file. The destination must be absent both initially and
  at commit. Failed/short reads, denied access and destination races leave no
  partial import and do not overwrite another file. A zero-byte import still
  issues a host read to establish access. This tool imports a requested PREFIX;
  it cannot determine the total artifact size. Use the length in trusted artifact
  metadata to import the whole artifact. It does not independently hash content.

Import never changes the retained host artifact. Imports are mutable RAM files,
unlike sealed imports of provided assets; importing never relaxes an existing
file's immutable flag. Non-seed runtime files do not survive a generation.
Importing under seed/ includes the bytes in later source snapshots and remains
subject to source count/content/framing limits. Import performs no extraction,
executable conversion, relocation or execution.

## Transport and concurrency

All development serial traffic belongs to task0. Host request correlation IDs
are shared with the inherited build/provided-asset bridge. Calls are synchronous
and sequential; there is no concurrent host request multiplexing or cancellation.

Bootstrap snapshot measurement and emission run with interrupts disabled so
user file writes cannot change the source halfway through capture. Artifact
allocation, publication and cleanup have short filesystem/allocator critical
sections (publication copies the requested bytes). The wait for host execution
and artifact response bytes does not disable interrupts. Normal user tasks can
be scheduled during that wait. Large allocation/copy and serial snapshot emission
can still delay preemption; this is not broad kernel preemptibility.

Responses require canonical protocol framing and matching request ID/type.
Mismatched but well-framed responses are drained and reported as failures.
A bad magic/version or frame length beyond FM halts this serial transaction,
matching inherited protocol behavior: arbitrary binary streams cannot be safely
resynchronized. Successful artifact reads with an unexpected size are drained
and fail before publication.

## Validation

bootstrap_tests.h is included only by bootstrap.c. boottest runs after filesystem
initialization, before scheduling. It uses in-memory peers with the production
request/response/import code, and submits no real host requests. Coverage includes
exact job envelope and snapshot argument framing, maximum snapshot arithmetic,
decimal overflow, range limits, invalid UTF-8/NUL, binary relay, diagnostic status,
wrong request ID/type, unknown status, short/long reads, multi-chunk import,
existing destinations, a destination race, later-chunk denial/short read, and
empty authorized/denied reads. File count and physical-page availability recover.

bootlive runs after tinit. It loads two ordinary user images through tuser:
an infinite non-syscalling loop and an unrelated program that exits with 37.
Both tasks are created with interrupts disabled, and the simulated slow response
reader restores interrupts at the start of its eight-tick wait. Before returning
any response byte, it disables interrupts and verifies that the unrelated program
finished and the infinite loop remains runnable. Completion outside that reader
cannot satisfy the check. The kernel does not explicitly yield in that reader. Both task resources are
then recovered. Existing scheduler, FP, loader, memory and wait tests remain.

These in-memory tests validate guest framing/import behavior and scheduling
during a simulated response. Subsequent real jobs exercised admission, execution,
artifact production, artifact read and import through the callable guest helpers.
Cross-generation host artifact retention has not been independently tested.
See sdk/README.md for retained artifact metadata and compiled boot regression.

## Provisioned scope and dependencies

The optional bootstrap service is configured with the pinned GCC container
specified by the operator, declared asset/artifact inputs, captured source at
/inputs/source/seed/, /work as cwd, and declared regular outputs below /work/out.
The operator's stated execution, input, output, retention and resource limits
remain authoritative. The TCC source asset is immutable source, not a runnable
guest compiler. Request #5 is approved: bootstrap execution permits new jobs
under the stated service limits. Zero retained jobs was not a denial of execution.
The operator says four jobs was the requested batch, not an enforced harness quota.

Request #3 is approved with operator note "fulfilled within the documented
freestanding scope." Guest-owned startup/syscall support, small runtime,
compilation scripts and ELF-to-CXE2 packaging in sdk/ have produced boot-tested
ordinary user binaries. This uses the provisioned host executor; it is not a
guest-native compiler port or full libc. The supplied DOS executable has not run;
the operator permits a source port, and DoomGeneric now runs through this pipeline.

Important observed host convention: outputs entries are relative to /work/out.
For /work/out/spin.cxe declare "spin.cxe", not "out/spin.cxe". The latter compiled
successfully but failed artifact collection with unsafe_output/no such file.
Files and scripts in captured seed/sdk/ are read from /inputs/source/seed/sdk/.
The helper returns trusted JSON as a UTF-8 payload, including job status,
diagnostics and artifacts [{id,name,size}]. Use id and size from that metadata
for exact binary imports; do not assume an opaque ID encodes a content hash.

Request #2 for display observation/input injection remains pending. The immutable
DOS/4G/Watcom DOOM.EXE remains unsupported by CXE loaders. Any compatibility path
would be optional generic userland work. It is not required for the Doom milestone;
the source-built route and immutable inputs are documented in doom/README.md.

Request #6 is approved: run/reap/import_provided_asset are callable in this
session. Live run of the inherited compiled report returned a task slot; reap
returned 42. A small supplied hello asset was imported and rejected mutation.
Argument launch has now been verified live with SHA and Doom demo workloads.
New kernel calls14/15 remain candidate-tested until generation transition. LAUNCH.md documents a file-driven
userland launch route that uses the existing run(path) binding.

Operational note from the operator's abandoned-generation report: a progressing
large read exceeded a harness serial receive deadline and closed that bridge.
Use small source/read replies (this generation used <=6000 source bytes per
read and small binary artifacts). Per-service legal maximum lengths do not
establish that every serial transfer fits the external deadline.

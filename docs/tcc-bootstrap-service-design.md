# Feature #3: Linux bootstrap jobs — design, not provisioning

Provide pinned upstream TCC source and a narrow Go host service that can build
and execute guest-authored **Linux** tooling in disposable rootless Podman jobs.
Compilers, modified compilers, converters, SDK sources, and runtime choices are
the implementor's work. Carcer treats their outputs as opaque bytes: it neither
implements nor needs knowledge of CXE2. Native guest compilation and self-hosting
are optional later milestones. Bootstrap facilities alone do **not** fulfill the
request for a complete guest-runnable compilation pipeline.

This change contains feasibility scripts and documentation only. No production
service, seed/guest changes, feature-ledger decision, or live provisioning is
included. The standalone copy is `/shared/tcc-bootstrap-service-design.md`.

## Fit with the existing Go harness

| Existing mechanism | Reuse / boundary |
| --- | --- |
| `internal/store/ProvidedAssets`, `provided-assets.json` | Operator supplies an immutable TCC tar asset. Existing IDs, digests, append-only revisions and 1 MiB range reads apply. A new upstream revision gets a new asset ID; never change an existing ID. |
| `internal/guest` v1 snapshot and capture | Capture guest-owned `seed/` inputs under the current run's content budget, 128-file and 255-byte-path bounds. Verify and freeze exact bytes at job admission. Never mount the mutable guest source tree. |
| `internal/build/CodexOSHostServices` and serial dispatcher | Add one synchronous job service and one artifact-read service in the existing scoped development exchange. Keep one outstanding request, correlation, bounded duplex transport, review quiescence and delivery checks. |
| Trusted `build`/`finish_generation` | Unchanged fixed compiler operation, candidate boot proof and exact-snapshot finish check. A bootstrap artifact is never implicitly a validated successor. |
| Run/generation archives, Git and forensic evidence | Reuse staging/atomic-publication conventions and digest identities. A small new job manifest/reference record is needed; today's kernel/ISO staging and provided-assets ledger are not a generic writable artifact store. |

The proposed capability needs its own explicit operator enablement at a validated
inactive gate, with pinned image/source identities and reviewed limits. Merely
approving feature #3 must not enable it. Implement a concrete Go owner, not a
resource registry, plugin framework, execution backend abstraction, or job queue.
Job admission is development-only: unavailable during review, candidate validation,
background serial handling and the read-only exit interview. Immutable artifact
reads can use the same explicitly authorized scopes as provided-asset reads.

## Proposed v1 interface

`bootstrap_job` has exactly two host-service arguments:

1. UTF-8 JSON, at most 16 KiB, rejecting unknown/duplicate fields: version `1`,
   `argv`, `assets`, `artifacts`, and `outputs`.
2. One complete source snapshot validated with the run's effective source budget.

Example shape (IDs/digests below are illustrative):

```json
{
  "version": 1,
  "argv": ["/bin/sh", "/inputs/source/seed/bootstrap.sh"],
  "assets": [{"id": "tcc-0fb54300", "sha256": "<pinned digest>"}],
  "artifacts": ["<run-scoped immutable artifact ID>"],
  "outputs": ["compiler.tar", "program.bin"]
}
```

The trusted runner constructs Podman's argv directly. Guest argv executes only
**inside the container**; a shell/script there is permitted. Guest input cannot
select host commands, image, container flags, mounts, credentials, environment,
network, IDs in other runs, or limits. Maximum 32 argv entries, 1,024 bytes each,
8 KiB total, valid UTF-8 without NUL. The fixed working directory is `/work`;
guests can create directories and change cwd within their Linux tooling.

Inputs are immutable mounts: source at `/inputs/source/seed/`, explicitly selected
frozen asset files at `/inputs/assets/<id>`, and authorized prior artifact files
at `/inputs/artifacts/<id>`. Working copies, extraction, patches, and tool execution
happen in `/work`; nothing auto-extracts an untrusted archive on the host. The
container sees only private staged copies, never asset-origin directories,
harness checkout, home, experiment storage, or mutable guest-source paths.
Resolve IDs through trusted records; never interpret guest IDs as host paths.

Outputs are explicit relative regular-file paths beneath `/work/out`, at most
32, each at most 16 MiB, at most 32 MiB total. Paths are UTF-8, at most 255 bytes,
with no empty/dot/parent components, absolute paths, NUL, or line breaks. Reject
symlinks, hardlinks, devices, FIFOs and sockets. Do not walk or import undeclared
output trees. A guest can package a compiler/sysroot as one opaque archive file;
Carcer does not interpret its internal executable or SDK format.

On guest command exit, a trusted container supervisor reports status over a
private control channel and holds the container alive. This status is not proof
of artifact correctness. Freeze the container cgroup before capture, export the
declared files into private host staging with hard byte/file/time bounds, then
kill/reap the whole container and atomically publish verified blobs + manifest.
Freezing stops descendant writers too. Do not stop the container before capture:
tmpfs output disappears at stop. Never publish while guest processes can mutate
captured files. A pause/capture/cleanup error publishes no successful artifact.

Here `podman cp` cannot see the job's tmpfs. The prototype instead exports
**known fixture files** from `/proc/<container-init-pid>/root` using `podman
unshare` while paused. Production needs a collector that verifies process/mount
identity, opens beneath a pinned directory without following links, rejects
non-regular files/hardlinks, and enforces independent byte/file/time bounds.
The fixture's raw tar pipeline is not a hostile-output security boundary.

Status `0` returns a bounded JSON manifest of job ID, immutable artifact IDs,
original logical names, sizes and SHA-256 values. Status `1` means workload
failure (including nonzero exit, signal, memory/PID/storage/diagnostic limit or
wall timeout); `2` means invalid request, busy/quota denial or trusted runtime
failure. Return a machine-readable reason and bounded diagnostics; distinguish
exit, OOM and deadline observations without guessing from stderr. No artifacts
are published on failure, and prior successful artifacts remain unchanged.

`read_bootstrap_artifact` takes `(artifact_id, offset, length)`, using the existing
provided-asset canonical unsigned-decimal/range semantics, maximum 1 MiB per
read, exact reads with no silent truncation. It permits only artifacts authorized
for the current run/generation lineage. Job results already enumerate outputs,
so a general filesystem/list/upload API is unnecessary. Guest helpers read chunks
and import bytes into guest-managed storage themselves; no direct host write into
the guest tree, automatic installation, or automatic executable loading occurs.

## Cancellation and lifecycle

One job at a time, no queue or detached guest jobs. Acquire both run and global
admission slots before capture; otherwise return `busy`. The synchronous service
response stays correlated with the invoking guest request. No PRISON/2 or async
serial-tool redesign is proposed.

Enforce a 180 s job deadline, independent of client connection lifetime. Stop,
operator abort/pause, session cancellation, guest disconnect and generation
retirement must cancel the job: TERM, 2 s grace, KILL the whole container cgroup,
then wait up to 5 s for confirmed teardown. Never release the admission slot or
report a frozen gate until cleanup is confirmed; cleanup failure blocks further
jobs/transitions and surfaces an operator error.

Today live operation serialization can make pause/abort wait behind an invocation.
Implementation therefore needs a small cancel handle reachable **before** waiting
for that operation lock. Merely passing a timeout inside the host-service handler
would not provide prompt operator cancellation. Preserve the existing generation,
review yield/quiesce/resume and delivery lifecycle; add cancellation regressions
at these boundaries. There is no concurrent guest cancellation request in v1.

## Ownership, capacity and persistence

The pinned TCC archive is an immutable upstream asset, not mutable run source.
A job can unpack it into scratch and apply guest-authored patches/replacements.
Persistent modifications and handwritten SDK sources normally live in the
captured source snapshot and count toward its existing 64 KiB / provisioned 1 MiB
content budget and file-count limit. No run capacity change is part of this design.

The upstream tar alone is about 4.8 MiB and contains more than 128 regular files:
copying the entire TCC tree into today's source snapshot does not fit. Scripts,
patches and selected replacements can reference the immutable base. If a chosen
SDK or modification set does not fit, the implementor/operator must make an
explicit storage decision; the bootstrap service must not silently raise source
limits or prescribe a guest layout.

Generated sysroots, Linux compilers and converter outputs can be immutable job
artifacts. They consume a **separate, explicitly approved artifact byte quota**,
not source-snapshot bytes. This also permits source-like generated output: the
host cannot reliably classify arbitrary bytes as source versus binary. Therefore
retention is an additional bounded storage capability, not a claim that the source
budget bounds all guest-controlled state. Importing bytes into `seed/` makes them
subject to source capacity on subsequent captures.

Record image/platform identity, upstream asset IDs/digests, exact snapshot hash,
input artifact IDs, argv, limits, timing, exit/termination reason, bounded log
identity and output hashes in a durable job manifest. Inputs must be captured
before execution; host paths and secrets never enter the guest-visible manifest.
Generation archives freeze the referenced job/artifact set without duplicating
blob bytes. The run store owns immutable blobs and committed references; restart
revalidates sizes/digests and reaps incomplete jobs before admitting work.

At continuation, authorize the selected completed parent's retained artifacts;
at rollback, authorize the selected parent's frozen set, not artifacts belonging
only to discarded later lineage. Preserve historical refs, so archived generations
remain inspectable. No automatic cross-run artifact access: future inheritance
must explicitly copy referenced blobs, validate destination quotas, and atomically
publish their identities before starting. Until that extension exists, reject
bootstrap-dependent cross-run inheritance clearly rather than leave dangling IDs.

Retain successful manifests/blobs while referenced by a live or archived
lineage. Admit no more jobs when byte/object limits would be exceeded; do not
automatically evict a compiler or sysroot needed after restart. Reclaim only
unreferenced blobs at an inactive gate, with an explicit operator action and
record. Bound failed manifests/logs separately; no writable persistent container
volume, hidden incremental build cache, or automatic artifact-to-asset promotion.

Missing guest facilities are an invocation helper that can serialize the two
arguments, asset/artifact chunk readers, binary-safe writes/import, and deliberate
persistence of the files/IDs it will need after reboot. Extraction, patches, Linux
build scripts, SDK/runtime implementation, target ABI work and executable packaging
remain guest-owned. Current generic asset reads do not themselves supply these
facilities or a guest-runnable compiler.

## Proposed limits and isolation

| Resource | Initial proposal |
| --- | --- |
| Admission | One job/run and one job globally across harness instances; reject busy, no queue |
| Per-job CPU / memory / swap / PIDs | 1 CPU quota, 512 MiB memory, zero swap, 64 tasks |
| Aggregate job slice | 1 CPU, 768 MiB memory, zero swap, 96 tasks including job-side overhead; dedicated job owner, not the live VM slice |
| Time | 180 s execution; 15 s capture; TERM 2 s then KILL; teardown verification 5 s |
| Writable scratch | `/work` 256 MiB tmpfs, `/tmp` 16 MiB tmpfs; both charged to memory; root image read-only; no other writable disk volume |
| Process rlimits | 128 open files, 64 MiB maximum regular file, zero core dumps |
| Diagnostics | 64 KiB combined stdout/stderr; stop on overflow, drain/cancel boundedly; no unbounded container log file |
| Captured mutable source | Current per-run snapshot budget + framing; unchanged count/path limits |
| Mounted inputs | At most 8 immutable assets/artifacts, 64 MiB total + current snapshot; reject oversized admission |
| Published outputs | 32 files, 16 MiB/file, 32 MiB/job |
| Retained artifacts | 128 MiB/run including manifests/logs, 256 blobs, 64 successful job manifests; reserve worst-case output budget before starting |
| Failure evidence | At most 32 records and 2 MiB/run; bounded oldest-failure rotation, never successful referenced blobs |
| Shared storage | 512 MiB across runs including blobs, metadata, logs and private staging, plus one pinned read-only image (this feasibility image is 1.45 GB unpacked); admission reserves capacity globally |

Use a separate rootless OS job owner with no experiment permissions. The control
process owns Podman; containers receive no control socket, credentials, host home,
network, host PID/IPC/UTS namespace, host devices beyond standard inert container
/dev entries, or host directories except private read-only input copies. Drop all
capabilities, set no-new-privileges, keep default seccomp and SELinux enforcing,
disable automatic proxy forwarding, clear image/host environment and set only
fixed PATH/HOME/LANG/TMPDIR. Never allow privileged mode or host namespace flags.

Private executable tmpfs permits guest Linux binaries. SELinux's executable-memory
policy remains active: TCC's default `-run` failed here; upstream
`--with-selinux` uses file-backed executable mappings and requires writable `/tmp`.
Do not disable SELinux to make arbitrary JIT behavior work. Container isolation
shares the host kernel and is not a VM security boundary. Runtime vulnerabilities
remain a risk; pin/update the runtime/image under operator control and require
independent review before exposing hostile jobs.

The feasibility uses the current unprivileged account and task-only systemd
slices, not a provisioned dedicated production job owner. Global admission,
retention accounting, safe export, crash recovery, run authorization and prompt
operator-cancellation wiring remain implementation work. Verify controller
values and namespace/mount policy at service startup and refuse enablement if
required controls are unavailable; rootless flags accepted by a CLI are not proof
that their controls are delegated or enforced.

## Feasibility, remaining decisions and bounded implementation

Pinned inputs, exact commands, tested targets, control results and cleanup are in
the [feasibility record](feasibility/tcc-bootstrap-2026-09-05.md); the reproducer is
[`prepare.sh`](../scripts/tcc-bootstrap-feasibility/prepare.sh) and
[`run.sh`](../scripts/tcc-bootstrap-feasibility/run.sh). Neither script opens an
experiment. Downloading inputs is a trusted network-enabled acquisition step;
all compiler/execution/probe containers use the pinned image with `--pull=never`
and `--network=none`.

Before implementation, confirm the concrete quotas/retention policy above,
provisioning ownership of the dedicated account/global slice, and whether
cross-run artifact copying is required for the first release. Decide the
container supervisor/control channel (including protection from guest forgery or
termination) and review the bounded output collector.
The full compiler image is convenient evidence, not a claim that a 1.45 GB image
is the smallest production image. Any smaller replacement needs its own digest
pin and the same feasibility/control verification. No decision about a guest
executable format, compiler backend or native self-hosting is required here.

Bounded implementation plan (separate future authorization):

1. Add disabled-by-default gate configuration, pinned input identities, strict
   request validation, reservations and the concrete job owner; test rejection
   before process admission and immutable snapshot capture.
2. Add rootless job lifecycle, preflight/controller checks, synchronous exchange
   cancellation and a trusted completion/freeze boundary; test timeout, noisy
   logs, fork/OOM/storage failures, operator cancellation and leak-free cleanup.
3. Add bounded safe export, atomic artifact/provenance publication, exact range
   reads and generation reference persistence; test malicious paths/links,
   overflow, interrupted publication, restart, rollback and quota exhaustion.
4. Integrate only the two scoped host services, feature availability reporting,
   and required guest-helper contract documentation. Exercise disposable
   end-to-end calls; retain existing build/finish/review/delivery tests. Review
   separately before any live provisioning or feature-ledger decision.

Primary references: the [TCC project](https://bellard.org/tcc/), the
[upstream repository](https://repo.or.cz/tinycc.git) at commit
`0fb54300b56512754221d80adda85ddb9815bceb` and its `README`, `configure`,
`tcc-doc.texi`, and `tccrun.c`; and the
[Podman run documentation](https://docs.podman.io/en/stable/markdown/podman-run.1.html).
Capability claims in the evidence are limited to the actual probes, not inferred
from upstream's general target or language list.

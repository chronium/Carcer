# Go bootstrap host services

These optional services use the existing v1 host-service framing and correlation
rules. They require no permanent-seed change. Provisioning is described
in [operator setup](bootstrap-service-operations.md). Request #3 remains separate
from both provisioning and fulfillment of a guest-runnable compiler pipeline.

## `bootstrap_job`

Exactly two arguments:

1. UTF-8 JSON, at most 16 KiB. Version 1 accepts only `version`, `argv`, `assets`,
   `artifacts`, `outputs`; unknown and duplicate fields, including nested
   duplicates, are rejected.
2. The exact current source snapshot, using the existing per-run content budget,
   file-count/path bounds and serialized framing. The host freezes these bytes;
   it never mounts a mutable source tree.

```json
{
  "version": 1,
  "argv": ["/bin/sh", "/inputs/source/seed/bootstrap.sh"],
  "assets": [{"id": "tcc-0fb54300", "sha256": "e696d12b9429faf08a08aeeaffe96769370e5ea50cf98218f45c74956b3b3f18"}],
  "artifacts": [],
  "outputs": ["compiler.tar", "program.bin"]
}
```

Argv has 1–32 entries, at most 1,024 UTF-8 bytes each and 8 KiB total, without
NUL. It is passed to a process **inside the container**, never a host shell.
The fixed cwd is `/work`. Environment is fixed; the guest can set variables in
its Linux script without affecting the worker environment.

Source is read-only at `/inputs/source/seed/`; selected immutable assets at
`/inputs/assets/<id>`; selected authorized artifacts at `/inputs/artifacts/<id>`.
IDs are resolved through trusted records, not interpreted as host paths. At most
8 assets/artifacts and 64 MiB are mounted, plus the current source snapshot.
Copies/patches, generated sysroots and execution happen in private scratch.
The service does not classify opaque bytes as source versus binaries.

Outputs are declared relative regular-file paths under `/work/out`: at most
32, 16 MiB each, 32 MiB total. A path is at most 255 UTF-8 bytes with no absolute,
empty, dot, parent, NUL, CR/LF or backslash components. Symlinks, hardlinks,
devices, sockets and FIFOs fail collection. Directories are not exported;
packaging a tree as opaque bytes inside a Linux job is the guest's choice.
No host archive extraction or automatic guest import occurs. Output contents
are collected only after trustworthy command completion and cgroup freezing.
No successful artifacts are published unless complete teardown is confirmed.

The v1 status is also included in a bounded JSON response:

| Status | Meaning |
| --- | --- |
| `0` | Completed, collected, cleaned and durably published; `artifacts` contains `{id,name,size}` entries |
| `1` | Workload failure, rejected output, diagnostic overflow, OOM/PID failure, cancellation or deadline; no published outputs |
| `2` | Not provisioned, invalid request/scope, busy/quota rejection, worker/cleanup/publication failure; no newly authorized outputs |

Admitted jobs return `job_id` for correlation with their persisted provenance;
pre-admission rejection has no job ID. The response includes `reason`, bounded
diagnostic display text, observed exit code/OOM/resource events, worker executable
digest and controller values when
available. Exit `-1` records a signal/unavailable normal exit code; it is not
invented from stderr. Diagnostics have a 64 KiB bound and are never interpreted
as completion messages. The immutable private job manifest additionally records
origin run/generation, exact source hash and capacity, selected input identities,
argv, image/TCC pins, limits, timestamps and output identities.

One job is admitted at a time globally, without queuing. Admission exists only
inside an active development invocation, including the implementor's planning
or implementation phase; background/startup handling, candidate validation,
review capture and exit interviews cannot start jobs. Guest helper discovery
remains fixed for one session. `bootstrap_job` is a recognized dynamic tool only
if advertised by the guest; its guest-tool argument is the JSON request string,
and that guest helper must capture/append the source snapshot itself.

Operator pause/abort/shutdown and invocation cancellation reach the job's cancel
handle independently of the live operation mutex. The host-service response
uses the existing serial/dynamic-tool delivery path. No concurrent guest-side
cancel request, detached job, automatic successor, or async protocol redesign
is introduced. A paused generation may resume normally; failed cleanup requires
recovery before retirement/publication can succeed.

## `read_bootstrap_artifact`

Exactly `(artifact_id, offset, length)`, where offsets/lengths are canonical
unsigned ASCII decimal. Length is at most 1 MiB and the entire range must exist;
o truncation occurs. Offset equal to size is valid only for a zero-length read.
Status zero returns **raw bytes**, not JSON. An unknown or unauthorized ID fails
even for a zero-length range.

Only the current run/generation lineage's committed references authorize reads.
Continuation and restart use the selected parent; rollback excludes later jobs.
Cross-run inheritance copies/verifies selected artifacts before atomic destination
publication and does not automatically enable execution or reads in the new run.
The explicit initial-destination provisioning option makes inherited reads
available before the first ready marker; see the operator setup guide.
The read service has the same explicit background/startup availability scope as
provided assets; review and interview models retain their existing restricted tools.

The guest owns binary-safe chunk retrieval, installation/import and persistence
of any IDs/files its successor needs. The service provides Linux bootstrap
facilities, not a native guest compiler, SDK, executable format, or self-hosting
requirement. Existing trusted `build` and `finish_generation` still require their
own exact-source compilation and candidate boot proof; a bootstrap artifact is
never an implicitly validated successor.

## Advertised guest import helper

The Go tool bridge recognizes `import_bootstrap_artifact` only when the current
session's guest advertises it. It is a mutating, implementation-only tool; planning,
review and interview sessions cannot invoke it. This is a guest helper, not an
additional host service. The harness forwards exactly `(id, length, path)`:

* `id`: opaque UTF-8, 1–255 bytes, no NUL; it need not be a hexadecimal digest.
* `length`: JSON integer 0–33554432, encoded as canonical unsigned decimal on wire.
* `path`: 1–255 bytes of UTF-8 under Generation 9's length-delimited RAM-file rules;
  it is not normalized or interpreted as a host filesystem path.

Generation 9 imports the prefix `[0,length)` in bounded reads, stages it privately,
and requires the destination to be absent both before reading and at commit.
A failed import leaves no partial destination. Zero length still requires artifact
authorization. The helper does not infer full artifact length, unpack or execute it.
The destination is mutable guest RAM state; ordinary source persistence rules apply.
Guest status/output and bridge delivery confirmation retain their usual meanings.

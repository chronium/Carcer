# Validated successors and generation provenance

The immutable generation archive is the source of truth for a completed
CodexOS generation. A generation may finish only from the exact current source
snapshot associated with its latest successful trusted build.

Guest-visible build success includes two mandatory stages:

1. deterministic trusted compilation, linking, and ISO creation;
2. an ephemeral boot of that exact ISO under the current trusted hardware
   profile, followed by the canonical READY marker and a successful read-only
   `list_tools` protocol exchange.

The candidate VM has the same external isolation as the running generation. It
has no persistent writable state, is never archived as a generation, and is
always terminated when validation ends. A compilation-successful image that
does not boot or speak the protocol is a failed guest build and cannot replace
the previous successful build. When a run has explicitly configured provided
assets, candidate protocol validation uses the same frozen in-memory asset
snapshot as the active generation; it does not reread the external directory.
This validation exists because experiment-001
demonstrated that a valid compilation and ISO can still select an unbootable
successor after the generation's development turn has permanently ended. A
healthy same-thread session may remain idle at the immutable gate solely for an
optional read-only exit interview, but it is closed before the successor boots
and cannot change archive or Git provenance.

## Build and review forensic evidence

Harness activity recorded after build/review provenance support was introduced
also has a trusted run-local identity under `build-review-provenance/`. Each
build invocation receives a generation-scoped, never-reused attempt ID before
processing. Its manifest hashes the exact serialized snapshot bytes received by
the builder, then connects the exact produced kernel and ISO hashes and sizes to
candidate QEMU start, READY observation, canonical protocol validation, the
final result, and any latest-success update. These hashes are forensic identity;
they do not replace the exact snapshot-byte comparison required by
`finish_generation`.

The exact source snapshot of each successful attempt is retained in that
attempt's run-local evidence. If a generation with a successful build is
aborted, its archive also receives `latest-success.snapshot` and a compact
`latest-success.json` identity manifest before temporary build storage is
removed. Kernel and ISO bytes are not duplicated there; their hashes and sizes
identify the exact artifacts validated by the attempt. An abort before any
successful build creates no such success record.

Each Luna consultation similarly receives a stable review ID. For every
persistent guest-source `read` delivered to Luna, trusted evidence stores the
requested path/range, result status, returned-byte hash and size, and the exact
returned bytes. This records what Luna actually received without changing its
tools or results. It is range evidence, not an invented atomic whole-source
snapshot, and it deliberately excludes provided-asset reads.

Build evidence is mandatory and fails closed. If trusted provenance storage
cannot allocate an attempt or durably preserve its required source/artifact
identity, the build returns a harness failure and cannot advance or erase
latest-success. Normal guest build acceptance remains the same: compilation,
READY, and canonical protocol validation must still succeed. The additional
failure case is trusted infrastructure failure, not guest build failure.

Review evidence has the opposite operational policy because observation must
not change Luna's consultation. A capture failure leaves Luna's exact tool
result and review outcome unchanged, degrades observability, and durably marks
`evidence_complete: false` whenever storage remains writable. The manifest
records `review_outcome` independently from `capture_outcome`. It may claim
complete evidence only after every referenced source-read file has been
verified against its recorded size and SHA-256. A historical review manifest
without these fields does not imply complete capture.

Manifests and byte evidence are written atomically and remain private run-local
infrastructure: they are not agent context, dynamic tools, metric labels,
generation Git commits, or public release artifacts. Historical generations
that predate this instrumentation remain reopenable but cannot be retroactively
given evidence that was never captured. This is trusted observational
instrumentation and does not change the Agent Contract or autonomous-agent
behavior; the fail-closed build-storage policy protects the audit invariant.

Local Git provenance is a derived human-readable projection of completed
archives. Each run uses the run-directory basename as its tag namespace:

```text
experiment-002/generation-0000
experiment-002/generation-0001
```

These are immutable annotated tags. Each tag points to the derived source
commit whose ancestry follows the archive's actual parent generation, and its
annotation includes deterministic run/generation metadata plus the exact
archived handoff text. Reconciliation reconstructs missing tags from archives
and rejects lightweight, moved, or differently annotated tags rather than
rewriting them.

The legacy `experiment/generation-*` tags published for experiment-001 predate
run-scoped naming. They remain historical and immutable; new recorders neither
migrate them nor use them as parents for another run.

## Browsable lineage branches

Provenance has four distinct layers:

```text
generation archives
    authoritative experimental record

<run>/generation-NNNN annotated tags
    immutable exact generation provenance and handoff

<run>/lineage-NNNN branches
    derived browsable heads for autonomous Git lineages

main
    trusted harness development
```

The initial completed lineage is `lineage-0000`. Ordinary completed successor
generations fast-forward that lineage branch. Each completed rollback starts the
next lineage ordinal and leaves every older lineage head frozen at its last
completed generation. Aborted generations create no source commit or tag, do not
advance a branch, and do not consume a lineage number.

For example:

```text
G0 -- G1 -- G2 -- G3
          \
           G4 -- G5

experiment-002/lineage-0000 -> G3
experiment-002/lineage-0001 -> G5
```

Each lineage branch points directly at an existing generation commit. No merge,
rebase, or synthetic commit is involved. The generation commit parents already
encode successor and rollback ancestry from the archives. Consequently, browsing
a lineage shows the experiment-base repository plus autonomous guest-source
evolution; it does not show current `main`, and merging `main` into a lineage
would corrupt this projection.

Lineage branches are reconstructible browsing aids, not authoritative records.
Reconciliation first validates or reconstructs immutable generation tags and
then creates missing branches or safely fast-forwards the currently growing
lineage. It rejects rewound, sideways, foreign, or unexpected managed lineage
refs and never force-updates or deletes them. Existing pre-lineage generation
tags and commits remain unchanged when their missing lineage branch is added.

The recorder operates only on local refs. A human must explicitly push any
lineage branch, for example `<run>/lineage-0000`, to the chosen authoritative
remote. The harness neither selects a remote nor performs network publication.

```console
git push REMOTE experiment-002/lineage-0000
```

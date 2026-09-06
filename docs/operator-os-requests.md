# Operator-authored OS requests (Go)

Operator OS requests express desired guest behavior. They are advisory input:
Sol may pursue, defer, or decline a request, choose its own design, or request an
external capability through the existing mechanism. Requests never change the
primary objective, block generation completion, or grant resources or permissions.

## Commands

The plain console and TUI accept the same small interface, including while an
implementor turn is active or the generation is paused or at a frozen gate:

| Command | Meaning |
| --- | --- |
| `os-request create TITLE \| DESCRIPTION` | Create an advisory OS request. The first literal pipe separates title and description. |
| `os-requests` | List active and withdrawn requests with applicable report and verification status. |
| `os-request N` | Inspect description, current projection, and full attributed revision history. |
| `os-request withdraw N REASON` | Withdraw a request; preserve its history. |
| `os-request verify N REPORT_REVISION NOTE` | Record explicit operator verification of the exact applicable completion report. |

Creation trims surrounding whitespace. Title and description must contain
non-whitespace UTF-8 text, at most 256 and 4,096 bytes respectively. Explanations,
withdrawal reasons, and verification notes have the same 4,096-byte limit;
completion evidence has an 8,192-byte limit. The ledger permits 128 requests,
4,096 revisions per request, and 16 MiB total. IDs are positive, stable, and never
reused. There is no edit/reopen command: withdraw and create a replacement when
intent changes. Invalid writes leave the prior ledger intact.

The TUI header distinguishes `external pending` approvals from `OS active`
requests; compact mode uses `pN` and `OSN`. Command results label requests as
operator OS input. Inspection escapes terminal controls and displays original
actor, run identity, generation where available, timestamp, and implementor
thread/turn. Operator authorship uses the harness process's OS account; it does
not claim to authenticate a distinct person sharing that account.

The existing `features`, `feature`, `feature-approve`, `feature-deny`,
`request_feature`, and `list_requests` interface remains the separate
**guest-to-operator external capability** ledger. OS request dispositions have
no approval or provisioning effect there.

## Implementor delivery and reports

Go contract version 10 adds `codexos.list_operator_requests` and
`codexos.record_operator_request`. They are trusted harness tools, independent of
guest tool advertisement, and available in planning and implementation. Recording
advisory metadata is a narrow planning exception; source mutation remains denied.
Neither tool is available to the independent reviewer or read-only exit interview.
The reviewer receives the originating turn's labelled advisory context so it does
not invent a requirement to fulfill every request.

`record_operator_request` takes `id`, `revision`, `disposition`, `explanation`,
and optional `evidence`. Dispositions are `pursuing`, `deferred`, `declined`, and
`completed`; completed requires nonblank evidence. The trusted bridge supplies
run, generation, model, thread, and turn attribution. Caller-supplied authorship
fields are rejected. The revision must have been presented as active in this
turn and still match durable state. A successful report returns its durable
revision and report and advances that request in this turn's list view. It does not
pull unrelated new operator input into the turn.

The bridge labels **implementor-reported completed; unverified** separately from
an explicit operator attestation. Verification refers to an exact current,
applicable completion report; it never silently follows later reports. A verified
request is still active until withdrawn. Sol cannot verify or withdraw requests.
Evidence is the implementor's recorded explanation of validation, not an automatic
proof that the behavior works.

Every supported non-interview turn receives a fresh labelled JSON projection:
initial planning, planning retries/resume, the normal implementation transition,
review-result continuations, and manual `agent` continuations. Requests created
or withdrawn after snapshot capture become visible at the next such boundary.
An in-flight report racing withdrawal fails with a revision error. The harness
does not interrupt an active turn, poll for work, send live messages, or start a
turn because a request changed. A boundary is the snapshot capture immediately
before `turn/start`, not the later app-server response.

`operator-request-context/` holds private immutable JSON artifacts. A `prepared`
artifact records the exact projection, ledger revision, per-request revisions,
phase, generation, thread, and turn sequence. A separate `dispatched` receipt
binds its path and SHA-256 to the returned turn ID. An attempt without a receipt
proves preparation only. `agent_operator_requests_presented` events reference the
same binding; neither a receipt nor an event proves that the model acted on input.
A provenance write failure fails the turn rather than silently omitting input.
Snapshots are outside generation archives and are never guest source or handoff
content. Retried turns allocate new artifacts.

## Persistence, lineage, and inheritance

`operator-requests.json` is a separate versioned run ledger with a random run ID.
Updates serialize through the existing concrete runtime owner, a store mutex,
and a file lock, then validate and atomically replace/fsync the complete ledger.
The semantic history only appends: creation text and attribution are preserved.
Repeated identical last store writes with the same actor and expected revision
are idempotent; stale writes cannot overwrite another revision. Missing optional
state means no requests; malformed existing state fails closed. Existing
wire formats, feature states, archives, and source ancestry do not change.

Requests and withdrawal history are run-wide and survive harness restarts, gate
reopening, generation retries, abort, and rollback. A report is applicable only
when it belongs to this run and either the current running/paused generation or
a completed generation in the selected parent chain. At an archived gate, the
same rule follows the archived generation and its parents. Aborted-generation
reports and reports from a branch excluded by rollback remain inspectable history
but never establish current completion; their linked verification is likewise
inapplicable. The latest eligible disposition is shown. Before a stopped run is
opened at its gate, no generation claim is projected as applicable. Request
state neither selects ancestry nor enters the completion gate.

The existing explicit cross-run initialization (`--inherit-from-run` with
`--inherit-from-generation` and the existing ISO/Git arguments) copies the
**whole run's operator request ledger as it exists at initialization**, including
withdrawn requests and historical reports. This is run-wide operator intent,
not a claim that every report belongs to the chosen source snapshot. Original
IDs, creation text, actor run IDs, timestamps, and histories remain exact. New
requests continue the ID sequence; new revisions carry the destination's fresh
run identity. All imported reports/verifications are informational history, so
the destination must make its own evidence-backed report before verification.

An immutable `cross-run-operator-requests.json` copy and its digest bind the
inherited history prefix. Destination loading validates that prefix without
requiring the source run to remain available. Later source changes do not sync.
A source without an OS-request ledger adds no OS-request state. The existing
cross-run manifest and external-capability import rules are unchanged.

## Disposable example

These are console commands for an already explicitly started **disposable Go
run**. They are documentation only; do not paste them into a live experiment.
No request or generation is created by building or testing this feature.

```text
os-request create Interactive shell | Consider a generic userland command interface; choose the design and timing.
os-requests
os-request 1
```

If these are entered during a turn, its next supported continuation receives
request 1. Sol could implement its own shell, request external upstream source,
defer it, or decline. An illustrative tool report is:

```json
{"id":1,"revision":1,"disposition":"deferred","explanation":"First establish generic userland loading; revisit afterward."}
```

If a later `completed` report is revision 3, the operator can inspect its evidence
and explicitly attest or withdraw:

```text
os-request 1
os-request verify 1 3 Observed an unrelated user program launched through the interface in this disposable run.
os-request withdraw 1 The requested behavior has been checked; no further work requested.
```

The numbers are illustrative: use the revisions printed by the actual disposable
run. Verification fails if report 3 is absent, stale, aborted, or outside the
selected lineage. Nothing in this example approves or provisions a capability.

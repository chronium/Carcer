# Go per-run source capacity (request #4)

The Go harness can provision **1,048,576 bytes (1 MiB) of aggregate source file
content** for one run. Fresh seed experiments and legacy runs without a setting keep
65,536 bytes (64 KiB). There is no global default change, compiler provisioning,
or guest/seed modification. Guest-side buffers, snapshot serialization, and tool
adaptations remain the implementor's work. Request #3, PRISON/2, and UI renaming
remain separate.

Request #4 describes serialized capacity. The Go validator counts **content
bytes**, excluding paths and length fields. Provisioning 1 MiB of content therefore
allows more than 1 MiB on the wire. With the unchanged v1 constraints, the largest
framing overhead is `2 + 128 * (2 + 255 + 4) = 33,410` bytes:

| Run content budget | Maximum serialized snapshot |
| --- | --- |
| 65,536 bytes | 98,946 bytes |
| 1,048,576 bytes | 1,081,986 bytes |

The maximum assumes all 128 paths use 255 bytes; actual framing is usually
smaller. No padding is required. Source file count (128), UTF-8 path length
(255 bytes), path safety/uniqueness, invocation argument counts, and 16 MiB serial
payload limits are unchanged. Handoff, review-proposal, diagnostic, provided-asset,
app-server, and QMP limits are independent and unchanged. The source-capture read
requests the remaining content allowance plus one byte to detect overflow; this
still fits existing invocation and transport limits. A guest tool must preserve
its EOF semantics; a host budget does not increase guest buffers automatically.

## Setting and lifecycle

Existing runs change capacity only at the inactive generation gate. A new
cross-run bootstrap can explicitly select its destination capacity before atomic
publication, as described below.

At the plain console or shared console command path:

```text
source-capacity 1048576
status
```

`source-capacity` accepts exactly `65536` or `1048576` content bytes. It requires
an opened, validated `AWAITING_NEXT_GENERATION` gate, no QEMU generation, no
transition or partial generation state, and no retained interview or active
console turn. End an interview before provisioning. Stopped-but-unvalidated,
running, and paused runs cannot be provisioned. The command does not start a
generation or approve a feature request.

The selected budget is atomically persisted and synced in the run's
`source-capacity.json`:

```json
{
  "schema_version": 1,
  "content_bytes": 1048576
}
```

Do not provision by editing this file: use the gate command so history and the
selected source are checked before changing state. Missing files mean 64 KiB;
malformed, unsupported, or symlinked settings fail closed. Each generation archive
from a provisioned run freezes its own copy. Archives without that file retain
the original 64 KiB interpretation. Changing today's run setting never rewrites
an archive or changes its original validation limit.

The budget survives process restart, gate reopening, continuation, and rollback.
The live runtime overrides static build configuration with this run's budget.
Source capture, reviewer snapshot parsing, trusted build/evidence, finish, archive
materialization, completed/aborted archive validation, and Git reconciliation use
the appropriate effective or archived budget. Finish still requires exact byte
identity with the latest candidate that passed build and boot/protocol validation.
Review yield, quiescence, resume, and delivery checks retain their existing behavior.

`status` reports the current budget and serialized maximum; `inspect N` reports
the archived generation's values. `generation_started` and
`source_capacity_provisioned` events record effective content/serialized limits,
and planning receives factual current capacity alongside the existing approved
feature context. This adds no guest coding-style or readability instruction.

Reducing capacity checks the selected successor first and fails without changing
the setting if it will not fit. An older larger archive can still be inspected
or reconciled under its own limit. Selecting it for rollback into a smaller run
fails before boot or transition publication. Cross-run inheritance validates the
source archive under its recorded limit, then checks the selected snapshot against
the destination's budget **before creating destination or staging state**. Cross-run initialization
creates a fresh destination with a 64 KiB default, independently of the source
run. An explicit `--inherit-source-capacity 1048576` selects 1 MiB for that new
destination. Without the flag, an oversized snapshot is rejected with the
65,536-byte destination limit.

The flag accepts `65536` or `1048576` content bytes and requires cross-run
inheritance (`--initial-iso`, both `--inherit-from-*` flags, and Git provenance).
It is rejected for seed-only startup and `--resume-at-gate`; existing runs use the
gate command. Validation precedes destination/staging creation. The selected
setting is synced inside the unpublished bootstrap directory and is published
atomically together with handoff, feature ledger, and provenance. Failure does
not publish a destination setting or partial run. Restart then loads the same
per-run setting; the flag is not needed or accepted on gate reopening.

For example, add the following flag to a new experiment's existing inheritance
launch command to support Experiment 4's expanded source:

```text
--inherit-source-capacity 1048576
```

The source remains the latest validated completed archive, the initial ISO must
match that archive's successor, and the Git base must match its recorded tag.
Unlike reopening an existing gate, an inheritance launch with `--initial-iso`
boots generation zero of the new run. Execute it only when explicitly choosing
to start that experiment. No source run or archive is modified by provisioning
the destination.

Unprovisioned default archives retain the legacy layout and interoperability.
The extra setting/archive file is a Go-only extension; the unchanged Python
harness cannot operate provisioned archives. Use Go for provisioned runs.

## Post-merge Experiment 4 procedure

These are operator actions after merge and any separately required Go-cutover
approval. Development does not perform them. Use Experiment 4's existing run
path, Git repository/base ref, and provided-assets configuration. Do not substitute
a new initial ISO or inheritance opening mode when reopening an existing gate.

1. Let the existing generation reach its inactive gate. In its console, end any
   retained interview, reconcile Git provenance with `git-record`, then `quit`.
   Never run two harness owners against the same run directory.
2. Update the trusted checkout and build the merged Go executable:

   ```sh
   cd /home/chronium/src/CodexOS-go
   git switch main
   git pull --ff-only origin main
   go build -o /tmp/codexos-source-capacity ./cmd/codexos
   ```

3. Reopen Experiment 4 using its existing paths, Git base, provided assets,
   and TUI selection:

   ```sh
   /tmp/codexos-source-capacity \
     --run-directory /srv/codexos/experiment-004 \
     --resume-at-gate \
     --provided-assets /shared/assets \
     --git-repository /home/chronium/src/CodexOS \
     --git-base-ref experiment-003/generation-0000 \
     --tui
   ```

4. At the reopened console, provision first, then record factual host availability
   through the existing feature ledger:

   ```text
   status
   feature 4
   source-capacity 1048576
   status
   feature-approve 4
   feature 4
   quit
   ```

   Confirm the TUI approval prompt when `feature-approve 4` is entered.
   Check that `status` says `Source content capacity: 1048576 bytes (snapshot
   maximum: 1081986 bytes)` before approving. `feature-approve 4` records the
   provisioned host capability; it does not provision it. If request #4 is already
   approved, skip that approval command and still provision and verify capacity.
   The ledger remains the factual approved-request mechanism; the setting,
   provisioning event, status, and trusted planning context specify the exact
   available budget. Guest adaptations are not claimed to be complete.
5. Restart with the same command from step 3. Check `status` and `feature 4` again:
   the budget and ledger decision must persist while the run remains at the gate.
   When the operator explicitly elects to begin the successor, use `continue`,
   then `agent`. An aborted gate requires a deliberate valid `rollback N` choice
   instead. Neither provisioning nor restarting automatically starts a generation.

## Verification

Verification uses disposable directories, synthetic fixed build tools and
QEMU/serial peers, and bounded serialized Go tests. It does not operate a live
experiment or prove guest-side adaptation. See the verification record below for
commands and outcomes.

Verified on 2026-09-05. The initial full/race suites and vet ran at implementation commit
`3a3ff98`. The explicit bootstrap-capacity follow-up is verified separately below. All checks passed:

| Check | Result |
| --- | --- |
| Focused Go suite | 8 affected packages passed |
| Complete Go suite | All 13 packages passed |
| Complete race suite | All 13 packages passed |
| `go vet` | No diagnostics |
| Process-leak audit | No new surviving Codex/QEMU/test processes or marked verification children |
| Documentation/diff | Local links resolve; whitespace checks pass; Python and seed unchanged |

Commands were run sequentially with bounded test/process concurrency:

```sh
export GOMAXPROCS=2
export GOCACHE=/tmp/codexos-go-cache
export GOMODCACHE=/tmp/codexos-go-modcache
export GOPATH=/tmp/codexos-go-path
export CODEXOS_SOURCE_CAPACITY_VERIFICATION=1

timeout --signal=TERM --kill-after=10s 360s go test -p=1 -parallel=1 ./internal/sourcecapacity ./internal/guest ./internal/build ./internal/experiment ./internal/provenance ./internal/store ./internal/agent ./internal/operator -count=1 -timeout=300s
timeout --signal=TERM --kill-after=10s 480s go test -p=1 -parallel=1 ./... -count=1 -timeout=300s
timeout --signal=TERM --kill-after=10s 600s go test -race -p=1 -parallel=1 ./... -count=1 -timeout=420s
timeout --signal=TERM --kill-after=10s 180s go vet -p=1 ./...
```

Focused regressions cover both content boundaries and overflow, worst-case
framing, capture and reviewer/build/finish agreement, large completed/aborted
archive persistence and reopening, Git reconciliation, immutable historical
limits, inactive-gate and retained-interview rejection, live pause/resume,
continuation and rollback persistence, fresh-run isolation, and rejection of
oversized inheritance before creating destination state. An operator regression
checks that provisioning and feature approval remain independent and start no
QEMU process. Existing review lifecycle and delivery tests also passed.

The process audit compared PID/start-time identities against a baseline of 86
processes and independently searched for children inheriting the unique
verification environment marker. It found zero survivors after all suites.
Pre-existing live processes were left untouched.

Opt-in real-image/toolchain acceptance and remote Codex/live-guest trials were
not run. Synthetic peers establish harness behavior, not a working expanded
guest implementation. No live Experiment 4 state was opened, provisioned, or
started during development; post-merge operator provisioning is still required.

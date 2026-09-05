# Bootstrap integration and decision-note verification — 2026-09-05

Code under test: `fddbccc`, on `feature/bootstrap-integration-notes`, based on
main `d29860b`. The branch corrects bootstrap availability context, forwards the
advertised guest import helper, and adds optional operator feature-decision notes.

## Focused regressions

| Boundary | Evidence |
| --- | --- |
| Current capability context | Enabled and disabled configurations with zero retained jobs; enabled wording explicitly permits new jobs without a batch grant; unprovisioned status remains distinct |
| Guest helper discovery and arguments | Advertisement-dependent schema/exposure; exact `(id,length,path)` forwarding; opaque UTF-8 IDs, byte bounds, canonical length, missing/extra/wrong-type arguments and invalid UTF-8 rejected |
| Mutating tool phases | Import denied during planning, review and exit interview; implementation forwards to the guest |
| Result delivery | Real disposable app-server subprocess exchanges confirm delivery for guest status 0 and 1; guest output/status preserved; bridge errors remain delivery failures; activity reports guest failures |
| Decision persistence | Approve and deny, 4096-byte Unicode boundary, malformed/oversized notes, cancellation, atomic status/note publication, restart and finalized-decision protection |
| Presentation and context | Escaped operator notes appear separately in request inspection/history; `list_requests` includes approved/denied notes and fresh approved context carries its labeled note |
| Lifecycle and inheritance | Reopen and successor continuation preserve notes; chained cross-run initialization preserves notes and descriptions, protects finalized inherited notes, and permits a pending inherited request's decision at a destination gate |
| Legacy compatibility | Existing five-field records and canonical cross-run ledgers retain Python/Go conformance; note-bearing records require the updated Go reader |

Generation 9's helper argument contract was inspected read-only. Tests forward
through disposable runtime/app-server fixtures; they do not execute imports in a
live Generation 9 guest. No host importing implementation was added.

## Complete checks

All three commands passed, run sequentially from the repository root:

```sh
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p=1 -parallel=1 ./... -count=1 -timeout=300s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -race -p=1 -parallel=1 ./... -count=1 -timeout=420s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go vet -p=1 ./...
```

Package and test parallelism were one, with two Go scheduler threads and a
768 MiB Go soft memory target. These are runtime/build limits, not a hard cgroup
memory ceiling. QEMU was installed and the ordinary QMP integration was available.

Optional rootless Podman acceptance (`CODEXOS_BOOTSTRAP_ACCEPTANCE`), its long
deadline variant, and real seed toolchain/image acceptance
(`CODEXOS_REAL_IMAGE_ACCEPTANCE`) were not enabled. The pinned-input operator
acceptance was skipped because `CODEXOS_BOOTSTRAP_TCC_TAR` was unset. These checks
were not substitutes for, or validations against, a live experiment.

## Cleanup and scope

A `/proc` snapshot before the complete checks recorded 384 processes. The final
PID/start-time comparison found zero new test executables, QEMU, bootstrap worker,
Podman, conmon, crun or fake helper processes; zero processes matching those classes
remained. The import app-server regression also explicitly closes each server and
waits for its PID to exit.

Only Go harness code, Go tests and documentation changed. Python, guest source,
the permanent seed, finalized interviews, archives, handoffs and live request
statuses were unchanged. No live capability was provisioned and no live generation
was started. Decision notes have no provisioning or enforcement effect.

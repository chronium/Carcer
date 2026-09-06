# Guest task and asset binding verification — 2026-09-05

Code tested: `223a4b2`, on `feature/guest-task-asset-tools`, based on main
`2b9cd38`. The change adds only advertisement-gated, implementation-only Go
bindings for guest `run`, `reap` and `import_provided_asset` operations. Argument
and result contracts are documented in [the tool reference](guest-task-asset-tools.md).

## Simulated bridge and app-server coverage

Focused Go regressions cover each tool's advertisement-dependent schema/exposure,
argument types and field sets, UTF-8 byte limits, argument order, literal guest
paths, uint32 task IDs, and rejection during planning/review/exit interviews.
Result tests preserve task IDs, `running`, zero and full-width exit status text,
nonzero guest tool status and bridge errors.

Disposable fake app-server subprocesses exercise the real session/JSONL delivery
lifecycle for launch, active/completed/fault reap, consumed-task failure and asset
import success/failure. Each exchange retains guest output and status, confirms
`tool_result_delivered`, and closes/reaps its server process. These tests simulate
the guest results and the app-server peer; they are not real Codex sessions.

## Actual guest execution

`TestGuestTaskToolsRealGuest` passed against a temporary copy of Experiment 4
Generation 10's archived successor ISO:

```text
SHA-256: 49a05ba0148cc1c5ec461da1d6badb6021364ddf95d34307fc9292bf6b377e35
QEMU: 10.2.2 (qemu-10.2.2-1.fc44)
Profile: test-v1, q35, kvm:tcg, qemu64, 1 vCPU, 128 MiB RAM
Devices: headless VGA, no NIC, no writable block device
```

The test uses the production serial dispatcher, tool client, Go tool forwarding
and frozen provided-asset host service. Temporary CXE1 binary fixtures execute
through the unchanged guest loader and scheduler. The lifecycle metadata adapter
is a test fixture; no experiment run or Codex session is opened.

Observed results:

- Import copied exact executable bytes into guest RAM; duplicate destinations and
  missing assets failed. A missing asset left no destination file.
- Writes, truncation and removal of the imported file failed; readback remained
  identical. A zero-sized asset was also imported and sealed.
- `run` returned task ID `1`. Reaping the sleeping task returned `running`, then
  `9223372036854775809`, with guest tool status `0`.
- A second reap of the consumed task failed. A subsequent faulting task reused
  slot `1` and reaped `18446744073709551615`, again with guest tool status `0`.
- Missing/empty executable paths and invalid task slots failed in the guest.

The standalone real exercise passed in 13.78 seconds and was also enabled in the
complete race suite. Its input ISO hash was checked unchanged after both runs.
This establishes actual guest execution and import evidence on the reduced test
profile; it does not validate Doom, arbitrary executable compatibility or the
8 GiB/four-vCPU experiment profile.

## Complete checks

All checks below passed, executed sequentially:

```sh
CODEXOS_GUEST_TASK_TOOL_ISO=/srv/codexos/experiment-004/generation-0010/successor/codexos.iso \
GOMAXPROCS=2 GOMEMLIMIT=768MiB \
go test -v -p=1 -parallel=1 ./internal/agent \
  -run '^TestGuestTaskToolsRealGuest$' -count=1 -timeout=120s

GOMAXPROCS=2 GOMEMLIMIT=768MiB \
go test -p=1 -parallel=1 ./... -count=1 -timeout=300s

CODEXOS_GUEST_TASK_TOOL_ISO=/srv/codexos/experiment-004/generation-0010/successor/codexos.iso \
GOMAXPROCS=2 GOMEMLIMIT=768MiB \
go test -race -p=1 -parallel=1 ./... -count=1 -timeout=420s

GOMAXPROCS=2 GOMEMLIMIT=768MiB go vet -p=1 ./...
```

Package/test parallelism was one, with two Go scheduler threads and a 768 MiB Go
soft memory target; the disposable VM had 128 MiB. The Go target is not a hard
cgroup memory ceiling. Ordinary full-suite execution left the real-guest opt-in
unset, while the separate exercise and full race suite enabled it explicitly.
Unrelated rootless bootstrap, long-deadline, real seed build and pinned-TCC-input
operator acceptances were not enabled.

## Cleanup and scope

A `/proc` baseline before the real exercise recorded 385 processes. The final
PID/start-time comparison found zero new test, QEMU, bootstrap worker, Podman,
conmon, crun or fake helper processes; none of those process classes remained.
No `co-task-tools-*` disposable VM directories remained. The real-guest test also
explicitly stops QEMU, waits for its PID to exit and removes its temporary state.

Only Go harness code, Go tests and relevant documentation changed. Archived
guest source, permanent seed, finalized interviews, live experiment state and
request statuses were unchanged by this work. No capability was
provisioned, request #6 was not approved, and no live generation was started.

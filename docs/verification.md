# Harness verification

Go is the maintained harness implementation. Tests live beside their packages
and use disposable state. Existing experiment archives remain supported;
verification must never use a live run as a substitute for a fixture.

## Complete checks

From the repository root, use the Go version in `go.mod`:

```sh
go test ./...
go test -race ./...
go vet ./...
```

On a resource-limited host, run the checks sequentially with bounded concurrency:

```sh
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p 1 -parallel 1 ./... -count=1 -timeout=300s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -race -p 1 -parallel 1 ./... -count=1 -timeout=420s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go vet -p 1 ./...
```

Use `-json` with the test commands to retain package results and explicit skip
reasons. Linux integration tests exercise Unix sockets, process ownership, and
real pseudo-terminals. Some cases require available QEMU or KVM; the generated
source-table regression uses a host C compiler. Missing prerequisites must be
reported alongside the results.

The ordinary suite includes disposable binary and operator lifecycle tests with
standalone QEMU/Codex peers, synthetic build utilities, and fake authentication.
These cover planning, tool delivery, build/finish, pause/resume, abort,
continuation, rollback, gate interviews, terminal restoration, and child reaping.
They do not contact a real Codex session or boot a real guest image. The optional
ordinary QEMU process test uses `-machine none -display none`.

## Opt-in acceptance

Keep these environment variables unset for checks that must avoid graphical
QEMU, real guest images, and rootless-container acceptance:

* `CODEXOS_REAL_IMAGE_ACCEPTANCE`
* `CODEXOS_GUEST_TASK_TOOL_ISO`
* `CODEXOS_BOOTSTRAP_ACCEPTANCE`
* `CODEXOS_BOOTSTRAP_TCC_TAR`
* `CODEXOS_BOOTSTRAP_DEADLINE_ACCEPTANCE`

`TestRealSeedImageBuildsAndPassesCandidateValidation` in `internal/build` is
enabled by `CODEXOS_REAL_IMAGE_ACCEPTANCE=1`. It requires the pinned Limine
inputs, cross-toolchain, bubblewrap, xorriso, and QEMU, and builds and validates
a disposable real seed image. The experiment-profile case requires KVM and opens
a GTK window. Run it only when graphical acceptance is authorized.

Real guest task-tool acceptance and rootless bootstrap acceptance have their own
inputs and prerequisites; see [guest task verification](guest-task-asset-verification.md)
and [bootstrap acceptance](bootstrap-service-acceptance.md). A skipped acceptance
case is not evidence that its external prerequisites or real-image behavior work.

## Archive and process safety

Archive regressions cover legacy completed-generation inspection and gate
reopening without changing archived bytes, immutable archive publication,
validated continuation and rollback, inherited handoffs and feature decisions,
cross-run identity/hash validation, and immutable Git lineage. Legacy records
without newer optional settings remain readable. Extensions such as
[decision notes](operator-feature-decisions.md#compatibility) and
[source capacity](run-source-capacity.md) require readers that support those
recorded settings.

For a process-leak audit, record process IDs and start times before verification,
then compare after all checks have exited. Inspect new surviving test, QEMU,
Codex, compiler, sandbox, and standalone helper processes; PID reuse alone must
not count as a surviving process. Use the tests' recorded child PIDs where
available. Do not terminate unrelated processes or alter live experiment state.

Application-owned TUI shutdown has pseudo-terminal and race coverage. Bubble
Tea's internal renderer/input error or panic path can still force terminal
cancellation; ordinary graceful-shutdown coverage does not establish safety of
that dependency failure path.

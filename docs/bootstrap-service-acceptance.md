# Go bootstrap service acceptance — 2026-09-05

Implementation: `49a3971`, branched from main `313b8ff`. Rootless job execution
was accepted at `7ef9163`; the final storage-only change has fresh focused, full
Go, race and vet verification. The original design's
[feasibility evidence](feasibility/tcc-bootstrap-2026-09-05.md) remains separate.
This record covers the production Go worker/collector/service and disposable
storage fixtures, not the feasibility tar collector. No live run, permanent seed,
Python implementation or feature-request status was changed.

## Inputs and environment

| Input | Exact value |
| --- | --- |
| Upstream | `https://repo.or.cz/tinycc.git` |
| TCC revision | `0fb54300b56512754221d80adda85ddb9815bceb` |
| `git archive --format=tar --prefix=tinycc/` SHA-256 | `e696d12b9429faf08a08aeeaffe96769370e5ea50cf98218f45c74956b3b3f18` |
| Image | `docker.io/library/gcc@sha256:a689e29bc3adf4663ef9a141d23081252764d1319c63f591a027bd6fd676f4c1` |
| amd64 image configuration | `f3d916a4884034b89cb6148781f07d8c92d94b6c6dc1b74dcbec3475d16400da` |
| Host | Fedora 44, Linux `6.19.10-300.fc44.x86_64`, amd64 |
| Runtime | Rootless Podman 5.8.1, crun 1.27, systemd cgroup v2, enforcing SELinux, seccomp |
| Go | `go1.26.7-X:nodwarf5 linux/amd64` |

The source archive is 4,997,120 bytes. Podman reports 1,444,940,831 unpacked image
bytes. Acquisition used anonymous registry access; jobs never pulled or used a
network. Tests created only temporary run/artifact/input directories and named
`bootstrapaccept<PID>` user slices. No VM, app-server session or live experiment
was needed.

## Reproduce, serially

Run from the reviewed checkout on the compatible rootless host. The acquisition
script requires a **new** directory. It fetches only the pinned source/image.
Static compilation is required because the host and image may use different libc
versions. Do not enable Podman acceptance inside the race/full-suite commands.

```bash
bootstrap_accept_root=$(mktemp -d /tmp/codexos-bootstrap-acceptance-XXXXXX)
bash scripts/tcc-bootstrap-feasibility/prepare.sh "$bootstrap_accept_root/inputs"
GOMAXPROCS=2 CGO_ENABLED=0 go test -c -p=1 \
  -o "$bootstrap_accept_root/bootstrap.test" ./internal/bootstrap
CODEXOS_BOOTSTRAP_ACCEPTANCE=1 \
CODEXOS_BOOTSTRAP_DEADLINE_ACCEPTANCE=1 \
CODEXOS_BOOTSTRAP_TCC_TAR="$bootstrap_accept_root/inputs/tcc-source.tar" \
GOMAXPROCS=2 "$bootstrap_accept_root/bootstrap.test" \
  -test.run '^TestRootless' -test.v -test.parallel=1 -test.timeout=600s

GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p=1 -parallel=1 \
  ./internal/bootstrap ./internal/store ./internal/experiment \
  ./internal/build ./internal/agent ./internal/operator -count=1 -timeout=180s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p=1 -parallel=1 \
  ./... -count=1 -timeout=300s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -race -p=1 -parallel=1 \
  ./... -count=1 -timeout=420s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go vet -p=1 ./...

podman ps -a --filter label=io.codexos.bootstrap=1
systemctl --user list-units --all 'bootstrapaccept*'
# After saving evidence, remove only this invocation's temporary directory.
rm -rf -- "$bootstrap_accept_root"
# Remove the exact pinned image only if this invocation acquired it and it was
# not present/used beforehand. Never use system/container/image prune.
```

Actual commands used `/tmp/codexos-bootstrap-acceptance-inputs` and a static
`/tmp/codexos-bootstrap-acceptance.test`. The complete rootless run passed before
the final additive response `job_id` field; its 180-second deadline passed in
180.247 seconds. After that metadata change, the TCC/reuse/failure acceptance ran
again successfully with the job-ID assertion. No execution/cleanup code changed
between those two runs. A later storage-only hardening change made initial
store publication atomic and recovered interrupted initializer/metadata files;
its focused regressions and all Go suites were rerun afterward.

## Results and evidence

| Boundary | Observed result |
| --- | --- |
| Host-service invocation | v1 request IDs/status preserved through real `bootstrap_job` and `read_bootstrap_artifact` handlers; router and advertised guest-tool forwarding covered by Go regressions |
| TCC build and execution | Pinned upstream configured with `--with-selinux --extra-cflags=-O0`; `make -j1 tcc libtcc1.a`; TCC ran a Linux amd64 C program successfully; initial job 2.442 seconds |
| Retrieval/reuse | Exact 512-byte artifact range returned; a fresh successor service imported the opaque compiler archive and compiled/ran another C program |
| Restart/rollback/inheritance | Parent references reopened; later output rejected after rollback; selected compiler copied into another disposable run and executed after explicit fixture provisioning; destination initially disabled |
| Real resource controls | `cpu.max=100000 100000`, `memory.max=536870912`, `memory.swap.max=0`, `pids.max=64`; preflight also checked aggregate 1 CPU / 768 MiB / zero swap / 96 tasks and worker membership |
| OOM/process exhaustion | Allocation workload killed with `memory.events oom_kill=1`; fork workload hit `pids.events max=1`; both failed with no artifacts and confirmed cleanup |
| Noise/deadline/cancellation | Diagnostics stopped at 65,536 bytes; independent 180-second timeout and caller cancellation both tore down all descendants before response |
| Sandbox and collector | Workload UID 65534, zero effective capabilities, no-new-privileges, seccomp, loopback only; unable to signal PID 1 or write protected completion; symlink/hardlink/FIFO outputs rejected |
| Invalid paths/requests | Absolute/traversal/component-link paths, special files, oversized output, duplicate/unknown JSON fields, malformed snapshot, bad ranges and excessive metadata rejected in focused tests |
| Quotas/publication | Run/global reservation rejection; near-128-MiB destination rejection with no destination publication; immutable hash checks; interrupted store initialization/metadata/job staging removed and unindexed successful output remained unauthorized |
| Worker interruption | Killed the actual worker while a job ran; next worker recovered owned container, descendants, staged input directory and active marker |
| Generation lifecycle | Existing full-suite review yield/quiesce/resume, delivery confirmation, archive, continuation and rollback tests retained; inactive-gate checks reject active/retained-interview/missing-pin provisioning |

The Linux resource fixture is ordinary C compiled by GCC in the sandbox; it is
not a guest compiler port. Compiler artifacts can contain source as well as
binaries. The tests do not infer native guest ABI support, SDK completeness,
executable packaging, a CXE2 backend, or self-hosting from Linux TCC success.

Focused tests, the complete Go suite, the complete race suite and vet all passed
after the final storage change. The final audit found no containers, runtime job
processes or acceptance units. Task inputs/binaries and the newly acquired pinned
image were removed after saving evidence. Documentation shell blocks and the
example sudoers entry passed their syntax checks.

Logs are in [the evidence directory](feasibility/bootstrap-service-2026-09-05/):
[full rootless acceptance](feasibility/bootstrap-service-2026-09-05/rootless.txt),
[final response/provenance acceptance](feasibility/bootstrap-service-2026-09-05/job-id.txt),
[focused tests](feasibility/bootstrap-service-2026-09-05/focused.txt),
[Go suite](feasibility/bootstrap-service-2026-09-05/go-suite.txt),
[race suite](feasibility/bootstrap-service-2026-09-05/race-suite.txt),
[vet](feasibility/bootstrap-service-2026-09-05/vet.txt), and
[cleanup audit](feasibility/bootstrap-service-2026-09-05/cleanup.txt).

## Runtime access and deployment limits

The installed entry point requires the dedicated `codexos-bootstrap` account.
Acceptance exercised the same worker and helper code through a package-local
launcher under the current unprivileged UID 1000 and disposable aggregate slice.
It did **not** install sudoers, create the dedicated account, or validate access
from that account to operator-owned paths: `sudo -n` requires a password here.
Those host-administration steps and the production sudo/systemd entry chain need
operator verification using [the exact setup guide](bootstrap-service-operations.md).
Production does not contain the test launcher override as a configurable option.

No running guest advertised the new helpers during acceptance. Their required
[request, source-capture and binary-safe import contract](bootstrap-host-services.md)
is documented and tool forwarding is tested; implementing guest facilities is
still the implementor's work. Bootstrap availability is not fulfillment of
feature request #3. Review/merge, setup, per-run provisioning, and starting a
generation remain separate operator actions.

Historical artifacts consume quota until their references can be retired; GC
never evicts archived tools. This implementation deliberately has one image,
one job owner and fixed limits, not a general execution platform. Linux amd64,
SELinux enforcement and the measured cgroup/kernel controls are required; other
hosts fail closed and are not covered by this acceptance.


## Review follow-up: recovery and initial inheritance

At `0aad574`, successful runtime recovery reopens admission when the generation
is running, preserves suspension when paused, and leaves failed recovery
suspended. `TestRuntimeBootstrapRecoveryRestoresOnlyRunningAdmission` calls the
runtime recovery API and then submits another job through its real serial
host-service exchange. Only the isolated worker process is synthetic in this
regression; admission, runtime state and storage publication are exercised.

`--provision-inherited-bootstrap TCC_ASSET_ID` now validates an unstarted
inherited destination and enables it before initial boot. The CLI/operator
regression `TestRunnerProvisionsInheritedBootstrapBeforeBoot` creates inheritance
through the real startup path and uses the exact pinned TCC asset. Its disposable
successor requests an inherited artifact **before** sending the ready marker.
Explicit provisioning returns the expected bytes; the default-disabled case
returns rejection. Wrong pins, malformed inherited references, partial generation
state, a wrong ISO and a failed worker probe prevent boot and leave execution
disabled. Retrying after a failed probe works without republishing the destination;
provisioning after a recorded first boot is rejected. No low-level destination
`Provision` call supplies the permission in this operator test.

The earlier rootless test is a storage/worker fixture and now calls the inherited
provisioning primitive; it is not presented as operator-gate evidence. The new
operator acceptance replaces that earlier coverage gap. Its QEMU and worker
process transports are disposable Go fixtures, not a native guest port or a
claim to have tested the administrative sudo/systemd installation path.

The operator test also exposed an omitted Git archive allowlist entry. Git
reconciliation now validates optional bootstrap references for completed and
aborted archives, and rejects malformed references instead of ignoring them.

Reproduce the new operator acceptance after acquiring the pinned archive (no
image pull is needed for these transport fixtures):

```bash
bootstrap_review_inputs=$(mktemp -d /tmp/bootstrap-review-inputs-XXXXXX)
git init -q "$bootstrap_review_inputs/tcc"
timeout --signal=TERM --kill-after=5s 120s \
  git -C "$bootstrap_review_inputs/tcc" fetch --depth=1 \
  https://repo.or.cz/tinycc.git 0fb54300b56512754221d80adda85ddb9815bceb
git -C "$bootstrap_review_inputs/tcc" archive --format=tar --prefix=tinycc/ \
  0fb54300b56512754221d80adda85ddb9815bceb > "$bootstrap_review_inputs/tcc-source.tar"
CODEXOS_BOOTSTRAP_TCC_TAR="$bootstrap_review_inputs/tcc-source.tar" \
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p=1 -parallel=1 \
  ./internal/operator -run 'TestRunnerProvisionsInheritedBootstrapBeforeBoot|TestInitialBootstrapCLI' \
  -count=1 -v -timeout=90s
GOMAXPROCS=2 GOMEMLIMIT=768MiB go test -p=1 -parallel=1 \
  ./internal/experiment -run TestRuntimeBootstrapRecovery -count=1
# Remove this invocation's inputs after saving evidence.
rm -rf -- "$bootstrap_review_inputs"
```

The [operator log](feasibility/bootstrap-service-2026-09-05/review-initial.txt),
[focused checks](feasibility/bootstrap-service-2026-09-05/review-focused.txt),
[full Go suite](feasibility/bootstrap-service-2026-09-05/review-go.txt),
[full race suite](feasibility/bootstrap-service-2026-09-05/review-race.txt),
[vet](feasibility/bootstrap-service-2026-09-05/review-vet.txt), and
[cleanup audit](feasibility/bootstrap-service-2026-09-05/review-cleanup.txt)
record passing follow-up results and an empty process/container audit. The full
suites use the same serialized/bounded
commands above, with `CODEXOS_BOOTSTRAP_TCC_TAR` set so the new operator acceptance
runs rather than skips. Rootless container acceptance was not rerun for these
runtime/provisioning changes; the original real-container evidence remains above.
Dedicated-account access still requires operator verification. No live capability,
feature decision, Python code or permanent seed was changed.

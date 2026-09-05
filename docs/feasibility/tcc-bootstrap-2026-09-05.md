# TCC bootstrap feasibility — 2026-09-05

**Result:** pinned upstream TCC builds and executes in an offline rootless Podman
job on this host. Retrieved Linux compiler files work in a second isolated job.
This demonstrates Linux bootstrap tooling, not a guest compiler port, production
service, provisioned feature, or complete guest-runnable compilation pipeline.
See the [proposed Go interface and implementation plan](../tcc-bootstrap-service-design.md).

## Exact inputs and reproduction

Scripts: commit `c89d54f` on `design/tcc-bootstrap-service`, based on main
`65f82ea`. All fixtures are under
[`scripts/tcc-bootstrap-feasibility/`](../../scripts/tcc-bootstrap-feasibility/).
They are host-side test inputs, not modifications to the seed or experiment guest.

| Input | Recorded identity |
| --- | --- |
| Upstream | `https://repo.or.cz/tinycc.git` |
| TCC commit | `0fb54300b56512754221d80adda85ddb9815bceb` |
| Commit metadata | `2026-09-04 18:46:13 +0200`, “Make bound checking faster.” |
| Source archive | `git archive --format=tar --prefix=tinycc/ <commit>` |
| Archive SHA-256 | `e696d12b9429faf08a08aeeaffe96769370e5ea50cf98218f45c74956b3b3f18` |
| Archive size / regular-file content | 4,997,120 bytes / 4,544,205 bytes in 546 files |
| Image origin | Official `docker.io/library/gcc:14.3.0-bookworm`, Linux amd64 |
| Pinned platform manifest | `sha256:a689e29bc3adf4663ef9a141d23081252764d1319c63f591a027bd6fd676f4c1` |
| Image configuration ID | `f3d916a4884034b89cb6148781f07d8c92d94b6c6dc1b74dcbec3475d16400da` |
| Unpacked image size | 1,444,948,472 bytes |

The image is addressed by platform manifest digest, not the mutable tag or the
multi-platform index. `prepare.sh` fetches the exact Git commit, verifies the tar
hash, pulls anonymously using a temporary empty auth file if needed, and checks
the image ID. Network access is confined to acquisition; no credentials are
staged as job inputs. The upstream commit's README/configure/documentation are
the revision-specific source for compiler capabilities; the older
[project page](https://bellard.org/tcc/) is only general background.

Requirements: Linux amd64, rootless Podman, a delegated user systemd cgroup-v2
session, Git, Bash, GNU coreutils/tar, and `jq`. Run serially, from the repository
root, with fresh output directories:

```bash
work=$(mktemp -d /tmp/codexos-tcc-demo-XXXXXX)
scripts/tcc-bootstrap-feasibility/prepare.sh "$work/inputs"
TCC_FEASIBILITY_SECRET=must-not-enter-container \
  scripts/tcc-bootstrap-feasibility/run.sh "$work/inputs" "$work/evidence"
cat "$work/evidence/result.txt"
```

Actual successful run used `/tmp/codexos-tcc-prepared-reproducer` and
`/tmp/codexos-tcc-evidence-final`; [runner output](tcc-bootstrap-2026-09-05/runner.txt)
records its unique task slice. `run.sh` contains every container command;
[job-argv.txt](tcc-bootstrap-2026-09-05/job-argv.txt) records shared flags.
The input mount contains only the pinned source tar and fixed fixture files.
No experiment directory, provided-assets origin, checkout, home, or control socket
was mounted. Container argv uses `--pull=never --network=none`, a read-only root,
private executable tmpfs, cleared environment, UID 65534, dropped capabilities,
no-new-privileges and the default seccomp/SELinux policy.

The exact upstream build commands, inside `/work/tinycc`, are:

```sh
./configure --prefix=/work/installed --enable-cross --with-selinux --extra-cflags=-O0
make -j1 tcc libtcc1.a i386-tcc arm64-tcc riscv64-tcc
```

`-O0` bounds the GCC bootstrap build's time; it is not a TCC guest adaptation.
No upstream source changes were made. The full
[`job.sh`](../../scripts/tcc-bootstrap-feasibility/job.sh) records compilation,
execution, object inspection, output hashes and the ready/hold boundary.

## Observed compiler and retrieval results

| Probe | Actual result |
| --- | --- |
| TCC version | `0.9.28rc (x86_64 Linux)` |
| Native C execution | `-std=c99 -run` and `-std=c11` Linux executable each printed `native-c99-c11-smoke=42` and exited zero |
| Language features exercised | Preprocessing, libc calls, designated initializers, variable-length array, `_Static_assert`; a smoke test, not complete C99/C11 conformance |
| Target object generation | x86-64 ELF64, i386 ELF32, AArch64 ELF64, RISC-V ELF64; verified with `readelf -h` |
| Linux converter | TCC-built program copied 16 opaque bytes, verified by `cmp`; no executable-format knowledge |
| Retrieval and reuse | Paused-container output capture; native TCC, headers, `libtcc1.a`, and `runmain.o` remounted read-only in a later container; `-run` printed `retrieved-tcc-executes` |

TCC is a C compiler; this work does not claim C++ support. Cross-target probes
produced freestanding objects only: no cross-libc/sysroot, cross linking, foreign
execution, guest ABI, executable packaging or guest runtime was validated.
The Linux compiler reuse probe depends on the pinned image's Linux headers and
libraries; the retrieved files are not claimed to be a standalone guest SDK.
Native self-hosting and a guest compiler backend were neither required nor tested.

Two findings changed the reproducer:

* Default TCC `-run` failed with `mprotect failed` under enforcing SELinux.
  Upstream `--with-selinux` uses file-backed mappings in `/tmp`; a bounded 16 MiB
  executable tmpfs permits this without disabling SELinux or seccomp.
* `podman cp` could not see running `/work` tmpfs output. Pausing and using
  `podman unshare tar -C /proc/<init-pid>/root/work/export ...` did capture the
  fixed files. Stopping first loses tmpfs. A secure collector for hostile output
  still needs process identity checks, beneath-only opens, type/link rejection
  and hard quotas; the fixture tar pipeline is not that implementation.

The reuse probe also established that this revision needs `runmain.o` alongside
the compiler and `libtcc1.a` for the tested `-run` invocation. Build/retrieval
results and SHA-256 values are in [build.txt](tcc-bootstrap-2026-09-05/build.txt)
and [artifact-sizes.txt](tcc-bootstrap-2026-09-05/artifact-sizes.txt).

## Actual rootless resource controls

[Environment](tcc-bootstrap-2026-09-05/environment.json): Podman 5.8.1, crun 1.27,
Linux amd64, rootless, systemd/cgroup v2, delegated cpu/io/memory/pids controllers.
SELinux was **Enforcing** and seccomp enabled. These are local observations,
not a promise that other rootless installations delegate the same controls.

| Control | Verified result |
| --- | --- |
| Build limits | Inspect: 512 MiB RAM, memory+swap also 512 MiB (zero swap), 64 PIDs, one CPU; root filesystem read-only |
| Aggregate parent | `memory.max=805306368`, `memory.swap.max=0`, `cpu.max=100000 100000`, `pids.max=96`; build cgroup membership verified beneath this slice |
| Memory failure probe | 64 MiB container tried to touch 128 MiB; exit 137, `OOMKilled=true` |
| PID failure probe | 16-task container forked 15 children, next fork returned `EAGAIN`; children killed and reaped |
| CPU probe | Four busy children at 0.5 CPU; 28/28 measured quota periods throttled, `throttled_usec=9642231` |
| Scratch exhaustion | Writing 9 MiB into 8 MiB tmpfs stopped at 8,388,608 bytes with `ENOSPC` |
| Rlimits | `/proc/self/limits`: 128 descriptors, 67,108,864-byte regular-file limit, zero core-file size |
| Diagnostics | No Podman log driver; noisy fixture captured a 65,537-byte overflow sentinel, was removed, retained log truncated to 65,536 bytes |
| Deadline | TERM-resistant shell/child stopped after 3 seconds via Podman timeout; `Running=false`, Podman's recorded exit code `-1` (not claimed as a normal exit/signal code) |
| Process privilege / network | UID 65534, `CapEff=0`, `NoNewPrivs=1`, `Seccomp=2`, only loopback interface |
| Inputs / secret isolation | Retrieved tooling read-only; synthetic host environment secret absent; checked experiment/home/socket paths absent |

The build used proposed production-sized limits. Failure probes deliberately
lowered individual limits and ran one at a time. Aggregate controller values and
actual child placement were verified; aggregate saturation, cross-harness
admission and disk-retention quotas were **not** implemented or stress-tested.
Rlimits were read back, not individually exhausted. The deadline probe validates
the runtime primitive; prompt operator cancellation through Go's operation lock,
crash recovery and a trusted completion supervisor remain future work.

Text captures normalize trailing terminal whitespace only. Raw observations: [build inspect](tcc-bootstrap-2026-09-05/build-inspect.json),
[parent limits](tcc-bootstrap-2026-09-05/aggregate-controls.txt),
[membership](tcc-bootstrap-2026-09-05/build-cgroup.txt),
[isolation](tcc-bootstrap-2026-09-05/isolation.txt),
[PID](tcc-bootstrap-2026-09-05/pids.txt),
[OOM](tcc-bootstrap-2026-09-05/memory-state.txt),
[CPU](tcc-bootstrap-2026-09-05/cpu.txt),
[disk](tcc-bootstrap-2026-09-05/disk.txt),
[deadline](tcc-bootstrap-2026-09-05/cancel-state.txt), and
[diagnostic bound](tcc-bootstrap-2026-09-05/noise-state.txt).
The [Podman documentation](https://docs.podman.io/en/stable/markdown/podman-run.1.html)
explains flags; observed controller files and failures establish local support.

## Cleanup, verification and scope

The runner's trap removes only its labeled containers, stops its anchor/slice,
reverts its runtime slice properties and removes private staged inputs. Empty
automatically generated parent slices were inspected (`populated 0`) and stopped
afterward with `systemctl --user stop codexos-tcc-feas.slice codexos-tcc.slice`.
The newly downloaded GCC image was removed by its configuration ID without force
or system prune. Both baseline and final container inventories were empty.

The [final audit](tcc-bootstrap-2026-09-05/cleanup-audit.json) found zero containers,
zero images, zero processes in task cgroups, no task units, and absent recorded
build-init/conmon PIDs. A separate process-name check found no `conmon`, `crun`,
TCC/cross-TCC, converter or resource-probe processes. Raw evidence and retrieved
fixture binaries are retained in `/shared/tcc-bootstrap-feasibility/` alongside
the pinned source and reproducer. Temporary acquisition/build directories were
removed; nothing was provided to a guest or installed as a run capability.

To clean a reproduction after saving evidence, remove its `$work` directory.
Remove the pinned image only if this reproduction acquired it and no other work
uses it; `prepare.sh` intentionally reuses an already present image. Do not prune
unrelated containers or images. The runner performs task-container cleanup even
on failed probes; the output directories are retained for diagnosis.

Verification: actual `prepare.sh` and complete serialized `run.sh` passed;
`bash -n` for both Bash scripts and `sh -n` for the container fixture passed.
GCC compiled the resource fixture with `-Wall -Wextra -Werror`; TCC compiled and
executed the functional fixtures. Documentation links and `git diff --check`
were checked. No Go runtime code changed, so no unrelated Go/Python full suite
was run. Production isolation/authorization/retention and hostile-output tests
belong to the separate implementation plan, not this feasibility claim.
